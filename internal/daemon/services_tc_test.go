// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-21, TC-SV-117
// (its builtinProviders and providerConfigs half; the injection half is in
// internal/providers/services), TC-SV-120 and TC-SV-127. Owner WIs: 1.3,
// 5.1.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services"
	servicesspec "github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// TC-SV-21: daemon.New injects the manager into every OperationsUser before serving. Owner WI 1.3. Requirements: FR-10, CON-4.
//
// - Setup: A fake provider registered under a real `providerConfigs` key (the `reloadProvider` pattern) that implements `provider.OperationsUser`, recording each `UseOperations` call and the time.
// - Steps:
//  1. `startInProcess`.
//  2. Start an operation of another provider.
//  3. Call the injected `Operations.Active()` from the fake.
//
// - Expected:
//   - `UseOperations` was called exactly once, before the listener accepted its first request;
//   - the injected value is the daemon's `*ops.Manager` (the same pointer used for operations);
//   - `Active()` returns the operation started in step 2.
func TestTCSV21_DaemonNewInjectsTheManagerIntoEvery(t *testing.T) {
	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "ops-user-test", Scopes: []string{"scripts:run", "scripts:read"}})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "apt", "scripts")
	stop := filepath.Join(t.TempDir(), "stop")
	user := &opsUserProvider{reloadProvider: &reloadProvider{}}
	r := startInProcessIn(t, cfgPath, stateDir, user, sleepJobProvider{stop: stop})

	user.mu.Lock()
	calls, at := append([]provider.Operations(nil), user.calls...), user.at
	user.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("UseOperations called %d times, want once", len(calls))
	}
	if mgr, ok := calls[0].(*ops.Manager); !ok || mgr != r.d.ops {
		t.Fatalf("injected %T %p, want the daemon's *ops.Manager %p", calls[0], calls[0], r.d.ops)
	}

	firstRequest := time.Now()
	if !at.Before(firstRequest) {
		t.Fatalf("UseOperations at %v, not before the first request at %v", at, firstRequest)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+listen+"/v1/providers/scripts/sleepjobs/j1/run", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %d %s (%v)", resp.StatusCode, b, err)
	}
	// Stop the job, and wait for its end, before the state directory goes.
	t.Cleanup(func() {
		if err := os.WriteFile(stop, nil, 0o600); err != nil {
			t.Error(err)
		}
		eventually(t, 15*time.Second, "the job ends", func() bool {
			active, err := calls[0].Active()
			return err == nil && len(active) == 0
		})
	})

	active, err := calls[0].Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Op != sleepJob.Op().ID || !strings.Contains(string(b), active[0].ID) ||
		(active[0].Status != apitypes.StatusQueued && active[0].Status != apitypes.StatusRunning) {
		t.Fatalf("Active() = %+v, want the started operation (%s)", active, b)
	}
}

// opsUserProvider is reloadProvider (the apt section) that also records
// each UseOperations call and when it came.
type opsUserProvider struct {
	*reloadProvider
	mu    sync.Mutex
	calls []provider.Operations
	at    time.Time
}

func (p *opsUserProvider) UseOperations(o provider.Operations) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, o)
	p.at = time.Now()
}

var sleepJob = def.Define[probeIn, jobOut](def.Op{
	ID: "scripts.sleepjobs.run", Provider: "scripts", Kind: def.KindResourceAction, ResourceType: "sleepjobs",
	Action: "run", Summary: "s",
	Description: "Detached job for the daemon tests that runs until told to stop. Side effects: none; long-running.",
	Scope:       def.Scope{Name: "scripts:run"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	Refine: daemonJob.Op().Refine,
	CLI:    def.CLIBinding{Noun: "sleepjob", Verb: "start", Positional: "name"},
})

// sleepJobProvider runs sleepJob as a detached operation until stop exists.
type sleepJobProvider struct{ stop string }

func (sleepJobProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "scripts", ReadScope: "scripts:read", Ops: []def.Op{sleepJob.Op()}}
}

func (sleepJobProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (sleepJobProvider) Preflight(context.Context) provider.Status { return ok() }

func (p sleepJobProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Detached(sleepJob, provider.DetachedSpec[probeIn, jobOut]{
		Prepare: func(context.Context, probeIn, *provider.Run) (detach.Spec, error) {
			return detach.Spec{Argv: []string{"/bin/sh", "-c", "while [ ! -e " + p.stop + " ]; do sleep 0.05; done"},
				Env: []string{"PATH=/usr/bin:/bin"}, KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}, nil
		},
		Finalize: func(_ context.Context, _ probeIn, x detach.Exit, _ *provider.Run) (jobOut, error) {
			return jobOut{ExitCode: x.ExitCode}, nil
		},
	})}
}

