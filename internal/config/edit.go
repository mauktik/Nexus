package config

// Remote configuration editing (FR-6, FR-7): the key schema derived from the
// Config structs, assignment parsing, Edit (D-5) and Inspect.
//
// Edit splices a new value token into the file when it can, so the common
// case changes one line and keeps every comment and blank line. Otherwise it
// re-encodes the yaml.Node tree, which keeps every comment on its key but
// drops blank lines and comment alignment (A-10). Either way the result must
// decode to the original document with exactly the assignments applied.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// maxValueBytes bounds one assigned value (NFR-5). It equals
// nexusd/spec.MaxConfigValueBytes, which the API declares.
const maxValueBytes = 4096

// KeyKind is what a configuration key holds.
type KeyKind int

const (
	KindBool KeyKind = iota + 1
	KindInt
	KindDuration
	KindString
	KindList
	KindMapping // a struct: its keys are Children
	KindMap     // a map of names to values, such as a script's env
)

func (k KeyKind) String() string {
	switch k {
	case KindBool:
		return "boolean"
	case KindInt:
		return "integer"
	case KindDuration:
		return "duration"
	case KindString:
		return "string"
	case KindList:
		return "list"
	case KindMapping:
		return "mapping"
	case KindMap:
		return "map"
	}
	return "KeyKind(" + strconv.Itoa(int(k)) + ")"
}

// Key is one key of the schema.
type Key struct {
	Path      string // providers.host.enabled
	Kind      KeyKind
	Sensitive bool     // from the `sensitive:"true"` tag (Script.Env)
	Children  []string // for a mapping: its keys, in declaration order
}

// UnknownKeyError is a key path the schema does not have, with its nearest
// valid parent mapping ("" for the top level) and that parent's keys.
type UnknownKeyError struct {
	Path, Parent string
	Children     []string
}

func (e *UnknownKeyError) Error() string {
	return fmt.Sprintf("unknown key %q; %s", e.Path, e.detail())
}

func (e *UnknownKeyError) detail() string {
	if e.Parent == "" {
		return "the top-level keys are: " + strings.Join(e.Children, ", ")
	}
	return fmt.Sprintf("the nearest valid parent is %s, whose keys are: %s", e.Parent, strings.Join(e.Children, ", "))
}

// schemaKey is a Key plus how to reach its value in a Config.
type schemaKey struct {
	Key
	typ   reflect.Type // the Go type, with Section[T] unwrapped to T
	index []int        // reflect.Value.FieldByIndex from Config
}

type keySchema struct {
	keys   map[string]*schemaKey
	leaves []*schemaKey // declaration order
	root   []string     // top-level keys
}

var schema = sync.OnceValue(func() *keySchema {
	s := &keySchema{keys: map[string]*schemaKey{}}
	s.root = s.walk(reflect.TypeOf(Config{}), "", nil)
	return s
})

// walk adds the fields of struct type t below path and returns their names.
func (s *keySchema) walk(t reflect.Type, path string, index []int) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		name := yamlName(f)
		if name == "" {
			continue
		}
		names = append(names, name)
		k := &schemaKey{
			Key:   Key{Path: join(path, name), Sensitive: f.Tag.Get("sensitive") == "true"},
			typ:   f.Type,
			index: append(slices.Clip(index), i),
		}
		if st, ok := sectionSettings(f.Type); ok {
			k.typ = st
			k.index = append(k.index, 0) // Section.Settings
		}
		k.Kind = kindOf(k.typ)
		s.keys[k.Path] = k
		if k.Kind == KindMapping {
			k.Children = s.walk(k.typ, k.Path, k.index)
		} else {
			s.leaves = append(s.leaves, k)
		}
	}
	return names
}

// sectionSettings unwraps Section[T] to T.
func sectionSettings(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	if _, ok := reflect.New(t).Interface().(sectionTarget); !ok {
		return nil, false
	}
	return t.Field(0).Type, true
}

