package detach_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
)

// TC-62: the launcher's argv is exactly the Transient unit invocation of the
// architecture, for both kill modes, and carries no environment: no
// --setenv, no Environment=, and no sensitive value anywhere.
func TestLauncherInvocationIsExactAndSecretFree(t *testing.T) {
	const (
		id       = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
		exe      = "/usr/local/bin/nexusd"
		opsDir   = "/var/lib/nexus/ops"
		sentinel = "SENTINEL-s3cr3t-value"
	)
	for _, km := range []detach.KillMode{detach.KillProcess, detach.KillControlGroup} {
		t.Run(string(km), func(t *testing.T) {
			spec := detach.Spec{
				Argv:     []string{"/opt/deploy/deploy-api.sh", "--branch", "main"},
				Env:      []string{"API_KEY=" + sentinel, "PATH=/usr/bin:/bin"},
				KillMode: km, MaxRuntime: time.Hour, Timeout: time.Minute, Grace: time.Second,
			}
			got := detach.Argv(id, exe, opsDir, spec)
			// Copied from architecture.md, Transient unit invocation.
			want := []string{
				"/usr/bin/systemd-run", "--unit=nexus-op-" + id, "--collect", "--quiet", "--service-type=exec",
				"--description=Nexus operation " + id,
				"--property=RefuseManualStop=yes",
				"--property=KillMode=" + string(km),
				"--", exe, "op-supervise", "--dir", "/var/lib/nexus/ops/" + id,
			}
			if !slices.Equal(got, want) {
				t.Fatalf("argv\n got %q\nwant %q", got, want)
			}
			for _, a := range got {
				if strings.Contains(a, "--setenv") || strings.Contains(a, "Environment=") ||
					strings.HasPrefix(a, "-E") || strings.Contains(a, sentinel) {
					t.Fatalf("argv element %q carries environment", a)
				}
			}
		})
	}
	if u := detach.Unit(id); u != "nexus-op-"+id+".service" {
		t.Fatalf("unit %q", u)
	}
}

// TC-63: detach.New(cfg, detach.Options{}) is the package's only
// constructor, and a nil Options.Launch selects the real systemd-run,
// asserted through RealLauncher without running it (real execution is at
// 3.5).
func TestSingleConstructorWithTestInjection(t *testing.T) {
	out, err := exec.Command("go", "doc", "github.com/mauktik/nexus/internal/detach").CombinedOutput()
	if err != nil {
		t.Fatalf("go doc: %v\n%s", err, out)
	}
	ctor := regexp.MustCompile(`(?m)^\s*func (\w+)\(.*\*Launcher\b`)
	var ctors []string
	for _, m := range ctor.FindAllStringSubmatch(string(out), -1) {
		ctors = append(ctors, m[1])
	}
	if !slices.Equal(ctors, []string{"New"}) {
		t.Fatalf("constructors of *Launcher: %v, want exactly [New]\n%s", ctors, out)
	}
	if !strings.Contains(string(out), "func New(cfg Config, opts Options) (*Launcher, error)") {
		t.Fatalf("New has an unexpected signature:\n%s", out)
	}

	dir := t.TempDir()
	real, err := detach.New(detach.Config{OpsDir: dir}, detach.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !real.RealLauncher() {
		t.Fatal("nil Options.Launch did not select the real systemd-run")
	}
	local, err := detach.New(detach.Config{OpsDir: dir}, detachtest.Local())
	if err != nil {
		t.Fatal(err)
	}
	if local.RealLauncher() {
		t.Fatal("an injected Launch was reported as the real launcher")
	}
	if got := local.Dir("abc"); got != filepath.Join(dir, "abc") {
		t.Fatalf("Dir = %q", got)
	}
}

// TC-67 (here): this Mac has no systemd, so the helper reports unavailable
// with a reason and providers never fall back to a child process. The
// positive case with a fixture layout is in launcher_internal_test.go.
func TestSystemdAvailabilityRefusesSilentDowngrade(t *testing.T) {
	a := detach.SystemdAvailable()
	if a.Available {
		if out, err := exec.Command("uname").Output(); err == nil && strings.TrimSpace(string(out)) == "Darwin" {
			t.Fatal("systemd reported available on macOS")
		}
		t.Skip("this host has systemd; the negative case needs a host without it")
	}
	if a.Reason == "" {
		t.Fatal("unavailable without a reason")
	}
}
