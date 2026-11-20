// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// Sentinels that must reach only the log, never a problem body (TC-ERR13).
const (
	sentinelPath   = "/var/lib/nexus/SENTINEL-91c2-cause"
	tokenShaped    = "nxs_a1b2c3d4e5f6_QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZn"
	panicSentinel2 = "SENTINEL-4be1-dispatcher-panic"
	secretLike     = "SECRETVALUE"
)

type spyIn struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

type spyOut struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// spyOps are one spy provider's declarations.
type spyOps struct {
	get, fail, boom, run, open, prob def.Typed[spyIn, spyOut]
	logs                             def.Typed[spyIn, apitypes.Frame]
	restart                          def.Typed[spyIn, spyOut]
}

func refineSpy(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = `^[a-z0-9-]+$`
	}
}

func defineSpyOps(prov string) spyOps {
	op := func(kind def.Kind, action string, scope def.Scope) def.Op {
		id := prov + ".things." + action
		if action == "" {
			id = prov + ".things.get"
		}
		return def.Op{
			ID: id, Provider: prov, Kind: kind, ResourceType: "things", Action: action,
			Summary: "s", Description: "Spy operation for the dispatcher tests.",
			Scope: scope, Risk: def.RiskReadOnly, Mode: def.ModeSync,
			CLI:    def.CLIBinding{Noun: "thing", Verb: "get", Positional: "name"},
			Refine: refineSpy,
		}
	}
	sub := func(action string, scope def.Scope) def.Op { return op(def.KindSubresource, action, scope) }
	logs := sub("logs", def.Scope{Name: prov + ":read"})
	logs.Mode = def.ModeStream
	restart := op(def.KindResourceAction, "restart", def.Scope{Name: prov + ":write"})
	restart.Risk, restart.Mode, restart.CLI.Verb = def.RiskMutating, def.ModeLongRunning, "restart"
	return spyOps{
		get:     def.Define[spyIn, spyOut](op(def.KindGet, "", def.Scope{Name: "test:write"})),
		fail:    def.Define[spyIn, spyOut](sub("fail", def.Scope{Name: prov + ":read"})),
		boom:    def.Define[spyIn, spyOut](sub("boom", def.Scope{Name: prov + ":read"})),
		run:     def.Define[spyIn, spyOut](sub("run", def.Scope{Name: "scripts:run", PerTarget: "scripts:run:{name}"})),
		open:    def.Define[spyIn, spyOut](sub("open", def.Scope{})),
		prob:    def.Define[spyIn, spyOut](sub("prob", def.Scope{Name: prov + ":read"})),
		logs:    def.Define[spyIn, apitypes.Frame](logs),
		restart: def.Define[spyIn, spyOut](restart),
	}
}

var spyDecls = map[string]spyOps{"spy": defineSpyOps("spy"), "spy2": defineSpyOps("spy2")}

// spyProvider counts every preflight and every handler entry.
type spyProvider struct {
	name       string
	ops        spyOps
	ok         atomic.Bool // Preflight result
	preflights atomic.Int64
	handled    atomic.Int64
	ran        atomic.Int64 // Long-running Run calls
}

func newSpy(name string, ok bool) *spyProvider {
	p := &spyProvider{name: name, ops: spyDecls[name]}
	p.ok.Store(ok)
	return p
}

func (p *spyProvider) Spec() def.ProviderSpec {
	o := p.ops
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: []def.Op{
		o.get.Op(), o.fail.Op(), o.boom.Op(), o.run.Op(), o.open.Op(), o.prob.Op(), o.logs.Op(), o.restart.Op(),
	}}
}

func (p *spyProvider) Configure(func(any) error) error { return nil }

func (p *spyProvider) Preflight(context.Context) provider.Status {
	p.preflights.Add(1)
	if p.ok.Load() {
		return provider.Status{State: apitypes.ProviderEnabled}
	}
	return provider.Status{State: apitypes.ProviderUnavailable, Reason: p.name + " dependency missing"}
}

