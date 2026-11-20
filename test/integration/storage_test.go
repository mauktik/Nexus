//go:build integration

// T1 of the storage provider (storage-provider WI 5.3): the installed nexusd
// under the exact shipped unit (systemd) or the shipped OpenRC script, driven
// over REST, with zpool and zfs replaced by the F-ZFS fake replaying the
// committed home-server capture (storage_fakezfs_test.go). No real ZFS runs
// in T1 (A-42). Every assertion reads back from the container's own state:
// the fake's calls.log and reads.log (written by the fake processes
// themselves), /proc, the journal or the OpenRC log, stat, df, du and the
// audit file, never Nexus's word alone. Expected ZFS values are computed here
// from the capture's text files, the oracle of TC-STG-125 and TC-STG-128.
// `make test-harness RUN='^TestTCSTG|^TestStorageFakeZFS$'` selects these; a
// test written for the other init system skips through requireInit.

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/storagetest"
	"github.com/mauktik/nexus/internal/systemspec"
)

const (
	stgSrv    = "/srv/nx-storage"
	stgVzdump = stgSrv + "/vzdump"
	stgTM     = stgSrv + "/tm"
	// stgFUSEDir is TC-STG-223's never-answering FUSE mount.
	stgFUSEDir = "/run/nx-stuck-fuse"
	// stgPATH and stgLCAll are zfs.Exec's whole environment (D-3,
	// TC-STG-51).
	stgPATH  = "PATH=/usr/sbin:/usr/bin:/sbin:/bin"
	stgLCAll = "LC_ALL=C"
	// The planted Time Machine bundles and the sparsebundle's last backup.
	stgSparse   = "nx-mac.sparsebundle"
	stgBackupBd = "nx-mac2.backupbundle"
	// The reason of a provider with no ZFS tools and no paths (TC-STG-227).
	stgNoZFSReason = "zfs: /usr/sbin/zpool not found (nor /sbin/zpool); backups: not configured; timeMachine: not configured"
	stgNoDevReason = "zfs: /dev/zfs not found (the zfs kernel module is not loaded)"
)

// stgLastBackup is the completion date planted in the sparsebundle's XML
// SnapshotHistory.plist.
var stgLastBackup = time.Date(2026, 11, 18, 21, 40, 0, 0, time.UTC)

// stgPaths are the backups and Time Machine keys of the planted trees.
const stgPaths = "backups: {vzdumpPaths: [" + stgVzdump + "]}, timeMachine: {paths: [" + stgTM + "]}"

// stgDaemon is the installed nexusd with providers.storage, started under
// the exact unit or the shipped OpenRC script.
type stgDaemon struct {
	*nexusdInstance
	pid      int
	since    time.Time
	logStart int64 // OpenRC: /var/log/messages' size before the start
}

// stgYAML is the providers lines of a storage config: storage enabled with
// keys, or disabled when keys is "disabled", and the nexusd provider, whose
// nexusd.actions.reload the tests reload with.
func stgYAML(keys string) string {
	const nexusd = "  nexusd: {enabled: true}\n"
	if keys == "disabled" {
		return nexusd + "  storage: {enabled: false}\n"
	}
	if keys != "" {
		keys = ", " + keys
	}
	return nexusd + "  storage: {enabled: true" + keys + "}\n"
}

// stgFresh wipes the host and installs nexusd with install.sh.
func stgFresh(t *testing.T) {
	t.Helper()
	freshHost(t)
	runInstall(t)
}

// stgStart writes the config (host provider on, providers.storage keys),
// starts nexusd under the detected init system and waits until the storage
// provider's preflight has settled. The token holds every storage scope,
// host:read, and nexusd:read and nexusd:admin (for nexusd.actions.reload
// and its operation).
func stgStart(t *testing.T, keys string) *stgDaemon {
	t.Helper()
	writeConfig(t, hostConfig(true, stgYAML(keys)))
	d := &stgDaemon{since: time.Now().Add(-time.Second)}
	switch detectedInit() {
	case platform.Systemd:
		d.pid = startExact(t)
		requireExactUnit(t)
		t.Logf("nexusd.service: %s sha256 %s = deploy/nexusd.service, no drop-in; MainPID %d", installedUnit,
			sha(t, installedUnit), d.pid)
	case platform.OpenRC:
		if fi, err := os.Stat("/var/log/messages"); err == nil {
			d.logStart = fi.Size()
		}
		lib(t, "start-nexusd")
		if d.pid = openrcMainPID(t); d.pid == 0 {
			t.Fatalf("nexusd is not started after start-nexusd: rc-service says %s", rcStatus(t))
		}
		t.Logf("nexusd under the shipped OpenRC script: %s sha256 %s, PID %d", openrcScript, sha(t, openrcScript), d.pid)
	default:
		t.Fatalf("the harness has no supported init system (detected %s)", detectedInit())
	}
	d.nexusdInstance = &nexusdInstance{t: t, listen: exactAddr,
		token: createToken(t, "tc-stg", spec.ReadScope, spec.ScrubScope, spec.SnapshotScope, hostspec.ReadScope,
			nexusdspec.ReadScope, nexusdspec.AdminScope),
		client: &http.Client{Timeout: 2 * time.Minute}}
	waitUntil(t, time.Minute, "the storage provider's preflight", func() (bool, string) {
		st := d.storageCap(false).Status
		return st == apitypes.ProviderEnabled || st == apitypes.ProviderUnavailable, string(st)
	})
	t.Cleanup(func() { stgKeepLogs(t) })
	return d
}

// stgKeepLogs copies the fake's logs into the run's logs (/harness/out).
func stgKeepLogs(t *testing.T) {
	name := strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
	for src, dst := range map[string]string{stgReadsLog: name + "-reads.log",
		filepath.Join(stgStateDir, storagetest.CallsFile): name + "-calls.log"} {
		if b, err := os.ReadFile(src); err == nil {
			if err := os.MkdirAll(hostOutDir, 0o755); err == nil {
				_ = os.WriteFile(filepath.Join(hostOutDir, dst), b, 0o644)
			}
		}
	}
}

// storageCap is the storage provider's capability, refreshed when asked.
func (d *stgDaemon) storageCap(refresh bool) apitypes.ProviderCapability {
	d.t.Helper()
	var caps apitypes.Capabilities
	path := "/v1/capabilities"
	if refresh {
		path += "?refresh=true"
	}
	d.mustGet(path, &caps)
	for _, p := range caps.Providers {
		if p.Name == spec.Name {
			return p
		}
	}
	d.t.Fatalf("capabilities list no %s provider", spec.Name)
	return apitypes.ProviderCapability{}
}

// stgPath is op's REST path with name in its {name} segment, path-escaped
// (a dataset's slashes become %2F), and query appended.
func stgPath(t *testing.T, op def.Op, name, query string) string {
	t.Helper()
	_, p, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	p = strings.Replace(p, "{name}", url.PathEscape(name), 1)
	if query != "" {
		p += "?" + query
	}
	return p
}

// read is a GET of op that must answer 200; it returns the body.
func (d *stgDaemon) read(op def.Op, name, query string) []byte {
	d.t.Helper()
	p := stgPath(d.t, op, name, query)
	code, b := d.get(p)
	if code != http.StatusOK {
		d.t.Fatalf("%s (GET %s): %d %s", op.ID, p, code, b)
	}
	return b
}

func stgDecode[T any](t *testing.T, what string, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v\n%s", what, err, b)
	}
	return v
}

// stgProblem decodes a problem body.
func stgProblem(t *testing.T, what string, code int, b []byte) *problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %d, the body is not a problem: %v\n%s", what, code, err, b)
	}
	return &p
}

// write posts op on name and returns the status and body.
func (d *stgDaemon) write(op def.Op, name, body string) (int, []byte) {
	d.t.Helper()
	if body == "" {
		body = "{}"
	}
	return d.post(stgPath(d.t, op, name, ""), body)
}

// run posts op on name, requires 202 and waits for the operation's end.
func (d *stgDaemon) run(op def.Op, name, body string) apitypes.Operation {
	d.t.Helper()
	code, b := d.write(op, name, body)
	if code != http.StatusAccepted {
		d.t.Fatalf("%s %s: %d %s", op.ID, name, code, b)
	}
	acc := stgDecode[apitypes.Operation](d.t, op.ID, b)
	return d.waitOp(acc.ID, 3*time.Minute)
}

