package nginx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// The file primitives of the nginx helper (FR-11, NFR-3). Every one works
// on names relative to an includeDir directory fd that openIncludeDir
// opened O_DIRECTORY|O_NOFOLLOW, and none follows a symlink at a name in
// it. Temporary files are dotfiles (.<name>.nexus-<short op id>.tmp), which
// nginx's include glob never matches (L-23) and list never shows.

// backupDirName is the directory under an op dir that holds a backup:
// content (the bytes) and meta.json (a Meta).
const backupDirName = "previous"

// Meta is what Backup recorded about a file, in previous/meta.json. When
// Exists is false the file did not exist, and Restore removes it.
type Meta struct {
	FileName string `json:"fileName"`
	Exists   bool   `json:"exists"`
	SHA256   string `json:"sha256,omitempty"`
	Size     int64  `json:"size"`
	Mode     uint32 `json:"mode"` // Permission bits only
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
}

// PreviousContentPath is where Backup keeps the previous bytes of a file
// for the op whose directory is opDir.
func PreviousContentPath(opDir string) string {
	return filepath.Join(opDir, backupDirName, "content")
}

// Backup copies fileName's bytes in dirfd to <opDir>/previous/content and
// records its Meta in <opDir>/previous/meta.json, both 0600 and fsynced.
// meta.json appears last, through a rename, so its presence means the
// backup is complete. A missing file is recorded as Exists: false with no
// content. Anything other than a regular file of at most max bytes is
// refused, and an existing backup is never overwritten.
func Backup(dirfd int, fileName, opDir string, max int64) (Meta, error) {
	if err := checkBaseName(fileName); err != nil {
		return Meta{}, err
	}
	var st unix.Stat_t
	err := ignoringEINTR(func() error { return unix.Fstatat(dirfd, fileName, &st, unix.AT_SYMLINK_NOFOLLOW) })
	missing := errors.Is(err, unix.ENOENT)
	if err != nil && !missing {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	m := Meta{FileName: fileName}
	var content []byte
	if !missing {
		f := newFileStat(fileName, &st)
		if f.Type != spec.FileTypeFile {
			return Meta{}, fmt.Errorf("back up %s: it is a %s, not a regular file", fileName, f.Type)
		}
		b, digest, err := readManaged(dirfd, f, max)
		if err != nil {
			return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
		}
		content = b
		m = Meta{FileName: fileName, Exists: true, SHA256: digest, Size: int64(len(b)),
			Mode: uint32(st.Mode) & 0o7777, UID: st.Uid, GID: st.Gid}
	}
	prev := filepath.Join(opDir, backupDirName)
	if err := os.Mkdir(prev, 0o700); err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	if m.Exists {
		if err := writeNew(filepath.Join(prev, "content"), content); err != nil {
			return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
		}
	}
	j, err := json.Marshal(m)
	if err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	tmp := filepath.Join(prev, ".meta.json.tmp")
	if err := writeNew(tmp, j); err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	if err := os.Rename(tmp, filepath.Join(prev, "meta.json")); err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	if err := syncPath(prev); err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	if err := syncPath(opDir); err != nil {
		return Meta{}, fmt.Errorf("back up %s: %w", fileName, err)
	}
	return m, nil
}

// Identity is the device and inode of a directory entry: what a rollback
// compares before it acts on a name, so it never acts on an entry that is
// not the one the operation made.
type Identity struct{ Dev, Ino uint64 }

// IsZero reports whether i names no entry.
func (i Identity) IsZero() bool { return i == Identity{} }

// ErrAppeared is WriteAtomic's error, with create set, when an entry
// appeared at the name after the caller checked that none was there. Nothing
// was changed: the entry is left as it is.
var ErrAppeared = errors.New("an entry appeared at the name while the file was being created")

// ErrNotWritten is what Restore wraps when it refuses because the entry at
// the name is not the file the operation wrote, or the operation never
// learned which file that is. Restore has changed nothing then: the entry
// is not Nexus's to remove or overwrite.
var ErrNotWritten = errors.New("not the file this operation wrote")

// WriteAtomic writes fileName in dirfd with content, owned uid:gid with
// permission bits mode, and returns the identity of the file it wrote. It
// creates the hidden tmpName with O_CREAT|O_EXCL|O_NOFOLLOW, so an existing
// entry there (a leftover file or a planted symlink) fails with EEXIST and
// is left as it was. The temp file is written, chowned, chmodded and
// fsynced, then renamed onto fileName and the directory is fsynced.
// fileName is never opened for writing. On failure the temp file is
// removed.
//
// With create unset the rename replaces whatever entry is at fileName (a
// symlink is replaced, never followed). With create set it never replaces:
// an entry there fails with ErrAppeared and is left as it was, which is how
// a create makes sure it is not overwriting a file that appeared after the
// caller's check. The identity is returned even with an error once the
// rename happened (the fsync after it failed), so the caller can undo
// exactly that file; before it, the identity is zero.
func WriteAtomic(dirfd int, fileName string, content []byte, mode uint32, uid, gid int, tmpName string,
	create bool) (id Identity, err error) {
	if err := checkBaseName(fileName); err != nil {
		return Identity{}, err
	}
	if err := checkBaseName(tmpName); err != nil {
		return Identity{}, err
	}
	if !strings.HasPrefix(tmpName, ".") {
		return Identity{}, fmt.Errorf("write %s: temporary name %q is not hidden", fileName, tmpName)
	}
	var fd int
	err = ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dirfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		return err
	})
	if err != nil {
		return Identity{}, fmt.Errorf("write %s: create %s: %w", fileName, tmpName, err)
	}
	created := true
	defer func() {
		if err != nil && created {
			if uerr := unlinkAt(dirfd, tmpName); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				err = errors.Join(err, fmt.Errorf("remove %s: %w", tmpName, uerr))
			}
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Identity{}, errors.Join(fmt.Errorf("write %s: %s: %w", fileName, tmpName, err), unix.Close(fd))
	}
	written := Identity{Dev: uint64(st.Dev), Ino: st.Ino}
	f := os.NewFile(uintptr(fd), tmpName)
	if err := fillFile(f, content, mode, uid, gid); err != nil {
		return Identity{}, fmt.Errorf("write %s: %s: %w", fileName, tmpName, err)
	}
	if create {
		if err := RenameNoReplace(dirfd, tmpName, fileName); err != nil {
			// RenameNoReplace also syncs the directory: the rename may have
			// happened when that failed.
			if now, serr := statAt(dirfd, fileName); serr == nil && now != nil && now.Dev == written.Dev &&
				now.Ino == written.Ino {
				created = false
				return written, fmt.Errorf("write %s: %w", fileName, err)
			}
			if errors.Is(err, unix.EEXIST) {
				return Identity{}, fmt.Errorf("write %s: %s: %w: %w", fileName, tmpName, ErrAppeared, err)
			}
			return Identity{}, fmt.Errorf("write %s: rename %s: %w", fileName, tmpName, err)
		}
		created = false
		return written, nil
	}
	if err := ignoringEINTR(func() error { return unix.Renameat(dirfd, tmpName, dirfd, fileName) }); err != nil {
		return Identity{}, fmt.Errorf("write %s: rename %s: %w", fileName, tmpName, err)
	}
	created = false
	return written, syncDirFd(dirfd, fileName)
}

