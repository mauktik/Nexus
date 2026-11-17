//go:build integration

// T1 of the host provider (system-provider WI 5.3): every host operation
// under the exact hardened unit that install.sh installs (systemd, Ubuntu)
// and under the shipped OpenRC script (Alpine), with the expected values
// read from the host's own tools and files (hostname, /etc/os-release,
// uname, nproc, free, df, lsblk or /sys/block, ip -j, systemctl,
// rc-status, timedatectl), never from Nexus. Each TC is its own test, so
// `make test-harness RUN='^TestTCHP|^TestEveryProviderOpUnderExactUnit'`
// and `make test-harness-alpine RUN='^TestTCHP'` select exactly them; a
// test written for the other init system skips through requireInit.
//
// The sampler's start and stop are logged by the daemon's background
// manager (internal/daemon/background.go) as "starting background provider"
// and "stopping background provider" with provider host: those are the
// "sampler started" and "sampler stopped" lines TC-HP-116 and TC-HP-121
// read.
//
// TC-HP-131 (WI 5.4) checks docs/host.md's sandbox section against TC-HP-115's list.

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
)

const (
	// hostInterval is providers.host.sampleInterval in every config here.
	hostInterval = 2 * time.Second
	// hostOutDir holds the files a host TC writes for the run's logs;
	// run.sh copies it into build/harness/logs/<stamp>/.
	hostOutDir = "/harness/out"
	// The JSON log lines of the sampler's start and stop (see above).
	hostStartLog = `"msg":"starting background provider"`
	hostStopLog  = `"msg":"stopping background provider"`
	hostLogAttr  = `"provider":"host"`
)

// hostReasonPrefixes is FR-12's reason vocabulary (internal/providers/host/
// reason.go, docs/host.md): every unavailable reason starts with one.
var hostReasonPrefixes = []string{
	"warming up", "not running", "internal error: access not allow-listed", "stale: last sample ",
	"permission denied: ", "not present: ", "timed out after ", "still pending: ", "not visible: ",
	"unrecognized format: ", "counter reset: ", "not reported: ", "command failed: ", "not supported: ",
}

// hostUsageFields are a filesystem's fields that a statfs fills.
var hostUsageFields = []string{"sizeBytes", "usedBytes", "availableBytes", "usedPercent", "inodesTotal", "inodesUsed",
	"inodesFree"}

// hostDaemon is nexusd with the host provider enabled at a 2 s interval,
// installed by install.sh and started under the exact unit or the shipped
// OpenRC script.
type hostDaemon struct {
	*nexusdInstance
	pid          int       // MainPID (systemd) or openrcMainPID
	since        time.Time // just before the start
	kernelBefore []string  // the kernel ring buffer before the start
}

// hostConfig is the nexusd config of these tests: loopback, plain HTTP and
// providers.host {enabled, sampleInterval: 2s}, then extra providers.
func hostConfig(enabled bool, extra string) string {
	return fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
		"  host: {enabled: %t, sampleInterval: 2s}\n%s", exactAddr, enabled, extra)
}

// hostSetupSystemd installs nexusd with install.sh on a wiped host, writes
// the config with extra providers, starts the exact unit (requireExactUnit:
// the installed unit is byte-equal to deploy/nexusd.service, no drop-in)
// and waits until the sampler is ready. The token holds host:read and
// scopes.
func hostSetupSystemd(t *testing.T, extra string, scopes ...string) *hostDaemon {
	t.Helper()
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	writeConfig(t, hostConfig(true, extra))
	d := &hostDaemon{kernelBefore: kernelLines(t), since: time.Now().Add(-time.Second)}
	d.pid = startExact(t)
	t.Logf("nexusd.service: %s sha256 %s = deploy/nexusd.service sha256 %s, no drop-in; MainPID %d",
		installedUnit, sha(t, installedUnit), sha(t, shippedUnit), d.pid)
	// exactNexusd names its token after the test, longer than a token
	// name may be for these test names.
	d.nexusdInstance = &nexusdInstance{t: t, listen: exactAddr,
		token:  createToken(t, "tc-hp", append([]string{hostspec.ReadScope}, scopes...)...),
		client: &http.Client{Timeout: 60 * time.Second}}
	hostWaitReady(t, d)
	return d
}

// hostSetupOpenRC is hostSetupSystemd on OpenRC: install.sh, the config,
// lib.sh start-nexusd (the shipped script), a host:read token minted by
// nexusd itself, and the sampler ready.
func hostSetupOpenRC(t *testing.T) *hostDaemon {
	t.Helper()
	requireInit(t, platform.OpenRC)
	freshHost(t)
	runInstall(t)
	writeConfig(t, hostConfig(true, ""))
	d := &hostDaemon{since: time.Now().Add(-time.Second)}
	lib(t, "start-nexusd")
	if d.pid = openrcMainPID(t); d.pid == 0 {
		t.Fatalf("nexusd is not started after start-nexusd: rc-service says %s", rcStatus(t))
	}
	t.Logf("nexusd under the shipped OpenRC script: %s sha256 %s, PID %d", openrcScript, sha(t, openrcScript), d.pid)
	d.nexusdInstance = &nexusdInstance{t: t, listen: exactAddr, token: createToken(t, "tc-hp", hostspec.ReadScope),
		client: &http.Client{Timeout: 60 * time.Second}}
	hostWaitReady(t, d)
	return d
}

// hostSetup is the daemon of a test that runs on both init systems.
func hostSetup(t *testing.T) *hostDaemon {
	t.Helper()
	switch detectedInit() {
	case platform.Systemd:
		return hostSetupSystemd(t, "")
	case platform.OpenRC:
		return hostSetupOpenRC(t)
	}
	t.Fatalf("the harness has no supported init system (detected %s)", detectedInit())
	return nil
}

// hostWaitReady waits up to 10 s for host.summary.get's sampler.state ready.
func hostWaitReady(t *testing.T, d *hostDaemon) {
	t.Helper()
	start := time.Now()
	waitUntil(t, 10*time.Second, "host.summary.get sampler.state ready", func() (bool, string) {
		code, b := d.get(hostOpPath(hostspec.SummaryGet.Op()))
		if code != http.StatusOK {
			return false, fmt.Sprintf("%d %s", code, b)
		}
		var l apitypes.List[hostspec.Summary]
		if err := json.Unmarshal(b, &l); err != nil || len(l.Items) != 1 {
			return false, string(b)
		}
		return l.Items[0].Properties.Sampler.State == hostspec.SamplerReady, l.Items[0].Properties.Sampler.State
	})
	t.Logf("host sampler ready %v after the start", time.Since(start).Round(time.Millisecond))
}

// hostOpPath is an op's REST path; the lists ask for limit=1000, the
// maximum, so one page holds every item.
func hostOpPath(op def.Op) string {
	p := "/v1/providers/" + hostspec.Name + "/" + op.ResourceType
	switch op.ResourceType {
	case hostspec.FilesystemsType, hostspec.DisksType, hostspec.InterfacesType:
		p += "?limit=1000"
	}
	return p
}

// hostGet is one op's response body; anything but 200 fails the test.
func hostGet(t *testing.T, d *hostDaemon, op def.Op) []byte {
	t.Helper()
	code, b := d.get(hostOpPath(op))
	if code != http.StatusOK {
		t.Fatalf("%s (GET %s): %d %s", op.ID, hostOpPath(op), code, b)
	}
	return b
}

// hostCallAll calls the ten host ops over REST, each answering 200, and
// returns their bodies by op ID; a list's single page must be its last.
func hostCallAll(t *testing.T, d *hostDaemon) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, op := range hostspec.Spec().Ops {
		b := hostGet(t, d, op)
		var l struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(b, &l); err != nil {
			t.Fatalf("%s: %v\n%s", op.ID, err, b)
		}
		if l.NextCursor != "" {
			t.Fatalf("%s: a second page at limit=1000 (nextCursor %q)", op.ID, l.NextCursor)
		}
		out[op.ID] = b
	}
	if len(out) != 10 {
		t.Fatalf("%d host ops called, want 10", len(out))
	}
	return out
}

// hostOne is the properties of a one-item read (current).
func hostOne[P any](t *testing.T, op string, b []byte) P {
	t.Helper()
	items := hostList[P](t, op, b)
	if len(items) != 1 || items[0].Name != hostspec.CurrentName {
		t.Fatalf("%s: %d items, want one named %s:\n%s", op, len(items), hostspec.CurrentName, b)
	}
	return items[0].Properties
}

// hostList is the items of a list response.
func hostList[P any](t *testing.T, op string, b []byte) []apitypes.Resource[P] {
	t.Helper()
	var l apitypes.List[apitypes.Resource[P]]
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("%s: %v\n%s", op, err, b)
	}
	return l.Items
}

func hostHealth(t *testing.T, d *hostDaemon) hostspec.HealthProperties {
	t.Helper()
	op := hostspec.HealthGet.Op()
	return hostOne[hostspec.HealthProperties](t, op.ID, hostGet(t, d, op))
}

func hostIdentity(t *testing.T, d *hostDaemon) hostspec.IdentityProperties {
	t.Helper()
	op := hostspec.IdentityGet.Op()
	return hostOne[hostspec.IdentityProperties](t, op.ID, hostGet(t, d, op))
}

func hostFilesystems(t *testing.T, d *hostDaemon) map[string]hostspec.FilesystemProperties {
	t.Helper()
	op := hostspec.FilesystemsList.Op()
	out := map[string]hostspec.FilesystemProperties{}
	for _, it := range hostList[hostspec.FilesystemProperties](t, op.ID, hostGet(t, d, op)) {
		out[it.Properties.MountPoint] = it.Properties
	}
	return out
}

// stdoutOf runs a command and returns its standard output alone, with its
// error; stderr is folded into the error.
func stdoutOf(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), err
}

