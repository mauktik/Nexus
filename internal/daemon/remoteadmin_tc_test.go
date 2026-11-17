// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-56, TC-RA-57,
// TC-RA-58, TC-RA-59 and TC-RA-ERR16 (WI 3.2): the reload status the daemon
// writes after every reload, and the byte-level configuration check.

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/reloadstatus"
)

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeBytes writes body to path and returns it, so the test knows the
// exact bytes the daemon will read.
func writeBytes(t *testing.T, path, body string) []byte {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return []byte(body)
}

// statusAt waits until the reload status in dir has sequence number seq and
// returns it.
func statusAt(t *testing.T, dir string, seq uint64) reloadstatus.Status {
	t.Helper()
	var s reloadstatus.Status
	eventually(t, 5*time.Second, fmt.Sprintf("reload status with seq %d", seq), func() bool {
		got, ok, err := reloadstatus.Read(dir)
		if err != nil {
			t.Fatalf("reloadstatus.Read: %v", err)
		}
		s = got
		return ok && got.Seq >= seq
	})
	if s.Seq != seq {
		t.Fatalf("reload status seq %d, want %d", s.Seq, seq)
	}
	return s
}

var bootID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TC-RA-56: every reload, SIGHUP or API, writes a status with the digest of
// the bytes read and the next seq.
func TestTCRA56_EveryReloadSIGHUPOrAPIWritesAStatusWithTheDigest(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	body := func(hostEnabled bool, extra string) string {
		return fmt.Sprintf("host: {name: daemon-test}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n"+
			"  apt: {enabled: true, lockWait: 100s}\n  host: {enabled: %t}\n%s", listen, hostEnabled, extra)
	}
	writeBytes(t, cfgPath, body(true, ""))
	r := startInProcess(t, cfgPath, &reloadProvider{})
	dir := r.d.StateDir()

	// Step 1.
	bytes1 := writeBytes(t, cfgPath, body(false, ""))
	r.signal(t, syscall.SIGHUP)
	s1 := statusAt(t, dir, 1)
	if s1.SHA256 != shaHex(bytes1) {
		t.Errorf("step 1: sha256 %s, want %s", s1.SHA256, shaHex(bytes1))
	}
	if !s1.Applied || s1.Stage != "" || s1.Error != "" {
		t.Errorf("step 1: applied %v, stage %q, error %q; want applied with no stage or error", s1.Applied, s1.Stage, s1.Error)
	}
	if s1.RestartRequired == nil || len(s1.RestartRequired) != 0 {
		t.Errorf("step 1: restartRequired %#v, want []", s1.RestartRequired)
	}

	// Step 2.
	bytes2 := writeBytes(t, cfgPath, body(false, "audit: {retentionDays: 30}\n"))
	res, err := r.d.Reload(context.Background())
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	s2 := statusAt(t, dir, 2)
	if s2.SHA256 != shaHex(bytes2) {
		t.Errorf("step 2: sha256 %s, want %s", s2.SHA256, shaHex(bytes2))
	}
	if !slices.Equal(s2.RestartRequired, []string{"audit"}) || !slices.Equal(s2.RestartRequired, res.RestartRequired) {
		t.Errorf("step 2: restartRequired %q, Reload returned %q; want [audit] in both", s2.RestartRequired, res.RestartRequired)
	}
	if !s2.Applied {
		t.Error("step 2: not applied")
	}

	if !bootID.MatchString(s1.Boot) || s1.Boot != s2.Boot {
		t.Errorf("boot %q then %q, want the same 16-hex value", s1.Boot, s2.Boot)
	}
	r.cancel()
	r.wait(t)
}

// TC-RA-57: a refused reload records its stage and digest, and the
// configuration in force is unchanged.
func TestTCRA57_ARefusedReloadRecordsItsStageAndDigestAndThe(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	body := func(lockWait, socket string) string {
		return fmt.Sprintf("listen: [%q]\ntls: {mode: off}\nproviders:\n"+
			"  apt: {enabled: true, lockWait: %s}\n  docker: {enabled: true, socket: %s}\n", listen, lockWait, socket)
	}
	writeBytes(t, cfgPath, body("100s", "/run/docker.sock"))
	apt, docker := &sectionProvider{name: "apt"}, &sectionProvider{name: "docker"}
	r := startInProcess(t, cfgPath, apt, docker)
	dir := r.d.StateDir()
	inForce := func(step string) {
		t.Helper()
		if got := apt.accepted.Load(); got != "1m40s" {
			t.Errorf("%s: apt section in force %v, want the startup 1m40s", step, got)
		}
		if got := docker.accepted.Load(); got != "/run/docker.sock" {
			t.Errorf("%s: docker section in force %v, want the startup /run/docker.sock", step, got)
		}
	}
	var boot string

	// Step 1.
	invalid := writeBytes(t, cfgPath, "listen: [\"127.0.0.1:1\"\n  bad: : yaml\n")
	r.signal(t, syscall.SIGHUP)
	s := statusAt(t, dir, 1)
	if s.Applied || s.Stage != "load" || s.SHA256 != shaHex(invalid) || s.Error == "" {
		t.Errorf("step 1: %+v, want not applied, stage load, sha256 %s and an error", s, shaHex(invalid))
	}
	boot = s.Boot
	inForce("step 1")

	// Step 2.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	r.signal(t, syscall.SIGHUP)
	s = statusAt(t, dir, 2)
	if s.Applied || s.Stage != "load" || s.SHA256 != "" || s.Error == "" {
		t.Errorf("step 2: %+v, want not applied, stage load, an empty sha256 and an error", s)
	}
	inForce("step 2")

	// Step 3.
	refused := writeBytes(t, cfgPath, body("400s", rejectSocket))
	_, err := r.d.Reload(context.Background())
	var re *provider.ReloadError
	if !errors.As(err, &re) || re.Stage != "apply" {
		t.Fatalf("step 3: %v, want a *provider.ReloadError of stage apply", err)
	}
	s = statusAt(t, dir, 3)
	if s.Applied || s.Stage != "apply" || s.SHA256 != shaHex(refused) || s.Error == "" {
		t.Errorf("step 3: %+v, want not applied, stage apply, sha256 %s and an error", s, shaHex(refused))
	}
	if s.Boot != boot {
		t.Errorf("boot changed from %q to %q within one run", boot, s.Boot)
	}
	inForce("step 3")
	r.cancel()
	r.wait(t)
}

