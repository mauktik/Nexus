package svcctltest

// F-HOSTS: a multi-service fake of a whole host's init system, built from
// one capture directory under internal/svcctl/testdata/services (WI 3.1's
// T1 container captures, and WI 6.0's host captures once committed). It is
// an svcctl.Options.Run.
//
// Reads answer from the capture, verbatim, with the capture's exit code:
// every command the capture's PROVENANCE lists for systemctl, rc-status,
// rc-update or rc-service is served from its file. What the capture does not
// hold is modeled from it:
//   - `systemctl show` of a unit with no show capture is rendered from the
//     unit's list-units and list-unit-files lines in the property order of
//     the capture's own show files (systemd 259's order, the -p order of
//     DF-1 being ignored by systemctl); an unknown unit exits 0 with
//     LoadState=not-found, ActiveState=inactive, SubState=dead and MainPID=0,
//     as testdata/systemd/show-not-found.txt does;
//   - `systemctl is-enabled` of a unit answers its oracle/is-enabled.tsv
//     word and exit code;
//   - `rc-service -C NAME status` answers " * status: WORD" with the exit
//     code oracle/rc-service-status.tsv records for WORD (crashed: 32, from
//     testdata/openrc/status-crashed.txt), and an unknown script answers
//     testdata/openrc/absent.txt;
//   - ListFailed's argv answers oracle/failed.txt (systemd), and OpenRC's
//     `rc-status --crashed` the crashed scripts, exiting 1 with no output
//     when there are none (testdata/openrc/rc-status-crashed-none.txt).
//
// Actions model each service's state with the captured outputs of the
// capture's actions/ directory (the planted nx-svc-test and nx-svc-fail),
// with the service's name put in:
//   - systemd start of a stopped service exits 0 with no output; the unit
//     then reports ActiveState=activating for Settle polls of show, then
//     active/running with a new MainPID and a later ActiveEnterTimestamp;
//   - start of a FailsOnStart service exits 1 with actions/start-nx-svc-fail
//     and leaves ActiveState=failed, SubState=failed, Result=exit-code,
//     MainPID=0 (OpenRC: stopped, as captured);
//   - start of a running service exits 0 and changes nothing (OpenRC: the
//     captured "has already been started" warning);
//   - stop of a failed systemd unit exits 0 and it stays failed
//     (testdata/systemd/stop-failed.txt).
//
// It is strict about how it is called, like Fake: any argv that is not one
// of the shapes svcctl builds fails the test through T and returns an error
// wrapping svcctl.ErrNotStarted.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
)

// Tool paths, as svcctl calls them.
const (
	systemctl = "/usr/bin/systemctl"
	rcStatus  = "/bin/rc-status"
	rcUpdate  = "/sbin/rc-update"
	rcService = "/sbin/rc-service"
)

// The fixed argv of DF-1 and ListFailed.
var (
	listUnitsArgv     = []string{systemctl, "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager", "--full"}
	listUnitFilesArgv = []string{systemctl, "list-unit-files", "--type=service", "--plain", "--no-legend", "--no-pager", "--full"}
	listFailedArgv    = []string{systemctl, "list-units", "--state=failed", "--plain", "--no-legend", "--no-pager", "--full"}
	showProps         = "Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath"
	rcStatusArgv      = []string{rcStatus, "-s", "-C"}
	rcUpdateArgv      = []string{rcUpdate, "show", "-v"}
	rcCrashedArgv     = []string{rcStatus, "--crashed"}
)

// showOrder is the order systemd 259 prints the DF-1 properties in, used
// when the capture has no show file to take the order from.
var showOrder = []string{"Id", "Description", "LoadState", "ActiveState", "SubState", "FragmentPath", "UnitFileState",
	"ActiveEnterTimestamp", "InactiveEnterTimestamp", "MainPID", "Result", "NRestarts", "MemoryCurrent"}

var serviceName = regexp.MustCompile(svcctl.ServiceNamePattern)

// DefaultSettle is how many show polls a started systemd unit reports
// activating before it is active.
const DefaultSettle = 2

// firstFakePID is the first MainPID the model gives a started service.
const firstFakePID = 40000

