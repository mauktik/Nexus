package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
)

// A hypothetical provider with distinct input and output types per binding.

type widgetGetIn struct {
	Name string `json:"name"`
}
type widgetGetOut struct {
	Color string `json:"color"`
}
type widgetLogsIn struct {
	Name string `json:"name"`
}
type widgetRepaintIn struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}
type widgetRepaintOut struct {
	apitypes.ActionResult
	Previous string `json:"previous"`
}
type widgetDeployIn struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type widgetDeployOut struct {
	apitypes.ActionResult
	ExitCode int `json:"exitCode"`
}

func baseOp(id string) def.Op {
	return def.Op{
		ID: id, Provider: "widgets", Kind: def.KindGet, ResourceType: "widgets",
		Summary: "s", Description: "Widget operation for the contract test.",
		Scope: def.Scope{Name: "widgets:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync,
		CLI: def.CLIBinding{Noun: "widget", Verb: "get", Positional: "name"},
	}
}

var (
	widgetGet = def.Define[widgetGetIn, widgetGetOut](baseOp("widgets.widgets.get"))

	widgetLogs = def.Define[widgetLogsIn, apitypes.Frame](func() def.Op {
		op := baseOp("widgets.widgets.logs")
		op.Kind, op.Action, op.Mode = def.KindSubresource, "logs", def.ModeStream
		op.CLI.Noun = "widgetlog"
		return op
	}())

	widgetRepaint = def.Define[widgetRepaintIn, widgetRepaintOut](func() def.Op {
		op := baseOp("widgets.widgets.repaint")
		op.Kind, op.Action = def.KindResourceAction, "repaint"
		op.Scope = def.Scope{Name: "widgets:write"}
		op.Risk, op.Mode = def.RiskMutating, def.ModeLongRunning
		op.CLI.Verb = "update"
		return op
	}())

	widgetDeploy = def.Define[widgetDeployIn, widgetDeployOut](func() def.Op {
		op := baseOp("widgets.widgets.deploy")
		op.Kind, op.Action = def.KindResourceAction, "deploy"
		op.Scope = def.Scope{Name: "widgets:write"}
		op.Risk, op.Mode, op.Cancel = def.RiskMutating, def.ModeLongRunning, def.CancelRunning
		op.CLI.Verb = "invoke"
		return op
	}())
)

type widgetProvider struct{}

func (widgetProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "widgets", ReadScope: "widgets:read",
		Ops: []def.Op{widgetGet.Op(), widgetLogs.Op(), widgetRepaint.Op(), widgetDeploy.Op()}}
}

func (widgetProvider) Configure(decode func(into any) error) error {
	var cfg struct{ Enabled bool }
	return decode(&cfg)
}

func (widgetProvider) Preflight(context.Context) Status {
	return Status{State: apitypes.ProviderEnabled}
}

func (widgetProvider) Bindings() []Binding {
	return []Binding{
		Sync(widgetGet, func(_ context.Context, in widgetGetIn) (widgetGetOut, error) {
			return widgetGetOut{Color: "red:" + in.Name}, nil
		}),
		Stream(widgetLogs, func(_ context.Context, in widgetLogsIn, emit func(apitypes.Frame) error) error {
			return emit(apitypes.DataFrame(0, time.Unix(0, 0).UTC(), apitypes.StreamStdout, in.Name))
		}),
		InProcess(widgetRepaint, InProcessSpec[widgetRepaintIn, widgetRepaintOut]{
			Admit: func(_ context.Context, in widgetRepaintIn) error {
				if in.Color == "" {
					return errors.New("color required")
				}
				return nil
			},
			Lane: func(_ context.Context, in widgetRepaintIn) (Lane, error) {
				return Lane{Key: "widget:" + in.Name, Policy: LaneReject}, nil
			},
			Run: func(_ context.Context, in widgetRepaintIn, r *Run) (widgetRepaintOut, error) {
				if _, err := io.WriteString(r.Output(), "repainting "+in.Name+"\n"); err != nil {
					return widgetRepaintOut{}, err
				}
				return widgetRepaintOut{ActionResult: apitypes.ActionResult{Changed: true}, Previous: "red"}, nil
			},
		}),
		Detached(widgetDeploy, DetachedSpec[widgetDeployIn, widgetDeployOut]{
			Lane: func(_ context.Context, in widgetDeployIn) (Lane, error) {
				return Lane{Key: "widget:" + in.Name, Policy: LaneReject}, nil
			},
			Prepare: func(_ context.Context, in widgetDeployIn, _ *Run) (detach.Spec, error) {
				return detach.Spec{Argv: []string{"/usr/bin/deploy", in.Name, in.Version},
					KillMode: detach.KillControlGroup, MaxRuntime: time.Hour}, nil
			},
			Finalize: func(_ context.Context, _ widgetDeployIn, x detach.Exit, _ *Run) (widgetDeployOut, error) {
				return widgetDeployOut{ActionResult: apitypes.ActionResult{Changed: x.ExitCode == 0}, ExitCode: x.ExitCode}, nil
			},
			LostDetail: func(_ *Run, in widgetDeployIn) string { return "deploy of " + in.Name + " may be partial" },
		}),
	}
}

