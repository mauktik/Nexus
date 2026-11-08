package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli/output"
	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
)

// Output flags every generated command has (FR-30).
const (
	flagOutput = "output"
	flagSelect = "select"
	flagFilter = "filter"
)

// frameText is the field of an apitypes.Frame holding its text.
const frameText = "d"

// cliTypePrefix types the errors the CLI raises itself, such as a usage or
// connection error, as problem types are urn:nexus:problem:<slug>.
const cliTypePrefix = "urn:nexus:cli:"

// nounPattern is what a noun must be: one lower-case word (FR-29).
var nounPattern = regexp.MustCompile(`^[a-z]+$`)

// opCommand is one generated `nexus <noun> <verb>` command. It runs one op,
// or two that share the noun and verb: one without a positional binding
// (a List) and one with (a Get). A name argument selects the Get.
type opCommand struct {
	noun, verb string
	ops        []def.Op                 // When merged: [without positional, with positional]
	columns    map[string][]output.Path // Op ID -> its table columns
}

// selectOp is the op args run: the Get of a merged pair when a name is
// given, and otherwise the only op or the List.
func (c *opCommand) selectOp(args []string) def.Op {
	if len(c.ops) == 2 && len(args) > 0 {
		return c.ops[1]
	}
	return c.ops[0]
}

// merged reports whether the command runs a List and a Get.
func (c *opCommand) merged() bool { return len(c.ops) == 2 }

// groupOps groups every op by noun and verb, in catalog order.
func groupOps(specs []def.ProviderSpec) ([]*opCommand, error) {
	var out []*opCommand
	byKey := map[string]*opCommand{}
	for _, spec := range specs {
		for _, op := range spec.Ops {
			if !nounPattern.MatchString(op.CLI.Noun) {
				return nil, fmt.Errorf("op %q: noun %q is not one lower-case word", op.ID, op.CLI.Noun)
			}
			if !def.IsApprovedVerb(op.CLI.Verb) {
				return nil, fmt.Errorf("op %q: verb %q is not approved", op.ID, op.CLI.Verb)
			}
			key := op.CLI.Noun + " " + op.CLI.Verb
			g := byKey[key]
			if g == nil {
				g = &opCommand{noun: op.CLI.Noun, verb: op.CLI.Verb, ops: []def.Op{op}}
				byKey[key] = g
				out = append(out, g)
				continue
			}
			// Only a List and a Get may share a command.
			prev := g.ops[0]
			if len(g.ops) != 1 || (prev.CLI.Positional == "") == (op.CLI.Positional == "") {
				return nil, fmt.Errorf("ops %q and %q both bind %q: only one with a positional and one without may share it", prev.ID, op.ID, key)
			}
			if op.CLI.Positional == "" {
				g.ops = []def.Op{op, prev}
			} else {
				g.ops = append(g.ops, op)
			}
		}
	}
	return out, nil
}

// addGenerated adds a command for every op in a.specs under root.
func (a *app) addGenerated(root *cobra.Command) error {
	groups, err := groupOps(a.specs)
	if err != nil {
		return err
	}
	nouns := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		nouns[c.Name()] = nil // Hand-written: no op may take its name
	}
	var order []string
	verbs := map[string][]string{}
	for _, g := range groups {
		noun, seen := nouns[g.noun]
		if seen && noun == nil {
			return fmt.Errorf("noun %q is a hand-written command", g.noun)
		}
		if noun == nil {
			noun = &cobra.Command{Use: g.noun, Args: cobra.ArbitraryArgs, RunE: noVerb}
			nouns[g.noun] = noun
			order = append(order, g.noun)
			root.AddCommand(noun)
		}
		cmd, err := a.newOpCommand(g)
		if err != nil {
			return err
		}
		noun.AddCommand(cmd)
		a.generated[cmd] = g
		verbs[g.noun] = append(verbs[g.noun], g.verb)
	}
	for _, n := range order {
		nouns[n].Short = "Verbs: " + strings.Join(verbs[n], ", ")
	}
	return nil
}

