// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package buildsys

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TC-SM-102 (WI 6.2; FR-19): deploy/install.sh is byte-identical to the base
// revision. The update helper runs the package's install.sh as its install
// step and relies on its restart contract (A-33), so the script must not
// drift. If it is ever changed, this case is replaced by TC-SM-104's
// backward-compatibility proof.
func TestTCSM102_DeployInstallShIsByteIdentical(t *testing.T) {
	const base = "d04583a"
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for TC-SM-102: %v", err)
	}
	root := repoRoot(t)
	cmd := exec.Command("git", "-C", root, "show", base+":deploy/install.sh")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show %s:deploy/install.sh: %v %s", base, err, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("deploy/install.sh differs from %s (%d bytes, base %d bytes): FR-19 keeps it unchanged; "+
			"a change needs TC-SM-104's backward-compatibility proof in place of this case", base, len(got), len(want))
	}
	t.Logf("deploy/install.sh equals %s:deploy/install.sh (%d bytes)", base, len(got))
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
