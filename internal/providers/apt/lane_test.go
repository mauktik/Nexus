// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/oplog"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt/spec"
)

// lockBase is the directory testdata/capture-lock.sh captured from real
// apt and dpkg.
const lockBase = "lock/ubuntu-latest"

func captureExit(t *testing.T, name string) int {
	t.Helper()
	code, err := strconv.Atoi(strings.TrimSpace(string(capture(t, lockBase, name+".exit"))))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestLockTestdataIsCaptured(t *testing.T) {
	prov := string(capture(t, lockBase, "PROVENANCE"))
	for _, want := range []string{"captured: ", "apt: apt ", "dpkg: ", "script: internal/providers/apt/testdata/capture-lock.sh"} {
		if !strings.Contains(prov, want) {
			t.Errorf("PROVENANCE lacks %q:\n%s", want, prov)
		}
	}
}

// TC-ERR48: apt's exit 100 with lock text is package-manager-busy, whichever
// of the two texts it printed, and an unrelated exit 100 is apt-failed.
func TestLockTextMapsToBusy(t *testing.T) {
	cases := []struct {
		capture string
		want    problem.Slug
	}{
		{"frontend-timeout", problem.PackageManagerBusy}, // Unable to acquire the dpkg frontend lock
		{"frontend-nowait", problem.PackageManagerBusy},  // Could not get lock
		{"unrelated", problem.AptFailed},                 // Unable to locate package
	}
	for _, c := range cases {
		code := captureExit(t, c.capture)
		if code != aptExitFailure {
			t.Fatalf("%s: captured exit %d, want 100; the premise changed", c.capture, code)
		}
		perr := aptFailure("apt-get", detach.Exit{ExitCode: code}, capture(t, lockBase, c.capture+".stderr"))
		if perr == nil || perr.Slug() != c.want {
			t.Errorf("%s: %v, want %s", c.capture, perr, c.want)
		}
	}
	for _, text := range []string{"Unable to acquire the dpkg frontend lock", "Could not get lock"} {
		if !strings.Contains(string(capture(t, lockBase, "frontend-timeout.stderr"))+
			string(capture(t, lockBase, "frontend-nowait.stderr")), text) {
			t.Errorf("the captures lack %q; the premise changed", text)
		}
	}

	// apt's progress while it waits names the lock too, on stdout: a run
	// that waited, then failed for another reason, is not busy.
	waited := capture(t, lockBase, "waited.stdout")
	if !strings.Contains(string(waited), "Could not get lock") {
		t.Fatal("waited.stdout lacks the progress text; the premise changed")
	}
	if perr := aptFailure("apt-get", detach.Exit{ExitCode: 100}, waited); perr.Slug() != problem.AptFailed {
		t.Errorf("progress text: %v, want apt-failed", perr)
	}
	// Lock text with another status, a signal, or no start is not busy.
	stderr := capture(t, lockBase, "frontend-nowait.stderr")
	for _, x := range []detach.Exit{{ExitCode: 1}, {ExitCode: -1, Signal: "SIGKILL"}, {ExitCode: -1, Error: "exec: no such file"}} {
		if perr := aptFailure("apt-get", x, stderr); perr == nil || perr.Slug() != problem.AptFailed {
			t.Errorf("%+v: %v, want apt-failed", x, perr)
		}
	}
	if perr := aptFailure("apt-get", detach.Exit{}, stderr); perr != nil {
		t.Errorf("exit 0: %v", perr)
	}
}

// writeOpOutput frames stdout and stderr into operation id's log, as the
// supervisor does.
func writeOpOutput(t *testing.T, opsDir, id string, stdout, stderr []byte) {
	t.Helper()
	w, err := oplog.Open(filepath.Join(opsDir, id, detach.OutputDir), oplog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(apitypes.StreamStdout, stdout); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(apitypes.StreamStderr, stderr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// Finalize reads apt's stderr from the operation's log, a file: the lock
// text there makes refresh busy, and only stderr counts.
func TestFinalizeReadsLockTextFromTheLog(t *testing.T) {
	ops := t.TempDir()
	p := New(Options{})
	p.UseOpsDir(ops)
	writeOpOutput(t, ops, "busy", capture(t, lockBase, "frontend-timeout.stdout"), capture(t, lockBase, "frontend-timeout.stderr"))
	writeOpOutput(t, ops, "other", capture(t, lockBase, "frontend-timeout.stdout"), capture(t, lockBase, "unrelated.stderr"))
	x := detach.Exit{ExitCode: 100}

	_, err := p.finalizeRefreshBusy(context.Background(), spec.RefreshInput{}, x, provider.NewRun("busy", nil))
	if slugOf(t, err) != problem.PackageManagerBusy {
		t.Errorf("lock text: %v", err)
	}
	_, err = p.finalizeRefreshBusy(context.Background(), spec.RefreshInput{}, x, provider.NewRun("other", nil))
	if slugOf(t, err) != problem.AptFailed {
		t.Errorf("unrelated: %v", err)
	}
	// No log at all is apt-failed, not an error of its own.
	_, err = p.finalizeRefreshBusy(context.Background(), spec.RefreshInput{}, x, provider.NewRun("nolog", nil))
	if slugOf(t, err) != problem.AptFailed {
		t.Errorf("no log: %v", err)
	}
	if out, err := p.finalizeRefreshBusy(context.Background(), spec.RefreshInput{}, detach.Exit{}, provider.NewRun("busy", nil)); err != nil || !out.Changed {
		t.Errorf("exit 0: %+v %v", out, err)
	}
}

func TestMutationArgvAndLane(t *testing.T) {
	argv := mutationArgv(90*time.Second+time.Millisecond, "install", "--", "nxplain")
	want := []string{AptGet, "-y", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold",
		"-o", "DPkg::Lock::Timeout=91", "-o", "APT::Color=0", "install", "--", "nxplain"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %q\nwant %q", argv, want)
	}
	for d, want := range map[time.Duration]int{-time.Second: 1, 0: 1, time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2} {
		if got := lockSeconds(d); got != want {
			t.Errorf("lockSeconds(%v) = %d, want %d", d, got, want)
		}
	}
	type upgradeIn struct{ Names []string }
	l, err := aptLane(context.Background(), upgradeIn{})
	if err != nil || l != (provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}) {
		t.Errorf("lane %+v %v", l, err)
	}
}
