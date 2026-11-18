package nexusd

// The service helper's config set (F5 step 6; FR-7, FR-8). The helper is
// root, outside nexusd's sandbox and outside nexusd.service. It checks the
// file is still the one admission validated, re-applies the edit and
// re-validates it with the daemon's own check, backs the file up, replaces
// it atomically, and runs `systemctl reload`. It then waits for the
// daemon's reload status (reload-status.json, F6) to report the new file's
// digest applied. A refused or unconfirmed reload restores the backup and
// reloads again (D-4), so the disk holds the configuration in force. Each
// reload attempt has one deadline, reloadTimeout, from the helper's clock,
// which bounds its systemctl calls, its retries and its polling.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/reloadstatus"
)

const (
	modeConfigSet = "config-set"
	// reloadTimeout is one deadline per reload attempt, the first reload's
	// and the restore's (D-4, A-3).
	reloadTimeout       = 12 * time.Second
	reloadRetryInterval = time.Second
	reloadPollInterval  = 250 * time.Millisecond
	// configSetMaxRuntime is the helper unit's MaxRuntime: two attempts and
	// 30 s for the rest (A-5).
	configSetMaxRuntime = 2*reloadTimeout + 30*time.Second
)

// configRecoveryDoc is the manual recovery of a failed restore.
const configRecoveryDoc = "docs/install.md#recovering-from-a-failed-configuration-change"

// The steps at which a test's fault hook (helperRun.beforeStep) may stop the
// mode (NFR-4).
const (
	stepFaultBackup      = "backup"
	stepFaultAfterBackup = "after-backup"
	stepFaultAfterTmp    = "after-tmp"
	stepFaultAfterRename = "after-rename"
	stepFaultAfterReload = "after-reload"
)

// configSetter is one run of the config-set mode.
type configSetter struct {
	h       *helperRun
	in      helperInput
	dir     string // ConfigPath's directory
	name    string // ConfigPath's base name
	unit    string
	timeout time.Duration
	keys    string // The keys being set, for the log; never the values
}

// helperConfigSetMode is the helper's config set (F5 step 6).
func helperConfigSetMode(ctx context.Context, h *helperRun, in helperInput) helperResult {
	c := &configSetter{h: h, in: in, unit: in.Paths.withDefaults().Unit, timeout: in.ReloadTimeout}
	if c.timeout <= 0 {
		c.timeout = reloadTimeout
	}
	var keys []string
	for _, ch := range in.Changes {
		k, _, _ := strings.Cut(ch, "=")
		keys = append(keys, k)
	}
	c.keys = strings.Join(keys, ", ")
	return c.run(ctx)
}

