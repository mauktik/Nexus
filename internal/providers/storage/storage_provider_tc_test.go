// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-110 to TC-STG-122
// (owner WI 4.1), plus the WI 4.1 tests of bounded and of a stuck
// configured path at preflight.
//
// Until the reads and the bindings exist (WIs 4.2 to 4.4 and 5.1), a
// "through the Dispatcher" step is exercised at the gate every storage
// handler calls first, p.require (D-6); the Dispatcher-level assertions of
// TC-STG-114, 116, 117, 119 and 122 are completed by the WIs that bind the
// ops (TC-STG-122's storage.health.get step is below, from WI 4.4).

package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// ---- test support ----

// captureDir is a host capture's directory, gated by capturegate.
func captureDir(t *testing.T, name string) string {
	t.Helper()
	return capturegate.Require(t, filepath.Join("testdata", "captures", name))
}

// captureStdout is a capture file's stdout and exit code.
func captureStdout(t *testing.T, dir, name string) ([]byte, int) {
	t.Helper()
	out, code, err := storagetest.CaptureStdout(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("capture %s: %v", name, err)
	}
	return out, code
}

var statAccess = regexp.MustCompile(`Access: \(([0-7]{4})/`)

// captureFS is the host's root as the capture saw it: each tool `stat`
// that exited 0 is a file of the captured mode (regular files only, as
// the capture's stat reports them), `ls -ln /dev/zfs` that exited 0 a
// device of the listed kind, and each Proxmox marker listing that exited 0
// a directory or file. Everything the capture did not find is absent.
func captureFS(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for file, path := range map[string]string{
		"host/stat-usr-sbin-zpool.txt": "usr/sbin/zpool", "host/stat-sbin-zpool.txt": "sbin/zpool",
		"host/stat-usr-sbin-zfs.txt": "usr/sbin/zfs", "host/stat-sbin-zfs.txt": "sbin/zfs",
	} {
		out, code := captureStdout(t, dir, file)
		if code != 0 {
			continue
		}
		m := statAccess.FindSubmatch(out)
		if m == nil || !bytes.Contains(out, []byte("regular file")) {
			t.Fatalf("%s: not a regular file with an Access mode:\n%s", file, out)
		}
		mode, err := strconv.ParseUint(string(m[1]), 8, 32)
		if err != nil {
			t.Fatalf("%s: mode %s: %v", file, m[1], err)
		}
		fsys[path] = &fstest.MapFile{Mode: fs.FileMode(mode)}
	}
	if out, code := captureStdout(t, dir, "host/ls-dev-zfs.txt"); code == 0 {
		mode := fs.FileMode(0o666)
		switch out[0] {
		case 'c':
			mode |= fs.ModeDevice | fs.ModeCharDevice
		case 'b':
			mode |= fs.ModeDevice
		}
		fsys[devZFS] = &fstest.MapFile{Mode: mode}
	}
	for file, path := range map[string]string{
		"host/ls-etc-pve.txt": "etc/pve", "host/ls-pve-manager-list.txt": "var/lib/dpkg/info/pve-manager.list",
	} {
		if out, code := captureStdout(t, dir, file); code == 0 {
			mode := fs.FileMode(0o644)
			if out[0] == 'd' {
				mode = fs.ModeDir | 0o755
			}
			fsys[path] = &fstest.MapFile{Mode: mode}
		}
	}
	return fsys
}

// homeFS is the home-server capture's root.
func homeFS(t *testing.T) fstest.MapFS { return captureFS(t, captureDir(t, "home-server")) }

// records is a slog handler that keeps every record.
type records struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec.Clone())
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

func (r *records) all() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs)
}

func (r *records) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = nil
}

// withMessage are the records with message msg.
func (r *records) withMessage(msg string) []slog.Record {
	var out []slog.Record
	for _, rec := range r.all() {
		if rec.Message == msg {
			out = append(out, rec)
		}
	}
	return out
}

// attrs flattens a record's attributes, groups joined with ".".
func attrs(rec slog.Record) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			for _, g := range a.Value.Group() {
				walk(prefix+a.Key+".", g)
			}
			return
		}
		out[prefix+a.Key] = a.Value.Any()
	}
	rec.Attrs(func(a slog.Attr) bool { walk("", a); return true })
	return out
}

// clock is F-CLOCK: a settable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// newTestProvider is a linux provider on fsys and run, logging to a
// records handler.
func newTestProvider(fsys fs.FS, run zfs.Runner, mod ...func(*Options)) (*Provider, *records) {
	rec := &records{}
	o := Options{Host: "test", Logger: slog.New(rec), GOOS: "linux", FS: fsys, Run: run}
	for _, m := range mod {
		m(&o)
	}
	return New(o), rec
}

