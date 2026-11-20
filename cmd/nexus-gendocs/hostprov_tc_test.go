// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests for planning/system-provider/test-cases.md TC-HP-111 (WI 5.1): the
// generated reference pages of the host provider are checked in and
// current.

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	host "github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-111: Generated CLI and API docs are current. Owner WI 5.1. Requirements: NFR-7, FR-14.
//
// TestGeneratedDocsAreCurrent checks every generated file is current; this
// checks the host pages are among them.
func TestTCHP111_GeneratedCLIAndAPIDocsAreCurrent(t *testing.T) {
	docs := repoDocs(t)
	if problems := check(docs, mustGenerate(t, nil)); len(problems) > 0 {
		t.Fatalf("generated documentation is stale; run `make docs`:\n  %s", strings.Join(problems, "\n  "))
	}

	// Two pages per noun: nexus_<noun>.md and nexus_<noun>_get.md. The glob
	// also matches the configured-hosts command's pages (nexus_host.md and
	// nexus_host_<verb>.md), which are not the provider's.
	var want []string
	nouns := map[string]bool{}
	for _, op := range host.Spec().Ops {
		nouns[op.CLI.Noun] = true
		want = append(want, "nexus_"+op.CLI.Noun+".md", "nexus_"+op.CLI.Noun+"_"+op.CLI.Verb+".md")
	}
	paths, err := filepath.Glob(filepath.Join(docs, "cli", "nexus_host*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range paths {
		if name := filepath.Base(p); name != "nexus_host.md" && !strings.HasPrefix(name, "nexus_host_") {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(nouns) != 10 || len(got) != 20 || !slices.Equal(got, want) {
		t.Errorf("docs/cli/nexus_host*.md = %v (%d), want the 20 pages %v", got, len(got), want)
	}

	api, err := os.ReadFile(filepath.Join(docs, "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(docs, "cli", "nexus.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range host.Spec().Ops {
		if !strings.Contains(string(api), "Operation ID: `"+op.ID+"`") {
			t.Errorf("docs/api.md lacks operation %s", op.ID)
		}
	}
	for noun := range nouns {
		if link := "(nexus_" + noun + ".md)"; !strings.Contains(string(index), link) {
			t.Errorf("docs/cli/nexus.md does not link %s", link)
		}
	}
}
