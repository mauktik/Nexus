// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
)

// flagWhatIf is the flag of the whatIf input of an op with Op.WhatIf.
const flagWhatIf = "what-if"

// registerWhatIfFlag declares a hidden --what-if on a mutating command whose
// ops have no whatIf input, so using it is a clear usage error rather than
// an unknown flag (FR-33). Ops with Op.WhatIf get the flag from their input.
func registerWhatIfFlag(cmd *cobra.Command, b *binding) {
	if !b.mutating || cmd.Flags().Lookup(flagWhatIf) != nil {
		return
	}
	cmd.Flags().Bool(flagWhatIf, false, "not supported: this command has no preview")
	_ = cmd.Flags().MarkHidden(flagWhatIf)
}

// checkWhatIf rejects --what-if where op does not declare Op.WhatIf, before
// anything is sent.
func checkWhatIf(cmd *cobra.Command, op def.Op) error {
	if flagSet(cmd, flagWhatIf) && !op.WhatIf {
		return usagef("--%s is not supported by %s %s %s: it has no preview; nothing was sent",
			flagWhatIf, programName, op.CLI.Noun, op.CLI.Verb)
	}
	return nil
}

// needsConfirmation reports whether running op asks first (FR-33): a
// destructive op (A-12: including script invoke and package update), or a
// mutation of more than one host. --what-if changes nothing, so it never
// asks.
func needsConfirmation(cmd *cobra.Command, op def.Op, hosts []string) bool {
	if !op.Mutating() || flagSet(cmd, flagWhatIf) {
		return false
	}
	return op.Risk == def.RiskDestructive || len(hosts) > 1
}

// confirmTargets decides confirmation once every target is known: for
// --stdin without --yes, after reading to EOF (A-55). hosts are the
// distinct hosts the command will send to, objects how many requests it
// will send (0 for one per host), and name the resource when there is one.
//
// It asks on the controlling terminal, /dev/tty, as sudo and ssh do, so it
// works when stdin is a pipe. With no controlling terminal (cron, an agent,
// CI) it needs --yes, and otherwise is a usage error: exit 2, nothing sent.
// Any answer but y or yes declines: exit 1, nothing sent.
func (a *app) confirmTargets(cmd *cobra.Command, op def.Op, hosts []string, objects int, name string) error {
	if !needsConfirmation(cmd, op, hosts) || flagSet(cmd, flagYes) {
		return nil
	}
	what := confirmQuestion(op, hosts, objects, name)
	tty, err := a.openTerminal()
	if err != nil {
		return usagef("%s needs confirmation and there is no terminal to ask on: pass --%s to proceed; nothing was sent",
			lowerFirst(what), flagYes)
	}
	defer tty.Close()
	if _, err := fmt.Fprintf(tty, "%s? [y/N] ", what); err != nil {
		return usagef("%s needs confirmation and the terminal cannot be written: pass --%s to proceed; nothing was sent",
			lowerFirst(what), flagYes)
	}
	answer, err := tty.ReadLine()
	if err == nil {
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		}
	} else {
		fmt.Fprintln(tty)
	}
	return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%s: not confirmed; nothing was sent", lowerFirst(what))}
}

// typedConfirmed reports whether an op of g has CLI.TypedConfirmation.
func typedConfirmed(g *opCommand) bool {
	return slices.ContainsFunc(g.ops, func(op def.Op) bool { return op.CLI.TypedConfirmation != "" })
}

// relaxTypedConfirmation drops the CLI.TypedConfirmation field of each of
// g's ops from the required fields b checks, so a command without its flag
// passes the input check and asks for it once every target is known
// (typedConfirmation). The server still requires it.
func relaxTypedConfirmation(g *opCommand, b *binding) error {
	for _, op := range g.ops {
		field := op.CLI.TypedConfirmation
		if field == "" {
			continue
		}
		base, ok := b.schemas[op.ID]
		if !ok {
			var err error
			if base, err = op.InputSchema(); err != nil {
				return err
			}
		}
		r, err := withoutRequired(base.Schema(), field)
		if err != nil {
			return fmt.Errorf("op %q: %w", op.ID, err)
		}
		b.schemas[op.ID] = r
	}
	return nil
}

