package nexusd

// The verified package pipeline (WI 5.2, FR-7, NFR-5). An uploaded release
// tarball is checked in a fixed order that the types enforce:
//
//  1. verifySignature hashes the file and checks its release signature. It
//     is the only constructor of verifiedArchive, so nothing below can run
//     on bytes that were not verified.
//  2. scan reads the verified bytes as gzip+tar and applies every layout
//     rule. It writes nothing.
//  3. extractFile and extractAll write only under a destination directory.
//
// scan, extractFile and extractAll each re-hash the stream they read and
// fail at EOF unless it is the verified digest, so a file swapped after
// verification is never accepted (no TOCTOU gap). Every refusal is a
// *pkgError carrying a check value; pkgProblem maps it to
// update-signature-invalid or update-package-invalid.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/release"
)

// Package limits. MaxPackageEntries replaces FR-7's 64 (A-43): a real
// release tree already holds more than 64 entries.
const (
	MaxPackageEntries       = 1024
	MaxPackageSize    int64 = 256 << 20 // Sum of the entry sizes (A-35)
)

const (
	// pkgMaxMetadata bounds the decompressed bytes that are not entry
	// content: headers, PAX and GNU records, padding and the end-of-archive
	// blocks. With MaxPackageSize it bounds the whole decompressed stream,
	// so a gzip bomb stops there whatever its headers declare.
	pkgMaxMetadata = 16 << 20
	// pkgMaxNameLen is Linux's PATH_MAX; a longer name cannot be extracted.
	pkgMaxNameLen = 4096
	// pkgReadBuffer is the size of the file and gzip read buffers.
	pkgReadBuffer = 32 << 10
	// pkgDirMode and the file modes are the normalized modes (A-36).
	pkgDirMode  fs.FileMode = 0o700
	pkgExecMode fs.FileMode = 0o755
	pkgFileMode fs.FileMode = 0o644
)

// pkgProbeTimeout bounds the "-version" run of a package's binary. Tests
// shorten it.
var pkgProbeTimeout = versionTimeout

// pkgRequiredFiles are the regular files every package holds below its
// top-level directory.
var pkgRequiredFiles = []string{"nexusd", "install.sh", "nexusd.service", "nexusd.default.yaml", "uninstall.sh"}

// pkgCheck is the check extension of update-package-invalid: which rule a
// package broke.
type pkgCheck string

const (
	pkgCheckSignature       pkgCheck = "signature"
	pkgCheckLayout          pkgCheck = "layout"
	pkgCheckEntryType       pkgCheck = "entry-type"
	pkgCheckPath            pkgCheck = "path"
	pkgCheckSize            pkgCheck = "size"
	pkgCheckEntries         pkgCheck = "entries"
	pkgCheckDuplicateEntry  pkgCheck = "duplicate-entry"
	pkgCheckRequiredFile    pkgCheck = "required-file"
	pkgCheckVersionProbe    pkgCheck = "version-probe"
	pkgCheckVersionMismatch pkgCheck = "version-mismatch"
)

// pkgError is a package refused by a check. An I/O error reading or
// writing is not a pkgError: it says nothing about the package.
type pkgError struct {
	check  pkgCheck
	detail string
	cause  error
}