func (a *app) newOpCommand(g *opCommand) (*cobra.Command, error) {
	b, err := newBinding(g)
	if err != nil {
		return nil, err
	}
	g.columns = map[string][]output.Path{}
	for _, op := range g.ops {
		cols, err := a.columnsFor(op)
		if err != nil {
			return nil, fmt.Errorf("op %q: %w", op.ID, err)
		}
		g.columns[op.ID] = cols
	}
	first := g.ops[0]
	cmd := &cobra.Command{
		Use:               g.verb + argsUse(g, b),
		Short:             first.Summary,
		Long:              first.Description,
		Example:           examples(g, b),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE:              run(func(cmd *cobra.Command, args []string) error { return a.invoke(cmd, g, b, args) }),
	}
	var ids []string
	for _, op := range g.ops {
		ids = append(ids, op.ID)
	}
	cmd.Annotations = map[string]string{"nexus.ops": strings.Join(ids, ",")}
	switch {
	case g.merged():
		get := g.ops[1]
		cmd.Short = fmt.Sprintf("%s, or with %s, %s", first.Summary, strings.ToUpper(b.positional), lowerFirst(get.Summary))
		cmd.Long = fmt.Sprintf("Without %[1]s: %[2]s\n\nWith %[1]s: %[3]s", strings.ToUpper(b.positional), first.Description, get.Description)
		cmd.Args = cobra.MaximumNArgs(1)
	case first.CLI.Positional != "":
		cmd.Args = cobra.ExactArgs(1)
	case b.variadic:
		// --names may give them instead; the schema check enforces required.
		cmd.Args = cobra.ArbitraryArgs
	default:
		cmd.Args = cobra.NoArgs
	}
	b.register(cmd)
	registerOutputFlags(cmd, g)
	registerTargetFlags(cmd, b)
	registerStdinFlags(cmd, g)
	// A wrong flag or argument count is a usage error of a data command,
	// printed as JSON in a machine format like any other.
	usage := func(c *cobra.Command, err error) error {
		a.resolveFormat(c)
		return &runError{err: &UsageError{Err: err}}
	}
	cmd.SetFlagErrorFunc(usage)
	if check := cmd.Args; check != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			// With --stdin, each input line gives the positional.
			if flagSet(c, flagStdin) {
				if len(args) > 0 {
					return usage(c, fmt.Errorf("--%s gives the %s of each line; arguments %q do not apply", flagStdin, b.positional, args))
				}
				return nil
			}
			if err := check(c, args); err != nil {
				return usage(c, err)
			}
			return nil
		}
	}
	return cmd, nil
}

// registerOutputFlags declares -o, --select and --filter on cmd.
func registerOutputFlags(cmd *cobra.Command, g *opCommand) {
	var keys []string
	for _, op := range g.ops {
		for _, f := range op.Filters {
			if !slices.Contains(keys, f.Key) {
				keys = append(keys, f.Key)
			}
		}
	}
	formats := make([]string, len(output.Formats))
	for i, f := range output.Formats {
		formats[i] = string(f)
	}
	fs := cmd.Flags()
	fs.StringP(flagOutput, "o", "", "output format: "+strings.Join(formats, ", ")+" (default table on a terminal, jsonl otherwise)")
	fs.StringSlice(flagSelect, nil, "print only these dot paths of each object, comma-separated, such as name,properties.state; host and id are always kept")
	usage := "key=value, repeatable: print only objects whose dot path has this value; a repeated key matches any of its values"
	if len(keys) > 0 {
		usage += "; the host filters by " + strings.Join(keys, ", ")
	}
	fs.StringArray(flagFilter, nil, usage)
	// It fails only for an unknown or already registered flag; as in register.
	_ = cmd.RegisterFlagCompletionFunc(flagOutput, cobra.FixedCompletions(formats, cobra.ShellCompDirectiveNoFileComp))
}

var operationType = reflect.TypeFor[apitypes.Operation]()

