package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// freeLoopbackPair returns two free loopback addresses: 127.0.0.1 and ::1
// when the host has IPv6 loopback, otherwise two 127.0.0.1 ports.
func freeLoopbackPair(t *testing.T) [2]string {
	t.Helper()
	second := "[::1]:0"
	if ln, err := net.Listen("tcp", second); err != nil {
		second = "127.0.0.1:0"
	} else {
		_ = ln.Close()
	}
	var addrs [2]string
	for i, a := range []string{"127.0.0.1:0", second} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			t.Fatal(err)
		}
		addrs[i] = ln.Addr().String()
		defer ln.Close()
	}
	return addrs
}

// waitHealthyAt polls GET /healthz at url until it answers 200, failing if
// nexusd exits first or 15s pass.
func (p *serveProc) waitHealthyAt(t *testing.T, c *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = resp.Status
		} else {
			last = err.Error()
		}
		select {
		case err := <-p.done:
			p.done <- err
			t.Fatalf("GET %s: nexusd exited before answering: %v\n%s", url, err, p.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("GET %s not 200 within 15s (last: %s)\n%s", url, last, p.output())
}

func listenYAML(addrs [2]string) string {
	quoted := make([]string, len(addrs))
	for i, a := range addrs {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return "listen: [" + strings.Join(quoted, ", ") + "]\n"
}

// A serve with several listen addresses answers /healthz on every one of
// them, with TLS off (loopback only) and with a generated certificate.
func TestServeAnswersOnEveryListenAddress(t *testing.T) {
	t.Run("tls off", func(t *testing.T) {
		addrs := freeLoopbackPair(t)
		cfg := writeConfig(t, "nexusd.yaml", listenYAML(addrs)+"tls: {mode: off}\n")
		p := startServeConfig(t, cfg, t.TempDir(), addrs[0])
		c := &http.Client{Timeout: time.Second}
		for _, a := range addrs {
			p.waitHealthyAt(t, c, "http://"+a+"/healthz")
		}
		if code, _ := p.terminate(t); code != 0 {
			t.Fatalf("exit status %d, want 0\n%s", code, p.output())
		}
	})

	t.Run("tls auto", func(t *testing.T) {
		addrs := freeLoopbackPair(t)
		cfg := writeConfig(t, "nexusd.yaml", listenYAML(addrs)+"tls: {mode: auto}\n")
		p := startServeConfig(t, cfg, t.TempDir(), addrs[0])
		// Verification is skipped: the generated certificate is pinned
		// elsewhere (TC-39); this test is about which listeners answer.
		c := &http.Client{Timeout: time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
		for _, a := range addrs {
			p.waitHealthyAt(t, c, "https://"+a+"/healthz")
		}
		if code, _ := p.terminate(t); code != 0 {
			t.Fatalf("exit status %d, want 0\n%s", code, p.output())
		}
	})
}
