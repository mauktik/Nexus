// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// asCLIEnv makes this test binary the nexus CLI with the fleet provider in
// its tree, so a test can run it as a real process in a real pipeline.
const asCLIEnv = "NEXUS_CLI_TEST_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(asCLIEnv) == "1" {
		os.Exit(runWithSpecs(os.Args[1:], OSEnv(), fleetSpecs()))
	}
	os.Exit(m.Run())
}

// cliProcess is this binary run as the CLI, in a session of its own, so it
// has no controlling terminal and /dev/tty cannot be opened.
func cliProcess(t *testing.T, home string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "NEXUS_") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") || strings.HasPrefix(e, "HOME=")
	})
	cmd.Env = append(env, asCLIEnv+"=1", "HOME="+home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// TC-228: `read --filter status=x --host all | mutate --stdin --yes`, two
// real processes in a session without a terminal: exit 0, no prompt, one
// mutation per server for the object the read listed there, and one output
// line per input line labeled per host.
func TestPipelineWithYesAndNoTerminal(t *testing.T) {
	a := startFleet(t, newFleet("srv-a", "a1=x", "a2=y"), false)
	b := startFleet(t, newFleet("srv-b", "b1=y", "b2=x"), false)
	home := writeClientConfig(t, "", clientHost{"alpha", a.url, a.token, nil}, clientHost{"beta", b.url, b.token, nil})

	read := cliProcess(t, home, "thing", "get", "--filter", "status=x", "--host", "all")
	mutate := cliProcess(t, home, "thing", "restart", "--stdin", "--yes")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var tee, readErr, out, mutErr bytes.Buffer
	read.Stdout, read.Stderr = io.MultiWriter(pw, &tee), &readErr
	mutate.Stdin, mutate.Stdout, mutate.Stderr = pr, &out, &mutErr
	if err := mutate.Start(); err != nil {
		t.Fatal(err)
	}
	if err := read.Start(); err != nil {
		t.Fatal(err)
	}
	_ = pr.Close() // The child holds its own copy
	readDone := read.Wait()
	_ = pw.Close()
	mutDone := mutate.Wait()
	if readDone != nil || mutDone != nil {
		t.Fatalf("read: %v, %s\nmutate: %v, %s", readDone, readErr.String(), mutDone, mutErr.String())
	}
	if mutErr.Len() != 0 {
		t.Errorf("mutate wrote to stderr (a prompt?): %q", mutErr.String())
	}

	listed := jsonLines(t, "read", tee.String())
	if len(listed) != 2 {
		t.Fatalf("read printed %v, want a1 on alpha and b2 on beta", listed)
	}
	if got := a.f.admitted(); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("alpha received mutations %v, want [a1]", got)
	}
	if got := b.f.admitted(); !slices.Equal(got, []string{"b2"}) {
		t.Errorf("beta received mutations %v, want [b2]", got)
	}
	ops := jsonLines(t, "mutate", out.String())
	if len(ops) != len(listed) {
		t.Fatalf("mutate printed %d lines for %d input lines: %s", len(ops), len(listed), out.String())
	}
	for i, op := range ops {
		if op["host"] != listed[i]["host"] || op["id"] == "" || op["action"] != fleetRestart.Op().ID {
			t.Errorf("output line %d = %v, want the operation for input %v", i+1, op, listed[i])
		}
	}
}

