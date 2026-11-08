package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// The fleet provider is the fan-out tests' provider: a filterable list, a
// get and one mutating op, served by the real core and server.
const (
	fleetName       = "fleet"
	fleetReadScope  = "fleet:read"
	fleetWriteScope = "fleet:write"
)

type fleetProps struct {
	Status string `json:"status"`
}

type fleetThing = apitypes.Resource[fleetProps]

type fleetListIn struct {
	Filter []string `json:"filter,omitempty"`
}

type fleetNameIn struct {
	Name string `json:"name" jsonschema:"Thing name."`
}

type fleetRestartOut struct {
	apitypes.ActionResult
	Name string `json:"name"`
}

func refineFleetName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = `^[a-z][a-z0-9-]{0,30}$`
	}
}

var (
	fleetList = def.Define[fleetListIn, apitypes.List[fleetThing]](def.Op{
		ID: fleetName + ".things.list", Provider: fleetName, Kind: def.KindList, ResourceType: "things",
		Summary: "List things", Description: "Lists the fan-out tests' things. Read-only.",
		Scope: def.Scope{Name: fleetReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Filters: []def.Filter{{Key: "status", Description: "Thing status."}},
		CLI:     def.CLIBinding{Noun: "thing", Verb: "get"},
		Refine: func(s *jsonschema.Schema) {
			if p := s.Properties["filter"]; p != nil && p.Items != nil {
				p.Items.Pattern = `^status=[a-z]+$`
			}
		},
	})
	fleetGet = def.Define[fleetNameIn, fleetThing](def.Op{
		ID: fleetName + ".things.get", Provider: fleetName, Kind: def.KindGet, ResourceType: "things",
		Summary: "Get a thing", Description: "Returns one of the fan-out tests' things. Read-only.",
		Scope: def.Scope{Name: fleetReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "thing", Verb: "get", Positional: "name"},
		Refine: refineFleetName,
	})
	fleetRestart = def.Define[fleetNameIn, fleetRestartOut](def.Op{
		ID: fleetName + ".things.restart", Provider: fleetName, Kind: def.KindResourceAction, ResourceType: "things",
		Action: "restart", Summary: "Restart a thing",
		Description: "Restarts a fan-out test thing. Side effects: none outside the test; admission records the name. " +
			"Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: fleetWriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "thing", Verb: "restart", Positional: "name"},
		Refine: refineFleetName,
	})
)

func fleetSpec() def.ProviderSpec {
	return def.ProviderSpec{Name: fleetName, ReadScope: fleetReadScope,
		Ops: []def.Op{fleetList.Op(), fleetGet.Op(), fleetRestart.Op()}}
}

// fleetSpecs is the catalog plus the fleet provider: the CLI's tree.
func fleetSpecs() []def.ProviderSpec { return append(catalog.All(), fleetSpec()) }

// fleet is one server's provider: its things, under the server's own host
// name, and the restarts admitted, which count the mutations it received.
type fleet struct {
	own    string
	things []fleetThing
	onList func() // Runs inside every list request

	mu       sync.Mutex
	restarts []string
}

func newFleet(own string, things ...string) *fleet {
	f := &fleet{own: own}
	for _, t := range things {
		name, status, _ := strings.Cut(t, "=")
		f.things = append(f.things, apitypes.NewResource(own, fleetName, "things", name, fleetProps{Status: status}))
	}
	return f
}

func (f *fleet) Spec() def.ProviderSpec          { return fleetSpec() }
func (f *fleet) Configure(func(any) error) error { return nil }
func (f *fleet) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (f *fleet) find(name string) (fleetThing, error) {
	for _, t := range f.things {
		if t.Name == name {
			return t, nil
		}
	}
	return fleetThing{}, problem.New(problem.NotFound, "no thing "+name)
}

func (f *fleet) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(fleetList, func(_ context.Context, in fleetListIn) (apitypes.List[fleetThing], error) {
			if f.onList != nil {
				f.onList()
			}
			var out apitypes.List[fleetThing]
			for _, t := range f.things {
				if len(in.Filter) == 0 || slices.Contains(in.Filter, "status="+t.Properties.Status) {
					out.Items = append(out.Items, t)
				}
			}
			return out, nil
		}),
		provider.Sync(fleetGet, func(_ context.Context, in fleetNameIn) (fleetThing, error) { return f.find(in.Name) }),
		provider.InProcess(fleetRestart, provider.InProcessSpec[fleetNameIn, fleetRestartOut]{
			Admit: func(_ context.Context, in fleetNameIn) error {
				if _, err := f.find(in.Name); err != nil {
					return err
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				f.restarts = append(f.restarts, in.Name)
				return nil
			},
			Run: func(_ context.Context, in fleetNameIn, _ *provider.Run) (fleetRestartOut, error) {
				return fleetRestartOut{ActionResult: apitypes.ActionResult{Changed: true}, Name: in.Name}, nil
			},
		}),
	}
}

