package nexusd

// update (F5): a detached operation. Admit, inside nexusd's sandbox, checks
// the staged upload in FR-7 order and refuses with no operation created:
// update-not-configured, upload-not-found, update-signature-invalid,
// update-package-invalid, then version-downgrade. Prepare verifies the
// upload again, extracts the whole package into ops/<id>/pkg/ and writes
// input.json; the service helper, outside nexusd.service, installs it
// (WI 5.4). Finalize, in the nexusd running afterwards, reads result.json
// and deletes the upload and the extracted package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/release"
	"github.com/mauktik/nexus/internal/version"
)

// modeUpdate is the helper's mode for update.
const modeUpdate = "update"

// installTimeout bounds the package's install.sh (A-7).
const installTimeout = 120 * time.Second

// pkgDirName is the directory of the extracted package in the operation's
// directory: ops/<id>/pkg/.
const pkgDirName = "pkg"

// updateEnv is the helper's environment: a fixed PATH and the C locale.
var updateEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}

// updateMaxRuntime is installTimeout + 2×(healthTimeout+20s) + 60s (A-8):
// install.sh, then the health checks of the update and of its rollback,
// each preceded by systemctl calls, and the slack of rollback's.
func updateMaxRuntime(t healthTarget) time.Duration {
	return installTimeout + 2*(t.Timeout+rollbackPhaseSlack) + rollbackRuntimeSlack
}

// Audit facts of an update (FR-16). Never key material.
const (
	factUploadID       = "uploadId"
	factSHA256         = "sha256"
	factPackageVersion = "packageVersion"
	factSignature      = "signature"
	factAllowDowngrade = "allowDowngrade"
)

// updateHooks are test-only fault and observation points; production
// leaves them nil.
type updateHooks struct {
	// beforePrepare, when set, runs at Prepare's entry.
	beforePrepare func(in spec.UpdateInput)
	// beforeInput, when set, runs between the temporary write of
	// input.json and its rename; an error fails the write there.
	beforeInput func(tmp string) error
	// onFinalize, when set, runs at Finalize's entry.
	onFinalize func()
}

// setUpdateHooks replaces the test hooks.
func (p *Provider) setUpdateHooks(h updateHooks) { p.updateHooks.Store(&h) }

// hooksOfUpdate returns the test hooks, none in production.
func (p *Provider) hooksOfUpdate() updateHooks {
	if h := p.updateHooks.Load(); h != nil {
		return *h
	}
	return updateHooks{}
}

// updateBinding binds nexusd.actions.update: detached, in the nexusd lane.
func (p *Provider) updateBinding() provider.Binding {
	return provider.Detached(spec.Update, provider.DetachedSpec[spec.UpdateInput, spec.UpdateResult]{
		Admit:      p.admitUpdate,
		Lane:       laneOf[spec.UpdateInput],
		Prepare:    p.prepareUpdate,
		Finalize:   p.finalizeUpdate,
		LostDetail: p.lostUpdateDetail,
	})
}

// uploadNotFound is the refusal of an upload ID that names no staged
// upload: unknown, expired, or already used.
func uploadNotFound(id string) error {
	return problem.New(problem.UploadNotFound, fmt.Sprintf("no staged upload %s: it is unknown, expired an hour "+
		"after it was uploaded, or was already used; upload the release again", id)).With("uploadId", id)
}

// updateNotConfigured is the refusal of an update without a trusted key.
func updateNotConfigured() error {
	return problem.New(problem.UpdateNotConfigured, "no trusted release key is configured: add one to "+
		"providers.nexusd.update.trustedKeys in the configuration file, then reload")
}

// pkgRefusal is a package pipeline error as the operation reports it: a
// package refusal as its problem, any other error as it is.
func pkgRefusal(err error) error {
	if p, ok := pkgProblem(err); ok {
		return p
	}
	return fmt.Errorf("nexusd: %w", err)
}

