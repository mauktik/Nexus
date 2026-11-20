// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Cases: TC-38 to TC-47, TC-E4 and TC-ERR6 in
// planning/pihole-dns-zones/test-cases.md.
//
// Every test builds a temp root (v5.Paths.Root) from testdata/, whose FTL
// transcripts and versions file are verbatim copies of a real FTL v5.25.2
// capture (testdata/PROVENANCE). The FTL text API is a real TCP server on
// 127.0.0.1 that replays those transcripts; its port goes into the root's
// pihole-FTL.conf. The DNS probe target is a real UDP responder. Expected
// values are parsed from the fixtures here, never typed in.
package v5_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

const (
	statsFixture   = "ftl-v5.25.2-stats.txt"
	versionFixture = "ftl-v5.25.2-version.txt"
	systemctl      = "/usr/bin/systemctl"
	ftlPID         = 4242
)

var timerArgv = []string{systemctl, "show", "nexus-pihole-reenable.timer", "--property=SubState,Description"}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// kv parses the space-separated `key value` lines of an FTL transcript up to
// ---EOM---, independently of the package under test.
func kv(t *testing.T, transcript []byte) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(string(transcript), "\n") {
		if line == "---EOM---" {
			return m
		}
		k, v, ok := strings.Cut(line, " ")
		if ok {
			m[k] = v
		}
	}
	t.Fatal("transcript has no ---EOM---")
	return nil
}

// keyvals parses KEY=VALUE lines (versions, setupVars).
func keyvals(t *testing.T, b []byte) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// host is a temp root laid out like a v5 install.
type host struct {
	t     *testing.T
	paths v5.Paths
}

func newHost(t *testing.T) *host {
	t.Helper()
	return &host{t: t, paths: v5.Paths{Root: t.TempDir()}}
}

func (h *host) put(abs string, b []byte) { writeFile(h.t, h.paths.Host(abs), b) }

func (h *host) versionsFixture() { h.put("/etc/pihole/versions", fixture(h.t, "versions")) }
func (h *host) setupVarsFixture() {
	h.put("/etc/pihole/setupVars.conf", fixture(h.t, "setupVars.conf"))
}

func (h *host) ftlConf(port int) {
	h.put("/etc/pihole/pihole-FTL.conf", []byte("PRIVACYLEVEL=0\nFTLPORT="+strconv.Itoa(port)+"\nPIDFILE=/run/pihole-FTL.pid\n"))
}

// ftlProcess writes the PID file and the /proc/<pid>/comm entry.
func (h *host) ftlProcess(pid int, comm string) {
	h.put("/run/pihole-FTL.pid", []byte(strconv.Itoa(pid)+"\n"))
	if comm != "" {
		h.put("/proc/"+strconv.Itoa(pid)+"/comm", []byte(comm+"\n"))
	}
}

// gravity builds gravity.db from Pi-hole core v5.18.4's template, then seeds.
func (h *host) gravity(seed ...string) {
	h.t.Helper()
	schema, err := os.ReadFile(filepath.Join("..", "gravitydb", "testdata", "gravity.db.sql"))
	if err != nil {
		h.t.Fatal(err)
	}
	path := h.paths.Host("/etc/pihole/gravity.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rwc")
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		h.t.Fatalf("load schema: %v", err)
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			h.t.Fatalf("seed %q: %v", s, err)
		}
	}
}

// fakeFTL is a TCP server speaking FTL's text API (V-1): it answers each
// `>cmd` line with the canned reply and closes on `>quit`.
type fakeFTL struct {
	port   int
	mu     sync.Mutex
	recvd  []string // bytes received, one entry per connection
	silent bool
}

func serveFTL(t *testing.T, replies map[string][]byte, silent bool) *fakeFTL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFTL{port: ln.Addr().(*net.TCPAddr).Port, silent: silent}
	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() {
		close(done)
		ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.handle(c, replies, done)
			}()
		}
	}()
	return f
}

func (f *fakeFTL) handle(c net.Conn, replies map[string][]byte, done <-chan struct{}) {
	defer c.Close()
	go func() {
		<-done
		c.Close()
	}()
	idx := f.newConn()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		f.record(idx, line)
		if err != nil {
			return
		}
		if f.silent {
			continue
		}
		cmd := strings.TrimSpace(line)
		if cmd == ">quit" {
			return
		}
		if reply, ok := replies[cmd]; ok {
			if _, err := c.Write(reply); err != nil {
				return
			}
		}
	}
}

func (f *fakeFTL) newConn() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recvd = append(f.recvd, "")
	return len(f.recvd) - 1
}

func (f *fakeFTL) record(i int, s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recvd[i] += s
}

func (f *fakeFTL) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.recvd)
}

func (f *fakeFTL) addr() netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(f.port))
}

func standardFTL(t *testing.T) *fakeFTL {
	return serveFTL(t, map[string][]byte{
		">stats":   fixture(t, statsFixture),
		">version": fixture(t, versionFixture),
	}, false)
}

