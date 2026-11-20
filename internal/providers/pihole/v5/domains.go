// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// AddDomain adds an allow or deny entry by writing gravity.db, never through
// list.sh, which splices the value into SQL unescaped and silently moves an
// entry between lists (V-14). After any external gravity run ends, in one
// BEGIN IMMEDIATE transaction it looks the value up across every type:
//   - on the opposite list with the same kind it rolls back with 409
//     conflict naming that entry (ASM-8): an entry is never moved;
//   - on the same list with the same kind it returns that row, created
//     false, and reloads nothing;
//   - otherwise it inserts the row with bound parameters (the schema's
//     trigger links it to the default group, A-15) and commits.
//
// Before it opens gravity.db it waits for a list reload in flight to end
// and repairs a stuck FTL (settleLists). A new row is followed by `pihole
// restartdns reload-lists` and a wait until FTL has reloaded its lists
// (reloadListsAndWait), then read back.
func (m *Mutator) AddDomain(ctx context.Context, in spec.DomainAddInput, out io.Writer) (spec.DomainProperties, bool, error) {
	var none spec.DomainProperties
	typ, err := gravitydb.TypeOf(in.List, in.Kind)
	if err != nil {
		return none, false, problem.New(problem.InternalError, err.Error()+"; nothing was changed")
	}
	if err := m.WaitExternalGravity(ctx, out); err != nil {
		return none, false, err
	}
	if err := m.settleLists(ctx, out); err != nil {
		return none, false, err
	}
	db, err := gravitydb.OpenReadWrite(ctx, m.Paths.GravityDBPath())
	if err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	defer db.Close()
	tx, err := db.BeginImmediate(ctx)
	if err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	// A no-op after Commit; otherwise the rollback that ends every refusal.
	defer rollback(tx, out)
	rows, err := tx.FindDomainAllTypes(ctx, in.Value)
	if err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	for _, r := range rows {
		if r.Kind != in.Kind {
			continue
		}
		if r.List != in.List {
			return none, false, crossList(r)
		}
		if err := tx.Commit(ctx); err != nil {
			return none, false, gravityFailed(err, "nothing was changed")
		}
		fmt.Fprintf(out, "pihole: %s %s entry %d already exists; nothing to add\n", r.List, r.Kind, r.PiholeID)
		return r, false, nil
	}
	id, err := tx.InsertDomain(ctx, typ, in.Value, in.Comment)
	if err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	if err := tx.Commit(ctx); err != nil {
		return none, false, gravityFailed(err, "nothing was changed")
	}
	fmt.Fprintf(out, "pihole: added %s %s entry %d to gravity.db\n", in.List, in.Kind, id)
	done := fmt.Sprintf("the entry was added as id %d", id)
	if err := m.reloadListsAndWait(ctx, out, done); err != nil {
		return none, false, err
	}
	got, err := domainByID(ctx, db, id)
	if err != nil {
		return none, false, gravityFailed(err, done+" and lists were reloaded, but it could not be read back")
	}
	return got, true, nil
}

// RemoveDomain deletes the entry with id from gravity.db in one BEGIN
// IMMEDIATE transaction, after any external gravity run ends and FTL's lists
// are settled (settleLists), then runs `pihole restartdns reload-lists` and
// waits until FTL has reloaded them, and returns the removed row. An id
// with no row is 404 not-found, with nothing reloaded.
func (m *Mutator) RemoveDomain(ctx context.Context, id int64, out io.Writer) (spec.DomainProperties, error) {
	var none spec.DomainProperties
	if err := m.WaitExternalGravity(ctx, out); err != nil {
		return none, err
	}
	if err := m.settleLists(ctx, out); err != nil {
		return none, err
	}
	db, err := gravitydb.OpenReadWrite(ctx, m.Paths.GravityDBPath())
	if err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	defer db.Close()
	// The row's value is looked up first, because the transaction finds rows
	// by value; inside it the row must still be there with that id.
	cur, err := domainByID(ctx, db, id)
	if err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	tx, err := db.BeginImmediate(ctx)
	if err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	defer rollback(tx, out)
	rows, err := tx.FindDomainAllTypes(ctx, cur.Value)
	if err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	found := false
	for _, r := range rows {
		if r.PiholeID == id {
			cur, found = r, true
			break
		}
	}
	if !found {
		return none, gravityFailed(fmt.Errorf("%w: domainlist id %d", gravitydb.ErrNotFound, id), "nothing was changed")
	}
	if err := tx.DeleteDomain(ctx, id); err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	if err := tx.Commit(ctx); err != nil {
		return none, gravityFailed(err, "nothing was changed")
	}
	fmt.Fprintf(out, "pihole: removed %s %s entry %d from gravity.db\n", cur.List, cur.Kind, id)
	if err := m.reloadListsAndWait(ctx, out, fmt.Sprintf("the entry with id %d was removed", id)); err != nil {
		return none, err
	}
	return cur, nil
}

// rollback ends tx when it was not committed. Its error cannot change the
// outcome, since nothing was committed, so it is only logged.
func rollback(tx *gravitydb.Tx, out io.Writer) {
	if err := tx.Rollback(); err != nil {
		fmt.Fprintf(out, "pihole: gravity.db rollback: %v\n", err)
	}
}

// domainByID is the domainlist row with id, or gravitydb.ErrNotFound.
func domainByID(ctx context.Context, db *gravitydb.DB, id int64) (spec.DomainProperties, error) {
	rows, err := db.Domains(ctx)
	if err != nil {
		return spec.DomainProperties{}, err
	}
	for _, r := range rows {
		if r.PiholeID == id {
			return r, nil
		}
	}
	return spec.DomainProperties{}, fmt.Errorf("%w: domainlist id %d", gravitydb.ErrNotFound, id)
}

// crossList is the 409 conflict of an add whose value and kind are on the
// opposite list: list.sh would move the entry; Nexus refuses (ASM-8).
func crossList(r spec.DomainProperties) error {
	return problem.New(problem.Conflict, fmt.Sprintf("the value is already on the %s list as %s entry %d; "+
		"an entry is never moved between lists: remove entry %d first; nothing was changed",
		r.List, r.Kind, r.PiholeID, r.PiholeID))
}

// gravityFailed maps a gravity.db error to the operation's problem, with
// state saying what was changed: a missing row is not-found, a UNIQUE
// conflict is conflict, and a missing, locked or unusable database is
// dependency-unavailable. Anything else is returned wrapped, an internal
// error.
func gravityFailed(err error, state string) error {
	switch {
	case errors.Is(err, gravitydb.ErrNotFound):
		return problem.New(problem.NotFound, "no pi-hole domain entry has that ID; "+state)
	case errors.Is(err, gravitydb.ErrConflict):
		return problem.New(problem.Conflict, "the entry already exists in gravity.db; "+state)
	case errors.Is(err, gravitydb.ErrUnavailable):
		return problem.New(problem.DependencyUnavailable,
			"gravity.db is unavailable: missing, unreadable, locked or being replaced; "+state).With("provider", spec.Name)
	case errors.Is(err, gravitydb.ErrInvalid):
		return problem.New(problem.DependencyUnavailable, "gravity.db is not a usable Pi-hole database; "+state).
			With("provider", spec.Name)
	}
	return fmt.Errorf("pihole: gravity.db: %v; %s", err, state)
}
