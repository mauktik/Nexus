package cli

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
)

// widgetInput has one field of each kind a flag is generated for.
type widgetInput struct {
	Label string   `json:"label,omitempty" jsonschema:"A label."`
	Count int      `json:"count,omitempty" jsonschema:"How many."`
	Force bool     `json:"force,omitempty" jsonschema:"Force it."`
	Color string   `json:"color,omitempty" jsonschema:"Its color."`
	Tags  []string `json:"tags,omitempty" jsonschema:"Tags; repeatable."`
}

// widgetUpdate is a test-only op, never in the catalog. It is added to the
// specs at run time, so its command appears with no CLI code edit.
var widgetUpdate = def.Define[widgetInput, struct{}](def.Op{
	ID: "testprov.actions.frob", Provider: "testprov", Kind: def.KindProviderAction, Action: "frob",
	Summary: "Frob widgets", Description: "Frobs widgets. Test only.",
	Scope: def.Scope{Name: "widgets:write"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "widget", Verb: "update"},
	Refine: func(s *jsonschema.Schema) {
		s.Properties["color"].Enum = []any{"red", "green"}
	},
})

func widgetSpecs() []def.ProviderSpec {
	return append(catalog.All(), def.ProviderSpec{Name: "testprov", ReadScope: "widgets:read", Ops: []def.Op{widgetUpdate.Op()}})
}

func testApp(specs []def.ProviderSpec) *app {
	return &app{env: Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Getenv: func(string) string { return "" }}, specs: specs}
}

// visible walks the commands a user can see: no hidden ones, such as
// cobra's __complete.
func visible(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		if !sub.Hidden {
			visible(sub, fn)
		}
	}
}

// handWritten are the only commands not generated from an op (Interfaces).
var handWritten = []string{"host add", "host get", "host remove", "command get", "help", "completion get"}

// fr29Examples are FR-29's examples, verbatim, with the op each selects.
var fr29Examples = []struct{ line, op string }{
	{"nexus container get", "docker.containers.list"},
	{"nexus container restart web", "docker.containers.restart"},
	{"nexus container update web", "docker.containers.update"},
	{"nexus packageupdate get", "apt.upgradable.list"},
	{"nexus package update", "apt.actions.upgrade"},
	{"nexus script invoke deploy-api", "scripts.scripts.invoke"},
	{"nexus operation get 0190a5d2-7c3e-7f00-8a00-000000000001", "system.operations.get"},
	{"nexus capability get", "system.capabilities.get"},
}

// TC-158: every op command is <noun> <verb>, singular noun, approved verb;
// FR-29's examples resolve; get with a name selects Get, without one List.
func TestGeneratedTreeObeysGrammar(t *testing.T) {
	a := testApp(nil)
	root := a.newRoot()
	ops := 0
	visible(root, func(c *cobra.Command) {
		depth := len(strings.Fields(c.CommandPath())) - 1
		switch {
		case depth == 1 && c.Name() == "help":
		case depth == 1:
			noun := c.Name()
			if noun == "" || strings.HasSuffix(noun, "s") || strings.ToLower(noun) != noun || strings.ContainsAny(noun, "-_ ") {
				t.Errorf("noun %q is not a singular lower-case word", noun)
			}
			if !c.HasSubCommands() {
				t.Errorf("noun %q has no verbs", noun)
			}
		case depth == 2:
			if !def.IsApprovedVerb(c.Name()) {
				t.Errorf("%s: verb %q is not approved", c.CommandPath(), c.Name())
			}
			if c.HasSubCommands() {
				t.Errorf("%s: a verb has subcommands", c.CommandPath())
			}
			if a.generated[c] != nil {
				ops++
			}
		case depth > 2:
			t.Errorf("%s: deeper than <noun> <verb>", c.CommandPath())
		}
	})
	if ops == 0 {
		t.Fatal("no generated op commands")
	}

	for _, ex := range fr29Examples {
		args := strings.Fields(ex.line)[1:]
		cmd, rest, err := root.Find(args)
		if err != nil || cmd == root {
			t.Errorf("%q does not resolve: %v", ex.line, err)
			continue
		}
		oc := a.generated[cmd]
		if oc == nil {
			t.Errorf("%q resolves to %s, not a generated command", ex.line, cmd.CommandPath())
			continue
		}
		if err := cmd.ValidateArgs(rest); err != nil {
			t.Errorf("%q: arguments rejected: %v", ex.line, err)
			continue
		}
		if got := oc.selectOp(rest).ID; got != ex.op {
			t.Errorf("%q selects %s, want %s", ex.line, got, ex.op)
		}
	}
	// The merge rule on every merged pair.
	for _, pair := range [][3]string{
		{"container", "docker.containers.list", "docker.containers.get"},
		{"package", "apt.packages.list", "apt.packages.get"},
		{"script", "scripts.scripts.list", "scripts.scripts.get"},
		{"operation", "system.operations.list", "system.operations.get"},
	} {
		cmd, _, err := root.Find([]string{pair[0], "get"})
		if err != nil || a.generated[cmd] == nil {
			t.Fatalf("%s get: %v", pair[0], err)
		}
		oc := a.generated[cmd]
		if got := oc.selectOp(nil).ID; got != pair[1] {
			t.Errorf("%s get selects %s, want %s", pair[0], got, pair[1])
		}
		if got := oc.selectOp([]string{"x"}).ID; got != pair[2] {
			t.Errorf("%s get x selects %s, want %s", pair[0], got, pair[2])
		}
		if err := cmd.ValidateArgs([]string{"a", "b"}); err == nil {
			t.Errorf("%s get accepts two names", pair[0])
		}
	}
}

