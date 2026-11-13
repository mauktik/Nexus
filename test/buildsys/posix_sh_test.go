// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md TC-32 and TC-34. WI 3.2
// removed the skips and wrote TC-34's body. An un-skipped passing test is
// the GREEN signal (test-strategy.md, RED Stub Style).

package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// nginx-provider TC-32: Deploy scripts are POSIX sh. Owner WI 3.2.
// Requirements: FR-24, CON-12.
func TestDeployScriptsArePOSIX(t *testing.T) {
	root := repoRoot(t)
	forbidden := map[string]*regexp.Regexp{
		"[[ ]]":            regexp.MustCompile(`\[\[`),
		"pipefail":         regexp.MustCompile(`pipefail`),
		"local":            regexp.MustCompile(`(^|[;&|\s])local\s`),
		"$'...'":           regexp.MustCompile(`\$'`),
		"array assignment": regexp.MustCompile(`^\s*\w+=\(`),
		"function keyword": regexp.MustCompile(`(^|\s)function\s+\w+`),
		"here-string":      regexp.MustCompile(`<<<`),
		"source":           regexp.MustCompile(`(^|[;&|\s])source\s`),
		"&>":               regexp.MustCompile(`&>`),
		"${v:off}":         regexp.MustCompile(`\$\{\w+:[0-9]`),
	}
	for _, rel := range []string{"deploy/install.sh", "deploy/uninstall.sh", "deploy.sh"} {
		path := filepath.Join(root, rel)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.HasPrefix(string(b), "#!/bin/sh\n") {
			t.Errorf("%s: first line is not #!/bin/sh", rel)
		}
		for _, parser := range [][]string{{"sh", "-n"}, {"dash", "-n"}, {"busybox", "sh", "-n"}} {
			if _, err := exec.LookPath(parser[0]); err != nil {
				t.Logf("%s not on PATH: %s parse skipped", parser[0], rel)
				continue
			}
			if out, err := exec.Command(parser[0], append(parser[1:], path)...).CombinedOutput(); err != nil {
				t.Errorf("%s: %s rejects it: %v\n%s", rel, strings.Join(parser, " "), err, out)
			}
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "#"); j >= 0 && (j == 0 || code[j-1] == ' ' || code[j-1] == '\t') {
				code = code[:j]
			}
			for name, re := range forbidden {
				if re.MatchString(code) {
					t.Errorf("%s:%d uses %s (not POSIX sh): %s", rel, i+1, name, line)
				}
			}
		}
		if _, err := exec.LookPath("shellcheck"); err == nil {
			if out, err := exec.Command("shellcheck", "-s", "sh", path).CombinedOutput(); err != nil {
				t.Errorf("%s: shellcheck -s sh: %v\n%s", rel, err, out)
			}
		} else {
			t.Logf("shellcheck not on PATH: lint of %s skipped", rel)
		}
	}
}

// nginx-provider TC-34: deploy.sh accepts either init system. Owner WI 3.2.
// Requirements: FR-24.
//
//	Setup: as the existing TestDeployScript, a fake ssh shim on PATH that
//	answers deploy.sh's host check with, in turn, the systemd marker
//	(/run/systemd/system is a directory), the OpenRC markers (/run/openrc,
//	/sbin/openrc-run and /sbin/rc-service executable), and neither.
//	Expected:
//	  - systemd and OpenRC: deploy.sh proceeds past the host check (the shim
//	    records the next remote command).
//	  - neither: non-zero exit, stderr names both systemd and OpenRC, and the
//	    shim records no command after the check.
//
// deploy.sh runs on the host itself, not over ssh, so the shim is a PATH
// directory: uname and id answer as an x86-64 Linux normal user, and sudo
// and go record their arguments. The markers are laid out under
// NEXUS_HOST_ROOT, which deploy.sh reads for exactly this. Proceeding past
// the host check is the go shim recording the build, the next command.
func TestDeployScriptAcceptsEitherInit(t *testing.T) {
	root := repoRoot(t)
	shims := t.TempDir()
	record := filepath.Join(t.TempDir(), "record")
	writeShim := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(shims, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeShim("uname", `case "$1" in -m) echo x86_64 ;; *) echo Linux ;; esac`+"\n")
	writeShim("id", "echo 1000\n")
	writeShim("sudo", `echo "sudo $*" >>"$NEXUS_SHIM_RECORD"; exit 1`+"\n")
	writeShim("go", `echo "go $*" >>"$NEXUS_SHIM_RECORD"; exit 1`+"\n")

	executable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		layout  func(t *testing.T, host string)
		proceed bool
	}{
		{"systemd", func(t *testing.T, host string) {
			if err := os.MkdirAll(filepath.Join(host, "run", "systemd", "system"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"openrc", func(t *testing.T, host string) {
			if err := os.MkdirAll(filepath.Join(host, "run", "openrc"), 0o755); err != nil {
				t.Fatal(err)
			}
			executable(t, filepath.Join(host, "sbin", "openrc-run"))
			executable(t, filepath.Join(host, "sbin", "rc-service"))
		}, true},
		{"neither", func(*testing.T, string) {}, false},
		// platform.DetectAt also requires both OpenRC binaries to be
		// executable.
		{"openrc without an executable rc-service", func(t *testing.T, host string) {
			if err := os.MkdirAll(filepath.Join(host, "run", "openrc"), 0o755); err != nil {
				t.Fatal(err)
			}
			executable(t, filepath.Join(host, "sbin", "openrc-run"))
			if err := os.WriteFile(filepath.Join(host, "sbin", "rc-service"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := t.TempDir()
			c.layout(t, host)
			if err := os.RemoveAll(record); err != nil {
				t.Fatal(err)
			}
			env := append(envWithout("PATH", "NEXUS_HOST_ROOT", "NEXUS_SHIM_RECORD", "TMPDIR"),
				"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
				"NEXUS_HOST_ROOT="+host,
				"NEXUS_SHIM_RECORD="+record,
				"TMPDIR="+t.TempDir(),
			)
			out, code := run(t, root, env, "./deploy.sh")
			recorded, err := os.ReadFile(record)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if c.proceed {
				if !strings.Contains(string(recorded), "go build") {
					t.Errorf("deploy.sh stopped before the build (exit %d):\n%s\nrecorded: %q", code, out, recorded)
				}
				if strings.Contains(out, "OpenRC is running") {
					t.Errorf("deploy.sh refused the host:\n%s", out)
				}
				return
			}
			if code == 0 {
				t.Errorf("deploy.sh exited 0 on a host with neither init system:\n%s", out)
			}
			if !strings.Contains(out, "systemd") || !strings.Contains(out, "OpenRC") {
				t.Errorf("the refusal does not name systemd and OpenRC:\n%s", out)
			}
			if len(recorded) != 0 {
				t.Errorf("deploy.sh ran commands after refusing the host: %q", recorded)
			}
		})
	}
}
