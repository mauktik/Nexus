// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/systemspec"
)

func (w *world) list(p auth.Principal, input string) (apitypes.List[apitypes.Operation], error) {
	res, err := w.call(p, systemspec.OperationsList.Op(), input, "")
	if err != nil {
		return apitypes.List[apitypes.Operation]{}, err
	}
	return res.Body.(apitypes.List[apitypes.Operation]), nil
}

func opIDs(ops []apitypes.Operation) []string {
	out := make([]string, len(ops))
	for i, o := range ops {
		out[i] = o.ID
	}
	return out
}

// TC-53: list and get enforce the provider read scope.
func TestOperationListAndGetEnforceProviderReadScope(t *testing.T) {
	w := newWorld(t)
	var tpIDs, tqIDs []string
	for i := range 3 {
		tpIDs = append(tpIDs, w.start(who("w1", "test:write"), tpOps.run.Op(), fmt.Sprintf(`{"name":"a%d"}`, i), "").ID)
		tqIDs = append(tqIDs, w.start(who("w2", "other:write"), tqOps.run.Op(), fmt.Sprintf(`{"name":"b%d"}`, i), "").ID)
	}
	slices.Reverse(tpIDs) // Newest first

	l, err := w.list(who("r", "test:read"), `{}`)
	if err != nil || !slices.Equal(opIDs(l.Items), tpIDs) {
		t.Fatalf("test:read list = %v (%v), want %v", opIDs(l.Items), err, tpIDs)
	}
	for _, p := range []auth.Principal{who("none"), who("script", "tp:run:a")} {
		l, err := w.list(p, `{}`)
		if err != nil || len(l.Items) != 0 || l.NextCursor != "" {
			t.Fatalf("%v list = %+v (%v), want empty", p.Scopes, l, err)
		}
		if b, _ := json.Marshal(l); !strings.Contains(string(b), `"items":[]`) {
			t.Fatalf("empty list = %s", b)
		}
	}
	all, err := w.list(who("both", "test:read", "other:read"), `{}`)
	if err != nil || len(all.Items) != 6 {
		t.Fatalf("both scopes: %d items (%v)", len(all.Items), err)
	}

	_, err = w.call(who("r", "test:read"), systemspec.OperationsGet.Op(), `{"id":"`+tqIDs[0]+`"}`, "")
	if pe := wantProblem(t, err, problem.Forbidden); pe.Status != http.StatusForbidden || pe.Extensions["requiredScope"] != "other:read" {
		t.Fatalf("get of another provider's op: %+v", pe)
	}
	_, err = w.call(who("r", "test:read"), systemspec.OperationsGet.Op(), `{"id":"01890a5d-ac96-774b-bcce-b302099a8059"}`, "")
	wantProblem(t, err, problem.NotFound)
	res, err := w.call(who("r", "test:read"), systemspec.OperationsGet.Op(), `{"id":"`+tpIDs[0]+`"}`, "")
	if err != nil || res.Body.(apitypes.Operation).ID != tpIDs[0] {
		t.Fatalf("get: %+v %v", res, err)
	}
}

