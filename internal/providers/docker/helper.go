package docker

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
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// Container update always runs detached (D-8, A-62). Prepare writes
// ops/<id>/input.json and returns a spec whose argv is
// [nexusd, "docker-update", "--dir", ops/<id>]: the supervisor in the
// nexus-op-<id> unit runs that hidden command, which performs the whole
// update and writes ops/<id>/result.json. Finalize only reads result.json
// and exit.json. A restart of nexusd does not interrupt the helper, so
// nothing rolls back at restart; a helper that dies without a result ends
// outcome-unknown naming the renamed previous container.

// HelperCommand is the hidden nexusd subcommand of the update helper.
const HelperCommand = "docker-update"

// Files of the helper in the operation's directory, beside spec.json,
// exit.json and output/.
const (
	inputFile  = "input.json"
	resultFile = "result.json"
)

// ModeStandalone is the helper's mode for a container Compose does not
// manage (WI 6.5 adds compose).
const ModeStandalone = "standalone"

// oldNameInfix is between a container's name and the operation's short ID
// in the name of the previous container while an update runs.
const oldNameInfix = "-nexus-old-"

// helperInput is input.json. It holds no secret: the registry credential is
// read by the helper from configDir.
type helperInput struct {
	OperationID   string        `json:"operationId"`
	Mode          string        `json:"mode"`
	Name          string        `json:"name"`        // The container's name when admitted
	ContainerID   string        `json:"containerId"` // Its full ID: the helper updates this container only
	OldName       string        `json:"oldName"`     // <name>-nexus-old-<short op id>
	HealthTimeout time.Duration `json:"healthTimeout"`
	CallTimeout   time.Duration `json:"callTimeout"`
	PullTimeout   time.Duration `json:"pullTimeout"`
	Socket        string        `json:"socket"`
	ConfigDir     string        `json:"configDir"`
}

// helperResult is result.json.
type helperResult struct {
	Changed             bool         `json:"changed"`
	RolledBack          bool         `json:"rolledBack"`
	ContainerID         string       `json:"containerId"`
	PreviousContainerID string       `json:"previousContainerId"`
	PreviousDigest      string       `json:"previousDigest"`
	NewDigest           string       `json:"newDigest"`
	Warnings            []string     `json:"warnings,omitempty"`
	Error               *helperError `json:"error,omitempty"`
}

// helperError is a failed update's problem: its slug and detail. Finalize
// adds rolledBack and the digests.
type helperError struct {
	Slug   problem.Slug `json:"slug"`
	Detail string       `json:"detail"`
}

// shortOpID is the random tail of an operation ID (a UUIDv7, whose leading
// characters are a timestamp shared by operations started together).
func shortOpID(id string) string {
	hex := strings.ReplaceAll(id, "-", "")
	if len(hex) > 12 {
		hex = hex[len(hex)-12:]
	}
	return hex
}

// oldName is the previous container's name during operation id.
func oldName(name, id string) string { return name + oldNameInfix + shortOpID(id) }

// UseOpsDir sets the directory of the operations' directories. It
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("docker: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// updateBinding binds docker.containers.update: detached, always (D-8).
func (p *Provider) updateBinding() provider.Binding {
	return provider.Detached(spec.Update, provider.DetachedSpec[spec.ContainerInput, spec.UpdateResult]{
		Admit: p.admitUpdate,
		Lane: func(ctx context.Context, in spec.ContainerInput) (provider.Lane, error) {
			return p.containerLane(ctx, in.Name)
		},
		Prepare:    p.prepareUpdate,
		Finalize:   p.finalizeUpdate,
		LostDetail: p.lostUpdateDetail,
	})
}

// pullTimeout bounds the helper's pull to half the maximum runtime, so a
// pull that hangs fails, having changed nothing, while the rest of the
// update still has time before MaxRuntime.
func pullTimeout(maxRuntime time.Duration) time.Duration { return maxRuntime / 2 }

// prepareUpdate checks the container again, writes input.json and returns
// the helper's spec. The argv is the helper's absolute path and fixed
// words: no shell string at any point (FR-25).
func (p *Provider) prepareUpdate(ctx context.Context, in spec.ContainerInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return detach.Spec{}, err
	}
	if err := p.updatable(ctx, cli, c); err != nil {
		return detach.Spec{}, err
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("docker: locate nexusd: %w", err)
	}
	s := p.Settings()
	name := displayName(c)
	hin := helperInput{
		OperationID: r.ID(), Mode: ModeStandalone, Name: name, ContainerID: c.ID, OldName: oldName(name, r.ID()),
		HealthTimeout: s.UpdateHealthTimeout, CallTimeout: p.timeout, PullTimeout: pullTimeout(s.UpdateMaxRuntime),
		Socket: s.Socket, ConfigDir: s.ConfigDir,
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("docker: create %s: %w", dir, err)
	}
	if err := writeJSONAtomic(dir, inputFile, hin); err != nil {
		return detach.Spec{}, err
	}
	say(r, "updating container %s in unit %s", name, detach.Unit(r.ID()))
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Dir:        dir,
		KillMode:   detach.KillControlGroup,
		MaxRuntime: s.UpdateMaxRuntime,
	}, nil
}

