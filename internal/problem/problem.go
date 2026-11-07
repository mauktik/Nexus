// Package problem implements RFC 9457 problem details and the registry of
// stable Nexus problem types (FR-8). Every non-2xx response, and every failed
// operation's error, uses this shape.
package problem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
)

// ContentType is the RFC 9457 media type for JSON problem details.
const ContentType = "application/problem+json"

// Error is an RFC 9457 problem details object. Extension members are written
// at the top level, beside the standard members.
type Error struct {
	Type       string // urn:nexus:problem:<slug>
	Title      string
	Status     int // HTTP status; 0 (omitted) for operation-level types
	Detail     string
	Instance   string
	Extensions map[string]any
}

// FieldError is one entry of validation-failed's errors[] member.
type FieldError struct {
	Path    string `json:"path"` // JSON Pointer into the input
	Message string `json:"message"`
}

// standardMembers are the RFC 9457 members an extension may not shadow.
var standardMembers = []string{"type", "title", "status", "detail", "instance"}

// New builds a problem of a registered type. It panics on an unregistered
// slug: types.go is the only place a problem type can be defined.
func New(slug Slug, detail string) *Error {
	t, ok := Lookup(slug)
	if !ok {
		panic(fmt.Sprintf("problem: unregistered slug %q", slug))
	}
	return &Error{Type: t.URI(), Title: t.Title, Status: t.Status, Detail: detail}
}

// With sets an extension member and returns e. It panics on a member the
// type's registry entry does not declare, or on a standard member name.
func (e *Error) With(key string, value any) *Error {
	if slices.Contains(standardMembers, key) {
		panic(fmt.Sprintf("problem: %q is a standard member, not an extension", key))
	}
	t, ok := LookupURI(e.Type)
	if !ok || !slices.Contains(t.Extensions, key) {
		panic(fmt.Sprintf("problem: type %q does not declare extension %q", e.Type, key))
	}
	if e.Extensions == nil {
		e.Extensions = map[string]any{}
	}
	e.Extensions[key] = value
	return e
}

// Slug returns the type's slug, or "" for a type outside the Nexus namespace.
func (e *Error) Slug() Slug {
	t, ok := LookupURI(e.Type)
	if !ok {
		return ""
	}
	return t.Slug
}

// Error implements error.
func (e *Error) Error() string {
	slug := string(e.Slug())
	if slug == "" {
		slug = e.Type
	}
	if e.Detail == "" {
		return slug
	}
	return slug + ": " + e.Detail
}

// MarshalJSON writes the standard members first, in RFC 9457 order, then the
// extensions sorted by name.
func (e Error) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	member := func(key string, value any) error {
		v, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("problem member %q: %w", key, err)
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(key) // Marshaling a string cannot fail
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
		n++
		return nil
	}

	type kv struct {
		key   string
		value any
	}
	members := []kv{{"type", e.Type}, {"title", e.Title}}
	if e.Status != 0 {
		members = append(members, kv{"status", e.Status})
	}
	if e.Detail != "" {
		members = append(members, kv{"detail", e.Detail})
	}
	if e.Instance != "" {
		members = append(members, kv{"instance", e.Instance})
	}
	keys := make([]string, 0, len(e.Extensions))
	for k := range e.Extensions {
		if !slices.Contains(standardMembers, k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		members = append(members, kv{k, e.Extensions[k]})
	}
	for _, m := range members {
		if err := member(m.key, m.value); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads the standard members and keeps every other top-level
// member as an extension, so a client tolerates extensions added later.
func (e *Error) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = Error{}
	fields := map[string]any{"type": &e.Type, "title": &e.Title, "status": &e.Status, "detail": &e.Detail, "instance": &e.Instance}
	for k, v := range raw {
		if dst, ok := fields[k]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return fmt.Errorf("problem member %q: %w", k, err)
			}
			continue
		}
		var ext any
		if err := json.Unmarshal(v, &ext); err != nil {
			return fmt.Errorf("problem member %q: %w", k, err)
		}
		if e.Extensions == nil {
			e.Extensions = map[string]any{}
		}
		e.Extensions[k] = ext
	}
	return nil
}

// Write sends e as an application/problem+json response with its status. An
// operation-level type, which has no status, is sent as 500. If e cannot be
// marshaled, a plain internal-error is sent instead.
func Write(w http.ResponseWriter, e *Error) error {
	body, err := json.Marshal(e)
	status := e.Status
	if err != nil {
		fallback := New(InternalError, "")
		body, _ = json.Marshal(fallback) // Only strings and an int: cannot fail
		status = fallback.Status
	}
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	if _, werr := w.Write(body); werr != nil {
		return fmt.Errorf("write problem response: %w", werr)
	}
	return err
}
