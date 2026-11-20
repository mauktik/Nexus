// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package v5

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/detach"
)

// Runner runs one command with an absolute argv[0] and no shell. A normal
// exit is (status, nil); only (0, nil) is success. The real implementation
// (exec) and the v5test.FakeHost implement it.
type Runner interface {
	Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (exitCode int, err error)
}

// versionPattern is the only part of a version value that is ever reported,
// so no other text from the versions file or FTL reaches a result.
var versionPattern = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*`)

// CleanVersion returns the leading ^v?[0-9]+(\.[0-9]+)* of s, or nil when s
// does not start with a version.
func CleanVersion(s string) *string {
	m := versionPattern.FindString(strings.TrimSpace(s))
	if m == "" {
		return nil
	}
	return &m
}

// Major is the major number of a version CleanVersion accepted, or 0.
func Major(v *string) int {
	if v == nil {
		return 0
	}
	digits, _, _ := strings.Cut(strings.TrimPrefix(*v, "v"), ".")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}

// VersionFile holds the installed versions from /etc/pihole/versions. A key
// that is absent or not a version is nil.
type VersionFile struct {
	Core, Web, FTL *string
}

// ReadVersions reads /etc/pihole/versions (V-8). Keys match exactly, as the
// whole text before the first '=': GITHUB_CORE_VERSION and the other
// GITHUB_* keys that `pihole updatechecker` writes are the newest upstream
// releases, not the installed ones, and must never be read as CORE_VERSION.
// The error is the file's open or read error (fs.ErrNotExist when missing).
func ReadVersions(p Paths) (VersionFile, error) {
	f, err := os.Open(p.VersionsPath())
	if err != nil {
		return VersionFile{}, err
	}
	defer f.Close()
	var v VersionFile
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "CORE_VERSION":
			v.Core = CleanVersion(val)
		case "WEB_VERSION":
			v.Web = CleanVersion(val)
		case "FTL_VERSION":
			v.FTL = CleanVersion(val)
		}
	}
	if err := sc.Err(); err != nil {
		return VersionFile{}, err
	}
	return v, nil
}

// DefaultFTLPort is FTL's text-API port when FTLPORT is unset (V-4).
const DefaultFTLPort = 4711

// FTLConf is what the reader needs from pihole-FTL.conf.
type FTLConf struct {
	Port    int    // FTLPORT, default 4711
	PIDFile string // PIDFILE, a host path, default /run/pihole-FTL.pid
	LogFile string // LOGFILE, a host path, default /var/log/pihole/FTL.log
}

// ReadFTLConf reads FTLPORT, PIDFILE and LOGFILE from pihole-FTL.conf, as
// FTL does (FTL v5.25.2 src/config.c parse_FTLconf, getpath,
// getLogFilePath): lines starting with '#' or ';' are comments, the first
// KEY= line wins, a port outside 1..65535, a path that is not absolute and
// an unreadable file give the defaults.
func ReadFTLConf(p Paths) FTLConf {
	c := FTLConf{Port: DefaultFTLPort, PIDFile: DefaultFTLPIDFile, LogFile: DefaultFTLLogFile}
	b, err := os.ReadFile(p.FTLConfPath())
	if err != nil {
		return c
	}
	var portSeen, pidSeen, logSeen bool
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "FTLPORT="); ok && !portSeen {
			portSeen = true
			if n, err := strconv.Atoi(leadingDigits(v)); err == nil && n > 0 && n <= 65535 {
				c.Port = n
			}
		}
		if v, ok := strings.CutPrefix(line, "PIDFILE="); ok && !pidSeen {
			pidSeen = true
			if fields := strings.Fields(v); len(fields) > 0 && filepath.IsAbs(fields[0]) {
				c.PIDFile = fields[0]
			}
		}
		if v, ok := strings.CutPrefix(line, "LOGFILE="); ok && !logSeen {
			logSeen = true
			if fields := strings.Fields(v); len(fields) > 0 && filepath.IsAbs(fields[0]) {
				c.LogFile = fields[0]
			}
		}
	}
	return c
}

func leadingDigits(s string) string {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// ReadBlockingEnabled returns BLOCKING_ENABLED from setupVars.conf, or nil
// when the file, the line or a true/false value is missing. Only that line
// is ever kept: every other line, WEBPASSWORD included, is discarded
// unread (FR-29). As in the shell that sources the file, the last line wins.
func ReadBlockingEnabled(p Paths) *bool {
	f, err := os.Open(p.SetupVarsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var enabled *bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		v, ok := bytes.CutPrefix(sc.Bytes(), []byte("BLOCKING_ENABLED="))
		if !ok {
			continue
		}
		switch string(bytes.TrimSpace(v)) {
		case "true":
			enabled = new(bool)
			*enabled = true
		case "false":
			enabled = new(bool)
		default:
			enabled = nil
		}
	}
	if sc.Err() != nil {
		return nil
	}
	return enabled
}

// FTLPID returns FTL's PID when the PID file names a live process whose
// /proc/<pid>/comm is pihole-FTL (A-24). It is the only liveness signal:
// `systemctl is-active pihole-FTL` stays active after FTL dies under the
// SysV script's generated unit (RemainAfterExit=yes), and nothing reads
// /etc/init.d, so this works under the native unit and the SysV one (A-55).
func FTLPID(p Paths) (int, bool) {
	b, err := os.ReadFile(p.Host(ReadFTLConf(p).PIDFile))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	comm, err := os.ReadFile(p.Host(filepath.Join(ProcDir, strconv.Itoa(pid), "comm")))
	if err != nil || strings.TrimSuffix(string(comm), "\n") != "pihole-FTL" {
		return 0, false
	}
	return pid, true
}

// ConfDirLoads reports whether /etc/dnsmasq.conf has a conf-dir= line that
// makes dnsmasq read dnsFile (V-10). Without one, `pihole-FTL --test` checks
// nothing and the DNS writes are refused. The rules are dnsmasq's (FTL
// v5.25.2 src/dnsmasq/option.c): a '#' at the start of a token begins a
// comment; spaces around '=' and ',' are allowed; the first value is the
// directory and the rest are suffixes, where "*.x" admits only names ending
// in .x and ".x" skips names ending in .x; names starting with '.', ending
// in '~' or of the form #…# are always skipped. A quoted value is not
// unquoted, so it reads as not loading: the safe direction.
func ConfDirLoads(p Paths, dnsFile string) bool {
	b, err := os.ReadFile(p.DnsmasqConfPath())
	if err != nil {
		return false
	}
	wantDir := filepath.Clean(filepath.Dir(dnsFile))
	base := filepath.Base(dnsFile)
	for _, line := range strings.Split(string(b), "\n") {
		name, arg, ok := dnsmasqOption(line)
		if !ok || name != "conf-dir" {
			continue
		}
		parts := strings.Split(arg, ",")
		dir := strings.TrimSpace(parts[0])
		if dir == "" || filepath.Clean(dir) != wantDir {
			continue
		}
		if confDirReads(base, parts[1:]) {
			return true
		}
	}
	return false
}

// dnsmasqOption tokenizes one dnsmasq.conf line into option name and value.
func dnsmasqOption(line string) (name, arg string, ok bool) {
	var b strings.Builder
	white := true
	for _, r := range line {
		if r == ' ' || r == '\t' || r == '\r' || r == '\v' || r == '\f' {
			b.WriteByte(' ')
			white = true
			continue
		}
		if white && r == '#' {
			break
		}
		b.WriteRune(r)
		white = false
	}
	s := strings.TrimSpace(b.String())
	name, arg, ok = strings.Cut(s, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(name), strings.TrimSpace(arg), true
}

// confDirReads applies dnsmasq's conf-dir name filters to base.
func confDirReads(base string, suffixes []string) bool {
	n := len(base)
	if n == 0 || base[n-1] == '~' || base[0] == '.' || (base[0] == '#' && base[n-1] == '#') {
		return false
	}
	var match, ignore []string
	for _, s := range suffixes {
		s = strings.TrimSpace(s)
		switch {
		case s == "" || s == "*":
		case strings.HasPrefix(s, "*"):
			match = append(match, s[1:])
		default:
			ignore = append(ignore, s)
		}
	}
	endsWith := func(list []string) bool {
		for _, s := range list {
			if n > len(s) && strings.HasSuffix(base, s) {
				return true
			}
		}
		return false
	}
	if len(match) > 0 && !endsWith(match) {
		return false
	}
	return !endsWith(ignore)
}

// The Nexus re-enable timer (architecture "Blocking stop"): WI 3.3 creates it
// with systemd-run --unit=nexus-pihole-reenable and a description of
// ReenableDescriptionPrefix followed by the RFC 3339 UTC time.
const (
	ReenableTimerUnit         = "nexus-pihole-reenable.timer"
	ReenableDescriptionPrefix = "Nexus: re-enable Pi-hole blocking at "
	// reenableShowTimeout bounds `systemctl show`, so Status stays within
	// its 1 s budget when systemd is slow (NFR-1).
	reenableShowTimeout = 500 * time.Millisecond
)

// ReenableTimer returns when the pending Nexus re-enable fires: only when
// the timer's SubState is waiting and its description carries the RFC 3339
// time (A-5, A-27). A native `pihole disable Ns` child is never reported
// (A-47). Every failure, including a nil runner, is nil, never an error.
func ReenableTimer(ctx context.Context, r Runner) *time.Time {
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reenableShowTimeout)
	defer cancel()
	var out bytes.Buffer
	code, err := r.Run(ctx, []string{detach.Systemctl, "show", ReenableTimerUnit, "--property=SubState,Description"}, &out, io.Discard)
	if err != nil || code != 0 {
		return nil
	}
	var waiting bool
	var at *time.Time
	for _, line := range strings.Split(out.String(), "\n") {
		key, val, _ := strings.Cut(line, "=")
		switch key {
		case "SubState":
			waiting = val == "waiting"
		case "Description":
			if ts, ok := strings.CutPrefix(val, ReenableDescriptionPrefix); ok {
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts)); err == nil {
					t = t.UTC()
					at = &t
				}
			}
		}
	}
	if !waiting {
		return nil
	}
	return at
}
