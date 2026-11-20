// Tests of planning/storage-provider/test-cases.md TC-STG-207.
// Owner WIs: 5.1 (docs/cli/ and docs/api.md regenerated with the storage
// ops in the catalog).

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	storage "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// TC-STG-207: generated docs are current and cover the storage ops and problems. Owner WI 5.1. Requirements: NFR-9.
//
// TestGeneratedDocsAreCurrent proves docs/ equals what the generator writes
// into memory from the catalog; this checks what that covers for storage.
func TestTCSTG207_GeneratedDocsAreCurrentAndCoverThe(t *testing.T) {
	docs := repoDocs(t)
	files := mustGenerate(t, nil)
	if problems := check(docs, files); len(problems) > 0 {
		t.Fatalf("generated documentation is stale; run `make docs`:\n  %s", strings.Join(problems, "\n  "))
	}

	nouns := map[string]bool{}
	for _, op := range storage.Spec().Ops {
		nouns[op.CLI.Noun] = true
		page := "cli/nexus_" + op.CLI.Noun + "_" + op.CLI.Verb + ".md"
		if _, err := os.Stat(filepath.Join(docs, filepath.FromSlash(page))); err != nil {
			t.Errorf("docs/%s: %v", page, err)
		}
	}
	if len(nouns) != 8 {
		t.Errorf("the storage ops have %d CLI nouns, want 8", len(nouns))
	}
	for noun := range nouns {
		if _, err := os.Stat(filepath.Join(docs, "cli", "nexus_"+noun+".md")); err != nil {
			t.Errorf("docs/cli/nexus_%s.md: %v", noun, err)
		}
	}

	api, err := os.ReadFile(filepath.Join(docs, "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	ops := 0
	for _, op := range storage.Spec().Ops {
		if !strings.Contains(string(api), "| `"+op.ID+"` |") {
			t.Errorf("docs/api.md's operation table lacks %s", op.ID)
		}
		ops++
	}
	if ops != 12 {
		t.Errorf("%d storage ops, want 12", ops)
	}
	for _, slug := range []problem.Slug{problem.NotAllowed, problem.ScanInProgress, problem.PoolUnhealthy,
		problem.PoolLowSpace, problem.ZFSCommandFailed} {
		if !strings.Contains(string(api), "\n### Problem."+string(slug)+"\n") {
			t.Errorf("docs/api.md has no Problem.%s section", slug)
		}
	}
}
