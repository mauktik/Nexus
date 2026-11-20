// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// t1EmuEnv enables TC-157. It needs the Docker CLI's current context to run
// linux/amd64 containers (emulated under Colima on this Mac).
const t1EmuEnv = "NEXUS_T1EMU"

// linuxScript runs inside the container: two real nexusd instances, one
// with tls.mode auto (pinned) and one serving a certificate from a test CA
// (system trust through SSL_CERT_FILE). It prints "R key=value" lines.
const linuxScript = `#!/bin/bash
set -u
T=/t
export HOME=$T/home PATH=$T:$PATH
mkdir -p $T/s1 $T/s2 $T/a1 $T/a2 $HOME
TOK1=$(nexusd token create --name e --state-dir $T/s1 2>/dev/null)
TOK2=$(nexusd token create --name e --state-dir $T/s2 2>/dev/null)
printf 'host: {name: pinned}\nlisten: ["127.0.0.1:18443"]\ntls: {mode: auto}\n' > $T/c1.yaml
printf 'host: {name: catrust}\nlisten: ["127.0.0.1:18444"]\ntls: {mode: files, certFile: %s/srv.pem, keyFile: %s/srv.key}\n' $T $T > $T/c2.yaml
chmod 600 $T/c1.yaml $T/c2.yaml
nexusd serve --config $T/c1.yaml --state-dir $T/s1 --audit-dir $T/a1 >$T/log1 2>&1 &
nexusd serve --config $T/c2.yaml --state-dir $T/s2 --audit-dir $T/a2 >$T/log2 2>&1 &
for port in 18443 18444; do
  for i in $(seq 1 300); do (exec 3<>/dev/tcp/127.0.0.1/$port) 2>/dev/null && break; sleep 0.1; done
done
FP=$(nexusd tls fingerprint --config $T/c1.yaml --state-dir $T/s1)
echo "R arch=$(uname -m)"
echo "R fp=$FP"
echo "$TOK1" | nexus host add good --url https://127.0.0.1:18443 --fingerprint "$FP" --token-stdin; echo "R pin_add=$?"
echo "$TOK1" | nexus host add bad --url https://127.0.0.1:18443 --fingerprint "$(printf '00:%.0s' $(seq 1 31))00" --token-stdin; echo "R mismatch_add=$?"
nexus capability get --host good >$T/caps.json; echo "R pin_get=$?"
echo "R caps=$(cat $T/caps.json)"
CFG=$HOME/.config/nexus/config.yaml
echo "R mode=$(stat -c %a $CFG)"
chmod 640 $CFG; nexus capability get --host good; echo "R group_readable=$?"; chmod 600 $CFG
echo "$TOK2" | SSL_CERT_FILE=$T/ca.pem nexus host add sys --url https://127.0.0.1:18444 --token-stdin; echo "R sys_add_root=$?"
SSL_CERT_FILE=$T/ca.pem nexus capability get --host sys >/dev/null; echo "R sys_get_root=$?"
nexus capability get --host sys; echo "R sys_get_noroot=$?"
echo "$TOK2" | nexus host add sys2 --url https://127.0.0.1:18444 --token-stdin; echo "R sys_add_noroot=$?"
echo "R mode_after=$(stat -c %a $CFG)"
`

// TC-157: the client on linux/amd64 against a real linux/amd64 nexusd, in
// `docker run --platform linux/amd64 ubuntu:latest`, without systemd.
func TestClientLinuxAmd64(t *testing.T) {
	if os.Getenv(t1EmuEnv) != "1" {
		t.Skipf("set %s=1 to run the linux/amd64 client case in Docker", t1EmuEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 but docker is not installed", t1EmuEnv)
	}
	dir := t.TempDir()
	for _, b := range []struct{ name, pkg string }{{"nexus", "."}, {"nexusd", "../nexusd"}} {
		build := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(dir, b.name), b.pkg)
		build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s for linux/amd64: %v\n%s", b.pkg, err, out)
		}
	}
	writeTestPKI(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(linuxScript), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	name := fmt.Sprintf("nexus-tc157-%d", time.Now().UnixNano())
	docker("run", "-d", "--platform", "linux/amd64", "--name", name, "ubuntu:latest", "sleep", "600")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	// docker cp, not a bind mount: Colima does not share the temp directory.
	docker("cp", dir, name+":/t")
	out := docker("exec", name, "bash", "/t/run.sh")
	t.Logf("container output:\n%s", out)

	got := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "R "), "="); ok && strings.HasPrefix(sc.Text(), "R ") {
			got[k] = v
		}
	}
	want := map[string]string{
		"arch": "x86_64", "pin_add": "0", "mismatch_add": "5", "pin_get": "0", "mode": "600",
		"group_readable": "3", "sys_add_root": "0", "sys_get_root": "0", "sys_get_noroot": "5",
		"sys_add_noroot": "5", "mode_after": "600",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if !strings.Contains(got["caps"], `"host":"pinned"`) {
		t.Errorf("capabilities over the pin: %q", got["caps"])
	}
}

// writeTestPKI writes ca.pem and a 127.0.0.1 server certificate it signed,
// srv.pem and srv.key.
func writeTestPKI(t *testing.T, dir string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nexus TC-157 root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"ca.pem":  {Type: "CERTIFICATE", Bytes: caDER},
		"srv.pem": {Type: "CERTIFICATE", Bytes: der},
		"srv.key": {Type: "PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
