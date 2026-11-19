package svcctl

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/platform"
)

// Details is one service as Describe reports it (DF-1 step 5). A field the
// init system does not report is its zero value with a reason in
// Unavailable, keyed by the field's JSON name (since, mainPid, subState,
// result, restarts, memoryBytes, unitFilePath, scriptPath, canonicalName).
type Details struct {
	Entry
	// CanonicalName is the unit's Id without .service, which differs from
	// Name for an alias (systemd); the script name on OpenRC.
	CanonicalName string
	// Known is false when the init system does not know the service; the
	// other fields are then unset. It is not an error.
	Known                    bool
	Since                    *time.Time
	MainPID                  int
	SubState, Result         string
	Restarts                 *int
	MemoryBytes              *int64
	UnitFilePath, ScriptPath string
	Unavailable              map[string]string
}

// describeProps are the only properties Describe asks systemd for (NFR-5:
// never Environment, ExecStart or the unit file's text).
const describeProps = "Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath"

var describePropList = strings.Split(describeProps, ",")

// describeShowArgv is the argv of Describe on systemd.
func describeShowArgv(service string) []string {
	return []string{systemctlPath, "show", "--timestamp=unix", "-p", describeProps, "--", service + ".service"}
}

// memoryNotSet is the MemoryCurrent systemd prints for no value
// (UINT64_MAX), besides "[not set]".
const memoryNotSet = "18446744073709551615"

// Reasons in Details.Unavailable.
const (
	reasonNotReported  = "not reported"
	reasonNotSet       = "not set"
	reasonNotRunning   = "not running"
	reasonNeverEntered = "never entered"
	reasonOpenRC       = "not reported by OpenRC"
)

// Describe reports one service (DF-1 step 5). name must match
// ServiceNamePattern, or the error wraps ErrNotStarted and nothing runs. On
// systemd it is one `systemctl show` of the fixed DF-1 properties; on OpenRC
// `rc-service -C NAME status`, the runlevels from `rc-update show -v`, the
// script path and the script's static description. A service the init
// system does not know is Known false with a nil error.
func Describe(ctx context.Context, init platform.InitSystem, name string, opts Options) (Details, error) {
	if err := checkService(name); err != nil {
		return Details{}, err
	}
	if opts.OutputCap <= 0 {
		opts.OutputCap = ListOutputCap
	}
	b := newBase(opts)
	switch init {
	case platform.Systemd:
		return b.describeSystemd(ctx, name)
	case platform.OpenRC:
		return b.describeOpenRC(ctx, name)
	}
	return Details{}, fmt.Errorf("%w: no service details for init system %q", ErrNotStarted, string(init))
}

func newDetails(name string) Details {
	return Details{Entry: Entry{Name: name, State: string(Unknown), Enabled: "unknown"}, Unavailable: map[string]string{}}
}