// refusedPort returns a loopback port nothing listens on.
func refusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// serveDNS answers every A query NOERROR with 127.0.0.1, or never answers.
func serveDNS(t *testing.T, silent bool) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		pc.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if silent {
				continue
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: true, RecursionAvailable: true})
			if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil ||
				b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET}, dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}) != nil {
				continue
			}
			out, err := b.Finish()
			if err != nil {
				continue
			}
			if _, err := pc.WriteTo(out, from); err != nil {
				return
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

// runner is a v5.Runner whose answer is fn; it records every argv.
type runner struct {
	mu    sync.Mutex
	argvs [][]string
	fn    func(argv []string, stdout io.Writer) (int, error)
}

func (r *runner) Run(_ context.Context, argv []string, stdout, _ io.Writer) (int, error) {
	r.mu.Lock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.mu.Unlock()
	return r.fn(argv, stdout)
}

func (r *runner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.argvs)
}

func timerRunner(out string, code int, err error) *runner {
	return &runner{fn: func(_ []string, stdout io.Writer) (int, error) {
		if _, werr := io.WriteString(stdout, out); werr != nil {
			return -1, werr
		}
		return code, err
	}}
}

const reenableRFC3339 = "2026-11-13T10:15:30Z"

func waitingTimer() *runner {
	return timerRunner("SubState=waiting\nDescription=Nexus: re-enable Pi-hole blocking at "+reenableRFC3339+"\n", 0, nil)
}

// fullHost is the TC-38 root: every source present and FTL up.
func fullHost(t *testing.T) (*host, *fakeFTL, netip.AddrPort) {
	h := newHost(t)
	ftl := standardFTL(t)
	h.versionsFixture()
	h.setupVarsFixture()
	h.ftlConf(ftl.port)
	h.ftlProcess(ftlPID, "pihole-FTL")
	h.gravity(
		"INSERT INTO info (property, value) VALUES ('gravity_count', '165493'), ('updated', '1720203654')",
		"INSERT INTO domainlist (type, domain, comment) VALUES (1, 'ads.example.com', 'deny one')",
		"INSERT INTO adlist (address, comment, number, status, date_updated) VALUES ('https://lists.example.com/hosts', 'list one', 42, 1, 1720203654)",
	)
	return h, ftl, serveDNS(t, false)
}

// TC-38: Status combines the versions file, the FTL PID, the DNS probe,
// setupVars, the timer and gravity.db info.
func TestTC38_StatusCombinesSources(t *testing.T) {
	h, _, dns := fullHost(t)
	r := &v5.Reader{Paths: h.paths, Runner: waitingTimer(), DNSServer: dns}
	got, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	vers := keyvals(t, fixture(t, "versions"))
	blocking := map[string]string{"true": "enabled", "false": "disabled"}[keyvals(t, fixture(t, "setupVars.conf"))["BLOCKING_ENABLED"]]
	reenable, err := time.Parse(time.RFC3339, reenableRFC3339)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Unix(1720203654, 0).UTC()
	count := int64(165493)
	tru := true
	want := spec.StatusProperties{
		Backend:        spec.BackendV5CLI,
		CoreVersion:    ptr(vers["CORE_VERSION"]),
		WebVersion:     ptr(vers["WEB_VERSION"]),
		FTLVersion:     ptr(vers["FTL_VERSION"]),
		FTLRunning:     &tru,
		DNSAnswering:   &tru,
		Blocking:       blocking,
		ReenableAt:     &reenable,
		GravityDomains: &count,
		GravityUpdated: &updated,
	}
	if blocking == "" {
		t.Fatal("setupVars fixture has no BLOCKING_ENABLED")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Status:\n got %s\nwant %s", js(t, got), js(t, want))
	}
}

func ptr[T any](v T) *T { return &v }

func js(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TC-38 (per field): FTL liveness is the PID file plus /proc/<pid>/comm.
func TestFTLPIDNeedsPiholeFTLComm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pidFile string
		comm    string
		wantOK  bool
	}{
		{"live pihole-FTL", "4242\n", "pihole-FTL", true},
		{"other process", "4242\n", "bash", false},
		{"dead pid", "4242\n", "", false},
		{"garbage pid file", "not-a-pid\n", "pihole-FTL", false},
		{"no pid file", "", "pihole-FTL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			if tc.pidFile != "" {
				h.put("/run/pihole-FTL.pid", []byte(tc.pidFile))
			}
			if tc.comm != "" {
				h.put("/proc/4242/comm", []byte(tc.comm+"\n"))
			}
			pid, ok := v5.FTLPID(h.paths)
			if ok != tc.wantOK || (ok && pid != 4242) {
				t.Errorf("FTLPID = (%d, %v), want ok=%v", pid, ok, tc.wantOK)
			}
		})
	}
	t.Run("PIDFILE from pihole-FTL.conf", func(t *testing.T) {
		h := newHost(t)
		h.put("/etc/pihole/pihole-FTL.conf", []byte("PIDFILE=/var/run/custom.pid\n"))
		h.put("/var/run/custom.pid", []byte("77\n"))
		h.put("/proc/77/comm", []byte("pihole-FTL\n"))
		if pid, ok := v5.FTLPID(h.paths); !ok || pid != 77 {
			t.Errorf("FTLPID = (%d, %v), want (77, true)", pid, ok)
		}
	})
}

