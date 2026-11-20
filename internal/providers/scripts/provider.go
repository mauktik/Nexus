// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
	// Preflight replaces the detach check. Nil means the real one. T0 tests,
	// on hosts without detached operations, set it to exercise the read path.
	Preflight func(context.Context) provider.Status
}

// Provider is the scripts provider.
type Provider struct {
	host      string
	preflight func(context.Context) provider.Status
	catalog   atomic.Pointer[Catalog]
	machine   machine                    // Run-as accounts and the filesystem the runner reads
	available func() detach.Availability // detach.Available outside tests
}

// New returns the provider with an empty catalog until Configure.
func New(opts Options) *Provider {
	p := &Provider{host: opts.Host, preflight: opts.Preflight, machine: realMachine, available: detach.Available}
	if p.preflight == nil {
		p.preflight = p.detachPreflight
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

// Preflight reports whether scripts can run: only as detached operations (a
// systemd transient unit, or a supervised process on OpenRC), so a run
// survives nexusd. Without them the provider is unavailable; it never falls
// back to a child of nexusd (CON-4).
func (p *Provider) Preflight(ctx context.Context) provider.Status { return p.preflight(ctx) }

func (p *Provider) detachPreflight(context.Context) provider.Status {
	if a := p.available(); !a.Available {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: "scripts run only as detached operations: " + a.Reason}
	}
	return provider.Status{State: apitypes.ProviderEnabled}
}

// Bindings binds the read operations and invoke, which runs the script in
// a transient unit (run.go).
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Detached(spec.Invoke, provider.DetachedSpec[spec.InvokeInput, spec.InvokeResult]{
			Admit:      p.admitInvoke,
			Lane:       p.invokeLane,
			Prepare:    p.prepareInvoke,
			Finalize:   finalizeInvoke,
			LostDetail: lostDetail,
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
			w.Pattern, w.Enum = prm.Pattern, prm.Enum
		}
		props.Parameters[i] = w
	}
	return apitypes.NewResource(p.host, spec.Name, spec.ResourceType, s.ID, props)
}

var idRE = regexp.MustCompile(spec.IDPattern)

func notFound() *problem.Error {
	return problem.New(problem.NotFound, "no script with that ID is in the catalog")
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
