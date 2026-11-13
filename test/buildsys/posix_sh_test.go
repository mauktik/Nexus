// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md TC-32 and TC-34. TC-32's
// body is complete; TC-34's assertions are in its comment. The developer of
// WI 3.2 deletes the t.Skip lines. An un-skipped passing test is the GREEN
// signal (test-strategy.md, RED Stub Style).

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
	t.Skip("not implemented: nginx-provider TC-32 (WI 3.2)")

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
func TestDeployScriptAcceptsEitherInit(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-34 (WI 3.2)")
}
