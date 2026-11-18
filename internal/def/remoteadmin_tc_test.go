// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-1, TC-RA-2,
// TC-RA-3, TC-RA-4, TC-RA-110, TC-RA-E1, TC-RA-E2, TC-RA-ERR1, TC-RA-ERR2,
// TC-RA-ERR3, TC-RA-ERR4 and TC-RA-ERR40 (WI 1.1): the SyncMutation,
// SecretOutput, Audited, CompletesAtAdmission, CLI.Variadic and
// CLI.EchoWarnings declarations and their validate rules.

package def

import (
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

// Compile-time check that CLIBinding stays comparable with the new fields
// (TC-RA-4): validateUpload compares it with ==.
var _ = CLIBinding{Variadic: "x", EchoWarnings: true} == CLIBinding{}

type tcraOut struct {
	apitypes.ActionResult
}

type tcraSecretOut struct {
	apitypes.ActionResult
	Token string   `json:"token"`
	Count int      `json:"count"`
	Names []string `json:"names"`
}

type tcraInner struct {
	Token string `json:"token"`
}

type tcraEmbeddedSecretOut struct {
	tcraInner
}

type tcraHiddenEmbeddedSecretOut struct {
	tcraInner `json:"-"`
}

type tcraOwnWarningsOut struct {
	Warnings []string `json:"warnings"`
}

type tcraIntWarningsOut struct {
	Warnings []int `json:"warnings"`
}

type tcraVariadicIn struct {
	Name    string   `json:"name"`
	Changes []string `json:"changes"`
}

// syncMutationOp is TC-RA-1's op A: a SyncMutation provider action that meets
// every rule.
func syncMutationOp(id string) Op {
	return Op{
		ID:           id,
		Provider:     "tcra",
		Kind:         KindProviderAction,
		Action:       "rotate",
		Summary:      "Rotate a credential",
		Description:  "Rotates a credential and returns it once. Synchronous: no operation.",
		Scope:        Scope{Name: "tcra:admin"},
		Risk:         RiskMutating,
		Mode:         ModeSync,
		SyncMutation: true,
		CLI:          CLIBinding{Noun: "tcra", Verb: "add"},
	}
}

// admissionOp is TC-RA-110's op C: a long-running mutating provider action
// that may complete at admission.
func admissionOp(id string) Op {
	return Op{
		ID:                   id,
		Provider:             "tcra",
		Kind:                 KindProviderAction,
		Action:               "update-config",
		Summary:              "Update the configuration",
		Description:          "Updates the configuration. Long-running unless nothing changes.",
		Scope:                Scope{Name: "tcra:admin"},
		Risk:                 RiskMutating,
		Mode:                 ModeLongRunning,
		CompletesAtAdmission: true,
		CLI:                  CLIBinding{Noun: "tcra", Verb: "update"},
	}
}

// mustDefine defines op with In and Out, failing the test on a panic.
func mustDefine[In, Out any](t *testing.T, op Op) Op {
	t.Helper()
	var typed Typed[In, Out]
	if msg, panicked := recoverMessage(func() { typed = Define[In, Out](op) }); panicked {
		t.Fatalf("op %q panicked: %s", op.ID, msg)
	}
	return typed.Op()
}

// mustRefuse defines op with In and Out and checks the panic names every want.
func mustRefuse[In, Out any](t *testing.T, op Op, want ...string) {
	t.Helper()
	msg, panicked := recoverMessage(func() { Define[In, Out](op) })
	if !panicked {
		t.Fatalf("op %q: no panic, want one naming %q", op.ID, want)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("op %q: panic %q does not name %q", op.ID, msg, w)
		}
	}
}

