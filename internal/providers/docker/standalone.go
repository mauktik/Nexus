package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// Standalone container update (FR-12, FR-13): admission in nexusd, and the
// helper's flow in the operation's unit. The helper's Engine calls are
// POST /images/create (pull, X-Registry-Auth from auths only, as in 6.3),
// POST /containers/{id}/stop, /rename?name=, /containers/create?name=,
// /networks/{id}/connect, /containers/{id}/start and DELETE
// /containers/{id} without v, so no volume is removed (Engine API v1.52).

// readySettle is how long a new container without a health check must stay
// running, with the same StartedAt and no restart, to count as up: one
// that exits at once is caught even if it is seen running first.
const readySettle = 2 * time.Second

// readyPoll is the health wait's inspection interval.
const readyPoll = 250 * time.Millisecond

// admitUpdate refuses, before an operation exists, an update the host
// cannot run (no detached operations, CON-4) or a container that is
// notUpdatable.
func (p *Provider) admitUpdate(ctx context.Context, in spec.ContainerInput) error {
	if a := p.systemd(); !a.Available {
		return problem.New(problem.CapabilityUnavailable, "container updates run only as detached operations: "+a.Reason).
			With("provider", spec.Name).With("reason", a.Reason)
	}
	cli, c, err := p.target(ctx, in.Name)
	if err != nil {
		return err
	}
	return p.updatable(ctx, cli, c)
}

// updatable refuses a container the update cannot replace. A
// Compose-managed one needs the Compose plugin (compose-plugin-missing)
// and its project's files (compose-files-missing). Any container is
// not-updatable when created from an image ID or a digest-pinned
// reference, auto-removed (A-36), stored notUpdatable by the last
// check-updates (A-13), or running an image with no registry digest for
// its repository (locally built, ASM-8). It contacts no registry and
// causes no Engine event.
func (p *Provider) updatable(ctx context.Context, cli *client.Client, c container.InspectResponse) error {
	if paused(c) {
		return problem.New(problem.Conflict, "the container is paused; Nexus does not unpause containers")
	}
	if composeProject(c) != "" {
		if _, err := p.composeAdmissible(c); err != nil {
			return err
		}
	}
	ref := ""
	if c.Config != nil {
		ref = c.Config.Image
	}
	if u, done := classifyLocal(c, ref); done {
		return refuseUpdate(u.Reason, u.Detail)
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return refuseUpdate(spec.ReasonNoRegistrySource, fmt.Sprintf("the image reference %q names no registry repository", ref))
	}
	checks, err := p.loadChecks()
	if err != nil {
		return err
	}
	if u := checks.updateOf(c.ID, c.Image); u.Status == spec.UpdateNotUpdatable {
		return refuseUpdate(u.Reason, u.Detail)
	}
	callCtx, cancel := p.callCtx(ctx)
	img, err := cli.ImageInspect(callCtx, c.Image)
	cancel()
	switch {
	case cerrdefs.IsNotFound(err):
		// The helper reports the missing image before changing anything.
		return nil
	case err != nil:
		return p.engineError(ctx, err, "inspect image", nil)
	}
	if len(repoDigestsFor(named, img.RepoDigests)) == 0 {
		return refuseUpdate(spec.ReasonLocallyBuilt, fmt.Sprintf("the image has no registry digest for %s: it was "+
			"built, loaded or retagged on this host, not pulled from a registry. %s", named.Name(), byScript))
	}
	return nil
}

// refuseUpdate is 422 not-updatable with the reason, and for a locally
// built image the hint naming the scripts provider (FR-12, ASM-8).
func refuseUpdate(reason, detail string) error {
	p := problem.New(problem.NotUpdatable, "the container cannot be updated: "+detail).With("reason", reason)
	if reason == spec.ReasonLocallyBuilt {
		p.With("hint", byScript)
	}
	return p
}

