package nginx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// Add and update (FR-11) write a site's content (Data Flow 2). nexusd only
// checks and stages: Admit refuses what it can before an operation exists,
// and Prepare writes the content into the operation's directory. The helper
// checks again, then backs up the file, writes it atomically, tests it
// (live when the site is enabled, in a shadow when it is disabled), and
// reloads an enabled site when nginx runs. A failed test or reload restores
// the backup, so includeDir is left as it was. Add is update with create:
// the site must not exist yet.
const (
	actionAdd    = "add"
	actionUpdate = "update"
)

// contentFile is the content of an add or update in its operation's
// directory (0600). Prepare writes it, and the helper removes it once it
// has a result. The content never goes into input.json, the operation's
// record, the audit log or an argv (FR-12, FR-15).
const contentFile = "content"

// writtenMode is the mode of every file a write leaves (FR-11 step 3).
const writtenMode = 0o644

// managedGID is the group a file a write leaves gets: root's. Only this
// package's tests change it, with managedUID (names.go), to their own.
var managedGID uint32

// writeRequest is add's or update's input as Admit and Prepare see it.
type writeRequest struct {
	Name    string
	Content string
	IfMatch string
	Enabled *bool
	WhatIf  bool
	Create  bool // add: the site must not exist yet
}

func addRequest(in spec.AddInput) writeRequest {
	return writeRequest{Name: in.Name, Content: in.Content, Enabled: in.Enabled, WhatIf: in.WhatIf, Create: true}
}

func updateRequest(in spec.WriteInput) writeRequest {
	return writeRequest{Name: in.Name, Content: in.Content, IfMatch: in.IfMatch, Enabled: in.Enabled,
		WhatIf: in.WhatIf}
}

// mustNotExist reports whether the write may only create: add, or update
// with ifMatch none.
func (r writeRequest) mustNotExist() bool { return r.Create || r.IfMatch == spec.IfMatchNone }

// writeBinding binds op, add or update, to helper action. req turns its
// input into a writeRequest. Finalize reads only result.json: it is given
// the record's input, whose content is redacted.
func writeBinding[In any](p *Provider, op def.Typed[In, spec.WriteResult], action string,
	req func(In) writeRequest) provider.Binding {
	return provider.Detached(op, provider.DetachedSpec[In, spec.WriteResult]{
		Admit: func(_ context.Context, in In) error { return p.admitWrite(req(in)) },
		Lane:  lane[In],
		Prepare: func(_ context.Context, in In, r *provider.Run) (detach.Spec, error) {
			return p.prepareWrite(action, req(in), r)
		},
		Finalize: func(_ context.Context, _ In, x detach.Exit, r *provider.Run) (spec.WriteResult, error) {
			return finalize[spec.WriteResult](p, action, x, r)
		},
		LostDetail: func(r *provider.Run, _ In) string { return p.lostDetail(action, r) },
	})
}

// admitWrite refuses, before any operation exists (Data Flow 2 step 2): a
// name that breaks the name rule, content that is not UTF-8, holds a NUL or
// exceeds maxConfigBytes, and, from an lstat of the site's two names, an
// entry the provider does not manage, a stale ifMatch, an add or ifMatch
// none of an existing site, and an enabled that differs from an existing
// site's state. The helper checks all of it again.
func (p *Provider) admitWrite(req writeRequest) error {
	s := p.Settings()
	if err := ValidName(req.Name, s); err != nil {
		return invalidField("/name", err.Error())
	}
	if msg := contentProblem(req.Content, s.MaxConfigBytes); msg != "" {
		return invalidField("/content", msg)
	}
	dir, err := openIncludeDir(s)
	if err != nil {
		return err
	}
	defer dir.Close()
	dirfd := int(dir.Fd())
	c, err := classify(dirfd, req.Name, s)
	if err != nil {
		return unavailable(fmt.Sprintf("includeDir %s cannot be read: %v", s.IncludeDir, err))
	}
	if _, perr := checkWrite(dirfd, c, req, s); perr != nil {
		return perr
	}
	return nil
}

