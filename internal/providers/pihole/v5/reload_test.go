// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Cases: TC-156, TC-157 and TC-ERR33 in planning/pihole-dns-zones/test-cases.md
// (WI 5.6): the FTL list-reload barrier of the v5 Mutator against a
// FakeHost whose FakeFTL encodes the reload race captured on FTL v5.25.2
// (test/harness/pihole/PROVENANCE "FTL RELOAD RACE", "FTL LIST RELOAD
// SIGNALS"): `pihole restartdns reload-lists` returns before FTL re-reads
// gravity.db, and a gravity.db commit during that re-read fails it and leaves
// FTL answering pi.hole only.
//
// Regression for the verified hazard "FTL list reload race" (A-64): a
// domain or adlist write must never land while FTL reloads its lists, and
// an operation returns only once FTL has reloaded them.
package v5_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const piholeBin = "/usr/local/bin/pihole"

var reloadListsArgv = []string{piholeBin, "restartdns", "reload-lists"}

// barrierHost is a FakeHost and a Mutator whose DNS server is its FakeFTL,
// with real sleeps.
func barrierHost(t *testing.T) (*v5test.FakeHost, *v5.Mutator) {
	t.Helper()
	h := v5test.NewFakeHost(t)
	return h, &v5.Mutator{Paths: h.Paths(), Runner: h, DNSServer: h.FTL().Addr(), Now: h.Now, Reload: v5test.FastReload()}
}

// fakeTime makes m's sleeps advance h's clock at once, with no real wait.
func fakeTime(h *v5test.FakeHost, m *v5.Mutator) {
	m.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.Advance(d)
		return nil
	}
}

// A-67: production's bounds are the constants; tests shorten the polls, the
// quiet window and the probe through Mutator.Reload (v5test.FastReload) and
// keep the 30 s cap, which TC-ERR33 reaches with a fake clock.
func TestListReloadBoundsArePinned(t *testing.T) {
	if v5.ListReloadPoll != 100*time.Millisecond || v5.ListReloadQuiet != 500*time.Millisecond ||
		v5.ListReloadWait != 30*time.Second || v5.ListReloadRecoveries != 1 ||
		v5.ListReloadProbeTimeout != time.Second || v5.ListReloadProbeTries != 3 {
		t.Errorf("bounds poll %s quiet %s wait %s recoveries %d probe %s x%d",
			v5.ListReloadPoll, v5.ListReloadQuiet, v5.ListReloadWait, v5.ListReloadRecoveries,
			v5.ListReloadProbeTimeout, v5.ListReloadProbeTries)
	}
	if f := v5test.FastReload(); f.Wait != 0 || f.Poll >= v5.ListReloadPoll || f.Quiet >= v5.ListReloadQuiet {
		t.Errorf("FastReload %+v must shorten poll and quiet and leave the wait cap alone", f)
	}
}

// syncBuf is an op log written by an action in another goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// changeCounter is gravity.db's file change counter (header bytes 24-27),
// read by the test itself.
func changeCounter(t *testing.T, h *v5test.FakeHost) uint32 {
	t.Helper()
	b, err := os.ReadFile(h.Paths().GravityDBPath())
	if err != nil || len(b) < 28 {
		t.Fatalf("gravity.db header: %v (%d bytes)", err, len(b))
	}
	return binary.BigEndian.Uint32(b[24:28])
}

func ftlLog(t *testing.T, h *v5test.FakeHost) string {
	t.Helper()
	b, err := os.ReadFile(h.FTL().LogPath())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func countArgv(argvs [][]string, prefix []string) int {
	n := 0
	for _, a := range argvs {
		if len(a) >= len(prefix) && slices.Equal(a[:len(prefix)], prefix) {
			n++
		}
	}
	return n
}

func slug(t *testing.T, err error) *problem.Error {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a problem", err)
	}
	return pe
}

func seedAdlist(t *testing.T, h *v5test.FakeHost, url string, gravity bool) int64 {
	t.Helper()
	if code, err := h.Run(context.Background(), []string{piholeBin, "-a", "adlist", "add", url}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("seed %s: %d %v", url, code, err)
	}
	if gravity {
		if code, err := h.Run(context.Background(), []string{piholeBin, "-g"}, io.Discard, io.Discard); code != 0 || err != nil {
			t.Fatalf("gravity: %d %v", code, err)
		}
	}
	h.ResetArgvs()
	return int64(len(h.Adlists()))
}

// startHeldReload starts a list reload that stays in progress, as a
// previous operation's reload or a blocking stop's would, and forgets the
// argv.
func startHeldReload(t *testing.T, h *v5test.FakeHost) {
	t.Helper()
	h.FTL().HoldReloads()
	if code, err := h.Run(context.Background(), reloadListsArgv, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("reload-lists: %d %v", code, err)
	}
	h.ResetArgvs()
	if !h.FTL().Reloading() {
		t.Fatal("no reload in progress")
	}
}

// waitIdle waits until FakeFTL has no reload in progress.
func waitIdle(t *testing.T, h *v5test.FakeHost) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); h.FTL().Reloading(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("FakeFTL still reloading")
		}
	}
}

