package nexusd

// The service helper. restart, update and rollback run detached: Prepare
// writes ops/<id>/input.json and returns a spec whose argv is
// [nexusd, "service-helper", "--dir", ops/<id>], and the supervisor in the
// nexus-op-<id> unit runs that hidden command as root, outside nexusd's
// sandbox and outside nexusd.service, so restarting nexusd does not stop it
// (FR-9, A-19). The helper does the work, writes ops/<id>/result.json and
// exits 0; Finalize, in whichever nexusd is then running, only reads
// result.json and exit.json. Without a result.json the operation ends
// outcome-unknown.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// HelperCommand is the hidden nexusd subcommand of the service helper.
const HelperCommand = "service-helper"

// Files of the helper in the operation's directory, beside spec.json,
// exit.json and output/.
const (
	inputFileName  = "input.json"
	resultFileName = "result.json"
)

// helperInput is input.json. Fields are only ever added, never renamed or
// given a new meaning, so a helper of one release reads the input of the
// next (A-30, ASM-8). Paths has no JSON tags: its Go field names are its
// wire names, and they must not be renamed either.
type helperInput struct {
	Mode            string        `json:"mode"`
	RunningVersion  string        `json:"runningVersion,omitempty"`
	ExpectedVersion string        `json:"expectedVersion,omitempty"`
	PackageDir      string        `json:"packageDir,omitempty"`
	NoChange        bool          `json:"noChange,omitempty"`
	Health          healthTarget  `json:"health"`
	InstallTimeout  time.Duration `json:"installTimeout,omitempty"`
	Paths           Paths         `json:"paths"`
}

// helperResult is result.json, read by Finalize of possibly another
// release: additive only, as helperInput (A-30).
type helperResult struct {
	Changed         bool         `json:"changed"`
	RolledBack      bool         `json:"rolledBack"`
	Version         string       `json:"version,omitempty"`         // restart: the running version
	PreviousVersion string       `json:"previousVersion,omitempty"` // update
	NewVersion      string       `json:"newVersion,omitempty"`      // update
	FromVersion     string       `json:"fromVersion,omitempty"`     // rollback
	ToVersion       string       `json:"toVersion,omitempty"`       // rollback
	Error           *helperError `json:"error,omitempty"`
}

// helperError is a failed operation's problem.
type helperError struct {
	Slug      problem.Slug `json:"slug"`
	Detail    string       `json:"detail"`
	Step      string       `json:"step,omitempty"`      // The update step that failed
	Condition int          `json:"condition,omitempty"` // The FR-10 condition, 1 to 4
}

// helperRun is what a mode works with.
type helperRun struct {
	sys    system
	out    io.Writer // Step lines, into the op log
	errOut io.Writer // Diagnostics, into the op log
	dir    string    // The operation's directory
}

// helperMode performs one operation. It returns the result to record
// whatever happened: once it may have changed the host, every outcome is a
// result, never a missing one.
type helperMode func(ctx context.Context, h *helperRun, in helperInput) helperResult

// modeOf returns the function of a mode. rollback (4.3) and update (5.4)
// add their cases.
func modeOf(name string) (helperMode, bool) {
	switch name {
	case modeRestart:
		return helperRestartMode, true
	default:
		return nil, false
	}
}

// maxResultFile bounds what readResult reads.
const maxResultFile = 1 << 20

