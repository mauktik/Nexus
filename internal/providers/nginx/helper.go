package nginx

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/svcctl"
)

// HelperCommand is the hidden nexusd subcommand that runs one nginx
// operation outside nexusd's sandbox: `nexusd nginx-op run --dir D`, started
// by op-supervise in the operation's unit or supervised process.
const HelperCommand = "nginx-op"

// Tester runs `nginx -t` on the live configuration.
type Tester interface {
	Test(ctx context.Context) (TestOutput, error)
}

// ShadowTester runs `nginx -t` as if candidatePath's bytes were in
// includeDir under presentedName, without changing includeDir (ASM-10): in
// a private mount namespace over a shadow copy of it (shadow_linux.go). As
// with Tester, an invalid configuration is a TestOutput with Valid false
// and the error is for a test that could not run.
type ShadowTester interface {
	Test(ctx context.Context, presentedName, candidatePath string) (TestOutput, error)
}

// env is what an action runs with, built from input.json by newEnv. Tests
// inject their own through runHelper.
type env struct {
	nginx  Tester
	shadow ShadowTester      // Set by runAction, which knows the operation's directory, unless build set it
	svc    svcctl.Controller // The init system, each call bounded by stateTimeout
	proc   ProcReader        // The host's processes, for reload verification
	out    io.Writer         // The operation log
}

// newEnv builds the real dependencies of in: the nginx binary it names,
// bounded by its testTimeout, writing to out; the service control of its
// init system, each call bounded by its stateTimeout; and the host's /proc.
// An init system svcctl does not support fails only the actions that use
// it, so the config test still runs.
func newEnv(in helperInput, out io.Writer) (env, error) {
	init := platform.InitSystem(in.InitSystem)
	svc, err := svcctl.New(init, svcctl.Options{Timeout: in.Settings.StateTimeout})
	if err != nil {
		svc = noController{init: init, err: err}
	}
	return env{nginx: nginxBin{path: in.Nginx, testTimeout: in.Settings.TestTimeout, log: out}, svc: svc,
		proc: HostProcReader(), out: out}, nil
}

// noController is the service control of an init system svcctl does not
// support: every call fails with why.
type noController struct {
	init platform.InitSystem
	err  error
}

func (c noController) Init() platform.InitSystem { return c.init }

func (c noController) Status(context.Context, string) (svcctl.Status, error) {
	return svcctl.Status{State: svcctl.Unknown}, c.err
}

func (c noController) Do(context.Context, svcctl.Action, string) (svcctl.Result, error) {
	return svcctl.Result{}, c.err
}

// action runs one operation's work in the helper. It returns the result
// that goes into result.json, or the problem the operation fails with.
type action func(ctx context.Context, e env, in helperInput) (any, *helperError)

// actions are the helper's actions by input.json's action.
var actions = map[string]action{
	actionTest:    runTest,
	actionStart:   runStart,
	actionStop:    runStop,
	actionRestart: runRestart,
	actionReload:  runReload,
}

// Helper is `nexusd nginx-op`. `run --dir D` reads D/input.json, runs its
// action and writes D/result.json, then exits 0 whatever the action's
// outcome. It exits non-zero only when it cannot write a result, which the
// operation reports as outcome-unknown. `shadow-test --dir D` is the shadow
// configuration test's child (shadow_linux.go), which only the helper starts.
func Helper(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "run":
			dir, code := dirFlag("run", args[1:], stderr)
			if code >= 0 {
				return code
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			return runHelper(ctx, dir, stdout, stderr, newEnv)
		case "shadow-test":
			dir, code := dirFlag("shadow-test", args[1:], stderr)
			if code >= 0 {
				return code
			}
			return shadowChild(dir, stderr)
		}
	}
	helperUsage(stderr)
	return 2
}

func helperUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: nexusd %[1]s run --dir dir\n       nexusd %[1]s shadow-test --dir dir\n", HelperCommand)
}

// dirFlag parses the --dir of sub. code is -1 when the helper should go on,
// else its exit code.
func dirFlag(sub string, args []string, stderr io.Writer) (dir string, code int) {
	fs := flag.NewFlagSet("nexusd "+HelperCommand+" "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	d := fs.String("dir", "", "the operation's directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", 0
		}
		return "", 2
	}
	if *d == "" || fs.NArg() > 0 {
		helperUsage(stderr)
		return "", 2
	}
	return *d, -1
}

// runHelper runs dir's action with the env build makes, and writes
// result.json. A result.json that already exists, even one that cannot be
// read, stops it: the supervisor runs the helper once, and a second start
// must never repeat a change.
func runHelper(ctx context.Context, dir string, stdout, stderr io.Writer,
	build func(helperInput, io.Writer) (env, error)) int {
	var prior helperResult
	if ok, err := readJSON(dir, resultFile, &prior); err != nil {
		fmt.Fprintf(stderr, "nexusd %s: %v; not running again\n", HelperCommand, err)
		return 1
	} else if ok {
		fmt.Fprintf(stderr, "nexusd %s: %s already exists; nothing to do\n", HelperCommand, resultFile)
		return 1
	}
	res := helperResult{}
	out, herr := runAction(ctx, dir, stdout, build)
	if herr != nil {
		res.Error = herr
		say(stdout, "nginx-op: failed: %s: %s", herr.Slug, herr.Detail)
	} else {
		if b, err := json.Marshal(out); err != nil {
			res.Error = failure(problem.InternalError, "the result cannot be encoded: %v", err)
		} else {
			res.Result = b
		}
	}
	if err := writeJSONAtomic(dir, resultFile, res); err != nil {
		fmt.Fprintf(stderr, "nexusd %s: %v\n", HelperCommand, err)
		return 1
	}
	return 0
}

// runAction reads input.json and runs its action. Input that cannot be read
// or names no known action fails the operation: nothing has been done.
func runAction(ctx context.Context, dir string, stdout io.Writer,
	build func(helperInput, io.Writer) (env, error)) (any, *helperError) {
	var in helperInput
	ok, err := readJSON(dir, inputFile, &in)
	switch {
	case err != nil:
		return nil, failure(problem.InternalError, "the helper's input cannot be read: %v; nothing was changed", err)
	case !ok:
		return nil, failure(problem.InternalError, "the helper has no %s; nothing was changed", inputFile)
	}
	act, ok := actions[in.Action]
	if !ok {
		return nil, failure(problem.InternalError, "the helper has no action %q; nothing was changed", in.Action)
	}
	e, err := build(in, stdout)
	if err != nil {
		return nil, failure(problem.InternalError, "the helper cannot start action %s: %v; nothing was changed",
			in.Action, err)
	}
	if e.shadow == nil {
		e.shadow = newShadowTester(dir, in, stdout)
	}
	return act(ctx, e, in)
}

// shadowTest runs e's shadow test of candidatePath presented as
// presentedName. A test that could not run fails with
// dependency-unavailable, as a live one does (runTest).
func shadowTest(ctx context.Context, e env, presentedName, candidatePath string) (TestOutput, *helperError) {
	say(e.out, "nginx-op: shadow test with the candidate as %s", presentedName)
	t, err := e.shadow.Test(ctx, presentedName, candidatePath)
	if err != nil {
		return t, failure(problem.DependencyUnavailable, "%v", err)
	}
	return t, nil
}
