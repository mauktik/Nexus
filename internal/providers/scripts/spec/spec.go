// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package spec declares the scripts provider's operations and wire types. It
// holds only data and types, so the CLI can import it through
// internal/catalog without linking the provider (CON-5, D-1).
//
// Scripts are defined only in the root-owned configuration file on the host.
// There is deliberately no operation that adds, changes or removes a script
// definition, because one would give its caller arbitrary code execution
// (FR-18, FR-25): the catalog can be listed, read and invoked, nothing else.
package spec

import (
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
)

// Provider identity, resource type and scopes.
const (
	Name         = "scripts"
	ResourceType = "scripts"
	ReadScope    = "scripts:read"
	RunScope     = "scripts:run"
	// RunTargetScope is the per-script scope, filled from the input's name.
	RunTargetScope = "scripts:run:{name}"
)

// IDPattern is what a catalog script ID must match. Its characters are those
// a per-script scope suffix (scripts:run:<id>) may hold, so every cataloged
// script can be granted on its own, and the ID is a safe route segment.
const IDPattern = `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`

// ParameterNamePattern is what a declared parameter name must match.
const ParameterNamePattern = `^[A-Za-z][A-Za-z0-9_]{0,63}$`

// MaxParameterValueLength bounds one parameter value, in bytes.
const MaxParameterValueLength = 4096

// List bounds, as for every list operation.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// CursorPattern matches the opaque cursors the list hands out: unpadded
// base64url of an ASCII script ID, whose first character is therefore never
// "-" or "_".
const CursorPattern = `^([A-Za-z0-9][A-Za-z0-9_-]*)?$`

const maxCursorLength = 512

