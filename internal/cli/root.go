// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package cli is the nexus command tree. Commands take the form
// `nexus <noun> <verb> [args]` (FR-29). No flag disables TLS verification,
// and no flag accepts a token (FR-35).
package cli

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/version"
)

const programName = "nexus"

// Terminal is the controlling terminal, used for confirmations and the
// token prompt even when stdin is a pipe.
type Terminal interface {
	io.Writer
	ReadLine() (string, error)
	// ReadSecret reads a line without echoing it.
	ReadSecret() (string, error)
	Close() error
}

// Env is the process environment a command runs in.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// OpenTerminal opens the controlling terminal. Nil means /dev/tty.
	OpenTerminal func() (Terminal, error)
	// RootCAs replaces the system trust store; nil means the system store.
	// It is a test seam only, because Go on macOS ignores SSL_CERT_FILE.
	RootCAs *x509.CertPool
}

// OSEnv is the real process environment.
func OSEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

// app is the state shared by one invocation's commands.
type app struct {
	env     Env
	host    string // --host
	verbose bool   // --verbose
	target  string // The host name an error message should name
	// format is the output format of the data command running, once
	// known; in a machine format its errors are JSON (FR-30).
	format output.Format
	// specs are the declarations the command tree is generated from; nil
	// means catalog.All().
	specs     []def.ProviderSpec
	generated map[*cobra.Command]*opCommand
	cfg       *clientconfig.Config // Read once, by config
	// intr detaches the waits of the data command running on SIGINT.
	intr *interrupter
	// multi is set when the data command running targets several hosts,
	// so text on stderr is labeled with its host.
	multi bool
}

// Run executes args and returns the process exit code.
func Run(args []string, env Env) int {
	return runWithSpecs(args, env, catalog.All())
}

// runWithSpecs is Run with the tree generated from specs; tests add specs.
func runWithSpecs(args []string, env Env, specs []def.ProviderSpec) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	// Hosts report progress, output and failures concurrently: one write
	// at a time keeps each line whole.
	env.Stderr = &lockedWriter{w: env.Stderr}
	a := &app{env: env, specs: specs}
	root, err := a.buildRoot()
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n", programName, err)
		return ExitFailure
	}
	root.SetArgs(args)
	err = unknownCommand(root, args)
	if err == nil {
		err = root.Execute()
	}
	if err == nil {
		return ExitOK
	}
	var re *runError
	if !errors.As(err, &re) {
		// Errors cobra raises itself: unknown commands, flags and arguments.
		fmt.Fprintf(env.Stderr, "%s: %v\nRun '%s --help' for usage.\n", programName, err, programName)
		return ExitUsage
	}
	var rep *reportedError
	if errors.As(re.err, &rep) {
		// Each host's or line's failure is already on stderr.
		if !a.format.Machine() {
			fmt.Fprintf(env.Stderr, "%s: %s\n", programName, rep.summary)
		}
		return rep.code
	}
	if a.format.Machine() {
		host := a.target
		if host == "" {
			host = a.host
		}
		// Like Fprintf below: a failed write to stderr has nowhere to go.
		_ = output.WriteLine(env.Stderr, errorObject(re.err, host))
	} else {
		fmt.Fprintf(env.Stderr, "%s: %s\n", programName, Message(re.err, a.target))
	}
	return ExitCode(re.err)
}

// runError marks an error returned by a command's own logic, as opposed to
// one cobra raised while parsing.
type runError struct{ err error }

func (e *runError) Error() string { return e.err.Error() }
func (e *runError) Unwrap() error { return e.err }

// run adapts a command body so its errors are told apart from parse errors.
func run(fn func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if err := fn(cmd, args); err != nil {
			return &runError{err: err}
		}
		return nil
	}
}

// noVerb is the body of a noun with no verb: usage, exit 2.
func noVerb(cmd *cobra.Command, args []string) error {
	cmd.SetOut(cmd.ErrOrStderr())
	_ = cmd.Usage()
	if len(args) > 0 {
		return &runError{err: usagef("unknown command %q for %q", args[0], cmd.CommandPath())}
	}
	return &runError{err: usagef("%s needs a command", cmd.CommandPath())}
}

// unknownCommand fails args that name no command: an argument left over
// where root or a noun expects a command. It runs before cobra parses
// flags, so the unknown command is reported, exit 2, whatever flags follow
// it, --help included; cobra would report a verb's flag as unknown first,
// or print the parent's help and succeed.
func unknownCommand(root *cobra.Command, args []string) error {
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return nil // Cobra's hidden completion command, added by Execute
	}
	// Cobra adds --help and -h to a command only as it executes it, and
	// Find reads a flag it does not know as taking the next word as its
	// value: --help before a noun would swallow the noun. Add them first,
	// as Execute would, so Find reads them as the booleans they are.
	initHelpFlags(root)
	target, rest, err := root.Find(args)
	if err != nil || !target.HasSubCommands() {
		return nil
	}
	if words := commandWords(target, rest); len(words) > 0 {
		return noVerb(target, words)
	}
	return nil
}

