// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// summary calls the summary handler and returns its properties, checked by
// oneItem (current, host tc, assertExplicit).
func (s *hostStack) summary() spec.SummaryProperties {
	s.t.Helper()
	out, err := s.p.summary(context.Background(), spec.NoInput{})
	return oneItem[spec.SummaryProperties](s.t, spec.SummaryType, out, err)
}

// largeOpts sizes a synthetic large host (test-strategy.md, Test Data):
// mounts extra filesystems at 64-byte mount points, ifaces extra interfaces
// with 15-byte names, disks extra whole block devices with diskstats lines
// and pids extra processes.
type largeOpts struct {
	mounts, ifaces, disks, pids int
	// noDevice leaves the extra interfaces without sys/class/net/<if>/device.
	noDevice bool
}

// largeMountPoint is the i-th extra mount point: exactly 64 bytes.
func largeMountPoint(i int) string {
	p := fmt.Sprintf("/srv/synthetic-%02d-", i)
	return p + strings.Repeat("x", 64-len(p))
}

// largeIface is the i-th extra interface: exactly 15 bytes (IFNAMSIZ − 1).
func largeIface(i int) string { return fmt.Sprintf("ens%012d", i) }

// largeDisk is the i-th extra disk, sd-style: sda … sdz, sdaa ….
func largeDisk(i int) string {
	if i < 26 {
		return "sd" + string(rune('a'+i))
	}
	return "sd" + string(rune('a'+i/26-1)) + string(rune('a'+i%26))
}