func (e *pkgError) Error() string {
	s := "package " + string(e.check) + " check: " + e.detail
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

func (e *pkgError) Unwrap() error { return e.cause }

// pkgFail returns a *pkgError as an error.
func pkgFail(check pkgCheck, cause error, format string, args ...any) error {
	return &pkgError{check: check, detail: fmt.Sprintf(format, args...), cause: cause}
}

// pkgCheckOf returns the check err failed, if it is a package refusal.
func pkgCheckOf(err error) (pkgCheck, bool) {
	var pe *pkgError
	if errors.As(err, &pe) {
		return pe.check, true
	}
	return "", false
}

// pkgProblem maps a package refusal to its problem: update-signature-invalid
// for the signature check, else update-package-invalid with the check. It
// returns false for any other error.
func pkgProblem(err error) (*problem.Error, bool) {
	var pe *pkgError
	if !errors.As(err, &pe) {
		return nil, false
	}
	if pe.check == pkgCheckSignature {
		return problem.New(problem.UpdateSignatureInvalid, pe.detail), true
	}
	return problem.New(problem.UpdatePackageInvalid, pe.detail).With("check", string(pe.check)), true
}

// pkgQuote quotes an archive-supplied name for an error, cut to 128 bytes.
func pkgQuote(s string) string {
	const maxQuoted = 128
	if len(s) > maxQuoted {
		return fmt.Sprintf("%q...", s[:maxQuoted])
	}
	return fmt.Sprintf("%q", s)
}

// pkgManifest is what scan learned: the top-level directory and the
// version it names.
type pkgManifest struct {
	top     string // nexusd-<version>-linux-<goarch>
	version string
}

// verifiedArchive is a release tarball whose bytes verified against a
// trusted key. Only verifySignature makes one.
type verifiedArchive struct {
	path   string
	digest [sha256.Size]byte
	size   int64
}

// verifySignature hashes the file at path and checks sigB64 (the .sig
// content: standard base64, optionally newline-terminated) against keys. A
// malformed signature or an empty key list is refused before the file is
// opened. The archive itself is never parsed here.
func verifySignature(path string, keys []ed25519.PublicKey, sigB64 string) (*verifiedArchive, error) {
	sig, err := release.DecodeSignature([]byte(sigB64))
	if err != nil {
		return nil, pkgFail(pkgCheckSignature, err, "the signature is not a base64 Ed25519 signature")
	}
	if len(keys) == 0 {
		return nil, pkgFail(pkgCheckSignature, nil, "no trusted key is configured")
	}
	f, err := pkgOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	// The anonymous struct hides *os.File's WriterTo so CopyBuffer uses buf.
	n, err := io.CopyBuffer(h, struct{ io.Reader }{f}, make([]byte, pkgReadBuffer))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var digest [sha256.Size]byte
	h.Sum(digest[:0])
	if !release.Verify(keys, digest, sig) {
		return nil, pkgFail(pkgCheckSignature, nil, "the signature does not verify against any trusted key")
	}
	return &verifiedArchive{path: path, digest: digest, size: n}, nil
}

// pkgOpen opens a regular file for reading, refusing a symlink.
func pkgOpen(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return f, nil
}

// pkgGunzip starts decompressing r. It is a seam: tests count its calls
// and the bytes read from what it returns. The archive must be a single
// gzip member; finish refuses anything after it.
var pkgGunzip = func(r io.Reader) (io.Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	gz.Multistream(false)
	return gz, nil
}

// pkgHashReader hashes every byte read from the file. It reads at most one
// byte past the verified size, so a file grown after verification is
// detected without reading all of it.
type pkgHashReader struct {
	r   io.Reader
	h   hash.Hash
	n   int64
	err error // The first read error other than EOF
}

func (r *pkgHashReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.h.Write(p[:n])
	r.n += int64(n)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// pkgCapReader fails once more than left bytes are read, so decompression
// stops at the bound however much the archive claims.
type pkgCapReader struct {
	r    io.Reader
	left int64
}

func (r *pkgCapReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		var one [1]byte
		n, err := r.r.Read(one[:])
		if n > 0 {
			return 0, pkgFail(pkgCheckSize, nil, "the decompressed archive is larger than %d bytes", MaxPackageSize+pkgMaxMetadata)
		}
		return 0, err
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

// pkgStream is one read of a verified archive: the file, hashed, through
// gzip and tar.
type pkgStream struct {
	va  *verifiedArchive
	f   *os.File
	raw *pkgHashReader
	br  *bufio.Reader
	dec io.Reader // Decompressed and capped
	tr  *tar.Reader

	entries int
	total   int64
	top     string
}

// pkgEntry is one validated archive entry.
type pkgEntry struct {
	hdr  *tar.Header
	name string // path.Clean(hdr.Name)
	rel  string // name below the top-level directory; "" for the top itself
	dir  bool
}

// open starts a read of the verified bytes. The caller closes the stream.
func (va *verifiedArchive) open() (*pkgStream, error) {
	f, err := pkgOpen(va.path)
	if err != nil {
		return nil, err
	}
	s := &pkgStream{va: va, f: f}
	s.raw = &pkgHashReader{r: io.LimitReader(f, va.size+1), h: sha256.New()}
	s.br = bufio.NewReaderSize(s.raw, pkgReadBuffer)
	gz, err := pkgGunzip(s.br)
	if err != nil {
		err = s.refuse(s.classify(err))
		f.Close()
		return nil, err
	}
	s.dec = &pkgCapReader{r: gz, left: MaxPackageSize + pkgMaxMetadata}
	s.tr = tar.NewReader(s.dec)
	return s, nil
}

func (s *pkgStream) close() { s.f.Close() }

// classify returns err as a refusal: a *pkgError as it is, a file read
// error as an I/O error, and anything else as a malformed archive.
func (s *pkgStream) classify(err error) error {
	if _, ok := pkgCheckOf(err); ok {
		return err
	}
	if s.raw.err != nil {
		return fmt.Errorf("read %s: %w", s.va.path, s.raw.err)
	}
	return pkgFail(pkgCheckLayout, err, "not a valid gzip-compressed tar archive")
}

// next returns the next entry, checked against the rules that make it safe
// to write: type, name, a single top-level directory, and the entry and
// size bounds. It returns io.EOF after the last entry.
func (s *pkgStream) next() (pkgEntry, error) {
	hdr, err := s.tr.Next()
	if err == io.EOF {
		return pkgEntry{}, io.EOF
	}
	if errors.Is(err, tar.ErrInsecurePath) && hdr != nil {
		return pkgEntry{}, pkgFail(pkgCheckPath, nil, "entry %s is not a local path", pkgQuote(hdr.Name))
	}
	if err != nil {
		return pkgEntry{}, s.classify(err)
	}
	s.entries++
	if s.entries > MaxPackageEntries {
		return pkgEntry{}, pkgFail(pkgCheckEntries, nil, "the archive holds more than %d entries", MaxPackageEntries)
	}
	// archive/tar has already applied PAX and GNU long-name records and
	// mapped the legacy regular-file type, so hdr.Name and hdr.Typeflag
	// are what would be extracted.
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	default:
		return pkgEntry{}, pkgFail(pkgCheckEntryType, nil, "entry %s has type %q; only regular files and directories are allowed",
			pkgQuote(hdr.Name), hdr.Typeflag)
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return pkgEntry{}, pkgFail(pkgCheckEntryType, nil, "entry %s is a sparse file", pkgQuote(hdr.Name))
		}
	}
	name, err := pkgCleanName(hdr.Name)
	if err != nil {
		return pkgEntry{}, err
	}
	top, rel, _ := strings.Cut(name, "/")
	if s.top == "" {
		s.top = top
	} else if top != s.top {
		return pkgEntry{}, pkgFail(pkgCheckLayout, nil, "the archive has more than one top-level entry: %s and %s",
			pkgQuote(s.top), pkgQuote(top))
	}
	dir := hdr.Typeflag == tar.TypeDir
	if rel == "" && !dir {
		return pkgEntry{}, pkgFail(pkgCheckLayout, nil, "the top-level entry %s is not a directory", pkgQuote(top))
	}
	if hdr.Size < 0 {
		return pkgEntry{}, pkgFail(pkgCheckLayout, nil, "entry %s has a negative size", pkgQuote(name))
	}
	if !dir {
		s.total += hdr.Size
		if s.total > MaxPackageSize {
			return pkgEntry{}, pkgFail(pkgCheckSize, nil, "the archive's files total more than %d bytes", MaxPackageSize)
		}
	}
	return pkgEntry{hdr: hdr, name: name, rel: rel, dir: dir}, nil
}

// pkgCleanName refuses an empty, over-long or absolute name and any ".."
// component, and returns the path.Clean'ed name. Tar names are
// "/"-separated whatever the host, so this uses path, not filepath.
func pkgCleanName(name string) (string, error) {
	if name == "" || len(name) > pkgMaxNameLen {
		return "", pkgFail(pkgCheckPath, nil, "entry name %s is empty or longer than %d bytes", pkgQuote(name), pkgMaxNameLen)
	}
	if strings.HasPrefix(name, "/") {
		return "", pkgFail(pkgCheckPath, nil, "entry %s is an absolute path", pkgQuote(name))
	}
	for c := range strings.SplitSeq(name, "/") {
		if c == ".." {
			return "", pkgFail(pkgCheckPath, nil, "entry %s has a \"..\" component", pkgQuote(name))
		}
	}
	clean := path.Clean(name)
	if clean == "." || !filepath.IsLocal(clean) {
		return "", pkgFail(pkgCheckPath, nil, "entry %s is not a path below the archive root", pkgQuote(name))
	}
	return clean, nil
}

// finish reads the rest of the archive and the file, and fails unless the
// bytes read are exactly the verified bytes. Only then is data after the
// gzip member refused.
func (s *pkgStream) finish() error {
	if _, err := io.Copy(io.Discard, s.dec); err != nil {
		return s.refuse(s.classify(err))
	}
	trailing, err := s.drain()
	if err != nil {
		return err
	}
	if trailing > 0 {
		return pkgFail(pkgCheckLayout, nil, "%d bytes follow the gzip stream", trailing)
	}
	return nil
}

// drain reads the rest of the file, without decompressing it, and fails
// with the signature check unless the file held exactly the verified
// bytes: anything else is not what verified. It returns how many bytes
// followed the gzip member.
func (s *pkgStream) drain() (int64, error) {
	trailing, err := io.Copy(io.Discard, s.br)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", s.va.path, err)
	}
	var digest [sha256.Size]byte
	s.raw.h.Sum(digest[:0])
	if s.raw.n != s.va.size || digest != s.va.digest {
		return 0, pkgFail(pkgCheckSignature, nil, "the package changed after its signature was verified")
	}
	return trailing, nil
}