// Host is F-HOSTS over one capture. Its methods are safe for concurrent use.
type Host struct {
	t    T
	dir  string
	init platform.InitSystem

	mu      sync.Mutex
	served  map[string]svcctl.Result // Captured outputs by space-joined argv
	order   []string                 // show property order
	units   map[string]*unit         // systemd, by name without .service
	scripts map[string]*script       // OpenRC
	answers map[string]answer
	hangs   map[string]bool
	settle  int
	clock   int64
	nextPID int
	calls   []Call
}

type answer struct {
	res svcctl.Result
	err error
}

// unit is a modeled systemd unit.
type unit struct {
	keys         []string
	props        map[string]string
	captured     []byte // The verbatim show capture while the model is unchanged
	changed      bool
	failsOnStart bool
	settle       int  // show polls left in activating
	starting     bool // A start is settling
}

// script is a modeled OpenRC script.
type script struct {
	word         string
	exists       bool
	captured     *svcctl.Result // The verbatim status capture while unchanged
	changed      bool
	failsOnStart bool
}

// CaptureDir is the directory of capture: a name under testdata/services,
// or an absolute path.
func CaptureDir(capture string) (string, error) {
	if filepath.IsAbs(capture) {
		return capture, nil
	}
	dir, err := TestdataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "services", capture), nil
}