func (c *configSetter) run(ctx context.Context) helperResult {
	in := c.in
	if !filepath.IsAbs(in.ConfigPath) || in.BackupPath != in.ConfigPath+".bak" || !filepath.IsAbs(in.StateDir) {
		return c.fail(problem.InternalError, fmt.Sprintf("input.json names configPath %q, backupPath %q and "+
			"stateDir %q; want absolute paths with backupPath = configPath + \".bak\"; nothing was changed",
			in.ConfigPath, in.BackupPath, in.StateDir))
	}
	c.dir, c.name = filepath.Dir(in.ConfigPath), filepath.Base(in.ConfigPath)
	step(c.h.out, fmt.Sprintf("config set: %s, keys %s", in.ConfigPath, c.keys))

	// 1. Temporary files of an earlier, killed run.
	c.removeStale()

	// 2. The file admission validated (FR-7 helper step 1).
	data, err := config.ReadFile(in.ConfigPath)
	if err != nil {
		return c.conflict(fmt.Sprintf("cannot read %s: %v; nothing was changed", in.ConfigPath, err))
	}
	if digest(data) != in.BaseSHA256 {
		return c.conflict(fmt.Sprintf("%s changed since the request was admitted; nothing was changed; run the "+
			"command again", in.ConfigPath))
	}
	fi, err := os.Stat(in.ConfigPath)
	if err != nil {
		return c.conflict(fmt.Sprintf("cannot read %s: %v; nothing was changed", in.ConfigPath, err))
	}
	perm := fi.Mode().Perm() & 0o600

	// 3. The deny-list again, the edit, and the daemon's own check.
	out, changes, errs := c.edit(data)
	if errs != nil {
		_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: the new configuration has %d error(s); nothing was changed\n",
			HelperCommand, len(errs)) // A lost log line never changes the outcome, as in step
		return helperResult{Error: &helperError{Slug: problem.ConfigInvalid, ConfigPath: in.ConfigPath, Errors: errs,
			Detail: fmt.Sprintf("the new %s has %d error(s); nothing was changed", in.ConfigPath, len(errs))}}
	}
	step(c.h.out, fmt.Sprintf("validated: keys %s", c.keys))

	// 4. The backup.
	if c.stopAt(stepFaultBackup) != nil {
		return helperResult{}
	}
	if err := writeFileAtomic(c.dir, filepath.Base(in.BackupPath), data, 0o600); err != nil {
		return c.fail(problem.InternalError, fmt.Sprintf("cannot write the backup %s: %v; %s was not changed",
			in.BackupPath, err, in.ConfigPath))
	}
	step(c.h.out, "backup: "+in.BackupPath)
	if c.stopAt(stepFaultAfterBackup) != nil {
		return helperResult{}
	}

	// 5. A change that landed while the backup was written (A-16).
	if now, err := config.ReadFile(in.ConfigPath); err != nil || digest(now) != in.BaseSHA256 {
		return c.conflict(fmt.Sprintf("%s changed while its backup was written; only the backup %s was written; "+
			"run the command again", in.ConfigPath, in.BackupPath))
	}

	// 6. The new file.
	err = writeFileAtomicHook(c.dir, c.name, out, perm, func() error { return c.stopAt(stepFaultAfterTmp) })
	if c.h.abort != nil {
		return helperResult{}
	}
	if err != nil {
		if now, rerr := os.ReadFile(in.ConfigPath); rerr != nil || digest(now) != digest(out) {
			return c.fail(problem.InternalError, fmt.Sprintf("cannot write %s: %v; it was not changed, and only "+
				"the backup %s was written", in.ConfigPath, err, in.BackupPath))
		}
		// Renamed, but not made durable: the reload decides as for any write.
		_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %s: %v\n", HelperCommand, in.ConfigPath, err)
	}
	step(c.h.out, fmt.Sprintf("write: %s (mode %04o), keys %s", in.ConfigPath, perm, c.keys))
	if c.stopAt(stepFaultAfterRename) != nil {
		return helperResult{}
	}

	// 7. The baseline: only a newer status answers this reload.
	base, hadBase, err := reloadstatus.Read(in.StateDir)
	if err != nil {
		_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %v; any reload status counts as newer\n", HelperCommand, err)
		hadBase = false
	}

	// 8. The reload.
	outSum := digest(out)
	st, ok, rerr := c.attempt(ctx, "reload", base, hadBase, func(reloadstatus.Status) bool { return true })
	if c.stopAt(stepFaultAfterReload) != nil {
		return helperResult{}
	}
	var why string
	switch {
	case ok && st.Applied && st.SHA256 == outSum:
		step(c.h.out, fmt.Sprintf("applied: keys %s (reload status seq %d)", c.keys, st.Seq))
		if len(st.RestartRequired) > 0 {
			step(c.h.out, "takes effect after a restart: "+strings.Join(st.RestartRequired, ", "))
		}
		return helperResult{Changed: true, ConfigChanges: changes, RestartRequired: st.RestartRequired,
			BackupPath: in.BackupPath}
	case ok && st.SHA256 != "" && st.SHA256 != outSum:
		step(c.h.out, fmt.Sprintf("conflict: the daemon read another %s (reload status seq %d)", in.ConfigPath,
			st.Seq))
		return c.conflict(fmt.Sprintf("another change replaced %s before the reload; it was kept, not restored; "+
			"the backup %s holds the file before this change", in.ConfigPath, in.BackupPath))
	case ok:
		why = fmt.Sprintf("the daemon refused the new configuration at stage %s: %s", st.Stage, st.Error)
		step(c.h.out, fmt.Sprintf("refused: stage %s (reload status seq %d)", st.Stage, st.Seq))
	default:
		why = fmt.Sprintf("the reload of the new configuration was not confirmed within %s: %v", c.timeout, rerr)
		step(c.h.out, fmt.Sprintf("unconfirmed: no reload status for the new %s within %s", in.ConfigPath,
			c.timeout))
	}
	return c.restore(ctx, data, perm, base, hadBase, why)
}

