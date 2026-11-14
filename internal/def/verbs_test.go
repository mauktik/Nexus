package def

import (
	"slices"
	"strings"
	"testing"
)

// TC-4: the approved verb list is exactly FR-29's, as amended to add reload
// (nginx-provider TC-53), with no synonyms.
func TestApprovedVerbsAreExact(t *testing.T) {
	want := []string{"get", "start", "stop", "restart", "update", "install", "remove", "invoke", "test", "add", "cancel", "reload", "rollback", "validate-config"}

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
	for _, synonym := range []string{"list", "delete", "run", "create", "set", "show", "describe", "exec", "refresh", "reconfigure", "Get", ""} {
		if IsApprovedVerb(synonym) {
			t.Errorf("IsApprovedVerb(%q) = true, want false: synonyms are not approved", synonym)
		}
	}
}

// TC-SM-1 (FR-13): the list gains reload, rollback and validate-config, in
// declaration order and nothing else, and each is accepted by Define.
func TestTCSM1_ApprovedVerbListGainsReloadRollback(t *testing.T) {
	want := []string{"get", "start", "stop", "restart", "update", "install", "remove", "invoke", "test", "add", "cancel", "reload", "rollback", "validate-config"}
	if got := ApprovedVerbs(); !slices.Equal(got, want) {
		t.Fatalf("ApprovedVerbs() = %v, want exactly %v in this order", got, want)
	}

	for _, verb := range []string{"reload", "rollback", "validate-config"} {
		op := readOp(uniqueID("test.verb"))
		op.CLI.Verb = verb
		if msg, panicked := recoverMessage(define(op)); panicked {
			t.Errorf("Define with verb %q panicked: %s", verb, msg)
		}
	}

	op := readOp(uniqueID("test.verb"))
	op.CLI.Verb = "selfupdate"
	msg, panicked := recoverMessage(define(op))
	if !panicked || !strings.Contains(msg, `unapproved CLI verb "selfupdate"`) || !strings.Contains(msg, op.ID) {
		t.Errorf("Define with verb selfupdate: panicked=%v msg=%q, want the unapproved-verb message", panicked, msg)
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
