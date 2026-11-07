package scripts_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts"
)

// TC-93 (second-state): integrity runs on every invocation, through the
// Dispatcher into Admit. A clean script passes it; after chmod o+w the next
// invocation is 422 script-integrity-failed and no operation is recorded.
//
// Execution is stubbed: until the runner (5.3) replaces it, Admit refuses
// every script that passes integrity with 503 capability-unavailable, "the
// script runner is not built in". That refusal is this test's "accepted":
// it is only reachable past the integrity check. 5.3 turns it into 202.
func TestIntegrityRunsOnEveryInvocation(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	base := scripts.CleanBase(t)
	path := filepath.Join(base, "deploy.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chmod := func(mode os.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	chmod(0o755)
	n := start(t, []provider.Provider{scripts.New(scripts.Options{Host: "testhost", Preflight: enabled})},
		fmt.Sprintf("      - id: deploy\n        description: Deploy\n        path: %s\n        runAs: %s\n"+
			"        workingDirectory: %s\n", path, u.Username, base))

	operations := func() int {
		t.Helper()
		r := n.do(t, http.MethodGet, "/v1/operations", n.tokens["full"], "")
		var ops apitypes.List[apitypes.Operation]
		if err := json.Unmarshal(r.body, &ops); err != nil {
			t.Fatalf("operations: %d %s", r.status, r.body)
		}
		return len(ops.Items)
	}
	invoke := func() response {
		t.Helper()
		return n.do(t, http.MethodPost, "/v1/providers/scripts/scripts/deploy/invoke", n.tokens["run"], "{}")
	}
	passes := func(step string) {
		t.Helper()
		r := invoke()
		p := r.problem(t)
		if r.status != http.StatusServiceUnavailable || p.Slug() != problem.CapabilityUnavailable ||
			p.Extensions["reason"] != "the script runner is not built in" {
			t.Fatalf("%s: %d %s, want the integrity check passed (the 5.3 placeholder refusal)", step, r.status, r.body)
		}
	}

	passes("clean script")
	before := operations()

	chmod(0o757)
	r := invoke()
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("after chmod o+w: %d %s, want 422", r.status, r.body)
	}
	p := r.problem(t)
	if p.Slug() != problem.ScriptIntegrityFailed || p.Extensions["check"] != scripts.CheckScriptWritable {
		t.Fatalf("after chmod o+w: %s", r.body)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Extensions["path"] != resolved {
		t.Errorf("path %v, want %s", p.Extensions["path"], resolved)
	}
	if after := operations(); after != before {
		t.Errorf("operations %d after the refusal, want %d", after, before)
	}

	// Each invocation checks afresh, so the fix takes effect at once.
	chmod(0o755)
	passes("after chmod o-w")
}