// Captures lists the capture directory names under testdata/services.
func Captures() ([]string, error) {
	dir, err := TestdataDir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "services"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// CaptureOutput is a captured command's output and exit code: the file's
// bytes before its trailing "Exit N" line.
func CaptureOutput(capture, file string) (svcctl.Result, error) {
	dir, err := CaptureDir(capture)
	if err != nil {
		return svcctl.Result{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
	if err != nil {
		return svcctl.Result{}, err
	}
	return splitExit(b, file)
}

var exitLine = regexp.MustCompile(`(?m)^Exit (\d+)\n?\z`)

func splitExit(b []byte, file string) (svcctl.Result, error) {
	m := exitLine.FindSubmatchIndex(b)
	if m == nil {
		return svcctl.Result{}, fmt.Errorf("svcctltest: %s has no trailing Exit line", file)
	}
	code, err := strconv.Atoi(string(b[m[2]:m[3]]))
	if err != nil {
		return svcctl.Result{}, err
	}
	return svcctl.Result{ExitCode: code, Output: slices.Clone(b[:m[0]])}, nil
}

// CaptureProvenance is a capture's PROVENANCE: its keys and, under "commands:",
// each output file's argv as written there.
type CaptureProvenance struct {
	Keys     map[string]string
	Commands map[string]string // file -> argv
}

// ReadProvenance parses a capture's PROVENANCE.
func ReadProvenance(capture string) (CaptureProvenance, error) {
	dir, err := CaptureDir(capture)
	if err != nil {
		return CaptureProvenance{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		return CaptureProvenance{}, err
	}
	p := CaptureProvenance{Keys: map[string]string{}, Commands: map[string]string{}}
	inCommands := false
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		if inCommands && strings.HasPrefix(line, "  ") {
			if f, argv, ok := strings.Cut(strings.TrimSpace(line), ": "); ok {
				p.Commands[f] = argv
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return CaptureProvenance{}, fmt.Errorf("svcctltest: %s/PROVENANCE: line without a key: %q", capture, line)
		}
		p.Keys[k] = strings.TrimSpace(v)
		inCommands = k == "commands"
	}
	return p, nil
}

// Init is the capture's init system, from its init-version.
func (p CaptureProvenance) Init() platform.InitSystem {
	v := p.Keys["init-version"]
	switch {
	case strings.HasPrefix(v, "systemd "):
		return platform.Systemd
	case strings.Contains(v, "OpenRC"):
		return platform.OpenRC
	}
	return ""
}

// NewHost returns F-HOSTS over capture (a name under testdata/services, or
// an absolute path).
func NewHost(t T, capture string) *Host {
	t.Helper()
	h := &Host{t: t, units: map[string]*unit{}, scripts: map[string]*script{}, answers: map[string]answer{},
		hangs: map[string]bool{}, served: map[string]svcctl.Result{}, settle: DefaultSettle, nextPID: firstFakePID}
	if err := h.load(capture); err != nil {
		t.Errorf("svcctltest: %v", err)
	}
	return h
}

func (h *Host) load(capture string) error {
	dir, err := CaptureDir(capture)
	if err != nil {
		return err
	}
	h.dir = dir
	prov, err := ReadProvenance(dir)
	if err != nil {
		return err
	}
	h.init = prov.Init()
	if h.init == "" {
		return fmt.Errorf("%s: PROVENANCE names no known init system: %q", capture, prov.Keys["init-version"])
	}
	if d, err := time.Parse(time.RFC3339, prov.Keys["date"]); err == nil {
		h.clock = d.Unix()
	}
	for file, argv := range prov.Commands {
		tool, _, _ := strings.Cut(argv, " ")
		if !slices.Contains([]string{systemctl, rcStatus, rcUpdate, rcService}, tool) || !strings.HasSuffix(file, ".txt") ||
			strings.Contains(file, "/") {
			continue
		}
		res, err := CaptureOutput(dir, file)
		if err != nil {
			return err
		}
		h.served[argv] = res
	}
	if _, err := loadFixtures(); err != nil {
		return err
	}
	h.order = showOrder
	if res, ok := h.served[strings.Join(showArgv("nexusd"), " ")]; ok {
		keys, _ := parseKV(res.Output)
		if len(keys) > 0 {
			h.order = keys
		}
	}
	return nil
}

// Init is the capture's init system.
func (h *Host) Init() platform.InitSystem { return h.init }

// Dir is the capture's directory.
func (h *Host) Dir() string { return h.dir }

// Answer makes argv answer res and err instead of the capture, every time.
func (h *Host) Answer(argv []string, res svcctl.Result, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[strings.Join(argv, " ")] = answer{res: res, err: err}
}

// HangOn makes argv block until its context is done and return the
// context's error, as the real runner does after killing the group.
func (h *Host) HangOn(argv []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hangs[strings.Join(argv, " ")] = true
}

// FailsOnStart makes every start or restart of service fail as the captured
// nx-svc-fail does.
func (h *Host) FailsOnStart(service string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch h.init {
	case platform.Systemd:
		h.unit(service).failsOnStart = true
	case platform.OpenRC:
		h.script(service).failsOnStart = true
	}
}

// SetSettle sets how many show polls a started systemd unit reports
// activating; the default is DefaultSettle.
func (h *Host) SetSettle(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settle = n
}

// Calls returns every call so far.
func (h *Host) Calls() []Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// Sent returns the argv of the calls that act on a service or the host:
// systemctl start, stop, restart and reboot, rc-service NAME start, stop and
// restart, and /sbin/reboot.
func (h *Host) Sent() [][]string {
	var out [][]string
	for _, c := range h.Calls() {
		if isSent(c.Argv) {
			out = append(out, c.Argv)
		}
	}
	return out
}

// Reads returns the argv of every other call.
func (h *Host) Reads() [][]string {
	var out [][]string
	for _, c := range h.Calls() {
		if !isSent(c.Argv) {
			out = append(out, c.Argv)
		}
	}
	return out
}

var sentVerbs = []string{"start", "stop", "restart", "reboot"}

func isSent(argv []string) bool {
	switch {
	case len(argv) == 1 && argv[0] == "/sbin/reboot":
		return true
	case len(argv) >= 2 && argv[0] == systemctl:
		return slices.Contains(sentVerbs, argv[1])
	case len(argv) == 4 && argv[0] == rcService:
		return slices.Contains(sentVerbs, argv[3])
	}
	return false
}

// Run is the svcctl.Options.Run.
func (h *Host) Run(ctx context.Context, argv []string) (svcctl.Result, error) {
	key := strings.Join(argv, " ")
	h.mu.Lock()
	hang := h.hangs[key]
	ans, override := h.answers[key]
	h.mu.Unlock()
	if hang {
		<-ctx.Done()
		return h.record(argv, svcctl.Result{}, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return h.record(argv, svcctl.Result{}, err)
	}
	if override {
		return h.record(argv, svcctl.Result{ExitCode: ans.res.ExitCode, Output: slices.Clone(ans.res.Output),
			Truncated: ans.res.Truncated}, ans.err)
	}
	h.mu.Lock()
	res, err := h.answer(argv)
	h.mu.Unlock()
	if err != nil {
		h.t.Helper()
		h.t.Errorf("svcctltest: unexpected argv %q: %v", argv, err)
		err = fmt.Errorf("%w: %w", svcctl.ErrNotStarted, err)
	}
	return h.record(argv, res, err)
}

func (h *Host) record(argv []string, res svcctl.Result, err error) (svcctl.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, Call{Argv: slices.Clone(argv), Result: res, Err: err})
	return res, err
}

// answer models one call. It is called with h.mu held.
func (h *Host) answer(argv []string) (svcctl.Result, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return svcctl.Result{}, errors.New("the program is not an absolute path")
	}
	if h.init == platform.Systemd && slices.Equal(argv, []string{systemctl, "reboot"}) ||
		h.init == platform.OpenRC && slices.Equal(argv, []string{"/sbin/reboot"}) {
		return svcctl.Result{}, nil
	}
	switch h.init {
	case platform.Systemd:
		return h.systemd(argv)
	case platform.OpenRC:
		return h.openrc(argv)
	}
	return svcctl.Result{}, fmt.Errorf("no init system")
}

func showArgv(name string) []string {
	return []string{systemctl, "show", "--timestamp=unix", "-p", showProps, "--", name + ".service"}
}

// unitArg is NAME from "NAME.service", when NAME is a valid service name.
func unitArg(arg string) (string, bool) {
	name, ok := strings.CutSuffix(arg, ".service")
	return name, ok && serviceName.MatchString(name)
}

func (h *Host) systemd(argv []string) (svcctl.Result, error) {
	switch {
	case slices.Equal(argv, listUnitsArgv):
		return h.listUnits()
	case slices.Equal(argv, listUnitFilesArgv):
		return h.servedOr(argv, "list-unit-files.txt")
	case slices.Equal(argv, listFailedArgv):
		return CaptureOutput(h.dir, "oracle/failed.txt")
	}
	if len(argv) == 7 && slices.Equal(argv[:6], showArgv("")[:6]) {
		if name, ok := unitArg(argv[6]); ok {
			return h.show(name), nil
		}
	}
	if len(argv) == 4 && argv[0] == systemctl && argv[2] == "--" {
		name, ok := unitArg(argv[3])
		if !ok {
			return svcctl.Result{}, errors.New("not a service unit")
		}
		switch argv[1] {
		case "is-enabled":
			return h.isEnabled(argv, name)
		case "start", "stop", "restart":
			return h.systemdAction(argv[1], name)
		}
	}
	if res, ok := h.served[strings.Join(argv, " ")]; ok {
		return cloneResult(res), nil
	}
	return svcctl.Result{}, errors.New("not a systemctl call svcctl makes")
}

func cloneResult(r svcctl.Result) svcctl.Result {
	return svcctl.Result{ExitCode: r.ExitCode, Output: slices.Clone(r.Output), Truncated: r.Truncated}
}

// servedOr is the PROVENANCE-served output of argv, or file's.
func (h *Host) servedOr(argv []string, file string) (svcctl.Result, error) {
	if res, ok := h.served[strings.Join(argv, " ")]; ok {
		return cloneResult(res), nil
	}
	return CaptureOutput(h.dir, file)
}

// listLine is one list-units line: UNIT LOAD ACTIVE SUB DESCRIPTION.
type listLine struct{ load, active, sub, desc string }

func (h *Host) unitListing() (map[string]listLine, map[string]string, error) {
	lu, err := h.servedOr(listUnitsArgv, "list-units.txt")
	if err != nil {
		return nil, nil, err
	}
	lf, err := h.servedOr(listUnitFilesArgv, "list-unit-files.txt")
	if err != nil {
		return nil, nil, err
	}
	units := map[string]listLine{}
	for line := range strings.Lines(string(lu.Output)) {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		name, ok := strings.CutSuffix(f[0], ".service")
		if !ok {
			continue
		}
		units[name] = listLine{load: f[1], active: f[2], sub: f[3], desc: strings.Join(f[4:], " ")}
	}
	files := map[string]string{}
	for line := range strings.Lines(string(lf.Output)) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if name, ok := strings.CutSuffix(f[0], ".service"); ok {
			files[name] = f[1]
		}
	}
	return units, files, nil
}

// listUnits is the captured list-units with each changed unit's line
// rewritten to its modeled state.
func (h *Host) listUnits() (svcctl.Result, error) {
	res, err := h.servedOr(listUnitsArgv, "list-units.txt")
	if err != nil {
		return res, err
	}
	var out bytes.Buffer
	for line := range strings.Lines(string(res.Output)) {
		f := strings.Fields(line)
		if len(f) >= 4 {
			if name, ok := strings.CutSuffix(f[0], ".service"); ok {
				if u, ok := h.units[name]; ok && u.changed {
					fmt.Fprintf(&out, "%s %s %s %s %s\n", f[0], u.props["LoadState"], u.props["ActiveState"],
						u.props["SubState"], u.props["Description"])
					continue
				}
			}
		}
		out.WriteString(line)
	}
	res.Output = out.Bytes()
	return res, nil
}

func parseKV(out []byte) ([]string, map[string]string) {
	var keys []string
	props := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			keys = append(keys, k)
			props[k] = v
		}
	}
	return keys, props
}

