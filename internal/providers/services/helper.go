// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/mauktik/nexus/internal/problem"
)

// HelperCommand is the hidden nexusd subcommand that runs one services
// action outside nexusd's sandbox (D-5, CON-2): `nexusd services-helper run
// --dir D`, started by op-supervise in the operation's unit or supervised
// process. It reads D/input.json and runs its mode.
const HelperCommand = "services-helper"

// helperEnv is the helper's seams, one per mode. The zero value is the
// real host.
type helperEnv struct {
	control controlHelperEnv
	reboot  rebootHelperEnv
}

// helperMode runs one mode of input.json, raw, in the operation's directory
// dir, and returns the helper's exit code. Each mode writes its own
// result.json.
type helperMode func(ctx context.Context, dir string, raw []byte, stdout, stderr io.Writer, env helperEnv) int

// helperModes are the helper's modes by input.json's mode.
var helperModes = map[string]helperMode{
	helperModeControl: runControlMode,
	helperModeReboot:  runRebootMode,
}

// Helper is `nexusd services-helper`. `run --dir D` reads D/input.json and
// runs its mode, which writes D/result.json. It exits 0 once a result is
// written, whatever the outcome, and non-zero when it cannot write one,
// which the operation reports as outcome-unknown. A SIGTERM, which the
// supervisor forwards on a cancel or a stop, is its stop context, made
// before input.json is read.
func Helper(args []string, stdout, stderr io.Writer) int {
	return helper(args, stdout, stderr, helperEnv{})
}

// helper is Helper with its seams. The tests' helper processes run it, so
// the SIGTERM stop context they are signaled through is the production one.
func helper(args []string, stdout, stderr io.Writer, env helperEnv) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return runHelper(ctx, args, stdout, stderr, env)
}

// runHelper is Helper with its stop context and seams.
func runHelper(ctx context.Context, args []string, stdout, stderr io.Writer, env helperEnv) int {
	if len(args) == 0 || args[0] != "run" {
		helperUsage(stderr)
		return 2
	}
	dir, code := helperDirFlag(args[1:], stderr)
	if code >= 0 {
		return code
	}
	// The supervisor runs the helper once; a second start must never
	// repeat an action, so a result.json that exists, even one that cannot
	// be read, stops it.
	var prior helperResult
	if ok, err := readHelperJSON(dir, helperResultFile, &prior); err != nil {
		fmt.Fprintf(stderr, "nexusd %s: %v; not running again\n", HelperCommand, err)
		return 1
	} else if ok {
		fmt.Fprintf(stderr, "nexusd %s: %s already exists; nothing to do\n", HelperCommand, helperResultFile)
		return 1
	}
	raw, ok, err := readHelperFile(dir, helperInputFile)
	var head helperHeader
	switch {
	case err != nil:
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the helper's input cannot be read: %v; nothing was changed", err), stdout, stderr)
	case !ok:
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the helper has no %s; nothing was changed", helperInputFile), stdout, stderr)
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the helper's input cannot be decoded: %v; nothing was changed", err), stdout, stderr)
	}
	mode, ok := helperModes[head.Mode]
	if !ok {
		return writeHelperOutcome(dir, nil, helperFailure(problem.InternalError,
			"the helper has no mode %q; nothing was changed", head.Mode), stdout, stderr)
	}
	return mode(ctx, dir, raw, stdout, stderr, env)
}

func helperUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: nexusd %s run --dir dir\n", HelperCommand)
}

// helperDirFlag parses run's --dir. code is -1 when the helper should go
// on, else its exit code.
func helperDirFlag(args []string, stderr io.Writer) (dir string, code int) {
	fs := flag.NewFlagSet("nexusd "+HelperCommand+" run", flag.ContinueOnError)
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

// writeHelperOutcome writes result.json with out, or with herr when it is
// set, and returns the helper's exit code: 0 once it is written.
func writeHelperOutcome(dir string, out any, herr *helperError, stdout, stderr io.Writer) int {
	var res helperResult
	if herr != nil {
		res.Error = herr
		helperSay(stdout, "failed: %s: %s", herr.Slug, herr.Detail)
	} else if b, err := json.Marshal(out); err != nil {
		res.Error = helperFailure(problem.InternalError, "the result cannot be encoded: %v", err)
	} else {
		res.Result = b
	}
	if err := writeHelperJSON(dir, helperResultFile, res); err != nil {
		fmt.Fprintf(stderr, "nexusd %s: %v\n", HelperCommand, err)
		return 1
	}
	return 0
}

// helperSay writes one services-op line to the operation log. A failed
// write to the log cannot be reported anywhere else, so it is dropped.
func helperSay(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "services-op: "+format+"\n", args...)
}
