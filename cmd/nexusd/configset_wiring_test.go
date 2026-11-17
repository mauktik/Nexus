package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TC-RA-77: The production service-helper wiring passes the daemon's validator. Owner WI 4.2. Requirements: FR-7, CON-2, ASM-10.
//
// The unit is a name no host has, so `systemctl reload` fails whether
// systemctl is absent (fork/exec), not connected, or present. That is a
// hard reload error, not "not active", so neither attempt waits: the
// helper restores the backup and records config-restore-failed, exit 0
// (it exits 1 only when it refuses to start or cannot record a result).
func TestTCRA77_TheProductionServiceHelperWiringPassesTheDaemonS(t *testing.T) {
	t.Run("a valid change reaches the reload", func(t *testing.T) { wiringValid(t) })
	t.Run("the daemon's own check refuses an invalid one", func(t *testing.T) { wiringInvalid(t) })
}

// wiringRig is a config file, a helper directory with its input.json, and
// the unit name that no host has.
type wiringRig struct {
	cfgPath, dir string
	orig         []byte
}

func newWiringRig(t *testing.T, change string) *wiringRig {
	t.Helper()
	orig, err := os.ReadFile(filepath.Join("..", "..", "deploy", "nexusd.default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	orig = bytes.Replace(orig, []byte("nexusd:\n    enabled: false"), []byte("nexusd:\n    enabled: true"), 1)
	cfgDir, stateDir, dir := t.TempDir(), t.TempDir(), t.TempDir()
	cfgPath := filepath.Join(cfgDir, "nexusd.yaml")
	if err := os.WriteFile(cfgPath, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(orig)
	in, err := json.Marshal(map[string]any{
		"mode": "config-set", "configPath": cfgPath, "backupPath": cfgPath + ".bak",
		"changes": []string{change}, "baseSha256": hex.EncodeToString(sum[:]),
		"stateDir": stateDir, "reloadTimeout": int64(12e9), "health": map[string]any{},
		"paths": map[string]string{"Unit": wiringUnit},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.json"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	return &wiringRig{cfgPath: cfgPath, dir: dir, orig: orig}
}

// wiringUnit is a name no host has.
const wiringUnit = "nexus-ra77-absent.service"

// run is the production entry point's service-helper, exit 0 wanted: a
// result was recorded.
func (r *wiringRig) run(t *testing.T) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run([]string{"service-helper", "--dir", r.dir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (a recorded result)\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "fault hook armed") {
		t.Errorf("the production wiring armed the fault hook:\n%s", out.String())
	}
	return out.String(), errb.String()
}

// result is result.json's error and backup path.
func (r *wiringRig) result(t *testing.T) (slug string, errs []struct{ Path string }, backupPath string) {
	t.Helper()
	var res struct {
		Error struct {
			Slug   string `json:"slug"`
			Errors []struct {
				Path string `json:"path"`
			} `json:"errors"`
		} `json:"error"`
		BackupPath string `json:"backupPath"`
	}
	b, err := os.ReadFile(filepath.Join(r.dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("result.json %s: %v", b, err)
	}
	for _, e := range res.Error.Errors {
		errs = append(errs, struct{ Path string }{e.Path})
	}
	return res.Error.Slug, errs, res.BackupPath
}

func wiringValid(t *testing.T) {
	r := newWiringRig(t, "providers.host.enabled=true")
	_, stderr := r.run(t)
	if !strings.Contains(stderr, "systemctl reload "+wiringUnit) {
		t.Errorf("stderr lacks the systemctl error from sys.Reload:\n%s", stderr)
	}
	if strings.Contains(stderr, "no validator") {
		t.Errorf("stderr has the no-validator refusal:\n%s", stderr)
	}
	if b, err := os.ReadFile(r.cfgPath + ".bak"); err != nil || !bytes.Equal(b, r.orig) {
		t.Errorf(".bak: %v, equal to the original %v", err, bytes.Equal(b, r.orig))
	}
	if b, err := os.ReadFile(r.cfgPath); err != nil || !bytes.Equal(b, r.orig) {
		t.Errorf("the restore did not put the original back: %v", err)
	}
	if slug, _, bak := r.result(t); slug != "config-restore-failed" || bak != r.cfgPath+".bak" {
		t.Errorf("result slug %q, backupPath %q, want config-restore-failed with the backup path", slug, bak)
	}
}

// wiringInvalid sends a change that config.Edit accepts and only the
// daemon's CheckConfigData refuses (audit.retentionDays=0, as TC-RA-ERR29
// does): a validator that accepts everything would let it through to the
// backup and the reload.
func wiringInvalid(t *testing.T) {
	r := newWiringRig(t, "audit.retentionDays=0")
	_, stderr := r.run(t)
	slug, errs, _ := r.result(t)
	if slug != "config-invalid" || len(errs) == 0 || errs[0].Path != "audit.retentionDays" {
		t.Errorf("result slug %q, errors %+v, want config-invalid at audit.retentionDays", slug, errs)
	}
	if _, err := os.Stat(r.cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("a .bak exists after a refused change: %v", err)
	}
	if b, err := os.ReadFile(r.cfgPath); err != nil || !bytes.Equal(b, r.orig) {
		t.Errorf("the config file is not byte-identical to the original: %v", err)
	}
	if strings.Contains(stderr, "systemctl reload") {
		t.Errorf("the helper reached the reload:\n%s", stderr)
	}
}
