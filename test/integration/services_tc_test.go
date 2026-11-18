//go:build integration

// T1 of the services provider (services-provider WI 5.3): the installed
// nexusd under the exact hardened unit (systemd: requireExactUnit) or the
// shipped OpenRC script, with the planted services of WI 3.1
// (plantServices). Every expected value is read back from the container's
// own tools (systemctl, journalctl, rc-status, rc-update, rc-service), its
// files (/proc, /run, the OpenRC log files, the audit log) and the T1
// captures, never from Nexus alone. Each TC is its own test, so
//
//	make test-harness RUN='^TestTCSV1(3[5-9]|4[0-7])_'
//	make test-harness-alpine RUN='^TestTCSV1(3[5-9]|4[0-7])_'
//
// select exactly them; a test written for the other init system skips
// through requireInit.
//
// No real reboot runs in T1. The reboot cases that need every guard to pass
// bind-mount a per-container boot ID over /proc/sys/kernel/random/boot_id
// before nexusd starts (svMountBootID), as LXC's lxc_setup_boot_id does, so
// preflight's container-mount branch runs for real. The one case that
// creates a reboot operation with every guard passing (TC-SV-145) uses the
// 300 s delay, cancels at once, and carries a fail-safe that kills the
// operation's helper about 290 s before the delay could end
// (svRebootFailSafe).

package integration

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/providers/services"
	svspec "github.com/mauktik/nexus/internal/providers/services/spec"
)

const (
	svServicesAt = "/v1/providers/services/services"
	svRebootAt   = "/v1/providers/services/actions/reboot"
	svOpsDir     = nexusdState + "/ops"
	svAuditFile  = logDir + "/audit.jsonl"
	// svBootIDFile is the per-container boot ID that svMountBootID mounts
	// over svBootIDPath (test-strategy.md, Test Data: T1 boot ID).
	svBootIDFile = "/run/nx-boot-id"
	svBootIDPath = "/proc/sys/kernel/random/boot_id"
	// svCancelBound is how long a canceled reboot may take to end before
	// the fail-safe runs (TC-SV-145).
	svCancelBound = 10 * time.Second
	// svRebootDelay is the delay of every reboot that can pass every guard:
	// the maximum, so the fail-safe lands about 290 s before the trigger.
	svRebootDelay = svspec.MaxDelaySeconds
)

// svScopes are the token's scopes: every services scope, and host:read for
// the host provider's readbacks (TC-SV-144, TC-SV-147).
var svScopes = []string{svspec.ReadScope, svspec.LogsScope, svspec.WriteScope, svspec.RebootScope, hostspec.ReadScope}

// svConfig is the config of these tests: hostConfig (loopback, plain HTTP,
// the host provider at 2 s) with the services section of test-strategy.md
// (Test Data: Config), nx-missing allow-listed but never installed
// (TC-SV-141), and reboot.enabled.
func svConfig(reboot bool) string {
	return hostConfig(true, "  services:\n    enabled: true\n    control:\n"+
		"      - {name: nx-svc-test}\n"+
		"      - {name: nx-svc-fail, timeout: 15s}\n"+
		"      - {name: nx-svc-slow, timeout: 60s}\n"+
		"      - {name: nx-svc-alias}\n"+
		"      - {name: nx-netalias}\n"+
		"      - {name: nx-missing}\n"+
		fmt.Sprintf("    reboot: {enabled: %t}\n", reboot))
}

// svHost is the installed nexusd with the services provider enabled.
type svHost struct {
	*nexusdInstance
	init platform.InitSystem
	// bootID is the per-container boot ID svMountBootID mounted; "" when
	// nothing is mounted.
	bootID string
}

type svOpts struct {
	reboot bool // providers.services.reboot.enabled
	mount  bool // the boot-ID bind mount before nexusd starts
	atBoot bool // nexusd enabled at boot (systemctl enable, rc-update add)
}

// svSetup is svInstall, the boot-ID mount when o.mount, and svStart.
func svSetup(t *testing.T, o svOpts) *svHost {
	t.Helper()
	h := svInstall(t, o)
	if o.mount {
		svMountBootID(t, h)
	}
	svStart(t, h)
	return h
}

// svInstall installs nexusd with install.sh on a wiped host, plants the
// test services, writes svConfig and, when o.atBoot, enables nexusd at
// boot (install.sh does not), disabling it again at cleanup.
func svInstall(t *testing.T, o svOpts) *svHost {
	t.Helper()
	init := detectedInit()
	if init != platform.Systemd && init != platform.OpenRC {
		t.Fatalf("the harness has no supported init system (detected %s)", init)
	}
	freshHost(t)
	runInstall(t)
	plantServices(t)
	writeConfig(t, svConfig(o.reboot))
	if o.atBoot {
		svSetAtBoot(t, init, true)
		t.Cleanup(func() {
			if out, err := svAtBootCmd(init, false); err != nil {
				t.Logf("cleanup: disable nexusd at boot: %v %s", err, out)
			}
		})
	}
	return &svHost{init: init}
}

func svAtBootCmd(init platform.InitSystem, on bool) (string, error) {
	switch {
	case init == platform.Systemd && on:
		return sh(time.Minute, detach.Systemctl, "enable", nexusdUnit)
	case init == platform.Systemd:
		return sh(time.Minute, detach.Systemctl, "disable", nexusdUnit)
	case on:
		return sh(time.Minute, "rc-update", "add", "nexusd", "default")
	}
	return sh(time.Minute, "rc-update", "del", "nexusd", "default")
}

// svSetAtBoot enables or disables nexusd at boot with the init system's
// own tool.
func svSetAtBoot(t *testing.T, init platform.InitSystem, on bool) {
	t.Helper()
	if out, err := svAtBootCmd(init, on); err != nil {
		t.Fatalf("nexusd at boot %t: %v %s", on, err, out)
	}
}

// svStart starts nexusd under the exact unit (systemd) or the shipped
// script (OpenRC), mints the token once, and waits until capabilities
// report the services provider enabled.
func svStart(t *testing.T, h *svHost) {
	t.Helper()
	if h.init == platform.Systemd {
		pid := startExact(t)
		t.Logf("nexusd.service: %s sha256 %s = deploy/nexusd.service, no drop-in; MainPID %d",
			installedUnit, sha(t, installedUnit), pid)
	} else {
		requireShippedScript(t)
		lib(t, "start-nexusd")
		pid := openrcMainPID(t)
		if pid == 0 {
			t.Fatalf("nexusd is not started after start-nexusd: rc-service says %s", rcStatus(t))
		}
		t.Logf("nexusd under the shipped OpenRC script: %s sha256 %s, PID %d", openrcScript, sha(t, openrcScript), pid)
	}
	if h.nexusdInstance == nil {
		h.nexusdInstance = &nexusdInstance{t: t, listen: exactAddr, token: createToken(t, "sv", svScopes...),
			client: &http.Client{Timeout: 60 * time.Second}}
	}
	waitUntil(t, 30*time.Second, "the services provider enabled in capabilities", func() (bool, string) {
		code, b := h.get("/v1/capabilities")
		if code != http.StatusOK {
			return false, fmt.Sprintf("%d %s", code, b)
		}
		var c apitypes.Capabilities
		if err := json.Unmarshal(b, &c); err != nil {
			return false, err.Error()
		}
		p, ok := svProviderOf(c)
		if !ok {
			return false, "no services provider"
		}
		return p.Status == apitypes.ProviderEnabled, string(p.Status) + " " + p.Reason
	})
}

// svMainPID is nexusd's PID: MainPID, or on OpenRC openrcMainPID.
func svMainPID(t *testing.T, h *svHost) int {
	t.Helper()
	if h.init == platform.OpenRC {
		return openrcMainPID(t)
	}
	pid, err := strconv.Atoi(showUnit(t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil || pid <= 0 {
		t.Fatalf("nexusd MainPID: %v", err)
	}
	return pid
}

// svMountBootID writes a fresh UUID to /run/nx-boot-id and bind-mounts it
// over /proc/sys/kernel/random/boot_id. Cleanup stops nexusd first, so it
// never reads the host kernel's boot ID again in this test, then unmounts
// it.
func svMountBootID(t *testing.T, h *svHost) {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(b))
	if err := os.WriteFile(svBootIDFile, []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSh(t, "mount", "--bind", svBootIDFile, svBootIDPath)
	t.Cleanup(func() {
		if out, err := sh(2*time.Minute, "bash", libSh, "stop-nexusd"); err != nil {
			t.Logf("cleanup: stop nexusd: %v %s", err, out)
		}
		if out, err := sh(time.Minute, "umount", svBootIDPath); err != nil {
			t.Errorf("cleanup: umount %s: %v %s", svBootIDPath, err, out)
		}
		if err := os.Remove(svBootIDFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Logf("cleanup: %v", err)
		}
	})
	if got := svReadTrim(t, svBootIDPath); got != id {
		t.Fatalf("%s reads %q after the bind mount, want %q", svBootIDPath, got, id)
	}
	h.bootID = id
	t.Logf("boot ID %s bind-mounted from %s over %s", id, svBootIDFile, svBootIDPath)
}

func svReadTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func svProviderOf(c apitypes.Capabilities) (apitypes.ProviderCapability, bool) {
	for _, p := range c.Providers {
		if p.Name == svspec.Name {
			return p, true
		}
	}
	return apitypes.ProviderCapability{}, false
}

// svCaps is GET /v1/capabilities (with refresh, the preflights run again
// first) and the services provider's entry.
func svCaps(t *testing.T, h *svHost, refresh bool) (apitypes.Capabilities, apitypes.ProviderCapability) {
	t.Helper()
	path := "/v1/capabilities"
	if refresh {
		path += "?refresh=true"
	}
	var c apitypes.Capabilities
	h.mustGet(path, &c)
	p, ok := svProviderOf(c)
	if !ok {
		t.Fatalf("no services provider in capabilities: %+v", c.Providers)
	}
	return c, p
}

// svDetail decodes one capabilities detail into into.
func svDetail(t *testing.T, p apitypes.ProviderCapability, key string, into any) {
	t.Helper()
	v, ok := p.Details[key]
	if !ok {
		t.Fatalf("capabilities details have no %s: %v", key, p.Details)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("detail %s %s: %v", key, b, err)
	}
}

// svRequireContainerMount is the first assertion of every reboot case that
// mounts a boot ID: preflight reports bootIdSource container-mount and the
// reboot available.
func svRequireContainerMount(t *testing.T, h *svHost) {
	t.Helper()
	_, p := svCaps(t, h, false)
	var source *string
	var avail bool
	svDetail(t, p, services.DetailBootIDSource, &source)
	svDetail(t, p, services.DetailRebootAvailable, &avail)
	if source == nil || *source != services.BootIDContainerMount || !avail {
		// nexusd's own view, from its mount namespace: whether the mount
		// reached it, and the boot ID it reads.
		pid := svMainPID(t, h)
		mi, _ := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
		var lines []string
		for l := range strings.Lines(string(mi)) {
			if strings.Contains(l, "/proc/sys") {
				lines = append(lines, strings.TrimSpace(l))
			}
		}
		seen, err := os.ReadFile(fmt.Sprintf("/proc/%d/root%s", pid, svBootIDPath))
		t.Fatalf("bootIdSource %v rebootAvailable %t, want %s and true; reason %q; nexusd (PID %d) reads boot_id %q (%v), "+
			"container %q; nexusd's /proc/sys mounts %q", source, avail, services.BootIDContainerMount,
			p.Details[services.DetailRebootUnavailableReason], pid, strings.TrimSpace(string(seen)), err, h.bootID, lines)
	}
	t.Logf("capabilities: bootIdSource %s, rebootAvailable true", *source)
}

// svReload sends SIGHUP to nexusd and waits until capabilities
// report rebootEnabled as want.
func svReload(t *testing.T, h *svHost, want bool) {
	t.Helper()
	pid := svMainPID(t, h)
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP nexusd %d: %v", pid, err)
	}
	waitUntil(t, 15*time.Second, fmt.Sprintf("capabilities rebootEnabled %t after the reload", want), func() (bool, string) {
		_, p := svCaps(t, h, false)
		v, _ := p.Details[services.DetailRebootEnabled].(bool)
		return v == want, fmt.Sprint(p.Details[services.DetailRebootEnabled])
	})
}

// svProblem decodes a problem answer and requires its status and slug.
func svProblem(t *testing.T, what string, code int, b []byte, wantCode int, wantSlug problem.Slug) problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %d, body is not a problem: %s", what, code, b)
	}
	if code != wantCode || p.Slug() != wantSlug {
		t.Fatalf("%s: %d %s, want %d %s: %s", what, code, p.Slug(), wantCode, wantSlug, b)
	}
	return p
}

