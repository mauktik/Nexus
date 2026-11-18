// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-88, TC-RA-89 and
// TC-RA-E24 (WI 4.4): the generated nexusdconfig commands against real
// servers running the real nexusd provider, core, ops manager and detach
// launcher, each behind a proxy that counts its requests.
//
// The service helper's system seam and F-SYS/F-RELOAD are unexported test
// code of package nexusd, out of this package's reach, so each host's
// launcher runs a stand-in for the helper's success path: it checks
// admission's digest, writes the backup and config.Edit's output, and
// writes result.json and exit.json. The real helper with F-RELOAD runs
// end to end in internal/providers/nexusd (TC-RA-84); these tests check
// the CLI: confirmation, fan-out, per-host results, warnings and exit codes.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	nexusdprov "github.com/mauktik/nexus/internal/providers/nexusd"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// configHostDaemon is the provider.Daemon of a config-set host: its
// ConfigPath, the real daemon checks, and a reload that does nothing.
type configHostDaemon struct{ configPath, stateDir string }

func (d configHostDaemon) ConfigPath() string   { return d.configPath }
func (d configHostDaemon) StateDir() string     { return d.stateDir }
func (d configHostDaemon) StartedAt() time.Time { return time.Now().Add(-time.Minute) }
func (d configHostDaemon) CheckConfig() provider.ConfigCheck {
	cfg, errs := daemon.CheckConfig(d.configPath)
	return provider.ConfigCheck{Config: cfg, Errors: errs}
}
func (d configHostDaemon) CheckConfigData(b []byte) provider.ConfigCheck {
	cfg, errs := daemon.CheckConfigData(b, d.configPath)
	return provider.ConfigCheck{Config: cfg, Errors: errs}
}
func (d configHostDaemon) Reload(context.Context) (provider.ReloadResult, error) {
	return provider.ReloadResult{}, nil
}

// configHost is a real nexus server over the real nexusd provider with a
// temp configuration file, reached through a proxy that counts POSTs.
type configHost struct {
	url, proxy, token string
	cfgPath           string

	mu       sync.Mutex
	posts    int
	launches int
}

func (h *configHost) postCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posts
}

func (h *configHost) launchCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.launches
}