// admitUpdate refuses, before an operation exists and in FR-7 order, a host
// without self-management, then an update without a trusted key, an
// unknown or expired upload, a signature that does not verify, a package
// that breaks a layout rule or whose nexusd does not report its version,
// and a downgrade without allowDowngrade, counting a running version that
// is not comparable as a downgrade (A-25). A signature or package refusal
// deletes the upload; nothing else does. It adds the audit facts of each
// step it passes.
func (p *Provider) admitUpdate(ctx context.Context, in spec.UpdateInput) error {
	if err := p.requireSelfManagement(); err != nil {
		return err
	}
	audit.AddFact(ctx, factUploadID, in.UploadID)
	audit.AddFact(ctx, factAllowDowngrade, strconv.FormatBool(in.AllowDowngrade))
	keys := p.current().keys
	if len(keys) == 0 {
		return updateNotConfigured()
	}
	s, err := p.stagingOf()
	if err != nil {
		return err
	}
	meta, dir, ok := s.get(in.UploadID)
	if !ok {
		return uploadNotFound(in.UploadID)
	}
	audit.AddFact(ctx, factSHA256, meta.SHA256)

	refuse := func(err error) error {
		perr, ok := pkgProblem(err)
		if !ok {
			if errors.Is(err, fs.ErrNotExist) {
				// The upload expired or was removed while admission read it.
				return uploadNotFound(in.UploadID)
			}
			return fmt.Errorf("nexusd: %w", err)
		}
		if perr.Slug() == problem.UpdateSignatureInvalid {
			audit.AddFact(ctx, factSignature, "invalid")
		}
		if rerr := s.remove(in.UploadID); rerr != nil {
			// The refusal stands; the upload expires within the hour.
			slog.Warn("nexusd: cannot delete a refused upload", "uploadId", in.UploadID, "error", rerr.Error())
			return perr
		}
		perr.Detail += "; the upload was deleted"
		return perr
	}
	va, err := verifySignature(filepath.Join(dir, stagedFileName), keys, meta.Signature)
	if err != nil {
		return refuse(err)
	}
	audit.AddFact(ctx, factSignature, "valid")
	m, err := va.scan(runtime.GOARCH)
	if err != nil {
		return refuse(err)
	}
	audit.AddFact(ctx, factPackageVersion, m.version)
	if err := va.probe(ctx, p.run, dir, m.version); err != nil {
		return refuse(err)
	}
	running := version.Version
	if c, ok := release.CompareVersions(m.version, running); (!ok || c < 0) && !in.AllowDowngrade {
		detail := fmt.Sprintf("the package is nexusd %s, older than the running nexusd %s; set allowDowngrade "+
			"to install it anyway", m.version, running)
		if !ok {
			detail = fmt.Sprintf("the running nexusd %s cannot be compared with the package's %s, so the update "+
				"counts as a downgrade; set allowDowngrade to install it anyway", running, m.version)
		}
		return problem.New(problem.VersionDowngrade, detail).With("runningVersion", running).
			With("packageVersion", m.version)
	}
	return nil
}

// prepareUpdate verifies and scans the upload again, since it may have
// changed since Admit, extracts the whole package into ops/<id>/pkg/,
// writes input.json and returns the helper's spec. The ops manager never
// calls Finalize after a Prepare failure, so any error once the extraction
// has started removes ops/<id>/pkg/ before returning (A-50).
func (p *Provider) prepareUpdate(_ context.Context, in spec.UpdateInput, r *provider.Run) (_ detach.Spec, err error) {
	hooks := p.hooksOfUpdate()
	if hooks.beforePrepare != nil {
		hooks.beforePrepare(in)
	}
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	d, err := p.daemonOf()
	if err != nil {
		return detach.Spec{}, err
	}
	keys := p.current().keys
	if len(keys) == 0 {
		return detach.Spec{}, updateNotConfigured()
	}
	s, err := p.stagingOf()
	if err != nil {
		return detach.Spec{}, err
	}
	meta, sdir, ok := s.get(in.UploadID)
	if !ok {
		return detach.Spec{}, uploadNotFound(in.UploadID)
	}
	va, err := verifySignature(filepath.Join(sdir, stagedFileName), keys, meta.Signature)
	if err != nil {
		return detach.Spec{}, pkgRefusal(err)
	}
	m, err := va.scan(runtime.GOARCH)
	if err != nil {
		return detach.Spec{}, pkgRefusal(err)
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

	pkg := filepath.Join(dir, pkgDirName)
	defer func() {
		if err != nil {
			if rerr := os.RemoveAll(pkg); rerr != nil {
				err = errors.Join(err, fmt.Errorf("nexusd: remove %s: %w", pkg, rerr))
			}
		}
	}()
	// Crash recovery re-runs Prepare for a Queued operation, and a previous
	// attempt may have died mid-extraction. Start from an empty pkg, since
	// extraction creates every file with O_EXCL.
	if err := os.RemoveAll(pkg); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: clear %s: %w", pkg, err)
	}
	if err := va.extractAll(pkg); err != nil {
		return detach.Spec{}, pkgRefusal(err)
	}
	running := version.Version
	hin := helperInput{
		Mode:            modeUpdate,
		RunningVersion:  running,
		ExpectedVersion: m.version,
		PackageDir:      filepath.Join(pkg, m.top),
		NoChange:        m.version == running,
		Health:          target,
		InstallTimeout:  installTimeout,
		Paths:           p.paths,
		SavedUnit:       savedUnitPath(d.StateDir()),
	}
	if err := writeJSONAtomic(dir, inputFileName, hin, hooks.beforeInput); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: %w", err)
	}
	if hin.NoChange {
		fmt.Fprintf(r.Output(), "nexusd %s is already running; nothing to install (from unit %s)\n", running,
			detach.Unit(r.ID()))
	} else {
		fmt.Fprintf(r.Output(), "updating %s from nexusd %s to nexusd %s (upload %s) from unit %s; health check "+
			"against %s within %s\n", p.paths.Unit, running, m.version, in.UploadID, detach.Unit(r.ID()), target.URL,
			target.Timeout)
	}
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Env:        updateEnv,
		Dir:        dir,
		KillMode:   detach.KillProcess,
		MaxRuntime: updateMaxRuntime(target),
	}, nil
}

