package nexusd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// tokenWindow is the rolling window of the remote token rate limit (A-1).
const tokenWindow = time.Minute

// Audit facts of the token ops (FR-13). Never the secret or its hash.
const (
	factTokenID     = "tokenId"
	factTokenName   = "tokenName"
	factScopes      = "scopes"
	factExpires     = "expires"
	factRevokedSelf = "revokedSelf"
	expiresNever    = "never"
)

// tokenGuard serializes the daemon's remote token mutations and bounds their
// successes to spec.TokenMutationsPerMinute per rolling tokenWindow (FR-5,
// A-1). Only successes count: a refused add or remove, and any list, leave
// the window as it was.
type tokenGuard struct {
	mu        sync.Mutex
	successes []time.Time // Oldest first
}

// begin locks the guard and prunes the window as of now. When the window is
// full, it unlocks and returns rate-limited with RetryAfter set to when the
// oldest success leaves it; otherwise the caller holds the lock and must
// call end.
func (g *tokenGuard) begin(now time.Time) error {
	g.mu.Lock()
	// A success exactly tokenWindow old still counts: the window is inclusive.
	i := 0
	for i < len(g.successes) && now.Sub(g.successes[i]) > tokenWindow {
		i++
	}
	g.successes = append(g.successes[:0], g.successes[i:]...)
	if len(g.successes) < spec.TokenMutationsPerMinute {
		return nil
	}
	wait := max(g.successes[0].Add(tokenWindow).Sub(now), time.Second)
	g.mu.Unlock()
	p := problem.New(problem.RateLimited, fmt.Sprintf("%d token adds and removes succeeded on this daemon in "+
		"the last minute, the most allowed; retry later", spec.TokenMutationsPerMinute))
	p.RetryAfter = wait
	return p
}

// end records a success at at when ok, and unlocks the guard.
func (g *tokenGuard) end(ok bool, at time.Time) {
	if ok {
		g.successes = append(g.successes, at)
	}
	g.mu.Unlock()
}

// tokensList returns every token of the host, local ones included, marking
// the one this request used (F2).
func (p *Provider) tokensList(ctx context.Context, _ spec.TokenListInput) (apitypes.List[spec.Token], error) {
	d, err := p.daemonOf()
	if err != nil {
		return apitypes.List[spec.Token]{}, err
	}
	toks, err := auth.List(d.StateDir())
	if err != nil {
		return apitypes.List[spec.Token]{}, fmt.Errorf("nexusd: list tokens: %w", err)
	}
	caller, _ := auth.PrincipalFrom(ctx)
	items := make([]spec.Token, 0, len(toks))
	for _, t := range toks {
		props := spec.TokenProperties{
			TokenID: t.ID,
			Scopes:  t.Scopes,
			Created: t.Created,
			Expires: t.Expires,
			Self:    caller.TokenID != "" && t.ID == caller.TokenID,
		}
		if t.CreatedBy != nil {
			props.CreatedBy = &spec.TokenRef{TokenID: t.CreatedBy.ID, Name: t.CreatedBy.Name}
		}
		items = append(items, apitypes.NewResource(p.host, spec.Name, spec.TokensType, t.Name, props))
	}
	return apitypes.List[spec.Token]{Items: items}, nil
}

// tokensAdd creates a token for the caller and answers with its secret, the
// only copy there is (F1, CON-3): the store keeps its hash, and neither the
// secret nor the hash reaches a fact, a log line or an error.
func (p *Provider) tokensAdd(ctx context.Context, in spec.TokenAddInput) (spec.TokenAddResult, error) {
	var none spec.TokenAddResult
	d, err := p.daemonOf()
	if err != nil {
		return none, err
	}
	if err := p.tokens.begin(p.now()); err != nil {
		return none, err
	}
	ok := false
	defer func() { p.tokens.end(ok, p.now()) }()

	ttl, err := auth.ParseLifetime(in.Expires)
	if err != nil {
		return none, problem.New(problem.ValidationFailed, "the input is invalid").
			With("errors", []problem.FieldError{{Path: "/expires", Message: err.Error()}})
	}
	req := auth.CreateRequest{Name: in.Name, Scopes: in.Scopes, TTL: ttl, Now: p.now(),
		MaxTokens: spec.MaxRemoteTokens}
	if caller, has := auth.PrincipalFrom(ctx); has && caller.TokenID != "" {
		req.CreatedBy = &auth.TokenRef{ID: caller.TokenID, Name: caller.Name}
	}
	secret, tok, err := auth.Create(d.StateDir(), req)
	if err != nil {
		var exists *auth.NameExistsError
		var limit *auth.LimitError
		switch {
		case errors.As(err, &exists):
			// The supplied name is not echoed: a near-variant of a secret
			// that the name pattern lets through must not reach a body.
			return none, problem.New(problem.Conflict,
				fmt.Sprintf("a token with this name already exists (id %s); revoke it first or choose another name", exists.ID))
		case errors.As(err, &limit):
			return none, problem.New(problem.TokenLimitReached, limit.Error()).With("maxTokens", limit.Max)
		}
		return none, fmt.Errorf("nexusd: create token: %w", err)
	}

	expires := expiresNever
	if tok.Expires != nil {
		expires = tok.Expires.UTC().Format(time.RFC3339)
	}
	audit.AddFact(ctx, factTokenID, tok.ID)
	audit.AddFact(ctx, factTokenName, tok.Name)
	audit.AddFact(ctx, factScopes, strings.Join(tok.Scopes, ","))
	audit.AddFact(ctx, factExpires, expires)
	ok = true
	return spec.TokenAddResult{
		ActionResult: apitypes.ActionResult{Changed: true},
		Token:        secret,
		TokenID:      tok.ID,
		Name:         tok.Name,
		Scopes:       tok.Scopes,
		Created:      tok.Created,
		Expires:      tok.Expires,
	}, nil
}

// tokensRemove revokes the token whose name or ID is in.Name (F3). Revoking
// the token this request used answers normally, with a warning: the request
// already authenticated, and the next one is refused.
func (p *Provider) tokensRemove(ctx context.Context, in spec.TokenRemoveInput) (spec.TokenRemoveResult, error) {
	var none spec.TokenRemoveResult
	d, err := p.daemonOf()
	if err != nil {
		return none, err
	}
	if err := p.tokens.begin(p.now()); err != nil {
		return none, err
	}
	ok := false
	defer func() { p.tokens.end(ok, p.now()) }()

	tok, err := auth.Revoke(d.StateDir(), in.Name)
	if err != nil {
		var nf *auth.NotFoundError
		if errors.As(err, &nf) {
			// The supplied reference is not echoed, for the reason above.
			return none, problem.New(problem.NotFound, "no token has this name or ID")
		}
		return none, fmt.Errorf("nexusd: revoke token: %w", err)
	}
	caller, _ := auth.PrincipalFrom(ctx)
	self := caller.TokenID != "" && tok.ID == caller.TokenID
	res := spec.TokenRemoveResult{
		ActionResult: apitypes.ActionResult{Changed: true},
		TokenID:      tok.ID,
		Name:         tok.Name,
		RevokedSelf:  self,
	}
	if self {
		res.Warnings = []string{fmt.Sprintf("the token this request used (%s, id %s) is revoked: this client "+
			"can no longer authenticate to this host with it", tok.Name, tok.ID)}
	}
	audit.AddFact(ctx, factTokenID, tok.ID)
	audit.AddFact(ctx, factTokenName, tok.Name)
	audit.AddFact(ctx, factRevokedSelf, strconv.FormatBool(self))
	ok = true
	return res, nil
}
