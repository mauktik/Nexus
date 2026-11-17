// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-5, TC-RA-6,
// TC-RA-7, TC-RA-E3. Owner WI: 1.2.

package problem

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TC-RA-5: The four new problem types and the extended config-apply-failed are registered as declared. Owner WI 1.2. Requirements: FR-5, FR-8, FR-9, FR-12, FR-11.
//
// The table is the Problem types table of
// planning/nexusd-remote-admin/architecture.md (Interfaces, internal/problem).
func TestTCRA5_TheFourNewProblemTypesAndTheExtendedConfigApply(t *testing.T) {
	want := []struct {
		slug           Slug
		status, exit   int
		extensions     []string
		operationLevel bool
	}{
		{"config-set-unavailable", 503, 4, []string{"provider", "reason"}, false},
		{"config-conflict", 0, 1, nil, true},
		{"config-restore-failed", 0, 1, []string{"backupPath"}, true},
		{"token-limit-reached", 409, 1, []string{"maxTokens"}, false},
		{"config-apply-failed", 0, 1, []string{"restored", "backupPath"}, true},
		{"rate-limited", 429, 3, nil, false},
	}
	inOps := func(slug Slug) bool {
		return slices.ContainsFunc(OperationLevel(), func(t Type) bool { return t.Slug == slug })
	}
	for _, w := range want {
		t.Run(string(w.slug), func(t *testing.T) {
			typ, ok := Lookup(w.slug)
			if !ok {
				t.Fatalf("%s: missing from registry", w.slug)
			}
			if typ.Status != w.status || typ.Exit != w.exit || typ.OperationLevel != w.operationLevel {
				t.Errorf("status %d exit %d operationLevel %v, want %d %d %v",
					typ.Status, typ.Exit, typ.OperationLevel, w.status, w.exit, w.operationLevel)
			}
			if !slices.Equal(typ.Extensions, w.extensions) {
				t.Errorf("extensions %v, want %v", typ.Extensions, w.extensions)
			}
			if typ.Title == "" {
				t.Error("empty title")
			}
			if got := inOps(w.slug); got != w.operationLevel {
				t.Errorf("in OperationLevel() = %v, want %v", got, w.operationLevel)
			}
			p := New(w.slug, "d")
			for _, ext := range w.extensions {
				p.With(ext, "x")
			}
			if len(p.Extensions) != len(w.extensions) {
				t.Errorf("With accepted %d of %d declared extensions", len(p.Extensions), len(w.extensions))
			}
		})
	}

	for _, slug := range []Slug{ConfigConflict, ConfigRestoreFailed} {
		if !slices.Contains(wantOperationLevel, slug) {
			t.Errorf("wantOperationLevel lacks %s", slug)
		}
	}
	for _, slug := range []Slug{ConfigSetUnavailable, TokenLimitReached} {
		if _, ok := wantHTTP[slug]; !ok {
			t.Errorf("wantHTTP lacks %s", slug)
		}
	}
}

