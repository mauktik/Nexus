package svcctl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/platform"
)

const (
	// maxDeptreeBytes bounds the read of /run/openrc/deptree (a few KiB on
	// the captured hosts). A larger file is treated as unreadable.
	maxDeptreeBytes = 1 << 20
	// maxInitScriptBytes bounds the read of /etc/init.d/<self>.
	maxInitScriptBytes = 64 << 10
)

// Deps is what Dependencies resolved.
type Deps struct {
	// Names are the services the self service depends on, sorted and
	// unique, each matching ServiceNamePattern. Never nil without an error.
	Names []string
	// Partial is set when the resolution is incomplete (OpenRC only): the
	// deptree could not be read or has no entry for the self service, so
	// Names are the unresolved needed names of its init script, or a needed
	// name has neither a deptree entry nor a provider and is kept as is.
	Partial bool
	// Reason says why Partial is set ("" otherwise).
	Reason string
}

// systemdDependencyKeys are the properties Dependencies reads on systemd. A
// PartOf= unit is kept exactly like a Requires= or BindsTo= one: stopping
// or restarting it stops or restarts the self service. StopPropagatedFrom
// is not read (A-39).
var systemdDependencyKeys = []string{"Requires", "BindsTo", "Requisite", "PartOf"}

// systemdDependenciesArgv is the argv of Dependencies on systemd.
func systemdDependenciesArgv(self string) []string {
	argv := []string{systemctlPath, "show"}
	for _, k := range systemdDependencyKeys {
		argv = append(argv, "-p", k)
	}
	return append(argv, "--", self+".service")
}

// Dependencies are the services self (a name without .service) cannot run
// without. On systemd they are the .service units of its Requires, BindsTo,
// Requisite and PartOf properties, without the suffix, from one `systemctl
// show` through opts.Run bounded by opts.Timeout; a non-zero exit is an
// error. On OpenRC they are the ineed names of self's entry in
// <RunRoot>/openrc/deptree, each resolved through the providedby names of
// the entry of that name (a virtual service such as net) or kept when the
// entry is a real script. An unusable deptree is not an error: the result
// is the needed names of <EtcRoot>/init.d/<self>'s depend(), unresolved,
// with Partial set. EtcRoot defaults to /etc.
// An invalid self or another init system is an error wrapping
// ErrNotStarted, and nothing is run.
func Dependencies(ctx context.Context, init platform.InitSystem, self string, opts Options) (Deps, error) {
	if err := checkService(self); err != nil {
		return Deps{}, err
	}
	b := newBase(opts)
	switch init {
	case platform.Systemd:
		argv := systemdDependenciesArgv(self)
		res, err := b.call(ctx, argv)
		if err != nil {
			return Deps{}, err
		}
		if res.ExitCode != 0 {
			return Deps{}, fmt.Errorf("svcctl: %s exited %d: %s", strings.Join(argv, " "), res.ExitCode,
				strings.TrimSpace(string(res.Output)))
		}
		return Deps{Names: systemdDependencyNames(string(res.Output))}, nil
	case platform.OpenRC:
		return openrcDependencies(b.runRoot, b.etcRoot, self), nil
	}
	return Deps{}, fmt.Errorf("%w: no dependency lookup for init system %q", ErrNotStarted, string(init))
}

// systemdDependencyNames are the .service units, without the suffix, of the
// systemdDependencyKeys lines of `systemctl show` output. Other units
// (targets, slices, mounts), other keys and names not matching
// ServiceNamePattern are dropped.
func systemdDependencyNames(out string) []string {
	names := []string{}
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "=")
		if !ok || !slices.Contains(systemdDependencyKeys, k) {
			continue
		}
		for _, u := range strings.Fields(v) {
			if n, ok := strings.CutSuffix(u, ".service"); ok && serviceName.MatchString(n) {
				names = append(names, n)
			}
		}
	}
	return sortedUnique(names)
}

func sortedUnique(names []string) []string {
	slices.Sort(names)
	return slices.Compact(names)
}

// deptreeEntry is one depinfo_N entry of the OpenRC deptree.
type deptreeEntry struct {
	ineed      []string
	providedby []string
}

// deptree maps a service name (a script or a virtual service) to its entry.
type deptree map[string]*deptreeEntry

