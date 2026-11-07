// Package storage keeps nexusd within its storage budget (NFR-10): it
// measures state.db, the ops directory and the audit directory, applies
// retention, frees space in a fixed order when over budget, and holds the
// limit flag the Dispatcher checks before admitting a new mutation (Data
// Flow step 5).
//
// The order when over budget is part of the contract: operation output is
// evicted oldest first, keeping the records; then audit files older than
// their retention are deleted; only then is the limit flag set. Audit
// younger than retention is never deleted, and the output of a Queued or
// Running operation is never touched. Reads, idempotent replays, read-risk
// requests, whatIf and cancel continue at the limit (A-68).
package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// Interval is how often accounting runs besides at startup and after each
// terminal operation.
const Interval = time.Hour

// Retention is what the accountant needs of operation retention.
// *ops.Retention implements it.
type Retention interface {
	// Prune removes expired records with their directories, and expired
	// idempotency keys.
	Prune() error
	// EvictOutput removes terminal operations' output, oldest first, until
	// excess bytes are freed, and returns the operations and bytes freed.
	EvictOutput(excess int64) ([]string, int64, error)
}

// AuditPruner deletes audit files older than their retention. *audit.Log
// implements it.
type AuditPruner interface {
	Prune() error
}

// Options configures New. There is no second constructor.
type Options struct {
	BudgetBytes int64 // limits.storageBudgetBytes; must be positive
	// StateDB is the state.db file, OpsDir the ops directory and AuditDir
	// the audit directory: together, what is measured.
	StateDB, OpsDir, AuditDir string
	Retention                 Retention   // Required
	Audit                     AuditPruner // Nil means audit files are never deleted here
	Now                       func() time.Time
	Logger                    *slog.Logger // Nil means slog.Default()
	// Observe, when non-nil, receives the report of every pass.
	Observe func(Report)
}

// Report is the outcome of one accounting pass.
type Report struct {
	At     time.Time
	Budget int64
	// Usage in bytes after the pass.
	StateBytes, OpsBytes, AuditBytes, Total int64
	Evicted                                 []string // Operations whose output this pass evicted
	Limited                                 bool     // The limit flag after the pass
}

// Accountant runs accounting passes and holds the limit flag. It is safe
// for concurrent use; passes are serialized. It starts no goroutine: the
// daemon runs a pass at startup, on each Interval, and when Pending
// delivers, from its own loop (NFR-1).
type Accountant struct {
	budget    int64
	stateDB   string
	opsDir    string
	auditDir  string
	retention Retention
	audit     AuditPruner
	now       func() time.Time
	log       *slog.Logger
	observe   func(Report)

	limited atomic.Bool
	pending chan struct{} // One slot: triggers coalesce
	mu      sync.Mutex    // Serializes passes
}

// New returns an accountant with the limit flag clear.
func New(opts Options) (*Accountant, error) {
	if opts.BudgetBytes <= 0 {
		return nil, fmt.Errorf("storage: budget must be positive, got %d", opts.BudgetBytes)
	}
	if opts.Retention == nil {
		return nil, errors.New("storage: no retention")
	}
	a := &Accountant{
		budget: opts.BudgetBytes, stateDB: opts.StateDB, opsDir: opts.OpsDir, auditDir: opts.AuditDir,
		retention: opts.Retention, audit: opts.Audit, now: opts.Now, log: opts.Logger, observe: opts.Observe,
		pending: make(chan struct{}, 1),
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a, nil
}

// Budget is the storage budget in bytes.
func (a *Accountant) Budget() int64 { return a.budget }

// Limited reports whether the limit flag is set.
func (a *Accountant) Limited() bool { return a.limited.Load() }

// Check is the Dispatcher's storage hook (Data Flow step 5): while the limit
// flag is set, a new mutation is refused with storage-limit-reached.
func (a *Accountant) Check() *problem.Error {
	if !a.limited.Load() {
		return nil
	}
	return problem.New(problem.StorageLimitReached,
		fmt.Sprintf("the storage budget of %d bytes is used up; new operations are refused until space is freed", a.budget))
}

// Trigger asks for a pass without waiting for it: a request made while one
// is already pending is merged into it. It never blocks, so a terminal
// commit can call it.
func (a *Accountant) Trigger() {
	select {
	case a.pending <- struct{}{}:
	default:
	}
}

// Pending delivers once per merged Trigger; the receiver then runs Account.
func (a *Accountant) Pending() <-chan struct{} { return a.pending }

// Account runs one pass: retention, then measurement, then, while over
// budget, output eviction and the deletion of expired audit files, and
// finally the limit flag, set when still over budget and cleared otherwise.
// A failed step is logged and the pass goes on with what it can measure.
func (a *Accountant) Account() Report {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.retention.Prune(); err != nil {
		a.log.Error("retention failed", "error", err.Error())
	}
	r, ok := a.measure()
	if ok && r.Total > a.budget {
		evicted, _, err := a.retention.EvictOutput(r.Total - a.budget)
		if err != nil {
			a.log.Error("output eviction failed", "error", err.Error())
		}
		r, ok = a.measure()
		r.Evicted = evicted
	}
	if ok && r.Total > a.budget && a.audit != nil {
		if err := a.audit.Prune(); err != nil {
			a.log.Error("audit retention failed", "error", err.Error())
		}
		evicted := r.Evicted
		r, ok = a.measure()
		r.Evicted = evicted
	}
	if ok {
		limited := r.Total > a.budget
		switch was := a.limited.Swap(limited); {
		case limited && !was:
			a.log.Warn("storage limit reached; new operations are refused", "usedBytes", r.Total, "budgetBytes", a.budget)
		case !limited && was:
			a.log.Info("storage back within budget; new operations are accepted", "usedBytes", r.Total, "budgetBytes", a.budget)
		}
	}
	// A pass that cannot measure keeps the flag as it was.
	r.Limited = a.limited.Load()
	if a.observe != nil {
		a.observe(r)
	}
	return r
}

// measure sizes state.db, the ops directory and the audit directory. ok is
// false when any of them cannot be read, which is logged.
func (a *Accountant) measure() (Report, bool) {
	r := Report{At: a.now(), Budget: a.budget}
	var errs []error
	var err error
	if r.StateBytes, err = fileSize(a.stateDB); err != nil {
		errs = append(errs, err)
	}
	if r.OpsBytes, err = DirSize(a.opsDir); err != nil {
		errs = append(errs, err)
	}
	if r.AuditBytes, err = DirSize(a.auditDir); err != nil {
		errs = append(errs, err)
	}
	r.Total = r.StateBytes + r.OpsBytes + r.AuditBytes
	if err := errors.Join(errs...); err != nil {
		a.log.Error("storage accounting cannot measure usage", "error", err.Error())
		return r, false
	}
	return r, true
}

func fileSize(path string) (int64, error) {
	if path == "" {
		return 0, nil
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", path, err)
	}
	return fi.Size(), nil
}

// treeSize is the total size of the regular files under root; a missing
// root, or a file removed during the walk, counts 0.
func DirSize(root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		n += fi.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", root, err)
	}
	return n, nil
}