// reload installs keys as the storage config and runs nexusd.actions.reload
// to Succeeded.
func (d *stgDaemon) reload(keys string) {
	d.t.Helper()
	writeConfig(d.t, hostConfig(true, stgYAML(keys)))
	_, p, err := openapi.Route(nexusdspec.Reload.Op())
	if err != nil {
		d.t.Fatal(err)
	}
	code, b := d.post(p, "{}")
	if code != http.StatusAccepted {
		d.t.Fatalf("nexusd.actions.reload: %d %s", code, b)
	}
	op := d.waitOp(stgDecode[apitypes.Operation](d.t, "reload", b).ID, time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		d.t.Fatalf("nexusd.actions.reload: %s %+v %s", op.Status, op.Error, op.Result)
	}
}

// log is this nexusd's log: the unit's journal lines of its MainPID (the
// window of --since, whole seconds, can hold the previous test's daemon), or
// the OpenRC script's logger lines in /var/log/messages since the start.
func (d *stgDaemon) log() string {
	d.t.Helper()
	if detectedInit() == platform.Systemd {
		var out strings.Builder
		tag := fmt.Sprintf("nexusd[%d]:", d.pid)
		for line := range strings.Lines(hostJournal(d.t, d.since)) {
			if strings.Contains(line, tag) {
				out.WriteString(line)
			}
		}
		return out.String()
	}
	b, err := os.ReadFile("/var/log/messages")
	if err != nil {
		d.t.Fatal(err)
	}
	if int64(len(b)) >= d.logStart {
		b = b[d.logStart:]
	}
	var out strings.Builder
	for line := range strings.Lines(string(b)) {
		if strings.Contains(line, "nexusd") {
			out.WriteString(line)
		}
	}
	return out.String()
}

// stgAudit is the audit file's records.
func stgAudit(t *testing.T) []systemspec.AuditRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []systemspec.AuditRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r systemspec.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		recs = append(recs, r)
	}
	return recs
}

// stgCalls is calls.log.
func stgCalls(t *testing.T) []storagetest.CallLine {
	t.Helper()
	return stgLines[storagetest.CallLine](t, filepath.Join(stgStateDir, storagetest.CallsFile))
}

// stgReads is reads.log.
func stgReads(t *testing.T) []storagetest.ReadLine {
	t.Helper()
	return stgLines[storagetest.ReadLine](t, stgReadsLog)
}

func stgLines[T any](t *testing.T, path string) []T {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []T
	for line := range strings.Lines(string(b)) {
		out = append(out, stgDecode[T](t, path, []byte(line)))
	}
	return out
}

// stgCallerEnv is a write's recorded environment without what the wrapper
// and its shell add (stgShellEnv), sorted.
func stgCallerEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(stgShellEnv, k) {
			out = append(out, kv)
		}
	}
	slices.Sort(out)
	return out
}

// stgCheckWrite checks one calls.log line: argv, euid 0, the exact
// environment, and CapEff holding CAP_SYS_ADMIN when sysAdmin.
func stgCheckWrite(t *testing.T, l storagetest.CallLine, argv []string, sysAdmin bool) {
	t.Helper()
	if !slices.Equal(l.Argv, argv) {
		t.Errorf("calls.log argv %q, want %q", l.Argv, argv)
	}
	if l.Euid != 0 {
		t.Errorf("%q: euid %d, want 0", l.Argv, l.Euid)
	}
	if got, want := stgCallerEnv(l.Env), []string{stgLCAll, stgPATH}; !slices.Equal(got, want) {
		t.Errorf("%q: environment %q (recorded %q), want exactly %q", l.Argv, got, l.Env, want)
	}
	has, err := storagetest.CapSysAdmin(l.CapEff)
	if err != nil || (sysAdmin && !has) {
		t.Errorf("%q: CapEff %s (CAP_SYS_ADMIN %v, %v), want bit 21", l.Argv, l.CapEff, has, err)
	}
	t.Logf("calls.log: argv %q euid %d env %q CapEff %s (CAP_SYS_ADMIN %v) ppid %d parents %v", l.Argv, l.Euid, l.Env,
		l.CapEff, has, l.PPid, l.Parents)
}

// ---- the oracle: the capture's text files ----

// stgRows is a capture file's tab-separated rows.
func stgRows(t *testing.T, rel string) [][]string {
	t.Helper()
	out, code, err := storagetest.CaptureStdout(filepath.Join(stgCapture, rel))
	if err != nil || code != 0 {
		t.Fatalf("capture %s: exit %d %v", rel, code, err)
	}
	var rows [][]string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimRight(line, "\n"); line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows
}

func stgU(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("oracle value %q: %v", s, err)
	}
	return v
}

func stgEqU(p *uint64, v uint64) bool { return p != nil && *p == v }

// stgCheckPool compares a pool item with its zpool list -H -p row: name,
// size, allocated, free, capacity, fragmentation, dedupratio, health,
// altroot, readonly.
func stgCheckPool(t *testing.T, it spec.Pool, r []string) {
	t.Helper()
	g := it.Properties
	dedup, _ := strconv.ParseFloat(r[6], 64)
	capPct, _ := strconv.Atoi(r[4])
	frag, _ := strconv.Atoi(r[5])
	ok := it.Name == r[0] && stgEqU(g.SizeBytes, stgU(t, r[1])) && stgEqU(g.AllocatedBytes, stgU(t, r[2])) &&
		stgEqU(g.FreeBytes, stgU(t, r[3])) && g.CapacityPercent != nil && *g.CapacityPercent == capPct &&
		g.FragmentationPercent != nil && *g.FragmentationPercent == frag && g.DedupRatio != nil && *g.DedupRatio == dedup &&
		g.RawHealth == r[7] && g.Health == strings.ToLower(r[7]) && g.ReadOnly != nil && *g.ReadOnly == (r[9] == "on") &&
		(r[8] == "-") == (g.Altroot == nil)
	if !ok {
		b, _ := json.Marshal(g)
		t.Errorf("pool %s: %s\nwant the capture row %q", it.Name, b, r)
	}
}

// stgCheckDataset compares a dataset with its zfs list -H -p row (name,
// type, used, avail, refer, usedbysnapshots, …, mountpoint at 12) and its
// snapshot count.
func stgCheckDataset(t *testing.T, it spec.Dataset, r []string, snaps int) {
	t.Helper()
	g := it.Properties
	ok := it.Name == r[0] && g.Type == r[1] && stgEqU(g.UsedBytes, stgU(t, r[2])) && stgEqU(g.AvailableBytes, stgU(t, r[3])) &&
		stgEqU(g.ReferencedBytes, stgU(t, r[4])) && stgEqU(g.UsedBySnapshotsBytes, stgU(t, r[5])) &&
		g.SnapshotCount != nil && *g.SnapshotCount == snaps
	if strings.HasPrefix(r[12], "/") {
		ok = ok && g.Mountpoint != nil && *g.Mountpoint == r[12]
	}
	if !ok {
		b, _ := json.Marshal(g)
		t.Errorf("dataset %s: %s\nwant the capture row %q and %d snapshots", it.Name, b, r, snaps)
	}
}

// stgSnapCounts is the snapshot count per dataset of the capture.
func stgSnapCounts(rows [][]string) map[string]int {
	n := map[string]int{}
	for _, r := range rows {
		ds, _, _ := strings.Cut(r[0], "@")
		n[ds]++
	}
	return n
}

// stgAllSnapshots pages storage.snapshots.list to its end.
func (d *stgDaemon) allSnapshots(query string) []spec.Snapshot {
	d.t.Helper()
	var out []spec.Snapshot
	cursor := ""
	for range 100 {
		q := "limit=500"
		if query != "" {
			q += "&" + query
		}
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		l := stgDecode[apitypes.List[spec.Snapshot]](d.t, "snapshots.list", d.read(spec.SnapshotsList.Op(), "", q))
		out = append(out, l.Items...)
		if cursor = l.NextCursor; cursor == "" {
			return out
		}
	}
	d.t.Fatal("snapshots.list: more than 100 pages")
	return nil
}

