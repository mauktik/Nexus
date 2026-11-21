// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md TC-29, TC-30 and TC-31.
// The bodies are complete; WI 3.1 removed the skips.
// An un-skipped passing test is the GREEN signal (test-strategy.md, RED Stub
// Style).

package buildsys

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// baseSHA is the commit the Ubuntu unit must stay byte-identical to (NFR-9).
const baseSHA = "295350991fecd02a25324cc8124b0c33e35045e1"

// nginx-provider TC-29: The OpenRC service script declares the required
// behavior. Owner WI 3.1. Requirements: FR-22, FR-23, ASM-17, CON-9.
func TestOpenRCScriptAudit(t *testing.T) {
	path := filepath.Join(repoRoot(t), "deploy", "nexusd.openrc")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	script := string(b)
	if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("sh -n %s: %v\n%s", path, err, out)
	}
	// checkbashisms skips (exit 2) a script whose #! is not /bin/sh unless
	// forced; #!/sbin/openrc-run scripts are POSIX sh.
	for _, lint := range [][]string{{"shellcheck", "-s", "sh"}, {"checkbashisms", "--force"}} {
		if _, err := exec.LookPath(lint[0]); err != nil {
			t.Logf("%s not on PATH: lint skipped", lint[0])
			continue
		}
		if out, err := exec.Command(lint[0], append(lint[1:], path)...).CombinedOutput(); err != nil {
			t.Errorf("%s %s: %v\n%s", lint[0], path, err, out)
		}
	}
	if !strings.HasPrefix(script, "#!/sbin/openrc-run\n") {
		t.Errorf("first line is not #!/sbin/openrc-run")
	}
	once := []string{
		`supervisor=supervise-daemon`,
		`command=/usr/local/bin/nexusd`,
		`command_args="serve --config /etc/nexus/nexusd.yaml"`,
		`command_user="root:root"`,
		`umask=077`,
		`rc_cgroup_cleanup="${rc_cgroup_cleanup:-NO}"`,
		`extra_started_commands="reload"`,
	}
	for _, line := range once {
		if n := countLines(script, line); n != 1 {
			t.Errorf("want exactly one line %q, found %d", line, n)
		}
	}
	for _, re := range []string{
		`(?m)^pidfile="?/run/`,
		`(?m)^respawn_delay=[1-9][0-9]*$`,
		`(?m)^respawn_max=[1-9][0-9]*$`,
		`(?m)^respawn_period=[1-9][0-9]*$`,
		`(?m)^output_logger="logger -t nexusd`,
		`(?m)^error_logger="logger -t nexusd`,
		`(?s)depend\(\)\s*\{[^}]*need net[^}]*\}`,
		`(?s)depend\(\)\s*\{[^}]*after firewall[^}]*\}`,
		`(?s)start_pre\(\)\s*\{[^}]*checkpath --directory --owner root:root --mode 0700[^}]*/var/lib/nexus[^}]*\}`,
		`(?s)start_pre\(\)\s*\{[^}]*/var/log/nexus[^}]*\}`,
		`(?s)reload\(\)\s*\{[^}]*supervise-daemon[^}]*--signal HUP[^}]*\}`,
	} {
		if !regexp.MustCompile(re).MatchString(script) {
			t.Errorf("script does not match %s", re)
		}
	}
	// The default must not override an operator's /etc/conf.d/nexusd value
	// (review Finding 1): exactly one assignment, the defaulting form above.
	if n := len(regexp.MustCompile(`(?m)^\s*rc_cgroup_cleanup=`).FindAllString(script, -1)); n != 1 {
		t.Errorf("want exactly one rc_cgroup_cleanup assignment, found %d", n)
	}
	// openrc-run sources /etc/conf.d/<service> before the service script.
	// Model that order: a prior YES must survive sourcing, and no prior value
	// must default to NO.
	for _, c := range []struct{ prior, want string }{
		{"rc_cgroup_cleanup=YES; ", "YES"},
		{"", "NO"},
	} {
		cmd := exec.Command("sh", "-c", c.prior+`. "$1"; printf %s "$rc_cgroup_cleanup"`, "sh", path)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("source %s with prior %q: %v", path, c.prior, err)
			continue
		}
		if got := string(out); got != c.want {
			t.Errorf("rc_cgroup_cleanup after sourcing with prior %q = %q, want %q", c.prior, got, c.want)
		}
	}
	// Supervisors must survive a stop: nothing but nexusd is signaled.
	for _, bad := range []string{"stop_post", "pkill", "killall", "op-supervise"} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains %q: a stop must affect only nexusd (FR-20 item 1)", bad)
		}
	}
}

// nginx-provider TC-30: The release tree carries the OpenRC script at 0755.
// Owner WI 3.1. Requirements: FR-23.
func TestReleaseTreeHasOpenRCScript(t *testing.T) {
	skipUnderMake(t)
	repo := copyRepo(t)
	mk, err := os.ReadFile(filepath.Join(repo, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^NEXUSD_FILES\s*:=.*deploy/nexusd\.openrc`).Match(mk) {
		t.Errorf("NEXUSD_FILES does not include deploy/nexusd.openrc")
	}
	// nexusd-tree only copies TREE_BIN, so any file stands in for the binary.
	bin := filepath.Join(t.TempDir(), "nexusd")
	if err := os.WriteFile(bin, []byte("stand-in"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	cmd := exec.Command("make", "--no-print-directory", "nexusd-tree", "TREE_BIN="+bin, "TREE_DIR="+tree)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make nexusd-tree: %v\n%s", err, out)
	}
	fi, err := os.Stat(filepath.Join(tree, "nexusd.openrc"))
	if err != nil {
		t.Fatalf("tree has no nexusd.openrc: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("nexusd.openrc mode %v, want 0755", fi.Mode().Perm())
	}
	unit, err := os.ReadFile(filepath.Join(tree, "nexusd.service"))
	if err != nil {
		t.Fatalf("tree has no nexusd.service: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(repo, "deploy", "nexusd.service"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(unit) != sha256.Sum256(src) {
		t.Errorf("tree nexusd.service differs from deploy/nexusd.service")
	}
}

// nginx-provider TC-31: deploy/nexusd.service is byte-identical to the base
// SHA. Owner WI 3.1. Requirements: NFR-9, CON-9.
func TestSystemdUnitByteIdenticalToBase(t *testing.T) {
	root := repoRoot(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	show := exec.Command("git", "show", baseSHA+":deploy/nexusd.service")
	show.Dir = root
	base, err := show.Output()
	if err != nil {
		t.Skipf("base commit %s not reachable (shallow clone?): %v", baseSHA, err)
	}
	cur, err := os.ReadFile(filepath.Join(root, "deploy", "nexusd.service"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(base) != sha256.Sum256(cur) {
		t.Fatalf("deploy/nexusd.service differs from %s (NFR-9 requires byte identity)", baseSHA)
	}
}

func countLines(text, line string) int {
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == line {
			n++
		}
	}
	return n
}
