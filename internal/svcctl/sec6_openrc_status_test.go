package svcctl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
)

// SEC-6 (CWE-200): an OpenRC init script can define its own status(), exit
// non-zero and print what it likes. A failed `rc-service NAME status` is
// reported to callers holding services:read (a get problem, an action's
// problem detail), so the script's text is never quoted, as SEC-4 keeps it
// out of an action's output: the error names the exit code only.
func TestSEC6_DescribeOpenRCStatusFailureQuotesNoScriptOutput(t *testing.T) {
	const secret = "token=hunter2-from-a-custom-status-function"
	run := func(_ context.Context, argv []string) (svcctl.Result, error) {
		return svcctl.Result{ExitCode: 3, Output: []byte("\n  " + secret + "\nsecond line\n")}, nil
	}
	_, err := svcctl.Describe(t.Context(), platform.OpenRC, "nx-svc-test", svcctl.Options{Run: run, EtcRoot: t.TempDir()})
	if err == nil {
		t.Fatal("Describe succeeded, want an error for a status that exited 3 with no status word")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "second line") {
		t.Fatalf("Describe's error quotes the init script's output: %v", err)
	}
	if !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("Describe's error = %v, want it to name the exit code", err)
	}
}
