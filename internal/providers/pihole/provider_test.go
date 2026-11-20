// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Cases: TC-52, TC-53, TC-55, TC-56, TC-57 and TC-ERR7 in
// planning/pihole-dns-zones/test-cases.md.
//
// Each test builds a temp root (v5.Paths.Root) with the Pi-hole files laid
// out at their host paths. The versions file is v5/testdata/versions, a
// verbatim copy of a real Pi-hole v5 host's (v5/testdata/PROVENANCE), and
// the expected versions are parsed from it here, never typed in. The FTL
// text API, when a case needs one, is a real TCP listener on 127.0.0.1
// whose port goes into the root's pihole-FTL.conf.
package pihole

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

const (
	defaultDNSFile = "/etc/dnsmasq.d/03-custom-dns-names.conf"
	pfVersions     = "v5/testdata/versions"
	pfDNSFixture   = "dnsfile/testdata/fr23-seven-lines.conf"
)

// pfRoot is a temp Pi-hole host root and the provider wired to it through
// the test seams only.
type pfRoot struct {
	t      *testing.T
	root   string
	state  string // The daemon's state directory; ops is <state>/ops
	p      *Provider
	runner *recordingRunner
}

func (r *pfRoot) path(abs string) string { return filepath.Join(r.root, abs) }

func (r *pfRoot) write(abs string, b []byte, mode fs.FileMode) {
	r.t.Helper()
	p := r.path(abs)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *pfRoot) remove(abs string) {
	r.t.Helper()
	if err := os.RemoveAll(r.path(abs)); err != nil {
		r.t.Fatal(err)
	}
}

func pfFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// closedFTLPort is a loopback port nothing listens on, so FTL's >version
// fallback is refused at once rather than reaching a real FTL on 4711.
func closedFTLPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// newV5Root lays out a v5 host: both binaries executable, the captured
// versions file, a pihole-FTL.conf naming a closed port, a dnsmasq.conf
// that loads /etc/dnsmasq.d, and the FR-23 DNS file. systemd is available.
func newV5Root(t *testing.T) *pfRoot {
	t.Helper()
	dir := t.TempDir()
	r := &pfRoot{t: t, root: filepath.Join(dir, "root"), state: filepath.Join(dir, "state"), runner: &recordingRunner{}}
	r.write(v5.PiholeBin, []byte("#!/bin/bash\n"), 0o755)
	r.write(v5.PiholeFTLBin, []byte("ELF"), 0o755)
	r.write(v5.VersionsFile, pfFixture(t, pfVersions), 0o644)
	r.write(v5.FTLConfFile, fmt.Appendf(nil, "FTLPORT=%d\n", closedFTLPort(t)), 0o644)
	r.write(v5.DnsmasqConfFile, []byte("conf-dir=/etc/dnsmasq.d\n"), 0o644)
	r.write(defaultDNSFile, pfFixture(t, pfDNSFixture), 0o644)
	r.p = New(Options{Host: "h", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.p.paths = v5.Paths{Root: r.root}
	r.p.runner = r.runner
	r.p.systemd = func() detach.Availability { return detach.Availability{Available: true} }
	r.p.UseOpsDir(filepath.Join(r.state, "ops"))
	return r
}

// newV6Root lays out a v6 host: no versions file, FTL refused, and a
// pihole.toml whose [misc] section sets etc_dnsmasq_d as given.
func newV6Root(t *testing.T, etcDnsmasqD string) *pfRoot {
	t.Helper()
	r := newV5Root(t)
	r.remove(v5.VersionsFile)
	r.write(v5.PiholeTOMLFile, []byte("[dns]\n  upstreams = [\"9.9.9.9\"]\n\n[misc]\n  etc_dnsmasq_d = "+
		etcDnsmasqD+" ### CHANGED, default = false\n"), 0o644)
	return r
}

// recordingRunner records every command; Preflight must never run one.
type recordingRunner struct {
	mu    sync.Mutex
	argvs [][]string
}

func (r *recordingRunner) Run(_ context.Context, argv []string, _, _ io.Writer) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	return 1, errors.New("recordingRunner: no command may run")
}

func (r *recordingRunner) calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.argvs)
}

