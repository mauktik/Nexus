// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

const (
	octetStream = "application/octet-stream"
	gzipType    = "application/gzip"
	uplCeiling  = 1 << 20
)

type uplIn struct {
	Signature string `json:"signature"`
}

type uplOut struct {
	UploadID  string `json:"uploadId"`
	SizeBytes int64  `json:"sizeBytes"`
}

type updIn struct {
	UploadID string `json:"uploadId"`
}

func uploadDecl(prov string) def.Typed[uplIn, uplOut] {
	return def.Define[uplIn, uplOut](def.Op{
		ID: prov + ".uploads.add", Provider: prov, Kind: def.KindUpload, ResourceType: "uploads",
		Summary: "s", Description: "Upload operation for the dispatcher upload tests.",
		Scope: def.Scope{Name: prov + ":admin"}, Risk: def.RiskMutating, Mode: def.ModeSync,
		Upload: &def.Upload{ContentTypes: []string{octetStream, gzipType}, MaxBytes: uplCeiling},
		Refine: func(s *jsonschema.Schema) { s.Properties["signature"].Pattern = `^[A-Za-z0-9+/=]+$` },
	})
}

func updateDecl(action, ref string) def.Typed[updIn, uplOut] {
	return def.Define[updIn, uplOut](def.Op{
		ID: "tp.actions." + action, Provider: "tp", Kind: def.KindProviderAction, Action: action,
		Summary: "s", Description: "Update operation for the registry CLI.Upload tests.",
		Scope: def.Scope{Name: "tp:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: "tp", Verb: "update",
			Upload: &def.CLIUpload{Op: ref, Field: "uploadId", FileFlag: "file"}},
	})
}

// The F-UPL provider tp and its neighbours. def.Define registers globally,
// so every declaration is made once.
var (
	tpUpload = uploadDecl("tp")
	tqUpload = uploadDecl("tq")
	tpList   = def.Define[updIn, uplOut](def.Op{
		ID: "tp.things.list", Provider: "tp", Kind: def.KindList, ResourceType: "things",
		Summary: "s", Description: "List operation for the registry CLI.Upload tests.",
		Scope: def.Scope{Name: "tp:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "thing", Verb: "get"},
	})
	tpUpdMissing = updateDecl("update-missing", "tp.uploads.nope")
	tpUpdList    = updateDecl("update-list", "tp.things.list")
	tpUpdOther   = updateDecl("update-other", "tq.uploads.add")
	tpUpdate     = updateDecl("update", "tp.uploads.add")
)

// uplProvider declares ops and binds whichever subset a test chooses.
type uplProvider struct {
	name  string
	ops   []def.Op
	binds []provider.Binding
}

func (p uplProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: p.ops}
}
func (uplProvider) Configure(func(any) error) error { return nil }
func (uplProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p uplProvider) Bindings() []provider.Binding { return p.binds }

func tpOps() []def.Op {
	return []def.Op{tpUpload.Op(), tpList.Op(), tpUpdMissing.Op(), tpUpdList.Op(), tpUpdOther.Op(), tpUpdate.Op()}
}

// uplRec records the order of the provider's and the audit sink's calls.
type uplRec struct {
	mu    sync.Mutex
	order []string
	got   []byte // What Receive read
}

func (r *uplRec) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, s)
}

func (r *uplRec) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

func (r *uplRec) count(s string) int {
	n := 0
	for _, c := range r.calls() {
		if c == s {
			n++
		}
	}
	return n
}

// release is the counting release F-UPL's Admit returns.
func (r *uplRec) release() { r.add("release") }

func (r *uplRec) admit(context.Context, uplIn) (func(), error) {
	r.add("admit")
	return r.release, nil
}

// receive reads the whole body, then records itself.
func (r *uplRec) receive(_ context.Context, _ uplIn, b provider.UploadBody) (uplOut, error) {
	data, err := io.ReadAll(b.Reader)
	r.mu.Lock()
	r.got = data
	r.mu.Unlock()
	r.add("receive")
	if err != nil {
		return uplOut{}, fmt.Errorf("receive: %w", err)
	}
	return uplOut{UploadID: "u-1", SizeBytes: int64(len(data))}, nil
}

