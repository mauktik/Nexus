// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package def

import (
	"fmt"
	"go/parser"
	"go/token"
	"maps"
	"os/exec"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

type resultItemsOut struct {
	Items []apitypes.Resource[struct{}] `json:"items"`
}

type plainItemsOut struct {
	Items []string `json:"items"`
}

// CLI.WaitedResultItems is accepted only on a long-running op whose output
// lists resources under items.
func TestDefineWaitedResultItems(t *testing.T) {
	op := mutatingOp(uniqueID("test.check"))
	op.CLI.WaitedResultItems = true
	typed := Define[struct{}, resultItemsOut](op)
	if item, ok := ResultItemType(typed.Op().OutputType()); !ok || item != reflect.TypeFor[apitypes.Resource[struct{}]]() {
		t.Errorf("ResultItemType = %v, %v; want the resource type", item, ok)
	}

	for _, tc := range []struct {
		name string
		def  func(Op)
		rule string
	}{
		{"sync op", func(op Op) {
			op.Risk, op.Mode = RiskReadOnly, ModeSync
			Define[struct{}, resultItemsOut](op)
		}, "requires ModeLongRunning"},
		{"output without items", func(op Op) { Define[struct{}, struct{}](op) }, "Items field listing resources"},
		{"items that are not resources", func(op Op) { Define[struct{}, plainItemsOut](op) }, "Items field listing resources"},
		{"a list, not a struct", func(op Op) { Define[struct{}, []apitypes.Resource[struct{}]](op) }, "Items field listing resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := mutatingOp(uniqueID("test.bad"))
			op.CLI.WaitedResultItems = true
			msg, panicked := recoverMessage(func() { tc.def(op) })
			if !panicked || !strings.Contains(msg, tc.rule) || !strings.Contains(msg, op.ID) {
				t.Errorf("panicked=%v msg=%q, want the op ID and %q", panicked, msg, tc.rule)
			}
		})
	}
}

type uploadIn struct {
	Signature string `json:"signature"`
}

type updateIn struct {
	UploadID       string `json:"uploadId"`
	AllowDowngrade bool   `json:"allowDowngrade"`
}

// uploadOp is the TC-SM-2 KindUpload declaration.
func uploadOp(id string) Op {
	return Op{
		ID:           id,
		Provider:     "tp",
		Kind:         KindUpload,
		ResourceType: "uploads",
		Summary:      "Upload a file",
		Description:  "Stages an uploaded file for a later operation. Synchronous.",
		Scope:        Scope{Name: "tp:admin"},
		Risk:         RiskMutating,
		Mode:         ModeSync,
		Upload:       &Upload{ContentTypes: []string{"application/octet-stream"}, MaxBytes: 1 << 20},
	}
}

// cliUploadOp is the TC-SM-3 long-running op that uploads through tp.uploads.add.
func cliUploadOp(id string) Op {
	op := mutatingOp(id)
	op.Kind, op.Action, op.CLI = KindProviderAction, "update", CLIBinding{Noun: "widget", Verb: "update"}
	op.ResourceType = ""
	op.CLI.Upload = &CLIUpload{
		Op:       "tp.uploads.add",
		Field:    "uploadId",
		FileFlag: "file",
		Sidecars: []CLISidecar{{Param: "signature", Flag: "signature", Suffix: ".sig", MaxBytes: 4096}},
	}
	return op
}

