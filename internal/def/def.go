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
	"time"

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
	KindUpload         Kind = "upload"          // POST /providers/{p}/{type}; raw body; input from the query
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
//
// A mutating op is ModeLongRunning: it answers 202 with an operation. There
// are three exceptions, which answer synchronously:
//   - OperationControl (provider "system" only) acts on an existing operation;
//   - KindUpload stages bytes for a later long-running op;
//   - SyncMutation answers 200 with no operation record, lane or replay.
//
// CompletesAtAdmission is not an exception: such an op stays long-running and
// may only end a call at admission with 200 and its output.
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
	Upload           *Upload     // KindUpload only: accepted content types and size ceiling
	// Refine adds what jsonschema.For cannot derive from Go types: enum,
	// pattern, maxLength. It runs once, on the input schema only.
	Refine func(*jsonschema.Schema)
	// Columns are the CLI's table columns: dot paths into one output object
	// (one item of a list), in order; the host column always comes first.
	// Empty means those of another op with the same output, else derived.
	Columns []string

	// SyncMutation lets a mutating op answer 200 synchronously, with no
	// operation record, lane or idempotency replay (CON-3, the third
	// exception after OperationControl and KindUpload). Its output is never
	// persisted: it reaches only the response body.
	SyncMutation bool
	// SecretOutput is the JSON name of a string field of the output that
	// carries a credential returned once. Requires SyncMutation.
	SecretOutput string
	// Audited makes a read-only sync or stream op's calls audited like a
	// mutation's: requested before the handler (fail-closed), then completed
	// or refused.
	Audited bool
	// CompletesAtAdmission lets a long-running op end a call at admission
	// with 200 and its output: no operation record, lane hold, Prepare or
	// unit. Its binding must provide DetachedSpec.Admission (D-7).
	CompletesAtAdmission bool
	// CompletesAcrossReboot lets a detached op's outcome be decided in the
	// boot after the one it was launched in: its helper records a reboot
	// request with detach.RecordReboot before triggering a reboot, and its
	// binding's DetachedSpec.AfterReboot reports the outcome once nexusd
	// runs in a new boot. Requires ModeLongRunning, Risk mutating or
	// destructive, Kind resource-action or provider-action, CLI.ReconnectWindow
	// > 0, and neither SyncMutation nor CompletesAtAdmission.
	CompletesAcrossReboot bool

	inputType  reflect.Type // Set by Define
	outputType reflect.Type // Set by Define
}

// Scope is the authorization an operation requires.
type Scope struct {
	Name       string // "containers:write"
	PerTarget  string // Optional alternative, "scripts:run:{name}"
	FromTarget bool   // Scope comes from the operation named by the target
}

// Upload is what a KindUpload operation accepts as its raw request body.
type Upload struct {
	ContentTypes []string // e.g. application/octet-stream, application/gzip
	MaxBytes     int64    // Ceiling; the binding may lower it at run time
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
	// WaitedResultItems makes a waited long-running command print, once the
	// operation succeeded, each object of its result's items instead of the
	// operation; --no-wait and an operation that did not succeed still
	// print the operation. The output must have an Items field listing
	// resources, which carry host and id. It changes the CLI only.
	WaitedResultItems bool
	// Upload makes the command upload a file through a KindUpload op first,
	// then set Field from the upload's result before running this op.
	Upload *CLIUpload
	// ReconnectWindow is how long a waited command keeps retrying the
	// connection to the daemon while it waits, e.g. across a daemon restart.
	// Zero means no retrying. At most 10 minutes; long-running ops only.
	ReconnectWindow time.Duration
	// Variadic is the []string input field that trailing arguments bind to.
	// Empty keeps the "names" convention.
	Variadic string
	// EchoWarnings also prints the result's warnings ([]string "warnings",
	// apitypes.ActionResult) on stderr, after printing the result.
	EchoWarnings bool
	// TypedConfirmation is the JSON name of a required string input field
	// the operator must type. The CLI asks for it on /dev/tty when its flag
	// is absent, never fills it from another flag or --yes, takes an
	// explicit flag as the confirmation (no y/N question), and runs the
	// command against exactly one host. Requires a mutating op; the field
	// must not be Positional.
	TypedConfirmation string
}

