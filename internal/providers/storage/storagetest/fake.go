package storagetest

// F-ZFS (test-strategy.md, Fake Fidelity Contracts; architecture WI 3.5):
// a zfs.Runner that replays a capture directory written by
// testdata/capture.sh and encodes what OpenZFS does, not success.
//
// Every refusal below is OpenZFS 2.4.1's own, at commit
// 1c702dda346a59e05cfd3029569bbb1d5d91c54b (zfs-2.4.1). Paths are of
// https://github.com/openzfs/zfs/blob/1c702dda346a59e05cfd3029569bbb1d5d91c54b/<path>:
//
//   - libzfs prints "<errbuf>: <description>\n" (lib/libzfs/libzfs_util.c
//     zfs_verror L350-L374, the print at L369-L370), the description being the
//     zfs_error_aux text when one was set, else libzfs_error_description's
//     (L87-L333).
//   - zpool scrub [-s] P: zpool_scan_range builds errbuf "cannot scrub P" or
//     "cannot cancel scrubbing P" (lib/libzfs/libzfs_pool.c L2845-L2865). EBUSY
//     with a running, unpaused scrub is EZFS_SCRUBBING "currently scrubbing;
//     use 'zpool scrub -s' to cancel scrub", with anything else running
//     (a resilver) EZFS_RESILVERING "currently resilvering" (L2878-L2927;
//     libzfs_util.c L253-L255, L137-L138); ENOENT is EZFS_NO_SCRUB "there is
//     no active scrub" (libzfs_pool.c L2928-L2929; libzfs_util.c L262-L263).
//     The kernel side: a scrub start is EBUSY while a scan runs
//     (module/zfs/dsl_scan.c dsl_scan_setup_check L854-L865) except that a
//     paused scrub is resumed (dsl_scan L1041-L1050); a stop is EBUSY during
//     a resilver (module/zfs/spa.c spa_scan_stop L9579-L9587) and ENOENT when
//     no scan runs (dsl_scan.c dsl_scan_cancel_check L1333-L1341), a paused
//     scrub counting as running. scrub_callback returns err != 0, so the
//     exit is 1 (cmd/zpool/zpool_main.c L8458-L8482, L8640-L8649).
//   - A pool that does not exist: zpool_open_canfail's "cannot open 'P': no
//     such pool" (lib/libzfs/libzfs_pool.c L1332-L1334), exit 1.
//   - zfs snapshot: the kernel reports EEXIST per snapshot
//     (module/zfs/dsl_dataset.c L1523-L1546, L1672-L1680, L1710-L1716), printed as "cannot
//     create snapshot 'D@N': dataset already exists" (lib/libzfs/
//     libzfs_dataset.c zfs_snapshot_nvl L4253-L4265; libzfs_util.c
//     L466-L470); nothing is created (one sync task), and with -r
//     zfs_do_snapshot adds "no snapshots were created" and exits 1
//     (cmd/zfs/zfs_main.c L4714-L4716).
//   - Without CAP_SYS_ADMIN (A-7): zpool scrub's ioctl policy is
//     zfs_secpolicy_config (module/zfs/zfs_ioctl.c L7640-L7643), EPERM
//     without CAP_SYS_ADMIN (L1089-L1097; module/os/linux/zfs/policy.c
//     L103-L106); zfs snapshot's is zfs_secpolicy_snapshot (zfs_ioctl.c
//     L951-L976), which needs secpolicy_zfs (CAP_SYS_ADMIN, policy.c
//     L252-L255) or a delegation (zfs_ioctl.c L473-L502). The policy fails
//     before the handler, so no per-snapshot error comes back and libzfs
//     prints its generic errbuf "cannot create snapshots " (libzfs_dataset.c
//     L4214-L4215, L4266-L4278); EPERM and EACCES are EZFS_PERM
//     "permission denied" (libzfs_util.c L400-L404, L165-L166).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The WI 1.1 synthetic scan vectors (testdata/captures/synthetic) SeedScan
// takes; the fake reads the first pool's scan_stats of each.
const (
	VectorScrubScanning    = "zpool-status-scrub-scanning.json"
	VectorScrubPaused      = "zpool-status-scrub-paused.json"
	VectorScrubCanceled    = "zpool-status-scrub-canceled.json"
	VectorScrubFinished    = "zpool-status-scrub-finished-repairs.json"
	VectorResilverScanning = "zpool-status-resilver-scanning.json"
)

// absentName is the name capture.sh's negative cases ask for.
const absentName = "nexus-capture-absent"

// Options are the fake's caller and clock.
type Options struct {
	// CapSysAdmin is whether the caller holds CAP_SYS_ADMIN. False, the
	// default, is the caller inside nexusd's sandbox: every write is denied
	// (A-7). Reads never check it.
	CapSysAdmin bool
	// Now is the fake clock (F-CLOCK); nil is time.Now.
	Now func() time.Time
	// Vectors is the directory of the synthetic scan vectors; empty is this
	// module's testdata/captures/synthetic.
	Vectors string
}

