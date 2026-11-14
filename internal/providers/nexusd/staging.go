package nexusd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// Staging layout under <StateDir>: staging/<uploadId>/release.tar.gz, the
// body, and staging/<uploadId>/meta.json, what was received (FR-6).
const (
	stagingDirName  = "staging"
	stagedFileName  = "release.tar.gz"
	stagedMetaName  = "meta.json"
	partialSuffix   = ".partial"
	maxStaged       = 2         // Uploads staged or being received at once (A-9)
	stagingTTL      = time.Hour // How long a staged upload is kept (A-9)
	stagingBufSize  = 32 << 10  // The copy buffer of a body (NFR-4)
	maxStagedMeta   = 4 << 10   // What sweep reads of a meta.json
	uploadIDByteLen = 16
)

var uploadIDRE = regexp.MustCompile(spec.UploadIDPattern)

// stagedMeta is meta.json.
type stagedMeta struct {
	UploadID  string    `json:"uploadId"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"sizeBytes"`
	Signature string    `json:"signature"`
	Received  time.Time `json:"received"`
	Expires   time.Time `json:"expires"`
}

// stagedUpload is a stored upload: one whose body and meta.json are on disk.
type stagedUpload struct {
	meta stagedMeta
	dir  string
	stop func() bool // Stops the expiry timer
}

// stagingHooks are test-only fault and observation points; production
// leaves them nil.
type stagingHooks struct {
	// wrapBody, when set, wraps the body receive reads.
	wrapBody func(io.Reader) io.Reader
	// afterRename, when set, runs once release.tar.gz is in place and before
	// meta.json is written; an error fails the receive there.
	afterRename func() error
}

// staging holds uploaded release tarballs until an update uses them or they
// expire an hour after they were received (FR-6, A-9). At most maxStaged
// are stored at once, and in flight and stored together they stay under
// maxStaged, except for one probe upload that is received while every slot
// holds a stored upload, so a retry can be recognised by its digest (A-26):
// at most maxStaged stored plus one in flight. One mutex guards both the
// anonymous in-flight count and the map of stored uploads: a reservation
// carries no upload ID, and receive generates one (A-51).
type staging struct {
	dir       string
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	mu       sync.Mutex
	inflight int
	stored   map[string]*stagedUpload
	hooks    stagingHooks
}

func newStaging(dir string, now func() time.Time, afterFunc func(time.Duration, func()) func() bool) *staging {
	return &staging{dir: dir, now: now, afterFunc: afterFunc, stored: map[string]*stagedUpload{}}
}

// open creates the staging directory 0700 and sweeps it.
func (s *staging) open() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	return s.sweep()
}

// reserve admits an upload about to be received, before its body is read
// (A-26). It is refused when another upload is already in flight and the
// in-flight and stored counts together reach maxStaged. With nothing in
// flight it is admitted even when maxStaged uploads are stored: that one
// probe is received so that a retry of a stored upload is recognised by its
// digest, and receive refuses it when it matches none. The release it
// returns frees the slot; calls after the first do nothing.
func (s *staging) reserve() (release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight > 0 && s.inflight+len(s.stored) >= maxStaged {
		return nil, s.fullLocked()
	}
	s.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inflight--
		})
	}, nil
}

// fullLocked is the conflict a reservation meets when every slot is taken.
// The detail names when the earliest stored upload expires (A-47); conflict
// declares no extension for it. When every slot is a body still being
// received, nothing has an expiry to name. s.mu is held.
func (s *staging) fullLocked() *problem.Error {
	if s.inflight >= maxStaged {
		return problem.New(problem.Conflict, fmt.Sprintf("%d uploads are still receiving, the most allowed at "+
			"once. Retry once one of them has finished.", maxStaged))
	}
	return s.storedFullLocked()
}

// storedFullLocked is the conflict for a body that matches no stored upload
// while maxStaged are stored. s.mu is held.
func (s *staging) storedFullLocked() *problem.Error {
	var earliest time.Time
	for _, u := range s.stored {
		if earliest.IsZero() || u.meta.Expires.Before(earliest) {
			earliest = u.meta.Expires
		}
	}
	return problem.New(problem.Conflict, fmt.Sprintf("%d uploads are already staged, the most allowed; the "+
		"earliest expires at %s. Retry after that.", maxStaged, earliest.UTC().Format(time.RFC3339)))
}