// fixtureVersions parses CORE_VERSION, WEB_VERSION and FTL_VERSION from the
// captured versions file, by exact key.
func fixtureVersions(t *testing.T) (coreV, web, ftl string) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(pfFixture(t, pfVersions)))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "CORE_VERSION":
			coreV = v
		case "WEB_VERSION":
			web = v
		case "FTL_VERSION":
			ftl = v
		}
	}
	if coreV == "" || web == "" || ftl == "" {
		t.Fatalf("fixture %s lacks a version: %q %q %q", pfVersions, coreV, web, ftl)
	}
	return coreV, web, ftl
}

func pfConfigs(enabled bool, decode func(any) error) map[string]core.ProviderConfig {
	return map[string]core.ProviderConfig{spec.Name: {Enabled: enabled, Decode: decode}}
}

func capabilityOf(t *testing.T, caps apitypes.Capabilities, name string) apitypes.ProviderCapability {
	t.Helper()
	i := slices.IndexFunc(caps.Providers, func(p apitypes.ProviderCapability) bool { return p.Name == name })
	if i < 0 {
		t.Fatalf("provider %q not in capabilities %+v", name, caps.Providers)
	}
	return caps.Providers[i]
}

// TC-ERR7: each missing dependency makes the provider unavailable with a
// distinct reason that names the cause.
func TestTCERR7_PreflightUnavailableReasons(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *pfRoot)
		want  string // A substring naming the cause
	}{
		{"no systemd", func(r *pfRoot) {
			r.p.systemd = func() detach.Availability {
				return detach.Availability{Reason: "/run/systemd/system is not a directory"}
			}
		}, "systemd"},
		{"no pihole", func(r *pfRoot) { r.remove(v5.PiholeBin) }, "pihole not found"},
		{"no pihole-FTL", func(r *pfRoot) { r.remove(v5.PiholeFTLBin) }, "pihole-FTL not found"},
		{"pihole not executable", func(r *pfRoot) { r.write(v5.PiholeBin, []byte("#!/bin/bash\n"), 0o644) }, "not executable"},
		{"no version source", func(r *pfRoot) { r.remove(v5.VersionsFile) }, "not detected"},
		{"no dnsFile", func(r *pfRoot) { r.remove(defaultDNSFile) }, "does not exist"},
		{"dnsFile is a directory", func(r *pfRoot) {
			r.remove(defaultDNSFile)
			if err := os.Mkdir(r.path(defaultDNSFile), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "is a directory"},
		{"dnsFile is a symlink", func(r *pfRoot) {
			target := r.path("/etc/dnsmasq.d/elsewhere.conf")
			if err := os.Rename(r.path(defaultDNSFile), target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, r.path(defaultDNSFile)); err != nil {
				t.Fatal(err)
			}
		}, "is a symlink"},
		{"v6 without etc_dnsmasq_d", func(r *pfRoot) {
			r.remove(v5.VersionsFile)
			r.write(v5.PiholeTOMLFile, []byte("[misc]\n  etc_dnsmasq_d = false\n"), 0o644)
		}, "etc_dnsmasq_d"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newV5Root(t)
			c.setup(r)
			st := r.p.Preflight(context.Background())
			if st.State != apitypes.ProviderUnavailable {
				t.Fatalf("state %q (details %v), want unavailable", st.State, st.Details)
			}
			if !strings.Contains(st.Reason, c.want) {
				t.Errorf("reason %q does not name the cause %q", st.Reason, c.want)
			}
			if prev, dup := seen[st.Reason]; dup {
				t.Errorf("reason %q is also case %q's", st.Reason, prev)
			}
			seen[st.Reason] = c.name
			if calls := r.runner.calls(); len(calls) != 0 {
				t.Errorf("preflight ran %q", calls)
			}
		})
	}
}