// fillFile writes content to the new file f, sets its owner and then its
// mode (in that order, as chown clears set-ID bits), fsyncs and closes it.
func fillFile(f *os.File, content []byte, mode uint32, uid, gid int) error {
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	fd := int(f.Fd())
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := unix.Fchmod(fd, mode&0o7777); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// Restore returns m.FileName in dirfd to what Backup recorded in m, undoing
// the write that made the entry identified by wrote. A file that existed
// gets the bytes of <opDir>/previous/content with the recorded mode and
// owner, through WriteAtomic with tmpName. A file that did not exist is
// removed. Either way the result is checked afterwards, and any difference
// (a hash, a mode, an owner, or a file that is still there) is an error
// naming what was expected and what was found: a restore is never reported
// done unless it is proven.
//
// Restore acts only on the entry it is undoing. When an entry is at the name
// and is not the one wrote identifies (replaced by someone else since the
// write, or wrote is zero because the caller never learned it), it is left
// as it is and the restore fails: it must not delete or overwrite a file
// the operation did not write. There is no atomic "unlink this inode", so
// the check and the change are two calls; the window is the time between
// them.
func Restore(dirfd int, m Meta, wrote Identity, opDir, tmpName string) error {
	if err := checkBaseName(m.FileName); err != nil {
		return err
	}
	here, err := statAt(dirfd, m.FileName)
	if err != nil {
		return fmt.Errorf("restore %s: %w", m.FileName, err)
	}
	if here != nil && (wrote.IsZero() || here.Dev != wrote.Dev || here.Ino != wrote.Ino) {
		return fmt.Errorf("restore %s: the %s there has inode %d and the operation wrote inode %d: %w",
			m.FileName, here.Type, here.Ino, wrote.Ino, ErrNotWritten)
	}
	if !m.Exists {
		if err := unlinkAt(dirfd, m.FileName); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("restore %s: remove the created file: %w", m.FileName, err)
		}
		now, err := statAt(dirfd, m.FileName)
		if err != nil {
			return fmt.Errorf("restore %s: %w", m.FileName, err)
		}
		if now != nil {
			return fmt.Errorf("restore %s: it did not exist before, and a %s is still there", m.FileName, now.Type)
		}
		return syncDirFd(dirfd, m.FileName)
	}
	path := PreviousContentPath(opDir)
	content, err := readPrevious(path, m.Size)
	if err != nil {
		return fmt.Errorf("restore %s: %w", m.FileName, err)
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return fmt.Errorf("restore %s: the previous content at %s has sha256 %s, but the backup recorded sha256 %s; %s was not restored",
			m.FileName, path, got, m.SHA256, m.FileName)
	}
	// The written file is gone when here is nil: put the previous one back
	// without replacing whatever has appeared since.
	if _, err := WriteAtomic(dirfd, m.FileName, content, m.Mode, int(m.UID), int(m.GID), tmpName, here == nil); err != nil {
		return fmt.Errorf("restore %s: %w", m.FileName, err)
	}
	got, err := Hash(dirfd, m.FileName)
	if err != nil {
		return fmt.Errorf("restore %s: re-hash: %w", m.FileName, err)
	}
	if got != m.SHA256 {
		return fmt.Errorf("restore %s: the restored file has sha256 %s, want sha256 %s", m.FileName, got, m.SHA256)
	}
	var st unix.Stat_t
	if err := ignoringEINTR(func() error {
		return unix.Fstatat(dirfd, m.FileName, &st, unix.AT_SYMLINK_NOFOLLOW)
	}); err != nil {
		return fmt.Errorf("restore %s: %w", m.FileName, err)
	}
	if mode := uint32(st.Mode) & 0o7777; mode != m.Mode || st.Uid != m.UID || st.Gid != m.GID {
		return fmt.Errorf("restore %s: the restored file has mode %#o and owner %d:%d, want mode %#o and owner %d:%d",
			m.FileName, mode, st.Uid, st.Gid, m.Mode, m.UID, m.GID)
	}
	return nil
}