// TC-156 (FR-10 to FR-13, A-64): no gravity.db write while a list reload is
// in flight. Each action, started while a reload is held, writes nothing
// until it ends: gravity.db's change counter stays, and no `-a adlist add`
// or `-g` runs; after the release the action succeeds and the held reload
// completes without failure markers.
func TestTC156_NoWriteWhileReloadInFlight(t *testing.T) {
	type action struct {
		name  string
		setup func(t *testing.T, h *v5test.FakeHost) int64
		run   func(ctx context.Context, m *v5.Mutator, id int64, out io.Writer) error
		argv  []string // the command that writes, which must not run while held
	}
	actions := []action{
		{"add-domain", nil, func(ctx context.Context, m *v5.Mutator, _ int64, out io.Writer) error {
			_, _, err := m.AddDomain(ctx, spec.DomainAddInput{List: "deny", Kind: "exact", Value: "held.example.com"}, out)
			return err
		}, nil},
		{"remove-domain", func(t *testing.T, h *v5test.FakeHost) int64 { return h.SeedDomain(1, "held.example.com", "") },
			func(ctx context.Context, m *v5.Mutator, id int64, out io.Writer) error {
				_, err := m.RemoveDomain(ctx, id, out)
				return err
			}, nil},
		{"add-adlist", nil, func(ctx context.Context, m *v5.Mutator, _ int64, out io.Writer) error {
			_, _, err := m.AddAdlist(ctx, "https://lists.example/new.txt", "", out)
			return err
		}, []string{piholeBin, "-a", "adlist", "add"}},
		{"remove-adlist", func(t *testing.T, h *v5test.FakeHost) int64 {
			return seedAdlist(t, h, "https://lists.example/old.txt", true)
		},
			func(ctx context.Context, m *v5.Mutator, id int64, out io.Writer) error {
				_, err := m.RemoveAdlist(ctx, id, out)
				return err
			}, nil},
		{"update-gravity", nil, func(ctx context.Context, m *v5.Mutator, _ int64, out io.Writer) error {
			_, err := m.UpdateGravity(ctx, out)
			return err
		}, []string{piholeBin, "-g"}},
	}
	for _, a := range actions {
		t.Run(a.name, func(t *testing.T) {
			h, m := barrierHost(t)
			var id int64
			if a.setup != nil {
				id = a.setup(t, h)
			}
			waitIdle(t, h)
			startHeldReload(t, h)
			counter := changeCounter(t, h)
			mark := len(ftlLog(t, h))
			var out syncBuf
			done := make(chan error, 1)
			go func() { done <- a.run(context.Background(), m, id, &out) }()
			for end := time.Now().Add(150 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
				if c := changeCounter(t, h); c != counter {
					t.Errorf("gravity.db committed (counter %d -> %d) while FTL reloaded its lists", counter, c)
					break
				}
				if a.argv != nil && countArgv(h.Argvs(), a.argv) != 0 {
					t.Errorf("%q ran while FTL reloaded its lists", a.argv)
					break
				}
			}
			// The "waiting" line is written once the action has looked at the
			// log: wait for it as long as a loaded host needs, not for 150 ms.
			waitFor(t, "the waiting line", func() bool {
				return strings.Contains(out.String(), "pihole: waiting for FTL to finish reloading its lists")
			})
			h.FTL().ReleaseReloads()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("after release: %v\n%s", err, out.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the action did not end after the release")
			}
			waitIdle(t, h)
			if log := ftlLog(t, h)[mark:]; strings.Contains(log, "database is locked") || strings.Contains(log, "Gravity database not available") {
				t.Errorf("a reload failed:\n%s", log)
			}
			if h.FTL().Stuck() {
				t.Error("FakeFTL is stuck after the action")
			}
		})
	}

	t.Run("add-adlist of a present URL waits for nothing", func(t *testing.T) {
		h, m := barrierHost(t)
		seedAdlist(t, h, "https://lists.example/present.txt", false)
		waitIdle(t, h)
		startHeldReload(t, h)
		defer h.FTL().ReleaseReloads()
		counter := changeCounter(t, h)
		start := time.Now()
		r, created, err := m.AddAdlist(context.Background(), "https://lists.example/present.txt", "", io.Discard)
		if err != nil || created || r.URL != "https://lists.example/present.txt" {
			t.Fatalf("add = %+v, %v, %v", r, created, err)
		}
		// The held reload never ends before the deferred release, so any
		// wait for it would last far longer than this bound.
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("an idempotent add waited %s", took)
		}
		if changeCounter(t, h) != counter || len(h.Argvs()) != 0 {
			t.Errorf("an idempotent add wrote: counter %d -> %d, argvs %q", counter, changeCounter(t, h), h.Argvs())
		}
	})
}

