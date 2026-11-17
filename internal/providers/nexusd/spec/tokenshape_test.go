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
		want := fmt.Sprintf("^%s[0-9a-f]{%d}_[A-Za-z0-9_-]{%s}$",
			regexp.QuoteMeta(authConst(t, "tokenPrefix")), 2*idBytes, authConst(t, "encodedLen"))
		if r.not != want {
			t.Errorf("%s: name not-pattern %q, want auth's token format %q", r.op, r.not, want)
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
	// Names, IDs and near misses still pass.
	for _, name := range []string{"ci", "abcdefabcdef", "nxs_ci", "nxs_0123456789ab_short",
		"nxs_0123456789AB_" + strings.Repeat("A", 43), strings.Repeat("a", 64)} {
		for op, r := range map[string]*jsonschema.Resolved{"add": add, "remove": remove} {
			if err := r.Validate(map[string]any{"name": name}); err != nil {
				t.Errorf("%s refuses the name %q: %v", op, name, err)
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