func TestReadFTLConfDefaults(t *testing.T) {
	h := newHost(t)
	if got := v5.ReadFTLConf(h.paths); got.Port != 4711 || got.PIDFile != "/run/pihole-FTL.pid" {
		t.Errorf("missing file: %+v, want port 4711 and /run/pihole-FTL.pid", got)
	}
	h.put("/etc/pihole/pihole-FTL.conf", []byte("#FTLPORT=1\nFTLPORT=99999\n"))
	if got := v5.ReadFTLConf(h.paths); got.Port != 4711 {
		t.Errorf("out-of-range FTLPORT: port %d, want 4711", got.Port)
	}
	h.put("/etc/pihole/pihole-FTL.conf", []byte("FTLPORT=4712\n"))
	if got := v5.ReadFTLConf(h.paths); got.Port != 4712 {
		t.Errorf("FTLPORT=4712: port %d", got.Port)
	}
}

// TC-E15 (NFR-7, A-65): FTL's log is /var/log/pihole/FTL.log unless
// pihole-FTL.conf's first LOGFILE= names another, as FTL v5.25.2 reads it
// (src/config.c getLogFilePath); the path resolves under Paths.Root.
func TestTCE15_FTLLogPath(t *testing.T) {
	h := newHost(t)
	if got := v5.ReadFTLConf(h.paths).LogFile; got != "/var/log/pihole/FTL.log" {
		t.Errorf("no pihole-FTL.conf: LogFile %q", got)
	}
	if got, want := h.paths.FTLLogPath(), filepath.Join(h.paths.Root, "var/log/pihole/FTL.log"); got != want {
		t.Errorf("FTLLogPath %q, want %q", got, want)
	}
	h.put("/etc/pihole/pihole-FTL.conf", []byte("PRIVACYLEVEL=0\n#LOGFILE=/tmp/commented.log\n"))
	if got := v5.ReadFTLConf(h.paths).LogFile; got != "/var/log/pihole/FTL.log" {
		t.Errorf("no LOGFILE= line: LogFile %q", got)
	}
	h.put("/etc/pihole/pihole-FTL.conf", []byte("LOGFILE=/var/log/alt/FTL.log\n"))
	if got := v5.ReadFTLConf(h.paths).LogFile; got != "/var/log/alt/FTL.log" {
		t.Errorf("one LOGFILE=: LogFile %q", got)
	}
	h.put("/etc/pihole/pihole-FTL.conf", []byte("LOGFILE=/var/log/first.log\nLOGFILE=/var/log/second.log\n"))
	if got := v5.ReadFTLConf(h.paths).LogFile; got != "/var/log/first.log" {
		t.Errorf("two LOGFILE=: LogFile %q, want the first", got)
	}
	if got, want := h.paths.FTLLogPath(), filepath.Join(h.paths.Root, "var/log/first.log"); got != want {
		t.Errorf("FTLLogPath %q, want %q", got, want)
	}
	if got := (v5.Paths{}).FTLLogPath(); got != "/var/log/pihole/FTL.log" {
		t.Errorf("production FTLLogPath %q", got)
	}
}

// TC-38 (per field): blocking from setupVars, else >stats status, else unknown.
func TestBlockingSources(t *testing.T) {
	ctx := context.Background()
	stats := kv(t, fixture(t, statsFixture))
	for _, tc := range []struct {
		name      string
		setupVars string
		ftlUp     bool
		want      string
	}{
		{"setupVars true", "BLOCKING_ENABLED=true\n", true, "enabled"},
		{"setupVars false", "BLOCKING_ENABLED=false\n", true, "disabled"},
		{"setupVars without the key falls back to >stats", "QUERY_LOGGING=true\n", true, stats["status"]},
		{"setupVars garbage value falls back to >stats", "BLOCKING_ENABLED=maybe\n", true, stats["status"]},
		{"no source", "QUERY_LOGGING=true\n", false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.put("/etc/pihole/setupVars.conf", []byte(tc.setupVars))
			port := refusedPort(t)
			if tc.ftlUp {
				port = standardFTL(t).port
			}
			h.ftlConf(port)
			r := &v5.Reader{Paths: h.paths, Runner: timerRunner("SubState=dead\n", 0, nil)}
			got, err := r.Blocking(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.want || got.ReenableAt != nil {
				t.Errorf("Blocking = %+v, want state %q and no reenableAt", got, tc.want)
			}
		})
	}
}

// TC-39: Summary takes its fields from >stats.
func TestTC39_SummaryReadsStats(t *testing.T) {
	h := newHost(t)
	ftl := standardFTL(t)
	h.ftlConf(ftl.port)
	stats := kv(t, fixture(t, statsFixture))
	pct, err := strconv.ParseFloat(stats["ads_percentage_today"], 64)
	if err != nil {
		t.Fatal(err)
	}
	want := spec.SummaryProperties{
		QueriesToday:    ptr(mustInt(t, stats["dns_queries_today"])),
		BlockedToday:    ptr(mustInt(t, stats["ads_blocked_today"])),
		BlockedPercent:  &pct,
		UniqueClients:   ptr(mustInt(t, stats["unique_clients"])),
		GravityDomains:  ptr(mustInt(t, stats["domains_being_blocked"])),
		FTLAPIReachable: true,
	}
	r := &v5.Reader{Paths: h.paths}
	got, err := r.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Summary:\n got %s\nwant %s", js(t, got), js(t, want))
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(js(t, got)), &keys); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"blockedPercent", "blockedToday", "ftlApiReachable", "gravityDomains", "queriesToday", "uniqueClients"}
	gotKeys := make([]string, 0, len(keys))
	for k := range keys {
		gotKeys = append(gotKeys, k)
	}
	slices.Sort(gotKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Errorf("summary fields %v, want exactly %v (no per-client or per-domain field, OOS-1)", gotKeys, wantKeys)
	}
}

