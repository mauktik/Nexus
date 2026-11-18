// Tests of planning/services-provider/test-cases.md TC-SV-7, TC-SV-ERR5
// (WI 1.2): the AfterReboot hook of a detached binding and the declaration
// and hook pairing that Detached enforces.

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
)

type tcsvRebootIn struct {
	Delay int `json:"delay"`
}

type tcsvRebootOut struct {
	Prev string `json:"prev"`
	Cur  string `json:"cur"`
}

// tcsvRebootOp is TC-SV-1's op: a long-running destructive provider action
// with a reconnect window. across sets CompletesAcrossReboot.
func tcsvRebootOp(id string, across bool) def.Op {
	return def.Op{
		ID:                    id,
		Provider:              "pl",
		Kind:                  def.KindProviderAction,
		Action:                "reboot",
		Summary:               "Reboot the host",
		Description:           "Reboots the host. Long-running: the operation completes after the reboot.",
		Scope:                 def.Scope{Name: "pl:reboot"},
		Risk:                  def.RiskDestructive,
		Mode:                  def.ModeLongRunning,
		Cancel:                def.CancelRunning,
		CompletesAcrossReboot: across,
		CLI:                   def.CLIBinding{Noun: "machine", Verb: "restart", ReconnectWindow: 10 * time.Minute},
	}
}

var (
	tcsvDeclaring = def.Define[tcsvRebootIn, tcsvRebootOut](tcsvRebootOp("pl.actions.reboot", true))
	tcsvPlain     = def.Define[tcsvRebootIn, tcsvRebootOut](tcsvRebootOp("pl.actions.plainreboot", false))
)

func tcsvPrepare(context.Context, tcsvRebootIn, *Run) (detach.Spec, error) {
	return detach.Spec{}, nil
}

func tcsvFinalize(context.Context, tcsvRebootIn, detach.Exit, *Run) (tcsvRebootOut, error) {
	return tcsvRebootOut{}, nil
}

// TC-SV-7 (FR-11, CON-4): Detached accepts a declaring op with AfterReboot,
// and the type-erased hook decodes and returns.
func TestTCSV7_DetachedAcceptsADeclaringOpWithAfterReboot(t *testing.T) {
	const prev, cur = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	var got tcsvRebootIn
	calls := 0
	f := func(_ context.Context, in tcsvRebootIn, rb Rebooted, _ *Run) (tcsvRebootOut, error) {
		calls++
		got = in
		return tcsvRebootOut{Prev: rb.PreviousBootID, Cur: rb.BootID}, nil
	}
	b := Detached(tcsvDeclaring, DetachedSpec[tcsvRebootIn, tcsvRebootOut]{
		Prepare: tcsvPrepare, Finalize: tcsvFinalize, AfterReboot: f,
	})
	df, ok := b.Detached()
	if !ok {
		t.Fatal("binding is not detached")
	}
	if df.AfterReboot == nil {
		t.Fatal("erased AfterReboot is nil for a spec with AfterReboot")
	}
	in, err := b.Decode(json.RawMessage(`{"delay":7}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out, err := df.AfterReboot(context.Background(), in, Rebooted{PreviousBootID: prev, BootID: cur}, NewRun("op-1", nil))
	if err != nil {
		t.Fatalf("AfterReboot: %v", err)
	}
	if calls != 1 || got != (tcsvRebootIn{Delay: 7}) {
		t.Errorf("f got %+v in %d calls, want {Delay:7} in 1", got, calls)
	}
	if want := any(tcsvRebootOut{Prev: prev, Cur: cur}); out != want {
		t.Errorf("result = %#v, want %#v", out, want)
	}
	if _, err := df.AfterReboot(context.Background(), "not the input type", Rebooted{}, NewRun("op-1", nil)); err == nil {
		t.Error("erased AfterReboot accepted an input of the wrong type")
	}

	plain := Detached(tcsvPlain, DetachedSpec[tcsvRebootIn, tcsvRebootOut]{Prepare: tcsvPrepare, Finalize: tcsvFinalize})
	pf, _ := plain.Detached()
	if pf.AfterReboot != nil {
		t.Error("erased AfterReboot is set for an op without the flag and without AfterReboot")
	}
}

// TC-SV-ERR5 (FR-11, CON-4): Detached panics on a declaration and hook
// mismatch.
func TestTCSVERR5_DetachedPanicsOnADeclarationAndHook(t *testing.T) {
	after := func(context.Context, tcsvRebootIn, Rebooted, *Run) (tcsvRebootOut, error) {
		return tcsvRebootOut{}, nil
	}
	cases := map[string]struct {
		op   def.Typed[tcsvRebootIn, tcsvRebootOut]
		spec DetachedSpec[tcsvRebootIn, tcsvRebootOut]
	}{
		"declares without AfterReboot": {
			op:   tcsvDeclaring,
			spec: DetachedSpec[tcsvRebootIn, tcsvRebootOut]{Prepare: tcsvPrepare, Finalize: tcsvFinalize},
		},
		"AfterReboot without the declaration": {
			op:   tcsvPlain,
			spec: DetachedSpec[tcsvRebootIn, tcsvRebootOut]{Prepare: tcsvPrepare, Finalize: tcsvFinalize, AfterReboot: after},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("did not panic")
				}
				msg := fmtAny(r)
				for _, want := range []string{c.op.Op().ID, "CompletesAcrossReboot", "AfterReboot"} {
					if !strings.Contains(msg, want) {
						t.Errorf("panic %q does not name %q", msg, want)
					}
				}
			}()
			Detached(c.op, c.spec)
		})
	}
}
