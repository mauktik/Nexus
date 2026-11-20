// Tests of planning/storage-provider/test-cases.md TC-STG-75 to TC-STG-80
// (WI 3.5, F-ZFS of test-strategy.md, Fake Fidelity Contracts).
//
// Each test runs on the home-server capture, which capturegate.Require skips
// while WI 1.2 has not committed it, and on a layout fixture: a capture
// directory written in t.TempDir() in capture.sh's layout (PROVENANCE
// commands, `Exit <code>` lines, .stderr files, [noeol]) whose zpool status
// documents are the WI 1.1 synthetic vectors under the pool names the test
// cases use. The fixture keeps the fake's own rules under test before the
// host capture lands; it is never committed. Expected values are read from
// the capture files by this file's own minimal reader, never from the fake.

package storagetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// t0 is the fake clock of TC-STG-77 and TC-STG-80, whole seconds because
// zpool and zfs print Unix seconds.
var t0 = time.Date(2026, 11, 19, 5, 10, 0, 0, time.UTC)

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

// recorder is a testing.TB whose Errorf is recorded instead of failing the
// test; everything else goes to the real test.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.errs)
}

// ---- the test's own capture reader ----

// capturedDir is where a capture named by NewFake's argument lies, from this
// package's directory (the test's working directory).
func capturedDir(capture string) string {
	if filepath.IsAbs(capture) {
		return capture
	}
	return filepath.Join("..", "testdata", "captures", capture)
}

type provCmd struct {
	file  string
	argv  []string
	noeol bool
}

// zfsCommands lists PROVENANCE's commands whose program is zpool or zfs, with
// capture.sh's `/usr/bin/env -i VAR=…` prefix removed.
func zfsCommands(t *testing.T, dir string) []provCmd {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		t.Fatal(err)
	}
	var out []provCmd
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if line == "commands:" {
			in = true
			continue
		}
		if !in || !strings.HasPrefix(line, "  ") {
			in = in && strings.HasPrefix(line, "  ")
			continue
		}
		name, cmd, _ := strings.Cut(strings.TrimPrefix(line, "  "), ": ")
		c := provCmd{file: name}
		cmd, c.noeol = strings.CutSuffix(cmd, " [noeol]")
		f := strings.Fields(cmd)
		if len(f) > 0 && f[0] == "/usr/bin/env" {
			f = f[1:]
			for len(f) > 0 && (f[0] == "-i" || strings.Contains(f[0], "=")) {
				f = f[1:]
			}
		}
		if len(f) == 0 {
			continue
		}
		if base := filepath.Base(f[0]); base != "zpool" && base != "zfs" {
			continue
		}
		c.argv = f
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatalf("%s: no zpool or zfs command in PROVENANCE", dir)
	}
	return out
}

// expected is a capture file as the tool wrote it: stdout without the Exit
// line (and without the newline capture.sh added for [noeol]), the .stderr
// file and the exit code.
func expected(t *testing.T, dir string, c provCmd) zfs.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, c.file))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(filepath.Join(dir, c.file+".stderr"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(string(b), "\n")
	i := strings.LastIndex(body, "\n")
	code, err := strconv.Atoi(strings.TrimPrefix(body[i+1:], "Exit "))
	if err != nil {
		t.Fatalf("%s: last line %q is not Exit <code>", c.file, body[i+1:])
	}
	stdout := body[:i+1]
	if c.noeol {
		stdout = strings.TrimSuffix(stdout, "\n")
	}
	return zfs.Result{Stdout: []byte(stdout), Stderr: stderr, ExitCode: code}
}

func absent(r zfs.Result) bool {
	return r.ExitCode == 127 && string(r.Stderr) == "not found\n" && len(r.Stdout) == 0
}

func sameResult(t *testing.T, what string, got, want zfs.Result) {
	t.Helper()
	if !bytes.Equal(got.Stdout, want.Stdout) {
		t.Errorf("%s: stdout differs from the capture:\n got %q\nwant %q", what, clip(got.Stdout), clip(want.Stdout))
	}
	if !bytes.Equal(got.Stderr, want.Stderr) {
		t.Errorf("%s: stderr %q, want %q", what, got.Stderr, want.Stderr)
	}
	if got.ExitCode != want.ExitCode {
		t.Errorf("%s: exit %d, want %d", what, got.ExitCode, want.ExitCode)
	}
}

func clip(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// rows is a -H listing's rows, split on tabs.
func rows(b []byte) [][]string {
	var out [][]string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, strings.Split(l, "\t"))
		}
	}
	return out
}

// fileResult reads one capture file by name (any capture, not only zfs).
func fileResult(t *testing.T, dir, name string) zfs.Result {
	t.Helper()
	return expected(t, dir, provCmd{file: name})
}

// ---- the layout fixture ----

const envPrefix = "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "

type fixtureFile struct {
	name, argv     string
	stdout, stderr string
	exit           int
	noeol          bool
}

