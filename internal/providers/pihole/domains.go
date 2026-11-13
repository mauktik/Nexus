package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ backend.DomainMutator = (*v5.Mutator)(nil)

// domainLostDetail is the outcome-unknown detail of an add-domain or
// remove-domain unit lost without a result.
const domainLostDetail = "the entry may have been added or removed; lists may not be reloaded"

var exactDomainRE = regexp.MustCompile(spec.ExactDomainPattern)

// The rule of remove-domain's addressing forms (FR-11).
const removeFormRule = "give either id, or list, kind and value together"

// domainBindings binds add-domain and remove-domain. add-domain's Finalize
// names the result's resource with this provider's host, which the helper
// does not know.
func (p *Provider) domainBindings() []provider.Binding {
	addID := spec.AddDomain.Op().ID
	addLost := func(*provider.Run, spec.DomainAddInput) string { return domainLostDetail }
	add := provider.Detached(spec.AddDomain, provider.DetachedSpec[spec.DomainAddInput, spec.DomainResult]{
		Admit: p.admitAddDomain,
		Lane:  piholeLane[spec.DomainAddInput],
		Prepare: func(ctx context.Context, in spec.DomainAddInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareHelper(ctx, addID, in, maxRuntimeDefault, r)
		},
		Finalize: func(_ context.Context, in spec.DomainAddInput, x detach.Exit, r *provider.Run) (spec.DomainResult, error) {
			res, err := finalizeHelper[spec.DomainResult](p, x, r, func() string { return addLost(r, in) })
			if err != nil {
				return spec.DomainResult{}, err
			}
			res.Domain = p.domainResource(res.Domain.Properties)
			return res, nil
		},
		LostDetail: addLost,
	})
	return []provider.Binding{
		add,
		detachedOp(p, spec.RemoveDomain, p.admitRemoveDomain, maxRuntimeDefault,
			func(*provider.Run, spec.DomainRemoveInput) string { return domainLostDetail }),
	}
}

// registerDomainActions adds the helper actions of add-domain and
// remove-domain.
func registerDomainActions(m map[string]helperAction) {
	m[spec.AddDomain.Op().ID] = runAddDomain
	m[spec.RemoveDomain.Op().ID] = runRemoveDomain
}

// checkExact is the Admit rule the schema cannot state: an exact entry's
// value must be a lowercase domain (CON-2, A-7).
func checkExact(kind, value string) *problem.Error {
	if kind == spec.KindExact && !exactDomainRE.MatchString(value) {
		return invalid("/value", "an exact entry must be a lowercase domain: must match "+spec.ExactDomainPattern)
	}
	return nil
}

// admitAddDomain refuses, before an operation exists, an exact value that
// is not a domain (400 at /value) and a value already on the opposite list
// with the same kind (409 naming that entry, ASM-8). The helper checks the
// conflict again inside its transaction.
func (p *Provider) admitAddDomain(ctx context.Context, in spec.DomainAddInput) error {
	if pe := checkExact(in.Kind, in.Value); pe != nil {
		return probErr(pe)
	}
	rows, err := p.domains(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Value == in.Value && r.Kind == in.Kind && r.List != in.List {
			return probErr(problem.New(problem.Conflict, fmt.Sprintf("the value is already on the %s list as %s "+
				"entry %d; an entry is never moved between lists: remove entry %d first", r.List, r.Kind, r.PiholeID, r.PiholeID)))
		}
	}
	return nil
}

// admitRemoveDomain requires exactly one addressing form (400 naming the
// rule) and an entry that exists (404), before an operation exists.
func (p *Provider) admitRemoveDomain(ctx context.Context, in spec.DomainRemoveInput) error {
	byValue := in.List != "" || in.Kind != "" || in.Value != ""
	full := in.List != "" && in.Kind != "" && in.Value != ""
	switch {
	case in.ID != 0 && byValue:
		return invalid("", removeFormRule+", not both")
	case in.ID == 0 && !full:
		return invalid("", removeFormRule)
	case in.ID < 0:
		return invalid("/id", "must be at least 1")
	}
	rows, err := p.domains(ctx)
	if err != nil {
		return err
	}
	if _, ok := findDomain(rows, in); !ok {
		return notFound("no pi-hole domain entry matches; nothing was changed")
	}
	return nil
}

// findDomain is the row in already Admit-checked in names: by id, or by
// list, kind and value.
func findDomain(rows []spec.DomainProperties, in spec.DomainRemoveInput) (spec.DomainProperties, bool) {
	for _, r := range rows {
		if in.ID != 0 && r.PiholeID == in.ID ||
			in.ID == 0 && r.List == in.List && r.Kind == in.Kind && r.Value == in.Value {
			return r, true
		}
	}
	return spec.DomainProperties{}, false
}

// runAddDomain adds the entry; created is false when it was already on that
// list, and then nothing changed.
func runAddDomain(ctx context.Context, c *helperCall) (any, error) {
	dm, err := domainMutator(c)
	if err != nil {
		return nil, err
	}
	var in spec.DomainAddInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the add-domain input: %v; nothing was changed", err)))
	}
	if pe := checkExact(in.Kind, in.Value); pe != nil {
		return nil, probErr(problem.New(problem.InternalError, "the add-domain input has an exact value that is not "+
			"a domain; nothing was changed"))
	}
	got, created, err := dm.AddDomain(ctx, in, c.Out)
	if err != nil {
		return nil, err
	}
	return spec.DomainResult{
		ActionResult: apitypes.ActionResult{Changed: created},
		Created:      created,
		// Finalize, in nexusd, names it with the provider's host.
		Domain: apitypes.NewResource("", spec.Name, spec.DomainResourceType, strconv.FormatInt(got.PiholeID, 10), got),
	}, nil
}

// runRemoveDomain removes the entry named by id, or by list, kind and
// value, which it resolves to an id from gravity.db first.
func runRemoveDomain(ctx context.Context, c *helperCall) (any, error) {
	dm, err := domainMutator(c)
	if err != nil {
		return nil, err
	}
	var in spec.DomainRemoveInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the remove-domain input: %v; nothing was changed", err)))
	}
	id := in.ID
	if id == 0 {
		rows, err := c.Reader.Domains(ctx)
		if err != nil {
			var pe *problem.Error
			if errors.As(err, &pe) {
				return nil, err
			}
			return nil, probErr(problem.New(problem.DependencyUnavailable,
				fmt.Sprintf("gravity.db could not be read to find the entry: %v; nothing was changed", err)).
				With("provider", spec.Name))
		}
		r, ok := findDomain(rows, in)
		if !ok {
			return nil, probErr(notFound("no pi-hole domain entry matches; nothing was changed"))
		}
		id = r.PiholeID
		fmt.Fprintf(c.Out, "pihole: the entry is id %d\n", id)
	}
	got, err := dm.RemoveDomain(ctx, id, c.Out)
	if err != nil {
		return nil, err
	}
	return spec.DomainRemoveResult{ActionResult: apitypes.ActionResult{Changed: true}, Removed: got}, nil
}

func domainMutator(c *helperCall) (backend.DomainMutator, error) {
	dm, ok := c.Mutator.(backend.DomainMutator)
	if !ok {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot change domains; nothing was changed", c.In.Backend)))
	}
	return dm, nil
}
