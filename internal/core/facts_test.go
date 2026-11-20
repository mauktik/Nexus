// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

type factIn struct {
	Name string `json:"name"`
}

var factRestart = def.Define[factIn, factIn](def.Op{
	ID: "fx.things.restart", Provider: "fx", Kind: def.KindResourceAction, ResourceType: "things",
	Action: "restart", Summary: "s", Description: "Long-running operation for the audit facts tests.",
	Scope: def.Scope{Name: "fx:write"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "thing", Verb: "restart", Positional: "name"},
})

// factProvider binds factRestart, whose Admit adds the fact k=v and then
// refuses with conflict while refuse is set.
type factProvider struct{ refuse *atomic.Bool }

func (factProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "fx", ReadScope: "fx:read", Ops: []def.Op{factRestart.Op()}}
}
func (factProvider) Configure(func(any) error) error { return nil }
func (factProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p factProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.InProcess(factRestart, provider.InProcessSpec[factIn, factIn]{
		Admit: func(ctx context.Context, _ factIn) error {
			audit.AddFact(ctx, "k", "v")
			if p.refuse.Load() {
				return problem.New(problem.Conflict, "refused by the test")
			}
			return nil
		},
		Run: func(_ context.Context, in factIn, _ *provider.Run) (factIn, error) { return in, nil },
	})}
}

// readAuditFile parses audit.jsonl in dir, oldest first.
func readAuditFile(t *testing.T, dir string) []systemspec.AuditRecord {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var recs []systemspec.AuditRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r systemspec.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}

