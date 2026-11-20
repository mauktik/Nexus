// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/system-provider/test-cases.md TC-HP-71, TC-HP-72, TC-HP-73, TC-HP-74, TC-HP-75, TC-HP-76, TC-HP-77, TC-HP-78, TC-HP-E18, TC-HP-E19, TC-HP-ERR18, TC-HP-ERR19, TC-HP-ERR20, TC-HP-ERR28.
// Owner WI: 4.1. The host stack (newHostStack) is in identity_test.go, the
// synthetic sensor, cpufreq and cpuinfo writers in cpu_test.go, and
// assertExplicit in explicit_test.go.

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// TC-HP-71: Identity on both captures matches the hosts' tools. Owner WI 4.1. Requirements: FR-5, FR-4, NFR-3, FR-15.
//
// - Setup: host stack per capture.
// - Steps: call the identity handler; decode into `apitypes.List[apitypes.Resource[spec.IdentityProperties]]`.
// - Expected:
//   - one item named `current`, `host: "tc"`;
//   - `hostname` = `tools/hostname`; `os` = the capture's os-release ID, VERSION_ID, PRETTY_NAME; `kernelRelease` and `architecture` = `tools/uname-rm`; `initSystem` = the platform's;
//   - `bootTime` = `btime` as RFC 3339; `uptimeSeconds` = `proc/uptime` first field;
//   - `virtualization.type` = `tools/systemd-detect-virt` (where captured);
//   - `dmi` fields equal the five files, trimmed; `timezone` per TC-HP-45; `assertExplicit` passes.
func TestTCHP71_IdentityOnBothCapturesMatchesTheHosts(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			p := s.identity() // oneItem checks current, host tc and assertExplicit

			if want := toolText(t, capture, "hostname"); !eqStr(p.Hostname, want) {
				t.Errorf("hostname %s, want %q", show(p.Hostname), want)
			}
			osr := toolOS(t, capture)
			if !eqStr(p.OS.ID, osr["ID"]) || !eqStr(p.OS.VersionID, osr["VERSION_ID"]) ||
				!eqStr(p.OS.PrettyName, osr["PRETTY_NAME"]) {
				t.Errorf("os = %s %s %s, want %q %q %q", show(p.OS.ID), show(p.OS.VersionID), show(p.OS.PrettyName),
					osr["ID"], osr["VERSION_ID"], osr["PRETTY_NAME"])
			}
			rm := strings.Fields(toolText(t, capture, "uname-rm"))
			if !eqStr(p.KernelRelease, rm[0]) || !eqStr(p.Architecture, rm[1]) {
				t.Errorf("kernel %s arch %s, want %v", show(p.KernelRelease), show(p.Architecture), rm)
			}
			if p.InitSystem != string(s.plat.Init) {
				t.Errorf("initSystem %q, want %q", p.InitSystem, s.plat.Init)
			}

			var btime int64
			for _, l := range strings.Split(string(capFile(t, capture, "fs", pathStat)), "\n") {
				if v, ok := strings.CutPrefix(l, "btime "); ok {
					btime = int64(atoiT(t, v))
				}
			}
			wantBoot := time.Unix(btime, 0).UTC()
			raw, _ := json.Marshal(p.BootTime)
			if p.BootTime == nil || !p.BootTime.Equal(wantBoot) || string(raw) != `"`+wantBoot.Format(time.RFC3339)+`"` {
				t.Errorf("bootTime %s, want %s", raw, wantBoot.Format(time.RFC3339))
			}
			up, err := strconv.ParseFloat(strings.Fields(string(capFile(t, capture, "fs", "proc/uptime")))[0], 64)
			if err != nil {
				t.Fatal(err)
			}
			if p.UptimeSeconds == nil || *p.UptimeSeconds != up {
				t.Errorf("uptimeSeconds %s, want %v", show(p.UptimeSeconds), up)
			}

			if b, err := os.ReadFile(filepath.Join("testdata", capture, "tools", "systemd-detect-virt")); err == nil {
				if want := strings.TrimSpace(string(b)); p.Virtualization.Type != want {
					t.Errorf("virtualization.type %q, want systemd-detect-virt's %q", p.Virtualization.Type, want)
				}
			}
			if p.Virtualization.Type != "docker" || p.Virtualization.Kind != "container" || p.Virtualization.Evidence == nil {
				t.Errorf("virtualization %+v, want docker container with evidence (both captures ran in docker)",
					p.Virtualization)
			}

			for field, got := range map[string]*string{"sys_vendor": p.DMI.SystemVendor, "product_name": p.DMI.ProductName,
				"product_version": p.DMI.ProductVersion, "bios_vendor": p.DMI.BIOSVendor, "bios_version": p.DMI.BIOSVersion} {
				want := strings.TrimSpace(string(capFile(t, capture, "fs", "sys/class/dmi/id/"+field)))
				if !eqStr(got, want) {
					t.Errorf("dmi %s = %s, want %q", field, show(got), want)
				}
			}

			// TC-HP-45: UBU's /etc/localtime links to Etc/UTC; ALP has none.
			tz := p.Timezone
			switch capture {
			case capUBU:
				if !eqStr(tz.Name, "Etc/UTC") || tz.OffsetSeconds == nil || *tz.OffsetSeconds != 0 ||
					!eqStr(tz.Source, "/etc/localtime link") {
					t.Errorf("timezone %+v, want Etc/UTC 0 from the link", tz)
				}
			case capALP:
				if !eqStr(tz.Name, "UTC") || tz.OffsetSeconds == nil || *tz.OffsetSeconds != 0 ||
					!eqStr(tz.Source, "default (no /etc/localtime)") {
					t.Errorf("timezone %+v, want the UTC default", tz)
				}
			}
		})
	}
}

