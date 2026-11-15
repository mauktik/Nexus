// Tests of planning/system-provider/test-cases.md TC-HP-28, TC-HP-29, TC-HP-30, TC-HP-31, TC-HP-32, TC-HP-33, TC-HP-34, TC-HP-35, TC-HP-36, TC-HP-37, TC-HP-38, TC-HP-39, TC-HP-E8, TC-HP-E9.
// Owner WI: 2.3. The parsers are checked against the WI 2.1 captures and the
// same hosts' tool outputs (external-test-vectors), plus synthetic edge lines.

package hostparse

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// x86Cpuinfo is two x86 processor stanzas (both captures are aarch64, which
// has no model name, cpu MHz or flags).
const x86Cpuinfo = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz
cpu MHz		: 2399.998
flags		: fpu vme de pse hypervisor lahf_lm

processor	: 1
vendor_id	: GenuineIntel
model name	: Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz
cpu MHz		: 2400.125
flags		: fpu vme de pse hypervisor lahf_lm
`

// ipv6DefaultRoute is an ipv6_route file in the kernel's format
// (ip6_fib.c ipv6_route_native_seq_show): a default route via fe80::1 on
// eth0 with metric 1024 and RTF_UP|RTF_GATEWAY, then routes that must be
// excluded: a default without RTF_GATEWAY, a /64 with a gateway, and the
// loopback reject route.
const ipv6DefaultRoute = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth1
20010db8000000000000000000000000 40 00000000000000000000000000000000 00 fe800000000000000000000000000002 00000100 00000001 00000000 00000003     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
`

// TC-HP-28: ProcStat parses CPU lines and btime from both captures. Owner WI 2.3. Requirements: FR-6, FR-11, NFR-4.
//
// - Setup: both captures' `fs/proc/stat`, `tools/nproc`.
// - Steps: `ProcStat(b)`.
// - Expected: the number of per-CPU entries equals `tools/nproc`; the total line's user/nice/system/idle/iowait/irq/softirq/steal equal the file's `cpu ` fields 1–8; guest and guest_nice are not added; `btime` equals the file's `btime` value.
func TestTCHP28_ProcStatParsesCPULinesAndBtimeFrom(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			b := readCap(t, c, "fs/proc/stat")
			st, err := ProcStat(b)
			if err != nil {
				t.Fatalf("ProcStat: %v", err)
			}
			if n := toolNproc(t, c); len(st.CPUs) != n {
				t.Errorf("per-CPU entries = %d, tools/nproc = %d", len(st.CPUs), n)
			}
			for i, cpu := range st.CPUs {
				if cpu.ID != i {
					t.Errorf("CPUs[%d].ID = %d", i, cpu.ID)
				}
			}
			var wantTotal []uint64
			var wantBtime uint64
			for _, l := range textLines(b) {
				f := strings.Fields(l)
				switch f[0] {
				case "cpu":
					for _, s := range f[1:9] {
						wantTotal = append(wantTotal, mustU64(t, s))
					}
				case "btime":
					wantBtime = mustU64(t, f[1])
				}
			}
			tot := st.Total
			got := []uint64{tot.User, tot.Nice, tot.System, tot.Idle, tot.IOWait, tot.IRQ, tot.SoftIRQ, tot.Steal}
			if !slices.Equal(got, wantTotal) {
				t.Errorf("total = %v, file's cpu fields 1-8 = %v", got, wantTotal)
			}
			if !st.HasBootTime || st.BootTime != wantBtime {
				t.Errorf("btime = %d (present %v), file = %d", st.BootTime, st.HasBootTime, wantBtime)
			}
		})
	}
	t.Run("guest not added", func(t *testing.T) {
		st, err := ProcStat([]byte("cpu  10 20 30 40 50 60 70 80 900 1000\ncpu0 10 20 30 40 50 60 70 80 900 1000\nbtime 5\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := CPUTimes{User: 10, Nice: 20, System: 30, Idle: 40, IOWait: 50, IRQ: 60, SoftIRQ: 70, Steal: 80}
		if st.Total != want || st.CPUs[0].Times != want {
			t.Errorf("total = %+v, cpu0 = %+v, want %+v", st.Total, st.CPUs[0].Times, want)
		}
		if got := st.Total.Total(); got != 360 {
			t.Errorf("Total() = %d, want 360 (guest 900 and guest_nice 1000 already counted in user and nice)", got)
		}
		if got := st.Total.Busy(); got != 360-40-50 {
			t.Errorf("Busy() = %d, want %d", got, 360-40-50)
		}
	})
	t.Run("btime absent", func(t *testing.T) {
		st, err := ProcStat([]byte("cpu  1 2 3 4\n"))
		if err != nil {
			t.Fatal(err)
		}
		if st.HasBootTime {
			t.Errorf("HasBootTime with no btime line")
		}
		if st.Total != (CPUTimes{User: 1, Nice: 2, System: 3, Idle: 4}) {
			t.Errorf("4-column cpu line = %+v", st.Total)
		}
	})
}

// TC-HP-29: Meminfo values are bytes and agree with free. Owner WI 2.3. Requirements: FR-6, NFR-3.
//
// - Setup: both captures' `fs/proc/meminfo`, `tools/free` (`free -b`).
// - Steps: `Meminfo(b)`.
// - Expected: each needed key equals its kB value × 1024; `MemTotal` equals `free -b` total exactly; `MemAvailable` within ±1% of `free -b` available; `SwapTotal` equals free's swap total.
func TestTCHP29_MeminfoValuesAreBytesAndAgreeWith(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			b := readCap(t, c, "fs/proc/meminfo")
			m, err := Meminfo(b)
			if err != nil {
				t.Fatalf("Meminfo: %v", err)
			}
			fileKB := map[string]uint64{}
			for _, l := range textLines(b) {
				k, rest, _ := strings.Cut(l, ":")
				fileKB[k] = mustU64(t, strings.Fields(rest)[0])
			}
			for _, k := range meminfoKeys {
				kb, ok := fileKB[k]
				if !ok {
					t.Fatalf("capture has no %s line", k)
				}
				if got, ok := m[k]; !ok || got != kb*1024 {
					t.Errorf("%s = %d (present %v), want %d kB × 1024 = %d", k, got, ok, kb, kb*1024)
				}
			}
			if len(m) != len(meminfoKeys) {
				t.Errorf("Meminfo has %d keys, want only the %d needed: %v", len(m), len(meminfoKeys), m)
			}
			total, avail, swap := toolFree(t, c)
			if m["MemTotal"] != total {
				t.Errorf("MemTotal = %d, free -b total = %d", m["MemTotal"], total)
			}
			if d := math.Abs(float64(m["MemAvailable"]) - float64(avail)); d > 0.01*float64(avail) {
				t.Errorf("MemAvailable = %d, free -b available = %d (off by %.0f, over 1%%)", m["MemAvailable"], avail, d)
			}
			if m["SwapTotal"] != swap {
				t.Errorf("SwapTotal = %d, free -b swap total = %d", m["SwapTotal"], swap)
			}
		})
	}
}