func (b base) describeSystemd(ctx context.Context, name string) (Details, error) {
	argv := describeShowArgv(name)
	res, err := b.call(ctx, argv)
	if err != nil {
		return Details{}, err
	}
	if res.ExitCode != 0 {
		return Details{}, fmt.Errorf("svcctl: systemctl show %s.service exited %d: %s", name, res.ExitCode, firstLine(res.Output))
	}
	if res.Truncated || len(res.Output) > b.outputCap {
		return Details{}, fmt.Errorf("svcctl: systemctl show %s.service output reached the cap", name)
	}
	kv := map[string]string{}
	for line := range strings.Lines(string(res.Output)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "="); ok && slices.Contains(describePropList, k) {
			if _, dup := kv[k]; !dup {
				kv[k] = v
			}
		}
	}
	load, ok := kv["LoadState"]
	if !ok {
		return Details{}, fmt.Errorf("svcctl: systemctl show %s.service printed no LoadState", name)
	}
	d := newDetails(name)
	if load == "not-found" {
		return d, nil
	}
	d.Known = true
	if id, ok := kv["Id"]; ok && strings.HasSuffix(id, ".service") && id != ".service" {
		d.CanonicalName = strings.TrimSuffix(id, ".service")
	} else {
		d.Unavailable["canonicalName"] = reasonNotReported
	}
	active, hasActive := kv["ActiveState"]
	sub, hasSub := kv["SubState"]
	d.State = string(mapActiveState(active))
	switch {
	case hasActive && hasSub:
		d.RawState = active + " (" + sub + ")"
	case hasActive:
		d.RawState = active
	}
	if hasSub {
		d.SubState = sub
	} else {
		d.Unavailable["subState"] = reasonNotReported
	}
	if r, ok := kv["Result"]; ok {
		d.Result = r
	} else {
		d.Unavailable["result"] = reasonNotReported
	}
	if state, ok := kv["UnitFileState"]; ok {
		d.Enabled, d.EnabledRaw = mapEnabled(state), state
	}
	if desc, ok := kv["Description"]; ok {
		d.Description, d.DescriptionKnown = desc, true
	}
	tsKey := "InactiveEnterTimestamp"
	if State(d.State) == Running {
		tsKey = "ActiveEnterTimestamp"
	}
	d.Since, d.Unavailable["since"] = unixTimestamp(kv, tsKey)
	switch pid, err := strconv.Atoi(kv["MainPID"]); {
	case err == nil && pid > 0:
		d.MainPID = pid
	case err == nil && pid == 0:
		d.Unavailable["mainPid"] = reasonNotRunning
	default:
		d.Unavailable["mainPid"] = reasonNotReported
	}
	if n, err := strconv.Atoi(kv["NRestarts"]); err == nil && n >= 0 {
		d.Restarts = &n
	} else {
		d.Unavailable["restarts"] = reasonNotReported
	}
	switch m := kv["MemoryCurrent"]; m {
	case "[not set]", memoryNotSet:
		d.Unavailable["memoryBytes"] = reasonNotSet
	default:
		if v, err := strconv.ParseUint(m, 10, 64); err == nil && v <= math.MaxInt64 {
			n := int64(v)
			d.MemoryBytes = &n
		} else {
			d.Unavailable["memoryBytes"] = reasonNotReported
		}
	}
	if p := kv["FragmentPath"]; p != "" {
		d.UnitFilePath = p
	} else {
		d.Unavailable["unitFilePath"] = reasonNotReported
	}
	d.Unavailable["scriptPath"] = "systemd units have no init script"
	for k, v := range d.Unavailable {
		if v == "" {
			delete(d.Unavailable, k)
		}
	}
	return d, nil
}

// unixTimestamp is the time of a `--timestamp=unix` property ("@SECONDS"),
// or nil with the reason: empty is never entered, anything else not
// reported.
func unixTimestamp(kv map[string]string, key string) (*time.Time, string) {
	v, ok := kv[key]
	switch {
	case !ok:
		return nil, reasonNotReported
	case v == "":
		return nil, reasonNeverEntered
	}
	if s, ok := strings.CutPrefix(v, "@"); ok {
		if sec, err := strconv.ParseInt(s, 10, 64); err == nil && sec > 0 {
			t := time.Unix(sec, 0).UTC()
			return &t, ""
		}
	}
	return nil, reasonNotReported
}

