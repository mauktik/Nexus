package nexusd

// update-config (F5): config set, a detached operation in the nexusd lane.
// Its Admission hook (D-7), inside nexusd's sandbox and before any
// operation exists, refuses a host without self-management, then reads the
// file, checks every assignment against the deny-list and the schema,
// edits the bytes and validates them with the daemon's own check. A set
// that changes nothing ends there with 200. A change hands Prepare the
// digest of the bytes it validated (A-15); Prepare refuses if the file no
// longer has it and writes input.json, and the service helper, outside
// nexusd.service, backs the file up, replaces it and reloads (WI 4.2).
// Finalize maps result.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// Audit facts of a config set (FR-13, A-23): old.<key> per assignment, the
// backup path of a change, and changed=false for a set that changes
// nothing. Values are cut at audit.MaxFactValueBytes by AddFact.
const (
	factOldPrefix  = "old."
	factBackupPath = "backupPath"
	factChanged    = "changed"
	// absentValue is the old.<key> fact of a key the file does not have.
	absentValue = "<absent>"
)

// configSetConflictRestarted is Prepare's refusal of an input without
// admission's digest: recovery re-ran a Queued operation after a restart
// (A-15).
const configSetConflictRestarted = "nexusd restarted before the change was prepared; nothing was changed; run the " +
	"command again"

// configSetBinding binds nexusd.actions.update-config: detached, in the
// nexusd lane, with an Admission hook that may end a no-op call (D-7).
func (p *Provider) configSetBinding() provider.Binding {
	return provider.Detached(spec.UpdateConfig, provider.DetachedSpec[spec.ConfigUpdateInput, spec.ConfigUpdateResult]{
		Admission:  p.admitConfigSet,
		Lane:       laneOf[spec.ConfigUpdateInput],
		Prepare:    p.prepareConfigSet,
		Finalize:   p.finalizeConfigSet,
		LostDetail: p.lostConfigSetDetail,
	})
}

// configSetReady reports whether config set can run here: exactly when
// self-management can (A-20), since its helper runs in a systemd unit
// outside nexusd.service.
func (p *Provider) configSetReady() spec.Availability { return p.selfManagementReady() }

// admitConfigSet is the Admission hook (F5 step 3). Every refusal happens
// before an operation exists: config-set-unavailable on a host without
// self-management, and otherwise one config-invalid carrying every error
// found, located by key. It adds the facts old.<key>, then either answers
// a set that changes nothing (A-14) or returns the input with admission's
// digest.
func (p *Provider) admitConfigSet(ctx context.Context, in spec.ConfigUpdateInput) (provider.Admitted[spec.ConfigUpdateInput, spec.ConfigUpdateResult], error) {
	var none provider.Admitted[spec.ConfigUpdateInput, spec.ConfigUpdateResult]
	if a := p.configSetReady(); !a.Available {
		return none, problem.New(problem.ConfigSetUnavailable, "nexusd config set is unavailable: "+a.Reason).
			With("provider", spec.Name).With("reason", a.Reason)
	}
	d, err := p.daemonOf()
	if err != nil {
		return none, err
	}
	path := d.ConfigPath()
	data, err := config.ReadFile(path)
	if err != nil {
		return none, configReadFailed(path, err)
	}

	as, errs := parseChanges(in.Changes)
	var out []byte
	var changes []config.Change
	if len(as) > 0 {
		var eerr error
		out, changes, eerr = config.Edit(data, path, as)
		if eerr != nil {
			errs = append(errs, editErrors(path, eerr)...)
		}
	}
	// The validator runs only on an edit with no error so far: a denied
	// key never reaches it, even with an otherwise valid value.
	if len(errs) == 0 {
		errs = configErrors(d.CheckConfigData(out).Errors)
	}
	if len(errs) > 0 {
		return none, problem.New(problem.ConfigInvalid, fmt.Sprintf("the change to %s has %d error(s); nothing "+
			"was changed", path, len(errs))).With("configPath", path).With("errors", errs)
	}

	res := make([]spec.ConfigChange, 0, len(changes))
	same := true
	for _, ch := range changes {
		old := absentValue
		if ch.Present {
			old = factValue(ch.Old)
		}
		audit.AddFact(ctx, factOldPrefix+ch.Key, old)
		res = append(res, spec.ConfigChange{Key: ch.Key, Old: ch.Old, New: ch.New})
		same = same && unchanged(ch)
	}
	if same {
		audit.AddFact(ctx, factChanged, "false")
		return provider.Admitted[spec.ConfigUpdateInput, spec.ConfigUpdateResult]{Done: true,
			Out: spec.ConfigUpdateResult{Changes: res, RestartRequired: []string{}}}, nil
	}
	audit.AddFact(ctx, factBackupPath, backupPathOf(path))
	in.BaseSHA256 = digest(data)
	return provider.Admitted[spec.ConfigUpdateInput, spec.ConfigUpdateResult]{In: in}, nil
}

