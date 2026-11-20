// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-9 (WI 1.1).

package capturegate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recorder is a testing.TB whose Helper, Skipf and Fatalf are recorded.
// Skipf and Fatalf end the calling goroutine with runtime.Goexit, as the real
// ones do, so Require never returns after either; the real test is neither
// skipped nor failed.
type recorder struct {
	*testing.T
	helper bool
	skips  []string
	fatals []string
}

func (r *recorder) Helper() { r.helper = true }

func (r *recorder) Skipf(format string, args ...any) {
	r.skips = append(r.skips, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// call runs Require(rec, path) in its own goroutine and waits for it. It
// returns the recorder and Require's result ("" when Require did not return).
func call(t *testing.T, path string) (*recorder, string) {
	t.Helper()
	rec := &recorder{T: t}
	var got string
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = Require(rec, path)
	}()
	<-done
	return rec, got
}

// TC-STG-9: capturegate.Require skips an absent capture, fails it under
// NEXUS_STORAGE_REQUIRE_CAPTURES=1, and imports nothing of this module.
// Owner WI 1.1. Requirements: NFR-8a, NFR-8.
func TestTCSTG9_CapturegateRequireSkipsAnAbsentCaptureFails(t *testing.T) {
	if EnvRequire != "NEXUS_STORAGE_REQUIRE_CAPTURES" {
		t.Fatalf("EnvRequire = %q", EnvRequire)
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(tmp, "f")
	d := filepath.Join(tmp, "d")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(tmp, "absent")
	want := "operator capture pending (WI 1.2): " + absent

	t.Run("present file and directory", func(t *testing.T) {
		t.Setenv(EnvRequire, "")
		for _, p := range []string{f, d} {
			rec, got := call(t, p)
			if got != p {
				t.Errorf("Require(%s) = %q, want the path", p, got)
			}
			if len(rec.skips) != 0 || len(rec.fatals) != 0 {
				t.Errorf("Require(%s): skips %q, fatals %q, want none", p, rec.skips, rec.fatals)
			}
			if !rec.helper {
				t.Errorf("Require(%s) did not call Helper", p)
			}
		}
	})

	for _, v := range []string{"", "0", "true"} {
		t.Run("absent with "+fmt.Sprintf("%q", v), func(t *testing.T) {
			t.Setenv(EnvRequire, v)
			rec, got := call(t, absent)
			if got != "" {
				t.Errorf("Require returned %q after Skipf", got)
			}
			if len(rec.fatals) != 0 {
				t.Errorf("fatals %q, want none", rec.fatals)
			}
			if len(rec.skips) != 1 || rec.skips[0] != want {
				t.Errorf("skips %q, want exactly [%q]", rec.skips, want)
			}
		})
	}

	t.Run("absent with 1", func(t *testing.T) {
		t.Setenv(EnvRequire, "1")
		rec, _ := call(t, absent)
		if len(rec.skips) != 0 {
			t.Errorf("skips %q, want none", rec.skips)
		}
		if len(rec.fatals) != 1 || rec.fatals[0] != want {
			t.Errorf("fatals %q, want exactly [%q]", rec.fatals, want)
		}
	})

	t.Run("stat error other than not-exist", func(t *testing.T) {
		t.Setenv(EnvRequire, "")
		p := filepath.Join(f, "x")
		_, statErr := os.Stat(p)
		if statErr == nil {
			t.Fatalf("stat %s succeeded", p)
		}
		rec, _ := call(t, p)
		if len(rec.skips) != 0 {
			t.Errorf("skips %q, want none", rec.skips)
		}
		if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], p) || !strings.Contains(rec.fatals[0], statErr.Error()) {
			t.Errorf("fatals %q, want one naming %s and %q", rec.fatals, p, statErr)
		}
	})

	t.Run("a leaf package", func(t *testing.T) {
		if _, err := exec.LookPath("go"); err != nil {
			t.Skip("go not on PATH")
		}
		out, err := exec.Command("go", "list", "-deps", ".").Output()
		if err != nil {
			t.Fatalf("go list -deps: %v", err)
		}
		self := "github.com/mauktik/nexus/internal/providers/storage/capturegate"
		for _, pkg := range strings.Fields(string(out)) {
			if strings.HasPrefix(pkg, "github.com/mauktik/nexus/") && pkg != self {
				t.Errorf("capturegate depends on %s", pkg)
			}
		}
	})
}