// restore writes the backup bytes back and confirms that the daemon
// applied them, against the same baseline, with a fresh deadline.
func (c *configSetter) restore(ctx context.Context, data []byte, perm fs.FileMode, base reloadstatus.Status,
	hadBase bool, why string) helperResult {
	in := c.in
	step(c.h.out, fmt.Sprintf("restore: %s from %s", in.ConfigPath, in.BackupPath))
	if err := writeFileAtomic(c.dir, c.name, data, perm); err != nil {
		return c.restoreFailed(fmt.Sprintf("%s; writing the backup back failed: %v", why, err))
	}
	bakSum := digest(data)
	st, ok, err := c.attempt(ctx, "restore", base, hadBase, func(s reloadstatus.Status) bool {
		return s.SHA256 == bakSum
	})
	switch {
	case ok && st.Applied:
		step(c.h.out, fmt.Sprintf("restored: %s is in force again (reload status seq %d)", in.ConfigPath, st.Seq))
		res := c.fail(problem.ConfigApplyFailed, fmt.Sprintf("%s; %s was restored from %s and is in force",
			why, in.ConfigPath, in.BackupPath))
		res.Restored, res.BackupPath = true, in.BackupPath
		return res
	case ok:
		return c.restoreFailed(fmt.Sprintf("%s; the daemon refused the restored file too, at stage %s: %s", why,
			st.Stage, st.Error))
	}
	return c.restoreFailed(fmt.Sprintf("%s; the restore was not confirmed within %s: %v", why, c.timeout, err))
}

// attempt is one reload attempt (D-4): `systemctl reload`, retried every
// reloadRetryInterval while the unit is not active, then a wait, polling
// every reloadPollInterval, for a status newer than base that ends it.
// One deadline, from the helper's clock, bounds all of it. ok is false when
// no status ended the attempt; err then says why.
func (c *configSetter) attempt(ctx context.Context, what string, base reloadstatus.Status, hadBase bool,
	ends func(reloadstatus.Status) bool) (reloadstatus.Status, bool, error) {
	deadline := c.h.sys.Now().Add(c.timeout)
	actx, cancel := c.h.sys.WithDeadline(ctx, deadline)
	defer cancel()
	step(c.h.out, fmt.Sprintf("%s: systemctl reload %s, confirmed within %s", what, c.unit, c.timeout))
	for {
		err := c.h.sys.Reload(actx)
		if err == nil {
			break
		}
		_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %s: %v\n", HelperCommand, what, err)
		if !errors.Is(err, errUnitNotActive) || actx.Err() != nil {
			return reloadstatus.Status{}, false, err
		}
		if c.sleep(actx, deadline, reloadRetryInterval) != nil {
			return reloadstatus.Status{}, false, fmt.Errorf("%s did not become active: %w", c.unit, err)
		}
	}
	var skipped reloadstatus.Status
	for {
		s, ok, err := reloadstatus.Read(c.in.StateDir)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %s: %v\n", HelperCommand, what, err)
		case ok && s.After(base, hadBase):
			if ends(s) {
				return s, true, nil
			}
			if s.Boot != skipped.Boot || s.Seq != skipped.Seq {
				skipped = s
				step(c.h.out, fmt.Sprintf("%s: skipped reload status seq %d (applied %t, sha256 %q): not the "+
					"backup's digest", what, s.Seq, s.Applied, s.SHA256))
			}
		}
		if c.sleep(actx, deadline, reloadPollInterval) != nil {
			return reloadstatus.Status{}, false, errors.New("no reload status answered it")
		}
	}
}

// sleep waits d on the helper's clock, but never past deadline; it fails
// once the deadline is reached.
func (c *configSetter) sleep(ctx context.Context, deadline time.Time, d time.Duration) error {
	if left := deadline.Sub(c.h.sys.Now()); left < d {
		d = left
	}
	if d <= 0 {
		return context.DeadlineExceeded
	}
	return c.h.sys.Sleep(ctx, d)
}

// edit checks each assignment against the deny-list, applies them with
// config.Edit and validates the result with the daemon's check. Errors are
// config-invalid's errors[]; nil means out is valid.
func (c *configSetter) edit(data []byte) ([]byte, []spec.ConfigChange, []spec.ConfigError) {
	var errs []spec.ConfigError
	var as []config.Assignment
	for _, ch := range c.in.Changes {
		a, err := config.ParseAssignment(ch)
		if err != nil {
			errs = append(errs, spec.ConfigError{Message: err.Error()})
			continue
		}
		if d, denied := deniedChange(a); denied {
			errs = append(errs, spec.ConfigError{Path: a.Key, Message: fmt.Sprintf("%s cannot be set remotely: %s",
				a.Key, d.Reason)})
			continue
		}
		as = append(as, a)
	}
	if len(errs) > 0 {
		return nil, nil, errs
	}
	out, changes, err := config.Edit(data, c.in.ConfigPath, as)
	if err != nil {
		var list config.ErrorList
		if errors.As(err, &list) {
			return nil, nil, configErrors(list)
		}
		return nil, nil, []spec.ConfigError{{Message: err.Error()}}
	}
	if list := c.h.check(out, c.in.ConfigPath); len(list) > 0 {
		return nil, nil, configErrors(list)
	}
	res := make([]spec.ConfigChange, 0, len(changes))
	for _, ch := range changes {
		res = append(res, spec.ConfigChange{Key: ch.Key, Old: ch.Old, New: ch.New})
	}
	return out, res, nil
}

