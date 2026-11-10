package docker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// Lane key prefixes (A-56): every Compose service of one project shares a
// lane, and a standalone container has its own.
const (
	composeLanePrefix   = "compose:"
	containerLanePrefix = "container:"
)

// defaultStopTimeout is Docker's stop timeout for a container created
// without one (Engine API v1.52, ContainerConfig.StopTimeout: "Timeout to
// stop a container in seconds", default 10).
const defaultStopTimeout = 10

// containerLane is the lane of every lifecycle and update operation on
// container name, all LaneReject (A-56): compose:<project> for a container
// with the Compose project label, container:<name> otherwise. It inspects the
// container, so an ID and a name of one container share a lane, and an
// absent container is not-found at admission.
func (p *Provider) containerLane(ctx context.Context, name string) (provider.Lane, error) {
	_, c, err := p.target(ctx, name)
	if err != nil {
		return provider.Lane{}, err
	}
	return laneOf(c), nil
}

func laneOf(c container.InspectResponse) provider.Lane {
	if project := composeProject(c); project != "" {
		return provider.Lane{Key: composeLanePrefix + project, Policy: provider.LaneReject}
	}
	return provider.Lane{Key: containerLanePrefix + strings.TrimPrefix(c.Name, "/"), Policy: provider.LaneReject}
}

func composeProject(c container.InspectResponse) string {
	if c.Config == nil {
		return ""
	}
	return c.Config.Labels[spec.ComposeProjectLabel]
}

// target validates name and inspects the container it names.
func (p *Provider) target(ctx context.Context, name string) (*client.Client, container.InspectResponse, error) {
	if !validName(name) {
		return nil, container.InspectResponse{}, invalid("/name", "must match "+spec.NamePattern)
	}
	cli, err := p.client()
	if err != nil {
		return nil, container.InspectResponse{}, err
	}
	callCtx, cancel := p.callCtx(ctx)
	res, err := cli.ContainerInspect(callCtx, name, client.ContainerInspectOptions{})
	cancel()
	if err != nil {
		return nil, container.InspectResponse{}, p.engineError(ctx, err, "inspect container", notFound())
	}
	return cli, res.Container, nil
}

func running(c container.InspectResponse) bool { return c.State != nil && c.State.Running }

func paused(c container.InspectResponse) bool { return c.State != nil && c.State.Paused }

func displayName(c container.InspectResponse) string { return strings.TrimPrefix(c.Name, "/") }

// actionError is engineError for a state-changing call. The Engine's 409,
// such as removing a running container, is a conflict with detail. Any other
// answer the Engine gave, such as a port already allocated or a missing bind
// source, is a refusal that retrying will not change: a conflict that quotes
// the Engine's text, which is untrusted. Only an Engine that cannot be
// reached, or does not answer in time, is dependency-unavailable (CR-30).
func (p *Provider) actionError(ctx context.Context, err error, what, detail string) error {
	switch {
	case cerrdefs.IsConflict(err):
		return problem.New(problem.Conflict, detail)
	case cerrdefs.IsNotFound(err), ctx.Err() != nil, unreachable(err):
		return p.engineError(ctx, err, what, notFound())
	}
	p.log.Warn("Docker Engine refused the call", "provider", spec.Name, "call", what, "error", err.Error())
	return problem.New(problem.Conflict, fmt.Sprintf("the Docker Engine refused to %s: %s", what, registryText(err.Error())))
}

func say(r *provider.Run, format string, args ...any) {
	// Progress is a convenience; a failed write never fails the operation.
	_, _ = fmt.Fprintf(r.Output(), format+"\n", args...)
}

