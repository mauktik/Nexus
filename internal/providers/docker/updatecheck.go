package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// Update detection (FR-11). The running image's digest is the RepoDigests
// entry for the reference's repository; the latest is the descriptor digest
// the Engine's GET /distribution/{name}/json reads from the registry (Engine
// API v1.52, DistributionInspect: "Return image digest and platform
// information by contacting the registry"). The registry answers it from a
// manifest request, so no layer is pulled. .Image is never compared: it is
// the manifest digest only on the containerd image store (A-32).
//
// No path reports upToDate after an error: the only assignment of
// UpdateUpToDate is in compare, reached only with both digests valid.

const (
	// checkLaneKey is the lane of check-updates: one check at a time, a
	// second answers 409 naming the first.
	checkLaneKey = "docker-check"
	// checkConcurrency bounds the concurrent registry calls of one check.
	checkConcurrency = 8
	// checkKVKey holds the last check's results, by container ID.
	checkKVKey = "docker-check/last"
	// maxRegistryText bounds the registry error text quoted in a reason.
	maxRegistryText = 512
)

// storedCheck is one container's stored result. ImageID ties it to the
// image the container ran when checked: a recreated or updated container
// is notChecked until the next check.
type storedCheck struct {
	ImageID string           `json:"imageId"`
	Update  spec.UpdateCheck `json:"update"`
}

// storedChecks is the value under checkKVKey.
type storedChecks struct {
	CheckedAt  time.Time              `json:"checkedAt"`
	Containers map[string]storedCheck `json:"containers"` // By container ID
}

// memKV is the store until the daemon hands one over (UseKV): T0 tests and
// tools that build the provider alone.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (k *memKV) KVGet(key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return slices.Clone(v), ok, nil
}

func (k *memKV) KVPut(key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string][]byte{}
	}
	k.m[key] = slices.Clone(value)
	return nil
}

// UseKV keeps check results in kv, the daemon's state.db, so they survive a
// restart. It implements provider.KVUser.
func (p *Provider) UseKV(kv provider.KV) {
	if kv != nil {
		p.kv.Store(&kv)
	}
}

func (p *Provider) store() provider.KV { return *p.kv.Load() }

// loadChecks reads the stored results; none is an empty set.
func (p *Provider) loadChecks() (storedChecks, error) {
	var s storedChecks
	b, ok, err := p.store().KVGet(checkKVKey)
	if err != nil {
		return s, fmt.Errorf("read the stored update checks: %w", err)
	}
	if !ok {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("decode the stored update checks: %w", err)
	}
	return s, nil
}

// updateOf is the stored update status of container id running imageID:
// notChecked unless the last check saw this container with this image.
func (s storedChecks) updateOf(id, imageID string) spec.UpdateCheck {
	if c, ok := s.Containers[id]; ok && c.ImageID == imageID && imageID != "" {
		return c.Update
	}
	return spec.UpdateCheck{Status: spec.UpdateNotChecked}
}

func (p *Provider) checkBinding() provider.Binding {
	return provider.InProcess(spec.CheckUpdates, provider.InProcessSpec[spec.CheckUpdatesInput, spec.CheckUpdatesResult]{
		Lane: func(context.Context, spec.CheckUpdatesInput) (provider.Lane, error) {
			return provider.Lane{Key: checkLaneKey, Policy: provider.LaneReject}, nil
		},
		Run: p.checkUpdates,
	})
}

// checkTarget is one container to check.
type checkTarget struct {
	name    string
	id      string
	imageID string
	ref     string
	update  spec.UpdateCheck
}