// TC-HP-30: Loadavg and Uptime parse the captured lines. Owner WI 2.3. Requirements: FR-4, FR-6.
//
// - Setup: both captures.
// - Steps: parse `proc/loadavg`, `proc/uptime`.
// - Expected: load1/5/15 equal the first three fields as float64; uptime seconds equal the first field; `tools/vmstat`'s run time is irrelevant here (no cross-check).
func TestTCHP30_LoadavgAndUptimeParseTheCapturedLines(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			b := readCap(t, c, "fs/proc/loadavg")
			la, err := Loadavg(b)
			if err != nil {
				t.Fatalf("Loadavg: %v", err)
			}
			f := strings.Fields(string(b))
			want := LoadAverage{Load1: mustF64(t, f[0]), Load5: mustF64(t, f[1]), Load15: mustF64(t, f[2])}
			if la != want {
				t.Errorf("Loadavg = %+v, want %+v", la, want)
			}
			b = readCap(t, c, "fs/proc/uptime")
			up, err := Uptime(b)
			if err != nil {
				t.Fatalf("Uptime: %v", err)
			}
			if w := mustF64(t, strings.Fields(string(b))[0]); up != w {
				t.Errorf("Uptime = %v, want %v", up, w)
			}
		})
	}
	la, err := Loadavg([]byte("1.50 0.25 12.00 3/229 1012\n"))
	if err != nil || la != (LoadAverage{1.5, 0.25, 12}) {
		t.Errorf("Loadavg synthetic = %+v, %v", la, err)
	}
}

// TC-HP-31: Vmstat distinguishes oom_kill absent from zero. Owner WI 2.3. Requirements: FR-10.
//
// - Setup: both captures' `proc/vmstat`; the same with the `oom_kill` line removed; one with `oom_kill 0`.
// - Steps: `Vmstat(b)`.
// - Expected: capture value and present=true; removed → present=false; `0` → value 0, present=true.
func TestTCHP31_VmstatDistinguishesOomKillAbsentFromZero(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			lines := textLines(readCap(t, c, "fs/proc/vmstat"))
			var want uint64
			var without, seven []string
			found := false
			for _, l := range lines {
				if f := strings.Fields(l); f[0] == "oom_kill" {
					want, found = mustU64(t, f[1]), true
					seven = append(seven, "oom_kill 7")
					continue
				}
				without = append(without, l)
				seven = append(seven, l)
			}
			if !found {
				t.Fatalf("capture has no oom_kill line")
			}
			v, err := Vmstat([]byte(strings.Join(lines, "\n") + "\n"))
			if err != nil || !v.OOMKillPresent || v.OOMKill != want {
				t.Errorf("capture: %+v, %v; want %d present", v, err, want)
			}
			v, err = Vmstat([]byte(strings.Join(without, "\n") + "\n"))
			if err != nil || v.OOMKillPresent || v.OOMKill != 0 {
				t.Errorf("removed: %+v, %v; want absent", v, err)
			}
			v, err = Vmstat([]byte(strings.Join(seven, "\n") + "\n"))
			if err != nil || !v.OOMKillPresent || v.OOMKill != 7 {
				t.Errorf("oom_kill 7: %+v, %v", v, err)
			}
		})
	}
	v, err := Vmstat([]byte("nr_free_pages 12\noom_kill 0\n"))
	if err != nil || !v.OOMKillPresent || v.OOMKill != 0 {
		t.Errorf("oom_kill 0: %+v, %v; want 0 present", v, err)
	}
}

// TC-HP-32: Pressure parses some and full, with full absent on older kernels. Owner WI 2.3. Requirements: FR-6.
//
// - Setup: captured `proc/pressure/{cpu,memory,io}` where present; a copy of `cpu` without its `full` line.
// - Steps: `Pressure(b)`.
// - Expected: avg10/avg60/avg300 equal the file's values; the copy without `full` returns `Full == nil`, `Some` set.
func TestTCHP32_PressureParsesSomeAndFullWithFull(t *testing.T) {
	oracle := func(t *testing.T, l string) PressureLine {
		var p PressureLine
		for _, kv := range strings.Fields(l)[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "avg10":
				p.Avg10 = mustF64(t, v)
			case "avg60":
				p.Avg60 = mustF64(t, v)
			case "avg300":
				p.Avg300 = mustF64(t, v)
			case "total":
				p.Total = mustU64(t, v)
			}
		}
		return p
	}
	for _, c := range captures {
		for _, res := range []string{"cpu", "memory", "io"} {
			t.Run(c+"/"+res, func(t *testing.T) {
				matches := globCap(t, c, "fs/proc/pressure/"+res)
				if len(matches) == 0 {
					t.Skipf("no proc/pressure/%s in %s", res, c)
				}
				b := readCap(t, c, "fs/proc/pressure/"+res)
				p, err := Pressure(b)
				if err != nil {
					t.Fatalf("Pressure: %v", err)
				}
				var some []string
				for _, l := range textLines(b) {
					want := oracle(t, l)
					var got *PressureLine
					switch strings.Fields(l)[0] {
					case "some":
						got = p.Some
						some = append(some, l)
					case "full":
						got = p.Full
					}
					if got == nil || *got != want {
						t.Errorf("%q parsed as %+v", l, got)
					}
				}
				q, err := Pressure([]byte(strings.Join(some, "\n") + "\n"))
				if err != nil || q.Some == nil || q.Full != nil || *q.Some != *p.Some {
					t.Errorf("without full: %+v, %v; want Some=%+v Full=nil", q, err, p.Some)
				}
			})
		}
	}
	p, err := Pressure([]byte("some avg10=1.50 avg60=0.25 avg300=12.00 total=42\n"))
	if err != nil || p.Full != nil || *p.Some != (PressureLine{1.5, 0.25, 12, 42}) {
		t.Errorf("synthetic: %+v, %v", p, err)
	}
}