// captured is one capture file: what the tool wrote and how it exited.
type captured struct {
	stdout, stderr []byte
	exit           int
}

func (c captured) result() zfs.Result {
	return zfs.Result{Stdout: bytes.Clone(c.stdout), Stderr: bytes.Clone(c.stderr), ExitCode: c.exit}
}

// notFound is capture.sh's record of a program the host does not have.
func (c captured) notFound() bool {
	return c.exit == 127 && string(c.stderr) == "not found\n" && len(c.stdout) == 0
}

type rule struct {
	stuck   bool
	release <-chan struct{}
}

// newSnap is a snapshot the fake created.
type newSnap struct {
	name, createdBy, refer string
	creation               int64
}

// Fake is the capture-replaying runner. Its Run method is a zfs.Runner and is
// safe for concurrent use.
type Fake struct {
	t      testing.TB
	dir    string
	opts   Options
	paths  zfs.Paths
	format zfs.Format

	byArgv  map[string]captured // key: argv joined by NUL
	byFile  map[string]captured // key: the capture file's relative name
	absent  map[string]bool     // program path → not on the host
	pools   map[string]bool
	dsNames []string // filesystems and volumes, in listing order
	refer   map[string]string
	baseRaw map[string]json.RawMessage // pool → captured scan_stats
	vectors map[string]json.RawMessage // vector file → scan_stats
	denial  *captured                  // sandbox-probe/write-probe.txt

	mu      sync.Mutex
	scans   map[string]json.RawMessage // pool → scan_stats after a change
	finish  map[string]bool
	snaps   map[string]bool // every snapshot name, captured and created
	created []newSnap
	reads   [][]string
	writes  [][]string
	rules   map[string]rule
}

// NewFake returns a fake on capture: a directory name under this module's
// testdata/captures (home-server, pihole, public-nginx), or an absolute
// path. The capture is gated with capturegate.Require, which skips the test
// while it is absent. At most one Options may be given.
func NewFake(t testing.TB, capture string, opts ...Options) *Fake {
	t.Helper()
	if len(opts) > 1 {
		t.Fatalf("storagetest.NewFake: %d Options, want at most one", len(opts))
	}
	dir := capture
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(capturesRoot(), capture)
	}
	capturegate.Require(t, dir)
	f := &Fake{
		t: t, dir: dir,
		byArgv: map[string]captured{}, byFile: map[string]captured{}, absent: map[string]bool{},
		pools: map[string]bool{}, refer: map[string]string{}, baseRaw: map[string]json.RawMessage{},
		vectors: map[string]json.RawMessage{}, scans: map[string]json.RawMessage{},
		finish: map[string]bool{}, snaps: map[string]bool{}, rules: map[string]rule{},
	}
	if len(opts) == 1 {
		f.opts = opts[0]
	}
	if f.opts.Now == nil {
		f.opts.Now = time.Now
	}
	if f.opts.Vectors == "" {
		f.opts.Vectors = filepath.Join(capturesRoot(), "synthetic")
	}
	if err := f.load(); err != nil {
		t.Fatalf("storagetest.NewFake(%s): %v", capture, err)
	}
	return f
}

// capturesRoot is this module's testdata/captures, found from this file so
// that every package's tests reach it.
func capturesRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", "captures")
}

func key(argv []string) string { return strings.Join(argv, "\x00") }

// load reads PROVENANCE's commands and the files they wrote, then the state
// the capture seeds: pools, datasets, snapshots and each pool's scan.
func (f *Fake) load() error {
	prov, err := os.ReadFile(filepath.Join(f.dir, "PROVENANCE"))
	if err != nil {
		return err
	}
	in := false
	for _, line := range strings.Split(string(prov), "\n") {
		if line == "commands:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			in = false
			continue
		}
		name, cmd, ok := strings.Cut(strings.TrimPrefix(line, "  "), ": ")
		if !ok {
			return fmt.Errorf("PROVENANCE command line %q", line)
		}
		cmd, noeol := strings.CutSuffix(cmd, " [noeol]")
		argv := toolArgv(strings.Fields(cmd))
		if argv == nil {
			continue
		}
		c, err := readCapture(filepath.Join(f.dir, name), noeol)
		if err != nil {
			return err
		}
		f.byArgv[key(argv)] = c
		f.byFile[name] = c
		switch filepath.Base(argv[0]) {
		case "zpool":
			if f.paths.Zpool == "" {
				f.paths.Zpool = argv[0]
			}
		case "zfs":
			if f.paths.Zfs == "" {
				f.paths.Zfs = argv[0]
			}
		}
		if c.notFound() {
			f.absent[argv[0]] = true
		}
	}
	if f.paths.Zpool == "" || f.paths.Zfs == "" {
		return errors.New("PROVENANCE names no zpool or no zfs command")
	}
	if c, ok := f.byArgv[key([]string{f.paths.Zfs, "version"})]; ok && c.exit == 0 {
		v, err := zfs.ParseVersion(c.stdout)
		if err != nil {
			return err
		}
		f.format = v.Format()
	}
	if probe := filepath.Join(f.dir, "sandbox-probe", "write-probe.txt"); fileExists(probe) {
		c, err := readCapture(probe, false)
		if err != nil {
			return err
		}
		if c.exit == 0 {
			// A-7's falsifier: the snapshot succeeded inside the sandbox.
			return errors.New("sandbox-probe/write-probe.txt exited 0: a write succeeded inside the sandbox, so A-7 (and this fake's denial) is wrong")
		}
		f.denial = &c
	}
	for _, v := range []string{VectorScrubScanning, VectorScrubPaused, VectorScrubCanceled, VectorScrubFinished, VectorResilverScanning} {
		raw, err := vectorScan(filepath.Join(f.opts.Vectors, v))
		if err != nil {
			return err
		}
		f.vectors[v] = raw
	}
	return f.seed()
}

