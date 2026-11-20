// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package capturegate is the storage provider's capture gate (NFR-8a, A-41):
// a capture-driven test asks Require for the operator capture it reads, and
// skips while that capture is absent, or fails when
// NEXUS_STORAGE_REQUIRE_CAPTURES=1 (the Phase 4 entry check, TC-STG-L0).
//
// It imports only the standard library, so every storage test package (the
// internal zfs, vzdump and timemachine tests, storagetest and storage) can
// import it without an import cycle. It is the only reader of EnvRequire.
package capturegate

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// EnvRequire names the variable that turns a pending capture from a skip
// into a failure. Only the value "1" does; any other value counts as unset.
const EnvRequire = "NEXUS_STORAGE_REQUIRE_CAPTURES"

// Require returns path when os.Stat(path) succeeds (a capture file or
// directory). When path does not exist it calls
// t.Skipf("operator capture pending (WI 1.2): %s", path), or t.Fatalf with the
// same message when os.Getenv(EnvRequire) == "1". Any other Stat error is
// t.Fatalf with the error.
func Require(t testing.TB, path string) string {
	t.Helper()
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return path
	case errors.Is(err, fs.ErrNotExist):
		if os.Getenv(EnvRequire) == "1" {
			t.Fatalf("operator capture pending (WI 1.2): %s", path)
		} else {
			t.Skipf("operator capture pending (WI 1.2): %s", path)
		}
	default:
		t.Fatalf("capture %s: %v", path, err)
	}
	return ""
}
