package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modulePath = "github.com/mauktik/nexus"

// forbiddenSubstrings may not appear anywhere in a contract package's
// dependency paths (TC-8): Docker client and bbolt.
var forbiddenSubstrings = []string{"moby", "bbolt"}

// forbiddenPackages are module packages a contract package may not reach.
// detach and ops are supervisor code (TC-8). core and provider are the
// daemon side of the system ops and of bindings; the catalog reaches the
// system ops only through internal/systemspec.
var forbiddenPackages = []string{"internal/detach", "internal/ops", "internal/core", "internal/provider"}

// contractPackages lists the packages the CLI may import. It globs the
// provider spec packages and the system spec, so packages added later are
// covered without editing this test.
func contractPackages(t *testing.T, root string) []string {
	t.Helper()
	pkgs := []string{"./internal/catalog", "./internal/def", "./internal/apitypes", "./internal/problem"}
	if fi, err := os.Stat(filepath.Join(root, "internal", "systemspec")); err == nil && fi.IsDir() {
		pkgs = append(pkgs, "./internal/systemspec")
	}
	specs, err := filepath.Glob(filepath.Join(root, "internal", "providers", "*", "spec"))
	if err != nil {
		t.Fatalf("glob spec packages: %v", err)
	}
	for _, s := range specs {
		rel, err := filepath.Rel(root, s)
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, "./"+filepath.ToSlash(rel))
	}
	return pkgs
}

// deps runs `go list -deps` for one package in root.
func deps(t *testing.T, root, pkg string, env ...string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, stderr)
	}
	return strings.Fields(string(out))
}

// violations returns, per package, each dependency that breaks the rule.
func violations(t *testing.T, root string, pkgs []string, env ...string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	for _, pkg := range pkgs {
		for _, dep := range deps(t, root, pkg, env...) {
			if isForbidden(dep) {
				found[pkg] = append(found[pkg], dep)
			}
		}
	}
	return found
}

func isForbidden(dep string) bool {
	for _, s := range forbiddenSubstrings {
		if strings.Contains(dep, s) {
			return true
		}
	}
	for _, p := range forbiddenPackages {
		full := modulePath + "/" + p
		if dep == full || strings.HasPrefix(dep, full+"/") {
			return true
		}
	}
	return false
}

// TC-8: contract packages stay free of heavy dependencies.
func TestContractPackagesHaveNoHeavyDependencies(t *testing.T) {
	root := moduleRoot(t)
	pkgs := contractPackages(t, root)
	if v := violations(t, root, pkgs); len(v) > 0 {
		for pkg, bad := range v {
			t.Errorf("%s depends on forbidden packages: %v", pkg, bad)
		}
	}

	// The catalog reaches the system ops only through internal/systemspec.
	if slices.Contains(pkgs, "./internal/systemspec") {
		if !slices.Contains(deps(t, root, "./internal/catalog"), modulePath+"/internal/systemspec") {
			t.Error("internal/systemspec exists but internal/catalog does not import it")
		}
	}
}

func TestIsForbidden(t *testing.T) {
	for dep, want := range map[string]bool{
		"github.com/moby/moby/client":                true,
		"go.etcd.io/bbolt":                           true,
		modulePath + "/internal/detach":              true,
		modulePath + "/internal/ops":                 true,
		modulePath + "/internal/ops/lanes":           true,
		modulePath + "/internal/core":                true,
		modulePath + "/internal/provider":            true,
		modulePath + "/internal/providers/x/spec":    false,
		modulePath + "/internal/oplog":               false,
		modulePath + "/internal/def":                 false,
		"github.com/google/jsonschema-go/jsonschema": false,
	} {
		if got := isForbidden(dep); got != want {
			t.Errorf("isForbidden(%q) = %v, want %v", dep, got, want)
		}
	}
}

// TC-8: a planted import makes the check fail. A throwaway module copy gets a
// stand-in internal/ops and a new provider spec package that imports it; the
// glob must find the spec and the check must flag it.
func TestPlantedHeavyImportIsCaught(t *testing.T) {
	root := copyContractModule(t)
	write(t, filepath.Join(root, "internal", "ops", "ops.go"), "package ops\n\nconst Planted = 1\n")
	write(t, filepath.Join(root, "internal", "providers", "planted", "spec", "spec.go"),
		"package spec\n\nimport \""+modulePath+"/internal/ops\"\n\nconst X = ops.Planted\n")

	pkgs := contractPackages(t, root)
	const planted = "./internal/providers/planted/spec"
	if !slices.Contains(pkgs, planted) {
		t.Fatalf("glob did not discover %s: %v", planted, pkgs)
	}
	v := violations(t, root, pkgs, "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off", "GOTOOLCHAIN=local")
	if !slices.Contains(v[planted], modulePath+"/internal/ops") {
		t.Fatalf("planted internal/ops import not flagged; violations = %v", v)
	}
	if len(v) != 1 {
		t.Errorf("unexpected violations beyond the planted package: %v", v)
	}
}

func copyContractModule(t *testing.T) string {
	t.Helper()
	src := moduleRoot(t)
	dst := t.TempDir()
	for _, f := range []string{"go.mod", "go.sum"} {
		copyFile(t, filepath.Join(src, f), filepath.Join(dst, f))
	}
	// catalog imports every provider spec package, so they are copied too.
	specs, err := filepath.Glob(filepath.Join(src, "internal", "providers", "*", "spec"))
	if err != nil {
		t.Fatal(err)
	}
	pkgs := []string{"catalog", "def", "apitypes", "problem", "systemspec"}
	for _, dir := range specs {
		rel, err := filepath.Rel(filepath.Join(src, "internal"), dir)
		if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, rel)
	}
	for _, pkg := range pkgs {
		files, err := filepath.Glob(filepath.Join(src, "internal", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if !strings.HasSuffix(f, "_test.go") {
				copyFile(t, f, filepath.Join(dst, "internal", pkg, filepath.Base(f)))
			}
		}
	}
	return dst
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	write(t, to, string(b))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAllListsUniqueOps(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range All() {
		if spec.Name == "" {
			t.Error("provider spec with an empty name")
		}
		for _, op := range spec.Ops {
			if seen[op.ID] {
				t.Errorf("op %q listed twice", op.ID)
			}
			seen[op.ID] = true
			if op.Provider != spec.Name {
				t.Errorf("op %q has provider %q inside spec %q", op.ID, op.Provider, spec.Name)
			}
		}
	}
	if len(Ops()) != len(seen) {
		t.Errorf("Ops() = %d ops, All() lists %d", len(Ops()), len(seen))
	}
}
