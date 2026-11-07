package auth

import "testing"

// TC-29: scope matching has no wildcards beyond scripts:run.
func TestScopeMatchingHasNoWildcardsBeyondScriptsRun(t *testing.T) {
	cases := []struct {
		held, required string
		want           bool
	}{
		{"scripts:run", "scripts:run:a", true},
		{"scripts:run:a", "scripts:run:a", true},
		{"scripts:run:a", "scripts:run:b", false},
		{"scripts:run:a", "scripts:run", false},
		{"containers:write", "containers:read", false},
		{"containers:*", "containers:read", false},
		{"", "containers:read", false},
		{"", "scripts:run:a", false},
		{"", "", false},
		// Edges of the one wildcard: it needs a non-empty script ID, and
		// nothing else that merely shares the prefix gains from it.
		{"scripts:run", "scripts:run:", false},
		{"scripts:run", "scripts:runner", false},
		{"scripts:run", "scripts:read", false},
		{"scripts", "scripts:run:a", false},
		{"scripts:run:", "scripts:run:a", false},
		{"*", "containers:read", false},
		{"containers:read", "", false},
	}
	for _, c := range cases {
		if got := Satisfies(c.held, c.required); got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", c.held, c.required, got, c.want)
		}
		if got := HasScope([]string{c.held}, c.required); got != c.want {
			t.Errorf("HasScope([%q], %q) = %v, want %v", c.held, c.required, got, c.want)
		}
	}
	if HasScope(nil, "containers:read") || HasScope([]string{}, "containers:read") {
		t.Error("no scopes satisfied a requirement")
	}
	if !HasScope([]string{"containers:write", "containers:read"}, "containers:read") {
		t.Error("a later matching scope was not found")
	}
	p := Principal{Scopes: []string{"scripts:run"}}
	if !p.Has("scripts:run:deploy-api") || p.Has("packages:write") {
		t.Errorf("Principal.Has disagrees with HasScope for %v", p.Scopes)
	}
}