// deniedChange is the deny-list entry refusing a, if any (FR-12).
func deniedChange(a config.Assignment) (spec.ConfigDeny, bool) {
	for _, d := range spec.ConfigDenyList {
		match := a.Key == d.Key || (d.Subtree && strings.HasPrefix(a.Key, d.Key+"."))
		if match && (d.Value == "" || a.Value == d.Value) {
			return d, true
		}
	}
	return spec.ConfigDeny{}, false
}

// removeStale removes the temporary files of ConfigPath and its backup
// that a killed run left: .<name>.*.tmp, which covers .<name>.bak.*.tmp.
func (c *configSetter) removeStale() {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %v\n", HelperCommand, err)
		return
	}
	prefix := "." + c.name + "."
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".tmp") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(c.dir, n)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %v\n", HelperCommand, err)
			continue
		}
		step(c.h.out, "removed stale temporary file "+p)
	}
}

// stopAt runs the fault hook at a named step. A hook error stops the mode
// with no result, as KillProcess would.
func (c *configSetter) stopAt(name string) error {
	if c.h.beforeStep == nil {
		return nil
	}
	if err := c.h.beforeStep(name); err != nil {
		c.h.abort = fmt.Errorf("stopped at step %s: %w", name, err)
		return c.h.abort
	}
	return nil
}

func (c *configSetter) fail(slug problem.Slug, detail string) helperResult {
	return helperResult{Error: &helperError{Slug: slug, Detail: detail}}
}

func (c *configSetter) conflict(detail string) helperResult {
	_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: %s\n", HelperCommand, detail)
	return c.fail(problem.ConfigConflict, detail)
}

// restoreFailed is config-restore-failed: the disk may not hold the
// configuration in force.
func (c *configSetter) restoreFailed(why string) helperResult {
	in := c.in
	_, _ = fmt.Fprintf(c.h.errOut, "nexusd %s: the restore of %s was not confirmed\n", HelperCommand, in.ConfigPath)
	res := c.fail(problem.ConfigRestoreFailed, fmt.Sprintf("%s. The configuration in force may differ from %s. "+
		"Recover by hand as %s describes: run `nexusd validate-config`, copy %s to %s if it is not valid, then run "+
		"`systemctl reload %s` and check `journalctl -u %s`", strings.TrimSuffix(why, "."), in.ConfigPath,
		configRecoveryDoc, in.BackupPath, in.ConfigPath, c.unit, c.unit))
	res.BackupPath = in.BackupPath
	return res
}

// digest is the hex SHA-256 of b, as the reload status records it.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic replaces dir/name with data at perm, owned by root when
// the helper is root: a temporary file .<name>.*.tmp in dir, chown 0:0,
// chmod, write, fsync, rename, then a directory fsync. A reader sees the
// old file or the complete new one, never a partial one.
func writeFileAtomic(dir, name string, data []byte, perm fs.FileMode) error {
	return writeFileAtomicHook(dir, name, data, perm, nil)
}

// writeFileAtomicHook is writeFileAtomic with a hook between the temporary
// file and the rename. A hook error leaves the temporary file, as a kill
// would; the next run removes it (removeStale).
func writeFileAtomicHook(dir, name string, data []byte, perm fs.FileMode, beforeRename func() error) (err error) {
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tmp := f.Name()
	keep := false
	defer func() {
		if err != nil && !keep {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	if os.Geteuid() == 0 {
		if err := f.Chown(0, 0); err != nil {
			return errors.Join(fmt.Errorf("chown %s: %w", tmp, err), f.Close())
		}
	}
	if err := f.Chmod(perm); err != nil {
		return errors.Join(fmt.Errorf("chmod %s: %w", tmp, err), f.Close())
	}
	if _, err := f.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", tmp, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", tmp, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			keep = true
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return syncDir(dir)
}
