//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt"
)

// Real lock contention and real interrupted dpkg states (WI 7.2): a real
// dpkg -i holds the frontend lock mid-postinst (the fixture's --gate), and
// a real dpkg --unpack leaves a package unpacked but not configured. The
// tests own the holder process and end it only by opening its gate; they
// never remove a lock file or signal a holder.

// frontendHolder reads the frontend lock's holder with its own F_GETLK on a
// read-only descriptor, independently of the provider's probe: 0 when free.
func frontendHolder(t *testing.T) int {
	t.Helper()
	f, err := os.Open(apt.FrontendLock)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		t.Fatal(err)
	}
	if lk.Type == syscall.F_UNLCK {
		return 0
	}
	return int(lk.Pid)
}

// dpkgHold is a real dpkg -i of a gated fixture package, holding the
// frontend lock in its postinst until released.
type dpkgHold struct {
	t       *testing.T
	pkg     string
	gate    string
	cmd     *exec.Cmd
	out     bytes.Buffer
	pid     int
	release func()
}

// aptHold starts dpkg -i of the gated package pkg and returns once dpkg
// holds the frontend lock. Cleanup releases it and purges pkg.
func aptHold(t *testing.T, pkg string) *dpkgHold {
	t.Helper()
	if pid := frontendHolder(t); pid != 0 {
		t.Fatalf("the frontend lock is already held by %d", pid)
	}
	h := &dpkgHold{t: t, pkg: pkg, gate: fmt.Sprintf("/run/nexus-gate-%s-%d", pkg, time.Now().UnixNano())}
	deb := aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), pkg, "1.0", "--gate", h.gate)
	h.cmd = exec.Command(apt.Dpkg, "-i", deb)
	// dpkg itself needs PATH (lib/dpkg/command.c); apt-get sets it for
	// the dpkg it runs from DPkg::Path.
	h.cmd.Env = append(apt.Env(), "PATH=/usr/sbin:/usr/bin:/sbin:/bin")
	h.cmd.Stdout, h.cmd.Stderr = &h.out, &h.out
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.pid = h.cmd.Process.Pid
	var once sync.Once
	h.release = func() {
		once.Do(func() {
			if err := os.WriteFile(h.gate, nil, 0o644); err != nil {
				t.Error(err)
			}
			if err := h.cmd.Wait(); err != nil {
				t.Errorf("dpkg -i %s: %v\n%s", pkg, err, h.out.String())
			}
			_ = os.Remove(h.gate)
			if out, err := sh(2*time.Minute, apt.Dpkg, "--purge", pkg); err != nil {
				t.Errorf("purge %s: %v\n%s", pkg, err, out)
			}
		})
	}
	t.Cleanup(h.release)
	waitUntil(t, time.Minute, "dpkg holding the frontend lock", func() (bool, string) {
		pid := frontendHolder(t)
		return pid == h.pid, fmt.Sprintf("holder %d, dpkg %d", pid, h.pid)
	})
	return h
}

// running reports whether the holder is still running and holding the lock.
func (h *dpkgHold) running() bool {
	return h.cmd.ProcessState == nil && frontendHolder(h.t) == h.pid
}

// aptUnpack leaves pkg unpacked but not configured, a real interrupted
// transaction, and checks that dpkg --audit reports it. Cleanup purges it.
func aptUnpack(t *testing.T, pkg string) {
	t.Helper()
	mustSh(t, apt.Dpkg, "--unpack", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), pkg, "1.0"))
	t.Cleanup(func() {
		if out, err := sh(2*time.Minute, apt.Dpkg, "--purge", pkg); err != nil {
			t.Errorf("purge %s: %v\n%s", pkg, err, out)
		}
	})
	if audit := mustSh(t, apt.Dpkg, "--audit"); !strings.Contains(audit, pkg) {
		t.Fatalf("dpkg --audit does not report the unpacked %s:\n%s", pkg, audit)
	}
	if st := aptStatus(t, pkg); st != "unpacked" {
		t.Fatalf("%s is %s, want unpacked", pkg, st)
	}
}

// aptStatus is dpkg-query's status word and version of pkg.
func aptStatus(t *testing.T, pkg string) string {
	t.Helper()
	return mustSh(t, apt.DpkgQuery, "-W", "-f=${db:Status-Status}", "--", pkg)
}

func aptVersion(t *testing.T, pkg string) string {
	t.Helper()
	return mustSh(t, apt.DpkgQuery, "-W", "-f=${Version}", "--", pkg)
}

// aptOpDir is the operation's directory of nexusd instance name.
func aptOpDir(name, id string) string {
	return filepath.Join(nexusdState, "nexusd-"+name, "ops", id)
}

