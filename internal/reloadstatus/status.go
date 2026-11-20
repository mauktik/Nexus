// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package reloadstatus is the outcome of nexusd's last configuration reload,
// <stateDir>/reload-status.json. The daemon writes it after every reload,
// SIGHUP or API (Daemon.reload); the config-set service helper reads it to
// learn whether the reload it triggered with `systemctl reload` applied the
// bytes it wrote. It imports nothing of nexusd's, so both sides can use it
// without an import cycle.
package reloadstatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"
)

// FileName is the status file in the daemon's state directory, mode 0600.
const FileName = "reload-status.json"

// fileMode is the status file's mode.
const fileMode = 0o600

// MaxBytes bounds Read. A status Write produces is far smaller: MaxErrorBytes
// bounds its only free-form member.
const MaxBytes = 64 << 10

// MaxErrorBytes bounds Status.Error as Write stores it, so a reload that
// fails with a long error list still yields a file Read accepts.
const MaxErrorBytes = 8 << 10

// Status is one reload's outcome.
type Status struct {
	Boot            string    `json:"boot"` // random per daemon start
	Seq             uint64    `json:"seq"`  // reloads since start, from 1
	At              time.Time `json:"at"`
	SHA256          string    `json:"sha256"` // hex digest of the bytes read; "" when unreadable
	Applied         bool      `json:"applied"`
	Stage           string    `json:"stage,omitempty"` // load | apply
	Error           string    `json:"error,omitempty"`
	RestartRequired []string  `json:"restartRequired"`
}

// Write replaces the status file in dir with s: a 0600 temporary file in dir
// is written and fsynced, renamed over the file, and dir is fsynced, so a
// reader sees the previous status or s, never part of one. A nil
// RestartRequired is written as [], and Error is cut to MaxErrorBytes.
func Write(dir string, s Status) (err error) {
	if s.RestartRequired == nil {
		s.RestartRequired = []string{}
	}
	s.Error = truncate(s.Error, MaxErrorBytes)
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode reload status: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+FileName+".*")
	if err != nil {
		return fmt.Errorf("create temporary reload status: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			// Best effort: the write already failed and is reported.
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	// CreateTemp already uses 0600; Chmod makes the mode independent of it.
	if err := tmp.Chmod(fileMode); err != nil {
		return fmt.Errorf("chmod temporary reload status: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary reload status: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary reload status: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary reload status: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("replace reload status: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync state directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}

// Read returns the status in dir. ok is false, with a nil error, when there
// is none yet. The file must be a regular file of at most MaxBytes; a
// symbolic link is refused, and the open never blocks, so a reader running
// as root cannot be pointed elsewhere or stalled through the state
// directory.
func Read(dir string) (s Status, ok bool, err error) {
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("open reload status: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close reload status: %w", cerr)
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return Status{}, false, fmt.Errorf("stat reload status: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Status{}, false, errors.New("reload status is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return Status{}, false, fmt.Errorf("read reload status: %w", err)
	}
	if len(data) > MaxBytes {
		return Status{}, false, fmt.Errorf("reload status is larger than %d bytes", MaxBytes)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, false, fmt.Errorf("decode reload status: %w", err)
	}
	return s, true, nil
}

// After reports whether s is a reload other than base: true when there was
// no base, or when (Boot, Seq) differs from base's. A new daemon start
// counts even though its Seq starts again from 1.
func (s Status) After(base Status, hadBase bool) bool {
	return !hadBase || s.Boot != base.Boot || s.Seq != base.Seq
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// s[n] is the first byte dropped; while it continues a sequence, drop
	// that sequence's earlier bytes too.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