// osRelease parses /etc/os-release as the test reads it.
func osRelease(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		m[k] = v
	}
	return m
}

// dfRow is df's size and used bytes of one mount point.
type dfRow struct{ size, used uint64 }

// hostDisk is a whole block device as the tools report it.
type hostDisk struct {
	size     uint64
	rota, rm bool
}

// hostTools is what the host's own tools say, at one moment.
type hostTools struct {
	hostname, kernel, arch string
	osRelease              map[string]string
	nproc                  int
	memTotal, memAvail     uint64
	df                     map[string]dfRow
	disks                  map[string]hostDisk // every device the tool lists, by kernel name
	wholeDisks             []string            // the whole disks the provider must list
	addrs                  map[string][]string // interface -> sorted address/prefix
	gateways               []string            // sorted "family address interface metric"
}

// hostToolView runs the host's tools: GNU tools and lsblk on systemd
// (Ubuntu), BusyBox df and /sys/block on OpenRC (Alpine). Both images
// carry procps (free), iproute2 (ip -j) and coreutils' nproc.
func hostToolView(t *testing.T, openrc bool) hostTools {
	t.Helper()
	v := hostTools{
		hostname:  strings.TrimSpace(mustSh(t, "hostname")),
		kernel:    strings.TrimSpace(mustSh(t, "uname", "-r")),
		arch:      strings.TrimSpace(mustSh(t, "uname", "-m")),
		osRelease: osRelease(t),
		df:        map[string]dfRow{},
		disks:     map[string]hostDisk{},
		addrs:     map[string][]string{},
	}
	n, err := strconv.Atoi(strings.TrimSpace(mustSh(t, "nproc")))
	if err != nil {
		t.Fatal(err)
	}
	v.nproc = n

	for line := range strings.Lines(mustSh(t, "free", "-b")) {
		f := strings.Fields(line)
		if len(f) >= 7 && f[0] == "Mem:" {
			v.memTotal, _ = strconv.ParseUint(f[1], 10, 64)
			v.memAvail, _ = strconv.ParseUint(f[6], 10, 64)
		}
	}
	if v.memTotal == 0 || v.memAvail == 0 {
		t.Fatalf("free -b: no Mem: total and available")
	}

	// df -a: every mount, so a filesystem that df would hide as a
	// duplicate of another is still in the table. A row that cannot be
	// read (statfs failed, "-") is left out; df's exit status then is 1.
	dfArgs := []string{"-a", "-B1", "--output=target,size,used,avail,pcent"}
	if openrc {
		dfArgs = []string{"-a", "-B1", "-P"}
	}
	out, err := stdoutOf(time.Minute, "df", dfArgs...)
	if err != nil {
		t.Logf("df %s (rows read anyway): %v", strings.Join(dfArgs, " "), err)
	}
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		var target, size, used string
		switch {
		case openrc && len(f) >= 6:
			target, size, used = f[len(f)-1], f[1], f[2]
		case !openrc && len(f) == 5:
			target, size, used = f[0], f[1], f[2]
		default:
			continue
		}
		s, err1 := strconv.ParseUint(size, 10, 64)
		u, err2 := strconv.ParseUint(used, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		v.df[target] = dfRow{size: s, used: u}
	}

	if openrc {
		ents, err := os.ReadDir("/sys/block")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
				strings.HasPrefix(name, "loop") && !exists(filepath.Join("/sys/block", name, "loop", "backing_file")) {
				continue
			}
			sectors := sysUint(t, filepath.Join("/sys/block", name, "size"))
			v.disks[name] = hostDisk{size: sectors * 512,
				rota: sysUint(t, filepath.Join("/sys/block", name, "queue", "rotational")) == 1,
				rm:   sysUint(t, filepath.Join("/sys/block", name, "removable")) == 1}
			v.wholeDisks = append(v.wholeDisks, name)
		}
	} else {
		var lb struct {
			Blockdevices []lsblkDev `json:"blockdevices"`
		}
		if err := json.Unmarshal([]byte(mustSh(t, "lsblk", "-b", "-J", "-o", "NAME,SIZE,ROTA,RM,TYPE")), &lb); err != nil {
			t.Fatal(err)
		}
		var walk func([]lsblkDev)
		walk = func(ds []lsblkDev) {
			for _, d := range ds {
				v.disks[d.Name] = hostDisk{size: lsblkUint(t, d.Size), rota: lsblkFlag(t, d.Rota), rm: lsblkFlag(t, d.RM)}
				if d.Type == "disk" {
					v.wholeDisks = append(v.wholeDisks, d.Name)
				}
				walk(d.Children)
			}
		}
		walk(lb.Blockdevices)
	}
	slices.Sort(v.wholeDisks)

	var ifs []struct {
		Ifname   string `json:"ifname"`
		AddrInfo []struct {
			Family    string `json:"family"`
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(mustSh(t, "ip", "-j", "addr")), &ifs); err != nil {
		t.Fatal(err)
	}
	for _, i := range ifs {
		as := []string{}
		for _, a := range i.AddrInfo {
			if a.Family == "inet" || a.Family == "inet6" {
				as = append(as, fmt.Sprintf("%s/%d", a.Local, a.Prefixlen))
			}
		}
		slices.Sort(as)
		v.addrs[i.Ifname] = as
	}

	for _, fam := range []struct{ flag, name string }{{"-4", "ipv4"}, {"-6", "ipv6"}} {
		var rs []struct {
			Dst     string `json:"dst"`
			Gateway string `json:"gateway"`
			Dev     string `json:"dev"`
			Metric  int    `json:"metric"`
		}
		if err := json.Unmarshal([]byte(mustSh(t, "ip", "-j", fam.flag, "route", "show", "default")), &rs); err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Gateway != "" {
				v.gateways = append(v.gateways, fmt.Sprintf("%s %s %s %d", fam.name, r.Gateway, r.Dev, r.Metric))
			}
		}
	}
	slices.Sort(v.gateways)
	return v
}

// lsblkDev is one lsblk -J device; ROTA and RM are booleans in current
// util-linux and "0"/"1" in older releases.
type lsblkDev struct {
	Name     string          `json:"name"`
	Size     json.RawMessage `json:"size"`
	Rota     json.RawMessage `json:"rota"`
	RM       json.RawMessage `json:"rm"`
	Type     string          `json:"type"`
	Children []lsblkDev      `json:"children"`
}

func lsblkFlag(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	switch strings.Trim(string(raw), `"`) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	}
	t.Fatalf("lsblk flag %s", raw)
	return false
}

func lsblkUint(t *testing.T, raw json.RawMessage) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.Trim(string(raw), `"`), 10, 64)
	if err != nil {
		t.Fatalf("lsblk size %s: %v", raw, err)
	}
	return n
}

func sysUint(t *testing.T, path string) uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return n
}

// hostKeys is m's keys, sorted.
func hostKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func deref[T any](p *T) (T, bool) {
	if p == nil {
		var zero T
		return zero, false
	}
	return *p, true
}

// within reports whether got is within frac of want.
func within(got, want uint64, frac float64) bool {
	return math.Abs(float64(got)-float64(want)) <= frac*float64(want)
}

