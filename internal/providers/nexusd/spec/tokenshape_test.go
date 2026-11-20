// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package spec_test

// The token name's not-token-shape refine (WI 2.2 amendment, NFR-1): its
// pattern is internal/auth's token format, so every token auth mints is
// refused as a name by add and remove, while names and IDs still pass.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// authConst is the literal value of the constant name in
// internal/auth/tokens.go.
func authConst(t *testing.T, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "..", "auth", "tokens.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	val := ""
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != name || len(vs.Values) != 1 {
			return true
		}
		if lit, ok := vs.Values[0].(*ast.BasicLit); ok {
			val = lit.Value
		}
		return false
	})
	if val == "" {
		t.Fatalf("internal/auth/tokens.go has no constant %s = <literal>", name)
	}
	if s, err := strconv.Unquote(val); err == nil {
		return s
	}
	return val
}

func TestTokenNameRefusesTheTokenStringShape(t *testing.T) {
	add, remove := schemaOf(t, spec.TokensAdd.Op()), schemaOf(t, spec.TokensRemove.Op())
	for _, r := range []struct {
		op  string
		not string
	}{
		{"add", patternOfNot(t, add.Schema().Properties["name"])},
		{"remove", patternOfNot(t, remove.Schema().Properties["name"])},
	} {
		idBytes, err := strconv.Atoi(authConst(t, "idBytes"))
		if err != nil {
			t.Fatal(err)
		}
		// The prefix and ID in any case, or any run of the secret's length.
		prefix := ""
		for _, c := range authConst(t, "tokenPrefix") {
			if unicode.IsLetter(c) {
				prefix += fmt.Sprintf("[%c%c]", unicode.ToUpper(c), unicode.ToLower(c))
			} else {
				prefix += regexp.QuoteMeta(string(c))
			}
		}
		want := fmt.Sprintf("^%s[0-9A-Fa-f]{%d}_|[A-Za-z0-9_-]{%s}", prefix, 2*idBytes, authConst(t, "encodedLen"))
		if r.not != want {
			t.Errorf("%s: name not-pattern %q, want auth's token format %q", r.op, r.not, want)
		}
		// One rule everywhere (user decision at confirm-before-merge,
		// 2026-11-17): the local create refuses with auth's own pattern and
		// message, which the spec repeats because cmd/nexus must not import
		// internal/auth.
		if r.not != auth.TokenStringPattern {
			t.Errorf("%s: name not-pattern %q, want auth.TokenStringPattern %q", r.op, r.not, auth.TokenStringPattern)
		}
	}
	for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
		if p := r.Schema().Properties["name"]; p == nil || p.Not == nil || p.Not.Description != auth.TokenStringRefusal {
			t.Errorf("%s: name refusal is not auth.TokenStringRefusal %q", op, auth.TokenStringRefusal)
		}
	}

	dir := t.TempDir()
	for i := range 32 {
		tok, _, err := auth.Create(dir, auth.CreateRequest{Name: fmt.Sprintf("t%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
			err := r.Validate(map[string]any{"name": tok})
			if err == nil {
				t.Errorf("%s accepts a token string as its name", op)
			} else if strings.Contains(err.Error(), tok) {
				t.Errorf("%s: the schema error quotes the token string: %v", op, err)
			}
		}
	}
	// Names, IDs and near misses that give nothing away still pass: a prefix
	// without the ID, an ID with no separator, a run one short of the secret,
	// and a long name broken by dots.
	for _, name := range []string{"ci", "abcdefabcdef", "nxs_ci", "nxs_0123456789ab", "nxs_0123456789ab-short",
		strings.Repeat("a", 42), strings.Repeat("a", 21) + "." + strings.Repeat("a", 21), strings.Repeat("a.", 32)} {
		for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
			if err := r.Validate(map[string]any{"name": name}); err != nil {
				t.Errorf("%s refuses the name %q: %v", op, name, err)
			}
		}
	}
}

// The variants of a token's secret that a name must not carry (SEC-1): each
// is refused by add and remove, and the refusal the client is told is the
// schema's own message, which never quotes the value. Each variant is derived
// from a token auth minted, and from fixed secrets that start with "-" and
// "_" so those cases always run: such a name also fails the name's own
// pattern, whose raw library error quotes the value. That error is not what
// reaches a client (core.validator.check sends the not schema's description,
// and cmd/nexus's notRefusal does the same; both are exercised end to end by
// TestTCRAE10 in internal/providers/nexusd and internal/cli), so it is not
// asserted on here.
func TestTokenNameRefusesEveryVariantOfASecret(t *testing.T) {
	add, remove := schemaOf(t, spec.TokensAdd.Op()), schemaOf(t, spec.TokensRemove.Op())
	tok, info, err := auth.Create(t.TempDir(), auth.CreateRequest{Name: "src"})
	if err != nil {
		t.Fatal(err)
	}
	id := info.ID
	secrets := map[string]string{
		"minted":                   strings.SplitN(tok, "_", 3)[2],
		"fixed dash-leading":       "-" + strings.Repeat("aB3_", 10) + "xy",
		"fixed underscore-leading": "_" + strings.Repeat("-Z9a", 10) + "xy",
	}
	// The refusal the validator picks for a name, as core.validator.check
	// does: the described not schema, when the name matches it.
	notOf := func(r *jsonschema.Resolved) *jsonschema.Resolved {
		ps := r.Schema().Properties["name"]
		if ps == nil || ps.Not == nil || ps.Not.Description == "" {
			t.Fatal("the name property has no described not schema")
		}
		nr, err := ps.Not.CloneSchemas().Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		return nr
	}
	nots := map[string]*jsonschema.Resolved{"add": notOf(add), "remove": notOf(remove)}
	for origin, secret := range secrets {
		if len(secret) != 43 {
			t.Fatalf("%s secret %q is not 43 characters", origin, secret)
		}
		for _, c := range []struct{ what, name string }{
			{"canonical", "nxs_" + id + "_" + secret},
			{"no prefix", id + "_" + secret},
			{"upper-case prefix", "NXS_" + id + "_" + secret},
			{"mixed-case prefix and ID", "Nxs_" + strings.ToUpper(id) + "_" + secret},
			{"bare secret", secret},
			{"secret inside a longer name", "ci-" + secret},
			{"secret then a suffix", secret + "-x"},
			{"prefix and ID with a short tail", "NXS_" + id + "_x"},
		} {
			for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
				who := fmt.Sprintf("%s (%s, %s)", op, origin, c.what)
				if err := r.Validate(map[string]any{"name": c.name}); err == nil {
					t.Errorf("%s accepts a name carrying the secret", who)
				}
				if nots[op].Validate(c.name) != nil {
					t.Errorf("%s: the name does not match the not schema, so the client would not be told the refusal", who)
				}
			}
		}
	}
	for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
		msg := r.Schema().Properties["name"].Not.Description
		for origin, secret := range secrets {
			if strings.Contains(msg, secret) {
				t.Errorf("%s: the refusal quotes the %s secret", op, origin)
			}
		}
	}
}

// patternOfNot is the pattern of the name property's not schema, or "".
func patternOfNot(t *testing.T, p *jsonschema.Schema) string {
	t.Helper()
	if p == nil {
		t.Fatal("no name property")
	}
	if p.Not == nil {
		return ""
	}
	return p.Not.Pattern
}