// updater is one run of the helper's standalone flow. Its flags record
// what has been changed, so the one rollback undoes exactly that.
type updater struct {
	in  helperInput
	cli *client.Client
	out io.Writer
	res helperResult

	old        container.InspectResponse
	wasRunning bool
	stopped    bool   // The old container's stop was attempted
	renamed    bool   // Its rename was attempted
	newID      string // The new container, once created
}

// runStandalone updates the container in, writing progress to out, and
// returns the result to store.
func runStandalone(ctx context.Context, in helperInput, out io.Writer) helperResult {
	return runUpdater(ctx, in, out, (*updater).run)
}

// runUpdater runs one mode's flow with the Engine client of in.
func runUpdater(ctx context.Context, in helperInput, out io.Writer, flow func(*updater, context.Context) helperResult) helperResult {
	u := &updater{in: in, out: out}
	u.res.PreviousContainerID = in.ContainerID
	if u.in.CallTimeout <= 0 {
		u.in.CallTimeout = DefaultCallTimeout
	}
	if u.in.PullTimeout <= 0 {
		u.in.PullTimeout = 30 * time.Minute
	}
	cli, err := newClient(in.Socket)
	if err != nil {
		return u.fail(problem.DependencyUnavailable, "the Docker client could not be configured: "+err.Error())
	}
	defer func() {
		if err := cli.Close(); err != nil {
			fmt.Fprintf(out, "closing the Docker client: %v\n", err)
		}
	}()
	u.cli = cli
	return flow(u, ctx)
}

func (u *updater) say(format string, args ...any) {
	// Progress is a convenience; a failed write never fails the update.
	_, _ = fmt.Fprintf(u.out, format+"\n", args...)
}

func (u *updater) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, u.in.CallTimeout)
}

func (u *updater) engine(err error) string { return describe(err, u.in.CallTimeout) }

// fail records the failure and returns the result.
func (u *updater) fail(slug problem.Slug, detail string) helperResult {
	u.res.Error = &helperError{Slug: slug, Detail: detail}
	u.say("update failed: %s", detail)
	return u.res
}

func (u *updater) inspect(ctx context.Context, id string) (container.InspectResponse, error) {
	callCtx, cancel := u.call(ctx)
	defer cancel()
	res, err := u.cli.ContainerInspect(callCtx, id, client.ContainerInspectOptions{})
	return res.Container, err
}