// TC-218: refresh takes neither the lock path nor the audit: it succeeds
// while a real dpkg holds the frontend lock and under a real interrupted
// state, with no probe log and no pre.json, and two refreshes still run one
// after the other on the apt lane.
func TestAptRefreshSkipsTheLockPath(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, false)
	const name = "apt-lock-refresh"
	n := aptd(t, name)
	since := time.Now().Add(-time.Second)

	hold := aptHold(t, "nxgate")
	// Control (S-2): apt-get update itself does not wait on the frontend
	// lock.
	if out, err := sh(2*time.Minute, apt.AptGet, "update", "-qq"); err != nil {
		t.Fatalf("apt-get update under the hold: %v\n%s", err, out)
	}
	aptBuild(t, "add", aptRepo, aptPlain, "2.0")

	// Step 1.
	op := aptRefresh(t, n)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("refresh under the hold: %+v", op)
	}
	if !hold.running() {
		t.Fatal("the holder finished before the refresh did; the hold proved nothing")
	}
	t.Logf("refresh %s %s while dpkg %d holds %s", op.ID, op.Status, hold.pid, apt.FrontendLock)
	dir := aptOpDir(name, op.ID)
	if _, err := os.Stat(filepath.Join(dir, detach.ExitFile)); err != nil {
		t.Fatalf("the refresh's directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, apt.PreFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refresh wrote %s: %v", apt.PreFile, err)
	}
	if got := aptPolicy(t, aptPlain)[aptPlain]; got != [2]string{"1.0", "2.0"} {
		t.Errorf("apt-cache policy after the refresh: %q, want the new version", got)
	}
	hold.release()

	// Step 2: a real interrupted transaction.
	aptUnpack(t, "nxhalf")
	op = aptRefresh(t, n)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("refresh under the interrupted state: %+v", op)
	}
	if st := aptStatus(t, "nxhalf"); st != "unpacked" {
		t.Errorf("nxhalf is %s after the refresh; nothing may repair it", st)
	}

	// Step 3: two together.
	ids := make([]string, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			code, b := aptPost(n, refreshAt)
			var o apitypes.Operation
			if err := json.Unmarshal(b, &o); err != nil || code != http.StatusAccepted {
				t.Errorf("refresh %d: %d %s", i, code, b)
				return
			}
			ids[i] = o.ID
		})
	}
	wg.Wait()
	var exits []detach.Exit
	for _, id := range ids {
		if id == "" {
			t.FailNow()
		}
		if o := n.waitOp(id, 3*time.Minute); o.Status != apitypes.StatusSucceeded {
			t.Fatalf("refresh %s: %+v", id, o)
		}
		x, ok, err := detach.ReadExit(aptOpDir(name, id))
		if err != nil || !ok {
			t.Fatalf("exit.json of %s: %v %v", id, ok, err)
		}
		exits = append(exits, x)
	}
	a, b := exits[0], exits[1]
	t.Logf("refresh %s ran %s..%s, %s ran %s..%s", ids[0], a.Started.Format(time.StampMicro), a.Ended.Format(time.StampMicro),
		ids[1], b.Started.Format(time.StampMicro), b.Ended.Format(time.StampMicro))
	if a.Started.Before(b.Ended) && b.Started.Before(a.Ended) {
		t.Errorf("the two refreshes overlapped: they are not on one lane")
	}

	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "--no-pager", "-o", "cat", "--since", since.Format("2006-01-02 15:04:05"))
	if strings.Contains(journal, "dpkg frontend lock") || strings.Contains(journal, "dpkg --audit") {
		t.Errorf("nexusd probed or audited for a refresh:\n%s", journal)
	}
}

// lockedBuffer is a log sink the provider and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// aptSeam is the apt provider driven through PrepareMutation and
// FinalizeMutation, the seam upgrade, install and remove bind to (7.3,
// 7.4), with its units started by the real launcher under systemd.
type aptSeam struct {
	t      *testing.T
	p      *apt.Provider
	log    *lockedBuffer
	ops    string
	launch *detach.Launcher
	seq    int
}

