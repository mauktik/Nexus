// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/svcctl"
)

// newSource is newSourceWith for a fixed sourceTimeout.
func newSource(fsys fs.FS, timeout time.Duration, statfs func(string) (Usage, error)) *source {
	return newSourceWith(fsys, func() time.Duration { return timeout }, statfs)
}

// inFlight reports whether bounded has a call in flight for key.
func (s *source) inFlight(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.inflight[key]
	return ok
}

// The WI 2.2 access-kind matrix: each allowed or refused access, and
// every refusal reaches no underlying fs.FS call.
func TestAllowListAccessKinds(t *testing.T) {
	type op func(src *source, name string) error
	read := func(src *source, n string) error { _, err := src.read(n); return err }
	list := func(src *source, n string) error { _, err := src.readDir(n); return err }
	stat := func(src *source, n string) error { _, err := src.stat(n); return err }
	link := func(src *source, n string) error { _, err := src.readLink(n); return err }
	cases := []struct {
		op      string
		do      op
		name    string
		allowed bool
	}{
		{"read", read, "sys/class/dmi/id/product_serial", false},
		{"read", read, "sys/class/dmi/id/product_uuid", false},
		{"read", read, "proc/1/cmdline", false},
		{"read", read, "proc/1/environ", false},
		{"read", read, "../etc/shadow", false},
		{"list", list, "etc", false},
		{"stat", stat, "etc/shadow", false},
		{"read", read, "usr/bin/chronyc", false},
		{"read", read, "proc", false},
		{"link", link, "etc/passwd", false},
		{"read", read, "proc/self/stat", false},
		{"read", read, "sys/devices/system/cpu/cpufreq/scaling_cur_freq", false},
		{"read", read, "sys/devices/system/cpu/cpuX/topology/core_id", false},
		// SEC-2: a loop device's backing file is existence-only, and the
		// mount table comes from mountinfo, so proc/mounts has no grant.
		{"read", read, "sys/block/loop0/loop/backing_file", false},
		{"read", read, "proc/mounts", false},
		{"stat", stat, "sys/block/loop0/loop/backing_file", true},
		{"read", read, "proc/stat", true},
		{"read", read, "etc/localtime", true},
		{"read", read, "proc/1/stat", true},
		{"read", read, "sys/devices/system/cpu/cpu3/topology/core_id", true},
		{"stat", stat, "usr/bin/chronyc", true},
		{"stat", stat, "proc/stat", true},
		{"stat", stat, "sys/block/vda", true},
		{"list", list, "proc", true},
		{"list", list, "sys/block/vda", true},
		{"link", link, "etc/localtime", true},
	}
	for _, c := range cases {
		ffs := fixtureFS(t, capUBU)
		src := newSource(ffs, 250*time.Millisecond, nil)
		err := c.do(src, c.name)
		if refused := errors.Is(err, errNotAllowed); refused == c.allowed {
			t.Errorf("%s %q: err = %v, want allowed=%v", c.op, c.name, err, c.allowed)
		}
		calls := ffs.recorded()
		if !c.allowed && len(calls) != 0 {
			t.Errorf("%s %q: refused, but F-FS recorded %v", c.op, c.name, calls)
		}
		if c.allowed && len(calls) != 1 {
			t.Errorf("%s %q: allowed, F-FS recorded %v, want one call", c.op, c.name, calls)
		}
	}
	if target, err := newSource(fixtureFS(t, capUBU), time.Second, nil).readLink("etc/localtime"); err != nil || target != "/usr/share/zoneinfo/Etc/UTC" {
		t.Errorf("readLink etc/localtime = %q, %v; want tools/readlink-localtime's target", target, err)
	}
}

