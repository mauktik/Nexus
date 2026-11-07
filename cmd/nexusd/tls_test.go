package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/tlsutil"
)

// sha256Hex is the pinning formula, computed here independently of tlsutil:
// SHA-256 over the certificate DER, colon-separated upper-case hex.
func sha256Hex(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	pairs := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		pairs = append(pairs, h[i:i+2])
	}
	return strings.Join(pairs, ":")
}

// runBounded runs nexusd and returns stdout, stderr and the exit code, killing
// it after 20s so a serve that wrongly starts cannot hang the test.
func runBounded(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nexusdBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("nexusd %v still running after 20s\nstdout:\n%s\nstderr:\n%s", args, stdout.String(), stderr.String())
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	t.Fatalf("run nexusd %v: %v", args, err)
	return "", "", -1
}

// startServeConfig runs nexusd serve on cfg with its state in stateDir.
func startServeConfig(t *testing.T, cfg, stateDir, addr string) *serveProc {
	t.Helper()
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", stateDir, "--audit-dir", t.TempDir())
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

// servedLeaf waits for the TLS listener and returns the leaf it presents.
// Verification is skipped here only to capture the leaf; the test pins it.
func (p *serveProc) servedLeaf(t *testing.T) []byte {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d := &net.Dialer{Timeout: time.Second}
		conn, err := tls.DialWithDialer(d, "tcp", p.addr, &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			certs := conn.ConnectionState().PeerCertificates
			_ = conn.Close()
			if len(certs) == 0 {
				t.Fatal("no peer certificate")
			}
			return certs[0].Raw
		}
		select {
		case err := <-p.done:
			p.done <- err
			t.Fatalf("nexusd exited before serving TLS: %v\n%s", err, p.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("nexusd not serving TLS within 15s\n%s", p.output())
	return nil
}

// loggedFingerprint returns the fingerprint and generated flag from the
// certificate record nexusd writes to stdout.
func loggedFingerprint(t *testing.T, out string) (string, bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var rec struct {
			Fingerprint string `json:"fingerprint"`
			Generated   bool   `json:"generated"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		if rec.Fingerprint != "" {
			return rec.Fingerprint, rec.Generated
		}
	}
	t.Fatalf("no fingerprint on stdout:\n%s", out)
	return "", false
}

// TC-39: self-signed certificate generation and stability.
func TestSelfSignedCertificate(t *testing.T) {
	state := t.TempDir()
	addr := freeAddr(t)
	cfg := writeConfig(t, "nexusd.yaml", fmt.Sprintf("listen: [%q]\ntls: {mode: auto, extraSANs: [nas.lan]}\n", addr))

	p1 := startServeConfig(t, cfg, state, addr)
	leaf1 := p1.servedLeaf(t)
	if code, _ := p1.terminate(t); code != 0 {
		t.Fatalf("first serve exited %d\n%s", code, p1.output())
	}
	fp1, generated := loggedFingerprint(t, p1.output())
	if !generated {
		t.Error("first start did not report generating the certificate")
	}

	p2 := startServeConfig(t, cfg, state, addr)
	leaf2 := p2.servedLeaf(t)
	if code, _ := p2.terminate(t); code != 0 {
		t.Fatalf("second serve exited %d\n%s", code, p2.output())
	}
	fp2, generated := loggedFingerprint(t, p2.output())
	if generated {
		t.Error("second start generated a new certificate")
	}

	out, errOut, code := runBounded(t, "tls", "fingerprint", "--config", cfg, "--state-dir", state)
	if code != 0 {
		t.Fatalf("tls fingerprint exited %d: %s", code, errOut)
	}
	fp3 := strings.TrimSuffix(out, "\n")

	want := sha256Hex(leaf1)
	if !bytes.Equal(leaf1, leaf2) {
		t.Fatal("the served certificate changed across restart")
	}
	for name, got := range map[string]string{"first start": fp1, "second start": fp2, "tls fingerprint": fp3} {
		if got != want {
			t.Errorf("%s printed %q, want %q (SHA-256 of the served leaf)", name, got, want)
		}
	}

	certPEM, err := os.ReadFile(filepath.Join(state, "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert.pem holds no PEM")
	}
	if !bytes.Equal(block.Bytes, leaf1) {
		t.Fatal("cert.pem is not the served certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.IsCA {
		t.Error("IsCA is true; the certificate must be a plain leaf (A-15)")
	}
	if want := cert.NotBefore.AddDate(10, 0, 0); !cert.NotAfter.Equal(want) {
		t.Errorf("validity %s to %s, want 10 years", cert.NotBefore, cert.NotAfter)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{host, "localhost", "nas.lan"} {
		if !slices.Contains(cert.DNSNames, n) {
			t.Errorf("DNS SANs %q lack %q", cert.DNSNames, n)
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	nonLoopback := 0
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		nonLoopback++
		if !slices.ContainsFunc(cert.IPAddresses, ipn.IP.Equal) {
			t.Errorf("IP SANs %v lack interface address %s", cert.IPAddresses, ipn.IP)
		}
	}
	t.Logf("checked %d non-loopback interface addresses", nonLoopback)

	keyPath := filepath.Join(state, "tls", "key.pem")
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key.pem mode %04o, want 0600", fi.Mode().Perm())
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		t.Fatal("key.pem holds no PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if ek, ok := key.(*ecdsa.PrivateKey); !ok || ek.Curve != elliptic.P256() {
		t.Errorf("key is %T, want ECDSA P-256", key)
	}

	// A Go client told to trust this one certificate accepts it by name.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	p3 := startServeConfig(t, cfg, state, addr)
	p3.servedLeaf(t)
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("client trusting the leaf: %v", err)
	}
	_ = conn.Close()
	if code, _ := p3.terminate(t); code != 0 {
		t.Fatalf("third serve exited %d", code)
	}
}

func writeGeneratedPair(t *testing.T, dir, prefix string) (string, string, []byte) {
	t.Helper()
	certPEM, keyPEM, err := tlsutil.Generate("files-host", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(dir, prefix+"cert.pem"), filepath.Join(dir, prefix+"key.pem")
	if err := os.WriteFile(c, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	return c, k, block.Bytes
}

// TC-40: configured certificate files.
func TestConfiguredCertificateFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, der := writeGeneratedPair(t, dir, "a-")
	otherCert, _, _ := writeGeneratedPair(t, dir, "b-")
	addr := freeAddr(t)
	files := func(c, k string) string {
		return fmt.Sprintf("listen: [%q]\ntls:\n  mode: files\n  certFile: %s\n  keyFile: %s\n", addr, c, k)
	}

	state := t.TempDir()
	cfg := writeConfig(t, "valid.yaml", files(certPath, keyPath))
	p := startServeConfig(t, cfg, state, addr)
	if leaf := p.servedLeaf(t); !bytes.Equal(leaf, der) {
		t.Fatal("the served leaf is not the configured certificate")
	}
	if code, _ := p.terminate(t); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
	if fp, _ := loggedFingerprint(t, p.output()); fp != sha256Hex(der) {
		t.Errorf("logged fingerprint %s, want %s", fp, sha256Hex(der))
	}
	if out, errOut, code := runBounded(t, "tls", "fingerprint", "--config", cfg); code != 0 || out != sha256Hex(der)+"\n" {
		t.Errorf("tls fingerprint: %q %q exit %d, want %s", out, errOut, code, sha256Hex(der))
	}
	if entries, _ := os.ReadDir(state); len(entries) > 0 {
		for _, e := range entries {
			if e.Name() == "tls" {
				t.Error("files mode generated a certificate in the state directory")
			}
		}
	}

	missing := filepath.Join(dir, "missing.pem")
	for _, c := range []struct {
		name, cert, key, want string
	}{
		{"mismatched", otherCert, keyPath, ":5:12: tls.keyFile:"},
		{"missing certificate", missing, keyPath, ":4:13: tls.certFile:"},
		{"missing key", certPath, missing, ":5:12: tls.keyFile:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := writeConfig(t, "bad.yaml", files(c.cert, c.key))
			out, code := runNexusd(t, "validate-config", "--config", bad)
			if code == 0 || !strings.Contains(out, bad+c.want) {
				t.Errorf("validate-config exit %d, want non-zero with %q:\n%s", code, bad+c.want, out)
			}
			stdout, stderr, code := runBounded(t, "serve", "--config", bad, "--state-dir", t.TempDir(), "--audit-dir", t.TempDir())
			if code == 0 || !strings.Contains(stderr, bad+c.want) {
				t.Errorf("serve exit %d, want non-zero with %q:\n%s", code, bad+c.want, stderr)
			}
			if strings.Contains(stdout, "nexusd ready") {
				t.Errorf("serve became ready:\n%s", stdout)
			}
		})
	}
}

// TC-ERR16: non-loopback without TLS is refused, with no override.
func TestNonLoopbackWithoutTLSRefused(t *testing.T) {
	port := func() string {
		_, p, err := net.SplitHostPort(freeAddr(t))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	lan := "192.168.1.20"
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				lan = ipn.IP.String()
				break
			}
		}
	}
	p := port()
	cases := []struct {
		listen  string
		bad     string
		probeAt string
	}{
		{fmt.Sprintf(`["0.0.0.0:%s"]`, p), "listen[0]", "127.0.0.1:" + p},
		{fmt.Sprintf(`["[::]:%s"]`, p), "listen[0]", "127.0.0.1:" + p},
		{fmt.Sprintf(`["%s:%s"]`, lan, p), "listen[0]", net.JoinHostPort(lan, p)},
		{fmt.Sprintf(`["127.0.0.1:%s", "0.0.0.0:%s"]`, p, port()), "listen[1]", "127.0.0.1:" + p},
	}
	for _, c := range cases {
		t.Run(c.listen, func(t *testing.T) {
			cfg := writeConfig(t, "off.yaml", "listen: "+c.listen+"\ntls: {mode: off}\n")
			out, code := runNexusd(t, "validate-config", "--config", cfg)
			if code == 0 || !strings.Contains(out, c.bad+":") || !strings.Contains(out, "loopback") {
				t.Errorf("validate-config exit %d, want non-zero naming %s and loopback:\n%s", code, c.bad, out)
			}
			stdout, stderr, code := runBounded(t, "serve", "--config", cfg, "--state-dir", t.TempDir(), "--audit-dir", t.TempDir())
			if code == 0 || !strings.Contains(stderr, c.bad+":") {
				t.Errorf("serve exit %d, want non-zero naming %s:\n%s", code, c.bad, stderr)
			}
			if strings.Contains(stdout, "nexusd ready") {
				t.Errorf("serve became ready:\n%s", stdout)
			}
			if conn, err := net.DialTimeout("tcp", c.probeAt, 200*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Errorf("something is listening on %s", c.probeAt)
			}
		})
	}
	for _, listen := range []string{`["127.0.0.1:` + port() + `"]`, `["[::1]:` + port() + `"]`} {
		cfg := writeConfig(t, "ok.yaml", "listen: "+listen+"\ntls: {mode: off}\n")
		if out, code := runNexusd(t, "validate-config", "--config", cfg); code != 0 {
			t.Errorf("loopback %s with tls.mode off: exit %d\n%s", listen, code, out)
		}
	}

	// No flag can override the rule.
	var help strings.Builder
	for _, args := range [][]string{{"serve", "-h"}, {"-h"}, {"tls", "fingerprint", "-h"}, {"validate-config", "-h"}} {
		out, _ := runNexusd(t, args...)
		help.WriteString(out)
	}
	if !strings.Contains(help.String(), "-config") {
		t.Fatalf("help output lacks the flags; the search would be vacuous:\n%s", help.String())
	}
	low := strings.ToLower(help.String())
	for _, bad := range []string{"insecure", "allow", "no-tls", "notls", "plaintext"} {
		if strings.Contains(low, bad) {
			t.Errorf("help mentions %q:\n%s", bad, help.String())
		}
	}
}

// `nexusd tls fingerprint` never generates a certificate, and says why it
// has none to show.
func TestTLSFingerprintWithoutCertificate(t *testing.T) {
	state := t.TempDir()
	auto := writeConfig(t, "auto.yaml", "tls: {mode: auto}\n")
	if out, errOut, code := runBounded(t, "tls", "fingerprint", "--config", auto, "--state-dir", state); code == 0 ||
		!strings.Contains(errOut, "nexusd serve") {
		t.Errorf("no certificate yet: %q %q exit %d, want an error pointing at nexusd serve", out, errOut, code)
	}
	if _, err := os.Stat(filepath.Join(state, "tls")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tls fingerprint created %s/tls: %v", state, err)
	}
	off := writeConfig(t, "off.yaml", "tls: {mode: off}\n")
	if _, errOut, code := runBounded(t, "tls", "fingerprint", "--config", off); code == 0 || !strings.Contains(errOut, "off") {
		t.Errorf("tls.mode off: %q exit %d, want an error", errOut, code)
	}
}
