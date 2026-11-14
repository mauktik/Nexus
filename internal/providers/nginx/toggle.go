package nginx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// Enable and disable (FR-9, FR-10) rename a site's file between its enabled
// and disabled names in place, with RenameNoReplace: the inode, and so the
// bytes, mode and owner, stay the same, and an existing name is never
// replaced. Nothing is ever deleted: every rollback is the rename back.
const (
	actionEnable  = "enable"
	actionDisable = "disable"
)

// toggleBinding binds op, enable or disable, to helper action.
func (p *Provider) toggleBinding(op def.Typed[spec.SiteInput, spec.SiteToggleResult], action string) provider.Binding {
	return provider.Detached(op, provider.DetachedSpec[spec.SiteInput, spec.SiteToggleResult]{
		Admit: p.admitToggle,
		Lane:  lane[spec.SiteInput],
		Prepare: func(_ context.Context, in spec.SiteInput, r *provider.Run) (detach.Spec, error) {
			return p.prepare(action, helperInput{Name: in.Name}, r)
		},
		Finalize: func(_ context.Context, _ spec.SiteInput, x detach.Exit, r *provider.Run) (spec.SiteToggleResult, error) {
			return finalize[spec.SiteToggleResult](p, action, x, r)
		},
		LostDetail: func(r *provider.Run, _ spec.SiteInput) string { return p.toggleLostDetail(action, r) },
	})
}

// admitToggle refuses, before any operation exists, a name that breaks the
// name rule (validation-failed), a site with neither file (not-found), and
// an entry the provider does not manage: a symlink, dangling or not, a
// non-regular file, a file not owned by root, or both files at once
// (nginx-config-unmanaged). The helper checks again, since the directory
// can change while the operation waits in the lane.
func (p *Provider) admitToggle(_ context.Context, in spec.SiteInput) error {
	s := p.Settings()
	if err := ValidName(in.Name, s); err != nil {
		return invalid("/name", err.Error())
	}
	dir, c, err := lookup(in.Name, s)
	if err != nil {
		return err
	}
	defer dir.Close()
	if !c.Managed {
		f := c.File()
		return problem.New(problem.NginxConfigUnmanaged, fmt.Sprintf("%s is not managed (%s): it is not renamed",
			f.FileName, c.Reason)).With("reason", c.Reason).With("fileName", f.FileName)
	}
	return nil
}

// toggleLostDetail is outcome-unknown's detail when the helper of an enable
// or disable ended without a result: which rename may have been made.
func (p *Provider) toggleLostDetail(action string, r *provider.Run) string {
	detail := fmt.Sprintf("the nginx helper for action %s stopped without reporting a result", action)
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detail
	}
	var in helperInput
	if ok, err := readJSON(dir, inputFile, &in); err != nil || !ok || in.Name == "" {
		return detail
	}
	t := newToggle(in.Name, in.Settings.settings(), action == actionEnable)
	return fmt.Sprintf("%s; %s may have been renamed to %s, which keeps its content, mode and owner", detail,
		t.from, t.to)
}

// toggle is one enable or disable of site name: the rename of from to to.
type toggle struct {
	name     string
	enable   bool
	from, to string
}

func newToggle(name string, s Settings, enable bool) toggle {
	t := toggle{name: name, enable: enable, from: s.EnabledFile(name), to: s.DisabledFile(name)}
	if enable {
		t.from, t.to = t.to, t.from
	}
	return t
}

func (t toggle) verb() string {
	if t.enable {
		return actionEnable
	}
	return actionDisable
}

// result is the toggle's result with file the site's file afterwards.
func (t toggle) result(changed bool, file string, reloaded bool, testOutput string,
	obs *spec.ReloadObservation) spec.SiteToggleResult {
	r := spec.SiteToggleResult{Name: t.name, FileName: file, Enabled: t.enable, Reloaded: reloaded,
		TestOutput: testOutput, Reload: obs}
	r.Changed = changed
	return r
}

func runEnable(ctx context.Context, e env, in helperInput) (any, *helperError) {
	return runToggle(ctx, e, in, true)
}

func runDisable(ctx context.Context, e env, in helperInput) (any, *helperError) {
	return runToggle(ctx, e, in, false)
}