// refuse returns a refusal as it is, unless the file no longer holds the
// verified bytes: then the archive read was not the verified one, and the
// refusal is the signature check. Any other error is returned unchanged.
// The stream cannot be read after refuse.
func (s *pkgStream) refuse(err error) error {
	if _, ok := pkgCheckOf(err); !ok {
		return err
	}
	if _, derr := s.drain(); derr != nil {
		if c, ok := pkgCheckOf(derr); ok && c == pkgCheckSignature {
			return derr
		}
	}
	return err
}

// pkgNode is a name in scan's tree of entries: a file, a directory entry,
// or a directory only implied by the names below it.
type pkgNode struct {
	kind pkgNodeKind
	kids map[string]*pkgNode
}

type pkgNodeKind uint8

const (
	pkgNodeImplied pkgNodeKind = iota
	pkgNodeDir
	pkgNodeFile
)

// add records the cleaned name. A name already present as an entry is a
// duplicate whatever the two types (A-46); a file where a directory is
// implied, or below a file, is a layout error. The tree holds one node per
// name component, so it is bounded by MaxPackageEntries × pkgMaxNameLen.
func (n *pkgNode) add(name string, dir bool) error {
	cur := n
	rest := name
	for {
		comp, more, found := strings.Cut(rest, "/")
		kid := cur.kids[comp]
		if !found {
			switch {
			case kid == nil:
			case kid.kind != pkgNodeImplied:
				return pkgFail(pkgCheckDuplicateEntry, nil, "entry %s appears more than once", pkgQuote(name))
			case !dir:
				return pkgFail(pkgCheckLayout, nil, "file %s is also a directory holding other entries", pkgQuote(name))
			}
			if kid == nil {
				kid = &pkgNode{}
				if cur.kids == nil {
					cur.kids = map[string]*pkgNode{}
				}
				cur.kids[comp] = kid
			}
			kid.kind = pkgNodeFile
			if dir {
				kid.kind = pkgNodeDir
			}
			return nil
		}
		if kid == nil {
			kid = &pkgNode{kind: pkgNodeImplied}
			if cur.kids == nil {
				cur.kids = map[string]*pkgNode{}
			}
			cur.kids[comp] = kid
		} else if kid.kind == pkgNodeFile {
			return pkgFail(pkgCheckLayout, nil, "entry %s is below a file", pkgQuote(name))
		}
		cur, rest = kid, more
	}
}