func (b base) describeOpenRC(ctx context.Context, name string) (Details, error) {
	res, err := b.call(ctx, []string{rcServicePath, "-C", name, "status"})
	if err != nil {
		return Details{}, err
	}
	out := strings.TrimSpace(string(res.Output))
	word, ok := statusWord(out)
	d := newDetails(name)
	if !ok {
		// rc-service exits 1 with "service `NAME' does not exist" for a
		// script that is not there (testdata/openrc/absent.txt).
		if res.ExitCode != 0 && strings.Contains(out, "does not exist") {
			return d, nil
		}
		if res.ExitCode != 0 {
			// An init script may define its own status() and print what it
			// likes, and this error reaches callers holding services:read
			// only, so none of that text is quoted (SEC-6, as SEC-4 does for
			// an action's output).
			return Details{}, fmt.Errorf("svcctl: rc-service -C %s status exited %d without a status word (its output is not quoted: an init script can print what services:logs protects)", name, res.ExitCode)
		}
	}
	update, err := b.listing(ctx, rcUpdateArgv)
	if err != nil {
		return Details{}, err
	}
	runlevels, _ := runlevelsOf(parseRCUpdate(update), name)
	d.Known = true
	d.CanonicalName = name
	d.State, d.RawState = string(mapRCWord(word)), word
	d.Runlevels, d.Enabled, d.EnabledRaw = runlevels, openrcEnabled(runlevels), strings.Join(runlevels, " ")
	d.ScriptPath = "/etc/init.d/" + name
	d.Description, d.DescriptionKnown = staticDescription(b.etcRoot, name)
	if State(d.State) == Running {
		if d.MainPID = b.openrcMainPID(name); d.MainPID == 0 {
			d.Unavailable["mainPid"] = "not found in OpenRC's daemon records"
		}
	} else {
		d.Unavailable["mainPid"] = reasonNotRunning
	}
	d.Unavailable["since"] = "OpenRC does not report when a service entered its state"
	for _, k := range []string{"subState", "result", "restarts", "memoryBytes"} {
		d.Unavailable[k] = reasonOpenRC
	}
	d.Unavailable["unitFilePath"] = "OpenRC has no unit files"
	return d, nil
}

// openrcMainPID is the PID of a running OpenRC service. A service run under
// supervise-daemon has <RunRoot>/openrc/options/<name>/child_pid, and its
// start-stop-daemon-style record in openrc/daemons/<name> names the
// supervisor's own pidfile (supervise-<name>.pid), so the child is the
// service's process and the record is not consulted: when the child is not
// live the answer is 0, never the supervisor. Any other service uses
// start-stop-daemon's records. A PID is kept only when <ProcRoot>/<pid>
// exists; 0 when none names a live process.
func (b base) openrcMainPID(name string) int {
	childPID := filepath.Join(b.runRoot, "openrc", "options", name, "child_pid")
	if _, err := os.Lstat(childPID); err != nil {
		return (&openrc{base: b}).mainPID(name)
	}
	pid := readPIDFile(childPID)
	if pid <= 0 {
		return 0
	}
	if _, err := os.Stat(filepath.Join(b.procRoot, strconv.Itoa(pid))); err != nil {
		return 0
	}
	return pid
}

// EnabledAtBoot reports whether name starts at boot (FR-10), with the init
// system's word: on systemd `systemctl is-enabled -- NAME.service`, true
// only for enabled; on OpenRC the runlevels `rc-update show -v` lists for
// it, true when they include default or boot. name must match
// ServiceNamePattern.
func EnabledAtBoot(ctx context.Context, init platform.InitSystem, name string, opts Options) (bool, string, error) {
	if err := checkService(name); err != nil {
		return false, "", err
	}
	if opts.OutputCap <= 0 {
		opts.OutputCap = ListOutputCap
	}
	b := newBase(opts)
	switch init {
	case platform.Systemd:
		res, err := b.call(ctx, []string{systemctlPath, "is-enabled", "--", name + ".service"})
		if err != nil {
			return false, "", err
		}
		word := strings.TrimSpace(string(res.Output))
		if word == "" || strings.ContainsAny(word, " \t\n") {
			return false, word, fmt.Errorf("svcctl: systemctl is-enabled %s.service exited %d: %s", name, res.ExitCode, firstLine(res.Output))
		}
		return word == "enabled", word, nil
	case platform.OpenRC:
		out, err := b.listing(ctx, rcUpdateArgv)
		if err != nil {
			return false, "", err
		}
		runlevels, _ := runlevelsOf(parseRCUpdate(out), name)
		return slices.Contains(runlevels, "default") || slices.Contains(runlevels, "boot"), strings.Join(runlevels, " "), nil
	}
	return false, "", fmt.Errorf("%w: no boot state for init system %q", ErrNotStarted, string(init))
}
