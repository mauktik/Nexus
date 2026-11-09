package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
)

// maxConcurrentHosts bounds how many hosts one command talks to at once
// (FR-32).
const maxConcurrentHosts = 8

// Flags of every generated command that select its hosts, and --yes.
const (
	flagTag = "tag"
	flagYes = "yes"
	// allHosts is the --host value that selects every configured host.
	allHosts = "all"
)

// hostTarget is one host a command sends to.
type hostTarget struct {
	clientconfig.Target
	// configured means Name is the client-side configured name, which then
	// replaces the host the server reports in every object printed, so
	// --stdin binds back to the same host whatever the server calls itself.
	// It is false for a URL in NEXUS_HOST, which has no configured name.
	configured bool
}

// registerTargetFlags declares --tag on cmd, and --yes when it mutates.
func registerTargetFlags(cmd *cobra.Command, b *binding) {
	fs := cmd.Flags()
	fs.StringSlice(flagTag, nil, "target every configured host with this tag; comma-separated or repeated for any of several")
	if b.mutating {
		fs.Bool(flagYes, false, "do not ask for confirmation; required to mutate without a terminal")
	}
}

// config is the client configuration, read once per invocation.
func (a *app) config() (*clientconfig.Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	c, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	a.cfg = c
	return c, nil
}

// envURL reports whether the default target is a URL in NEXUS_HOST: no
// --host given, and NEXUS_HOST holds a URL rather than a name.
func (a *app) envURL() bool {
	return a.host == "" && strings.Contains(strings.TrimSpace(a.env.Getenv(clientconfig.EnvHost)), "://")
}

// hostNames splits --host on commas. An empty element is a usage error.
func hostNames(flag string) ([]string, error) {
	if flag == "" {
		return nil, nil
	}
	names := strings.Split(flag, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
		if names[i] == "" {
			return nil, usagef("--host %q: an empty host name", flag)
		}
	}
	return names, nil
}

// multiHost reports whether --host or --tag may select more than one host.
func (a *app) multiHost(cmd *cobra.Command) (bool, []string, error) {
	tags, err := tagsOf(cmd)
	if err != nil {
		return false, nil, err
	}
	names, err := hostNames(a.host)
	if err != nil {
		return false, nil, err
	}
	return len(tags) > 0 || len(names) > 1 || slices.Contains(names, allHosts), tags, nil
}

func tagsOf(cmd *cobra.Command) ([]string, error) {
	if cmd.Flags().Lookup(flagTag) == nil {
		return nil, nil
	}
	tags, err := cmd.Flags().GetStringSlice(flagTag)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		if strings.TrimSpace(t) == "" {
			return nil, usagef("--%s: an empty tag", flagTag)
		}
	}
	return tags, nil
}

// targets resolves the hosts a data command sends to (FR-32): the names in
// --host, every configured host for --host all, the hosts with a --tag, and
// otherwise the one host NEXUS_HOST or the default host names. Every target
// resolves before anything is sent; an unknown host, or no host at all, is
// a usage error.
func (a *app) targets(cmd *cobra.Command) ([]hostTarget, error) {
	multi, tags, err := a.multiHost(cmd)
	if err != nil {
		return nil, err
	}
	if !multi {
		t, err := a.defaultTarget()
		if err != nil {
			return nil, err
		}
		return []hostTarget{t}, nil
	}
	if a.host != "" && len(tags) > 0 {
		return nil, usagef("--host and --%s cannot be combined", flagTag)
	}
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	configured := slices.Sorted(maps.Keys(cfg.Hosts))
	var selected []string
	switch names, _ := hostNames(a.host); {
	case len(tags) > 0:
		for _, n := range configured {
			if slices.ContainsFunc(cfg.Hosts[n].Tags, func(t string) bool { return slices.Contains(tags, t) }) {
				selected = append(selected, n)
			}
		}
		if len(selected) == 0 {
			return nil, usagef("no configured host has tag %s: see `%s host get`", strings.Join(tags, " or "), programName)
		}
	case slices.Contains(names, allHosts):
		if len(names) > 1 {
			return nil, usagef("--host %s cannot be combined with other names", allHosts)
		}
		if len(configured) == 0 {
			return nil, &noHostError{}
		}
		selected = configured
	default:
		for _, n := range names {
			if _, ok := cfg.Hosts[n]; !ok {
				return nil, &clientconfig.UnknownHostError{Name: n}
			}
			if !slices.Contains(selected, n) {
				selected = append(selected, n)
			}
		}
	}
	token := strings.TrimSpace(a.env.Getenv(clientconfig.EnvToken))
	if token != "" && len(selected) > 1 {
		return nil, usagef("%s replaces one host's token; it cannot be used with %d hosts", clientconfig.EnvToken, len(selected))
	}
	out := make([]hostTarget, len(selected))
	for i, n := range selected {
		h := cfg.Hosts[n]
		out[i] = hostTarget{Target: clientconfig.Target{Name: n, URL: h.URL, Fingerprint: h.Fingerprint, Token: h.Token}, configured: true}
		if token != "" {
			out[i].Token = token
		}
	}
	return out, nil
}