func kindOf(t reflect.Type) KeyKind {
	switch {
	case t == durationType:
		return KindDuration
	case t.Kind() == reflect.Bool:
		return KindBool
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		return KindInt
	case t.Kind() == reflect.String:
		return KindString
	case t.Kind() == reflect.Slice:
		return KindList
	case t.Kind() == reflect.Map:
		return KindMap
	case t.Kind() == reflect.Struct:
		return KindMapping
	}
	panic("config: no key kind for " + t.String())
}

func (k *schemaKey) export() Key {
	out := k.Key
	out.Children = slices.Clone(k.Children)
	return out
}

func (s *keySchema) lookup(path string) (*schemaKey, error) {
	if k, ok := s.keys[path]; ok {
		return k, nil
	}
	parent, children := "", s.root
	parts := strings.Split(path, ".")
	for i := len(parts) - 1; i > 0; i-- {
		if k, ok := s.keys[strings.Join(parts[:i], ".")]; ok && k.Kind == KindMapping {
			parent, children = k.Path, k.Children
			break
		}
	}
	return nil, &UnknownKeyError{Path: path, Parent: parent, Children: slices.Clone(children)}
}

// LookupKey returns the schema key at a dotted path, or an *UnknownKeyError
// naming the nearest valid parent.
func LookupKey(path string) (Key, error) {
	k, err := schema().lookup(path)
	if err != nil {
		return Key{}, err
	}
	return k.export(), nil
}

// LeafKeys returns every non-mapping key in declaration order. The script
// catalog is one leaf: its entries are not keys.
func LeafKeys() []Key {
	leaves := schema().leaves
	out := make([]Key, len(leaves))
	for i, k := range leaves {
		out[i] = k.export()
	}
	return out
}

// Assignment is one KEY=VALUE.
type Assignment struct{ Key, Value string }

// ParseAssignment splits s at the first '='. Errors name only the key part,
// because the value may be a secret.
func ParseAssignment(s string) (Assignment, error) {
	key, value, ok := strings.Cut(s, "=")
	switch {
	case !ok:
		return Assignment{}, fmt.Errorf("%q has no '=': want KEY=VALUE", key)
	case key == "":
		return Assignment{}, errors.New("an assignment has an empty key: want KEY=VALUE")
	}
	return Assignment{Key: key, Value: value}, nil
}

// Change is what one assignment did.
type Change struct {
	Key      string
	Old, New any  // typed (bool, int64, string); Old nil when absent
	Present  bool // the key was in the file
}

// parseValue checks v against k's kind (A-30) and returns its typed value.
// The message names the expected type and never the value.
func parseValue(k *schemaKey, v string) (any, string) {
	switch k.Kind {
	case KindBool:
		switch v {
		case "true":
			return true, ""
		case "false":
			return false, ""
		}
		return nil, "invalid value: expected a boolean, true or false"
	case KindInt:
		for i, c := range v {
			if (c < '0' || c > '9') && (i > 0 || c != '-') {
				return nil, "invalid value: expected a decimal integer"
			}
		}
		n, err := strconv.ParseInt(v, 10, k.typ.Bits())
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				lo, hi := int64(-1)<<(k.typ.Bits()-1), int64(1)<<(k.typ.Bits()-1)-1
				return nil, fmt.Sprintf("invalid value: expected a decimal integer from %d to %d", lo, hi)
			}
			return nil, "invalid value: expected a decimal integer"
		}
		return n, ""
	case KindDuration:
		if _, err := time.ParseDuration(v); err != nil {
			return nil, "invalid value: expected a duration such as 30s or 1h"
		}
		return v, ""
	case KindString:
		switch {
		case len(v) > maxValueBytes:
			return nil, fmt.Sprintf("invalid value: expected a string of at most %d bytes", maxValueBytes)
		case !utf8.ValidString(v):
			return nil, "invalid value: expected a string of valid UTF-8"
		case strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			return nil, "invalid value: expected a string without control characters"
		}
		return v, ""
	case KindList:
		return nil, "is a list, which cannot be set: edit the file to change it"
	case KindMap:
		return nil, "is a mapping, which cannot be set: edit the file to change it"
	}
	return nil, fmt.Sprintf("is a mapping, which cannot be set; set one of its keys: %s", strings.Join(k.Children, ", "))
}