// TC-SM-2 (FR-6, CON-3): a valid KindUpload op is accepted as the second
// exception to "mutating means long-running"; a generic sync mutating op is not.
func TestTCSM2_AValidKindUploadDeclarationIsAccepted(t *testing.T) {
	op := uploadOp(uniqueID("tp.uploads.add"))
	var typed Typed[uploadIn, struct{}]
	if msg, panicked := recoverMessage(func() { typed = Define[uploadIn, struct{}](op) }); panicked {
		t.Fatalf("valid KindUpload op panicked: %s", msg)
	}
	got := typed.Op()
	if got.Kind != KindUpload || got.Upload != op.Upload || !reflect.DeepEqual(*got.Upload, *op.Upload) {
		t.Errorf("Op() = kind %q upload %+v, want kind %q upload %+v unchanged", got.Kind, got.Upload, KindUpload, op.Upload)
	}

	sync := mutatingOp(uniqueID("test.bad"))
	sync.Mode = ModeSync
	if msg, panicked := recoverMessage(define(sync)); !panicked || !strings.Contains(msg, "ModeLongRunning") {
		t.Errorf("sync mutating KindResourceAction: panicked=%v msg=%q, want the ModeLongRunning rule", panicked, msg)
	}
}

// TC-SM-3 (FR-13, FR-14): CLI.Upload and ReconnectWindow are accepted on a
// long-running op, at the 10m and 65536-byte boundaries, and round-trip.
func TestTCSM3_CLIUploadAndReconnectWindowDeclarationsAre(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Op)
	}{
		{"CLI.Upload", func(*Op) {}},
		{"ReconnectWindow 60s", func(op *Op) { op.CLI.ReconnectWindow = 60 * time.Second }},
		{"ReconnectWindow 10m", func(op *Op) { op.CLI.ReconnectWindow = 10 * time.Minute }},
		{"sidecar MaxBytes 65536", func(op *Op) { op.CLI.Upload.Sidecars[0].MaxBytes = 65536 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := cliUploadOp(uniqueID("tp.update"))
			tc.mod(&op)
			var typed Typed[updateIn, struct{}]
			if msg, panicked := recoverMessage(func() { typed = Define[updateIn, struct{}](op) }); panicked {
				t.Fatalf("Define panicked: %s", msg)
			}
			got := typed.Op().CLI
			if !reflect.DeepEqual(got.Upload, op.CLI.Upload) || got.ReconnectWindow != op.CLI.ReconnectWindow {
				t.Errorf("CLI = upload %+v window %v, want upload %+v window %v",
					got.Upload, got.ReconnectWindow, op.CLI.Upload, op.CLI.ReconnectWindow)
			}
		})
	}
}

// TC-SM-ERR1 (FR-6, CON-3): every KindUpload rule panics naming the op and the rule.
func TestTCSMERR1_InvalidKindUploadDeclarationsPanicWithA(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Op)
		rule string
	}{
		{"ModeLongRunning", func(op *Op) { op.Mode = ModeLongRunning }, "KindUpload requires ModeSync"},
		{"ModeStream", func(op *Op) { op.Mode = ModeStream }, "KindUpload requires ModeSync"},
		{"RiskReadOnly", func(op *Op) { op.Risk = RiskReadOnly }, "KindUpload requires RiskMutating"},
		{"RiskDestructive", func(op *Op) { op.Risk = RiskDestructive }, "KindUpload requires RiskMutating"},
		{"empty Scope.Name", func(op *Op) { op.Scope.Name = "" }, "KindUpload requires Scope.Name"},
		{"MCP set", func(op *Op) { op.MCP = &MCPBinding{Tool: "upload"} }, "KindUpload must not set MCP"},
		{"Upload nil", func(op *Op) { op.Upload = nil }, "KindUpload requires Upload"},
		{"empty ContentTypes", func(op *Op) { op.Upload.ContentTypes = nil }, "Upload.ContentTypes"},
		{"MaxBytes 0", func(op *Op) { op.Upload.MaxBytes = 0 }, "Upload.MaxBytes"},
		{"MaxBytes -1", func(op *Op) { op.Upload.MaxBytes = -1 }, "Upload.MaxBytes"},
		{"CLI.Noun", func(op *Op) { op.CLI.Noun = "x" }, "KindUpload requires an empty CLI binding"},
		{"CLI.Verb", func(op *Op) { op.CLI.Verb = "add" }, "KindUpload requires an empty CLI binding"},
		{"provider system", func(op *Op) { op.Provider = SystemProvider }, "KindUpload is not allowed for provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := uploadOp(uniqueID("tp.uploads.bad"))
			tc.mod(&op)
			msg, panicked := recoverMessage(func() { Define[uploadIn, struct{}](op) })
			if !panicked || !strings.Contains(msg, tc.rule) || !strings.Contains(msg, op.ID) {
				t.Errorf("panicked=%v msg=%q, want the op ID and %q", panicked, msg, tc.rule)
			}
		})
	}
}

