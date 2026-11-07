// Package docker is the docker provider: it reads and manages the existing
// containers of the host's Docker Engine through the Engine API on the local
// Unix socket, with API version negotiation (CON-6). It never exposes the
// socket, and never creates, runs or execs into a container (FR-25). Its
// declarations live in the spec subpackage.
//
// The Engine being down affects this provider only: every call is bounded,
// a failure answers dependency-unavailable, and the next call after the
// Engine returns succeeds without restarting nexusd (NFR-3).
package docker

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// Options configures New.
type Options struct {
	Host   string       // Reported in every resource
	Logger *slog.Logger // Engine failures are logged here; nil means slog.Default()
	// CallTimeout replaces DefaultCallTimeout. Tests shorten it.
	CallTimeout time.Duration
	// Preflight replaces the ping. Nil means the real one. T0 contract tests,
	// on hosts without Docker, set it to serve the provider's routes.
	Preflight func(context.Context) provider.Status
}

// Provider is the docker provider.
type Provider struct {
	host      string
	log       *slog.Logger
	timeout   time.Duration
	preflight func(context.Context) provider.Status
	engine    atomic.Pointer[engine]
}

// engine is one configuration's settings and client. A reload builds a new
// one and swaps it in whole; a call in flight keeps the one it started with.
type engine struct {
	settings config.DockerConfig
	cli      *client.Client // Nil until Configure
}

// New returns the provider; it has no client until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, log: opts.Logger, timeout: opts.CallTimeout, preflight: opts.Preflight}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.timeout <= 0 {
		p.timeout = DefaultCallTimeout
	}
	p.engine.Store(&engine{})
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the docker section and builds a client for its
// socket. A section it refuses leaves the running client as it was. The
// client is replaced only when the socket changes; it opens no connection
// until a call.
func (p *Provider) Configure(decode func(into any) error) error {
	settings := config.Default().Providers.Docker.Settings
	if err := decode(&settings); err != nil {
		return err
	}
	cur := p.engine.Load()
	if cur.cli != nil && cur.settings.Socket == settings.Socket {
		p.engine.Store(&engine{settings: settings, cli: cur.cli})
		return nil
	}
	cli, err := newClient(settings.Socket)
	if err != nil {
		return err
	}
	p.engine.Store(&engine{settings: settings, cli: cli})
	if cur.cli != nil {
		// Only idle connections close; a call in flight on the old client
		// finishes.
		if err := cur.cli.Close(); err != nil {
			p.log.Debug("closing the previous Docker client", "error", err.Error())
		}
	}
	return nil
}

// Settings returns the docker section in force, defaults applied.
func (p *Provider) Settings() config.DockerConfig { return p.engine.Load().settings }

// Preflight pings the Engine, which also negotiates the API version. The
// negotiated version and the socket are reported in capabilities.
func (p *Provider) Preflight(ctx context.Context) provider.Status {
	if p.preflight != nil {
		return p.preflight(ctx)
	}
	e := p.engine.Load()
	if e.cli == nil {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: "the docker section was not configured"}
	}
	ctx, cancel := p.callCtx(ctx)
	defer cancel()
	ping, err := e.cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: reason(e.settings.Socket, err, p.timeout)}
	}
	return provider.Status{State: apitypes.ProviderEnabled, Details: map[string]any{
		"socket":           e.settings.Socket,
		"apiVersion":       e.cli.ClientVersion(),
		"daemonApiVersion": ping.APIVersion,
	}}
}

// Bindings binds the read operations and the lifecycle actions.
func (p *Provider) Bindings() []provider.Binding {
	return append([]provider.Binding{
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Stream(spec.Logs, p.logs),
	}, p.lifecycleBindings()...)
}
