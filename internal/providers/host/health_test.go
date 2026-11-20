// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// health calls the health handler and returns its properties, checked by
// oneItem (current, host tc, assertExplicit).
func (s *hostStack) health() spec.HealthProperties {
	s.t.Helper()
	out, err := s.p.health(context.Background(), spec.NoInput{})
	return oneItem[spec.HealthProperties](s.t, spec.HealthType, out, err)
}

// The container markers of the captures (both ran in docker) and the
// others the classifier knows by file.
var containerMarkers = []string{pathDockerEnv, pathContainerEnv, pathSDContainer}

// outsideContainer removes every container marker, so the host reads as a
// VM or bare metal.
func (s *hostStack) outsideContainer() {
	for _, m := range containerMarkers {
		s.ffs.remove(m)
	}
}

// insideContainer makes the host a systemd-nspawn-style container through
// run/systemd/container, whatever else is present.
func (s *hostStack) insideContainer() {
	s.ffs.replace(pathSDContainer, []byte("docker\n"))
}

// rebootMarker plants var/run/reboot-required with its package list.
func (s *hostStack) rebootMarker(pkgs string) {
	s.ffs.replace(pathRebootMarker, nil)
	s.ffs.replace(pathRebootPackages, []byte(pkgs))
}

// removeWriters removes every reboot-required marker writer.
func (s *hostStack) removeWriters() {
	for _, w := range rebootMarkerWriters {
		s.ffs.remove(w)
	}
}

// osRelease is the capture's kernel release.
func osReleaseOf(t *testing.T, capture string) string {
	t.Helper()
	return strings.TrimSpace(string(capFile(t, capture, "fs", pathOSRelease)))
}

// The precedence of A-18: the Debian rule applies to any Debian-family OS
// (here one that is Debian only by ID_LIKE), in a container or not, and an
// Alpine host never reads the Debian marker.
func TestRebootPrecedence(t *testing.T) {
	derived := platform.Info{Init: platform.Systemd, OS: platform.OS{ID: "linuxmint", IDLike: []string{"ubuntu"}}}
	for _, inContainer := range []bool{false, true} {
		t.Run(fmt.Sprintf("debian-like container=%v", inContainer), func(t *testing.T) {
			s := newHostStackWith(t, capUBU, &derived)
			s.outsideContainer()
			if inContainer {
				s.insideContainer()
			}
			s.rebootMarker("linux-base\n")
			rr := s.health().RebootRequired
			if rr.State != spec.RebootTrue || rr.Check != checkDebianMarker || !slices.Equal(rr.Packages, []string{"linux-base"}) {
				t.Errorf("rebootRequired %+v, want true by the Debian marker", rr)
			}
		})
	}

	t.Run("alpine ignores the debian marker", func(t *testing.T) {
		s := newHostStack(t, capALP)
		s.outsideContainer()
		rel := osReleaseOf(t, capALP)
		s.ffs.replace("lib/modules/"+rel+"/modules.dep", nil)
		s.rebootMarker("linux-base\n")
		s.ffs.resetRecorder()
		rr := s.health().RebootRequired
		if rr.State != spec.RebootFalse || rr.Check != checkAlpineModules {
			t.Errorf("rebootRequired %+v, want false by the modules check", rr)
		}
		for _, c := range s.ffs.recorded() {
			if strings.HasPrefix(c.name, "var/run/reboot-required") {
				t.Errorf("Alpine read the Debian marker: %s", c)
			}
		}
	})
}

// The package list follows the apt provider's rule: names matching apt's
// package pattern, without duplicates, at most 1000; \r line ends and
// surrounding space are trimmed.
func TestRebootPackagesFollowApt(t *testing.T) {
	var b strings.Builder
	b.WriteString("linux-image-6.8.0-50-generic\r\n  linux-base  \nlinux-base\nBad_Name\n-leading\n\nx\n")
	for i := range 1100 {
		fmt.Fprintf(&b, "pkg%04d\n", i)
	}
	s := newHostStack(t, capUBU)
	s.rebootMarker(b.String())
	rr := s.health().RebootRequired
	if len(rr.Packages) != 1000 {
		t.Fatalf("%d packages, want the bound of 1000", len(rr.Packages))
	}
	if !slices.Equal(rr.Packages[:3], []string{"linux-image-6.8.0-50-generic", "linux-base", "pkg0000"}) {
		t.Errorf("packages start %q", rr.Packages[:3])
	}
	if last := rr.Packages[999]; last != "pkg0997" {
		t.Errorf("last package %q, want pkg0997", last)
	}
}

