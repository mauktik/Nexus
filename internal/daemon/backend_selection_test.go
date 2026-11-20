// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/store"
)

// nginx-provider TC-23: daemon.New on OpenRC launches through the process
// backend. Nothing is injected into Options.Detach: New selects the backend
// from Options.Platform, and the operation's supervisor is the nexusd
// op-supervise --process that the process backend starts, with its
// identity in supervisor.json. PATH is an empty directory and systemd-run
// is never named in the daemon's log; the operation reaching Succeeded is
// the proof that no systemd-run was needed.
func TestDaemonOnOpenRCUsesProcessBackend(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process backend needs Linux")
	}
	exe, err := detachtest.Supervisor() // Before PATH empties: it runs the go tool
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	logs := &syncBuffer{}
	w := newRecWorld(t, detach.Options{})
	w.plat = &platform.Info{Init: platform.OpenRC}
	w.process = detach.ProcessConfig{Executable: exe, CgroupRoot: "-"}
	w.logs = logs
	goOn := filepath.Join(t.TempDir(), "go-on")
	w.prov.script = func(string) string { return "while [ ! -e " + goOn + " ]; do sleep 0.05; done" }

	r := w.start()
	w.waitHealthy()
	code, body, err := w.post(recJob.Op(), "probe", recIn{})
	if err != nil || code != http.StatusAccepted {
		t.Fatalf("POST: %d %v %v", code, body, err)
	}
	id := body["id"].(string)
	dir := filepath.Join(w.stateDir, "ops", id)
	var ident detach.Identity
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if ident, err = detach.ReadIdentity(dir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s: %v", detach.IdentityFile, err)
		}
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", ident.PID))
	if err != nil {
		t.Fatalf("the supervisor's cmdline: %v", err)
	}
	argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if argv[0] != exe || !slices.Contains(argv, "op-supervise") || !slices.Contains(argv, "--process") {
		t.Fatalf("supervisor argv %q, want %s op-supervise ... --process", argv, exe)
	}
	if err := os.WriteFile(goOn, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := waitTerminal(t, r, id, 30*time.Second); rec.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %+v, want Succeeded", rec.Status, rec.Error)
	}
	if n := logs.count("systemd-run"); n != 0 {
		t.Fatalf("the daemon log names systemd-run %d times", n)
	}
}

// recoveryEnd is how restart recovery left one record.
type recoveryEnd struct {
	Status apitypes.OperationStatus
	Slug   string
	Detail string
	Result string
}

// recoveryAfter is what restart recovery left of the four TC-26 records,
// by name, and the answer to a new request on their lane afterwards.
type recoveryAfter struct {
	Ends  map[string]recoveryEnd
	Probe int
}

