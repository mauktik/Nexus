// Package output renders the objects a data command returns (FR-30): as a
// table, JSON, JSON Lines or YAML, with dot-path field selection and
// client-side filtering. It writes results only; the caller sends
// progress, warnings and errors to stderr.
package output

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// Format is an output format.
type Format string

const (
	Table Format = "table"
	JSON  Format = "json"  // One indented array of every object
	JSONL Format = "jsonl" // One compact object per line, as printed
	YAML  Format = "yaml"  // One sequence of every object
)

// Formats lists the formats in the order usage shows them.
var Formats = []Format{Table, JSON, JSONL, YAML}

// ParseFormat parses an -o value.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	return "", fmt.Errorf("-o must be table, json, jsonl or yaml, not %q", s)
}

// DefaultFormat is table on a terminal and jsonl otherwise (FR-30).
func DefaultFormat(tty bool) Format {
	if tty {
		return Table
	}
	return JSONL
}

// Machine reports whether f is for programs: errors are then JSON.
func (f Format) Machine() bool { return f == JSON || f == JSONL || f == YAML }

// ColorEnabled reports whether a table may use color: only on a terminal,
// and not when NO_COLOR is set to any non-empty value (https://no-color.org)
// or TERM is dumb.
func ColorEnabled(tty bool, getenv func(string) string) bool {
	return tty && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// Options configure a Printer.
type Options struct {
	Format  Format
	Columns []Path  // Table columns after host; --select replaces them
	Select  []Path  // --select: keep only these paths, plus host and id
	Filters []Match // Client-side --filter matches
	Color   bool    // A bold table header; never set without a terminal
	// Text, in a table without Select, prints this field of each object
	// verbatim instead of a row: the text of a log frame.
	Text string
}

// Printer writes objects to one writer in one format. jsonl writes each
// object as it is printed; the other formats write on Close.
type Printer struct {
	w    io.Writer
	o    Options
	objs []map[string]any
}

// NewPrinter returns a printer writing to w.
func NewPrinter(w io.Writer, o Options) *Printer { return &Printer{w: w, o: o} }

// Print filters, selects and prints one object.
func (p *Printer) Print(obj map[string]any) error {
	if !MatchAll(obj, p.o.Filters) {
		return nil
	}
	if len(p.o.Select) > 0 {
		obj = Select(obj, p.o.Select)
	}
	switch {
	case p.o.Format == JSONL:
		return WriteLine(p.w, obj)
	case p.o.Format == Table && p.o.Text != "" && len(p.o.Select) == 0:
		_, err := io.WriteString(p.w, textOf(obj, p.o.Text))
		return err
	}
	p.objs = append(p.objs, obj)
	return nil
}

// Close writes what the format holds back until the end.
func (p *Printer) Close() error {
	objs := p.objs
	if objs == nil {
		objs = []map[string]any{}
	}
	switch p.o.Format {
	case JSON:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(objs); err != nil {
			return err
		}
		_, err := p.w.Write(b.Bytes())
		return err
	case YAML:
		seq := make([]any, len(objs))
		for i, o := range objs {
			seq[i] = yamlValue(o)
		}
		data, err := yaml.Marshal(seq)
		if err != nil {
			return err
		}
		_, err = p.w.Write(data)
		return err
	case Table:
		if p.o.Text != "" && len(p.o.Select) == 0 {
			return nil
		}
		cols := p.o.Columns
		if len(p.o.Select) > 0 {
			cols = p.o.Select
		}
		bw := bufio.NewWriter(p.w)
		if err := writeTable(bw, cols, objs, p.o.Color); err != nil {
			return err
		}
		return bw.Flush()
	}
	return nil
}

// DecodeObject decodes one JSON object, keeping numbers exact.
func DecodeObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return m, nil
}

// WriteLine writes v as one compact JSON line, without HTML escaping.
func WriteLine(w io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
}

// yamlValue turns json.Number into a YAML number, which the YAML encoder
// would otherwise quote as the string it is.
func yamlValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = yamlValue(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = yamlValue(x)
		}
		return out
	case json.Number:
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil {
			return f
		}
		return string(v)
	}
	return v
}