// TC-52 step 1, without the registry: the enabled details of a v5 host.
func TestPreflightV5Details(t *testing.T) {
	t.Parallel()
	r := newV5Root(t)
	coreV, web, ftl := fixtureVersions(t)
	st := r.p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled || st.Reason != "" {
		t.Fatalf("status %+v, want enabled", st)
	}
	want := map[string]any{
		DetailBackend:              spec.BackendV5CLI,
		DetailCoreVersion:          coreV,
		DetailWebVersion:           web,
		DetailFTLVersion:           ftl,
		DetailDNSFile:              defaultDNSFile,
		DetailDNSWrites:            DNSWritesAvailable,
		DetailPreNexusBackupPath:   filepath.Join(r.state, "pihole", "backups", "03-custom-dns-names.conf.pre-nexus"),
		DetailPreNexusBackupExists: false,
	}
	if fmt.Sprint(st.Details) != fmt.Sprint(want) {
		t.Errorf("details\n got %v\nwant %v", st.Details, want)
	}
}

// dnsWrites reports a dnsmasq.conf that does not load the DNS file's
// directory, and stays informational: the provider is still enabled.
func TestPreflightDNSWritesReportsConfDir(t *testing.T) {
	t.Parallel()
	r := newV5Root(t)
	r.write(v5.DnsmasqConfFile, []byte("# conf-dir=/etc/dnsmasq.d\nconf-dir=/etc/other\n"), 0o644)
	st := r.p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled {
		t.Fatalf("status %+v, want enabled", st)
	}
	dw, _ := st.Details[DetailDNSWrites].(string)
	if dw == DNSWritesAvailable || !strings.Contains(dw, "conf-dir") || !strings.Contains(dw, "/etc/dnsmasq.d") {
		t.Errorf("dnsWrites %q, want the conf-dir reason naming /etc/dnsmasq.d", dw)
	}
}

