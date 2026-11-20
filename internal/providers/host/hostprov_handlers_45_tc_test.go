// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: SKIP-only (system-provider)
//
// Tests of planning/system-provider/test-cases.md TC-HP-98, TC-HP-99, TC-HP-100, TC-HP-101, TC-HP-102, TC-HP-103, TC-HP-E26, TC-HP-ERR26, TC-HP-ERR27, TC-HP-134.
// Owner WI: 4.5. The synthetic large host (newLargeHost) and the summary
// helpers are in summary_test.go; the metrics helpers in metrics_test.go.

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// handlerCall is one of the ten handlers, called with its default input
// (lists with limit 1000).
type handlerCall struct {
	name string
	call func() (any, error)
}

func tenHandlers(p *Provider) []handlerCall {
	ctx := context.Background()
	list := spec.ListInput{Limit: spec.MaxLimit}
	none := spec.NoInput{}
	return []handlerCall{
		{"summary", func() (any, error) { return p.summary(ctx, none) }},
		{"identity", func() (any, error) { return p.identity(ctx, none) }},
		{"cpu", func() (any, error) { return p.cpu(ctx, none) }},
		{"memory", func() (any, error) { return p.memory(ctx, none) }},
		{"filesystems", func() (any, error) { return p.filesystems(ctx, list) }},
		{"disks", func() (any, error) { return p.disks(ctx, list) }},
		{"interfaces", func() (any, error) { return p.interfaces(ctx, list) }},
		{"netconfig", func() (any, error) { return p.netconfig(ctx, none) }},
		{"health", func() (any, error) { return p.health(ctx, none) }},
		{"metrics", func() (any, error) { return p.metrics(ctx, spec.MetricsInput{}) }},
	}
}

// TC-HP-98: Summary values equal the detail operations. Owner WI 4.5. Requirements: FR-4, FR-15.
//
// - Setup: host stack per capture, ready snapshot, F-CLK frozen so all calls see the same snapshot.
// - Steps: summary, identity, cpu, memory, filesystems, interfaces, netconfig, health handlers.
// - Expected: each summary field equals the same field of its detail op (identity subset, cpu `model`/`logicalCpus`/`usagePercent`/`load`, memory subset, each filesystem's `mountPoint`/`sizeBytes`/`usedPercent`, each listed interface's state/rates, gateways, reboot state, failed count, OOM kills); no process list in the summary; one top-level `sampler`; `assertExplicit` passes.
func TestTCHP98_SummaryValuesEqualTheDetailOperations(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s, _ := newFSStack(t, capture)
			s.ready()
			raw, err := s.p.summary(context.Background(), spec.NoInput{})
			if err != nil {
				t.Fatal(err)
			}
			sum := s.summary()
			id, cpu, mem := s.identity(), s.cpu(), s.memory()
			fss, ifs, nc, h := s.fsAll(), s.ifaceList(), s.netconfig(), s.health()

			wantIdentitySubset(t, sum.Identity, id)
			wantCPUSubset(t, sum.CPU, cpu)
			if !reflect.DeepEqual(sum.Sampler, cpu.Sampler) || sum.Sampler.State != spec.SamplerReady {
				t.Errorf("sampler %s, cpu op's %s; want ready and equal", show(&sum.Sampler), show(&cpu.Sampler))
			}

			m := sum.Memory
			if show(m.TotalBytes) != show(mem.TotalBytes) || show(m.AvailableBytes) != show(mem.AvailableBytes) ||
				show(m.UsedPercent) != show(mem.UsedPercent) || show(m.SwapTotalBytes) != show(mem.SwapTotalBytes) ||
				show(m.SwapUsedBytes) != show(mem.SwapUsedBytes) || m.TotalBytes == nil {
				t.Errorf("summary memory %s, memory op %s", show(&m), show(&mem))
			}

			if sum.FilesystemsTotal != len(fss) || sum.FilesystemsTruncated != (len(fss) > spec.SummaryMaxFilesystems) {
				t.Errorf("filesystemsTotal %d truncated %v, filesystems op lists %d", sum.FilesystemsTotal,
					sum.FilesystemsTruncated, len(fss))
			}
			for i, f := range sum.Filesystems {
				d := fss[i]
				if f.MountPoint != d.MountPoint || show(f.SizeBytes) != show(d.SizeBytes) ||
					show(f.UsedPercent) != show(d.UsedPercent) || f.Unavailable["sizeBytes"] != d.Unavailable["sizeBytes"] {
					t.Errorf("filesystems[%d] %s, filesystems op %s %s %s", i, show(&f), d.MountPoint,
						show(d.SizeBytes), show(d.UsedPercent))
				}
			}

			var nonLoop []string
			for _, it := range ifs {
				if it.Properties.Loopback == nil || !*it.Properties.Loopback {
					nonLoop = append(nonLoop, it.Name)
				}
			}
			if got := slices.Sorted(slices.Values(summaryIfaceNames(sum))); !slices.Equal(got, nonLoop) ||
				sum.Network.InterfacesTotal != len(nonLoop) || len(nonLoop) == 0 {
				t.Errorf("summary interfaces %v total %d, non-loopback %v", got, sum.Network.InterfacesTotal, nonLoop)
			}
			for _, si := range sum.Network.Interfaces {
				d := ifaceAt(t, ifs, si.Name)
				if si.State != d.State || show(si.RxBytesPerSecond) != show(d.RxBytesPerSecond) ||
					show(si.TxBytesPerSecond) != show(d.TxBytesPerSecond) || si.RxBytesPerSecond == nil {
					t.Errorf("summary %s %s, interfaces op state %s rx %s tx %s", si.Name, show(&si), d.State,
						show(d.RxBytesPerSecond), show(d.TxBytesPerSecond))
				}
			}
			if !reflect.DeepEqual(sum.Network.DefaultGateways, nc.DefaultGateways) {
				t.Errorf("gateways %s, netconfig op %s", show(&sum.Network.DefaultGateways), show(&nc.DefaultGateways))
			}

			hh := sum.Health
			if hh.RebootRequired != h.RebootRequired.State || show(hh.FailedServicesCount) != show(h.FailedServices.Count) ||
				show(hh.OOMKills) != show(h.OOMKills) || hh.Unavailable["rebootRequired"] != h.RebootRequired.Unavailable["state"] {
				t.Errorf("summary health %s, health op reboot %q count %s oom %s", show(&hh), h.RebootRequired.State,
					show(h.FailedServices.Count), show(h.OOMKills))
			}

			b, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{`"topByCpu"`, `"topByMemory"`, `"processSampleSeconds"`} {
				if strings.Contains(string(b), k) {
					t.Errorf("the summary carries %s", k)
				}
			}
			if n := strings.Count(string(b), `"sampler":`); n != 1 {
				t.Errorf("the summary has %d sampler objects, want one top-level", n)
			}
		})
	}
}

