package cli

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/def"
)

// CommandTree returns the command tree generated from specs, nil meaning
// catalog.All(), for the documentation generator (cmd/nexus-gendocs). It
// is built exactly as Run builds it, with an empty environment; it is for
// reading only, not for executing commands.
func CommandTree(specs []def.ProviderSpec) (*cobra.Command, error) {
	a := &app{
		env: Env{
			Stdin:  strings.NewReader(""),
			Stdout: io.Discard,
			Stderr: io.Discard,
			Getenv: func(string) string { return "" },
		},
		specs: specs,
	}
	return a.buildRoot()
}