// Every list and stat-only entry that a later WI uses resolves: a capture
// instance passes the allow-list and reaches F-FS, or, where no capture has
// one (sensors, clock sources, reboot markers), a synthetic instance is
// permitted with that kind.
func TestAllowListListAndStatOnlyEntriesResolve(t *testing.T) {
	examples := map[string]string{"*": "x0", "<n>": "7"}
	for _, e := range allowList {
		if e.kinds&(accList|accStatOnly) == 0 {
			continue
		}
		pattern := strings.Join(e.segs, "/")
		kind := accList
		if e.kinds&accStatOnly != 0 {
			kind = accStatOnly
		}
		resolved := false
		for _, c := range hostCaptures {
			ffs := fixtureFS(t, c.dir)
			src := newSource(ffs, time.Second, nil)
			_ = fs.WalkDir(ffs, ".", func(name string, d fs.DirEntry, err error) error {
				if err != nil || name == "." || !matchSegs(e.segs, strings.Split(name, "/")) {
					return nil
				}
				var aerr error
				if kind == accList {
					_, aerr = src.readDir(name)
				} else {
					_, aerr = src.stat(name)
				}
				if aerr != nil {
					t.Errorf("%s: %s %s: %v", c.dir, kind, name, aerr)
				}
				resolved = true
				return nil
			})
		}
		if !resolved {
			var segs []string
			for _, s := range e.segs {
				for w, ex := range examples {
					s = strings.Replace(s, w, ex, 1)
				}
				segs = append(segs, s)
			}
			if name := strings.Join(segs, "/"); !permits(name, kind) {
				t.Errorf("%s: no capture instance, and %q is not permitted as %s", pattern, name, kind)
			}
		}
	}
}

// The host constants mirror apt's (architecture WI 4.4).
func TestRebootMarkersMirrorApt(t *testing.T) {
	var want []string
	for _, w := range apt.RebootMarkerWriters {
		want = append(want, strings.TrimPrefix(w, "/"))
	}
	if !slices.Equal(rebootMarkerWriters, want) {
		t.Errorf("rebootMarkerWriters = %q, want apt.RebootMarkerWriters %q", rebootMarkerWriters, want)
	}
	for _, w := range rebootMarkerWriters {
		if !permits(w, accStatOnly) || permits(w, accRead) {
			t.Errorf("%s: want stat-only", w)
		}
	}
	if m := strings.TrimPrefix(apt.RebootMarker, "/"); !permits(m, accStatOnly) || permits(m, accRead) {
		t.Errorf("%s: want stat-only", m)
	}
	if p := strings.TrimPrefix(apt.RebootPackagesFile, "/"); !permits(p, accRead) {
		t.Errorf("%s: want read", p)
	}
}

// Generic bounded, stuck branch: timed out within timeout+100ms, then
// still pending at once with one seam call and at most one goroutine, then
// a fresh call after release.
func TestBoundedStuckSource(t *testing.T) {
	src := newSource(fstest.MapFS{}, 200*time.Millisecond, nil)
	release := make(chan struct{})
	var calls atomic.Int32
	seam := func() (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}
	baseline := runtime.NumGoroutine()
	start := time.Now()
	_, err := bounded(t.Context(), src, "k", seam)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("first call took %v, want at most timeout+100ms", el)
	}
	if reasonFor("k", err) != "timed out after 200ms: k" {
		t.Errorf("first call: %v", err)
	}
	for i := 2; i <= 5; i++ {
		s := time.Now()
		_, err := bounded(t.Context(), src, "k", seam)
		if reasonFor("k", err) != "still pending: k" || time.Since(s) > 10*time.Millisecond {
			t.Errorf("call %d: %v after %v, want still pending at once", i, err, time.Since(s))
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("seam saw %d calls, want 1", n)
	}
	if n := runtime.NumGoroutine(); n > baseline+1 {
		t.Errorf("goroutines grew from %d to %d, want at most 1", baseline, n)
	}
	close(release)
	waitGoroutines(t, baseline)
	if src.inFlight("k") {
		t.Error("key still in flight after release")
	}
	if v, err := bounded(t.Context(), src, "k", seam); v != 7 || err != nil || calls.Load() != 2 {
		t.Errorf("after release: %d, %v, %d calls; want 7 from a fresh call", v, err, calls.Load())
	}
}

// Generic bounded, join branch: two concurrent calls on a 50 ms seam share
// one call's value.
func TestBoundedJoinsHealthyCall(t *testing.T) {
	src := newSource(fstest.MapFS{}, 250*time.Millisecond, nil)
	var calls atomic.Int32
	seam := func() (string, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return "v", nil
	}
	var wg sync.WaitGroup
	got := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { got[i], errs[i] = bounded(t.Context(), src, "j", seam) })
	}
	wg.Wait()
	for i := range 2 {
		if got[i] != "v" || errs[i] != nil {
			t.Errorf("call %d = %q, %v", i, got[i], errs[i])
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("seam saw %d calls, want 1", n)
	}
}