// TC-HP-99: The summary is at most 16 KiB on a 32-mount, 32-interface host. Owner WI 4.5. Requirements: FR-4, NFR-7.
//
// - Setup: the synthetic large host of test-strategy.md (32 mounts with 64-byte mount points, each added with `addMount(…, both)` and its own fresh `maj:min`, so each is visible and is its own dedupe group; 32 interfaces with 15-byte names, IPv4 + IPv6 each), warming up (all sampler fields null with reasons, the largest shape).
// - Steps: summary handler; `json.Marshal` the full list response.
// - Expected: ≤ 16,384 bytes; `filesystemsTruncated` and `interfacesTruncated` false; totals 32 + the capture's own.
//
// The TC's two clauses cannot hold at once: 32 extra entries plus the
// capture's own are more than 32, so truncated is true. Both shapes run:
// "extra" (32 extra each; totals 32 + the capture's own, truncated true) and
// "exactly32" (extras that bring each total to exactly 32; truncated
// false). In both the 32 kept filesystems and interfaces are the largest
// the host offers, and the size bound must hold.
func TestTCHP99_TheSummaryIsAtMost16KiB(t *testing.T) {
	base := newHostStack(t, capUBU)
	base.start()
	base.waitSeq(1)
	ownFS, ownIfaces := base.summary().FilesystemsTotal, base.summary().Network.InterfacesTotal
	if ownFS == 0 || ownIfaces == 0 {
		t.Fatalf("the capture has %d filesystems and %d interfaces of its own", ownFS, ownIfaces)
	}
	for _, c := range []struct {
		name           string
		mounts, ifaces int
	}{
		{"extra", 32, 32},
		{"exactly32", spec.SummaryMaxFilesystems - ownFS, spec.SummaryMaxInterfaces - ownIfaces},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newLargeHost(t, largeOpts{mounts: c.mounts, ifaces: c.ifaces})
			s.start()
			s.waitSeq(1)
			out, err := s.p.summary(context.Background(), spec.NoInput{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: summary is %d bytes", c.name, len(b))
			if len(b) > 16384 {
				t.Errorf("summary is %d bytes, want at most 16384", len(b))
			}
			p := s.summary()
			if p.Sampler.State != spec.SamplerWarmingUp {
				t.Errorf("sampler %q, want warming-up", p.Sampler.State)
			}
			fsTotal, ifTotal := c.mounts+ownFS, c.ifaces+ownIfaces
			if p.FilesystemsTotal != fsTotal || p.FilesystemsTruncated != (fsTotal > 32) || len(p.Filesystems) != min(32, fsTotal) {
				t.Errorf("filesystems %d total %d truncated %v; want %d total %d truncated %v", len(p.Filesystems),
					p.FilesystemsTotal, p.FilesystemsTruncated, min(32, fsTotal), fsTotal, fsTotal > 32)
			}
			n := p.Network
			if n.InterfacesTotal != ifTotal || n.InterfacesTruncated != (ifTotal > 32) || len(n.Interfaces) != min(32, ifTotal) {
				t.Errorf("interfaces %d total %d truncated %v; want %d total %d truncated %v", len(n.Interfaces),
					n.InterfacesTotal, n.InterfacesTruncated, min(32, ifTotal), ifTotal, ifTotal > 32)
			}
			all := s.fsAll()
			for i, f := range p.Filesystems {
				if f.MountPoint != all[i].MountPoint {
					t.Errorf("filesystems[%d] = %s, want %s (the first 32 by mount point)", i, f.MountPoint,
						all[i].MountPoint)
				}
			}
			for _, i := range n.Interfaces {
				if len(i.Name) == 15 && (i.IPv4 == nil || i.IPv6 == nil || i.RxBytesPerSecond != nil ||
					i.Unavailable["rxBytesPerSecond"] != reasonWarmingUp) {
					t.Errorf("%s: %s, want both addresses and rates warming up", i.Name, show(&i))
				}
			}
		})
	}
}

