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

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
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
	FromVersion     string        `json:"fromVersion,omitempty"` // rollback: the running version
	ToVersion       string        `json:"toVersion,omitempty"`   // rollback: the rollback binary's version
	// SavedUnit is the saved copy of the unit file, swapped with
	// Paths.UnitFile when it exists (rollback).
	SavedUnit string `json:"savedUnit,omitempty"`
	// config-set (F5 step 6); NoChange is not used: a no-change set never
	// reaches Prepare.
	ConfigPath    string        `json:"configPath,omitempty"`
	BackupPath    string        `json:"backupPath,omitempty"`
	Changes       []string      `json:"changes,omitempty"`
	BaseSHA256    string        `json:"baseSha256,omitempty"` // admission's digest (D-7, A-15)
	StateDir      string        `json:"stateDir,omitempty"`
	ReloadTimeout time.Duration `json:"reloadTimeout,omitempty"`
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
	// config-set.
	ConfigChanges   []spec.ConfigChange `json:"configChanges,omitempty"` // key, old, new
	RestartRequired []string            `json:"restartRequired,omitempty"`
	BackupPath      string              `json:"backupPath,omitempty"`
	Restored        bool                `json:"restored,omitempty"`
}

// helperError is a failed operation's problem.
type helperError struct {
	Slug      problem.Slug `json:"slug"`
	Detail    string       `json:"detail"`
	Step      string       `json:"step,omitempty"`      // The update step that failed
	Condition int          `json:"condition,omitempty"` // The FR-10 condition, 1 to 4
	// config-set: config-invalid's file and located errors. The stage (load
	// or apply) at which the daemon refused a new file is in Detail.
	ConfigPath string             `json:"configPath,omitempty"`
	Errors     []spec.ConfigError `json:"errors,omitempty"`
}

// helperRun is what a mode works with.
type helperRun struct {
	sys    system
	out    io.Writer // Step lines, into the op log
	errOut io.Writer // Diagnostics, into the op log
	dir    string    // The operation's directory
	// procDir is where /proc/<pid>/exe lives: /proc, a test's directory
	// in T0.
	procDir string
	// callTimeout bounds each systemctl call the update mode makes
	// (systemctlTimeout); a test injects a shorter one.
	callTimeout time.Duration
	// check is the daemon's validator (config-set); never nil there.
	check ConfigCheckFunc
	// beforeStep is a test's fault hook, called at the config-set steps
	// named stepFault*; nil in production. An error stops the mode as
	// KillProcess would: abort is set and no result is written.
	beforeStep func(name string) error
	abort      error
}

// helperMode performs one operation. It returns the result to record
// whatever happened: once it may have changed the host, every outcome is a
// result, never a missing one.
type helperMode func(ctx context.Context, h *helperRun, in helperInput) helperResult

// modeOf returns the function of a mode.
func modeOf(name string) (helperMode, bool) {
	switch name {
	case modeRestart:
		return helperRestartMode, true
	case modeUpdate:
		return helperUpdateMode, true
	case modeRollback:
		return helperRollbackMode, true
	case modeConfigSet:
		return helperConfigSetMode, true
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
	// config-invalid is a request-level type that the config-set helper's
	// re-validation also ends an operation with (ASM-10).
	if !ok || (!t.OperationLevel && t.Slug != problem.ConfigInvalid) {
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
	if has("restored") {
		p.With("restored", res.Restored)
	}
	if has("configPath") && e.ConfigPath != "" {
		p.With("configPath", e.ConfigPath)
	}
	if has("errors") && e.Errors != nil {
		p.With("errors", e.Errors)
	}
	for ext, v := range map[string]string{
		"version": res.Version, "previousVersion": res.PreviousVersion, "newVersion": res.NewVersion,
		"fromVersion": res.FromVersion, "toVersion": res.ToVersion, "backupPath": res.BackupPath,
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
	// procDir replaces /proc, and callTimeout the 30 s bound of a
	// systemctl call, in a test; zero values are the defaults.
	procDir     string
	callTimeout time.Duration
	// check is the daemon's validator; mode config-set refuses without it.
	check ConfigCheckFunc
	// beforeStep is the fault hook of helperRun; nil in production.
	beforeStep func(name string) error
}

// ConfigCheckFunc is the daemon's byte-level configuration check: in
// production, daemon.CheckConfigData, which this package cannot import
// (D-6). It returns the located errors, none when the bytes are valid.
type ConfigCheckFunc func(data []byte, file string) config.ErrorList

// noValidatorText is the refusal of mode config-set without a validator.
const noValidatorText = "no validator"

// faultHookMarker is the step line written when the fault hook is armed,
// which production never does.
const faultHookMarker = "test fault hook armed"

// Helper is `nexusd service-helper --dir D`, run by op-supervise in the
// operation's unit. It reads D/input.json, performs the mode and writes
// D/result.json. Steps go to stdout and diagnostics to stderr, both into
// the operation's output. It returns 0 once result.json is written, whatever
// the operation's outcome, and 1 when it refuses to start or cannot record
// the result. check is the daemon's validator, which mode config-set
// needs: with a nil check that mode refuses to start (exit 1, no result),
// while the other modes run.
func Helper(args []string, stdout, stderr io.Writer, check ConfigCheckFunc) int {
	return runHelper(args, stdout, stderr, helperEnv{
		newSystem: func(in helperInput) system {
			unit := in.Paths.withDefaults().Unit
			return newRealSystem(unit)
		},
		mode:  modeOf,
		check: check,
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
	if in.Mode == modeConfigSet && env.check == nil {
		return refuse("mode %s has %s: nexusd passes the daemon's configuration check; nothing was changed",
			modeConfigSet, noValidatorText)
	}
	// Never act twice: the supervisor runs the helper once, and a second
	// start must not repeat a finished restart, update or rollback.
	switch _, err := os.Lstat(filepath.Join(*dir, resultFileName)); {
	case err == nil:
		return refuse("%s already exists; not running again", resultFileName)
	case !errors.Is(err, fs.ErrNotExist):
		return refuse("%v; not running again", err)
	}
	h := &helperRun{sys: env.newSystem(in), out: stdout, errOut: stderr, dir: *dir, procDir: env.procDir,
		callTimeout: env.callTimeout, check: env.check, beforeStep: env.beforeStep}
	if h.procDir == "" {
		h.procDir = "/proc"
	}
	if h.callTimeout <= 0 {
		h.callTimeout = systemctlTimeout
	}
	step(stdout, fmt.Sprintf("service helper: %s", in.Mode))
	if h.beforeStep != nil {
		step(stdout, faultHookMarker)
	}
	res := run(context.Background(), h, in)
	if h.abort != nil {
		// Stopped as KillProcess would stop it: no result.json.
		return refuse("%v", h.abort)
	}
	if err := writeJSONAtomic(*dir, resultFileName, res, env.beforeRename); err != nil {
		return refuse("%v", err)
	}
	step(stdout, "wrote "+resultFileName)
	if in.Mode == modeUpdate {
		// Only now that result.json is written: the package is not needed
		// any more, since install.sh's output is in the op log (F6 step 8,
		// A-50). A lost unit keeps it until the op's retention.
		removePackage(*dir, stdout, stderr)
	}
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