// TC-RA-58: the boot ID differs per daemon start.
func TestTCRA58_TheBootIDDiffersPerDaemonStart(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	writeAptConfig(t, cfgPath, freePort(t), "100s", "")
	stateDir := t.TempDir()
	var statuses []reloadstatus.Status
	for run := range 2 {
		r := startInProcessIn(t, cfgPath, stateDir, &reloadProvider{})
		if _, err := r.d.Reload(context.Background()); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
		statuses = append(statuses, statusAt(t, stateDir, 1))
		r.cancel()
		r.wait(t)
	}
	if statuses[0].Boot == "" || statuses[0].Boot == statuses[1].Boot {
		t.Fatalf("boot %q then %q, want two different non-empty values", statuses[0].Boot, statuses[1].Boot)
	}
}

// TC-RA-59: CheckConfigData on a file's bytes equals CheckConfig on the
// file, and CheckConfig is ReadFile + the one byte-level check.
func TestTCRA59_CheckConfigDataOnAFileSBytesEqualsCheckConfigOn(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "checkconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			path := checkconfigFixture(e.Name())
			wantCfg, wantErrs := CheckConfig(path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			gotCfg, gotErrs := CheckConfigData(data, path)
			if dump(gotErrs) != dump(wantErrs) || !reflect.DeepEqual(gotErrs, wantErrs) {
				t.Errorf("CheckConfigData errors:\n%s\nCheckConfig errors:\n%s", dump(gotErrs), dump(wantErrs))
			}
			if !reflect.DeepEqual(gotCfg, wantCfg) {
				t.Errorf("CheckConfigData and CheckConfig return different configurations")
			}
			if len(wantErrs) == 0 && wantCfg == nil {
				t.Errorf("no configuration for a valid fixture")
			}
		})
	}

	src, err := os.ReadFile("validate.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "core.ValidateProviders([]provider.Provider{p}"); n != 1 {
		t.Errorf("validate.go has %d per-provider loops, want one", n)
	}
	body := funcBody(t, string(src), "func checkConfigWith(")
	if !strings.Contains(body, "config.ReadFile(") || !strings.Contains(body, "checkConfigDataWith(") ||
		strings.Contains(body, "config.Load(") {
		t.Errorf("checkConfigWith is not config.ReadFile + checkConfigDataWith:\n%s", body)
	}
}

// funcBody returns the source of the top-level function starting with decl.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%s not found", decl)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("%s has no end", decl)
	}
	return src[i : i+j]
}

// TC-RA-ERR16: a reload-status write failure never changes the reload's
// outcome.
func TestTCRAERR16_AReloadStatusWriteFailureNeverChangesTheReloadS(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	listen := freePort(t)
	writeAptConfig(t, cfgPath, listen, "100s", "")
	p := &reloadProvider{}
	r := startInProcess(t, cfgPath, p)
	if err := os.Mkdir(filepath.Join(r.d.StateDir(), reloadstatus.FileName), 0o700); err != nil {
		t.Fatal(err)
	}

	writeAptConfig(t, cfgPath, listen, "200s", "")
	res, err := r.d.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if res.RestartRequired == nil || len(res.RestartRequired) != 0 {
		t.Errorf("restartRequired %#v, want []", res.RestartRequired)
	}
	if got := p.lockWait.Load(); got != "3m20s" {
		t.Errorf("apt section in force %v, want 3m20s", got)
	}
	var lines []string
	for line := range strings.Lines(logText(r.logs)) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["level"] == "ERROR" && strings.Contains(line, "reload-status") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Errorf("%d Error-level log lines name reload-status, want 1:\n%s", len(lines), strings.Join(lines, ""))
	}
	r.cancel()
	r.wait(t)
}
