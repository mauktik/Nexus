// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

// writeTable writes a header and one row per object: host, then cols.
func writeTable(w io.Writer, cols []Path, objs []map[string]any, color bool) error {
	all := append([]Path{{"host"}}, cols...)
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	heads := make([]string, len(all))
	for i, c := range all {
		heads[i] = header(c)
	}
	fmt.Fprintln(tw, strings.Join(heads, "\t"))
	for _, o := range objs {
		cells := make([]string, len(all))
		for i, c := range all {
			cells[i] = cell(Get(o, c))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	out := b.Bytes()
	if color {
		// After alignment, so the escape bytes take no column width.
		head, rest, _ := bytes.Cut(out, []byte("\n"))
		out = append(append([]byte("\x1b[1m"), head...), append([]byte("\x1b[0m\n"), rest...)...)
	}
	_, err := w.Write(out)
	return err
}

// header names a column: its path, upper-cased, without a leading
// properties segment, which every resource column would share.
func header(p Path) string {
	if len(p) > 1 && p[0] == "properties" {
		p = p[1:]
	}
	return strings.ToUpper(p.String())
}

// cell renders a column's values: scalars as text, joined by ", " when the
// path crosses an array, and anything else as compact JSON.
func cell(vals []any) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		s, ok := scalarString(v)
		if !ok {
			data, err := json.Marshal(v)
			if err != nil {
				data = []byte(fmt.Sprint(v))
			}
			s = string(data)
		}
		parts = append(parts, Sanitize(s, false))
	}
	return strings.Join(parts, ", ")
}

// textOf is an object's text field for a terminal: a log frame's data, or
// a note for a truncation marker, which carries none.
func textOf(obj map[string]any, field string) string {
	if t, _ := obj["type"].(string); t == "truncated" {
		lost, _ := scalarString(obj["lostBytes"])
		return fmt.Sprintf("[%s bytes of output were not kept]\n", lost)
	}
	s, _ := obj[field].(string)
	return Sanitize(s, true)
}

// Sanitize makes control characters visible, so host-originated text
// (FR-28) cannot move the cursor or retitle a terminal. With keepLines,
// newline and tab pass through.
func Sanitize(s string, keepLines bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case keepLines && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			// C1 controls, and the bidirectional overrides that can make
			// text read differently from what it holds.
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// CheckPath reports whether p names a field of t's JSON form. A map, an
// interface or raw JSON on the way accepts any rest.
func CheckPath(t reflect.Type, p Path) error {
	for i, seg := range p {
		t = deref(t)
		if t == rawType || t.Kind() == reflect.Map || t.Kind() == reflect.Interface {
			return nil
		}
		if t.Kind() != reflect.Struct || t == timeType {
			return fmt.Errorf("column %s: %s is not an object", p, Path(p[:i]))
		}
		f, ok := jsonField(t, seg)
		if !ok {
			return fmt.Errorf("column %s: %s has no field %q", p, t, seg)
		}
		t = f
	}
	return nil
}

// deref steps through pointers and, for a path, through arrays.
func deref(t reflect.Type) reflect.Type {
	for {
		switch {
		case t.Kind() == reflect.Pointer:
			t = t.Elem()
		case t != rawType && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
			t = t.Elem()
		default:
			return t
		}
	}
}

// jsonField finds the field named name in t's JSON form, including the
// fields an untagged embedded struct promotes.
func jsonField(t reflect.Type, name string) (reflect.Type, bool) {
	for _, f := range jsonFields(t) {
		if f.name == name {
			return f.typ, true
		}
	}
	return nil, false
}

type field struct {
	name string
	typ  reflect.Type
}

func jsonFields(t reflect.Type) []field {
	var out []field
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && tag == "-" {
			continue
		}
		if f.Anonymous && name == "" && deref(f.Type).Kind() == reflect.Struct {
			out = append(out, jsonFields(deref(f.Type))...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, field{name, f.Type})
	}
	return out
}

// DeriveColumns is the column set of an op that declares none: the scalar
// top-level fields of t, then those of its properties, in declared order.
// host is left out; every table shows it first.
func DeriveColumns(t reflect.Type) []Path {
	var out []Path
	t = deref(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	for _, f := range jsonFields(t) {
		switch {
		case f.name == "host":
		case scalar(f.typ):
			out = append(out, Path{f.name})
		case f.name == "properties" && deref(f.typ).Kind() == reflect.Struct:
			for _, pf := range jsonFields(deref(f.typ)) {
				if scalar(pf.typ) {
					out = append(out, Path{f.name, pf.name})
				}
			}
		}
	}
	return out
}

func scalar(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
