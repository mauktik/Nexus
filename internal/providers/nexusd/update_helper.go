// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// The service helper's update (F6) and its automatic rollback (F7). The
// helper is the old binary, root, outside nexusd's sandbox and outside
// nexusd.service. It checks that the unit is active, backs up the running
// binary (from /proc/<MainPID>/exe, verified by -version) and the unit
// file, runs the package's install.sh, which restarts an active service
// itself (A-33), restarts the unit once only when install.sh did not, and
// runs the FR-10 health check against the package's version, excluding the
// old process. A failure from install.sh on restores the backup by copy
// (A-21), so nexusd.prev keeps the version that ran before, reloads
// systemd, clears a failed state, restarts and checks health against the
// version that ran before. Every outcome is a result: update-failed with
// rolledBack, or update-rollback-failed when the restore is not healthy
// either. runHelper removes the extracted package once result.json is
// written (A-50).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// The update's steps, as the result's step and the log lines name them.
const (
	stepPrecheck = "precheck"
	stepBackup   = "backup"
	stepInstall  = "install"
	stepRestart  = "restart"
	stepHealth   = "health"
	stepRestore  = "restore"
)

// prevTmpSuffix names the backup before its version is verified:
// /usr/local/bin/nexusd.prev.tmp.
const prevTmpSuffix = ".tmp"

// installScript is the package's installer, run from the package
// directory.
const installScript = "install.sh"

// recoveryDoc is the manual recovery of a failed update's restore.
const recoveryDoc = "docs/install.md#recovering-from-a-failed-update"

// updater is one run of the update mode.
type updater struct {
	h     *helperRun
	in    helperInput
	paths Paths
}

// helperUpdateMode is the helper's update (F6, F7).
func helperUpdateMode(ctx context.Context, h *helperRun, in helperInput) helperResult {
	if in.NoChange {
		step(h.out, fmt.Sprintf("nexusd %s is already running; nothing to install", in.RunningVersion))
		// Both versions are the running one, so the result tells what ran.
		return helperResult{PreviousVersion: in.RunningVersion, NewVersion: in.RunningVersion}
	}
	u := &updater{h: h, in: in, paths: in.Paths.withDefaults()}
	return u.run(ctx)
}

func (u *updater) run(ctx context.Context) helperResult {
	unit := u.paths.Unit
	pid0, err := u.precheck(ctx)
	if err != nil {
		return u.unchanged(stepPrecheck, fmt.Sprintf("%v; nothing was changed", err))
	}
	step(u.h.out, fmt.Sprintf("%s: %s is active, MainPID %d", stepPrecheck, unit, pid0))

	if err := u.backup(ctx, pid0); err != nil {
		return u.unchanged(stepBackup, fmt.Sprintf("the backup of the running nexusd failed: %v; nexusd was not "+
			"changed", err))
	}
	step(u.h.out, fmt.Sprintf("%s: %s (nexusd %s, from /proc/%d/exe)%s", stepBackup, u.paths.Prev,
		u.in.RunningVersion, pid0, u.savedUnitNote()))

	timeout := u.in.InstallTimeout
	if timeout <= 0 {
		timeout = installTimeout
	}
	script := filepath.Join(u.in.PackageDir, installScript)
	step(u.h.out, fmt.Sprintf("%s: %s, nexusd %s, within %s", stepInstall, script, u.in.ExpectedVersion, timeout))
	installStart := u.h.sys.Now()
	if err := u.install(ctx, script, timeout); err != nil {
		return u.rollback(ctx, stepInstall, err.Error())
	}

	st, err := u.show(ctx)
	if err != nil {
		return u.rollback(ctx, stepRestart, fmt.Sprintf("cannot read %s after install.sh: %v", unit, err))
	}
	if st.MainPID == pid0 {
		// install.sh restarts only an active service (A-33); it did not, so
		// the helper does, once.
		step(u.h.out, fmt.Sprintf("%s: install.sh did not restart %s (MainPID %d); restarting it", stepRestart,
			unit, pid0))
		step(u.h.out, "restart-started "+u.stamp())
		if err := u.restart(ctx); err != nil {
			return u.rollback(ctx, stepRestart, fmt.Sprintf("systemd refused to restart %s: %v", unit, err))
		}
	} else {
		// The restart ran inside install.sh, at a moment the helper does not
		// see: the start of install.sh bounds it from below.
		step(u.h.out, fmt.Sprintf("%s: install.sh restarted %s (MainPID %d, was %d)", stepRestart, unit,
			st.MainPID, pid0))
		step(u.h.out, "restart-started "+installStart.UTC().Format(time.RFC3339Nano))
	}

	step(u.h.out, fmt.Sprintf("%s: %s within %s, expecting nexusd %s", stepHealth, u.in.Health.URL,
		u.in.Health.Timeout, u.in.ExpectedVersion))
	if _, err := waitHealthy(ctx, u.h.sys, u.in.Health, u.in.ExpectedVersion, pid0); err != nil {
		_, why := failureCondition(err)
		return u.rollback(ctx, stepHealth, why)
	}
	step(u.h.out, "healthy "+u.stamp())
	step(u.h.out, fmt.Sprintf("%s is healthy, running nexusd %s", unit, u.in.ExpectedVersion))
	return helperResult{Changed: true, PreviousVersion: u.in.RunningVersion, NewVersion: u.in.ExpectedVersion}
}

