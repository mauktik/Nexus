// Tests of planning/services-provider/test-cases.md TC-SV-102 to TC-SV-114,
// TC-SV-116, TC-SV-150, TC-SV-E16, TC-SV-E17, TC-SV-E18, TC-SV-E20 and
// TC-SV-ERR25 to TC-SV-ERR32 (WI 4.4): the guarded reboot, its admission,
// lane, Prepare, Finalize, LostDetail and AfterReboot, and the
// services-helper's reboot mode (D-3 S1 to S5, D-4, DF-4, DF-5).
//
// No test ever runs a reboot: every trigger is a fake (F-HELPER), or for
// the S4 process-group cases (TC-SV-150) a /bin/sh child that touches marker
// files, run through the real svcctl.Run in place of RebootArgv.
//
// Four harnesses:
//   - rbHelper: runReboot in process, with F-HELPER's Sleep, Run and hook
//     and a cancel that is the cancel file and the stop context's cancel;
//   - rbWorld: the provider over a T1 capture with F-ROOTS planted (boot ID
//     CUR, kernel 7.0.0-42-generic, the Debian reboot marker), F-OPS,
//     F-DAEMON and F-HOSTS answering `is-enabled nexusd`;
//   - rbStack: rbWorld behind the real Dispatcher, ops manager (F-BOOT),
//     store and audit log, with the helper run in process by a fake
//     launcher, or, for TC-SV-114, TC-SV-150 (c) and TC-SV-ERR31, run as a
//     subprocess by the real op-supervise through detachtest.Local();
//   - the helper process: this test binary re-executed with
//     rbHelperProcessEnv naming its configuration (Go's helper-process
//     pattern); it makes the stop context with signal.NotifyContext as
//     Helper does and runs runHelper with F-HELPER seams.

package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
	"github.com/mauktik/nexus/internal/systemspec"
)

// F-BOOT and F-ROOTS values (test-strategy.md).
const (
	rbPrev      = "11111111-1111-4111-8111-111111111111"
	rbCur       = "22222222-2222-4222-8222-222222222222"
	rbKernel    = "7.0.0-42-generic"
	rbNewKernel = "7.0.0-43-generic"
	rbPkg       = "linux-image-7.0.0-43-generic"
	rbUptime    = 5640281.12
	rbCapture   = "t1-ubuntu-systemd"
	rbHost      = "h1"
	// rbHelperProcessEnv names the helper-process configuration: set, this
	// test binary is the services helper, not the tests.
	rbHelperProcessEnv = "NEXUS_SERVICES_REBOOT_HELPER_TEST"
)

var rbIsEnabledArgv = []string{systemctlTool, "is-enabled", "--", "nexusd.service"}

func TestMain(m *testing.M) {
	if cfg := os.Getenv(rbHelperProcessEnv); cfg != "" {
		os.Exit(rbHelperProcess(cfg))
	}
	os.Exit(m.Run())
}

// ---- F-HELPER in process ----

// rbFake is F-HELPER: the reboot mode's Sleep, Run and hook, recording
// every call. sleepFn decides each Sleep by its index (0 is the delay);
// nil returns at once for the delay and blocks until the context is done
// after it. triggerFn decides each trigger; nil exits 0.
type rbFake struct {
	mu        sync.Mutex
	dir       string
	sleeps    []time.Duration
	sleepErrs []error
	triggers  [][]string
	trigCtx   []context.Context
	stages    []string
	recorded  map[string]bool // ReadReboot ok at each stage
	recBoot   map[string]string
	sleepFn   func(n int, ctx context.Context, d time.Duration) error
	triggerFn func(ctx context.Context, argv []string) (svcctl.Result, error)
	stageFn   func(stage string)
}

func (f *rbFake) env(procRoot string) rebootHelperEnv {
	return rebootHelperEnv{ProcRoot: procRoot,
		Sleep: func(ctx context.Context, d time.Duration) error {
			f.mu.Lock()
			n := len(f.sleeps)
			f.sleeps = append(f.sleeps, d)
			fn := f.sleepFn
			f.mu.Unlock()
			var err error
			switch {
			case fn != nil:
				err = fn(n, ctx, d)
			case n > 0:
				<-ctx.Done()
				err = ctx.Err()
			}
			f.mu.Lock()
			f.sleepErrs = append(f.sleepErrs, err)
			f.mu.Unlock()
			return err
		},
		Run: func(ctx context.Context, argv []string) (svcctl.Result, error) {
			f.mu.Lock()
			f.triggers = append(f.triggers, slices.Clone(argv))
			f.trigCtx = append(f.trigCtx, ctx)
			fn := f.triggerFn
			f.mu.Unlock()
			if fn != nil {
				return fn(ctx, argv)
			}
			return svcctl.Result{}, nil
		},
		hook: func(stage string) {
			f.mu.Lock()
			f.stages = append(f.stages, stage)
			dir, fn := f.dir, f.stageFn
			f.mu.Unlock()
			if dir != "" {
				r, ok, err := detach.ReadReboot(dir)
				f.mu.Lock()
				if f.recorded == nil {
					f.recorded, f.recBoot = map[string]bool{}, map[string]string{}
				}
				f.recorded[stage], f.recBoot[stage] = ok && err == nil, r.BootID
				f.mu.Unlock()
			}
			if fn != nil {
				fn(stage)
			}
		},
	}
}

func (f *rbFake) triggerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.triggers)
}

// rbHelper is the reboot mode run in process on one operation directory.
type rbHelper struct {
	t        *testing.T
	dir      string
	procRoot string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	fake     *rbFake
}

// errRBStop is the cause of every injected stop, so a context that a stop
// reached says so through context.Cause.
var errRBStop = errors.New("injected stop")

// newRBHelper writes input.json for init with delaySeconds and timeout
// 300 s, and plants the boot ID CUR under a ProcRoot.
func newRBHelper(t *testing.T, init platform.InitSystem, delay int) *rbHelper {
	t.Helper()
	h := &rbHelper{t: t, dir: t.TempDir(), procRoot: rbProcRoot(t, rbCur)}
	h.ctx, h.cancel = context.WithCancelCause(context.Background())
	t.Cleanup(func() { h.cancel(nil) })
	h.fake = &rbFake{dir: h.dir}
	rbWriteInput(t, h.dir, init, delay)
	return h
}

func rbWriteInput(t *testing.T, dir string, init platform.InitSystem, delay int) {
	t.Helper()
	in := rebootHelperInput{Mode: helperModeReboot, OperationID: "op-1", Init: string(init), DelaySeconds: delay,
		TimeoutSeconds: 300, BootID: rbCur, KernelRelease: rbKernel}
	if err := writeHelperJSON(dir, helperInputFile, in); err != nil {
		t.Fatal(err)
	}
}

// rbProcRoot is a ProcRoot holding boot only.
func rbProcRoot(t *testing.T, boot string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, procBootID), boot+"\n")
	return root
}

// inject is the real cancel: the cancel-request file, then the stop.
func (h *rbHelper) inject() {
	if err := detach.RequestCancel(h.dir); err != nil {
		h.t.Error(err)
	}
	h.cancel(errRBStop)
}

func (h *rbHelper) run() int { return runReboot(h.ctx, h.dir, h.fake.env(h.procRoot)) }

// record is ReadReboot of the helper's directory, failing on an error.
func rbRecord(t *testing.T, dir string) (detach.RebootRecord, bool) {
	t.Helper()
	r, ok, err := detach.ReadReboot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

func rbNoResult(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, helperResultFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("result.json exists (%v), want none", err)
	}
}

// ---- The helper process ----

// rbHelperConfig configures the helper process (rbHelperProcess).
type rbHelperConfig struct {
	ProcRoot string `json:"procRoot"`
	// Stage, when set, makes the hook at that stage (or each of several
	// joined by "+") write <dir>/hook-<stage> and wait (polling every 10 ms, at most 10 s) for
	// <dir>/hook-<stage>.release.
	Stage string `json:"stage,omitempty"`
	// RealDelay makes the delay a real sleep; otherwise it returns at once.
	// The post-trigger wait is always real, so it blocks until a stop.
	RealDelay bool `json:"realDelay,omitempty"`
	// Script, when set, is the trigger: `/bin/sh -c Script`, with {dir}
	// replaced, run through the real svcctl.Run with the context the
	// helper gave the seam. Otherwise the trigger appends a line to
	// <dir>/trigger-called and exits 0 at once.
	Script string `json:"script,omitempty"`
}

// rbHelperProcess is this binary as `nexusd services-helper run --dir D`
// with F-HELPER seams. It runs the production helper (Helper's own body),
// so the SIGTERM stop context a real cancel reaches is the production one,
// made before input.json is read (CR-5).
func rbHelperProcess(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	var cfg rbHelperConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if len(os.Args) < 3 || os.Args[1] != HelperCommand {
		fmt.Fprintf(os.Stderr, "helper process: args %q\n", os.Args)
		return 3
	}
	args := os.Args[2:]
	dir := ""
	for i, a := range args {
		if a == "--dir" && i+1 < len(args) {
			dir = args[i+1]
		}
	}
	var sleeps atomic.Int32
	env := rebootHelperEnv{ProcRoot: cfg.ProcRoot,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if sleeps.Add(1) == 1 && !cfg.RealDelay {
				return nil
			}
			return sleepCtx(ctx, d)
		},
		Run: func(ctx context.Context, argv []string) (svcctl.Result, error) {
			if cfg.Script != "" {
				return svcctl.Run(ctx, []string{"/bin/sh", "-c", strings.ReplaceAll(cfg.Script, "{dir}", dir)})
			}
			f, err := os.OpenFile(filepath.Join(dir, "trigger-called"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
			if err != nil {
				return svcctl.Result{}, err
			}
			_, werr := fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), strings.Join(argv, " "))
			if err := errors.Join(werr, f.Close()); err != nil {
				return svcctl.Result{}, err
			}
			return svcctl.Result{}, nil
		},
		hook: func(stage string) {
			if !slices.Contains(strings.Split(cfg.Stage, "+"), stage) {
				return
			}
			base := filepath.Join(dir, "hook-"+stage)
			if err := os.WriteFile(base, nil, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				if _, err := os.Stat(base + ".release"); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		},
	}
	return helper(args, os.Stdout, os.Stderr, helperEnv{reboot: env})
}

