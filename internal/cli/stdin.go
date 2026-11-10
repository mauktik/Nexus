package cli

import (
	"bufio"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
)

// Flags of a command that acts on a resource (FR-31).
const (
	flagStdin       = "stdin"
	flagStopOnError = "stop-on-error"
)

// maxStdinLine bounds one JSON Lines input line.
const maxStdinLine = 1 << 20

// stdinOp is the op --stdin runs: the one bound to a positional argument,
// which is the Get of a merged pair.
func stdinOp(g *opCommand) (def.Op, bool) {
	for _, op := range g.ops {
		if op.CLI.Positional != "" {
			return op, true
		}
	}
	return def.Op{}, false
}

// registerStdinFlags declares --stdin and --stop-on-error on a command that
// acts on a resource.
func registerStdinFlags(cmd *cobra.Command, g *opCommand) {
	op, ok := stdinOp(g)
	if !ok {
		return
	}
	fs := cmd.Flags()
	binds := "its id"
	if op.CLI.Positional != "id" {
		binds += " or " + op.CLI.Positional
	}
	fs.Bool(flagStdin, false, "read JSON Lines objects from stdin, such as another command's output, and run once per line: "+
		"each binds its host and "+binds+"; a line without host uses the target host")
	fs.Bool(flagStopOnError, false, "with --stdin, stop at the first line that fails")
}

func flagSet(cmd *cobra.Command, name string) bool {
	fl := cmd.Flags().Lookup(name)
	return fl != nil && fl.Changed && fl.Value.String() == "true"
}

// stdinLine is one input line, bound or failed.
type stdinLine struct {
	n     int    // 1-based line number
	host  string // The line's host; empty means the target host
	value string // The positional argument
	err   error  // Why the line cannot run
}

// bindLine parses one JSON Lines object and binds it to op: host, and the
// positional from the field of its name, or for name, from the resource id
// /providers/{provider}/{type}/{name} too.
func bindLine(n int, raw []byte, op def.Op) stdinLine {
	l := stdinLine{n: n}
	obj, err := output.DecodeObject(raw)
	if err != nil {
		l.err = usagef("not a JSON object: %v", err)
		return l
	}
	if h, ok := obj["host"]; ok {
		s, isStr := h.(string)
		if !isStr || s == "" {
			l.err = usagef("host must be a non-empty string")
			return l
		}
		l.host = s
	}
	field := op.CLI.Positional
	value, err := stringField(obj, field)
	if err != nil {
		l.err = err
		return l
	}
	if field == "name" {
		id, err := stringField(obj, "id")
		if err != nil {
			l.err = err
			return l
		}
		if id != "" {
			fromID, err := resourceName(id, op)
			if err != nil {
				l.err = err
				return l
			}
			if value != "" && value != fromID {
				l.err = usagef("id %q and name %q disagree", id, value)
				return l
			}
			value = fromID
		}
	}
	if value == "" {
		if field == "name" {
			l.err = usagef("the line has neither id nor name")
		} else {
			l.err = usagef("the line has no %s", field)
		}
		return l
	}
	l.value = value
	return l
}

func stringField(obj map[string]any, field string) (string, error) {
	v, ok := obj[field]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", usagef("%s must be a string", field)
	}
	return s, nil
}

// resourceName parses a resource id to its name. The provider and resource
// type must be op's, so an id of another kind is not acted on by name.
func resourceName(id string, op def.Op) (string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "providers" || parts[2] == "" || parts[3] == "" || parts[4] == "" {
		return "", usagef("id %q is not a resource id /providers/{provider}/{type}/{name}", id)
	}
	if parts[2] != op.Provider || (op.ResourceType != "" && parts[3] != op.ResourceType) {
		return "", usagef("id %q is not a %s %s, which %s %s acts on", id, op.Provider, op.ResourceType, op.CLI.Noun, op.CLI.Verb)
	}
	return parts[4], nil
}

// stdinRun is one --stdin invocation: its op, output and per-host clients.
type stdinRun struct {
	a       *app
	cmd     *cobra.Command
	b       *binding
	op      def.Op
	server  []string // Server-side filters
	w       waitOptions
	p       *lockedPrinter
	clients map[string]*stdinHost
	token   string // NEXUS_TOKEN, which may apply to one host only
	first   string // The first host a line bound, when token is set
}

type stdinHost struct {
	t hostTarget
	c *client.Client
}