// ListInput is the input of scripts.scripts.list.
type ListInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size: default 100, maximum 1000."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from a previous page's nextCursor."`
}

// ScriptInput names one cataloged script.
type ScriptInput struct {
	Name string `json:"name" jsonschema:"Script ID from the catalog."`
}

// InvokeInput is the input of scripts.scripts.invoke.
type InvokeInput struct {
	Name string `json:"name" jsonschema:"Script ID from the catalog."`
	// Parameters are validated against the script's declarations before
	// anything runs; an undeclared name is refused. A value reaches the
	// script as one argument or one environment variable, never through a
	// shell, so it is inert data.
	Parameters map[string]string `json:"parameters,omitempty" jsonschema:"Values of the script's declared parameters, by parameter name."`
}

// Script is one cataloged script as the API returns it. Its id is
// /providers/scripts/scripts/<script ID>.
type Script = apitypes.Resource[ScriptProperties]

// ScriptProperties is a script's catalog entry with its defaults applied.
// Environment variable values are never returned, only their names (NFR-4).
type ScriptProperties struct {
	Description             string      `json:"description"`
	Path                    string      `json:"path"` // Absolute path of the executable
	RunAs                   string      `json:"runAs"`
	WorkingDirectory        string      `json:"workingDirectory"`
	TimeoutSeconds          float64     `json:"timeoutSeconds"`
	Concurrency             int         `json:"concurrency"`
	TerminationGraceSeconds float64     `json:"terminationGraceSeconds"` // Between SIGTERM and SIGKILL
	Env                     []string    `json:"env"`                     // Names only, sorted
	Parameters              []Parameter `json:"parameters"`              // In declared order
}

// Parameter is one declared script parameter.
type Parameter struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Pattern is the regular expression a value must match, and Enum the
	// values it may take. Both are omitted for a sensitive parameter, whose
	// pattern or values may describe the secret.
	Pattern   string   `json:"pattern,omitempty"`
	Enum      []string `json:"enum,omitempty"`
	Sensitive bool     `json:"sensitive"`
	Arg       string   `json:"arg,omitempty"` // The value follows this argument
	Env       string   `json:"env,omitempty"` // The value is passed in this environment variable
	// Position is the 1-based position of a positional parameter, one with
	// neither arg nor env, among the positional arguments. Zero otherwise.
	Position int `json:"position,omitempty"`
}

// InvokeResult is the result of a script run.
type InvokeResult struct {
	apitypes.ActionResult
	ExitCode        int     `json:"exitCode"`
	Signal          string  `json:"signal"` // Signal that ended the script, if any
	DurationSeconds float64 `json:"durationSeconds"`
}

func refineList(s *jsonschema.Schema) {
	lo, hi := 1.0, float64(MaxLimit)
	if p := s.Properties["limit"]; p != nil {
		p.Minimum, p.Maximum = &lo, &hi
	}
	if p := s.Properties["cursor"]; p != nil {
		n := maxCursorLength
		p.Pattern, p.MaxLength = CursorPattern, &n
	}
}

func refineName(s *jsonschema.Schema) {
	if p := s.Properties["name"]; p != nil {
		p.Pattern = IDPattern
	}
}

func refineInvoke(s *jsonschema.Schema) {
	refineName(s)
	if p := s.Properties["parameters"]; p != nil {
		p.PropertyNames = &jsonschema.Schema{Pattern: ParameterNamePattern}
		if v := p.AdditionalProperties; v != nil {
			n := MaxParameterValueLength
			v.MaxLength = &n
			// Each value is checked against its parameter's declared type and
			// pattern before the run, and is passed on only as argv or env.
			def.MarkFreeText(v)
		}
	}
}

// The scripts operations, in the order of the Operation catalog table.
var (
	// scriptColumns are a script's table columns in the CLI.
	scriptColumns = []string{"name", "properties.description", "properties.runAs", "properties.timeoutSeconds"}

	List = def.Define[ListInput, apitypes.List[Script]](def.Op{
		ID: Name + ".scripts.list", Provider: Name, Kind: def.KindList, ResourceType: ResourceType,
		Summary: "List cataloged scripts",
		Description: "Lists the scripts cataloged in this host's nexusd configuration, sorted by ID, with each " +
			"script's description, executable, run-as user, working directory, timeout, concurrency limit, " +
			"termination grace and declared parameters. Environment variables are listed by name only; their " +
			"values are never returned. Read-only. Paged with limit and cursor. Scripts are defined only in " +
			"the configuration file on the host: no operation adds, changes or removes one.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:     def.CLIBinding{Noun: "script", Verb: "get"},
		MCP:     &def.MCPBinding{Tool: "list_scripts", Description: "List the scripts this host allows to be run."},
		Refine:  refineList,
		Columns: scriptColumns,
	})

	Get = def.Define[ScriptInput, Script](def.Op{
		ID: Name + ".scripts.get", Provider: Name, Kind: def.KindGet, ResourceType: ResourceType,
		Summary: "Get a cataloged script",
		Description: "Returns one cataloged script with its description, executable, run-as user, working " +
			"directory, timeout, concurrency limit, termination grace and declared parameters. Environment " +
			"variables are listed by name only; their values are never returned. Read-only.",
		Scope: def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
		CLI:     def.CLIBinding{Noun: "script", Verb: "get", Positional: "name"},
		MCP:     &def.MCPBinding{Tool: "get_script", Description: "Get one script's definition and parameters."},
		Refine:  refineName,
		Columns: scriptColumns,
	})

	Invoke = def.Define[InvokeInput, InvokeResult](def.Op{
		ID: Name + ".scripts.invoke", Provider: Name, Kind: def.KindResourceAction, ResourceType: ResourceType,
		Action:  "invoke",
		Summary: "Run a cataloged script",
		Description: "Runs a cataloged script as its configured run-as user with the given values of its " +
			"declared parameters. Side effects: whatever the script does on this host as its run-as user; " +
			"Nexus neither knows nor limits them. Only declared parameters are accepted, each checked against " +
			"its declaration first, and each value reaches the script as one argument or environment variable, " +
			"never through a shell. Long-running: answers 202 with an operation, whose result holds the exit " +
			"code; it runs detached from nexusd (a systemd unit, or a supervised process on OpenRC), so it " +
			"survives a restart of nexusd, and a running script can be canceled. Answers 409 when the script's " +
			"concurrency limit is reached. " +
			"Requires scripts:run, or scripts:run:<id> for this script.",
		Scope: def.Scope{Name: RunScope, PerTarget: RunTargetScope}, Risk: def.RiskDestructive,
		Mode: def.ModeLongRunning, Cancel: def.CancelRunning,
		CLI:    def.CLIBinding{Noun: "script", Verb: "invoke", Positional: "name"},
		MCP:    &def.MCPBinding{Tool: "run_script", Description: "Run a cataloged script with its declared parameters."},
		Refine: refineInvoke,
	})
)

// Spec returns the scripts provider's declarations for the catalog.
func Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: Name, ReadScope: ReadScope, Ops: []def.Op{
		List.Op(), Get.Op(), Invoke.Op(),
	}}
}
