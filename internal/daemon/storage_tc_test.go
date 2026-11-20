// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-208, TC-STG-209, TC-STG-210.
// Owner WIs: 5.1 (the storage provider in builtinProviders and
// providerConfigs).

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	hostprov "github.com/mauktik/nexus/internal/providers/host"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	storageprov "github.com/mauktik/nexus/internal/providers/storage"
	storagespec "github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
	"github.com/mauktik/nexus/internal/svcctl"
)

// ---- test support (WI 5.1) ----

var discardLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

// ubuntuFS is the host provider's Ubuntu capture (the host tests' fixture).
var ubuntuFS = filepath.Join("..", "providers", "host", "testdata", "ubuntu-systemd", "fs")

// fixtureHost is the real host provider reading the Ubuntu capture; the
// seams that would reach this machine are fixed, as the host CLI tests'.
func fixtureHost(name string) *hostprov.Provider {
	plat := platform.DetectAt("linux", ubuntuFS)
	plat.Init = platform.Systemd
	return hostprov.New(hostprov.Options{
		Host: name, Logger: discardLog, Platform: &plat, GOOS: "linux", FS: os.DirFS(ubuntuFS),
		Statfs: func(string) (hostprov.Usage, error) {
			return hostprov.Usage{IsDir: true, Blocks: 2621440, Bfree: 1310720, Bavail: 1310720, Files: 655360,
				Ffree: 327680, Frsize: 4096}, nil
		},
		Uname: func() (string, error) { return "aarch64", nil },
		Addrs: func() (map[string][]netip.Prefix, error) { return map[string][]netip.Prefix{}, nil },
		Run: func(context.Context, []string) (svcctl.Result, error) {
			return svcctl.Result{}, errors.New("no commands in this test")
		},
	})
}

// zfsHostFS is a Linux host that is not a Proxmox node, with zpool and zfs
// at the capture's paths and /dev/zfs, as the home-server capture saw it.
func zfsHostFS(f *storagetest.Fake) fstest.MapFS {
	return fstest.MapFS{
		strings.TrimPrefix(f.Paths().Zpool, "/"): {Mode: 0o755},
		strings.TrimPrefix(f.Paths().Zfs, "/"):   {Mode: 0o755},
		"dev/zfs":                                {Mode: fs.ModeDevice | fs.ModeCharDevice | 0o666},
	}
}

// rewrite answers argv with the fake's JSON output changed by fn.
type rewrite struct {
	argv []string
	key  string // The document's container: "pools" or "datasets"
	fn   func(members map[string]any)
}

// fixtureRunner is home-server F-ZFS with the rewrites applied.
func fixtureRunner(t *testing.T, f *storagetest.Fake, rewrites ...rewrite) zfs.Runner {
	t.Helper()
	return func(ctx context.Context, argv []string) (zfs.Result, error) {
		res, err := f.Run(ctx, argv)
		if err != nil {
			return res, err
		}
		for _, rw := range rewrites {
			if !slices.Equal(argv, rw.argv) {
				continue
			}
			doc := decodeNumbers(t, res.Stdout)
			rw.fn(doc[rw.key].(map[string]any))
			out, err := json.Marshal(doc)
			if err != nil {
				t.Error(err)
			}
			res.Stdout = out
		}
		return res, nil
	}
}

func decodeNumbers(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Errorf("decode %.80s: %v", b, err)
	}
	return m
}

func setProp(item map[string]any, prop string, v any) {
	item["properties"].(map[string]any)[prop].(map[string]any)["value"] = v
}

// fixtureStorage is the real storage provider on home-server F-ZFS.
func fixtureStorage(t *testing.T, rewrites ...rewrite) *storageprov.Provider {
	t.Helper()
	f := storagetest.NewFake(t, "home-server")
	return storageprov.New(storageprov.Options{Host: "daemon-test", Logger: discardLog, GOOS: "linux",
		FS: zfsHostFS(f), Run: fixtureRunner(t, f, rewrites...)})
}

