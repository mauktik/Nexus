// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-34, TC-SV-35,
// TC-SV-36. Owner WI: 2.2.

package spec_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

func inputSchema(t *testing.T, op def.Op) *jsonschema.Schema {
	t.Helper()
	r, err := op.InputSchema()
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	return r.Schema()
}

// validates reports whether the property schema p accepts v.
func validates(t *testing.T, p *jsonschema.Schema, v any) bool {
	t.Helper()
	r, err := p.CloneSchemas().Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.Validate(v) == nil
}

func opByID(t *testing.T, id string) def.Op {
	t.Helper()
	for _, op := range spec.Spec().Ops {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("no op %s", id)
	return def.Op{}
}

func prop(t *testing.T, op def.Op, name string) *jsonschema.Schema {
	t.Helper()
	p := inputSchema(t, op).Properties[name]
	if p == nil {
		t.Fatalf("%s: no input %q", op.ID, name)
	}
	return p
}

func enumOf(p *jsonschema.Schema) []string {
	var out []string
	for _, v := range p.Enum {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func checkRange(t *testing.T, op def.Op, name string, lo, hi float64) {
	t.Helper()
	p := prop(t, op, name)
	if p.Minimum == nil || *p.Minimum != lo || p.Maximum == nil || *p.Maximum != hi {
		t.Errorf("%s: %s bounds %v..%v, want %v..%v", op.ID, name, p.Minimum, p.Maximum, lo, hi)
	}
	for _, v := range []float64{lo - 1, hi + 1} {
		if validates(t, p, v) {
			t.Errorf("%s: %s accepts %v", op.ID, name, v)
		}
	}
	for _, v := range []float64{lo, hi} {
		if !validates(t, p, v) {
			t.Errorf("%s: %s refuses %v", op.ID, name, v)
		}
	}
}

// Every op validates (Define panics at init otherwise), has a resolvable
// input schema, and the copied constants stay equal to svcctl's.
func TestOpsValidateAndCopiesMatchSvcctl(t *testing.T) {
	ops := spec.Spec().Ops
	if len(ops) != 8 {
		t.Fatalf("spec has %d ops, want 8", len(ops))
	}
	for _, op := range ops {
		if op.InputType() == nil {
			t.Errorf("%s: not built by def.Define", op.ID)
		}
		inputSchema(t, op)
		if _, err := op.OutputSchema(); err != nil {
			t.Errorf("%s: output schema: %v", op.ID, err)
		}
	}
	if spec.ServiceNamePattern != svcctl.ServiceNamePattern {
		t.Errorf("ServiceNamePattern %q, svcctl's %q", spec.ServiceNamePattern, svcctl.ServiceNamePattern)
	}
	want := []string{string(svcctl.Running), string(svcctl.Stopped), string(svcctl.Failed),
		string(svcctl.Starting), string(svcctl.Stopping), string(svcctl.Unknown)}
	if !slices.Equal(spec.States, want) {
		t.Errorf("States %v, svcctl's %v", spec.States, want)
	}
}

// TC-SV-34: Protected matches exact names and glob prefixes, case-sensitively. Owner WI 2.2. Requirements: FR-9.
func TestTCSV34_ProtectedMatchesExactNamesAndGlobPrefixes(t *testing.T) {
	cases := []struct {
		list  []string
		label string
		yes   map[string]string // name -> the entry it matches
		no    []string
	}{
		{
			spec.ProtectedSystemd, "systemd",
			map[string]string{
				"ssh": "ssh", "sshd": "sshd", "dbus": "dbus", "getty@tty1": "getty@*",
				"serial-getty@ttyS0": "serial-getty@*", "nexus-op-01JABC": "nexus-op-*", "nexusd": "nexusd",
				"NetworkManager": "NetworkManager",
			},
			[]string{"SSH", "sshdx", "gettyx", "nexus-opx", "nexusd2", "networkmanager", "cron"},
		},
		{
			spec.ProtectedOpenRC, "openrc",
			map[string]string{
				"sshd": "sshd", "dropbear": "dropbear", "networking": "networking", "syslog": "syslog",
				"udev": "udev", "devfs": "devfs", "dmesg": "dmesg", "localmount": "localmount", "nexusd": "nexusd",
			},
			[]string{"ssh", "nginx"},
		},
	}
	for _, c := range cases {
		for name, entry := range c.yes {
			ok, reason := spec.Protected(name, c.list)
			if !ok {
				t.Errorf("%s: %q not protected", c.label, name)
			}
			if reason != entry {
				t.Errorf("%s: %q reason %q, want the entry %q", c.label, name, reason, entry)
			}
		}
		for _, name := range c.no {
			if ok, reason := spec.Protected(name, c.list); ok || reason != "" {
				t.Errorf("%s: %q protected (%q)", c.label, name, reason)
			}
		}
	}
}

// TC-SV-35: Schema refinements bound every input. Owner WI 2.2. Requirements: FR-5, FR-6, FR-10, NFR-5, A-42.
func TestTCSV35_SchemaRefinementsBoundEveryInput(t *testing.T) {
	named := 0
	for _, op := range spec.Spec().Ops {
		p := inputSchema(t, op).Properties["name"]
		if p == nil {
			continue
		}
		named++
		if p.Pattern != svcctl.ServiceNamePattern || p.MaxLength == nil || *p.MaxLength != 64 {
			t.Errorf("%s: name pattern %q maxLength %v", op.ID, p.Pattern, p.MaxLength)
		}
		if p.Not == nil || p.Not.Pattern != `\.service$` {
			t.Errorf("%s: name has no not-pattern \\.service$: %+v", op.ID, p.Not)
		}
		for _, bad := range []string{"", "-x", "a b", "a/b", "../x", "a;b", "x.service", strings.Repeat("a", 65)} {
			if validates(t, p, bad) {
				t.Errorf("%s: name accepts %q", op.ID, bad)
			}
		}
		for _, good := range []string{"nginx", "nx-svc-test", "getty@tty1", "nx-a", "a.b_c", "x.service2", strings.Repeat("a", 64)} {
			if !validates(t, p, good) {
				t.Errorf("%s: name refuses %q", op.ID, good)
			}
		}
		if !slices.Contains(inputSchema(t, op).Required, "name") {
			t.Errorf("%s: name not required", op.ID)
		}
	}
	if named != 6 {
		t.Errorf("%d ops take a name, want 6 (all but list and reboot)", named)
	}

	logs := opByID(t, "services.services.logs")
	checkRange(t, logs, "tail", 1, 1000)
	since := prop(t, logs, "since")
	if since.MaxLength == nil || *since.MaxLength != 40 {
		t.Errorf("since maxLength %v, want 40", since.MaxLength)
	}
	for _, good := range []string{"2026-11-18T07:36:00Z", "2026-11-18T07:36:00.5+01:00", "90m", "168h"} {
		if !validates(t, since, good) {
			t.Errorf("since refuses %q", good)
		}
	}
	for _, bad := range []string{"7d", "yesterday", "", strings.Repeat("1", 40) + "h"} {
		if validates(t, since, bad) {
			t.Errorf("since accepts %q", bad)
		}
	}
	if got := enumOf(prop(t, logs, "priority")); !slices.Equal(got,
		[]string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}) {
		t.Errorf("priority enum %v", got)
	}

	reboot := opByID(t, "services.actions.reboot")
	checkRange(t, reboot, "delaySeconds", 5, 300)
	ch := prop(t, reboot, "confirmHost")
	if ch.MaxLength == nil || *ch.MaxLength != 253 {
		t.Errorf("confirmHost maxLength %v, want 253", ch.MaxLength)
	}
	if !slices.Contains(inputSchema(t, reboot).Required, "confirmHost") {
		t.Error("confirmHost not required")
	}
	if spec.ConfirmHostPattern != `^[^\x00-\x1f\x7f]+$` {
		t.Errorf("ConfirmHostPattern %q", spec.ConfirmHostPattern)
	}
	if ch.Pattern != spec.ConfirmHostPattern {
		t.Errorf("confirmHost pattern %q", ch.Pattern)
	}
	if _, free := ch.Extra[def.FreeTextKeyword]; free {
		t.Error("confirmHost is marked free text; it must carry ConfirmHostPattern instead")
	}
	for _, bad := range []string{"h1\n", "h1\x00", "h1\x1b[31m", "h1\x7f", "", strings.Repeat("h", 254)} {
		if validates(t, ch, bad) {
			t.Errorf("confirmHost accepts %q", bad)
		}
	}
	for _, good := range []string{"h1", "web-01.lan", "Home_Server", "home-server", strings.Repeat("h", 253)} {
		if !validates(t, ch, good) {
			t.Errorf("confirmHost refuses %q", good)
		}
	}

	control := opByID(t, "services.services.control")
	if got := enumOf(prop(t, control, "action")); !slices.Equal(got, []string{"start", "stop", "restart"}) {
		t.Errorf("action enum %v", got)
	}
	if !slices.Contains(inputSchema(t, control).Required, "action") {
		t.Error("action not required")
	}

	list := opByID(t, "services.services.list")
	if got := enumOf(prop(t, list, "state")); !slices.Equal(got,
		[]string{"running", "stopped", "failed", "starting", "stopping", "unknown"}) {
		t.Errorf("state enum %v", got)
	}
	if got := enumOf(prop(t, list, "enabled")); !slices.Equal(got,
		[]string{"enabled", "disabled", "static", "masked", "indirect", "generated", "alias", "unknown"}) {
		t.Errorf("enabled enum %v", got)
	}
	if got := enumOf(prop(t, list, "controllable")); !slices.Equal(got, []string{"true", "false"}) {
		t.Errorf("controllable enum %v", got)
	}
	checkRange(t, list, "limit", 1, 1000)
	if c := prop(t, list, "cursor"); c.Pattern == "" || c.MaxLength == nil {
		t.Errorf("cursor unbounded: pattern %q maxLength %v", c.Pattern, c.MaxLength)
	}
	if req := inputSchema(t, list).Required; len(req) != 0 {
		t.Errorf("list requires %v, want no required input", req)
	}
}

// TC-SV-36: No input is named host, MCP descriptions are verbatim, and the reboot has no MCP tool. Owner WI 2.2. Requirements: FR-14, FR-15, NFR-6, ASM-6.
func TestTCSV36_NoInputIsNamedHostMCPDescriptions(t *testing.T) {
	for _, op := range spec.Spec().Ops {
		for name := range inputSchema(t, op).Properties {
			if strings.EqualFold(name, "host") {
				t.Errorf("%s: input field %q", op.ID, name)
			}
		}
	}

	want := map[string]string{
		"services.services.list":    "List system services and their state.",
		"services.services.get":     "Get one system service's state.",
		"services.services.logs":    "Read a service's recent log lines.",
		"services.services.control": "Start, stop or restart an allow-listed service.",
	}
	tools := 0
	for _, op := range spec.Spec().Ops {
		if op.MCP == nil {
			continue
		}
		tools++
		if d, ok := want[op.ID]; !ok || op.MCP.Description != d {
			t.Errorf("%s: MCP description %q, want %q", op.ID, op.MCP.Description, d)
		}
	}
	if tools != 4 {
		t.Errorf("%d MCP tools, want 4", tools)
	}

	reboot := opByID(t, "services.actions.reboot")
	if reboot.MCP != nil {
		t.Errorf("reboot has an MCP binding %+v", reboot.MCP)
	}
	for _, s := range []string{"confirmHost", "providers.services.reboot.enabled", "enabled at boot",
		"queued or running", "force", "configuration must be valid", "delaySeconds", "cancel", "not an MCP tool"} {
		if !strings.Contains(reboot.Description, s) {
			t.Errorf("reboot description lacks %q", s)
		}
	}
}
