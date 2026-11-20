// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-12 (WI 1.3).

package audit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/audit"
)

// TC-RA-12 (FR-13, NFR-5): OutcomeCompleted exists and MaxFacts is 64,
// enforced through the constant.
func TestTCRA12_OutcomeCompletedExistsAndMaxFactsIs64Enforced(t *testing.T) {
	if audit.OutcomeCompleted != "completed" {
		t.Errorf("OutcomeCompleted = %q, want completed", audit.OutcomeCompleted)
	}
	if audit.MaxFacts != 64 {
		t.Errorf("MaxFacts = %d, want 64", audit.MaxFacts)
	}

	ctx := audit.WithFacts(context.Background())
	last := fmt.Sprintf("k%d", audit.MaxFacts)
	for i := range audit.MaxFacts + 1 {
		audit.AddFact(ctx, fmt.Sprintf("k%d", i), "v")
	}
	got := audit.Facts(ctx)
	if len(got) != audit.MaxFacts {
		t.Fatalf("%d facts kept after %d distinct adds, want %d", len(got), audit.MaxFacts+1, audit.MaxFacts)
	}
	if _, ok := got[last]; ok {
		t.Fatalf("fact %s past the bound was kept", last)
	}
	if _, ok := got["k0"]; !ok {
		t.Fatal("fact k0 within the bound was dropped")
	}

	ctx = audit.WithFacts(context.Background())
	audit.AddFact(ctx, strings.Repeat("k", audit.MaxFactKeyBytes+1), strings.Repeat("v", audit.MaxFactValueBytes+1))
	for k, v := range audit.Facts(ctx) {
		if len(k) != audit.MaxFactKeyBytes || len(v) != audit.MaxFactValueBytes {
			t.Fatalf("stored key %d bytes, value %d bytes; want %d and %d",
				len(k), len(v), audit.MaxFactKeyBytes, audit.MaxFactValueBytes)
		}
	}
}
