// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/systemspec"
)

func seedRotated(t *testing.T, dir string, at time.Time) string {
	t.Helper()
	name := audit.RotatedName(at)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"id":"x","outcome":"denied"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return name
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TC-37: rotation at 16 MiB and retention by age.
func TestRotationAndRetention(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	const days = 90
	retention := days * 24 * time.Hour
	oldName := seedRotated(t, dir, clk.Now().Add(-retention-time.Hour))
	youngName := seedRotated(t, dir, clk.Now().Add(-retention+time.Hour))

	l := open(t, audit.Options{Dir: dir, Now: clk.Now, RetentionDays: days})
	if audit.DefaultRotateBytes != 16<<20 {
		t.Fatalf("DefaultRotateBytes = %d, want 16 MiB (A-54)", audit.DefaultRotateBytes)
	}
	pad := strings.Repeat("p", 64<<10)
	params, _ := json.Marshal(map[string]string{"pad": pad})
	written := 0
	for written <= audit.DefaultRotateBytes+(256<<10) {
		if _, err := l.Write(systemspec.AuditRecord{Action: "t.op.fill", Params: params, Outcome: audit.OutcomeRequested}, nil); err != nil {
			t.Fatalf("Write: %v", err)
		}
		written += len(params) + 200
	}

	live := filepath.Join(dir, audit.FileName)
	if fi, err := os.Stat(live); err != nil || fi.Size() >= audit.DefaultRotateBytes {
		t.Fatalf("live file after rotation: %v, %v", fi, err)
	}
	if m := mode(t, live); m != 0o600 {
		t.Fatalf("live file mode %o, want 600", m)
	}
	rotated := audit.RotatedName(clk.Now())
	p := filepath.Join(dir, rotated)
	if m := mode(t, p); m != 0o600 {
		t.Fatalf("rotated file mode %o, want 600", m)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("no %s after writing past 16 MiB: %v", rotated, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	lines := 0
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		size += int64(len(sc.Bytes()) + 1)
		var r systemspec.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Action != "t.op.fill" {
			t.Fatalf("rotated line %d is not a valid record: %v", lines+1, err)
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, zr); err != nil {
		t.Fatal(err)
	}
	if lines == 0 || size > audit.DefaultRotateBytes {
		t.Fatalf("rotated file: %d lines, %d bytes", lines, size)
	}
	if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(rotated, ".gz"))); !os.IsNotExist(err) {
		t.Fatalf("the uncompressed rotated file remains: %v", err)
	}

	// Retention: advance the clock so the young seed is still inside the
	// window, then prune.
	clk.Add(30 * time.Minute)
	if err := l.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if slices.Contains(names, oldName) {
		t.Fatalf("%s is older than retention and was kept: %v", oldName, names)
	}
	for _, want := range []string{youngName, rotated, audit.FileName} {
		if !slices.Contains(names, want) {
			t.Fatalf("%s is younger than retention and was deleted: %v", want, names)
		}
	}

	// A young name with an old mtime is kept: deletion needs both to be old.
	clk.Add(time.Hour)
	mixed := seedRotated(t, dir, clk.Now().Add(-retention+time.Minute))
	old := clk.Now().Add(-2 * retention)
	if err := os.Chtimes(filepath.Join(dir, mixed), old, old); err != nil {
		t.Fatal(err)
	}
	if err := l.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, mixed)); err != nil {
		t.Fatalf("a file whose name is inside retention was deleted: %v", err)
	}
	// The young seed has now aged past retention.
	if _, err := os.Stat(filepath.Join(dir, youngName)); !os.IsNotExist(err) {
		t.Fatalf("%s aged past retention and was kept: %v", youngName, err)
	}
}