func (r *uplRec) spec() provider.UploadSpec[uplIn, uplOut] {
	return provider.UploadSpec[uplIn, uplOut]{Admit: r.admit, Receive: r.receive}
}

// spyAudit is an AuditWriter that records each written record's outcome in
// the shared call order.
type spyAudit struct {
	rec           *uplRec
	failRequested bool
	onRequested   func()

	mu      sync.Mutex
	records []systemspec.AuditRecord
}

func (a *spyAudit) Write(rec systemspec.AuditRecord, _ []string) (string, error) {
	if rec.Outcome == audit.OutcomeRequested && a.failRequested {
		return "", errors.New("audit disk full")
	}
	a.mu.Lock()
	a.records = append(a.records, rec)
	id := fmt.Sprintf("req-%d", len(a.records))
	a.mu.Unlock()
	a.rec.add("audit:" + rec.Outcome)
	if rec.Outcome == audit.OutcomeRequested && a.onRequested != nil {
		a.onRequested()
	}
	return id, nil
}

func (a *spyAudit) Redact(_, _ string, params json.RawMessage, _ []string) (json.RawMessage, error) {
	return params, nil
}

func (a *spyAudit) written() []systemspec.AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.records)
}

func (a *spyAudit) outcomes() []string {
	var out []string
	for _, r := range a.written() {
		out = append(out, r.Outcome)
	}
	return out
}

// countingReader counts Read calls and bytes.
type countingReader struct {
	r     io.Reader
	reads atomic.Int64
	n     atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads.Add(1)
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// failReader fails the test if it is read at all.
type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Error("the upload body was read")
	return 0, io.EOF
}

// tooLargeReader returns 10 bytes, then the transport's cap error.
type tooLargeReader struct{ sent bool }

func (r *tooLargeReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "0123456789"), nil
	}
	return 0, fmt.Errorf("read: %w", core.ErrUploadTooLarge)
}

func uplBody(n int, contentType string, contentLength int64) (*provider.UploadBody, *countingReader, []byte) {
	data := bytes.Repeat([]byte("nexus-upload-"), n/13+1)[:n]
	cr := &countingReader{r: bytes.NewReader(data)}
	return &provider.UploadBody{Reader: cr, ContentType: contentType, ContentLength: contentLength}, cr, data
}

type uplFixtureOpts struct {
	storage func() *problem.Error
}

