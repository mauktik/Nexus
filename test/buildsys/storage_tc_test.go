// Stub-style: SKIP-only (storage-provider)
//
// Stubs of planning/storage-provider/test-cases.md TC-STG-1, TC-STG-2, TC-STG-42, TC-STG-211, TC-STG-212, TC-STG-213.
// Owner WIs: 1.1, 2.1, 5.1, 5.2. Each stub compiles today and skips; the
// developer of the owning work item replaces the skip and the comment with
// the test, keeping the function name. An un-skipped passing test is the
// GREEN signal (planning/storage-provider/test-strategy.md, RED Stub Style).

package buildsys

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	storagespec "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// captureScript is the storage provider's host capture script (WI 1.1).
const captureScript = "internal/providers/storage/testdata/capture.sh"

// zfsPrefix is the fixed environment every ZFS command of capture.sh runs in.
const zfsPrefix = "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "

// scriptLine is one logical line of a shell script: its continuation lines
// joined, comments removed, with the line number it starts on and the
// top-level function it is in ("" outside any).
type scriptLine struct {
	n    int
	fn   string
	code string
}

var (
	reFuncStart = regexp.MustCompile(`^([a-z_]+)\(\) \{$`)
	reZFSCall   = regexp.MustCompile(`"\$(ZPOOL|ZFS)"\s+([a-z-]+)`)
)

// stripComment removes a `#` comment: a # at the start or after a blank,
// as TestDeployScriptsArePOSIX's checker does.
func stripComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

func scriptLines(t *testing.T, text string) []scriptLine {
	t.Helper()
	var out []scriptLine
	fn := ""
	var cur *scriptLine
	for i, raw := range strings.Split(text, "\n") {
		code := stripComment(raw)
		if cur == nil {
			if m := reFuncStart.FindStringSubmatch(code); m != nil {
				fn = m[1]
			}
			if code == "}" {
				fn = ""
			}
			cur = &scriptLine{n: i + 1, fn: fn}
		}
		trimmed := strings.TrimRight(code, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			cur.code += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		cur.code += code
		out = append(out, *cur)
		cur = nil
	}
	return out
}

func readCaptureScript(t *testing.T) (string, []scriptLine) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), captureScript))
	if err != nil {
		t.Fatal(err)
	}
	return string(b), scriptLines(t, string(b))
}

