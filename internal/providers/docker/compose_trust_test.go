package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/scripts"
)

// The T0 projects are made by the test user, not root, so every test but
// the ones below that stage the check themselves runs with it accepting.
func init() { composeTrust = func(string, bool) error { return nil } }

// withTrust runs the test with composeTrust replaced by f.
func withTrust(t *testing.T, f func(path string, dir bool) error) {
	t.Helper()
	old := composeTrust
	composeTrust = f
	t.Cleanup(func() { composeTrust = old })
}

// SEC-1: every path Compose reads is judged: the working directory, each -f
// file, each --env-file, and the .env file of the working directory, which
// Compose reads without being told.
func TestComposeTrustChecksEveryPathComposeReads(t *testing.T) {
	dir, labels := composeProjectDir(t, "media")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	type call struct {
		path string
		dir  bool
	}
	var calls []call
	withTrust(t, func(path string, d bool) error { calls = append(calls, call{path, d}); return nil })
	if _, err := composeTargetOf(labeled(labels)); err != nil {
		t.Fatal(err)
	}
	want := []call{{dir, true}, {filepath.Join(dir, "compose.yaml"), false}, {filepath.Join(dir, "override.yaml"), false},
		{filepath.Join(dir, "nx.env"), false}, {filepath.Join(dir, ".env"), false}}
	if !slices.Equal(calls, want) {
		t.Fatalf("checked %v, want %v", calls, want)
	}

	// An untrusted path refuses the update, naming the path the check found.
	withTrust(t, func(path string, d bool) error {
		if filepath.Base(path) == ".env" {
			return problem.New(problem.ScriptIntegrityFailed, path+" is owned by UID 1000, not root").With("path", path)
		}
		return nil
	})
	_, err := composeTargetOf(labeled(labels))
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.ComposeFilesMissing || pe.Extensions["path"] != filepath.Join(dir, ".env") ||
		!strings.Contains(pe.Detail, "owned by UID 1000") || !strings.Contains(pe.Detail, "nothing was changed") {
		t.Fatalf("untrusted .env: %v", err)
	}
}

// SEC-1 with the real check: a project the unprivileged test user owns is
// refused. TC-182 covers a root-owned project under a user-owned home.
func TestComposeTrustRefusesAProjectNotOwnedByRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("files made by root are root-owned")
	}
	withTrust(t, scripts.CheckRootOwned)
	_, labels := composeProjectDir(t, "media")
	_, err := composeTargetOf(labeled(labels))
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.ComposeFilesMissing || !strings.Contains(pe.Detail, "not trusted") ||
		!strings.Contains(pe.Detail, "chown root") {
		t.Fatalf("got %v, want compose-files-missing naming the chown", err)
	}
}

// fakeComposeHash is a docker executable whose `compose ... config --hash S`
// prints out; any other verb fails.
func fakeComposeHash(t *testing.T, out string) string {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "docker")
	writeExec(t, fake, "#!/bin/sh\ncase \"$*\" in\n*config*) cat <<'EOF'\n"+out+"\nEOF\n;;\n*) exit 9 ;;\nesac\n", 0o755)
	return fake
}

// CR-29: a service whose definition changed since its container was created
// is refused before anything is pulled; an unchanged one, and one that
// cannot be compared, go on.
func TestComposeChangedComparesTheConfigHash(t *testing.T) {
	tg := composeTarget{Project: "media", Service: "app", WorkingDir: t.TempDir(), ConfigFiles: []string{"/x/compose.yaml"}}
	hashOld := strings.Repeat("a", 64)
	hashNew := strings.Repeat("b", 64)
	withHash := func(h string) container.InspectResponse {
		return labeled(map[string]string{composeHashLabel: h})
	}
	for _, tc := range []struct {
		name   string
		output string
		c      container.InspectResponse
		want   string // "" proceeds; else a substring of the refusal
	}{
		{"unchanged", "app " + hashOld, withHash(hashOld), ""},
		{"unchanged, bare hash", hashOld, withHash(hashOld), ""},
		{"edited", "app " + hashNew, withHash(hashOld), "changed since container app was created"},
		{"no label on the container", "app " + hashNew, labeled(nil), ""},
		{"output not understood", "a b c d", withHash(hashOld), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			u := &updater{in: helperInput{Name: "app", ComposeTimeout: time.Minute}, out: &out}
			got := u.composeChanged(context.Background(), fakeComposeHash(t, tc.output), tg, tc.c)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// The command that reads the hash is an argument vector with the
	// service as --hash's value.
	if got := composeArgv("docker", tg, "config", "--hash"); got[len(got)-2] != "--hash" || got[len(got)-1] != "app" {
		t.Errorf("argv %q", got)
	}
}
