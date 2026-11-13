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