// opsDirSpy is the storage provider recording the ops dir the daemon
// injects.
type opsDirSpy struct {
	*storageprov.Provider
	dir atomic.Pointer[string]
}

func (s *opsDirSpy) UseOpsDir(dir string) {
	s.dir.Store(&dir)
	s.Provider.UseOpsDir(dir)
}

// withFixtures is builtinProviders with the named providers replaced by
// fixtures.
func withFixtures(t *testing.T, fixtures ...provider.Provider) []provider.Provider {
	t.Helper()
	ps := builtinProviders("daemon-test", discardLog)
	for _, f := range fixtures {
		i := slices.IndexFunc(ps, func(p provider.Provider) bool { return p.Spec().Name == f.Spec().Name })
		if i < 0 {
			t.Fatalf("builtinProviders has no %s", f.Spec().Name)
		}
		ps[i] = f
	}
	return ps
}

// storageConfig writes a configuration with the host section enabled when
// host, and the storage section body (YAML, indented under storage:).
func storageConfig(t *testing.T, path, listen string, host bool, storage string) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n", listen)
	if host {
		b.WriteString("  host: {enabled: true}\n")
	}
	if storage != "" {
		b.WriteString("  storage:\n")
		for line := range strings.Lines(storage) {
			b.WriteString("    " + line)
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// getJSON answers GET path on the daemon at listen with token, decoding a
// 200 into out.
func getJSON(t *testing.T, listen, path, token string, out any) {
	t.Helper()
	status, body := request(t, http.MethodGet, listen, path, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s: %d %.400s", path, status, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: %v\n%.400s", path, err, body)
	}
}

func request(t *testing.T, method, listen, path, token string, body []byte) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+listen+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 30 * time.Second}
	defer c.CloseIdleConnections()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func mint(t *testing.T, stateDir string, scopes ...string) string {
	t.Helper()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "storage-tc", Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func storageHealth(t *testing.T, listen, token string) storagespec.HealthProperties {
	t.Helper()
	var l apitypes.List[storagespec.Health]
	getJSON(t, listen, "/v1/providers/storage/health", token, &l)
	if len(l.Items) != 1 {
		t.Fatalf("storage.health.get: %d items, want 1", len(l.Items))
	}
	return l.Items[0].Properties
}

func hostSignals(t *testing.T, listen, token string) []apitypes.ProviderSignals {
	t.Helper()
	var l apitypes.List[hostspec.Health]
	getJSON(t, listen, "/v1/providers/host/health", token, &l)
	if len(l.Items) != 1 {
		t.Fatalf("host.health.get: %d items, want 1", len(l.Items))
	}
	return l.Items[0].Properties.ProviderSignals
}

// ---- tests ----

// TC-STG-208: production wiring: the storage provider is built in, injected, and feeds the host's providerSignals. Owner WI 5.1. Requirements: FR-1, FR-16, FR-12, CON-4.
func TestTCSTG208_ProductionWiringTheStorageProviderIsBuilt(t *testing.T) {
	// The production list: storage is built in, with its three interfaces,
	// and has a configuration section.
	var prod provider.Provider
	for _, p := range builtinProviders("daemon-test", discardLog) {
		if p.Spec().Name == storagespec.Name {
			prod = p
		}
	}
	if prod == nil {
		t.Fatal("builtinProviders has no storage provider")
	}
	if _, ok := prod.(*storageprov.Provider); !ok {
		t.Errorf("builtinProviders' storage is a %T, want *storage.Provider", prod)
	}
	_, isOps := prod.(provider.OpsDirUser)
	_, isHealth := prod.(provider.HealthSource)
	_, isBg := prod.(provider.Background)
	if !isOps || !isHealth || !isBg {
		t.Errorf("storage implements OpsDirUser %v, HealthSource %v, Background %v; want all three", isOps, isHealth, isBg)
	}
	def := config.Default()
	if _, ok := providerConfigs(&def)[storagespec.Name]; !ok {
		t.Error("providerConfigs has no storage section")
	}

	sourceTimeout := config.Default().Providers.Host.Settings.SourceTimeout
	check := func(t *testing.T, storage provider.Provider) (storagespec.HealthProperties, apitypes.ProviderSignals) {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
		listen := freePort(t)
		storageConfig(t, cfgPath, listen, true, "enabled: true\n")
		r := startInProcess(t, cfgPath, withFixtures(t, fixtureHost("daemon-test"), storage)...)
		token := mint(t, r.d.StateDir(), hostspec.ReadScope, storagespec.ReadScope)

		// Step 1: the registry.
		if ok, st := r.d.reg.Available(storagespec.Name); !ok {
			t.Fatalf("storage is not available: %+v", st)
		}
		n := 0
		for _, e := range r.d.reg.Entries() {
			if e.Provider == storagespec.Name {
				n++
			}
		}
		if n != len(storagespec.Spec().Ops) {
			t.Errorf("the registry binds %d storage ops, want %d", n, len(storagespec.Spec().Ops))
		}

		start := time.Now()
		blocks := hostSignals(t, listen, token)
		if el := time.Since(start); el > 2*sourceTimeout+time.Second {
			t.Errorf("host.health.get took %s, want within sourceTimeout (%s) plus its own reads", el, sourceTimeout)
		}
		h := storageHealth(t, listen, token)
		if len(blocks) != 1 || blocks[0].Source != storagespec.Name {
			t.Fatalf("providerSignals %+v, want one block from storage", blocks)
		}
		b := blocks[0]
		if b.Counts == nil || *b.Counts != h.Counts {
			t.Errorf("the block's counts %+v, want storage.health.get's %+v", b.Counts, h.Counts)
		}
		return h, b
	}

	t.Run("home-server", func(t *testing.T) {
		spy := &opsDirSpy{Provider: fixtureStorage(t)}
		check(t, spy)
		if d := spy.dir.Load(); d == nil || filepath.Base(*d) != "ops" {
			t.Errorf("the ops dir injected: %v, want <stateDir>/ops", d)
		}
	})

	// Step 2: TC-STG-166's capture: 600 over-quota datasets and z1
	// degraded.
	t.Run("600 over-quota datasets", func(t *testing.T) {
		f := storagetest.NewFake(t, "home-server")
		overQuota := rewrite{argv: f.Argv(zfs.CmdDatasetList), key: "datasets", fn: func(ds map[string]any) {
			tmpl, err := json.Marshal(ds["z2/d1/timemachine"])
			if err != nil {
				t.Error(err)
			}
			clear(ds)
			for i := range 600 {
				item := decodeNumbers(t, tmpl)
				name := fmt.Sprintf("z1/q%04d", i)
				item["name"], item["pool"] = name, "z1"
				setProp(item, "quota", json.Number("1000"))
				setProp(item, "used", json.Number("900"))
				ds[name] = item
			}
		}}
		degraded := rewrite{argv: f.Argv(zfs.CmdPoolList), key: "pools", fn: func(pools map[string]any) {
			setProp(pools["z1"].(map[string]any), "health", "DEGRADED")
		}}
		h, b := check(t, fixtureStorage(t, overQuota, degraded))
		if h.Counts.Warning != 600 || h.Counts.Critical < 1 {
			t.Errorf("storage.health.get counts %+v, want warning 600 and critical at least 1", h.Counts)
		}
		if len(h.Signals) != storagespec.MaxSignals || !h.SignalsTruncated {
			t.Errorf("storage.health.get: %d signals, truncated %v; want 500 and true", len(h.Signals), h.SignalsTruncated)
		}
		if len(b.Signals) != 10 || !b.SignalsTruncated {
			t.Errorf("the block: %d signals, truncated %v; want 10 and true", len(b.Signals), b.SignalsTruncated)
		}
	})
}

// TC-STG-209: storage disabled by default adds no block, no background run and no tool beyond a disabled provider's. Owner WI 5.1. Requirements: NFR-7, FR-1.
func TestTCSTG209_StorageDisabledByDefaultAddsNoBlock(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "deploy", "nexusd.default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The shipped file, served on a free port without TLS, with the host
	// section enabled so host.health.get answers. Storage stays as shipped.
	listen := freePort(t)
	text := string(src)
	for _, r := range [][2]string{
		{`listen: ["127.0.0.1:7443"]`, fmt.Sprintf("listen: [%q]", listen)},
		{"  mode: auto ", "  mode: off "},
		{"  host:\n    enabled: false", "  host:\n    enabled: true"},
	} {
		if strings.Count(text, r[0]) != 1 {
			t.Fatalf("deploy/nexusd.default.yaml has %d of %q, want 1", strings.Count(text, r[0]), r[0])
		}
		text = strings.Replace(text, r[0], r[1], 1)
	}
	if !strings.Contains(text, "  storage:\n    enabled: false") {
		t.Fatal("deploy/nexusd.default.yaml does not ship storage disabled")
	}
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startInProcess(t, cfgPath, withFixtures(t, fixtureHost("daemon-test"))...)
	token := mint(t, r.d.StateDir(), hostspec.ReadScope, storagespec.ReadScope, "containers:read")

	if blocks := hostSignals(t, listen, token); len(blocks) != 0 {
		t.Errorf("providerSignals %+v, want []", blocks)
	}
	var caps apitypes.Capabilities
	getJSON(t, listen, "/v1/capabilities", token, &caps)
	state := map[string]apitypes.ProviderState{}
	for _, p := range caps.Providers {
		state[p.Name] = p.Status
	}
	if state[storagespec.Name] != apitypes.ProviderDisabled || state["docker"] != apitypes.ProviderDisabled {
		t.Errorf("capabilities: storage %q, docker %q; want both disabled", state[storagespec.Name], state["docker"])
	}
	// The control: the enabled host's background start is logged.
	eventually(t, 5*time.Second, "the host's background start", func() bool {
		return slices.ContainsFunc(logRecords(r.logs), func(rec map[string]any) bool {
			return rec["msg"] == "starting background provider" && rec["provider"] == hostspec.Name
		})
	})
	for _, rec := range logRecords(r.logs) {
		if rec["msg"] == "starting background provider" && rec["provider"] == storagespec.Name {
			t.Errorf("a disabled storage started its background: %v", rec)
		}
	}
	// The MCP tool list shows no tool of storage, as of every other
	// disabled provider: docker's are absent too.
	m := &mcpDaemon{base: "http://" + listen}
	tools := m.tools(t, token)
	for _, op := range storagespec.Spec().Ops {
		if op.MCP != nil && slices.Contains(tools, op.MCP.Tool) {
			t.Errorf("the disabled storage provider lists %s", op.MCP.Tool)
		}
	}
	if slices.Contains(tools, "list_containers") || !slices.Contains(tools, "get_host_health") {
		t.Errorf("tools %v: want the enabled host's and no disabled docker's", tools)
	}
}

// gatedDetach is a detach backend whose units stay active, running nothing,
// until release: a detached storage write held Running.
type gatedDetach struct {
	mu       sync.Mutex
	dirs     []string
	released chan struct{}
	once     sync.Once
}

func newGatedDetach() *gatedDetach { return &gatedDetach{released: make(chan struct{})} }

func (g *gatedDetach) release() { g.once.Do(func() { close(g.released) }) }

func (g *gatedDetach) launched() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.dirs)
}

