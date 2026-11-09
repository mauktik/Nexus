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
