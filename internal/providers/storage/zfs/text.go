// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package zfs

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The -o column lists of the text forms. Argv requests them, and the text
// parsers split each row on tabs into exactly these columns.
const (
	PoolListColumns     = "name,size,allocated,free,capacity,fragmentation,dedupratio,health,altroot,readonly"
	PoolGetColumns      = "name,property,value,source"
	DatasetColumns      = "name,type,used,avail,refer,usedbysnapshots,quota,refquota,reservation,compression,compressratio,recordsize,mountpoint,mounted,encryption,keystatus"
	SnapshotColumns     = "name,creation,used,refer,userrefs,nexus:created-by"
	SnapshotTimeColumns = "name,creation"
)

// textLoc is the zone zpool status's ctime(3) dates are read in: the tools
// run with an environment without TZ (D-3), so zpool prints the date in the
// zone of /etc/localtime, which nexusd, with no TZ in its unit either, reads
// the same way through time.Local (A-60). Tests pin it: UTC for the derived
// twins, the zone of a capture's PROVENANCE for a host capture.
var textLoc = time.Local

// rows splits -H output into tab-separated rows of one of the allowed
// column lists, all rows the same width. Output that does not end in a
// newline was cut short and is an error.
func rows(b []byte, command string, columns ...string) ([]string, [][]string, error) {
	if len(b) > StdoutCap {
		return nil, nil, fmt.Errorf("zfs: %s output is %d bytes, over %d", command, len(b), StdoutCap)
	}
	if len(b) == 0 {
		return strings.Split(columns[0], ","), nil, nil
	}
	if b[len(b)-1] != '\n' {
		return nil, nil, fmt.Errorf("zfs: %s output does not end in a newline", command)
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	var cols []string
	out := make([][]string, 0, len(lines))
	for i, l := range lines {
		f := strings.Split(l, "\t")
		if cols == nil {
			for _, c := range columns {
				if cs := strings.Split(c, ","); len(cs) == len(f) {
					cols = cs
					break
				}
			}
		}
		if cols == nil || len(f) != len(cols) {
			return nil, nil, fmt.Errorf("zfs: %s line %d has %d tab-separated fields, want %d", command, i+1, len(f), len(strings.Split(columns[0], ",")))
		}
		if f[0] == "" {
			return nil, nil, fmt.Errorf("zfs: %s line %d has an empty name", command, i+1)
		}
		out = append(out, f)
	}
	return cols, out, nil
}

func rowGetter(cols, row []string) getter {
	return func(col string) (string, bool) {
		for i, c := range cols {
			if c == col {
				return row[i], true
			}
		}
		return "", false
	}
}

// ParsePoolListText reads `zpool list -H -p -o PoolListColumns [NAME]`.
func ParsePoolListText(b []byte) ([]Pool, error) {
	cols, rs, err := rows(b, "zpool list", PoolListColumns)
	if err != nil {
		return nil, err
	}
	pools := make([]Pool, 0, len(rs))
	for _, r := range rs {
		p := Pool{Name: boundText(r[0])}
		fillPoolList(&p, rowGetter(cols, r), "")
		pools = append(pools, p)
	}
	return pools, nil
}

// ParsePoolGetText reads `zpool get -H -p -o PoolGetColumns all NAME`.
func ParsePoolGetText(b []byte) (map[string]string, error) {
	_, rs, err := rows(b, "zpool get", PoolGetColumns)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, errors.New("zfs: zpool get lists no pool")
	}
	out := make(map[string]string, len(rs))
	for i, r := range rs {
		if r[0] != rs[0][0] {
			return nil, fmt.Errorf("zfs: zpool get line %d is for another pool", i+1)
		}
		out[boundText(r[1])] = boundText(r[2])
	}
	return out, nil
}

// ParseDatasetsText reads `zfs list -H -p -t filesystem,volume -o
// DatasetColumns [NAME]`.
func ParseDatasetsText(b []byte) ([]Dataset, error) {
	cols, rs, err := rows(b, "zfs list", DatasetColumns)
	if err != nil {
		return nil, err
	}
	out := make([]Dataset, 0, len(rs))
	for _, r := range rs {
		d := Dataset{Name: boundText(r[0]), Pool: boundText(poolOf(r[0]))}
		fillDataset(&d, rowGetter(cols, r), "")
		out = append(out, d)
	}
	return out, nil
}

