// Tests of planning/system-provider/test-cases.md TC-HP-90, TC-HP-91, TC-HP-92, TC-HP-93, TC-HP-94, TC-HP-95, TC-HP-96, TC-HP-97, TC-HP-ERR25.
// Owner WI: 4.4. The health helpers (s.health, the container and reboot
// marker setters, svcctlFailed) are in health_test.go; the host stack is in
// identity_test.go.

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/apt"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-HP-90: Reboot required on the Debian family follows the apt rule. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU F-FS variants: (a) `var/run/reboot-required` and `.pkgs` with `linux-image-6.8.0-50-generic\nlinux-base\n`; (b) no marker, one `apt.RebootMarkerWriters` path present; (c) no marker, no writer; (d) as (a) plus the container marker `run/systemd/container`.
// - Steps: health handler.
// - Expected: (a) and (d) `state "true"`, `packages` = the two names, `check debian-reboot-required-marker`, `evidence` names `/var/run/reboot-required`; (b) `"false"`; (c) `"unknown"` with evidence that no marker writer is installed. (d) encodes open item 1 of test-strategy.md.
func TestTCHP90_RebootRequiredOnTheDebianFamilyFollows(t *testing.T) {
	const pkgs = "linux-image-6.8.0-50-generic\nlinux-base\n"
	wantPkgs := []string{"linux-image-6.8.0-50-generic", "linux-base"}
	cases := []struct {
		name  string
		setup func(s *hostStack)
		state string
	}{
		{"a marker", func(s *hostStack) { s.rebootMarker(pkgs) }, spec.RebootTrue},
		{"b writer", func(s *hostStack) {
			s.removeWriters()
			s.ffs.replace(rebootMarkerWriters[0], nil)
		}, spec.RebootFalse},
		{"c neither", func(s *hostStack) { s.removeWriters() }, spec.RebootUnknown},
	}
	for _, c := range cases {
		// (d) is (a) in a container; each variant is checked both ways and
		// must give the same answer (A-18).
		for _, inContainer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s container=%v", c.name, inContainer), func(t *testing.T) {
				s := newHostStack(t, capUBU)
				s.outsideContainer()
				if inContainer {
					s.insideContainer()
				}
				c.setup(s)
				rr := s.health().RebootRequired
				if rr.State != c.state || rr.Check != "debian-reboot-required-marker" {
					t.Fatalf("rebootRequired %+v, want %s by debian-reboot-required-marker", rr, c.state)
				}
				if !strings.Contains(rr.Evidence, "/var/run/reboot-required") {
					t.Errorf("evidence %q does not name /var/run/reboot-required", rr.Evidence)
				}
				switch c.state {
				case spec.RebootTrue:
					if !slices.Equal(rr.Packages, wantPkgs) {
						t.Errorf("packages %q, want %q", rr.Packages, wantPkgs)
					}
				case spec.RebootFalse:
					if len(rr.Packages) != 0 || len(rr.Unavailable) != 0 {
						t.Errorf("rebootRequired %+v, want no packages and no reason", rr)
					}
				case spec.RebootUnknown:
					if !strings.Contains(rr.Evidence, "no marker writer is installed") ||
						!strings.HasPrefix(rr.Unavailable["state"], prefixNotReported) {
						t.Errorf("rebootRequired %+v, want evidence that no marker writer is installed", rr)
					}
				}
			})
		}
	}
}