// unit is the model of name, seeded from the capture on first use. It is
// called with h.mu held.
func (h *Host) unit(name string) *unit {
	if u, ok := h.units[name]; ok {
		return u
	}
	u := &unit{}
	h.units[name] = u
	if res, ok := h.served[strings.Join(showArgv(name), " ")]; ok {
		u.keys, u.props = parseKV(res.Output)
		u.captured = res.Output
		return u
	}
	u.keys = h.order
	u.props = map[string]string{"Id": name + ".service", "Description": name + ".service", "LoadState": "not-found",
		"ActiveState": "inactive", "SubState": "dead", "FragmentPath": "", "UnitFileState": "",
		"ActiveEnterTimestamp": "", "InactiveEnterTimestamp": "", "MainPID": "0", "Result": "success",
		"NRestarts": "0", "MemoryCurrent": "[not set]"}
	units, files, err := h.unitListing()
	if err != nil {
		return u
	}
	l, listed := units[name]
	state, hasFile := files[name]
	if !hasFile {
		if tmpl, inst, ok := strings.Cut(name, "@"); ok && inst != "" {
			state, hasFile = files[tmpl+"@"]
		}
	}
	switch {
	case listed:
		u.props["LoadState"], u.props["ActiveState"], u.props["SubState"] = l.load, l.active, l.sub
		u.props["Description"] = l.desc
		if l.active == "active" || l.active == "reloading" {
			u.props["MainPID"] = strconv.Itoa(h.newPID())
		}
		if l.active == "failed" {
			u.props["Result"] = "exit-code"
		}
	case hasFile:
		u.props["LoadState"] = "loaded"
	}
	if hasFile {
		u.props["UnitFileState"] = state
	}
	return u
}

