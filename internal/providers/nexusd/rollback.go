package nexusd

// rollback (F10): a detached, destructive operation. Admit refuses a host
// without self-management, a missing rollback binary (rollback-unavailable),
// a rollback binary that rejects the configuration file nexusd runs with
// (config-invalid, its own messages) and one whose -version is not
// "nexusd <version>". Prepare writes input.json; the service helper, outside
// nexusd.service, swaps the binary with nexusd.prev and the unit file with
// its saved copy, when there is one, so a rollback can itself be rolled
// back. It then reloads systemd, clears a failed state, restarts the unit
// and runs the FR-10 health check against the rollback binary's version.
// On failure it swaps back and runs the same sequence against the version
// that ran before, and the operation fails rollback-failed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/version"
)

// modeRollback is the helper's mode for rollback.
const modeRollback = "rollback"

// validateTimeout bounds the rollback binary's validate-config run.
const validateTimeout = 10 * time.Second

// The parts of rollback's MaxRuntime (A-8): each of its two health checks
// (the rollback's and the swap back's) runs for healthTimeout plus the
// stability window, and each is preceded by systemctl calls; the slack
// covers the swaps, -version runs and the supervisor.
const (
	rollbackPhaseSlack   = 20 * time.Second
	rollbackRuntimeSlack = 60 * time.Second
)

// rollbackMaxRuntime is 2×(healthTimeout+20s)+60s (A-8).
func rollbackMaxRuntime(t healthTarget) time.Duration {
	return 2*(t.Timeout+rollbackPhaseSlack) + rollbackRuntimeSlack
}

// savedUnitDir and savedUnitName place the saved copy of the unit file in
// the state directory: <stateDir>/rollback/nexusd.service. An update saves
// the installed unit there; a rollback swaps the two.
const (
	savedUnitDir  = "rollback"
	savedUnitName = "nexusd.service"
)

// savedUnitPath is the saved unit file under stateDir.
func savedUnitPath(stateDir string) string {
	return filepath.Join(stateDir, savedUnitDir, savedUnitName)
}

// swapTmpSuffix names the copy a swap keeps of the file it replaces, beside
// it: /usr/local/bin/nexusd.swap.tmp (A-22). A swap interrupted between its
// second and third step leaves the rollback file missing and its content
// there.
const swapTmpSuffix = ".swap.tmp"

// swapNewSuffix names the copy a cross-filesystem swap renames over the
// installed unit file.
const swapNewSuffix = ".swap.new.tmp"

// rollbackBinding binds nexusd.actions.rollback: detached, in the nexusd
// lane.
func (p *Provider) rollbackBinding() provider.Binding {
	return provider.Detached(spec.Rollback, provider.DetachedSpec[spec.RollbackInput, spec.RollbackResult]{
		Admit:      p.admitRollback,
		Lane:       laneOf[spec.RollbackInput],
		Prepare:    p.prepareRollback,
		Finalize:   p.finalizeRollback,
		LostDetail: p.lostRollbackDetail,
	})
}

// admitRollback refuses, before an operation exists, a host without
// self-management, then a rollback binary that is missing, rejects the
// configuration file or does not report a version.
func (p *Provider) admitRollback(ctx context.Context, _ spec.RollbackInput) error {
	if err := p.requireSelfManagement(); err != nil {
		return err
	}
	d, err := p.daemonOf()
	if err != nil {
		return err
	}
	_, err = p.checkPrev(ctx, d.ConfigPath())
	return err
}

// rollbackUnavailable is the refusal of a rollback binary that cannot be
// rolled back to.
func (p *Provider) rollbackUnavailable(detail string) error {
	return problem.New(problem.RollbackUnavailable, detail).With("path", p.paths.Prev)
}