// parseDeptree parses OpenRC's deptree, lines of the form
// depinfo_N_service='name' and depinfo_N_key_M='value'. Only the service,
// ineed and providedby keys are kept; any other line is ignored. When two
// entries name the same service the first is kept.
func parseDeptree(b []byte) deptree {
	byIndex := map[string]*deptreeEntry{}
	names := map[string]string{} // index -> service
	type kv struct{ idx, key, val string }
	var vals []kv
	for line := range bytes.Lines(b) {
		rest, ok := strings.CutPrefix(strings.TrimRight(string(line), "\r\n"), "depinfo_")
		if !ok {
			continue
		}
		lhs, rhs, ok := strings.Cut(rest, "=")
		if !ok || len(rhs) < 2 || rhs[0] != '\'' || rhs[len(rhs)-1] != '\'' {
			continue
		}
		val := rhs[1 : len(rhs)-1]
		if strings.ContainsRune(val, '\'') {
			continue
		}
		parts := strings.Split(lhs, "_")
		if !isDigits(parts[0]) {
			continue
		}
		switch {
		case len(parts) == 2 && parts[1] == "service":
			if _, dup := names[parts[0]]; !dup {
				names[parts[0]] = val
			}
		case len(parts) == 3 && isDigits(parts[2]) && (parts[1] == "ineed" || parts[1] == "providedby"):
			vals = append(vals, kv{parts[0], parts[1], val})
		}
	}
	d := deptree{}
	for idx, name := range names {
		if _, dup := d[name]; dup {
			continue
		}
		e := &deptreeEntry{}
		byIndex[idx] = e
		d[name] = e
	}
	for _, v := range vals {
		e, ok := byIndex[v.idx]
		if !ok {
			continue
		}
		if v.key == "ineed" {
			e.ineed = append(e.ineed, v.val)
		} else {
			e.providedby = append(e.providedby, v.val)
		}
	}
	return d
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolveDeptree resolves the ineed names of self's entry in d: a name
// whose entry has providedby names becomes those providers, a name whose
// entry has none is a real script and stays itself, and a name with no
// entry stays itself and makes the result Partial. Names not matching
// ServiceNamePattern are dropped. A missing self entry gives no names.
func resolveDeptree(d deptree, self string) Deps {
	names := []string{}
	var unresolved []string
	if e, ok := d[self]; ok {
		for _, n := range e.ineed {
			if !serviceName.MatchString(n) {
				continue
			}
			dep, ok := d[n]
			switch {
			case !ok:
				names = append(names, n)
				unresolved = append(unresolved, n)
			case len(dep.providedby) > 0:
				for _, p := range dep.providedby {
					if serviceName.MatchString(p) {
						names = append(names, p)
					}
				}
			default:
				names = append(names, n)
			}
		}
	}
	res := Deps{Names: sortedUnique(names)}
	if len(unresolved) > 0 {
		res.Partial = true
		res.Reason = "no deptree entry or provider for " + strings.Join(sortedUnique(unresolved), ", ")
	}
	return res
}

// openrcDependencies is Dependencies on OpenRC.
func openrcDependencies(runRoot, etcRoot, self string) Deps {
	path := filepath.Join(runRoot, "openrc", "deptree")
	d, err := readDeptree(path)
	if err == nil {
		if _, ok := d[self]; ok {
			return resolveDeptree(d, self)
		}
		err = fmt.Errorf("%s has no entry for %s", path, self)
	}
	script := filepath.Join(etcRoot, "init.d", self)
	needs, serr := readInitScriptNeeds(script)
	reason := fmt.Sprintf("the deptree is unusable (%v); the needed names of %s are not resolved", err, script)
	if serr != nil {
		reason = fmt.Sprintf("the deptree is unusable (%v), and so is the init script (%v)", err, serr)
	}
	return Deps{Names: needs, Partial: true, Reason: reason}
}

// readDeptree reads and parses the deptree at path, through openRegular and
// bounded by maxDeptreeBytes.
func readDeptree(path string) (deptree, error) {
	b, err := readBounded(path, maxDeptreeBytes)
	if err != nil {
		return nil, err
	}
	return parseDeptree(b), nil
}

// readInitScriptNeeds are the need names of the init script at path. They
// are never nil.
func readInitScriptNeeds(path string) ([]string, error) {
	b, err := readBounded(path, maxInitScriptBytes)
	if err != nil {
		return []string{}, err
	}
	return initScriptNeeds(b), nil
}

// readBounded is the content of the regular file at path, opened through
// openRegular. A file over max bytes is an error.
func readBounded(path string, max int64) ([]byte, error) {
	f, fi, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is over %d bytes", path, max)
	}
	return b, nil
}

var dependFunc = regexp.MustCompile(`(?m)^[ \t]*depend[ \t]*\([ \t]*\)`)

// initScriptNeeds are the names after `need` in the depend() function of an
// openrc-run script, sorted and unique, each matching ServiceNamePattern.
// The function body runs from the first { after depend() to its matching }.
// Statements are split at newlines and semicolons, and a # starts a comment.
// The script is not run, so names built from variables are not seen.
func initScriptNeeds(b []byte) []string {
	names := []string{}
	loc := dependFunc.FindIndex(b)
	if loc == nil {
		return names
	}
	rest := b[loc[1]:]
	open := bytes.IndexByte(rest, '{')
	if open < 0 {
		return names
	}
	depth, end := 0, len(rest)
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
		if depth == 0 {
			end = i
			break
		}
	}
	body := string(rest[open+1 : end])
	for _, stmt := range strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == ';' }) {
		f := strings.Fields(stmt)
		if len(f) == 0 || f[0] != "need" {
			continue
		}
		for _, n := range f[1:] {
			if strings.HasPrefix(n, "#") {
				break
			}
			if serviceName.MatchString(n) {
				names = append(names, n)
			}
		}
	}
	return sortedUnique(names)
}
