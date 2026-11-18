// Package provider is the contract between a provider implementation and the
// core. Bindings tie a handler to a def.Typed declaration, so a handler whose
// types differ from the declaration does not compile. This package is
// daemon-only: the CLI imports declarations through internal/catalog instead.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
)

// Provider is one compiled-in provider.
type Provider interface {
	Spec() def.ProviderSpec                      // Name, ReadScope, Ops
	Configure(decode func(into any) error) error // Strict decode of its config section
	Preflight(ctx context.Context) Status        // Enabled or unavailable, plus reason and details
	Bindings() []Binding
}

// KV is the daemon's small persistent key-value store (the kv bucket of
// state.db). Keys are prefixed with the name of what writes them.
type KV interface {
	KVGet(key string) ([]byte, bool, error)
	KVPut(key string, value []byte) error
}

// KVUser is a provider that keeps state in KV. The daemon calls UseKV once,
// after opening the store and before serving any request.
type KVUser interface {
	UseKV(KV)
}

// OpsDirUser is a provider whose detached operations keep files of their
// own in the operation's directory, <dir>/<operation id>, beside spec.json
// and exit.json (the docker update helper's input.json and result.json).
// The daemon calls UseOpsDir once, before serving any request.
type OpsDirUser interface {
	UseOpsDir(dir string)
}

// Status is a provider's preflight result.
type Status struct {
	State   apitypes.ProviderState
	Reason  string         // Human-readable; required unless enabled
	Details map[string]any // Reported in capabilities
}

// LanePolicy is what a lane does with work beyond its limit.
type LanePolicy string

const (
	LaneQueue  LanePolicy = "queue"  // FIFO
	LaneReject LanePolicy = "reject" // 409 naming the blocking operation
)

// Lane serializes operations that must not overlap.
type Lane struct {
	Key    string     // "apt", "script:deploy-api", "container:web", "compose:media"
	Policy LanePolicy // LaneQueue or LaneReject
	Limit  int        // Concurrent slots; 0 means 1
}

// NoLane has an empty Key: no queue, no conflict check, no lane mutex.
var NoLane = Lane{}

// Slots returns the lane's concurrent slots, defaulting to 1.
func (l Lane) Slots() int {
	if l.Limit <= 0 {
		return 1
	}
	return l.Limit
}

// Run is the context an in-process operation runs with.
type Run struct {
	id     string
	output io.Writer
}

// NewRun creates the run context for operation id. Output written by the
// handler goes to output; a nil output discards it.
func NewRun(id string, output io.Writer) *Run {
	if output == nil {
		output = io.Discard
	}
	return &Run{id: id, output: output}
}

// ID is the operation ID.
func (r *Run) ID() string { return r.id }

// Output is where the handler writes progress for the operation log.
func (r *Run) Output() io.Writer { return r.output }

// InProcessSpec is a long-running operation that runs inside nexusd. A crash
// of nexusd ends it outcome-unknown.
type InProcessSpec[In, Out any] struct {
	Admit func(context.Context, In) error         // Optional synchronous pre-checks; return a *problem.Error
	Lane  func(context.Context, In) (Lane, error) // Optional; nil means NoLane. Runs once, at admission
	Run   func(context.Context, In, *Run) (Out, error)
}

// Admitted is what an Admission hook decided.
type Admitted[In, Out any] struct {
	// Done ends the call now with Out (200): no operation record, lane
	// hold, Prepare or unit. Only an op declaring def.Op.CompletesAtAdmission
	// may answer Done; the Dispatcher refuses it from any other op.
	Done bool
	Out  Out
	// In is the input Prepare runs on when !Done. Fields tagged json:"-"
	// carry what admission derived; they are never persisted.
	In In
}

// DetachedSpec is a long-running operation that runs in a systemd transient
// unit outside nexusd, so it survives a crash or restart of the daemon
// (FR-6).
type DetachedSpec[In, Out any] struct {
	Admit func(context.Context, In) error         // Optional synchronous pre-checks; return a *problem.Error
	Lane  func(context.Context, In) (Lane, error) // Optional; nil means NoLane. Runs once, at admission
	// Admission replaces Admit (setting both panics in Detached). It runs at
	// Admit's step, before the lane; an error refuses the call as Admit's
	// does. Required for an op declaring def.Op.CompletesAtAdmission (D-7).
	Admission func(context.Context, In) (Admitted[In, Out], error)
	// Prepare builds the process spec and captures any pre-state. Its
	// context is canceled by a cancel of the queued operation. Required.
	Prepare func(context.Context, In, *Run) (detach.Spec, error)
	// Finalize turns exit.json into the result. It must be idempotent, read
	// files and never wait, and must not depend on sensitive input fields:
	// In is decoded from the record's redacted input. Required.
	Finalize func(context.Context, In, detach.Exit, *Run) (Out, error)
	// LostDetail is appended to outcome-unknown when the unit died without
	// exit.json. Optional.
	LostDetail func(*Run, In) string
	// AfterReboot decides the outcome of an op declaring
	// def.Op.CompletesAcrossReboot whose unit ended without a result of its
	// own after its helper recorded a reboot request in an earlier boot.
	// Same rules as Finalize: idempotent, reads files, never waits, never
	// depends on sensitive input. Required exactly when the op declares
	// CompletesAcrossReboot; Detached panics otherwise.
	AfterReboot func(context.Context, In, Rebooted, *Run) (Out, error)
}

