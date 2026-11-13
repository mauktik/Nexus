package ops_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/detach/detachtest"
)

// suiteFiles are the behavioral suites that run on every detach backend
// (NFR-8): restart recovery, cancel, liveness, lane release, idempotency,
// the detached lifecycle and the operation lifecycle.
var suiteFiles = []string{
	"recovery_test.go", "cancel_test.go", "liveness_test.go", "lane_release_test.go",
	"idempotency_test.go", "detached_test.go", "lifecycle_test.go",
}

// nginx-provider TC-25: the internal/ops behavioral suites pass unchanged on
// every backend. Every top-level test of the suites is only the backend
// loop, perBackend(t, test<Name>), so `go test -v` shows a /local subtest
// of each, and on Linux a /process subtest too; the body is the test as it
// was. Off Linux, Backends logs why there is no process backend.
func TestBehavioralSuitesRunPerBackend(t *testing.T) {
	var names []string
	for _, b := range detachtest.Backends(t) {
		names = append(names, b.Name)
	}
	want := []string{"local"}
	if runtime.GOOS == "linux" {
		want = append(want, "process")
	}
	if !slices.Equal(names, want) {
		t.Fatalf("backends %q on %s, want %q", names, runtime.GOOS, want)
	}

	fset := token.NewFileSet()
	tests := 0
	for _, file := range suiteFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			tests++
			if body := strings.TrimPrefix(fn.Name.Name, "Test"); !runsPerBackend(fn, "test"+body) {
				t.Errorf("%s: %s is not perBackend(t, test%s)", fset.Position(fn.Pos()), fn.Name.Name, body)
			}
		}
	}
	if tests == 0 {
		t.Fatal("no tests found in the behavioral suites")
	}
}

// runsPerBackend reports whether fn's body is the one statement
// perBackend(t, body).
func runsPerBackend(fn *ast.FuncDecl, body string) bool {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	isIdent := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	return isIdent(call.Fun, "perBackend") && isIdent(call.Args[0], "t") && isIdent(call.Args[1], body)
}
