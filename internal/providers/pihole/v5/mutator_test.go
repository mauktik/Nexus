// Security review SEC-3/SEC-4: a setuid-root program that blocks with the
// script as argv[1] (`sudo /opt/pihole/gravity.sh` at its password prompt)
// keeps its caller's real uid (measured: "Uid: 65534 0 0 0"); the real-uid
// check is the guard. comm is defence in depth only: a symlink named bash
// to a setuid binary gives it comm "bash".
//
// Security review SEC-2: a gravity run outside Nexus is a root process whose
// argv[1] is the gravity script. Any local user can start a process whose
// command line only contains the script's path, and Nexus must not wait for
// it (CWE-807): the wait holds the pihole lane for up to 30 minutes.
package v5_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

func TestGravityRunningNeedsARootProcessWithTheScriptAsArgv1(t *testing.T) {
	const script = v5.GravityScript
	cases := []struct {
		name string
		uid  int
		argv []string
		want bool
	}{
		{"root, bash, the script", 0, []string{"/bin/bash", script}, true},
		{"root, with the script's own arguments", 0, []string{"bash", script, "--flag"}, true},
		{"unprivileged lookalike: sleep with the path as an argument", 1000, []string{"sleep", "99999", script}, false},
		{"unprivileged lookalike: the path at argv[2]", 1000, []string{"/bin/sh", "-c", script}, false},
		{"unprivileged process really running the script", 1000, []string{"/bin/bash", script}, false},
		{"root, but the path is not argv[1]", 0, []string{"sleep", "1d", script}, false},
		{"root, argv[1] only starts with the path", 0, []string{"/bin/bash", script + ".d/x"}, false},
		{"root, argv[1] only contains the path", 0, []string{"/bin/bash", "x" + script}, false},
		{"root, the script is argv[0]", 0, []string{script}, false},
		{"root, sh", 0, []string{"/bin/sh", script}, true},
		{"root, dash", 0, []string{"dash", script}, true},
		{"sudo-shaped argv with uid 0 (comm sudo)", 0, []string{"sudo", script}, false},
		{"sudo-shaped argv with the script's arguments, uid 0 (comm sudo)", 0, []string{"/usr/bin/sudo", script, "-g"}, false},
		{"root, su", 0, []string{"su", script}, false},
		{"root, pkexec", 0, []string{"pkexec", script}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := v5test.NewFakeHost(t)
			pid, _ := h.PlantProcess(c.uid, c.argv...)
			got, ok := v5.GravityRunning(h.Paths())
			if ok != c.want || ok && got != pid {
				t.Errorf("GravityRunning = %d, %v; want running=%v (pid %d)", got, ok, c.want, pid)
			}
		})
	}

	t.Run("a setuid program keeps its caller's real uid, even with a spoofed comm", func(t *testing.T) {
		// `ln -s /usr/bin/sudo ~/bash; ~/bash /opt/pihole/gravity.sh` at the
		// password prompt: argv and comm look real, effective uid is 0, the
		// real uid is the caller's (security review SEC-4, measured on T1).
		for _, uids := range []string{"65534\t0\t0\t0", "0\t65534\t65534\t65534"} {
			h := v5test.NewFakeHost(t)
			pid, _ := h.PlantProcessComm(0, "bash", "bash", script)
			status := h.Paths().Host(filepath.Join(v5.ProcDir, strconv.Itoa(pid), "status"))
			if err := os.Remove(status); err != nil { // PlantProcess writes it read-only
				t.Fatal(err)
			}
			if err := os.WriteFile(status, []byte("Name:\tbash\nUid:\t"+uids+"\nGid:\t0\t0\t0\t0\n"), 0o444); err != nil {
				t.Fatal(err)
			}
			if got, ok := v5.GravityRunning(h.Paths()); ok {
				t.Errorf("GravityRunning = %d, true for Uid %q", got, uids)
			}
		}
	})

	t.Run("a setuid program that renamed its argv[0] to a shell is not trusted", func(t *testing.T) {
		// `exec -a bash sudo /opt/pihole/gravity.sh`: argv has the real shape,
		// comm is the executed file's name.
		h := v5test.NewFakeHost(t)
		h.PlantProcessComm(0, "sudo", "bash", script)
		if got, ok := v5.GravityRunning(h.Paths()); ok {
			t.Errorf("GravityRunning = %d, true for a process whose comm is sudo", got)
		}
	})

	t.Run("the script's own name as comm counts, as for a direct shebang exec", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		want, _ := h.PlantProcessComm(0, "gravity.sh", "/bin/bash", script)
		if got, ok := v5.GravityRunning(h.Paths()); !ok || got != want {
			t.Errorf("GravityRunning = %d, %v; want %d", got, ok, want)
		}
	})

	t.Run("a process with no comm file is not trusted", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		pid, _ := h.PlantProcess(0, "/bin/bash", script)
		if err := os.Remove(filepath.Join(h.Root(), v5.ProcDir, strconv.Itoa(pid), "comm")); err != nil {
			t.Fatal(err)
		}
		if got, ok := v5.GravityRunning(h.Paths()); ok {
			t.Errorf("GravityRunning = %d, true without a comm file", got)
		}
	})

	t.Run("a process with no status file is not trusted", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		pid, _ := h.PlantProcess(0, "/bin/bash", script)
		if err := os.Remove(filepath.Join(h.Root(), v5.ProcDir, strconv.Itoa(pid), "status")); err != nil {
			t.Fatal(err)
		}
		if got, ok := v5.GravityRunning(h.Paths()); ok {
			t.Errorf("GravityRunning = %d, true without a status file", got)
		}
	})

	t.Run("a real root run is found beside lookalikes, and the lowest PID wins", func(t *testing.T) {
		h := v5test.NewFakeHost(t)
		h.PlantProcess(1000, "sleep", "99999", script)
		want, _ := h.PlantProcess(0, "/bin/bash", script)
		h.PlantProcess(0, "/bin/bash", script)
		if got, ok := v5.GravityRunning(h.Paths()); !ok || got != want {
			t.Errorf("GravityRunning = %d, %v; want %d", got, ok, want)
		}
	})
}

// The wait for a lookalike ends at once (no 30-minute stall of the lane).
func TestWaitExternalGravityIgnoresALookalike(t *testing.T) {
	h := v5test.NewFakeHost(t)
	h.PlantProcess(1000, "sleep", "99999", v5.GravityScript)
	m := &v5.Mutator{Paths: h.Paths(), Runner: h, Now: h.Now, GravityPoll: time.Minute,
		Sleep: func(_ context.Context, d time.Duration) error {
			t.Error("slept: waited for a lookalike")
			h.Advance(d)
			return nil
		}}
	if err := m.WaitExternalGravity(context.Background(), io.Discard); err != nil {
		t.Fatalf("waited for a lookalike: %v", err)
	}
}