func writeCapture(t *testing.T, dir string, files []fixtureFile) {
	t.Helper()
	var cmds strings.Builder
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		out := f.stdout
		mark := ""
		if f.noeol {
			out += "\n"
			mark = " [noeol]"
		}
		out += fmt.Sprintf("Exit %d\n", f.exit)
		if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".stderr", []byte(f.stderr), 0o644); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(f.name, "sandbox-probe/") {
			fmt.Fprintf(&cmds, "  %s: %s%s\n", f.name, f.argv, mark)
		}
	}
	prov := "host: layout-fixture\nsource: host\ndate: 2026-11-19T05:00:00Z\ntimezone: UTC\nutc-offset: +0000\n" +
		"zfs-version: zfs-2.4.1-1\ncaptured-as: root\noptions: none\ncommands:\n" + cmds.String()
	if err := os.WriteFile(filepath.Join(dir, "PROVENANCE"), []byte(prov), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusFixture is the synthetic finished-scrub vector's pool under each of
// names (its "tank" strings renamed), in one zpool status document.
func statusFixture(t *testing.T, names ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "captures", "synthetic", "zpool-status-scrub-finished-repairs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var pools map[string]json.RawMessage
	if err := json.Unmarshal(doc["pools"], &pools); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "{\"output_version\":%s,\"pools\":{", doc["output_version"])
	for i, n := range names {
		if i > 0 {
			out.WriteString(",")
		}
		fmt.Fprintf(&out, "%q:%s", n, bytes.ReplaceAll(pools["tank"], []byte(`"tank"`), []byte(strconv.Quote(n))))
	}
	out.WriteString("}}\n")
	return out.String()
}

