// Package systemspec declares the system.* operations and their wire types.
// Like a provider's spec package it holds only data and types, so the CLI can
// import it through internal/catalog without linking daemon code (CON-5,
// G-16). The bindings live in internal/core/systembind.go.
package systemspec

import (
	"encoding/json"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
)

// Name is the provider name of every system operation.
const Name = def.SystemProvider

// List bounds shared by the list operations.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// operationIDPattern matches the lowercase canonical UUIDv7 that opid issues.
const operationIDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`

// cursorPattern matches the opaque cursors the server hands out.
const cursorPattern = `^[A-Za-z0-9_-]*$`

const maxCursorLength = 512

// CapabilitiesGetInput is the input of system.capabilities.get.
type CapabilitiesGetInput struct {
	Refresh bool `json:"refresh,omitempty" jsonschema:"Re-run every enabled provider's preflight first."`
}

// ListInput is the input of the paged list operations.
type ListInput struct {
	Limit  int      `json:"limit,omitempty" jsonschema:"Page size: default 100, maximum 1000."`
	Cursor string   `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous page's nextCursor."`
	Filter []string `json:"filter,omitempty"` // Repeatable key=value; declared keys only
}

// OperationInput names one operation.
type OperationInput struct {
	ID string `json:"id" jsonschema:"Operation ID."`
}

// OperationLogsInput is the input of system.operations.logs.
type OperationLogsInput struct {
	ID     string `json:"id" jsonschema:"Operation ID."`
	Offset int64  `json:"offset,omitempty" jsonschema:"Absolute byte offset to start from."`
	Follow bool   `json:"follow,omitempty" jsonschema:"Keep streaming until the operation ends."`
}

// AuditRecord is one audit log entry as the API returns it (FR-24, FR-30),
// and one line of audit.jsonl. Parameters are already redacted when the
// record is written.
type AuditRecord struct {
	ID        string          `json:"id"`
	Host      string          `json:"host"`
	Time      time.Time       `json:"time"`
	TokenName string          `json:"tokenName"`
	TokenID   string          `json:"tokenId"`
	Source    string          `json:"source"`  // TCP peer address; an IPv6 /64 for an aggregate
	Surface   string          `json:"surface"` // "rest" or "mcp"
	Provider  string          `json:"provider"`
	Action    string          `json:"action"` // Op ID
	Target    string          `json:"target"`
	Params    json.RawMessage `json:"params,omitempty"`
	// Outcome is requested, denied, refused, replayed, stored (an upload
	// whose body was stored), completed (a SyncMutation or Audited call, or
	// a long-running call that ended at admission, that succeeded), or an
	// operation state for a state change.
	Outcome string `json:"outcome"`
	Problem string `json:"problem,omitempty"` // Problem slug of a refusal or denial
	// InvalidFields are the failing input paths of a validation refusal.
	// Values are never recorded: unvalidated input cannot be redacted.
	InvalidFields []string `json:"invalidFields,omitempty"`
	OperationID   string   `json:"operationId,omitempty"`
	// RequestID is the id of the requested record this record answers: on
	// the replayed, refused, completed or stored record of a request that
	// had already written one, and on the Queued record of the operation
	// that request created.
	// So no requested record stands alone (FR-24, Data Flow step 10).
	RequestID string `json:"requestId,omitempty"`
	Count     int    `json:"count,omitempty"` // Denials one aggregate stands for under rate limiting (A-47)
	// Facts are what provider code reported about the request through
	// audit.AddFact, such as an upload's sha256: never secrets or key
	// material (FR-16). Bounded in count and size.
	Facts map[string]string `json:"facts,omitempty"`
}

func refineList(filterKeys ...string) func(*jsonschema.Schema) {
	return func(s *jsonschema.Schema) {
		lo, hi := 1.0, float64(MaxLimit)
		if p := s.Properties["limit"]; p != nil {
			p.Minimum, p.Maximum = &lo, &hi
		}
		if p := s.Properties["cursor"]; p != nil {
			n := maxCursorLength
			p.Pattern, p.MaxLength = cursorPattern, &n
		}
		if p := s.Properties["filter"]; p != nil && p.Items != nil {
			keys := ""
			for i, k := range filterKeys {
				if i > 0 {
					keys += "|"
				}
				keys += k
			}
			n := 256
			p.Items.Pattern = `^(` + keys + `)=[^\x00-\x1f]*$`
			p.Items.MaxLength = &n
		}
	}
}

func refineOperationID(s *jsonschema.Schema) {
	if p := s.Properties["id"]; p != nil {
		p.Pattern = operationIDPattern
	}
}

func refineLogs(s *jsonschema.Schema) {
	refineOperationID(s)
	zero := 0.0
	if p := s.Properties["offset"]; p != nil {
		p.Minimum = &zero
	}
}

var (
	operationFilters = []def.Filter{
		{Key: "status", Description: "Operation status: Queued, Running, Succeeded, Failed or Canceled."},
		{Key: "provider", Description: "Provider that owns the operation."},
		{Key: "createdAfter", Description: "Only operations created after this RFC 3339 time."},
		{Key: "createdBefore", Description: "Only operations created before this RFC 3339 time."},
	}
	auditFilters = []def.Filter{
		{Key: "after", Description: "Only records after this RFC 3339 time."},
		{Key: "before", Description: "Only records before this RFC 3339 time."},
		{Key: "tokenName", Description: "Name of the token that made the request."},
		{Key: "provider", Description: "Provider of the requested action."},
		{Key: "action", Description: "Operation ID of the requested action."},
		{Key: "outcome", Description: "Record outcome, such as requested, denied, refused, replayed, stored or completed."},
	}
)

func filterKeys(fs []def.Filter) []string {
	keys := make([]string, len(fs))
	for i, f := range fs {
		keys[i] = f.Key
	}
	return keys
}

