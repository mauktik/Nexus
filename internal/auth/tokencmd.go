// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// TokenCommand runs `<prog> token <create|list|revoke> ...` and returns the
// exit code: 0 on success, 1 on failure, 2 on a usage error. It is the
// bootstrap and recovery path: it works on the state directory directly, with
// no running daemon and no existing token. The nexusd provider's token ops,
// gated by nexusd:admin, also create, list and revoke tokens. A created token
// is printed once, alone on stdout, and nothing else ever prints a secret or
// a hash.
func TokenCommand(prog string, args []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintf(stderr, "Usage:\n"+
			"  %[1]s token create --name NAME [--scope SCOPE]... [--expires DURATION] [--state-dir DIR]\n"+
			"  %[1]s token list [--state-dir DIR]\n"+
			"  %[1]s token revoke ID|NAME [--state-dir DIR]\n", prog)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "create":
		return tokenCreate(prog, args[1:], stdout, stderr)
	case "list":
		return tokenList(prog, args[1:], stdout, stderr)
	case "revoke":
		return tokenRevoke(prog, args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	}
	fmt.Fprintf(stderr, "%s token: unknown subcommand %q\n", prog, args[0])
	usage()
	return 2
}

type scopeList []string

func (s *scopeList) String() string { return strings.Join(*s, ",") }

func (s *scopeList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parse parses fs and allows the flags and up to maxArgs positional
// arguments in any order, so `revoke ID --state-dir D` works.
func parse(fs *flag.FlagSet, args []string, maxArgs int) ([]string, int) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0
			}
			return nil, 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) > maxArgs {
		fmt.Fprintf(fs.Output(), "%s: unexpected argument %q\n", fs.Name(), pos[maxArgs])
		return nil, 2
	}
	return pos, -1
}

func newFlags(prog, sub string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(prog+" token "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("state-dir", DefaultStateDir, "state directory holding "+TokenFileName)
	return fs, dir
}

func tokenCreate(prog string, args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags(prog, "create", stderr)
	name := fs.String("name", "", "token name (required, unique)")
	var scopes scopeList
	fs.Var(&scopes, "scope", "scope to grant; repeat for more. A token has no scopes unless given")
	expires := fs.String("expires", "", "lifetime such as 24h or 30d; empty means no expiry")
	if _, code := parse(fs, args, 0); code >= 0 {
		return code
	}
	if *name == "" {
		fmt.Fprintf(stderr, "%s: --name is required\n", fs.Name())
		return 2
	}
	ttl, err := ParseLifetime(*expires)
	if err != nil {
		fmt.Fprintf(stderr, "%s: --expires: %v\n", fs.Name(), err)
		return 2
	}
	tok, t, err := Create(*dir, CreateRequest{Name: *name, Scopes: scopes, TTL: ttl})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", fs.Name(), err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, tok); err != nil {
		fmt.Fprintf(stderr, "%s: token %s was created but could not be printed (%v); revoke it with `%s token revoke %s`\n",
			fs.Name(), t.ID, err, prog, t.ID)
		return 1
	}
	fmt.Fprintf(stderr, "Created token %q (id %s). It is shown only this once; store it now.\n", t.Name, t.ID)
	return 0
}

// ParseLifetime accepts a Go duration or a whole number of days ("30d"), up to
// 36500 days. Empty means no expiry and gives 0.
func ParseLifetime(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 36500 {
			return 0, fmt.Errorf("%q is not a positive number of days", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration such as 24h or 30d", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be positive", s)
	}
	return d, nil
}

func tokenList(prog string, args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags(prog, "list", stderr)
	if _, code := parse(fs, args, 0); code >= 0 {
		return code
	}
	toks, err := List(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", fs.Name(), err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPES\tCREATED\tEXPIRES")
	for _, t := range toks {
		scopes := strings.Join(t.Scopes, ",")
		if scopes == "" {
			scopes = "-"
		}
		exp := "never"
		if t.Expires != nil {
			exp = t.Expires.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, scopes, t.Created.Format(time.RFC3339), exp)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", fs.Name(), err)
		return 1
	}
	return 0
}

func tokenRevoke(prog string, args []string, stdout, stderr io.Writer) int {
	fs, dir := newFlags(prog, "revoke", stderr)
	pos, code := parse(fs, args, 1)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "%s: give the ID or name of the token to revoke\n", fs.Name())
		return 2
	}
	t, err := Revoke(*dir, pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", fs.Name(), err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "Revoked token %q (id %s).\n", t.Name, t.ID); err != nil {
		return 1
	}
	return 0
}