// TC-39: without domains_being_blocked, gravityDomains is info.gravity_count.
func TestSummaryGravityFallsBackToInfo(t *testing.T) {
	h := newHost(t)
	ftl := serveFTL(t, map[string][]byte{">stats": []byte("dns_queries_today 3\n---EOM---\n\n")}, false)
	h.ftlConf(ftl.port)
	h.gravity("INSERT INTO info (property, value) VALUES ('gravity_count', '1234')")
	got, err := (&v5.Reader{Paths: h.paths}).Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.GravityDomains == nil || *got.GravityDomains != 1234 || got.QueriesToday == nil || *got.QueriesToday != 3 {
		t.Errorf("Summary = %s, want gravityDomains 1234 from info and queriesToday 3", js(t, got))
	}
}

func writeVersions(h *host, s string) { h.put("/etc/pihole/versions", []byte(s)) }

// TC-40: version detection precedence, then Select.
func TestTC40_VersionDetectionPrecedence(t *testing.T) {
	ctx := context.Background()
	ftlVersion := kv(t, fixture(t, versionFixture))["version"]
	vers := keyvals(t, fixture(t, "versions"))
	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, h *host)
		wantMajor int
		wantCore  *string
		wantFTL   *string
	}{
		{
			name: "captured versions file: GITHUB_* ignored",
			setup: func(t *testing.T, h *host) {
				h.versionsFixture()
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 5, wantCore: ptr(vers["CORE_VERSION"]), wantFTL: ptr(vers["FTL_VERSION"]),
		},
		{
			name: "CORE_VERSION=v5.18.4",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "CORE_VERSION=v5.18.4\n")
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 5, wantCore: ptr("v5.18.4"),
		},
		{
			name: "only FTL_VERSION=v5.25.2",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "FTL_VERSION=v5.25.2\n")
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 5, wantFTL: ptr("v5.25.2"),
		},
		{
			name:      "no file, FTL >version",
			setup:     func(t *testing.T, h *host) { h.ftlConf(standardFTL(t).port) },
			wantMajor: 5, wantFTL: ptr(ftlVersion),
		},
		{
			name: "no file, FTL down, pihole.toml present",
			setup: func(t *testing.T, h *host) {
				h.ftlConf(refusedPort(t))
				h.put("/etc/pihole/pihole.toml", []byte("[misc]\n  etc_dnsmasq_d = true\n"))
			},
			wantMajor: 6,
		},
		{
			name:      "nothing",
			setup:     func(t *testing.T, h *host) { h.ftlConf(refusedPort(t)) },
			wantMajor: 0,
		},
		{
			name: "CORE_VERSION=garbage falls through to FTL_VERSION",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "CORE_VERSION=garbage\nFTL_VERSION=v5.25.2\n")
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 5, wantFTL: ptr("v5.25.2"),
		},
		{
			name: "CORE_VERSION=garbage falls through to FTL >version",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "CORE_VERSION=garbage\n")
				h.ftlConf(standardFTL(t).port)
			},
			wantMajor: 5, wantFTL: ptr(ftlVersion),
		},
		{
			name: "CORE_VERSION=garbage and no other source",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "CORE_VERSION=garbage\n")
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 0,
		},
		{
			name: "only the matched version prefix is reported",
			setup: func(t *testing.T, h *host) {
				writeVersions(h, "CORE_VERSION=v5.18.4-12-gdeadbee <script>\n")
				h.ftlConf(refusedPort(t))
			},
			wantMajor: 5, wantCore: ptr("v5.18.4"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			tc.setup(t, h)
			v := backend.Detect(ctx, h.paths, v5.FTLAPITimeout)
			if v.Major != tc.wantMajor {
				t.Errorf("Major = %d, want %d", v.Major, tc.wantMajor)
			}
			if !reflect.DeepEqual(v.Core, tc.wantCore) || !reflect.DeepEqual(v.FTL, tc.wantFTL) {
				t.Errorf("Core, FTL = %v, %v; want %v, %v", deref(v.Core), deref(v.FTL), deref(tc.wantCore), deref(tc.wantFTL))
			}
		})
	}

	t.Run("captured fixture reports v5.18.3 / v5.21 / v5.25.2", func(t *testing.T) {
		h := newHost(t)
		h.versionsFixture()
		vf, err := v5.ReadVersions(h.paths)
		if err != nil {
			t.Fatal(err)
		}
		if deref(vf.Core) != "v5.18.3" || deref(vf.Web) != "v5.21" || deref(vf.FTL) != "v5.25.2" {
			t.Errorf("ReadVersions = %s / %s / %s, want v5.18.3 / v5.21 / v5.25.2", deref(vf.Core), deref(vf.Web), deref(vf.FTL))
		}
	})

	deps := backend.Deps{Runner: timerRunner("", 0, nil), DNSServer: netip.MustParseAddrPort("127.0.0.1:53")}
	t.Run("Select v5", func(t *testing.T) {
		r, _, name, err := backend.Select(backend.Versions{Major: 5}, v5.Paths{Root: t.TempDir()}, deps)
		if err != nil || name != spec.BackendV5CLI || r == nil || r.Name() != spec.BackendV5CLI {
			t.Errorf("Select(5) = (%v, %q, %v), want the v5-cli reader", r, name, err)
		}
	})
	t.Run("Select v6", func(t *testing.T) {
		r, m, name, err := backend.Select(backend.Versions{Major: 6}, v5.Paths{}, deps)
		if err != nil || name != spec.BackendUnsupported || r == nil || m == nil || r.Name() != spec.BackendUnsupported {
			t.Errorf("Select(6) = (%v, %v, %q, %v), want the unsupported backend", r, m, name, err)
		}
	})
	for _, major := range []int{0, 4, 7} {
		r, m, name, err := backend.Select(backend.Versions{Major: major}, v5.Paths{}, deps)
		if r != nil || m != nil || name != "" || !errors.Is(err, backend.ErrNotDetected) {
			t.Errorf("Select(%d) = (%v, %v, %q, %v), want (nil, nil, \"\", ErrNotDetected)", major, r, m, name, err)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TC-41: the v6 misc.etc_dnsmasq_d line scan.
func TestTC41_V6EtcDnsmasqD(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want bool
	}{
		{"true under [misc]", "[dns]\n  upstreams = [ \"1.1.1.1\" ]\n\n[misc]\n  privacylevel = 0\n  etc_dnsmasq_d = true\n", true},
		{"false", "[misc]\n  etc_dnsmasq_d = false\n", false},
		{"commented out", "[misc]\n  # etc_dnsmasq_d = true\n", false},
		{"under another section", "[dns]\n  etc_dnsmasq_d = true\n\n[misc]\n  privacylevel = 0\n\n[misc.check]\n  etc_dnsmasq_d = true\n", false},
		{"true with a comment (FTL v6 writer form)", "[misc]\n  etc_dnsmasq_d = true ### CHANGED, default = false\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			h.put("/etc/pihole/pihole.toml", []byte(tc.toml))
			ok, reason := backend.CheckV6(h.paths)
			if ok != tc.want {
				t.Errorf("CheckV6 = %v (%q), want %v", ok, reason, tc.want)
			}
			if !ok && !strings.Contains(reason, "misc.etc_dnsmasq_d") {
				t.Errorf("reason %q does not name misc.etc_dnsmasq_d", reason)
			}
			if ok && reason != "" {
				t.Errorf("reason %q on success", reason)
			}
		})
	}
}