// stamp is the system's time for the NFR-1 lines.
func (u *updater) stamp() string { return u.h.sys.Now().UTC().Format(time.RFC3339Nano) }

// savedUnitNote names the unit file's backup in the backup step's line.
func (u *updater) savedUnitNote() string {
	if u.in.SavedUnit == "" {
		return ""
	}
	return fmt.Sprintf(" and %s", u.in.SavedUnit)
}

// failed is a result of the update.
func (u *updater) failed(slug problem.Slug, rolledBack bool, stepName, detail string) helperResult {
	_, _ = fmt.Fprintln(u.h.errOut, detail) // A lost log line never changes the outcome, as in step
	return helperResult{RolledBack: rolledBack, PreviousVersion: u.in.RunningVersion,
		NewVersion: u.in.ExpectedVersion, Error: &helperError{Slug: slug, Detail: detail, Step: stepName}}
}

// unchanged is the update-failed of a precheck or backup, before
// install.sh: nothing needs restoring.
func (u *updater) unchanged(stepName, detail string) helperResult {
	return u.failed(problem.UpdateFailed, false, stepName, detail)
}

// bounded runs one systemctl call under the helper's per-call bound, so a
// call that hangs still ends in a result (A-8).
func (u *updater) bounded(ctx context.Context, what string, call func(context.Context) error) error {
	d := u.h.callTimeout
	if d <= 0 {
		d = systemctlTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := call(cctx)
	if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%s did not return within %s: %w", what, d, err)
	}
	return err
}

func (u *updater) show(ctx context.Context) (unitState, error) {
	var st unitState
	err := u.bounded(ctx, "systemctl show "+u.paths.Unit, func(ctx context.Context) error {
		var err error
		st, err = u.h.sys.Show(ctx)
		return err
	})
	return st, err
}

func (u *updater) restart(ctx context.Context) error {
	return u.bounded(ctx, "systemctl restart "+u.paths.Unit, u.h.sys.Restart)
}

// precheck returns the MainPID of the active unit (F6 step 2).
func (u *updater) precheck(ctx context.Context) (int, error) {
	st, err := u.show(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot read %s: %w", u.paths.Unit, err)
	}
	if st.ActiveState != "active" || st.MainPID <= 0 {
		return 0, fmt.Errorf("%s is %s/%s (MainPID %d), not active: update a running nexusd only", u.paths.Unit,
			st.ActiveState, st.SubState, st.MainPID)
	}
	return st.MainPID, nil
}

