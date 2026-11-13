package catalog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/def"
	nexusd "github.com/mauktik/nexus/internal/providers/nexusd/spec"
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

// cliUploadViolations checks the CLI.Upload of every op of every spec (CON-3,
// A-49): the op it names exists in the same ProviderSpec, is KindUpload, and
// its output has a string field whose JSON name is CLI.Upload.Field. core.New
// checks only bound ops; this is the spec-level half. visited lists the ops
// with a CLI.Upload, so a caller can show the check was not vacuous.
func cliUploadViolations(specs []def.ProviderSpec) (violations, visited []string) {
	for _, spec := range specs {
		for _, op := range spec.Ops {
			u := op.CLI.Upload
			if u == nil {
				continue
			}
			visited = append(visited, op.ID)
			i := slices.IndexFunc(spec.Ops, func(o def.Op) bool { return o.ID == u.Op })
			var reason string
			switch {
			case i < 0:
				reason = fmt.Sprintf("names %q, which is not an op of provider %q", u.Op, spec.Name)
			case spec.Ops[i].Kind != def.KindUpload:
				reason = fmt.Sprintf("names %q, which is %s, not %s", u.Op, spec.Ops[i].Kind, def.KindUpload)
			default:
				reason = outputFieldProblem(spec.Ops[i], u.Field)
			}
			if reason != "" {
				violations = append(violations, fmt.Sprintf("%s: CLI.Upload %s", op.ID, reason))
			}
		}
	}
	return violations, visited
}

// outputFieldProblem says why upload's output has no string field with JSON
// name field, or returns "".
func outputFieldProblem(upload def.Op, field string) string {
	t := upload.OutputType()
	if t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return fmt.Sprintf("names %q, whose output %v is not a struct", upload.ID, t)
	}
	for _, f := range reflect.VisibleFields(t) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || f.Anonymous || name != field {
			continue
		}
		if f.Type.Kind() != reflect.String {
			return fmt.Sprintf("field %q: output field of %q is %v, not a string", field, upload.ID, f.Type)
		}
		return ""
	}
	return fmt.Sprintf("field %q: %q has no output field with that JSON name", field, upload.ID)
}

// Planted upload ops for TC-SM-93 step 3: def.Define registers IDs
// globally, so each is declared once, at package level.
type (
	plantedNoField struct {
		SHA256 string `json:"sha256"`
	}
	plantedIntField struct {
		UploadID int64 `json:"uploadId"`
	}
)

func plantedUpload(id string) def.Op {
	op := nexusd.Upload.Op()
	op.ID = id
	return op
}

var (
	plantedUploadNoField  = def.Define[nexusd.UploadInput, plantedNoField](plantedUpload("nexusd.uploads.planted-no-field"))
	plantedUploadIntField = def.Define[nexusd.UploadInput, plantedIntField](plantedUpload("nexusd.uploads.planted-int-field"))
)

// plantedNexusd returns a copy of the nexusd spec whose update's CLI.Upload
// names ref, with extra ops appended. Neither the shared ops nor their
// CLI.Upload are changed.
func plantedNexusd(ref string, extra ...def.Op) def.ProviderSpec {
	s := nexusd.Spec()
	s.Ops = append(slices.Clone(s.Ops), extra...)
	for i, op := range s.Ops {
		if op.ID == nexusd.Update.Op().ID {
			u := *op.CLI.Upload
			u.Op = ref
			s.Ops[i].CLI.Upload = &u
		}
	}
	return s
}

// TC-SM-93 (WI 6.1; FR-1, FR-17, FR-13): The catalog golden gains exactly
// the seven nexusd rows (steps 2 and 3; step 1 is TestCatalogMatchesGoldenTable).
func TestTCSM93_TheCatalogGoldenGainsExactlyThe(t *testing.T) {
	// Step 2: the catalog has no violation, and the check visited update.
	got, visited := cliUploadViolations(All())
	if len(got) != 0 {
		t.Errorf("catalog CLI.Upload violations: %q", got)
	}
	update := nexusd.Update.Op().ID
	if !slices.Contains(visited, update) {
		t.Errorf("the check visited %q, not %s: it is vacuous", visited, update)
	}

	// Step 3: each planted spec yields exactly one violation naming the op
	// and the rule.
	for _, c := range []struct {
		name string
		spec def.ProviderSpec
		want string
	}{
		{"absent", plantedNexusd("nexusd.uploads.missing"), "is not an op of provider"},
		{"list", plantedNexusd(nexusd.List.Op().ID), "not upload"},
		{"no field", plantedNexusd(plantedUploadNoField.Op().ID, plantedUploadNoField.Op()), "has no output field"},
		{"not a string", plantedNexusd(plantedUploadIntField.Op().ID, plantedUploadIntField.Op()), "not a string"},
	} {
		got, _ := cliUploadViolations([]def.ProviderSpec{c.spec})
		if len(got) != 1 || !strings.HasPrefix(got[0], update+": ") || !strings.Contains(got[0], c.want) {
			t.Errorf("%s: violations %q, want exactly one for %s containing %q", c.name, got, update, c.want)
		}
	}
	if s := nexusd.Update.Op().CLI.Upload.Op; s != nexusd.Upload.Op().ID {
		t.Errorf("planting changed the shared declaration: update's CLI.Upload.Op is %q", s)
	}
}