// Configure takes effect on the next preflight: a reloaded dnsFile is the
// one checked and reported (FR-2).
func TestConfigureDNSFileTakesEffect(t *testing.T) {
	t.Parallel()
	r := newV5Root(t)
	const other = "/etc/dnsmasq.d/09-nexus.conf"
	r.write(other, []byte("address=/a.lan/192.168.7.9\n"), 0o644)
	if err := r.p.Configure(func(into any) error {
		into.(*config.PiholeConfig).DNSFile = other
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := r.p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled || st.Details[DetailDNSFile] != other ||
		st.Details[DetailPreNexusBackupPath] != filepath.Join(r.state, "pihole", "backups", "09-nexus.conf.pre-nexus") {
		t.Fatalf("status %+v, want enabled on %s", st, other)
	}
	// A refused section leaves the settings in force.
	if err := r.p.Configure(func(any) error { return errors.New("bad section") }); err == nil {
		t.Fatal("Configure accepted a failing decode")
	}
	if got := r.p.Settings().DNSFile; got != other {
		t.Errorf("dnsFile after a refused section: %q, want %q", got, other)
	}
}

// TC-52: the details are a snapshot as of the last preflight; a backup that
// appears after startup shows only through a refresh (A-53).
func TestTC52_DetailsSnapshotThenBackupAppears(t *testing.T) {
	t.Parallel()
	r := newV5Root(t)
	core0, web, ftl := fixtureVersions(t)
	reg, err := core.New([]provider.Provider{r.p}, core.Options{Host: "h", Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bk := filepath.Join(r.state, "pihole", "backups", "03-custom-dns-names.conf.pre-nexus")

	// Step 1.
	c := capabilityOf(t, reg.Capabilities(ctx, false), spec.Name)
	if c.Status != apitypes.ProviderEnabled {
		t.Fatalf("pihole %+v, want enabled", c)
	}
	d := c.Details
	if d[DetailBackend] != spec.BackendV5CLI || d[DetailCoreVersion] != core0 || d[DetailWebVersion] != web ||
		d[DetailFTLVersion] != ftl || d[DetailDNSFile] != defaultDNSFile || d[DetailDNSWrites] != DNSWritesAvailable ||
		d[DetailPreNexusBackupPath] != bk || d[DetailPreNexusBackupExists] != false {
		t.Fatalf("step 1 details %v", d)
	}

	// Step 2: the backup appears; capabilities without refresh still show
	// the startup snapshot.
	if err := os.MkdirAll(filepath.Dir(bk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bk, pfFixture(t, pfDNSFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := capabilityOf(t, reg.Capabilities(ctx, false), spec.Name).Details; d[DetailPreNexusBackupExists] != false {
		t.Errorf("step 2 preNexusBackupExists %v, want the snapshot's false", d[DetailPreNexusBackupExists])
	}

	// Step 3: refresh re-runs preflight.
	d = capabilityOf(t, reg.Capabilities(ctx, true), spec.Name).Details
	if d[DetailPreNexusBackupExists] != true || d[DetailPreNexusBackupPath] != bk {
		t.Errorf("step 3 details %v, want preNexusBackupExists=true at %s", d, bk)
	}
}

// pfSnapshot records every path under root with its type, size, mode and
// modification time.
func pfSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%v %d %s", fi.Mode(), fi.Size(), fi.ModTime().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// silentFTL accepts connections and never answers; it counts them.
func silentFTL(t *testing.T) (port int, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted = &atomic.Int32{}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return // Closed by the cleanup
			}
			accepted.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
		<-done
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return ln.Addr().(*net.TCPAddr).Port, accepted
}

// TC-53: preflight runs no command, changes no file, and finishes within
// the 1 s startup budget even when FTL is silent and the versions file is
// absent, which is the slowest path through version detection.
func TestTC53_PreflightReadOnlyWithinBudget(t *testing.T) {
	run := func(t *testing.T, r *pfRoot, want apitypes.ProviderState) {
		t.Helper()
		before := pfSnapshot(t, filepath.Dir(r.root))
		start := time.Now()
		st := r.p.Preflight(context.Background())
		elapsed := time.Since(start)
		if st.State != want {
			t.Errorf("status %+v, want %s", st, want)
		}
		if elapsed >= core.DefaultStartupPreflightTimeout {
			t.Errorf("preflight took %v, budget %v", elapsed, core.DefaultStartupPreflightTimeout)
		}
		if calls := r.runner.calls(); len(calls) != 0 {
			t.Errorf("preflight ran %q", calls)
		}
		after := pfSnapshot(t, filepath.Dir(r.root))
		if fmt.Sprint(before) != fmt.Sprint(after) {
			t.Errorf("preflight changed the tree\nbefore %v\n after %v", before, after)
		}
	}
	t.Run("silent FTL, no versions file", func(t *testing.T) {
		r := newV5Root(t)
		r.remove(v5.VersionsFile)
		port, accepted := silentFTL(t)
		r.write(v5.FTLConfFile, fmt.Appendf(nil, "FTLPORT=%d\n", port), 0o644)
		run(t, r, apitypes.ProviderUnavailable)
		if accepted.Load() == 0 {
			t.Error("the FTL >version fallback was not exercised")
		}
	})
	t.Run("enabled v5 host", func(t *testing.T) {
		run(t, newV5Root(t), apitypes.ProviderEnabled)
	})
	t.Run("enabled v6 host", func(t *testing.T) {
		run(t, newV6Root(t, "true"), apitypes.ProviderEnabled)
	})
}

// TC-57: a v6 host that loads /etc/dnsmasq.d is enabled with the
// unsupported backend, and dnsWrites names the unverified v6 write path.
func TestTC57_V6UnsupportedBackend(t *testing.T) {
	r := newV6Root(t, "true")
	reg, err := core.New([]provider.Provider{r.p}, core.Options{Host: "h", Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	c := capabilityOf(t, reg.Capabilities(context.Background(), false), spec.Name)
	if c.Status != apitypes.ProviderEnabled {
		t.Fatalf("pihole %+v, want enabled", c)
	}
	if c.Details[DetailBackend] != spec.BackendUnsupported || c.Details[DetailDNSWrites] != backend.ReasonV6DNSWrites ||
		c.Details[DetailDNSFile] != defaultDNSFile {
		t.Errorf("details %v, want backend %q, dnsWrites %q", c.Details, spec.BackendUnsupported, backend.ReasonV6DNSWrites)
	}
	for _, k := range []string{DetailCoreVersion, DetailWebVersion, DetailFTLVersion} {
		if v, ok := c.Details[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null: no version source names it", k, v, ok)
		}
	}
}

// stubProvider stands in for docker and apt: enabled, with no bindings.
type stubProvider struct{ name string }

func (s stubProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: s.name, ReadScope: s.name + ":read"}
}
func (stubProvider) Configure(func(any) error) error { return nil }
func (stubProvider) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (stubProvider) Bindings() []provider.Binding { return nil }

// TC-55: pihole enabled over a root without the pihole binary is
// unavailable with that reason, and docker and apt stay enabled (NFR-5).
func TestTC55_UnavailablePiholeLeavesOthersEnabled(t *testing.T) {
	t.Parallel()
	r := newV5Root(t)
	r.remove(v5.PiholeBin)
	cfgs := map[string]core.ProviderConfig{"docker": {Enabled: true}, "apt": {Enabled: true}, spec.Name: {Enabled: true}}
	reg, err := core.New([]provider.Provider{stubProvider{"docker"}, stubProvider{"apt"}, r.p},
		core.Options{Host: "h", Providers: cfgs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("startup failed: %v", err)
	}
	for _, refresh := range []bool{false, true} {
		caps := reg.Capabilities(context.Background(), refresh)
		if c := capabilityOf(t, caps, spec.Name); c.Status != apitypes.ProviderUnavailable ||
			!strings.Contains(c.Reason, "pihole not found") {
			t.Errorf("refresh=%v: pihole %+v, want unavailable, \"pihole not found\"", refresh, c)
		}
		for _, name := range []string{"docker", "apt"} {
			if c := capabilityOf(t, caps, name); c.Status != apitypes.ProviderEnabled {
				t.Errorf("refresh=%v: %s %+v, want enabled", refresh, name, c)
			}
		}
	}
	if ok, st := reg.Available(spec.Name); ok || st.State != apitypes.ProviderUnavailable {
		t.Errorf("Available(pihole) = %v %+v, want false, unavailable: calls answer capability-unavailable", ok, st)
	}
}

// TC-56, registry half: with the preflight overridden, pihole is listed
// enabled and every op it binds is served; a reload that disables it
// reports it disabled. MCP tools/list is built from exactly these served
// entries of the providers not disabled (mcpadapter build).
func TestTC56_EnabledThenDisabledByReload(t *testing.T) {
	p := New(Options{Host: "h", Preflight: func(context.Context) provider.Status {
		return provider.Status{State: apitypes.ProviderEnabled}
	}})
	reg, err := core.New([]provider.Provider{p}, core.Options{Host: "h", Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if c := capabilityOf(t, reg.Capabilities(ctx, false), spec.Name); c.Status != apitypes.ProviderEnabled ||
		len(c.ResourceTypes) == 0 {
		t.Fatalf("pihole %+v, want enabled with its resource types", c)
	}
	var served []string
	for _, e := range reg.Entries() {
		if e.Provider == spec.Name {
			served = append(served, e.Binding.Op().ID)
		}
	}
	var bound []string
	for _, b := range p.Bindings() {
		bound = append(bound, b.Op().ID)
	}
	slices.Sort(bound)
	if !slices.Equal(served, bound) {
		t.Errorf("served pihole ops %v, want the bound %v", served, bound)
	}
	if ok, _ := reg.Available(spec.Name); !ok {
		t.Error("pihole not available while enabled")
	}

	if err := reg.Apply(ctx, pfConfigs(false, nil)); err != nil {
		t.Fatal(err)
	}
	if c := capabilityOf(t, reg.Capabilities(ctx, false), spec.Name); c.Status != apitypes.ProviderDisabled {
		t.Errorf("after reload: pihole %+v, want disabled", c)
	}
	if ok, st := reg.Available(spec.Name); ok || st.State != apitypes.ProviderDisabled {
		t.Errorf("after reload: Available %v %+v, want disabled", ok, st)
	}
}
