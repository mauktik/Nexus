// Stub-style: SKIP-only (nexusd-remote-admin)
//
// Stubs of planning/nexusd-remote-admin/test-cases.md TC-RA-26, TC-RA-27, TC-RA-60, TC-RA-61, TC-RA-67, TC-RA-68, TC-RA-E7, TC-RA-E17.
// Owner WIs: 2.2, 3.3, 4.1. Each stub compiles today and skips; the developer of
// the owning work item replaces the skip and the comment with the test,
// keeping the function name so that grep TestTCRA traces it. An
// un-skipped passing test is the GREEN signal
// (planning/nexusd-remote-admin/test-strategy.md, RED Stub Style).

package spec_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// TC-RA-26: The three token ops are declared as the catalog table states and match the golden rows. Owner WI 2.2. Requirements: FR-1, FR-2, FR-3, FR-10, FR-11, ASM-1, ASM-2.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.tokens.list`, `nexusd.tokens.add` and `nexusd.tokens.remove`.
//   - Expected:
//   - Routes: `GET /providers/nexusd/tokens`, `POST /providers/nexusd/tokens/{name}/add` and `POST .../{name}/remove`.
//   - All three: scope `nexusd:admin`, `MCP == nil`, CLI noun `nexusdtoken`.
//   - list: read-only, `Audited`, verb `get`, columns `name`, `properties.tokenId`, `properties.scopes`, `properties.expires`, `properties.self`.
//   - add: mutating, `ModeSync`, `SyncMutation`, `SecretOutput == "token"`, verb `add`, positional `name`, columns `name`, `tokenId`, `token`, `expires`.
//   - remove: destructive, `SyncMutation`, `CLI.EchoWarnings`, verb `remove`, positional `name`.
//   - Order: the ops appear after `nexusd.uploads.add` in `Spec()` order.
//   - Descriptions: each contains "nexusd:admin" and "root-equivalent". add's also contains "returned once".
//   - Catalog: the golden table has these three rows, and its row count has risen by 3.
func TestTCRA26_TheThreeTokenOpsAreDeclaredAsTheCatalogTable(t *testing.T) {
	type row struct {
		id, route string
		risk      def.Risk
		mode      def.Mode
		verb, pos string
		sync      bool // SyncMutation
		audited   bool
		secret    string
		echo      bool // CLI.EchoWarnings
	}
	want := []row{
		{"nexusd.tokens.list", "GET /v1/providers/nexusd/tokens", def.RiskReadOnly, def.ModeSync, "get", "", false, true, "", false},
		{"nexusd.tokens.add", "POST /v1/providers/nexusd/tokens/{name}/add", def.RiskMutating, def.ModeSync, "add", "name", true, false, "token", false},
		{"nexusd.tokens.remove", "POST /v1/providers/nexusd/tokens/{name}/remove", def.RiskDestructive, def.ModeSync, "remove", "name", true, false, "", true},
	}
	ops := spec.Spec().Ops
	upload := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == "nexusd.uploads.add" })
	if upload < 0 {
		t.Fatal("Spec() has no nexusd.uploads.add")
	}
	for i, w := range want {
		at := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == w.id })
		if at < 0 {
			t.Errorf("Spec() has no %s", w.id)
			continue
		}
		if at != upload+1+i {
			t.Errorf("%s is op %d of Spec(), want %d (after nexusd.uploads.add)", w.id, at, upload+1+i)
		}
		op := ops[at]
		method, path, err := openapi.Route(op)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		got := row{op.ID, method + " " + path, op.Risk, op.Mode, op.CLI.Verb, op.CLI.Positional, op.SyncMutation,
			op.Audited, op.SecretOutput, op.CLI.EchoWarnings}
		if got != w {
			t.Errorf("%s:\n got %+v\nwant %+v", op.ID, got, w)
		}
		if op.Scope != (def.Scope{Name: spec.AdminScope}) || op.MCP != nil || op.CLI.Noun != "nexusdtoken" {
			t.Errorf("%s: scope %+v, MCP %+v, noun %q; want nexusd:admin, none, nexusdtoken", op.ID, op.Scope, op.MCP, op.CLI.Noun)
		}
		for _, s := range []string{"nexusd:admin", "root-equivalent"} {
			if !strings.Contains(op.Description, s) {
				t.Errorf("%s: description lacks %q", op.ID, s)
			}
		}
	}

	if got, want := spec.TokensList.Op().Columns, []string{"name", "properties.tokenId", "properties.scopes",
		"properties.expires", "properties.self"}; !slices.Equal(got, want) {
		t.Errorf("list columns %q, want %q", got, want)
	}
	add := spec.TokensAdd.Op()
	if got, want := add.Columns, []string{"name", "tokenId", "token", "expires"}; !slices.Equal(got, want) {
		t.Errorf("add columns %q, want %q", got, want)
	}
	if !add.Mutating() || !strings.Contains(add.Description, "returned once") {
		t.Errorf("add: mutating %t, description %q; want mutating and \"returned once\"", add.Mutating(), add.Description)
	}
	if !spec.TokensRemove.Op().Mutating() {
		t.Error("remove is not mutating")
	}
	// The catalog rows and their count are TestCatalogMatchesGoldenTable's
	// (internal/catalog/golden_test.go).
}

