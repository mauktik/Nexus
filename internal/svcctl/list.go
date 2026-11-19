package svcctl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/mauktik/nexus/internal/platform"
)

// rcUpdatePath is the absolute path of rc-update.
const rcUpdatePath = "/sbin/rc-update"

// ListOutputCap is the cap on each listing's output (DF-1). Output that
// reaches it is an error, never a partial list.
const ListOutputCap = 4 << 20

// maxScriptBytes bounds the read of an init script for its description.
const maxScriptBytes = 4 << 10

// Entry is one service as List reports it (DF-1).
type Entry struct {
	Name string // Without .service; a template keeps its "@" (getty@)
	// State is one of the State words; RawState is the init system's own:
	// "<ACTIVE> (<SUB>)" on systemd, the rc-status word on OpenRC.
	State, RawState string
	// Enabled is one of enabled, disabled, static, masked, indirect,
	// generated, alias or unknown; EnabledRaw is the init system's word.
	Enabled, EnabledRaw string
	Description         string
	DescriptionKnown    bool
	Runlevels           []string // OpenRC: the runlevels that include it
	// UnitFileOnly is true for an entry seen only in the boot configuration
	// listing (systemd list-unit-files, OpenRC rc-update): a template, or a
	// service that is not loaded. Its State is stopped.
	UnitFileOnly bool
}

// The argv of List.
var (
	listUnitsArgv     = []string{systemctlPath, "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager", "--full"}
	listUnitFilesArgv = []string{systemctlPath, "list-unit-files", "--type=service", "--plain", "--no-legend", "--no-pager", "--full"}
	rcStatusArgv      = []string{rcStatusPath, "-s", "-C"}
	rcUpdateArgv      = []string{rcUpdatePath, "show", "-v"}
)

// enabledWords are the systemd unit-file states mapped as themselves; any
// other word is unknown (FR-4).
var enabledWords = []string{"enabled", "disabled", "static", "masked", "indirect", "generated", "alias"}

// mapEnabled is word when it is exactly one of enabledWords, else unknown.
func mapEnabled(word string) string {
	if slices.Contains(enabledWords, word) {
		return word
	}
	return "unknown"
}

// mapActiveState maps a systemd ActiveState word exactly, as Status does.
func mapActiveState(word string) State {
	switch word {
	case "active", "reloading":
		return Running
	case "activating":
		return Starting
	case "deactivating":
		return Stopping
	case "inactive":
		return Stopped
	case "failed":
		return Failed
	}
	return Unknown
}

// rcRawState is the word as RawState: one of OpenRC's own state words, or
// "unknown". A custom status() prints what it likes after "status:", and
// that token reaches callers holding services:read, so it is never passed on
// (SEC-8). The words are those of _status in openrc-run.sh (stopping,
// starting, inactive, crashed, started, stopped) and rc-status (hotplugged,
// scheduled, failed).
func rcRawState(word string) string {
	switch word {
	case "started", "stopped", "crashed", "starting", "stopping", "inactive", "failed", "hotplugged", "scheduled":
		return word
	}
	return string(Unknown)
}

// mapRCWord maps an OpenRC status word exactly, as Status does.
func mapRCWord(word string) State {
	switch word {
	case "started":
		return Running
	case "stopped":
		return Stopped
	case "crashed":
		return Failed
	case "starting":
		return Starting
	case "stopping":
		return Stopping
	}
	return Unknown
}

