package detach

import (
	"slices"
	"testing"
)

// The real unit lister keeps only operation unit names from list-units
// output: the legend's header and footer, a bullet circle if one is
// printed, and other units are skipped.
func TestUnitNamesFromListUnits(t *testing.T) {
	out := []byte(`UNIT                                                  LOAD      ACTIVE   SUB     DESCRIPTION
nexus-op-01a1b779-8149-7035-a5a5-8df4e45d4beb.service loaded    active   running Nexus operation 01a1b779-8149-7035-a5a5-8df4e45d4beb
● nexus-op-x.service                                   not-found inactive dead    nexus-op-x.service
nexus-opx.service                                     loaded    active   running Not an operation unit
nexus-op-y.timer                                      loaded    active   waiting Not a service

3 loaded units listed.
`)
	got := unitNames(out)
	want := []string{"nexus-op-01a1b779-8149-7035-a5a5-8df4e45d4beb.service", "nexus-op-x.service"}
	if !slices.Equal(got, want) {
		t.Fatalf("unitNames = %q, want %q", got, want)
	}
}