// TC-HP-72: Identity and capabilities never disagree on OS and init system. Owner WI 4.1. Requirements: FR-5.
//
// - Setup: host stack with `Platform: nil` (production default) on the dev machine.
// - Steps: call identity; compute `platform.Current()`.
// - Expected: `os.id`, `os.versionId`, `os.prettyName`, `initSystem` equal `platform.Current()`'s, which is what `internal/core/capabilities.go` reports.
func TestTCHP72_IdentityAndCapabilitiesNeverDisagreeOnOS(t *testing.T) {
	s := newHostStackWith(t, capUBU, nil)
	p := s.identity()
	cur := platform.Current()
	same := func(field string, got *string, want string) {
		t.Helper()
		if (want == "" && got != nil) || (want != "" && !eqStr(got, want)) {
			t.Errorf("os.%s = %s, platform.Current() has %q", field, show(got), want)
		}
	}
	same("id", p.OS.ID, cur.OS.ID)
	same("versionId", p.OS.VersionID, cur.OS.VersionID)
	same("prettyName", p.OS.PrettyName, cur.OS.PrettyName)
	if p.InitSystem != string(cur.Init) {
		t.Errorf("initSystem %q, platform.Current() has %q", p.InitSystem, cur.Init)
	}
}

// TC-HP-73: Clock synchronization on systemd comes from timedatectl. Owner WI 4.1. Requirements: FR-5, CON-1, CON-3.
//
// - Setup: host stack (UBU, init systemd); F-RUN answers `tools/timedatectl`; variants where F-FS has `run/systemd/timesync`, `run/chrony/chronyd.pid`, `run/ntpd.pid`, none.
// - Steps: identity handler.
// - Expected: exactly one F-RUN call `{"/usr/bin/timedatectl","show","--property=NTPSynchronized","--value"}`; `clock.synchronized` = `"true"` for `yes`, `"false"` for `no`; `clock.source` = `systemd-timesyncd`, `chrony`, `ntpd`, `kernel` per variant.
func TestTCHP73_ClockSynchronizationOnSystemdComesFromTimedatectl(t *testing.T) {
	variants := []struct{ marker, source string }{
		{"run/systemd/timesync", "systemd-timesyncd"},
		{"run/chrony/chronyd.pid", "chrony"},
		{"run/ntpd.pid", "ntpd"},
		{"", "kernel"},
	}
	for _, v := range variants {
		for _, answer := range []struct{ out, want string }{{"", "true"}, {"no\n", "false"}} {
			t.Run(v.source+"/"+answer.want, func(t *testing.T) {
				s := newHostStack(t, capUBU)
				if answer.out != "" {
					s.run.set(argvTimedatectl, []byte(answer.out), 0)
				} else if toolText(t, capUBU, "timedatectl") != "yes" {
					t.Fatal("tools/timedatectl is not yes")
				}
				if v.marker != "" {
					s.ffs.replace(v.marker, []byte("1\n"))
				}
				p := s.identity()
				calls := s.run.recorded()
				if len(calls) != 1 || argvKey(calls[0]) != argvKey(argvTimedatectl) {
					t.Errorf("F-RUN calls %q, want exactly %q", calls, argvTimedatectl)
				}
				if p.Clock.Synchronized != answer.want || !eqStr(p.Clock.Source, v.source) {
					t.Errorf("clock %+v (source %s), want %s from %s", p.Clock, show(p.Clock.Source), answer.want, v.source)
				}
				checkClockStatOnly(t, s.ffs)
			})
		}
	}
}

// checkClockStatOnly is the WI's clock-collector check: chronyc and the
// three markers are only ever stat'ed, never opened.
func checkClockStatOnly(t *testing.T, ffs *fakeFS) {
	t.Helper()
	for _, name := range []string{"usr/bin/chronyc", "run/systemd/timesync", "run/chrony/chronyd.pid", "run/ntpd.pid"} {
		for _, c := range ffs.recorded() {
			if c.name == name && c.op != "stat" {
				t.Errorf("clock collector made %s", c)
			}
		}
	}
}