// TC-157 (FR-10, FR-12, NFR-5, A-66, A-68, A-69): a failed reload is
// retried once, and a stuck FTL is reloaded before a write, whether its log
// shows the failure or only a query does.
func TestTC157_RetryAndRepairBeforeWrite(t *testing.T) {
	t.Run("a failed reload after the commit is retried once", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().FailNextReloads(1)
		var out bytes.Buffer
		if _, created, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "retry.example.com"}, &out); err != nil || !created {
			t.Fatalf("add: %v %v\n%s", created, err, out.String())
		}
		if !strings.Contains(out.String(), "pihole: FTL's list reload failed (gravity.db was busy); reloading again") {
			t.Errorf("log:\n%s", out.String())
		}
		if n := countArgv(h.Argvs(), reloadListsArgv); n != 2 {
			t.Errorf("%d reload-lists after the commit, want 2: %q", n, h.Argvs())
		}
		if h.FTL().Stuck() {
			t.Error("FakeFTL stuck after the retry")
		}
	})

	t.Run("a stuck FTL with a failed record is reloaded before an adlist add", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().ForceStuck()
		var out bytes.Buffer
		if _, created, err := m.AddAdlist(context.Background(), "https://lists.example/r.txt", "", &out); err != nil || !created {
			t.Fatalf("add-adlist: %v %v\n%s", created, err, out.String())
		}
		argvs := h.Argvs()
		reload := slices.IndexFunc(argvs, func(a []string) bool { return slices.Equal(a, reloadListsArgv) })
		add := slices.IndexFunc(argvs, func(a []string) bool { return len(a) > 3 && a[1] == "-a" && a[3] == "add" })
		if reload < 0 || add < 0 || reload > add {
			t.Errorf("argvs %q: want reload-lists before -a adlist add", argvs)
		}
		if h.FTL().Stuck() {
			t.Error("FakeFTL still stuck after the add (an adlist add sends no reload of its own)")
		}
	})

	t.Run("the probe finds a stuck FTL whose log shows success", func(t *testing.T) {
		h, m := barrierHost(t)
		id := h.SeedDomain(1, "seeded.example.com", "")
		h.FTL().ForceStuckUnlogged()
		var out bytes.Buffer
		if _, err := m.RemoveDomain(context.Background(), id, &out); err != nil {
			t.Fatalf("remove: %v\n%s", err, out.String())
		}
		log := out.String()
		first := strings.Index(log, "pihole: running pihole restartdns reload-lists")
		removed := strings.Index(log, "pihole: removed deny exact entry")
		if first < 0 || removed < 0 || first > removed {
			t.Errorf("want a reload before the delete:\n%s", log)
		}
		if n := countArgv(h.Argvs(), reloadListsArgv); n != 2 {
			t.Errorf("%d reload-lists, want 2 (repair, then after the commit)", n)
		}
		if h.FTL().Stuck() {
			t.Error("FakeFTL stuck after the remove")
		}
	})
}

// TC-ERR33 (FR-10, FR-12, NFR-5, A-67, A-68): the barrier's failure paths,
// each through add-domain.
func TestTCERR33_BarrierFailurePaths(t *testing.T) {
	add := func(ctx context.Context, m *v5.Mutator, out io.Writer) error {
		_, _, err := m.AddDomain(ctx, spec.DomainAddInput{List: "deny", Kind: "exact", Value: "err33.example.com"}, out)
		return err
	}
	commandFailed := func(t *testing.T, err error, state string) *problem.Error {
		t.Helper()
		pe := slug(t, err)
		if pe.Slug() != problem.PiholeCommandFailed || pe.Extensions["command"] != "pihole restartdns reload-lists" ||
			pe.Extensions["exitCode"] != 0 || pe.Extensions["rolledBack"] != false {
			t.Fatalf("got %s %+v: %s", pe.Slug(), pe.Extensions, pe.Detail)
		}
		for _, s := range []string{state, "may answer only pi.hole", "run `pihole restartdns reload-lists`"} {
			if !strings.Contains(pe.Detail, s) {
				t.Errorf("detail %q lacks %q", pe.Detail, s)
			}
		}
		return pe
	}

	t.Run("1 both reloads fail", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().FailNextReloads(2)
		err := add(context.Background(), m, io.Discard)
		commandFailed(t, err, "was added")
		if len(h.DomainRows()) != 1 {
			t.Error("the committed row must stay (A-68)")
		}
	})

	t.Run("2 the reload after the commit never ends", func(t *testing.T) {
		h, m := barrierHost(t)
		fakeTime(h, m)
		h.FTL().HoldReloads()
		defer h.FTL().ReleaseReloads()
		start := h.Now()
		err := add(context.Background(), m, io.Discard)
		pe := commandFailed(t, err, "was added")
		if !strings.Contains(pe.Detail, "within 30 s") || h.Now().Sub(start) < 30*time.Second {
			t.Errorf("detail %q after %s of fake time", pe.Detail, h.Now().Sub(start))
		}
		if len(h.DomainRows()) != 1 {
			t.Error("the committed row must stay")
		}
	})

	t.Run("3 a reload in flight never ends", func(t *testing.T) {
		h, m := barrierHost(t)
		fakeTime(h, m)
		startHeldReload(t, h)
		defer h.FTL().ReleaseReloads()
		err := add(context.Background(), m, io.Discard)
		pe := slug(t, err)
		if pe.Slug() != problem.PiholeBusy {
			t.Fatalf("got %s: %s", pe.Slug(), pe.Detail)
		}
		if w, ok := pe.Extensions["waitedSeconds"].(int); !ok || w < 30 {
			t.Errorf("waitedSeconds %v", pe.Extensions["waitedSeconds"])
		}
		if len(h.DomainRows()) != 0 || countArgv(h.Argvs(), reloadListsArgv) != 0 {
			t.Errorf("rows %v argvs %q: nothing may be written or reloaded", h.DomainRows(), h.Argvs())
		}
	})

	t.Run("4 the log is gone while FTL runs", func(t *testing.T) {
		h, m := barrierHost(t)
		if err := os.Remove(h.FTL().LogPath()); err != nil {
			t.Fatal(err)
		}
		err := add(context.Background(), m, io.Discard)
		pe := slug(t, err)
		if pe.Slug() != problem.DependencyUnavailable || !strings.Contains(pe.Detail, "/var/log/pihole/FTL.log") ||
			!strings.Contains(pe.Detail, "nothing was changed") {
			t.Fatalf("got %s: %s", pe.Slug(), pe.Detail)
		}
		if len(h.DomainRows()) != 0 || countArgv(h.Argvs(), reloadListsArgv) != 0 {
			t.Errorf("rows %v argvs %q", h.DomainRows(), h.Argvs())
		}
	})

	t.Run("5 FTL is stopped", func(t *testing.T) {
		h, m := barrierHost(t)
		fakeTime(h, m)
		h.FTL().Stop()
		var out bytes.Buffer
		start := h.Now()
		if err := add(context.Background(), m, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "pihole: FTL is not running; its lists load when it starts") || h.Now() != start {
			t.Errorf("waited %s; log:\n%s", h.Now().Sub(start), out.String())
		}
	})

	for _, how := range []string{"truncated", "renamed"} {
		t.Run("6 the log is "+how+" after the mark", func(t *testing.T) {
			h, m := barrierHost(t)
			h.FTL().HoldReloads()
			var out syncBuf
			done := make(chan error, 1)
			go func() { done <- add(context.Background(), m, &out) }()
			for deadline := time.Now().Add(5 * time.Second); countArgv(h.Argvs(), reloadListsArgv) == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no reload-lists")
				}
			}
			time.Sleep(100 * time.Millisecond) // the barrier has polled the start marker
			p := h.FTL().LogPath()
			var err error
			if how == "truncated" {
				err = os.Truncate(p, 0)
			} else {
				err = os.Rename(p, p+".1")
			}
			if err != nil {
				t.Fatal(err)
			}
			h.FTL().ReleaseReloads()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%v\n%s", err, out.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the add did not end")
			}
			if !strings.Contains(out.String(), "pihole: FTL reloaded its lists in ") {
				t.Errorf("log:\n%s", out.String())
			}
		})
	}

	t.Run("7 stuck, and both repair reloads fail", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().ForceStuck()
		h.FTL().FailNextReloads(2)
		err := add(context.Background(), m, io.Discard)
		commandFailed(t, err, "nothing was changed")
		if len(h.DomainRows()) != 0 {
			t.Errorf("rows %v: no row may be written", h.DomainRows())
		}
	})

	t.Run("8 ctx is cancelled while the barrier waits", func(t *testing.T) {
		h, m := barrierHost(t)
		startHeldReload(t, h)
		defer h.FTL().ReleaseReloads()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		err := add(ctx, m, io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want it to wrap context.Canceled", err)
		}
		if len(h.DomainRows()) != 0 {
			t.Error("a row was written")
		}
	})

	t.Run("9 reload-lists exits 1", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FailCommand(reloadListsArgv, 1)
		err := add(context.Background(), m, io.Discard)
		pe := slug(t, err)
		if pe.Slug() != problem.PiholeCommandFailed || pe.Extensions["exitCode"] != 1 || !strings.Contains(pe.Detail, "lists may not be reloaded") {
			t.Errorf("got %s %+v: %s", pe.Slug(), pe.Extensions, pe.Detail)
		}
	})
}

