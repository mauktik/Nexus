// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// hostStack is the host stack of the Phase 4 handler tests
// (test-strategy.md, Handlers): the real provider over F-FS, F-STATFS,
// F-RUN, F-UNAME, F-ADDRS, F-CLK and F-TICK, reporting host "tc". It
// embeds the sampler stack, so ready() drives it to a known snapshot.
type hostStack struct {
	*samplerStack
	run  *fakeRun
	plat platform.Info
}

// newHostStack builds the stack over a capture. The platform's OS is the
// capture's, read by internal/platform from the capture's root; its init
// system is the one the capture ran.
func newHostStack(t *testing.T, capture string) *hostStack {
	t.Helper()
	plat := platform.DetectAt("linux", filepath.Join("testdata", capture, "fs"))
	plat.Init = platform.Systemd
	if capture == capALP {
		plat.Init = platform.OpenRC
	}
	return newHostStackWith(t, capture, &plat)
}

// newHostStackWith is newHostStack with Options.Platform set to plat (nil:
// the production default, platform.Current()).
func newHostStackWith(t *testing.T, capture string, plat *platform.Info) *hostStack {
	t.Helper()
	ffs := fixtureFS(t, capture)
	s := &samplerStack{t: t, ffs: ffs, clk: newFakeClock(), tks: newFakeTickers(t), logs: &logCapture{}}
	run := newFakeRun(t, capture)
	s.p = New(Options{Host: "tc", Logger: slog.New(s.logs), Platform: plat, GOOS: "linux", FS: ffs,
		Statfs: newFakeStatfs(t, capture).Statfs, Uname: fakeUname(t, capture), Addrs: fakeAddrs(t, capture),
		Run: run.Run, Now: s.clk.Now, Ticker: s.tks.New, PageSize: 4096})
	hs := &hostStack{samplerStack: s, run: run}
	if plat != nil {
		hs.plat = *plat
	} else {
		hs.plat = platform.Current()
	}
	return hs
}

// timeout sets sourceTimeout.
func (s *hostStack) timeout(d time.Duration) {
	s.t.Helper()
	if err := s.p.Configure(hostSection(s.t, "sourceTimeout: "+d.String()+"\n")); err != nil {
		s.t.Fatalf("Configure: %v", err)
	}
}

// oneItem checks a one-item read's list and its resource fields, runs
// assertExplicit on the raw output, and returns the properties decoded
// from the JSON, so the test sees the wire shape.
func oneItem[P any](t *testing.T, typ string, out any, err error) P {
	t.Helper()
	if err != nil {
		t.Fatalf("%s handler: %v", typ, err)
	}
	assertExplicit(t, out)
	raw, mErr := json.Marshal(out)
	if mErr != nil {
		t.Fatalf("marshal: %v", mErr)
	}
	var l apitypes.List[apitypes.Resource[P]]
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatalf("decode %s: %v\n%s", typ, err, raw)
	}
	if len(l.Items) != 1 || l.NextCursor != "" {
		t.Fatalf("%s: %d items, nextCursor %q; want one item and no cursor", typ, len(l.Items), l.NextCursor)
	}
	r := l.Items[0]
	if r.Name != spec.CurrentName || r.Host != "tc" || r.Type != typ ||
		r.ID != apitypes.ResourceID(spec.Name, typ, spec.CurrentName) {
		t.Errorf("%s resource = id %q type %q name %q host %q; want %s, %s, current, tc", typ, r.ID, r.Type, r.Name,
			r.Host, apitypes.ResourceID(spec.Name, typ, spec.CurrentName), typ)
	}
	return r.Properties
}

func (s *hostStack) identity() spec.IdentityProperties {
	s.t.Helper()
	out, err := s.p.identity(context.Background(), spec.NoInput{})
	return oneItem[spec.IdentityProperties](s.t, spec.IdentityType, out, err)
}

func (s *hostStack) cpu() spec.CPUProperties {
	s.t.Helper()
	out, err := s.p.cpu(context.Background(), spec.NoInput{})
	return oneItem[spec.CPUProperties](s.t, spec.CPUType, out, err)
}

func (s *hostStack) memory() spec.MemoryProperties {
	s.t.Helper()
	out, err := s.p.memory(context.Background(), spec.NoInput{})
	return oneItem[spec.MemoryProperties](s.t, spec.MemoryType, out, err)
}

// toolText is a capture's tools/<name>, trimmed.
func toolText(t *testing.T, capture, name string) string {
	t.Helper()
	return strings.TrimSpace(string(capFile(t, capture, "tools", name)))
}