func uplDispatcher(t *testing.T, sink core.AuditWriter, spec provider.UploadSpec[uplIn, uplOut], o uplFixtureOpts) *core.Dispatcher {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p := uplProvider{name: "tp", ops: tpOps(), binds: []provider.Binding{provider.Upload(tpUpload, spec)}}
	reg, err := core.New([]provider.Provider{p}, core.Options{
		Logger: log, StorageCheck: o.storage,
		Providers: map[string]core.ProviderConfig{"tp": {Enabled: true}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	return core.NewDispatcher(reg, core.DispatcherOptions{Logger: log, Audit: sink})
}

func invokeUpload(ctx context.Context, d *core.Dispatcher, who auth.Principal, b *provider.UploadBody) (core.InvokeResult, error) {
	return d.Invoke(ctx, core.InvokeRequest{
		Caller: who, Surface: core.SurfaceREST, Op: tpUpload.Op(),
		Input: json.RawMessage(`{"signature":"c2lnbmF0dXJl"}`), Upload: b,
	})
}

func want201(t *testing.T, res core.InvokeResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.Status)
	}
}

func wantTooLarge(t *testing.T, err error, maxBytes int64) {
	t.Helper()
	p := wantProblem(t, err, http.StatusRequestEntityTooLarge, problem.UploadTooLarge)
	if got := p.Extensions["maxBytes"]; got != float64(maxBytes) {
		t.Fatalf("maxBytes = %v, want %d", got, maxBytes)
	}
}

type receiveFunc = func(context.Context, uplIn, provider.UploadBody) (uplOut, error)

// TC-SM-26 (WI 2.2; FR-6, NFR-7, FR-16): Dispatcher upload path runs scope, then validation, then Admit, then requested audit, then Receive, and answers 201
func TestTCSM26_DispatcherUploadPathRunsScopeThen(t *testing.T) {
	admin := caller("tp:admin")

	t.Run("success", func(t *testing.T) {
		rec := &uplRec{}
		d := uplDispatcher(t, &spyAudit{rec: rec}, rec.spec(), uplFixtureOpts{})
		b, cr, data := uplBody(1024, gzipType, 1024)
		res, err := invokeUpload(context.Background(), d, admin, b)
		want201(t, res, err)
		if res.Body != (uplOut{UploadID: "u-1", SizeBytes: 1024}) {
			t.Fatalf("body = %#v, want the provider's output", res.Body)
		}
		// release is last: it ran once, after Receive returned and the
		// stored record (WI 3.4) was written.
		if got := rec.calls(); !slices.Equal(got, []string{"admit", "audit:requested", "receive", "audit:stored", "release"}) {
			t.Fatalf("order = %v, want admit, audit:requested, receive, audit:stored, release", got)
		}
		if cr.n.Load() != 1024 || !bytes.Equal(rec.got, data) {
			t.Fatalf("Receive read %d bytes (equal %t), want the 1024 bytes sent", cr.n.Load(), bytes.Equal(rec.got, data))
		}
	})

	// A failure after the requested record is refused tied to it, and the
	// release runs once, after Receive returned or unwound (A-51).
	afterRequested := func(t *testing.T, mk func(*uplRec) receiveFunc) {
		t.Helper()
		rec := &uplRec{}
		sink := &spyAudit{rec: rec}
		d := uplDispatcher(t, sink, provider.UploadSpec[uplIn, uplOut]{Admit: rec.admit, Receive: mk(rec)}, uplFixtureOpts{})
		b, _, _ := uplBody(1024, gzipType, 1024)
		res, err := invokeUpload(context.Background(), d, admin, b)
		if res.Status == http.StatusCreated {
			t.Fatal("answered 201")
		}
		wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
		if got := rec.calls(); !slices.Equal(got, []string{"admit", "audit:requested", "receive", "audit:refused", "release"}) {
			t.Fatalf("order = %v, want admit, audit:requested, receive, audit:refused, release", got)
		}
		if recs := sink.written(); recs[1].RequestID != "req-1" || recs[1].Problem != string(problem.InternalError) {
			t.Fatalf("refused record = %+v, want internal-error tied to requested record req-1", recs[1])
		}
	}
	t.Run("receive error", func(t *testing.T) {
		afterRequested(t, func(rec *uplRec) receiveFunc {
			return func(ctx context.Context, in uplIn, b provider.UploadBody) (uplOut, error) {
				_, _ = rec.receive(ctx, in, b)
				return uplOut{}, errors.New("disk full")
			}
		})
	})
	t.Run("receive panic", func(t *testing.T) {
		afterRequested(t, func(rec *uplRec) receiveFunc {
			return func(ctx context.Context, in uplIn, b provider.UploadBody) (uplOut, error) {
				_, _ = rec.receive(ctx, in, b)
				panic("receive exploded")
			}
		})
	})

	t.Run("nil release", func(t *testing.T) {
		rec := &uplRec{}
		admit := func(context.Context, uplIn) (func(), error) { rec.add("admit"); return nil, nil }
		d := uplDispatcher(t, &spyAudit{rec: rec}, provider.UploadSpec[uplIn, uplOut]{Admit: admit, Receive: rec.receive}, uplFixtureOpts{})
		b, _, _ := uplBody(1024, octetStream, 1024)
		res, err := invokeUpload(context.Background(), d, admin, b)
		want201(t, res, err)
	})
	t.Run("nil spec Admit", func(t *testing.T) {
		rec := &uplRec{}
		d := uplDispatcher(t, &spyAudit{rec: rec}, provider.UploadSpec[uplIn, uplOut]{Receive: rec.receive}, uplFixtureOpts{})
		b, _, _ := uplBody(1024, octetStream, 1024)
		res, err := invokeUpload(context.Background(), d, admin, b)
		want201(t, res, err)
		if got := rec.calls(); !slices.Equal(got, []string{"audit:requested", "receive", "audit:stored"}) {
			t.Fatalf("order = %v, want audit:requested, receive, audit:stored", got)
		}
	})
}

// TC-SM-27 (WI 2.2; FR-6): The binding's MaxBytes caps uploads, and a value above the op ceiling is clamped
func TestTCSM27_TheBindingSMaxBytesCapsUploads(t *testing.T) {
	for _, tc := range []struct {
		name          string
		binding, want int64
	}{
		{"below the ceiling", 4 << 10, 4096},
		{"above the ceiling is clamped", 8 << 20, uplCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &uplRec{}
			spec := rec.spec()
			spec.MaxBytes = func() int64 { return tc.binding }
			d := uplDispatcher(t, &spyAudit{rec: rec}, spec, uplFixtureOpts{})

			b, _, _ := uplBody(int(tc.want), gzipType, tc.want)
			res, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
			want201(t, res, err)

			b, cr, _ := uplBody(int(tc.want)+1, gzipType, tc.want+1)
			_, err = invokeUpload(context.Background(), d, caller("tp:admin"), b)
			wantTooLarge(t, err, tc.want)
			if cr.reads.Load() != 0 {
				t.Fatalf("the oversize body was read %d times", cr.reads.Load())
			}
		})
	}
}