// homeServerFixture writes a capture in the home-server layout: pools z1, z2
// and z3, datasets z1/ds, z1/ds/share and z1/ds/other, with probe deciding
// whether sandbox-probe/write-probe.txt (the --write-probe denial) exists.
func homeServerFixture(t *testing.T, probe bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home-server")
	zp, zf := "/usr/sbin/zpool", "/usr/sbin/zfs"
	status := statusFixture(t, "z1", "z2", "z3")
	version, err := os.ReadFile(filepath.Join("..", "testdata", "captures", "upstream-openzfs-2.4.1", "zfs-version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ds := strings.Join([]string{
		"z1\tfilesystem\t4096000\t9000000\t98304\t0\t0\t0\t0\tlz4\t1.00\t131072\t/z1\tyes\toff\t-",
		"z1/ds\tfilesystem\t3072000\t9000000\t131072\t8192\t0\t0\t0\tlz4\t1.20\t131072\t/z1/ds\tyes\toff\t-",
		"z1/ds/other\tfilesystem\t1024000\t9000000\t1024000\t0\t0\t0\t0\tlz4\t1.00\t131072\t/media/other\tyes\toff\t-",
		"z1/ds/share\tfilesystem\t2048000\t9000000\t2000000\t40960\t0\t0\t0\tlz4\t1.50\t1048576\t/media/share\tyes\toff\t-",
		"z2\tfilesystem\t1000\t5000\t1000\t0\t0\t0\t0\tlz4\t1.00\t131072\t/z2\tyes\toff\t-",
		"z3\tfilesystem\t1000\t5000\t1000\t0\t0\t0\t0\tlz4\t1.00\t131072\t/z3\tyes\toff\t-",
	}, "\n") + "\n"
	snaps := "z1/ds@auto-1\t1794700000\t8192\t131072\t0\t-\n" +
		"z1/ds/share@auto-1\t1794700000\t40960\t1990000\t0\t-\n" +
		"z1/ds/share@nexus-20261118T000000Z\t1794790000\t0\t2000000\t0\top-00000000000000aa\n" +
		"z2@auto-1\t1794700000\t0\t1000\t0\t-\n"
	var times strings.Builder
	for _, r := range rows([]byte(snaps)) {
		fmt.Fprintf(&times, "%s\t%s\n", r[0], r[1])
	}
	pl := zfs.PoolListColumns
	files := []fixtureFile{
		{name: "host/id-u.txt", argv: "id -u", stdout: "0\n"},
		{name: "zfs/zfs-version.txt", argv: envPrefix + zf + " version", stdout: string(version)},
		{name: "zfs/zfs-version.json", argv: envPrefix + zf + " version -j", noeol: true,
			stdout: `{"output_version":{"command":"zfs version","vers_major":0,"vers_minor":1},"zfs_version":{"userland":"zfs-2.4.1-1","kernel":"zfs-kmod-2.4.1-1"}}`},
		{name: "zfs/zpool-list.json", argv: envPrefix + zp + " list -j --json-int -o " + pl,
			stdout: `{"output_version":{"command":"zpool list","vers_major":0,"vers_minor":1},"pools":{"z1":{"name":"z1","type":"POOL","state":"ONLINE","properties":{"health":{"value":"ONLINE","source":{"type":"NONE","data":"-"}}}},"z2":{"name":"z2","type":"POOL","state":"ONLINE","properties":{"health":{"value":"ONLINE","source":{"type":"NONE","data":"-"}}}},"z3":{"name":"z3","type":"POOL","state":"ONLINE","properties":{"health":{"value":"ONLINE","source":{"type":"NONE","data":"-"}}}}}}` + "\n"},
		{name: "zfs/zpool-list.txt", argv: envPrefix + zp + " list -H -p -o " + pl,
			stdout: "z1\t1000\t500\t500\t50\t1\t1.00\tONLINE\t-\toff\nz2\t1000\t100\t900\t10\t0\t1.00\tONLINE\t-\toff\nz3\t1000\t100\t900\t10\t0\t1.00\tONLINE\t-\toff\n"},
		{name: "zfs/zpool-list-names.txt", argv: envPrefix + zp + " list -H -o name", stdout: "z1\nz2\nz3\n"},
		{name: "zfs/zpool-status.json", argv: envPrefix + zp + " status -j --json-int", stdout: status},
		{name: "zfs/zpool-status-v.json", argv: envPrefix + zp + " status -j --json-int -v", stdout: status},
		{name: "zfs/zpool-status-v.txt", argv: envPrefix + zp + " status -p -v", stdout: "  pool: z1\n state: ONLINE\n\nerrors: No known data errors\n"},
		{name: "zfs/zpool-get-z1.json", argv: envPrefix + zp + " get -j --json-int all z1",
			stdout: `{"output_version":{"command":"zpool get","vers_major":0,"vers_minor":1},"pools":{"z1":{"name":"z1","type":"POOL","properties":{}}}}` + "\n"},
		{name: "zfs/zpool-get-z1.txt", argv: envPrefix + zp + " get -H -p -o name,property,value,source all z1", stdout: "z1\tsize\t1000\t-\n"},
		{name: "zfs/zfs-list-datasets.json", argv: envPrefix + zf + " list -j --json-int -t filesystem,volume -o " + zfs.DatasetColumns,
			stdout: `{"output_version":{"command":"zfs list","vers_major":0,"vers_minor":1},"datasets":{"z1":{"name":"z1","type":"FILESYSTEM","pool":"z1","properties":{}},"z1/ds":{"name":"z1/ds","type":"FILESYSTEM","pool":"z1","properties":{}},"z1/ds/other":{"name":"z1/ds/other","type":"FILESYSTEM","pool":"z1","properties":{}},"z1/ds/share":{"name":"z1/ds/share","type":"FILESYSTEM","pool":"z1","properties":{}},"z2":{"name":"z2","type":"FILESYSTEM","pool":"z2","properties":{}},"z3":{"name":"z3","type":"FILESYSTEM","pool":"z3","properties":{}}}}` + "\n"},
		{name: "zfs/zfs-list-datasets.txt", argv: envPrefix + zf + " list -H -p -t filesystem,volume -o " + zfs.DatasetColumns, stdout: ds},
		{name: "zfs/zfs-list-snapshots.json", argv: envPrefix + zf + " list -j --json-int -t snapshot -o " + zfs.SnapshotColumns,
			stdout: `{"output_version":{"command":"zfs list","vers_major":0,"vers_minor":1},"datasets":{}}` + "\n"},
		{name: "zfs/zfs-list-snapshots.txt", argv: envPrefix + zf + " list -H -p -t snapshot -o " + zfs.SnapshotColumns, stdout: snaps},
		{name: "zfs/zfs-list-snapshots-creation.txt", argv: envPrefix + zf + " list -H -p -t snapshot -o name,creation", stdout: times.String()},
		{name: "zfs/negative-zpool-list.json", argv: envPrefix + zp + " list -j --json-int -o " + pl + " nexus-capture-absent",
			stderr: "cannot open 'nexus-capture-absent': no such pool\n", exit: 1},
		{name: "zfs/negative-zpool-status.json", argv: envPrefix + zp + " status -j --json-int nexus-capture-absent",
			stderr: "cannot open 'nexus-capture-absent': no such pool\n", exit: 1},
		{name: "zfs/negative-zfs-list.json", argv: envPrefix + zf + " list -j --json-int nexus-capture-absent/x",
			stderr: "cannot open 'nexus-capture-absent/x': dataset does not exist\n", exit: 1},
	}
	if probe {
		files = append(files, fixtureFile{name: "sandbox-probe/write-probe.txt",
			stderr: "cannot create snapshots : permission denied\n(probe)\n", exit: 1})
	}
	writeCapture(t, dir, files)
	return dir
}

// noZFSFixture is a host without ZFS: every zpool and zfs command is Exit
// 127 with `not found`, as capture.sh records a missing program.
func noZFSFixture(t *testing.T, host string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), host)
	zp, zf := "/usr/sbin/zpool", "/usr/sbin/zfs"
	nf := func(name, argv string) fixtureFile {
		return fixtureFile{name: name, argv: envPrefix + argv, stderr: "not found\n", exit: 127}
	}
	writeCapture(t, dir, []fixtureFile{
		nf("zfs/zfs-version.txt", zf+" version"),
		nf("zfs/zpool-list.json", zp+" list -j --json-int -o "+zfs.PoolListColumns),
		nf("zfs/zpool-list-names.txt", zp+" list -H -o name"),
		nf("zfs/zpool-status.json", zp+" status -j --json-int"),
	})
	return dir
}

