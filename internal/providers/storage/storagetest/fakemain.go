// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storagetest

// The T1 fake tools' replay entry (storage-provider WI 5.3; test-strategy.md,
// "T1 fake tools"). In the harness container /usr/sbin/zpool and
// /usr/sbin/zfs are two-line sh wrappers that exec the integration test
// binary's TestStorageFakeZFS, which calls Main with the tool's argv. Each
// call is one process: Main loads F-ZFS on the capture, applies the state
// earlier writes left in StateDir/state.json, answers the argv, and saves the
// state again after a write.
//
// Under the shipped unit nexusd's reads run inside its sandbox, where
// ProtectSystem=strict makes StateDir read-only, so only the storage-helper's
// writes, which run outside it, change the state. The A-7 denial is decided
// from the process's own /proc/self/status CapEff, never from a flag. A write
// issued from inside the sandbox cannot append to calls.log: it is logged to
// the reads log instead, marked Write, and F-ZFS answers it from CapEff alone,
// which is OpenZFS's own denial (code review CR-8).
//
// Hang mode: a marker file StateDir/hang-<subcommand> (hang-status for every
// zpool status) makes the call block after it is logged. An empty marker
// sleeps, a process nexusd's SIGKILL ends; a marker holding a path stats that
// path, which the T1 test serves from a FUSE mount that never answers, so the
// process sits in uninterruptible sleep and survives SIGKILL (A-31) until the
// test aborts the FUSE connection.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The files of a MainConfig's StateDir.
const (
	StateFile = "state.json"
	CallsFile = "calls.log"
	lockFile  = "state.lock"
	// HangPrefix + a tool subcommand ("status") is a hang marker.
	HangPrefix = "hang-"
)

// capSysAdminBit is CAP_SYS_ADMIN's bit in a capability set
// (include/uapi/linux/capability.h: #define CAP_SYS_ADMIN 21).
const capSysAdminBit = 21

// Exit codes of a call the fake could not answer as the tool would.
const (
	// ExitFakeError: the fake itself failed (unexpected argv, unreadable
	// capture or state); the message is on stderr.
	ExitFakeError = 125
)

// MainConfig is where the T1 fake finds its capture and keeps its state.
type MainConfig struct {
	// Capture is the capture directory replayed, absolute.
	Capture string
	// StateDir holds state.json, calls.log and the hang markers. Under the
	// shipped unit it is writable only outside nexusd's sandbox.
	StateDir string
	// ReadsLog is the file each read call appends one line to. It must be
	// writable inside the sandbox, so it does not live in StateDir.
	ReadsLog string
}

// ReadLine is one line of the reads log: one per read process start.
type ReadLine struct {
	PID    int      `json:"pid"`
	Argv   []string `json:"argv"`
	CapEff string   `json:"capEff"`
	// Write marks a write argv that could not be logged in calls.log because
	// StateDir is read-only: it was issued from inside the sandbox.
	Write bool      `json:"write,omitempty"`
	Time  time.Time `json:"time"`
}

// CallLine is one line of calls.log: one per write process start, with what
// the process was given.
type CallLine struct {
	PID         int       `json:"pid"`
	PPid        int       `json:"ppid"`
	Argv        []string  `json:"argv"`
	Euid        int       `json:"euid"`
	Env         []string  `json:"env"`
	CapEff      string    `json:"capEff"`
	CapSysAdmin bool      `json:"capSysAdmin"`
	Parents     []Parent  `json:"parents"`
	Time        time.Time `json:"time"`
}

// Parent is one ancestor of a write process, nearest first.
type Parent struct {
	PID     int      `json:"pid"`
	Cmdline []string `json:"cmdline"`
}

// mainState is state.json: what earlier writes changed.
type mainState struct {
	Scans   map[string]json.RawMessage `json:"scans"`
	Created []savedSnap                `json:"created"`
}

type savedSnap struct {
	Name      string `json:"name"`
	CreatedBy string `json:"createdBy"`
	Refer     string `json:"refer"`
	Creation  int64  `json:"creation"`
}

// mainAbort is a Fatalf or Skipf of the fake's TB: Main recovers it.
type mainAbort struct{ msg string }