// TC-STG-1: capture.sh is POSIX sh and its default run issues only read-only ZFS subcommands. Owner WI 1.1. Requirements: NFR-8a, CON-2, CON-6.
func TestTCSTG1_CaptureShIsPOSIXShAndIts(t *testing.T) {
	text, lines := readCaptureScript(t)
	path := filepath.Join(repoRoot(t), captureScript)
	if !strings.HasPrefix(text, "#!/bin/sh\n") {
		t.Error("capture.sh: first line is not #!/bin/sh")
	}
	// The checker of TestDeployScriptsArePOSIX.
	forbidden := map[string]*regexp.Regexp{
		"[[ ]]":            regexp.MustCompile(`\[\[`),
		"pipefail":         regexp.MustCompile(`pipefail`),
		"local":            regexp.MustCompile(`(^|[;&|\s])local\s`),
		"$'...'":           regexp.MustCompile(`\$'`),
		"array assignment": regexp.MustCompile(`^\s*\w+=\(`),
		"function keyword": regexp.MustCompile(`(^|\s)function\s+\w+`),
		"here-string":      regexp.MustCompile(`<<<`),
		"source":           regexp.MustCompile(`(^|[;&|\s])source\s`),
		"&>":               regexp.MustCompile(`&>`),
		"${v:off}":         regexp.MustCompile(`\$\{\w+:[0-9]`),
	}
	for i, line := range strings.Split(text, "\n") {
		code := stripComment(line)
		for name, re := range forbidden {
			if re.MatchString(code) {
				t.Errorf("capture.sh:%d uses %s (not POSIX sh): %s", i+1, name, line)
			}
		}
	}
	for _, parser := range [][]string{{"sh", "-n"}, {"dash", "-n"}, {"busybox", "sh", "-n"}} {
		if _, err := exec.LookPath(parser[0]); err != nil {
			t.Logf("%s not on PATH: capture.sh parse skipped", parser[0])
			continue
		}
		if out, err := exec.Command(parser[0], append(parser[1:], path)...).CombinedOutput(); err != nil {
			t.Errorf("capture.sh: %s rejects it: %v\n%s", strings.Join(parser, " "), err, out)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", "-s", "sh", path).CombinedOutput(); err != nil {
			t.Errorf("capture.sh: shellcheck -s sh: %v\n%s", err, out)
		}
	} else {
		t.Log("shellcheck not on PATH: lint of capture.sh skipped")
	}

	reads := map[string]bool{"version": true, "list": true, "status": true, "get": true}
	writes := map[string]bool{"scrub": true, "snapshot": true, "create": true, "destroy": true, "export": true, "import": true, "set": true}
	optIn := map[string]bool{"write_probe": true, "error_pool": true}
	calls := 0
	for _, l := range lines {
		for _, m := range reZFSCall.FindAllStringSubmatch(l.code, -1) {
			calls++
			sub := m[2]
			switch {
			case optIn[l.fn]:
				if !reads[sub] && !writes[sub] {
					t.Errorf("capture.sh:%d: unexpected ZFS subcommand %q in %s", l.n, sub, l.fn)
				}
			case !reads[sub]:
				t.Errorf("capture.sh:%d: ZFS subcommand %q outside write_probe and error_pool (only version, list, status, get)", l.n, sub)
			}
		}
		// zpool and zfs run only through $ZPOOL and $ZFS.
		if regexp.MustCompile(`(^|[\s;&|(])(zpool|zfs)\s+(version|list|status|get|scrub|snapshot|create|destroy|export|import|set)\b`).MatchString(l.code) {
			t.Errorf("capture.sh:%d: zpool or zfs invoked by name: %s", l.n, l.code)
		}
	}
	if calls == 0 {
		t.Fatal("capture.sh: no ZFS invocation found")
	}
	for fn, flag := range map[string]string{"write_probe": "--write-probe", "error_pool": "--error-pool"} {
		reCall := regexp.MustCompile(`(^|[\s;&|(])` + fn + `(\s|$|;)`)
		reArm := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(flag) + `\)\s+` + fn + `\b`)
		defined, arms := false, 0
		for _, l := range lines {
			if strings.HasPrefix(l.code, fn+"() {") {
				defined = true
				continue
			}
			if !reCall.MatchString(l.code) {
				continue
			}
			if reArm.MatchString(l.code) {
				arms++
			} else {
				t.Errorf("capture.sh:%d: %s is reached outside its %s case arm: %s", l.n, fn, flag, l.code)
			}
		}
		if !defined || arms != 1 {
			t.Errorf("capture.sh: %s defined %v, reached from %d %s case arms, want defined and 1", fn, defined, arms, flag)
		}
	}
	for _, l := range lines {
		for _, tok := range strings.FieldsFunc(l.code, func(r rune) bool { return strings.ContainsRune(" \t;&|()`\"'$", r) }) {
			switch tok {
			case "pvesm", "qm", "pct", "pvesh", "vzdump", "curl", "wget":
				t.Errorf("capture.sh:%d invokes %s: %s", l.n, tok, l.code)
			}
		}
	}
}