// TC-HP-74: Clock synchronization elsewhere comes from chronyc or is unknown. Owner WI 4.1. Requirements: FR-5.
//
// - Setup: host stack (ALP, init openrc); variant 1: F-FS has `usr/bin/chronyc`, F-RUN answers `tools/chronyc-tracking` (or a capture-derived line); variant 2: no chronyc.
// - Steps: identity handler.
// - Expected: v1: one call `{"/usr/bin/chronyc","-c","tracking"}`, `synchronized` per field 14, source `chrony`; v2: no F-RUN call, `synchronized "unknown"`, `unavailable.synchronized` (or `clock`) = `not supported: no supported time-sync query (systemd-timedated, chrony)`.
func TestTCHP74_ClockSynchronizationElsewhereComesFromChronycOr(t *testing.T) {
	// Neither capture has chronyc (fakes_test.go); these lines follow
	// chronyc -c tracking's CSV, field 14 the leap status.
	const tracking = "A29FC87B,ntp.example,3,1763174253.123456789,0.000012,-0.000003,0.000020,-12.345,0.001,0.010,0.012345,0.001234,64.5,%s\n"
	for _, v := range []struct{ leap, want string }{{"Normal", "true"}, {"Not synchronised", "false"}} {
		t.Run("chronyc/"+v.want, func(t *testing.T) {
			s := newHostStack(t, capALP)
			s.ffs.replace("usr/bin/chronyc", []byte("ELF"))
			s.run.set(argvChronyc, fmt.Appendf(nil, tracking, v.leap), 0)
			p := s.identity()
			if n := s.run.count(argvChronyc); n != 1 || len(s.run.recorded()) != 1 {
				t.Errorf("F-RUN calls %q, want one %q", s.run.recorded(), argvChronyc)
			}
			if p.Clock.Synchronized != v.want || !eqStr(p.Clock.Source, "chrony") {
				t.Errorf("clock %+v (source %s), want %s from chrony", p.Clock, show(p.Clock.Source), v.want)
			}
			checkClockStatOnly(t, s.ffs)
		})
	}
	t.Run("none", func(t *testing.T) {
		s := newHostStack(t, capALP)
		p := s.identity()
		if calls := s.run.recorded(); len(calls) != 0 {
			t.Errorf("F-RUN calls %q, want none", calls)
		}
		const why = "not supported: no supported time-sync query (systemd-timedated, chrony)"
		if p.Clock.Synchronized != "unknown" || p.Clock.Unavailable["synchronized"] != why {
			t.Errorf("clock %+v, want unknown with %q", p.Clock, why)
		}
		if s.ffs.count("stat", "usr/bin/chronyc") != 1 {
			t.Errorf("usr/bin/chronyc stat'ed %d times, want 1", s.ffs.count("stat", "usr/bin/chronyc"))
		}
		checkClockStatOnly(t, s.ffs)
	})
}

// clockSyscalls are the @clock group's calls and their Go spellings
// (A-14, D-8).
var clockSyscalls = []string{"Adjtimex", "ClockAdjtime", "adjtimex", "clock_adjtime", "Settimeofday", "ClockSettime",
	"SYS_ADJTIMEX", "SYS_CLOCK_ADJTIME"}