// backup copies the running image, /proc/<pid0>/exe, to nexusd.prev.tmp,
// verifies that it reports runningVersion, copies the unit file to the
// saved unit, then renames the copy onto nexusd.prev (F6 step 3, A-20).
// On an error nexusd.prev.tmp is gone and Paths.Binary and Paths.UnitFile
// are untouched.
func (u *updater) backup(ctx context.Context, pid0 int) (err error) {
	exe := filepath.Join(u.h.procDir, strconv.Itoa(pid0), "exe")
	tmp := u.paths.Prev + prevTmpSuffix
	if err := copyNew(exe, tmp); err != nil {
		return fmt.Errorf("copy %s to %s: %w", exe, tmp, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
	}()
	v, err := u.h.sys.ExeVersion(ctx, tmp)
	if err != nil {
		return fmt.Errorf("the copy of the running nexusd does not report its version: %w", err)
	}
	if v != u.in.RunningVersion {
		return fmt.Errorf("the running process %d is nexusd %s, not the running version nexusd %s", pid0, v,
			u.in.RunningVersion)
	}
	if u.in.SavedUnit != "" {
		if err := os.MkdirAll(filepath.Dir(u.in.SavedUnit), 0o700); err != nil {
			return fmt.Errorf("save the unit file: %w", err)
		}
		fi, err := os.Stat(u.paths.UnitFile)
		if err != nil {
			return fmt.Errorf("save the unit file: %w", err)
		}
		if err := copyAtomic(u.paths.UnitFile, u.in.SavedUnit, fi.Mode().Perm()); err != nil {
			return fmt.Errorf("save the unit file %s to %s: %w", u.paths.UnitFile, u.in.SavedUnit, err)
		}
	}
	if err := os.Rename(tmp, u.paths.Prev); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, u.paths.Prev, err)
	}
	return syncDir(filepath.Dir(u.paths.Prev))
}