// commandSet is "noun verb" for every visible command below the root.
func commandSet(root *cobra.Command) map[string]*cobra.Command {
	set := map[string]*cobra.Command{}
	visible(root, func(c *cobra.Command) {
		if c == root || c.HasSubCommands() {
			return
		}
		set[strings.TrimPrefix(c.CommandPath(), programName+" ")] = c
	})
	return set
}

// TC-159: every op has a command and every command has an op, except the
// hand-written list; a spec added at run time gets its command.
func TestEveryOpHasACommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		specs []def.ProviderSpec
	}{{"catalog", catalog.All()}, {"with test spec", widgetSpecs()}} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(tc.specs)
			root := a.newRoot()
			cmds := commandSet(root)

			fromOps := map[string][]string{}
			for _, spec := range tc.specs {
				for _, op := range spec.Ops {
					k := op.CLI.Noun + " " + op.CLI.Verb
					fromOps[k] = append(fromOps[k], op.ID)
				}
			}
			for k, ids := range fromOps {
				c, ok := cmds[k]
				if !ok {
					t.Errorf("ops %v have no command %q", ids, k)
					continue
				}
				oc := a.generated[c]
				if oc == nil {
					t.Errorf("%q is not generated", k)
					continue
				}
				var got []string
				for _, op := range oc.ops {
					got = append(got, op.ID)
				}
				slices.Sort(got)
				slices.Sort(ids)
				if !slices.Equal(got, ids) {
					t.Errorf("%q runs %v, want %v", k, got, ids)
				}
			}
			var extras []string
			for k, c := range cmds {
				if _, ok := fromOps[k]; ok {
					continue
				}
				if a.generated[c] != nil {
					t.Errorf("%q is generated but no op declares it", k)
				}
				extras = append(extras, k)
			}
			slices.Sort(extras)
			want := slices.Clone(handWritten)
			slices.Sort(want)
			if !slices.Equal(extras, want) {
				t.Errorf("commands without an op: %v, want exactly %v", extras, want)
			}
			if tc.specs[len(tc.specs)-1].Name == "testprov" {
				if _, ok := cmds["widget update"]; !ok {
					t.Error("the test spec's command is missing")
				}
			}
		})
	}
}

// TC-159: the CLI links declarations only (D-1), the system ops reach it
// through internal/systemspec, and nothing ships or calls PowerShell.
func TestCLILinksDeclarationsOnly(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./cmd/nexus")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/nexus: %v", err)
	}
	deps := strings.Fields(string(out))
	const mod = "github.com/mauktik/nexus"
	for _, d := range deps {
		banned := strings.Contains(d, "moby") || strings.Contains(d, "bbolt")
		for _, p := range []string{mod + "/internal/detach", mod + "/internal/ops"} {
			banned = banned || d == p || strings.HasPrefix(d, p+"/")
		}
		if banned {
			t.Errorf("nexus links %s", d)
		}
	}
	if !slices.Contains(deps, "github.com/mauktik/nexus/internal/systemspec") {
		t.Error("nexus does not link internal/systemspec")
	}

	needle := []byte("pw" + "sh") // Built so this file does not match itself
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "planning" || name == "build" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".ps1") {
			t.Errorf("%s: PowerShell script in the repository", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil // Binary
		}
		scanned++
		if bytes.Contains(bytes.ToLower(data), needle) {
			t.Errorf("%s references %s", path, needle)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("scanned %d text files; the walk is not covering the repository", scanned)
	}
}