// checkUpdates classifies every container and stores the results. A
// per-container failure is that container's unknown; only a failure to
// list the containers, or to store the results, fails the operation.
func (p *Provider) checkUpdates(ctx context.Context, _ spec.CheckUpdatesInput, r *provider.Run) (spec.CheckUpdatesResult, error) {
	var none spec.CheckUpdatesResult
	cli, err := p.client()
	if err != nil {
		return none, err
	}
	callCtx, cancel := p.callCtx(ctx)
	res, err := cli.ContainerList(callCtx, client.ContainerListOptions{All: true})
	cancel()
	if err != nil {
		return none, p.engineError(ctx, err, "list containers", nil)
	}
	targets := make([]*checkTarget, 0, len(res.Items))
	for _, s := range res.Items {
		if name := primaryName(s.Names); name != "" {
			targets = append(targets, &checkTarget{name: name, id: s.ID})
		}
	}
	slices.SortFunc(targets, func(a, b *checkTarget) int { return strings.Compare(a.name, b.name) })

	configDir := p.Settings().ConfigDir
	creds, credErr := loadRegistryConfig(configDir)
	if credErr != nil {
		p.log.Warn("registry credentials unreadable", "provider", spec.Name, "error", credErr.Error())
	}
	c := &checker{p: p, cli: cli, creds: creds, credErr: credErr, configDir: configDir,
		images: newImageCache(), latest: map[string]*latestEntry{}}
	say(r, "checking %d containers for image updates", len(targets))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(checkConcurrency)
	for _, t := range targets {
		g.Go(func() error {
			c.check(gctx, t)
			return nil
		})
	}
	_ = g.Wait() // check never fails: every outcome is a status.
	if err := ctx.Err(); err != nil {
		return none, err
	}

	now := time.Now().UTC()
	stored := storedChecks{CheckedAt: now, Containers: map[string]storedCheck{}}
	out := spec.CheckUpdatesResult{CheckedAt: now, Items: []spec.ContainerUpdate{}}
	for _, t := range targets {
		if t.id == "" {
			continue // Removed since the list.
		}
		t.update.CheckedAt = &now
		stored.Containers[t.id] = storedCheck{ImageID: t.imageID, Update: t.update}
		out.Items = append(out.Items, apitypes.NewResource(p.host, spec.Name, spec.ResourceType, t.name,
			spec.ContainerUpdateProperties{ContainerID: t.id, ImageReference: t.ref, Update: t.update}))
		line := fmt.Sprintf("%s: %s", t.name, t.update.Status)
		if t.update.Reason != "" {
			line += " (" + t.update.Reason + ")"
		}
		say(r, "%s", line)
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return none, fmt.Errorf("encode the update checks: %w", err)
	}
	if err := p.store().KVPut(checkKVKey, b); err != nil {
		return none, fmt.Errorf("store the update checks: %w", err)
	}
	return out, nil
}

// checker is the state of one check-updates run.
type checker struct {
	p         *Provider
	cli       *client.Client
	creds     *registryConfig
	credErr   error
	configDir string
	images    *imageCache
	mu        sync.Mutex
	latest    map[string]*latestEntry // By reference: one registry call each
}

// latestEntry is one reference's registry answer, shared by every
// container created from it.
type latestEntry struct {
	once   sync.Once
	digest string
	fail   *spec.UpdateCheck // Set when the registry call failed
}

// check classifies one container into t. Every early return sets a
// status other than upToDate.
func (c *checker) check(ctx context.Context, t *checkTarget) {
	p := c.p
	callCtx, cancel := p.callCtx(ctx)
	res, err := c.cli.ContainerInspect(callCtx, t.id, client.ContainerInspectOptions{})
	cancel()
	switch {
	case cerrdefs.IsNotFound(err):
		t.id = "" // Removed since the list.
		return
	case err != nil:
		t.update = unknown(spec.ReasonEngineError, "the Docker Engine could not describe the container: "+describe(err, p.timeout))
		return
	}
	ctr := res.Container
	t.imageID = ctr.Image
	if ctr.Config != nil {
		t.ref = ctr.Config.Image
	}
	if u, done := classifyLocal(ctr, t.ref); done {
		t.update = u
		return
	}
	named, err := reference.ParseNormalizedNamed(t.ref)
	if err != nil {
		t.update = notUpdatable(spec.ReasonNoRegistrySource, fmt.Sprintf("the image reference %q names no registry repository", t.ref))
		return
	}
	digests, missing, err := c.images.lookup(ctx, p, c.cli, ctr.Image)
	switch {
	case err != nil:
		t.update = unknown(spec.ReasonEngineError, "the Docker Engine could not describe the container's image")
		return
	case missing:
		t.update = unknown(spec.ReasonEngineError, "the container's image "+ctr.Image+" is no longer on this host, "+
			"so its digest cannot be compared")
		return
	}
	current := repoDigestsFor(named, digests)
	if len(current) == 0 {
		t.update = notUpdatable(spec.ReasonLocallyBuilt, fmt.Sprintf("the image has no registry digest for %s: it "+
			"was built, loaded or retagged on this host, not pulled from a registry. %s", named.Name(), byScript))
		return
	}
	t.update = c.compare(ctx, reference.TagNameOnly(named), current)
}