func (u *updater) run(ctx context.Context) helperResult {
	name, old := u.in.Name, u.in.OldName
	c, err := u.inspect(ctx, u.in.ContainerID)
	switch {
	case cerrdefs.IsNotFound(err):
		return u.fail(problem.NotFound, fmt.Sprintf("container %s no longer exists; nothing was changed", name))
	case err != nil:
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the container: "+u.engine(err))
	}
	if displayName(c) != name {
		return u.fail(problem.Conflict, fmt.Sprintf("container %s was renamed to %s after the update was requested; "+
			"nothing was changed", name, displayName(c)))
	}
	ref := ""
	if c.Config != nil {
		ref = c.Config.Image
	}
	if project := composeProject(c); project != "" {
		return u.fail(problem.NotUpdatable, fmt.Sprintf("container %s belongs to Docker Compose project %q; nothing was changed", name, project))
	}
	if chk, done := classifyLocal(c, ref); done {
		return u.fail(problem.NotUpdatable, chk.Detail+"; nothing was changed")
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return u.fail(problem.NotUpdatable, fmt.Sprintf("the image reference %q names no registry repository; nothing was changed", ref))
	}
	tagged := reference.TagNameOnly(named)
	u.old, u.wasRunning = c, running(c)

	callCtx, cancel := u.call(ctx)
	oldImg, err := u.cli.ImageInspect(callCtx, c.Image)
	cancel()
	switch {
	case cerrdefs.IsNotFound(err):
		return u.fail(problem.DependencyUnavailable, fmt.Sprintf("the container's image %s is no longer on this "+
			"host, so its digest cannot be compared; nothing was changed", c.Image))
	case err != nil:
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the container's image: "+u.engine(err))
	}
	current := repoDigestsFor(named, oldImg.RepoDigests)
	if len(current) == 0 {
		return u.fail(problem.NotUpdatable, fmt.Sprintf("the image has no registry digest for %s: it was built on "+
			"this host. %s; nothing was changed", named.Name(), byScript))
	}
	u.res.PreviousDigest = current[0]

	// The registry's digest for the reference, with the credential from
	// auths for exactly this registry, or none.
	reg, err := u.registryAccess(named)
	if err != nil {
		return u.fail(problem.DependencyUnavailable, err.Error()+"; nothing was changed")
	}
	u.say("checking %s for a newer image", tagged)
	callCtx, cancel = u.call(ctx)
	dist, err := u.cli.DistributionInspect(callCtx, tagged.String(), client.DistributionInspectOptions{EncodedRegistryAuth: reg.creds.encoded})
	cancel()
	if err == nil && dist.Descriptor.Digest.Validate() != nil {
		err = errors.New("the registry answered without a valid digest")
	}
	if err != nil {
		return u.fail(problem.DependencyUnavailable, registryFailure(err, tagged.String(), reg.domain, reg.creds, reg.path, u.in.CallTimeout)+
			"; nothing was changed")
	}
	latest := dist.Descriptor.Digest.String()
	u.res.NewDigest = latest
	if slices.Contains(current, latest) {
		u.res.PreviousDigest, u.res.ContainerID = latest, c.ID
		u.say("container %s already runs the newest image %s (%s); nothing to do", name, tagged, latest)
		return u.res
	}

	if f := u.unserved(ctx, named, current, reg); f != nil {
		return u.fail(f.Slug, f.Detail+"; nothing was changed")
	}

	u.say("pulling %s (%s)", tagged, latest)
	if err := u.pull(ctx, tagged.String(), reg.creds.encoded); err != nil {
		return u.fail(problem.DependencyUnavailable, fmt.Sprintf("pulling %s failed: %s; nothing was changed",
			tagged, registryText(err.Error())))
	}
	callCtx, cancel = u.call(ctx)
	newImg, err := u.cli.ImageInspect(callCtx, tagged.String())
	cancel()
	if err != nil {
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the pulled image: "+u.engine(err)+
			"; nothing was changed")
	}
	if d := repoDigestsFor(named, newImg.RepoDigests); len(d) > 0 && !slices.Contains(d, latest) {
		u.res.NewDigest = d[0]
	}
	if newImg.ID == c.Image {
		u.res.NewDigest, u.res.ContainerID = u.res.PreviousDigest, c.ID
		u.say("the pulled image is the one container %s runs; nothing to do", name)
		return u.res
	}

	u.res.Warnings = u.dependents(ctx, c)
	cs := recreateSpec(c, oldImg.InspectResponse)

	if u.wasRunning {
		u.say("stopping container %s", name)
		u.stopped = true
		stopCtx, cancel := context.WithTimeout(ctx, stopWait(nil, c)+u.in.CallTimeout)
		_, err := u.cli.ContainerStop(stopCtx, c.ID, client.ContainerStopOptions{})
		cancel()
		if err != nil {
			return u.rollbackAndFail(ctx, problem.DependencyUnavailable, "stopping the container failed: "+u.engine(err))
		}
	}
	u.renamed = true
	callCtx, cancel = u.call(ctx)
	_, err = u.cli.ContainerRename(callCtx, c.ID, client.ContainerRenameOptions{NewName: old})
	cancel()
	if err != nil {
		return u.rollbackAndFail(ctx, problem.DependencyUnavailable, fmt.Sprintf("renaming the container to %s failed: %s",
			old, u.engine(err)))
	}
	u.say("renamed container %s to %s", name, old)

	callCtx, cancel = u.call(ctx)
	created, err := u.cli.ContainerCreate(callCtx, client.ContainerCreateOptions{
		Name: name, Config: cs.Config, HostConfig: cs.HostConfig, NetworkingConfig: cs.Networking,
	})
	cancel()
	if err != nil {
		return u.rollbackAndFail(ctx, problem.DependencyUnavailable, "the Docker Engine refused to create the new "+
			"container: "+registryText(err.Error()))
	}
	u.newID = created.ID
	for _, w := range created.Warnings {
		u.res.Warnings = append(u.res.Warnings, "the Docker Engine warned when creating the new container: "+registryText(w))
	}
	u.say("created container %s (%s)", name, short(created.ID))
	for _, ne := range cs.Connect {
		callCtx, cancel = u.call(ctx)
		_, err := u.cli.NetworkConnect(callCtx, ne.Name, client.NetworkConnectOptions{Container: created.ID, EndpointConfig: ne.Endpoint})
		cancel()
		if err != nil {
			return u.rollbackAndFail(ctx, problem.DependencyUnavailable, fmt.Sprintf("connecting the new container to "+
				"network %s failed: %s", ne.Name, registryText(err.Error())))
		}
		u.say("connected container %s to network %s", name, ne.Name)
	}

	if u.wasRunning {
		callCtx, cancel = u.call(ctx)
		_, err := u.cli.ContainerStart(callCtx, created.ID, client.ContainerStartOptions{})
		cancel()
		if err != nil {
			return u.rollbackAndFail(ctx, problem.UpdateHealthFailed, "the new container failed to start: "+registryText(err.Error()))
		}
		u.say("started container %s; waiting up to %s for it to be running, and healthy if it has a health check",
			name, u.in.HealthTimeout)
		if err := waitReady(ctx, u.inspect, created.ID, u.in.HealthTimeout); err != nil {
			return u.rollbackAndFail(ctx, problem.UpdateHealthFailed, err.Error())
		}
		u.say("the new container %s is healthy", name)
	} else {
		u.say("container %s was not running, so the new container is left stopped", name)
	}

	callCtx, cancel = u.call(ctx)
	_, err = u.cli.ContainerRemove(callCtx, c.ID, client.ContainerRemoveOptions{})
	cancel()
	if err != nil {
		u.res.Warnings = append(u.res.Warnings, fmt.Sprintf("the previous container %s could not be removed (%s): "+
			"remove it by hand; its volumes are in use by the new container", old, registryText(err.Error())))
		u.say("could not remove the previous container %s: %s", old, registryText(err.Error()))
	} else {
		u.say("removed the previous container %s, keeping its volumes", old)
	}
	u.res.Changed, u.res.ContainerID = true, created.ID
	u.say("updated container %s to %s", name, u.res.NewDigest)
	return u.res
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// pull pulls ref, bounded by the pull timeout, writing the stream's
// overall status lines (not per-layer progress) to the output.
func (u *updater) pull(ctx context.Context, ref, auth string) error {
	ctx, cancel := context.WithTimeout(ctx, u.in.PullTimeout)
	defer cancel()
	resp, err := u.cli.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: auth})
	if err != nil {
		return err
	}
	defer resp.Close()
	for m, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if m.Error != nil {
			return errors.New(m.Error.Message)
		}
		if m.ID == "" && m.Status != "" {
			u.say("%s", registryText(m.Status))
		}
	}
	return ctx.Err()
}

