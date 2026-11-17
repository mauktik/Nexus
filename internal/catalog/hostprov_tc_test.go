// Tests for planning/system-provider/test-cases.md TC-HP-104 (WI 5.1): the
// host rows of the catalog golden table.

package catalog_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	host "github.com/mauktik/nexus/internal/providers/host/spec"
	pihole "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// TC-HP-104: The catalog golden table has the ten host rows. Owner WI 5.1. Requirements: FR-14, FR-2, NFR-7.
//
// TestCatalogMatchesGoldenTable checks the rows themselves; this checks the
// count, the header comment and where the host ops sit in catalog.All().
func TestTCHP104_TheCatalogGoldenTableHasTheTen(t *testing.T) {
	// 71 rows at 47d93bc plus the ten host rows, plus the three nexusd token
	// rows (TC-RA-26) and the two config read rows (TC-RA-60) of
	// nexusd-remote-admin.
	if len(golden) != 86 {
		t.Errorf("golden has %d rows, want 86", len(golden))
	}
	src, err := os.ReadFile("golden_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(86 rows:", "the host table", "planning/system-provider/architecture.md"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("golden's header comment lacks %q", want)
		}
	}

	// The host spec comes right after pihole's in catalog.All().
	names := make([]string, 0, len(catalog.All()))
	for _, s := range catalog.All() {
		names = append(names, s.Name)
	}
	ph, h := slices.Index(names, pihole.Name), slices.Index(names, host.Name)
	if ph < 0 || h != ph+1 {
		t.Errorf("catalog.All() order %v, want %s right after %s", names, host.Name, pihole.Name)
	}

	// Filtered to provider host, the catalog is the last ten golden rows, in
	// order, and they are the host spec's ops.
	var got []row
	for _, op := range catalog.Ops() {
		if op.Provider == host.Name {
			got = append(got, rowOf(t, op))
		}
	}
	if want := golden[len(golden)-10:]; !slices.Equal(got, want) {
		t.Errorf("host rows of the catalog:\n got %v\nwant %v", got, want)
	}
	if len(got) != len(host.Spec().Ops) {
		t.Errorf("catalog has %d host ops, the spec %d", len(got), len(host.Spec().Ops))
	}
	for _, r := range got {
		if r.scope != host.ReadScope || r.risk != def.RiskReadOnly || r.mode != def.ModeSync {
			t.Errorf("%s: scope %q risk %s mode %s, want %s read-only sync", r.id, r.scope, r.risk, r.mode, host.ReadScope)
		}
	}
}