// scriptStep runs fn once m's sleeps have moved the clock at past the
// first sleep.
type scriptStep struct {
	at time.Duration
	fn func()
}

// scriptTime makes m's sleeps advance h's clock at once and then run, in
// m's goroutine and in order, the steps that have come due. FakeFTL's
// reloads take an hour, so they end only when a step calls EndReload: the
// test decides when each log line appears relative to the barrier's polls,
// with no wall-clock wait. ran is how many steps ran.
func scriptTime(t *testing.T, h *v5test.FakeHost, m *v5.Mutator, steps ...scriptStep) (ran func() int) {
	t.Helper()
	h.FTL().SetReloadDuration(time.Hour)
	var t0 time.Time
	next := 0
	m.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t0.IsZero() {
			t0 = h.Now()
		}
		h.Advance(d)
		for next < len(steps) && h.Now().Sub(t0) >= steps[next].at {
			steps[next].fn()
			next++
		}
		return nil
	}
	return func() int { return next }
}

func endReload(t *testing.T, h *v5test.FakeHost) func() {
	return func() {
		if !h.FTL().EndReload() {
			t.Error("no reload in progress to end")
		}
	}
}

func signalReloadLists(t *testing.T, h *v5test.FakeHost) func() {
	return func() {
		if code, err := h.Run(context.Background(), reloadListsArgv, io.Discard, io.Discard); code != 0 || err != nil {
			t.Errorf("reload-lists: %d %v", code, err)
		}
	}
}

// The barrier's log reading: a reload signalled while another runs makes
// one more reload follow (FTL's event flag, src/signals.c L290); the
// barrier returns only once that one has ended too and the log is quiet.
// Every poll is on the scripted clock: Nexus's reload takes 150 ms, a
// reload-lists outside Nexus lands 10 ms into it, and the one that follows
// takes 150 ms more.
func TestListReloadBarrierWaitsForAFollowingReload(t *testing.T) {
	h, m := barrierHost(t)
	ran := scriptTime(t, h, m,
		scriptStep{10 * time.Millisecond, signalReloadLists(t, h)}, // a reload outside Nexus, while Nexus's own runs
		scriptStep{150 * time.Millisecond, endReload(t, h)},
		scriptStep{300 * time.Millisecond, endReload(t, h)},
	)
	var out bytes.Buffer
	if _, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "allow", Kind: "exact", Value: "follow.example.com"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if ran() != 3 || h.FTL().Reloading() || h.FTL().Stuck() || h.FTL().ReloadsCompleted() != 2 {
		t.Errorf("returned after %d of 3 steps while FakeFTL reloading=%v stuck=%v completed=%d\nop log:\n%sFTL log:\n%s", ran(),
			h.FTL().Reloading(), h.FTL().Stuck(), h.FTL().ReloadsCompleted(), out.String(), ftlLog(t, h))
	}
}

