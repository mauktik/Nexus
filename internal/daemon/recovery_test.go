// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package daemon

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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/store"
)

// recIn is the input of the recovery test ops: the lane comes from it.
type recIn struct {
	Name   string `json:"name"`
	Lane   string `json:"lane,omitempty"`
	Policy string `json:"policy,omitempty"`
}

func refineRec(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = `^[a-z0-9-]+$`
	}
	if p := s.Properties["lane"]; p != nil {
		p.Pattern = `^[a-z0-9:-]*$`
	}
	if p := s.Properties["policy"]; p != nil {
		p.Enum = []any{"", "queue", "reject"}
	}
}

var (
	recJob = def.Define[recIn, jobOut](def.Op{
		ID: "scripts.recjobs.run", Provider: "scripts", Kind: def.KindResourceAction, ResourceType: "recjobs",
		Action: "run", Summary: "s",
		Description: "Detached job for the restart recovery tests. Side effects: runs a shell child; long-running.",
		Scope:       def.Scope{Name: "scripts:run"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Refine: refineRec, CLI: def.CLIBinding{Noun: "recjob", Verb: "start", Positional: "name"},
	})
	recTask = def.Define[recIn, jobOut](def.Op{
		ID: "scripts.rectasks.run", Provider: "scripts", Kind: def.KindResourceAction, ResourceType: "rectasks",
		Action: "run", Summary: "s",
		Description: "In-process task for the restart recovery tests. Side effects: none; long-running.",
		Scope:       def.Scope{Name: "scripts:run"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		Refine: refineRec, CLI: def.CLIBinding{Noun: "rectask", Verb: "start", Positional: "name"},
	})
)

// recProvider runs recJob detached and recTask in process. Prepare and Run
// call the test's hooks.
type recProvider struct {
	mu      sync.Mutex
	prepare func(name string)                            // Called first in Prepare
	script  func(name string) string                     // The child's shell script; nil means exit 0
	run     func(ctx context.Context, name string) error // In-process work; nil returns at once
	order   []string                                     // Names in Prepare order
	runCtxs map[string]error                             // ctx.Err() as each Run returned
}

func (*recProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "scripts", ReadScope: "scripts:read", Ops: []def.Op{recJob.Op(), recTask.Op()}}
}

func (*recProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (*recProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func recLane(_ context.Context, in recIn) (provider.Lane, error) {
	if in.Lane == "" {
		return provider.NoLane, nil
	}
	return provider.Lane{Key: in.Lane, Policy: provider.LanePolicy(in.Policy)}, nil
}

func (p *recProvider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Detached(recJob, provider.DetachedSpec[recIn, jobOut]{
			Lane: recLane,
			Prepare: func(_ context.Context, in recIn, _ *provider.Run) (detach.Spec, error) {
				p.mu.Lock()
				p.order = append(p.order, in.Name)
				hook, script := p.prepare, p.script
				p.mu.Unlock()
				if hook != nil {
					hook(in.Name)
				}
				sh := "exit 0"
				if script != nil {
					sh = script(in.Name)
				}
				return detach.Spec{Argv: []string{"/bin/sh", "-c", sh}, Env: []string{"PATH=/usr/bin:/bin"},
					KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}, nil
			},
			Finalize: func(_ context.Context, _ recIn, x detach.Exit, _ *provider.Run) (jobOut, error) {
				return jobOut{ExitCode: x.ExitCode}, nil
			},
			LostDetail: func(_ *provider.Run, in recIn) string { return "lost " + in.Name },
		}),
		provider.InProcess(recTask, provider.InProcessSpec[recIn, jobOut]{
			Lane: recLane,
			Run: func(ctx context.Context, in recIn, _ *provider.Run) (jobOut, error) {
				p.mu.Lock()
				run := p.run
				p.mu.Unlock()
				var err error
				if run != nil {
					err = run(ctx, in.Name)
				}
				p.mu.Lock()
				if p.runCtxs == nil {
					p.runCtxs = map[string]error{}
				}
				p.runCtxs[in.Name] = ctx.Err()
				p.mu.Unlock()
				return jobOut{}, err
			},
		}),
	}
}

func (p *recProvider) prepared() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.order)
}

