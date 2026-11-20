// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// fakeMutation is a provider whose lock probe and dpkg commands are
// scripted from the lock captures, with an operations directory and a
// root of its own.
type fakeMutation struct {
	t *testing.T
	p *Provider

	mu     sync.Mutex
	locks  []lockState // Successive probes; the last one repeats
	probes int
	audits [][]byte // Successive dpkg --audit outputs; the last one repeats
	// auditAt is the number of probes made before each audit.
	auditAt []int
	calls   [][]string
	logBuf  bytes.Buffer
	ops     string
}

func newFakeMutation(t *testing.T, lockWait time.Duration) *fakeMutation {
	t.Helper()
	f := &fakeMutation{t: t, ops: t.TempDir(), audits: [][]byte{nil}, locks: []lockState{{}}}
	f.p = New(Options{Logger: slog.New(slog.NewTextHandler(syncWriter{f}, nil))})
	f.p.root = t.TempDir()
	f.p.lockPoll = 5 * time.Millisecond
	f.p.UseOpsDir(f.ops)
	s := config.Default().Providers.Apt.Settings
	s.LockWait, s.MaxRuntime = lockWait, 3*time.Hour
	f.p.settings.Store(&s)
	f.p.probe = func(path string) (lockState, error) {
		if path != f.p.hostPath(FrontendLock) {
			t.Errorf("probed %s", path)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		st := f.locks[min(f.probes, len(f.locks)-1)]
		f.probes++
		return st, nil
	}
	f.p.run = func(_ context.Context, c command) (result, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, slices.Clone(c.Argv))
		switch {
		case slices.Equal(c.Argv, []string{Dpkg, "--audit"}):
			f.auditAt = append(f.auditAt, f.probes)
			n := 0
			for _, call := range f.calls {
				if call[0] == Dpkg {
					n++
				}
			}
			return result{Stdout: f.audits[min(n-1, len(f.audits)-1)]}, nil
		case slices.Equal(c.Argv, cmdDpkgQueryAll().Argv):
			return result{Stdout: capture(t, lockBase, "dpkg-query-before.txt")}, nil
		}
		t.Errorf("unexpected command %q", c.Argv)
		return result{ExitCode: 2}, nil
	}
	return f
}

type syncWriter struct{ f *fakeMutation }

func (w syncWriter) Write(b []byte) (int, error) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	return w.f.logBuf.Write(b)
}

func (f *fakeMutation) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logBuf.String()
}

