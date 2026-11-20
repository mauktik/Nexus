// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package auth is the authentication boundary of nexusd: bearer tokens stored
// as hashes in tokens.json, the HTTP middleware that checks them, the
// per-source failure rate limiter, and the token store's create, list and
// revoke. The local `nexusd token` command stays the bootstrap and recovery
// path: it needs no running daemon and no existing token. The nexusd
// provider's token ops, gated by nexusd:admin, also create, list and revoke
// through the same store; both serialize on the store's lock.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"
)

// DefaultStateDir is the state directory of an installed nexusd.
const DefaultStateDir = "/var/lib/nexus"

// TokenFileName is the token store inside the state directory. It holds
// hashes only and is replaced by atomic rename.
const TokenFileName = "tokens.json"

// TokenFileMode is the permission of tokens.json (NFR-4).
const TokenFileMode os.FileMode = 0o600

// lockFileName serializes writers. The lock cannot live on tokens.json
// itself, because each write replaces that file's inode.
const lockFileName = "tokens.json.lock"

// maxTokenFileBytes bounds what a load reads; a real file is a few KB.
const maxTokenFileBytes = 4 << 20

// Token format: nxs_<id>_<secret>. The ID is 6 random bytes in lowercase hex
// and the secret 32 random bytes in unpadded base64url (43 characters). The ID
// has no underscore, so the first underscore after the prefix ends it; the
// secret's alphabet may itself contain underscores.
const (
	tokenPrefix       = "nxs_"
	idBytes           = 6
	tokenEntropyBytes = 32
	encodedLen        = 43 // base64.RawURLEncoding.EncodedLen(tokenEntropyBytes)
)

// TokenStringPattern matches a name that gives a token's secret away: the
// token prefix and ID in any case, or any run of encodedLen base64url
// characters, the length of the secret. The run is unanchored, so a name
// that merely holds a secret matches too. The nexusd provider's token name
// schema repeats it (cmd/nexus must not import this package), and a test
// pins the two and derives both from the constants above.
const TokenStringPattern = `^[Nn][Xx][Ss]_[0-9A-Fa-f]{12}_|[A-Za-z0-9_-]{43}`

// TokenStringRefusal is what a name matching TokenStringPattern is told,
// locally and remotely. It never quotes the value.
const TokenStringRefusal = "this looks like, or contains, a token string; pass the token's name or ID, never the token itself"

// strictB64 rejects non-canonical encodings, so exactly one string encodes
// each 32-byte value.
var strictB64 = base64.RawURLEncoding.Strict()

