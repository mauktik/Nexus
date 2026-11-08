package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
)

// Flags every generated command reserves; no input field may take them.
const (
	flagIdempotencyKey = "idempotency-key"
	flagParam          = "param"
)

// --version is the root's own flag, not inherited, so an input may take it.
var reservedFlags = []string{"help", "host", "verbose", flagIdempotencyKey, flagOutput, flagSelect, flagFilter,
	flagTag, flagYes, flagStdin, flagStopOnError}

// filterField is the input that carries server-side filters. The output
// layer's --filter fills it with the keys the op declares (9.3).
const filterField = "filter"

// variadicField is the input field trailing arguments bind to, when an op
// has no positional binding: `nexus package install curl jq`.
const variadicField = "names"

type fieldKind int

const (
	kindString  fieldKind = iota // Including an enum
	kindInt                      // integer
	kindBool                     // boolean
	kindStrings                  // array of strings; the flag repeats
	kindMap                      // object of strings; the flag repeats as k=v
)

// inputField is one input property bound to a flag.
type inputField struct {
	name  string // JSON name
	flag  string
	kind  fieldKind
	enum  []string
	usage string
}

// binding maps a generated command's flags and arguments to its ops'
// inputs. It is built from the input schemas alone, so a new op needs no
// CLI code (FR-3).
type binding struct {
	fields     []*inputField              // Flag-bound fields, in flag order
	opFields   map[string]map[string]bool // Op ID -> its flag-bound JSON names
	positional string                     // The positional input field, if any op has one
	variadic   bool                       // Trailing arguments bind to variadicField
	required   bool                       // variadicField is required
	mutating   bool                       // --idempotency-key is offered
}

// newBinding derives the flags of g's ops from their input schemas.
func newBinding(g *opCommand) (*binding, error) {
	b := &binding{opFields: map[string]map[string]bool{}}
	byFlag := map[string]*inputField{}
	for _, op := range g.ops {
		resolved, err := op.InputSchema()
		if err != nil {
			return nil, err
		}
		s := resolved.Schema()
		own := map[string]bool{}
		b.opFields[op.ID] = own
		b.mutating = b.mutating || op.Mutating()
		if p := op.CLI.Positional; p != "" {
			b.positional = p
			ps := s.Properties[p]
			if ps == nil {
				return nil, fmt.Errorf("op %q: positional %q is not an input field", op.ID, p)
			}
			if k, err := kindOf(ps); err != nil || k != kindString || !slices.Contains(s.Required, p) {
				return nil, fmt.Errorf("op %q: positional %q must be a required string", op.ID, p)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			if name == op.CLI.Positional || name == filterField {
				continue
			}
			f, err := fieldOf(name, s.Properties[name])
			if err != nil {
				return nil, fmt.Errorf("op %q: %w", op.ID, err)
			}
			if prev, ok := byFlag[f.flag]; ok {
				if prev.name != f.name || prev.kind != f.kind || !slices.Equal(prev.enum, f.enum) {
					return nil, fmt.Errorf("op %q: flag --%s means different inputs in %s %s", op.ID, f.flag, g.noun, g.verb)
				}
			} else {
				if slices.Contains(reservedFlags, f.flag) {
					return nil, fmt.Errorf("op %q: input %q takes the reserved flag --%s", op.ID, name, f.flag)
				}
				byFlag[f.flag] = f
				b.fields = append(b.fields, f)
			}
			own[name] = true
			if f.kind == kindMap && op.Kind != def.KindResourceAction && op.Kind != def.KindProviderAction {
				return nil, fmt.Errorf("op %q: a key=value input cannot be sent as a query", op.ID)
			}
		}
		if op.CLI.Positional == "" && len(g.ops) == 1 {
			if f := byFlag[variadicField]; f != nil && f.kind == kindStrings {
				b.variadic = true
				b.required = slices.Contains(s.Required, variadicField)
			}
		}
	}
	slices.SortFunc(b.fields, func(x, y *inputField) int { return strings.Compare(x.flag, y.flag) })
	return b, nil
}

// fieldOf binds one input property: its flag is the kebab-case JSON name,
// except a string map, which is --param k=v when named parameters.
func fieldOf(name string, s *jsonschema.Schema) (*inputField, error) {
	k, err := kindOf(s)
	if err != nil {
		return nil, fmt.Errorf("input %q: %w", name, err)
	}
	f := &inputField{name: name, flag: kebab(name), kind: k, usage: s.Description}
	for _, v := range s.Enum {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("input %q: enum value %v is not a string", name, v)
		}
		f.enum = append(f.enum, str)
	}
	switch {
	case k == kindMap && name == "parameters":
		f.flag = flagParam
		f.usage += " Repeat as --param key=value."
	case k == kindMap:
		f.usage += fmt.Sprintf(" Repeat as --%s key=value.", f.flag)
	case k == kindStrings:
		f.usage += " Repeatable."
	}
	if len(f.enum) > 0 {
		f.usage += " One of: " + strings.Join(f.enum, ", ") + "."
	}
	f.usage = strings.TrimSpace(f.usage)
	return f, nil
}

