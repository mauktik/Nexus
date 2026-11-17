package spec_test

import (
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// TC-SM-19: Spec lists exactly the seven ops of the Operation catalog table,
// with their kind, route segments, scope, risk, mode, CLI command and MCP
// tool, followed by the three token ops of nexusd-remote-admin's table
// (TC-RA-26).
func TestSpecListsTheOps(t *testing.T) {
	type row struct {
		id, resourceType, action string
		kind                     def.Kind
		scope                    string
		risk                     def.Risk
		mode                     def.Mode
		noun, verb, tool         string
	}
	want := []row{
		{"nexusd.daemons.list", "daemons", "", def.KindList, "nexusd:read", def.RiskReadOnly, def.ModeSync, "nexusd", "get", "get_nexusd"},
		{"nexusd.actions.validate-config", "", "validate-config", def.KindProviderAction, "nexusd:read", def.RiskReadOnly, def.ModeSync, "nexusd", "validate-config", "validate_nexusd_config"},
		{"nexusd.actions.reload", "", "reload", def.KindProviderAction, "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd", "reload", ""},
		{"nexusd.actions.restart", "", "restart", def.KindProviderAction, "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd", "restart", ""},
		{"nexusd.actions.update", "", "update", def.KindProviderAction, "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd", "update", ""},
		{"nexusd.actions.rollback", "", "rollback", def.KindProviderAction, "nexusd:admin", def.RiskDestructive, def.ModeLongRunning, "nexusd", "rollback", ""},
		{"nexusd.uploads.add", "uploads", "", def.KindUpload, "nexusd:admin", def.RiskMutating, def.ModeSync, "", "", ""},
		{"nexusd.tokens.list", "tokens", "", def.KindList, "nexusd:admin", def.RiskReadOnly, def.ModeSync, "nexusdtoken", "get", ""},
		{"nexusd.tokens.add", "tokens", "add", def.KindResourceAction, "nexusd:admin", def.RiskMutating, def.ModeSync, "nexusdtoken", "add", ""},
		{"nexusd.tokens.remove", "tokens", "remove", def.KindResourceAction, "nexusd:admin", def.RiskDestructive, def.ModeSync, "nexusdtoken", "remove", ""},
	}

	s := spec.Spec()
	if s.Name != "nexusd" || s.ReadScope != "nexusd:read" {
		t.Fatalf("spec name %q read scope %q", s.Name, s.ReadScope)
	}
	var got []row
	scopes := map[string]bool{}
	for _, op := range s.Ops {
		if op.Provider != spec.Name {
			t.Errorf("%s: provider %q", op.ID, op.Provider)
		}
		if op.Scope.PerTarget != "" || op.Scope.FromTarget {
			t.Errorf("%s: scope %+v, want only a name", op.ID, op.Scope)
		}
		tool := ""
		if op.MCP != nil {
			tool = op.MCP.Tool
		}
		got = append(got, row{op.ID, op.ResourceType, op.Action, op.Kind, op.Scope.Name, op.Risk, op.Mode,
			op.CLI.Noun, op.CLI.Verb, tool})
		scopes[op.Scope.Name] = true
	}
	if !slices.Equal(got, want) {
		t.Errorf("ops:\n got %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(scopes, map[string]bool{spec.ReadScope: true, spec.AdminScope: true}) {
		t.Errorf("scopes %v, want %s and %s", scopes, spec.ReadScope, spec.AdminScope)
	}

	consts := []struct {
		name     string
		got, exp any
	}{
		{"Name", spec.Name, "nexusd"},
		{"ReadScope", spec.ReadScope, "nexusd:read"},
		{"AdminScope", spec.AdminScope, "nexusd:admin"},
		{"DaemonsType", spec.DaemonsType, "daemons"},
		{"UploadsType", spec.UploadsType, "uploads"},
		{"TokensType", spec.TokensType, "tokens"},
		{"MaxRemoteTokens", spec.MaxRemoteTokens, 256},
		{"TokenMutationsPerMinute", spec.TokenMutationsPerMinute, 10},
		{"LaneKey", spec.LaneKey, "nexusd"},
		{"DefaultMaxUploadBytes", spec.DefaultMaxUploadBytes, 64 << 20},
		{"MaxUploadBytesCeiling", spec.MaxUploadBytesCeiling, 256 << 20},
		{"DefaultHealthTimeout", spec.DefaultHealthTimeout, 30 * time.Second},
		{"ReconnectWindow", spec.ReconnectWindow, 60 * time.Second},
	}
	for _, c := range consts {
		if c.got != c.exp {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.exp)
		}
	}

	// The reads are idempotent; a read op's output columns match the table.
	for _, op := range []def.Op{spec.List.Op(), spec.ValidateConfig.Op(), spec.TokensList.Op()} {
		if !op.Idempotent {
			t.Errorf("%s: not idempotent", op.ID)
		}
	}
	wantCols := []string{"name", "properties.version", "properties.uptimeSeconds", "properties.previous.version",
		"properties.updateConfigured"}
	if cols := spec.List.Op().Columns; !slices.Equal(cols, wantCols) {
		t.Errorf("get columns %q, want %q", cols, wantCols)
	}
}

// allowedImports are the only non-standard-library packages the spec
// package may import.
var allowedImports = []string{
	"github.com/mauktik/nexus/internal/def",
	"github.com/mauktik/nexus/internal/apitypes",
	"github.com/mauktik/nexus/internal/problem",
	"github.com/google/jsonschema-go/jsonschema",
}

// TC-SM-19 (step 2): the spec package imports only def, apitypes, problem
// and jsonschema beyond the standard library, the catalog import rule that
// internal/catalog's TestContractPackagesHaveNoHeavyDependencies enforces on
// its dependencies.
func TestImportsOnlyContractPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for imp := range strings.FieldsSeq(string(out)) {
		std := !strings.Contains(strings.SplitN(imp, "/", 2)[0], ".")
		if !std && !slices.Contains(allowedImports, imp) {
			t.Errorf("imports %s, want only %v and the standard library", imp, allowedImports)
		}
	}
}