// TC-HP-33: Cpuinfo gives the model, per-processor MHz and the hypervisor flag. Owner WI 2.3. Requirements: FR-5, FR-6.
//
// - Setup: both captures' `proc/cpuinfo`.
// - Steps: `Cpuinfo(b)`.
// - Expected: model equals the first `model name` value; MHz count equals the number of `processor` stanzas (or zero when the arch has no `cpu MHz`); hypervisor flag equals whether the first `flags` line contains the word `hypervisor`.
func TestTCHP33_CpuinfoGivesTheModelPerProcessorMHz(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			b := readCap(t, c, "fs/proc/cpuinfo")
			ci, err := Cpuinfo(b)
			if err != nil {
				t.Fatalf("Cpuinfo: %v", err)
			}
			var procs, mhz int
			model, flags := "", ""
			for _, l := range textLines(b) {
				k, v, _ := strings.Cut(l, ":")
				switch strings.TrimSpace(k) {
				case "processor":
					procs++
				case "cpu MHz":
					mhz++
				case "model name":
					if model == "" {
						model = strings.TrimSpace(v)
					}
				case "flags":
					if flags == "" {
						flags = v
					}
				}
			}
			if ci.Processors != procs || procs != toolNproc(t, c) {
				t.Errorf("Processors = %d, file stanzas = %d, nproc = %d", ci.Processors, procs, toolNproc(t, c))
			}
			if mhz != 0 && mhz != procs {
				t.Fatalf("test oracle: %d cpu MHz lines for %d processors", mhz, procs)
			}
			if len(ci.MHz) != mhz {
				t.Errorf("MHz count = %d, want %d", len(ci.MHz), mhz)
			}
			if ci.Model != model {
				t.Errorf("Model = %q, want %q", ci.Model, model)
			}
			if want := slices.Contains(strings.Fields(flags), "hypervisor"); ci.Hypervisor != want {
				t.Errorf("Hypervisor = %v, want %v", ci.Hypervisor, want)
			}
		})
	}
	t.Run("x86", func(t *testing.T) {
		ci, err := Cpuinfo([]byte(x86Cpuinfo))
		if err != nil {
			t.Fatal(err)
		}
		want := CPUInfo{Model: "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz", MHz: []float64{2399.998, 2400.125}, Hypervisor: true, Processors: 2}
		if !reflect.DeepEqual(ci, want) {
			t.Errorf("Cpuinfo = %+v, want %+v", ci, want)
		}
		bare := strings.ReplaceAll(x86Cpuinfo, " hypervisor", " hypervisorx")
		if ci, err := Cpuinfo([]byte(bare)); err != nil || ci.Hypervisor {
			t.Errorf("flag word hypervisorx: Hypervisor = %v, %v", ci.Hypervisor, err)
		}
	})
}

// TC-HP-34: Diskstats parses 14, 18 and 20 field lines. Owner WI 2.3. Requirements: FR-8, FR-11.
//
// - Setup: both captures' `proc/diskstats`; synthetic lines derived from a capture line truncated to 14 and extended to 20 fields.
// - Steps: `Diskstats(b)`.
// - Expected: one entry per line with name, major, minor, reads completed (field 4), sectors read (6), writes completed (8), sectors written (10); all three field counts parse identically for the shared fields.
func TestTCHP34_DiskstatsParses1418And20Field(t *testing.T) {
	oracle := func(t *testing.T, l string) DiskStat {
		f := strings.Fields(l)
		maj, mn := mustU64(t, f[0]), mustU64(t, f[1])
		return DiskStat{Major: uint32(maj), Minor: uint32(mn), Name: f[2],
			ReadsCompleted: mustU64(t, f[3]), SectorsRead: mustU64(t, f[5]),
			WritesCompleted: mustU64(t, f[7]), SectorsWritten: mustU64(t, f[9])}
	}
	for _, c := range captures {
		for _, dir := range []string{"fs", "t1"} {
			t.Run(c+"/"+dir, func(t *testing.T) {
				b := readCap(t, c, dir+"/proc/diskstats")
				d, err := Diskstats(b)
				if err != nil {
					t.Fatalf("Diskstats: %v", err)
				}
				lines := textLines(b)
				if len(d) != len(lines) {
					t.Fatalf("%d entries for %d lines", len(d), len(lines))
				}
				for i, l := range lines {
					if want := oracle(t, l); d[i] != want {
						t.Errorf("line %q parsed as %+v, want %+v", l, d[i], want)
					}
				}
			})
		}
	}
	var line20 string
	for _, l := range textLines(readCap(t, "ubuntu-systemd", "fs/proc/diskstats")) {
		if f := strings.Fields(l); f[2] == "vdb" {
			line20 = l
		}
	}
	f := strings.Fields(line20)
	if len(f) != 20 {
		t.Fatalf("capture vdb line has %d fields, want 20", len(f))
	}
	want := oracle(t, line20)
	for _, n := range []int{14, 18, 20} {
		d, err := Diskstats([]byte(strings.Join(f[:n], " ") + "\n"))
		if err != nil || len(d) != 1 || d[0] != want {
			t.Errorf("%d fields: %+v, %v; want %+v", n, d, err, want)
		}
	}
	if d, err := Diskstats([]byte(strings.Join(f[:13], " ") + "\n")); err == nil {
		t.Errorf("13 fields parsed: %+v", d)
	}
}