// TC-54: list paging, limits, cursors and filters.
func TestListParameters(t *testing.T) {
	w := newWorld(t)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	statuses := []apitypes.OperationStatus{apitypes.StatusQueued, apitypes.StatusRunning, apitypes.StatusFailed, apitypes.StatusCanceled}
	var fixture []store.OpRecord // Creation order
	for i := range 150 {
		w.setClock(t0.Add(time.Duration(i) * time.Minute))
		prov := []string{"tp", "tq"}[i%2]
		res, err := w.store.CreateWithIdempotency(store.OpRecord{Action: prov + ".things.run", Provider: prov,
			Exec: store.ExecInProcess}, store.IdempotencyKey{})
		if err != nil {
			t.Fatal(err)
		}
		switch st := statuses[(i/2)%4]; st {
		case apitypes.StatusRunning:
			_, err = w.store.CompareAndSetStatus(res.ID, apitypes.StatusQueued, st)
		case apitypes.StatusFailed:
			_, err = w.store.CommitTerminal(res.ID, apitypes.StatusQueued, st, nil, problem.New(problem.InternalError, "x"))
		case apitypes.StatusCanceled:
			_, err = w.store.CompareAndSetStatus(res.ID, apitypes.StatusQueued, st)
		}
		if err != nil {
			t.Fatal(err)
		}
		rec, err := w.store.Get(res.ID)
		if err != nil {
			t.Fatal(err)
		}
		fixture = append(fixture, rec)
	}
	p := who("both", "test:read", "other:read")
	expect := func(keep func(store.OpRecord) bool) []string {
		var out []string
		for i := len(fixture) - 1; i >= 0; i-- {
			if keep(fixture[i]) {
				out = append(out, fixture[i].ID)
			}
		}
		return out
	}
	all := func(input map[string]any) []string {
		t.Helper()
		var got []string
		for {
			b, _ := json.Marshal(input)
			l, err := w.list(p, string(b))
			if err != nil {
				t.Fatalf("list %s: %v", b, err)
			}
			got = append(got, opIDs(l.Items)...)
			if l.NextCursor == "" {
				return got
			}
			input["cursor"] = l.NextCursor
		}
	}

	first, err := w.list(p, `{}`)
	if err != nil || len(first.Items) != systemspec.DefaultLimit || first.NextCursor == "" {
		t.Fatalf("default page: %d items, cursor %q (%v)", len(first.Items), first.NextCursor, err)
	}
	if got := all(map[string]any{}); !slices.Equal(got, expect(func(store.OpRecord) bool { return true })) {
		t.Fatalf("paging returned %d ids, not the fixture newest first", len(got))
	}
	if l, err := w.list(p, `{"limit":1000}`); err != nil || len(l.Items) != 150 || l.NextCursor != "" {
		t.Fatalf("limit=1000: %d (%v)", len(l.Items), err)
	}
	for _, bad := range []string{`{"limit":1001}`, `{"limit":0}`, `{"cursor":"garbage"}`, `{"filter":["nope=1"]}`,
		`{"filter":["status=Approved"]}`, `{"filter":["createdAfter=yesterday"]}`} {
		_, err := w.list(p, bad)
		if pe := wantProblem(t, err, problem.ValidationFailed); pe.Status != http.StatusBadRequest {
			t.Fatalf("%s: %+v", bad, pe)
		}
	}
	cut := t0.Add(75 * time.Minute).Format(time.RFC3339)
	cases := map[string]func(store.OpRecord) bool{
		"status=Failed":        func(r store.OpRecord) bool { return r.Status == apitypes.StatusFailed },
		"provider=tq":          func(r store.OpRecord) bool { return r.Provider == "tq" },
		"provider=x":           func(r store.OpRecord) bool { return false },
		"createdAfter=" + cut:  func(r store.OpRecord) bool { return r.Created.After(t0.Add(75 * time.Minute)) },
		"createdBefore=" + cut: func(r store.OpRecord) bool { return r.Created.Before(t0.Add(75 * time.Minute)) },
	}
	for f, keep := range cases {
		if got, want := all(map[string]any{"filter": []string{f}, "limit": 7}), expect(keep); !slices.Equal(got, want) {
			t.Fatalf("filter %s: %d ids, want %d", f, len(got), len(want))
		}
	}

	// The same parameters through the REST query string.
	w.serve()
	tok := w.mint("lister", "test:read", "other:read")
	resp, b := w.do(http.MethodGet, "/v1/operations?filter=status=Failed&limit=1000", tok, "")
	var l apitypes.List[apitypes.Operation]
	if err := json.Unmarshal(b, &l); err != nil || resp.StatusCode != http.StatusOK ||
		!slices.Equal(opIDs(l.Items), expect(cases["status=Failed"])) {
		t.Fatalf("REST filter: %d %s", resp.StatusCode, b)
	}
	for _, q := range []string{"limit=1001", "limit=0", "cursor=garbage", "filter=nope=1"} {
		if resp, b := w.do(http.MethodGet, "/v1/operations?"+q, tok, ""); resp.StatusCode != http.StatusBadRequest ||
			decodeProblem(t, b)["type"] != "urn:nexus:problem:validation-failed" {
			t.Fatalf("REST %s: %d %s", q, resp.StatusCode, b)
		}
	}
}