// hostCompareTools is TC-HP-114 and TC-HP-122's comparison: the host's
// tools, the ten ops, the tools again, then every value checked against
// the tools' reading before or after the calls. It returns the bodies.
func hostCompareTools(t *testing.T, d *hostDaemon, openrc bool) map[string][]byte {
	t.Helper()
	before := hostToolView(t, openrc)
	bodies := hostCallAll(t, d)
	after := hostToolView(t, openrc)
	either := func(f func(hostTools) bool) bool { return f(before) || f(after) }

	// Identity.
	id := hostOne[hostspec.IdentityProperties](t, hostspec.IdentityGet.Op().ID, bodies[hostspec.IdentityGet.Op().ID])
	strField := func(what string, got *string, want func(hostTools) string) {
		t.Helper()
		g, ok := deref(got)
		if !ok || !either(func(v hostTools) bool { return want(v) == g }) {
			t.Errorf("identity %s %q (set %v), host's tools %q", what, g, ok, want(before))
			return
		}
		t.Logf("identity %-13s %q = host's tools", what, g)
	}
	strField("hostname", id.Hostname, func(v hostTools) string { return v.hostname })
	strField("os.id", id.OS.ID, func(v hostTools) string { return v.osRelease["ID"] })
	strField("os.versionId", id.OS.VersionID, func(v hostTools) string { return v.osRelease["VERSION_ID"] })
	strField("os.prettyName", id.OS.PrettyName, func(v hostTools) string { return v.osRelease["PRETTY_NAME"] })
	strField("kernelRelease", id.KernelRelease, func(v hostTools) string { return v.kernel })
	strField("architecture", id.Architecture, func(v hostTools) string { return v.arch })

	// CPU.
	cpu := hostOne[hostspec.CPUProperties](t, hostspec.CPUGet.Op().ID, bodies[hostspec.CPUGet.Op().ID])
	if n, ok := deref(cpu.LogicalCPUs); !ok || n != before.nproc {
		t.Errorf("cpu logicalCpus %d (set %v), nproc %d", n, ok, before.nproc)
	} else {
		t.Logf("cpu logicalCpus %d = nproc", n)
	}

	// Memory: total exact, available within 1%.
	mem := hostOne[hostspec.MemoryProperties](t, hostspec.MemoryGet.Op().ID, bodies[hostspec.MemoryGet.Op().ID])
	if tot, ok := deref(mem.TotalBytes); !ok || !either(func(v hostTools) bool { return v.memTotal == tot }) {
		t.Errorf("memory totalBytes %d (set %v), free -b total %d", tot, ok, before.memTotal)
	} else {
		t.Logf("memory totalBytes %d = free -b", tot)
	}
	if av, ok := deref(mem.AvailableBytes); !ok || !either(func(v hostTools) bool { return within(av, v.memAvail, 0.01) }) {
		t.Errorf("memory availableBytes %d (set %v), free -b available %d / %d (±1%%)", av, ok, before.memAvail, after.memAvail)
	} else {
		t.Logf("memory availableBytes %d within 1%% of free -b's %d / %d", av, before.memAvail, after.memAvail)
	}

	// Filesystems: size exact, used within one block of the df bracket.
	fsOp := hostspec.FilesystemsList.Op().ID
	compared := 0
	for _, it := range hostList[hostspec.FilesystemProperties](t, fsOp, bodies[fsOp]) {
		fs := it.Properties
		size, ok := deref(fs.SizeBytes)
		if !ok {
			t.Logf("filesystem %s (%s): no usage (%q)", fs.MountPoint, fs.FSType, fs.Unavailable["sizeBytes"])
			continue
		}
		b, okB := before.df[fs.MountPoint]
		a, okA := after.df[fs.MountPoint]
		if !okB && !okA {
			t.Errorf("filesystem %s (%s) is listed, but df -a has no row for it", fs.MountPoint, fs.FSType)
			continue
		}
		if !okB {
			b = a
		}
		if !okA {
			a = b
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(fs.MountPoint, &st); err != nil {
			t.Errorf("statfs %s: %v", fs.MountPoint, err)
			continue
		}
		block := uint64(st.Frsize)
		if block == 0 {
			block = uint64(st.Bsize)
		}
		used, _ := deref(fs.UsedBytes)
		lo, hi := min(b.used, a.used), max(b.used, a.used)
		switch {
		case size != b.size && size != a.size:
			t.Errorf("filesystem %s sizeBytes %d, df %d / %d", fs.MountPoint, size, b.size, a.size)
		case fs.UsedBytes == nil || used+block < lo || used > hi+block:
			t.Errorf("filesystem %s usedBytes %d, df %d..%d (block %d)", fs.MountPoint, used, lo, hi, block)
		default:
			compared++
			t.Logf("filesystem %-24s %-8s size %d = df; used %d in df's %d..%d ± %d", fs.MountPoint, fs.FSType, size, used, lo,
				hi, block)
		}
	}
	if _, ok := before.df["/"]; compared == 0 || !ok {
		t.Errorf("%d filesystems compared with df; / in df: %v", compared, ok)
	}

	// Disks.
	dkOp := hostspec.DisksList.Op().ID
	disks := map[string]hostspec.DiskProperties{}
	for _, it := range hostList[hostspec.DiskProperties](t, dkOp, bodies[dkOp]) {
		disks[it.Name] = it.Properties
	}
	for _, name := range before.wholeDisks {
		if _, ok := disks[name]; !ok {
			t.Errorf("disk %s is listed by the tools but not by host.disks.list (%v)", name, hostKeys(disks))
		}
	}
	dcompared := 0
	for _, name := range hostKeys(disks) {
		p := disks[name]
		want, ok := before.disks[name]
		if !ok {
			t.Logf("disk %s: not listed by the tools (size %v)", name, p.SizeBytes)
			continue
		}
		size, _ := deref(p.SizeBytes)
		rota, okR := deref(p.Rotational)
		rm, okM := deref(p.Removable)
		if p.SizeBytes == nil || size != want.size || !okR || rota != want.rota || !okM || rm != want.rm {
			t.Errorf("disk %s: size %d rotational %v (%v) removable %v (%v), tools %+v", name, size, rota, okR, rm, okM, want)
			continue
		}
		dcompared++
		t.Logf("disk %-8s size %d rotational %v removable %v = tools", name, size, rota, rm)
	}
	if dcompared == 0 {
		t.Errorf("no disk compared: provider %v, tools %v", hostKeys(disks), before.wholeDisks)
	}

	// Interfaces: each interface's addresses with prefixes.
	ifOp := hostspec.InterfacesList.Op().ID
	ifaces := map[string][]string{}
	for _, it := range hostList[hostspec.InterfaceProperties](t, ifOp, bodies[ifOp]) {
		as := []string{}
		for _, a := range append(slices.Clone(it.Properties.IPv4), it.Properties.IPv6...) {
			as = append(as, fmt.Sprintf("%s/%d", a.Address, a.PrefixLength))
		}
		slices.Sort(as)
		ifaces[it.Name] = as
	}
	if got, want := hostKeys(ifaces), hostKeys(before.addrs); !slices.Equal(got, want) {
		t.Errorf("interfaces %q, ip -j addr %q", got, want)
	}
	for name, got := range ifaces {
		if !either(func(v hostTools) bool { return slices.Equal(got, v.addrs[name]) }) {
			t.Errorf("interface %s addresses %q, ip -j addr %q", name, got, before.addrs[name])
			continue
		}
		t.Logf("interface %-12s %q = ip -j addr", name, got)
	}

	// Default gateways.
	ncOp := hostspec.NetConfigGet.Op().ID
	nc := hostOne[hostspec.NetConfigProperties](t, ncOp, bodies[ncOp])
	gws := []string{}
	for _, g := range nc.DefaultGateways {
		gws = append(gws, fmt.Sprintf("%s %s %s %d", g.Family, g.Address, g.Interface, g.Metric))
	}
	slices.Sort(gws)
	if !either(func(v hostTools) bool { return slices.Equal(gws, v.gateways) }) {
		t.Errorf("defaultGateways %q, ip -j route show default %q", gws, before.gateways)
	} else {
		t.Logf("defaultGateways %q = ip -j route", gws)
	}
	return bodies
}

// hostReason is one entry of an unavailable map: op, JSON path, reason.
type hostReason struct{ op, path, reason string }

// hostUnavailable is TC-HP-115's inline D-5 walker over every response: no
// null without a reason; every unavailable key names a field of its object
// that is null, [] or the tri-state "unknown"; every reason starts with a
// vocabulary prefix; no list item is null except in series[].values. The
// (op, path, reason) list goes to hostOutDir/file and the test log.
func hostUnavailable(t *testing.T, bodies map[string][]byte, file string) []hostReason {
	t.Helper()
	var rs []hostReason
	for _, op := range hostspec.Spec().Ops {
		dec := json.NewDecoder(bytes.NewReader(bodies[op.ID]))
		dec.UseNumber()
		var doc any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		var errs []string
		hostWalk(op.ID, "$", doc, false, &rs, &errs)
		for _, e := range errs {
			t.Errorf("%s: %s", op.ID, e)
		}
	}
	var sb strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&sb, "%s\t%s\t%s\n", r.op, r.path, r.reason)
	}
	if err := os.MkdirAll(hostOutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hostOutDir, file)
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if !exists(path) {
		t.Fatalf("%s was not written", path)
	}
	t.Logf("%s: %d unavailable fields (op, json path, reason):\n%s", path, len(rs), sb.String())
	return rs
}

func hostWalk(op, path string, node any, nullItemsOK bool, rs *[]hostReason, errs *[]string) {
	switch n := node.(type) {
	case map[string]any:
		reasons := map[string]string{}
		if u, ok := n["unavailable"]; ok {
			m, isObj := u.(map[string]any)
			if !isObj {
				*errs = append(*errs, fmt.Sprintf("%s.unavailable is %v, not an object", path, u))
			}
			for k, r := range m {
				s, isStr := r.(string)
				if !isStr {
					*errs = append(*errs, fmt.Sprintf("%s.unavailable.%s is %v, not a string", path, k, r))
					continue
				}
				reasons[k] = s
			}
		}
		for _, k := range hostKeys(n) {
			if k != "unavailable" && n[k] == nil && reasons[k] == "" {
				*errs = append(*errs, fmt.Sprintf("%s.%s is null with no reason", path, k))
			}
		}
		for _, k := range hostKeys(reasons) {
			r := reasons[k]
			*rs = append(*rs, hostReason{op: op, path: path + "." + k, reason: r})
			if !slices.ContainsFunc(hostReasonPrefixes, func(p string) bool { return strings.HasPrefix(r, p) }) {
				*errs = append(*errs, fmt.Sprintf("%s.unavailable.%s: reason %q starts with no vocabulary prefix", path, k, r))
			}
			val, ok := n[k]
			l, isList := val.([]any)
			switch {
			case !ok:
				*errs = append(*errs, fmt.Sprintf("%s.unavailable names %s, which is not a field", path, k))
			case val == nil, isList && len(l) == 0, val == "unknown":
			default:
				*errs = append(*errs, fmt.Sprintf("%s.unavailable names %s (%q), which is populated: %v", path, k, r, val))
			}
		}
		for _, k := range hostKeys(n) {
			if k == "unavailable" {
				continue
			}
			i := strings.LastIndexByte(path, '[')
			series := i >= 0 && strings.HasSuffix(path, "]") && strings.HasSuffix(path[:i], ".series")
			hostWalk(op, path+"."+k, n[k], k == "values" && series, rs, errs)
		}
	case []any:
		for i, item := range n {
			p := path + "[" + strconv.Itoa(i) + "]"
			if item == nil {
				if !nullItemsOK {
					*errs = append(*errs, fmt.Sprintf("%s: a list item is null", p))
				}
				continue
			}
			hostWalk(op, p, item, false, rs, errs)
		}
	}
}

// hostJournal is nexusd.service's journal since since, in the default
// format, so systemd's own lines about the unit are in it too.
func hostJournal(t *testing.T, since time.Time) string {
	t.Helper()
	return mustSh(t, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10), "--no-pager")
}

// hostDenials are the journal lines of a sandbox fault TC-HP-116 names,
// with journalFaults' patterns (harness_test.go) besides.
var hostDenials = []string{"SIGSYS", "Bad system call", "seccomp", "Permission denied", "Operation not permitted"}

