package svcctl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/svcctl"
)

// SEC-8 (CWE-200): an OpenRC init script can define its own status() and
// print "status: <token>". That token reaches callers holding services:read
// (get, a control problem's rawState, the operation log) as RawState, so
// RawState is OpenRC's own state word or "unknown", never the script's token.
func TestSEC8_OpenRCRawStateIsAnAllowListedWord(t *testing.T) {
	const secret = "hunter2-from-a-custom-status-function"
	for _, tc := range []struct {
		word, wantRaw string
		wantState     svcctl.State
	}{
		{"started", "started", svcctl.Running},
		{"stopped", "stopped", svcctl.Stopped},
		{"crashed", "crashed", svcctl.Failed},
		{"starting", "starting", svcctl.Starting},
		{"stopping", "stopping", svcctl.Stopping},
		{"inactive", "inactive", svcctl.Unknown},
		{"failed", "failed", svcctl.Unknown},
		{"hotplugged", "hotplugged", svcctl.Unknown},
		{"scheduled", "scheduled", svcctl.Unknown},
		{secret, "unknown", svcctl.Unknown},
		{"Started", "unknown", svcctl.Unknown},
	} {
		t.Run("describe "+tc.word, func(t *testing.T) {
			run := func(_ context.Context, argv []string) (svcctl.Result, error) {
				if strings.HasSuffix(argv[0], "rc-service") {
					return svcctl.Result{Output: []byte(" * status: " + tc.word + "\n")}, nil
				}
				return svcctl.Result{Output: []byte("nx-svc-test | default\n")}, nil
			}
			d, err := svcctl.Describe(t.Context(), platform.OpenRC, "nx-svc-test", svcctl.Options{Run: run, EtcRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if d.RawState != tc.wantRaw || d.State != string(tc.wantState) {
				t.Fatalf("Describe RawState %q State %q, want %q and %q", d.RawState, d.State, tc.wantRaw, tc.wantState)
			}
		})
		t.Run("list "+tc.word, func(t *testing.T) {
			run := func(_ context.Context, argv []string) (svcctl.Result, error) {
				if strings.HasSuffix(argv[0], "rc-status") {
					return svcctl.Result{Output: []byte(" nx-svc-test   [  " + tc.word + "  ]\n")}, nil
				}
				return svcctl.Result{Output: []byte("nx-svc-test | default\n")}, nil
			}
			es, err := svcctl.List(t.Context(), platform.OpenRC, svcctl.Options{Run: run, EtcRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if len(es) != 1 || es[0].RawState != tc.wantRaw || es[0].State != string(tc.wantState) {
				t.Fatalf("List = %+v, want RawState %q State %q", es, tc.wantRaw, tc.wantState)
			}
		})
	}
}
