package release

import (
	"regexp"
	"testing"
)

// The Makefile release-version check; every version CompareVersions accepts
// must also pass it.
var makefileReleaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// TC-SM-12 (WI 1.3; FR-7, ASM-6): CompareVersions follows SemVer 2.0
// precedence.
func TestTCSM12_CompareVersionsFollowsSemVer20Precedence(t *testing.T) {
	check := func(t *testing.T, a, b string, want int) {
		t.Helper()
		got, ok := CompareVersions(a, b)
		if !ok || got != want {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d, true", a, b, got, ok, want)
		}
		for _, s := range []string{a, b} {
			if !makefileReleaseVersion.MatchString(s) {
				t.Errorf("%q is accepted but fails the Makefile release-version regex", s)
			}
		}
	}

	pairs := []struct {
		older, newer string
	}{
		{"v1.2.3", "v1.2.4"},
		{"v1.9.0", "v1.10.0"},
		{"v1.99.99", "v2.0.0"},
		{"v1.0.0-rc.1", "v1.0.0"},
		{"v0.0.9", "v0.1.0"},
		{"v1.0.0-1", "v1.0.0-a"},
		{"v1.0.0-9", "v1.0.0-10"},
		{"v1.0.0-x-y", "v1.0.0-x-z"},
		{"v9.9.1", "v9.9.2"},
		{"v18446744073709551615.0.0", "v18446744073709551616.0.0"},
	}
	for _, p := range pairs {
		check(t, p.older, p.newer, -1)
		check(t, p.newer, p.older, +1)
	}

	// SemVer 2.0 §11 example chain.
	chain := []string{
		"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta",
		"v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0",
	}
	for i := 0; i+1 < len(chain); i++ {
		check(t, chain[i], chain[i+1], -1)
		check(t, chain[i+1], chain[i], +1)
	}
	for _, v := range append(chain, "v9.9.2", "v0.0.0", "v1.0.0-0.3.7") {
		check(t, v, v, 0)
	}
}

func TestCompareVersionsRejectsNonReleaseVersions(t *testing.T) {
	bad := []string{
		"dev", "1.2.3", "v1.2", "v1.2.3.4", "v01.2.3", "",
		"v1.02.3", "v1.2.03", "V1.2.3", "v1.2.3-", "v1.2.3-rc..1", "v1.2.3-rc.01",
		"v1.2.3+build", "v1.2.3-rc.1+build", " v1.2.3", "v1.2.3\n", "v-1.2.3",
		"v1.2.3-rc_1", "v1.2.3-dirty-abc 1",
		// git describe output, with and without the leading v.
		"1.2.3-4-gabcdef0", "abcdef0",
	}
	for _, s := range bad {
		for _, args := range [][2]string{{s, "v1.2.3"}, {"v1.2.3", s}} {
			if cmp, ok := CompareVersions(args[0], args[1]); ok {
				t.Errorf("CompareVersions(%q, %q) = %d, true; want ok=false", args[0], args[1], cmp)
			}
		}
	}
}