// oldValue is the typed value a present scalar node holds, or nil.
func oldValue(k *schemaKey, n *yaml.Node) any {
	if n.Kind != yaml.ScalarNode || n.ShortTag() == "!!null" {
		return nil
	}
	switch k.Kind {
	case KindBool:
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case KindInt:
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
	}
	return n.Value
}

// planned is one checked assignment.
type planned struct {
	key     *schemaKey
	parts   []string
	node    *yaml.Node // the value node in the file; nil when absent
	repl    *yaml.Node // the new value as a lone scalar node
	token   string     // repl rendered by the encoder
	generic any        // repl decoded generically, for the semantic check
	change  Change
}

// Edit applies as to data (D-5) and returns the new bytes and one Change per
// assignment. Errors are an ErrorList located by key path: unknown key (with
// nearest parent), list or mapping (OOS-3), wrong type, alias/anchor target,
// over-long value. It never validates the result; callers do (CheckConfigData).
func Edit(data []byte, file string, as []Assignment) ([]byte, []Change, error) {
	doc, err := parseDocument(data, file)
	if err != nil {
		return nil, nil, err
	}
	errs := collector{file: file}
	plans := make([]*planned, 0, len(as))
	seen := map[string]bool{}
	for _, a := range as {
		if p := plan(doc, a, seen, &errs); p != nil {
			plans = append(plans, p)
		}
	}
	if err := errs.err(); err != nil {
		return nil, nil, err
	}
	changes := make([]Change, len(plans))
	for i, p := range plans {
		changes[i] = p.change
	}

	if out, ok := splice(data, plans); ok && verify(data, out, plans) {
		return out, changes, nil
	}
	out, err := reencode(doc, plans)
	if err != nil {
		return nil, nil, err
	}
	if !verify(data, out, plans) {
		return nil, nil, errors.New("internal error: the edited configuration failed its semantic check")
	}
	return out, changes, nil
}

// parseDocument parses data as a single YAML document whose root is a
// mapping, creating an empty one for an empty file.
func parseDocument(data []byte, file string) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	switch err := dec.Decode(&doc); {
	case errors.Is(err, io.EOF):
		doc = yaml.Node{Kind: yaml.DocumentNode}
	case err != nil:
		return nil, ErrorList{syntaxError(file, err)}
	default:
		var extra yaml.Node
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, ErrorList{{File: file, Line: extra.Line, Msg: "the file must hold a single YAML document"}}
		}
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	return &doc, nil
}

