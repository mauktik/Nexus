// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-47 to TC-RA-54,
// TC-RA-E11 to TC-RA-E14, TC-RA-ERR13, TC-RA-ERR14 and TC-RA-ERR15, owned by
// WI 3.1: the key schema, assignments, Edit (D-5), Inspect and ReadFile.
//
// The fixtures are the shipped deploy/nexusd.default.yaml, read in place,
// and the hand-commented testdata/edit/commented.yaml. Golden outputs are in
// testdata/edit; go test -run TestTCRA50 -update rewrites them.

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/audit"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
)

var update = flag.Bool("update", false, "rewrite the testdata/edit golden files from this code")

const commentedPath = "testdata/edit/commented.yaml"

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assignments(t *testing.T, kvs ...string) []Assignment {
	t.Helper()
	as := make([]Assignment, len(kvs))
	for i, kv := range kvs {
		a, err := ParseAssignment(kv)
		if err != nil {
			t.Fatalf("ParseAssignment(%q): %v", kv, err)
		}
		as[i] = a
	}
	return as
}

func mustEdit(t *testing.T, data []byte, kvs ...string) ([]byte, []Change) {
	t.Helper()
	out, changes, err := Edit(data, "nexusd.yaml", assignments(t, kvs...))
	if err != nil {
		t.Fatalf("Edit(%q): %v", kvs, err)
	}
	if len(changes) != len(kvs) {
		t.Fatalf("Edit(%q) returned %d changes, want %d", kvs, len(changes), len(kvs))
	}
	return out, changes
}

// editRefused runs Edit on one assignment and returns its single error,
// asserting that no bytes come back and the error is located at key.
func editRefused(t *testing.T, data []byte, key, value string) *Error {
	t.Helper()
	out, changes, err := Edit(data, "nexusd.yaml", []Assignment{{Key: key, Value: value}})
	if out != nil || changes != nil {
		t.Errorf("Edit(%s=...) returned %d bytes and %d changes with an error, want none", key, len(out), len(changes))
	}
	var list ErrorList
	if !errors.As(err, &list) || len(list) != 1 {
		t.Fatalf("Edit(%s=...) error = %v (%T), want an ErrorList of one", key, err, err)
	}
	if list[0].Path != key || list[0].File != "nexusd.yaml" {
		t.Errorf("error at %s:%s, want nexusd.yaml:%s", list[0].File, list[0].Path, key)
	}
	return list[0]
}

func mustParse(t *testing.T, data []byte) *Config {
	t.Helper()
	cfg, err := Parse(data, "nexusd.yaml")
	if err != nil {
		t.Fatalf("Parse of the edited output: %v\n%s", err, data)
	}
	return cfg
}

func lines(b []byte) []string { return strings.Split(string(b), "\n") }

// replaceLine replaces the single line that starts with prefix.
func replaceLine(t *testing.T, data []byte, prefix, with string) []byte {
	t.Helper()
	ls := lines(data)
	found := -1
	for i, l := range ls {
		if strings.HasPrefix(l, prefix) {
			if found >= 0 {
				t.Fatalf("more than one line starts with %q", prefix)
			}
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("no line starts with %q", prefix)
	}
	if with == "" {
		ls = slices.Delete(ls, found, found+1)
	} else {
		ls[found] = with
	}
	return []byte(strings.Join(ls, "\n"))
}

// comments are the comments at one key path: the key's and its value's,
// merged, so a comment moving between the two still counts as attached.
type comments struct{ Head, Line, Foot string }

// commentMap maps each key path, plus "<doc>" and "" for the document and
// root mapping, to its comments. Paths without comments are left out.
func commentMap(t *testing.T, data []byte) map[string]comments {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	m := map[string]comments{}
	put := func(path string, ns ...*yaml.Node) {
		var c comments
		cat := func(a, b string) string {
			if a == "" || b == "" {
				return a + b
			}
			return a + "\n" + b
		}
		for _, n := range ns {
			c.Head, c.Line, c.Foot = cat(c.Head, n.HeadComment), cat(c.Line, n.LineComment), cat(c.Foot, n.FootComment)
		}
		if c != (comments{}) {
			m[path] = c
		}
	}
	put("<doc>", &doc)
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				p := join(path, n.Content[i].Value)
				put(p, n.Content[i], n.Content[i+1])
				walk(n.Content[i+1], p)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				p := path + "[" + strconv.Itoa(i) + "]"
				put(p, c)
				walk(c, p)
			}
		}
	}
	if len(doc.Content) > 0 {
		put("", doc.Content[0])
		walk(doc.Content[0], "")
	}
	return m
}

// keyPaths lists every mapping key path in document order.
func keyPaths(t *testing.T, data []byte) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := join(path, n.Content[i].Value)
			paths = append(paths, p)
			walk(n.Content[i+1], p)
		}
	}
	walk(doc.Content[0], "")
	return paths
}

func decodeGeneric(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// genericAt returns the value at a dotted path of a generic decode and
// whether it is there.
func genericAt(v any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[part]; !ok {
			return nil, false
		}
	}
	return v, true
}

