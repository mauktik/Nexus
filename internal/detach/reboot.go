// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RebootFile is the record, in an operation's directory, that the
// operation's helper asked the host to reboot. Its presence is what lets an
// op declaring def.Op.CompletesAcrossReboot complete in a later boot.
const RebootFile = "reboot-requested"

// maxRebootRecord bounds how much of RebootFile ReadReboot reads. A real
// record is well under 200 bytes.
const maxRebootRecord = 4 << 10

// RebootRecord is the content of RebootFile.
type RebootRecord struct {
	BootID      string    `json:"bootId"`      // The boot the request was recorded in
	RequestedAt time.Time `json:"requestedAt"` // When the helper recorded it
}

// RecordReboot writes r as dir/reboot-requested, mode 0600, through a
// temporary file, fsync, rename and a directory fsync, so a reader sees the
// previous record or the new one, never part of one. It replaces any record
// already there.
func RecordReboot(dir string, r RebootRecord) error {
	if r.BootID == "" {
		return errors.New("detach: record reboot: empty boot id")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("detach: encode reboot record: %w", err)
	}
	return writeAtomic(dir, RebootFile, b)
}

// RetractReboot removes dir/reboot-requested and syncs dir. A record that is
// already absent is not an error.
func RetractReboot(dir string) error {
	err := os.Remove(filepath.Join(dir, RebootFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("detach: retract %s: %w", RebootFile, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("detach: retract %s: %w", RebootFile, err)
	}
	return nil
}

// ReadReboot reads dir/reboot-requested. ok is false, with a nil error, only
// when there is no record. A record that exists but cannot be read or decoded
// is an error with ok true: the reboot may have been requested, so a caller
// must never take it for no record.
func ReadReboot(dir string) (r RebootRecord, ok bool, err error) {
	path := filepath.Join(dir, RebootFile)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return RebootRecord{}, false, nil
	}
	if err != nil {
		return RebootRecord{}, true, fmt.Errorf("detach: read reboot record %s: %w", path, err)
	}
	b, rerr := io.ReadAll(io.LimitReader(f, maxRebootRecord+1))
	if cerr := f.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return RebootRecord{}, true, fmt.Errorf("detach: read reboot record %s: %w", path, rerr)
	}
	if len(b) > maxRebootRecord {
		return RebootRecord{}, true, fmt.Errorf("detach: reboot record %s is larger than %d bytes", path, maxRebootRecord)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return RebootRecord{}, true, fmt.Errorf("detach: decode reboot record %s: %w", path, err)
	}
	if r.BootID == "" {
		return RebootRecord{}, true, fmt.Errorf("detach: reboot record %s has no boot id", path)
	}
	return r, true, nil
}