func hasType(s *jsonschema.Schema, typ string) bool {
	return s.Type == typ || slices.Contains(s.Types, typ)
}

func visit(s *jsonschema.Schema, path string, f func(path string, s *jsonschema.Schema)) {
	if s == nil {
		return
	}
	f(path, s)
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		visit(s.Properties[name], path+"/"+name, f)
	}
	visit(s.Items, path+"/*", f)
	if a := s.AdditionalProperties; a != nil && a.Not == nil {
		visit(a, path+"/{key}", f)
	}
}

// forbiddenFieldParts are what no input field name may contain: a path,
// command, shell, exec or URL (CON-4, NSM FR-25).
var forbiddenFieldParts = []string{"path", "command", "shell", "exec", "url"}

// forbiddenWords mirrors internal/conformance's op ID and route words outside
// v1; the conformance suite applies it to the wired catalog (6.1). As there,
// allowedTokenOps alone may carry "token" and "tokens".
var forbiddenWords = []string{
	"create", "exec", "execute", "run", "shell", "reboot", "shutdown", "poweroff", "halt",
	"repository", "repositories", "repo", "repos", "sources", "key", "keys",
	"reconcile", "drift", "deploy", "deployment", "deployments", "template", "templates",
	"schedule", "schedules", "cron", "metrics", "self", "selfupdate", "token", "tokens",
}

var allowedTokenOps = []string{"nexusd.tokens.list", "nexusd.tokens.add", "nexusd.tokens.remove"}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '.' || r == '/' || r == '-' || r == '_' || r == '{' || r == '}'
	})
}

// route is op's REST route by its kind, as internal/openapi builds it.
func route(op def.Op) string {
	base := "/providers/" + op.Provider
	switch op.Kind {
	case def.KindProviderAction:
		return base + "/actions/" + op.Action
	default:
		return base + "/" + op.ResourceType
	}
}

