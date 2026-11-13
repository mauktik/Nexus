// nexusd-self-management test stubs: planning/nexusd-self-management/test-cases.md.
//
// Stub style: SKIP-only. Every test here compiles today and skips with its
// TC ID. The developer of the owning work item replaces the t.Skip with the
// assertions described in the comment above the test; the test passing
// un-skipped is the GREEN signal. Keep the function name so that
// `grep TestTCSM` traces the case back to test-cases.md.
package client

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TC-SM-64 (WI 4.1; FR-10): client.PinnedTLS is exported and normalizes fingerprints
func TestTCSM64_ClientPinnedTLSIsExportedAndNormalizes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	f := hex.EncodeToString(sum[:])
	var pairs []string
	for i := 0; i < len(f); i += 2 {
		pairs = append(pairs, f[i:i+2])
	}
	get := func(fp string) error {
		conf, err := PinnedTLS(fp)
		if err != nil {
			return err
		}
		tr := &http.Transport{TLSClientConfig: conf}
		defer tr.CloseIdleConnections()
		c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		resp, err := c.Get(srv.URL + "/healthz")
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	// Step 1: lower case, upper case and colon-separated.
	for _, fp := range []string{f, strings.ToUpper(f), strings.Join(pairs, ":")} {
		if err := get(fp); err != nil {
			t.Errorf("PinnedTLS(%s): GET failed: %v", fp, err)
		}
	}

	// Step 2: a wrong pin aborts the handshake with *PinMismatchError.
	wrong := strings.Repeat("ab", sha256.Size)
	var pm *PinMismatchError
	if err := get(wrong); !errors.As(err, &pm) {
		t.Errorf("PinnedTLS(wrong): GET err %v, want *PinMismatchError", err)
	}

	// Step 3: a malformed fingerprint is an error.
	if conf, err := PinnedTLS("zz"); err == nil || conf != nil {
		t.Errorf("PinnedTLS(zz) = %v, %v; want nil and an error", conf, err)
	}

	// Step 4: pinning.go sets InsecureSkipVerify only beside VerifyConnection
	// (the module-wide check is internal/cli TestInsecureSkipVerifyOnlyWithVerifyConnection).
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "pinning.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var skip, verify bool
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					skip = skip || key.Name == "InsecureSkipVerify"
					verify = verify || key.Name == "VerifyConnection"
				}
			}
		}
		if skip && !verify {
			t.Errorf("%s: InsecureSkipVerify without VerifyConnection", fset.Position(lit.Pos()))
		}
		return true
	})
}
