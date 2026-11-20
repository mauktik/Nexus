// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-200.
// Owner WIs: 5.1 (the storage line of catalog.All() and its golden rows).

package catalog_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/def"
	storage "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// goldenBase is the commit the storage branch's golden table is diffed
// against.
const goldenBase = "43e5792"

// goldenLines are the row lines of golden_test.go's golden table, as text.
func goldenLines(t *testing.T, src string) []string {
	t.Helper()
	_, table, ok := strings.Cut(src, "var golden = []row{\n")
	if !ok {
		t.Fatal("golden_test.go has no golden table")
	}
	table, _, _ = strings.Cut(table, "\n}\n")
	var out []string
	for _, l := range strings.Split(table, "\n") {
		if strings.HasPrefix(l, "\t{\"") {
			out = append(out, l)
		}
	}
	return out
}

// archRows are the storage rows of the Operation catalog table of
// planning/storage-provider/architecture.md, as golden rows.
func archRows(t *testing.T, text string) []row {
	t.Helper()
	_, section, ok := strings.Cut(text, "### Operation catalog (golden rows)")
	if !ok {
		t.Fatal("architecture.md has no Operation catalog section")
	}
	reRoute := regexp.MustCompile("(GET|POST) (/[^` ]*)")
	unquote := func(s string) string { return strings.Trim(strings.TrimSpace(s), "`") }
	var out []row
	for _, l := range strings.Split(section, "\n") {
		if !strings.HasPrefix(l, "| `storage.") {
			continue
		}
		f := strings.Split(strings.Trim(l, "|"), "|")
		if len(f) != 8 {
			t.Fatalf("architecture.md row %q has %d cells, want 8", l, len(f))
		}
		m := reRoute.FindStringSubmatch(f[1])
		if m == nil {
			t.Fatalf("architecture.md row %q has no route", l)
		}
		tool := unquote(f[7])
		if tool == "–" {
			tool = ""
		}
		out = append(out, row{id: unquote(f[0]), route: m[1] + " /providers/storage" + m[2], scope: unquote(f[4]),
			risk: def.Risk(strings.TrimSpace(f[3])), mode: def.Mode(strings.TrimSpace(f[2])), cli: unquote(f[6]), tool: tool})
	}
	return out
}

// TC-STG-200: the catalog golden table gains exactly the twelve storage rows. Owner WI 5.1. Requirements: FR-1, FR-17, CON-4.
func TestTCSTG200_TheCatalogGoldenTableGainsExactlyThe(t *testing.T) {
	head, err := os.ReadFile("golden_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// rc7 integration: 87 base rows, services' 8 and storage's 12.
	if !strings.Contains(string(head), "(107 rows:") || len(golden) != 107 {
		t.Errorf("golden has %d rows, its header %q; want 107 and (107 rows:", len(golden), "(107 rows:")
	}
	// The storage rows are the last twelve, after services' eight, so the
	// host-row slice of hostprov_tc_test.go, golden[len(golden)-30 :
	// len(golden)-20], still selects the ten host rows.
	for i, r := range golden[len(golden)-30 : len(golden)-20] {
		if !strings.HasPrefix(r.id, "host.") {
			t.Errorf("host-row slice entry %d is %s", i, r.id)
		}
	}
	stg := golden[len(golden)-12:]
	for _, r := range stg {
		if !strings.HasPrefix(r.id, storage.Name+".") {
			t.Errorf("trailing row %s is not a storage row", r.id)
		}
	}
	var specIDs []string
	for _, op := range storage.Spec().Ops {
		specIDs = append(specIDs, op.ID)
	}
	var rowIDs []string
	for _, r := range stg {
		rowIDs = append(rowIDs, r.id)
	}
	if !slices.Equal(rowIDs, specIDs) {
		t.Errorf("storage rows %v, want the spec's ops %v", rowIDs, specIDs)
	}

	// The rows are the architecture's Operation catalog.
	arch, err := os.ReadFile(filepath.Join("..", "..", "planning", "storage-provider", "architecture.md"))
	switch {
	case os.IsNotExist(err):
		t.Log("no planning/ directory (a repository copy): the architecture table is not compared")
	case err != nil:
		t.Fatal(err)
	default:
		if want := archRows(t, string(arch)); !slices.Equal(stg, want) {
			t.Errorf("storage golden rows:\n got %v\nwant %v (architecture.md)", stg, want)
		}
	}

	// Against the base: every base row byte for byte, and only the twelve
	// storage rows added.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH: the golden table is not diffed against " + goldenBase)
	}
	show := exec.Command("git", "show", goldenBase+":internal/catalog/golden_test.go")
	base, err := show.Output()
	if err != nil {
		t.Skipf("base %s not reachable (shallow clone?): %v", goldenBase, err)
	}
	baseRows, headRows := goldenLines(t, string(base)), goldenLines(t, string(head))
	if len(baseRows) != 87 || len(headRows) != len(golden) {
		t.Fatalf("parsed %d base and %d head rows, want 87 and %d", len(baseRows), len(headRows), len(golden))
	}
	var added []string
	for _, l := range headRows {
		if strings.HasPrefix(l, "\t{\"services.") {
			continue // rc7 integration: the sibling services provider's rows (its TC-SV-4 counts them)
		}
		if !slices.Contains(baseRows, l) {
			added = append(added, l)
		}
	}
	for _, l := range baseRows {
		if !slices.Contains(headRows, l) {
			t.Errorf("base row changed or removed: %s", l)
		}
	}
	if len(added) != 12 {
		t.Errorf("%d rows added, want 12: %v", len(added), added)
	}
	for _, l := range added {
		if !strings.HasPrefix(l, "\t{\""+storage.Name+".") {
			t.Errorf("added row is not a storage row: %s", l)
		}
	}
}