func hostJournalFaults(journal string) []string {
	faults := journalFaults(journal)
	for _, p := range hostDenials {
		for line := range strings.Lines(journal) {
			if strings.Contains(line, p) {
				faults = append(faults, fmt.Sprintf("journal (%s): %s", p, strings.TrimSpace(line)))
			}
		}
	}
	return faults
}

// hostLogLine reports whether journal has a line with every part.
func hostLogLine(journal string, parts ...string) bool {
	for line := range strings.Lines(journal) {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return true
		}
	}
	return false
}

// hostBurner starts a CPU burner, sh -c 'while :; do :; done', with argv
// prefix (setpriv for another user) and kills it at cleanup.
func hostBurner(t *testing.T, prefix ...string) int {
	t.Helper()
	argv := append(slices.Clone(prefix), "sh", "-c", "while :; do :; done")
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	t.Logf("burner %q PID %d", argv, cmd.Process.Pid)
	return cmd.Process.Pid
}

// hostWaitTop polls host.health.get for up to 3 intervals for pid in
// topByCpu with cpuPercent >= 50.
func hostWaitTop(t *testing.T, d *hostDaemon, pid int) hostspec.Process {
	t.Helper()
	var hit hostspec.Process
	start := time.Now()
	waitUntil(t, 3*hostInterval, fmt.Sprintf("PID %d in topByCpu at >= 50%%", pid), func() (bool, string) {
		h := hostHealth(t, d)
		var seen []string
		for _, p := range h.TopByCPU {
			c, _ := deref(p.CPUPercent)
			seen = append(seen, fmt.Sprintf("%d %s %.1f", p.PID, p.Command, c))
			if p.PID == pid && p.CPUPercent != nil && *p.CPUPercent >= 50 {
				hit = p
				return true, ""
			}
		}
		return false, strings.Join(seen, "; ")
	})
	c, _ := deref(hit.CPUPercent)
	u, _ := deref(hit.User)
	uid, _ := deref(hit.UID)
	t.Logf("topByCpu after %v: PID %d command %q uid %d user %q cpuPercent %.1f", time.Since(start).Round(time.Millisecond),
		hit.PID, hit.Command, uid, u, c)
	return hit
}

// TC-HP-114: Every op under the exact hardened unit agrees with the host's own tools (systemd). Owner WI 5.3. Requirements: NFR-5, NFR-3, FR-4, FR-5, FR-6, FR-7, FR-8, FR-9, CON-1, CON-5.
//
// - Setup: `requireInit(t, platform.Systemd)`; `freshHost`; `runInstall`; `writeConfig` with `providers.host: {enabled: true, sampleInterval: 2s}`; `startExact`; `exactNexusd(t, "host:read")`; wait until `host.summary.get`'s `sampler.state` is `ready` (`waitUntil`, 10 s).
// - Steps: call all ten ops over REST (lists with `limit=1000`); run `hostname`, `cat /etc/os-release`, `uname -r`, `uname -m`, `nproc`, `free -b`, `df -B1 --output=target,size,used,avail,pcent`, `lsblk -b -J -o NAME,SIZE,ROTA,RM,TYPE`, `ip -j addr`, `ip -j route`, `ip -j -6 route` immediately before and after.
// - Expected: hostname, os ID/VERSION_ID/PRETTY_NAME, kernel, arch, logicalCpus exact; `availableBytes` within ±1% of free's available and `totalBytes` exact; each listed filesystem's size equals df exactly and used within one block (or the before/after df bracket); each lsblk disk's size/rota/rm equal; each interface's addresses with prefixes equal `ip -j addr`; default gateways equal `ip -j route`; every op answered 200.
func TestTCHP114_EveryOpUnderTheExactHardenedUnit(t *testing.T) {
	d := hostSetupSystemd(t, "")
	hostCompareTools(t, d, false)
}

// TC-HP-115: Every field is populated or explicitly unavailable under the unit (systemd). Owner WI 5.3. Requirements: FR-12, NFR-5.
//
// - Setup: as TC-HP-114.
// - Steps: decode each response into `map[string]any`; walk it with the inline D-5 walker; collect every `(op, json path, reason)` from `unavailable` maps; write them to `build/harness/logs/<stamp>/host-unavailable-systemd.txt`.
// - Expected: no null without a reason; no `unavailable` key naming a populated field; every reason starts with a vocabulary prefix; the list file exists and is logged.
//
// The file is written to /harness/out in the container, which run.sh
// copies into build/harness/logs/<stamp>/.
func TestTCHP115_EveryFieldIsPopulatedOrExplicitlyUnavailable(t *testing.T) {
	d := hostSetupSystemd(t, "")
	hostUnavailable(t, hostCallAll(t, d), "host-unavailable-systemd.txt")
}

// TC-HP-116: No sandbox denial, crash or restart during the run (systemd). Owner WI 5.3. Requirements: CON-1, NFR-4, NFR-5.
//
// - Setup: as TC-HP-114, recording `since` and the kernel log line count before start.
// - Steps: after TC-HP-114's calls: `showUnit(nexusd, MainPID, NRestarts, ActiveState)`; `journalctl -u nexusd --since @<since>`; new kernel log lines.
// - Expected: MainPID equals `startExact`'s, `NRestarts 0`, `active`; the journal has none of `SIGSYS`, `Bad system call`, `seccomp`, `Permission denied`, `Operation not permitted`; no kernel `audit: type=1326` line for `nexusd`; the window is non-vacuous (it contains nexusd's start line and the sampler's info "sampler started" line).
func TestTCHP116_NoSandboxDenialCrashOrRestartDuring(t *testing.T) {
	d := hostSetupSystemd(t, "")
	hostCompareTools(t, d, false)
	// Two more rounds over more than one interval, so every sampler path
	// (process scan, rates, history) has run under the unit as well.
	for range 2 {
		time.Sleep(hostInterval)
		hostCallAll(t, d)
	}

	u := showUnit(t, nexusdUnit, "MainPID", "NRestarts", "ActiveState", "Result")
	journal := hostJournal(t, d.since)
	kjournal, _ := sh(30*time.Second, "journalctl", "-k", "--since", "@"+strconv.FormatInt(d.since.Unix(), 10), "--no-pager")
	newKernel := kernelLines(t)
	if len(newKernel) >= len(d.kernelBefore) {
		newKernel = newKernel[len(d.kernelBefore):]
	}
	t.Logf("nexusd.service %v; journal window %d lines; %d new kernel lines", u, strings.Count(journal, "\n"), len(newKernel))
	faults := hostJournalFaults(journal)
	for _, line := range append(strings.Split(kjournal, "\n"), newKernel...) {
		if strings.Contains(line, "audit: type=1326") && strings.Contains(line, `comm="nexusd"`) ||
			strings.Contains(line, "SECCOMP") && strings.Contains(line, "nexusd") {
			faults = append(faults, "kernel: "+strings.TrimSpace(line))
		}
	}
	if u["MainPID"] != strconv.Itoa(d.pid) || u["NRestarts"] != "0" || u["ActiveState"] != "active" {
		faults = append(faults, fmt.Sprintf("nexusd.service %v, started as MainPID %d", u, d.pid))
	}
	if len(faults) > 0 {
		t.Fatalf("sandbox denials:\n%s\n--- journal ---\n%s", strings.Join(faults, "\n"), journal)
	}
	if !strings.Contains(journal, `"msg":"nexusd ready"`) || !hostLogLine(journal, hostStartLog, hostLogAttr) {
		t.Fatalf("the window is vacuous: no nexusd ready or host sampler start line\n%s", journal)
	}
	for line := range strings.Lines(journal) {
		if strings.Contains(line, `"msg":"nexusd ready"`) || strings.Contains(line, hostStartLog) {
			t.Logf("window: %s", strings.TrimSpace(line))
		}
	}
}

