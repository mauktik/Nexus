// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package def

import "testing"

// TC-SM-1 (WI 1.1; FR-13): Approved verb list gains reload, rollback and validate-config
func TestTCSM1_ApprovedVerbListGainsReloadRollback(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Read the exported approved-verb list (the one `verbs_test.go` TC-4 asserts).
	//   2. `def.Define` an op with `CLI.Verb` set to each of `reload`, `rollback` and
	//     `validate-config`.
	// - Expected:
	//   - The list is exactly `get start stop restart update install remove invoke test add cancel
	//     reload rollback validate-config`, in the order the file declares. There are no other
	//     additions.
	//   - All three Defines succeed.
	//   - A Define with verb `selfupdate` still panics with the existing unapproved-verb message.
	t.Skip("TC-SM-1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-2 (WI 1.1; FR-6, CON-3): A valid KindUpload declaration is accepted
func TestTCSM2_AValidKindUploadDeclarationIsAccepted(t *testing.T) {
	// - Setup: a test op `{ID: "tp.uploads.add", Provider: "tp", Kind: KindUpload, ResourceType:
	//   "uploads", Mode: ModeSync, Risk: RiskMutating, Scope: {Name: "tp:admin"}, Upload:
	//   &Upload{ContentTypes: ["application/octet-stream"], MaxBytes: 1<<20}}` with an empty CLI
	//   binding and `MCP: nil`.
	// - Steps:
	//   1. Call `def.Define[In, Out](op)` with an `In` that has one string field `signature`.
	// - Expected:
	//   - No panic.
	//   - The returned `Typed` carries `Upload` unchanged.
	//   - The op is the second accepted exception to "mutating ⇒ long-running": a `ModeSync` mutating
	//     `KindResourceAction` without `OperationControl` still panics.
	t.Skip("TC-SM-2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-3 (WI 1.1; FR-13, FR-14): CLI.Upload and ReconnectWindow declarations are accepted, including the 10 m boundary
func TestTCSM3_CLIUploadAndReconnectWindowDeclarationsAre(t *testing.T) {
	// - Setup: a long-running test op whose `In` has string `uploadId` and bool `allowDowngrade`.
	// - Steps:
	//   1. Define it with `CLI.Upload{Op: "tp.uploads.add", Field: "uploadId", FileFlag: "file",
	//     Sidecars: [{Param: "signature", Flag: "signature", Suffix: ".sig", MaxBytes: 4096}]}`.
	//   2. Define it with `ReconnectWindow: 60*time.Second`.
	//   3. Define it with `ReconnectWindow: 10*time.Minute`.
	//   4. Define it with a sidecar `MaxBytes: 65536`.
	// - Expected: all four Defines succeed, and the fields round-trip on the `Typed`'s `Op()`.
	t.Skip("TC-SM-3: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-4 (WI 1.1; CON-3): def imports nothing new beyond time
func TestTCSM4_DefImportsNothingNewBeyondTime(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Run `go list -deps -f '{{.ImportPath}}' ./internal/def` from the module root.
	//   2. Filter the output to non-stdlib packages and to stdlib packages not present at base
	//     `d04583a`.
	// - Expected:
	//   - The non-stdlib set is unchanged from base, computed by the test from the same command on
	//     `git show d04583a:internal/def/*.go` imports.
	//   - The only new stdlib import is `time`, or none if it was already present.
	t.Skip("TC-SM-4: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-ERR1 (WI 1.1; FR-6, CON-3): Invalid KindUpload declarations panic with a specific message
func TestTCSMERR1_InvalidKindUploadDeclarationsPanicWithA(t *testing.T) {
	// - Setup: the TC-SM-2 op mutated per row.
	// - Steps:
	//   1. Define each variant inside `recover`:
	//      - `Mode: ModeLongRunning`;
	//      - `Risk: RiskReadOnly`;
	//      - an empty `Scope.Name`;
	//      - `MCP` set;
	//      - `Upload: nil`;
	//      - empty `ContentTypes`;
	//      - `MaxBytes: 0`;
	//      - `MaxBytes: -1`;
	//      - `CLI.Noun: "x"`;
	//      - `Provider: "system"`.
	// - Expected:
	//   - Each variant panics.
	//   - The message contains the op ID and names the rule (for example `KindUpload requires
	//     ModeSync`).
	t.Skip("TC-SM-ERR1: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-ERR2 (WI 1.1; FR-6): Upload set on any other kind panics
func TestTCSMERR2_UploadSetOnAnyOtherKind(t *testing.T) {
	// - Setup: valid `KindList`, `KindGet`, `KindProviderAction` and `KindResourceAction` ops.
	// - Steps:
	//   1. Add `Upload: &Upload{...}` to each.
	//   2. Define each.
	// - Expected: each panics with a message naming `Upload` and the kind.
	t.Skip("TC-SM-ERR2: SKIP-only stub; un-skip when WI 1.1 lands")
}

// TC-SM-ERR3 (WI 1.1; FR-13, FR-14): Invalid CLI.Upload and ReconnectWindow declarations panic
func TestTCSMERR3_InvalidCLIUploadAndReconnectWindowDeclarations(t *testing.T) {
	// - Setup: the TC-SM-3 op mutated per row.
	// - Steps:
	//   1. Define each variant:
	//      - `CLI.Upload` on a `ModeSync` op;
	//      - an empty `Op`;
	//      - `Field: "nope"`;
	//      - `Field: "allowDowngrade"` (a bool);
	//      - `FileFlag: "File_Path"`;
	//      - a sidecar with an empty `Param`, `Flag` or `Suffix`;
	//      - a sidecar with `MaxBytes` 0 or 65537;
	//      - `ReconnectWindow` of -1s, 10m+1ns, or on a sync op.
	// - Expected: each panics, with a message naming the field.
	t.Skip("TC-SM-ERR3: SKIP-only stub; un-skip when WI 1.1 lands")
}
