package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
)

// apiFake is an API host for the confirmation tests: it lists one
// container per name, accepts any mutation with an operation that has
// already succeeded, and counts the mutations.
type apiFake struct {
	*httptest.Server
	own        string
	containers []string

	mu    sync.Mutex
	posts []fakePost
	seq   atomic.Int64
}

type fakePost struct {
	path string
	body map[string]any
}

func startAPIFake(t *testing.T, own string, containers ...string) *apiFake {
	t.Helper()
	f := &apiFake{own: own, containers: containers}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.posts = append(f.posts, fakePost{r.URL.Path, body})
			f.mu.Unlock()
			id := fmt.Sprintf("019a0000-0000-7000-8000-%012d", f.seq.Add(1))
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(f.operation(id, apitypes.StatusQueued, nil))
		case strings.HasPrefix(r.URL.Path, "/v1/operations/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/operations/")
			_ = json.NewEncoder(w).Encode(f.operation(id, apitypes.StatusSucceeded, json.RawMessage(`{"changed":true,"whatIf":true}`)))
		case r.URL.Path == "/v1/providers/docker/containers":
			var list apitypes.List[apitypes.Resource[map[string]any]]
			for _, c := range f.containers {
				list.Items = append(list.Items, apitypes.NewResource(own, "docker", "containers", c,
					map[string]any{"update": map[string]any{"status": "updateAvailable"}}))
			}
			_ = json.NewEncoder(w).Encode(list)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *apiFake) operation(id string, st apitypes.OperationStatus, result json.RawMessage) apitypes.Operation {
	return apitypes.Operation{ID: id, Host: f.own, Action: "fake", Provider: "fake", Status: st, Created: time.Now(), Result: result}
}

func (f *apiFake) mutations() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

func (f *apiFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = nil
}

// ttyRun is one CLI run under expect, which gives it a controlling
// terminal: the prompts it answered and the CLI's own exit, stdout and
// stderr, kept apart from the terminal.
type ttyRun struct {
	code           int
	prompts        int
	tty            string // What the terminal showed
	stdout, stderr string
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// expectScript spawns NX_CMD on a new pseudo-terminal, answers NX_ANSWER
// to every [y/N] prompt, and exits with the command's exit status.
const expectScript = `set timeout 30
set prompts 0
spawn -noecho sh -c $env(NX_CMD)
expect {
	-re {\[y/N\] $} { incr prompts; send -- "$env(NX_ANSWER)\r"; exp_continue }
	timeout { puts "\nNX_TIMEOUT"; exit 99 }
	eof
}
set r [wait]
puts "\nNX_PROMPTS=$prompts"
exit [lindex $r 3]
`

// underExpect runs the shell pipeline line, in which $NX writes the CLI,
// under expect, answering answer. stdin, when not empty, is piped to the
// pipeline.
func underExpect(t *testing.T, home, stdin, answer, line string) ttyRun {
	t.Helper()
	expectBin, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	dir := t.TempDir()
	in, out, errf := filepath.Join(dir, "in"), filepath.Join(dir, "out"), filepath.Join(dir, "err")
	if err := os.WriteFile(in, []byte(stdin), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.exp")
	if err := os.WriteFile(script, []byte(expectScript), 0o600); err != nil {
		t.Fatal(err)
	}
	full := fmt.Sprintf("NX=%s; export NX; { %s; } <%s >%s 2>%s", shellQuote(os.Args[0]), line, shellQuote(in), shellQuote(out), shellQuote(errf))
	cmd := exec.Command(expectBin, script)
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "NEXUS_") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") || strings.HasPrefix(e, "HOME=")
	})
	cmd.Env = append(env, asCLIEnv+"=1", "HOME="+home, "NX_CMD="+full, "NX_ANSWER="+answer)
	// expect itself has no terminal; spawn gives the pipeline its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	tty, err := cmd.CombinedOutput()
	var r ttyRun
	r.tty = string(tty)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	m := regexp.MustCompile(`NX_PROMPTS=(\d+)`).FindStringSubmatch(r.tty)
	if m == nil {
		t.Fatalf("expect did not finish: %s", r.tty)
	}
	r.prompts, _ = strconv.Atoi(m[1])
	o, _ := os.ReadFile(out)
	e, _ := os.ReadFile(errf)
	r.stdout, r.stderr = string(o), string(e)
	return r
}

