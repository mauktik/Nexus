// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-1, TC-SV-2,
// TC-SV-3, TC-SV-ERR1, TC-SV-ERR2 and TC-SV-ERR3 (WI 1.1): the
// CompletesAcrossReboot and CLI.TypedConfirmation declarations, Audited
// widened to streams, and their validate rules.

package def

import (
	"slices"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

type tcsvRebootIn struct {
	ConfirmHost string `json:"confirmHost"`
	Force       bool   `json:"force,omitempty"`
	Note        string `json:"note,omitempty"`
	Label       string `json:"label,omitzero"`
}

// rebootOp is TC-SV-1's planted pl.actions.reboot: a long-running
// destructive provider action with a 10-minute reconnect window that
// completes across a reboot.
func rebootOp(id string) Op {
	return Op{
		ID:                    id,
		Provider:              "pl",
		Kind:                  KindProviderAction,
		Action:                "reboot",
		Summary:               "Reboot the host",
		Description:           "Reboots the host. Long-running: the operation completes after the reboot.",
		Scope:                 Scope{Name: "pl:reboot"},
		Risk:                  RiskDestructive,
		Mode:                  ModeLongRunning,
		Cancel:                CancelRunning,
		CompletesAcrossReboot: true,
		CLI:                   CLIBinding{Noun: "machine", Verb: "restart", ReconnectWindow: 10 * time.Minute},
	}
}

// confirmOp is TC-SV-2's planted op: destructive and long-running, with the
// operator typing confirmHost.
func confirmOp(id string) Op {
	op := rebootOp(id)
	op.CompletesAcrossReboot = false
	op.CLI = CLIBinding{Noun: "machine", Verb: "restart", TypedConfirmation: "confirmHost"}
	return op
}

// TC-SV-1 (FR-11, CON-4): CompletesAcrossReboot is accepted on a
// long-running destructive provider action and a mutating resource action,
// each with the maximum reconnect window.
func TestTCSV1_CompletesAcrossRebootIsAcceptedOnALongRunning(t *testing.T) {
	a := mustDefine[struct{}, struct{}](t, rebootOp(uniqueID("pl.actions.reboot")))

	b := rebootOp(uniqueID("pl.units.reboot"))
	b.Kind, b.ResourceType, b.Risk = KindResourceAction, "units", RiskMutating
	b.CLI.Positional = "name"
	bGot := mustDefine[struct{}, struct{}](t, b)

	for _, got := range []Op{a, bGot} {
		if !got.CompletesAcrossReboot {
			t.Errorf("op %q: CompletesAcrossReboot = false, want true", got.ID)
		}
		if got.CLI.ReconnectWindow != 10*time.Minute {
			t.Errorf("op %q: CLI.ReconnectWindow = %v, want 10m", got.ID, got.CLI.ReconnectWindow)
		}
	}
}

// TC-SV-2 (FR-10, FR-13, FR-15, CON-4): TypedConfirmation naming a required,
// non-positional string field of a mutating op is accepted, and the input
// schema lists that field as required.
func TestTCSV2_TypedConfirmationIsAcceptedOnARequiredNon(t *testing.T) {
	got := mustDefine[tcsvRebootIn, struct{}](t, confirmOp(uniqueID("pl.actions.reboot")))
	if got.CLI.TypedConfirmation != "confirmHost" {
		t.Errorf("CLI.TypedConfirmation = %q, want %q", got.CLI.TypedConfirmation, "confirmHost")
	}
	r, err := got.InputSchema()
	if err != nil {
		t.Fatalf("InputSchema: %v", err)
	}
	if req := r.Schema().Required; !slices.Contains(req, "confirmHost") {
		t.Errorf("input schema required = %v, want it to list confirmHost", req)
	}
}

// TC-SV-3 (FR-6, FR-16, CON-4): Audited is accepted on a read-only stream
// and still on a read-only sync op.
func TestTCSV3_AuditedIsAcceptedOnAReadOnly(t *testing.T) {
	stream := readOp(uniqueID("pl.units.logs"))
	stream.Mode, stream.Audited = ModeStream, true
	if got := mustDefine[struct{}, apitypes.Frame](t, stream); !got.Audited || got.Mode != ModeStream {
		t.Errorf("stream: Audited=%v Mode=%q, want true %q", got.Audited, got.Mode, ModeStream)
	}

	sync := readOp(uniqueID("pl.config.get"))
	sync.Audited = true
	if got := mustDefine[struct{}, struct{}](t, sync); !got.Audited || got.Mode != ModeSync {
		t.Errorf("sync: Audited=%v Mode=%q, want true %q", got.Audited, got.Mode, ModeSync)
	}
}

// TC-SV-ERR1 (FR-11, CON-4): each CompletesAcrossReboot rule refuses the
// TC-SV-1 op varied one field at a time, naming the declaration and the rule.
func TestTCSVERR1_CompletesAcrossRebootRefusalsNameTheirRule(t *testing.T) {
	cases := []struct {
		name string
		vary func(*Op)
		rule string
	}{
		{"ModeSync", func(op *Op) { op.Mode = ModeSync }, "requires ModeLongRunning"},
		{"ModeStream", func(op *Op) { op.Mode = ModeStream }, "requires ModeLongRunning"},
		{"RiskReadOnly", func(op *Op) { op.Risk = RiskReadOnly }, "requires a mutating op"},
		{"KindList", func(op *Op) { op.Kind = KindList }, "requires KindResourceAction or KindProviderAction"},
		{"KindGet", func(op *Op) { op.Kind = KindGet }, "requires KindResourceAction or KindProviderAction"},
		{"KindSubresource", func(op *Op) { op.Kind = KindSubresource }, "requires KindResourceAction or KindProviderAction"},
		{"ReconnectWindow 0", func(op *Op) { op.CLI.ReconnectWindow = 0 }, "requires CLI.ReconnectWindow > 0"},
		{"SyncMutation", func(op *Op) { op.SyncMutation = true }, "cannot be set with SyncMutation"},
		{"CompletesAtAdmission", func(op *Op) { op.CompletesAtAdmission = true }, "cannot be set with CompletesAtAdmission"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := rebootOp(uniqueID("pl.actions.reboot"))
			c.vary(&op)
			mustRefuse[struct{}, apitypes.Frame](t, op, "CompletesAcrossReboot", c.rule)
		})
	}
}