// kindOf is the flag kind of a property; null in a type list is ignored,
// as a nullable value is simply left unset.
func kindOf(s *jsonschema.Schema) (fieldKind, error) {
	t := s.Type
	if t == "" {
		for _, x := range s.Types {
			if x != "null" {
				if t != "" {
					return 0, fmt.Errorf("several types %v", s.Types)
				}
				t = x
			}
		}
	}
	switch t {
	case "string":
		return kindString, nil
	case "integer":
		return kindInt, nil
	case "boolean":
		return kindBool, nil
	case "array":
		if s.Items != nil {
			if k, err := kindOf(s.Items); err == nil && k == kindString {
				return kindStrings, nil
			}
		}
		return 0, fmt.Errorf("only string arrays are supported")
	case "object":
		if v := s.AdditionalProperties; v != nil {
			if k, err := kindOf(v); err == nil && k == kindString {
				return kindMap, nil
			}
		}
		return 0, fmt.Errorf("only string maps are supported")
	}
	return 0, fmt.Errorf("unsupported type %q", t)
}

// kebab turns a JSON name into a flag name: whatIf becomes what-if.
func kebab(name string) string {
	var sb strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			if i > 0 {
				sb.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// register declares the flags on cmd.
func (b *binding) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	for _, f := range b.fields {
		switch f.kind {
		case kindString:
			if len(f.enum) > 0 {
				fs.Var(&enumValue{allowed: f.enum}, f.flag, f.usage)
				_ = cmd.RegisterFlagCompletionFunc(f.flag, cobra.FixedCompletions(f.enum, cobra.ShellCompDirectiveNoFileComp))
			} else {
				fs.String(f.flag, "", f.usage)
			}
		case kindInt:
			fs.Int(f.flag, 0, f.usage)
		case kindBool:
			fs.Bool(f.flag, false, f.usage)
		case kindStrings, kindMap:
			fs.StringArray(f.flag, nil, f.usage)
		}
	}
	if b.mutating {
		fs.String(flagIdempotencyKey, "", fmt.Sprintf("Idempotency-Key header: a retry with the same key and input returns the original operation (at most %d visible ASCII characters)", openapi.IdempotencyKeyMaxLength))
	}
}

// input assembles op's input from the flags set, the arguments and the
// server-side filters, and validates it against the input schema, so a bad
// value is a usage error and nothing is sent.
func (b *binding) input(fs *pflag.FlagSet, op def.Op, args []string, filters []string) (map[string]any, error) {
	in := map[string]any{}
	own := b.opFields[op.ID]
	for _, f := range b.fields {
		fl := fs.Lookup(f.flag)
		if fl == nil || !fl.Changed {
			continue
		}
		if !own[f.name] {
			if op.CLI.Positional != "" {
				return nil, usagef("--%s cannot be used with %s", f.flag, strings.ToUpper(op.CLI.Positional))
			}
			return nil, usagef("--%s applies only with %s", f.flag, strings.ToUpper(b.positional))
		}
		var (
			v   any
			err error
		)
		switch f.kind {
		case kindString:
			v = fl.Value.String()
		case kindInt:
			v, err = fs.GetInt(f.flag)
		case kindBool:
			v, err = fs.GetBool(f.flag)
		case kindStrings:
			v, err = fs.GetStringArray(f.flag)
		case kindMap:
			var pairs []string
			if pairs, err = fs.GetStringArray(f.flag); err == nil {
				v, err = keyValues(f.flag, pairs)
			}
		}
		if err != nil {
			return nil, err
		}
		in[f.name] = v
	}
	if p := op.CLI.Positional; p != "" && len(args) > 0 {
		in[p] = args[0]
	}
	if b.variadic && len(args) > 0 {
		prev, _ := in[variadicField].([]string)
		in[variadicField] = append(prev, args...)
	}
	if len(filters) > 0 {
		in[filterField] = filters
	}
	if err := validateInput(op, in); err != nil {
		return nil, err
	}
	return in, nil
}

func keyValues(flag string, pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, usagef("--%s %q: want key=value", flag, p)
		}
		if _, dup := out[k]; dup {
			return nil, usagef("--%s: key %q given more than once", flag, k)
		}
		out[k] = v
	}
	return out, nil
}

// validateInput checks in against op's input schema, as the server will.
func validateInput(op def.Op, in map[string]any) error {
	resolved, err := op.InputSchema()
	if err != nil {
		return err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode input: %w", err)
	}
	var inst any
	if err := json.Unmarshal(data, &inst); err != nil {
		return fmt.Errorf("decode input: %w", err)
	}
	if err := resolved.Validate(inst); err != nil {
		return usagef("invalid input for %s %s: %v", op.CLI.Noun, op.CLI.Verb, err)
	}
	return nil
}

// idempotencyKey reads --idempotency-key: empty when unset, otherwise
// 1 to IdempotencyKeyMaxLength visible ASCII characters.
func (b *binding) idempotencyKey(fs *pflag.FlagSet) (string, error) {
	if !b.mutating {
		return "", nil
	}
	fl := fs.Lookup(flagIdempotencyKey)
	if fl == nil || !fl.Changed {
		return "", nil
	}
	k := fl.Value.String()
	if k == "" || len(k) > openapi.IdempotencyKeyMaxLength {
		return "", usagef("--%s must be 1 to %d characters", flagIdempotencyKey, openapi.IdempotencyKeyMaxLength)
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", usagef("--%s may hold only visible ASCII characters", flagIdempotencyKey)
		}
	}
	return k, nil
}

// enumValue is a string flag restricted to the schema's enum.
type enumValue struct {
	v       string
	allowed []string
}

func (e *enumValue) String() string { return e.v }
func (e *enumValue) Type() string   { return "string" }

func (e *enumValue) Set(s string) error {
	if !slices.Contains(e.allowed, s) {
		return fmt.Errorf("must be one of %s", strings.Join(e.allowed, ", "))
	}
	e.v = s
	return nil
}
