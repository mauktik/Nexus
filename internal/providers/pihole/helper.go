package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// Every pihole mutation runs detached (CON-1): Prepare (detached.go) writes
// ops/<id>/input.json and returns a spec whose argv is
// [nexusd, "pihole-helper", "--dir", ops/<id>]. The supervisor in the
// nexus-op-<id> unit runs that hidden command as root; it performs the whole
// change through the backend's Mutator and writes ops/<id>/result.json.
// Finalize only reads result.json and exit.json.

// HelperCommand is the hidden nexusd subcommand of the pihole helper.
const HelperCommand = "pihole-helper"

// Files of the helper in the operation's directory, beside spec.json,
// exit.json and output/.
const (
	inputFile  = "input.json"
	resultFile = "result.json"
)

// helperInput is input.json. It holds the op's validated input and what
// Prepare snapshotted: a reload after Prepare does not change it (FR-2).
type helperInput struct {
	OperationID string          `json:"operationId"`
	Op          string          `json:"op"`      // The op ID, e.g. pihole.actions.stop-blocking
	Backend     string          `json:"backend"` // spec.BackendV5CLI or spec.BackendUnsupported
	Input       json.RawMessage `json:"input"`
	DNSFile     string          `json:"dnsFile"`
	StateDir    string          `json:"stateDir"`
	OpDir       string          `json:"opDir"`
	Deadline    time.Time       `json:"deadline"` // The helper's context ends here
}

// helperResult is result.json: the action's result, or its error. Error is
// present only when the action's error was non-nil.
type helperResult struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *helperError    `json:"error,omitempty"`
}

// helperError is a failed action's problem.
type helperError struct {
	Slug   problem.Slug   `json:"slug"`
	Detail string         `json:"detail"`
	Ext    map[string]any `json:"ext,omitempty"`
}

// helperInject carries the helper's test seams. Nil, in production, means
// the real exec runner, root "/", DNS server 127.0.0.1:53, time.Now and
// the dnscheck.ReadyWindow readiness window.
type helperInject struct {
	Runner      v5.Runner
	Root        string
	DNSServer   netip.AddrPort
	Now         func() time.Time
	ReadyWindow time.Duration
}

// helperCall is what an action works with.
type helperCall struct {
	In        helperInput
	Mutator   backend.Mutator
	Paths     v5.Paths
	DNSServer netip.AddrPort
	Now       func() time.Time
	// ReadyWindow is how long FTL has to answer after a restart or reload.
	ReadyWindow time.Duration
	Out         io.Writer // The operation's output: `pihole: <step>` lines and command output
}

// helperAction performs one op in the helper. A non-nil error is the op's
// problem; a *problem.Error crosses into it only through probErr.
type helperAction func(ctx context.Context, c *helperCall) (any, error)

// helperActions are the actions by op ID. Each area's work item adds its
// entries in its own file through a register…Actions call here.
var helperActions = newHelperActions()

func newHelperActions() map[string]helperAction {
	m := map[string]helperAction{}
	registerServiceActions(m)
	return m
}

// probErr is the only place a *problem.Error becomes an error: a nil one is
// a nil error, never a non-nil interface holding a nil pointer.
func probErr(pe *problem.Error) error {
	if pe == nil {
		return nil
	}
	return pe
}

// Helper is `nexusd pihole-helper --dir D`, run by op-supervise in the
// operation's unit. It reads D/input.json, runs the op's action and writes
// D/result.json. It returns 0 once result.json is written, whatever the
// action's outcome; without a result.json the operation ends
// outcome-unknown.
func Helper(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexusd "+HelperCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the operation's directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: nexusd %s --dir dir\n", HelperCommand)
		return 2
	}
	return runHelper(context.Background(), *dir, stdout, stderr, nil)
}

// helperSeams resolves inject to the seams the helper runs with.
func helperSeams(inject *helperInject) helperInject {
	s := helperInject{Runner: v5.Exec{}, Root: "/", DNSServer: defaultDNSServer, Now: time.Now, ReadyWindow: dnscheck.ReadyWindow}
	if inject == nil {
		return s
	}
	if inject.Runner != nil {
		s.Runner = inject.Runner
	}
	if inject.Root != "" {
		s.Root = inject.Root
	}
	if inject.DNSServer.IsValid() {
		s.DNSServer = inject.DNSServer
	}
	if inject.Now != nil {
		s.Now = inject.Now
	}
	if inject.ReadyWindow > 0 {
		s.ReadyWindow = inject.ReadyWindow
	}
	return s
}