// recoverFour seeds four Running records on reject lane t as a previous
// nexusd left them, starts a daemon on backend "process" (selected by
// daemon.New on OpenRC) or "local" (detachtest.Local), and returns how
// they ended, all within the 60-second bound:
//   - a: a live supervisor whose child sleeps 3 s;
//   - b: exited, with exit.json code 0;
//   - c: supervisor gone, no exit.json;
//   - d: an identity naming a live process with a forged boot ID.
func recoverFour(t *testing.T, backend, exe string) recoveryAfter {
	t.Helper()
	cfg := detach.ProcessConfig{Executable: exe, CgroupRoot: "-"}
	var w *recWorld
	var launch func(context.Context, detach.Invocation) error
	switch backend {
	case "process":
		w = newRecWorld(t, detach.Options{})
		w.plat = &platform.Info{Init: platform.OpenRC}
		w.process = cfg
		// a's supervisor was started by the nexusd before this one.
		prev := cfg
		prev.OpsDir = filepath.Join(w.stateDir, "ops")
		launch = detach.ProcessBackend(prev).Launch
	case "local":
		local := detachtest.Local()
		w = newRecWorld(t, local)
		launch = local.Launch
	default:
		t.Fatalf("no backend %q", backend)
	}
	names := []string{"a", "b", "c", "d"}
	recs := make([]store.OpRecord, len(names))
	status := make([]apitypes.OperationStatus, len(names))
	for i, n := range names {
		recs[i], status[i] = jobRec(n, "t", store.LaneReject), apitypes.StatusRunning
	}
	ids := w.seed(recs, status)
	dir := func(i int) string { return filepath.Join(w.stateDir, "ops", ids[i]) }
	for i := range ids {
		if err := os.MkdirAll(dir(i), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}

	if err := detach.WriteSpec(dir(0), detach.Spec{Argv: []string{"/bin/sh", "-c", "sleep 3"},
		Env: []string{"PATH=/usr/bin:/bin"}, KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := launch(context.Background(), detach.Invocation{ID: ids[0], Unit: detach.Unit(ids[0]), Dir: dir(0)}); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().UTC().Truncate(time.Second)
	if err := detach.WriteExit(dir(1), detach.Exit{Started: ended.Add(-time.Second), Ended: ended}); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteIdentity(dir(2), detach.Identity{PID: gone.ProcessState.Pid(), StartTicks: 1,
		BootID: strings.TrimSpace(string(boot))}); err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteIdentity(dir(3), detach.Identity{PID: os.Getpid(), StartTicks: startTicks(t, os.Getpid()),
		BootID: "00000000-0000-4000-8000-000000000000"}); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	const bound = 60 * time.Second
	r := w.start()
	w.waitHealthy()
	after := recoveryAfter{Ends: map[string]recoveryEnd{}}
	for i, n := range names {
		rec := waitTerminal(t, r, ids[i], bound-time.Since(t0))
		end := recoveryEnd{Status: rec.Status, Result: string(rec.Result)}
		if rec.Error != nil {
			// The unit name holds the ID, which differs between runs.
			end.Slug, end.Detail = string(rec.Error.Slug()), strings.ReplaceAll(rec.Error.Detail, ids[i], "<id>")
		}
		after.Ends[n] = end
	}
	if took := time.Since(t0); took > bound {
		t.Fatalf("recovery took %v, over %v", took, bound)
	}
	// The slot is freed just after the last terminal commit.
	for retry := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		code, body, err := w.post(recJob.Op(), "probe", recIn{Lane: "t", Policy: "reject"})
		if err != nil {
			t.Fatal(err)
		}
		after.Probe = code
		if code != http.StatusConflict || time.Now().After(retry) {
			if code == http.StatusAccepted {
				waitTerminal(t, r, body["id"].(string), 30*time.Second)
			}
			break
		}
	}
	return after
}

// startTicks is field 22 of /proc/<pid>/stat, counted after the command
// name, which may itself hold spaces and parentheses.
func startTicks(t *testing.T, pid int) uint64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	n, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// nginx-provider TC-26: restart recovery classifies process-backend records
// as on systemd: the live supervisor is reattached and ends Succeeded, the
// exited one is finalized from exit.json, and the gone and forged ones end
// outcome-unknown, within 60 s; the result, and the lane after it, are the
// same as with detachtest.Local.
func TestRecoveryWithProcessBackend(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the process backend needs Linux")
	}
	exe, err := detachtest.Supervisor()
	if err != nil {
		t.Fatal(err)
	}
	var process, local recoveryAfter
	t.Run("process", func(t *testing.T) { process = recoverFour(t, "process", exe) })
	t.Run("local", func(t *testing.T) { local = recoverFour(t, "local", exe) })
	if t.Failed() {
		return
	}
	succeeded := recoveryEnd{Status: apitypes.StatusSucceeded, Result: `{"exitCode":0}`}
	for _, n := range []string{"a", "b"} {
		if got := process.Ends[n]; got != succeeded {
			t.Errorf("process %s: %+v, want %+v", n, got, succeeded)
		}
	}
	for _, n := range []string{"c", "d"} {
		if got := process.Ends[n]; got.Status != apitypes.StatusFailed || got.Slug != string(problem.OutcomeUnknown) {
			t.Errorf("process %s: %+v, want Failed outcome-unknown", n, got)
		}
	}
	if process.Probe != http.StatusAccepted {
		t.Errorf("process: a new request on lane t after recovery got %d, want 202", process.Probe)
	}
	if fmt.Sprint(process) != fmt.Sprint(local) {
		t.Errorf("after recovery\nprocess %+v\nlocal   %+v", process, local)
	}
}

// nginx-provider TC-ERR6: On an unknown init system nexusd serves reads and refuses detached work
// Owner WI 2.4. Requirements: FR-18, FR-26.
//
//	Setup: daemon.New with Platform Unknown, the real availability, and scripts
//	and docker enabled (fake socket).
//	Steps:
//	  1. GET capabilities.
//	  2. Invoke a script.
//	  3. List scripts.
//	Expected:
//	  - Capabilities show initSystem: unknown, and scripts unavailable with a
//	    reason naming no supported init system (systemd or OpenRC).
//	  - The invoke gives capability-unavailable naming the same.
//	  - nexusd is serving: /healthz is 200.
//
// The compiled-in providers keep the real detach.Available, which reads the
// host's own init system: the case runs only on a host that has neither
// systemd nor OpenRC (macOS at T0).
func TestUnknownInitRefusesDetachedWork(t *testing.T) {
	if a := detach.Available(); a.Available {
		t.Skip("this host has a supported init system; the case needs one without")
	}
	const noInit = "no supported init system (systemd or OpenRC)"
	dir := t.TempDir()
	stateDir, auditDir, cfgPath := filepath.Join(dir, "state"), filepath.Join(dir, "audit"), filepath.Join(dir, "nexusd.yaml")
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "t", Scopes: []string{"scripts:read", "scripts:run"}})
	if err != nil {
		t.Fatal(err)
	}
	marker, script := filepath.Join(dir, "ran"), filepath.Join(dir, "touch.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The docker socket does not exist: docker is enabled, and its update
	// gate is the docker package's TC-27 half.
	cfg := fmt.Sprintf("listen: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  docker: {enabled: true, socket: %q}\n"+
		"  scripts:\n    enabled: true\n    catalog:\n"+
		"      - id: touch\n        description: Touch a marker\n        path: %s\n        runAs: nobody\n"+
		"        workingDirectory: %s\n", freePort(t), "/nonexistent/docker.sock", script, dir)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(c, Options{ConfigPath: cfgPath, StateDir: stateDir, AuditDir: auditDir,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Platform: &platform.Info{Init: platform.Unknown}})
	if err != nil {
		t.Fatalf("startup failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	select {
	case <-d.Ready():
	case err := <-done:
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon not ready")
	}
	base := "http://" + d.Addrs()[0].String()
	do := func(method, path string) (int, []byte) {
		t.Helper()
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequest(method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}

	// Step 1.
	code, b := do(http.MethodGet, "/v1/capabilities")
	var caps apitypes.Capabilities
	if err := json.Unmarshal(b, &caps); err != nil || code != http.StatusOK {
		t.Fatalf("capabilities: %d %s", code, b)
	}
	if caps.InitSystem != string(platform.Unknown) {
		t.Errorf("initSystem %q, want %q", caps.InitSystem, platform.Unknown)
	}
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == "scripts" })
	if i < 0 {
		t.Fatalf("scripts is not compiled in: %s", b)
	}
	if p := caps.Providers[i]; p.Status != apitypes.ProviderUnavailable || !strings.Contains(p.Reason, noInit) {
		t.Errorf("scripts %+v, want unavailable naming %q", p, noInit)
	}

	// Step 2.
	code, b = do(http.MethodPost, "/v1/providers/scripts/scripts/touch/invoke")
	var pe problem.Error
	if err := json.Unmarshal(b, &pe); err != nil || code != http.StatusServiceUnavailable ||
		pe.Slug() != problem.CapabilityUnavailable || !strings.Contains(fmt.Sprint(pe.Extensions["reason"]), noInit) {
		t.Fatalf("invoke: %d %s, want 503 capability-unavailable naming %q", code, b, noInit)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the script ran: stat marker: %v", err)
	}

	// Step 3: the provider is unavailable as a whole, so its reads are
	// refused the same way; nexusd itself keeps serving.
	if code, b := do(http.MethodGet, "/v1/providers/scripts/scripts"); code != http.StatusServiceUnavailable {
		t.Errorf("list scripts: %d %s, want 503", code, b)
	}
	if code, b := do(http.MethodGet, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz: %d %s, want 200", code, b)
	}
}