// TC-HP-117: PID 1's mountinfo is readable under the unit and later host mounts appear. Owner WI 5.3. Requirements: FR-7.
//
// - Setup: as TC-HP-114.
// - Steps:
//  1. `host.filesystems.list`.
//  2. With nexusd still running, mount a loop-backed ext4 image (`truncate -s 32M /var/tmp/tc-hp.img`, `mkfs.ext4 -q`, `mount -o loop` at `/mnt/tc-hp`; cleanup `umount` and `rm`), list again, and run `df -B1 /mnt/tc-hp`. (A tmpfs is not used: FR-7 excludes tmpfs outside `/tmp`, `/run` and `/dev/shm`.)
//  3. The PrivateTmp case (A-13, D-9): mount a second loop-backed ext4 image at `/tmp/tc-hp-hidden` on the host (`mkdir -p`, `mount -o loop`; cleanup `umount`, `rmdir` and `rm`). List again, and run `df -B1 /tmp/tc-hp-hidden` on the host.
//  4. Log the evidence: the lines of nexusd's `/proc/<MainPID>/mountinfo` for `/`, `/tmp` and `/tmp/tc-hp-hidden` (mount ID, parent ID, `maj:min`, root), and the result of `nsenter -m -t <MainPID> stat -c '%F' /tmp/tc-hp-hidden`.
//  5. Finding 14's case: `nsenter -m -t <MainPID> mkdir -p /tmp/tc-hp-hidden` creates the path inside nexusd's private `/tmp` (cleanup `nsenter -m -t <MainPID> rmdir`). List again.
//
// - Expected:
//   - No item has `readOnly` in `unavailable`, and `/` has `readOnly false`.
//   - After step 2, `/mnt/tc-hp` is listed with `sizeBytes` equal to df's and `fsType ext4`. This is A-13: a host mount made after start is visible, and statfs reaches it.
//   - After step 3, the host's `df` reports numbers for `/tmp/tc-hp-hidden`, so the mount is real, and the host's `/proc/1/mountinfo` lists it, so the case is not vacuous. The provider lists `/tmp/tc-hp-hidden` with `fsType ext4`. Its `sizeBytes`, `usedBytes`, `availableBytes`, `usedPercent` and inode fields are null, with the reason `not visible: /tmp/tc-hp-hidden is hidden in nexusd's mount namespace`.
//   - Step 4's lines and stat result are logged with `t.Logf` and committed with the T1 logs as the live evidence of A-13's shape and A-38's rule. The stat result is expected to be ENOENT; the test logs any other result and does not fail on it. There is no device-number check.
//   - After step 5, the path exists in nexusd's namespace and `statfs` there would succeed on the private `/tmp`. The item is still `not visible:` with null usage, never the private `/tmp`'s numbers. So ENOENT is not the only safeguard (Finding 14).
//
// A-13 rests on the host's / being a shared mount, which systemd makes it
// at boot on a real host, but not in a container: mount_setup() marks /
// MS_SHARED only "if (detect_container() <= 0 && !skip_setup)"
// (systemd src/shared/mount-setup.c), and Docker's root is private. With a
// private /, the unit's namespace receives no later host mount, and the
// provider reports /mnt/tc-hp not visible (T1 run 20261115T060733Z:
// nexusd's mountinfo had no line at /mnt/tc-hp, and statfs there in its
// namespace reached the root overlay). So the test makes / rshared, as on
// a host, before nexusd starts, and puts the propagation back at cleanup.
// The hardened unit is not changed.
func TestTCHP117_PID1SMountinfoIsReadableUnder(t *testing.T) {
	requireInit(t, platform.Systemd)
	prop := strings.TrimSpace(mustSh(t, "findmnt", "-n", "-o", "PROPAGATION", "/"))
	t.Logf("harness / propagation %s; making it shared, as systemd does at boot on a host", prop)
	switch prop {
	case "private", "shared", "slave", "unbindable":
	default:
		t.Fatalf("findmnt / PROPAGATION %q", prop)
	}
	mustSh(t, "mount", "--make-rshared", "/")
	t.Cleanup(func() {
		if out, err := sh(time.Minute, "mount", "--make-r"+prop, "/"); err != nil {
			t.Errorf("mount --make-r%s /: %v %s", prop, err, out)
		}
	})
	d := hostSetupSystemd(t, "")
	hostMountEvidence(t, d.pid, "/")
	pid := strconv.Itoa(d.pid)

	// Step 1.
	fss := hostFilesystems(t, d)
	for mp, fs := range fss {
		if r, ok := fs.Unavailable["readOnly"]; ok {
			t.Errorf("%s: readOnly unavailable (%q): /proc/1/mountinfo is not readable under the unit", mp, r)
		}
	}
	if ro, ok := deref(fss["/"].ReadOnly); !ok || ro {
		t.Fatalf("/ readOnly %v (set %v), want false: %+v", ro, ok, fss["/"])
	}
	t.Logf("step 1: %d filesystems, readOnly read for each; / readOnly false; listed %q", len(fss), hostKeys(fss))

	// loopMount makes a 32 MiB ext4 image and mounts it at mp on the host,
	// undone at cleanup.
	loopMount := func(img, mp string) {
		t.Helper()
		t.Cleanup(func() { _ = os.Remove(img) })
		mustSh(t, "truncate", "-s", "32M", img)
		mustSh(t, "mkfs.ext4", "-q", "-F", img)
		if err := os.MkdirAll(mp, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(mp) })
		mustSh(t, "mount", "-o", "loop", img, mp)
		t.Cleanup(func() {
			if out, err := sh(time.Minute, "umount", mp); err != nil {
				t.Errorf("umount %s: %v %s", mp, err, out)
			}
		})
	}
	dfSize := func(mp string) uint64 {
		t.Helper()
		out := mustSh(t, "df", "-B1", "--output=size,used,avail,target", mp)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		f := strings.Fields(lines[len(lines)-1])
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil || f[len(f)-1] != mp {
			t.Fatalf("df -B1 %s: %q", mp, out)
		}
		t.Logf("host df -B1 %s: %s", mp, lines[len(lines)-1])
		return n
	}

	// Step 2: a host mount made after the start is visible and statfs
	// reaches it.
	const vis = "/mnt/tc-hp"
	loopMount("/var/tmp/tc-hp.img", vis)
	want := dfSize(vis)
	fss = hostFilesystems(t, d)
	got, ok := fss[vis]
	hostMountEvidence(t, d.pid, vis)
	if !ok {
		t.Fatalf("step 2: %s is not listed: %q", vis, hostKeys(fss))
	}
	if size, ok := deref(got.SizeBytes); !ok || size != want || got.FSType != "ext4" {
		t.Fatalf("step 2: %s %+v, want ext4 with sizeBytes %d (df)", vis, got, want)
	}
	t.Logf("step 2: %s listed, ext4, sizeBytes %d = df", vis, want)

	// Step 3: a host mount under /tmp, which PrivateTmp hides.
	const hidden = "/tmp/tc-hp-hidden"
	loopMount("/var/tmp/tc-hp-hidden.img", hidden)
	if size := dfSize(hidden); size == 0 {
		t.Fatalf("host df reports no size for %s", hidden)
	}
	pid1, err := os.ReadFile("/proc/1/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if !hostMountinfoHas(string(pid1), hidden) {
		t.Fatalf("the host's /proc/1/mountinfo does not list %s: the case is vacuous", hidden)
	}
	wantReason := "not visible: " + hidden + " is hidden in nexusd's mount namespace"
	checkHidden := func(step string) {
		t.Helper()
		fss := hostFilesystems(t, d)
		h, ok := fss[hidden]
		if !ok {
			t.Fatalf("%s: %s is not listed: %q", step, hidden, hostKeys(fss))
		}
		if h.FSType != "ext4" {
			t.Errorf("%s: %s fsType %q, want ext4", step, hidden, h.FSType)
		}
		nulls := map[string]bool{"sizeBytes": h.SizeBytes == nil, "usedBytes": h.UsedBytes == nil,
			"availableBytes": h.AvailableBytes == nil, "usedPercent": h.UsedPercent == nil, "inodesTotal": h.InodesTotal == nil,
			"inodesUsed": h.InodesUsed == nil, "inodesFree": h.InodesFree == nil}
		for _, f := range hostUsageFields {
			if !nulls[f] || h.Unavailable[f] != wantReason {
				t.Errorf("%s: %s %s null %v, reason %q, want null with %q", step, hidden, f, nulls[f], h.Unavailable[f], wantReason)
			}
		}
		b, _ := json.Marshal(h)
		t.Logf("%s: %s %s", step, hidden, b)
	}
	checkHidden("step 3")

	// Step 4: A-13's shape, from nexusd's own mount namespace.
	self, err := os.ReadFile("/proc/" + pid + "/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(self)) {
		f := strings.Fields(line)
		if len(f) >= 5 && (f[4] == "/" || f[4] == "/tmp" || f[4] == hidden) {
			t.Logf("step 4: /proc/%s/mountinfo: mount ID %s parent %s maj:min %s root %s mount point %s", pid, f[0], f[1], f[2],
				f[3], f[4])
		}
	}
	out, err := sh(30*time.Second, "nsenter", "-m", "-t", pid, "stat", "-c", "%F", hidden)
	t.Logf("step 4: nsenter -m -t %s stat -c %%F %s: %v %q (ENOENT expected)", pid, hidden, err, strings.TrimSpace(out))

	// Step 5: the path exists in nexusd's namespace, on its private /tmp.
	mustSh(t, "nsenter", "-m", "-t", pid, "mkdir", "-p", hidden)
	t.Cleanup(func() { _, _ = sh(30*time.Second, "nsenter", "-m", "-t", pid, "rmdir", hidden) })
	out = mustSh(t, "nsenter", "-m", "-t", pid, "stat", "-f", "-c", "%T %b blocks of %S", hidden)
	t.Logf("step 5: in nexusd's namespace %s now exists: statfs %s", hidden, strings.TrimSpace(out))
	checkHidden("step 5")
}

// hostMountEvidence logs the mountinfo lines at mp in the host's
// namespace (PID 1) and in nexusd's, with statfs of mp in nexusd's.
func hostMountEvidence(t *testing.T, pid int, mp string) {
	t.Helper()
	for _, p := range []string{"1", strconv.Itoa(pid)} {
		b, err := os.ReadFile("/proc/" + p + "/mountinfo")
		if err != nil {
			t.Logf("/proc/%s/mountinfo: %v", p, err)
			continue
		}
		n := 0
		for line := range strings.Lines(string(b)) {
			if f := strings.Fields(line); len(f) >= 5 && (f[4] == mp || f[4] == filepath.Dir(mp)) {
				n++
				t.Logf("/proc/%s/mountinfo: %s", p, strings.TrimSpace(line))
			}
		}
		if n == 0 {
			t.Logf("/proc/%s/mountinfo: no line at %s or %s", p, mp, filepath.Dir(mp))
		}
	}
	out, err := sh(30*time.Second, "nsenter", "-m", "-t", strconv.Itoa(pid), "stat", "-f", "-c", "%T %b blocks of %S", mp)
	t.Logf("nsenter -m -t %d stat -f %s: %v %q", pid, mp, err, strings.TrimSpace(out))
}

// hostMountinfoHas reports whether a mountinfo text has a mount at mp.
func hostMountinfoHas(mountinfo, mp string) bool {
	for line := range strings.Lines(mountinfo) {
		if f := strings.Fields(line); len(f) >= 5 && f[4] == mp {
			return true
		}
	}
	return false
}

