// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxReasonBytes bounds every unavailable reason, signal message and
	// error detail that can carry text from a parser, a tool or a file the
	// host or a share user controls (B-SEC-1).
	maxReasonBytes = 256
	// reasonTruncated ends a reason that was cut.
	reasonTruncated = "…[truncated]"
)

// boundReason is s made safe to return to a client: invalid UTF-8 replaced,
// control and format characters (line breaks, escape sequences, bidi
// overrides) replaced by a space, and at most maxReasonBytes long, cut on a
// rune boundary and ended with reasonTruncated.
func boundReason(s string) string {
	// Only the head of s is ever kept, so a megabyte is never scanned: each
	// kept rune is at least one byte, and one more than the bound is enough.
	var b strings.Builder
	for _, r := range s {
		if b.Len() > maxReasonBytes {
			break
		}
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = ' '
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= maxReasonBytes {
		return out
	}
	cut := maxReasonBytes - len(reasonTruncated)
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + reasonTruncated
}
