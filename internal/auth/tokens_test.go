package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

var tokenRE = regexp.MustCompile(`^nxs_([0-9a-f]{12})_([A-Za-z0-9_-]{43})$`)

func setUmask(m int) int { return syscall.Umask(m) }

func quietLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func mustCreate(t *testing.T, dir string, req CreateRequest) (string, Token) {
	t.Helper()
	tok, rec, err := Create(dir, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return tok, rec
}

// TC-E2: token expiry boundary.
func TestTokenExpiryBoundary(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tok, rec := mustCreate(t, dir, CreateRequest{Name: "e", TTL: 24 * time.Hour, Now: expiry.Add(-24 * time.Hour)})
	if rec.Expires == nil || !rec.Expires.Equal(expiry) {
		t.Fatalf("Expires = %v, want %v", rec.Expires, expiry)
	}
	clk := &fakeClock{}
	a := New(dir, Options{Now: clk.Now, Logger: quietLog()})
	for _, c := range []struct {
		at   time.Time
		want error
	}{
		{expiry.Add(-time.Second), nil},
		{expiry, ErrInvalidToken},
		{expiry.Add(time.Second), ErrInvalidToken},
	} {
		clk.t = c.at
		p, err := a.Authenticate(tok)
		if !errors.Is(err, c.want) {
			t.Fatalf("at expiry%+v: err = %v, want %v", c.at.Sub(expiry), err, c.want)
		}
		if c.want == nil && p.TokenID != rec.ID {
			t.Fatalf("principal %+v, want token %s", p, rec.ID)
		}
	}
}

func TestCreateFormatHashAndDefaults(t *testing.T) {
	dir := t.TempDir()
	tok, rec := mustCreate(t, dir, CreateRequest{Name: "plain"})
	m := tokenRE.FindStringSubmatch(tok)
	if m == nil {
		t.Fatalf("token %q does not match nxs_<id>_<secret>", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(m[2])
	if err != nil || len(raw) != 32 {
		t.Fatalf("secret decodes to %d bytes (%v), want 32", len(raw), err)
	}
	if rec.ID != m[1] || len(rec.Scopes) != 0 || rec.Expires != nil {
		t.Fatalf("record %+v: want id %s, no scopes, no expiry", rec, m[1])
	}
	data, err := os.ReadFile(filepath.Join(dir, TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if !bytes.Contains(data, []byte(hex.EncodeToString(sum[:]))) {
		t.Fatalf("tokens.json lacks the SHA-256 of the secret: %s", data)
	}
	if bytes.Contains(data, []byte(m[2])) {
		t.Fatal("tokens.json holds the raw secret")
	}
	var f struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil || len(f.Tokens) != 1 {
		t.Fatalf("tokens.json = %s (%v)", data, err)
	}
	if s, ok := f.Tokens[0]["scopes"].([]any); !ok || len(s) != 0 {
		t.Fatalf("scopes = %#v, want an empty list", f.Tokens[0]["scopes"])
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	for _, req := range []CreateRequest{
		{Name: ""},
		{Name: "has space"},
		{Name: "-dash"},
		{Name: "ok", Scopes: []string{"Containers:read"}},
		{Name: "ok", Scopes: []string{"containers"}},
		{Name: "ok", Scopes: []string{"containers:read; rm"}},
		{Name: "ok", TTL: -time.Hour},
	} {
		if _, _, err := Create(dir, req); err == nil {
			t.Errorf("Create(%+v) succeeded", req)
		}
	}
	mustCreate(t, dir, CreateRequest{Name: "dup"})
	if _, _, err := Create(dir, CreateRequest{Name: "dup"}); err == nil {
		t.Error("a second token with the same name was created")
	}
	mustCreate(t, dir, CreateRequest{Name: "s", Scopes: []string{"containers:read", "scripts:run:deploy-api", "containers:read"}})
}

func TestTokenFileModeIs0600(t *testing.T) {
	dir := t.TempDir()
	old := setUmask(0)
	defer setUmask(old)
	mustCreate(t, dir, CreateRequest{Name: "m"})
	fi, err := os.Stat(filepath.Join(dir, TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != TokenFileMode {
		t.Fatalf("tokens.json mode %04o, want %04o", perm, TokenFileMode)
	}
}

// A tokens.json that others can read or write fails closed: a writable file
// would let anyone add a token.
func TestLooseTokenFileFailsClosed(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602} {
		dir := t.TempDir()
		tok, _ := mustCreate(t, dir, CreateRequest{Name: "p"})
		a := New(dir, Options{Logger: quietLog()})
		if _, err := a.Authenticate(tok); err != nil {
			t.Fatalf("0600 file: %v", err)
		}
		path := filepath.Join(dir, TokenFileName)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Authenticate(tok); !errors.Is(err, ErrTokenStoreUnavailable) {
			t.Fatalf("mode %04o: err = %v, want ErrTokenStoreUnavailable", mode, err)
		}
		if _, err := List(dir); err == nil {
			t.Fatalf("mode %04o: List succeeded", mode)
		}
		if _, _, err := Create(dir, CreateRequest{Name: "q"}); err == nil {
			t.Fatalf("mode %04o: Create succeeded over a loose file", mode)
		}
	}
}

func TestCheckTokenFileOwnerWhenRoot(t *testing.T) {
	dir := t.TempDir()
	mustCreate(t, dir, CreateRequest{Name: "o"})
	fi, err := os.Stat(filepath.Join(dir, TokenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("the file is root-owned when the test runs as root")
	}
	if err := checkTokenFile(fi, os.Geteuid()); err != nil {
		t.Fatalf("own file, not root: %v", err)
	}
	if err := checkTokenFile(fi, 0); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("running as root, a non-root-owned tokens.json: err = %v, want refusal", err)
	}
}

func TestMalformedTokensAreInvalid(t *testing.T) {
	dir := t.TempDir()
	tok, _ := mustCreate(t, dir, CreateRequest{Name: "x"})
	a := New(dir, Options{Logger: quietLog()})
	id, secret := tok[4:16], tok[17:]
	// Setting an unused low bit of the final character gives a non-canonical
	// encoding of the same 32 bytes, which must not be accepted.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	alt := alphabet[strings.IndexByte(alphabet, secret[len(secret)-1])|1]
	for _, bad := range []string{
		"", "nxs_", tok[:len(tok)-1], tok + "A", "NXS_" + tok[4:], "nxs_" + strings.ToUpper(id) + "_" + secret,
		"nxs_" + id + "_" + secret[:42] + string(alt),
		"nxs_" + id + "_" + secret + "=", "nxs_" + id + secret,
	} {
		if bad == tok {
			continue
		}
		if _, err := a.Authenticate(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Authenticate(%q) err = %v, want ErrInvalidToken", bad, err)
		}
	}
}

func TestRevokeAndListNeverExposeSecrets(t *testing.T) {
	dir := t.TempDir()
	tok, rec := mustCreate(t, dir, CreateRequest{Name: "r", Scopes: []string{"audit:read"}})
	a := New(dir, Options{Logger: quietLog()})
	if _, err := a.Authenticate(tok); err != nil {
		t.Fatal(err)
	}
	list, err := List(dir)
	if err != nil || len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if _, err := Revoke(dir, "nope"); err == nil {
		t.Fatal("revoking an unknown token succeeded")
	}
	if _, err := Revoke(dir, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token: err = %v", err)
	}
	if list, err := List(dir); err != nil || len(list) != 0 {
		t.Fatalf("after revoke List = %+v, %v", list, err)
	}
}

func TestMissingTokenFileRefusesEverything(t *testing.T) {
	a := New(filepath.Join(t.TempDir(), "absent"), Options{Logger: quietLog()})
	if _, err := a.Authenticate("nxs_000000000000_" + strings.Repeat("A", 43)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

// The store's typed errors stay matchable with errors.As when a caller wraps
// them, which is how the nexusd provider maps them to problem types.
func TestTypedErrorsMatchThroughWrapping(t *testing.T) {
	dir := t.TempDir()
	_, a := mustCreate(t, dir, CreateRequest{Name: "a"})

	_, _, err := Create(dir, CreateRequest{Name: "a"})
	var ne *NameExistsError
	if !errors.As(fmt.Errorf("add: %w", err), &ne) || ne.Name != "a" || ne.ID != a.ID {
		t.Errorf("duplicate name: err = %v, want a wrapped *NameExistsError", err)
	}
	_, err = Revoke(dir, "absent")
	var nf *NotFoundError
	if !errors.As(fmt.Errorf("remove: %w", err), &nf) || nf.Ref != "absent" {
		t.Errorf("unknown ref: err = %v, want a wrapped *NotFoundError", err)
	}
	_, _, err = Create(dir, CreateRequest{Name: "b", MaxTokens: 1})
	var le *LimitError
	if !errors.As(fmt.Errorf("add: %w", err), &le) || le.Max != 1 {
		t.Errorf("cap: err = %v, want a wrapped *LimitError", err)
	}
	if want := "the token store already holds 1 tokens, the most allowed; revoke one first"; err == nil || err.Error() != want {
		t.Errorf("LimitError text = %v, want %q", err, want)
	}
}
