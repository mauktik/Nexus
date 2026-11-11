package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts"
)

// The T0 projects are made by the test user, not root, so every test but
// the ones below that stage the check themselves runs with it accepting.
func init() { composeTrust = func(string, bool, []scripts.Owner) error { return nil } }

// withTrust runs the test with composeTrust replaced by f.
func withTrust(t *testing.T, f func(path string, dir bool, owners []scripts.Owner) error) {
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
	withTrust(t, func(path string, d bool, _ []scripts.Owner) error { calls = append(calls, call{path, d}); return nil })
	if _, err := composeTargetOf(labeled(labels), nil); err != nil {
		t.Fatal(err)
	}
	want := []call{{dir, true}, {filepath.Join(dir, "compose.yaml"), false}, {filepath.Join(dir, "override.yaml"), false},
		{filepath.Join(dir, "nx.env"), false}, {filepath.Join(dir, ".env"), false}}
	if !slices.Equal(calls, want) {
		t.Fatalf("checked %v, want %v", calls, want)
	}

	// An untrusted path refuses the update, naming the path the check found.
	withTrust(t, func(path string, d bool, _ []scripts.Owner) error {
		if filepath.Base(path) == ".env" {
			return problem.New(problem.ScriptIntegrityFailed, path+" is owned by UID 1000, not root").With("path", path)
		}
		return nil
	})
	_, err := composeTargetOf(labeled(labels), nil)
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
	withTrust(t, scripts.CheckOwnedBy)
	_, labels := composeProjectDir(t, "media")
	_, err := composeTargetOf(labeled(labels), nil)
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

// trustingProvider is updProvider on f with trustedOwners set to names.
func trustingProvider(t *testing.T, f *updateEngine, names ...string) (*Provider, string, error) {
	t.Helper()
	p, ops := updProvider(t, f)
	cfgDir := p.Settings().ConfigDir
	err := p.Configure(func(into any) error {
		c := into.(*config.DockerConfig)
		c.Socket, c.ConfigDir, c.Compose.TrustedOwners = f.socket, cfgDir, names
		return nil
	})
	return p, ops, err
}

// currentOwner is the test user as a trusted owner.
func currentOwner(t *testing.T) scripts.Owner {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if uid == 0 {
		t.Skip("root is always trusted, so it is never listed")
	}
	return scripts.Owner{Name: u.Username, UID: uint32(uid)}
}

// providers.docker.compose.trustedOwners is resolved when the provider is
// configured, root dropped; an unknown name refuses the section and keeps
// the owners in force. The resolved owners reach every check: admission,
// input.json, and the helper's own check.
func TestComposeTrustedOwnersReachEveryCheck(t *testing.T) {
	me := currentOwner(t)
	mine := []scripts.Owner{me}
	f, _ := composeEngine(t)
	p, ops, err := trustingProvider(t, f, "root", me.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.trustedOwners(); !slices.Equal(got, mine) {
		t.Fatalf("resolved %+v, want %+v", got, mine)
	}
	err = p.Configure(func(into any) error {
		into.(*config.DockerConfig).Compose.TrustedOwners = []string{"nx-no-such-user-182"}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "providers.docker.compose.trustedOwners") ||
		!strings.Contains(err.Error(), `"nx-no-such-user-182"`) {
		t.Fatalf("unknown owner: %v", err)
	}
	if got := p.trustedOwners(); !slices.Equal(got, mine) {
		t.Fatalf("after a refused section, owners %+v, want %+v", got, mine)
	}

	var seen [][]scripts.Owner
	withTrust(t, func(_ string, _ bool, owners []scripts.Owner) error { seen = append(seen, owners); return nil })
	p.composeTools = toolsPresent
	if err := p.admitUpdate(context.Background(), spec.ContainerInput{Name: "app"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if _, err := p.prepareUpdate(context.Background(), spec.ContainerInput{Name: "app"}, provider.NewRun(testOpID, nil)); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || slices.ContainsFunc(seen, func(o []scripts.Owner) bool { return !slices.Equal(o, mine) }) {
		t.Fatalf("checks saw owners %+v, want %+v each time", seen, mine)
	}
	b, err := os.ReadFile(filepath.Join(ops, testOpID, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	var hin helperInput
	if err := json.Unmarshal(b, &hin); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hin.TrustedOwners, mine) {
		t.Fatalf("input.json trustedOwners %+v, want %+v\n%s", hin.TrustedOwners, mine, b)
	}

	// The helper judges with the owners in input.json. Its run stops at
	// the local-build refusal, after the check.
	rf := newRunEngine(t, true)
	rf.known = map[string]bool{}
	_, labels := composeProjectDir(t, "media")
	rf.ctrs[runOldID].labels = labels
	old := helperComposeTools
	helperComposeTools = func(string) composeTools { return composeTools{Docker: "/nonexistent/docker", Plugin: "/x"} }
	t.Cleanup(func() { helperComposeTools = old })
	in := rf.input()
	in.Mode, in.ComposeTimeout, in.TrustedOwners = ModeCompose, time.Minute, mine
	in.Compose = &composeTarget{Project: "media", Service: "app"}
	seen = nil
	var out bytes.Buffer
	if res := runCompose(context.Background(), in, &out); res.Error == nil || res.Error.Slug != problem.NotUpdatable {
		t.Fatalf("helper result %+v %+v\n%s", res, res.Error, out.String())
	}
	if len(seen) == 0 || slices.ContainsFunc(seen, func(o []scripts.Owner) bool { return !slices.Equal(o, mine) }) {
		t.Fatalf("the helper's check saw owners %+v, want %+v each time", seen, mine)
	}
}

// With no trusted owners, an ownership refusal names trustedOwners as the
// alternative to chown root; with some, or for a refusal that trusting an
// owner would not cure, it does not.
func TestComposeTrustRefusalNamesTrustedOwners(t *testing.T) {
	dir, labels := composeProjectDir(t, "media")
	const hint = "providers.docker.compose.trustedOwners"
	for _, tc := range []struct {
		name     string
		check    string
		owners   []scripts.Owner
		wantHint bool
	}{
		{"owner, none trusted", scripts.CheckDirectoryOwner, nil, true},
		{"file owner, none trusted", scripts.CheckScriptOwner, nil, true},
		{"owner, some trusted", scripts.CheckDirectoryOwner, []scripts.Owner{{Name: "deploy", UID: 1000}}, false},
		{"world-writable, none trusted", scripts.CheckDirectoryWorldWritable, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTrust(t, func(path string, _ bool, _ []scripts.Owner) error {
				return problem.New(problem.ScriptIntegrityFailed, "directory "+path+" is owned by UID 1000, not root. "+
					"Fix: chown root "+path).With("check", tc.check).With("path", path)
			})
			_, err := composeTargetOf(labeled(labels), tc.owners)
			pe := problemOf(t, err)
			if pe.Slug() != problem.ComposeFilesMissing || pe.Extensions["path"] != dir ||
				!strings.Contains(pe.Detail, "chown root "+dir) {
				t.Fatalf("refusal %+v", pe)
			}
			if got := strings.Contains(pe.Detail, hint); got != tc.wantHint {
				t.Fatalf("names %s: %v, want %v\n%s", hint, got, tc.wantHint, pe.Detail)
			}
		})
	}
}
