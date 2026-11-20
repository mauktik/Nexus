// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package auth

import "strings"

// ScriptsRun is the only scope with a wildcard meaning: it grants every
// per-script scope scripts:run:{id} (FR-22, ASM-4).
const ScriptsRun = "scripts:run"

// Satisfies reports whether the held scope grants the required one. Scopes
// match exactly. The single exception is ScriptsRun, which also grants
// scripts:run:{id} for any non-empty id. Nothing else is a wildcard: not "*",
// not "containers:*", and a write scope does not imply its read scope. An
// empty scope grants nothing and an empty requirement is never satisfied, so
// a declaration that forgets its scope cannot be met by accident.
func Satisfies(held, required string) bool {
	if held == "" || required == "" {
		return false
	}
	if held == required {
		return true
	}
	id, ok := strings.CutPrefix(required, ScriptsRun+":")
	return ok && id != "" && held == ScriptsRun
}

// HasScope reports whether any held scope grants required.
func HasScope(held []string, required string) bool {
	for _, h := range held {
		if Satisfies(h, required) {
			return true
		}
	}
	return false
}

// Has reports whether the principal holds a scope that grants required.
func (p Principal) Has(required string) bool { return HasScope(p.Scopes, required) }
