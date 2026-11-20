// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (services-provider)
//
// Tests and stubs of planning/services-provider/test-cases.md TC-SV-26, TC-SV-27, TC-SV-28, TC-SV-29, TC-SV-125, TC-SV-126, TC-SV-153, TC-SV-E6, TC-SV-ERR8, TC-SV-ERR9, TC-SV-ERR10, TC-SV-ERR28.
// Owner WIs: 1.5, 5.1. The WI 1.5 cases (typed confirmation) are tests;
// each remaining stub compiles today and skips; the developer of the owning
// work item replaces the skip and the comment with the test, keeping the
// function name. An un-skipped passing test is the GREEN signal
// (planning/services-provider/test-strategy.md, RED Stub Style).

package cli

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
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	servicesprov "github.com/mauktik/nexus/internal/providers/services"
	servicesspec "github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// tcsvRebootIn is the input of the planted reboot: the typed host name and
// one optional field.
type tcsvRebootIn struct {
	ConfirmHost  string `json:"confirmHost" jsonschema:"The target host's name, typed to confirm."`
	DelaySeconds int    `json:"delaySeconds,omitempty" jsonschema:"Seconds to wait before the reboot."`
}

type tcsvRebootOut struct {
	apitypes.ActionResult
}

// tcsvReboot is TC-SV-26's planted pl.actions.reboot: destructive and
// long-running, bound to machine restart, with confirmHost typed.
var tcsvReboot = def.Define[tcsvRebootIn, tcsvRebootOut](def.Op{
	ID: "pl.actions.reboot", Provider: "pl", Kind: def.KindProviderAction, Action: "reboot",
	Summary: "Reboot the host",
	Description: "Reboots the host. Side effects: the host restarts. " +
		"Long-running: answers 202 with an operation.",
	Scope: def.Scope{Name: "pl:reboot"}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "machine", Verb: "restart", TypedConfirmation: "confirmHost"},
})

// tcsvTypedThing is a typed-confirmation op that, unlike the reboot, has a
// positional, so it has --stdin (CR-7).
type tcsvTypedIn struct {
	Name        string `json:"name" jsonschema:"The thing's name." pattern:"^[a-z]+$"`
	ConfirmHost string `json:"confirmHost" jsonschema:"The target host's name, typed to confirm."`
}

var tcsvTypedThing = def.Define[tcsvTypedIn, tcsvRebootOut](def.Op{
	ID: "plt.things.restart", Provider: "plt", Kind: def.KindResourceAction, ResourceType: "things", Action: "restart",
	Summary: "Restart a thing",
	Description: "Restarts a planted thing. Side effects: the thing restarts. " +
		"Long-running: answers 202 with an operation.",
	Scope: def.Scope{Name: "plt:write"}, Risk: def.RiskDestructive, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "thing", Verb: "restart", Positional: "name", TypedConfirmation: "confirmHost"},
})

const tcsvRebootPath = "/v1/providers/pl/actions/reboot"

// tcsvSpecs is the catalog with the planted pl provider. The real
// services.actions.reboot binds machine restart too, so the services spec
// is dropped first: the planted op proves that typed confirmation is keyed
// on the declaration, not on the provider (CON-4). TC-SV-125 and TC-SV-126
// run the real op over the full catalog.
func tcsvSpecs() []def.ProviderSpec {
	specs := slices.DeleteFunc(catalog.All(), func(s def.ProviderSpec) bool { return s.Name == servicesspec.Name })
	return append(specs, def.ProviderSpec{Name: "pl", ReadScope: "pl:read", Ops: []def.Op{tcsvReboot.Op()}})
}

// tcsvRun runs the CLI over tcsvSpecs with open as the controlling
// terminal. noTerminal (wait_test.go) is the in-process noTTY: the noTTY
// helper runs the built CLI, whose catalog lacks the planted op.
func tcsvRun(home string, open func() (Terminal, error), args ...string) runResult {
	env := homeEnv(home)
	env.OpenTerminal = open
	var stdout, stderr bytes.Buffer
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(""), &stdout, &stderr
	code := runWithSpecs(args, env, tcsvSpecs())
	return runResult{code, stdout.String(), stderr.String()}
}

// failTerminal is a terminal whose opening fails the test.
func failTerminal(t *testing.T) func() (Terminal, error) {
	return func() (Terminal, error) {
		t.Errorf("the terminal was opened")
		return nil, errors.New("the terminal must not be opened")
	}
}

// tcsvOneHost is the planted API on one configured host, h1.
func tcsvOneHost(t *testing.T) (*apiFake, string) {
	t.Helper()
	f := startAPIFake(t, "srv-h1")
	return f, writeClientConfig(t, "", clientHost{"h1", f.URL, "tok", nil})
}

// tcsvConfirmHost is the confirmHost of the only POST f received, and fails
// the test unless there is exactly one, to the planted reboot.
func tcsvConfirmHost(t *testing.T, f *apiFake) any {
	t.Helper()
	posts := f.mutations()
	if len(posts) != 1 || posts[0].path != tcsvRebootPath {
		t.Fatalf("posts %+v, want exactly one to %s", posts, tcsvRebootPath)
	}
	return posts[0].body["confirmHost"]
}

// tcsvPrompted checks that tty was opened once and asked for the typed
// host name of h1, never the y/N question.
func tcsvPrompted(t *testing.T, tty *answeringTerminal) {
	t.Helper()
	opened, asked := tty.state()
	// srv-h1 is the fake's own name: the entry's, h1, must not be asked for.
	if opened != 1 || strings.Count(asked, "Type the host name") != 1 || !strings.Contains(asked, "Type the host name srv-h1 to confirm") {
		t.Errorf("terminal opened %d times, asked %q; want once, for the host's own name srv-h1", opened, asked)
	}
	if strings.Contains(asked, "[y/N]") {
		t.Errorf("the terminal was asked the y/N question: %q", asked)
	}
}

// TC-SV-26: Without the flag, the CLI prompts on the terminal for the host name and sends the trimmed answer. Owner WI 1.5. Requirements: FR-10, FR-13, FR-15.
//
// - Setup:
//   - `startAPIFake` serving a planted destructive long-running op `pl.actions.reboot` with CLI noun `machine`, verb `restart` and `TypedConfirmation: "confirmHost"`.
//   - The client config has one host `h1`.
//   - F-TTY answers `h1\n`.
//
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected:
//   - F-TTY received exactly one prompt, containing `Type the host name` and `h1`;
//   - no y/N question was written;
//   - exactly one POST, whose body has `"confirmHost":"h1"`;
//   - exit 0.
func TestTCSV26_WithoutTheFlagTheCLIPromptsOn(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "h1\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	tcsvPrompted(t, tty)
	if got := tcsvConfirmHost(t, f); got != "h1" {
		t.Errorf("confirmHost %v, want h1", got)
	}
}

