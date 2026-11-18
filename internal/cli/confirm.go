package cli

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

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
// with its own name, so it is never taken from --host or --yes, and is not
// checked here beyond being non-empty. With no terminal, it is a usage error
// naming the flag: exit 2, nothing sent.
func (a *app) typedConfirmation(cmd *cobra.Command, op def.Op, hosts []string, in map[string]any) error {
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
	if _, err := fmt.Fprintf(tty, "Type the host name %s to confirm: ", host); err != nil {
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