// A panic inside a bounded call reaches the caller, so the request's or
// tick's recovery handles it, and the key is cleared.
func TestBoundedPanicReachesCaller(t *testing.T) {
	src := newSource(fstest.MapFS{}, time.Second, nil)
	func() {
		defer func() {
			r := recover()
			p, ok := r.(*boundedPanic)
			if !ok || p.value != "boom" || !strings.Contains(p.Error(), "panic in bounded call p: boom") {
				t.Errorf("recovered %v, want the bounded panic carrying boom", r)
			}
		}()
		_, _ = bounded(t.Context(), src, "p", func() (int, error) { panic("boom") })
		t.Error("bounded returned instead of panicking")
	}()
	if src.inFlight("p") {
		t.Error("key still in flight after the panic")
	}
	if v, err := bounded(t.Context(), src, "p", func() (int, error) { return 1, nil }); v != 1 || err != nil {
		t.Errorf("after the panic: %d, %v", v, err)
	}
}

// A caller whose context ends stops waiting at once, as a timeout naming
// its key, without cancelling the call others may join.
func TestBoundedCallerContextDone(t *testing.T) {
	src := newSource(fstest.MapFS{}, time.Second, nil)
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := bounded(ctx, src, "c", func() (int, error) { <-release; return 0, nil })
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(reasonFor("c", err), "timed out after ") {
		t.Errorf("err = %v (reason %q), want a timeout wrapping context.Canceled", err, reasonFor("c", err))
	}
	if !src.inFlight("c") {
		t.Error("the call was dropped from the table while still running")
	}
}

// Review Finding 7: svcctl's not-started shape gives not present for a
// missing binary and command failed otherwise.
func TestReasonForCommandStart(t *testing.T) {
	notStarted := func(cause error) error {
		return fmt.Errorf("%w: %w", svcctl.ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: "/usr/bin/chronyc", Err: cause})
	}
	if r := reasonFor("/usr/bin/chronyc", notStarted(syscall.ENOENT)); r != "not present: /usr/bin/chronyc" {
		t.Errorf("ENOENT: %q", r)
	}
	if r := reasonFor("/usr/bin/chronyc", notStarted(syscall.EACCES)); r != "command failed: /usr/bin/chronyc not started" {
		t.Errorf("EACCES: %q", r)
	}
	if r := reasonFor("/bin/rc-status", fmt.Errorf("%w: refusing to run", svcctl.ErrNotStarted)); r != "command failed: /bin/rc-status not started" {
		t.Errorf("other: %q", r)
	}
	if r := reasonCommandExit("/usr/bin/timedatectl", 1); r != "command failed: /usr/bin/timedatectl exit 1" {
		t.Errorf("exit: %q", r)
	}
	if r := reasonStale(95 * time.Second); r != "stale: last sample 95s old" {
		t.Errorf("stale: %q", r)
	}
	if r := reasonNotVisible("/tmp/x"); r != "not visible: /tmp/x is hidden in nexusd's mount namespace" {
		t.Errorf("not visible: %q", r)
	}
	if r := reasonFor("x", nil); r != "" {
		t.Errorf("nil error: %q", r)
	}
}

