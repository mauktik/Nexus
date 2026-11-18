// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-22, TC-SV-41, TC-SV-132, TC-SV-134.
// Owner WIs: 1.3, 3.1, 5.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package buildsys

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TC-SV-22: The existing recovery and detached suites are unchanged by the reboot rules. Owner WI 1.3. Requirements: CON-4.
//
// - Setup: `repoRoot`; base `43e5792`.
// - Steps:
//  1. `git diff --exit-code 43e5792 -- internal/ops/recovery_test.go internal/ops/detached_test.go internal/ops/cancel_test.go`.
//  2. Unless `skipUnderMake`, run `go test -race -count=1 -run 'Recovery|Detached|Cancel' ./internal/ops`.
//
// - Expected: Step 1 shows no diff, and step 2 passes. Ops without the flag behave as before, proven by an unchanged suite.
func TestTCSV22_TheExistingRecoveryAndDetachedSuitesAre(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const base = "43e5792"
	root := repoRoot(t)
	if out, code := run(t, root, nil, "git", "cat-file", "-e", base+"^{commit}"); code != 0 {
		t.Skipf("base commit %s not reachable (shallow clone?): %s", base, out)
	}
	if out, code := run(t, root, nil, "git", "diff", "--exit-code", base, "--",
		"internal/ops/recovery_test.go", "internal/ops/detached_test.go", "internal/ops/cancel_test.go"); code != 0 {
		t.Errorf("the recovery, detached or cancel suite differs from %s (CON-4): exit %d\n%s", base, code, out)
	}
	skipUnderMake(t)
	if out, code := run(t, root, nil, "go", "test", "-race", "-count=1", "-run", "Recovery|Detached|Cancel", "./internal/ops"); code != 0 {
		t.Errorf("the unchanged recovery, detached and cancel suites fail (CON-4): exit %d\n%s", code, out)
	}
}

// TC-SV-41: capture.sh is POSIX sh and read-only. Owner WI 3.1. Requirements: NFR-3, CON-1.
//
// - Setup: `repoRoot`.
// - Steps:
//  1. `sh -n capture.sh`, and `dash -n` when dash exists.
//  2. Scan each non-comment line.
//
// - Expected:
//   - the script parses;
//   - the shebang is `#!/bin/sh`;
//   - no line matches `systemctl +(start|stop|restart|reload|enable|disable|mask|kill|reboot|daemon-reload)`, `rc-service +\S+ +(start|stop|restart|zap)`, `rc-update +(add|del)`, `(^|[;&|] *)(/sbin/)?(reboot|poweroff|halt|shutdown)\b`, `\brm\b` or `kill`;
//   - every `>` redirect targets a path under `"$OUT"`;
//   - no Bash-only syntax (`[[`, `function `, arrays, `$'…'`).
//
// A `>&N` duplication (`2>&1`, `1>&2`) names a descriptor, not a path, so
// it is not a redirect target.
func TestTCSV41_CaptureShIsPOSIXShAndRead(t *testing.T) {
	script := filepath.Join(repoRoot(t), "internal", "svcctl", "testdata", "services", "capture.sh")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("capture.sh: %v", err)
	}
	for _, shell := range []string{"sh", "dash"} {
		if _, err := exec.LookPath(shell); err != nil {
			if shell == "sh" {
				t.Fatal("no sh on PATH")
			}
			t.Logf("%s not on PATH: not checked with it", shell)
			continue
		}
		if out, code := run(t, filepath.Dir(script), nil, shell, "-n", script); code != 0 {
			t.Errorf("%s -n capture.sh: exit %d\n%s", shell, code, out)
		}
	}
	text := string(b)
	if first, _, _ := strings.Cut(text, "\n"); first != "#!/bin/sh" {
		t.Errorf("shebang %q, want #!/bin/sh", first)
	}
	for i, line := range strings.Split(text, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		for _, f := range tcsv41Forbidden {
			if f.re.MatchString(code) {
				t.Errorf("capture.sh:%d %s: %s", i+1, f.what, line)
			}
		}
		for _, m := range tcsv41Redirect.FindAllStringSubmatch(code, -1) {
			target := m[1]
			if !strings.HasPrefix(target, "&") && !strings.HasPrefix(target, `"$OUT/`) {
				t.Errorf("capture.sh:%d writes outside \"$OUT\" (%s): %s", i+1, target, line)
			}
		}
	}
}

// tcsv41Forbidden are TC-SV-41's patterns: a state change, a deletion, a
// signal, or Bash-only syntax.
var tcsv41Forbidden = []struct {
	what string
	re   *regexp.Regexp
}{
	{"systemctl changes state", regexp.MustCompile(`systemctl +(start|stop|restart|reload|enable|disable|mask|kill|reboot|daemon-reload)`)},
	{"rc-service changes state", regexp.MustCompile(`rc-service +\S+ +(start|stop|restart|zap)`)},
	{"rc-update changes runlevels", regexp.MustCompile(`rc-update +(add|del)`)},
	{"power command", regexp.MustCompile(`(^|[;&|] *)(/sbin/)?(reboot|poweroff|halt|shutdown)\b`)},
	{"rm", regexp.MustCompile(`\brm\b`)},
	{"kill", regexp.MustCompile(`kill`)},
	{"Bash-only [[", regexp.MustCompile(`\[\[`)},
	{"Bash-only function keyword", regexp.MustCompile(`(^|[;&|] *)function `)},
	{"Bash-only array", regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*=\(|\$\{[A-Za-z_][A-Za-z0-9_]*\[`)},
	{"Bash-only $'...'", regexp.MustCompile(`\$'`)},
}

// tcsv41Redirect is an output redirect (>, >>, N>, N>>) and its target.
var tcsv41Redirect = regexp.MustCompile(`[0-9]*>>? *(&[0-9-]|"[^"]*"|[^ ;&|)]+)`)

// TC-SV-132: Base OOS-7 is amended in place, keeping the original text. Owner WI 5.2. Requirements: NFR-8.
//
// - Setup: The file at base and at head.
// - Steps:
//  1. Extract the OOS-7 section at both.
//
// - Expected: The base text of OOS-7 is still present verbatim, the head adds text naming `services-provider`, and the other sections are unchanged.
func TestTCSV132_BaseOOS7IsAmendedInPlace(t *testing.T) {
	t.Skip("TC-SV-132: SKIP-only stub; un-skip when WI 5.2 lands")
}

// TC-SV-134: The hardened unit and the OpenRC script are unchanged, and go.mod gains no module. Owner WI 5.2. Requirements: CON-1, CON-3, OOS-9.
//
// - Setup: Base `43e5792`.
// - Steps:
//  1. `git diff --exit-code 43e5792 -- deploy/nexusd.service deploy/nexusd.openrc`.
//  2. Compare the `require` blocks of `go.mod` at base and head.
//
// - Expected: No diff, and the same module set (versions may differ only by a security bump that the WI records).
func TestTCSV134_TheHardenedUnitAndTheOpenRCScript(t *testing.T) {
	t.Skip("TC-SV-134: SKIP-only stub; un-skip when WI 5.2 lands")
}