func (p *spyProvider) Bindings() []provider.Binding {
	o := p.ops
	echo := func(_ context.Context, in spyIn) (spyOut, error) {
		p.handled.Add(1)
		return spyOut(in), nil
	}
	return []provider.Binding{
		provider.Sync(o.get, echo),
		provider.Sync(o.run, echo),
		provider.Sync(o.open, echo),
		provider.Sync(o.fail, func(context.Context, spyIn) (spyOut, error) {
			p.handled.Add(1)
			return spyOut{}, fmt.Errorf("open %s: bad credentials %s", sentinelPath, tokenShaped)
		}),
		provider.Sync(o.boom, func(context.Context, spyIn) (spyOut, error) {
			p.handled.Add(1)
			panic(panicSentinel2)
		}),
		provider.Sync(o.prob, func(context.Context, spyIn) (spyOut, error) {
			p.handled.Add(1)
			return spyOut{}, problem.New(problem.NotFound, "no such thing")
		}),
		provider.Stream(o.logs, func(_ context.Context, in spyIn, emit func(apitypes.Frame) error) error {
			p.handled.Add(1)
			return emit(apitypes.Frame{S: apitypes.StreamStdout, D: in.Name})
		}),
		provider.InProcess(o.restart, provider.InProcessSpec[spyIn, spyOut]{
			Run: func(context.Context, spyIn, *provider.Run) (spyOut, error) {
				p.ran.Add(1)
				return spyOut{}, nil
			},
		}),
	}
}

type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	reg   *core.Registry
	d     *core.Dispatcher
	spy   *spyProvider
	spy2  *spyProvider
	logs  *logBuf
	clock *fakeClock
}

// newFixture registers spy (enabled in configuration unless spyDisabled,
// with the given preflight result) and spy2 (always disabled).
func newFixture(t *testing.T, spyDisabled, spyOK bool) *fixture {
	t.Helper()
	f := &fixture{
		spy: newSpy("spy", spyOK), spy2: newSpy("spy2", true), logs: &logBuf{},
		clock: &fakeClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)},
	}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	reg, err := core.New([]provider.Provider{f.spy, f.spy2}, core.Options{
		Host: "testhost", Version: "v0.0.0-test", Logger: log, Now: f.clock.Now,
		Providers: map[string]core.ProviderConfig{"spy": {Enabled: !spyDisabled}, "spy2": {Enabled: false}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	f.reg = reg
	f.d = core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	return f
}

func caller(scopes ...string) auth.Principal {
	return auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "dispatcher-tests", Scopes: scopes}
}

func (f *fixture) invoke(t *testing.T, op def.Op, input string, scopes ...string) (core.InvokeResult, error) {
	t.Helper()
	return f.d.Invoke(context.Background(), core.InvokeRequest{
		Caller: caller(scopes...), Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input),
	})
}

// wantProblem asserts err is a problem with the given status and slug, and
// returns it as the client would decode it from the wire.
func wantProblem(t *testing.T, err error, status int, slug problem.Slug) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) {
		t.Fatalf("error = %v (%T), want a %s problem", err, err, slug)
	}
	b, merr := json.Marshal(p)
	if merr != nil {
		t.Fatal(merr)
	}
	var wire problem.Error
	if uerr := json.Unmarshal(b, &wire); uerr != nil {
		t.Fatal(uerr)
	}
	if wire.Status != status || wire.Slug() != slug {
		t.Fatalf("problem = %s, want %d %s", b, status, slug)
	}
	return &wire
}

func wantOK(t *testing.T, res core.InvokeResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
}

// TC-30: a missing scope returns 403 naming the requirement, and the handler
// does not run. The scoped token passes the scope check.
func TestMissingScopeReturns403NamingTheRequirement(t *testing.T) {
	f := newFixture(t, false, true)
	get := f.spy.ops.get.Op()

	_, err := f.invoke(t, get, `{"name":"a"}`)
	p := wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if p.Extensions["requiredScope"] != "test:write" {
		t.Fatalf("requiredScope = %v, want test:write", p.Extensions["requiredScope"])
	}
	// A scope that only resembles the requirement is still a denial.
	_, err = f.invoke(t, get, `{"name":"a"}`, "test:read", "test:*", "*")
	wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if n := f.spy.handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times for denied callers", n)
	}

	res, err := f.invoke(t, get, `{"name":"a"}`, "test:write")
	wantOK(t, res, err)
	if out, ok := res.Body.(spyOut); !ok || out.Name != "a" {
		t.Fatalf("body = %#v", res.Body)
	}
	if n := f.spy.handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