// invalidField is validation-failed of one input field, with the message
// as the detail too, so a refusal inside the operation says the same.
func invalidField(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, msg).
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

// contentProblem is why content cannot be written, or "". Its size is in
// bytes, as maxConfigBytes is.
func contentProblem(content string, max int64) string {
	switch {
	case int64(len(content)) > max:
		return fmt.Sprintf("content is %d bytes, more than maxConfigBytes (%d bytes)", len(content), max)
	case strings.IndexByte(content, 0) >= 0:
		return "content must not contain a NUL byte"
	case !utf8.ValidString(content):
		return "content must be UTF-8 text"
	}
	return ""
}

// writeTarget is the file a write replaces or creates.
type writeTarget struct {
	exists  bool   // The site exists, in one state
	enabled bool   // The state the site has, or will have when created
	file    string // The name written: the enabled or the disabled one
	sha256  string // The current content's, when ifMatch needed it
}

// checkWrite applies Data Flow 2 step 2's checks of the site c to req,
// against the directory dirfd, and returns the file the write targets.
func checkWrite(dirfd int, c site, req writeRequest, s Settings) (writeTarget, *problem.Error) {
	t := writeTarget{exists: c.Exists()}
	if t.exists && !c.Managed {
		f := c.File()
		return t, problem.New(problem.NginxConfigUnmanaged, fmt.Sprintf("%s is not managed (%s): it is not written",
			f.FileName, c.Reason)).With("reason", c.Reason).With("fileName", f.FileName)
	}
	switch {
	case t.exists && req.Create:
		return t, problem.New(problem.Conflict, fmt.Sprintf("site %s exists (%s): add only creates a site; use "+
			"update to write it", req.Name, c.File().FileName))
	case t.exists && req.IfMatch == spec.IfMatchNone:
		return t, problem.New(problem.Conflict, fmt.Sprintf("site %s exists (%s), and ifMatch none writes only a "+
			"site that does not", req.Name, c.File().FileName))
	case !t.exists && req.IfMatch != "" && req.IfMatch != spec.IfMatchNone:
		return t, problem.New(problem.Conflict, fmt.Sprintf("site %s does not exist, so no content matches "+
			"ifMatch %s", req.Name, req.IfMatch))
	}
	t.enabled = req.Enabled == nil || *req.Enabled
	if t.exists {
		t.enabled = c.Enabled != nil
		if req.Enabled != nil && *req.Enabled != t.enabled {
			return t, enabledMismatch(req.Name, t.enabled)
		}
	}
	t.file = s.DisabledFile(req.Name)
	if t.enabled {
		t.file = s.EnabledFile(req.Name)
	}
	if t.exists && req.IfMatch != "" && req.IfMatch != spec.IfMatchNone {
		_, digest, err := readManaged(dirfd, c.File(), s.MaxConfigBytes)
		if err != nil {
			var p *problem.Error
			if errors.As(err, &p) {
				return t, p
			}
			return t, unavailable(err.Error())
		}
		if digest != req.IfMatch {
			return t, problem.New(problem.Conflict, fmt.Sprintf("%s has sha256 %s, not ifMatch %s: it changed "+
				"since it was read", t.file, digest, req.IfMatch))
		}
		t.sha256 = digest
	}
	return t, nil
}

// enabledMismatch refuses an enabled that would change an existing site's
// state: update keeps it, and enable and disable change it (L-37).
func enabledMismatch(name string, enabled bool) *problem.Error {
	now, verb := "disabled", "start"
	if enabled {
		now, verb = "enabled", "stop"
	}
	msg := fmt.Sprintf("site %s is %s, and a write keeps an existing site's state: enabled applies only to a site "+
		"it creates; use nexus nginxsite %s %s to change it", name, now, verb, name)
	return invalidField("/enabled", msg)
}