// TC-STG-2: capture.sh runs ZFS commands with the fixed environment and never reads MachineID.plist or band contents. Owner WI 1.1. Requirements: NFR-5, NFR-8a, CON-7.
func TestTCSTG2_CaptureShRunsZFSCommandsWithThe(t *testing.T) {
	text, lines := readCaptureScript(t)
	for _, tool := range []string{"zpool", "zfs"} {
		if !strings.Contains(text, "for p in /usr/sbin/"+tool+" /sbin/"+tool+"; do") {
			t.Errorf("capture.sh: $%s is not resolved from /usr/sbin then /sbin", strings.ToUpper(tool))
		}
	}
	for _, l := range lines {
		for _, idx := range reZFSCall.FindAllStringIndex(l.code, -1) {
			if !strings.HasSuffix(l.code[:idx[0]], zfsPrefix) {
				t.Errorf("capture.sh:%d: a ZFS command without the prefix %q: %s", l.n, zfsPrefix, l.code)
			}
			// Each output goes to its own file through the capture helpers;
			// only error_pool's EXIT trap (cleanup) runs one directly.
			head := strings.TrimSpace(l.code)
			ok := strings.HasPrefix(head, "cap ") || strings.HasPrefix(head, "probe ") || strings.HasPrefix(head, "if probe ") ||
				(l.fn == "error_pool" && strings.HasPrefix(head, "trap '"))
			if !ok {
				t.Errorf("capture.sh:%d: a ZFS command outside cap and probe: %s", l.n, l.code)
			}
		}
	}
	var runBody strings.Builder
	for _, l := range lines {
		if l.fn == "run" {
			runBody.WriteString(l.code + "\n")
		}
	}
	for _, want := range []string{`"$@" >"$f" 2>"$f.stderr"`, `echo "Exit $RC" >>"$f"`, `echo "not found" >"$f.stderr"`, `RC=127`} {
		if !strings.Contains(runBody.String(), want) {
			t.Errorf("capture.sh: run does not contain %q (stdout to its file, then the Exit line; stderr to <file>.stderr)", want)
		}
	}
	for _, line := range []string{"cap() {", "probe() {"} {
		if !strings.Contains(text, line) {
			t.Errorf("capture.sh: no %s", line)
		}
	}
	reMachineCase := regexp.MustCompile(`^\s*MachineID\.plist\)\s+return 1 ;;$`)
	reReaders := regexp.MustCompile(`(^|[\s;&|(])(cat|head|tail|cp|plutil|od|xxd)\s`)
	duGNU := false
	for _, l := range lines {
		if strings.Contains(l.code, "MachineID") {
			if !reMachineCase.MatchString(l.code) || reReaders.MatchString(l.code) {
				t.Errorf("capture.sh:%d: MachineID outside the case that skips it: %s", l.n, l.code)
			}
		}
		if strings.Contains(l.code, "bands") {
			if !strings.Contains(l.code, `du -s --block-size=1 "$2/bands"`) && !strings.Contains(l.code, `du -sk "$2/bands"`) {
				t.Errorf("capture.sh:%d: bands outside the du oracle: %s", l.n, l.code)
			}
			duGNU = duGNU || strings.Contains(l.code, `du -s --block-size=1 "$2/bands"`)
		}
	}
	if !duGNU {
		t.Error("capture.sh: no `du -s --block-size=1` of a bundle's bands")
	}
	if strings.Contains(text, "keylocation") {
		t.Error("capture.sh mentions keylocation")
	}
}

// TC-STG-42: adding-a-provider.md documents HealthSource. Owner WI 2.1. Requirements: CON-4, NFR-9.
//
// - Setup: The file.
// - Steps:
//  1. Find the section titled "Generic mechanism added for the storage provider: `provider.HealthSource`".
//
// - Expected: It exists and names `HealthSource`, `HealthSignals`, `HealthSourcesUser`, the bound (the host's `sourceTimeout`), the one-call-in-flight rule, that the mechanism is keyed on the interface and not on a provider name, and that a disabled provider contributes no block. The intro's list of generic mechanisms names it.
func TestTCSTG42_AddingAProviderMdDocumentsHealthSource(t *testing.T) {
	guide := readFile(t, filepath.Join(repoRoot(t), "docs", "adding-a-provider.md"))
	const title = "Generic mechanism added for the storage provider: `provider.HealthSource`"
	s := section(guide, title)
	if s == "" {
		t.Fatalf("docs/adding-a-provider.md has no %q section", title)
	}
	requireAll(t, "the provider.HealthSource section", s, "`HealthSource`", "`HealthSignals`",
		"`HealthSourcesUser`", "`sourceTimeout`", "one call in flight per source",
		"keyed on the interface, not on a provider name", "a disabled provider contributes no block")

	intro, _, found := strings.Cut(guide, "## 1. ")
	if !found {
		t.Fatal("docs/adding-a-provider.md has no section 1")
	}
	requireAll(t, "the intro of docs/adding-a-provider.md", intro, "`provider.HealthSource`")
}

// storageGoFiles are the non-test Go files under internal/providers/storage,
// relative to root with forward slashes, each with its parsed imports.
func storageGoFiles(t *testing.T, root string) map[string]*ast.File {
	t.Helper()
	out := map[string]*ast.File{}
	dir := filepath.Join(root, "internal", "providers", "storage")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no Go file under internal/providers/storage")
	}
	return out
}

