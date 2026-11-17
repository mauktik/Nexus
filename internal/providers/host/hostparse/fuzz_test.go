package hostparse

import (
	"bytes"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// One fuzz target per parser (NFR-4, TC-HP-39). Each target is named
// Fuzz<Parser>, starts with addCaptureSeeds(f) and calls its parser. `go
// test` runs every seed; `go test -fuzz=Fuzz<Parser>` fuzzes from them.
//
// To add a parser: give it an entry in fuzzSeedGlobs (and synthetic edge
// inputs in fuzzSyntheticSeeds when the captures lack a shape) and a
// Fuzz<Parser> function below. TestTCHP39_EveryParserHasAFuzzTargetSeeded
// finds every exported parser taking []byte and fails without one.

// fuzzSeedDirs are the capture directories fuzzSeedGlobs patterns are
// relative to: the two samples and tools/ (command outputs).
var fuzzSeedDirs = []string{"fs", "t1", "tools"}

// fuzzSeedGlobs maps a fuzz target to the capture files that seed it: glob
// patterns relative to each of fuzzSeedDirs in each capture. Every regular
// file matched in either capture is added.
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

	// WI 2.4. The captures have no hwmon, thermal or cpufreq file, so
	// FuzzMillidegrees also takes the integer topology files.
	"FuzzScalarInt": {"sys/class/net/*/mtu", "sys/class/net/*/ifindex", "sys/class/net/*/type",
		"sys/class/net/*/speed", "sys/devices/system/cpu/cpu*/topology/*", "sys/block/*/dev"},
	"FuzzScalarString": {"sys/class/dmi/id/*", "sys/class/net/*/operstate", "run/systemd/container",
		"proc/sys/kernel/osrelease"},
	"FuzzMillidegrees": {"sys/class/hwmon/*/temp*_input", "sys/class/thermal/thermal_zone*/temp",
		"sys/devices/system/cpu/cpu*/topology/core_id"},
	"FuzzOperstate":                  {"sys/class/net/*/operstate"},
	"FuzzLinkSpeed":                  {"sys/class/net/*/speed"},
	"FuzzBlockSize":                  {"sys/block/*/size", "sys/block/*/*/size"},
	"FuzzRotational":                 {"sys/block/*/queue/rotational"},
	"FuzzRemovable":                  {"sys/block/*/removable"},
	"FuzzResolvConf":                 {"etc/resolv.conf", "run/systemd/resolve/resolv.conf"},
	"FuzzResolvedInUse":              {"etc/resolv.conf", "run/systemd/resolve/resolv.conf"},
	"FuzzPasswd":                     {"etc/passwd"},
	"FuzzTimezone":                   {"etc/localtime", "etc/timezone", "readlink-localtime"},
	"FuzzTimedatectlNTPSynchronized": {"timedatectl", "systemd-detect-virt"},
	"FuzzChronyTracking":             {"timedatectl", "systemd-detect-virt", "nproc"},
}

// wi24FuzzTargets are the WI 2.4 targets, whose seeds TC-HP-E10 runs
// through every WI 2.4 parser.
var wi24FuzzTargets = []string{"FuzzScalarInt", "FuzzScalarString", "FuzzMillidegrees", "FuzzOperstate",
	"FuzzLinkSpeed", "FuzzBlockSize", "FuzzRotational", "FuzzRemovable", "FuzzResolvConf",
	"FuzzResolvedInUse", "FuzzPasswd", "FuzzTimezone", "FuzzTimedatectlNTPSynchronized", "FuzzChronyTracking"}

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

	// WI 2.4: the synthetic tables of TC-HP-40..47 (sysfs_test.go and
	// hostprov_sysfs_tc_test.go).
	"FuzzScalarInt":    {"1800000\n", "-1\n", "0x10", "9223372036854775808"},
	"FuzzScalarString": {"Package id 0\n", "a\xffb\n", "\n"},
	"FuzzMillidegrees": {"45000\n", "-5250\n", "45.0\n", "99999999999999999999\n"},
	"FuzzOperstate":    {"up\n", "down", "unknown", "dormant", "notpresent", "lowerlayerdown", "testing", "garbage"},
	"FuzzLinkSpeed":    {"1000\n", "-1", "0", "-5", "abc"},
	"FuzzBlockSize":    {"41943040\n", "-1", "36028797018963968", "18446744073709551616"},
	"FuzzRotational":   {"0\n", "1\n", "2", "true"},
	"FuzzRemovable":    {"0\n", "1\n", "01", "-1"},
	"FuzzResolvConf": {
		"# c\n; nameserver 9.9.9.9\nsearch example.com\noptions edns0\nnameserver not-an-address\nnameserver fe80::1%eth0\n",
		strings.Repeat("nameserver 10.0.0.1\n", 17),
	},
	// etc and run separated by NUL; without NUL there is no run file.
	"FuzzResolvedInUse": {
		"nameserver 127.0.0.53\noptions edns0 trust-ad\n\x00nameserver 192.168.5.1\n",
		"nameserver 127.0.0.54\n\x00",
		"nameserver 127.0.0.53\nnameserver 1.1.1.1\n\x00nameserver 1.1.1.1\n",
		"nameserver 127.0.0.53\n",
	},
	"FuzzPasswd": {
		"x\na:b:notanumber:0:g:/h:/s\n+::::::\nroot:x:0:0:root:/root:/bin/sh\r\nbig:x:4294967296:0::/:/bin/sh\n",
	},
	"FuzzTimezone":                   {"Europe/Berlin\n", "/usr/share/zoneinfo/../../etc/passwd", "TZif2"},
	"FuzzTimedatectlNTPSynchronized": {"yes\n", "no\n", "maybe", "NTPSynchronized=yes\n", "yes\nno\n"},
	"FuzzChronyTracking": {
		chronyTrackingSynced, chronyTrackingUnsynced,
		withLeap("Insert second"), withLeap("Delete second"), withLeap("Invalid"),
		"00000000,,0,0.0,0.0,0.0,0.0,0.000,0.000,0.000,1.0,1.0,0.0\n",
		"506 Cannot talk to daemon\n",
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
		for _, dir := range fuzzSeedDirs {
			for _, g := range globs {
				for _, m := range globCap(f, c, filepath.Join(dir, g)) {
					if fi, err := os.Stat(m); err != nil || !fi.Mode().IsRegular() {
						continue
					}
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

func FuzzScalarInt(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := ScalarInt(b); err != nil {
			return
		}
	})
}

func FuzzScalarString(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		s := ScalarString(b)
		if !utf8.ValidString(s) || s != strings.TrimSpace(s) {
			t.Fatalf("ScalarString = %q", s)
		}
	})
}

