package cli

import (
	"cmp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/def"
)

// commandRow is one line of `nexus command get`.
type commandRow struct {
	Noun    string   `json:"noun"`
	Verb    string   `json:"verb"`
	Command string   `json:"command"`
	Usage   string   `json:"usage"`
	Summary string   `json:"summary"`
	Ops     []string `json:"ops,omitempty"` // Empty for a hand-written command
}

// commandCommand is `nexus command get [--verb V] [--noun N]`, the
// Get-Command equivalent (FR-29).
func (a *app) commandCommand() *cobra.Command {
	noun := &cobra.Command{Use: "command", Short: "Verbs: get", Args: cobra.ArbitraryArgs, RunE: noVerb}
	var verb, nounFilter string
	get := &cobra.Command{
		Use:     "get",
		Short:   "List the commands, optionally by verb or noun",
		Long:    "List every `nexus <noun> <verb>` command, one JSON object per line, sorted by noun and verb.",
		Example: "  nexus command get\n  nexus command get --verb get\n  nexus command get --noun container",
		Args:    cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if verb != "" && !def.IsApprovedVerb(verb) {
				return usagef("--verb %q is not an approved verb: use one of %s", verb, strings.Join(def.ApprovedVerbs(), ", "))
			}
			for _, row := range a.commandRows(cmd.Root()) {
				if (verb == "" || row.Verb == verb) && (nounFilter == "" || row.Noun == nounFilter) {
					if err := a.printJSON(row); err != nil {
						return err
					}
				}
			}
			return nil
		}),
	}
	get.Flags().StringVar(&verb, "verb", "", "only commands with this verb")
	get.Flags().StringVar(&nounFilter, "noun", "", "only commands with this noun")
	_ = get.RegisterFlagCompletionFunc("verb", cobra.FixedCompletions(def.ApprovedVerbs(), cobra.ShellCompDirectiveNoFileComp))
	noun.AddCommand(get)
	return noun
}

// commandRows lists every visible <noun> <verb> command under root.
func (a *app) commandRows(root *cobra.Command) []commandRow {
	var rows []commandRow
	for _, n := range root.Commands() {
		if n.Hidden || !n.HasSubCommands() {
			continue
		}
		for _, v := range n.Commands() {
			if v.Hidden {
				continue
			}
			row := commandRow{Noun: n.Name(), Verb: v.Name(), Command: v.CommandPath(), Usage: v.UseLine(), Summary: v.Short}
			if g := a.generated[v]; g != nil {
				for _, op := range g.ops {
					row.Ops = append(row.Ops, op.ID)
				}
			}
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(x, y commandRow) int {
		return cmp.Or(strings.Compare(x.Noun, y.Noun), strings.Compare(x.Verb, y.Verb))
	})
	return rows
}