// streamClient never reuses connections, so a closed stream leaves no
// goroutine behind.
var streamClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// waitGoroutines waits for the goroutine count to fall to baseline.
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines %d > baseline %d:\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openStream starts a GET whose body the caller reads; cancel ends it.
func (w *world) openStream(ctx context.Context, path, tok string) *http.Response {
	w.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base+path, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := streamClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	return resp
}

func readFrame(t *testing.T, r *bufio.Reader) apitypes.Frame {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f struct {
		Type      string    `json:"type"`
		O         *int64    `json:"o"`
		T         time.Time `json:"t"`
		S         string    `json:"s"`
		D         string    `json:"d"`
		LostBytes int64     `json:"lostBytes"`
	}
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("frame %q: %v", line, err)
	}
	return apitypes.Frame{Type: f.Type, O: f.O, T: f.T, S: f.S, D: f.D, LostBytes: f.LostBytes}
}

// TC-56: the operation log endpoint serves completed and live output, a
// reconnect resumes with no gap or repeat, an expired offset starts with the
// truncation marker, and a disconnected follower's goroutine exits.
func TestOperationLogEndpoint(t *testing.T) {
	w := newWorld(t, func(o *ops.Options) { o.OutputCapBytes = 1 << 20 })
	w.serve()
	tok := w.mint("reader", "test:write", "test:read")

	// Step 1: completed output.
	w.tp.on("done", func(_ context.Context, in thingIn, r *provider.Run) (thingOut, error) {
		for i := range 3 {
			fmt.Fprintf(r.Output(), "line %d\n", i)
		}
		return thingOut{Name: in.Name}, nil
	})
	done := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"done"}`, "")
	w.waitStatus(done.ID, apitypes.StatusSucceeded)
	resp, b := w.do(http.MethodGet, "/v1/operations/"+done.ID+"/logs", tok, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("logs: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	page, err := oplog.NewReader(filepath.Join(w.stateDir, "ops", done.ID, "output")).Read(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, f := range page.Frames {
		line, _ := json.Marshal(f)
		want.Write(append(line, '\n'))
	}
	if string(b) != want.String() || !strings.Contains(string(b), `"d":"line 0\nline 1\nline 2\n"`) &&
		!strings.Contains(string(b), `"d":"line 2\n"`) {
		t.Fatalf("logs body:\n%s\nwant:\n%s", b, want.String())
	}

	// Step 2: follow a live op, disconnect, and resume from the offset.
	baseline := runtime.NumGoroutine()
	release := make(chan struct{})
	tick := make(chan struct{})
	w.tp.on("live", func(ctx context.Context, in thingIn, r *provider.Run) (thingOut, error) {
		for i := 0; ; i++ {
			select {
			case <-release:
				return thingOut{Name: in.Name}, nil
			case <-tick:
				fmt.Fprintf(r.Output(), "tick %d\n", i)
			case <-ctx.Done():
				return thingOut{}, ctx.Err()
			}
		}
	})
	live := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"live"}`, "")
	tick <- struct{}{}
	tick <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	resp = w.openStream(ctx, "/v1/operations/"+live.ID+"/logs?offset=0&follow=true", tok)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("follow content type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	next := int64(0)
	for got.Len() < len("tick 0\ntick 1\n") {
		f := readFrame(t, br)
		if *f.O != next {
			t.Fatalf("frame at %d, want %d", *f.O, next)
		}
		got.WriteString(f.D)
		next += int64(len(f.D))
	}
	cancel()
	_ = resp.Body.Close()
	waitGoroutines(t, baseline+2) // The live op's worker and its output log
	tick <- struct{}{}
	resp = w.openStream(context.Background(), fmt.Sprintf("/v1/operations/%s/logs?offset=%d&follow=true", live.ID, next), tok)
	br = bufio.NewReader(resp.Body)
	tick <- struct{}{}
	close(release)
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var f apitypes.Frame
		var raw struct {
			O *int64 `json:"o"`
			D string `json:"d"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			t.Fatal(err)
		}
		f.O, f.D = raw.O, raw.D
		if *f.O != next {
			t.Fatalf("resumed frame at %d, want %d (gap or repeat)", *f.O, next)
		}
		got.WriteString(f.D)
		next += int64(len(f.D))
	}
	_ = resp.Body.Close()
	if got.String() != "tick 0\ntick 1\ntick 2\ntick 3\n" {
		t.Fatalf("followed output %q", got.String())
	}
	waitGoroutines(t, baseline)

	// Step 3: an offset before the retained data.
	// 3 MiB in 32 KiB writes against a 1 MiB cap: the oldest segments go.
	big := strings.Repeat(strings.Repeat("x", 63)+"\n", oplog.MaxFrameBytes/64)
	w.tp.on("big", func(_ context.Context, in thingIn, r *provider.Run) (thingOut, error) {
		for range (3 << 20) / len(big) {
			if _, err := io.WriteString(r.Output(), big); err != nil {
				return thingOut{}, err
			}
		}
		return thingOut{Name: in.Name}, nil
	})
	bigOp := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"big"}`, "")
	w.waitStatus(bigOp.ID, apitypes.StatusSucceeded)
	resp = w.openStream(context.Background(), "/v1/operations/"+bigOp.ID+"/logs?offset=0", tok)
	f := readFrame(t, bufio.NewReader(resp.Body))
	_ = resp.Body.Close()
	if !f.Truncated() || f.LostBytes <= 0 {
		t.Fatalf("first frame %+v, want the truncation marker", f)
	}
}