func (f *Fake) seed() error {
	if c, ok := f.byArgv[key(f.Argv(zfs.CmdPoolNames))]; ok && c.exit == 0 {
		for _, r := range lines(c.stdout) {
			f.pools[r] = true
		}
	}
	if c, ok := f.byArgv[key(zfs.Argv(f.paths, zfs.Text, zfs.CmdDatasetList))]; ok && c.exit == 0 {
		cols := strings.Split(zfs.DatasetColumns, ",")
		ri := slices.Index(cols, "refer")
		for _, l := range lines(c.stdout) {
			r := strings.Split(l, "\t")
			if len(r) != len(cols) {
				return fmt.Errorf("zfs-list-datasets.txt row %q has %d columns, want %d", l, len(r), len(cols))
			}
			f.dsNames = append(f.dsNames, r[0])
			f.refer[r[0]] = r[ri]
		}
	}
	for _, cmd := range []zfs.Command{zfs.CmdSnapshotList, zfs.CmdSnapshotTimes} {
		if c, ok := f.byArgv[key(f.Argv(cmd))]; ok && c.exit == 0 {
			for _, l := range lines(c.stdout) {
				name, _, _ := strings.Cut(l, "\t")
				f.snaps[name] = true
			}
		}
	}
	c, ok := f.byArgv[key(zfs.Argv(f.paths, zfs.JSON, zfs.CmdPoolStatus))]
	if !ok || c.exit != 0 {
		return nil
	}
	_, pools, err := statusPools(c.stdout)
	if err != nil {
		return fmt.Errorf("zpool status JSON: %w", err)
	}
	for _, m := range pools {
		p, err := decodeObject(m.value)
		if err != nil {
			return err
		}
		if raw, ok := p.get("scan_stats"); ok {
			f.baseRaw[m.key] = raw
		}
	}
	return nil
}

// toolArgv is the argv Exec would run for a PROVENANCE command: capture.sh's
// `/usr/bin/env -i VAR=VALUE…` prefix removed. Nil when the program is not
// zpool or zfs.
func toolArgv(f []string) []string {
	if len(f) > 0 && f[0] == "/usr/bin/env" {
		f = f[1:]
		for len(f) > 0 && (f[0] == "-i" || strings.Contains(f[0], "=")) {
			f = f[1:]
		}
	}
	if len(f) == 0 || !filepath.IsAbs(f[0]) {
		return nil
	}
	if b := filepath.Base(f[0]); b != "zpool" && b != "zfs" {
		return nil
	}
	return f
}

// readCapture reads a capture file: stdout, then an `Exit <code>` line
// (capture.sh added a newline before it when stdout had none: noeol), and
// the .stderr beside it.
func readCapture(path string, noeol bool) (captured, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return captured{}, err
	}
	stderr, err := os.ReadFile(path + ".stderr")
	if err != nil {
		return captured{}, err
	}
	body, ok := bytes.CutSuffix(b, []byte("\n"))
	if !ok {
		return captured{}, fmt.Errorf("%s does not end in a newline", path)
	}
	i := bytes.LastIndexByte(body, '\n')
	last, found := bytes.CutPrefix(body[i+1:], []byte("Exit "))
	code, err := strconv.Atoi(string(last))
	if !found || err != nil {
		return captured{}, fmt.Errorf("%s: last line %q is not Exit <code>", path, body[i+1:])
	}
	stdout := body[:i+1]
	if noeol {
		stdout = bytes.TrimSuffix(stdout, []byte("\n"))
	}
	return captured{stdout: stdout, stderr: stderr, exit: code}, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Runner is f.Run as a zfs.Runner.
