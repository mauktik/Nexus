package nexusd

// Tests of the service helper's config-set mode (WI 4.2), on F-SYS with
// F-RELOAD (fakesystem_test.go) and the real daemon validator through
// CheckConfigDataForTest.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/reloadstatus"
)

// errKilled is the fault hook's kill, as KillProcess would stop the helper.
var errKilled = errors.New("killed by the test's fault hook")

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// realCheck is the production validator's shape over the real
// daemon.CheckConfigData.
func realCheck(data []byte, file string) config.ErrorList {
	_, errs := CheckConfigDataForTest(data, file)
	return errs
}

// csRig is one config-set helper run over temp files.
type csRig struct {
	f          *fakeSystem
	rl         *fakeReload
	dir        string // The operation's directory
	cfgDir     string
	cfgPath    string
	stateDir   string
	orig       []byte
	in         helperInput
	check      ConfigCheckFunc
	beforeStep func(name string) error
	stdout     bytes.Buffer
	stderr     bytes.Buffer
	t0         time.Time
	code       int
}

// newCSRig writes the default file with nexusd enabled at mode perm, boots
// the fake unit, and makes the fake daemon serve the file, with one reload
// already handled so a baseline status exists. The input is what
// prepareConfigSet writes.
func newCSRig(t *testing.T, perm fs.FileMode, changes ...string) *csRig {
	t.Helper()
	if len(changes) == 0 {
		changes = []string{"providers.host.enabled=true"}
	}
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "nexusd.yaml")
	orig := defaultConfigWithNexusd(t)
	if err := os.WriteFile(cfgPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgPath, perm); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	f := newFakeSystem(t, 200)
	f.bootActive(t, "v9.9.1", "ok")
	rl := f.enableReload(t, cfgPath, stateDir)
	f.sighup()
	f.runUntil(f.now.Add(time.Second))
	if rl.handled != 1 {
		t.Fatalf("baseline reload not handled: %d", rl.handled)
	}
	return &csRig{
		f: f, rl: rl, dir: t.TempDir(), cfgDir: cfgDir, cfgPath: cfgPath, stateDir: stateDir, orig: orig,
		in: helperInput{Mode: modeConfigSet, ConfigPath: cfgPath, BackupPath: cfgPath + ".bak", Changes: changes,
			BaseSHA256: shaHex(orig), StateDir: stateDir, ReloadTimeout: reloadTimeout, Paths: f.paths},
		check: realCheck,
	}
}

func (r *csRig) env() helperEnv {
	return helperEnv{newSystem: func(helperInput) system { return r.f }, mode: modeOf, check: r.check,
		beforeStep: r.beforeStep}
}

// writeIn writes input.json.
func (r *csRig) writeIn(t *testing.T) {
	t.Helper()
	b, err := json.Marshal(r.in)
	if err != nil {
		t.Fatal(err)
	}
	writeInput(t, r.dir, string(b))
}