// TC-RA-1 (CON-3, FR-1, FR-3): a SyncMutation op that meets every rule is
// accepted, and the OperationControl and KindUpload exceptions still are.
func TestTCRA1_ASyncMutationOpThatMeetsEveryRuleIsAcceptedAnd(t *testing.T) {
	a := mustDefine[struct{}, tcraOut](t, syncMutationOp(uniqueID("tcra.actions.rotate")))
	if !a.Mutating() || a.Mode != ModeSync || !a.SyncMutation {
		t.Errorf("op A: Mutating()=%v Mode=%q SyncMutation=%v, want true %q true", a.Mutating(), a.Mode, a.SyncMutation, ModeSync)
	}

	b := syncMutationOp(uniqueID("tcra.tokens.remove"))
	b.Kind, b.ResourceType, b.Action, b.Risk = KindResourceAction, "tokens", "remove", RiskDestructive
	b.CLI = CLIBinding{Noun: "tcra", Verb: "remove", Positional: "name"}
	mustDefine[struct{}, tcraOut](t, b)

	// The shapes of system.operations.cancel and nexusd.uploads.add: the real
	// declarations live in packages that import def.
	mustDefine[struct{}, struct{}](t, cancelShape(uniqueID("system.operations.cancel")))
	mustDefine[uploadIn, struct{}](t, uploadOp(uniqueID("nexusd.uploads.add")))
}

// TC-RA-2 (CON-3, NFR-1): SecretOutput naming a string output field is accepted.
func TestTCRA2_SecretOutputNamingAStringOutputFieldIsAccepted(t *testing.T) {
	op := syncMutationOp(uniqueID("tcra.tokens.add"))
	op.SecretOutput = "token"
	got := mustDefine[struct{}, tcraSecretOut](t, op)
	if got.SecretOutput != "token" {
		t.Errorf("SecretOutput = %q, want %q", got.SecretOutput, "token")
	}
}

// TC-RA-3 (FR-13, FR-2, FR-6): Audited is accepted on a read-only sync op.
func TestTCRA3_AuditedIsAcceptedOnAReadOnlySyncOp(t *testing.T) {
	op := readOp(uniqueID("tcra.tokens.list"))
	op.Kind, op.CLI.Positional, op.Audited = KindList, "", true
	got := mustDefine[struct{}, struct{}](t, op)
	if got.Mutating() || !got.Audited {
		t.Errorf("Mutating()=%v Audited=%v, want false true", got.Mutating(), got.Audited)
	}
}

// TC-RA-4 (FR-11, FR-3, FR-7): CLI.Variadic and CLI.EchoWarnings are accepted
// where declared correctly, and validateUpload's == on CLIBinding still works.
func TestTCRA4_CLIVariadicAndCLIEchoWarningsAreAcceptedWhere(t *testing.T) {
	op := admissionOp(uniqueID("tcra.actions.update"))
	op.CompletesAtAdmission = false
	op.CLI = CLIBinding{Noun: "tcra", Verb: "update", Variadic: "changes", EchoWarnings: true}
	got := mustDefine[tcraVariadicIn, tcraOut](t, op)
	if got.CLI.Variadic != "changes" || !got.CLI.EchoWarnings {
		t.Errorf("CLI = %+v, want Variadic changes and EchoWarnings", got.CLI)
	}

	up := uploadOp(uniqueID("tcra.uploads.add"))
	up.CLI = CLIBinding{Variadic: "changes"}
	mustRefuse[uploadIn, struct{}](t, up, "KindUpload requires an empty CLI binding")
}

// TC-RA-110 (CON-3, FR-7): CompletesAtAdmission is accepted on a long-running
// mutating action without MCP.
func TestTCRA110_CompletesAtAdmissionIsAcceptedOnALongRunning(t *testing.T) {
	c := mustDefine[struct{}, tcraOut](t, admissionOp(uniqueID("tcra.actions.update-config")))

	cr := admissionOp(uniqueID("tcra.configs.update"))
	cr.Kind, cr.ResourceType, cr.Action = KindResourceAction, "configs", "update"
	cr.CLI.Positional = "name"
	c2 := mustDefine[struct{}, tcraOut](t, cr)

	for _, op := range []Op{c, c2} {
		if !op.CompletesAtAdmission || op.Mode != ModeLongRunning {
			t.Errorf("op %q: CompletesAtAdmission=%v Mode=%q, want true %q", op.ID, op.CompletesAtAdmission, op.Mode, ModeLongRunning)
		}
	}

	// The shape of nexusd.actions.reload: long-running without the flag.
	reload := mutatingOp(uniqueID("nexusd.actions.reload"))
	reload.Kind, reload.ResourceType, reload.Action = KindProviderAction, "", "reload"
	reload.CLI = CLIBinding{Noun: "nexusd", Verb: "reload"}
	mustDefine[struct{}, tcraOut](t, reload)
}