// TC-HP-91: Reboot required on Alpine compares the running kernel with /lib/modules. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS, no container markers (VM capture); variants with `lib/modules/<osrelease>` present and removed.
// - Steps: health handler.
// - Expected: present → `"false"`, missing → `"true"`; `check alpine-running-kernel-modules`; evidence names `/lib/modules/<release>` with the capture's release.
func TestTCHP91_RebootRequiredOnAlpineComparesTheRunning(t *testing.T) {
	rel := osReleaseOf(t, capALP)
	modules := "lib/modules/" + rel
	for _, present := range []bool{true, false} {
		t.Run(fmt.Sprintf("modules present=%v", present), func(t *testing.T) {
			s := newHostStack(t, capALP)
			s.outsideContainer()
			if present {
				s.ffs.replace(modules+"/modules.dep", nil)
			}
			rr := s.health().RebootRequired
			want := spec.RebootTrue
			if present {
				want = spec.RebootFalse
			}
			if rr.State != want || rr.Check != "alpine-running-kernel-modules" {
				t.Errorf("rebootRequired %+v, want %s by alpine-running-kernel-modules", rr, want)
			}
			if !strings.Contains(rr.Evidence, "/"+modules) {
				t.Errorf("evidence %q does not name /%s", rr.Evidence, modules)
			}
			if len(rr.Packages) != 0 || len(rr.Unavailable) != 0 {
				t.Errorf("rebootRequired %+v, want no packages and no reason", rr)
			}
		})
	}
}

// TC-HP-92: Reboot required is unknown in a non-Debian container. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: ALP F-FS plus `.dockerenv`, `lib/modules/<release>` missing; a platform with an unknown OS family and no container.
// - Steps: health handler.
// - Expected: container: `"unknown"`, evidence says the kernel belongs to the container host; unknown family: `"unknown"` with `not supported: …`.
func TestTCHP92_RebootRequiredIsUnknownInANon(t *testing.T) {
	t.Run("alpine container", func(t *testing.T) {
		s := newHostStack(t, capALP) // the capture has /.dockerenv; no lib/modules
		rr := s.health().RebootRequired
		if rr.State != spec.RebootUnknown || !strings.Contains(rr.Evidence, "container host") ||
			rr.Unavailable["state"] == "" {
			t.Errorf("rebootRequired %+v, want unknown: the kernel belongs to the container host", rr)
		}
		if !strings.Contains(rr.Evidence, "/lib/modules/"+osReleaseOf(t, capALP)) {
			t.Errorf("evidence %q does not name the modules directory", rr.Evidence)
		}
	})
	t.Run("unknown family", func(t *testing.T) {
		s := newHostStackWith(t, capUBU, &platform.Info{Init: platform.Systemd, OS: platform.OS{ID: "arch"}})
		s.outsideContainer()
		s.rebootMarker("linux-base\n") // never read outside the Debian family
		rr := s.health().RebootRequired
		if rr.State != spec.RebootUnknown || !strings.HasPrefix(rr.Unavailable["state"], "not supported: ") {
			t.Errorf("rebootRequired %+v, want unknown, not supported", rr)
		}
		if len(rr.Packages) != 0 {
			t.Errorf("packages %q, want []", rr.Packages)
		}
	})
}

// TC-HP-93: Reboot constants mirror the apt provider. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: import `internal/providers/apt`.
// - Steps: compare.
// - Expected: the host constants equal `strings.TrimPrefix(apt.RebootMarker, "/")`, `…RebootPackagesFile`, and each of `apt.RebootMarkerWriters` with the leading `/` removed, in order; every one is allow-listed.
func TestTCHP93_RebootConstantsMirrorTheAptProvider(t *testing.T) {
	trim := func(p string) string { return strings.TrimPrefix(p, "/") }
	if pathRebootMarker != trim(apt.RebootMarker) || !permits(pathRebootMarker, accStatOnly) ||
		permits(pathRebootMarker, accRead) {
		t.Errorf("marker %q, want %q, stat-only", pathRebootMarker, trim(apt.RebootMarker))
	}
	if pathRebootPackages != trim(apt.RebootPackagesFile) || !permits(pathRebootPackages, accRead) {
		t.Errorf("package list %q, want %q, readable", pathRebootPackages, trim(apt.RebootPackagesFile))
	}
	want := make([]string, len(apt.RebootMarkerWriters))
	for i, w := range apt.RebootMarkerWriters {
		want[i] = trim(w)
	}
	if !slices.Equal(rebootMarkerWriters, want) {
		t.Errorf("writers %q, want %q in order", rebootMarkerWriters, want)
	}
	for _, w := range rebootMarkerWriters {
		if !permits(w, accStatOnly) || permits(w, accRead) {
			t.Errorf("writer %q is not allow-listed stat-only", w)
		}
	}
}