var (
	idRE    = regexp.MustCompile(`^[0-9a-f]{12}$`)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	scopeRE = regexp.MustCompile(`^[a-z][a-z0-9-]*:[a-z][a-z0-9-]*(:[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

	tokenStringRE = regexp.MustCompile(TokenStringPattern)
)

// Token describes a stored token. It never carries the secret or its hash.
type Token struct {
	ID        string
	Name      string
	Scopes    []string // Empty unless given at creation (FR-22)
	Created   time.Time
	Expires   *time.Time // Nil means no expiry
	CreatedBy *TokenRef  // The token that created it remotely; nil for a local create
}

// TokenRef identifies a token by ID and name.
type TokenRef struct {
	ID   string
	Name string
}

// NameExistsError refuses a create whose name a stored token already has.
type NameExistsError struct {
	Name string
	ID   string // The existing token's ID
}

func (e *NameExistsError) Error() string {
	return fmt.Sprintf("a token named %q already exists (id %s); revoke it first or choose another name", e.Name, e.ID)
}

// NotFoundError refuses a revoke whose reference matches no token's ID or name.
type NotFoundError struct {
	Ref string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no token with id or name %q", e.Ref)
}

// LimitError refuses a create when the store already holds Max tokens.
type LimitError struct {
	Max int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("the token store already holds %d tokens, the most allowed; revoke one first", e.Max)
}

// record is one entry of tokens.json.
type record struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Hash    string     `json:"sha256"` // Hex SHA-256 of the 32 decoded secret bytes
	Scopes  []string   `json:"scopes"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
	// Absent for a local create, so an rc3 store and an rc3 reader, which
	// ignores the unknown key, see the same file (CON-5).
	CreatedBy *createdByJSON `json:"createdBy,omitempty"`
}

// createdByJSON is the wire form of TokenRef in tokens.json.
type createdByJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type tokenFile struct {
	Tokens []record `json:"tokens"`
}

func (r record) token() Token {
	t := Token{ID: r.ID, Name: r.Name, Scopes: slices.Clone(r.Scopes), Created: r.Created}
	if r.Expires != nil {
		e := *r.Expires
		t.Expires = &e
	}
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	if r.CreatedBy != nil {
		t.CreatedBy = &TokenRef{ID: r.CreatedBy.ID, Name: r.CreatedBy.Name}
	}
	return t
}

// CreateRequest describes a new token.
type CreateRequest struct {
	Name   string
	Scopes []string      // Duplicates are dropped; none means no scopes
	TTL    time.Duration // Zero means no expiry
	Now    time.Time     // Zero means time.Now
	// MaxTokens refuses the create with *LimitError when the store already
	// holds that many tokens, checked under the lock. Zero means no cap.
	MaxTokens int
	CreatedBy *TokenRef // The creating token for a remote create; nil for a local one
}

// Create adds a token to the store in dir, creating dir (0700) if needed, and
// returns the token string. That string is the only copy of the secret: the
// store keeps its SHA-256. A 256-bit random secret needs no slow KDF. A full
// store gives *LimitError, checked before the name, and a taken name gives
// *NameExistsError. A name carrying a token string is refused first, so its
// error never quotes it, however long it is.
func Create(dir string, req CreateRequest) (string, Token, error) {
	if tokenStringRE.MatchString(req.Name) {
		return "", Token{}, errors.New("invalid token name: " + TokenStringRefusal)
	}
	if !nameRE.MatchString(req.Name) {
		return "", Token{}, fmt.Errorf("invalid token name %q: use 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit", req.Name)
	}
	var scopes []string
	for _, s := range req.Scopes {
		if !scopeRE.MatchString(s) {
			return "", Token{}, fmt.Errorf("invalid scope %q: want provider:access or provider:access:id, such as containers:read", s)
		}
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	if scopes == nil {
		scopes = []string{}
	}
	if req.TTL < 0 {
		return "", Token{}, errors.New("expiry must be in the future")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	raw := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, fmt.Errorf("generate secret: %w", err)
	}
	digest := sha256.Sum256(raw)
	rec := record{
		Name:    req.Name,
		Hash:    hex.EncodeToString(digest[:]),
		Scopes:  scopes,
		Created: now,
	}
	if req.CreatedBy != nil {
		rec.CreatedBy = &createdByJSON{ID: req.CreatedBy.ID, Name: req.CreatedBy.Name}
	}
	if req.TTL > 0 {
		e := now.Add(req.TTL)
		rec.Expires = &e
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Token{}, fmt.Errorf("create state directory: %w", err)
	}
	err := update(dir, func(recs []record) ([]record, error) {
		if req.MaxTokens > 0 && len(recs) >= req.MaxTokens {
			return nil, &LimitError{Max: req.MaxTokens}
		}
		for _, r := range recs {
			if r.Name == rec.Name {
				return nil, &NameExistsError{Name: r.Name, ID: r.ID}
			}
		}
		for {
			id := make([]byte, idBytes)
			if _, err := rand.Read(id); err != nil {
				return nil, fmt.Errorf("generate token id: %w", err)
			}
			rec.ID = hex.EncodeToString(id)
			if !slices.ContainsFunc(recs, func(r record) bool { return r.ID == rec.ID }) {
				break
			}
		}
		return append(recs, rec), nil
	})
	if err != nil {
		return "", Token{}, err
	}
	return tokenPrefix + rec.ID + "_" + strictB64.EncodeToString(raw), rec.token(), nil
}

// List returns every stored token, without secrets or hashes.
func List(dir string) ([]Token, error) {
	recs, err := readRecords(filepath.Join(dir, TokenFileName), os.Geteuid())
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.token())
	}
	return out, nil
}

// Revoke removes the token whose ID or name is ref, or gives *NotFoundError.
// A running daemon refuses it from its next request on.
func Revoke(dir, ref string) (Token, error) {
	var gone record
	err := update(dir, func(recs []record) ([]record, error) {
		i := slices.IndexFunc(recs, func(r record) bool { return r.ID == ref || r.Name == ref })
		if i < 0 {
			return nil, &NotFoundError{Ref: ref}
		}
		gone = recs[i]
		return slices.Delete(recs, i, i+1), nil
	})
	if err != nil {
		return Token{}, err
	}
	return gone.token(), nil
}