func (f *Fake) Runner() zfs.Runner { return f.Run }

// Paths are the tool paths of the capture.
func (f *Fake) Paths() zfs.Paths { return f.paths }

// Format is the format the capture's zfs version selects (D-5).
func (f *Fake) Format() zfs.Format { return f.format }

// Argv is zfs.Argv with the capture's paths and format.
func (f *Fake) Argv(cmd zfs.Command, args ...string) []string {
	return zfs.Argv(f.paths, f.format, cmd, args...)
}

// Reads returns every read argv answered so far, in order.
func (f *Fake) Reads() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneAll(f.reads)
}

// Writes returns every zpool scrub, zpool scrub -s and zfs snapshot argv
// issued so far, in order, whether ZFS accepted or refused it.
func (f *Fake) Writes() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneAll(f.writes)
}

func cloneAll(a [][]string) [][]string {
	out := make([][]string, len(a))
	for i, x := range a {
		out[i] = slices.Clone(x)
	}
	return out
}

// Hang makes every call of argv block until its ctx ends and return an error
// wrapping ctx.Err(), as Exec does after killing the tool's group.
func (f *Fake) Hang(argv []string) { f.addRule(argv, rule{}) }

// Stuck makes every call of argv ignore its ctx until release is closed: a
// process in uninterruptible sleep that SIGKILL cannot end (NFR-2, A-31).
// Then the call ends as Exec would: an error wrapping ctx.Err() when the ctx
// ended meanwhile, else the answer.
func (f *Fake) Stuck(argv []string, release <-chan struct{}) {
	f.addRule(argv, rule{stuck: true, release: release})
}

