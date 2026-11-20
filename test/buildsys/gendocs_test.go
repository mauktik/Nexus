// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package buildsys

import (
	"path/filepath"
	"strings"
	"testing"
)

// TC-SM-100 (WI 6.1; FR-18, NFR-7, FR-13): the generated documentation of a
// repo copy is current, the update page documents its upload flags, and
// docs/api.md lists the upload route and the 13 FR-16 problem types.
func TestTCSM100_GeneratedDocsAreCurrentAndThe(t *testing.T) {
	dir := copyRepo(t)

	// Step 1.
	out, code := run(t, dir, nil, "go", "run", "./cmd/nexus-gendocs", "-dir", "docs", "-check")
	if code != 0 {
		t.Fatalf("nexus-gendocs -check exited %d: %s", code, out)
	}

	// Step 2.
	update := readFile(t, filepath.Join(dir, "docs", "cli", "nexus_nexusd_update.md"))
	for _, flag := range []string{"--file", "--signature", "--allow-downgrade"} {
		if !strings.Contains(update, flag) {
			t.Errorf("docs/cli/nexus_nexusd_update.md lacks %s", flag)
		}
	}
	api := readFile(t, filepath.Join(dir, "docs", "api.md"))
	if !strings.Contains(api, "`POST /v1/providers/nexusd/uploads`") {
		t.Error("docs/api.md lacks the route POST /v1/providers/nexusd/uploads")
	}
	// The FR-16 problem types: eight answered by a request and five that
	// appear only inside operation.error.
	for _, slug := range []string{
		"config-invalid", "upload-too-large", "upload-not-found", "update-not-configured",
		"update-signature-invalid", "update-package-invalid", "version-downgrade", "rollback-unavailable",
		"config-apply-failed", "restart-unhealthy", "update-failed", "update-rollback-failed", "rollback-failed",
	} {
		if !strings.Contains(api, "### Problem."+slug+"\n") || !strings.Contains(api, `"urn:nexus:problem:`+slug+`"`) {
			t.Errorf("docs/api.md does not document the problem type %s", slug)
		}
	}
}
