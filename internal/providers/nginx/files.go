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

// WriteAtomic replaces fileName in dirfd with content, owned uid:gid with
// permission bits mode. It creates the hidden tmpName with
// O_CREAT|O_EXCL|O_NOFOLLOW, so an existing entry there (a leftover file or
// a planted symlink) fails with EEXIST and is left as it was. The temp file
// is written, chowned, chmodded and fsynced, then renamed onto fileName,
// which replaces whatever entry is there (a symlink is replaced, never
// followed), and the directory is fsynced. fileName is never opened for
// writing. On failure the temp file is removed.
func WriteAtomic(dirfd int, fileName string, content []byte, mode uint32, uid, gid int, tmpName string) (err error) {
	if err := checkBaseName(fileName); err != nil {
		return err
	}
	if err := checkBaseName(tmpName); err != nil {
		return err
	}
	if !strings.HasPrefix(tmpName, ".") {
		return fmt.Errorf("write %s: temporary name %q is not hidden", fileName, tmpName)
	}
	var fd int
	err = ignoringEINTR(func() (err error) {
		fd, err = unix.Openat(dirfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		return err
	})
	if err != nil {
		return fmt.Errorf("write %s: create %s: %w", fileName, tmpName, err)
	}
	created := true
	defer func() {
		if err != nil && created {
			if uerr := unlinkAt(dirfd, tmpName); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				err = errors.Join(err, fmt.Errorf("remove %s: %w", tmpName, uerr))
			}
		}
	}()
	f := os.NewFile(uintptr(fd), tmpName)
	if err := fillFile(f, content, mode, uid, gid); err != nil {
		return fmt.Errorf("write %s: %s: %w", fileName, tmpName, err)
	}
	if err := ignoringEINTR(func() error { return unix.Renameat(dirfd, tmpName, dirfd, fileName) }); err != nil {
		return fmt.Errorf("write %s: rename %s: %w", fileName, tmpName, err)
	}
	created = false
	return syncDirFd(dirfd, fileName)
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

// Restore returns m.FileName in dirfd to what Backup recorded in m. A file
// that existed gets the bytes of <opDir>/previous/content with the recorded
// mode and owner, through WriteAtomic with tmpName. A file that did not
// exist is removed. Either way the result is checked afterwards, and any
// difference (a hash, a mode, an owner, or a file that is still there) is
// an error naming what was expected and what was found: a restore is never
// reported done unless it is proven.
func Restore(dirfd int, m Meta, opDir, tmpName string) error {
	if err := checkBaseName(m.FileName); err != nil {
		return err
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
	if err := WriteAtomic(dirfd, m.FileName, content, m.Mode, int(m.UID), int(m.GID), tmpName); err != nil {
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