// checkPrev checks the rollback binary and returns its version: it must be
// a regular file, `<prev> validate-config --config <configPath>` must exit
// 0 within 10 s, so nexusd would start again on that binary with the
// configuration in use, and `<prev> -version` must print
// "nexusd <version>" within 5 s. A run that fails because ctx ended returns
// ctx's error, not rollback-unavailable.
func (p *Provider) checkPrev(ctx context.Context, configPath string) (string, error) {
	prev := p.paths.Prev
	fi, err := os.Lstat(prev)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		detail := fmt.Sprintf("there is no rollback binary at %s", prev)
		if tmp := p.paths.Binary + swapTmpSuffix; lexists(tmp) {
			detail += fmt.Sprintf("; %s holds the binary an interrupted rollback moved out of the way: check "+
				"its -version and rename it to %s by hand", tmp, prev)
		}
		return "", p.rollbackUnavailable(detail)
	case err != nil:
		return "", p.rollbackUnavailable(fmt.Sprintf("cannot read the rollback binary: %v", err))
	case !fi.Mode().IsRegular():
		return "", p.rollbackUnavailable(fmt.Sprintf("the rollback binary %s is not a regular file (%s)", prev,
			fi.Mode().Type()))
	}

	vctx, cancel := context.WithTimeout(ctx, validateTimeout)
	res, err := p.run.Run(vctx, []string{prev, "validate-config", "--config", configPath})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("nexusd: validate-config of the rollback binary: %w", context.Cause(ctx))
		}
		return "", p.rollbackUnavailable(fmt.Sprintf("%s validate-config could not run: %v", prev, err))
	}
	if res.ExitCode != 0 {
		return "", prevRejects(prev, configPath, res)
	}

	vctx, cancel = context.WithTimeout(ctx, versionTimeout)
	res, err = p.run.Run(vctx, []string{prev, "-version"})
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return "", fmt.Errorf("nexusd: -version of the rollback binary: %w", context.Cause(ctx))
	case err != nil:
		return "", p.rollbackUnavailable(fmt.Sprintf("%s -version could not run: %v", prev, err))
	case res.ExitCode != 0:
		return "", p.rollbackUnavailable(fmt.Sprintf("%s -version exited with code %d: %s", prev, res.ExitCode,
			trimOutput(res.Stderr)))
	}
	v, ok := parseVersion(res.Stdout)
	if !ok {
		return "", p.rollbackUnavailable(fmt.Sprintf("%s -version printed %q, not \"nexusd <version>\"", prev,
			trimOutput(res.Stdout)))
	}
	return v, nil
}

// prevRejects is the config-invalid of a rollback binary whose
// validate-config failed: each line it printed to stderr is one error's
// message (it prints one problem per line), or its stdout's when stderr is
// empty.
func prevRejects(prev, configPath string, res RunResult) error {
	out := res.Stderr
	if len(bytes.TrimSpace(out)) == 0 {
		out = res.Stdout
	}
	errs := []spec.ConfigError{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			errs = append(errs, spec.ConfigError{Message: line})
		}
	}
	if len(errs) == 0 {
		errs = append(errs, spec.ConfigError{Message: fmt.Sprintf("%s validate-config exited with code %d",
			prev, res.ExitCode)})
	}
	detail := fmt.Sprintf("the rollback binary %s rejects %s (%d error(s)), so nexusd would not start after the "+
		"rollback; nothing was changed", prev, configPath, len(errs))
	return problem.New(problem.ConfigInvalid, detail).With("configPath", configPath).With("errors", errs)
}

// prepareRollback checks the rollback binary again, since it may have
// changed since Admit, writes input.json and returns the helper's spec.
func (p *Provider) prepareRollback(ctx context.Context, _ spec.RollbackInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	d, err := p.daemonOf()
	if err != nil {
		return detach.Spec{}, err
	}
	to, err := p.checkPrev(ctx, d.ConfigPath())
	if err != nil {
		return detach.Spec{}, err
	}
	check := d.CheckConfig()
	if check.Config == nil {
		if err := configValid(d.ConfigPath(), check.Errors); err != nil {
			return detach.Spec{}, err
		}
		return detach.Spec{}, fmt.Errorf("nexusd: %s passed its check but was not loaded", d.ConfigPath())
	}
	target, err := targetFrom(check.Config, d.StateDir())
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: health target: %w", err)
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: locate nexusd: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: create %s: %w", dir, err)
	}
	in := helperInput{
		Mode:        modeRollback,
		FromVersion: version.Version,
		ToVersion:   to,
		Health:      target,
		Paths:       p.paths,
		SavedUnit:   savedUnitPath(d.StateDir()),
	}
	if err := writeJSONAtomic(dir, inputFileName, in, nil); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: %w", err)
	}
	fmt.Fprintf(r.Output(), "rolling %s back from nexusd %s to nexusd %s (%s) from unit %s; health check against "+
		"%s within %s\n", p.paths.Unit, in.FromVersion, to, p.paths.Prev, detach.Unit(r.ID()), target.URL,
		target.Timeout)
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Dir:        dir,
		KillMode:   detach.KillProcess,
		MaxRuntime: rollbackMaxRuntime(target),
	}, nil
}

