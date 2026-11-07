// Command nexusd is the Nexus server daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/version"
)

const name = "nexusd"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "validate-config":
			return validateConfig(args[1:], stdout, stderr)
		case "serve":
			return serve(args[1:], stderr)
		}
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage:\n  %s -version\n  %s serve [--config path]\n  %s validate-config [--config path]\n\nFlags:\n", name, name, name)
		fs.PrintDefaults()
	}
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

// validateConfig loads the file exactly as startup does, prints every
// problem, and exits. It never opens a listener or starts anything.
func validateConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name+" validate-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file to check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s validate-config: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	if _, err := config.Load(*path); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s: configuration is valid\n", *path); err != nil {
		return 1
	}
	return 0
}

// serve runs the daemon until SIGTERM. Logs go to stdout as JSON; only
// failures to start are written to stderr.
func serve(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(name+" serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s serve: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	d, err := daemon.New(cfg, daemon.Options{ConfigPath: *path})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