// parseChanges splits the assignments and refuses, located by key, a
// malformed one, a key assigned twice, a key on the deny-list (before any
// schema rule, D-10) and a value over spec.MaxConfigValueBytes. It returns
// the assignments that passed, for config.Edit.
func parseChanges(changes []string) ([]config.Assignment, []spec.ConfigError) {
	var errs []spec.ConfigError
	if len(changes) > spec.MaxConfigChanges {
		errs = append(errs, spec.ConfigError{Message: fmt.Sprintf("%d changes; at most %d are allowed in one call",
			len(changes), spec.MaxConfigChanges)})
	}
	as := make([]config.Assignment, 0, len(changes))
	seen := map[string]bool{}
	for _, s := range changes {
		a, err := config.ParseAssignment(s)
		if err != nil {
			errs = append(errs, spec.ConfigError{Message: err.Error()})
			continue
		}
		if seen[a.Key] {
			errs = append(errs, spec.ConfigError{Path: a.Key, Message: "assigned more than once"})
			continue
		}
		seen[a.Key] = true
		if deny, denied := deniedChange(a); denied {
			errs = append(errs, spec.ConfigError{Path: a.Key, Message: "cannot be set remotely: " + deny.Reason})
			continue
		}
		if len(a.Value) > spec.MaxConfigValueBytes {
			// Never the value: it may be long, or a secret.
			errs = append(errs, spec.ConfigError{Path: a.Key, Message: fmt.Sprintf("invalid value: %d bytes, "+
				"longer than %d bytes", len(a.Value), spec.MaxConfigValueBytes)})
			continue
		}
		as = append(as, a)
	}
	return as, errs
}

// editErrors are config.Edit's errors as config-invalid's errors[].
func editErrors(path string, err error) []spec.ConfigError {
	var list config.ErrorList
	if errors.As(err, &list) {
		if errs := configErrors(list); len(errs) > 0 {
			return errs
		}
	}
	return []spec.ConfigError{{File: path, Message: err.Error()}}
}

// unchanged reports that an assignment leaves its key as it is: the key is
// in the file with an equal decoded value, durations compared as
// time.Duration (A-14). An absent key is always a change, even to its
// default.
func unchanged(ch config.Change) bool {
	if !ch.Present || ch.Old == nil {
		return false
	}
	if k, err := config.LookupKey(ch.Key); err == nil && k.Kind == config.KindDuration {
		o, ok1 := ch.Old.(string)
		n, ok2 := ch.New.(string)
		if !ok1 || !ok2 {
			return false
		}
		od, err1 := time.ParseDuration(o)
		nd, err2 := time.ParseDuration(n)
		return err1 == nil && err2 == nil && od == nd
	}
	return reflect.DeepEqual(ch.Old, ch.New)
}

// factValue is an old value as an audit fact.
func factValue(v any) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(v)
}

// backupPathOf is where the helper saves the file before changing it.
func backupPathOf(configPath string) string { return configPath + ".bak" }

