// Tests for planning/system-provider/test-cases.md TC-HP-105 (WI 5.1): the
// conformance suite covers the host provider through catalogStubs.

package conformance_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	host "github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-105: The conformance suite covers the host provider. Owner WI 5.1. Requirements: NFR-7, FR-2, FR-14, NFR-6.
//
// The whole suite (TestScopeAndAuditCoverEveryOp,
// TestEveryStringInputIsConstrained, TestNoArbitraryExecutionSurface) walks
// the host ops once catalogStubs has them; this is the host filter of each.
// The test case's "insufficient-scope" is the registered forbidden problem
// with requiredScope: no other slug exists for a missing scope.
func TestTCHP105_TheConformanceSuiteCoversTheHostProvider(t *testing.T) {
	s := newStack(t)
	var served []def.Op
	for _, op := range s.ops() {
		if op.Provider == host.Name {
			served = append(served, op)
		}
	}
	if len(served) != len(host.Spec().Ops) || len(served) != 10 {
		t.Fatalf("the stack serves %d host ops, want the spec's 10", len(served))
	}

	// Scope: a token without host:read is refused naming it, and one
	// holding exactly host:read is answered 200.
	reader := s.exact[host.ReadScope]
	if reader == "" {
		t.Fatalf("no token holds exactly %s", host.ReadScope)
	}
	for _, op := range served {
		r := s.call(t, op, s.none, nil)
		if r.status != http.StatusForbidden {
			t.Errorf("%s without %s: %d %.200s, want 403", op.ID, host.ReadScope, r.status, r.body)
		} else if p := r.problem(t); p.Slug() != problem.Forbidden || p.Extensions["requiredScope"] != host.ReadScope {
			t.Errorf("%s: problem %s %v, want forbidden naming %s", op.ID, p.Slug(), p.Extensions, host.ReadScope)
		}
		if r := s.call(t, op, reader, nil); r.status != http.StatusOK {
			t.Errorf("%s with %s: %d %.200s, want 200", op.ID, host.ReadScope, r.status, r.body)
		}
	}

	// Strings: every string input is constrained and bounded, and the walk
	// found the two the host ops have.
	var strs []string
	for _, op := range served {
		for _, f := range unconstrained(t, op) {
			t.Errorf("unconstrained input string: %s", f)
		}
		visitSchema(inputSchema(t, op), "", func(path string, sc *jsonschema.Schema) {
			if !hasType(sc, "string") {
				return
			}
			strs = append(strs, op.ID+" "+path)
			if sc.Pattern == "" && len(sc.Enum) == 0 {
				t.Errorf("%s %s: no pattern or enum", op.ID, path)
			}
			if sc.MaxLength == nil {
				t.Errorf("%s %s: no maxLength", op.ID, path)
			}
		})
	}
	for _, want := range []string{"host.filesystems.list /cursor", "host.disks.list /cursor",
		"host.interfaces.list /cursor", "host.metrics.get /metric/*"} {
		if !slices.Contains(strs, want) {
			t.Errorf("the walk did not visit %s: %v", want, strs)
		}
	}

	// No execution surface, and no input field names a path, a device or a
	// command.
	for _, op := range served {
		for _, f := range executionSurface(t, op) {
			t.Errorf("execution surface: %s", f)
		}
		visitSchema(inputSchema(t, op), "", func(path string, sc *jsonschema.Schema) {
			for _, name := range sortedKeys(sc.Properties) {
				n := normalize(name)
				for _, bad := range []string{"path", "device", "dev", "command", "cmd", "file"} {
					if strings.Contains(n, bad) {
						t.Errorf("%s: input field %s/%s", op.ID, path, name)
					}
				}
			}
		})
	}
}
