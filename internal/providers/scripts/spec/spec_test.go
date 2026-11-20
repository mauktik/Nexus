// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package spec_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// TC-90 (step 1): the only op that is not a read is scripts.scripts.invoke,
// and no op changes a script definition: no add, change or remove action,
// no route other than list, get and invoke, and invoke's input names a
// cataloged script rather than describing one.
func TestNoOpChangesTheCatalog(t *testing.T) {
	s := spec.Spec()
	if s.Name != spec.Name || s.ReadScope != spec.ReadScope {
		t.Fatalf("spec %q read scope %q", s.Name, s.ReadScope)
	}
	var ids, mutating []string
	for _, op := range s.Ops {
		ids = append(ids, op.ID)
		if op.Provider != spec.Name {
			t.Errorf("%s: provider %q", op.ID, op.Provider)
		}
		if op.Mutating() {
			mutating = append(mutating, op.ID)
		}
	}
	if want := []string{"scripts.scripts.list", "scripts.scripts.get", "scripts.scripts.invoke"}; !slices.Equal(ids, want) {
		t.Fatalf("ops %q, want %q", ids, want)
	}
	if want := []string{"scripts.scripts.invoke"}; !slices.Equal(mutating, want) {
		t.Fatalf("non-read ops %q, want only %q", mutating, want)
	}

	inv := spec.Invoke.Op()
	if inv.Kind != def.KindResourceAction || inv.Action != "invoke" {
		t.Errorf("invoke is %s %q, want a resource action named invoke", inv.Kind, inv.Action)
	}
	// Invoke's input names a script and gives parameter values; it carries
	// nothing that could define or change one.
	r, err := inv.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	var props []string
	for name := range r.Schema().Properties {
		props = append(props, name)
	}
	slices.Sort(props)
	if want := []string{"name", "parameters"}; !slices.Equal(props, want) {
		t.Errorf("invoke input fields %q, want %q", props, want)
	}

	for _, op := range s.Ops {
		if op.Mutating() {
			continue
		}
		if op.Risk != def.RiskReadOnly || op.Mode != def.ModeSync || !op.Idempotent {
			t.Errorf("%s: read op is %s %s idempotent=%v", op.ID, op.Risk, op.Mode, op.Idempotent)
		}
		if op.Scope.Name != spec.ReadScope {
			t.Errorf("%s: scope %q, want %q", op.ID, op.Scope.Name, spec.ReadScope)
		}
	}
}

// Invoke's scope, cancel policy and description are what the Operation
// catalog states, and the reads say env values are never returned.
func TestInvokeDeclaration(t *testing.T) {
	op := spec.Invoke.Op()
	if op.Scope != (def.Scope{Name: "scripts:run", PerTarget: "scripts:run:{name}"}) {
		t.Errorf("scope %+v", op.Scope)
	}
	if op.Risk != def.RiskDestructive || op.Mode != def.ModeLongRunning || op.Cancel != def.CancelRunning {
		t.Errorf("risk %s mode %s cancel %s", op.Risk, op.Mode, op.Cancel)
	}
	if op.CLI != (def.CLIBinding{Noun: "script", Verb: "invoke", Positional: "name"}) || op.MCP == nil || op.MCP.Tool != "run_script" {
		t.Errorf("CLI %+v MCP %+v", op.CLI, op.MCP)
	}
	if !strings.Contains(op.Description, "Side effects:") || !strings.Contains(op.Description, "Long-running") {
		t.Errorf("description does not state side effects and long-running: %q", op.Description)
	}
	for _, read := range []def.Op{spec.List.Op(), spec.Get.Op()} {
		if !strings.Contains(read.Description, "values are never returned") {
			t.Errorf("%s: description does not say env values are never returned", read.ID)
		}
	}
}

// The schema accepts a well-formed invocation and refuses a bad script ID, a
// bad parameter name and an oversize value before any handler runs.
func TestInvokeInputSchema(t *testing.T) {
	r, err := spec.Invoke.Op().InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]any{"name": "deploy-api", "parameters": map[string]any{"branch": "main; rm -rf /"}}
	if err := r.Validate(ok); err != nil {
		t.Fatalf("valid input refused: %v", err)
	}
	bad := map[string]map[string]any{
		"script ID with a slash": {"name": "../x"},
		"parameter name":         {"name": "x", "parameters": map[string]any{"-rf": "a"}},
		"oversize value":         {"name": "x", "parameters": map[string]any{"a": strings.Repeat("A", spec.MaxParameterValueLength+1)}},
		"non-string value":       {"name": "x", "parameters": map[string]any{"a": 1.0}},
		"unknown field":          {"name": "x", "path": "/bin/sh"},
	}
	for name, in := range bad {
		if err := r.Validate(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