// start starts a stopped container. A running one is left alone, so its
// StartedAt does not change (TC-E19). Paused is refused as Docker refuses it.
func (p *Provider) start(ctx context.Context, in spec.ContainerInput, r *provider.Run) (spec.ActionResult, error) {
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return spec.ActionResult{}, err
	}
	name := displayName(c)
	switch {
	case paused(c):
		return spec.ActionResult{}, problem.New(problem.Conflict, "the container is paused; Nexus does not unpause containers")
	case running(c):
		say(r, "container %s is already running; nothing to do", name)
		return spec.ActionResult{ContainerID: c.ID}, nil
	}
	say(r, "starting container %s", name)
	callCtx, cancel := p.callCtx(ctx)
	_, err = cli.ContainerStart(callCtx, c.ID, client.ContainerStartOptions{})
	cancel()
	if err != nil {
		return spec.ActionResult{}, p.actionError(ctx, err, "start container", "the Docker Engine refused to start the container in its current state")
	}
	say(r, "started container %s", name)
	return spec.ActionResult{ActionResult: apitypes.ActionResult{Changed: true}, ContainerID: c.ID}, nil
}

// stop stops a running container. A stopped one is left alone, so its
// FinishedAt does not change (TC-E19).
func (p *Provider) stop(ctx context.Context, in spec.StopInput, r *provider.Run) (spec.ActionResult, error) {
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return spec.ActionResult{}, err
	}
	name := displayName(c)
	if !running(c) {
		say(r, "container %s is not running; nothing to do", name)
		return spec.ActionResult{ContainerID: c.ID}, nil
	}
	wait := stopWait(in.TimeoutSeconds, c)
	say(r, "stopping container %s (killed after %s)", name, wait)
	callCtx, cancel := p.stopCtx(ctx, wait)
	_, err = cli.ContainerStop(callCtx, c.ID, client.ContainerStopOptions{Timeout: in.TimeoutSeconds})
	cancel()
	if err != nil {
		return spec.ActionResult{}, p.actionError(ctx, err, "stop container", "the Docker Engine refused to stop the container in its current state")
	}
	say(r, "stopped container %s", name)
	return spec.ActionResult{ActionResult: apitypes.ActionResult{Changed: true}, ContainerID: c.ID}, nil
}

// restart stops and starts a container, or starts a stopped one, as the
// Engine's restart does. It always changes the container.
func (p *Provider) restart(ctx context.Context, in spec.StopInput, r *provider.Run) (spec.ActionResult, error) {
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return spec.ActionResult{}, err
	}
	name := displayName(c)
	wait := stopWait(in.TimeoutSeconds, c)
	say(r, "restarting container %s (killed after %s if it does not stop)", name, wait)
	callCtx, cancel := p.stopCtx(ctx, wait)
	_, err = cli.ContainerRestart(callCtx, c.ID, client.ContainerRestartOptions{Timeout: in.TimeoutSeconds})
	cancel()
	if err != nil {
		return spec.ActionResult{}, p.actionError(ctx, err, "restart container", "the Docker Engine refused to restart the container in its current state")
	}
	say(r, "restarted container %s", name)
	return spec.ActionResult{ActionResult: apitypes.ActionResult{Changed: true}, ContainerID: c.ID}, nil
}

// stopWait is how long the Engine waits before it kills the container: the
// requested timeout, else the container's own, else Docker's default. A
// container whose own timeout is negative (wait forever) is bounded here by
// MaxTimeoutSeconds, for the call deadline only.
func stopWait(requested *int, c container.InspectResponse) time.Duration {
	secs := defaultStopTimeout
	switch {
	case requested != nil:
		secs = *requested
	case c.Config != nil && c.Config.StopTimeout != nil:
		secs = *c.Config.StopTimeout
		if secs < 0 {
			secs = spec.MaxTimeoutSeconds
		}
	}
	return time.Duration(secs) * time.Second
}

// stopCtx bounds a stop or restart call: the Engine may legitimately wait
// the whole stop timeout before it answers, so the call timeout comes on
// top of it.
func (p *Provider) stopCtx(ctx context.Context, wait time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, wait+p.timeout)
}

