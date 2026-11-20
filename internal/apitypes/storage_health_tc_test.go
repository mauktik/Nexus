// Tests of planning/storage-provider/test-cases.md TC-STG-19 (WI 1.3).

package apitypes

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// keysOf returns the sorted keys of a JSON object.
func keysOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TC-STG-19: health wire types have the documented JSON shape. Owner WI 1.3. Requirements: FR-15, FR-16.
func TestTCSTG19_HealthWireTypesHaveTheDocumentedJSON(t *testing.T) {
	if SeverityCritical != "critical" || SeverityWarning != "warning" || SeverityInfo != "info" {
		t.Errorf("severity words %q %q %q", SeverityCritical, SeverityWarning, SeverityInfo)
	}

	// The Interfaces block declares stale without omitempty (D-7), so the
	// empty block carries "stale":false.
	b, err := json.Marshal(ProviderSignals{Source: "storage"})
	if err != nil {
		t.Fatal(err)
	}
	const empty = `{"source":"storage","observedAt":null,"counts":null,"signals":[],"signalsTruncated":false,"stale":false}`
	if string(b) != empty {
		t.Errorf("empty block:\n got %s\nwant %s", b, empty)
	}

	b, err = json.Marshal(ProviderSignals{Source: "storage", Stale: true, Unavailable: map[string]string{"signals": "timed out"}})
	if err != nil {
		t.Fatal(err)
	}
	var stale map[string]json.RawMessage
	if err := json.Unmarshal(b, &stale); err != nil {
		t.Fatal(err)
	}
	if string(stale["stale"]) != "true" || string(stale["unavailable"]) != `{"signals":"timed out"}` {
		t.Errorf("stale block: %s", b)
	}

	at := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	filled := ProviderSignals{
		Source: "storage", ObservedAt: &at, Counts: &SignalCounts{Critical: 1, Warning: 2, Info: 3},
		Signals: []HealthSignal{{ID: "pool-not-online", Severity: SeverityCritical, Subject: "pool:z1",
			Message: "pool z1 is degraded", ObservedAt: at}},
		SignalsTruncated: true,
	}
	b, err = json.Marshal(filled)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Counts  json.RawMessage   `json:"counts"`
		Signals []json.RawMessage `json:"signals"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(t, top.Counts); !slices.Equal(got, []string{"critical", "info", "warning"}) {
		t.Errorf("counts keys %v", got)
	}
	if string(top.Counts) != `{"critical":1,"warning":2,"info":3}` {
		t.Errorf("counts %s", top.Counts)
	}
	if len(top.Signals) != 1 {
		t.Fatalf("signals %s", b)
	}
	if got := keysOf(t, top.Signals[0]); !slices.Equal(got, []string{"id", "message", "observedAt", "severity", "subject"}) {
		t.Errorf("signal keys %v", got)
	}
	if got := keysOf(t, b); !slices.Equal(got, []string{"counts", "observedAt", "signals", "signalsTruncated", "source", "stale"}) {
		t.Errorf("block keys %v", got)
	}

	var back ProviderSignals
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Source != "storage" || back.Counts == nil || *back.Counts != *filled.Counts || len(back.Signals) != 1 ||
		back.Signals[0] != filled.Signals[0] || !back.SignalsTruncated || back.ObservedAt == nil || !back.ObservedAt.Equal(at) {
		t.Errorf("round trip: %+v", back)
	}
}