func (h *Host) newPID() int {
	h.nextPID++
	return h.nextPID
}

func (h *Host) tick() string {
	h.clock++
	return "@" + strconv.FormatInt(h.clock, 10)
}

func (u *unit) set(kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		if _, ok := u.props[kv[i]]; !ok {
			u.keys = append(slices.Clip(u.keys), kv[i])
		}
		u.props[kv[i]] = kv[i+1]
	}
	u.changed = true
	u.captured = nil
}

func (h *Host) show(name string) svcctl.Result {
	u := h.unit(name)
	if u.starting {
		if u.settle > 0 {
			u.settle--
		} else {
			u.starting = false
			u.set("ActiveState", "active", "SubState", "running", "MainPID", strconv.Itoa(h.newPID()),
				"ActiveEnterTimestamp", h.tick())
		}
	}
	if u.captured != nil {
		return svcctl.Result{Output: slices.Clone(u.captured)}
	}
	var out bytes.Buffer
	for _, k := range u.keys {
		fmt.Fprintf(&out, "%s=%s\n", k, u.props[k])
	}
	return svcctl.Result{Output: out.Bytes()}
}

func (h *Host) isEnabled(argv []string, name string) (svcctl.Result, error) {
	if res, ok := h.served[strings.Join(argv, " ")]; ok {
		return cloneResult(res), nil
	}
	b, err := os.ReadFile(filepath.Join(h.dir, "oracle", "is-enabled.tsv"))
	if err != nil {
		return svcctl.Result{}, err
	}
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) == 3 && f[0] == name+".service" {
			code, err := strconv.Atoi(f[2])
			if err != nil {
				return svcctl.Result{}, err
			}
			out := ""
			if f[1] != "" {
				out = f[1] + "\n"
			}
			return svcctl.Result{ExitCode: code, Output: []byte(out)}, nil
		}
	}
	return svcctl.Result{}, fmt.Errorf("no oracle/is-enabled.tsv line for %s.service", name)
}

