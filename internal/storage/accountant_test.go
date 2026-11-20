// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/storage"
)

// dirs is a state.db file, an ops directory and an audit directory of known
// sizes.
type dirs struct {
	db, ops, audit string
}

func newDirs(t *testing.T) dirs {
	t.Helper()
	root := t.TempDir()
	d := dirs{db: filepath.Join(root, "state.db"), ops: filepath.Join(root, "ops"), audit: filepath.Join(root, "audit")}
	for _, p := range []string{d.ops, d.audit} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func writeN(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeRetention frees files on request and records the order of calls.
type fakeRetention struct {
	calls   *[]string
	outputs []string // Files EvictOutput removes, oldest first
}

func (f *fakeRetention) Prune() error {
	*f.calls = append(*f.calls, "prune")
	return nil
}

func (f *fakeRetention) EvictOutput(excess int64) ([]string, int64, error) {
	*f.calls = append(*f.calls, "evict")
	var ids []string
	var freed int64
	for len(f.outputs) > 0 && freed < excess {
		p := f.outputs[0]
		f.outputs = f.outputs[1:]
		fi, err := os.Stat(p)
		if err != nil {
			return ids, freed, err
		}
		if err := os.Remove(p); err != nil {
			return ids, freed, err
		}
		ids = append(ids, filepath.Base(p))
		freed += fi.Size()
	}
	return ids, freed, nil
}

type fakeAudit struct {
	calls *[]string
	old   []string // Files Prune removes
}

func (f *fakeAudit) Prune() error {
	*f.calls = append(*f.calls, "audit")
	for _, p := range f.old {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newAccountant(t *testing.T, d dirs, budget int64, r storage.Retention, a storage.AuditPruner) *storage.Accountant {
	t.Helper()
	acct, err := storage.New(storage.Options{BudgetBytes: budget, StateDB: d.db, OpsDir: d.ops, AuditDir: d.audit,
		Retention: r, Audit: a, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

// The total covers state.db, the ops directory and the audit directory.
func TestAccountMeasuresTheThreeLocations(t *testing.T) {
	d := newDirs(t)
	writeN(t, d.db, 1000)
	writeN(t, filepath.Join(d.ops, "a", "output", "000001.seg"), 200)
	writeN(t, filepath.Join(d.ops, "a", "exit.json"), 30)
	writeN(t, filepath.Join(d.audit, "audit.jsonl"), 70)
	var calls []string
	r := newAccountant(t, d, 1<<20, &fakeRetention{calls: &calls}, &fakeAudit{calls: &calls}).Account()
	if r.StateBytes != 1000 || r.OpsBytes != 230 || r.AuditBytes != 70 || r.Total != 1300 || r.Budget != 1<<20 || r.Limited {
		t.Fatalf("report %+v", r)
	}
	if !slices.Equal(calls, []string{"prune"}) {
		t.Fatalf("calls %v: within budget, only retention runs", calls)
	}
}

// Over budget: retention, then output eviction, then expired audit, then
// the flag; each later step runs only while still over budget, and the flag
// clears once usage falls.
func TestAccountOrderAndFlag(t *testing.T) {
	d := newDirs(t)
	writeN(t, d.db, 100)
	out1, out2 := filepath.Join(d.ops, "1"), filepath.Join(d.ops, "2")
	writeN(t, out1, 300)
	writeN(t, out2, 300)
	old := filepath.Join(d.audit, "old.gz")
	writeN(t, old, 500)
	young := filepath.Join(d.audit, "young.gz")
	writeN(t, young, 400)

	cases := []struct {
		budget      int64
		wantCalls   []string
		wantEvicted []string
		limited     bool
	}{
		// 1600 used. 1400: one output suffices.
		{1400, []string{"prune", "evict"}, []string{"1"}, false},
		// 1300 used. 600: the last output and the old audit file.
		{600, []string{"prune", "evict", "audit"}, []string{"2"}, false},
		// 500 used. 100: nothing more may go.
		{100, []string{"prune", "evict", "audit"}, nil, true},
	}
	ret := &fakeRetention{outputs: []string{out1, out2}}
	aud := &fakeAudit{old: []string{old}}
	for i, c := range cases {
		var calls []string
		ret.calls, aud.calls = &calls, &calls
		a := newAccountant(t, d, c.budget, ret, aud)
		r := a.Account()
		if !slices.Equal(calls, c.wantCalls) || !slices.Equal(r.Evicted, c.wantEvicted) || r.Limited != c.limited || a.Limited() != c.limited {
			t.Fatalf("case %d: calls %v, report %+v", i, calls, r)
		}
		if _, err := os.Stat(young); err != nil {
			t.Fatalf("case %d: young audit file gone: %v", i, err)
		}
		if c.limited {
			p := a.Check()
			if p == nil || p.Slug() != problem.StorageLimitReached || p.Status != 507 {
				t.Fatalf("Check at the limit = %+v", p)
			}
			// Space freed elsewhere: the next pass clears the flag.
			if err := os.Remove(young); err != nil {
				t.Fatal(err)
			}
			if r := a.Account(); r.Limited || a.Check() != nil {
				t.Fatalf("after freeing space: %+v", r)
			}
		} else if a.Check() != nil {
			t.Fatalf("case %d: Check refused below the limit", i)
		}
	}
}

// TC-SM-48: the staging directory of uploads is measured into the total and
// counts toward the limit flag, but no pass evicts from it: a staged upload
// expires by itself.
func TestTCSM48_StagingBytesCountTowardTheStorage(t *testing.T) {
	const mib = 1 << 20
	d := newDirs(t)
	staging := filepath.Join(t.TempDir(), "staging")
	staged := []string{filepath.Join(staging, "u1", "upload.tar.gz"), filepath.Join(staging, "u2", "upload.tar.gz")}
	writeN(t, staged[0], 2*mib)
	writeN(t, staged[1], 1*mib)
	writeN(t, filepath.Join(d.ops, "a", "exit.json"), 2*mib)
	var calls []string
	acct, err := storage.New(storage.Options{BudgetBytes: 4 * mib, StateDB: d.db, OpsDir: d.ops, AuditDir: d.audit,
		StagingDir: staging, Retention: &fakeRetention{calls: &calls}, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	r := acct.Account()
	if r.StagingBytes != 3*mib || r.OpsBytes != 2*mib || r.Total != 5*mib {
		t.Fatalf("report %+v: want 3 MiB staged within a 5 MiB total", r)
	}
	if !r.Limited || !acct.Limited() {
		t.Fatalf("report %+v: 5 MiB over a 4 MiB budget must set the limit flag", r)
	}
	if !slices.Equal(calls, []string{"prune", "evict"}) {
		t.Fatalf("calls %v: over budget, the pass must try eviction", calls)
	}
	for _, p := range staged {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("eviction touched staging: %v", err)
		}
	}
}

// A pass that cannot measure keeps the flag as it was.
func TestAccountKeepsTheFlagWhenItCannotMeasure(t *testing.T) {
	d := newDirs(t)
	writeN(t, d.db, 100)
	var calls []string
	a := newAccountant(t, d, 10, &fakeRetention{calls: &calls}, nil)
	if !a.Account().Limited {
		t.Fatal("over budget, flag clear")
	}
	if err := os.Chmod(d.audit, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d.audit, 0o700) })
	writeN(t, filepath.Join(d.ops, "x"), 0)
	if err := os.Remove(d.db); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory")
	}
	if !a.Account().Limited {
		t.Fatal("an unmeasurable pass cleared the flag")
	}
}

// Triggers merge into one pending pass and never block.
func TestTriggerCoalesces(t *testing.T) {
	var calls []string
	a := newAccountant(t, newDirs(t), 1, &fakeRetention{calls: &calls}, nil)
	for range 5 {
		a.Trigger()
	}
	n := 0
	for {
		select {
		case <-a.Pending():
			n++
			continue
		default:
		}
		break
	}
	if n != 1 {
		t.Fatalf("%d pending passes, want 1", n)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := storage.New(storage.Options{BudgetBytes: 0, Retention: &fakeRetention{}}); err == nil {
		t.Fatal("zero budget accepted")
	}
	if _, err := storage.New(storage.Options{BudgetBytes: 1}); err == nil {
		t.Fatal("no retention accepted")
	}
}
