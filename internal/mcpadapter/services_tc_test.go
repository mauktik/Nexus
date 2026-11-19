// Tests of planning/services-provider/test-cases.md TC-SV-123, TC-SV-124,
// TC-SV-149, TC-SV-151 and TC-SV-152. Owner WIs: 5.1 (TC-SV-123,
// TC-SV-124), 5.4 (TC-SV-149, TC-SV-151, TC-SV-152).

package mcpadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/mcpadapter"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	servicesprov "github.com/mauktik/nexus/internal/providers/services"
	services "github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// servicesTools are the services provider's MCP tools (FR-15): the reboot
// is not one.
var servicesTools = []string{"control_service", "get_service", "get_service_logs", "list_services"}

// TC-SV-123: MCP lists exactly four services tools within the budget, and no reboot tool. Owner WI 5.1. Requirements: FR-15, NFR-6, ASM-6.
//
// - Setup: `catalogTools(t)` and `toolBudget`.
// - Steps:
//  1. Filter the tools that come from `services.*` ops.
//  2. Compute the budget with and without them.
//
// - Expected:
//   - names are exactly `list_services`, `get_service`, `get_service_logs` and `control_service`;
//   - the first three have `readOnlyHint: true`, and `control_service` has `readOnlyHint: false` and `destructiveHint: true`;
//   - each description ends with the untrusted suffix;
//   - no tool name contains `reboot` or `machine`;
//   - the services delta is ≤ 800 characters and ≤ 200 tokens, and the total is ≤ 3,100 tokens;
//   - `TestToolBudget`'s pinned count is 72.
func TestTCSV123_MCPListsExactlyFourServicesToolsWithin(t *testing.T) {
	tools := catalogTools(t)
	specOf := map[string]string{}
	for _, op := range catalog.Ops() {
		if op.Provider == services.Name && op.MCP != nil {
			specOf[op.MCP.Tool] = op.MCP.Description
		}
	}
	var got []string
	for _, tool := range tools {
		if _, ok := specOf[tool.Name]; ok {
			got = append(got, tool.Name)
		}
		if strings.Contains(tool.Name, "reboot") || strings.Contains(tool.Name, "machine") {
			t.Errorf("tool %s names reboot or machine: the reboot is not an MCP tool", tool.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, servicesTools) {
		t.Fatalf("services tools %v, want %v", got, servicesTools)
	}

	without := slices.DeleteFunc(slices.Clone(tools), func(tool *mcp.Tool) bool {
		_, ok := specOf[tool.Name]
		return ok
	})
	for _, tool := range tools {
		if _, ok := specOf[tool.Name]; !ok {
			continue
		}
		a := tool.Annotations
		switch {
		case a == nil:
			t.Errorf("%s: no annotations", tool.Name)
		case tool.Name == "control_service":
			if a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
				t.Errorf("%s: annotations %+v, want readOnlyHint false and destructiveHint true", tool.Name, a)
			}
		case !a.ReadOnlyHint:
			t.Errorf("%s: annotations %+v, want readOnlyHint true", tool.Name, a)
		}
		if !strings.HasPrefix(tool.Description, specOf[tool.Name]) || !strings.HasSuffix(tool.Description, untrustedSuffix) {
			t.Errorf("%s: description %q, want the spec's text ending with the untrusted suffix", tool.Name, tool.Description)
		}
	}

	chars, total := toolBudget(tools)
	baseChars, base := toolBudget(without)
	t.Logf("TC-SV-123: %d tools, %d characters, %d tokens; services adds %d tools, %d characters, %d tokens "+
		"(ceiling: 800 characters, 200 tokens, total 3,100)", len(tools), chars, total, len(tools)-len(without),
		chars-baseChars, total-base)
	if d := chars - baseChars; d > 800 {
		t.Errorf("services adds %d characters, want at most 800", d)
	}
	if d := total - base; d <= 0 || d > 200 {
		t.Errorf("services adds %d tokens, want 1 to 200", d)
	}
	if total > 3100 {
		t.Errorf("total %d tokens, want at most 3,100", total)
	}
	if len(tools) != 72 {
		t.Errorf("%d tools, want 72", len(tools))
	}
}

// TC-SV-124: get_service_logs is since-paged with the default tail. Owner WI 5.1. Requirements: FR-6, FR-15.
//
// - Setup: The services provider over F-HOSTS with TC-SV-85's 2,500-line journal.
// - Steps:
//  1. Call `get_service_logs {name: nexusd}`.
//  2. Call it with `since` from the response's continuation.
//
// - Expected:
//   - (1) the newest 200 lines, with a continuation value;
//   - (2) the lines after it, oldest first, at most 1,000;
//   - the input schema of the tool has `since` and `tail`, and its description carries the since-paged suffix.
func TestTCSV124_GetServiceLogsIsSincePagedWith(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	lines, t0 := synthJournal(t, capture, 5000)
	fh := svcctltest.NewHost(t, capture)
	fh.SetJournal("nexusd", lines[:2500])
	cs := servicesMCP(t, capture, fh)

	// The tool's schema and description.
	var tool *mcp.Tool
	for _, tl := range listTools(t, cs) {
		if tl.Name == "get_service_logs" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatal("get_service_logs is not listed")
	}
	b, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"since", "tail"} {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("get_service_logs input has no %s: %s", k, b)
		}
	}
	if !strings.Contains(tool.Description, "call again with since set to nextOffset") {
		t.Errorf("get_service_logs description %q lacks the since-paged suffix", tool.Description)
	}

	// (1) No since: the newest 200 lines, with a continuation.
	first := servicesLogsPage(t, cs, map[string]any{"name": "nexusd"})
	if len(first.Frames) != services.DefaultTail {
		t.Fatalf("%d frames, want the default tail %d", len(first.Frames), services.DefaultTail)
	}
	if first.Frames[0].D != "l-2300" || first.Frames[len(first.Frames)-1].D != "l-2499" {
		t.Errorf("frames %s to %s, want l-2300 to l-2499", first.Frames[0].D, first.Frames[len(first.Frames)-1].D)
	}
	if first.NextOffset == "" || first.Truncated {
		t.Fatalf("nextOffset %q, truncated %v: want a continuation", first.NextOffset, first.Truncated)
	}

	// (2) The host logs 2,500 more lines; since from the continuation pages
	// forward from it, oldest first, at most 1,000 lines. since is inclusive,
	// so the line at the continuation's time comes again.
	fh.SetJournal("nexusd", lines)
	next := servicesLogsPage(t, cs, map[string]any{"name": "nexusd", "since": first.NextOffset})
	if len(next.Frames) != services.MaxTail {
		t.Fatalf("%d frames after the continuation, want %d", len(next.Frames), services.MaxTail)
	}
	for i, f := range next.Frames {
		if want := fmt.Sprintf("l-%04d", 2499+i); f.D != want {
			t.Fatalf("frame %d is %s, want %s: not the lines after the continuation, oldest first", i, f.D, want)
		}
	}
	if !next.Frames[0].T.Equal(t0.Add(2499 * time.Second)) {
		t.Errorf("first frame at %s, want the continuation's time %s", next.Frames[0].T, t0.Add(2499*time.Second))
	}
}