// mainTB is the TB F-ZFS reports to inside Main. The process's output is
// the tool's, so nothing goes to the test's log: an error or a skip ends
// the call with ExitFakeError and the message on stderr.
type mainTB struct {
	testing.TB
	errs []string
}

func (m *mainTB) Helper()                   {}
func (m *mainTB) Log(...any)                {}
func (m *mainTB) Logf(string, ...any)       {}
func (m *mainTB) Error(a ...any)            { m.errs = append(m.errs, fmt.Sprint(a...)) }
func (m *mainTB) Errorf(f string, a ...any) { m.errs = append(m.errs, fmt.Sprintf(f, a...)) }
func (m *mainTB) Fatal(a ...any)            { panic(mainAbort{fmt.Sprint(a...)}) }
func (m *mainTB) Fatalf(f string, a ...any) { panic(mainAbort{fmt.Sprintf(f, a...)}) }
func (m *mainTB) FailNow()                  { panic(mainAbort{"FailNow"}) }
func (m *mainTB) Skip(a ...any)             { panic(mainAbort{fmt.Sprint(a...)}) }
func (m *mainTB) Skipf(f string, a ...any)  { panic(mainAbort{fmt.Sprintf(f, a...)}) }
func (m *mainTB) SkipNow()                  { panic(mainAbort{"SkipNow"}) }

// Main answers one zpool or zfs call, argv being the tool's whole argv
// (argv[0] the tool's absolute path), writes what the tool would have
// written to stdout and stderr, and returns its exit code. t is the test the
// call runs in; nothing is reported to it.
func Main(t testing.TB, c MainConfig, argv []string, stdout, stderr io.Writer) (code int) {
	tb := &mainTB{TB: t}
	defer func() {
		if r := recover(); r != nil {
			a, ok := r.(mainAbort)
			if !ok {
				a.msg = fmt.Sprintf("panic: %v", r)
			}
			fmt.Fprintf(stderr, "fakezfs: %s\n", a.msg)
			code = ExitFakeError
		}
	}()
	if len(argv) < 2 || !filepath.IsAbs(argv[0]) {
		fmt.Fprintf(stderr, "fakezfs: argv %q: want an absolute tool path and a subcommand\n", argv)
		return ExitFakeError
	}
	capEff, err := ownCapEff()
	if err != nil {
		fmt.Fprintf(stderr, "fakezfs: %v\n", err)
		return ExitFakeError
	}
	capSysAdmin := capEff&(1<<capSysAdminBit) != 0
	capHex := fmt.Sprintf("%016x", capEff)

	write := isWriteArgv(argv)
	stateWritable := true
	if write {
		err := appendLine(filepath.Join(c.StateDir, CallsFile), callLine(argv, capHex, capSysAdmin))
		if errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) {
			// A write from inside nexusd's sandbox, where StateDir is
			// read-only. Log it with the reads and let F-ZFS decide from
			// CapEff: without CAP_SYS_ADMIN it answers OpenZFS's denial.
			stateWritable = false
			err = appendLine(c.ReadsLog, ReadLine{PID: os.Getpid(), Argv: argv, CapEff: capHex, Write: true, Time: time.Now().UTC()})
		}
		if err != nil {
			fmt.Fprintf(stderr, "fakezfs: %v\n", err)
			return ExitFakeError
		}
	} else {
		line := ReadLine{PID: os.Getpid(), Argv: argv, CapEff: capHex, Time: time.Now().UTC()}
		if err := appendLine(c.ReadsLog, line); err != nil {
			fmt.Fprintf(stderr, "fakezfs: %v\n", err)
			return ExitFakeError
		}
	}
	if err := hang(filepath.Join(c.StateDir, HangPrefix+argv[1])); err != nil {
		fmt.Fprintf(stderr, "fakezfs: %v\n", err)
		return ExitFakeError
	}

	if write && stateWritable {
		unlock, err := lockState(c.StateDir)
		if err != nil {
			fmt.Fprintf(stderr, "fakezfs: %v\n", err)
			return ExitFakeError
		}
		defer unlock()
	}
	f := NewFake(tb, c.Capture, Options{CapSysAdmin: capSysAdmin, Vectors: filepath.Join(filepath.Dir(c.Capture), "synthetic")})
	if err := f.loadState(filepath.Join(c.StateDir, StateFile)); err != nil {
		fmt.Fprintf(stderr, "fakezfs: %v\n", err)
		return ExitFakeError
	}
	res, err := f.Run(t.Context(), argv)
	if len(tb.errs) > 0 || err != nil {
		fmt.Fprintf(stderr, "fakezfs: %s\n", strings.Join(append(tb.errs, errText(err)), "; "))
		return ExitFakeError
	}
	if write && res.ExitCode == 0 {
		if err := f.saveState(c.StateDir); err != nil {
			fmt.Fprintf(stderr, "fakezfs: %v\n", err)
			return ExitFakeError
		}
	}
	if _, err := stdout.Write(res.Stdout); err != nil {
		return ExitFakeError
	}
	if _, err := stderr.Write(res.Stderr); err != nil {
		return ExitFakeError
	}
	return res.ExitCode
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isWriteArgv is the fake's own reading of argv: zpool scrub [-s] P or zfs
// snapshot …; F-ZFS classifies it again exactly.
func isWriteArgv(argv []string) bool {
	switch filepath.Base(argv[0]) + " " + argv[1] {
	case "zpool scrub", "zfs snapshot":
		return true
	}
	return false
}