// TC-42: WEBPASSWORD never leaves setupVars.conf.
func TestTC42_WebPasswordNeverLeaves(t *testing.T) {
	hash := keyvals(t, fixture(t, "setupVars.conf"))["WEBPASSWORD"]
	if len(hash) != 64 {
		t.Fatalf("fixture WEBPASSWORD is %d characters, want 64", len(hash))
	}
	var logs bytes.Buffer
	prevSlog := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(&logs)
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	ctx := context.Background()
	var seen []string
	collect := func(v any, err error) {
		seen = append(seen, js(t, v))
		if err != nil {
			seen = append(seen, err.Error())
		}
	}
	for _, withDB := range []bool{true, false} {
		h := newHost(t)
		h.versionsFixture()
		h.setupVarsFixture()
		h.ftlConf(standardFTL(t).port)
		h.ftlProcess(ftlPID, "pihole-FTL")
		if withDB {
			h.gravity()
		}
		r := &v5.Reader{Paths: h.paths, Runner: waitingTimer(), DNSServer: serveDNS(t, false)}
		st, err := r.Status(ctx)
		collect(st, err)
		if st.Blocking != "disabled" {
			t.Errorf("status blocking %q, want disabled", st.Blocking)
		}
		sum, err := r.Summary(ctx)
		collect(sum, err)
		b, err := r.Blocking(ctx)
		collect(b, err)
		if b.State != "disabled" {
			t.Errorf("blocking %q, want disabled", b.State)
		}
		d, err := r.Domains(ctx)
		collect(d, err)
		a, err := r.Adlists(ctx)
		collect(a, err)
		collect(v5.ReadBlockingEnabled(h.paths), nil)
	}
	seen = append(seen, logs.String())
	for _, s := range seen {
		// Case-sensitive: t.TempDir paths carry the test's mixed-case name.
		if strings.Contains(s, hash) || strings.Contains(s, "WEBPASSWORD") {
			t.Errorf("WEBPASSWORD leaked: %q", s)
		}
	}
}