// prepareConfigSet refuses with config-conflict, nothing changed, an input
// without admission's digest (nexusd restarted in between, A-15) or a file
// that no longer has it, then writes input.json and returns the helper's
// spec (F5 step 5). It never edits or validates: the helper does both.
func (p *Provider) prepareConfigSet(_ context.Context, in spec.ConfigUpdateInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	if in.BaseSHA256 == "" {
		return detach.Spec{}, problem.New(problem.ConfigConflict, configSetConflictRestarted)
	}
	d, err := p.daemonOf()
	if err != nil {
		return detach.Spec{}, err
	}
	path := d.ConfigPath()
	data, err := config.ReadFile(path)
	if err != nil {
		return detach.Spec{}, problem.New(problem.ConfigConflict, fmt.Sprintf("cannot read %s since the change "+
			"was admitted: %v; nothing was changed; run the command again", path, err))
	}
	if digest(data) != in.BaseSHA256 {
		return detach.Spec{}, problem.New(problem.ConfigConflict, fmt.Sprintf("%s changed since the change was "+
			"admitted; nothing was changed; run the command again", path))
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: locate nexusd: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: create %s: %w", dir, err)
	}
	hin := helperInput{
		Mode:          modeConfigSet,
		ConfigPath:    path,
		BackupPath:    backupPathOf(path),
		Changes:       in.Changes,
		BaseSHA256:    in.BaseSHA256,
		StateDir:      d.StateDir(),
		ReloadTimeout: reloadTimeout,
		Paths:         p.paths,
	}
	if err := writeJSONAtomic(dir, inputFileName, hin, nil); err != nil {
		return detach.Spec{}, fmt.Errorf("nexusd: %w", err)
	}
	fmt.Fprintf(r.Output(), "setting %s in %s from unit %s; reload of %s confirmed within %s\n",
		strings.Join(changeKeys(in.Changes), ", "), path, detach.Unit(r.ID()), p.paths.Unit, reloadTimeout)
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Dir:        dir,
		KillMode:   detach.KillProcess,
		MaxRuntime: configSetMaxRuntime,
	}, nil
}

// changeKeys are the keys of the assignments, never their values.
func changeKeys(changes []string) []string {
	keys := make([]string, 0, len(changes))
	for _, c := range changes {
		k, _, _ := strings.Cut(c, "=")
		keys = append(keys, k)
	}
	return keys
}

// finalizeConfigSet states the outcome from result.json. It only reads
// files.
func (p *Provider) finalizeConfigSet(_ context.Context, in spec.ConfigUpdateInput, x detach.Exit, r *provider.Run) (spec.ConfigUpdateResult, error) {
	var none spec.ConfigUpdateResult
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
			"result; %s", how, p.lostConfigSetDetail(r, in)))
	}
	if res.Error != nil {
		return none, res.problem()
	}
	changes := res.ConfigChanges
	if changes == nil {
		changes = []spec.ConfigChange{}
	}
	restart := res.RestartRequired
	if restart == nil {
		restart = []string{}
	}
	return spec.ConfigUpdateResult{
		ActionResult:    apitypes.ActionResult{Changed: res.Changed, Warnings: configSetWarnings(changes, restart)},
		Changes:         changes,
		RestartRequired: restart,
		BackupPath:      res.BackupPath,
	}, nil
}

// configSetWarnings is one lock-out warning per changed key under
// spec.LockoutKeyPrefixes (ASM-3), then the restart note when keys take
// effect only at the next restart (ASM-4: config set never restarts).
func configSetWarnings(changes []spec.ConfigChange, restart []string) []string {
	var out []string
	for _, ch := range changes {
		if reflect.DeepEqual(ch.Old, ch.New) {
			continue
		}
		for _, prefix := range spec.LockoutKeyPrefixes {
			if strings.HasPrefix(ch.Key, prefix) {
				out = append(out, ch.Key+" takes effect when nexusd restarts; if it changes how this client "+
					"connects (certificate, pin or auth limit), you may be locked out until a root login on the "+
					"host fixes it")
				break
			}
		}
	}
	if len(restart) > 0 {
		out = append(out, "run nexus nexusd restart to apply: "+strings.Join(restart, ", "))
	}
	return out
}

// lostConfigSetDetail is what to check by hand after a config set that
// stopped without a result, naming the files of input.json when it can be
// read and the daemon's otherwise. It only reads a file.
func (p *Provider) lostConfigSetDetail(r *provider.Run, _ spec.ConfigUpdateInput) string {
	var path, backup string
	if dir, err := p.opDir(r.ID()); err == nil {
		var in helperInput
		if b, err := os.ReadFile(filepath.Join(dir, inputFileName)); err == nil && json.Unmarshal(b, &in) == nil {
			path, backup = in.ConfigPath, in.BackupPath
		}
	}
	if path == "" {
		if d, err := p.daemonOf(); err == nil {
			path = d.ConfigPath()
		}
	}
	if backup == "" && path != "" {
		backup = backupPathOf(path)
	}
	return fmt.Sprintf("%[1]s may or may not have been changed, and nexusd may or may not have reloaded it. "+
		"Check by hand: `nexusd validate-config` checks %[1]s, `nexus nexusdconfig get` shows its keys, and %[2]s, "+
		"when it exists, holds the file before this change", path, backup)
}
