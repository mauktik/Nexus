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

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/storage"
)

// reportSpy collects the storage accounting reports of a daemon.
type reportSpy struct {
	mu      sync.Mutex
	reports []storage.Report
}

func (s *reportSpy) observe(r storage.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
}

func (s *reportSpy) snapshot() []storage.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storage.Report(nil), s.reports...)
}

// waitFor waits until at least n reports exist and returns them.
func (s *reportSpy) waitFor(t *testing.T, n int, what string) []storage.Report {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r := s.snapshot(); len(r) >= n {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d accounting passes, want %d", what, len(s.snapshot()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TC-E17: storage accounting runs at startup, on each hourly tick and after
// a terminal operation; the default budget is 1 GiB, and the total covers
// state.db, the ops directory and the audit directory. The clock seam
// drives the passes' time.
func TestStorageAccountingTriggersAndDefaults(t *testing.T) {
	if storage.Interval != time.Hour {
		t.Fatalf("accounting interval %s, want 1h", storage.Interval)
	}
	if got := config.Default().Limits.StorageBudgetBytes; got != 1073741824 {
		t.Fatalf("default budget %d, want 1073741824", got)
	}

	stateDir := t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "storage-test", Scopes: []string{"scripts:run", "scripts:read"}})
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

	var clockMu sync.Mutex
	clock := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advance := func(d time.Duration) time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(d)
		return clock
	}
	tick := make(chan time.Time)
	spy := &reportSpy{}
	auditDir := t.TempDir()
	d, err := New(cfg, Options{
		Providers:      []provider.Provider{detachedProvider{}},
		ConfigPath:     cfgPath,
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		StateDir:       stateDir,
		AuditDir:       auditDir,
		Detach:         detachtest.Local(),
		Now:            now,
		StorageTick:    tick,
		StorageObserve: spy.observe,
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

	// Startup: one pass before the listener opened.
	reports := spy.snapshot()
	if len(reports) != 1 {
		t.Fatalf("%d passes at startup, want 1", len(reports))
	}
	start := reports[0]
	if start.Budget != 1073741824 || start.Limited || !start.At.Equal(now()) || start.StateBytes == 0 {
		t.Fatalf("startup report %+v", start)
	}

	// Hourly: one tick, one pass, at the clock's time.
	at := advance(time.Hour)
	tick <- at
	reports = spy.waitFor(t, 2, "after one hour")
	if !reports[1].At.Equal(at) {
		t.Fatalf("hourly pass at %v, want %v", reports[1].At, at)
	}

	// After a terminal operation.
	c := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path, body string) apitypes.Operation {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+listen+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		var o apitypes.Operation
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatalf("%s %s: %s: %v", method, path, b, err)
		}
		return o
	}
	op := do(http.MethodPost, "/v1/providers/scripts/daemonjobs/j1/run", "{}")
	deadline := time.Now().Add(15 * time.Second)
	for !op.Status.Terminal() {
		if time.Now().After(deadline) {
			t.Fatalf("operation still %s", op.Status)
		}
		time.Sleep(20 * time.Millisecond)
		op = do(http.MethodGet, "/v1/operations/"+op.ID, "")
	}
	reports = spy.waitFor(t, 3, "after a terminal operation")
	last := reports[len(reports)-1]
	if last.StateBytes == 0 || last.OpsBytes == 0 || last.AuditBytes == 0 ||
		last.Total != last.StateBytes+last.OpsBytes+last.AuditBytes {
		t.Fatalf("report after the operation %+v: each location must be measured", last)
	}
	ops, err := storage.DirSize(filepath.Join(stateDir, "ops"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := storage.DirSize(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	if ops == 0 || audit == 0 {
		t.Fatalf("fixture: ops %d bytes, audit %d bytes", ops, audit)
	}
}
