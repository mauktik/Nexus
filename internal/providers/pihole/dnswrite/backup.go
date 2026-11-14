package dnswrite

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The pre-Nexus backup (FR-23, NFR-7): the DNS file's bytes as they were
// before Nexus first wrote it, kept forever outside /etc/dnsmasq.d (CON-6).
const (
	backupDirName = "backups"
	backupSuffix  = ".pre-nexus"
	backupDirMode = 0o700
	backupMode    = 0o600
)

// PreNexusBackupPath is where the first write of dnsFile saves its
// original bytes: <stateDir>/backups/<basename>.pre-nexus. Its metadata is
// beside it, with ".json" appended.
func PreNexusBackupPath(stateDir, dnsFile string) string {
	return filepath.Join(stateDir, backupDirName, filepath.Base(dnsFile)+backupSuffix)
}

// backupMeta is the .pre-nexus.json beside the backup.
type backupMeta struct {
	UID       int       `json:"uid"`
	GID       int       `json:"gid"`
	Mode      string    `json:"mode"` // Octal permission bits, such as "0644"
	Size      int       `json:"size"`
	SHA256    string    `json:"sha256"` // Hex of the original bytes
	CreatedAt time.Time `json:"createdAt"`
}

// ensurePreNexusBackup creates the pre-Nexus backup of orig unless it
// exists, and reports whether this call created it. A backup that exists is
// never touched. Creation is O_EXCL, mode 0600 in a 0700 directory; the
// backup and its metadata are fsynced, and the backup is read back and its
// SHA-256 compared with orig's. On any failure the files this call created
// are removed, so a later write tries again, and the error is returned
// before the DNS file is written.
func ensurePreNexusBackup(stateDir, dnsFile string, orig []byte, o owner, now time.Time) (string, bool, error) {
	path := PreNexusBackupPath(stateDir, dnsFile)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, backupDirMode); err != nil {
		return "", false, fmt.Errorf("create %s: %w", dir, err)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		return "", false, err
	}
	if !di.IsDir() {
		return "", false, fmt.Errorf("%s is not a directory", dir)
	}
	if di.Mode().Perm() != backupDirMode {
		if err := os.Chmod(dir, backupDirMode); err != nil {
			return "", false, err
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return path, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}

	sum := sha256.Sum256(orig)
	meta, err := json.MarshalIndent(backupMeta{
		UID: o.uid, GID: o.gid, Mode: fmt.Sprintf("%#o", o.mode.Perm()), Size: len(orig),
		SHA256: hex.EncodeToString(sum[:]), CreatedAt: now.UTC(),
	}, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := createExclusive(path, orig); err != nil {
		return "", false, err
	}
	if err := finishBackup(path, sum, append(meta, '\n'), dir); err != nil {
		return "", false, errors.Join(err, os.Remove(path), removeIfExists(path+".json"))
	}
	return path, true, nil
}

// finishBackup reads the new backup back and compares its SHA-256, writes
// the metadata, and fsyncs the directory.
func finishBackup(path string, sum [sha256.Size]byte, meta []byte, dir string) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back %s: %w", path, err)
	}
	if back := sha256.Sum256(got); !bytes.Equal(back[:], sum[:]) {
		return fmt.Errorf("the backup %s read back differs from the original (sha256 %x, want %x)", path, back, sum)
	}
	if err := writeSynced(path+".json", meta); err != nil {
		return err
	}
	return syncDir(dir)
}

// createExclusive creates path with O_EXCL, mode 0600, writes b and fsyncs
// it. A file it created and could not complete is removed.
func createExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupMode)
	if err != nil {
		return err
	}
	if err := writeSyncClose(f, b); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

// writeSynced writes b to path, mode 0600, replacing what is there, and
// fsyncs it.
func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, backupMode)
	if err != nil {
		return err
	}
	if err := f.Chmod(backupMode); err != nil {
		return errors.Join(err, f.Close())
	}
	return writeSyncClose(f, b)
}

// writeSyncClose writes b to f, fsyncs and closes it.
func writeSyncClose(f *os.File, b []byte) error {
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", f.Name(), err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync %s: %w", f.Name(), err), f.Close())
	}
	return f.Close()
}

// syncDir fsyncs a directory, so a create or rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fsync %s: %w", dir, err), d.Close())
	}
	return d.Close()
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