// TC-HP-94: Failed services from the captured outputs, cut at 20 names. Owner WI 4.4. Requirements: FR-10.
//
// - Setup: UBU with F-RUN answering `list-units-failed.txt`; ALP with `rc-status-crashed.txt`; UBU with a capture-derived output of 21 unit lines.
// - Steps: health handler.
// - Expected: `failedServices.count` and `names` equal `svcctl.Failed`'s on the same output, `source` names the tool; with 21: `count 21`, 20 names (the first 20 sorted), `namesTruncated true`.
func TestTCHP94_FailedServicesFromTheCapturedOutputsCut(t *testing.T) {
	svcTestdata := filepath.Join("..", "..", "svcctl", "testdata")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(svcTestdata, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var many bytes.Buffer
	line := strings.Fields(strings.TrimSpace(string(capFile(t, capUBU, "tools", "systemctl-failed"))))
	for i := 21; i >= 1; i-- { // unsorted on purpose
		fmt.Fprintf(&many, "unit-%02d.service %s\n", i, strings.Join(line[1:], " "))
	}
	cases := []struct {
		name, capture string
		argv          []string
		output        []byte
		exit          int
		source        string
	}{
		{"systemd", capUBU, argvSystemctlFailed, read("systemd/list-units-failed.txt"), 0, "systemd"},
		{"openrc", capALP, argvRCStatusCrashed, read("openrc/rc-status-crashed.txt"), 0, "openrc"},
		{"systemd 21", capUBU, argvSystemctlFailed, many.Bytes(), 0, "systemd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, c.capture)
			s.run.set(c.argv, c.output, c.exit)
			fsv := s.health().FailedServices
			want := svcctlFailed(t, s.plat.Init, c.output, c.exit)
			if want.Count == 0 {
				t.Fatalf("the %s output lists nothing", c.name)
			}
			wantNames := want.Names[:min(len(want.Names), spec.MaxFailedServiceNames)]
			if fsv.Count == nil || *fsv.Count != want.Count || !slices.Equal(fsv.Names, wantNames) ||
				fsv.Source != c.source || fsv.NamesTruncated != (want.Count > spec.MaxFailedServiceNames) {
				t.Errorf("failedServices %+v (count %s), want count %d, names %q, source %s", fsv, show(fsv.Count),
					want.Count, wantNames, c.source)
			}
			if c.name == "systemd 21" {
				if *fsv.Count != 21 || len(fsv.Names) != 20 || !fsv.NamesTruncated || fsv.Names[0] != "unit-01.service" ||
					fsv.Names[19] != "unit-20.service" {
					t.Errorf("21 units: %+v, want count 21, the first 20 sorted, truncated", fsv)
				}
			}
		})
	}
}

