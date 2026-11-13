// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package openapi_test

import "testing"

// TC-SM-23 (WI 2.1; FR-6): OpenAPI describes a KindUpload op as a POST with query parameters, a binary body and 201
func TestTCSM23_OpenAPIDescribesAKindUploadOpAs(t *testing.T) {
	// - Setup: a test-local spec with the TC-SM-2 upload op (`In{signature string}`, `Out{uploadId,
	//   sha256 string; sizeBytes int64; expiresAt time}`).
	// - Steps:
	//   1. `openapi.Route(op)`.
	//   2. Build the document and read `paths["/v1/providers/tp/uploads"].post`.
	// - Expected:
	//   - Route is `POST /v1/providers/tp/uploads`.
	//   - `parameters` has `signature` with `in: query`.
	//   - `requestBody.required` is true, with one content entry per type, each
	//     `{"type":"string","contentMediaType":"<type>"}`.
	//   - The description states the ceiling in bytes.
	//   - `responses` has `201` with the output schema, and the problem responses include
	//     `upload-too-large` (413), `conflict` (409) and `storage-limit-reached` (507).
	t.Skip("TC-SM-23: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-SM-24 (WI 2.1; FR-6, NFR-7): The document with an upload route validates, and route-versus-document coverage includes it
func TestTCSM24_TheDocumentWithAnUploadRoute(t *testing.T) {
	// - Setup: the TC-SM-23 spec registered with the real core and server (`TestDocumentMatchesRoutes`
	//   pattern).
	// - Steps:
	//   1. Run the libopenapi validator on the document (as `TestDocumentPassesIndependentValidator`
	//     does).
	//   2. Compare the server's recorded routes against the document.
	// - Expected:
	//   - Zero validation errors (A-38).
	//   - The upload route appears in both sets, and neither set has extras.
	t.Skip("TC-SM-24: SKIP-only stub; un-skip when WI 2.1 lands")
}

// TC-SM-25 (WI 2.1; FR-16): Slug-specific extension schemas
func TestTCSM25_SlugSpecificExtensionSchemas(t *testing.T) {
	// - Setup: the full catalog document (after 1.2).
	// - Steps:
	//   1. Read the problem schemas for `config-invalid`, `update-failed`, `rollback-failed`,
	//     `upload-too-large`, `version-downgrade` and docker's `dependency-unavailable`.
	// - Expected:
	//   - `config-invalid.errors` is an array of objects with `file`, `line`, `column`, `path` and
	//     `message`, where `message` is required.
	//   - `update-failed` has a boolean `rolledBack` and strings `step`, `previousVersion` and
	//     `newVersion`.
	//   - `rollback-failed` has `condition`, `fromVersion` and `toVersion`.
	//   - `upload-too-large.maxBytes` is an integer.
	//   - `version-downgrade` has `runningVersion` and `packageVersion`.
	//   - Docker's `rolledBack` description is byte-identical to base.
	t.Skip("TC-SM-25: SKIP-only stub; un-skip when WI 2.1 lands")
}