// toolOS reads the capture's tools/ copy of the OS identification with a
// parser of the test's own, independent of internal/platform's.
func toolOS(t *testing.T, capture string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range strings.Split(toolText(t, capture, "os-release"), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		}
		out[k] = v
	}
	return out
}

func eqStr(got *string, want string) bool { return got != nil && *got == want }

func show[T any](p *T) string {
	if p == nil {
		return "null"
	}
	b, _ := json.Marshal(*p)
	return string(b)
}

// An identity from a host whose files are all missing still has every
// field explicit, and the OS fields of an empty platform carry a reason.
func TestIdentityEverySourceMissing(t *testing.T) {
	s := newHostStackWith(t, capUBU, &platform.Info{Init: platform.Unknown})
	s.ffs.addFault(&fsFault{match: func(string) bool { return true }, errno: syscall.ENOENT})
	p := s.identity()
	if p.Hostname != nil || p.KernelRelease != nil || p.BootTime != nil || p.UptimeSeconds != nil {
		t.Errorf("identity = %+v, want hostname, kernel, boot time and uptime null", p)
	}
	if got := p.Unavailable["hostname"]; got != "not present: /proc/sys/kernel/hostname" {
		t.Errorf("hostname reason %q", got)
	}
	if p.OS.ID != nil || p.OS.Unavailable["id"] == "" {
		t.Errorf("os = %+v, want id null with a reason", p.OS)
	}
	if p.InitSystem != "unknown" {
		t.Errorf("initSystem %q, want unknown", p.InitSystem)
	}
	if p.Virtualization.Type != "none" || p.Virtualization.Kind != "none" {
		t.Errorf("virtualization %+v, want none", p.Virtualization)
	}
	// Without /etc/localtime the zone is UTC (A-31), whatever else failed.
	if !eqStr(p.Timezone.Name, "UTC") || p.Timezone.OffsetSeconds == nil || *p.Timezone.OffsetSeconds != 0 {
		t.Errorf("timezone %+v, want UTC 0", p.Timezone)
	}
}

// A uname failure is a reason that quotes no error text.
func TestIdentityUnameFailure(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.p.uname = func() (string, error) { return "", errNotSupported("uname on plan9") }
	p := s.identity()
	if p.Architecture != nil || p.Unavailable["architecture"] != "not supported: uname on plan9" {
		t.Errorf("architecture %s with %q", show(p.Architecture), p.Unavailable["architecture"])
	}
	s.p.uname = func() (string, error) { return "", os.ErrPermission }
	p = s.identity()
	if p.Architecture != nil || p.Unavailable["architecture"] != "not present: uname(2) machine" {
		t.Errorf("architecture %s with %q", show(p.Architecture), p.Unavailable["architecture"])
	}
}

// Empty DMI files and an empty hostname are not reported rather than "".
func TestIdentityEmptyValues(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("sys/class/dmi/id/product_version", []byte("\n"))
	s.ffs.replace("proc/sys/kernel/hostname", []byte(" \n"))
	p := s.identity()
	if p.DMI.ProductVersion != nil ||
		p.DMI.Unavailable["productVersion"] != "not reported: empty /sys/class/dmi/id/product_version" {
		t.Errorf("productVersion %s with %q", show(p.DMI.ProductVersion), p.DMI.Unavailable["productVersion"])
	}
	if p.Hostname != nil || p.Unavailable["hostname"] != "unrecognized format: /proc/sys/kernel/hostname" {
		t.Errorf("hostname %s with %q", show(p.Hostname), p.Unavailable["hostname"])
	}
}

// A /etc/localtime that is not a tzfile keeps the name and reports the
// offset unreadable; a denied one is a permission reason.
func TestIdentityTimezoneFaults(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("etc/localtime", []byte("not a tzfile"))
	p := s.identity()
	if !eqStr(p.Timezone.Name, "Etc/UTC") || p.Timezone.OffsetSeconds != nil ||
		p.Timezone.Unavailable["offsetSeconds"] != "unrecognized format: /etc/localtime" {
		t.Errorf("timezone %+v", p.Timezone)
	}

	s = newHostStack(t, capUBU)
	s.ffs.deny("etc/localtime")
	p = s.identity()
	if p.Timezone.OffsetSeconds != nil || p.Timezone.Unavailable["offsetSeconds"] != "permission denied: /etc/localtime" {
		t.Errorf("timezone %+v", p.Timezone)
	}
}
