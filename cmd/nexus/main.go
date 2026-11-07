// Command nexus is the Nexus command-line client.
package main

import (
	"os"

	"github.com/mauktik/nexus/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.OSEnv()))
}