// authRegex returns the pattern of the regexp.MustCompile literal that
// internal/auth assigns to name, read from its source: auth keeps the
// regexes unexported, and the spec package must not import auth.
func authRegex(t *testing.T, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "..", "auth", "tokens.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pattern := ""
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != name || len(vs.Values) != 1 {
			return true
		}
		call, ok := vs.Values[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok {
			if pattern, err = strconv.Unquote(lit.Value); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		return false
	})
	if pattern == "" {
		t.Fatalf("internal/auth/tokens.go has no %s = regexp.MustCompile(<literal>)", name)
	}
	return pattern
}

func tokenAddSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	return schemaOf(t, spec.TokensAdd.Op())
}

// TC-RA-27: Token name and scope patterns equal the auth package's regexes. Owner WI 2.2. Requirements: FR-1, NFR-5.
//
//   - Setup: The test imports `internal/auth` (test only) for its name and scope regex strings, which `auth` exports or exposes through a test accessor added by WI 2.1/2.2.
//   - Steps:
//     1. Compare the schema `pattern` strings of `name` and `scopes.items` with auth's.
//     2. Run an accept/reject table through both: `ci`, `a.b-c_d`, a 64-char name, a 65-char name, `-x`, `a b`, `abcdefabcdef` (a 12-hex ID), `containers:read`, `x:`, `X:read`.
//   - Expected: The strings are equal, and every table row gets the same verdict from both regexes. `go list -deps ./cmd/nexus` does not include `internal/auth`.
func TestTCRA27_TokenNameAndScopePatternsEqualTheAuthPackageS(t *testing.T) {
	r := tokenAddSchema(t)
	props := r.Schema().Properties
	nameP, scopes := props["name"], props["scopes"]
	if nameP == nil || scopes == nil || scopes.Items == nil {
		t.Fatalf("add input has no name or scopes items: %+v", props)
	}
	authName, authScope := authRegex(t, "nameRE"), authRegex(t, "scopeRE")
	if nameP.Pattern != authName {
		t.Errorf("name pattern %q, auth's nameRE %q", nameP.Pattern, authName)
	}
	if scopes.Items.Pattern != authScope {
		t.Errorf("scopes item pattern %q, auth's scopeRE %q", scopes.Items.Pattern, authScope)
	}
	nameRE, scopeRE := regexp.MustCompile(authName), regexp.MustCompile(authScope)

	names := []struct {
		v  string
		ok bool
	}{
		{"ci", true}, {"a.b-c_d", true}, {strings.Repeat("a.", 32), true}, {strings.Repeat("a", 65), false},
		{"-x", false}, {"a b", false}, {"abcdefabcdef", true},
	}
	for _, c := range names {
		schemaOK := r.Validate(map[string]any{"name": c.v}) == nil
		_, _, err := auth.Create(t.TempDir(), auth.CreateRequest{Name: c.v})
		if schemaOK != c.ok || nameRE.MatchString(c.v) != c.ok || (err == nil) != c.ok {
			t.Errorf("name %q: schema %t, auth regex %t, auth.Create err %v; want accepted %t",
				c.v, schemaOK, nameRE.MatchString(c.v), err, c.ok)
		}
	}
	scopeCases := []struct {
		v  string
		ok bool
	}{{"containers:read", true}, {"x:", false}, {"X:read", false}}
	for _, c := range scopeCases {
		schemaOK := r.Validate(map[string]any{"name": "ci", "scopes": []any{c.v}}) == nil
		_, _, err := auth.Create(t.TempDir(), auth.CreateRequest{Name: "ci", Scopes: []string{c.v}})
		if schemaOK != c.ok || scopeRE.MatchString(c.v) != c.ok || (err == nil) != c.ok {
			t.Errorf("scope %q: schema %t, auth regex %t, auth.Create err %v; want accepted %t",
				c.v, schemaOK, scopeRE.MatchString(c.v), err, c.ok)
		}
	}

	// The CLI builds without auth: the spec imports it in this test only.
	out, err := exec.Command("go", "list", "-deps", "github.com/mauktik/nexus/cmd/nexus").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if slices.Contains(strings.Fields(string(out)), "github.com/mauktik/nexus/internal/auth") {
		t.Error("cmd/nexus depends on internal/auth")
	}
}