// eachCapture runs body on the named host capture (skipped while absent) and
// on the layout fixture.
func eachCapture(t *testing.T, host string, fixture func(*testing.T) string, body func(t *testing.T, capture string)) {
	t.Run(host, func(t *testing.T) { body(t, host) })
	t.Run(host+"-layout-fixture", func(t *testing.T) { body(t, fixture(t)) })
}

func homeServer(t *testing.T) string { return homeServerFixture(t, false) }

func run(t *testing.T, f *Fake, argv []string) zfs.Result {
	t.Helper()
	if argv == nil {
		t.Fatal("nil argv")
	}
	res, err := f.Run(context.Background(), argv)
	if err != nil {
		t.Fatalf("%q: %v", argv, err)
	}
	return res
}

// poolScan is pool's scan in a zpool status JSON document, by the zfs parser.
func poolScan(t *testing.T, doc []byte, pool string) *zfs.Scan {
	t.Helper()
	pools, err := zfs.ParseStatusJSON(doc)
	if err != nil {
		t.Fatalf("zpool status: %v", err)
	}
	for _, p := range pools {
		if p.Name == pool {
			return p.Scan
		}
	}
	t.Fatalf("pool %s not in the zpool status output", pool)
	return nil
}

func scanWords(s *zfs.Scan) string {
	if s == nil {
		return "<nil>"
	}
	return s.Function + "/" + s.State
}

// TC-STG-75: the fake replays captured stdout, stderr and exit code byte for byte, and fails the test on an unknown argv. Owner WI 3.5. Requirements: NFR-8, NFR-4.
func TestTCSTG75_TheFakeReplaysCapturedStdoutStderrAnd(t *testing.T) {
	eachCapture(t, "home-server", homeServer, func(t *testing.T, capture string) {
		rec := &recorder{TB: t}
		f := NewFake(rec, capture)
		dir := capturedDir(capture)
		cmds := zfsCommands(t, dir)
		for _, c := range cmds {
			want := expected(t, dir, c)
			got, err := f.Run(context.Background(), c.argv)
			if absent(want) {
				if !errors.Is(err, zfs.ErrNotStarted) || !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s %q: err %v, want ErrNotStarted and fs.ErrNotExist", c.file, c.argv, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %q: %v", c.file, c.argv, err)
				continue
			}
			sameResult(t, c.file, got, want)
		}
		if errs := rec.errors(); len(errs) != 0 {
			t.Fatalf("captured argv were refused: %q", errs)
		}
		if got := len(f.Reads()); got != len(cmds) {
			t.Errorf("Reads() has %d argv, want %d", got, len(cmds))
		}

		cols := strings.Split(zfs.DatasetColumns, ",")
		short := slices.Clone(f.Argv(zfs.CmdDatasetList))
		short[slices.Index(short, zfs.DatasetColumns)] = strings.Join(cols[:len(cols)-1], ",")
		for _, argv := range [][]string{{f.Paths().Zpool, "iostat"}, {"/bin/sh"}, short} {
			before := len(rec.errors())
			res, err := f.Run(context.Background(), argv)
			if !errors.Is(err, zfs.ErrNotStarted) {
				t.Errorf("%q: err %v, want ErrNotStarted", argv, err)
			}
			if res.ExitCode != 0 || res.Stdout != nil || res.Stderr != nil {
				t.Errorf("%q: a result %+v beside the error", argv, res)
			}
			errs := rec.errors()
			if len(errs) != before+1 || !strings.Contains(errs[len(errs)-1], fmt.Sprintf("%q", argv)) {
				t.Errorf("%q: Errorf calls %q, want one naming the argv", argv, errs[before:])
			}
		}
		if len(f.Writes()) != 0 {
			t.Errorf("Writes() = %q after reads only", f.Writes())
		}
	})
}