// TC-SM-ERR2 (FR-6): Upload on any kind but KindUpload panics naming Upload and the kind.
func TestTCSMERR2_UploadSetOnAnyOtherKind(t *testing.T) {
	list := readOp("")
	list.Kind, list.CLI.Positional = KindList, ""
	sub := readOp("")
	sub.Kind, sub.Action = KindSubresource, "config"
	pa := mutatingOp("")
	pa.Kind, pa.ResourceType = KindProviderAction, ""
	for _, base := range []Op{list, readOp(""), sub, pa, mutatingOp("")} {
		t.Run(string(base.Kind), func(t *testing.T) {
			op := base
			op.ID = uniqueID("test.bad")
			if msg, panicked := recoverMessage(define(op)); panicked {
				t.Fatalf("valid %s op panicked without Upload: %s", op.Kind, msg)
			}
			op.ID = uniqueID("test.bad")
			op.Upload = &Upload{ContentTypes: []string{"application/octet-stream"}, MaxBytes: 1 << 20}
			msg, panicked := recoverMessage(define(op))
			if !panicked || !strings.Contains(msg, "Upload") || !strings.Contains(msg, fmt.Sprintf("%q", op.Kind)) || !strings.Contains(msg, op.ID) {
				t.Errorf("panicked=%v msg=%q, want the op ID, Upload and kind %q", panicked, msg, op.Kind)
			}
		})
	}
}

// TC-SM-ERR3 (FR-13, FR-14): invalid CLI.Upload and ReconnectWindow panic naming the field.
func TestTCSMERR3_InvalidCLIUploadAndReconnectWindowDeclarations(t *testing.T) {
	sync := func(op *Op) { op.Risk, op.Mode = RiskReadOnly, ModeSync }
	for _, tc := range []struct {
		name string
		mod  func(*Op)
		rule string
	}{
		{"CLI.Upload on a sync op", sync, "CLI.Upload requires ModeLongRunning"},
		{"empty Op", func(op *Op) { op.CLI.Upload.Op = "" }, "CLI.Upload.Op"},
		{"unknown Field", func(op *Op) { op.CLI.Upload.Field = "nope" }, "CLI.Upload.Field"},
		{"empty Field", func(op *Op) { op.CLI.Upload.Field = "" }, "CLI.Upload.Field"},
		{"bool Field", func(op *Op) { op.CLI.Upload.Field = "allowDowngrade" }, "CLI.Upload.Field"},
		{"Go name, not JSON name, as Field", func(op *Op) { op.CLI.Upload.Field = "UploadID" }, "CLI.Upload.Field"},
		{"FileFlag not kebab", func(op *Op) { op.CLI.Upload.FileFlag = "File_Path" }, "CLI.Upload.FileFlag"},
		{"empty FileFlag", func(op *Op) { op.CLI.Upload.FileFlag = "" }, "CLI.Upload.FileFlag"},
		{"FileFlag with a leading hyphen", func(op *Op) { op.CLI.Upload.FileFlag = "-file" }, "CLI.Upload.FileFlag"},
		{"FileFlag with a double hyphen", func(op *Op) { op.CLI.Upload.FileFlag = "file--path" }, "CLI.Upload.FileFlag"},
		{"sidecar empty Param", func(op *Op) { op.CLI.Upload.Sidecars[0].Param = "" }, "CLI.Upload.Sidecars[0].Param"},
		{"sidecar empty Flag", func(op *Op) { op.CLI.Upload.Sidecars[0].Flag = "" }, "CLI.Upload.Sidecars[0].Flag"},
		{"sidecar empty Suffix", func(op *Op) { op.CLI.Upload.Sidecars[0].Suffix = "" }, "CLI.Upload.Sidecars[0].Suffix"},
		{"sidecar MaxBytes 0", func(op *Op) { op.CLI.Upload.Sidecars[0].MaxBytes = 0 }, "CLI.Upload.Sidecars[0].MaxBytes"},
		{"sidecar MaxBytes 65537", func(op *Op) { op.CLI.Upload.Sidecars[0].MaxBytes = 65537 }, "CLI.Upload.Sidecars[0].MaxBytes"},
		{"ReconnectWindow -1s", func(op *Op) { op.CLI.ReconnectWindow = -time.Second }, "CLI.ReconnectWindow"},
		{"ReconnectWindow 10m+1ns", func(op *Op) { op.CLI.ReconnectWindow = 10*time.Minute + 1 }, "CLI.ReconnectWindow"},
		{"ReconnectWindow on a sync op", func(op *Op) {
			sync(op)
			op.CLI.Upload = nil
			op.CLI.ReconnectWindow = time.Minute
		}, "CLI.ReconnectWindow requires ModeLongRunning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := cliUploadOp(uniqueID("tp.update.bad"))
			tc.mod(&op)
			msg, panicked := recoverMessage(func() { Define[updateIn, struct{}](op) })
			if !panicked || !strings.Contains(msg, tc.rule) || !strings.Contains(msg, op.ID) {
				t.Errorf("panicked=%v msg=%q, want the op ID and %q", panicked, msg, tc.rule)
			}
		})
	}
}

