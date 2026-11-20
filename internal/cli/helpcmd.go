// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// helpCommand is `nexus help [<noun> [<verb>]]`, the Get-Help equivalent
// (FR-29). It replaces cobra's default help command, which prints usage and
// succeeds for an unknown topic; this one exits 2.
func (a *app) helpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "help [NOUN [VERB]]",
		Short: "Show usage and examples for a command",
		Long: "Show a command's usage, flags and examples. Without arguments, show the commands.\n" +
			"`nexus command get` lists every command.",
		Example: "  nexus help container restart\n  nexus help completion get",
		Args:    cobra.MaximumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
			parent, rest, err := cmd.Root().Find(args)
			if err != nil || len(rest) > 0 || len(args) >= 2 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []cobra.Completion
			for _, c := range parent.Commands() {
				if c.IsAvailableCommand() && c != cmd && strings.HasPrefix(c.Name(), toComplete) {
					out = append(out, cobra.CompletionWithDesc(c.Name(), c.Short))
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: run(func(cmd *cobra.Command, args []string) error {
			target, rest, err := cmd.Root().Find(args)
			if err != nil || len(rest) > 0 || target.Hidden {
				return usagef("unknown command %q: `nexus command get` lists the commands", strings.Join(args, " "))
			}
			return target.Help()
		}),
	}
}
