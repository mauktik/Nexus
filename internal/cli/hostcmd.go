package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
)

// maxTokenBytes bounds a token read from stdin.
const maxTokenBytes = 4096

// hostCommand is `nexus host add|get|remove`, the hand-written commands
// that manage the client configuration (FR-35).
func (a *app) hostCommand() *cobra.Command {
	noun := &cobra.Command{Use: "host", Short: "Configured hosts", Args: cobra.ArbitraryArgs, RunE: noVerb}
	noun.AddCommand(a.hostAddCommand(), a.hostGetCommand(), a.hostRemoveCommand())
	return noun
}

type hostAddFlags struct {
	url         string
	fingerprint string
	tokenStdin  bool
	tags        []string
	makeDefault bool
}

func (a *app) hostAddCommand() *cobra.Command {
	var f hostAddFlags
	cmd := &cobra.Command{
		Use:   "add NAME --url URL [--fingerprint SHA256] [--token-stdin]",
		Short: "Register a host and pin its certificate",
		Long: "Register a host. The server certificate's SHA-256 fingerprint is shown and must be\n" +
			"confirmed on the terminal or match --fingerprint; compare it with `nexusd tls fingerprint`\n" +
			"on the host. A certificate that verifies against system trust is trusted that way instead.\n" +
			"The token is read from the terminal without echo, or from stdin with --token-stdin;\n" +
			"it is never accepted as an argument.",
		Example: "  nexus host add nas --url https://nas.lan:8443\n" +
			"  nexus host add nas --url https://nas.lan:8443 --fingerprint SHA256 --token-stdin < token.txt",
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error { return a.hostAdd(cmd, args[0], f) }),
	}
	fl := cmd.Flags()
	fl.StringVar(&f.url, "url", "", "host URL, such as https://nas.lan:8443 (http:// only for a loopback IP)")
	fl.StringVar(&f.fingerprint, "fingerprint", "", "expected SHA-256 fingerprint of the server certificate")
	fl.BoolVar(&f.tokenStdin, "token-stdin", false, "read the token from stdin instead of prompting")
	fl.StringSliceVar(&f.tags, "tag", nil, "tag the host; repeat for more")
	fl.BoolVar(&f.makeDefault, "default", false, "make this the default host")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

func (a *app) hostAdd(cmd *cobra.Command, name string, f hostAddFlags) error {
	if err := clientconfig.ValidateHostName(name); err != nil {
		return &UsageError{Err: err}
	}
	u, err := client.ParseURL(f.url)
	if err != nil {
		return err // Refused before connecting
	}
	if f.fingerprint != "" && u.Scheme != "https" {
		return usagef("--fingerprint applies to https URLs only")
	}
	var want string
	if f.fingerprint != "" {
		if want, err = client.NormalizeFingerprint(f.fingerprint); err != nil {
			return &UsageError{Err: err}
		}
	}
	path, err := a.configPath()
	if err != nil {
		return err
	}
	cfg, err := clientconfig.Load(path)
	if err != nil {
		return err
	}
	if _, exists := cfg.Hosts[name]; exists {
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("host %q already exists; remove it first with `nexus host remove %s`", name, name)}
	}
	a.target = name

	host := clientconfig.Host{URL: u.String(), Tags: f.tags}
	var term Terminal
	defer func() {
		if term != nil {
			_ = term.Close()
		}
	}()
	if u.Scheme == "https" {
		probe, err := client.Probe(cmd.Context(), u, a.env.RootCAs)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.env.Stderr, "Server certificate SHA-256 fingerprint:\n  %s\n", probe.Fingerprint)
		switch {
		case want != "":
			if want != probe.Fingerprint {
				return &client.PinMismatchError{Want: want, Got: probe.Fingerprint}
			}
			fmt.Fprintln(a.env.Stderr, "It matches --fingerprint; pinning it.")
			host.Fingerprint = probe.Fingerprint
		case probe.SystemTrusted:
			fmt.Fprintln(a.env.Stderr, "The certificate is trusted by the system; the host will be verified by system trust.")
		default:
			term, err = a.openTerminal()
			if err != nil {
				return &ExitError{Code: ExitConnection, Err: fmt.Errorf(
					"the certificate is not trusted by the system (%v) and there is no terminal to confirm it: "+
						"pass --fingerprint with the value `nexusd tls fingerprint` prints on the host", probe.SystemErr)}
			}
			fmt.Fprintf(term, "Compare it with `nexusd tls fingerprint` on the host.\nTrust and pin this certificate for host %q? [y/N] ", name)
			answer, err := term.ReadLine()
			if err != nil {
				return fmt.Errorf("read confirmation: %w", err)
			}
			if ans := strings.ToLower(strings.TrimSpace(answer)); ans != "y" && ans != "yes" {
				return &ExitError{Code: ExitFailure, Err: fmt.Errorf("host %q not added: the certificate was not confirmed", name)}
			}
			host.Fingerprint = probe.Fingerprint
		}
	}

	token, err := a.readToken(f.tokenStdin, &term)
	if err != nil {
		return err
	}
	host.Token = token

	// One authenticated request over the verified connection proves the URL,
	// pin and token before anything is saved.
	c, err := a.newClient(host.URL, host.Fingerprint, host.Token)
	if err != nil {
		return err
	}
	var caps apitypes.Capabilities
	if err := c.Do(cmd.Context(), http.MethodGet, "/capabilities", nil, &caps); err != nil {
		return err
	}

	if cfg.Hosts == nil {
		cfg.Hosts = map[string]clientconfig.Host{}
	}
	cfg.Hosts[name] = host
	if f.makeDefault || cfg.DefaultHost == "" {
		cfg.DefaultHost = name
	}
	if err := clientconfig.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.env.Stderr, "Added host %q (%s, nexusd %s, trust: %s) to %s\n", name, host.URL, caps.Version, host.Trust(), path)
	return nil
}

