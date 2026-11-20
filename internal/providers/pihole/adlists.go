// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ backend.AdlistMutator = (*v5.Mutator)(nil)

var (
	adlistURLRE = regexp.MustCompile(spec.URLPattern)
	commentRE   = regexp.MustCompile(spec.CommentPattern)
)

// The rule of remove-adlist's addressing forms (FR-12).
const removeAdlistRule = "give either id or url"

// The outcome-unknown details of a unit lost without a result.
const (
	adlistLostDetail  = "the adlist may have been added or removed; list the adlists to see"
	gravityLostDetail = "pihole -g may not have finished; gravity.db holds the lists of the last run that " +
		"completed: check the gravity time with `nexus pihole get`"
)

// adlistRuntime is the MaxRuntime of an adlist op (A-3): an hour when it
// runs gravity, else the default.
func adlistRuntime(updateGravity bool) time.Duration {
	if updateGravity {
		return maxRuntimeGravity
	}
	return maxRuntimeDefault
}

func adlistLost(updateGravity bool) string {
	if updateGravity {
		return adlistLostDetail + "; " + gravityLostDetail
	}
	return adlistLostDetail
}

// adlistBindings binds add-adlist, remove-adlist and update-gravity.
// add-adlist's Finalize names the result's resource with this provider's
// host, which the helper does not know.
func (p *Provider) adlistBindings() []provider.Binding {
	addID, removeID := spec.AddAdlist.Op().ID, spec.RemoveAdlist.Op().ID
	addLost := func(_ *provider.Run, in spec.AdlistAddInput) string { return adlistLost(in.UpdateGravity) }
	removeLost := func(_ *provider.Run, in spec.AdlistRemoveInput) string { return adlistLost(in.UpdateGravity) }
	add := provider.Detached(spec.AddAdlist, provider.DetachedSpec[spec.AdlistAddInput, spec.AdlistResult]{
		Lane: piholeLane[spec.AdlistAddInput],
		Prepare: func(ctx context.Context, in spec.AdlistAddInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareHelper(ctx, addID, in, adlistRuntime(in.UpdateGravity), r)
		},
		Finalize: func(_ context.Context, in spec.AdlistAddInput, x detach.Exit, r *provider.Run) (spec.AdlistResult, error) {
			res, err := finalizeHelper[spec.AdlistResult](p, x, r, func() string { return addLost(r, in) })
			if err != nil {
				return spec.AdlistResult{}, err
			}
			res.Adlist = p.adlistResource(res.Adlist.Properties)
			return res, nil
		},
		LostDetail: addLost,
	})
	remove := provider.Detached(spec.RemoveAdlist, provider.DetachedSpec[spec.AdlistRemoveInput, spec.AdlistRemoveResult]{
		Admit: p.admitRemoveAdlist,
		Lane:  piholeLane[spec.AdlistRemoveInput],
		Prepare: func(ctx context.Context, in spec.AdlistRemoveInput, r *provider.Run) (detach.Spec, error) {
			return p.prepareHelper(ctx, removeID, in, adlistRuntime(in.UpdateGravity), r)
		},
		Finalize: func(_ context.Context, in spec.AdlistRemoveInput, x detach.Exit, r *provider.Run) (spec.AdlistRemoveResult, error) {
			return finalizeHelper[spec.AdlistRemoveResult](p, x, r, func() string { return removeLost(r, in) })
		},
		LostDetail: removeLost,
	})
	return []provider.Binding{
		add,
		remove,
		detachedOp(p, spec.UpdateGravity, nil, maxRuntimeGravity,
			func(*provider.Run, spec.NoInput) string { return gravityLostDetail }),
	}
}

// registerAdlistActions adds the helper actions of add-adlist,
// remove-adlist and update-gravity.
func registerAdlistActions(m map[string]helperAction) {
	m[spec.AddAdlist.Op().ID] = runAddAdlist
	m[spec.RemoveAdlist.Op().ID] = runRemoveAdlist
	m[spec.UpdateGravity.Op().ID] = runUpdateGravity
}

// admitRemoveAdlist requires exactly one of id and url (400 naming the
// rule) and an adlist that exists (404), before an operation exists.
func (p *Provider) admitRemoveAdlist(ctx context.Context, in spec.AdlistRemoveInput) error {
	switch {
	case in.ID != 0 && in.URL != "":
		return probErr(invalid("", removeAdlistRule+", not both"))
	case in.ID == 0 && in.URL == "":
		return probErr(invalid("", removeAdlistRule))
	case in.ID < 0:
		return probErr(invalid("/id", "must be at least 1"))
	}
	rows, err := p.adlists(ctx)
	if err != nil {
		return err
	}
	if _, ok := findAdlist(rows, in); !ok {
		return probErr(notFound("no pi-hole adlist matches; nothing was changed"))
	}
	return nil
}