// remove removes a stopped container, keeping every volume unless
// removeVolumes. The Engine's v=true deletes only anonymous volumes and
// keeps named ones (S-5), so named volumes are then removed one by one; a
// volume another container still uses is kept with a warning. The mounts
// are read before the removal, since they are gone after it.
func (p *Provider) remove(ctx context.Context, in spec.RemoveInput, r *provider.Run) (spec.RemoveResult, error) {
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return spec.RemoveResult{}, err
	}
	name := displayName(c)
	if running(c) {
		return spec.RemoveResult{}, problem.New(problem.Conflict, "the container is running; stop it before removing it")
	}
	volumes := volumeNames(c.Mounts)

	say(r, "removing container %s", name)
	callCtx, cancel := p.callCtx(ctx)
	_, err = cli.ContainerRemove(callCtx, c.ID, client.ContainerRemoveOptions{RemoveVolumes: in.RemoveVolumes})
	cancel()
	if err != nil {
		return spec.RemoveResult{}, p.actionError(ctx, err, "remove container",
			"the Docker Engine refused to remove the container: it is running or already being removed")
	}
	say(r, "removed container %s", name)

	res := spec.RemoveResult{ActionResult: apitypes.ActionResult{Changed: true}, ContainerID: c.ID, RemovedVolumes: []string{}}
	if in.RemoveVolumes {
		// The container is gone: a volume that cannot be removed is a
		// warning, never a failure of an operation that has already acted.
		for _, v := range volumes {
			if gone, warning := p.removeVolume(ctx, cli, v); gone {
				res.RemovedVolumes = append(res.RemovedVolumes, v)
				say(r, "removed volume %s", v)
			} else {
				res.Warnings = append(res.Warnings, warning)
				say(r, "kept volume %s", v)
			}
		}
	}
	if project := composeProject(c); project != "" {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the container belongs to Docker Compose project %q: "+
			"the next Compose deploy of the project recreates it", project))
	}
	return res, nil
}

// removeVolume deletes one volume. Not-found counts as removed: the
// Engine's v=true has already deleted the anonymous ones.
func (p *Provider) removeVolume(ctx context.Context, cli *client.Client, name string) (gone bool, warning string) {
	callCtx, cancel := p.callCtx(ctx)
	_, err := cli.VolumeRemove(callCtx, name, client.VolumeRemoveOptions{})
	cancel()
	switch {
	case err == nil, cerrdefs.IsNotFound(err):
		return true, ""
	case cerrdefs.IsConflict(err):
		return false, fmt.Sprintf("volume %s is still in use by another container and was kept", name)
	}
	p.log.Warn("Docker Engine call failed", "provider", spec.Name, "call", "remove volume", "error", err.Error())
	return false, fmt.Sprintf("volume %s was kept: %s", name, describe(err, p.timeout))
}

// volumeNames are the names of a container's volume mounts, sorted and
// without duplicates; bind and tmpfs mounts are not volumes.
func volumeNames(mounts []container.MountPoint) []string {
	var out []string
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Name != "" {
			out = append(out, m.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// lifecycleBindings binds start, stop, restart and remove, each in-process
// with the container's lane.
func (p *Provider) lifecycleBindings() []provider.Binding {
	return []provider.Binding{
		provider.InProcess(spec.Start, provider.InProcessSpec[spec.ContainerInput, spec.ActionResult]{
			Lane: func(ctx context.Context, in spec.ContainerInput) (provider.Lane, error) {
				return p.containerLane(ctx, in.Name)
			},
			Run: p.start,
		}),
		provider.InProcess(spec.Stop, provider.InProcessSpec[spec.StopInput, spec.ActionResult]{
			Lane: func(ctx context.Context, in spec.StopInput) (provider.Lane, error) {
				return p.containerLane(ctx, in.Name)
			},
			Run: p.stop,
		}),
		provider.InProcess(spec.Restart, provider.InProcessSpec[spec.StopInput, spec.ActionResult]{
			Lane: func(ctx context.Context, in spec.StopInput) (provider.Lane, error) {
				return p.containerLane(ctx, in.Name)
			},
			Run: p.restart,
		}),
		provider.InProcess(spec.Remove, provider.InProcessSpec[spec.RemoveInput, spec.RemoveResult]{
			Lane: func(ctx context.Context, in spec.RemoveInput) (provider.Lane, error) {
				return p.containerLane(ctx, in.Name)
			},
			Run: p.remove,
		}),
	}
}
