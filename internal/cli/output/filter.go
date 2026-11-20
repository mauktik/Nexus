// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package output

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Match is one --filter key=value.
type Match struct {
	Key   string // As given: a declared server key or a dot path
	Path  Path
	Value string
}

// ParseFilters parses --filter values. The value may itself hold '='.
func ParseFilters(values []string) ([]Match, error) {
	out := make([]Match, 0, len(values))
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--filter %q: want key=value", v)
		}
		p, err := ParsePath(k)
		if err != nil {
			return nil, fmt.Errorf("--filter %q: %w", v, err)
		}
		out = append(out, Match{Key: k, Path: p, Value: val})
	}
	return out, nil
}

// SplitFilters sends a match whose key the op declares to the host, as
// key=value, and keeps the rest to apply here.
func SplitFilters(ms []Match, serverKeys []string) (server []string, client []Match) {
	for _, m := range ms {
		if slices.Contains(serverKeys, m.Key) {
			server = append(server, m.Key+"="+m.Value)
		} else {
			client = append(client, m)
		}
	}
	return server, client
}

// MatchAll reports whether obj passes every key's matches. Matches with the
// same key are alternatives, as repeated server-side keys are; a path
// through an array matches when any element does.
func MatchAll(obj map[string]any, ms []Match) bool {
	byKey := map[string]bool{}
	var keys []string
	for _, m := range ms {
		if _, seen := byKey[m.Key]; !seen {
			keys = append(keys, m.Key)
			byKey[m.Key] = false
		}
		if !byKey[m.Key] && matches(obj, m) {
			byKey[m.Key] = true
		}
	}
	for _, k := range keys {
		if !byKey[k] {
			return false
		}
	}
	return true
}

func matches(obj map[string]any, m Match) bool {
	for _, v := range Get(obj, m.Path) {
		if s, ok := scalarString(v); ok && s == m.Value {
			return true
		}
	}
	return false
}

// scalarString is a scalar as a filter compares it and a table shows it.
func scalarString(v any) (string, bool) {
	switch v := v.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	}
	return "", false
}