// The pattern and the bound are unexported in the apt package, so this
// test pins them to apt's source: if apt changes either, it fails until
// reboot.go follows (internal/providers/apt/parse.go pkgNameRE,
// internal/providers/apt/provider.go maxRebootPackages).
func TestRebootPackagesRuleMatchesAptSource(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "apt", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	re := regexp.MustCompile("pkgNameRE\\s*=\\s*regexp\\.MustCompile\\(`([^`]*)`\\)")
	m := re.FindStringSubmatch(read("parse.go"))
	if m == nil {
		t.Fatal("apt parse.go: pkgNameRE not found")
	}
	if m[1] != rebootPackagePattern.String() {
		t.Errorf("apt pkgNameRE %q, host %q", m[1], rebootPackagePattern.String())
	}
	bound := regexp.MustCompile(`const maxRebootPackages\s*=\s*(\d+)`).FindStringSubmatch(read("provider.go"))
	if bound == nil {
		t.Fatal("apt provider.go: maxRebootPackages not found")
	}
	if n, err := strconv.Atoi(bound[1]); err != nil || n != maxRebootPackages {
		t.Errorf("apt maxRebootPackages %s, host %d", bound[1], maxRebootPackages)
	}
}

// A marker without its package list is still true; the empty list says
// why.
func TestRebootMarkerWithoutPackageList(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathRebootMarker, nil)
	rr := s.health().RebootRequired
	if rr.State != spec.RebootTrue || len(rr.Packages) != 0 ||
		rr.Unavailable["packages"] != "not present: /var/run/reboot-required.pkgs" {
		t.Errorf("rebootRequired %+v, want true, packages [] not present", rr)
	}
}

// On an Alpine VM, an unreadable kernel release or modules directory is
// unknown with the reason, never true.
func TestRebootAlpineUnreadable(t *testing.T) {
	s := newHostStack(t, capALP)
	s.outsideContainer()
	s.ffs.missing(pathOSRelease)
	rr := s.health().RebootRequired
	if rr.State != unknownState || rr.Unavailable["state"] != "not present: /proc/sys/kernel/osrelease" {
		t.Errorf("rebootRequired %+v, want unknown, osrelease not present", rr)
	}

	s = newHostStack(t, capALP)
	s.outsideContainer()
	rel := osReleaseOf(t, capALP)
	s.ffs.deny("lib/modules/" + rel)
	rr = s.health().RebootRequired
	if rr.State != unknownState || rr.Unavailable["state"] != "permission denied: /lib/modules/"+rel {
		t.Errorf("rebootRequired %+v, want unknown, permission denied", rr)
	}
}

// OpenRC's rc-status exits 1 with no output when nothing crashed: that is
// a count of 0, not a failure.
func TestFailedServicesNoneCrashed(t *testing.T) {
	s := newHostStack(t, capALP)
	fsv := s.health().FailedServices
	if fsv.Count == nil || *fsv.Count != 0 || len(fsv.Names) != 0 || fsv.NamesTruncated || fsv.Source != "openrc" ||
		len(fsv.Unavailable) != 0 {
		t.Errorf("failedServices %+v, want count 0 from openrc", fsv)
	}
}

// A missing rc-status is not present, naming its path.
func TestFailedServicesMissingRCStatus(t *testing.T) {
	s := newHostStack(t, capALP)
	s.run.notStarted(argvRCStatusCrashed, syscall.ENOENT)
	fsv := s.health().FailedServices
	want := "not present: /bin/rc-status"
	if fsv.Count != nil || len(fsv.Names) != 0 || fsv.Unavailable["count"] != want || fsv.Unavailable["names"] != want {
		t.Errorf("failedServices %+v, want count null and names [] with %q", fsv, want)
	}
}

// An init system without a failed-service listing runs nothing.
func TestFailedServicesUnknownInit(t *testing.T) {
	s := newHostStackWith(t, capUBU, &platform.Info{Init: platform.Unknown, OS: platform.OS{ID: "ubuntu"}})
	fsv := s.health().FailedServices
	if fsv.Count != nil || !strings.HasPrefix(fsv.Unavailable["count"], prefixNotSupported) || fsv.Source != "unknown" {
		t.Errorf("failedServices %+v, want count null, not supported", fsv)
	}
	if n := len(s.run.recorded()); n != 0 {
		t.Errorf("%d commands run, want none: %q", n, s.run.recorded())
	}
}