// TC-HP-118: A failed transient unit appears in failedServices (systemd). Owner WI 5.3. Requirements: FR-10.
//
// - Setup: as TC-HP-114; `systemd-run --unit nx-host-failed /bin/false`; wait until `systemctl is-failed nx-host-failed` prints `failed`; cleanup `systemctl reset-failed nx-host-failed`.
// - Steps: `host.health.get`; `systemctl --failed --plain --no-legend`.
// - Expected: `failedServices.names` contains `nx-host-failed.service`; `count` equals the number of lines systemctl prints.
func TestTCHP118_AFailedTransientUnitAppearsInFailedServices(t *testing.T) {
	d := hostSetupSystemd(t, "")
	const unit = "nx-host-failed"
	t.Cleanup(func() { _, _ = sh(time.Minute, detach.Systemctl, "reset-failed", unit) })
	out, err := sh(time.Minute, "systemd-run", "--unit", unit, "/bin/false")
	t.Logf("systemd-run --unit %s /bin/false: %v %s", unit, err, strings.TrimSpace(out))
	waitUntil(t, 30*time.Second, unit+" failed", func() (bool, string) {
		out, _ := sh(10*time.Second, detach.Systemctl, "is-failed", unit)
		return strings.TrimSpace(out) == "failed", out
	})

	fs := hostHealth(t, d).FailedServices
	listed, _ := stdoutOf(time.Minute, detach.Systemctl, "--failed", "--plain", "--no-legend")
	var lines []string
	for line := range strings.Lines(listed) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	count, ok := deref(fs.Count)
	t.Logf("failedServices count %d names %q source %s; systemctl --failed: %q", count, fs.Names, fs.Source, lines)
	if !slices.Contains(fs.Names, unit+".service") {
		t.Errorf("failedServices.names %q lacks %s.service", fs.Names, unit)
	}
	if !ok || count != len(lines) {
		t.Errorf("failedServices.count %d (set %v), systemctl --failed prints %d lines", count, ok, len(lines))
	}
}