// startConfigHost serves the real nexusd provider as own over a copy of
// body. systemd is the provider's availability seam. Its "cli" token holds
// nexusd:admin and nexusd:read.
func startConfigHost(t *testing.T, own string, body []byte, systemd func() detach.Availability) *configHost {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	h := &configHost{cfgPath: filepath.Join(t.TempDir(), "nexusd.yaml")}
	if err := os.WriteFile(h.cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	opsDir := filepath.Join(stateDir, "ops")
	launcher, err := detach.New(detach.Config{OpsDir: opsDir, Executable: "/x/nexusd", Logger: log}, detach.Options{
		Launch: func(_ context.Context, inv detach.Invocation) error {
			h.mu.Lock()
			h.launches++
			h.mu.Unlock()
			return standInHelper(inv.Dir)
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
		},
		Signal:       func(context.Context, string, syscall.Signal) error { return nil },
		ListUnits:    func(context.Context) ([]string, error) { return nil, nil },
		PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := nexusdprov.New(nexusdprov.Options{Host: own, Systemd: systemd, InService: func() error { return nil },
		Executable: func() (string, error) { return "/x/nexusd", nil }})
	p.UseDaemon(configHostDaemon{configPath: h.cfgPath, stateDir: stateDir})
	p.UseOpsDir(opsDir)

	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: own, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: opsDir, Host: own, Logger: log, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: own, Version: "v0.0.0-test", Logger: log,
		Audit: alog, Ops: mgr, Providers: map[string]core.ProviderConfig{nexusdspec.Name: {Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	h.token, _, err = auth.Create(stateDir, auth.CreateRequest{Name: "cli",
		Scopes: []string{nexusdspec.AdminScope, nexusdspec.ReadScope}})
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
	h.url = "http://" + ln.Addr().String()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	target, err := url.Parse(h.url)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h.mu.Lock()
			h.posts++
			h.mu.Unlock()
		}
		rp.ServeHTTP(w, r)
	}))
	h.proxy = ps.URL
	t.Cleanup(func() {
		ps.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	return h
}

// standInHelper does in dir what the service helper's config-set mode does
// on success: check the file still has admission's digest, save it as the
// backup, write config.Edit's output, and record result.json and exit.json.
// The restart-required keys are the changed keys' top-level sections, as
// the daemon reports them for the keys these tests set.
func standInHelper(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		return err
	}
	var in struct {
		ConfigPath string   `json:"configPath"`
		BackupPath string   `json:"backupPath"`
		Changes    []string `json:"changes"`
		BaseSHA256 string   `json:"baseSha256"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	data, err := os.ReadFile(in.ConfigPath)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != in.BaseSHA256 {
		return fmt.Errorf("%s changed since admission", in.ConfigPath)
	}
	var as []config.Assignment
	for _, c := range in.Changes {
		a, err := config.ParseAssignment(c)
		if err != nil {
			return err
		}
		as = append(as, a)
	}
	out, chs, err := config.Edit(data, in.ConfigPath, as)
	if err != nil {
		return err
	}
	if err := os.WriteFile(in.BackupPath, data, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(in.ConfigPath, out, 0o600); err != nil {
		return err
	}
	changes := []nexusdspec.ConfigChange{}
	restart := []string{}
	for _, c := range chs {
		changes = append(changes, nexusdspec.ConfigChange{Key: c.Key, Old: c.Old, New: c.New})
		if top, _, _ := strings.Cut(c.Key, "."); top != "providers" && !slices.Contains(restart, top) {
			restart = append(restart, top)
		}
	}
	res, err := json.Marshal(map[string]any{"changed": true, "configChanges": changes, "restartRequired": restart,
		"backupPath": in.BackupPath})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), res, 0o600); err != nil {
		return err
	}
	now := time.Now()
	return detach.WriteExit(dir, detach.Exit{Started: now, Ended: now})
}

// availableSystemd is the availability seam of a systemd host.
func availableSystemd() detach.Availability { return detach.Availability{Available: true} }

// loopbackTLSOff is a valid file on loopback with TLS off.
const loopbackTLSOff = "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: \"off\"\nproviders:\n  nexusd:\n    enabled: true\n"

// tlsLockoutWarning is the lock-out warning of a tls.mode change (ASM-3).
const tlsLockoutWarning = "tls.mode takes effect when nexusd restarts; if it changes how this client connects " +
	"(certificate or pin), you may be locked out until a root login on the host fixes it"

// TC-RA-88: nexusdconfig update fans out with confirmation, prints per-host results and echoes warnings. Owner WI 4.4. Requirements: FR-11, FR-7, ASM-3.
func TestTCRA88_NexusdconfigUpdateFansOutWithConfirmationPrints(t *testing.T) {
	a := startConfigHost(t, "srv-a", []byte(loopbackTLSOff), availableSystemd)
	b := startConfigHost(t, "srv-b", []byte(loopbackTLSOff), availableSystemd)
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil}, clientHost{"b", b.proxy, b.token, nil})

	// Step 1: no terminal and no --yes.
	r := runCLI(tokenEnv(home), "nexusdconfig", "update", "--host", "a,b", "tls.mode=auto")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--yes") {
		t.Errorf("without --yes: exit %d, want %d naming --yes: %s", r.code, ExitUsage, r.stderr)
	}
	if a.postCount() != 0 || b.postCount() != 0 {
		t.Fatalf("without --yes: POSTs a=%d b=%d, want 0", a.postCount(), b.postCount())
	}

	// Step 2.
	r = runCLI(tokenEnv(home), "nexusdconfig", "update", "--host", "a,b", "--yes", "tls.mode=auto")
	if r.code != ExitOK {
		t.Fatalf("with --yes: exit %d: %s", r.code, r.stderr)
	}
	results := map[string]map[string]any{}
	for _, l := range jsonLines(t, "stdout", r.stdout) {
		h, _ := l["host"].(string)
		if _, dup := results[h]; dup {
			t.Fatalf("two results for %q:\n%s", h, r.stdout)
		}
		results[h] = l
	}
	for name, h := range map[string]*configHost{"a": a, "b": b} {
		res, ok := results[name]
		if !ok {
			t.Errorf("no result for %s:\n%s", name, r.stdout)
			continue
		}
		// The CLI waited like reload: the printed result is the
		// operation's, with the helper's outcome.
		out, _ := res["result"].(map[string]any)
		if res["status"] != "Succeeded" || out == nil {
			t.Errorf("%s: %v, want a succeeded operation with its result", name, res)
			continue
		}
		changes, _ := out["changes"].([]any)
		var ch map[string]any
		if len(changes) == 1 {
			ch, _ = changes[0].(map[string]any)
		}
		if out["changed"] != true || ch["key"] != "tls.mode" || ch["old"] != "off" || ch["new"] != "auto" ||
			fmt.Sprint(out["restartRequired"]) != "[tls]" || out["backupPath"] != h.cfgPath+".bak" {
			t.Errorf("%s: result %v, want changed, tls.mode off -> auto, restartRequired [tls] and backupPath %s.bak",
				name, res, h.cfgPath)
		}
		if h.postCount() != 1 || h.launchCount() != 1 {
			t.Errorf("%s: POSTs %d, launches %d; want 1 each", name, h.postCount(), h.launchCount())
		}
		if got := string(readFileBytes(t, h.cfgPath)); !strings.Contains(got, "mode: auto") {
			t.Errorf("%s: the file was not changed:\n%s", name, got)
		}
	}
	warns := warningLines(t, r.stderr, true)
	for _, name := range []string{"a", "b"} {
		if n := slices.Index(warns, name+": "+tlsLockoutWarning); n < 0 ||
			slices.Index(warns[n+1:], name+": "+tlsLockoutWarning) >= 0 {
			t.Errorf("warning lines %q, want the lock-out warning once for %s (stderr %q)", warns, name, r.stderr)
		}
	}
}

// tableCell is the cell under column col of row, read at the header's
// offset: a table's blank cells make strings.Fields unusable.
func tableCell(t *testing.T, header, row, col string) string {
	t.Helper()
	i := strings.Index(header, col)
	if i < 0 {
		t.Fatalf("header %q has no column %s", header, col)
	}
	if i >= len(row) {
		return ""
	}
	return strings.Fields(row[i:] + " ")[0]
}

// A no-change nexusdconfig update answers 200 with its result (D-7). In a
// table it prints the result's own columns, with changed false, instead of
// a waited operation's mostly blank ones (user decision at
// confirm-before-merge, 2026-11-17); JSON is unchanged. Over two hosts, one
// changed and one not, each table holds its own host.
func TestTCRA114_ANoChangeConfigUpdatePrintsTheResultColumnsInATable(t *testing.T) {
	a := startConfigHost(t, "srv-a", []byte(loopbackTLSOff), availableSystemd)
	b := startConfigHost(t, "srv-b", []byte(strings.Replace(loopbackTLSOff, `mode: "off"`, "mode: auto", 1)),
		availableSystemd)
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil}, clientHost{"b", b.proxy, b.token, nil})

	// One host, no change.
	r := runCLI(tokenEnv(home), "nexusdconfig", "update", "--yes", "-o", "table", "tls.mode=off")
	if r.code != ExitOK {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout %q, want a header and one row", r.stdout)
	}
	if h := strings.Fields(lines[0]); !slices.Equal(h, []string{"HOST", "CHANGED", "BACKUPPATH", "RESTORED"}) {
		t.Errorf("header %q, want the result's columns HOST CHANGED BACKUPPATH RESTORED", h)
	}
	if got := tableCell(t, lines[0], lines[1], "CHANGED"); got != "false" {
		t.Errorf("CHANGED %q, want false:\n%s", got, r.stdout)
	}
	if got := tableCell(t, lines[0], lines[1], "HOST"); got != "a" {
		t.Errorf("HOST %q, want a:\n%s", got, r.stdout)
	}
	if a.launchCount() != 0 {
		t.Errorf("launches %d, want 0", a.launchCount())
	}

	// JSON is unchanged: the result object.
	r = runCLI(tokenEnv(home), "nexusdconfig", "update", "--yes", "-o", "json", "tls.mode=off")
	var printed []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &printed); r.code != ExitOK || err != nil || len(printed) != 1 ||
		printed[0]["changed"] != false || printed[0]["host"] != "a" || printed[0]["status"] != nil {
		t.Errorf("json: exit %d, stdout %q (%v), want the result object alone", r.code, r.stdout, err)
	}

	// Two hosts: b changes (an operation, waited), a does not (its result).
	r = runCLI(tokenEnv(home), "nexusdconfig", "update", "--host", "a,b", "--yes", "-o", "table", "tls.mode=off")
	if r.code != ExitOK {
		t.Fatalf("two hosts: exit %d: %s", r.code, r.stderr)
	}
	tables := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n\n")
	if len(tables) != 2 {
		t.Fatalf("two hosts: stdout %q, want two tables", r.stdout)
	}
	op := strings.Split(tables[0], "\n")
	res := strings.Split(tables[1], "\n")
	if len(op) != 2 || !slices.Contains(strings.Fields(op[0]), "STATUS") ||
		tableCell(t, op[0], op[1], "HOST") != "b" || tableCell(t, op[0], op[1], "STATUS") != "Succeeded" {
		t.Errorf("two hosts: operation table %q, want b's succeeded operation", tables[0])
	}
	if len(res) != 2 || tableCell(t, res[0], res[1], "HOST") != "a" || tableCell(t, res[0], res[1], "CHANGED") != "false" {
		t.Errorf("two hosts: result table %q, want a with changed false", tables[1])
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// catalogEnvFixture is the nexusd provider's catalog-env fixture, whose
// script env carries the sentinel.
func catalogEnvFixture(t *testing.T) []byte {
	t.Helper()
	return readFileBytes(t, filepath.Join("..", "providers", "nexusd", "testdata", "config", "catalog-env.yaml"))
}

// TC-RA-89: nexusdconfig get and get KEY print tables. Owner WI 4.4. Requirements: FR-6, FR-11.
func TestTCRA89_NexusdconfigGetAndGetKEYPrintTables(t *testing.T) {
	const sentinel = "RA_ENV_SENTINEL_9f3c"
	body := catalogEnvFixture(t)
	if !strings.Contains(string(body), sentinel) {
		t.Fatal("the catalog-env fixture does not hold the sentinel")
	}
	a := startConfigHost(t, "srv-a", body, availableSystemd)
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil})
	wantHeader := []string{"HOST", "NAME", "VALUE", "EFFECTIVE", "SOURCE"}

	// Step 1.
	r := runCLI(tokenEnv(home), "nexusdconfig", "get", "-o", "table")
	if r.code != ExitOK {
		t.Fatalf("get: exit %d: %s", r.code, r.stderr)
	}
	rows := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if header := strings.Fields(rows[0]); !slices.Equal(header, wantHeader) {
		t.Errorf("get: header %q, want %q", header, wantHeader)
	}
	leaves := config.LeafKeys()
	if len(rows)-1 != len(leaves) {
		t.Errorf("get: %d rows, want one per leaf key (%d)", len(rows)-1, len(leaves))
	}
	for i, k := range leaves {
		if i+1 < len(rows) && strings.Fields(rows[i+1])[1] != k.Path {
			t.Errorf("get: row %d %q, want key %s", i+1, rows[i+1], k.Path)
		}
	}
	catalogRow := slices.IndexFunc(rows, func(row string) bool {
		f := strings.Fields(row)
		return len(f) > 1 && f[1] == "providers.scripts.catalog"
	})
	if catalogRow < 0 || !strings.Contains(rows[catalogRow], config.Redacted) {
		t.Errorf("get: the catalog row does not show %s:\n%s", config.Redacted, r.stdout)
	}

	// Step 2.
	k := runCLI(tokenEnv(home), "nexusdconfig", "get", "providers.host.enabled", "-o", "table")
	if k.code != ExitOK {
		t.Fatalf("get KEY: exit %d: %s", k.code, k.stderr)
	}
	krows := strings.Split(strings.TrimSuffix(k.stdout, "\n"), "\n")
	if header := strings.Fields(krows[0]); !slices.Equal(header, wantHeader) {
		t.Errorf("get KEY: header %q, want %q", header, wantHeader)
	}
	if len(krows) != 2 || strings.Fields(krows[1])[1] != "providers.host.enabled" {
		t.Errorf("get KEY: table %q, want one providers.host.enabled row", k.stdout)
	}

	for _, out := range []string{r.stdout, k.stdout} {
		if n := strings.Count(out, sentinel); n != 0 {
			t.Errorf("stdout holds the sentinel %d times", n)
		}
	}
}

// TC-RA-E24: A multi-host config set with one refused host prints both results and exits non-zero. Owner WI 4.4. Requirements: FR-11, OOS-6.
func TestTCRAE24_AMultiHostConfigSetWithOneRefusedHostPrintsBoth(t *testing.T) {
	const reason = "requires systemd (detected openrc)"
	a := startConfigHost(t, "srv-a", []byte(loopbackTLSOff), availableSystemd)
	b := startConfigHost(t, "srv-b", []byte(loopbackTLSOff), func() detach.Availability {
		return detach.Availability{Reason: reason}
	})
	home := writeClientConfig(t, "a", clientHost{"a", a.proxy, a.token, nil}, clientHost{"b", b.proxy, b.token, nil})

	// Step 1.
	r := runCLI(tokenEnv(home), "nexusdconfig", "update", "--host", "a,b", "--yes", "providers.host.enabled=true")
	if r.code != ExitPartial {
		t.Errorf("exit %d, want the multi-host failure code %d: %s", r.code, ExitPartial, r.stderr)
	}
	lines := jsonLines(t, "stdout", r.stdout)
	if len(lines) != 1 || lines[0]["host"] != "a" {
		t.Fatalf("stdout %q, want a's result alone", r.stdout)
	}
	out, _ := lines[0]["result"].(map[string]any)
	if lines[0]["status"] != "Succeeded" || out["changed"] != true {
		t.Errorf("a: result %v, want changed", lines[0])
	}
	errs := errorsByHost(t, r.stderr)
	e, ok := errs["b"]
	if !ok {
		t.Fatalf("no error for b: %s", r.stderr)
	}
	if typ, _ := e["type"].(string); typ != problem.TypePrefix+string(problem.ConfigSetUnavailable) ||
		e["reason"] != reason {
		t.Errorf("b: error %v, want config-set-unavailable with reason %q", e, reason)
	}
	if _, has := errs["a"]; has {
		t.Errorf("an error for a: %v", errs["a"])
	}

	// No cross-host rollback (OOS-6).
	if got := string(readFileBytes(t, a.cfgPath)); got == loopbackTLSOff ||
		!strings.Contains(got, "host:\n    enabled: true") {
		t.Errorf("a's file was not left changed:\n%s", got)
	}
	if got := string(readFileBytes(t, b.cfgPath)); got != loopbackTLSOff {
		t.Errorf("b's file changed:\n%s", got)
	}
	if b.launchCount() != 0 {
		t.Errorf("b: launches %d, want 0", b.launchCount())
	}
}