// TC-RA-E1 (CON-3): a SecretOutput field reached only through an embedded
// struct is accepted when it is a string, and not when JSON hides the struct.
func TestTCRAE1_ASecretOutputFieldReachedOnlyThroughAnEmbedded(t *testing.T) {
	op := syncMutationOp(uniqueID("tcra.tokens.add"))
	op.SecretOutput = "token"
	mustDefine[struct{}, tcraEmbeddedSecretOut](t, op)

	hidden := syncMutationOp(uniqueID("tcra.tokens.add"))
	hidden.SecretOutput = "token"
	mustRefuse[struct{}, tcraHiddenEmbeddedSecretOut](t, hidden, "SecretOutput", `"token"`)
}

// TC-RA-E2 (FR-11): EchoWarnings accepts a []string warnings field, own or
// promoted from apitypes.ActionResult, and refuses one of another type.
func TestTCRAE2_EchoWarningsAcceptsWarningsReachedThroughThe(t *testing.T) {
	echo := func() Op {
		op := syncMutationOp(uniqueID("tcra.tokens.remove"))
		op.CLI.EchoWarnings = true
		return op
	}
	mustDefine[struct{}, tcraOut](t, echo())
	mustDefine[struct{}, tcraOwnWarningsOut](t, echo())
	mustRefuse[struct{}, tcraIntWarningsOut](t, echo(), "CLI.EchoWarnings", "warnings")
}

// TC-RA-ERR1 (CON-3): SyncMutation is refused wherever one of its rules fails.
func TestTCRAERR1_SyncMutationIsRefusedWhereverARuleFails(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*Op)
		rule   string
	}{
		{"read-only risk", func(o *Op) { o.Risk = RiskReadOnly }, "mutating"},
		{"long-running", func(o *Op) { o.Mode = ModeLongRunning }, "ModeSync"},
		{"KindList", func(o *Op) { o.Kind, o.ResourceType, o.Action = KindList, "tokens", "" }, "KindResourceAction or KindProviderAction"},
		{"KindUpload", func(o *Op) {
			o.Kind, o.ResourceType, o.Action, o.CLI = KindUpload, "uploads", "", CLIBinding{}
			o.Upload = &Upload{ContentTypes: []string{"application/octet-stream"}, MaxBytes: 1 << 20}
		}, "KindResourceAction or KindProviderAction"},
		{"provider system", func(o *Op) { o.Provider = SystemProvider }, `provider "system"`},
		{"empty scope", func(o *Op) { o.Scope = Scope{} }, "Scope.Name"},
		{"MCP set", func(o *Op) { o.MCP = &MCPBinding{Tool: "rotate", Description: "Rotate."} }, "MCP"},
		{"WhatIf", func(o *Op) { o.WhatIf = true }, "WhatIf"},
		{"Idempotent", func(o *Op) { o.Idempotent = true }, "Idempotent"},
		{"OperationControl", func(o *Op) { o.OperationControl = true }, "OperationControl"},
		{"CLI.Upload", func(o *Op) {
			o.CLI.Upload = &CLIUpload{Op: "tcra.uploads.add", Field: "uploadId", FileFlag: "file"}
		}, "CLI.Upload"},
		{"CLI.ReconnectWindow", func(o *Op) { o.CLI.ReconnectWindow = time.Second }, "CLI.ReconnectWindow"},
		{"CLI.WaitedResultItems", func(o *Op) { o.CLI.WaitedResultItems = true }, "CLI.WaitedResultItems"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := syncMutationOp(uniqueID("tcra.actions.rotate"))
			tc.modify(&op)
			mustRefuse[updateIn, tcraOut](t, op, "SyncMutation", tc.rule)
		})
	}

	t.Run("no exception", func(t *testing.T) {
		op := syncMutationOp(uniqueID("tcra.actions.rotate"))
		op.SyncMutation = false
		mustRefuse[struct{}, tcraOut](t, op, "mutating op must be ModeLongRunning")
	})
}