// dependents warns about containers that share c's network or PID
// namespace (container:<name> modes): they keep the previous container's
// namespace until they are restarted.
func (u *updater) dependents(ctx context.Context, c container.InspectResponse) []string {
	name := displayName(c)
	callCtx, cancel := u.call(ctx)
	list, err := u.cli.ContainerList(callCtx, client.ContainerListOptions{All: true})
	cancel()
	if err != nil {
		return []string{"other containers could not be checked for container:" + name + " network or PID modes: " + u.engine(err)}
	}
	var out []string
	for _, s := range list.Items {
		if s.ID == c.ID {
			continue
		}
		d, err := u.inspect(ctx, s.ID)
		if err != nil || d.HostConfig == nil {
			continue // Removed since the list, or unreadable: nothing to warn about.
		}
		for _, m := range []struct{ kind, mode string }{
			{"network", string(d.HostConfig.NetworkMode)}, {"PID", string(d.HostConfig.PidMode)},
		} {
			if dependsOn(m.mode, name, c.ID) {
				out = append(out, fmt.Sprintf("container %s uses the %s namespace of %s (%s): it keeps the previous "+
					"container's namespace until it is restarted", displayName(d), m.kind, name, m.mode))
			}
		}
	}
	slices.Sort(out)
	return out
}