// F-FS fidelity: the shaping the test strategy requires of it.
func TestFakeFSFidelity(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	if fi, err := ffs.Stat("proc/stat"); err != nil || fi.Size() != 0 {
		t.Errorf("proc/stat size: %v, %v; want 0", fi, err)
	}
	if fi, err := ffs.Stat("sys/class/net/eth0/mtu"); err != nil || fi.Size() != 4096 {
		t.Errorf("sysfs attribute size: %v, %v; want 4096", fi, err)
	}
	f, err := ffs.Open("sys/class/net/lo/speed")
	if err != nil {
		t.Fatalf("open lo/speed: %v", err)
	}
	if _, err := f.Read(make([]byte, 8)); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("read lo/speed: %v, want EINVAL (UNREADABLE)", err)
	}
	for _, dir := range []string{"sys/class/hwmon", "sys/class/thermal"} {
		if entries, err := ffs.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("placeholder %s: %v, %v; want an empty directory", dir, entries, err)
		}
	}
	if _, err := ffs.Stat("etc/shadow"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat etc/shadow: %v, want ErrNotExist", err)
	}
	ffs.deny("proc/pressure/*")
	if _, err := ffs.Open("proc/pressure/io"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("denied open: %v, want ErrPermission", err)
	}
	before := string(ffs.rawRead("proc/stat"))
	ffs.advance()
	if after := string(ffs.rawRead("proc/stat")); after == before {
		t.Error("advance did not switch proc/stat to t1/")
	}
	if _, err := ffs.Stat("proc/453/stat"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pid 453 exited by t1/: stat err %v, want ErrNotExist", err)
	}
	if entries, err := ffs.ReadDir("proc"); err != nil || slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == "453" }) {
		t.Errorf("proc listing after advance still holds pid 453 (%v)", err)
	}
}

// F-STATFS answers from the capture: counts from statfs.txt, IsDir from
// stat-mounts.txt, captured errnos from UNREADABLE, ENOENT otherwise.
func TestFakeStatfsFidelity(t *testing.T) {
	st := newFakeStatfs(t, capUBU)
	if u, err := st.Statfs("/"); err != nil || !u.IsDir || u.Blocks != 15402443 || u.Frsize != 4096 || u.Files != 3932160 {
		t.Errorf("/ = %+v, %v; want statfs.txt's root row", u, err)
	}
	if _, err := st.Statfs("/proc/sys/fs/binfmt_misc"); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("binfmt_misc: %v, want ELOOP from UNREADABLE", err)
	}
	if _, err := st.Statfs("/nowhere"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unknown mount: %v, want ErrNotExist", err)
	}
	if st.count("/") != 1 {
		t.Errorf("count = %d", st.count("/"))
	}
}