// helperRollbackMode is the helper's rollback (F10): note the running
// MainPID, swap the binaries and the unit files, then reload systemd, clear
// a failed state, restart and wait for a new, healthy process of the
// rollback binary's version. On failure it swaps back and runs the same
// sequence against the version that ran before.
func helperRollbackMode(ctx context.Context, h *helperRun, in helperInput) helperResult {
	paths := in.Paths.withDefaults()
	from, to := in.FromVersion, in.ToVersion
	failed := func(cond int, detail string) helperResult {
		_, _ = fmt.Fprintln(h.errOut, detail) // A lost log line never changes the outcome, as in step
		return helperResult{FromVersion: from, ToVersion: to,
			Error: &helperError{Slug: problem.RollbackFailed, Detail: detail, Condition: cond}}
	}
	st, err := h.sys.Show(ctx)
	if err != nil {
		return failed(0, fmt.Sprintf("cannot read %s before the rollback (%v); nothing was changed", paths.Unit, err))
	}
	pid0 := st.MainPID
	step(h.out, fmt.Sprintf("swapping %s (nexusd %s) with %s (nexusd %s), MainPID %d", paths.Binary, from,
		paths.Prev, to, pid0))
	if err := swapInstalled(paths, in.SavedUnit, nil); err != nil {
		return failed(0, fmt.Sprintf("the rollback to nexusd %s did not start: %v. %s was not restarted", to, err,
			paths.Unit))
	}
	err = rollbackActivate(ctx, h, in, to, pid0)
	if err == nil {
		step(h.out, fmt.Sprintf("%s is healthy, running nexusd %s", paths.Unit, to))
		return helperResult{Changed: true, FromVersion: from, ToVersion: to}
	}
	cond, why := failureCondition(err)
	first := fmt.Sprintf("the rollback to nexusd %s failed: %s", to, why)
	step(h.out, fmt.Sprintf("nexusd %s is not healthy; swapping back to nexusd %s", to, from))
	if err := swapInstalled(paths, in.SavedUnit, nil); err != nil {
		return failed(cond, fmt.Sprintf("%s; swapping back to nexusd %s failed too: %v. %s", first, from, err,
			manualRecovery(paths, in.SavedUnit)))
	}
	if err := rollbackActivate(ctx, h, in, from, -1); err != nil {
		_, why := failureCondition(err)
		return failed(cond, fmt.Sprintf("%s; after swapping back, nexusd %s is not healthy either: %s. %s", first,
			from, why, manualRecovery(paths, in.SavedUnit)))
	}
	step(h.out, fmt.Sprintf("%s is healthy again, running nexusd %s", paths.Unit, from))
	return failed(cond, fmt.Sprintf("%s; the swap was undone and nexusd %s is running and healthy", first, from))
}

