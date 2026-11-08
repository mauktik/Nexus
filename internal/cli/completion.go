package cli

import (
	"github.com/spf13/cobra"
)

// completionShells are the shells a script is generated for (FR-29).
var completionShells = []string{"bash", "zsh", "fish"}

// completionCommand is `nexus completion get --shell bash|zsh|fish`. It
// replaces cobra's default `completion <shell>`, which does not follow the
// <noun> <verb> grammar. The scripts call nexus's hidden __complete command,
// which cobra adds at run time.
func (a *app) completionCommand() *cobra.Command {
	noun := &cobra.Command{Use: "completion", Short: "Verbs: get", Args: cobra.ArbitraryArgs, RunE: noVerb}
	var shell string
	get := &cobra.Command{
		Use:   "get --shell bash|zsh|fish",
		Short: "Print the shell completion script",
		Long: "Print the completion script for a shell to stdout.\n\n" +
			"bash (needs the bash-completion package):\n  nexus completion get --shell bash > ~/.nexus.bash && echo '. ~/.nexus.bash' >> ~/.bashrc\n" +
			"zsh (needs compinit):\n  nexus completion get --shell zsh > \"${fpath[1]}/_nexus\"\n" +
			"fish:\n  nexus completion get --shell fish > ~/.config/fish/completions/nexus.fish",
		Example: "  nexus completion get --shell bash > /etc/bash_completion.d/nexus\n" +
			"  nexus completion get --shell fish | source",
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			root, w := cmd.Root(), a.env.Stdout
			switch shell {
			case "bash":
				return root.GenBashCompletionV2(w, true)
			case "zsh":
				return root.GenZshCompletion(w)
			case "fish":
				return root.GenFishCompletion(w, true)
			}
			return usagef("--shell %q is not supported: use bash, zsh or fish", shell)
		}),
	}
	get.Flags().StringVar(&shell, "shell", "", "bash, zsh or fish")
	_ = get.MarkFlagRequired("shell")
	_ = get.RegisterFlagCompletionFunc("shell", cobra.FixedCompletions(completionShells, cobra.ShellCompDirectiveNoFileComp))
	noun.AddCommand(get)
	return noun
}