// TC-HP-100: Summary interface order and primary addresses. Owner WI 4.5. Requirements: FR-4.
//
// - Setup: UBU plus F-ADDRS variants for one interface: [link-local, ULA, global] in that kernel order; [link-local, ULA]; [link-local]; no IPv4.
// - Steps: summary handler.
// - Expected: loopback excluded; interfaces with a `device` first, then by name; primary IPv6 = the global, then ULA, then link-local per variant; primary IPv4 = first in kernel order; no IPv4 → `ipv4` null with `not reported: no IPv4 address`.
func TestTCHP100_SummaryInterfaceOrderAndPrimaryAddresses(t *testing.T) {
	s := newHostStack(t, capUBU)
	if names := summaryIfaceNames(s.summary()); !slices.Equal(names, []string{"docker0", "eth0"}) {
		t.Errorf("without devices: %v, want docker0, eth0 (by name, lo excluded)", names)
	}

	for _, c := range []struct {
		name   string
		addrs  []string
		v4, v6 string
	}{
		{"link-local, ULA, global", []string{"198.51.100.7/24", "192.0.2.10/24", "fe80::1/64", "fd12:3456::1/64", "2001:db8::1/64"},
			"198.51.100.7/24", "2001:db8::1/64"},
		{"link-local, ULA", []string{"192.0.2.10/24", "fe80::1/64", "fd12:3456::1/64"}, "192.0.2.10/24", "fd12:3456::1/64"},
		{"link-local", []string{"192.0.2.10/24", "fe80::1/64"}, "192.0.2.10/24", "fe80::1/64"},
		{"no IPv4", []string{"fe80::1/64"}, "", "fe80::1/64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, capUBU)
			s.ffs.replace("sys/class/net/eth0/device/uevent", []byte("DRIVER=virtio_net\n"))
			withAddrs(s, "eth0", c.addrs...)
			p := s.summary()
			if names := summaryIfaceNames(p); !slices.Equal(names, []string{"eth0", "docker0"}) {
				t.Errorf("interfaces %v, want eth0 (a device) then docker0", names)
			}
			eth0 := summaryIface(t, p, "eth0")
			if show(eth0.IPv6) != strconv.Quote(c.v6) {
				t.Errorf("ipv6 %s, want %q", show(eth0.IPv6), c.v6)
			}
			switch {
			case c.v4 == "":
				if eth0.IPv4 != nil || eth0.Unavailable["ipv4"] != "not reported: no IPv4 address" {
					t.Errorf("ipv4 %s with %q, want null with not reported: no IPv4 address", show(eth0.IPv4),
						eth0.Unavailable["ipv4"])
				}
			case show(eth0.IPv4) != strconv.Quote(c.v4):
				t.Errorf("ipv4 %s, want %q", show(eth0.IPv4), c.v4)
			}
			if d := summaryIface(t, p, "docker0"); d.IPv6 != nil || d.Unavailable["ipv6"] != "not reported: no IPv6 address" {
				t.Errorf("docker0 ipv6 %s with %q", show(d.IPv6), d.Unavailable["ipv6"])
			}
		})
	}
}