// rollbackActivate makes systemd run the installed binary and unit:
// daemon-reload, reset-failed (at its start limit systemd accepts a
// --no-block restart but refuses the start), restart --no-block, then the
// FR-10 health check against expected, excluding the PID that ran before;
// the health check is what detects a start that fails. exclude < 0 means the
// MainPID right before the restart.
func rollbackActivate(ctx context.Context, h *helperRun, in helperInput, expected string, exclude int) error {
	unit := in.Paths.withDefaults().Unit
	if err := h.sys.DaemonReload(ctx); err != nil {
		return err
	}
	step(h.out, fmt.Sprintf("reset-failed %s, clearing a failed state and the start limit", unit))
	if err := h.sys.ResetFailed(ctx); err != nil {
		return err
	}
	if exclude < 0 {
		st, err := h.sys.Show(ctx)
		if err != nil {
			return err
		}
		exclude = st.MainPID
	}
	step(h.out, fmt.Sprintf("restarting %s onto nexusd %s", unit, expected))
	if err := h.sys.Restart(ctx); err != nil {
		return err
	}
	step(h.out, fmt.Sprintf("health check: %s within %s", in.Health.URL, in.Health.Timeout))
	_, err := waitHealthy(ctx, h.sys, in.Health, expected, exclude)
	return err
}

// failureCondition is a failure's FR-10 condition, 0 when it is not a failed
// health check, and its text.
func failureCondition(err error) (int, string) {
	var ce *conditionError
	if errors.As(err, &ce) {
		return ce.Condition, ce.Error()
	}
	return 0, err.Error()
}

// manualRecovery is the detail's guidance when nexusd may be down.
func manualRecovery(p Paths, savedUnit string) string {
	unit := fmt.Sprintf("its unit file at %s", p.UnitFile)
	if savedUnit != "" {
		unit += fmt.Sprintf(" (the other unit file is %s)", savedUnit)
	}
	return fmt.Sprintf("nexusd may be down. Recover by hand: `journalctl -u %[1]s` shows why it fails; put a working "+
		"nexusd at %[2]s (the other binary is %[3]s, or %[2]s%[4]s if %[3]s is missing) and %[5]s, then run "+
		"`systemctl daemon-reload`, `systemctl reset-failed %[1]s` and `systemctl restart %[1]s`, and check "+
		"`%[2]s -version` and `systemctl status %[1]s`", p.Unit, p.Binary, p.Prev, swapTmpSuffix, unit)
}

// finalizeRollback states the outcome from result.json. It only reads
// files.
func (p *Provider) finalizeRollback(_ context.Context, in spec.RollbackInput, x detach.Exit, r *provider.Run) (spec.RollbackResult, error) {
	var none spec.RollbackResult
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	res, ok, err := readResult(dir)
	if err != nil {
		return none, err
	}
	if !ok {
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("the service helper could not be started "+
				"(%s); nothing was changed", x.Error))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		return none, problem.New(problem.OutcomeUnknown, fmt.Sprintf("the service helper %s without reporting a "+
			"result; %s", how, p.lostRollbackDetail(r, in)))
	}
	if res.Error != nil {
		return none, res.problem()
	}
	return spec.RollbackResult{ActionResult: apitypes.ActionResult{Changed: true}, FromVersion: res.FromVersion,
		ToVersion: res.ToVersion}, nil
}

// lostRollbackDetail is what to check by hand after a rollback that
// stopped without a result, naming the paths of input.json when it can be
// read, and the provider's own otherwise. It only reads a file.
func (p *Provider) lostRollbackDetail(r *provider.Run, _ spec.RollbackInput) string {
	paths := p.paths
	if dir, err := p.opDir(r.ID()); err == nil {
		var in helperInput
		if b, err := os.ReadFile(filepath.Join(dir, inputFileName)); err == nil && json.Unmarshal(b, &in) == nil {
			paths = in.Paths.withDefaults()
		}
	}
	return fmt.Sprintf("%[1]s may or may not have been rolled back. Check by hand: `systemctl status %[1]s` shows "+
		"whether it is active and since when, and `%[2]s -version` and `%[3]s -version` which binary is "+
		"installed where. If %[3]s is missing, the swap was interrupted and %[2]s%[4]s holds the other binary: "+
		"rename it to %[3]s", paths.Unit, paths.Binary, paths.Prev, swapTmpSuffix)
}

// swapError is a swap that failed. changed reports whether the pair was
// left other than it was; when it was not, nothing needs undoing.
type swapError struct {
	changed bool
	err     error
}

