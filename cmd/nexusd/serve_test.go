package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
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
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// serveProc is a real `nexusd serve` process.
type serveProc struct {
	cmd    *exec.Cmd
	addr   string
	mu     sync.Mutex
	stdout bytes.Buffer
	done   chan error
}

func (p *serveProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.Write(b)
}

func (p *serveProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.String()
}

// startServe starts nexusd serve with env replacing NOTIFY_SOCKET.
func startServe(t *testing.T, notifySocket string) *serveProc {
	t.Helper()
	addr := freeAddr(t)
	cfg := writeConfig(t, "nexusd.yaml", "listen: [\""+addr+"\"]\ntls: {mode: off}\n")
	// serve opens state.db in its state directory at startup (3.2).
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", t.TempDir(), "--audit-dir", t.TempDir())
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "NOTIFY_SOCKET=") })
	if notifySocket != "" {
		cmd.Env = append(cmd.Env, "NOTIFY_SOCKET="+notifySocket)
	}
	p := &serveProc{cmd: cmd, addr: addr, done: make(chan error, 1)}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = p, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-p.done
		}
		if stderr.Len() > 0 {
			t.Logf("nexusd stderr:\n%s", stderr.String())
		}
	})
	return p
}

func (p *serveProc) waitHealthy(t *testing.T) {
	t.Helper()
	c := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get("http://" + p.addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case err := <-p.done:
			p.done <- err
			t.Fatalf("nexusd exited before becoming healthy: %v\n%s", err, p.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("nexusd not healthy within 15s\n%s", p.output())
}

// terminate sends SIGTERM and returns the exit code and how long exit took.
func (p *serveProc) terminate(t *testing.T) (int, time.Duration) {
	t.Helper()
	start := time.Now()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		elapsed := time.Since(start)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), elapsed
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, elapsed
	case <-time.After(20 * time.Second):
		t.Fatalf("nexusd still running 20s after SIGTERM")
		return -1, 0
	}
}

// TC-20: readiness notification and bounded shutdown.
func TestServeReadinessAndShutdown(t *testing.T) {
	t.Run("with NOTIFY_SOCKET", func(t *testing.T) {
		// Unix socket paths are short; t.TempDir under /var/folders is too long.
		dir, err := os.MkdirTemp("/tmp", "nx")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "notify.sock")
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		p := startServe(t, sock)
		if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("no notification: %v\n%s", err, p.output())
		}
		if got := string(buf[:n]); got != "READY=1" {
			t.Fatalf("first notification = %q, want READY=1", got)
		}
		// READY=1 is sent only once the listener is open, so /healthz must
		// answer at once, not after a retry.
		c := &http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://" + p.addr + "/healthz")
		if err != nil {
			t.Fatalf("/healthz after READY=1: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/healthz after READY=1: %d", resp.StatusCode)
		}

		code, elapsed := p.terminate(t)
		if code != 0 {
			t.Fatalf("exit status %d, want 0\n%s", code, p.output())
		}
		if elapsed > 11*time.Second {
			t.Fatalf("exit took %v, want within 11s", elapsed)
		}
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "STOPPING=1" {
			t.Errorf("shutdown notification = %q, %v; want STOPPING=1", buf[:n], err)
		}
	})

	t.Run("without NOTIFY_SOCKET", func(t *testing.T) {
		p := startServe(t, "")
		p.waitHealthy(t)
		code, elapsed := p.terminate(t)
		if code != 0 || elapsed > 11*time.Second {
			t.Fatalf("exit %d after %v\n%s", code, elapsed, p.output())
		}
	})
}

// TC-22: logs are structured JSON on stdout.
func TestServeLogsAreJSON(t *testing.T) {
	p := startServe(t, "")
	p.waitHealthy(t)
	c := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/v1/capabilities", "/v1/nope"} {
		resp, err := c.Get("http://" + p.addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if code, _ := p.terminate(t); code != 0 {
		t.Fatalf("exit %d", code)
	}

	out := p.output()
	sc := bufio.NewScanner(strings.NewReader(out))
	lines := 0
	for sc.Scan() {
		lines++
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %q", lines, sc.Text())
		}
		for _, k := range []string{"time", "level", "msg"} {
			if _, ok := rec[k]; !ok {
				t.Errorf("line %d lacks %q: %s", lines, k, sc.Text())
			}
		}
	}
	if lines < 3 {
		t.Fatalf("only %d stdout lines through start, request and shutdown:\n%s", lines, out)
	}
}

// TC-24: no runtime plugin loading.
func TestNoPluginPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("implausibly short dependency list: %v", deps)
	}
	if slices.Contains(deps, "plugin") {
		t.Fatal("cmd/nexusd depends on the standard library plugin package")
	}
}
