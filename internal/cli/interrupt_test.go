// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runSignaled runs the CLI as a real process and sends it SIGINT once at
// least polled distinct operations of px are being polled, which is when
// that many waits are in progress.
func runSignaled(t *testing.T, home string, px *recProxy, stdin string, polled int, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := cliProcess(t, home, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for distinctPolls(px) < polled {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("the CLI polled %d operations, want %d: %s", distinctPolls(px), polled, errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit interface{ ExitCode() int }
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errb.String()
}

func distinctPolls(px *recProxy) int {
	seen := map[string]bool{}
	for _, r := range px.matching(http.MethodGet, func(p string) bool { return strings.HasPrefix(p, "/v1/operations/") }) {
		seen[r.path] = true
	}
	return len(seen)
}

// mutations are the requests that start an operation: every POST but a
// cancel.
func mutations(px *recProxy) int {
	return len(px.matching(http.MethodPost, func(p string) bool { return !strings.HasSuffix(p, "/cancel") }))
}

// CR-1: SIGINT during a multi-host wait detaches the hosts being waited
// for, and sends nothing to the hosts still queued behind the 8-host limit.
func TestInterruptSendsNothingToQueuedHosts(t *testing.T) {
	ge := startGate(t)
	var hosts []clientHost
	for i := range maxConcurrentHosts + 2 {
		hosts = append(hosts, clientHost{fmt.Sprintf("h%02d", i), ge.px.URL, ge.srv.token, nil})
	}
	home := writeClientConfig(t, "", hosts...)

	code, _, stderr := runSignaled(t, home, ge.px, "", maxConcurrentHosts,
		"gadget", "start", "gated", "--host", "all", "--yes", "-o", "jsonl")
	t.Logf("exit %d, stderr:\n%s", code, stderr)
	if code != ExitInterrupted {
		t.Errorf("exit %d, want 130", code)
	}
	if got := mutations(ge.px); got != maxConcurrentHosts {
		t.Errorf("%d mutations sent, want %d: none after SIGINT", got, maxConcurrentHosts)
	}
	if n := strings.Count(stderr, "not sent: interrupted"); n != 2 {
		t.Errorf("%d hosts reported as not sent, want 2: %s", n, stderr)
	}
	if c := ge.px.cancels(); len(c) != 0 {
		t.Errorf("cancel requests %v, want none", c)
	}
}

// CR-36: SIGINT with one host failed and one waiting exits 130, not the
// partial-failure code 6.
func TestInterruptAfterPartialExits130(t *testing.T) {
	ge := startGate(t)
	home := writeClientConfig(t, "",
		clientHost{"alpha", ge.px.URL, ge.srv.token, nil}, clientHost{"down", closedURL(t), "tok", nil})

	code, _, stderr := runSignaled(t, home, ge.px, "", 1, "gadget", "start", "gated", "--host", "alpha,down", "--yes", "-o", "jsonl")
	t.Logf("exit %d, stderr:\n%s", code, stderr)
	if code != ExitInterrupted {
		t.Errorf("exit %d, want 130", code)
	}
	if got := mutations(ge.px); got != 1 {
		t.Errorf("%d mutations sent, want 1", got)
	}
}

// CR-36: with --stdin, SIGINT while a line waits sends no later line.
func TestInterruptStdinSendsNoLaterLine(t *testing.T) {
	ge := startGate(t)
	home := writeClientConfig(t, "alpha", clientHost{"alpha", ge.px.URL, ge.srv.token, nil})
	in := `{"host":"alpha","name":"gated"}` + "\n" + `{"host":"alpha","name":"gated"}` + "\n" + `{"host":"alpha","name":"gated"}` + "\n"

	code, _, stderr := runSignaled(t, home, ge.px, in, 1, "gadget", "start", "--stdin", "--yes", "-o", "jsonl")
	t.Logf("exit %d, stderr:\n%s", code, stderr)
	if code != ExitInterrupted {
		t.Errorf("exit %d, want 130", code)
	}
	if got := mutations(ge.px); got != 1 {
		t.Errorf("%d mutations sent, want 1: no line after SIGINT", got)
	}
}