// TC-31: scope, then capability, then validation; the handler never runs.
func TestDispatcherPipelineOrder(t *testing.T) {
	f := newFixture(t, true, true)
	get := f.spy.ops.get.Op()
	const bad = `{"name":5}`

	_, err := f.invoke(t, get, bad)
	wantProblem(t, err, http.StatusForbidden, problem.Forbidden)

	_, err = f.invoke(t, get, bad, "test:write")
	p := wantProblem(t, err, http.StatusServiceUnavailable, problem.CapabilityUnavailable)
	if p.Extensions["provider"] != "spy" {
		t.Fatalf("provider = %v, want spy", p.Extensions["provider"])
	}

	if err := f.reg.Apply(context.Background(), map[string]core.ProviderConfig{"spy": {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	_, err = f.invoke(t, get, bad, "test:write")
	wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)

	if n := f.spy.handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times", n)
	}
	// The same order holds for a long-running op: denied before anything runs.
	_, err = f.invoke(t, f.spy.ops.restart.Op(), `{"name":"a"}`)
	wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if n := f.spy.ran.Load(); n != 0 {
		t.Fatalf("long-running Run called %d times", n)
	}
}

// TC-ERR11: capability-unavailable names provider and reason, and the
// preflight of an unavailable provider is re-run only once its last check
// is more than 5 seconds old.
func TestCapabilityUnavailableNamesProviderAndPreflightRechecks(t *testing.T) {
	f := newFixture(t, false, false)
	get := f.spy.ops.get.Op()
	if n := f.spy.preflights.Load(); n != 1 {
		t.Fatalf("preflights after New = %d, want 1", n)
	}

	_, err := f.invoke(t, get, `{"name":"a"}`, "test:write")
	p := wantProblem(t, err, http.StatusServiceUnavailable, problem.CapabilityUnavailable)
	if p.Extensions["provider"] != "spy" || p.Extensions["reason"] != "spy dependency missing" {
		t.Fatalf("unavailable problem extensions = %v", p.Extensions)
	}
	_, err = f.invoke(t, f.spy2.ops.get.Op(), `{"name":"a"}`, "test:write")
	p = wantProblem(t, err, http.StatusServiceUnavailable, problem.CapabilityUnavailable)
	reason, _ := p.Extensions["reason"].(string)
	if p.Extensions["provider"] != "spy2" || !strings.Contains(reason, "disabled") {
		t.Fatalf("disabled problem extensions = %v", p.Extensions)
	}

	// Repaired, but the last check is only 4 seconds old.
	f.spy.ok.Store(true)
	f.clock.Advance(4 * time.Second)
	_, err = f.invoke(t, get, `{"name":"a"}`, "test:write")
	wantProblem(t, err, http.StatusServiceUnavailable, problem.CapabilityUnavailable)
	if n := f.spy.preflights.Load(); n != 1 {
		t.Fatalf("preflights within 5 seconds = %d, want 1", n)
	}

	f.clock.Advance(2 * time.Second)
	res, err := f.invoke(t, get, `{"name":"a"}`, "test:write")
	wantOK(t, res, err)
	if n := f.spy.preflights.Load(); n != 2 {
		t.Fatalf("preflights after 6 seconds = %d, want 2", n)
	}
	if n := f.spy.handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	if n := f.spy2.preflights.Load(); n != 0 {
		t.Fatalf("disabled provider preflighted %d times", n)
	}
}

// Concurrent calls on a stale unavailable provider share one re-check.
func TestPreflightRecheckRunsOncePerInterval(t *testing.T) {
	f := newFixture(t, false, false)
	get := f.spy.ops.get.Op()
	f.clock.Advance(6 * time.Second)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := f.d.Invoke(context.Background(), core.InvokeRequest{
				Caller: caller("test:write"), Op: get, Input: json.RawMessage(`{"name":"a"}`),
			})
			var p *problem.Error
			if !errors.As(err, &p) || p.Slug() != problem.CapabilityUnavailable {
				t.Errorf("err = %v, want capability-unavailable", err)
			}
		})
	}
	wg.Wait()
	if n := f.spy.preflights.Load(); n != 2 {
		t.Fatalf("preflights = %d, want 2 (startup plus one re-check)", n)
	}
}

