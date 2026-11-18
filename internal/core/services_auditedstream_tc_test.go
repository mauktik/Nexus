// Tests of planning/services-provider/test-cases.md TC-SV-23, TC-SV-24,
// TC-SV-25, TC-SV-E5 and TC-SV-ERR7 (WI 1.4): the terminal audit record of
// an Audited stream (D-7).

package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	dspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

type paIn struct {
	Name string `json:"name"`
}

func paLogsOp(id, action string, audited bool) def.Op {
	return def.Op{
		ID: id, Provider: "pa", Kind: def.KindSubresource, ResourceType: "things", Action: action,
		Summary: "s", Description: "Planted stream operation for the audited-stream tests.",
		Scope: def.Scope{Name: "pa:read"}, Risk: def.RiskReadOnly, Mode: def.ModeStream, Audited: audited,
		CLI: def.CLIBinding{Noun: "thing" + action, Verb: "get", Positional: "name"},
	}
}

var (
	paLogs    = def.Define[paIn, apitypes.Frame](paLogsOp("pa.things.logs", "logs", true))
	paRawLogs = def.Define[paIn, apitypes.Frame](paLogsOp("pa.things.rawlogs", "rawlogs", false)) // The same op without Audited
)

// streamHandler is the body of every planted stream binding; the fixture's
// counter counts its calls.
type streamHandler func(ctx context.Context, emit func(apitypes.Frame) error) error

type paProvider struct {
	handler func() streamHandler
	calls   *atomic.Int64
}

func (p paProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "pa", ReadScope: "pa:read", Ops: []def.Op{paLogs.Op(), paRawLogs.Op()}}
}
func (paProvider) Configure(func(any) error) error { return nil }
func (paProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p paProvider) Bindings() []provider.Binding {
	h := func(ctx context.Context, _ paIn, emit func(apitypes.Frame) error) error {
		p.calls.Add(1)
		return p.handler()(ctx, emit)
	}
	return []provider.Binding{provider.Stream(paLogs, h), provider.Stream(paRawLogs, h)}
}

// dockerLogsProvider binds the real docker.containers.logs declaration to a
// stub stream handler.
type dockerLogsProvider struct{ handler func() streamHandler }

func (dockerLogsProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: dspec.Name, ReadScope: dspec.ReadScope, Ops: []def.Op{dspec.Logs.Op()}}
}
func (dockerLogsProvider) Configure(func(any) error) error { return nil }
func (dockerLogsProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p dockerLogsProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Stream(dspec.Logs, func(ctx context.Context, _ dspec.LogsInput, emit func(apitypes.Frame) error) error {
		return p.handler()(ctx, emit)
	})}
}

// faultyAudit fails every write, so nothing reaches the real log.
type faultyAudit struct{ *audit.Log }

func (faultyAudit) Write(systemspec.AuditRecord, []string) (string, error) {
	return "", fmt.Errorf("write audit record: %w", syscall.ENOSPC)
}

type streamFixture struct {
	d       *Dispatcher
	lg      *audit.Log
	calls   *atomic.Int64
	handler atomic.Pointer[streamHandler]
}

func (f *streamFixture) set(h streamHandler) { f.handler.Store(&h) }

