package scripts

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
)

// paramsScript declares one parameter of each mapping and kind: arg, env,
// positional, enum, regex and typed.
func paramsScript(t *testing.T) Script {
	t.Helper()
	c, err := NewCatalog([]config.Script{{
		ID: "deploy", Description: "Deploy", Path: "/opt/deploy.sh", RunAs: "deploy", WorkingDirectory: "/opt",
		Parameters: []config.Parameter{
			{Name: "branch", Type: "string", Pattern: `^[a-z][a-z0-9-]*$`, Arg: "--branch"},
			{Name: "target", Type: "string", Pattern: `^[a-z]+$`},
			{Name: "stage", Type: "string", Enum: []string{"prod", "staging"}, Arg: "--stage"},
			{Name: "apiKey", Type: "string", Pattern: `^[A-Za-z0-9]+$`, Sensitive: true, Env: "API_KEY"},
			{Name: "count", Type: "integer", Arg: "-n"},
			{Name: "region", Type: "string", Pattern: `^[a-z]+-[0-9]$`},
			{Name: "dry", Type: "boolean", Env: "DRY_RUN"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.Get("deploy")
	if !ok {
		t.Fatal("deploy not in the catalog")
	}
	return s
}

// TC-94 step 1: valid input gives the exact argv, options in declared order
// then positionals in declared order, and env-mapped values never reach
// argv.
func TestParameterArgvMapping(t *testing.T) {
	s := paramsScript(t)
	values := map[string]string{
		"branch": "main", "target": "api", "stage": "prod", "apiKey": "s3cr3tKey",
		"count": "-3", "region": "eu-1", "dry": "true",
	}
	argv, env, err := s.arguments(values)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--branch", "main", "--stage", "prod", "-n", "-3", "api", "eu-1"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if wantEnv := map[string]string{"API_KEY": "s3cr3tKey", "DRY_RUN": "true"}; !maps.Equal(env, wantEnv) {
		t.Errorf("env %v, want %v", env, wantEnv)
	}
	for _, a := range argv {
		if a == "s3cr3tKey" || a == "true" {
			t.Errorf("env-mapped value %q reached argv %q", a, argv)
		}
	}

	// Optional options and env parameters may be left out; nothing takes
	// their place.
	argv, env, err = s.arguments(map[string]string{"target": "api", "region": "eu-1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "eu-1"}; !slices.Equal(argv, want) || len(env) != 0 {
		t.Errorf("argv %q env %v, want %q and no env", argv, env, want)
	}
}

// TC-94 step 2: an undeclared parameter, a pattern miss, an enum miss and
// a wrong type are each validation-failed naming the parameter, and the
// message never echoes the value.
func TestParameterValidationFailures(t *testing.T) {
	s := paramsScript(t)
	base := map[string]string{"target": "api", "region": "eu-1"}
	cases := []struct {
		name, param, value, msg string
	}{
		{"undeclared", "command", "id", "not a declared parameter"},
		{"pattern miss", "branch", "; rm -rf /", "pattern"},
		{"pattern miss on a leading dash", "target", "--force", "pattern"},
		{"enum miss", "stage", "dev", "declared values"},
		{"wrong type integer", "count", "three", "integer"},
		{"integer overflow", "count", "99999999999999999999", "64 bits"},
		{"wrong type boolean", "dry", "yes", "true or false"},
		{"NUL byte", "branch", "main\x00x", "NUL"},
		{"sensitive pattern miss", "apiKey", "not secret!", "pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := maps.Clone(base)
			values[tc.param] = tc.value
			argv, env, err := s.arguments(values)
			if argv != nil || env != nil {
				t.Errorf("argv %q env %v returned with an error", argv, env)
			}
			fe := fieldErrors(t, err)
			if len(fe) != 1 || fe[0].Path != "/parameters/"+tc.param || !strings.Contains(fe[0].Message, tc.msg) {
				t.Fatalf("errors %+v, want one at /parameters/%s containing %q", fe, tc.param, tc.msg)
			}
			if strings.Contains(fe[0].Message, tc.value) {
				t.Errorf("message %q echoes the value", fe[0].Message)
			}
		})
	}

	t.Run("missing positional", func(t *testing.T) {
		_, _, err := s.arguments(map[string]string{"target": "api"})
		fe := fieldErrors(t, err)
		if len(fe) != 1 || fe[0].Path != "/parameters/region" || !strings.Contains(fe[0].Message, "required") {
			t.Fatalf("errors %+v, want region required", fe)
		}
	})

	t.Run("every failure is reported, sorted by path", func(t *testing.T) {
		_, _, err := s.arguments(map[string]string{"zzz": "1", "count": "x", "target": "API", "region": "eu-1"})
		fe := fieldErrors(t, err)
		var paths []string
		for _, e := range fe {
			paths = append(paths, e.Path)
		}
		if want := []string{"/parameters/count", "/parameters/target", "/parameters/zzz"}; !slices.Equal(paths, want) {
			t.Fatalf("paths %q, want %q", paths, want)
		}
	})
}

func fieldErrors(t *testing.T, err error) []problem.FieldError {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
		t.Fatalf("error %v, want validation-failed", err)
	}
	fe, ok := p.Extensions["errors"].([]problem.FieldError)
	if !ok {
		t.Fatalf("errors member %T", p.Extensions["errors"])
	}
	return fe
}

// SEC-3: a pattern decides the whole value. Go's MatchString succeeds on any
// substring, so an unanchored [A-Za-z0-9._/-]+ would admit a value that
// starts with a dash and holds a shell character.
func TestParameterPatternsMatchTheWholeValue(t *testing.T) {
	c, err := NewCatalog([]config.Script{{
		ID: "deploy", Description: "Deploy", Path: "/opt/deploy.sh", RunAs: "deploy", WorkingDirectory: "/opt",
		Parameters: []config.Parameter{
			{Name: "ref", Type: "string", Pattern: `[A-Za-z0-9._/-]+`},
			{Name: "branch", Type: "string", Pattern: `main|develop`, Arg: "--branch"},
			{Name: "anchored", Type: "string", Pattern: `^[a-z]+$`, Env: "X"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.Get("deploy")
	if _, _, err := s.arguments(map[string]string{"ref": "v1.2/rc-1", "branch": "main", "anchored": "abc"}); err != nil {
		t.Fatalf("valid values refused: %v", err)
	}
	for _, v := range []map[string]string{
		{"ref": "--upload-pack=touch /tmp/pwn", "branch": "main", "anchored": "abc"},
		{"ref": "x; anything", "branch": "main", "anchored": "abc"},
		{"ref": "ok", "branch": "main; id", "anchored": "abc"},
		{"ref": "ok", "branch": "xdevelop", "anchored": "abc"},
		{"ref": "ok", "branch": "main", "anchored": "abc\n"},
	} {
		_, _, err := s.arguments(v)
		var p *problem.Error
		if !errors.As(err, &p) || p.Slug() != problem.ValidationFailed {
			t.Errorf("%v: got %v, want validation-failed", v, err)
		}
	}
}