// TC-SM-E1 (WI 2.2; FR-6): A declared Content-Length exactly at the cap is accepted, and cap+1 is refused unread
func TestTCSME1_ADeclaredContentLengthExactlyAt(t *testing.T) {
	rec := &uplRec{}
	spec := rec.spec()
	spec.MaxBytes = func() int64 { return 64 << 10 }
	d := uplDispatcher(t, &spyAudit{rec: rec}, spec, uplFixtureOpts{})

	b, cr, _ := uplBody(65536, octetStream, 65536)
	res, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
	want201(t, res, err)
	if cr.n.Load() != 65536 {
		t.Fatalf("read %d bytes, want 65536", cr.n.Load())
	}
	admits := rec.count("admit")

	b, cr, _ = uplBody(65537, octetStream, 65537)
	_, err = invokeUpload(context.Background(), d, caller("tp:admin"), b)
	wantTooLarge(t, err, 65536)
	if cr.reads.Load() != 0 || rec.count("admit") != admits {
		t.Fatalf("cap+1: reads %d, Admit calls %d more; want 0 and 0", cr.reads.Load(), rec.count("admit")-admits)
	}
}

// TC-SM-ERR9 (WI 2.2; FR-6, FR-1, NFR-7): Unauthorized and forbidden upload requests never reach Admit, Receive or the body
func TestTCSMERR9_UnauthorizedAndForbiddenUploadRequestsNever(t *testing.T) {
	rec := &uplRec{}
	sink := &spyAudit{rec: rec}
	d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{})

	b, cr, _ := uplBody(1024, gzipType, 1024)
	_, err := invokeUpload(context.Background(), d, auth.Principal{}, b)
	wantProblem(t, err, http.StatusUnauthorized, problem.Unauthorized)

	b2, cr2, _ := uplBody(1024, gzipType, 1024)
	_, err = invokeUpload(context.Background(), d, caller("tp:read"), b2)
	p := wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if p.Extensions["requiredScope"] != "tp:admin" {
		t.Fatalf("requiredScope = %v, want tp:admin", p.Extensions["requiredScope"])
	}

	if n := rec.count("admit") + rec.count("receive"); n != 0 || cr.reads.Load()+cr2.reads.Load() != 0 {
		t.Fatalf("Admit/Receive calls %d, body reads %d; want 0 and 0", n, cr.reads.Load()+cr2.reads.Load())
	}
	// Both are audited as every other op's 401 and 403 are: denied, with no
	// requested record.
	recs := sink.written()
	if len(recs) != 2 || recs[0].Outcome != audit.OutcomeDenied || recs[0].Problem != string(problem.Unauthorized) ||
		recs[1].Outcome != audit.OutcomeDenied || recs[1].Problem != string(problem.Forbidden) {
		t.Fatalf("records = %+v, want denied unauthorized and denied forbidden", recs)
	}
}

