package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

type jobOut struct {
	ExitCode int `json:"exitCode"`
}

var daemonJob = def.Define[probeIn, jobOut](def.Op{
	ID: "scripts.daemonjobs.run", Provider: "scripts", Kind: def.KindResourceAction, ResourceType: "daemonjobs",
	Action: "run", Summary: "s",
	Description: "Detached job for the daemon tests. Side effects: runs a shell child; long-running.",
	Scope:       def.Scope{Name: "scripts:run"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	Refine: func(s *jsonschema.Schema) {
		if p := s.Properties["name"]; p != nil {
			p.Pattern = `^[a-z0-9-]+$`
		}
	},
	CLI: def.CLIBinding{Noun: "daemonjob", Verb: "start", Positional: "name"},
})

// detachedProvider runs daemonJob as a detached operation.
type detachedProvider struct{}

func (detachedProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "scripts", ReadScope: "scripts:read", Ops: []def.Op{daemonJob.Op()}}
}

func (detachedProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (detachedProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (detachedProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Detached(daemonJob, provider.DetachedSpec[probeIn, jobOut]{
		Prepare: func(_ context.Context, in probeIn, _ *provider.Run) (detach.Spec, error) {
			return detach.Spec{Argv: []string{"/bin/sh", "-c", "echo job " + in.Name + "; exit 4"},
				Env: []string{"PATH=/usr/bin:/bin"}, KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}, nil
		},
		Finalize: func(_ context.Context, _ probeIn, x detach.Exit, _ *provider.Run) (jobOut, error) {
			return jobOut{ExitCode: x.ExitCode}, nil
		},
	})}
}

// daemon.Options.Detach reaches detach.New: a daemon-level test runs a
// detached operation through the HTTP API with the seams of
// detachtest.Local, and the injected launcher, not systemd-run, starts the
// real supervisor in the daemon's ops directory.
func TestDaemonPassesDetachOptionsToTheLauncher(t *testing.T) {
	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "detach-test", Scopes: []string{"scripts:run", "scripts:read"}})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeConfig(t, cfgPath, listen, "scripts")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	local := detachtest.Local()
	var mu sync.Mutex
	var launches []detach.Invocation
	seams := local
	seams.Launch = func(ctx context.Context, inv detach.Invocation) error {
		mu.Lock()
		launches = append(launches, inv)
		mu.Unlock()
		return local.Launch(ctx, inv)
	}
	d, err := New(cfg, Options{
		Providers:  []provider.Provider{detachedProvider{}},
		ConfigPath: cfgPath,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		StateDir:   stateDir,
		AuditDir:   t.TempDir(),
		Detach:     seams,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-runErr; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	select {
	case <-d.Ready():
	case err := <-runErr:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready")
	}

	c := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path, body string) (int, apitypes.Operation) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+listen+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var o apitypes.Operation
		if resp.StatusCode < 300 {
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatalf("%s %s: %s: %v", method, path, b, err)
			}
		} else {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return resp.StatusCode, o
	}
	code, op := do(http.MethodPost, "/v1/providers/scripts/daemonjobs/j1/run", "{}")
	if code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	deadline := time.Now().Add(15 * time.Second)
	for op.Status != apitypes.StatusFailed && op.Status != apitypes.StatusSucceeded {
		if time.Now().After(deadline) {
			t.Fatalf("operation still %s", op.Status)
		}
		time.Sleep(50 * time.Millisecond)
		_, op = do(http.MethodGet, "/v1/operations/"+op.ID, "")
	}
	if op.Status != apitypes.StatusSucceeded || !strings.Contains(string(op.Result), `"exitCode":4`) {
		t.Fatalf("operation %+v, result %s", op, op.Result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(launches) != 1 || launches[0].ID != op.ID ||
		launches[0].Dir != filepath.Join(stateDir, "ops", op.ID) || launches[0].Argv[0] != detach.SystemdRun {
		t.Fatalf("launches %+v", launches)
	}
}
