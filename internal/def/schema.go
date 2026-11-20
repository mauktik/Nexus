// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package def

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// jsonschema.For derives types, required properties and, for every struct,
// additionalProperties: false. It does not derive enum or pattern (A-42): a
// string property accepts any value unless Refine constrains it, which the
// conformance suite enforces (4.2).

type schemaKey struct {
	id     string
	output bool
}

type schemaEntry struct {
	once     sync.Once
	resolved *jsonschema.Resolved
	err      error
}

var schemaCache sync.Map // schemaKey -> *schemaEntry

// InputSchema returns the resolved input schema: jsonschema.For of the input
// type with Refine applied. It is computed once per op and cached, so Refine
// runs at most once.
func (o Op) InputSchema() (*jsonschema.Resolved, error) {
	return o.schema(false)
}

// OutputSchema returns the resolved output schema. Refine does not apply.
func (o Op) OutputSchema() (*jsonschema.Resolved, error) {
	return o.schema(true)
}

func (o Op) schema(output bool) (*jsonschema.Resolved, error) {
	t := o.inputType
	if output {
		t = o.outputType
	}
	if t == nil {
		return nil, fmt.Errorf("def: op %q was not built by Define", o.ID)
	}
	v, _ := schemaCache.LoadOrStore(schemaKey{id: o.ID, output: output}, &schemaEntry{})
	e := v.(*schemaEntry)
	e.once.Do(func() {
		var refine func(*jsonschema.Schema)
		if !output {
			refine = o.Refine
		}
		e.resolved, e.err = resolve(t, refine)
		if e.err != nil {
			e.err = fmt.Errorf("def: op %q: %w", o.ID, e.err)
		}
	})
	return e.resolved, e.err
}

func resolve(t reflect.Type, refine func(*jsonschema.Schema)) (*jsonschema.Resolved, error) {
	s, err := jsonschema.ForType(t, nil)
	if err != nil {
		return nil, fmt.Errorf("generate schema for %v: %w", t, err)
	}
	if refine != nil {
		refine(s)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve schema for %v: %w", t, err)
	}
	return r, nil
}