// readPrevious reads a backup's content file, which must be a regular file
// of exactly size bytes.
func readPrevious(path string, size int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size {
		return nil, fmt.Errorf("%s holds %d bytes or more, but the backup recorded %d", path, len(b), size)
	}
	return b, nil
}

// Hash is the hex SHA-256 of the regular file name in dirfd. It never
// follows a symlink and never blocks on a FIFO.
func Hash(dirfd int, name string) (string, error) {
	if err := checkBaseName(name); err != nil {
		return "", err
	}
	var fd int
	err := ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", fmt.Errorf("hash %s: %w", name, err)
	}
	if t := fileType(uint32(st.Mode)); t != spec.FileTypeFile {
		return "", fmt.Errorf("hash %s: it is a %s, not a regular file", name, t)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkBaseName refuses anything that is not a single entry name, so no
// primitive ever leaves its directory.
func checkBaseName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%q is not a file name in includeDir", name)
	}
	return nil
}

func unlinkAt(dirfd int, name string) error {
	return ignoringEINTR(func() error { return unix.Unlinkat(dirfd, name, 0) })
}

// syncDirFd fsyncs the directory dirfd after fileName changed in it.
func syncDirFd(dirfd int, fileName string) error {
	if err := ignoringEINTR(func() error { return unix.Fsync(dirfd) }); err != nil {
		return fmt.Errorf("%s: sync the directory: %w", fileName, err)
	}
	return nil
}

// writeNew creates path, which must not exist, as a 0600 file holding b,
// and fsyncs it.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	// The umask can only remove bits from 0600; Chmod states the contract (L-54).
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// syncPath fsyncs the directory at path, so the entries made in it last.
func syncPath(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		return errors.Join(err, d.Close())
	}
	return d.Close()
}