// defaultTarget is the single host --host, NEXUS_HOST or the default host
// names. With none of them it is a usage error listing the configured hosts.
func (a *app) defaultTarget() (hostTarget, error) {
	t, err := clientconfig.Resolve(a.host, a.env.Getenv, a.config)
	if errors.Is(err, clientconfig.ErrNoHost) {
		e := &noHostError{}
		if cfg, cerr := a.config(); cerr == nil {
			e.hosts = slices.Sorted(maps.Keys(cfg.Hosts))
		}
		return hostTarget{}, e
	}
	if err != nil {
		return hostTarget{}, err
	}
	return hostTarget{Target: t, configured: !a.envURL()}, nil
}

// noHostError is clientconfig.ErrNoHost with the configured hosts named.
type noHostError struct{ hosts []string }

func (e *noHostError) Error() string {
	if len(e.hosts) == 0 {
		return clientconfig.ErrNoHost.Error()
	}
	return fmt.Sprintf("no host selected and no default host: pass --host NAME, --host %s or --%s TAG, or set %s; configured hosts: %s",
		allHosts, flagTag, clientconfig.EnvHost, strings.Join(e.hosts, ", "))
}

func (e *noHostError) Is(target error) bool { return target == clientconfig.ErrNoHost }

// targetClient builds the client for t.
func (a *app) targetClient(t hostTarget) (*client.Client, error) {
	return a.newClient(t.URL, t.Fingerprint, t.Token)
}

// fanOut runs send against every target, at most maxConcurrentHosts at
// once, and prints each host's result as it arrives. A host's failure is
// reported for that host alone and does not stop the others (FR-32); the
// hosts are independent, so there is no shared fate to cancel on.
func (a *app) fanOut(ctx context.Context, targets []hostTarget, send func(context.Context, hostTarget) ([]byte, error),
	show func(hostTarget, []byte) error) error {
	sem := make(chan struct{}, maxConcurrentHosts)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex // Guards the printer, stderr and tally
		tally outcome
	)
	finish := func(t hostTarget, body []byte, err error) {
		mu.Lock()
		defer mu.Unlock()
		// A long-running op that ended badly still has its operation to
		// print, beside its error.
		if body != nil {
			if serr := show(t, body); err == nil {
				err = serr
			}
		}
		if err != nil {
			a.reportFailure(t.Name, 0, err)
		}
		tally.add(err)
	}
	for _, t := range targets {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			finish(t, nil, ctx.Err())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := send(ctx, t)
			<-sem
			finish(t, body, err)
		}()
	}
	wg.Wait()
	return tally.result(fmt.Sprintf("of %d hosts", len(targets)))
}

// reportFailure writes one host's or input line's failure to stderr: a
// JSON line in a machine format, text otherwise. line is 0 for none.
func (a *app) reportFailure(host string, line int, err error) {
	if a.format.Machine() {
		obj := errorObject(err, host)
		if line > 0 {
			obj["line"] = line
		}
		// A failed write to stderr has nowhere to go.
		_ = output.WriteLine(a.env.Stderr, obj)
		return
	}
	prefix := programName + ": "
	if line > 0 {
		prefix += fmt.Sprintf("line %d: ", line)
	}
	fmt.Fprintf(a.env.Stderr, "%s%s\n", prefix, Message(err, host))
}

// outcome tallies the results of several hosts or input lines.
type outcome struct {
	succeeded int
	codes     []int // Exit code of each failure, in order
}

func (o *outcome) add(err error) {
	if err == nil {
		o.succeeded++
		return
	}
	o.codes = append(o.codes, ExitCode(err))
}

// result aggregates the exit code (FR-34): 0 when all succeeded, 6 when
// some succeeded and some failed, and when all failed, their common code,
// or 6 when they differ.
func (o *outcome) result(of string) error {
	if len(o.codes) == 0 {
		return nil
	}
	code := o.codes[0]
	for _, c := range o.codes[1:] {
		if c != code {
			code = ExitPartial
		}
	}
	if o.succeeded > 0 {
		code = ExitPartial
	}
	return &reportedError{code: code, summary: fmt.Sprintf("%d %s failed", len(o.codes), of)}
}

// reportedError ends a command whose failures were already reported, one
// per host or input line. Its summary is printed only as text.
type reportedError struct {
	code    int
	summary string
}

func (e *reportedError) Error() string { return e.summary }

func (e *reportedError) Unwrap() error {
	return &ExitError{Code: e.code, Err: errors.New(e.summary)}
}