// scan applies every layout rule to the verified bytes and writes nothing:
// exactly one top-level directory nexusd-<v>-linux-<goarch> (A-13), only
// regular files and directories, no absolute or ".." names, the size and
// entry bounds, no duplicate names (A-46) and the required files. It
// re-hashes the stream and fails at EOF unless it is the verified digest.
func (va *verifiedArchive) scan(goarch string) (pkgManifest, error) {
	s, err := va.open()
	if err != nil {
		return pkgManifest{}, err
	}
	defer s.close()
	var m pkgManifest
	var names pkgNode
	found := map[string]bool{}
	for {
		e, err := s.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return pkgManifest{}, s.refuse(err)
		}
		if m.top == "" {
			v, err := pkgParseTop(s.top, goarch)
			if err != nil {
				return pkgManifest{}, s.refuse(err)
			}
			m = pkgManifest{top: s.top, version: v}
		}
		if err := names.add(e.name, e.dir); err != nil {
			return pkgManifest{}, s.refuse(err)
		}
		if !e.dir {
			found[e.rel] = true
		}
	}
	if err := s.finish(); err != nil {
		return pkgManifest{}, err
	}
	if m.top == "" {
		return pkgManifest{}, pkgFail(pkgCheckLayout, nil, "the archive is empty")
	}
	for _, r := range pkgRequiredFiles {
		if !found[r] {
			return pkgManifest{}, pkgFail(pkgCheckRequiredFile, nil, "the package has no regular file %s/%s", m.top, r)
		}
	}
	return m, nil
}