// stgNineReads calls the nine storage reads and checks the ZFS ones
// against the oracle; it returns each read's body by op ID.
func (d *stgDaemon) nineReads() map[string][]byte {
	t := d.t
	t.Helper()
	bodies := map[string][]byte{}
	pools := stgRows(t, "zfs/zpool-list.txt")
	dsRows := stgRows(t, "zfs/zfs-list-datasets.txt")
	snapRows := stgRows(t, "zfs/zfs-list-snapshots.txt")
	snapCounts := stgSnapCounts(snapRows)

	bodies[spec.PoolsList.Op().ID] = d.read(spec.PoolsList.Op(), "", "")
	pl := stgDecode[apitypes.List[spec.Pool]](t, "pools.list", bodies[spec.PoolsList.Op().ID])
	if len(pl.Items) != len(pools) {
		t.Errorf("pools.list: %d pools, want %d (the capture)", len(pl.Items), len(pools))
	}
	for i, it := range pl.Items {
		if i < len(pools) {
			stgCheckPool(t, it, pools[i])
		}
	}

	bodies[spec.PoolsGet.Op().ID] = d.read(spec.PoolsGet.Op(), "z1", "")
	z1 := stgDecode[spec.Pool](t, "pools.get z1", bodies[spec.PoolsGet.Op().ID])
	stgCheckPool(t, z1, pools[0])
	if len(z1.Properties.Vdevs) == 0 || z1.Properties.Scan == nil || z1.Properties.Errors == nil {
		t.Errorf("pools.get z1: vdevs %d scan %v errors %v, want them read", len(z1.Properties.Vdevs), z1.Properties.Scan,
			z1.Properties.Errors)
	}

	bodies[spec.DatasetsList.Op().ID] = d.read(spec.DatasetsList.Op(), "", "limit=500")
	dl := stgDecode[apitypes.List[spec.Dataset]](t, "datasets.list", bodies[spec.DatasetsList.Op().ID])
	byName := map[string]spec.Dataset{}
	for _, it := range dl.Items {
		byName[it.Name] = it
	}
	if len(dl.Items) != len(dsRows) || dl.NextCursor != "" {
		t.Errorf("datasets.list: %d datasets (next %q), want %d", len(dl.Items), dl.NextCursor, len(dsRows))
	}
	for _, r := range dsRows {
		stgCheckDataset(t, byName[r[0]], r, snapCounts[r[0]])
	}

	bodies[spec.DatasetsGet.Op().ID] = d.read(spec.DatasetsGet.Op(), "z1/ds/share", "")
	got := stgDecode[spec.Dataset](t, "datasets.get", bodies[spec.DatasetsGet.Op().ID])
	gb, _ := json.Marshal(got)
	lb, _ := json.Marshal(byName["z1/ds/share"])
	if !bytes.Equal(gb, lb) {
		t.Errorf("datasets.get z1/ds/share:\n%s\nwant the list item\n%s", gb, lb)
	}

	snaps := d.allSnapshots("")
	bodies[spec.SnapshotsList.Op().ID], _ = json.Marshal(snaps)
	if len(snaps) != len(snapRows) {
		t.Errorf("snapshots.list: %d snapshots, want %d", len(snaps), len(snapRows))
	}
	bySnap := map[string]spec.Snapshot{}
	for _, s := range snaps {
		bySnap[s.Name] = s
	}
	bad := 0
	for _, r := range snapRows {
		s, ok := bySnap[r[0]]
		g := s.Properties
		if !ok || g.CreatedAt.Unix() != int64(stgU(t, r[1])) || !stgEqU(g.UsedBytes, stgU(t, r[2])) ||
			!stgEqU(g.ReferencedBytes, stgU(t, r[3])) || !stgEqU(g.Holds, stgU(t, r[4])) || g.CreatedByNexus != (r[5] != "-") {
			if bad++; bad <= 5 {
				b, _ := json.Marshal(s)
				t.Errorf("snapshot %s: %s, want the capture row %q", r[0], b, r)
			}
		}
	}

	bodies[spec.HealthGet.Op().ID] = d.read(spec.HealthGet.Op(), "", "")
	h := stgDecode[apitypes.List[spec.Health]](t, "health.get", bodies[spec.HealthGet.Op().ID])
	if len(h.Items) != 1 || !slices.Equal(h.Items[0].Properties.Checked, spec.Array[string]{"zfs", "backups", "timeMachine"}) {
		t.Errorf("health.get: %s, want one item that checked zfs, backups and timeMachine", bodies[spec.HealthGet.Op().ID])
	}
	for _, op := range []def.Op{spec.BackupsList.Op(), spec.BackupsSummary.Op(), spec.TimeMachineList.Op()} {
		bodies[op.ID] = d.read(op, "", "")
	}
	t.Logf("nine reads 200: %d pools, %d datasets, %d snapshots, health %d signals", len(pl.Items), len(dl.Items), len(snaps),
		len(h.Items[0].Properties.Signals))
	return bodies
}

// ---- the planted vzdump and Time Machine trees (test-strategy.md, T1
// planted data) ----

// stgArchives are the planted archives, from the home-server dump listing.
var stgArchives = []string{
	"vzdump-lxc-103-2026_08_27-10_34_54.tar.zst",
	"vzdump-lxc-106-2026_08_27-10_34_41.tar.zst",
	"vzdump-qemu-107-2026_08_27-10_28_13.vma.zst",
}

// stgEscapingNotes is the archive whose .notes is a symlink to /etc/shadow.
const stgEscapingNotes = "vzdump-lxc-106-2026_08_27-10_34_41.tar.zst"

func stgOKLog(vmid string) []byte {
	return []byte("INFO: starting new backup job: vzdump " + vmid + "\nINFO: Starting Backup of VM " + vmid +
		"\nINFO: Finished Backup of VM " + vmid + " (00:00:07)\nINFO: Backup job finished successfully\n")
}