// TC-HP-35: NetDev parses counters with either colon spacing. Owner WI 2.3. Requirements: FR-9, FR-11.
//
// - Setup: both captures' `proc/net/dev`; a synthetic line `eth0:1234 5 …` (no space after the colon, as kernels print for large counters).
// - Steps: `NetDev(b)`.
// - Expected: the two header lines are skipped; one entry per interface; rx bytes/packets/errs/drop and tx bytes/packets/errs/drop equal the file's columns; the no-space line parses the same as the spaced one; interface names equal `tools/ip-addr` names.
func TestTCHP35_NetDevParsesCountersWithEitherColonSpacing(t *testing.T) {
	for _, c := range captures {
		for _, dir := range []string{"fs", "t1"} {
			t.Run(c+"/"+dir, func(t *testing.T) {
				b := readCap(t, c, dir+"/proc/net/dev")
				n, err := NetDev(b)
				if err != nil {
					t.Fatalf("NetDev: %v", err)
				}
				lines := textLines(b)
				if len(n) != len(lines)-2 {
					t.Fatalf("%d entries for %d lines (2 header lines)", len(n), len(lines))
				}
				var names []string
				for i, l := range lines[2:] {
					name, rest, _ := strings.Cut(l, ":")
					f := strings.Fields(rest)
					want := NetDevStat{Name: strings.TrimSpace(name),
						RxBytes: mustU64(t, f[0]), RxPackets: mustU64(t, f[1]), RxErrs: mustU64(t, f[2]), RxDrop: mustU64(t, f[3]),
						TxBytes: mustU64(t, f[8]), TxPackets: mustU64(t, f[9]), TxErrs: mustU64(t, f[10]), TxDrop: mustU64(t, f[11])}
					if n[i] != want {
						t.Errorf("line %q parsed as %+v, want %+v", l, n[i], want)
					}
					names = append(names, n[i].Name)
				}
				ip := toolIfNames(t, c)
				slices.Sort(names)
				slices.Sort(ip)
				if !slices.Equal(names, ip) {
					t.Errorf("interfaces = %v, ip addr = %v", names, ip)
				}
			})
		}
	}
	spaced, err := NetDev([]byte("  eth0:    1234       5    6    7    0     0          0         0      678       9   10   11    0     0       0          0\n"))
	if err != nil {
		t.Fatal(err)
	}
	tight, err := NetDev([]byte("eth0:1234 5 6 7 0 0 0 0 678 9 10 11 0 0 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []NetDevStat{{Name: "eth0", RxBytes: 1234, RxPackets: 5, RxErrs: 6, RxDrop: 7, TxBytes: 678, TxPackets: 9, TxErrs: 10, TxDrop: 11}}
	if !slices.Equal(spaced, want) || !slices.Equal(tight, want) {
		t.Errorf("spaced = %+v, no-space = %+v, want %+v", spaced, tight, want)
	}
}

// TC-HP-36: Default routes from route and ipv6_route equal ip route. Owner WI 2.3. Requirements: FR-9.
//
// - Setup: both captures' `proc/net/route`, `proc/net/ipv6_route`, `tools/ip-route`.
// - Steps: `RouteV4(b)`, `RouteV6(b)`.
// - Expected: the IPv4 default gateway(s) (destination 0, mask 0, `RTF_GATEWAY` set, little-endian hex decoded) equal `ip route`'s `default via X dev Y metric M`; IPv6 likewise against `ip -6 route` default (when the capture has one); non-default and non-gateway lines are excluded.
func TestTCHP36_DefaultRoutesFromRouteAndIpv6Route(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			r4, err := RouteV4(readCap(t, c, "fs/proc/net/route"))
			if err != nil {
				t.Fatalf("RouteV4: %v", err)
			}
			var want []DefaultRoute
			for _, e := range toolIPRoute(t, c) {
				if e.Dst == "default" {
					want = append(want, DefaultRoute{Gateway: netip.MustParseAddr(e.Gateway), Interface: e.Dev, Metric: e.Metric})
				}
			}
			if len(want) == 0 {
				t.Fatalf("tools/ip-route has no default route")
			}
			if !slices.Equal(r4, want) {
				t.Errorf("RouteV4 = %+v, ip route default = %+v", r4, want)
			}
			// Neither capture recorded `ip -6 route` and neither ipv6_route
			// holds a default route with RTF_GATEWAY (only lo's reject
			// routes), so no IPv6 default is the right answer here.
			r6, err := RouteV6(readCap(t, c, "fs/proc/net/ipv6_route"))
			if err != nil || len(r6) != 0 {
				t.Errorf("RouteV6 = %+v, %v; want none", r6, err)
			}
		})
	}
	t.Run("synthetic v4", func(t *testing.T) {
		b := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
			"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" + // default via 192.168.1.1 metric 100
			"wlan0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" + // default without RTF_GATEWAY
			"eth0\t0001A8C0\t0101A8C0\t0003\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" + // 192.168.1.0/24 via a gateway
			"eth1\t00000000\tFE01A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" // second default via 192.168.1.254
		r, err := RouteV4([]byte(b))
		want := []DefaultRoute{
			{Gateway: netip.MustParseAddr("192.168.1.1"), Interface: "eth0", Metric: 100},
			{Gateway: netip.MustParseAddr("192.168.1.254"), Interface: "eth1", Metric: 600},
		}
		if err != nil || !slices.Equal(r, want) {
			t.Errorf("RouteV4 = %+v, %v; want %+v", r, err, want)
		}
	})
	t.Run("synthetic v6", func(t *testing.T) {
		r, err := RouteV6([]byte(ipv6DefaultRoute))
		want := []DefaultRoute{{Gateway: netip.MustParseAddr("fe80::1"), Interface: "eth0", Metric: 1024}}
		if err != nil || !slices.Equal(r, want) {
			t.Errorf("RouteV6 = %+v, %v; want %+v", r, err, want)
		}
	})
}

