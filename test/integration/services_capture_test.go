// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
)

// The T1 capture driver (services-provider WI 3.1). It is not a TC: it
// makes the container captures under internal/svcctl/testdata/services/
// that the svcctl tests read. Run it once per image:
//
//	make test-harness RUN='^TestServicesCapture$'                       # t1-ubuntu-systemd
//	make test-harness BASE=debian:stable RUN='^TestServicesCapture$'    # t1-debian-systemd
//	make test-harness BASE=debian:12 RUN='^TestServicesCapture$'        # t1-debian12-systemd
//	make test-harness-alpine RUN='^TestServicesCapture$'                # t1-alpine-openrc
//	make test-harness-alpine BASE=alpine:3.22 RUN='^TestServicesCapture$' # t1-alpine322-openrc
//
// The capture lands in build/harness/logs/<stamp>/services-capture/<name>/
// (run.sh copies /harness/out), and is committed from there; see
// internal/svcctl/testdata/README.md.

const (
	// captureScript is capture.sh where run.sh copies internal/svcctl/testdata.
	captureScript = "/harness/src/github.com/mauktik/nexus/internal/svcctl/testdata/services/capture.sh"
	// captureOut holds the captures; run.sh copies /harness/out to the logs.
	captureOut = hostOutDir + "/services-capture"
	// svcctlPath is svcctl's whole environment (internal/svcctl env()).
	svcctlPath = "PATH=/usr/sbin:/usr/bin:/sbin:/bin"
)

// captureName is the capture directory of this image: by os-release, the
// five T1 images of WI 3.1. NEXUS_CAPTURE_NAME overrides it.
func captureName(t *testing.T) string {
	t.Helper()
	if n := os.Getenv("NEXUS_CAPTURE_NAME"); n != "" {
		return n
	}
	o := platform.Current().OS
	switch {
	case o.ID == "ubuntu":
		return "t1-ubuntu-systemd"
	case o.ID == "debian" && o.VersionID == "12":
		return "t1-debian12-systemd"
	case o.ID == "debian":
		return "t1-debian-systemd"
	case o.ID == "alpine" && strings.HasPrefix(o.VersionID, "3.22."):
		return "t1-alpine322-openrc"
	case o.ID == "alpine":
		return "t1-alpine-openrc"
	}
	t.Fatalf("no capture name for os-release ID %q VERSION_ID %q: set NEXUS_CAPTURE_NAME", o.ID, o.VersionID)
	return ""
}

// The planted T1 services of test-strategy.md ("Planted T1 services").
var plantedUnits = map[string]string{
	"nx-svc-test.service": `[Unit]
Description=nexus T1 test service
[Service]
Type=simple
ExecStart=/bin/sh -c 'i=0; while :; do echo nx-svc-line-$i; i=$((i+1)); sleep 1; done'
`,
	"nx-svc-fail.service": `[Unit]
Description=nexus T1 failing service
[Service]
Type=oneshot
RemainAfterExit=no
ExecStart=/bin/false
`,
	"nx-svc-slow.service": `[Unit]
Description=nexus T1 slow-start service
[Service]
Type=simple
ExecStartPre=/bin/sleep 20
ExecStart=/bin/sleep infinity
`,
	"nx-svc-other.service": `[Unit]
Description=nexus T1 service outside the allow-list
[Service]
Type=simple
ExecStart=/bin/sh -c 'i=0; while :; do echo nx-svc-line-$i; i=$((i+1)); sleep 1; done'
`,
	"nx-svc-alias.service": `[Unit]
Description=nexus T1 aliased service
[Service]
Type=simple
ExecStart=/bin/sleep infinity
[Install]
Alias=nx-netalias.service
`,
}

var plantedScripts = map[string]string{
	"nx-svc-test": `#!/sbin/openrc-run
description="nexus T1 test service"
supervisor=supervise-daemon
command=/bin/sh
command_args="-c 'i=0; while :; do echo nx-svc-line-\$i; i=\$((i+1)); sleep 1; done'"
output_log=/var/log/nx-svc-test.log
error_log=/var/log/nx-svc-test.err
`,
	"nx-svc-fail": `#!/sbin/openrc-run
description="nexus T1 failing service"
command=/bin/false
`,
	"nx-svc-slow": `#!/sbin/openrc-run
description="nexus T1 slow-start service"
supervisor=supervise-daemon
command=/bin/sleep
command_args=3600
start_pre() { sleep 20; }
`,
}