// itemType is the Go type of one object op prints: the operation a
// long-running op answers with, one item of a list, or the output itself.
func itemType(op def.Op) reflect.Type {
	if op.Mode == def.ModeLongRunning {
		return operationType
	}
	t := op.OutputType()
	if t == nil {
		return nil
	}
	if f, ok := t.FieldByName("Items"); op.Kind == def.KindList && ok && f.Type.Kind() == reflect.Slice {
		return f.Type.Elem()
	}
	return t
}

// columnsFor is op's table columns: those it declares, else those of the
// first op declaring columns for the same output, else derived from it.
// Every declared column must name a field of the output.
func (a *app) columnsFor(op def.Op) ([]output.Path, error) {
	t := itemType(op)
	if t == nil {
		return nil, nil
	}
	declared := op.Columns
	for _, spec := range a.specs {
		for _, o := range spec.Ops {
			if len(declared) == 0 && len(o.Columns) > 0 && itemType(o) == t {
				declared = o.Columns
			}
		}
	}
	if len(declared) == 0 {
		return output.DeriveColumns(t), nil
	}
	cols := make([]output.Path, 0, len(declared))
	for _, c := range declared {
		p, err := output.ParsePath(c)
		if err != nil {
			return nil, err
		}
		if err := output.CheckPath(t, p); err != nil {
			return nil, err
		}
		cols = append(cols, p)
	}
	return cols, nil
}

// argsUse is the argument part of the usage line.
func argsUse(g *opCommand, b *binding) string {
	switch {
	case g.merged():
		return " [" + strings.ToUpper(b.positional) + "]"
	case b.positional != "":
		return " " + strings.ToUpper(b.positional)
	case b.variadic && b.required:
		return " " + strings.ToUpper(variadicField) + "..."
	case b.variadic:
		return " [" + strings.ToUpper(variadicField) + "...]"
	}
	return ""
}

// examples shows each op of g once, with its summary as a comment.
func examples(g *opCommand, b *binding) string {
	var lines []string
	for _, op := range g.ops {
		line := programName + " " + g.noun + " " + g.verb
		if p := op.CLI.Positional; p != "" {
			line += " <" + p + ">"
		}
		if b.variadic {
			line += " <name>..."
		}
		for _, f := range b.fields {
			if f.kind == kindMap && b.opFields[op.ID][f.name] {
				line += " --" + f.flag + " <key>=<value>"
			}
		}
		lines = append(lines, "  # "+op.Summary, "  "+line)
		if op.Mutating() && !op.OperationControl {
			lines = append(lines, "  # The same, safe to retry", "  "+line+" --"+flagIdempotencyKey+" <key>")
		}
	}
	return strings.Join(lines, "\n")
}

func lowerFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToLower(r)) + s[i+len(string(r)):]
	}
	return s
}

// invoke runs the op args select through the API on every target host
// (FR-32), or once per line of stdin (FR-31), and prints the objects the
// hosts answer with through the output layer.
func (a *app) invoke(cmd *cobra.Command, g *opCommand, b *binding, args []string) error {
	if flagSet(cmd, flagStdin) {
		return a.invokeStdin(cmd, g, b)
	}
	if flagSet(cmd, flagStopOnError) {
		return usagef("--%s applies only with --%s", flagStopOnError, flagStdin)
	}
	op := g.selectOp(args)
	opts, server, err := a.outputOptions(cmd, g, op)
	if err != nil {
		return err
	}
	in, err := b.input(cmd.Flags(), op, args, server)
	if err != nil {
		return err
	}
	req, err := buildRequest(op, in)
	if err != nil {
		return err
	}
	key, err := b.idempotencyKey(cmd.Flags())
	if err != nil {
		return err
	}
	if key != "" {
		req.Header = http.Header{"Idempotency-Key": []string{key}}
	}
	targets, err := a.targets(cmd)
	if err != nil {
		return err
	}
	hosts := make([]string, len(targets))
	for i, t := range targets {
		hosts[i] = t.Name
	}
	if err := a.confirmTargets(cmd, op, hosts); err != nil {
		return err
	}
	p := output.NewPrinter(a.env.Stdout, opts)
	send := func(ctx context.Context, t hostTarget) ([]byte, error) {
		c, err := a.targetClient(t)
		if err != nil {
			return nil, err
		}
		resp, err := c.Send(ctx, req)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}
	show := func(t hostTarget, body []byte) error { return a.printResponse(p, t, op, in, body) }
	if len(targets) == 1 {
		// One host: its error is the command's, as without fan-out.
		t := targets[0]
		a.target = t.Name
		body, err := send(cmd.Context(), t)
		if err != nil {
			return err
		}
		if err := show(t, body); err != nil {
			return err
		}
		return p.Close()
	}
	ferr := a.fanOut(cmd.Context(), targets, send, show)
	if err := p.Close(); err != nil {
		return err
	}
	return ferr
}