// TC-RA-60: The config get ops are declared as the catalog table states. Owner WI 3.3. Requirements: FR-6, FR-10, FR-11, ASM-5, ASM-1.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.config.list` and `nexusd.config.get`.
//   - Expected:
//   - Routes: `GET /providers/nexusd/config` and `GET /providers/nexusd/config/{name}`.
//   - Declarations: scope `nexusd:admin`, read-only, sync, `Audited`, `MCP == nil`, noun `nexusdconfig`, verb `get` (one merged command), positional `name` on get.
//   - Columns: `name`, `properties.value`, `properties.effective`, `properties.source`.
//   - Descriptions: each states "on-disk", `[REDACTED]`, and "404" with "nearest" parent.
//   - Catalog: the golden table has 2 more rows, and `catalogStubs` binds both.
func TestTCRA60_TheConfigGetOpsAreDeclaredAsTheCatalogTable(t *testing.T) {
	want := []struct {
		id, route string
		kind      def.Kind
		pos       string
	}{
		{"nexusd.config.list", "GET /v1/providers/nexusd/config", def.KindList, ""},
		{"nexusd.config.get", "GET /v1/providers/nexusd/config/{name}", def.KindGet, "name"},
	}
	ops := spec.Spec().Ops
	tokens := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == "nexusd.tokens.remove" })
	if tokens < 0 {
		t.Fatal("Spec() has no nexusd.tokens.remove")
	}
	cols := []string{"name", "properties.value", "properties.effective", "properties.source"}
	for i, w := range want {
		at := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == w.id })
		if at < 0 {
			t.Errorf("Spec() has no %s", w.id)
			continue
		}
		if at != tokens+1+i {
			t.Errorf("%s is op %d of Spec(), want %d (after nexusd.tokens.remove)", w.id, at, tokens+1+i)
		}
		op := ops[at]
		method, path, err := openapi.Route(op)
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		if got := method + " " + path; got != w.route {
			t.Errorf("%s: route %q, want %q", op.ID, got, w.route)
		}
		if op.Kind != w.kind || op.ResourceType != spec.ConfigType || op.Scope != (def.Scope{Name: spec.AdminScope}) ||
			op.Risk != def.RiskReadOnly || op.Mutating() || op.Mode != def.ModeSync || !op.Audited || op.SyncMutation ||
			op.MCP != nil {
			t.Errorf("%s: kind %s, type %q, scope %+v, risk %s, mode %s, audited %t, sync mutation %t, MCP %+v; "+
				"want %s, config, nexusd:admin, read-only, sync, audited, no MCP", op.ID, op.Kind, op.ResourceType,
				op.Scope, op.Risk, op.Mode, op.Audited, op.SyncMutation, op.MCP, w.kind)
		}
		if !op.Idempotent {
			t.Errorf("%s: not idempotent", op.ID)
		}
		// One merged command: nexusdconfig get [NAME].
		if op.CLI.Noun != "nexusdconfig" || op.CLI.Verb != "get" || op.CLI.Positional != w.pos {
			t.Errorf("%s: CLI %+v, want nexusdconfig get with positional %q", op.ID, op.CLI, w.pos)
		}
		if !slices.Equal(op.Columns, cols) {
			t.Errorf("%s: columns %q, want %q", op.ID, op.Columns, cols)
		}
		for _, s := range []string{"on-disk", "not the configuration in force", "[REDACTED]", "404", "nearest",
			"nexusd:admin", "root-equivalent", "audited"} {
			if !strings.Contains(op.Description, s) {
				t.Errorf("%s: description lacks %q", op.ID, s)
			}
		}
	}
	if spec.ConfigType != "config" {
		t.Errorf("ConfigType = %q, want config", spec.ConfigType)
	}

	// The wire types: a key resource's properties are value, effective and
	// source, and value and effective are null when absent.
	b, err := json.Marshal(spec.ConfigKeyProperties{Source: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"value":null,"effective":null,"source":"default"}` {
		t.Errorf("ConfigKeyProperties JSON %s", got)
	}
	var key spec.ConfigKey = apitypes.NewResource("h", spec.Name, spec.ConfigType, "host.name",
		spec.ConfigKeyProperties{Value: "a", Effective: "a", Source: "file"})
	if key.Name != "host.name" || key.Properties.Source != "file" {
		t.Errorf("ConfigKey %+v", key)
	}
	// The catalog rows and their count are TestCatalogMatchesGoldenTable's
	// (internal/catalog/golden_test.go); catalogStubs binding both is
	// TestTCSM94_TheConformanceSuiteCoversNexusdThrough's (every nexusd op is
	// served).
}

// schemaPaths returns every key path of the config schema: each leaf of
// config.LeafKeys and each mapping, from the top level down.
func schemaPaths(t *testing.T) []string {
	t.Helper()
	var root *config.UnknownKeyError
	if _, err := config.LookupKey(""); !errors.As(err, &root) || len(root.Children) == 0 {
		t.Fatalf("LookupKey(\"\"): %v, want the top-level keys", err)
	}
	var paths []string
	var walk func(path string)
	walk = func(path string) {
		k, err := config.LookupKey(path)
		if err != nil {
			t.Fatalf("LookupKey(%q): %v", path, err)
		}
		paths = append(paths, path)
		if k.Kind == config.KindMapping {
			for _, c := range k.Children {
				walk(path + "." + c)
			}
		}
	}
	for _, c := range root.Children {
		walk(c)
	}
	for _, k := range config.LeafKeys() {
		if !slices.Contains(paths, k.Path) {
			t.Errorf("leaf %s is not under the top-level mappings", k.Path)
		}
	}
	return paths
}

// TC-RA-61: The config key-name pattern accepts every schema key and rejects others. Owner WI 3.3. Requirements: FR-6, NFR-5.
//
//   - Setup: `config.LeafKeys()` and mapping keys (test-only import).
//   - Steps:
//     1. Match each schema path against the `name` pattern.
//     2. Match the rejects: `Providers.x`, `a..b`, `.a`, `a.`, `a/b`, `../etc`, 9 segments, a 65-char segment, `a b`.
//   - Expected: Every schema path matches and every reject fails. `TestEveryStringInputIsConstrained` passes.
func TestTCRA61_TheConfigKeyNamePatternAcceptsEverySchemaKeyAnd(t *testing.T) {
	r := schemaOf(t, spec.ConfigGet.Op())
	p := r.Schema().Properties["name"]
	if p == nil || p.Pattern == "" {
		t.Fatalf("config get input has no name pattern: %+v", r.Schema().Properties)
	}
	re := regexp.MustCompile(p.Pattern)
	accepts := func(v string) (bool, bool) {
		return r.Validate(map[string]any{"name": v}) == nil, re.MatchString(v)
	}

	// Step 1: every schema path, leaf or mapping, is accepted.
	paths := schemaPaths(t)
	if len(paths) < len(config.LeafKeys()) || !slices.Contains(paths, "providers.host.enabled") ||
		!slices.Contains(paths, "providers") {
		t.Fatalf("schema paths %q: the walk missed keys", paths)
	}
	for _, path := range paths {
		if schemaOK, reOK := accepts(path); !schemaOK || !reOK {
			t.Errorf("schema key %q: schema %t, pattern %t; want accepted", path, schemaOK, reOK)
		}
	}
	// The bounds themselves: 8 segments and a 64-character segment.
	for _, v := range []string{strings.TrimSuffix(strings.Repeat("a.", 8), "."), "a" + strings.Repeat("B", 63)} {
		if schemaOK, reOK := accepts(v); !schemaOK || !reOK {
			t.Errorf("%q: schema %t, pattern %t; want accepted", v, schemaOK, reOK)
		}
	}

	// Step 2: the rejects.
	for _, v := range []string{
		"Providers.x", "a..b", ".a", "a.", "a/b", "../etc",
		strings.TrimSuffix(strings.Repeat("a.", 9), "."), "a" + strings.Repeat("b", 64), "a b",
		"", "providers.scripts.catalog.backup\n", "host.name=x",
	} {
		if schemaOK, reOK := accepts(v); schemaOK || reOK {
			t.Errorf("%q: schema %t, pattern %t; want rejected", v, schemaOK, reOK)
		}
	}
	// TestEveryStringInputIsConstrained (internal/conformance) and
	// TestInputsAreConstrained cover the name input with the others.
}

// TC-RA-67: The config set op is declared as the catalog table states. Owner WI 4.1. Requirements: FR-7, FR-9, FR-10, FR-11, NFR-5, ASM-1.
//
//   - Setup: `spec.Spec()`.
//   - Steps:
//     1. Find `nexusd.actions.update-config`.
//     2. Build its generated input JSON schema.
//     3. `json.Marshal(spec.ConfigUpdateInput{Changes: []string{"a=b"}, BaseSHA256: "x"})`.
//   - Expected:
//   - Declaration: `KindProviderAction`, action `update-config`, route `POST /providers/nexusd/actions/update-config`, mutating, long-running, `CancelQueuedOnly`, `CompletesAtAdmission` (D-7), scope `nexusd:admin`, `MCP == nil`.
//   - CLI: `{Noun: nexusdconfig, Verb: update, Variadic: changes, EchoWarnings: true}`.
//   - `changes` schema: `minItems` 1, `maxItems` `spec.MaxConfigChanges`, item `maxLength` `MaxConfigValueBytes + 520`.
//   - No baseline on the wire: the input schema has no `baseSha256` property (or any property other than `changes`), and step 3's JSON is exactly `{"changes":["a=b"]}`: `BaseSHA256` is tagged `json:"-"` (D-7, A-15).
//   - Wire types: `DaemonProperties` has `configSet` (json).
//   - Description: it names `config-set-unavailable`, `config-apply-failed`, `config-restore-failed`, `restartRequired`, the backup path, and `200` with `changed false` and no operation when nothing changes.
//   - Lane: the `LaneKey` doc mentions config set.
//   - Catalog: one more golden row, and `catalogStubs` has a `testprovider.InProcess` entry (which has no Admission hook; only `provider.Detached` requires one).
func TestTCRA67_TheConfigSetOpIsDeclaredAsTheCatalogTableStates(t *testing.T) {
	// Step 1: the declaration, right after nexusd.config.get.
	ops := spec.Spec().Ops
	get := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == "nexusd.config.get" })
	at := slices.IndexFunc(ops, func(op def.Op) bool { return op.ID == "nexusd.actions.update-config" })
	if at < 0 || get < 0 {
		t.Fatalf("Spec() has no nexusd.actions.update-config (%d) or nexusd.config.get (%d)", at, get)
	}
	if at != get+1 {
		t.Errorf("update-config is op %d of Spec(), want %d (after nexusd.config.get)", at, get+1)
	}
	op := ops[at]
	if op.ID != spec.UpdateConfig.Op().ID {
		t.Errorf("spec.UpdateConfig is %s", spec.UpdateConfig.Op().ID)
	}
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	if got := method + " " + path; got != "POST /v1/providers/nexusd/actions/update-config" {
		t.Errorf("route %q", got)
	}
	if op.Kind != def.KindProviderAction || op.Action != "update-config" || op.ResourceType != "" ||
		op.Risk != def.RiskMutating || !op.Mutating() || op.Mode != def.ModeLongRunning ||
		op.Cancel != def.CancelQueuedOnly || !op.CompletesAtAdmission || op.SyncMutation || op.Audited ||
		op.Scope != (def.Scope{Name: spec.AdminScope}) || op.MCP != nil {
		t.Errorf("declaration %+v; want a mutating long-running provider action update-config, cancelable "+
			"while queued, CompletesAtAdmission, scope nexusd:admin, no MCP", op)
	}
	if want := (def.CLIBinding{Noun: "nexusdconfig", Verb: "update", Variadic: "changes", EchoWarnings: true}); op.CLI != want {
		t.Errorf("CLI %+v, want %+v", op.CLI, want)
	}

	// Step 2: the generated input schema is changes alone, bounded.
	r := schemaOf(t, op)
	var props []string
	for name := range r.Schema().Properties {
		props = append(props, name)
	}
	if !slices.Equal(props, []string{"changes"}) {
		t.Errorf("input properties %q, want only changes (no baseSha256)", props)
	}
	ch := r.Schema().Properties["changes"]
	if ch == nil || ch.MinItems == nil || *ch.MinItems != 1 || ch.MaxItems == nil ||
		*ch.MaxItems != spec.MaxConfigChanges || ch.Items == nil || ch.Items.Pattern == "" ||
		ch.Items.MaxLength == nil || *ch.Items.MaxLength != spec.MaxConfigValueBytes+520 {
		t.Fatalf("changes schema %+v, want minItems 1, maxItems %d, an item pattern and item maxLength %d",
			ch, spec.MaxConfigChanges, spec.MaxConfigValueBytes+520)
	}
	if spec.MaxConfigChanges != 32 {
		t.Errorf("MaxConfigChanges = %d, want 32", spec.MaxConfigChanges)
	}
	if err := r.Validate(map[string]any{"changes": []any{"a=b"}, "baseSha256": "x"}); err == nil {
		t.Error("an input carrying baseSha256 was accepted")
	}

	// Step 3: the baseline never reaches the wire.
	b, err := json.Marshal(spec.ConfigUpdateInput{Changes: []string{"a=b"}, BaseSHA256: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"changes":["a=b"]}` {
		t.Errorf("ConfigUpdateInput JSON %s, want {\"changes\":[\"a=b\"]}", b)
	}
	if f, ok := reflect.TypeFor[spec.ConfigUpdateInput]().FieldByName("BaseSHA256"); !ok || f.Tag.Get("json") != "-" {
		t.Errorf("BaseSHA256 field %+v, want json:\"-\"", f)
	}

	// The wire types.
	if f, ok := reflect.TypeFor[spec.DaemonProperties]().FieldByName("ConfigSet"); !ok ||
		f.Tag.Get("json") != "configSet" || f.Type != reflect.TypeFor[spec.Availability]() {
		t.Errorf("DaemonProperties.ConfigSet %+v, want an Availability tagged configSet", f)
	}
	res, err := json.Marshal(spec.ConfigUpdateResult{
		Changes: []spec.ConfigChange{{Key: "host.name", Old: "a", New: "b"}}, RestartRequired: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res), `{"changed":false,"changes":[{"key":"host.name","old":"a","new":"b"}],`+
		`"restartRequired":[],"restored":false}`; got != want {
		t.Errorf("ConfigUpdateResult JSON\n got %s\nwant %s", got, want)
	}
	if f, ok := reflect.TypeFor[spec.ConfigUpdateResult]().FieldByName("BackupPath"); !ok ||
		f.Tag.Get("json") != "backupPath,omitempty" {
		t.Errorf("ConfigUpdateResult.BackupPath %+v, want json backupPath,omitempty", f)
	}
	if op.OutputType() != reflect.TypeFor[spec.ConfigUpdateResult]() {
		t.Errorf("output type %v, want spec.ConfigUpdateResult", op.OutputType())
	}

	// The description.
	for _, s := range []string{"422 config-invalid", "unknown", "cannot be set remotely", "list or a mapping",
		"wrong type", "config-set-unavailable", "OpenRC", "200 with changed false", "no operation",
		"Side effects:", "never restarts", "restartRequired", "backupPath", ".bak", "config-apply-failed",
		"restored true", "config-restore-failed", "Long-running", "nexusd:admin", "root-equivalent"} {
		if !strings.Contains(op.Description, s) {
			t.Errorf("description lacks %q", s)
		}
	}

	// The lane: every lane op's description and the LaneKey doc name config set.
	for _, lane := range []def.Op{spec.Reload.Op(), spec.Restart.Op(), spec.Update.Op(), spec.Rollback.Op(), op} {
		if !strings.Contains(lane.Description, "rollback and config set share one lane") {
			t.Errorf("%s: description does not say config set shares the lane", lane.ID)
		}
	}
	if doc := laneKeyDoc(t); !strings.Contains(doc, "config set") {
		t.Errorf("LaneKey doc %q does not mention config set", doc)
	}
	// The golden row and its count are TestCatalogMatchesGoldenTable's and
	// TestTCHP104's (internal/catalog); the catalogStubs InProcess entry is
	// TestTCSM94_TheConformanceSuiteCoversNexusdThrough's (every nexusd op
	// is served).
}

// laneKeyDoc returns the doc comment of spec.LaneKey, read from spec.go.
func laneKeyDoc(t *testing.T) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "spec.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			if len(vs.Names) == 1 && vs.Names[0].Name == "LaneKey" {
				if vs.Doc != nil {
					return vs.Doc.Text()
				}
				return g.Doc.Text()
			}
		}
	}
	t.Fatal("spec.go declares no LaneKey")
	return ""
}