// noTTY runs the CLI in a session of its own, with no controlling
// terminal, stdin from stdin and stdout a pipe.
func noTTY(t *testing.T, home, stdin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := cliProcess(t, home, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errb.String()
}

// cliLine is the shell form of the CLI with args, for underExpect.
func cliLine(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return `"$NX" ` + strings.Join(q, " ")
}

// confirmCase is one command of TC-175 and what it must do.
type confirmCase struct {
	args   []string
	stdin  string
	prompt string // The question asked; empty when none is
	posts  int    // Mutations sent once it proceeds
}

// TC-175: destructive commands (A-12: script invoke and package update
// included) and mutations of two hosts ask once on the controlling
// terminal; n sends nothing, y proceeds; with no controlling terminal they
// need --yes or exit 2 with nothing sent. A one-host mutation, by argument
// or by --stdin, never asks; container remove --stdin asks even for one
// host.
func TestConfirmationRules(t *testing.T) {
	alpha, beta := startAPIFake(t, "srv-a", "web"), startAPIFake(t, "srv-b", "web")
	home := writeClientConfig(t, "alpha", clientHost{"alpha", alpha.URL, "tok", nil}, clientHost{"beta", beta.URL, "tok", nil})
	oneHost := `{"host":"alpha","id":"/providers/docker/containers/web"}` + "\n"
	twoHosts := oneHost + `{"host":"beta","name":"web"}` + "\n"
	cases := []confirmCase{
		{args: []string{"script", "invoke", "deploy-api"}, prompt: "Invoke script deploy-api on alpha? [y/N] ", posts: 1},
		{args: []string{"package", "update"}, prompt: "Update packages on alpha? [y/N] ", posts: 1},
		{args: []string{"container", "remove", "web"}, prompt: "Remove container web on alpha? [y/N] ", posts: 1},
		{args: []string{"container", "update", "web", "--host", "alpha,beta"}, prompt: "Update container web on 2 hosts (alpha, beta)? [y/N] ", posts: 2},
		{args: []string{"container", "update", "--stdin"}, stdin: twoHosts, prompt: "Update 2 containers on 2 hosts (alpha, beta)? [y/N] ", posts: 2},
		{args: []string{"container", "remove", "--stdin"}, stdin: oneHost, prompt: "Remove 1 container on alpha? [y/N] ", posts: 1},
		{args: []string{"container", "update", "web"}, posts: 1},
		{args: []string{"container", "update", "--stdin"}, stdin: oneHost, posts: 1},
	}
	sent := func() int {
		n := len(alpha.mutations()) + len(beta.mutations())
		alpha.reset()
		beta.reset()
		return n
	}
	for _, c := range cases {
		name := strings.Join(c.args, " ")
		t.Run(name, func(t *testing.T) {
			// On a controlling terminal, stdin piped where --stdin reads it.
			for _, answer := range []string{"n", "y"} {
				r := underExpect(t, home, c.stdin, answer, cliLine(append(c.args, "-o", "jsonl")...))
				n := sent()
				switch {
				case c.prompt == "":
					if r.prompts != 0 || r.code != 0 || n != c.posts {
						t.Errorf("terminal: %d prompts, exit %d, %d sent; want no prompt, exit 0, %d sent\n%s%s", r.prompts, r.code, n, c.posts, r.tty, r.stderr)
					}
				case r.prompts != 1 || !strings.Contains(r.tty, c.prompt):
					t.Errorf("answer %s: %d prompts on the terminal %q, want once %q", answer, r.prompts, r.tty, c.prompt)
				case answer == "n" && (r.code != ExitFailure || n != 0):
					t.Errorf("answer n: exit %d, %d sent; want exit 1 and nothing sent: %s", r.code, n, r.stderr)
				case answer == "y" && (r.code != 0 || n != c.posts):
					t.Errorf("answer y: exit %d, %d sent; want exit 0 and %d sent: %s", r.code, n, c.posts, r.stderr)
				}
				if strings.Contains(r.stdout+r.stderr, "[y/N]") {
					t.Errorf("the prompt reached stdout or stderr: %q %q", r.stdout, r.stderr)
				}
				if c.prompt == "" {
					break
				}
			}
			// No controlling terminal: --yes or exit 2 with nothing sent.
			code, _, stderr := noTTY(t, home, c.stdin, append(c.args, "-o", "jsonl")...)
			n := sent()
			if c.prompt == "" {
				if code != 0 || n != c.posts {
					t.Errorf("no terminal: exit %d, %d sent; want 0 and %d: %s", code, n, c.posts, stderr)
				}
				return
			}
			if code != ExitUsage || n != 0 || !strings.Contains(stderr, "--yes") {
				t.Errorf("no terminal without --yes: exit %d, %d sent, stderr %q; want exit 2, nothing sent, naming --yes", code, n, stderr)
			}
			code, _, stderr = noTTY(t, home, c.stdin, append(c.args, "--yes", "-o", "jsonl")...)
			if n := sent(); code != 0 || n != c.posts || strings.Contains(stderr, "[y/N]") {
				t.Errorf("no terminal with --yes: exit %d, %d sent: %s", code, n, stderr)
			}
		})
	}
}

// answeringTerminal is a controlling terminal that answers answer, and records
// when it was opened and what it was asked.
type answeringTerminal struct {
	mu     sync.Mutex
	answer string
	opened int
	asked  bytes.Buffer
}

func (f *answeringTerminal) open() (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return f, nil
}

func (f *answeringTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked.Write(p)
}

func (f *answeringTerminal) ReadLine() (string, error)   { return f.answer, nil }
func (f *answeringTerminal) ReadSecret() (string, error) { return "", io.EOF }
func (f *answeringTerminal) Close() error                { return nil }

func (f *answeringTerminal) state() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.asked.String()
}