func importPaths(f *ast.File) []string {
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err == nil {
			out = append(out, p)
		}
	}
	return out
}

// TC-STG-211: the storage package executes only zpool and zfs, and has no network or hypervisor client. Owner WI 5.1. Requirements: FR-20, CON-6, CON-2.
func TestTCSTG211_TheStoragePackageExecutesOnlyZpoolAnd(t *testing.T) {
	root := repoRoot(t)

	// Step 1: go list names the packages; each file's own imports say which
	// one brings os/exec or a network package in.
	out, code := run(t, root, nil, "go", "list", "-deps", "-f", "{{.ImportPath}}", "./internal/providers/storage/...")
	if code != 0 {
		t.Fatalf("go list: exit %d\n%s", code, out)
	}
	var pkgs []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(l, modulePath+"/internal/providers/storage") {
			pkgs = append(pkgs, l)
		}
	}
	files := storageGoFiles(t, root)
	dirs := map[string]bool{}
	for path := range files {
		dirs[modulePath+"/"+filepath.ToSlash(filepath.Dir(path))] = true
	}
	for _, p := range pkgs {
		if !dirs[p] {
			t.Errorf("go list names %s, whose files were not read", p)
		}
	}
	network := map[string]bool{"net": true, "net/http": true, "crypto/tls": true}
	execBy := []string{}
	for path, f := range files {
		for _, imp := range importPaths(f) {
			switch {
			case imp == "os/exec":
				execBy = append(execBy, path)
			case network[imp] || strings.HasPrefix(imp, "net/http/"):
				t.Errorf("%s imports %s", path, imp)
			}
		}
	}
	slices.Sort(execBy)
	if want := []string{"internal/providers/storage/zfs/exec.go"}; !slices.Equal(execBy, want) {
		t.Errorf("os/exec is imported by %v, want only %v", execBy, want)
	}

	// Step 2: the hypervisor tools occur in no string literal as a program:
	// alone, as a path's last element, or as a command line's first word.
	tools := []string{"pvesm", "pvesh", "qm", "pct", "vzdump"}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if _, ok := n.(*ast.ImportSpec); ok {
				return false // An import path is a package, not a program
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			first, _, _ := strings.Cut(strings.TrimSpace(v), " ")
			for _, tool := range tools {
				if first == tool || strings.HasSuffix(first, "/"+tool) {
					t.Errorf("%s: the string literal %s names %s as a program", path, lit.Value, tool)
				}
			}
			return true
		})
	}

	// Step 3: go.mod has no requirement the base lacks.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH: go.mod is not diffed against " + storageBase)
	}
	show := exec.Command("git", "show", storageBase+":go.mod")
	show.Dir = root
	base, err := show.Output()
	if err != nil {
		t.Skipf("base %s not reachable (shallow clone?): %v", storageBase, err)
	}
	cur := readFile(t, filepath.Join(root, "go.mod"))
	baseReqs := requirements(string(base))
	for mod := range requirements(cur) {
		if !baseReqs[mod] {
			t.Errorf("go.mod requires %s, which %s does not", mod, storageBase)
		}
	}
}

// storageBase is the commit the storage branch is diffed against.
const storageBase = "43e5792"

// requirements are the module paths go.mod text requires.
func requirements(gomod string) map[string]bool {
	out := map[string]bool{}
	block := false
	for _, l := range strings.Split(gomod, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "require (":
			block = true
		case block && l == ")":
			block = false
		case block && l != "" && !strings.HasPrefix(l, "//"):
			out[strings.Fields(l)[0]] = true
		case strings.HasPrefix(l, "require "):
			out[strings.Fields(l)[1]] = true
		}
	}
	return out
}