// Failed services are queried once per request, under bounded keyed by
// the argv0, with sourceTimeout of their own.
func TestFailedServicesBoundedByArgv0(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.timeout(250 * time.Millisecond)
	s.run.hang(argvSystemctlFailed)
	start := time.Now()
	fsv := s.health().FailedServices
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Errorf("health took %v, want about 250ms", took)
	}
	if fsv.Unavailable["count"] != "timed out after 250ms: /usr/bin/systemctl" {
		t.Errorf("failedServices %+v", fsv)
	}
	if n := s.run.count(argvSystemctlFailed); n != 1 {
		t.Errorf("systemctl ran %d times, want 1", n)
	}
}

// Without a sampler the process lists are [] and the interval null, all
// with "not running"; the live flags are still there.
func TestHealthWithoutSampler(t *testing.T) {
	s := newHostStack(t, capUBU)
	h := s.health()
	if h.Sampler.State != spec.SamplerNotRunning {
		t.Errorf("sampler %q", h.Sampler.State)
	}
	for _, k := range []string{"topByCpu", "topByMemory", "processSampleSeconds"} {
		if h.Unavailable[k] != reasonNotRunning {
			t.Errorf("%s reason %q, want %q", k, h.Unavailable[k], reasonNotRunning)
		}
	}
	if len(h.TopByCPU) != 0 || len(h.TopByMemory) != 0 || h.ProcessSampleSeconds != nil {
		t.Errorf("process lists %v %v %v, want [] [] null", h.TopByCPU, h.TopByMemory, h.ProcessSampleSeconds)
	}
	if h.OOMKills == nil || h.RebootRequired.State == "" || h.FailedServices.Count == nil {
		t.Errorf("health flags missing: %+v", h)
	}
}

// A status without a Uid line, or an unreadable /etc/passwd, leaves only
// the owner fields null with the reason.
func TestProcessOwnerFaults(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	h := s.health()
	pid := h.TopByCPU[0].PID
	status := fmt.Sprintf("proc/%d/status", pid)

	s.ffs.replace(status, []byte("Name:\tx\n"))
	s.ffs.deny(pathPasswd)
	h = s.health()
	for _, pr := range append(slices.Clone(h.TopByCPU), h.TopByMemory...) {
		switch {
		case pr.PID == pid:
			want := "unrecognized format: /" + status
			if pr.UID != nil || pr.User != nil || pr.Unavailable["uid"] != want || pr.Unavailable["user"] != want {
				t.Errorf("pid %d: %+v, want uid and user null with %q", pid, pr, want)
			}
		case pr.UID == nil || pr.User != nil || pr.Unavailable["user"] != "permission denied: /etc/passwd":
			t.Errorf("pid %d: %+v, want the uid and user null with permission denied", pr.PID, pr)
		}
		if pr.CPUPercent == nil || pr.RSSBytes == nil {
			t.Errorf("pid %d lost its sampled values: %+v", pr.PID, pr)
		}
	}
}

// OOM kills distinguish a present 0 from a non-zero count and from an
// unreadable /proc/vmstat.
func TestOOMKillsValues(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace(pathVmstat, []byte("nr_free_pages 1\noom_kill 7\n"))
	if h := s.health(); h.OOMKills == nil || *h.OOMKills != 7 {
		t.Errorf("oomKills %s, want 7", show(h.OOMKills))
	}
	s.ffs.deny(pathVmstat)
	if h := s.health(); h.OOMKills != nil || h.Unavailable["oomKills"] != "permission denied: /proc/vmstat" {
		t.Errorf("oomKills %s with %q", show(h.OOMKills), h.Unavailable["oomKills"])
	}
	s = newHostStack(t, capUBU)
	s.ffs.replace(pathVmstat, []byte("garbage"))
	if h := s.health(); h.OOMKills != nil || h.Unavailable["oomKills"] != "unrecognized format: /proc/vmstat" {
		t.Errorf("oomKills %s with %q", show(h.OOMKills), h.Unavailable["oomKills"])
	}
}

// svcctlFailed is what svcctl.ListFailed makes of output from the tool of
// init, exiting with exit.
func svcctlFailed(t *testing.T, init platform.InitSystem, output []byte, exit int) svcctl.FailedServices {
	t.Helper()
	run := func(context.Context, []string) (svcctl.Result, error) {
		return svcctl.Result{ExitCode: exit, Output: output}, nil
	}
	fs, err := svcctl.ListFailed(context.Background(), init, svcctl.Options{Run: run})
	if err != nil {
		t.Fatalf("svcctl.ListFailed: %v", err)
	}
	return fs
}