// recWorld is a state directory with a token, a config, and the daemons
// started on them in turn.
type recWorld struct {
	t        *testing.T
	stateDir string
	auditDir string
	cfgPath  string
	listen   string
	token    string
	prov     *recProvider
	seams    detach.Options
	client   *http.Client
	// plat, process and logs, when set, are the daemons' Platform, Process
	// and log output (backend_selection_test.go).
	plat    *platform.Info
	process detach.ProcessConfig
	logs    io.Writer
}

func newRecWorld(t *testing.T, seams detach.Options) *recWorld {
	t.Helper()
	w := &recWorld{t: t, stateDir: t.TempDir(), auditDir: t.TempDir(), listen: freePort(t), prov: &recProvider{},
		seams: seams, client: &http.Client{Timeout: 10 * time.Second}}
	tok, _, err := auth.Create(w.stateDir, auth.CreateRequest{Name: "recovery-test", Scopes: []string{"scripts:run", "scripts:read"}})
	if err != nil {
		t.Fatal(err)
	}
	w.token = tok
	w.cfgPath = filepath.Join(t.TempDir(), "nexusd.yaml")
	writeConfig(t, w.cfgPath, w.listen, "scripts")
	return w
}

// seed stores records as a previous nexusd left them, before any daemon
// opens state.db. It returns their IDs in order.
func (w *recWorld) seed(recs []store.OpRecord, status []apitypes.OperationStatus) []string {
	w.t.Helper()
	st, err := store.Open(w.stateDir, store.Options{})
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			w.t.Fatal(err)
		}
	}()
	ids := make([]string, len(recs))
	for i, rec := range recs {
		res, err := st.CreateWithIdempotency(rec, store.IdempotencyKey{})
		if err != nil {
			w.t.Fatal(err)
		}
		ids[i] = res.ID
		if status[i] == apitypes.StatusRunning {
			if _, err := st.CompareAndSetStatus(res.ID, apitypes.StatusQueued, apitypes.StatusRunning); err != nil {
				w.t.Fatal(err)
			}
		}
	}
	return ids
}

func jobRec(name, lane string, policy store.LanePolicy) store.OpRecord {
	in, err := json.Marshal(recIn{Name: name, Lane: lane, Policy: string(policy)})
	if err != nil {
		panic(err)
	}
	rec := store.OpRecord{Action: recJob.Op().ID, Provider: "scripts", Target: name, TokenName: "seeder",
		Input: in, Exec: store.ExecDetached, StartScope: "scripts:run"}
	if lane != "" {
		rec.Lane = store.Lane{Key: lane, Policy: policy, Limit: 1}
	}
	return rec
}

// running is one started daemon.
type running struct {
	d      *Daemon
	cancel context.CancelFunc
	runErr chan error
	exited bool
}

