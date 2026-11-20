// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mauktik/nexus/internal/client"
	"github.com/mauktik/nexus/internal/clientconfig"
)

// walk visits every command in the tree.
func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}

// TC-ERR60: no flag disables verification, and --insecure is a usage error.
func TestNoFlagDisablesVerification(t *testing.T) {
	a := &app{env: Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Getenv: func(string) string { return "" }}}
	root := a.newRoot()
	banned := []string{"insecure", "skip-verify", "no-verify", "tls-skip", "skipverify", "noverify"}
	seen := 0
	walk(root, func(c *cobra.Command) {
		for _, set := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags(), c.InheritedFlags()} {
			set.VisitAll(func(f *pflag.Flag) {
				seen++
				name := strings.ToLower(f.Name)
				for _, b := range banned {
					if strings.Contains(name, b) {
						t.Errorf("%s: flag --%s disables verification", c.CommandPath(), f.Name)
					}
				}
				// No flag takes a token as its value (FR-35).
				if strings.Contains(name, "token") && f.Value.Type() != "bool" {
					t.Errorf("%s: flag --%s accepts a token as an argument", c.CommandPath(), f.Name)
				}
			})
		}
	})
	if seen == 0 {
		t.Fatal("walked no flags")
	}

	for _, args := range [][]string{
		{"--insecure"},
		{"capability", "get", "--insecure"},
		{"host", "add", "x", "--url", "https://127.0.0.1:1", "--insecure"},
		{"host", "add", "x", "--url", "https://127.0.0.1:1", "--skip-verify"},
		{"host", "add", "x", "--url", "https://127.0.0.1:1", "--token", "SENTINEL"},
	} {
		var stderr bytes.Buffer
		code := Run(args, Env{Stdout: &bytes.Buffer{}, Stderr: &stderr, Getenv: func(string) string { return "" }})
		if code != ExitUsage {
			t.Errorf("nexus %v exited %d, want 2", args, code)
		}
		if strings.Contains(stderr.String(), "SENTINEL") {
			t.Errorf("nexus %v echoed the token: %s", args, stderr.String())
		}
	}
}

// moduleRoot finds the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// skipVerifyViolations reports each place in src where InsecureSkipVerify is
// set without a VerifyConnection in the same tls.Config literal.
func skipVerifyViolations(fset *token.FileSet, file *ast.File) []string {
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			var skip, verify bool
			for _, e := range n.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "InsecureSkipVerify":
					if v, ok := kv.Value.(*ast.Ident); !ok || v.Name != "false" {
						skip = true
					}
				case "VerifyConnection":
					if v, ok := kv.Value.(*ast.Ident); !ok || v.Name != "nil" {
						verify = true
					}
				}
			}
			if skip && !verify {
				out = append(out, fset.Position(n.Pos()).String())
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "InsecureSkipVerify" {
					out = append(out, fset.Position(n.Pos()).String()+" (assignment)")
				}
			}
		}
		return true
	})
	return out
}