// TC-219: a follow outlasts WriteTimeout, a plain handler does not, and a
// follow ends promptly on Shutdown.
func TestFollowOutlastsWriteTimeoutAndEndsOnShutdown(t *testing.T) {
	w := newWorld(t)
	w.serve(func(s *http.Server) { s.WriteTimeout = 2 * time.Second })
	tok := w.mint("reader", "test:write", "test:read")
	ticker := func(ctx context.Context, in thingIn, r *provider.Run) (thingOut, error) {
		for i := range 10 {
			fmt.Fprintf(r.Output(), "frame %d\n", i)
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return thingOut{}, ctx.Err()
			}
		}
		return thingOut{Name: in.Name}, nil
	}
	w.tp.on("slowout", ticker)
	w.tp.on("slowout2", ticker)

	// Step 1.
	op := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"slowout"}`, "")
	begin := time.Now()
	resp := w.openStream(context.Background(), "/v1/operations/"+op.ID+"/logs?follow=true", tok)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("follow cut off after %v: %v", time.Since(begin), err)
	}
	for i := range 10 {
		if !strings.Contains(string(body), fmt.Sprintf(`"d":"frame %d\n"`, i)) {
			t.Fatalf("frame %d missing after %v:\n%s", i, time.Since(begin), body)
		}
	}
	if d := time.Since(begin); d < 4*time.Second {
		t.Fatalf("follow ended after %v, before the op did", d)
	}

	// Step 2: the control.
	begin = time.Now()
	resp, err = streamClient.Do(func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, w.base+"/v1/providers/tp/things/x/slow", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req
	}())
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("a plain handler outlived WriteTimeout (%v)", time.Since(begin))
	}

	// Step 3.
	baseline := runtime.NumGoroutine()
	op2 := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"slowout2"}`, "")
	resp = w.openStream(context.Background(), "/v1/operations/"+op2.ID+"/logs?follow=true", tok)
	ended := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		ended <- err
	}()
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	begin = time.Now()
	if err := w.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(begin); d >= 2*time.Second {
		t.Fatalf("Shutdown took %v", d)
	}
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the follower did not end on Shutdown")
	}
	w.waitStatus(op2.ID, apitypes.StatusSucceeded)
	waitGoroutines(t, baseline)
}

