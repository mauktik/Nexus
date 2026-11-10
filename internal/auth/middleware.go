package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// ErrInvalidToken means the presented credentials do not authenticate. It
// deliberately does not say why: unknown ID, wrong secret, expiry and
// malformed input are indistinguishable to a client.
var ErrInvalidToken = errors.New("invalid token")

// ErrTokenStoreUnavailable means tokens.json cannot be used (unreadable,
// corrupt, or with unsafe permissions), so nothing authenticates.
var ErrTokenStoreUnavailable = errors.New("token store unavailable")

// Principal is an authenticated caller.
type Principal struct {
	TokenID string
	Name    string
	Scopes  []string
	Source  netip.Addr // From SourceAddr
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal the middleware attached to ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Options configures New.
type Options struct {
	// Now drives token expiry and rate-limit windows. Nil means time.Now.
	Now func() time.Time
	// FailureLimitPerMinute is auth.failureLimitPerMinute. Zero means
	// DefaultFailureLimitPerMinute.
	FailureLimitPerMinute int
	// Logger receives authentication failures and token file errors. Nil
	// means slog.Default().
	Logger *slog.Logger
	// Audit records every denied request (FR-24). Nil means denials are
	// only logged.
	Audit Auditor
}

// Auditor is what the middleware needs of the audit log; *audit.Log
// implements it.
type Auditor interface {
	// Write appends one record durably.
	Write(rec systemspec.AuditRecord, sensitive []string) (string, error)
	// RateLimited counts a denial of a rate-limited source into that
	// source's aggregate record for the window (A-47).
	RateLimited(source, surface string)
}

// Authenticator checks bearer tokens against tokens.json in a state
// directory. It stats the file on every request and reloads it when its
// size, modification time or inode changes, so `nexusd token revoke` applies
// to the next request without a restart or signal.
type Authenticator struct {
	path    string
	now     func() time.Time
	log     *slog.Logger
	limiter *Limiter
	audit   Auditor
	euid    int

	mu     sync.Mutex
	loaded bool
	key    fileKey
	byID   map[string]entry
	err    error // Non-nil while the current file is unusable
}

type entry struct {
	rec  record
	hash [sha256.Size]byte
}

// fileKey identifies one version of tokens.json without reading it: size,
// modification time and inode, plus mode and owner, so a chmod or chown that
// makes the file unsafe is noticed on the next request too.
type fileKey struct {
	exists bool
	size   int64
	mtime  time.Time
	mode   fs.FileMode
	dev    uint64
	ino    uint64
	uid    uint32
}

// New returns an authenticator for the token store in dir.
func New(dir string, opts Options) *Authenticator {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Authenticator{
		path:    filepath.Join(dir, TokenFileName),
		now:     opts.Now,
		log:     opts.Logger,
		limiter: NewLimiter(opts.FailureLimitPerMinute, opts.Now),
		audit:   opts.Audit,
		euid:    os.Geteuid(),
	}
}

// dummyHash is compared when the ID is unknown, so an unknown ID costs the
// same work as a wrong secret.
var dummyHash [sha256.Size]byte

// Authenticate checks a token string. It returns ErrInvalidToken or
// ErrTokenStoreUnavailable on failure; neither error includes the token.
func (a *Authenticator) Authenticate(tok string) (Principal, error) {
	id, raw, ok := parseToken(tok)
	byID, err := a.tokens()
	if err != nil {
		return Principal{}, ErrTokenStoreUnavailable
	}
	if !ok {
		return Principal{}, ErrInvalidToken
	}
	e, known := byID[id]
	want := dummyHash
	if known {
		want = e.hash
	}
	got := sha256.Sum256(raw)
	match := subtle.ConstantTimeCompare(got[:], want[:]) == 1
	if !known || !match {
		return Principal{}, ErrInvalidToken
	}
	if e.rec.Expires != nil && !a.now().Before(*e.rec.Expires) {
		return Principal{}, ErrInvalidToken
	}
	scopes := slices.Clone(e.rec.Scopes)
	if scopes == nil {
		scopes = []string{}
	}
	return Principal{TokenID: e.rec.ID, Name: e.rec.Name, Scopes: scopes}, nil
}

// tokens returns the current token set, reloading tokens.json if it changed.
func (a *Authenticator) tokens() (map[string]entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key, err := statKey(a.path)
	if err != nil {
		if a.loaded || a.err == nil {
			a.log.Error("cannot stat the token file; refusing every token until it is fixed", "path", a.path, "error", err.Error())
		}
		a.loaded, a.byID, a.err = false, nil, err
		return nil, err
	}
	if a.loaded && key == a.key {
		return a.byID, a.err
	}
	a.key, a.loaded = key, true
	a.byID, a.err = a.load()
	if a.err != nil {
		// Logged once per version of the file, not once per request.
		a.log.Error("token file unusable; refusing every token until it is fixed", "path", a.path, "error", a.err.Error())
		a.byID = nil
		return nil, a.err
	}
	if !key.exists {
		a.log.Warn("no token file; every request needing a token is refused until `nexusd token create` runs", "path", a.path)
	}
	return a.byID, nil
}

// load reads the file through one descriptor and keys the cache by that
// descriptor's stat, so the key always describes the bytes that were read.
func (a *Authenticator) load() (map[string]entry, error) {
	f, err := os.Open(a.path)
	if errors.Is(err, fs.ErrNotExist) {
		a.key = fileKey{}
		return map[string]entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	a.key = keyOf(fi)
	recs, hashes, err := decodeChecked(f, fi, a.path, a.euid)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]entry, len(recs))
	for i, r := range recs {
		byID[r.ID] = entry{rec: r, hash: hashes[i]}
	}
	return byID, nil
}