// install runs install.sh within timeout (F6 step 4, A-7). Its output
// streams into the op log. A non-zero exit, a timeout or a failure to run
// is an error naming it.
func (u *updater) install(ctx context.Context, script string, timeout time.Duration) error {
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := u.h.sys.RunInstall(ictx, script, u.in.PackageDir, u.h.out)
	switch {
	case errors.Is(ictx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return fmt.Errorf("install.sh timed out after %s and was killed", timeout)
	case err != nil:
		return fmt.Errorf("install.sh could not run: %w", err)
	case code != 0:
		return fmt.Errorf("install.sh failed with exit code %d (its output is in the operation's log)", code)
	}
	return nil
}

// rollback restores the version that ran before after a failure at
// failedStep (F7) and returns the result: update-failed with rolledBack
// when the restored nexusd is healthy, update-rollback-failed otherwise.
func (u *updater) rollback(ctx context.Context, failedStep, why string) helperResult {
	first := fmt.Sprintf("the update to nexusd %s failed at step %s: %s", u.in.ExpectedVersion, failedStep, why)
	_, _ = fmt.Fprintln(u.h.errOut, first) // A lost log line never changes the outcome, as in step
	step(u.h.out, fmt.Sprintf("%s: nexusd %s from %s%s", stepRestore, u.in.RunningVersion, u.paths.Prev,
		u.savedUnitNote()))
	if err := u.restore(ctx); err != nil {
		return u.failed(problem.UpdateRollbackFailed, false, stepRestore, fmt.Sprintf("%s; restoring nexusd %s "+
			"failed too: %v. %s", first, u.in.RunningVersion, err, u.recovery()))
	}
	step(u.h.out, "healthy "+u.stamp())
	step(u.h.out, fmt.Sprintf("%s is healthy again, running nexusd %s", u.paths.Unit, u.in.RunningVersion))
	return u.failed(problem.UpdateFailed, true, failedStep, fmt.Sprintf("%s; nexusd %s was restored and is "+
		"running and healthy", first, u.in.RunningVersion))
}

// restore copies nexusd.prev over the binary, keeping nexusd.prev (A-21),
// and the saved unit over the unit file, then reloads systemd, clears a
// failed state (at its start limit systemd accepts a --no-block restart
// but refuses the start), restarts and checks health against the version
// that ran before, excluding the MainPID right before the restart (F7
// steps 1-3); the health check is what detects a start that fails.
func (u *updater) restore(ctx context.Context) error {
	if err := copyOver(u.paths.Prev, u.paths.Binary); err != nil {
		return fmt.Errorf("copy %s to %s: %w", u.paths.Prev, u.paths.Binary, err)
	}
	if u.in.SavedUnit != "" {
		if err := copyOver(u.in.SavedUnit, u.paths.UnitFile); err != nil {
			return fmt.Errorf("copy %s to %s: %w", u.in.SavedUnit, u.paths.UnitFile, err)
		}
	}
	if err := u.bounded(ctx, "systemctl daemon-reload", u.h.sys.DaemonReload); err != nil {
		return err
	}
	step(u.h.out, fmt.Sprintf("%s: reset-failed %s, clearing a failed state and the start limit", stepRestore,
		u.paths.Unit))
	if err := u.bounded(ctx, "systemctl reset-failed "+u.paths.Unit, u.h.sys.ResetFailed); err != nil {
		return err
	}
	st, err := u.show(ctx)
	if err != nil {
		return err
	}
	step(u.h.out, fmt.Sprintf("%s: restarting %s onto nexusd %s (MainPID %d)", stepRestore, u.paths.Unit,
		u.in.RunningVersion, st.MainPID))
	step(u.h.out, "restart-started "+u.stamp())
	if err := u.restart(ctx); err != nil {
		return err
	}
	step(u.h.out, fmt.Sprintf("%s: health check: %s within %s, expecting nexusd %s", stepRestore,
		u.in.Health.URL, u.in.Health.Timeout, u.in.RunningVersion))
	if _, err := waitHealthy(ctx, u.h.sys, u.in.Health, u.in.RunningVersion, st.MainPID); err != nil {
		_, why := failureCondition(err)
		return errors.New(why)
	}
	return nil
}

// copyOver replaces dst atomically with a copy of src, with src's
// permissions.
func copyOver(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return copyAtomic(src, dst, fi.Mode().Perm())
}

// recovery is update-rollback-failed's guidance: nexusd may be down.
func (u *updater) recovery() string {
	p := u.paths
	unitFile := ""
	if u.in.SavedUnit != "" {
		unitFile = fmt.Sprintf(" and %s to %s", u.in.SavedUnit, p.UnitFile)
	}
	return fmt.Sprintf("nexusd may be down. Recover by hand as %[1]s describes: `journalctl -u %[2]s` shows why "+
		"it fails; copy %[3]s (nexusd %[4]s) to %[5]s%[6]s, then run `systemctl daemon-reload`, `systemctl "+
		"reset-failed %[2]s` and `systemctl restart %[2]s`, and check `%[5]s -version` and `systemctl status %[2]s`",
		recoveryDoc, p.Unit, p.Prev, u.in.RunningVersion, p.Binary, unitFile)
}

// removePackage removes the operation's extracted package, ops/<id>/pkg,
// which holds packageDir. A failure is logged and changes nothing else:
// Finalize removes it again, and the op's retention at the latest (A-50).
func removePackage(dir string, out, errOut io.Writer) {
	pkg := filepath.Join(dir, pkgDirName)
	if err := os.RemoveAll(pkg); err != nil {
		_, _ = fmt.Fprintf(errOut, "nexusd %s: cannot remove the extracted package %s: %v\n", HelperCommand, pkg,
			err) // A lost log line never changes the outcome, as in step
		return
	}
	step(out, "removed "+pkg)
}