// runToggle is the enable and disable action. Everything that can refuse
// without acting runs before the rename: the re-check of the site and, as
// a reload may follow, the identification of nginx's master. After the
// rename, a failed test or reload renames back, so a failure leaves
// includeDir as it was.
func runToggle(ctx context.Context, e env, in helperInput, enable bool) (any, *helperError) {
	s := in.Settings.settings()
	if err := ValidName(in.Name, s); err != nil {
		return nil, failure(problem.ValidationFailed, "site name %q: %v; nothing was changed", in.Name, err)
	}
	t := newToggle(in.Name, s, enable)
	dir, err := openIncludeDir(s)
	if err != nil {
		detail := err.Error()
		if p := (*problem.Error)(nil); errors.As(err, &p) {
			detail = p.Detail
		}
		return nil, failure(problem.DependencyUnavailable, "%s; nothing was changed", detail)
	}
	defer dir.Close()
	dirfd := int(dir.Fd())

	c, err := classify(dirfd, t.name, s)
	switch {
	case err != nil:
		return nil, failure(problem.DependencyUnavailable, "includeDir %s cannot be read: %v; nothing was changed",
			s.IncludeDir, err)
	case !c.Exists():
		return nil, failure(problem.NotFound, "no site %s: neither %s nor %s exists; nothing was changed", t.name,
			s.EnabledFile(t.name), s.DisabledFile(t.name))
	case !c.Managed:
		f := c.File()
		return nil, failure(problem.NginxConfigUnmanaged, "%s is not managed (%s): it is not renamed, and nothing "+
			"was changed", f.FileName, c.Reason).with("reason", c.Reason).with("fileName", f.FileName)
	}
	// A managed site has exactly one of its two files.
	done := c.Disabled
	if t.enable {
		done = c.Enabled
	}
	if done != nil {
		say(e.out, "nginx-op: %s is already %sd; nothing to do", done.FileName, t.verb())
		return t.result(false, done.FileName, false, "", nil), nil
	}
	orig := c.File()

	running, m, st, herr := resolveMaster(ctx, e, in)
	if herr != nil {
		return nil, herr
	}

	if err := RenameNoReplace(dirfd, t.from, t.to); err != nil {
		return nil, t.renameFailed(dirfd, orig, err)
	}
	say(e.out, "nginx-op: renamed %s to %s", t.from, t.to)

	say(e.out, "nginx-op: running %s -t", in.Nginx)
	out, err := e.nginx.Test(ctx)
	if err != nil {
		return nil, t.rollBack(e, dirfd, orig, failure(problem.DependencyUnavailable, "nginx -t with %s could not "+
			"run: %v", t.to, err))
	}
	if !out.Valid {
		say(e.out, "nginx-op: the configuration with %s is invalid", t.to)
		return nil, t.rollBack(e, dirfd, orig, failure(problem.NginxConfigInvalid, "nginx -t failed with %s: the "+
			"configuration is invalid", t.to).with("testOutput", out.Output))
	}
	say(e.out, "nginx-op: the configuration is valid")

	if !running {
		say(e.out, "nginx-op: nginx is %s; nothing was reloaded", st.State)
		return t.result(true, t.to, false, out.Output, nil), nil
	}
	obs, herr := reloadAndVerify(ctx, e, in, m)
	if herr != nil {
		return nil, t.reloadFailed(ctx, e, in, dirfd, orig, out.Output, obs, herr)
	}
	return t.result(true, t.to, true, out.Output, &obs), nil
}

// renameFailed is the failure of the toggle's rename. RenameNoReplace
// changes nothing when it fails, which is checked: a to that appeared since
// the re-check (EEXIST) is a conflict, any other failure that left the
// directory as it was is dependency-unavailable, and a directory that is
// not as it was is outcome-unknown.
func (t toggle) renameFailed(dirfd int, orig *fileStat, err error) *helperError {
	if errors.Is(err, unix.EEXIST) && t.fromIs(dirfd, orig) {
		return failure(problem.NginxConfigUnmanaged, "%s appeared while %s was being %sd: %s is not renamed, and "+
			"nothing was changed", t.to, t.name, t.verb(), t.from).with("reason", spec.ReasonConflict).
			with("fileName", t.to)
	}
	if !t.unchanged(dirfd, orig) {
		return failure(problem.OutcomeUnknown, "renaming %s to %s failed: %v; %s and %s are no longer as they were",
			t.from, t.to, err, t.from, t.to)
	}
	return failure(problem.DependencyUnavailable, "renaming %s to %s failed: %v; nothing was changed", t.from, t.to,
		err)
}

// fromIs reports whether from is still the entry orig.
func (t toggle) fromIs(dirfd int, orig *fileStat) bool {
	from, err := statAt(dirfd, t.from)
	return err == nil && from != nil && from.Dev == orig.Dev && from.Ino == orig.Ino
}

// unchanged reports whether from is still the entry orig and to is absent.
func (t toggle) unchanged(dirfd int, orig *fileStat) bool {
	to, err := statAt(dirfd, t.to)
	return err == nil && to == nil && t.fromIs(dirfd, orig)
}