// TC-HP-101: Metrics operation over a running sampler. Owner WI 4.5. Requirements: FR-13, FR-11.
//
// - Setup: host stack (UBU), 20 ticks fired (alternating fs/t1 via `F-FS.advance` toggles, F-CLK +10 s each).
// - Steps: metrics with `{}`; `{metric: [network, disk]}`; `{windowSeconds: 86400}`; then with no run (provider never started).
// - Expected: default → cpu, load, memory, swap series with `stepSeconds` 10 and 20 points; network/disk → per-entity series; 86400 window → `from` = `historyStart` aligned; no run → `series []`, `points 0`, `sampler.state not-running` with reason `not running`; `assertExplicit` passes.
//
// "20 ticks" is read as 20 samples: the run's immediate sample and 19
// F-TICK fires, 10 s apart, which span 20 steps of 10 s.
func TestTCHP101_MetricsOperationOverARunningSampler(t *testing.T) {
	s := newHostStack(t, capUBU)
	b := s.start()
	s.waitSeq(1)
	for i := 1; i < 20; i++ {
		setAdvanced(s.ffs, i%2 == 1)
		s.fire(b, 10*time.Second)
		s.waitSeq(uint64(i + 1))
	}

	p := s.metricsGet(spec.MetricsInput{})
	var got []string
	for _, sr := range p.Series {
		got = append(got, sr.Metric+"/"+sr.Name+"/"+sr.Entity)
		if len(sr.Values) != p.Points {
			t.Errorf("%s/%s has %d values, points %d", sr.Metric, sr.Name, len(sr.Values), p.Points)
		}
	}
	want := []string{"cpu/usagePercent/", "load/load1/", "memory/usedBytes/", "memory/availableBytes/", "swap/usedBytes/"}
	if !slices.Equal(got, want) || p.StepSeconds != 10 || p.Points != 20 || p.Sampler.State != spec.SamplerReady {
		t.Errorf("default: series %v step %d points %d sampler %q; want %v, 10, 20, ready", got, p.StepSeconds,
			p.Points, p.Sampler.State, want)
	}
	if load := findSeries(t, p, spec.MetricLoad, spec.SeriesLoad1, ""); slices.Contains(load.Values, nil) {
		t.Errorf("load1 has a null bucket: %s", show(&load.Values))
	}

	p = s.metricsGet(spec.MetricsInput{Metric: []string{spec.MetricNetwork, spec.MetricDisk}})
	entities := map[string][]string{}
	for _, sr := range p.Series {
		entities[sr.Metric+"/"+sr.Name] = append(entities[sr.Metric+"/"+sr.Name], sr.Entity)
	}
	wantEnt := map[string][]string{
		"network/rxBytesPerSecond": {"docker0", "eth0", "lo"}, "network/txBytesPerSecond": {"docker0", "eth0", "lo"},
		"disk/readBytesPerSecond": {"vda", "vdb", "vdc"}, "disk/writeBytesPerSecond": {"vda", "vdb", "vdc"},
	}
	if !reflect.DeepEqual(entities, wantEnt) {
		t.Errorf("per-entity series %v, want %v", entities, wantEnt)
	}
	if disks := diskNames(s.diskList()); !slices.Equal(disks, wantEnt["disk/readBytesPerSecond"]) {
		t.Errorf("disk series entities differ from the disks op's %v", disks)
	}

	p = s.metricsGet(spec.MetricsInput{WindowSeconds: 86400})
	if p.HistoryStart == nil {
		t.Fatal("historyStart null on a running sampler")
	}
	stepMs := int64(p.StepSeconds) * 1000
	if from := p.HistoryStart.UnixMilli() / stepMs * stepMs; p.From.UnixMilli() != from || p.StepSeconds < 173 {
		t.Errorf("from %v step %d, want historyStart %v aligned down to the step (%v)", p.From, p.StepSeconds,
			*p.HistoryStart, time.UnixMilli(from).UTC())
	}

	n := newHostStack(t, capUBU)
	p = n.metricsGet(spec.MetricsInput{})
	if len(p.Series) != 0 || p.Points != 0 || p.Sampler.State != spec.SamplerNotRunning ||
		p.Sampler.Unavailable["sampledAt"] != reasonNotRunning || p.Unavailable["historyStart"] != reasonNotRunning {
		t.Errorf("no run: %s", show(&p))
	}
}

// fsKind is the allow-list access an F-FS call needed, checked after the
// fact (TC-HP-102, Finding 1): an Open needs a read entry, a ReadDir a list
// entry, a Stat or Lstat a read or stat-only entry, and a ReadLink a link
// entry (D-3).
func allowedAs(c fsCall) bool {
	switch c.op {
	case "open":
		return permits(c.name, accRead)
	case "readdir":
		return permits(c.name, accList)
	case "stat", "lstat":
		return permits(c.name, accRead|accStatOnly)
	case "readlink":
		return permits(c.name, accLink)
	}
	return false
}

// TC-HP-102: Nothing outside the allow-list is ever read, and no sensitive path is touched. Owner WI 4.5. Requirements: NFR-6, CON-1.
//
// - Setup: host stack per capture with F-FS recording (F-SPY-FS).
// - Steps: Preflight; `RunBackground` with 3 ticks; every one of the ten handlers once (lists with limit 1000); stop.
// - Expected:
//   - Every recorded access matches an allow-list entry of its access kind, checked after the fact with `allowedAs(path, kind)` (Finding 1): an `Open` needs a read entry, a `ReadDir` a list entry, and a `Stat` or `ReadLink` a read or stat-only entry.
//   - No recorded path matches `(?i)(cmdline|environ|serial|uuid|asset_tag|kmsg|shadow|/arp$|neigh|/mem$|kcore|/fd/|/maps$)`.
//   - No recorded path is a mount point of the capture's mountinfo, other than allow-listed files that happen to be mount points. Mount points are reached only through F-STATFS (Finding 2).
//   - The recording is non-vacuous. It includes an `Open` of `proc/1/mountinfo`, of `proc/self/mountinfo` (visibility, D-9), of `sys/class/dmi/id/sys_vendor` and of at least one `proc/<pid>/stat`, a `ReadDir` of `proc` (process scan), and a `Stat` of at least one clock-source marker (`run/systemd/timesync` on UBU, from the clock-source check; `usr/bin/chronyc` on ALP, from the chronyc check).
//
// A ReadLink is held to the link entry, the production check, which is
// stricter than "read or stat-only".
func TestTCHP102_NothingOutsideTheAllowListIsEver(t *testing.T) {
	sensitive := regexp.MustCompile(`(?i)(cmdline|environ|serial|uuid|asset_tag|kmsg|shadow|/arp$|neigh|/mem$|kcore|/fd/|/maps$)`)
	for _, c := range []struct{ capture, marker string }{{capUBU, "run/systemd/timesync"}, {capALP, "usr/bin/chronyc"}} {
		t.Run(c.capture, func(t *testing.T) {
			s := newHostStack(t, c.capture)
			s.ffs.resetRecorder()
			if st := s.p.Preflight(context.Background()); st.State != "enabled" {
				t.Fatalf("Preflight %q %q", st.State, st.Reason)
			}
			b := s.start()
			s.waitSeq(1)
			for i := range 3 {
				setAdvanced(s.ffs, i%2 == 0)
				s.fire(b, 10*time.Second)
				s.waitSeq(uint64(i + 2))
			}
			for _, h := range tenHandlers(s.p) {
				if _, err := h.call(); err != nil {
					t.Errorf("%s: %v", h.name, err)
				}
			}
			b.cancel()
			<-b.done

			mounts := map[string]bool{}
			for _, e := range s.ffs.mountinfo(t, pathHostMountinfo) {
				mounts[e.mountPoint()] = true
			}
			calls := s.ffs.recorded()
			seen := map[string]bool{}
			pidStat := regexp.MustCompile(`^proc/[0-9]+/stat$`)
			for _, call := range calls {
				if !allowedAs(call) {
					t.Errorf("%s is outside the allow-list for its kind", call)
				}
				if sensitive.MatchString(call.name) {
					t.Errorf("%s touches a sensitive path", call)
				}
				if mounts["/"+call.name] && !permits(call.name, accRead|accList) {
					t.Errorf("%s reaches a mount point through F-FS", call)
				}
				seen[call.String()] = true
				if call.op == "open" && pidStat.MatchString(call.name) {
					seen["open proc/<pid>/stat"] = true
				}
			}
			for _, want := range []string{"open proc/1/mountinfo", "open proc/self/mountinfo",
				"open sys/class/dmi/id/sys_vendor", "open proc/<pid>/stat", "readdir proc", "stat " + c.marker} {
				if !seen[want] {
					t.Errorf("the recording lacks %s (%d calls)", want, len(calls))
				}
			}
		})
	}
}

