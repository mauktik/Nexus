package def

import (
	"slices"
	"testing"
)

// TC-4: the approved verb list is exactly FR-29's, with no synonyms.
func TestApprovedVerbsAreExact(t *testing.T) {
	want := []string{"get", "start", "stop", "restart", "update", "install", "remove", "invoke", "test", "add", "cancel"}

	got := ApprovedVerbs()
	if !sameSet(got, want) {
		t.Fatalf("ApprovedVerbs() = %v, want exactly %v", got, want)
	}
	if len(got) != len(want) {
		t.Fatalf("ApprovedVerbs() has %d entries (duplicates?), want %d", len(got), len(want))
	}
	for _, v := range want {
		if !IsApprovedVerb(v) {
			t.Errorf("IsApprovedVerb(%q) = false, want true", v)
		}
	}
	for _, synonym := range []string{"list", "delete", "run", "create", "set", "show", "describe", "exec", "Get", ""} {
		if IsApprovedVerb(synonym) {
			t.Errorf("IsApprovedVerb(%q) = true, want false: synonyms are not approved", synonym)
		}
	}
}

func TestApprovedVerbsReturnsACopy(t *testing.T) {
	got := ApprovedVerbs()
	got[0] = "list"
	if IsApprovedVerb("list") {
		t.Fatal("mutating the ApprovedVerbs() result changed the approved set")
	}
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y)) && len(slices.Compact(x)) == len(y)
}
