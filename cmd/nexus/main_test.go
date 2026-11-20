// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package main

// These tests execute the real nexus binary against a real `nexusd serve`
// (T0, darwin/arm64 on the development Mac), or against a counting TLS
// listener where a test must prove that nothing was sent.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	nexusBin  string
	nexusdBin string
)

// TestMain builds both binaries. NEXUS_TEST_NEXUS_BIN substitutes another
// nexus binary, such as the pre-9.1 stub, to watch these tests fail.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nexus-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	nexusBin = filepath.Join(dir, "nexus")
	nexusdBin = filepath.Join(dir, "nexusd")
	for _, b := range []struct{ out, pkg string }{{nexusBin, "."}, {nexusdBin, "../nexusd"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build", b.pkg+":", err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	if alt := os.Getenv("NEXUS_TEST_NEXUS_BIN"); alt != "" {
		nexusBin = alt
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fingerprintOf is the pinning formula, computed independently of the code
// under test: SHA-256 over the DER, colon-separated upper-case hex.
func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	pairs := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

func freeAddr(t *testing.T, network, host string) string {
	t.Helper()
	ln, err := net.Listen(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", host, err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// daemon is a real `nexusd serve`.
type daemon struct {
	addrs []string
	url   string // https:// (or http:// for tls off) URL of the first address
	token string // A token with no scopes; capabilities needs none
	fp    string // Fingerprint of the served leaf, from its DER; "" for tls off
}

// startNexusd starts nexusd with tls.mode (auto or off) on fresh loopback
// ports, named hostName, with one token minted before it starts.
func startNexusd(t *testing.T, mode, hostName string, listenHosts ...string) *daemon {
	t.Helper()
	if len(listenHosts) == 0 {
		listenHosts = []string{"127.0.0.1"}
	}
	d := &daemon{}
	for _, h := range listenHosts {
		network := "tcp4"
		if strings.Contains(h, ":") {
			network = "tcp6"
		}
		d.addrs = append(d.addrs, freeAddr(t, network, h))
	}
	state := t.TempDir()
	out, err := exec.Command(nexusdBin, "token", "create", "--name", "cli-e2e", "--state-dir", state).Output()
	if err != nil {
		t.Fatalf("nexusd token create: %v", err)
	}
	d.token = strings.TrimSpace(string(out))

	quoted := make([]string, len(d.addrs))
	for i, a := range d.addrs {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	body := fmt.Sprintf("host: {name: %s}\nlisten: [%s]\ntls: {mode: %s}\n", hostName, strings.Join(quoted, ", "), mode)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", state, "--audit-dir", t.TempDir())
	var logs syncBuf
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Logf("nexusd output:\n%s", logs.String())
		}
	})

	scheme := "https"
	if mode == "off" {
		scheme = "http"
	}
	d.url = scheme + "://" + d.addrs[0]
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		if mode == "off" {
			var resp *http.Response
			resp, err = (&http.Client{Timeout: time.Second}).Get(d.url + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				return d
			}
		} else {
			// Test-only capture of the served leaf; the test pins it.
			var conn *tls.Conn
			conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", d.addrs[0], &tls.Config{InsecureSkipVerify: true})
			if err == nil {
				d.fp = fingerprintOf(conn.ConnectionState().PeerCertificates[0].Raw)
				_ = conn.Close()
				return d
			}
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("nexusd exited: %v\n%s", err, logs.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("nexusd not ready within 15s\n%s", logs.String())
	return nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// cliEnv is an isolated environment for nexus: its own HOME, no XDG or
// NEXUS_* variables from the developer's shell.
type cliEnv struct {
	home  string
	extra []string
}

func newCLIEnv(t *testing.T) *cliEnv { return &cliEnv{home: t.TempDir()} }

func (e *cliEnv) environ(extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") ||
			strings.HasPrefix(kv, "NEXUS_") || strings.HasPrefix(kv, "SSL_CERT_")
	})
	env = append(env, "HOME="+e.home)
	env = append(env, e.extra...)
	return append(env, extra...)
}

func (e *cliEnv) configPath() string { return filepath.Join(e.home, ".config", "nexus", "config.yaml") }

type result struct {
	code           int
	stdout, stderr string
}

func (r result) all() string { return r.stdout + r.stderr }

// nexus runs the binary with stdin and extra environment.
func (e *cliEnv) nexus(t *testing.T, stdin string, env []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nexusBin, args...)
	cmd.Env = e.environ(env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout: stdout.String(), stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		t.Fatalf("run nexus %v: %v", args, err)
	}
	return r
}

// hosts reads the saved configuration through `nexus host get -o json`, so
// the token is checked separately from the file.
func (e *cliEnv) hostViews(t *testing.T) map[string]map[string]any {
	t.Helper()
	r := e.nexus(t, "", nil, "host", "get", "-o", "json")
	if r.code != 0 {
		t.Fatalf("host get exited %d: %s", r.code, r.stderr)
	}
	var views []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &views); err != nil {
		t.Fatalf("host get -o json: %v\n%s", err, r.stdout)
	}
	out := map[string]map[string]any{}
	for _, v := range views {
		out[v["name"].(string)] = v
	}
	return out
}

// expectScript drives nexus on a PTY. It answers the confirmation with
// $ANSWER and, after a yes, types $TOKEN at the hidden prompt. It exits with
// nexus's exit code.
const expectScript = `
set timeout 30
log_user 1
spawn -noecho {*}$env(NEXUS_ARGS)
expect {
  -re {\[y/N\] $} {}
  timeout { puts "\nEXPECT: no confirmation prompt"; exit 99 }
  eof { catch wait r; exit [lindex $r 3] }
}
send -- "$env(ANSWER)\r"
if {$env(ANSWER) eq "y"} {
  expect {
    "Token (input hidden): " {}
    timeout { puts "\nEXPECT: no token prompt"; exit 98 }
  }
  send -- "$env(TOKEN)\r"
}
expect eof
catch wait r
exit [lindex $r 3]
`

// onPTY runs nexus under expect with a controlling terminal and returns the
// exit code and everything the terminal showed.
func (e *cliEnv) onPTY(t *testing.T, answer, token string, args ...string) (int, string) {
	t.Helper()
	if _, err := exec.LookPath("expect"); err != nil {
		t.Skip("expect is not installed")
	}
	script := filepath.Join(t.TempDir(), "drive.exp")
	if err := os.WriteFile(script, []byte(expectScript), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "expect", "-f", script)
	quoted := make([]string, 0, len(args)+1)
	for _, a := range append([]string{nexusBin}, args...) {
		quoted = append(quoted, "{"+a+"}")
	}
	cmd.Env = e.environ("NEXUS_ARGS="+strings.Join(quoted, " "), "ANSWER="+answer, "TOKEN="+token)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("expect: %v\n%s", err, out)
	return -1, ""
}

// TC-152: host add pins the fingerprint, by flag and by confirmation on a
// terminal, and a pinned host then works. TC-ERR62 step 2: the token
// prompt does not echo.
func TestHostAddPinsFingerprint(t *testing.T) {
	d := startNexusd(t, "auto", "alpha")
	e := newCLIEnv(t)

	r := e.nexus(t, d.token+"\n", nil, "host", "add", "a", "--url", d.url, "--fingerprint", d.fp, "--token-stdin")
	if r.code != 0 {
		t.Fatalf("step 1: host add a exited %d: %s", r.code, r.all())
	}
	if !strings.Contains(r.stderr, d.fp) {
		t.Errorf("step 1: fingerprint not shown: %s", r.stderr)
	}

	code, screen := e.onPTY(t, "y", d.token, "host", "add", "b", "--url", d.url)
	if code != 0 {
		t.Fatalf("step 2 (y): exit %d\n%s", code, screen)
	}
	if !strings.Contains(screen, d.fp) {
		t.Errorf("step 2: the terminal did not show %s:\n%s", d.fp, screen)
	}
	if strings.Contains(screen, d.token) {
		t.Errorf("step 2: the token was echoed on the terminal:\n%s", screen)
	}

	code, screen = e.onPTY(t, "n", d.token, "host", "add", "c", "--url", d.url)
	if code == 0 {
		t.Fatalf("step 2 (n): exit 0, want failure\n%s", screen)
	}
	if !strings.Contains(screen, d.fp) {
		t.Errorf("step 2 (n): the terminal did not show the same fingerprint:\n%s", screen)
	}

	views := e.hostViews(t)
	for name, want := range map[string]bool{"a": true, "b": true, "c": false} {
		v, ok := views[name]
		if ok != want {
			t.Errorf("host %s saved = %v, want %v", name, ok, want)
			continue
		}
		if ok && (v["fingerprint"] != d.fp || v["trust"] != "pin") {
			t.Errorf("host %s: %v, want pinned to %s", name, v, d.fp)
		}
	}

	for _, h := range []string{"a", "b"} {
		r = e.nexus(t, "", nil, "capability", "get", "--host", h)
		if r.code != 0 {
			t.Fatalf("step 3: capability get --host %s exited %d: %s", h, r.code, r.all())
		}
		// Output carries the client-side name (9.4); the one server, alpha,
		// answers both names over the pinned connection.
		var caps struct{ ID, Host, APIVersion string }
		if err := json.Unmarshal([]byte(r.stdout), &caps); err != nil || caps.Host != h || caps.ID != "/capabilities" {
			t.Errorf("step 3: capabilities %q (%v)", r.stdout, err)
		}
	}
}

// countingTLS is a real TLS listener that counts requests and keeps every
// header line it receives.
type countingTLS struct {
	*httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	headers  strings.Builder
}

func startCountingTLS(t *testing.T) *countingTLS {
	t.Helper()
	c := &countingTLS{}
	c.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		c.mu.Lock()
		_ = r.Header.Write(&c.headers)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"/capabilities","host":"counting","apiVersion":"v1","version":"v0","providers":[]}`)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *countingTLS) saw(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.headers.String(), s)
}

// writeConfig writes the client configuration directly, at mode.
func (e *cliEnv) writeConfig(t *testing.T, body string, mode os.FileMode) {
	t.Helper()
	path := e.configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

const sentinel = "nexus_SENTINEL_e2e_51c0de"

// TC-ERR59: a pin mismatch exits 5 before any request, and the token never
// reaches the server, for a command and for host add.
func TestPinMismatchSendsNothing(t *testing.T) {
	srv := startCountingTLS(t)
	wrong := strings.Repeat("AB:", 31) + "AB"
	e := newCLIEnv(t)
	e.writeConfig(t, fmt.Sprintf("defaultHost: x\nhosts:\n  x:\n    url: %s\n    fingerprint: %s\n    token: %s\n", srv.URL, wrong, sentinel), 0o600)

	r := e.nexus(t, "", nil, "capability", "get")
	if r.code != 5 {
		t.Fatalf("command against a wrong pin exited %d, want 5: %s", r.code, r.all())
	}
	if !strings.Contains(r.stderr, "mismatch") || !strings.Contains(r.stderr, fingerprintOf(srv.Certificate().Raw)) {
		t.Errorf("mismatch message does not name the presented fingerprint: %s", r.stderr)
	}

	r = e.nexus(t, sentinel+"\n", nil, "host", "add", "y", "--url", srv.URL, "--fingerprint", wrong, "--token-stdin")
	if r.code != 5 {
		t.Fatalf("host add with a wrong --fingerprint exited %d, want 5: %s", r.code, r.all())
	}
	if n := srv.requests.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
	if srv.saw(sentinel) {
		t.Error("the sentinel token reached the server")
	}
	if strings.Contains(r.all(), sentinel) {
		t.Error("the sentinel token was printed")
	}
}

// TC-ERR61: a token in a group- or world-readable config is refused with
// exit 3, naming the file and mode, before any request.
func TestReadableConfigTokenRefused(t *testing.T) {
	srv := startCountingTLS(t)
	e := newCLIEnv(t)
	body := fmt.Sprintf("defaultHost: x\nhosts:\n  x:\n    url: %s\n    fingerprint: %s\n    token: %s\n",
		srv.URL, fingerprintOf(srv.Certificate().Raw), sentinel)
	for _, mode := range []os.FileMode{0o640, 0o604} {
		e.writeConfig(t, body, mode)
		r := e.nexus(t, "", nil, "capability", "get")
		if r.code != 3 {
			t.Fatalf("mode %04o: exit %d, want 3: %s", mode, r.code, r.all())
		}
		if !strings.Contains(r.stderr, e.configPath()) || !strings.Contains(r.stderr, fmt.Sprintf("%04o", mode)) {
			t.Errorf("mode %04o: message does not name the file and mode: %s", mode, r.stderr)
		}
		if strings.Contains(r.all(), sentinel) {
			t.Errorf("mode %04o: the token was printed", mode)
		}
	}
	if n := srv.requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests from refused configs", n)
	}
	e.writeConfig(t, body, 0o600)
	if r := e.nexus(t, "", nil, "capability", "get"); r.code != 0 {
		t.Fatalf("mode 0600: exit %d: %s", r.code, r.all())
	}
	if srv.requests.Load() != 1 || !srv.saw("Bearer "+sentinel) {
		t.Error("mode 0600 did not send exactly one authenticated request")
	}
}

// TC-ERR62: no --token flag; the token is not in argv (ps), output, errors
// or verbose logs.
func TestTokensNeverExposed(t *testing.T) {
	d := startNexusd(t, "auto", "alpha")
	e := newCLIEnv(t)

	r := e.nexus(t, "", nil, "host", "add", "x", "--url", d.url, "--fingerprint", d.fp, "--token", sentinel)
	if r.code != 2 {
		t.Fatalf("--token exited %d, want 2: %s", r.code, r.all())
	}
	if strings.Contains(r.all(), sentinel) {
		t.Error("the rejected --token value was echoed")
	}

	// host add waits on stdin for the token; ps shows its arguments then.
	cmd := exec.Command(nexusBin, "host", "add", "p", "--url", d.url, "--fingerprint", d.fp, "--token-stdin")
	cmd.Env = e.environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr syncBuf
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(stderr.String(), "matches --fingerprint") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "matches --fingerprint") {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("host add never reached the token read: %s", stderr.String())
	}
	ps, err := exec.Command("ps", "-ww", "-o", "args=", "-p", fmt.Sprint(cmd.Process.Pid)).Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if !strings.Contains(string(ps), "host add p") {
		t.Fatalf("ps did not show the process: %q", ps)
	}
	if strings.Contains(string(ps), d.token) || strings.Contains(string(ps), sentinel) {
		t.Errorf("ps shows the token: %q", ps)
	}
	if _, err := io.WriteString(stdin, d.token+"\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("host add p: %v\n%s", err, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), d.token) {
		t.Error("host add printed the token")
	}

	checks := []struct {
		name string
		env  []string
		args []string
		code int
	}{
		{"host get -o json", nil, []string{"host", "get", "-o", "json"}, 0},
		{"host get -o yaml", nil, []string{"host", "get", "-o", "yaml"}, 0},
		{"host get table", nil, []string{"host", "get", "-o", "table"}, 0},
		{"verbose run", nil, []string{"capability", "get", "--host", "p", "--verbose"}, 0},
		{"failing command (bad NEXUS_TOKEN)", []string{"NEXUS_TOKEN=" + sentinel}, []string{"capability", "get", "--host", "p", "--verbose"}, 3},
	}
	for _, c := range checks {
		r := e.nexus(t, "", c.env, c.args...)
		if r.code != c.code {
			t.Errorf("%s: exit %d, want %d: %s", c.name, r.code, c.code, r.all())
		}
		if strings.Contains(r.all(), d.token) || strings.Contains(r.all(), sentinel) {
			t.Errorf("%s: output contains the token:\n%s", c.name, r.all())
		}
		if strings.HasPrefix(c.name, "verbose") && !strings.Contains(r.stderr, "> GET") {
			t.Errorf("%s: no request log on stderr: %s", c.name, r.stderr)
		}
	}
	if v := e.hostViews(t)["p"]; v["hasToken"] != true {
		t.Errorf("host get does not report the stored token: %v", v)
	}
}

// TC-154 end to end: XDG_CONFIG_HOME, then HOME, with umask 000.
func TestConfigLocationAndMode(t *testing.T) {
	d := startNexusd(t, "auto", "alpha")
	e := newCLIEnv(t)
	xdg := t.TempDir()
	run := func(env []string) result {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `umask 000; exec "$@"`, "sh", nexusBin,
			"host", "add", "a", "--url", d.url, "--fingerprint", d.fp, "--token-stdin")
		cmd.Env = e.environ(env...)
		cmd.Stdin = strings.NewReader(d.token)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return result{code: ee.ExitCode(), stderr: out.String()}
		} else if err != nil {
			t.Fatal(err)
		}
		return result{stderr: out.String()}
	}
	for _, tc := range []struct {
		name string
		env  []string
		path string
	}{
		{"XDG_CONFIG_HOME", []string{"XDG_CONFIG_HOME=" + xdg}, filepath.Join(xdg, "nexus", "config.yaml")},
		{"HOME", nil, filepath.Join(e.home, ".config", "nexus", "config.yaml")},
	} {
		if r := run(tc.env); r.code != 0 {
			t.Fatalf("%s: host add exited %d: %s", tc.name, r.code, r.stderr)
		}
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %s is %04o under umask 000, want 0600", tc.name, tc.path, fi.Mode().Perm())
		}
	}
}

// TC-155: NEXUS_HOST as a name and as a URL, NEXUS_TOKEN, NEXUS_FINGERPRINT.
func TestEnvironmentOverrides(t *testing.T) {
	a := startNexusd(t, "auto", "alpha")
	b := startNexusd(t, "auto", "beta")
	e := newCLIEnv(t)
	// Saved host A holds a token A rejects, so only NEXUS_TOKEN can make it
	// work; it is pinned, and it is the default.
	e.writeConfig(t, fmt.Sprintf("defaultHost: a\nhosts:\n  a:\n    url: %s\n    fingerprint: %s\n    token: wrong-token\n", a.url, a.fp), 0o600)

	host := func(r result) string {
		var caps struct{ Host string }
		_ = json.Unmarshal([]byte(r.stdout), &caps)
		return caps.Host
	}
	// host is the output label: a configured host's client-side name (9.4),
	// or for a URL target, the host of its URL, which --stdin maps back to
	// that target (CR-32). server is the URL the
	// request went to, from --verbose; exit 0 with a token only that server
	// accepts, over its pin, proves it answered.
	for _, tc := range []struct {
		name   string
		env    []string
		code   int
		host   string
		server string
	}{
		{"saved token is used", []string{"NEXUS_HOST=a"}, 3, "", ""},
		{"NEXUS_HOST=a with NEXUS_TOKEN", []string{"NEXUS_HOST=a", "NEXUS_TOKEN=" + a.token}, 0, "a", a.url},
		{"NEXUS_TOKEN alone, default host", []string{"NEXUS_TOKEN=" + a.token}, 0, "a", a.url},
		{"URL with the right fingerprint", []string{"NEXUS_HOST=" + b.url, "NEXUS_FINGERPRINT=" + b.fp, "NEXUS_TOKEN=" + b.token}, 0, "127.0.0.1", b.url},
		{"URL with a wrong fingerprint", []string{"NEXUS_HOST=" + b.url, "NEXUS_FINGERPRINT=" + a.fp, "NEXUS_TOKEN=" + b.token}, 5, "", ""},
		{"URL without a fingerprint: system trust", []string{"NEXUS_HOST=" + b.url, "NEXUS_TOKEN=" + b.token}, 5, "", ""},
		{"unknown NEXUS_HOST name", []string{"NEXUS_HOST=zz"}, 2, "", ""},
	} {
		r := e.nexus(t, "", tc.env, "capability", "get", "--verbose")
		if r.code != tc.code {
			t.Errorf("%s: exit %d, want %d: %s", tc.name, r.code, tc.code, r.all())
			continue
		}
		if tc.host != "" && host(r) != tc.host {
			t.Errorf("%s: output host %q, want %q", tc.name, host(r), tc.host)
		}
		if tc.server != "" && !strings.Contains(r.stderr, "> GET "+tc.server+"/v1/capabilities") {
			t.Errorf("%s: request did not go to %s: %s", tc.name, tc.server, r.stderr)
		}
	}
}

// TC-E27: plain HTTP is accepted only for loopback IPs, and refused before
// connecting otherwise.
func TestPlainHTTPOnlyLoopback(t *testing.T) {
	// One daemon per address: a single nexusd with two plain-HTTP listeners
	// fails to start (reported separately; not this work item's code).
	d := startNexusd(t, "off", "plain4", "127.0.0.1")
	d6 := startNexusd(t, "off", "plain6", "::1")
	e := newCLIEnv(t)
	for i, srv := range []*daemon{d, d6} {
		name, addr := fmt.Sprintf("lo%d", i), srv.addrs[0]
		r := e.nexus(t, srv.token, nil, "host", "add", name, "--url", "http://"+addr, "--token-stdin")
		if r.code != 0 {
			t.Fatalf("http://%s: exit %d: %s", addr, r.code, r.all())
		}
		if r := e.nexus(t, "", nil, "capability", "get", "--host", name); r.code != 0 {
			t.Fatalf("capability get over http://%s: exit %d: %s", addr, r.code, r.all())
		}
	}
	_, port, _ := net.SplitHostPort(d.addrs[0])
	for _, raw := range []string{"http://192.168.1.2:" + port, "http://nas.lan:" + port} {
		start := time.Now()
		r := e.nexus(t, d.token, nil, "host", "add", "lan", "--url", raw, "--token-stdin")
		if r.code != 2 || !strings.Contains(r.stderr, "refusing plain HTTP") {
			t.Errorf("%s: exit %d, want 2 with a refusal: %s", raw, r.code, r.all())
		}
		if strings.Contains(r.stderr, "fingerprint") || time.Since(start) > 5*time.Second {
			t.Errorf("%s: refusal was not immediate (%v): %s", raw, time.Since(start), r.stderr)
		}
	}
	if _, ok := e.hostViews(t)["lan"]; ok {
		t.Error("a plain-HTTP LAN host was saved")
	}
}

// Exit 4 with the capability message when the host lacks a route (NFR-5)
// is covered against a real internal/server in internal/client; here the
// usage-error paths of the binary itself.
func TestUsageErrors(t *testing.T) {
	e := newCLIEnv(t)
	for _, args := range [][]string{{}, {"--insecure"}, {"host"}, {"host", "frob"}, {"nosuch"}, {"host", "add", "x"}} {
		if r := e.nexus(t, "", nil, args...); r.code != 2 {
			t.Errorf("nexus %v: exit %d, want 2: %s", args, r.code, r.all())
		}
	}
	if r := e.nexus(t, "", nil, "capability", "get"); r.code != 2 || !strings.Contains(r.stderr, "no host selected") {
		t.Errorf("no host: exit %d: %s", r.code, r.all())
	}
}
