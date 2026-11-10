package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/storage"
	"github.com/mauktik/nexus/internal/store"
)

// Retention of operation records (FR-4): a terminal record is removed once
// it ended more than RetentionAge ago and is not among the newest
// RetentionKeep records, so whichever rule keeps more wins.
const (
	RetentionAge  = 30 * 24 * time.Hour
	RetentionKeep = 1000
)

// RetentionStore is what retention needs of internal/store. *store.Store
// implements it.
type RetentionStore interface {
	Get(id string) (store.OpRecord, error)
	PruneTerminal(olderThan time.Time, keepNewest int) ([]string, error)
	SetOutputEvicted(id string) error
	PurgeExpiredIdempotency() (int, error)
}

// RetentionOptions configures NewRetention.
type RetentionOptions struct {
	Store RetentionStore // Required
	// Dir is the ops directory, which holds ops/<id> per operation.
	// Required.
	Dir    string
	Now    func() time.Time // Retention cutoff; nil means time.Now
	Logger *slog.Logger     // Nil means slog.Default()
}

// Retention removes expired operation records with their directories and
// expired idempotency keys, and evicts the output of terminal operations
// when the storage budget is exceeded (NFR-10). It never touches a Queued or
// Running operation, its record or its output.
type Retention struct {
	store RetentionStore
	dir   string
	now   func() time.Time
	log   *slog.Logger
}

// NewRetention returns the retention of the operations in opts.Dir.
func NewRetention(opts RetentionOptions) (*Retention, error) {
	if opts.Store == nil {
		return nil, errors.New("ops retention: no store")
	}
	if opts.Dir == "" {
		return nil, errors.New("ops retention: no ops directory")
	}
	r := &Retention{store: opts.Store, dir: opts.Dir, now: opts.Now, log: opts.Logger}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// Prune deletes the terminal records retention no longer keeps, then their
// ops directories, then the expired idempotency keys. A directory that
// cannot be removed is reported but does not stop the rest.
func (r *Retention) Prune() error {
	ids, err := r.store.PruneTerminal(r.now().Add(-RetentionAge), RetentionKeep)
	if err != nil {
		return fmt.Errorf("prune operation records: %w", err)
	}
	var errs []error
	for _, id := range ids {
		dir, ok := r.opDir(id)
		if !ok {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove the directory of operation %s: %w", id, err))
		}
	}
	errs = append(errs, r.sweepOrphans()...)
	purged, err := r.store.PurgeExpiredIdempotency()
	if err != nil {
		errs = append(errs, fmt.Errorf("purge idempotency keys: %w", err))
	}
	if len(ids) > 0 || purged > 0 {
		r.log.Info("retention removed expired state", "operations", len(ids), "idempotencyKeys", purged)
	}
	return errors.Join(errs...)
}

// sweepOrphans removes the ops directories that no record owns: records go
// first and directories second, so a failed removal or a stop between the
// two leaves a directory no later pass would name. This is safe because a
// record is always created before its directory. A directory whose lookup
// fails for any other reason is left alone.
func (r *Retention) sweepOrphans() []error {
	entries, err := os.ReadDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []error{fmt.Errorf("sweep operation directories: %w", err)}
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !isOpID(e.Name()) {
			continue
		}
		if _, err := r.store.Get(e.Name()); !errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.dir, e.Name())); err != nil {
			errs = append(errs, fmt.Errorf("remove the orphaned directory of operation %s: %w", e.Name(), err))
			continue
		}
		r.log.Warn("retention removed the directory of an operation with no record", "operationId", e.Name())
	}
	return errs
}

// EvictOutput removes the output of terminal operations, oldest first,
// until at least excess bytes are freed or none is left. Each record is
// kept and marked outputEvicted before its output goes, so a crash between
// the two leaves a marked record whose output the next pass removes. It
// returns the IDs whose output it removed and the bytes freed.
func (r *Retention) EvictOutput(excess int64) ([]string, int64, error) {
	entries, err := os.ReadDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("evict operation output: %w", err)
	}
	// Operation IDs are UUIDv7, so name order is creation order.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && isOpID(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	var (
		evicted []string
		freed   int64
		errs    []error
	)
	for _, id := range names {
		if freed >= excess {
			break
		}
		out := filepath.Join(r.dir, id, "output")
		size, err := storage.DirSize(out)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if size == 0 {
			continue
		}
		rec, err := r.store.Get(id)
		if errors.Is(err, store.ErrNotFound) {
			// No record: not an operation this store knows, so not ours to
			// evict.
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !rec.Status.Terminal() {
			continue
		}
		if err := r.store.SetOutputEvicted(id); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(out); err != nil {
			errs = append(errs, fmt.Errorf("remove the output of operation %s: %w", id, err))
			continue
		}
		evicted = append(evicted, id)
		freed += size
	}
	if len(evicted) > 0 {
		r.log.Warn("operation output evicted to stay within the storage budget", "operations", len(evicted), "bytes", freed)
	}
	return evicted, freed, errors.Join(errs...)
}

// opDir is ops/<id>, for a well-formed ID only, so a stored ID can never
// name a path outside the ops directory.
func (r *Retention) opDir(id string) (string, bool) {
	if !isOpID(id) {
		r.log.Error("retention skipped a malformed operation ID", "operationId", id)
		return "", false
	}
	return filepath.Join(r.dir, id), true
}

// opIDRE is the form of an operation ID (internal/opid): a lowercase UUID.
var opIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isOpID(s string) bool { return opIDRE.MatchString(s) }

// OnTerminal returns s with notify called after each committed terminal
// transition: every CommitTerminal, and each CompareAndSetStatus to a
// terminal status (a cancel of a queued operation). Storage accounting
// runs after each terminal operation through it (NFR-10). notify must not
// block.
func OnTerminal(s Store, notify func()) Store {
	return terminalNotifier{Store: s, notify: notify}
}

type terminalNotifier struct {
	Store
	notify func()
}

func (n terminalNotifier) CompareAndSetStatus(id string, from, to apitypes.OperationStatus) (store.OpRecord, error) {
	rec, err := n.Store.CompareAndSetStatus(id, from, to)
	if err == nil && to.Terminal() {
		n.notify()
	}
	return rec, err
}

func (n terminalNotifier) CommitTerminal(id string, from, to apitypes.OperationStatus, result json.RawMessage, perr *problem.Error) (store.OpRecord, error) {
	rec, err := n.Store.CommitTerminal(id, from, to, result, perr)
	if err == nil {
		n.notify()
	}
	return rec, err
}