// TC-STG-76: the fake replays captured negative cases and the no-ZFS hosts. Owner WI 3.5. Requirements: FR-3, FR-5, FR-7, NFR-8.
func TestTCSTG76_TheFakeReplaysCapturedNegativeCasesAnd(t *testing.T) {
	eachCapture(t, "home-server", homeServer, func(t *testing.T, capture string) {
		f := NewFake(t, capture)
		dir := capturedDir(capture)
		var neg int
		for _, c := range zfsCommands(t, dir) {
			if !strings.HasPrefix(filepath.Base(c.file), "negative-") {
				continue
			}
			neg++
			want := expected(t, dir, c)
			if want.ExitCode == 0 {
				t.Errorf("%s: the negative capture exited 0", c.file)
			}
			got, err := f.Run(context.Background(), c.argv)
			if err != nil {
				t.Errorf("%s: a Go error %v, want the captured exit", c.file, err)
				continue
			}
			sameResult(t, c.file, got, want)
		}
		if neg != 3 {
			t.Errorf("%d negative captures, want 3 (zpool list, zpool status, zfs list)", neg)
		}

		// Another missing name is the same libzfs refusal with that name.
		negStatus := fileResult(t, dir, "zfs/negative-zpool-status.json")
		got := run(t, f, f.Argv(zfs.CmdPoolStatusVerbose, "nosuchpool"))
		wantErr := bytes.ReplaceAll(negStatus.Stderr, []byte("nexus-capture-absent"), []byte("nosuchpool"))
		if got.ExitCode != negStatus.ExitCode || !bytes.Equal(got.Stderr, wantErr) {
			t.Errorf("status of a missing pool: exit %d %q, want %d %q", got.ExitCode, got.Stderr, negStatus.ExitCode, wantErr)
		}
		negZfs := fileResult(t, dir, "zfs/negative-zfs-list.json")
		got = run(t, f, f.Argv(zfs.CmdSnapshotList, "z1/nosuch"))
		wantErr = bytes.ReplaceAll(negZfs.Stderr, []byte("nexus-capture-absent/x"), []byte("z1/nosuch"))
		if got.ExitCode != negZfs.ExitCode || !bytes.Equal(got.Stderr, wantErr) || len(got.Stdout) != 0 {
			t.Errorf("snapshots of a missing dataset: exit %d %q %q, want %d %q", got.ExitCode, got.Stderr, got.Stdout, negZfs.ExitCode, wantErr)
		}
	})
	for _, host := range []string{"pihole", "public-nginx"} {
		eachCapture(t, host, func(t *testing.T) string { return noZFSFixture(t, host) }, func(t *testing.T, capture string) {
			f := NewFake(t, capture)
			argv := zfs.Argv(f.Paths(), f.Format(), zfs.CmdPoolNames)
			res, err := f.Run(context.Background(), argv)
			if !errors.Is(err, zfs.ErrNotStarted) || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%q: err %v, want ErrNotStarted and fs.ErrNotExist", argv, err)
			}
			if res.ExitCode != 0 || res.Stdout != nil {
				t.Errorf("%q: a result %+v beside the error", argv, res)
			}
			// A write on a host without the tool is not started either.
			if _, err := f.Run(context.Background(), zfs.Argv(f.Paths(), f.Format(), zfs.CmdScrubStart, "z1")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("scrub on %s: err %v, want fs.ErrNotExist", host, err)
			}
		})
	}
}

// Refusal texts of OpenZFS 2.4.1 (1c702dda346a59e05cfd3029569bbb1d5d91c54b):
// zpool_scan_range's errbuf (lib/libzfs/libzfs_pool.c L2845-L2865) followed
// by libzfs_error_description (lib/libzfs/libzfs_util.c L253-L263, L137-L138)
// as zfs_verror prints them (L369-L370).
const (
	wantScrubbing   = "cannot scrub z1: currently scrubbing; use 'zpool scrub -s' to cancel scrub\n"
	wantNoScrub     = "cannot cancel scrubbing z1: there is no active scrub\n"
	wantResilvering = "cannot scrub z2: currently resilvering\n"
)