func (w *recWorld) start() *running {
	w.t.Helper()
	cfg, err := config.Load(w.cfgPath)
	if err != nil {
		w.t.Fatal(err)
	}
	logs := w.logs
	if logs == nil {
		logs = io.Discard
	}
	d, err := New(cfg, Options{
		Providers:  []provider.Provider{w.prov},
		ConfigPath: w.cfgPath,
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
		StateDir:   w.stateDir,
		AuditDir:   w.auditDir,
		Detach:     w.seams,
		Platform:   w.plat,
		Process:    w.process,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{d: d, cancel: cancel, runErr: make(chan error, 1)}
	go func() { r.runErr <- d.Run(ctx) }()
	w.t.Cleanup(func() { r.stop(w.t) })
	return r
}

func (r *running) stop(t *testing.T) {
	r.cancel()
	if r.exited {
		return
	}
	r.exited = true
	select {
	case err := <-r.runErr:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Errorf("the daemon did not stop")
	}
}

// waitHealthy polls /healthz until it answers 200.
func (w *recWorld) waitHealthy() {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := w.client.Get("http://" + w.listen + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("/healthz never answered 200: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// post starts op on the named thing, returning the status and the decoded
// body.
func (w *recWorld) post(op def.Op, name string, in recIn) (int, map[string]any, error) {
	body, err := json.Marshal(struct {
		Lane   string `json:"lane,omitempty"`
		Policy string `json:"policy,omitempty"`
	}{in.Lane, in.Policy})
	if err != nil {
		return 0, nil, err
	}
	url := fmt.Sprintf("http://%s/v1/providers/scripts/%s/%s/%s", w.listen, op.ResourceType, name, op.Action)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("decode %s: %w", b, err)
	}
	return resp.StatusCode, m, nil
}

// waitRecord polls the running daemon's store until id is terminal.
func waitTerminal(t *testing.T, r *running, id string, limit time.Duration) store.OpRecord {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		rec, err := r.d.ops.Get(id)
		if err == nil && rec.Status.Terminal() {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s: %+v (%v), not terminal after %v", id, rec, err, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gatedState wraps local's UnitState: the units of held IDs block until
// release is closed, ignoring their context like a hung systemctl, then
// answer absent; other units answer as local does.
func gatedState(local detach.Options, held func(unit string) bool, release <-chan struct{}) func(context.Context, string) (detach.UnitState, error) {
	return func(ctx context.Context, unit string) (detach.UnitState, error) {
		if !held(unit) {
			return local.UnitState(ctx, unit)
		}
		<-release
		return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
	}
}

func idSet(ids ...string) func(string) bool {
	units := map[string]bool{}
	for _, id := range ids {
		units[detach.Unit(id)] = true
	}
	return func(unit string) bool { return units[unit] }
}

func wantOutcomeUnknown(t *testing.T, rec store.OpRecord) {
	t.Helper()
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.OutcomeUnknown {
		t.Fatalf("operation %s: %s %+v, want Failed outcome-unknown", rec.ID, rec.Status, rec.Error)
	}
}

// TC-210: the lane of a reattaching Running record is held from the first
// request, before classification has decided anything.
func TestLanesOccupiedBeforeClassification(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	local := detachtest.Local()
	var held atomic.Pointer[func(string) bool]
	seams := local
	seams.UnitState = func(ctx context.Context, unit string) (detach.UnitState, error) {
		return gatedState(local, (*held.Load()), release)(ctx, unit)
	}
	w := newRecWorld(t, seams)
	ids := w.seed([]store.OpRecord{jobRec("held", "script:held", store.LaneReject)},
		[]apitypes.OperationStatus{apitypes.StatusRunning})
	f := idSet(ids[0])
	held.Store(&f)

	r := w.start()
	w.waitHealthy()
	code, body, err := w.post(recJob.Op(), "next", recIn{Lane: "script:held", Policy: "reject"})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusConflict || body["blockingOperationId"] != ids[0] {
		t.Fatalf("first POST: %d %v, want 409 naming %s", code, body, ids[0])
	}
	if rec, err := r.d.ops.Get(ids[0]); err != nil || rec.Status != apitypes.StatusRunning {
		t.Fatalf("seeded op: %+v %v, want still Running", rec, err)
	}

	open()
	wantOutcomeUnknown(t, waitTerminal(t, r, ids[0], 20*time.Second))

	// The slot is freed just after the terminal commit.
	for retry := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		code, body, err = w.post(recJob.Op(), "next", recIn{Lane: "script:held", Policy: "reject"})
		if err != nil {
			t.Fatal(err)
		}
		if code != http.StatusConflict || time.Now().After(retry) {
			break
		}
	}
	if code != http.StatusAccepted {
		t.Fatalf("second POST: %d %v, want 202", code, body)
	}
	waitTerminal(t, r, body["id"].(string), 20*time.Second)
}

// TC-211: re-enqueued operations run before a request made after the
// restart, in order, each after the previous one is terminal.
func TestRestoredQueueOrderPrecedesNewRequests(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	local := detachtest.Local()
	var held atomic.Pointer[func(string) bool]
	seams := local
	seams.UnitState = func(ctx context.Context, unit string) (detach.UnitState, error) {
		return gatedState(local, (*held.Load()), release)(ctx, unit)
	}
	w := newRecWorld(t, seams)
	const lane = "script:fifo"
	ids := w.seed([]store.OpRecord{
		jobRec("a", lane, store.LaneQueue), jobRec("b", lane, store.LaneQueue), jobRec("c", lane, store.LaneQueue),
	}, []apitypes.OperationStatus{apitypes.StatusRunning, apitypes.StatusQueued, apitypes.StatusQueued})
	f := idSet(ids[0])
	held.Store(&f)

	var r atomic.Pointer[running]
	byName := sync.Map{}
	byName.Store("a", ids[0])
	byName.Store("b", ids[1])
	byName.Store("c", ids[2])
	previous := map[string]string{"b": "a", "c": "b", "d": "c"}
	var violations sync.Map
	w.prov.prepare = func(name string) {
		prev, ok := byName.Load(previous[name])
		if !ok {
			violations.Store(name, "previous operation unknown")
			return
		}
		rec, err := r.Load().d.ops.Get(prev.(string))
		if err != nil || !rec.Status.Terminal() {
			violations.Store(name, fmt.Sprintf("started while %s was %s (%v)", previous[name], rec.Status, err))
		}
	}
	r.Store(w.start())
	w.waitHealthy()
	code, body, err := w.post(recJob.Op(), "d", recIn{Lane: lane, Policy: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusAccepted {
		t.Fatalf("POST d: %d %v", code, body)
	}
	d := body["id"].(string)
	byName.Store("d", d)
	time.Sleep(300 * time.Millisecond)
	if got := w.prov.prepared(); len(got) != 0 {
		t.Fatalf("started before A was classified: %q", got)
	}
	if rec, err := r.Load().d.ops.Get(d); err != nil || rec.Status != apitypes.StatusQueued {
		t.Fatalf("d: %+v %v, want Queued", rec, err)
	}

	now := time.Now()
	aDir := filepath.Join(w.stateDir, "ops", ids[0])
	if err := os.MkdirAll(aDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := detach.WriteExit(aDir, detach.Exit{Started: now, Ended: now}); err != nil {
		t.Fatal(err)
	}
	open()
	a := waitTerminal(t, r.Load(), ids[0], 20*time.Second)
	if a.Status != apitypes.StatusSucceeded {
		t.Fatalf("a: %s %+v, want Succeeded from its exit.json", a.Status, a.Error)
	}
	if rec := waitTerminal(t, r.Load(), d, 30*time.Second); rec.Status != apitypes.StatusSucceeded {
		t.Fatalf("d: %s %+v", rec.Status, rec.Error)
	}
	if got := w.prov.prepared(); !slices.Equal(got, []string{"b", "c", "d"}) {
		t.Fatalf("execution order %q, want b c d", got)
	}
	violations.Range(func(k, v any) bool {
		t.Errorf("%s: %s", k, v)
		return true
	})
}

// TC-69: with 1,000 Running records and a UnitState that blocks, the
// listener is ready in under 2 seconds and lanes are already occupied.
func TestRecoveryDoesNotDelayReadiness(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	local := detachtest.Local()
	seams := local
	seams.UnitState = gatedState(local, func(string) bool { return true }, release)
	w := newRecWorld(t, seams)
	const n = 1000
	recs := make([]store.OpRecord, n)
	status := make([]apitypes.OperationStatus, n)
	for i := range recs {
		recs[i] = jobRec(fmt.Sprintf("s%d", i), fmt.Sprintf("script:s%d", i), store.LaneReject)
		status[i] = apitypes.StatusRunning
	}
	ids := w.seed(recs, status)

	t0 := time.Now()
	r := w.start()
	w.waitHealthy()
	if took := time.Since(t0); took >= 2*time.Second {
		t.Fatalf("ready after %v, want under 2s", took)
	}
	code, body, err := w.post(recJob.Op(), "s500", recIn{Lane: "script:s500", Policy: "reject"})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusConflict || body["blockingOperationId"] != ids[500] {
		t.Fatalf("POST: %d %v, want 409 naming %s", code, body, ids[500])
	}
	for _, i := range []int{0, 500, n - 1} {
		if rec, err := r.d.ops.Get(ids[i]); err != nil || rec.Status != apitypes.StatusRunning {
			t.Fatalf("record %d: %+v %v, want Running while recovery is in progress", i, rec, err)
		}
	}
}

// TC-70: SIGTERM stops intake, gives in-process operations their grace,
// and leaves a detached operation's unit running and unsignaled; after a
// restart the unfinished in-process operation is outcome-unknown and the
// detached one reaches its true end.
func TestSIGTERMLeavesDetachedWorkAlone(t *testing.T) {
	local := detachtest.Local()
	var mu sync.Mutex
	var signals []string
	seams := local
	seams.Signal = func(ctx context.Context, unit string, sig syscall.Signal) error {
		mu.Lock()
		signals = append(signals, unit+" "+sig.String())
		mu.Unlock()
		return local.Signal(ctx, unit, sig)
	}
	w := newRecWorld(t, seams)
	proceed := filepath.Join(t.TempDir(), "proceed")
	termSent := make(chan struct{})
	unstick := make(chan struct{})
	defer close(unstick)
	started := make(chan string, 2)
	w.prov.script = func(string) string {
		return "while [ ! -e " + proceed + " ]; do sleep 0.05; done; exit 3"
	}
	w.prov.run = func(ctx context.Context, name string) error {
		started <- name
		switch name {
		case "grace":
			<-termSent
			time.Sleep(time.Second)
			return nil
		default: // stuck: never finishes in time, and ignores its context
			<-unstick
			return errors.New("unstuck")
		}
	}

	r := w.start()
	w.waitHealthy()
	ids := map[string]string{}
	for _, s := range []struct {
		op   def.Op
		name string
	}{{recTask.Op(), "grace"}, {recTask.Op(), "stuck"}, {recJob.Op(), "bg"}} {
		code, body, err := w.post(s.op, s.name, recIn{})
		if err != nil || code != http.StatusAccepted {
			t.Fatalf("POST %s: %d %v %v", s.name, code, body, err)
		}
		ids[s.name] = body["id"].(string)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("the in-process operations did not start")
		}
	}
	bgUnit := detach.Unit(ids["bg"])
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, err := seams.UnitState(context.Background(), bgUnit); err == nil && st.ActiveState == "active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached operation's supervisor never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		if err := os.WriteFile(proceed, nil, 0o600); err != nil {
			t.Error(err)
		}
	}()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	sentAt := time.Now()
	close(termSent)
	// Intake stops: requests fail once draining begins.
	for {
		_, _, err := w.post(recTask.Op(), "late", recIn{})
		if err != nil {
			break
		}
		if time.Since(sentAt) > 3*time.Second {
			t.Fatal("requests still accepted 3s after SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-r.runErr:
		r.exited = true
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon did not stop after SIGTERM")
	}
	if took := time.Since(sentAt); took > ShutdownTimeout+3*time.Second {
		t.Fatalf("shutdown took %v", took)
	}

	st, err := store.Open(w.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	grace, gerr := st.Get(ids["grace"])
	stuck, serr := st.Get(ids["stuck"])
	bg, berr := st.Get(ids["bg"])
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(gerr, serr, berr); err != nil {
		t.Fatal(err)
	}
	w.prov.mu.Lock()
	graceCtx := w.prov.runCtxs["grace"]
	w.prov.mu.Unlock()
	if grace.Status != apitypes.StatusSucceeded || graceCtx != nil {
		t.Fatalf("grace: %s (context %v), want Succeeded inside the grace period", grace.Status, graceCtx)
	}
	if stuck.Status != apitypes.StatusRunning || bg.Status != apitypes.StatusRunning {
		t.Fatalf("after shutdown: stuck %s, bg %s; want both Running", stuck.Status, bg.Status)
	}
	if st, err := seams.UnitState(context.Background(), bgUnit); err != nil || st.ActiveState != "active" {
		t.Fatalf("the detached child after shutdown: %+v %v, want still running", st, err)
	}

	r2 := w.start()
	w.waitHealthy()
	wantOutcomeUnknown(t, waitTerminal(t, r2, ids["stuck"], 20*time.Second))
	if rec, err := r2.d.ops.Get(ids["bg"]); err != nil || rec.Status != apitypes.StatusRunning {
		t.Fatalf("bg after restart: %+v %v, want Running (reattached)", rec, err)
	}
	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bg = waitTerminal(t, r2, ids["bg"], 20*time.Second)
	if bg.Status != apitypes.StatusSucceeded || string(bg.Result) != `{"exitCode":3}` {
		t.Fatalf("bg: %s %s %+v, want its true end", bg.Status, bg.Result, bg.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signals) != 0 {
		t.Fatalf("signals sent: %q", signals)
	}
}
