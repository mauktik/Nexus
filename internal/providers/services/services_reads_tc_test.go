// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-80, TC-SV-81,
// TC-SV-82, TC-SV-83, TC-SV-84, TC-SV-85, TC-SV-86, TC-SV-87, TC-SV-88,
// TC-SV-E15, TC-SV-ERR18, TC-SV-ERR19, TC-SV-ERR20 and TC-SV-ERR21 (WI 4.2):
// list, get and logs, through the real Dispatcher (and the MCP adapter for
// the page shape and the untrusted label) over the T1 captures, with
// F-HOSTS (svcctltest.NewHost) and F-ROOTS (testRoot).
//
// The registry here holds readsOnly, the provider with only list, get and
// logs bound (readBindings), so these tests exercise the reads in isolation.

package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/conformance/testprovider"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/mcpadapter"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
	"github.com/mauktik/nexus/internal/systemspec"
)

const readsJournalctl = "/usr/bin/journalctl"

// readsOnly is the provider with list, get and logs bound.
type readsOnly struct{ *Provider }

func (r readsOnly) Bindings() []provider.Binding { return r.readBindings() }

// readsAudit is the Dispatcher's audit log: it keeps every record.
type readsAudit struct {
	mu   sync.Mutex
	recs []systemspec.AuditRecord
}

func (a *readsAudit) Write(rec systemspec.AuditRecord, _ []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec.ID = fmt.Sprintf("rec-%d", len(a.recs)+1)
	a.recs = append(a.recs, rec)
	return rec.ID, nil
}

func (a *readsAudit) Redact(_, _ string, params json.RawMessage, _ []string) (json.RawMessage, error) {
	return params, nil
}

// of is the records of action, in order.
func (a *readsAudit) of(action string) []systemspec.AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []systemspec.AuditRecord
	for _, r := range a.recs {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// readsSetup is one world: a capture, its services section, and changes to
// F-ROOTS and F-HOSTS made before the provider exists.
type readsSetup struct {
	capture string
	body    string   // The services section; "" = enabled with no allow-list
	allow   []string // systemd: names preflight's LoadState show is answered for
	edit    func(t *testing.T, root string, fh *svcctltest.Host)
	now     func() time.Time
	// runLines wraps F-HOSTS's RunLines.
	runLines func(inner func(context.Context, []string, int, func([]byte) bool) (svcctl.Result, error)) func(context.Context, []string, int, func([]byte) bool) (svcctl.Result, error)
	extra    []provider.Provider
}

type readsWorld struct {
	p     *Provider
	fh    *svcctltest.Host
	root  string
	init  platform.InitSystem
	reg   *core.Registry
	d     *core.Dispatcher
	audit *readsAudit
}

func newReadsWorld(t *testing.T, s readsSetup) *readsWorld {
	t.Helper()
	root := testRoot(t, s.capture)
	fh := svcctltest.NewHost(t, s.capture)
	init := fh.Init()
	if init == platform.Systemd && len(s.allow) > 0 {
		answerLoadStates(t, fh, s.capture, nil, s.allow...)
	}
	if s.edit != nil {
		s.edit(t, root, fh)
	}
	runLines := fh.RunLines
	if s.runLines != nil {
		runLines = s.runLines(fh.RunLines)
	}
	plat := platform.Info{Init: init, OS: platform.OS{ID: "linux"}}
	p := New(Options{Host: "h1", Logger: slog.New(slog.DiscardHandler), GOOS: "linux", Platform: &plat, Run: fh.Run,
		RunLines: runLines, ProcRoot: filepath.Join(root, "proc"), RunRoot: filepath.Join(root, "run"),
		EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root), Now: s.now})
	body := s.body
	if body == "" {
		body = "enabled: true\n"
	}
	providers := []provider.Provider{readsOnly{p}}
	cfg := map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: section(t, body)}}
	for _, e := range s.extra {
		providers = append(providers, e)
		cfg[e.Spec().Name] = core.ProviderConfig{Enabled: true}
	}
	reg, err := core.New(providers, core.Options{Host: "h1", Version: "v0.0.0-test",
		Logger: slog.New(slog.DiscardHandler), Platform: plat, Providers: cfg})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	if ok, st := reg.Available(spec.Name); !ok {
		t.Fatalf("services is %s: %s", st.State, st.Reason)
	}
	aw := &readsAudit{}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: slog.New(slog.DiscardHandler), Audit: aw})
	return &readsWorld{p: p, fh: fh, root: root, init: init, reg: reg, d: d, audit: aw}
}

// readsCaller holds both read scopes, as tokR of TC-SV-88.
var readsCaller = auth.Principal{TokenID: "tokR", Name: "tc-reads",
	Scopes: []string{spec.ReadScope, spec.LogsScope, testprovider.ReadScope}}

func (w *readsWorld) invoke(t *testing.T, op def.Op, input string) (core.InvokeResult, error) {
	t.Helper()
	return w.d.Invoke(context.Background(), core.InvokeRequest{Caller: readsCaller, Surface: core.SurfaceREST, Op: op,
		Input: json.RawMessage(input)})
}

func (w *readsWorld) list(t *testing.T, input string) (apitypes.List[spec.Service], error) {
	t.Helper()
	res, err := w.invoke(t, spec.List.Op(), input)
	if err != nil {
		return apitypes.List[spec.Service]{}, err
	}
	l, ok := res.Body.(apitypes.List[spec.Service])
	if !ok {
		t.Fatalf("list body %T, want apitypes.List[spec.Service]", res.Body)
	}
	return l, nil
}

func (w *readsWorld) mustList(t *testing.T, input string) apitypes.List[spec.Service] {
	t.Helper()
	l, err := w.list(t, input)
	if err != nil {
		t.Fatalf("list %s: %v", input, err)
	}
	return l
}