// servicesPage is get_service_logs's page.
type servicesPage struct {
	Frames     []apitypes.Frame `json:"frames"`
	NextOffset string           `json:"nextOffset"`
	Truncated  bool             `json:"truncated"`
}

func servicesLogsPage(t *testing.T, cs *mcp.ClientSession, args map[string]any) servicesPage {
	t.Helper()
	p, _ := toolPage(t, cs, "get_service_logs", args)
	return p
}

// toolPage calls a log tool and decodes its page, failing on an error
// result. keys are the result JSON's top-level keys.
func toolPage(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (servicesPage, map[string]json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: %v", tool, args, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s %v: error result %s", tool, args, raw)
	}
	var p servicesPage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	return p, keys
}

// servicesMCP is an MCP client session, holding services:logs, on the real
// services provider over F-HOSTS fh and the F-ROOTS of capture, through the
// real registry, Dispatcher and adapter.
func servicesMCP(t *testing.T, capture string, fh *svcctltest.Host) *mcp.ClientSession {
	t.Helper()
	return servicesMCPOn(t, servicesRoot(t, capture), fh, 0)
}

// servicesMCPOn is servicesMCP over F-ROOTS root, with the adapter's page
// size pageBytes (zero: the default).
func servicesMCPOn(t *testing.T, root string, fh *svcctltest.Host, pageBytes int) *mcp.ClientSession {
	t.Helper()
	plat := platform.Info{Init: fh.Init(), OS: platform.OS{ID: "linux"}}
	log := slog.New(slog.DiscardHandler)
	p := servicesprov.New(servicesprov.Options{Host: "h1", Logger: log, GOOS: "linux", Platform: &plat, Run: fh.Run,
		RunLines: fh.RunLines, ProcRoot: filepath.Join(root, "proc"), RunRoot: filepath.Join(root, "run"),
		EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root)})
	cfg, err := config.Parse([]byte("host:\n  name: h1\nproviders:\n  services:\n    enabled: true\n"), "nexusd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log,
		Platform: plat, Providers: map[string]core.ProviderConfig{
			services.Name: {Enabled: true, Decode: cfg.Providers.Services.Decode},
		}})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if ok, st := reg.Available(services.Name); !ok {
		t.Fatalf("services is %s: %s", st.State, st.Reason)
	}
	alog, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alog.Close() })
	// logs is Audited: the Dispatcher writes requested, then completed.
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log, Audit: alog})
	a, err := mcpadapter.New(reg, d, mcpadapter.Options{MaxBodyBytes: 1 << 20, Logger: log, LogPageBytes: pageBytes})
	if err != nil {
		t.Fatalf("mcpadapter.New: %v", err)
	}
	caller := auth.Principal{TokenID: "tokL", Name: "tc-sv-124", Scopes: []string{services.ReadScope, services.LogsScope}}
	return mcpSession(t, a, caller)
}

