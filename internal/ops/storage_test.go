// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/storage"
	"github.com/mauktik/nexus/internal/systemspec"
)

// retention is real operation retention over the world's state, on the
// world's clock.
func (w *world) retention() *ops.Retention {
	w.t.Helper()
	r, err := ops.NewRetention(ops.RetentionOptions{Store: w.store, Dir: filepath.Join(w.stateDir, "ops"), Now: w.now, Logger: w.log})
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

// accountant installs a real storage accountant with budget over the
// world's state.db, ops directory and audit directory; the world's storage
// hook then answers from its limit flag.
func (w *world) accountant(budget int64) *storage.Accountant {
	w.t.Helper()
	a, err := storage.New(storage.Options{
		BudgetBytes: budget, StateDB: w.store.Path(), OpsDir: filepath.Join(w.stateDir, "ops"),
		AuditDir: w.auditDir, Retention: w.retention(), Audit: w.alog, Now: w.now, Logger: w.log,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	w.acct.Store(a)
	return a
}

// atLimit installs an accountant whose budget nothing fits and runs a pass,
// so the limit flag is set.
func (w *world) atLimit() {
	w.t.Helper()
	if r := w.accountant(1).Account(); !r.Limited {
		w.t.Fatalf("a 1-byte budget left the flag clear: %+v", r)
	}
}

// usage is what the accountant measures, computed independently of it.
func (w *world) usage() int64 {
	w.t.Helper()
	n := int64(0)
	for _, d := range []string{w.stateDir, w.auditDir} {
		s, err := storage.DirSize(d)
		if err != nil {
			w.t.Fatal(err)
		}
		n += s
	}
	return n
}

func (w *world) outputSize(id string) int64 {
	w.t.Helper()
	n, err := storage.DirSize(filepath.Join(w.opDir(id), "output"))
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) evicted(id string) bool {
	w.t.Helper()
	rec, err := w.store.Get(id)
	if err != nil {
		w.t.Fatal(err)
	}
	return rec.OutputEvicted
}

// writeAuditFile writes a rotated audit file of n random bytes, rotated and
// last modified at at, and returns its path and hash.
func writeAuditFile(t *testing.T, dir string, at time.Time, n int) (string, [32]byte) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, audit.RotatedName(at))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p, sha256.Sum256(b)
}

func fileHash(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("young audit file: %v", err)
	}
	return sha256.Sum256(b)
}

const outChunk = 512 << 10

// writesOutput makes runs of name write outChunk bytes of output, then
// wait for release when it is non-nil.
func (p *testProv) writesOutput(name string, wrote chan<- struct{}, release <-chan struct{}) {
	p.on(name, func(ctx context.Context, in thingIn, r *provider.Run) (thingOut, error) {
		if _, err := r.Output().Write(bytes.Repeat([]byte("x"), outChunk)); err != nil {
			return thingOut{}, err
		}
		if wrote != nil {
			close(wrote)
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return thingOut{}, ctx.Err()
			}
		}
		return thingOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
	})
}