// TC-SV-ERR2 (FR-10, CON-4): each TypedConfirmation rule refuses the TC-SV-2
// op varied, naming the declaration and the rule.
func TestTCSVERR2_TypedConfirmationRefusalsNameTheirRule(t *testing.T) {
	cases := []struct {
		name string
		vary func(*Op)
		rule string
	}{
		{"unknown field", func(op *Op) { op.CLI.TypedConfirmation = "nope" }, "unknown field"},
		{"bool field", func(op *Op) { op.CLI.TypedConfirmation = "force" }, "not a string field"},
		{"omitempty field", func(op *Op) { op.CLI.TypedConfirmation = "note" }, "not required"},
		{"omitzero field", func(op *Op) { op.CLI.TypedConfirmation = "label" }, "not required"},
		{"positional field", func(op *Op) { op.CLI.Positional = "confirmHost" }, "CLI.Positional"},
		{"read-only op", func(op *Op) { op.Risk = RiskReadOnly }, "requires a mutating op"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := confirmOp(uniqueID("pl.actions.reboot"))
			c.vary(&op)
			mustRefuse[tcsvRebootIn, struct{}](t, op, "TypedConfirmation", c.rule)
		})
	}
}

// TC-SV-ERR3 (FR-16, CON-4): Audited is refused on a mutating stream and on a
// long-running read.
func TestTCSVERR3_AuditedOnAMutatingStreamOrA(t *testing.T) {
	mutating := readOp(uniqueID("pl.units.follow"))
	mutating.Mode, mutating.Risk, mutating.Audited = ModeStream, RiskMutating, true
	mustRefuse[struct{}, apitypes.Frame](t, mutating, "Audited requires RiskReadOnly")

	longRunning := readOp(uniqueID("pl.actions.scan"))
	longRunning.Mode, longRunning.Audited = ModeLongRunning, true
	mustRefuse[struct{}, struct{}](t, longRunning, "Audited requires ModeSync or ModeStream")
}
