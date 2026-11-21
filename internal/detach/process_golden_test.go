// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package detach

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the systemd golden files from this code")

// goldenSpec is the fixed Spec of the systemd golden files. It carries a
// value of every field, the sensitive environment included.
var goldenSpec = Spec{
	Argv:           []string{"/usr/bin/apt-get", "-y", "install", "nginx"},
	Env:            []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "SECRET_TOKEN=s3cr3t"},
	Dir:            "/var/lib/nexus",
	Credential:     &Credential{UID: 1000, GID: 1000, Groups: []uint32{27, 100}},
	Timeout:        10 * time.Minute,
	Grace:          30 * time.Second,
	OutputCapBytes: 1 << 20,
	KillMode:       KillProcess,
	MaxRuntime:     2 * time.Hour,
}

// nginx-provider TC-18: the systemd path is byte-identical (NFR-9, CON-11).
// The goldens in testdata/golden were produced by the code at 2953509, which
// is also the code of this package's launcher.go and spec.go when the process
// backend was added. To regenerate them from that code without git stash:
//
//	git worktree add /tmp/nexus-2953509 2953509
//	cp internal/detach/process_golden_test.go /tmp/nexus-2953509/internal/detach/
//	(cd /tmp/nexus-2953509 && go test ./internal/detach -run TestSystemdArgvAndSpecBytesUnchanged -update)
//	cp /tmp/nexus-2953509/internal/detach/testdata/golden/* internal/detach/testdata/golden/
//	git worktree remove /tmp/nexus-2953509
//
// Supervise(dir, diag, ModeUnit) is the existing code path: every test in
// supervisor_test.go runs `nexusd op-supervise --dir` without --process and
// passes unchanged.
func TestSystemdArgvAndSpecBytesUnchanged(t *testing.T) {
	argv := Argv("01JC4ZQ8M6W5Y3T2R1P0N9K8H7", "/usr/local/bin/nexusd", "/var/lib/nexus/ops", goldenSpec)
	gotArgv, err := json.MarshalIndent(argv, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	gotArgv = append(gotArgv, '\n')
	dir := t.TempDir()
	if err := WriteSpec(dir, goldenSpec); err != nil {
		t.Fatal(err)
	}
	gotSpec, err := os.ReadFile(filepath.Join(dir, SpecFile))
	if err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", "golden")
	if *update {
		if err := os.MkdirAll(golden, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, b := range map[string][]byte{"systemd-argv.json": gotArgv, "spec.json": gotSpec} {
			if err := os.WriteFile(filepath.Join(golden, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, got := range map[string][]byte{"systemd-argv.json": gotArgv, "spec.json": gotSpec} {
		want, err := os.ReadFile(filepath.Join(golden, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed:\n got %s\nwant %s", name, got, want)
		}
	}
	if slices.Contains(argv, "--process") {
		t.Errorf("systemd argv carries --process: %q", argv)
	}
}