// ParseSnapshotsText reads `zfs list -H -p -t snapshot -o SnapshotColumns`
// (snapshots.list) or `-o SnapshotTimeColumns` (the dataset reads), told
// apart by the number of columns.
func ParseSnapshotsText(b []byte) ([]Snapshot, error) {
	cols, rs, err := rows(b, "zfs list", SnapshotColumns, SnapshotTimeColumns)
	if err != nil {
		return nil, err
	}
	full := len(cols) == len(strings.Split(SnapshotColumns, ","))
	out := make([]Snapshot, 0, len(rs))
	for _, r := range rs {
		var s Snapshot
		if err := fillSnapshot(&s, r[0], rowGetter(cols, r), full); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ParseStatusText reads the text of `zpool status -p [-v] [NAME]`
// (status_callback in cmd/zpool/zpool_main.c): per pool the pool and state
// lines, the status and action texts, the scan lines, the config table and
// the errors section.
func ParseStatusText(b []byte) ([]Pool, error) {
	if len(b) > StdoutCap {
		return nil, fmt.Errorf("zfs: zpool status output is %d bytes, over %d", len(b), StdoutCap)
	}
	if len(b) == 0 {
		return nil, nil
	}
	if b[len(b)-1] != '\n' {
		return nil, errors.New("zfs: zpool status output does not end in a newline")
	}
	st := statusText{lines: strings.Split(string(b[:len(b)-1]), "\n")}
	for st.i < len(st.lines) {
		l := st.lines[st.i]
		name, ok := strings.CutPrefix(l, "  pool: ")
		if !ok {
			if strings.TrimSpace(l) == "" {
				st.i++
				continue
			}
			return nil, fmt.Errorf("zfs: zpool status line %d: expected \"  pool:\", got %q", st.i+1, boundText(l))
		}
		st.i++
		p, err := st.pool(name)
		if err != nil {
			return nil, err
		}
		st.pools = append(st.pools, p)
	}
	return st.pools, nil
}

type statusText struct {
	lines []string
	i     int
	pools []Pool
}

// sectionKey returns the keyword of a "<keyword>: …" header line (the
// keywords are right-aligned to column 8), or "".
func sectionKey(l string) (string, string) {
	t := strings.TrimLeft(l, " ")
	if len(l)-len(t) > 7 || strings.HasPrefix(l, "\t") {
		return "", ""
	}
	k, rest, ok := strings.Cut(t, ":")
	if !ok || k == "" || strings.Trim(k, "abcdefghijklmnopqrstuvwxyz") != "" {
		return "", ""
	}
	if rest != "" && rest[0] != ' ' {
		return "", ""
	}
	return k, strings.TrimPrefix(rest, " ")
}

// continuation collects the tab-indented lines that follow a header line.
func (st *statusText) continuation(first string) []string {
	out := []string{first}
	for st.i < len(st.lines) && strings.HasPrefix(st.lines[st.i], "\t") {
		out = append(out, st.lines[st.i])
		st.i++
	}
	return out
}

func (st *statusText) pool(name string) (Pool, error) {
	p := Pool{Name: boundText(name)}
	r := reasons{&p.Unavailable, "zpool status"}
	sawState, sawConfig, sawErrors := false, false, false
	haveScan := false // a scrub or resilver section was read
	p.Scan = &Scan{Function: ScanNone, State: ScanNone}
	for st.i < len(st.lines) {
		l := st.lines[st.i]
		if strings.HasPrefix(l, "  pool: ") {
			break
		}
		st.i++
		if strings.TrimSpace(l) == "" {
			continue
		}
		key, rest := sectionKey(l)
		switch key {
		case "state":
			sawState = true
			p.RawHealth, p.Health = boundText(rest), normalizeHealth(rest)
		case "status", "action":
			text := ptr(boundText(strings.Join(st.continuation(rest), "\n")))
			if key == "status" {
				p.Status = text
			} else {
				p.Action = text
			}
		case "scan":
			// print_scan_status prints the scrub, or the newer error scrub,
			// first and may then add a resilver or a rebuild ("resilvered
			// (<vdev>) …"). The first scrub or resilver section is the
			// pool's scan, as in the JSON form's one scan_stats; a later
			// section never replaces it.
			sc := parseScanText(st.continuation(rest))
			if haveScan {
				break
			}
			if sc.Function == ScanScrub || sc.Function == ScanResilver {
				p.Scan, haveScan = sc, true
			} else if p.Scan.Function == ScanNone {
				p.Scan = sc
			}
		case "scrub":
			// An error scrub (print_err_scrub_status): the scan only when
			// no scrub or resilver section and no other scan was seen.
			st.continuation(rest)
			if !haveScan && p.Scan.Function == ScanNone {
				p.Scan = parseErrorScrubText(rest)
			}
		case "config":
			sawConfig = true
			if strings.TrimSpace(rest) != "" {
				// "The configuration cannot be determined."
				continue
			}
			if err := st.config(&p); err != nil {
				return Pool{}, err
			}
		case "errors":
			sawErrors = true
			st.errors(&p, rest)
		case "":
			return Pool{}, fmt.Errorf("zfs: zpool status line %d: unexpected %q", st.i, boundText(l))
		default:
			// see:, remove:, checkpoint:, expand: and others carry nothing
			// the model keeps.
			st.continuation(rest)
		}
	}
	if !sawState {
		r.absent("health")
	}
	if !sawConfig || len(p.Vdevs) == 0 {
		r.absent("vdevs")
		r.absent("vdevCount")
		r.absent("errors")
	}
	if !sawErrors {
		r.absent("permanentErrors")
	}
	finishPool(&p)
	return p, nil
}

// config reads the vdev table: a blank line, the NAME STATE READ WRITE CKSUM
// header, then one row per vdev, indented two spaces per level after a tab
// (print_status_config), with class headers (dedup, special, logs, cache,
// spares) at the root's indent; a blank line ends it.
func (st *statusText) config(p *Pool) error {
	for st.i < len(st.lines) && strings.TrimSpace(st.lines[st.i]) == "" {
		st.i++
	}
	if st.i >= len(st.lines) || !strings.HasPrefix(strings.TrimSpace(st.lines[st.i]), "NAME") {
		return fmt.Errorf("zfs: zpool status pool %q: no vdev table header", p.Name)
	}
	st.i++
	type row struct {
		name, state, class string
		depth              int
		counts             [3]*uint64
	}
	var rs []row
	class := ""
	for st.i < len(st.lines) {
		l := st.lines[st.i]
		if strings.TrimSpace(l) == "" {
			break
		}
		st.i++
		body, ok := strings.CutPrefix(l, "\t")
		if !ok {
			return fmt.Errorf("zfs: zpool status pool %q: vdev row %q does not start with a tab", p.Name, boundText(l))
		}
		t := strings.TrimLeft(body, " ")
		indent := len(body) - len(t)
		f := strings.Fields(t)
		if indent%2 != 0 || len(f) == 0 {
			return fmt.Errorf("zfs: zpool status pool %q: malformed vdev row %q", p.Name, boundText(l))
		}
		depth := indent / 2
		if depth > maxVdevDepth {
			return fmt.Errorf("zfs: zpool status pool %q: vdev tree is deeper than %d", p.Name, maxVdevDepth)
		}
		if c, ok := classWords[f[0]]; ok && len(f) == 1 && depth == 0 && len(rs) > 0 && f[0] != "l2cache" {
			class = c
			continue
		}
		if len(f) < 2 {
			return fmt.Errorf("zfs: zpool status pool %q: vdev row %q has no state", p.Name, boundText(l))
		}
		if depth == 0 {
			class = ""
		}
		if len(rs) == 0 && depth != 0 || len(rs) > 0 && depth > rs[len(rs)-1].depth+1 {
			return fmt.Errorf("zfs: zpool status pool %q: vdev row %q skips a level", p.Name, boundText(l))
		}
		rw := row{name: f[0], state: f[1], class: class, depth: depth}
		if len(f) >= 5 {
			var c [3]*uint64
			ok := true
			for k := range 3 {
				n, err := strconv.ParseUint(f[2+k], 10, 64)
				if err != nil {
					ok = false
					break
				}
				c[k] = &n
			}
			if ok {
				rw.counts = c
			}
		}
		rs = append(rs, rw)
	}
	parents := make([]string, 0, 8)
	for i, rw := range rs {
		leaf := i+1 == len(rs) || rs[i+1].depth <= rw.depth
		parents = parents[:min(rw.depth, len(parents))]
		parent := ""
		if rw.depth > 0 && len(parents) > 0 {
			parent = parents[len(parents)-1]
		}
		cls := ""
		if rw.depth == 1 {
			cls = rw.class
		}
		p.Vdevs = append(p.Vdevs, Vdev{
			Name:     boundText(rw.name),
			Type:     vdevType("", rw.name, cls, rw.depth, leaf),
			State:    vdevStateWord(rw.state),
			Depth:    rw.depth,
			Parent:   boundText(parent),
			Read:     rw.counts[0],
			Write:    rw.counts[1],
			Checksum: rw.counts[2],
		})
		// A class member's parent is the root: class headers sit at the
		// root's level.
		if rw.depth == 0 {
			parents = append(parents[:0], rw.name)
		} else {
			parents = append(parents, rw.name)
		}
	}
	return nil
}

var dataErrors = regexp.MustCompile(`^([0-9]+) data errors, use '-v' for a list$`)

// errors reads the errors section: "No known data errors", "<N> data
// errors, use '-v' for a list", or the permanent-error file list of -v
// (print_error_log: a blank line, then each path after seven spaces and a
// space).
func (st *statusText) errors(p *Pool, rest string) {
	switch {
	case rest == "No known data errors":
		p.ErrorCount = ptr(uint64(0))
	case strings.HasPrefix(rest, "Permanent errors have been detected"):
		var paths []string
		for st.i < len(st.lines) {
			l := st.lines[st.i]
			if strings.HasPrefix(l, "  pool: ") {
				break
			}
			st.i++
			if path, ok := strings.CutPrefix(l, "        "); ok {
				paths = append(paths, path)
			} else if strings.TrimSpace(l) != "" {
				st.i--
				break
			}
		}
		p.ErrorCount = ptr(uint64(len(paths)))
		keepPaths(p, paths)
	default:
		if m := dataErrors.FindStringSubmatch(rest); m != nil {
			if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
				p.ErrorCount = &n
				return
			}
		}
		reasons{&p.Unavailable, "zpool status"}.bad("permanentErrors", rest, "an error count")
	}
}

var (
	scanDone     = regexp.MustCompile(`^(scrub repaired|resilvered) (\S+) in ((?:[0-9]+ days )?[0-9]+:[0-9]{2}:[0-9]{2}) with ([0-9]+) errors on (.+)$`)
	scanCanceled = regexp.MustCompile(`^(scrub|resilver) canceled on (.+)$`)
	scanRunning  = regexp.MustCompile(`^(scrub|resilver) (in progress|paused) since (.+)$`)
	scanStarted  = regexp.MustCompile(`^\tscrub started on (.+)$`)
	scanScanned  = regexp.MustCompile(`^\t(\S+) / (\S+) scanned`)
	scanRepaired = regexp.MustCompile(`^\t(\S+) (?:repaired|resilvered), `)
	dhms         = regexp.MustCompile(`^(?:([0-9]+) days )?([0-9]+):([0-9]{2}):([0-9]{2})$`)
	niceBytes    = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([BKMGTPE])$`)
)

// parseScanText reads the scan lines of print_scan_scrub_resilver_status
// (OpenZFS 2.4.1 cmd/zpool/zpool_main.c L8937-L9029). The text form prints
// sizes rounded (zfs_nicebytes, even with -p), so Processed, Examined and
// ToExamine are the nearest byte count of the printed value (A-59). A value
// the text does not print stays nil: Examined and ToExamine of a finished or
// canceled scan, Processed of a canceled scan, Errors of a scan that is not
// finished and Start of a canceled scan. The provider reports each as null
// with the reason of DF-2 step 3, never as zero. Dates are ctime(3) read in
// textLoc, the zone zpool printed them in (A-60).
func parseScanText(ls []string) *Scan {
	s := &Scan{Function: ScanUnknown, State: ScanUnknown}
	first := ls[0]
	if first == "none requested" {
		return &Scan{Function: ScanNone, State: ScanNone}
	}
	fn := func(w string) string {
		if strings.HasPrefix(w, "scrub") {
			return ScanScrub
		}
		return ScanResilver
	}
	switch {
	case scanDone.MatchString(first):
		m := scanDone.FindStringSubmatch(first)
		s.Function, s.State = fn(m[1]), ScanFinished
		s.Processed = parseNiceBytes(m[2])
		if n, err := strconv.ParseUint(m[4], 10, 64); err == nil {
			s.Errors = &n
		}
		s.End = parseCtime(m[5])
		if d, ok := parseDHMS(m[3]); ok && s.End != nil {
			s.Start = ptr(s.End.Add(-d))
		}
	case scanCanceled.MatchString(first):
		m := scanCanceled.FindStringSubmatch(first)
		s.Function, s.State = fn(m[1]), ScanCanceled
		s.End = parseCtime(m[2])
	case scanRunning.MatchString(first):
		m := scanRunning.FindStringSubmatch(first)
		s.Function, s.State = fn(m[1]), ScanScanning
		s.Start = parseCtime(m[3])
		if m[2] == "paused" {
			s.State, s.Start = ScanPaused, nil
		}
		for _, l := range ls[1:] {
			switch {
			case scanStarted.MatchString(l):
				s.Start = parseCtime(scanStarted.FindStringSubmatch(l)[1])
			case scanScanned.MatchString(l):
				m := scanScanned.FindStringSubmatch(l)
				s.Examined, s.ToExamine = parseNiceBytes(m[1]), parseNiceBytes(m[2])
			case scanRepaired.MatchString(l):
				s.Processed = parseNiceBytes(scanRepaired.FindStringSubmatch(l)[1])
			}
		}
	}
	return s
}

// parseErrorScrubText reads print_err_scrub_status's first line.
func parseErrorScrubText(first string) *Scan {
	s := &Scan{Function: ScanErrorScrub, State: ScanUnknown}
	switch {
	case strings.HasPrefix(first, "scrubbed "):
		s.State = ScanFinished
	case strings.HasPrefix(first, "error scrub canceled"):
		s.State = ScanCanceled
	case strings.HasPrefix(first, "error scrub in progress"):
		s.State = ScanScanning
	case strings.HasPrefix(first, "error scrub paused"):
		s.State = ScanPaused
	}
	return s
}

func parseCtime(s string) *time.Time {
	t, err := time.ParseInLocation(time.ANSIC, strings.TrimSpace(s), textLoc)
	if err != nil {
		return nil
	}
	return ptr(t.UTC())
}

func parseDHMS(s string) (time.Duration, bool) {
	m := dhms.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	var n [4]int64
	for i, v := range m[1:] {
		if v == "" {
			continue
		}
		x, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return 0, false
		}
		n[i] = x
	}
	return time.Duration(((n[0]*24+n[1])*60+n[2])*60+n[3]) * time.Second, true
}

// parseNiceBytes reads a zfs_nicebytes size ("0B", "256K", "62.7G"): exact
// when it has no fraction, else the nearest byte count of the rounded value.
func parseNiceBytes(s string) *uint64 {
	m := niceBytes.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	shift := strings.Index("BKMGTPE", m[2]) * 10
	v := f * math.Exp2(float64(shift))
	if v >= math.Exp2(64) {
		return nil
	}
	if !strings.Contains(m[1], ".") {
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil || shift > 0 && n > math.MaxUint64>>shift {
			return nil
		}
		return ptr(n << shift)
	}
	return ptr(uint64(math.Round(v)))
}