// configure runs Configure with the defaults changed by mod.
func configure(t *testing.T, p *Provider, mod func(*config.StorageConfig)) {
	t.Helper()
	if err := p.Configure(func(into any) error {
		c := into.(*config.StorageConfig)
		c.Enabled = true
		if mod != nil {
			mod(c)
		}
		return nil
	}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

// setCommandTimeout sets commandTimeout through Settings, below the config
// minimum when a test needs it (test-strategy.md, Required test seams).
func setCommandTimeout(p *Provider, d time.Duration) {
	s := p.Settings()
	s.CommandTimeout = d
	p.settings.Store(&s)
}

// noRun is a runner that fails the test: nothing may be run.
func noRun(t *testing.T) zfs.Runner {
	return func(_ context.Context, argv []string) (zfs.Result, error) {
		t.Errorf("unexpected run of %q", argv)
		return zfs.Result{}, zfs.ErrNotStarted
	}
}

// preflight runs Preflight with a timeout of d.
func preflight(p *Provider, d time.Duration) provider.Status {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return p.Preflight(ctx)
}

func feats(t *testing.T, st provider.Status) map[string]featureDetail {
	t.Helper()
	f, ok := st.Details[DetailFeatures].(map[string]featureDetail)
	if !ok || len(f) != 3 {
		t.Fatalf("details.features = %#v, want the three features", st.Details[DetailFeatures])
	}
	return f
}

// detailsJSON is the details as capabilities shows them.
func detailsJSON(t *testing.T, st provider.Status) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(st.Details)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	return m
}

// problemOf is err's problem, failing the test unless it is a 503
// capability-unavailable.
func problemOf(t *testing.T, err error) *problem.Error {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.CapabilityUnavailable || pe.Status != 503 {
		t.Fatalf("err = %v, want a 503 capability-unavailable problem", err)
	}
	return pe
}

func countArgv(reads [][]string, argv []string) int {
	n := 0
	for _, r := range reads {
		if slices.Equal(r, argv) {
			n++
		}
	}
	return n
}

// fsWith is fsys with extra entries.
func fsWith(fsys fstest.MapFS, extra fstest.MapFS) fstest.MapFS {
	out := fstest.MapFS{}
	for k, v := range fsys {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// statErrFS answers Stat of one name with err.
type statErrFS struct {
	fstest.MapFS
	name string
	err  error
}

func (f statErrFS) Stat(name string) (fs.FileInfo, error) {
	if name == f.name {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: f.err}
	}
	return f.MapFS.Stat(name)
}

// dirs makes a directory, a regular file and a missing path in a temp dir.
func dirs(t *testing.T) (dir, file, missing string) {
	t.Helper()
	base := storagetest.TempDir(t)
	storagetest.Build(t, base, storagetest.Node{Path: "dir", Type: storagetest.Dir},
		storagetest.Node{Path: "file", Content: []byte("x")})
	return filepath.Join(base, "dir"), filepath.Join(base, "file"), filepath.Join(base, "missing")
}

// ---- TC-STG-110 to TC-STG-122 ----

// TC-STG-110: preflight on pihole and Public-Nginx captures reports every feature's reason. Owner WI 4.1. Requirements: FR-3, NFR-7, CON-3.
//
// The pihole and public-nginx subtests skip until their captures are
// committed (capturegate); the no-tools subtest runs the same assertions on
// a root with no tool, no /dev/zfs and no marker.
func TestTCSTG110_PreflightOnPiholeAndPublicNginxCaptures(t *testing.T) {
	const want = "zfs: /usr/sbin/zpool not found (nor /sbin/zpool); backups: not configured; timeMachine: not configured"
	check := func(t *testing.T, fsys fs.FS, fake *storagetest.Fake) {
		p, _ := newTestProvider(fsys, fake.Runner())
		configure(t, p, nil)
		st := preflight(p, time.Second)
		if st.State != apitypes.ProviderUnavailable || st.Reason != want {
			t.Fatalf("Preflight = %s %q, want unavailable %q", st.State, st.Reason, want)
		}
		f := feats(t, st)
		for name, reason := range map[string]string{
			featureZFS: "zfs: /usr/sbin/zpool not found (nor /sbin/zpool)", featureBackups: "not configured",
			featureTimeMachine: "not configured",
		} {
			if f[name] != (featureDetail{Available: false, Reason: reason}) {
				t.Errorf("features.%s = %+v, want unavailable %q", name, f[name], reason)
			}
		}
		d := detailsJSON(t, st)
		if string(d[DetailZFSVersion]) != "null" || string(d[DetailZFSVersionReason]) == `""` || d[DetailZFSVersionReason] == nil {
			t.Errorf("zfsVersion %s with reason %s, want null with a reason", d[DetailZFSVersion], d[DetailZFSVersionReason])
		}
		if string(d[DetailPools]) != "[]" {
			t.Errorf("pools = %s, want []", d[DetailPools])
		}
		if r := fake.Reads(); len(r) != 0 || len(fake.Writes()) != 0 {
			t.Errorf("F-ZFS calls %q, want none", r)
		}
	}
	for _, host := range []string{"pihole", "public-nginx"} {
		t.Run(host, func(t *testing.T) {
			dir := captureDir(t, host)
			check(t, captureFS(t, dir), storagetest.NewFake(t, host))
		})
	}
	t.Run("no-tools", func(t *testing.T) {
		check(t, fstest.MapFS{"etc/hostname": {}}, storagetest.NewFake(t, "home-server"))
	})
}

// TC-STG-111: preflight on the home-server capture reports zfs available with its details. Owner WI 4.1. Requirements: FR-3.
func TestTCSTG111_PreflightOnTheHomeServerCaptureReports(t *testing.T) {
	dir := captureDir(t, "home-server")
	fake := storagetest.NewFake(t, "home-server")
	fsys := captureFS(t, dir)
	vz, tm := storagetest.TempDir(t), storagetest.TempDir(t)
	p, rec := newTestProvider(fsys, fake.Runner())
	configure(t, p, func(c *config.StorageConfig) {
		c.Scrub = config.StorageScrubConfig{Enabled: true, Pools: []string{"z1", "gone"}}
		c.Snapshots.Enabled = true
		c.Snapshots.Datasets = []config.StorageSnapshotDataset{{Name: "z1/ds/share"}, {Name: "z9/x", Recursive: true}}
		c.Backups.VzdumpPaths = []string{vz}
		c.TimeMachine.Paths = []string{tm}
	})
	st := preflight(p, 5*time.Second)
	if st.State != apitypes.ProviderEnabled {
		t.Fatalf("Preflight = %s %q, want enabled", st.State, st.Reason)
	}
	for name, fd := range feats(t, st) {
		if !fd.Available || fd.Reason != "" {
			t.Errorf("features.%s = %+v, want available", name, fd)
		}
	}

	// The oracle: the capture's own zfs version and zpool list lines.
	ver, _ := captureStdout(t, dir, "zfs/zfs-version.txt")
	var userland, kernel string
	for _, l := range strings.Split(strings.TrimSpace(string(ver)), "\n") {
		if k, ok := strings.CutPrefix(l, "zfs-kmod-"); ok {
			kernel = k
		} else if u, ok := strings.CutPrefix(l, "zfs-"); ok {
			userland = u
		}
	}
	wantVersion := versionDetail{Userland: userland, Kernel: kernel, Format: "json"}
	if v, ok := st.Details[DetailZFSVersion].(*versionDetail); !ok || v == nil || *v != wantVersion {
		t.Errorf("zfsVersion = %#v, want %+v", st.Details[DetailZFSVersion], wantVersion)
	}
	names, _ := captureStdout(t, dir, "zfs/zpool-list-names.txt")
	wantPools := strings.Fields(string(names))
	if got := st.Details[DetailPools]; !slices.Equal(got.([]string), wantPools) {
		t.Errorf("pools = %v, want %v", got, wantPools)
	}
	if st.Details[DetailZpoolPath] != "/usr/sbin/zpool" || st.Details[DetailZfsPath] != "/usr/sbin/zfs" {
		t.Errorf("zpoolPath %v, zfsPath %v, want /usr/sbin/zpool and /usr/sbin/zfs",
			st.Details[DetailZpoolPath], st.Details[DetailZfsPath])
	}
	wantScrub := scrubControl{Enabled: true, Pools: []poolControl{{"z1", controlOK}, {"gone", controlNotImported}}}
	if got := st.Details[DetailScrubControl].(scrubControl); got.Enabled != wantScrub.Enabled || !slices.Equal(got.Pools, wantScrub.Pools) {
		t.Errorf("scrubControl = %+v, want %+v", got, wantScrub)
	}
	wantSnap := snapshotControl{Enabled: true, MaxPoolCapacityPercent: spec.DefaultMaxPoolCapacityPercent,
		Datasets: []datasetControl{{"z1/ds/share", false, controlOK}, {"z9/x", true, controlNotFound}}}
	if got := st.Details[DetailSnapshotControl].(snapshotControl); got.Enabled != wantSnap.Enabled ||
		got.MaxPoolCapacityPercent != wantSnap.MaxPoolCapacityPercent || !slices.Equal(got.Datasets, wantSnap.Datasets) {
		t.Errorf("snapshotControl = %+v, want %+v", got, wantSnap)
	}
	if len(fake.Writes()) != 0 {
		t.Errorf("writes %q, want none", fake.Writes())
	}
	d := detailsJSON(t, st)
	if !bytes.Contains(d[DetailZFSVersion], []byte(`"format":"json"`)) {
		t.Errorf("zfsVersion JSON = %s", d[DetailZFSVersion])
	}
	if n := len(rec.withMessage("storage preflight")); n != 1 {
		t.Errorf("%d storage preflight records, want 1", n)
	}
}

// TC-STG-112: tools present but no /dev/zfs, or a /dev/zfs that is not a character device. Owner WI 4.1. Requirements: FR-3.
func TestTCSTG112_ToolsPresentButNoDevZfsOr(t *testing.T) {
	home := homeFS(t)
	tools := fstest.MapFS{}
	for k, v := range home {
		if strings.Contains(k, "sbin/") {
			tools[k] = v
		}
	}
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
		want *regexp.Regexp
	}{
		{"absent", tools, regexp.MustCompile(`^` + regexp.QuoteMeta(reasonNoDevZFS) + `$`)},
		{"regular-file", fsWith(tools, fstest.MapFS{devZFS: {Mode: 0o644}}),
			regexp.MustCompile(`^zfs: /dev/zfs is not a character device \(mode -rw-r--r--\)$`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := storagetest.NewFake(t, "home-server")
			p, _ := newTestProvider(tc.fsys, fake.Runner())
			configure(t, p, nil)
			st := preflight(p, time.Second)
			if got := feats(t, st)[featureZFS]; got.Available || !tc.want.MatchString(got.Reason) {
				t.Errorf("features.zfs = %+v, want unavailable matching %s", got, tc.want)
			}
			if r := fake.Reads(); len(r) != 0 {
				t.Errorf("F-ZFS reads %q, want none", r)
			}
		})
	}
}

// TC-STG-113: zpool list failing or hanging at preflight makes zfs unavailable with an exact, bounded reason. Owner WI 4.1. Requirements: FR-3, NFR-2.
func TestTCSTG113_ZpoolListFailingOrHangingAtPreflight(t *testing.T) {
	fsys := homeFS(t)
	line := strings.Repeat("e", 300)
	t.Run("exit-1", func(t *testing.T) {
		fake := storagetest.NewFake(t, "home-server")
		names := fake.Argv(zfs.CmdPoolNames)
		run := func(ctx context.Context, argv []string) (zfs.Result, error) {
			if slices.Equal(argv, names) {
				return zfs.Result{Stderr: []byte(line + "\nsecond line\n"), ExitCode: 1}, nil
			}
			return fake.Run(ctx, argv)
		}
		want := "zfs: zpool list exited 1: " + line[:200]
		p, _ := newTestProvider(fsys, run)
		configure(t, p, nil)
		st := preflight(p, time.Second)
		if got := feats(t, st)[featureZFS]; got.Available || got.Reason != want {
			t.Errorf("features.zfs = %+v, want unavailable %q", got, want)
		}
		if st.State != apitypes.ProviderUnavailable || st.Reason != want+"; backups: not configured; timeMachine: not configured" {
			t.Errorf("Preflight = %s %q, want unavailable naming every feature", st.State, st.Reason)
		}

		vz := storagetest.TempDir(t)
		configure(t, p, func(c *config.StorageConfig) { c.Backups.VzdumpPaths = []string{vz} })
		st = preflight(p, time.Second)
		f := feats(t, st)
		if st.State != apitypes.ProviderEnabled || f[featureZFS].Available || !f[featureBackups].Available {
			t.Errorf("with a backups path: %s, features %+v; want enabled, zfs unavailable, backups available", st.State, f)
		}
	})
	t.Run("hang", func(t *testing.T) {
		fake := storagetest.NewFake(t, "home-server")
		fake.Hang(fake.Argv(zfs.CmdPoolNames))
		p, _ := newTestProvider(fsys, fake.Runner())
		configure(t, p, nil)
		setCommandTimeout(p, 2*time.Second)
		start := time.Now()
		st := preflight(p, time.Second)
		if el := time.Since(start); el > time.Second+100*time.Millisecond {
			t.Errorf("Preflight took %s, want at most 1.1s", el)
		}
		const want = "zfs: zpool list did not finish within 1s"
		if got := feats(t, st)[featureZFS]; got.Available || got.Reason != want {
			t.Errorf("features.zfs = %+v, want unavailable %q", got, want)
		}
		if st.State != apitypes.ProviderUnavailable {
			t.Errorf("Preflight = %s, want unavailable with no path configured", st.State)
		}
	})
}

// TC-STG-114: zero imported pools is available, not an error. Owner WI 4.1. Requirements: FR-3, FR-4.
//
// The storage.pools.list step (200 with items: []) is WI 4.2's handler,
// bound in WI 5.1; here the gate it calls first lets it through.
func TestTCSTG114_ZeroImportedPoolsIsAvailableNotAn(t *testing.T) {
	fake := storagetest.NewFake(t, "home-server")
	names := fake.Argv(zfs.CmdPoolNames)
	run := func(ctx context.Context, argv []string) (zfs.Result, error) {
		if slices.Equal(argv, names) {
			return zfs.Result{}, nil // exit 0, no output: no pool imported
		}
		return fake.Run(ctx, argv)
	}
	p, _ := newTestProvider(homeFS(t), run)
	configure(t, p, nil)
	st := preflight(p, time.Second)
	if got := feats(t, st)[featureZFS]; !got.Available {
		t.Fatalf("features.zfs = %+v, want available", got)
	}
	if st.State != apitypes.ProviderEnabled {
		t.Errorf("Preflight = %s %q, want enabled", st.State, st.Reason)
	}
	if d := detailsJSON(t, st); string(d[DetailPools]) != "[]" {
		t.Errorf("pools = %s, want []", d[DetailPools])
	}
	if err := p.require(context.Background(), featureZFS); err != nil {
		t.Errorf("require(zfs) = %v, want nil (the read answers, not 503)", err)
	}
}

// TC-STG-115: backups and timeMachine availability per configured path. Owner WI 4.1. Requirements: FR-3, FR-9, FR-11, CON-7.
func TestTCSTG115_BackupsAndTimeMachineAvailabilityPerConfiguredPath(t *testing.T) {
	dir, file, missing := dirs(t)
	for _, tc := range []struct {
		feature string
		set     func(*config.StorageConfig, []string)
	}{
		{featureBackups, func(c *config.StorageConfig, p []string) { c.Backups.VzdumpPaths = p }},
		{featureTimeMachine, func(c *config.StorageConfig, p []string) { c.TimeMachine.Paths = p }},
	} {
		t.Run(tc.feature, func(t *testing.T) {
			p, _ := newTestProvider(fstest.MapFS{}, noRun(t))
			configure(t, p, func(c *config.StorageConfig) { tc.set(c, []string{file, dir}) })
			if got := feats(t, preflight(p, time.Second))[tc.feature]; !got.Available {
				t.Errorf("[file, dir]: %s = %+v, want available", tc.feature, got)
			}
			configure(t, p, func(c *config.StorageConfig) { tc.set(c, []string{file, missing}) })
			want := file + ": not a directory; " + missing + ": no such file or directory"
			if got := feats(t, preflight(p, time.Second))[tc.feature]; got.Available || got.Reason != want {
				t.Errorf("[file, missing]: %s = %+v, want unavailable %q", tc.feature, got, want)
			}
		})
	}
}

// TC-STG-116: a zfs version parse failure leaves zfsVersion null and selects the text format. Owner WI 4.1. Requirements: FR-3, FR-4.
//
// The values of the pools read (TC-STG-125's) are WI 4.2's; here the
// selected format builds the -H -p argv, which the fake answers.
func TestTCSTG116_AZfsVersionParseFailureLeavesZfsVersion(t *testing.T) {
	fake := storagetest.NewFake(t, "home-server")
	version := fake.Argv(zfs.CmdVersion)
	run := func(ctx context.Context, argv []string) (zfs.Result, error) {
		if slices.Equal(argv, version) {
			return zfs.Result{Stdout: []byte("garbage\n")}, nil
		}
		return fake.Run(ctx, argv)
	}
	p, _ := newTestProvider(homeFS(t), run)
	configure(t, p, nil)
	st := preflight(p, time.Second)
	if got := feats(t, st)[featureZFS]; !got.Available {
		t.Fatalf("features.zfs = %+v, want available", got)
	}
	d := detailsJSON(t, st)
	if string(d[DetailZFSVersion]) != "null" {
		t.Errorf("zfsVersion = %s, want null", d[DetailZFSVersion])
	}
	if r, _ := st.Details[DetailZFSVersionReason].(string); r == "" {
		t.Errorf("zfsVersionReason = %q, want the parse error", r)
	}
	z := p.zfs.Load()
	if z == nil || z.format != zfs.Text {
		t.Fatalf("format = %v, want text", z)
	}
	argv := zfs.Argv(z.paths, z.format, zfs.CmdPoolList)
	if !slices.Contains(argv, "-H") || !slices.Contains(argv, "-p") || slices.Contains(argv, "-j") {
		t.Errorf("pools argv %q, want the -H -p form", argv)
	}
	res, err := z.tools.Run(context.Background(), argv)
	if err != nil || res.ExitCode != 0 || countArgv(fake.Reads(), argv) != 1 {
		t.Errorf("text pools read: %v exit %d, reads %q", err, res.ExitCode, fake.Reads())
	}
}

// TC-STG-117: one info line per preflight and none per request. Owner WI 4.1. Requirements: NFR-7.
//
// Step 2 calls the gate of each of the nine reads (health.get gates each
// feature, DF-5), 5 times each, before and after the 5 s re-check age, then
// each read 5 times through the Dispatcher on the real bindings (WI 5.1).
func TestTCSTG117_OneInfoLinePerPreflightAndNone(t *testing.T) {
	reads := map[string][]string{
		"storage.health.get": featureNames, "storage.pools.list": {featureZFS}, "storage.pools.get": {featureZFS},
		"storage.datasets.list": {featureZFS}, "storage.datasets.get": {featureZFS},
		"storage.snapshots.list": {featureZFS}, "storage.backups.list": {featureBackups},
		"storage.backups.summary": {featureBackups}, "storage.timemachine.list": {featureTimeMachine},
	}
	check := func(t *testing.T, fsys fs.FS, fake *storagetest.Fake) {
		clk := &clock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
		p, rec := newTestProvider(fsys, fake.Runner(), func(o *Options) { o.Now = clk.Now })
		configure(t, p, nil)
		preflight(p, time.Second)
		preflight(p, time.Second)
		all := rec.all()
		if len(all) != 2 {
			t.Fatalf("%d records, want 2", len(all))
		}
		for _, r := range all {
			a := attrs(r)
			if r.Message != "storage preflight" || r.Level != slog.LevelInfo {
				t.Errorf("record %q at %s, want storage preflight at info", r.Message, r.Level)
			}
			for _, f := range featureNames {
				if a[f+".available"] != false || a[f+".reason"] == "" || a[f+".reason"] == nil {
					t.Errorf("record attrs %v: want %s.available false with a reason", a, f)
				}
			}
		}
		rec.reset()
		for _, advance := range []time.Duration{0, 6 * time.Second} {
			clk.set(clk.Now().Add(advance))
			for _, gates := range reads {
				for range 5 {
					for _, f := range gates {
						problemOf(t, p.require(context.Background(), f))
					}
				}
			}
		}
		if r := rec.all(); len(r) != 0 {
			t.Errorf("%d records from the reads, want none: %v", len(r), r)
		}

		// Step 2 through the Dispatcher, on the real bindings (WI 5.1):
		// each read answers 503 and logs nothing.
		reg, err := core.New([]provider.Provider{p}, core.Options{Host: "tc", Version: "v0.0.0-test", Logger: discard,
			Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true, Decode: func(into any) error {
				into.(*config.StorageConfig).Enabled = true
				return nil
			}}}})
		if err != nil {
			t.Fatalf("core.New: %v", err)
		}
		d := core.NewDispatcher(reg, core.DispatcherOptions{Logger: discard})
		rec.reset()
		inputs := map[string]string{"storage.pools.get": `{"name":"z1"}`, "storage.datasets.get": `{"name":"z1/ds"}`}
		called := 0
		for _, op := range spec.Spec().Ops {
			if _, ok := reads[op.ID]; !ok {
				continue
			}
			in := inputs[op.ID]
			if in == "" {
				in = "{}"
			}
			for range 5 {
				_, err := d.Invoke(context.Background(), core.InvokeRequest{
					Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-117", Scopes: []string{spec.ReadScope}},
					Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(in),
				})
				problemOf(t, err)
				called++
			}
		}
		if called != 45 {
			t.Errorf("%d Dispatcher calls, want 45 (nine reads, 5 times each)", called)
		}
		if r := rec.all(); len(r) != 0 {
			t.Errorf("%d records from the Dispatcher calls, want none: %v", len(r), r)
		}
	}
	t.Run("pihole", func(t *testing.T) {
		dir := captureDir(t, "pihole")
		check(t, captureFS(t, dir), storagetest.NewFake(t, "pihole"))
	})
	t.Run("no-zfs", func(t *testing.T) {
		check(t, fstest.MapFS{"etc/hostname": {}}, storagetest.NewFake(t, "home-server"))
	})
}

