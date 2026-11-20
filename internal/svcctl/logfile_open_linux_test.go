//go:build linux

package svcctl

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// SEC-2: TailFile refuses a symbolic link in the path, and a hard-linked
// file, through openat2(2) and through the walk that stands in when the
// kernel or a seccomp filter has no openat2.
func TestSEC2_TailFileOnBothOpenPaths(t *testing.T) {
	for _, walkOnly := range []bool{false, true} {
		name := "openat2"
		if walkOnly {
			name = "walk"
		}
		t.Run(name, func(t *testing.T) {
			noOpenat2 = walkOnly
			t.Cleanup(func() { noOpenat2 = false })
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			real := filepath.Join(dir, "real")
			if err := os.MkdirAll(real, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(real, "app.log")
			if err := os.WriteFile(file, []byte("a\nb\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := TailFile(file, 5, 1<<20); err != nil || !slices.Equal(got, []string{"a", "b"}) {
				t.Fatalf("TailFile(file) = %q, %v; want a and b", got, err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			if got, err := TailFile(filepath.Join(link, "app.log"), 5, 1<<20); err == nil ||
				!strings.Contains(err.Error(), "symbolic link") || !errors.Is(err, syscall.ELOOP) {
				t.Errorf("TailFile(through a link) = %q, %v; want a symbolic-link error wrapping ELOOP", got, err)
			}
			if err := os.Link(file, filepath.Join(real, "second")); err != nil {
				t.Fatal(err)
			}
			if got, err := TailFile(file, 5, 1<<20); err == nil || !strings.Contains(err.Error(), "hard links") {
				t.Errorf("TailFile(hard-linked) = %q, %v; want a hard-links error", got, err)
			}
		})
	}
}
