package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
)

type itemIn struct {
	Name string `json:"name"`
}

type itemOut struct {
	Name string `json:"name"`
}

func itemOp(prov, id string, kind def.Kind, resType, action string) def.Op {
	op := def.Op{
		ID: id, Provider: prov, Kind: kind, ResourceType: resType, Action: action,
		Summary: "s", Description: "Test operation for the capabilities case.",
		Scope: def.Scope{Name: prov + ":read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: prov, Verb: "get"},
	}
	if kind == def.KindResourceAction || kind == def.KindProviderAction {
		op.Scope.Name, op.Risk, op.Mode = prov+":write", def.RiskMutating, def.ModeLongRunning
		op.CLI.Verb = "restart"
	}
	return op
}

// testProvider declares a list, a resource action and a provider action, so
// capabilities have resource types and actions to report.
type testProvider struct {
	name      string
	list      def.Typed[itemIn, itemOut]
	restart   def.Typed[itemIn, itemOut]
	refresh   def.Typed[itemIn, itemOut]
	preflight func(context.Context) provider.Status
	calls     atomic.Int64
}

// testOps are the declarations of one provider name. def.Define registers
// globally, so they are made once per name and reused by every test (and by
// every run under -count=N).
type testOps struct {
	list, restart, refresh def.Typed[itemIn, itemOut]
}

var (
	testOpsMu sync.Mutex
	testOpsBy = map[string]testOps{}
)

func newTestProvider(name string, preflight func(context.Context) provider.Status) *testProvider {
	testOpsMu.Lock()
	defer testOpsMu.Unlock()
	o, ok := testOpsBy[name]
	if !ok {
		o = testOps{
			list:    def.Define[itemIn, itemOut](itemOp(name, name+".things.list", def.KindList, "things", "")),
			restart: def.Define[itemIn, itemOut](itemOp(name, name+".things.restart", def.KindResourceAction, "things", "restart")),
			refresh: def.Define[itemIn, itemOut](itemOp(name, name+".actions.refresh", def.KindProviderAction, "", "refresh")),
		}
		testOpsBy[name] = o
	}
	return &testProvider{name: name, list: o.list, restart: o.restart, refresh: o.refresh, preflight: preflight}
}

func (p *testProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read",
		Ops: []def.Op{p.list.Op(), p.restart.Op(), p.refresh.Op()}}
}

func (p *testProvider) Configure(decode func(any) error) error {
	var c struct{ Enabled bool }
	return decode(&c)
}

func (p *testProvider) Preflight(ctx context.Context) provider.Status {
	p.calls.Add(1)
	return p.preflight(ctx)
}

func (p *testProvider) Bindings() []provider.Binding {
	run := func(context.Context, itemIn, *provider.Run) (itemOut, error) { return itemOut{}, nil }
	return []provider.Binding{
		provider.Sync(p.list, func(_ context.Context, in itemIn) (itemOut, error) { return itemOut(in), nil }),
		provider.InProcess(p.restart, provider.InProcessSpec[itemIn, itemOut]{Run: run}),
		provider.InProcess(p.refresh, provider.InProcessSpec[itemIn, itemOut]{Run: run}),
	}
}

func enabled(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

// capsToken authenticates getCapabilities.
var capsToken string

func getCapabilities(t *testing.T, url string) apitypes.Capabilities {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+capsToken)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, body)
	}
	var caps apitypes.Capabilities
	if err := json.Unmarshal(body, &caps); err != nil {
		t.Fatalf("decode capabilities: %v: %s", err, body)
	}
	return caps
}

func byName(caps apitypes.Capabilities) map[string]apitypes.ProviderCapability {
	m := map[string]apitypes.ProviderCapability{}
	for _, p := range caps.Providers {
		m[p.Name] = p
	}
	return m
}