// TC-SV-120: A SIGHUP reload applies a changed services section without a restart. Owner WI 5.1. Requirements: FR-1, FR-8.
//
// - Setup: `startInProcess` with `providers.services: {enabled: true, control: []}` and the provider's `Run` seam set to an F-HOSTS of `t1-ubuntu-systemd` (through the daemon's test hook for provider options).
// - Steps:
//  1. Read `controlAllowList` from capabilities.
//  2. Rewrite the file with `control: [{name: nx-svc-test}]` and send SIGHUP.
//  3. Read capabilities again, and the PID.
//
// - Expected: Before: `[]`. After: `[{name: nx-svc-test, status: ok, …}]`. The same daemon instance, with no restart.
func TestTCSV120_ASIGHUPReloadAppliesAChangedServices(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	fh := svcctltest.NewHost(t, capture)
	answerLoadState(t, fh, capture, "nx-svc-test")
	root := servicesTestRoot(t, capture)
	plat := platform.Info{Init: fh.Init(), OS: platform.OS{ID: "linux"}}
	p := services.New(services.Options{Host: "daemon-test", Logger: slog.New(slog.DiscardHandler), GOOS: "linux",
		Platform: &plat, Run: fh.Run, RunLines: fh.RunLines, ProcRoot: filepath.Join(root, "proc"),
		RunRoot: filepath.Join(root, "run"), EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root)})

	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	write := func(control string) {
		t.Helper()
		body := fmt.Sprintf("host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
			"  services: {enabled: true, control: %s}\n", listen, control)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[]")
	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "tc-sv-120", Scopes: []string{servicesspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	r := startInProcessIn(t, cfgPath, stateDir, p)
	d, started := r.d, r.d.StartedAt()

	allowList := func() []map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+listen+"/v1/capabilities?refresh=true", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var caps apitypes.Capabilities
		if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(caps.Providers, func(c apitypes.ProviderCapability) bool { return c.Name == servicesspec.Name })
		if i < 0 || caps.Providers[i].Status != apitypes.ProviderEnabled {
			t.Fatalf("services capability %+v, want enabled", caps.Providers)
		}
		b, err := json.Marshal(caps.Providers[i].Details[services.DetailControlAllowList])
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("controlAllowList %s: %v", b, err)
		}
		return out
	}

	// Step 1.
	if got := allowList(); len(got) != 0 {
		t.Fatalf("controlAllowList before the reload %v, want []", got)
	}

	// Steps 2 and 3.
	write("[{name: nx-svc-test}]")
	n := r.logs.count("configuration reloaded")
	r.signal(t, syscall.SIGHUP)
	eventually(t, 10*time.Second, "the reload", func() bool {
		return r.logs.count("configuration reloaded") > n
	})
	got := allowList()
	if len(got) != 1 || got[0]["name"] != "nx-svc-test" || got[0]["status"] != services.StatusOK {
		t.Fatalf("controlAllowList after the reload %v, want [{name: nx-svc-test, status: ok, ...}]", got)
	}
	if r.d != d || !r.d.StartedAt().Equal(started) {
		t.Error("the daemon was replaced or restarted by the reload")
	}
}

// answerLoadState makes fh answer preflight's `show -p LoadState,Id` over
// name with what the capture's own model shows for the unit (the services
// package's answerLoadStates, for one name).
func answerLoadState(t *testing.T, fh *svcctltest.Host, capture, name string) {
	t.Helper()
	const systemctl = "/usr/bin/systemctl"
	model := svcctltest.NewHost(t, capture)
	res, err := model.Run(context.Background(), []string{systemctl, "show", "--timestamp=unix", "-p",
		"Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath",
		"--", name + ".service"})
	if err != nil {
		t.Fatal(err)
	}
	kv := map[string]string{}
	for line := range strings.Lines(string(res.Output)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			kv[k] = v
		}
	}
	if kv["LoadState"] != "loaded" {
		t.Fatalf("%s is %q in %s, want loaded", name, kv["LoadState"], capture)
	}
	fh.Answer([]string{systemctl, "show", "-p", "LoadState,Id", "--", name + ".service"},
		svcctl.Result{Output: []byte("Id=" + kv["Id"] + "\nLoadState=" + kv["LoadState"] + "\n")}, nil)
}