func (g *gatedDetach) options() detach.Options {
	return detach.Options{
		Launch: func(_ context.Context, inv detach.Invocation) error {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.dirs = append(g.dirs, inv.Dir)
			return nil
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			select {
			case <-g.released:
				return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
			default:
				return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
			}
		},
		Signal:       func(context.Context, string, syscall.Signal) error { return nil },
		ListUnits:    func(context.Context) ([]string, error) { return nil, nil },
		PollInterval: 20 * time.Millisecond,
	}
}

// TC-STG-210: a reload applies a changed storage section without a restart; an operation in progress keeps its configuration. Owner WI 5.1. Requirements: FR-1, FR-15.
func TestTCSTG210_AReloadAppliesAChangedStorageSection(t *testing.T) {
	const base = "enabled: true\ncommandTimeout: 30s\nscrub: {enabled: true, pools: [z1]}\n"
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	storageConfig(t, cfgPath, listen, false, base)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	gate := newGatedDetach()
	t.Cleanup(gate.release)
	r := &inProcess{sigs: make(chan os.Signal), runErr: make(chan error, 1), logs: &syncBuffer{}}
	r.d, err = New(cfg, Options{Providers: withFixtures(t, fixtureStorage(t)), ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), StateDir: t.TempDir(), AuditDir: t.TempDir(),
		Detach: gate.options(), Signals: r.sigs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.runErr <- r.d.Run(ctx) }()
	t.Cleanup(func() {
		gate.release()
		cancel()
		select {
		case <-r.runErr:
		case <-time.After(ShutdownTimeout + 5*time.Second):
			t.Error("Run still running after the test")
		}
	})
	select {
	case <-r.d.Ready():
	case err := <-r.runErr:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("not ready within 10s")
	}
	d := r.d
	token := mint(t, d.StateDir(), storagespec.ReadScope, storagespec.ScrubScope)
	capacity := func(h storagespec.HealthProperties) []string {
		var out []string
		for _, s := range h.Signals {
			if s.ID == "pool-capacity" {
				out = append(out, s.Severity+" "+s.Subject)
			}
		}
		slices.Sort(out)
		return out
	}

	// Step 1: state A.
	if got := capacity(storageHealth(t, listen, token)); len(got) != 0 {
		t.Fatalf("state A has pool-capacity signals %v, want none", got)
	}
	var pools apitypes.List[storagespec.Pool]
	getJSON(t, listen, "/v1/providers/storage/pools", token, &pools)
	var want []string
	for _, p := range pools.Items {
		want = append(want, "critical pool:"+p.Name)
	}
	slices.Sort(want)
	if len(want) == 0 {
		t.Fatal("the capture lists no pool")
	}

	// Step 2: a z1 scrub held Running, then the reload.
	status, body := request(t, http.MethodPost, listen, "/v1/providers/storage/pools/z1/scrub", token, []byte(`{}`))
	if status != http.StatusAccepted {
		t.Fatalf("scrub z1: %d %.400s", status, body)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the scrub's unit launched", func() bool { return len(gate.launched()) == 1 })
	storageConfig(t, cfgPath, listen, false, "enabled: true\ncommandTimeout: 60s\nscrub: {enabled: true, pools: [z1]}\n"+
		"thresholds: {poolCapacityWarn: 1, poolCapacityCrit: 2}\n")
	r.signal(t, syscall.SIGHUP)
	eventually(t, 10*time.Second, "health shows pool-capacity (state A')", func() bool {
		return slices.Equal(capacity(storageHealth(t, listen, token)), want)
	})

	// Step 3: the running operation keeps its configuration.
	var running apitypes.Operation
	getJSON(t, listen, "/v1/operations/"+op.ID, token, &running)
	if running.Status.Terminal() {
		t.Errorf("the scrub is %s, want it still running", running.Status)
	}
	b, err := os.ReadFile(filepath.Join(gate.launched()[0], "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		CommandTimeout string `json:"commandTimeout"`
	}
	if err := json.Unmarshal(b, &in); err != nil || in.CommandTimeout != "30s" {
		t.Errorf("input.json commandTimeout %q (%v), want 30s: %s", in.CommandTimeout, err, b)
	}
	storageConfig(t, cfgPath, listen, false, base)
	r.signal(t, syscall.SIGHUP)
	eventually(t, 10*time.Second, "the pool-capacity signals clear", func() bool {
		return len(capacity(storageHealth(t, listen, token))) == 0
	})
	// The same daemon served every step: Run never returned.
	select {
	case err := <-r.runErr:
		r.done = true
		t.Fatalf("the daemon stopped: %v", err)
	default:
	}
	if r.d != d {
		t.Error("the daemon was replaced")
	}
}