// TC-SM-20: every string input is constrained, none names a path, command or
// URL, update takes exactly uploadId and allowDowngrade, and the schema
// refuses bad upload IDs and signatures.
func TestInputsAreConstrained(t *testing.T) {
	strs := 0
	for _, op := range spec.Spec().Ops {
		r, err := op.InputSchema()
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		visit(r.Schema(), "", func(path string, s *jsonschema.Schema) {
			if hasType(s, "string") {
				strs++
				if s.Pattern == "" && len(s.Enum) == 0 {
					t.Errorf("%s %s: string input without pattern or enum", op.ID, path)
				}
			}
			for name := range s.Properties {
				n := strings.ToLower(name)
				for _, part := range forbiddenFieldParts {
					if strings.Contains(n, part) {
						t.Errorf("%s: input field %s/%s contains %q", op.ID, path, name, part)
					}
				}
			}
		})
		for _, w := range slices.Concat(words(op.ID), words(route(op))) {
			if (w == "token" || w == "tokens") && slices.Contains(allowedTokenOps, op.ID) {
				continue
			}
			if slices.Contains(forbiddenWords, w) {
				t.Errorf("%s: forbidden word %q in op ID or route %s", op.ID, w, route(op))
			}
		}
	}
	if strs != 6 {
		t.Errorf("found %d string inputs, want 6 (uploadId, signature, token add's name, scopes[] and expires, "+
			"and token remove's name)", strs)
	}

	update := schemaOf(t, spec.Update.Op())
	var props []string
	for name := range update.Schema().Properties {
		props = append(props, name)
	}
	slices.Sort(props)
	if want := []string{"allowDowngrade", "uploadId"}; !slices.Equal(props, want) {
		t.Errorf("update input fields %q, want %q", props, want)
	}
	if p := update.Schema().Properties["uploadId"]; p == nil || p.Pattern != `^[0-9a-f]{32}$` {
		t.Errorf("uploadId schema %+v, want pattern ^[0-9a-f]{32}$", p)
	}

	upload := schemaOf(t, spec.Upload.Op())
	if p := upload.Schema().Properties["signature"]; p == nil || p.Pattern != `^[A-Za-z0-9+/]{86}==$` ||
		p.MaxLength == nil || *p.MaxLength != 88 {
		t.Errorf("signature schema %+v, want pattern ^[A-Za-z0-9+/]{86}==$ and maxLength 88", p)
	}

	// The input of every other op is empty and closed.
	for _, op := range []def.Op{spec.List.Op(), spec.ValidateConfig.Op(), spec.Reload.Op(), spec.Restart.Op(),
		spec.Rollback.Op()} {
		r := schemaOf(t, op)
		if err := r.Validate(map[string]any{}); err != nil {
			t.Errorf("%s: empty input refused: %v", op.ID, err)
		}
		if err := r.Validate(map[string]any{"path": "/etc/nexus/nexusd.yaml"}); err == nil {
			t.Errorf("%s: unknown field accepted", op.ID)
		}
	}

	id := "0123456789abcdef0123456789abcdef"
	if err := update.Validate(map[string]any{"uploadId": id}); err != nil {
		t.Errorf("valid uploadId refused: %v", err)
	}
	if err := update.Validate(map[string]any{"uploadId": id, "allowDowngrade": true}); err != nil {
		t.Errorf("valid uploadId with allowDowngrade refused: %v", err)
	}
	for name, v := range map[string]any{
		"uppercase hex": strings.ToUpper(id),
		"31 characters": id[:31],
		"33 characters": id + "0",
		"dot dot slash": "../x",
		"slash":         "a/b",
		"empty":         "",
	} {
		if err := update.Validate(map[string]any{"uploadId": v}); err == nil {
			t.Errorf("uploadId %s (%q) accepted", name, v)
		}
	}
	if err := update.Validate(map[string]any{}); err == nil {
		t.Error("update without uploadId accepted")
	}

	sig := strings.Repeat("Ab+/", 21) + "Cd" + "=="
	if len(sig) != 88 {
		t.Fatalf("fixture signature has %d characters", len(sig))
	}
	if err := upload.Validate(map[string]any{"signature": sig}); err != nil {
		t.Errorf("valid signature refused: %v", err)
	}
	for name, v := range map[string]string{
		"87 characters":     sig[:85] + "==",
		"89 characters":     sig[:87] + "A=",
		"89 with padding":   "A" + sig,
		"URL-safe alphabet": strings.Repeat("Ab-_", 21) + "Cd" + "==",
		"unpadded":          sig[:86],
		"newline":           sig + "\n",
	} {
		if err := upload.Validate(map[string]any{"signature": v}); err == nil {
			t.Errorf("signature %s (%q) accepted", name, v)
		}
	}
}

func schemaOf(t *testing.T, op def.Op) *jsonschema.Resolved {
	t.Helper()
	r, err := op.InputSchema()
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	return r
}

