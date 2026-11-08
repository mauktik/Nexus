package cli

import (
	"bufio"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// rowKeys parses `command get` output, one JSON object per line, into
// "noun verb" keys.
func rowKeys(t *testing.T, out string) []string {
	t.Helper()
	var keys []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var row struct{ Noun, Verb, Command, Usage, Summary string }
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("row %q: %v", sc.Text(), err)
		}
		if row.Command != programName+" "+row.Noun+" "+row.Verb || row.Usage == "" || row.Summary == "" {
			t.Errorf("incomplete row %+v", row)
		}
		keys = append(keys, row.Noun+" "+row.Verb)
	}
	return keys
}

// TC-161: command get lists exactly the tree's commands, filtered by verb
// and noun; help shows usage and an example; an unknown command exits 2.
func TestDiscoveryCommands(t *testing.T) {
	root := testApp(nil).newRoot()
	var all []string
	visible(root, func(c *cobra.Command) {
		if f := strings.Fields(c.CommandPath()); len(f) == 3 {
			all = append(all, f[1]+" "+f[2])
		}
	})
	slices.Sort(all)
	if len(all) < 25 {
		t.Fatalf("walked %d commands", len(all))
	}
	filter := func(noun, verb string) []string {
		var out []string
		for _, k := range all {
			n, v, _ := strings.Cut(k, " ")
			if (noun == "" || n == noun) && (verb == "" || v == verb) {
				out = append(out, k)
			}
		}
		return out
	}

	env := Env{Getenv: func(string) string { return "" }}
	for _, tc := range []struct {
		args       []string
		noun, verb string
	}{
		{nil, "", ""},
		{[]string{"--verb", "get"}, "", "get"},
		{[]string{"--noun", "container"}, "container", ""},
		{[]string{"--verb", "get", "--noun", "container"}, "container", "get"},
	} {
		r := runCLI(env, append([]string{"command", "get"}, tc.args...)...)
		if r.code != 0 {
			t.Fatalf("command get %v exited %d: %s", tc.args, r.code, r.stderr)
		}
		got := rowKeys(t, r.stdout)
		want := filter(tc.noun, tc.verb)
		if !slices.Equal(got, want) {
			t.Errorf("command get %v:\n got %v\nwant %v", tc.args, got, want)
		}
		if len(want) == 0 {
			t.Errorf("command get %v: empty expectation", tc.args)
		}
	}
	if r := runCLI(env, "command", "get", "--verb", "list"); r.code != ExitUsage {
		t.Errorf("unapproved --verb exited %d, want 2", r.code)
	}

	r := runCLI(env, "help", "container", "restart")
	if r.code != 0 {
		t.Fatalf("help container restart exited %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"Usage:", "nexus container restart NAME", "Examples:", "Flags:", "--idempotency-key"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help container restart lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, args := range [][]string{{"help", "nope", "nope"}, {"help", "container", "nope"}, {"help", "container", "restart", "x"}} {
		if r := runCLI(env, args...); r.code != ExitUsage || r.stdout != "" {
			t.Errorf("nexus %v exited %d, want 2, stdout %q", args, r.code, r.stdout)
		}
	}
	// Every command help reaches has an example.
	visible(root, func(c *cobra.Command) {
		if len(strings.Fields(c.CommandPath())) == 3 && c.Example == "" {
			t.Errorf("%s has no example", c.CommandPath())
		}
	})
}