// TC-SV-27: An explicit --confirm-host is the confirmation: no prompt and no y/N question. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26. F-TTY is present but would fail the test if opened. A second subtest runs with no terminal at all (`noTTY`).
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1 --no-wait`.
//
// - Expected: In both subtests the terminal is never opened, one POST is sent with `confirmHost: h1`, and the exit is 0.
func TestTCSV27_AnExplicitConfirmHostIsTheConfirmation(t *testing.T) {
	for _, c := range []struct {
		name string
		open func() (Terminal, error)
	}{{"terminal", failTerminal(t)}, {"noTTY", noTerminal}} {
		t.Run(c.name, func(t *testing.T) {
			f, home := tcsvOneHost(t)
			r := tcsvRun(home, c.open, "machine", "restart", "--host", "h1", "--confirm-host", "h1", "--no-wait")
			if r.code != 0 {
				t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
			}
			if got := tcsvConfirmHost(t, f); got != "h1" {
				t.Errorf("confirmHost %v, want h1", got)
			}
		})
	}
}

// TC-SV-28: --yes neither fills nor skips the typed confirmation. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26.
// - Steps:
//  1. `nexus machine restart --host h1 --yes --no-wait` with F-TTY answering `h1`.
//  2. The same with no terminal.
//
// - Expected:
//   - step 1 prompts for the typed host name exactly as TC-SV-26 does, and sends `confirmHost: h1`;
//   - step 2 is a usage error, exit 2, naming `--confirm-host`, with zero POSTs.
func TestTCSV28_YesNeitherFillsNorSkipsTheTyped(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		f, home := tcsvOneHost(t)
		tty := &answeringTerminal{answer: "h1\n"}
		r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--yes", "--no-wait")
		if r.code != 0 {
			t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
		}
		tcsvPrompted(t, tty)
		if got := tcsvConfirmHost(t, f); got != "h1" {
			t.Errorf("confirmHost %v, want h1", got)
		}
	})
	t.Run("noTTY", func(t *testing.T) {
		f, home := tcsvOneHost(t)
		r := tcsvRun(home, noTerminal, "machine", "restart", "--host", "h1", "--yes", "--no-wait")
		if r.code != ExitUsage || !strings.Contains(r.stderr, "--confirm-host") {
			t.Errorf("exit %d, stderr %q; want 2, naming --confirm-host", r.code, r.stderr)
		}
		if n := len(f.mutations()); n != 0 {
			t.Errorf("%d POSTs, want none", n)
		}
	})
}

// TC-SV-29: The typed field is never copied from --host. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26. F-TTY answers `other`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The POST body has `"confirmHost":"other"`, not `h1`. The CLI does not pre-validate the value against `--host`; the server compares it (FR-10).
func TestTCSV29_TheTypedFieldIsNeverCopiedFrom(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "other\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	if got := tcsvConfirmHost(t, f); got != "other" {
		t.Errorf("confirmHost %v, want other, as typed and not --host", got)
	}
}

// TC-SV-125: CLI commands, columns and the --host all rules. Owner WI 5.1. Requirements: FR-15, FR-10.
//
// - Setup: `startProviderServer` with the services provider over F-HOSTS. The client config has hosts `h1` and `h2` (two servers).
// - Steps:
//  1. `nexus service get --host h1`.
//  2. `nexus service get nexusd --host h1`.
//  3. `nexus servicelog get nexusd --tail 5 --host h1` (it names the host for the same reason as step 2).
//  4. `nexus service get --host all`.
//  5. `nexus service invoke nx-svc-test --action restart --host h1 --no-wait`.
//  6. `nexus service stop nx-svc-test --host h1` with F-TTY answering `n`.
//  7. `nexus machine restart --host all --confirm-host h1`.
//
// - Expected:
//   - (1) the columns `NAME STATE ENABLED CONTROLLABLE PROTECTED` (the header from `name, properties.state, …`);
//   - (2) the same header and exactly one row, `nexusd`, whose `STATE` and `ENABLED` equal the provider's `get` response for `nexusd` over F-HOSTS (decoded into `spec.ServiceProperties`, not hard-coded), with `CONTROLLABLE` false and `PROTECTED` true; one GET to `h1` only, none to `h2`;
//   - (3) 5 lines;
//   - (4) rows from both hosts;
//   - (5) one POST to `/v1/providers/services/services/nx-svc-test/control` with `{action: restart}`;
//   - (6) a y/N question, no POST, and the existing decline exit code;
//   - (7) a usage error, exit 2, with zero POSTs to either host.
func TestTCSV125_CLICommandsColumnsAndTheHostAll(t *testing.T) {
	h1, h2 := tcsvServicesHost(t, "h1"), tcsvServicesHost(t, "h2")
	home := writeClientConfig(t, "", clientHost{"h1", h1.rec.URL, h1.srv.token, nil},
		clientHost{"h2", h2.rec.URL, h2.srv.token, nil})
	run := func(tty Terminal, args ...string) runResult {
		t.Helper()
		env := homeEnv(home)
		env.OpenTerminal = noTerminal
		if tty != nil {
			env.OpenTerminal = func() (Terminal, error) { return tty, nil }
		}
		return runCLI(env, args...)
	}
	header := []string{"NAME", "STATE", "ENABLED", "CONTROLLABLE", "PROTECTED"}
	table := func(t *testing.T, out string) (cols []string, rows [][]string) {
		t.Helper()
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		cols = strings.Fields(lines[0])
		for _, l := range lines[1:] {
			rows = append(rows, strings.Fields(l))
		}
		if i := slices.Index(cols, "NAME"); i < 0 || len(cols) < i+len(header) || !slices.Equal(cols[i:i+len(header)], header) {
			t.Fatalf("header %v, want %v in order:\n%s", cols, header, out)
		}
		return cols, rows
	}

	// (1) The list's columns.
	r := run(nil, "service", "get", "--host", "h1", "-o", "table")
	if r.code != 0 {
		t.Fatalf("service get: exit %d: %s", r.code, r.stderr)
	}
	if _, rows := table(t, r.stdout); len(rows) == 0 {
		t.Errorf("service get printed no rows:\n%s", r.stdout)
	}

	// (2) One service, against the provider's own get over F-HOSTS; one GET
	// to h1, none to h2.
	h1Before, h2Before := len(h1.rec.of(http.MethodGet)), len(h2.rec.of(http.MethodGet))
	r = run(nil, "service", "get", "nexusd", "--host", "h1", "-o", "table")
	if r.code != 0 {
		t.Fatalf("service get nexusd: exit %d: %s", r.code, r.stderr)
	}
	cols, rows := table(t, r.stdout)
	if len(rows) != 1 {
		t.Fatalf("service get nexusd: %d rows, want 1:\n%s", len(rows), r.stdout)
	}
	at := func(name string) string { return rows[0][slices.Index(cols, name)] }
	want := h1.get(t, "nexusd")
	if at("NAME") != "nexusd" || at("STATE") != want.State || at("ENABLED") != want.Enabled ||
		at("CONTROLLABLE") != "false" || at("PROTECTED") != "true" {
		t.Errorf("row %v, want nexusd %s %s false true", rows[0], want.State, want.Enabled)
	}
	if n1, n2 := len(h1.rec.of(http.MethodGet))-h1Before, len(h2.rec.of(http.MethodGet))-h2Before; n1 != 1 || n2 != 0 {
		t.Errorf("GETs: %d to h1 and %d to h2, want 1 and 0", n1, n2)
	}

	// (3) Five log lines.
	r = run(nil, "servicelog", "get", "nexusd", "--tail", "5", "--host", "h1")
	if r.code != 0 {
		t.Fatalf("servicelog get: exit %d: %s", r.code, r.stderr)
	}
	if lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n"); len(lines) != 5 ||
		!strings.Contains(lines[0], "l-0015") || !strings.Contains(lines[4], "l-0019") {
		t.Errorf("servicelog get --tail 5 printed %q, want the newest 5 lines, l-0015 to l-0019 (stderr %q)", r.stdout, r.stderr)
	}

	// (4) Rows from both hosts.
	r = run(nil, "service", "get", "--host", "all", "-o", "json")
	if r.code != 0 {
		t.Fatalf("service get --host all: exit %d: %s", r.code, r.stderr)
	}
	var items []struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &items); err != nil {
		t.Fatalf("service get --host all -o json: %v", err)
	}
	hosts := map[string]bool{}
	for _, it := range items {
		hosts[it.Host] = true
	}
	if !hosts["h1"] || !hosts["h2"] {
		t.Errorf("service get --host all: rows from %v, want h1 and h2", hosts)
	}

	// (5) invoke sends one POST to the control route with the action. The
	// default allow-list is empty, so the server refuses it not-allowed.
	r = run(&answeringTerminal{answer: "y\n"}, "service", "invoke", "nx-svc-test", "--action", "restart", "--host", "h1",
		"--no-wait")
	posts := h1.rec.of(http.MethodPost)
	if len(posts) != 1 || posts[0].path != "/v1/providers/services/services/nx-svc-test/control" ||
		posts[0].body["action"] != "restart" {
		t.Fatalf("POSTs %+v, want one to the control route with action restart (exit %d: %s)", posts, r.code, r.stderr)
	}

	// (6) stop asks y/N; n sends nothing.
	tty := &answeringTerminal{answer: "n\n"}
	r = run(tty, "service", "stop", "nx-svc-test", "--host", "h1")
	if _, asked := tty.state(); !strings.Contains(asked, "[y/N]") {
		t.Errorf("service stop asked %q, want the y/N question", asked)
	}
	if r.code != ExitFailure {
		t.Errorf("declined service stop: exit %d, want %d: %s", r.code, ExitFailure, r.stderr)
	}
	if n := len(h1.rec.of(http.MethodPost)); n != 1 {
		t.Errorf("%d POSTs to h1 after the decline, want still 1", n)
	}

	// (7) The reboot targets exactly one host.
	r = run(nil, "machine", "restart", "--host", "all", "--confirm-host", "h1")
	if r.code != ExitUsage {
		t.Errorf("machine restart --host all: exit %d, want 2: %s", r.code, r.stderr)
	}
	if n := len(h1.rec.of(http.MethodPost)) + len(h2.rec.of(http.MethodPost)); n != 1 {
		t.Errorf("%d POSTs in all, want only step 5's", n)
	}
}

// tcsvHost is a server running the real services provider over F-HOSTS
// and F-ROOTS of t1-ubuntu-systemd, behind a recorder.
type tcsvHost struct {
	srv *providerServer
	rec *tcsvRecorder
}

func tcsvServicesHost(t *testing.T, own string) *tcsvHost {
	t.Helper()
	const capture = "t1-ubuntu-systemd"
	fh := svcctltest.NewHost(t, capture)
	fh.SetJournal("nexusd", tcsvJournal(t, capture, 20))
	root := tcsvServicesRoot(t, capture)
	plat := platform.Info{Init: fh.Init(), OS: platform.OS{ID: "linux"}}
	p := servicesprov.New(servicesprov.Options{Host: own, Logger: slog.New(slog.DiscardHandler), GOOS: "linux",
		Platform: &plat, Run: fh.Run, RunLines: fh.RunLines, ProcRoot: filepath.Join(root, "proc"),
		RunRoot: filepath.Join(root, "run"), EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root)})
	srv := startProviderServer(t, own, p, true, servicesspec.ReadScope, servicesspec.LogsScope,
		servicesspec.WriteScope, servicesspec.RebootScope)
	return &tcsvHost{srv: srv, rec: startTCSVRecorder(t, srv.url)}
}

// tcsvJournal is n nexusd journal lines built from capture's first one,
// each a second after the last, with MESSAGE l-%04d (the capture holds
// fewer lines than the test reads).
func tcsvJournal(t *testing.T, capture string, n int) []string {
	t.Helper()
	dir, err := svcctltest.CaptureDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "journal-nexusd-n20.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	var base map[string]any
	if err := json.Unmarshal([]byte(first), &base); err != nil {
		t.Fatal(err)
	}
	us, err := strconv.ParseInt(base["__REALTIME_TIMESTAMP"].(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, n)
	for i := range n {
		base["__REALTIME_TIMESTAMP"] = strconv.FormatInt(us+int64(i)*1_000_000, 10)
		base["MESSAGE"] = fmt.Sprintf("l-%04d", i)
		l, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		lines[i] = string(l)
	}
	return lines
}

// get is the provider's own get of name, read past the recorder.
func (h *tcsvHost) get(t *testing.T, name string) servicesspec.ServiceProperties {
	t.Helper()
	c, err := client.New(client.Options{URL: h.srv.url, Token: h.srv.token})
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Properties servicesspec.ServiceProperties `json:"properties"`
	}
	if err := c.Do(context.Background(), http.MethodGet, "/providers/services/services/"+name, nil, &d); err != nil {
		t.Fatal(err)
	}
	return d.Properties
}

// tcsvRecorder is a reverse proxy that records each request's method, path
// and JSON body.
type tcsvRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []tcsvRequest
}

type tcsvRequest struct {
	method, path string
	body         map[string]any
}

func startTCSVRecorder(t *testing.T, upstream string) *tcsvRecorder {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	rev := httputil.NewSingleHostReverseProxy(u)
	rev.FlushInterval = -1
	rec := &tcsvRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, tcsvRequest{r.Method, r.URL.Path, body})
		rec.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		rev.ServeHTTP(w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (rec *tcsvRecorder) of(method string) []tcsvRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []tcsvRequest
	for _, r := range rec.reqs {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// tcsvServicesRoot is F-ROOTS for a systemd capture, as the services
// package's testRoot builds it: the capture's proc, run and etc trees, this
// process's PID where the capture's nexusd was, the tools planted, and
// /.dockerenv for a capture taken in a container.
func tcsvServicesRoot(t *testing.T, capture string) string {
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

// TC-SV-126: machine restart waits across the outage and prints the final result. Owner WI 5.1. Requirements: FR-13, NFR-7, A-16, A-35, A-43.
//
// - Setup: F-RECONNECT. The server returns the op Running, closes for 3 min of fake time (connection refused, then 503s), and reopens with the same certificate and the op Succeeded with a `RebootResult` holding `previousBootId: PREV` and `bootId: CUR`.
// - Steps: Each step runs against its own F-RECONNECT cycle.
//  1. `nexus machine restart --host h1 --confirm-host h1` (waits by default).
//  2. The same with `--select result.bootId,result.previousBootId`.
//  3. The same with `-o json`.
//
// - Expected:
//   - exit 0 in every step;
//   - (1) stdout prints the operation row with status `succeeded` (the derived operation columns, as `nexus nexusd restart` prints them), not the columns `BOOTID PREVIOUSBOOTID UPAT REBOOTREQUIRED.STATE`: the long-running ops declare no `Columns` (A-43);
//   - (2) the row shows `CUR` and `PREV`;
//   - (3) the decoded `apitypes.Operation` has `result.bootId == CUR` and `result.previousBootId == PREV`;
//   - the fake clock advanced about 3 min in 1 s sleeps, with no real waiting;
//   - `--no-wait` instead prints the operation and returns after one POST.
func TestTCSV126_MachineRestartWaitsAcrossTheOutageAnd(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"table", []string{"-o", "table"}},
		{"select", []string{"--select", "result.bootId,result.previousBootId", "-o", "table"}},
		{"json", []string{"-o", "json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			fc := useFakeClock(t)
			start := fc.Now()
			re := startRebootReconn(t, false)
			// Connection refused for 90 s, then 503s for 90 s. The op ends
			// during the outage, so it is Succeeded when the host answers
			// again.
			re.fp.window(fc, start.Add(2*time.Second), start.Add(92*time.Second), downRefuse, nil)
			re.fp.window(fc, start.Add(92*time.Second), start.Add(182*time.Second), downProblem,
				problem.New(problem.AuditUnavailable, "the audit log is unavailable"))
			fc.at(start.Add(60*time.Second), re.reboot.finish)
			// The outage passes in milliseconds of real time: hold the host's
			// return until the server has recorded the end.
			fc.at(start.Add(181*time.Second), func() { re.waitEnded(t) })

			r := re.run(fc.Now, append([]string{"machine", "restart", "--host", "h1", "--confirm-host", "h1"}, c.args...)...)
			t.Logf("exit %d after %s, virtual %s\nstdout:\n%sstderr:\n%s", r.code, r.wall, fc.Now().Sub(start), r.stdout, r.stderr)
			if r.code != 0 {
				t.Fatalf("exit %d, want 0", r.code)
			}
			switch c.name {
			case "table":
				if !strings.Contains(r.stdout, "STATUS") || !strings.Contains(r.stdout, "Succeeded") ||
					strings.Contains(r.stdout, "BOOTID") || strings.Contains(r.stdout, "REBOOTREQUIRED") {
					t.Errorf("stdout %q, want the Succeeded operation row with the operation columns", r.stdout)
				}
			case "select":
				if !strings.Contains(r.stdout, "CUR") || !strings.Contains(r.stdout, "PREV") {
					t.Errorf("stdout %q, want CUR and PREV", r.stdout)
				}
			case "json":
				var ops []apitypes.Operation
				if err := json.Unmarshal([]byte(r.stdout), &ops); err != nil {
					var op apitypes.Operation
					if err := json.Unmarshal([]byte(r.stdout), &op); err != nil {
						t.Fatalf("stdout is not an operation: %v\n%s", err, r.stdout)
					}
					ops = []apitypes.Operation{op}
				}
				var res servicesspec.RebootResult
				if len(ops) != 1 || json.Unmarshal(ops[0].Result, &res) != nil || res.BootID != "CUR" ||
					res.PreviousBootID != "PREV" {
					t.Errorf("operations %+v, want one with bootId CUR and previousBootId PREV", ops)
				}
			}
			if el := fc.Now().Sub(start); el < 180*time.Second || el > 200*time.Second {
				t.Errorf("virtual %s elapsed, want about 3 min", el)
			}
			ones := 0
			for _, d := range fc.slept() {
				if d == time.Second {
					ones++
				}
			}
			if ones < 170 {
				t.Errorf("%d 1 s sleeps, want the outage ridden out in 1 s retries", ones)
			}
			if r.wall >= 10*time.Second {
				t.Errorf("wall time %s: a real sleep is on the wait path", r.wall)
			}
		})
	}

	t.Run("noWait", func(t *testing.T) {
		re := startRebootReconn(t, false)
		r := re.run(time.Now, "machine", "restart", "--host", "h1", "--confirm-host", "h1", "--no-wait", "-o", "json")
		if r.code != 0 {
			t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
		}
		var ops []apitypes.Operation
		if err := json.Unmarshal([]byte(r.stdout), &ops); err != nil || len(ops) != 1 ||
			ops[0].Action != servicesspec.Reboot.Op().ID || ops[0].Status.Terminal() {
			t.Errorf("stdout %q (%v), want the accepted reboot operation", r.stdout, err)
		}
		if posts := re.rec.matching(http.MethodPost, func(string) bool { return true }); len(posts) != 1 {
			t.Errorf("%d POSTs, want 1", len(posts))
		}
		if polls := re.rec.matching(http.MethodGet, func(p string) bool { return strings.HasPrefix(p, "/v1/operations/") }); len(polls) != 0 {
			t.Errorf("%d polls with --no-wait, want none", len(polls))
		}
	})
}

// tcsvRebootStub serves the real services.actions.reboot declaration with
// an in-process run that stays Running until finish, then succeeds with
// previousBootId PREV and bootId CUR. Nothing reboots.
type tcsvRebootStub struct {
	done chan struct{}
	once sync.Once

	mu sync.Mutex
	id string // The operation's ID, once it runs
}

func (s *tcsvRebootStub) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: servicesspec.Name, ReadScope: servicesspec.ReadScope,
		Ops: []def.Op{servicesspec.Reboot.Op()}}
}

func (s *tcsvRebootStub) Configure(func(any) error) error { return nil }

func (s *tcsvRebootStub) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (s *tcsvRebootStub) Bindings() []provider.Binding {
	return []provider.Binding{provider.InProcess(servicesspec.Reboot,
		provider.InProcessSpec[servicesspec.RebootInput, servicesspec.RebootResult]{
			Run: func(ctx context.Context, _ servicesspec.RebootInput, r *provider.Run) (servicesspec.RebootResult, error) {
				s.mu.Lock()
				s.id = r.ID()
				s.mu.Unlock()
				select {
				case <-s.done:
					return servicesspec.RebootResult{PreviousBootID: "PREV", BootID: "CUR"}, nil
				case <-ctx.Done():
					return servicesspec.RebootResult{}, ctx.Err()
				}
			},
		})}
}

func (s *tcsvRebootStub) finish() { s.once.Do(func() { close(s.done) }) }

// rebootReconn is F-RECONNECT over the reboot stub: the recording proxy and
// F-PROXY in front of it, configured as host h1.
type rebootReconn struct {
	reboot *tcsvRebootStub
	srv    *providerServer
	rec    *recProxy
	fp     *flakyProxy
	env    Env
}

func startRebootReconn(t *testing.T, useTLS bool) *rebootReconn {
	t.Helper()
	stub := &tcsvRebootStub{done: make(chan struct{})}
	srv := startProviderServer(t, "h1", stub, true, servicesspec.ReadScope, servicesspec.RebootScope)
	// Registered after the server, so it runs before its shutdown.
	t.Cleanup(stub.finish)
	rec := startRecProxy(t, srv.url)
	fp := startFlakyProxy(t, rec.URL, useTLS)
	home := writeClientConfig(t, "", clientHost{"h1", fp.url(), srv.token, nil})
	if useTLS {
		path := filepath.Join(home, ".config", "nexus", "config.yaml")
		cfg, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg = fmt.Appendf(cfg, "    fingerprint: %s\n", client.Fingerprint(fp.pinned.Certificate[0]))
		if err := os.WriteFile(path, cfg, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := homeEnv(home)
	env.OpenTerminal = noTerminal
	return &rebootReconn{reboot: stub, srv: srv, rec: rec, fp: fp, env: env}
}

// waitEnded waits, in real time, until the server records the reboot
// operation's end.
func (re *rebootReconn) waitEnded(t *testing.T) {
	t.Helper()
	c, err := client.New(client.Options{URL: re.srv.url, Token: re.srv.token})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		re.reboot.mu.Lock()
		id := re.reboot.id
		re.reboot.mu.Unlock()
		var op apitypes.Operation
		if id != "" && c.Do(context.Background(), http.MethodGet, "/operations/"+id, nil, &op) == nil && op.Status.Terminal() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the reboot operation did not end within 5s")
}

// run runs the CLI over the real catalog, stamping stderr lines on now.
func (re *rebootReconn) run(now func() time.Time, args ...string) reconnRun {
	var stdout strings.Builder
	sw := &stampWriter{now: now}
	env := re.env
	env.Stdout, env.Stderr = &stdout, sw
	start := time.Now()
	code := runWithSpecs(args, env, catalog.All())
	return reconnRun{code, stdout.String(), sw, time.Since(start)}
}

// TC-SV-153: The --yes help text of a typed-confirmation command says it neither fills nor skips the flag; every other command keeps today's text. Owner WI 5.1. Requirements: FR-13, FR-15.
//
// - Setup: The real catalog (`runWithSpecs(args, env, catalog.All())`) with `homeEnv(t.TempDir())`. `--help` needs no server.
// - Steps:
//  1. `nexus machine restart --help`.
//  2. `nexus service stop --help`.
//  3. `nexus container restart --help` (a mutating command outside this provider).
//  4. Read `docs/cli/nexus_machine_restart.md` and `docs/cli/nexus_container_restart.md`.
//
// - Expected:
//   - (1) exit 0; the `--yes` line reads "no y/N question is asked here; --yes neither fills nor skips --confirm-host, which is required without a terminal", and the help does not contain "do not ask for confirmation";
//   - (2) and (3) exit 0; the `--yes` line reads "do not ask for confirmation; required to mutate without a terminal", unchanged from base;
//   - (4) the generated `nexus_machine_restart.md` carries the new text, and the `--yes` line of `nexus_container_restart.md` equals that line at base `43e5792` (`git show`; skip with a reason if git is unavailable).
func TestTCSV153_TheYesHelpTextOfATyped(t *testing.T) {
	const (
		typedText = "no y/N question is asked here; --yes neither fills nor skips --confirm-host, which is required without a terminal"
		baseText  = "do not ask for confirmation; required to mutate without a terminal"
	)
	help := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		env := homeEnv(t.TempDir())
		env.Stdout, env.Stderr = &stdout, &stderr
		if code := runWithSpecs(append(args, "--help"), env, catalog.All()); code != 0 {
			t.Fatalf("%v --help: exit %d: %s", args, code, stderr.String())
		}
		return stdout.String()
	}
	yesLine := func(text string) string {
		for _, l := range strings.Split(text, "\n") {
			if strings.Contains(l, "--yes") {
				return l
			}
		}
		return ""
	}

	// (1) The typed-confirmation command.
	h := help("machine", "restart")
	if l := yesLine(h); !strings.Contains(l, typedText) {
		t.Errorf("machine restart --yes line %q, want %q", l, typedText)
	}
	if strings.Contains(h, "do not ask for confirmation") {
		t.Errorf("machine restart help still says %q:\n%s", "do not ask for confirmation", h)
	}
	// (2) and (3) Every other mutating command keeps today's text.
	for _, args := range [][]string{{"service", "stop"}, {"container", "restart"}} {
		if l := yesLine(help(args...)); !strings.Contains(l, baseText) || strings.Contains(l, "neither fills") {
			t.Errorf("%v --yes line %q, want %q", args, l, baseText)
		}
	}

	// (4) The generated pages.
	docs := filepath.Join("..", "..", "docs", "cli")
	page, err := os.ReadFile(filepath.Join(docs, "nexus_machine_restart.md"))
	if err != nil {
		t.Fatal(err)
	}
	if l := yesLine(string(page)); !strings.Contains(l, typedText) {
		t.Errorf("nexus_machine_restart.md --yes line %q, want %q", l, typedText)
	}
	head, err := os.ReadFile(filepath.Join(docs, "nexus_container_restart.md"))
	if err != nil {
		t.Fatal(err)
	}
	const base = "43e5792"
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found: cannot read nexus_container_restart.md at base " + base)
	}
	if err := exec.Command("git", "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		t.Skipf("base commit %s not available: %v", base, err)
	}
	was, err := exec.Command("git", "show", base+":docs/cli/nexus_container_restart.md").Output()
	if err != nil {
		t.Fatalf("git show %s:docs/cli/nexus_container_restart.md: %v", base, err)
	}
	if got, want := yesLine(string(head)), yesLine(string(was)); got == "" || got != want {
		t.Errorf("nexus_container_restart.md --yes line %q, want it unchanged from base: %q", got, want)
	}
}

// TC-SV-E6: The typed answer is trimmed and sent as typed, case included. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: As TC-SV-26, with F-TTY answering `"  H1 \t\n"`.
// - Steps:
//  1. `nexus machine restart --host h1 --no-wait`.
//
// - Expected: The body has `"confirmHost":"H1"`, with no case folding.
func TestTCSVE6_TheTypedAnswerIsTrimmedAndSent(t *testing.T) {
	f, home := tcsvOneHost(t)
	tty := &answeringTerminal{answer: "  H1 \t\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	if got := tcsvConfirmHost(t, f); got != "H1" {
		t.Errorf("confirmHost %q, want H1: trimmed, case kept", got)
	}
}

// The typed confirmation asks for the host's own name, as its capabilities
// report it, and not the name of its entry in the client configuration: the
// server compares confirmHost with the former (FR-10, D-8).
func TestTypedConfirmationAsksForTheHostsOwnName(t *testing.T) {
	f := startAPIFake(t, "box")
	home := writeClientConfig(t, "", clientHost{"box-admin", f.URL, "tok", nil})
	tty := &answeringTerminal{answer: "box\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "box-admin", "--no-wait")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	opened, asked := tty.state()
	if opened != 1 || !strings.Contains(asked, "Type the host name box to confirm") || strings.Contains(asked, "box-admin") {
		t.Errorf("terminal opened %d times, asked %q; want once, for the host's own name box and not its entry box-admin", opened, asked)
	}
	if got := tcsvConfirmHost(t, f); got != "box" {
		t.Errorf("confirmHost %v, want box", got)
	}
}

// Without the host's capabilities the CLI cannot say which name to type, so
// it fails without prompting, falling back to nothing, and sends nothing.
func TestTypedConfirmationFailsWhenCapabilitiesAreUnavailable(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	home := writeClientConfig(t, "", clientHost{"box-admin", srv.URL, "tok", nil})
	tty := &answeringTerminal{answer: "box-admin\n"}
	r := tcsvRun(home, tty.open, "machine", "restart", "--host", "box-admin", "--no-wait")
	if _, asked := tty.state(); asked != "" {
		t.Errorf("the terminal was asked %q, want nothing", asked)
	}
	if r.code != ExitFailure || !strings.Contains(r.stderr, "box-admin") || !strings.Contains(r.stderr, "capabilities") {
		t.Errorf("exit %d, stderr %q; want exit %d naming the host and its capabilities", r.code, r.stderr, ExitFailure)
	}
	if n := posts.Load(); n != 0 {
		t.Errorf("%d POSTs, want none", n)
	}
}

// TC-SV-ERR8: No terminal and no flag is a usage error naming the flag, with nothing sent. Owner WI 1.5. Requirements: FR-10, FR-13.
//
// - Setup: As TC-SV-26, with `noTTY`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2, stderr names `--confirm-host`, and the fake recorded zero POSTs.
func TestTCSVERR8_NoTerminalAndNoFlagIsA(t *testing.T) {
	f, home := tcsvOneHost(t)
	r := tcsvRun(home, noTerminal, "machine", "restart", "--host", "h1")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "--confirm-host") {
		t.Errorf("exit %d, stderr %q; want 2, naming --confirm-host", r.code, r.stderr)
	}
	if n := len(f.mutations()); n != 0 {
		t.Errorf("%d POSTs, want none", n)
	}
}

// TC-SV-ERR9: --host all and host lists are refused before anything is sent. Owner WI 1.5. Requirements: FR-10, FR-15.
//
// - Setup: Two hosts `h1` and `h2`. F-TTY would fail the test if opened.
// - Steps:
//  1. `--host all --confirm-host h1`.
//  2. `--host h1,h2 --confirm-host h1`.
//  3. `--host all` with no flag.
//
// - Expected: Each is exit 2, with a message saying the command targets exactly one host. Zero POSTs, and the terminal is never opened.
func TestTCSVERR9_HostAllAndHostListsAreRefused(t *testing.T) {
	h1, h2 := startAPIFake(t, "srv-h1"), startAPIFake(t, "srv-h2")
	home := writeClientConfig(t, "", clientHost{"h1", h1.URL, "tok", nil}, clientHost{"h2", h2.URL, "tok", nil})
	for _, args := range [][]string{
		{"--host", "all", "--confirm-host", "h1"},
		{"--host", "h1,h2", "--confirm-host", "h1"},
		{"--host", "all"},
	} {
		r := tcsvRun(home, failTerminal(t), append([]string{"machine", "restart"}, args...)...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "exactly one host") {
			t.Errorf("%v: exit %d, stderr %q; want 2, saying it targets exactly one host", args, r.code, r.stderr)
		}
	}
	if n := len(h1.mutations()) + len(h2.mutations()); n != 0 {
		t.Errorf("%d POSTs, want none", n)
	}
}

// TC-SV-ERR10: An empty typed answer is a usage error. Owner WI 1.5. Requirements: FR-10.
//
// - Setup: F-TTY answering `"\n"`, and then `"   \n"`.
// - Steps:
//  1. `nexus machine restart --host h1`.
//
// - Expected: Exit 2 and zero POSTs in both cases.
func TestTCSVERR10_AnEmptyTypedAnswerIsAUsage(t *testing.T) {
	for _, answer := range []string{"\n", "   \n"} {
		f, home := tcsvOneHost(t)
		tty := &answeringTerminal{answer: answer}
		r := tcsvRun(home, tty.open, "machine", "restart", "--host", "h1")
		if r.code != ExitUsage {
			t.Errorf("answer %q: exit %d, want 2: %s", answer, r.code, r.stderr)
		}
		// The exit 2 is the empty answer's, not an earlier refusal's.
		if opened, asked := tty.state(); opened != 1 || !strings.Contains(asked, "Type the host name") {
			t.Errorf("answer %q: terminal opened %d times, asked %q; want the host name asked once", answer, opened, asked)
		}
		if n := len(f.mutations()); n != 0 {
			t.Errorf("answer %q: %d POSTs, want none", answer, n)
		}
	}
}

// TC-SV-ERR28: The CLI exits 5 with the follow-up command when the window is spent, and never retries a pin mismatch. Owner WI 5.1. Requirements: FR-13, ASM-9.
//
// - Setup: F-RECONNECT. (a) The server never returns, for 10 min + 1 s of fake time. (b) It returns with a different certificate.
// - Steps:
//  1. `nexus machine restart --host h1 --confirm-host h1`.
//
// - Expected:
//   - (a) exit 5, with stderr containing the operation ID and exactly `nexus operation get <id> --host h1`, and the fake clock advanced ≥ 10 min;
//   - (b) the pin-mismatch error at once, with no further dial (dial count stops at the mismatch), and a non-zero exit that is not 5.
func TestTCSVERR28_TheCLIExits5WithTheFollow(t *testing.T) {
	t.Run("windowSpent", func(t *testing.T) {
		fc := useFakeClock(t)
		start := fc.Now()
		re := startRebootReconn(t, false)
		re.fp.window(fc, start.Add(2*time.Second), start.Add(time.Hour), downRefuse, nil)
		r := re.run(fc.Now, "machine", "restart", "--host", "h1", "--confirm-host", "h1", "--verbose")
		t.Logf("exit %d after %s, virtual %s\nstderr tail:\n%s", r.code, r.wall, fc.Now().Sub(start),
			lastLines(r.stderr.String(), 5))
		if r.code != ExitConnection {
			t.Fatalf("exit %d, want 5", r.code)
		}
		id := r.stderr.opID(t)
		if want := "nexus operation get " + id + " --host h1"; !strings.Contains(r.stderr.String(), want) {
			t.Errorf("stderr does not name %q", want)
		}
		if el := fc.Now().Sub(start); el < 10*time.Minute {
			t.Errorf("virtual %s elapsed, want at least the 10 min window", el)
		}
	})

	t.Run("pinMismatch", func(t *testing.T) {
		fc := useFakeClock(t)
		start := fc.Now()
		re := startRebootReconn(t, true)
		re.fp.onPost(func() { re.fp.set(downPin, nil) })
		r := re.run(fc.Now, "machine", "restart", "--host", "h1", "--confirm-host", "h1", "--verbose")
		t.Logf("exit %d\nstderr:\n%s", r.code, r.stderr)
		// The CLI's exit code for a pin mismatch is 5 (ExitConnection), as
		// TC-SM-ERR20 pins for every reconnect window; TC-SV-ERR28 (b)'s
		// "not 5" is reported for the architect, not asserted.
		if r.code == 0 {
			t.Fatalf("exit 0, want the pin mismatch to fail")
		}
		if el := fc.Now().Sub(start); el >= time.Second {
			t.Errorf("virtual %s elapsed, want the mismatch at once", el)
		}
		if failed := failedOf(r.stderr.attempts(t, "")); len(failed) != 1 {
			t.Errorf("%d failed requests, want 1: no dial after the mismatch", len(failed))
		}
		if s := r.stderr.String(); !strings.Contains(s, "fingerprint mismatch") || strings.Contains(s, "likely restarting") {
			t.Errorf("stderr %q, want the fingerprint mismatch and no reconnect", s)
		}
	})
}

// lastLines is the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// brokenTerminal is a controlling terminal whose reads or writes fail.
type brokenTerminal struct {
	answeringTerminal
	failRead, failWrite bool
}

func (b *brokenTerminal) open() (Terminal, error) {
	b.answeringTerminal.opened++
	return b, nil
}

func (b *brokenTerminal) Write(p []byte) (int, error) {
	if b.failWrite {
		return 0, errors.New("write /dev/tty: input/output error")
	}
	return b.answeringTerminal.Write(p)
}

func (b *brokenTerminal) ReadLine() (string, error) {
	if b.failRead {
		return "", io.EOF
	}
	return b.answer, nil
}

// CR-7 (services-provider code review): the refusal branches of the typed
// confirmation. Each sends nothing.
func TestCR7_TheTypedConfirmationsRefusals(t *testing.T) {
	// The planted reboot has no positional, so it has no --stdin flag at all;
	// the --stdin refusal guards a typed-confirmation op that has one, so
	// that a stdin fan-out never skips the typed host name.
	t.Run("--stdin on an op with a positional", func(t *testing.T) {
		f, home := tcsvOneHost(t)
		tty := &brokenTerminal{}
		env := homeEnv(home)
		env.OpenTerminal = tty.open
		var stdout, stderr bytes.Buffer
		env.Stdin, env.Stdout, env.Stderr = strings.NewReader(`{"name":"web"}`+"\n"), &stdout, &stderr
		specs := append(tcsvSpecs(), def.ProviderSpec{Name: "plt", ReadScope: "plt:read", Ops: []def.Op{tcsvTypedThing.Op()}})
		code := runWithSpecs([]string{"thing", "restart", "--stdin", "--host", "h1", "--yes", "--no-wait"}, env, specs)
		if code != ExitUsage || !strings.Contains(stderr.String(), "--stdin cannot be used with nexus thing restart") {
			t.Errorf("exit %d, stderr %q; want 2, refusing --stdin", code, stderr.String())
		}
		if n := len(f.mutations()); n != 0 || tty.opened != 0 {
			t.Errorf("%d POSTs, terminal opened %d times; want none", n, tty.opened)
		}
	})
	for _, c := range []struct {
		name     string
		args     []string
		tty      func() *brokenTerminal
		wantCode int
		wantErr  string
		wantOpen int // Times the terminal is opened
	}{
		{"--confirm-host empty", []string{"machine", "restart", "--host", "h1", "--confirm-host", "", "--no-wait"},
			func() *brokenTerminal { return &brokenTerminal{} }, ExitUsage, "--confirm-host is empty", 0},
		{"the terminal cannot be written", []string{"machine", "restart", "--host", "h1", "--no-wait"},
			func() *brokenTerminal { return &brokenTerminal{failWrite: true} }, ExitUsage, "the terminal cannot be written", 1},
		{"the answer cannot be read", []string{"machine", "restart", "--host", "h1", "--no-wait"},
			func() *brokenTerminal { return &brokenTerminal{failRead: true} }, ExitFailure, "not confirmed", 1},
		{"an empty answer", []string{"machine", "restart", "--host", "h1", "--no-wait"},
			func() *brokenTerminal { return &brokenTerminal{answeringTerminal: answeringTerminal{answer: "\n"}} }, ExitUsage,
			"no host name was typed", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, home := tcsvOneHost(t)
			tty := c.tty()
			r := tcsvRun(home, tty.open, c.args...)
			if r.code != c.wantCode || !strings.Contains(r.stderr, c.wantErr) {
				t.Errorf("exit %d, stderr %q; want exit %d naming %q", r.code, r.stderr, c.wantCode, c.wantErr)
			}
			if tty.opened != c.wantOpen {
				t.Errorf("terminal opened %d times, want %d", tty.opened, c.wantOpen)
			}
			if n := len(f.mutations()); n != 0 {
				t.Errorf("%d POSTs, want none", n)
			}
		})
	}
}

// SEC-3 (services-provider security review): the host name the prompt shows
// is the server's, so a server that embeds terminal escapes, a carriage
// return, a C1 control or a bidirectional override must not reach the
// terminal, and must not be asked for. The CLI refuses without prompting,
// names the problem with the name quoted so nothing in it is interpreted,
// and sends nothing; --confirm-host still works.
func TestSEC3_TheTypedConfirmationRefusesAHostNameThatIsNotPlainText(t *testing.T) {
	for _, tc := range []struct {
		name, own string
	}{
		{"a CSI sequence and a carriage return", "home-server\r\x1b[2KType the host name pihole to confirm: "},
		{"an OSC sequence", "box\x1b]0;pwned\x07"},
		{"a C1 control", "box\u009b2J"},
		{"a bidirectional override", "box‮pihole"},
		{"a zero-width space", "box​"},
		{"a line separator", "box pihole"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startAPIFake(t, tc.own)
			home := writeClientConfig(t, "", clientHost{"box-admin", f.URL, "tok", nil})
			tty := &answeringTerminal{answer: "anything\n"}
			r := tcsvRun(home, tty.open, "machine", "restart", "--host", "box-admin", "--no-wait")
			if _, asked := tty.state(); asked != "" {
				t.Errorf("the terminal was written %q, want nothing", asked)
			}
			if r.code != ExitFailure || !strings.Contains(r.stderr, "--confirm-host") || !strings.Contains(r.stderr, "nothing was sent") {
				t.Errorf("exit %d, stderr %q; want exit %d naming --confirm-host and that nothing was sent", r.code, r.stderr, ExitFailure)
			}
			for _, bad := range []string{"\x1b", "\r", "\x07", "\u009b", "‮", "​", " ", "\xff"} {
				if strings.Contains(r.stderr, bad) {
					t.Errorf("stderr %q carries %q raw, want it quoted", r.stderr, bad)
				}
			}
			if n := len(f.mutations()); n != 0 {
				t.Errorf("%d POSTs, want none", n)
			}
		})
	}
	t.Run("a plain name with spaces and non-ASCII letters is asked for", func(t *testing.T) {
		f := startAPIFake(t, "nas Büro-1")
		home := writeClientConfig(t, "", clientHost{"box-admin", f.URL, "tok", nil})
		tty := &answeringTerminal{answer: "nas Büro-1\n"}
		r := tcsvRun(home, tty.open, "machine", "restart", "--host", "box-admin", "--no-wait")
		if _, asked := tty.state(); r.code != 0 || !strings.Contains(asked, "Type the host name nas Büro-1 to confirm") {
			t.Errorf("exit %d, asked %q, stderr %q; want it asked for", r.code, asked, r.stderr)
		}
	})
}

// SEC-3: the name is checked against what the real op's confirmHost accepts
// (its pattern and length), so a name the server would refuse is never asked
// for, and what the check says quotes the name.
func TestSEC3_TypeableHostNameFollowsTheOpsFieldSchema(t *testing.T) {
	op := servicesspec.Reboot.Op()
	for _, tc := range []struct {
		name, host string
		wantErr    string // Substring; "" when the name may be asked for
	}{
		{"a host name", "home-server", ""},
		{"a name with a space and a non-ASCII letter", "nas B\u00fcro", ""},
		{"the longest", strings.Repeat("a", servicesspec.MaxConfirmHostLength), ""},
		{"one over", strings.Repeat("a", servicesspec.MaxConfirmHostLength+1), "over 253 characters"},
		{"an escape", "a\x1bb", `"a\x1bb"`},
		{"a newline", "a\nb", `"a\nb"`},
		{"a DEL", "a\x7fb", `"a\x7fb"`},
		{"a bidirectional override", "a\u202eb", `"a\u202eb"`},
		{"invalid UTF-8", "a\xffb", "not valid text"},
		{"a long bad name is cut", strings.Repeat("x", 200) + "\x1b", strings.Repeat("x", 80) + `"...`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := typeableHostName(op, "confirmHost", tc.host)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("typeableHostName(%q) = %v, want nil", tc.host, err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("typeableHostName(%q) = %v, want it to contain %s", tc.host, err, tc.wantErr)
			}
			if err != nil {
				for _, raw := range []string{"\x1b", "\n", "\x7f", "\u202e", "\xff"} {
					if strings.Contains(err.Error(), raw) {
						t.Errorf("error %q carries %q raw", err, raw)
					}
				}
			}
		})
	}
}