// exec runs the helper over an input.json already written.
func (r *csRig) exec(t *testing.T) (helperResult, bool) {
	t.Helper()
	r.t0 = r.f.Now()
	r.code = runHelper([]string{"--dir", r.dir}, &r.stdout, &r.stderr, r.env())
	res, ok, err := readResult(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	return res, ok
}

// run writes input.json and runs the helper; it wants a result.
func (r *csRig) run(t *testing.T) helperResult {
	t.Helper()
	r.writeIn(t)
	res, ok := r.exec(t)
	if !ok || r.code != 0 {
		t.Fatalf("helper exit %d, result %v\nstdout:\n%s\nstderr:\n%s", r.code, ok, r.stdout.String(),
			r.stderr.String())
	}
	return res
}

// elapsed is the virtual time the run took.
func (r *csRig) elapsed() time.Duration { return r.f.Now().Sub(r.t0) }

// want is config.Edit's output for the rig's changes.
func (r *csRig) want(t *testing.T) []byte {
	t.Helper()
	var as []config.Assignment
	for _, c := range r.in.Changes {
		a, err := config.ParseAssignment(c)
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	out, _, err := config.Edit(r.orig, r.cfgPath, as)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *csRig) disk(t *testing.T) []byte { return readBytes(t, r.cfgPath) }

// wantPC is FR-8's post-condition: the disk holds the bytes in force.
func (r *csRig) wantPC(t *testing.T) {
	t.Helper()
	if got := shaHex(r.disk(t)); got != r.rl.appliedSHA() {
		t.Errorf("PC: sha(disk) %s != F-RELOAD applied %s", got, r.rl.appliedSHA())
	}
}

func (r *csRig) wantNoBackup(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(r.in.BackupPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (%v), want none", r.in.BackupPath, err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func modeOfFile(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// otherValid is the default file with another valid change, as a manual
// edit by someone else.
func (r *csRig) otherValid(t *testing.T) []byte {
	t.Helper()
	out, _, err := config.Edit(r.orig, r.cfgPath, []config.Assignment{{Key: "host.name", Value: "edited-by-hand"}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wantSlug(t *testing.T, res helperResult, slug problem.Slug) {
	t.Helper()
	if res.Error == nil || res.Error.Slug != slug {
		t.Fatalf("result %+v (error %+v), want %s", res, res.Error, slug)
	}
}

// wantRestored is config-apply-failed with restored and the backup's path.
func (r *csRig) wantRestored(t *testing.T, res helperResult, stage string) {
	t.Helper()
	wantSlug(t, res, problem.ConfigApplyFailed)
	if !res.Restored || res.BackupPath != r.in.BackupPath || res.Changed {
		t.Errorf("result restored %v, backupPath %q, changed %v; want true, %q, false", res.Restored, res.BackupPath,
			res.Changed, r.in.BackupPath)
	}
	// The stage the daemon refused the new file at is in the detail, the only
	// place a client sees it; an unconfirmed reload has none.
	if has := strings.Contains(res.Error.Detail, "refused the new configuration at stage "+stage); has != (stage != "") {
		t.Errorf("detail %q: names stage %q = %v, want %v", res.Error.Detail, stage, has, stage != "")
	}
	if !bytes.Equal(r.disk(t), r.orig) {
		t.Error("the disk does not hold the original bytes after the restore")
	}
	r.wantPC(t)
}

// TC-RA-70: A successful config set writes the edit, a 0600 backup and the result, and preserves mode and owner. Owner WI 4.2. Requirements: FR-7, ASM-7, NFR-5.
func TestTCRA70_ASuccessfulConfigSetWritesTheEditA0600BackupAnd(t *testing.T) {
	for _, perm := range []fs.FileMode{0o600, 0o400, 0o640} {
		t.Run(perm.String(), func(t *testing.T) {
			r := newCSRig(t, perm)
			res := r.run(t)
			if res.Error != nil {
				t.Fatalf("error %+v\n%s", res.Error, r.stderr.String())
			}
			if !bytes.Equal(r.disk(t), r.want(t)) {
				t.Errorf("disk != config.Edit(orig):\n%s", r.disk(t))
			}
			if got, want := modeOfFile(t, r.cfgPath), perm&0o600; got != want {
				t.Errorf("file mode %04o, want %04o", got, want)
			}
			if !bytes.Equal(readBytes(t, r.in.BackupPath), r.orig) {
				t.Error(".bak != the original bytes")
			}
			if got := modeOfFile(t, r.in.BackupPath); got != 0o600 {
				t.Errorf(".bak mode %04o, want 0600", got)
			}
			for _, p := range []string{r.cfgPath, r.in.BackupPath} {
				fi, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if uid := fi.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Geteuid() {
					t.Errorf("%s owner %d, want %d", p, uid, os.Geteuid())
				}
			}
			want := []spec.ConfigChange{{Key: "providers.host.enabled", Old: false, New: true}}
			if !res.Changed || !reflect.DeepEqual(res.ConfigChanges, want) || len(res.RestartRequired) != 0 ||
				res.BackupPath != r.cfgPath+".bak" {
				t.Errorf("result %+v, want changed, %+v, no restartRequired, backupPath %s.bak", res, want, r.cfgPath)
			}
			if r.rl.reloadCalls != 1 {
				t.Errorf("reloadCalls %d, want 1", r.rl.reloadCalls)
			}
			r.wantPC(t)
		})
	}
}

// TC-RA-72: An inactive unit is retried until the reload is confirmed. Owner WI 4.2. Requirements: FR-8.
func TestTCRA72_AnInactiveUnitIsRetriedUntilTheReloadIsConfirmed(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.rl.inactiveFor = 3
	res := r.run(t)
	if res.Error != nil || !res.Changed {
		t.Fatalf("result %+v (error %+v), want success", res, res.Error)
	}
	r.wantPC(t)
	if r.rl.reloadCalls != 4 {
		t.Fatalf("reloadCalls %d, want 4", r.rl.reloadCalls)
	}
	for i := 1; i < len(r.rl.callTimes); i++ {
		if d := r.rl.callTimes[i].Sub(r.rl.callTimes[i-1]); d != reloadRetryInterval {
			t.Errorf("reload call %d came %s after the previous, want %s", i+1, d, reloadRetryInterval)
		}
	}
	if r.elapsed() >= reloadTimeout {
		t.Errorf("virtual time %s, want under %s", r.elapsed(), reloadTimeout)
	}
}

// TC-RA-73: Kill at every helper step leaves the file whole, either old or new. Owner WI 4.2. Requirements: NFR-4, FR-8.
// Its Recovery bullet (outcome-unknown with LostDetail in the stack) is TC-RA-80, WI 4.3's.
func TestTCRA73_KillAtEveryHelperStepLeavesTheFileWholeEitherOld(t *testing.T) {
	for _, c := range []struct {
		step string
		new  bool
	}{
		{stepFaultBackup, false}, {stepFaultAfterBackup, false}, {stepFaultAfterTmp, false},
		{stepFaultAfterRename, true}, {stepFaultAfterReload, true},
	} {
		t.Run(c.step, func(t *testing.T) {
			r := newCSRig(t, 0o600)
			fired := false
			r.beforeStep = func(name string) error {
				if name == c.step {
					fired = true
					return errKilled
				}
				return nil
			}
			r.writeIn(t)
			_, ok := r.exec(t)
			if !fired {
				t.Fatalf("the hook never reached %s", c.step)
			}
			if ok || r.code != 1 {
				t.Errorf("exit %d, result.json %v; want 1 and none", r.code, ok)
			}
			want := r.orig
			if c.new {
				want = r.want(t)
			}
			disk := r.disk(t)
			if len(disk) == 0 || !bytes.Equal(disk, want) {
				t.Errorf("disk sha %s, want %s (new %v)", shaHex(disk), shaHex(want), c.new)
			}
			if !strings.Contains(r.stdout.String(), faultHookMarker) {
				t.Errorf("stdout lacks the armed hook's marker %q", faultHookMarker)
			}

			// The next helper run removes any leftover temporary file.
			next := &csRig{f: r.f, rl: r.rl, dir: t.TempDir(), cfgDir: r.cfgDir, cfgPath: r.cfgPath,
				stateDir: r.stateDir, orig: disk, in: r.in, check: realCheck}
			next.in.BaseSHA256 = shaHex(disk)
			next.run(t)
			for _, n := range dirNames(t, r.cfgDir) {
				if strings.HasSuffix(n, ".tmp") {
					t.Errorf("leftover %s after the next run", n)
				}
			}
		})
	}
}

// snapshot is a recursive listing: path -> size and mtime.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		m[rel] = fmt.Sprintf("%d/%s", fi.Size(), fi.ModTime())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// changedEntries are the names new or changed in after, and removed ones.
func changedEntries(before, after map[string]string) (changed, removed []string) {
	for k, v := range after {
		if before[k] != v {
			changed = append(changed, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	slices.Sort(changed)
	slices.Sort(removed)
	return changed, removed
}

// TC-RA-74: The helper writes only the config file, its backup and their temporary files. Owner WI 4.2. Requirements: NFR-5, CON-1.
func TestTCRA74_TheHelperWritesOnlyTheConfigFileItsBackupAnd(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "succeeds", true: "refused and restored"}[refused], func(t *testing.T) {
			r := newCSRig(t, 0o600)
			if refused {
				r.rl.refuseApply = 1
			}
			// While the tmp exists, the config dir holds only the file, the
			// backup and the file's temporary file.
			r.beforeStep = func(name string) error {
				if name != stepFaultAfterTmp {
					return nil
				}
				for _, n := range dirNames(t, r.cfgDir) {
					ok, _ := filepath.Match(".nexusd.yaml.*.tmp", n)
					if !ok && n != "nexusd.yaml" && n != "nexusd.yaml.bak" {
						t.Errorf("at %s the config dir holds %s", name, n)
					}
				}
				return nil
			}
			r.writeIn(t)
			cfg0, state0, op0 := snapshot(t, r.cfgDir), snapshot(t, r.stateDir), snapshot(t, r.dir)
			time.Sleep(10 * time.Millisecond) // So a rewrite shows in the mtime
			res, ok := r.exec(t)
			if !ok || r.code != 0 {
				t.Fatalf("exit %d, result %v\n%s", r.code, ok, r.stderr.String())
			}
			if refused {
				r.wantRestored(t, res, "apply")
			} else if res.Error != nil {
				t.Fatalf("error %+v", res.Error)
			}
			if changed, removed := changedEntries(cfg0, snapshot(t, r.cfgDir)); !slices.Equal(changed,
				[]string{"nexusd.yaml", "nexusd.yaml.bak"}) || len(removed) != 0 {
				t.Errorf("config dir: changed %v, removed %v; want [nexusd.yaml nexusd.yaml.bak], none", changed,
					removed)
			}
			if changed, removed := changedEntries(op0, snapshot(t, r.dir)); !slices.Equal(changed,
				[]string{resultFileName}) || len(removed) != 0 {
				t.Errorf("op dir: changed %v, removed %v; want [result.json], none", changed, removed)
			}
			if changed, removed := changedEntries(state0, snapshot(t, r.stateDir)); !slices.Equal(changed,
				[]string{reloadstatus.FileName}) || len(removed) != 0 {
				t.Errorf("state dir: changed %v, removed %v; want [reload-status.json], none", changed, removed)
			}
		})
	}
}

// TC-RA-75: The helper's step lines name keys and the backup path, never values. Owner WI 4.2. Requirements: NFR-1, FR-13.
// The step lines go to stdout and diagnostics to stderr; both are the op log,
// so the sentinel is counted in both.
func TestTCRA75_TheHelperSStepLinesNameKeysAndTheBackupPathNever(t *testing.T) {
	const sentinel = "RA-VALUE-SENTINEL"
	r := newCSRig(t, 0o600, "host.name="+sentinel)
	res := r.run(t)
	if res.Error != nil {
		t.Fatalf("error %+v", res.Error)
	}
	steps := r.stdout.String()
	if !strings.Contains(steps, "host.name") || !strings.Contains(steps, r.in.BackupPath) {
		t.Errorf("step lines do not name host.name and %s:\n%s", r.in.BackupPath, steps)
	}
	if n := strings.Count(steps+r.stderr.String(), sentinel); n != 0 {
		t.Errorf("the value occurs %d times in the op log:\n%s\n%s", n, steps, r.stderr.String())
	}
	if len(res.ConfigChanges) != 1 || res.ConfigChanges[0].New != sentinel {
		t.Errorf("configChanges %+v, want new %s (non-vacuity)", res.ConfigChanges, sentinel)
	}
}

// TC-RA-76: problem() maps config-invalid, restored and backupPath from the helper result. Owner WI 4.2. Requirements: FR-8, CON-2.
func TestTCRA76_ProblemMapsConfigInvalidRestoredAndBackupPath(t *testing.T) {
	errs := []spec.ConfigError{{Path: "x", Message: "refused by helper"}}
	for _, c := range []struct {
		res  helperResult
		slug problem.Slug
		ext  map[string]any
	}{
		{helperResult{Error: &helperError{Slug: problem.ConfigInvalid, Detail: "d", ConfigPath: "/etc/nexus/nexusd.yaml",
			Errors: errs}}, problem.ConfigInvalid,
			map[string]any{"configPath": "/etc/nexus/nexusd.yaml", "errors": errs}},
		{helperResult{Restored: true, BackupPath: "/b.bak", Error: &helperError{Slug: problem.ConfigApplyFailed,
			Detail: "d"}}, problem.ConfigApplyFailed,
			map[string]any{"restored": true, "backupPath": "/b.bak"}},
		{helperResult{BackupPath: "/b.bak", Error: &helperError{Slug: problem.ConfigRestoreFailed, Detail: "d"}},
			problem.ConfigRestoreFailed, map[string]any{"backupPath": "/b.bak"}},
		{helperResult{BackupPath: "/b.bak", Error: &helperError{Slug: problem.ConfigConflict, Detail: "d"}},
			problem.ConfigConflict, nil},
		{helperResult{Error: &helperError{Slug: "config-from-the-future", Detail: "d"}}, problem.InternalError, nil},
	} {
		p := c.res.problem()
		if p.Slug() != c.slug {
			t.Errorf("%s: slug %s, want %s", c.res.Error.Slug, p.Slug(), c.slug)
			continue
		}
		got := p.Extensions
		if len(got) == 0 && len(c.ext) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.ext) {
			t.Errorf("%s: extensions %v, want %v", c.slug, got, c.ext)
		}
	}
}

// TC-RA-E18: The coalesced-signal case is still judged by digest. Owner WI 4.2. Requirements: FR-8.
func TestTCRAE18_TheCoalescedSignalCaseIsStillJudgedByDigest(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.rl.coalesce = true
	h0, s0 := r.rl.handled, r.rl.signals
	r.f.sighup() // Pending before the helper's Reload
	res := r.run(t)
	if res.Error != nil || !res.Changed {
		t.Fatalf("result %+v (error %+v), want success", res, res.Error)
	}
	if last := r.rl.statuses[len(r.rl.statuses)-1]; last.SHA256 != shaHex(r.want(t)) {
		t.Errorf("the status covers %s, want sha(out)", last.SHA256)
	}
	r.wantPC(t)
	if r.rl.handled-h0 != 1 || r.rl.signals-s0 != 2 || r.rl.coalesced != 1 {
		t.Errorf("handled %d for %d signals (coalesced %d), want 1 for 2", r.rl.handled-h0, r.rl.signals-s0,
			r.rl.coalesced)
	}
}

// TC-RA-E19: A change landing after the helper's backup ends config-conflict with only the backup written. Owner WI 4.2. Requirements: FR-7, ASM-7.
func TestTCRAE19_AChangeLandingAfterTheHelperSBackupEndsConfig(t *testing.T) {
	r := newCSRig(t, 0o600)
	other := r.otherValid(t)
	r.beforeStep = func(name string) error {
		if name == stepFaultAfterBackup {
			return os.WriteFile(r.cfgPath, other, 0o600)
		}
		return nil
	}
	res := r.run(t)
	wantSlug(t, res, problem.ConfigConflict)
	if !bytes.Equal(r.disk(t), other) {
		t.Error("the disk does not hold the other bytes")
	}
	if !bytes.Equal(readBytes(t, r.in.BackupPath), r.orig) {
		t.Error(".bak != the original bytes")
	}
	if r.rl.reloadCalls != 0 {
		t.Errorf("reloadCalls %d, want 0", r.rl.reloadCalls)
	}
}

// TC-RA-E20: Stale temporary files from an earlier crash are removed. Owner WI 4.2. Requirements: NFR-4, NFR-5.
func TestTCRAE20_StaleTemporaryFilesFromAnEarlierCrashAreRemoved(t *testing.T) {
	r := newCSRig(t, 0o600)
	for _, n := range []string{".nexusd.yaml.123.tmp", ".nexusd.yaml.bak.9.tmp", "notes.tmp"} {
		if err := os.WriteFile(filepath.Join(r.cfgDir, n), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if res := r.run(t); res.Error != nil {
		t.Fatalf("error %+v", res.Error)
	}
	names := dirNames(t, r.cfgDir)
	for _, n := range []string{".nexusd.yaml.123.tmp", ".nexusd.yaml.bak.9.tmp"} {
		if slices.Contains(names, n) {
			t.Errorf("%s is still present", n)
		}
	}
	if !slices.Contains(names, "notes.tmp") {
		t.Error("the unrelated notes.tmp was removed")
	}
}

// TC-RA-E21: helperInput and helperResult stay additive. Owner WI 4.2. Requirements: CON-2.
func TestTCRAE21_HelperInputAndHelperResultStayAdditive(t *testing.T) {
	var in helperInput
	if err := json.Unmarshal([]byte(`{"mode":"update","runningVersion":"v9.9.1","expectedVersion":"v9.9.2",`+
		`"packageDir":"/var/lib/nexus/ops/x/pkg/nexusd","health":{"url":"https://127.0.0.1:8443","timeout":30000000000},`+
		`"installTimeout":120000000000,"paths":{"Binary":"/usr/local/bin/nexusd","Unit":"nexusd.service"}}`),
		&in); err != nil || in.Mode != modeUpdate || in.ConfigPath != "" {
		t.Fatalf("decode update input: %v, %+v", err, in)
	}
	var res helperResult
	if err := json.Unmarshal([]byte(`{"changed":false,"rolledBack":true,"previousVersion":"v9.9.1",`+
		`"newVersion":"v9.9.2","error":{"slug":"update-failed","detail":"d","step":"health","condition":2}}`),
		&res); err != nil || res.Error.Step != "health" {
		t.Fatalf("decode update result: %v, %+v", err, res)
	}
	b, err := json.Marshal(helperResult{Changed: true, PreviousVersion: "v9.9.1", NewVersion: "v9.9.2",
		Error: &helperError{Slug: problem.UpdateFailed, Detail: "d", Step: "health"}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"configPath", "configChanges", "restartRequired", "backupPath", "restored"} {
		if _, ok := m[k]; ok {
			t.Errorf("an update result has %q: %s", k, b)
		}
	}
	for _, k := range []string{"configPath", "errors", "stage"} {
		if _, ok := m["error"].(map[string]any)[k]; ok {
			t.Errorf("an update result's error has %q: %s", k, b)
		}
	}
	for _, mode := range []string{modeRestart, modeUpdate, modeRollback, modeConfigSet} {
		if _, ok := modeOf(mode); !ok {
			t.Errorf("modeOf(%q) is unknown", mode)
		}
	}
}

// TC-RA-E27: The worst-case unconfirmed-then-restored path completes within NFR-2's 30 s. Owner WI 4.2. Requirements: NFR-2, FR-8.
func TestTCRAE27_TheWorstCaseUnconfirmedThenRestoredPathCompletes(t *testing.T) {
	const outside = 2500 * time.Millisecond
	if 2*reloadTimeout+outside > 30*time.Second {
		t.Errorf("NFR-2: 2*reloadTimeout + %s = %s, over 30s", outside, 2*reloadTimeout+outside)
	}
	for _, inactive := range []int{0, 2} {
		t.Run(map[int]string{0: "A", 2: "B inactive 2"}[inactive], func(t *testing.T) {
			r := newCSRig(t, 0o600)
			r.rl.inactiveFor = inactive
			r.rl.noAnswer = 1
			r.rl.onReload = func(call int) {
				if call == inactive+2 { // The restore's reload
					r.rl.delayN, r.rl.delayD = 1, reloadTimeout-reloadPollInterval
				}
			}
			res := r.run(t)
			r.wantRestored(t, res, "")
			if r.elapsed() > 2*reloadTimeout {
				t.Errorf("virtual time %s, over 2*reloadTimeout", r.elapsed())
			}
			last := r.rl.statuses[len(r.rl.statuses)-1]
			if !last.Applied || last.SHA256 != shaHex(r.orig) {
				t.Errorf("the last status %+v, want applied with the backup's digest", last)
			}
			if !last.At.Equal(r.f.Now()) || r.f.Now().Equal(r.t0.Add(2*reloadTimeout)) {
				t.Errorf("the restore attempt ended at %s, want at the delayed status %s, not its deadline",
					r.elapsed(), last.At.Sub(r.t0))
			}
		})
	}
}

// TC-RA-E31: The restore attempt skips a late status for the new file and confirms only the backup's digest. Owner WI 4.2. Requirements: FR-8.
// F-RELOAD handles a signal that waited for a slow apply 1 ms after that
// status, so the restore's signal is delayed 3 s for a poll to see the late
// status: otherwise it is overwritten before any poll and the skip is never
// exercised.
func TestTCRAE31_TheRestoreAttemptSkipsALateStatusForTheNewFile(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.rl.slowN, r.rl.slowD = 1, reloadTimeout+2*time.Second
	r.rl.onReload = func(call int) {
		if call == 2 {
			r.rl.delayN, r.rl.delayD = 1, 3*time.Second
		}
	}
	res := r.run(t)
	r.wantRestored(t, res, "")
	out := shaHex(r.want(t))
	late := slices.IndexFunc(r.rl.statuses, func(s reloadstatus.Status) bool { return s.SHA256 == out })
	if late < 0 || !r.rl.statuses[late].At.After(r.t0.Add(reloadTimeout)) {
		t.Fatalf("no late status for sha(out) after the first deadline: %+v", r.rl.statuses)
	}
	if !strings.Contains(r.stdout.String(), "skipped reload status") {
		t.Errorf("the restore wait never saw and skipped the late status:\n%s", r.stdout.String())
	}
	if last := r.rl.statuses[len(r.rl.statuses)-1]; !last.Applied || last.SHA256 != shaHex(r.orig) ||
		!last.At.Equal(r.f.Now()) {
		t.Errorf("the restore ended on %+v at %s, want applied with the backup's digest", last, r.elapsed())
	}
	if r.elapsed() > 2*reloadTimeout {
		t.Errorf("virtual time %s, over 2*reloadTimeout", r.elapsed())
	}
}

// TC-RA-E32: An inactive unit that outlasts the attempt deadline ends the attempt at reloadTimeout and restores. Owner WI 4.2. Requirements: FR-8, NFR-2.
func TestTCRAE32_AnInactiveUnitThatOutlastsTheAttemptDeadlineEnds(t *testing.T) {
	n := int(reloadTimeout/reloadRetryInterval) + 3
	r := newCSRig(t, 0o600)
	r.rl.inactiveFor = n
	res := r.run(t)
	deadline := r.t0.Add(reloadTimeout)
	first := 0
	for i, at := range r.rl.callTimes {
		if at.Before(deadline) {
			first++
			if d := r.rl.deadlines[i]; d.IsZero() || d.After(deadline) {
				t.Errorf("reload call %d's deadline %v, want <= t0+reloadTimeout", i+1, d)
			}
		}
	}
	if first >= len(r.rl.callTimes) || !r.rl.callTimes[first].Equal(deadline) {
		t.Fatalf("the first attempt did not end at t0+reloadTimeout: calls at %v", r.rl.callTimes)
	}
	// The restore attempt spends the remaining inactive calls, one per
	// retry interval, from its own fresh deadline.
	left := n - first
	if time.Duration(left)*reloadRetryInterval < reloadTimeout {
		r.wantRestored(t, res, "")
	} else {
		wantSlug(t, res, problem.ConfigRestoreFailed)
	}
	if r.elapsed() > 2*reloadTimeout {
		t.Errorf("virtual time %s, over 2*reloadTimeout", r.elapsed())
	}
}

// TC-RA-ERR19: A config-base mismatch at the helper ends config-conflict with nothing written. Owner WI 4.2. Requirements: FR-7.
func TestTCRAERR19_AConfigBaseMismatchAtTheHelperEndsConfigConflict(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.writeIn(t)
	other := r.otherValid(t)
	if err := os.WriteFile(r.cfgPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	res, ok := r.exec(t)
	if !ok {
		t.Fatalf("no result\n%s", r.stderr.String())
	}
	wantSlug(t, res, problem.ConfigConflict)
	if !bytes.Equal(r.disk(t), other) {
		t.Error("the disk does not hold the rewritten bytes")
	}
	r.wantNoBackup(t)
	if r.rl.reloadCalls != 0 {
		t.Errorf("reloadCalls %d, want 0", r.rl.reloadCalls)
	}
}

// TC-RA-ERR20: A helper re-validation failure ends config-invalid with nothing written. Owner WI 4.2. Requirements: FR-7, ASM-10.
func TestTCRAERR20_AHelperReValidationFailureEndsConfigInvalidWith(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.check = func([]byte, string) config.ErrorList {
		return config.ErrorList{{Path: "x", Msg: "refused by helper"}}
	}
	res := r.run(t)
	wantSlug(t, res, problem.ConfigInvalid)
	want := []spec.ConfigError{{Path: "x", Message: "refused by helper"}}
	if !reflect.DeepEqual(res.Error.Errors, want) || res.Error.ConfigPath != r.cfgPath {
		t.Errorf("errors %+v, configPath %q; want %+v, %q", res.Error.Errors, res.Error.ConfigPath, want, r.cfgPath)
	}
	if p := res.problem(); p.Slug() != problem.ConfigInvalid || !reflect.DeepEqual(p.Extensions["errors"], want) {
		t.Errorf("problem %+v", p)
	}
	if !bytes.Equal(r.disk(t), r.orig) {
		t.Error("the disk changed")
	}
	r.wantNoBackup(t)
	if r.rl.reloadCalls != 0 {
		t.Errorf("reloadCalls %d, want 0", r.rl.reloadCalls)
	}
}

// TC-RA-ERR21: A reload refused at stage apply restores the backup and ends config-apply-failed. Owner WI 4.2. Requirements: FR-8.
func TestTCRAERR21_AReloadRefusedAtStageApplyRestoresTheBackupAnd(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.rl.refuseApply = 1
	res := r.run(t)
	r.wantRestored(t, res, "apply")
	if !strings.Contains(res.Error.Detail, "fake provider refused the new configuration") {
		t.Errorf("detail %q lacks the first reload's error", res.Error.Detail)
	}
	if got := modeOfFile(t, r.cfgPath); got != 0o600 {
		t.Errorf("mode %04o, want the original 0600", got)
	}
	if r.rl.reloadCalls != 2 {
		t.Errorf("reloadCalls %d, want 2", r.rl.reloadCalls)
	}
}

// TC-RA-ERR22: A reload refused at stage load with an empty digest restores rather than reporting a conflict. Owner WI 4.2. Requirements: FR-8.
func TestTCRAERR22_AReloadRefusedAtStageLoadWithAnEmptyDigest(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.rl.loadRefuse = 1
	res := r.run(t)
	r.wantRestored(t, res, "load")
}

// TC-RA-ERR23: An unconfirmed reload is restored, and an unconfirmed restore ends config-restore-failed. Owner WI 4.2. Requirements: FR-8, NFR-2.
func TestTCRAERR23_AnUnconfirmedReloadIsRestoredAndAnUnconfirmed(t *testing.T) {
	t.Run("A", func(t *testing.T) {
		r := newCSRig(t, 0o600)
		r.rl.noAnswer = 1
		res := r.run(t)
		r.wantRestored(t, res, "")
		if len(r.rl.callTimes) != 2 || r.rl.callTimes[1].Sub(r.t0) != reloadTimeout {
			t.Errorf("reload calls at %v, want the restore's at t0+%s", r.rl.callTimes, reloadTimeout)
		}
	})
	t.Run("B", func(t *testing.T) {
		r := newCSRig(t, 0o600)
		r.rl.noAnswer = 2
		res := r.run(t)
		wantSlug(t, res, problem.ConfigRestoreFailed)
		if res.BackupPath != r.in.BackupPath || !strings.Contains(res.Error.Detail, configRecoveryDoc) {
			t.Errorf("backupPath %q, detail %q", res.BackupPath, res.Error.Detail)
		}
		if configRecoveryDoc != "docs/install.md#recovering-from-a-failed-configuration-change" {
			t.Errorf("recovery doc %q", configRecoveryDoc)
		}
		if !bytes.Equal(r.disk(t), r.orig) {
			t.Error("the disk does not hold the backup bytes")
		}
		if r.elapsed() > 2*reloadTimeout || r.elapsed() > configSetMaxRuntime {
			t.Errorf("virtual time %s, over 2*reloadTimeout", r.elapsed())
		}
		if configSetMaxRuntime != 54*time.Second {
			t.Errorf("configSetMaxRuntime %s, want 54s (A-5)", configSetMaxRuntime)
		}
	})
}

// TC-RA-ERR24: A file replaced before the reload ends config-conflict with no restore. Owner WI 4.2. Requirements: FR-8, FR-7.
func TestTCRAERR24_AFileReplacedBeforeTheReloadEndsConfigConflict(t *testing.T) {
	r := newCSRig(t, 0o600)
	other := r.otherValid(t)
	r.rl.replaceBefore = other
	res := r.run(t)
	wantSlug(t, res, problem.ConfigConflict)
	if res.Restored {
		t.Error("restored, want no restore")
	}
	if !bytes.Equal(r.disk(t), other) {
		t.Error("the other bytes were not kept")
	}
	if r.rl.reloadCalls != 1 {
		t.Errorf("reloadCalls %d, want 1", r.rl.reloadCalls)
	}
}

// TC-RA-ERR25: A nil validator makes the config-set mode refuse to start. Owner WI 4.2. Requirements: FR-7, CON-2.
func TestTCRAERR25_ANilValidatorMakesTheConfigSetModeRefuseToStart(t *testing.T) {
	r := newCSRig(t, 0o600)
	r.writeIn(t)
	cfg0 := snapshot(t, r.cfgDir)
	var out, errw bytes.Buffer
	if code := Helper([]string{"--dir", r.dir}, &out, &errw, nil); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errw.String(), noValidatorText) {
		t.Errorf("stderr %q lacks %q", errw.String(), noValidatorText)
	}
	// The same refusal through the fake counts no reload.
	r.check = nil
	if _, ok := r.exec(t); ok || r.code != 1 {
		t.Errorf("exit %d, result %v; want 1 and none", r.code, ok)
	}
	if changed, removed := changedEntries(cfg0, snapshot(t, r.cfgDir)); len(changed)+len(removed) != 0 {
		t.Errorf("the config dir changed: %v %v", changed, removed)
	}
	if r.rl.reloadCalls != 0 {
		t.Errorf("reloadCalls %d, want 0", r.rl.reloadCalls)
	}

	// An update-mode input with a nil check still runs.
	dir := t.TempDir()
	writeInput(t, dir, `{"mode":"update","runningVersion":"v9.9.1","noChange":true}`)
	out.Reset()
	errw.Reset()
	if code := Helper([]string{"--dir", dir}, &out, &errw, nil); code != 0 {
		t.Errorf("update with a nil check: exit %d, %s", code, errw.String())
	}
	if _, ok, err := readResult(dir); !ok || err != nil {
		t.Errorf("update with a nil check: result %v, %v", ok, err)
	}
}