// plan checks one assignment against the schema and the file.
func plan(doc *yaml.Node, a Assignment, seen map[string]bool, errs *collector) *planned {
	k, err := schema().lookup(a.Key)
	if err != nil {
		var u *UnknownKeyError
		errors.As(err, &u)
		errs.add(0, 0, a.Key, "unknown key; "+u.detail())
		return nil
	}
	if seen[a.Key] {
		errs.add(0, 0, a.Key, "assigned more than once")
		return nil
	}
	seen[a.Key] = true

	p := &planned{key: k, parts: strings.Split(a.Key, ".")}
	n := doc.Content[0]
	for i, part := range p.parts {
		if msg := shared(n, i == 0); msg != "" {
			errs.add(n.Line, n.Column, a.Key, msg)
			return nil
		}
		if n.Kind != yaml.MappingNode {
			if isNull(n) {
				n = nil // a null parent is an absent one
				break
			}
			errs.addf(n.Line, n.Column, a.Key, "%s is %s, not a mapping", strings.Join(p.parts[:i], "."), describe(n))
			return nil
		}
		var next *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == part {
				next = n.Content[j+1]
				break
			}
		}
		if n = next; n == nil {
			break
		}
	}
	p.node = n

	line, col := 0, 0
	if n != nil {
		line, col = n.Line, n.Column
		switch {
		case n.Kind == yaml.AliasNode:
			errs.add(line, col, a.Key, "is an alias of another value: edit the file to change it")
			return nil
		case n.Anchor != "":
			errs.add(line, col, a.Key, "carries an anchor that other values may refer to: edit the file to change it")
			return nil
		case n.Style&yaml.TaggedStyle != 0:
			errs.add(line, col, a.Key, "has an explicit tag: edit the file to change it")
			return nil
		}
	}
	v, msg := parseValue(k, a.Value)
	if msg != "" {
		errs.add(line, col, a.Key, msg)
		return nil
	}

	p.repl = &yaml.Node{}
	if err := p.repl.Encode(v); err != nil {
		errs.add(line, col, a.Key, "cannot encode the value")
		return nil
	}
	b, err := yaml.Marshal(p.repl)
	if err == nil {
		err = p.repl.Decode(&p.generic)
	}
	if err != nil {
		errs.add(line, col, a.Key, "cannot encode the value")
		return nil
	}
	p.token = strings.TrimSuffix(string(b), "\n")
	p.change = Change{Key: a.Key, New: v, Present: n != nil}
	if n != nil {
		p.change.Old = oldValue(k, n)
	}
	return p
}

// shared reports a mapping on the path that an edit would change for
// other keys too: an alias, or one carrying an anchor.
func shared(n *yaml.Node, root bool) string {
	switch {
	case n.Kind == yaml.AliasNode:
		return "a parent of the key is an alias of another mapping: edit the file to change it"
	case n.Anchor != "" && n.Kind == yaml.MappingNode:
		if root {
			return "the document carries an anchor: edit the file to change it"
		}
		return "a parent of the key carries an anchor that other values may refer to: edit the file to change it"
	}
	return ""
}

// splice replaces each assigned scalar's source token in place. It reports
// false when any assignment needs the tree edited instead.
func splice(data []byte, plans []*planned) ([]byte, bool) {
	type span struct {
		start, end int
		token      string
	}
	spans := make([]span, 0, len(plans))
	lineStarts := []int{0}
	for i, c := range data {
		if c == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}
	for _, p := range plans {
		start, end, ok := tokenExtent(data, lineStarts, p.node)
		if !ok || strings.Contains(p.token, "\n") {
			return nil, false
		}
		spans = append(spans, span{start, end, p.token})
	}
	slices.SortFunc(spans, func(a, b span) int { return b.start - a.start })
	out := slices.Clone(data)
	for _, s := range spans {
		out = slices.Concat(out[:s.start], []byte(s.token), out[s.end:])
	}
	return out, true
}

