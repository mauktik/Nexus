// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// The strict decoder exists because yaml.Node.Decode builds a fresh internal
// decoder with knownFields off, so yaml.Decoder.KnownFields never reaches a
// section decoded from its node (go.yaml.in/yaml/v3 v3.0.6, yaml.go:140).
// It walks mappings against struct fields itself, reports every unknown key
// at the key node's line and column, and hands only leaf values to yaml.

var durationType = reflect.TypeOf(time.Duration(0))

// position is a 1-based line and column in the file.
type position struct{ line, col int }

// locations maps a dotted key path to the position of its value.
type locations map[string]position

type decoder struct {
	errs collector
	locs locations
}

func newDecoder(file string) *decoder {
	return &decoder{errs: collector{file: file}, locs: locations{}}
}

// decode decodes n into out, a non-nil pointer, keeping values already in
// out where the document is silent so defaults survive.
func (d *decoder) decode(n *yaml.Node, out any, path string) {
	d.value(n, reflect.ValueOf(out).Elem(), path)
}

func (d *decoder) value(n *yaml.Node, v reflect.Value, path string) {
	n = resolveAlias(n)
	d.locs[path] = position{n.Line, n.Column}

	if v.CanAddr() {
		if s, ok := v.Addr().Interface().(sectionTarget); ok {
			s.setNode(n, d.errs.file, path)
			d.value(n, reflect.ValueOf(s.settings()).Elem(), path)
			return
		}
	}

	switch {
	case v.Kind() == reflect.Struct && v.Type() != durationType:
		d.mapping(n, v, path)
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Struct:
		d.sequence(n, v, path)
	default:
		d.leaf(n, v, path)
	}
}

func (d *decoder) mapping(n *yaml.Node, v reflect.Value, path string) {
	if isNull(n) {
		return
	}
	if n.Kind != yaml.MappingNode {
		d.errs.addf(n.Line, n.Column, path, "expected a mapping, got %s", describe(n))
		return
	}
	fields := yamlFields(v.Type())
	seen := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := resolveAlias(n.Content[i]), n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			d.errs.addf(k.Line, k.Column, path, "expected a key name, got %s", describe(k))
			continue
		}
		kp := join(path, k.Value)
		if prev, dup := seen[k.Value]; dup {
			d.errs.addf(k.Line, k.Column, kp, "key %q already defined at line %d", k.Value, prev.Line)
			continue
		}
		seen[k.Value] = k
		idx, ok := fields[k.Value]
		if !ok {
			d.errs.addf(k.Line, k.Column, kp, "unknown key %q", k.Value)
			continue
		}
		d.value(val, v.Field(idx), kp)
	}
}

func (d *decoder) sequence(n *yaml.Node, v reflect.Value, path string) {
	if isNull(n) {
		return
	}
	if n.Kind != yaml.SequenceNode {
		d.errs.addf(n.Line, n.Column, path, "expected a sequence, got %s", describe(n))
		return
	}
	s := reflect.MakeSlice(v.Type(), len(n.Content), len(n.Content))
	for i, item := range n.Content {
		d.value(item, s.Index(i), path+"["+strconv.Itoa(i)+"]")
	}
	v.Set(s)
}

// leaf hands a scalar, list or map value to yaml. The value is decoded into a
// fresh variable and assigned only on success, so a bad value leaves the
// default in place and is reported once, here.
func (d *decoder) leaf(n *yaml.Node, v reflect.Value, path string) {
	if v.Type() == durationType {
		d.duration(n, v, path)
		return
	}
	tmp := reflect.New(v.Type())
	if err := n.Decode(tmp.Interface()); err != nil {
		d.errs.addf(n.Line, n.Column, path, "invalid value: expected %s, got %s", expectedType(v.Type()), describe(n))
		return
	}
	v.Set(tmp.Elem())
}

// duration parses Go duration strings ("90s", "1h30m") only. Bare numbers
// are refused so a unit is never guessed.
func (d *decoder) duration(n *yaml.Node, v reflect.Value, path string) {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		d.errs.addf(n.Line, n.Column, path, "invalid duration %s: want a string such as \"30s\" or \"1h\"", describe(n))
		return
	}
	dur, err := time.ParseDuration(n.Value)
	if err != nil {
		d.errs.addf(n.Line, n.Column, path, "invalid duration %q: want a string such as \"30s\" or \"1h\"", n.Value)
		return
	}
	v.SetInt(int64(dur))
}

// yamlFields maps yaml key names to field indexes for exported, tagged fields.
func yamlFields(t reflect.Type) map[string]int {
	m := make(map[string]int, t.NumField())
	for i := range t.NumField() {
		if name := yamlName(t.Field(i)); name != "" {
			m[name] = i
		}
	}
	return m
}

func yamlName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "-" {
		return ""
	}
	return name
}

// resolveAlias follows alias nodes. The parser rejects alias cycles.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// describe names what a node is, never what it holds: the value may be a
// secret (a script env entry, for one), and validate-config output reaches
// every nexusd:read caller.
func describe(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str":
			return "a string"
		case "!!int", "!!float":
			return "a number"
		case "!!bool":
			return "a boolean"
		case "!!null":
			return "null"
		}
		return "a scalar"
	default:
		return "an unexpected node"
	}
}

// expectedType names what a leaf field accepts, from its Go type. yaml's own
// decode error is not used: it quotes a prefix of the offending value.
func expectedType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Map:
		return "a mapping of NAME: value"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	default:
		return "a different kind of value"
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
