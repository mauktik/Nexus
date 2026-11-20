// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"fmt"
	"slices"
	"strings"
)

// Path is a dot path into an object: properties.state is ["properties",
// "state"]. Through an array it applies to every element.
type Path []string

func (p Path) String() string { return strings.Join(p, ".") }

// ParsePath parses one dot path.
func ParsePath(s string) (Path, error) {
	p := Path(strings.Split(s, "."))
	for _, seg := range p {
		if seg == "" {
			return nil, fmt.Errorf("%q is not a dot path such as properties.state", s)
		}
	}
	return p, nil
}

// ParsePaths parses --select values, each a comma-separated list.
func ParsePaths(values []string) ([]Path, error) {
	var out []Path
	for _, v := range values {
		for _, s := range strings.Split(v, ",") {
			p, err := ParsePath(strings.TrimSpace(s))
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// identity are the fields every printed object keeps (FR-30).
var identity = []Path{{"host"}, {"id"}}

func (p Path) equal(q Path) bool { return slices.Equal(p, q) }

func isIdentity(p Path) bool { return slices.ContainsFunc(identity, p.equal) }

// selectColumns are the table columns after host for --select: id, as
// every selection keeps it (FR-30), then the paths given, each once.
func selectColumns(paths []Path) []Path {
	out := []Path{{"id"}}
	for _, p := range paths {
		if !isIdentity(p) && !slices.ContainsFunc(out, p.equal) {
			out = append(out, p)
		}
	}
	return out
}

// Select returns obj with only the paths given, plus host and id. A path
// obj lacks is left out; through an array it keeps each element's part.
func Select(obj map[string]any, paths []Path) map[string]any {
	out := map[string]any{}
	for _, p := range append(append([]Path{}, identity...), paths...) {
		if v, ok := pick(obj, p); ok {
			out = merge(out, v.(map[string]any)).(map[string]any)
		}
	}
	return out
}

// pick returns the part of v that p names, keeping the structure around it.
func pick(v any, p Path) (any, bool) {
	if len(p) == 0 {
		return clone(v), true // merge may write into it
	}
	switch v := v.(type) {
	case map[string]any:
		child, ok := v[p[0]]
		if !ok {
			return nil, false
		}
		sub, ok := pick(child, p[1:])
		if !ok {
			return nil, false
		}
		return map[string]any{p[0]: sub}, true
	case []any:
		out := make([]any, len(v))
		found := false
		for i, el := range v {
			if sub, ok := pick(el, p); ok {
				out[i], found = sub, true
			} else if _, isMap := el.(map[string]any); isMap {
				out[i] = map[string]any{} // Keeps positions aligned for merge
			}
		}
		return out, found
	}
	return nil, false
}

func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = clone(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = clone(x)
		}
		return out
	}
	return v
}

// merge folds b into a: maps key by key, arrays element by element.
func merge(a, b any) any {
	switch b := b.(type) {
	case map[string]any:
		am, ok := a.(map[string]any)
		if !ok {
			return b
		}
		for k, v := range b {
			if prev, ok := am[k]; ok {
				am[k] = merge(prev, v)
			} else {
				am[k] = v
			}
		}
		return am
	case []any:
		as, ok := a.([]any)
		if !ok || len(as) != len(b) {
			return b
		}
		for i := range as {
			if b[i] != nil {
				as[i] = merge(as[i], b[i])
			}
		}
		return as
	}
	return b
}

// Get returns the values at p, one per array element on the way; none
// when obj lacks p.
func Get(v any, p Path) []any {
	if len(p) == 0 {
		if arr, ok := v.([]any); ok {
			return arr
		}
		return []any{v}
	}
	switch v := v.(type) {
	case map[string]any:
		child, ok := v[p[0]]
		if !ok {
			return nil
		}
		return Get(child, p[1:])
	case []any:
		var out []any
		for _, el := range v {
			out = append(out, Get(el, p)...)
		}
		return out
	}
	return nil
}
