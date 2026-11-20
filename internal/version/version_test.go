// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

// TC-1 (unit half): without -ldflags -X the binaries report a non-empty
// development version.
func TestDefaultVersionIsNonEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty without an ldflag; want a non-empty development version")
	}
}