// plantServices installs the planted T1 services for the detected init
// system and removes them in cleanup (unplantServices). On systemd
// nx-svc-alias is enabled, which links nx-netalias.service to it.
func plantServices(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { unplantServices(t) })
	switch detectedInit() {
	case platform.Systemd:
		for name, unit := range plantedUnits {
			if err := os.WriteFile(filepath.Join("/etc/systemd/system", name), []byte(unit), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		mustSh(t, detach.Systemctl, "daemon-reload")
		mustSh(t, detach.Systemctl, "enable", "nx-svc-alias.service")
	case platform.OpenRC:
		for name, script := range plantedScripts {
			if err := os.WriteFile(filepath.Join("/etc/init.d", name), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("no planted services for init %s", detectedInit())
	}
}

// unplantServices stops and removes the planted services. It logs what it
// cannot undo instead of failing, so the rest of the cleanup still runs.
func unplantServices(t *testing.T) {
	t.Helper()
	switch detectedInit() {
	case platform.Systemd:
		var units []string
		for name := range plantedUnits {
			units = append(units, name)
		}
		for _, args := range [][]string{
			append([]string{"stop"}, units...),
			{"disable", "nx-svc-alias.service"},
			append([]string{"reset-failed"}, units...),
		} {
			if out, err := sh(time.Minute, detach.Systemctl, args...); err != nil {
				t.Logf("cleanup: systemctl %s: %v %s", strings.Join(args, " "), err, out)
			}
		}
		for _, name := range append(units, "nx-netalias.service") {
			if err := os.Remove(filepath.Join("/etc/systemd/system", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Logf("cleanup: %v", err)
			}
		}
		if out, err := sh(time.Minute, detach.Systemctl, "daemon-reload"); err != nil {
			t.Logf("cleanup: systemctl daemon-reload: %v %s", err, out)
		}
	case platform.OpenRC:
		for name := range plantedScripts {
			for _, a := range []string{"stop", "zap"} {
				if out, err := sh(time.Minute, "rc-service", name, a); err != nil {
					t.Logf("cleanup: rc-service %s %s: %v %s", name, a, err, out)
				}
			}
			if err := os.Remove(filepath.Join("/etc/init.d", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Logf("cleanup: %v", err)
			}
		}
		for _, f := range []string{"/var/log/nx-svc-test.log", "/var/log/nx-svc-test.err"} {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Logf("cleanup: %v", err)
			}
		}
	}
}

// captureAction runs one control argv with svcctl's environment (env -i
// with svcctl's PATH and LC_ALL=C, no stdin), writes its combined output
// and an Exit line to dir/actions/file, and returns the PROVENANCE
// commands line.
func captureAction(t *testing.T, dir, file string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{svcctlPath, "LC_ALL=C"}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("%s: %v", strings.Join(argv, " "), err)
		}
		code = ee.ExitCode()
	}
	fmt.Fprintf(&out, "Exit %d\n", code)
	rel := "actions/" + file
	if err := os.WriteFile(filepath.Join(dir, rel), out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s -> exit %d", rel, strings.Join(argv, " "), code)
	return fmt.Sprintf("  %s: %s\n", rel, strings.Join(argv, " "))
}

func TestServicesCapture(t *testing.T) {
	name := captureName(t)
	initSys := detectedInit()
	freshHost(t)
	runInstall(t)
	writeConfig(t, hostConfig(true, ""))
	plantServices(t)

	var pid int
	switch initSys {
	case platform.Systemd:
		pid = startExact(t)
	case platform.OpenRC:
		lib(t, "start-nexusd")
		if pid = openrcMainPID(t); pid == 0 {
			t.Fatalf("nexusd is not started after start-nexusd: rc-service says %s", rcStatus(t))
		}
	default:
		t.Fatalf("no init system to capture (detected %s)", initSys)
	}
	t.Logf("capture %s: nexusd PID %d under the shipped %s service", name, pid, initSys)

	dir := filepath.Join(captureOut, name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "actions"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The planted services exercised once with svcctl's exact argv:
	// systemctl ACTION -- NAME.service, rc-service -C NAME ACTION.
	var actions strings.Builder
	act := func(file, verb, svc string) {
		var argv []string
		if initSys == platform.Systemd {
			argv = []string{"/usr/bin/systemctl", verb, "--", svc + ".service"}
		} else {
			argv = []string{"/sbin/rc-service", "-C", svc, verb}
		}
		actions.WriteString(captureAction(t, dir, file, argv...))
	}
	act("start-nx-svc-test.txt", "start", "nx-svc-test")
	if initSys == platform.OpenRC {
		act("start-already-nx-svc-test.txt", "start", "nx-svc-test")
	}
	act("stop-nx-svc-test.txt", "stop", "nx-svc-test")
	act("restart-nx-svc-test.txt", "restart", "nx-svc-test")
	act("start-nx-svc-fail.txt", "start", "nx-svc-fail")
	// A few journal or log lines of nx-svc-test and of nexusd exist
	// before the capture reads them.
	time.Sleep(2 * time.Second)

	out, err := sh(5*time.Minute, "/bin/sh", captureScript, name, dir)
	t.Logf("capture.sh: %v\n%s", err, strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("capture.sh %s: %v", name, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "PROVENANCE"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(actions.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	prov, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prov), "\nsource: container\n") {
		t.Errorf("PROVENANCE has no source: container:\n%s", prov)
	}
	t.Logf("%s/PROVENANCE:\n%s", dir, prov)
}