// TC-RA-68: The deny-list keys exist in the schema, and the table holds the FR-12 entries and every path-valued key. Owner WI 4.1. Requirements: FR-12, ASM-3, NFR-5.
//
//   - Setup: None. FR-12 as amended by the user decision of 2026-11-17 (`requirements.md` § User decisions): every key whose value is a file path, directory, file-name suffix or service name is on the deny-list. `providers.nginx.disabledSuffix` is included by analogy with `includeSuffix` (A-35, the autonomous default, awaiting confirmation). The test's list of reviewed settable strings is exactly `host.name` and `tls.mode` (D-10, A-13).
//   - Steps:
//     1. Run `config.LookupKey(d.Key)` for each `spec.ConfigDenyList` entry.
//     2. Walk `config.LeafKeys()` and take every key of kind `KindString`.
//   - Expected:
//   - Every key is found, and every entry has a non-empty `Reason`.
//   - The table holds exactly these 12 entries, in this order:
//   - the three original FR-12 entries: `providers.nexusd.update.trustedKeys`, `providers.nexusd.enabled` (with `Value: "false"`) and `providers.scripts.catalog` (with `Subtree: true`);
//   - the nine path-, file-name- and service-valued keys: `tls.certFile`, `tls.keyFile`, `providers.docker.socket`, `providers.docker.configDir`, `providers.nginx.includeDir`, `providers.nginx.includeSuffix`, `providers.nginx.disabledSuffix`, `providers.nginx.service` and `providers.pihole.dnsFile`, each with `Value: ""` (any value), `Subtree: false`, and the one shared path reason (each of the nine `Reason` strings is equal).
//   - String-key classification (drift guard, A-13): every `KindString` key from step 2 is either on `spec.ConfigDenyList` or in the reviewed settable list, and the settable remainder is exactly `{host.name, tls.mode}`. A string key added to the schema later fails this test until someone classifies it. The failure message names the unclassified key.
//   - `listen` is not in the table. It is refused as a list (OOS-3, TC-RA-ERR28), by the user decision that `listen` is not settable remotely.
//   - `spec.LockoutKeyPrefixes` is `["tls.", "auth."]`, with no `listen` entry.
func TestTCRA68_TheDenyListKeysExistInTheSchemaAndTheTableHolds(t *testing.T) {
	const pathReason = "its value names a file, directory or service that nexus uses as root; it changes only by " +
		"editing the file"
	type entry struct {
		key, value string
		subtree    bool
	}
	want := []entry{
		{"providers.nexusd.update.trustedKeys", "", false},
		{"providers.nexusd.enabled", "false", false},
		{"providers.scripts.catalog", "", true},
	}
	paths := []string{"tls.certFile", "tls.keyFile", "providers.docker.socket", "providers.docker.configDir",
		"providers.nginx.includeDir", "providers.nginx.includeSuffix", "providers.nginx.disabledSuffix",
		"providers.nginx.service", "providers.pihole.dnsFile"}
	for _, k := range paths {
		want = append(want, entry{k, "", false})
	}
	var got []entry
	for i, d := range spec.ConfigDenyList {
		got = append(got, entry{d.Key, d.Value, d.Subtree})
		// Step 1: every key is in the schema, with a reason.
		if _, err := config.LookupKey(d.Key); err != nil {
			t.Errorf("deny-list key %s: %v", d.Key, err)
		}
		switch {
		case strings.TrimSpace(d.Reason) == "":
			t.Errorf("deny-list key %s has no reason", d.Key)
		case i < 3 && d.Reason == pathReason:
			t.Errorf("deny-list key %s has the path reason, want its own", d.Key)
		case i >= 3 && d.Reason != pathReason:
			t.Errorf("deny-list key %s reason %q, want the shared path reason %q", d.Key, d.Reason, pathReason)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("ConfigDenyList\n got %+v\nwant %+v", got, want)
	}

	// Step 2: every string key of the schema is classified (A-13).
	settable := []string{"host.name", "tls.mode"}
	var remainder []string
	strs := 0
	for _, k := range config.LeafKeys() {
		if k.Kind != config.KindString {
			continue
		}
		strs++
		denied := slices.ContainsFunc(spec.ConfigDenyList, func(d spec.ConfigDeny) bool {
			return d.Value == "" && (d.Key == k.Path || d.Subtree && strings.HasPrefix(k.Path, d.Key+"."))
		})
		switch {
		case denied:
		case slices.Contains(settable, k.Path):
			remainder = append(remainder, k.Path)
		default:
			t.Errorf("string key %s is neither on spec.ConfigDenyList nor a reviewed settable string: classify "+
				"it (deny it if its value names a file, directory, suffix or service; A-13)", k.Path)
		}
	}
	if !slices.Equal(remainder, settable) {
		t.Errorf("settable string keys %q, want exactly %q", remainder, settable)
	}
	if strs != len(paths)+len(settable) {
		t.Errorf("%d string keys in the schema, want %d (the nine denied and the two settable)", strs,
			len(paths)+len(settable))
	}

	// listen is refused as a list (OOS-3), not by the deny-list.
	if slices.ContainsFunc(spec.ConfigDenyList, func(d spec.ConfigDeny) bool { return d.Key == "listen" }) {
		t.Error("listen is on the deny-list; it is refused as a list")
	}
	if k, err := config.LookupKey("listen"); err != nil || k.Kind != config.KindList {
		t.Errorf("LookupKey(listen) = %+v, %v; want a list", k, err)
	}
	if !slices.Equal(spec.LockoutKeyPrefixes, []string{"tls.", "auth."}) {
		t.Errorf("LockoutKeyPrefixes %q, want [tls. auth.]", spec.LockoutKeyPrefixes)
	}
}

// TC-RA-E7: The expires pattern accepts the local command's forms and bounds its length. Owner WI 2.2. Requirements: FR-1, NFR-5.
//
//   - Setup: The schema of `TokenAddInput.expires`.
//   - Steps:
//     1. Match `90d`, `99999d`, `1h30m`, `1.5h`, `300ms`, `0d`, `100000d`, `-1h`, `1y`, `""`, and a 33-char valid duration.
//   - Expected:
//   - Accepted: `90d`, `99999d`, `1h30m`, `1.5h` and `300ms`. `""` is allowed by `omitempty`.
//   - Rejected: `0d`, `100000d`, `-1h`, `1y` and the 33-char value (`maxLength` 32).
func TestTCRAE7_TheExpiresPatternAcceptsTheLocalCommandSFormsAnd(t *testing.T) {
	r := tokenAddSchema(t)
	p := r.Schema().Properties["expires"]
	if p == nil || p.MaxLength == nil || *p.MaxLength != 32 {
		t.Fatalf("expires schema %+v, want maxLength 32", p)
	}
	long := strings.Repeat("1h", 15) + "10s"
	if len(long) != 33 || !regexp.MustCompile(p.Pattern).MatchString(long) {
		t.Fatalf("fixture %q: %d characters, matches the pattern %t", long, len(long),
			regexp.MustCompile(p.Pattern).MatchString(long))
	}
	for v, ok := range map[string]bool{
		"90d": true, "99999d": true, "1h30m": true, "1.5h": true, "300ms": true,
		"0d": false, "100000d": false, "-1h": false, "1y": false, long: false,
	} {
		if err := r.Validate(map[string]any{"name": "ci", "expires": v}); (err == nil) != ok {
			t.Errorf("expires %q: error %v, want accepted %t", v, err, ok)
		}
	}
	// "" is never sent: omitempty drops it, and no expiry is valid.
	b, err := json.Marshal(spec.TokenAddInput{Name: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"ci"}` {
		t.Errorf("input without expiry marshals to %s, want {\"name\":\"ci\"}", b)
	}
	var in map[string]any
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(in); err != nil {
		t.Errorf("input without expiry refused: %v", err)
	}
}

// TC-RA-E17: The changes item pattern and count bounds. Owner WI 4.1. Requirements: NFR-5, FR-7.
//
//   - Setup: The `ConfigUpdateInput` schema.
//   - Steps:
//     1. Validate these inputs:
//   - `["providers.host.enabled=true"]`;
//   - `["host.name="]`;
//   - `["=x"]`;
//   - `["Providers.x=1"]`;
//   - `["a.b=c\u0001"]`;
//   - `[]`;
//   - 32 items;
//   - 33 items;
//   - one item whose value has 4096 bytes;
//   - one item longer than `maxLength`.
//   - Expected:
//   - Accepted: `providers.host.enabled=true`, `host.name=` (the empty value is allowed by the pattern), 32 items, and the 4096-byte value.
//   - Rejected: `=x`, `Providers.x=1`, the control character, `[]`, 33 items, and the item over `maxLength`.
func TestTCRAE17_TheChangesItemPatternAndCountBounds(t *testing.T) {
	r := schemaOf(t, spec.UpdateConfig.Op())
	items := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = "host.name=h" + strconv.Itoa(i)
		}
		return out
	}
	maxLen := spec.MaxConfigValueBytes + 520
	// The longest key the pattern takes: 8 segments of 64 characters.
	longKey := strings.TrimSuffix(strings.Repeat("a"+strings.Repeat("b", 63)+".", 8), ".")
	if n := len(longKey) + 1 + spec.MaxConfigValueBytes; n != maxLen {
		t.Fatalf("the longest key, = and a %d-byte value are %d characters, want maxLength %d",
			spec.MaxConfigValueBytes, n, maxLen)
	}
	over := "host.name=" + strings.Repeat("a", maxLen-len("host.name=")+1)
	if len(over) != maxLen+1 {
		t.Fatalf("fixture over maxLength has %d characters", len(over))
	}
	for _, c := range []struct {
		name    string
		changes []any
		ok      bool
	}{
		{"a boolean", []any{"providers.host.enabled=true"}, true},
		{"an empty value", []any{"host.name="}, true},
		{"32 items", items(32), true},
		{"a 4096-byte value", []any{"host.name=" + strings.Repeat("a", spec.MaxConfigValueBytes)}, true},
		{"the longest key and value", []any{longKey + "=" + strings.Repeat("a", spec.MaxConfigValueBytes)}, true},
		{"no key", []any{"=x"}, false},
		{"an uppercase first letter", []any{"Providers.x=1"}, false},
		{"a control character", []any{"a.b=c\u0001"}, false},
		{"a newline", []any{"a.b=c\n"}, false},
		{"DEL", []any{"a.b=c\u007f"}, false},
		{"no =", []any{"host.name"}, false},
		{"no items", []any{}, false},
		{"33 items", items(33), false},
		{"over maxLength", []any{over}, false},
	} {
		if err := r.Validate(map[string]any{"changes": c.changes}); (err == nil) != c.ok {
			t.Errorf("%s: error %v, want accepted %t", c.name, err, c.ok)
		}
	}
	if err := r.Validate(map[string]any{}); err == nil {
		t.Error("an input without changes was accepted")
	}
	// null would reach admission as no assignments, past minItems.
	if err := r.Validate(map[string]any{"changes": nil}); err == nil {
		t.Error("changes null was accepted")
	}
}