// TC-SM-ERR10 (WI 2.2; FR-6): A wrong Content-Type is validation-failed and the body is unread
func TestTCSMERR10_AWrongContentTypeIsValidation(t *testing.T) {
	rec := &uplRec{}
	sink := &spyAudit{rec: rec}
	d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{})
	for _, ct := range []string{"application/json", "text/plain", "", "application/gzip-not"} {
		b, cr, _ := uplBody(1024, ct, 1024)
		_, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
		p := wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)
		fe := fieldErrors(t, p)
		if len(fe) != 1 || fe[0].Path != "" || fe[0].Message != "Content-Type must be one of application/octet-stream, application/gzip" {
			t.Fatalf("%q: errors = %+v", ct, fe)
		}
		if cr.reads.Load() != 0 || rec.count("admit") != 0 {
			t.Fatalf("%q: reads %d, Admit calls %d; want 0 and 0", ct, cr.reads.Load(), rec.count("admit"))
		}
	}
	if got := sink.outcomes(); len(got) != 4 || slices.ContainsFunc(got, func(o string) bool { return o != audit.OutcomeRefused }) {
		t.Fatalf("outcomes = %v, want 4 refused", got)
	}

	// The media type is compared, case-insensitively and without parameters.
	b, _, _ := uplBody(16, "Application/GZIP; charset=binary", 16)
	res, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
	want201(t, res, err)
}

// TC-SM-ERR11 (WI 2.2; FR-6, NFR-4): A declared oversize body is refused 413 with zero reads and no Admit
func TestTCSMERR11_ADeclaredOversizeBodyIsRefused(t *testing.T) {
	rec := &uplRec{}
	sink := &spyAudit{rec: rec}
	d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{})
	_, err := invokeUpload(context.Background(), d, caller("tp:admin"),
		&provider.UploadBody{Reader: failReader{t}, ContentType: gzipType, ContentLength: 2 << 20})
	wantTooLarge(t, err, uplCeiling)
	if rec.count("admit") != 0 || slices.Contains(sink.outcomes(), audit.OutcomeRequested) {
		t.Fatalf("calls %v, outcomes %v; want no Admit and no requested record", rec.calls(), sink.outcomes())
	}
	if got := sink.written(); len(got) != 1 || got[0].Problem != string(problem.UploadTooLarge) {
		t.Fatalf("records = %+v, want one refused upload-too-large", got)
	}
}

// TC-SM-ERR12 (WI 2.2; FR-6): A streamed oversize body maps to upload-too-large
func TestTCSMERR12_AStreamedOversizeBodyMapsTo(t *testing.T) {
	t.Run("cap error", func(t *testing.T) {
		rec := &uplRec{}
		sink := &spyAudit{rec: rec}
		d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{})
		_, err := invokeUpload(context.Background(), d, caller("tp:admin"),
			&provider.UploadBody{Reader: &tooLargeReader{}, ContentType: gzipType, ContentLength: -1})
		wantTooLarge(t, err, uplCeiling)
		if got := sink.outcomes(); !slices.Equal(got, []string{audit.OutcomeRequested, audit.OutcomeRefused}) {
			t.Fatalf("outcomes = %v, want requested, refused (and no stored)", got)
		}
		if r := sink.written()[1]; r.RequestID != "req-1" || r.Problem != string(problem.UploadTooLarge) {
			t.Fatalf("refused record = %+v", r)
		}
		if rec.count("release") != 1 {
			t.Fatalf("release calls = %d, want 1", rec.count("release"))
		}
	})
	t.Run("other error", func(t *testing.T) {
		rec := &uplRec{}
		sink := &spyAudit{rec: rec}
		receive := func(context.Context, uplIn, provider.UploadBody) (uplOut, error) {
			return uplOut{}, errors.New("disk full")
		}
		d := uplDispatcher(t, sink, provider.UploadSpec[uplIn, uplOut]{Admit: rec.admit, Receive: receive}, uplFixtureOpts{})
		res, err := invokeUpload(context.Background(), d, caller("tp:admin"),
			&provider.UploadBody{Reader: &tooLargeReader{}, ContentType: gzipType, ContentLength: -1})
		if res.Status == http.StatusCreated {
			t.Fatal("answered 201")
		}
		p := wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
		if strings.Contains(p.Detail, "disk full") {
			t.Fatalf("the cause leaked: %s", p.Detail)
		}
		if r := sink.written(); len(r) != 2 || r[1].Outcome != audit.OutcomeRefused || r[1].RequestID != "req-1" {
			t.Fatalf("records = %+v, want requested then refused tied to it", r)
		}
	})
}

