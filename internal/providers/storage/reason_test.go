// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSecB1_BoundReason(t *testing.T) {
	long := strings.Repeat("é", 400)
	for name, in := range map[string]string{
		"short":    "a plain reason",
		"long":     long,
		"controls": "line one\nline two\x1b[31m\x00‮evil",
		"invalid":  "bad \xff\xfe bytes",
	} {
		got := boundReason(in)
		if len(got) > maxReasonBytes {
			t.Errorf("%s: %d bytes, want at most %d", name, len(got), maxReasonBytes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: not valid UTF-8: %q", name, got)
		}
		for _, r := range got {
			if r < ' ' || r == 0x7f || r == 0x202e {
				t.Errorf("%s: control or format character %U kept in %q", name, r, got)
			}
		}
	}
	if got := boundReason("a plain reason"); got != "a plain reason" {
		t.Errorf("a short reason changed to %q", got)
	}
	if got := boundReason(long); !strings.HasSuffix(got, "…[truncated]") {
		t.Errorf("a long reason does not end in the marker: %q", got)
	}
}