// TC-STG-77: the fake's scan-state machine encodes ZFS's own refusals. Owner WI 3.5. Requirements: FR-13, NFR-8.
func TestTCSTG77_TheFakeSScanStateMachineEncodes(t *testing.T) {
	eachCapture(t, "home-server", homeServer, func(t *testing.T, capture string) {
		f := NewFake(t, capture, Options{CapSysAdmin: true, Now: clockAt(t0)})
		status := func(pool string) []byte { return run(t, f, f.Argv(zfs.CmdPoolStatus, pool)).Stdout }
		z2Before := poolScan(t, status("z2"), "z2")
		var wantWrites [][]string
		write := func(cmd zfs.Command, pool string) zfs.Result {
			argv := f.Argv(cmd, pool)
			wantWrites = append(wantWrites, argv)
			return run(t, f, argv)
		}

		// (1) scrub z1 starts the SCANNING vector at the clock's now.
		if r := write(zfs.CmdScrubStart, "z1"); r.ExitCode != 0 || len(r.Stderr) != 0 {
			t.Fatalf("scrub z1: exit %d %q", r.ExitCode, r.Stderr)
		}
		s := poolScan(t, status("z1"), "z1")
		if scanWords(s) != "scrub/scanning" || s.Start == nil || !s.Start.Equal(t0) {
			t.Errorf("z1 after scrub: %s start %v, want scrub/scanning at %v", scanWords(s), s.Start, t0)
		}
		if all := poolScan(t, run(t, f, f.Argv(zfs.CmdPoolStatusVerbose)).Stdout, "z1"); scanWords(all) != "scrub/scanning" {
			t.Errorf("z1 in the all-pools -v status: %s, want scrub/scanning", scanWords(all))
		}
		if z2 := poolScan(t, status("z2"), "z2"); scanWords(z2) != scanWords(z2Before) || fmt.Sprint(z2.Start) != fmt.Sprint(z2Before.Start) {
			t.Errorf("z2 changed: %s, want %s", scanWords(z2), scanWords(z2Before))
		}

		// (2) a second scrub is ZFS's EBUSY refusal.
		if r := write(zfs.CmdScrubStart, "z1"); r.ExitCode != 1 || string(r.Stderr) != wantScrubbing {
			t.Errorf("second scrub: exit %d %q, want 1 %q", r.ExitCode, r.Stderr, wantScrubbing)
		}

		// (3) stop cancels it.
		if r := write(zfs.CmdScrubStop, "z1"); r.ExitCode != 0 {
			t.Errorf("scrub -s z1: exit %d %q", r.ExitCode, r.Stderr)
		}
		if s := poolScan(t, status("z1"), "z1"); scanWords(s) != "scrub/canceled" {
			t.Errorf("z1 after stop: %s, want scrub/canceled", scanWords(s))
		}

		// (4) a second stop has nothing to cancel.
		if r := write(zfs.CmdScrubStop, "z1"); r.ExitCode != 1 || string(r.Stderr) != wantNoScrub {
			t.Errorf("second stop: exit %d %q, want 1 %q", r.ExitCode, r.Stderr, wantNoScrub)
		}

		// (5) a resilver refuses a scrub.
		f.SeedScan("z2", VectorResilverScanning)
		if r := write(zfs.CmdScrubStart, "z2"); r.ExitCode != 1 || string(r.Stderr) != wantResilvering {
			t.Errorf("scrub during a resilver: exit %d %q, want 1 %q", r.ExitCode, r.Stderr, wantResilvering)
		}
		if s := poolScan(t, status("z2"), "z2"); scanWords(s) != "resilver/scanning" {
			t.Errorf("z2 after the refused scrub: %s, want resilver/scanning", scanWords(s))
		}

		// (6) a paused scrub is resumed.
		f.SeedScan("z3", VectorScrubPaused)
		if s := poolScan(t, status("z3"), "z3"); scanWords(s) != "scrub/paused" {
			t.Fatalf("seeded z3: %s, want scrub/paused", scanWords(s))
		}
		if r := write(zfs.CmdScrubStart, "z3"); r.ExitCode != 0 {
			t.Errorf("scrub of a paused z3: exit %d %q", r.ExitCode, r.Stderr)
		}
		if s := poolScan(t, status("z3"), "z3"); scanWords(s) != "scrub/scanning" {
			t.Errorf("z3 after resume: %s, want scrub/scanning", scanWords(s))
		}

		if got := f.Writes(); !slices.EqualFunc(got, wantWrites, slices.Equal) {
			t.Errorf("Writes() = %q, want %q", got, wantWrites)
		}
		for _, r := range f.Reads() {
			if slices.Contains(r, "scrub") {
				t.Errorf("a write %q among Reads()", r)
			}
		}
	})
}

// TC-STG-77 (beyond the listed steps): stop during a resilver, scrub of a
// missing pool, FinishImmediately and StaleFinished.
func TestTCSTG77_ScanStateEdges(t *testing.T) {
	f := NewFake(t, homeServer(t), Options{CapSysAdmin: true, Now: clockAt(t0)})
	f.SeedScan("z2", VectorResilverScanning)
	if r := run(t, f, f.Argv(zfs.CmdScrubStop, "z2")); r.ExitCode != 1 || string(r.Stderr) != "cannot cancel scrubbing z2: currently resilvering\n" {
		t.Errorf("stop during a resilver: exit %d %q", r.ExitCode, r.Stderr)
	}
	if r := run(t, f, f.Argv(zfs.CmdScrubStart, "nosuch")); r.ExitCode != 1 || string(r.Stderr) != "cannot open 'nosuch': no such pool\n" {
		t.Errorf("scrub of a missing pool: exit %d %q", r.ExitCode, r.Stderr)
	}
	f.FinishImmediately("z1")
	run(t, f, f.Argv(zfs.CmdScrubStart, "z1"))
	s := poolScan(t, run(t, f, f.Argv(zfs.CmdPoolStatus, "z1")).Stdout, "z1")
	if scanWords(s) != "scrub/finished" || s.Start == nil || !s.Start.Equal(t0) {
		t.Errorf("FinishImmediately: %s start %v, want scrub/finished at %v", scanWords(s), s.Start, t0)
	}
	f.StaleFinished("z3")
	s = poolScan(t, run(t, f, f.Argv(zfs.CmdPoolStatus, "z3")).Stdout, "z3")
	if scanWords(s) != "scrub/finished" || s.Start == nil || !s.Start.Before(t0.Add(-5*time.Second)) {
		t.Errorf("StaleFinished: %s start %v, want finished before %v", scanWords(s), s.Start, t0.Add(-5*time.Second))
	}
	// After a scan change the text form of zpool status is not encoded: the
	// fake refuses it instead of replaying the stale capture.
	rec := &recorder{TB: t}
	g := NewFake(rec, homeServer(t), Options{CapSysAdmin: true, Now: clockAt(t0)})
	run(t, g, g.Argv(zfs.CmdScrubStart, "z1"))
	text := zfs.Argv(g.Paths(), zfs.Text, zfs.CmdPoolStatusVerbose)
	if _, err := g.Run(context.Background(), text); !errors.Is(err, zfs.ErrNotStarted) || len(rec.errors()) != 1 {
		t.Errorf("text status after a scrub: err %v, Errorf %q; want a refusal", err, rec.errors())
	}
}

