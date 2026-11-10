// Package store is the durable state of nexusd: operation records,
// idempotency keys and small key-value data, in one bbolt file, state.db.
//
// Every commit is fsynced by bbolt before it returns, so a record the store
// reported as written survives a crash of the process (FR-6). The file is
// locked exclusively while open, so a second daemon on the same directory
// fails fast with ErrLocked instead of corrupting it.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/mauktik/nexus/internal/opid"
)

// FileName is the database file inside the state directory.
const FileName = "state.db"

// FileMode is the permission of state.db (NFR-4).
const FileMode os.FileMode = 0o600

// LockTimeout is how long Open waits for another process to release the file
// lock before it returns ErrLocked.
const LockTimeout = 5 * time.Second

// bbolt polls the lock every 50 ms and gives up once the elapsed time exceeds
// its Timeout minus that interval (go.etcd.io/bbolt@v1.5.0 bolt_unix.go:40).
// The slack makes the real wait at least LockTimeout.
const lockPollSlack = 50 * time.Millisecond

var (
	bucketOps  = []byte("ops")
	bucketIdem = []byte("idem")
	bucketKV   = []byte("kv")
)

// ErrLocked means another process holds state.db open.
var ErrLocked = errors.New("state database is locked by another process")

// ErrNotFound means no operation record has the requested ID.
var ErrNotFound = errors.New("operation not found")

// Options configures a Store.
type Options struct {
	Now func() time.Time // Nil means time.Now. Drives Created, Started, Ended and idempotency expiry
}

// Store is the open state database. It is safe for concurrent use.
type Store struct {
	db   *bolt.DB
	now  func() time.Time
	path string
}

// Open opens or creates state.db in dir with mode 0600, waiting at most
// LockTimeout for another process to release it.
func Open(dir string, opts Options) (*Store, error) {
	path := filepath.Join(dir, FileName)
	db, err := bolt.Open(path, FileMode, &bolt.Options{Timeout: LockTimeout + lockPollSlack})
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("open %s: %w (waited %s; another nexusd may be running)", path, ErrLocked, LockTimeout)
		}
		return nil, fmt.Errorf("open %s: %v", path, err)
	}
	// The create mode only applies to a new file; an existing one is
	// tightened too.
	if err := os.Chmod(path, FileMode); err != nil {
		return nil, closeAfter(db, fmt.Errorf("chmod %s: %v", path, err))
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketOps, bucketIdem, bucketKV} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("create bucket %s: %v", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, closeAfter(db, fmt.Errorf("initialize %s: %v", path, err))
	}
	// Operation IDs are scan keys: seed the generator past the largest one on
	// disk, so a clock that stepped back across a restart cannot order new
	// operations before old ones (FIFO restore, List cursors).
	err = db.View(func(tx *bolt.Tx) error {
		if k, _ := tx.Bucket(bucketOps).Cursor().Last(); k != nil {
			opid.Observe(string(k))
		}
		return nil
	})
	if err != nil {
		return nil, closeAfter(db, fmt.Errorf("read %s: %v", path, err))
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now, path: path}, nil
}

func closeAfter(db *bolt.DB, err error) error {
	if cerr := db.Close(); cerr != nil {
		return fmt.Errorf("%w (close: %v)", err, cerr)
	}
	return err
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Close releases the file and its lock.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close %s: %v", s.path, err)
	}
	return nil
}

func (s *Store) nowUTC() time.Time { return s.now().UTC() }