func fieldErrors(t *testing.T, p *problem.Error) []problem.FieldError {
	t.Helper()
	b, err := json.Marshal(p.Extensions["errors"])
	if err != nil {
		t.Fatal(err)
	}
	var fe []problem.FieldError
	if err := json.Unmarshal(b, &fe); err != nil {
		t.Fatalf("errors[] = %s: %v", b, err)
	}
	return fe
}

// TC-ERR12: validation failures carry errors[] and never reach handlers.
func TestValidationFailuresCarryErrorsAndSkipHandlers(t *testing.T) {
	f := newFixture(t, false, true)
	get := f.spy.ops.get.Op()
	cases := []struct {
		name, input, path string
	}{
		{"malformed JSON", `{"name":`, ""},
		{"trailing data", `{"name":"a"} {}`, ""},
		{"not an object", `["a"]`, ""},
		{"wrong type", `{"name":5}`, "/name"},
		{"wrong nested type", `{"name":"a","count":"many"}`, "/count"},
		{"missing required field", `{}`, "/name"},
		{"additional property", `{"name":"a","bogus":1}`, "/bogus"},
		{"case-folded duplicate", `{"name":"a","NAME":"b"}`, "/NAME"},
		{"pattern", `{"name":"` + secretLike + `"}`, "/name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.invoke(t, get, c.input, "test:write")
			p := wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)
			fe := fieldErrors(t, p)
			if len(fe) == 0 {
				t.Fatal("errors[] is empty")
			}
			paths := []string{}
			for _, e := range fe {
				paths = append(paths, e.Path)
				if e.Message == "" {
					t.Errorf("entry %+v has no message", e)
				}
			}
			found := false
			for _, pth := range paths {
				found = found || pth == c.path
			}
			if !found {
				t.Errorf("errors[] paths = %q, want one of them %q", paths, c.path)
			}
			b, _ := json.Marshal(p)
			if strings.Contains(string(b), secretLike) {
				t.Errorf("the problem echoes the input value: %s", b)
			}
		})
	}
	if n := f.spy.handled.Load(); n != 0 {
		t.Fatalf("handler ran %d times", n)
	}
	// A transport that could not assemble the input reports it at the
	// validation step, so the order still holds: no scope means 403 first.
	inErr := problem.New(problem.ValidationFailed, "the request body must be a JSON object")
	_, err := f.d.Invoke(context.Background(), core.InvokeRequest{Caller: caller(), Op: get, InputError: inErr})
	wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	_, err = f.d.Invoke(context.Background(), core.InvokeRequest{Caller: caller("test:write"), Op: get, InputError: inErr})
	if fe := fieldErrors(t, wantProblem(t, err, http.StatusBadRequest, problem.ValidationFailed)); len(fe) == 0 {
		t.Fatal("transport input error has an empty errors[]")
	}
}

// TC-ERR13: internal errors do not leak causes.
func TestInternalErrorsDoNotLeakCauses(t *testing.T) {
	f := newFixture(t, false, true)
	_, err := f.invoke(t, f.spy.ops.fail.Op(), `{"name":"a"}`, "spy:read")
	p := wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	b, _ := json.Marshal(p)
	for _, leak := range []string{sentinelPath, "SENTINEL", tokenShaped, "nxs_", "credentials"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("problem body leaks %q: %s", leak, b)
		}
	}
	if !strings.Contains(f.logs.String(), sentinelPath) {
		t.Fatalf("the cause was not logged: %s", f.logs.String())
	}

	// A handler's own problem passes through unchanged.
	_, err = f.invoke(t, f.spy.ops.prob.Op(), `{"name":"a"}`, "spy:read")
	wantProblem(t, err, http.StatusNotFound, problem.NotFound)
}

// A handler panic is contained to its request, logged with its stack, and
// never reported to the caller.
func TestHandlerPanicIsContained(t *testing.T) {
	f := newFixture(t, false, true)
	_, err := f.invoke(t, f.spy.ops.boom.Op(), `{"name":"a"}`, "spy:read")
	p := wantProblem(t, err, http.StatusInternalServerError, problem.InternalError)
	if b, _ := json.Marshal(p); strings.Contains(string(b), panicSentinel2) {
		t.Fatalf("problem leaks the panic value: %s", b)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, panicSentinel2) || !strings.Contains(logs, "goroutine") {
		t.Fatalf("panic not logged with a stack: %s", logs)
	}
	res, err := f.invoke(t, f.spy.ops.get.Op(), `{"name":"b"}`, "test:write")
	wantOK(t, res, err)
}

