package docker

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"golang.org/x/sync/errgroup"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// inspectConcurrency bounds the container inspections of one list call.
const inspectConcurrency = 8

var (
	nameRE   = regexp.MustCompile(spec.NamePattern)
	cursorRE = regexp.MustCompile(spec.CursorPattern)
)

func (p *Provider) client() (*client.Client, error) {
	cli := p.engine.Load().cli
	if cli == nil {
		return nil, unavailable("the docker section was not configured")
	}
	return cli, nil
}

func (p *Provider) list(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Container], error) {
	var none apitypes.List[spec.Container]
	after := ""
	if in.Cursor != "" {
		name, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || !cursorRE.MatchString(in.Cursor) || !nameRE.Match(name) {
			return none, invalid("/cursor", "is not a cursor this server issued")
		}
		after = string(name)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	keep, err := statusFilter(in.Filter)
	if err != nil {
		return none, err
	}
	cli, err := p.client()
	if err != nil {
		return none, err
	}
	checks, err := p.loadChecks()
	if err != nil {
		return none, err
	}

	callCtx, cancel := p.callCtx(ctx)
	res, err := cli.ContainerList(callCtx, client.ContainerListOptions{All: true})
	cancel()
	if err != nil {
		return none, p.engineError(ctx, err, "list containers", nil)
	}
	type entry struct{ name, id string }
	entries := make([]entry, 0, len(res.Items))
	for _, s := range res.Items {
		// The filter applies before paging, from the summary's image ID, so
		// a page is full and cursors stay stable (A-13).
		if keep != nil && !keep[checks.updateOf(s.ID, s.ImageID).Status] {
			continue
		}
		if name := primaryName(s.Names); name != "" {
			entries = append(entries, entry{name: name, id: s.ID})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	start, _ := slices.BinarySearchFunc(entries, after, func(e entry, n string) int { return strings.Compare(e.name, n) })
	if start < len(entries) && entries[start].name == after {
		start++
	}
	end := min(start+limit, len(entries))
	page := entries[start:end]

	// Inspection by ID: the list's names are already resolved, and a name
	// reused meanwhile must not substitute another container.
	images := newImageCache()
	found := make([]*spec.Container, len(page))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(inspectConcurrency)
	for i, e := range page {
		g.Go(func() error {
			c, err := p.inspect(gctx, cli, e.id, images, checks)
			if err != nil {
				var perr *problem.Error
				if errors.As(err, &perr) && perr.Slug() == problem.NotFound {
					return nil // Removed since the list: not part of this page.
				}
				return err
			}
			if keep != nil && !keep[c.Properties.Update.Status] {
				return nil // Its image changed since the list.
			}
			found[i] = &c
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return none, err
	}
	out := apitypes.List[spec.Container]{Items: make([]spec.Container, 0, len(page))}
	for _, c := range found {
		if c != nil {
			out.Items = append(out.Items, *c)
		}
	}
	if end < len(entries) {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1].name))
	}
	return out, nil
}

func (p *Provider) get(ctx context.Context, in spec.ContainerInput) (spec.Container, error) {
	if !validName(in.Name) {
		return spec.Container{}, invalid("/name", "must match "+spec.NamePattern)
	}
	cli, err := p.client()
	if err != nil {
		return spec.Container{}, err
	}
	checks, err := p.loadChecks()
	if err != nil {
		return spec.Container{}, err
	}
	return p.inspect(ctx, cli, in.Name, newImageCache(), checks)
}

func validName(name string) bool {
	return len(name) <= spec.MaxNameLength && nameRE.MatchString(name)
}

// inspect reads one container, and its image through images, into the wire
// form, with its stored update status from checks. ref is a name or an ID.
func (p *Provider) inspect(ctx context.Context, cli *client.Client, ref string, images *imageCache,
	checks storedChecks) (spec.Container, error) {
	callCtx, cancel := p.callCtx(ctx)
	res, err := cli.ContainerInspect(callCtx, ref, client.ContainerInspectOptions{})
	cancel()
	if err != nil {
		return spec.Container{}, p.engineError(ctx, err, "inspect container", notFound())
	}
	c := res.Container
	digests, err := images.repoDigests(ctx, p, cli, c.Image)
	if err != nil {
		return spec.Container{}, err
	}
	return p.resource(c, digests, checks.updateOf(c.ID, c.Image)), nil
}

// resource is the wire form of an inspected container.
func (p *Provider) resource(c container.InspectResponse, repoDigests []string, update spec.UpdateCheck) spec.Container {
	props := spec.ContainerProperties{
		ContainerID: c.ID,
		Health:      spec.HealthNone,
		Ports:       []spec.Port{},
		Update:      update,
	}
	if c.Config != nil {
		props.ImageReference = c.Config.Image
		props.ComposeProject = c.Config.Labels[spec.ComposeProjectLabel]
		props.ComposeService = c.Config.Labels[spec.ComposeServiceLabel]
	}
	props.ImageDigest = repoDigest(props.ImageReference, repoDigests)
	if c.State != nil {
		props.State = string(c.State.Status)
		if c.State.Health != nil && c.State.Health.Status != "" {
			props.Health = string(c.State.Health.Status)
		}
	}
	if c.HostConfig != nil {
		props.RestartPolicy = spec.RestartPolicy{
			Name:              string(c.HostConfig.RestartPolicy.Name),
			MaximumRetryCount: c.HostConfig.RestartPolicy.MaximumRetryCount,
		}
	}
	if c.NetworkSettings != nil {
		props.Ports = ports(c.NetworkSettings.Ports)
	}
	return apitypes.NewResource(p.host, spec.Name, spec.ResourceType, strings.TrimPrefix(c.Name, "/"), props)
}

// ports flattens Docker's port map: one entry per host binding, or one
// without a host side for a port that is exposed but not published.
func ports(m network.PortMap) []spec.Port {
	out := []spec.Port{}
	for port, bindings := range m {
		base := spec.Port{ContainerPort: int(port.Num()), Protocol: string(port.Proto())}
		if len(bindings) == 0 {
			out = append(out, base)
			continue
		}
		for _, b := range bindings {
			e := base
			if b.HostIP.IsValid() {
				e.HostIP = b.HostIP.String()
			}
			if n, err := strconv.Atoi(b.HostPort); err == nil {
				e.HostPort = n
			}
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b spec.Port) int {
		return cmp.Or(cmp.Compare(a.ContainerPort, b.ContainerPort), strings.Compare(a.Protocol, b.Protocol),
			strings.Compare(a.HostIP, b.HostIP), cmp.Compare(a.HostPort, b.HostPort))
	})
	return out
}

// primaryName is a container's own name from a list summary. Names also
// holds the link aliases of other containers, as "/other/alias"; the own
// name has a single leading slash.
func primaryName(names []string) string {
	for _, n := range names {
		if rest, ok := strings.CutPrefix(n, "/"); ok && !strings.Contains(rest, "/") {
			return rest
		}
	}
	return ""
}

// repoDigest is the digest of the RepoDigests entry for ref's repository,
// "" when ref names no repository (an image ID) or the image has no digest
// for it (built or retagged locally). An entry for another repository is
// never used: it would describe a different source (A-32).
func repoDigest(ref string, repoDigests []string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	for _, rd := range repoDigests {
		n, err := reference.ParseNormalizedNamed(rd)
		if err != nil {
			continue
		}
		if c, ok := n.(reference.Canonical); ok && n.Name() == named.Name() {
			return c.Digest().String()
		}
	}
	return ""
}

// imageCache inspects each image ID at most once within one request.
type imageCache struct {
	mu      sync.Mutex
	entries map[string]*imageEntry
}

type imageEntry struct {
	once    sync.Once
	digests []string
	missing bool // The image is no longer on this host
	err     error
}

func newImageCache() *imageCache { return &imageCache{entries: map[string]*imageEntry{}} }

// repoDigests returns the RepoDigests of image id. An image removed since
// the container was created has none.
func (ic *imageCache) repoDigests(ctx context.Context, p *Provider, cli *client.Client, id string) ([]string, error) {
	digests, _, err := ic.lookup(ctx, p, cli, id)
	return digests, err
}

// lookup returns the RepoDigests of image id, and whether the image is
// missing: removed since the container was created.
func (ic *imageCache) lookup(ctx context.Context, p *Provider, cli *client.Client, id string) ([]string, bool, error) {
	if id == "" {
		return nil, true, nil
	}
	ic.mu.Lock()
	e, ok := ic.entries[id]
	if !ok {
		e = &imageEntry{}
		ic.entries[id] = e
	}
	ic.mu.Unlock()
	e.once.Do(func() {
		callCtx, cancel := p.callCtx(ctx)
		defer cancel()
		res, err := cli.ImageInspect(callCtx, id)
		switch {
		case cerrdefs.IsNotFound(err):
			e.missing = true
		case err != nil:
			e.err = p.engineError(ctx, err, "inspect image", nil)
		default:
			e.digests = res.RepoDigests
		}
	})
	return e.digests, e.missing, e.err
}

func notFound() *problem.Error {
	return problem.New(problem.NotFound, "no container with that name or ID exists")
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