func (f *fleet) admitted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.restarts)
}

// fleetServer is a real nexus server, core.New and server.New over the
// fleet provider, with its own token store, audit log and operations.
type fleetServer struct {
	url   string
	token string
	f     *fleet
}

// startFleet serves f under its own host name; disabled leaves the fleet
// provider off, so the host lacks the capability.
func startFleet(t *testing.T, f *fleet, disabled bool) *fleetServer {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: f.own, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: filepath.Join(stateDir, "ops"), Host: f.own, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := core.New([]provider.Provider{f}, core.Options{Host: f.own, Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{fleetName: {Enabled: !disabled}}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli", Scopes: []string{fleetReadScope, fleetWriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(reg, server.Options{Logger: log,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 1000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	return &fleetServer{url: "http://" + ln.Addr().String(), token: tok, f: f}
}

// clientHost is one configured host of a test's client configuration.
type clientHost struct {
	name, url, token string
	tags             []string
}

// writeClientConfig writes the client configuration under a fresh HOME and
// returns that HOME.
func writeClientConfig(t *testing.T, defaultHost string, hosts ...clientHost) string {
	t.Helper()
	home := t.TempDir()
	var b strings.Builder
	if defaultHost != "" {
		fmt.Fprintf(&b, "defaultHost: %s\n", defaultHost)
	}
	b.WriteString("hosts:\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "  %s:\n    url: %s\n    token: %s\n", h.name, h.url, h.token)
		if len(h.tags) > 0 {
			fmt.Fprintf(&b, "    tags: [%s]\n", strings.Join(h.tags, ", "))
		}
	}
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func homeEnv(home string) Env {
	return Env{Getenv: func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}}
}

// closedURL is a loopback URL nothing listens on: an unreachable host.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}

// errorsByHost indexes the JSON error lines on stderr by host.
func errorsByHost(t *testing.T, stderr string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, e := range jsonLines(t, "stderr", stderr) {
		h, _ := e["host"].(string)
		if _, dup := out[h]; dup {
			t.Errorf("two errors for host %q: %s", h, stderr)
		}
		out[h] = e
	}
	return out
}

// TC-168 at T0, against real servers: results labeled by client-side
// name, capability-unavailable and a connection failure each for their
// host only, exit 6, for --host a,b,c, --tag and --host all; the default
// host alone succeeds. TC-168 itself runs real nexusd at T1.
func TestFanOutReportsEachHost(t *testing.T) {
	a := startFleet(t, newFleet("srv-a", "a1=x", "a2=y"), false)
	b := startFleet(t, newFleet("srv-b", "b1=x"), true)
	home := writeClientConfig(t, "alpha",
		clientHost{"alpha", a.url, a.token, []string{"web"}},
		clientHost{"beta", b.url, b.token, []string{"web"}},
		clientHost{"gamma", closedURL(t), "tok", []string{"web"}},
		clientHost{"delta", a.url, a.token, nil}) // Not tagged web
	env := homeEnv(home)
	specs := fleetSpecs()

	for _, sel := range [][]string{{"--host", "alpha,beta,gamma"}, {"--tag", "web"}, {"--host", "all"}} {
		r := runSpecsCLI(env, specs, append([]string{"thing", "get", "-o", "jsonl"}, sel...)...)
		if r.code != ExitPartial {
			t.Errorf("%v: exit %d, want %d: %s", sel, r.code, ExitPartial, r.stderr)
		}
		hosts := map[string]int{}
		for _, o := range jsonLines(t, "stdout", r.stdout) {
			h, _ := o["host"].(string)
			hosts[h]++
		}
		want := map[string]int{"alpha": 2}
		if sel[1] == "all" {
			want["delta"] = 2
		}
		if fmt.Sprint(hosts) != fmt.Sprint(want) {
			t.Errorf("%v: objects per host %v, want %v (labeled by client-side name, not srv-a)", sel, hosts, want)
		}
		errs := errorsByHost(t, r.stderr)
		if len(errs) != 2 {
			t.Errorf("%v: errors %v, want one for beta and one for gamma", sel, errs)
		}
		if e := errs["beta"]; e["type"] != "urn:nexus:problem:capability-unavailable" || e["exit"] != float64(ExitNotFound) {
			t.Errorf("%v: beta error %v, want capability-unavailable, exit 4", sel, e)
		}
		if e := errs["gamma"]; e["type"] != "urn:nexus:cli:connection" || e["exit"] != float64(ExitConnection) {
			t.Errorf("%v: gamma error %v, want a connection failure, exit 5", sel, e)
		}
	}

	r := runSpecsCLI(env, specs, "thing", "get", "-o", "jsonl")
	if objs := jsonLines(t, "default", r.stdout); r.code != 0 || len(objs) != 2 || objs[0]["host"] != "alpha" {
		t.Errorf("default host: exit %d, objects %v: %s", r.code, objs, r.stderr)
	}

	// Text mode: one message per failed host and a summary.
	r = runSpecsCLI(env, specs, "thing", "get", "--host", "alpha,beta", "-o", "table")
	if r.code != ExitPartial || !strings.Contains(r.stderr, "host beta: capability-unavailable") ||
		!strings.Contains(r.stderr, "1 of 2 hosts failed") || !strings.Contains(r.stdout, "alpha") {
		t.Errorf("table: exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	// All hosts failing the same way exit with that code.
	r = runSpecsCLI(env, specs, "thing", "get", "--host", "beta,gamma", "-o", "jsonl")
	if r.code != ExitPartial {
		t.Errorf("beta,gamma: exit %d, want 6 for different failures", r.code)
	}
	r = runSpecsCLI(env, specs, "thing", "get", "--host", "beta,beta", "-o", "jsonl")
	if r.code != ExitNotFound {
		t.Errorf("beta,beta: exit %d, want 4: one host, its own code", r.code)
	}
}

// Every named host must be configured, and selectors must agree, before
// anything is sent.
func TestFanOutSelectionErrorsSendNothing(t *testing.T) {
	srv := startCaptureServer(t)
	home := writeClientConfig(t, "", clientHost{"alpha", srv.URL, "t", []string{"web"}}, clientHost{"beta", srv.URL, "t", nil})
	env := homeEnv(home)
	for _, args := range [][]string{
		{"--host", "alpha,zz"},
		{"--host", "alpha,"},
		{"--host", "all,alpha"},
		{"--tag", "nope"},
		{"--host", "alpha,beta", "--tag", "web"},
	} {
		r := runSpecsCLI(env, fleetSpecs(), append([]string{"thing", "get"}, args...)...)
		if r.code != ExitUsage {
			t.Errorf("%v: exit %d, want 2: %s", args, r.code, r.stderr)
		}
	}
	tokenEnv := env
	tokenEnv.Getenv = func(k string) string {
		if k == "NEXUS_TOKEN" {
			return "one-token"
		}
		return env.Getenv(k)
	}
	if r := runSpecsCLI(tokenEnv, fleetSpecs(), "thing", "get", "--host", "all"); r.code != ExitUsage ||
		!strings.Contains(r.stderr, "NEXUS_TOKEN") {
		t.Errorf("NEXUS_TOKEN with two hosts: exit %d: %s", r.code, r.stderr)
	}
	if reqs := srv.take(); len(reqs) != 0 {
		t.Errorf("%d requests sent, want none", len(reqs))
	}
}

// TC-169: at most 8 hosts in flight, and exactly 8 at some point; all 20
// reported.
func TestFanOutConcurrencyLimit(t *testing.T) {
	var inflight, peak atomic.Int32
	full := make(chan struct{})
	var once sync.Once
	track := func() {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if n >= maxConcurrentHosts {
			once.Do(func() { close(full) })
		}
		// Hold until 8 are in flight, then a little longer, so a higher
		// limit would show as a higher peak.
		select {
		case <-full:
		case <-time.After(3 * time.Second):
		}
		time.Sleep(50 * time.Millisecond)
	}
	var hosts []clientHost
	for i := range 20 {
		f := newFleet(fmt.Sprintf("srv-%02d", i), fmt.Sprintf("t%02d=x", i))
		f.onList = track
		s := startFleet(t, f, false)
		hosts = append(hosts, clientHost{fmt.Sprintf("h%02d", i), s.url, s.token, nil})
	}
	env := homeEnv(writeClientConfig(t, "", hosts...))

	r := runSpecsCLI(env, fleetSpecs(), "thing", "get", "--host", "all", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if got := peak.Load(); got != maxConcurrentHosts {
		t.Errorf("peak in flight %d, want exactly %d", got, maxConcurrentHosts)
	}
	seen := map[string]bool{}
	for _, o := range jsonLines(t, "stdout", r.stdout) {
		h, _ := o["host"].(string)
		seen[h] = true
	}
	if len(seen) != 20 {
		t.Errorf("%d hosts reported, want 20: %v", len(seen), seen)
	}
}

// TC-ERR64: with hosts configured and no default, a mutation and a read
// both exit 2 with no request; the message says no host was selected and,
// on the read, names the configured hosts.
func TestNoTargetNoDefault(t *testing.T) {
	srv := startCaptureServer(t)
	env := homeEnv(writeClientConfig(t, "", clientHost{"alpha", srv.URL, "t", nil}, clientHost{"beta", srv.URL, "t", nil}))
	for _, args := range [][]string{{"thing", "restart", "t1", "--yes"}, {"thing", "get"}} {
		for _, format := range []string{"jsonl", "table"} {
			r := runSpecsCLI(env, fleetSpecs(), append(args, "-o", format)...)
			if r.code != ExitUsage {
				t.Errorf("%v -o %s: exit %d, want 2: %s", args, format, r.code, r.stderr)
			}
			if !strings.Contains(r.stderr, "no host selected") {
				t.Errorf("%v -o %s: stderr %q does not say no host was selected", args, format, r.stderr)
			}
			if args[1] == "get" && !strings.Contains(r.stderr, "configured hosts: alpha, beta") {
				t.Errorf("%v -o %s: stderr %q does not list the configured hosts", args, format, r.stderr)
			}
		}
	}
	if reqs := srv.take(); len(reqs) != 0 {
		t.Errorf("%d requests sent, want none", len(reqs))
	}
}

// TC-E29: the output's host is the client-side name, not the server's own,
// and --stdin binds that name back to the same server.
func TestClientNameOverridesServerHost(t *testing.T) {
	s := startFleet(t, newFleet("server-own-name", "w1=x"), false)
	other := startFleet(t, newFleet("alpha", "w1=x"), false) // Named like the client's name for s
	env := homeEnv(writeClientConfig(t, "", clientHost{"alpha", s.url, s.token, nil}, clientHost{"server-own-name", other.url, other.token, nil}))

	r := runSpecsCLI(env, fleetSpecs(), "thing", "get", "--host", "alpha", "-o", "jsonl")
	objs := jsonLines(t, "read", r.stdout)
	if r.code != 0 || len(objs) != 1 || objs[0]["host"] != "alpha" {
		t.Fatalf("read: exit %d, objects %v, want host alpha: %s", r.code, objs, r.stderr)
	}

	env.Stdin = strings.NewReader(r.stdout)
	r = runSpecsCLI(env, fleetSpecs(), "thing", "restart", "--stdin", "--yes", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("restart --stdin: exit %d: %s", r.code, r.stderr)
	}
	if got := s.f.admitted(); !slices.Equal(got, []string{"w1"}) {
		t.Errorf("server alpha admitted %v, want [w1]", got)
	}
	if got := other.f.admitted(); len(got) != 0 {
		t.Errorf("the server named alpha admitted %v, want nothing", got)
	}
	if objs := jsonLines(t, "restart", r.stdout); len(objs) != 1 || objs[0]["host"] != "alpha" {
		t.Errorf("restart output %v, want one operation labeled alpha", objs)
	}
}