// prepareWrite writes the content (0600) and input.json into the
// operation's directory and returns the helper's spec. A failure removes
// the content again. The content's size and SHA-256 (FR-15) are logged by
// the helper: the operation log of a detached operation is the helper's
// output, and the operations manager gives Prepare a discarded one.
func (p *Provider) prepareWrite(action string, req writeRequest, r *provider.Run) (sp detach.Spec, err error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("nginx: create %s: %w", dir, err)
	}
	if err := writeNew(filepath.Join(dir, contentFile), []byte(req.Content)); err != nil {
		p.log.Warn("nginx operation not prepared", "operationId", r.ID(), "action", action, "error", err.Error())
		return detach.Spec{}, fmt.Errorf("nginx: write the content: %w", err)
	}
	defer func() {
		if err != nil {
			if rerr := removeContent(dir); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
	}()
	return p.prepare(action, helperInput{Name: req.Name, Create: req.Create, IfMatch: req.IfMatch,
		Enabled: req.Enabled, WhatIf: req.WhatIf}, r)
}

// removeContent removes the operation's content file, if there is one.
func removeContent(dir string) error {
	if err := os.Remove(filepath.Join(dir, contentFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("nginx: remove the content: %w", err)
	}
	return nil
}

// readContent reads the operation's content file, a regular file opened
// without following a symlink, and checks it again.
func readContent(dir string, max int64) ([]byte, *helperError) {
	f, err := os.OpenFile(filepath.Join(dir, contentFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, failure(problem.InternalError, "the helper cannot read its content: %v; nothing was changed", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, failure(problem.InternalError, "the helper's content is not a regular file (%v); nothing was "+
			"changed", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, failure(problem.InternalError, "the helper cannot read its content: %v; nothing was changed", err)
	}
	if msg := contentProblem(string(b), max); msg != "" {
		return nil, failure(problem.ValidationFailed, "%s; nothing was changed", msg)
	}
	return b, nil
}

// tempName is the hidden temporary name of a write of site name by
// operation id: .<name>.nexus-<short id>.tmp. The short id is the random
// tail of the operation's UUIDv7.
func tempName(name, id string) string {
	short := id
	if i := strings.LastIndexByte(short, '-'); i >= 0 {
		short = short[i+1:]
	}
	if len(short) > 12 {
		short = short[len(short)-12:]
	}
	return "." + name + ".nexus-" + short + ".tmp"
}

// runWrite is Data Flow 2 step 5. Everything that can refuse without acting
// runs before the backup: the re-check of the site and, for an enabled
// site, the identification of nginx's master. After the write, a failed
// test or reload restores the backup.
func runWrite(ctx context.Context, e env, in helperInput) (any, *helperError) {
	s := in.Settings.settings()
	if err := ValidName(in.Name, s); err != nil {
		return nil, failure(problem.ValidationFailed, "site name %q: %v; nothing was changed", in.Name, err)
	}
	content, herr := readContent(e.dir, s.MaxConfigBytes)
	if herr != nil {
		return nil, herr
	}
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

	c, err := classify(dirfd, in.Name, s)
	if err != nil {
		return nil, failure(problem.DependencyUnavailable, "includeDir %s cannot be read: %v; nothing was changed",
			s.IncludeDir, err)
	}
	req := writeRequest{Name: in.Name, IfMatch: in.IfMatch, Enabled: in.Enabled, WhatIf: in.WhatIf, Create: in.Create}
	tg, perr := checkWrite(dirfd, c, req, s)
	if perr != nil {
		return nil, unchangedFailure(perr)
	}
	sum := sha256.Sum256(content)
	say(e.out, "content: %d bytes, sha256 %s", len(content), hex.EncodeToString(sum[:]))
	w := &write{e: e, in: in, dirfd: dirfd, name: in.Name, target: tg, content: content,
		sha256: hex.EncodeToString(sum[:]), candidate: filepath.Join(e.dir, contentFile),
		presented: s.EnabledFile(in.Name), tmp: tempName(in.Name, in.OperationID)}
	if in.WhatIf {
		return w.whatIf(ctx)
	}
	return w.run(ctx)
}

// unchangedFailure is the helper's failure for p, a refusal before any
// change: its slug, detail and extensions.
func unchangedFailure(p *problem.Error) *helperError {
	h := failure(p.Slug(), "%s; nothing was changed", p.Detail)
	for k, v := range p.Extensions {
		h = h.with(k, v)
	}
	return h
}

// write is one add or update in the helper.
type write struct {
	e         env
	in        helperInput
	dirfd     int
	name      string
	target    writeTarget
	content   []byte
	sha256    string
	candidate string   // The content file, which a shadow test presents
	presented string   // The site's enabled file name
	tmp       string   // WriteAtomic's temporary name
	meta      Meta     // The backup, once taken
	wrote     Identity // The file WriteAtomic made, which a rollback undoes
}

// whatIf is step 5b: a shadow test of the content presented as the site's
// enabled file, whatever its state, and nothing else.
func (w *write) whatIf(ctx context.Context) (any, *helperError) {
	out, herr := shadowTest(ctx, w.e, w.presented, w.candidate)
	if herr != nil {
		herr.Detail += "; nothing was changed"
		return nil, herr
	}
	verdict := "valid"
	if !out.Valid {
		verdict = "invalid"
	}
	say(w.e.out, "nginx-op: whatIf: the configuration with %s would be %s; nothing was changed", w.presented, verdict)
	valid := out.Valid
	return spec.WriteResult{Name: w.name, FileName: w.target.file, SHA256: w.sha256,
		SizeBytes: int64(len(w.content)), Enabled: w.target.enabled, WhatIf: true, Valid: &valid,
		TestOutput: out.Output}, nil
}

// run is steps 5b2 to 5h.
func (w *write) run(ctx context.Context) (any, *helperError) {
	e, file := w.e, w.target.file
	running := false
	var m Master
	var st svcctl.Status
	if w.target.enabled {
		var herr *helperError
		running, m, st, herr = resolveMaster(ctx, e, w.in)
		if herr != nil {
			return nil, herr
		}
	}

	if herr := w.backUp(); herr != nil {
		return nil, herr
	}
	// A file that did not exist is created without replacing: an entry that
	// appeared since the backup is not ours to overwrite, or to roll back.
	id, err := WriteAtomic(w.dirfd, file, w.content, writtenMode, int(managedUID), int(managedGID), w.tmp,
		!w.meta.Exists)
	w.wrote = id
	if err != nil {
		return nil, w.writeFailed(err)
	}
	say(e.out, "nginx-op: wrote %s (%d bytes, sha256 %s)", file, len(w.content), w.sha256)

	out, herr := w.test(ctx)
	if herr != nil {
		return nil, w.rollBack(herr)
	}
	if !out.Valid {
		say(e.out, "nginx-op: the configuration with %s is invalid", file)
		return nil, w.rollBack(failure(problem.NginxConfigInvalid, "nginx -t failed with %s: the configuration is "+
			"invalid", w.presented).with("testOutput", out.Output))
	}
	say(e.out, "nginx-op: the configuration is valid")

	switch {
	case !w.target.enabled:
		say(e.out, "nginx-op: %s is disabled; nginx is not reloaded", w.name)
		return w.result(false, out.Output, nil), nil
	case !running:
		say(e.out, "nginx-op: nginx is %s; nothing was reloaded", st.State)
		return w.result(false, out.Output, nil), nil
	}
	obs, herr := reloadAndVerify(ctx, e, w.in, m)
	if herr != nil {
		return nil, w.reloadFailed(ctx, out.Output, obs, herr)
	}
	return w.result(true, out.Output, &obs), nil
}

// backUp is step 5c, and proves the backup is of the entry the re-check
// saw: a file that appeared, vanished, changed owner or, under ifMatch,
// changed content since is a conflict, with nothing changed.
func (w *write) backUp() *helperError {
	s := w.in.Settings.settings()
	file := w.target.file
	m, err := Backup(w.dirfd, file, w.e.dir, s.MaxConfigBytes)
	if err != nil {
		return failure(problem.DependencyUnavailable, "%s cannot be backed up: %v; nothing was changed", file, err)
	}
	w.meta = m
	switch {
	case m.Exists != w.target.exists, m.Exists && m.UID != managedUID,
		w.target.sha256 != "" && m.SHA256 != w.target.sha256:
		return failure(problem.Conflict, "%s changed while it was being backed up; nothing was changed", file)
	case m.Exists:
		say(w.e.out, "nginx-op: previous content of %s is kept at %s (sha256 %s)", file,
			PreviousContentPath(w.e.dir), m.SHA256)
	default:
		say(w.e.out, "nginx-op: %s does not exist yet; a failure removes it again", file)
	}
	return nil
}

// test is step 5e: a live nginx -t for an enabled site, else a shadow test
// with the site presented as enabled (ASM-10). A test that could not run is
// dependency-unavailable.
func (w *write) test(ctx context.Context) (TestOutput, *helperError) {
	if !w.target.enabled {
		return shadowTest(ctx, w.e, w.presented, w.candidate)
	}
	say(w.e.out, "nginx-op: running %s -t", w.in.Nginx)
	out, err := w.e.nginx.Test(ctx)
	if err != nil {
		return out, failure(problem.DependencyUnavailable, "nginx -t with %s could not run: %v", w.target.file, err)
	}
	return out, nil
}

// result is the write's result.
func (w *write) result(reloaded bool, testOutput string, obs *spec.ReloadObservation) spec.WriteResult {
	r := spec.WriteResult{Name: w.name, FileName: w.target.file, SHA256: w.sha256, SizeBytes: int64(len(w.content)),
		Created: !w.meta.Exists, Enabled: w.target.enabled, Reloaded: reloaded, TestOutput: testOutput,
		PreviousSHA256: w.meta.SHA256, Reload: obs}
	r.Changed = true
	return r
}

// restored says what the restore did.
func (w *write) restored() string {
	if w.meta.Exists {
		return fmt.Sprintf("%s was restored to its previous content, mode and owner", w.target.file)
	}
	return fmt.Sprintf("the created %s was removed", w.target.file)
}

// previous says where an operator finds what to restore by hand.
func (w *write) previous() string {
	if w.meta.Exists {
		return fmt.Sprintf("the previous content of %s (sha256 %s) is kept at %s", w.target.file, w.meta.SHA256,
			PreviousContentPath(w.e.dir))
	}
	return fmt.Sprintf("%s did not exist before: removing it restores the state before the operation", w.target.file)
}

// unknown is outcome-unknown after first, the failure that started the
// rollback, and what went wrong with it; it names the previous content.
func (w *write) unknown(first string, format string, args ...any) *helperError {
	return failure(problem.OutcomeUnknown, "%s; %s; %s", first, fmt.Sprintf(format, args...), w.previous())
}

// notWritten is outcome-unknown after first, the failure that started the
// rollback, when Restore refused because the entry at the name is not the
// file this operation wrote (err). It says what Nexus found and that the
// entry was left untouched, and tells the operator to inspect it: that entry
// may be somebody else's, so the guidance of previous() (remove it) does not
// apply. An overwrite's previous content is still where Nexus kept it.
func (w *write) notWritten(first string, err error, tail string) *helperError {
	detail := fmt.Sprintf("%s; the restore was refused, because Nexus found at %s an entry that is not the file this "+
		"operation wrote, and left it untouched (%v)%s; inspect it before changing it", first, w.target.file, err, tail)
	if w.meta.Exists {
		detail += fmt.Sprintf("; the content before the operation (sha256 %s) is kept at %s", w.meta.SHA256,
			PreviousContentPath(w.e.dir))
	}
	return failure(problem.OutcomeUnknown, "%s", detail)
}

// rollBack restores the backup after herr, a failure before any reload,
// and says so in herr's rolledBack. nginx was not reloaded, so it still
// runs the configuration it had. A restore that fails leaves the file not
// proven to be either, which is outcome-unknown.
func (w *write) rollBack(herr *helperError) *helperError {
	if err := Restore(w.dirfd, w.meta, w.wrote, w.e.dir, w.tmp); err != nil {
		if errors.Is(err, ErrNotWritten) {
			return w.notWritten(herr.Detail, err, " (nginx was not reloaded)")
		}
		return w.unknown(herr.Detail, "restoring %s failed (%v), so it may hold the new content (nginx was not "+
			"reloaded)", w.target.file, err)
	}
	say(w.e.out, "nginx-op: %s", w.restored())
	herr.Detail += "; " + w.restored() + ", so nothing was changed"
	return herr.with("rolledBack", true)
}

// writeFailed is a WriteAtomic that failed. When the target is as the
// backup recorded, nothing was changed; else it is restored.
func (w *write) writeFailed(err error) *helperError {
	if errors.Is(err, ErrAppeared) {
		return failure(problem.Conflict, "%s appeared while it was being created: it is not replaced, and nothing "+
			"was changed", w.target.file)
	}
	herr := failure(problem.DependencyUnavailable, "writing %s failed: %v", w.target.file, err)
	if w.unchanged() {
		herr.Detail += "; nothing was changed"
		return herr
	}
	return w.rollBack(herr)
}

// unchanged reports whether the target is still as the backup recorded it.
func (w *write) unchanged() bool {
	now, err := statAt(w.dirfd, w.target.file)
	switch {
	case err != nil:
		return false
	case !w.meta.Exists:
		return now == nil
	case now == nil:
		return false
	}
	digest, err := Hash(w.dirfd, w.target.file)
	return err == nil && digest == w.meta.SHA256
}

// reloadFailed is step 5g for a write: herr is the failure of the reload
// of the new configuration, obs what it observed. When no reload was sent,
// the restore is all there is to undo. Otherwise it restores, tests,
// identifies the master again and reloads again: a confirmed second reload
// is the definite nginx-reload-failed with rolledBack and rollbackReload, as
// is an nginx no longer running (nothing to reload, rollbackReload absent).
// Anything else leaves which configuration nginx runs unknown, and names
// the previous content.
func (w *write) reloadFailed(ctx context.Context, testOutput string, obs spec.ReloadObservation,
	herr *helperError) *helperError {
	if herr.Slug == problem.DependencyUnavailable {
		return w.rollBack(herr) // reloadAndVerify sent no reload
	}
	e, file := w.e, w.target.file
	first := fmt.Sprintf("the reload with the new %s failed: %s", file, herr.Detail)
	unknown := func(format string, args ...any) *helperError {
		u := w.unknown(first, format, args...)
		if obs.Verdict != "" {
			u = u.with("observed", obs)
		}
		return u
	}
	if err := Restore(w.dirfd, w.meta, w.wrote, e.dir, w.tmp); err != nil {
		if errors.Is(err, ErrNotWritten) {
			u := w.notWritten(first, err, ", and which configuration nginx runs is unknown")
			if obs.Verdict != "" {
				u = u.with("observed", obs)
			}
			return u
		}
		return unknown("restoring %s failed (%v), so it may hold the new content and which configuration nginx "+
			"runs is unknown", file, err)
	}
	say(e.out, "nginx-op: %s", w.restored())
	say(e.out, "nginx-op: running %s -t", w.in.Nginx)
	again, err := e.nginx.Test(ctx)
	switch {
	case err != nil:
		return unknown("%s, but nginx -t could not run (%v), so nginx was not reloaded again and which "+
			"configuration it runs is unknown", w.restored(), err)
	case !again.Valid:
		return unknown("%s, but nginx -t then failed, so nginx was not reloaded again and which configuration it "+
			"runs is unknown: %s", w.restored(), strings.TrimSpace(again.Output))
	}
	running, m, st, herr2 := resolveMaster(ctx, e, w.in)
	if herr2 != nil {
		return unknown("%s, but nginx was not reloaded again: %s; which configuration it runs is unknown",
			w.restored(), herr2.Detail)
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
		return failed(fmt.Sprintf("%s, and nginx is now %s, so there was nothing to reload", w.restored(), st.State))
	}
	back, herr3 := reloadAndVerify(ctx, e, w.in, m)
	if herr3 != nil {
		u := unknown("%s, but the reload that followed failed too: %s; which configuration nginx runs is unknown",
			w.restored(), herr3.Detail)
		if back.Verdict != "" {
			u = u.with("rollbackReload", back)
		}
		return u
	}
	return failed(fmt.Sprintf("%s and nginx reloaded it (verdict %s)", w.restored(), back.Verdict)).
		with("rollbackReload", back)
}