// TC-STG-118: an unavailable feature is re-checked on call after 5 s, once for concurrent callers. Owner WI 4.1. Requirements: FR-3, NFR-7.
func TestTCSTG118_AnUnavailableFeatureIsReCheckedOn(t *testing.T) {
	if recheckInterval != core.RecheckInterval {
		t.Fatalf("recheckInterval %s, want core.RecheckInterval %s", recheckInterval, core.RecheckInterval)
	}
	fake := storagetest.NewFake(t, "home-server")
	names := fake.Argv(zfs.CmdPoolNames)
	var calls atomic.Int32
	var hang atomic.Bool
	hang.Store(true)
	hungDone := make(chan struct{})
	run := func(ctx context.Context, argv []string) (zfs.Result, error) {
		if slices.Equal(argv, names) {
			calls.Add(1)
			if hang.Load() {
				<-ctx.Done()
				defer close(hungDone)
				return zfs.Result{}, ctx.Err()
			}
		}
		return fake.Run(ctx, argv)
	}
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	clk := &clock{now: t0}
	p, rec := newTestProvider(homeFS(t), run, func(o *Options) { o.Now = clk.Now })
	configure(t, p, nil)
	setCommandTimeout(p, 300*time.Millisecond)

	st := preflight(p, 200*time.Millisecond)
	const reason = "zfs: zpool list did not finish within 200ms"
	if got := feats(t, st)[featureZFS]; got.Available || got.Reason != reason {
		t.Fatalf("features.zfs = %+v, want unavailable %q", got, reason)
	}
	<-hungDone // the hung zpool list's flight has ended
	hang.Store(false)

	clk.set(t0.Add(4 * time.Second))
	pe := problemOf(t, p.require(context.Background(), featureZFS))
	if pe.Extensions["reason"] != reason || !strings.Contains(pe.Detail, reason) {
		t.Errorf("problem %q %v, want the preflight reason", pe.Detail, pe.Extensions)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d zpool list calls at T0+4s, want 1 (no re-check)", n)
	}

	clk.set(t0.Add(6 * time.Second))
	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.require(context.Background(), featureZFS)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v, want nil (200)", i, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d zpool list calls, want 2 (one re-check)", n)
	}
	changes := rec.withMessage("storage feature availability changed")
	if len(changes) != 1 {
		t.Fatalf("%d availability records, want 1", len(changes))
	}
	if a := attrs(changes[0]); changes[0].Level != slog.LevelInfo || a["feature"] != featureZFS || a["available"] != true {
		t.Errorf("availability record %s %v, want info zfs available", changes[0].Level, a)
	}
}