// settleLocked decides what a received body with digest and signature
// becomes: an unexpired stored upload with the same digest and signature is
// returned as dup, whatever the count (A-26); otherwise the body is refused
// with conflict when maxStaged uploads are stored. s.mu is held.
func (s *staging) settleLocked(digest, signature string) (existing stagedMeta, dup bool, err error) {
	for _, u := range s.stored {
		if u.meta.SHA256 == digest && u.meta.Signature == signature && s.now().Before(u.meta.Expires) {
			return u.meta, true, nil
		}
	}
	if len(s.stored) >= maxStaged {
		return stagedMeta{}, false, s.storedFullLocked()
	}
	return stagedMeta{}, false, nil
}

// newUploadID returns 128 random bits as lowercase hex.
func newUploadID() (string, error) {
	var b [uploadIDByteLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate an upload ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// receive streams body into a new staging/<id>/ under a server-generated
// ID: through a 32 KiB buffer and a SHA-256 tee into release.tar.gz.partial
// (0600, O_EXCL), then fsync, rename to release.tar.gz, chmod 0400 and
// meta.json (F4). On success the upload is stored, with its expiry armed,
// before receive returns. When a stored upload has the same digest and
// signature, the new copy is discarded and that upload is returned, even
// with maxStaged stored; a body that matches none is refused with conflict
// when maxStaged are stored (A-26). Any failure removes the directory.
// receive never touches the in-flight count: the reservation's release does.
func (s *staging) receive(ctx context.Context, signature string, body io.Reader) (_ stagedMeta, err error) {
	id, err := newUploadID()
	if err != nil {
		return stagedMeta{}, err
	}
	dir := filepath.Join(s.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return stagedMeta{}, fmt.Errorf("create the upload directory: %w", err)
	}
	defer func() {
		if err != nil {
			if rerr := os.RemoveAll(dir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("remove the failed upload: %w", rerr))
			}
		}
	}()
	s.mu.Lock()
	hooks := s.hooks
	s.mu.Unlock()
	if hooks.wrapBody != nil {
		body = hooks.wrapBody(body)
	}
	digest, size, err := writeBody(ctx, filepath.Join(dir, stagedFileName), body)
	if err != nil {
		return stagedMeta{}, err
	}
	if hooks.afterRename != nil {
		if err := hooks.afterRename(); err != nil {
			return stagedMeta{}, err
		}
	}
	// Settled once before meta.json is written, so a refused body never
	// leaves a valid-looking upload for a restart's sweep, and again at
	// the insert, which is the atomic decision.
	s.mu.Lock()
	existing, dup, err := s.settleLocked(digest, signature)
	s.mu.Unlock()
	if err != nil {
		return stagedMeta{}, err
	}
	if dup {
		discardDuplicate(dir)
		return existing, nil
	}
	received := s.now().UTC().Truncate(time.Second)
	meta := stagedMeta{UploadID: id, SHA256: digest, SizeBytes: size, Signature: signature, Received: received,
		Expires: received.Add(stagingTTL)}
	if err := writeJSONAtomic(dir, stagedMetaName, meta, nil); err != nil {
		return stagedMeta{}, err
	}

	s.mu.Lock()
	existing, dup, err = s.settleLocked(digest, signature)
	if err == nil && !dup {
		s.insertLocked(meta, dir)
	}
	s.mu.Unlock()
	if err != nil {
		return stagedMeta{}, err
	}
	if dup {
		discardDuplicate(dir)
		return existing, nil
	}
	return meta, nil
}

// discardDuplicate removes the directory of a body that matched a stored
// upload. The stored upload stands; a failed removal is only clutter that the
// next sweep removes.
func discardDuplicate(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("nexusd: cannot remove a duplicate upload", "dir", dir, "error", err.Error())
	}
}