// byScript is the remedy for a locally built image (ASM-8).
const byScript = "Update it with the script that builds it (the scripts provider), then recreate the container"

// classifyLocal decides what needs no registry: an image-ID reference, a
// digest-pinned reference and an auto-removed container.
func classifyLocal(ctr container.InspectResponse, ref string) (spec.UpdateCheck, bool) {
	if isImageID(ref, ctr.Image) {
		return notUpdatable(spec.ReasonNoRegistrySource, "the container was created from an image ID, which "+
			"names no registry to compare with"), true
	}
	if named, err := reference.ParseNormalizedNamed(ref); err == nil {
		if _, ok := named.(reference.Canonical); ok {
			return notUpdatable(spec.ReasonDigestPinned, "the container was created from a digest-pinned "+
				"reference ("+ref+"), whose image never changes"), true
		}
	}
	if ctr.HostConfig != nil && ctr.HostConfig.AutoRemove {
		return notUpdatable(spec.ReasonAutoRemove, "the container is removed when it stops (--rm), so it cannot "+
			"be replaced by an update"), true
	}
	return spec.UpdateCheck{}, false
}

var hexRE = regexp.MustCompile(`^[a-f0-9]{1,64}$`)

// isImageID reports whether ref is the image's ID or a prefix of it, with or
// without "sha256:" (docker run accepts both).
func isImageID(ref, imageID string) bool {
	hex := strings.TrimPrefix(imageID, "sha256:")
	r := strings.TrimPrefix(ref, "sha256:")
	return ref != "" && hexRE.MatchString(r) && strings.HasPrefix(hex, r)
}

// repoDigestsFor are the valid digests of the RepoDigests entries for
// named's repository. Entries of other repositories describe other
// sources and are ignored (A-32).
func repoDigestsFor(named reference.Named, repoDigests []string) []string {
	var out []string
	for _, rd := range repoDigests {
		n, err := reference.ParseNormalizedNamed(rd)
		if err != nil {
			continue
		}
		if c, ok := n.(reference.Canonical); ok && n.Name() == named.Name() && c.Digest().Validate() == nil {
			out = append(out, c.Digest().String())
		}
	}
	return out
}

// compare asks the registry for ref's digest and compares it with the
// running image's.
func (c *checker) compare(ctx context.Context, ref reference.Named, current []string) spec.UpdateCheck {
	latest, fail := c.latestOf(ctx, ref)
	if fail != nil {
		f := *fail
		if f.Status == spec.UpdateUnknown {
			f.CurrentDigest = current[0]
		}
		return f
	}
	if slices.Contains(current, latest) {
		return spec.UpdateCheck{Status: spec.UpdateUpToDate, CurrentDigest: latest, LatestDigest: latest}
	}
	// On the containerd image store every tagged image has a RepoDigest, a
	// local build included, so a digest that differs from the registry's
	// does not yet mean a newer image: the registry must also know the
	// running one (CR-2).
	if u, done := c.runningDigestUnknown(ctx, ref, current); done {
		return u
	}
	return spec.UpdateCheck{Status: spec.UpdateAvailable, CurrentDigest: current[0], LatestDigest: latest}
}

