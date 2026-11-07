// Package scripts is the scripts provider: it runs scripts that the host's
// administrator cataloged in the root-owned configuration file, and nothing
// else (FR-18, FR-25). Its declarations live in the spec subpackage.
//
// The catalog is read-only through every surface. A reload builds a new
// Catalog and swaps it in atomically; an operation already admitted keeps
// what it was started with, because its process spec is written to its own
// directory (spec.json) and never re-read from the catalog.
package scripts

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"sync/atomic"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// Options configures New.
type Options struct {
	Host string // Reported in every resource
	// Preflight replaces the systemd check. Nil means the real one. T0 tests,
	// on hosts without systemd, set it to exercise the read path.
	Preflight func(context.Context) provider.Status
}

// Provider is the scripts provider.
type Provider struct {
	host      string
	preflight func(context.Context) provider.Status
	catalog   atomic.Pointer[Catalog]
}

// New returns the provider with an empty catalog until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, preflight: opts.Preflight}
	if p.preflight == nil {
		p.preflight = systemdPreflight
	}
	p.catalog.Store(&Catalog{index: map[string]int{}})
	return p
}

// Spec returns the declarations.
func (p *Provider) Spec() def.ProviderSpec { return spec.Spec() }

// Configure strictly decodes the scripts section and swaps in its catalog. A
// section it refuses leaves the running catalog as it was.
func (p *Provider) Configure(decode func(into any) error) error {
	c, err := decodeCatalog(decode)
	if err != nil {
		return err
	}
	p.catalog.Store(c)
	return nil
}

// Catalog returns the current catalog. It is immutable: a reload replaces it.
func (p *Provider) Catalog() *Catalog { return p.catalog.Load() }

// Preflight reports whether scripts can run: only in a systemd transient
// unit, so a run survives nexusd. Without systemd the provider is
// unavailable; it never falls back to a child of nexusd (CON-4).
func (p *Provider) Preflight(ctx context.Context) provider.Status { return p.preflight(ctx) }

func systemdPreflight(context.Context) provider.Status {
	a := detach.SystemdAvailable()
	if !a.Available {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: "scripts run only under systemd: " + a.Reason}
	}
	return provider.Status{State: apitypes.ProviderEnabled}
}

// Bindings binds the read operations and invoke.
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Detached(spec.Invoke, provider.DetachedSpec[spec.InvokeInput, spec.InvokeResult]{
			Admit:    p.admitInvoke,
			Prepare:  prepareInvoke,
			Finalize: finalizeInvoke,
		}),
	}
}

func (p *Provider) list(_ context.Context, in spec.ListInput) (apitypes.List[spec.Script], error) {
	var none apitypes.List[spec.Script]
	after := ""
	if in.Cursor != "" {
		id, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || !idRE.Match(id) {
			return none, invalid("/cursor", "is not a cursor this server issued")
		}
		after = string(id)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	scripts, more := p.catalog.Load().after(after, limit)
	out := apitypes.List[spec.Script]{Items: make([]spec.Script, len(scripts))}
	for i, s := range scripts {
		out.Items[i] = p.resource(s)
	}
	if more {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(scripts[len(scripts)-1].ID))
	}
	return out, nil
}

func (p *Provider) get(_ context.Context, in spec.ScriptInput) (spec.Script, error) {
	s, ok := p.catalog.Load().Get(in.Name)
	if !ok {
		return spec.Script{}, notFound()
	}
	return p.resource(s), nil
}

// resource is the wire form of s: everything but env values, and no pattern
// of a sensitive parameter, which may describe the secret.
func (p *Provider) resource(s Script) spec.Script {
	props := spec.ScriptProperties{
		Description:             s.Description,
		Path:                    s.Path,
		RunAs:                   s.RunAs,
		WorkingDirectory:        s.WorkingDirectory,
		TimeoutSeconds:          s.Timeout.Seconds(),
		Concurrency:             s.Concurrency,
		TerminationGraceSeconds: s.TerminationGrace.Seconds(),
		Env:                     s.EnvNames(),
		Parameters:              make([]spec.Parameter, len(s.Parameters)),
	}
	for i, prm := range s.Parameters {
		w := spec.Parameter{Name: prm.Name, Type: prm.Type, Sensitive: prm.Sensitive, Arg: prm.Arg, Env: prm.Env,
			Position: prm.Position}
		if !prm.Sensitive {
			w.Pattern = prm.Pattern
		}
		props.Parameters[i] = w
	}
	return apitypes.NewResource(p.host, spec.Name, spec.ResourceType, s.ID, props)
}

var idRE = regexp.MustCompile(spec.IDPattern)

// errNoRunner is returned by Prepare and Finalize, which admitInvoke keeps
// from ever being reached until the runner exists.
var errNoRunner = errors.New("scripts: the script runner is not built in")

// admitInvoke refuses an unknown script with not-found. Script execution
// (5.3) replaces the rest of this binding; until then every invocation of a
// cataloged script is refused here, before an operation exists, so nothing
// can start.
func (p *Provider) admitInvoke(_ context.Context, in spec.InvokeInput) error {
	if _, ok := p.catalog.Load().Get(in.Name); !ok {
		return notFound()
	}
	return problem.New(problem.CapabilityUnavailable, "this nexusd cannot run scripts").
		With("provider", spec.Name).With("reason", "the script runner is not built in")
}

func prepareInvoke(context.Context, spec.InvokeInput, *provider.Run) (detach.Spec, error) {
	return detach.Spec{}, errNoRunner
}

func finalizeInvoke(context.Context, spec.InvokeInput, detach.Exit, *provider.Run) (spec.InvokeResult, error) {
	return spec.InvokeResult{}, errNoRunner
}

func notFound() *problem.Error {
	return problem.New(problem.NotFound, "no script with that ID is in the catalog")
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
