// Package def declares operations once, for every surface: REST routing,
// OpenAPI, MCP tools, the CLI and authorization are all derived from an Op
// (FR-3). It has no runtime or provider dependencies, so the CLI can import
// every declaration without linking daemon code (CON-5).
package def

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
)

// Risk classifies an operation's effect on the host.
type Risk string

const (
	RiskReadOnly    Risk = "read-only"
	RiskMutating    Risk = "mutating"
	RiskDestructive Risk = "destructive"
)

// Mode is how an operation answers.
type Mode string

const (
	ModeSync        Mode = "sync"
	ModeLongRunning Mode = "long-running" // 202 plus an operation
	ModeStream      Mode = "stream"       // NDJSON apitypes.Frame values
)

// Kind places an operation in the resource model and so decides its route.
type Kind string

const (
	KindList           Kind = "list"            // GET /providers/{p}/{type}
	KindGet            Kind = "get"             // GET /providers/{p}/{type}/{name}
	KindSubresource    Kind = "subresource"     // GET /providers/{p}/{type}/{name}/{action}
	KindResourceAction Kind = "resource-action" // POST /providers/{p}/{type}/{name}/{action}
	KindProviderAction Kind = "provider-action" // POST /providers/{p}/actions/{action}
)

// Cancel is what POST /operations/{id}/cancel may do to an operation.
type Cancel string

const (
	CancelQueuedOnly Cancel = "queued-only" // The default
	CancelRunning    Cancel = "running"
)

// SystemProvider is the only provider that may declare Path, OperationControl
// or Scope.FromTarget.
const SystemProvider = "system"

// Op is a single operation declaration. Build it with Define.
type Op struct {
	ID               string // Stable OpenAPI operationId, e.g. "docker.containers.restart"
	Provider         string
	Kind             Kind
	ResourceType     string // "containers"; route segment. Empty for KindProviderAction
	Action           string // Route segment for actions and subresources
	Path             string // Explicit route under /v1, e.g. "/operations/{id}/cancel". Provider "system" only
	OperationControl bool   // Acts on an existing operation; may be sync and mutating. Provider "system" only
	Summary          string
	Description      string // Written for an agent: side effects, long-running or not
	Scope            Scope
	Risk             Risk
	Mode             Mode
	Idempotent       bool        // MCP idempotentHint
	WhatIf           bool        // Input has whatIf; CLI exposes --what-if
	Cancel           Cancel      // Empty means CancelQueuedOnly
	Filters          []Filter    // Documented server-side filter keys
	Sensitive        []string    // Input JSON paths redacted from audit and never persisted
	Untrusted        bool        // Output carries host-originated text (FR-28)
	CLI              CLIBinding  // Noun, Verb (must be approved), Positional
	MCP              *MCPBinding // Nil means not exposed as a tool
	// Refine adds what jsonschema.For cannot derive from Go types: enum,
	// pattern, maxLength. It runs once, on the input schema only.
	Refine func(*jsonschema.Schema)

	inputType  reflect.Type // Set by Define
	outputType reflect.Type // Set by Define
}

// Scope is the authorization an operation requires.
type Scope struct {
	Name       string // "containers:write"
	PerTarget  string // Optional alternative, "scripts:run:{name}"
	FromTarget bool   // Scope comes from the operation named by the target
}

// Filter documents one server-side filter key of a list operation.
type Filter struct {
	Key         string
	Description string
}

// CLIBinding names the generated `nexus <noun> <verb>` command.
type CLIBinding struct {
	Noun       string // Singular, e.g. "container"
	Verb       string // One of ApprovedVerbs
	Positional string // Input field bound to the positional argument, if any
}

// MCPBinding exposes an operation as an MCP tool.
type MCPBinding struct {
	Tool        string // e.g. "restart_container"
	Description string // Concise; counts against the tool budget (NFR-8)
}

// ProviderSpec is a provider's declarations: what the catalog lists.
type ProviderSpec struct {
	Name      string
	ReadScope string // Scope for reading the provider's operations (FR-22)
	Ops       []Op
}

// Typed is an Op bound to its input and output types, so handler signatures
// are checked at compile time when it is bound to an implementation.
type Typed[In, Out any] struct {
	op Op
}

// Op returns the declaration.
func (t Typed[In, Out]) Op() Op { return t.op }

// InputType is the Go type of the operation's input. Nil unless built by Define.
func (o Op) InputType() reflect.Type { return o.inputType }

// OutputType is the Go type of the operation's output. Nil unless built by Define.
func (o Op) OutputType() reflect.Type { return o.outputType }

// Mutating reports whether the operation changes host state.
func (o Op) Mutating() bool { return o.Risk != RiskReadOnly }

var (
	definedMu sync.Mutex
	defined   = map[string]bool{}
)

var frameType = reflect.TypeFor[apitypes.Frame]()

// Define validates a declaration and binds it to In and Out. It is meant for
// package-level variables and panics on any rule violation, so a bad
// declaration stops the binary at init rather than misbehaving at run time.
func Define[In, Out any](op Op) Typed[In, Out] {
	op.inputType = reflect.TypeFor[In]()
	op.outputType = reflect.TypeFor[Out]()
	if op.Cancel == "" {
		op.Cancel = CancelQueuedOnly
	}
	if err := validate(op); err != nil {
		panic(fmt.Sprintf("def: op %q: %v", op.ID, err))
	}

	definedMu.Lock()
	defer definedMu.Unlock()
	if defined[op.ID] {
		panic(fmt.Sprintf("def: op %q: duplicate ID", op.ID))
	}
	defined[op.ID] = true
	return Typed[In, Out]{op: op}
}

func validate(op Op) error {
	if op.ID == "" {
		return fmt.Errorf("empty ID")
	}
	if !IsApprovedVerb(op.CLI.Verb) {
		return fmt.Errorf("unapproved CLI verb %q: use one of %v", op.CLI.Verb, approvedVerbs)
	}
	if strings.TrimSpace(op.Description) == "" {
		return fmt.Errorf("missing Description")
	}
	switch op.Risk {
	case RiskReadOnly, RiskMutating, RiskDestructive:
	default:
		return fmt.Errorf("unknown Risk %q", op.Risk)
	}
	switch op.Mode {
	case ModeSync, ModeLongRunning, ModeStream:
	default:
		return fmt.Errorf("unknown Mode %q", op.Mode)
	}

	system := op.Provider == SystemProvider
	if op.OperationControl && !system {
		return fmt.Errorf("OperationControl is allowed only for provider %q", SystemProvider)
	}
	if op.Path != "" && !system {
		return fmt.Errorf("Path is allowed only for provider %q", SystemProvider)
	}
	if op.Scope.FromTarget && !system {
		return fmt.Errorf("Scope.FromTarget is allowed only for provider %q", SystemProvider)
	}

	if op.Mode == ModeStream && op.outputType != frameType {
		return fmt.Errorf("ModeStream requires output type apitypes.Frame, got %v", op.outputType)
	}

	if op.Mutating() {
		if op.OperationControl {
			// The single exception (D-2): an op acting on an existing record
			// may answer synchronously, and may take its scope from the target.
			if op.Scope.Name == "" && !op.Scope.FromTarget {
				return fmt.Errorf("OperationControl op with empty Scope.Name must set Scope.FromTarget")
			}
		} else {
			if op.Mode != ModeLongRunning {
				return fmt.Errorf("mutating op must be ModeLongRunning, got %q", op.Mode)
			}
			if op.Scope.Name == "" {
				return fmt.Errorf("mutating op has empty Scope.Name")
			}
		}
	}
	return nil
}