// tokenExtent finds the byte range of a single-line plain or quoted scalar's
// source token. Node.Column counts runes from 1 (A-9).
func tokenExtent(data []byte, lineStarts []int, n *yaml.Node) (start, end int, ok bool) {
	if n == nil || n.Kind != yaml.ScalarNode || n.Line < 1 || n.Line > len(lineStarts) || n.Column < 1 {
		return 0, 0, false
	}
	start = lineStarts[n.Line-1]
	for range n.Column - 1 {
		if start >= len(data) || data[start] == '\n' {
			return 0, 0, false
		}
		_, size := utf8.DecodeRune(data[start:])
		start += size
	}
	rest := data[start:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	var token string
	switch n.Style {
	case 0:
		if n.Value == "" {
			return 0, 0, false // an empty null has no token to replace
		}
		token = n.Value
	case yaml.SingleQuotedStyle:
		token = "'" + strings.ReplaceAll(n.Value, "'", "''") + "'"
	case yaml.DoubleQuotedStyle:
		if len(rest) == 0 || rest[0] != '"' {
			return 0, 0, false
		}
		for i := 1; i < len(rest); i++ {
			switch rest[i] {
			case '\\':
				i++
			case '"':
				return start, start + i + 1, true
			}
		}
		return 0, 0, false // the closing quote is on a later line
	default:
		return 0, 0, false
	}
	if !bytes.HasPrefix(rest, []byte(token)) {
		return 0, 0, false // a multi-line or otherwise unusual token
	}
	return start, start + len(token), true
}

// reencode edits the node tree and encodes it at indent 2.
func reencode(doc *yaml.Node, plans []*planned) ([]byte, error) {
	for _, p := range plans {
		n := p.node
		if n == nil {
			n = createPath(doc.Content[0], p.parts)
		}
		n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, p.repl.Tag, p.repl.Value, p.repl.Style
		n.Content, n.Alias = nil, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("internal error: re-encoding the configuration: %v", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("internal error: re-encoding the configuration: %v", err)
	}
	return buf.Bytes(), nil
}

// createPath returns the value node at parts below the root mapping,
// appending missing keys and turning null parents into mappings.
func createPath(n *yaml.Node, parts []string) *yaml.Node {
	for _, part := range parts {
		if isNull(n) {
			n.Kind, n.Tag, n.Value, n.Style = yaml.MappingNode, "!!map", "", 0
		}
		var next *yaml.Node
		for j := 0; j+1 < len(n.Content); j += 2 {
			if n.Content[j].Value == part {
				next = n.Content[j+1]
				break
			}
		}
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
		}
		n = next
	}
	return n
}

// verify reports whether out holds one document that decodes generically to
// orig's with exactly the assignments applied.
func verify(orig, out []byte, plans []*planned) bool {
	var want any
	if err := yaml.Unmarshal(orig, &want); err != nil {
		return false
	}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	var got any
	if err := dec.Decode(&got); err != nil {
		return false
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return false
	}
	for _, p := range plans {
		var ok bool
		if want, ok = setGeneric(want, p.parts, p.generic); !ok {
			return false
		}
	}
	return reflect.DeepEqual(want, got)
}

func setGeneric(v any, parts []string, value any) (any, bool) {
	if len(parts) == 0 {
		return value, true
	}
	if v == nil {
		v = map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	child, ok := setGeneric(m[parts[0]], parts[1:], value)
	m[parts[0]] = child
	return m, ok
}

// Sources of an Entry.
const SourceFile, SourceDefault = "file", "default"

// Redacted replaces a sensitive value. It equals audit.Redacted.
const Redacted = "[REDACTED]"

// Entry is one key as Inspect reports it. Value is as written in the file
// (nil when absent) and Effective is what Parse gives, durations in their
// short form. For a mapping, Value is a map of its child entries by key name
// and Effective a map of their effective values.
type Entry struct {
	Key              string
	Value, Effective any
	Source           string
}

// Inspect reports one key, a leaf or a mapping, of the file data. An unknown
// key is an *UnknownKeyError; a file that does not parse is Parse's
// ErrorList.
func Inspect(data []byte, file, key string) (Entry, error) {
	k, err := schema().lookup(key)
	if err != nil {
		return Entry{}, err
	}
	in, err := newInspection(data, file)
	if err != nil {
		return Entry{}, err
	}
	return in.entry(k), nil
}

// InspectAll reports every leaf key in LeafKeys order.
func InspectAll(data []byte, file string) ([]Entry, error) {
	in, err := newInspection(data, file)
	if err != nil {
		return nil, err
	}
	leaves := schema().leaves
	out := make([]Entry, len(leaves))
	for i, k := range leaves {
		out[i] = in.entry(k)
	}
	return out, nil
}

type inspection struct {
	cfg  reflect.Value
	root *yaml.Node
}

func newInspection(data []byte, file string) (*inspection, error) {
	cfg, err := Parse(data, file)
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(data, file)
	if err != nil {
		return nil, err
	}
	return &inspection{cfg: reflect.ValueOf(cfg).Elem(), root: doc.Content[0]}, nil
}

func (in *inspection) entry(k *schemaKey) Entry {
	n := in.root
	for _, part := range strings.Split(k.Path, ".") {
		n = mappingValue(n, part)
	}
	e := Entry{Key: k.Path, Source: SourceDefault}
	if n != nil {
		e.Source = SourceFile
	}
	if k.Kind == KindMapping {
		children := make(map[string]Entry, len(k.Children))
		effective := make(map[string]any, len(k.Children))
		for _, c := range k.Children {
			ce := in.entry(schema().keys[join(k.Path, c)])
			children[c], effective[c] = ce, ce.Effective
		}
		e.Value, e.Effective = children, effective
		return e
	}
	e.Effective = effectiveValue(in.cfg.FieldByIndex(k.index), k.Sensitive)
	if n != nil {
		e.Value = writtenValue(k, n)
	}
	return e
}

// mappingValue returns the value of key in mapping n, following aliases, or
// nil.
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if resolveAlias(n.Content[i]).Value == key {
			return resolveAlias(n.Content[i+1])
		}
	}
	return nil
}

