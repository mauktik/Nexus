package ops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

// TC-47: a long-running op answers 202 with Location and Retry-After, and
// polling shows Running then Succeeded with the result and ordered times.
func TestLongRunningAcknowledgementAndTerminalRecord(t *testing.T) {
	perBackend(t, testLongRunningAcknowledgementAndTerminalRecord)
}

func testLongRunningAcknowledgementAndTerminalRecord(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	w.serve()
	tok := w.mint("deployer", "test:write", "test:read")
	g := w.tp.gated("web")

	resp, body := w.do(http.MethodPost, "/v1/providers/tp/things/web/run", tok, `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST: %d %s", resp.StatusCode, body)
	}
	op := decodeOp(t, body)
	if loc := resp.Header.Get("Location"); loc != "/v1/operations/"+op.ID {
		t.Fatalf("Location = %q, want /v1/operations/%s", loc, op.ID)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want 1", ra)
	}
	if op.Status != apitypes.StatusQueued && op.Status != apitypes.StatusRunning {
		t.Fatalf("202 status = %s", op.Status)
	}
	if op.Host != "testhost" || op.Action != "tp.things.run" || op.Provider != "tp" || op.Target != "web" || op.TokenName != "deployer" {
		t.Fatalf("202 body = %+v", op)
	}

	g.waitStarted(t)
	resp, body = w.do(http.MethodGet, resp.Header.Get("Location"), tok, "")
	if got := decodeOp(t, body); resp.StatusCode != http.StatusOK || got.ID != op.ID || got.Status != apitypes.StatusRunning ||
		got.Started == nil || got.Ended != nil {
		t.Fatalf("while gated: %d %s", resp.StatusCode, body)
	}
	g.open()
	var final apitypes.Operation
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = w.do(http.MethodGet, "/v1/operations/"+op.ID, tok, "")
		final = decodeOp(t, body)
		if final.Status.Terminal() || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final.ID != op.ID || final.Status != apitypes.StatusSucceeded || final.Error != nil ||
		string(final.Result) != `{"changed":true,"name":"web"}` || final.TokenName != "deployer" ||
		final.Target != "web" || final.Action != "tp.things.run" {
		t.Fatalf("final = %s", body)
	}
	if final.Started == nil || final.Ended == nil || final.Started.Before(final.Created) || final.Ended.Before(*final.Started) {
		t.Fatalf("timestamps out of order: created %v started %v ended %v", final.Created, final.Started, final.Ended)
	}
}

// TC-48: every state change has one audit record. The terminal change is
// audited first and committed second; the transitions out of Queued are a
// compare-and-set first and an audit second (WI 3.2: only the
// compare-and-set decides which happened). Creation commits, then writes
// the Queued record, since the ID exists only once committed.
func TestStateChangesAuditThenCommit(t *testing.T) { perBackend(t, testStateChangesAuditThenCommit) }

func testStateChangesAuditThenCommit(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	w.tp.on("bad", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		return thingOut{}, problem.New(problem.DependencyUnavailable, "the dependency is down").With("provider", "tp")
	})
	p := who("ops", "test:write")
	good := w.start(p, tpOps.run.Op(), `{"name":"good"}`, "")
	bad := w.start(p, tpOps.run.Op(), `{"name":"bad"}`, "")
	w.waitStatus(good.ID, apitypes.StatusSucceeded)
	w.waitStatus(bad.ID, apitypes.StatusFailed)
	// The worker returns right after its commit; let the manager finish.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	events := w.seq.snapshot()
	at := func(ev string) int {
		i := slices.Index(events, ev)
		if i < 0 {
			t.Fatalf("missing event %q in %q", ev, events)
		}
		if slices.Index(events[i+1:], ev) >= 0 {
			t.Fatalf("event %q happened twice in %q", ev, events)
		}
		return i
	}
	for id, end := range map[string]apitypes.OperationStatus{good.ID: apitypes.StatusSucceeded, bad.ID: apitypes.StatusFailed} {
		if at("commit Queued "+id) > at("audit Queued "+id) {
			t.Errorf("%s: Queued audited before it was created", id)
		}
		if at("commit Running "+id) > at("audit Running "+id) {
			t.Errorf("%s: Running audited before the compare-and-set", id)
		}
		if at("audit "+string(end)+" "+id) > at("commit "+string(end)+" "+id) {
			t.Errorf("%s: %s committed before it was audited", id, end)
		}
	}
	recs := w.records()
	for id, want := range map[string][]string{good.ID: {"Queued", "Running", "Succeeded"}, bad.ID: {"Queued", "Running", "Failed"}} {
		var got []string
		for _, r := range recs {
			if r.OperationID == id {
				got = append(got, r.Outcome)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: state-change records %v, want %v", id, got, want)
		}
	}
	if f := filterRecs(recs, outcome("Failed")); len(f) != 1 || f[0].Problem != string(problem.DependencyUnavailable) {
		t.Errorf("Failed record = %+v", f)
	}
}

// TC-51: a run that returns a problem, a plain error, or panics fails with a
// structured error; the panic is contained to its operation and logged with
// its stack, and the next operation on the lane runs.
func TestOperationFailureAndPanicContainment(t *testing.T) {
	perBackend(t, testOperationFailureAndPanicContainment)
}

func testOperationFailureAndPanicContainment(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	const sentinel = "SENTINEL-77aa-run-failure"
	w.tp.on("prob", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		return thingOut{}, problem.New(problem.DependencyUnavailable, "the dependency is down").With("provider", "tp")
	})
	w.tp.on("plain", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		return thingOut{}, errors.New("open /secret/" + sentinel + ": permission denied")
	})
	w.tp.on("boom", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		panic(sentinel)
	})
	p := who("ops", "test:write")
	want := map[string]problem.Slug{"prob": problem.DependencyUnavailable, "plain": problem.InternalError, "boom": problem.InternalError}
	for _, name := range []string{"prob", "plain", "boom"} {
		op := w.start(p, tpOps.run.Op(), `{"name":"`+name+`","lane":"l1"}`, "")
		rec := w.waitStatus(op.ID, apitypes.StatusFailed)
		if rec.Error == nil || rec.Error.Slug() != want[name] {
			t.Fatalf("%s: error %+v, want %s", name, rec.Error, want[name])
		}
		b, err := json.Marshal(rec.ToAPI("testhost"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), sentinel) {
			t.Fatalf("%s: the cause reached the operation: %s", name, b)
		}
		var api map[string]any
		if err := json.Unmarshal(b, &api); err != nil {
			t.Fatal(err)
		}
		if e, ok := api["error"].(map[string]any); !ok || e["type"] != "urn:nexus:problem:"+string(want[name]) {
			t.Fatalf("%s: wire error = %v", name, api["error"])
		}
	}
	logs := w.logs.String()
	if !strings.Contains(logs, `"msg":"operation panic"`) || !strings.Contains(logs, "runtime/debug.Stack") {
		t.Fatalf("the panic and its stack were not logged:\n%s", logs)
	}
	next := w.start(p, tpOps.run.Op(), `{"name":"after","lane":"l1"}`, "")
	w.waitStatus(next.ID, apitypes.StatusSucceeded)
}

// TC-52: the status set is exactly the five states, a terminal state never
// changes, and a mutating op starts with no approval step.
func TestOperationStatusSetHasNoApprovalState(t *testing.T) {
	perBackend(t, testOperationStatusSetHasNoApprovalState)
}

func testOperationStatusSetHasNoApprovalState(t *testing.T, _ detachtest.Backend) {
	all := []apitypes.OperationStatus{apitypes.StatusQueued, apitypes.StatusRunning, apitypes.StatusSucceeded,
		apitypes.StatusFailed, apitypes.StatusCanceled}
	if got := []string{"Queued", "Running", "Succeeded", "Failed", "Canceled"}; !slices.Equal(got, func() []string {
		var s []string
		for _, x := range all {
			s = append(s, string(x))
		}
		return s
	}()) {
		t.Fatalf("status set = %v", all)
	}
	for _, s := range all {
		if s.Terminal() != (s == apitypes.StatusSucceeded || s == apitypes.StatusFailed || s == apitypes.StatusCanceled) {
			t.Fatalf("%s: Terminal() = %v", s, s.Terminal())
		}
	}

	w := newWorld(t)
	p := who("ops", "test:write")
	op := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "")
	done := w.waitStatus(op.ID, apitypes.StatusSucceeded)

	// Illegal transitions out of a terminal state, through every path.
	if _, err := w.mgr.Cancel(context.Background(), op.ID, func(string) def.Cancel { return def.CancelRunning }); err == nil {
		t.Fatal("cancel of a Succeeded operation succeeded")
	} else {
		wantProblem(t, err, problem.NotCancelable)
	}
	if _, err := w.store.CommitTerminal(op.ID, apitypes.StatusRunning, apitypes.StatusFailed, nil,
		problem.New(problem.InternalError, "x")); !errors.Is(err, store.ErrStatusConflict) {
		t.Fatalf("Running->Failed on a Succeeded record: %v", err)
	}
	for _, to := range all {
		if _, err := w.store.CompareAndSetStatus(op.ID, apitypes.StatusSucceeded, to); err == nil {
			t.Fatalf("Succeeded->%s allowed", to)
		}
	}
	if _, err := w.store.CompareAndSetStatus(op.ID, apitypes.StatusRunning, "Approved"); err == nil {
		t.Fatal("an approval state was accepted")
	}
	if after, err := w.store.Get(op.ID); err != nil || after.Status != done.Status || !after.Ended.Equal(*done.Ended) {
		t.Fatalf("terminal record changed: %+v %v", after, err)
	}
	// No approval step: the only records of the op are its lifecycle.
	var outcomes []string
	for _, r := range w.records() {
		if r.OperationID == op.ID {
			outcomes = append(outcomes, r.Outcome)
		}
	}
	if !slices.Equal(outcomes, []string{"Queued", "Running", "Succeeded"}) {
		t.Fatalf("lifecycle = %v", outcomes)
	}
}

// TC-33: the requested record is durable before the handler runs: the
// handler finds it in the audit file on disk, after the writes that synced
// it. The record carries every FR-24 field; the operation ID it lacks at
// that moment is carried by the operation's Queued record, which names it.
func TestAuditRecordIsDurableBeforeExecution(t *testing.T) {
	perBackend(t, testAuditRecordIsDurableBeforeExecution)
}

func testAuditRecordIsDurableBeforeExecution(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	type seen struct {
		file   []byte
		writes int64
	}
	got := make(chan seen, 1)
	w.tp.on("audited", func(context.Context, thingIn, *provider.Run) (thingOut, error) {
		b, err := os.ReadFile(filepath.Join(w.auditDir, audit.FileName))
		if err != nil {
			return thingOut{}, err
		}
		got <- seen{file: b, writes: w.aud.writes.Load()}
		return thingOut{Name: "audited"}, nil
	})
	p := who("ops", "test:write")
	p.Source = netip.MustParseAddr("192.0.2.9")
	op := w.start(p, tpOps.run.Op(), `{"name":"audited","secret":"SENTINELSECRET","note":"hello"}`, "")
	s := <-got
	w.waitStatus(op.ID, apitypes.StatusSucceeded)

	// requested, Queued and Running were each written and synced (Write
	// returns only after fsync) before the handler ran.
	if s.writes < 3 {
		t.Fatalf("%d audit writes before the handler, want at least 3", s.writes)
	}
	var req, queued *systemspec.AuditRecord
	for _, line := range bytes.Split(bytes.TrimSpace(s.file), []byte("\n")) {
		var r systemspec.AuditRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		switch {
		case r.Outcome == audit.OutcomeRequested && r.Action == "tp.things.run":
			req = &r
		case r.Outcome == "Queued" && r.OperationID == op.ID:
			queued = &r
		}
	}
	if req == nil || queued == nil {
		t.Fatalf("at execution time the file holds no requested or Queued record:\n%s", s.file)
	}
	if req.Time.IsZero() || req.ID == "" || req.Host != "testhost" || req.TokenName != "ops" || req.TokenID != p.TokenID ||
		req.Source != "192.0.2.9" || req.Surface != "rest" || req.Provider != "tp" || req.Action != "tp.things.run" ||
		req.Target != "audited" {
		t.Fatalf("requested record = %+v", req)
	}
	if string(req.Params) != `{"name":"audited","note":"hello","secret":"[REDACTED]"}` {
		t.Fatalf("params = %s", req.Params)
	}
	if queued.RequestID != req.ID || queued.TokenName != "ops" || queued.Target != "audited" {
		t.Fatalf("Queued record %+v does not name requested record %s", queued, req.ID)
	}
}

// TC-35: sensitive values and the bearer token never enter the audit log;
// the field names remain with the redaction marker.
func TestSensitiveValuesAndTokensNeverEnterAudit(t *testing.T) {
	perBackend(t, testSensitiveValuesAndTokensNeverEnterAudit)
}

func testSensitiveValuesAndTokensNeverEnterAudit(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	w.serve()
	tok := w.mint("deployer", "test:write", "test:read")
	const top, nested = "SENTINELTOPA1", "SENTINELNESTEDB2"
	body := `{"secret":"` + top + `","nested":{"key":"` + nested + `","plain":"visible"}}`
	resp, b := w.do(http.MethodPost, "/v1/providers/tp/things/web/run", tok, body, "Idempotency-Key", "k-35")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST: %d %s", resp.StatusCode, b)
	}
	id := decodeOp(t, b).ID
	w.waitStatus(id, apitypes.StatusSucceeded)
	if resp, b := w.do(http.MethodPost, "/v1/providers/tp/things/web/run", tok, body, "Idempotency-Key", "k-35"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay: %d %s", resp.StatusCode, b)
	}

	raw, err := os.ReadFile(filepath.Join(w.auditDir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	secret := tok[strings.LastIndex(tok[:17], "_")+1:]
	for _, s := range []string{top, nested, tok, secret} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatalf("%q is in the audit log", s)
		}
	}
	n := 0
	for _, r := range w.records() {
		if r.Outcome != audit.OutcomeRequested && r.Outcome != audit.OutcomeReplayed {
			continue
		}
		n++
		want := `{"name":"web","nested":{"key":"[REDACTED]","plain":"visible"},"secret":"[REDACTED]"}`
		if string(r.Params) != want {
			t.Fatalf("%s params = %s, want %s", r.Outcome, r.Params, want)
		}
	}
	if n != 2 {
		t.Fatalf("%d records with params, want requested and replayed", n)
	}
}

// TC-222: no OpRecord internal field reaches any API body or the wire type,
// and state.db holds the ordinary parameter but not the sensitive one.
func TestAPIResponsesContainNoInternalFields(t *testing.T) {
	perBackend(t, testAPIResponsesContainNoInternalFields)
}

func testAPIResponsesContainNoInternalFields(t *testing.T, _ detachtest.Backend) {
	// The internal fields are those of store.OpRecord that the wire type
	// lacks, computed by reflection rather than typed in.
	wire := map[string]bool{}
	ot := reflect.TypeFor[apitypes.Operation]()
	for i := range ot.NumField() {
		wire[ot.Field(i).Name] = true
	}
	var internal []string
	rt := reflect.TypeFor[store.OpRecord]()
	for i := range rt.NumField() {
		if f := rt.Field(i); !wire[f.Name] && f.Name != "Host" {
			internal = append(internal, f.Name)
		}
	}
	for _, f := range []string{"Input", "HasSensitive", "Exec", "StartScope", "Lane"} {
		if !slices.Contains(internal, f) {
			t.Fatalf("internal fields %v lack %s", internal, f)
		}
	}
	lowerFirst := func(s string) string { return strings.ToLower(s[:1]) + s[1:] }
	wireKeys := map[string]bool{}
	for i := range ot.NumField() {
		name, _, _ := strings.Cut(ot.Field(i).Tag.Get("json"), ",")
		wireKeys[name] = true
	}
	for _, f := range internal {
		if wireKeys[lowerFirst(f)] || wireKeys[strings.ToLower(f)] {
			t.Fatalf("the wire type exposes %s", f)
		}
	}

	w := newWorld(t)
	w.serve()
	tok := w.mint("deployer", "test:write", "test:read")
	const sentinel, ordinary = "SENTINELSECRET222", "ordinary-note-222"
	g := w.tp.gated("held")
	// One op holds the lane so the second stays Queued and can be canceled.
	resp, held := w.do(http.MethodPost, "/v1/providers/tp/things/held/run", tok, `{"lane":"l222"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST held: %d %s", resp.StatusCode, held)
	}
	g.waitStarted(t)
	resp, accepted := w.do(http.MethodPost, "/v1/providers/tp/things/web/run", tok,
		`{"lane":"l222","secret":"`+sentinel+`","note":"`+ordinary+`"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST: %d %s", resp.StatusCode, accepted)
	}
	id := decodeOp(t, accepted).ID
	_, got := w.do(http.MethodGet, "/v1/operations/"+id, tok, "")
	_, list := w.do(http.MethodGet, "/v1/operations", tok, "")
	resp, canceled := w.do(http.MethodPost, "/v1/operations/"+id+"/cancel", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d %s", resp.StatusCode, canceled)
	}
	g.open()

	var keys func(v any, out map[string]bool)
	keys = func(v any, out map[string]bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				out[k] = true
				if k != "result" {
					keys(c, out)
				}
			}
		case []any:
			for _, c := range x {
				keys(c, out)
			}
		}
	}
	for name, body := range map[string][]byte{"202": accepted, "get": got, "list": list, "cancel": canceled} {
		if bytes.Contains(body, []byte(sentinel)) {
			t.Fatalf("%s body holds the sensitive value: %s", name, body)
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		keys(v, found)
		for _, f := range internal {
			if found[lowerFirst(f)] || found[strings.ToLower(f)] {
				t.Fatalf("%s body exposes internal field %s: %s", name, f, body)
			}
		}
	}

	raw, err := os.ReadFile(filepath.Join(w.stateDir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(ordinary)) {
		t.Fatal("state.db lacks the ordinary parameter")
	}
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Fatal("state.db holds the sensitive value")
	}
	rec, err := w.store.Get(id)
	if err != nil || !rec.HasSensitive || rec.StartScope != "test:write" || rec.Exec != store.ExecInProcess ||
		rec.Lane.Key != "l222" || rec.Lane.Policy != store.LaneQueue || rec.Lane.Limit != 1 {
		t.Fatalf("record = %+v %v", rec, err)
	}
}

// Read-risk requests (a read-only long-running op, or whatIf) follow the
// mutating path without the storage check, ignore their key, write no
// requested, refused or replayed record, and are not refused by a failing
// audit log; their state changes are still audited (Data Flow).
func TestReadRiskRequests(t *testing.T) { perBackend(t, testReadRiskRequests) }

func testReadRiskRequests(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	p := who("ops", "test:read", "test:write")
	w.overFull.Store(true)
	a := w.start(p, tpOps.check.Op(), `{"name":"c"}`, "K")
	b := w.start(p, tpOps.check.Op(), `{"name":"c"}`, "K")
	if a.ID == b.ID {
		t.Fatal("a read-risk request replayed by key")
	}
	wi := w.start(p, tpOps.run.Op(), `{"name":"w","whatIf":true}`, "")
	for _, id := range []string{a.ID, b.ID, wi.ID} {
		w.waitStatus(id, apitypes.StatusSucceeded)
	}
	_, err := w.call(p, tpOps.run.Op(), `{"name":"w"}`, "")
	wantProblem(t, err, problem.StorageLimitReached)
	w.overFull.Store(false)

	w.aud.mode.Store(failWrite)
	c := w.start(p, tpOps.check.Op(), `{"name":"c"}`, "")
	w.waitStatus(c.ID, apitypes.StatusSucceeded)
	w.aud.mode.Store(failNone)

	recs := w.records()
	for _, r := range recs {
		if r.Action == tpOps.check.Op().ID || (r.Action == "tp.things.run" && r.Target == "w" && r.Outcome != audit.OutcomeRefused) {
			switch r.Outcome {
			case audit.OutcomeRequested, audit.OutcomeRefused, audit.OutcomeReplayed:
				t.Fatalf("read-risk request wrote %+v", r)
			}
		}
	}
	var changes []string
	for _, r := range recs {
		if r.OperationID == a.ID {
			changes = append(changes, r.Outcome)
		}
	}
	if !slices.Equal(changes, []string{"Queued", "Running", "Succeeded"}) {
		t.Fatalf("read-risk state changes = %v", changes)
	}
}

// The storage hook (4.3) refuses new mutations at Data Flow step 5, after the
// idempotency lookup, so a replay at the limit returns the original (F2).
func TestStorageLimitRefusesNewMutationsButNotReplays(t *testing.T) {
	perBackend(t, testStorageLimitRefusesNewMutationsButNotReplays)
}

func testStorageLimitRefusesNewMutationsButNotReplays(t *testing.T, _ detachtest.Backend) {
	w := newWorld(t)
	p := who("ops", "test:write")
	op := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	w.overFull.Store(true)
	if r := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K"); r.ID != op.ID {
		t.Fatalf("replay at the limit: %s", r.ID)
	}
	_, err := w.call(p, tpOps.run.Op(), `{"name":"web"}`, "K2")
	if pe := wantProblem(t, err, problem.StorageLimitReached); pe.Status != 507 {
		t.Fatalf("%+v", pe)
	}
	if w.tp.admitted.Load() != 1 {
		t.Fatalf("Admit ran %d times; the storage check precedes it", w.tp.admitted.Load())
	}
	refused := filterRecs(w.records(), outcome(audit.OutcomeRefused))
	if len(refused) != 1 || refused[0].Problem != string(problem.StorageLimitReached) {
		t.Fatalf("refused = %+v", refused)
	}
}