// update runs fn on the current records under an exclusive flock and
// replaces tokens.json by atomic rename with the result. It never touches
// state.db, so it does not wait on a running daemon.
func update(dir string, fn func([]record) ([]record, error)) (err error) {
	lock, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_RDWR|os.O_CREATE, TokenFileMode)
	if err != nil {
		return fmt.Errorf("open token lock: %w", err)
	}
	defer func() {
		// Closing the descriptor releases the flock.
		if cerr := lock.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close token lock: %w", cerr)
		}
	}()
	if err := flock(lock); err != nil {
		return fmt.Errorf("lock token file: %w", err)
	}

	path := filepath.Join(dir, TokenFileName)
	recs, err := readRecords(path, os.Geteuid())
	if err != nil {
		return err
	}
	recs, err = fn(recs)
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []record{}
	}
	data, err := json.MarshalIndent(tokenFile{Tokens: recs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token file: %w", err)
	}
	return writeAtomic(dir, path, append(data, '\n'))
}

func flock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// writeAtomic writes data to a 0600 temporary file in dir, fsyncs it, renames
// it over path and fsyncs dir, so a reader sees the old file or the new one,
// never a partial one.
func writeAtomic(dir, path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".tokens.json.*")
	if err != nil {
		return fmt.Errorf("create temporary token file: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	// CreateTemp already uses 0600; Chmod makes the mode independent of it.
	if err := tmp.Chmod(TokenFileMode); err != nil {
		return fmt.Errorf("chmod temporary token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary token file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync state directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("close state directory: %w", err)
	}
	return nil
}

// readRecords loads tokens.json. A missing file holds no tokens.
func readRecords(path string, euid int) ([]record, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	recs, _, err := decodeChecked(f, fi, path, euid)
	return recs, err
}

// decodeChecked checks the opened file's mode and owner, then reads and
// validates it, returning the records and each one's decoded hash. Any
// problem fails the whole file: a partly understood token store is not used.
func decodeChecked(f io.Reader, fi fs.FileInfo, path string, euid int) ([]record, [][sha256.Size]byte, error) {
	if err := checkTokenFile(fi, euid); err != nil {
		return nil, nil, fmt.Errorf("token file %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read token file %s: %w", path, err)
	}
	if len(data) > maxTokenFileBytes {
		return nil, nil, fmt.Errorf("token file %s is larger than %d bytes", path, maxTokenFileBytes)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, nil, fmt.Errorf("token file %s is not valid JSON: %v", path, err)
	}
	hashes := make([][sha256.Size]byte, len(tf.Tokens))
	ids := make(map[string]bool, len(tf.Tokens))
	for i, r := range tf.Tokens {
		if !idRE.MatchString(r.ID) || ids[r.ID] {
			return nil, nil, fmt.Errorf("token file %s: entry %d has a missing, malformed or duplicate id", path, i)
		}
		ids[r.ID] = true
		b, err := hex.DecodeString(r.Hash)
		if err != nil || len(b) != sha256.Size {
			return nil, nil, fmt.Errorf("token file %s: token %s has a malformed sha256", path, r.ID)
		}
		copy(hashes[i][:], b)
		if r.Scopes == nil {
			tf.Tokens[i].Scopes = []string{}
		}
	}
	if tf.Tokens == nil {
		tf.Tokens = []record{}
	}
	return tf.Tokens, hashes, nil
}

// checkTokenFile refuses a tokens.json that is not a regular file or that
// gives group or other any access: one others can write would let them add a
// token. Running as root, the file must also be owned by root.
func checkTokenFile(fi fs.FileInfo, euid int) error {
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %04o gives group or other access; run chmod 0600 on it", perm)
	}
	if euid != 0 {
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine the file owner")
	}
	if st.Uid != 0 {
		return fmt.Errorf("owned by uid %d, must be owned by root", st.Uid)
	}
	return nil
}

// parseToken splits a presented token into its ID and decoded secret. The
// error never includes the input.
func parseToken(tok string) (string, []byte, bool) {
	if len(tok) != len(tokenPrefix)+2*idBytes+1+encodedLen || tok[:len(tokenPrefix)] != tokenPrefix {
		return "", nil, false
	}
	rest := tok[len(tokenPrefix):]
	id, enc := rest[:2*idBytes], rest[2*idBytes+1:]
	if rest[2*idBytes] != '_' || !idRE.MatchString(id) {
		return "", nil, false
	}
	raw, err := strictB64.DecodeString(enc)
	if err != nil || len(raw) != tokenEntropyBytes {
		return "", nil, false
	}
	return id, raw, true
}