// writeBody streams body into path+".partial" and renames it to path, mode
// 0400. It returns the lowercase hex SHA-256 of the body and its size.
func writeBody(ctx context.Context, path string, body io.Reader) (_ string, _ int64, err error) {
	partial := path + partialSuffix
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create the upload file: %w", err)
	}
	h := sha256.New()
	buf := make([]byte, stagingBufSize)
	// The struct hides any WriterTo of body, so the copy goes through buf.
	n, err := io.CopyBuffer(io.MultiWriter(f, h), struct{ io.Reader }{ctxReader{ctx: ctx, r: body}}, buf)
	if err != nil {
		return "", 0, errors.Join(fmt.Errorf("receive the upload: %w", err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return "", 0, errors.Join(fmt.Errorf("sync the upload: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("close the upload: %w", err)
	}
	if err := os.Rename(partial, path); err != nil {
		return "", 0, fmt.Errorf("store the upload: %w", err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return "", 0, fmt.Errorf("store the upload: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ctxReader stops reading once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.r.Read(p)
}

// insertLocked stores meta's upload and arms its expiry. s.mu is held.
func (s *staging) insertLocked(meta stagedMeta, dir string) {
	u := &stagedUpload{meta: meta, dir: dir}
	id := meta.UploadID
	u.stop = s.afterFunc(meta.Expires.Sub(s.now()), func() { s.expire(id, u) })
	s.stored[id] = u
}

// expire deletes u when its timer fires, unless it was removed first.
func (s *staging) expire(id string, u *stagedUpload) {
	s.mu.Lock()
	if s.stored[id] != u {
		s.mu.Unlock()
		return
	}
	delete(s.stored, id)
	s.mu.Unlock()
	if err := os.RemoveAll(u.dir); err != nil {
		slog.Warn("nexusd: cannot remove an expired upload", "uploadId", id, "error", err.Error())
	}
}

// get returns the stored upload id and the directory that holds it. An
// unknown, malformed or expired ID is not found.
func (s *staging) get(id string) (stagedMeta, string, bool) {
	if !uploadIDRE.MatchString(id) {
		return stagedMeta{}, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.stored[id]
	if !ok || !s.now().Before(u.meta.Expires) {
		return stagedMeta{}, "", false
	}
	return u.meta, u.dir, true
}

// remove deletes the upload id, stored or not. It is idempotent: an unknown
// ID or a directory already gone is not an error.
func (s *staging) remove(id string) error {
	if !uploadIDRE.MatchString(id) {
		return nil
	}
	s.mu.Lock()
	u, ok := s.stored[id]
	delete(s.stored, id)
	s.mu.Unlock()
	if ok {
		u.stop()
	}
	if err := os.RemoveAll(filepath.Join(s.dir, id)); err != nil {
		return fmt.Errorf("remove upload %s: %w", id, err)
	}
	return nil
}

// sweep reconciles the staging directory at startup: it deletes every entry
// that is not a directory with a valid, unexpired meta.json and its body,
// removes anything else inside the live ones (probe-* directories and
// .partial files left by a stop), and stores the live ones with their
// expiry re-armed (A-9).
func (s *staging) sweep() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("sweep the staging directory: %w", err)
	}
	var errs []error
	now := s.now()
	for _, e := range entries {
		dir := filepath.Join(s.dir, e.Name())
		meta, ok := readStagedMeta(dir, e)
		if !ok || !now.Before(meta.Expires) {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := pruneStagedDir(dir); err != nil {
			errs = append(errs, err)
		}
		s.mu.Lock()
		s.insertLocked(meta, dir)
		s.mu.Unlock()
	}
	if len(errs) > 0 {
		return fmt.Errorf("sweep the staging directory: %w", errors.Join(errs...))
	}
	return nil
}

// readStagedMeta returns the meta.json of the staging entry e at dir when
// e is a directory named by a valid upload ID, its meta.json decodes and
// names that ID, and its body is a regular file of the recorded size.
func readStagedMeta(dir string, e fs.DirEntry) (stagedMeta, bool) {
	if !e.IsDir() || !uploadIDRE.MatchString(e.Name()) {
		return stagedMeta{}, false
	}
	f, err := os.Open(filepath.Join(dir, stagedMetaName))
	if err != nil {
		return stagedMeta{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStagedMeta+1))
	if err != nil || len(b) > maxStagedMeta {
		return stagedMeta{}, false
	}
	var m stagedMeta
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return stagedMeta{}, false
	}
	if m.UploadID != e.Name() || len(m.SHA256) != sha256.Size*2 || m.Expires.IsZero() || m.SizeBytes < 0 {
		return stagedMeta{}, false
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return stagedMeta{}, false
	}
	fi, err := os.Lstat(filepath.Join(dir, stagedFileName))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != m.SizeBytes {
		return stagedMeta{}, false
	}
	return m, true
}

// pruneStagedDir removes everything in a live upload's directory except its
// body and meta.json.
func pruneStagedDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if n := e.Name(); n == stagedFileName || n == stagedMetaName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
