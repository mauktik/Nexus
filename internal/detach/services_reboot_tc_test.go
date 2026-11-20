// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-5, TC-SV-6,
// TC-SV-E1, TC-SV-ERR4 (WI 1.2): the reboot record of an operation's
// directory.

package detach_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/detach"
)

const (
	tcsvPrevBoot = "11111111-1111-1111-1111-111111111111"
	tcsvCurBoot  = "22222222-2222-2222-2222-222222222222"
)

func tcsvRecord(boot string) detach.RebootRecord {
	return detach.RebootRecord{BootID: boot, RequestedAt: time.Date(2026, 11, 18, 9, 0, 0, 123456789, time.UTC)}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TC-SV-5 (FR-11, CON-5): RecordReboot writes the record atomically with
// mode 0600 and ReadReboot reads it back.
func TestTCSV5_RecordRebootWritesTheRecordAtomicallyWithMode(t *testing.T) {
	dir := t.TempDir()
	rec := tcsvRecord(tcsvPrevBoot)

	got, ok, err := detach.ReadReboot(dir)
	if got != (detach.RebootRecord{}) || ok || err != nil {
		t.Fatalf("ReadReboot before a write = (%+v, %v, %v), want (zero, false, nil)", got, ok, err)
	}

	if err := detach.RecordReboot(dir, rec); err != nil {
		t.Fatalf("RecordReboot: %v", err)
	}
	if detach.RebootFile != "reboot-requested" {
		t.Errorf("RebootFile = %q, want reboot-requested", detach.RebootFile)
	}
	path := filepath.Join(dir, detach.RebootFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != detach.RebootFile {
		t.Errorf("directory holds %q, want exactly [%s]", names, detach.RebootFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("record is not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "bootId,requestedAt" {
		t.Errorf("record keys = %v, want [bootId requestedAt]", keys)
	}

	got, ok, err = detach.ReadReboot(dir)
	if err != nil || !ok {
		t.Fatalf("ReadReboot = (%+v, %v, %v), want the record", got, ok, err)
	}
	if got.BootID != rec.BootID || !got.RequestedAt.Equal(rec.RequestedAt) || got.RequestedAt.Nanosecond() != 123456789 {
		t.Errorf("ReadReboot = %+v, want %+v to the nanosecond", got, rec)
	}

	cur := tcsvRecord(tcsvCurBoot)
	if err := detach.RecordReboot(dir, cur); err != nil {
		t.Fatalf("second RecordReboot: %v", err)
	}
	got, ok, err = detach.ReadReboot(dir)
	if err != nil || !ok || got.BootID != tcsvCurBoot || !got.RequestedAt.Equal(cur.RequestedAt) {
		t.Errorf("ReadReboot after replace = (%+v, %v, %v), want the %s record", got, ok, err, tcsvCurBoot)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("directory holds %q after replace, want one file", names)
	}
}

// TC-SV-6 (FR-11, CON-5): RetractReboot removes the record and is
// idempotent.
func TestTCSV6_RetractRebootRemovesTheRecordAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := detach.RetractReboot(dir); err != nil {
		t.Errorf("RetractReboot with no record = %v, want nil", err)
	}
	if err := detach.RecordReboot(dir, tcsvRecord(tcsvPrevBoot)); err != nil {
		t.Fatalf("RecordReboot: %v", err)
	}
	if err := detach.RetractReboot(dir); err != nil {
		t.Fatalf("RetractReboot: %v", err)
	}
	if _, ok, err := detach.ReadReboot(dir); ok || err != nil {
		t.Errorf("ReadReboot after retract = (ok %v, %v), want (false, nil)", ok, err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory holds %q after retract, want nothing", names)
	}
	if err := detach.RetractReboot(dir); err != nil {
		t.Errorf("second RetractReboot = %v, want nil", err)
	}
}

// TC-SV-E1 (FR-11, CON-5): RecordReboot under a concurrent reader never
// exposes a partial record.
func TestTCSVE1_RecordRebootUnderAConcurrentReaderNeverExposes(t *testing.T) {
	dir := t.TempDir()
	prev, cur := tcsvRecord(tcsvPrevBoot), tcsvRecord(tcsvCurBoot)
	if err := detach.RecordReboot(dir, prev); err != nil {
		t.Fatalf("first RecordReboot: %v", err)
	}

	var done atomic.Bool
	var reads atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				got, ok, err := detach.ReadReboot(dir)
				reads.Add(1)
				switch {
				case err != nil:
					errs <- "read error: " + err.Error()
					return
				case !ok:
					errs <- "ok == false after the first write"
					return
				case !got.RequestedAt.Equal(prev.RequestedAt) || (got.BootID != tcsvPrevBoot && got.BootID != tcsvCurBoot):
					errs <- "unexpected record: " + got.BootID
					return
				}
			}
		}()
	}
	for i := range 1000 {
		r := prev
		if i%2 == 1 {
			r = cur
		}
		if err := detach.RecordReboot(dir, r); err != nil {
			done.Store(true)
			wg.Wait()
			t.Fatalf("RecordReboot %d: %v", i, err)
		}
	}
	done.Store(true)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if reads.Load() == 0 {
		t.Error("no reader ran")
	}
}

// TC-SV-ERR4 (FR-11, CON-5): ReadReboot on a corrupt record is an error, and
// RecordReboot to a missing directory fails.
func TestTCSVERR4_ReadRebootOnACorruptRecordIsAn(t *testing.T) {
	for name, content := range map[string][]byte{
		"truncated JSON": []byte(`{"bootId":`),
		"1 MiB of a":     []byte(strings.Repeat("a", 1<<20)),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, detach.RebootFile), content, 0o600); err != nil {
				t.Fatal(err)
			}
			got, ok, err := detach.ReadReboot(dir)
			if err == nil {
				t.Fatalf("ReadReboot = (%+v, %v, nil), want an error", got, ok)
			}
			if !ok {
				t.Errorf("ReadReboot ok = false for a present but corrupt record, want true")
			}
			if !strings.Contains(err.Error(), detach.RebootFile) {
				t.Errorf("error %q does not name %s", err, detach.RebootFile)
			}
		})
	}

	t.Run("missing directory", func(t *testing.T) {
		parent := t.TempDir()
		missing := filepath.Join(parent, "absent")
		if err := detach.RecordReboot(missing, tcsvRecord(tcsvPrevBoot)); err == nil {
			t.Error("RecordReboot to a missing directory = nil, want an error")
		}
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Errorf("Lstat(missing) = %v, want not-exist", err)
		}
		if names := dirNames(t, parent); len(names) != 0 {
			t.Errorf("parent holds %q, want nothing created", names)
		}
	})
}