// newStreamFixture is auditFixture for the planted stream ops and the real
// docker logs op, over a real audit log. wrap, when non-nil, replaces the
// Dispatcher's audit writer.
func newStreamFixture(t *testing.T, wrap func(*audit.Log) AuditWriter) *streamFixture {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	f := &streamFixture{lg: lg, calls: &atomic.Int64{}}
	f.set(func(context.Context, func(apitypes.Frame) error) error { return nil })
	current := func() streamHandler { return *f.handler.Load() }
	reg, err := New([]provider.Provider{paProvider{handler: current, calls: f.calls}, dockerLogsProvider{handler: current}},
		Options{Logger: log, Audit: lg, Providers: map[string]ProviderConfig{"pa": {Enabled: true}, dspec.Name: {Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	opts := DispatcherOptions{Logger: log}
	if wrap != nil {
		opts.Audit = wrap(lg)
	}
	f.d = NewDispatcher(reg, opts)
	return f
}

var tok1 = auth.Principal{TokenID: "tok1", Name: "ops", Scopes: []string{"pa:read", dspec.ReadScope}}

// run invokes op and drives its stream to the end, collecting the frames.
func (f *streamFixture) run(ctx context.Context, op def.Op, input string) ([]apitypes.Frame, error) {
	res, err := f.d.Invoke(ctx, InvokeRequest{Caller: tok1, Surface: SurfaceREST, Op: op, Input: json.RawMessage(input)})
	if err != nil {
		return nil, err
	}
	if res.Stream == nil {
		return nil, errors.New("no stream")
	}
	var frames []apitypes.Frame
	err = res.Stream(ctx, func(fr apitypes.Frame) error {
		frames = append(frames, fr)
		return nil
	})
	return frames, err
}

func emitN(n int) streamHandler {
	return func(_ context.Context, emit func(apitypes.Frame) error) error {
		for i := range n {
			if err := emit(apitypes.Frame{D: fmt.Sprintf("line %d", i)}); err != nil {
				return err
			}
		}
		return nil
	}
}

// TC-SV-23: An Audited stream that ends without error writes requested then
// completed, linked.
func TestTCSV23_AnAuditedStreamThatEndsWithoutError(t *testing.T) {
	f := newStreamFixture(t, nil)
	var before int
	f.set(func(ctx context.Context, emit func(apitypes.Frame) error) error {
		before = len(records(t, f.lg)) // What the log holds before the first frame
		if err := emitN(3)(ctx, emit); err != nil {
			return err
		}
		audit.AddFact(ctx, "service", "x")
		return nil
	})
	frames, err := f.run(context.Background(), paLogs.Op(), `{"name":"web"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[2].D != "line 2" {
		t.Fatalf("frames = %+v, want three", frames)
	}
	if before != 1 {
		t.Fatalf("records before the first frame = %d, want the requested record", before)
	}
	recs := records(t, f.lg)
	if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeCompleted {
		t.Fatalf("records = %+v, want requested then completed", recs)
	}
	req, done := recs[0], recs[1]
	if done.RequestID != req.ID || req.ID == "" {
		t.Fatalf("completed names %q, want the requested record %q", done.RequestID, req.ID)
	}
	for _, r := range recs {
		if r.Action != "pa.things.logs" || r.TokenID != "tok1" || r.Target != "web" {
			t.Fatalf("record = %+v, want op pa.things.logs, token tok1, target web", r)
		}
	}
	if done.Facts["service"] != "x" || done.Facts["clientDisconnected"] != "" {
		t.Fatalf("completed facts = %v, want service=x only", done.Facts)
	}
}

// TC-SV-24: An Audited stream that fails writes requested then refused with
// the sanitized slug.
func TestTCSV24_AnAuditedStreamThatFailsWritesRequested(t *testing.T) {
	const planted = "/secret/path"
	cases := []struct {
		name   string
		err    error
		slug   problem.Slug
		status int
	}{
		{"problem", problem.New(problem.DependencyUnavailable, "journalctl: planted "+planted), problem.DependencyUnavailable, http.StatusServiceUnavailable},
		{"plain error", errors.New("journalctl: planted " + planted), problem.InternalError, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStreamFixture(t, nil)
			f.set(func(ctx context.Context, emit func(apitypes.Frame) error) error {
				if err := emitN(1)(ctx, emit); err != nil {
					return err
				}
				return tc.err
			})
			frames, err := f.run(context.Background(), paLogs.Op(), `{"name":"web"}`)
			if len(frames) != 1 {
				t.Fatalf("frames = %+v, want one", frames)
			}
			p := problemOf(t, err, tc.status)
			if p.Slug() != tc.slug {
				t.Fatalf("client problem = %v, want %s", p, tc.slug)
			}
			if tc.slug == problem.InternalError && strings.Contains(p.Detail, planted) {
				t.Fatalf("a plain error's cause reached the client: %q", p.Detail)
			}
			recs := records(t, f.lg)
			if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeRefused {
				t.Fatalf("records = %+v, want requested then refused", recs)
			}
			ref := recs[1]
			if ref.RequestID != recs[0].ID || ref.Problem != string(p.Slug()) {
				t.Fatalf("refused = %+v, want slug %s naming %q", ref, p.Slug(), recs[0].ID)
			}
			b, _ := json.Marshal(ref)
			if strings.Contains(string(b), planted) || strings.Contains(string(b), "journalctl") {
				t.Fatalf("the refused record carries more than the sanitized problem: %s", b)
			}
		})
	}
}

// TC-SV-25: An unaudited stream still writes no audit record.
func TestTCSV25_AnUnauditedStreamStillWritesNoAudit(t *testing.T) {
	f := newStreamFixture(t, nil)
	ops := []struct {
		op    def.Op
		input string
	}{{paRawLogs.Op(), `{"name":"web"}`}, {dspec.Logs.Op(), `{"name":"web"}`}}
	for _, o := range ops {
		f.set(emitN(2))
		if frames, err := f.run(context.Background(), o.op, o.input); err != nil || len(frames) != 2 {
			t.Fatalf("%s: frames %+v, err %v", o.op.ID, frames, err)
		}
		f.set(func(ctx context.Context, emit func(apitypes.Frame) error) error {
			_ = emitN(1)(ctx, emit)
			return errors.New("failed")
		})
		if _, err := f.run(context.Background(), o.op, o.input); err == nil {
			t.Fatalf("%s: the failing stream succeeded", o.op.ID)
		}
	}
	if recs := records(t, f.lg); len(recs) != 0 {
		t.Fatalf("records = %+v, want none", recs)
	}
}

// serveStreams serves f's Dispatcher over a real HTTP server the way the
// REST transport drives a stream: one NDJSON line per frame, flushed, with
// the request's context.
func serveStreams(t *testing.T, f *streamFixture, op def.Op) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := f.d.Invoke(r.Context(), InvokeRequest{Caller: tok1, Surface: SurfaceREST, Op: op,
			Input: json.RawMessage(`{"name":"web"}`)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rc := http.NewResponseController(w)
		_ = res.Stream(r.Context(), func(fr apitypes.Frame) error {
			line, err := json.Marshal(fr)
			if err != nil {
				return err
			}
			if _, err := w.Write(append(line, '\n')); err != nil {
				return err
			}
			return rc.Flush()
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// terminal waits up to 2 s for the requested record and one terminal record.
func terminal(t *testing.T, lg *audit.Log) []systemspec.AuditRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		recs := records(t, lg)
		if len(recs) >= 2 || time.Now().After(deadline) {
			// A second terminal record would be written at once; give it a
			// moment to show before counting.
			time.Sleep(50 * time.Millisecond)
			return records(t, lg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TC-SV-E5: An Audited stream ended by a client disconnect records completed
// after a frame and refused before one, both with clientDisconnected.
func TestTCSVE5_AnAuditedStreamEndedByAClient(t *testing.T) {
	t.Run("after a frame", func(t *testing.T) {
		f := newStreamFixture(t, nil)
		f.set(func(ctx context.Context, emit func(apitypes.Frame) error) error {
			for i := 0; ; i++ {
				if err := emit(apitypes.Frame{D: fmt.Sprintf("line %d", i)}); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
		srv := serveStreams(t, f, paLogs.Op())
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // Before EOF: closes the connection

		recs := terminal(t, f.lg)
		if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeCompleted ||
			recs[1].RequestID != recs[0].ID || recs[1].Facts["clientDisconnected"] != "true" {
			t.Fatalf("records = %+v, want requested then completed with clientDisconnected=true", recs)
		}
	})

	t.Run("before any frame", func(t *testing.T) {
		f := newStreamFixture(t, nil)
		entered := make(chan struct{})
		f.set(func(ctx context.Context, _ func(apitypes.Frame) error) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
		srv := serveStreams(t, f, paLogs.Op())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		done := make(chan error, 1)
		go func() {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("the handler was not entered")
		}
		cancel() // Closes the connection before any byte of the response
		if err := <-done; err == nil {
			t.Fatal("the request succeeded")
		}

		recs := terminal(t, f.lg)
		if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeRefused ||
			recs[1].RequestID != recs[0].ID || recs[1].Problem == "" || recs[1].Facts["clientDisconnected"] != "true" {
			t.Fatalf("records = %+v, want requested then refused with clientDisconnected=true", recs)
		}
	})
}

// TC-SV-ERR7: A failed requested-record write refuses the audited stream
// before the handler.
func TestTCSVERR7_AFailedRequestedRecordWriteRefusesThe(t *testing.T) {
	f := newStreamFixture(t, func(lg *audit.Log) AuditWriter { return faultyAudit{lg} })
	f.set(emitN(3))
	res, err := f.d.Invoke(context.Background(), InvokeRequest{Caller: tok1, Surface: SurfaceREST, Op: paLogs.Op(),
		Input: json.RawMessage(`{"name":"web"}`)})
	p := problemOf(t, err, http.StatusServiceUnavailable)
	if p.Slug() != problem.AuditUnavailable {
		t.Fatalf("problem = %v, want audit-unavailable", p)
	}
	if res.Stream != nil || f.calls.Load() != 0 {
		t.Fatalf("stream %v, handler calls %d; want no stream and no call", res.Stream != nil, f.calls.Load())
	}
	if recs := records(t, f.lg); len(recs) != 0 {
		t.Fatalf("records = %+v, want none", recs)
	}
}