// addMount writes IDs and parents that fit each view (test-strategy.md,
// Mount views), never sharing self IDs with proc/1.
func TestFakeFSAddMountViews(t *testing.T) {
	entryAt := func(es []miEntry, mp string) []miEntry {
		var out []miEntry
		for _, e := range es {
			if e.mountPoint() == mp {
				out = append(out, e)
			}
		}
		return out
	}
	byID := func(es []miEntry, id int) (miEntry, bool) {
		for _, e := range es {
			if e.id == id {
				return e, true
			}
		}
		return miEntry{}, false
	}

	t.Run("both", func(t *testing.T) {
		ffs := fixtureFS(t, capUBU)
		ffs.addMount(t, `0 0 8:65 / /mnt/my\040disk rw,relatime shared:1 - ext4 /dev/sde1 rw`, viewBoth)
		host, self := ffs.mountinfo(t, "proc/1/mountinfo"), ffs.mountinfo(t, "proc/self/mountinfo")
		h, s := entryAt(host, "/mnt/my disk"), entryAt(self, "/mnt/my disk")
		if len(h) != 1 || len(s) != 1 || h[0].majmin != "8:65" || s[0].majmin != "8:65" {
			t.Fatalf("host %v, self %v", h, s)
		}
		if p, ok := byID(self, s[0].parent); !ok || p.mountPoint() != "/" {
			t.Errorf("self parent %d is not the reachable /", s[0].parent)
		}
		if p, ok := byID(host, h[0].parent); !ok || p.mountPoint() != "/" {
			t.Errorf("host parent %d is not /", h[0].parent)
		}
		if _, clash := byID(host, s[0].id); clash {
			t.Errorf("self ID %d is also a proc/1 ID; the files never share mount IDs", s[0].id)
		}
		if s[0].id <= maxID(self[:len(self)-1]) {
			t.Errorf("self ID %d is not fresh", s[0].id)
		}
	})

	t.Run("overMounted", func(t *testing.T) {
		ffs := fixtureFS(t, capUBU)
		ffs.addMount(t, "0 0 8:80 / /srv/x rw - ext4 /dev/sdf1 rw", viewOverMounted("0:251"), idOrderDesc())
		s := entryAt(ffs.mountinfo(t, "proc/self/mountinfo"), "/srv/x")
		if len(s) != 2 || s[0].majmin != "0:251" || s[1].majmin != "8:80" {
			t.Fatalf("self at /srv/x = %v, want the over-mount first", s)
		}
		if s[0].parent != s[1].id || s[0].id < s[1].id {
			t.Errorf("over-mount %v must be stacked on %v with descending IDs", s[0], s[1])
		}
	})

	t.Run("underAncestor", func(t *testing.T) {
		ffs := fixtureFS(t, capUBU)
		// Shape (b): the capture's self /tmp is a tmpfs (0:50).
		ffs.addMount(t, "0 0 8:96 / /tmp/hidden rw - ext4 /dev/sdg1 rw", viewUnderAncestor("/tmp"))
		self := ffs.mountinfo(t, "proc/self/mountinfo")
		tmp := entryAt(self, "/tmp")
		hidden := entryAt(self, "/tmp/hidden")
		if len(hidden) != 1 || len(tmp) < 2 {
			t.Fatalf("self /tmp %v, /tmp/hidden %v", tmp, hidden)
		}
		bind := tmp[len(tmp)-1]
		if !strings.Contains(bind.root, "systemd-private-") || bind.majmin != "0:50" {
			t.Errorf("bind %v, want a private root on 0:50", bind)
		}
		if p, ok := byID(self, bind.parent); !ok || p.mountPoint() != "/tmp" {
			t.Errorf("bind %v is not stacked on the copied /tmp", bind)
		}
		if p, ok := byID(self, hidden[0].parent); !ok || p.mountPoint() != "/tmp" || p.id == bind.id {
			t.Errorf("hidden %v must keep the copied /tmp as parent", hidden[0])
		}
	})

	t.Run("ambiguous and hostOnly", func(t *testing.T) {
		ffs := fixtureFS(t, capUBU)
		ffs.addMount(t, "0 0 8:97 / /srv/a rw - ext4 /dev/sdh1 rw", viewAmbiguous("0:252"))
		ffs.addMount(t, "0 0 8:98 / /srv/h rw - ext4 /dev/sdi1 rw", viewHostOnly)
		self := ffs.mountinfo(t, "proc/self/mountinfo")
		a := entryAt(self, "/srv/a")
		if len(a) != 2 || a[0].parent != a[1].parent || a[0].majmin == a[1].majmin {
			t.Errorf("ambiguous: %v", a)
		}
		if h := entryAt(self, "/srv/h"); len(h) != 0 {
			t.Errorf("hostOnly reached self: %v", h)
		}
		if h := entryAt(ffs.mountinfo(t, "proc/1/mountinfo"), "/srv/h"); len(h) != 1 {
			t.Errorf("hostOnly missing from proc/1: %v", h)
		}
	})

	t.Run("root forms", func(t *testing.T) {
		ffs := fixtureFS(t, capUBU)
		ffs.selfRoot(t, rootOwnParent)
		self := ffs.mountinfo(t, "proc/self/mountinfo")
		r := self[selfRootEntry(t, self)]
		if r.parent != r.id {
			t.Errorf("ownParent: %v", r)
		}
		ffs.stackRoot(t)
		roots := entryAt(ffs.mountinfo(t, "proc/self/mountinfo"), "/")
		if len(roots) != 2 || roots[1].parent != roots[0].id || roots[1].majmin != roots[0].majmin {
			t.Errorf("stackRoot: %v", roots)
		}
	})
}