// TC-HP-95: OOM kills come from vmstat only. Owner WI 4.4. Requirements: FR-10, NFR-6.
//
// - Setup: UBU; variant with the `oom_kill` line removed.
// - Steps: health handler; read F-FS's recorder.
// - Expected: `oomKills` = the captured `oom_kill`; removed → null with `not present: /proc/vmstat oom_kill`; no recorded path contains `kmsg` or `log`.
func TestTCHP95_OOMKillsComeFromVmstatOnly(t *testing.T) {
	want := u64T(t, strings.Fields(func() string {
		for _, l := range strings.Split(string(capFile(t, capUBU, "fs", pathVmstat)), "\n") {
			if strings.HasPrefix(l, "oom_kill ") {
				return l
			}
		}
		t.Fatal("the capture has no oom_kill")
		return ""
	}())[1])

	s := newHostStack(t, capUBU)
	s.ready()
	s.ffs.resetRecorder()
	h := s.health()
	if h.OOMKills == nil || *h.OOMKills != want {
		t.Errorf("oomKills %s, want %d", show(h.OOMKills), want)
	}
	for _, c := range s.ffs.recorded() {
		if strings.Contains(c.name, "kmsg") || strings.Contains(c.name, "log") {
			t.Errorf("health read %s", c)
		}
	}

	var stripped []string
	for _, l := range strings.Split(string(capFile(t, capUBU, "fs", pathVmstat)), "\n") {
		if !strings.HasPrefix(l, "oom_kill ") {
			stripped = append(stripped, l)
		}
	}
	s.ffs.replace(pathVmstat, []byte(strings.Join(stripped, "\n")))
	h = s.health()
	if h.OOMKills != nil || h.Unavailable["oomKills"] != "not present: /proc/vmstat oom_kill" {
		t.Errorf("oomKills %s with %q, want null, not present: /proc/vmstat oom_kill", show(h.OOMKills),
			h.Unavailable["oomKills"])
	}
}

// TC-HP-96: Top processes carry owner names and only comm. Owner WI 4.4. Requirements: FR-10, NFR-6, FR-15.
//
// - Setup: ready snapshot per capture; variant where one top pid's uid has no passwd entry; variant where one top pid's `status` is removed after the scan.
// - Steps: health handler.
// - Expected:
//   - `topByCpu`/`topByMemory` equal TC-HP-64's; `uid` from `t1/proc/<pid>/status`; `user` from `etc/passwd`;
//   - no passwd entry → `user` null with `not reported: uid N has no /etc/passwd entry`; vanished → the sampled values kept, `uid` and `user` null with `not present: /proc/<pid>/status`;
//   - `processSampleSeconds` = 10; `assertExplicit` passes.
func TestTCHP96_TopProcessesCarryOwnerNamesAndOnly(t *testing.T) {
	for _, capture := range []string{capUBU, capALP} {
		t.Run(capture, func(t *testing.T) {
			s := newHostStack(t, capture)
			s.ready()
			h := s.health()
			top := expectedTop(t, capture, nil)
			users := passwdOf(t, capture)
			check := func(name string, got []spec.Process, want []procRate) {
				t.Helper()
				if len(got) != len(want) || len(got) != spec.TopProcesses {
					t.Fatalf("%s has %d, want %d (5)", name, len(got), len(want))
				}
				for i, g := range got {
					w := want[i]
					if g.PID != w.pid || g.Command != w.comm || g.RSSBytes == nil || *g.RSSBytes != w.rssBytes ||
						g.CPUPercent == nil || !near(*g.CPUPercent, w.cpuPercent) {
						t.Errorf("%s[%d] = %+v, want %+v", name, i, g, w)
					}
					uid := statusUID(t, capture, g.PID)
					if g.UID == nil || *g.UID != uid {
						t.Errorf("%s[%d] uid %s, want %d", name, i, show(g.UID), uid)
					}
					if u, ok := users[uid]; !ok || !eqStr(g.User, u) {
						t.Errorf("%s[%d] user %s, want %q", name, i, show(g.User), u)
					}
				}
			}
			check("topByCpu", h.TopByCPU, top.byCPU)
			check("topByMemory", h.TopByMemory, top.byMem)
			if h.ProcessSampleSeconds == nil || *h.ProcessSampleSeconds != 10 {
				t.Errorf("processSampleSeconds %s, want 10", show(h.ProcessSampleSeconds))
			}
		})
	}

	t.Run("no passwd entry", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ready()
		uid := statusUID(t, capUBU, s.health().TopByCPU[0].PID)
		var kept []string
		for _, l := range strings.Split(string(capFile(t, capUBU, "fs", pathPasswd)), "\n") {
			if f := strings.Split(l, ":"); len(f) == 7 && f[2] == strconv.Itoa(uid) {
				continue
			}
			kept = append(kept, l)
		}
		s.ffs.replace(pathPasswd, []byte(strings.Join(kept, "\n")))
		h := s.health() // assertExplicit runs in oneItem
		want := fmt.Sprintf("not reported: uid %d has no /etc/passwd entry", uid)
		for _, pr := range append(slices.Clone(h.TopByCPU), h.TopByMemory...) {
			if pr.UID == nil || *pr.UID != uid {
				continue
			}
			if pr.User != nil || pr.Unavailable["user"] != want {
				t.Errorf("pid %d user %s with %q, want null with %q", pr.PID, show(pr.User), pr.Unavailable["user"], want)
			}
		}
	})

	t.Run("vanished", func(t *testing.T) {
		s := newHostStack(t, capUBU)
		s.ready()
		before := s.health()
		gone := before.TopByCPU[0]
		s.ffs.remove(fmt.Sprintf("proc/%d/status", gone.PID))
		h := s.health()
		want := fmt.Sprintf("not present: /proc/%d/status", gone.PID)
		found := 0
		for _, pr := range append(slices.Clone(h.TopByCPU), h.TopByMemory...) {
			if pr.PID != gone.PID {
				if pr.UID == nil {
					t.Errorf("pid %d lost its uid", pr.PID)
				}
				continue
			}
			found++
			if pr.UID != nil || pr.User != nil || pr.Unavailable["uid"] != want || pr.Unavailable["user"] != want {
				t.Errorf("vanished pid %d: %+v, want uid and user null with %q", pr.PID, pr, want)
			}
			if pr.Command != gone.Command || show(pr.CPUPercent) != show(gone.CPUPercent) ||
				show(pr.RSSBytes) != show(gone.RSSBytes) {
				t.Errorf("vanished pid %d: %+v lost its sampled values %+v", pr.PID, pr, gone)
			}
		}
		if found == 0 {
			t.Errorf("pid %d left the lists", gone.PID)
		}
	})
}