func (e *swapError) Error() string { return e.err.Error() }
func (e *swapError) Unwrap() error { return e.err }

// swapInstalled swaps the installed binary with the rollback binary and,
// when savedUnit exists, the installed unit file with savedUnit (F10,
// A-22). The installed binary and unit file exist at every instant; the
// swap of a pair is not atomic. A unit swap that fails without changing the
// units swaps the binaries back.
//
// hook, when not nil, is called after each step with its number (1-3 the
// binaries, 4-6 the units); an error from it stops the swap there, as a
// crash would, and is returned as it is (a test's crash injection).
func swapInstalled(p Paths, savedUnit string, hook func(step int) error) error {
	var stop error
	at := func(n int) error {
		if hook == nil {
			return nil
		}
		stop = hook(n)
		return stop
	}
	if err := swapPair(p.Binary, p.Prev, p.Binary+swapTmpSuffix, false, at, 0); err != nil {
		return err
	}
	if savedUnit == "" {
		return nil
	}
	fi, err := os.Lstat(savedUnit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err == nil && !fi.Mode().IsRegular():
		err = fmt.Errorf("%s is not a regular file (%s)", savedUnit, fi.Mode().Type())
	case err == nil:
		err = swapPair(p.UnitFile, savedUnit, savedUnit+swapTmpSuffix, true, at, 3)
		if err == nil {
			return nil
		}
		if stop != nil {
			return err
		}
		var se *swapError
		if errors.As(err, &se) && se.changed {
			return fmt.Errorf("the binaries were swapped, but the unit files were not: %w", err)
		}
	}
	if berr := swapPair(p.Binary, p.Prev, p.Binary+swapTmpSuffix, false, func(int) error { return nil },
		0); berr != nil {
		return fmt.Errorf("swapping the unit files failed: %w; swapping the binaries back failed: %w", err, berr)
	}
	return fmt.Errorf("swapping the unit files failed: %w; the binaries were swapped back", err)
}

// swapPair swaps the contents of cur and other in three steps: copy cur to
// tmp; replace cur with other, by a rename, or, when cross (other may be on
// another file system), by a copy renamed over cur; rename tmp to other. cur
// exists at every instant. after is called after each step with base plus
// its number.
func swapPair(cur, other, tmp string, cross bool, after func(int) error, base int) error {
	if err := copyNew(cur, tmp); err != nil {
		return &swapError{err: fmt.Errorf("copy %s to %s: %w; nothing was changed", cur, tmp, err)}
	}
	if err := after(base + 1); err != nil {
		return err
	}
	unchanged := func(err error) error {
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		return &swapError{err: fmt.Errorf("replace %s with %s: %w; nothing was changed", cur, other, err)}
	}
	src := other
	if cross {
		src = cur + swapNewSuffix
		if err := copyNew(other, src); err != nil {
			return unchanged(err)
		}
	}
	if err := os.Rename(src, cur); err != nil {
		if cross {
			if rerr := os.Remove(src); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
		return unchanged(err)
	}
	half := func(err error) error {
		return &swapError{changed: true, err: fmt.Errorf("%w; %s now holds the content of %s, and the former %s "+
			"is at %s: rename it to %s by hand", err, cur, other, cur, tmp, other)}
	}
	if err := syncDir(filepath.Dir(cur)); err != nil {
		return half(err)
	}
	if err := after(base + 2); err != nil {
		return err
	}
	if err := os.Rename(tmp, other); err != nil {
		return half(err)
	}
	if err := syncDir(filepath.Dir(other)); err != nil {
		return &swapError{changed: true, err: fmt.Errorf("%s and %s were swapped, but %w", cur, other, err)}
	}
	return after(base + 3)
}

// lexists reports whether path names a file, without following a link.
func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// copyNew copies src to a new file dst with src's permissions, durably. An
// existing dst, the leftover of an interrupted swap, is removed first, and
// never followed if it is a symbolic link.
func copyNew(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	perm := fi.Mode().Perm()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := os.Remove(dst); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if _, err := io.Copy(f, in); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(perm); err != nil { // The umask may have narrowed it
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}
