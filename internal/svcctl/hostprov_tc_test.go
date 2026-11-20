// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/system-provider/test-cases.md TC-HP-16, TC-HP-17,
// TC-HP-18, TC-HP-19, TC-HP-20, TC-HP-ERR6, TC-HP-ERR7, TC-HP-ERR8 and
// TC-HP-ERR9 (WI 1.4). They are in the internal package because TC-HP-20
// inspects the default runner of a base; F-RUN and the capture helpers are in
// failed_test.go.
//
// The architecture names the listing function Failed, but the package already
// has the State constant Failed, so the function is ListFailed.

package svcctl

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/platform"
)

// The planted unit and service of the captures (testdata/README.md).
const (
	plantedUnit    = "tc-hp-fail.service"
	plantedService = "tc-hp-crash"
)

// TC-HP-16: Failed lists systemd failed units from the captured output.
func TestTCHP16_FailedListsSystemdFailedUnitsFromThe(t *testing.T) {
	c := readCapture(t, "systemd/list-units-failed.txt")
	if !strings.Contains(c.command, plantedUnit) {
		t.Fatalf("README command %q does not name the planted unit %s", c.command, plantedUnit)
	}
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{out: c.out, exit: c.exit})
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if calls := frun.Calls(); len(calls) != 1 || !slices.Equal(calls[0], wantListUnitsArgv) {
		t.Errorf("calls %q, want exactly one %q", calls, wantListUnitsArgv)
	}
	want := wantSystemdNames(c.out)
	if !slices.Equal(got.Names, want) {
		t.Errorf("Names = %q, want %q", got.Names, want)
	}
	if got.Count != len(got.Names) {
		t.Errorf("Count = %d, want len(Names) = %d", got.Count, len(got.Names))
	}
	if !slices.Contains(got.Names, plantedUnit) {
		t.Errorf("Names %q lack the planted %s", got.Names, plantedUnit)
	}
}

// TC-HP-17: Failed reports none from the captured empty systemd output.
func TestTCHP17_FailedReportsNoneFromTheCapturedEmpty(t *testing.T) {
	c := readCapture(t, "systemd/list-units-failed-none.txt")
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{out: c.out, exit: c.exit})
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 || got.Names == nil || len(got.Names) != 0 {
		t.Errorf("got %+v (nil Names %v), want Count 0 and non-nil empty Names", got, got.Names == nil)
	}
}

// TC-HP-18: Failed lists OpenRC crashed services from the captured output.
func TestTCHP18_FailedListsOpenRCCrashedServicesFromThe(t *testing.T) {
	c := readCapture(t, "openrc/rc-status-crashed.txt")
	if !strings.Contains(c.command, plantedService) {
		t.Fatalf("README command %q does not name the planted service %s", c.command, plantedService)
	}
	frun := newFRun(t).answer(wantRCStatusArgv, fAnswer{out: c.out, exit: c.exit})
	got, err := ListFailed(t.Context(), platform.OpenRC, Options{Run: frun.Run, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if calls := frun.Calls(); len(calls) != 1 || !slices.Equal(calls[0], wantRCStatusArgv) {
		t.Errorf("calls %q, want exactly one %q", calls, wantRCStatusArgv)
	}
	want := wantOpenRCNames(c.out)
	if !slices.Equal(got.Names, want) || got.Count != len(want) {
		t.Errorf("got %+v, want Names %q", got, want)
	}
	if !slices.Contains(got.Names, plantedService) {
		t.Errorf("Names %q lack the planted %s", got.Names, plantedService)
	}
}

// TC-HP-19: Failed treats the captured OpenRC "none crashed" result as none.
func TestTCHP19_FailedTreatsTheCapturedOpenRCNoneCrashed(t *testing.T) {
	c := readCapture(t, "openrc/rc-status-crashed-none.txt")
	for name, a := range map[string]fAnswer{
		"captured":        {out: c.out, exit: c.exit},
		"exit 0, empty":   {exit: 0},
		"exit 1, newline": {out: "\n", exit: 1},
	} {
		frun := newFRun(t).answer(wantRCStatusArgv, a)
		got, err := ListFailed(t.Context(), platform.OpenRC, Options{Run: frun.Run, Timeout: time.Second})
		if err != nil || got.Count != 0 || got.Names == nil || len(got.Names) != 0 {
			t.Errorf("%s: %+v, %v; want no error, Count 0, empty Names", name, got, err)
		}
	}
}

// TC-HP-20: Run is the exported real runner and remains the default.
func TestTCHP20_RunIsTheExportedRealRunnerAnd(t *testing.T) {
	for _, init := range []platform.InitSystem{platform.Systemd, platform.OpenRC} {
		c, err := New(init, Options{})
		if err != nil {
			t.Fatal(err)
		}
		var b base
		switch c := c.(type) {
		case *systemd:
			b = c.base
		case *openrc:
			b = c.base
		}
		if reflect.ValueOf(b.run).Pointer() != reflect.ValueOf(Run).Pointer() {
			t.Errorf("%s: the default runner is not Run", init)
		}
	}
	if b := newBase(Options{}); reflect.ValueOf(b.run).Pointer() != reflect.ValueOf(Run).Pointer() {
		t.Error("newBase(Options{}).run is not Run")
	}

	// No identifier realRun remains in the package, tests included.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	old := "real" + "Run"
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == old {
				t.Errorf("%s: identifier %s remains", fset.Position(id.Pos()), old)
			}
			return true
		})
	}

	// TestRealRunnerEnvCapAndTimeout's assertions, against Run itself.
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	envdump := write("envdump.sh", "#!/usr/bin/awk -f\nBEGIN {\n"+
		"\twhile ((getline line < \"-\") > 0) print \"stdin: \" line\n"+
		"\tfor (k in ENVIRON) print k \"=\" ENVIRON[k]\n}\n")
	big := write("big.sh", "#!/bin/sh\nhead -c 204800 /dev/zero | tr '\\0' x\n")
	sleepPID := filepath.Join(dir, "sleep.pid")
	hang := write("hang.sh", "#!/bin/sh\nsleep 60 &\necho $! > "+sleepPID+".tmp\nmv "+sleepPID+".tmp "+sleepPID+"\nwait\n")

	t.Setenv("NEXUS_SVCCTL_LEAK", "must-not-pass")
	res, err := Run(t.Context(), []string{envdump})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("envdump: %v, exit %d, %q", err, res.ExitCode, res.Output)
	}
	lines := strings.Split(strings.TrimRight(string(res.Output), "\n"), "\n")
	slices.Sort(lines)
	if want := []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}; !slices.Equal(lines, want) {
		t.Errorf("envdump printed %q, want exactly %q", lines, want)
	}

	res, err = Run(t.Context(), []string{big})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("big: %v, exit %d", err, res.ExitCode)
	}
	if len(res.Output) != OutputCap || OutputCap != 65536 {
		t.Errorf("big: %d bytes of output, want exactly 65536", len(res.Output))
	}

	const hangTimeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), hangTimeout)
	defer cancel()
	start := time.Now()
	res, err = Run(ctx, []string{hang})
	if el := time.Since(start); el > hangTimeout+2*time.Second {
		t.Errorf("hang returned after %s, want within %s of its timeout", el, 2*time.Second)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hang: err = %v (result %+v), want context.DeadlineExceeded", err, res)
	}
	b, err := os.ReadFile(sleepPID)
	if err != nil {
		t.Fatalf("hang.sh did not start its sleep within %s: %v", hangTimeout, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The orphaned sleep is reaped by init; until then it is a zombie that
	// kill(pid, 0) still finds.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if out, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("sleep (pid %d) is still running after the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := Run(t.Context(), []string{"sh", "-c", "true"}); !errors.Is(err, ErrNotStarted) {
		t.Errorf("a relative program: err = %v, want ErrNotStarted", err)
	}
}