// hang blocks while marker exists (see the file comment); it returns at
// once when it does not.
func hang(marker string) error {
	b, err := os.ReadFile(marker)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if path := strings.TrimSpace(string(b)); path != "" {
		_, err := os.Stat(path)
		return fmt.Errorf("hang on %s ended: %v", path, err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

// ownCapEff is this process's effective capability set.
func ownCapEff() (uint64, error) {
	v, err := statusField(os.Getpid(), "CapEff")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(v, 16, 64)
}

// statusField is a field of /proc/<pid>/status.
func statusField(pid int, field string) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("/proc/%d/status has no %s line", pid, field)
}

// callLine is calls.log's record of this process.
func callLine(argv []string, capHex string, capSysAdmin bool) CallLine {
	l := CallLine{PID: os.Getpid(), PPid: os.Getppid(), Argv: argv, Euid: os.Geteuid(), Env: os.Environ(),
		CapEff: capHex, CapSysAdmin: capSysAdmin, Time: time.Now().UTC()}
	for pid, n := os.Getppid(), 0; pid > 1 && n < 16; n++ {
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		l.Parents = append(l.Parents, Parent{PID: pid, Cmdline: strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")})
		v, err := statusField(pid, "PPid")
		if err != nil {
			break
		}
		if pid, err = strconv.Atoi(v); err != nil {
			break
		}
	}
	return l
}

// appendLine appends v as one JSON line, in one write (O_APPEND).
func appendLine(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, err = fh.Write(append(b, '\n'))
	return errors.Join(err, fh.Close())
}

// lockState takes StateDir's write lock, so concurrent writes change the
// state one after the other.
func lockState(dir string) (func(), error) {
	fh, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(err, fh.Close())
	}
	return func() { _ = fh.Close() }, nil
}

// loadState applies state.json, when present, to f.
func (f *Fake) loadState(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st mainState
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for pool, raw := range st.Scans {
		f.scans[pool] = raw
	}
	for _, s := range st.Created {
		f.snaps[s.Name] = true
		f.created = append(f.created, newSnap{name: s.Name, createdBy: s.CreatedBy, refer: s.Refer, creation: s.Creation})
	}
	return nil
}

// saveState writes f's changes to dir/state.json atomically.
func (f *Fake) saveState(dir string) error {
	f.mu.Lock()
	st := mainState{Scans: f.scans, Created: []savedSnap{}}
	for _, s := range f.created {
		st.Created = append(st.Created, savedSnap{Name: s.name, CreatedBy: s.createdBy, Refer: s.refer, Creation: s.creation})
	}
	b, err := json.Marshal(st)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, StateFile+".*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(b)); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, StateFile))
}

// CapSysAdmin reports whether a CapEff hex word holds CAP_SYS_ADMIN.
func CapSysAdmin(capEffHex string) (bool, error) {
	v, err := strconv.ParseUint(capEffHex, 16, 64)
	if err != nil {
		return false, err
	}
	return v&(1<<capSysAdminBit) != 0, nil
}