// noCoreFiles lowers the helper's soft RLIMIT_CORE to 0, which every child
// inherits, so a crashing `pihole-FTL --test` (SIGSEGV on FTL v5.25.2, WI
// 0.1) leaves no core file in "/" or with systemd-coredump (A-57). The hard
// limit is unchanged.
func noCoreFiles() error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil {
		return err
	}
	lim.Cur = 0
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &lim)
}

// backendVersions is the detection result that selects backend name again
// in the helper: the backend Prepare recorded, not one detected anew.
var backendVersions = map[string]backend.Versions{
	spec.BackendV5CLI:       {Major: 5},
	spec.BackendUnsupported: {Major: 6},
}

// runHelper is the helper. It returns non-zero only when result.json cannot
// be written.
func runHelper(ctx context.Context, dir string, stdout, stderr io.Writer, inject *helperInject) int {
	if err := noCoreFiles(); err != nil {
		fmt.Fprintf(stderr, "%s: cannot disable core files: %v\n", HelperCommand, err)
	}
	// Never run twice: a result means the action already ran.
	if _, err := os.Lstat(filepath.Join(dir, resultFile)); err == nil {
		fmt.Fprintf(stderr, "%s: %s already exists in %s; nothing to do\n", HelperCommand, resultFile, dir)
		return 0
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "%s: %v; not running again\n", HelperCommand, err)
		return 1
	}
	res := helperRun(ctx, dir, stdout, inject)
	if err := writeJSONAtomic(dir, resultFile, res); err != nil {
		fmt.Fprintf(stderr, "%s: write %s in %s: %v\n", HelperCommand, resultFile, dir, err)
		return 1
	}
	return 0
}

// helperRun reads input.json and runs the action, as a result.
func helperRun(ctx context.Context, dir string, stdout io.Writer, inject *helperInject) helperResult {
	var in helperInput
	b, err := os.ReadFile(filepath.Join(dir, inputFile))
	if err == nil {
		err = json.Unmarshal(b, &in)
	}
	if err != nil {
		return errorResult(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot read its %s: %v; nothing was changed", inputFile, err)))
	}
	if !in.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, in.Deadline)
		defer cancel()
	}
	action, ok := helperActions[in.Op]
	if !ok {
		return errorResult(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper has no action for %q; nothing was changed", in.Op)))
	}
	v, ok := backendVersions[in.Backend]
	if !ok {
		return errorResult(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper has no backend %q; nothing was changed", in.Backend)))
	}
	s := helperSeams(inject)
	paths := v5.Paths{Root: s.Root}
	_, m, _, err := backend.Select(v, paths, backend.Deps{Runner: s.Runner, DNSServer: s.DNSServer, Now: s.Now})
	if err != nil {
		return errorResult(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot select backend %q: %v; nothing was changed", in.Backend, err)))
	}
	out, err := action(ctx, &helperCall{In: in, Mutator: m, Paths: paths, DNSServer: s.DNSServer, Now: s.Now,
		ReadyWindow: s.ReadyWindow, Out: stdout})
	if err != nil {
		var pe *problem.Error
		if !errors.As(err, &pe) {
			pe = problem.New(problem.InternalError, fmt.Sprintf("the pi-hole helper failed: %v", err))
		}
		return errorResult(pe)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return errorResult(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot encode its result: %v", err)))
	}
	return helperResult{Result: raw}
}

func errorResult(pe *problem.Error) helperResult {
	slug := pe.Slug()
	if slug == "" {
		slug = problem.InternalError
	}
	return helperResult{Error: &helperError{Slug: slug, Detail: pe.Detail, Ext: pe.Extensions}}
}

// writeJSONAtomic replaces dir/name with v as JSON, mode 0600, through a
// temporary file, fsync, rename and a directory fsync: a reader sees no
// file or a complete one.
func writeJSONAtomic(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("pihole: encode %s: %w", name, err)
	}
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("pihole: write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	// CreateTemp already uses 0600; Chmod states the contract.
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("pihole: write %s: %w", name, err), f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("pihole: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("pihole: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("pihole: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("pihole: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("pihole: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("pihole: sync %s: %w", dir, serr)
	}
	return nil
}
