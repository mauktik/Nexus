package output

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
)

func TestParseFormatAndDefault(t *testing.T) {
	for _, s := range []string{"table", "json", "jsonl", "yaml"} {
		f, err := ParseFormat(s)
		if err != nil || string(f) != s {
			t.Errorf("ParseFormat(%q) = %q, %v", s, f, err)
		}
	}
	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(xml) accepted")
	}
	if DefaultFormat(true) != Table || DefaultFormat(false) != JSONL {
		t.Errorf("defaults: tty %q, piped %q; want table and jsonl", DefaultFormat(true), DefaultFormat(false))
	}
	if Table.Machine() || !JSON.Machine() || !JSONL.Machine() || !YAML.Machine() || Format("").Machine() {
		t.Error("Machine() is wrong")
	}
}

// NO_COLOR (https://no-color.org) and TERM=dumb turn color off; it is never
// on without a terminal.
func TestColorEnabled(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if !ColorEnabled(true, env(nil)) {
		t.Error("color off on a plain terminal")
	}
	for _, c := range []struct {
		tty bool
		env map[string]string
	}{
		{false, nil},
		{true, map[string]string{"NO_COLOR": "1"}},
		{true, map[string]string{"TERM": "dumb"}},
	} {
		if ColorEnabled(c.tty, env(c.env)) {
			t.Errorf("color on for tty=%v env=%v", c.tty, c.env)
		}
	}
}

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	m, err := DecodeObject([]byte(s))
	if err != nil {
		t.Fatalf("DecodeObject(%s): %v", s, err)
	}
	return m
}

