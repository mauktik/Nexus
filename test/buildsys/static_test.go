// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package buildsys

import "testing"

// nginx-provider TC-5: staticcheck rejects a dynamically linked ELF and accepts a static one
// Owner WI 1.3. Requirements: FR-38.
//
//	Setup:
//	  - A static binary: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build
//	    ./cmd/nexusd.
//	  - A dynamic fixture: test/buildsys/testdata/dynamic.elf, a committed tiny
//	    dynamic ELF with PT_INTERP and DT_NEEDED libc.so.6. Its provenance (build
//	    command) is recorded in testdata/README.md.
//	Steps:
//	  1. go run ./test/buildsys/staticcheck <static>.
//	  2. go run ./test/buildsys/staticcheck <dynamic.elf>.
//	  3. go run ./test/buildsys/staticcheck <static> <dynamic.elf>.
//	Expected:
//	  - Step 1 exits 0.
//	  - Step 2 exits non-zero, and stderr names the file and both PT_INTERP and
//	    DT_NEEDED.
//	  - Step 3 exits non-zero (any failure fails).
func TestStaticcheckRejectsDynamicELF(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-5 (WI 1.3)")
}

// nginx-provider TC-6: make build-release fails on a dynamically linked release binary
// Owner WI 1.3. Requirements: FR-38.
//
//	Setup: skipUnderMake. Use copyRepo(t).
//	Steps:
//	  1. make build-release in the copy.
//	  2. In a second copy, edit RELEASE_BUILD to CGO_ENABLED=1 go build
//	    -ldflags=-linkmode=external. Skip this step with a reason when no C
//	    toolchain is present.
//	  3. Run make build-release there.
//	Expected:
//	  - Step 1 exits 0, and debug/elf finds no PT_INTERP in dist/nexusd-linux-
//	    amd64 or dist/nexus-linux-amd64.
//	  - Step 3 exits non-zero with the staticcheck message.
//	  - The Makefile text has a comment above RELEASE_BUILD naming the pure-Go
//	    os/user and net paths (L-15).
func TestBuildReleaseRunsStaticcheck(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-6 (WI 1.3)")
}
