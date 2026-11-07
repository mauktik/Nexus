// Command nexus is the Nexus command-line client. This stub only reports its version.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mauktik/nexus/internal/version"
)

const name = "nexus"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		if _, err := fmt.Fprintf(stdout, "%s %s\n", name, version.Version); err != nil {
			return 1
		}
		return 0
	}
	fs.Usage()
	return 2
}