// reasonsOf flattens every unavailable map of v's JSON into path → reason.
// A list item that has a name, mountPoint, pid, cpu or address is keyed by
// it, so outputs whose lists differ in order or length still compare.
func reasonsOf(t *testing.T, v any) map[string]string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walk func(path string, n any)
	walk = func(path string, n any) {
		switch x := n.(type) {
		case map[string]any:
			if u, ok := x["unavailable"].(map[string]any); ok {
				for k, r := range u {
					out[path+"."+k] = fmt.Sprint(r)
				}
			}
			for k, c := range x {
				if k != "unavailable" {
					walk(path+"."+k, c)
				}
			}
		case []any:
			for i, c := range x {
				key := strconv.Itoa(i)
				if o, ok := c.(map[string]any); ok {
					for _, f := range []string{"name", "mountPoint", "pid", "cpu", "address", "label"} {
						if v, ok := o[f]; ok {
							key = f + "=" + fmt.Sprint(v)
							break
						}
					}
				}
				walk(path+"["+key+"]", c)
			}
		}
	}
	walk("$", doc)
	return out
}

// TC-HP-103: Every operation answers with explicit unavailability under every fault profile. Owner WI 4.5. Requirements: FR-12, FR-3, NFR-4.
//
// - Setup: each capture × profiles {none, SP-SANDBOX, SP-DENY-ALL, SP-NO-PSI, SP-VM-NO-SENSORS} × sampler states {not-running, warming-up, ready, stale}.
// - Steps: call all ten handlers.
// - Expected: no handler returns an error; every output passes `assertExplicit`; under SP-DENY-ALL every non-Preflight field is null or `[]` with a `permission denied: /…` reason; under SP-SANDBOX the denied fields carry `permission denied: <their path>` and nothing else is unavailable that is available with no profile.
//
// Two documented whole-op answers are kept, and asserted exactly: under
// SP-DENY-ALL the disks and interfaces lists cannot list sys/block or
// sys/class/net, and refuse with dependency-unavailable naming the denial
// (WI 4.2, WI 4.3: disks_test.go, interfaces_test.go). A list has no
// unavailable map to carry the reason. Under SP-DENY-ALL, fields that come
// from no file (the platform's OS and init system, uname, netlink, statfs,
// the command runner) and from the three Preflight files stay available;
// every reason that names a host path is a permission denial, never
// "not present:" or "unrecognized format:", so a denial is never reported
// as an absence.
func TestTCHP103_EveryOperationAnswersWithExplicitUnavailabilityUnder(t *testing.T) {
	profiles := []struct {
		name  string
		apply func(*hostStack)
	}{
		{"none", func(*hostStack) {}},
		{"SP-SANDBOX", func(s *hostStack) { spSandbox(s.t, s.ffs); spSandboxRun(s.run) }},
		{"SP-DENY-ALL", func(s *hostStack) { spDenyAll(s.ffs) }},
		{"SP-NO-PSI", func(s *hostStack) { spNoPSI(s.ffs) }},
		{"SP-VM-NO-SENSORS", func(s *hostStack) { spVMNoSensors(s.t, s.ffs) }},
	}
	states := []struct {
		name  string
		state string
		drive func(*hostStack)
	}{
		{"not-running", spec.SamplerNotRunning, func(*hostStack) {}},
		{"warming-up", spec.SamplerWarmingUp, func(s *hostStack) { s.start(); s.waitSeq(1) }},
		{"ready", spec.SamplerReady, func(s *hostStack) { s.ready() }},
		{"stale", spec.SamplerStale, func(s *hostStack) { s.ready(); s.clk.Advance(31 * time.Second) }},
	}
	samplerReason := regexp.MustCompile(`^(warming up|not running|stale: last sample \d+s old)$`)
	sandboxCommand := reasonCommandExit(argvTimedatectl[0], 1)
	for _, capture := range []string{capUBU, capALP} {
		for _, st := range states {
			var baseline map[string]map[string]string
			for _, pr := range profiles {
				t.Run(capture+"/"+st.name+"/"+pr.name, func(t *testing.T) {
					s := newHostStack(t, capture)
					pr.apply(s)
					st.drive(s)
					reasons := map[string]map[string]string{}
					for _, h := range tenHandlers(s.p) {
						out, err := h.call()
						if pr.name == "SP-DENY-ALL" && (h.name == "disks" || h.name == "interfaces") {
							dir := map[string]string{"disks": "/sys/block", "interfaces": "/sys/class/net"}[h.name]
							var pe *problem.Error
							if !errors.As(err, &pe) || pe.Slug() != problem.DependencyUnavailable ||
								pe.Detail != "permission denied: "+dir {
								t.Errorf("%s: err %v, want dependency-unavailable with permission denied: %s", h.name, err, dir)
							}
							continue
						}
						if err != nil {
							t.Errorf("%s: %v", h.name, err)
							continue
						}
						assertExplicit(t, out)
						reasons[h.name] = reasonsOf(t, out)
						if raw, _ := json.Marshal(out); !strings.Contains(string(raw), `"state":"`+st.state+`"`) &&
							h.name != "identity" && h.name != "memory" && h.name != "netconfig" && h.name != "filesystems" {
							t.Errorf("%s: no sampler.state %q", h.name, st.state)
						}
					}
					switch pr.name {
					case "none":
						baseline = reasons
					case "SP-DENY-ALL":
						denied := 0
						for op, rs := range reasons {
							for k, r := range rs {
								if strings.HasPrefix(r, prefixPermission+"/") {
									denied++
								}
								if strings.HasPrefix(r, prefixNotPresent+"/") || strings.HasPrefix(r, prefixUnrecognized) {
									t.Errorf("%s %s: %q reports a denied path as absent or unparsable", op, k, r)
								}
							}
						}
						if denied < 30 {
							t.Errorf("only %d permission-denied reasons", denied)
						}
					case "SP-SANDBOX":
						for op, rs := range reasons {
							for k, r := range rs {
								if baseline[op][k] == r || samplerReason.MatchString(r) {
									continue
								}
								if !strings.HasPrefix(r, prefixPermission+"/") && r != sandboxCommand {
									t.Errorf("%s %s: %q under SP-SANDBOX (no profile: %q)", op, k, r, baseline[op][k])
								}
							}
						}
					}
				})
			}
		}
	}
}

