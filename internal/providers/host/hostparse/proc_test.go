// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"encoding/json"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The two WI 2.1 captures (internal/providers/host/testdata/README.md). Each
// has fs/ (first sample), t1/ (the dynamic files 10 s later) and tools/ (the
// host's own tools from the same run), which the tests compare with.
var captures = []string{"ubuntu-systemd", "alpine-openrc"}

func capPath(capture string, rel ...string) string {
	return filepath.Join(append([]string{"..", "testdata", capture}, rel...)...)
}

func readCap(t testing.TB, capture, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(capPath(capture, rel))
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	return b
}

func globCap(t testing.TB, capture, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(capPath(capture, pattern))
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	return m
}

// textLines splits b into lines without terminators and without the empty
// string after a final newline.
func textLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func mustU64(t testing.TB, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("test oracle: %q is not a uint64: %v", s, err)
	}
	return v
}

func mustF64(t testing.TB, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("test oracle: %q is not a float: %v", s, err)
	}
	return v
}

// toolNproc is tools/nproc.
func toolNproc(t testing.TB, capture string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(string(readCap(t, capture, "tools/nproc"))))
	if err != nil {
		t.Fatalf("tools/nproc: %v", err)
	}
	return n
}

// toolFree is `free -b`: the Mem total and available columns and the Swap
// total.
func toolFree(t testing.TB, capture string) (memTotal, memAvailable, swapTotal uint64) {
	t.Helper()
	lines := textLines(readCap(t, capture, "tools/free"))
	header := strings.Fields(lines[0])
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i + 1 // data rows start with the "Mem:" label
			}
		}
		t.Fatalf("tools/free: no %q column in %q", name, lines[0])
		return 0
	}
	var memSeen, swapSeen bool
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		switch f[0] {
		case "Mem:":
			memTotal, memAvailable, memSeen = mustU64(t, f[col("total")]), mustU64(t, f[col("available")]), true
		case "Swap:":
			swapTotal, swapSeen = mustU64(t, f[col("total")]), true
		}
	}
	if !memSeen || !swapSeen {
		t.Fatalf("tools/free: Mem or Swap row missing")
	}
	return memTotal, memAvailable, swapTotal
}

// toolVmstatBusy is 100 - id - wa from the last line of `vmstat 10 2`, the
// line that covers the fs/ to t1/ interval.
func toolVmstatBusy(t testing.TB, capture string) float64 {
	t.Helper()
	lines := textLines(readCap(t, capture, "tools/vmstat"))
	header := strings.Fields(lines[1])
	last := strings.Fields(lines[len(lines)-1])
	if len(header) != len(last) {
		t.Fatalf("tools/vmstat: header %q and row %q differ in width", lines[1], lines[len(lines)-1])
	}
	var id, wa = -1, -1
	for i, h := range header {
		switch h {
		case "id":
			id = i
		case "wa":
			wa = i
		}
	}
	if id < 0 || wa < 0 {
		t.Fatalf("tools/vmstat: no id/wa column in %q", lines[1])
	}
	return 100 - mustF64(t, last[id]) - mustF64(t, last[wa])
}

type ipRouteEntry struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
	Metric  uint32 `json:"metric"`
}

// toolIPRoute is `ip -j route`.
func toolIPRoute(t testing.TB, capture string) []ipRouteEntry {
	t.Helper()
	var out []ipRouteEntry
	if err := json.Unmarshal(readCap(t, capture, "tools/ip-route"), &out); err != nil {
		t.Fatalf("tools/ip-route: %v", err)
	}
	return out
}

// toolIfNames is the ifname of every link of `ip -j addr`.
func toolIfNames(t testing.TB, capture string) []string {
	t.Helper()
	var links []struct {
		IfName string `json:"ifname"`
	}
	if err := json.Unmarshal(readCap(t, capture, "tools/ip-addr"), &links); err != nil {
		t.Fatalf("tools/ip-addr: %v", err)
	}
	var out []string
	for _, l := range links {
		out = append(out, l.IfName)
	}
	return out
}

// toolDfTargets is every mount point of tools/df: the first column of GNU
// `df --output=target,...`, the last of BusyBox `df -B1`.
func toolDfTargets(t testing.TB, capture string) []string {
	t.Helper()
	lines := textLines(readCap(t, capture, "tools/df"))
	first := strings.HasPrefix(lines[0], "Mounted on")
	var out []string
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if first {
			out = append(out, f[0])
		} else {
			out = append(out, f[len(f)-1])
		}
	}
	return out
}

func TestUnescapeOctal(t *testing.T) {
	for in, want := range map[string]string{
		`/a\040b`:     "/a b",
		`/t\011n\012`: "/t\tn\n",
		`/bs\134x`:    `/bs\x`,
		`/hash\043`:   "/hash#",
		`/short\04`:   `/short\04`,
		`/bad\089`:    `/bad\089`,
		`/over\777`:   `/over\777`,
		`/end\`:       `/end\`,
		`plain`:       "plain",
		``:            "",
	} {
		if got := unescapeOctal(in); got != want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Metric column of /proc/net/route is a u32 printed with %d
// (fib_trie.c fib_route_seq_show), so a priority above 2^31-1 shows as a
// negative number.
func TestRouteV4MetricPrintedNegative(t *testing.T) {
	b := []byte("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t-1\t00000000\t0\t0\t0\n" +
		"eth1\t00000000\t0201A8C0\t0003\t0\t0\t4294967296\t00000000\t0\t0\t0\n")
	got, err := RouteV4(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []DefaultRoute{{Gateway: netip.MustParseAddr("192.168.1.1"), Interface: "eth0", Metric: math.MaxUint32}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RouteV4 = %+v, want %+v", got, want)
	}
}

func TestParseFloatRejectsNonFinite(t *testing.T) {
	for _, s := range []string{"NaN", "Inf", "-Inf", "+Inf", "1e400"} {
		if _, err := Uptime([]byte(s + " 1.00\n")); err == nil {
			t.Errorf("Uptime(%q) accepted a non-finite value", s)
		}
	}
	if _, err := Uptime([]byte("-1.00 1.00\n")); err == nil {
		t.Errorf("Uptime accepted a negative uptime")
	}
}

func TestDiskStatBytes(t *testing.T) {
	d := DiskStat{SectorsRead: 3, SectorsWritten: 5}
	if d.BytesRead() != 3*512 || d.BytesWritten() != 5*512 {
		t.Fatalf("BytesRead/BytesWritten = %d/%d, want %d/%d", d.BytesRead(), d.BytesWritten(), 3*512, 5*512)
	}
}

func TestPidStatRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",
		"1 systemd S 0",
		"1 (systemd S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704 3738",
		"1 )systemd( S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704 3738",
		"x (systemd) S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704 3738",
		"1 (systemd) S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704",
	} {
		if _, err := PidStat([]byte(s)); err == nil {
			t.Errorf("PidStat(%q) returned no error", s)
		}
	}
}
