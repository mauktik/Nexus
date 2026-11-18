package auth

import (
	"errors"
	"strings"
	"testing"
)

// The token-name rule everywhere (user decision at confirm-before-merge,
// 2026-11-17; SEC-1, CR-16): the local create refuses a name that looks
// like, or contains, a token string, with the remote op's message and
// without echoing the value. Each variant is derived from a minted token.
func TestCreateRefusesANameCarryingATokenString(t *testing.T) {
	dir := t.TempDir()
	tok, info := mustCreate(t, dir, CreateRequest{Name: "src"})
	id, secret := info.ID, strings.SplitN(tok, "_", 3)[2]
	if len(secret) != encodedLen {
		t.Fatalf("secret %q is not %d characters", secret, encodedLen)
	}
	for _, c := range []struct{ what, name string }{
		{"canonical", tok},
		{"no prefix", id + "_" + secret},
		{"upper-case prefix", strings.ToUpper(tok[:4]) + id + "_" + secret},
		{"mixed-case prefix and ID", "Nxs_" + strings.ToUpper(id) + "_" + secret},
		{"bare secret", secret},
		{"secret inside a longer name", "ci-" + secret},
		{"secret then a suffix", secret + "-x"},
		{"prefix and ID with a short tail", "NXS_" + id + "_x"},
		{"longer than 64 characters", "ci-" + secret + "-" + strings.Repeat("x", 30)},
	} {
		_, _, err := Create(dir, CreateRequest{Name: c.name})
		switch {
		case err == nil:
			t.Errorf("%s: Create accepted a name carrying a token string", c.what)
		case !strings.Contains(err.Error(), TokenStringRefusal):
			t.Errorf("%s: Create error %q, want the refusal %q", c.what, err, TokenStringRefusal)
		case strings.Contains(err.Error(), c.name) || strings.Contains(err.Error(), secret) ||
			strings.Contains(err.Error(), id+"_"):
			t.Errorf("%s: Create error echoes the value: %q", c.what, err)
		}

		r := runTokenCommand("create", "--name", c.name, "--state-dir", dir)
		if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, TokenStringRefusal) {
			t.Errorf("%s: token create code %d stdout %q stderr %q, want 1, nothing and the refusal", c.what, r.code,
				r.stdout, r.stderr)
		}
		if strings.Contains(r.stderr, secret) || strings.Contains(r.stderr, id+"_") {
			t.Errorf("%s: token create stderr echoes the value: %q", c.what, r.stderr)
		}
	}
	if toks, err := List(dir); err != nil || len(toks) != 1 || toks[0].ID != id {
		t.Errorf("List = %+v, %v; want only src", toks, err)
	}

	// Names and near misses that give nothing away are still created.
	for _, name := range []string{"ci", "abcdefabcdef", "nxs_ci", "nxs_0123456789ab", "nxs_0123456789ab-short",
		strings.Repeat("a", 42), strings.Repeat("a", 21) + "." + strings.Repeat("a", 21), strings.Repeat("a.", 32)} {
		if _, _, err := Create(dir, CreateRequest{Name: name}); err != nil {
			t.Errorf("Create refuses the name %q: %v", name, err)
		}
	}
}

// A token stored under such a name before the rule (here written straight
// into the store) keeps working: it authenticates, list shows it, and
// revoke by its ID removes it.
func TestAStoredTokenStringNameKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	tok, info := mustCreate(t, dir, CreateRequest{Name: "placeholder", Scopes: []string{"audit:read"}})
	other, _ := mustCreate(t, dir, CreateRequest{Name: "other"})
	legacy := "ci-" + strings.Repeat("Ab0_-", 9)
	if err := update(dir, func(recs []record) ([]record, error) {
		for i := range recs {
			if recs[i].ID == info.ID {
				recs[i].Name = legacy
			}
		}
		return recs, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Create(dir, CreateRequest{Name: legacy}); err == nil {
		t.Fatal("Create accepts the legacy name, so this fixture does not test the rule")
	}

	a := New(dir, Options{Logger: quietLog()})
	if p, err := a.Authenticate(tok); err != nil || p.TokenID != info.ID {
		t.Fatalf("Authenticate = %+v, %v; want the legacy-named token", p, err)
	}
	toks, err := List(dir)
	if err != nil || len(toks) != 2 || toks[0].Name != legacy {
		t.Fatalf("List = %+v, %v; want the legacy name listed", toks, err)
	}
	if r := runTokenCommand("list", "--state-dir", dir); r.code != 0 || !strings.Contains(r.stdout, legacy) {
		t.Errorf("token list code %d stdout %q, want the legacy name", r.code, r.stdout)
	}
	if r := runTokenCommand("revoke", info.ID, "--state-dir", dir); r.code != 0 {
		t.Fatalf("token revoke by ID: code %d stderr %q", r.code, r.stderr)
	}
	if _, err := a.Authenticate(tok); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the revoked token still authenticates: %v", err)
	}
	if _, err := a.Authenticate(other); err != nil {
		t.Errorf("the other token no longer authenticates: %v", err)
	}
}