// TC-SM-21: restart, update and rollback declare a 60 s reconnect window and
// the others none; only update carries the CLI.Upload binding, which names
// the upload op; the lane ops are cancelable only while queued.
func TestReconnectWindowsAndUploadBinding(t *testing.T) {
	windows := map[string]time.Duration{
		"nexusd.actions.restart":  60 * time.Second,
		"nexusd.actions.update":   60 * time.Second,
		"nexusd.actions.rollback": 60 * time.Second,
	}
	for _, op := range spec.Spec().Ops {
		if got := op.CLI.ReconnectWindow; got != windows[op.ID] {
			t.Errorf("%s: CLI.ReconnectWindow %v, want %v", op.ID, got, windows[op.ID])
		}
		if op.ID != "nexusd.actions.update" && op.CLI.Upload != nil {
			t.Errorf("%s: CLI.Upload %+v, want none", op.ID, op.CLI.Upload)
		}
		if op.ID != "nexusd.uploads.add" && op.Upload != nil {
			t.Errorf("%s: Upload %+v, want none", op.ID, op.Upload)
		}
	}

	u := spec.Update.Op().CLI.Upload
	if u == nil {
		t.Fatal("update has no CLI.Upload")
	}
	got := *u
	got.FileUsage, got.Sidecars = "", slices.Clone(u.Sidecars)
	for i := range got.Sidecars {
		got.Sidecars[i].Usage = ""
	}
	want := def.CLIUpload{Op: "nexusd.uploads.add", Field: "uploadId", FileFlag: "file",
		Sidecars: []def.CLISidecar{{Param: "signature", Flag: "signature", Suffix: ".sig", MaxBytes: 4096}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CLI.Upload %+v, want %+v", got, want)
	}
	if u.FileUsage == "" || len(u.Sidecars) != 1 || u.Sidecars[0].Usage == "" {
		t.Errorf("CLI.Upload has no usage text: %+v", u)
	}
	// The binding names this spec's upload op, whose input has the sidecar's
	// field, and whose result has the field the update input takes.
	up := spec.Upload.Op()
	if u.Op != up.ID || up.Kind != def.KindUpload || up.Provider != spec.Name {
		t.Errorf("CLI.Upload.Op %q does not name the upload op %q", u.Op, up.ID)
	}
	if _, ok := schemaOf(t, up).Schema().Properties["signature"]; !ok {
		t.Error("upload input has no signature field")
	}
	if f, ok := up.OutputType().FieldByName("UploadID"); !ok || f.Tag.Get("json") != "uploadId" {
		t.Error("upload result has no uploadId field")
	}

	if up.Upload == nil || !reflect.DeepEqual(*up.Upload, def.Upload{
		ContentTypes: []string{"application/octet-stream", "application/gzip"}, MaxBytes: 256 << 20,
	}) {
		t.Errorf("upload declaration %+v", up.Upload)
	}
	if up.CLI != (def.CLIBinding{}) || up.MCP != nil {
		t.Errorf("upload op has CLI %+v MCP %+v, want neither", up.CLI, up.MCP)
	}

	for _, op := range []def.Op{spec.Reload.Op(), spec.Restart.Op(), spec.Update.Op(), spec.Rollback.Op()} {
		if op.Cancel != def.CancelQueuedOnly {
			t.Errorf("%s: cancel %q, want %q", op.ID, op.Cancel, def.CancelQueuedOnly)
		}
	}
}

// TC-SM-22: descriptions state side effects and long-running, restart, update
// and rollback state FR-11's effect on other providers' operations, and only
// the two reads are MCP tools, read-only, with descriptions of at most 120
// characters.
func TestDescriptions(t *testing.T) {
	for _, op := range spec.Spec().Ops {
		if strings.TrimSpace(op.Description) == "" || strings.TrimSpace(op.Summary) == "" {
			t.Errorf("%s: empty summary or description", op.ID)
		}
		if op.Mutating() && !strings.Contains(op.Description, "Side effects:") {
			t.Errorf("%s: mutating op does not state its side effects", op.ID)
		}
		if op.Mode == def.ModeLongRunning && !strings.Contains(op.Description, "Long-running") {
			t.Errorf("%s: long-running op does not say so", op.ID)
		}
		if op.Mode != def.ModeLongRunning && strings.Contains(strings.ToLower(op.Description), "long-running") {
			t.Errorf("%s: %s op says long-running", op.ID, op.Mode)
		}
	}
	for _, op := range []def.Op{spec.Restart.Op(), spec.Update.Op(), spec.Rollback.Op()} {
		for _, s := range []string{"in-process operations of other providers end outcome-unknown", "detached operations survive"} {
			if !strings.Contains(op.Description, s) {
				t.Errorf("%s: description does not say %q", op.ID, s)
			}
		}
	}
	for _, op := range spec.Spec().Ops {
		read := op.ID == "nexusd.daemons.list" || op.ID == "nexusd.actions.validate-config"
		switch {
		case !read && op.MCP != nil:
			t.Errorf("%s: mutating op is an MCP tool %q", op.ID, op.MCP.Tool)
		case read && op.MCP == nil:
			t.Errorf("%s: not an MCP tool", op.ID)
		case read:
			if n := len(op.MCP.Description); n == 0 || n > 120 {
				t.Errorf("%s: MCP description has %d characters, want 1..120", op.ID, n)
			}
			// readOnlyHint derives from the risk.
			if op.Risk != def.RiskReadOnly || op.Mutating() {
				t.Errorf("%s: risk %s, want read-only", op.ID, op.Risk)
			}
		}
	}
}