// TC-HP-E26: Lists over the summary bound are cut with an indicator, never silently. Owner WI 4.5. Requirements: FR-4.
//
// - Setup: the synthetic large host with 40 extra mounts (each added with `addMount(…, both)` and its own fresh `maj:min`) and 40 extra interfaces.
// - Steps: summary handler.
// - Expected: 32 filesystems, `filesystemsTotal` = 40 + the capture's, `filesystemsTruncated true`; 32 interfaces, `interfacesTotal` likewise, `interfacesTruncated true`; the kept filesystems are the first 32 by mount point.
func TestTCHPE26_ListsOverTheSummaryBoundAreCut(t *testing.T) {
	base := newHostStack(t, capUBU).summary()
	s, _ := newLargeHost(t, largeOpts{mounts: 40, ifaces: 40})
	p := s.summary()
	if len(p.Filesystems) != 32 || p.FilesystemsTotal != 40+base.FilesystemsTotal || !p.FilesystemsTruncated {
		t.Errorf("filesystems %d total %d truncated %v; want 32, %d, true", len(p.Filesystems), p.FilesystemsTotal,
			p.FilesystemsTruncated, 40+base.FilesystemsTotal)
	}
	all := s.fsAll()
	var want, got []string
	for _, f := range all[:32] {
		want = append(want, f.MountPoint)
	}
	for _, f := range p.Filesystems {
		got = append(got, f.MountPoint)
	}
	if !slices.Equal(got, want) || !slices.IsSorted(got) {
		t.Errorf("kept filesystems %v, want the first 32 by mount point %v", got, want)
	}
	n := p.Network
	if len(n.Interfaces) != 32 || n.InterfacesTotal != 40+base.Network.InterfacesTotal || !n.InterfacesTruncated {
		t.Errorf("interfaces %d total %d truncated %v; want 32, %d, true", len(n.Interfaces), n.InterfacesTotal,
			n.InterfacesTruncated, 40+base.Network.InterfacesTotal)
	}
	// Devices first, then by name: the 40 extras are devices, so the 32
	// kept are the first 32 of them by name.
	for i, it := range n.Interfaces {
		if it.Name != largeIface(i) {
			t.Errorf("interfaces[%d] = %s, want %s", i, it.Name, largeIface(i))
		}
	}
}