// TC-175 step 5: one line written to --stdin and the pipe held open. With
// --yes the request goes before EOF; without it nothing is sent and the
// terminal is not even opened until EOF, when it asks once.
func TestStdinConfirmationWaitsForEOF(t *testing.T) {
	alpha := startAPIFake(t, "srv-a", "web")
	home := writeClientConfig(t, "alpha", clientHost{"alpha", alpha.URL, "tok", nil})
	line := `{"host":"alpha","name":"web"}` + "\n"
	for _, yes := range []bool{true, false} {
		tty := &answeringTerminal{answer: "y"}
		env := homeEnv(home)
		env.OpenTerminal = tty.open
		pr, pw := io.Pipe()
		env.Stdin, env.Stdout, env.Stderr = pr, io.Discard, io.Discard
		args := []string{"container", "remove", "--stdin"}
		if yes {
			args = append(args, "--yes")
		}
		done := make(chan int, 1)
		go func() { done <- runWithSpecs(args, env, fleetSpecs()) }()
		go func() { _, _ = io.WriteString(pw, line) }()
		time.Sleep(500 * time.Millisecond)
		sentBefore := len(alpha.mutations())
		opened, _ := tty.state()
		_ = pw.Close()
		code := <-done
		opened2, asked := tty.state()
		t.Logf("--yes=%v: before EOF %d sent, terminal opened %d times; after EOF exit %d, asked %q", yes, sentBefore, opened, code, asked)
		if yes && (sentBefore != 1 || opened2 != 0) {
			t.Errorf("--yes: %d sent before EOF, terminal opened %d times; want 1 and never", sentBefore, opened2)
		}
		if !yes && (sentBefore != 0 || opened != 0 || opened2 != 1 || asked != "Remove 1 container on alpha? [y/N] ") {
			t.Errorf("without --yes: %d sent and terminal opened %d times before EOF, %d after; asked %q", sentBefore, opened, opened2, asked)
		}
		if code != 0 || len(alpha.mutations()) != 1 {
			t.Errorf("exit %d, %d sent in all, want 0 and 1", code, len(alpha.mutations()))
		}
		alpha.reset()
	}
}

// TC-176: --what-if sends whatIf, prints the preview and needs neither a
// prompt nor --yes, even for destructive package update with no terminal;
// on an op without Op.WhatIf it is a usage error with nothing sent.
func TestWhatIf(t *testing.T) {
	alpha := startAPIFake(t, "srv-a", "web")
	home := writeClientConfig(t, "alpha", clientHost{"alpha", alpha.URL, "tok", nil})
	code, stdout, stderr := noTTY(t, home, "", "package", "update", "--what-if")
	posts := alpha.mutations()
	if code != 0 || len(posts) != 1 || posts[0].path != "/v1/providers/apt/actions/upgrade" || posts[0].body["whatIf"] != true {
		t.Fatalf("package update --what-if: exit %d, sent %+v: %s", code, posts, stderr)
	}
	op := onlyOp(t, stdout)
	if op.Status != apitypes.StatusSucceeded || !strings.Contains(string(op.Result), `"whatIf":true`) {
		t.Errorf("printed %+v, want the preview", op)
	}
	alpha.reset()
	for _, args := range [][]string{
		{"container", "restart", "x", "--what-if"},
		{"container", "restart", "x", "--what-if", "--yes"},
		{"container", "remove", "--stdin", "--what-if", "--yes"},
	} {
		code, _, stderr := noTTY(t, home, `{"name":"x"}`+"\n", args...)
		if code != ExitUsage || !strings.Contains(stderr, "not supported") {
			t.Errorf("%v: exit %d, stderr %q; want 2, unsupported", args, code, stderr)
		}
	}
	if n := len(alpha.mutations()); n != 0 {
		t.Errorf("%d requests sent for an unsupported --what-if", n)
	}
}

