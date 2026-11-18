// Stub-style: SKIP-only (services-provider) for TC-SV-149, TC-SV-151 and
// TC-SV-152 (WI 5.4).
//
// Tests and stubs of planning/services-provider/test-cases.md TC-SV-123,
// TC-SV-124, TC-SV-149, TC-SV-151, TC-SV-152. Owner WIs: 5.1 (TC-SV-123,
// TC-SV-124, tests), 5.4 (TC-SV-149, TC-SV-151, TC-SV-152). Each stub
// compiles today and skips; the developer of the owning work item replaces
// the skip and the comment with the test, keeping the function name. An
// un-skipped passing test is the GREEN signal
// (planning/services-provider/test-strategy.md, RED Stub Style).

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
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_service_logs", Arguments: args})
	if err != nil {
		t.Fatalf("get_service_logs %v: %v", args, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_service_logs %v: error result %s", args, raw)
	}
	var p servicesPage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// servicesMCP is an MCP client session, holding services:logs, on the real
// services provider over F-HOSTS fh and the F-ROOTS of capture, through the
// real registry, Dispatcher and adapter.
func servicesMCP(t *testing.T, capture string, fh *svcctltest.Host) *mcp.ClientSession {
	t.Helper()
	root := servicesRoot(t, capture)
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
	a, err := mcpadapter.New(reg, d, mcpadapter.Options{MaxBodyBytes: 1 << 20, Logger: log})
	if err != nil {
		t.Fatalf("mcpadapter.New: %v", err)
	}
	caller := auth.Principal{TokenID: "tokL", Name: "tc-sv-124", Scopes: []string{services.ReadScope, services.LogsScope}}
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

// servicesRoot is F-ROOTS for a systemd capture, as the services package's
// testRoot builds it: the capture's proc, run and etc trees, this process's
// PID where the capture's nexusd was, the tools planted, and /.dockerenv
// for a capture taken in a container.
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
	if prov.Init() != platform.Systemd {
		t.Fatalf("%s is not a systemd capture", capture)
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
// - Setup: The services provider over F-HOSTS and F-ROOTS from `t1-alpine-openrc`, with `nx-svc-test`'s `output_log` and `error_log` pointing at temp files holding TC-SV-86's 3,000 `out-*` lines (200 bytes each) and 10 `err-*` lines. The MCP adapter is the real one (`mcpadapter.New` over the real registry and Dispatcher). Subcase (2) builds it with `Options.LogPageBytes: 16 << 10`, so the page is cut by the page size whatever the exact count of the provider's 64 KiB bound: the provider returns about 327 lines (64 KiB) for `tail: 1000`, well over 16 KiB.
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
	t.Skip("TC-SV-149: SKIP-only stub; un-skip when WI 5.4 lands")
}

// TC-SV-151: The since-pager gives no nextOffset for a page that holds an untimed frame, cut or not. Owner WI 5.4. Requirements: FR-6, FR-15, A-38.
//
// - Setup: A planted `ModeStream` op whose input has `since` and `tail`, so the adapter treats it as since-paged (`internal/mcpadapter/tools.go:130-140`). Its handler emits a scripted `[]apitypes.Frame` (the real frame type: an untimed frame has the zero `T`, which `omitzero` leaves out). The real adapter (`mcpadapter.New` over a real registry and Dispatcher) with `Options.LogPageBytes: 64`. Each frame's `D` is 10 ASCII bytes. Subcases:
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
	t.Skip("TC-SV-151: SKIP-only stub; un-skip when WI 5.4 lands")
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
	t.Skip("TC-SV-152: SKIP-only stub; un-skip when WI 5.4 lands")
}
