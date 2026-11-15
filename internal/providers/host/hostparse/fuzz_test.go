package hostparse

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// One fuzz target per parser (NFR-4, TC-HP-39). Each target is named
// Fuzz<Parser>, starts with addCaptureSeeds(f) and calls its parser. `go
// test` runs every seed; `go test -fuzz=Fuzz<Parser>` fuzzes from them.
//
// To add a parser: give it an entry in fuzzSeedGlobs (and synthetic edge
// inputs in fuzzSyntheticSeeds when the captures lack a shape) and a
// Fuzz<Parser> function below. TestTCHP39_EveryParserHasAFuzzTargetSeeded
// finds every exported parser taking []byte and fails without one.

// fuzzSeedGlobs maps a fuzz target to the capture files that seed it: glob
// patterns relative to each capture's fs/ and t1/ directories. Every match in
// both captures is added.
var fuzzSeedGlobs = map[string][]string{
	"FuzzProcStat":     {"proc/stat"},
	"FuzzMeminfo":      {"proc/meminfo"},
	"FuzzLoadavg":      {"proc/loadavg"},
	"FuzzUptime":       {"proc/uptime"},
	"FuzzVmstat":       {"proc/vmstat"},
	"FuzzPressure":     {"proc/pressure/*"},
	"FuzzCpuinfo":      {"proc/cpuinfo"},
	"FuzzDiskstats":    {"proc/diskstats"},
	"FuzzNetDev":       {"proc/net/dev"},
	"FuzzRouteV4":      {"proc/net/route"},
	"FuzzRouteV6":      {"proc/net/ipv6_route"},
	"FuzzMountinfo":    {"proc/[0-9]*/mountinfo", "proc/self/mountinfo"},
	"FuzzPidStat":      {"proc/[0-9]*/stat"},
	"FuzzPidStatusUID": {"proc/[0-9]*/status"},
}

// fuzzSyntheticSeeds are edge inputs the captures do not hold (both are
// aarch64 containers on one VM kernel): x86 cpuinfo, an IPv6 default route,
// hostile comm values, mountinfo escapes and optional fields, 14-field
// diskstats, the no-space net/dev colon.
var fuzzSyntheticSeeds = map[string][]string{
	"FuzzProcStat": {"cpu  10 20 30 40 50 60 70 80 90 100\ncpu0 1 2 3 4\nbtime 1\n"},
	"FuzzMeminfo":  {"MemTotal: 18446744073709551615 kB\n"},
	"FuzzPressure": {"some avg10=1.50 avg60=0.25 avg300=0.00 total=42\n"},
	"FuzzCpuinfo":  {x86Cpuinfo},
	"FuzzDiskstats": {
		"   8       0 sda 1 2 3 4 5 6 7 8 9 10 11\n",
	},
	"FuzzNetDev":  {"eth0:1234 5 0 0 0 0 0 0 678 9 0 0 0 0 0 0\n"},
	"FuzzRouteV6": {ipv6DefaultRoute},
	"FuzzMountinfo": {
		`36 35 98:0 /mnt1 /mnt\040with\011tab rw,noatime shared:1 master:2 - ext3 /dev/root rw,errors=continue` + "\n",
	},
	"FuzzPidStat": {
		"1 (a) (b) S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704 3738\n",
		"1 () S 0 1 1 0 -1 4194560 4678 31755 0 2 6 8 6 13 20 0 1 0 246582732 25800704 3738\n",
	},
}

// addCaptureSeeds adds every capture file matching f's fuzzSeedGlobs entry,
// its synthetic seeds and the empty input.
func addCaptureSeeds(f *testing.F) {
	f.Helper()
	globs, ok := fuzzSeedGlobs[f.Name()]
	if !ok {
		f.Fatalf("%s has no fuzzSeedGlobs entry", f.Name())
	}
	n := 0
	for _, c := range captures {
		for _, dir := range []string{"fs", "t1"} {
			for _, g := range globs {
				for _, m := range globCap(f, c, filepath.Join(dir, g)) {
					b, err := os.ReadFile(m)
					if err != nil {
						f.Fatalf("read seed: %v", err)
					}
					f.Add(b)
					n++
				}
			}
		}
	}
	if n == 0 {
		f.Fatalf("%s: no capture file matches %q", f.Name(), globs)
	}
	for _, s := range fuzzSyntheticSeeds[f.Name()] {
		f.Add([]byte(s))
	}
	f.Add([]byte{})
}

func FuzzProcStat(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		st, err := ProcStat(b)
		if err != nil {
			return
		}
		_ = st.Total.Total()
		seen := map[int]bool{}
		for _, c := range st.CPUs {
			if c.ID < 0 || seen[c.ID] {
				t.Fatalf("bad or duplicate cpu id %d", c.ID)
			}
			seen[c.ID] = true
		}
	})
}

func FuzzMeminfo(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Meminfo(b)
		if err != nil {
			return
		}
		if _, ok := m["MemTotal"]; !ok {
			t.Fatalf("no error but MemTotal missing: %v", m)
		}
		for k, v := range m {
			if !slices.Contains(meminfoKeys, k) {
				t.Fatalf("unexpected key %q", k)
			}
			if v%1024 != 0 {
				t.Fatalf("%s = %d is not a whole number of kB", k, v)
			}
		}
	})
}

func FuzzLoadavg(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := Loadavg(b); err != nil {
			return
		}
	})
}

func FuzzUptime(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Uptime(b)
		if err == nil && (s < 0 || math.IsNaN(s)) {
			t.Fatalf("uptime %v", s)
		}
	})
}

func FuzzVmstat(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := Vmstat(b)
		if err == nil && !v.OOMKillPresent && v.OOMKill != 0 {
			t.Fatalf("absent oom_kill with value %d", v.OOMKill)
		}
	})
}

func FuzzPressure(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Pressure(b)
		if err == nil && p.Some == nil {
			t.Fatalf("no error but Some is nil")
		}
	})
}

func FuzzCpuinfo(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Cpuinfo(b)
		if err == nil && c.Processors == 0 {
			t.Fatalf("no error but no processor")
		}
	})
}

func FuzzDiskstats(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := Diskstats(b)
		if err == nil && len(d) == 0 {
			t.Fatalf("no error but no disk")
		}
	})
}

func FuzzNetDev(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := NetDev(b)
		if err == nil && len(n) == 0 {
			t.Fatalf("no error but no interface")
		}
		for _, s := range n {
			if s.Name == "" {
				t.Fatalf("empty interface name")
			}
		}
	})
}

func FuzzRouteV4(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := RouteV4(b)
		if err != nil {
			return
		}
		for _, d := range r {
			if !d.Gateway.Is4() || d.Interface == "" {
				t.Fatalf("bad default route %+v", d)
			}
		}
	})
}

func FuzzRouteV6(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := RouteV6(b)
		if err != nil {
			return
		}
		for _, d := range r {
			if !d.Gateway.Is6() || d.Interface == "" {
				t.Fatalf("bad default route %+v", d)
			}
		}
	})
}

func FuzzMountinfo(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Mountinfo(b)
		if err == nil && len(m) == 0 {
			t.Fatalf("no error but no mount")
		}
	})
}

func FuzzPidStat(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := PidStat(b)
		if err == nil && (p.PID < 0 || len(p.Comm) > len(b)) {
			t.Fatalf("bad stat %+v", p)
		}
	})
}

func FuzzPidStatusUID(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := PidStatusUID(b); err != nil {
			return
		}
	})
}