// TC-STG-119: an unavailable feature answers 503 and leaves the other features serving. Owner WI 4.1. Requirements: FR-3, NFR-7.
func TestTCSTG119_AnUnavailableFeatureAnswers503AndLeaves(t *testing.T) {
	_, file, _ := dirs(t)
	fake := storagetest.NewFake(t, "home-server")
	p, _ := newTestProvider(homeFS(t), fake.Runner())
	configure(t, p, func(c *config.StorageConfig) { c.Backups.VzdumpPaths = []string{file} })
	if st := preflight(p, 5*time.Second); st.State != apitypes.ProviderEnabled {
		t.Fatalf("Preflight = %s %q, want enabled", st.State, st.Reason)
	}
	ctx := context.Background()
	for _, op := range []string{"storage.pools.list", "storage.datasets.list"} {
		if err := p.require(ctx, featureZFS); err != nil {
			t.Errorf("%s: %v, want served", op, err)
		}
	}
	backupsReason := file + ": not a directory"
	for _, op := range []string{"storage.backups.list", "storage.backups.summary"} {
		pe := problemOf(t, p.require(ctx, featureBackups))
		if !strings.Contains(pe.Detail, backupsReason) || pe.Extensions["provider"] != spec.Name {
			t.Errorf("%s: problem %q %v, want the backups reason %q", op, pe.Detail, pe.Extensions, backupsReason)
		}
	}
	if pe := problemOf(t, p.require(ctx, featureTimeMachine)); !strings.Contains(pe.Detail, "not configured") {
		t.Errorf("storage.timemachine.list: problem %q, want not configured", pe.Detail)
	}
}