// TC-HP-ERR6: Failed rejects non-zero exits with output.
func TestTCHPERR6_FailedRejectsNonZeroExitsWithOutput(t *testing.T) {
	for _, tc := range []struct {
		init platform.InitSystem
		argv []string
		a    fAnswer
		want string
	}{
		{platform.OpenRC, wantRCStatusArgv, fAnswer{out: "rc-status: some error\n", exit: 1}, "exit 1"},
		{platform.Systemd, wantListUnitsArgv, fAnswer{out: "Failed to connect to bus: No such file or directory\n", exit: 1}, "exit 1"},
		{platform.OpenRC, wantRCStatusArgv, fAnswer{exit: 2}, "exit 2"},
	} {
		frun := newFRun(t).answer(tc.argv, tc.a)
		got, err := ListFailed(t.Context(), tc.init, Options{Run: frun.Run, Timeout: time.Second})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %+v: err = %v, want one naming %q", tc.init, tc.a, err, tc.want)
		}
		if got.Names != nil || got.Count != 0 {
			t.Errorf("%s %+v: got %+v, want no names", tc.init, tc.a, got)
		}
	}
}

// TC-HP-ERR7: Failed drops lines that are not unit or service names.
func TestTCHPERR7_FailedDropsLinesThatAreNotUnit(t *testing.T) {
	sd := readCapture(t, "systemd/list-units-failed.txt")
	junk := "● x.service loaded failed failed X\nWarning: something\na b c\n../../etc.service\n" +
		strings.Repeat("a", 300-len(".service")) + ".service loaded failed failed Y\n"
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{out: sd.out + junk, exit: sd.exit})
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if want := wantSystemdNames(sd.out); !slices.Equal(got.Names, want) || got.Count != len(want) {
		t.Errorf("systemd: got %+v, want only the capture's names %q", got, want)
	}

	rc := readCapture(t, "openrc/rc-status-crashed.txt")
	frun = newFRun(t).answer(wantRCStatusArgv, fAnswer{out: rc.out + " * x\n$(rm -rf /)\n", exit: rc.exit})
	got, err = ListFailed(t.Context(), platform.OpenRC, Options{Run: frun.Run, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if want := wantOpenRCNames(rc.out); !slices.Equal(got.Names, want) || got.Count != len(want) {
		t.Errorf("openrc: got %+v, want only the capture's names %q", got, want)
	}
}

// TC-HP-ERR8: Failed honours its timeout.
func TestTCHPERR8_FailedHonoursItsTimeout(t *testing.T) {
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{hang: true})
	start := time.Now()
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run, Timeout: 200 * time.Millisecond})
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("returned after %s, want within 300ms", el)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v (got %+v), want context.DeadlineExceeded", err, got)
	}
}

// TC-HP-ERR9: Failed on an unknown init system does not run anything.
func TestTCHPERR9_FailedOnAnUnknownInitSystemDoes(t *testing.T) {
	for _, init := range []platform.InitSystem{platform.Unknown, ""} {
		frun := newFRun(t)
		_, err := ListFailed(t.Context(), init, Options{Run: frun.Run, Timeout: time.Second})
		if !errors.Is(err, ErrNotStarted) {
			t.Errorf("%q: err = %v, want ErrNotStarted", init, err)
		}
		if calls := frun.Calls(); len(calls) != 0 {
			t.Errorf("%q: ran %q", init, calls)
		}
	}
}