// Rebooted is what the manager knows when an op declaring
// def.Op.CompletesAcrossReboot finds itself in a new boot.
type Rebooted struct {
	PreviousBootID string    // From ops/<id>/reboot-requested
	BootID         string    // This boot's
	RequestedAt    time.Time // When the helper recorded the request
	ReadyAt        time.Time // When this nexusd sent READY=1 and began recovery
}

// BindingKind is how a binding executes.
type BindingKind string

const (
	BindSync      BindingKind = "sync"
	BindStream    BindingKind = "stream"
	BindInProcess BindingKind = "in-process"
	BindDetached  BindingKind = "detached"
	BindUpload    BindingKind = "upload"
)

// InProcessFuncs is an in-process binding with its types erased. Each function
// takes the value returned by Binding.Decode.
type InProcessFuncs struct {
	Admit func(ctx context.Context, in any) error
	Lane  func(ctx context.Context, in any) (Lane, error)
	Run   func(ctx context.Context, in any, r *Run) (any, error)
}

// DetachedFuncs is a detached binding with its types erased. Each function
// takes the value returned by Binding.Decode.
type DetachedFuncs struct {
	Admit      func(ctx context.Context, in any) error
	Lane       func(ctx context.Context, in any) (Lane, error)
	Prepare    func(ctx context.Context, in any, r *Run) (detach.Spec, error)
	Finalize   func(ctx context.Context, in any, x detach.Exit, r *Run) (any, error)
	LostDetail func(r *Run, in any) string // Empty when the spec has none
	// Admission is the spec's Admission hook: done with out, or the input
	// next that Prepare runs on. Nil when the spec has none.
	Admission func(ctx context.Context, in any) (done bool, out any, next any, err error)
	// AfterReboot is the spec's AfterReboot hook. Nil exactly when the op
	// does not declare def.Op.CompletesAcrossReboot.
	AfterReboot func(ctx context.Context, in any, rb Rebooted, r *Run) (any, error)
}

// UploadBody is the raw request body of a KindUpload call.
type UploadBody struct {
	// Reader streams the body. The transport caps it: a read past the cap
	// fails with an error wrapping core.ErrUploadTooLarge.
	Reader        io.Reader
	ContentType   string
	ContentLength int64 // -1 when unknown
}

// UploadSpec is a KindUpload operation: a sync, mutating call whose input
// comes from the query and whose request body is streamed to Receive.
type UploadSpec[In, Out any] struct {
	// MaxBytes is the body size limit at run time. Nil means the op's
	// Upload.MaxBytes; a larger value is clamped to it.
	MaxBytes func() int64
	// Admit is an optional pre-check that runs before the requested audit
	// record and before the body is read; return a *problem.Error to refuse.
	// Its release frees whatever Admit holds (nexusd: one upload slot) and
	// may be nil. The Dispatcher defers a non-nil release the moment Admit
	// returns, before it looks at err, so release runs exactly once on every
	// later path: Admit refusal, requested-write failure, a context done
	// before Receive, Receive error or panic, and success. An Admit that
	// refuses should hold nothing and return a nil release. release must be
	// cheap, non-blocking and safe to call from the request goroutine.
	Admit func(context.Context, In) (release func(), err error)
	// Receive reads the body and stores it. Required. It must remove any
	// partial state when it fails.
	Receive func(context.Context, In, UploadBody) (Out, error)
}

// UploadFuncs is an upload binding with its types erased. Each function
// takes the value returned by Binding.Decode.
type UploadFuncs struct {
	MaxBytes func() int64                                                  // Never above the op's Upload.MaxBytes
	Admit    func(ctx context.Context, in any) (release func(), err error) // A nil spec Admit returns (nil, nil)
	Receive  func(ctx context.Context, in any, b UploadBody) (any, error)
}