// TC-STG-120: non-Linux GOOS reports every feature unavailable and the package builds there. Owner WI 4.1. Requirements: FR-3, CON-3.
func TestTCSTG120_NonLinuxGOOSReportsEveryFeatureUnavailable(t *testing.T) {
	fake := storagetest.NewFake(t, "home-server")
	dir, _, _ := dirs(t)
	p, _ := newTestProvider(homeFS(t), fake.Runner(), func(o *Options) { o.GOOS = "darwin" })
	configure(t, p, func(c *config.StorageConfig) {
		c.Backups.VzdumpPaths = []string{dir}
		c.TimeMachine.Paths = []string{dir}
	})
	st := preflight(p, time.Second)
	if st.State != apitypes.ProviderUnavailable || st.Reason != reasonNotLinux {
		t.Errorf("Preflight = %s %q, want unavailable %q", st.State, st.Reason, reasonNotLinux)
	}
	for name, fd := range feats(t, st) {
		if fd.Available || fd.Reason != reasonNotLinux {
			t.Errorf("features.%s = %+v, want unavailable %q", name, fd, reasonNotLinux)
		}
	}
	if r := fake.Reads(); len(r) != 0 {
		t.Errorf("F-ZFS reads %q, want none", r)
	}
	if n := p.fl.count(); n != 0 {
		t.Errorf("%d path flights, want none", n)
	}
}