// TC-88: over budget, output is evicted oldest first with the records kept
// and outputEvicted set, never a running operation's; then audit files
// older than retention go; only then is the limit flag set. Young audit
// files are never touched. At the limit a mutation is 507 while reads
// succeed, and freeing space clears the flag.
func TestOverBudgetEvictionOrderAndLimitFlag(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:read", "test:write")
	run := tpOps.run.Op()

	var term []string
	for _, n := range []string{"a", "b", "c"} {
		w.tp.writesOutput(n, nil, nil)
		o := w.start(p, run, `{"name":"`+n+`"}`, "")
		w.waitStatus(o.ID, apitypes.StatusSucceeded)
		term = append(term, o.ID)
	}
	wrote, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	free := func() { releaseOnce.Do(func() { close(release) }) }
	defer free()
	w.tp.writesOutput("r", wrote, release)
	running := w.start(p, run, `{"name":"r"}`, "")
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the running operation never wrote its output")
	}
	runningOut := w.outputSize(running.ID)
	if runningOut < outChunk {
		t.Fatalf("running output is %d bytes", runningOut)
	}

	now := time.Now()
	oldPath, _ := writeAuditFile(t, w.auditDir, now.Add(-200*24*time.Hour), 2*outChunk)
	youngPath, youngHash := writeAuditFile(t, w.auditDir, now.Add(-24*time.Hour), 64<<10)

	// Pass 1: a little more than one output over budget, so the two oldest
	// outputs go and nothing else.
	a := w.accountant(w.usage() - w.outputSize(term[0]) - 64<<10)
	r := a.Account()
	if !slices.Equal(r.Evicted, term[:2]) || r.Limited {
		t.Fatalf("pass 1: evicted %v (want %v), limited %v", r.Evicted, term[:2], r.Limited)
	}
	for i, id := range term {
		rec, err := w.store.Get(id)
		if err != nil || rec.Status != apitypes.StatusSucceeded {
			t.Fatalf("record %s after eviction: %+v %v", id, rec, err)
		}
		if gone := i < 2; rec.OutputEvicted != gone || (w.outputSize(id) == 0) != gone {
			t.Fatalf("op %d: outputEvicted %v, output %d bytes", i, rec.OutputEvicted, w.outputSize(id))
		}
	}
	res, err := w.call(p, systemspec.OperationsGet.Op(), `{"id":"`+term[0]+`"}`, "")
	if err != nil || !res.Body.(apitypes.Operation).OutputEvicted {
		t.Fatalf("API read of an evicted operation: %+v %v", res.Body, err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("pass 1 deleted the old audit file although output sufficed: %v", err)
	}

	// Pass 2: evicting the last terminal output is not enough; the old
	// audit file goes before any limit.
	a = w.accountant(w.usage() - w.outputSize(term[2]) - outChunk)
	r = a.Account()
	if !slices.Equal(r.Evicted, term[2:]) || r.Limited {
		t.Fatalf("pass 2: evicted %v (want %v), limited %v", r.Evicted, term[2:], r.Limited)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old audit file still there: %v", err)
	}
	if fileHash(t, youngPath) != youngHash {
		t.Fatal("young audit file changed")
	}
	if w.outputSize(running.ID) != runningOut || w.evicted(running.ID) {
		t.Fatal("a running operation's output was touched")
	}

	// Step 2: only young audit and running output are left to free.
	a = w.accountant(w.usage() - 128<<10)
	r = a.Account()
	if !r.Limited || len(r.Evicted) != 0 {
		t.Fatalf("step 2: %+v, want limited with nothing evicted", r)
	}
	if fileHash(t, youngPath) != youngHash || w.outputSize(running.ID) != runningOut {
		t.Fatal("at the limit, young audit or running output was touched")
	}
	runs := w.tp.runs.Load()
	_, err = w.call(p, run, `{"name":"m"}`, "")
	if pe := wantProblem(t, err, problem.StorageLimitReached); pe.Status != http.StatusInsufficientStorage {
		t.Fatalf("status %d", pe.Status)
	}
	if _, err := w.call(p, tpOps.peek.Op(), `{"name":"m"}`, ""); err != nil {
		t.Fatalf("a sync read at the limit: %v", err)
	}
	if _, err := w.call(p, systemspec.OperationsGet.Op(), `{"id":"`+running.ID+`"}`, ""); err != nil {
		t.Fatalf("an operation read at the limit: %v", err)
	}
	if _, err := w.call(p, systemspec.OperationsList.Op(), `{}`, ""); err != nil {
		t.Fatalf("an operation list at the limit: %v", err)
	}

	// Step 3: the running operation ends, so its output may go; the next
	// pass frees it and clears the flag.
	free()
	w.waitStatus(running.ID, apitypes.StatusSucceeded)
	r = a.Account()
	if r.Limited || !slices.Equal(r.Evicted, []string{running.ID}) {
		t.Fatalf("step 3: %+v", r)
	}
	if fileHash(t, youngPath) != youngHash {
		t.Fatal("young audit file changed")
	}
	o := w.start(p, run, `{"name":"m"}`, "")
	w.waitStatus(o.ID, apitypes.StatusSucceeded)
	if w.tp.runs.Load() != runs+1 {
		t.Fatalf("runs %d -> %d", runs, w.tp.runs.Load())
	}
}

// TC-206: at the storage limit, a replay returns the original operation,
// because the idempotency lookup precedes the storage check, and is
// audited replayed; a new key with the same body is 507 and audited
// refused.
func TestReplayAtTheStorageLimitReturnsTheOriginal(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	op := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	w.atLimit()

	if r := w.start(p, tpOps.run.Op(), `{"name":"web"}`, "K"); r.ID != op.ID {
		t.Fatalf("replay at the limit answered %s, want %s", r.ID, op.ID)
	}
	_, err := w.call(p, tpOps.run.Op(), `{"name":"web"}`, "K-new")
	if pe := wantProblem(t, err, problem.StorageLimitReached); pe.Status != http.StatusInsufficientStorage {
		t.Fatalf("status %d", pe.Status)
	}
	recs := w.records()
	replayed := filterRecs(recs, outcome(audit.OutcomeReplayed))
	if len(replayed) != 1 || replayed[0].OperationID != op.ID {
		t.Fatalf("replayed = %+v", replayed)
	}
	refused := filterRecs(recs, outcome(audit.OutcomeRefused))
	if len(refused) != 1 || refused[0].Problem != string(problem.StorageLimitReached) {
		t.Fatalf("refused = %+v", refused)
	}
	if n := w.countOps(); n != 1 {
		t.Fatalf("%d operations exist, want 1", n)
	}
}

