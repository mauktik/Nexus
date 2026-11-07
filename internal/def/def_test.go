package def

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
)

var opSeq atomic.Int64

// uniqueID keeps table cases from tripping the duplicate-ID rule by accident.
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s.%d", prefix, opSeq.Add(1))
}

// readOp is a valid read-only sync declaration under the test provider.
func readOp(id string) Op {
	return Op{
		ID:           id,
		Provider:     "test",
		Kind:         KindGet,
		ResourceType: "widgets",
		Summary:      "Get a widget",
		Description:  "Returns one widget. Read-only and fast.",
		Scope:        Scope{Name: "widgets:read"},
		Risk:         RiskReadOnly,
		Mode:         ModeSync,
		CLI:          CLIBinding{Noun: "widget", Verb: "get", Positional: "name"},
	}
}

// mutatingOp is a valid mutating long-running declaration.
func mutatingOp(id string) Op {
	op := readOp(id)
	op.Kind = KindResourceAction
	op.Action = "restart"
	op.Description = "Restarts a widget. Long-running: returns an operation."
	op.Scope = Scope{Name: "widgets:write"}
	op.Risk = RiskMutating
	op.Mode = ModeLongRunning
	op.CLI.Verb = "restart"
	return op
}

// cancelShape is the system.operations.cancel declaration shape.
func cancelShape(id string) Op {
	return Op{
		ID:               id,
		Provider:         "system",
		Path:             "/operations/{id}/cancel",
		OperationControl: true,
		Summary:          "Cancel an operation",
		Description:      "Cancels a queued operation or a running script. Synchronous.",
		Scope:            Scope{FromTarget: true},
		Risk:             RiskMutating,
		Mode:             ModeSync,
		CLI:              CLIBinding{Noun: "operation", Verb: "cancel", Positional: "id"},
	}
}

func recoverMessage(f func()) (msg string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			msg, panicked = fmt.Sprint(r), true
		}
	}()
	f()
	return "", false
}

func define(op Op) func() {
	return func() { Define[struct{}, struct{}](op) }
}

// TC-ERR1: Define rejects invalid declarations at init, naming the op and the rule.
func TestDefineRejectsInvalidDeclarations(t *testing.T) {
	dupID := uniqueID("test.dup")
	Define[struct{}, struct{}](readOp(dupID))

	cases := []struct {
		name string
		op   func(id string) Op
		id   string // fixed ID; empty means uniqueID
		rule string // substring the panic must contain besides the op ID
	}{
		{"unapproved CLI verb", func(id string) Op { op := readOp(id); op.CLI.Verb = "list"; return op }, "", "unapproved CLI verb"},
		{"empty CLI verb", func(id string) Op { op := readOp(id); op.CLI.Verb = ""; return op }, "", "unapproved CLI verb"},
		{"duplicate ID", readOp, dupID, "duplicate ID"},
		{"empty ID", func(string) Op { return readOp("") }, "", "empty ID"},
		{"missing Description", func(id string) Op { op := readOp(id); op.Description = ""; return op }, "", "missing Description"},
		{"blank Description", func(id string) Op { op := readOp(id); op.Description = " \t"; return op }, "", "missing Description"},
		{"mutating ModeSync", func(id string) Op { op := mutatingOp(id); op.Mode = ModeSync; return op }, "", "ModeLongRunning"},
		{"destructive ModeSync", func(id string) Op {
			op := mutatingOp(id)
			op.Risk, op.Mode = RiskDestructive, ModeSync
			return op
		}, "", "ModeLongRunning"},
		{"mutating empty Scope.Name", func(id string) Op { op := mutatingOp(id); op.Scope.Name = ""; return op }, "", "empty Scope.Name"},
		{"unknown risk", func(id string) Op { op := readOp(id); op.Risk = ""; return op }, "", "unknown Risk"},
		{"unknown mode", func(id string) Op { op := readOp(id); op.Mode = "fast"; return op }, "", "unknown Mode"},
		{"cancel shape under provider test", func(id string) Op { op := cancelShape(id); op.Provider = "test"; return op }, "", "OperationControl"},
		{"cancel shape without OperationControl", func(id string) Op { op := cancelShape(id); op.OperationControl = false; return op }, "", "ModeLongRunning"},
		{"control op with empty scope and no FromTarget", func(id string) Op {
			op := cancelShape(id)
			op.Scope = Scope{}
			return op
		}, "", "FromTarget"},
		{"Path under provider test", func(id string) Op { op := readOp(id); op.Path = "/widgets/{id}"; return op }, "", "Path"},
		{"FromTarget under provider test", func(id string) Op { op := readOp(id); op.Scope.FromTarget = true; return op }, "", "FromTarget"},
		{"generic mutating sync op under system", func(id string) Op {
			op := mutatingOp(id)
			op.Provider, op.Mode = "system", ModeSync
			return op
		}, "", "ModeLongRunning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			if id == "" {
				id = uniqueID("test.bad")
			}
			op := tc.op(id)
			msg, panicked := recoverMessage(define(op))
			if !panicked {
				t.Fatalf("Define(%s) did not panic", tc.name)
			}
			if !strings.Contains(msg, tc.rule) {
				t.Errorf("panic %q does not name the rule %q", msg, tc.rule)
			}
			if op.ID != "" && !strings.Contains(msg, op.ID) {
				t.Errorf("panic %q does not name the op ID %q", msg, op.ID)
			}
		})
	}
}