// mcpSession is an SDK client session on a, every request carrying caller.
func mcpSession(t *testing.T, a *mcpadapter.Adapter, caller auth.Principal) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(rw, r.WithContext(auth.WithPrincipal(r.Context(), caller)))
	}))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "services-tc-test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("MCP connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// servicesRoot is F-ROOTS for capture, as the services package's testRoot
// builds it: the capture's proc, run and etc trees, this process's PID where
// the capture's nexusd was (and OpenRC's options/nexusd/child_pid), the init
// system's tools planted, and /.dockerenv for a capture taken in a
// container.
func servicesRoot(t *testing.T, capture string) string {
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
	pid := strconv.Itoa(os.Getpid())
	plant := map[string]string{filepath.Join("proc", pid, "cgroup"): string(cg)}
	switch prov.Init() {
	case platform.Systemd:
		plant["usr/bin/systemctl"], plant["usr/bin/journalctl"] = "", ""
	case platform.OpenRC:
		plant["run/openrc/options/nexusd/child_pid"] = pid + "\n"
		plant["sbin/rc-service"], plant["bin/rc-status"], plant["sbin/rc-update"] = "", "", ""
	default:
		t.Fatalf("%s is neither a systemd nor an OpenRC capture", capture)
	}
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

// synthJournal is n journal lines from capture's first nexusd line, each a
// second after the last, with MESSAGE l-%04d, and the first line's time (as
// the services package's readsSynthJournal).
func synthJournal(t *testing.T, capture string, n int) ([]string, time.Time) {
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
	return lines, time.UnixMicro(us)
}

// TC-SV-149: get_service_logs on OpenRC file logs never hands back a continuation that answers 400. Owner WI 5.4. Requirements: FR-6, FR-15, ASM-5, A-38.
//
// - Setup: The services provider over F-HOSTS and F-ROOTS from `t1-alpine-openrc`, with `nx-svc-test`'s `output_log` and `error_log` pointing at temp files holding TC-SV-86's 3,000 `out-*` lines (200 bytes each) and 10 `err-*` lines. Every frame the provider emits is untimed: its `T` is the zero time, sent on the wire as `t: "0001-01-01T00:00:00Z"` (TC-SV-86), and the pager's untimed rule keys on `T.IsZero()`. The MCP adapter is the real one (`mcpadapter.New` over the real registry and Dispatcher). Subcase (2) builds it with `Options.LogPageBytes: 16 << 10`, so the page is cut by the page size whatever the exact count of the provider's 64 KiB bound: the provider returns about 327 lines (64 KiB) for `tail: 1000`, well over 16 KiB.
// - Steps:
//  1. Call `get_service_logs {name: nx-svc-test}` (default page size).
//  2. Call `get_service_logs {name: nx-svc-test, tail: 1000}` on the 16 KiB adapter.
//  3. Call `get_service_logs {name: nx-svc-test, tail: 50}` on the 16 KiB adapter (the narrowing `docs/mcp.md` tells the agent to use).
//
// - Expected:
//
//   - no call answers 400 or any other error, and no call is ever made with a `since` taken from a result: no page carries a continuation;
//
//   - (1) is a success with the newest 200 `out-*` lines and the 10 `err-*` lines (as TC-SV-86), `truncated: false`, and the result JSON has no `nextOffset` key;
//
//   - (2) has `truncated: true` and no `nextOffset` key; its `out-*` frames are a contiguous ascending run of lines from the provider's tail window (each `out-N` has N ≥ 2000), fewer than the provider returned;
//
//   - (3) has `truncated: false`, no `nextOffset` key, and its last `out-*` frame is `out-2999` (the newest lines).
//
//     The `docs/mcp.md` wording check moved to TC-SV-130 (WI 5.2) in Revision 1, because WI 5.4 lands before the docs.
func TestTCSV149_GetServiceLogsOnOpenRCFileLogs(t *testing.T) {
	const capture = "t1-alpine-openrc"
	out := make([]string, 3000)
	for i := range out {
		l := fmt.Sprintf("out-%04d ", i)
		out[i] = l + strings.Repeat("x", 200-len(l))
	}
	errs := make([]string, 10)
	for i := range errs {
		errs[i] = fmt.Sprintf("err-%02d", i)
	}
	session := func(t *testing.T, pageBytes int) *mcp.ClientSession {
		t.Helper()
		root := servicesRoot(t, capture)
		// A log path has no symlink component (SEC-2); macOS's /var is one.
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		outPath, errPath := filepath.Join(dir, "out.log"), filepath.Join(dir, "err.log")
		for path, lines := range map[string][]string{outPath: out, errPath: errs} {
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		script := filepath.Join(root, "etc", "init.d", "nx-svc-test")
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Replace(string(b), "output_log=/var/log/nx-svc-test.log", "output_log="+outPath, 1)
		s = strings.Replace(s, "error_log=/var/log/nx-svc-test.err", "error_log="+errPath, 1)
		if s == string(b) {
			t.Fatal("the capture's nx-svc-test script has no output_log and error_log lines to point elsewhere")
		}
		if err := os.WriteFile(script, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
		return servicesMCPOn(t, root, svcctltest.NewHost(t, capture), pageBytes)
	}
	// call is one page; it fails on any error result. No page may carry a
	// continuation, so no call is ever made with a since from a result.
	call := func(t *testing.T, cs *mcp.ClientSession, args map[string]any) (stdout, stderr []string, truncated bool) {
		t.Helper()
		p, keys := toolPage(t, cs, "get_service_logs", args)
		if v, ok := keys["nextOffset"]; ok {
			t.Fatalf("%v: nextOffset %s on untimed frames: a since from it answers 400", args, v)
		}
		for i, f := range p.Frames {
			if !f.T.IsZero() {
				t.Fatalf("%v: frame %d has time %s, want the zero time", args, i, f.T)
			}
			switch f.S {
			case "stdout":
				stdout = append(stdout, f.D)
			case "stderr":
				stderr = append(stderr, f.D)
			default:
				t.Fatalf("%v: frame %d stream %q", args, i, f.S)
			}
		}
		return stdout, stderr, p.Truncated
	}

	t.Run("1 default page", func(t *testing.T) {
		stdout, stderr, truncated := call(t, session(t, 0), map[string]any{"name": "nx-svc-test"})
		if !slices.Equal(stdout, out[len(out)-services.DefaultTail:]) {
			t.Errorf("%d stdout frames, want the newest %d", len(stdout), services.DefaultTail)
		}
		if !slices.Equal(stderr, errs) {
			t.Errorf("stderr frames %q, want %q", stderr, errs)
		}
		if truncated {
			t.Error("truncated: true, want false")
		}
	})

	cs := session(t, 16<<10)
	t.Run("2 tail 1000 on a 16 KiB page", func(t *testing.T) {
		stdout, _, truncated := call(t, cs, map[string]any{"name": "nx-svc-test", "tail": 1000})
		if !truncated {
			t.Error("truncated: false, want true: the page size cut the provider's 64 KiB")
		}
		// The provider returns the newest lines that fit 64 KiB with the err
		// lines (TC-SV-86); the page holds fewer.
		errBytes := 0
		for _, e := range errs {
			errBytes += len(e) + 1
		}
		if provided := (64<<10 - errBytes) / 201; len(stdout) == 0 || len(stdout) >= provided {
			t.Fatalf("%d stdout frames, want 1 to %d", len(stdout), provided-1)
		}
		first := slices.Index(out, stdout[0])
		if first < 2000 || first+len(stdout) > len(out) || !slices.Equal(stdout, out[first:first+len(stdout)]) {
			t.Errorf("stdout frames %.8q..., want a contiguous ascending run of out-N with N >= 2000", stdout[:1])
		}
	})
	t.Run("3 tail 50 on a 16 KiB page", func(t *testing.T) {
		stdout, _, truncated := call(t, cs, map[string]any{"name": "nx-svc-test", "tail": 50})
		if truncated {
			t.Error("truncated: true, want false")
		}
		if len(stdout) == 0 || !strings.HasPrefix(stdout[len(stdout)-1], "out-2999 ") {
			t.Errorf("last stdout frame %.8q, want out-2999", stdout[max(0, len(stdout)-1):])
		}
	})
}

// TC-SV-151: The since-pager gives no nextOffset for a page that holds an untimed frame, cut or not. Owner WI 5.4. Requirements: FR-6, FR-15, A-38.
//
// - Setup: A planted `ModeStream` op whose input has `since` and `tail`, so the adapter treats it as since-paged (`internal/mcpadapter/tools.go:130-140`). Its handler emits a scripted `[]apitypes.Frame` (the real frame type: an untimed frame has the zero `T`, which the wire carries as `t: "0001-01-01T00:00:00Z"`; the pager tells it apart by `T.IsZero()`, not by a missing key, since otherwise `optools.go`'s since-pager would hand back a year-0001 `nextOffset`). The real adapter (`mcpadapter.New` over a real registry and Dispatcher) with `Options.LogPageBytes: 64`. Each frame's `D` is 10 ASCII bytes. Subcases:
//   - (a) 3 untimed frames (30 bytes, not cut);
//   - (b) 20 untimed frames (200 bytes, cut by the 64-byte page);
//   - (c) 1 timed frame followed by 2 untimed frames (not cut);
//   - (d) 1 timed frame followed by 19 untimed frames (cut);
//   - (e) 3 timed frames at `t0`, `t0+1s`, `t0+2s` (not cut), the unchanged timed case.
//
// - Steps:
//  1. Call the op's tool once per subcase, with no `since`.
//
// - Expected:
//   - (a) to (d): the result JSON has no `nextOffset` key; (a) and (c) have `truncated: false` and every frame; (b) and (d) have `truncated: true` and a prefix of the emitted frames whose encoded text fits 64 bytes;
//   - (e): `nextOffset` equals `formatTime(t0+2s)` (the pager's own format) and `truncated: false`, exactly as before WI 5.4;
//   - the existing since-pager tests in `internal/mcpadapter/optools_internal_test.go` (`TestPageBuilder`, `TestPageStopsStream`, `TestSincePagingAlwaysAdvances`, `TestSincePagingKeepsALineThatFitsAPage`, `TestPageLimitCountsEncodedBytes`, `TestPageLimitHoldsOnTheWire`) and `TestLogToolsReturnOneBoundedPage` pass unchanged: they are the regression for timed frames.
func TestTCSV151_TheSincePagerGivesNoNextOffsetForUntimed(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	frames := func(timed, untimed int) []apitypes.Frame {
		var fs []apitypes.Frame
		for i := range timed {
			fs = append(fs, apitypes.Frame{T: t0.Add(time.Duration(i) * time.Second), S: "stdout", D: fmt.Sprintf("timed-%04d", i)})
		}
		for i := range untimed {
			fs = append(fs, apitypes.Frame{S: "stdout", D: fmt.Sprintf("untim-%04d", i)})
		}
		return fs
	}
	cases := []struct {
		name   string
		frames []apitypes.Frame
		cut    bool
		next   string // The nextOffset wanted; empty: no key
	}{
		{"a untimed not cut", frames(0, 3), false, ""},
		{"b untimed cut", frames(0, 20), true, ""},
		{"c timed then untimed not cut", frames(1, 2), false, ""},
		{"d timed then untimed cut", frames(1, 19), true, ""},
		{"e timed not cut", frames(3, 0), false, t0.Add(2 * time.Second).UTC().Format(time.RFC3339Nano)},
		// Beyond the TC's rows: an untimed line larger than the page leaves
		// its start in the page, as a timed one does, rather than an empty
		// page with nothing to continue from.
		{"f one untimed line larger than the page", []apitypes.Frame{{S: "stdout", D: strings.Repeat("y", 100)}}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pw := newPagedWorld(t, 64)
			pw.p.set(tc.frames, false)
			p, keys := toolPage(t, pw.cs, pagedTool, map[string]any{})
			want := tc.frames
			switch {
			case len(tc.frames) == 1 && tc.cut:
				want = []apitypes.Frame{{S: "stdout", D: strings.Repeat("y", 64)}}
			case tc.cut:
				want = tc.frames[:6] // 6 frames of 10 bytes fit 64; the 7th does not
			}
			if p.Truncated != tc.cut {
				t.Errorf("truncated %v, want %v", p.Truncated, tc.cut)
			}
			if !sameFrames(p.Frames, want) {
				t.Errorf("frames %+v, want %+v", p.Frames, want)
			}
			v, ok := keys["nextOffset"]
			switch {
			case tc.next == "" && ok:
				t.Errorf("nextOffset %s on a page holding an untimed frame, want no key", v)
			case tc.next != "" && p.NextOffset != tc.next:
				t.Errorf("nextOffset %q, want %q", p.NextOffset, tc.next)
			}
		})
	}
}

// TC-SV-152: An MCP page that fills ends the audited stream completed with no clientDisconnected fact. Owner WI 5.4. Requirements: FR-16, CON-4, A-44.
//
// - Setup:
//   - A planted `ModeStream`, `RiskReadOnly`, `Audited` op whose input has `since` and `tail`, so the adapter treats it as since-paged (`internal/mcpadapter/tools.go:130-140`); the TC-SV-151 planted op with `Audited` added.
//   - The real adapter (`mcpadapter.New` over a real registry and Dispatcher) with `Options.LogPageBytes: 64`, and a real `audit.Log` over `t.TempDir()`, read back as TC-SV-23 reads it.
//   - Its handler emits `[]apitypes.Frame` (the real frame type) with `T` at `t0 + i*1s` and a 10-byte ASCII `D`. Subcases:
//   - (a) 20 frames: the 64-byte page fills. The handler stops at the first `emit` error and returns nil;
//   - (b) as (a), but after the `emit` error the handler waits for `ctx` to be done and returns `ctx.Err()` (what the handler returns after the stop does not matter);
//   - (c) 3 frames (30 bytes): the page does not fill. The handler returns nil.
//
// - Steps:
//  1. Call the op's tool once per subcase, with no `since`.
//  2. Read the audit records.
//  3. Separately, wrap the sentinel: `fmt.Errorf("%w: page full", core.ErrStreamStopped)`.
//
// - Expected:
//   - (a) and (b): the tool result has `truncated: true`; the audit log holds exactly two records, `requested` then `completed`, and `completed` names `requested`'s ID and carries no `clientDisconnected` fact;
//   - (c): `truncated: false` with all three frames, and the same two records with no `clientDisconnected` fact, exactly as before WI 5.4;
//   - step 3: `errors.Is(err, core.ErrStreamStopped)` is true;
//   - TC-SV-23, TC-SV-24, TC-SV-25 and TC-SV-E5 pass unchanged: a real client disconnect still records `clientDisconnected=true` (A-40), and REST never returns the sentinel.
func TestTCSV152_AnMCPPageThatFillsEndsTheAudited(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	timed := func(n int) []apitypes.Frame {
		fs := make([]apitypes.Frame, n)
		for i := range fs {
			fs[i] = apitypes.Frame{T: t0.Add(time.Duration(i) * time.Second), S: "stdout", D: fmt.Sprintf("line-%04d", i) + "."}
		}
		return fs
	}
	cases := []struct {
		name       string
		frames     []apitypes.Frame
		waitForCtx bool
		emitErr    bool // The handler returns the emit error (code review CR-15)
		full       bool
	}{
		{"a page fills, handler returns the emit error", timed(20), false, true, true},
		{"b page fills, handler returns ctx.Err()", timed(20), true, false, true},
		{"c page does not fill", timed(3), false, false, false},
		{"d page fills, handler returns nil", timed(20), false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pw := newPagedWorld(t, 64)
			pw.p.set(tc.frames, tc.waitForCtx)
			pw.p.setReturnsEmitError(tc.emitErr)
			p, _ := toolPage(t, pw.cs, pagedAuditedTool, map[string]any{})
			if p.Truncated != tc.full {
				t.Errorf("truncated %v, want %v", p.Truncated, tc.full)
			}
			if !tc.full && !sameFrames(p.Frames, tc.frames) {
				t.Errorf("frames %+v, want all three", p.Frames)
			}
			page, err := pw.audit.Query(audit.Query{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			recs := page.Items
			slices.Reverse(recs) // Oldest first
			if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeCompleted {
				t.Fatalf("records %+v, want requested then completed", recs)
			}
			if recs[1].RequestID == "" || recs[1].RequestID != recs[0].ID {
				t.Errorf("completed names %q, want the requested record %q", recs[1].RequestID, recs[0].ID)
			}
			if v, ok := recs[1].Facts["clientDisconnected"]; ok {
				t.Errorf("completed carries clientDisconnected=%s: the pager's own stop is not a client disconnect", v)
			}
		})
	}
}

// The planted since-paged stream ops of TC-SV-151 and TC-SV-152: an input
// with since and tail, so the adapter pages them by since; the second is
// Audited.
const (
	pagedName        = "pg"
	pagedScope       = "pg:read"
	pagedTool        = "get_pg_lines"
	pagedAuditedTool = "get_pg_audited_lines"
)

// PagedInput is the planted ops' input.
type PagedInput struct {
	Since string `json:"since,omitempty" jsonschema:"Only lines at or after this RFC 3339 time."`
	Tail  *int   `json:"tail,omitempty" jsonschema:"The newest this many lines."`
}

func pagedOp(action, tool string, audited bool) def.Op {
	return def.Op{
		ID: pagedName + ".lines." + action, Provider: pagedName, Kind: def.KindList, ResourceType: "lines",
		Action: action, Summary: "Read planted lines",
		Description: "Streams planted log lines for the since-pager tests. Read-only.",
		Scope:       def.Scope{Name: pagedScope}, Risk: def.RiskReadOnly, Mode: def.ModeStream, Idempotent: true,
		Audited: audited,
		CLI:     def.CLIBinding{Noun: "pgline" + action, Verb: "get"},
		MCP:     &def.MCPBinding{Tool: tool, Description: "Read planted lines."},
	}
}

var (
	pagedLines        = def.Define[PagedInput, apitypes.Frame](pagedOp("logs", pagedTool, false))
	pagedAuditedLines = def.Define[PagedInput, apitypes.Frame](pagedOp("auditedlogs", pagedAuditedTool, true))
)

// pagedProv emits its scripted frames. At the first emit error it returns
// nil, or, with waitForCtx, waits for its context to end and returns
// ctx.Err().
type pagedProv struct {
	mu         sync.Mutex
	frames     []apitypes.Frame
	waitForCtx bool
	emitErr    bool // Return the emit error itself, as a handler that passes it up does
}

func (p *pagedProv) setReturnsEmitError(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.emitErr = v
}

func (p *pagedProv) set(frames []apitypes.Frame, waitForCtx bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames, p.waitForCtx = frames, waitForCtx
}

func (p *pagedProv) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: pagedName, ReadScope: pagedScope, Ops: []def.Op{pagedLines.Op(), pagedAuditedLines.Op()}}
}
func (p *pagedProv) Configure(func(any) error) error { return nil }
func (p *pagedProv) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (p *pagedProv) Bindings() []provider.Binding {
	h := func(ctx context.Context, _ PagedInput, emit func(apitypes.Frame) error) error {
		p.mu.Lock()
		frames, wait, passUp := p.frames, p.waitForCtx, p.emitErr
		p.mu.Unlock()
		for _, f := range frames {
			if err := emit(f); err != nil {
				if passUp {
					return err
				}
				if wait {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
		}
		return nil
	}
	return []provider.Binding{provider.Stream(pagedLines, h), provider.Stream(pagedAuditedLines, h)}
}

type pagedWorld struct {
	p     *pagedProv
	cs    *mcp.ClientSession
	audit *audit.Log
}

// newPagedWorld is the planted provider through the real registry,
// Dispatcher and adapter with a page of pageBytes, over a real audit log.
func newPagedWorld(t *testing.T, pageBytes int) *pagedWorld {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	p := &pagedProv{}
	alog, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alog.Close() })
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log,
		Audit: alog, Providers: map[string]core.ProviderConfig{pagedName: {Enabled: true}}})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: log, Audit: alog})
	a, err := mcpadapter.New(reg, d, mcpadapter.Options{MaxBodyBytes: 1 << 20, Logger: log, LogPageBytes: pageBytes})
	if err != nil {
		t.Fatalf("mcpadapter.New: %v", err)
	}
	cs := mcpSession(t, a, auth.Principal{TokenID: "tokP", Name: "tc-sv-151", Scopes: []string{pagedScope}})
	return &pagedWorld{p: p, cs: cs, audit: alog}
}

// sameFrames compares frames by time, stream and text.
func sameFrames(got, want []apitypes.Frame) bool {
	return slices.EqualFunc(got, want, func(a, b apitypes.Frame) bool {
		return a.T.Equal(b.T) && a.S == b.S && a.D == b.D
	})
}
