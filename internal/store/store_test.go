package store

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
)

func openTemp(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// fixedClock is a test clock that advances only when told to.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// crashFixture is the record the helper process commits. Every field is set,
// including the internal ones, so the reopen check covers all of them.
func crashFixture() OpRecord {
	return OpRecord{
		Action:        "scripts.run",
		Provider:      "scripts",
		Target:        "/providers/scripts/scripts/deploy-api",
		TokenName:     "ci",
		Result:        json.RawMessage(`{"changed":true}`),
		Error:         problem.New(problem.OutcomeUnknown, "never started"),
		OutputEvicted: true,
		Input:         json.RawMessage(`{"ref":"main"}`),
		HasSensitive:  true,
		Exec:          ExecDetached,
		StartScope:    "scripts:run:deploy-api",
		Lane:          Lane{Key: "script:deploy-api", Policy: LaneReject, Limit: 2},
	}
}

var crashFixtureTime = time.Date(2026, 11, 7, 12, 0, 0, 123456789, time.UTC)

const helperEnv = "NEXUS_STORE_HELPER_DIR"

func TestMain(m *testing.M) {
	code := m.Run()
	if helperBin != "" {
		if err := os.RemoveAll(filepath.Dir(helperBin)); err != nil {
			fmt.Fprintf(os.Stderr, "remove helper build: %v\n", err)
		}
	}
	os.Exit(code)
}

// TestHelperProcess is not a test. It runs only inside a helper process
// started by startHelper: it commits crashFixture, prints the ID and the exact
// stored bytes, then holds the store open until it is killed.
func TestHelperProcess(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	s, err := Open(dir, Options{Now: func() time.Time { return crashFixtureTime }})
	if err != nil {
		fmt.Printf("ERR %v\n", err)
		os.Exit(2)
	}
	res, err := s.CreateWithIdempotency(crashFixture(), IdempotencyKey{TokenID: "tok-1", Key: "k-1", RequestHash: "h-1"})
	if err != nil {
		fmt.Printf("ERR %v\n", err)
		os.Exit(2)
	}
	var raw []byte
	err = s.db.View(func(tx *bolt.Tx) error {
		raw = append([]byte{}, tx.Bucket(bucketOps).Get([]byte(res.ID))...)
		return nil
	})
	if err != nil {
		fmt.Printf("ERR %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("ID %s\nRAW %s\nREADY\n", res.ID, base64.StdEncoding.EncodeToString(raw))
	time.Sleep(10 * time.Minute)
	os.Exit(3)
}

var (
	helperOnce sync.Once
	helperBin  string
	helperErr  error
)

// helperBinary builds this package's test binary with CGO_ENABLED=0, so the
// helper process runs the store exactly as a static release build would.
func helperBinary(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nexus-store-helper-")
		if err != nil {
			helperErr = err
			return
		}
		helperBin = filepath.Join(dir, "store.test")
		cmd := exec.Command("go", "test", "-c", "-o", helperBin, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			helperErr = fmt.Errorf("build helper with CGO_ENABLED=0: %v\n%s", err, out)
		}
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	return helperBin
}

type helper struct {
	cmd *exec.Cmd
	id  string
	raw []byte
}

func startHelper(t *testing.T, dir string) *helper {
	t.Helper()
	bin := helperBinary(t)
	cmd := exec.Command(bin, "-test.run=^TestHelperProcess$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), helperEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	h := &helper{cmd: cmd}
	t.Cleanup(func() {
		if h.cmd.ProcessState == nil {
			if err := h.cmd.Process.Kill(); err != nil {
				t.Logf("kill helper: %v", err)
			}
			if err := h.cmd.Wait(); err != nil {
				t.Logf("helper exit: %v", err)
			}
		}
	})

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("helper exited before READY")
			}
			switch {
			case strings.HasPrefix(line, "ID "):
				h.id = strings.TrimPrefix(line, "ID ")
			case strings.HasPrefix(line, "RAW "):
				h.raw, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "RAW "))
				if err != nil {
					t.Fatalf("helper RAW: %v", err)
				}
			case strings.HasPrefix(line, "ERR "):
				t.Fatalf("helper: %s", line)
			case line == "READY":
				// Drain the rest so the helper never blocks on a full pipe.
				go func() {
					for range lines {
					}
				}()
				return h
			}
		case <-deadline:
			t.Fatal("helper did not become ready in 30s")
		}
	}
}

func (h *helper) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	err := h.cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("helper wait: %v", err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not die from SIGKILL: %v", err)
	}
}

