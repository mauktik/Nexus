package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/tlsutil"
)

func errorsAt(t *testing.T, err error, path string) []*Error {
	t.Helper()
	var list ErrorList
	if !errors.As(err, &list) {
		t.Fatalf("error = %v, want an ErrorList", err)
	}
	var at []*Error
	for _, e := range list {
		if e.Path == path {
			at = append(at, e)
		}
	}
	return at
}

// TC-ERR16: tls.mode off is valid only when every listen address is
// loopback. 0.0.0.0 and :: count as non-loopback, and there is no override.
func TestTLSOffRequiresLoopback(t *testing.T) {
	refused := []struct {
		name   string
		listen string
		bad    []int
	}{
		{"every IPv4 interface", `["0.0.0.0:7443"]`, []int{0}},
		{"every IPv6 interface", `["[::]:7443"]`, []int{0}},
		{"LAN address", `["192.168.1.20:7443"]`, []int{0}},
		{"loopback mixed with every interface", `["127.0.0.1:7443", "0.0.0.0:7443"]`, []int{1}},
		{"hostname", `["nas.lan:7443"]`, []int{0}},
		{"localhost is a name, not an address", `["localhost:7443"]`, []int{0}},
		{"IPv4-mapped LAN address", `["[::ffff:192.168.1.20]:7443"]`, []int{0}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			src := "listen: " + tc.listen + "\ntls: {mode: off}\n"
			_, err := Parse([]byte(src), "c.yaml")
			if err == nil {
				t.Fatalf("listen %s with tls.mode off was accepted", tc.listen)
			}
			for _, i := range tc.bad {
				at := errorsAt(t, err, fmt.Sprintf("listen[%d]", i))
				if len(at) != 1 || !strings.Contains(at[0].Msg, "loopback") || !strings.Contains(at[0].Msg, "tls.mode") {
					t.Fatalf("listen[%d]: %v; want one error about loopback and tls.mode in:\n%v", i, at, err)
				}
				if at[0].Line != 1 || at[0].Column == 0 {
					t.Errorf("error %v is not located at the value", at[0])
				}
			}
			var list ErrorList
			errors.As(err, &list)
			if len(list) != len(tc.bad) {
				t.Errorf("%d errors, want %d:\n%v", len(list), len(tc.bad), err)
			}
		})
	}

	for _, listen := range []string{`["127.0.0.1:7443"]`, `["[::1]:7443"]`, `["127.0.0.1:7443", "[::1]:7443"]`, `["127.0.0.2:7443"]`} {
		src := "listen: " + listen + "\ntls: {mode: off}\n"
		if _, err := Parse([]byte(src), "c.yaml"); err != nil {
			t.Errorf("loopback %s with tls.mode off: %v", listen, err)
		}
	}
	// With TLS, any address is allowed.
	for _, mode := range []string{"auto"} {
		src := `listen: ["0.0.0.0:7443", "[::]:7443"]` + "\ntls: {mode: " + mode + "}\n"
		if _, err := Parse([]byte(src), "c.yaml"); err != nil {
			t.Errorf("non-loopback with tls.mode %s: %v", mode, err)
		}
	}
}

// TC-ERR16 step 2: no configuration key can override the loopback rule.
func TestNoTLSOverrideKey(t *testing.T) {
	var keys []string
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, prefix string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		for i := range rt.NumField() {
			f := rt.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			keys = append(keys, prefix+tag)
			ft := f.Type
			// Provider sections carry their settings in a generic field.
			if s, ok := reflect.New(ft).Interface().(sectionTarget); ok {
				ft = reflect.TypeOf(s.settings()).Elem()
			}
			walk(ft, prefix+tag+".")
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	if len(keys) < 20 {
		t.Fatalf("only %d keys found; the search below would be vacuous: %v", len(keys), keys)
	}
	for _, k := range keys {
		low := strings.ToLower(k)
		// mcp.allowedOrigins is the /mcp Origin allow-list (8.1), not a
		// transport override.
		if k == "mcp.allowedOrigins" {
			continue
		}
		for _, bad := range []string{"insecure", "allow", "no-tls", "notls", "plaintext", "override"} {
			if strings.Contains(low, bad) {
				t.Errorf("configuration key %s looks like a TLS override (%q)", k, bad)
			}
		}
	}
	_, err := Parse([]byte("listen: [\"0.0.0.0:7443\"]\ntls: {mode: off, allowInsecure: true}\n"), "c.yaml")
	if at := errorsAt(t, err, "tls.allowInsecure"); len(at) != 1 || !strings.Contains(at[0].Msg, "unknown key") {
		t.Fatalf("tls.allowInsecure: %v", err)
	}
}

// TC-43 step 3: there is no trustedProxies key, so a config naming it fails.
func TestTrustedProxiesIsUnknown(t *testing.T) {
	for _, src := range []string{
		"trustedProxies: [\"10.0.0.1\"]\n",
		"auth:\n  trustedProxies: [\"10.0.0.1\"]\n",
	} {
		_, err := Parse([]byte(src), "c.yaml")
		var list ErrorList
		if !errors.As(err, &list) {
			t.Fatalf("%q: %v, want an ErrorList", src, err)
		}
		found := false
		for _, e := range list {
			if strings.HasSuffix(e.Path, "trustedProxies") && strings.Contains(e.Msg, "unknown key") && e.Line > 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: no located unknown-key error naming trustedProxies:\n%v", src, err)
		}
	}
}

func writePair(t *testing.T, dir, prefix, host string) (certPath, keyPath string) {
	t.Helper()
	certPEM, keyPEM, err := tlsutil.Generate(host, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, prefix+"cert.pem")
	keyPath = filepath.Join(dir, prefix+"key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// TC-40: files mode loads the configured pair at validation, so a missing,
// unparsable or mismatched pair fails validate-config and startup with a
// located error.
func TestTLSFilesAreChecked(t *testing.T) {
	dir := t.TempDir()
	cert, key := writePair(t, dir, "a-", "nas")
	otherCert, _ := writePair(t, dir, "b-", "other")
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")

	src := func(c, k string) []byte {
		return fmt.Appendf(nil, "tls:\n  mode: files\n  certFile: %s\n  keyFile: %s\n", c, k)
	}
	if _, err := Parse(src(cert, key), "c.yaml"); err != nil {
		t.Fatalf("valid pair: %v", err)
	}
	cases := []struct {
		name       string
		cert, key  string
		path, want string
		line       int
	}{
		{"missing certificate", missing, key, "tls.certFile", "no such file", 3},
		{"missing key", cert, missing, "tls.keyFile", "no such file", 4},
		{"certificate is not PEM", garbage, key, "tls.certFile", "CERTIFICATE", 3},
		{"key is not PEM", cert, garbage, "tls.keyFile", "key", 4},
		{"mismatched pair", otherCert, key, "tls.keyFile", "does not match", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(src(tc.cert, tc.key), "c.yaml")
			if err == nil {
				t.Fatal("accepted")
			}
			at := errorsAt(t, err, tc.path)
			if len(at) != 1 || !strings.Contains(at[0].Msg, tc.want) {
				t.Fatalf("%s: %v; want one error containing %q in:\n%v", tc.path, at, tc.want, err)
			}
			if at[0].Line != tc.line || at[0].Column == 0 {
				t.Errorf("error %v, want line %d with a column", at[0], tc.line)
			}
		})
	}
}
