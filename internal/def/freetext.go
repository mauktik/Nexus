// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package def

import "github.com/google/jsonschema-go/jsonschema"

// FreeTextKeyword marks a string input property as deliberately free text: a
// value the provider passes on only as inert data, never as a command, flag
// or path. Refine sets it to true on the property's schema. The conformance
// suite accepts a string property only when it has enum, pattern or this
// marker (A-42).
const FreeTextKeyword = "x-nexus-free-text"

// MarkFreeText sets FreeTextKeyword on s.
func MarkFreeText(s *jsonschema.Schema) {
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	s.Extra[FreeTextKeyword] = true
}