// TC-207: cancel has no storage check: at the limit a new mutation is 507,
// while a queued and a running CancelRunning operation are both canceled,
// and each cancel is audited.
func TestCancelSucceedsAtTheStorageLimit(t *testing.T) {
	w := localWorld(t)
	w.td.onPrepare("long", func(context.Context, thingIn) (detach.Spec, error) {
		return shJob("echo pid=$$; while :; do sleep 0.1; done"), nil
	})
	p := who("ops", "td:write")
	job := tdOps.job.Op()
	running := w.start(p, job, `{"name":"long","lane":"l207","policy":"queue","limit":1}`, "")
	w.childPID(running.ID)
	queued := w.start(p, job, `{"name":"next","lane":"l207","policy":"queue","limit":1}`, "")
	if rec, err := w.store.Get(queued.ID); err != nil || rec.Status != apitypes.StatusQueued {
		t.Fatalf("second operation: %+v %v, want Queued", rec, err)
	}
	w.atLimit()

	_, err := w.call(p, job, `{"name":"other"}`, "")
	wantProblem(t, err, problem.StorageLimitReached)

	w.mustCancel(p, queued.ID, "")
	w.mustCancel(p, running.ID, "")
	w.waitStatus(queued.ID, apitypes.StatusCanceled)
	w.waitStatus(running.ID, apitypes.StatusCanceled)

	for _, id := range []string{queued.ID, running.ID} {
		var requested, canceled int
		for _, r := range w.records() {
			if r.OperationID != id {
				continue
			}
			switch {
			case r.Action == cancelOp && r.Outcome == audit.OutcomeRequested:
				requested++
			case r.Outcome == string(apitypes.StatusCanceled):
				canceled++
			}
		}
		if requested != 1 || canceled != 1 {
			t.Fatalf("operation %s: %d cancel requests and %d Canceled records audited, want 1 and 1", id, requested, canceled)
		}
	}
}

