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
		{"ci", true}, {"a.b-c_d", true}, {strings.Repeat("a", 64), true}, {strings.Repeat("a", 65), false},
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
	t.Skip("TC-RA-67: SKIP-only stub; un-skip when WI 4.1 lands")
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
	t.Skip("TC-RA-68: SKIP-only stub; un-skip when WI 4.1 lands")
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
	t.Skip("TC-RA-E17: SKIP-only stub; un-skip when WI 4.1 lands")
}
