package auth

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretish matches identifiers that hold a secret, a hash or a digest.
var secretish = regexp.MustCompile(`(?i)(hash|sum|digest|secret)`)

// typeCheck type-checks files as one package as far as it can: standard
// library imports resolve from source, and anything unresolved is ignored, so
// byte-array and byte-slice operands are known whatever their names.
func typeCheck(t *testing.T, fset *token.FileSet, files []*ast.File) *types.Info {
	t.Helper()
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil), Error: func(error) {}}
	_, _ = conf.Check("p", fset, files, info) // Errors are tolerated by design
	if len(info.Types) == 0 {
		t.Fatal("type checking recorded no types")
	}
	return info
}

// isBytes reports whether e is a byte array or a byte slice, such as a
// [32]byte SHA-256 sum.
func isBytes(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Type == nil {
		return false
	}
	var elem types.Type
	switch u := tv.Type.Underlying().(type) {
	case *types.Array:
		elem = u.Elem()
	case *types.Slice:
		elem = u.Elem()
	default:
		return false
	}
	b, ok := elem.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Uint8
}

// insecureCompares reports every place in f that compares secret material
// other than through subtle.ConstantTimeCompare: == or != on a byte array or
// byte slice, or with a secret-named operand, and any bytes.Equal,
// bytes.Compare, slices.Equal or reflect.DeepEqual call.
func insecureCompares(fset *token.FileSet, info *types.Info, f *ast.File) []string {
	var found []string
	named := func(e ast.Expr) bool {
		if isBytes(info, e) {
			return true
		}
		for {
			switch x := e.(type) {
			case *ast.ParenExpr:
				e = x.X
			case *ast.IndexExpr:
				e = x.X
			case *ast.SliceExpr:
				e = x.X
			case *ast.StarExpr:
				e = x.X
			case *ast.SelectorExpr:
				return secretish.MatchString(x.Sel.Name) || secretish.MatchString(exprName(x.X))
			case *ast.Ident:
				return secretish.MatchString(x.Name)
			default:
				return false
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BinaryExpr:
			if (x.Op == token.EQL || x.Op == token.NEQ) && (named(x.X) || named(x.Y)) {
				found = append(found, fset.Position(x.Pos()).String()+": "+x.Op.String())
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				switch exprName(sel.X) + "." + sel.Sel.Name {
				case "bytes.Equal", "bytes.Compare", "reflect.DeepEqual", "slices.Equal":
					found = append(found, fset.Position(x.Pos()).String()+": "+exprName(sel.X)+"."+sel.Sel.Name)
				}
			}
		}
		return true
	})
	return found
}

func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func usesConstantTimeCompare(f *ast.File) bool {
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && exprName(sel.X) == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			used = true
		}
		return true
	})
	return used
}

// TC-ERR8 (AST half): secret comparisons go through
// subtle.ConstantTimeCompare and no == is applied to hash bytes.
func TestSecretComparisonsAreConstantTime(t *testing.T) {
	// Control: the checker flags the comparisons it exists to forbid.
	planted := `package p
func f(rec struct{ hash [32]byte }, sum [32]byte, secret []byte, want []byte) bool {
	if sum == rec.hash { return true }
	var got, want [32]byte
	if got == want { return true }
	if string(secret) != "x" { return false }
	if rec.hash[0] == 1 { return true }
	return bytes.Equal(sum[:], want)
}`
	fset := token.NewFileSet()
	pf, err := parser.ParseFile(fset, "planted.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := insecureCompares(fset, typeCheck(t, fset, []*ast.File{pf}), pf); len(got) != 4 {
		// sum == rec.hash, rec.hash[0] == 1, got == want (by type, whatever
		// the names) and bytes.Equal. string(secret) is a conversion, so a
		// string comparison of it is not flagged.
		t.Fatalf("control: checker flagged %v, want 4 findings", got)
	}
	if usesConstantTimeCompare(pf) {
		t.Fatal("control: planted file reported as using ConstantTimeCompare")
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
	}
	info := typeCheck(t, fset, parsed)
	ctc := false
	checked := 0
	for _, f := range parsed {
		checked++
		if bad := insecureCompares(fset, info, f); len(bad) > 0 {
			t.Errorf("non-constant-time comparison of secret material: %v", bad)
		}
		if usesConstantTimeCompare(f) {
			ctc = true
		}
	}
	if checked < 4 {
		t.Fatalf("checked only %d source files", checked)
	}
	if !ctc {
		t.Fatal("no source file in internal/auth calls subtle.ConstantTimeCompare")
	}
}
