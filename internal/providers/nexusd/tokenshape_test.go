// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// TC-RA-E10's server half (NFR-1, WI 2.2 amendment): a full token string
// sent as the {name} of nexusd.tokens.remove or nexusd.tokens.add is
// refused with 400 validation-failed, and the string appears neither in the
// problem, the audit log on disk, nor the daemon's log.

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

func TestTCRAE10_TokenStringAsNameIsRefusedWithoutEchoingIt(t *testing.T) {
	s := newTokenStack(t)
	// The admin's own token, and a well-formed token no store holds.
	unknown := "nxs_0123456789ab_" + strings.Repeat("A", 42) + "E"
	// SEC-1: every variant of a live token's secret that a name could carry.
	minted := s.added(t, "minted", "")
	id, secret := minted.TokenID, strings.SplitN(minted.Token, "_", 3)[2]
	variants := []string{
		minted.Token,
		id + "_" + secret,
		"NXS_" + id + "_" + secret,
		"Nxs_" + strings.ToUpper(id) + "_" + secret,
		secret,
		"ci-" + secret,
		secret + "-x",
	}
	// Secrets that start with "-" and "_" also fail the name's own pattern,
	// so these cases are fixed, not left to the minted secret's first
	// character: the client must still be told the refusal, not the pattern.
	for _, fixed := range []string{"-" + strings.Repeat("aB3_", 10) + "xy", "_" + strings.Repeat("-Z9a", 10) + "xy"} {
		variants = append(variants, fixed, "ci-"+fixed, fixed+"-x", id+"_"+fixed, "NXS_"+id+"_"+fixed)
	}
	needles := append([]string{s.admin, unknown, minted.Token, secret}, variants...)
	before := s.storeSum(t)
	type call struct {
		action, name string
		call         func(t *testing.T, token, name string) response
	}
	addCall := func(t *testing.T, token, name string) response { return s.add(t, token, name, "") }
	calls := []call{
		{spec.TokensRemove.Op().ID, s.admin, s.remove},
		{spec.TokensRemove.Op().ID, unknown, s.remove},
		{spec.TokensAdd.Op().ID, s.admin, addCall},
		{spec.TokensAdd.Op().ID, unknown, addCall},
	}
	for _, v := range variants {
		calls = append(calls, call{spec.TokensRemove.Op().ID, v, s.remove}, call{spec.TokensAdd.Op().ID, v, addCall})
	}
	for _, c := range calls {
		r := c.call(t, s.admin, c.name)
		wantTokenProblem(t, r, http.StatusBadRequest, problem.ValidationFailed)
		if !strings.Contains(string(r.body), `"path":"/name"`) {
			t.Errorf("%s: errors[] does not name /name: %s", c.action, r.body)
		}
		// The refusal's own message, not the property's generic description.
		if !strings.Contains(string(r.body), "looks like, or contains, a token string") {
			t.Errorf("%s: the refusal's message is missing: %s", c.action, r.body)
		}
		noSecretIn(t, c.action+" problem", r.body, needles...)
		recs := s.auditOf(t, c.action)
		if len(recs) == 0 {
			t.Fatalf("%s: no audit record", c.action)
		}
		last := recs[len(recs)-1]
		if last.Outcome != audit.OutcomeRefused || last.Problem != string(problem.ValidationFailed) ||
			last.Target != "" || len(last.Params) != 0 {
			t.Errorf("%s: refused record %+v, want refused validation-failed with no target or params", c.action, last)
		}
	}
	if after := s.storeSum(t); after != before {
		t.Error("the token store changed")
	}
	err := filepath.WalkDir(s.auditDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		noSecretIn(t, "audit file "+filepath.Base(p), b, needles...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	noSecretIn(t, "daemon log", []byte(s.logs.String()), needles...)
	for _, tk := range s.tokens(t) {
		if strings.Contains(tk.Name, secret) {
			t.Errorf("a stored token is named with the secret: %q", tk.Name)
		}
	}
}

// SEC-1: a supplied name is never echoed by the not-found and conflict
// problems of the token ops, so a near-variant of a secret that the name
// schema lets through cannot reach a body, the audit log or the daemon log.
func TestTokenOpsDoNotEchoTheSuppliedName(t *testing.T) {
	s := newTokenStack(t)
	const name = "echo-probe-name"
	const missing = "missing-probe-name"
	s.added(t, name, "")
	dup := s.add(t, s.admin, name, "")
	m := wantTokenProblem(t, dup, http.StatusConflict, problem.Conflict)
	nf := s.remove(t, s.admin, missing)
	wantTokenProblem(t, nf, http.StatusNotFound, problem.NotFound)
	for what, r := range map[string]response{"conflict": dup, "not-found": nf} {
		noSecretIn(t, what+" problem", r.body, name, missing)
	}
	if d, _ := m["detail"].(string); !strings.Contains(d, s.idOf(t, name)) {
		t.Errorf("conflict detail %q does not name the existing token's ID", d)
	}
	noSecretIn(t, "daemon log", []byte(s.logs.String()), "\""+name+"\"", "\""+missing+"\"")
}