// TC-ERR1 step 3: the cancel shape is accepted only under provider system.
func TestDefineAcceptsOnlySystemOperationControl(t *testing.T) {
	if msg, panicked := recoverMessage(define(cancelShape(uniqueID("system.operations.cancel")))); panicked {
		t.Fatalf("cancel shape under provider system panicked: %s", msg)
	}
	if msg, panicked := recoverMessage(define(mutatingOp(uniqueID("test.ok")))); panicked {
		t.Fatalf("valid mutating long-running op panicked: %s", msg)
	}
	sysPath := readOp(uniqueID("system.operations.get"))
	sysPath.Provider, sysPath.Path, sysPath.Scope = "system", "/operations/{id}", Scope{FromTarget: true}
	if msg, panicked := recoverMessage(define(sysPath)); panicked {
		t.Fatalf("system read op with Path and FromTarget panicked: %s", msg)
	}
}

func TestDefineStreamRequiresFrameOutput(t *testing.T) {
	stream := readOp(uniqueID("test.logs"))
	stream.Kind, stream.Action, stream.Mode = KindSubresource, "logs", ModeStream

	msg, panicked := recoverMessage(func() { Define[struct{}, struct{}](stream) })
	if !panicked || !strings.Contains(msg, "ModeStream") || !strings.Contains(msg, stream.ID) {
		t.Fatalf("ModeStream with non-Frame output: panicked=%v msg=%q", panicked, msg)
	}

	stream.ID = uniqueID("test.logs")
	if msg, panicked := recoverMessage(func() { Define[struct{}, apitypes.Frame](stream) }); panicked {
		t.Fatalf("ModeStream with apitypes.Frame output panicked: %s", msg)
	}
}

func TestDefineDefaultsCancelAndRecordsTypes(t *testing.T) {
	typed := Define[modeInput, apitypes.Frame](readOp(uniqueID("test.types")))
	op := typed.Op()
	if op.Cancel != CancelQueuedOnly {
		t.Errorf("Cancel = %q, want default %q", op.Cancel, CancelQueuedOnly)
	}
	if got := op.InputType().Name(); got != "modeInput" {
		t.Errorf("InputType = %q, want modeInput", got)
	}
	if got := op.OutputType().Name(); got != "Frame" {
		t.Errorf("OutputType = %q, want Frame", got)
	}
	if op.Mutating() {
		t.Errorf("Mutating() = true for risk %q", op.Risk)
	}
	if !(Op{Risk: RiskMutating}).Mutating() || !(Op{Risk: RiskDestructive}).Mutating() {
		t.Error("Mutating() = false for a mutating or destructive risk")
	}
}
