package integration

import (
	"testing"
	"time"
)

// TestTC118Oracle is the negative control for TC-SM-118's oracle: the
// planted rows (a restart created before U ended, or while U reads
// non-terminal) must be flagged, so the integration test cannot pass by
// classifying every accepted restart as legitimate.
func TestTC118Oracle(t *testing.T) {
	const u = "01a1d8e7-9ad8"
	ended := time.Date(2026, 11, 14, 7, 13, 46, 440819000, time.UTC)
	accepted := func(created time.Time) tc118Attempt {
		return tc118Attempt{code: 0, opID: "01a1d8e7-b757", created: created}
	}
	cases := []struct {
		name      string
		a         tc118Attempt
		uTerminal bool
		uEnded    *time.Time
		want      tc118Verdict
	}{
		{"409 naming U", tc118Attempt{code: 1, status: float64(409), blocking: u}, false, nil, tc118Refused},
		{"409 naming U after U ended", tc118Attempt{code: 1, status: float64(409), blocking: u}, true, &ended, tc118Refused},
		{"409 naming another op", tc118Attempt{code: 1, status: float64(409), blocking: "01a1d8e7-0000"}, false, nil, tc118Violation},
		{"409 naming nothing", tc118Attempt{code: 1, status: float64(409)}, false, nil, tc118Violation},
		{"no answer", tc118Attempt{code: cli5}, false, nil, tc118Unanswered},
		{"admitted after U ended", accepted(ended.Add(398 * time.Millisecond)), true, &ended, tc118AfterU},
		{"admitted at U's end", accepted(ended), true, &ended, tc118AfterU},
		{"planted: created 1us before U ended", accepted(ended.Add(-time.Microsecond)), true, &ended, tc118Violation},
		{"planted: created while U non-terminal", accepted(ended), false, nil, tc118Violation},
		{"planted: U terminal without ended", accepted(ended), true, nil, tc118Violation},
		{"exit 0 without an operation", tc118Attempt{code: 0}, true, &ended, tc118Violation},
		{"other error", tc118Attempt{code: 1, status: float64(500)}, false, nil, tc118Violation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := tc118Classify(c.a, u, c.uTerminal, c.uEnded)
			if got != c.want {
				t.Errorf("tc118Classify = %s (%s), want %s", got, why, c.want)
			}
			if (got == tc118Violation) != (why != "") {
				t.Errorf("verdict %s with reason %q: a violation needs a reason, nothing else has one", got, why)
			}
		})
	}
}