// Binding is a handler bound to its declaration. The core calls it through
// the type-erased methods; the types were already checked at compile time.
type Binding struct {
	op        def.Op
	kind      BindingKind
	decode    func(json.RawMessage) (any, error)
	sync      func(context.Context, any) (any, error)
	stream    func(context.Context, any, func(apitypes.Frame) error) error
	inProcess *InProcessFuncs
	detached  *DetachedFuncs
	upload    *UploadFuncs
}

// Op returns the declaration the binding implements.
func (b Binding) Op() def.Op { return b.op }

// Kind returns how the binding executes.
func (b Binding) Kind() BindingKind { return b.kind }

// Decode strictly decodes schema-validated input into the op's input type.
func (b Binding) Decode(raw json.RawMessage) (any, error) { return b.decode(raw) }

// CallSync runs a sync binding with input from Decode.
func (b Binding) CallSync(ctx context.Context, in any) (any, error) {
	if b.sync == nil {
		return nil, fmt.Errorf("op %q: binding is %s, not sync", b.op.ID, b.kind)
	}
	return b.sync(ctx, in)
}

// CallStream runs a stream binding with input from Decode.
func (b Binding) CallStream(ctx context.Context, in any, emit func(apitypes.Frame) error) error {
	if b.stream == nil {
		return fmt.Errorf("op %q: binding is %s, not stream", b.op.ID, b.kind)
	}
	return b.stream(ctx, in, emit)
}

// InProcess returns the erased functions of an in-process binding.
func (b Binding) InProcess() (InProcessFuncs, bool) {
	if b.inProcess == nil {
		return InProcessFuncs{}, false
	}
	return *b.inProcess, true
}

// Detached returns the erased functions of a detached binding.
func (b Binding) Detached() (DetachedFuncs, bool) {
	if b.detached == nil {
		return DetachedFuncs{}, false
	}
	return *b.detached, true
}

// Upload returns the erased functions of an upload binding.
func (b Binding) Upload() (UploadFuncs, bool) {
	if b.upload == nil {
		return UploadFuncs{}, false
	}
	return *b.upload, true
}

// Sync binds a handler to a ModeSync operation.
func Sync[In, Out any](op def.Typed[In, Out], h func(context.Context, In) (Out, error)) Binding {
	d := op.Op()
	requireMode(d, def.ModeSync, "Sync")
	if h == nil {
		panic(fmt.Sprintf("provider: op %q: nil Sync handler", d.ID))
	}
	return Binding{
		op:     d,
		kind:   BindSync,
		decode: decoder[In](),
		sync: func(ctx context.Context, in any) (any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			return h(ctx, v)
		},
	}
}

// Stream binds a handler to a ModeStream operation. The handler calls emit
// once per frame and stops when emit returns an error.
func Stream[In any](op def.Typed[In, apitypes.Frame],
	h func(ctx context.Context, in In, emit func(apitypes.Frame) error) error) Binding {
	d := op.Op()
	requireMode(d, def.ModeStream, "Stream")
	if h == nil {
		panic(fmt.Sprintf("provider: op %q: nil Stream handler", d.ID))
	}
	return Binding{
		op:     d,
		kind:   BindStream,
		decode: decoder[In](),
		stream: func(ctx context.Context, in any, emit func(apitypes.Frame) error) error {
			v, err := typedInput[In](d, in)
			if err != nil {
				return err
			}
			return h(ctx, v, emit)
		},
	}
}

// InProcess binds a long-running operation that runs inside nexusd.
func InProcess[In, Out any](op def.Typed[In, Out], s InProcessSpec[In, Out]) Binding {
	d := op.Op()
	requireMode(d, def.ModeLongRunning, "InProcess")
	if s.Run == nil {
		panic(fmt.Sprintf("provider: op %q: InProcessSpec.Run is nil", d.ID))
	}
	funcs := &InProcessFuncs{
		Admit: func(ctx context.Context, in any) error {
			v, err := typedInput[In](d, in)
			if err != nil {
				return err
			}
			if s.Admit == nil {
				return nil
			}
			return s.Admit(ctx, v)
		},
		Lane: func(ctx context.Context, in any) (Lane, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return Lane{}, err
			}
			if s.Lane == nil {
				return NoLane, nil
			}
			return s.Lane(ctx, v)
		},
		Run: func(ctx context.Context, in any, r *Run) (any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			return s.Run(ctx, v, r)
		},
	}
	return Binding{op: d, kind: BindInProcess, decode: decoder[In](), inProcess: funcs}
}

