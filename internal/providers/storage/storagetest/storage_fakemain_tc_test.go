package storagetest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
)

// TC-STG-78 (code review CR-8): a write issued from inside the sandbox, where
// StateDir is read-only, is answered with OpenZFS's own denial, not with a
// fake error. Main logs it to the reads log with Write set, leaves calls.log
// and state.json alone, and F-ZFS decides from CapEff, which has no
// CAP_SYS_ADMIN in a process that is neither root nor privileged.
func TestTCSTG78_AWriteFromInsideTheSandboxIsDeniedByF(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Main reads CapEff from /proc")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory's mode bits; the sandbox case is a non-root or read-only-mount process")
	}
	capture, err := filepath.Abs(capturegate.Require(t, capturedDir("home-server")))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	reads := filepath.Join(t.TempDir(), "reads.log")
	if err := os.Chmod(state, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o755) })

	var stdout, stderr bytes.Buffer
	code := Main(t, MainConfig{Capture: capture, StateDir: state, ReadsLog: reads}, []string{"/usr/sbin/zpool", "scrub", "z1"}, &stdout, &stderr)
	if code != 1 || stderr.String() != "cannot scrub z1: permission denied\n" {
		t.Errorf("exit %d, stderr %q; want 1 and %q", code, stderr.String(), "cannot scrub z1: permission denied\n")
	}
	if _, err := os.Stat(filepath.Join(state, CallsFile)); err == nil {
		t.Errorf("%s was written in a read-only state directory", CallsFile)
	}
	b, err := os.ReadFile(reads)
	if err != nil {
		t.Fatal(err)
	}
	var line ReadLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &line); err != nil {
		t.Fatalf("reads log %q: %v", b, err)
	}
	if !line.Write || len(line.Argv) < 2 || line.Argv[1] != "scrub" {
		t.Errorf("reads log line %+v, want the scrub marked Write", line)
	}
}