// TC-43: only a waiting Nexus timer with an RFC 3339 description counts.
func TestTC43_ReenableTimer(t *testing.T) {
	want, err := time.Parse(time.RFC3339, reenableRFC3339)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		r    *runner
		want *time.Time
	}{
		{"waiting with RFC 3339", waitingTimer(), &want},
		{"waiting, keys reversed", timerRunner("Description=Nexus: re-enable Pi-hole blocking at "+reenableRFC3339+"\nSubState=waiting\n", 0, nil), &want},
		{"dead", timerRunner("SubState=dead\nDescription=Nexus: re-enable Pi-hole blocking at "+reenableRFC3339+"\n", 0, nil), nil},
		{"waiting, malformed description", timerRunner("SubState=waiting\nDescription=Nexus: re-enable Pi-hole blocking at soon\n", 0, nil), nil},
		{"waiting, foreign description", timerRunner("SubState=waiting\nDescription=/usr/local/bin/pihole enable\n", 0, nil), nil},
		{"exit 1", timerRunner("SubState=waiting\nDescription=Nexus: re-enable Pi-hole blocking at "+reenableRFC3339+"\n", 1, nil), nil},
		{"runner error", timerRunner("", -1, errors.New("cannot start")), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := v5.ReenableTimer(context.Background(), tc.r)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReenableTimer = %v, want %v", got, tc.want)
			}
			if argvs := tc.r.recorded(); len(argvs) != 1 || !slices.Equal(argvs[0], timerArgv) {
				t.Errorf("argv %q, want %q", argvs, timerArgv)
			}
		})
	}
	if got := v5.ReenableTimer(context.Background(), nil); got != nil {
		t.Errorf("nil runner: %v, want nil", got)
	}
}

// TC-44: conf-dir in /etc/dnsmasq.conf loads dnsFile (V-10, V-11).
func TestTC44_ConfDirLoads(t *testing.T) {
	const dnsFile = "/etc/dnsmasq.d/03-custom-dns-names.conf"
	for _, tc := range []struct {
		name string
		conf *string
		want bool
	}{
		{"conf-dir=/etc/dnsmasq.d", ptr("conf-dir=/etc/dnsmasq.d\n"), true},
		{"absent file", nil, false},
		{"ignore suffix .bak", ptr("conf-dir=/etc/dnsmasq.d,.bak\n"), true},
		{"match *.conf, trailing slash", ptr("conf-dir=/etc/dnsmasq.d/,*.conf\n"), true},
		{"match *.list excludes .conf", ptr("conf-dir=/etc/dnsmasq.d/,*.list\n"), false},
		{"other directory", ptr("conf-dir=/other\n"), false},
		{"commented line", ptr("#conf-dir=/etc/dnsmasq.d\n"), false},
		{"ignore suffix .conf", ptr("conf-dir=/etc/dnsmasq.d,.conf\n"), false},
		{"spaces around = and a trailing comment", ptr("  conf-dir = /etc/dnsmasq.d  # Pi-hole\n"), true},
		{"second line loads it", ptr("conf-dir=/other\nconf-dir=/etc/dnsmasq.d\n"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHost(t)
			if tc.conf != nil {
				h.put("/etc/dnsmasq.conf", []byte(*tc.conf))
			}
			if got := v5.ConfDirLoads(h.paths, dnsFile); got != tc.want {
				t.Errorf("ConfDirLoads = %v, want %v", got, tc.want)
			}
		})
	}
	h := newHost(t)
	h.put("/etc/dnsmasq.conf", []byte("conf-dir=/etc/dnsmasq.d\n"))
	for _, name := range []string{"/etc/dnsmasq.d/.hidden.conf", "/etc/dnsmasq.d/x.conf~", "/etc/dnsmasq.d/#x.conf#"} {
		if v5.ConfDirLoads(h.paths, name) {
			t.Errorf("ConfDirLoads(%q) = true; dnsmasq skips that name (V-11)", name)
		}
	}
}