// Detached binds a long-running operation that runs in a transient unit.
func Detached[In, Out any](op def.Typed[In, Out], s DetachedSpec[In, Out]) Binding {
	d := op.Op()
	requireMode(d, def.ModeLongRunning, "Detached")
	if s.Prepare == nil || s.Finalize == nil {
		panic(fmt.Sprintf("provider: op %q: DetachedSpec needs Prepare and Finalize", d.ID))
	}
	if s.Admit != nil && s.Admission != nil {
		panic(fmt.Sprintf("provider: op %q: DetachedSpec sets both Admit and Admission; Admission replaces Admit", d.ID))
	}
	if d.CompletesAtAdmission && s.Admission == nil {
		panic(fmt.Sprintf("provider: op %q: declares CompletesAtAdmission, so DetachedSpec needs Admission", d.ID))
	}
	if d.CompletesAcrossReboot && s.AfterReboot == nil {
		panic(fmt.Sprintf("provider: op %q: declares CompletesAcrossReboot, so DetachedSpec needs AfterReboot", d.ID))
	}
	if !d.CompletesAcrossReboot && s.AfterReboot != nil {
		panic(fmt.Sprintf("provider: op %q: DetachedSpec sets AfterReboot, but the op does not declare CompletesAcrossReboot", d.ID))
	}
	funcs := &DetachedFuncs{
		Admit: func(ctx context.Context, in any) error {
			v, err := typedInput[In](d, in)
			if err != nil {
				return err
			}
			if s.Admit == nil {
				return nil
			}
			return s.Admit(ctx, v)
		},
		Lane: func(ctx context.Context, in any) (Lane, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return Lane{}, err
			}
			if s.Lane == nil {
				return NoLane, nil
			}
			return s.Lane(ctx, v)
		},
		Prepare: func(ctx context.Context, in any, r *Run) (detach.Spec, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return detach.Spec{}, err
			}
			return s.Prepare(ctx, v, r)
		},
		Finalize: func(ctx context.Context, in any, x detach.Exit, r *Run) (any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			return s.Finalize(ctx, v, x, r)
		},
		LostDetail: func(r *Run, in any) string {
			v, err := typedInput[In](d, in)
			if err != nil || s.LostDetail == nil {
				return ""
			}
			return s.LostDetail(r, v)
		},
	}
	if s.Admission != nil {
		funcs.Admission = func(ctx context.Context, in any) (bool, any, any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return false, nil, nil, err
			}
			a, err := s.Admission(ctx, v)
			if err != nil {
				return false, nil, nil, err
			}
			if a.Done {
				return true, a.Out, nil, nil
			}
			return false, nil, a.In, nil
		}
	}
	if s.AfterReboot != nil {
		funcs.AfterReboot = func(ctx context.Context, in any, rb Rebooted, r *Run) (any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			return s.AfterReboot(ctx, v, rb, r)
		}
	}
	return Binding{op: d, kind: BindDetached, decode: decoder[In](), detached: funcs}
}

// Upload binds a KindUpload operation, whose request body is streamed to
// Receive.
func Upload[In, Out any](op def.Typed[In, Out], s UploadSpec[In, Out]) Binding {
	d := op.Op()
	requireMode(d, def.ModeSync, "Upload")
	if d.Kind != def.KindUpload || d.Upload == nil {
		panic(fmt.Sprintf("provider: op %q: Upload requires KindUpload, op is %s", d.ID, d.Kind))
	}
	if s.Receive == nil {
		panic(fmt.Sprintf("provider: op %q: UploadSpec.Receive is nil", d.ID))
	}
	ceiling := d.Upload.MaxBytes
	funcs := &UploadFuncs{
		MaxBytes: func() int64 {
			if s.MaxBytes == nil {
				return ceiling
			}
			return min(s.MaxBytes(), ceiling)
		},
		Admit: func(ctx context.Context, in any) (func(), error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			if s.Admit == nil {
				return nil, nil
			}
			return s.Admit(ctx, v)
		},
		Receive: func(ctx context.Context, in any, b UploadBody) (any, error) {
			v, err := typedInput[In](d, in)
			if err != nil {
				return nil, err
			}
			return s.Receive(ctx, v, b)
		},
	}
	return Binding{op: d, kind: BindUpload, decode: decoder[In](), upload: funcs}
}

func requireMode(op def.Op, want def.Mode, constructor string) {
	if op.Mode != want {
		panic(fmt.Sprintf("provider: op %q: %s requires %s, op is %s", op.ID, constructor, want, op.Mode))
	}
}

func decoder[In any]() func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		var v In
		if len(bytes.TrimSpace(raw)) == 0 {
			return v, nil
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("decode input: %w", err)
		}
		return v, nil
	}
}

func typedInput[In any](op def.Op, in any) (In, error) {
	v, ok := in.(In)
	if !ok {
		var zero In
		return zero, fmt.Errorf("op %q: input is %T, want %T", op.ID, in, zero)
	}
	return v, nil
}