// The FR-31 example, verbatim, against two hosts at T0 (TC-170 runs it
// against real nexusd and Docker at T1): on a terminal it asks once and
// updates each host's container; with no terminal it exits 2 and sends
// nothing; with --yes it runs with no prompt.
func TestFR31PipelineConfirmation(t *testing.T) {
	alpha, beta := startAPIFake(t, "srv-a", "web-a"), startAPIFake(t, "srv-b", "web-b")
	home := writeClientConfig(t, "", clientHost{"alpha", alpha.URL, "tok", nil}, clientHost{"beta", beta.URL, "tok", nil})
	pipeline := func(yes string) string {
		return `"$NX" container get --filter status=updateAvailable --host all | "$NX" container update --stdin` + yes
	}
	check := func(what string, wantA, wantB int) {
		t.Helper()
		a, b := alpha.mutations(), beta.mutations()
		if len(a) != wantA || len(b) != wantB {
			t.Errorf("%s: alpha received %v, beta %v; want %d and %d", what, a, b, wantA, wantB)
		}
		if wantA == 1 && (a[0].path != "/v1/providers/docker/containers/web-a/update" || b[0].path != "/v1/providers/docker/containers/web-b/update") {
			t.Errorf("%s: alpha %s, beta %s, want each host's own container", what, a[0].path, b[0].path)
		}
		alpha.reset()
		beta.reset()
	}

	r := underExpect(t, home, "", "y", pipeline(""))
	t.Logf("terminal, answer y: exit %d, prompts %d\nterminal: %q\nstdout:\n%sstderr: %q", r.code, r.prompts, r.tty, r.stdout, r.stderr)
	if r.code != 0 || r.prompts != 1 || !strings.Contains(r.tty, "Update 2 containers on 2 hosts (alpha, beta)? [y/N] ") {
		t.Errorf("terminal: exit %d, %d prompts", r.code, r.prompts)
	}
	// The read prints each host's result as it arrives, so the input, and
	// the output that follows it line by line, can come in either order.
	out := jsonLines(t, "stdout", r.stdout)
	var hosts []string
	for _, o := range out {
		hosts = append(hosts, fmt.Sprint(o["host"]))
		if o["status"] != "Succeeded" {
			t.Errorf("output %v, want the operation waited for", o)
		}
	}
	if slices.Sort(hosts); len(out) != 2 || !slices.Equal(hosts, []string{"alpha", "beta"}) {
		t.Errorf("output %v, want one operation per input line, one labeled alpha and one beta", out)
	}
	check("terminal", 1, 1)

	for _, c := range []struct {
		yes  string
		code int
		n    int
	}{{"", ExitUsage, 0}, {" --yes", 0, 1}} {
		dir := t.TempDir()
		cmd := exec.Command("sh", "-c", pipeline(c.yes)+" >"+shellQuote(filepath.Join(dir, "out")))
		env := slices.DeleteFunc(os.Environ(), func(e string) bool {
			return strings.HasPrefix(e, "NEXUS_") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") || strings.HasPrefix(e, "HOME=")
		})
		cmd.Env = append(env, asCLIEnv+"=1", "HOME="+home, "NX="+os.Args[0])
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		stderr, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		stdout, _ := os.ReadFile(filepath.Join(dir, "out"))
		t.Logf("no terminal%s: exit %d\nstdout:\n%sstderr: %s", c.yes, code, stdout, stderr)
		if code != c.code || strings.Contains(string(stderr), "[y/N]") {
			t.Errorf("no terminal%s: exit %d, want %d", c.yes, code, c.code)
		}
		check("no terminal"+c.yes, c.n, c.n)
	}
}
