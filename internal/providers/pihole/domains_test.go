//go:build pihole_red

// Stub-style: SKIP-only (WI 3.4). Excluded from `make test` by the
// pihole_red build tag; the WI 3.4 developer deletes the //go:build line and
// these must pass. Cases: TC-82 to TC-85, TC-87, TC-ERR16 in
// planning/pihole-dns-zones/test-cases.md. Shared helpers are in
// service_test.go (WI 3.2).
//
// Regression for the verified v5 hazard "list.sh regex SQL quoting and silent
// cross-list move" (V-14): the provider never calls list.sh; it writes
// gravity.db with bound parameters and refuses a cross-list add with 409.
// FakeHost encodes list.sh's real behavior (TC-70d), so an accidental
// `pihole -b/-w/--regex/--white-regex` call is visible and wrong.
package pihole

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

func usedListSh(argvs [][]string) bool {
	for _, a := range argvs {
		for _, f := range []string{"-w", "-b", "--regex", "--white-regex", "--wild", "--allow", "--deny"} {
			if len(a) > 1 && a[0] == piholeBin && a[1] == f {
				return true
			}
		}
	}
	return false
}

func reloads(events []string) int {
	n := 0
	for _, e := range events {
		if e == "reload-lists" {
			n++
		}
	}
	return n
}

// TC-82: add creates a deny-exact row (type 1), reloads lists once, and
// returns the row read back from gravity.db; adding it again is created:false
// with no further reload.
func TestTC82_AddDomainCreatedThenIdempotent(t *testing.T) {
	h, m := newV5(t)
	in := spec.DomainAddInput{List: "deny", Kind: "exact", Value: "ads.example.com", Comment: "nexus test"}
	got, created, err := m.AddDomain(context.Background(), in, &bytes.Buffer{})
	if err != nil || !created {
		t.Fatalf("add = %+v, %v, %v", got, created, err)
	}
	rows := h.DomainRows()
	if len(rows) != 1 || rows[0].Type != 1 || rows[0].Domain != in.Value || rows[0].Comment != in.Comment || rows[0].ID != got.PiholeID {
		t.Fatalf("rows %+v, result %+v", rows, got)
	}
	if got.List != "deny" || got.Kind != "exact" || got.Value != in.Value || !got.Enabled {
		t.Errorf("result %+v", got)
	}
	if reloads(h.FTLEvents()) != 1 {
		t.Errorf("reload-lists sent %d times, want 1", reloads(h.FTLEvents()))
	}
	_, created, err = m.AddDomain(context.Background(), in, &bytes.Buffer{})
	if err != nil || created || len(h.DomainRows()) != 1 || reloads(h.FTLEvents()) != 1 {
		t.Errorf("second add: created=%v err=%v rows=%d reloads=%d", created, err, len(h.DomainRows()), reloads(h.FTLEvents()))
	}
	if usedListSh(h.Argvs()) {
		t.Errorf("list.sh was invoked: %q", h.Argvs())
	}
}

// TC-83 (regression: list.sh silent move): a value already in the opposite
// list with the same kind is refused 409 naming the existing entry; the row
// keeps its type; list.sh is never run.
func TestTC83_CrossListRefusedNotMoved(t *testing.T) {
	h, m := newV5(t)
	id := h.SeedDomain(0 /* allow exact */, "cdn.example.com", "")
	_, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "cdn.example.com"}, &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "conflict" || !strings.Contains(pe.Detail, strconv.FormatInt(id, 10)) {
		t.Errorf("got %s %q, want conflict naming id %d", slug, pe.Detail, id)
	}
	if rows := h.DomainRows(); len(rows) != 1 || rows[0].Type != 0 {
		t.Errorf("rows %+v: the allow entry must be untouched", rows)
	}
	if usedListSh(h.Argvs()) || reloads(h.FTLEvents()) != 0 {
		t.Errorf("argvs %q events %v", h.Argvs(), h.FTLEvents())
	}
	// Same value, other kind (regex deny) is not a cross-list conflict.
	if _, created, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "regex", Value: "cdn.example.com"}, &bytes.Buffer{}); err != nil || !created {
		t.Errorf("regex deny of the same text: created=%v err=%v", created, err)
	}
}

