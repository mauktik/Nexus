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
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
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
}

// Run executes args and returns the process exit code.
func Run(args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	a := &app{env: env}
	root := a.newRoot()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var re *runError
	if !errors.As(err, &re) {
		// Errors cobra raises itself: unknown commands, flags and arguments.
		fmt.Fprintf(env.Stderr, "%s: %v\nRun '%s --help' for usage.\n", programName, err, programName)
		return ExitUsage
	}
	fmt.Fprintf(env.Stderr, "%s: %s\n", programName, Message(re.err, a.target))
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

func (a *app) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           programName,
		Short:         "Manage Nexus hosts",
		Long:          "nexus manages containers, packages and scripts on hosts running nexusd.\nCommands take the form: nexus <noun> <verb> [args]",
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
	pf.StringVar(&a.host, "host", "", "target host: a configured name (default: "+clientconfig.EnvHost+", then the default host)")
	pf.BoolVar(&a.verbose, "verbose", false, "log each request and response status to stderr")

	root.AddCommand(a.hostCommand(), a.capabilityCommand())
	return root
}

// capabilityCommand is `nexus capability get`. The generated command tree
// (9.2) supersedes this hand-written form.
func (a *app) capabilityCommand() *cobra.Command {
	noun := &cobra.Command{Use: "capability", Short: "Host capabilities", Args: cobra.ArbitraryArgs, RunE: noVerb}
	noun.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Show the host's providers and whether each is usable",
		Args:  cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			var caps apitypes.Capabilities
			if err := c.Do(cmd.Context(), http.MethodGet, "/capabilities", nil, &caps); err != nil {
				return err
			}
			return a.printJSON(caps)
		}),
	})
	return noun
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

// client resolves the target host and builds its client. The configuration
// is read, and its permissions checked, before anything is sent.
func (a *app) client() (*client.Client, error) {
	t, err := clientconfig.Resolve(a.host, a.env.Getenv, a.loadConfig)
	if err != nil {
		return nil, err
	}
	a.target = t.Name
	return a.newClient(t.URL, t.Fingerprint, t.Token)
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