// TC-HP-37: Mountinfo decodes escapes, optional fields and options. Owner WI 2.3. Requirements: FR-7.
//
// - Setup: both captures' `proc/1/mountinfo` (or `self`); synthetic lines with `\040`, `\011`, `\012`, `\134` in the mount point and with zero, one and two optional fields (`shared:1 master:2`) before ` - `.
// - Steps: `Mountinfo(b)`.
// - Expected: mount id, `maj:min`, root, decoded mount point, per-mount options, fstype, source and super options per line; every `tools/df` target is among the parsed mount points.
func TestTCHP37_MountinfoDecodesEscapesOptionalFieldsAndOptions(t *testing.T) {
	for _, c := range captures {
		for _, file := range []string{"fs/proc/1/mountinfo", "fs/proc/self/mountinfo"} {
			t.Run(c+"/"+file, func(t *testing.T) {
				b := readCap(t, c, file)
				m, err := Mountinfo(b)
				if err != nil {
					t.Fatalf("Mountinfo: %v", err)
				}
				lines := textLines(b)
				if len(m) != len(lines) {
					t.Fatalf("%d mounts for %d lines", len(m), len(lines))
				}
				var points []string
				for i, l := range lines {
					f := strings.Split(l, " ")
					sep := slices.Index(f[6:], "-") + 6
					id, _ := strconv.Atoi(f[0])
					parent, _ := strconv.Atoi(f[1])
					maj, mn, _ := strings.Cut(f[2], ":")
					want := Mount{ID: id, ParentID: parent, Major: uint32(mustU64(t, maj)), Minor: uint32(mustU64(t, mn)),
						Root: f[3], MountPoint: f[4], Options: strings.Split(f[5], ","), Optional: f[6:sep],
						FSType: f[sep+1], Source: f[sep+2], SuperOptions: f[sep+3]}
					if len(want.Optional) == 0 {
						want.Optional = nil
					}
					if !reflect.DeepEqual(m[i], want) {
						t.Errorf("line %q\nparsed as %+v\nwant      %+v", l, m[i], want)
					}
					points = append(points, m[i].MountPoint)
				}
				if file == "fs/proc/1/mountinfo" {
					for _, target := range toolDfTargets(t, c) {
						if !slices.Contains(points, target) {
							t.Errorf("df target %q not among mount points %v", target, points)
						}
					}
				}
			})
		}
	}
	t.Run("synthetic", func(t *testing.T) {
		b := `36 35 98:0 /mnt\0401 /mnt\040with\011tab\012nl\134bs rw,noatime - ext3 /dev/root\040x rw,errors=continue
37 35 98:1 / /one rw master:1 - ext4 /dev/sda1 rw
38 35 0:52 / /two ro,nosuid shared:1 master:2 - tmpfs none ro,size=1024k
`
		m, err := Mountinfo([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		want := []Mount{
			{ID: 36, ParentID: 35, Major: 98, Minor: 0, Root: "/mnt 1", MountPoint: "/mnt with\ttab\nnl\\bs",
				Options: []string{"rw", "noatime"}, FSType: "ext3", Source: "/dev/root x", SuperOptions: "rw,errors=continue"},
			{ID: 37, ParentID: 35, Major: 98, Minor: 1, Root: "/", MountPoint: "/one",
				Options: []string{"rw"}, Optional: []string{"master:1"}, FSType: "ext4", Source: "/dev/sda1", SuperOptions: "rw"},
			{ID: 38, ParentID: 35, Major: 0, Minor: 52, Root: "/", MountPoint: "/two",
				Options: []string{"ro", "nosuid"}, Optional: []string{"shared:1", "master:2"}, FSType: "tmpfs", Source: "none", SuperOptions: "ro,size=1024k"},
		}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("Mountinfo =\n%+v\nwant\n%+v", m, want)
		}
	})
}

// TC-HP-38: PidStat and PidStatusUID handle hostile comm values. Owner WI 2.3. Requirements: FR-10, FR-11, NFR-6.
//
// - Setup: every `fs/proc/[0-9]*/stat` and `status` in both captures; synthetic stat lines derived from a capture line with comm `a) (b`, `) (`, `x y z`, and an empty comm `()`.
// - Steps: `PidStat(b)`, `PidStatusUID(b)`.
// - Expected: comm is the text between the first `(` and the last `)`; utime, stime, starttime and rss equal fields 14, 15, 22, 24 counted after the last `)`; UID is the first number of the `Uid:` line; every captured file parses.
func TestTCHP38_PidStatAndPidStatusUIDHandleHostileCommValues(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			stats := append(globCap(t, c, "fs/proc/[0-9]*/stat"), globCap(t, c, "t1/proc/[0-9]*/stat")...)
			if len(stats) == 0 {
				t.Fatalf("no pid stat files")
			}
			for _, path := range stats {
				b := readCap(t, c, mustRel(t, c, path))
				p, err := PidStat(b)
				if err != nil {
					t.Errorf("%s: %v", path, err)
					continue
				}
				// Every captured comm is one word, so plain field splitting is
				// an independent oracle for these files.
				f := strings.Fields(string(b))
				want := PidStatInfo{PID: int(mustU64(t, f[0])), Comm: strings.TrimSuffix(strings.TrimPrefix(f[1], "("), ")"), State: f[2],
					UTime: mustU64(t, f[13]), STime: mustU64(t, f[14]), StartTime: mustU64(t, f[21])}
				rss, err := strconv.ParseInt(f[23], 10, 64)
				if err != nil {
					t.Fatalf("test oracle: rss %q", f[23])
				}
				want.RSSPages = rss
				if p != want {
					t.Errorf("%s parsed as %+v, want %+v", path, p, want)
				}
				if dir := filepath.Base(filepath.Dir(path)); strconv.Itoa(p.PID) != dir {
					t.Errorf("%s: PID %d", path, p.PID)
				}
			}
			statuses := append(globCap(t, c, "fs/proc/[0-9]*/status"), globCap(t, c, "t1/proc/[0-9]*/status")...)
			for _, path := range statuses {
				b := readCap(t, c, mustRel(t, c, path))
				uid, err := PidStatusUID(b)
				if err != nil {
					t.Errorf("%s: %v", path, err)
					continue
				}
				var want uint64
				for _, l := range textLines(b) {
					if f := strings.Fields(l); f[0] == "Uid:" {
						want = mustU64(t, f[1])
					}
				}
				if uint64(uid) != want {
					t.Errorf("%s: UID %d, want %d", path, uid, want)
				}
			}
		})
	}
	t.Run("hostile comm", func(t *testing.T) {
		line := string(readCap(t, "ubuntu-systemd", "fs/proc/1/stat"))
		base, err := PidStat([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if base.Comm != "systemd" || base.UTime != 6 || base.STime != 8 || base.StartTime != 246582732 || base.RSSPages != 3738 {
			t.Fatalf("capture fs/proc/1/stat parsed as %+v", base)
		}
		for _, comm := range []string{"a) (b", ") (", "x y z", "", "((", "))", "a\nb"} {
			p, err := PidStat([]byte(strings.Replace(line, "(systemd)", "("+comm+")", 1)))
			want := base
			want.Comm = comm
			if err != nil || p != want {
				t.Errorf("comm %q: %+v, %v; want %+v", comm, p, err, want)
			}
		}
	})
	t.Run("status", func(t *testing.T) {
		uid, err := PidStatusUID([]byte("Name:\tnginx\nUid:\t33\t0\t33\t33\nGid:\t33\t33\t33\t33\n"))
		if err != nil || uid != 33 {
			t.Errorf("UID = %d, %v; want the real uid 33", uid, err)
		}
		if _, err := PidStatusUID([]byte("Name:\tx\nGid:\t0\t0\t0\t0\n")); err == nil {
			t.Errorf("no Uid line: no error")
		}
	})
}

func mustRel(t testing.TB, capture, path string) string {
	t.Helper()
	rel, err := filepath.Rel(capPath(capture), path)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// TC-HP-39: Every parser has a fuzz target seeded from the captures. Owner WI 2.3. Requirements: NFR-4.
//
// - Setup: none.
// - Steps: list the `Fuzz*` functions of `hostparse` (go/parser over `fuzz_test.go`); `go test ./internal/providers/host/hostparse` runs their seeds.
// - Expected: one fuzz target per parser of WI 2.3 and 2.4 (names listed in the TC's table in the stub); each adds every matching capture file from both captures as a seed; no seed panics.
//
// The parsers are found, not listed: every exported function of the
// package's non-test files whose first parameter is []byte needs a
// Fuzz<Name> target, so WI 2.4's parsers are held to this test too. wi23 is
// the WI 2.3 set, checked so the discovery itself cannot silently find none.
func TestTCHP39_EveryParserHasAFuzzTargetSeeded(t *testing.T) {
	wi23 := []string{"ProcStat", "Meminfo", "Loadavg", "Uptime", "Vmstat", "Pressure", "Cpuinfo",
		"Diskstats", "NetDev", "RouteV4", "RouteV6", "Mountinfo", "PidStat", "PidStatusUID"}

	fset := token.NewFileSet()
	var parsers []string
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range srcs {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || len(fn.Type.Params.List) == 0 {
				continue
			}
			if at, ok := fn.Type.Params.List[0].Type.(*ast.ArrayType); ok && at.Len == nil {
				if id, ok := at.Elt.(*ast.Ident); ok && id.Name == "byte" {
					parsers = append(parsers, fn.Name.Name)
				}
			}
		}
	}
	for _, p := range wi23 {
		if !slices.Contains(parsers, p) {
			t.Errorf("WI 2.3 parser %s not found among %v", p, parsers)
		}
	}

	file, err := parser.ParseFile(fset, "fuzz_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	fuzz := map[string]*ast.FuncDecl{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Fuzz") {
			fuzz[fn.Name.Name] = fn
		}
	}
	calls := func(fn *ast.FuncDecl, name string) bool {
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if ce, ok := n.(*ast.CallExpr); ok {
				if id, ok := ce.Fun.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
			return !found
		})
		return found
	}
	for _, p := range parsers {
		name := "Fuzz" + p
		fn, ok := fuzz[name]
		if !ok {
			t.Errorf("parser %s has no %s in fuzz_test.go", p, name)
			continue
		}
		if len(fn.Body.List) == 0 || !isCallTo(fn.Body.List[0], "addCaptureSeeds") {
			t.Errorf("%s does not start with addCaptureSeeds(f)", name)
		}
		if !calls(fn, p) {
			t.Errorf("%s never calls %s", name, p)
		}
		if _, ok := fuzzSeedGlobs[name]; !ok {
			t.Errorf("%s has no fuzzSeedGlobs entry", name)
		}
	}
	for name := range fuzz {
		if _, ok := fuzzSeedGlobs[name]; !ok {
			t.Errorf("%s has no fuzzSeedGlobs entry", name)
		}
	}
	// Every WI 2.3 target is seeded from both captures.
	for _, p := range wi23 {
		for _, c := range captures {
			n := 0
			for _, g := range fuzzSeedGlobs["Fuzz"+p] {
				n += len(globCap(t, c, filepath.Join("fs", g)))
			}
			if n == 0 {
				t.Errorf("Fuzz%s: no seed from %s (globs %q)", p, c, fuzzSeedGlobs["Fuzz"+p])
			}
		}
	}
}

