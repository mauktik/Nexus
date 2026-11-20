// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// rotatedLayout is the timestamp in a rotated file's name. It is fixed
// width, so name order is rotation order.
const rotatedLayout = "20060102T150405.000000000Z"

const (
	rotatedPrefix = "audit-"
	jsonlSuffix   = ".jsonl"
	gzSuffix      = ".jsonl.gz"
	tmpSuffix     = ".tmp"
)

// RotatedName is the name of the file rotated at t:
// audit-<UTC timestamp>.jsonl.gz.
func RotatedName(t time.Time) string {
	return rotatedPrefix + t.UTC().Format(rotatedLayout) + gzSuffix
}

// rotatedTime parses the timestamp of a rotated file's name, compressed or
// not yet compressed.
func rotatedTime(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, rotatedPrefix) {
		return time.Time{}, false
	}
	stem := strings.TrimPrefix(name, rotatedPrefix)
	switch {
	case strings.HasSuffix(stem, gzSuffix):
		stem = strings.TrimSuffix(stem, gzSuffix)
	case strings.HasSuffix(stem, jsonlSuffix):
		stem = strings.TrimSuffix(stem, jsonlSuffix)
	default:
		return time.Time{}, false
	}
	t, err := time.Parse(rotatedLayout, stem)
	return t, err == nil
}

// rotate moves the live file aside, opens a new one, then compresses the
// moved file. l.mu is held. The rename comes first, so a crash at any
// point leaves every record in exactly one file: recover finishes the
// compression on the next Open.
func (l *Log) rotate() error {
	at := l.now().UTC()
	if !at.After(l.lastRotated) {
		at = l.lastRotated.Add(time.Nanosecond)
	}
	for {
		_, err := os.Lstat(filepath.Join(l.dir, RotatedName(at)))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
		at = at.Add(time.Nanosecond)
	}
	live := filepath.Join(l.dir, FileName)
	moved := filepath.Join(l.dir, strings.TrimSuffix(RotatedName(at), ".gz"))
	if err := os.Rename(live, moved); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	l.lastRotated = at
	old, oldSize := l.f, l.size
	if err := l.openLive(); err != nil {
		// Keep appending to the moved file; recover compresses it later.
		l.f, l.size = old, oldSize
		return err
	}
	if err := errors.Join(old.Close(), syncDir(l.dir)); err != nil {
		l.log.Error("audit rotation", "dir", l.dir, "error", err.Error())
	}
	if err := compress(moved); err != nil {
		return err
	}
	if err := l.Prune(); err != nil {
		l.log.Error("audit retention failed", "dir", l.dir, "error", err.Error())
	}
	return nil
}

// recover finishes rotations a crash interrupted: it removes partial
// compressions and compresses every moved file that has no .gz yet.
func (l *Log) recover() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("audit directory: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(l.dir, name)
		switch {
		case strings.HasPrefix(name, rotatedPrefix) && strings.HasSuffix(name, gzSuffix+tmpSuffix):
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("audit recovery: %w", err)
			}
		case strings.HasSuffix(name, jsonlSuffix) && name != FileName:
			t, ok := rotatedTime(name)
			if !ok {
				continue
			}
			if t.After(l.lastRotated) {
				l.lastRotated = t
			}
			if err := compress(p); err != nil {
				return fmt.Errorf("audit recovery: %w", err)
			}
		default:
			if t, ok := rotatedTime(name); ok && t.After(l.lastRotated) {
				l.lastRotated = t
			}
		}
	}
	return nil
}

// compress writes src (audit-<ts>.jsonl) as audit-<ts>.jsonl.gz with mode
// 0600, durably, then removes src. A .gz that already exists is complete,
// because it is only ever created by rename.
func compress(src string) error {
	dst := src + ".gz"
	if _, err := os.Lstat(dst); err == nil {
		return removeFile(src)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("compress audit file: %w", err)
	}
	defer in.Close()
	tmp := dst + tmpSuffix
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("compress audit file: %w", err)
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("compress audit file: %w", err), removeFile(tmp))
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("compress audit file: %w", err)
	}
	return removeFile(src)
}

func removeFile(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", filepath.Base(p), err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// Dir is the audit directory, which storage accounting measures (NFR-10).
func (l *Log) Dir() string { return l.dir }

// Prune deletes the rotated files older than the retention period. A file's
// age is its newest possible record: the later of its rotation time and its
// modification time, so neither a renamed nor a touched file loses records
// younger than retention (NFR-10). The live file is never deleted. It is
// safe to call concurrently with Write: storage accounting calls it when
// over budget, and a file another call already removed is not an error.
func (l *Log) Prune() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return fmt.Errorf("audit retention: %w", err)
	}
	cutoff := l.now().Add(-l.retention)
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, gzSuffix) {
			continue
		}
		newest, ok := rotatedTime(name)
		if !ok {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if newest.Before(cutoff) {
			if err := removeFile(filepath.Join(l.dir, name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("audit retention: %w", err)
	}
	return nil
}