// TC-HP-ERR26: One hung mount keeps the summary under its bound. Owner WI 4.5. Requirements: FR-4, NFR-1.
//
// - Setup: UBU with SP-HUNG-NFS (`/mnt/nfs`, fresh `maj:min` `0:250`, added with `addMount(…, both)`, so the seam is reached; the hang covers the whole seam call, including the directory check; Finding 2); timeout 250 ms; plus F-RUN `hang` on the failed-units argv.
// - Steps: summary handler; measure.
// - Expected: returns within 550 ms (`sourceTimeout` + 300 ms) even with two hung sources (collectors run concurrently); `/mnt/nfs` usage and `health.failedServicesCount` null with their `timed out` reasons (not `not visible:` for `/mnt/nfs`).
func TestTCHPERR26_OneHungMountKeepsTheSummaryUnder(t *testing.T) {
	s, st := newFSStack(t, capUBU)
	s.timeout(250 * time.Millisecond)
	spHungNFS(t, s.ffs, st)
	s.run.hang(argvSystemctlFailed)

	start := time.Now()
	out, err := s.p.summary(context.Background(), spec.NoInput{})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > 550*time.Millisecond {
		t.Errorf("summary took %v, want within sourceTimeout + 300ms", took)
	}
	p := oneItem[spec.SummaryProperties](t, spec.SummaryType, out, nil)
	var nfs *spec.SummaryFilesystem
	for i := range p.Filesystems {
		if p.Filesystems[i].MountPoint == spHungNFSMount {
			nfs = &p.Filesystems[i]
		}
	}
	const fsWhy = "timed out after 250ms: statfs /mnt/nfs"
	if nfs == nil || nfs.SizeBytes != nil || nfs.UsedPercent != nil || nfs.Unavailable["sizeBytes"] != fsWhy ||
		nfs.Unavailable["usedPercent"] != fsWhy {
		t.Errorf("/mnt/nfs = %s, want size and used null with %q", show(nfs), fsWhy)
	}
	const cmdWhy = "timed out after 250ms: /usr/bin/systemctl"
	if h := p.Health; h.FailedServicesCount != nil || h.Unavailable["failedServicesCount"] != cmdWhy {
		t.Errorf("failedServicesCount %s with %q, want null with %q", show(h.FailedServicesCount),
			h.Unavailable["failedServicesCount"], cmdWhy)
	}
	for _, f := range p.Filesystems {
		if f.MountPoint != spHungNFSMount && f.SizeBytes == nil {
			t.Errorf("%s: size null with %q", f.MountPoint, f.Unavailable["sizeBytes"])
		}
	}
}

// metricsOnly is the provider with host.metrics.get bound, for the
// dispatcher's validation before the bindings of WI 5.1 exist.
type metricsOnly struct{ *Provider }

func (m metricsOnly) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(spec.MetricsGet, m.metrics)}
}

// TC-HP-ERR27: Metrics input refusals in the handler. Owner WI 4.5. Requirements: FR-13, FR-14.
//
// - Setup: host stack with a run.
// - Steps: call the handler directly with `stepSeconds 1`; and through a test server (dispatcher validation) with `windowSeconds 10`.
// - Expected: both are `*problem.Error` with slug `validation-failed` naming the field (`stepSeconds`, `windowSeconds`); the history is unchanged afterwards.
func TestTCHPERR27_MetricsInputRefusalsInTheHandler(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	h := s.p.run.Load().history()
	before := stored(h)

	_, err := s.p.metrics(context.Background(), spec.MetricsInput{StepSeconds: 1})
	wantValidationFailed(t, err, "stepSeconds")

	reg, err := core.New([]provider.Provider{metricsOnly{s.p}}, core.Options{
		Host: "tc", Version: "v0.0.0-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Platform: s.plat, Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	invoke := func(input string) (core.InvokeResult, error) {
		return d.Invoke(context.Background(), core.InvokeRequest{
			Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-hp-err27", Scopes: []string{spec.ReadScope}},
			Surface: core.SurfaceREST, Op: spec.MetricsGet.Op(), Input: json.RawMessage(input),
		})
	}
	_, err = invoke(`{"windowSeconds":10}`)
	wantValidationFailed(t, err, "windowSeconds")
	// The positive control: the same route answers a valid input.
	res, err := invoke(`{"windowSeconds":60}`)
	if err != nil {
		t.Fatalf("valid input refused: %v", err)
	}
	if l, ok := res.Body.(apitypes.List[spec.Metrics]); !ok || len(l.Items) != 1 {
		t.Errorf("body %#v, want the metrics list", res.Body)
	}

	// fmt, not reflect.DeepEqual: a null stored value is NaN.
	if after := stored(h); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("history changed: %d points before, %d after", len(before), len(after))
	}
}