func isCallTo(s ast.Stmt, name string) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	ce, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := ce.Fun.(*ast.Ident)
	return ok && id.Name == name
}

// e8Case is one parser under TC-HP-E8's mutations. recs flattens its result
// into records keyed by what identifies a line (cpu id, meminfo key,
// interface, mount id, ...), so a mutation's effect can be compared record by
// record.
type e8Case struct {
	name  string
	files map[string][]byte
	recs  func([]byte) (map[string]string, error)
	// single: the file is one record, so a broken field is an error.
	single bool
	// brk and field name the token replaced by "x" for the non-numeric
	// mutation: field (strings.Fields index) of the first line whose
	// fields, joined by single spaces, start with brk. brkOptional skips
	// the mutation for a file without such a line (aarch64 cpuinfo has no
	// cpu MHz).
	brk         string
	field       int
	brkOptional bool
	// anchor is the prefix (fields joined by single spaces) of the required
	// anchor line(s), removed for the missing-anchor mutation; empty when
	// any valid line is the anchor.
	anchor string
	// freeText are record keys holding free text, which a new trailing column
	// legitimately extends (cpuinfo's model name).
	freeText []string
}

func one(k string, v any, err error) (map[string]string, error) {
	if err != nil {
		return nil, err
	}
	return map[string]string{k: fmt.Sprintf("%+v", v)}, nil
}