// TC-220: a provider.Stream op passes through the Dispatcher: scope first,
// NDJSON frames as they come, the handler's context ends with the client,
// an early failure is problem+json, and operation logs are at their Path.
func TestProviderStreamPassesThroughTheDispatcher(t *testing.T) {
	w := newWorld(t)
	w.serve()
	reader := w.mint("reader", "test:read")
	none := w.mint("none")
	entered := make(chan struct{}, 4)
	frame2 := make(chan struct{})
	handlerDone := make(chan error, 1)
	w.tp.mu.Lock()
	w.tp.streams["web"] = func(ctx context.Context, emit func(apitypes.Frame) error) error {
		entered <- struct{}{}
		if err := emit(apitypes.DataFrame(0, time.Now(), apitypes.StreamStdout, "one\n")); err != nil {
			return err
		}
		select {
		case <-frame2:
		case <-ctx.Done():
			handlerDone <- ctx.Err()
			return ctx.Err()
		}
		return emit(apitypes.DataFrame(4, time.Now(), apitypes.StreamStdout, "two\n"))
	}
	w.tp.mu.Unlock()

	// Step 1.
	resp, b := w.do(http.MethodGet, "/v1/providers/tp/things/web/logs", none, "")
	if resp.StatusCode != http.StatusForbidden || len(entered) != 0 {
		t.Fatalf("no scope: %d %s, handler entered %d", resp.StatusCode, b, len(entered))
	}
	// Step 2.
	baseline := runtime.NumGoroutine()
	resp = w.openStream(context.Background(), "/v1/providers/tp/things/web/logs", reader)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	if f := readFrame(t, br); f.D != "one\n" {
		t.Fatalf("frame 1 = %+v", f)
	}
	close(frame2)
	if f := readFrame(t, br); f.D != "two\n" {
		t.Fatalf("frame 2 = %+v", f)
	}
	_ = resp.Body.Close()

	// Step 3.
	frame2 = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	resp = w.openStream(ctx, "/v1/providers/tp/things/web/logs", reader)
	readFrame(t, bufio.NewReader(resp.Body))
	cancel()
	_ = resp.Body.Close()
	select {
	case err := <-handlerDone:
		if err == nil {
			t.Fatal("handler context not canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's context was not canceled on disconnect")
	}
	waitGoroutines(t, baseline)

	// Step 4.
	resp, b = w.do(http.MethodGet, "/v1/providers/tp/things/missing/logs", reader, "")
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("early failure: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}

	// Step 5.
	found := false
	for _, r := range w.srv.Routes() {
		if r.OpID == systemspec.OperationsLogs.Op().ID && r.Method == http.MethodGet && r.Path == "/v1"+systemspec.OperationsLogs.Op().Path {
			found = true
		}
	}
	if !found {
		t.Fatalf("operation logs not routed at its Path: %+v", w.srv.Routes())
	}
	op := w.start(who("ops", "test:write"), tpOps.run.Op(), `{"name":"web"}`, "")
	w.waitStatus(op.ID, apitypes.StatusSucceeded)
	resp, b = w.do(http.MethodGet, "/v1/operations/"+op.ID+"/logs", reader, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("operation logs: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
}
