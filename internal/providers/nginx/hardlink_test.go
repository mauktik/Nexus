// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Security review SEC-3: a managed site is a root-owned regular file with
// one link, and the provider is available only for an includeDir that only
// root can change.

package nginx

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// nginx-provider SEC-3 A: a file with another hard link is not managed and is never read
func TestHardlinkedFileIsUnmanaged(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("root only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	siteTree(t, f.dir, map[string]entry{"ok.conf": file(validSite)})
	if err := os.Link(outside, filepath.Join(f.dir, "h.conf")); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
	if err := os.Link(outside, filepath.Join(f.dir, "d.conf.disabled")); err != nil {
		t.Fatal(err)
	}
	s := startNginx(t, f)

	var page listPage
	s.getJSON("/providers/nginx/sites", http.StatusOK, &page)
	by := map[string]spec.SiteProperties{}
	for _, it := range decodeSites(t, page.Items) {
		by[it.Name] = it.Properties
	}
	if ok := by["ok"]; !ok.Managed || ok.SHA256 == nil {
		t.Errorf("ok = %+v, want a managed file with a sha256: one link is the normal case", ok)
	}
	for _, name := range []string{"h", "d"} {
		if p := by[name]; p.Managed || p.UnmanagedReason != spec.ReasonNotRegular || p.SHA256 != nil {
			t.Errorf("%s = %+v, want unmanaged (%s) without a sha256: it has another hard link", name, p,
				spec.ReasonNotRegular)
		}
		path := "/providers/nginx/sites/" + name + "/content"
		if p := s.problemOf(path, http.StatusUnprocessableEntity, "nginx-config-unmanaged"); p.Reason != spec.ReasonNotRegular {
			t.Errorf("%s: reason %q, want %s", path, p.Reason, spec.ReasonNotRegular)
		}
	}
}

// nginx-provider SEC-3 B: readManaged refuses a file with another hard link even when classify did not see it
func TestReadManagedRefusesAHardlinkCreatedAfterTheClassification(t *testing.T) {
	ownedByTester(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.conf"), []byte(validSite), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openIncludeDir(Settings{IncludeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := statAt(int(f.Fd()), "a.conf")
	if err != nil || st == nil {
		t.Fatalf("statAt: %v, %v", st, err)
	}
	if err := os.Link(filepath.Join(dir, "a.conf"), filepath.Join(t.TempDir(), "elsewhere")); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
	if _, _, err := readManaged(int(f.Fd()), st, 1<<20); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Errorf("readManaged = %v, want a refusal naming the hard link", err)
	}
}

// nginx-provider SEC-3 C: the provider is unavailable for an includeDir that is not root's alone to change
func TestPreflightRefusesIncludeDirOthersCanChange(t *testing.T) {
	ctx := context.Background()
	t.Run("owned by root and 0755 is available", func(t *testing.T) {
		ownedByTester(t)
		f := newFixture(t, runningSystemd())
		if st := f.p.Preflight(ctx); st.State != apitypes.ProviderEnabled {
			t.Fatalf("Preflight = %+v, want enabled", st)
		}
	})
	for _, mode := range []os.FileMode{0o775, 0o757, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			ownedByTester(t)
			f := newFixture(t, runningSystemd())
			chmod(t, f.dir, mode)
			st := f.p.Preflight(ctx)
			if st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, f.dir) ||
				!strings.Contains(st.Reason, "writable") {
				t.Errorf("Preflight = %+v, want unavailable naming %s and that it is writable by others", st, f.dir)
			}
		})
	}
	t.Run("owned by someone else", func(t *testing.T) {
		setManagedUID(t, uint32(os.Getuid())+1)
		f := newFixture(t, runningSystemd())
		st := f.p.Preflight(ctx)
		if st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, f.dir) ||
			!strings.Contains(st.Reason, "owned by") {
			t.Errorf("Preflight = %+v, want unavailable naming %s and its owner", st, f.dir)
		}
	})
}
