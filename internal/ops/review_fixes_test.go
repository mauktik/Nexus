package ops_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/store"
)

// spyWorldWith is a spyWorld whose manager options are also adjusted by
// tune, and whose launcher seams by seams.
func spyWorldWith(t *testing.T, spy *unitSpy, tune func(*ops.Options), seams func(*detach.Options)) *world {
	var opsDir atomic.Value
	return newWorld(t, func(o *ops.Options) {
		opsDir.Store(o.Dir)
		if tune != nil {
			tune(o)
		}
		d := spy.options(func() string { return opsDir.Load().(string) })
		d.ListUnits = func(context.Context) ([]string, error) { return nil, nil }
		if seams != nil {
			seams(&d)
		}
		withDetach(t, d)(o)
	})
}

// CR-3: limits.outputCapBytes reaches the spec a detached operation's
// supervisor opens its output log with.
func TestDetachedOperationCarriesConfiguredOutputCap(t *testing.T) {
	spy := newUnitSpy()
	got := make(chan int64, 1)
	base := spy.onLaunch
	spy.onLaunch = func(inv detach.Invocation) error {
		b, err := os.ReadFile(filepath.Join(inv.Dir, detach.SpecFile))
		if err != nil {
			return err
		}
		var f struct {
			OutputCapBytes int64 `json:"outputCapBytes"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		got <- f.OutputCapBytes
		return base(inv)
	}
	w := spyWorldWith(t, spy, func(o *ops.Options) { o.OutputCapBytes = 4096 }, nil)
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"cap"}`, "")
	if c := <-got; c != 4096 {
		t.Fatalf("spec.json outputCapBytes = %d, want the configured 4096", c)
	}
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
}

// casHook runs after returns from CompareAndSetStatus to Running.
type casHook struct {
	ops.Store
	after func(id string)
}

func (c *casHook) CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (store.OpRecord, error) {
	rec, err := c.Store.CompareAndSetStatus(id, from, to)
	if err == nil && to == apitypes.StatusRunning {
		c.after(id)
	}
	return rec, err
}

// CR-17: when the max-runtime file cannot be written nothing is launched,
// because a reattached operation would have no backstop.
func TestDetachedLaunchRefusedWhenMaxRuntimeCannotBeRecorded(t *testing.T) {
	spy := newUnitSpy()
	var opsDir string
	w := spyWorldWith(t, spy, func(o *ops.Options) {
		opsDir = o.Dir
		o.Store = &casHook{Store: o.Store, after: func(id string) {
			// A non-empty directory in the file's place makes the rename fail
			// while the rest of the operation directory stays writable.
			if err := os.MkdirAll(filepath.Join(opsDir, id, "max-runtime", "x"), 0o700); err != nil {
				t.Error(err)
			}
		}}
	}, nil)
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"nofile"}`, "")
	rec := w.waitStatus(op.ID, apitypes.StatusFailed)
	if rec.Error == nil || rec.Error.Slug() != problem.InternalError {
		t.Fatalf("error %+v, want internal-error", rec.Error)
	}
	if n := spy.launched(op.ID); n != 0 {
		t.Fatalf("%d launches, want none", n)
	}
}

// CR-17: a reattached operation with no max-runtime file still has a
// MaxRuntime backstop, so a unit that hangs while active cannot hold its
// lane forever.
func TestReattachedWithoutMaxRuntimeFileStillHasABackstop(t *testing.T) {
	var shift atomic.Int64
	spy := newUnitSpy() // UnitState: always active
	w := spyWorldWith(t, spy, nil, func(d *detach.Options) {
		d.Now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	})
	id := w.seed(tdRec("hang", store.Lane{}), apitypes.StatusRunning)
	if err := os.MkdirAll(w.opDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, w.recoverNow(), 15*time.Second, "classification")
	if rec, err := w.store.Get(id); err != nil || rec.Status != apitypes.StatusRunning {
		t.Fatalf("hang: %+v %v, want Running (reattached)", rec, err)
	}
	shift.Store(int64(48 * time.Hour))
	rec := w.waitStatus(id, apitypes.StatusFailed)
	wantOutcomeUnknown(t, rec, "maximum runtime")
}

// CR-18: a UnitState error or timeout during classification is unknown,
// never absent: the operation is reattached, the reason is logged, and no
// lost re-check starts from the error.
func TestRecoveryUnitStateErrorIsUnknownNotAbsent(t *testing.T) {
	spy := newUnitSpy()
	var calls atomic.Int64
	spy.state = func(context.Context, string) (detach.UnitState, error) {
		switch calls.Add(1) {
		case 1:
			return detach.UnitState{}, errors.New("systemctl: connection refused")
		case 2:
			return detach.UnitState{}, context.DeadlineExceeded
		}
		return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
	}
	w := spyWorldWith(t, spy, nil, nil)
	id := w.seed(tdRec("flaky", store.Lane{}), apitypes.StatusRunning)
	if err := os.MkdirAll(w.opDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, w.recoverNow(), 20*time.Second, "classification")
	rec, err := w.store.Get(id)
	if err != nil || rec.Status != apitypes.StatusRunning {
		t.Fatalf("flaky: %+v %v, want Running (reattached)", rec, err)
	}
	logs := w.logs.String()
	for _, want := range []string{`"reason":"error"`, `"reason":"timeout"`, "operation reattached"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %s:\n%s", want, logs)
		}
	}
	w.writeExit(id, detach.Exit{Started: time.Now(), Ended: time.Now()})
	w.waitStatus(id, apitypes.StatusSucceeded)
}

// CR-21: spec.json, which can hold sensitive values, is removed when a
// detached operation ends, even if the supervisor never read it.
func TestSpecJSONIsRemovedWhenTheOperationEnds(t *testing.T) {
	spy := newUnitSpy() // Its launches finish at once without reading spec.json
	w := spyWorldWith(t, spy, nil, nil)
	op := w.start(who("ops", "td:write"), tdOps.job.Op(), `{"name":"leftover"}`, "")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	if _, err := os.Stat(filepath.Join(w.opDir(op.ID), detach.SpecFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spec.json after the end: %v, want it removed", err)
	}
}