// newLargeHost is the synthetic large host of test-strategy.md (4.5): the
// Ubuntu capture's fs/ and t1/ copied into t.TempDir() and served by F-FS
// from there, plus the extras of o:
//   - each mount added with addMount(…, both) and its own fresh maj:min from
//     0:300 upward, answering F-STATFS with the root's row;
//   - each interface with every attribute, a device directory (unless
//     noDevice), /proc/net/dev lines in both samples and one IPv4 and one
//     IPv6 address through F-ADDRS;
//   - each disk with its sys/block attributes (maj 8, minor 16·i) and
//     diskstats lines in both samples;
//   - each pid a copy of the captured proc/1/{stat,status} pair with the pid
//     fields rewritten, in both samples.
func newLargeHost(t *testing.T, o largeOpts) (*hostStack, *fakeStatfs) {
	t.Helper()
	s, st := newFSStack(t, capUBU)
	root := t.TempDir()
	fsDir, t1Dir := filepath.Join(root, "fs"), filepath.Join(root, "t1")
	for dst, src := range map[string]string{fsDir: "fs", t1Dir: "t1"} {
		if err := os.CopyFS(dst, os.DirFS(filepath.Join("testdata", capUBU, src))); err != nil {
			t.Fatalf("copy the capture's %s: %v", src, err)
		}
	}
	write := func(dir, name, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	appendTo := func(dir, name, lines string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, lines...), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rootRow := st.row("/")
	for i := range o.mounts {
		mp := largeMountPoint(i)
		s.ffs.addMount(t, fmt.Sprintf("0 0 0:%d / %s rw,relatime shared:%d - ext4 /dev/synth%d rw", 300+i, mp, 950+i, i),
			viewBoth)
		u := rootRow
		u.Bavail = rootRow.Bavail - uint64(i) // distinct numbers per mount
		st.set(mp, u)
	}

	extra := map[string][]netip.Prefix{}
	var devFS, devT1 strings.Builder
	for i := range o.ifaces {
		name := largeIface(i)
		dir := "sys/class/net/" + name + "/"
		write(fsDir, dir+"type", "1\n")
		write(fsDir, dir+"operstate", "up\n")
		write(fsDir, dir+"address", fmt.Sprintf("02:00:00:00:%02x:%02x\n", i/256, i%256))
		write(fsDir, dir+"mtu", "1500\n")
		write(fsDir, dir+"speed", "1000\n")
		write(fsDir, dir+"ifindex", fmt.Sprintf("%d\n", 100+i))
		if !o.noDevice {
			write(fsDir, dir+"device/uevent", "DRIVER=synthetic\n")
		}
		fmt.Fprintf(&devFS, "%s: %d 10 0 0 0 0 0 0 %d 20 0 0 0 0 0 0\n", name, 1000+i, 2000+i)
		fmt.Fprintf(&devT1, "%s: %d 15 0 0 0 0 0 0 %d 25 0 0 0 0 0 0\n", name, 11000+i, 22000+i)
		extra[name] = []netip.Prefix{
			netip.MustParsePrefix(fmt.Sprintf("10.%d.%d.1/24", 100+i/256, i%256)),
			netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x::1/64", 0x1000+i)),
		}
	}
	if o.ifaces > 0 {
		appendTo(fsDir, "proc/net/dev", devFS.String())
		appendTo(t1Dir, "proc/net/dev", devT1.String())
		orig := s.p.addrs
		s.p.addrs = func() (map[string][]netip.Prefix, error) {
			m, err := orig()
			if err != nil {
				return nil, err
			}
			out := make(map[string][]netip.Prefix, len(m)+len(extra))
			for k, v := range m {
				out[k] = v
			}
			for k, v := range extra {
				out[k] = v
			}
			return out, nil
		}
	}

	var dsFS, dsT1 strings.Builder
	for i := range o.disks {
		name := largeDisk(i)
		dir := "sys/block/" + name + "/"
		write(fsDir, dir+"dev", fmt.Sprintf("8:%d\n", 16*i))
		write(fsDir, dir+"size", "2097152\n")
		write(fsDir, dir+"removable", "0\n")
		write(fsDir, dir+"queue/rotational", "0\n")
		write(fsDir, dir+"device/model", "SYNTHETIC DISK\n")
		fmt.Fprintf(&dsFS, "   8 %7d %s 100 0 800 10 200 0 1600 20 0 30 30 0 0 0 0 0 0\n", 16*i, name)
		fmt.Fprintf(&dsT1, "   8 %7d %s 150 0 1800 15 260 0 2600 25 0 40 40 0 0 0 0 0 0\n", 16*i, name)
	}
	if o.disks > 0 {
		appendTo(fsDir, "proc/diskstats", dsFS.String())
		appendTo(t1Dir, "proc/diskstats", dsT1.String())
	}

	pidField := regexp.MustCompile(`(?m)^(Pid|Tgid):\t1$`)
	for i := range o.pids {
		pid := fmt.Sprint(200000 + i)
		for _, dir := range []string{fsDir, t1Dir} {
			b, err := os.ReadFile(filepath.Join(dir, "proc", "1", "stat"))
			if err != nil {
				t.Fatal(err)
			}
			rest, ok := strings.CutPrefix(string(b), "1 ")
			if !ok {
				t.Fatalf("proc/1/stat does not start with its pid: %q", b)
			}
			write(dir, "proc/"+pid+"/stat", pid+" "+rest)
			status, err := os.ReadFile(filepath.Join(dir, "proc", "1", "status"))
			if err != nil {
				t.Fatal(err)
			}
			write(dir, "proc/"+pid+"/status", pidField.ReplaceAllString(string(status), "${1}:\t"+pid))
		}
	}

	s.ffs.base, s.ffs.t1 = os.DirFS(fsDir), os.DirFS(t1Dir)
	return s, st
}

// The newLargeHost builder adds what it says: the extras are listed by the
// detail ops, each mount is visible with its own numbers and each extra pid
// is scanned in both samples.
func TestLargeHostBuilder(t *testing.T) {
	s, _ := newLargeHost(t, largeOpts{mounts: 3, ifaces: 3, disks: 3, pids: 3})
	_, snap := s.ready()
	for i := range 3 {
		f := fsAt(t, s.fsAll(), largeMountPoint(i))
		if f.SizeBytes == nil || len(f.MountPoint) != 64 {
			t.Errorf("%s: size %s, mount point %d bytes", f.MountPoint, show(f.SizeBytes), len(f.MountPoint))
		}
		ip := ifaceAt(t, s.ifaceList(), largeIface(i))
		if len(largeIface(i)) != 15 || len(ip.IPv4) != 1 || len(ip.IPv6) != 1 || ip.RxBytesPerSecond == nil {
			t.Errorf("%s: %+v", largeIface(i), ip)
		}
		if !slices.Contains(diskNames(s.diskList()), largeDisk(i)) {
			t.Errorf("disk %s is not listed", largeDisk(i))
		}
	}
	if snap.procsWhy != "" {
		t.Errorf("process scan failed: %s", snap.procsWhy)
	}
	if b, err := s.ffs.Open("proc/200002/stat"); err != nil {
		t.Errorf("extra pid after advance: %v", err)
	} else {
		b.Close()
	}
	if names := listedPids(t, s.ffs); !slices.Contains(names, "200002") {
		t.Errorf("proc lists %v after advance, without the extra pids", names)
	}
	if largeDisk(25) != "sdz" || largeDisk(26) != "sdaa" || largeDisk(31) != "sdaf" {
		t.Errorf("largeDisk names %s %s %s", largeDisk(25), largeDisk(26), largeDisk(31))
	}
}

// listedPids lists the pid entries of proc as F-FS serves them now.
func listedPids(t *testing.T, f *fakeFS) []string {
	t.Helper()
	entries, err := f.ReadDir("proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if allDigits(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// withAddrs replaces one interface's addresses in F-ADDRS.
func withAddrs(s *hostStack, name string, prefixes ...string) {
	orig := s.p.addrs
	s.p.addrs = func() (map[string][]netip.Prefix, error) {
		m, err := orig()
		if err != nil {
			return nil, err
		}
		out := make(map[string][]netip.Prefix, len(m))
		for k, v := range m {
			out[k] = v
		}
		out[name] = nil
		for _, p := range prefixes {
			out[name] = append(out[name], netip.MustParsePrefix(p))
		}
		return out, nil
	}
}

// summaryIface is the summary's entry for name.
func summaryIface(t *testing.T, p spec.SummaryProperties, name string) spec.SummaryInterface {
	t.Helper()
	for _, i := range p.Network.Interfaces {
		if i.Name == name {
			return i
		}
	}
	t.Fatalf("summary lists no interface %s", name)
	return spec.SummaryInterface{}
}

func summaryIfaceNames(p spec.SummaryProperties) []string {
	var out []string
	for _, i := range p.Network.Interfaces {
		out = append(out, i.Name)
	}
	return out
}

// The primary IPv6 classes of A-24 on single addresses: a global unicast
// outranks a unique-local, which outranks a link-local; within a class the
// kernel's order decides; anything else (site-local, multicast) never
// qualifies.
func TestPrimaryIPv6Choice(t *testing.T) {
	addrs := func(ss ...string) spec.Array[spec.Address] {
		var out spec.Array[spec.Address]
		for _, s := range ss {
			p := netip.MustParsePrefix(s)
			out = append(out, spec.Address{Address: p.Addr().String(), PrefixLength: p.Bits()})
		}
		return out
	}
	for _, c := range []struct {
		in   []string
		want string
		why  string
	}{
		{[]string{"fe80::1/64", "fd12::1/64", "2001:db8::1/64"}, "2001:db8::1/64", ""},
		{[]string{"2001:db8::2/64", "2001:db8::1/64"}, "2001:db8::2/64", ""},
		{[]string{"fe80::1/64", "fd12::1/64", "fc00::9/7"}, "fd12::1/64", ""},
		{[]string{"fe80::2/64", "fe80::1/64"}, "fe80::2/64", ""},
		{[]string{"fec0::1/10"}, "", "not reported: no global, unique-local or link-local IPv6 address"},
		{nil, "", "not reported: no IPv6 address"},
	} {
		got, why := primaryIPv6(addrs(c.in...))
		if (got == nil && c.want != "") || (got != nil && *got != c.want) || why != c.why {
			t.Errorf("primaryIPv6(%v) = %s, %q; want %q, %q", c.in, show(got), why, c.want, c.why)
		}
	}
}

// The summary's identity-basic and CPU-basic collectors give the detail
// ops' values and reasons under faults as well as on a whole capture, so
// the two never drift apart (FR-4, FR-15).
func TestSummaryBasicCollectorsMatchDetailUnderFaults(t *testing.T) {
	faults := map[string]func(*hostStack){
		"denied": func(s *hostStack) {
			for _, p := range []string{pathHostname, pathOSRelease, pathUptime, pathStat, pathCPUInfo, pathLoadavg} {
				s.ffs.deny(p)
			}
			s.p.uname = func() (string, error) { return "", syscall.EPERM }
		},
		"empty": func(s *hostStack) {
			for _, p := range []string{pathHostname, pathOSRelease, pathCPUInfo} {
				s.ffs.replace(p, []byte("\n"))
			}
			s.ffs.replace(pathUptime, []byte("bogus\n"))
			s.ffs.replace(pathLoadavg, []byte("bogus\n"))
			s.p.uname = func() (string, error) { return "", nil }
		},
		"unsupported": func(s *hostStack) {
			s.p.uname = func() (string, error) { return "", errNotSupported("uname on plan9") }
			s.ffs.replace(pathStat, []byte("cpu 1 2 3 4 5 6 7 8\nbtime x\n"))
		},
	}
	for name, apply := range faults {
		t.Run(name, func(t *testing.T) {
			s := newHostStackWith(t, capUBU, &platform.Info{Init: platform.Systemd})
			apply(s)
			sum, id, cpu := s.summary(), s.identity(), s.cpu()
			wantIdentitySubset(t, sum.Identity, id)
			wantCPUSubset(t, sum.CPU, cpu)
		})
	}
}

// wantIdentitySubset checks the summary's identity against the identity op:
// each value and each reason of the subset.
func wantIdentitySubset(t *testing.T, got spec.SummaryIdentity, id spec.IdentityProperties) {
	t.Helper()
	if show(got.Hostname) != show(id.Hostname) || show(got.KernelRelease) != show(id.KernelRelease) ||
		show(got.Architecture) != show(id.Architecture) || got.InitSystem != id.InitSystem ||
		show(got.BootTime) != show(id.BootTime) || show(got.UptimeSeconds) != show(id.UptimeSeconds) ||
		show(&got.OS) != show(&id.OS) {
		t.Errorf("summary identity %s\n identity op %s", show(&got), show(&id))
	}
	for _, k := range []string{"hostname", "kernelRelease", "architecture", "bootTime", "uptimeSeconds"} {
		if got.Unavailable[k] != id.Unavailable[k] {
			t.Errorf("identity.%s reason %q, identity op %q", k, got.Unavailable[k], id.Unavailable[k])
		}
	}
}

// wantCPUSubset checks the summary's CPU against the CPU op.
func wantCPUSubset(t *testing.T, got spec.SummaryCPU, cpu spec.CPUProperties) {
	t.Helper()
	if show(got.Model) != show(cpu.Model) || show(got.LogicalCPUs) != show(cpu.LogicalCPUs) ||
		show(got.UsagePercent) != show(cpu.UsagePercent) || show(&got.Load) != show(&cpu.Load) {
		t.Errorf("summary cpu %s\n cpu op model %s logicalCpus %s usage %s load %s", show(&got), show(cpu.Model),
			show(cpu.LogicalCPUs), show(cpu.UsagePercent), show(&cpu.Load))
	}
	for _, k := range []string{"model", "logicalCpus", "usagePercent"} {
		if got.Unavailable[k] != cpu.Unavailable[k] {
			t.Errorf("cpu.%s reason %q, cpu op %q", k, got.Unavailable[k], cpu.Unavailable[k])
		}
	}
}

// A total failure of the list sources leaves the summary's lists [] with
// their reasons, never an error (Error Handling: nothing else fails a
// read).
func TestSummaryListSourcesUnreadable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny(pathHostMountinfo)
	s.ffs.deny(pathSelfMountinfo)
	s.ffs.deny(netClassDir)
	s.ffs.deny(pathRouteV4)
	p := s.summary()
	if len(p.Filesystems) != 0 || p.FilesystemsTotal != 0 || p.FilesystemsTruncated ||
		p.Unavailable["filesystems"] != "permission denied: /proc/1/mountinfo" {
		t.Errorf("filesystems %v total %d truncated %v with %q", p.Filesystems, p.FilesystemsTotal,
			p.FilesystemsTruncated, p.Unavailable["filesystems"])
	}
	n := p.Network
	if len(n.Interfaces) != 0 || n.InterfacesTotal != 0 || n.Unavailable["interfaces"] != "permission denied: /sys/class/net" {
		t.Errorf("interfaces %v total %d with %q", n.Interfaces, n.InterfacesTotal, n.Unavailable["interfaces"])
	}
	if len(n.DefaultGateways) != 0 || n.Unavailable["defaultGateways"] != "permission denied: /proc/net/route" {
		t.Errorf("gateways %v with %q", n.DefaultGateways, n.Unavailable["defaultGateways"])
	}
}

// CR-3: the summary's filesystems failure takes its reason from the one
// mount-table read that failed; it does not read mountinfo again (and then
// has no reason when that second read succeeds).
func TestSummaryFilesystemsFailureReadsMountinfoOnce(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny(pathHostMountinfo)
	s.ffs.deny(pathSelfMountinfo)
	s.ffs.resetRecorder()
	_, why := s.p.summaryFilesystems(t.Context())
	if why != "permission denied: /proc/1/mountinfo" {
		t.Errorf("reason %q, want the host file's", why)
	}
	for _, name := range []string{pathHostMountinfo, pathSelfMountinfo} {
		if n := s.ffs.count("open", name); n != 1 {
			t.Errorf("%s opened %d times, want 1", name, n)
		}
	}
}

// An interface whose type cannot be read may not be loopback, so it is
// listed; one whose operstate cannot be read is unknown with the reason;
// denied addresses make both primaries null with the Addrs reason.
func TestSummaryInterfaceFaults(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("sys/class/net/lo/type")
	s.ffs.deny("sys/class/net/eth0/operstate")
	s.p.addrs = func() (map[string][]netip.Prefix, error) { return nil, os.ErrPermission }
	p := s.summary()
	if names := summaryIfaceNames(p); !slices.Equal(names, []string{"docker0", "eth0", "lo"}) {
		t.Errorf("interfaces %v, want docker0, eth0, lo", names)
	}
	eth0 := summaryIface(t, p, "eth0")
	if eth0.State != unknownState || eth0.Unavailable["state"] != "permission denied: /sys/class/net/eth0/operstate" {
		t.Errorf("eth0 state %q with %q", eth0.State, eth0.Unavailable["state"])
	}
	if eth0.IPv4 != nil || eth0.IPv6 != nil || eth0.Unavailable["ipv4"] != "permission denied: netlink" ||
		eth0.Unavailable["ipv6"] != "permission denied: netlink" {
		t.Errorf("eth0 ipv4 %s ipv6 %s with %v", show(eth0.IPv4), show(eth0.IPv6), eth0.Unavailable)
	}
}

// A panic in one collector is raised again in the request's goroutine, so
// the dispatcher's recovery sees it, and the other collectors still finish.
func TestSummaryCollectorPanicReachesTheCaller(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.panicOn(pathVmstat, 1)
	defer func() {
		if v := recover(); v == nil || !strings.Contains(fmt.Sprint(v), "planted panic") {
			t.Errorf("recovered %v, want the planted panic", v)
		}
	}()
	_, _ = s.p.summary(context.Background(), spec.NoInput{})
	t.Error("summary returned after a collector panicked")
}