// syncBuffer is one buffer for stdout and stderr, so their order shows.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TC-ERR65: one output or error per line, in order, exit 6; with
// --stop-on-error, nothing after the first failure.
func TestStdinPerLineFailures(t *testing.T) {
	s := startFleet(t, newFleet("srv-a", "t1=x", "t2=x"), false)
	env := homeEnv(writeClientConfig(t, "", clientHost{"alpha", s.url, s.token, nil}))
	input := strings.Join([]string{
		`{"host":"alpha","id":"/providers/fleet/things/t1"}`, // Valid, by id
		`{"host":"alpha","id":`,                              // Malformed
		`{"host":"nowhere","name":"t1"}`,                     // Unknown host
		`{"host":"alpha","name":"missing"}`,                  // 404
		`{"host":"alpha","name":"t2"}`,                       // Valid, by name
	}, "\n") + "\n"

	run := func(extra ...string) (int, []map[string]any) {
		var both syncBuffer
		e := env
		e.Stdin = strings.NewReader(input)
		e.Stdout, e.Stderr = &both, &both
		code := runWithSpecs(append([]string{"thing", "restart", "--stdin", "--yes", "-o", "jsonl"}, extra...), e, fleetSpecs())
		return code, jsonLines(t, "output", both.String())
	}

	code, lines := run()
	if code != ExitPartial {
		t.Errorf("exit %d, want 6", code)
	}
	want := []struct {
		line  int
		typ   string
		exit  int
		which string
	}{
		{1, "", 0, "t1"},
		{2, "urn:nexus:cli:usage", ExitUsage, ""},
		{3, "urn:nexus:cli:usage", ExitUsage, ""},
		{4, "urn:nexus:problem:not-found", ExitNotFound, ""},
		{5, "", 0, "t2"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d output lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, w := range want {
		l := lines[i]
		if w.typ == "" {
			if l["host"] != "alpha" || l["action"] != fleetRestart.Op().ID || l["level"] != nil {
				t.Errorf("line %d: %v, want the operation on alpha", w.line, l)
			}
			continue
		}
		if l["level"] != "error" || l["type"] != w.typ || l["exit"] != float64(w.exit) || l["line"] != float64(w.line) {
			t.Errorf("line %d: %v, want error %s exit %d", w.line, l, w.typ, w.exit)
		}
	}
	if e := lines[2]; e["host"] != "nowhere" || !strings.Contains(fmt.Sprint(e["message"]), `unknown host "nowhere"`) {
		t.Errorf("unknown host error %v", e)
	}
	if got := s.f.admitted(); !slices.Equal(got, []string{"t1", "t2"}) {
		t.Errorf("admitted %v, want [t1 t2]", got)
	}

	code, lines = run("--stop-on-error")
	if code == 0 {
		t.Error("--stop-on-error exited 0")
	}
	if len(lines) != 2 || lines[1]["line"] != float64(2) {
		t.Errorf("--stop-on-error printed %v, want line 1's operation and line 2's error only", lines)
	}
	if got := s.f.admitted(); !slices.Equal(got, []string{"t1", "t2", "t1"}) {
		t.Errorf("--stop-on-error: admitted %v, want only line 1 more", got)
	}
}

// Without --yes, a mutation reads stdin to EOF before sending anything, so
// confirmation (9.5) sees every host; with --yes, lines run as they come.
func TestStdinReadsToEOFBeforeMutatingWithoutYes(t *testing.T) {
	s := startFleet(t, newFleet("srv-a", "t1=x", "t2=x"), false)
	env := homeEnv(writeClientConfig(t, "alpha", clientHost{"alpha", s.url, s.token, nil}))

	start := func(args ...string) (*io.PipeWriter, chan int) {
		pr, pw := io.Pipe()
		e := env
		e.Stdin, e.Stdout, e.Stderr = pr, io.Discard, io.Discard
		done := make(chan int, 1)
		go func() { done <- runWithSpecs(args, e, fleetSpecs()) }()
		return pw, done
	}
	waitAdmitted := func(n int) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if len(s.f.admitted()) >= n {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}

	// Written aside: a CLI that never reads stdin must not hang the test.
	write := func(pw *io.PipeWriter, s string) { go func() { _, _ = io.WriteString(pw, s) }() }
	pw, done := start("thing", "restart", "--stdin", "--yes")
	write(pw, `{"name":"t1"}`+"\n")
	if !waitAdmitted(1) {
		t.Fatal("with --yes, the first line was not sent before EOF")
	}
	_ = pw.Close()
	if code := <-done; code != 0 {
		t.Fatalf("with --yes: exit %d", code)
	}

	pw, done = start("thing", "restart", "--stdin")
	write(pw, `{"name":"t2"}`+"\n")
	time.Sleep(300 * time.Millisecond)
	if got := s.f.admitted(); len(got) != 1 {
		t.Fatalf("without --yes, %v admitted before EOF", got)
	}
	_ = pw.Close()
	<-done // What follows EOF is confirmation's (9.5)
}

// --stdin binds the positional from the line, so arguments, a multi-host
// selection and a shared Idempotency-Key are usage errors, as is
// --stop-on-error alone; an id of another resource type is a line error.
func TestStdinUsage(t *testing.T) {
	srv := startCaptureServer(t)
	env := homeEnv(writeClientConfig(t, "alpha", clientHost{"alpha", srv.URL, "t", nil}, clientHost{"beta", srv.URL, "t", nil}))
	env.Stdin = strings.NewReader(`{"name":"t1"}` + "\n")
	for _, args := range [][]string{
		{"thing", "restart", "t1", "--stdin", "--yes"},
		{"thing", "restart", "--stdin", "--yes", "--host", "all"},
		{"thing", "restart", "--stdin", "--yes", "--host", "alpha,beta"},
		{"thing", "restart", "--stdin", "--yes", "--idempotency-key", "k"},
		{"thing", "restart", "t1", "--yes", "--stop-on-error"},
	} {
		if r := runSpecsCLI(env, fleetSpecs(), args...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want 2: %s", args, r.code, r.stderr)
		}
	}
	env.Stdin = strings.NewReader(`{"host":"alpha","id":"/providers/docker/containers/t1"}` + "\n" +
		`{"host":"alpha","id":"/providers/fleet/things/t1","name":"t2"}` + "\n" + `{"host":"alpha"}` + "\n")
	r := runSpecsCLI(env, fleetSpecs(), "thing", "restart", "--stdin", "--yes", "-o", "jsonl")
	if errs := jsonLines(t, "stderr", r.stderr); r.code != ExitUsage || len(errs) != 3 {
		t.Errorf("bad bindings: exit %d, errors %v", r.code, errs)
	}
	if reqs := srv.take(); len(reqs) != 0 {
		t.Errorf("%d requests sent, want none", len(reqs))
	}
}
