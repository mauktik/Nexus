// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package reloadstatus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TC-RA-55: reloadstatus writes atomically with mode 0600, reads back,
// bounds the read and orders statuses by boot and seq.
func TestTCRA55_ReloadstatusWritesAtomicallyWithMode0600Reads(t *testing.T) {
	dir := t.TempDir()

	// Step 1.
	got, ok, err := Read(dir)
	if ok || err != nil {
		t.Fatalf("step 1: Read on an empty dir = %+v, %v, %v; want ok false and a nil error", got, ok, err)
	}

	// Steps 2 and 3.
	s1 := Status{Boot: "b1", Seq: 1, At: time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC),
		SHA256: strings.Repeat("ab", 32), Applied: true, RestartRequired: []string{}}
	if err := Write(dir, s1); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	got, ok, err = Read(dir)
	if err != nil || !ok {
		t.Fatalf("step 3: Read = %v, %v", ok, err)
	}
	if !reflect.DeepEqual(got, s1) {
		t.Fatalf("step 3: read %+v, want %+v", got, s1)
	}
	path := filepath.Join(dir, FileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("step 3: mode %04o, want 0600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"applied", "at", "boot", "restartRequired", "seq", "sha256"}; !slices.Equal(keys, want) {
		t.Errorf("step 3: JSON members %v, want %v", keys, want)
	}
	if string(members["restartRequired"]) != "[]" {
		t.Errorf("step 3: restartRequired is %s, want []", members["restartRequired"])
	}

	// Stage and error appear when set; a nil RestartRequired is still [].
	failed := Status{Boot: "b1", Seq: 2, SHA256: "", Stage: "load", Error: "cannot read configuration"}
	if err := Write(dir, failed); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members = nil
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	if _, ok := members["stage"]; !ok {
		t.Error("stage omitted although set")
	}
	if _, ok := members["error"]; !ok {
		t.Error("error omitted although set")
	}
	if string(members["restartRequired"]) != "[]" {
		t.Errorf("a nil restartRequired is written as %s, want []", members["restartRequired"])
	}

	// Step 4.
	big := `{"boot":"` + strings.Repeat("x", 65<<10) + `"}`
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(dir); err == nil {
		t.Error("step 4: no error for a 65 KiB file; the read must be bounded at 64 KiB")
	}

	// Step 5.
	s2 := Status{Boot: "b1", Seq: 2}
	other := Status{Boot: "b2", Seq: 1}
	for _, tc := range []struct {
		name    string
		s, base Status
		hadBase bool
		want    bool
	}{
		{"S2 after S1", s2, s1, true, true},
		{"{b2,1} after S1", other, s1, true, true},
		{"S1 after S1", s1, s1, true, false},
		{"any after no base", s1, Status{}, false, true},
	} {
		if got := tc.s.After(tc.base, tc.hadBase); got != tc.want {
			t.Errorf("step 5: %s = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Cleanup: only the status file is left.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only %s", names, FileName)
	}
}

// A failed write leaves no temporary file behind.
func TestWriteFailureLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, Status{Boot: "b", Seq: 1}); err == nil {
		t.Fatal("Write over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir holds %d entries after a failed write, want only the directory", len(entries))
	}
}

// Read refuses anything but a regular file, and never blocks on a FIFO.
func TestReadRefusesNonRegularFiles(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, FileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Read(dir); err == nil {
			t.Fatal("no error for a directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		if err := os.WriteFile(target, []byte(`{"boot":"b","seq":1,"restartRequired":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, FileName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Read(dir); err == nil {
			t.Fatal("no error for a symbolic link")
		}
	})
}

// TC-RA-E15: concurrent reads of reload-status during writes never see
// partial JSON.
func TestTCRAE15_ConcurrentReadsOfReloadStatusDuringWritesNever(t *testing.T) {
	dir := t.TempDir()
	const writes, readers = 1000, 4
	done := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	for range readers {
		wg.Go(func() {
			var last uint64
			for {
				s, ok, err := Read(dir)
				if err != nil {
					errs <- err.Error()
					return
				}
				if ok {
					if s.Seq < last {
						errs <- "seq went from " + itoa(last) + " to " + itoa(s.Seq)
						return
					}
					last = s.Seq
				}
				select {
				case <-done:
					return
				default:
				}
			}
		})
	}
	for i := uint64(1); i <= writes; i++ {
		if err := Write(dir, Status{Boot: "b", Seq: i, Applied: true, RestartRequired: []string{}}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	s, ok, err := Read(dir)
	if err != nil || !ok || s.Seq != writes {
		t.Fatalf("final Read = %+v, %v, %v; want seq %d", s, ok, err, writes)
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// Write cuts a long Error so the file stays within Read's bound, and never
// splits a UTF-8 sequence.
func TestWriteBoundsTheError(t *testing.T) {
	dir := t.TempDir()
	long := "x" + strings.Repeat("é", MaxBytes)
	if err := Write(dir, Status{Boot: "b", Seq: 1, Stage: "load", Error: long}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("Read = %v, %v", ok, err)
	}
	if len(s.Error) > MaxErrorBytes || !utf8.ValidString(s.Error) || !strings.HasPrefix(long, s.Error) {
		t.Fatalf("error of %d bytes (valid UTF-8 %v), want a valid prefix of at most %d", len(s.Error), utf8.ValidString(s.Error), MaxErrorBytes)
	}
	if len(s.Error) < MaxErrorBytes-1 {
		t.Fatalf("error cut to %d bytes, want %d or one less", len(s.Error), MaxErrorBytes)
	}
}