// scanClockSyscalls returns "file: name" for each clock syscall named in a
// non-test .go file under root.
func scanClockSyscalls(t *testing.T, root string) (hits []string, scanned int) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, name := range clockSyscalls {
			if strings.Contains(string(b), name) {
				hits = append(hits, path+": "+name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits, scanned
}

// TC-HP-75: The provider never makes a clock syscall. Owner WI 4.1. Requirements: CON-1, NFR-4.
//
// - Setup: none.
// - Steps: grep every non-test `.go` file under `internal/providers/host` (recursively) for `Adjtimex`, `ClockAdjtime`, `adjtimex`, `clock_adjtime`, `Settimeofday`, `ClockSettime`, `SYS_ADJTIMEX`, `SYS_CLOCK_ADJTIME`.
// - Expected: no match; a planted temp copy with `unix.Adjtimex` is caught.
func TestTCHP75_TheProviderNeverMakesAClockSyscall(t *testing.T) {
	hits, scanned := scanClockSyscalls(t, ".")
	if len(hits) != 0 {
		t.Errorf("clock syscalls named in the provider: %v", hits)
	}
	for _, f := range []string{"clock.go", "provider.go", "source.go", "hostparse/timesync.go"} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("scan root is not the provider: %v", err)
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files", scanned)
	}

	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package host\n\nimport \"golang.org/x/sys/unix\"\n\nfunc f() { var tx unix.Timex; _, _ = unix.Adjtimex(&tx) }\n"
	if err := os.WriteFile(filepath.Join(planted, "sub", "planted.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "sub", "planted_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _ = scanClockSyscalls(t, planted)
	if len(hits) != 1 || !strings.HasSuffix(hits[0], "planted.go: Adjtimex") {
		t.Errorf("planted copy: hits %v, want exactly planted.go: Adjtimex", hits)
	}
}

// TC-HP-76: CPU detail from both captures. Owner WI 4.1. Requirements: FR-6, FR-15.
//
// - Setup: host stack per capture, ready snapshot.
// - Steps: CPU handler; decode into `spec.CPUProperties`.
// - Expected:
//   - `model` = cpuinfo first model name; `logicalCpus` = `tools/nproc`; `physicalCores` = distinct (`physical_package_id`, `core_id`) pairs, or cpuinfo `cpu cores` when topology is absent;
//   - `load` = `proc/loadavg`; per-CPU `frequencyMhz` = `scaling_cur_freq`/1000 where present, else cpuinfo MHz; `frequencyMhz` = their mean;
//   - `temperatures`: one per hwmon `temp*_input` (label = `temp*_label`, else hwmon `name`) and per thermal zone (label = `type`), `celsius` = value/1000, `source` hwmon/thermal;
//   - usage fields equal TC-HP-61's; `sampler.state` ready; `assertExplicit` passes.
func TestTCHP76_CPUDetailFromBothCaptures(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		for _, synth := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/synthetic=%v", capture, synth), func(t *testing.T) {
				s := newHostStack(t, capture)
				var freq []float64
				var ss []synthSensor
				if synth {
					// cpufreq for cpu0 and cpu1 only: cpu2 and cpu3 fall
					// back to cpuinfo MHz.
					info, mhz := synthCPUInfo(4, 4)
					s.ffs.replace("proc/cpuinfo", info)
					freq = append(synthCPUFreq(s.ffs, 2), mhz[2:]...)
					ss = synthSensors(s.ffs)
				}
				_, snap := s.ready()
				p := s.cpu()
				now := s.clk.Now()

				if synth {
					if !eqStr(p.Model, "Synthetic(R) CPU @ 2.40GHz") {
						t.Errorf("model %s", show(p.Model))
					}
				} else if p.Model != nil || p.Unavailable["model"] != "not reported: no model name in /proc/cpuinfo" {
					t.Errorf("aarch64 capture: model %s with %q, want not reported", show(p.Model), p.Unavailable["model"])
				}
				if nproc := atoiT(t, toolText(t, capture, "nproc")); p.LogicalCPUs == nil || *p.LogicalCPUs != nproc {
					t.Errorf("logicalCpus %s, want nproc %d", show(p.LogicalCPUs), nproc)
				}
				pairs := map[string]bool{}
				for i := range 4 {
					dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/", i)
					pairs[strings.TrimSpace(string(capFile(t, capture, "fs", dir+"physical_package_id")))+"/"+
						strings.TrimSpace(string(capFile(t, capture, "fs", dir+"core_id")))] = true
				}
				if p.PhysicalCores == nil || *p.PhysicalCores != len(pairs) {
					t.Errorf("physicalCores %s, want %d distinct pairs", show(p.PhysicalCores), len(pairs))
				}
				la := strings.Fields(string(s.ffs.rawRead(pathLoadavg)))
				for i, got := range []*float64{p.Load.Load1, p.Load.Load5, p.Load.Load15} {
					want, _ := strconv.ParseFloat(la[i], 64)
					if got == nil || *got != want {
						t.Errorf("load[%d] %s, want %v", i, show(got), want)
					}
				}

				if len(p.PerCPU) != 4 {
					t.Fatalf("perCpu %+v, want 4", p.PerCPU)
				}
				for i, c := range p.PerCPU {
					if c.CPU != i {
						t.Errorf("perCpu[%d].cpu = %d", i, c.CPU)
					}
					if want := mustVal(t, snap, now, "per-CPU usage", snap.perCPU[i].usage); c.UsagePercent == nil ||
						*c.UsagePercent != want {
						t.Errorf("cpu%d usage %s, want the snapshot's %v", i, show(c.UsagePercent), want)
					}
					if _, ok := c.Unavailable["frequencyMhz"]; synth && (c.FrequencyMHz == nil || *c.FrequencyMHz != freq[i] || ok) {
						t.Errorf("cpu%d frequency %s %v, want %v with no reason", i, show(c.FrequencyMHz), c.Unavailable, freq[i])
					}
				}
				if synth && (p.FrequencyMHz == nil || math.Abs(*p.FrequencyMHz-avgOf(freq)) > 1e-9) {
					t.Errorf("frequencyMhz %s, want the mean %v", show(p.FrequencyMHz), avgOf(freq))
				}

				if synth {
					if len(p.Temperatures) != len(ss) {
						t.Errorf("%d temperatures, want %d", len(p.Temperatures), len(ss))
					}
					for i, sn := range ss {
						if i < len(p.Temperatures) {
							g := p.Temperatures[i]
							src := "hwmon"
							if strings.HasPrefix(sn.path, "sys/class/thermal") {
								src = "thermal"
							}
							if g.Source != src || g.Celsius == nil || *g.Celsius != float64(sn.milliC)/1000 {
								t.Errorf("temperatures[%d] = %+v, want %s %v", i, g, src, float64(sn.milliC)/1000)
							}
						}
					}
				} else if len(p.Temperatures) != 0 {
					t.Errorf("temperatures %+v on a capture without sensors", p.Temperatures)
				}

				// TC-HP-61's values, through the same snapshot.
				for name, c := range map[string]struct {
					got *float64
					v   rateVal
				}{
					"usagePercent": {p.UsagePercent, snap.cpu.usage}, "userPercent": {p.Times.UserPercent, snap.cpu.user},
					"systemPercent": {p.Times.SystemPercent, snap.cpu.system},
					"iowaitPercent": {p.Times.IowaitPercent, snap.cpu.iowait},
					"stealPercent":  {p.Times.StealPercent, snap.cpu.steal},
				} {
					if want := mustVal(t, snap, now, name, c.v); c.got == nil || *c.got != want {
						t.Errorf("%s %s, want %v", name, show(c.got), want)
					}
				}
				checkCPU(t, capture, snap, now)
				if p.Sampler.State != spec.SamplerReady {
					t.Errorf("sampler.state %q, want ready", p.Sampler.State)
				}
			})
		}
	}
}