// Default-deny: no authenticated caller means 401; an op the caller passes
// with a weakened declaration is checked against the registered one; a
// provider op that declares no scope needs its provider's read scope.
func TestDispatcherDeniesByDefault(t *testing.T) {
	f := newFixture(t, false, true)
	get := f.spy.ops.get.Op()

	_, err := f.d.Invoke(context.Background(), core.InvokeRequest{Op: get, Input: json.RawMessage(`{"name":"a"}`)})
	wantProblem(t, err, http.StatusUnauthorized, problem.Unauthorized)

	weakened := get
	weakened.Scope = def.Scope{}
	weakened.Provider = def.SystemProvider
	_, err = f.invoke(t, weakened, `{"name":"a"}`)
	p := wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if p.Extensions["requiredScope"] != "test:write" {
		t.Fatalf("requiredScope = %v", p.Extensions["requiredScope"])
	}

	open := f.spy.ops.open.Op()
	_, err = f.invoke(t, open, `{"name":"a"}`)
	p = wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if p.Extensions["requiredScope"] != "spy:read" {
		t.Fatalf("requiredScope of an unscoped provider op = %v, want spy:read", p.Extensions["requiredScope"])
	}
	res, err := f.invoke(t, open, `{"name":"a"}`, "spy:read")
	wantOK(t, res, err)

	unknown := get
	unknown.ID = "spy.things.unregistered"
	_, err = f.invoke(t, unknown, `{"name":"a"}`, "test:write")
	var up *problem.Error
	if !errors.As(err, &up) || up.Status != http.StatusNotFound {
		t.Fatalf("unregistered op: %v", err)
	}
	if n := f.spy.handled.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	if !strings.Contains(f.logs.String(), "request denied") {
		t.Errorf("denials are not recorded: %s", f.logs.String())
	}
}

// scripts:run satisfies scripts:run:{name}; the per-target scope is resolved
// from the input, so a Target that names another script cannot borrow it.
func TestPerTargetScope(t *testing.T) {
	f := newFixture(t, false, true)
	run := f.spy.ops.run.Op()
	call := func(target string, scopes ...string) error {
		_, err := f.d.Invoke(context.Background(), core.InvokeRequest{
			Caller: caller(scopes...), Op: run, Target: target, Input: json.RawMessage(`{"name":"deploy"}`),
		})
		return err
	}
	if err := call("deploy", "scripts:run:deploy"); err != nil {
		t.Fatalf("per-target scope: %v", err)
	}
	if err := call("deploy", "scripts:run"); err != nil {
		t.Fatalf("scripts:run: %v", err)
	}
	p := wantProblem(t, call("deploy", "scripts:run:other"), http.StatusForbidden, problem.Forbidden)
	if p.Extensions["requiredScope"] != "scripts:run" {
		t.Fatalf("requiredScope = %v", p.Extensions["requiredScope"])
	}
	wantProblem(t, call("other", "scripts:run:other"), http.StatusForbidden, problem.Forbidden)
	wantProblem(t, call("other", "scripts:run:deploy"), http.StatusBadRequest, problem.ValidationFailed)
	if n := f.spy.handled.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2", n)
	}
}

// A stream op is checked before its stream exists; the transport drives it.
func TestStreamOpGoesThroughTheChecks(t *testing.T) {
	f := newFixture(t, false, true)
	logs := f.spy.ops.logs.Op()
	res, err := f.invoke(t, logs, `{"name":"a"}`)
	wantProblem(t, err, http.StatusForbidden, problem.Forbidden)
	if res.Stream != nil {
		t.Fatal("a denied call returned a stream")
	}
	res, err = f.invoke(t, logs, `{"name":"a"}`, "spy:read")
	wantOK(t, res, err)
	if res.Stream == nil {
		t.Fatal("no stream for a ModeStream op")
	}
	var frames []apitypes.Frame
	if err := res.Stream(context.Background(), func(fr apitypes.Frame) error {
		frames = append(frames, fr)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0].D != "a" {
		t.Fatalf("frames = %+v", frames)
	}
}