// action is a capture of the planted services' actions, with from replaced
// by name.
func (h *Host) action(file, from, name string) (svcctl.Result, error) {
	res, err := CaptureOutput(h.dir, "actions/"+file)
	if err != nil {
		return res, fmt.Errorf("the capture has no actions/%s to model this call with: %w", file, err)
	}
	res.Output = bytes.ReplaceAll(res.Output, []byte(from), []byte(name))
	return res, nil
}

// readme is a testdata/README.md capture of nginx, with nginx replaced by
// name.
func readme(file, name string) (svcctl.Result, error) {
	fx, err := loadFixtures()
	if err != nil {
		return svcctl.Result{}, err
	}
	r, ok := fx[file]
	if !ok {
		return svcctl.Result{}, fmt.Errorf("no capture %s", file)
	}
	return svcctl.Result{ExitCode: r.ExitCode, Output: bytes.ReplaceAll(r.Output, []byte("nginx"), []byte(name))}, nil
}

func (h *Host) systemdAction(verb, name string) (svcctl.Result, error) {
	u := h.unit(name)
	if u.props["LoadState"] == "not-found" {
		return readme("systemd/"+verb+"-not-found.txt", name)
	}
	active := u.props["ActiveState"]
	running := active == "active" || active == "reloading" || active == "activating"
	switch verb {
	case "stop":
		switch {
		case active == "failed":
			u.set("MainPID", "0")
			return readme("systemd/stop-failed.txt", name)
		case running:
			u.starting = false
			u.set("ActiveState", "inactive", "SubState", "dead", "MainPID", "0", "InactiveEnterTimestamp", h.tick(),
				"MemoryCurrent", "[not set]")
			return h.action("stop-nx-svc-test.txt", "nx-svc-test", name)
		}
		return readme("systemd/stop-already.txt", name)
	case "start":
		if running {
			return h.action("start-nx-svc-test.txt", "nx-svc-test", name)
		}
	}
	if u.failsOnStart {
		u.starting = false
		u.set("ActiveState", "failed", "SubState", "failed", "Result", "exit-code", "MainPID", "0",
			"InactiveEnterTimestamp", h.tick())
		return h.action("start-nx-svc-fail.txt", "nx-svc-fail", name)
	}
	u.starting, u.settle = true, h.settle
	u.set("ActiveState", "activating", "SubState", "start", "Result", "success", "MainPID", "0")
	return h.action(verb+"-nx-svc-test.txt", "nx-svc-test", name)
}

// rcStatusLine is " NAME [ WORD ... ]" of rc-status -s -C.
var rcStatusLine = regexp.MustCompile(`^\s*(\S+)\s+\[\s*(\S+)[^\]]*\]\s*$`)

func (h *Host) openrc(argv []string) (svcctl.Result, error) {
	switch {
	case slices.Equal(argv, rcStatusArgv):
		return h.rcStatus()
	case slices.Equal(argv, rcUpdateArgv):
		return h.servedOr(argv, "rc-update.txt")
	case slices.Equal(argv, rcCrashedArgv):
		return h.rcCrashed()
	}
	if len(argv) == 3 && argv[0] == rcService && argv[1] == "--exists" && serviceName.MatchString(argv[2]) {
		if h.script(argv[2]).exists {
			return readme("openrc/exists.txt", argv[2])
		}
		return readme("openrc/exists-absent.txt", argv[2])
	}
	if len(argv) == 4 && argv[0] == rcService && argv[1] == "-C" && serviceName.MatchString(argv[2]) {
		name, verb := argv[2], argv[3]
		s := h.script(name)
		if !s.exists && slices.Contains([]string{"status", "start", "stop", "restart"}, verb) {
			return readme("openrc/absent.txt", name)
		}
		switch verb {
		case "status":
			return h.rcServiceStatus(s)
		case "start", "stop", "restart":
			return h.openrcAction(s, verb, name)
		}
	}
	if res, ok := h.served[strings.Join(argv, " ")]; ok {
		return cloneResult(res), nil
	}
	return svcctl.Result{}, errors.New("not an OpenRC call svcctl makes")
}

func (h *Host) rcStatus() (svcctl.Result, error) {
	res, err := h.servedOr(rcStatusArgv, "rc-status.txt")
	if err != nil {
		return res, err
	}
	var out bytes.Buffer
	for line := range strings.Lines(string(res.Output)) {
		if m := rcStatusLine.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			if s, ok := h.scripts[m[1]]; ok && s.changed {
				fmt.Fprintf(&out, " %s [  %s  ]\n", m[1], s.word)
				continue
			}
		}
		out.WriteString(line)
	}
	res.Output = out.Bytes()
	return res, nil
}