// TC-RA-6: New types and extensions round-trip through MarshalJSON and UnmarshalJSON. Owner WI 1.2. Requirements: FR-8, FR-9.
func TestTCRA6_NewTypesAndExtensionsRoundTripThroughMarshalJSON(t *testing.T) {
	cases := []struct {
		in   *Error
		slug Slug
		want map[string]any
	}{
		{
			New(ConfigApplyFailed, "d").With("restored", true).With("backupPath", "/etc/nexus/nexusd.yaml.bak"),
			ConfigApplyFailed,
			map[string]any{"restored": true, "backupPath": "/etc/nexus/nexusd.yaml.bak"},
		},
		{
			New(ConfigSetUnavailable, "d").With("provider", "nexusd").With("reason", "requires systemd"),
			ConfigSetUnavailable,
			map[string]any{"provider": "nexusd", "reason": "requires systemd"},
		},
	}
	for _, c := range cases {
		t.Run(string(c.slug), func(t *testing.T) {
			first, err := json.Marshal(c.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out Error
			if err := json.Unmarshal(first, &out); err != nil {
				t.Fatalf("unmarshal %s: %v", first, err)
			}
			second, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("second marshal: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("second marshal differs:\n first %s\nsecond %s", first, second)
			}
			if out.Slug() != c.slug {
				t.Errorf("Slug() = %q, want %q", out.Slug(), c.slug)
			}
			for k, v := range c.want {
				if out.Extensions[k] != v {
					t.Errorf("extension %s = %#v, want %#v", k, out.Extensions[k], v)
				}
			}
			if len(out.Extensions) != len(c.want) {
				t.Errorf("extensions = %v, want %v", out.Extensions, c.want)
			}
		})
	}
}

// TC-RA-7: Write sets Retry-After for a 429 with RetryAfter, and the body is unchanged. Owner WI 1.2. Requirements: FR-5.
func TestTCRA7_WriteSetsRetryAfterForA429WithRetryAfterAndThe(t *testing.T) {
	e := New(RateLimited, "too many")

	base := httptest.NewRecorder()
	if err := Write(base, e); err != nil {
		t.Fatalf("Write without RetryAfter: %v", err)
	}
	if h, ok := base.Result().Header["Retry-After"]; ok {
		t.Errorf("Retry-After = %v with RetryAfter unset, want absent", h)
	}
	baseBody := base.Body.Bytes()

	cases := []struct {
		retryAfter time.Duration
		want       string // "" means absent
	}{
		{1500 * time.Millisecond, "2"},
		{60 * time.Second, "60"},
		{time.Nanosecond, "1"},
		{59*time.Second + time.Millisecond, "60"},
		{0, ""},
	}
	for _, c := range cases {
		t.Run(c.retryAfter.String(), func(t *testing.T) {
			if c.retryAfter > 0 {
				// The expected value is max(1, ceil(seconds)), per the test case.
				computed := strconv.Itoa(int(math.Max(1, math.Ceil(c.retryAfter.Seconds()))))
				if computed != c.want {
					t.Fatalf("table drift: max(1, ceil(%v)) = %s, table says %s", c.retryAfter, computed, c.want)
				}
			}
			p := *e
			p.RetryAfter = c.retryAfter
			rec := httptest.NewRecorder()
			if err := Write(rec, &p); err != nil {
				t.Fatalf("Write: %v", err)
			}
			res := rec.Result()
			if res.StatusCode != http.StatusTooManyRequests {
				t.Errorf("status = %d, want 429", res.StatusCode)
			}
			got, present := res.Header["Retry-After"]
			switch {
			case c.want == "" && present:
				t.Errorf("Retry-After = %v, want absent", got)
			case c.want != "" && (len(got) != 1 || got[0] != c.want):
				t.Errorf("Retry-After = %v, want [%s]", got, c.want)
			}
			body := rec.Body.Bytes()
			if !bytes.Equal(body, baseBody) {
				t.Errorf("body = %s, want the body without RetryAfter %s", body, baseBody)
			}
			if strings.Contains(strings.ToLower(string(body)), "retryafter") {
				t.Errorf("body %s has a retryAfter member", body)
			}
		})
	}
}

// TC-RA-E3: Retry-After is not set on a non-429 problem even when RetryAfter is set. Owner WI 1.2. Requirements: FR-5.
func TestTCRAE3_RetryAfterIsNotSetOnANon429ProblemEvenWhen(t *testing.T) {
	p := New(Conflict, "x")
	p.RetryAfter = 5 * time.Second
	rec := httptest.NewRecorder()
	if err := Write(rec, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res := rec.Result()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", res.StatusCode)
	}
	if h, ok := res.Header["Retry-After"]; ok {
		t.Errorf("Retry-After = %v on a 409, want absent", h)
	}
}
