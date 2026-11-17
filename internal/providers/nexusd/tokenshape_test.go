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
	before := s.storeSum(t)
	for _, c := range []struct {
		action, tok string
		call        func(t *testing.T, token, name string) response
	}{
		{spec.TokensRemove.Op().ID, s.admin, s.remove},
		{spec.TokensRemove.Op().ID, unknown, s.remove},
		{spec.TokensAdd.Op().ID, s.admin, func(t *testing.T, token, name string) response { return s.add(t, token, name, "") }},
		{spec.TokensAdd.Op().ID, unknown, func(t *testing.T, token, name string) response { return s.add(t, token, name, "") }},
	} {
		r := c.call(t, s.admin, c.tok)
		wantTokenProblem(t, r, http.StatusBadRequest, problem.ValidationFailed)
		if !strings.Contains(string(r.body), `"path":"/name"`) {
			t.Errorf("%s: errors[] does not name /name: %s", c.action, r.body)
		}
		noSecretIn(t, c.action+" problem", r.body, c.tok)
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
		noSecretIn(t, "audit file "+filepath.Base(p), b, s.admin, unknown)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	noSecretIn(t, "daemon log", []byte(s.logs.String()), s.admin, unknown)
}