var errNotJSON = errors.New("the host answered with a body that is not JSON objects")

// resolveFormat sets the format of the running data command: -o when it
// is valid, else the default for stdout.
func (a *app) resolveFormat(cmd *cobra.Command) {
	a.format = output.DefaultFormat(isTerminal(a.env.Stdout))
	if fl := cmd.Flags().Lookup(flagOutput); fl != nil && fl.Changed {
		if f, err := output.ParseFormat(fl.Value.String()); err == nil {
			a.format = f
		}
	}
}

// outputOptions reads -o, --select and --filter, and returns the filters
// to send: those whose key op declares (FR-30).
func (a *app) outputOptions(cmd *cobra.Command, g *opCommand, op def.Op) (output.Options, []string, error) {
	a.resolveFormat(cmd)
	fs := cmd.Flags()
	if fl := fs.Lookup(flagOutput); fl.Changed {
		if _, err := output.ParseFormat(fl.Value.String()); err != nil {
			return output.Options{}, nil, &UsageError{Err: err}
		}
	}
	sel, err := fs.GetStringSlice(flagSelect)
	if err != nil {
		return output.Options{}, nil, err
	}
	paths, err := output.ParsePaths(sel)
	if err != nil {
		return output.Options{}, nil, usagef("--%s: %v", flagSelect, err)
	}
	raw, err := fs.GetStringArray(flagFilter)
	if err != nil {
		return output.Options{}, nil, err
	}
	matches, err := output.ParseFilters(raw)
	if err != nil {
		return output.Options{}, nil, &UsageError{Err: err}
	}
	var keys []string
	for _, f := range op.Filters {
		keys = append(keys, f.Key)
	}
	server, local := output.SplitFilters(matches, keys)
	o := output.Options{Format: a.format, Columns: g.columns[op.ID], Select: paths, Filters: local}
	o.Color = a.format == output.Table && output.ColorEnabled(isTerminal(a.env.Stdout), a.env.Getenv)
	if op.Mode == def.ModeStream {
		o.Text = frameText
	}
	return o, server, nil
}

// printResponse prints each object of body, labeled with host and id.
func (a *app) printResponse(p *output.Printer, t hostTarget, op def.Op, in map[string]any, body []byte) error {
	switch {
	case op.Mode == def.ModeStream:
		target, _ := in[op.CLI.Positional].(string)
		for line := range bytes.Lines(body) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			f, err := output.DecodeObject(line)
			if err != nil {
				return errNotJSON
			}
			f["id"] = frameLocator(target, f)
			if err := p.Print(label(f, t)); err != nil {
				return err
			}
		}
		return nil
	case len(bytes.TrimSpace(body)) == 0:
		return nil
	case op.Kind == def.KindList:
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return errNotJSON
		}
		for _, raw := range page.Items {
			obj, err := output.DecodeObject(raw)
			if err != nil {
				return errNotJSON
			}
			if err := p.Print(label(obj, t)); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			a.warn(t.Name, "more results: repeat with --cursor "+page.NextCursor, map[string]any{"nextCursor": page.NextCursor})
		}
		return nil
	}
	obj, err := output.DecodeObject(body)
	if err != nil {
		return errNotJSON
	}
	return p.Print(label(obj, t))
}

// label sets obj's host to the client-side configured name of t, whatever
// the server called itself, so --stdin binds back to the same host (FR-31,
// FR-32). A URL target has no configured name: it labels only an object
// the host sent without one.
func label(obj map[string]any, t hostTarget) map[string]any {
	if h, _ := obj["host"].(string); t.configured || h == "" {
		obj["host"] = t.Name
	}
	return obj
}