func (h *Host) rcCrashed() (svcctl.Result, error) {
	words, err := h.rcWords()
	if err != nil {
		return svcctl.Result{}, err
	}
	var crashed []string
	for name, w := range words {
		if s, ok := h.scripts[name]; ok {
			w = s.word
		}
		if w == "crashed" {
			crashed = append(crashed, name)
		}
	}
	if len(crashed) == 0 {
		return readme("openrc/rc-status-crashed-none.txt", "")
	}
	slices.Sort(crashed)
	return svcctl.Result{Output: []byte(strings.Join(crashed, "\n") + "\n")}, nil
}

// rcWords are the scripts the capture knows, each with its status word:
// the rc-status bracket word, else oracle/rc-service-status.tsv's, else
// stopped for a script only rc-update lists.
func (h *Host) rcWords() (map[string]string, error) {
	words := map[string]string{}
	up, err := h.servedOr(rcUpdateArgv, "rc-update.txt")
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(string(up.Output)) {
		if name, _, ok := strings.Cut(line, "|"); ok && strings.TrimSpace(name) != "" {
			words[strings.TrimSpace(name)] = "stopped"
		}
	}
	if b, err := os.ReadFile(filepath.Join(h.dir, "oracle", "rc-service-status.tsv")); err == nil {
		for line := range strings.Lines(string(b)) {
			if f := strings.Split(strings.TrimRight(line, "\n"), "\t"); len(f) == 3 && f[1] != "" {
				words[f[0]] = f[1]
			}
		}
	}
	st, err := h.servedOr(rcStatusArgv, "rc-status.txt")
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(string(st.Output)) {
		if m := rcStatusLine.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil {
			words[m[1]] = m[2]
		}
	}
	return words, nil
}

// script is the model of name. It is called with h.mu held.
func (h *Host) script(name string) *script {
	if s, ok := h.scripts[name]; ok {
		return s
	}
	s := &script{word: "stopped"}
	h.scripts[name] = s
	if words, err := h.rcWords(); err == nil {
		s.word, s.exists = words[name]
	}
	if res, ok := h.served[strings.Join([]string{rcService, "-C", name, "status"}, " ")]; ok {
		s.captured = &res
	}
	return s
}

// rcStatusExit is the exit code of `rc-service -C NAME status` for word, as
// captured.
func (h *Host) rcStatusExit(word string) int {
	if b, err := os.ReadFile(filepath.Join(h.dir, "oracle", "rc-service-status.tsv")); err == nil {
		for line := range strings.Lines(string(b)) {
			if f := strings.Split(strings.TrimRight(line, "\n"), "\t"); len(f) == 3 && f[1] == word {
				if code, err := strconv.Atoi(f[2]); err == nil {
					return code
				}
			}
		}
	}
	if r, err := readme("openrc/status-"+word+".txt", ""); err == nil {
		return r.ExitCode
	}
	return 1
}

func (h *Host) rcServiceStatus(s *script) (svcctl.Result, error) {
	if s.captured != nil && !s.changed {
		return cloneResult(*s.captured), nil
	}
	return svcctl.Result{ExitCode: h.rcStatusExit(s.word), Output: []byte(" * status: " + s.word + "\n")}, nil
}

func (h *Host) openrcAction(s *script, verb, name string) (svcctl.Result, error) {
	setWord := func(w string) {
		s.word, s.changed = w, true
	}
	switch verb {
	case "start":
		switch s.word {
		case "started":
			return h.action("start-already-nx-svc-test.txt", "nx-svc-test", name)
		case "crashed":
			return readme("openrc/start-crashed.txt", name)
		}
	case "stop":
		switch s.word {
		case "started":
			setWord("stopped")
			return h.action("stop-nx-svc-test.txt", "nx-svc-test", name)
		case "crashed":
			setWord("stopped")
			return readme("openrc/stop-crashed.txt", name)
		}
		return readme("openrc/stop-already.txt", name)
	}
	if s.failsOnStart {
		setWord("stopped")
		return h.action("start-nx-svc-fail.txt", "nx-svc-fail", name)
	}
	setWord("started")
	return h.action(verb+"-nx-svc-test.txt", "nx-svc-test", name)
}