// readResult reads dir/result.json. ok is false when there is none.
func readResult(dir string) (res helperResult, ok bool, err error) {
	f, err := os.Open(filepath.Join(dir, resultFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return res, false, nil
	}
	if err != nil {
		return res, false, fmt.Errorf("nexusd: read %s: %w", resultFileName, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResultFile+1))
	if err != nil {
		return res, false, fmt.Errorf("nexusd: read %s: %w", resultFileName, err)
	}
	if len(b) > maxResultFile {
		return res, false, fmt.Errorf("nexusd: %s is larger than %d bytes", resultFileName, maxResultFile)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return res, false, fmt.Errorf("nexusd: decode %s: %w", resultFileName, err)
	}
	return res, true, nil
}

// problem is the failed operation's problem, with each member of the result
// that its type declares. A slug this release does not know as an
// operation's outcome (a newer helper's) is an internal error carrying the
// helper's detail.
func (res helperResult) problem() *problem.Error {
	e := res.Error
	t, ok := problem.Lookup(e.Slug)
	if !ok || !t.OperationLevel {
		return problem.New(problem.InternalError, fmt.Sprintf("%s (%s)", e.Detail, e.Slug))
	}
	p := problem.New(t.Slug, e.Detail)
	has := func(ext string) bool { return slices.Contains(t.Extensions, ext) }
	if has("condition") && e.Condition != 0 {
		p.With("condition", e.Condition)
	}
	if has("step") && e.Step != "" {
		p.With("step", e.Step)
	}
	if has("rolledBack") {
		p.With("rolledBack", res.RolledBack)
	}
	for ext, v := range map[string]string{
		"version": res.Version, "previousVersion": res.PreviousVersion, "newVersion": res.NewVersion,
		"fromVersion": res.FromVersion, "toVersion": res.ToVersion,
	} {
		if has(ext) && v != "" {
			p.With(ext, v)
		}
	}
	return p
}

// helperEnv are the helper's seams.
type helperEnv struct {
	newSystem func(in helperInput) system
	mode      func(name string) (helperMode, bool)
	// beforeRename runs between the temp write of result.json and its
	// rename (a test's fault injection); nil in production.
	beforeRename func(tmp string) error
}

// Helper is `nexusd service-helper --dir D`, run by op-supervise in the
// operation's unit. It reads D/input.json, performs the mode and writes
// D/result.json. Steps go to stdout and diagnostics to stderr, both into
// the operation's output. It returns 0 once result.json is written, whatever
// the operation's outcome, and 1 when it refuses to start or cannot record
// the result.
func Helper(args []string, stdout, stderr io.Writer) int {
	return runHelper(args, stdout, stderr, helperEnv{
		newSystem: func(in helperInput) system {
			unit := in.Paths.withDefaults().Unit
			return newRealSystem(unit)
		},
		mode: modeOf,
	})
}

func runHelper(args []string, stdout, stderr io.Writer, env helperEnv) int {
	refuse := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "nexusd %s: "+format+"\n", append([]any{HelperCommand}, a...)...)
		return 1
	}
	fset := flag.NewFlagSet("nexusd "+HelperCommand, flag.ContinueOnError)
	fset.SetOutput(stderr)
	dir := fset.String("dir", "", "the operation's directory")
	if err := fset.Parse(args); err != nil {
		return 1
	}
	switch {
	case fset.NArg() > 0:
		return refuse("usage: nexusd %s --dir dir", HelperCommand)
	case *dir == "":
		return refuse("--dir is required")
	case !filepath.IsAbs(*dir):
		return refuse("--dir %q is not an absolute path", *dir)
	}
	b, err := os.ReadFile(filepath.Join(*dir, inputFileName))
	if err != nil {
		return refuse("read %s: %v", inputFileName, err)
	}
	var in helperInput
	if err := json.Unmarshal(b, &in); err != nil {
		return refuse("decode %s: %v", inputFileName, err)
	}
	run, ok := env.mode(in.Mode)
	if !ok {
		return refuse("unknown mode %q", in.Mode)
	}
	// Never act twice: the supervisor runs the helper once, and a second
	// start must not repeat a finished restart, update or rollback.
	switch _, err := os.Lstat(filepath.Join(*dir, resultFileName)); {
	case err == nil:
		return refuse("%s already exists; not running again", resultFileName)
	case !errors.Is(err, fs.ErrNotExist):
		return refuse("%v; not running again", err)
	}
	h := &helperRun{sys: env.newSystem(in), out: stdout, errOut: stderr, dir: *dir}
	step(stdout, fmt.Sprintf("service helper: %s", in.Mode))
	res := run(context.Background(), h, in)
	if err := writeJSONAtomic(*dir, resultFileName, res, env.beforeRename); err != nil {
		return refuse("%v", err)
	}
	step(stdout, "wrote "+resultFileName)
	return 0
}

// writeResult records the helper's result in dir/result.json, atomically.
func writeResult(dir string, res helperResult) error {
	return writeJSONAtomic(dir, resultFileName, res, nil)
}

// step writes one progress line, prefixed with an RFC 3339 timestamp. The
// supervisor frames stdout into the op log. A lost log line never changes
// the operation's outcome, so its write error is not acted on.
func step(out io.Writer, name string) {
	_, _ = fmt.Fprintf(out, "%s %s\n", time.Now().UTC().Format(time.RFC3339), name)
}

// writeJSONAtomic replaces dir/name with v as JSON, mode 0600, through a
// temporary file, fsync, rename and a directory fsync, as docker's
// writeJSONAtomic: a reader sees no file or a complete one. beforeRename,
// when set, runs between the temp write and the rename.
func writeJSONAtomic(dir, name string, v any, beforeRename func(tmp string) error) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if beforeRename != nil {
		if err := beforeRename(tmp); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return syncDir(dir)
}