// dropPath deletes a dotted path from a generic decode.
func dropPath(v any, path string) {
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		v = m[part]
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, parts[len(parts)-1])
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "edit", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if want := readBytes(t, path); !bytes.Equal(got, want) {
		t.Errorf("output differs from %s; rerun with -update and review the diff\ngot:\n%s", path, got)
	}
}

func TestTCRA47_TheKeySchemaFindsEveryKeyWithItsKindIn(t *testing.T) {
	kinds := []struct {
		key  string
		want KeyKind
	}{
		{"providers.host.enabled", KindBool},
		{"limits.maxBodyBytes", KindInt},
		{"providers.host.sampleInterval", KindDuration},
		{"tls.mode", KindString},
		{"listen", KindList},
		{"providers.host", KindMapping},
		{"providers.scripts.catalog", KindList},
	}
	for _, tc := range kinds {
		k, err := LookupKey(tc.key)
		if err != nil {
			t.Errorf("LookupKey(%s): %v", tc.key, err)
			continue
		}
		if k.Path != tc.key || k.Kind != tc.want {
			t.Errorf("LookupKey(%s) = %s %v, want %s %v", tc.key, k.Path, k.Kind, tc.key, tc.want)
		}
	}
	host, _ := LookupKey("providers.host")
	if want := []string{"enabled", "sampleInterval", "historyDuration", "sourceTimeout"}; !slices.Equal(host.Children, want) {
		t.Errorf("providers.host children = %q, want %q (struct order)", host.Children, want)
	}

	leaves := LeafKeys()
	var got []string
	for _, k := range leaves {
		if k.Kind == KindMapping {
			t.Errorf("LeafKeys includes the mapping %s", k.Path)
		}
		if strings.HasPrefix(k.Path, "providers.scripts.catalog.") {
			t.Errorf("LeafKeys includes %s: the catalog is one leaf", k.Path)
		}
		got = append(got, k.Path)
	}
	if !slices.Contains(got, "providers.scripts.catalog") {
		t.Error("LeafKeys is missing providers.scripts.catalog")
	}

	// Declaration order: the struct walk config_test.go uses to keep the
	// default file and the structs aligned, minus mappings and catalog fields.
	var decl []string
	for _, p := range structKeyPaths(reflect.TypeOf(Config{}), "") {
		if k, err := LookupKey(p); err == nil && k.Kind != KindMapping {
			decl = append(decl, p)
		}
	}
	if !slices.Equal(got, decl) {
		t.Errorf("LeafKeys order:\n%q\nwant declaration order:\n%q", got, decl)
	}

	// The default file's scalar and list key paths, Section transparent.
	var doc yaml.Node
	if err := yaml.Unmarshal(readBytes(t, defaultConfigPath), &doc); err != nil {
		t.Fatal(err)
	}
	var file []string
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := join(path, n.Content[i].Value)
			if v := n.Content[i+1]; v.Kind == yaml.MappingNode {
				walk(v, p)
			} else {
				file = append(file, p)
			}
		}
	}
	walk(doc.Content[0], "")
	if a, b := slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(file)); !slices.Equal(a, b) {
		t.Errorf("LeafKeys set:\n%q\nwant the default file's leaf set:\n%q", a, b)
	}
	// Within every mapping the file lists keys in struct order. The one
	// exception is the order of the provider sections themselves: the file
	// lists pihole before scripts, while Providers declares it after nexusd.
	section := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "providers."); ok {
			name, _, _ := strings.Cut(rest, ".")
			return name
		}
		return ""
	}
	byFileSection := slices.Clone(file)
	order := map[string]int{}
	for i, p := range got {
		if _, ok := order[section(p)]; !ok {
			order[section(p)] = i
		}
	}
	slices.SortStableFunc(byFileSection, func(a, b string) int { return order[section(a)] - order[section(b)] })
	if !slices.Equal(byFileSection, got) {
		t.Errorf("the default file's key order within sections differs from LeafKeys:\n%q\n%q", byFileSection, got)
	}

	// A-23: every audit fact key old.<path> fits.
	for _, k := range leaves {
		if n := len("old." + k.Path); n > audit.MaxFactKeyBytes {
			t.Errorf("old.%s is %d bytes, over audit.MaxFactKeyBytes %d", k.Path, n, audit.MaxFactKeyBytes)
		}
	}
}