// stgPlantTrees plants /srv/nx-storage: the vzdump tree (three archives,
// sparse at their captured sizes, with the captured mtimes, their logs and
// notes, the escaping notes symlink and dump/escape -> /etc) and the Time
// Machine share (a sparsebundle with an XML SnapshotHistory.plist and 16
// bands of 1 MiB, a backupbundle with a binary plist header).
func stgPlantTrees(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(stgSrv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(stgSrv); err != nil {
			t.Errorf("cleanup %s: %v", stgSrv, err)
		}
	})
	out, _, err := storagetest.CaptureStdout(filepath.Join(stgCapture, "shares/2/ls.txt"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := storagetest.ParseListing(out)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]storagetest.Listed{}
	for _, e := range listed {
		byName[e.Name] = e
	}
	notes103, _, err := storagetest.CaptureStdout(filepath.Join(stgCapture, "vzdump/newest-notes-line1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	head, _, err := storagetest.CaptureStdout(filepath.Join(stgCapture, "vzdump/newest-log-head.txt"))
	if err != nil {
		t.Fatal(err)
	}
	tail, _, err := storagetest.CaptureStdout(filepath.Join(stgCapture, "vzdump/newest-log-tail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var nodes []storagetest.Node
	for _, a := range stgArchives {
		e, ok := byName[a]
		if !ok {
			t.Fatalf("%s is not in the home-server dump listing", a)
		}
		stem := strings.TrimSuffix(strings.TrimSuffix(a, ".tar.zst"), ".vma.zst")
		vmid := strings.Split(stem, "-")[2]
		log := stgOKLog(vmid)
		if strings.Contains(a, "lxc-103") {
			log = append(append([]byte{}, head...), tail...)
		}
		nodes = append(nodes,
			storagetest.Node{Path: "dump/" + a, Size: e.Size, ModTime: e.ModTime},
			storagetest.Node{Path: "dump/" + stem + ".log", Content: log, ModTime: e.ModTime})
		switch a {
		case stgEscapingNotes:
			nodes = append(nodes, storagetest.Node{Path: "dump/" + a + ".notes", Type: storagetest.Symlink, Target: "/etc/shadow"})
		case stgArchives[0]:
			nodes = append(nodes, storagetest.Node{Path: "dump/" + a + ".notes", Content: notes103, ModTime: e.ModTime})
		default:
			nodes = append(nodes, storagetest.Node{Path: "dump/" + a + ".notes", Content: []byte("guest-" + vmid + "\n"), ModTime: e.ModTime})
		}
	}
	nodes = append(nodes, storagetest.Node{Path: "dump/escape", Type: storagetest.Symlink, Target: "/etc"})
	storagetest.Build(t, stgVzdump, nodes...)

	band := bytes.Repeat([]byte{0x5a}, 1<<20)
	tm := storagetest.BundleNodes(stgSparse)
	tm = append(tm, storagetest.Node{Path: stgSparse + "/com.apple.TimeMachine.SnapshotHistory.plist",
		Content: storagetest.SnapshotHistoryXML(stgLastBackup.Add(-24*time.Hour), stgLastBackup)})
	tm = append(tm, storagetest.BandNodes(stgSparse, 16, band)...)
	tm = append(tm, storagetest.BundleNodes(stgBackupBd)...)
	tm = append(tm, storagetest.Node{Path: stgBackupBd + "/com.apple.TimeMachine.SnapshotHistory.plist",
		Content: append([]byte(storagetest.BinaryPlistMagic), 0, 1, 2, 3, 4, 5, 6, 7)})
	tm = append(tm, storagetest.BandNodes(stgBackupBd, 2, band)...)
	storagetest.Build(t, stgTM, tm...)
	// The fake and the provider read as root without CAP_DAC_OVERRIDE
	// under the unit: everything planted is root's.
	mustSh(t, "chown", "-R", "root:root", stgSrv)
}

// stgSystemd is TC-STG-220's setup: install, the fake tools, /dev/zfs, the
// planted trees, and nexusd under the exact unit with keys after the paths.
func stgSystemd(t *testing.T, keys string) *stgDaemon {
	t.Helper()
	requireInit(t, platform.Systemd)
	stgFresh(t)
	stgFakeTools(t, true)
	stgPlantTrees(t)
	if keys != "" {
		keys = ", " + keys
	}
	return stgStart(t, stgPaths+keys)
}

// TC-STG-220: every storage read answers under the exact hardened unit and
// agrees with the fake's oracle.
func TestTCSTG220_EveryStorageReadAnswersUnderTheExact(t *testing.T) {
	d := stgSystemd(t, "")
	d.nineReads()
	reads := stgReads(t)
	if len(reads) == 0 {
		t.Fatal("reads.log is empty: no zpool or zfs read reached the fake")
	}
	for _, r := range reads {
		if len(r.Argv) == 0 || (r.Argv[0] != stgZpool && r.Argv[0] != stgZfs) {
			t.Errorf("reads.log argv %q: not /usr/sbin/zpool or /usr/sbin/zfs", r.Argv)
		}
	}
	if calls := stgCalls(t); len(calls) != 0 {
		t.Errorf("calls.log has %d write calls, want none: %+v", len(calls), calls)
	}
	t.Logf("reads.log: %d read processes, every argv[0] /usr/sbin/zpool or /usr/sbin/zfs; calls.log empty", len(reads))
}

// TC-STG-221: planted vzdump and Time Machine trees are read, and escaping
// symlinks are not followed, under the unit.
func TestTCSTG221_PlantedVzdumpAndTimeMachineTreesAre(t *testing.T) {
	d := stgSystemd(t, "")
	shadow, err := os.ReadFile("/etc/shadow")
	if err != nil {
		t.Fatal(err)
	}

	bb := d.read(spec.BackupsList.Op(), "", "limit=500")
	bl := stgDecode[apitypes.List[spec.Backup]](t, "backups.list", bb)
	got := map[string]spec.BackupProperties{}
	for _, it := range bl.Items {
		got[it.Name] = it.Properties
	}
	if len(bl.Items) != len(stgArchives) {
		t.Errorf("backups.list: %d archives %v, want %v", len(bl.Items), slices.Collect(func(yield func(string) bool) {
			for _, it := range bl.Items {
				if !yield(it.Name) {
					return
				}
			}
		}), stgArchives)
	}
	for _, a := range stgArchives {
		p, ok := got[a]
		size, err := strconv.ParseInt(strings.TrimSpace(mustSh(t, "stat", "-c", "%s", filepath.Join(stgVzdump, "dump", a))), 10, 64)
		if err != nil || !ok || p.SizeBytes == nil || *p.SizeBytes != size {
			t.Errorf("%s: listed %v size %v, want stat -c %%s %d (%v)", a, ok, p.SizeBytes, size, err)
		}
	}
	esc := got[stgEscapingNotes]
	if esc.Notes != nil || esc.Unavailable["notes"] == "" {
		t.Errorf("%s: notes %v unavailable %v, want null with a reason (its .notes points to /etc/shadow)", stgEscapingNotes,
			esc.Notes, esc.Unavailable)
	}
	t.Logf("%s: notes null, reason %q", stgEscapingNotes, esc.Unavailable["notes"])

	sb := d.read(spec.BackupsSummary.Op(), "", "")
	sum := stgDecode[apitypes.List[spec.BackupSummary]](t, "backups.summary", sb)
	if len(sum.Items) != 1 || len(sum.Items[0].Properties.Directories) != 1 {
		t.Fatalf("backups.summary: %s, want one item with one directory", sb)
	}
	dir := sum.Items[0].Properties.Directories[0]
	// The two symlinks that leave the configured root: dump/escape -> /etc
	// and the 106 archive's .notes -> /etc/shadow. Neither resolves inside
	// the root to a regular file, so both are unrecognized.
	if !dir.Available || dir.Archives != len(stgArchives) || dir.UnrecognizedFiles != 2 {
		t.Errorf("backups.summary directory %+v, want available, %d archives, 2 unrecognized (escape and the escaping notes)",
			dir, len(stgArchives))
	}

	tb := d.read(spec.TimeMachineList.Op(), "", "")
	for _, body := range [][]byte{bb, sb, tb} {
		for line := range strings.Lines(string(shadow)) {
			if line = strings.TrimSpace(line); line != "" && bytes.Contains(body, []byte(line)) {
				t.Errorf("a line of /etc/shadow is in a response: %q", line)
			}
		}
	}

	// The background size walk runs at start: wait for the sparsebundle's
	// size.
	var share spec.ShareProperties
	waitUntil(t, time.Minute, "the first Time Machine size walk", func() (bool, string) {
		l := stgDecode[apitypes.List[spec.Share]](t, "timemachine.list", d.read(spec.TimeMachineList.Op(), "", ""))
		if len(l.Items) != 1 {
			return false, fmt.Sprintf("%d shares", len(l.Items))
		}
		share = l.Items[0].Properties
		for _, b := range share.Bundles {
			if b.Name == strings.TrimSuffix(stgSparse, ".sparsebundle") && b.SizeComputedAt != nil {
				return true, ""
			}
		}
		return false, "no size yet"
	})
	df := strings.Fields(lastLine(mustSh(t, "df", "-B1", stgTM)))
	dfSize, err := strconv.ParseUint(df[1], 10, 64)
	if err != nil || share.Path != stgTM || !stgEqU(share.SizeBytes, dfSize) {
		t.Errorf("share %s sizeBytes %v, want df -B1's %d (%v): %q", share.Path, share.SizeBytes, dfSize, err, df)
	}
	for _, b := range share.Bundles {
		switch b.Name {
		case strings.TrimSuffix(stgSparse, ".sparsebundle"):
			du, err := strconv.ParseUint(strings.Fields(mustSh(t, "du", "-s", "--block-size=1", filepath.Join(stgTM, stgSparse, "bands")))[0], 10, 64)
			if b.LastBackupAt == nil || !b.LastBackupAt.Equal(stgLastBackup) || b.LastBackupSource != "plist" {
				t.Errorf("%s: lastBackupAt %v (%s), want the planted %s from the plist", b.Name, b.LastBackupAt, b.LastBackupSource,
					stgLastBackup)
			}
			if err != nil || b.SizeBytes == nil || !within(*b.SizeBytes, du, 0.01) {
				t.Errorf("%s: sizeBytes %v, want du -s --block-size=1 bands %d within 1%% (%v)", b.Name, b.SizeBytes, du, err)
			}
			t.Logf("%s: lastBackupAt %s, sizeBytes %d, du %d", b.Name, b.LastBackupAt, *b.SizeBytes, du)
		case strings.TrimSuffix(stgBackupBd, ".backupbundle"):
			bjs, _ := json.Marshal(b)
			if !strings.Contains(string(bjs), "binary plist not parsed") {
				t.Errorf("%s: %s, want a binary plist not parsed reason", b.Name, bjs)
			}
		default:
			t.Errorf("unexpected bundle %q", b.Name)
		}
	}
	if share.BundleCount != 2 {
		t.Errorf("bundleCount %d, want 2", share.BundleCount)
	}
}

// TC-STG-222: no sandbox denial in the journal during the storage reads.
func TestTCSTG222_NoSandboxDenialInTheJournalDuring(t *testing.T) {
	d := stgSystemd(t, "")
	d.nineReads()
	if faults := journalFaults(hostJournal(t, d.since)); len(faults) != 0 {
		t.Errorf("sandbox denials in nexusd.service's journal:\n%s", strings.Join(faults, "\n"))
	}
	capEff := procStatusField(d.pid, "CapEff")
	has, err := storagetest.CapSysAdmin(capEff)
	if err != nil || has {
		t.Errorf("nexusd MainPID %d CapEff %q: CAP_SYS_ADMIN %v (%v), want it absent", d.pid, capEff, has, err)
	}
	for _, r := range stgReads(t) {
		if has, err := storagetest.CapSysAdmin(r.CapEff); err != nil || has {
			t.Errorf("read %q ran with CapEff %s: CAP_SYS_ADMIN %v (%v), want it absent", r.Argv, r.CapEff, has, err)
		}
	}
	t.Logf("no denial pattern in the journal; MainPID %d CapEff %s lacks bit 21, as every read process's", d.pid, capEff)
}

// stgFakeProcs are the running fake processes: those with a thread whose
// command line holds the wrappers' -test.run argument. Every thread is
// looked at, because a SIGKILLed Go process stuck in one thread keeps its
// thread group after the leader exited, and a zombie leader has no command
// line (so pgrep -f no longer sees it). It returns each PID's task states.
func stgFakeProcs(t *testing.T) map[int]string {
	t.Helper()
	out := map[int]string{}
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range pids {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil {
			continue
		}
		tasks, _ := filepath.Glob(p + "/task/[0-9]*")
		fake := false
		var states []string
		for _, task := range tasks {
			cmd, _ := os.ReadFile(task + "/cmdline")
			if slices.Contains(strings.Split(string(cmd), "\x00"), "-test.run=^TestStorageFakeZFS$") {
				fake = true
			}
			if st, err := os.ReadFile(task + "/stat"); err == nil {
				if i := bytes.LastIndexByte(st, ')'); i > 0 && i+2 < len(st) {
					states = append(states, string(st[i+2]))
				}
			}
		}
		if fake {
			out[pid] = strings.Join(states, "")
		}
	}
	return out
}

// stgPgrep is pgrep -fc's count of the fake processes, for the log.
func stgPgrep() string {
	out, _ := sh(10*time.Second, "pgrep", "-fc", `test\.run=\^TestStorageFakeZFS`)
	return strings.TrimSpace(out)
}

// TC-STG-223: a hung zpool status is bounded under the unit and never
// delays get_host_health. The hang is real kernel state: the fake stats a
// FUSE name whose lookup is never answered, so the process is in
// uninterruptible sleep and nexusd's SIGKILL does not end it (A-31).
func TestTCSTG223_AHungZpoolStatusIsBoundedUnder(t *testing.T) {
	requireInit(t, platform.Systemd)
	stgFresh(t)
	stgFakeTools(t, true)
	stgPlantTrees(t)
	fuse := stgStuckFUSE(t, stgFUSEDir) // aborted before the cleanup above stops nexusd
	d := stgStart(t, stgPaths+", commandTimeout: 5s")
	const timeout = 5 * time.Second
	if err := os.WriteFile(filepath.Join(stgStateDir, storagetest.HangPrefix+"status"), []byte(fuse.path()), 0o644); err != nil {
		t.Fatal(err)
	}
	pools := func() (time.Duration, spec.PoolProperties) {
		start := time.Now()
		b := d.read(spec.PoolsList.Op(), "", "")
		took := time.Since(start)
		l := stgDecode[apitypes.List[spec.Pool]](t, "pools.list", b)
		if len(l.Items) != 3 {
			t.Fatalf("pools.list: %s, want the three pools", b)
		}
		return took, l.Items[0].Properties
	}

	// (1) the first call waits commandTimeout for the stuck status.
	took, z1 := pools()
	if took < timeout-500*time.Millisecond || took > timeout+3*time.Second || z1.Scan != nil ||
		!strings.Contains(z1.Unavailable["scan"], "did not finish within 5s") {
		t.Errorf("first pools.list took %s, scan %v reason %q, want about 5s, scan null, did not finish within 5s",
			took, z1.Scan, z1.Unavailable["scan"])
	}
	t.Logf("(1) pools.list took %s: scan null, %q", took.Round(time.Millisecond), z1.Unavailable["scan"])
	procs := stgFakeProcs(t)
	stuckKilled := 0
	for pid, states := range procs {
		pnd := procStatusField(pid, "ShdPnd") + "/" + procStatusField(pid, "SigPnd")
		t.Logf("fake process %d after the timeout: task states %s, pending signals (shared/own) %s", pid, states, pnd)
		if strings.Contains(states, "D") {
			stuckKilled++
		}
	}
	if stuckKilled != 1 || len(procs) != 1 {
		t.Errorf("after the timeout %d fake processes (%d in uninterruptible sleep), want the one stuck status alive (A-31)",
			len(procs), stuckKilled)
	}

	// (2) the second identical call answers at once, starting nothing.
	before := len(stgReads(t))
	took, z1 = pools()
	if took > 500*time.Millisecond || !strings.Contains(z1.Unavailable["scan"], "is still running since") {
		t.Errorf("second pools.list took %s, reason %q, want under 500ms and the pending reason", took, z1.Unavailable["scan"])
	}
	readsAfter := stgReads(t)
	for _, r := range readsAfter[before:] {
		if slices.Contains(r.Argv, "status") {
			t.Errorf("the second pools.list started another zpool status: %q", r.Argv)
		}
	}
	t.Logf("(2) pools.list took %s: %q; reads.log %d -> %d lines (no new status)", took.Round(time.Millisecond),
		z1.Unavailable["scan"], before, len(readsAfter))

	// (3) five distinct reads in parallel: at most 4 fake processes.
	// The goroutines only GET: they never call t.Fatal, directly or through
	// a helper, and hand each answer (or its error) back to this goroutine.
	type answer struct {
		what string
		code int
		body []byte
		err  error
	}
	reads := []struct {
		op         def.Op
		name, what string
	}{
		{spec.PoolsGet.Op(), "z1", "pools.get z1"}, {spec.PoolsGet.Op(), "z2", "pools.get z2"},
		{spec.PoolsGet.Op(), "z3", "pools.get z3"}, {spec.DatasetsGet.Op(), "z1/ds/share", "datasets.get z1/ds/share"},
		{spec.SnapshotsList.Op(), "", "snapshots.list"},
	}
	answers := make([]answer, len(reads))
	paths := make([]string, len(reads))
	for i, r := range reads {
		paths[i] = stgPath(t, r.op, r.name, "")
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i, r := range reads {
		wg.Go(func() {
			code, body, err := d.tryGet(paths[i])
			answers[i] = answer{what: r.what, code: code, body: body, err: err}
		})
	}
	go func() { wg.Wait(); close(done) }()
	most, samples := 0, 0
	for sampling := true; sampling; {
		select {
		case <-done:
			sampling = false
		case <-time.After(50 * time.Millisecond):
		}
		n := len(stgFakeProcs(t))
		samples++
		if n > most {
			most = n
			t.Logf("(3) %d fake processes (pgrep -fc: %s)", n, stgPgrep())
		}
	}
	if most > 4 {
		t.Errorf("(3) up to %d fake processes at once, want at most 4", most)
	}
	// Each of the five reads was answered: 200, or 503 naming the stuck
	// status (pending or timed out) while it is stuck.
	codes := map[string]int{}
	for _, a := range answers {
		codes[a.what] = a.code
		switch {
		case a.err != nil:
			t.Errorf("(3) %s: %v", a.what, a.err)
		case a.code == http.StatusOK:
		case a.code == http.StatusServiceUnavailable && (bytes.Contains(a.body, []byte("did not finish within")) ||
			bytes.Contains(a.body, []byte("is still running since"))):
		default:
			t.Errorf("(3) %s answered %d %s, want 200 or 503 with the pending or timeout reason", a.what, a.code, a.body)
		}
	}
	t.Logf("(3) five reads answered %v; at most %d fake processes over %d samples", codes, most, samples)

	// (4) host.health.get within sourceTimeout plus 1 s.
	start := time.Now()
	health := hostHealth(t, &hostDaemon{nexusdInstance: d.nexusdInstance})
	took = time.Since(start)
	var block *apitypes.ProviderSignals
	for i := range health.ProviderSignals {
		if health.ProviderSignals[i].Source == spec.Name {
			block = &health.ProviderSignals[i]
		}
	}
	if took > hostspec.DefaultSourceTimeout+time.Second || block == nil || (block.Counts != nil && len(block.Unavailable) == 0 && !block.Stale) {
		b, _ := json.Marshal(block)
		t.Errorf("(4) host.health.get took %s, storage block %s, want within 3s and a block carrying a reason", took, b)
	} else {
		b, _ := json.Marshal(block)
		t.Logf("(4) host.health.get took %s; storage block %s", took.Round(time.Millisecond), b)
	}

	// (5) host.identity.get answers.
	if id := hostIdentity(t, &hostDaemon{nexusdInstance: d.nexusdInstance}); id.Hostname == nil {
		t.Errorf("(5) host.identity.get: %+v", id)
	}

	// Past commandTimeout, storage.health.get raises zfs-unresponsive.
	h := stgDecode[apitypes.List[spec.Health]](t, "health.get", d.read(spec.HealthGet.Op(), "", ""))
	if !slices.ContainsFunc(h.Items[0].Properties.Signals, func(s apitypes.HealthSignal) bool {
		return s.ID == "zfs-unresponsive" && s.Severity == apitypes.SeverityCritical
	}) {
		t.Errorf("storage.health.get signals %+v, want a critical zfs-unresponsive", h.Items[0].Properties.Signals)
	}
	t.Logf("FUSE: %d lookups of %s read and left unanswered", fuse.stuckLookups(), fuse.path())
	if err := os.Remove(filepath.Join(stgStateDir, storagetest.HangPrefix+"status")); err != nil {
		t.Fatal(err)
	}
	fuse.abort()
	waitUntil(t, 30*time.Second, "the stuck fake processes to exit once the FUSE connection is aborted", func() (bool, string) {
		p := stgFakeProcs(t)
		return len(p) == 0, fmt.Sprint(p)
	})
}

// tryGet is get for a goroutine other than the test's: it returns its error
// instead of failing the test.
func (n *nexusdInstance) tryGet(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+n.listen+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// stgWaitScan waits for pools.get z1's scan state.
func (d *stgDaemon) scanState(pool string) string {
	d.t.Helper()
	p := stgDecode[spec.Pool](d.t, "pools.get", d.read(spec.PoolsGet.Op(), pool, ""))
	if p.Properties.Scan == nil {
		return "null: " + p.Properties.Unavailable["scan"]
	}
	return p.Properties.Scan.State
}

// stgScrubResult is a scrub operation's result.
func stgScrubResult(t *testing.T, op apitypes.Operation) spec.ScrubResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("%s: %s %+v", op.Action, op.Status, op.Error)
	}
	return stgDecode[spec.ScrubResult](t, op.Action, op.Result)
}

// stgRequested is the requested audit record of an operation.
func stgRequested(t *testing.T, recs []systemspec.AuditRecord, opID string) (systemspec.AuditRecord, bool) {
	t.Helper()
	var req systemspec.AuditRecord
	var ok bool
	for _, r := range recs {
		if r.OperationID == opID && r.Outcome == audit.OutcomeRequested {
			req, ok = r, true
		}
	}
	if !ok {
		// The requested record precedes the operation; the queued record
		// names it.
		for _, r := range recs {
			if r.OperationID == opID && r.RequestID != "" {
				for _, q := range recs {
					if q.ID == r.RequestID {
						return q, true
					}
				}
			}
		}
	}
	return req, ok
}

// stgOutcomes are the outcomes of an operation's records, in order, with
// its requested record first.
func stgOutcomes(t *testing.T, recs []systemspec.AuditRecord, opID string) []string {
	t.Helper()
	var out []string
	if req, ok := stgRequested(t, recs, opID); ok {
		out = append(out, req.Outcome)
	}
	for _, r := range recs {
		if r.OperationID == opID && r.Outcome != audit.OutcomeRequested {
			out = append(out, r.Outcome)
		}
	}
	return out
}

// TC-STG-224: scrub start and stop run the fake outside the sandbox with
// CAP_SYS_ADMIN and the exact argv.
func TestTCSTG224_ScrubStartAndStopRunTheFake(t *testing.T) {
	d := stgSystemd(t, "scrub: {enabled: true, pools: [z1]}")
	start := d.run(spec.PoolsScrub.Op(), "z1", "")
	if r := stgScrubResult(t, start); r.Scan.State != "scanning" || r.Pool != "z1" {
		t.Errorf("scrub result %+v, want z1 scanning", r)
	}
	if st := d.scanState("z1"); st != "scanning" {
		t.Errorf("pools.get z1 after the scrub: scan.state %q, want scanning", st)
	}
	stop := d.run(spec.PoolsStopScrub.Op(), "z1", "")
	if r := stgScrubResult(t, stop); r.Scan.State != "canceled" {
		t.Errorf("stop-scrub result %+v, want canceled", r)
	}
	if st := d.scanState("z1"); st != "canceled" {
		t.Errorf("pools.get z1 after the stop: scan.state %q, want canceled", st)
	}

	calls := stgCalls(t)
	if len(calls) != 2 {
		t.Fatalf("calls.log has %d lines, want exactly 2: %+v", len(calls), calls)
	}
	stgCheckWrite(t, calls[0], []string{stgZpool, "scrub", "z1"}, true)
	stgCheckWrite(t, calls[1], []string{stgZpool, "scrub", "-s", "z1"}, true)

	recs := stgAudit(t)
	for _, op := range []apitypes.Operation{start, stop} {
		outcomes := stgOutcomes(t, recs, op.ID)
		req, _ := stgRequested(t, recs, op.ID)
		if len(outcomes) < 2 || outcomes[0] != audit.OutcomeRequested || outcomes[len(outcomes)-1] != string(apitypes.StatusSucceeded) ||
			req.Facts["pool"] != "z1" || req.Facts["force"] != "false" {
			t.Errorf("%s %s: audit outcomes %v facts %v, want requested … Succeeded with pool z1, force false", op.Action, op.ID,
				outcomes, req.Facts)
		}
		t.Logf("audit %s %s: %v facts %v", op.Action, op.ID, outcomes, req.Facts)
	}
}

// TC-STG-225: a snapshot is created outside the sandbox with the nexus
// property and returned.
func TestTCSTG225_ASnapshotIsCreatedOutsideTheSandbox(t *testing.T) {
	d := stgSystemd(t, "snapshots: {enabled: true, datasets: [{name: z1/ds/share}]}")
	op := d.run(spec.DatasetsSnapshot.Op(), "z1/ds/share", `{"label":"t1"}`)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("snapshot: %s %+v", op.Status, op.Error)
	}
	res := stgDecode[spec.SnapshotResult](t, "snapshot result", op.Result)
	calls := stgCalls(t)
	if len(calls) != 1 {
		t.Fatalf("calls.log has %d lines, want 1: %+v", len(calls), calls)
	}
	snap := calls[0].Argv[len(calls[0].Argv)-1]
	if !regexp.MustCompile(`^z1/ds/share@nexus-[0-9]{8}T[0-9]{6}Z-t1$`).MatchString(snap) {
		t.Errorf("snapshot name %q, want z1/ds/share@nexus-<UTC>-t1", snap)
	}
	stgCheckWrite(t, calls[0], []string{stgZfs, "snapshot", "-o", spec.CreatedByProperty + "=" + op.ID, snap}, true)

	l := d.allSnapshots("dataset=" + url.QueryEscape("z1/ds/share") + "&createdByNexus=true")
	if len(l) != 1 || l[0].Name != snap || res.Snapshot.Name != snap {
		t.Fatalf("snapshots.list createdByNexus: %+v, result %+v, want exactly %s", l, res.Snapshot, snap)
	}
	rb, _ := json.Marshal(res.Snapshot)
	lb, _ := json.Marshal(l[0])
	if !bytes.Equal(rb, lb) {
		t.Errorf("the result's item\n%s\ndiffers from the listed\n%s", rb, lb)
	}
	req, _ := stgRequested(t, stgAudit(t), op.ID)
	if req.Facts["dataset"] != "z1/ds/share" || "z1/ds/share@"+req.Facts["snapshot"] != snap || req.Facts["recursive"] != "false" {
		t.Errorf("audit facts %v, want dataset z1/ds/share, snapshot %s, recursive false", req.Facts, snap)
	}
	t.Logf("created %s by %s: %s; audit facts %v", snap, op.ID, rb, req.Facts)
}

// TC-STG-226: every write refusal is audited and leaves calls.log unchanged.
func TestTCSTG226_EveryWriteRefusalIsAuditedAndLeaves(t *testing.T) {
	d := stgSystemd(t, "scrub: {enabled: true, pools: [z1]}, snapshots: {enabled: true, datasets: [{name: z1/ds/share}]}")
	refused := func() int {
		n := 0
		for _, r := range stgAudit(t) {
			if r.Outcome == audit.OutcomeRefused && r.Provider == spec.Name {
				n++
			}
		}
		return n
	}
	want := func(what string, code int, b []byte, slug problem.Slug, n int) *problem.Error {
		t.Helper()
		p := stgProblem(t, what, code, b)
		if code != http.StatusConflict || p.Slug() != slug {
			t.Errorf("%s: %d %s, want 409 %s", what, code, b, slug)
		}
		if got := refused(); got != n {
			t.Errorf("%s: %d refused audit records, want %d", what, got, n)
		}
		t.Logf("%s: %d %s %q %v", what, code, p.Slug(), p.Detail, p.Extensions)
		return p
	}
	code, b := d.write(spec.PoolsScrub.Op(), "z2", "")
	want("scrub z2 (not allow-listed)", code, b, problem.NotAllowed, 1)

	first := d.run(spec.PoolsScrub.Op(), "z1", "")
	stgScrubResult(t, first)
	code, b = d.write(spec.PoolsScrub.Op(), "z1", "")
	want("scrub z1 while scanning", code, b, problem.ScanInProgress, 2)

	code, b = d.write(spec.DatasetsSnapshot.Op(), "z1/ds/appdata", `{"label":"t1"}`)
	want("snapshot z1/ds/appdata (not allow-listed)", code, b, problem.NotAllowed, 3)

	d.reload(stgPaths + ", scrub: {enabled: false, pools: [z1]}, snapshots: {enabled: true, datasets: []}")
	code, b = d.write(spec.PoolsScrub.Op(), "z1", "")
	if p := want("scrub z1 with scrub.enabled false", code, b, problem.NotAllowed, 4); p.Extensions["configKey"] != "providers.storage.scrub.enabled" {
		t.Errorf("configKey %v, want providers.storage.scrub.enabled", p.Extensions["configKey"])
	}

	calls := stgCalls(t)
	if len(calls) != 1 {
		t.Fatalf("calls.log has %d lines, want only the first z1 scrub: %+v", len(calls), calls)
	}
	stgCheckWrite(t, calls[0], []string{stgZpool, "scrub", "z1"}, true)
}

// stgTwelve calls each storage op once (pools z1, dataset z1/ds/share) and
// requires 503 capability-unavailable from every one.
func (d *stgDaemon) twelve(ops []def.Op) {
	t := d.t
	t.Helper()
	for _, op := range ops {
		name := ""
		switch op.ResourceType {
		case spec.PoolsType:
			name = "z1"
		case spec.DatasetsType:
			name = "z1/ds/share"
		}
		if op.Kind == def.KindList {
			name = ""
		}
		var code int
		var b []byte
		if op.Risk == def.RiskReadOnly {
			code, b = d.get(stgPath(t, op, name, ""))
		} else {
			body := "{}"
			if op.ID == spec.DatasetsSnapshot.Op().ID {
				body = `{"label":"t1"}`
			}
			code, b = d.write(op, name, body)
		}
		p := stgProblem(t, op.ID, code, b)
		if code != http.StatusServiceUnavailable || p.Slug() != problem.CapabilityUnavailable {
			t.Errorf("%s: %d %s, want 503 capability-unavailable", op.ID, code, b)
		}
	}
}

// stgShape is a host health's shape apart from providerSignals: its JSON
// keys and the keys of its unavailable map.
func stgShape(t *testing.T, h hostspec.HealthProperties) string {
	t.Helper()
	b, _ := json.Marshal(h)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "providerSignals")
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
	un := slices.Sorted(func(yield func(string) bool) {
		for k := range h.Unavailable {
			if !yield(k) {
				return
			}
		}
	})
	return fmt.Sprintf("keys %v unavailable %v failedServices.source %s", keys, un, h.FailedServices.Source)
}

// TC-STG-227: with no ZFS the provider is unavailable with every reason and
// affects nothing else (debian:12 as pihole, alpine:3.22 as Public-Nginx;
// it runs on every harness image).
func TestTCSTG227_OnDebian12AndAlpine322(t *testing.T) {
	stgFresh(t)
	for _, p := range []string{stgZpool, stgZfs, "/sbin/zpool", "/sbin/zfs"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists: this case needs a host without the ZFS tools", p)
		}
	}
	stgDevZFSState(t, false)
	d := stgStart(t, "")

	// Step 1. Capabilities without refresh=true: a refresh runs another
	// preflight, and its line would be counted below.
	c := d.storageCap(false)
	if c.Status != apitypes.ProviderUnavailable || c.Reason != stgNoZFSReason {
		t.Errorf("capabilities: storage %s %q, want unavailable %q", c.Status, c.Reason, stgNoZFSReason)
	}
	// The storage provider's own lines: its preflight line, and anything
	// else naming storage except the core's "request refused" line, which
	// the Dispatcher writes for every audited refusal (a write answered
	// 503), whatever the provider.
	storageLines := func() (preflight, other int) {
		for line := range strings.Lines(d.log()) {
			switch {
			case strings.Contains(line, "storage preflight"):
				preflight++
			case strings.Contains(line, "storage") && !strings.Contains(line, "request refused"):
				other++
			}
		}
		return preflight, other
	}
	pre0, other0 := storageLines()
	ops := spec.Spec().Ops
	if len(ops) != 12 {
		t.Fatalf("%d storage ops, want 12", len(ops))
	}
	d.twelve(ops) // 12 requests
	health := hostHealth(t, &hostDaemon{nexusdInstance: d.nexusdInstance})
	var block *apitypes.ProviderSignals
	for i := range health.ProviderSignals {
		if health.ProviderSignals[i].Source == spec.Name {
			block = &health.ProviderSignals[i]
		}
	}
	bj, _ := json.Marshal(block)
	if block == nil || block.Counts != nil || !strings.Contains(string(bj), "/usr/sbin/zpool not found") {
		t.Errorf("host.health.get storage block %s, want counts null and the not-found reason", bj)
	}
	t.Logf("host.health.get storage block: %s", bj)
	if id := hostIdentity(t, &hostDaemon{nexusdInstance: d.nexusdInstance}); id.Hostname == nil {
		t.Errorf("host.identity.get: %+v", id)
	}
	for range 5 { // 20 requests in all with capabilities, health and identity
		if code, b := d.get(stgPath(t, spec.PoolsList.Op(), "", "")); code != http.StatusServiceUnavailable {
			t.Errorf("pools.list: %d %s, want 503", code, b)
		}
	}
	window := time.Since(d.since)
	pre1, other1 := storageLines()
	// The registry re-checks an unavailable provider on a call once its
	// last check is 5 s old (core.RecheckInterval, TC-STG-118), and a
	// re-check is a preflight with its line. Within 5 s of the start there
	// can be none, so exactly the startup line; a slower run may hold one
	// per 5 s, never one per request.
	maxPre := 1
	if window >= 5*time.Second {
		maxPre = 1 + int(window/(5*time.Second))
	}
	if pre0 != 1 || pre1 < 1 || pre1 > maxPre || other1 != other0 {
		t.Errorf("over 20 requests in %s: %d storage preflight lines (%d before), %d other storage lines (%d before), want 1 (at most %d) and none per request",
			window.Round(time.Millisecond), pre1, pre0, other1, other0, maxPre)
	}
	d.reload("")
	pre2, _ := storageLines()
	if pre2 != pre1+1 || (maxPre == 1 && pre2 != 2) {
		t.Errorf("after one reload: %d storage preflight lines, want exactly %d (startup and reload)", pre2, pre1+1)
	}
	t.Logf("log: storage preflight lines %d -> %d -> %d over 20 requests (%s after the start) and one reload; other storage lines %d -> %d",
		pre0, pre1, pre2, window.Round(time.Millisecond), other0, other1)

	// Step 2: the writes allow-listed, still 503 and never refused.
	// capRefused counts the capability-unavailable audit records per action;
	// any other refused record for storage is a failure.
	capRefused := func() map[string]int {
		n := map[string]int{}
		for _, r := range stgAudit(t) {
			if r.Provider != spec.Name || r.Outcome != audit.OutcomeRefused {
				continue
			}
			if r.Problem != string(problem.CapabilityUnavailable) {
				t.Errorf("a refused audit record for %s with %s: admission ran for an unavailable provider", r.Action, r.Problem)
			}
			n[r.Action]++
		}
		return n
	}
	before := capRefused()
	d.reload("scrub: {enabled: true, pools: [z1]}, snapshots: {enabled: true, datasets: [{name: z1/ds/share}]}")
	writes := []def.Op{spec.PoolsScrub.Op(), spec.PoolsStopScrub.Op(), spec.DatasetsSnapshot.Op()}
	d.twelve(writes)
	// The Dispatcher refuses an unavailable provider before admission (DF-2
	// step 1): no admission refusal (not-allowed) is ever recorded. The core
	// does audit the capability refusal of every write itself
	// (internal/core/dispatcher_audit_test.go pins it), so a refused record
	// may only carry capability-unavailable, and each write refused here
	// must have produced one.
	after := capRefused()
	total := 0
	for _, op := range writes {
		got := after[op.ID] - before[op.ID]
		if got < 1 {
			t.Errorf("%s answered 503 with %d new capability-unavailable audit records, want at least 1", op.ID, got)
		}
		total += got
	}
	t.Logf("audit: %d capability-unavailable records for the %d writes of step 2 (%d refused storage records in all, none not-allowed)", total, len(writes), sumCounts(after))

	// The host's other health fields, against storage disabled.
	d.reload("disabled")
	off := hostHealth(t, &hostDaemon{nexusdInstance: d.nexusdInstance})
	if slices.ContainsFunc(off.ProviderSignals, func(b apitypes.ProviderSignals) bool { return b.Source == spec.Name }) {
		t.Errorf("storage disabled: providerSignals still has a storage block")
	}
	if a, b := stgShape(t, health), stgShape(t, off); a != b {
		t.Errorf("host health with storage unavailable: %s\nwith storage disabled: %s", a, b)
	}
}

// TC-STG-228: with the fake tools and no /dev/zfs, zfs is unavailable with
// the module reason.
func TestTCSTG228_OnDebian12WithTheFakeTools(t *testing.T) {
	requireInit(t, platform.Systemd)
	stgFresh(t)
	stgFakeTools(t, false)
	d := stgStart(t, "")
	c := d.storageCap(true)
	feats, _ := c.Details["features"].(map[string]any)
	zfsFeat, _ := feats["zfs"].(map[string]any)
	if reason, _ := zfsFeat["reason"].(string); reason != stgNoDevReason {
		t.Errorf("features.zfs %v, want reason %q", zfsFeat, stgNoDevReason)
	}
	if r := stgReads(t); len(r) != 0 {
		t.Errorf("reads.log has %d lines, want none: %+v", len(r), r)
	}
	t.Logf("capabilities storage %s %q; features.zfs %v; reads.log empty", c.Status, c.Reason, zfsFeat)
}

// TC-STG-229: on Alpine/OpenRC the scrub runs under op-supervise outside the
// service's limits.
func TestTCSTG229_OnAlpineOpenRCTheScrubRunsUnder(t *testing.T) {
	requireInit(t, platform.OpenRC)
	stgFresh(t)
	stgFakeTools(t, true)
	d := stgStart(t, "scrub: {enabled: true, pools: [z1]}")
	if r := stgScrubResult(t, d.run(spec.PoolsScrub.Op(), "z1", "")); r.Scan.State != "scanning" {
		t.Errorf("scrub result %+v, want scanning", r)
	}
	if r := stgScrubResult(t, d.run(spec.PoolsStopScrub.Op(), "z1", "")); r.Scan.State != "canceled" {
		t.Errorf("stop-scrub result %+v, want canceled", r)
	}
	calls := stgCalls(t)
	if len(calls) != 2 {
		t.Fatalf("calls.log has %d lines, want 2: %+v", len(calls), calls)
	}
	for i, argv := range [][]string{{stgZpool, "scrub", "z1"}, {stgZpool, "scrub", "-s", "z1"}} {
		l := calls[i]
		stgCheckWrite(t, l, argv, false)
		if len(l.Parents) == 0 || l.Parents[0].PID != l.PPid {
			t.Errorf("%q: parents %+v do not start at PPid %d", l.Argv, l.Parents, l.PPid)
		}
		if !slices.ContainsFunc(l.Parents, func(p storagetest.Parent) bool { return slices.Contains(p.Cmdline, "op-supervise") }) {
			t.Errorf("%q: no op-supervise in the parent chain %+v", l.Argv, l.Parents)
		}
	}
}

// TC-STG-230: capture.sh in the harness containers yields the degradation
// fixtures' file set.
func TestTCSTG230_CaptureShInTheHarnessContainersYields(t *testing.T) {
	const out, capMap = "/tmp/cap", "/tmp/capmap.txt"
	script := filepath.Join(filepath.Dir(stgCaptures), "capture.sh")
	committed := ""
	switch o := platform.Current().OS; {
	case o.ID == "debian":
		committed = filepath.Join(stgCaptures, "pihole")
	case o.ID == "alpine":
		committed = filepath.Join(stgCaptures, "public-nginx")
	}
	for _, p := range []string{out, capMap} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(out); _ = os.Remove(capMap) })
	roots := []string{"/etc", "/root", "/home", "/var", "/tmp", "/srv", "/opt", "/media", "/mnt", "/usr", "/harness-tmp"}
	// What the container's own daemons write meanwhile is not capture.sh's:
	// the journal or syslog, and a running nexusd's state and logs.
	skip := []string{out, "/var/log/journal", "/var/log/messages", "/var/lib/nexus", "/var/log/nexus", "/var/lib/systemd",
		"/var/cache/ldconfig"}
	before := stgTree(roots, skip)
	marker := time.Now()
	time.Sleep(1100 * time.Millisecond) // BusyBox find and stat work in whole seconds
	res, err := sh(5*time.Minute, "sh", script, "--out", out, "--map", capMap)
	t.Logf("capture.sh: %v\n%s", err, res)
	if err != nil {
		t.Fatalf("capture.sh exited non-zero: %v", err)
	}
	files := map[string]bool{}
	err = filepath.WalkDir(out, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		files[rel] = true
		if rel == "PROVENANCE" || strings.HasSuffix(rel, ".stderr") {
			return nil
		}
		if _, _, err := storagetest.CaptureStdout(p); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("walk %s: %v (%d files)", out, err, len(files))
	}
	if mode := strings.TrimSpace(mustSh(t, "stat", "-c", "%a", capMap)); mode != "600" {
		t.Errorf("%s mode %s, want 600", capMap, mode)
	}
	after := stgTree(roots, skip)
	var stray []string
	for p, mt := range after {
		if p == capMap {
			continue
		}
		if old, ok := before[p]; !ok || (mt.After(marker) && !old.Equal(mt) && !stgIsDir(p)) {
			stray = append(stray, p)
		}
	}
	slices.Sort(stray)
	if len(stray) != 0 {
		t.Errorf("capture.sh wrote outside %s and %s: %q", out, capMap, stray)
	}
	t.Logf("capture.sh wrote %d files under %s, every one with an Exit line; %s mode 600; nothing else written", len(files),
		out, capMap)

	if committed == "" {
		t.Skipf("no committed degradation capture for %s: the file-set comparison runs on debian (pihole) and alpine (public-nginx)",
			platform.Current().OS.ID)
	}
	if _, err := os.Stat(committed); err != nil {
		t.Skipf("file-set comparison owed: the committed capture %s is absent (operator capture pending, WI 1.2): %v", committed, err)
	}
	want := map[string]bool{}
	_ = filepath.WalkDir(committed, func(p string, de fs.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			rel, _ := filepath.Rel(committed, p)
			want[rel] = true
		}
		return err
	})
	if a, b := slices.Sorted(stgKeys(files)), slices.Sorted(stgKeys(want)); !slices.Equal(a, b) {
		t.Errorf("capture.sh's file set\n%q\ndiffers from the committed %s\n%q", a, committed, b)
	}
}

func stgKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func stgIsDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// stgTree is every path under roots with its mtime, without the skipped
// trees.
func stgTree(roots, skip []string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, r := range roots {
		_ = filepath.WalkDir(r, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if slices.ContainsFunc(skip, func(s string) bool { return p == s || strings.HasPrefix(p, s+"/") }) {
				if de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if fi, err := de.Info(); err == nil {
				out[p] = fi.ModTime()
			}
			return nil
		})
	}
	return out
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