// The SP-* profiles apply their faults.
func TestFaultProfiles(t *testing.T) {
	ffs := fixtureFS(t, capUBU)
	spSandbox(t, ffs)
	for _, name := range []string{"proc/1/mountinfo", "sys/class/dmi/id/sys_vendor", "proc/pressure/cpu"} {
		if _, err := ffs.Open(name); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("SP-SANDBOX %s: %v", name, err)
		}
	}
	if _, err := ffs.Open("proc/1/status"); err != nil {
		t.Errorf("SP-SANDBOX: root's status denied: %v", err)
	}

	ffs = fixtureFS(t, capUBU)
	spDenyAll(ffs)
	for _, name := range spPreflightFiles {
		if _, err := ffs.Open(name); err != nil {
			t.Errorf("SP-DENY-ALL %s: %v", name, err)
		}
	}
	if _, err := ffs.Open("proc/loadavg"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("SP-DENY-ALL proc/loadavg: %v", err)
	}

	ffs = fixtureFS(t, capUBU)
	spNoPSI(ffs)
	if _, err := ffs.Open("proc/pressure/cpu"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("SP-NO-PSI: %v", err)
	}

	ffs = fixtureFS(t, capUBU)
	spVMNoSensors(t, ffs)
	if entries, err := ffs.ReadDir("sys/class/hwmon"); err != nil || len(entries) != 0 {
		t.Errorf("SP-VM-NO-SENSORS: %v, %v", entries, err)
	}

	ffs = fixtureFS(t, capUBU)
	st := newFakeStatfs(t, capUBU)
	release := spHungNFS(t, ffs, st)
	defer release()
	if s := ffs.mountinfo(t, "proc/self/mountinfo"); !slices.ContainsFunc(s, func(e miEntry) bool {
		return e.mountPoint() == spHungNFSMount && e.majmin == "0:250"
	}) {
		t.Error("SP-HUNG-NFS: /mnt/nfs missing from self")
	}
	src := newSource(ffs, 50*time.Millisecond, st.Statfs)
	if _, err := src.statfs(t.Context(), spHungNFSMount); reasonFor(spHungNFSMount, err) != "timed out after 50ms: statfs /mnt/nfs" {
		t.Errorf("SP-HUNG-NFS: %v", err)
	}
	if calls := ffs.recorded(); slices.ContainsFunc(calls, func(c fsCall) bool { return strings.HasPrefix(c.name, "mnt") }) {
		t.Errorf("F-FS saw the mount point: %v", calls)
	}
}

// The default Addrs seam reports the loopback with its prefix on every
// platform.
func TestDefaultAddrsLoopback(t *testing.T) {
	addrs, err := defaultAddrs()
	if err != nil {
		t.Fatalf("defaultAddrs: %v", err)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback == 0 {
			continue
		}
		if !slices.Contains(addrs[ifc.Name], netip.MustParsePrefix("127.0.0.1/8")) {
			t.Errorf("%s: %v, want 127.0.0.1/8", ifc.Name, addrs[ifc.Name])
		}
		return
	}
	t.Skip("no loopback interface")
}

func TestIPNetPrefix(t *testing.T) {
	cases := []struct {
		in   net.Addr
		want string
	}{
		{&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)}, "192.168.1.5/24"},
		{&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}, "fe80::1/64"},
		{&net.IPNet{IP: net.ParseIP("::ffff:10.0.0.1"), Mask: net.CIDRMask(96, 128)}, "::ffff:10.0.0.1/96"},
	}
	for _, c := range cases {
		p, ok := ipNetPrefix(c.in)
		if !ok || p.String() != c.want {
			t.Errorf("ipNetPrefix(%v) = %v, %v; want %s", c.in, p, ok, c.want)
		}
	}
	if _, ok := ipNetPrefix(&net.IPAddr{IP: net.ParseIP("10.0.0.1")}); ok {
		t.Error("a non-IPNet address converted")
	}
}

// The allow-list patterns are well formed and every wildcard segment
// refuses a name that only the wildcard could supply badly.
func TestAllowListPatterns(t *testing.T) {
	for _, e := range allowList {
		p := strings.Join(e.segs, "/")
		if e.kinds == 0 || path.Clean(p) != p {
			t.Errorf("entry %q: kinds %v", p, e.kinds)
		}
	}
	for _, name := range []string{"proc/1/stat", "proc/42/status"} {
		if !permits(name, accRead) {
			t.Errorf("%s refused", name)
		}
	}
	for _, name := range []string{"proc/self/status", "proc/1x/stat", "sys/class/net//mtu", "sys/class/net/\x00/mtu", "sys/class/hwmon/hwmon0/temp_input"} {
		if permits(name, accAny) {
			t.Errorf("%s permitted", name)
		}
	}
}
