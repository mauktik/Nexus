// Tests of planning/system-provider/test-cases.md TC-HP-40, TC-HP-41, TC-HP-42, TC-HP-43, TC-HP-44, TC-HP-45, TC-HP-46, TC-HP-47, TC-HP-E10.
// Owner WI: 2.4.

package hostparse

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TC-HP-40: Operstate mapping. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: a table.
// - Steps: `Operstate(s)` for `up\n`, `down`, `unknown`, `dormant`, `notpresent`, `lowerlayerdown`, `testing`, `""`, `garbage`.
// - Expected: `up`, `down`, `unknown`, `dormant`, `down`, `down`, `unknown`, `unknown`, `unknown`.
func TestTCHP40_OperstateMapping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"up\n", "up"},
		{"down", "down"},
		{"unknown", "unknown"},
		{"dormant", "dormant"},
		{"notpresent", "down"},
		{"lowerlayerdown", "down"},
		{"testing", "unknown"},
		{"", "unknown"},
		{"garbage", "unknown"},
	} {
		if got := Operstate([]byte(tc.in)); got != tc.want {
			t.Errorf("Operstate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Every captured operstate is one of the kernel's seven words
	// (sysfs-class-net) and maps as above.
	for _, c := range captures {
		files := globCap(t, c, "fs/sys/class/net/*/operstate")
		if len(files) == 0 {
			t.Fatalf("%s: no operstate captured", c)
		}
		for _, p := range files {
			raw := strings.TrimSpace(string(readFile(t, p)))
			want := map[string]string{"up": "up", "down": "down", "unknown": "unknown", "dormant": "dormant",
				"notpresent": "down", "lowerlayerdown": "down", "testing": "unknown"}[raw]
			if want == "" {
				t.Fatalf("%s: %q is not a kernel operstate", p, raw)
			}
			if got := Operstate(readFile(t, p)); got != want {
				t.Errorf("%s: Operstate = %q, want %q", p, got, want)
			}
		}
	}
}

// TC-HP-41: Link speed reports only positive values. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: captured `sys/class/net/*/speed` values; a table.
// - Steps: parse `1000\n`, `-1`, `0`, `-5`, `abc`, `""`.
// - Expected: `1000`; the rest are "not reported" (no error leaks to a value of 0 or negative).
func TestTCHP41_LinkSpeedReportsOnlyPositiveValues(t *testing.T) {
	if v, ok := LinkSpeed([]byte("1000\n")); !ok || v != 1000 {
		t.Errorf("LinkSpeed(1000) = %d, %v; want 1000, true", v, ok)
	}
	for _, in := range []string{"-1", "0", "-5", "abc", ""} {
		if v, ok := LinkSpeed([]byte(in)); ok || v != 0 {
			t.Errorf("LinkSpeed(%q) = %d, %v; want not reported", in, v, ok)
		}
	}
	seenPositive, seenUnknown := false, false
	for _, c := range captures {
		for _, p := range globCap(t, c, "fs/sys/class/net/*/speed") {
			raw, err := strconv.ParseInt(strings.TrimSpace(string(readFile(t, p))), 10, 64)
			if err != nil {
				t.Fatalf("test oracle: %s: %v", p, err)
			}
			v, ok := LinkSpeed(readFile(t, p))
			switch {
			case raw > 0:
				seenPositive = true
				if !ok || v != raw {
					t.Errorf("%s: LinkSpeed = %d, %v; want %d", p, v, ok, raw)
				}
			default:
				seenUnknown = true
				if ok || v != 0 {
					t.Errorf("%s (%d): LinkSpeed = %d, %v; want not reported", p, raw, v, ok)
				}
			}
		}
	}
	if !seenPositive || !seenUnknown {
		t.Fatalf("captures lack a positive (%v) or SPEED_UNKNOWN (%v) speed", seenPositive, seenUnknown)
	}
}

// lsblkDev is one device of `lsblk -b -J -o NAME,SIZE,ROTA,RM,...`.
type lsblkDev struct {
	Name     string     `json:"name"`
	Size     uint64     `json:"size"`
	Rota     bool       `json:"rota"`
	RM       bool       `json:"rm"`
	Type     string     `json:"type"`
	Children []lsblkDev `json:"children"`
}

// TC-HP-42: Block sizes and flags agree with lsblk. Owner WI 2.4. Requirements: FR-8, NFR-3.
//
// - Setup: UBU `fs/sys/block/*/{size,queue/rotational,removable}`, `tools/lsblk` (`lsblk -b -J`).
// - Steps: parse each.
// - Expected: size × 512 equals lsblk `size` for every disk lsblk lists as `type: disk`; rotational and removable equal lsblk `rota` and `rm`.
func TestTCHP42_BlockSizesAndFlagsAgreeWithLsblk(t *testing.T) {
	for _, c := range captures {
		var out struct {
			Blockdevices []lsblkDev `json:"blockdevices"`
		}
		if err := json.Unmarshal(readCap(t, c, "tools/lsblk"), &out); err != nil {
			t.Fatalf("%s: tools/lsblk: %v", c, err)
		}
		disks, parts := 0, 0
		for _, d := range out.Blockdevices {
			if d.Type != "disk" {
				continue
			}
			disks++
			dir := filepath.Join("fs/sys/block", d.Name)
			size, err := BlockSize(readCap(t, c, filepath.Join(dir, "size")))
			if err != nil || size != d.Size {
				t.Errorf("%s %s: BlockSize = %d, %v; lsblk size %d", c, d.Name, size, err, d.Size)
			}
			rot, err := Rotational(readCap(t, c, filepath.Join(dir, "queue/rotational")))
			if err != nil || rot != d.Rota {
				t.Errorf("%s %s: Rotational = %v, %v; lsblk rota %v", c, d.Name, rot, err, d.Rota)
			}
			rm, err := Removable(readCap(t, c, filepath.Join(dir, "removable")))
			if err != nil || rm != d.RM {
				t.Errorf("%s %s: Removable = %v, %v; lsblk rm %v", c, d.Name, rm, err, d.RM)
			}
			// Partition sizes are in 512-byte sectors too.
			for _, p := range d.Children {
				if p.Type != "part" {
					continue
				}
				parts++
				size, err := BlockSize(readCap(t, c, filepath.Join(dir, p.Name, "size")))
				if err != nil || size != p.Size {
					t.Errorf("%s %s: BlockSize = %d, %v; lsblk size %d", c, p.Name, size, err, p.Size)
				}
			}
		}
		if disks == 0 || parts == 0 {
			t.Fatalf("%s: lsblk lists %d disks and %d partitions", c, disks, parts)
		}
	}
}

// nameserverOracle is the address of each "nameserver" line of a
// resolv.conf as the test reads it.
func nameserverOracle(b []byte) []netip.Addr {
	var out []netip.Addr
	for _, l := range textLines(b) {
		if rest, ok := strings.CutPrefix(l, "nameserver "); ok {
			out = append(out, netip.MustParseAddr(strings.TrimSpace(rest)))
		}
	}
	return out
}

// TC-HP-43: resolv.conf parsing, stub detection and resolved-in-use. Owner WI 2.4. Requirements: FR-9.
//
// - Setup: UBU `fs/etc/resolv.conf` and `fs/run/systemd/resolve/resolv.conf`; ALP `fs/etc/resolv.conf`; synthetic: 17 nameservers, `search`/`options`/comment lines, `127.0.0.53` plus `1.1.1.1`.
// - Steps: parse; `Stub(addr)`; `ResolvedInUse(etc, run)`.
// - Expected: nameservers in file order, at most 16 (the 17th dropped); comments and other keywords ignored; `Stub` true for `127.0.0.53`, `127.0.0.1`, `::1`, false for `192.168.7.1`, `fe80::1`; `ResolvedInUse` true for UBU (when its etc file lists only `127.0.0.53`/`127.0.0.54`), false for ALP, false for the mixed synthetic, false when the run file is nil.
func TestTCHP43_ResolvConfParsingStubDetectionAndResolved(t *testing.T) {
	// The captures: the resolvers are the files' upstreams.
	for _, c := range captures {
		etc := readCap(t, c, "fs/etc/resolv.conf")
		want := nameserverOracle(etc)
		if len(want) == 0 {
			t.Fatalf("%s: capture resolv.conf has no nameserver", c)
		}
		if got := ResolvConf(etc); !slices.Equal(got, want) {
			t.Errorf("%s: ResolvConf = %v, want %v", c, got, want)
		}
		var run []byte
		if b, err := os.ReadFile(capPath(c, "fs/run/systemd/resolve/resolv.conf")); err == nil {
			run = b
			if got, want := ResolvConf(run), nameserverOracle(run); !slices.Equal(got, want) {
				t.Errorf("%s run file: ResolvConf = %v, want %v", c, got, want)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		// Neither capture runs systemd-resolved (Docker writes the etc
		// file with the VM's upstream), so neither etc file lists only
		// the resolved stubs: false for both, as for any host whose etc
		// file names a real upstream.
		onlyStubs := !slices.ContainsFunc(want, func(a netip.Addr) bool {
			return a != netip.MustParseAddr("127.0.0.53") && a != netip.MustParseAddr("127.0.0.54")
		})
		if wantInUse := onlyStubs && run != nil; ResolvedInUse(etc, run) != wantInUse {
			t.Errorf("%s: ResolvedInUse = %v, want %v", c, !wantInUse, wantInUse)
		}
	}

	// 17 nameservers among comments and other keywords: the first 16 in
	// order.
	var b strings.Builder
	var want []netip.Addr
	b.WriteString("# comment\n; nameserver 9.9.9.9\nsearch example.com\noptions edns0 trust-ad\nnameserver not-an-address\n")
	for i := 1; i <= 17; i++ {
		a := netip.AddrFrom4([4]byte{10, 0, 0, byte(i)})
		if i%2 == 0 {
			a = netip.MustParseAddr("fd00::" + strconv.Itoa(i))
		}
		b.WriteString("nameserver " + a.String() + "\n")
		if i == 3 {
			b.WriteString("domain example.com\n#nameserver 8.8.8.8\n")
		}
		if i <= 16 {
			want = append(want, a)
		}
	}
	if got := ResolvConf([]byte(b.String())); !slices.Equal(got, want) {
		t.Errorf("17 servers: ResolvConf = %v, want %v", got, want)
	}

	for _, a := range []string{"127.0.0.53", "127.0.0.1", "::1", "127.255.255.254"} {
		if !Stub(netip.MustParseAddr(a)) {
			t.Errorf("Stub(%s) = false", a)
		}
	}
	for _, a := range []string{"192.168.7.1", "fe80::1", "1.1.1.1", "::"} {
		if Stub(netip.MustParseAddr(a)) {
			t.Errorf("Stub(%s) = true", a)
		}
	}

	resolvedEtc := []byte("# This is /run/systemd/resolve/stub-resolv.conf managed by man:systemd-resolved(8).\nnameserver 127.0.0.53\noptions edns0 trust-ad\nsearch .\n")
	resolvedRun := []byte("# This is /run/systemd/resolve/resolv.conf managed by man:systemd-resolved(8).\nnameserver 192.168.5.1\nsearch .\n")
	mixed := []byte("nameserver 127.0.0.53\nnameserver 1.1.1.1\n")
	for _, tc := range []struct {
		name      string
		etc, run  []byte
		wantInUse bool
	}{
		{"stub and run file", resolvedEtc, resolvedRun, true},
		{"proxy stub", []byte("nameserver 127.0.0.54\nnameserver 127.0.0.53\n"), resolvedRun, true},
		{"run file without upstream", resolvedEtc, []byte("# no servers\n"), true},
		{"run file nil", resolvedEtc, nil, false},
		{"mixed", mixed, resolvedRun, false},
		{"other loopback", []byte("nameserver 127.0.0.1\n"), resolvedRun, false},
		{"no etc server", []byte("search .\n"), resolvedRun, false},
	} {
		if got := ResolvedInUse(tc.etc, tc.run); got != tc.wantInUse {
			t.Errorf("%s: ResolvedInUse = %v, want %v", tc.name, got, tc.wantInUse)
		}
	}
}

// TC-HP-44: passwd maps uid to name and skips malformed lines. Owner WI 2.4. Requirements: FR-10.
//
// - Setup: both captures' `etc/passwd`; synthetic lines `x`, `a:b:notanumber:…`, `+::::::` (NIS).
// - Steps: parse.
// - Expected: `0 → root`, each capture line's uid → name; malformed lines skipped without error.
func TestTCHP44_PasswdMapsUidToNameAndSkips(t *testing.T) {
	kept := regexp.MustCompile(`(?m)^passwd: reduced to .* \((\d+) of \d+ entries kept\)$`)
	for _, c := range captures {
		b := readCap(t, c, "fs/etc/passwd")
		m := Passwd(b)
		if m[0] != "root" {
			t.Errorf("%s: uid 0 = %q, want root", c, m[0])
		}
		for _, l := range textLines(b) {
			f := strings.Split(l, ":")
			if got := m[uint32(mustU64(t, f[2]))]; got != f[0] {
				t.Errorf("%s: uid %s = %q, want %q", c, f[2], got, f[0])
			}
		}
		// PROVENANCE records how many entries the capture kept.
		sm := kept.FindSubmatch(readCap(t, c, "PROVENANCE"))
		if sm == nil {
			t.Fatalf("%s: PROVENANCE has no passwd line", c)
		}
		if n := int(mustU64(t, string(sm[1]))); len(m) != n {
			t.Errorf("%s: Passwd has %d uids, PROVENANCE kept %d", c, len(m), n)
		}
	}

	in := "x\n" +
		"a:b:notanumber:0:gecos:/home/a:/bin/sh\n" +
		"+::::::\n" +
		"-bad:x:5:5::/:/bin/sh\n" +
		"#root:x:6:6::/:/bin/sh\n" +
		"short:x:7:7\n" +
		":x:8:8::/:/bin/sh\n" +
		"big:x:4294967296:0::/:/bin/sh\n" +
		"neg:x:-1:0::/:/bin/sh\n" +
		"root:x:0:0:root:/root:/bin/sh\r\n" +
		"toor:x:0:0:dup:/root:/bin/sh\n" +
		"nobody:x:65534:65534:nobody:/:/sbin/nologin"
	want := map[uint32]string{0: "root", 65534: "nobody"}
	got := Passwd([]byte(in))
	if len(got) != len(want) || got[0] != "root" || got[65534] != "nobody" {
		t.Errorf("Passwd(synthetic) = %v, want %v", got, want)
	}
	if got := Passwd(nil); len(got) != 0 {
		t.Errorf("Passwd(nil) = %v", got)
	}
}

// TC-HP-45: Timezone name and offset. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: `testdata/tz/America_New_York`; inputs: (a) `/etc/timezone` = `Europe/Berlin` and a localtime link to `…/zoneinfo/America/New_York`; (b) no `/etc/timezone`, link `/usr/share/zoneinfo/America/New_York`; (c) no link target (a copied file) and no `/etc/timezone`; (d) no `/etc/localtime`.
// - Steps: resolve name and offset at `2026-01-15T12:00:00Z` and `2026-07-15T12:00:00Z`.
// - Expected: (a) name `Europe/Berlin` (timezone file wins); (b) `America/New_York`, offsets −18000 and −14400 (computed with `time.LoadLocationFromTZData` from the tzfile in-test, not literals in the assertion); (c) name not reported, offset still parsed; (d) `UTC`, 0, source `default (no /etc/localtime)`.
func TestTCHP45_TimezoneNameAndOffset(t *testing.T) {
	ny := readFile(t, filepath.Join("..", "testdata", "tz", "America_New_York"))
	loc, err := time.LoadLocationFromTZData("America/New_York", ny)
	if err != nil {
		t.Fatalf("test oracle: %v", err)
	}
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	offset := func(at time.Time) int {
		_, off := at.In(loc).Zone()
		return off
	}
	if offset(winter) == offset(summer) {
		t.Fatalf("test oracle: the tzfile has no DST change between %v and %v", winter, summer)
	}

	for _, tc := range []struct {
		name                 string
		localtime, etcTZ     []byte
		link                 string
		wantName, wantSource string
	}{
		{"a", ny, []byte("Europe/Berlin\n"), "../usr/share/zoneinfo/America/New_York", "Europe/Berlin", TZSourceTimezoneFile},
		{"b", ny, nil, "/usr/share/zoneinfo/America/New_York", "America/New_York", TZSourceLink},
		{"c", ny, nil, "", "", ""},
	} {
		for _, at := range []time.Time{winter, summer} {
			got, err := Timezone(tc.localtime, tc.etcTZ, tc.link, at)
			if err != nil {
				t.Fatalf("(%s) %v: %v", tc.name, at, err)
			}
			if got.Name != tc.wantName || got.NameSource != tc.wantSource {
				t.Errorf("(%s) name %q from %q, want %q from %q", tc.name, got.Name, got.NameSource, tc.wantName, tc.wantSource)
			}
			if got.Offset != offset(at) || got.Source != TZSourceLocaltime {
				t.Errorf("(%s) %v: offset %d from %q, want %d from %q", tc.name, at, got.Offset, got.Source, offset(at), TZSourceLocaltime)
			}
		}
	}

	// (d) No /etc/localtime: UTC whatever /etc/timezone says.
	for _, etcTZ := range [][]byte{nil, []byte("Europe/Berlin\n")} {
		got, err := Timezone(nil, etcTZ, "", summer)
		want := TimezoneInfo{Name: "UTC", NameSource: TZSourceDefault, Offset: 0, Abbrev: "UTC", Source: "default (no /etc/localtime)"}
		if err != nil || got != want {
			t.Errorf("(d) etc/timezone %q: %+v, %v; want %+v", etcTZ, got, err, want)
		}
	}

	// Not a zone name: ignored, falling through to the link or nothing.
	for _, bad := range []string{"", "\n", "../../etc/passwd", "Europe/Berlin extra", "/abs/path", "a//b"} {
		got, err := Timezone(ny, []byte(bad), "/usr/share/zoneinfo/America/New_York", summer)
		if err != nil || got.Name != "America/New_York" {
			t.Errorf("etc/timezone %q: name %q, %v; want the link's", bad, got.Name, err)
		}
		if got, _ := Timezone(ny, nil, "/usr/share/zoneinfo/"+bad, summer); got.Name != "" {
			t.Errorf("link to %q: name %q, want not reported", bad, got.Name)
		}
	}
	// A localtime that is not a tzfile: an error, the name still set.
	got, err := Timezone([]byte("not a tzfile"), []byte("Europe/Berlin\n"), "", summer)
	if err == nil || got.Name != "Europe/Berlin" {
		t.Errorf("bad tzfile: %+v, %v; want an error and the name", got, err)
	}

	// The captures: Ubuntu's localtime is a copy of the file its link
	// (tools/readlink-localtime) names; Alpine has no /etc/localtime.
	link := strings.TrimSpace(string(readCap(t, "ubuntu-systemd", "tools/readlink-localtime")))
	_, wantName, _ := strings.Cut(link, "zoneinfo/")
	ubu, err := Timezone(readCap(t, "ubuntu-systemd", "fs/etc/localtime"), nil, link, summer)
	if err != nil || ubu.Name != wantName || ubu.Name != "Etc/UTC" || ubu.Offset != 0 || ubu.Abbrev != "UTC" || ubu.Source != TZSourceLocaltime {
		t.Errorf("ubuntu-systemd: %+v, %v; want Etc/UTC from the link, offset 0 from /etc/localtime", ubu, err)
	}
	if _, err := os.Stat(capPath("alpine-openrc", "fs/etc/localtime")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("alpine-openrc: expected no etc/localtime, stat: %v", err)
	}
	alp, err := Timezone(nil, nil, "", summer)
	if err != nil || alp.Name != "UTC" || alp.Offset != 0 || alp.Source != TZSourceDefault {
		t.Errorf("alpine-openrc: %+v, %v", alp, err)
	}
}

// captureVirtFacts gathers VirtFacts from a capture's fs/ the way the
// provider does from the host.
func captureVirtFacts(t *testing.T, c string) VirtFacts {
	t.Helper()
	exists := func(rel string) bool {
		_, err := os.Stat(capPath(c, "fs", rel))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	content := func(rel string) string {
		b, err := os.ReadFile(capPath(c, "fs", rel))
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	mounts, err := Mountinfo(readCap(t, c, "fs/proc/self/mountinfo"))
	if err != nil {
		t.Fatalf("%s: mountinfo: %v", c, err)
	}
	ci, err := Cpuinfo(readCap(t, c, "fs/proc/cpuinfo"))
	if err != nil {
		t.Fatalf("%s: cpuinfo: %v", c, err)
	}
	return VirtFacts{
		DockerEnv:        exists(".dockerenv"),
		ContainerEnv:     exists("run/.containerenv"),
		SystemdContainer: content("run/systemd/container"),
		Mounts:           mounts,
		ProcVZ:           exists("proc/vz"),
		ProcBC:           exists("proc/bc"),
		OSRelease:        content("proc/sys/kernel/osrelease"),
		HypervisorType:   content("sys/hypervisor/type"),
		SysVendor:        content("sys/class/dmi/id/sys_vendor"),
		ProductName:      content("sys/class/dmi/id/product_name"),
		HypervisorFlag:   ci.Hypervisor,
	}
}

// TC-HP-46: Virtualization classification. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: a facts table plus both captures' facts; `tools/systemd-detect-virt` where present.
// - Steps: `Classify(facts)`.
// - Expected:
//   - `/.dockerenv` → docker/container; `/run/.containerenv` → podman/container; `run/systemd/container` = `lxc` → lxc/container; `fuse.lxcfs` on `/proc/meminfo` → lxc; `/proc/vz` without `/proc/bc` → openvz; osrelease containing `microsoft` → wsl;
//   - a container marker plus DMI `QEMU` → the container (containers first);
//   - `sys/hypervisor/type` `xen` → xen/vm; each DMI table row → its type/vm (`innotek GmbH` → oracle; `Microsoft Corporation` + `Virtual Machine` → microsoft);
//   - hypervisor flag with no table match → unknown/vm; nothing → none/none;
//   - each capture equals its `tools/systemd-detect-virt` output; `evidence` names the marker used.
func TestTCHP46_VirtualizationClassification(t *testing.T) {
	lxcfs := []Mount{
		{MountPoint: "/", FSType: "ext4"},
		{MountPoint: "/proc/meminfo", FSType: "fuse.lxcfs"},
	}
	for _, tc := range []struct {
		name                string
		f                   VirtFacts
		typ, kind, evidence string
	}{
		{"dockerenv", VirtFacts{DockerEnv: true}, "docker", "container", "/.dockerenv"},
		{"containerenv", VirtFacts{ContainerEnv: true}, "podman", "container", "/run/.containerenv"},
		{"systemd container lxc", VirtFacts{SystemdContainer: "lxc\n"}, "lxc", "container", "/run/systemd/container: lxc"},
		{"systemd container nspawn", VirtFacts{SystemdContainer: "systemd-nspawn\n"}, "systemd-nspawn", "container", "/run/systemd/container: systemd-nspawn"},
		{"systemd container other", VirtFacts{SystemdContainer: "oci\n"}, "container-other", "container", "/run/systemd/container: oci"},
		{"lxcfs", VirtFacts{Mounts: lxcfs}, "lxc", "container", "fuse.lxcfs on /proc/meminfo"},
		{"lxcfs not over proc", VirtFacts{Mounts: []Mount{{MountPoint: "/var/lib/lxcfs", FSType: "fuse.lxcfs"}}}, "none", "none", "no virtualization marker"},
		{"openvz", VirtFacts{ProcVZ: true}, "openvz", "container", "/proc/vz without /proc/bc"},
		{"openvz host", VirtFacts{ProcVZ: true, ProcBC: true}, "none", "none", "no virtualization marker"},
		{"wsl2", VirtFacts{OSRelease: "5.15.167.4-microsoft-standard-WSL2\n"}, "wsl", "container", "/proc/sys/kernel/osrelease"},
		{"wsl1", VirtFacts{OSRelease: "4.4.0-19041-Microsoft\n"}, "wsl", "container", "/proc/sys/kernel/osrelease"},
		{"container first", VirtFacts{DockerEnv: true, SysVendor: "QEMU", HypervisorFlag: true}, "docker", "container", "/.dockerenv"},
		{"lxc over QEMU", VirtFacts{SystemdContainer: "lxc", SysVendor: "QEMU"}, "lxc", "container", "/run/systemd/container: lxc"},
		{"xen", VirtFacts{HypervisorType: "xen\n", SysVendor: "QEMU"}, "xen", "vm", "/sys/hypervisor/type"},
		{"QEMU", VirtFacts{SysVendor: "QEMU\n", ProductName: "Standard PC (Q35 + ICH9, 2009)\n"}, "qemu", "vm", "/sys/class/dmi/id/sys_vendor: QEMU"},
		{"KVM product over QEMU vendor", VirtFacts{SysVendor: "QEMU", ProductName: "KVM"}, "kvm", "vm", "/sys/class/dmi/id/product_name: KVM"},
		{"OpenStack", VirtFacts{SysVendor: "OpenStack Foundation", ProductName: "OpenStack Compute"}, "kvm", "vm", "/sys/class/dmi/id/product_name: OpenStack Compute"},
		{"VMware", VirtFacts{SysVendor: "VMware, Inc.", ProductName: "VMware20,1"}, "vmware", "vm", "/sys/class/dmi/id/product_name: VMware20,1"},
		{"innotek", VirtFacts{SysVendor: "innotek GmbH", ProductName: "VirtualBox"}, "oracle", "vm", "/sys/class/dmi/id/sys_vendor: innotek GmbH"},
		{"Oracle", VirtFacts{SysVendor: "Oracle Corporation"}, "oracle", "vm", "/sys/class/dmi/id/sys_vendor: Oracle Corporation"},
		{"Hyper-V", VirtFacts{SysVendor: "Microsoft Corporation", ProductName: "Virtual Machine"}, "microsoft", "vm", "/sys/class/dmi/id/sys_vendor: Microsoft Corporation, product_name: Virtual Machine"},
		{"Surface is no VM", VirtFacts{SysVendor: "Microsoft Corporation", ProductName: "Surface Pro 9"}, "none", "none", "no virtualization marker"},
		{"Xen DMI", VirtFacts{SysVendor: "Xen", ProductName: "HVM domU"}, "xen", "vm", "/sys/class/dmi/id/sys_vendor: Xen"},
		{"Amazon EC2", VirtFacts{SysVendor: "Amazon EC2", ProductName: "t3.micro"}, "amazon", "vm", "/sys/class/dmi/id/sys_vendor: Amazon EC2"},
		{"Google", VirtFacts{SysVendor: "Google", ProductName: "Google Compute Engine"}, "google", "vm", "/sys/class/dmi/id/product_name: Google Compute Engine"},
		{"Parallels", VirtFacts{SysVendor: "Parallels Software International Inc.", ProductName: "Parallels Virtual Platform"}, "parallels", "vm", "/sys/class/dmi/id/product_name: Parallels Virtual Platform"},
		{"BHYVE", VirtFacts{SysVendor: "FreeBSD", ProductName: "BHYVE"}, "bhyve", "vm", "/sys/class/dmi/id/product_name: BHYVE"},
		{"hypervisor flag, no table match", VirtFacts{SysVendor: "Some Vendor", ProductName: "Some Board", HypervisorFlag: true}, "unknown", "vm", "cpuinfo hypervisor flag"},
		{"bare metal", VirtFacts{SysVendor: "Dell Inc.", ProductName: "PowerEdge R640", OSRelease: "6.8.0-100-generic"}, "none", "none", "no virtualization marker"},
		{"nothing", VirtFacts{}, "none", "none", "no virtualization marker"},
	} {
		got := Classify(tc.f)
		want := Virtualization{Type: tc.typ, Kind: tc.kind, Evidence: tc.evidence}
		if got != want {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, got, want)
		}
	}

	// The captures. Ubuntu ran systemd-detect-virt; Alpine has none, and
	// its PROVENANCE host line records the virtualization capture.sh saw.
	provVirt := regexp.MustCompile(`(?m)^host: .*\bvirtualization (\S+)`)
	for _, c := range captures {
		var want string
		if b, err := os.ReadFile(capPath(c, "tools/systemd-detect-virt")); err == nil {
			want = strings.TrimSpace(string(b))
		} else if errors.Is(err, fs.ErrNotExist) {
			sm := provVirt.FindSubmatch(readCap(t, c, "PROVENANCE"))
			if sm == nil {
				t.Fatalf("%s: no virtualization in PROVENANCE", c)
			}
			want = string(sm[1])
		} else {
			t.Fatal(err)
		}
		got := Classify(captureVirtFacts(t, c))
		if got.Type != want || got.Kind != VirtKindContainer || got.Evidence != "/.dockerenv" {
			t.Errorf("%s: Classify = %+v, want type %q (container, /.dockerenv)", c, got, want)
		}
	}
}

// Real `chronyc -c tracking` lines: chrony 4.8 (Alpine package) run as
// `chronyd -x -f c.conf` in a throwaway alpine:latest container on
// 2026-11-15, c.conf holding `server time.cloudflare.com iburst`;
// chronyTrackingSynced after 20 s, chronyTrackingUnsynced after 1 s. The
// WI 2.1 captures have no chrony.
const (
	chronyTrackingSynced   = "A29FC801,162.159.200.1,4,1794712543.797592244,0.001606995,-0.001198918,0.001198918,0.000,-122.355,1000000.000,0.035488494,13.866610527,1.8,Normal\n"
	chronyTrackingUnsynced = "00000000,,0,0.000000000,0.000000000,0.000000000,0.000000000,0.000,0.000,0.000,1.000000000,1.000000000,0.0,Not synchronised\n"
)

// withLeap is chronyTrackingSynced with field 14 replaced.
func withLeap(leap string) string {
	f := strings.Split(strings.TrimSuffix(chronyTrackingSynced, "\n"), ",")
	f[13] = leap
	return strings.Join(f, ",") + "\n"
}

// TC-HP-47: Time-sync output parsers. Owner WI 2.4. Requirements: FR-5.
//
// - Setup: captured `tools/timedatectl` and, where present, `tools/chronyc-tracking`; synthetic CSV lines derived from the capture with field 14 replaced.
// - Steps: `TimedatectlNTPSynchronized`, `ChronyTracking`.
// - Expected: `yes\n` → true, `no` → false, `maybe` → error; field 14 `Normal`, `Insert second`, `Delete second` → true, `Not synchronised` → false, other → error; a line with 13 fields → error.
func TestTCHP47_TimeSyncOutputParsers(t *testing.T) {
	captured := readCap(t, "ubuntu-systemd", "tools/timedatectl")
	if string(captured) != "yes\n" {
		t.Fatalf("test oracle: tools/timedatectl is %q", captured)
	}
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{string(captured), true, false},
		{"yes\n", true, false},
		{"no", false, false},
		{"no\n", false, false},
		{"maybe", false, true},
		{"", false, true},
		{"Yes", false, true},
		{"NTPSynchronized=yes\n", false, true},
		{"yes\nno\n", false, true},
	} {
		got, err := TimedatectlNTPSynchronized([]byte(tc.in))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("TimedatectlNTPSynchronized(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}

	for _, c := range captures {
		if _, err := os.Stat(capPath(c, "tools/chronyc-tracking")); err == nil {
			t.Errorf("%s: tools/chronyc-tracking is now captured; compare ChronyTracking with it", c)
		}
	}
	thirteen := strings.Join(strings.Split(chronyTrackingSynced, ",")[:13], ",") + "\n"
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{chronyTrackingSynced, true, false},
		{chronyTrackingUnsynced, false, false},
		{withLeap("Normal"), true, false},
		{withLeap("Insert second"), true, false},
		{withLeap("Delete second"), true, false},
		{withLeap("Not synchronised"), false, false},
		{withLeap("Invalid"), false, true},
		{withLeap("normal"), false, true},
		{withLeap(""), false, true},
		{thirteen, false, true},
		{"", false, true},
		{"506 Cannot talk to daemon\n", false, true},
	} {
		got, err := ChronyTracking([]byte(tc.in))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ChronyTracking(%q) = %v, %v; want %v, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// TC-HP-E10: The /sys, /etc and /run parsers survive the fuzz seeds. Owner WI 2.4. Requirements: NFR-4.
//
// - Setup: seeds from both captures plus the synthetic tables of TC-HP-40..47.
// - Steps: `go test` runs the seeds.
// - Expected: no panic; `ChronyTracking` and `TimedatectlNTPSynchronized` return errors, not values, for every non-matching seed.
func TestTCHPE10_TheSysEtcAndRunParsersSurvive(t *testing.T) {
	// Every seed of every WI 2.4 fuzz target, through every WI 2.4 parser.
	seeds := map[string]bool{}
	for _, target := range wi24FuzzTargets {
		globs, ok := fuzzSeedGlobs[target]
		if !ok {
			t.Fatalf("%s has no fuzzSeedGlobs entry", target)
		}
		for _, c := range captures {
			for _, dir := range fuzzSeedDirs {
				for _, g := range globs {
					for _, m := range globCap(t, c, filepath.Join(dir, g)) {
						if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
							seeds[string(readFile(t, m))] = true
						}
					}
				}
			}
		}
		for _, s := range fuzzSyntheticSeeds[target] {
			seeds[s] = true
		}
	}
	if len(seeds) < 20 {
		t.Fatalf("only %d distinct seeds", len(seeds))
	}

	// The seeds that are real outputs of each command.
	timedatectlOK := map[string]bool{"yes\n": true, "no\n": true}
	chronyOK := map[string]bool{chronyTrackingSynced: true, chronyTrackingUnsynced: true}
	for _, l := range []string{"Normal", "Insert second", "Delete second", "Not synchronised"} {
		chronyOK[withLeap(l)] = true
	}

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for s := range seeds {
		b := []byte(s)
		_, _ = ScalarInt(b)
		_ = ScalarString(b)
		_, _ = Millidegrees(b)
		_ = Operstate(b)
		_, _ = LinkSpeed(b)
		_, _ = BlockSize(b)
		_, _ = Rotational(b)
		_, _ = Removable(b)
		_ = ResolvConf(b)
		_ = ResolvedInUse(b, b)
		_ = Passwd(b)
		_, _ = Timezone(b, b, s, now)
		if v, err := TimedatectlNTPSynchronized(b); err == nil && !timedatectlOK[s] {
			t.Errorf("TimedatectlNTPSynchronized(%.60q) = %v, want an error", s, v)
		}
		if v, err := ChronyTracking(b); err == nil && !chronyOK[s] {
			t.Errorf("ChronyTracking(%.60q) = %v, want an error", s, v)
		}
	}
}

func readFile(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
