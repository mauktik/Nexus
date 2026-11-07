package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

// systemBindings binds the system.* operations implemented so far. The rest
// of internal/systemspec is bound as its machinery lands: the operation ops
// at 3.2.
func (r *Registry) systemBindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(systemspec.CapabilitiesGet,
			func(ctx context.Context, in systemspec.CapabilitiesGetInput) (apitypes.Capabilities, error) {
				return r.Capabilities(ctx, in.Refresh), nil
			}),
		provider.Sync(systemspec.AuditList, r.auditList),
	}
}

// auditList is system.audit.list: audit records newest first, across the
// live and rotated files, with the declared filters.
func (r *Registry) auditList(_ context.Context, in systemspec.ListInput) (apitypes.List[systemspec.AuditRecord], error) {
	var none apitypes.List[systemspec.AuditRecord]
	if r.audit == nil {
		return none, problem.New(problem.AuditUnavailable, "the audit log is not available")
	}
	q, perr := auditQuery(in)
	if perr != nil {
		return none, perr
	}
	page, err := r.audit.Query(q)
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