// initHelpFlags adds the default help flag to cmd and its subcommands.
func initHelpFlags(cmd *cobra.Command) {
	cmd.InitDefaultHelpFlag()
	for _, c := range cmd.Commands() {
		initHelpFlags(c)
	}
}

// commandWords are the arguments of args that are not flags or flag
// values, read as cobra's Find reads them to find a command: a flag that
// is not a known boolean takes the next argument as its value. That
// includes the root's --version, which cobra adds only once it executes.
func commandWords(cmd *cobra.Command, args []string) []string {
	fs := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	fs.AddFlagSet(cmd.Flags())
	fs.AddFlagSet(cmd.InheritedFlags())
	boolean := func(f *pflag.Flag) bool { return f != nil && f.NoOptDefVal != "" }
	var words []string
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case s == "--":
			return words
		case strings.HasPrefix(s, "--") && !strings.Contains(s, "="):
			if !boolean(fs.Lookup(s[2:])) {
				i++
			}
		case strings.HasPrefix(s, "-") && len(s) == 2 && s != "-":
			if !boolean(fs.ShorthandLookup(s[1:])) {
				i++
			}
		case s != "" && !strings.HasPrefix(s, "-"):
			words = append(words, s)
		}
	}
	return words
}

// newRoot is buildRoot for declarations known to be valid; it panics
// otherwise, as def.Define does for a bad declaration.
func (a *app) newRoot() *cobra.Command {
	root, err := a.buildRoot()
	if err != nil {
		panic(err)
	}
	return root
}

// buildRoot builds the command tree: the hand-written commands, then one
// command per op in a.specs (9.2).
func (a *app) buildRoot() (*cobra.Command, error) {
	if a.specs == nil {
		a.specs = catalog.All()
	}
	a.generated = map[*cobra.Command]*opCommand{}
	root := &cobra.Command{
		Use:   programName,
		Short: "Manage Nexus hosts",
		Long: "nexus manages containers, packages and scripts on hosts running nexusd.\n" +
			"Commands take the form: nexus <noun> <verb> [args]\n" +
			"`nexus command get` lists them; `nexus help <noun> <verb>` shows one's usage and examples.",
		Version:       version.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE:          noVerb,
	}
	root.SetVersionTemplate(programName + " {{.Version}}\n")
	root.SetIn(a.env.Stdin)
	root.SetOut(a.env.Stdout)
	root.SetErr(a.env.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	pf := root.PersistentFlags()
	pf.StringVar(&a.host, "host", "", "target hosts: configured names, comma-separated, or all (default: "+clientconfig.EnvHost+", then the default host)")
	pf.BoolVar(&a.verbose, "verbose", false, "log each request and response status to stderr")

	help := a.helpCommand()
	root.SetHelpCommand(help)
	root.AddCommand(a.hostCommand(), a.commandCommand(), a.completionCommand(), help)
	if err := a.addGenerated(root); err != nil {
		return nil, err
	}
	return root, nil
}

// configPath is the client configuration file.
func (a *app) configPath() (string, error) { return clientconfig.Path(a.env.Getenv) }

func (a *app) loadConfig() (*clientconfig.Config, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	return clientconfig.Load(path)
}

func (a *app) newClient(url, fingerprint, token string) (*client.Client, error) {
	opts := client.Options{URL: url, Fingerprint: fingerprint, Token: token, RootCAs: a.env.RootCAs}
	if a.verbose {
		opts.Verbose = a.env.Stderr
	}
	return client.New(opts)
}

// printJSON writes v as one JSON line, or indented on a terminal.
func (a *app) printJSON(v any) error {
	var data []byte
	var err error
	if isTerminal(a.env.Stdout) {
		data, err = json.MarshalIndent(v, "", "  ")
	} else {
		data, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.env.Stdout, "%s\n", data)
	return err
}

func isTerminal(w any) bool {
	f, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}

// openTerminal opens the controlling terminal.
func (a *app) openTerminal() (Terminal, error) {
	if a.env.OpenTerminal != nil {
		return a.env.OpenTerminal()
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if !term.IsTerminal(int(f.Fd())) {
		_ = f.Close()
		return nil, errors.New("/dev/tty is not a terminal")
	}
	return &ttyFile{f: f, r: bufio.NewReader(f)}, nil
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ttyFile is the real controlling terminal.
type ttyFile struct {
	f *os.File
	r *bufio.Reader
}

func (t *ttyFile) Write(p []byte) (int, error) { return t.f.Write(p) }
func (t *ttyFile) Close() error                { return t.f.Close() }

func (t *ttyFile) ReadLine() (string, error) {
	line, err := t.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ReadSecret disables echo while the line is read. term.ReadPassword reads
// the descriptor directly, so it must not be mixed with buffered input that
// is still pending; prompts always ask for the secret on a fresh line.
func (t *ttyFile) ReadSecret() (string, error) {
	b, err := term.ReadPassword(int(t.f.Fd()))
	fmt.Fprintln(t.f) // The newline the user typed was not echoed
	if err != nil {
		return "", err
	}
	return string(b), nil
}