func FuzzMillidegrees(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Millidegrees(b)
		if err == nil && (math.IsNaN(c) || math.IsInf(c, 0)) {
			t.Fatalf("Millidegrees = %v", c)
		}
	})
}

func FuzzOperstate(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if s := Operstate(b); !slices.Contains([]string{"up", "down", "unknown", "dormant"}, s) {
			t.Fatalf("Operstate = %q", s)
		}
	})
}

func FuzzLinkSpeed(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, ok := LinkSpeed(b); (ok && v <= 0) || (!ok && v != 0) {
			t.Fatalf("LinkSpeed = %d, %v", v, ok)
		}
	})
}

func FuzzBlockSize(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, err := BlockSize(b); err == nil && v%SectorSize != 0 {
			t.Fatalf("BlockSize = %d is not whole sectors", v)
		}
	})
}

func FuzzRotational(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, err := Rotational(b); err != nil && v {
			t.Fatalf("Rotational: true with error %v", err)
		}
	})
}

func FuzzRemovable(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, err := Removable(b); err != nil && v {
			t.Fatalf("Removable: true with error %v", err)
		}
	})
}

func FuzzResolvConf(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		a := ResolvConf(b)
		if len(a) > MaxNameservers {
			t.Fatalf("%d nameservers", len(a))
		}
		for _, x := range a {
			if !x.IsValid() {
				t.Fatalf("invalid address")
			}
		}
	})
}

// FuzzResolvedInUse splits its input at the first NUL into the etc and run
// files; without a NUL the run file is absent.
func FuzzResolvedInUse(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		etc, run, found := bytes.Cut(b, []byte{0})
		if !found {
			run = nil
		} else if run == nil {
			run = []byte{}
		}
		if !ResolvedInUse(etc, run) {
			return
		}
		servers := ResolvConf(etc)
		if run == nil || len(servers) == 0 {
			t.Fatalf("in use without run file or etc server")
		}
		for _, s := range servers {
			if s.Unmap() != netip.MustParseAddr("127.0.0.53") && s.Unmap() != netip.MustParseAddr("127.0.0.54") {
				t.Fatalf("in use with etc server %v", s)
			}
		}
	})
}

func FuzzPasswd(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		for uid, name := range Passwd(b) {
			if name == "" || strings.ContainsAny(name, ":\n") || !utf8.ValidString(name) {
				t.Fatalf("uid %d: name %q", uid, name)
			}
		}
	})
}

// FuzzTimezone uses its input as /etc/localtime, /etc/timezone and the link
// target at once.
func FuzzTimezone(f *testing.F) {
	addCaptureSeeds(f)
	if b, err := os.ReadFile(filepath.Join("..", "testdata", "tz", "America_New_York")); err == nil {
		f.Add(b)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, b []byte) {
		tz, err := Timezone(b, b, string(b), now)
		if tz.Name != "" && !validZoneName(tz.Name) && tz.NameSource != TZSourceDefault {
			t.Fatalf("name %q", tz.Name)
		}
		if err == nil && tz.Source == "" {
			t.Fatalf("no error but no source")
		}
	})
}

func FuzzTimedatectlNTPSynchronized(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := TimedatectlNTPSynchronized(b)
		if err == nil && v != (strings.TrimSpace(string(b)) == "yes") {
			t.Fatalf("%q = %v", b, v)
		}
		if err != nil && v {
			t.Fatalf("true with error %v", err)
		}
	})
}

func FuzzChronyTracking(f *testing.F) {
	addCaptureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		if v, err := ChronyTracking(b); err != nil && v {
			t.Fatalf("true with error %v", err)
		}
	})
}