// assertRetained reopens dir and checks the helper's record is intact.
func assertRetained(t *testing.T, dir string, h *helper) {
	t.Helper()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	var raw []byte
	err = s.db.View(func(tx *bolt.Tx) error {
		for cerr := range tx.Check() {
			return fmt.Errorf("consistency check: %v", cerr)
		}
		raw = append([]byte{}, tx.Bucket(bucketOps).Get([]byte(h.id))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, h.raw) {
		t.Fatalf("stored bytes differ after reopen:\n got %s\nwant %s", raw, h.raw)
	}

	got, err := s.Get(h.id)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	want := crashFixture()
	want.ID = h.id
	want.Status = apitypes.StatusQueued
	want.Created = crashFixtureTime
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record after reopen:\n got %+v\nwant %+v", got, want)
	}
	enc, err := encodeRecord(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, h.raw) {
		t.Fatalf("helper wrote %s, fixture encodes as %s", h.raw, enc)
	}

	e, ok, err := s.LookupIdempotency("tok-1", "k-1")
	if err != nil || !ok || e.OperationID != h.id || e.RequestHash != "h-1" {
		t.Fatalf("idempotency entry after reopen: %+v %v %v", e, ok, err)
	}
}

// TC-12: a record committed by a process killed with SIGKILL is intact, byte
// for byte, when the same state.db is reopened. The writer is built with
// CGO_ENABLED=0 and no server process is involved.
func TestCrashRetainsCommittedRecord(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, dir)
	if h.id == "" || len(h.raw) == 0 {
		t.Fatalf("helper did not report its record: id=%q raw=%d bytes", h.id, len(h.raw))
	}
	h.kill(t)
	assertRetained(t, dir, h)

	out, err := exec.Command("go", "version", "-m", helperBinary(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CGO_ENABLED=0") {
		t.Fatalf("helper build settings do not show CGO_ENABLED=0:\n%s", out)
	}
}

// TC-ERR4: while one process holds state.db, a second opener gets ErrLocked
// after 5 to 7 seconds, and the file is unharmed.
func TestSecondOpenerFailsFast(t *testing.T) {
	dir := t.TempDir()
	h := startHelper(t, dir)

	start := time.Now()
	s, err := Open(dir, Options{})
	elapsed := time.Since(start)
	if err == nil {
		if cerr := s.Close(); cerr != nil {
			t.Error(cerr)
		}
		t.Fatal("second Open succeeded while the helper holds the lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("error is not ErrLocked: %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, FileName)) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("lock error does not name the file and the lock: %v", err)
	}
	if elapsed < 5*time.Second || elapsed > 7*time.Second {
		t.Fatalf("second Open took %v, want 5s to 7s", elapsed)
	}
	t.Logf("second Open failed after %v: %v", elapsed, err)

	h.kill(t)
	assertRetained(t, dir, h)
}

// TC-13: state.db has mode 0600 even with umask 000.
func TestStateFileMode(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("state.db mode = %#o, want 0600", got)
	}

	// An existing file with looser bits is tightened on open.
	if err := os.Chmod(filepath.Join(dir, FileName), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("reopened state.db mode = %#o, want 0600", got)
	}
}

// internalMarkers are values only internal fields carry.
var internalMarkers = []string{"INPUT-MARKER-7f3a", "SCOPE-MARKER-91be", "LANE-MARKER-c40d", "inprocess"}

// ToAPI output carries every wire key and no internal field, by key or value.
func TestToAPIHasNoInternalField(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := OpRecord{
		ID: "0190a1b2-0000-7000-8000-000000000001", Action: "docker.containers.restart",
		Provider: "docker", Target: "/providers/docker/containers/web", TokenName: "ci",
		Status: apitypes.StatusRunning, Created: started, Started: &started,
		Input:        json.RawMessage(`{"x":"INPUT-MARKER-7f3a"}`),
		HasSensitive: true,
		Exec:         ExecInProcess,
		StartScope:   "SCOPE-MARKER-91be",
		Lane:         Lane{Key: "LANE-MARKER-c40d", Policy: LaneQueue, Limit: 3},
	}
	b, err := json.Marshal(rec.ToAPI("nas"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	wantKeys := []string{"action", "created", "ended", "error", "host", "id", "outputEvicted", "provider", "result", "started", "status", "target", "tokenName"}
	if !sameSet(keys, wantKeys) {
		t.Fatalf("ToAPI keys = %v, want exactly %v", keys, wantKeys)
	}
	for _, marker := range internalMarkers {
		if bytes.Contains(b, []byte(marker)) {
			t.Fatalf("ToAPI output contains internal value %q: %s", marker, b)
		}
	}
	for _, k := range []string{"input", "hasSensitive", "exec", "startScope", "lane"} {
		if _, ok := m[k]; ok {
			t.Fatalf("ToAPI output has internal key %q", k)
		}
	}

	// OpRecord itself marshals to nothing, so an accidental json.Marshal of a
	// record cannot leak internal fields either.
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("json.Marshal(OpRecord) = %s, want {}", raw)
	}

	// ToAPI does not alias the record's mutable fields.
	op := rec.ToAPI("nas")
	*op.Started = op.Started.Add(time.Hour)
	if !rec.Started.Equal(started) {
		t.Fatal("ToAPI aliases Started")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		if !set[x] {
			return false
		}
	}
	return true
}

func TestKV(t *testing.T) {
	s := openTemp(t, Options{})
	if _, ok, err := s.KVGet("docker-check/web"); err != nil || ok {
		t.Fatalf("absent key: ok=%v err=%v", ok, err)
	}
	if err := s.KVPut("docker-check/web", []byte(`{"status":"upToDate"}`)); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.KVGet("docker-check/web")
	if err != nil || !ok || string(v) != `{"status":"upToDate"}` {
		t.Fatalf("KVGet = %q %v %v", v, ok, err)
	}
	if err := s.KVDelete("docker-check/web"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.KVGet("docker-check/web"); err != nil || ok {
		t.Fatalf("after delete: ok=%v err=%v", ok, err)
	}
	if err := s.KVDelete("never-there"); err != nil {
		t.Fatalf("delete absent key: %v", err)
	}
	if err := s.KVPut("", []byte("x")); err == nil {
		t.Fatal("KVPut accepted an empty key")
	}
}