// servicesTestRoot is F-ROOTS for a systemd capture, as the services
// package's testRoot builds it: the capture's proc, run and etc trees, this
// process's PID where the capture's nexusd was, the tools planted, and
// /.dockerenv for a capture taken in a container.
func servicesTestRoot(t *testing.T, capture string) string {
	t.Helper()
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	prov, err := svcctltest.ReadProvenance(capture)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, sub := range []string{"proc", "run", "etc"} {
		src := filepath.Join(dir, sub)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.CopyFS(filepath.Join(root, sub), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	cg, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	plant := map[string]string{filepath.Join("proc", strconv.Itoa(os.Getpid()), "cgroup"): string(cg),
		"usr/bin/systemctl": "", "usr/bin/journalctl": ""}
	if prov.Keys["source"] == "container" {
		plant[".dockerenv"] = ""
	}
	for name, content := range plant {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TC-SV-127: OpenAPI documents the eight routes with their scopes and problem types. Owner WI 5.1. Requirements: FR-15, NFR-8.
//
// - Setup: The daemon's OpenAPI document (the `openapi_test.go` pattern).
// - Steps:
//  1. Find the eight paths under `/v1/providers/services`.
//
// - Expected:
//   - each path has its method, its security scope, and `x-untrusted` (or the existing untrusted marker);
//   - start, stop, restart and control list 409 (`not-allowed`, `protected-service`, `conflict`) and 404;
//   - the reboot lists 400, 409 (`reboot-unavailable`, `nexusd-not-enabled-at-boot`, `operations-running`, `conflict`) and 422 (`config-invalid`);
//   - logs lists 422 `logs-unavailable`;
//   - start, stop, restart and control accept `Idempotency-Key`.
func TestTCSV127_OpenAPIDocumentsTheEightRoutesWithTheir(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "services")
	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "tc-sv-127", Scopes: []string{servicesspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	// The compiled-in providers: an unavailable provider's ops stay
	// documented, so this holds on any host.
	startInProcessIn(t, cfgPath, stateDir)
	req, err := http.NewRequest(http.MethodGet, "http://"+listen+"/v1/openapi.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	type operation struct {
		OperationID   string `json:"operationId"`
		Description   string `json:"description"`
		RequiredScope struct {
			Scopes []string `json:"scopes"`
		} `json:"x-required-scope"`
		Parameters []struct {
			Name string `json:"name"`
			In   string `json:"in"`
		} `json:"parameters"`
		Responses map[string]json.RawMessage `json:"responses"`
	}
	var doc struct {
		Paths map[string]map[string]operation `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}

	// The document has no per-op problem-type list and no untrusted
	// extension: the transport's problems are listed per status, a
	// provider's own are the default response, and the op's description
	// names each of them. Untrusted is checked on the declaration, which the
	// REST and MCP surfaces label from.
	control := []string{"not-allowed", "protected-service", "conflict", "not-found"}
	cases := []struct {
		op       def.Op
		method   string
		statuses []string
		slugs    []string
		keyed    bool
	}{
		{servicesspec.List.Op(), "get", nil, nil, false},
		{servicesspec.Get.Op(), "get", []string{"404"}, []string{"not-found"}, false},
		{servicesspec.Logs.Op(), "get", []string{"404"}, []string{"logs-unavailable", "not-found"}, false},
		{servicesspec.Start.Op(), "post", []string{"404", "409"}, control, true},
		{servicesspec.Stop.Op(), "post", []string{"404", "409"}, control, true},
		{servicesspec.Restart.Op(), "post", []string{"404", "409"}, control, true},
		{servicesspec.Control.Op(), "post", []string{"404", "409"}, control, true},
		{servicesspec.Reboot.Op(), "post", []string{"400", "409", "422"}, []string{"validation-failed",
			"reboot-unavailable", "nexusd-not-enabled-at-boot", "operations-running", "conflict", "config-invalid"}, true},
	}
	n := 0
	for path := range doc.Paths {
		if strings.HasPrefix(path, "/v1/providers/services/") {
			n += len(doc.Paths[path])
		}
	}
	if n != len(cases) {
		t.Errorf("%d operations under /v1/providers/services, want %d", n, len(cases))
	}
	for _, c := range cases {
		path := "/v1/providers/services/" + strings.TrimPrefix(strings.SplitN(routeOf(t, c.op), " ", 2)[1],
			"/v1/providers/services/")
		o, ok := doc.Paths[path][c.method]
		if !ok {
			t.Errorf("%s: no %s %s in the document", c.op.ID, strings.ToUpper(c.method), path)
			continue
		}
		if o.OperationID != c.op.ID {
			t.Errorf("%s %s: operationId %s, want %s", c.method, path, o.OperationID, c.op.ID)
		}
		if !slices.Equal(o.RequiredScope.Scopes, []string{c.op.Scope.Name}) {
			t.Errorf("%s: x-required-scope %v, want [%s]", c.op.ID, o.RequiredScope.Scopes, c.op.Scope.Name)
		}
		if !c.op.Untrusted {
			t.Errorf("%s: not declared Untrusted", c.op.ID)
		}
		for _, st := range c.statuses {
			if _, ok := o.Responses[st]; !ok {
				t.Errorf("%s: no %s response", c.op.ID, st)
			}
		}
		if _, ok := o.Responses["default"]; !ok {
			t.Errorf("%s: no default problem response", c.op.ID)
		}
		text := o.Description
		for _, r := range o.Responses {
			text += string(r)
		}
		for _, slug := range c.slugs {
			if !strings.Contains(text, slug) {
				t.Errorf("%s: neither the description nor a response names %s", c.op.ID, slug)
			}
		}
		keyed := slices.ContainsFunc(o.Parameters, func(p struct {
			Name string `json:"name"`
			In   string `json:"in"`
		}) bool {
			return p.Name == "Idempotency-Key" && p.In == "header"
		})
		if keyed != c.keyed {
			t.Errorf("%s: Idempotency-Key %v, want %v", c.op.ID, keyed, c.keyed)
		}
	}
}

// routeOf is op's "METHOD /v1/path".
func routeOf(t *testing.T, op def.Op) string {
	t.Helper()
	switch op.ID {
	case servicesspec.List.Op().ID:
		return "GET /v1/providers/services/services"
	case servicesspec.Get.Op().ID:
		return "GET /v1/providers/services/services/{name}"
	case servicesspec.Logs.Op().ID:
		return "GET /v1/providers/services/services/{name}/logs"
	case servicesspec.Reboot.Op().ID:
		return "POST /v1/providers/services/actions/reboot"
	}
	return "POST /v1/providers/services/services/{name}/" + op.Action
}

// TC-SV-117 (WI 5.1; FR-1, CON-4), the composition root's half:
// builtinProviders holds exactly one services provider, which takes the
// daemon's three hooks, and providerConfigs hands it its section. The
// injection itself, through daemon.New, is
// TestTCSV117_BuiltinProvidersAndProviderConfigsIncludeServicesAndThe in
// internal/providers/services.
func TestTCSV117_BuiltinProvidersHoldOneServicesProvider(t *testing.T) {
	var found []provider.Provider
	for _, p := range builtinProviders("h1", slog.New(slog.DiscardHandler)) {
		if p.Spec().Name == servicesspec.Name {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("builtinProviders has %d services providers, want 1", len(found))
	}
	if _, ok := found[0].(*services.Provider); !ok {
		t.Errorf("the services provider is %T, want *services.Provider", found[0])
	}
	if _, ok := found[0].(provider.OpsDirUser); !ok {
		t.Error("the services provider is not an OpsDirUser")
	}
	if _, ok := found[0].(provider.DaemonUser); !ok {
		t.Error("the services provider is not a DaemonUser")
	}
	if _, ok := found[0].(provider.OperationsUser); !ok {
		t.Error("the services provider is not an OperationsUser")
	}

	cfg, err := config.Parse([]byte("host: {name: h1}\nproviders:\n  services: {enabled: true, control: [{name: web}]}\n"),
		"nexusd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	row, ok := providerConfigs(cfg)[servicesspec.Name]
	if !ok {
		t.Fatal("providerConfigs has no services row")
	}
	if !row.Enabled || row.Decode == nil {
		t.Fatalf("services row %+v, want enabled with a decoder", row)
	}
	var got config.ServicesConfig
	if err := row.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Control) != 1 || got.Control[0].Name != "web" {
		t.Errorf("the services row decodes %+v, want the section's control list", got)
	}
}