// svAccepted requires 202 and returns the operation.
func svAccepted(t *testing.T, what string, code int, b []byte) apitypes.Operation {
	t.Helper()
	if code != http.StatusAccepted {
		t.Fatalf("%s: %d %s, want 202", what, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil || op.ID == "" {
		t.Fatalf("%s: %v %s", what, err, b)
	}
	return op
}

// svAct posts action on a service and waits for its end.
func svAct(t *testing.T, h *svHost, name, action string) (apitypes.Operation, svspec.ServiceResult) {
	t.Helper()
	code, b := h.post(svServicesAt+"/"+name+"/"+action, "{}")
	op := h.waitOp(svAccepted(t, action+" "+name, code, b).ID, 3*time.Minute)
	var res svspec.ServiceResult
	if op.Status == apitypes.StatusSucceeded {
		if err := json.Unmarshal(op.Result, &res); err != nil {
			t.Fatalf("%s %s result %s: %v", action, name, op.Result, err)
		}
	}
	t.Logf("%s %s: operation %s %s, result %s, error %v", action, name, op.ID, op.Status, op.Result, op.Error)
	return op, res
}

// svWord is the first line a command prints on stdout, whatever its exit
// status (systemctl is-active exits 3 for an inactive unit).
func svWord(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	l, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(l)
}

// svStdout is a command's stdout; any failure fails the test.
func svStdout(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := stdoutOf(time.Minute, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return out
}

// svShow is `systemctl show --timestamp=unix` of props for name.service.
func svShow(t *testing.T, name string, props ...string) map[string]string {
	t.Helper()
	out := mustSh(t, detach.Systemctl, "show", "--timestamp=unix", "-p", strings.Join(props, ","), "--", name+".service")
	m := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			m[k] = v
		}
	}
	return m
}

// svUnixTime is a `--timestamp=unix` value ("@SECONDS"), or nil.
func svUnixTime(v string) *time.Time {
	s, ok := strings.CutPrefix(v, "@")
	if !ok {
		return nil
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return nil
	}
	tm := time.Unix(sec, 0).UTC()
	return &tm
}

func svSameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// The state mappings of TC-SV-46 and TC-SV-49: svcctl's ActiveState and
// OpenRC status mappings, exactly; any other word is unknown.
var (
	svActiveStates = map[string]string{"active": "running", "reloading": "running", "activating": "starting",
		"deactivating": "stopping", "inactive": "stopped", "failed": "failed"}
	svRCStates = map[string]string{"started": "running", "stopped": "stopped", "crashed": "failed",
		"starting": "starting", "stopping": "stopping"}
)

func svSystemdState(word string) string {
	if s, ok := svActiveStates[word]; ok {
		return s
	}
	return "unknown"
}

func svOpenRCState(word string) string {
	if s, ok := svRCStates[word]; ok {
		return s
	}
	return "unknown"
}

// svEnabledWord is TC-SV-46's enabled mapping: the word when it is one of
// the FR-4 words other than unknown, else unknown.
func svEnabledWord(word string) string {
	if word != "unknown" && slices.Contains(svspec.EnabledValues, word) {
		return word
	}
	return "unknown"
}

var svRCStatusWord = regexp.MustCompile(`status: (\S+)`)

// svRCStatus is `rc-service -C NAME status`'s word.
func svRCStatus(t *testing.T, name string) string {
	t.Helper()
	out, _ := sh(time.Minute, "rc-service", "-C", name, "status")
	if m := svRCStatusWord.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	t.Fatalf("rc-service -C %s status: %q", name, out)
	return ""
}

// svChildPID is supervise-daemon's child_pid of an OpenRC service, or 0.
func svChildPID(name string) int {
	b, err := os.ReadFile(filepath.Join("/run/openrc/options", name, "child_pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// svPIDInfo describes pid for a failure: its executable and parent.
func svPIDInfo(pid int) string {
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return fmt.Sprintf("%d (exe %s, PPid %s)", pid, exe, procStatusField(pid, "PPid"))
}

// svList pages through the list with the default page size and returns
// every service by name, requiring each page in name order.
func svList(t *testing.T, h *svHost) map[string]svspec.ServiceProperties {
	t.Helper()
	out := map[string]svspec.ServiceProperties{}
	var last string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("more than 100 pages")
		}
		path := svServicesAt
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		var l apitypes.List[svspec.Service]
		h.mustGet(path, &l)
		for _, s := range l.Items {
			if s.Name <= last {
				t.Fatalf("list: %q after %q: not in name order or repeated", s.Name, last)
			}
			last = s.Name
			out[s.Name] = s.Properties
		}
		if l.NextCursor == "" {
			return out
		}
		cursor = l.NextCursor
	}
}

func svGet(t *testing.T, h *svHost, name string) svspec.ServiceDetails {
	t.Helper()
	var d svspec.ServiceDetail
	h.mustGet(svServicesAt+"/"+name, &d)
	return d.Properties
}

// svOpEntries is the operation directories under /var/lib/nexus/ops.
func svOpEntries(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(svOpsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// svOpUnits is every nexus-op-* unit systemd knows, loaded or not; nil on
// OpenRC, where an operation is a supervised process (svOpEntries counts
// it).
func svOpUnits(t *testing.T) []string {
	t.Helper()
	if detectedInit() != platform.Systemd {
		return nil
	}
	var out []string
	for line := range strings.Lines(mustSh(t, detach.Systemctl, "list-units", "--all", "--plain", "--no-legend",
		"--no-pager", detach.UnitPrefix+"*")) {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// svAuditRec is one audit.jsonl record (systemspec.AuditRecord's fields
// these tests read).
type svAuditRec struct {
	ID          string            `json:"id"`
	Action      string            `json:"action"`
	Target      string            `json:"target"`
	Outcome     string            `json:"outcome"`
	Problem     string            `json:"problem"`
	OperationID string            `json:"operationId"`
	RequestID   string            `json:"requestId"`
	Facts       map[string]string `json:"facts"`
}

// svAudit is the audit log's records after its first mark.
func svAudit(t *testing.T, mark int) []svAuditRec {
	t.Helper()
	b, err := os.ReadFile(svAuditFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []svAuditRec
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" || i < mark {
			continue
		}
		var r svAuditRec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%s line %d %q: %v", svAuditFile, i+1, line, err)
		}
		out = append(out, r)
	}
	return out
}

func svAuditMark(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(svAuditFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	// Every record ends in a newline, so an empty file has none.
	return strings.Count(string(b), "\n")
}

// svRefused is the refused records of action ("" is every action) after
// mark, waiting up to 5 s for want of them.
func svRefused(t *testing.T, mark int, action string, want int) []svAuditRec {
	t.Helper()
	var got []svAuditRec
	deadline := time.Now().Add(5 * time.Second)
	for {
		got = got[:0]
		for _, r := range svAudit(t, mark) {
			if (action == "" || r.Action == action) && r.Outcome == "refused" {
				got = append(got, r)
			}
		}
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// svPID1 is the container's PID 1 start time (/proc/1/stat field 22).
func svPID1(t *testing.T) uint64 {
	t.Helper()
	return readProcStat(t, 1).startTicks
}

// svCapture is a file of this image's T1 capture (WI 3.1), which run.sh
// copies in with internal/svcctl/testdata.
func svCapture(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(captureScript), captureName(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// svCaptureExit is the Exit line of a captured action.
func svCaptureExit(t *testing.T, rel string) int {
	t.Helper()
	s := svCapture(t, rel)
	i := strings.LastIndex(s, "Exit ")
	if i < 0 {
		t.Fatalf("%s has no Exit line:\n%s", rel, s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[i+len("Exit "):]))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return n
}

// svRequireOpUnit checks that operation id ran in its own transient unit,
// nexus-op-<id>.service: the journal has entries of that unit, and
// nexusd.service's journal has no helper line. The helper's output goes to
// the operation log, not the journal, so the unit's entries are systemd's
// own records of it (UNIT=nexus-op-<id>.service, _SYSTEMD_UNIT=init.scope:
// started, deactivated), unless a later systemd also logs under
// _SYSTEMD_UNIT=nexus-op-<id>.service.
func svRequireOpUnit(t *testing.T, id string, since time.Time) {
	t.Helper()
	unit := detach.Unit(id)
	out := mustSh(t, "journalctl", "--no-pager", "-o", "json", "--output-fields=_SYSTEMD_UNIT,UNIT,MESSAGE", "-u", unit)
	units := map[string]int{}
	n := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			SystemdUnit string `json:"_SYSTEMD_UNIT"`
			Unit        string `json:"UNIT"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		n++
		units["_SYSTEMD_UNIT="+e.SystemdUnit]++
		units["UNIT="+e.Unit]++
	}
	if n == 0 || units["_SYSTEMD_UNIT="+unit]+units["UNIT="+unit] == 0 {
		t.Errorf("operation %s: the journal has no entry of %s (%d entries: %v)", id, unit, n, units)
	} else {
		t.Logf("operation %s ran in %s: %d journal entries %v", id, unit, n, units)
	}
	nj := mustSh(t, "journalctl", "--no-pager", "-o", "cat", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10))
	for line := range strings.Lines(nj) {
		if strings.Contains(line, services.HelperCommand) || strings.Contains(line, id) && strings.Contains(line, "through") {
			t.Errorf("operation %s: nexusd.service's journal has the helper's line: %s", id, strings.TrimSpace(line))
		}
	}
}

// TC-SV-135: T1 systemd list and get agree with systemctl in the same container. Owner WI 5.3. Requirements: FR-4, FR-5, NFR-2, CON-1, CON-3.
//
// - Setup: `requireInit(systemd)`, the planted services, `startExact`, `requireExactUnit`, and the token of test-strategy.md.
// - Steps:
//  1. Page through `GET /v1/providers/services/services`.
//  2. At the same moment, run `systemctl list-units --type=service --all --plain --no-legend` and `systemctl list-unit-files --type=service --plain --no-legend`, and `systemctl is-active`/`is-enabled` per name.
//  3. `GET …/services/nexusd` and `…/nx-svc-test`, compared with `systemctl show -p …`.
//
// - Expected:
//   - names are equal, and state and enabled are equal per name by TC-SV-46's mapping;
//   - a name whose state changed between the two reads (a timer-driven unit) is retried once and must then agree;
//   - the failed set equals `systemctl list-units --state=failed --type=service`'s names and contains `nx-svc-fail` (failed once at setup);
//   - get fields equal `show`'s.
func TestTCSV135_T1SystemdListAndGetAgreeWith(t *testing.T) {
	requireInit(t, platform.Systemd)
	h := svSetup(t, svOpts{})
	requireExactUnit(t)
	mustSh(t, detach.Systemctl, "start", "nx-svc-test.service")
	if out, err := sh(time.Minute, detach.Systemctl, "start", "nx-svc-fail.service"); err == nil {
		t.Fatalf("systemctl start nx-svc-fail.service succeeded: %s", out)
	}

	// The tool's view: the listings' names, and each name's words.
	type view struct {
		names    []string
		inUnits  map[string]bool
		fileWord map[string]string
	}
	listings := func() view {
		v := view{inUnits: map[string]bool{}, fileWord: map[string]string{}}
		var names []string
		for line := range strings.Lines(svStdout(t, detach.Systemctl, "list-units", "--type=service", "--all", "--plain",
			"--no-legend", "--no-pager", "--full")) {
			if f := strings.Fields(line); len(f) > 0 && strings.HasSuffix(f[0], ".service") {
				n := strings.TrimSuffix(f[0], ".service")
				v.inUnits[n] = true
				names = append(names, n)
			}
		}
		for line := range strings.Lines(svStdout(t, detach.Systemctl, "list-unit-files", "--type=service", "--plain",
			"--no-legend", "--no-pager", "--full")) {
			if f := strings.Fields(line); len(f) > 1 && strings.HasSuffix(f[0], ".service") {
				n := strings.TrimSuffix(f[0], ".service")
				v.fileWord[n] = f[1]
				names = append(names, n)
			}
		}
		slices.Sort(names)
		v.names = slices.Compact(names)
		return v
	}
	wantOf := func(v view, name string) (state, enabled, active, isEnabled string) {
		active = svWord(t, detach.Systemctl, "is-active", "--", name+".service")
		isEnabled = svWord(t, detach.Systemctl, "is-enabled", "--", name+".service")
		state = svSystemdState(active)
		switch {
		case strings.HasSuffix(name, "@"):
			// A template is no unit is-active can name: stopped (TC-SV-48).
			state = "stopped"
		case !v.inUnits[name] && v.fileWord[name] == "alias":
			// is-active follows an alias to a target no listing names:
			// unknown (TC-SV-46).
			state = "unknown"
		}
		return state, svEnabledWord(isEnabled), active, isEnabled
	}
	compare := func(got map[string]svspec.ServiceProperties, v view, only []string) []string {
		var bad []string
		for _, name := range only {
			p, ok := got[name]
			if !ok {
				bad = append(bad, name+": not listed")
				continue
			}
			state, enabled, active, isEnabled := wantOf(v, name)
			if p.State != state || p.Enabled != enabled {
				bad = append(bad, fmt.Sprintf("%s: state %q enabled %q, want %q %q (is-active %q, is-enabled %q)",
					name, p.State, p.Enabled, state, enabled, active, isEnabled))
			}
		}
		return bad
	}
	nameOf := func(s string) string { n, _, _ := strings.Cut(s, ":"); return n }

	got, v := svList(t, h), listings()
	gotNames := slices.Sorted(func(yield func(string) bool) {
		for n := range got {
			if !yield(n) {
				return
			}
		}
	})
	if !slices.Equal(gotNames, v.names) {
		t.Logf("names differ on the first read; retrying once: list %d names, tools %d", len(gotNames), len(v.names))
		got, v = svList(t, h), listings()
		gotNames = slices.Sorted(func(yield func(string) bool) {
			for n := range got {
				if !yield(n) {
					return
				}
			}
		})
		if !slices.Equal(gotNames, v.names) {
			t.Fatalf("list names %q, want the union of list-units and list-unit-files %q", gotNames, v.names)
		}
	}
	bad := compare(got, v, v.names)
	if len(bad) > 0 {
		t.Logf("%d names disagree on the first read; retrying them once: %q", len(bad), bad)
		var again []string
		for _, b := range bad {
			again = append(again, nameOf(b))
		}
		if bad = compare(svList(t, h), listings(), again); len(bad) > 0 {
			t.Errorf("list disagrees with systemctl after a retry:\n%s", strings.Join(bad, "\n"))
		}
	}
	t.Logf("list: %d services, equal to the listings' union, states and enabled equal to is-active/is-enabled", len(got))

	var failed []string
	for line := range strings.Lines(svStdout(t, detach.Systemctl, "list-units", "--state=failed", "--type=service",
		"--plain", "--no-legend", "--no-pager", "--full")) {
		if f := strings.Fields(line); len(f) > 0 && strings.HasSuffix(f[0], ".service") {
			failed = append(failed, strings.TrimSuffix(f[0], ".service"))
		}
	}
	slices.Sort(failed)
	var gotFailed []string
	for n, p := range svList(t, h) {
		if p.State == "failed" {
			gotFailed = append(gotFailed, n)
		}
	}
	slices.Sort(gotFailed)
	if !slices.Equal(gotFailed, failed) || !slices.Contains(failed, "nx-svc-fail") {
		t.Errorf("failed set %q, want systemctl list-units --state=failed %q, with nx-svc-fail", gotFailed, failed)
	}
	t.Logf("failed set %q", gotFailed)

	for _, name := range []string{"nexusd", "nx-svc-test"} {
		bad := svCompareGet(t, h, name)
		if len(bad) > 0 {
			t.Logf("get %s disagrees on the first read; retrying once: %q", name, bad)
			bad = svCompareGet(t, h, name)
		}
		for _, b := range bad {
			t.Errorf("get %s: %s", name, b)
		}
	}
}

// svCompareGet compares get name with `systemctl show` of DF-1's
// properties, read just after it.
func svCompareGet(t *testing.T, h *svHost, name string) []string {
	t.Helper()
	d := svGet(t, h, name)
	s := svShow(t, name, "Id", "LoadState", "ActiveState", "SubState", "UnitFileState", "Description", "MainPID",
		"ActiveEnterTimestamp", "InactiveEnterTimestamp", "Result", "NRestarts", "MemoryCurrent", "FragmentPath")
	var bad []string
	eq := func(field, got, want string) {
		if got != want {
			bad = append(bad, fmt.Sprintf("%s %q, want %q", field, got, want))
		}
	}
	str := func(p *string) string {
		if p == nil {
			return "<null>"
		}
		return *p
	}
	eq("state", d.State, svSystemdState(s["ActiveState"]))
	eq("rawState", d.RawState, s["ActiveState"]+" ("+s["SubState"]+")")
	eq("subState", str(d.SubState), s["SubState"])
	eq("result", str(d.Result), s["Result"])
	eq("enabledRaw", d.EnabledRaw, s["UnitFileState"])
	eq("enabled", d.Enabled, svEnabledWord(s["UnitFileState"]))
	eq("description", str(d.Description), s["Description"])
	eq("unitFilePath", str(d.UnitFilePath), s["FragmentPath"])
	eq("canonicalName", str(d.CanonicalName), strings.TrimSuffix(s["Id"], ".service"))
	pid := "<null>"
	if d.MainPID != nil {
		pid = strconv.Itoa(*d.MainPID)
	}
	wantPID := s["MainPID"]
	if wantPID == "0" {
		wantPID = "<null>"
	}
	eq("mainPid", pid, wantPID)
	restarts := "<null>"
	if d.Restarts != nil {
		restarts = strconv.Itoa(*d.Restarts)
	}
	eq("restarts", restarts, s["NRestarts"])
	ts := "InactiveEnterTimestamp"
	if d.State == "running" {
		ts = "ActiveEnterTimestamp"
	}
	if !svSameTime(d.Since, svUnixTime(s[ts])) {
		bad = append(bad, fmt.Sprintf("since %v, want %s %q", d.Since, ts, s[ts]))
	}
	switch m := s["MemoryCurrent"]; m {
	case "[not set]", "18446744073709551615", "":
		if d.MemoryBytes != nil {
			bad = append(bad, fmt.Sprintf("memoryBytes %d, want null (MemoryCurrent %q)", *d.MemoryBytes, m))
		}
	default:
		// The process allocates between the two reads: equal within 25%.
		want, err := strconv.ParseInt(m, 10, 64)
		if err != nil || d.MemoryBytes == nil || float64(*d.MemoryBytes) < 0.75*float64(want) ||
			float64(*d.MemoryBytes) > 1.25*float64(want) {
			bad = append(bad, fmt.Sprintf("memoryBytes %v, want about MemoryCurrent %s", d.MemoryBytes, m))
		}
	}
	if len(bad) == 0 {
		t.Logf("get %s equals systemctl show: state %s (%s), mainPid %s, since %v, unit %s, enabled %s",
			name, d.State, d.RawState, pid, d.Since, str(d.UnitFilePath), d.EnabledRaw)
	}
	return bad
}

// svRCStatusLine is one `rc-status -s -C` line: NAME [ WORD ... ].
var svRCStatusLine = regexp.MustCompile(`^\s*(\S+)\s+\[\s*(\S+)[^\]]*\]\s*$`)

// TC-SV-136: T1 OpenRC list and get agree with rc-status, rc-update and rc-service. Owner WI 5.3. Requirements: FR-4, FR-5, NFR-2, CON-3.
//
// - Setup: `requireInit(openrc)`, the planted scripts, `nexusd` under supervise-daemon (`openrcMainPID`).
// - Steps:
//  1. List, then `rc-status -s -C` and `rc-update show -v`.
//  2. Get `nx-svc-test`, then `rc-service -C nx-svc-test status`.
//
// - Expected: Names, states (TC-SV-49's mapping), runlevels and enabled are equal. Get's state equals `rc-service`'s, and `scriptPath` is `/etc/init.d/nx-svc-test`.
func TestTCSV136_T1OpenRCListAndGetAgreeWith(t *testing.T) {
	requireInit(t, platform.OpenRC)
	h := svSetup(t, svOpts{})
	mustSh(t, "rc-service", "nx-svc-test", "start")

	type view struct {
		names     []string
		word      map[string]string
		runlevels map[string][]string
	}
	tools := func() view {
		v := view{word: map[string]string{}, runlevels: map[string][]string{}}
		var names []string
		for line := range strings.Lines(svStdout(t, "rc-status", "-s", "-C")) {
			if m := svRCStatusLine.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
				v.word[m[1]] = m[2]
				names = append(names, m[1])
			}
		}
		for line := range strings.Lines(svStdout(t, "rc-update", "show", "-v")) {
			if l, r, ok := strings.Cut(line, "|"); ok && strings.TrimSpace(l) != "" {
				n := strings.TrimSpace(l)
				v.runlevels[n] = strings.Fields(r)
				names = append(names, n)
			}
		}
		slices.Sort(names)
		v.names = slices.Compact(names)
		return v
	}
	compare := func(got map[string]svspec.ServiceProperties, v view) []string {
		var bad []string
		names := slices.Sorted(func(yield func(string) bool) {
			for n := range got {
				if !yield(n) {
					return
				}
			}
		})
		if !slices.Equal(names, v.names) {
			return []string{fmt.Sprintf("names %q, want rc-status ∪ rc-update %q", names, v.names)}
		}
		for _, n := range names {
			p := got[n]
			word, ok := v.word[n]
			if !ok {
				// rc-status -s leaves out scripts its keywords exclude;
				// rc-service reports them stopped (TC-SV-49).
				word = "stopped"
			}
			enabled := "disabled"
			if len(v.runlevels[n]) > 0 {
				enabled = "enabled"
			}
			if p.State != svOpenRCState(word) || p.RawState != word || p.Enabled != enabled ||
				!slices.Equal(p.Runlevels, v.runlevels[n]) && len(p.Runlevels)+len(v.runlevels[n]) > 0 {
				bad = append(bad, fmt.Sprintf("%s: state %q raw %q enabled %q runlevels %q, want %q %q %q %q", n,
					p.State, p.RawState, p.Enabled, p.Runlevels, svOpenRCState(word), word, enabled, v.runlevels[n]))
			}
		}
		return bad
	}
	got, v := svList(t, h), tools()
	if bad := compare(got, v); len(bad) > 0 {
		t.Logf("the first read disagrees; retrying once: %q", bad)
		got, v = svList(t, h), tools()
		for _, b := range compare(got, v) {
			t.Error(b)
		}
	}
	t.Logf("list: %d services equal to rc-status -s -C and rc-update show -v", len(got))

	d := svGet(t, h, "nx-svc-test")
	word := svRCStatus(t, "nx-svc-test")
	if d.State != svOpenRCState(word) || d.RawState != word || word != "started" {
		t.Errorf("get nx-svc-test: state %q raw %q, rc-service status %q (want started)", d.State, d.RawState, word)
	}
	if d.ScriptPath == nil || *d.ScriptPath != "/etc/init.d/nx-svc-test" {
		t.Errorf("get nx-svc-test: scriptPath %v, want /etc/init.d/nx-svc-test", d.ScriptPath)
	}
	if d.MainPID != nil {
		t.Logf("get nx-svc-test: mainPid %s; supervise-daemon's child_pid %d", svPIDInfo(*d.MainPID), svChildPID("nx-svc-test"))
	}
	if !slices.Equal(d.Runlevels, v.runlevels["nx-svc-test"]) && len(d.Runlevels)+len(v.runlevels["nx-svc-test"]) > 0 {
		t.Errorf("get nx-svc-test: runlevels %q, want %q", d.Runlevels, v.runlevels["nx-svc-test"])
	}
	t.Logf("get nx-svc-test: %s (%s), scriptPath %s; rc-service says %s", d.State, d.RawState, *d.ScriptPath, word)
}

// TC-SV-137: T1 logs agree with journalctl and with the declared OpenRC log file. Owner WI 5.3. Requirements: FR-6, NFR-2, CON-1, ASM-8.
//
// - Setup: `nx-svc-test` started, run for ≥ 8 s, then stopped with the host's own tool (`systemctl stop` / `rc-service stop`), so its log no longer grows and the two reads see the same lines. `T_start` is when it was started.
// - Steps:
//  1. Systemd: `GET …/nx-svc-test/logs?tail=5`, then `journalctl -u nx-svc-test.service -n 5 -o cat`; and `since=<T_start>` with no tail.
//  2. OpenRC: `logs tail=5`, then `tail -n 5 /var/log/nx-svc-test.log`.
//  3. `logs?priority=err` on OpenRC.
//
// - Expected:
//   - the 5 frame messages equal the tool's 5 lines, in the same order;
//   - the since frames equal `journalctl -u nx-svc-test.service --since=@<T_start> -o cat`'s `nx-svc-line-*` lines, starting at `nx-svc-line-0`, with consecutive numbers;
//   - (3) 400 `validation-failed`.
func TestTCSV137_T1LogsAgreeWithJournalctlAndWith(t *testing.T) {
	h := svSetup(t, svOpts{})
	// Every earlier nx-svc-line is at least a second before T_start.
	time.Sleep(1100 * time.Millisecond)
	tStart := time.Now().Truncate(time.Second)
	if h.init == platform.Systemd {
		mustSh(t, detach.Systemctl, "start", "nx-svc-test.service")
	} else {
		mustSh(t, "rc-service", "nx-svc-test", "start")
	}
	time.Sleep(9 * time.Second)
	if h.init == platform.Systemd {
		mustSh(t, detach.Systemctl, "stop", "nx-svc-test.service")
	} else {
		mustSh(t, "rc-service", "nx-svc-test", "stop")
	}

	messages := func(fs []apitypes.Frame) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.D)
		}
		return out
	}
	lines := func(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }
	frames, ok := h.frames(svServicesAt+"/nx-svc-test/logs?tail=5", 30*time.Second, nil)
	if !ok {
		t.Fatal("the tail=5 stream did not end by itself")
	}
	var want []string
	if h.init == platform.Systemd {
		want = lines(svStdout(t, "journalctl", "--no-pager", "-u", "nx-svc-test.service", "-n", "5", "-o", "cat"))
	} else {
		want = lines(svStdout(t, "tail", "-n", "5", "/var/log/nx-svc-test.log"))
	}
	if got := messages(frames); len(want) != 5 || !slices.Equal(got, want) {
		t.Errorf("logs tail=5: %q, want the tool's 5 lines %q", got, want)
	} else {
		t.Logf("logs tail=5 = the tool's last 5 lines %q", got)
	}

	if h.init == platform.OpenRC {
		code, slug, p := h.problem(svServicesAt + "/nx-svc-test/logs?priority=err")
		if code != http.StatusBadRequest || slug != problem.ValidationFailed {
			t.Errorf("logs priority=err on OpenRC: %d %s %s, want 400 validation-failed", code, slug, p.Detail)
		} else {
			t.Logf("logs priority=err on OpenRC: 400 validation-failed: %s", p.Detail)
		}
		return
	}

	since := url.QueryEscape(tStart.UTC().Format(time.RFC3339))
	frames, ok = h.frames(svServicesAt+"/nx-svc-test/logs?since="+since, 30*time.Second, nil)
	if !ok {
		t.Fatal("the since stream did not end by itself")
	}
	numbered := func(ls []string) []string {
		var out []string
		for _, l := range ls {
			if strings.HasPrefix(l, "nx-svc-line-") {
				out = append(out, l)
			}
		}
		return out
	}
	got := numbered(messages(frames))
	want = numbered(lines(svStdout(t, "journalctl", "--no-pager", "-u", "nx-svc-test.service",
		"--since=@"+strconv.FormatInt(tStart.Unix(), 10), "-o", "cat")))
	if !slices.Equal(got, want) {
		t.Errorf("logs since=%s: %q, want journalctl --since's lines %q", tStart.Format(time.RFC3339), got, want)
	}
	if len(got) < 8 {
		t.Errorf("logs since: %d nx-svc-line lines over a run of 9 s, want at least 8", len(got))
	}
	for i, l := range got {
		if l != fmt.Sprintf("nx-svc-line-%d", i) {
			t.Errorf("logs since: line %d is %q, want nx-svc-line-%d (from nx-svc-line-0, consecutive)", i, l, i)
			break
		}
	}
	t.Logf("logs since=%s: %d lines nx-svc-line-0 to %s, equal to journalctl --since", tStart.Format(time.RFC3339),
		len(got), got[len(got)-1])
}

// TC-SV-138: T1 start, stop and restart of nx-svc-test match the host's own state readback. Owner WI 5.3. Requirements: FR-7, NFR-4, CON-1, CON-3.
//
// - Setup: `nx-svc-test` stopped and allow-listed.
// - Steps:
//  1. POST start, wait, then `systemctl is-active` / `rc-service -C nx-svc-test status`.
//  2. Note `MainPID` and `ActiveEnterTimestamp`, POST restart, wait, and read both again.
//  3. POST stop, wait, and read back.
//  4. POST stop again.
//
// - Expected:
//   - (1) the result `state: running` and the tool says `active` / `started`;
//   - (2) the result `running`, with a new MainPID and a later timestamp, matching the tool;
//   - (3) `stopped`, and the tool says `inactive` / `stopped`;
//   - (4) `changed: false`, with `ActiveEnterTimestamp`/`InactiveEnterTimestamp` unchanged;
//   - on systemd, each action ran in a `nexus-op-<id>` unit (journal `_SYSTEMD_UNIT`), not in `nexusd.service`.
func TestTCSV138_T1StartStopAndRestartOfNx(t *testing.T) {
	since := time.Now().Add(-time.Second)
	h := svSetup(t, svOpts{})
	systemd := h.init == platform.Systemd
	const name = "nx-svc-test"

	// readback is the tool's own word, the main PID and the times the
	// service entered its states (on OpenRC, the main process's start).
	type readback struct {
		word           string
		pid            int
		active         string // ActiveEnterTimestamp, @SECONDS
		inactive       string // InactiveEnterTimestamp, @SECONDS
		activeMono     uint64 // ActiveEnterTimestampMonotonic, or the PID's start ticks
		inactiveMono   uint64
		activeUnixTime *time.Time
	}
	read := func() readback {
		if systemd {
			s := svShow(t, name, "MainPID", "ActiveEnterTimestamp", "InactiveEnterTimestamp",
				"ActiveEnterTimestampMonotonic", "InactiveEnterTimestampMonotonic")
			var r readback
			r.word = svWord(t, detach.Systemctl, "is-active", "--", name+".service")
			r.pid, _ = strconv.Atoi(s["MainPID"])
			r.active, r.inactive = s["ActiveEnterTimestamp"], s["InactiveEnterTimestamp"]
			r.activeMono, _ = strconv.ParseUint(s["ActiveEnterTimestampMonotonic"], 10, 64)
			r.inactiveMono, _ = strconv.ParseUint(s["InactiveEnterTimestampMonotonic"], 10, 64)
			r.activeUnixTime = svUnixTime(r.active)
			return r
		}
		r := readback{word: svRCStatus(t, name)}
		if r.word == "started" {
			r.pid = svChildPID(name)
			if r.pid > 0 {
				r.activeMono = readProcStat(t, r.pid).startTicks
			}
		}
		return r
	}
	if r := read(); r.word != map[bool]string{true: "inactive", false: "stopped"}[systemd] {
		t.Fatalf("%s is %q before the test, want stopped", name, r.word)
	}
	wantPID := func(what string, res svspec.ServiceResult, r readback) {
		t.Helper()
		if res.MainPID == nil || *res.MainPID != r.pid || r.pid <= 0 {
			got := "null"
			if res.MainPID != nil {
				got = svPIDInfo(*res.MainPID)
			}
			t.Errorf("%s: result mainPid %s, the tool's main PID %s", what, got, svPIDInfo(r.pid))
		}
	}
	var ids []string

	op, res := svAct(t, h, name, svspec.ActionStart)
	ids = append(ids, op.ID)
	r1 := read()
	if op.Status != apitypes.StatusSucceeded || res.State != "running" || !res.Changed ||
		r1.word != map[bool]string{true: "active", false: "started"}[systemd] {
		t.Fatalf("start: %s state %q changed %t; the tool says %q", op.Status, res.State, res.Changed, r1.word)
	}
	wantPID("start", res, r1)
	if systemd && !svSameTime(res.Since, r1.activeUnixTime) {
		t.Errorf("start: result since %v, ActiveEnterTimestamp %s", res.Since, r1.active)
	}
	t.Logf("start: running, MainPID %d; the tool says %s", r1.pid, r1.word)

	op, res = svAct(t, h, name, svspec.ActionRestart)
	ids = append(ids, op.ID)
	r2 := read()
	if op.Status != apitypes.StatusSucceeded || res.State != "running" || !res.Changed ||
		r2.word != r1.word {
		t.Fatalf("restart: %s state %q changed %t; the tool says %q", op.Status, res.State, res.Changed, r2.word)
	}
	wantPID("restart", res, r2)
	if r2.pid == r1.pid || r2.activeMono <= r1.activeMono {
		t.Errorf("restart: MainPID %d -> %d, entered running %d -> %d: want a new PID and a later time",
			r1.pid, r2.pid, r1.activeMono, r2.activeMono)
	}
	if systemd && !svSameTime(res.Since, r2.activeUnixTime) {
		t.Errorf("restart: result since %v, ActiveEnterTimestamp %s", res.Since, r2.active)
	}
	t.Logf("restart: running, MainPID %d -> %d, entered running %d -> %d", r1.pid, r2.pid, r1.activeMono, r2.activeMono)

	op, res = svAct(t, h, name, svspec.ActionStop)
	ids = append(ids, op.ID)
	r3 := read()
	if op.Status != apitypes.StatusSucceeded || res.State != "stopped" || !res.Changed || res.MainPID != nil ||
		r3.word != map[bool]string{true: "inactive", false: "stopped"}[systemd] {
		t.Fatalf("stop: %s state %q changed %t mainPid %v; the tool says %q", op.Status, res.State, res.Changed,
			res.MainPID, r3.word)
	}
	if systemd && !svSameTime(res.Since, svUnixTime(r3.inactive)) {
		t.Errorf("stop: result since %v, InactiveEnterTimestamp %s", res.Since, r3.inactive)
	}
	t.Logf("stop: stopped; the tool says %s", r3.word)

	op, res = svAct(t, h, name, svspec.ActionStop)
	ids = append(ids, op.ID)
	r4 := read()
	if op.Status != apitypes.StatusSucceeded || res.State != "stopped" || res.Changed {
		t.Fatalf("second stop: %s state %q changed %t, want Succeeded stopped changed false", op.Status, res.State, res.Changed)
	}
	if !(r4.word == r3.word && r4.active == r3.active && r4.inactive == r3.inactive &&
		r4.activeMono == r3.activeMono && r4.inactiveMono == r3.inactiveMono && r4.pid == r3.pid) {
		t.Errorf("second stop changed the unit: %+v -> %+v", r3, r4)
	}
	t.Logf("second stop: changed false; the tool's readback is unchanged (%s, entered %s/%d)", r4.word, r4.inactive,
		r4.inactiveMono)

	if systemd {
		for _, id := range ids {
			svRequireOpUnit(t, id, since)
		}
	}
}

// TC-SV-139: T1 a failed start reports service-action-failed and the host shows the unit failed. Owner WI 5.3. Requirements: FR-7, NFR-4.
//
// - Setup: `nx-svc-fail` allow-listed (timeout 15 s), after `reset-failed` / `zap`.
// - Steps:
//  1. POST start, wait.
//  2. `systemctl show -p ActiveState,Result nx-svc-fail` / `rc-service status`.
//
// - Expected:
//   - Failed `service-action-failed`, with `state: failed`, `exitCode` equal to `systemctl start`'s own exit code (1 on systemd; the OpenRC captured code), and the output quoted ≤ 2 KiB;
//   - the tool reports `failed` (`Result=exit-code`) / `crashed` or `stopped`, as the T1 capture of the same action shows.
func TestTCSV139_T1AFailedStartReportsServiceAction(t *testing.T) {
	h := svSetup(t, svOpts{})
	systemd := h.init == platform.Systemd
	// reset-failed exits 1 for a unit that is not failed, as a fresh
	// nx-svc-fail is not; the state is checked below either way.
	reset := []string{detach.Systemctl, "reset-failed", "nx-svc-fail.service"}
	if !systemd {
		reset = []string{"rc-service", "nx-svc-fail", "zap"}
	}
	out, err := sh(time.Minute, reset[0], reset[1:]...)
	t.Logf("%s: %v %s", strings.Join(reset, " "), err, strings.TrimSpace(out))
	if systemd {
		if st := svWord(t, detach.Systemctl, "is-active", "--", "nx-svc-fail.service"); st != "inactive" {
			t.Fatalf("nx-svc-fail is %q after reset-failed, want inactive", st)
		}
	} else if w := svRCStatus(t, "nx-svc-fail"); w != "stopped" {
		t.Fatalf("nx-svc-fail is %q after zap, want stopped", w)
	}
	wantExit := svCaptureExit(t, "actions/start-nx-svc-fail.txt")
	if systemd && wantExit != 1 {
		t.Fatalf("the T1 capture's systemctl start nx-svc-fail exited %d, want 1", wantExit)
	}

	op, _ := svAct(t, h, "nx-svc-fail", svspec.ActionStart)
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != problem.ServiceActionFailed {
		t.Fatalf("start nx-svc-fail: %s %v, want Failed service-action-failed", op.Status, op.Error)
	}
	ext := op.Error.Extensions
	state, _ := ext["state"].(string)
	exit, _ := ext["exitCode"].(float64)
	if int(exit) != wantExit {
		t.Errorf("exitCode %v, want %d (the T1 capture of the same action, actions/start-nx-svc-fail.txt)", ext["exitCode"], wantExit)
	}
	marker := fmt.Sprintf(" exited %d: ", wantExit)
	i := strings.Index(op.Error.Detail, marker)
	if i < 0 {
		t.Errorf("detail quotes no output after %q: %s", marker, op.Error.Detail)
	} else if q := op.Error.Detail[i+len(marker):]; len(q) > 2<<10 {
		t.Errorf("the quoted output is %d bytes, over 2 KiB", len(q))
	}

	if systemd {
		s := svShow(t, "nx-svc-fail", "ActiveState", "Result")
		if s["ActiveState"] != "failed" || s["Result"] != "exit-code" || state != "failed" {
			t.Errorf("systemctl show nx-svc-fail: %v, problem state %q; want failed, Result=exit-code, state failed", s, state)
		}
		t.Logf("start nx-svc-fail: Failed service-action-failed, state %s, exitCode %d; systemctl show: %v; detail %q",
			state, int(exit), s, op.Error.Detail)
		return
	}
	word := svRCStatus(t, "nx-svc-fail")
	oracle := ""
	for line := range strings.Lines(svCapture(t, "oracle/rc-service-status.tsv")) {
		if f := strings.Split(strings.TrimRight(line, "\n"), "\t"); len(f) >= 2 && f[0] == "nx-svc-fail" {
			oracle = f[1]
		}
	}
	if (word != "crashed" && word != "stopped") || word != oracle || state != svOpenRCState(word) {
		t.Errorf("rc-service -C nx-svc-fail status %q, the T1 capture's %q, problem state %q: want crashed or stopped, "+
			"equal to the capture, with the state its mapping", word, oracle, state)
	}
	t.Logf("start nx-svc-fail: Failed service-action-failed, state %s, exitCode %d; rc-service says %s (capture %s); "+
		"detail %q", state, int(exit), word, oracle, op.Error.Detail)
}

// svDeptree is OpenRC's own dependency tree, /run/openrc/deptree: each
// service's ineed and providedby values (depinfo_N_service,
// depinfo_N_ineed_M, depinfo_N_providedby_M).
func svDeptree(t *testing.T) map[string]map[string][]string {
	t.Helper()
	b, err := os.ReadFile("/run/openrc/deptree")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`^depinfo_(\d+)_([a-z]+)(?:_\d+)?='([^']*)'$`)
	names := map[string]string{}
	vals := map[string]map[string][]string{}
	for l := range strings.Lines(string(b)) {
		m := line.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		if m[2] == "service" {
			names[m[1]] = m[3]
			continue
		}
		if vals[m[1]] == nil {
			vals[m[1]] = map[string][]string{}
		}
		vals[m[1]][m[2]] = append(vals[m[1]][m[2]], m[3])
	}
	out := map[string]map[string][]string{}
	for idx, name := range names {
		if _, dup := out[name]; !dup {
			out[name] = vals[idx]
			if out[name] == nil {
				out[name] = map[string][]string{}
			}
		}
	}
	return out
}

// svDeptreeNeeds is what nexusd's deptree entry resolves to: each ineed
// name, or the providedby names of its entry (a virtual such as net).
func svDeptreeNeeds(t *testing.T, d map[string]map[string][]string) []string {
	t.Helper()
	var out []string
	for _, n := range d["nexusd"]["ineed"] {
		if p := d[n]["providedby"]; len(p) > 0 {
			out = append(out, p...)
		} else {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// svAliasNetworkd links nx-netalias.service to systemd-networkd.service
// (test-strategy.md, Test Data), in place of plantServices' link to
// nx-svc-alias; unplantServices removes it.
func svAliasNetworkd(t *testing.T) {
	t.Helper()
	mustSh(t, detach.Systemctl, "disable", "nx-svc-alias.service")
	frag := svShow(t, "systemd-networkd", "FragmentPath")["FragmentPath"]
	if frag == "" {
		t.Fatal("systemd-networkd.service has no unit file in this image, so nx-netalias cannot alias it")
	}
	link := "/etc/systemd/system/nx-netalias.service"
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(frag, link); err != nil {
		t.Fatal(err)
	}
	mustSh(t, detach.Systemctl, "daemon-reload")
	if id := svShow(t, "nx-netalias", "Id")["Id"]; id != "systemd-networkd.service" {
		t.Fatalf("nx-netalias.service has Id %q, want systemd-networkd.service", id)
	}
}

// TC-SV-140: T1 every FR-9 refusal sends nothing: the state is unchanged and no nexus-op unit appears. Owner WI 5.3. Requirements: FR-9, NFR-4, FR-16.
//
// - Setup:
//   - systemd names `nexusd`, `ssh`, `dbus`, `systemd-journald`, `getty@tty1` and `nx-netalias` (canonical `systemd-networkd`);
//   - OpenRC names `nexusd`, `sshd`, `networking` and the `net` provider;
//   - before each request, read `ActiveEnterTimestamp`/`MainPID` (or `rc-service status`) and count `nexus-op-*` units (`systemctl list-units --all 'nexus-op-*'`) or the op directories.
//
// - Steps:
//  1. POST start, stop and restart for each name, and `control` with each action.
//
// - Expected:
//   - each answers 409 `protected-service`. The static names are refused by name before any lookup, so this holds even where the container lacks the service (for example `ssh`);
//   - the readbacks are unchanged, and the `nexus-op-*` count and `/var/lib/nexus/ops/` entries are unchanged;
//   - the audit log file has one `refused` record per request with slug `protected-service`.
func TestTCSV140_T1EveryFR9RefusalSendsNothing(t *testing.T) {
	h := svInstall(t, svOpts{})
	var names []string
	if h.init == platform.Systemd {
		svAliasNetworkd(t)
		names = []string{"nexusd", "ssh", "dbus", "systemd-journald", "getty@tty1", "nx-netalias"}
	} else {
		// The net provider is what nexusd's deptree entry resolves net to.
		// On T1 the deptree resolves no net for nexusd (TC-SV-147), so the
		// static networking, Alpine's net provider, stands for it.
		d := svDeptree(t)
		var net []string
		if slices.Contains(d["nexusd"]["ineed"], "net") {
			net = d["net"]["providedby"]
		}
		t.Logf("deptree: nexusd ineed %q, net providedby %q: net provider(s) %q", d["nexusd"]["ineed"],
			d["net"]["providedby"], net)
		names = append([]string{"nexusd", "sshd", "networking"}, net...)
		slices.Sort(names)
		names = slices.Compact(names)
	}
	svStart(t, h)

	readback := func(name string) string {
		if h.init == platform.Systemd {
			s := svShow(t, name, "LoadState", "ActiveState", "SubState", "MainPID", "ActiveEnterTimestampMonotonic",
				"InactiveEnterTimestampMonotonic")
			return fmt.Sprint(s)
		}
		out, _ := sh(time.Minute, "rc-service", "-C", name, "status")
		pid := svChildPID(name)
		if name == "nexusd" {
			pid = openrcMainPID(t)
		}
		return fmt.Sprintf("%s pid %d", strings.TrimSpace(out), pid)
	}

	mark := svAuditMark(t)
	type req struct{ op, path, body string }
	requests := 0
	for _, name := range names {
		var rs []req
		for _, a := range svspec.Actions {
			rs = append(rs, req{svspec.Name + ".services." + a, svServicesAt + "/" + name + "/" + a, "{}"})
		}
		for _, a := range svspec.Actions {
			rs = append(rs, req{svspec.Control.Op().ID, svServicesAt + "/" + name + "/control", fmt.Sprintf(`{"action":%q}`, a)})
		}
		for _, r := range rs {
			before, units, dirs := readback(name), svOpUnits(t), svOpEntries(t)
			code, b := h.post(r.path, r.body)
			p := svProblem(t, "POST "+r.path+" "+r.body, code, b, http.StatusConflict, problem.ProtectedService)
			after, unitsAfter, dirsAfter := readback(name), svOpUnits(t), svOpEntries(t)
			if after != before || !slices.Equal(units, unitsAfter) || !slices.Equal(dirs, dirsAfter) {
				t.Errorf("POST %s %s changed something: readback %s -> %s, nexus-op units %q -> %q, op dirs %q -> %q",
					r.path, r.body, before, after, units, unitsAfter, dirs, dirsAfter)
			}
			requests++
			t.Logf("POST %s %s: 409 protected-service (%s); readback unchanged: %s; %d nexus-op units, %d op dirs",
				r.path, r.body, p.Extensions["reason"], after, len(units), len(dirs))
		}
	}
	refused := svRefused(t, mark, "", requests)
	var bad []string
	n := 0
	for _, r := range svAudit(t, mark) {
		if r.Outcome != "refused" {
			continue
		}
		n++
		if r.Problem != string(problem.ProtectedService) || !strings.HasPrefix(r.Action, svspec.Name+".services.") {
			bad = append(bad, fmt.Sprintf("%s %s %s", r.Action, r.Target, r.Problem))
		}
	}
	if n != requests || len(bad) > 0 {
		t.Errorf("audit: %d refused records for %d requests (%d counted by svRefused), not protected-service: %q",
			n, requests, len(refused), bad)
	}
	t.Logf("audit: %d refused records, each protected-service, for %d requests on %q", n, requests, names)
}

// TC-SV-141: T1 not-allowed and unknown names create nothing. Owner WI 5.3. Requirements: FR-7, FR-8.
//
// - Setup: `nx-svc-other` installed but not allow-listed. `nx-missing` allow-listed but not installed (capabilities show `status: missing`).
// - Steps:
//  1. POST start `nx-svc-other` and start `nx-missing`.
//
// - Expected: 409 `not-allowed` (`configKey: providers.services.control`) and 404 `not-found` respectively, with no operation and `nx-svc-other` still inactive.
func TestTCSV141_T1NotAllowedAndUnknownNamesCreate(t *testing.T) {
	h := svInstall(t, svOpts{})
	if h.init == platform.OpenRC {
		// plantServices has no OpenRC nx-svc-other: this case plants it.
		const script = "/etc/init.d/nx-svc-other"
		if err := os.WriteFile(script, []byte("#!/sbin/openrc-run\ndescription=\"nexus T1 service outside the allow-list\"\n"+
			"supervisor=supervise-daemon\ncommand=/bin/sleep\ncommand_args=3600\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, a := range []string{"stop", "zap"} {
				if out, err := sh(time.Minute, "rc-service", "nx-svc-other", a); err != nil {
					t.Logf("cleanup: rc-service nx-svc-other %s: %v %s", a, err, out)
				}
			}
			if err := os.Remove(script); err != nil {
				t.Logf("cleanup: %v", err)
			}
		})
	}
	svStart(t, h)
	inactive := func() string {
		if h.init == platform.Systemd {
			return svWord(t, detach.Systemctl, "is-active", "--", "nx-svc-other.service")
		}
		return svRCStatus(t, "nx-svc-other")
	}
	wantInactive := map[bool]string{true: "inactive", false: "stopped"}[h.init == platform.Systemd]

	_, p := svCaps(t, h, false)
	var allow []services.AllowListEntry
	svDetail(t, p, services.DetailControlAllowList, &allow)
	i := slices.IndexFunc(allow, func(e services.AllowListEntry) bool { return e.Name == "nx-missing" })
	if i < 0 || allow[i].Status != services.StatusMissing {
		t.Fatalf("capabilities controlAllowList %+v: want nx-missing with status missing", allow)
	}
	t.Logf("capabilities: nx-missing %s (%s)", allow[i].Status, allow[i].Reason)

	units, dirs := svOpUnits(t), svOpEntries(t)
	if w := inactive(); w != wantInactive {
		t.Fatalf("nx-svc-other is %q before the request, want %s", w, wantInactive)
	}
	code, b := h.post(svServicesAt+"/nx-svc-other/start", "{}")
	np := svProblem(t, "start nx-svc-other", code, b, http.StatusConflict, problem.NotAllowed)
	if np.Extensions["configKey"] != "providers.services.control" {
		t.Errorf("not-allowed configKey %v, want providers.services.control", np.Extensions["configKey"])
	}
	code, b = h.post(svServicesAt+"/nx-missing/start", "{}")
	mp := svProblem(t, "start nx-missing", code, b, http.StatusNotFound, problem.NotFound)
	if u, d := svOpUnits(t), svOpEntries(t); !slices.Equal(u, units) || !slices.Equal(d, dirs) {
		t.Errorf("an operation was created: nexus-op units %q -> %q, op dirs %q -> %q", units, u, dirs, d)
	}
	if w := inactive(); w != wantInactive {
		t.Errorf("nx-svc-other is %q after the request, want %s", w, wantInactive)
	}
	t.Logf("start nx-svc-other: 409 not-allowed (%s); start nx-missing: 404 not-found (%s); no operation, nx-svc-other %s",
		np.Detail, mp.Detail, wantInactive)
}

// TC-SV-142: T1 the reboot is unavailable without a boot-ID mount and available with one. Owner WI 5.3. Requirements: FR-3, FR-10, A-3, ASM-7.
//
// - Setup: `reboot.enabled: true` written with `writeConfig`. `/run/nx-boot-id` written as in test-strategy Test Data.
// - Steps:
//  1. Start without the bind mount, and read capabilities.
//  2. Stop, `mount --bind /run/nx-boot-id /proc/sys/kernel/random/boot_id` (registering `umount /proc/sys/kernel/random/boot_id` in `t.Cleanup` at once), start with `startExact`, and read capabilities again.
//
// - Expected:
//   - (1) `rebootAvailable: false`, with the ASM-7 reason, and a POST reboot answers 409 `reboot-unavailable` with that reason;
//   - (2) `bootIdSource: container-mount`, `rebootAvailable: true`, and `cat /proc/sys/kernel/random/boot_id` inside the container equals `/run/nx-boot-id`;
//   - after cleanup, `/proc/self/mountinfo` has no mount on `/proc/sys/kernel/random/boot_id`.
func TestTCSV142_T1TheRebootIsUnavailableWithoutA(t *testing.T) {
	h := svInstall(t, svOpts{reboot: true})
	// Registered before the mount, so it runs after the mount's cleanup.
	t.Cleanup(func() {
		b, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		if hostMountinfoHas(string(b), svBootIDPath) {
			t.Errorf("after cleanup /proc/self/mountinfo still has a mount on %s", svBootIDPath)
		} else {
			t.Logf("after cleanup /proc/self/mountinfo has no mount on %s", svBootIDPath)
		}
	})
	if b, err := os.ReadFile("/proc/self/mountinfo"); err != nil || hostMountinfoHas(string(b), svBootIDPath) {
		t.Fatalf("a mount on %s before the test (%v)", svBootIDPath, err)
	}
	svStart(t, h)

	c, p := svCaps(t, h, false)
	var avail bool
	var reason string
	var source *string
	svDetail(t, p, services.DetailRebootAvailable, &avail)
	svDetail(t, p, services.DetailRebootUnavailableReason, &reason)
	svDetail(t, p, services.DetailBootIDSource, &source)
	if avail || source != nil || !strings.Contains(reason, "boot ID is the host kernel's") ||
		!strings.Contains(reason, "nothing is mounted over "+svBootIDPath) {
		t.Fatalf("without the mount: rebootAvailable %t bootIdSource %v reason %q, want false, null and the ASM-7 reason",
			avail, source, reason)
	}
	code, b := h.post(svRebootAt, fmt.Sprintf(`{"confirmHost":%q}`, c.Host))
	rp := svProblem(t, "reboot without the mount", code, b, http.StatusConflict, problem.RebootUnavailable)
	if rp.Extensions["reason"] != reason {
		t.Errorf("reboot-unavailable reason %q, want capabilities' %q", rp.Extensions["reason"], reason)
	}
	t.Logf("without the mount: rebootAvailable false (%s); POST reboot 409 reboot-unavailable with that reason", reason)

	lib(t, "stop-nexusd")
	svMountBootID(t, h)
	svStart(t, h)
	svRequireContainerMount(t, h)
	if got, want := svReadTrim(t, svBootIDPath), svReadTrim(t, svBootIDFile); got != want {
		t.Errorf("cat %s = %q, want %s's %q", svBootIDPath, got, svBootIDFile, want)
	}
	t.Logf("with the mount: %s reads %s = %s", svBootIDPath, h.bootID, svBootIDFile)
}

// svRebootBody is a reboot request's body.
func svRebootBody(confirmHost string, extra string) string {
	b, _ := json.Marshal(confirmHost)
	return `{"confirmHost":` + string(b) + extra + `}`
}

// TC-SV-143: T1 every reboot guard refuses on the real host state. Owner WI 5.3. Requirements: FR-10, FR-16, A-42.
//
// - Setup: TC-SV-142 step 2 state (bind mount up, unmounted in cleanup), with `bootIdSource: container-mount` asserted before any POST. The host name is read from `system.capabilities.get`.
// - Steps:
//  1. `reboot.enabled: false` (file edit and SIGHUP), then POST.
//  2. Re-enable it and POST `confirmHost` = the host name uppercased; then POST `confirmHost` = `h1; reboot`, and `confirmHost` = `$(reboot)` (A-42: shell metacharacters pass the schema and are only compared).
//  3. `systemctl disable nexusd` (or `rc-update del nexusd default`), POST, and restore.
//  4. Start `nx-svc-slow` (Running about 20 s) and POST.
//  5. POST `{force: true, whatIf: true}` while `nx-svc-slow` runs.
//  6. Append `bogus: 1` to `/etc/nexus/nexusd.yaml` without a reload, POST, and restore.
//
// - Expected:
//   - (1) 409 `reboot-unavailable`;
//   - (2) 400 `validation-failed` for each of the three values, with nothing sent and the container still up, which proves WI 5.1's `inertAllowed` entry for `confirmHost` (A-42);
//   - (3) 409 `nexusd-not-enabled-at-boot`, with `state` equal to `systemctl is-enabled nexusd`'s output;
//   - (4) 409 `operations-running` listing the slow op's ID;
//   - (5) Succeeded whatIf with the operations guard passed by `force`;
//   - (6) 422 `config-invalid`;
//   - every refusal leaves no reboot op directory, and the audit log has `refused` with the slug;
//   - the container's PID 1 start time (`/proc/1/stat` field 22) is unchanged throughout.
func TestTCSV143_T1EveryRebootGuardRefusesOnThe(t *testing.T) {
	h := svSetup(t, svOpts{reboot: true, mount: true, atBoot: true})
	svRequireContainerMount(t, h)
	c, _ := svCaps(t, h, false)
	host := c.Host
	pid1 := svPID1(t)
	mark := svAuditMark(t)
	var wantSlugs []string

	refuse := func(what, body string, wantCode int, slug problem.Slug) problem.Error {
		t.Helper()
		dirs := svOpEntries(t)
		code, b := h.post(svRebootAt, body)
		p := svProblem(t, what, code, b, wantCode, slug)
		if d := svOpEntries(t); !slices.Equal(d, dirs) {
			t.Errorf("%s: op dirs %q -> %q, want no new reboot operation", what, dirs, d)
		}
		if got := svPID1(t); got != pid1 {
			t.Fatalf("%s: PID 1 start time %d -> %d: the container restarted", what, pid1, got)
		}
		wantSlugs = append(wantSlugs, string(slug))
		t.Logf("%s: %d %s: %s", what, code, slug, p.Detail)
		return p
	}

	writeConfig(t, svConfig(false))
	svReload(t, h, false)
	refuse("(1) reboot.enabled false", svRebootBody(host, ""), http.StatusConflict, problem.RebootUnavailable)

	writeConfig(t, svConfig(true))
	svReload(t, h, true)
	svRequireContainerMount(t, h)
	for _, v := range []string{strings.ToUpper(host), "h1; reboot", "$(reboot)"} {
		p := refuse(fmt.Sprintf("(2) confirmHost %q", v), svRebootBody(v, ""), http.StatusBadRequest, problem.ValidationFailed)
		if !strings.Contains(p.Detail, "confirmHost") && !strings.Contains(fmt.Sprint(p.Extensions["errors"]), "/confirmHost") {
			t.Errorf("(2) confirmHost %q: the refusal does not name confirmHost: %s %v", v, p.Detail, p.Extensions)
		}
	}
	if v := strings.ToUpper(host); v == host {
		t.Fatalf("the host name %q has no letter to uppercase", host)
	}

	svSetAtBoot(t, h.init, false)
	wantState := ""
	if h.init == platform.Systemd {
		wantState = svWord(t, detach.Systemctl, "is-enabled", "--", nexusdUnit)
	} else {
		for line := range strings.Lines(svStdout(t, "rc-update", "show", "-v")) {
			if l, r, ok := strings.Cut(line, "|"); ok && strings.TrimSpace(l) == "nexusd" {
				wantState = strings.Join(strings.Fields(r), " ")
			}
		}
		if wantState == "" {
			wantState = "no runlevel"
		}
	}
	p := refuse("(3) nexusd not enabled at boot", svRebootBody(host, ""), http.StatusConflict, problem.NexusdNotEnabledAtBoot)
	if p.Extensions["state"] != wantState {
		t.Errorf("(3) state %v, want the tool's %q", p.Extensions["state"], wantState)
	}
	svSetAtBoot(t, h.init, true)

	code, b := h.post(svServicesAt+"/nx-svc-slow/start", "{}")
	slow := svAccepted(t, "start nx-svc-slow", code, b)
	waitUntil(t, 30*time.Second, "the nx-svc-slow start Running", func() (bool, string) {
		op := h.operation(slow.ID)
		return op.Status == apitypes.StatusRunning, string(op.Status)
	})
	p = refuse("(4) nx-svc-slow running", svRebootBody(host, ""), http.StatusConflict, problem.OperationsRunning)
	if ids, _ := p.Extensions["operationIds"].([]any); !slices.Contains(ids, any(slow.ID)) {
		t.Errorf("(4) operationIds %v, want the slow start's %s", p.Extensions["operationIds"], slow.ID)
	}

	code, b = h.post(svRebootAt, svRebootBody(host, `,"force":true,"whatIf":true`))
	wi := h.waitOp(svAccepted(t, "(5) force whatIf", code, b).ID, 2*time.Minute)
	if h.operation(slow.ID).Status != apitypes.StatusRunning {
		t.Errorf("(5) nx-svc-slow's start ended before the whatIf did: the operations guard was not exercised")
	}
	var r svspec.RebootResult
	if wi.Status != apitypes.StatusSucceeded || json.Unmarshal(wi.Result, &r) != nil || !r.WhatIf {
		t.Fatalf("(5) force whatIf: %s %v %s, want Succeeded whatIf", wi.Status, wi.Error, wi.Result)
	}
	gi := slices.IndexFunc(r.Guards, func(g svspec.RebootGuard) bool { return g.Name == services.GuardNoOperations })
	if gi < 0 || !r.Guards[gi].Passed || !strings.Contains(r.Guards[gi].Detail, "force") ||
		!slices.Contains(r.RunningOperations, slow.ID) {
		t.Errorf("(5) guards %+v, running %q: want %s passed by force with %s running", r.Guards, r.RunningOperations,
			services.GuardNoOperations, slow.ID)
	}
	t.Logf("(5) force whatIf %s: Succeeded, %s: %s", wi.ID, services.GuardNoOperations, r.Guards[gi].Detail)

	if op := h.waitOp(slow.ID, 2*time.Minute); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("nx-svc-slow's start: %s %v", op.Status, op.Error)
	}
	f, err := os.OpenFile(nexusdConfig, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("bogus: 1\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	refuse("(6) bogus: 1 in the config", svRebootBody(host, ""), http.StatusUnprocessableEntity, problem.ConfigInvalid)
	writeConfig(t, svConfig(true))

	var gotSlugs []string
	for _, r := range svRefused(t, mark, svspec.Reboot.Op().ID, len(wantSlugs)) {
		gotSlugs = append(gotSlugs, r.Problem)
	}
	if !slices.Equal(gotSlugs, wantSlugs) {
		t.Errorf("audit: refused reboot records %q, want %q", gotSlugs, wantSlugs)
	}
	if got := svPID1(t); got != pid1 {
		t.Errorf("PID 1 start time %d -> %d", pid1, got)
	}
	t.Logf("audit: refused %q; PID 1 start time %d unchanged", gotSlugs, pid1)
}

// TC-SV-144: T1 whatIf reports the guards with the container's boot ID and writes no record. Owner WI 5.3. Requirements: FR-10.
//
// - Setup: The TC-SV-142 step 2 state (bind mount up, unmounted in cleanup), with `bootIdSource: container-mount` asserted first, and all guards passing.
// - Steps:
//  1. POST `{confirmHost: <host>, whatIf: true}`, and wait.
//
// - Expected:
//   - Succeeded with `whatIf: true`, `wouldReboot: true`, five guards passed;
//   - `bootId` equals `/run/nx-boot-id`, and `uptimeSeconds` is within 5 s of `/proc/uptime`'s first field;
//   - `rebootRequired.state` equals the host provider's `GET /v1/providers/host/health` value;
//   - no `reboot-requested` file in the op directory;
//   - PID 1 is unchanged.
func TestTCSV144_T1WhatIfReportsTheGuardsWithThe(t *testing.T) {
	h := svSetup(t, svOpts{reboot: true, mount: true, atBoot: true})
	svRequireContainerMount(t, h)
	hd := &hostDaemon{nexusdInstance: h.nexusdInstance}
	hostWaitReady(t, hd)
	c, _ := svCaps(t, h, false)
	pid1 := svPID1(t)

	code, b := h.post(svRebootAt, svRebootBody(c.Host, `,"whatIf":true`))
	op := h.waitOp(svAccepted(t, "whatIf", code, b).ID, 2*time.Minute)
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatal(err)
	}
	var r svspec.RebootResult
	if op.Status != apitypes.StatusSucceeded || json.Unmarshal(op.Result, &r) != nil {
		t.Fatalf("whatIf: %s %v %s", op.Status, op.Error, op.Result)
	}
	passed := 0
	for _, g := range r.Guards {
		if g.Passed {
			passed++
		}
	}
	if !r.WhatIf || r.WouldReboot == nil || !*r.WouldReboot || len(r.Guards) != 5 || passed != 5 {
		t.Errorf("whatIf %t wouldReboot %v guards %+v: want true, true and five passed", r.WhatIf, r.WouldReboot, r.Guards)
	}
	if want := svReadTrim(t, svBootIDFile); r.BootID != want || r.BootID != h.bootID {
		t.Errorf("bootId %q, want %s's %q", r.BootID, svBootIDFile, want)
	}
	procUp, err := strconv.ParseFloat(strings.Fields(string(up))[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	if r.UptimeSeconds == nil || *r.UptimeSeconds > procUp || procUp-*r.UptimeSeconds > 5 {
		t.Errorf("uptimeSeconds %v, want within 5 s of /proc/uptime's %.2f", r.UptimeSeconds, procUp)
	}
	health := hostHealth(t, hd)
	if r.RebootRequired.State != health.RebootRequired.State {
		t.Errorf("rebootRequired.state %q, want the host provider's %q", r.RebootRequired.State, health.RebootRequired.State)
	}
	if _, err := os.Stat(filepath.Join(svOpsDir, op.ID, detach.RebootFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s/%s/%s: %v, want no such file", svOpsDir, op.ID, detach.RebootFile, err)
	}
	if got := svPID1(t); got != pid1 {
		t.Errorf("PID 1 start time %d -> %d", pid1, got)
	}
	t.Logf("whatIf %s: Succeeded, wouldReboot true, %d guards passed, bootId %s, uptime %.2f (/proc/uptime %.2f), "+
		"rebootRequired %s = host health's; no %s; PID 1 unchanged", op.ID, passed, r.BootID, *r.UptimeSeconds, procUp,
		r.RebootRequired.State, detach.RebootFile)
}

// svRebootFailSafe kills operation id's helper before it can record or
// trigger the reboot: on both init systems every process whose command line
// is `services-helper run --dir .../<id>`, then on systemd the operation's
// unit, and on OpenRC every process carrying the operation's directory
// (supervisor and helper), checking none is left. With id "" it kills every
// services helper.
func svRebootFailSafe(t *testing.T, id string) {
	t.Helper()
	pattern := services.HelperCommand + " run --dir .*/" + id
	if id == "" {
		pattern = services.HelperCommand + " run --dir "
	}
	out, err := sh(30*time.Second, "pkill", "-KILL", "-f", pattern)
	t.Logf("fail-safe: pkill -KILL -f %q: %v %s", pattern, err, out)
	if id == "" {
		return
	}
	if detectedInit() == platform.Systemd {
		out, err := sh(30*time.Second, detach.Systemctl, "kill", "-s", "SIGKILL", detach.Unit(id))
		t.Logf("fail-safe: systemctl kill -s SIGKILL %s: %v %s", detach.Unit(id), err, out)
		return
	}
	dir := filepath.Join(svOpsDir, id)
	out, err = sh(30*time.Second, "pkill", "-KILL", "-f", dir)
	t.Logf("fail-safe: pkill -KILL -f %s: %v %s", dir, err, out)
	time.Sleep(200 * time.Millisecond)
	if left, err := sh(30*time.Second, "pgrep", "-f", dir); err == nil {
		t.Errorf("fail-safe: pgrep -f %s still finds %s", dir, strings.TrimSpace(left))
	}
}

// TC-SV-145: T1 a delay-window cancel ends Canceled with no record and no trigger. Owner WI 5.3. Requirements: FR-10, FR-11, CON-5.
//
// - Setup: The TC-SV-142 step 2 state (bind mount up, unmounted in cleanup), with `bootIdSource: container-mount` asserted first. Every guard passes, so a cancel that does not land would let the helper run the real `systemctl reboot` (or BusyBox `reboot`) inside the privileged container after the delay. Fail-safe (review Finding 3): registered in `t.Cleanup` before the POST, and also run by step 4 on timeout. If the operation is not terminal 10 s after the cancel, the test kills the helper before it can trigger and then fails:
//
//   - systemd: `systemctl kill -s SIGKILL nexus-op-<id>.service`;
//
//   - OpenRC: `pkill -KILL -f <op directory>` (the supervisor and the helper both carry the op directory in their argv), then checks that `pgrep -f <op directory>` finds nothing.
//
//     The 300 s delay leaves about 290 s between the fail-safe and the earliest possible trigger.
//
// - Steps:
//  1. POST `{confirmHost: <host>, delaySeconds: 300}`, and wait for Running.
//  2. On systemd, check that `systemctl show -p ActiveState nexus-op-<id>.service` is `active`.
//  3. `POST /v1/operations/<id>/cancel`.
//  4. Wait for terminal, at most 10 s. On timeout, run the fail-safe and `t.Fatalf` naming the op ID and its last state.
//
// - Expected:
//   - Canceled within 10 s (the fail-safe never ran);
//   - no `reboot-requested` ever existed (checked every 100 ms during the run);
//   - systemd: `systemctl list-jobs` never showed `reboot.target`, and the journal has no `reboot` request from the op unit;
//   - OpenRC: no `openrc shutdown` in `/var/log/messages` or the op log;
//   - PID 1 is unchanged and `nexusd` keeps answering;
//   - the audit log has `requested` then the cancel records.
func TestTCSV145_T1ADelayWindowCancelEndsCanceled(t *testing.T) {
	h := svSetup(t, svOpts{reboot: true, mount: true, atBoot: true})
	svRequireContainerMount(t, h)
	systemd := h.init == platform.Systemd
	c, _ := svCaps(t, h, false)
	pid1 := svPID1(t)
	mark := svAuditMark(t)
	msgMark := 0
	if !systemd {
		msgMark = messagesLines(t)
	}
	since := time.Now().Add(-time.Second)

	// The fail-safe, registered before the POST: any operation not
	// terminal when the test ends has its helper killed.
	var mu sync.Mutex
	id := ""
	t.Cleanup(func() {
		mu.Lock()
		opID := id
		mu.Unlock()
		if opID != "" {
			if code, b := h.get("/v1/operations/" + opID); code == http.StatusOK {
				var op apitypes.Operation
				if json.Unmarshal(b, &op) == nil && terminal(op.Status) {
					return
				}
			}
		}
		t.Errorf("cleanup: reboot operation %q not known to be terminal: running the fail-safe", opID)
		svRebootFailSafe(t, opID)
	})

	// The watcher: every 100 ms, the record file and systemd's jobs.
	stop := make(chan struct{})
	watched := make(chan []string, 1)
	go func() {
		var seen []string
		polls := 0
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			mu.Lock()
			opID := id
			mu.Unlock()
			if opID != "" {
				if _, err := os.Stat(filepath.Join(svOpsDir, opID, detach.RebootFile)); err == nil {
					seen = append(seen, fmt.Sprintf("%s existed at %s", detach.RebootFile, time.Now().Format(time.RFC3339Nano)))
				}
			}
			if systemd {
				if out, err := exec.Command(detach.Systemctl, "list-jobs", "--plain", "--no-legend", "--no-pager").Output(); err == nil &&
					strings.Contains(string(out), "reboot.target") {
					seen = append(seen, "list-jobs: "+strings.TrimSpace(string(out)))
				}
			}
			polls++
			select {
			case <-stop:
				watched <- append(seen, fmt.Sprintf("polls=%d", polls))
				return
			case <-tick.C:
			}
		}
	}()
	stopWatch := func() []string {
		close(stop)
		return <-watched
	}

	code, b := h.post(svRebootAt, svRebootBody(c.Host, fmt.Sprintf(`,"delaySeconds":%d`, svRebootDelay)))
	op := svAccepted(t, "reboot delaySeconds 300", code, b)
	mu.Lock()
	id = op.ID
	mu.Unlock()
	waitUntil(t, 30*time.Second, "the reboot operation Running", func() (bool, string) {
		o := h.operation(op.ID)
		return o.Status == apitypes.StatusRunning, string(o.Status)
	})
	if systemd {
		waitUntil(t, 10*time.Second, detach.Unit(op.ID)+" active", func() (bool, string) {
			st := svWord(t, detach.Systemctl, "show", "-p", "ActiveState", "--value", detach.Unit(op.ID))
			return st == "active", st
		})
	}
	// The helper is in its delay: the log says so before the cancel.
	waitUntil(t, 10*time.Second, "the helper in its delay", func() (bool, string) {
		l := h.opLogs(op.ID)
		return strings.Contains(l, "rebooting with "), l
	})

	cancelAt := time.Now()
	if code, b := h.post("/v1/operations/"+op.ID+"/cancel", "{}"); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("cancel %s: %d %s", op.ID, code, b)
	}
	var last apitypes.Operation
	for {
		last = h.operation(op.ID)
		if terminal(last.Status) {
			break
		}
		if time.Since(cancelAt) > svCancelBound {
			svRebootFailSafe(t, op.ID)
			t.Fatalf("reboot operation %s still %s %v after the cancel: the fail-safe killed its helper", op.ID,
				last.Status, time.Since(cancelAt).Round(time.Millisecond))
		}
		time.Sleep(100 * time.Millisecond)
	}
	took := time.Since(cancelAt)
	seen := stopWatch()
	if last.Status != apitypes.StatusCanceled {
		t.Errorf("reboot operation %s ended %s %v after the cancel, want Canceled", op.ID, last.Status, last.Error)
	}
	if len(seen) > 1 {
		t.Errorf("during the run: %q", seen)
	}
	if _, err := os.Stat(filepath.Join(svOpsDir, op.ID, detach.RebootFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s after the cancel: %v", detach.RebootFile, err)
	}
	opLog := h.opLogs(op.ID)
	if !strings.Contains(opLog, "nothing was recorded or sent") || strings.Contains(opLog, "recorded the reboot request") {
		t.Errorf("the op log does not say the reboot stopped in its delay with nothing recorded or sent:\n%s", opLog)
	}
	if systemd {
		j := mustSh(t, "journalctl", "--no-pager", "-o", "cat", "--since", "@"+strconv.FormatInt(since.Unix(), 10))
		for line := range strings.Lines(j) {
			if strings.Contains(line, "reboot.target") || strings.Contains(line, "System is rebooting") {
				t.Errorf("journal: %s", strings.TrimSpace(line))
			}
		}
		for line := range strings.Lines(mustSh(t, "journalctl", "--no-pager", "-o", "cat", "-u", detach.Unit(op.ID))) {
			if strings.Contains(line, "systemctl reboot") {
				t.Errorf("journal of %s: %s", detach.Unit(op.ID), strings.TrimSpace(line))
			}
		}
	} else {
		for _, s := range []string{messagesSince(t, msgMark), opLog} {
			if strings.Contains(s, "openrc shutdown") {
				t.Errorf("an openrc shutdown ran:\n%s", s)
			}
		}
	}
	if got := svPID1(t); got != pid1 {
		t.Errorf("PID 1 start time %d -> %d", pid1, got)
	}
	if code, b := h.get("/v1/capabilities"); code != http.StatusOK {
		t.Errorf("nexusd stopped answering: %d %s", code, b)
	}

	recs := svAudit(t, mark)
	req := slices.IndexFunc(recs, func(r svAuditRec) bool {
		return r.Action == svspec.Reboot.Op().ID && r.Outcome == "requested"
	})
	canceled := -1
	var trail []string
	for i, r := range recs {
		trail = append(trail, fmt.Sprintf("%s %s %s op=%s", r.Action, r.Outcome, r.Problem, r.OperationID))
		if i > req && req >= 0 && (r.OperationID == op.ID || strings.Contains(r.Target, op.ID)) &&
			(r.Outcome == string(apitypes.StatusCanceled) || strings.Contains(r.Action, "cancel")) && canceled < 0 {
			canceled = i
		}
	}
	if req < 0 || canceled < 0 {
		t.Errorf("audit: want the reboot's requested record then its cancel records: %q", trail)
	}
	t.Logf("reboot %s: Canceled %v after the cancel; watcher %q; no %s; PID 1 unchanged; audit %q", op.ID,
		took.Round(time.Millisecond), seen, detach.RebootFile, trail)
}

// TC-SV-146: T1 reads under the exact unit cause no sandbox denial. Owner WI 5.3. Requirements: CON-1, NFR-4, ASM-8, A-1, A-2.
//
// - Setup: `startUnderSandbox`/`startExact` with `requireExactUnit`, and the start time `since`.
// - Steps:
//  1. List, get `nexusd`, logs `nexusd tail=200` and capabilities.
//  2. Run `journalFaults(systemLog(t))` over the window, restricted to `sandboxComms` plus `comm="journalctl"`.
//
// - Expected:
//   - all four answer 200, and logs returns ≥ 1 frame (the journal was readable under `CAP_DAC_READ_SEARCH`);
//   - no `SECCOMP`, `audit: type=1326`, `Operation not permitted` or `Permission denied` line from `nexusd`, `systemctl` or `journalctl`;
//   - `deploy/nexusd.service` installed equals the repository file (`requireExactUnit`).
func TestTCSV146_T1ReadsUnderTheExactUnitCause(t *testing.T) {
	requireInit(t, platform.Systemd)
	h := svInstall(t, svOpts{})
	since := time.Now().Add(-time.Second)
	kernelBefore := kernelLines(t)
	svStart(t, h)
	requireExactUnit(t)

	for _, path := range []string{svServicesAt, svServicesAt + "/nexusd", "/v1/capabilities"} {
		if code, b := h.get(path); code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, code, b)
		}
	}
	frames, ok := h.frames(svServicesAt+"/nexusd/logs?tail=200", 30*time.Second, nil)
	if !ok || len(frames) < 1 {
		t.Errorf("logs nexusd tail=200: %d frames, ended %t; want at least one", len(frames), ok)
	}
	t.Logf("list, get nexusd, capabilities: 200; logs nexusd tail=200: 200 with %d frames", len(frames))
	time.Sleep(time.Second)

	comms := append(slices.Clone(sandboxComms), `comm="journalctl"`)
	ours := func(line string) bool {
		return slices.ContainsFunc(comms, func(c string) bool { return strings.Contains(line, c) }) ||
			strings.Contains(line, " nexusd[") || strings.Contains(line, " systemctl[") || strings.Contains(line, " journalctl[")
	}
	var window strings.Builder
	for line := range strings.Lines(mustSh(t, "journalctl", "--no-pager", "--since", "@"+strconv.FormatInt(since.Unix(), 10))) {
		if ours(line) {
			window.WriteString(line)
		}
	}
	for line := range strings.Lines(mustSh(t, "journalctl", "--no-pager", "-o", "cat", "-u", nexusdUnit, "--since",
		"@"+strconv.FormatInt(since.Unix(), 10))) {
		window.WriteString(line)
	}
	kernelAfter := kernelLines(t)
	if len(kernelAfter) > len(kernelBefore) {
		for _, line := range kernelAfter[len(kernelBefore):] {
			if ours(line) {
				window.WriteString(line + "\n")
			}
		}
	}
	faults := journalFaults(window.String())
	for _, f := range faults {
		t.Error(f)
	}
	t.Logf("sandbox: %d journal and kernel lines of nexusd, systemctl and journalctl in the window, no denial: %t",
		strings.Count(window.String(), "\n"), len(faults) == 0)
	requireExactUnit(t)
}

// TC-SV-147: T1 the protected set and selfService are resolved from the running unit. Owner WI 5.3. Requirements: FR-3, FR-9, A-8, A-11.
//
// - Setup: Defaults.
// - Steps:
//  1. Read capabilities.
//  2. Compare with `cat /proc/$(MainPID)/cgroup` and `systemctl show -p Requires,BindsTo,Requisite nexusd`, or with `/run/openrc/options/nexusd/child_pid` and `rc-service --resolve`/the deptree.
//
// - Expected:
//   - `selfService` is `nexusd`;
//   - `protected` contains every `.service` dependency the tool lists (systemd), or on OpenRC every service nexusd's deptree entry resolves to. The T1 Docker OpenRC image has no `ineed` for nexusd and no `net` entry, so on T1 this is the static `ProtectedOpenRC` list only; this case does not claim a T1 `net` resolution (real `net` resolution is TC-SV-L3 on Public-Nginx);
//   - `initSystem` equals the host provider's `initSystem`.
func TestTCSV147_T1TheProtectedSetAndSelfServiceAre(t *testing.T) {
	h := svSetup(t, svOpts{})
	c, p := svCaps(t, h, false)
	var self *string
	var protected []string
	var initSys string
	svDetail(t, p, services.DetailSelfService, &self)
	svDetail(t, p, services.DetailProtected, &protected)
	svDetail(t, p, services.DetailInitSystem, &initSys)
	if self == nil || *self != "nexusd" {
		t.Errorf("selfService %v, want nexusd", self)
	}
	pid := svMainPID(t, h)
	if h.init == platform.Systemd {
		cg := svReadTrim(t, fmt.Sprintf("/proc/%d/cgroup", pid))
		var unit string
		for line := range strings.Lines(cg) {
			if path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
				for _, el := range strings.Split(path, "/") {
					if strings.HasSuffix(el, ".service") {
						unit = el
					}
				}
			}
		}
		if unit != nexusdUnit {
			t.Errorf("/proc/%d/cgroup %q: last .service element %q, want %s", pid, cg, unit, nexusdUnit)
		}
		s := svShow(t, "nexusd", "Requires", "BindsTo", "Requisite")
		var deps []string
		for _, k := range []string{"Requires", "BindsTo", "Requisite"} {
			for _, u := range strings.Fields(s[k]) {
				if n, ok := strings.CutSuffix(u, ".service"); ok {
					deps = append(deps, n)
					if !slices.Contains(protected, n) {
						t.Errorf("%s=%s: %s is not in protected %q", k, s[k], n, protected)
					}
				}
			}
		}
		t.Logf("cgroup %q -> %s; Requires/BindsTo/Requisite .service %q all in protected %q", cg, unit, deps, protected)
	} else {
		child := svChildPID("nexusd")
		if child != pid {
			t.Errorf("/run/openrc/options/nexusd/child_pid %d, want the answering daemon %d", child, pid)
		}
		resolved := svWord(t, "rc-service", "--resolve", "nexusd")
		if resolved != "/etc/init.d/nexusd" {
			t.Errorf("rc-service --resolve nexusd: %q", resolved)
		}
		d := svDeptree(t)
		needs := svDeptreeNeeds(t, d)
		for _, n := range slices.Concat(svspec.ProtectedOpenRC, needs) {
			if !slices.Contains(protected, n) {
				t.Errorf("protected %q lacks %s (static list, or nexusd's deptree resolution %q)", protected, n, needs)
			}
		}
		t.Logf("child_pid %d = nexusd's PID; rc-service --resolve nexusd = %s; deptree: nexusd ineed %q resolves to %q; "+
			"protected %q ⊇ ProtectedOpenRC and the resolution", child, resolved, d["nexusd"]["ineed"], needs, protected)
	}
	hd := &hostDaemon{nexusdInstance: h.nexusdInstance}
	hostWaitReady(t, hd)
	id := hostIdentity(t, hd)
	if initSys != id.InitSystem || initSys != c.InitSystem || initSys != string(h.init) {
		t.Errorf("initSystem %q, host provider %q, capabilities %q, detected %s: want all equal", initSys,
			id.InitSystem, c.InitSystem, h.init)
	}
	t.Logf("selfService nexusd; initSystem %s = the host provider's", initSys)
}