// rollbackAndFail runs the one rollback and fails with slug: rolledBack
// true once the previous container is back under its name, running as it
// was.
func (u *updater) rollbackAndFail(ctx context.Context, slug problem.Slug, detail string) helperResult {
	u.say("rolling back: %s", detail)
	if err := u.rollback(ctx); err != nil {
		u.res.RolledBack = false
		return u.fail(slug, fmt.Sprintf("%s; the rollback failed (%v): %s", detail, err, restoreByHand(u.in.Name, u.in.OldName)))
	}
	u.res.RolledBack, u.res.ContainerID = true, u.in.ContainerID
	u.say("rolled back: container %s is the previous container again", u.in.Name)
	return u.fail(slug, detail+"; the previous container was restored")
}

// rollback undoes what the flags record, each step only if still needed,
// so running it again changes nothing: stop and remove the new container,
// rename the old one back, start it if it was running. Volumes are never
// removed: the new container mounts the old one's.
func (u *updater) rollback(ctx context.Context) error {
	var errs []error
	ids := []string{}
	if u.newID != "" {
		ids = append(ids, u.newID)
	}
	// A create that failed may still have made a container under the name.
	if c, err := u.inspect(ctx, u.in.Name); err == nil && c.ID != u.in.ContainerID && !slices.Contains(ids, c.ID) {
		ids = append(ids, c.ID)
	}
	for _, id := range ids {
		if c, err := u.inspect(ctx, id); err == nil && running(c) {
			stopCtx, cancel := context.WithTimeout(ctx, stopWait(nil, c)+u.in.CallTimeout)
			_, err := u.cli.ContainerStop(stopCtx, id, client.ContainerStopOptions{})
			cancel()
			if err != nil {
				u.say("stopping the new container %s failed: %s; removing it by force", short(id), u.engine(err))
			}
		}
		callCtx, cancel := u.call(ctx)
		_, err := u.cli.ContainerRemove(callCtx, id, client.ContainerRemoveOptions{Force: true})
		cancel()
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove the new container %s: %s", short(id), u.engine(err)))
			continue
		}
		u.say("removed the new container %s", short(id))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if !u.renamed && !u.stopped {
		return nil
	}
	old, err := u.inspect(ctx, u.in.ContainerID)
	if err != nil {
		return fmt.Errorf("inspect the previous container: %s", u.engine(err))
	}
	if u.renamed && displayName(old) != u.in.Name {
		callCtx, cancel := u.call(ctx)
		_, err := u.cli.ContainerRename(callCtx, old.ID, client.ContainerRenameOptions{NewName: u.in.Name})
		cancel()
		if err != nil {
			return fmt.Errorf("rename %s back to %s: %s", u.in.OldName, u.in.Name, registryText(err.Error()))
		}
		u.say("renamed %s back to %s", u.in.OldName, u.in.Name)
	}
	if u.stopped && u.wasRunning && !running(old) {
		callCtx, cancel := u.call(ctx)
		_, err := u.cli.ContainerStart(callCtx, old.ID, client.ContainerStartOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("start the previous container: %s", registryText(err.Error()))
		}
		u.say("started the previous container %s", u.in.Name)
	}
	return nil
}

