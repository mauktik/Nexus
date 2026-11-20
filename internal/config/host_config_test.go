// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/daemon"
)

// TC-HP-ERR4, the validate-config half: daemon.CheckConfig reports an
// unknown key in providers.host with its location, as config.Load does in
// TestTCHPERR4_AnUnknownKeyInProvidersHostFails. Requirements: FR-1.
func TestHostUnknownKeyFailsCheckConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(path, []byte("providers:\n  host:\n    enabled: true\n    interval: 5s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, list := daemon.CheckConfig(path)
	if len(list) == 0 {
		t.Fatalf("CheckConfig accepted the file, configuration %+v", cfg)
	}
	msg := list.Error()
	if !strings.Contains(msg, "providers.host.interval") || !strings.Contains(msg, path+":4") {
		t.Errorf("CheckConfig errors %q do not name providers.host.interval at %s:4", msg, path)
	}
}