// List lists every service the init system knows (DF-1). On systemd it runs
// list-units and list-unit-files concurrently; on OpenRC rc-status -s -C and
// rc-update show -v, reading each script's static description under
// EtcRoot. Each listing is capped at opts.OutputCap (0 = ListOutputCap);
// output that reaches the cap, a non-zero exit, or a failed or timed-out
// call is an error, with a nil list. The entries are sorted by name.
func List(ctx context.Context, init platform.InitSystem, opts Options) ([]Entry, error) {
	if opts.OutputCap <= 0 {
		opts.OutputCap = ListOutputCap
	}
	b := newBase(opts)
	var first, second []string
	switch init {
	case platform.Systemd:
		first, second = listUnitsArgv, listUnitFilesArgv
	case platform.OpenRC:
		first, second = rcStatusArgv, rcUpdateArgv
	default:
		return nil, fmt.Errorf("%w: no service listing for init system %q", ErrNotStarted, string(init))
	}
	outs, err := b.listings(ctx, first, second)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if init == platform.Systemd {
		entries = mergeSystemd(parseListUnits(outs[0]), parseListUnitFiles(outs[1]))
	} else {
		entries = mergeOpenRC(parseRCStatus(outs[0]), parseRCUpdate(outs[1]), b.etcRoot)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

// listings runs the argvs concurrently and returns their outputs. The
// first failure is the error and cancels the other calls; every call has
// returned before listings does.
func (b base) listings(ctx context.Context, argvs ...[]string) ([]string, error) {
	g, ctx := errgroup.WithContext(ctx)
	outs := make([]string, len(argvs))
	for i, argv := range argvs {
		g.Go(func() error {
			out, err := b.listing(ctx, argv)
			outs[i] = out
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return outs, nil
}

// listing runs one listing argv under the call rules and the output cap.
func (b base) listing(ctx context.Context, argv []string) (string, error) {
	res, err := b.call(ctx, argv)
	if err != nil {
		return "", err
	}
	name := strings.Join(argv[:2], " ")
	if res.Truncated || len(res.Output) > b.outputCap {
		return "", fmt.Errorf("svcctl: %s output reached the %d MiB cap; refusing a partial list", name, b.outputCap>>20)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("svcctl: %s exited %d: %s", name, res.ExitCode, firstLine(res.Output))
	}
	return string(res.Output), nil
}

// firstLine is the first non-empty line of out, trimmed.
func firstLine(out []byte) string {
	for line := range strings.Lines(string(out)) {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// unitLine is one list-units line.
type unitLine struct {
	name, load, active, sub, description string
	hasDescription                       bool
}

// serviceUnit is NAME of a "NAME.service" first column, when it is a unit
// name systemd could print (unitNamePattern, at most unitNameMax).
func serviceUnit(field string) (string, bool) {
	if len(field) > unitNameMax || !unitName.MatchString(field) {
		return "", false
	}
	name, ok := strings.CutSuffix(field, ".service")
	return name, ok && name != ""
}

// cutField splits s at its first run of spaces or tabs, after leading ones.
func cutField(s string) (field, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// parseListUnits parses `systemctl list-units --plain --no-legend`: UNIT
// LOAD ACTIVE SUB DESCRIPTION, the description being the rest of the line
// verbatim. Lines that do not start with a .service unit and three words
// (warnings, other units) are dropped; template names never come from here.
func parseListUnits(out string) []unitLine {
	var units []unitLine
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		var f [4]string
		rest := line
		for i := range f {
			f[i], rest = cutField(rest)
		}
		name, ok := serviceUnit(f[0])
		if !ok || f[3] == "" || strings.HasSuffix(name, "@") {
			continue
		}
		desc := strings.TrimRight(strings.TrimLeft(rest, " \t"), " \t")
		units = append(units, unitLine{name: name, load: f[1], active: f[2], sub: f[3], description: desc,
			hasDescription: desc != ""})
	}
	return units
}

// unitFile is one list-unit-files line.
type unitFile struct{ name, state string }

// parseListUnitFiles parses `systemctl list-unit-files --plain --no-legend`:
// UNIT FILE STATE, with or without a PRESET column after it.
func parseListUnitFiles(out string) []unitFile {
	var files []unitFile
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if name, ok := serviceUnit(f[0]); ok {
			files = append(files, unitFile{name: name, state: f[1]})
		}
	}
	return files
}

// mergeSystemd merges the two listings by name (DF-1 step 3). An entry
// only list-unit-files has is stopped, inactive (dead), except an alias,
// whose state is unknown. A loaded instance with no unit-file line of its own takes its template's: systemd
// resolves an instance's unit file through the template. A unit with no
// unit file at all has no enabled word.
func mergeSystemd(units []unitLine, files []unitFile) []Entry {
	states := map[string]string{}
	for _, f := range files {
		if _, dup := states[f.name]; !dup {
			states[f.name] = f.state
		}
	}
	seen := map[string]bool{}
	var entries []Entry
	for _, u := range units {
		if seen[u.name] {
			continue
		}
		seen[u.name] = true
		e := Entry{Name: u.name, State: string(mapActiveState(u.active)), RawState: u.active + " (" + u.sub + ")",
			Description: u.description, DescriptionKnown: u.hasDescription, Enabled: "unknown"}
		state, ok := states[u.name]
		if tmpl, inst, isInst := strings.Cut(u.name, "@"); !ok && isInst && inst != "" {
			state, ok = states[tmpl+"@"]
		}
		if ok {
			e.Enabled, e.EnabledRaw = mapEnabled(state), state
		}
		entries = append(entries, e)
	}
	for _, f := range files {
		if seen[f.name] {
			continue
		}
		seen[f.name] = true
		e := Entry{Name: f.name, State: string(Stopped), RawState: "inactive (dead)", Enabled: mapEnabled(f.state),
			EnabledRaw: f.state, UnitFileOnly: true}
		if f.state == "alias" && !strings.HasSuffix(f.name, "@") {
			// systemctl show and is-active of an alias report its target,
			// which no listing names (procps is systemd-sysctl, active),
			// so its state is not known.
			e.State, e.RawState = string(Unknown), ""
		}
		// Otherwise what systemctl show reports for a unit that is not
		// loaded (ActiveState=inactive, SubState=dead).
		entries = append(entries, e)
	}
	return entries
}

// rcStatusLine is one `rc-status -s -C` line.
type rcStatusLine struct{ name, word string }

// parseRCStatus parses `rc-status -s -C`: " NAME   [  WORD ... ]". The word
// is the first inside the brackets ("started 00:00:05 (0)" is started).
func parseRCStatus(out string) []rcStatusLine {
	var lines []rcStatusLine
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		open, close := strings.IndexByte(line, '['), strings.LastIndexByte(line, ']')
		if open <= 0 || close != len(line)-1 || close < open {
			continue
		}
		name := strings.TrimSpace(line[:open])
		words := strings.Fields(line[open+1 : close])
		if name == "" || strings.ContainsAny(name, " \t") || len(words) == 0 {
			continue
		}
		lines = append(lines, rcStatusLine{name: name, word: words[0]})
	}
	return lines
}

// rcUpdateLine is one `rc-update show -v` line.
type rcUpdateLine struct {
	name      string
	runlevels []string
}

// parseRCUpdate parses `rc-update show -v`: "NAME | RUNLEVEL ...", with an
// empty right side for a script in no runlevel.
func parseRCUpdate(out string) []rcUpdateLine {
	var lines []rcUpdateLine
	for line := range strings.Lines(out) {
		l, r, ok := strings.Cut(line, "|")
		name := strings.TrimSpace(l)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		lines = append(lines, rcUpdateLine{name: name, runlevels: strings.Fields(r)})
	}
	return lines
}

// runlevelsOf is name's runlevels in an rc-update listing, and whether it
// is listed.
func runlevelsOf(lines []rcUpdateLine, name string) ([]string, bool) {
	for _, l := range lines {
		if l.name == name {
			return l.runlevels, true
		}
	}
	return nil, false
}

// openrcEnabled is enabled when the script is in any runlevel.
func openrcEnabled(runlevels []string) string {
	if len(runlevels) > 0 {
		return "enabled"
	}
	return "disabled"
}

// mergeOpenRC merges the two listings by name. A script only rc-update
// lists (rc-status -s leaves out the scripts its keywords exclude, such as
// -docker) is reported as rc-service reports it: stopped.
func mergeOpenRC(status []rcStatusLine, update []rcUpdateLine, etcRoot string) []Entry {
	seen := map[string]bool{}
	var entries []Entry
	add := func(name, word string, fromStatus bool) {
		if seen[name] {
			return
		}
		seen[name] = true
		runlevels, _ := runlevelsOf(update, name)
		e := Entry{Name: name, State: string(mapRCWord(word)), RawState: rcRawState(word), Runlevels: runlevels,
			Enabled: openrcEnabled(runlevels), EnabledRaw: strings.Join(runlevels, " "), UnitFileOnly: !fromStatus}
		e.Description, e.DescriptionKnown = staticDescription(etcRoot, name)
		entries = append(entries, e)
	}
	for _, s := range status {
		add(s.name, s.word, true)
	}
	for _, u := range update {
		add(u.name, "stopped", false)
	}
	return entries
}

// staticDescription is the literal value of the description= assignment in
// <etcRoot>/init.d/<name>, read without following a symlink or blocking,
// within maxScriptBytes. It is known only when every description= line
// assigns the same literal (literalValue); a value built from a variable,
// a command or an escape is unknown, never expanded.
func staticDescription(etcRoot, name string) (string, bool) {
	if !serviceName.MatchString(name) {
		return "", false
	}
	f, _, err := openRegular(filepath.Join(etcRoot, "init.d", name))
	if err != nil {
		return "", false
	}
	defer f.Close()
	return literalAssignment(io.LimitReader(f, maxScriptBytes), "description")
}

// literalAssignment is the value of key= in a shell script, when every
// assignment of key at the start of a line (after blanks) is the same
// literal: "...", '...' or a bare word, with no expansion, escape or
// command.
func literalAssignment(r io.Reader, key string) (string, bool) {
	var vals []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimLeft(sc.Text(), " \t"), key+"=")
		if !ok {
			continue
		}
		v, ok := literalValue(strings.TrimRight(rest, " \t\r"))
		if !ok {
			return "", false
		}
		vals = append(vals, v)
	}
	slices.Sort(vals)
	if vals = slices.Compact(vals); len(vals) != 1 {
		return "", false
	}
	return vals[0], true
}

// literalValue is the value of a shell word that expands to itself.
func literalValue(w string) (string, bool) {
	switch {
	case len(w) >= 2 && w[0] == '"' && w[len(w)-1] == '"':
		v := w[1 : len(w)-1]
		return v, !strings.ContainsAny(v, "\"$`\\")
	case len(w) >= 2 && w[0] == '\'' && w[len(w)-1] == '\'':
		v := w[1 : len(w)-1]
		return v, !strings.Contains(v, "'")
	}
	return w, w != "" && !strings.ContainsAny(w, "\"'$`\\ \t;&|<>(){}*?[]#~")
}
