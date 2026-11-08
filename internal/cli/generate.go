package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
)

// nounPattern is what a noun must be: one lower-case word (FR-29).
var nounPattern = regexp.MustCompile(`^[a-z]+$`)

// opCommand is one generated `nexus <noun> <verb>` command. It runs one op,
// or two that share the noun and verb: one without a positional binding
// (a List) and one with (a Get). A name argument selects the Get.
type opCommand struct {
	noun, verb string
	ops        []def.Op // When merged: [without positional, with positional]
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
	return cmd, nil
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

// invoke runs the op args select through the API. Its output is the
// response as JSON; formatting it is the output layer's (9.3).
func (a *app) invoke(cmd *cobra.Command, g *opCommand, b *binding, args []string) error {
	op := g.selectOp(args)
	in, err := b.input(cmd.Flags(), op, args)
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
	c, err := a.client()
	if err != nil {
		return err
	}
	resp, err := c.Send(cmd.Context(), req)
	if err != nil {
		return err
	}
	if op.Mode == def.ModeStream {
		_, err := a.env.Stdout.Write(resp.Body)
		return err
	}
	if len(resp.Body) == 0 {
		return nil
	}
	if !json.Valid(resp.Body) {
		return fmt.Errorf("the host answered with a body that is not JSON")
	}
	return a.printJSON(json.RawMessage(resp.Body))
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