// TC-STG-121: a Proxmox VE node is refused, and an unreadable marker fails closed. Owner WI 4.1. Requirements: FR-20, CON-6.
func TestTCSTG121_AProxmoxVENodeIsRefusedAnd(t *testing.T) {
	if want := []string{"/etc/pve", "/var/lib/dpkg/info/pve-manager.list"}; !slices.Equal(proxmoxMarkers, want) {
		t.Fatalf("proxmoxMarkers = %q, want the services branch's %q", proxmoxMarkers, want)
	}
	home := homeFS(t)
	for _, tc := range []struct {
		name string
		fsys fs.FS
		want *regexp.Regexp
	}{
		{"etc-pve", fsWith(home, fstest.MapFS{"etc/pve": {Mode: fs.ModeDir | 0o755}}),
			regexp.MustCompile(`^` + regexp.QuoteMeta(reasonProxmox) + `$`)},
		{"pve-manager-list", fsWith(home, fstest.MapFS{"var/lib/dpkg/info/pve-manager.list": {}}),
			regexp.MustCompile(`^` + regexp.QuoteMeta(reasonProxmox) + `$`)},
		{"unreadable", statErrFS{MapFS: home, name: "etc/pve", err: fs.ErrPermission},
			regexp.MustCompile(`/etc/pve.*permission denied`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := storagetest.NewFake(t, "home-server")
			dir, _, _ := dirs(t)
			p, _ := newTestProvider(tc.fsys, fake.Runner())
			configure(t, p, func(c *config.StorageConfig) { c.Backups.VzdumpPaths = []string{dir} })
			st := preflight(p, time.Second)
			if st.State != apitypes.ProviderUnavailable || !tc.want.MatchString(st.Reason) {
				t.Errorf("Preflight = %s %q, want unavailable matching %s", st.State, st.Reason, tc.want)
			}
			for name, fd := range feats(t, st) {
				if fd.Available || fd.Reason != st.Reason {
					t.Errorf("features.%s = %+v, want unavailable with the provider's reason", name, fd)
				}
			}
			if r := fake.Reads(); len(r) != 0 {
				t.Errorf("F-ZFS reads %q, want none", r)
			}
		})
	}
}

// TC-STG-122: Configure swaps settings atomically; a request in flight keeps its snapshot. Owner WI 4.1. Requirements: FR-1.
//
// The request in flight here is a preflight blocked in zpool list; the
// thresholds of storage.health.get (WI 4.4) are checked by that WI on the
// same snapshot rule.
func TestTCSTG122_ConfigureSwapsSettingsAtomicallyARequestIn(t *testing.T) {
	fake := storagetest.NewFake(t, "home-server")
	names := fake.Argv(zfs.CmdPoolNames)
	release := make(chan struct{})
	fake.Stuck(names, release)
	tm := storagetest.TempDir(t)
	p, _ := newTestProvider(homeFS(t), fake.Runner())
	configure(t, p, func(c *config.StorageConfig) {
		c.Scrub = config.StorageScrubConfig{Enabled: true, Pools: []string{"z1"}}
	})
	setCommandTimeout(p, 10*time.Second)

	done := make(chan provider.Status, 1)
	go func() { done <- preflight(p, 10*time.Second) }()
	deadline := time.Now().Add(5 * time.Second)
	for countArgv(fake.Reads(), names) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the preflight never reached zpool list")
		}
		time.Sleep(time.Millisecond)
	}
	configure(t, p, func(c *config.StorageConfig) {
		c.Scrub = config.StorageScrubConfig{Enabled: false, Pools: []string{"z2"}}
		c.Snapshots.MaxPoolCapacityPercent = 50
		c.TimeMachine.Paths = []string{tm}
	})
	if s := p.Settings(); s.Scrub.Pools[0] != "z2" || s.Snapshots.MaxPoolCapacityPercent != 50 {
		t.Fatalf("Settings after Configure = %+v, want the new section", s)
	}
	close(release)
	st := <-done
	if got := st.Details[DetailScrubControl].(scrubControl); !got.Enabled || !slices.Equal(got.Pools, []poolControl{{"z1", controlOK}}) {
		t.Errorf("in-flight scrubControl = %+v, want the old snapshot's z1", got)
	}
	if got := st.Details[DetailSnapshotControl].(snapshotControl); got.MaxPoolCapacityPercent != spec.DefaultMaxPoolCapacityPercent {
		t.Errorf("in-flight maxPoolCapacityPercent = %d, want the old %d", got.MaxPoolCapacityPercent, spec.DefaultMaxPoolCapacityPercent)
	}
	if got := feats(t, st)[featureTimeMachine]; got.Available || got.Reason != reasonNotConf {
		t.Errorf("in-flight timeMachine = %+v, want not configured (old snapshot)", got)
	}

	st = preflight(p, 5*time.Second)
	if got := st.Details[DetailScrubControl].(scrubControl); got.Enabled || !slices.Equal(got.Pools, []poolControl{{"z2", controlOK}}) {
		t.Errorf("next scrubControl = %+v, want the new z2", got)
	}
	if got := st.Details[DetailSnapshotControl].(snapshotControl); got.MaxPoolCapacityPercent != 50 {
		t.Errorf("next maxPoolCapacityPercent = %d, want 50", got.MaxPoolCapacityPercent)
	}
	if got := feats(t, st)[featureTimeMachine]; !got.Available {
		t.Errorf("next timeMachine = %+v, want available", got)
	}
}