func (f *Fake) addRule(argv []string, r rule) {
	f.t.Helper()
	if _, ok := f.byArgv[key(argv)]; !ok {
		if _, _, _, ok := f.classify(argv); !ok {
			f.t.Errorf("storagetest: Hang or Stuck on unexpected argv %q", argv)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[key(argv)] = r
}

// SeedScan sets pool's scan to a synthetic vector's (a resilver running, a
// paused scrub, …), as if ZFS had reached that state outside the test.
func (f *Fake) SeedScan(pool, vector string) {
	f.t.Helper()
	raw, ok := f.vectors[vector]
	if !ok || !f.pools[pool] {
		f.t.Errorf("storagetest: SeedScan(%q, %q): unknown pool or vector", pool, vector)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans[pool] = raw
}

// FinishImmediately makes pool's next accepted scrub FINISHED at once, with
// start and end time now: a tiny pool (A-25).
func (f *Fake) FinishImmediately(pool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finish[pool] = true
}

// StaleFinished sets pool's scan to a scrub that FINISHED an hour ago.
func (f *Fake) StaleFinished(pool string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.opts.Now().Add(-time.Hour).Unix()
	raw, err := setScan(f.vectors[VectorScrubFinished], map[string]any{
		"start_time": start, "pass_start": start, "end_time": start + 600,
	})
	if err != nil {
		f.t.Errorf("storagetest: StaleFinished: %v", err)
		return
	}
	f.scans[pool] = raw
}

// Run answers argv from the capture and the fake's state. An argv that is
// neither captured nor built by zfs.Argv, or one whose answer the fake
// cannot derive, fails the calling test and returns an error wrapping
// zfs.ErrNotStarted.
func (f *Fake) Run(ctx context.Context, argv []string) (zfs.Result, error) {
	f.t.Helper()
	if err := ctx.Err(); err != nil {
		return zfs.Result{}, fmt.Errorf("zfs: %q: %w", argv, err)
	}
	c, isCaptured := f.byArgv[key(argv)]
	cmd, form, args, shaped := f.classify(argv)
	if !isCaptured && !shaped {
		return f.refuse(argv, "unexpected argv %q: neither captured nor built by zfs.Argv for this capture's tools", argv)
	}
	write := shaped && isWrite(cmd)
	f.mu.Lock()
	if write {
		f.writes = append(f.writes, slices.Clone(argv))
	} else {
		f.reads = append(f.reads, slices.Clone(argv))
	}
	r, blocked := f.rules[key(argv)]
	f.mu.Unlock()
	if blocked {
		if !r.stuck {
			<-ctx.Done()
			return zfs.Result{}, fmt.Errorf("zfs: %s was stopped: %w", argv[0], ctx.Err())
		}
		<-r.release
		if err := ctx.Err(); err != nil {
			return zfs.Result{}, fmt.Errorf("zfs: %s was stopped: %w", argv[0], err)
		}
	}
	if f.absent[argv[0]] {
		return zfs.Result{}, fmt.Errorf("%w: %w", zfs.ErrNotStarted,
			&fs.PathError{Op: "fork/exec", Path: argv[0], Err: syscall.ENOENT})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if write {
		return f.write(cmd, args)
	}
	return f.read(argv, c, isCaptured, cmd, form, args, shaped)
}

// refuse fails the calling test and returns the not-started error.
func (f *Fake) refuse(argv []string, format string, a ...any) (zfs.Result, error) {
	f.t.Helper()
	msg := fmt.Sprintf(format, a...)
	f.t.Errorf("storagetest: %s", msg)
	return zfs.Result{}, fmt.Errorf("%w: storagetest: %s (argv %q)", zfs.ErrNotStarted, msg, argv)
}

func (f *Fake) unencoded(argv []string, what string) (zfs.Result, error) {
	f.t.Helper()
	return f.refuse(argv, "argv %q: %s is not encoded by the fake", argv, what)
}

var commands = []zfs.Command{
	zfs.CmdVersion, zfs.CmdPoolNames, zfs.CmdPoolList, zfs.CmdPoolStatus, zfs.CmdPoolStatusVerbose,
	zfs.CmdPoolGet, zfs.CmdDatasetList, zfs.CmdSnapshotTimes, zfs.CmdSnapshotList,
	zfs.CmdSnapshotListRecursive, zfs.CmdSnapshotGet, zfs.CmdScrubStart, zfs.CmdScrubStop,
}

// classify finds the command, format and args with which zfs.Argv builds
// argv for the capture's tools.
func (f *Fake) classify(argv []string) (zfs.Command, zfs.Format, []string, bool) {
	if len(argv) >= 5 && argv[0] == f.paths.Zfs && argv[1] == "snapshot" && argv[2] == "-o" {
		// zfs.Argv puts the property name; equality below checks it.
		if _, id, ok := strings.Cut(argv[3], "="); ok {
			name := argv[len(argv)-1]
			for _, cmd := range []zfs.Command{zfs.CmdSnapshotCreate, zfs.CmdSnapshotCreateRecursive} {
				if slices.Equal(zfs.Argv(f.paths, f.format, cmd, id, name), argv) {
					return cmd, f.format, []string{id, name}, true
				}
			}
		}
	}
	for _, cmd := range commands {
		for n := 0; n <= 1 && n < len(argv); n++ {
			args := argv[len(argv)-n:]
			for _, form := range []zfs.Format{zfs.JSON, zfs.Text} {
				if slices.Equal(zfs.Argv(f.paths, form, cmd, args...), argv) {
					return cmd, form, slices.Clone(args), true
				}
			}
		}
	}
	return 0, 0, nil, false
}

func isWrite(cmd zfs.Command) bool {
	switch cmd {
	case zfs.CmdScrubStart, zfs.CmdScrubStop, zfs.CmdSnapshotCreate, zfs.CmdSnapshotCreateRecursive:
		return true
	}
	return false
}

// ---- reads ----

func (f *Fake) read(argv []string, c captured, isCaptured bool, cmd zfs.Command, form zfs.Format, args []string, shaped bool) (zfs.Result, error) {
	snapshotListing := cmd == zfs.CmdSnapshotTimes || cmd == zfs.CmdSnapshotList ||
		cmd == zfs.CmdSnapshotListRecursive || cmd == zfs.CmdSnapshotGet
	status := cmd == zfs.CmdPoolStatus || cmd == zfs.CmdPoolStatusVerbose
	changed := shaped && (snapshotListing && len(f.created) > 0 || status && len(f.scans) > 0)
	if isCaptured && !changed {
		if !shaped && len(f.created) > 0 && strings.Contains(strings.Join(argv, " "), "-t snapshot") {
			return f.unencoded(argv, "a snapshot listing outside zfs.Argv after a snapshot was created")
		}
		return c.result(), nil
	}
	switch {
	case status:
		return f.status(argv, cmd, form, args)
	case snapshotListing:
		return f.snapshotList(argv, cmd, args)
	case cmd == zfs.CmdPoolList || cmd == zfs.CmdDatasetList:
		return f.namedList(argv, cmd, form, args)
	case cmd == zfs.CmdPoolGet && !f.pools[args[0]]:
		return f.missing(argv, cmd, form, args[0])
	}
	return f.unencoded(argv, "this read")
}

// base is the captured answer of cmd in form with no name.
func (f *Fake) base(cmd zfs.Command, form zfs.Format) (captured, bool) {
	c, ok := f.byArgv[key(zfs.Argv(f.paths, form, cmd))]
	return c, ok && c.exit == 0
}

// missing is ZFS's answer for a pool or dataset that does not exist: the
// captured negative case of the same tool with the name replaced (libzfs's
// "cannot open '<name>': …" is the same for every read).
func (f *Fake) missing(argv []string, cmd zfs.Command, form zfs.Format, name string) (zfs.Result, error) {
	file, absentArg, keepStdout := "zfs/negative-zfs-list.json", absentName+"/x", cmd == zfs.CmdDatasetList && form == zfs.JSON
	switch cmd {
	case zfs.CmdPoolList:
		file, absentArg, keepStdout = "zfs/negative-zpool-list.json", absentName, form == zfs.JSON
	case zfs.CmdPoolStatus, zfs.CmdPoolStatusVerbose, zfs.CmdPoolGet:
		if cmd == zfs.CmdPoolGet && form == zfs.JSON {
			return f.unencoded(argv, "zpool get -j of a missing pool (no captured negative case)")
		}
		file, absentArg, keepStdout = "zfs/negative-zpool-status.json", absentName, form == zfs.JSON
	}
	c, ok := f.byFile[file]
	if !ok {
		return f.unencoded(argv, "a missing name without the captured "+file)
	}
	res := zfs.Result{ExitCode: c.exit, Stderr: bytes.ReplaceAll(c.stderr, []byte(absentArg), []byte(name))}
	if keepStdout {
		res.Stdout = bytes.ReplaceAll(c.stdout, []byte(absentArg), []byte(name))
	}
	return res, nil
}

// status is zpool status from the captured all-pools document, narrowed to
// the named pool and with each changed pool's scan_stats.
func (f *Fake) status(argv []string, cmd zfs.Command, form zfs.Format, args []string) (zfs.Result, error) {
	name := ""
	if len(args) == 1 {
		name = args[0]
		if !f.pools[name] {
			return f.missing(argv, cmd, form, name)
		}
	}
	if form != zfs.JSON {
		return f.unencoded(argv, "the text of zpool status for one pool or after a scan change")
	}
	b, ok := f.base(cmd, form)
	if !ok {
		return f.unencoded(argv, "zpool status without its captured all-pools document")
	}
	top, pools, err := statusPools(b.stdout)
	if err != nil {
		return f.unencoded(argv, "an unreadable captured zpool status document ("+err.Error()+")")
	}
	var out object
	for _, m := range pools {
		if name != "" && m.key != name {
			continue
		}
		if raw, ok := f.scans[m.key]; ok {
			p, err := decodeObject(m.value)
			if err != nil {
				return f.unencoded(argv, "an unreadable pool object ("+err.Error()+")")
			}
			m.value = p.set("scan_stats", raw, "vdevs").encode()
		}
		out = append(out, m)
	}
	return zfs.Result{Stdout: reencode(b.stdout, top.set("pools", out.encode(), "").encode())}, nil
}

func statusPools(doc []byte) (object, object, error) {
	return container(doc, "pools")
}

// container decodes a document and its object member name.
func container(doc []byte, name string) (object, object, error) {
	top, err := decodeObject(doc)
	if err != nil {
		return nil, nil, err
	}
	raw, ok := top.get(name)
	if !ok {
		return nil, nil, fmt.Errorf("no %q member", name)
	}
	inner, err := decodeObject(raw)
	return top, inner, err
}

// reencode keeps the document's trailing newline.
func reencode(orig, b []byte) []byte {
	if bytes.HasSuffix(orig, []byte("\n")) {
		return append(b, '\n')
	}
	return b
}

// namedList is zpool list NAME or zfs list NAME: the captured listing of
// every pool or dataset narrowed to NAME (JSON: its member; text: its row).
func (f *Fake) namedList(argv []string, cmd zfs.Command, form zfs.Format, args []string) (zfs.Result, error) {
	if len(args) != 1 {
		return f.unencoded(argv, "this listing")
	}
	name := args[0]
	exists := f.pools[name]
	if cmd == zfs.CmdDatasetList {
		exists = slices.Contains(f.dsNames, name)
	}
	if !exists {
		return f.missing(argv, cmd, form, name)
	}
	b, ok := f.base(cmd, form)
	if !ok {
		return f.unencoded(argv, "a named listing without its captured full listing")
	}
	if form == zfs.Text {
		var out []byte
		for _, l := range lines(b.stdout) {
			if first, _, _ := strings.Cut(l, "\t"); first == name {
				out = append(out, l+"\n"...)
			}
		}
		return zfs.Result{Stdout: out}, nil
	}
	member := "pools"
	if cmd == zfs.CmdDatasetList {
		member = "datasets"
	}
	top, items, err := container(b.stdout, member)
	if err != nil {
		return f.unencoded(argv, "an unreadable captured listing ("+err.Error()+")")
	}
	var out object
	for _, m := range items {
		if m.key == name {
			out = append(out, m)
		}
	}
	return zfs.Result{Stdout: reencode(b.stdout, top.set(member, out.encode(), "").encode())}, nil
}

// snapshotList is a -H -p snapshot listing (D-5 exception): the captured
// rows plus the snapshots the fake created, filtered as the argv asks.
func (f *Fake) snapshotList(argv []string, cmd zfs.Command, args []string) (zfs.Result, error) {
	baseCmd := zfs.CmdSnapshotList
	if cmd == zfs.CmdSnapshotTimes {
		baseCmd = zfs.CmdSnapshotTimes
	}
	b, ok := f.base(baseCmd, zfs.Text)
	if !ok {
		return f.unencoded(argv, "a snapshot listing without its captured full listing")
	}
	rows := lines(b.stdout)
	for _, s := range f.created {
		row := s.name + "\t" + strconv.FormatInt(s.creation, 10)
		if baseCmd == zfs.CmdSnapshotList {
			// used 0 (nothing written since), refer the dataset's, no holds.
			row += "\t0\t" + s.refer + "\t0\t" + s.createdBy
		}
		rows = insertSnapshot(rows, row)
	}
	keep := func(string) bool { return true }
	if len(args) == 1 {
		arg := args[0]
		switch cmd {
		case zfs.CmdSnapshotGet:
			if !f.snaps[arg] {
				return f.missing(argv, cmd, zfs.Text, arg)
			}
			keep = func(n string) bool { return n == arg }
		case zfs.CmdSnapshotListRecursive:
			if !slices.Contains(f.dsNames, arg) {
				return f.missing(argv, cmd, zfs.Text, arg)
			}
			keep = func(n string) bool { ds := datasetOf(n); return ds == arg || strings.HasPrefix(ds, arg+"/") }
		default: // -d 1 DATASET
			if !slices.Contains(f.dsNames, arg) {
				return f.missing(argv, cmd, zfs.Text, arg)
			}
			keep = func(n string) bool { return datasetOf(n) == arg }
		}
	}
	var out []byte
	for _, r := range rows {
		if name, _, _ := strings.Cut(r, "\t"); keep(name) {
			out = append(out, r+"\n"...)
		}
	}
	return zfs.Result{Stdout: out}, nil
}

func datasetOf(snapshot string) string {
	ds, _, _ := strings.Cut(snapshot, "@")
	return ds
}

// insertSnapshot places a new (newest) snapshot's row where zfs list's
// default order puts it: datasets by name, a dataset's snapshots by
// creation (cmd/zfs/zfs_main.c L3529-L3531; cmd/zfs/zfs_iter.c zfs_compare).
func insertSnapshot(rows []string, row string) []string {
	name, _, _ := strings.Cut(row, "\t")
	ds := datasetOf(name)
	i := len(rows)
	for j, r := range rows {
		n, _, _ := strings.Cut(r, "\t")
		if datasetOf(n) > ds {
			i = j
			break
		}
	}
	return slices.Insert(rows, i, row)
}

// ---- writes ----

func (f *Fake) write(cmd zfs.Command, args []string) (zfs.Result, error) {
	switch cmd {
	case zfs.CmdScrubStart, zfs.CmdScrubStop:
		return f.scrub(cmd == zfs.CmdScrubStop, args[0])
	default:
		return f.snapshot(args[0], args[1], cmd == zfs.CmdSnapshotCreateRecursive)
	}
}

func refused(stderr string) (zfs.Result, error) {
	return zfs.Result{Stderr: []byte(stderr), ExitCode: 1}, nil
}

// scanState is the function and state words of a scan_stats object, and its
// pause time.
type scanState struct {
	Function   string `json:"function"`
	State      string `json:"state"`
	ScrubPause uint64 `json:"scrub_pause"`
}

func (f *Fake) scrub(stop bool, pool string) (zfs.Result, error) {
	f.t.Helper()
	if !f.pools[pool] {
		return refused(fmt.Sprintf("cannot open '%s': no such pool\n", pool))
	}
	errbuf := "cannot scrub " + pool
	if stop {
		errbuf = "cannot cancel scrubbing " + pool
	}
	if !f.opts.CapSysAdmin {
		return refused(errbuf + ": permission denied\n")
	}
	raw, ok := f.scans[pool]
	if !ok {
		raw, ok = f.baseRaw[pool]
	}
	var st scanState
	if ok {
		if err := json.Unmarshal(raw, &st); err != nil {
			return f.unencoded([]string{"scrub", pool}, "a scan_stats the fake cannot read ("+err.Error()+")")
		}
	} else if _, captured := f.base(zfs.CmdPoolStatus, zfs.JSON); !captured {
		return f.unencoded([]string{"scrub", pool}, "a scrub without a captured zpool status JSON document")
	}
	now := f.opts.Now().Unix()
	running := st.State == "SCANNING"
	scrubbing := running && st.Function == "SCRUB"
	var err error
	switch {
	case stop && running && !scrubbing:
		return refused(errbuf + ": currently resilvering\n")
	case stop && scrubbing:
		raw, err = setScan(raw, map[string]any{"state": "CANCELED", "end_time": now})
	case stop:
		return refused(errbuf + ": there is no active scrub\n")
	case scrubbing && st.ScrubPause != 0:
		var spent struct {
			Spent uint64 `json:"scrub_spent_paused"`
		}
		if err := json.Unmarshal(raw, &spent); err != nil {
			return f.unencoded([]string{"scrub", pool}, "a paused scan_stats the fake cannot read")
		}
		raw, err = setScan(raw, map[string]any{
			"scrub_pause": 0, "scrub_spent_paused": spent.Spent + uint64(max(now-int64(st.ScrubPause), 0)),
		})
	case scrubbing:
		return refused(errbuf + ": currently scrubbing; use 'zpool scrub -s' to cancel scrub\n")
	case running:
		return refused(errbuf + ": currently resilvering\n")
	case f.finish[pool]:
		delete(f.finish, pool)
		raw, err = setScan(f.vectors[VectorScrubFinished], map[string]any{
			"start_time": now, "pass_start": now, "end_time": now,
		})
	default:
		raw, err = setScan(f.vectors[VectorScrubScanning], map[string]any{
			"start_time": now, "pass_start": now, "end_time": 0, "scrub_pause": 0,
		})
	}
	if err != nil {
		return f.unencoded([]string{"scrub", pool}, "a scan change ("+err.Error()+")")
	}
	f.scans[pool] = raw
	return zfs.Result{}, nil
}

func (f *Fake) snapshot(createdBy, name string, recursive bool) (zfs.Result, error) {
	f.t.Helper()
	ds, snap, _ := strings.Cut(name, "@")
	if !slices.Contains(f.dsNames, ds) {
		return f.unencoded([]string{"zfs", "snapshot", name}, "a snapshot of a dataset the capture does not list (zfs prints its usage)")
	}
	targets := []string{ds}
	if recursive {
		for _, d := range f.dsNames {
			if strings.HasPrefix(d, ds+"/") {
				targets = append(targets, d)
			}
		}
	}
	trailer := ""
	if recursive {
		trailer = "no snapshots were created\n"
	}
	if !f.opts.CapSysAdmin {
		if f.denial != nil {
			return zfs.Result{Stderr: append(bytes.Clone(f.denial.stderr), trailer...), ExitCode: f.denial.exit}, nil
		}
		return refused("cannot create snapshots : permission denied\n" + trailer)
	}
	var exists strings.Builder
	for _, d := range targets {
		if f.snaps[d+"@"+snap] {
			fmt.Fprintf(&exists, "cannot create snapshot '%s@%s': dataset already exists\n", d, snap)
		}
	}
	if exists.Len() > 0 {
		return refused(exists.String() + trailer)
	}
	now := f.opts.Now().Unix()
	for _, d := range targets {
		n := d + "@" + snap
		f.snaps[n] = true
		f.created = append(f.created, newSnap{name: n, createdBy: createdBy, refer: f.refer[d], creation: now})
	}
	return zfs.Result{}, nil
}

// ---- order-keeping JSON ----

// member is one name and value of a JSON object; object keeps their order,
// which zpool status uses for the vdev tree (D-8).
type member struct {
	key   string
	value json.RawMessage
}

type object []member

func decodeObject(b []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object: %v", tok)
	}
	var o object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("object key %v", tok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, member{key: k, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o object) get(k string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == k {
			return m.value, true
		}
	}
	return nil, false
}

// set replaces k's value in place, or inserts it before the member named
// before (appends when there is none).
func (o object) set(k string, v json.RawMessage, before string) object {
	out := slices.Clone(o)
	for i, m := range out {
		if m.key == k {
			out[i].value = v
			return out
		}
	}
	for i, m := range out {
		if m.key == before {
			return slices.Insert(out, i, member{key: k, value: v})
		}
	}
	return append(out, member{key: k, value: v})
}

func (o object) encode() []byte {
	b := []byte{'{'}
	for i, m := range o {
		if i > 0 {
			b = append(b, ',')
		}
		k, _ := json.Marshal(m.key) // a string always marshals
		b = append(b, k...)
		b = append(b, ':')
		b = append(b, m.value...)
	}
	return append(b, '}')
}

// setScan returns raw with the given members set (numbers or strings).
func setScan(raw json.RawMessage, values map[string]any) (json.RawMessage, error) {
	o, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := json.Marshal(values[k])
		if err != nil {
			return nil, err
		}
		o = o.set(k, v, "")
	}
	return o.encode(), nil
}

// vectorScan is the scan_stats of the first pool of a zpool status vector.
func vectorScan(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_, pools, err := statusPools(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("%s: no pool", path)
	}
	p, err := decodeObject(pools[0].value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	raw, ok := p.get("scan_stats")
	if !ok {
		return nil, fmt.Errorf("%s: no scan_stats", path)
	}
	return raw, nil
}