// TC-SM-ERR13 (WI 2.2; FR-16, FR-6): An audit write failure refuses the upload with audit-unavailable before Receive
func TestTCSMERR13_AnAuditWriteFailureRefusesThe(t *testing.T) {
	t.Run("requested write fails", func(t *testing.T) {
		rec := &uplRec{}
		d := uplDispatcher(t, &spyAudit{rec: rec, failRequested: true}, rec.spec(), uplFixtureOpts{})
		b, cr, _ := uplBody(1024, gzipType, 1024)
		_, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
		wantProblem(t, err, http.StatusServiceUnavailable, problem.AuditUnavailable)
		if rec.count("receive") != 0 || cr.reads.Load() != 0 || rec.count("release") != 1 {
			t.Fatalf("calls %v, reads %d; want no Receive, no reads, one release", rec.calls(), cr.reads.Load())
		}
	})
	t.Run("ctx ends after requested", func(t *testing.T) {
		rec := &uplRec{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sink := &spyAudit{rec: rec, onRequested: cancel}
		d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{})
		b, cr, _ := uplBody(1024, gzipType, 1024)
		res, err := invokeUpload(ctx, d, caller("tp:admin"), b)
		if err == nil || res.Status == http.StatusCreated {
			t.Fatalf("status %d, err %v; want a refusal", res.Status, err)
		}
		if rec.count("receive") != 0 || cr.reads.Load() != 0 || rec.count("release") != 1 {
			t.Fatalf("calls %v, reads %d; want no Receive, no reads, one release", rec.calls(), cr.reads.Load())
		}
		if got := sink.outcomes(); !slices.Equal(got, []string{audit.OutcomeRequested, audit.OutcomeRefused}) {
			t.Fatalf("outcomes = %v, want requested, refused (and no stored)", got)
		}
	})
}

// TC-SM-ERR14 (WI 2.2; FR-6, FR-16): An Admit refusal returns its problem with a refused audit record
func TestTCSMERR14_AnAdmitRefusalReturnsItsProblem(t *testing.T) {
	for _, tc := range []struct {
		name        string
		holdRelease bool // Admit refuses but still returns a release (A-51)
	}{{"nil release", false}, {"non-nil release", true}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &uplRec{}
			sink := &spyAudit{rec: rec}
			admit := func(context.Context, uplIn) (func(), error) {
				rec.add("admit")
				var release func()
				if tc.holdRelease {
					release = rec.release
				}
				return release, problem.New(problem.Conflict, "two uploads are already staged")
			}
			d := uplDispatcher(t, sink, provider.UploadSpec[uplIn, uplOut]{Admit: admit, Receive: rec.receive}, uplFixtureOpts{})
			b, cr, _ := uplBody(1024, gzipType, 1024)
			_, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
			wantProblem(t, err, http.StatusConflict, problem.Conflict)
			if rec.count("receive") != 0 || cr.reads.Load() != 0 {
				t.Fatalf("calls %v, reads %d; want no Receive and no reads", rec.calls(), cr.reads.Load())
			}
			if want := map[bool]int{false: 0, true: 1}[tc.holdRelease]; rec.count("release") != want {
				t.Fatalf("release calls = %d, want %d", rec.count("release"), want)
			}
			if r := sink.written(); len(r) != 1 || r[0].Outcome != audit.OutcomeRefused || r[0].Problem != string(problem.Conflict) {
				t.Fatalf("records = %+v, want one refused conflict and no requested", r)
			}
		})
	}
}

