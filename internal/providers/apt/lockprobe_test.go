package apt

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// lockHolderEnv makes the test binary a lock holder: it takes a write lock
// on the whole file named in the variable, as dpkg and apt do, prints
// "locked", and holds it until its stdin closes.
const lockHolderEnv = "NEXUS_APT_TEST_LOCK_HOLDER"

func init() {
	path := os.Getenv(lockHolderEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		os.Exit(2)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// holdLock starts a separate process, owned by the test, that holds a
// write lock on path. release closes its stdin, so it exits by itself: the
// test never signals it.
func holdLock(t *testing.T, path string) (pid int, release func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		_ = stdin.Close()
		t.Fatalf("lock holder: %q %v, exit %v", line, err, cmd.Wait())
	}
	done := false
	release = func() {
		if done {
			return
		}
		done = true
		if err := stdin.Close(); err != nil {
			t.Error(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Errorf("lock holder: %v", err)
		}
	}
	t.Cleanup(release)
	return cmd.Process.Pid, release
}

// The probe sees a real fcntl lock of another process, with its PID, from
// a read-only descriptor, takes nothing, and sees it free once released.
func TestProbeLockSeesAnotherProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-frontend")
	if st, err := probeLock(path); err != nil || st.Held {
		t.Fatalf("missing file: %+v %v, want free", st, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the probe created the lock file: %v", err)
	}

	pid, release := holdLock(t, path)
	// Read-only for everyone: a probe that opened it for writing would fail
	// as a non-root user, and root's success proves nothing here.
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		st, err := probeLock(path)
		if err != nil || !st.Held || st.PID != pid {
			t.Fatalf("held by %d: %+v %v", pid, st, err)
		}
	}
	// Probing twice left the holder's lock in place: the holder still has
	// it, so another process still sees it.
	release()
	if st, err := probeLock(path); err != nil || st.Held {
		t.Fatalf("released: %+v %v, want free", st, err)
	}
}