// TC-18: capabilities report every provider state (second-state).
func TestCapabilitiesReportEveryProviderState(t *testing.T) {
	toggle := filepath.Join(t.TempDir(), "ready")
	on := newTestProvider("alpha", enabled)
	off := newTestProvider("beta", enabled)
	flaky := newTestProvider("gamma", func(context.Context) provider.Status {
		if _, err := os.Stat(toggle); errors.Is(err, fs.ErrNotExist) {
			return provider.Status{State: apitypes.ProviderUnavailable, Reason: "toggle file is missing"}
		}
		return provider.Status{State: apitypes.ProviderEnabled}
	})

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	reg, err := core.New([]provider.Provider{on, off, flaky}, core.Options{
		Host: "testhost", Version: "v1.2.3-test", Logger: log,
		Providers: map[string]core.ProviderConfig{
			"alpha": {Enabled: true},
			"beta":  {Enabled: false},
			"gamma": {Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	stateDir := t.TempDir()
	capsToken, _, err = auth.Create(stateDir, auth.CreateRequest{Name: "caps"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(reg, server.Options{Logger: log, Auth: auth.New(stateDir, auth.Options{Logger: log})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	url := "http://" + ln.Addr().String() + "/v1/capabilities"

	// Step 1.
	caps := getCapabilities(t, url)
	if caps.ID != apitypes.CapabilitiesID || caps.Host != "testhost" ||
		caps.APIVersion != apitypes.APIVersion || caps.Version != "v1.2.3-test" {
		t.Fatalf("identity = %+v", caps)
	}
	// A zero Options.Platform reports the detected host (FR-18).
	if host := platform.Current(); caps.InitSystem != string(host.Init) || caps.OS.ID != host.OS.ID {
		t.Errorf("platform = %q %+v, want the detected %+v", caps.InitSystem, caps.OS, host)
	}
	want := map[string]apitypes.ProviderState{
		"alpha": apitypes.ProviderEnabled,
		"beta":  apitypes.ProviderDisabled,
		"gamma": apitypes.ProviderUnavailable,
	}
	got := byName(caps)
	if len(got) != len(want) {
		t.Fatalf("providers = %+v, want exactly alpha, beta, gamma", caps.Providers)
	}
	for name, state := range want {
		p := got[name]
		if p.Status != state {
			t.Errorf("%s status = %q, want %q", name, p.Status, state)
		}
		if state != apitypes.ProviderEnabled && strings.TrimSpace(p.Reason) == "" {
			t.Errorf("%s: %s without a reason", name, state)
		}
		if !slices.Equal(p.ResourceTypes, []string{"things"}) {
			t.Errorf("%s resourceTypes = %v, want [things]", name, p.ResourceTypes)
		}
		if !slices.Equal(p.Actions, []string{"refresh", "restart"}) {
			t.Errorf("%s actions = %v, want [refresh restart]", name, p.Actions)
		}
	}
	if !strings.Contains(got["gamma"].Reason, "toggle file is missing") {
		t.Errorf("gamma reason = %q, want the preflight's reason", got["gamma"].Reason)
	}
	if off.calls.Load() != 0 {
		t.Errorf("disabled provider was preflighted %d times", off.calls.Load())
	}

	// Step 2: the cached result stands until a refresh.
	if err := os.WriteFile(toggle, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s := byName(getCapabilities(t, url))["gamma"].Status; s != apitypes.ProviderUnavailable {
		t.Fatalf("step 2 gamma = %q, want unavailable (no refresh)", s)
	}

	// Step 3.
	got = byName(getCapabilities(t, url+"?refresh=true"))
	if s := got["gamma"].Status; s != apitypes.ProviderEnabled {
		t.Fatalf("step 3 gamma = %q, want enabled", s)
	}
	if got["gamma"].Reason != "" {
		t.Errorf("enabled gamma keeps reason %q", got["gamma"].Reason)
	}
	if s := got["beta"].Status; s != apitypes.ProviderDisabled {
		t.Errorf("refresh changed beta to %q", s)
	}
}

// Preflight failures of every kind leave the registry usable: a panic and an
// overrun both become unavailable with a reason (TC-ERR5 at the core level).
func TestPreflightPanicAndTimeoutAreUnavailable(t *testing.T) {
	boom := newTestProvider("delta", func(context.Context) provider.Status { panic("preflight exploded") })
	slow := newTestProvider("epsilon", func(ctx context.Context) provider.Status {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		return provider.Status{State: apitypes.ProviderEnabled}
	})
	reg, err := core.New([]provider.Provider{boom, slow}, core.Options{
		Host: "h", Version: "v", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		PreflightTimeout: 100 * time.Millisecond,
		Providers: map[string]core.ProviderConfig{
			"delta": {Enabled: true}, "epsilon": {Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(reg.Capabilities(context.Background(), false))
	for _, name := range []string{"delta", "epsilon"} {
		if got[name].Status != apitypes.ProviderUnavailable || got[name].Reason == "" {
			t.Errorf("%s = %+v, want unavailable with a reason", name, got[name])
		}
	}
	if strings.Contains(got["delta"].Reason, "preflight exploded") {
		t.Errorf("panic value leaked into the API: %q", got["delta"].Reason)
	}
}

// A reload that enables a provider swaps it in with a fresh preflight, and a
// Configure error keeps the previous enablement.
func TestApplySwapsEnablement(t *testing.T) {
	p := newTestProvider("zeta", enabled)
	reg, err := core.New([]provider.Provider{p}, core.Options{
		Host: "h", Version: "v", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if s := byName(reg.Capabilities(ctx, false))["zeta"].Status; s != apitypes.ProviderDisabled {
		t.Fatalf("without a section zeta = %q, want disabled", s)
	}
	if err := reg.Apply(ctx, map[string]core.ProviderConfig{"zeta": {Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if s := byName(reg.Capabilities(ctx, false))["zeta"].Status; s != apitypes.ProviderEnabled {
		t.Fatalf("after Apply zeta = %q, want enabled", s)
	}
	bad := map[string]core.ProviderConfig{"zeta": {Enabled: false, Decode: func(any) error {
		return errors.New("bad section")
	}}}
	if err := reg.Apply(ctx, bad); err == nil {
		t.Fatal("Apply with a Configure error succeeded")
	}
	if s := byName(reg.Capabilities(ctx, false))["zeta"].Status; s != apitypes.ProviderEnabled {
		t.Fatalf("failed Apply changed zeta to %q", s)
	}
}

// Bad provider sets are refused at construction.
func TestNewRejectsInvalidProviderSets(t *testing.T) {
	a := newTestProvider("eta", enabled)
	if _, err := core.New([]provider.Provider{a, a}, core.Options{}); err == nil {
		t.Error("duplicate provider name accepted")
	}
	sys := newTestProvider("system", enabled)
	if _, err := core.New([]provider.Provider{sys}, core.Options{}); err == nil {
		t.Error("provider named system accepted")
	}
}
