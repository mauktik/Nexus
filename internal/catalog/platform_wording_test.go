package catalog_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
)

// detachedSentence is how a detached operation's description says where it
// runs (WI 2.4): on either init system, not systemd alone.
const detachedSentence = "runs detached from nexusd (a systemd unit, or a supervised process on OpenRC), so it " +
	"survives a restart of nexusd"

// nginx-provider TC-28 (FR-26): no description claims systemd alone, the
// detached operations say where they run on both init systems, and the
// generated docs carry the new wording.
func TestDescriptionsAreInitNeutral(t *testing.T) {
	for _, op := range catalog.Ops() {
		texts := []string{op.Description}
		if op.MCP != nil {
			texts = append(texts, op.MCP.Description)
		}
		for _, s := range texts {
			if strings.Contains(s, "systemd unit") && !strings.Contains(s, "OpenRC") {
				t.Errorf("%s: description names a systemd unit without OpenRC: %q", op.ID, s)
			}
		}
	}
	for _, id := range []string{"scripts.scripts.invoke", "docker.containers.update", "apt.actions.refresh"} {
		found := false
		for _, op := range catalog.Ops() {
			if op.ID == id {
				found = true
				if !strings.Contains(op.Description, detachedSentence) {
					t.Errorf("%s: description lacks %q: %q", id, detachedSentence, op.Description)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the catalog", id)
		}
	}

	if testing.Short() {
		t.Skip("-short: not running nexus-gendocs -check")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to run nexus-gendocs -check")
	}
	cmd := exec.Command(gobin, "run", "./cmd/nexus-gendocs", "-dir", "docs", "-check")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nexus-gendocs -check: %v\n%s", err, out)
	}
}
