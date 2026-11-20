// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package release

import "strings"

// CompareVersions orders two release versions by SemVer 2.0 precedence
// (semver.org §11). It returns -1, 0 or +1 as a is older than, equal to or
// newer than b.
//
// A release version is v<major>.<minor>.<patch>[-<prerelease>]: the form the
// Makefile release-version target accepts, narrowed to valid SemVer 2.0
// (no leading zeros in numeric parts, no empty prerelease identifiers) and
// without build metadata. ok is false when either argument is anything else,
// such as "dev" or a version without the leading v; callers decide what a
// non-comparable version means.
func CompareVersions(a, b string) (cmp int, ok bool) {
	va, ok := parseVersion(a)
	if !ok {
		return 0, false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return 0, false
	}
	for i := range va.core {
		if c := compareNumeric(va.core[i], vb.core[i]); c != 0 {
			return c, true
		}
	}
	return comparePrerelease(va.pre, vb.pre), true
}

type version struct {
	core [3]string // major, minor, patch as validated digit strings
	pre  []string  // prerelease identifiers; nil for a normal version
}

func parseVersion(s string) (version, bool) {
	var v version
	rest, found := strings.CutPrefix(s, "v")
	if !found {
		return v, false
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	parts := strings.Split(core, ".")
	if len(parts) != len(v.core) {
		return v, false
	}
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		v.core[i] = p
	}
	if !hasPre {
		return v, true
	}
	v.pre = strings.Split(pre, ".")
	for _, id := range v.pre {
		if id == "" || !isIdentifier(id) {
			return v, false
		}
		if isNumeric(id) && len(id) > 1 && id[0] == '0' {
			return v, false
		}
	}
	return v, true
}

// isNumeric reports whether s is a non-empty string of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isIdentifier reports whether s uses only the SemVer identifier characters
// [0-9A-Za-z-].
func isIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// compareNumeric compares digit strings without leading zeros by value, so
// no part is too large to compare.
func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// comparePrerelease applies semver.org §11.3 and §11.4: a normal version
// outranks any prerelease; identifiers compare left to right, numeric ones
// by value and below alphanumeric ones, alphanumeric ones in ASCII order;
// a shorter list that is a prefix of a longer one ranks lower.
func comparePrerelease(a, b []string) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		an, bn := isNumeric(a[i]), isNumeric(b[i])
		var c int
		switch {
		case an && bn:
			c = compareNumeric(a[i], b[i])
		case an:
			c = -1
		case bn:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}
