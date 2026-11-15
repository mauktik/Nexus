// Stub-style: SKIP-only (system-provider)
//
// Stubs of planning/system-provider/test-cases.md TC-HP-109.
// Owner WIs: 5.1. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test. An un-skipped passing test is the GREEN signal
// (planning/system-provider/test-strategy.md, RED Stub Style).

package cli

import "testing"

// TC-HP-109: CLI surface against a server running the real provider. Owner WI 5.1. Requirements: FR-14.
//
// - Setup: `startProviderServer(t, "h1", hostprov.New(Options{FS: F-FS(UBU), GOOS: "linux", …}), true, "system:read")` and a second host `h2` the same; `writeClientConfig`.
// - Steps:
//  1. `nexus hostsummary get -o json`;
//  2. `nexus hostfs get` (table);
//  3. `nexus hostnet get --host all -o json`;
//  4. `nexus hostsummary get --help`;
//  5. `nexus host get` (the configured-hosts command).
//
// - Expected: 1 prints a one-element list whose item is named `current`, exit 0; 2 prints the five column headers of the Columns table with one row per filesystem; 3 has items from both hosts; 4 lists no `--stdin` flag; 5 still prints the client's configured hosts (unchanged command).
func TestTCHP109_CLISurfaceAgainstAServerRunningThe(t *testing.T) {
	t.Skip("TC-HP-109: SKIP-only stub; un-skip when WI 5.1 lands")
}
