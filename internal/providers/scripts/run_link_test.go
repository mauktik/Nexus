// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// linkedFixture is a runnerFixture whose catalog path is a symlink to the
// real script, as a deploy directory with a "current" link is.
func linkedFixture(t *testing.T) (f *runnerFixture, link string) {
	t.Helper()
	f = newRunnerFixture(t, config.Script{})
	link = filepath.Join(filepath.Dir(f.path), "current.sh")
	symlink(t, f.path, link)
	c, err := NewCatalog([]config.Script{{
		ID: "deploy", Description: "Deploy", Path: link, RunAs: f.id.name, WorkingDirectory: filepath.Dir(f.path),
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.p.catalog.Store(c)
	return f, link
}

// CR-6, FR-20, 5.2/5.3: Prepare executes the path the integrity check
// resolved, not the catalog's path. With a symlinked catalog path the two
// differ, so a Prepare that ran the catalog path fails here.
func TestPrepareExecutesTheResolvedPathNotTheCatalogPath(t *testing.T) {
	f, link := linkedFixture(t)
	in := spec.InvokeInput{Name: "deploy"}
	got, err := f.p.prepareInvoke(context.Background(), in, provider.NewRun("op-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if link == f.path {
		t.Fatal("the fixture path was not a link")
	}
	if len(got.Argv) == 0 || got.Argv[0] != f.path {
		t.Errorf("argv[0] %q, want the link's target %q and not the catalog path %q", got.Argv, f.path, link)
	}
}

// CR-6: a link repointed after Admit is not followed. Prepare checks again,
// so a link now leading to a group- and world-writable script is refused.
func TestPrepareRefusesALinkRepointedAfterAdmit(t *testing.T) {
	f, link := linkedFixture(t)
	in := spec.InvokeInput{Name: "deploy"}
	if err := f.p.admitInvoke(context.Background(), in); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	evil := script(t, filepath.Join(filepath.Dir(f.path), "evil.sh"), 0o777)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	symlink(t, evil, link)
	_, err := f.p.prepareInvoke(context.Background(), in, provider.NewRun("op-1", nil))
	refusal(t, err, CheckScriptWritable, evil)
}