func statKey(path string) (fileKey, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileKey{}, nil
	}
	if err != nil {
		return fileKey{}, err
	}
	return keyOf(fi), nil
}

func keyOf(fi fs.FileInfo) fileKey {
	k := fileKey{exists: true, size: fi.Size(), mtime: fi.ModTime(), mode: fi.Mode()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.dev, k.ino, k.uid = uint64(st.Dev), uint64(st.Ino), st.Uid
	}
	return k
}

// exempt reports whether a request needs no token: only GET or HEAD of
// exactly /healthz.
func exempt(r *http.Request) bool {
	return r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// Middleware requires a valid bearer token on every request except GET
// /healthz. A source over its failure limit gets 429 before its token is
// looked at. The authenticated Principal reaches next through the request
// context (PrincipalFrom). Only the Authorization header is read: a token in
// the query string is ignored. Every refusal is audited as denied (FR-24);
// those of a rate-limited source collapse into one aggregate per window.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		src := SourceAddr(r)
		// Attempts from one source are evaluated one at a time (Begin), so a
		// concurrent burst cannot outrun the limit.
		end, ok, retry := a.limiter.Begin(src)
		if !ok {
			if a.audit != nil {
				a.audit.RateLimited(sourceGroup(src), surfaceOf(r))
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
			a.write(w, r, problem.New(problem.RateLimited, "too many failed authentication attempts from this source; retry later"))
			return
		}
		tok, present := bearer(r.Header)
		if !present {
			end(true)
			a.log.Info("authentication failed", "source", src.String(), "reason", "no bearer credentials")
			a.denied(r, src)
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus"`)
			a.write(w, r, unauthorized())
			return
		}
		p, err := a.Authenticate(tok)
		if err != nil {
			reason := "invalid token"
			// A token store that is unavailable is not the client's fault:
			// not counted against the source.
			unavailable := errors.Is(err, ErrTokenStoreUnavailable)
			end(!unavailable)
			if unavailable {
				reason = "token store unavailable"
			}
			a.log.Info("authentication failed", "source", src.String(), "reason", reason)
			a.denied(r, src)
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus", error="invalid_token"`)
			a.write(w, r, unauthorized())
			return
		}
		end(false)
		p.Source = src
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// denied audits one authentication failure. No credential material is
// recorded: an unauthenticated request has no token name or ID, and the
// presented token is never read into the record. The response does not
// depend on the write; a failure is logged and the audit log marks itself
// unhealthy.
func (a *Authenticator) denied(r *http.Request, src netip.Addr) {
	if a.audit == nil {
		return
	}
	rec := systemspec.AuditRecord{Surface: surfaceOf(r), Outcome: "denied", Problem: string(problem.Unauthorized)}
	if src.IsValid() {
		rec.Source = src.String()
	}
	if _, err := a.audit.Write(rec, nil); err != nil {
		a.log.Error("audit write failed", "outcome", rec.Outcome, "problem", rec.Problem, "source", rec.Source,
			"error", err.Error())
	}
}

// sourceGroup names the group the limiter counts a source in: an IPv4
// address, or an IPv6 /64.
func sourceGroup(a netip.Addr) string {
	k := sourceKey(a)
	if !k.IsValid() {
		return ""
	}
	if k.Addr().Is4() {
		return k.Addr().String()
	}
	return k.String()
}

// surfaceOf is the transport a request arrived on: MCP at /mcp (8.1), REST
// everywhere else.
func surfaceOf(r *http.Request) string {
	if r.URL.Path == "/mcp" || strings.HasPrefix(r.URL.Path, "/mcp/") {
		return "mcp"
	}
	return "rest"
}

func unauthorized() *problem.Error {
	return problem.New(problem.Unauthorized, "a valid bearer token is required")
}

// bearer extracts the token from exactly one Authorization header of the form
// "Bearer <token>" (RFC 6750 2.1; the scheme is case-insensitive). present is
// false when there are no bearer credentials at all; a malformed value yields
// present with an empty token, which fails as invalid.
func bearer(h http.Header) (tok string, present bool) {
	vals := h.Values("Authorization")
	if len(vals) == 0 {
		return "", false
	}
	if len(vals) > 1 {
		return "", true
	}
	scheme, rest, _ := strings.Cut(vals[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	rest = strings.TrimLeft(rest, " ")
	if rest == "" || strings.ContainsAny(rest, " \t") {
		return "", true
	}
	return rest, true
}

func (a *Authenticator) write(w http.ResponseWriter, r *http.Request, p *problem.Error) {
	if err := problem.Write(w, p); err != nil {
		a.log.Debug("problem response write failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	}
}