// TC-STG-78: writes without CAP_SYS_ADMIN are denied as inside the sandbox; reads never are. Owner WI 3.5. Requirements: CON-1, NFR-8.
func TestTCSTG78_WritesWithoutCAPSYSADMINAreDenied(t *testing.T) {
	check := func(t *testing.T, capture string) {
		f := NewFake(t, capture, Options{Now: clockAt(t0)})
		dir := capturedDir(capture)
		statusBefore := run(t, f, f.Argv(zfs.CmdPoolStatusVerbose))
		snapsBefore := run(t, f, f.Argv(zfs.CmdSnapshotList))

		// zpool scrub: zfs_secpolicy_config's EPERM (module/zfs/zfs_ioctl.c
		// L1089-L1097, L7640-L7643) through zpool_standard_error.
		if r := run(t, f, f.Argv(zfs.CmdScrubStart, "z1")); r.ExitCode != 1 || string(r.Stderr) != "cannot scrub z1: permission denied\n" {
			t.Errorf("scrub z1: exit %d %q, want 1 permission denied", r.ExitCode, r.Stderr)
		}
		wantSnap := zfs.Result{ExitCode: 1, Stderr: []byte("cannot create snapshots : permission denied\n")}
		if _, err := os.Stat(filepath.Join(dir, "sandbox-probe", "write-probe.txt")); err == nil {
			wantSnap = fileResult(t, dir, "sandbox-probe/write-probe.txt")
		}
		r := run(t, f, f.Argv(zfs.CmdSnapshotCreate, "x", "z1/ds/share@nexus-t"))
		if r.ExitCode != wantSnap.ExitCode || !bytes.Equal(r.Stderr, wantSnap.Stderr) {
			t.Errorf("snapshot: exit %d %q, want %d %q", r.ExitCode, r.Stderr, wantSnap.ExitCode, wantSnap.Stderr)
		}
		if len(f.Writes()) != 2 {
			t.Errorf("Writes() = %q, want the two denied writes", f.Writes())
		}
		sameResult(t, "status after the denials", run(t, f, f.Argv(zfs.CmdPoolStatusVerbose)), statusBefore)
		sameResult(t, "snapshots after the denials", run(t, f, f.Argv(zfs.CmdSnapshotList)), snapsBefore)

		// Step 2: every read answers as captured.
		for _, c := range zfsCommands(t, dir) {
			want := expected(t, dir, c)
			if absent(want) {
				continue
			}
			sameResult(t, c.file, run(t, f, c.argv), want)
		}
	}
	eachCapture(t, "home-server", homeServer, check)
	t.Run("layout-fixture-write-probe", func(t *testing.T) { check(t, homeServerFixture(t, true)) })
}

// TC-STG-79: hang honours the context and stuck does not. Owner WI 3.5. Requirements: NFR-2, NFR-8.
func TestTCSTG79_HangHonoursTheContextAndStuckDoes(t *testing.T) {
	eachCapture(t, "home-server", homeServer, func(t *testing.T, capture string) {
		hang := NewFake(t, capture)
		status := hang.Argv(zfs.CmdPoolStatus)
		hang.Hang(status)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		res, err := hang.Run(ctx, status)
		took := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Hang: err %v, want ctx.Err()", err)
		}
		if res.Stdout != nil || res.Stderr != nil || res.ExitCode != 0 {
			t.Errorf("Hang: a result %+v", res)
		}
		if took < 50*time.Millisecond || took > time.Second {
			t.Errorf("Hang returned after %v, want about 50ms", took)
		}

		stuck := NewFake(t, capture)
		release := make(chan struct{})
		stuck.Stuck(status, release)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel2()
		done := make(chan error, 1)
		start = time.Now()
		go func() {
			_, err := stuck.Run(ctx2, status)
			done <- err
		}()
		time.AfterFunc(200*time.Millisecond, func() { close(release) })
		select {
		case err := <-done:
			t.Fatalf("Stuck returned after %v (err %v), before release", time.Since(start), err)
		case <-time.After(150 * time.Millisecond):
		}
		err = <-done
		took = time.Since(start)
		if took < 200*time.Millisecond {
			t.Errorf("Stuck returned after %v, before release at 200ms", took)
		}
		// Exec reports the expired ctx once the process finally exits.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Stuck after release with an expired ctx: err %v, want ctx.Err()", err)
		}

		// Released before its deadline, the call answers from the capture.
		quick := make(chan struct{})
		close(quick)
		other := NewFake(t, capture)
		other.Stuck(status, quick)
		want := run(t, NewFake(t, capture), status)
		sameResult(t, "stuck then released", run(t, other, status), want)
	})
}