var _ Provider = widgetProvider{}

// TC-9: the provider contract compiles for every binding kind, and each
// binding exposes its def.Op.
func TestBindingsExposeTheirOps(t *testing.T) {
	p := widgetProvider{}
	bindings := p.Bindings()
	spec := p.Spec()
	if len(bindings) != len(spec.Ops) {
		t.Fatalf("%d bindings for %d ops", len(bindings), len(spec.Ops))
	}
	wantKinds := []BindingKind{BindSync, BindStream, BindInProcess, BindDetached}
	for i, b := range bindings {
		if b.Op().ID != spec.Ops[i].ID {
			t.Errorf("binding %d exposes op %q, want %q", i, b.Op().ID, spec.Ops[i].ID)
		}
		if b.Kind() != wantKinds[i] {
			t.Errorf("binding %d kind %q, want %q", i, b.Kind(), wantKinds[i])
		}
		if b.Op().InputType() == nil || b.Op().InputType() == b.Op().OutputType() {
			t.Errorf("binding %d: input type %v, output type %v; want distinct, set by Define", i, b.Op().InputType(), b.Op().OutputType())
		}
	}
}

func TestErasedCallsReachTypedHandlers(t *testing.T) {
	ctx := context.Background()
	bindings := widgetProvider{}.Bindings()
	get, logs, repaint := bindings[0], bindings[1], bindings[2]

	in, err := get.Decode(json.RawMessage(`{"name":"w1"}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out, err := get.CallSync(ctx, in)
	if err != nil || out.(widgetGetOut).Color != "red:w1" {
		t.Fatalf("CallSync = %v, %v", out, err)
	}
	if _, err := get.Decode(json.RawMessage(`{"name":"w1","extra":1}`)); err == nil {
		t.Error("Decode accepted an unknown field")
	}
	if _, err := get.CallSync(ctx, widgetLogsIn{Name: "w1"}); err == nil {
		t.Error("CallSync accepted an input of the wrong type")
	}
	if _, err := logs.CallSync(ctx, in); err == nil {
		t.Error("CallSync on a stream binding returned no error")
	}

	logsIn, err := logs.Decode(json.RawMessage(`{"name":"w2"}`))
	if err != nil {
		t.Fatalf("Decode logs: %v", err)
	}
	var frames []apitypes.Frame
	if err := logs.CallStream(ctx, logsIn, func(f apitypes.Frame) error { frames = append(frames, f); return nil }); err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	if len(frames) != 1 || frames[0].D != "w2" {
		t.Errorf("frames = %+v", frames)
	}

	rIn, err := repaint.Decode(json.RawMessage(`{"name":"w3","color":"blue"}`))
	if err != nil {
		t.Fatalf("Decode repaint: %v", err)
	}
	ip, ok := repaint.InProcess()
	if !ok {
		t.Fatal("InProcess() not available on an InProcess binding")
	}
	if err := ip.Admit(ctx, rIn); err != nil {
		t.Errorf("Admit: %v", err)
	}
	lane, err := ip.Lane(ctx, rIn)
	if err != nil || lane.Key != "widget:w3" || lane.Policy != LaneReject || lane.Slots() != 1 {
		t.Errorf("Lane = %+v (slots %d), %v", lane, lane.Slots(), err)
	}
	var buf bytes.Buffer
	res, err := ip.Run(ctx, rIn, NewRun("op-1", &buf))
	if err != nil || !res.(widgetRepaintOut).Changed || buf.String() != "repainting w3\n" {
		t.Errorf("Run = %+v, %v, output %q", res, err, buf.String())
	}
	if _, ok := get.InProcess(); ok {
		t.Error("InProcess() available on a sync binding")
	}
}

// TC-9 (extended at 3.3): the erased detached functions reach the typed
// Prepare, Finalize and LostDetail, and only a detached binding has them.
func TestErasedDetachedCallsReachTypedHandlers(t *testing.T) {
	ctx := context.Background()
	bindings := widgetProvider{}.Bindings()
	deploy := bindings[3]
	in, err := deploy.Decode(json.RawMessage(`{"name":"w4","version":"1.2"}`))
	if err != nil {
		t.Fatalf("Decode deploy: %v", err)
	}
	df, ok := deploy.Detached()
	if !ok {
		t.Fatal("Detached() not available on a Detached binding")
	}
	if err := df.Admit(ctx, in); err != nil {
		t.Errorf("Admit with no spec Admit: %v", err)
	}
	if lane, err := df.Lane(ctx, in); err != nil || lane.Key != "widget:w4" {
		t.Errorf("Lane = %+v, %v", lane, err)
	}
	r := NewRun("op-4", nil)
	spec, err := df.Prepare(ctx, in, r)
	if err != nil || strings.Join(spec.Argv, " ") != "/usr/bin/deploy w4 1.2" {
		t.Errorf("Prepare = %+v, %v", spec, err)
	}
	out, err := df.Finalize(ctx, in, detach.Exit{ExitCode: 3}, r)
	if err != nil || out.(widgetDeployOut).ExitCode != 3 || out.(widgetDeployOut).Changed {
		t.Errorf("Finalize = %+v, %v", out, err)
	}
	if d := df.LostDetail(r, in); d != "deploy of w4 may be partial" {
		t.Errorf("LostDetail = %q", d)
	}
	if df.Admission != nil {
		t.Error("Admission is set for a spec without an Admission hook")
	}
	if _, err := df.Prepare(ctx, widgetGetIn{Name: "w4"}, r); err == nil {
		t.Error("Prepare accepted an input of the wrong type")
	}
	for i, b := range bindings[:3] {
		if _, ok := b.Detached(); ok {
			t.Errorf("binding %d (%s) reports Detached()", i, b.Kind())
		}
	}
	if _, ok := deploy.InProcess(); ok {
		t.Error("InProcess() available on a detached binding")
	}
}

func TestInProcessDefaults(t *testing.T) {
	b := InProcess(widgetRepaint, InProcessSpec[widgetRepaintIn, widgetRepaintOut]{
		Run: func(context.Context, widgetRepaintIn, *Run) (widgetRepaintOut, error) { return widgetRepaintOut{}, nil },
	})
	ip, _ := b.InProcess()
	ctx := context.Background()
	in := widgetRepaintIn{Name: "w"}
	if err := ip.Admit(ctx, in); err != nil {
		t.Errorf("nil Admit should admit, got %v", err)
	}
	if lane, err := ip.Lane(ctx, in); err != nil || lane != NoLane {
		t.Errorf("nil Lane should be NoLane, got %+v, %v", lane, err)
	}
	if NewRun("x", nil).Output() == nil {
		t.Error("Run with no output writer returned a nil Output()")
	}
}

func TestBindingModeMismatchPanics(t *testing.T) {
	cases := map[string]func(){
		"Sync on a long-running op": func() {
			Sync(widgetRepaint, func(context.Context, widgetRepaintIn) (widgetRepaintOut, error) { return widgetRepaintOut{}, nil })
		},
		"InProcess on a sync op": func() {
			InProcess(widgetGet, InProcessSpec[widgetGetIn, widgetGetOut]{
				Run: func(context.Context, widgetGetIn, *Run) (widgetGetOut, error) { return widgetGetOut{}, nil },
			})
		},
		"InProcess without Run": func() { InProcess(widgetRepaint, InProcessSpec[widgetRepaintIn, widgetRepaintOut]{}) },
		"nil sync handler":      func() { Sync[widgetGetIn, widgetGetOut](widgetGet, nil) },
		"nil stream handler":    func() { Stream[widgetLogsIn](widgetLogs, nil) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("did not panic")
				}
				if !strings.Contains(fmtAny(r), "widgets.widgets.") {
					t.Errorf("panic %v does not name the op", r)
				}
			}()
			f()
		})
	}
}

func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// TC-9: a handler whose types do not match its declaration is a compile error.
// A probe package is vetted in a throwaway copy of the contract packages; a
// matching control proves the harness can pass.
func TestMismatchedHandlerIsACompileError(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go vet in a temporary module")
	}
	const header = `package probe

import (
	"context"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

type in struct{ Name string ` + "`json:\"name\"`" + ` }
type out struct{ Color string ` + "`json:\"color\"`" + ` }

var op = def.Define[in, out](def.Op{
	ID: "probe.widgets.get", Provider: "probe", Kind: def.KindGet, ResourceType: "widgets",
	Description: "probe", Scope: def.Scope{Name: "probe:read"},
	Risk: def.RiskReadOnly, Mode: def.ModeSync, CLI: def.CLIBinding{Noun: "widget", Verb: "get"},
})
`
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"matching handler (control)", `var B = provider.Sync(op, func(context.Context, in) (out, error) { return out{}, nil })`, false},
		{"wrong input type", `var B = provider.Sync(op, func(context.Context, string) (out, error) { return out{}, nil })`, true},
		{"wrong output type", `var B = provider.Sync(op, func(context.Context, in) (int, error) { return 0, nil })`, true},
		{"explicit mismatched instantiation", `var B = provider.Sync[in, int](op, func(context.Context, in) (int, error) { return 0, nil })`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyContractModule(t)
			dir := filepath.Join(root, "internal", "probe")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(header+tc.body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "vet", "./internal/probe")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local")
			outb, err := cmd.CombinedOutput()
			if tc.wantErr && err == nil {
				t.Fatalf("go vet passed a mismatched handler:\n%s", outb)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("go vet failed the control:\n%s", outb)
			}
			if tc.wantErr && !strings.Contains(string(outb), "probe.go") {
				t.Fatalf("go vet failed, but not on the probe:\n%s", outb)
			}
		})
	}
}

// copyContractModule copies go.mod, go.sum and the non-test sources of the
// contract packages into a temporary module with the same path.
func copyContractModule(t *testing.T) string {
	t.Helper()
	src := moduleRoot(t)
	dst := t.TempDir()
	for _, f := range []string{"go.mod", "go.sum"} {
		copyFile(t, filepath.Join(src, f), filepath.Join(dst, f))
	}
	for _, pkg := range contractPackages(t, src) {
		files, err := filepath.Glob(filepath.Join(src, "internal", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			copyFile(t, f, filepath.Join(dst, "internal", pkg, filepath.Base(f)))
		}
	}
	return dst
}

// contractPackages is the transitive closure of the contract packages'
// in-module imports (non-test), so a new import in any of them is copied
// rather than failing the probe with a missing package.
func contractPackages(t *testing.T, src string) []string {
	t.Helper()
	const prefix = "github.com/mauktik/nexus/internal/"
	args := []string{"list", "-deps", "-f", "{{.ImportPath}}"}
	for _, pkg := range []string{"def", "apitypes", "problem", "provider", "detach", "oplog"} {
		args = append(args, "./internal/"+pkg)
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = src
	outb, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var pkgs []string
	for _, line := range strings.Fields(string(outb)) {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			pkgs = append(pkgs, filepath.FromSlash(rest))
		}
	}
	return pkgs
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