// Phase 5 verification (T1: TestPiholeWriteWaitsForQueuedReload): a write
// that arrives just after FTL's completion line of a reload with another
// queued behind it. The log shows a record that has ended, with two start
// lines; the queued reload logs no start line of its own. FTL holds every
// query while it reloads (PROVENANCE "FTL LIST RELOAD SIGNALS", QUERIES
// DURING A RELOAD): a query sent after the first completion line is held
// through the queued reload too (only a few queries held BEFORE it were
// answered in the gap, FakeFTL's QueuedGapAnswers), so the pre-write probe,
// which the barrier sends after the first completion line, is answered only
// once the queued reload has ended, and the commit cannot meet it. This is
// the residual risk A-69 records: a probe answered in the gap would let a
// commit meet the queued reload's start; it was not observed in 13 rounds.
func TestListReloadBarrierWriteAfterACompletionWithAQueuedReload(t *testing.T) {
	h, m := barrierHost(t)
	f := h.FTL()
	f.SetReloadDuration(time.Hour)
	signalReloadLists(t, h)()
	signalReloadLists(t, h)() // queued: one more reload follows
	mark := len(ftlLog(t, h))
	f.SetReloadDuration(100 * time.Millisecond)
	endReload(t, h)() // the first ends now; the queued one begins and takes 100 ms
	var out bytes.Buffer
	if _, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "queued.example.com"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	probed := -1
	for _, q := range f.Queries() {
		if strings.HasPrefix(q.Name, "nexus-probe-") {
			probed = q.Reloads
			break
		}
	}
	log := ftlLog(t, h)[mark:]
	if probed != 2 || f.Stuck() || strings.Contains(log, "database is locked") || strings.Contains(log, "Gravity database not available") {
		t.Errorf("first probe answered after %d reloads (want 2), stuck %v\nop log:\n%sFTL log:\n%s", probed, f.Stuck(),
			out.String(), log)
	}
}