// TC-SM-4 (CON-3): against base 3c12ee9, def gains no non-stdlib dependency
// and no direct import but time.
func TestTCSM4_DefImportsNothingNewBeyondTime(t *testing.T) {
	const base = "3c12ee9"
	const pkg = "github.com/mauktik/nexus/internal/def"
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	if err := exec.Command("git", "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		t.Skipf("base commit %s not available: %v", base, err)
	}

	baseImports := map[string]bool{}
	for _, f := range strings.Fields(run("git", "ls-tree", "--name-only", "--full-name", base, "./")) {
		if path.Ext(f) != ".go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, run("git", "show", base+":"+f), parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s at %s: %v", f, base, err)
		}
		for _, imp := range file.Imports {
			baseImports[strings.Trim(imp.Path.Value, `"`)] = true
		}
	}
	if len(baseImports) == 0 {
		t.Fatalf("no imports found for internal/def at %s", base)
	}

	for _, imp := range strings.Fields(run("go", "list", "-f", `{{join .Imports "\n"}}`, pkg)) {
		if !baseImports[imp] && imp != "time" {
			t.Errorf("internal/def imports %q, absent at %s and not time", imp, base)
		}
	}

	// deps maps each package of the -deps closure to whether it is stdlib.
	deps := func(pkgs ...string) map[string]bool {
		args := append([]string{"list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}"}, pkgs...)
		m := map[string]bool{}
		for _, line := range strings.Split(run("go", args...), "\n") {
			p, std, _ := strings.Cut(line, " ")
			m[p] = std == "true"
		}
		return m
	}
	now := deps(pkg)
	was := deps(slices.Sorted(maps.Keys(baseImports))...)
	was[pkg] = false
	viaTime := deps("time")
	for p, std := range now {
		if _, ok := was[p]; ok {
			continue
		}
		if !std {
			t.Errorf("new non-stdlib dependency %q", p)
		} else if _, ok := viaTime[p]; !ok {
			t.Errorf("new stdlib dependency %q not brought in by time", p)
		}
	}
	for p, std := range was {
		if _, ok := now[p]; !ok && !std {
			t.Errorf("non-stdlib dependency %q at %s is gone; the set must be unchanged", p, base)
		}
	}
}