// TC-HP-134: The sampler with a summary and a 24 h metrics call on every tick keeps the heap under 32 MiB. Owner WI 4.5. Requirements: NFR-2, FR-13, FR-4.
//
// - Setup:
//   - Finding 16 splits this out of TC-HP-133, so the shape that needs WI 4.5's handlers is owned by WI 4.5.
//   - The synthetic large host of test-strategy.md, generated from UBU into `t.TempDir()`: 32 extra interfaces, 32 extra block devices with diskstats lines, and 300 extra pids. The pids are copies of one captured `proc/<pid>/{stat,status}` pair with the pid fields rewritten, so each tick's process scan reads and parses a realistic volume.
//   - `debug.SetGCPercent(100)` (restored in cleanup).
//   - Baseline: `runtime.GC()`, `debug.FreeOSMemory()`, then `runtime.ReadMemStats`.
//
// - Steps:
//  1. Configure `historyDuration: 24h`, `sampleInterval: 2s`.
//  2. Fill the history ring to capacity (21,600 stored points for every slot; A-40) through the history API, as TC-HP-60 does.
//  3. Start `RunBackground`. Fire 200 ticks with F-TICK, alternating `F-FS.advance()` between the two dynamic samples and advancing F-CLK 2 s per tick. After each tick, call the summary handler and `host.metrics.get` for the full window once.
//  4. Read `runtime.ReadMemStats` after every 20th tick, with no forced GC.
//
// - Expected:
//   - Every reading of `HeapSys − HeapReleased` is below baseline + 32 MiB.
//   - The maximum and every reading are logged with `t.Logf`, so WI 5.4 can compare them with the live RSS of TC-HP-130.
//   - The ring still holds 21,600 points per slot at the end: the ticks overwrote, not grew.
//
// The ring is filled right after the run starts (the history exists only
// within a run), with the host's own entity names, so every one of the 32
// interface and 32 disk slots is held by an entity the ticks keep feeding.
func TestTCHP134_TheSamplerWithASummaryAndA(t *testing.T) {
	const heapBound = 32 << 20 // NFR-2
	old := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(old) })

	s, _ := newLargeHost(t, largeOpts{ifaces: 32, disks: 32, pids: 300})
	if err := s.p.Configure(hostSection(t, "historyDuration: 24h\nsampleInterval: 2s\n")); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	var ms runtime.MemStats
	runtime.GC()
	debug.FreeOSMemory()
	runtime.ReadMemStats(&ms)
	baseline := ms.HeapSys - ms.HeapReleased

	b := s.start()
	first := s.waitSeq(1)
	h := s.p.run.Load().history()
	ifaces := slices.Sorted(maps.Keys(first.ifaces))
	whole := wholeDisks(s.p.src)
	disks := slices.Sorted(maps.Keys(whole))
	if len(ifaces) < spec.MaxHistoryInterfaces || len(disks) < spec.MaxHistoryDisks {
		t.Fatalf("the large host has %d interfaces and %d whole disks, want at least 32 each", len(ifaces), len(disks))
	}
	fill := fullSample(time.Time{}, 1)
	for _, n := range ifaces[:spec.MaxHistoryInterfaces] {
		fill.Interfaces = append(fill.Interfaces, histEntity{Name: n, Device: true, A: fp(1e6), B: fp(1e6)})
	}
	for _, n := range disks[:spec.MaxHistoryDisks] {
		fill.Disks = append(fill.Disks, histEntity{Name: n, Device: true, A: fp(2e6), B: fp(2e6)})
	}
	start := s.clk.Now().Add(-24 * time.Hour)
	for i := range 2 * spec.MaxHistoryPoints {
		fill.At = start.Add(time.Duration(i) * 2 * time.Second)
		h.Append(fill)
	}
	slotsFull := func() {
		t.Helper()
		if n := len(stored(h)); n != spec.MaxHistoryPoints {
			t.Errorf("the ring holds %d points, want %d", n, spec.MaxHistoryPoints)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for kind, max := range map[int]int{entIface: spec.MaxHistoryInterfaces, entDisk: spec.MaxHistoryDisks} {
			if n := len(h.ents[kind].slots); n != max {
				t.Errorf("entity kind %d holds %d slots, want %d", kind, n, max)
			}
			for _, sl := range h.ents[kind].slots {
				if len(sl.a) != h.capacity || len(sl.b) != h.capacity {
					t.Errorf("slot %s rings %d/%d, want %d", sl.name, len(sl.a), len(sl.b), h.capacity)
				}
			}
		}
	}
	slotsFull()

	var peak uint64
	var readings []string
	for i := 1; i <= 200; i++ {
		// F-FS keeps every access it records (about 400 a tick here); that
		// is the fixture's memory, not the provider's, so it is dropped.
		s.ffs.resetRecorder()
		setAdvanced(s.ffs, i%2 == 1)
		s.fire(b, 2*time.Second)
		s.waitSeq(uint64(i + 1))
		if _, err := s.p.summary(context.Background(), spec.NoInput{}); err != nil {
			t.Fatalf("tick %d: summary: %v", i, err)
		}
		out, err := s.p.metrics(context.Background(), spec.MetricsInput{WindowSeconds: spec.MaxWindowSeconds})
		if err != nil {
			t.Fatalf("tick %d: metrics: %v", i, err)
		}
		if m := out.Items[0].Properties; m.Points == 0 || m.Sampler.State != spec.SamplerReady {
			t.Fatalf("tick %d: metrics points %d sampler %q", i, m.Points, m.Sampler.State)
		}
		if i%20 != 0 {
			continue
		}
		runtime.ReadMemStats(&ms)
		inUse := ms.HeapSys - ms.HeapReleased
		peak = max(peak, inUse)
		readings = append(readings, fmt.Sprintf("tick %d: %.2f MiB", i, float64(inUse)/(1<<20)))
		if inUse >= baseline+heapBound {
			t.Errorf("tick %d: HeapSys − HeapReleased %d ≥ baseline %d + 32 MiB", i, inUse, baseline)
		}
	}
	t.Logf("HeapSys − HeapReleased readings: %s", strings.Join(readings, ", "))
	t.Logf("max HeapSys − HeapReleased %d bytes (%.2f MiB), baseline %d bytes (%.2f MiB), delta %.2f MiB",
		peak, float64(peak)/(1<<20), baseline, float64(baseline)/(1<<20), float64(peak-baseline)/(1<<20))
	slotsFull()
	runtime.KeepAlive(h)
}