// TC-84 (regression: list.sh regex SQL interpolation): regex values with the
// characters FTL regex needs, and an SQL-injection probe without a quote, are
// stored byte for byte through bound parameters; the table survives.
func TestTC84_RegexStoredByteExact(t *testing.T) {
	h, m := newV5(t)
	values := []string{
		`(^|\.)ads\.example\.com$`,
		`^ad[s]?\.;querytype=A`,
		`x$|y`,
		`a/b=c`,
		`x);DROP TABLE domainlist;--`,
		`\x27`, // a literal backslash-x-2-7, not a quote
	}
	for _, v := range values {
		if _, created, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "regex", Value: v}, &bytes.Buffer{}); err != nil || !created {
			t.Fatalf("add %q: created=%v err=%v", v, created, err)
		}
	}
	rows := h.DomainRows()
	if len(rows) != len(values) {
		t.Fatalf("%d rows, want %d (table damaged?)", len(rows), len(values))
	}
	for i, r := range rows {
		if r.Domain != values[i] || r.Type != 3 {
			t.Errorf("row %d = type %d %q, want type 3 %q", i, r.Type, r.Domain, values[i])
		}
	}
	if usedListSh(h.Argvs()) {
		t.Error("list.sh was invoked for a regex")
	}
}

// TC-85: remove by id returns the removed row and reloads lists; an unknown
// id is not-found with no reload.
func TestTC85_RemoveDomain(t *testing.T) {
	h, m := newV5(t)
	id := h.SeedDomain(1, "ads.example.com", "c")
	got, err := m.RemoveDomain(context.Background(), id, &bytes.Buffer{})
	if err != nil || got.PiholeID != id || got.Value != "ads.example.com" || got.List != "deny" {
		t.Fatalf("remove = %+v, %v", got, err)
	}
	if len(h.DomainRows()) != 0 || reloads(h.FTLEvents()) != 1 {
		t.Errorf("rows %d reloads %d", len(h.DomainRows()), reloads(h.FTLEvents()))
	}
	_, err = m.RemoveDomain(context.Background(), id, &bytes.Buffer{})
	if slug, _ := slugOf(t, err); slug != "not-found" || reloads(h.FTLEvents()) != 1 {
		t.Errorf("second remove: %s, reloads %d", slug, reloads(h.FTLEvents()))
	}
}

// TC-87 (V-7, A-16): a mutation waits while an external gravity run is in
// progress, logs that it waits, and proceeds once it ends.
func TestTC87_WaitsForExternalGravity(t *testing.T) {
	h, m := newV5(t)
	_, stop := h.PlantGravityProcess()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "late.example.com"}, &out)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("add finished during a gravity run: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if len(h.DomainRows()) != 0 {
		t.Fatal("row written during an external gravity run (it would be lost at the swap)")
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("add did not resume after the gravity run ended")
	}
	if !strings.Contains(out.String(), "pihole: waiting for a gravity run") {
		t.Errorf("log %q", out.String())
	}
}

// TC-ERR16: a failed reload-lists after the insert is pihole-command-failed
// whose detail says the entry was added.
func TestTCERR16_ReloadFailureAfterAdd(t *testing.T) {
	h, m := newV5(t)
	h.FailCommand([]string{piholeBin, "restartdns", "reload-lists"}, 1)
	_, _, err := m.AddDomain(context.Background(), spec.DomainAddInput{List: "deny", Kind: "exact", Value: "ads.example.com"}, &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != "pihole-command-failed" || !strings.Contains(pe.Detail, "added") || pe.Extensions["exitCode"] != 1 {
		t.Errorf("got %s %q %+v", slug, pe.Detail, pe.Extensions)
	}
	if len(h.DomainRows()) != 1 {
		t.Error("the committed row should remain (detail reports it)")
	}
}