func render(t *testing.T, o Options, objs ...map[string]any) string {
	t.Helper()
	var b bytes.Buffer
	p := NewPrinter(&b, o)
	for _, m := range objs {
		if err := p.Print(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const sample = `{"id":"/providers/docker/containers/web","host":"nas","name":"web","type":"containers",
	"properties":{"state":"running","foo":"y","ports":[{"port":80},{"port":443}],"size":12345678901,"ratio":0.5}}`

func TestFormatsAgree(t *testing.T) {
	m := obj(t, sample)
	jl := render(t, Options{Format: JSONL}, m, m)
	lines := strings.Split(strings.TrimSuffix(jl, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl lines = %d: %q", len(lines), jl)
	}
	var fromJSONL []any
	for _, l := range lines {
		var v any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("jsonl line %q: %v", l, err)
		}
		fromJSONL = append(fromJSONL, v)
	}
	var fromJSON []any
	if err := json.Unmarshal([]byte(render(t, Options{Format: JSON}, m, m)), &fromJSON); err != nil {
		t.Fatalf("json: %v", err)
	}
	if !reflect.DeepEqual(fromJSON, fromJSONL) {
		t.Errorf("json %v != jsonl %v", fromJSON, fromJSONL)
	}
	var fromYAML any
	if err := yaml.Unmarshal([]byte(render(t, Options{Format: YAML}, m, m)), &fromYAML); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	// Through JSON, so YAML's int and JSON's float64 compare as numbers.
	if !reflect.DeepEqual(viaJSON(t, fromYAML), fromJSON) {
		t.Errorf("yaml %v != json %v", fromYAML, fromJSON)
	}
	if n := fromJSON[0].(map[string]any)["properties"].(map[string]any)["size"]; n != float64(12345678901) {
		t.Errorf("size = %v", n)
	}
}

func viaJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-E28 at the unit level: nothing printed is an empty result, not an error.
func TestEmpty(t *testing.T) {
	if s := render(t, Options{Format: JSONL}); s != "" {
		t.Errorf("jsonl = %q", s)
	}
	if s := render(t, Options{Format: JSON}); strings.TrimSpace(s) != "[]" {
		t.Errorf("json = %q", s)
	}
	if s := render(t, Options{Format: YAML}); strings.TrimSpace(s) != "[]" {
		t.Errorf("yaml = %q", s)
	}
	s := render(t, Options{Format: Table, Columns: []Path{{"name"}}})
	if s != "HOST  NAME\n" {
		t.Errorf("table = %q", s)
	}
}

func TestSelect(t *testing.T) {
	paths, err := ParsePaths([]string{"name,properties.state", "properties.ports.port"})
	if err != nil {
		t.Fatal(err)
	}
	got := Select(obj(t, sample), paths)
	want := obj(t, `{"id":"/providers/docker/containers/web","host":"nas","name":"web",
		"properties":{"state":"running","ports":[{"port":80},{"port":443}]}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Select = %v\nwant %v", got, want)
	}
	// A path that is absent is left out, not invented.
	if got := Select(obj(t, sample), []Path{{"nope", "x"}}); len(got) != 2 {
		t.Errorf("absent path: %v", got)
	}
	for _, bad := range []string{"", "a..b", ".a", "a."} {
		if _, err := ParsePaths([]string{bad}); err == nil {
			t.Errorf("ParsePaths(%q) accepted", bad)
		}
	}
}

func TestFilters(t *testing.T) {
	ms, err := ParseFilters([]string{"status=x", "properties.foo=y", "properties.foo=z", "status=a=b"})
	if err != nil {
		t.Fatal(err)
	}
	server, client := SplitFilters(ms, []string{"status"})
	if !slices.Equal(server, []string{"status=x", "status=a=b"}) {
		t.Errorf("server = %v", server)
	}
	if len(client) != 2 || client[0].Key != "properties.foo" || client[1].Value != "z" {
		t.Errorf("client = %+v", client)
	}
	m := obj(t, sample)
	if !MatchAll(m, client) { // foo=y or foo=z
		t.Error("foo=y|z does not match foo=y")
	}
	cases := []struct {
		filter string
		match  bool
	}{
		{"properties.foo=z", false},
		{"properties.ports.port=443", true}, // Any element of an array
		{"properties.size=12345678901", true},
		{"properties.ratio=0.5", true},
		{"properties.missing=", false},
		{"name=web", true},
	}
	for _, c := range cases {
		ms, err := ParseFilters([]string{c.filter})
		if err != nil {
			t.Fatal(err)
		}
		if MatchAll(m, ms) != c.match {
			t.Errorf("%s: match = %v, want %v", c.filter, !c.match, c.match)
		}
	}
	// Different keys must all hold.
	ms, _ = ParseFilters([]string{"name=web", "properties.foo=z"})
	if MatchAll(m, ms) {
		t.Error("name=web AND foo=z matched")
	}
	for _, bad := range []string{"novalue", "=x", "a..b=1"} {
		if _, err := ParseFilters([]string{bad}); err == nil {
			t.Errorf("ParseFilters(%q) accepted", bad)
		}
	}
}

func TestTable(t *testing.T) {
	m := obj(t, sample)
	m2 := obj(t, `{"id":"/x","host":"nas","name":"evil\u001b[31m\nline","properties":{"state":"exited"}}`)
	s := render(t, Options{Format: Table, Columns: []Path{{"name"}, {"properties", "state"}, {"properties", "ports", "port"}}}, m, m2)
	want := "HOST  NAME                STATE    PORTS.PORT\n" +
		"nas   web                 running  80, 443\n" +
		"nas   evil\\x1b[31m\\nline  exited   \n"
	if s != want {
		t.Errorf("table:\n%s\nwant:\n%s", s, want)
	}
	if strings.ContainsRune(s, 0x1b) {
		t.Error("an escape byte reached the table")
	}
	colored := render(t, Options{Format: Table, Color: true, Columns: []Path{{"name"}}}, m)
	if !strings.HasPrefix(colored, "\x1b[1m") {
		t.Errorf("colored header = %q", colored)
	}
	// --select replaces the spec's columns.
	s = render(t, Options{Format: Table, Columns: []Path{{"name"}}, Select: []Path{{"properties", "state"}}}, m)
	if s != "HOST  STATE\nnas   running\n" {
		t.Errorf("select table = %q", s)
	}
}

// Log text in a table is printed as written, with control characters other
// than newline and tab made visible, and a truncation marker as a note.
func TestTableText(t *testing.T) {
	f1 := obj(t, `{"o":0,"t":"2026-11-07T12:00:00Z","s":"out","d":"hello\tworld\n","host":"nas","id":"op@0"}`)
	f2 := obj(t, `{"type":"truncated","lostBytes":1024,"host":"nas","id":"op@truncated"}`)
	f3 := obj(t, `{"o":12,"t":"2026-11-07T12:00:01Z","s":"err","d":"\u001b]0;pwned\u0007bye\n","host":"nas","id":"op@12"}`)
	s := render(t, Options{Format: Table, Text: "d"}, f1, f2, f3)
	want := "hello\tworld\n[1024 bytes of output were not kept]\n\\x1b]0;pwned\\x07bye\n"
	if s != want {
		t.Errorf("text = %q, want %q", s, want)
	}
}

type props struct {
	State  string            `json:"state"`
	Ports  []struct{ P int } `json:"ports"`
	Extra  map[string]string `json:"extra"`
	Hidden string            `json:"-"`
	embedded
}

type embedded struct {
	Arch string `json:"architecture"`
}

func TestCheckPathAndDerive(t *testing.T) {
	rt := reflect.TypeFor[apitypes.Resource[props]]()
	for _, ok := range []string{"name", "properties.state", "properties.ports.P", "properties.extra.anything", "properties.architecture"} {
		p, _ := ParsePath(ok)
		if err := CheckPath(rt, p); err != nil {
			t.Errorf("CheckPath(%s): %v", ok, err)
		}
	}
	for _, bad := range []string{"nope", "properties.Hidden", "properties.state.x", "properties.embedded"} {
		p, _ := ParsePath(bad)
		if err := CheckPath(rt, p); err == nil {
			t.Errorf("CheckPath(%s) accepted", bad)
		}
	}
	var got []string
	for _, p := range DeriveColumns(rt) {
		got = append(got, p.String())
	}
	if want := []string{"id", "type", "name", "properties.state", "properties.architecture"}; !slices.Equal(got, want) {
		t.Errorf("DeriveColumns = %v, want %v", got, want)
	}
	got = nil
	for _, p := range DeriveColumns(reflect.TypeFor[apitypes.Operation]()) {
		got = append(got, p.String())
	}
	if !slices.Contains(got, "created") || !slices.Contains(got, "started") || slices.Contains(got, "result") || slices.Contains(got, "host") {
		t.Errorf("DeriveColumns(Operation) = %v", got)
	}
	_ = time.Time{}
}

func TestWriteLine(t *testing.T) {
	var b bytes.Buffer
	if err := WriteLine(&b, map[string]any{"message": "a <b> & `c`"}); err != nil {
		t.Fatal(err)
	}
	if b.String() != "{\"message\":\"a <b> & `c`\"}\n" {
		t.Errorf("WriteLine = %q", b.String())
	}
	if _, err := DecodeObject([]byte(`[1]`)); err == nil {
		t.Error("DecodeObject accepted an array")
	}
}