// The storage check refuses an upload before Admit and before the body.
func TestUploadStorageCheckPrecedesAdmit(t *testing.T) {
	rec := &uplRec{}
	sink := &spyAudit{rec: rec}
	full := func() *problem.Error { return problem.New(problem.StorageLimitReached, "the disk is full") }
	d := uplDispatcher(t, sink, rec.spec(), uplFixtureOpts{storage: full})
	b, cr, _ := uplBody(1024, gzipType, 1024)
	_, err := invokeUpload(context.Background(), d, caller("tp:admin"), b)
	wantProblem(t, err, http.StatusInsufficientStorage, problem.StorageLimitReached)
	if rec.count("admit") != 0 || cr.reads.Load() != 0 || !slices.Equal(sink.outcomes(), []string{audit.OutcomeRefused}) {
		t.Fatalf("calls %v, reads %d, outcomes %v", rec.calls(), cr.reads.Load(), sink.outcomes())
	}
}

// A transport that hands an upload op no body gets an internal error, and
// nothing runs.
func TestUploadWithoutABodyIsRefused(t *testing.T) {
	rec := &uplRec{}
	d := uplDispatcher(t, &spyAudit{rec: rec}, rec.spec(), uplFixtureOpts{})
	_, err := invokeUpload(context.Background(), d, caller("tp:admin"), nil)
	wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	if rec.count("admit") != 0 {
		t.Fatalf("calls = %v", rec.calls())
	}
}

// TC-SM-ERR15 (WI 2.2; FR-13, CON-3): The registry rejects bad CLI.Upload references
func TestTCSMERR15_TheRegistryRejectsBadCLIUpload(t *testing.T) {
	recv := func(context.Context, uplIn, provider.UploadBody) (uplOut, error) { return uplOut{}, nil }
	run := func(context.Context, updIn, *provider.Run) (uplOut, error) { return uplOut{}, nil }
	upd := func(op def.Typed[updIn, uplOut]) provider.Binding {
		return provider.InProcess(op, provider.InProcessSpec[updIn, uplOut]{Run: run})
	}
	upload := provider.Upload(tpUpload, provider.UploadSpec[uplIn, uplOut]{Receive: recv})
	list := provider.Sync(tpList, func(context.Context, updIn) (uplOut, error) { return uplOut{}, nil })
	tq := uplProvider{name: "tq", ops: []def.Op{tqUpload.Op()},
		binds: []provider.Binding{provider.Upload(tqUpload, provider.UploadSpec[uplIn, uplOut]{Receive: recv})}}

	for _, tc := range []struct {
		row   string
		binds []provider.Binding
		want  []string // Substrings of the error; nil means New succeeds
	}{
		{"(a) missing", []provider.Binding{upd(tpUpdMissing), upload},
			[]string{"tp.actions.update-missing", "tp.uploads.nope", "does not exist"}},
		{"(b) not KindUpload", []provider.Binding{upd(tpUpdList), list},
			[]string{"tp.actions.update-list", "tp.things.list", "not a KindUpload op"}},
		{"(c) other provider", []provider.Binding{upd(tpUpdOther), upload},
			[]string{"tp.actions.update-other", "tq.uploads.add", "another provider"}},
		{"(d) not bound", []provider.Binding{upd(tpUpdate)},
			[]string{"tp.actions.update", "tp.uploads.add", "is not bound"}},
		{"(e) both bound", []provider.Binding{upd(tpUpdate), upload}, nil},
		{"(f) neither bound", []provider.Binding{list}, nil},
		{"(g) upload bound only", []provider.Binding{upload}, nil},
	} {
		t.Run(tc.row, func(t *testing.T) {
			tp := uplProvider{name: "tp", ops: tpOps(), binds: tc.binds}
			reg, err := core.New([]provider.Provider{tp, tq}, core.Options{
				Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			})
			if tc.want == nil {
				if err != nil || reg == nil {
					t.Fatalf("New: %v, want a registry", err)
				}
				return
			}
			if err == nil || reg != nil {
				t.Fatalf("New: registry built %t, error %v; want an error and no registry", reg != nil, err)
			}
			for _, s := range tc.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not name %q", err, s)
				}
			}
		})
	}
}
