package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/store"
)

var printedTokenRE = regexp.MustCompile(`^nxs_([0-9a-f]{12})_([A-Za-z0-9_-]{43})\n$`)

// runToken runs `nexusd token ...` against dir and returns stdout, stderr
// and the exit code.
func runToken(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	full := append([]string{"token"}, args...)
	full = append(full, "--state-dir", dir)
	cmd := exec.Command(nexusdBin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	t.Fatalf("run nexusd %v: %v", full, err)
	return "", "", -1
}

func createToken(t *testing.T, dir string, args ...string) (token, id string, secret []byte) {
	t.Helper()
	out, errOut, code := runToken(t, dir, append([]string{"create"}, args...)...)
	if code != 0 {
		t.Fatalf("token create %v exited %d: %s", args, code, errOut)
	}
	m := printedTokenRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("stdout %q is not exactly one nxs_<id>_<secret> line", out)
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil {
		t.Fatalf("secret is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("secret decodes to %d bytes, want 32", len(raw))
	}
	return strings.TrimSpace(out), m[1], raw
}

type tokenFileRecord struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	SHA256  string     `json:"sha256"`
	Scopes  []string   `json:"scopes"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires"`
}

func readTokenFile(t *testing.T, dir string) ([]byte, []tokenFileRecord) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Tokens []tokenFileRecord `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("tokens.json is not valid JSON: %v\n%s", err, data)
	}
	return data, f.Tokens
}

// TC-25: tokens are created once, random and hashed at rest.
func TestTokenCreateListHashedAtRest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	before := time.Now().Add(-time.Second)
	tokA, idA, secA := createToken(t, dir, "--name", "a")
	tokB, idB, secB := createToken(t, dir, "--name", "b", "--scope", "containers:read", "--expires", "24h")
	after := time.Now().Add(time.Second)
	if tokA == tokB || idA == idB || bytes.Equal(secA, secB) {
		t.Fatalf("tokens are not distinct: %s %s", tokA, tokB)
	}

	data, recs := readTokenFile(t, dir)
	if len(recs) != 2 {
		t.Fatalf("tokens.json holds %d tokens, want 2", len(recs))
	}
	want := map[string]struct {
		name, tok string
		secret    []byte
		scopes    []string
		expires   bool
	}{
		idA: {"a", tokA, secA, []string{}, false},
		idB: {"b", tokB, secB, []string{"containers:read"}, true},
	}
	for _, r := range recs {
		w, ok := want[r.ID]
		if !ok {
			t.Fatalf("unexpected token id %q", r.ID)
		}
		sum := sha256.Sum256(w.secret)
		if r.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: sha256 %q, want the SHA-256 of the printed secret", w.name, r.SHA256)
		}
		if r.Name != w.name || !slices.Equal(r.Scopes, w.scopes) || r.Scopes == nil {
			t.Errorf("%s: record %+v, want name %q scopes %q", w.name, r, w.name, w.scopes)
		}
		if r.Created.Before(before) || r.Created.After(after) {
			t.Errorf("%s: created %v outside the run", w.name, r.Created)
		}
		switch {
		case w.expires && (r.Expires == nil || r.Expires.Sub(r.Created) != 24*time.Hour):
			t.Errorf("%s: expires %v, want created+24h", w.name, r.Expires)
		case !w.expires && r.Expires != nil:
			t.Errorf("%s: expires %v, want none", w.name, r.Expires)
		}
		enc := w.tok[17:]
		if bytes.Contains(data, []byte(enc)) || bytes.Contains(data, w.secret) {
			t.Errorf("%s: tokens.json holds the raw secret", w.name)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tokens.json mode %04o, want 0600", perm)
	}

	out, errOut, code := runToken(t, dir, "list")
	if code != 0 {
		t.Fatalf("token list exited %d: %s", code, errOut)
	}
	for _, s := range []string{"a", "b", idA, idB, "containers:read"} {
		if !strings.Contains(out, s) {
			t.Errorf("token list lacks %q:\n%s", s, out)
		}
	}
	for _, r := range recs {
		if strings.Contains(out+errOut, r.SHA256) {
			t.Error("token list prints a hash")
		}
	}
	for _, tok := range []string{tokA, tokB} {
		if strings.Contains(out+errOut, tok[17:]) {
			t.Error("token list prints a secret")
		}
	}
}

func TestTokenCommandUsageErrors(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{},
		{"create"},
		{"create", "--name", "x", "--expires", "soon"},
		{"create", "--name", "x", "--expires", "-1h"},
		{"create", "--name", "x", "extra"},
		{"revoke"},
		{"mint"},
	} {
		out, _, code := runToken(t, dir, args...)
		if code == 0 {
			t.Errorf("token %v exited 0", args)
		}
		if strings.Contains(out, "nxs_") {
			t.Errorf("token %v printed a token on failure", args)
		}
	}
	if _, errOut, code := runToken(t, dir, "revoke", "nobody"); code == 0 {
		t.Error("revoking an unknown token exited 0")
	} else if !strings.Contains(errOut, "nobody") {
		t.Errorf("revoke error does not name the token: %s", errOut)
	}
}

// TC-E3: concurrent token writes lose nothing.
func TestConcurrentTokenCreates(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	tokens := make([]string, n)
	var stopRead atomic.Bool
	readerDone := make(chan error, 1)
	var reads atomic.Int64
	go func() {
		// A concurrent reader must never see a partial file.
		path := filepath.Join(dir, "tokens.json")
		for !stopRead.Load() {
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				readerDone <- err
				return
			}
			if !json.Valid(data) {
				readerDone <- fmt.Errorf("partial tokens.json observed: %q", data)
				return
			}
			reads.Add(1)
		}
		readerDone <- nil
	}()

	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			cmd := exec.Command(nexusdBin, "token", "create", "--name", fmt.Sprintf("c%02d", i), "--state-dir", dir)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				errs <- fmt.Sprintf("create c%02d: %v: %s", i, err, stderr.String())
				return
			}
			tokens[i] = strings.TrimSpace(stdout.String())
		})
	}
	wg.Wait()
	stopRead.Store(true)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 {
		t.Fatal("the concurrent reader never read the file")
	}

	_, recs := readTokenFile(t, dir)
	if len(recs) != n {
		t.Fatalf("tokens.json holds %d tokens, want %d", len(recs), n)
	}
	out, _, code := runToken(t, dir, "list")
	if code != 0 {
		t.Fatalf("token list exited %d", code)
	}
	a := auth.New(dir, auth.Options{})
	for i, tok := range tokens {
		name := fmt.Sprintf("c%02d", i)
		if !strings.Contains(out, name) {
			t.Errorf("token list lacks %s", name)
		}
		p, err := a.Authenticate(tok)
		if err != nil || p.Name != name {
			t.Errorf("token %s does not authenticate: %+v %v", name, p, err)
		}
	}
}

// startServeWithState runs nexusd serve with its state in dir.
func startServeWithState(t *testing.T, dir string) *serveProc {
	t.Helper()
	addr := freeAddr(t)
	cfg := writeConfig(t, "nexusd.yaml", "listen: [\""+addr+"\"]\ntls: {mode: off}\n")
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", dir, "--audit-dir", t.TempDir())
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

func getWith(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TC-27: revocation applies on the next request without restart.
func TestRevocationWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	p := startServeWithState(t, dir)
	p.waitHealthy(t)
	// The daemon does not open state.db until the operation manager (3.2)
	// lands, so this process holds the bbolt lock in its place: the token
	// commands below must not touch it. Once serve holds state.db itself,
	// drop this stand-in.
	db, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	url := "http://" + p.addr + "/v1/capabilities"
	start := time.Now()
	tok, id, _ := createToken(t, dir, "--name", "live")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("token create took %v with state.db locked; it must not wait on bbolt", d)
	}
	if code := getWith(t, url, tok); code != http.StatusOK {
		t.Fatalf("new token: %d, want 200\n%s", code, p.output())
	}
	start = time.Now()
	if _, errOut, code := runToken(t, dir, "revoke", id); code != 0 {
		t.Fatalf("token revoke exited %d: %s", code, errOut)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("token revoke took %v with state.db locked", d)
	}
	if code := getWith(t, url, tok); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", code)
	}
	if strings.Contains(p.output(), tok[17:]) {
		t.Fatal("the daemon logged the token secret")
	}
	if code, _ := p.terminate(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, auth.TokenFileName)); err != nil {
		t.Fatal(err)
	}
}
