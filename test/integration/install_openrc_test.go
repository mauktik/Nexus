//go:build integration

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package integration

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
)

// nginx-provider TC-33: The systemd branch of the installer behaves exactly as before, under dash
// Owner WI 3.2. Requirements: FR-24, NFR-9.
//
//	Setup: Ubuntu harness, using freshHost(t).
//	Steps:
//	  1. sh /harness/release/nexusd/install.sh (/bin/sh is dash on Ubuntu).
//	  2. Record files, modes, owners and hashes under /usr/local/bin,
//	    /etc/systemd/system, /etc/nexus and /var/lib/nexus.
//	  3. Repeat on a fresh host with the base SHA's install.sh (committed as
//	    test/harness/fixtures/base-install.sh from git show
//	    d04583a:deploy/install.sh), run with bash.
//	Expected:
//	  - The two recorded trees are identical, as are the stdout messages apart
//	    from timestamps.
//	  - The existing TC-179, TC-180 and TC-181 (base feature) still pass with sh
//	    instead of bash.
func TestSystemdInstallUnderDashUnchanged(t *testing.T) {
	requireInit(t, platform.Systemd)
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(shell) != "dash" {
		t.Fatalf("/bin/sh is %s, want dash", shell)
	}
	baseInstall := "/harness/fixtures/base-install.sh"
	roots := []string{"/usr/local/bin", "/etc/systemd/system", "/etc/nexus", nexusdState}

	// Step 1 and 2: the current installer under dash, on a fresh host.
	freshHost(t)
	out, err := sh(3*time.Minute, "/bin/sh", installSh)
	t.Logf("sh install.sh: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	requireExactUnit(t)
	gotTree := recordTrees(t, roots)
	gotOut := withoutTimestamps(out)

	// Step 3: the base SHA's installer under bash, on a fresh host, from a
	// copy of the same release tree.
	wipeInstall(t)
	dir := t.TempDir()
	for _, f := range []string{"nexusd", "nexusd.service", "nexusd.default.yaml"} {
		mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", filepath.Join(releaseTree, f), filepath.Join(dir, f))
	}
	mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", baseInstall, filepath.Join(dir, "install.sh"))
	out, err = sh(3*time.Minute, "bash", filepath.Join(dir, "install.sh"))
	t.Logf("bash base install.sh: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	wantTree := recordTrees(t, roots)
	wantOut := withoutTimestamps(out)

	if gotTree != wantTree {
		t.Errorf("the installed trees differ\nsh install.sh:\n%s\nbash base install.sh:\n%s", gotTree, wantTree)
	}
	if gotOut != wantOut {
		t.Errorf("the installer output differs\nsh install.sh:\n%s\nbash base install.sh:\n%s", gotOut, wantOut)
	}
}

// recordTrees lists every file, directory and link under roots with its
// mode, owner and SHA-256 or link target. Times are left out.
func recordTrees(t *testing.T, roots []string) string {
	t.Helper()
	var b strings.Builder
	for _, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(&b, "%s absent\n", root)
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := os.Lstat(path)
			if err != nil {
				return err
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("%s: no stat_t", path)
			}
			what := ""
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				what = "-> " + target
			case fi.Mode().IsRegular():
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				what = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
			}
			fmt.Fprintf(&b, "%s %v %d:%d %s\n", path, fi.Mode(), st.Uid, st.Gid, what)
			return nil
		})
		if err != nil {
			t.Fatalf("record %s: %v", root, err)
		}
	}
	return b.String()
}

var timestampRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`)

func withoutTimestamps(s string) string {
	return timestampRE.ReplaceAllString(s, "TIMESTAMP")
}

// nginx-provider TC-47: install.sh on OpenRC installs, never enables, and restarts only on upgrade
// Owner WI 3.4. Requirements: FR-24, CON-12, ASM-17.
//
//	Setup: Fresh Alpine host state, with no /usr/local/bin/nexusd, no
//	/etc/init.d/nexusd and no /etc/nexus.
//	Steps:
//	  1. sh install.sh.
//	  2. Inspect.
//	  3. Edit /etc/nexus/nexusd.yaml (append a comment) and run again.
//	  4. rc-service nexusd start, record the PID, and run sh install.sh again.
//	Expected:
//	  - After step 1:
//	    - /usr/local/bin/nexusd and /etc/init.d/nexusd are root:root 0755;
//	    - /etc/nexus/nexusd.yaml is 0600 with the default content;
//	    - rc-update show lists no nexusd;
//	    - rc-service nexusd status is stopped;
//	    - stdout contains rc-update add nexusd default and rc-service nexusd
//	      start;
//	    - the config's listen address is loopback.
//	  - Step 3 leaves the config hash unchanged.
//	  - Step 4 restarts nexusd: the PID changes and the service is started.
func TestOpenRCInstallFirstAndUpgrade(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-47 (WI 3.4)")
}

// nginx-provider TC-48: uninstall.sh on OpenRC keeps data unless purged and warns about live supervisors
// Owner WI 3.4. Requirements: FR-24.
//
//	Setup: Installed and started. rc-update add nexusd default. One running
//	detached op.
//	Steps:
//	  1. sh uninstall.sh.
//	  2. Reinstall, then sh uninstall.sh --purge.
//	Expected:
//	  - Step 1:
//	    - stdout warns about the running supervisor and names its op ID;
//	    - nexusd is stopped and removed from every runlevel;
//	    - /etc/init.d/nexusd and the binary are gone;
//	    - /etc/nexus, /var/lib/nexus, /var/log/nexus and the audit log remain;
//	    - the supervisor is still alive (it is not killed).
//	  - Step 2 also removes /etc/nexus, /var/lib/nexus, /var/log/nexus and
//	    /etc/conf.d/nexusd.
func TestOpenRCUninstallAndPurge(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-48 (WI 3.4)")
}

// nginx-provider TC-ERR7: install.sh refuses on an unsupported init system and changes nothing
// Owner WI 3.4. Requirements: FR-24, CON-12.
//
//	Setup: unshare -m sh -c 'mount -t tmpfs none /run && sh install.sh', which
//	hides /run/systemd and /run/openrc. Snapshot hashes of /usr/local/bin,
//	/etc/init.d, /etc/systemd/system and /etc/nexus first.
//	Steps:
//	  1. Run it.
//	Expected:
//	  - A non-zero exit, with stderr naming systemd and OpenRC.
//	  - The snapshot is unchanged.
func TestInstallRefusesUnknownInit(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR7 (WI 3.4)")
}