func (w *readsWorld) get(t *testing.T, name string) (spec.ServiceDetail, error) {
	t.Helper()
	in, err := json.Marshal(spec.NameInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.invoke(t, spec.Get.Op(), string(in))
	if err != nil {
		return spec.ServiceDetail{}, err
	}
	d, ok := res.Body.(spec.ServiceDetail)
	if !ok {
		t.Fatalf("get body %T, want spec.ServiceDetail", res.Body)
	}
	return d, nil
}

func (w *readsWorld) mustGet(t *testing.T, name string) spec.ServiceDetail {
	t.Helper()
	d, err := w.get(t, name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return d
}

// logs runs the logs stream to its end and returns its frames.
func (w *readsWorld) logs(t *testing.T, input string) ([]apitypes.Frame, error) {
	t.Helper()
	res, err := w.invoke(t, spec.Logs.Op(), input)
	if err != nil {
		return nil, err
	}
	if res.Stream == nil {
		t.Fatalf("logs %s: no stream", input)
	}
	frames := []apitypes.Frame{}
	err = res.Stream(context.Background(), func(f apitypes.Frame) error {
		frames = append(frames, f)
		return nil
	})
	return frames, err
}

func (w *readsWorld) mustLogs(t *testing.T, input string) []apitypes.Frame {
	t.Helper()
	frames, err := w.logs(t, input)
	if err != nil {
		t.Fatalf("logs %s: %v", input, err)
	}
	return frames
}

// allNames follows the cursor with limit until the last page.
func (w *readsWorld) allNames(t *testing.T, filter string, limit int) []string {
	t.Helper()
	var names []string
	cursor := ""
	for range 10000 {
		in := map[string]any{"limit": limit}
		if filter != "" {
			if err := json.Unmarshal([]byte(filter), &in); err != nil {
				t.Fatal(err)
			}
			in["limit"] = limit
		}
		if cursor != "" {
			in["cursor"] = cursor
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		page := w.mustList(t, string(b))
		for _, s := range page.Items {
			names = append(names, s.Name)
		}
		if page.NextCursor == "" {
			return names
		}
		cursor = page.NextCursor
	}
	t.Fatal("the cursor never ended")
	return nil
}

func readsWantProblem(t *testing.T, err error, slug problem.Slug) *problem.Error {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != slug {
		t.Fatalf("err = %v, want %s", err, slug)
	}
	return pe
}

// readsWantInvalid checks err is validation-failed naming field.
func readsWantInvalid(t *testing.T, err error, field string) *problem.Error {
	t.Helper()
	pe := readsWantProblem(t, err, problem.ValidationFailed)
	errs, _ := pe.Extensions["errors"].([]problem.FieldError)
	if !slices.ContainsFunc(errs, func(e problem.FieldError) bool { return e.Path == "/"+field }) {
		t.Errorf("errors %v, want one naming /%s", pe.Extensions["errors"], field)
	}
	return pe
}

// readsOracle are svcctl.List over a second F-HOSTS on the same capture:
// the expected set of TC-SV-46 and TC-SV-49.
func readsOracle(t *testing.T, capture string) []svcctl.Entry {
	t.Helper()
	fh := svcctltest.NewHost(t, capture)
	entries, err := svcctl.List(context.Background(), fh.Init(), svcctl.Options{Run: fh.Run})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func readsNames(entries []svcctl.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func readsCaptures(t *testing.T) []string {
	t.Helper()
	all := captureNames(t)
	return slices.Sorted(func(yield func(string) bool) {
		for k := range all {
			if !yield(k) {
				return
			}
		}
	})
}

// TC-SV-80: list pages every service exactly once in name order. Owner WI 4.2. Requirements: FR-4, NFR-2.
func TestTCSV80_ListPagesEveryServiceExactlyOnceIn(t *testing.T) {
	for _, capture := range readsCaptures(t) {
		t.Run(capture, func(t *testing.T) {
			w := newReadsWorld(t, readsSetup{capture: capture})
			want := readsNames(readsOracle(t, capture))
			if !slices.IsSorted(want) {
				slices.Sort(want)
			}
			var got []string
			cursor := ""
			for page := 0; ; page++ {
				in := `{"limit":7}`
				if cursor != "" {
					in = fmt.Sprintf(`{"limit":7,"cursor":%q}`, cursor)
				}
				l := w.mustList(t, in)
				for _, s := range l.Items {
					got = append(got, s.Name)
				}
				if l.NextCursor == "" {
					if len(l.Items) > 7 || len(l.Items) == 0 && len(want) > 0 {
						t.Errorf("last page has %d items", len(l.Items))
					}
					break
				}
				if len(l.Items) != 7 {
					t.Errorf("page %d has %d items and a cursor, want 7", page, len(l.Items))
				}
				cursor = l.NextCursor
				if page > len(want) {
					t.Fatal("the cursor never ended")
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("paged names (%d) != expected (%d)\ngot  %q\nwant %q", len(got), len(want), got, want)
			}
			if len(slices.Compact(slices.Clone(got))) != len(got) {
				t.Errorf("a name repeats: %q", got)
			}
			def := w.mustList(t, `{}`)
			if len(def.Items) != min(spec.DefaultLimit, len(want)) {
				t.Errorf("default page has %d items, want %d", len(def.Items), min(spec.DefaultLimit, len(want)))
			}
		})
	}
}

// TC-SV-81: list filters by state, enabled and controllable. Owner WI 4.2. Requirements: FR-4.
func TestTCSV81_ListFiltersByStateEnabledAndControllable(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	allow := []string{"nx-svc-test", "nx-svc-fail"}
	w := newReadsWorld(t, readsSetup{capture: capture, allow: allow,
		body: "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: nx-svc-fail\n"})
	oracle := readsOracle(t, capture)
	controllable := func(e svcctl.Entry) bool { return slices.Contains(allow, e.Name) }
	for _, c := range []struct {
		filter string
		pred   func(svcctl.Entry) bool
	}{
		{`{"state":"failed"}`, func(e svcctl.Entry) bool { return e.State == "failed" }},
		{`{"state":"running"}`, func(e svcctl.Entry) bool { return e.State == "running" }},
		{`{"enabled":"masked"}`, func(e svcctl.Entry) bool { return e.Enabled == "masked" }},
		{`{"controllable":"true"}`, controllable},
		{`{"controllable":"false"}`, func(e svcctl.Entry) bool { return !controllable(e) }},
		{`{"state":"failed","controllable":"true"}`, func(e svcctl.Entry) bool { return e.State == "failed" && controllable(e) }},
	} {
		var want []string
		for _, e := range oracle {
			if c.pred(e) {
				want = append(want, e.Name)
			}
		}
		got := w.allNames(t, c.filter, spec.MaxLimit)
		if !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", c.filter, got, want)
		}
		if len(want) == 0 {
			t.Errorf("%s: the oracle has no match, so the filter is untested", c.filter)
		}
	}
	if got := w.allNames(t, `{"state":"failed","controllable":"true"}`, 100); !slices.Equal(got, []string{"nx-svc-fail"}) {
		t.Errorf("state=failed&controllable=true = %q, want [nx-svc-fail]", got)
	}

	// The .service failed set agrees with ListFailed's .service subset.
	oh := svcctltest.NewHost(t, capture)
	failed, err := svcctl.ListFailed(context.Background(), platform.Systemd, svcctl.Options{Run: oh.Run})
	if err != nil {
		t.Fatal(err)
	}
	var wantFailed []string
	for _, n := range failed.Names {
		if s, ok := strings.CutSuffix(n, ".service"); ok {
			wantFailed = append(wantFailed, s)
		}
	}
	if got := w.allNames(t, `{"state":"failed"}`, 100); !slices.Equal(got, wantFailed) {
		t.Errorf("state=failed = %q, ListFailed's .service subset = %q", got, wantFailed)
	}

	// A filter value outside its enum is refused naming the parameter, by
	// the Dispatcher's schema and by the handler itself.
	for field, bad := range map[string]string{"state": "sleeping", "enabled": "on", "controllable": "yes"} {
		_, err := w.list(t, fmt.Sprintf(`{%q:%q}`, field, bad))
		readsWantInvalid(t, err, field)
		in := spec.ListInput{}
		switch field {
		case "state":
			in.State = bad
		case "enabled":
			in.Enabled = bad
		case "controllable":
			in.Controllable = bad
		}
		_, err = w.p.list(context.Background(), in)
		readsWantInvalid(t, err, field)
	}
}

// TC-SV-82: controllable and protected come from one settings snapshot. Owner WI 4.2. Requirements: FR-4, FR-8, FR-9.
func TestTCSV82_ControllableAndProtectedComeFromOneSettings(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	ctx := context.Background()
	w := newReadsWorld(t, readsSetup{capture: capture,
		body: "enabled: true\ncontrol:\n  - name: nx-svc-test\n  - name: nexusd-db\n",
		edit: func(t *testing.T, root string, fh *svcctltest.Host) {
			// The TC-SV-77 systemd setup: nexusd runs as nexus-agent, which
			// Requires= nexusd-db.
			cg := filepath.Join(root, "proc", strconv.Itoa(os.Getpid()), "cgroup")
			b, err := os.ReadFile(cg)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, cg, strings.Replace(string(b), "/nexusd.service", "/nexus-agent.service", 1))
			deps, err := svcctltest.CaptureOutput(capture, "deps-nexusd.txt")
			if err != nil {
				t.Fatal(err)
			}
			out := strings.Replace(string(deps.Output), "Requires=", "Requires=nexusd-db.service ", 1)
			fh.Answer([]string{systemctlTool, "show", "-p", "Requires", "-p", "BindsTo", "-p", "Requisite", "-p", "PartOf",
				"--", "nexus-agent.service"}, svcctl.Result{Output: []byte(out)}, nil)
			answerLoadStates(t, fh, capture, nil, "nx-svc-test", "nexusd-db")
			// nexusd-db and ssh are loaded, running units (the capture has
			// neither): list-units gains their lines, and show answers
			// nx-svc-test's properties under their Ids.
			units, err := svcctltest.CaptureOutput(capture, "list-units.txt")
			if err != nil {
				t.Fatal(err)
			}
			listing := string(units.Output) + "nexusd-db.service loaded active running Nexus database\n" +
				"ssh.service loaded active running OpenBSD Secure Shell server\n"
			fh.Answer([]string{systemctlTool, "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager", "--full"},
				svcctl.Result{Output: []byte(listing)}, nil)
			model := svcctltest.NewHost(t, capture)
			show, err := model.Run(ctx, showArgv("nx-svc-test"))
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"nexusd-db", "ssh"} {
				fh.Answer(showArgv(n), svcctl.Result{Output: []byte(strings.ReplaceAll(string(show.Output),
					"nx-svc-test.service", n+".service"))}, nil)
			}
		}})
	want := map[string]struct {
		controllable, protected bool
		why                     string
	}{
		"nx-svc-test": {true, false, ""},
		"nexusd-db":   {false, true, "Requires="},
		"ssh":         {false, true, "ssh"},
	}
	listed := map[string]spec.ServiceProperties{}
	for _, s := range w.mustList(t, `{"limit":1000}`).Items {
		listed[s.Name] = s.Properties
	}
	for name, wv := range want {
		check := func(src string, p spec.ServiceProperties) {
			if p.Controllable != wv.controllable || p.Protected != wv.protected || !strings.Contains(p.ProtectedReason, wv.why) {
				t.Errorf("%s %s: controllable %v, protected %v (%q), want %v, %v naming %q", src, name, p.Controllable,
					p.Protected, p.ProtectedReason, wv.controllable, wv.protected, wv.why)
			}
			if p.Controllable && p.Protected {
				t.Errorf("%s %s is controllable and protected", src, name)
			}
		}
		p, ok := listed[name]
		if !ok {
			t.Errorf("list has no %s", name)
		} else {
			check("list", p)
		}
		check("get", w.mustGet(t, name).Properties.ServiceProperties)
	}
}

// readsWantDetails checks got against svcctl.Describe's d: every non-null field
// equals d's, every null one has a reason in unavailable, and no non-null
// one has a reason.
func readsWantDetails(t *testing.T, got spec.ServiceDetails, d svcctl.Details) {
	t.Helper()
	if got.State != d.State || got.RawState != d.RawState || got.Enabled != d.Enabled || got.EnabledRaw != d.EnabledRaw {
		t.Errorf("state %q/%q enabled %q/%q, want %q/%q %q/%q", got.State, got.RawState, got.Enabled, got.EnabledRaw,
			d.State, d.RawState, d.Enabled, d.EnabledRaw)
	}
	if !slices.Equal(got.Runlevels, d.Runlevels) {
		t.Errorf("runlevels %q, want %q", got.Runlevels, d.Runlevels)
	}
	str := readsVal[string]
	fields := map[string]struct{ got, want any }{
		"description":   {str(got.Description), map[bool]any{true: d.Description, false: nil}[d.DescriptionKnown]},
		"subState":      {str(got.SubState), readsNilIf(d.Unavailable, "subState", d.SubState)},
		"result":        {str(got.Result), readsNilIf(d.Unavailable, "result", d.Result)},
		"unitFilePath":  {str(got.UnitFilePath), readsNilIf(d.Unavailable, "unitFilePath", d.UnitFilePath)},
		"scriptPath":    {str(got.ScriptPath), readsNilIf(d.Unavailable, "scriptPath", d.ScriptPath)},
		"canonicalName": {str(got.CanonicalName), readsNilIf(d.Unavailable, "canonicalName", d.CanonicalName)},
	}
	fields["mainPid"] = struct{ got, want any }{readsVal(got.MainPID), readsNilIf(d.Unavailable, "mainPid", d.MainPID)}
	fields["since"] = struct{ got, want any }{readsUnixNano(got.Since), readsUnixNano(d.Since)}
	fields["restarts"] = struct{ got, want any }{readsVal(got.Restarts), readsVal(d.Restarts)}
	fields["memoryBytes"] = struct{ got, want any }{readsVal(got.MemoryBytes), readsVal(d.MemoryBytes)}
	for k, f := range fields {
		if fmt.Sprint(f.got) != fmt.Sprint(f.want) {
			t.Errorf("%s = %v, want %v", k, f.got, f.want)
		}
		reason, has := got.Unavailable[k]
		switch {
		case f.got == nil && (!has || reason == ""):
			t.Errorf("%s is null with no reason in unavailable %v", k, got.Unavailable)
		case f.got != nil && has:
			t.Errorf("%s is %v but unavailable has %q", k, f.got, reason)
		case f.got == nil && d.Unavailable[k] != "" && reason != d.Unavailable[k]:
			t.Errorf("%s reason %q, want svcctl's %q", k, reason, d.Unavailable[k])
		}
	}
	for k := range got.Unavailable {
		if _, ok := fields[k]; !ok {
			t.Errorf("unavailable names %q, which is not a field", k)
		}
	}
}

func readsVal[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func readsUnixNano(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.UnixNano()
}

func readsNilIf[T comparable](unavailable map[string]string, key string, v T) any {
	if _, ok := unavailable[key]; ok {
		return nil
	}
	var zero T
	if key == "mainPid" && v == zero {
		return nil
	}
	return v
}

// TC-SV-83: get returns every field, a reason for every null, and logs info. Owner WI 4.2. Requirements: FR-5, FR-6.
func TestTCSV83_GetReturnsEveryFieldAReasonFor(t *testing.T) {
	ctx := context.Background()
	describe := func(t *testing.T, w *readsWorld, capture, name string) svcctl.Details {
		t.Helper()
		oh := svcctltest.NewHost(t, capture)
		d, err := svcctl.Describe(ctx, oh.Init(), name, svcctl.Options{Run: oh.Run, ProcRoot: filepath.Join(w.root, "proc"),
			RunRoot: filepath.Join(w.root, "run"), EtcRoot: filepath.Join(w.root, "etc")})
		if err != nil || !d.Known {
			t.Fatalf("oracle Describe %s: %+v, %v", name, d, err)
		}
		return d
	}
	for _, capture := range readsCaptures(t) {
		t.Run(capture, func(t *testing.T) {
			w := newReadsWorld(t, readsSetup{capture: capture})
			got := w.mustGet(t, "nexusd")
			if got.Name != "nexusd" || got.Host != "h1" || got.Type != spec.ResourceType {
				t.Errorf("resource %+v", got)
			}
			readsWantDetails(t, got.Properties, describe(t, w, capture, "nexusd"))
			// The JSON has every field, null or not.
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var obj struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"since", "mainPid", "subState", "result", "restarts", "memoryBytes", "unitFilePath",
				"scriptPath", "canonicalName", "description", "logs"} {
				if _, ok := obj.Properties[k]; !ok {
					t.Errorf("properties have no %q key: %s", k, raw)
				}
			}
			logs := got.Properties.Logs
			switch w.init {
			case platform.Systemd:
				if logs != (spec.LogsInfo{Available: true, Source: spec.LogSourceJournal}) {
					t.Errorf("logs %+v, want available journal", logs)
				}
			case platform.OpenRC:
				// The shipped deploy/nexusd.openrc logs through logger.
				if logs.Available || logs.Source != spec.LogSourceUnavailable || !strings.Contains(logs.Reason, "output_logger") {
					t.Errorf("logs %+v, want unavailable naming output_logger", logs)
				}
				test := w.mustGet(t, "nx-svc-test")
				readsWantDetails(t, test.Properties, describe(t, w, capture, "nx-svc-test"))
				if l := test.Properties.Logs; l != (spec.LogsInfo{Available: true, Source: spec.LogSourceOpenRCFiles}) {
					t.Errorf("nx-svc-test logs %+v, want available openrc-files", l)
				}
			}
		})
	}
	t.Run("t1-alpine-openrc without log lines", func(t *testing.T) {
		w := newReadsWorld(t, readsSetup{capture: "t1-alpine-openrc", edit: func(t *testing.T, root string, _ *svcctltest.Host) {
			path := filepath.Join(root, "etc", "init.d", "nx-svc-test")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var kept []string
			for line := range strings.Lines(string(b)) {
				if !strings.HasPrefix(line, "output_log=") && !strings.HasPrefix(line, "error_log=") {
					kept = append(kept, line)
				}
			}
			writeFile(t, path, strings.Join(kept, ""))
		}})
		l := w.mustGet(t, "nx-svc-test").Properties.Logs
		if l != (spec.LogsInfo{Source: spec.LogSourceUnavailable, Reason: "this service declares no log file"}) {
			t.Errorf("logs %+v, want unavailable: this service declares no log file", l)
		}
	})
}

// readsJournalLines are svcctl.Journal's lines of name over a second F-HOSTS on
// capture: TC-SV-62's decoded lines.
func readsJournalLines(t *testing.T, capture, name string, q svcctl.JournalQuery) []apitypes.Frame {
	t.Helper()
	oh := svcctltest.NewHost(t, capture)
	var out []apitypes.Frame
	err := svcctl.Journal(context.Background(), name, q, svcctl.Options{RunLines: oh.RunLines}, func(l svcctl.JournalLine) bool {
		out = append(out, apitypes.Frame{T: l.T, S: l.S, D: l.D})
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readsFramesEqual(a, b []apitypes.Frame) bool {
	return slices.EqualFunc(a, b, func(x, y apitypes.Frame) bool {
		return x.T.Equal(y.T) && x.S == y.S && x.D == y.D && x.O == nil && x.Type == ""
	})
}

// readsMCP is an MCP client session on the world's Dispatcher through the
// MCP adapter, as caller.
func readsMCP(t *testing.T, w *readsWorld, caller auth.Principal) *mcp.ClientSession {
	t.Helper()
	a, err := mcpadapter.New(w.reg, w.d, mcpadapter.Options{MaxBodyBytes: 1 << 20, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("mcpadapter.New: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		a.ServeHTTP(rw, r.WithContext(auth.WithPrincipal(r.Context(), caller)))
	}))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "services-reads-test", Version: "1.0.0"}, nil)
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

func readsTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tools/call %s: error result %s", name, raw)
	}
	return raw
}

// TC-SV-84: logs returns the newest tail lines as frames, newest last, within 64 KiB per page. Owner WI 4.2. Requirements: FR-6, NFR-2.
func TestTCSV84_LogsReturnsTheNewestTailLinesAs(t *testing.T) {
	for _, capture := range readsCaptures(t) {
		if captureNames(t)[capture] != platform.Systemd {
			continue
		}
		t.Run(capture, func(t *testing.T) {
			w := newReadsWorld(t, readsSetup{capture: capture})
			all := readsJournalLines(t, capture, "nexusd", svcctl.JournalQuery{Tail: spec.DefaultTail})
			if len(all) == 0 || len(all) >= spec.DefaultTail {
				t.Fatalf("the capture holds %d lines; the test needs fewer than the default tail", len(all))
			}
			got := w.mustLogs(t, `{"name":"nexusd"}`)
			if !readsFramesEqual(got, all) {
				t.Errorf("frames %+v, want the capture's %+v", got, all)
			}
			for i := 1; i < len(got); i++ {
				if got[i].T.Before(got[i-1].T) {
					t.Errorf("frame %d (%s) is older than frame %d (%s): not oldest first", i, got[i].T, i-1, got[i-1].T)
				}
			}
			for _, tail := range []int{5, 2} {
				want := all[max(0, len(all)-tail):]
				if got := w.mustLogs(t, fmt.Sprintf(`{"name":"nexusd","tail":%d}`, tail)); !readsFramesEqual(got, want) {
					t.Errorf("tail=%d frames %+v, want the last %d %+v", tail, got, tail, want)
				}
			}

			// The MCP page has get_container_logs's shape, within 64 KiB.
			raw := readsTool(t, readsMCP(t, w, readsCaller), "get_service_logs", map[string]any{"name": "nexusd"})
			var page map[string]json.RawMessage
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"frames", "nextOffset", "truncated"} {
				if _, ok := page[k]; !ok {
					t.Errorf("page has no %q: %s", k, raw)
				}
			}
			var frames []apitypes.Frame
			if err := json.Unmarshal(page["frames"], &frames); err != nil {
				t.Fatal(err)
			}
			if !readsFramesEqual(frames, all) {
				t.Errorf("MCP frames %+v, want %+v", frames, all)
			}
			text := 0
			for _, f := range frames {
				text += len(f.D)
			}
			if text > 64<<10 {
				t.Errorf("page text %d bytes, over 64 KiB", text)
			}
		})
	}
}

// readsSynthJournal is n journal lines from capture's first nexusd line, each a
// second after the last, with MESSAGE l-%04d, and the first line's time.
func readsSynthJournal(t *testing.T, capture string, n int) ([]string, time.Time) {
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

// TC-SV-85: logs with since and no tail pages forward and stops at 1000 lines. Owner WI 4.2. Requirements: FR-6.
func TestTCSV85_LogsWithSinceAndNoTailPages(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	lines, t0 := readsSynthJournal(t, capture, 2500)
	now := t0.Add(60 * time.Minute)
	w := newReadsWorld(t, readsSetup{capture: capture, now: func() time.Time { return now },
		edit: func(_ *testing.T, _ string, fh *svcctltest.Host) { fh.SetJournal("nexusd", lines) }})

	since := t0.Add(100 * time.Second).UTC().Format(time.RFC3339Nano)
	got := w.mustLogs(t, fmt.Sprintf(`{"name":"nexusd","since":%q}`, since))
	if len(got) != spec.MaxTail {
		t.Fatalf("%d frames, want %d", len(got), spec.MaxTail)
	}
	if got[0].D != "l-0100" || got[len(got)-1].D != "l-1099" {
		t.Errorf("frames %s to %s, want l-0100 to l-1099", got[0].D, got[len(got)-1].D)
	}
	for i, f := range got {
		if want := fmt.Sprintf("l-%04d", 100+i); f.D != want || !f.T.Equal(t0.Add(time.Duration(100+i)*time.Second)) {
			t.Fatalf("frame %d = %+v, want %s at its time", i, f, want)
		}
	}
	if !w.fh.StoppedEarly() {
		t.Error("F-HOSTS did not record stoppedEarly: the reader did not stop at 1000 lines")
	}

	w.mustLogs(t, `{"name":"nexusd","since":"90m"}`)
	from := now.Add(-90 * time.Minute)
	want := fmt.Sprintf("--since=@%d.%06d", from.Unix(), from.Nanosecond()/1000)
	var argvs [][]string
	for _, c := range w.fh.Calls() {
		if len(c.Argv) > 0 && c.Argv[0] == readsJournalctl {
			argvs = append(argvs, c.Argv)
		}
	}
	if len(argvs) != 2 || !slices.Contains(argvs[1], want) || slices.Contains(argvs[1], "-n") {
		t.Errorf("journalctl argvs %q, want the second with %s and no -n", argvs, want)
	}
}

// readsFileLogWorld is t1-alpine-openrc with nx-svc-test's output_log and
// error_log at temp files holding out and errs.
func readsFileLogWorld(t *testing.T, out, errs []string) *readsWorld {
	t.Helper()
	// TailFile refuses a symbolic link in a log file's path (SEC-2), and on
	// macOS /var, under which t.TempDir is, is one.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outPath, errPath := filepath.Join(dir, "out.log"), filepath.Join(dir, "err.log")
	writeFile(t, outPath, strings.Join(out, "\n")+"\n")
	writeFile(t, errPath, strings.Join(errs, "\n")+"\n")
	return newReadsWorld(t, readsSetup{capture: "t1-alpine-openrc", edit: func(t *testing.T, root string, _ *svcctltest.Host) {
		path := filepath.Join(root, "etc", "init.d", "nx-svc-test")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := strings.Replace(string(b), "output_log=/var/log/nx-svc-test.log", "output_log="+outPath, 1)
		s = strings.Replace(s, "error_log=/var/log/nx-svc-test.err", "error_log="+errPath, 1)
		if s == string(b) {
			t.Fatal("the capture's nx-svc-test script has no output_log and error_log lines to point elsewhere")
		}
		writeFile(t, path, s)
	}})
}

// TC-SV-86: OpenRC file logs keep the newest lines within 64 KiB with stdout and stderr labelled. Owner WI 4.2. Requirements: FR-6, ASM-5, A-38.
func TestTCSV86_OpenRCFileLogsKeepTheNewestLines(t *testing.T) {
	out := make([]string, 3000)
	for i := range out {
		l := fmt.Sprintf("out-%04d ", i)
		out[i] = l + strings.Repeat("x", 200-len(l))
	}
	errs := make([]string, 10)
	for i := range errs {
		errs[i] = fmt.Sprintf("err-%02d", i)
	}
	w := readsFileLogWorld(t, out, errs)
	split := func(frames []apitypes.Frame) (stdout, stderr []string) {
		for _, f := range frames {
			switch f.S {
			case "stdout":
				stdout = append(stdout, f.D)
			case "stderr":
				stderr = append(stderr, f.D)
			default:
				t.Errorf("frame stream %q", f.S)
			}
		}
		return stdout, stderr
	}
	untimed := func(frames []apitypes.Frame) {
		for i, f := range frames {
			if !f.T.IsZero() {
				t.Fatalf("frame %d has time %s, want none", i, f.T)
			}
		}
		t.Run("t is the zero time on the wire", func(t *testing.T) {
			// The data-frame wire shape always carries t (OpenAPI: required),
			// so an untimed frame sends the zero time and consumers key on
			// T.IsZero() (orchestrator decision after WI 4.2; A-38).
			for i, f := range frames {
				b, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]json.RawMessage
				if err := json.Unmarshal(b, &m); err != nil {
					t.Fatal(err)
				}
				if got := string(m["t"]); got != `"0001-01-01T00:00:00Z"` {
					t.Fatalf("frame %d JSON t = %s, want the zero time: %s", i, got, b)
				}
			}
		})
	}

	frames := w.mustLogs(t, `{"name":"nx-svc-test"}`)
	untimed(frames)
	stdout, stderr := split(frames)
	if !slices.Equal(stdout, out[len(out)-spec.DefaultTail:]) {
		t.Errorf("default: %d stdout frames from %.8q, want the newest %d", len(stdout), stdout, spec.DefaultTail)
	}
	if !slices.Equal(stderr, errs) {
		t.Errorf("default: stderr frames %q, want %q", stderr, errs)
	}

	frames = w.mustLogs(t, `{"name":"nx-svc-test","tail":1000}`)
	untimed(frames)
	stdout, stderr = split(frames)
	if !slices.Equal(stderr, errs) {
		t.Errorf("tail=1000: stderr frames %q, want %q", stderr, errs)
	}
	text := 0
	for _, f := range frames {
		text += len(f.D) + 1
	}
	if text > 64<<10 {
		t.Errorf("tail=1000: %d bytes of text, over 64 KiB", text)
	}
	// The bound forces the oldest stdout line: one more would not fit.
	errBytes := 0
	for _, e := range errs {
		errBytes += len(e) + 1
	}
	n := (64<<10 - errBytes) / 201
	if !slices.Equal(stdout, out[len(out)-n:]) {
		t.Errorf("tail=1000: %d stdout frames, the oldest %.8q; want the newest %d from %.8q", len(stdout),
			stdout[:min(1, len(stdout))], n, out[len(out)-n])
	}
	if text+201 <= 64<<10 {
		t.Errorf("tail=1000: %d bytes leave room for the next older line", text)
	}
}

// readsPlanted are the values TC-SV-87 plants where no output may reach.
var readsPlanted = []string{"planted-env-7f3a", "planted-confd-91c2", "planted-cmd-55e0"}

// readsPlantFIFOs makes proc/<pid>/cmdline and environ FIFOs: an open for
// reading of one blocks, and readsWantNoReader detects it.
func readsPlantFIFOs(t *testing.T, root string, pid int) []string {
	t.Helper()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range []string{"cmdline", "environ"} {
		p := filepath.Join(dir, f)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// readsWantNoReader checks nothing holds the FIFO open for reading: an open for
// writing that does not block fails with ENXIO only then.
func readsWantNoReader(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		syscall.Close(fd)
		t.Errorf("%s was opened for reading", path)
		return
	}
	if !errors.Is(err, syscall.ENXIO) {
		t.Errorf("open %s for writing: %v, want ENXIO", path, err)
	}
}

// TC-SV-87: No environment, command line or foreign conf.d value appears in any output. Owner WI 4.2. Requirements: NFR-5, FR-14.
func TestTCSV87_NoEnvironmentCommandLineOrForeignConf(t *testing.T) {
	for _, capture := range []string{firstCaptureOf(t, platform.Systemd, "t1-ubuntu-systemd"), "t1-alpine-openrc"} {
		t.Run(capture, func(t *testing.T) {
			var fifos []string
			w := newReadsWorld(t, readsSetup{capture: capture, edit: func(t *testing.T, root string, fh *svcctltest.Host) {
				env := svcctl.Result{Output: []byte("Environment=NX_SECRET=planted-env-7f3a\n")}
				for _, props := range []string{"Environment", "Environment,ExecStart"} {
					fh.Answer([]string{systemctlTool, "show", "-p", props, "--", "nexusd.service"}, env, nil)
				}
				pid := os.Getpid() // OpenRC: child_pid
				if fh.Init() == platform.Systemd {
					oh := svcctltest.NewHost(t, capture)
					d, err := svcctl.Describe(context.Background(), platform.Systemd, "nexusd", svcctl.Options{Run: oh.Run})
					if err != nil || d.MainPID == 0 {
						t.Fatalf("the capture's nexusd has no MainPID: %+v %v", d, err)
					}
					pid = d.MainPID
				}
				fifos = readsPlantFIFOs(t, root, pid)
				if fh.Init() == platform.OpenRC {
					writeFile(t, filepath.Join(root, "etc", "conf.d", "nx-svc-test"),
						"NX_TOKEN=planted-confd-91c2\noutput_log=/var/log/nx-svc-test.log\n")
				}
			}})

			var outputs [][]byte
			keep := func(v any) {
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				outputs = append(outputs, b)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				l, err := w.list(t, `{"limit":1000}`)
				if err != nil {
					t.Errorf("list: %v", err)
					return
				}
				keep(l)
				for _, s := range l.Items {
					if !serviceName.MatchString(s.Name) {
						continue
					}
					if d, err := w.get(t, s.Name); err != nil {
						keep(err)
					} else {
						keep(d)
					}
					if frames, err := w.logs(t, fmt.Sprintf(`{"name":%q}`, s.Name)); err != nil {
						keep(err)
					} else {
						keep(frames)
					}
				}
				keep(w.p.Preflight(context.Background()).Details)
			}()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the reads blocked: a planted FIFO was opened")
			}
			if len(outputs) < 3 {
				t.Fatalf("only %d outputs", len(outputs))
			}
			for _, b := range outputs {
				for _, p := range readsPlanted {
					if strings.Contains(string(b), p) {
						t.Errorf("output carries %s: %.300s", p, b)
					}
				}
			}
			for _, c := range w.fh.Calls() {
				for _, a := range c.Argv {
					if strings.Contains(a, "Environment") || strings.Contains(a, "ExecStart") {
						t.Errorf("argv %q requests a sensitive property", c.Argv)
					}
				}
			}
			for _, f := range fifos {
				readsWantNoReader(t, f)
			}
		})
	}
}

// TC-SV-88: Reads are untrusted, logs are audited, and list and get are not. Owner WI 4.2. Requirements: FR-14, FR-16, ASM-4.
func TestTCSV88_ReadsAreUntrustedLogsAreAuditedAnd(t *testing.T) {
	w := newReadsWorld(t, readsSetup{capture: "t1-ubuntu-systemd"})
	cs := readsMCP(t, w, readsCaller)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"list_services", map[string]any{}},
		{"get_service", map[string]any{"name": "nexusd"}},
		{"get_service_logs", map[string]any{"name": "nexusd", "tail": 3}},
	} {
		raw := readsTool(t, cs, c.tool, c.args)
		var got struct {
			ContentTrust string `json:"contentTrust"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.ContentTrust != "untrusted-host-data" {
			t.Errorf("%s: contentTrust %q, want untrusted-host-data: %.200s", c.tool, got.ContentTrust, raw)
		}
	}
	recs := w.audit.of(spec.Logs.Op().ID)
	if len(recs) != 2 || recs[0].Outcome != audit.OutcomeRequested || recs[1].Outcome != audit.OutcomeCompleted {
		t.Fatalf("logs records %+v, want requested then completed", recs)
	}
	for _, r := range recs {
		if r.TokenID != "tokR" || r.Target != "nexusd" {
			t.Errorf("record %+v, want token tokR and target nexusd", r)
		}
	}
	if recs[1].RequestID != recs[0].ID || recs[1].Facts["service"] != "nexusd" {
		t.Errorf("completed %+v, want it to name %s with the fact service=nexusd", recs[1], recs[0].ID)
	}
	for _, op := range []def.Op{spec.List.Op(), spec.Get.Op()} {
		if r := w.audit.of(op.ID); len(r) != 0 {
			t.Errorf("%s wrote records %+v, want none", op.ID, r)
		}
	}
}

// TC-SV-E15: Paging handles a cursor at the last item, limit 1 and limit 1000. Owner WI 4.2. Requirements: FR-4.
func TestTCSVE15_PagingHandlesACursorAtTheLast(t *testing.T) {
	const capture = "t1-ubuntu-systemd"
	w := newReadsWorld(t, readsSetup{capture: capture})
	want := readsNames(readsOracle(t, capture))
	if len(want) >= spec.MaxLimit {
		t.Fatalf("the capture holds %d services, not fewer than %d", len(want), spec.MaxLimit)
	}
	var got []string
	cursor := ""
	for i := 0; i <= len(want); i++ {
		in := `{"limit":1}`
		if cursor != "" {
			in = fmt.Sprintf(`{"limit":1,"cursor":%q}`, cursor)
		}
		l := w.mustList(t, in)
		if len(l.Items) != 1 {
			t.Fatalf("page %d has %d items, want 1", i, len(l.Items))
		}
		got = append(got, l.Items[0].Name)
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Errorf("limit=1 pages %q, want %q", got, want)
	}

	all := w.mustList(t, `{"limit":1000}`)
	if len(all.Items) != len(want) || all.NextCursor != "" {
		t.Errorf("limit=1000: %d items, cursor %q; want %d and none", len(all.Items), all.NextCursor, len(want))
	}

	last := base64.RawURLEncoding.EncodeToString([]byte(want[len(want)-1]))
	end := w.mustList(t, fmt.Sprintf(`{"cursor":%q}`, last))
	if len(end.Items) != 0 || end.NextCursor != "" {
		t.Errorf("cursor at the last name: %d items, cursor %q; want an empty page and none", len(end.Items), end.NextCursor)
	}
	if end.Items == nil {
		t.Error("an empty page's items are null, want []")
	}

	_, err := w.list(t, `{"cursor":"!!"}`)
	readsWantInvalid(t, err, "cursor")
	_, err = w.p.list(context.Background(), spec.ListInput{Cursor: "*"})
	readsWantInvalid(t, err, "cursor")
}

// TC-SV-ERR18: get and logs answer 404 for an unknown name and 400 for a bad name without an exec. Owner WI 4.2. Requirements: FR-5, FR-6, NFR-5.
func TestTCSVERR18_GetAndLogsAnswer404ForAn(t *testing.T) {
	for _, capture := range []string{"t1-ubuntu-systemd", "t1-alpine-openrc"} {
		t.Run(capture, func(t *testing.T) {
			w := newReadsWorld(t, readsSetup{capture: capture})
			_, err := w.get(t, "nx-nope")
			readsWantProblem(t, err, problem.NotFound)
			_, err = w.logs(t, `{"name":"nx-nope"}`)
			readsWantProblem(t, err, problem.NotFound)
			for _, c := range w.fh.Calls() {
				if c.Argv[0] == readsJournalctl {
					t.Errorf("logs of an unknown name ran %q", c.Argv)
				}
			}

			before := len(w.fh.Calls())
			_, err = w.get(t, "a;b")
			readsWantInvalid(t, err, "name")
			_, err = w.logs(t, `{"name":"../x"}`)
			readsWantInvalid(t, err, "name")
			ctx := context.Background()
			for _, name := range []string{"a;b", "../x", "nexusd.service", ""} {
				_, err = w.p.get(ctx, spec.NameInput{Name: name})
				readsWantInvalid(t, err, "name")
				err = w.p.logs(ctx, spec.LogsInput{Name: name}, func(apitypes.Frame) error { return nil })
				readsWantInvalid(t, err, "name")
			}
			if calls := w.fh.Calls()[before:]; len(calls) != 0 {
				t.Errorf("bad names ran %d calls: %v", len(calls), calls)
			}
		})
	}
}

// TC-SV-ERR19: logs refuses out-of-range and platform-inapplicable inputs with 400. Owner WI 4.2. Requirements: FR-6.
func TestTCSVERR19_LogsRefusesOutOfRangeAndPlatform(t *testing.T) {
	ctx := context.Background()
	intp := func(n int) *int { return &n }
	for _, capture := range []string{"t1-ubuntu-systemd", "t1-alpine-openrc"} {
		t.Run(capture, func(t *testing.T) {
			w := newReadsWorld(t, readsSetup{capture: capture})
			before := len(w.fh.Calls())
			cases := []struct {
				in    spec.LogsInput
				field string
			}{
				{spec.LogsInput{Name: "nexusd", Tail: intp(0)}, "tail"},
				{spec.LogsInput{Name: "nexusd", Tail: intp(1001)}, "tail"},
				{spec.LogsInput{Name: "nexusd", Since: "169h"}, "since"},
				{spec.LogsInput{Name: "nexusd", Since: "7d"}, "since"},
				{spec.LogsInput{Name: "nexusd", Priority: "loud"}, "priority"},
			}
			if w.init == platform.OpenRC {
				cases = append(cases,
					struct {
						in    spec.LogsInput
						field string
					}{spec.LogsInput{Name: "nx-svc-test", Priority: "err"}, "priority"},
					struct {
						in    spec.LogsInput
						field string
					}{spec.LogsInput{Name: "nx-svc-test", Since: "1h"}, "since"})
			}
			for _, c := range cases {
				in, err := json.Marshal(c.in)
				if err != nil {
					t.Fatal(err)
				}
				frames, err := w.logs(t, string(in))
				pe := readsWantInvalid(t, err, c.field)
				if len(frames) != 0 {
					t.Errorf("%s: %d frames", in, len(frames))
				}
				err = w.p.logs(ctx, c.in, func(apitypes.Frame) error { return nil })
				hpe := readsWantInvalid(t, err, c.field)
				if w.init == platform.OpenRC && (c.in.Since == "1h" || c.in.Priority == "err") {
					for _, p := range []*problem.Error{pe, hpe} {
						if !strings.Contains(fmt.Sprint(p.Extensions["errors"]), "does not apply to file logs") {
							t.Errorf("%s: %v, want it to say the field does not apply to file logs", in, p.Extensions["errors"])
						}
					}
				}
			}
			if calls := w.fh.Calls()[before:]; len(calls) != 0 {
				t.Errorf("refused inputs ran %d calls: %v", len(calls), calls)
			}
		})
	}
}

// TC-SV-ERR20: logs with no source answers 422 logs-unavailable and streams nothing. Owner WI 4.2. Requirements: FR-6, ASM-5, A-15.
func TestTCSVERR20_LogsWithNoSourceAnswers422Logs(t *testing.T) {
	check := func(t *testing.T, w *readsWorld, name, source, reason string) {
		t.Helper()
		frames, err := w.logs(t, fmt.Sprintf(`{"name":%q}`, name))
		pe := readsWantProblem(t, err, problem.LogsUnavailable)
		if len(frames) != 0 {
			t.Errorf("%s: %d frames, want none", name, len(frames))
		}
		if pe.Status != 422 || pe.Extensions["service"] != name || pe.Extensions["logSource"] != source ||
			!strings.Contains(fmt.Sprint(pe.Extensions["reason"]), reason) {
			t.Errorf("%s: %d %v, want 422 with service, logSource %s and a reason naming %q", name, pe.Status,
				pe.Extensions, source, reason)
		}
		var mine []systemspec.AuditRecord
		for _, r := range w.audit.of(spec.Logs.Op().ID) {
			if r.Target == name {
				mine = append(mine, r)
			}
		}
		if len(mine) != 2 || mine[0].Outcome != audit.OutcomeRequested || mine[1].Outcome != audit.OutcomeRefused ||
			mine[1].Problem != string(problem.LogsUnavailable) || mine[1].RequestID != mine[0].ID {
			t.Errorf("%s: records %+v, want requested then refused logs-unavailable", name, mine)
		}
	}
	t.Run("openrc", func(t *testing.T) {
		w := newReadsWorld(t, readsSetup{capture: "t1-alpine-openrc"})
		lf, err := svcctl.OpenRCLogFiles(filepath.Join(w.root, "etc"), "acpid")
		if err != nil || lf.Reason != svcctl.NoLogFileReason {
			t.Fatalf("acpid declares a log: %+v %v", lf, err)
		}
		check(t, w, "acpid", spec.LogSourceOpenRCFiles, "this service declares no log file")
		check(t, w, "docker", spec.LogSourceOpenRCFiles, "output_logger")
	})
	t.Run("systemd", func(t *testing.T) {
		w := newReadsWorld(t, readsSetup{capture: "t1-ubuntu-systemd",
			edit: func(_ *testing.T, _ string, fh *svcctltest.Host) { fh.NoJournal() }})
		check(t, w, "nexusd", spec.LogSourceJournal, "journalctl is not present")
	})
}

// TC-SV-ERR21: A failing or hung journalctl answers 503 within the timeout and blocks no other provider. Owner WI 4.2. Requirements: NFR-1, NFR-3.
//
// The provider has no Timeout option (WI 4.1's Options): svcctl bounds each
// journal read at svcctl.DefaultTimeout (TC-SV-63). The hang variant puts
// the 300 ms bound in the RunLines seam, as the real runner's expired bound
// returns, and checks what the provider makes of it.
func TestTCSVERR21_AFailingOrHungJournalctlAnswers503(t *testing.T) {
	argv := []string{readsJournalctl, "--no-pager", "--quiet", "-o", "json", "--output-fields=MESSAGE,PRIORITY", "-u",
		"nexusd.service", "-n", "200"}
	t.Run("exit 1", func(t *testing.T) {
		const stderr = "No journal files were opened due to insufficient permissions."
		w := newReadsWorld(t, readsSetup{capture: "t1-ubuntu-systemd", edit: func(_ *testing.T, _ string, fh *svcctltest.Host) {
			fh.Answer(argv, svcctl.Result{ExitCode: 1, Output: []byte(stderr + "\n")}, nil)
		}})
		_, err := w.logs(t, `{"name":"nexusd"}`)
		pe := readsWantProblem(t, err, problem.DependencyUnavailable)
		if pe.Status != 503 || !strings.Contains(pe.Detail, "journalctl") || !strings.Contains(pe.Detail, stderr) {
			t.Errorf("%d %q, want 503 naming journalctl and %q", pe.Status, pe.Detail, stderr)
		}
	})
	t.Run("hang", func(t *testing.T) {
		spy := &testprovider.Spy{}
		stub := testprovider.NewStub(testprovider.Spec(), testprovider.Sync(spy, testprovider.WidgetsList))
		w := newReadsWorld(t, readsSetup{capture: "t1-ubuntu-systemd", extra: []provider.Provider{stub},
			edit: func(_ *testing.T, _ string, fh *svcctltest.Host) { fh.HangOn(argv) },
			runLines: func(inner func(context.Context, []string, int, func([]byte) bool) (svcctl.Result, error)) func(context.Context, []string, int, func([]byte) bool) (svcctl.Result, error) {
				return func(ctx context.Context, argv []string, maxLine int, onLine func([]byte) bool) (svcctl.Result, error) {
					ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
					defer cancel()
					return inner(ctx, argv, maxLine, onLine)
				}
			}})
		start := time.Now()
		type result struct {
			err     error
			elapsed time.Duration
		}
		done := make(chan result, 1)
		go func() {
			_, err := w.logs(t, `{"name":"nexusd"}`)
			done <- result{err, time.Since(start)}
		}()
		time.Sleep(50 * time.Millisecond)
		select {
		case r := <-done:
			t.Fatalf("logs answered after %s, before the hang: %v", r.elapsed, r.err)
		default:
		}
		other := time.Now()
		if _, err := w.invoke(t, testprovider.WidgetsList.Op(), `{}`); err != nil {
			t.Fatalf("planted list: %v", err)
		}
		if d := time.Since(other); d >= 100*time.Millisecond {
			t.Errorf("the planted provider's list took %s while logs hung, want < 100ms", d)
		}
		select {
		case r := <-done:
			pe := readsWantProblem(t, r.err, problem.DependencyUnavailable)
			if pe.Status != 503 || !strings.Contains(pe.Detail, "journalctl") {
				t.Errorf("%d %q, want 503 naming journalctl", pe.Status, pe.Detail)
			}
			if r.elapsed >= time.Second {
				t.Errorf("the hang answered after %s, want within 1s", r.elapsed)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the hung logs call never answered")
		}
	})
}
