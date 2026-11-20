// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// TC-38: the audit query spans rotated files, filters, pages, and needs
// audit:read.
func TestAuditQuerySpansRotatedFilesWithFiltersAndScope(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	now := base
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	lg, err := audit.Open(audit.Options{Dir: dir, Host: "testhost", RotateBytes: 4 << 10, Logger: log,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	tokens := []string{"alice", "bob"}
	providers := []string{"docker", "apt", "scripts"}
	outcomes := []string{audit.OutcomeRequested, audit.OutcomeDenied, audit.OutcomeRefused}
	var known []systemspec.AuditRecord // In write order
	for i := range 60 {
		now = base.Add(time.Duration(i) * time.Minute)
		rec := systemspec.AuditRecord{
			TokenName: tokens[i%2], TokenID: fmt.Sprintf("%012x", i%2), Source: "192.0.2.9", Surface: "rest",
			Provider: providers[i%3], Action: providers[i%3] + ".things." + []string{"start", "stop"}[i%4/2],
			Target: fmt.Sprintf("t%d", i), Outcome: outcomes[i%3%len(outcomes)],
		}
		if i%5 == 0 {
			rec.Outcome = audit.OutcomeReplayed
		}
		id, err := lg.Write(rec, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec.ID, rec.Host, rec.Time = id, "testhost", now
		known = append(known, rec)
	}
	var rotated, live int
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl.gz") {
			rotated++
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, audit.FileName)); err == nil {
		live = strings.Count(string(b), "\n")
	}
	if rotated == 0 || live == 0 || live == len(known) {
		t.Fatalf("records are not spread over rotated and live files: %d rotated, %d live", rotated, live)
	}

	d := core.NewDispatcher(registryWithAudit(t, lg, log), core.DispatcherOptions{Logger: log})
	list := func(scopes []string, input string) (apitypes.List[systemspec.AuditRecord], error) {
		res, err := d.Invoke(context.Background(), core.InvokeRequest{
			Caller: auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "auditor", Scopes: scopes},
			Op:     systemspec.AuditList.Op(), Input: json.RawMessage(input),
		})
		if err != nil {
			return apitypes.List[systemspec.AuditRecord]{}, err
		}
		return res.Body.(apitypes.List[systemspec.AuditRecord]), nil
	}
	// all pages through the list, so the cursor is exercised too.
	all := func(filters []string) []systemspec.AuditRecord {
		t.Helper()
		var got []systemspec.AuditRecord
		cursor := ""
		for range 100 {
			in, _ := json.Marshal(map[string]any{"limit": 7, "cursor": cursor, "filter": filters})
			page, err := list([]string{"audit:read"}, string(in))
			if err != nil {
				t.Fatalf("filters %v: %v", filters, err)
			}
			got = append(got, page.Items...)
			if page.NextCursor == "" {
				return got
			}
			cursor = page.NextCursor
		}
		t.Fatal("paging did not end")
		return nil
	}
	expect := func(keep func(systemspec.AuditRecord) bool) []systemspec.AuditRecord {
		var want []systemspec.AuditRecord
		for i := len(known) - 1; i >= 0; i-- {
			if keep(known[i]) {
				want = append(want, known[i])
			}
		}
		return want
	}
	after, before := base.Add(10*time.Minute), base.Add(45*time.Minute)
	cases := []struct {
		filters []string
		keep    func(systemspec.AuditRecord) bool
	}{
		{nil, func(systemspec.AuditRecord) bool { return true }},
		{[]string{"after=" + after.Format(time.RFC3339)}, func(r systemspec.AuditRecord) bool { return r.Time.After(after) }},
		{[]string{"before=" + before.Format(time.RFC3339)}, func(r systemspec.AuditRecord) bool { return r.Time.Before(before) }},
		{[]string{"tokenName=bob"}, func(r systemspec.AuditRecord) bool { return r.TokenName == "bob" }},
		{[]string{"provider=apt"}, func(r systemspec.AuditRecord) bool { return r.Provider == "apt" }},
		{[]string{"action=docker.things.stop"}, func(r systemspec.AuditRecord) bool { return r.Action == "docker.things.stop" }},
		{[]string{"outcome=replayed"}, func(r systemspec.AuditRecord) bool { return r.Outcome == audit.OutcomeReplayed }},
		{[]string{"after=" + after.Format(time.RFC3339), "before=" + before.Format(time.RFC3339), "tokenName=alice", "outcome=denied"},
			func(r systemspec.AuditRecord) bool {
				return r.Time.After(after) && r.Time.Before(before) && r.TokenName == "alice" && r.Outcome == audit.OutcomeDenied
			}},
	}
	for _, c := range cases {
		got, want := all(c.filters), expect(c.keep)
		if len(want) == 0 && len(c.filters) > 0 {
			t.Fatalf("filters %v: the known set has no match; the case proves nothing", c.filters)
		}
		ids := func(rs []systemspec.AuditRecord) []string {
			out := make([]string, len(rs))
			for i, r := range rs {
				out[i] = r.ID
			}
			return out
		}
		if !slices.Equal(ids(got), ids(want)) {
			t.Fatalf("filters %v:\n got %v\nwant %v", c.filters, ids(got), ids(want))
		}
		for i := range got {
			if !got[i].Time.Equal(want[i].Time) || got[i].TokenName != want[i].TokenName || got[i].Target != want[i].Target {
				t.Fatalf("filters %v: record %d = %+v, want %+v", c.filters, i, got[i], want[i])
			}
		}
	}

	_, err = list([]string{"docker:read", "apt:read"}, `{}`)
	var p *problem.Error
	if !asProblem(err, &p) || p.Status != http.StatusForbidden || p.Extensions["requiredScope"] != "audit:read" {
		t.Fatalf("without audit:read: %v, want 403 naming audit:read", err)
	}
	_, err = list([]string{"audit:read"}, `{"filter":["after=yesterday"]}`)
	if !asProblem(err, &p) || p.Slug() != problem.ValidationFailed {
		t.Fatalf("bad time filter: %v, want validation-failed", err)
	}
	_, err = list([]string{"audit:read"}, `{"cursor":"not-a-cursor"}`)
	if !asProblem(err, &p) || p.Slug() != problem.ValidationFailed {
		t.Fatalf("foreign cursor: %v, want validation-failed", err)
	}
}

func registryWithAudit(t *testing.T, lg *audit.Log, log *slog.Logger) *core.Registry {
	t.Helper()
	reg, err := core.New(nil, core.Options{Host: "testhost", Version: "v0.0.0-test", Logger: log, Audit: lg})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func asProblem(err error, p **problem.Error) bool { return errors.As(err, p) }