// invokeStdin runs op once per JSON Lines object on stdin (FR-31), in
// order. Each line prints its own objects or error, and a failure does not
// stop later lines unless --stop-on-error. A mutation without --yes reads
// to EOF before sending anything, so confirmation (9.5) sees every host the
// input names (A-55); otherwise lines run as they arrive.
func (a *app) invokeStdin(cmd *cobra.Command, g *opCommand, b *binding) error {
	op, _ := stdinOp(g)
	opts, server, err := a.outputOptions(cmd, g, op)
	if err != nil {
		return err
	}
	if multi, _, err := a.multiHost(cmd); err != nil {
		return err
	} else if multi {
		return usagef("--%s binds each line's host: --host may name one default host, and --host %s and --%s do not apply",
			flagStdin, allHosts, flagTag)
	}
	if fl := cmd.Flags().Lookup(flagIdempotencyKey); fl != nil && fl.Changed {
		return usagef("--%s cannot be used with --%s: each line is a different request", flagIdempotencyKey, flagStdin)
	}
	if err := checkWhatIf(cmd, op); err != nil {
		return err
	}
	w, err := waitOptionsOf(cmd)
	if err != nil {
		return err
	}
	r := &stdinRun{a: a, cmd: cmd, b: b, op: op, server: server, w: w, p: &lockedPrinter{p: output.NewPrinter(a.env.Stdout, opts)},
		clients: map[string]*stdinHost{}, token: strings.TrimSpace(a.env.Getenv(clientconfig.EnvToken))}
	stop := flagSet(cmd, flagStopOnError)
	var tally outcome
	runOne := func(l stdinLine) bool {
		// After SIGINT detached a wait, no further line is sent.
		if a.intr.hit() {
			return false
		}
		err := r.line(cmd.Context(), l)
		if err != nil {
			a.reportFailure(l.host, l.n, err)
		}
		tally.add(err)
		return (err == nil || !stop) && !a.intr.hit()
	}

	sc := bufio.NewScanner(a.env.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxStdinLine)
	n, readFailed := 0, false
	next := func() (stdinLine, bool) {
		for sc.Scan() {
			n++
			if len(strings.TrimSpace(sc.Text())) == 0 {
				continue
			}
			return bindLine(n, sc.Bytes(), op), true
		}
		// A read error, such as a line over maxStdinLine, fails that line
		// and ends the input.
		if err := sc.Err(); err != nil && !readFailed {
			readFailed = true
			return stdinLine{n: n + 1, err: usagef("reading stdin: %v", err)}, true
		}
		return stdinLine{}, false
	}

	if op.Mutating() && !flagSet(cmd, flagYes) && !flagSet(cmd, flagWhatIf) {
		var lines []stdinLine
		for l, ok := next(); ok; l, ok = next() {
			lines = append(lines, l)
		}
		if hosts, objects := r.distinctHosts(lines); objects > 0 {
			if err := a.confirmTargets(cmd, op, hosts, objects, ""); err != nil {
				return err
			}
		}
		for _, l := range lines {
			if !runOne(l) {
				break
			}
		}
	} else {
		for l, ok := next(); ok; l, ok = next() {
			if !runOne(l) {
				break
			}
		}
	}
	if err := r.p.close(); err != nil {
		return err
	}
	return tally.result(fmt.Sprintf("of %d input lines", tally.succeeded+len(tally.codes)))
}

// distinctHosts names the hosts lines bind, a line without one counting as
// the target host, and counts the lines that bound, for confirmation.
func (r *stdinRun) distinctHosts(lines []stdinLine) (hosts []string, objects int) {
	for _, l := range lines {
		if l.err != nil {
			continue
		}
		objects++
		h := l.host
		if h == "" {
			if t, err := r.a.defaultTarget(); err == nil {
				h = t.Name
			}
		}
		if h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts, objects
}

// line sends one bound line and prints what its host answers.
func (r *stdinRun) line(ctx context.Context, l stdinLine) error {
	if l.err != nil {
		return l.err
	}
	h, err := r.host(l.host)
	if err != nil {
		return err
	}
	in, err := r.b.input(r.cmd.Flags(), r.op, []string{l.value}, r.server)
	if err != nil {
		return err
	}
	req, err := buildRequest(r.op, in)
	if err != nil {
		return err
	}
	body, err := r.a.execute(ctx, h.c, h.t, r.op, in, req, r.w, r.p)
	if body != nil {
		if perr := r.p.response(r.a, h.t, r.op, in, body); err == nil {
			err = perr
		}
	}
	return err
}

// host resolves a line's host: a configured name, the NEXUS_HOST URL
// target by its name, or with none, the target host.
func (r *stdinRun) host(name string) (*stdinHost, error) {
	if h, ok := r.clients[name]; ok {
		return h, nil
	}
	var (
		t   hostTarget
		err error
	)
	if name == "" {
		t, err = r.a.defaultTarget()
	} else if dt, derr := r.a.defaultTarget(); derr == nil && !dt.configured && dt.Name == name {
		t = dt
	} else {
		var ct clientconfig.Target
		ct, err = clientconfig.Resolve(name, r.a.env.Getenv, r.a.config)
		t = hostTarget{Target: ct, configured: true}
	}
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		if r.first != "" && r.first != t.Name {
			return nil, usagef("%s replaces one host's token, already used for %s; this line names %s", clientconfig.EnvToken, r.first, t.Name)
		}
		r.first = t.Name
	}
	c, err := r.a.targetClient(t)
	if err != nil {
		return nil, err
	}
	h := &stdinHost{t: t, c: c}
	r.clients[name] = h
	return h, nil
}
