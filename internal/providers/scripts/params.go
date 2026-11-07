package scripts

import (
	"cmp"
	"slices"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
)

// arguments checks values against the script's declarations (FR-19, FR-25)
// and maps them as the Configuration section states: an arg parameter
// becomes two arguments, its arg then the value; an env parameter becomes
// one environment variable; a positional parameter, one with neither,
// becomes one argument in declared order. Options come first, in declared
// order, then the positionals. It returns the arguments that follow the
// script's path and the variables the env parameters set.
//
// A value reaches the script only as one whole argument or variable, never
// through a shell, so it is inert data whatever it holds. Options and env
// parameters may be left out; a positional parameter is required, since
// leaving one out would shift every later one into its place.
//
// Every failure is reported, as validation-failed at /parameters/<name>,
// sorted by path. A message never quotes the value, which may be sensitive.
func (s Script) arguments(values map[string]string) (argv []string, env map[string]string, err error) {
	var errs []problem.FieldError
	fail := func(name, msg string) {
		errs = append(errs, problem.FieldError{Path: "/parameters/" + name, Message: msg})
	}
	declared := make(map[string]bool, len(s.Parameters))
	for _, p := range s.Parameters {
		declared[p.Name] = true
	}
	for name := range values {
		if !declared[name] {
			fail(name, "is not a declared parameter of this script")
		}
	}

	var options, positional []string
	env = map[string]string{}
	for _, p := range s.Parameters {
		v, ok := values[p.Name]
		if !ok {
			if p.Position > 0 {
				fail(p.Name, "required: a positional parameter must be given")
			}
			continue
		}
		var enum []string
		if len(p.Enum) > 0 {
			enum = p.Enum
		}
		if msg := config.CheckParameterValue(p.Type, p.re, enum, v); msg != "" {
			fail(p.Name, msg)
			continue
		}
		switch {
		case p.Env != "":
			env[p.Env] = v
		case p.Arg != "":
			options = append(options, p.Arg, v)
		default:
			positional = append(positional, v)
		}
	}
	if len(errs) > 0 {
		slices.SortFunc(errs, func(a, b problem.FieldError) int { return cmp.Compare(a.Path, b.Path) })
		return nil, nil, problem.New(problem.ValidationFailed, "the input is invalid").With("errors", errs)
	}
	// Positionals are appended in declared order, which is Position order.
	return append(options, positional...), env, nil
}