// finalizeUpdate states the outcome from result.json, or, without one,
// outcome-unknown naming the previous container. It reads files only.
func (p *Provider) finalizeUpdate(_ context.Context, in spec.ContainerInput, x detach.Exit, r *provider.Run) (spec.UpdateResult, error) {
	var none spec.UpdateResult
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	res, ok, err := readResult(dir)
	if err != nil {
		return none, err
	}
	if !ok {
		name, old := p.updateNames(r, in)
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("the update helper for container %s could "+
				"not be started (%s); nothing was changed", name, x.Error))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		return none, problem.New(problem.OutcomeUnknown, fmt.Sprintf("the update helper for container %s %s "+
			"without reporting a result; %s", name, how, restoreByHand(name, old)))
	}
	if res.Error != nil {
		return none, res.problem()
	}
	return spec.UpdateResult{
		ActionResult:        apitypes.ActionResult{Changed: res.Changed, Warnings: res.Warnings},
		ContainerID:         res.ContainerID,
		PreviousContainerID: res.PreviousContainerID,
		PreviousDigest:      res.PreviousDigest,
		NewDigest:           res.NewDigest,
	}, nil
}

// lostUpdateDetail is added to outcome-unknown when the unit died without
// exit.json.
func (p *Provider) lostUpdateDetail(r *provider.Run, in spec.ContainerInput) string {
	name, old := p.updateNames(r, in)
	return restoreByHand(name, old)
}

// updateNames are the container's name and the previous container's name,
// from input.json, else from the input.
func (p *Provider) updateNames(r *provider.Run, in spec.ContainerInput) (name, old string) {
	if dir, err := p.opDir(r.ID()); err == nil {
		var hin helperInput
		if b, err := os.ReadFile(filepath.Join(dir, inputFile)); err == nil && json.Unmarshal(b, &hin) == nil &&
			hin.Name != "" && hin.OldName != "" {
			return hin.Name, hin.OldName
		}
	}
	return in.Name, oldName(in.Name, r.ID())
}

func restoreByHand(name, old string) string {
	return fmt.Sprintf("container %s may be partly replaced. If container %s exists, it is the previous container, "+
		"stopped and renamed: inspect it and restore it by hand (remove any new %s, rename %s back to %s and start "+
		"it). Nothing was rolled back automatically", name, old, name, old, name)
}

// problem is the failed update's problem with rolledBack and the digests
// where its type declares them.
func (res helperResult) problem() *problem.Error {
	t, ok := problem.Lookup(res.Error.Slug)
	if !ok {
		return problem.New(problem.InternalError, res.Error.Detail)
	}
	p := problem.New(t.Slug, res.Error.Detail)
	has := func(ext string) bool { return slices.Contains(t.Extensions, ext) }
	if has("provider") {
		p.With("provider", spec.Name)
	}
	if has(spec.ExtRolledBack) {
		p.With(spec.ExtRolledBack, res.RolledBack)
	}
	if has(spec.ExtPreviousDigest) && res.PreviousDigest != "" {
		p.With(spec.ExtPreviousDigest, res.PreviousDigest)
	}
	if has(spec.ExtNewDigest) && res.NewDigest != "" {
		p.With(spec.ExtNewDigest, res.NewDigest)
	}
	return p
}

func readResult(dir string) (helperResult, bool, error) {
	var res helperResult
	b, err := os.ReadFile(filepath.Join(dir, resultFile))
	if errors.Is(err, fs.ErrNotExist) {
		return res, false, nil
	}
	if err != nil {
		return res, false, fmt.Errorf("docker: read %s: %w", resultFile, err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return res, false, fmt.Errorf("docker: decode %s: %w", resultFile, err)
	}
	return res, true, nil
}

// writeJSONAtomic replaces dir/name with v as JSON, mode 0600, through a
// temporary file, fsync, rename and a directory fsync: a reader sees no
// file or a complete one.
func writeJSONAtomic(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("docker: encode %s: %w", name, err)
	}
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("docker: write %s: %w", name, err)
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
		return errors.Join(fmt.Errorf("docker: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("docker: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("docker: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("docker: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("docker: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("docker: sync %s: %w", dir, serr)
	}
	return nil
}

// UpdateHelper is `nexusd docker-update --dir D`, run by op-supervise in the
// operation's unit. It reads D/input.json, updates the container, and
// writes D/result.json. Progress goes to stdout and diagnostics to stderr,
// both into the operation's output. It returns 0 once result.json is
// written, whatever the update's outcome; without a result.json the
// operation ends outcome-unknown.
func UpdateHelper(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nexusd "+HelperCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the operation's directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: nexusd %s --dir dir\n", HelperCommand)
		return 2
	}
	var hin helperInput
	b, err := os.ReadFile(filepath.Join(*dir, inputFile))
	if err == nil {
		err = json.Unmarshal(b, &hin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: read %s: %v\n", HelperCommand, inputFile, err)
		return 1
	}
	if hin.Mode != ModeStandalone {
		fmt.Fprintf(stderr, "%s: unknown mode %q\n", HelperCommand, hin.Mode)
		return 1
	}
	if _, ok, _ := readResult(*dir); ok {
		// Never update twice: the supervisor runs the helper once, and a
		// second start must not repeat a finished update.
		fmt.Fprintf(stderr, "%s: %s already exists; nothing to do\n", HelperCommand, resultFile)
		return 1
	}
	res := runStandalone(context.Background(), hin, stdout)
	if err := writeJSONAtomic(*dir, resultFile, res); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", HelperCommand, err)
		return 1
	}
	return 0
}