// runningDigestUnknown asks the registry for the running image's digest
// and, when it does not know it, classifies the image as built on this host.
// A manifest the registry has since garbage-collected reads the same way,
// which is the safe direction: such a container is not replaced. An answer
// that cannot be had is unknown, so that updateAvailable is only ever
// reported for an image the registry is known to have served.
func (c *checker) runningDigestUnknown(ctx context.Context, ref reference.Named, current []string) (spec.UpdateCheck, bool) {
	domain := strings.ToLower(reference.Domain(ref))
	creds, err := c.creds.forRegistry(domain)
	if err != nil {
		return unknown(spec.ReasonCredentialConfig, "the registry credential could not be used: "+err.Error()), true
	}
	known, err := registryKnows(ctx, c.cli, c.p.callCtx, ref, current, creds.encoded)
	switch {
	case err != nil && ctx.Err() != nil:
		return unknown(spec.ReasonRegistryError, "the check was interrupted before the registry answered"), true
	case err != nil:
		return unknown(spec.ReasonRegistryError, fmt.Sprintf("the registry check for the running image of %s failed: %s",
			ref.String(), registryText(err.Error()))), true
	case !known:
		return notUpdatable(spec.ReasonLocallyBuilt, fmt.Sprintf("the registry %s has no image with the running digest %s "+
			"for %s: it was built or retagged on this host. %s", domain, current[0], ref.Name(), byScript)), true
	}
	return spec.UpdateCheck{}, false
}

// registryKnows reports whether the registry serves any of digests, the
// running image's digests for named's repository. A manifest unknown
// answer means it does not; any other failure is returned.
func registryKnows(ctx context.Context, cli *client.Client, call func(context.Context) (context.Context, context.CancelFunc),
	named reference.Named, digests []string, auth string) (bool, error) {
	for _, d := range digests {
		callCtx, cancel := call(ctx)
		_, err := cli.DistributionInspect(callCtx, reference.TrimNamed(named).Name()+"@"+d,
			client.DistributionInspectOptions{EncodedRegistryAuth: auth})
		cancel()
		switch {
		case err == nil:
			return true, nil
		case !absent(err):
			return false, err
		}
	}
	return false, nil
}

// latestOf is the registry's digest for ref, asked once per run.
func (c *checker) latestOf(ctx context.Context, ref reference.Named) (string, *spec.UpdateCheck) {
	key := ref.String()
	c.mu.Lock()
	e, ok := c.latest[key]
	if !ok {
		e = &latestEntry{}
		c.latest[key] = e
	}
	c.mu.Unlock()
	e.once.Do(func() { e.digest, e.fail = c.ask(ctx, ref) })
	return e.digest, e.fail
}