// TC-45: the Reader runs nothing but `systemctl show` for the timer.
func TestTC45_ReaderNeverExecutesPihole(t *testing.T) {
	h, _, dns := fullHost(t)
	rec := &runner{}
	rec.fn = func(argv []string, stdout io.Writer) (int, error) {
		if len(argv) < 2 || argv[0] != systemctl || argv[1] != "show" {
			t.Errorf("Reader ran %q", argv)
			return 127, nil
		}
		_, err := io.WriteString(stdout, "SubState=dead\n")
		return 0, err
	}
	r := &v5.Reader{Paths: h.paths, Runner: rec, DNSServer: dns}
	ctx := context.Background()
	if _, err := r.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Summary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Blocking(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Domains(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Adlists(ctx); err != nil {
		t.Fatal(err)
	}
	argvs := rec.recorded()
	if len(argvs) == 0 {
		t.Fatal("the timer was never read")
	}
	for _, a := range argvs {
		if !slices.Equal(a, timerArgv) {
			t.Errorf("ran %q, want only %q", a, timerArgv)
		}
	}
}

// TC-46: the FTL text-API client protocol.
func TestTC46_FTLAPIProtocol(t *testing.T) {
	ctx := context.Background()
	statsBytes := fixture(t, statsFixture)
	stats := kv(t, statsBytes)

	t.Run("stats", func(t *testing.T) {
		// Bytes after ---EOM--- must not be parsed.
		ftl := serveFTL(t, map[string][]byte{">stats": append(slices.Clone(statsBytes), "dns_queries_today 999999\n"...)}, false)
		got, err := v5.FTLStats(ctx, ftl.addr(), v5.FTLAPITimeout)
		if err != nil {
			t.Fatal(err)
		}
		if got.DNSQueriesToday == nil || *got.DNSQueriesToday != mustInt(t, stats["dns_queries_today"]) {
			t.Errorf("dns_queries_today = %v, want %s (parsing must stop at ---EOM---)", got.DNSQueriesToday, stats["dns_queries_today"])
		}
		if got.Status == nil || *got.Status != stats["status"] {
			t.Errorf("status = %v, want %s", got.Status, stats["status"])
		}
		waitReceived(t, ftl, ">stats\n>quit\n")
	})

	t.Run("version", func(t *testing.T) {
		ftl := standardFTL(t)
		got, err := v5.FTLVersion(ctx, ftl.addr(), v5.FTLAPITimeout)
		if err != nil {
			t.Fatal(err)
		}
		if want := kv(t, fixture(t, versionFixture))["version"]; got == nil || *got != want {
			t.Errorf("FTLVersion = %v, want %s", got, want)
		}
		waitReceived(t, ftl, ">version\n>quit\n")
	})

	t.Run("silent server times out at 500 ms", func(t *testing.T) {
		ftl := serveFTL(t, nil, true)
		start := time.Now()
		_, err := v5.FTLStats(ctx, ftl.addr(), v5.FTLAPITimeout)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("silent server: no error")
		}
		// The lower bound is the deadline itself; the upper one is generous
		// because a loaded host delays a timer (review CR-15).
		if elapsed < 400*time.Millisecond || elapsed > v5.FTLAPITimeout+2*time.Second {
			t.Errorf("silent server returned after %v, want about 500 ms", elapsed)
		}
	})

	t.Run("refused", func(t *testing.T) {
		addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(refusedPort(t)))
		if _, err := v5.FTLStats(ctx, addr, v5.FTLAPITimeout); err == nil {
			t.Error("refused port: no error")
		}
	})

	// Security review SEC-1: FTLPORT is unprivileged, so a local process can
	// answer in FTL's place. A reply is read only up to v5.MaxFTLReply bytes
	// and keeps only its first 64 keys.
	t.Run("an oversized reply is refused", func(t *testing.T) {
		for name, body := range map[string][]byte{
			"one line without a newline": bytes.Repeat([]byte("x"), 4*v5.MaxFTLReply),
			"many lines, EOM after the cap": append(bytes.Repeat([]byte("key value\n"), v5.MaxFTLReply/4),
				"---EOM---\n"...),
		} {
			t.Run(name, func(t *testing.T) {
				ftl := serveFTL(t, map[string][]byte{">stats": body}, false)
				start := time.Now()
				_, err := v5.FTLStats(ctx, ftl.addr(), 5*time.Second)
				if err == nil || !strings.Contains(err.Error(), "reply longer than") {
					t.Fatalf("err = %v, want a refusal for a reply over %d bytes", err, v5.MaxFTLReply)
				}
				if took := time.Since(start); took > 3*time.Second {
					t.Errorf("the refusal took %v: the cap, not the deadline, must end the read", took)
				}
			})
		}
	})

	t.Run("a reply under the cap with 1000 keys keeps the first 64", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("dns_queries_today 7\n")
		for i := range 1000 {
			fmt.Fprintf(&b, "junk%d x\n", i)
		}
		b.WriteString("ads_blocked_today 9\n---EOM---\n")
		ftl := serveFTL(t, map[string][]byte{">stats": []byte(b.String())}, false)
		got, err := v5.FTLStats(ctx, ftl.addr(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got.DNSQueriesToday == nil || *got.DNSQueriesToday != 7 {
			t.Errorf("a key among the first 64 was lost: %v", got.DNSQueriesToday)
		}
		if got.AdsBlockedToday != nil {
			t.Errorf("key 1002 was kept (%d): at most 64 keys may be", *got.AdsBlockedToday)
		}
	})

	t.Run("reply without ---EOM--- is an error", func(t *testing.T) {
		ftl := serveFTL(t, map[string][]byte{">stats": []byte("dns_queries_today 1\n")}, false)
		if _, err := v5.FTLStats(ctx, ftl.addr(), 200*time.Millisecond); err == nil {
			t.Error("truncated reply: no error")
		}
	})
}

// waitReceived waits for the server to see the client's full byte stream
// (the client may close right after writing >quit).
func waitReceived(t *testing.T, f *fakeFTL, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := f.received()
		if len(got) == 1 && got[0] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server received %q, want one connection sending %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TC-47: the unsupported backend answers capability-unavailable everywhere.
func TestTC47_UnsupportedBackend(t *testing.T) {
	ctx := context.Background()
	u := backend.Unsupported{}
	var (
		_ backend.Reader          = u
		_ backend.Mutator         = u
		_ backend.ServiceMutator  = u
		_ backend.BlockingMutator = u
		_ backend.DomainMutator   = u
		_ backend.AdlistMutator   = u
	)
	if u.Name() != spec.BackendUnsupported {
		t.Errorf("Name = %q", u.Name())
	}
	calls := map[string]error{}
	_, calls["Status"] = u.Status(ctx)
	_, calls["Summary"] = u.Summary(ctx)
	_, calls["Blocking"] = u.Blocking(ctx)
	_, calls["Domains"] = u.Domains(ctx)
	_, calls["Adlists"] = u.Adlists(ctx)
	calls["WaitExternalGravity"] = u.WaitExternalGravity(ctx, io.Discard)
	calls["RestartDNS"] = u.RestartDNS(ctx, io.Discard)
	calls["FlushCache"] = u.FlushCache(ctx, io.Discard)
	_, calls["TestConfig"] = u.TestConfig(ctx, io.Discard)
	_, calls["StopBlocking"] = u.StopBlocking(ctx, time.Minute, io.Discard)
	_, calls["StartBlocking"] = u.StartBlocking(ctx, io.Discard)
	_, _, calls["AddDomain"] = u.AddDomain(ctx, spec.DomainAddInput{}, io.Discard)
	_, calls["RemoveDomain"] = u.RemoveDomain(ctx, 1, io.Discard)
	_, _, calls["AddAdlist"] = u.AddAdlist(ctx, "https://example.com/l", "", io.Discard)
	_, calls["RemoveAdlist"] = u.RemoveAdlist(ctx, 1, io.Discard)
	_, calls["UpdateGravity"] = u.UpdateGravity(ctx, io.Discard)
	for name, err := range calls {
		var pe *problem.Error
		if !errors.As(err, &pe) {
			t.Errorf("%s: error %v is not a *problem.Error", name, err)
			continue
		}
		if pe.Slug() != problem.CapabilityUnavailable || pe.Extensions["provider"] != "pihole" || pe.Extensions["reason"] != "v6 backend not implemented" {
			t.Errorf("%s: %s %v, want capability-unavailable{provider:pihole, reason:\"v6 backend not implemented\"}", name, pe.Slug(), pe.Extensions)
		}
	}
	if pid, ok := u.FTLPID(); ok || pid != 0 {
		t.Errorf("FTLPID = (%d, %v), want (0, false)", pid, ok)
	}
}

// TC-E4: an unknown key is ignored and a missing key is null.
func TestTCE4_UnknownAndMissingKeys(t *testing.T) {
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(fixture(t, statsFixture)), "\n") {
		if strings.HasPrefix(line, "unique_clients ") {
			continue
		}
		if line == "---EOM---\n" {
			b.WriteString("future_key 1\n")
		}
		b.WriteString(line)
	}
	h := newHost(t)
	ftl := serveFTL(t, map[string][]byte{">stats": []byte(b.String())}, false)
	h.ftlConf(ftl.port)
	got, err := (&v5.Reader{Paths: h.paths}).Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.UniqueClients != nil {
		t.Errorf("uniqueClients = %d, want null", *got.UniqueClients)
	}
	if !got.FTLAPIReachable || got.QueriesToday == nil || got.BlockedToday == nil || got.BlockedPercent == nil || got.GravityDomains == nil {
		t.Errorf("other fields unset: %s", js(t, got))
	}
}

// TC-ERR6: with FTL down, Status and Summary succeed with nulls.
func TestTCERR6_FTLDown(t *testing.T) {
	h := newHost(t)
	h.versionsFixture()
	h.setupVarsFixture()
	h.ftlConf(refusedPort(t))
	h.ftlProcess(ftlPID, "") // the PID file names a dead process
	r := &v5.Reader{Paths: h.paths, Runner: timerRunner("SubState=dead\n", 0, nil), DNSServer: serveDNS(t, true)}
	ctx := context.Background()

	start := time.Now()
	st, err := r.Status(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("Status took %v, want under 1 s", elapsed)
	}
	if st.FTLRunning == nil || *st.FTLRunning || st.DNSAnswering == nil || *st.DNSAnswering {
		t.Errorf("Status = %s, want ftlRunning:false, dnsAnswering:false", js(t, st))
	}
	if st.GravityDomains != nil || st.GravityUpdated != nil || st.ReenableAt != nil {
		t.Errorf("Status = %s, want null gravity and reenableAt", js(t, st))
	}

	sum, err := r.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if !reflect.DeepEqual(sum, spec.SummaryProperties{}) {
		t.Errorf("Summary = %s, want every field null and ftlApiReachable:false", js(t, sum))
	}
}

// Domains and Adlists read gravity.db read-only.
func TestDomainsAndAdlists(t *testing.T) {
	h, _, _ := fullHost(t)
	r := &v5.Reader{Paths: h.paths}
	ctx := context.Background()
	d, err := r.Domains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0].Value != "ads.example.com" || d[0].List != "deny" || d[0].Kind != "exact" {
		t.Errorf("Domains = %s", js(t, d))
	}
	a, err := r.Adlists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].URL != "https://lists.example.com/hosts" || a[0].Domains == nil || *a[0].Domains != 42 {
		t.Errorf("Adlists = %s", js(t, a))
	}

	empty := &v5.Reader{Paths: v5.Paths{Root: t.TempDir()}}
	if _, err := empty.Domains(ctx); err == nil {
		t.Error("Domains without gravity.db: no error")
	}
	if _, err := empty.Adlists(ctx); err == nil {
		t.Error("Adlists without gravity.db: no error")
	}
}