// typedConfirmation is the confirmation of an op with CLI.TypedConfirmation
// (D-8), which replaces the y/N question: the operator types the target
// host's name into the field. The command targets exactly one host. A set
// flag is the confirmation; otherwise the CLI asks on the controlling
// terminal and puts the trimmed answer in in. The server compares the value
// with its own name, as its capabilities report it, which need not be the
// name of the host's entry in the client configuration. So the CLI fetches
// the capabilities and asks for that name; if it cannot, it fails without
// asking, since the entry name may be the wrong one to ask for. A name that
// is not plain printable text, or that the server would not accept (the
// pattern and length of the op's field), is refused too, and is never
// written to the terminal raw (SEC-3). The answer
// is never taken from --host or --yes, and is not checked here beyond being
// non-empty. With no terminal, it is a usage error naming the flag: exit 2,
// nothing sent.
func (a *app) typedConfirmation(cmd *cobra.Command, op def.Op, targets []hostTarget, in map[string]any) error {
	hosts := make([]string, len(targets))
	for i, t := range targets {
		hosts[i] = t.Name
	}
	field := op.CLI.TypedConfirmation
	flag := kebab(field)
	command := programName + " " + op.CLI.Noun + " " + op.CLI.Verb
	if len(hosts) != 1 {
		return usagef("%s targets exactly one host, and %d were selected (%s): name one with --host; nothing was sent",
			command, len(hosts), strings.Join(hosts, ", "))
	}
	if flagChanged(cmd, flag) {
		if v, _ := in[field].(string); v == "" {
			return usagef("--%s is empty: give the name of host %s; nothing was sent", flag, hosts[0])
		}
		return nil
	}
	host := hosts[0]
	tty, err := a.openTerminal()
	if err != nil {
		return usagef("%s on %s needs the host name typed to confirm and there is no terminal to ask on: pass --%s <host name>; nothing was sent",
			command, host, flag)
	}
	defer tty.Close()
	own, err := a.hostOwnName(cmd.Context(), targets[0])
	if err != nil {
		return fmt.Errorf("%s on %s: cannot ask for the host name to type, as its capabilities are unavailable: %w; nothing was sent",
			command, host, err)
	}
	if err := typeableHostName(op, field, own); err != nil {
		return fmt.Errorf("%s on %s: %w; nothing was sent: check the host, then pass --%s <host name>", command, host, err, flag)
	}
	if _, err := fmt.Fprintf(tty, "Type the host name %s to confirm: ", own); err != nil {
		return usagef("%s on %s needs the host name typed to confirm and the terminal cannot be written: pass --%s <host name>; nothing was sent",
			command, host, flag)
	}
	answer, err := tty.ReadLine()
	if err != nil {
		fmt.Fprintln(tty)
		return &ExitError{Code: ExitFailure, Err: fmt.Errorf("%s on %s: not confirmed; nothing was sent", command, host)}
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return usagef("%s on %s: no host name was typed to confirm; nothing was sent", command, host)
	}
	in[field] = answer
	return nil
}

// typeableHostName refuses name, the host's own name as its server reports
// it, unless it is safe to write on the terminal and to ask for: valid text
// of printable characters only (no control, format or separator characters,
// which can rewrite the prompt line or carry terminal commands), and
// matching the pattern and length the op's input schema gives field, which
// is what the server will check the answer against.
func typeableHostName(op def.Op, field, name string) error {
	bad := func(why string) error {
		return fmt.Errorf("the host reports a name that cannot be typed to confirm, %s (%s)", why, quotedPrefix(name, 80))
	}
	if !utf8.ValidString(name) {
		return bad("it is not valid text")
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return bad("it has a control, format or separator character")
		}
	}
	res, err := op.InputSchema()
	if err != nil {
		return fmt.Errorf("the input schema of %s cannot be read to check the host's name: %w", op.ID, err)
	}
	prop := res.Schema().Properties[field]
	if prop == nil {
		return nil
	}
	if prop.MaxLength != nil && utf8.RuneCountInString(name) > *prop.MaxLength {
		return bad(fmt.Sprintf("it is over %d characters", *prop.MaxLength))
	}
	if prop.Pattern != "" {
		re, err := regexp.Compile(prop.Pattern)
		if err != nil {
			return fmt.Errorf("the pattern of %s cannot be used to check the host's name: %w", field, err)
		}
		if !re.MatchString(name) {
			return bad("it does not match what the server accepts")
		}
	}
	return nil
}

// quotedPrefix is s as a Go-quoted string of at most n characters of it, so
// nothing in it is interpreted by a terminal.
func quotedPrefix(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strconv.Quote(string(r[:n])) + "..."
	}
	return strconv.Quote(s)
}

// hostOwnName is t's own name, as the capabilities of its nexusd report it.
func (a *app) hostOwnName(ctx context.Context, t hostTarget) (string, error) {
	c, err := a.targetClient(t)
	if err != nil {
		return "", err
	}
	var caps apitypes.Capabilities
	if err := c.Do(ctx, http.MethodGet, "/capabilities", nil, &caps); err != nil {
		return "", err
	}
	if caps.Host == "" {
		return "", errors.New("the capabilities name no host")
	}
	return caps.Host, nil
}

// confirmQuestion phrases what is about to happen, such as "Update 3
// containers on 2 hosts (alpha, beta)".
func confirmQuestion(op def.Op, hosts []string, objects int, name string) string {
	verb := upperFirst(op.CLI.Verb)
	noun := op.CLI.Noun
	var subject string
	switch {
	case name != "":
		subject = noun + " " + name
	case objects == 1:
		subject = "1 " + noun
	case objects > 1:
		subject = fmt.Sprintf("%d %ss", objects, noun)
	default:
		subject = noun + "s"
	}
	on := "on " + strings.Join(hosts, "")
	if len(hosts) > 1 {
		on = fmt.Sprintf("on %d hosts (%s)", len(hosts), strings.Join(slices.Sorted(slices.Values(hosts)), ", "))
	}
	return verb + " " + subject + " " + on
}

func upperFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}