// TC-STG-212: code importing both storage packages uses an explicit alias. Owner WI 5.1. Requirements: CON-5.
func TestTCSTG212_CodeImportingBothStoragePackagesUsesAn(t *testing.T) {
	root := repoRoot(t)
	const (
		accounting = modulePath + "/internal/storage"
		prov       = modulePath + "/internal/providers/storage"
	)
	aliasOf := func(f *ast.File, path string) (string, bool) {
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == path {
				if imp.Name == nil {
					return "", true
				}
				return imp.Name.Name, true
			}
		}
		return "", false
	}
	both := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		_, hasAcct := aliasOf(f, accounting)
		alias, hasProv := aliasOf(f, prov)
		if hasAcct && hasProv {
			both++
			if alias != "storageprov" {
				t.Errorf("%s imports both storage packages, %s as %q, want storageprov", path, prov, alias)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d files import both storage packages", both)
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "internal", "daemon", "providers.go"), nil,
		parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if alias, ok := aliasOf(f, prov); !ok || alias != "storageprov" {
		t.Errorf("internal/daemon/providers.go imports %s as %q (imported %v), want storageprov", prov, alias, ok)
	}
}

// TC-STG-213: docs/storage.md, docs/security.md, docs/mcp.md, docs/install.md and README cover the provider. Owner WI 5.2. Requirements: NFR-9, FR-2, FR-18, CON-5, CON-6.
//
// - Setup: The files; the storage keys from `deploy/nexusd.default.yaml`.
// - Steps:
//  1. Check each document.
//
// - Expected:
//   - `docs/storage.md` names every key of the default section with its default; the three features with their exact reason strings; every FR-15 signal id and threshold; the write allow-lists and how to enable them; the vzdump layout and name grammar; what is and is not read for Time Machine (`MachineID.plist`, `bands/` contents never); the LXC notes; `zfs-auto-snapshot` coexistence; that `internal/storage` is unrelated;
//   - `docs/security.md` has rows for `storage:read`, `storage:scrub`, `storage:snapshot` with FR-2's exposure notes, neither write scope in the "potentially root-equivalent" list, and the section "What the storage provider reads and writes";
//   - `docs/mcp.md` lists the five tools as untrusted;
//   - `docs/install.md` shows the home-server `vzdumpPaths` and `timeMachine.paths` example; `README.md`'s docs table links `docs/storage.md`; `cmd/nexus-gendocs` lists `storage.md` in `handWritten`.
//
// The keys and defaults are read from the shipped default file, the op IDs
// from the spec and the signal ids from health.go, and each documented
// reason string is also required in the code that words it, so the guide
// follows the code as built.
func TestTCSTG213_DocsStorageMdDocsSecurityMdDocs(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string { return readFile(t, filepath.Join(root, filepath.FromSlash(rel))) }
	storage := read("docs/storage.md")

	// Every key of the default section, with its default, in one table row.
	keys := storageDefaultKeys(t, read("deploy/nexusd.default.yaml"))
	if len(keys) != 19 {
		t.Errorf("deploy/nexusd.default.yaml's storage section has %d keys, want 19: %v", len(keys), keys)
	}
	var rows []string
	for _, l := range strings.Split(storage, "\n") {
		if strings.HasPrefix(l, "|") {
			rows = append(rows, l)
		}
	}
	for _, kv := range keys {
		if !slices.ContainsFunc(rows, func(r string) bool {
			return strings.Contains(r, "| `"+kv[0]+"` |") && strings.Contains(r, "| `"+kv[1]+"` |")
		}) {
			t.Errorf("docs/storage.md has no table row naming `%s` with its default `%s`", kv[0], kv[1])
		}
	}
	requireAll(t, "docs/storage.md (off by default)", storage,
		"by default the provider is off, both kinds of write are off, and every list", "is empty")

	// Every operation.
	var ids []string
	for _, op := range storagespec.Spec().Ops {
		ids = append(ids, "`"+op.ID+"`")
	}
	if len(ids) != 12 {
		t.Errorf("the storage spec declares %d operations, want 12", len(ids))
	}
	requireAll(t, "docs/storage.md (op IDs)", storage, ids...)

	// The three features and their exact reason strings, each also in the
	// code that words it.
	features := section(storage, "Features and why they are unavailable")
	requireAll(t, "docs/storage.md's features section", features, "**`zfs`**", "**`backups`**", "**`timeMachine`**",
		"`/usr/sbin`", "`/sbin`", "`/dev/zfs`", "`zfs version`", "503 `capability-unavailable`", "5 seconds")
	src := read("internal/providers/storage/preflight.go") + read("internal/providers/storage/zfs/tools.go") +
		read("internal/providers/storage/features.go")
	for doc, code := range map[string]string{
		"`storage provider requires Linux`":                                    `"storage provider requires Linux"`,
		"`Proxmox VE node: the storage provider never runs on the hypervisor`": `"Proxmox VE node: the storage provider never runs on the hypervisor"`,
		"`the Proxmox VE marker <path> cannot be checked: <error>`":            `"the Proxmox VE marker %s cannot be checked: %s"`,
		"`zfs: /usr/sbin/zpool not found (nor /sbin/zpool)`":                   `"zfs: %s/%s not found (nor %s/%s)"`,
		"`zfs: /dev/zfs not found (the zfs kernel module is not loaded)`":      `"zfs: /dev/zfs not found (the zfs kernel module is not loaded)"`,
		"`zfs: /dev/zfs is not a character device (mode <mode>)`":              `"zfs: /dev/zfs is not a character device (mode %s)"`,
		"`zfs: zpool list exited <n>: <first stderr line>`":                    `"%s exited %d: %s"`,
		"`zfs: zpool list did not finish within <bound>`":                      `"%s did not finish within %s"`,
		"`not configured`":                  `"not configured"`,
		"`not checked yet`":                 `"not checked yet"`,
		"`is not executable (mode <mode>)`": `"is not executable (mode %04o)"`,
	} {
		requireAll(t, "docs/storage.md's features section (reasons)", features, doc)
		if !strings.Contains(src, code) {
			t.Errorf("the reason %s is documented, but the provider's code has no %s", doc, code)
		}
	}

	// Every FR-15 signal id, from the code, with its thresholds and the
	// subject forms, guest:<vmid> included (TC-STG-164).
	health := read("internal/providers/storage/health.go")
	var sigs []string
	for _, m := range regexp.MustCompile(`(?m)^\s*signal\w+\s*=\s*"([a-z-]+)"`).FindAllStringSubmatch(health, -1) {
		sigs = append(sigs, "`"+m[1]+"`")
	}
	if len(sigs) != 12 {
		t.Errorf("health.go declares %d signal ids, want 12: %v", len(sigs), sigs)
	}
	signals := section(storage, "Health signals")
	requireAll(t, "docs/storage.md's health signals section", signals, sigs...)
	requireAll(t, "docs/storage.md's health signals section (thresholds)", signals,
		"`thresholds.poolCapacityWarn` (80%)", "`thresholds.poolCapacityCrit` (90%)",
		"`thresholds.scrubMaxAgeDays` (35)", "`thresholds.quotaWarn` (85%)", "`thresholds.shareWarn` (85%)",
		"`thresholds.shareCrit` (95%)", "`backups.maxAge` (192 h)", "`timeMachine.maxAge` (168 h)")
	requireAll(t, "docs/storage.md's health signals section (subjects)", signals,
		"`pool:<name>`", "`dataset:<name>`", "`guest:<type>/<vmid>`", "`guest:<vmid>`", "`share:<path>`",
		"`bundle:<path>/<name>`", "`zfs`", "nothing names its type")
	for _, form := range []string{`"guest:%d"`, `"guest:%s/%d"`, `"share:"`, `"bundle:"`, `"dataset:"`, `"pool:"`} {
		if !strings.Contains(health, form) {
			t.Errorf("internal/providers/storage/health.go has no subject form %s", form)
		}
	}
	requireAll(t, "docs/storage.md's stale providerSignals block", section(signals, "In `get_host_health`"),
		"`stale: true`", "5 minutes", "`counts: null`", "`sourceTimeout`")

	// The scrub age after a canceled or paused scrub (D-14).
	requireAll(t, "docs/storage.md's scrub-age rule", section(storage, "The scrub age after a canceled or paused scrub"),
		"`canceled`", "`paused`", "`lastScrubSource: remembered`", "`scrub-age-unknown`", "instead of `scrub-overdue`")

	// The write allow-lists, how to enable them, force and a stopped write.
	writes := section(storage, "Guarded writes")
	requireAll(t, "docs/storage.md's guarded writes section", writes,
		"`providers.storage.scrub.enabled`", "`providers.storage.scrub.pools`",
		"`providers.storage.snapshots.enabled`", "`providers.storage.snapshots.datasets`",
		"REST and the `nexus` CLI, never MCP", "409 `not-allowed`", "`configKey`",
		"sudo nexusd validate-config", "sudo systemctl reload nexusd", "--scope storage:scrub",
		"overrides one health only: a `degraded` pool", "whatever `force` says", "409 `pool-low-space`",
		"nothing was sent", "`outcome-unknown`", "`Canceled`, even when the command was already sent", "`SIGINT`",
		"hidden `storage-helper` subcommand", "outside the sandbox", "`CAP_SYS_ADMIN`",
		"security.md#what-the-storage-provider-reads-and-writes")

	// The vzdump layout, name grammar, A-63, A-64 and expectedGuests.
	vz := section(storage, "Proxmox vzdump backups")
	requireAll(t, "docs/storage.md's vzdump section", vz,
		"vzdump-<qemu|lxc>-<vmid>-<YYYY_MM_DD-HH_MM_SS>.<vma|tar>[.<zst|gz|lzo>]", "`dump/`", "`<stem>.log`",
		"`<archive>.notes`", "`<stem>.notes`", "`<archive>.protected`", "`<stem>.vma.dat`", "`<stem>.tmp`",
		"`timestampZone: node-local`", "never calls Proxmox")
	requireAll(t, "docs/storage.md's in-progress archive rule (A-63)", section(vz, "The summary per guest"),
		"counts in its guest's `count` and `totalBytes`", "never the guest's latest", "`latestAt: null`")
	requireAll(t, "docs/storage.md's expectedGuests section", section(vz, "Making a missing backup job visible"),
		"`backups.expectedGuests`", "`guest:<vmid>`", "`stale: true`")
	requireAll(t, "docs/storage.md's byte-budgeted pages (A-64)", section(vz, "Paging the archive list"),
		"896 KiB", "before `limit` items", "still returns a `nextCursor`")

	// Time Machine: what is and is not read, and why inProgress is null.
	tm := section(storage, "Time Machine shares")
	requireAll(t, "docs/storage.md's Time Machine section", tm,
		"`com.apple.TimeMachine.SnapshotHistory.plist`", "What is never read: the contents of band files",
		"`MachineID.plist`", "`bands/`", "`not determinable from the share`", "`lock`", "500,000 entries",
		"`timeMachine.sizeInterval`")

	// The LXC notes, zfs-auto-snapshot and internal/storage.
	requireAll(t, "docs/storage.md's LXC notes", section(storage, "LXC guests"),
		"leave the provider disabled", "503 `capability-unavailable`", "`counts: null`", "`op-supervise`",
		"`/etc/pve`")
	requireAll(t, "docs/storage.md's zfs-auto-snapshot section", section(storage, "Beside zfs-auto-snapshot"),
		"`zfs-auto-snap_<label>-<date>`", "never removes a `nexus-` snapshot", "never collides")
	requireAll(t, "docs/storage.md (CON-5)", section(storage, "Naming"),
		"`internal/storage` is unrelated", "`internal/providers/storage`")

	// The home-server example, in the guide and in install.md.
	example := []string{`vzdumpPaths: ["/media/vzdump"]`, `paths: ["/media/timemachine", "/media/timemachine2"]`,
		"home-server"}
	requireAll(t, "docs/storage.md (home-server example)", storage, example...)
	install := read("docs/install.md")
	requireAll(t, "docs/install.md (home-server example)", install, example...)
	requireAll(t, "docs/install.md", install, "storage.md", "`storage:read`")

	// Privacy: no email address and no bundle name but a mac-N placeholder.
	email := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.[A-Za-z]{2,}`)
	for _, rel := range []string{"docs/storage.md", "docs/security.md", "docs/mcp.md", "docs/install.md", "README.md"} {
		if m := email.FindString(read(rel)); m != "" {
			t.Errorf("%s holds an email address %q", rel, m)
		}
	}
	for _, m := range regexp.MustCompile(`bundle:/[^ `+"`"+`]*/([^/ `+"`"+`]+)`).FindAllStringSubmatch(storage, -1) {
		if m[1] != "<name>" && !regexp.MustCompile(`^mac-[0-9]+$`).MatchString(m[1]) {
			t.Errorf("docs/storage.md names a bundle %q; use the mac-N placeholders", m[1])
		}
	}

	// security.md: the three rows with FR-2's notes, neither write scope
	// in the root-equivalent list, and the reads-and-writes section.
	security := read("docs/security.md")
	for scope, notes := range map[string][]string{
		"storage:read": {"dataset and snapshot names", "mount points", "Time Machine bundle names",
			"computer names", "backup notes"},
		"storage:scrub":    {"sustained read load on every disk", "shared Proxmox storage"},
		"storage:snapshot": {"fill a pool over time", "snapshots hold freed blocks"},
	} {
		row := regexp.MustCompile("(?m)^\\|\\s*`" + regexp.QuoteMeta(scope) + "`\\s*\\|.*$").FindString(security)
		if row == "" {
			t.Errorf("docs/security.md's Scopes table has no row for `%s`", scope)
		}
		requireAll(t, "docs/security.md's "+scope+" row", row, notes...)
	}
	rootEq := section(security, "Mutating scopes are potentially root-equivalent")
	list, _, _ := strings.Cut(rootEq, "\n\n- ")
	if strings.Contains(list, "storage:") {
		t.Errorf("docs/security.md lists a storage scope as potentially root-equivalent: %q", oneLine(list))
	}
	requireAll(t, "docs/security.md's root-equivalent section", rootEq,
		"`storage:scrub` and `storage:snapshot` are not in this list")
	rw := section(security, "What the storage provider reads and writes")
	if rw == "" {
		t.Error(`docs/security.md has no "What the storage provider reads and writes" section`)
	}
	requireAll(t, "docs/security.md's storage section", rw,
		"`zpool`", "`zfs`", "`providers.storage.backups.vzdumpPaths`", "`providers.storage.timeMachine.paths`",
		"completion dates", "notes file", "log", "`MachineID.plist`", "band files", "`keylocation`",
		"user property other than the provider's own `nexus:created-by`", "run outside the sandbox",
		"fixed argument lists", "`CAP_SYS_ADMIN`", "no write is an MCP tool")

	// mcp.md lists the five tools as untrusted.
	untrusted := ""
	for _, para := range strings.Split(read("docs/mcp.md"), "\n") {
		if strings.Contains(para, "**Untrusted content.**") {
			untrusted = para
		}
	}
	var tools []string
	for _, op := range storagespec.Spec().Ops {
		if op.MCP != nil {
			tools = append(tools, "`"+op.MCP.Tool+"`")
		}
	}
	if len(tools) != 5 {
		t.Errorf("the storage spec declares %d MCP tools, want 5", len(tools))
	}
	requireAll(t, "docs/mcp.md's untrusted list", untrusted, tools...)

	// README's docs table and handWritten.
	readme := read("README.md")
	if !regexp.MustCompile(`(?m)^\| \[docs/storage\.md\]\(docs/storage\.md\) \|`).MatchString(readme) {
		t.Error("README.md's docs table has no row linking docs/storage.md")
	}
	hw := regexp.MustCompile(`(?m)^var handWritten = \[\]string\{(.*)\}$`).FindStringSubmatch(read("cmd/nexus-gendocs/main_test.go"))
	if hw == nil || !strings.Contains(hw[1], `"storage.md"`) {
		t.Error("handWritten in cmd/nexus-gendocs/main_test.go does not list storage.md")
	}
}

// storageDefaultKeys returns each leaf key of providers.storage in the
// shipped default file as {dotted key, value}, the trailing comment
// removed.
func storageDefaultKeys(t *testing.T, yaml string) [][2]string {
	t.Helper()
	lines := strings.Split(yaml, "\n")
	start := slices.Index(lines, "  storage:")
	if start < 0 {
		t.Fatal(`deploy/nexusd.default.yaml has no "  storage:" section`)
	}
	type level struct {
		indent int
		name   string
	}
	var stack []level
	var out [][2]string
	re := regexp.MustCompile(`^( +)([A-Za-z]+):\s*([^#]*?)\s*(#.*)?$`)
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		m := re.FindStringSubmatch(l)
		if m == nil || len(m[1]) <= 2 {
			break
		}
		indent := len(m[1])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		if m[3] == "" {
			stack = append(stack, level{indent, m[2]})
			continue
		}
		var path []string
		for _, s := range stack {
			path = append(path, s.name)
		}
		out = append(out, [2]string{strings.Join(append(path, m[2]), "."), m[3]})
	}
	return out
}