// TC-HP-77: Memory detail follows the free definitions. Owner WI 4.1. Requirements: FR-6, NFR-3.
//
// - Setup: host stack per capture.
// - Steps: memory handler.
// - Expected:
//   - from the captured meminfo (×1024): `usedBytes` = MemTotal − MemAvailable; `usedPercent` = used/total × 100; `cachedBytes` = Cached + SReclaimable; `sharedBytes` = Shmem; `swapUsedBytes` = SwapTotal − SwapFree; `buffersBytes`, `freeBytes`, `availableBytes`, `swapFreeBytes` direct;
//   - `totalBytes` = `tools/free` total exactly; `availableBytes` within ±1% of free's available;
//   - `pressure.{cpu,memory,io}` equal the PSI files where present; `assertExplicit` passes; the resource is not marked untrusted.
func TestTCHP77_MemoryDetailFollowsTheFreeDefinitions(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			p := s.memory()

			mi := map[string]uint64{}
			for _, l := range strings.Split(string(capFile(t, capture, "fs", pathMeminfo)), "\n") {
				f := strings.Fields(l)
				if len(f) == 3 && f[2] == "kB" {
					mi[strings.TrimSuffix(f[0], ":")] = u64T(t, f[1]) * 1024
				}
			}
			used := mi["MemTotal"] - mi["MemAvailable"]
			for name, c := range map[string]struct {
				got  *uint64
				want uint64
			}{
				"usedBytes": {p.UsedBytes, used}, "cachedBytes": {p.CachedBytes, mi["Cached"] + mi["SReclaimable"]},
				"sharedBytes": {p.SharedBytes, mi["Shmem"]}, "swapUsedBytes": {p.SwapUsedBytes, mi["SwapTotal"] - mi["SwapFree"]},
				"buffersBytes": {p.BuffersBytes, mi["Buffers"]}, "freeBytes": {p.FreeBytes, mi["MemFree"]},
				"availableBytes": {p.AvailableBytes, mi["MemAvailable"]}, "swapFreeBytes": {p.SwapFreeBytes, mi["SwapFree"]},
				"swapTotalBytes": {p.SwapTotalBytes, mi["SwapTotal"]}, "totalBytes": {p.TotalBytes, mi["MemTotal"]},
			} {
				if c.got == nil || *c.got != c.want {
					t.Errorf("%s = %s, want %d", name, show(c.got), c.want)
				}
			}
			if want := float64(used) / float64(mi["MemTotal"]) * 100; p.UsedPercent == nil || *p.UsedPercent != want {
				t.Errorf("usedPercent %s, want %v", show(p.UsedPercent), want)
			}

			free := strings.Fields(strings.Split(toolText(t, capture, "free"), "\n")[1])
			if total := u64T(t, free[1]); p.TotalBytes == nil || *p.TotalBytes != total {
				t.Errorf("totalBytes %s, want free's %d exactly", show(p.TotalBytes), total)
			}
			if avail := float64(u64T(t, free[6])); p.AvailableBytes == nil ||
				math.Abs(float64(*p.AvailableBytes)-avail) > avail/100 {
				t.Errorf("availableBytes %s, not within 1%% of free's %v", show(p.AvailableBytes), avail)
			}

			for x, got := range map[string]*spec.PSI{"cpu": p.Pressure.CPU, "memory": p.Pressure.Memory, "io": p.Pressure.IO} {
				lines := strings.Split(strings.TrimSpace(string(capFile(t, capture, "fs", "proc/pressure/"+x))), "\n")
				if got == nil || got.Some == nil || got.Full == nil || len(lines) != 2 {
					t.Errorf("pressure.%s = %+v, want some and full", x, got)
					continue
				}
				for i, avg := range []*spec.PSIAverages{got.Some, got.Full} {
					var a10, a60, a300 float64
					var total uint64
					var kind string
					if _, err := fmt.Sscanf(lines[i], "%s avg10=%f avg60=%f avg300=%f total=%d", &kind, &a10, &a60, &a300,
						&total); err != nil {
						t.Fatal(err)
					}
					if *avg.Avg10 != a10 || *avg.Avg60 != a60 || *avg.Avg300 != a300 {
						t.Errorf("pressure.%s.%s = %v %v %v, want %v %v %v", x, kind, *avg.Avg10, *avg.Avg60, *avg.Avg300,
							a10, a60, a300)
					}
				}
			}
			if spec.MemoryGet.Op().Untrusted {
				t.Error("host.memory.get is marked untrusted; it carries no host text")
			}
		})
	}
}

// TC-HP-78: CPU sampler fields report the sampler state, other fields stay live. Owner WI 4.1. Requirements: FR-11, FR-12.
//
// - Setup: host stack (UBU) in three states: no run (not-running), one sample (warming-up), stale (TC-HP-66).
// - Steps: CPU handler in each state.
// - Expected: `usagePercent`, `times`, each `perCpu[].usagePercent` null with `not running`, `warming up`, or `stale: last sample Ns old` respectively; `model`, `logicalCpus`, `load`, `frequencyMhz` populated in all three; `sampler.state` matches; `assertExplicit` passes.
func TestTCHP78_CPUSamplerFieldsReportTheSamplerState(t *testing.T) {
	states := []struct {
		name, state, why string
		drive            func(s *hostStack)
	}{
		{"not-running", spec.SamplerNotRunning, reasonNotRunning, func(*hostStack) {}},
		{"warming-up", spec.SamplerWarmingUp, reasonWarmingUp, func(s *hostStack) { s.start(); s.waitSeq(1) }},
		{"stale", spec.SamplerStale, "stale: last sample 31s old", func(s *hostStack) {
			s.ready()
			s.clk.Advance(31 * time.Second)
		}},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			s := newHostStack(t, capUBU)
			info, _ := synthCPUInfo(4, 4)
			s.ffs.replace("proc/cpuinfo", info)
			synthCPUFreq(s.ffs, 4)
			st.drive(s)
			p := s.cpu()
			if p.Sampler.State != st.state {
				t.Errorf("sampler.state %q, want %q", p.Sampler.State, st.state)
			}
			nulls := map[string]struct {
				got *float64
				why string
			}{
				"usagePercent":        {p.UsagePercent, p.Unavailable["usagePercent"]},
				"times.userPercent":   {p.Times.UserPercent, p.Times.Unavailable["userPercent"]},
				"times.systemPercent": {p.Times.SystemPercent, p.Times.Unavailable["systemPercent"]},
				"times.iowaitPercent": {p.Times.IowaitPercent, p.Times.Unavailable["iowaitPercent"]},
				"times.stealPercent":  {p.Times.StealPercent, p.Times.Unavailable["stealPercent"]},
			}
			for i, c := range p.PerCPU {
				nulls[fmt.Sprintf("perCpu[%d].usagePercent", i)] = struct {
					got *float64
					why string
				}{c.UsagePercent, c.Unavailable["usagePercent"]}
			}
			if len(p.PerCPU) != 4 {
				t.Errorf("perCpu %+v, want 4 entries", p.PerCPU)
			}
			for name, c := range nulls {
				if c.got != nil || c.why != st.why {
					t.Errorf("%s = %s with %q, want null with %q", name, show(c.got), c.why, st.why)
				}
			}
			if p.Model == nil || p.LogicalCPUs == nil || p.Load.Load1 == nil || p.FrequencyMHz == nil {
				t.Errorf("live fields model %s logicalCpus %s load1 %s frequencyMhz %s, want all populated",
					show(p.Model), show(p.LogicalCPUs), show(p.Load.Load1), show(p.FrequencyMHz))
			}
		})
	}
}