// TC-ERR60: InsecureSkipVerify: true appears only together with
// VerifyConnection, in every non-test file of the module.
func TestInsecureSkipVerifyOnlyWithVerifyConnection(t *testing.T) {
	// The check catches a planted violation.
	fset := token.NewFileSet()
	planted, err := parser.ParseFile(fset, "planted.go",
		"package p\nimport \"crypto/tls\"\nvar a = &tls.Config{InsecureSkipVerify: true}\n"+
			"var b = &tls.Config{InsecureSkipVerify: true, VerifyConnection: f}\n"+
			"func g(c *tls.Config) { c.InsecureSkipVerify = true }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if v := skipVerifyViolations(fset, planted); len(v) != 2 {
		t.Fatalf("planted violations found: %v, want 2 (the bare literal and the assignment)", v)
	}

	root := moduleRoot(t)
	files, withSkip := 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "planning" || name == "testdata" || name == "dist" || name == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		if bytes.Contains(src, []byte("InsecureSkipVerify")) {
			withSkip++
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		for _, v := range skipVerifyViolations(fset, f) {
			t.Errorf("%s: InsecureSkipVerify without VerifyConnection", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 || withSkip == 0 {
		t.Fatalf("scanned %d files, %d mention InsecureSkipVerify; the walk is not covering the module", files, withSkip)
	}
}

// fakeTerminal answers prompts from a script and records what was shown.
type fakeTerminal struct {
	out     bytes.Buffer
	lines   []string
	secrets []string
}

func (f *fakeTerminal) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *fakeTerminal) Close() error                { return nil }
func (f *fakeTerminal) ReadLine() (string, error) {
	if len(f.lines) == 0 {
		return "", errors.New("no more input")
	}
	l := f.lines[0]
	f.lines = f.lines[1:]
	return l, nil
}
func (f *fakeTerminal) ReadSecret() (string, error) {
	if len(f.secrets) == 0 {
		return "", errors.New("no more input")
	}
	s := f.secrets[0]
	f.secrets = f.secrets[1:]
	return s, nil
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nexus cli test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// issue returns a 127.0.0.1 leaf signed by ca, or self-signed for a nil ca.
func issue(t *testing.T, ca *testCA) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "nexus cli test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	parent, signer := tmpl, any(key)
	if ca != nil {
		parent, signer = ca.cert, ca.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type capServer struct {
	*httptest.Server
	requests atomic.Int64
	authz    atomic.Value
}

func startCapServer(t *testing.T, cert tls.Certificate) *capServer {
	t.Helper()
	cs := &capServer{}
	cs.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.requests.Add(1)
		cs.authz.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"/capabilities","host":"srv","apiVersion":"v1","version":"v1.2.3","providers":[]}`))
	}))
	cs.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	cs.StartTLS()
	t.Cleanup(cs.Close)
	return cs
}

type runResult struct {
	code           int
	stdout, stderr string
}

func runCLI(env Env, args ...string) runResult {
	var stdout, stderr bytes.Buffer
	env.Stdout, env.Stderr = &stdout, &stderr
	code := Run(args, env)
	return runResult{code, stdout.String(), stderr.String()}
}

// TC-153 through the CLI: a host added by system trust works; the same
// stored host against an untrusted server exits 5.
func TestHostAddSystemTrust(t *testing.T) {
	ca := newCA(t)
	trusted := startCapServer(t, issue(t, ca))
	untrusted := startCapServer(t, issue(t, nil))
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env := Env{Getenv: getenv, RootCAs: ca.pool(), Stdin: strings.NewReader("tok-sys\n"),
		OpenTerminal: func() (Terminal, error) { return nil, errors.New("no terminal") }}

	r := runCLI(env, "host", "add", "sys", "--url", trusted.URL, "--token-stdin")
	if r.code != 0 {
		t.Fatalf("host add by system trust exited %d: %s", r.code, r.stderr)
	}
	path, _ := clientconfig.Path(getenv)
	cfg, err := clientconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Hosts["sys"]; h.Fingerprint != "" || h.Trust() != "system" || h.Token != "tok-sys" {
		t.Fatalf("stored host %+v, want system trust", h)
	}
	if r := runCLI(env, "capability", "get", "--host", "sys"); r.code != 0 {
		t.Fatalf("capability get over system trust exited %d: %s", r.code, r.stderr)
	}

	// Point the system-trust host at a server the roots do not trust.
	cfg.Hosts["sys"] = clientconfig.Host{URL: untrusted.URL, Token: "tok-sys"}
	if err := clientconfig.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(env, "capability", "get", "--host", "sys"); r.code != ExitConnection {
		t.Fatalf("untrusted server exited %d, want 5: %s", r.code, r.stderr)
	}
	if untrusted.requests.Load() != 0 {
		t.Error("a request reached the untrusted server")
	}

	// Adding the untrusted server with no terminal and no --fingerprint
	// cannot be confirmed: exit 5, nothing saved.
	env.Stdin = strings.NewReader("tok\n")
	r = runCLI(env, "host", "add", "u", "--url", untrusted.URL, "--token-stdin")
	if r.code != ExitConnection || !strings.Contains(r.stderr, client.Fingerprint(untrusted.Certificate().Raw)) {
		t.Fatalf("untrusted add without a terminal: exit %d, stderr %s", r.code, r.stderr)
	}
}

// The confirmation and the no-echo token prompt go to the terminal; the
// token is never printed.
func TestHostAddConfirmOnTerminal(t *testing.T) {
	srv := startCapServer(t, issue(t, nil))
	fp := client.Fingerprint(srv.Certificate().Raw)
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	const secret = "nexus_SENTINEL_cli_7b1d"
	for _, tc := range []struct {
		name, answer string
		code         int
		saved        bool
	}{{"b", "y", 0, true}, {"c", "n", ExitFailure, false}} {
		tty := &fakeTerminal{lines: []string{tc.answer}, secrets: []string{secret}}
		env := Env{Getenv: getenv, OpenTerminal: func() (Terminal, error) { return tty, nil }}
		r := runCLI(env, "host", "add", tc.name, "--url", srv.URL)
		if r.code != tc.code {
			t.Fatalf("answer %q: exit %d, want %d: %s", tc.answer, r.code, tc.code, r.stderr)
		}
		if !strings.Contains(r.stderr, fp) {
			t.Errorf("answer %q: fingerprint %s not shown: %s", tc.answer, fp, r.stderr)
		}
		all := r.stdout + r.stderr + tty.out.String()
		if strings.Contains(all, secret) {
			t.Errorf("answer %q: the token was printed", tc.answer)
		}
		cfg, err := clientconfig.Load(filepath.Join(home, ".config", "nexus", "config.yaml"))
		if err != nil && tc.saved {
			t.Fatal(err)
		}
		if _, ok := cfg.Hosts[tc.name]; ok != tc.saved {
			t.Errorf("answer %q: saved = %v, want %v", tc.answer, ok, tc.saved)
		}
		if tc.saved && cfg.Hosts[tc.name].Fingerprint != fp {
			t.Errorf("pinned %q, want %q", cfg.Hosts[tc.name].Fingerprint, fp)
		}
	}
	if got := srv.authz.Load(); got != "Bearer "+secret {
		t.Errorf("server saw Authorization %q", got)
	}
}