func TestTCRA48_ParseAssignmentSplitsAtTheFirst(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Assignment
	}{
		{"a.b=c=d", Assignment{Key: "a.b", Value: "c=d"}},
		{"a.b=", Assignment{Key: "a.b", Value: ""}},
	} {
		got, err := ParseAssignment(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseAssignment(%q) = %+v, %v, want %+v", tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ in, key, value string }{
		{"a.b", "a.b", ""},
		{"=x", "", "x"},
	} {
		_, err := ParseAssignment(tc.in)
		if err == nil {
			t.Errorf("ParseAssignment(%q) succeeded, want an error", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) || (tc.value != "" && strings.Contains(err.Error(), tc.value)) {
			t.Errorf("ParseAssignment(%q) error %q, want one naming %q and not %q", tc.in, err, tc.key, tc.value)
		}
	}
}

func TestTCRA49_SettingAnExistingScalarOnTheShippedDefaultFile(t *testing.T) {
	data := readBytes(t, defaultConfigPath)

	// changedLines asserts the outputs differ only on the lines of the given
	// keys, each with only the value token replaced.
	changedLines := func(out []byte, edits map[string][2]string) {
		t.Helper()
		before, after := lines(data), lines(out)
		if len(before) != len(after) {
			t.Fatalf("line count %d, want %d", len(after), len(before))
		}
		n := 0
		for i := range before {
			if before[i] == after[i] {
				continue
			}
			n++
			ok := false
			for name, v := range edits {
				if strings.Replace(before[i], name+": "+v[0], name+": "+v[1], 1) == after[i] {
					ok = true
				}
			}
			if !ok {
				t.Errorf("line %d changed beyond a value token:\n-%s\n+%s", i+1, before[i], after[i])
			}
		}
		if n != len(edits) {
			t.Errorf("%d lines changed, want %d", n, len(edits))
		}
	}

	out, changes := mustEdit(t, data, "providers.host.enabled=true")
	changedLines(out, map[string][2]string{"enabled": {"false", "true"}})
	if want := (Change{Key: "providers.host.enabled", Old: false, New: true, Present: true}); changes[0] != want {
		t.Errorf("change = %+v, want %+v", changes[0], want)
	}
	if cfg := mustParse(t, out); !cfg.Providers.Host.Settings.Enabled {
		t.Error("providers.host.enabled decoded false, want true")
	}

	out, changes = mustEdit(t, data, "audit.retentionDays=30", "providers.host.sampleInterval=5s")
	changedLines(out, map[string][2]string{"retentionDays": {"90", "30"}, "sampleInterval": {"10s", "5s"}})
	want := []Change{
		{Key: "audit.retentionDays", Old: int64(90), New: int64(30), Present: true},
		{Key: "providers.host.sampleInterval", Old: "10s", New: "5s", Present: true},
	}
	if !slices.Equal(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
	cfg := mustParse(t, out)
	if cfg.Audit.RetentionDays != 30 || cfg.Providers.Host.Settings.SampleInterval != 5*time.Second {
		t.Errorf("decoded retentionDays %d, sampleInterval %s, want 30 and 5s", cfg.Audit.RetentionDays,
			cfg.Providers.Host.Settings.SampleInterval)
	}
}

func TestTCRA50_CommentsUntouchedValuesAndKeyOrderSurviveEditsOn(t *testing.T) {
	const assigned = "providers.host.sourceTimeout"
	for _, fx := range []struct{ name, path string }{
		{"default", defaultConfigPath},
		{"commented", commentedPath},
	} {
		t.Run(fx.name, func(t *testing.T) {
			data := readBytes(t, fx.path)

			t.Run("splice", func(t *testing.T) {
				const key = "providers.host.enabled"
				out, _ := mustEdit(t, data, key+"=true")
				if got, want := commentMap(t, out), commentMap(t, data); !reflect.DeepEqual(got, want) {
					t.Errorf("comment map changed:\n got %v\nwant %v", got, want)
				}
				before, after := decodeGeneric(t, data), decodeGeneric(t, out)
				if v, _ := genericAt(after, key); v != true {
					t.Errorf("%s = %v, want true", key, v)
				}
				dropPath(before, key)
				dropPath(after, key)
				if !reflect.DeepEqual(before, after) {
					t.Error("an untouched key decodes differently")
				}
				if got, want := keyPaths(t, out), keyPaths(t, data); !slices.Equal(got, want) {
					t.Errorf("key order %q, want %q", got, want)
				}
				mustParse(t, out)
				checkGolden(t, fx.name+"-splice.golden.yaml", out)
			})

			t.Run("insert", func(t *testing.T) {
				// sourceTimeout is the last key of providers.host in both
				// fixtures, so appending it restores the original order.
				in := replaceLine(t, data, "    sourceTimeout:", "")
				out, changes := mustEdit(t, in, assigned+"=2s")
				if want := (Change{Key: assigned, Old: nil, New: "2s", Present: false}); changes[0] != want {
					t.Errorf("change = %+v, want %+v", changes[0], want)
				}
				// Every comment of the input stays on its key, and so does
				// every comment of the fixture except the removed line's own.
				if got, want := commentMap(t, out), commentMap(t, in); !reflect.DeepEqual(got, want) {
					t.Errorf("comment map changed:\n got %v\nwant %v", got, want)
				}
				orig := commentMap(t, data)
				if orig[assigned].Line == "" {
					t.Fatalf("fixture has no line comment on %s", assigned)
				}
				delete(orig, assigned)
				if got := commentMap(t, out); !reflect.DeepEqual(got, orig) {
					t.Errorf("comment map differs from the fixture's beyond %s's line comment:\n got %v\nwant %v", assigned, got, orig)
				}
				before, after := decodeGeneric(t, data), decodeGeneric(t, out)
				if v, _ := genericAt(after, assigned); v != "2s" {
					t.Errorf("%s = %v, want 2s", assigned, v)
				}
				dropPath(before, assigned)
				dropPath(after, assigned)
				if !reflect.DeepEqual(before, after) {
					t.Error("an untouched key decodes differently")
				}
				paths := keyPaths(t, out)
				if want := keyPaths(t, data); !slices.Equal(paths, want) {
					t.Errorf("key order %q, want %q", paths, want)
				}
				host := valueNode(mustRoot(t, out), "providers.host")
				if last := host.Content[len(host.Content)-2].Value; last != "sourceTimeout" {
					t.Errorf("last key of providers.host is %s, want the inserted sourceTimeout", last)
				}
				mustParse(t, out)
				checkGolden(t, fx.name+"-insert.golden.yaml", out)
			})
		})
	}
}

func mustRoot(t *testing.T, data []byte) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Content[0]
}

// normalize applies the documented re-encode normalization (A-10): blank
// lines dropped, runs of spaces before a comment collapsed.
func normalize(ls []string) string {
	spaces := regexp.MustCompile(` +#`)
	var out []string
	for _, l := range ls {
		if strings.TrimSpace(l) != "" {
			out = append(out, spaces.ReplaceAllString(l, " #"))
		}
	}
	return strings.Join(out, "\n")
}

func TestTCRA51_AssigningAnAbsentKeyCreatesItAndItsMissing(t *testing.T) {
	data := readBytes(t, commentedPath)
	in := replaceLine(t, data, "  # Pi-hole stays off", "")
	in = replaceLine(t, in, "  pihole:", "")

	out, changes := mustEdit(t, in, "providers.pihole.enabled=true")
	if want := (Change{Key: "providers.pihole.enabled", Old: nil, New: true, Present: false}); changes[0] != want {
		t.Errorf("change = %+v, want %+v", changes[0], want)
	}
	root := mustRoot(t, out)
	if m := valueNode(root, "providers.pihole"); m == nil || m.Kind != yaml.MappingNode {
		t.Fatalf("providers.pihole is %v, want a new mapping\n%s", m, out)
	}
	if v := valueNode(root, "providers.pihole.enabled"); v == nil || v.ShortTag() != "!!bool" || v.Value != "true" {
		t.Errorf("providers.pihole.enabled is %+v, want !!bool true", v)
	}
	if cfg := mustParse(t, out); !cfg.Providers.Pihole.Settings.Enabled {
		t.Error("providers.pihole.enabled decoded false")
	}

	// Outside the two new lines, only the documented normalization.
	outLines := lines(out)
	i := slices.Index(outLines, "  pihole:")
	if i < 0 || i+1 >= len(outLines) || outLines[i+1] != "    enabled: true" {
		t.Fatalf("no new block mapping \"  pihole:\\n    enabled: true\" in\n%s", out)
	}
	rest := slices.Delete(slices.Clone(outLines), i, i+2)
	if got, want := normalize(rest), normalize(lines(in)); got != want {
		t.Errorf("output beyond the new lines differs from the input after normalization:\n got:\n%s\nwant:\n%s", got, want)
	}
	if got, want := commentMap(t, out), commentMap(t, in); !reflect.DeepEqual(got, want) {
		t.Errorf("comment map changed:\n got %v\nwant %v", got, want)
	}
	checkGolden(t, "commented-new-mapping.golden.yaml", out)
}

func TestTCRA52_ValuesAreTypedByTheSchema(t *testing.T) {
	data := readBytes(t, commentedPath)
	if v := valueNode(mustRoot(t, data), "host.name"); v.Value != "true" || v.Style != yaml.DoubleQuotedStyle {
		t.Fatalf("fixture host.name is %q style %v, want the quoted string \"true\"", v.Value, v.Style)
	}
	for _, tc := range []struct {
		kv, tag, value string
		style          yaml.Style
		parseErr       string // the only Parse error expected, at its key
	}{
		{kv: "providers.host.enabled=true", tag: "!!bool", value: "true"},
		{kv: "host.name=true", tag: "!!str", value: "true", style: yaml.DoubleQuotedStyle},
		{kv: "host.name=yes", tag: "!!str", value: "yes", style: yaml.DoubleQuotedStyle},
		// 90s is typed correctly but outside sampleInterval's 2s..60s, so
		// Parse refuses it on that bound alone; historyDuration takes it.
		{kv: "providers.host.sampleInterval=90s", tag: "!!str", value: "90s", parseErr: "must be between 2s and 60s"},
		{kv: "providers.host.historyDuration=90s", tag: "!!str", value: "90s"},
		{kv: "limits.maxBodyBytes=1048576", tag: "!!int", value: "1048576"},
		{kv: "tls.mode=auto", tag: "!!str", value: "auto"},
	} {
		t.Run(tc.kv, func(t *testing.T) {
			out, _ := mustEdit(t, data, tc.kv)
			key, _, _ := strings.Cut(tc.kv, "=")
			v := valueNode(mustRoot(t, out), key)
			if v == nil || v.ShortTag() != tc.tag || v.Value != tc.value || v.Style != tc.style {
				t.Fatalf("%s node = %+v, want %s %q style %v", key, v, tc.tag, tc.value, tc.style)
			}
			_, err := Parse(out, "nexusd.yaml")
			if tc.parseErr == "" {
				if err != nil {
					t.Errorf("Parse: %v", err)
				}
				return
			}
			var list ErrorList
			if !errors.As(err, &list) || len(list) != 1 || list[0].Path != key || !strings.Contains(list[0].Msg, tc.parseErr) {
				t.Errorf("Parse error %v, want only %q at %s", err, tc.parseErr, key)
			}
		})
	}
}

func TestTCRA53_InspectAndInspectAllReportValueEffectiveSource(t *testing.T) {
	const sentinel = "RA_ENV_SENTINEL_9f3c"
	if Redacted != audit.Redacted {
		t.Errorf("config.Redacted %q != audit.Redacted %q", Redacted, audit.Redacted)
	}
	data := readBytes(t, commentedPath)
	data = replaceLine(t, data, "    catalog: []", strings.Join([]string{
		"    catalog:",
		"      - id: deploy",
		"        description: Deploy the API",
		"        path: /opt/deploy.sh",
		"        runAs: deploy",
		"        workingDirectory: /opt",
		"        timeout: 90m",
		"        env: {TOKEN: " + sentinel + "}",
	}, "\n"))
	data = replaceLine(t, data, "    historyDuration:", "")

	all, err := InspectAll(data, "nexusd.yaml")
	if err != nil {
		t.Fatalf("InspectAll: %v", err)
	}
	var keys, want []string
	byKey := map[string]Entry{}
	for _, e := range all {
		keys = append(keys, e.Key)
		byKey[e.Key] = e
	}
	for _, k := range LeafKeys() {
		want = append(want, k.Path)
	}
	if !slices.Equal(keys, want) {
		t.Errorf("InspectAll keys %q, want LeafKeys order %q", keys, want)
	}

	hd := byKey["providers.host.historyDuration"]
	if wantHD := (Entry{Key: "providers.host.historyDuration", Value: nil, Effective: shortDuration(hostspec.DefaultHistoryDuration), Source: SourceDefault}); hd != wantHD {
		t.Errorf("absent historyDuration = %+v, want %+v", hd, wantHD)
	}
	for _, tc := range []Entry{
		{Key: "providers.host.sampleInterval", Value: "10s", Effective: "10s", Source: SourceFile},
		{Key: "limits.maxBodyBytes", Value: int64(1048576), Effective: int64(1048576), Source: SourceFile},
		{Key: "providers.host.enabled", Value: false, Effective: false, Source: SourceFile},
		{Key: "tls.mode", Value: "auto", Effective: "auto", Source: SourceFile},
		{Key: "auth.failureLimitPerMinute", Value: nil, Effective: int64(10), Source: SourceDefault},
		{Key: "providers.docker.updateMaxRuntime", Value: nil, Effective: "1h", Source: SourceDefault},
	} {
		if got := byKey[tc.Key]; !reflect.DeepEqual(got, tc) {
			t.Errorf("%s = %#v, want %#v", tc.Key, got, tc)
		}
	}
	if got, want := byKey["listen"].Value, []any{"127.0.0.1:7443"}; !reflect.DeepEqual(got, want) {
		t.Errorf("listen value %#v, want %#v", got, want)
	}

	hdOne, err := Inspect(data, "nexusd.yaml", "providers.host.historyDuration")
	if err != nil || hdOne != hd {
		t.Errorf("Inspect(historyDuration) = %+v, %v, want %+v", hdOne, err, hd)
	}

	cat, err := Inspect(data, "nexusd.yaml", "providers.scripts.catalog")
	if err != nil {
		t.Fatalf("Inspect(catalog): %v", err)
	}
	if !reflect.DeepEqual(cat, byKey["providers.scripts.catalog"]) {
		t.Errorf("Inspect(catalog) = %#v differs from InspectAll's %#v", cat, byKey["providers.scripts.catalog"])
	}
	envOf := func(v any) any {
		list, ok := v.([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("catalog %#v, want one entry", v)
		}
		m, ok := list[0].(map[string]any)
		if !ok {
			t.Fatalf("catalog entry %#v, want a mapping", list[0])
		}
		return m["env"]
	}
	wantEnv := map[string]any{"TOKEN": Redacted}
	if got := envOf(cat.Value); !reflect.DeepEqual(got, wantEnv) {
		t.Errorf("catalog value env %#v, want %#v", got, wantEnv)
	}
	if got := envOf(cat.Effective); !reflect.DeepEqual(got, wantEnv) {
		t.Errorf("catalog effective env %#v, want %#v", got, wantEnv)
	}
	if got := cat.Effective.([]any)[0].(map[string]any)["timeout"]; got != "5400s" {
		t.Errorf("catalog effective timeout %#v, want shortDuration's 5400s", got)
	}

	host, err := Inspect(data, "nexusd.yaml", "providers.host")
	if err != nil {
		t.Fatalf("Inspect(providers.host): %v", err)
	}
	children, ok := host.Value.(map[string]Entry)
	if !ok || host.Key != "providers.host" || host.Source != SourceFile {
		t.Fatalf("providers.host = %#v, want a mapping entry from the file", host)
	}
	if len(children) != 4 || children["enabled"] != byKey["providers.host.enabled"] || children["historyDuration"] != hd {
		t.Errorf("providers.host children %#v, want its four entries", children)
	}

	for name, v := range map[string]any{"InspectAll": all, "catalog": cat, "providers.host": host} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		if n := bytes.Count(b, []byte(sentinel)); n != 0 {
			t.Errorf("JSON of %s holds the env secret %d times", name, n)
		}
	}
}

func TestTCRA54_ReadFileKeepsLoadSOwnerAndModeCheckAndLoadEquals(t *testing.T) {
	data := readBytes(t, defaultConfigPath)
	dir := t.TempDir()
	root := os.Geteuid() == 0
	for _, mode := range []os.FileMode{0o600, 0o644} {
		path := filepath.Join(dir, "nexusd-"+strconv.FormatUint(uint64(mode), 8)+".yaml")
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		uid := fi.Sys().(*syscall.Stat_t).Uid

		// Step 1: the euid-0 core gives exactly Load's error text, as at
		// 521261c: ErrorList{{File: path, Msg: checkFile's message}}.
		got, err := readChecked(path, 0)
		_, loadErr := load(path, 0)
		var wantMsg string
		switch {
		case uid != 0:
			wantMsg = "owned by uid " + strconv.FormatUint(uint64(uid), 10) + ", must be owned by root; run chown root:root " +
				path + " && chmod 0600 " + path
		case mode == 0o644:
			wantMsg = "mode 0644 gives group or other access and the file can hold secrets; run chmod 0600 " + path
		}
		if wantMsg == "" {
			if err != nil || loadErr != nil || !bytes.Equal(got, data) {
				t.Errorf("readChecked(%04o, euid 0) = %d bytes, %v; load: %v, want the file", mode, len(got), err, loadErr)
			}
		} else {
			var list ErrorList
			want := ErrorList{{File: path, Msg: wantMsg}}
			if !errors.As(err, &list) || !reflect.DeepEqual(list, want) || got != nil {
				t.Errorf("readChecked(%04o, euid 0) = %v, want %v", mode, err, want)
			}
			if loadErr == nil || loadErr.Error() != err.Error() {
				t.Errorf("load(%04o, euid 0) error %v, want the same as readChecked's %v", mode, loadErr, err)
			}
		}
		if !root {
			t.Logf("not root: the file is owned by uid %d, so the euid-0 check stops at the owner, as Load does", uid)
		}

		// Step 2: as the test user.
		got, err = ReadFile(path)
		if root && mode == 0o644 {
			if err == nil {
				t.Errorf("ReadFile(%04o) as root succeeded", mode)
			}
		} else if err != nil || !bytes.Equal(got, data) {
			t.Errorf("ReadFile(%04o) = %d bytes, %v, want the file", mode, len(got), err)
		}

		// Step 3: Load is ReadFile then Parse.
		cfg, loadErr := Load(path)
		var viaRead *Config
		var readErr error
		if b, err := ReadFile(path); err != nil {
			readErr = err
		} else {
			viaRead, readErr = Parse(b, path)
		}
		if !reflect.DeepEqual(cfg, viaRead) || !reflect.DeepEqual(loadErr, readErr) {
			t.Errorf("Load(%04o) = %v, %v; Parse(ReadFile) = %v, %v", mode, cfg != nil, loadErr, viaRead != nil, readErr)
		}
	}

	missing := filepath.Join(dir, "missing.yaml")
	_, loadErr := Load(missing)
	_, readErr := ReadFile(missing)
	var list ErrorList
	if !errors.As(readErr, &list) || loadErr == nil || loadErr.Error() != readErr.Error() {
		t.Errorf("missing file: ReadFile error %v (%T), Load error %v, want the same ErrorList", readErr, readErr, loadErr)
	}
}

func TestTCRAE11_ANonASCIICommentBeforeAKeyStillSplicesAtTheRight(t *testing.T) {
	data := readBytes(t, commentedPath)
	if !bytes.Contains(data, []byte("    # Ünïcode ✓\n    enabled: false")) {
		t.Fatal("fixture lacks the non-ASCII comment above providers.host.enabled")
	}

	// spliced asserts out is data with only the token at off replaced.
	spliced := func(t *testing.T, data, out []byte, off int, old, repl string) {
		t.Helper()
		if !bytes.HasPrefix(data[off:], []byte(old)) {
			t.Fatalf("no %q at offset %d", old, off)
		}
		want := append(append(slices.Clone(data[:off]), repl...), data[off+len(old):]...)
		if !bytes.Equal(out, want) {
			t.Errorf("output is not the input with %q at byte %d replaced by %q:\n%s", old, off, repl, out)
		}
	}

	out, _ := mustEdit(t, data, "providers.host.enabled=true")
	spliced(t, data, out, bytes.Index(data, []byte("    enabled: false # Off"))+len("    enabled: "), "false", "true")
	mustParse(t, out)

	// Non-ASCII earlier on the key's own line, where the rune column and
	// the byte column differ (A-9).
	same := replaceLine(t, data, "  pihole:", `  pihole: {dnsFile: "/etc/dnsmasq.d/ünï-✓.conf", enabled: false}`)
	out, _ = mustEdit(t, same, "providers.pihole.enabled=true")
	spliced(t, same, out, bytes.Index(same, []byte("enabled: false}"))+len("enabled: "), "false", "true")

	// The variant: a flow mapping holding a non-ASCII value. Either path is
	// allowed; the semantic check must pass.
	variant := replaceLine(t, data, "host:", `host:    {name: "ünï"}`)
	variant = replaceLine(t, variant, `  name: "true"`, "")
	out, _ = mustEdit(t, variant, "host.name=x")
	want := decodeGeneric(t, variant)
	want.(map[string]any)["host"].(map[string]any)["name"] = "x"
	if got := decodeGeneric(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("variant output decodes to %v, want %v", got, want)
	}
	if cfg := mustParse(t, out); cfg.Host.Name != "x" {
		t.Errorf("host.name = %q, want x", cfg.Host.Name)
	}
}

func TestTCRAE12_AFlowMappingParentFallsBackToReEncodingAndPasses(t *testing.T) {
	data := readBytes(t, commentedPath)
	if !bytes.Contains(data, []byte("  pihole: {enabled: false}")) {
		t.Fatal("fixture lacks the flow mapping providers.pihole")
	}
	out, _ := mustEdit(t, data, "providers.pihole.enabled=true")
	want := decodeGeneric(t, data)
	want.(map[string]any)["providers"].(map[string]any)["pihole"].(map[string]any)["enabled"] = true
	if got := decodeGeneric(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("output decodes to %v, want the original plus the assignment %v", got, want)
	}
	if cfg := mustParse(t, out); !cfg.Providers.Pihole.Settings.Enabled {
		t.Error("providers.pihole.enabled decoded false")
	}
	if got, wantC := commentMap(t, out), commentMap(t, data); !reflect.DeepEqual(got, wantC) {
		t.Errorf("comment map changed:\n got %v\nwant %v", got, wantC)
	}

	// A token that is valid plain in block context but not inside a flow
	// mapping: the splice fails the semantic check, so Edit re-encodes.
	flow := replaceLine(t, data, "host:", "host: {name: x} # Flow host")
	flow = replaceLine(t, flow, `  name: "true"`, "")
	out, _ = mustEdit(t, flow, "host.name=a,b")
	naive := bytes.Replace(flow, []byte("{name: x}"), []byte("{name: a,b}"), 1)
	if bytes.Equal(out, naive) {
		t.Fatal("Edit kept the splice that changes the document's meaning")
	}
	want = decodeGeneric(t, flow)
	want.(map[string]any)["host"].(map[string]any)["name"] = "a,b"
	if got := decodeGeneric(t, out); !reflect.DeepEqual(got, want) {
		t.Errorf("fallback output decodes to %v, want %v\n%s", got, want, out)
	}
	if got, wantC := commentMap(t, out), commentMap(t, flow); !reflect.DeepEqual(got, wantC) {
		t.Errorf("fallback comment map changed:\n got %v\nwant %v", got, wantC)
	}
	if cfg := mustParse(t, out); cfg.Host.Name != "a,b" {
		t.Errorf("host.name = %q, want a,b", cfg.Host.Name)
	}
}

func TestTCRAE13_ValueLengthAndCharacterBounds(t *testing.T) {
	data := readBytes(t, defaultConfigPath)
	long := strings.Repeat("a", 4096)
	out, changes := mustEdit(t, data, "host.name="+long)
	if changes[0].New != long {
		t.Error("the 4096-byte value changed on the way in")
	}
	if cfg := mustParse(t, out); cfg.Host.Name != long {
		t.Errorf("host.name decoded to %d bytes, want 4096", len(cfg.Host.Name))
	}
	for name, v := range map[string]string{
		"4097 bytes": strings.Repeat("a", 4097),
		"\\x01":      "a\x01b",
		"\\x7f":      "a\x7fb",
	} {
		t.Run(name, func(t *testing.T) {
			e := editRefused(t, data, "host.name", v)
			if strings.Contains(e.Error(), v) || strings.Contains(e.Error(), "aaaa") {
				t.Errorf("error %q holds the value", e)
			}
		})
	}
}

func TestTCRAE14_EditedOutputAddsNoKeyBeyondTheAssignments(t *testing.T) {
	data := readBytes(t, defaultConfigPath)
	out, _ := mustEdit(t, data, "providers.host.enabled=true", "audit.retentionDays=30", "limits.maxBodyBytes=2097152")
	before, after := keyPaths(t, data), keyPaths(t, out)
	for _, p := range after {
		if !slices.Contains(before, p) {
			t.Errorf("the edit added key %s", p)
		}
	}
	if len(after) != len(before) {
		t.Errorf("%d keys after, %d before", len(after), len(before))
	}

	// CON-7: no new YAML dependency. pb33f/go-yaml is an indirect
	// dependency of libopenapi, outside this package's imports.
	var direct []string
	inRequire := false
	for _, l := range lines(readBytes(t, "../../go.mod")) {
		l = strings.TrimSpace(l)
		switch {
		case l == "require (":
			inRequire = true
		case l == ")":
			inRequire = false
		case inRequire && !strings.HasSuffix(l, "// indirect"):
			direct = append(direct, strings.Fields(l)[0])
		}
	}
	for _, m := range direct {
		if strings.Contains(strings.ToLower(m), "yaml") && m != "go.yaml.in/yaml/v3" {
			t.Errorf("go.mod requires the YAML module %s", m)
		}
	}
	f, err := parser.ParseFile(token.NewFileSet(), "edit.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		first, _, _ := strings.Cut(p, "/")
		if strings.Contains(first, ".") && p != "go.yaml.in/yaml/v3" && !strings.HasPrefix(p, "github.com/mauktik/nexus/") {
			t.Errorf("edit.go imports %s", p)
		}
	}
}

func TestTCRAERR13_UnknownKeyPathsNameTheNearestValidParentAndIts(t *testing.T) {
	host, err := LookupKey("providers.host")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, parent string
		children    []string
	}{
		{"providers.host.enabeld", "providers.host", host.Children},
		{"providers.nope.enabled", "providers", []string{"docker", "apt", "scripts", "nginx", "nexusd", "pihole", "host", "storage"}},
		{"bogus", "", []string{"host", "listen", "tls", "limits", "auth", "audit", "mcp", "providers"}},
	} {
		_, err := LookupKey(tc.key)
		var u *UnknownKeyError
		if !errors.As(err, &u) {
			t.Errorf("LookupKey(%s) error %v (%T), want *UnknownKeyError", tc.key, err, err)
			continue
		}
		want := &UnknownKeyError{Path: tc.key, Parent: tc.parent, Children: tc.children}
		if !reflect.DeepEqual(u, want) {
			t.Errorf("LookupKey(%s) = %+v, want %+v", tc.key, u, want)
		}
		if !strings.Contains(u.Error(), tc.key) || !strings.Contains(u.Error(), strings.Join(tc.children, ", ")) {
			t.Errorf("error text %q does not name the key and the parent's keys", u.Error())
		}
	}
}

