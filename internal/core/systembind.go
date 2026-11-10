package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

// systemBindings binds the system.* operations. The operation ops are bound
// when the registry has an operation manager.
func (r *Registry) systemBindings() []provider.Binding {
	b := []provider.Binding{
		provider.Sync(systemspec.CapabilitiesGet,
			func(ctx context.Context, in systemspec.CapabilitiesGetInput) (apitypes.Capabilities, error) {
				return r.Capabilities(ctx, in.Refresh), nil
			}),
		provider.Sync(systemspec.AuditList, r.auditList),
	}
	if r.ops != nil {
		b = append(b,
			provider.Sync(systemspec.OperationsList, r.operationsList),
			// get and logs are FromTarget: the Dispatcher has loaded the
			// record and checked its provider's read scope (FR-22).
			provider.Sync(systemspec.OperationsGet, r.ops.GetOperation),
			provider.Stream(systemspec.OperationsLogs, r.ops.Logs),
			// cancel is FromTarget too, against the record's StartScope, and
			// the Dispatcher has written its request record before this runs.
			provider.Sync(systemspec.OperationsCancel,
				func(ctx context.Context, in systemspec.OperationInput) (apitypes.Operation, error) {
					return r.ops.Cancel(ctx, in.ID, r.cancelPolicy)
				}),
		)
	}
	return b
}

// operationsList is system.operations.list: only operations of providers
// whose read scope the caller holds (A-50). A caller with none gets an
// empty list.
func (r *Registry) operationsList(ctx context.Context, in systemspec.ListInput) (apitypes.List[apitypes.Operation], error) {
	caller, _ := auth.PrincipalFrom(ctx)
	readable := []string{}
	for _, p := range r.providers {
		s := p.Spec()
		if s.ReadScope != "" && caller.Has(s.ReadScope) {
			readable = append(readable, s.Name)
		}
	}
	return r.ops.List(ctx, in, readable)
}

// cancelPolicy is the declared Cancel of the op an operation runs.
func (r *Registry) cancelPolicy(action string) def.Cancel {
	for _, e := range r.entries {
		if op := e.Binding.Op(); op.ID == action {
			return op.Cancel
		}
	}
	return def.CancelQueuedOnly
}

// auditList is system.audit.list: audit records newest first, across the
// live and rotated files, with the declared filters.
func (r *Registry) auditList(ctx context.Context, in systemspec.ListInput) (apitypes.List[systemspec.AuditRecord], error) {
	var none apitypes.List[systemspec.AuditRecord]
	if r.audit == nil {
		return none, problem.New(problem.AuditUnavailable, "the audit log is not available")
	}
	q, perr := auditQuery(in)
	if perr != nil {
		return none, perr
	}
	page, err := r.audit.QueryContext(ctx, q)
	if errors.Is(err, audit.ErrBadCursor) {
		return none, invalidInput(problem.FieldError{Path: "/cursor", Message: "is not a cursor this server issued"})
	}
	if err != nil {
		return none, fmt.Errorf("audit query: %w", err)
	}
	return page, nil
}

// auditQuery reads the paging input and the key=value filters, whose keys
// the schema already restricts to the declared ones. A repeated key keeps
// its last value.
func auditQuery(in systemspec.ListInput) (audit.Query, *problem.Error) {
	q := audit.Query{Limit: in.Limit, Cursor: in.Cursor}
	for i, f := range in.Filter {
		key, val, _ := strings.Cut(f, "=")
		switch key {
		case "after", "before":
			t, err := time.Parse(time.RFC3339Nano, val)
			if err != nil {
				return q, invalidInput(problem.FieldError{Path: fmt.Sprintf("/filter/%d", i),
					Message: key + " must be an RFC 3339 time"})
			}
			if key == "after" {
				q.After = t
			} else {
				q.Before = t
			}
		case "tokenName":
			q.TokenName = val
		case "provider":
			q.Provider = val
		case "action":
			q.Action = val
		case "outcome":
			q.Outcome = val
		default:
			return q, invalidInput(problem.FieldError{Path: fmt.Sprintf("/filter/%d", i), Message: "unknown filter key"})
		}
	}
	return q, nil
}