// CLIUpload binds a long-running command to a KindUpload op of the same
// provider, which uploads a file before the command runs.
type CLIUpload struct {
	// Op is the ID of a KindUpload op of the same provider. core.New checks it
	// for bound ops only (the referenced op must also be bound); the catalog
	// test checks every spec op.
	Op        string
	Field     string // String input field set from the upload result's field of the same JSON name
	FileFlag  string // "file": required flag naming the file to upload
	FileUsage string
	Sidecars  []CLISidecar
}

// CLISidecar is a small file uploaded with the main one as an input field of
// the upload op, e.g. a detached signature.
type CLISidecar struct {
	Param    string // Upload op input field receiving the trimmed file content ("signature")
	Flag     string // "signature"
	Suffix   string // ".sig": the default path is <file><suffix>
	MaxBytes int    // 4096
	Usage    string
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

// ResultItemType is the type of one item of t's Items field, tagged items,
// when the items are resources: structs with string fields ID and Host,
// tagged id and host, as apitypes.Resource has.
func ResultItemType(t reflect.Type) (reflect.Type, bool) {
	if t == nil || t.Kind() != reflect.Struct {
		return nil, false
	}
	f, ok := t.FieldByName("Items")
	if !ok || f.Type.Kind() != reflect.Slice || jsonName(f) != "items" {
		return nil, false
	}
	item := f.Type.Elem()
	if item.Kind() != reflect.Struct {
		return nil, false
	}
	for name, tag := range map[string]string{"ID": "id", "Host": "host"} {
		g, ok := item.FieldByName(name)
		if !ok || g.Type.Kind() != reflect.String || jsonName(g) != tag {
			return nil, false
		}
	}
	return item, true
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// jsonOmits reports whether f's JSON tag has omitempty or omitzero, which
// make the field optional in the generated schema.
func jsonOmits(f reflect.StructField) bool {
	_, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
	for _, o := range strings.Split(opts, ",") {
		if o == "omitempty" || o == "omitzero" {
			return true
		}
	}
	return false
}

func validate(op Op) error {
	if op.ID == "" {
		return fmt.Errorf("empty ID")
	}
	// A KindUpload op has no command of its own: the CLI reaches it only
	// through another op's CLI.Upload, so it is the one kind without a verb.
	if op.Kind != KindUpload && !IsApprovedVerb(op.CLI.Verb) {
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

	if err := validateDeclarations(op); err != nil {
		return err
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

	if op.Kind == KindUpload {
		if err := validateUpload(op); err != nil {
			return err
		}
	} else if op.Upload != nil {
		return fmt.Errorf("Upload is allowed only for KindUpload, got Kind %q", op.Kind)
	}

	if op.Mode == ModeStream && op.outputType != frameType {
		return fmt.Errorf("ModeStream requires output type apitypes.Frame, got %v", op.outputType)
	}

	if op.CLI.Upload != nil {
		if err := validateCLIUpload(op); err != nil {
			return err
		}
	}
	if w := op.CLI.ReconnectWindow; w != 0 {
		if op.Mode != ModeLongRunning {
			return fmt.Errorf("CLI.ReconnectWindow requires ModeLongRunning, got %q", op.Mode)
		}
		if w < 0 || w > maxReconnectWindow {
			return fmt.Errorf("CLI.ReconnectWindow %v out of range: want 0 < w <= %v", w, maxReconnectWindow)
		}
	}

	if v := op.CLI.Variadic; v != "" {
		if !isStringSliceField(op.inputType, v) {
			return fmt.Errorf("CLI.Variadic %q is not a []string field of input %v", v, op.inputType)
		}
		if op.CLI.Positional != "" {
			return fmt.Errorf("CLI.Variadic %q cannot be set with CLI.Positional %q", v, op.CLI.Positional)
		}
	}
	if op.CLI.EchoWarnings && !hasWarnings(op.outputType) {
		return fmt.Errorf("CLI.EchoWarnings requires an output with a []string field warnings, got %v", op.outputType)
	}

	if op.CLI.WaitedResultItems {
		if op.Mode != ModeLongRunning {
			return fmt.Errorf("CLI.WaitedResultItems requires ModeLongRunning, got %q", op.Mode)
		}
		if _, ok := ResultItemType(op.outputType); !ok {
			return fmt.Errorf("CLI.WaitedResultItems requires an output with an Items field listing resources, got %v", op.outputType)
		}
	}

	if op.Mutating() {
		if op.OperationControl {
			// The first exception (D-2): an op acting on an existing record
			// may answer synchronously, and may take its scope from the target.
			if op.Scope.Name == "" && !op.Scope.FromTarget {
				return fmt.Errorf("OperationControl op with empty Scope.Name must set Scope.FromTarget")
			}
		} else if op.Kind != KindUpload && !op.SyncMutation {
			// KindUpload is the second exception: it stages bytes for a later
			// long-running op and is checked by validateUpload. SyncMutation
			// is the third, checked by validateDeclarations.
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

// validateDeclarations checks CompletesAcrossReboot, CompletesAtAdmission,
// SyncMutation, SecretOutput, Audited and CLI.TypedConfirmation. It runs
// before the other rules so that a refusal names the declaration whose rule
// failed.
func validateDeclarations(op Op) error {
	action := op.Kind == KindResourceAction || op.Kind == KindProviderAction
	if op.CompletesAcrossReboot {
		switch {
		case !op.Mutating():
			return fmt.Errorf("CompletesAcrossReboot requires a mutating op, got Risk %q", op.Risk)
		case op.Mode != ModeLongRunning:
			return fmt.Errorf("CompletesAcrossReboot requires ModeLongRunning, got %q", op.Mode)
		case !action:
			return fmt.Errorf("CompletesAcrossReboot requires KindResourceAction or KindProviderAction, got Kind %q", op.Kind)
		case op.CLI.ReconnectWindow <= 0:
			// The waiting CLI must outlast the reboot's outage.
			return fmt.Errorf("CompletesAcrossReboot requires CLI.ReconnectWindow > 0, got %v", op.CLI.ReconnectWindow)
		case op.SyncMutation:
			return fmt.Errorf("CompletesAcrossReboot cannot be set with SyncMutation")
		case op.CompletesAtAdmission:
			return fmt.Errorf("CompletesAcrossReboot cannot be set with CompletesAtAdmission")
		}
	}
	if op.CompletesAtAdmission {
		switch {
		case !op.Mutating():
			return fmt.Errorf("CompletesAtAdmission requires a mutating op, got Risk %q", op.Risk)
		case op.Mode != ModeLongRunning:
			return fmt.Errorf("CompletesAtAdmission requires ModeLongRunning, got %q", op.Mode)
		case !action:
			return fmt.Errorf("CompletesAtAdmission requires KindResourceAction or KindProviderAction, got Kind %q", op.Kind)
		case op.MCP != nil:
			// The MCP adapter maps long-running ops to operations only.
			return fmt.Errorf("CompletesAtAdmission must not set MCP")
		case op.SyncMutation:
			return fmt.Errorf("CompletesAtAdmission cannot be set with SyncMutation")
		case op.WhatIf:
			return fmt.Errorf("CompletesAtAdmission must not set WhatIf")
		case op.CLI.Upload != nil:
			return fmt.Errorf("CompletesAtAdmission must not set CLI.Upload")
		}
	}
	if op.SyncMutation {
		switch {
		case !op.Mutating():
			return fmt.Errorf("SyncMutation requires a mutating op, got Risk %q", op.Risk)
		case op.Mode != ModeSync:
			return fmt.Errorf("SyncMutation requires ModeSync, got %q", op.Mode)
		case !action:
			return fmt.Errorf("SyncMutation requires KindResourceAction or KindProviderAction, got Kind %q", op.Kind)
		case op.Provider == SystemProvider:
			return fmt.Errorf("SyncMutation is not allowed for provider %q", SystemProvider)
		case op.Scope.Name == "":
			return fmt.Errorf("SyncMutation requires Scope.Name")
		case op.MCP != nil:
			return fmt.Errorf("SyncMutation must not set MCP")
		case op.WhatIf:
			return fmt.Errorf("SyncMutation must not set WhatIf")
		case op.Idempotent:
			return fmt.Errorf("SyncMutation must not set Idempotent")
		case op.OperationControl:
			return fmt.Errorf("SyncMutation must not set OperationControl")
		case op.CLI.Upload != nil:
			return fmt.Errorf("SyncMutation must not set CLI.Upload")
		case op.CLI.ReconnectWindow != 0:
			return fmt.Errorf("SyncMutation must not set CLI.ReconnectWindow")
		case op.CLI.WaitedResultItems:
			return fmt.Errorf("SyncMutation must not set CLI.WaitedResultItems")
		}
	}
	if s := op.SecretOutput; s != "" {
		if !op.SyncMutation {
			return fmt.Errorf("SecretOutput %q requires SyncMutation", s)
		}
		if !isStringField(op.outputType, s) {
			return fmt.Errorf("SecretOutput %q is not a string field of output %v", s, op.outputType)
		}
	}
	if op.Audited {
		if op.Risk != RiskReadOnly {
			return fmt.Errorf("Audited requires RiskReadOnly, got %q", op.Risk)
		}
		if op.Mode != ModeSync && op.Mode != ModeStream {
			return fmt.Errorf("Audited requires ModeSync or ModeStream, got %q", op.Mode)
		}
	}
	if c := op.CLI.TypedConfirmation; c != "" {
		if !op.Mutating() {
			return fmt.Errorf("CLI.TypedConfirmation %q requires a mutating op, got Risk %q", c, op.Risk)
		}
		f, ok := jsonStructField(op.inputType, c)
		switch {
		case !ok:
			return fmt.Errorf("CLI.TypedConfirmation %q is an unknown field of input %v", c, op.inputType)
		case f.Type.Kind() != reflect.String:
			return fmt.Errorf("CLI.TypedConfirmation %q is not a string field of input %v", c, op.inputType)
		case jsonOmits(f):
			// The schema leaves an omitempty or omitzero field optional; the
			// confirmation must be required so the daemon refuses its absence.
			return fmt.Errorf("CLI.TypedConfirmation %q is not required: its field is omitempty or omitzero", c)
		case c == op.CLI.Positional:
			return fmt.Errorf("CLI.TypedConfirmation %q must not be CLI.Positional", c)
		}
	}
	return nil
}

const (
	maxReconnectWindow = 10 * time.Minute
	maxSidecarBytes    = 64 << 10
)

// validateUpload checks the KindUpload rules: a sync, mutating, scoped op
// with an upload declaration, no MCP tool and no command, outside system.
func validateUpload(op Op) error {
	if op.Mode != ModeSync {
		return fmt.Errorf("KindUpload requires ModeSync, got %q", op.Mode)
	}
	if op.Risk != RiskMutating {
		return fmt.Errorf("KindUpload requires RiskMutating, got %q", op.Risk)
	}
	if op.Scope.Name == "" {
		return fmt.Errorf("KindUpload requires Scope.Name")
	}
	if op.MCP != nil {
		return fmt.Errorf("KindUpload must not set MCP")
	}
	if op.Upload == nil {
		return fmt.Errorf("KindUpload requires Upload")
	}
	if len(op.Upload.ContentTypes) == 0 {
		return fmt.Errorf("KindUpload requires non-empty Upload.ContentTypes")
	}
	if op.Upload.MaxBytes <= 0 {
		return fmt.Errorf("KindUpload requires Upload.MaxBytes > 0, got %d", op.Upload.MaxBytes)
	}
	if op.CLI != (CLIBinding{}) {
		return fmt.Errorf("KindUpload requires an empty CLI binding, got %+v", op.CLI)
	}
	if op.Provider == SystemProvider {
		return fmt.Errorf("KindUpload is not allowed for provider %q", SystemProvider)
	}
	return nil
}

// validateCLIUpload checks a CLI.Upload binding against op's own declaration.
// That Op names a KindUpload op of the same provider is checked where every
// op is known (core.New and the catalog test), not here.
func validateCLIUpload(op Op) error {
	u := op.CLI.Upload
	if op.Mode != ModeLongRunning {
		return fmt.Errorf("CLI.Upload requires ModeLongRunning, got %q", op.Mode)
	}
	if u.Op == "" {
		return fmt.Errorf("CLI.Upload.Op is empty")
	}
	if !isStringField(op.inputType, u.Field) {
		return fmt.Errorf("CLI.Upload.Field %q is not a string field of input %v", u.Field, op.inputType)
	}
	if !isKebab(u.FileFlag) {
		return fmt.Errorf("CLI.Upload.FileFlag %q is not a kebab-case flag name", u.FileFlag)
	}
	for i, s := range u.Sidecars {
		switch {
		case s.Param == "":
			return fmt.Errorf("CLI.Upload.Sidecars[%d].Param is empty", i)
		case s.Flag == "":
			return fmt.Errorf("CLI.Upload.Sidecars[%d].Flag is empty", i)
		case s.Suffix == "":
			return fmt.Errorf("CLI.Upload.Sidecars[%d].Suffix is empty", i)
		case s.MaxBytes <= 0 || s.MaxBytes > maxSidecarBytes:
			return fmt.Errorf("CLI.Upload.Sidecars[%d].MaxBytes %d out of range: want 0 < n <= %d", i, s.MaxBytes, maxSidecarBytes)
		}
	}
	return nil
}

// isStringField reports whether struct type t has a string field whose JSON
// name is name.
func isStringField(t reflect.Type, name string) bool {
	f, ok := jsonField(t, name)
	return ok && f.Kind() == reflect.String
}

// isStringSliceField reports whether struct type t has a []string field whose
// JSON name is name.
func isStringSliceField(t reflect.Type, name string) bool {
	f, ok := jsonField(t, name)
	return ok && f.Kind() == reflect.Slice && f.Elem().Kind() == reflect.String
}

// hasWarnings reports whether struct type t has a []string field warnings,
// its own or promoted from an embedded apitypes.ActionResult.
func hasWarnings(t reflect.Type) bool {
	return isStringSliceField(t, "warnings")
}

// jsonField returns the type of the field that encoding/json encodes under
// name in struct type t.
func jsonField(t reflect.Type, name string) (reflect.Type, bool) {
	f, ok := jsonStructField(t, name)
	if !ok {
		return nil, false
	}
	return f.Type, true
}

// jsonStructField returns the field that encoding/json encodes under name in
// struct type t. It follows encoding/json: an embedded struct without a JSON
// name is flattened, "-" hides a field (and so all of an embedded struct's
// fields), the shallowest field wins and, at equal depth, the only tagged
// one; any other tie hides the name.
func jsonStructField(t reflect.Type, name string) (reflect.StructField, bool) {
	if t == nil || t.Kind() != reflect.Struct || name == "" {
		return reflect.StructField{}, false
	}
	type match struct {
		field  reflect.StructField
		tagged bool
	}
	seen := map[reflect.Type]bool{}
	for level := []reflect.Type{t}; len(level) > 0; {
		var next []reflect.Type
		var matches []match
		for _, st := range level {
			if seen[st] {
				continue
			}
			seen[st] = true
			for i := range st.NumField() {
				f := st.Field(i)
				tag := jsonName(f)
				if tag == "-" {
					continue
				}
				if f.Anonymous {
					ft := f.Type
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if tag == "" && ft.Kind() == reflect.Struct {
						next = append(next, ft)
						continue
					}
				}
				if !f.IsExported() {
					continue
				}
				json := tag
				if json == "" {
					json = f.Name
				}
				if json == name {
					matches = append(matches, match{f, tag != ""})
				}
			}
		}
		switch len(matches) {
		case 0:
			level = next
			continue
		case 1:
			return matches[0].field, true
		}
		var dominant []match
		for _, m := range matches {
			if m.tagged {
				dominant = append(dominant, m)
			}
		}
		if len(dominant) == 1 {
			return dominant[0].field, true
		}
		return reflect.StructField{}, false
	}
	return reflect.StructField{}, false
}

// isKebab reports whether s is a kebab-case flag name: lowercase words of
// letters and digits, starting with a letter, joined by single hyphens.
func isKebab(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' || s[len(s)-1] == '-' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && s[i-1] != '-':
		default:
			return false
		}
	}
	return true
}
