//go:build integration

// Package integration is T1: it runs only inside the harness container that
// `make test-harness` (real systemd) or `make test-harness-alpine` (real
// OpenRC) starts (test/harness/run.sh), as root. This file holds the shared
// helpers, the harness self-check (TC-71), production wiring (TC-75) and the
// shipped unit (TC-227); the survival proof is in survival_test.go and, on
// OpenRC, openrc_survival_test.go. A test that needs one init system says
// so with requireInit (platform_test.go).
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"debug/elf"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
)

const (
	libSh        = "/harness/lib.sh"
	shippedUnit  = "/harness/deploy/nexusd.service"
	nexusdBin    = "/usr/local/bin/nexusd"
	opsdBin      = "/usr/local/bin/opsd"
	markerRoot   = "/run/nexus-harness" // opsd's MarkerRoot
	nexusdUnit   = "nexusd.service"
	nexusdConfig = "/etc/nexus/nexusd.yaml"
	nexusdState  = "/var/lib/nexus"
)

func TestMain(m *testing.M) {
	// As the nexus CLI, so a test can run it as a real process in a shell
	// pipeline (TC-170).
	if os.Getenv(asCLIEnv) == "1" {
		os.Exit(cli.Run(os.Args[1:], cli.OSEnv()))
	}
	if os.Getenv("NEXUS_HARNESS") != "1" {
		fmt.Fprintln(os.Stderr, "test/integration runs only inside the T1 harness container: make test-harness")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// sh runs a command with a bound and returns its combined output.
func sh(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func mustSh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := sh(2*time.Minute, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

// lib runs a test/harness/lib.sh command.
func lib(t *testing.T, args ...string) string {
	t.Helper()
	return mustSh(t, "bash", append([]string{libSh}, args...)...)
}

// showUnit is systemctl show for the named properties. On OpenRC,
// nexusd.service is the nexusd service (openrcService) and an operation
// unit is its process-backend supervisor (processUnit), each with only the
// properties that have an equivalent; any other fails the test, which must
// then be gated with requireInit(t, platform.Systemd).
func showUnit(t *testing.T, unit string, props ...string) map[string]string {
	t.Helper()
	if detectedInit() == platform.OpenRC {
		if unit == nexusdUnit {
			return openrcService(t, props...)
		}
		return processUnit(t, unit, props...)
	}
	args := []string{"show", unit}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out := mustSh(t, detach.Systemctl, args...)
	m := map[string]string{}
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			m[k] = v
		}
	}
	return m
}

// openrcService answers showUnit for the nexusd OpenRC service:
// ActiveState from `rc-service nexusd status` (started is active, crashed
// failed, stopped inactive) and MainPID, the answering daemon (openrcMainPID).
func openrcService(t *testing.T, props ...string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, p := range props {
		switch p {
		case "ActiveState":
			m[p] = map[string]string{"started": "active", "crashed": "failed", "starting": "activating",
				"stopping": "deactivating"}[rcStatus(t)]
			if m[p] == "" {
				m[p] = "inactive"
			}
		case "MainPID":
			m[p] = strconv.Itoa(openrcMainPID(t))
		default:
			t.Fatalf("systemctl show -p %s has no OpenRC equivalent; gate the test with requireInit(t, platform.Systemd)", p)
		}
	}
	return m
}

// opDirOf finds operation id's directory in the ops directory of any
// daemon the tests run: the default state directory or one of a test's own.
func opDirOf(id string) string {
	for _, pattern := range []string{filepath.Join(nexusdState, "ops", id), filepath.Join(nexusdState, "*", "ops", id)} {
		if m, _ := filepath.Glob(pattern); len(m) == 1 {
			return m[0]
		}
	}
	return ""
}

// processUnit answers showUnit for operation unit nexus-op-<id>.service on
// OpenRC from the process backend: ActiveState and LoadState from its
// UnitState (a live supervisor identity is active and loaded, anything
// else inactive and not-found, as a collected unit), MainPID from
// supervisor.json while it is live.
func processUnit(t *testing.T, unit string, props ...string) map[string]string {
	t.Helper()
	id, ok := strings.CutPrefix(strings.TrimSuffix(unit, ".service"), detach.UnitPrefix)
	if !ok || !strings.HasSuffix(unit, ".service") {
		t.Fatalf("showUnit %s on OpenRC: not an operation unit", unit)
	}
	st := detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}
	dir := opDirOf(id)
	if dir != "" {
		var err error
		opts := detach.ProcessBackend(detach.ProcessConfig{OpsDir: filepath.Dir(dir)})
		if st, err = opts.UnitState(context.Background(), unit); err != nil {
			t.Fatalf("UnitState %s: %v", unit, err)
		}
	}
	m := map[string]string{}
	for _, p := range props {
		switch p {
		case "ActiveState":
			m[p] = st.ActiveState
		case "LoadState":
			m[p] = st.LoadState
		case "MainPID":
			m[p] = "0"
			if st.ActiveState == "active" {
				if ident, err := detach.ReadIdentity(dir); err == nil {
					m[p] = strconv.Itoa(ident.PID)
				}
			}
		default:
			t.Fatalf("systemctl show %s -p %s has no OpenRC equivalent; gate the test with requireInit(t, platform.Systemd)", unit, p)
		}
	}
	return m
}

// opCgroupPIDs lists every process in operation id's process-backend
// cgroup, /sys/fs/cgroup/nexus-ops/<id> and its work group.
func opCgroupPIDs(id string) []int {
	var pids []int
	_ = filepath.WalkDir(filepath.Join("/sys/fs/cgroup/nexus-ops", id), func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() != "cgroup.procs" {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	return pids
}

// inOpCgroup reports whether a /proc/<pid>/cgroup file places the process
// in operation id's unit (systemd) or nexus-ops group, its work group
// included (OpenRC).
func inOpCgroup(cgroupFile, id string) bool {
	if detectedInit() != platform.OpenRC {
		return strings.Contains(cgroupFile, "/"+detach.Unit(id))
	}
	for line := range strings.Lines(cgroupFile) {
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if ok && (p == "/nexus-ops/"+id || strings.HasPrefix(p, "/nexus-ops/"+id+"/")) {
			return true
		}
	}
	return false
}

// opSupervisor checks how operation id, whose directory is opDir, was
// launched and returns its supervisor's PID: on systemd, its unit's
// KillMode is killMode and its ExecStart is `nexusd op-supervise --dir
// <opDir>`; on OpenRC, supervisor.json names a live process whose argv is
// `nexusd op-supervise --dir <opDir> --process`, in its nexus-ops group, and
// a control-group operation's work runs in that group's work subgroup.
func opSupervisor(t *testing.T, id, opDir, killMode string, workPID int) int {
	t.Helper()
	unit := detach.Unit(id)
	if detectedInit() != platform.OpenRC {
		u := showUnit(t, unit, "KillMode", "ExecStart", "MainPID")
		if u["KillMode"] != killMode || !strings.Contains(u["ExecStart"], "argv[]="+nexusdBin+" op-supervise --dir "+opDir+" ;") {
			t.Fatalf("unit %s %v", unit, u)
		}
		pid, err := strconv.Atoi(u["MainPID"])
		if err != nil || pid == 0 {
			t.Fatalf("unit %s MainPID %q", unit, u["MainPID"])
		}
		return pid
	}
	ident, err := detach.ReadIdentity(opDir)
	if err != nil || !pidAlive(ident.PID) {
		t.Fatalf("supervisor of %s: %+v %v", id, ident, err)
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ident.PID))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if want := []string{nexusdBin, "op-supervise", "--dir", opDir, "--process"}; !slices.Equal(argv, want) {
		t.Fatalf("supervisor %d argv %q, want %q", ident.PID, argv, want)
	}
	if cg := pidCgroup(t, ident.PID); cg != "0::/nexus-ops/"+id {
		t.Fatalf("supervisor %d in cgroup %q, want 0::/nexus-ops/%s", ident.PID, cg, id)
	}
	if workPID > 0 {
		cg, want := pidCgroup(t, workPID), "0::/nexus-ops/"+id
		if killMode == string(detach.KillControlGroup) {
			want += "/work"
		}
		if cg != want {
			t.Fatalf("pid %d of %s (%s) in cgroup %q, want %q", workPID, id, killMode, cg, want)
		}
	}
	t.Logf("%s: supervisor %d %q in %s (process backend)", id, ident.PID, argv, pidCgroup(t, ident.PID))
	return ident.PID
}

// killOpUnit sends SIGKILL to every process of operation unit unit, as
// `systemctl kill --signal=SIGKILL` does: on OpenRC, its supervisor and
// every process in its cgroup.
func killOpUnit(t *testing.T, unit string) {
	t.Helper()
	if detectedInit() != platform.OpenRC {
		_, _ = sh(time.Minute, detach.Systemctl, "kill", "--signal=SIGKILL", unit)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
	pids := opCgroupPIDs(id)
	if dir := opDirOf(id); dir != "" {
		if ident, err := detach.ReadIdentity(dir); err == nil {
			pids = append(pids, ident.PID)
		}
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// unitProperties is what the init system holds about operation unit unit:
// `systemctl show`, or on OpenRC its supervisor's command line,
// environment and supervisor.json.
func unitProperties(t *testing.T, unit string) []byte {
	t.Helper()
	if detectedInit() != platform.OpenRC {
		return []byte(mustSh(t, detach.Systemctl, "show", unit))
	}
	id := strings.TrimSuffix(strings.TrimPrefix(unit, detach.UnitPrefix), ".service")
	dir := opDirOf(id)
	ident, err := detach.ReadIdentity(dir)
	if err != nil {
		t.Fatalf("supervisor of %s: %v", unit, err)
	}
	var out []byte
	for _, f := range []string{fmt.Sprintf("/proc/%d/cmdline", ident.PID), fmt.Sprintf("/proc/%d/environ", ident.PID),
		filepath.Join(dir, detach.IdentityFile)} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

// systemLog is the host's log of the last ten minutes or more: the
// journal, or on OpenRC /var/log/messages, where the nexusd script's
// logger lines land.
func systemLog(t *testing.T) []byte {
	t.Helper()
	if detectedInit() != platform.OpenRC {
		return []byte(mustSh(t, "journalctl", "--no-pager", "-o", "cat", "--since", "-10min"))
	}
	b, err := os.ReadFile("/var/log/messages")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// startNexusdNoBlock starts nexusd without waiting for it: `systemctl
// start --no-block`, or on OpenRC rc-service in the background (a crashed
// service zapped first).
func startNexusdNoBlock(t *testing.T) {
	t.Helper()
	if detectedInit() != platform.OpenRC {
		mustSh(t, detach.Systemctl, "start", "--no-block", nexusdUnit)
		return
	}
	cmd := exec.Command("sh", "-c", "rc-service nexusd status >/dev/null 2>&1 || rc-service nexusd zap >/dev/null 2>&1; rc-service nexusd start")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Logf("rc-service nexusd start (in the background): %v", err)
		}
	})
}

// rcStatus is the nexusd service's state word from rc-service.
func rcStatus(t *testing.T) string {
	t.Helper()
	out, _ := sh(30*time.Second, "rc-service", "nexusd", "status")
	for _, w := range []string{"crashed", "started", "starting", "stopping", "inactive", "stopped"} {
		if strings.Contains(out, "status: "+w) {
			return w
		}
	}
	if strings.Contains(out, "does not exist") {
		return "stopped"
	}
	t.Fatalf("rc-service nexusd status: %q", out)
	return ""
}

// openrcPidfile is the shipped script's pidfile="/run/${RC_SVCNAME}.pid".
const openrcPidfile = "/run/nexusd.pid"

// openrcMainPID is the PID of the answering daemon, or 0 when the service
// is not started: the pidfile's process, or, when that is the
// supervise-daemon of the shipped script, its child (L-11).
func openrcMainPID(t *testing.T) int {
	t.Helper()
	if st := rcStatus(t); st != "started" {
		return 0
	}
	b, err := os.ReadFile(openrcPidfile)
	if err != nil {
		t.Fatalf("nexusd is started but: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("%s: %q", openrcPidfile, b)
	}
	if !isSuperviseDaemon(pid) {
		return pid
	}
	kids := childrenOf(pid)
	if len(kids) != 1 {
		return 0 // Between a crash and the respawn
	}
	return kids[0]
}

// isSuperviseDaemon reports whether pid runs OpenRC's supervise-daemon. It
// reads the executable, not comm: the kernel truncates comm to 15 bytes
// ("supervise-daemo").
func isSuperviseDaemon(pid int) bool {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return err == nil && filepath.Base(exe) == "supervise-daemon"
}

// procStatusField is a field of /proc/<pid>/status, such as PPid or Umask.
func procStatusField(pid int, field string) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// childrenOf lists the live processes whose parent is pid.
func childrenOf(pid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var kids []int
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p == pid {
			continue
		}
		if procStatusField(p, "PPid") == strconv.Itoa(pid) && pidAlive(p) {
			kids = append(kids, p)
		}
	}
	return kids
}

func activeState(t *testing.T, unit string) string {
	t.Helper()
	return showUnit(t, unit, "ActiveState")["ActiveState"]
}

// waitUntil polls cond every 100 ms until it holds or timeout passes.
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s (last: %s)", timeout, what, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pidAlive reports whether pid exists and is not a zombie.
func pidAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the parenthesized command name.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}

func pidCgroup(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("cgroup of %d: %v", pid, err)
	}
	return strings.TrimSpace(string(b))
}

// waitPIDFile waits for a pid file a child writes and returns the pid.
func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitUntil(t, 30*time.Second, path, func() (bool, string) {
		b, err := os.ReadFile(path)
		if err != nil {
			return false, err.Error()
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0, string(b)
	})
	return pid
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// unitInstalled installs the shipped unit and a loopback tls-auto config
// through lib.sh once per run, so every test that runs a daemon runs it
// under deploy/nexusd.service.
var unitInstalled sync.Once

func installShippedUnit(t *testing.T) {
	t.Helper()
	unitInstalled.Do(func() {
		cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
		if err := os.WriteFile(cfg, []byte("listen: [\"127.0.0.1:7443\"]\ntls: {mode: auto}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		lib(t, "install-nexusd", cfg)
	})
}

// opsd is one opsd daemon run as nexusd.service, under the shipped
// hardening, through a drop-in that replaces ExecStart.
type opsd struct {
	t        *testing.T
	name     string
	stateDir string
	listen   string
	token    string
	client   *http.Client
}

var nextPort = 7460

// opsdConfig controls the drop-in. onlyExecStart leaves Restart=on-failure
// in force (TC-227); otherwise the drop-in also sets Restart=no, so a
// survival test decides when the killed daemon comes back.
type opsdConfig struct {
	onlyExecStart bool
	flags         []string
}

func newOpsd(t *testing.T, name string, c opsdConfig) *opsd {
	t.Helper()
	installShippedUnit(t)
	nextPort++
	o := &opsd{
		t: t, name: name,
		stateDir: filepath.Join(nexusdState, "opsd-"+name),
		listen:   fmt.Sprintf("127.0.0.1:%d", nextPort),
		client:   &http.Client{Timeout: 15 * time.Second},
	}
	auditDir := filepath.Join("/var/log/nexus", "opsd-"+name)
	for _, d := range []string{o.stateDir, auditDir} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tok, _, err := auth.Create(o.stateDir, auth.CreateRequest{Name: "harness", Scopes: []string{"scripts:run", "scripts:read"}})
	if err != nil {
		t.Fatal(err)
	}
	o.token = tok

	tmp := t.TempDir()
	cfgSrc := filepath.Join(tmp, "opsd.yaml")
	cfg := fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts: {enabled: true}\n", o.listen)
	if err := os.WriteFile(cfgSrc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := "/etc/nexus/opsd-" + name + ".yaml"
	lib(t, "install-config", cfgSrc, cfgPath)

	execStart := strings.Join(append([]string{opsdBin, "serve", "--config", cfgPath, "--state-dir", o.stateDir,
		"--audit-dir", auditDir}, c.flags...), " ")
	dropin := "[Service]\nExecStart=\nExecStart=" + execStart + "\n"
	if !c.onlyExecStart {
		dropin += "Restart=no\n"
	}
	dropinSrc := filepath.Join(tmp, "opsd.conf")
	if err := os.WriteFile(dropinSrc, []byte(dropin), 0o644); err != nil {
		t.Fatal(err)
	}
	lib(t, "stop-nexusd")
	lib(t, "dropin", "opsd", dropinSrc)
	t.Cleanup(func() {
		lib(t, "stop-nexusd")
		lib(t, "remove-dropins")
	})
	return o
}

// start starts the unit; systemctl returns once READY=1 arrived.
func (o *opsd) start() {
	o.t.Helper()
	lib(o.t, "start-nexusd")
	if st := activeState(o.t, nexusdUnit); st != "active" {
		o.t.Fatalf("opsd unit %s after start", st)
	}
}

func (o *opsd) mainPID() int {
	o.t.Helper()
	pid, err := strconv.Atoi(showUnit(o.t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil {
		o.t.Fatal(err)
	}
	return pid
}

// kill9 sends SIGKILL to the daemon's main process only, as a crash would,
// and waits for systemd to see the unit stop.
func (o *opsd) kill9() {
	o.t.Helper()
	pid := o.mainPID()
	if pid == 0 {
		o.t.Fatal("opsd has no main process")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		o.t.Fatalf("kill -9 %d: %v", pid, err)
	}
	waitUntil(o.t, 15*time.Second, "opsd unit to stop after kill -9", func() (bool, string) {
		st := activeState(o.t, nexusdUnit)
		return st == "failed" || st == "inactive", st
	})
	o.t.Logf("kill -9 opsd pid %d: nexusd.service %s", pid, activeState(o.t, nexusdUnit))
}

func (o *opsd) do(method, path, body string) (int, []byte) {
	o.t.Helper()
	req, err := http.NewRequest(method, "http://"+o.listen+path, strings.NewReader(body))
	if err != nil {
		o.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.Do(req)
	if err != nil {
		o.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		o.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// job is the input of scripts.harnessjobs.run, without the name.
type job struct {
	Behavior string `json:"behavior,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	KillMode string `json:"killMode,omitempty"`
	Lane     string `json:"lane,omitempty"`
	Policy   string `json:"policy,omitempty"`
}

// post starts job name and returns the status and body.
func (o *opsd) post(name string, j job) (int, []byte) {
	o.t.Helper()
	if err := os.RemoveAll(markerDir(name)); err != nil {
		o.t.Fatal(err)
	}
	b, err := json.Marshal(j)
	if err != nil {
		o.t.Fatal(err)
	}
	return o.do(http.MethodPost, "/v1/providers/scripts/harnessjobs/"+name+"/run", string(b))
}

// startJob starts job name and requires 202.
func (o *opsd) startJob(name string, j job) apitypes.Operation {
	o.t.Helper()
	code, b := o.post(name, j)
	if code != http.StatusAccepted {
		o.t.Fatalf("start %s: %d %s", name, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		o.t.Fatal(err)
	}
	o.t.Logf("started %s: operation %s, unit %s", name, op.ID, detach.Unit(op.ID))
	return op
}

func (o *opsd) get(id string) apitypes.Operation {
	o.t.Helper()
	code, b := o.do(http.MethodGet, "/v1/operations/"+id, "")
	if code != http.StatusOK {
		o.t.Fatalf("get %s: %d %s", id, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		o.t.Fatal(err)
	}
	return op
}

func terminal(s apitypes.OperationStatus) bool {
	return s == apitypes.StatusSucceeded || s == apitypes.StatusFailed || s == apitypes.StatusCanceled
}

func (o *opsd) waitStatus(id string, want apitypes.OperationStatus) {
	o.t.Helper()
	waitUntil(o.t, 30*time.Second, id+" "+string(want), func() (bool, string) {
		s := o.get(id).Status
		return s == want, string(s)
	})
}

// waitTerminal waits until operation id is terminal and returns it with
// how long that took.
func (o *opsd) waitTerminal(id string, timeout time.Duration) (apitypes.Operation, time.Duration) {
	o.t.Helper()
	start := time.Now()
	var op apitypes.Operation
	waitUntil(o.t, timeout, id+" to end", func() (bool, string) {
		op = o.get(id)
		return terminal(op.Status), string(op.Status)
	})
	return op, time.Since(start)
}

// logs returns operation id's whole output, frame data concatenated.
func (o *opsd) logs(id string) string {
	o.t.Helper()
	code, b := o.do(http.MethodGet, "/v1/operations/"+id+"/logs", "")
	if code != http.StatusOK {
		o.t.Fatalf("logs %s: %d %s", id, code, b)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			o.t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		if f.Truncated() {
			o.t.Fatalf("output truncated: %s", sc.Text())
		}
		out.WriteString(f.D)
	}
	if err := sc.Err(); err != nil {
		o.t.Fatal(err)
	}
	return out.String()
}

func (o *opsd) opDir(id string) string { return filepath.Join(o.stateDir, "ops", id) }

func markerDir(name string) string { return filepath.Join(markerRoot, name) }

// release lets a gate job's child finish.
func release(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(markerDir(name), "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func problemSlug(op apitypes.Operation) string {
	if op.Error == nil {
		return ""
	}
	return string(op.Error.Slug())
}

// TC-71: the harness is architecture-aware and its init system is ready:
// systemd running with its required units, or OpenRC with docker started
// in the default runlevel. run.sh itself asserts the host side:
// architecture from docker info, binaries in build/harness/ and dist/
// untouched, no bind mount, logs collected and the container removed on
// every exit.
func TestHarnessBootsArchitectureAware(t *testing.T) {
	want := os.Getenv("NEXUS_HARNESS_ARCH")
	if want == "" || runtime.GOARCH != want {
		t.Fatalf("test binary GOARCH %s, harness built for %q", runtime.GOARCH, want)
	}
	uname := strings.TrimSpace(mustSh(t, "uname", "-m"))
	machine := map[string]elf.Machine{"aarch64": elf.EM_AARCH64, "x86_64": elf.EM_X86_64}[uname]
	if (uname == "aarch64") != (want == "arm64") || machine == 0 {
		t.Fatalf("container machine %s, harness architecture %s", uname, want)
	}
	for _, bin := range []string{nexusdBin, opsdBin, os.Args[0]} {
		f, err := elf.Open(bin)
		if err != nil {
			t.Fatalf("%s: %v", bin, err)
		}
		m := f.Machine
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if m != machine {
			t.Fatalf("%s is %v, container is %v", bin, m, machine)
		}
	}
	if detectedInit() == platform.OpenRC {
		rc := mustSh(t, "rc-status", "default")
		if !openrcStarted(rc, "docker") {
			t.Fatalf("docker is not started in the default runlevel:\n%s", rc)
		}
		if a := detach.Available(); !a.Available {
			t.Fatalf("detach.Available on OpenRC: %s", a.Reason)
		}
		t.Logf("arch %s (%s), %s", want, uname, strings.TrimSpace(mustSh(t, "openrc", "--version")))
		return
	}
	state, _ := sh(10*time.Second, detach.Systemctl, "is-system-running")
	if s := strings.TrimSpace(state); s != "running" && s != "degraded" {
		t.Fatalf("system state %q", s)
	}
	for _, u := range []string{"systemd-journald.service", "dbus.service", "docker.service"} {
		if st := activeState(t, u); st != "active" {
			t.Fatalf("%s is %s", u, st)
		}
	}
	if a := detach.SystemdAvailable(); !a.Available {
		t.Fatalf("detach.SystemdAvailable: %s", a.Reason)
	}
	t.Logf("arch %s (%s), system %s, %s", want, uname, strings.TrimSpace(state),
		strings.SplitN(mustSh(t, detach.Systemctl, "--version"), "\n", 2)[0])
}

// TC-71: a failing test still has its logs collected and its container
// removed. It fails only when run.sh is asked to prove that.
func TestHarnessFailurePathIsCollected(t *testing.T) {
	if os.Getenv("NEXUS_HARNESS_EXPECT_FAIL") != "1" {
		t.Skip("set NEXUS_HARNESS_EXPECT_FAIL=1 to prove run.sh collects logs and removes the container on failure")
	}
	t.Fatal("deliberate failure (NEXUS_HARNESS_EXPECT_FAIL=1)")
}

// TC-75: the default detach.New, nil Launch, creates a real nexus-op-*
// unit through the exact systemd-run invocation, for both kill modes.
func TestProductionWiringCreatesRealUnit(t *testing.T) {
	requireInit(t, platform.Systemd)
	opsDir := t.TempDir()
	l, err := detach.New(detach.Config{OpsDir: opsDir, Executable: nexusdBin}, detach.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !l.RealLauncher() {
		t.Fatal("nil Launch did not select the real systemd-run")
	}
	for _, mode := range []detach.KillMode{detach.KillControlGroup, detach.KillProcess} {
		t.Run(string(mode), func(t *testing.T) {
			id := "tc75-" + string(mode)
			spec := detach.Spec{Argv: []string{"/usr/bin/sleep", "300"}, Env: []string{}, Grace: 2 * time.Second,
				KillMode: mode, MaxRuntime: time.Hour}
			ctx := context.Background()
			if err := l.Start(ctx, id, spec); err != nil {
				t.Fatal(err)
			}
			unit := detach.Unit(id)
			p := showUnit(t, unit, "LoadState", "ActiveState", "RefuseManualStop", "KillMode", "ExecStart", "Environment")
			t.Logf("%s: %v", unit, p)
			if p["LoadState"] != "loaded" || p["ActiveState"] != "active" {
				t.Fatalf("unit not running: %v", p)
			}
			if p["RefuseManualStop"] != "yes" || p["KillMode"] != string(mode) {
				t.Fatalf("RefuseManualStop=%s KillMode=%s, want yes and %s", p["RefuseManualStop"], p["KillMode"], mode)
			}
			if want := "argv[]=" + nexusdBin + " op-supervise --dir " + filepath.Join(opsDir, id) + " ;"; !strings.Contains(p["ExecStart"], want) {
				t.Fatalf("ExecStart %q lacks %q", p["ExecStart"], want)
			}
			if p["Environment"] != "" {
				t.Fatalf("unit carries Environment=%q", p["Environment"])
			}
			if err := l.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 30*time.Second, "exit.json of "+id, func() (bool, string) {
				x, ok, err := detach.ReadExit(l.Dir(id))
				return ok && err == nil && x.Canceled, fmt.Sprint(ok, err)
			})
			waitUntil(t, 30*time.Second, unit+" collected", func() (bool, string) {
				st, err := l.State(ctx, id)
				return err == nil && st == detach.Absent, fmt.Sprint(st, err)
			})
		})
	}
}

// wantDirectives is the [Service] section WI 3.5 specifies, in order. A
// change to deploy/nexusd.service must change this list, and 10.1 records
// every removal (A-23).
var wantDirectives = []string{
	"Type=notify",
	"ExecStart=/usr/local/bin/nexusd serve --config /etc/nexus/nexusd.yaml",
	"ExecReload=/bin/kill -HUP $MAINPID",
	"Restart=on-failure",
	"StateDirectory=nexus",
	"StateDirectoryMode=0700",
	"LogsDirectory=nexus",
	"LogsDirectoryMode=0700",
	"NoNewPrivileges=yes",
	"ProtectSystem=strict",
	"ProtectHome=read-only",
	"PrivateTmp=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectKernelLogs=yes",
	"ProtectControlGroups=yes",
	"ProtectClock=yes",
	"ProtectHostname=yes",
	"RestrictNamespaces=yes",
	"RestrictRealtime=yes",
	"RestrictSUIDSGID=yes",
	"LockPersonality=yes",
	"MemoryDenyWriteExecute=yes",
	"SystemCallArchitectures=native",
	"SystemCallFilter=@system-service",
	"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
	"CapabilityBoundingSet=CAP_DAC_READ_SEARCH",
}

// serviceDirectives returns the [Service] lines of a unit file.
func serviceDirectives(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	section := ""
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "["):
			section = line
		case section == "[Service]":
			out = append(out, line)
		}
	}
	return out
}

// denialPatterns are what a sandbox fault leaves in the journal, each with
// the directives that can cause it, so a failure names its suspect.
var denialPatterns = []struct{ pattern, directives string }{
	{"SECCOMP", "SystemCallFilter=@system-service or SystemCallArchitectures=native"},
	{"audit: type=1326", "SystemCallFilter=@system-service or SystemCallArchitectures=native"},
	{"AF_NETLINK", "RestrictAddressFamilies"},
	{"Permission denied", "ProtectSystem=strict, ProtectHome=read-only, PrivateTmp or CapabilityBoundingSet"},
	{"Operation not permitted", "CapabilityBoundingSet, NoNewPrivileges, RestrictNamespaces or RestrictSUIDSGID"},
	{"address family not supported", "RestrictAddressFamilies"},
	{"Read-only file system", "ProtectSystem=strict or ProtectHome=read-only"},
	{"status=31/SYS", "SystemCallFilter=@system-service or SystemCallArchitectures=native"},
	{"Bad system call", "SystemCallFilter=@system-service or SystemCallArchitectures=native"},
}

// journalFaults returns each journal line of the window matching a denial
// pattern, with its suspect directives.
func journalFaults(journal string) []string {
	var faults []string
	for _, d := range denialPatterns {
		for line := range strings.Lines(journal) {
			if strings.Contains(line, d.pattern) {
				faults = append(faults, fmt.Sprintf("journal (suspect %s): %s", d.directives, strings.TrimSpace(line)))
			}
		}
	}
	return faults
}

// startUnderSandbox starts nexusd.service through lib.sh; a failure names
// the suspect directive from the unit's journal, so a sandbox fault is not
// read as a script failure.
func startUnderSandbox(t *testing.T, since time.Time) {
	t.Helper()
	out, err := sh(2*time.Minute, "bash", libSh, "start-nexusd")
	if err == nil {
		return
	}
	journal, _ := sh(30*time.Second, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
	faults := journalFaults(journal)
	if len(faults) == 0 {
		faults = []string{"no denial pattern in the journal: not identified as a sandbox fault"}
	}
	t.Fatalf("nexusd.service did not start under the shipped hardening: %v\n%s\n%s", err, strings.Join(faults, "\n"), out)
}

// sandboxComms are the processes whose kernel log lines concern this test.
var sandboxComms = []string{`comm="nexusd"`, `comm="opsd"`, `comm="systemd-run"`, `comm="systemctl"`}

// TC-227: the shipped unit, installed through lib.sh with no directive
// changed, starts nexusd through Type=notify; netlink works inside the
// sandbox; opsd under the same hardening, through a drop-in that replaces
// only ExecStart, launches an op unit; and nothing is denied.
func TestShippedUnitRunsNexusdAndLaunchesOpUnit(t *testing.T) {
	requireInit(t, platform.Systemd)
	if got := serviceDirectives(t, shippedUnit); !slices.Equal(got, wantDirectives) {
		t.Fatalf("deploy/nexusd.service [Service] differs from the WI 3.5 list:\n got  %q\n want %q", got, wantDirectives)
	}
	dmesgBefore := kernelLines(t)
	since := time.Now().Add(-time.Second)

	// Step 1: install through lib.sh and start.
	lib(t, "stop-nexusd")
	lib(t, "remove-dropins")
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(cfg, []byte("listen: [\"127.0.0.1:7443\"]\ntls: {mode: auto}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(nexusdState, "tls")); err != nil {
		t.Fatal(err)
	}
	lib(t, "install-nexusd", cfg)
	unitInstalled.Do(func() {})
	if fi, err := os.Stat(nexusdConfig); err != nil {
		t.Fatal(err)
	} else if st := fi.Sys().(*syscall.Stat_t); fi.Mode().Perm() != 0o600 || st.Uid != 0 || st.Gid != 0 {
		t.Fatalf("%s: mode %v uid %d gid %d, want root:root 0600", nexusdConfig, fi.Mode().Perm(), st.Uid, st.Gid)
	}
	if b, err := os.ReadFile("/etc/systemd/system/" + nexusdUnit); err != nil {
		t.Fatal(err)
	} else if orig, err := os.ReadFile(shippedUnit); err != nil || !bytes.Equal(b, orig) {
		t.Fatalf("installed unit differs from deploy/nexusd.service (%v)", err)
	}
	t.Cleanup(func() { lib(t, "stop-nexusd") })
	startUnderSandbox(t, since)

	// Step 2: Type=notify and active: systemctl start returned because
	// READY=1 arrived.
	p := showUnit(t, nexusdUnit, "Type", "ActiveState", "SubState", "NotifyAccess", "MainPID", "DropInPaths")
	t.Logf("nexusd.service: %v", p)
	if p["Type"] != "notify" || p["ActiveState"] != "active" || p["SubState"] != "running" || p["NotifyAccess"] != "main" {
		t.Fatalf("nexusd.service %v, want Type=notify ActiveState=active SubState=running NotifyAccess=main", p)
	}
	if p["DropInPaths"] != "" {
		t.Fatalf("drop-ins present: %s", p["DropInPaths"])
	}
	eff := showUnit(t, nexusdUnit, "ProtectSystem", "ProtectHome", "PrivateTmp", "NoNewPrivileges",
		"RestrictAddressFamilies", "SystemCallArchitectures", "CapabilityBoundingSet", "MemoryDenyWriteExecute")
	t.Logf("effective hardening: %v", eff)
	if eff["ProtectSystem"] != "strict" || eff["ProtectHome"] != "read-only" || eff["NoNewPrivileges"] != "yes" ||
		eff["PrivateTmp"] != "yes" || eff["MemoryDenyWriteExecute"] != "yes" ||
		eff["CapabilityBoundingSet"] != "cap_dac_read_search" {
		t.Fatalf("hardening not in effect: %v", eff)
	}

	// Step 3: the certificate covers the hostname, localhost and a
	// non-loopback interface address.
	cert := readCert(t, filepath.Join(nexusdState, "tls", "cert.pem"))
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	var sanIPs []string
	for _, ip := range cert.IPAddresses {
		sanIPs = append(sanIPs, ip.String())
	}
	t.Logf("certificate SANs: DNS %v IP %v", cert.DNSNames, sanIPs)
	if !slices.Contains(cert.DNSNames, host) || !slices.Contains(cert.DNSNames, "localhost") {
		t.Fatalf("SANs %v lack hostname %q or localhost", cert.DNSNames, host)
	}
	ifAddrs := interfaceAddrs(t)
	t.Logf("non-loopback interface addresses (ip -j addr): %v", ifAddrs)
	if len(ifAddrs) == 0 {
		t.Fatal("the container has no non-loopback interface address; the netlink check cannot discriminate")
	}
	covered := false
	for _, a := range ifAddrs {
		if slices.Contains(sanIPs, a) {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("no interface address %v among SAN IPs %v: netlink enumeration failed under RestrictAddressFamilies", ifAddrs, sanIPs)
	}

	// Step 4: opsd under the same [Service] hardening, ExecStart replaced
	// only; opsd sends READY=1, so Type stays notify.
	o := newOpsd(t, "tc227", opsdConfig{onlyExecStart: true})
	dropin, err := os.ReadFile("/etc/systemd/system/nexusd.service.d/opsd.conf")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(dropin)) {
		line = strings.TrimSpace(line)
		if line != "" && line != "[Service]" && !strings.HasPrefix(line, "ExecStart=") {
			t.Fatalf("drop-in changes more than ExecStart: %q", line)
		}
	}
	startUnderSandbox(t, since)
	p = showUnit(t, nexusdUnit, "Type", "ActiveState", "ExecStart", "DropInPaths")
	t.Logf("nexusd.service with the opsd drop-in: %v", p)
	if p["Type"] != "notify" || !strings.Contains(p["ExecStart"], opsdBin) {
		t.Fatalf("opsd drop-in not in effect: %v", p)
	}
	op := o.startJob("tc227", job{Behavior: "quick"})
	unit := detach.Unit(op.ID)
	waitUntil(t, 30*time.Second, unit+" to exist", func() (bool, string) {
		u := showUnit(t, unit, "LoadState", "ActiveState")
		if u["LoadState"] == "loaded" {
			return true, ""
		}
		// A quick job may already be done: its exit.json proves the unit ran.
		return exists(filepath.Join(o.opDir(op.ID), detach.ExitFile)), fmt.Sprint(u)
	})
	end, _ := o.waitTerminal(op.ID, 60*time.Second)
	t.Logf("op %s from inside the sandbox: %s, result %s", op.ID, end.Status, end.Result)
	if end.Status != apitypes.StatusSucceeded {
		t.Fatalf("op %s ended %s (%v)", op.ID, end.Status, end.Error)
	}
	gate := o.startJob("tc227-gate", job{})
	gunit := detach.Unit(gate.ID)
	waitUntil(t, 30*time.Second, gunit+" active", func() (bool, string) {
		u := showUnit(t, gunit, "LoadState", "ActiveState")
		return u["LoadState"] == "loaded" && u["ActiveState"] == "active", fmt.Sprint(u)
	})
	t.Logf("%s: %v", gunit, showUnit(t, gunit, "LoadState", "ActiveState", "RefuseManualStop", "KillMode"))
	release(t, "tc227-gate")
	if end, _ := o.waitTerminal(gate.ID, 60*time.Second); end.Status != apitypes.StatusSucceeded {
		t.Fatalf("gate op ended %s (%v)", end.Status, end.Error)
	}

	// Step 5: no seccomp or permission denial in the window.
	sinceArg := "@" + strconv.FormatInt(since.Unix(), 10)
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--since", sinceArg, "--no-pager")
	kjournal, _ := sh(30*time.Second, "journalctl", "-k", "--since", sinceArg, "--no-pager")
	newKernel := kernelLines(t)
	if len(newKernel) >= len(dmesgBefore) {
		newKernel = newKernel[len(dmesgBefore):]
	}
	faults := journalFaults(journal)
	for _, d := range denialPatterns {
		for _, line := range append(strings.Split(kjournal, "\n"), newKernel...) {
			if strings.Contains(line, d.pattern) {
				if slices.ContainsFunc(sandboxComms, func(c string) bool { return strings.Contains(line, c) }) {
					faults = append(faults, fmt.Sprintf("kernel (suspect %s): %s", d.directives, strings.TrimSpace(line)))
				} else {
					t.Logf("kernel line matching %q from another process: %s", d.pattern, strings.TrimSpace(line))
				}
			}
		}
	}
	res := showUnit(t, nexusdUnit, "Result", "ExecMainStatus", "NRestarts")
	t.Logf("journal window: %d nexusd lines, %d new kernel lines; %v", strings.Count(journal, "\n"), len(newKernel), res)
	if res["Result"] != "success" || res["NRestarts"] != "0" {
		faults = append(faults, fmt.Sprintf("nexusd.service Result=%s NRestarts=%s (a SIGSYS kill points to SystemCallFilter)",
			res["Result"], res["NRestarts"]))
	}
	if len(faults) > 0 {
		t.Fatalf("sandbox denials:\n%s\n--- journal ---\n%s", strings.Join(faults, "\n"), journal)
	}
}

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("%s: no PEM block", path)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// interfaceAddrs reads the non-loopback addresses from `ip -j addr`, an
// independent reader of what nexusd enumerated over netlink.
func interfaceAddrs(t *testing.T) []string {
	t.Helper()
	var ifs []struct {
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(mustSh(t, "ip", "-j", "addr")), &ifs); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range ifs {
		for _, a := range i.AddrInfo {
			if ip := net.ParseIP(a.Local); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

// kernelLines is the kernel ring buffer; the privileged harness can read
// it, and seccomp audit records land there.
func kernelLines(t *testing.T) []string {
	t.Helper()
	out, err := sh(30*time.Second, "dmesg")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Logf("dmesg unavailable: %v %s", err, out)
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}
