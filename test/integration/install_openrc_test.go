// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// install.sh and uninstall.sh on both init systems (nginx-provider TC-33,
// TC-47, TC-48 and TC-ERR7), run with sh: dash on Ubuntu, BusyBox ash on
// Alpine.

package integration

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// fileSHA is the SHA-256 of a file's bytes.
func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

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
//	    2953509:deploy/install.sh), run with bash.
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
	requireInit(t, platform.OpenRC)
	freshHost(t)
	t.Cleanup(func() { _, _ = sh(time.Minute, "rc-service", "nexusd", "stop") })

	// Step 1 and 2.
	out := runInstall(t)
	requireMode(t, nexusdBin, 0o755)
	requireMode(t, openrcScript, 0o755)
	requireMode(t, nexusdConfig, 0o600)
	got, err := os.ReadFile(nexusdConfig)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(releaseTree, "nexusd.default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is not the default configuration", nexusdConfig)
	}
	if show := mustSh(t, "rc-update", "show", "-v"); regexp.MustCompile(`(?m)^[ \t]*nexusd[ \t]*\|[ \t]*\S`).MatchString(show) {
		t.Fatalf("rc-update show lists nexusd in a runlevel:\n%s", show)
	}
	if st := rcStatus(t); st != "stopped" {
		t.Fatalf("rc-service nexusd status: %s, want stopped", st)
	}
	for _, next := range []string{"rc-update add nexusd default", "rc-service nexusd start"} {
		if !strings.Contains(out, next) {
			t.Errorf("install.sh output lacks %q", next)
		}
	}
	listen := regexp.MustCompile(`(?m)^listen:.*$`).FindString(string(got))
	if !regexp.MustCompile(`^listen:\s*\[\s*"?127\.0\.0\.1:\d+"?\s*\]`).MatchString(listen) {
		t.Fatalf("default config %q does not listen on loopback only", listen)
	}
	t.Logf("first install: %s; %s", listen, strings.TrimSpace(mustSh(t, "ls", "-l", nexusdBin, openrcScript, nexusdConfig)))

	// Step 3: an operator's edit survives a reinstall.
	f, err := os.OpenFile(nexusdConfig, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("# operator edit tc47\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before := fileSHA(t, nexusdConfig)
	out = runInstall(t)
	if after := fileSHA(t, nexusdConfig); after != before {
		t.Fatalf("install.sh changed the edited config: %s -> %s", before, after)
	}
	if !strings.Contains(out, "kept the existing") {
		t.Errorf("install.sh output does not say it kept the config:\n%s", out)
	}

	// Step 4: on a running service, install.sh restarts it.
	lib(t, "start-nexusd")
	pid := openrcMainPID(t)
	out = runInstall(t)
	var pid2 int
	waitUntil(t, 30*time.Second, "a new nexusd after the upgrade", func() (bool, string) {
		pid2 = openrcMainPID(t)
		return pid2 != 0 && pid2 != pid, fmt.Sprint(pid2)
	})
	if st := rcStatus(t); st != "started" || !strings.Contains(out, "restarted the running nexusd service") {
		t.Fatalf("after the upgrade: status %s, output:\n%s", st, out)
	}
	t.Logf("upgrade restarted nexusd: pid %d -> %d", pid, pid2)

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
	requireInit(t, platform.OpenRC)
	freshHost(t)
	ensureUsers(t)
	runInstall(t)
	dir := testDir(t, "tc48")
	m := markers(t, "tc48", "root")
	script := sleepThenMark(t, filepath.Join(dir, "sleeper.sh"), filepath.Join(m, "marker"), 300)
	writeConfig(t, fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts:\n    enabled: true\n    catalog:\n%s",
		exactAddr, catalogYAML(entry{id: "sleeper", path: script})))
	d := &scriptsd{t: t, stateDir: nexusdState, auditDir: logDir, listen: exactAddr,
		tokens: map[string]string{"run": createToken(t, "tc48", spec.RunScope, spec.ReadScope)},
		client: &http.Client{Timeout: 30 * time.Second}}
	mustSh(t, "rc-update", "add", "nexusd", "default")
	lib(t, "start-nexusd")
	op := d.invoke("sleeper", nil)
	d.waitStatus(op.ID, apitypes.StatusRunning)
	ident := supervisorOf(t, d, op.ID)
	t.Cleanup(func() { killOpUnit(t, detach.Unit(op.ID)) })

	// Step 1: uninstall keeps the data and the running supervisor.
	out, err := sh(3*time.Minute, "sh", uninstallSh)
	t.Logf("sh uninstall.sh: %v\n%s", err, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "operation supervisors still running") || !strings.Contains(out, op.ID) {
		t.Errorf("uninstall.sh does not warn about the running supervisor of %s", op.ID)
	}
	// The supervisor is nexusd op-supervise too: only a daemon counts.
	if o, err := sh(10*time.Second, "pgrep", "-x", "-f", nexusdBin+" serve.*"); err == nil {
		t.Errorf("nexusd serve still runs after uninstall: %s", o)
	}
	if links, _ := filepath.Glob("/etc/runlevels/*/nexusd"); len(links) > 0 {
		t.Errorf("nexusd is still in runlevels: %v", links)
	}
	for _, gone := range []string{openrcScript, nexusdBin} {
		if exists(gone) {
			t.Errorf("%s remains after uninstall", gone)
		}
	}
	for _, kept := range []string{"/etc/nexus", nexusdState, logDir} {
		if !exists(kept) {
			t.Errorf("%s was removed without --purge", kept)
		}
	}
	if audit, _ := os.ReadDir(logDir); len(audit) == 0 {
		t.Errorf("the audit log in %s is gone", logDir)
	}
	if !pidAlive(ident.PID) || readProcStat(t, ident.PID).startTicks != ident.StartTicks {
		t.Errorf("supervisor %d of %s was killed by uninstall", ident.PID, op.ID)
	}
	t.Logf("after uninstall: supervisor %d alive; kept /etc/nexus, %s, %s", ident.PID, nexusdState, logDir)

	// Step 2: --purge removes the data and conf.d too.
	runInstall(t)
	if err := os.WriteFile(openrcConfD, []byte("# tc48\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = sh(3*time.Minute, "sh", uninstallSh, "--purge")
	t.Logf("sh uninstall.sh --purge: %v\n%s", err, out)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"/etc/nexus", nexusdState, logDir, openrcConfD, openrcScript, nexusdBin} {
		if exists(gone) {
			t.Errorf("%s remains after --purge", gone)
		}
	}

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
	roots := []string{"/usr/local/bin", "/etc/init.d", "/etc/systemd/system", "/etc/nexus"}
	before := recordTrees(t, roots)
	cmd := exec.Command("unshare", "-m", "sh", "-c", "mount -t tmpfs none /run && sh "+installSh)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	t.Logf("%s: %v\nstdout: %s\nstderr: %s", detectedInit(), err, stdout.String(), stderr.String())
	if err == nil {
		t.Fatal("install.sh succeeded with /run hidden")
	}
	if !strings.Contains(stderr.String(), "systemd") || !strings.Contains(stderr.String(), "OpenRC") {
		t.Errorf("stderr does not name systemd and OpenRC: %q", stderr.String())
	}
	if after := recordTrees(t, roots); after != before {
		t.Errorf("install.sh changed the host:\nbefore:\n%s\nafter:\n%s", before, after)
	}

}