// TC-E16: a key answers until 24 hours after creation, and past that a
// request with it creates a new operation whether or not retention has
// purged the key, so expiry never depends on eviction timing.
func TestIdempotencyWindowBoundary(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	w.setClock(t0)
	orig := map[string]string{}
	for _, k := range []string{"K-kept", "K-purged"} {
		o := w.start(p, tpOps.run.Op(), `{"name":"web"}`, k)
		w.waitStatus(o.ID, apitypes.StatusSucceeded)
		orig[k] = o.ID
	}

	w.setClock(t0.Add(23*time.Hour + 59*time.Minute))
	for k, id := range orig {
		if r := w.start(p, tpOps.run.Op(), `{"name":"web"}`, k); r.ID != id {
			t.Fatalf("%s at T+23h59m: %s, want the original %s", k, r.ID, id)
		}
	}

	w.setClock(t0.Add(24*time.Hour + time.Minute))
	if err := w.retention().Prune(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := w.store.LookupIdempotency(p.TokenID, "K-purged"); err != nil || ok {
		t.Fatalf("lookup after purge: %v %v", ok, err)
	}
	for k, id := range orig {
		if r := w.start(p, tpOps.run.Op(), `{"name":"web"}`, k); r.ID == id {
			t.Fatalf("%s at T+24h01m replayed %s", k, id)
		}
	}
	if n := w.countOps(); n != 4 {
		t.Fatalf("%d operations, want 4", n)
	}
}

// TC-E18: at the limit, a read-risk long-running request and a whatIf
// request are accepted; the same mutating op without whatIf is 507.
func TestLimitDoesNotBlockReadRiskRequests(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:read", "test:write")
	w.atLimit()

	check := w.start(p, tpOps.check.Op(), `{"name":"c"}`, "")
	whatIf := w.start(p, tpOps.run.Op(), `{"name":"w","whatIf":true}`, "")
	for _, id := range []string{check.ID, whatIf.ID} {
		w.waitStatus(id, apitypes.StatusSucceeded)
	}
	_, err := w.call(p, tpOps.run.Op(), `{"name":"w"}`, "")
	if pe := wantProblem(t, err, problem.StorageLimitReached); pe.Status != http.StatusInsufficientStorage {
		t.Fatalf("status %d", pe.Status)
	}
}

// TC-ERR23: every refusal of an authenticated mutating request after the
// scope check writes one refused record with its problem slug, and nothing
// executes: validation failure, capability unavailable, storage limit,
// Admit refusal, lane conflict and an oversize body. A validation record
// names the op, the valid target and the failing field paths, and no
// parameter value reaches the file. A failing audit writer does not change
// a refusal's answer, but it sets the audit-health flag, and a mutation is
// then refused audit-unavailable until a requested record is written.
func TestRefusedMutatingRequestsAreAudited(t *testing.T) {
	w := newWorld(t)
	w.tq.down.Store(true)
	w.reg.Refresh(context.Background())
	w.serve()
	p := who("ops", "test:read", "test:write", "other:write")
	run := tpOps.run.Op()

	g := w.tp.gated("g")
	defer g.open()
	holder := w.start(p, run, `{"name":"g","lane":"lr","policy":"reject","limit":1}`, "")
	g.waitStarted(t)
	runs := w.tp.runs.Load()

	const sentSecret, sentNote = "SECRETSENTINEL7f3a!", "NOTESENTINEL9c1e!"
	_, err := w.call(p, run, `{"name":"v","secret":"`+sentSecret+`","note":"`+sentNote+`"}`, "")
	wantProblem(t, err, problem.ValidationFailed)

	_, err = w.call(p, tqOps.run.Op(), `{"name":"u"}`, "")
	wantProblem(t, err, problem.CapabilityUnavailable)

	w.atLimit()
	_, err = w.call(p, run, `{"name":"s"}`, "")
	wantProblem(t, err, problem.StorageLimitReached)
	w.acct.Store(nil)

	w.tp.refuse.Store(true)
	_, err = w.call(p, run, `{"name":"a"}`, "")
	wantProblem(t, err, problem.NotUpdatable)
	w.tp.refuse.Store(false)

	_, err = w.call(p, run, `{"name":"l","lane":"lr","policy":"reject","limit":1}`, "")
	if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != holder.ID {
		t.Fatalf("conflict %+v", pe)
	}

	// An oversize body, declared and chunked, through the real server.
	tok := w.mint("big", "test:write")
	big := `{"note":"` + strings.Repeat("x", 1<<20) + `"}`
	for _, body := range []io.Reader{strings.NewReader(big), io.MultiReader(strings.NewReader(big))} {
		req, err := http.NewRequest(http.MethodPost, w.base+"/v1/providers/tp/things/big/run", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge || decodeProblem(t, b)["type"] == nil {
			t.Fatalf("oversize body: %d %s", resp.StatusCode, b)
		}
	}

	if got := w.tp.runs.Load(); got != runs {
		t.Fatalf("a refused request executed: runs %d -> %d", runs, got)
	}
	refused := filterRecs(w.records(), outcome(audit.OutcomeRefused))
	var slugs []string
	for _, r := range refused {
		slugs = append(slugs, r.Problem)
	}
	want := []string{
		string(problem.ValidationFailed), string(problem.CapabilityUnavailable), string(problem.StorageLimitReached),
		string(problem.NotUpdatable), string(problem.Conflict), string(problem.PayloadTooLarge), string(problem.PayloadTooLarge),
	}
	if !slices.Equal(slugs, want) {
		t.Fatalf("refused records %v, want %v", slugs, want)
	}
	v := refused[0]
	if v.Action != run.ID || v.Target != "v" || !slices.Equal(v.InvalidFields, []string{"/note", "/secret"}) || len(v.Params) != 0 {
		t.Fatalf("validation record %+v", v)
	}
	if big := refused[5]; big.Action != run.ID || big.Target != "big" || big.TokenName != "big" {
		t.Fatalf("payload-too-large record %+v", big)
	}
	raw, err := os.ReadFile(filepath.Join(w.auditDir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"SECRETSENTINEL7f3a", "NOTESENTINEL9c1e"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatalf("sentinel %s reached the audit file", s)
		}
	}

	// Step 3: the audit writer fails.
	w.aud.mode.Store(failReal)
	_, err = w.call(p, run, `{"name":"v","note":"bad!"}`, "")
	if pe := wantProblem(t, err, problem.ValidationFailed); pe.Status != http.StatusBadRequest {
		t.Fatalf("status %d", pe.Status)
	}
	if !strings.Contains(w.logs.String(), `"msg":"audit write failed"`) {
		t.Fatal("the failed refused write was not logged")
	}
	if w.alog.Healthy() {
		t.Fatal("a failed refused write left the audit log healthy")
	}
	_, err = w.call(p, run, `{"name":"after"}`, "")
	wantProblem(t, err, problem.AuditUnavailable)
	if got := w.tp.runs.Load(); got != runs {
		t.Fatalf("a mutation ran with the audit log failing: runs %d -> %d", runs, got)
	}

	// Step 4: restored.
	w.aud.mode.Store(failNone)
	o := w.start(p, run, `{"name":"after"}`, "")
	w.waitStatus(o.ID, apitypes.StatusSucceeded)
	if !w.alog.Healthy() {
		t.Fatal("a successful requested write did not clear the flag")
	}
}
