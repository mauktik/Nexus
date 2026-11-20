// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package spec_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// opRow is one row of the architecture's nginx operations table
// (planning/nginx-provider/architecture.md, Interfaces, Part B), with the
// flags the table states in its risk column.
type opRow struct {
	id        string
	kind      def.Kind
	route     string // Method and path under /v1
	scope     string
	risk      def.Risk
	mode      def.Mode
	cli       string // Noun, verb and the positional field, if any
	tool      string
	idem      bool
	untrusted bool
	whatIf    bool
	sensitive []string
}

// table is the operations table, row for row. Untrusted is set on the
// table's Untrusted rows and on every op whose result carries nginx -t
// output (testOutput), which is host-originated text (base FR-28).
var table = []opRow{
	{"nginx.service.list", def.KindList, "GET /providers/nginx/service", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginx get", "get_nginx_status", true, false, false, nil},
	{"nginx.actions.test", def.KindProviderAction, "POST /providers/nginx/actions/test", "nginx:read", def.RiskReadOnly, def.ModeLongRunning, "nginxconfig test", "test_nginx_config", true, true, false, nil},
	{"nginx.actions.start", def.KindProviderAction, "POST /providers/nginx/actions/start", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx start", "start_nginx", true, true, false, nil},
	{"nginx.actions.stop", def.KindProviderAction, "POST /providers/nginx/actions/stop", "nginx:write", def.RiskDestructive, def.ModeLongRunning, "nginx stop", "stop_nginx", true, true, false, nil},
	{"nginx.actions.restart", def.KindProviderAction, "POST /providers/nginx/actions/restart", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx restart", "restart_nginx", false, true, false, nil},
	{"nginx.actions.reload", def.KindProviderAction, "POST /providers/nginx/actions/reload", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx reload", "reload_nginx", false, true, false, nil},
	{"nginx.sites.list", def.KindList, "GET /providers/nginx/sites", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsite get", "list_nginx_sites", true, true, false, nil},
	{"nginx.sites.get", def.KindGet, "GET /providers/nginx/sites/{name}", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsite get name", "get_nginx_site", true, true, false, nil},
	{"nginx.sites.content", def.KindSubresource, "GET /providers/nginx/sites/{name}/content", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsitecontent get name", "get_nginx_site_content", true, true, false, nil},
	{"nginx.sites.enable", def.KindResourceAction, "POST /providers/nginx/sites/{name}/enable", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginxsite start name", "enable_nginx_site", true, true, false, nil},
	{"nginx.sites.disable", def.KindResourceAction, "POST /providers/nginx/sites/{name}/disable", "nginx:write", def.RiskDestructive, def.ModeLongRunning, "nginxsite stop name", "disable_nginx_site", true, true, false, nil},
	{"nginx.sites.add", def.KindResourceAction, "POST /providers/nginx/sites/{name}/add", "nginx:config", def.RiskMutating, def.ModeLongRunning, "nginxsite add name", "add_nginx_site", false, true, true, []string{"content"}},
	{"nginx.sites.update", def.KindResourceAction, "POST /providers/nginx/sites/{name}/update", "nginx:config", def.RiskMutating, def.ModeLongRunning, "nginxsite update name", "update_nginx_site", false, true, true, []string{"content"}},
}

func rowOf(t *testing.T, op def.Op) opRow {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	cli := op.CLI.Noun + " " + op.CLI.Verb
	if op.CLI.Positional != "" {
		cli += " " + op.CLI.Positional
	}
	tool := ""
	if op.MCP != nil {
		tool = op.MCP.Tool
	}
	return opRow{
		id: op.ID, kind: op.Kind, route: method + " " + strings.TrimPrefix(path, "/v1"), scope: op.Scope.Name,
		risk: op.Risk, mode: op.Mode, cli: cli, tool: tool, idem: op.Idempotent, untrusted: op.Untrusted,
		whatIf: op.WhatIf, sensitive: op.Sensitive,
	}
}

// nginx-provider TC-56: the 13 nginx operations are declared exactly as the
// operations table. TestCatalogMatchesGoldenTable (internal/catalog) checks
// the golden rows and TestAllListsUniqueOps that nginx is listed once.
func TestOperationsMatchTable(t *testing.T) {
	s := spec.Spec()
	if s.Name != "nginx" || s.ReadScope != "nginx:read" {
		t.Errorf("spec = %q read scope %q, want nginx and nginx:read", s.Name, s.ReadScope)
	}
	if len(s.Ops) != len(table) {
		t.Fatalf("spec has %d ops, the table %d", len(s.Ops), len(table))
	}
	for i, op := range s.Ops {
		got, want := rowOf(t, op), table[i]
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i+1, got, want)
		}
		if op.Provider != spec.Name {
			t.Errorf("%s: provider %q", op.ID, op.Provider)
		}
		if op.Mode == def.ModeLongRunning && op.Cancel != def.CancelQueuedOnly {
			t.Errorf("%s: Cancel %q, want queued-only", op.ID, op.Cancel)
		}
		if op.Mutating() && op.Mode != def.ModeLongRunning {
			t.Errorf("%s: a mutation must be long-running", op.ID)
		}
		if op.CLI.WaitedResultItems {
			t.Errorf("%s: WaitedResultItems set", op.ID)
		}
		if op.Scope.PerTarget != "" || op.Scope.FromTarget {
			t.Errorf("%s: unexpected scope %+v", op.ID, op.Scope)
		}
	}
}

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

// nginx-provider TC-57: the input schemas constrain names, ifMatch, cursor
// and content, and add and update mark content sensitive.
func TestInputSchemaRefinements(t *testing.T) {
	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	named := 0
	for _, op := range spec.Spec().Ops {
		s := inputSchema(t, op)
		for name := range s.Properties {
			switch strings.ToLower(name) {
			case "path", "service", "command", "binary", "includedir":
				t.Errorf("%s: input property %q", op.ID, name)
			}
		}
		if p := s.Properties["name"]; p != nil {
			named++
			if p.Pattern != `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` || p.Not == nil || p.Not.Pattern != `\.\.` {
				t.Errorf("%s: name pattern %q", op.ID, p.Pattern)
			}
			for _, bad := range []string{"a..b", "..", "-x", ".hidden", "a/b", strings.Repeat("a", 129), ""} {
				if validates(t, p, bad) {
					t.Errorf("%s: name accepts %q", op.ID, bad)
				}
			}
			for _, good := range []string{"example.com", "a", "w1", "api_v2-site", strings.Repeat("a", 128)} {
				if !validates(t, p, good) {
					t.Errorf("%s: name refuses %q", op.ID, good)
				}
			}
			if !slices.Contains(s.Required, "name") {
				t.Errorf("%s: name not required", op.ID)
			}
		}
		if p := s.Properties["cursor"]; p != nil {
			if p.Pattern != spec.CursorPattern || p.MaxLength == nil || *p.MaxLength != 512 {
				t.Errorf("%s: cursor pattern %q max %v", op.ID, p.Pattern, p.MaxLength)
			}
			if validates(t, p, "-x") || validates(t, p, "a/b") || !validates(t, p, "ZXhhbXBsZS5jb20") {
				t.Errorf("%s: cursor bounds wrong", op.ID)
			}
		}
		if p := s.Properties["limit"]; p != nil {
			if p.Minimum == nil || *p.Minimum != 1 || p.Maximum == nil || *p.Maximum != 1000 {
				t.Errorf("%s: limit bounds %v..%v", op.ID, p.Minimum, p.Maximum)
			}
		}
	}
	if named != 6 {
		t.Errorf("%d ops take a name, want 6 (get, content, enable, disable, add, update)", named)
	}
	if s := inputSchema(t, opByID(t, "nginx.sites.list")); s.Properties["cursor"] == nil || s.Properties["limit"] == nil {
		t.Errorf("sites.list lacks limit or cursor: %v", s.Properties)
	}

	for _, id := range []string{"nginx.sites.add", "nginx.sites.update"} {
		op := opByID(t, id)
		s := inputSchema(t, op)
		c := s.Properties["content"]
		if c == nil {
			t.Fatalf("%s: no content", id)
		}
		if v, _ := c.Extra[def.FreeTextKeyword].(bool); !v {
			t.Errorf("%s: content is not marked free text", id)
		}
		if c.MaxLength == nil || *c.MaxLength != 1048576 || spec.MaxConfigBytesCeiling != 1048576 {
			t.Errorf("%s: content maxLength %v", id, c.MaxLength)
		}
		if !slices.Contains(s.Required, "content") {
			t.Errorf("%s: content not required", id)
		}
		if !slices.Equal(op.Sensitive, []string{"content"}) || !op.WhatIf {
			t.Errorf("%s: Sensitive %v WhatIf %v", id, op.Sensitive, op.WhatIf)
		}
		if s.Properties["whatIf"] == nil || s.Properties["enabled"] == nil {
			t.Errorf("%s: lacks whatIf or enabled", id)
		}
	}

	if p := inputSchema(t, opByID(t, "nginx.sites.add")).Properties["ifMatch"]; p != nil {
		t.Errorf("add has an ifMatch property")
	}
	p := inputSchema(t, opByID(t, "nginx.sites.update")).Properties["ifMatch"]
	if p == nil || p.Pattern != `^([0-9a-f]{64}|none)$` {
		t.Fatalf("update ifMatch = %+v", p)
	}
	for _, good := range []string{hex64, "none"} {
		if !validates(t, p, good) {
			t.Errorf("ifMatch refuses %q", good)
		}
	}
	for _, bad := range []string{strings.ToUpper(hex64), "*", "", hex64[:63], hex64 + "0", "None", "none\n"} {
		if validates(t, p, bad) {
			t.Errorf("ifMatch accepts %q", bad)
		}
	}
	if slices.Contains(inputSchema(t, opByID(t, "nginx.sites.update")).Required, "ifMatch") {
		t.Errorf("ifMatch is required; it is optional")
	}

	// Every other op takes none of the write fields.
	for _, op := range spec.Spec().Ops {
		if op.ID == "nginx.sites.add" || op.ID == "nginx.sites.update" {
			continue
		}
		if len(op.Sensitive) > 0 || op.WhatIf {
			t.Errorf("%s: Sensitive %v WhatIf %v", op.ID, op.Sensitive, op.WhatIf)
		}
	}
}

// TestWireTypesUseCamelCaseTags checks the explicit json tags of the wire
// types against the architecture's names, including the keys of the
// reload observation the nginx-reload-failed problem carries.
func TestWireTypesUseCamelCaseTags(t *testing.T) {
	pid, sha, valid := 7, "abc", true
	for _, c := range []struct {
		v    any
		keys []string
	}{
		{spec.ReloadObservation{}, []string{"masterPid", "oldWorkers", "newWorkers", "initExitCode", "verdict"}},
		{spec.ServiceProperties{MainPID: &pid}, []string{"state", "since", "mainPid", "version", "initSystem", "service", "includeDir", "includeSuffix", "disabledSuffix"}},
		{spec.SiteProperties{SHA256: &sha, UnmanagedReason: "symlink"}, []string{"enabled", "fileName", "managed", "fileType", "unmanagedReason", "conflict", "sizeBytes", "modTime", "sha256"}},
		{spec.SiteContent{}, []string{"id", "name", "fileName", "sha256", "sizeBytes", "content"}},
		{spec.TestResult{}, []string{"changed", "valid", "output", "outputTruncated"}},
		{spec.ServiceResult{Reload: &spec.ReloadObservation{}}, []string{"changed", "state", "mainPid", "testOutput", "reload"}},
		{spec.SiteToggleResult{Reload: &spec.ReloadObservation{}}, []string{"changed", "name", "fileName", "enabled", "reloaded", "testOutput", "reload"}},
		{spec.WriteResult{Valid: &valid, PreviousSHA256: "p", Reload: &spec.ReloadObservation{}}, []string{"changed", "name", "fileName", "sha256", "sizeBytes", "created", "enabled", "reloaded", "whatIf", "valid", "testOutput", "previousSha256", "reload"}},
		{spec.WriteInput{IfMatch: "none", Enabled: &valid, WhatIf: true}, []string{"name", "content", "ifMatch", "enabled", "whatIf"}},
		{spec.AddInput{Enabled: &valid, WhatIf: true}, []string{"name", "content", "enabled", "whatIf"}},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(m))
		for k := range m {
			got = append(got, k)
		}
		want := slices.Clone(c.keys)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%T keys = %v, want %v", c.v, got, want)
		}
	}
}

// nginx-provider TC-59: the MCP tools carry the right hints, add's and
// update's descriptions state root-equivalence, and each long-running op's
// description says it returns an operation ID. The budget itself is
// TestToolBudget (internal/mcpadapter), run on the full catalog.
func TestMCPHints(t *testing.T) {
	srv := startServer(t, "alpha")
	tools := srv.mcpTools(t)
	type hints struct{ readOnly, destructive, idempotent bool }
	want := map[string]hints{}
	for _, r := range table {
		want[r.tool] = hints{r.risk == def.RiskReadOnly, r.risk == def.RiskDestructive, r.idem}
	}
	for _, name := range []string{"stop_nginx", "disable_nginx_site"} {
		if !want[name].destructive {
			t.Errorf("%s is not destructive in the table", name)
		}
	}
	for _, name := range []string{"get_nginx_status", "list_nginx_sites", "get_nginx_site", "get_nginx_site_content", "test_nginx_config"} {
		if !want[name].readOnly {
			t.Errorf("%s is not read-only in the table", name)
		}
	}
	for name, w := range want {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("tool %s is not listed", name)
			continue
		}
		a := tool.Annotations
		if a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.IdempotentHint == nil {
			t.Errorf("%s: hints not all explicit: %+v", name, a)
			continue
		}
		if got := (hints{*a.ReadOnlyHint, *a.DestructiveHint, *a.IdempotentHint}); got != w {
			t.Errorf("%s: hints %+v, want %+v", name, got, w)
		}
	}

	for _, op := range spec.Spec().Ops {
		if op.MCP == nil {
			t.Fatalf("%s has no MCP tool", op.ID)
		}
		if n := len(op.MCP.Description); n > 120 {
			t.Errorf("%s: MCP description is %d characters; keep it short", op.ID, n)
		}
		if op.Mode == def.ModeLongRunning && !strings.Contains(op.Description, "operation ID") {
			t.Errorf("%s: Description does not say it returns an operation ID", op.ID)
		}
		if op.Scope.Name == spec.ConfigScope && !strings.Contains(op.Description, "root-equivalent") {
			t.Errorf("%s: Description does not state that nginx:config is root-equivalent", op.ID)
		}
		if op.Untrusted && !strings.Contains(op.Description, "untrusted") {
			t.Errorf("%s: Untrusted, but the Description does not say so", op.ID)
		}
	}
}

// mcpTool is one tools/list entry with its annotations.
type mcpTool struct {
	Name        string `json:"name"`
	Annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
	} `json:"annotations"`
}

// mcpTools lists the server's MCP tools with a raw tools/list.
func (s *server) mcpTools(t *testing.T) map[string]mcpTool {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
		`"io.modelcontextprotocol/clientInfo":{"name":"nginx-spec","version":"1"},` +
		`"io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequest(http.MethodPost, s.url+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msg struct {
		Result struct {
			Tools []mcpTool `json:"tools"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	out := map[string]mcpTool{}
	for _, tool := range msg.Result.Tools {
		out[tool.Name] = tool
	}
	return out
}
