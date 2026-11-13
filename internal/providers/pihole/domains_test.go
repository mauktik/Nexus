// Cases: TC-82 to TC-87, TC-E3, TC-ERR16 and TC-ERR17 in
// planning/pihole-dns-zones/test-cases.md (WI 3.4): the v5 domain methods
// against a FakeHost, and add-domain and remove-domain through the
// Dispatcher, the operation manager and the helper. Shared helpers are in
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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
	"github.com/mauktik/nexus/internal/store"
)

// newV5Lists is newV5 with the Mutator's DNS server on the host's FakeFTL,
// which the list-reload barrier probes (WI 5.6, A-66), as production's is
// 127.0.0.1:53.
func newV5Lists(t *testing.T) (*v5test.FakeHost, *v5.Mutator) {
	t.Helper()
	h, m := newV5(t)
	m.DNSServer = h.FTL().Addr()
	return h, m
}

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
	h, m := newV5Lists(t)
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
	h, m := newV5Lists(t)
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
	h, m := newV5Lists(t)
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
	h, m := newV5Lists(t)
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
	h, m := newV5Lists(t)
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
	h, m := newV5Lists(t)
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

// domainWorld runs add-domain and remove-domain as the server does: the
// Dispatcher (schema, Admit, lane), the operation manager, and a detach
// launcher whose unit runs the helper in-process over the FakeHost.
type domainWorld struct {
	*helperWorld
	st   *store.Store
	disp *core.Dispatcher

	mu sync.Mutex
	// beforeHelper runs in the unit just before the helper, after Admit.
	beforeHelper func()
}

var domainCaller = auth.Principal{TokenID: "tok-dom", Name: "dom", Scopes: []string{spec.ReadScope, spec.WriteScope}}