// A FTL that dies during a reload and is restarted leaves a start line with
// no completion line; the new process's "FTL started" line ends it, so the
// next write does not wait for a reload that will never complete.
func TestListReloadBarrierNewFTLProcessEndsOldReload(t *testing.T) {
	h, m := barrierHost(t)
	startHeldReload(t, h)
	if code, err := h.Run(context.Background(), []string{piholeBin, "restartdns"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("restartdns: %d %v", code, err)
	}
	h.FTL().ReleaseReloads()
	waitIdle(t, h)
	for deadline := time.Now().Add(3 * time.Second); func() bool { _, ok := m.FTLPID(); return !ok }(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("restarted FTL has no PID file")
		}
	}
	fakeTime(h, m)
	var out bytes.Buffer
	// add-adlist: the pre-write check, and no reload of its own to wait for.
	if _, _, err := m.AddAdlist(context.Background(), "https://lists.example/restarted.txt", "", &out); err != nil {
		t.Fatalf("add after an FTL restart: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "waiting for FTL to finish reloading") {
		t.Errorf("waited for the dead process's reload:\n%s", out.String())
	}
}

// Two reload signals within one sleep of FTL's database thread merge into
// one reload: two start lines, one completion line (the TC-125 control on
// the image: `pihole -w X; pihole -b X`, then reload-lists). The log then
// shows no reload in flight, and a write does not wait for one.
func TestListReloadBarrierMergedSignals(t *testing.T) {
	h, m := barrierHost(t)
	f, err := os.OpenFile(h.FTL().LogPath(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("[2026-03-14 15:10:00.100 4242M] Received: Real-time signal 0 (34 -> 0)\n" +
		"[2026-03-14 15:10:00.140 4242M] Received: Real-time signal 0 (34 -> 0)\n" +
		"[2026-03-14 15:10:00.210 4242/T4258] Compiled 0 whitelist and 0 blacklist regex filters for 1 clients in 0.1 msec\n" +
		"[2026-03-14 15:10:00.210 4242/T4258] Blocking status is enabled\n")
	if cerr := f.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	fakeTime(h, m)
	var out bytes.Buffer
	if _, _, err := m.AddAdlist(context.Background(), "https://lists.example/merged.txt", "", &out); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "waiting for FTL") {
		t.Errorf("waited for a reload that is not in flight:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Code review iteration 1 (CR-1, CR-3, CR-4, CR-7, CR-10): the fences the
// review found missing, each shown to fail when the behavior it guards is
// removed.

// hookRunner is a Runner that runs after(argv) once the command ended, in
// the caller's goroutine: the test's way to do something to the host between
// a command's effect and the barrier's next look.
type hookRunner struct {
	v5.Runner
	after func(argv []string)
}

func (r hookRunner) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	code, err := r.Runner.Run(ctx, argv, stdout, stderr)
	if r.after != nil {
		r.after(argv)
	}
	return code, err
}

// appendFTLLog writes whole log lines, as FTL would, with the time and the
// process (`[<time> <pid>M] <msg>`), straight into FTL's log.
func appendFTLLog(t *testing.T, h *v5test.FakeHost, msgs ...string) {
	t.Helper()
	f, err := os.OpenFile(h.FTL().LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, err := f.WriteString("[2026-03-14 15:10:00.100 4242M] " + m + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond every 2 ms for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// CR-3 (PROVENANCE "FTL LIST RELOAD SIGNALS"): after a reload's completion
// line FTL still reads gravity.db without logging (check_inaccessible_adlists),
// and a commit during that read fails it. The barrier's quiet window is what
// keeps the next write out of it: an operation must not return, and so a
// next write must not start, while that read may still run. FakeFTL's read
// is made longer than a probe's answer, 150 ms, and the barrier's quiet
// longer than that: removing the quiet window lets the commit meet the read.
// On the device the read holds queries (lock_shm), so a probe sent during it
// waits for it; FakeFTL answers the probe during the read, stricter than
// FTL, so that it is the quiet window and not a probe that outlasts the read
// that keeps the commit out (DefaultPostCompileReads).
func TestListReloadQuietWindowKeepsTheNextCommitOutOfTheReadAfterCompile(t *testing.T) {
	h, m := barrierHost(t)
	h.FTL().SetPostCompileReads(150 * time.Millisecond)
	m.Reload = v5.ReloadTiming{Poll: 10 * time.Millisecond, Quiet: 250 * time.Millisecond, ProbeTimeout: 250 * time.Millisecond}
	mark := len(ftlLog(t, h))
	if _, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "first.example.com"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	h.SeedDomain(1, "next.example.com", "") // the next operation's commit, at once
	time.Sleep(400 * time.Millisecond)
	if log := ftlLog(t, h)[mark:]; strings.Contains(log, v5test.LogInaccessibleAdlists) || h.FTL().Stuck() {
		t.Errorf("a commit right after the operation met FTL's read after the completion line (stuck %v):\n%s", h.FTL().Stuck(), log)
	}
}

// CR-3: gravity.sh ends with `pihole restartdns reload` (SIGHUP, L946 on the
// image), so a write right after `pihole -g` meets that reload unless the
// barrier waits for it (the next write's settleLists, A-69). FakeHost's -g
// now sends it.
func TestListReloadBarrierAWriteAfterUpdateGravityWaitsForGravitysOwnReload(t *testing.T) {
	h, m := barrierHost(t)
	h.FTL().SetReloadDuration(150 * time.Millisecond)
	mark := len(ftlLog(t, h))
	ctx := context.Background()
	if _, err := m.UpdateGravity(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !h.FTL().Reloading() {
		t.Fatal("pihole -g sent no reload: gravity.sh ends with `restartdns reload`")
	}
	var out bytes.Buffer
	if _, _, err := m.AddDomain(ctx, spec.DomainAddInput{List: "deny", Kind: "exact", Value: "after-gravity.example.com"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	log := ftlLog(t, h)[mark:]
	if !strings.Contains(out.String(), "pihole: waiting for FTL to finish reloading its lists") ||
		strings.Contains(log, "database is locked") || strings.Contains(log, "Gravity database not available") || h.FTL().Stuck() {
		t.Errorf("the write did not wait for gravity's reload (stuck %v)\nop log:\n%sFTL log:\n%s", h.FTL().Stuck(), out.String(), log)
	}
}

// CR-4: a new FTL process's "FTL started" line ends a reload record the old
// process left open (it died mid-reload), so the next write does not wait 30 s
// and fail pihole-busy. The log has a start line and then the new process's
// start line, and no completion line: only that line can end the record.
func TestListReloadBarrierNewProcessLineEndsARecordLeftOpen(t *testing.T) {
	h, m := barrierHost(t)
	appendFTLLog(t, h, "Received: Real-time signal 0 (34 -> 0)", "########## FTL started on fakehost! ##########")
	fakeTime(h, m)
	var out bytes.Buffer
	if _, _, err := m.AddAdlist(context.Background(), "https://lists.example/new-process.txt", "", &out); err != nil {
		t.Fatalf("add after a restarted FTL: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "waiting for FTL to finish reloading") {
		t.Errorf("waited for the dead process's reload:\n%s", out.String())
	}
	// The control: without the new process's line, the same log is a reload
	// in flight that never ends.
	h2, m2 := barrierHost(t)
	appendFTLLog(t, h2, "Received: Real-time signal 0 (34 -> 0)")
	fakeTime(h2, m2)
	_, _, err := m2.AddAdlist(context.Background(), "https://lists.example/control.txt", "", io.Discard)
	if pe := slug(t, err); pe.Slug() != problem.PiholeBusy {
		t.Errorf("control: got %s, want pihole-busy for a reload that never ends", pe.Slug())
	}
}

// CR-7: FTL's one SQLite log callback also reports pihole-FTL.db, so a
// "database is locked" line about it, in the middle of a gravity.db reload,
// is not a failed reload (src/database/common.c).
func TestListReloadBarrierIgnoresALockedLineOfPiholeFTLDB(t *testing.T) {
	h, m := barrierHost(t)
	ran := scriptTime(t, h, m,
		scriptStep{20 * time.Millisecond, func() {
			appendFTLLog(t, h, `SQLite3 message: database is locked in "INSERT INTO queries VALUES (?)" (5)`,
				`SQLite3 message: database is locked in "UPDATE ftl SET value = ? WHERE id = ?;" (5)`)
		}},
		scriptStep{150 * time.Millisecond, endReload(t, h)},
	)
	var out bytes.Buffer
	if _, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "busy-ftl-db.example.com"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "reloading again") || ran() != 2 || n(h) != 1 {
		t.Errorf("a pihole-FTL.db line failed the reload: %d of 2 steps, %d reload-lists\n%s", ran(), n(h), out.String())
	}
}

// n is how many reload-lists the host ran.
func n(h *v5test.FakeHost) int { return countArgv(h.Argvs(), reloadListsArgv) }

// CR-1: a log that cannot be read after the commit is reported as that, not
// as a reload that failed or timed out, and the cause reaches the op log.
func TestListReloadBarrierReportsAnUnreadableLog(t *testing.T) {
	t.Run("after the commit", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().SetReloadDuration(time.Hour) // FTL never logs the completion
		m.Runner = hookRunner{Runner: h, after: func(argv []string) {
			if slices.Equal(argv, reloadListsArgv) {
				if err := os.Remove(h.FTL().LogPath()); err != nil {
					t.Error(err)
				}
			}
		}}
		fakeTime(h, m)
		var out bytes.Buffer
		_, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "nolog.example.com"}, &out)
		pe := slug(t, err)
		if pe.Slug() != problem.PiholeCommandFailed || !strings.Contains(pe.Detail, "could not be read") ||
			strings.Contains(pe.Detail, "within 30 s") || pe.Extensions["rolledBack"] != false {
			t.Fatalf("got %s %+v: %s", pe.Slug(), pe.Extensions, pe.Detail)
		}
		if !strings.Contains(out.String(), "pihole: FTL's log /var/log/pihole/FTL.log could not be read: ") {
			t.Errorf("the op log does not say why:\n%s", out.String())
		}
		if len(h.DomainRows()) != 1 {
			t.Error("the committed row must stay")
		}
	})

	t.Run("while a reload is awaited before a write", func(t *testing.T) {
		h, m := barrierHost(t)
		startHeldReload(t, h)
		defer h.FTL().ReleaseReloads()
		if err := os.Remove(h.FTL().LogPath()); err != nil {
			t.Fatal(err)
		}
		// The log existed when the reload started: a watch needs it readable
		// first, so remove it only once the barrier is waiting.
		appendFTLLog(t, h, "Received: Real-time signal 0 (34 -> 0)")
		fakeTime(h, m)
		sleep := m.Sleep
		first := true
		m.Sleep = func(ctx context.Context, d time.Duration) error {
			if first {
				first = false
				if err := os.Remove(h.FTL().LogPath()); err != nil {
					t.Error(err)
				}
			}
			return sleep(ctx, d)
		}
		var out bytes.Buffer
		_, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "nolog2.example.com"}, &out)
		pe := slug(t, err)
		if pe.Slug() != problem.DependencyUnavailable || !strings.Contains(pe.Detail, "not readable") {
			t.Fatalf("got %s: %s", pe.Slug(), pe.Detail)
		}
		if !strings.Contains(out.String(), "could not be read: ") || len(h.DomainRows()) != 0 {
			t.Errorf("op log:\n%srows %v", out.String(), h.DomainRows())
		}
	})
}

// CR-10: recovery paths of the barrier that no test reached.
func TestListReloadBarrierRecoveryPaths(t *testing.T) {
	add := func(ctx context.Context, m *v5.Mutator, out io.Writer) error {
		_, _, err := m.AddDomain(ctx, spec.DomainAddInput{List: "deny", Kind: "exact", Value: "cr10.example.com"}, out)
		return err
	}

	t.Run("FTL stops while a write waits for a reload", func(t *testing.T) {
		h, m := barrierHost(t)
		startHeldReload(t, h)
		var out syncBuf
		done := make(chan error, 1)
		go func() { done <- add(context.Background(), m, &out) }()
		waitFor(t, "the wait", func() bool { return strings.Contains(out.String(), "waiting for FTL to finish reloading") })
		h.FTL().Stop()
		if err := <-done; err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "pihole: FTL is not running; its lists load when it starts") || len(h.DomainRows()) != 1 {
			t.Errorf("rows %v, log:\n%s", h.DomainRows(), out.String())
		}
	})

	t.Run("FTL stops while the barrier waits for its own reload", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().HoldReloads()
		var out syncBuf
		done := make(chan error, 1)
		go func() { done <- add(context.Background(), m, &out) }()
		waitFor(t, "the reload-lists", func() bool { return n(h) == 1 })
		h.FTL().Stop()
		if err := <-done; err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if !strings.Contains(out.String(), "pihole: FTL is not running; its lists load when it starts") || len(h.DomainRows()) != 1 {
			t.Errorf("rows %v, log:\n%s", h.DomainRows(), out.String())
		}
	})

	t.Run("ctx is cancelled after the commit", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().HoldReloads()
		defer h.FTL().ReleaseReloads()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- add(ctx, m, io.Discard) }()
		waitFor(t, "the reload-lists", func() bool { return n(h) == 1 })
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want it to wrap context.Canceled", err)
		}
		if len(h.DomainRows()) != 1 {
			t.Error("the committed row must stay")
		}
	})

	t.Run("ctx is cancelled while the pre-write probe waits", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().ForceStuckUnlogged() // the probe is never answered
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		err := add(ctx, m, io.Discard)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "probing FTL") {
			t.Fatalf("err %v, want a cancelled probe", err)
		}
		// The probe's 3 tries take 750 ms; a cancelled one must end sooner and
		// must not go on to reload or write.
		if took := time.Since(start); took > 600*time.Millisecond || n(h) != 0 || len(h.DomainRows()) != 0 {
			t.Errorf("took %s, %d reload-lists, rows %v", took, n(h), h.DomainRows())
		}
	})

	t.Run("ctx is cancelled while the probe after the commit waits", func(t *testing.T) {
		h, m := barrierHost(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		scriptTime(t, h, m,
			scriptStep{150 * time.Millisecond, endReload(t, h)},
			scriptStep{150 * time.Millisecond, func() { h.FTL().ForceStuckUnlogged() }}, // the probe is never answered
			scriptStep{150 * time.Millisecond, func() { time.AfterFunc(100*time.Millisecond, cancel) }},
		)
		err := add(ctx, m, io.Discard)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "probing FTL") {
			t.Fatalf("err %v, want a cancelled probe", err)
		}
		if len(h.DomainRows()) != 1 {
			t.Error("the committed row must stay")
		}
	})

	t.Run("the log shows a reload but the probe gets no answer", func(t *testing.T) {
		h, m := barrierHost(t)
		ran := scriptTime(t, h, m,
			scriptStep{150 * time.Millisecond, endReload(t, h)},
			scriptStep{150 * time.Millisecond, func() { h.FTL().ForceStuckUnlogged() }}, // stuck, log shows success
			scriptStep{400 * time.Millisecond, endReload(t, h)},                         // the repair reload
		)
		var out bytes.Buffer
		if err := add(context.Background(), m, &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if ran() != 3 || n(h) != 2 || !strings.Contains(out.String(), "reloading again") || h.FTL().Stuck() {
			t.Errorf("%d of 3 steps, %d reload-lists, stuck %v\n%s", ran(), n(h), h.FTL().Stuck(), out.String())
		}
	})

	for _, how := range []string{"truncated", "renamed"} {
		t.Run("the log is "+how+" before the barrier's first look", func(t *testing.T) {
			h, m := barrierHost(t)
			appendFTLLog(t, h, "some line before the reload") // the watch has a mark to fall below
			m.Reload.Wait = 3 * time.Second
			m.Runner = hookRunner{Runner: h, after: func(argv []string) {
				if !slices.Equal(argv, reloadListsArgv) {
					return
				}
				// The start line went with the old file; FTL's completion
				// line goes to the new one.
				p := h.FTL().LogPath()
				var err error
				if how == "truncated" {
					err = os.Truncate(p, 0)
				} else {
					err = os.Rename(p, p+".1")
				}
				if err != nil {
					t.Error(err)
				}
			}}
			var out bytes.Buffer
			if err := add(context.Background(), m, &out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "pihole: FTL reloaded its lists in ") {
				t.Errorf("log:\n%s", out.String())
			}
		})
	}

	t.Run("a log tail over 256 KiB is read from a whole line", func(t *testing.T) {
		h, m := barrierHost(t)
		const tail = 256 << 10 // the barrier's tail read
		// The file is "<junk without a newline><the cut line><filler>", and the
		// last 256 KiB begin exactly at the cut line, which looks like a start
		// marker: a reader that kept it would see a reload in flight.
		const cut = "[2026-03-14 15:10:00.100 4242M] Received: Real-time signal 0 (34 -> 0)\n"
		const filler = "[2026-03-14 15:10:00.100 4242M] filler line of the log, 128 bytes wide " +
			"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n"
		head := strings.Repeat(filler, 80) + "junk"
		body := cut
		for len(body)+len(filler) <= tail {
			body += filler
		}
		if rem := tail - len(body); rem > 0 {
			body += strings.Repeat("x", rem-1) + "\n"
		}
		if len(body) != tail {
			t.Fatalf("the tail is %d bytes, want %d", len(body), tail)
		}
		if err := os.WriteFile(h.FTL().LogPath(), []byte(head+body), 0o644); err != nil {
			t.Fatal(err)
		}
		fakeTime(h, m)
		var out bytes.Buffer
		if _, _, err := m.AddAdlist(context.Background(), "https://lists.example/long-log.txt", "", &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "waiting for FTL to finish reloading") {
			t.Errorf("read the cut first line as a reload start:\n%s", out.String())
		}
	})

	t.Run("the log is a directory", func(t *testing.T) {
		h, m := barrierHost(t)
		if err := os.Remove(h.FTL().LogPath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(h.FTL().LogPath(), 0o755); err != nil {
			t.Fatal(err)
		}
		pe := slug(t, add(context.Background(), m, io.Discard))
		if pe.Slug() != problem.DependencyUnavailable || len(h.DomainRows()) != 0 {
			t.Errorf("got %s: %s; rows %v", pe.Slug(), pe.Detail, h.DomainRows())
		}
	})

	t.Run("reload-lists fails while FTL is stopped", func(t *testing.T) {
		h, m := barrierHost(t)
		h.FTL().Stop()
		h.FailCommand(reloadListsArgv, 1)
		pe := slug(t, add(context.Background(), m, io.Discard))
		if pe.Slug() != problem.PiholeCommandFailed || pe.Extensions["exitCode"] != 1 || len(h.DomainRows()) != 1 {
			t.Errorf("got %s %+v: %s; rows %v", pe.Slug(), pe.Extensions, pe.Detail, h.DomainRows())
		}
	})
}