// ask makes the registry call: anonymous unless auths has a credential for
// exactly this registry.
func (c *checker) ask(ctx context.Context, ref reference.Named) (string, *spec.UpdateCheck) {
	p := c.p
	domain := strings.ToLower(reference.Domain(ref))
	if c.credErr != nil {
		u := unknown(spec.ReasonCredentialConfig, "the registry credentials could not be read: "+c.credErr.Error())
		return "", &u
	}
	creds, err := c.creds.forRegistry(domain)
	if err != nil {
		u := unknown(spec.ReasonCredentialConfig, "the registry credential could not be used: "+err.Error())
		return "", &u
	}
	callCtx, cancel := p.callCtx(ctx)
	res, err := c.cli.DistributionInspect(callCtx, ref.String(), client.DistributionInspectOptions{EncodedRegistryAuth: creds.encoded})
	cancel()
	if err == nil {
		if d := res.Descriptor.Digest; d.Validate() == nil {
			return d.String(), nil
		}
		u := unknown(spec.ReasonRegistryError, "the registry answered without a valid digest for "+ref.String())
		return "", &u
	}
	// Never the credential: the Engine's text names the registry and the
	// failure only.
	p.log.Warn("registry check failed", "provider", spec.Name, "reference", ref.String(),
		"authenticated", creds.encoded != "", "error", registryText(err.Error()))
	var u spec.UpdateCheck
	switch {
	case ctx.Err() != nil:
		u = unknown(spec.ReasonRegistryError, "the check was interrupted before the registry answered")
	case errors.Is(err, context.DeadlineExceeded):
		u = unknown(spec.ReasonRegistryError, fmt.Sprintf("the registry %s did not answer within %s", domain, p.timeout))
	case absent(err):
		// On the containerd image store every tagged image has a RepoDigest
		// (name@target), built or pulled, so a never-pushed local build is
		// recognized here: its registry has no such manifest. The classic
		// store has no RepoDigest for it and never gets this far.
		u = notUpdatable(spec.ReasonLocallyBuilt, fmt.Sprintf("the registry %s has no image for %s (%s): it was "+
			"built or retagged on this host and never pushed. %s", domain, ref.String(), registryText(err.Error()), byScript))
	case refused(err) && creds.encoded != "":
		u = unknown(spec.ReasonCredentialRefused, fmt.Sprintf("the registry %s refused the credential in auths[%q] of "+
			"%s: %s. Run docker login %s again for that configuration", domain, creds.key, c.creds.path,
			registryText(err.Error()), domain))
	case refused(err) && creds.helper != "":
		u = unknown(spec.ReasonCredentialHelper, fmt.Sprintf("the registry %s refused anonymous access, and its "+
			"credential is held by the credential helper %s (%s in %s), which Nexus never runs. Store the "+
			"credential in auths instead: run docker login %s with a configuration that has no credsStore or "+
			"credHelpers entry for it, or set providers.docker.configDir to a directory whose config.json has it "+
			"in auths", domain, creds.helper, creds.helperSource, c.creds.path, domain))
	case refused(err):
		u = unknown(spec.ReasonAuthRequired, fmt.Sprintf("the registry %s refused anonymous access, and %s has no "+
			"credential for it in auths: run docker login %s for that configuration, or set "+
			"providers.docker.configDir to the directory of a config.json that has one. A registry also answers so "+
			"for a repository that does not exist: if this image was built on this host and never pushed, it "+
			"cannot be updated from a registry; %s", domain, c.creds.path, domain, lowerFirst(byScript)))
	case client.IsErrConnectionFailed(err):
		u = unknown(spec.ReasonEngineError, "the Docker Engine is not reachable")
	default:
		u = unknown(spec.ReasonRegistryError, fmt.Sprintf("the registry check for %s failed: %s", ref.String(), registryText(err.Error())))
	}
	return "", &u
}

// absent reports whether the registry answered that the reference does
// not exist (manifest or repository unknown), as opposed to refusing it.
func absent(err error) bool {
	if refused(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return cerrdefs.IsNotFound(err) || strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "name unknown")
}

func lowerFirst(s string) string { return strings.ToLower(s[:1]) + s[1:] }

// refused reports whether the registry refused access: the Engine's 401 or
// 403, or its text for a registry's 401 that it reports with another status.
func refused(err error) bool {
	if cerrdefs.IsUnauthorized(err) || cerrdefs.IsPermissionDenied(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "no basic auth credentials") || strings.Contains(msg, "access denied") ||
		strings.Contains(msg, "denied:") || strings.Contains(msg, "may require 'docker login'")
}

// registryText is the Engine's error text, quoted in a reason: control
// characters removed and the length bounded.
func registryText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxRegistryText {
		s = string(r[:maxRegistryText]) + "..."
	}
	return s
}

func notUpdatable(reason, detail string) spec.UpdateCheck {
	return spec.UpdateCheck{Status: spec.UpdateNotUpdatable, Reason: reason, Detail: detail}
}

func unknown(reason, detail string) spec.UpdateCheck {
	return spec.UpdateCheck{Status: spec.UpdateUnknown, Reason: reason, Detail: detail}
}

// statusFilter parses the list's filter values into the statuses to keep;
// nil keeps every container.
func statusFilter(filters []string) (map[string]bool, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	keep := map[string]bool{}
	for _, f := range filters {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k != spec.StatusFilter || !slices.Contains([]string{spec.UpdateUpToDate, spec.UpdateAvailable,
			spec.UpdateNotUpdatable, spec.UpdateUnknown, spec.UpdateNotChecked}, v) {
			return nil, problem.New(problem.ValidationFailed, "the input is invalid").
				With("errors", []problem.FieldError{{Path: "/filter", Message: "must be status=<upToDate|updateAvailable|notUpdatable|unknown|notChecked>"}})
		}
		keep[v] = true
	}
	return keep, nil
}