// TC-STG-122, carried forward to WI 4.4: storage.health.get evaluates with
// the thresholds of its start while a Configure lands mid-request, and the
// next request with the new ones.
func TestTCSTG122_StorageHealthGetKeepsTheThresholdsOfItsStart(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	s := newHStack(t, hopts{zfs: true, timeout: 10 * time.Second,
		setup: func(f *storagetest.Fake, _ *overlay) { f.Stuck(f.Argv(zfs.CmdPoolStatus), release) }})
	if w := s.p.Settings().Thresholds.PoolCapacityWarn; w != 80 {
		t.Fatalf("poolCapacityWarn %d, want 80", w)
	}
	status := s.fake.Argv(zfs.CmdPoolStatus)
	type answer struct {
		res core.InvokeResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := s.d.Invoke(context.Background(), core.InvokeRequest{
			Caller:  auth.Principal{TokenID: "a1b2c3d4e5f6", Name: "tc-stg-122", Scopes: []string{spec.ReadScope}},
			Surface: core.SurfaceREST, Op: spec.HealthGet.Op(), Input: json.RawMessage(`{}`),
		})
		done <- answer{res, err}
	}()
	waitFor(t, func() bool { return countArgv(s.fake.Reads(), status) == 1 })
	configure(t, s.p, func(c *config.StorageConfig) { c.Thresholds.PoolCapacityWarn = 1 })
	once.Do(func() { close(release) })
	a := <-done
	if a.err != nil {
		t.Fatal(a.err)
	}
	h := a.res.Body.(apitypes.List[spec.Health]).Items[0].Properties

	var want []sig
	for name, p := range homePools(t) {
		if p.capacity >= 80 {
			t.Fatalf("pool %s at %d%%: the capture no longer has every pool below 80%%", name, p.capacity)
		}
		want = append(want, sig{signalPoolCapacity, apitypes.SeverityWarning, "pool:" + name})
	}
	for _, sg := range h.Signals {
		if sg.ID == signalPoolCapacity {
			t.Errorf("the request in flight raised %v: it must evaluate with poolCapacityWarn 80", sg)
		}
	}
	h = s.health(t)
	var got []apitypes.HealthSignal
	for _, sg := range h.Signals {
		if sg.ID == signalPoolCapacity {
			got = append(got, sg)
		}
	}
	if g, w := sigsOf(got), sortedSigs(want...); !slices.Equal(g, w) {
		t.Errorf("next request's pool-capacity %v, want one warning per pool %v", g, w)
	}
}

// ---- WI 4.1: bounded (D-15) and a stuck path at preflight ----

// stuckRoot is an OpenRoot seam whose open of one path ignores its caller
// until release closes, as an openat on a suspended pool does; it counts
// the opens of that path.
type stuckRoot struct {
	path    string
	release chan struct{}
	opens   atomic.Int32
}

func (s *stuckRoot) open(path string) (Root, error) {
	if path == s.path {
		s.opens.Add(1)
		<-s.release
	}
	return openRoot(path)
}