func e8Cases(t *testing.T) []e8Case {
	both := func(rel string) map[string][]byte {
		out := map[string][]byte{}
		for _, c := range captures {
			out[c] = readCap(t, c, rel)
		}
		return out
	}
	withV6Default := both("fs/proc/net/ipv6_route")
	for k, v := range withV6Default {
		withV6Default[k] = append(slices.Clone(v), ipv6DefaultRoute...)
	}
	cpuinfo := both("fs/proc/cpuinfo")
	cpuinfo["x86"] = []byte(x86Cpuinfo)
	return []e8Case{
		{name: "ProcStat", files: both("fs/proc/stat"), brk: "cpu1 ", field: 1, anchor: "cpu ",
			recs: func(b []byte) (map[string]string, error) {
				st, err := ProcStat(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{"cpu": fmt.Sprintf("%+v", st.Total)}
				for _, c := range st.CPUs {
					out[fmt.Sprintf("cpu%d", c.ID)] = fmt.Sprintf("%+v", c.Times)
				}
				if st.HasBootTime {
					out["btime"] = strconv.FormatUint(st.BootTime, 10)
				}
				return out, nil
			}},
		{name: "Meminfo", files: both("fs/proc/meminfo"), brk: "MemFree:", field: 1, anchor: "MemTotal:",
			recs: func(b []byte) (map[string]string, error) {
				m, err := Meminfo(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{}
				for k, v := range m {
					out[k] = strconv.FormatUint(v, 10)
				}
				return out, nil
			}},
		{name: "Loadavg", files: both("fs/proc/loadavg"), single: true, brk: "", field: 1,
			recs: func(b []byte) (map[string]string, error) { v, err := Loadavg(b); return one("load", v, err) }},
		{name: "Uptime", files: both("fs/proc/uptime"), single: true, brk: "", field: 0,
			recs: func(b []byte) (map[string]string, error) { v, err := Uptime(b); return one("uptime", v, err) }},
		{name: "Vmstat", files: both("fs/proc/vmstat"), brk: "oom_kill", field: 1,
			recs: func(b []byte) (map[string]string, error) {
				v, err := Vmstat(b)
				if err != nil || !v.OOMKillPresent {
					return map[string]string{}, err
				}
				return one("oom_kill", v.OOMKill, nil)
			}},
		{name: "Pressure", files: both("fs/proc/pressure/memory"), brk: "full", field: 1, anchor: "some",
			recs: func(b []byte) (map[string]string, error) {
				p, err := Pressure(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{"some": fmt.Sprintf("%+v", *p.Some)}
				if p.Full != nil {
					out["full"] = fmt.Sprintf("%+v", *p.Full)
				}
				return out, nil
			}},
		{name: "Cpuinfo", files: cpuinfo, brk: "cpu MHz", field: 3, brkOptional: true, anchor: "processor", freeText: []string{"model"},
			recs: func(b []byte) (map[string]string, error) {
				c, err := Cpuinfo(b)
				if err != nil {
					return nil, err
				}
				return map[string]string{"model": c.Model, "mhz": fmt.Sprint(c.MHz),
					"hypervisor": fmt.Sprint(c.Hypervisor), "processors": strconv.Itoa(c.Processors)}, nil
			}},
		{name: "Diskstats", files: both("fs/proc/diskstats"), brk: "253 0 vda", field: 3,
			recs: func(b []byte) (map[string]string, error) {
				d, err := Diskstats(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{}
				for _, s := range d {
					out[s.Name] = fmt.Sprintf("%+v", s)
				}
				return out, nil
			}},
		{name: "NetDev", files: both("fs/proc/net/dev"), brk: "eth0:", field: 1,
			recs: func(b []byte) (map[string]string, error) {
				n, err := NetDev(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{}
				for _, s := range n {
					out[s.Name] = fmt.Sprintf("%+v", s)
				}
				return out, nil
			}},
		{name: "RouteV4", files: both("fs/proc/net/route"), brk: "eth0 00000000", field: 2, anchor: "Iface",
			recs: func(b []byte) (map[string]string, error) { return routeRecs(RouteV4(b)) }},
		{name: "RouteV6", files: withV6Default, brk: "00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe80", field: 8,
			recs: func(b []byte) (map[string]string, error) { return routeRecs(RouteV6(b)) }},
		{name: "Mountinfo", files: both("fs/proc/1/mountinfo"), brk: "", field: 0,
			recs: func(b []byte) (map[string]string, error) {
				m, err := Mountinfo(b)
				if err != nil {
					return nil, err
				}
				out := map[string]string{}
				for _, s := range m {
					out[strconv.Itoa(s.ID)] = fmt.Sprintf("%+v", s)
				}
				return out, nil
			}},
		{name: "PidStat", files: both("fs/proc/1/stat"), single: true, brk: "", field: 13,
			recs: func(b []byte) (map[string]string, error) { v, err := PidStat(b); return one("stat", v, err) }},
		{name: "PidStatusUID", files: both("fs/proc/1/status"), single: true, brk: "Uid:", field: 1, anchor: "Uid:",
			recs: func(b []byte) (map[string]string, error) { v, err := PidStatusUID(b); return one("uid", v, err) }},
	}
}

func routeRecs(r []DefaultRoute, err error) (map[string]string, error) {
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range r {
		out[d.Interface+" "+d.Gateway.String()] = fmt.Sprintf("%+v", d)
	}
	return out, nil
}

func joinLines(lines []string, eol string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, eol) + eol)
}

// e8Break replaces one numeric field of lines[idx] with "x": the parser must
// skip that line alone (or fail, for a one-record file).
func e8Break(t *testing.T, tc e8Case, lines []string, idx int) {
	t.Helper()
	f := strings.Fields(lines[idx])
	f[tc.field] = "x"
	broken := slices.Clone(lines)
	broken[idx] = strings.Join(f, " ")
	got, err := tc.recs(joinLines(broken, "\n"))
	if tc.single {
		if err == nil {
			t.Errorf("non-numeric field %d: no error, got %v", tc.field, got)
		}
	} else {
		want, wantErr := tc.recs(joinLines(slices.Delete(slices.Clone(lines), idx, idx+1), "\n"))
		if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("non-numeric field %d of %q: %v, %v; want the line skipped: %v, %v", tc.field, broken[idx], got, err, want, wantErr)
		}
	}
}

func normalized(l string) string { return strings.Join(strings.Fields(l), " ") }

// TC-HP-E8: Parsers skip malformed lines and never panic. Owner WI 2.3. Requirements: NFR-4.
//
// - Setup: per parser, each capture file mutated: a new trailing column on every line, CRLF line endings, a truncated last line, one non-numeric field, an empty file; and the required anchor removed (`cpu ` from stat, `MemTotal` from meminfo).
// - Steps: parse each.
// - Expected: no panic; mutations yield the same values for unaffected lines and skip only the broken ones; an empty file or a missing anchor returns an error (not zero values).
func TestTCHPE8_ParsersSkipMalformedLinesAndNeverPanic(t *testing.T) {
	for _, tc := range e8Cases(t) {
		for src, b := range tc.files {
			t.Run(tc.name+"/"+src, func(t *testing.T) {
				lines := textLines(b)
				base, err := tc.recs(b)
				if err != nil {
					t.Fatalf("unmutated: %v", err)
				}

				// A new trailing column on every line.
				var trailing []string
				for _, l := range lines {
					if strings.TrimSpace(l) != "" {
						l += " 0"
					}
					trailing = append(trailing, l)
				}
				got, err := tc.recs(joinLines(trailing, "\n"))
				if err != nil {
					t.Errorf("trailing column: %v", err)
				}
				for k, v := range base {
					if !slices.Contains(tc.freeText, k) && got[k] != v {
						t.Errorf("trailing column: %s = %q, want %q", k, got[k], v)
					}
				}
				if len(got) != len(base) {
					t.Errorf("trailing column: %d records, want %d", len(got), len(base))
				}

				// CRLF line endings.
				got, err = tc.recs(joinLines(lines, "\r\n"))
				if err != nil || !reflect.DeepEqual(got, base) {
					t.Errorf("CRLF: %v, %v; want %v", got, err, base)
				}

				// A truncated last line: every record of the other lines is
				// unchanged (the truncated line may parse or be skipped).
				trunc := slices.Clone(lines)
				last := trunc[len(trunc)-1]
				trunc[len(trunc)-1] = last[:len(last)/2]
				got, gotErr := tc.recs(joinLines(trunc, "\n"))
				if want, err := tc.recs(joinLines(lines[:len(lines)-1], "\n")); err == nil {
					if gotErr != nil {
						t.Errorf("truncated last line: %v", gotErr)
					}
					for k, v := range want {
						if got[k] != v {
							t.Errorf("truncated last line: %s = %q, want %q", k, got[k], v)
						}
					}
				}

				// One non-numeric field: that line alone is skipped.
				idx := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(normalized(l), tc.brk) })
				switch {
				case idx < 0 && tc.brkOptional:
				case idx < 0:
					t.Fatalf("no line starting with %q to break", tc.brk)
				default:
					e8Break(t, tc, lines, idx)
				}

				// An empty file.
				for _, empty := range [][]byte{nil, []byte("\n"), []byte("\r\n\r\n")} {
					if got, err := tc.recs(empty); err == nil {
						t.Errorf("empty file %q: no error, got %v", empty, got)
					}
				}

				// The required anchor removed.
				if tc.anchor != "" {
					var rest []string
					for _, l := range lines {
						if !strings.HasPrefix(normalized(l), tc.anchor) {
							rest = append(rest, l)
						}
					}
					if len(rest) == len(lines) {
						t.Fatalf("no anchor line %q", tc.anchor)
					}
					if got, err := tc.recs(joinLines(rest, "\n")); err == nil {
						t.Errorf("anchor %q removed: no error, got %v", tc.anchor, got)
					}
				}
			})
		}
	}
}