// TC-HP-E18: A VM with no sensors reports an empty temperature list with a reason. Owner WI 4.1. Requirements: FR-6, FR-12.
//
// - Setup: UBU with SP-VM-NO-SENSORS.
// - Steps: CPU handler.
// - Expected: `temperatures []`; `unavailable.temperatures` = `not reported: no temperature sensors (common in VMs)`.
func TestTCHPE18_AVMWithNoSensorsReportsAn(t *testing.T) {
	s := newHostStack(t, capUBU)
	spVMNoSensors(t, s.ffs)
	out, err := s.p.cpu(context.Background(), spec.NoInput{})
	p := oneItem[spec.CPUProperties](t, spec.CPUType, out, err)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"temperatures":[]`) {
		t.Errorf("temperatures is not [] in %s", raw)
	}
	if got, want := p.Unavailable["temperatures"], "not reported: no temperature sensors (common in VMs)"; got != want {
		t.Errorf("temperatures reason %q, want %q", got, want)
	}
	if s.ffs.count("readdir", "sys/class/hwmon") != 1 || s.ffs.count("readdir", "sys/class/thermal") != 1 {
		t.Errorf("hwmon and thermal listed %d and %d times, want once each", s.ffs.count("readdir", "sys/class/hwmon"),
			s.ffs.count("readdir", "sys/class/thermal"))
	}
}

// TC-HP-E19: Hostname comes from the kernel file, not os.Hostname. Owner WI 4.1. Requirements: FR-5, ASM-1.
//
// - Setup: UBU F-FS with `replace("proc/sys/kernel/hostname", "tc-hp-host\n")`.
// - Steps: identity handler.
// - Expected: `hostname "tc-hp-host"` (differs from the dev machine's `os.Hostname()`); F-FS recorded `proc/sys/kernel/hostname`.
func TestTCHPE19_HostnameComesFromTheKernelFileNot(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("proc/sys/kernel/hostname", []byte("tc-hp-host\n"))
	p := s.identity()
	if !eqStr(p.Hostname, "tc-hp-host") {
		t.Errorf("hostname %s, want tc-hp-host", show(p.Hostname))
	}
	if h, err := os.Hostname(); err == nil && h == "tc-hp-host" {
		t.Fatal("the dev machine is named tc-hp-host; the test cannot tell the sources apart")
	}
	if s.ffs.count("open", "proc/sys/kernel/hostname") != 1 {
		t.Errorf("proc/sys/kernel/hostname opened %d times, want 1", s.ffs.count("open", "proc/sys/kernel/hostname"))
	}
}

// TC-HP-ERR18: Clock command failures are reported with a reason. Owner WI 4.1. Requirements: FR-5, FR-12.
//
// - Setup: UBU with F-RUN for timedatectl: (a) exit 1 with `Failed to connect to bus`; (b) an `ErrNotStarted` (ENOENT) error; (c) `hang` (timeout 250 ms); (d) exit 0 with `maybe`; (e) an `ErrNotStarted` whose cause is `EACCES` (not ENOENT). F-RUN builds (b) and (e) as `fmt.Errorf("%w: %w", svcctl.ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: argv0, Err: cause})`, the shape `realRun` returns (`internal/svcctl/svcctl.go:205`).
// - Steps: identity handler.
// - Expected: `clock.synchronized` unavailable with (a) `command failed: /usr/bin/timedatectl exit 1`, (b) `not present: /usr/bin/timedatectl` (Finding 7), (c) `timed out after 250ms: /usr/bin/timedatectl`, (d) `unrecognized format: …`, (e) `command failed: /usr/bin/timedatectl not started` (Finding 7); the handler returns within 350 ms in (c); the rest of identity is populated; the output never contains the command's text (`Failed to connect`).
func TestTCHPERR18_ClockCommandFailuresAreReportedWithA(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *fakeRun)
		why   string
	}{
		{"a exit 1", func(r *fakeRun) { r.set(argvTimedatectl, []byte("Failed to connect to bus: No such file\n"), 1) },
			"command failed: /usr/bin/timedatectl exit 1"},
		{"b not installed", func(r *fakeRun) { r.notStarted(argvTimedatectl, syscall.ENOENT) },
			"not present: /usr/bin/timedatectl"},
		{"c hang", func(r *fakeRun) { r.hang(argvTimedatectl) }, "timed out after 250ms: /usr/bin/timedatectl"},
		{"d maybe", func(r *fakeRun) { r.set(argvTimedatectl, []byte("maybe\n"), 0) },
			"unrecognized format: /usr/bin/timedatectl"},
		{"e not executable", func(r *fakeRun) { r.notStarted(argvTimedatectl, syscall.EACCES) },
			"command failed: /usr/bin/timedatectl not started"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, capUBU)
			s.timeout(250 * time.Millisecond)
			c.setup(s.run)
			start := time.Now()
			out, err := s.p.identity(context.Background(), spec.NoInput{})
			if took := time.Since(start); took > 350*time.Millisecond {
				t.Errorf("identity took %v, want at most 350ms", took)
			}
			p := oneItem[spec.IdentityProperties](t, spec.IdentityType, out, err)
			if p.Clock.Synchronized != "unknown" || p.Clock.Unavailable["synchronized"] != c.why {
				t.Errorf("clock %+v, want unknown with %q", p.Clock, c.why)
			}
			if p.Hostname == nil || p.KernelRelease == nil || p.OS.ID == nil || p.DMI.SystemVendor == nil {
				t.Errorf("the rest of identity is not populated: %+v", p)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "Failed to connect") || strings.Contains(string(raw), "maybe") {
				t.Errorf("output quotes the command's text: %s", raw)
			}
		})
	}
	// The F-RUN shape of (b) and (e) is realRun's.
	var r fakeRun
	r.table = map[string]runEntry{}
	r.notStarted(argvTimedatectl, syscall.EACCES)
	_, err := r.Run(context.Background(), argvTimedatectl)
	if !strings.Contains(err.Error(), svcctl.ErrNotStarted.Error()) {
		t.Errorf("F-RUN notStarted error %v does not wrap ErrNotStarted", err)
	}
}

// TC-HP-ERR19: A blocked sensor read times out without holding the CPU op. Owner WI 4.1. Requirements: FR-6, FR-12, NFR-1.
//
// - Setup: UBU (or a capture-derived hwmon) with F-FS `block` on one `temp*_input`; timeout 250 ms; goroutine baseline taken with `runtime.NumGoroutine()` before the first call.
// - Steps: call the CPU handler 5 times in sequence while the sensor stays blocked; measure each; then `release()`.
// - Expected:
//   - Each call returns within 350 ms.
//   - Call 1: the blocked sensor is absent from `temperatures` (or has null `celsius`) with `timed out after 250ms: /sys/class/hwmon/…`. Other sensors are present.
//   - Calls 2 to 5: the same sensor reports `still pending: /sys/class/hwmon/…` (Finding 3).
//   - F-FS recorded exactly one `Open` of that sensor path across the 5 calls.
//   - After call 5, `runtime.NumGoroutine()` is at most baseline + 1: one goroutine per stuck source, not one per request.
//   - After `release`, the goroutine count returns to the baseline (`eventually`).
func TestTCHPERR19_ABlockedSensorReadTimesOutWithout(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.timeout(250 * time.Millisecond)
	ss := synthSensors(s.ffs)
	const stuck = "sys/class/hwmon/hwmon0/temp2_input"
	release := s.ffs.block(stuck)
	baseline := runtime.NumGoroutine()

	for call := 1; call <= 5; call++ {
		start := time.Now()
		p := s.cpu()
		if took := time.Since(start); took > 350*time.Millisecond {
			t.Errorf("call %d took %v, want at most 350ms", call, took)
		}
		want := "still pending: /" + stuck
		if call == 1 {
			want = "timed out after 250ms: /" + stuck
		}
		if len(p.Temperatures) != len(ss) {
			t.Fatalf("call %d: temperatures %+v, want %d sensors", call, p.Temperatures, len(ss))
		}
		for i, g := range p.Temperatures {
			if i == 1 { // hwmon0 temp2, the stuck one
				if g.Celsius != nil || g.Unavailable["celsius"] != want {
					t.Errorf("call %d: stuck sensor %+v, want celsius null with %q", call, g, want)
				}
			} else if g.Celsius == nil {
				t.Errorf("call %d: sensor %d (%+v) is null", call, i, g)
			}
		}
	}
	if n := s.ffs.count("open", stuck); n != 1 {
		t.Errorf("%s opened %d times across 5 calls, want 1", stuck, n)
	}
	// Goroutines of finished bounded reads may still be returning; the
	// stuck one stays.
	eventually(t, time.Second, func() bool { return runtime.NumGoroutine() <= baseline+1 },
		"goroutines at most baseline %d + 1 (now %d)", baseline, runtime.NumGoroutine())
	release()
	waitGoroutines(t, baseline)
}

// TC-HP-ERR20: The sandbox profile yields precise reasons for identity, CPU and memory. Owner WI 4.1. Requirements: FR-12, CON-1, NFR-6.
//
// - Setup: UBU with SP-SANDBOX; separately SP-NO-PSI; separately `remove` every `cpufreq/scaling_cur_freq`.
// - Steps: identity, CPU, memory handlers.
// - Expected: each `dmi.*` null with `permission denied: /sys/class/dmi/id/<file>`; `pressure.cpu|memory|io` null with `permission denied: /proc/pressure/<x>` (SP-SANDBOX) or `not present: /proc/pressure/<x> (kernel without PSI)` (SP-NO-PSI); without cpufreq, per-CPU frequency falls back to cpuinfo MHz with no reason; `assertExplicit` passes for all.
func TestTCHPERR20_TheSandboxProfileYieldsPreciseReasonsFor(t *testing.T) {
	dmi := func(p spec.IdentityProperties) map[string]*string {
		return map[string]*string{"systemVendor": p.DMI.SystemVendor, "productName": p.DMI.ProductName,
			"productVersion": p.DMI.ProductVersion, "biosVendor": p.DMI.BIOSVendor, "biosVersion": p.DMI.BIOSVersion}
	}
	files := map[string]string{"systemVendor": "sys_vendor", "productName": "product_name",
		"productVersion": "product_version", "biosVendor": "bios_vendor", "biosVersion": "bios_version"}
	psi := func(p spec.MemoryProperties) map[string]*spec.PSI {
		return map[string]*spec.PSI{"cpu": p.Pressure.CPU, "memory": p.Pressure.Memory, "io": p.Pressure.IO}
	}

	t.Run("SP-SANDBOX", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		spSandbox(t, s.ffs)
		spSandboxRun(s.run)
		id := s.identity()
		for field, got := range dmi(id) {
			want := "permission denied: /sys/class/dmi/id/" + files[field]
			if got != nil || id.DMI.Unavailable[field] != want {
				t.Errorf("dmi.%s = %s with %q, want null with %q", field, show(got), id.DMI.Unavailable[field], want)
			}
		}
		if id.Clock.Unavailable["synchronized"] != "command failed: /usr/bin/timedatectl exit 1" {
			t.Errorf("clock %+v", id.Clock)
		}
		raw, _ := json.Marshal(id)
		if strings.Contains(string(raw), strings.TrimSpace(spSandboxOutput)) {
			t.Errorf("identity quotes timedatectl's output: %s", raw)
		}
		s.cpu()
		m := s.memory()
		for x, got := range psi(m) {
			want := "permission denied: /proc/pressure/" + x
			if got != nil || m.Pressure.Unavailable[x] != want {
				t.Errorf("pressure.%s = %+v with %q, want null with %q", x, got, m.Pressure.Unavailable[x], want)
			}
		}
	})
	t.Run("SP-NO-PSI", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		spNoPSI(s.ffs)
		s.identity()
		s.cpu()
		m := s.memory()
		for x, got := range psi(m) {
			want := "not present: /proc/pressure/" + x + " (kernel without PSI)"
			if got != nil || m.Pressure.Unavailable[x] != want {
				t.Errorf("pressure.%s = %+v with %q, want null with %q", x, got, m.Pressure.Unavailable[x], want)
			}
		}
		if m.TotalBytes == nil {
			t.Error("totalBytes is null without PSI")
		}
	})
	t.Run("no cpufreq", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		info, mhz := synthCPUInfo(4, 4)
		s.ffs.replace("proc/cpuinfo", info)
		synthCPUFreq(s.ffs, 4)
		for i := range 4 {
			s.ffs.remove(fmt.Sprintf("sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", i))
		}
		s.identity()
		s.memory()
		p := s.cpu()
		for i, c := range p.PerCPU {
			if why, ok := c.Unavailable["frequencyMhz"]; c.FrequencyMHz == nil || *c.FrequencyMHz != mhz[i] || ok {
				t.Errorf("cpu%d frequency %s with %q, want cpuinfo's %v with no reason", i, show(c.FrequencyMHz),
					why, mhz[i])
			}
		}
		if p.FrequencyMHz == nil || math.Abs(*p.FrequencyMHz-avgOf(mhz)) > 1e-9 {
			t.Errorf("frequencyMhz %s, want %v", show(p.FrequencyMHz), avgOf(mhz))
		}
	})
}

// TC-HP-ERR28: assertExplicit catches each kind of violation. Owner WI 4.1. Requirements: FR-12.
//
// - Setup: a fake `testing.TB` that records failures; planted JSON-marshalable values.
// - Steps: run `assertExplicit` on: `{a: null}` (no unavailable); `{a: 1, unavailable: {a: "x"}}`; `{a: null, unavailable: {a: ""}}`; `{l: [], unavailable: {l: "r"}}`; nested `{o: {b: null}}`; `{series: [{values: [null]}]}`; `{a: null, unavailable: {a: "permission denied: /x"}}`.
// - Expected: the first four violations and the nested one are reported (except `l: []` with a reason, which is valid); the `series.values` null and the last, well-formed object pass.
func TestTCHPERR28_AssertExplicitCatchesEachKindOfViolation(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		fails bool
	}{
		{"null without unavailable", `{"a": null}`, true},
		{"unavailable names a value", `{"a": 1, "unavailable": {"a": "x"}}`, true},
		{"empty reason", `{"a": null, "unavailable": {"a": ""}}`, true},
		{"empty list with a reason", `{"l": [], "unavailable": {"l": "r"}}`, false},
		{"nested null", `{"o": {"b": null}}`, true},
		{"series values null", `{"series": [{"values": [null]}]}`, false},
		{"well-formed", `{"a": null, "unavailable": {"a": "permission denied: /x"}}`, false},
		// Beyond the TC: the exemption is series[].values only, a
		// reason must name a field, and "unknown" stands for null.
		{"null list item", `{"l": [null]}`, true},
		{"values outside series", `{"other": [{"values": [null]}]}`, true},
		{"unavailable names no field", `{"unavailable": {"gone": "r"}}`, true},
		{"unknown with a reason", `{"s": "unknown", "unavailable": {"s": "not supported: x"}}`, false},
		{"non-empty list with a reason", `{"l": [1], "unavailable": {"l": "r"}}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal([]byte(c.doc), &v); err != nil {
				t.Fatal(err)
			}
			rec := &explicitRecorder{TB: t}
			assertExplicit(rec, v)
			if got := len(rec.errs) > 0; got != c.fails {
				t.Errorf("%s: reported %v (%q), want reported %v", c.doc, got, rec.errs, c.fails)
			}
		})
	}
}