func TestPreflightStuckPathAnswersPendingAndStartsNoSecondOpen(t *testing.T) {
	stuckDir, _, _ := dirs(t)
	other := storagetest.TempDir(t)
	sr := &stuckRoot{path: stuckDir, release: make(chan struct{})}
	p, _ := newTestProvider(fstest.MapFS{}, noRun(t), func(o *Options) { o.OpenRoot = sr.open })
	configure(t, p, func(c *config.StorageConfig) {
		c.Backups.VzdumpPaths = []string{stuckDir}
		c.TimeMachine.Paths = []string{other}
	})
	setCommandTimeout(p, 300*time.Millisecond)

	start := time.Now()
	st := preflight(p, time.Second)
	if el := time.Since(start); el > 300*time.Millisecond+200*time.Millisecond {
		t.Errorf("first preflight took %s, want about commandTimeout (300ms)", el)
	}
	want := "open " + stuckDir + " did not finish within 300ms"
	if got := feats(t, st)[featureBackups]; got.Available || got.Reason != want {
		t.Errorf("backups = %+v, want unavailable %q", got, want)
	}
	if got := feats(t, st)[featureTimeMachine]; !got.Available {
		t.Errorf("timeMachine = %+v, want available beside the stuck path", got)
	}

	start = time.Now()
	st = preflight(p, time.Second)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("second preflight took %s, want at once", el)
	}
	if got := feats(t, st)[featureBackups]; got.Available || !strings.HasPrefix(got.Reason, "open "+stuckDir+" is still running since ") {
		t.Errorf("backups = %+v, want the pending reason", got)
	}
	if n := sr.opens.Load(); n != 1 {
		t.Errorf("%d opens of the stuck path, want 1", n)
	}
	if n := p.fl.countUnder(stuckDir); n != 1 {
		t.Errorf("%d flights under the stuck path, want 1", n)
	}

	close(sr.release)
	waitFor(t, func() bool { return p.fl.count() == 0 })
	if got := feats(t, preflight(p, time.Second))[featureBackups]; !got.Available {
		t.Errorf("after release backups = %+v, want available", got)
	}
	if n := sr.opens.Load(); n != 2 {
		t.Errorf("%d opens after release, want 2", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// newFlightsFor is a flights table with a fixed timeout.
func newFlightsFor(d time.Duration) *flights { return newFlights(func() time.Duration { return d }) }

func TestBoundedFirstFlightJoinAndValue(t *testing.T) {
	fl := newFlightsFor(time.Second)
	ctx := context.Background()
	v, err := bounded(ctx, fl, "/a", "read /a/x", func() (int, error) { return 7, nil })
	if v != 7 || err != nil {
		t.Fatalf("first flight = %d, %v; want 7, nil", v, err)
	}
	gate := make(chan struct{})
	var calls atomic.Int32
	fn := func() (string, error) { calls.Add(1); <-gate; return "ok", nil }
	results := make(chan string, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := bounded(ctx, fl, "/a", "scan /a", fn)
			if err != nil {
				t.Errorf("joined caller: %v", err)
			}
			results <- v
		}()
	}
	waitFor(t, func() bool { return calls.Load() == 1 && fl.count() == 1 })
	time.Sleep(20 * time.Millisecond) // let the other callers join
	close(gate)
	wg.Wait()
	close(results)
	for v := range results {
		if v != "ok" {
			t.Errorf("joined caller got %q, want ok", v)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fn ran %d times, want 1 (joined)", n)
	}
	waitFor(t, func() bool { return fl.count() == 0 })
	if _, err := bounded(ctx, fl, "/a", "read /a/x", func() (int, error) { return 0, errNotDir }); !errors.Is(err, errNotDir) {
		t.Errorf("error flight = %v, want fn's error", err)
	}
}

func TestBoundedTimeoutPendingGateAndRelease(t *testing.T) {
	const timeout = 300 * time.Millisecond
	fl := newFlightsFor(timeout)
	ctx := context.Background()
	release := make(chan struct{})
	var stuckCalls, fastCalls atomic.Int32
	stuck := func() (int, error) { stuckCalls.Add(1); <-release; return 1, nil }
	fast := func() (int, error) { fastCalls.Add(1); return 2, nil }

	start := time.Now()
	_, err := bounded(ctx, fl, "/a", "scan /a", stuck)
	var te zfs.TimeoutError
	if !errors.As(err, &te) || err.Error() != "scan /a did not finish within 300ms" {
		t.Fatalf("stuck call = %v, want scan /a did not finish within 300ms", err)
	}
	if el := time.Since(start); el > timeout+100*time.Millisecond {
		t.Errorf("stuck call took %s", el)
	}

	for range 20 {
		start := time.Now()
		_, err := bounded(ctx, fl, "/a", "scan /a", stuck)
		var pe zfs.PendingError
		if !errors.As(err, &pe) || pe.What != "scan /a" || time.Since(start) > 50*time.Millisecond {
			t.Fatalf("call past the timeout = %v after %s, want PendingError at once", err, time.Since(start))
		}
	}
	_, err = bounded(ctx, fl, "/a", "read /a/x", fast)
	var pe zfs.PendingError
	if !errors.As(err, &pe) || pe.What != "scan /a" {
		t.Errorf("other key under the stuck path = %v, want PendingError naming scan /a", err)
	}
	if v, err := bounded(ctx, fl, "/b", "scan /b", fast); v != 2 || err != nil {
		t.Errorf("other path = %d, %v; want served", v, err)
	}
	if fastCalls.Load() != 1 || stuckCalls.Load() != 1 || fl.count() != 1 {
		t.Errorf("fast %d, stuck %d calls, %d flights; want 1, 1, 1", fastCalls.Load(), stuckCalls.Load(), fl.count())
	}

	close(release)
	waitFor(t, func() bool { return fl.count() == 0 })
	if v, err := bounded(ctx, fl, "/a", "read /a/x", fast); v != 2 || err != nil {
		t.Errorf("after release read /a/x = %d, %v; want served", v, err)
	}
	if v, err := bounded(ctx, fl, "/a", "scan /a", stuck); v != 1 || err != nil || stuckCalls.Load() != 2 {
		t.Errorf("after release scan /a = %d, %v (%d calls); want a second flight", v, err, stuckCalls.Load())
	}
}

func TestBoundedCallerContextShorterThanTimeout(t *testing.T) {
	fl := newFlightsFor(300 * time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := bounded(ctx, fl, "/a", "scan /a", func() (int, error) { <-release; return 0, nil })
	var te zfs.TimeoutError
	if !errors.As(err, &te) || te.What != "scan /a" {
		t.Errorf("err = %v, want a TimeoutError naming scan /a", err)
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Errorf("took %s, want within the caller's 100ms + 50ms", el)
	}
}

func TestBoundedPerPathCap(t *testing.T) {
	fl := newFlightsFor(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	gate := make(chan struct{})
	var running, peak atomic.Int32
	fn := func() (int, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return 0, nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = bounded(ctx, fl, "/a", "read /a/"+strconv.Itoa(i), fn)
		}()
	}
	waitFor(t, func() bool { return running.Load() == int32(spec.MaxPathFlights) })
	time.Sleep(50 * time.Millisecond)
	if n := fl.countUnder("/a"); n != spec.MaxPathFlights {
		t.Errorf("%d flights under /a, want %d (the fifth waits for a slot)", n, spec.MaxPathFlights)
	}
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v, want served once a slot freed", i, err)
		}
	}
	if p := peak.Load(); p != int32(spec.MaxPathFlights) {
		t.Errorf("peak %d running under /a, want %d", p, spec.MaxPathFlights)
	}

	// A caller that cannot get a slot gets a TimeoutError by its deadline.
	block := make(chan struct{})
	defer close(block)
	for i := range spec.MaxPathFlights {
		go func() {
			_, _ = bounded(context.Background(), fl, "/c", "read /c/"+strconv.Itoa(i), func() (int, error) { <-block; return 0, nil })
		}()
	}
	waitFor(t, func() bool { return fl.countUnder("/c") == spec.MaxPathFlights })
	short, cancelShort := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	_, err := bounded(short, fl, "/c", "read /c/fifth", func() (int, error) { return 0, nil })
	var te zfs.TimeoutError
	if !errors.As(err, &te) || te.What != "read /c/fifth" || time.Since(start) > 200*time.Millisecond {
		t.Errorf("fifth caller = %v after %s, want a TimeoutError by its 100ms deadline", err, time.Since(start))
	}
}

func TestBoundedCarriesAPanicToTheCaller(t *testing.T) {
	fl := newFlightsFor(time.Second)
	defer func() {
		r := recover()
		bp, ok := r.(*boundedPanic)
		if !ok || bp.what != "read /a/x" || bp.value != "boom" {
			t.Errorf("recovered %v, want the boundedPanic of read /a/x", r)
		}
		if fl.count() != 0 {
			t.Errorf("%d flights after the panic, want 0", fl.count())
		}
	}()
	_, _ = bounded(context.Background(), fl, "/a", "read /a/x", func() (int, error) { panic("boom") })
	t.Error("bounded returned, want the panic carried")
}

// TestStatfsSeamDefault checks the platform seam: statfs of a directory on
// linux reports a block size, elsewhere an error (CON-3).
func TestStatfsSeamDefault(t *testing.T) {
	u, err := defaultStatfs(os.TempDir())
	if goosLinux() {
		if err != nil || u.Bsize == 0 || u.Blocks == 0 {
			t.Errorf("defaultStatfs = %+v, %v; want block counts", u, err)
		}
		return
	}
	if err == nil {
		t.Errorf("defaultStatfs off linux = %+v, want an error", u)
	}
}

func goosLinux() bool { return New(Options{}).goos == "linux" }