func newAptSeam(t *testing.T, lockWait time.Duration) *aptSeam {
	t.Helper()
	s := &aptSeam{t: t, log: &lockedBuffer{}, ops: filepath.Join(nexusdState, "apt-seam-"+fmt.Sprint(time.Now().UnixNano()))}
	if err := os.MkdirAll(s.ops, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(s.ops) })
	s.p = apt.New(apt.Options{Host: "harness", Logger: slog.New(slog.NewTextHandler(s.log, nil))})
	if err := s.p.Configure(func(into any) error {
		c := into.(*config.AptConfig)
		c.Enabled, c.LockWait = true, lockWait
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.p.UseOpsDir(s.ops)
	l, err := detach.New(detach.Config{OpsDir: s.ops, Executable: nexusdBin}, detach.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.launch = l
	return s
}

func (s *aptSeam) run() *provider.Run {
	s.seq++
	return provider.NewRun(fmt.Sprintf("t1-apt-%d-%d", time.Now().UnixNano(), s.seq), nil)
}

func (s *aptSeam) hasPre(r *provider.Run) bool {
	_, err := os.Stat(filepath.Join(s.ops, r.ID(), apt.PreFile))
	return err == nil
}

// execute launches spec in a real transient unit and returns its exit.json.
func (s *aptSeam) execute(r *provider.Run, spec detach.Spec) detach.Exit {
	s.t.Helper()
	if err := s.launch.Start(context.Background(), r.ID(), spec); err != nil {
		s.t.Fatal(err)
	}
	var x detach.Exit
	waitUntil(s.t, 3*time.Minute, "exit.json of "+r.ID(), func() (bool, string) {
		got, ok, err := detach.ReadExit(filepath.Join(s.ops, r.ID()))
		if err != nil {
			s.t.Fatal(err)
		}
		x = got
		return ok, "running"
	})
	return x
}

// The mutation path against real dpkg: Prepare waits for a real holder and
// logs its PID, ends busy at lockWait and on cancel without a pre.json,
// reports a real interrupted state broken without repairing it, and apt's
// own DPkg::Lock::Timeout makes a lock taken after the probe busy too.
// Then a real run's Finalize matches dpkg-query and keeps the conffile.
func TestAptMutationLockAndAuditAgainstRealDpkg(t *testing.T) {
	requireInit(t, platform.Systemd)
	aptFixture(t, true)
	// nxconf 1.0 installed with its conffile edited locally, 2.0 available.
	mustSh(t, apt.Dpkg, "-i", aptBuild(t, "deb", filepath.Join(t.TempDir(), "debs"), "nxconf", "1.0", "--conffile"))
	t.Cleanup(func() { _, _ = sh(2*time.Minute, apt.Dpkg, "--purge", "nxconf") })
	if err := os.WriteFile("/etc/nxconf.conf", []byte("nxconf local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	aptBuild(t, "add", aptRepo, "nxconf", "2.0", "--conffile")
	mustSh(t, apt.AptGet, "update", "-qq")
	args := []string{"install", "--", aptPlain, "nxconf"}

	t.Run("waits for a real holder", func(t *testing.T) {
		s := newAptSeam(t, 2*time.Minute)
		hold := aptHold(t, "nxgate")
		r := s.run()
		type prepared struct {
			spec detach.Spec
			err  error
		}
		done := make(chan prepared, 1)
		go func() {
			spec, err := s.p.PrepareMutation(context.Background(), r, args)
			done <- prepared{spec, err}
		}()
		time.Sleep(3 * time.Second)
		select {
		case p := <-done:
			t.Fatalf("Prepare returned while dpkg held the lock: %+v", p)
		default:
		}
		if want := fmt.Sprintf("holderPid=%d", hold.pid); !strings.Contains(s.log.String(), want) {
			t.Errorf("the log does not name holder %d:\n%s", hold.pid, s.log.String())
		}
		if s.hasPre(r) || !hold.running() {
			t.Fatalf("pre.json %v, holder running %v during the wait", s.hasPre(r), hold.running())
		}
		t.Logf("still waiting after 3s on dpkg %d; log: %s", hold.pid, strings.TrimSpace(s.log.String()))
		hold.release()
		select {
		case p := <-done:
			if p.err != nil {
				t.Fatal(p.err)
			}
			if !s.hasPre(r) || !slices.Contains(p.spec.Argv, "install") {
				t.Errorf("after the release: pre.json %v, argv %q", s.hasPre(r), p.spec.Argv)
			}
		case <-time.After(time.Minute):
			t.Fatal("Prepare did not continue once the lock was free")
		}
	})

	t.Run("busy at lockWait", func(t *testing.T) {
		s := newAptSeam(t, 2*time.Second)
		hold := aptHold(t, "nxgate")
		r := s.run()
		start := time.Now()
		_, err := s.p.PrepareMutation(context.Background(), r, args)
		var perr *problem.Error
		if !errors.As(err, &perr) || perr.Slug() != problem.PackageManagerBusy || !strings.Contains(err.Error(), fmt.Sprint(hold.pid)) {
			t.Fatalf("%v, want package-manager-busy naming %d", err, hold.pid)
		}
		if d := time.Since(start); d < 2*time.Second {
			t.Errorf("busy after %v, before lockWait", d)
		}
		t.Logf("after %v: %v", time.Since(start).Round(time.Millisecond), err)
		if s.hasPre(r) || !hold.running() {
			t.Errorf("pre.json %v, holder running %v", s.hasPre(r), hold.running())
		}
	})

	t.Run("cancel during the wait", func(t *testing.T) {
		s := newAptSeam(t, 2*time.Minute)
		hold := aptHold(t, "nxgate")
		r := s.run()
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Second, cancel)
		_, err := s.p.PrepareMutation(ctx, r, args)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v, want context.Canceled", err)
		}
		t.Logf("canceled: %v", err)
		if s.hasPre(r) || !hold.running() {
			t.Errorf("pre.json %v, holder running %v", s.hasPre(r), hold.running())
		}
	})

	t.Run("interrupted state is broken and not repaired", func(t *testing.T) {
		s := newAptSeam(t, 2*time.Minute)
		aptUnpack(t, "nxhalf")
		r := s.run()
		_, err := s.p.PrepareMutation(context.Background(), r, args)
		var perr *problem.Error
		if !errors.As(err, &perr) || perr.Slug() != problem.PackageManagerBroken {
			t.Fatalf("%v, want package-manager-broken", err)
		}
		t.Logf("broken: %v", err)
		if st := aptStatus(t, "nxhalf"); st != "unpacked" || s.hasPre(r) {
			t.Errorf("nxhalf %s after Prepare (want unpacked: no repair), pre.json %v", st, s.hasPre(r))
		}
	})

	t.Run("lock taken after the probe is busy through apt", func(t *testing.T) {
		s := newAptSeam(t, 3*time.Second)
		r := s.run()
		spec, err := s.p.PrepareMutation(context.Background(), r, args)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(spec.Argv, "DPkg::Lock::Timeout=3") || spec.KillMode != detach.KillProcess {
			t.Fatalf("spec %+v", spec)
		}
		hold := aptHold(t, "nxgate")
		x := s.execute(r, spec)
		out, err := s.p.FinalizeMutation(r, x)
		var perr *problem.Error
		if !errors.As(err, &perr) || perr.Slug() != problem.PackageManagerBusy || out.ExitCode != 100 {
			t.Fatalf("exit %+v: %+v %v, want package-manager-busy with exit 100", x, out, err)
		}
		t.Logf("argv %q; exit %+v; %v", spec.Argv, x, err)
		if d := x.Ended.Sub(x.Started); d < 2*time.Second {
			t.Errorf("apt gave up after %v, before DPkg::Lock::Timeout", d)
		}
		if len(out.Changes) != 0 || aptVersion(t, aptPlain) != "1.0" || !hold.running() {
			t.Errorf("changes %+v, nxplain %s, holder running %v", out.Changes, aptVersion(t, aptPlain), hold.running())
		}
	})

	t.Run("a real run finalizes from files", func(t *testing.T) {
		s := newAptSeam(t, 2*time.Minute)
		r := s.run()
		spec, err := s.p.PrepareMutation(context.Background(), r, args)
		if err != nil {
			t.Fatal(err)
		}
		x := s.execute(r, spec)
		out, err := s.p.FinalizeMutation(r, x)
		if err != nil || x.ExitCode != 0 {
			t.Fatalf("exit %+v: %v", x, err)
		}
		// Independent readback: dpkg-query and the conffile on disk.
		if aptVersion(t, aptPlain) != "2.0" || aptVersion(t, "nxconf") != "2.0" {
			t.Fatalf("dpkg-query: nxplain %s nxconf %s", aptVersion(t, aptPlain), aptVersion(t, "nxconf"))
		}
		var got []string
		for _, c := range out.Changes {
			got = append(got, fmt.Sprintf("%s %s>%s", c.Package, c.OldVersion, c.NewVersion))
		}
		if want := []string{"nxconf 1.0>2.0", "nxplain 1.0>2.0"}; !slices.Equal(got, want) {
			t.Errorf("changes %q, want %q", got, want)
		}
		t.Logf("exit %d; outcome %+v", x.ExitCode, out)
		conf, err := os.ReadFile("/etc/nxconf.conf")
		if err != nil || string(conf) != "nxconf local\n" {
			t.Errorf("/etc/nxconf.conf: %q %v, want the local version kept", conf, err)
		}
		if !slices.Equal(out.Conffiles, []string{"/etc/nxconf.conf"}) || !out.ConffilesComplete || out.ExitCode != 0 {
			t.Errorf("outcome %+v", out)
		}
		// After a restart: a new provider over the same files agrees.
		again := apt.New(apt.Options{Host: "harness"})
		again.UseOpsDir(s.ops)
		if out2, err := again.FinalizeMutation(r, x); err != nil || fmt.Sprint(out2) != fmt.Sprint(out) {
			t.Errorf("after restart: %+v %v\nwant %+v", out2, err, out)
		}
	})
}