func (f *fakeMutation) commands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeMutation) put(rel string, b []byte) {
	f.t.Helper()
	path := f.p.hostPath(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeMutation) hasPre(id string) bool {
	_, err := os.Stat(filepath.Join(f.ops, id, PreFile))
	return err == nil
}

var installArgs = []string{"install", "--", "nxplain"}

// A free lock and an empty audit: the spec is apt-get with the fixed
// options, KillMode=process and maxRuntime, and pre.json holds the
// dpkg-query snapshot and dpkg.log's position.
func TestPrepareMutationSnapshotsAndBuildsTheUnit(t *testing.T) {
	f := newFakeMutation(t, 300*time.Second)
	f.put(DpkgLog, []byte("2026-11-08 00:00:00 startup archives unpack\n"))
	r := provider.NewRun("op-free", nil)
	s, err := f.p.PrepareMutation(context.Background(), r, installArgs)
	if err != nil {
		t.Fatal(err)
	}
	if s.KillMode != detach.KillProcess || s.MaxRuntime != 3*time.Hour || s.Dir != "/" || !slices.Equal(s.Env, Env()) {
		t.Errorf("spec %+v", s)
	}
	want := mutationArgv(300*time.Second, installArgs...)
	if !slices.Equal(s.Argv, want) {
		t.Errorf("argv %q\nwant %q", s.Argv, want)
	}
	if err := s.Validate(); err != nil {
		t.Error(err)
	}
	cmds := f.commands()
	if len(cmds) != 2 || cmds[0][0] != Dpkg || cmds[1][0] != DpkgQuery {
		t.Errorf("commands %q, want the audit, then the snapshot", cmds)
	}
	var pre preState
	b, err := os.ReadFile(filepath.Join(f.ops, r.ID(), PreFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pre); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(f.ops, r.ID(), PreFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pre.json mode: %v %v", fi.Mode(), err)
	}
	snap := parseDpkgQuery(capture(t, lockBase, "dpkg-query-before.txt"))
	if len(pre.Packages) != len(snap) || len(snap) < 50 || !pre.DpkgLog.Exists || pre.DpkgLog.Size != 44 || pre.DpkgLog.Inode == 0 {
		t.Errorf("pre.json: %d packages (want %d), dpkg.log %+v", len(pre.Packages), len(snap), pre.DpkgLog)
	}
	if strings.Contains(f.log(), "waiting for the dpkg frontend lock") {
		t.Errorf("a free lock was logged as waited on:\n%s", f.log())
	}
}

// A held lock is waited on, its holder's PID logged once, and the run
// continues once it is free, with what is left of lockWait as apt's own
// timeout.
func TestPrepareMutationWaitsForTheLock(t *testing.T) {
	f := newFakeMutation(t, 300*time.Second)
	f.locks = []lockState{{Held: true, PID: 4242}, {Held: true, PID: 4242}, {Held: true, PID: 4242}, {}}
	var out bytes.Buffer
	s, err := f.p.PrepareMutation(context.Background(), provider.NewRun("op-wait", &out), installArgs)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(f.log(), "holderPid=4242"); n != 1 {
		t.Errorf("holder logged %d times:\n%s", n, f.log())
	}
	if !strings.Contains(out.String(), "held by process 4242") {
		t.Errorf("run output %q", out.String())
	}
	if !f.hasPre("op-wait") || !slices.Contains(s.Argv, "DPkg::Lock::Timeout=300") {
		t.Errorf("argv %q", s.Argv)
	}
	f.mu.Lock()
	auditAt := slices.Clone(f.auditAt)
	f.mu.Unlock()
	if !slices.Equal(auditAt, []int{4}) {
		t.Errorf("audits after %v probes, want one after the 4th, which saw the lock free", auditAt)
	}
}

// The lockWait limit ends package-manager-busy, naming the holder, with no
// audit, no snapshot and no pre.json.
func TestPrepareMutationBusyAtTheLimit(t *testing.T) {
	f := newFakeMutation(t, 100*time.Millisecond)
	f.locks = []lockState{{Held: true, PID: 777}}
	start := time.Now()
	_, err := f.p.PrepareMutation(context.Background(), provider.NewRun("op-busy", nil), installArgs)
	if slugOf(t, err) != problem.PackageManagerBusy || !strings.Contains(err.Error(), "777") {
		t.Fatalf("%v, want package-manager-busy naming 777", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 5*time.Second {
		t.Errorf("busy after %v, want the 100ms lockWait", d)
	}
	if cmds := f.commands(); len(cmds) != 0 || f.hasPre("op-busy") {
		t.Errorf("ran %q, pre.json %v", cmds, f.hasPre("op-busy"))
	}
}

// A cancel during the wait ends Prepare at once with the context's error,
// before the audit, so the core launches nothing.
func TestPrepareMutationCancelDuringTheWait(t *testing.T) {
	f := newFakeMutation(t, time.Hour)
	f.p.lockPoll = time.Minute
	f.locks = []lockState{{Held: true, PID: 99}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.p.PrepareMutation(ctx, provider.NewRun("op-cancel", nil), installArgs)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prepare did not return on cancel")
	}
	if cmds := f.commands(); len(cmds) != 0 || f.hasPre("op-cancel") {
		t.Errorf("ran %q, pre.json %v", cmds, f.hasPre("op-cancel"))
	}
}

// A non-empty audit with the lock still free is package-manager-broken,
// and nothing but the audit ran: no repair.
func TestPrepareMutationBrokenIsNotRepaired(t *testing.T) {
	f := newFakeMutation(t, 300*time.Second)
	f.audits = [][]byte{capture(t, lockBase, "audit-unpacked.stdout")}
	_, err := f.p.PrepareMutation(context.Background(), provider.NewRun("op-broken", nil), installArgs)
	if slugOf(t, err) != problem.PackageManagerBroken || !strings.Contains(err.Error(), "unpacked but not yet configured") {
		t.Fatalf("%v, want package-manager-broken", err)
	}
	if cmds := f.commands(); len(cmds) != 1 || !slices.Equal(cmds[0], []string{Dpkg, "--audit"}) || f.hasPre("op-broken") {
		t.Errorf("ran %q, pre.json %v", cmds, f.hasPre("op-broken"))
	}
}

// S-1: a transaction that took the lock between the probe and the audit
// makes the audit non-empty while healthy; the lock held again sends
// Prepare back to the wait instead of failing.
func TestPrepareMutationAuditDuringATransactionWaitsAgain(t *testing.T) {
	f := newFakeMutation(t, 300*time.Second)
	hold := capture(t, lockBase, "audit-hold.stdout")
	if !strings.Contains(string(hold), "half configured") {
		t.Fatal("audit-hold lacks the transaction in progress; the premise changed")
	}
	// Free for the first probe, held when re-probed after the audit, then
	// free again.
	f.locks = []lockState{{}, {Held: true, PID: 31}, {Held: true, PID: 31}, {}}
	f.audits = [][]byte{hold, nil}
	if _, err := f.p.PrepareMutation(context.Background(), provider.NewRun("op-race", nil), installArgs); err != nil {
		t.Fatal(err)
	}
	cmds := f.commands()
	if len(cmds) != 3 || cmds[0][0] != Dpkg || cmds[1][0] != Dpkg || cmds[2][0] != DpkgQuery || !f.hasPre("op-race") {
		t.Errorf("ran %q", cmds)
	}
}

// finalizeFixture is the state after a real apt-get install from the lock
// captures: pre.json from dpkg-query before it, dpkg's status file after
// it, and dpkg.log with the lines it appended after earlier ones.
func finalizeFixture(t *testing.T) (*fakeMutation, *provider.Run) {
	t.Helper()
	f := newFakeMutation(t, 300*time.Second)
	f.put(DpkgLog, []byte("2026-11-08 00:00:00 conffile /etc/old.conf keep\n"))
	r := provider.NewRun("op-final", nil)
	if _, err := f.p.PrepareMutation(context.Background(), r, installArgs); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(f.p.hostPath(DpkgLog), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write(capture(t, lockBase, "dpkg-log.txt")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	f.put(DpkgStatus, capture(t, lockBase, "status-after.txt"))
	return f, r
}

// Finalize diffs the snapshot against dpkg's status file, reads the kept
// conffiles from dpkg.log at the recorded offset, and records the exit.
func TestFinalizeMutationReadsTheOutcome(t *testing.T) {
	f, r := finalizeFixture(t)
	out, err := f.p.FinalizeMutation(r, detach.Exit{ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range out.Changes {
		got = append(got, fmt.Sprintf("%s:%s %s>%s %s>%s", c.Package, c.Architecture, c.OldVersion, c.NewVersion, c.OldStatus, c.NewStatus))
	}
	want := []string{
		"nxconf:all 1.0>2.0 installed>installed",
		"nxnew:all >1.0 not-installed>installed",
		"nxplain:all 1.0>2.0 installed>installed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes %q\nwant %q", got, want)
	}
	if !slices.Equal(out.Conffiles, []string{"/etc/nxconf.conf"}) || !out.ConffilesComplete {
		t.Errorf("conffiles %q complete %v", out.Conffiles, out.ConffilesComplete)
	}
	if out.ExitCode != 0 || out.RebootRequired != spec.RebootUnknown {
		t.Errorf("outcome %+v", out)
	}

	// After a restart: a new provider over the same files says the same.
	p2 := New(Options{})
	p2.root = f.p.root
	p2.UseOpsDir(f.ops)
	again, err := p2.FinalizeMutation(r, detach.Exit{ExitCode: 0})
	if err != nil || fmt.Sprint(again) != fmt.Sprint(out) {
		t.Errorf("after restart: %+v %v\nwant %+v", again, err, out)
	}

	// apt failed on a held lock: busy, with the outcome still read.
	writeOpOutput(t, f.ops, r.ID(), nil, capture(t, lockBase, "frontend-timeout.stderr"))
	busy, err := f.p.FinalizeMutation(r, detach.Exit{ExitCode: 100})
	if slugOf(t, err) != problem.PackageManagerBusy || busy.ExitCode != 100 || len(busy.Changes) != 3 {
		t.Errorf("exit 100 with lock text: %+v %v", busy, err)
	}
}

// CR-26: a file ucf manages is kept without a dpkg.log line, so a run that
// leaves a package with such a file installed cannot list every kept
// conffile. A registry naming only other packages leaves it complete.
func TestFinalizeMutationWithUcfManagedFiles(t *testing.T) {
	f, r := finalizeFixture(t)
	f.put(UcfRegistry, []byte("openssh-server \t /etc/ssh/sshd_config\ngrub-pc \t /etc/default/grub\n"))
	out, err := f.p.FinalizeMutation(r, detach.Exit{})
	if err != nil || !out.ConffilesComplete {
		t.Fatalf("registry of other packages: complete %v, %v", out.ConffilesComplete, err)
	}
	f.put(UcfRegistry, []byte("openssh-server \t /etc/ssh/sshd_config\nnxconf \t /etc/nxconf.d/ucf.conf\n"))
	out, err = f.p.FinalizeMutation(r, detach.Exit{})
	if err != nil || out.ConffilesComplete || !slices.Equal(out.Conffiles, []string{"/etc/nxconf.conf"}) {
		t.Errorf("registry naming a changed package: %q complete %v, %v", out.Conffiles, out.ConffilesComplete, err)
	}
	if err := os.Remove(f.p.hostPath(UcfRegistry)); err != nil {
		t.Fatal(err)
	}
	if out, err = f.p.FinalizeMutation(r, detach.Exit{}); err != nil || !out.ConffilesComplete {
		t.Errorf("no registry: complete %v, %v", out.ConffilesComplete, err)
	}
	// A registry that cannot be read rules nothing out.
	if err := os.MkdirAll(f.p.hostPath(UcfRegistry), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err = f.p.FinalizeMutation(r, detach.Exit{}); err != nil || out.ConffilesComplete {
		t.Errorf("unreadable registry: complete %v, %v", out.ConffilesComplete, err)
	}
}

// A dpkg.log rotated since Prepare leaves the conffiles incomplete.
func TestFinalizeMutationAfterRotation(t *testing.T) {
	f, r := finalizeFixture(t)
	path := f.p.hostPath(DpkgLog)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	f.put(DpkgLog, []byte("2026-11-08 00:09:00 conffile /etc/after.conf keep\n2026-11-08 00:09:00 conffile /etc/x.conf install\n"))
	out, err := f.p.FinalizeMutation(r, detach.Exit{ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	if out.ConffilesComplete || !slices.Equal(out.Conffiles, []string{"/etc/after.conf"}) {
		t.Errorf("rotated: %q complete %v", out.Conffiles, out.ConffilesComplete)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.p.FinalizeMutation(r, detach.Exit{}); out.ConffilesComplete || len(out.Conffiles) != 0 {
		t.Errorf("missing: %q complete %v", out.Conffiles, out.ConffilesComplete)
	}
}

// dpkg's status file read by Finalize gives exactly what dpkg-query printed
// at the same moment.
func TestParseStatusFileMatchesDpkgQuery(t *testing.T) {
	fromFile := parseStatusFile(capture(t, lockBase, "status-after.txt"))
	fromQuery := snapOf(parseDpkgQuery(capture(t, lockBase, "dpkg-query-after.txt")))
	if len(fromQuery) < 50 || len(diffPackages(fromQuery, fromFile)) != 0 || len(diffPackages(fromFile, fromQuery)) != 0 {
		t.Errorf("status file %d packages, dpkg-query %d; differences %+v", len(fromFile), len(fromQuery),
			diffPackages(fromQuery, fromFile))
	}
	before := snapOf(parseDpkgQuery(capture(t, lockBase, "dpkg-query-before.txt")))
	if n := len(diffPackages(before, fromQuery)); n != 3 {
		t.Errorf("%d changes between the captures, want 3", n)
	}
}

func snapOf(pkgs []dpkgPackage) []snapPackage {
	out := make([]snapPackage, len(pkgs))
	for i, d := range pkgs {
		out[i] = snapPackage(d)
	}
	return out
}

func TestParseConffileKeeps(t *testing.T) {
	log := "2026-11-08 00:03:37 conffile /etc/nxconf.conf keep\n" +
		"2026-11-08 00:03:37 conffile /etc/new.conf install\n" +
		"2026-11-08 00:03:37 conffile /etc/with space.conf keep\n" +
		"2026-11-08 00:03:37 status installed nxconf:all 2.0\n" +
		"garbage keep\n2026-11-08 00:03:37 conffile keep\n"
	if got := parseConffileKeeps([]byte(log)); !slices.Equal(got, []string{"/etc/nxconf.conf", "/etc/with space.conf"}) {
		t.Errorf("%q", got)
	}
}