// waitReady waits up to timeout for container id to be running and, if it
// has a health check, healthy (FR-13). Without a health check it must stay
// running for readySettle. It fails at once when the container exits or
// restarts. Compose updates (6.5) share it.
func waitReady(ctx context.Context, inspect func(context.Context, string) (container.InspectResponse, error),
	id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	settle := min(readySettle, timeout/2)
	restarts, startedAt, since, last := -1, "", time.Time{}, "not inspected"
	for {
		c, err := inspect(ctx, id)
		now := time.Now()
		switch {
		case err != nil:
			last = "the Docker Engine could not describe it: " + err.Error()
		case c.State == nil:
			last = "no state reported"
		default:
			st := c.State
			if restarts < 0 {
				restarts = c.RestartCount
			}
			switch {
			case c.RestartCount > restarts:
				return fmt.Errorf("the new container exited with code %d and was restarted", st.ExitCode)
			case !st.Running && !st.Restarting:
				return fmt.Errorf("the new container exited with code %d", st.ExitCode)
			case st.Restarting:
				return errors.New("the new container exited and is restarting")
			case st.Health != nil:
				if st.Health.Status == container.Healthy {
					return nil
				}
				last = "health " + string(st.Health.Status)
			default:
				if st.StartedAt != startedAt {
					startedAt, since = st.StartedAt, now
				} else if now.Sub(since) >= settle {
					return nil
				}
				last = "running"
			}
		}
		if now.After(deadline) {
			return fmt.Errorf("the new container was not running and healthy within %s (last seen: %s)", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// registryFailure describes a failed registry digest check for the update.
// registryAccessInfo is a registry and the credential auths holds for it.
type registryAccessInfo struct {
	domain string // reference.Domain, lowercased
	path   string // The config.json read
	creds  registryCreds
}

// registryAccess reads the credential for named's registry from auths of
// docker.configDir: exactly this registry's, or none.
func (u *updater) registryAccess(named reference.Named) (registryAccessInfo, error) {
	reg := registryAccessInfo{domain: strings.ToLower(reference.Domain(named))}
	rc, err := loadRegistryConfig(u.in.ConfigDir)
	if err != nil {
		return reg, fmt.Errorf("the registry credentials could not be read: %w", err)
	}
	reg.path = rc.path
	if reg.creds, err = rc.forRegistry(reg.domain); err != nil {
		return reg, fmt.Errorf("the registry credential could not be used: %w", err)
	}
	return reg, nil
}

// unserved asks the registry whether it serves the running image: any of
// current, the image's digests for named's repository. On the containerd
// image store a local build has a RepoDigest too, and the tag may name
// another image in the registry: replacing it would discard the build
// (CR-2). It returns the update's failure, or nil when the registry serves
// the image. Both the standalone and the Compose helper ask before they
// pull.
func (u *updater) unserved(ctx context.Context, named reference.Named, current []string, reg registryAccessInfo) *helperError {
	known, err := registryKnows(ctx, u.cli, u.call, named, current, reg.creds.encoded)
	switch {
	case err != nil:
		return &helperError{Slug: problem.DependencyUnavailable, Detail: registryFailure(err,
			reference.TagNameOnly(named).String(), reg.domain, reg.creds, reg.path, u.in.CallTimeout)}
	case !known:
		return &helperError{Slug: problem.NotUpdatable, Detail: fmt.Sprintf("the registry %s has no image with the "+
			"running digest %s for %s: it was built or retagged on this host. %s", reg.domain, current[0], named.Name(), byScript)}
	}
	return nil
}

func registryFailure(err error, ref, domain string, creds registryCreds, path string, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("the registry %s did not answer within %s", domain, timeout)
	case client.IsErrConnectionFailed(err):
		return "the Docker Engine is not reachable"
	case absent(err):
		return fmt.Sprintf("the registry %s has no image for %s (%s)", domain, ref, registryText(err.Error()))
	case refused(err) && creds.encoded != "":
		return fmt.Sprintf("the registry %s refused the credential in auths[%q] of %s: %s", domain, creds.key, path,
			registryText(err.Error()))
	case refused(err) && creds.helper != "":
		return fmt.Sprintf("the registry %s refused anonymous access, and its credential is held by the credential "+
			"helper %s, which Nexus never runs: store it in auths of %s", domain, creds.helper, path)
	case refused(err):
		return fmt.Sprintf("the registry %s refused anonymous access, and %s has no credential for it in auths", domain, path)
	}
	return fmt.Sprintf("the registry check for %s failed: %s", ref, registryText(err.Error()))
}
