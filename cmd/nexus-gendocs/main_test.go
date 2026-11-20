package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
)

// handWritten are the documentation pages NFR-9 requires that are written
// by hand; the generator neither writes nor checks their content.
var handWritten = []string{"install.md", "security.md", "scripts.md", "mcp.md", "adding-a-provider.md", "nginx.md", "pihole.md", "host.md", "storage.md"}

// repoDocs is the checked-in docs directory.
func repoDocs(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "docs")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// copyDir copies the regular files under src into a new temporary directory.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	return dst
}

// withDescription returns the catalog with op id's description replaced.
func withDescription(t *testing.T, id, desc string) []def.ProviderSpec {
	t.Helper()
	specs := catalog.All()
	found := false
	for i := range specs {
		specs[i].Ops = slices.Clone(specs[i].Ops)
		for j := range specs[i].Ops {
			if specs[i].Ops[j].ID == id {
				specs[i].Ops[j].Description = desc
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("op %s is not in the catalog", id)
	}
	return specs
}

func mustGenerate(t *testing.T, specs []def.ProviderSpec) map[string][]byte {
	t.Helper()
	files, err := generate(specs)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return files
}

func mentions(problems []string, name string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, name) })
}

// TC-185 step 1: the checked-in generated docs equal what `make docs`
// writes from the current command definitions and OpenAPI document, and
// every documentation page NFR-9 requires is present.
func TestGeneratedDocsAreCurrent(t *testing.T) {
	docs := repoDocs(t)
	files := mustGenerate(t, nil)
	if _, ok := files["api.md"]; !ok {
		t.Fatal("the generator does not produce api.md")
	}
	if _, ok := files["cli/nexus.md"]; !ok {
		t.Fatal("the generator does not produce cli/nexus.md")
	}
	if problems := check(docs, files); len(problems) > 0 {
		t.Fatalf("generated documentation is stale; run `make docs` and commit docs/:\n  %s", strings.Join(problems, "\n  "))
	}
	for _, name := range handWritten {
		fi, err := os.Stat(filepath.Join(docs, name))
		if err != nil {
			t.Errorf("docs/%s: %v", name, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("docs/%s is empty", name)
		}
	}
	readme, err := os.ReadFile(filepath.Join(filepath.Dir(docs), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(slices.Clone(handWritten), "api.md", "cli/nexus.md") {
		if !bytes.Contains(readme, []byte("docs/"+name)) {
			t.Errorf("README.md does not link docs/%s", name)
		}
	}
}

// TC-185 step 2: on a copy of docs/, a changed op description makes the
// stale check fail naming each file it changes, and so do a hand edit, a
// missing generated file and a leftover one. The unchanged catalog checks
// clean against the same copy, so the check discriminates.
func TestStaleCheckNamesTheFile(t *testing.T) {
	tmp := copyDir(t, repoDocs(t))
	if problems := check(tmp, mustGenerate(t, nil)); len(problems) > 0 {
		t.Fatalf("control: the unchanged copy is not clean:\n  %s", strings.Join(problems, "\n  "))
	}

	changed := withDescription(t, "scripts.scripts.invoke", "A changed description of the script run.")
	problems := check(tmp, mustGenerate(t, changed))
	for _, name := range []string{"api.md", "cli/nexus_script_invoke.md"} {
		if !mentions(problems, filepath.Join(tmp, name)) {
			t.Errorf("a changed op description: the check does not name %s; it reported %q", name, problems)
		}
	}
	if mentions(problems, "nexus_container_get.md") {
		t.Errorf("an unrelated command's page is reported stale: %q", problems)
	}

	edited := filepath.Join(tmp, "cli", "nexus_container_get.md")
	if err := os.WriteFile(edited, []byte("hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tmp, "cli", "nexus_host_add.md")); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(tmp, "cli", "nexus_widget_get.md")
	if err := os.WriteFile(leftover, []byte("a removed command\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems = check(tmp, mustGenerate(t, nil))
	for _, want := range []string{edited, filepath.Join(tmp, "cli", "nexus_host_add.md"), leftover} {
		if !mentions(problems, want) {
			t.Errorf("the check does not name %s; it reported %q", want, problems)
		}
	}

	// The command line: -check exits 1 naming the file, and writing then
	// checking is clean.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-check", "-dir", tmp}, &stdout, &stderr); code != 1 {
		t.Fatalf("-check on a stale tree: exit %d, want 1; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), edited) || !strings.Contains(stderr.String(), "make docs") {
		t.Errorf("-check stderr does not name %s and the fix:\n%s", edited, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-dir", tmp}, &stdout, &stderr); code != 0 {
		t.Fatalf("writing: exit %d; stderr:\n%s", code, stderr.String())
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("writing left the page of a removed command in place: %v", err)
	}
	stderr.Reset()
	if code := run([]string{"-check", "-dir", tmp}, &stdout, &stderr); code != 0 {
		t.Fatalf("-check after writing: exit %d; stderr:\n%s", code, stderr.String())
	}
	for _, name := range handWritten {
		if _, err := os.Stat(filepath.Join(tmp, name)); err != nil {
			t.Errorf("writing removed or never had hand-written %s: %v", name, err)
		}
	}
}

// Generation depends only on the declarations: two runs are byte-equal.
func TestGenerateIsDeterministic(t *testing.T) {
	a, b := mustGenerate(t, nil), mustGenerate(t, nil)
	if len(a) != len(b) {
		t.Fatalf("runs produced %d and %d files", len(a), len(b))
	}
	for name, body := range a {
		if !bytes.Equal(body, b[name]) {
			t.Errorf("%s differs between two runs", name)
		}
	}
}

// The generator reads declarations only: it links no Docker client, bbolt,
// supervisor or daemon code, like the CLI.
func TestGeneratorLinksNoDaemonCode(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/mauktik/nexus/cmd/nexus-gendocs").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	const module = "github.com/mauktik/nexus/internal/"
	forbidden := []string{module + "detach", module + "ops", module + "core", module + "provider",
		module + "daemon", module + "server", module + "mcpadapter"}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "moby") || strings.Contains(dep, "bbolt") ||
			slices.ContainsFunc(forbidden, func(f string) bool { return dep == f || strings.HasPrefix(dep, f+"/") }) {
			t.Errorf("nexus-gendocs links %s", dep)
		}
	}
}