// renameBack undoes the toggle's rename and proves it: from is the entry
// orig again, and to is absent.
func (t toggle) renameBack(e env, dirfd int, orig *fileStat) error {
	if err := RenameNoReplace(dirfd, t.to, t.from); err != nil {
		return err
	}
	if !t.unchanged(dirfd, orig) {
		return fmt.Errorf("after renaming %s back, %s is not the file it was", t.to, t.from)
	}
	say(e.out, "nginx-op: renamed %s back to %s", t.to, t.from)
	return nil
}

// rollBack renames back after herr, a failure before any reload, and states
// whether it did in herr's rolledBack. A rollback that fails leaves to in
// place, as the detail says; nginx was not reloaded, so it still runs the
// configuration it had.
func (t toggle) rollBack(e env, dirfd int, orig *fileStat, herr *helperError) *helperError {
	if err := t.renameBack(e, dirfd, orig); err != nil {
		herr.Detail += fmt.Sprintf("; renaming %s back to %s failed: %v, so %s is left in place (nginx was not "+
			"reloaded)", t.to, t.from, err, t.to)
		return herr.with("rolledBack", false)
	}
	herr.Detail += fmt.Sprintf("; %s was renamed back to %s, so nothing was changed", t.to, t.from)
	return herr.with("rolledBack", true)
}

// reloadFailed is Data Flow 2 step 5g for a toggle: herr is the failure of
// the reload of the renamed configuration, obs what it observed. When no
// reload was sent, the rename back is all there is to undo. Otherwise it
// renames back, tests, identifies the master again and reloads again: a
// confirmed second reload is the definite nginx-reload-failed with
// rolledBack and rollbackReload, as is an nginx no longer running (nothing
// to reload, rollbackReload absent). Anything else leaves which
// configuration nginx runs unknown.
func (t toggle) reloadFailed(ctx context.Context, e env, in helperInput, dirfd int, orig *fileStat, testOutput string,
	obs spec.ReloadObservation, herr *helperError) *helperError {
	if herr.Slug == problem.DependencyUnavailable {
		return t.rollBack(e, dirfd, orig, herr) // reloadAndVerify sent no reload
	}
	first := fmt.Sprintf("the reload with %s failed: %s", t.to, herr.Detail)
	unknown := func(format string, args ...any) *helperError {
		u := failure(problem.OutcomeUnknown, "%s; %s", first, fmt.Sprintf(format, args...))
		if obs.Verdict != "" {
			u = u.with("observed", obs)
		}
		return u
	}
	if err := t.renameBack(e, dirfd, orig); err != nil {
		return unknown("renaming %s back to %s failed: %v, so %s is left in place and which configuration nginx "+
			"runs is unknown", t.to, t.from, err, t.to)
	}
	say(e.out, "nginx-op: running %s -t", in.Nginx)
	again, err := e.nginx.Test(ctx)
	switch {
	case err != nil:
		return unknown("%s was renamed back to %s, but nginx -t could not run (%v), so nginx was not reloaded "+
			"again and which configuration it runs is unknown", t.to, t.from, err)
	case !again.Valid:
		return unknown("%s was renamed back to %s, but nginx -t then failed, so nginx was not reloaded again and "+
			"which configuration it runs is unknown: %s", t.to, t.from, strings.TrimSpace(again.Output))
	}
	running, m, st, herr2 := resolveMaster(ctx, e, in)
	if herr2 != nil {
		return unknown("%s was renamed back to %s, but nginx was not reloaded again: %s; which configuration it "+
			"runs is unknown", t.to, t.from, herr2.Detail)
	}
	failed := func(detail string) *helperError {
		f := failure(problem.NginxReloadFailed, "%s; %s", first, detail).with("rolledBack", true).
			with("testOutput", testOutput)
		if obs.Verdict != "" {
			f = f.with("observed", obs)
		}
		return f
	}
	if !running {
		say(e.out, "nginx-op: nginx is %s; there is nothing to reload", st.State)
		return failed(fmt.Sprintf("%s was renamed back to %s, and nginx is now %s, so there was nothing to reload",
			t.to, t.from, st.State))
	}
	back, herr3 := reloadAndVerify(ctx, e, in, m)
	if herr3 != nil {
		u := unknown("%s was renamed back to %s, but the reload that followed failed too: %s; which configuration "+
			"nginx runs is unknown", t.to, t.from, herr3.Detail)
		if back.Verdict != "" {
			u = u.with("rollbackReload", back)
		}
		return u
	}
	return failed(fmt.Sprintf("%s was renamed back to %s and nginx reloaded it (verdict %s)", t.to, t.from,
		back.Verdict)).with("rollbackReload", back)
}
