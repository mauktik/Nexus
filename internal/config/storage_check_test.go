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

// TC-STG-30, the validate-config half: daemon.CheckConfig reports an unknown
// key in providers.storage with its location, as config.Load does in
// TestTCSTG30_UnknownKeysInProvidersStorageAreErrors. Requirements: FR-1.
func TestStorageUnknownKeyFailsCheckConfig(t *testing.T) {
	for _, tc := range []struct{ section, key string }{
		{"scrub:\n      poolz: []", "providers.storage.scrub.poolz"},
		{"thresholds:\n      poolCapacityWarning: 80", "providers.storage.thresholds.poolCapacityWarning"},
	} {
		path := filepath.Join(t.TempDir(), "nexusd.yaml")
		if err := os.WriteFile(path, []byte("providers:\n  storage:\n    "+tc.section+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, list := daemon.CheckConfig(path)
		if len(list) == 0 {
			t.Fatalf("CheckConfig accepted the file, configuration %+v", cfg)
		}
		msg := list.Error()
		if !strings.Contains(msg, tc.key) || !strings.Contains(msg, path+":4:7") {
			t.Errorf("CheckConfig errors %q do not name %s at %s:4:7", msg, tc.key, path)
		}
	}
}