// findAdlist is the row in already Admit-checked in names: by id or by url.
func findAdlist(rows []spec.AdlistProperties, in spec.AdlistRemoveInput) (spec.AdlistProperties, bool) {
	for _, r := range rows {
		if in.ID != 0 && r.PiholeID == in.ID || in.ID == 0 && r.URL == in.URL {
			return r, true
		}
	}
	return spec.AdlistProperties{}, false
}

// runAddAdlist adds the adlist; created is false when its URL was already
// listed. With updateGravity, gravity runs afterwards either way.
func runAddAdlist(ctx context.Context, c *helperCall) (any, error) {
	am, err := adlistMutator(c)
	if err != nil {
		return nil, err
	}
	var in spec.AdlistAddInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the add-adlist input: %v; nothing was changed", err)))
	}
	// The schema admitted both; webpage.sh splices them into SQL unescaped
	// (V-9), so the helper checks them again before they reach it (CON-7).
	if !adlistURLRE.MatchString(in.URL) || in.Comment != "" && !commentRE.MatchString(in.Comment) {
		return nil, probErr(problem.New(problem.InternalError, "the add-adlist input has a url or comment outside "+
			"its pattern; nothing was changed"))
	}
	got, created, err := am.AddAdlist(ctx, in.URL, in.Comment, c.Out)
	if err != nil {
		return nil, err
	}
	res := spec.AdlistResult{
		ActionResult: apitypes.ActionResult{Changed: created},
		Created:      created,
		// Finalize, in nexusd, names it with the provider's host.
		Adlist: apitypes.NewResource("", spec.Name, spec.AdlistResourceType, strconv.FormatInt(got.PiholeID, 10), got),
	}
	if in.UpdateGravity {
		done := fmt.Sprintf("adlist %d was added", got.PiholeID)
		if !created {
			done = fmt.Sprintf("adlist %d was already listed", got.PiholeID)
		}
		g, err := gravityAfter(ctx, am, c, done)
		if err != nil {
			return nil, err
		}
		res.Gravity, res.Changed, res.Warnings = &g, true, g.Warnings
	}
	return res, nil
}

// runRemoveAdlist removes the adlist named by id, or by url, which it
// resolves to an id from gravity.db first.
func runRemoveAdlist(ctx context.Context, c *helperCall) (any, error) {
	am, err := adlistMutator(c)
	if err != nil {
		return nil, err
	}
	var in spec.AdlistRemoveInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the remove-adlist input: %v; nothing was changed", err)))
	}
	id := in.ID
	if id == 0 {
		rows, err := c.Reader.Adlists(ctx)
		if err != nil {
			var pe *problem.Error
			if errors.As(err, &pe) {
				return nil, err
			}
			return nil, probErr(problem.New(problem.DependencyUnavailable,
				fmt.Sprintf("gravity.db could not be read to find the adlist: %v; nothing was changed", err)).
				With("provider", spec.Name))
		}
		r, ok := findAdlist(rows, in)
		if !ok {
			return nil, probErr(notFound("no pi-hole adlist matches; nothing was changed"))
		}
		id = r.PiholeID
		fmt.Fprintf(c.Out, "pihole: the adlist is id %d\n", id)
	}
	got, err := am.RemoveAdlist(ctx, id, c.Out)
	if err != nil {
		return nil, err
	}
	res := spec.AdlistRemoveResult{ActionResult: apitypes.ActionResult{Changed: true}, Removed: got}
	if in.UpdateGravity {
		g, err := gravityAfter(ctx, am, c, fmt.Sprintf("adlist %d was removed", id))
		if err != nil {
			return nil, err
		}
		res.Gravity, res.Warnings = &g, g.Warnings
	}
	return res, nil
}

// runUpdateGravity runs gravity.
func runUpdateGravity(ctx context.Context, c *helperCall) (any, error) {
	am, err := adlistMutator(c)
	if err != nil {
		return nil, err
	}
	return am.UpdateGravity(ctx, c.Out)
}

// gravityAfter runs gravity after an adlist change. Its failure's detail
// adds done, what the change already did.
func gravityAfter(ctx context.Context, am backend.AdlistMutator, c *helperCall, done string) (spec.GravityResult, error) {
	g, err := am.UpdateGravity(ctx, c.Out)
	var pe *problem.Error
	if errors.As(err, &pe) {
		pe.Detail += "; " + done + ", but gravity was not updated"
	}
	return g, err
}

func adlistMutator(c *helperCall) (backend.AdlistMutator, error) {
	am, ok := c.Mutator.(backend.AdlistMutator)
	if !ok {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot change adlists; nothing was changed", c.In.Backend)))
	}
	return am, nil
}