// TC-SM-49 (WI 3.4; FR-16, FR-6): Facts are attached to refused and requested records, and an upload writes a stored record
func TestTCSM49_FactsAreAttachedToRefusedAnd(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	auditDir, stateDir := t.TempDir(), t.TempDir()
	lg, err := audit.Open(audit.Options{Dir: auditDir, Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: lg, Dir: filepath.Join(stateDir, "ops"), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(ctx)
	})

	rec := &uplRec{}
	var hash string
	receive := func(ctx context.Context, in uplIn, b provider.UploadBody) (uplOut, error) {
		out, err := rec.receive(ctx, in, b)
		sum := sha256.Sum256(rec.got)
		hash = hex.EncodeToString(sum[:])
		audit.AddFact(ctx, "sha256", hash)
		return out, err
	}
	refuse := &atomic.Bool{}
	upl := uplProvider{name: "tp", ops: tpOps(), binds: []provider.Binding{
		provider.Upload(tpUpload, provider.UploadSpec[uplIn, uplOut]{Admit: rec.admit, Receive: receive})}}
	reg, err := core.New([]provider.Provider{upl, factProvider{refuse: refuse}}, core.Options{
		Logger: log, Audit: lg, Ops: mgr,
		Providers: map[string]core.ProviderConfig{"tp": {Enabled: true}, "fx": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	who := caller("tp:admin", "fx:write")

	// Step 1: upload.
	b, _, _ := uplBody(1024, gzipType, 1024)
	res, err := invokeUpload(context.Background(), d, who, b)
	want201(t, res, err)

	// Steps 2 and 3: the long-running op refused, then admitted.
	restart := func() (core.InvokeResult, error) {
		return d.Invoke(context.Background(), core.InvokeRequest{Caller: who, Surface: core.SurfaceREST,
			Op: factRestart.Op(), Input: json.RawMessage(`{"name":"web-1"}`)})
	}
	refuse.Store(true)
	_, err = restart()
	wantProblem(t, err, http.StatusConflict, problem.Conflict)
	refuse.Store(false)
	res, err = restart()
	if err != nil || res.Status != http.StatusAccepted {
		t.Fatalf("admitted restart = %+v, %v; want 202", res, err)
	}

	// Step 4.
	var upload, restarts []systemspec.AuditRecord
	for _, r := range readAuditFile(t, auditDir) {
		switch r.Action {
		case tpUpload.Op().ID:
			upload = append(upload, r)
		case factRestart.Op().ID:
			if r.Outcome == audit.OutcomeRequested || r.Outcome == audit.OutcomeRefused {
				restarts = append(restarts, r)
			}
		}
	}
	if len(upload) != 2 || upload[0].Outcome != audit.OutcomeRequested || upload[1].Outcome != audit.OutcomeStored {
		t.Fatalf("upload records = %+v, want requested then stored", upload)
	}
	if upload[1].RequestID == "" || upload[1].RequestID != upload[0].ID {
		t.Fatalf("stored requestId = %q, want the requested record's id %q", upload[1].RequestID, upload[0].ID)
	}
	if hash == "" || !maps.Equal(upload[1].Facts, map[string]string{"sha256": hash}) {
		t.Fatalf("stored facts = %v, want sha256=%s", upload[1].Facts, hash)
	}
	if upload[1].TokenID != who.TokenID || upload[1].Provider != "tp" {
		t.Fatalf("stored record = %+v, want the caller and provider", upload[1])
	}
	if len(restarts) != 2 {
		t.Fatalf("restart records = %+v, want refused then requested", restarts)
	}
	kv := map[string]string{"k": "v"}
	if r := restarts[0]; r.Outcome != audit.OutcomeRefused || r.Problem != string(problem.Conflict) || !maps.Equal(r.Facts, kv) {
		t.Fatalf("refused record = %+v, want conflict with facts k=v", r)
	}
	if r := restarts[1]; r.Outcome != audit.OutcomeRequested || !maps.Equal(r.Facts, kv) {
		t.Fatalf("requested record = %+v, want facts k=v", r)
	}
}

// A refusal after the upload's requested record carries the facts Receive
// added before it failed, and no stored record is written.
func TestUploadRefusedAfterReceiveCarriesFacts(t *testing.T) {
	rec := &uplRec{}
	sink := &spyAudit{rec: rec}
	receive := func(ctx context.Context, in uplIn, b provider.UploadBody) (uplOut, error) {
		_, _ = rec.receive(ctx, in, b)
		audit.AddFact(ctx, "uploadId", "u-1")
		return uplOut{}, errors.New("disk full")
	}
	d := uplDispatcher(t, sink, provider.UploadSpec[uplIn, uplOut]{Admit: rec.admit, Receive: receive}, uplFixtureOpts{})
	b, _, _ := uplBody(1024, gzipType, 1024)
	_, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
	wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	recs := sink.written()
	if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[0].Facts != nil {
		t.Fatalf("records = %+v, want requested without facts, then refused", recs)
	}
	if r := recs[1]; r.Outcome != audit.OutcomeRefused || !maps.Equal(r.Facts, map[string]string{"uploadId": "u-1"}) {
		t.Fatalf("refused record = %+v, want facts uploadId=u-1", r)
	}
}

// failStoredAudit is a spyAudit whose stored writes fail.
type failStoredAudit struct{ *spyAudit }

func (a failStoredAudit) Write(rec systemspec.AuditRecord, sensitive []string) (string, error) {
	if rec.Outcome == audit.OutcomeStored {
		return "", errors.New("audit disk full")
	}
	return a.spyAudit.Write(rec, sensitive)
}

// TC-SM-ERR26 (WI 3.4; FR-16, FR-6): A failed stored write is logged and the upload still answers 201
func TestTCSMERR26_AFailedStoredWriteIsLogged(t *testing.T) {
	logs := &logBuf{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	rec := &uplRec{}
	p := uplProvider{name: "tp", ops: tpOps(), binds: []provider.Binding{provider.Upload(tpUpload, rec.spec())}}
	reg, err := core.New([]provider.Provider{p}, core.Options{
		Logger: log, Providers: map[string]core.ProviderConfig{"tp": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log, Audit: failStoredAudit{&spyAudit{rec: rec}}})
	b, _, _ := uplBody(1024, gzipType, 1024)
	res, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
	want201(t, res, err)

	var found bool
	for line := range strings.Lines(logs.String()) {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["msg"] == "audit write failed" && e["outcome"] == audit.OutcomeStored {
			found = true
			// note logs a failed refused or denied write at the same level.
			if e["level"] != slog.LevelError.String() || e["requestId"] != "req-1" || e["op"] != tpUpload.Op().ID {
				t.Fatalf("log entry = %v, want level ERROR naming requestId req-1 and the op", e)
			}
		}
	}
	if !found {
		t.Fatalf("no error entry for the failed stored write in:\n%s", logs.String())
	}
}