// TC-RA-ERR2 (CON-3, NFR-1): SecretOutput is refused without SyncMutation or
// on a field that is not a string.
func TestTCRAERR2_SecretOutputIsRefusedWithoutSyncMutationOrOnA(t *testing.T) {
	longRunning := admissionOp(uniqueID("tcra.actions.update"))
	longRunning.CompletesAtAdmission = false
	longRunning.SecretOutput = "token"
	mustRefuse[struct{}, tcraSecretOut](t, longRunning, "SecretOutput", "SyncMutation")

	for _, field := range []string{"count", "names", "absent"} {
		t.Run(field, func(t *testing.T) {
			op := syncMutationOp(uniqueID("tcra.tokens.add"))
			op.SecretOutput = field
			mustRefuse[struct{}, tcraSecretOut](t, op, "SecretOutput", `"`+field+`"`)
		})
	}
}

// TC-RA-ERR3 (FR-13): Audited is refused on a mutating op or a long-running read.
func TestTCRAERR3_AuditedIsRefusedOnAMutatingOpOrALongRunningRead(t *testing.T) {
	mutating := mutatingOp(uniqueID("tcra.widgets.restart"))
	mutating.Audited = true
	mustRefuse[struct{}, struct{}](t, mutating, "Audited", "RiskReadOnly")

	read := readOp(uniqueID("tcra.widgets.get"))
	read.Mode, read.Audited = ModeLongRunning, true
	mustRefuse[struct{}, struct{}](t, read, "Audited", "ModeSync")
}

// TC-RA-ERR4 (FR-11): Variadic and EchoWarnings are refused when misdeclared.
func TestTCRAERR4_VariadicAndEchoWarningsAreRefusedWhenMisdeclared(t *testing.T) {
	variadic := func(field, positional string) Op {
		op := admissionOp(uniqueID("tcra.actions.update"))
		op.CompletesAtAdmission = false
		op.CLI = CLIBinding{Noun: "tcra", Verb: "update", Variadic: field, Positional: positional}
		return op
	}
	mustRefuse[tcraVariadicIn, tcraOut](t, variadic("name", ""), "CLI.Variadic", `"name"`)
	mustRefuse[tcraVariadicIn, tcraOut](t, variadic("absent", ""), "CLI.Variadic", `"absent"`)
	mustRefuse[tcraVariadicIn, tcraOut](t, variadic("changes", "name"), "CLI.Variadic", "CLI.Positional")

	echo := admissionOp(uniqueID("tcra.actions.update"))
	echo.CompletesAtAdmission = false
	echo.CLI.EchoWarnings = true
	mustRefuse[struct{}, struct{}](t, echo, "CLI.EchoWarnings", "warnings")
}

// TC-RA-ERR40 (CON-3): CompletesAtAdmission is refused wherever one of its
// rules fails.
func TestTCRAERR40_CompletesAtAdmissionIsRefusedWhereverARule(t *testing.T) {
	cases := []struct {
		name   string
		modify func(*Op)
		rule   string
	}{
		{"read-only risk", func(o *Op) { o.Risk = RiskReadOnly }, "mutating"},
		{"sync", func(o *Op) { o.Mode = ModeSync }, "ModeLongRunning"},
		{"KindList", func(o *Op) { o.Kind, o.ResourceType, o.Action = KindList, "configs", "" }, "KindResourceAction or KindProviderAction"},
		{"KindUpload with CLI.Upload", func(o *Op) {
			o.Kind, o.ResourceType, o.Action = KindUpload, "uploads", ""
			o.CLI.Upload = &CLIUpload{Op: "tcra.uploads.add", Field: "uploadId", FileFlag: "file"}
		}, "KindResourceAction or KindProviderAction"},
		{"MCP set", func(o *Op) { o.MCP = &MCPBinding{Tool: "update_config", Description: "Update."} }, "MCP"},
		{"SyncMutation", func(o *Op) { o.SyncMutation = true }, "SyncMutation"},
		{"WhatIf", func(o *Op) { o.WhatIf = true }, "WhatIf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := admissionOp(uniqueID("tcra.actions.update-config"))
			tc.modify(&op)
			mustRefuse[updateIn, tcraOut](t, op, "CompletesAtAdmission", tc.rule)
		})
	}

	// CLI.Upload alone, on an otherwise valid kind.
	op := admissionOp(uniqueID("tcra.actions.update-config"))
	op.CLI.Upload = &CLIUpload{Op: "tcra.uploads.add", Field: "uploadId", FileFlag: "file"}
	mustRefuse[updateIn, tcraOut](t, op, "CompletesAtAdmission", "CLI.Upload")
}
