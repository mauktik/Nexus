// Stub-style: SKIP-only (services-provider)
//
// Stubs of planning/services-provider/test-cases.md TC-SV-21, TC-SV-120, TC-SV-127.
// Owner WIs: 1.3, 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/services-provider/test-strategy.md, RED Stub Style).

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/provider"
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
	t.Skip("TC-SV-120: SKIP-only stub; un-skip when WI 5.1 lands")
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
	t.Skip("TC-SV-127: SKIP-only stub; un-skip when WI 5.1 lands")
}