// frameLocator is a log frame's id (Interfaces): <target>@<o> for operation
// output, <target>@truncated for a truncation marker, and <target>@<t> for
// a container log line. It locates a frame; it is not a unique key.
func frameLocator(target string, f map[string]any) string {
	if t, _ := f["type"].(string); t == apitypes.FrameTypeTruncated {
		return target + "@truncated"
	}
	if o, ok := f["o"].(json.Number); ok {
		return target + "@" + o.String()
	}
	t, _ := f["t"].(string)
	return target + "@" + t
}

// warn writes a warning about host to stderr: a JSON line in a machine
// format.
func (a *app) warn(host, msg string, fields map[string]any) {
	if !a.format.Machine() {
		fmt.Fprintf(a.env.Stderr, "%s: warning: host %s: %s\n", programName, hostOr(host), msg)
		return
	}
	w := map[string]any{"level": "warning", "host": host, "message": msg}
	maps.Copy(w, fields)
	// As for Fprintf above: a failed write to stderr has nowhere to go.
	_ = output.WriteLine(a.env.Stderr, w)
}

// errorObject is err as a data command prints it in a machine format: the
// problem details when the host sent them, else a CLI type, with the host,
// the message and the exit code.
func errorObject(err error, host string) map[string]any {
	m := map[string]any{}
	var prob *problem.Error
	if errors.As(err, &prob) {
		// A problem that does not round-trip falls back to the CLI type
		// below; its message still carries the title and detail.
		var fields map[string]any
		if data, e := json.Marshal(prob); e == nil && json.Unmarshal(data, &fields) == nil {
			m = fields
		}
	}
	if _, ok := m["type"].(string); !ok {
		m["type"] = cliTypePrefix + errorKind(err)
	}
	m["level"] = "error"
	m["host"] = host
	m["message"] = Message(err, host)
	m["exit"] = ExitCode(err)
	return m
}

// errorKind names an error the CLI raised itself, by its exit code.
func errorKind(err error) string {
	var (
		pin    *client.PinMismatchError
		status *client.StatusError
	)
	switch {
	case errors.As(err, &pin):
		return "certificate-mismatch"
	case errors.As(err, &status):
		return "http-status"
	}
	switch ExitCode(err) {
	case ExitUsage:
		return "usage"
	case ExitAuth:
		return "auth"
	case ExitNotFound:
		return "not-found"
	case ExitConnection:
		return "connection"
	}
	return "error"
}

// buildRequest places op's input as the server reads it: the route
// wildcards in the path, the rest as query parameters for GET and as the
// JSON body otherwise.
func buildRequest(op def.Op, in map[string]any) (client.Request, error) {
	method, path, err := openapi.Route(op)
	if err != nil {
		return client.Request{}, err
	}
	path = strings.TrimPrefix(path, openapi.Prefix)
	rest := maps.Clone(in)
	for _, n := range openapi.Wildcards(path) {
		v, _ := in[n].(string)
		if v == "" || v == "." || v == ".." {
			return client.Request{}, usagef("%s must be given, and not be . or ..", strings.ToUpper(n))
		}
		path = strings.Replace(path, "{"+n+"}", url.PathEscape(v), 1)
		delete(rest, n)
	}
	req := client.Request{Method: method, Path: path}
	if op.Mode == def.ModeStream {
		req.Accept = "application/x-ndjson"
	}
	if method != http.MethodGet {
		req.Body = rest
		return req, nil
	}
	q := url.Values{}
	for k, v := range rest {
		switch v := v.(type) {
		case string:
			q.Set(k, v)
		case int:
			q.Set(k, strconv.Itoa(v))
		case bool:
			q.Set(k, strconv.FormatBool(v))
		case []string:
			q[k] = v
		default:
			return client.Request{}, fmt.Errorf("op %q: input %q cannot be sent as a query parameter", op.ID, k)
		}
	}
	req.Query = q
	return req, nil
}