// finalizeUpdate states the outcome from result.json. Whatever it is, it
// deletes the staged upload and ops/<id>/pkg/: the helper normally removed
// the package already, but may have stopped between result.json and its
// removal. Both are idempotent, so Finalize can run again.
func (p *Provider) finalizeUpdate(_ context.Context, in spec.UpdateInput, x detach.Exit, r *provider.Run) (spec.UpdateResult, error) {
	var none spec.UpdateResult
	if h := p.hooksOfUpdate().onFinalize; h != nil {
		h()
	}
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	p.cleanUpdate(in.UploadID, filepath.Join(dir, pkgDirName))
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
			"result; %s", how, p.lostUpdateDetail(r, in)))
	}
	if res.Error != nil {
		return none, res.problem()
	}
	return spec.UpdateResult{ActionResult: apitypes.ActionResult{Changed: res.Changed},
		PreviousVersion: res.PreviousVersion, NewVersion: res.NewVersion, RolledBack: false}, nil
}

// cleanUpdate deletes the staged upload and the extracted package. A
// failure is logged: it never changes the operation's outcome, the upload
// expires within the hour, and the package goes with the operation's
// directory.
func (p *Provider) cleanUpdate(uploadID, pkg string) {
	if s, err := p.stagingOf(); err != nil {
		slog.Warn("nexusd: cannot delete the update's upload", "uploadId", uploadID, "error", err.Error())
	} else if err := s.remove(uploadID); err != nil {
		slog.Warn("nexusd: cannot delete the update's upload", "uploadId", uploadID, "error", err.Error())
	}
	if err := os.RemoveAll(pkg); err != nil {
		slog.Warn("nexusd: cannot remove the extracted package", "dir", pkg, "error", err.Error())
	}
}

// lostUpdateDetail is what to check by hand after an update that stopped
// without a result, naming the paths of input.json when it can be read,
// and the provider's own otherwise. It only reads a file.
func (p *Provider) lostUpdateDetail(r *provider.Run, _ spec.UpdateInput) string {
	paths := p.paths
	if dir, err := p.opDir(r.ID()); err == nil {
		var in helperInput
		if b, err := os.ReadFile(filepath.Join(dir, inputFileName)); err == nil && json.Unmarshal(b, &in) == nil {
			paths = in.Paths.withDefaults()
		}
	}
	return fmt.Sprintf("%[1]s may or may not have been updated, or rolled back. Check by hand: `systemctl status "+
		"%[1]s` shows whether it is active and since when, `%[2]s -version` the installed version, and `%[3]s "+
		"-version` the backup an update saves before installing. If nexusd is not healthy, follow "+
		"docs/install.md#recovering-from-a-failed-update", paths.Unit, paths.Binary, paths.Prev)
}