func TestTCRAERR14_ListsMappingsAnchorsAliasesAndExplicitTagsAre(t *testing.T) {
	data := readBytes(t, defaultConfigPath)
	for _, tc := range []struct{ key, value, word string }{
		{"listen", "127.0.0.1:1", "list"},
		{"tls.extraSANs", "x", "list"},
		{"providers.scripts.catalog", "x", "list"},
		{"providers.host", "x", "mapping"},
	} {
		if e := editRefused(t, data, tc.key, tc.value); !strings.Contains(e.Msg, tc.word) {
			t.Errorf("%s: message %q does not name a %s", tc.key, e.Msg, tc.word)
		}
	}

	anchored := replaceLine(t, data, "  name: ", "  name: &n x")
	anchored = replaceLine(t, anchored, "  certFile: ", "  certFile: *n")
	tagged := replaceLine(t, data, "  name: ", "  name: !!str x")
	msgs := map[string]string{}
	for _, tc := range []struct {
		name, key, word string
		data            []byte
	}{
		{"anchor", "host.name", "anchor", anchored},
		{"alias", "tls.certFile", "alias", anchored},
		{"tag", "host.name", "tag", tagged},
	} {
		e := editRefused(t, tc.data, tc.key, "y")
		if !strings.Contains(e.Msg, tc.word) || e.Line == 0 {
			t.Errorf("%s: error %q (line %d), want a located message naming the %s", tc.name, e.Msg, e.Line, tc.word)
		}
		msgs[e.Msg] = tc.name
	}
	if len(msgs) != 3 {
		t.Errorf("the anchor, alias and tag cases share messages: %v", msgs)
	}
}

func TestTCRAERR15_WrongTypeValuesAreRefused(t *testing.T) {
	data := readBytes(t, defaultConfigPath)
	for _, tc := range []struct{ key, value, want string }{
		{"providers.host.enabled", "yes", "boolean"},
		{"providers.host.enabled", "1", "boolean"},
		{"limits.maxBodyBytes", "0x10", "integer"},
		{"limits.maxBodyBytes", "99999999999999999999", "integer"},
		{"providers.host.sampleInterval", "10", "duration"},
		{"audit.retentionDays", "1.5", "integer"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			e := editRefused(t, data, tc.key, tc.value)
			if !strings.Contains(e.Msg, tc.want) || strings.Contains(e.Msg, tc.value) {
				t.Errorf("message %q, want one naming %s without the value", e.Msg, tc.want)
			}
		})
	}
}