// operationColumns are the table columns of an operation, which every
// long-running op answers with.
var operationColumns = []string{"id", "action", "status", "created", "ended"}

// The system operations, in the order of the Operation catalog table.
var (
	CapabilitiesGet = def.Define[CapabilitiesGetInput, apitypes.Capabilities](def.Op{
		ID: "system.capabilities.get", Provider: Name, Kind: def.KindGet, ResourceType: "capabilities",
		Path:    "/capabilities",
		Summary: "Describe this host's providers and their availability",
		Description: "Returns the host identity, API and build version, and every compiled-in provider " +
			"with its status (enabled, disabled by configuration, or unavailable after a failed preflight), " +
			"the reason when not enabled, and its resource types and actions. Read-only. " +
			"With refresh=true every enabled provider's preflight is re-run first.",
		Scope: def.Scope{}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:     def.CLIBinding{Noun: "capability", Verb: "get"},
		MCP:     &def.MCPBinding{Tool: "get_capabilities", Description: "List this host's providers and whether each is usable."},
		Columns: []string{"version", "apiVersion", "providers.name", "providers.status"},
	})

	OperationsList = def.Define[ListInput, apitypes.List[apitypes.Operation]](def.Op{
		ID: "system.operations.list", Provider: Name, Kind: def.KindList, ResourceType: "operations",
		Path:    "/operations",
		Summary: "List operations",
		Description: "Lists operations newest first, limited to providers whose read scope the caller holds. " +
			"Read-only. Paged with limit and cursor; filter by status, provider, createdAfter and createdBefore.",
		Scope: def.Scope{}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Filters: operationFilters,
		CLI:     def.CLIBinding{Noun: "operation", Verb: "get"},
		MCP:     &def.MCPBinding{Tool: "list_operations", Description: "List recent operations and their status."},
		Refine:  refineList(filterKeys(operationFilters)...),
		Columns: operationColumns,
	})

	OperationsGet = def.Define[OperationInput, apitypes.Operation](def.Op{
		ID: "system.operations.get", Provider: Name, Kind: def.KindGet, ResourceType: "operations",
		Path:    "/operations/{id}",
		Summary: "Get one operation",
		Description: "Returns one operation with its status, timestamps, result and structured error. " +
			"Read-only. Requires the read scope of the operation's provider.",
		Scope: def.Scope{FromTarget: true}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:     def.CLIBinding{Noun: "operation", Verb: "get", Positional: "id"},
		MCP:     &def.MCPBinding{Tool: "get_operation", Description: "Get an operation's status, result and error."},
		Refine:  refineOperationID,
		Columns: operationColumns,
	})

	OperationsLogs = def.Define[OperationLogsInput, apitypes.Frame](def.Op{
		ID: "system.operations.logs", Provider: Name, Kind: def.KindSubresource, ResourceType: "operations",
		Action:  "logs",
		Path:    "/operations/{id}/logs",
		Summary: "Read an operation's output",
		Description: "Streams an operation's captured output as NDJSON frames from a byte offset, " +
			"optionally following until the operation ends. Read-only. The output is host-originated text. " +
			"Requires the read scope of the operation's provider.",
		Scope: def.Scope{FromTarget: true}, Risk: def.RiskReadOnly, Mode: def.ModeStream, Idempotent: true,
		Untrusted: true,
		CLI:       def.CLIBinding{Noun: "operationlog", Verb: "get", Positional: "id"},
		MCP:       &def.MCPBinding{Tool: "get_operation_logs", Description: "Read a page of an operation's output."},
		Refine:    refineLogs,
	})

	OperationsCancel = def.Define[OperationInput, apitypes.Operation](def.Op{
		ID: "system.operations.cancel", Provider: Name, Kind: def.KindResourceAction, ResourceType: "operations",
		Action:           "cancel",
		Path:             "/operations/{id}/cancel",
		OperationControl: true,
		Summary:          "Cancel an operation",
		Description: "Cancels a queued operation, or a running one whose action allows it, and returns the " +
			"operation. Side effects: a queued operation becomes Canceled without running; a running one is " +
			"stopped and ends Canceled, possibly having made part of its changes. Starts no new work. " +
			"Repeating it on a canceled operation changes nothing. Answers at once; not long-running. " +
			"Requires the scope that started the operation.",
		Scope: def.Scope{FromTarget: true}, Risk: def.RiskMutating, Mode: def.ModeSync, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "operation", Verb: "cancel", Positional: "id"},
		MCP:    &def.MCPBinding{Tool: "cancel_operation", Description: "Cancel a queued or cancelable running operation."},
		Refine: refineOperationID,
	})

	AuditList = def.Define[ListInput, apitypes.List[AuditRecord]](def.Op{
		ID: "system.audit.list", Provider: Name, Kind: def.KindList, ResourceType: "audit",
		Path:    "/audit",
		Summary: "Query the audit log",
		Description: "Lists audit records newest first. Read-only. Paged with limit and cursor; filter by " +
			"after, before, tokenName, provider, action and outcome.",
		Scope: def.Scope{Name: "audit:read"}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Filters: auditFilters,
		CLI:     def.CLIBinding{Noun: "auditrecord", Verb: "get"},
		Refine:  refineList(filterKeys(auditFilters)...),
		Columns: []string{"time", "tokenName", "action", "target", "outcome"},
	})
)

// Spec returns the system declarations for the catalog.
func Spec() def.ProviderSpec {
	return def.ProviderSpec{
		Name: Name,
		Ops: []def.Op{
			CapabilitiesGet.Op(),
			OperationsList.Op(),
			OperationsGet.Op(),
			OperationsLogs.Op(),
			OperationsCancel.Op(),
			AuditList.Op(),
		},
	}
}
