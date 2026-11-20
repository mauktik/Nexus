// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-ERR42 (WI 1.3).

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
)

// widgetConfigure is op C: a detached op that may complete at admission.
var widgetConfigure = def.Define[widgetDeployIn, widgetDeployOut](func() def.Op {
	op := baseOp("widgets.widgets.configure")
	op.Kind, op.Action = def.KindResourceAction, "configure"
	op.Scope = def.Scope{Name: "widgets:write"}
	op.Risk, op.Mode, op.CompletesAtAdmission = def.RiskMutating, def.ModeLongRunning, true
	op.CLI.Verb = "update"
	op.CLI.Noun = "widgetconf"
	return op
}())

func deploySpec() DetachedSpec[widgetDeployIn, widgetDeployOut] {
	return DetachedSpec[widgetDeployIn, widgetDeployOut]{
		Prepare: func(context.Context, widgetDeployIn, *Run) (detach.Spec, error) {
			return detach.Spec{Argv: []string{"/bin/true"}, KillMode: detach.KillProcess, MaxRuntime: time.Hour}, nil
		},
		Finalize: func(context.Context, widgetDeployIn, detach.Exit, *Run) (widgetDeployOut, error) {
			return widgetDeployOut{}, nil
		},
	}
}

func admitWidget(context.Context, widgetDeployIn) error { return nil }

// admissionWidget answers Done for version "same", else the input with
// Version rewritten, and refuses version "bad".
func admissionWidget(_ context.Context, in widgetDeployIn) (Admitted[widgetDeployIn, widgetDeployOut], error) {
	switch in.Version {
	case "bad":
		return Admitted[widgetDeployIn, widgetDeployOut]{}, errors.New("refused by admission")
	case "same":
		return Admitted[widgetDeployIn, widgetDeployOut]{Done: true, Out: widgetDeployOut{ExitCode: 7}}, nil
	}
	in.Version = "admitted-" + in.Version
	return Admitted[widgetDeployIn, widgetDeployOut]{In: in}, nil
}

// TC-RA-ERR42 (CON-3): provider.Detached panics on Admit plus Admission, and
// on CompletesAtAdmission without Admission.
func TestTCRAERR42_ProviderDetachedPanicsOnAdmitPlusAdmissionAnd(t *testing.T) {
	both := deploySpec()
	both.Admit, both.Admission = admitWidget, admissionWidget
	noHook := deploySpec()
	noHook.Admit = admitWidget
	cases := map[string]struct {
		f    func()
		want []string
	}{
		"Admit plus Admission": {func() { Detached(widgetConfigure, both) },
			[]string{"widgets.widgets.configure", "Admit", "Admission"}},
		"CompletesAtAdmission without Admission": {func() { Detached(widgetConfigure, noHook) },
			[]string{"widgets.widgets.configure", "CompletesAtAdmission", "Admission"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("did not panic")
				}
				for _, w := range c.want {
					if !strings.Contains(fmtAny(r), w) {
						t.Errorf("panic %v does not name %q", r, w)
					}
				}
			}()
			c.f()
		})
	}

	// A hook on an undeclared op is allowed; its Done is refused at run time.
	withHook := deploySpec()
	withHook.Admission = admissionWidget
	b := Detached(widgetDeploy, withHook)
	df, _ := b.Detached()
	if df.Admission == nil {
		t.Fatal("erased Admission is nil for a spec with Admission")
	}
	ctx := context.Background()
	in, err := b.Decode(json.RawMessage(`{"name":"w1","version":"same"}`))
	if err != nil {
		t.Fatal(err)
	}
	done, out, next, err := df.Admission(ctx, in)
	if err != nil || !done || next != nil || out.(widgetDeployOut).ExitCode != 7 {
		t.Fatalf("Admission(same) = %v, %#v, %#v, %v; want Done with the hook's output", done, out, next, err)
	}
	in, _ = b.Decode(json.RawMessage(`{"name":"w1","version":"2"}`))
	done, out, next, err = df.Admission(ctx, in)
	if err != nil || done || out != nil || next.(widgetDeployIn) != (widgetDeployIn{Name: "w1", Version: "admitted-2"}) {
		t.Fatalf("Admission(2) = %v, %#v, %#v, %v; want the hook's input", done, out, next, err)
	}
	in, _ = b.Decode(json.RawMessage(`{"name":"w1","version":"bad"}`))
	if _, _, _, err := df.Admission(ctx, in); err == nil || err.Error() != "refused by admission" {
		t.Fatalf("Admission(bad) error = %v, want the hook's error", err)
	}
	if _, _, _, err := df.Admission(ctx, widgetGetIn{Name: "w1"}); err == nil {
		t.Fatal("Admission accepted an input of the wrong type")
	}

	// A declared op with Admission and no Admit is accepted.
	declared := deploySpec()
	declared.Admission = admissionWidget
	if df, _ := Detached(widgetConfigure, declared).Detached(); df.Admission == nil {
		t.Fatal("erased Admission is nil for op C")
	}

	// The erased Admission is nil for a spec without one.
	if df, _ := Detached(widgetDeploy, deploySpec()).Detached(); df.Admission != nil {
		t.Fatal("erased Admission is set for a spec without Admission")
	}
}