// readToken reads the token from stdin, or from the terminal without echo.
// *term is the terminal already opened for the confirmation, if any.
func (a *app) readToken(fromStdin bool, term *Terminal) (string, error) {
	var token string
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(a.env.Stdin, maxTokenBytes+1))
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		if len(data) > maxTokenBytes {
			return "", usagef("the token on stdin exceeds %d bytes", maxTokenBytes)
		}
		token = strings.TrimSpace(string(data))
	} else {
		if *term == nil {
			t, err := a.openTerminal()
			if err != nil {
				return "", usagef("no terminal to prompt for the token: pass --token-stdin and pipe the token in")
			}
			*term = t
		}
		fmt.Fprint(*term, "Token (input hidden): ")
		s, err := (*term).ReadSecret()
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		token = strings.TrimSpace(s)
	}
	if token == "" {
		return "", usagef("no token given")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", usagef("the token must be a single line without spaces")
	}
	return token, nil
}

// hostView is how a configured host is printed. It never carries the token.
type hostView struct {
	Host        string   `json:"host" yaml:"host"`
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name" yaml:"name"`
	URL         string   `json:"url" yaml:"url"`
	Trust       string   `json:"trust" yaml:"trust"`
	Fingerprint string   `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`
	Tags        []string `json:"tags" yaml:"tags"`
	Default     bool     `json:"default" yaml:"default"`
	HasToken    bool     `json:"hasToken" yaml:"hasToken"`
}

func (a *app) hostGetCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "get [NAME]",
		Short:   "List configured hosts, or show one",
		Example: "  nexus host get\n  nexus host get nas",
		Args:    cobra.MaximumNArgs(1),
		RunE: run(func(_ *cobra.Command, args []string) error {
			if output != "" && !slices.Contains([]string{"table", "json", "jsonl", "yaml"}, output) {
				return usagef("-o must be table, json, jsonl or yaml, not %q", output)
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Hosts))
			for n := range cfg.Hosts {
				names = append(names, n)
			}
			slices.Sort(names)
			if len(args) == 1 {
				if _, ok := cfg.Hosts[args[0]]; !ok {
					return &ExitError{Code: ExitNotFound, Err: &clientconfig.UnknownHostError{Name: args[0]}}
				}
				names = []string{args[0]}
			}
			views := make([]hostView, 0, len(names))
			for _, n := range names {
				h := cfg.Hosts[n]
				tags := h.Tags
				if tags == nil {
					tags = []string{}
				}
				views = append(views, hostView{Host: n, ID: n, Name: n, URL: h.URL, Trust: h.Trust(),
					Fingerprint: h.Fingerprint, Tags: tags, Default: n == cfg.DefaultHost, HasToken: h.Token != ""})
			}
			if output == "" {
				output = "jsonl"
				if isTerminal(a.env.Stdout) {
					output = "table"
				}
			}
			return writeHosts(a.env.Stdout, output, views)
		}),
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, jsonl or yaml (default table on a terminal, jsonl otherwise)")
	return cmd
}

func writeHosts(w io.Writer, format string, views []hostView) error {
	bw := bufio.NewWriter(w)
	switch format {
	case "json":
		data, err := json.MarshalIndent(views, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(bw, "%s\n", data)
	case "jsonl":
		enc := json.NewEncoder(bw)
		for _, v := range views {
			if err := enc.Encode(v); err != nil {
				return err
			}
		}
	case "yaml":
		data, err := yaml.Marshal(views)
		if err != nil {
			return err
		}
		_, _ = bw.Write(data)
	default:
		tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tURL\tTRUST\tDEFAULT\tTOKEN\tTAGS")
		for _, v := range views {
			def, tok := "", "none"
			if v.Default {
				def = "*"
			}
			if v.HasToken {
				tok = "set"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.URL, v.Trust, def, tok, strings.Join(v.Tags, ","))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func (a *app) hostRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "remove NAME",
		Short:   "Remove a configured host and its token",
		Example: "  nexus host remove nas",
		Args:    cobra.ExactArgs(1),
		RunE: run(func(_ *cobra.Command, args []string) error {
			path, err := a.configPath()
			if err != nil {
				return err
			}
			cfg, err := clientconfig.Load(path)
			if err != nil {
				return err
			}
			name := args[0]
			if _, ok := cfg.Hosts[name]; !ok {
				return &ExitError{Code: ExitNotFound, Err: &clientconfig.UnknownHostError{Name: name}}
			}
			delete(cfg.Hosts, name)
			if cfg.DefaultHost == name {
				cfg.DefaultHost = ""
			}
			if err := clientconfig.Save(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(a.env.Stderr, "Removed host %q\n", name)
			return nil
		}),
	}
}
