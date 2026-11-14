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

// backendIndependent are the tests of the suites that run once, with no
// backend subtests: they never start the backend's supervisor, so a
// /process subtest of them would run the code of /local again and read as
// parity evidence it is not. Each is either in-process (no detached
// operation) or drives the detach seams through injected fakes, because the
// seam's answer is the thing under test. A test not here is perBackend, and
// its body must use the backend it is given.
var backendIndependent = map[string]string{
	"TestCancelAQueuedOperation":                               "a queued in-process operation; no detach",
	"TestRepeatCancelReturnsTheSameResultAndIgnoresKeys":       "in-process operations; no detach",
	"TestCancelRefusalsAndAuthorization":                       "in-process operations; no detach",
	"TestLaunchFailureFailsTheOperation":                       "the launch error is injected: the ops handling of it is the subject",
	"TestCancelCannotLoseARaceWithADequeuedOperation":          "a race in the ops layer, driven through a launch spy; the readiness race of a real launch is TestCancelRightAfterStartEndsCanceled in internal/detach",
	"TestLongRunningAcknowledgementAndTerminalRecord":          "in-process operations; no detach",
	"TestStateChangesAuditThenCommit":                          "in-process operations; no detach",
	"TestOperationFailureAndPanicContainment":                  "in-process operations; no detach",
	"TestOperationStatusSetHasNoApprovalState":                 "in-process operations; no detach",
	"TestAuditRecordIsDurableBeforeExecution":                  "in-process operations; no detach",
	"TestSensitiveValuesAndTokensNeverEnterAudit":              "in-process operations; no detach",
	"TestAPIResponsesContainNoInternalFields":                  "in-process operations; no detach",
	"TestReadRiskRequests":                                     "in-process operations; no detach",
	"TestStorageLimitRefusesNewMutationsButNotReplays":         "in-process operations; no detach",
	"TestIdempotencyKeyReturnsTheOriginalOperation":            "in-process operations; no detach",
	"TestReplaysAreAuditedReplayed":                            "in-process operations; no detach",
	"TestConcurrentSameKeyOnDifferentLanesCreatesOneOperation": "100 detached operations through a launch spy: the ops arbitration is the subject, and no real supervisor should start",
	"TestIdempotencyBeforeAdmissionAndValidation":              "in-process operations; no detach",
	"TestKeyReuseWithADifferentRequest":                        "in-process operations; no detach",
	"TestLaneFreeWhenRecordReadsTerminal":                      "in-process and spied operations: the lane bookkeeping is the subject",
	"TestLaneReapCrashSafety":                                  "in-process operations; no detach",
	"TestWatcherLivenessPollingWhileTheDaemonIsUp":             "synctest with a spy: the polling schedule on a fake clock is the subject",
	"TestRecoveryBlockedClassificationHitsDeadline":            "an injected UnitState that never answers: the ops deadline is the subject",
}

// nginx-provider TC-25 (as corrected by CR-A1): the internal/ops behavioral
// suites pass on every backend. A test that depends on the detach backend is
// only the backend loop, perBackend(t, test<Name>), so `go test -v` shows a
// /local subtest of it, and on Linux a /process subtest too, and its body
// takes the backend by name and uses it: a body that ignores its backend
// would make /process a duplicate of /local. A test that does not depend on
// the backend is on backendIndependent, with the reason, and runs once. Off
// Linux, Backends logs why there is no process backend.
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
	seen := map[string]bool{}
	for _, file := range suiteFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		funcs := map[string]*ast.FuncDecl{}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			tests++
			seen[fn.Name.Name] = true
			pos := fset.Position(fn.Pos())
			bodyName := "test" + strings.TrimPrefix(fn.Name.Name, "Test")
			_, independent := backendIndependent[fn.Name.Name]
			switch {
			case independent && runsPerBackend(fn, bodyName):
				t.Errorf("%s: %s is on backendIndependent but runs perBackend", pos, fn.Name.Name)
			case independent:
				if mentionsBackend(fn) {
					t.Errorf("%s: %s is backend-independent but mentions a backend", pos, fn.Name.Name)
				}
			case !runsPerBackend(fn, bodyName):
				t.Errorf("%s: %s is neither perBackend(t, %s) nor on backendIndependent", pos, fn.Name.Name, bodyName)
			default:
				body := funcs[bodyName]
				if body == nil {
					t.Errorf("%s: %s is not declared in %s", pos, bodyName, file)
				} else if name := backendParam(body); name == "" {
					t.Errorf("%s: %s takes its backend as _ or not at all: its /process subtest would repeat /local", fset.Position(body.Pos()), bodyName)
				} else if !usesIdent(body, name) {
					t.Errorf("%s: %s never uses its backend %s: its /process subtest would repeat /local", fset.Position(body.Pos()), bodyName, name)
				}
			}
		}
	}
	if tests == 0 {
		t.Fatal("no tests found in the behavioral suites")
	}
	for name := range backendIndependent {
		if !seen[name] {
			t.Errorf("backendIndependent names %s, which is not a test of the suites", name)
		}
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

// backendParam is the name of body's second parameter, the backend it runs
// on; empty when it has none or ignores it as _.
func backendParam(body *ast.FuncDecl) string {
	var names []*ast.Ident
	for _, field := range body.Type.Params.List {
		names = append(names, field.Names...)
	}
	if len(names) != 2 || names[1].Name == "_" {
		return ""
	}
	return names[1].Name
}

// usesIdent reports whether fn's body refers to name.
func usesIdent(fn *ast.FuncDecl, name string) bool {
	used := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			used = true
		}
		return !used
	})
	return used
}

// mentionsBackend reports whether fn refers to the backend machinery, which
// a backend-independent test has no business with.
func mentionsBackend(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "backendWorld" || id.Name == "withBackend" || id.Name == "perBackend") {
			found = true
		}
		return !found
	})
	return found
}
