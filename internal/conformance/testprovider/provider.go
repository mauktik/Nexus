// Package testprovider is a provider that exists only for the conformance
// suite (4.2). It is declared and bound exactly as a real provider is, through
// def.Define and the provider binding constructors, and is passed to core.New
// with no change to core or transport code: that is FR-3's acceptance test.
//
// Its handlers are spies. A spy records each call that reached its handler,
// which is only a call that passed authorization and validation, so the FR-25
// negative suite can show that a hostile value it accepted arrived as inert
// data. The generic Sync, Stream and InProcess helpers bind a spy to any typed
// declaration, so a provider added to the catalog gets stub handlers for the
// conformance suite with one line per op.
package testprovider

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

// FreeTextKeyword is def.FreeTextKeyword, the free-text schema marker.
const FreeTextKeyword = def.FreeTextKeyword

// MarkFreeText sets FreeTextKeyword on s.
func MarkFreeText(s *jsonschema.Schema) { def.MarkFreeText(s) }

// Provider identity and scopes.
const (
	Name       = "conformance"
	ReadScope  = "conformance:read"
	WriteScope = "conformance:write"
)

const namePattern = `^[a-z][a-z0-9-]{0,62}$`

// WidgetInput names one widget.
type WidgetInput struct {
	Name string `json:"name" jsonschema:"Widget name."`
}

// ListInput filters the widget list.
type ListInput struct {
	Filter []string `json:"filter,omitempty"` // Repeatable color=<color>
}

// RestartOptions is a nested object, so the suite walks nested properties.
type RestartOptions struct {
	Delay string `json:"delay,omitempty" jsonschema:"Delay before the restart, such as 5s."`
}

// RestartInput is the input of the mutating op.
type RestartInput struct {
	Name    string          `json:"name" jsonschema:"Widget name."`
	Mode    string          `json:"mode,omitempty" jsonschema:"soft or hard."`
	Note    string          `json:"note,omitempty" jsonschema:"Free text recorded with the restart."`
	Options *RestartOptions `json:"options,omitempty"`
}

// EchoInput is the input of the free-text op.
type EchoInput struct {
	Text string   `json:"text,omitempty" jsonschema:"Free text returned unchanged."`
	Tags []string `json:"tags,omitempty" jsonschema:"Tags: red or blue."`
}

// Widget is a widget resource.
type Widget struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Name string `json:"name"`
}

// RestartResult is the restart op's result.
type RestartResult struct {
	apitypes.ActionResult
	Name string `json:"name"`
}

// EchoResult is the echo op's result.
type EchoResult struct {
	Text string `json:"text"`
}

func refineName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = namePattern
	}
}

func refineList(s *jsonschema.Schema) {
	if p := s.Properties["filter"]; p != nil && p.Items != nil {
		p.Items.Pattern = `^color=(red|blue)$`
	}
}

func refineRestart(s *jsonschema.Schema) {
	refineName(s)
	if p := s.Properties["mode"]; p != nil {
		p.Enum = []any{"soft", "hard"}
	}
	if p := s.Properties["note"]; p != nil {
		n := 1024
		p.MaxLength = &n
		MarkFreeText(p)
	}
	if p := s.Properties["options"]; p != nil {
		if d := p.Properties["delay"]; d != nil {
			d.Pattern = `^[0-9]{1,3}s$`
		}
	}
}

func refineEcho(s *jsonschema.Schema) {
	if p := s.Properties["text"]; p != nil {
		MarkFreeText(p)
	}
	if p := s.Properties["tags"]; p != nil && p.Items != nil {
		p.Items.Enum = []any{"red", "blue"}
	}
}

// The declarations cover every Kind and Mode, a declared scope and a
// provider op that declares none (its read scope applies).
var (
	WidgetsList = def.Define[ListInput, apitypes.List[Widget]](def.Op{
		ID: Name + ".widgets.list", Provider: Name, Kind: def.KindList, ResourceType: "widgets",
		Summary: "List widgets", Description: "Lists the conformance provider's widgets. Read-only.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		Filters: []def.Filter{{Key: "color", Description: "Widget color: red or blue."}},
		CLI:     def.CLIBinding{Noun: "widget", Verb: "get"},
		MCP:     &def.MCPBinding{Tool: "list_widgets", Description: "List conformance widgets."},
		Refine:  refineList,
	})
	WidgetsGet = def.Define[WidgetInput, Widget](def.Op{
		ID: Name + ".widgets.get", Provider: Name, Kind: def.KindGet, ResourceType: "widgets",
		Summary: "Get a widget", Description: "Returns one conformance widget. Read-only.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "widget", Verb: "get", Positional: "name"},
		MCP:    &def.MCPBinding{Tool: "get_widget", Description: "Get one conformance widget."},
		Refine: refineName,
	})
	WidgetsLogs = def.Define[WidgetInput, apitypes.Frame](def.Op{
		ID: Name + ".widgets.logs", Provider: Name, Kind: def.KindSubresource, ResourceType: "widgets",
		Action: "logs", Summary: "Read a widget's output",
		Description: "Streams a conformance widget's output as NDJSON frames. Read-only.",
		Scope:       def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeStream, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "widgetlog", Verb: "get", Positional: "name"},
		Refine: refineName,
	})
	WidgetsRestart = def.Define[RestartInput, RestartResult](def.Op{
		ID: Name + ".widgets.restart", Provider: Name, Kind: def.KindResourceAction, ResourceType: "widgets",
		Action: "restart", Summary: "Restart a widget",
		Description: "Restarts a conformance widget. Side effects: none outside the test; the handler only " +
			"records its input. Long-running: answers 202 with an operation.",
		Scope: def.Scope{Name: WriteScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI:    def.CLIBinding{Noun: "widget", Verb: "restart", Positional: "name"},
		MCP:    &def.MCPBinding{Tool: "restart_widget", Description: "Restart a conformance widget."},
		Refine: refineRestart,
	})
	// ActionsEcho declares no scope, so the provider's read scope applies.
	ActionsEcho = def.Define[EchoInput, EchoResult](def.Op{
		ID: Name + ".actions.echo", Provider: Name, Kind: def.KindProviderAction, Action: "echo",
		Summary: "Echo text", Description: "Returns its free-text input unchanged. Read-only.",
		Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:    def.CLIBinding{Noun: "echo", Verb: "test"},
		Refine: refineEcho,
	})
)