// rbHelperEnvFor writes cfg and returns the environment entry that makes
// this binary the helper process with it.
func rbHelperEnvFor(t *testing.T, cfg rbHelperConfig) string {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "helper.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return rbHelperProcessEnv + "=" + path
}

func rbTestBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// rbProc is the helper process started directly, as TC-SV-ERR26 and
// TC-SV-ERR30 run it: no supervisor, real signals.
type rbProc struct {
	t    *testing.T
	dir  string
	cmd  *exec.Cmd
	done chan struct{}
	code int
	end  time.Time
}

func startRBProc(t *testing.T, delay int, cfg rbHelperConfig) *rbProc {
	t.Helper()
	p := &rbProc{t: t, dir: t.TempDir(), done: make(chan struct{})}
	rbWriteInput(t, p.dir, platform.Systemd, delay)
	cfg.ProcRoot = rbProcRoot(t, rbCur)
	p.cmd = exec.Command(rbTestBinary(t), HelperCommand, "run", "--dir", p.dir)
	p.cmd.Env = append(os.Environ(), rbHelperEnvFor(t, cfg))
	var out bytes.Buffer
	p.cmd.Stdout, p.cmd.Stderr = &out, &out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := p.cmd.Wait()
		p.end = time.Now()
		p.code = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.code = ee.ExitCode()
		} else if err != nil {
			p.code = -2
		}
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
		if t.Failed() {
			t.Logf("helper output:\n%s", out.String())
		}
	})
	return p
}

func (p *rbProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *rbProc) wait(limit time.Duration) int {
	p.t.Helper()
	select {
	case <-p.done:
		return p.code
	case <-time.After(limit):
		p.t.Fatalf("the helper did not exit within %v", limit)
		return 0
	}
}

func (p *rbProc) signal(sig syscall.Signal) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
}

// waitFile waits for path to exist, at most limit.
func waitFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", path, limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func triggerLines(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "trigger-called"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// ---- F-OPS, F-DAEMON and the provider world ----

// rbOps is F-OPS: the Active answers, one per call (the last repeats).
type rbOps struct {
	mu    sync.Mutex
	seq   [][]string
	calls int
}

func (o *rbOps) set(ids ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq = [][]string{ids}
}

func (o *rbOps) script(answers ...[]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq = answers
}

func (o *rbOps) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *rbOps) Active() ([]provider.ActiveOperation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	var ids []string
	if n := len(o.seq); n > 0 {
		ids = o.seq[min(o.calls, n)-1]
	}
	out := make([]provider.ActiveOperation, len(ids))
	for i, id := range ids {
		out[i] = provider.ActiveOperation{ID: id, Op: "test.op", Status: apitypes.StatusRunning}
	}
	return out, nil
}

// rbDaemon is F-DAEMON: CheckConfig answers errs.
type rbDaemon struct {
	mu    sync.Mutex
	errs  config.ErrorList
	calls int
}

func (d *rbDaemon) fail(fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errs = nil
	if fail {
		d.errs = config.ErrorList{{File: "/etc/nexus/nexusd.yaml", Line: 3, Column: 5, Path: "providers.services.reboot.timeout",
			Msg: "must be 1m to 30m"}}
	}
}

func (d *rbDaemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *rbDaemon) ConfigPath() string   { return "/etc/nexus/nexusd.yaml" }
func (d *rbDaemon) StateDir() string     { return "" }
func (d *rbDaemon) StartedAt() time.Time { return time.Time{} }
func (d *rbDaemon) CheckConfig() provider.ConfigCheck {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return provider.ConfigCheck{Errors: slices.Clone(d.errs)}
}
func (d *rbDaemon) CheckConfigData([]byte) provider.ConfigCheck { return provider.ConfigCheck{} }
func (d *rbDaemon) Reload(context.Context) (provider.ReloadResult, error) {
	return provider.ReloadResult{}, errors.New("not in this test")
}

// rbRoot is F-ROOTS for the T1 capture, a VM rather than a container
// (no /.dockerenv), with the boot ID CUR, the kernel 7.0.0-42-generic, the
// uptime and the Debian reboot marker listing linux-image-7.0.0-43-generic.
func rbRoot(t *testing.T) string {
	t.Helper()
	root := testRoot(t, rbCapture)
	for _, m := range []string{".dockerenv", "run/.containerenv", "run/systemd/container"} {
		if err := os.Remove(filepath.Join(root, m)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "proc", procBootID), rbCur+"\n")
	writeFile(t, filepath.Join(root, "proc", procOSRelease), rbKernel+"\n")
	writeFile(t, filepath.Join(root, "proc", procUptime), fmt.Sprintf("%.2f 22134.55\n", rbUptime))
	writeFile(t, filepath.Join(root, "var", "run", "reboot-required"), "*** System restart required ***\n")
	writeFile(t, filepath.Join(root, "var", "run", "reboot-required.pkgs"), rbPkg+"\nlinux-base\n")
	return root
}

// rbWorld is the provider ready to reboot: every guard passes until a test
// fails one.
type rbWorld struct {
	t      *testing.T
	root   string
	fh     *svcctltest.Host
	p      *Provider
	ops    *rbOps
	daemon *rbDaemon
	opsDir string
	host   string
	seq    int
}

type rbOpts struct {
	host     string // Default rbHost
	disabled bool   // providers.services.reboot.enabled false
	edit     func(t *testing.T, root string)
}

func newRBWorld(t *testing.T, o rbOpts) *rbWorld {
	t.Helper()
	if o.host == "" {
		o.host = rbHost
	}
	root := rbRoot(t)
	if o.edit != nil {
		o.edit(t, root)
	}
	fh := svcctltest.NewHost(t, rbCapture)
	plat := platform.Info{Init: fh.Init(), OS: platform.OS{ID: "ubuntu", IDLike: []string{"debian"}}}
	p := New(Options{Host: o.host, Logger: slog.New(slog.DiscardHandler), GOOS: "linux", Platform: &plat, Run: fh.Run,
		RunLines: fh.RunLines, ProcRoot: filepath.Join(root, "proc"), RunRoot: filepath.Join(root, "run"),
		EtcRoot: filepath.Join(root, "etc"), HostFS: os.DirFS(root), Executable: ctlExe})
	w := &rbWorld{t: t, root: root, fh: fh, p: p, ops: &rbOps{}, daemon: &rbDaemon{}, opsDir: t.TempDir(), host: o.host}
	w.enabledAtBoot(true)
	w.configure(!o.disabled)
	p.UseOpsDir(w.opsDir)
	p.UseOperations(w.ops)
	p.UseDaemon(w.daemon)
	return w
}

func (w *rbWorld) section(enabled bool) func(any) error {
	return section(w.t, fmt.Sprintf("enabled: true\nreboot:\n  enabled: %v\n  timeout: 5m\n", enabled))
}

// configure sets providers.services.reboot.enabled and preflights.
func (w *rbWorld) configure(enabled bool) {
	w.t.Helper()
	configure(w.t, w.p, w.section(enabled))
	st := w.p.Preflight(context.Background())
	wantAvailable(w.t, st)
	if _, ok := st.Details[DetailRebootUnavailableReason]; ok {
		w.t.Fatalf("rebootAvailable false: %v", st.Details[DetailRebootUnavailableReason])
	}
}

// enabledAtBoot scripts `systemctl is-enabled nexusd.service`.
func (w *rbWorld) enabledAtBoot(on bool) {
	if on {
		w.fh.Answer(rbIsEnabledArgv, svcctl.Result{Output: []byte("enabled\n")}, nil)
	} else {
		w.fh.Answer(rbIsEnabledArgv, svcctl.Result{ExitCode: 1, Output: []byte("disabled\n")}, nil)
	}
}

func (w *rbWorld) funcs() (provider.Binding, provider.DetachedFuncs) {
	w.t.Helper()
	bs := w.p.rebootBindings()
	if len(bs) != 1 || bs[0].Op().ID != spec.Reboot.Op().ID {
		w.t.Fatalf("rebootBindings = %d bindings", len(bs))
	}
	f, ok := bs[0].Detached()
	if !ok {
		w.t.Fatal("the reboot is not detached")
	}
	return bs[0], f
}

func (w *rbWorld) decode(input string) any {
	w.t.Helper()
	b, _ := w.funcs()
	in, err := b.Decode(json.RawMessage(input))
	if err != nil {
		w.t.Fatalf("decode %s: %v", input, err)
	}
	return in
}

// prepare runs Prepare on input as operation id.
func (w *rbWorld) prepare(id, input string) (detach.Spec, error) {
	_, f := w.funcs()
	return f.Prepare(context.Background(), w.decode(input), provider.NewRun(id, nil))
}

func (w *rbWorld) input(id string) rebootHelperInput {
	w.t.Helper()
	var in rebootHelperInput
	ok, err := readHelperJSON(filepath.Join(w.opsDir, id), helperInputFile, &in)
	if err != nil || !ok {
		w.t.Fatalf("input.json of %s: %v %v", id, ok, err)
	}
	return in
}

// ---- The stack ----

// rbProvider serves the control bindings and the reboot binding, whose
// Prepare adds env to the spec (the helper-process switch) when set.
type rbProvider struct {
	*Provider
	env []string
}

func (r rbProvider) Bindings() []provider.Binding {
	s := r.Provider.rebootSpec()
	if r.env != nil {
		prep := s.Prepare
		s.Prepare = func(ctx context.Context, in spec.RebootInput, run *provider.Run) (detach.Spec, error) {
			sp, err := prep(ctx, in, run)
			sp.Env = append(sp.Env, r.env...)
			return sp, err
		}
	}
	return append(r.Provider.controlBindings(), provider.Detached(spec.Reboot, s))
}