// writtenValue is a present leaf's value as written, typed by its kind.
// Parse has already accepted it.
func writtenValue(k *schemaKey, n *yaml.Node) any {
	if isNull(n) {
		return nil
	}
	switch k.Kind {
	case KindBool, KindInt, KindDuration, KindString:
		return oldValue(k, n)
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil
	}
	return redactGeneric(v, k.typ, k.Sensitive)
}

// redactGeneric replaces sensitive values in a generic decode of a value of
// Go type t: a sensitive map keeps its keys and loses its values.
func redactGeneric(v any, t reflect.Type, sensitive bool) any {
	if sensitive {
		if m, ok := v.(map[string]any); ok {
			out := make(map[string]any, len(m))
			for name := range m {
				out[name] = Redacted
			}
			return out
		}
		return Redacted
	}
	switch t.Kind() {
	case reflect.Slice:
		if list, ok := v.([]any); ok {
			out := make([]any, len(list))
			for i, item := range list {
				out[i] = redactGeneric(item, t.Elem(), false)
			}
			return out
		}
	case reflect.Struct:
		if m, ok := v.(map[string]any); ok && t != durationType {
			out := make(map[string]any, len(m))
			for i := range t.NumField() {
				f := t.Field(i)
				if name := yamlName(f); name != "" {
					if fv, ok := m[name]; ok {
						out[name] = redactGeneric(fv, f.Type, f.Tag.Get("sensitive") == "true")
					}
				}
			}
			return out
		}
	}
	return v
}

// effectiveValue converts a decoded configuration value to plain data:
// durations in their short form, structs as maps by yaml name, and the
// values of a sensitive map redacted.
func effectiveValue(v reflect.Value, sensitive bool) any {
	if v.Type() == durationType {
		return shortDuration(time.Duration(v.Int()))
	}
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.String:
		if sensitive {
			return Redacted
		}
		return v.String()
	case reflect.Slice:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = effectiveValue(v.Index(i), sensitive)
		}
		return out
	case reflect.Map:
		out := make(map[string]any, v.Len())
		for it := v.MapRange(); it.Next(); {
			if sensitive {
				out[it.Key().String()] = Redacted
			} else {
				out[it.Key().String()] = effectiveValue(it.Value(), false)
			}
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if name := yamlName(f); name != "" {
				out[name] = effectiveValue(v.Field(i), f.Tag.Get("sensitive") == "true")
			}
		}
		return out
	}
	return nil
}