// TC-HP-119: The reboot-required marker drives rebootRequired (systemd, Ubuntu). Owner WI 5.3. Requirements: FR-10.
//
// - Setup: as TC-HP-114; save and restore any existing marker files in cleanup.
// - Steps: write `/var/run/reboot-required` and `.pkgs` = `linux-image-tc-hp`; `host.health.get`; remove both; `host.health.get`.
// - Expected: first `state "true"` with `packages ["linux-image-tc-hp"]`; second `"false"` if any `apt.RebootMarkerWriters` path exists on the harness (`test -e`), else `"unknown"`; the apt provider's capabilities `rebootRequired` (if enabled) agrees. (Encodes open item 1.)
//
// The apt provider is enabled here, so its agreement is checked.
func TestTCHP119_TheRebootRequiredMarkerDrivesRebootRequiredSystemd(t *testing.T) {
	d := hostSetupSystemd(t, "  apt:\n    enabled: true\n", aptspec.ReadScope)
	const marker, pkgs = "/var/run/reboot-required", "/var/run/reboot-required.pkgs"
	for _, p := range []string{marker, pkgs} {
		if b, err := os.ReadFile(p); err == nil {
			t.Cleanup(func() {
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Errorf("restore %s: %v", p, err)
				}
			})
		} else {
			t.Cleanup(func() { _ = os.Remove(p) })
		}
	}
	aptState := func() any {
		t.Helper()
		var caps apitypes.Capabilities
		d.mustGet("/v1/capabilities?refresh=true", &caps)
		for _, pc := range caps.Providers {
			if pc.Name == aptspec.Name {
				if pc.Status != apitypes.ProviderEnabled {
					t.Fatalf("apt %s: %s", pc.Status, pc.Reason)
				}
				return pc.Details[aptspec.DetailRebootRequired]
			}
		}
		t.Fatal("no apt provider in /v1/capabilities")
		return nil
	}

	if err := os.WriteFile(marker, []byte("*** System restart required ***\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkgs, []byte("linux-image-tc-hp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := hostHealth(t, d).RebootRequired
	a := aptState()
	t.Logf("marker present: rebootRequired %+v; apt capabilities rebootRequired %v", r, a)
	if r.State != hostspec.RebootTrue || !slices.Equal(r.Packages, []string{"linux-image-tc-hp"}) || a != r.State {
		t.Errorf("marker present: state %q packages %q, apt %v; want true, [linux-image-tc-hp], apt agreeing", r.State, r.Packages, a)
	}

	for _, p := range []string{marker, pkgs} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	want := hostspec.RebootUnknown
	for _, w := range apt.RebootMarkerWriters {
		if _, err := sh(10*time.Second, "test", "-e", w); err == nil {
			want = hostspec.RebootFalse
			t.Logf("marker writer present: %s", w)
		}
	}
	r = hostHealth(t, d).RebootRequired
	a = aptState()
	t.Logf("marker removed: rebootRequired %+v; apt capabilities rebootRequired %v; want %s", r, a, want)
	if r.State != want || a != r.State {
		t.Errorf("marker removed: state %q, apt %v, want %q for both", r.State, a, want)
	}
}

// TC-HP-120: A CPU burner appears in topByCpu (systemd). Owner WI 5.3. Requirements: FR-10, FR-11, NFR-3.
//
// - Setup: as TC-HP-114; start `sh -c 'while :; do :; done'` (cleanup kill).
// - Steps: poll `host.health.get` for up to 3 intervals (6 s).
// - Expected: an entry with the burner's PID, `command "sh"`, `user "root"`, `cpuPercent ≥ 50`; `ps -o pcpu= -p <pid>` (or `/proc/<pid>/stat` delta) agrees within ±20 percentage points.
func TestTCHP120_ACPUBurnerAppearsInTopByCpuSystemd(t *testing.T) {
	d := hostSetupSystemd(t, "")
	pid := hostBurner(t)
	p := hostWaitTop(t, d, pid)
	if u, _ := deref(p.User); p.Command != "sh" || u != "root" {
		t.Errorf("burner entry command %q user %q, want sh and root", p.Command, u)
	}
	out := strings.TrimSpace(mustSh(t, "ps", "-o", "pcpu=", "-p", strconv.Itoa(pid)))
	ps, err := strconv.ParseFloat(out, 64)
	if err != nil {
		t.Fatalf("ps -o pcpu=: %q", out)
	}
	c, _ := deref(p.CPUPercent)
	t.Logf("cpuPercent %.1f, ps -o pcpu= %.1f", c, ps)
	if math.Abs(c-ps) > 20 {
		t.Errorf("cpuPercent %.1f and ps %.1f differ by more than 20 points", c, ps)
	}
}

// TC-HP-121: Disable by reload stops the provider; re-enable starts empty and warming up (systemd). Owner WI 5.3. Requirements: FR-1, FR-11, FR-13, NFR-2.
//
// - Setup: as TC-HP-114 with ≥ 3 history points.
// - Steps:
//  1. `writeConfig` with `enabled: false`; `systemctl reload nexusd`; call each op.
//  2. `writeConfig` with `enabled: true`; reload; immediately call `host.metrics.get` and `host.cpu.get`; wait 2 intervals; call again.
//
// - Expected: 1: every op answers `capability-unavailable` and the journal shows "sampler stopped" within one interval of the reload. 2: first `points 0`, `sampler.state warming-up`, `usagePercent` null with `warming up`; then `ready` with ≥ 1 point. MainPID unchanged throughout.
//
// The sampler samples once at once when it starts (RunBackground), and
// that sample is the history's first point, so the first metrics call
// after the re-enable sees points 0 only if it wins that race. The test
// records the points it sees and asserts what the race cannot change: at
// most that one point, a history starting after the re-enable (no point
// of the earlier run survives) and the sampler warming up.
func TestTCHP121_DisableByReloadStopsTheProviderRe(t *testing.T) {
	d := hostSetupSystemd(t, "")
	metricsOp, cpuOp := hostspec.MetricsGet.Op(), hostspec.CPUGet.Op()
	metrics := func() hostspec.MetricsProperties {
		t.Helper()
		return hostOne[hostspec.MetricsProperties](t, metricsOp.ID, hostGet(t, d, metricsOp))
	}
	var m hostspec.MetricsProperties
	waitUntil(t, 5*hostInterval, ">= 3 history points", func() (bool, string) {
		m = metrics()
		return m.Points >= 3, strconv.Itoa(m.Points)
	})
	t.Logf("setup: %d history points since %v", m.Points, m.HistoryStart)

	// Step 1: disabled by a reload.
	writeConfig(t, hostConfig(false, ""))
	reload1 := time.Now()
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	summary := hostspec.SummaryGet.Op()
	waitUntil(t, 10*time.Second, "host ops capability-unavailable", func() (bool, string) {
		code, slug, _ := d.problem(hostOpPath(summary))
		return slug == problem.CapabilityUnavailable, fmt.Sprintf("%d %s", code, slug)
	})
	for _, op := range hostspec.Spec().Ops {
		code, slug, p := d.problem(hostOpPath(op))
		if slug != problem.CapabilityUnavailable {
			t.Errorf("disabled: %s answered %d %s, want capability-unavailable", op.ID, code, slug)
		} else {
			t.Logf("disabled: %-22s %d %s: %s", op.ID, code, slug, p.Detail)
		}
	}
	var stopped time.Time
	waitUntil(t, 10*time.Second, "the host sampler stop line in the journal", func() (bool, string) {
		j, _ := sh(30*time.Second, "journalctl", "-u", nexusdUnit, "--since", "@"+strconv.FormatInt(reload1.Unix()-1, 10),
			"--no-pager", "-o", "short-unix")
		for line := range strings.Lines(j) {
			if strings.Contains(line, hostStopLog) && strings.Contains(line, hostLogAttr) {
				f := strings.Fields(line)
				if s, err := strconv.ParseFloat(f[0], 64); err == nil {
					stopped = time.UnixMicro(int64(s * 1e6))
					t.Logf("step 1: %s", strings.TrimSpace(line))
					return true, ""
				}
			}
		}
		return false, j
	})
	if lag := stopped.Sub(reload1); lag > hostInterval {
		t.Errorf("the sampler stopped %v after the reload, more than one interval", lag)
	} else {
		t.Logf("step 1: the sampler stopped %v after the reload", lag.Round(time.Millisecond))
	}

	// Step 2: re-enabled by a reload. The first answer that is not
	// capability-unavailable and comes from a running sampler is the
	// immediate one.
	writeConfig(t, hostConfig(true, ""))
	reload2 := time.Now()
	mustSh(t, detach.Systemctl, "reload", nexusdUnit)
	var first hostspec.MetricsProperties
	var cpuFirst hostspec.CPUProperties
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, b := d.get(hostOpPath(metricsOp))
		if code == http.StatusOK {
			first = hostOne[hostspec.MetricsProperties](t, metricsOp.ID, b)
			if first.Sampler.State != hostspec.SamplerNotRunning {
				cpuFirst = hostOne[hostspec.CPUProperties](t, cpuOp.ID, hostGet(t, d, cpuOp))
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("host.metrics.get after the re-enable: %d %s", code, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	since := time.Since(reload2).Round(time.Millisecond)
	t.Logf("step 2 (%v after the reload): metrics points %d historyStart %v sampler %+v; cpu sampler %s usagePercent %v (%q)",
		since, first.Points, first.HistoryStart, first.Sampler, cpuFirst.Sampler.State, cpuFirst.UsagePercent,
		cpuFirst.Unavailable["usagePercent"])
	if first.Points > 1 || first.HistoryStart != nil && first.HistoryStart.Before(reload2.Add(-time.Second)) {
		t.Errorf("re-enabled history is not empty: points %d, historyStart %v before the reload at %v", first.Points,
			first.HistoryStart, reload2)
	}
	if first.Sampler.State != hostspec.SamplerWarmingUp || cpuFirst.Sampler.State != hostspec.SamplerWarmingUp {
		t.Errorf("sampler %s / %s, want warming-up", first.Sampler.State, cpuFirst.Sampler.State)
	}
	if cpuFirst.UsagePercent != nil || cpuFirst.Unavailable["usagePercent"] != "warming up" {
		t.Errorf("cpu usagePercent %v (%q), want null with warming up", cpuFirst.UsagePercent, cpuFirst.Unavailable["usagePercent"])
	}

	time.Sleep(2 * hostInterval)
	again := metrics()
	cpuAgain := hostOne[hostspec.CPUProperties](t, cpuOp.ID, hostGet(t, d, cpuOp))
	t.Logf("step 2, 2 intervals later: metrics points %d sampler %s; cpu %s usagePercent %v", again.Points, again.Sampler.State,
		cpuAgain.Sampler.State, cpuAgain.UsagePercent)
	if again.Sampler.State != hostspec.SamplerReady || again.Points < 1 || cpuAgain.UsagePercent == nil {
		t.Errorf("2 intervals after the re-enable: sampler %s, points %d, usagePercent %v; want ready, >= 1, set",
			again.Sampler.State, again.Points, cpuAgain.UsagePercent)
	}
	if u := showUnit(t, nexusdUnit, "MainPID", "NRestarts"); u["MainPID"] != strconv.Itoa(d.pid) || u["NRestarts"] != "0" {
		t.Errorf("nexusd.service %v, started as MainPID %d", u, d.pid)
	}
}

// TC-HP-122: Every op under OpenRC agrees with BusyBox and the host's files. Owner WI 5.3. Requirements: NFR-5, NFR-3, CON-5, FR-4, FR-5, FR-6, FR-7, FR-9.
//
// - Setup: `requireInit(t, platform.OpenRC)`; `freshHost`; `runInstall`; `writeConfig` (as TC-HP-114); `lib(t, "start-nexusd")`; `createToken(t, "tc-hp", "host:read")`.
// - Steps: call all ten ops; compare with `hostname`, `/etc/os-release`, `uname -r -m`, `nproc` (or `grep -c ^processor /proc/cpuinfo`), `free -b`, `df -B1` (or `df -k` × 1024), `ip -o addr`, `ip route`, `ip -6 route`.
// - Expected: the same comparisons and tolerances as TC-HP-114 (no lsblk; disk sizes against `/sys/block/*/size` × 512 read by the test); the inline walker of TC-HP-115 passes and writes `host-unavailable-openrc.txt`; `rc-service nexusd status` stays `started` with the same PID (`openrcMainPID`).
//
// The harness image's iproute2 answers ip -j, the same tool as ip -o.
func TestTCHP122_EveryOpUnderOpenRCAgreesWithBusyBox(t *testing.T) {
	d := hostSetupOpenRC(t)
	bodies := hostCompareTools(t, d, true)
	hostUnavailable(t, bodies, "host-unavailable-openrc.txt")
	if st, pid := rcStatus(t), openrcMainPID(t); st != "started" || pid != d.pid {
		t.Errorf("rc-service nexusd status %s PID %d, started as PID %d", st, pid, d.pid)
	}
}

// TC-HP-123: A crashed OpenRC service appears in failedServices. Owner WI 5.3. Requirements: FR-10, CON-5.
//
// - Setup: TC-HP-122's daemon; write `/etc/init.d/nx-host-crash` (`command=/bin/sleep`, `command_args=3600`, `command_background=yes`, `pidfile=/run/nx-host-crash.pid`); `rc-service nx-host-crash start`; `kill -9` its pid; cleanup `rc-service nx-host-crash zap` and remove the script.
// - Steps: `rc-status --crashed`; `host.health.get`.
// - Expected: `rc-status --crashed` lists `nx-host-crash`; `failedServices.names` contains `nx-host-crash`; `count` equals rc-status's line count; `/bin/rc-status` exists (A-17).
func TestTCHP123_ACrashedOpenRCServiceAppearsInFailedServices(t *testing.T) {
	d := hostSetupOpenRC(t)
	const svc, script, pidfile = "nx-host-crash", "/etc/init.d/nx-host-crash", "/run/nx-host-crash.pid"
	body := "#!/sbin/openrc-run\ncommand=/bin/sleep\ncommand_args=3600\ncommand_background=yes\npidfile=" + pidfile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = sh(time.Minute, "rc-service", svc, "zap")
		_ = os.Remove(script)
		_ = os.Remove(pidfile)
	})
	mustSh(t, "rc-service", svc, "start")
	pid := waitPIDFile(t, pidfile)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var lines []string
	waitUntil(t, 10*time.Second, "rc-status --crashed lists "+svc, func() (bool, string) {
		out, _ := stdoutOf(30*time.Second, "rc-status", "--crashed")
		lines = nil
		for line := range strings.Lines(out) {
			if s := strings.TrimSpace(line); s != "" {
				lines = append(lines, s)
			}
		}
		return slices.Contains(lines, svc), out
	})
	if !exists("/bin/rc-status") {
		t.Error("/bin/rc-status does not exist (A-17)")
	}
	fs := hostHealth(t, d).FailedServices
	count, ok := deref(fs.Count)
	t.Logf("killed %s PID %d; rc-status --crashed %q; failedServices count %d names %q source %s", svc, pid, lines, count,
		fs.Names, fs.Source)
	if !slices.Contains(fs.Names, svc) {
		t.Errorf("failedServices.names %q lacks %s", fs.Names, svc)
	}
	if !ok || count != len(lines) {
		t.Errorf("failedServices.count %d (set %v), rc-status --crashed prints %d lines", count, ok, len(lines))
	}
}

// TC-HP-124: In the Alpine container, rebootRequired is unknown and virtualization is a container. Owner WI 5.3. Requirements: FR-10, FR-5.
//
// - Setup: TC-HP-122's daemon.
// - Steps: `host.health.get`; `host.identity.get`.
// - Expected: `rebootRequired.state "unknown"` with container evidence; `virtualization.kind "container"` and `type` naming the marker found (`docker` when `/.dockerenv` exists, checked by the test).
func TestTCHP124_InTheAlpineContainerRebootRequiredIsUnknown(t *testing.T) {
	d := hostSetupOpenRC(t)
	r := hostHealth(t, d).RebootRequired
	v := hostIdentity(t, d).Virtualization
	ev, _ := deref(v.Evidence)
	dockerenv := exists("/.dockerenv")
	t.Logf("rebootRequired %+v; virtualization type %s kind %s evidence %q; /.dockerenv exists %v", r, v.Type, v.Kind, ev, dockerenv)
	if r.State != hostspec.RebootUnknown || !strings.Contains(r.Evidence, "container") ||
		!strings.HasPrefix(r.Unavailable["state"], "not supported: ") {
		t.Errorf("rebootRequired %+v, want unknown with container evidence and a not supported: reason", r)
	}
	if v.Kind != "container" {
		t.Errorf("virtualization kind %q, want container", v.Kind)
	}
	if dockerenv && v.Type != "docker" {
		t.Errorf("/.dockerenv exists but virtualization type is %q, want docker", v.Type)
	}
	if !dockerenv {
		t.Errorf("/.dockerenv is absent in the Docker harness: the type check is vacuous (type %q)", v.Type)
	}
}

// TC-HP-125: TC-182 sweeps the ten host ops under the exact unit. Owner WI 5.3. Requirements: NFR-5, CON-1, FR-2.
//
// - Setup: TC-182 with `host: {enabled: true, sampleInterval: 2s}` added to its config and `host:read` to its token scopes.
// - Steps: run TC-182.
// - Expected: the count of exercised ops equals the catalog's op count including the ten host ops; each host op answered 200.
//
// TC-182 runs once per process: when this test has run it, the later
// TestEveryProviderOpUnderExactUnit reports this run's result (tc182).
func TestTCHP125_TC182SweepsTheTenHostOps(t *testing.T) {
	TestEveryProviderOpUnderExactUnit(t)
	if t.Failed() || t.Skipped() {
		return
	}
	var want []string
	for _, op := range hostspec.Spec().Ops {
		want = append(want, op.ID)
	}
	t.Logf("TC-182 (run by %s): %d of %d catalog ops exercised; host ops answered 200: %q", tc182.by, tc182.swept,
		tc182.catalog, tc182.hostOps)
	if !slices.Equal(tc182.hostOps, want) {
		t.Errorf("TC-182's host ops %q, want the ten %q", tc182.hostOps, want)
	}
	if tc182.catalog == 0 || tc182.swept != tc182.catalog {
		t.Errorf("TC-182 exercised %d ops, the catalog has %d", tc182.swept, tc182.catalog)
	}
}

// TC-HP-126: Identity reports the harness's init system and OS on both targets. Owner WI 5.3. Requirements: CON-5, FR-5.
//
// - Setup: either target's daemon (no `requireInit`; runs on both).
// - Steps: `host.identity.get`; `detectedInit()`; parse `/etc/os-release`.
// - Expected: `initSystem` = `detectedInit()`; `os.id` = os-release ID; the same values as `GET /v1/capabilities`.
func TestTCHP126_IdentityReportsTheHarnessSInitSystem(t *testing.T) {
	d := hostSetup(t)
	id := hostIdentity(t, d)
	osr := osRelease(t)
	var caps apitypes.Capabilities
	d.mustGet("/v1/capabilities", &caps)
	osID, _ := deref(id.OS.ID)
	ver, _ := deref(id.OS.VersionID)
	t.Logf("identity initSystem %s os.id %s versionId %s; detectedInit %s; os-release ID %s; capabilities initSystem %s os %+v",
		id.InitSystem, osID, ver, detectedInit(), osr["ID"], caps.InitSystem, caps.OS)
	if id.InitSystem != string(detectedInit()) || caps.InitSystem != id.InitSystem {
		t.Errorf("initSystem %q, detectedInit %q, capabilities %q", id.InitSystem, detectedInit(), caps.InitSystem)
	}
	if osID != osr["ID"] || caps.OS.ID != osID || caps.OS.VersionID != ver {
		t.Errorf("os.id %q versionId %q, os-release ID %q, capabilities %+v", osID, ver, osr["ID"], caps.OS)
	}
}

// TC-HP-131: The documented sandbox-unavailable list matches what T1 observed. Owner WI 5.4. Requirements: NFR-5, FR-12, NFR-7.
//
// - Setup: TC-HP-115's collected list; `/harness/src/docs/host.md`.
// - Steps: extract the "Unavailable under the systemd sandbox" section; for each observed `(field, reason prefix)` caused by the sandbox (`permission denied:` or `command failed:` reasons), look it up.
// - Expected: every observed sandbox-caused field is listed with its cause; the section no longer says "to be confirmed by WI 5.4".
//
// run.sh does not copy docs/ to /harness/src; the release tree it installs
// from does carry them (make nexusd-tree copies docs/), built from the same
// checkout, so host.md is read from /harness/src/docs when present and from
// /harness/release/nexusd/docs otherwise. The check runs both ways: a field
// observed unavailable because of the sandbox must be in the section with
// its reason, and the section may say that no field is only when none was
// observed.
func TestTCHP131_TheDocumentedSandboxUnavailableListMatchesWhat(t *testing.T) {
	d := hostSetupSystemd(t, "")
	observed := hostUnavailable(t, hostCallAll(t, d), "host-unavailable-systemd.txt")

	var doc []byte
	var docPath string
	for _, p := range []string{"/harness/src/docs/host.md", filepath.Join(releaseTree, "docs", "host.md")} {
		b, err := os.ReadFile(p)
		if err == nil {
			doc, docPath = b, p
			break
		}
	}
	if doc == nil {
		t.Fatal("docs/host.md is in neither /harness/src/docs nor the release tree's docs")
	}
	const heading = "## Unavailable under the systemd sandbox"
	text := string(doc)
	i := strings.Index(text, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("%s has no %q section", docPath, heading)
	}
	sec := text[i+len(heading)+2:]
	if j := strings.Index(sec, "\n## "); j >= 0 {
		sec = sec[:j]
	}
	t.Logf("%s, %q (%d bytes):\n%s", docPath, heading, len(sec), sec)
	flat := strings.Join(strings.Fields(sec), " ")
	if strings.Contains(flat, "to be confirmed by WI 5.4") {
		t.Error("the section still says it is to be confirmed by WI 5.4")
	}

	const noneObserved = "no field is unavailable because of the sandbox"
	var sandbox []hostReason
	for _, r := range observed {
		if strings.HasPrefix(r.reason, "permission denied: ") || strings.HasPrefix(r.reason, "command failed: ") {
			sandbox = append(sandbox, r)
		}
	}
	t.Logf("%d unavailable fields observed, %d with permission denied: or command failed:", len(observed), len(sandbox))
	if len(sandbox) == 0 && !strings.Contains(flat, noneObserved) {
		t.Errorf("no sandbox-caused field was observed, but the section does not say %q", noneObserved)
	}
	if len(sandbox) > 0 && strings.Contains(flat, noneObserved) {
		t.Errorf("the section says %q, but T1 observed %d: %v", noneObserved, len(sandbox), sandbox)
	}
	for _, r := range sandbox {
		field := r.path[strings.LastIndexByte(r.path, '.')+1:]
		prefix := r.reason[:strings.Index(r.reason, ":")+1]
		found := false
		for line := range strings.Lines(sec) {
			if strings.HasPrefix(line, "|") && strings.Contains(line, "`"+field+"`") && strings.Contains(line, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s %s (%s) is not a row of the section naming `%s` with %q and its cause", r.op, r.path, r.reason,
				field, prefix)
		}
	}
}

// TC-HP-E28: Processes of other users are sampled under the unit (systemd). Owner WI 5.3. Requirements: FR-10, ASM-4.
//
// - Setup: TC-HP-114's daemon; a burner as `nobody` (`setpriv --reuid=65534 --regid=65534 --clear-groups sh -c 'while :; do :; done'`).
// - Steps: poll `host.health.get` for 3 intervals.
// - Expected: the burner is in `topByCpu` with `uid 65534`, `user "nobody"`, `cpuPercent ≥ 50` (A-28: no hidepid; `/proc/<pid>/status` readable).
func TestTCHPE28_ProcessesOfOtherUsersAreSampledUnder(t *testing.T) {
	d := hostSetupSystemd(t, "")
	pid := hostBurner(t, "setpriv", "--reuid=65534", "--regid=65534", "--clear-groups")
	t.Logf("burner /proc/%d/status Uid: %s", pid, procStatusField(pid, "Uid"))
	p := hostWaitTop(t, d, pid)
	uid, okU := deref(p.UID)
	u, _ := deref(p.User)
	if !okU || uid != 65534 || u != "nobody" {
		t.Errorf("burner entry uid %d (set %v) user %q, want 65534 and nobody", uid, okU, u)
	}
}

// TC-HP-E29: Clock synchronization under the unit matches timedatectl or is explained. Owner WI 5.3. Requirements: FR-5, CON-1.
//
// - Setup: either target's daemon.
// - Steps: `host.identity.get`; on systemd, `timedatectl show -p NTPSynchronized --value` run by the test.
// - Expected: systemd: `clock.synchronized` equals `"true"`/`"false"` for `yes`/`no`, or is unavailable with a `command failed:`/`timed out` reason (then listed in host.md, TC-HP-131); OpenRC without chrony: `"unknown"` with `not supported:`; no SIGSYS in either case (TC-HP-116's scan covers the run).
func TestTCHPE29_ClockSynchronizationUnderTheUnitMatchesTimedatectl(t *testing.T) {
	d := hostSetup(t)
	c := hostIdentity(t, d).Clock
	src, _ := deref(c.Source)
	why := c.Unavailable["synchronized"]
	switch detectedInit() {
	case platform.Systemd:
		out, err := stdoutOf(30*time.Second, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
		ntp := strings.TrimSpace(out)
		t.Logf("clock synchronized %q source %q reason %q; timedatectl NTPSynchronized %q (%v)", c.Synchronized, src, why, ntp, err)
		want := map[string]string{"yes": "true", "no": "false"}[ntp]
		explained := c.Synchronized == "unknown" &&
			(strings.HasPrefix(why, "command failed: ") || strings.HasPrefix(why, "timed out after "))
		if !(want != "" && c.Synchronized == want) && !explained {
			t.Errorf("clock.synchronized %q (%q), timedatectl %q: neither equal nor explained", c.Synchronized, why, ntp)
		}
		j := hostJournal(t, d.since)
		for _, p := range []string{"SIGSYS", "Bad system call", "status=31/SYS"} {
			if strings.Contains(j, p) {
				t.Errorf("the journal has %q:\n%s", p, j)
			}
		}
		if u := showUnit(t, nexusdUnit, "NRestarts", "MainPID"); u["NRestarts"] != "0" || u["MainPID"] != strconv.Itoa(d.pid) {
			t.Errorf("nexusd.service %v, started as %d", u, d.pid)
		}
	case platform.OpenRC:
		chrony := exists("/usr/bin/chronyc")
		t.Logf("clock synchronized %q source %q reason %q; /usr/bin/chronyc exists %v", c.Synchronized, src, why, chrony)
		if chrony {
			t.Fatalf("/usr/bin/chronyc exists: this case is OpenRC without chrony")
		}
		if c.Synchronized != "unknown" || !strings.HasPrefix(why, "not supported: ") {
			t.Errorf("clock.synchronized %q (%q), want unknown with not supported:", c.Synchronized, why)
		}
		if st, pid := rcStatus(t), openrcMainPID(t); st != "started" || pid != d.pid {
			t.Errorf("rc-service nexusd %s PID %d, started as %d", st, pid, d.pid)
		}
	}
}