type rbStack struct {
	*rbWorld
	fake     *rbFake
	stateDir string
	auditDir string
	boot     atomic.Pointer[string]
	seams    detach.Options // The detach seams, kept across a manager restart
	local    bool           // seams are detachtest.Local's
	env      []string
	log      *slog.Logger
	alog     *audit.Log
	st       *store.Store
	mgr      *ops.Manager
	reg      *core.Registry
	disp     *core.Dispatcher
	ctx      context.Context // The in-process helpers' stop context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	launches atomic.Int32
	out      ctlBuffer
}

type rbStackOpts struct {
	rbOpts
	// local runs the helper as a subprocess under the real op-supervise
	// (detachtest.Local), configured by helper; otherwise in process.
	local  bool
	helper rbHelperConfig
	// wrap adjusts the local seams (TC-SV-ERR31's delayed Signal).
	wrap func(d *detach.Options)
	boot string // F-BOOT; default rbCur
}

func newRBStack(t *testing.T, o rbStackOpts) *rbStack {
	t.Helper()
	s := &rbStack{rbWorld: newRBWorld(t, o.rbOpts), fake: &rbFake{}, stateDir: t.TempDir(), auditDir: t.TempDir(),
		local: o.local, log: slog.New(slog.DiscardHandler)}
	if o.boot == "" {
		o.boot = rbCur
	}
	s.setBoot(o.boot)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.opsDir = filepath.Join(s.stateDir, "ops")
	s.p.UseOpsDir(s.opsDir)
	if o.local {
		if o.helper.ProcRoot == "" {
			o.helper.ProcRoot = filepath.Join(s.root, "proc")
		}
		s.env = []string{rbHelperEnvFor(t, o.helper)}
		s.p.executable = rbTestBinary(t)
		s.seams = detachtest.Local()
		s.seams.ListUnits = func(context.Context) ([]string, error) { return nil, nil }
		if o.wrap != nil {
			o.wrap(&s.seams)
		}
	} else {
		s.seams = detach.Options{
			PollInterval: 10 * time.Millisecond,
			Launch:       s.launch,
			UnitState: func(context.Context, string) (detach.UnitState, error) {
				return detach.UnitState{ActiveState: "active", LoadState: "loaded"}, nil
			},
			Signal:    func(context.Context, string, syscall.Signal) error { return nil },
			ListUnits: func(context.Context) ([]string, error) { return nil, nil },
		}
	}
	alog, err := audit.Open(audit.Options{Dir: s.auditDir, Host: rbHost, Logger: s.log})
	if err != nil {
		t.Fatal(err)
	}
	s.alog = alog
	s.open()
	t.Cleanup(func() {
		s.closeState()
		if err := s.alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
		if t.Failed() {
			t.Logf("helper output:\n%s", s.out.String())
		}
	})
	// Runs first: no in-process helper outlives the test.
	t.Cleanup(func() {
		s.cancel()
		s.wg.Wait()
	})
	return s
}

func (s *rbStack) setBoot(b string) { s.boot.Store(&b) }

// open opens the store, the manager and the Dispatcher over the stack's
// state directory.
func (s *rbStack) open() {
	t := s.t
	t.Helper()
	st, err := store.Open(s.stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.st = st
	exe := ""
	if s.local {
		if exe, err = detachtest.Supervisor(); err != nil {
			t.Fatal(err)
		}
	} else {
		exe = ctlExe
	}
	seams := s.seams
	if seams.PollInterval == 0 {
		seams.PollInterval = 20 * time.Millisecond
	}
	launcher, err := detach.New(detach.Config{OpsDir: s.opsDir, Executable: exe, Logger: s.log}, seams)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: s.alog, Dir: s.opsDir, Host: rbHost, Logger: s.log,
		PollInterval: 20 * time.Millisecond, Detach: launcher,
		BootID: func() (string, error) { return *s.boot.Load(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.mgr = mgr
	reg, err := core.New([]provider.Provider{rbProvider{Provider: s.p, env: s.env}}, core.Options{Host: rbHost,
		Logger: s.log, Audit: s.alog, Ops: mgr,
		Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: s.section(true)}}})
	if err != nil {
		t.Fatal(err)
	}
	s.reg = reg
	s.disp = core.NewDispatcher(reg, core.DispatcherOptions{Logger: s.log, Audit: s.alog})
	// core.New configured the provider from the stack's section; the
	// world's own choice (enabled or not) is the test's.
	s.p.UseOperations(s.ops)
	s.p.UseDaemon(s.daemon)
}

// closeState shuts the manager down and closes the store, as nexusd stops.
func (s *rbStack) closeState() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.mgr.Shutdown(ctx); err != nil {
		s.t.Errorf("manager shutdown: %v", err)
	}
	if err := s.st.Close(); err != nil {
		s.t.Errorf("store close: %v", err)
	}
}

// restart is a nexusd restart in boot: the manager recovers.
func (s *rbStack) restart(boot string) (before, after time.Time) {
	s.t.Helper()
	s.closeState()
	s.setBoot(boot)
	s.open()
	bindings := map[string]provider.Binding{}
	for _, e := range s.reg.Entries() {
		bindings[e.Binding.Op().ID] = e.Binding
	}
	if err := s.mgr.Restore(func(id string) (provider.Binding, bool) { b, ok := bindings[id]; return b, ok }); err != nil {
		s.t.Fatalf("Restore: %v", err)
	}
	before = time.Now()
	done := s.mgr.Recover()
	after = time.Now()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.t.Fatal("recovery did not finish")
	}
	return before, after
}

// launch is the in-process unit: the helper from spec.json's argv with
// F-HELPER seams, then exit.json.
func (s *rbStack) launch(_ context.Context, inv detach.Invocation) error {
	b, err := os.ReadFile(filepath.Join(inv.Dir, detach.SpecFile))
	if err != nil {
		return err
	}
	var sp struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal(b, &sp); err != nil {
		return err
	}
	if len(sp.Argv) < 2 || sp.Argv[1] != HelperCommand {
		return fmt.Errorf("argv %q does not run %s", sp.Argv, HelperCommand)
	}
	s.launches.Add(1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		started := time.Now()
		code := runHelper(s.ctx, sp.Argv[2:], &s.out, &s.out, helperEnv{reboot: s.fake.env(filepath.Join(s.root, "proc"))})
		_ = detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()})
	}()
	return nil
}

var rbCaller = auth.Principal{TokenID: "tokB", Name: "rebooter",
	Scopes: []string{spec.ReadScope, spec.WriteScope, spec.RebootScope}}

func (s *rbStack) invoke(op def.Op, input string) (core.InvokeResult, error) {
	return s.disp.Invoke(context.Background(), core.InvokeRequest{Caller: rbCaller, Surface: core.SurfaceREST, Op: op,
		Input: json.RawMessage(input)})
}

func (s *rbStack) reboot(input string) (core.InvokeResult, error) {
	return s.invoke(spec.Reboot.Op(), input)
}

func (s *rbStack) accepted(input string) string {
	s.t.Helper()
	res, err := s.reboot(input)
	if err != nil || res.Status != 202 || res.OperationID == "" {
		s.t.Fatalf("reboot %s = %d %q (%v), want 202", input, res.Status, res.OperationID, err)
	}
	return res.OperationID
}