// TC-HP-97: Health output never carries command lines or environment. Owner WI 4.4. Requirements: NFR-6, FR-10.
//
// - Setup: ready snapshot (UBU).
// - Steps: marshal the health output; walk every key.
// - Expected: process objects have exactly `pid`, `command`, `uid`, `user`, `cpuPercent`, `rssBytes` (plus `unavailable` when set); no key contains `cmdline`, `args`, `argv`, `env`; each `command` is ≤ 15 bytes and equals that pid's captured comm.
func TestTCHP97_HealthOutputNeverCarriesCommandLinesOr(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	out, err := s.p.health(context.Background(), spec.NoInput{})
	h := oneItem[spec.HealthProperties](t, spec.HealthType, out, err)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, n any)
	walk = func(path string, n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, c := range v {
				lk := strings.ToLower(k)
				for _, bad := range []string{"cmdline", "args", "argv", "env"} {
					if strings.Contains(lk, bad) {
						t.Errorf("%s.%s: key contains %q", path, k, bad)
					}
				}
				walk(path+"."+k, c)
			}
		case []any:
			for i, c := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), c)
			}
		}
	}
	walk("$", doc)

	props := doc.(map[string]any)["items"].([]any)[0].(map[string]any)["properties"].(map[string]any)
	allowed := []string{"command", "cpuPercent", "pid", "rssBytes", "uid", "user"}
	for _, list := range []string{"topByCpu", "topByMemory"} {
		items := props[list].([]any)
		if len(items) == 0 {
			t.Fatalf("%s is empty", list)
		}
		for i, it := range items {
			var keys []string
			for k := range it.(map[string]any) {
				if k != "unavailable" {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			if !slices.Equal(keys, allowed) {
				t.Errorf("%s[%d] keys %q, want exactly %q", list, i, keys, allowed)
			}
		}
	}
	for _, pr := range append(slices.Clone(h.TopByCPU), h.TopByMemory...) {
		b := capFile(t, capUBU, "t1", fmt.Sprintf("proc/%d/stat", pr.PID))
		comm := string(b[bytes.IndexByte(b, '(')+1 : bytes.LastIndexByte(b, ')')])
		if len(pr.Command) > 15 || pr.Command != comm {
			t.Errorf("pid %d command %q, want the captured comm %q (at most 15 bytes)", pr.PID, pr.Command, comm)
		}
	}
}

// TC-HP-ERR25: Failed-service query failures leave the count unavailable. Owner WI 4.4. Requirements: FR-10, FR-12.
//
// - Setup: UBU with F-RUN for the failed-units argv: `hang` (timeout 250 ms); `ErrNotStarted` whose cause is ENOENT (the `realRun` shape of TC-HP-ERR18); exit 1 with bus error text.
// - Steps: health handler.
// - Expected: `failedServices.count` null and `names []`, both in `unavailable`, with `timed out after 250ms: /usr/bin/systemctl`, `not present: /usr/bin/systemctl` (Finding 7), `command failed: /usr/bin/systemctl exit 1`; the rest of health populated; no command output text in the response.
func TestTCHPERR25_FailedServiceQueryFailuresLeaveTheCount(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *fakeRun)
		why   string
	}{
		{"hang", func(r *fakeRun) { r.hang(argvSystemctlFailed) }, "timed out after 250ms: /usr/bin/systemctl"},
		{"not installed", func(r *fakeRun) { r.notStarted(argvSystemctlFailed, syscall.ENOENT) },
			"not present: /usr/bin/systemctl"},
		{"exit 1", func(r *fakeRun) {
			r.set(argvSystemctlFailed, []byte("Failed to connect to bus: No such file or directory\n"), 1)
		}, "command failed: /usr/bin/systemctl exit 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, capUBU)
			s.timeout(250 * time.Millisecond)
			c.setup(s.run)
			start := time.Now()
			out, err := s.p.health(context.Background(), spec.NoInput{})
			if took := time.Since(start); took > 400*time.Millisecond {
				t.Errorf("health took %v, want about 250ms at most", took)
			}
			h := oneItem[spec.HealthProperties](t, spec.HealthType, out, err)
			fsv := h.FailedServices
			if fsv.Count != nil || len(fsv.Names) != 0 || fsv.Unavailable["count"] != c.why ||
				fsv.Unavailable["names"] != c.why || fsv.NamesTruncated || fsv.Source != "systemd" {
				t.Errorf("failedServices %+v, want count null and names [] with %q", fsv, c.why)
			}
			if h.OOMKills == nil || h.RebootRequired.State != spec.RebootFalse {
				t.Errorf("the rest of health is not populated: %+v", h)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "Failed to connect") || strings.Contains(string(raw), "bus") {
				t.Errorf("output quotes the command's text: %s", raw)
			}
		})
	}
}

// statusUID is the real uid of t1/proc/<pid>/status, by the test's own
// reading of the Uid: line.
func statusUID(t *testing.T, capture string, pid int) int {
	t.Helper()
	for _, l := range strings.Split(string(capFile(t, capture, "t1", fmt.Sprintf("proc/%d/status", pid))), "\n") {
		if rest, ok := strings.CutPrefix(l, "Uid:"); ok {
			return atoiT(t, strings.Fields(rest)[0])
		}
	}
	t.Fatalf("pid %d: no Uid line", pid)
	return 0
}

// passwdOf maps the capture's /etc/passwd uids to names, first line wins.
func passwdOf(t *testing.T, capture string) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, l := range strings.Split(string(capFile(t, capture, "fs", pathPasswd)), "\n") {
		f := strings.Split(l, ":")
		if len(f) != 7 {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if _, dup := out[uid]; err != nil || dup {
			continue
		}
		out[uid] = f[0]
	}
	return out
}
