package cli

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/clientconfig"
)

// CR-32: the output of a NEXUS_HOST URL target is labeled with the name
// --stdin resolves back to that URL target, not the name the server gives
// itself: a configured host with that name must not receive the mutation
// and the URL target's token.
func TestURLTargetOutputBindsBackThroughStdin(t *testing.T) {
	s := startFleet(t, newFleet("server-own-name", "w1=x"), false)
	other := startFleet(t, newFleet("server-own-name", "w1=x"), false)
	home := writeClientConfig(t, "", clientHost{"server-own-name", other.url, other.token, nil})
	env := homeEnv(home)
	base := env.Getenv
	env.Getenv = func(k string) string {
		switch k {
		case clientconfig.EnvHost:
			return s.url
		case clientconfig.EnvToken:
			return s.token
		}
		return base(k)
	}

	r := runSpecsCLI(env, fleetSpecs(), "thing", "get", "-o", "jsonl")
	objs := jsonLines(t, "read", r.stdout)
	if r.code != 0 || len(objs) != 1 {
		t.Fatalf("read: exit %d, objects %v: %s", r.code, objs, r.stderr)
	}
	if h := objs[0]["host"]; h == "server-own-name" {
		t.Fatalf("host %q is the server's own name, which names a configured host", h)
	}

	env.Stdin = strings.NewReader(r.stdout)
	r = runSpecsCLI(env, fleetSpecs(), "thing", "restart", "--stdin", "--yes", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("restart --stdin: exit %d: %s", r.code, r.stderr)
	}
	if got := s.f.admitted(); !slices.Equal(got, []string{"w1"}) {
		t.Errorf("the URL target admitted %v, want [w1]", got)
	}
	if got := other.f.admitted(); len(got) != 0 {
		t.Errorf("the configured host of the same name admitted %v, want nothing", got)
	}
}

// CR-33: --follow over more than 8 hosts starts every host's stream; a
// followed stream holds its slot until it ends, so the limit must not
// apply to it.
func TestFollowAcrossMoreThanEightHosts(t *testing.T) {
	ge := startGate(t)
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "gated", "--no-wait", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("start: exit %d: %s", r.code, r.stderr)
	}
	id := onlyOp(t, r.stdout).ID
	n := maxConcurrentHosts + 1
	var hosts []clientHost
	for i := range n {
		hosts = append(hosts, clientHost{fmt.Sprintf("h%02d", i), ge.px.URL, ge.srv.token, nil})
	}
	env := homeEnv(writeClientConfig(t, "", hosts...))

	done := make(chan runResult, 1)
	go func() {
		done <- runSpecsCLI(env, fleetSpecs(), "operationlog", "get", id, "--follow", "--host", "all", "-o", "jsonl")
	}()
	isLogs := func(p string) bool { return strings.HasSuffix(p, "/logs") }
	deadline := time.Now().Add(5 * time.Second)
	for len(ge.px.matching(http.MethodGet, isLogs)) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(ge.px.matching(http.MethodGet, isLogs)); got != n {
		t.Errorf("%d of %d hosts' streams opened while all follow", got, n)
	}
	ge.g.open()
	select {
	case res := <-done:
		if res.code != 0 {
			t.Errorf("exit %d: %s", res.code, res.stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the follow did not end after the operation did")
	}
}

// CR-34: a stream from several hosts in the table format labels each line
// with its host; from one host it does not.
func TestMultiHostStreamTableLabelsHosts(t *testing.T) {
	ge := startGate(t)
	g := ge.g
	g.counter = 5
	r := runSpecsCLI(ge.env, fleetSpecs(), "gadget", "start", "counter", "--no-wait", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("start: exit %d: %s", r.code, r.stderr)
	}
	id := onlyOp(t, r.stdout).ID
	env := homeEnv(writeClientConfig(t, "",
		clientHost{"alpha", ge.px.URL, ge.srv.token, nil}, clientHost{"beta", ge.px.URL, ge.srv.token, nil}))

	r = runSpecsCLI(env, fleetSpecs(), "operationlog", "get", id, "--follow", "--host", "alpha,beta", "-o", "table")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	count := map[string]int{}
	for line := range strings.Lines(r.stdout) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "[alpha] "):
			count["alpha"]++
		case strings.HasPrefix(line, "[beta] "):
			count["beta"]++
		default:
			t.Errorf("line %q has no host label", line)
		}
	}
	if count["alpha"] != 5 || count["beta"] != 5 {
		t.Errorf("lines per host %v, want 5 each: %q", count, r.stdout)
	}

	r = runSpecsCLI(env, fleetSpecs(), "operationlog", "get", id, "--follow", "--host", "alpha", "-o", "table")
	if want := "1\n2\n3\n4\n5\n"; r.stdout != want {
		t.Errorf("one host printed %q, want %q", r.stdout, want)
	}
}

// CR-35: a live container-log stream whose frames are stamped slightly out
// of order loses none; only the window a reconnect repeats is dropped.
func TestResumeKeepsOutOfOrderLiveFrames(t *testing.T) {
	frame := func(ts string) map[string]any { return map[string]any{"t": ts, "data": "x"} }
	const (
		t1 = "2026-01-01T00:00:01Z"
		t2 = "2026-01-01T00:00:02Z"
		t3 = "2026-01-01T00:00:03Z"
	)
	r := &resume{lastO: -1}
	r.reopen() // The first stream
	for _, ts := range []string{t2, t1} {
		if !r.next(frame(ts)) {
			t.Errorf("live frame at %s dropped on the first stream", ts)
		}
	}
	if r.lastT != t2 {
		t.Errorf("lastT %s after an earlier frame, want %s", r.lastT, t2)
	}

	r.reopen() // Reconnect after the frame at t2: the stream repeats from there
	if r.next(frame(t2)) {
		t.Errorf("the repeated frame at %s was delivered again", t2)
	}
	if r.next(frame(t1)) {
		t.Errorf("a repeated frame before lastT was delivered again")
	}
	if !r.next(frame(t3)) {
		t.Errorf("the new frame at %s was dropped", t3)
	}
	if !r.next(frame(t2)) {
		t.Errorf("a live frame out of order after the replay was dropped")
	}
}

// CR-83: the --stdin help does not repeat itself when the positional is id.
func TestStdinHelpDoesNotRepeatID(t *testing.T) {
	r := runSpecsCLI(Env{Getenv: func(string) string { return "" }}, fleetSpecs(), "operation", "get", "--help")
	if strings.Contains(r.stdout+r.stderr, "id or id") {
		t.Errorf("help repeats itself:\n%s%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "binds its host and its id; a line") {
		t.Errorf("help does not say each line binds its host and its id:\n%s", r.stdout)
	}
	r = runSpecsCLI(Env{Getenv: func(string) string { return "" }}, fleetSpecs(), "thing", "restart", "--help")
	if !strings.Contains(r.stdout, "binds its host and its id or name;") {
		t.Errorf("help does not say each line binds its host and its id or name:\n%s", r.stdout)
	}
}