func (s *rbStack) wait(id string, limit time.Duration, ok func(store.OpRecord) bool) store.OpRecord {
	s.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		rec, err := s.st.Get(id)
		if err == nil && ok(rec) {
			return rec
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("operation %s: %+v (%v)", id, rec, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *rbStack) records() []systemspec.AuditRecord {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.auditDir, audit.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	var recs []systemspec.AuditRecord
	for line := range strings.Lines(string(b)) {
		var r systemspec.AuditRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			s.t.Fatalf("audit line %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

func (s *rbStack) operations() []store.OpRecord {
	s.t.Helper()
	page, err := s.st.List(store.ListQuery{Limit: 1000})
	if err != nil {
		s.t.Fatal(err)
	}
	return page.Records
}

func (s *rbStack) opDirs() []string {
	s.t.Helper()
	ents, err := os.ReadDir(s.opsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// wantStatus requires err to be a problem of slug with status.
func wantStatus(t *testing.T, err error, status int, slug problem.Slug) *problem.Error {
	t.Helper()
	pe := wantProblem(t, err, slug)
	if pe.Status != status {
		t.Fatalf("%s status %d, want %d", slug, pe.Status, status)
	}
	return pe
}

// wantConfirmHostInvalid requires 400 validation-failed on /confirmHost.
func wantConfirmHostInvalid(t *testing.T, err error) {
	t.Helper()
	pe := wantStatus(t, err, 400, problem.ValidationFailed)
	b, _ := json.Marshal(pe.Extensions["errors"])
	if !strings.Contains(string(b), `"/confirmHost"`) {
		t.Fatalf("errors %s do not name /confirmHost", b)
	}
}

func extJSON(pe *problem.Error, key string) string {
	b, _ := json.Marshal(pe.Extensions[key])
	return string(b)
}

// ---- Tests ----

// TC-SV-102: Admission checks the reboot guards in FR-10 order and reports the first failure. Owner WI 4.4. Requirements: FR-10.
func TestTCSV102_AdmissionChecksTheRebootGuardsInFR(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	// Fail all five.
	s.configure(false)
	s.enabledAtBoot(false)
	s.ops.set("01X")
	s.daemon.fail(true)

	_, err := s.reboot(`{"confirmHost":"h2"}`)
	wantConfirmHostInvalid(t, err)

	_, err = s.reboot(`{"confirmHost":"h1"}`)
	pe := wantStatus(t, err, 409, problem.RebootUnavailable)
	if got := pe.Extensions["configKey"]; got != "providers.services.reboot.enabled" || pe.Extensions["reason"] == "" {
		t.Errorf("reboot-unavailable extensions %v", pe.Extensions)
	}

	s.configure(true)
	_, err = s.reboot(`{"confirmHost":"h1"}`)
	pe = wantStatus(t, err, 409, problem.NexusdNotEnabledAtBoot)
	if pe.Extensions["service"] != "nexusd" || pe.Extensions["state"] != "disabled" {
		t.Errorf("nexusd-not-enabled-at-boot extensions %v", pe.Extensions)
	}

	s.enabledAtBoot(true)
	_, err = s.reboot(`{"confirmHost":"h1"}`)
	pe = wantStatus(t, err, 409, problem.OperationsRunning)
	if got := extJSON(pe, "operationIds"); got != `["01X"]` {
		t.Errorf("operationIds %s, want [\"01X\"]", got)
	}

	s.ops.set()
	_, err = s.reboot(`{"confirmHost":"h1"}`)
	pe = wantStatus(t, err, 422, problem.ConfigInvalid)
	if pe.Extensions["configPath"] != "/etc/nexus/nexusd.yaml" {
		t.Errorf("config-invalid extensions %v", pe.Extensions)
	}

	s.daemon.fail(false)
	s.accepted(`{"confirmHost":"h1"}`)
}

// TC-SV-103: force skips only the operations guard and is an audit fact. Owner WI 4.4. Requirements: FR-10, FR-16.
func TestTCSV103_ForceSkipsOnlyTheOperationsGuardAnd(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	s.ops.set("01X")
	id := s.accepted(`{"confirmHost":"h1","force":true}`)
	var found bool
	for _, r := range s.records() {
		if r.Action == spec.Reboot.Op().ID && r.Outcome == audit.OutcomeRequested {
			found = true
			if r.Facts["force"] != "true" {
				t.Errorf("requested facts %v, want force=true", r.Facts)
			}
		}
	}
	if !found {
		t.Fatal("no requested record")
	}
	_ = id

	s.daemon.fail(true)
	_, err := s.reboot(`{"confirmHost":"h1","force":true}`)
	wantStatus(t, err, 422, problem.ConfigInvalid)

	s.daemon.fail(false)
	s.enabledAtBoot(false)
	_, err = s.reboot(`{"confirmHost":"h1","force":true}`)
	wantStatus(t, err, 409, problem.NexusdNotEnabledAtBoot)
}

// TC-SV-104: whatIf reports every guard and reboots nothing. Owner WI 4.4. Requirements: FR-10, FR-12.
func TestTCSV104_WhatIfReportsEveryGuardAndRebootsNothing(t *testing.T) {
	names := []string{GuardConfirmHost, GuardRebootEnabled, GuardNexusdAtBoot, GuardNoOperations, GuardConfigValid}
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("failing=%v", failing), func(t *testing.T) {
			s := newRBStack(t, rbStackOpts{})
			if failing {
				s.enabledAtBoot(false)
				s.ops.set("01X")
			}
			id := s.accepted(`{"confirmHost":"h1","whatIf":true}`)
			rec := s.wait(id, 15*time.Second, terminal)
			if rec.Status != apitypes.StatusSucceeded {
				t.Fatalf("whatIf ended %s %+v", rec.Status, rec.Error)
			}
			var res spec.RebootResult
			if err := json.Unmarshal(rec.Result, &res); err != nil {
				t.Fatal(err)
			}
			if !res.WhatIf || res.BootID != rbCur || res.UptimeSeconds == nil || *res.UptimeSeconds != rbUptime ||
				res.RebootRequired.State != "true" || !slices.Contains(res.RebootRequired.Packages, rbPkg) {
				t.Errorf("result %s", rec.Result)
			}
			if len(res.Guards) != len(names) {
				t.Fatalf("guards %+v, want five", res.Guards)
			}
			for i, g := range res.Guards {
				if g.Name != names[i] || g.Detail == "" {
					t.Errorf("guard %d = %+v, want %s with a detail", i, g, names[i])
				}
				wantPass := !failing || (g.Name != GuardNexusdAtBoot && g.Name != GuardNoOperations)
				if g.Passed != wantPass {
					t.Errorf("guard %s passed %v, want %v", g.Name, g.Passed, wantPass)
				}
			}
			if res.WouldReboot == nil || *res.WouldReboot == failing {
				t.Errorf("wouldReboot %v, want %v", res.WouldReboot, !failing)
			}
			if res.NexusdEnabledAtBoot == nil || *res.NexusdEnabledAtBoot == failing {
				t.Errorf("nexusdEnabledAtBoot %v, want %v", res.NexusdEnabledAtBoot, !failing)
			}
			wantOps := []string{}
			if failing {
				wantOps = []string{"01X"}
			}
			if !slices.Equal(res.RunningOperations, wantOps) {
				t.Errorf("runningOperations %q, want %q", res.RunningOperations, wantOps)
			}
			s.fake.mu.Lock()
			stages, trig := len(s.fake.stages), len(s.fake.triggers)
			s.fake.mu.Unlock()
			if stages != 0 || trig != 0 {
				t.Errorf("hook stages %d, trigger calls %d; want none", stages, trig)
			}
			if _, ok := rbRecord(t, filepath.Join(s.opsDir, id)); ok {
				t.Error("a reboot record exists")
			}
		})
	}
}

// TC-SV-105: Admission records confirmHost, force and bootId as audit facts. Owner WI 4.4. Requirements: FR-16.
func TestTCSV105_AdmissionRecordsConfirmHostForceAndBootIdAs(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	id := s.accepted(`{"confirmHost":"h1"}`)
	for _, r := range s.records() {
		if r.Action == spec.Reboot.Op().ID && r.Outcome == audit.OutcomeRequested {
			want := map[string]string{"confirmHost": "h1", "force": "false", "bootId": rbCur}
			for k, v := range want {
				if r.Facts[k] != v {
					t.Errorf("fact %s = %q, want %q (%v)", k, r.Facts[k], v, r.Facts)
				}
			}
			if r.TokenID != "tokB" {
				t.Errorf("token %q, want tokB", r.TokenID)
			}
			return
		}
	}
	t.Fatalf("no requested record for %s", id)
}

// TC-SV-106: Prepare re-runs the guards excluding its own operation and snapshots the input. Owner WI 4.4. Requirements: FR-10, FR-11, NFR-7, A-37.
func TestTCSV106_PrepareReRunsTheGuardsExcludingIts(t *testing.T) {
	w := newRBWorld(t, rbOpts{})
	w.ops.set("op-own")
	sp, err := w.prepare("op-own", `{"confirmHost":"h1"}`)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	in := w.input("op-own")
	if in.Mode != "reboot" || in.DelaySeconds != 10 || in.TimeoutSeconds != 300 || in.Init != "systemd" ||
		in.BootID != rbCur || in.KernelRelease != rbKernel || in.RebootRequired.State != "true" ||
		!slices.Contains(in.RebootRequired.Packages, rbPkg) {
		t.Errorf("input.json %+v", in)
	}
	if sp.KillMode != detach.KillProcess || sp.Timeout != 10*time.Second+5*time.Minute+60*time.Second ||
		sp.MaxRuntime != 10*time.Second+5*time.Minute+120*time.Second {
		t.Errorf("spec %+v", sp)
	}
	if sp.Grace != 40*time.Second || sp.Grace <= svcctl.DefaultTimeout {
		t.Errorf("Grace %v, want exactly 40s and above svcctl.DefaultTimeout %v", sp.Grace, svcctl.DefaultTimeout)
	}
	if want := []string{ctlExe, HelperCommand, "run", "--dir", filepath.Join(w.opsDir, "op-own")}; !slices.Equal(sp.Argv, want) {
		t.Errorf("argv %q, want %q", sp.Argv, want)
	}
}

// TC-SV-107: DF-5: a cancel during the delay ends Canceled with no record and no trigger. Owner WI 4.4. Requirements: FR-10, FR-11.
func TestTCSV107_DF5ACancelDuringTheDelay(t *testing.T) {
	t.Run("a during the delay", func(t *testing.T) {
		h := newRBHelper(t, platform.Systemd, 10)
		h.fake.sleepFn = func(n int, ctx context.Context, d time.Duration) error {
			h.inject()
			<-ctx.Done()
			return ctx.Err()
		}
		if code := h.run(); code != rebootStopExit {
			t.Fatalf("exit %d, want %d", code, rebootStopExit)
		}
		if len(h.fake.sleepErrs) != 1 || !errors.Is(h.fake.sleepErrs[0], context.Canceled) {
			t.Errorf("Sleep returned %v, want the ctx error", h.fake.sleepErrs)
		}
		rbWantNothing(t, h)
	})
	t.Run("b at after-delay", func(t *testing.T) {
		h := newRBHelper(t, platform.Systemd, 10)
		h.fake.stageFn = func(stage string) {
			if stage == stageAfterDelay {
				h.inject()
			}
		}
		if code := h.run(); code != rebootStopExit {
			t.Fatalf("exit %d, want %d", code, rebootStopExit)
		}
		rbWantNothing(t, h)
	})
}

// rbWantNothing: no record, no after-record hook, no trigger, no result.
func rbWantNothing(t *testing.T, h *rbHelper) {
	t.Helper()
	if _, ok := rbRecord(t, h.dir); ok {
		t.Error("a reboot record exists")
	}
	if slices.Contains(h.fake.stages, stageAfterRecord) {
		t.Errorf("stages %q include after-record", h.fake.stages)
	}
	if n := h.fake.triggerCount(); n != 0 {
		t.Errorf("trigger calls %d, want 0", n)
	}
	rbNoResult(t, h.dir)
}

// TC-SV-108: DF-5: a cancel before the second check stops the trigger, and one after it does not. Owner WI 4.4. Requirements: FR-10, FR-11, A-28.
func TestTCSV108_DF5ACancelBetweenTheTwo(t *testing.T) {
	for _, stage := range []string{stageAfterCheck1, stageAfterRecord, stageAfterCheck2} {
		t.Run(stage, func(t *testing.T) {
			h := newRBHelper(t, platform.Systemd, 10)
			h.fake.stageFn = func(s string) {
				if s == stage {
					h.inject()
				}
			}
			code := h.run()
			if code != rebootStopExit {
				t.Fatalf("exit %d, want %d", code, rebootStopExit)
			}
			rbNoResult(t, h.dir)
			rec, ok := rbRecord(t, h.dir)
			if stage == stageAfterCheck2 {
				if n := h.fake.triggerCount(); n != 1 {
					t.Errorf("trigger calls %d, want 1", n)
				}
				if !ok || rec.BootID != rbCur {
					t.Errorf("record %+v %v, want kept with %s", rec, ok, rbCur)
				}
				if len(h.fake.sleeps) != 1 {
					t.Errorf("Sleep calls %v, want only the delay", h.fake.sleeps)
				}
				return
			}
			if n := h.fake.triggerCount(); n != 0 {
				t.Errorf("trigger calls %d, want 0", n)
			}
			if !h.fake.recorded[stageAfterRecord] || h.fake.recBoot[stageAfterRecord] != rbCur {
				t.Errorf("at after-record the record was %v (%q), want written with %s", h.fake.recorded[stageAfterRecord],
					h.fake.recBoot[stageAfterRecord], rbCur)
			}
			if ok {
				t.Error("the record is present at exit, want retracted")
			}
			if slices.Contains(h.fake.stages, stageAfterCheck2) {
				t.Errorf("stages %q include after-check-2", h.fake.stages)
			}
		})
	}
}

// TC-SV-109: DF-5: a cancel while the trigger runs or after it (S4, S5) never kills the trigger. Owner WI 4.4. Requirements: FR-10, FR-11, A-28, A-36.
func TestTCSV109_DF5ACancelThatLandsAfter(t *testing.T) {
	const denied = "Failed to reboot system via logind: Access denied"
	type want struct {
		code     int
		record   bool
		result   bool
		sleeps   int
		afterErr bool
	}
	cases := []struct {
		name  string
		setup func(h *rbHelper, trigErr *error)
		want  want
	}{
		{"a S4 trigger succeeds", func(h *rbHelper, trigErr *error) {
			h.fake.triggerFn = func(ctx context.Context, _ []string) (svcctl.Result, error) {
				h.inject()
				*trigErr = ctx.Err()
				return svcctl.Result{}, nil
			}
		}, want{code: rebootStopExit, record: true, sleeps: 1}},
		{"b S4 trigger fails", func(h *rbHelper, trigErr *error) {
			h.fake.triggerFn = func(ctx context.Context, _ []string) (svcctl.Result, error) {
				h.inject()
				*trigErr = ctx.Err()
				return svcctl.Result{ExitCode: 1, Output: []byte(denied)}, nil
			}
		}, want{code: 0, result: true, sleeps: 1}},
		{"c S5 at the hook", func(h *rbHelper, trigErr *error) {
			h.fake.stageFn = func(s string) {
				if s == stageAfterTrigger {
					h.inject()
				}
			}
		}, want{code: rebootStopExit, record: true, sleeps: 2, afterErr: true}},
		{"d S5 during the wait", func(h *rbHelper, trigErr *error) {
			h.fake.sleepFn = func(n int, ctx context.Context, d time.Duration) error {
				if n == 0 {
					return nil
				}
				h.inject()
				<-ctx.Done()
				return ctx.Err()
			}
		}, want{code: rebootStopExit, record: true, sleeps: 2, afterErr: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRBHelper(t, platform.Systemd, 10)
			var trigErr error
			c.setup(h, &trigErr)
			code := h.run()
			if code != c.want.code {
				t.Fatalf("exit %d, want %d", code, c.want.code)
			}
			if n := h.fake.triggerCount(); n != 1 {
				t.Fatalf("trigger calls %d, want 1", n)
			}
			if h.ctx.Err() == nil {
				t.Fatal("no stop was injected")
			}
			// trigErr is the trigger's ctx.Err() just after the stop, while
			// the trigger ran (S4); the cause shows whether the stop ever
			// reached that context (svcctl's own bound cancels it on return).
			if cause := context.Cause(h.fake.trigCtx[0]); trigErr != nil || errors.Is(cause, errRBStop) {
				t.Errorf("a stop reached the trigger: ctx.Err() %v during it, cause %v", trigErr, cause)
			}
			rec, ok := rbRecord(t, h.dir)
			if ok != c.want.record || (ok && rec.BootID != rbCur) {
				t.Errorf("record %+v present %v, want %v", rec, ok, c.want.record)
			}
			if len(h.fake.sleeps) != c.want.sleeps {
				t.Errorf("Sleep calls %v, want %d", h.fake.sleeps, c.want.sleeps)
			}
			if c.want.sleeps == 2 && (h.fake.sleeps[1] != 300*time.Second ||
				!errors.Is(h.fake.sleepErrs[1], context.Canceled)) {
				t.Errorf("post-trigger Sleep(%v) = %v, want Sleep(300s) ending with the ctx error", h.fake.sleeps[1],
					h.fake.sleepErrs[1])
			}
			if !c.want.result {
				rbNoResult(t, h.dir)
				return
			}
			res := readResult(t, h.dir)
			if res.Error == nil || res.Error.Slug != problem.RebootDidNotHappen || extString(t, res.Error, "stage") != "trigger" ||
				string(res.Error.Ext["exitCode"]) != "1" {
				t.Errorf("result %+v, want reboot-did-not-happen at stage trigger, exit code 1", res.Error)
			}
		})
	}
}

// TC-SV-110: A trigger that fails retracts the record and ends reboot-did-not-happen at stage trigger. Owner WI 4.4. Requirements: FR-11.
func TestTCSV110_ATriggerThatFailsRetractsTheRecord(t *testing.T) {
	out := []byte("Failed to reboot system via logind: Access denied\n" + strings.Repeat("x", 5<<10))
	cases := []struct {
		name string
		res  svcctl.Result
		err  error
	}{
		{"exit 1", svcctl.Result{ExitCode: 1, Output: out}, nil},
		{"not started", svcctl.Result{}, fmt.Errorf("%w: fork/exec /usr/bin/systemctl: %w", svcctl.ErrNotStarted, syscall.ENOENT)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRBHelper(t, platform.Systemd, 10)
			h.fake.triggerFn = func(context.Context, []string) (svcctl.Result, error) { return c.res, c.err }
			if code := h.run(); code != 0 {
				t.Fatalf("exit %d, want 0", code)
			}
			if _, ok := rbRecord(t, h.dir); ok {
				t.Error("the record is present, want retracted")
			}
			res := readResult(t, h.dir)
			if res.Error == nil || res.Error.Slug != problem.RebootDidNotHappen || extString(t, res.Error, "stage") != "trigger" {
				t.Fatalf("result %+v", res.Error)
			}
			if c.err == nil {
				if string(res.Error.Ext["exitCode"]) != "1" {
					t.Errorf("exitCode %s, want 1", res.Error.Ext["exitCode"])
				}
				if !strings.Contains(res.Error.Detail, string(out[:2<<10])) || strings.Contains(res.Error.Detail, string(out[:2<<10+1])) {
					t.Errorf("detail does not quote exactly the first 2 KiB of the output (%d bytes)", len(res.Error.Detail))
				}
			} else {
				if string(res.Error.Ext["exitCode"]) != "null" {
					t.Errorf("exitCode %s, want null", res.Error.Ext["exitCode"])
				}
				if !strings.Contains(res.Error.Detail, syscall.ENOENT.Error()) {
					t.Errorf("detail %q does not carry the tool error", res.Error.Detail)
				}
			}
			// Finalize: Failed with that problem, by a normal exit (R5).
			w := newRBWorld(t, rbOpts{})
			w.opsDir = filepath.Dir(h.dir)
			w.p.UseOpsDir(w.opsDir)
			_, f := w.funcs()
			_, err := f.Finalize(context.Background(), w.decode(`{"confirmHost":"h1"}`), detach.Exit{ExitCode: 0},
				provider.NewRun(filepath.Base(h.dir), nil))
			pe := wantProblem(t, err, problem.RebootDidNotHappen)
			if pe.Extensions["stage"] == nil {
				t.Errorf("problem %+v has no stage", pe)
			}
		})
	}
}

// TC-SV-111: A trigger that returns but never takes the host down ends reboot-did-not-happen at stage wait. Owner WI 4.4. Requirements: FR-11.
func TestTCSV111_ATriggerThatReturnsButNeverTakes(t *testing.T) {
	h := newRBHelper(t, platform.Systemd, 10)
	h.fake.sleepFn = func(int, context.Context, time.Duration) error { return nil }
	if code := h.run(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if want := []time.Duration{10 * time.Second, 300 * time.Second}; !slices.Equal(h.fake.sleeps, want) {
		t.Errorf("Sleep calls %v, want %v", h.fake.sleeps, want)
	}
	if _, ok := rbRecord(t, h.dir); ok {
		t.Error("the record is present, want retracted")
	}
	res := readResult(t, h.dir)
	if res.Error == nil || res.Error.Slug != problem.RebootDidNotHappen || extString(t, res.Error, "stage") != "wait" ||
		extString(t, res.Error, "bootId") != rbCur {
		t.Errorf("result %+v, want reboot-did-not-happen at stage wait with bootId", res.Error)
	}
}

// TC-SV-112: The trigger is RebootArgv through svcctl's runner. Owner WI 4.4. Requirements: FR-10, FR-12, CON-2.
func TestTCSV112_TheTriggerIsRebootArgvThroughSvcctlsRunner(t *testing.T) {
	for init, want := range map[platform.InitSystem][]string{
		platform.Systemd: {"/usr/bin/systemctl", "reboot"},
		platform.OpenRC:  {"/sbin/reboot"},
	} {
		t.Run(string(init), func(t *testing.T) {
			h := newRBHelper(t, init, 10)
			h.fake.triggerFn = func(context.Context, []string) (svcctl.Result, error) {
				h.inject()
				return svcctl.Result{}, nil
			}
			h.run()
			if len(h.fake.triggers) != 1 || !slices.Equal(h.fake.triggers[0], want) {
				t.Fatalf("triggers %q, want exactly %q", h.fake.triggers, want)
			}
			for _, a := range h.fake.triggers[0] {
				for _, bad := range []string{"-f", "--force", "poweroff", "halt", "kexec", "soft-reboot", "pct", "qm"} {
					if a == bad {
						t.Errorf("argv %q contains %q", h.fake.triggers[0], bad)
					}
				}
			}
		})
	}
}

// rbAlpineRoot is TC-SV-113's F-ROOTS tree: the new kernel's osrelease and
// modules directory, no reboot marker and no container marker.
func rbAlpineRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "proc", procOSRelease), rbNewKernel+"\n")
	writeFile(t, filepath.Join(root, "lib", "modules", rbNewKernel, "modules.dep"), "")
	return root
}

// TC-SV-113: AfterReboot builds the result from Rebooted, the new kernel and the reboot-required state. Owner WI 4.4. Requirements: FR-11, A-41.
func TestTCSV113_AfterRebootBuildsTheResultFromRebootedThe(t *testing.T) {
	root := rbAlpineRoot(t)
	plat := platform.Info{Init: platform.OpenRC, OS: platform.OS{ID: "alpine"}}
	var execs atomic.Int32
	p := New(Options{Host: rbHost, Logger: slog.New(slog.DiscardHandler), GOOS: "linux", Platform: &plat,
		Run: func(context.Context, []string) (svcctl.Result, error) {
			execs.Add(1)
			return svcctl.Result{}, errors.New("no exec in AfterReboot")
		},
		ProcRoot: filepath.Join(root, "proc"), HostFS: os.DirFS(root)})
	opsDir := t.TempDir()
	p.UseOpsDir(opsDir)
	dir := filepath.Join(opsDir, "op-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeHelperJSON(dir, helperInputFile, rebootHelperInput{Mode: helperModeReboot, Init: "systemd",
		DelaySeconds: 10, TimeoutSeconds: 300, BootID: rbPrev, KernelRelease: rbKernel,
		RebootRequired: spec.RebootRequired{State: "true"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	t1 := t0.Add(95 * time.Second)
	rb := provider.Rebooted{PreviousBootID: rbPrev, BootID: rbCur, RequestedAt: t0, ReadyAt: t1}
	in := spec.RebootInput{ConfirmHost: rbHost}
	run := provider.NewRun("op-1", nil)
	got, err := p.afterReboot(context.Background(), in, rb, run)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || got.PreviousBootID != rbPrev || got.BootID != rbCur || got.RebootRequestedAt == nil ||
		!got.RebootRequestedAt.Equal(t0) || got.UpAt == nil || !got.UpAt.Equal(t1) ||
		got.PreviousKernelRelease != rbKernel || got.KernelRelease != rbNewKernel || got.RebootRequired.State != "false" {
		b, _ := json.Marshal(got)
		t.Fatalf("AfterReboot = %s", b)
	}
	again, err := p.afterReboot(context.Background(), in, rb, run)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Errorf("a second call = %+v, %v; want an equal value", again, err)
	}
	if n := execs.Load(); n != 0 {
		t.Errorf("execs %d, want none", n)
	}
}

// TC-SV-114: End to end through the real manager: the record is held through shutdown and the op is Succeeded in the next boot with both boot IDs. Owner WI 4.4. Requirements: FR-10, FR-11, FR-16, NFR-7, CON-4, CON-5.
func TestTCSV114_EndToEndThroughTheRealManager(t *testing.T) {
	s := newRBStack(t, rbStackOpts{local: true, boot: rbPrev})
	writeFile(t, filepath.Join(s.root, "proc", procBootID), rbPrev+"\n")
	id := s.accepted(`{"confirmHost":"h1","delaySeconds":5}`)
	accepted := time.Now()
	// The admission record and the Running record were durable before
	// the 202.
	var requested bool
	for _, r := range s.records() {
		requested = requested || (r.Action == spec.Reboot.Op().ID && r.Outcome == audit.OutcomeRequested)
	}
	if !requested {
		t.Fatal("no requested audit record at the 202")
	}
	if rec, err := s.st.Get(id); err != nil || (rec.Status != apitypes.StatusRunning && rec.Status != apitypes.StatusQueued) {
		t.Fatalf("operation at the 202: %+v %v", rec, err)
	}
	dir := filepath.Join(s.opsDir, id)
	waitFile(t, filepath.Join(dir, "trigger-called"), 15*time.Second)
	if fi, err := os.Stat(filepath.Join(dir, "trigger-called")); err != nil || fi.ModTime().Before(accepted.Add(-time.Second)) {
		t.Errorf("trigger-called %v (%v), want after the 202 at %v", fi.ModTime(), err, accepted)
	}
	if rec, ok := rbRecord(t, dir); !ok || rec.BootID != rbPrev {
		t.Fatalf("record %+v %v, want written in %s before the trigger", rec, ok, rbPrev)
	}

	// The shutdown: SIGTERM the supervisor, with no cancel request.
	sent := time.Now()
	if err := s.seams.Signal(context.Background(), detach.Unit(id), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	x := rbWaitExit(t, dir, 2*time.Second)
	if !x.Interrupted || x.Canceled || x.ExitCode != rebootStopExit || x.Signal != "" {
		t.Errorf("exit.json %+v, want interrupted with the helper's own exit 143", x)
	}
	if took := x.Ended.Sub(sent); took > time.Second {
		t.Errorf("the helper exited %v after the SIGTERM, want within 1 s", took)
	}
	if _, ok := rbRecord(t, dir); !ok {
		t.Fatal("the record is gone after the shutdown (S5 keeps it)")
	}
	time.Sleep(200 * time.Millisecond)
	if rec, _ := s.st.Get(id); rec.Status != apitypes.StatusRunning {
		t.Fatalf("after the shutdown: %s %+v, want Running (R3)", rec.Status, rec.Error)
	}

	// The next boot.
	writeFile(t, filepath.Join(s.root, "proc", procBootID), rbCur+"\n")
	writeFile(t, filepath.Join(s.root, "proc", procOSRelease), rbNewKernel+"\n")
	before, after := s.restart(rbCur)
	rec := s.wait(id, 15*time.Second, terminal)
	if rec.Status != apitypes.StatusSucceeded {
		t.Fatalf("after the reboot: %s %+v, want Succeeded", rec.Status, rec.Error)
	}
	var res spec.RebootResult
	if err := json.Unmarshal(rec.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.PreviousBootID != rbPrev || res.BootID != rbCur || res.UpAt == nil || res.UpAt.Before(before) ||
		res.UpAt.After(after) || res.PreviousKernelRelease != rbKernel || res.KernelRelease != rbNewKernel || !res.Changed {
		t.Errorf("result %s, want %s to %s up in [%v, %v]", rec.Result, rbPrev, rbCur, before, after)
	}
	var terminalFacts map[string]string
	for _, r := range s.records() {
		if r.OperationID == id && apitypes.OperationStatus(r.Outcome).Terminal() {
			terminalFacts = r.Facts
			if apitypes.OperationStatus(r.Outcome) != apitypes.StatusSucceeded {
				t.Errorf("terminal record %+v", r)
			}
		}
	}
	if terminalFacts["previousBootId"] != rbPrev || terminalFacts["bootId"] != rbCur {
		t.Errorf("terminal facts %v", terminalFacts)
	}
}

// rbWaitExit waits for dir's exit.json.
func rbWaitExit(t *testing.T, dir string, limit time.Duration) detach.Exit {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		x, ok, err := detach.ReadExit(dir)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return x
		}
		if time.Now().After(deadline) {
			t.Fatalf("no exit.json in %s within %v", dir, limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TC-SV-116: The reboot lane refuses a second reboot. Owner WI 4.4. Requirements: FR-10.
func TestTCSV116_TheRebootLaneRefusesASecondReboot(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	s.fake.sleepFn = func(_ int, ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	first := s.accepted(`{"confirmHost":"h1","delaySeconds":300}`)
	s.wait(first, 5*time.Second, func(r store.OpRecord) bool { return r.Status == apitypes.StatusRunning })
	_, err := s.reboot(`{"confirmHost":"h1","force":true}`)
	pe := wantStatus(t, err, 409, problem.Conflict)
	if pe.Extensions["blockingOperationId"] != first {
		t.Errorf("blockingOperationId %v, want %s", pe.Extensions["blockingOperationId"], first)
	}
	if n := len(s.operations()); n != 1 {
		t.Errorf("%d operations, want 1", n)
	}
	_, f := s.funcs()
	lane, err := f.Lane(context.Background(), s.decode(`{"confirmHost":"h1"}`))
	if err != nil || lane != (provider.Lane{Key: "reboot", Policy: provider.LaneReject}) {
		t.Errorf("Lane = %+v, %v", lane, err)
	}
}

// TC-SV-150: Subprocess, real trigger process: a cancel while the trigger runs never kills it (S4, A-36). Owner WI 4.4. Requirements: FR-10, FR-11, CON-5, A-28, A-36, A-37.
func TestTCSV150_ARealTriggerProcessIsNeverKilledByA(t *testing.T) {
	const okScript = "touch {dir}/trigger-started; sleep 1; touch {dir}/trigger-done"
	const failScript = okScript + "; exit 1"
	for _, c := range []struct {
		name   string
		script string
	}{{"a child exits 0", okScript}, {"b child exits 1", failScript}} {
		t.Run(c.name, func(t *testing.T) {
			w := newRBWorld(t, rbOpts{})
			w.p.executable = rbTestBinary(t)
			sp, err := w.prepare("op-1", `{"confirmHost":"h1","delaySeconds":5}`)
			if err != nil {
				t.Fatal(err)
			}
			sp.Env = []string{rbHelperEnvFor(t, rbHelperConfig{ProcRoot: filepath.Join(w.root, "proc"), Script: c.script})}
			if sp.Grace != 40*time.Second {
				t.Fatalf("Grace %v, want the production 40 s", sp.Grace)
			}
			exe, err := detachtest.Supervisor()
			if err != nil {
				t.Fatal(err)
			}
			l, err := detach.New(detach.Config{OpsDir: w.opsDir, Executable: exe, Logger: slog.New(slog.DiscardHandler)},
				detachtest.Local())
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Start(context.Background(), "op-1", sp); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(w.opsDir, "op-1")
			waitFile(t, filepath.Join(dir, "trigger-started"), 5*time.Second)
			sent := time.Now()
			if err := l.Cancel(context.Background(), "op-1"); err != nil {
				t.Fatal(err)
			}
			x := rbWaitExit(t, dir, 5*time.Second)
			done, err := os.Stat(filepath.Join(dir, "trigger-done"))
			if err != nil {
				t.Fatalf("trigger-done: %v: the trigger was killed", err)
			}
			if x.Ended.Before(done.ModTime()) {
				t.Errorf("the helper ended at %v, before trigger-done at %v", x.Ended, done.ModTime())
			}
			if x.Signal != "" || x.Ended.Sub(sent) > 3*time.Second {
				t.Errorf("exit %+v %v after the cancel: want the helper's own exit within 3 s", x, x.Ended.Sub(sent))
			}
			if !x.Canceled {
				t.Errorf("exit.json %+v, want canceled", x)
			}
			rec, ok := rbRecord(t, dir)
			if c.script == okScript {
				if !ok || rec.BootID != rbCur || x.ExitCode != rebootStopExit {
					t.Errorf("record %+v %v, exit %d; want kept and 143", rec, ok, x.ExitCode)
				}
				rbNoResult(t, dir)
				return
			}
			if ok || x.ExitCode != 0 {
				t.Errorf("record present %v, exit %d; want retracted and 0", ok, x.ExitCode)
			}
			res := readResult(t, dir)
			if res.Error == nil || res.Error.Slug != problem.RebootDidNotHappen || extString(t, res.Error, "stage") != "trigger" ||
				string(res.Error.Ext["exitCode"]) != "1" {
				t.Errorf("result %+v", res.Error)
			}
		})
	}
	t.Run("c through the manager", func(t *testing.T) {
		s := newRBStack(t, rbStackOpts{local: true, helper: rbHelperConfig{Script: failScript}})
		id := s.accepted(`{"confirmHost":"h1","delaySeconds":5}`)
		dir := filepath.Join(s.opsDir, id)
		waitFile(t, filepath.Join(dir, "trigger-started"), 5*time.Second)
		l, err := detach.New(detach.Config{OpsDir: s.opsDir, Logger: s.log}, s.seams)
		if err != nil {
			t.Fatal(err)
		}
		sent := time.Now()
		if err := l.Cancel(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		rec := s.wait(id, 5*time.Second, terminal)
		if rec.Status != apitypes.StatusCanceled || time.Since(sent) > 5*time.Second {
			t.Fatalf("ended %s %+v after %v, want Canceled within 5 s", rec.Status, rec.Error, time.Since(sent))
		}
		if !fileExists(filepath.Join(dir, "trigger-done")) {
			t.Error("trigger-done is missing: the trigger was killed")
		}
		rbWantLaneFree(t, s, id, dir)
	})
}

// rbWantLaneFree: the record is gone, the manager no longer lists the op,
// and a whatIf reboot is admitted.
func rbWantLaneFree(t *testing.T, s *rbStack, id, dir string) {
	t.Helper()
	if _, ok := rbRecord(t, dir); ok {
		t.Error("reboot-requested is present")
	}
	active, err := s.mgr.Active()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.ID == id {
			t.Errorf("Active lists %s", id)
		}
	}
	res, err := s.reboot(`{"confirmHost":"h1","whatIf":true}`)
	if err != nil || res.Status != 202 {
		t.Fatalf("whatIf reboot = %d, %v; want 202 (the lane is free)", res.Status, err)
	}
	if rec := s.wait(res.OperationID, 15*time.Second, terminal); rec.Status != apitypes.StatusSucceeded {
		t.Errorf("whatIf ended %s %+v", rec.Status, rec.Error)
	}
}

// TC-SV-E16: LostDetail names the service and action, and for the reboot the boot-ID comparison. Owner WI 4.4. Requirements: FR-7, FR-11.
func TestTCSVE16_LostDetailNamesTheServiceAndActionAnd(t *testing.T) {
	w := newRBWorld(t, rbOpts{})
	for _, b := range w.p.controlBindings() {
		if b.Op().ID != spec.Restart.Op().ID {
			continue
		}
		f, _ := b.Detached()
		in, err := b.Decode(json.RawMessage(`{"name":"nx-svc-test"}`))
		if err != nil {
			t.Fatal(err)
		}
		d := f.LostDetail(provider.NewRun("op-1", nil), in)
		if !strings.Contains(d, "nx-svc-test") || !strings.Contains(d, "restart") {
			t.Errorf("control LostDetail %q", d)
		}
	}
	if _, err := w.prepare("op-2", `{"confirmHost":"h1"}`); err != nil {
		t.Fatal(err)
	}
	_, f := w.funcs()
	d := f.LostDetail(provider.NewRun("op-2", nil), w.decode(`{"confirmHost":"h1"}`))
	for _, want := range []string{"the host may or may not have rebooted", "/proc/sys/kernel/random/boot_id", "previousBootId", rbCur} {
		if !strings.Contains(d, want) {
			t.Errorf("reboot LostDetail %q does not contain %q", d, want)
		}
	}
}

// TC-SV-E17: A guard that passes at admission but fails in Prepare fails the operation before any trigger. Owner WI 4.4. Requirements: FR-10.
func TestTCSVE17_AGuardThatPassesAtAdmissionBut(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	s.ops.script(nil, []string{"01Y"})
	id := s.accepted(`{"confirmHost":"h1"}`)
	rec := s.wait(id, 15*time.Second, terminal)
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.OperationsRunning {
		t.Fatalf("ended %s %+v, want Failed operations-running", rec.Status, rec.Error)
	}
	if got, _ := json.Marshal(rec.Error.Extensions["operationIds"]); string(got) != `["01Y"]` {
		t.Errorf("operationIds %s, want [\"01Y\"]", got)
	}
	if fileExists(filepath.Join(s.opsDir, id, helperInputFile)) {
		t.Error("input.json was written")
	}
	if n := s.launches.Load(); n != 0 {
		t.Errorf("launches %d, want 0", n)
	}
}

// TC-SV-E18: confirmHost differing only by case or surrounding space is refused. Owner WI 4.4. Requirements: FR-10.
func TestTCSVE18_ConfirmHostDifferingOnlyByCaseOrSurrounding(t *testing.T) {
	s := newRBStack(t, rbStackOpts{rbOpts: rbOpts{host: "home-server"}})
	for _, v := range []string{"Home-server", "home-server ", "home-server.lan"} {
		_, err := s.reboot(fmt.Sprintf(`{"confirmHost":%q}`, v))
		wantConfirmHostInvalid(t, err)
	}
	if n := len(s.operations()); n != 0 {
		t.Errorf("%d operations, want none", n)
	}
	if dirs := s.opDirs(); len(dirs) != 0 {
		t.Errorf("operation directories %q, want none", dirs)
	}
}

// TC-SV-E20: delaySeconds defaults to 10 and accepts its bounds. Owner WI 4.4. Requirements: FR-10, NFR-7.
func TestTCSVE20_DelaySecondsDefaultsTo10AndAcceptsIts(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	s.fake.sleepFn = func(int, context.Context, time.Duration) error { return nil }
	for _, c := range []struct {
		input string
		want  int
	}{{`{"confirmHost":"h1"}`, 10}, {`{"confirmHost":"h1","delaySeconds":5}`, 5}, {`{"confirmHost":"h1","delaySeconds":300}`, 300}} {
		id := s.accepted(c.input)
		s.wait(id, 15*time.Second, terminal)
		if in := s.input(id); in.DelaySeconds != c.want {
			t.Errorf("%s: delaySeconds %d, want %d", c.input, in.DelaySeconds, c.want)
		}
	}
}

// TC-SV-ERR25: Each reboot guard's refusal has its slug and extensions, creates nothing and is audited refused. Owner WI 4.4. Requirements: FR-10, FR-16, A-15.
func TestTCSVERR25_EachRebootGuardsRefusalHasItsSlug(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		fail   func(s *rbStack)
		status int
		slug   problem.Slug
		check  func(t *testing.T, pe *problem.Error)
	}{
		{"confirmHost", `{"confirmHost":"h2"}`, func(*rbStack) {}, 400, problem.ValidationFailed, nil},
		{"enabled", `{"confirmHost":"h1"}`, func(s *rbStack) { s.configure(false) }, 409, problem.RebootUnavailable,
			func(t *testing.T, pe *problem.Error) {
				if pe.Extensions["configKey"] != rebootConfigKey || pe.Extensions["reason"] == "" {
					t.Errorf("extensions %v", pe.Extensions)
				}
			}},
		{"nexusd at boot", `{"confirmHost":"h1"}`, func(s *rbStack) { s.enabledAtBoot(false) }, 409,
			problem.NexusdNotEnabledAtBoot, func(t *testing.T, pe *problem.Error) {
				if pe.Extensions["service"] != "nexusd" || pe.Extensions["state"] != "disabled" {
					t.Errorf("extensions %v", pe.Extensions)
				}
			}},
		{"operations", `{"confirmHost":"h1"}`, func(s *rbStack) { s.ops.set("01X") }, 409, problem.OperationsRunning,
			func(t *testing.T, pe *problem.Error) {
				if got := extJSON(pe, "operationIds"); got != `["01X"]` {
					t.Errorf("operationIds %s", got)
				}
			}},
		{"config", `{"confirmHost":"h1"}`, func(s *rbStack) { s.daemon.fail(true) }, 422, problem.ConfigInvalid,
			func(t *testing.T, pe *problem.Error) {
				if pe.Extensions["configPath"] == nil || pe.Extensions["errors"] == nil {
					t.Errorf("extensions %v", pe.Extensions)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRBStack(t, rbStackOpts{})
			c.fail(s)
			_, err := s.reboot(c.input)
			pe := wantStatus(t, err, c.status, c.slug)
			if c.check != nil {
				c.check(t, pe)
			}
			if n := len(s.operations()); n != 0 {
				t.Errorf("%d operations, want none", n)
			}
			if dirs := s.opDirs(); len(dirs) != 0 {
				t.Errorf("operation directories %q, want none", dirs)
			}
			var refused bool
			for _, r := range s.records() {
				if r.Action == spec.Reboot.Op().ID && r.Outcome == audit.OutcomeRefused {
					refused = true
					if r.Problem != string(c.slug) || r.Facts["confirmHost"] == "" || r.Facts["force"] != "false" {
						t.Errorf("refused record %+v", r)
					}
				}
			}
			if !refused {
				t.Error("no refused audit record")
			}
			if n := s.fake.triggerCount(); n != 0 {
				t.Errorf("trigger calls %d", n)
			}
		})
	}
}

// TC-SV-ERR26: SIGTERM during the delay exits 143 with no record and no trigger. Owner WI 4.4. Requirements: FR-10, FR-11.
func TestTCSVERR26_SIGTERMDuringTheDelayExits143With(t *testing.T) {
	p := startRBProc(t, 30, rbHelperConfig{RealDelay: true})
	time.Sleep(300 * time.Millisecond)
	sent := time.Now()
	p.signal(syscall.SIGTERM)
	if code := p.wait(5 * time.Second); code != rebootStopExit {
		t.Fatalf("exit %d, want 143", code)
	}
	if took := p.end.Sub(sent); took > time.Second {
		t.Errorf("exited %v after the SIGTERM, want within 1 s", took)
	}
	if fileExists(filepath.Join(p.dir, detach.RebootFile)) || fileExists(filepath.Join(p.dir, "trigger-called")) {
		t.Error("a record or a trigger marker exists")
	}
}

// TC-SV-ERR27: The reboot answers 503 capability-unavailable when the provider is unavailable. Owner WI 4.4. Requirements: FR-3, FR-12, CON-6.
func TestTCSVERR27_TheRebootAnswers503CapabilityUnavailableWhen(t *testing.T) {
	// The Proxmox marker is planted after the world's own preflight, and
	// the stack reopened, so core's preflight finds it.
	s := newRBStack(t, rbStackOpts{})
	if err := os.MkdirAll(filepath.Join(s.root, "etc", "pve"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.closeState()
	s.open()
	calls := len(s.fh.Calls())
	_, err := s.reboot(`{"confirmHost":"h1"}`)
	pe := wantStatus(t, err, 503, problem.CapabilityUnavailable)
	if r, _ := pe.Extensions["reason"].(string); !strings.Contains(r, "Proxmox") {
		t.Errorf("reason %v does not give the Proxmox reason", pe.Extensions)
	}
	_, err = s.invoke(spec.Start.Op(), `{"name":"nx-svc-test"}`)
	wantStatus(t, err, 503, problem.CapabilityUnavailable)
	if got := s.fh.Calls(); len(got) != calls {
		t.Errorf("F-HOSTS calls %v after the requests, want none", got[calls:])
	}
	for _, r := range s.records() {
		if r.Outcome == audit.OutcomeRequested {
			t.Errorf("requested record %+v", r)
		}
	}
}

// TC-SV-ERR29: Out-of-range delaySeconds is refused with 400. Owner WI 4.4. Requirements: FR-10.
func TestTCSVERR29_OutOfRangeDelaySecondsIsRefusedWith(t *testing.T) {
	s := newRBStack(t, rbStackOpts{})
	for _, v := range []int{4, 301, -1} {
		_, err := s.reboot(fmt.Sprintf(`{"confirmHost":"h1","delaySeconds":%d}`, v))
		pe := wantStatus(t, err, 400, problem.ValidationFailed)
		if b, _ := json.Marshal(pe.Extensions); !strings.Contains(string(b), "delaySeconds") {
			t.Errorf("%d: problem %s does not name delaySeconds", v, b)
		}
	}
	if s.ops.count() != 0 || s.daemon.count() != 0 {
		t.Errorf("F-OPS calls %d, F-DAEMON calls %d; want none", s.ops.count(), s.daemon.count())
	}
}

// TC-SV-ERR30: A real cancel (request file plus SIGTERM) before the second check retracts the record and never triggers, and one after it lets the trigger run once. Owner WI 4.4. Requirements: FR-10, FR-11, CON-5, A-28, A-37.
func TestTCSVERR30_ARealCancelRequestFilePlusSIGTERM(t *testing.T) {
	for _, stage := range []string{stageAfterCheck1, stageAfterRecord, stageAfterCheck2} {
		t.Run(stage, func(t *testing.T) {
			p := startRBProc(t, 5, rbHelperConfig{Stage: stage})
			waitFile(t, filepath.Join(p.dir, "hook-"+stage), 15*time.Second)
			if err := detach.RequestCancel(p.dir); err != nil {
				t.Fatal(err)
			}
			sent := time.Now()
			p.signal(syscall.SIGTERM)
			time.Sleep(200 * time.Millisecond)
			if !p.alive() {
				t.Fatal("the helper died of the SIGTERM in the hook")
			}
			released := time.Now()
			writeFile(t, filepath.Join(p.dir, "hook-"+stage+".release"), "")
			if code := p.wait(5 * time.Second); code != rebootStopExit {
				t.Fatalf("exit %d, want 143", code)
			}
			if took := p.end.Sub(released); took > time.Second {
				t.Errorf("exited %v after the release, want within 1 s", took)
			}
			t.Logf("SIGTERM to exit: %v", p.end.Sub(sent))
			rec, ok := rbRecord(t, p.dir)
			if stage == stageAfterCheck2 {
				if n := triggerLines(t, p.dir); n != 1 {
					t.Errorf("trigger-called written %d times, want once", n)
				}
				if !ok || rec.BootID != rbCur {
					t.Errorf("record %+v %v, want kept", rec, ok)
				}
				return
			}
			if ok || fileExists(filepath.Join(p.dir, "trigger-called")) {
				t.Errorf("record %v, trigger-called %v; want neither", ok, fileExists(filepath.Join(p.dir, "trigger-called")))
			}
		})
	}
}

// TC-SV-ERR31: Through the real manager, a cancel accepted before the record whose SIGTERM lands after it ends Canceled, not held. Owner WI 4.4. Requirements: FR-10, FR-11, CON-4, CON-5.
func TestTCSVERR31_ThroughTheRealManagerACancelAccepted(t *testing.T) {
	var dirOf atomic.Pointer[string]
	wrap := func(d *detach.Options) {
		real := d.Signal
		d.Signal = func(ctx context.Context, unit string, sig syscall.Signal) error {
			go func() {
				dir := *dirOf.Load()
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
					if fileExists(filepath.Join(dir, "hook-"+stageAfterRecord)) {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				_ = real(context.WithoutCancel(ctx), unit, sig)
				_ = os.WriteFile(filepath.Join(dir, "sigterm-forwarded"), nil, 0o600)
			}()
			return nil
		}
	}
	// The hook holds at after-check-1, then at after-record.
	s := newRBStack(t, rbStackOpts{local: true, helper: rbHelperConfig{Stage: "after-check-1+after-record"}, wrap: wrap})
	id := s.accepted(`{"confirmHost":"h1","delaySeconds":5}`)
	dir := filepath.Join(s.opsDir, id)
	dirOf.Store(&dir)
	waitFile(t, filepath.Join(dir, "hook-"+stageAfterCheck1), 15*time.Second)
	res, err := s.invoke(systemspec.OperationsCancel.Op(), fmt.Sprintf(`{"id":%q}`, id))
	if err != nil || res.Status != 200 {
		t.Fatalf("cancel = %d, %v; want accepted", res.Status, err)
	}
	waitFile(t, filepath.Join(dir, detach.CancelFile), 5*time.Second)
	writeFile(t, filepath.Join(dir, "hook-"+stageAfterCheck1+".release"), "")
	waitFile(t, filepath.Join(dir, "sigterm-forwarded"), 15*time.Second)
	time.Sleep(200 * time.Millisecond)
	released := time.Now()
	writeFile(t, filepath.Join(dir, "hook-"+stageAfterRecord+".release"), "")
	rec := s.wait(id, 5*time.Second, terminal)
	if rec.Status != apitypes.StatusCanceled {
		t.Fatalf("ended %s %+v, want Canceled", rec.Status, rec.Error)
	}
	if took := time.Since(released); took > 5*time.Second {
		t.Errorf("Canceled %v after the release", took)
	}
	if fileExists(filepath.Join(dir, "trigger-called")) {
		t.Error("trigger-called exists")
	}
	rbWantLaneFree(t, s, id, dir)
	var sawRequested bool
	for _, r := range s.records() {
		if r.Action == spec.Reboot.Op().ID && r.Outcome == audit.OutcomeRequested {
			sawRequested = true
		}
		if r.OperationID == id && r.Facts["previousBootId"] != "" {
			t.Errorf("record %+v names a reboot", r)
		}
	}
	if !sawRequested {
		t.Error("no requested record")
	}
}

// TC-SV-ERR32: SIGTERM with no cancel request after the record leaves the record, and triggers only after the second check. Owner WI 4.4. Requirements: FR-10, FR-11.
func TestTCSVERR32_SIGTERMWithNoCancelRequestBetweenThe(t *testing.T) {
	for _, stage := range []string{stageAfterRecord, stageAfterCheck2} {
		t.Run(stage, func(t *testing.T) {
			p := startRBProc(t, 5, rbHelperConfig{Stage: stage})
			waitFile(t, filepath.Join(p.dir, "hook-"+stage), 15*time.Second)
			p.signal(syscall.SIGTERM)
			time.Sleep(200 * time.Millisecond)
			released := time.Now()
			writeFile(t, filepath.Join(p.dir, "hook-"+stage+".release"), "")
			if code := p.wait(5 * time.Second); code != rebootStopExit {
				t.Fatalf("exit %d, want 143", code)
			}
			if took := p.end.Sub(released); took > time.Second {
				t.Errorf("exited %v after the release", took)
			}
			if rec, ok := rbRecord(t, p.dir); !ok || rec.BootID != rbCur {
				t.Errorf("record %+v %v, want kept with %s", rec, ok, rbCur)
			}
			n := triggerLines(t, p.dir)
			if stage == stageAfterRecord && n != 0 {
				t.Errorf("trigger-called %d times, want none (S3)", n)
			}
			if stage == stageAfterCheck2 && n != 1 {
				t.Errorf("trigger-called %d times, want once (S4)", n)
			}
		})
	}
}
