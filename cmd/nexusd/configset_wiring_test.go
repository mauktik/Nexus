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
	const unit = "nexus-ra77-absent.service"
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
		"changes": []string{"providers.host.enabled=true"}, "baseSha256": hex.EncodeToString(sum[:]),
		"stateDir": stateDir, "reloadTimeout": int64(12e9), "health": map[string]any{},
		"paths": map[string]string{"Unit": unit},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.json"), in, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"service-helper", "--dir", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (a recorded result)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(),
			stderr.String())
	}
	if !strings.Contains(stderr.String(), "systemctl reload "+unit) {
		t.Errorf("stderr lacks the systemctl error from sys.Reload:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "no validator") {
		t.Errorf("stderr has the no-validator refusal:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "fault hook armed") {
		t.Errorf("the production wiring armed the fault hook:\n%s", stdout.String())
	}
	if b, err := os.ReadFile(cfgPath + ".bak"); err != nil || !bytes.Equal(b, orig) {
		t.Errorf(".bak: %v, equal to the original %v", err, bytes.Equal(b, orig))
	}
	if b, err := os.ReadFile(cfgPath); err != nil || !bytes.Equal(b, orig) {
		t.Errorf("the restore did not put the original back: %v", err)
	}
	var res struct {
		Error struct {
			Slug string `json:"slug"`
		} `json:"error"`
		BackupPath string `json:"backupPath"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Error.Slug != "config-restore-failed" ||
		res.BackupPath != cfgPath+".bak" {
		t.Errorf("result %s (%v), want config-restore-failed with the backup path", b, err)
	}
}