// pkgParseTop returns the version a top-level directory names. It must be
// nexusd-<version>-linux-<goarch>, with a release version (A-13, A-25).
func pkgParseTop(top, goarch string) (string, error) {
	prefix, suffix := "nexusd-", "-linux-"+goarch
	v, ok := strings.CutPrefix(top, prefix)
	if ok {
		v, ok = strings.CutSuffix(v, suffix)
	}
	if ok {
		_, ok = release.CompareVersions(v, v)
	}
	if !ok || goarch == "" {
		return "", pkgFail(pkgCheckLayout, nil, "the top-level directory %s is not nexusd-<version>-linux-%s", pkgQuote(top), goarch)
	}
	return v, nil
}

// pkgFileModeOf normalizes a tar mode (A-36): 0755 if any execute bit is
// set, else 0644. Setuid, setgid and sticky are never kept.
func pkgFileModeOf(mode int64) fs.FileMode {
	if mode&0o111 != 0 {
		return pkgExecMode
	}
	return pkgFileMode
}

// pkgOpenDst opens dst as a root for extraction, creating it 0700 when
// missing. An existing dst must be a directory, not a symlink.
func pkgOpenDst(dst string) (*os.Root, error) {
	fi, err := os.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dst, pkgDirMode); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return nil, fmt.Errorf("extract: %s is not a directory", dst)
	}
	return os.OpenRoot(dst)
}

// pkgRemoveDst removes dst after a failed extraction and joins any error
// doing so to err.
func pkgRemoveDst(dst string, err error) error {
	if rerr := os.RemoveAll(dst); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// pkgMkdirAll creates each missing directory of name below root at 0700.
// A component that exists must be a real directory: a symlink or a file in
// the way fails, so nothing is written through it. os.Root also refuses
// any path that escapes root.
func pkgMkdirAll(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	end := 0
	for end < len(name) {
		next := strings.IndexByte(name[end+1:], '/')
		if next < 0 {
			end = len(name)
		} else {
			end += 1 + next
		}
		p := name[:end]
		fi, err := root.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := pkgMkdir(root, p); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return pkgFail(pkgCheckPath, nil, "%s is a symlink in the extraction directory", pkgQuote(p))
		case !fi.IsDir():
			return pkgFail(pkgCheckDuplicateEntry, nil, "%s already exists and is not a directory", pkgQuote(p))
		}
	}
	return nil
}