// Call is one handler call a spy recorded.
type Call struct {
	Op          string
	OperationID string // Set for a long-running op
	Input       any    // The decoded input, as the handler received it
}

// Spy records handler calls. The zero value is ready to use.
type Spy struct {
	mu      sync.Mutex
	calls   []Call
	changed chan struct{} // Closed and replaced on every record
}

func (s *Spy) record(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// Calls returns every recorded call, oldest first.
func (s *Spy) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Wait returns the first recorded call that match accepts, waiting for one
// until ctx ends.
func (s *Spy) Wait(ctx context.Context, match func(Call) bool) (Call, error) {
	for {
		s.mu.Lock()
		for _, c := range s.calls {
			if match(c) {
				s.mu.Unlock()
				return c, nil
			}
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Call{}, ctx.Err()
		}
	}
}

// Sync binds a spy to a ModeSync declaration. The handler returns the zero
// output.
func Sync[In, Out any](s *Spy, op def.Typed[In, Out]) provider.Binding {
	id := op.Op().ID
	return provider.Sync(op, func(_ context.Context, in In) (Out, error) {
		s.record(Call{Op: id, Input: in})
		var out Out
		return out, nil
	})
}

// Stream binds a spy to a ModeStream declaration. The handler emits nothing.
func Stream[In any](s *Spy, op def.Typed[In, apitypes.Frame]) provider.Binding {
	id := op.Op().ID
	return provider.Stream(op, func(_ context.Context, in In, _ func(apitypes.Frame) error) error {
		s.record(Call{Op: id, Input: in})
		return nil
	})
}

// InProcess binds a spy to a long-running declaration. The run records the
// call with its operation ID and returns the zero output. It also stubs an op
// a real provider binds Detached: the declaration does not name the binding
// kind, and the Dispatcher's checks before admission are the same for both.
func InProcess[In, Out any](s *Spy, op def.Typed[In, Out]) provider.Binding {
	id := op.Op().ID
	return provider.InProcess(op, provider.InProcessSpec[In, Out]{
		Run: func(_ context.Context, in In, r *provider.Run) (Out, error) {
			s.record(Call{Op: id, OperationID: r.ID(), Input: in})
			var out Out
			return out, nil
		},
	})
}

// Stub is a provider made of a declaration and bindings: the conformance
// provider, and stub providers for catalog specs.
type Stub struct {
	spec     def.ProviderSpec
	bindings []provider.Binding
}

// NewStub returns an always-enabled provider with spec and bindings.
func NewStub(spec def.ProviderSpec, bindings ...provider.Binding) *Stub {
	return &Stub{spec: spec, bindings: bindings}
}

// Spec returns the declarations.
func (p *Stub) Spec() def.ProviderSpec { return p.spec }

// Configure accepts any section: the stub has no settings.
func (p *Stub) Configure(func(any) error) error { return nil }

// Preflight always reports the provider enabled.
func (p *Stub) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

// Bindings returns the bindings.
func (p *Stub) Bindings() []provider.Binding { return slices.Clone(p.bindings) }

// Spec is the conformance provider's declarations.
func Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: Name, ReadScope: ReadScope, Ops: []def.Op{
		WidgetsList.Op(), WidgetsGet.Op(), WidgetsLogs.Op(), WidgetsRestart.Op(), ActionsEcho.Op(),
	}}
}

// New returns the conformance provider with every handler bound to s.
func New(s *Spy) *Stub {
	return NewStub(Spec(),
		Sync(s, WidgetsList),
		Sync(s, WidgetsGet),
		Stream(s, WidgetsLogs),
		InProcess(s, WidgetsRestart),
		Sync(s, ActionsEcho),
	)
}

// InputJSON returns a recorded input as a generic JSON value, so a test can
// read a field by its JSON path.
func InputJSON(c Call) (any, error) {
	b, err := json.Marshal(c.Input)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}