func newDomainWorld(t *testing.T) *domainWorld {
	t.Helper()
	w := &domainWorld{helperWorld: newHelperWorld(t)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	launcher, err := detach.New(detach.Config{OpsDir: w.opsDir, Logger: log}, detach.Options{
		Launch: func(_ context.Context, inv detach.Invocation) error {
			if err := os.Remove(filepath.Join(inv.Dir, detach.SpecFile)); err != nil {
				return err
			}
			w.mu.Lock()
			hook := w.beforeHelper
			w.mu.Unlock()
			if hook != nil {
				hook()
			}
			started := time.Now()
			code := runHelper(context.Background(), inv.Dir, io.Discard, io.Discard, w.inject())
			return detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()})
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
		},
		Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signals") },
		ListUnits: func(context.Context) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alog.Close() })
	st, err := store.Open(filepath.Dir(w.opsDir), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: w.opsDir, Host: "h", Logger: log, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("manager shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	reg, err := core.New([]provider.Provider{w.p}, core.Options{Host: "h", Logger: log, Audit: alog, Ops: mgr,
		Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	w.st, w.disp = st, core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	return w
}

// invoke calls op with the JSON input as REST does. It returns the problem
// of a refusal, or else the finished operation's record.
func (w *domainWorld) invoke(op def.Op, input string) (store.OpRecord, *problem.Error) {
	w.t.Helper()
	res, err := w.disp.Invoke(context.Background(), core.InvokeRequest{
		Caller: domainCaller, Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input)})
	if err != nil {
		var pe *problem.Error
		if !errors.As(err, &pe) {
			w.t.Fatalf("%s: error %T %v is not a problem", op.ID, err, err)
		}
		return store.OpRecord{}, pe
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := w.st.Get(res.OperationID)
		if err == nil && rec.Status.Terminal() {
			return rec, nil
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation %s: %+v (%v)", res.OperationID, rec, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// opCount is the number of operations ever created.
func (w *domainWorld) opCount() int {
	w.t.Helper()
	page, err := w.st.List(store.ListQuery{Limit: store.MaxListLimit})
	if err != nil {
		w.t.Fatal(err)
	}
	return len(page.Records)
}

func succeeded[T any](t *testing.T, rec store.OpRecord) T {
	t.Helper()
	var out T
	if rec.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s: %v", rec.Status, rec.Error)
	}
	if err := json.Unmarshal(rec.Result, &out); err != nil {
		t.Fatalf("result %s: %v", rec.Result, err)
	}
	return out
}

// TC-86 (FR-10, FR-11, CON-2): a cross-list add is 409 at the API with no
// operation; removal by (list, kind, value) works and a missing value is
// 404 before an operation; a conflicting row that appears after Admit
// fails the operation with conflict from the helper, inserting nothing.
func TestTC86_DomainAdmitBeforeOperation(t *testing.T) {
	w := newDomainWorld(t)
	allowID := w.h.SeedDomain(0, "cdn.example.com", "")

	_, pe := w.invoke(spec.AddDomain.Op(), `{"list":"deny","kind":"exact","value":"cdn.example.com"}`)
	wantProblem(t, "cross-list add", pe, problem.Conflict, 409, "")
	if !strings.Contains(pe.Detail, strconv.FormatInt(allowID, 10)) {
		t.Errorf("detail %q does not name entry %d", pe.Detail, allowID)
	}
	if n := w.opCount(); n != 0 {
		t.Fatalf("%d operations after a refused add, want 0", n)
	}

	denyID := w.h.SeedDomain(3, `^ad[s]?\.;querytype=A`, "seeded")
	rec, pe := w.invoke(spec.RemoveDomain.Op(), `{"list":"deny","kind":"regex","value":"^ad[s]?\\.;querytype=A"}`)
	if pe != nil {
		t.Fatalf("remove by value: %v", pe)
	}
	rm := succeeded[spec.DomainRemoveResult](t, rec)
	if !rm.Changed || rm.Removed.PiholeID != denyID || rm.Removed.Value != `^ad[s]?\.;querytype=A` || rm.Removed.Comment != "seeded" {
		t.Errorf("removed %+v, want entry %d", rm, denyID)
	}
	if rows := w.h.DomainRows(); len(rows) != 1 || rows[0].ID != allowID {
		t.Errorf("rows %+v, want only the allow entry", rows)
	}
	before := w.opCount()
	_, pe = w.invoke(spec.RemoveDomain.Op(), `{"list":"deny","kind":"exact","value":"nowhere.example.com"}`)
	wantProblem(t, "remove a missing value", pe, problem.NotFound, 404, "")
	if n := w.opCount(); n != before {
		t.Errorf("%d operations after a refused remove, want %d", n, before)
	}

	// The race: Admit sees no race.example.com; the row appears as allow
	// before the helper runs.
	w.mu.Lock()
	w.beforeHelper = func() { w.h.SeedDomain(0, "race.example.com", "") }
	w.mu.Unlock()
	rows := len(w.h.DomainRows())
	rec, pe = w.invoke(spec.AddDomain.Op(), `{"list":"deny","kind":"exact","value":"race.example.com"}`)
	if pe != nil {
		t.Fatalf("race add refused at Admit: %v", pe)
	}
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.Conflict {
		t.Fatalf("race add: %s %v, want failed with conflict", rec.Status, rec.Error)
	}
	got := w.h.DomainRows()
	if len(got) != rows+1 || got[len(got)-1].Type != 0 || got[len(got)-1].Domain != "race.example.com" {
		t.Errorf("rows %+v: only the racing allow row may be new", got)
	}
	if !strings.Contains(rec.Error.Detail, strconv.FormatInt(got[len(got)-1].ID, 10)) {
		t.Errorf("detail %q does not name the racing entry %d", rec.Error.Detail, got[len(got)-1].ID)
	}
	if usedListSh(w.h.Argvs()) {
		t.Errorf("list.sh was invoked: %q", w.h.Argvs())
	}
}

// add-domain through the whole path: the result is the row read back,
// named by the provider's host, and a second add is created false with no
// further reload; remove by id returns the removed row.
func TestDomainActionsEndToEnd(t *testing.T) {
	w := newDomainWorld(t)
	rec, pe := w.invoke(spec.AddDomain.Op(), `{"list":"deny","kind":"exact","value":"ads.example.com","comment":"nexus test"}`)
	if pe != nil {
		t.Fatalf("add: %v", pe)
	}
	res := succeeded[spec.DomainResult](t, rec)
	rows := w.h.DomainRows()
	if len(rows) != 1 || !res.Created || !res.Changed || res.Domain.Host != "h" ||
		res.Domain.Name != strconv.FormatInt(rows[0].ID, 10) || res.Domain.Properties.PiholeID != rows[0].ID ||
		res.Domain.Properties.Comment != "nexus test" || res.Domain.Type != spec.DomainResourceType {
		t.Fatalf("result %+v, rows %+v", res, rows)
	}
	rec, pe = w.invoke(spec.AddDomain.Op(), `{"list":"deny","kind":"exact","value":"ads.example.com"}`)
	if pe != nil {
		t.Fatalf("second add: %v", pe)
	}
	if res2 := succeeded[spec.DomainResult](t, rec); res2.Created || res2.Changed || res2.Domain.Properties.PiholeID != rows[0].ID {
		t.Errorf("second add %+v, want created false for entry %d", res2, rows[0].ID)
	}
	if reloads(w.h.FTLEvents()) != 1 {
		t.Errorf("reload-lists sent %d times, want 1", reloads(w.h.FTLEvents()))
	}
	rec, pe = w.invoke(spec.RemoveDomain.Op(), `{"id":`+strconv.FormatInt(rows[0].ID, 10)+`}`)
	if pe != nil {
		t.Fatalf("remove: %v", pe)
	}
	if rm := succeeded[spec.DomainRemoveResult](t, rec); rm.Removed.PiholeID != rows[0].ID || len(w.h.DomainRows()) != 0 {
		t.Errorf("remove %+v, rows %+v", rm, w.h.DomainRows())
	}
	if reloads(w.h.FTLEvents()) != 2 || usedListSh(w.h.Argvs()) {
		t.Errorf("events %v argvs %q", w.h.FTLEvents(), w.h.Argvs())
	}
}

// TC-E3 (FR-11, FR-12): remove-domain needs exactly one addressing form;
// otherwise 400 validation-failed naming the rule, with no operation.
func TestTCE3_RemoveDomainAddressingForms(t *testing.T) {
	w := newDomainWorld(t)
	w.h.SeedDomain(1, "ads.example.com", "")
	for _, in := range []string{
		`{"id":1,"value":"ads.example.com"}`,
		`{"list":"deny","kind":"exact"}`,
		`{}`,
	} {
		_, pe := w.invoke(spec.RemoveDomain.Op(), in)
		wantProblem(t, in, pe, problem.ValidationFailed, 400, "")
		errs, _ := pe.Extensions["errors"].([]problem.FieldError)
		if len(errs) != 1 || !strings.Contains(errs[0].Message, "give either id, or list, kind and value together") {
			t.Errorf("%s: errors %+v do not name the rule", in, errs)
		}
	}
	if n := w.opCount(); n != 0 || len(w.h.DomainRows()) != 1 {
		t.Errorf("%d operations, %d rows; want 0 and the seeded row", n, len(w.h.DomainRows()))
	}
}

// TC-ERR17 (FR-10, CON-2): an exact value that is not a lowercase domain is
// 400 validation-failed at /value before an operation; the same text as a
// regex is admitted.
func TestTCERR17_DomainAdmitValidation(t *testing.T) {
	w := newDomainWorld(t)
	for _, v := range []string{"a/b", "ADS.example.com", "*.example.com"} {
		in, _ := json.Marshal(spec.DomainAddInput{List: "deny", Kind: "exact", Value: v})
		_, pe := w.invoke(spec.AddDomain.Op(), string(in))
		wantProblem(t, v, pe, problem.ValidationFailed, 400, "/value")
	}
	if n := w.opCount(); n != 0 || len(w.h.DomainRows()) != 0 {
		t.Fatalf("%d operations, %d rows; want none", n, len(w.h.DomainRows()))
	}
	rec, pe := w.invoke(spec.AddDomain.Op(), `{"list":"deny","kind":"regex","value":"*.example.com"}`)
	if pe != nil {
		t.Fatalf("regex add: %v", pe)
	}
	if res := succeeded[spec.DomainResult](t, rec); !res.Created || res.Domain.Properties.Kind != spec.KindRegex {
		t.Errorf("regex add %+v", res)
	}
}

// add-domain and remove-domain are bound detached with MaxRuntime 10 min
// and the LostDetail a lost unit's outcome-unknown carries.
func TestDomainBindings(t *testing.T) {
	const lost = "the entry may have been added or removed; lists may not be reloaded"
	w := newHelperWorld(t)
	check := func(id string, df provider.DetachedFuncs, in any) {
		run := provider.NewRun("op-lost-"+id, nil)
		s, err := df.Prepare(context.Background(), in, run)
		if err != nil || s.MaxRuntime != 10*time.Minute {
			t.Errorf("%s: Prepare MaxRuntime %s, %v; want 10m", id, s.MaxRuntime, err)
		}
		if df.LostDetail == nil || df.LostDetail(run, in) != lost {
			t.Errorf("%s: LostDetail is not %q", id, lost)
		}
		_, err = df.Finalize(context.Background(), in, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
		if slug, pe := slugOf(t, err); slug != problem.OutcomeUnknown || !strings.Contains(pe.Detail, lost) {
			t.Errorf("%s: lost unit: %v %q; want outcome-unknown containing %q", id, slug, pe.Detail, lost)
		}
	}
	found := 0
	for _, b := range w.p.Bindings() {
		switch b.Op().ID {
		case spec.AddDomain.Op().ID:
			df, _ := b.Detached()
			check("add", df, spec.DomainAddInput{List: "deny", Kind: "exact", Value: "a.example.com"})
			found++
		case spec.RemoveDomain.Op().ID:
			df, _ := b.Detached()
			check("remove", df, spec.DomainRemoveInput{ID: 1})
			found++
		}
	}
	if found != 2 {
		t.Errorf("%d of add-domain and remove-domain bound, want 2", found)
	}
}

// helperOp runs the bound op opID as the core does: Admit, Prepare, the
// helper on the operation's directory, then Finalize. It returns the typed
// result or the operation's problem, and the operation log.
func helperOp(w *helperWorld, opID, id string, in any) (any, string, error) {
	w.t.Helper()
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == opID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		w.t.Fatalf("%s is not bound", opID)
	}
	ctx := context.Background()
	if df.Admit != nil {
		if err := df.Admit(ctx, in); err != nil {
			w.t.Fatalf("Admit: %v", err)
		}
	}
	if _, err := df.Prepare(ctx, in, provider.NewRun(id, nil)); err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	var out bytes.Buffer
	code := runHelper(ctx, filepath.Join(w.opsDir, id), &out, &out, w.inject())
	res, err := df.Finalize(ctx, in, detach.Exit{ExitCode: code}, provider.NewRun(id, nil))
	return res, out.String(), err
}

// probesAfter counts the barrier's probe queries FakeFTL received once at
// least reloads list reloads had completed.
func probesAfter(f *v5test.FakeFTL, reloads int) int {
	n := 0
	for _, q := range f.Queries() {
		if q.Reloads >= reloads && strings.HasPrefix(q.Name, "nexus-probe-") && strings.HasSuffix(q.Name, ".invalid") &&
			len(strings.TrimSuffix(strings.TrimPrefix(q.Name, "nexus-probe-"), ".invalid")) == 16 {
			n++
		}
	}
	return n
}

// TC-155 (FR-10 to FR-12, NFR-7, A-64, A-67) [hazard: reload-race]:
// add-domain, remove-domain and remove-adlist, each run through its helper
// action, return only after the FTL list reload it triggered has completed:
// FakeFTL takes 300 ms to reload, as a long regex list makes real FTL do.
// On the pre-fix code each returned as soon as `pihole restartdns
// reload-lists` exited, with the reload still running.
func TestTC155_OpsReturnAfterFTLReloadedLists(t *testing.T) {
	w := newHelperWorld(t)
	f := w.h.FTL()
	listID := seedAdlist(t, w.h, listA)
	if code, err := w.h.Run(context.Background(), []string{piholeBin, "-g"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("gravity: %d %v", code, err)
	}
	f.SetReloadDuration(300 * time.Millisecond)
	steps := []struct {
		name string
		opID string
		in   func() any
	}{
		{"add-domain", spec.AddDomain.Op().ID, func() any {
			return spec.DomainAddInput{List: "deny", Kind: "exact", Value: "tc155.example.com"}
		}},
		{"remove-domain", spec.RemoveDomain.Op().ID, func() any {
			return spec.DomainRemoveInput{List: "deny", Kind: "exact", Value: "tc155.example.com"}
		}},
		{"remove-adlist", spec.RemoveAdlist.Op().ID, func() any { return spec.AdlistRemoveInput{ID: listID} }},
	}
	for i, s := range steps {
		before, argvs := f.ReloadsCompleted(), len(w.h.Argvs())
		_, log, err := helperOp(w, s.opID, "op-tc155-"+strconv.Itoa(i), s.in())
		completed, stuck := f.ReloadsCompleted(), f.Stuck()
		if err != nil {
			t.Fatalf("%s: %v\n%s", s.name, err, log)
		}
		if completed != before+1 || stuck {
			t.Errorf("%s returned with %d reloads completed (was %d), stuck %v: it must return after its reload ended",
				s.name, completed, before, stuck)
		}
		if n := countReloadLists(w.h.Argvs()[argvs:]); n != 1 {
			t.Errorf("%s: %d reload-lists, want exactly 1 after the commit", s.name, n)
		}
		if !strings.Contains(log, "pihole: FTL reloaded its lists in ") {
			t.Errorf("%s log:\n%s", s.name, log)
		}
		if probesAfter(f, before+1) == 0 {
			t.Errorf("%s: no nexus-probe-<16 hex>.invalid query after its reload", s.name)
		}
	}
}

func countReloadLists(argvs [][]string) int {
	n := 0
	for _, a := range argvs {
		if len(a) == 3 && a[0] == piholeBin && a[1] == "restartdns" && a[2] == "reload-lists" {
			n++
		}
	}
	return n
}