// TC-STG-80: the fake's snapshot creation shows in the next listing with the property, and a repeated name fails. Owner WI 3.5. Requirements: FR-14, FR-8, NFR-8.
func TestTCSTG80_TheFakeSSnapshotCreationShowsIn(t *testing.T) {
	eachCapture(t, "home-server", homeServer, func(t *testing.T, capture string) {
		f := NewFake(t, capture, Options{CapSysAdmin: true, Now: clockAt(t0)})
		dir := capturedDir(capture)
		const id = "op-0123456789abcdef"
		name := "z1/ds/share@nexus-20261119T051000Z"
		create := f.Argv(zfs.CmdSnapshotCreate, id, name)
		listOf := func(cmd zfs.Command, ds string) []zfs.Snapshot {
			argv := f.Argv(cmd)
			if ds != "" {
				argv = f.Argv(cmd, ds)
			}
			if !slices.Contains(argv, "-H") || !slices.Contains(argv, "-p") {
				t.Fatalf("%q is not the -H -p text form", argv)
			}
			snaps, err := zfs.ParseSnapshotsText(run(t, f, argv).Stdout)
			if err != nil {
				t.Fatal(err)
			}
			return snaps
		}
		find := func(snaps []zfs.Snapshot, n string) *zfs.Snapshot {
			for i := range snaps {
				if snaps[i].Name == n {
					return &snaps[i]
				}
			}
			return nil
		}
		before := listOf(zfs.CmdSnapshotList, "z1/ds/share")

		// (1)
		if r := run(t, f, create); r.ExitCode != 0 || len(r.Stderr) != 0 {
			t.Fatalf("snapshot: exit %d %q", r.ExitCode, r.Stderr)
		}
		after := listOf(zfs.CmdSnapshotList, "z1/ds/share")
		s := find(after, name)
		if s == nil || !s.CreatedAt.Equal(t0) || s.CreatedBy != id {
			t.Fatalf("listing after create: %+v, want %s created %v by %s", s, name, t0, id)
		}
		if len(after) != len(before)+1 || after[len(after)-1].Name != name {
			t.Errorf("listing after create has %d rows (newest %s), want %d with the new one last", len(after), after[len(after)-1].Name, len(before)+1)
		}
		if find(listOf(zfs.CmdSnapshotTimes, "z1/ds/share"), name) == nil || find(listOf(zfs.CmdSnapshotList, ""), name) == nil {
			t.Error("the new snapshot is missing from the name,creation or the all-snapshots listing")
		}
		if got := listOf(zfs.CmdSnapshotGet, name); len(got) != 1 || got[0].CreatedBy != id {
			t.Errorf("the helper's readback: %+v", got)
		}

		// (2)
		want := "cannot create snapshot '" + name + "': dataset already exists\n"
		if r := run(t, f, create); r.ExitCode != 1 || string(r.Stderr) != want {
			t.Errorf("repeat: exit %d %q, want 1 %q", r.ExitCode, r.Stderr, want)
		}

		// (3) -r on z1/ds: z1/ds and every descendant in the capture.
		var family []string
		for _, r := range rows(fileResult(t, dir, "zfs/zfs-list-datasets.txt").Stdout) {
			if r[0] == "z1/ds" || strings.HasPrefix(r[0], "z1/ds/") {
				family = append(family, r[0])
			}
		}
		const n2 = "nexus-20261119T051100Z"
		if r := run(t, f, f.Argv(zfs.CmdSnapshotCreateRecursive, id, "z1/ds@"+n2)); r.ExitCode != 0 {
			t.Fatalf("snapshot -r: exit %d %q", r.ExitCode, r.Stderr)
		}
		rec := listOf(zfs.CmdSnapshotListRecursive, "z1/ds")
		for _, ds := range family {
			if s := find(rec, ds+"@"+n2); s == nil || !s.CreatedAt.Equal(t0) || s.CreatedBy != id {
				t.Errorf("-r: %s@%s is %+v", ds, n2, s)
			}
		}
		if len(family) < 2 {
			t.Errorf("the capture has %d datasets under z1/ds, want z1/ds and a descendant", len(family))
		}
		// A recursive name that exists on one descendant fails whole.
		r := run(t, f, f.Argv(zfs.CmdSnapshotCreateRecursive, id, "z1/ds@nexus-20261119T051000Z"))
		wantR := "cannot create snapshot '" + name + "': dataset already exists\nno snapshots were created\n"
		if r.ExitCode != 1 || string(r.Stderr) != wantR {
			t.Errorf("-r collision: exit %d %q, want 1 %q", r.ExitCode, r.Stderr, wantR)
		}
		if find(listOf(zfs.CmdSnapshotList, "z1/ds"), "z1/ds@nexus-20261119T051000Z") != nil {
			t.Error("a failed recursive snapshot created z1/ds@nexus-20261119T051000Z")
		}
		if len(f.Writes()) != 4 {
			t.Errorf("Writes() = %q, want 4", f.Writes())
		}
	})
}