// TC-HP-E9: CPU and per-process deltas from the captures are consistent. Owner WI 2.3. Requirements: NFR-3, FR-10.
//
// - Setup: both captures' `fs/` and `t1/`.
// - Steps: parse both samples; compute Δ per pid present in both with equal starttime and the total Δ over all CPUs.
// - Expected: every per-pid Δ ≥ 0; Σ per-pid Δ ≤ total CPU Δ (all CPUs); total CPU % within ±5 pp of `tools/vmstat` (this validates the capture itself before the sampler relies on it).
func TestTCHPE9_CPUAndPerProcessDeltasFromThe(t *testing.T) {
	for _, c := range captures {
		t.Run(c, func(t *testing.T) {
			s0, err := ProcStat(readCap(t, c, "fs/proc/stat"))
			if err != nil {
				t.Fatal(err)
			}
			s1, err := ProcStat(readCap(t, c, "t1/proc/stat"))
			if err != nil {
				t.Fatal(err)
			}
			if s1.Total.Total() <= s0.Total.Total() {
				t.Fatalf("total CPU ticks did not advance: %d -> %d", s0.Total.Total(), s1.Total.Total())
			}
			total := s1.Total.Total() - s0.Total.Total()
			busy := s1.Total.Busy() - s0.Total.Busy()
			pct := 100 * float64(busy) / float64(total)
			if want := toolVmstatBusy(t, c); math.Abs(pct-want) > 5 {
				t.Errorf("CPU %% fs->t1 = %.2f, vmstat 100-id-wa = %.0f (over 5 pp apart)", pct, want)
			}

			var sum uint64
			compared := 0
			for _, p1path := range globCap(t, c, "t1/proc/[0-9]*/stat") {
				pid := filepath.Base(filepath.Dir(p1path))
				if len(globCap(t, c, "fs/proc/"+pid+"/stat")) == 0 {
					continue
				}
				p0, err := PidStat(readCap(t, c, "fs/proc/"+pid+"/stat"))
				if err != nil {
					t.Fatal(err)
				}
				p1, err := PidStat(readCap(t, c, "t1/proc/"+pid+"/stat"))
				if err != nil {
					t.Fatal(err)
				}
				if p0.StartTime != p1.StartTime {
					continue // pid reused
				}
				a, b := p0.UTime+p0.STime, p1.UTime+p1.STime
				if b < a {
					t.Errorf("pid %s: utime+stime went back %d -> %d", pid, a, b)
					continue
				}
				sum += b - a
				compared++
			}
			if compared == 0 {
				t.Fatalf("no pid in both samples")
			}
			if sum > total {
				t.Errorf("Σ per-pid Δ = %d > total CPU Δ = %d", sum, total)
			}
			t.Logf("%d pids, Σ Δ = %d ticks, total Δ = %d ticks, CPU %.2f%%", compared, sum, total, pct)
		})
	}
}
