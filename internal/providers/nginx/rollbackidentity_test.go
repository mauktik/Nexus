//go:build linux

// Code review B, CR-B3: a rollback undoes only what the operation did. A
// create must not replace an entry that appeared after the re-check, and
// the rollback of a create or of an overwrite must act on the inode the
// write made, never on whatever is at the name by then.

package nginx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

const intruderSite = "server {\n    listen 8299;\n}\n"

// replaceEntry makes name in dir a new file holding content, as a package
// manager or an operator replacing it would: a new inode renamed over it.
func replaceEntry(t *testing.T, dir, name, content string) {
	t.Helper()
	tmp := filepath.Join(dir, ".intruder")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Error(err)
	}
}

// nginx-provider CR-B3 A: the rollback of a created file leaves an entry that replaced it
func TestCreateRollbackLeavesAReplacedEntry(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"s.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	// After nginx -t read the created n.conf, something else replaces it.
	w.set(func(h *lcHarness) { h.afterTest = func() { replaceEntry(t, w.f.dir, "n.conf", intruderSite) } })

	o := w.write(spec.Add.Op(), spec.AddInput{Name: "n", Content: invalidSite})
	d := o.failed(t, problem.OutcomeUnknown)
	if !strings.Contains(d, "n.conf") {
		t.Errorf("detail %q does not name n.conf", d)
	}
	if got := readString(t, filepath.Join(w.f.dir, "n.conf")); got != intruderSite {
		t.Errorf("n.conf holds %q, want the entry that replaced it, %q: a rollback deleted a file it did not create",
			got, intruderSite)
	}
	if got, want := withoutStatus(w.calls()), []string{"nginx -t"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q: no reload", got, want)
	}
}

// nginx-provider CR-B3 B: the rollback of an overwrite leaves an entry that replaced the written file
func TestOverwriteRollbackLeavesAReplacedEntry(t *testing.T) {
	fakeNginxBinary(t)
	w := newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	w.useCapture("ubuntu-valid.jsonl", true, w.f.nginx)
	w.set(func(h *lcHarness) { h.afterTest = func() { replaceEntry(t, w.f.dir, "a.conf", intruderSite) } })

	o := w.write(spec.Update.Op(), spec.WriteInput{Name: "a", Content: invalidSite})
	d := o.failed(t, problem.OutcomeUnknown)
	if prev := filepath.Join(o.dir, "previous", "content"); !strings.Contains(d, prev) {
		t.Errorf("detail %q does not name the previous content at %s", d, prev)
	}
	if got := readString(t, filepath.Join(w.f.dir, "a.conf")); got != intruderSite {
		t.Errorf("a.conf holds %q, want the entry that replaced it, %q: the restore overwrote a file it did not write",
			got, intruderSite)
	}
}