// pkgMkdir creates one directory and sets its mode exactly, whatever the
// umask.
func pkgMkdir(root *os.Root, name string) error {
	if err := root.Mkdir(name, pkgDirMode); err != nil {
		return err
	}
	d, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	err = d.Chmod(pkgDirMode)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// pkgErrReader remembers the first read error other than EOF, so a copy
// can tell a bad archive from a failed write.
type pkgErrReader struct {
	r   io.Reader
	err error
}

func (r *pkgErrReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

// pkgWriteFile creates name below root (O_EXCL|O_NOFOLLOW: never over an
// existing file or through a symlink) and copies the current entry of s
// into it, then sets the normalized mode on the open file.
func pkgWriteFile(root *os.Root, name string, s *pkgStream, mode fs.FileMode) (err error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return pkgFail(pkgCheckDuplicateEntry, err, "%s already exists in the extraction directory", pkgQuote(name))
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	// The anonymous struct hides *os.File's ReaderFrom so CopyBuffer uses buf.
	src := &pkgErrReader{r: s.tr}
	if _, err := io.CopyBuffer(struct{ io.Writer }{f}, src, make([]byte, pkgReadBuffer)); err != nil {
		if src.err != nil {
			return s.classify(src.err)
		}
		return err
	}
	return f.Chmod(mode)
}

// extractAll writes the whole archive under dst: directories 0700, files
// 0755 or 0644 (A-36), each file created O_EXCL|O_NOFOLLOW. It applies the
// per-entry safety rules of scan but not its layout rules, so it is safe
// even on an archive scan was never run on. The digest is checked at EOF;
// on any error dst is removed.
func (va *verifiedArchive) extractAll(dst string) (err error) {
	root, err := pkgOpenDst(dst)
	if err != nil {
		return pkgRemoveDst(dst, err)
	}
	defer root.Close()
	defer func() {
		if err != nil {
			err = pkgRemoveDst(dst, err)
		}
	}()
	s, err := va.open()
	if err != nil {
		return err
	}
	defer s.close()
	for {
		e, err := s.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s.refuse(err)
		}
		if e.dir {
			if err := pkgMkdirAll(root, e.name); err != nil {
				return s.refuse(err)
			}
			continue
		}
		if err := pkgMkdirAll(root, path.Dir(e.name)); err != nil {
			return s.refuse(err)
		}
		if err := pkgWriteFile(root, e.name, s, pkgFileModeOf(e.hdr.Mode)); err != nil {
			return s.refuse(err)
		}
	}
	return s.finish()
}

// extractFile writes the regular file <top>/name of the archive to
// dst/name and returns that path. name is one path component. It reads the
// whole archive, applying the per-entry safety rules, and checks the
// digest at EOF; on any error dst is removed.
func (va *verifiedArchive) extractFile(name, dst string) (_ string, err error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", fmt.Errorf("extract: %q is not a file name", name)
	}
	root, err := pkgOpenDst(dst)
	if err != nil {
		return "", pkgRemoveDst(dst, err)
	}
	defer root.Close()
	defer func() {
		if err != nil {
			err = pkgRemoveDst(dst, err)
		}
	}()
	s, err := va.open()
	if err != nil {
		return "", err
	}
	defer s.close()
	written := false
	for {
		e, err := s.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", s.refuse(err)
		}
		if e.rel != name {
			continue
		}
		if e.dir {
			return "", s.refuse(pkgFail(pkgCheckRequiredFile, nil, "%s/%s is a directory", s.top, name))
		}
		if written {
			return "", s.refuse(pkgFail(pkgCheckDuplicateEntry, nil, "entry %s appears more than once", pkgQuote(e.name)))
		}
		if err := pkgWriteFile(root, name, s, pkgFileModeOf(e.hdr.Mode)); err != nil {
			return "", s.refuse(err)
		}
		written = true
	}
	if err := s.finish(); err != nil {
		return "", err
	}
	if !written {
		return "", pkgFail(pkgCheckRequiredFile, nil, "the package has no regular file %s/%s", s.top, name)
	}
	return filepath.Join(dst, name), nil
}

// probe extracts the package's nexusd into a new probe-* directory under
// stagingDir, runs probeVersion on it against want, and always removes the
// directory (admission step 5).
func (va *verifiedArchive) probe(ctx context.Context, run Runner, stagingDir, want string) (err error) {
	dir, err := os.MkdirTemp(stagingDir, "probe-")
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	bin, err := va.extractFile("nexusd", abs)
	if err != nil {
		return err
	}
	return probeVersion(ctx, run, bin, want)
}

// errPkgProbeTimeout ends a package binary's "-version" run.
var errPkgProbeTimeout = errors.New("-version did not finish in time")

// probeVersion runs "bin -version" for at most pkgProbeTimeout (5 s) and
// requires it to print "nexusd <want>". A run that fails, exits non-zero,
// times out or prints anything else fails version-probe; another version
// fails version-mismatch.
func probeVersion(ctx context.Context, run Runner, bin, want string) error {
	if !filepath.IsAbs(bin) {
		return fmt.Errorf("probe: %s is not an absolute path", bin)
	}
	runCtx, cancel := context.WithTimeoutCause(ctx, pkgProbeTimeout, errPkgProbeTimeout)
	defer cancel()
	start := time.Now()
	res, err := run.Run(runCtx, []string{bin, "-version"})
	if err != nil {
		return pkgFail(pkgCheckVersionProbe, err, "the package's nexusd -version failed after %s",
			time.Since(start).Round(time.Millisecond))
	}
	if res.ExitCode != 0 {
		return pkgFail(pkgCheckVersionProbe, nil, "the package's nexusd -version exited %d", res.ExitCode)
	}
	v, ok := parseVersion(res.Stdout)
	if !ok {
		return pkgFail(pkgCheckVersionProbe, nil, "the package's nexusd -version printed %s, not \"nexusd <version>\"",
			pkgQuote(string(res.Stdout)))
	}
	if v != want {
		return pkgFail(pkgCheckVersionMismatch, nil, "the package's nexusd is %s, but the package is %s", v, want)
	}
	return nil
}
