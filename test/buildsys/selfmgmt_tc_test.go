// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package buildsys

import "testing"

// TC-SM-17 (WI 1.4; FR-15): make -n release runs the key check before check and the build
func TestTCSM17_MakeNReleaseRunsTheKey(t *testing.T) {
	// - Setup: throwaway copy of the repo (existing buildsys pattern); `skipUnderMake`.
	// - Steps:
	//   1. `make -n release VERSION=v0.0.0-test NEXUS_RELEASE_KEY=/nonexistent`.
	// - Expected:
	//   - In the dry-run output, the first command that references the key (`nexus-release check-key`)
	//     appears before `go vet`, `go test` and the first `go build`.
	//   - The output contains `nexus-release sign --key '/nonexistent'`, and `SHA256SUMS` lists
	//     `*.tar.gz *.sig`.
	t.Skip("TC-SM-17: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SM-18 (WI 1.4; FR-15): make release with a temp key signs only the nexusd tarball and lists the .sig in SHA256SUMS
func TestTCSM18_MakeReleaseWithATempKey(t *testing.T) {
	// - Setup: throwaway repo copy; a temp key from `nexus-release keygen`; `skipUnderMake`. The
	//   `check` step is overridden as `TC-178` does, so the run does not recurse into tests.
	// - Steps:
	//   1. `make release VERSION=v0.0.0-test NEXUS_RELEASE_KEY=$key`.
	//   2. List `dist/release`.
	//   3. Run `sha256sum -c SHA256SUMS` (`shasum -a 256 -c` on macOS).
	//   4. `release.Verify` the `.sig` against the temp public key.
	// - Expected:
	//   - Exactly one `.sig` exists: `nexusd-v0.0.0-test-linux-amd64.tar.gz.sig` (A-37). The client
	//     tarballs have none.
	//   - `SHA256SUMS` contains a line for the `.sig`, and the check passes.
	//   - Verification is true.
	t.Skip("TC-SM-18: SKIP-only stub; un-skip when WI 1.4 lands")
}

// TC-SM-100 (WI 6.1; FR-18, NFR-7, FR-13): Generated docs are current, and the update page documents its flags
func TestTCSM100_GeneratedDocsAreCurrentAndThe(t *testing.T) {
	// - Setup: repo copy.
	// - Steps:
	//   1. `go run ./cmd/nexus-gendocs -dir docs -check`.
	//   2. Read `docs/cli/nexus_nexusd_update.md` and `docs/api.md`.
	// - Expected:
	//   - Step 1 exits 0 with no stale file.
	//   - The update page contains `--file`, `--signature` and `--allow-downgrade`.
	//   - `docs/api.md` lists the upload route and the 13 new FR-16 problem types (including
	//     `upload-too-large`).
	t.Skip("TC-SM-100: SKIP-only stub; un-skip when WI 6.1 lands")
}

// TC-SM-102 (WI 6.2; FR-19): deploy/install.sh is byte-identical to the base revision
func TestTCSM102_DeployInstallShIsByteIdentical(t *testing.T) {
	// - Setup: git available.
	// - Steps:
	//   1. `git show d04583a:deploy/install.sh`.
	//   2. Compare it with the working-tree file.
	// - Expected: the two are identical. If a change is ever made, this test is replaced by
	//   TC-SM-104's backward-compatibility proof.
	t.Skip("TC-SM-102: SKIP-only stub; un-skip when WI 6.2 lands")
}

// TC-SM-105 (WI 6.3; FR-18, CON-5, NFR-5, FR-15): The documentation covers the scopes, the signing key, recovery and the verbs
func TestTCSM105_TheDocumentationCoversTheScopesThe(t *testing.T) {
	// - Setup: none.
	// - Steps:
	//   1. Read the files.
	//   2. Assert the required contents.
	// - Expected:
	//   - `docs/security.md` contains `nexusd:read`, `nexusd:admin` and "root-equivalent", a
	//     release-signing-key section mentioning `~/.config/nexus/release-signing.key`, `0600`,
	//     "SHA256SUMS" with "integrity", rotation, and what a stolen key allows.
	//   - `docs/install.md`:
	//     - contains `nexus nexusd update --file` and `make release-key`;
	//     - no longer contains "Nexus does not update itself";
	//     - has the heading anchor `recovering-from-a-failed-update`, which is the exact anchor the
	//       helper's `update-rollback-failed` detail references, with the 6 manual commands;
	//     - states the first install is manual.
	//   - `docs/mcp.md` lists `get_nexusd` and `validate_nexusd_config`, and its budget figure equals
	//     TC-SM-96's measured value.
	//   - `docs/adding-a-provider.md` lists the verbs `reload`, `rollback` and `validate-config` and
	//     the four generic mechanisms.
	//   - NSM requirements: OOS-11, FR-22, FR-29 and NFR-4 are amended.
	t.Skip("TC-SM-105: SKIP-only stub; un-skip when WI 6.3 lands")
}

// TC-SM-106 (WI 7.1; CON-1): The harness refuses a non-local Docker context
func TestTCSM106_TheHarnessRefusesANonLocal(t *testing.T) {
	// - Setup: a stub `docker` script first on `PATH`. It answers `context inspect` with endpoint
	//   `tcp://192.168.7.5:2376` (run 1) or `unix:///…/colima/default/docker.sock` (run 2), answers
	//   `info` with `aarch64`, and logs every invocation to a file. No real Docker is used.
	// - Steps:
	//   1. Run 1: `bash test/harness/run.sh`.
	//   2. Run 1 again with `NEXUS_HARNESS_ALLOW_REMOTE=1`.
	//   3. Run 2: `bash test/harness/run.sh` (stop the stub at the first `docker build` by exiting
	//     99).
	// - Expected:
	//   - Step 1: non-zero exit, stderr names the endpoint, and the log has no `run`, `build` or `cp`
	//     invocation.
	//   - Step 2: proceeds past the guard (reaches `docker build`).
	//   - Step 3: proceeds past the guard.
	t.Skip("TC-SM-106: SKIP-only stub; un-skip when WI 7.1 lands")
}

// TC-SM-108 (WI 7.1; FR-7): The real nexusd tarball's entry count exceeds 64 and stays within the 1024 limit
func TestTCSM108_TheRealNexusdTreeSEntry(t *testing.T) {
	// - Setup: repo copy; `skipUnderMake`; a host-arch nexusd built with `-X …Version=v9.9.2`.
	// - Steps:
	//   1. `make nexusd-tree TREE_BIN=… TREE_DIR=$tmp/nexusd-v9.9.2-linux-<goarch>`.
	//   2. `make nexusd-tarball TREE_DIR=… TARBALL=$tmp/t.tar.gz`.
	//   3. Read every header of the tarball with `compress/gzip` and `archive/tar`.
	// - Expected:
	//   - The entry count is greater than 64 (66 or more after this feature's docs) and at most 1024.
	//     The 1024 is hard-coded in the test with a comment citing A-43; the test does not import the
	//     unexported scan constant.
	//   - The tarball was packed with `TAR_FLAGS`: every header has uid and gid 0 and no xattr or PAX
	//     `SCHILY.xattr.*` records (the `readTarball` checks).
	t.Skip("TC-SM-108: SKIP-only stub; un-skip when WI 7.1 lands")
}

// TC-SM-ERR7 (WI 1.4; FR-15): make release with a missing or 0644 key fails before any tarball
func TestTCSMERR7_MakeReleaseWithAMissingOr(t *testing.T) {
	// - Setup: repo copy; `skipUnderMake`. Run (a) with `NEXUS_RELEASE_KEY=/nonexistent` and (b) with
	//   a valid key chmod 0644.
	// - Steps:
	//   1. `make release VERSION=v0.0.0-test`.
	// - Expected:
	//   - Non-zero exit in both.
	//   - stderr names the reason.
	//   - `dist/release/` has no `*.tar.gz` (absent or empty), and `build-release` did not run (no
	//     `dist/nexusd-linux-amd64`).
	t.Skip("TC-SM-ERR7: SKIP-only stub; un-skip when WI 1.4 lands")
}
