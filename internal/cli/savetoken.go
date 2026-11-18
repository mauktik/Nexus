package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
)

// flagSaveAsHost stores the secret an op returns once in the client
// configuration (ASM-8).
const flagSaveAsHost = "save-as-host"

// registerSaveFlag declares --save-as-host on a command with an op that
// returns a secret once (SecretOutput).
func registerSaveFlag(cmd *cobra.Command, g *opCommand) {
	for _, op := range g.ops {
		if op.SecretOutput != "" {
			cmd.Flags().String(flagSaveAsHost, "", "store the returned token in the client configuration as host NAME, "+
				"with the target's URL and fingerprint, and print the result without it; one target only")
			return
		}
	}
}

// flagChanged reports whether the command has flag name and it was given.
func flagChanged(cmd *cobra.Command, name string) bool {
	fl := cmd.Flags().Lookup(name)
	return fl != nil && fl.Changed
}

// tokenSave is a --save-as-host request. Everything but the save itself is
// checked before anything is sent.
type tokenSave struct {
	name   string // The host name to save the token as
	field  string // The op's SecretOutput field
	path   string // The client configuration file
	target hostTarget
}

// saveAsHostOf reads --save-as-host for op: nil when it is not given. The
// name must be a valid host name. --stdin is refused by invoke before op is
// chosen.
func saveAsHostOf(cmd *cobra.Command, op def.Op) (*tokenSave, error) {
	if !flagChanged(cmd, flagSaveAsHost) {
		return nil, nil
	}
	if op.SecretOutput == "" {
		return nil, usagef("--%s applies only to an op that returns a token", flagSaveAsHost)
	}
	name, err := cmd.Flags().GetString(flagSaveAsHost)
	if err != nil {
		return nil, err
	}
	if err := clientconfig.ValidateHostName(name); err != nil {
		return nil, usagef("--%s: %v; nothing was sent", flagSaveAsHost, err)
	}
	return &tokenSave{name: name, field: op.SecretOutput}, nil
}

// checkSave completes s's checks once the targets are known: exactly one,
// and a name that is new or already names a host at the target's URL, so
// saving rotates that host's token. Nothing has been sent yet.
func (a *app) checkSave(s *tokenSave, targets []hostTarget) error {
	if s == nil {
		return nil
	}
	if len(targets) != 1 {
		return usagef("--%s saves the token of one host; it cannot be used with %d targets; nothing was sent",
			flagSaveAsHost, len(targets))
	}
	path, err := a.configPath()
	if err != nil {
		return err
	}
	cfg, err := clientconfig.Load(path)
	if err != nil {
		return err
	}
	if err := checkSaveURL(cfg, s.name, targets[0].URL); err != nil {
		return usagef("%v; nothing was sent", err)
	}
	s.path, s.target = path, targets[0]
	return nil
}

// checkSaveURL refuses to save as name when it names a host at another URL.
func checkSaveURL(cfg *clientconfig.Config, name, url string) error {
	if h, ok := cfg.Hosts[name]; ok && !sameURL(h.URL, url) {
		return fmt.Errorf("--%s %s: host %s has URL %s, not the target's %s; a token is saved only as a new host or "+
			"as the same host to rotate it", flagSaveAsHost, name, name, h.URL, url)
	}
	return nil
}

func sameURL(x, y string) bool {
	ux, errx := client.ParseURL(x)
	uy, erry := client.ParseURL(y)
	if errx != nil || erry != nil {
		return x == y
	}
	return ux.String() == uy.String()
}

// saveToken stores the secret field of body, the host's answer, as host
// s.name and returns body without it. If the save fails, it returns body
// whole, so the secret is printed on stdout rather than lost, with an
// exit-1 error. Nothing it writes to stderr holds the secret.
func (a *app) saveToken(s *tokenSave, body []byte) ([]byte, error) {
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return body, errNotJSON
	}
	secret, _ := obj[s.field].(string)
	if secret == "" {
		return body, &ExitError{Code: ExitFailure, Err: fmt.Errorf("token not saved as host %s: the answer has no %s", s.name, s.field)}
	}
	if err := s.store(secret); err != nil {
		// No error of Load or Save quotes a token; redacting keeps it so.
		msg := strings.ReplaceAll(err.Error(), secret, "[redacted]")
		return body, &ExitError{Code: ExitFailure, Err: fmt.Errorf(
			"token not saved as host %s in %s: %s; it is printed on stdout instead", s.name, s.path, msg)}
	}
	delete(obj, s.field)
	out, err := json.Marshal(obj)
	if err != nil {
		return body, &ExitError{Code: ExitFailure, Err: fmt.Errorf("token saved as host %s in %s, but the result cannot be printed: %v",
			s.name, s.path, err)}
	}
	fmt.Fprintf(a.env.Stderr, "token saved as host %s in %s\n", s.name, s.path)
	return out, nil
}

// store saves secret as host s.name with the target's URL and fingerprint,
// keeping the tags of an existing entry. It makes the host the default only
// when there is none.
func (s *tokenSave) store(secret string) error {
	cfg, err := clientconfig.Load(s.path)
	if err != nil {
		return err
	}
	// The file may have changed since checkSave read it.
	if err := checkSaveURL(cfg, s.name, s.target.URL); err != nil {
		return err
	}
	if cfg.Hosts == nil {
		cfg.Hosts = map[string]clientconfig.Host{}
	}
	cfg.Hosts[s.name] = clientconfig.Host{URL: s.target.URL, Fingerprint: s.target.Fingerprint, Token: secret,
		Tags: cfg.Hosts[s.name].Tags}
	if cfg.DefaultHost == "" {
		cfg.DefaultHost = s.name
	}
	return clientconfig.Save(s.path, cfg)
}
