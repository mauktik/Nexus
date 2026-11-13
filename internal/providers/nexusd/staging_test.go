package nexusd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// stagingT0 is the virtual start of the F-CLK tests: a whole second, so the
// expiresAt of an upload and the 409 detail format the same instant alike.
var stagingT0 = time.Date(2026, 11, 13, 9, 42, 0, 0, time.UTC)

const uploadsPath = "/providers/nexusd/uploads"

// uploadConfig writes a config file whose providers.nexusd section sets
// maxUploadBytes and the trusted keys.
func uploadConfig(t *testing.T, maxBytes int64, keys ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  docker:\n    enabled: false\n"+
		"  nexusd:\n    enabled: true\n    update:\n      maxUploadBytes: %d\n", maxBytes)
	if len(keys) > 0 {
		fmt.Fprintf(&b, "      trustedKeys: [%q]\n", strings.Join(keys, `", "`))
	}
	p := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// randomBytes returns n random bytes.
func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newSignature returns a valid-pattern signature: 64 random bytes in
// standard base64, 88 characters.
func newSignature(t *testing.T) string {
	t.Helper()
	sig := base64.StdEncoding.EncodeToString(randomBytes(t, ed25519.SignatureSize))
	if !regexp.MustCompile(spec.SignaturePattern).MatchString(sig) {
		t.Fatalf("signature %q does not match the pattern", sig)
	}
	return sig
}

// postUpload POSTs an upload without failing the test, so goroutines can
// call it. length is the declared Content-Length; -1 sends the body
// chunked.
func (s *stack) postUpload(ctx context.Context, token, query, contentType string, body io.Reader, length int64,
	header http.Header) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/v1"+uploadsPath+"?"+query, body)
	if err != nil {
		return response{}, err
	}
	req.ContentLength = length
	if length < 0 {
		req.ContentLength = 0 // Unknown with a body: chunked
		req.Body = io.NopCloser(body)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", contentType)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// upload POSTs body with signature sig as application/gzip.
func (s *stack) upload(t *testing.T, token, sig string, body []byte) response {
	t.Helper()
	r, err := s.postUpload(t.Context(), token, "signature="+url.QueryEscape(sig), "application/gzip",
		bytes.NewReader(body), int64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// stored decodes a 201 upload answer, and returns the raw expiresAt string
// as the response carried it.
func stored(t *testing.T, r response) (spec.UploadResult, string) {
	t.Helper()
	r.want(t, http.StatusCreated)
	res := decodeJSON[spec.UploadResult](t, r.body)
	raw := decodeJSON[map[string]any](t, r.body)
	exp, _ := raw["expiresAt"].(string)
	if exp == "" {
		t.Fatalf("no expiresAt string: %s", r.body)
	}
	return res, exp
}

// stg returns the provider's staging store.
func (s *stack) stg(t *testing.T) *staging {
	t.Helper()
	st, err := s.p.stagingOf()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// inflight reads the in-flight count.
func (s *staging) inflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// storedCount reads how many uploads are stored.
func (s *staging) storedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stored)
}

// setHooks replaces the test hooks.
func (s *staging) setHooks(h stagingHooks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = h
}

// stagingEntries lists the names in dir, sorted.
func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func (s *stack) stagingDir() string { return filepath.Join(s.stateDir, stagingDirName) }

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantConflict asserts a 409 conflict and returns its detail.
func wantConflict(t *testing.T, r response) string {
	t.Helper()
	r.want(t, http.StatusConflict)
	m := problemOf(t, r.body)
	if slugOf(m) != string(problem.Conflict) {
		t.Fatalf("slug %s, want conflict: %s", slugOf(m), r.body)
	}
	d, _ := m["detail"].(string)
	return d
}

// modeOfPath is the permission bits of path.
func modeOfPath(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TC-SM-71 (WI 5.1; FR-6): An upload over REST stores the body with the
// right layout, modes and meta.
func TestTCSM71_AnUploadOverRESTStoresThe(t *testing.T) {
	clk := newFakeClock(stagingT0)
	s := newStack(t, stackConfig{opts: Options{Now: clk.Now}, clock: clk, section: uploadConfig(t, 1<<20)})
	body := randomBytes(t, 200<<10)
	sig := newSignature(t)

	res, _ := stored(t, s.upload(t, s.admin, sig, body))
	if !regexp.MustCompile(spec.UploadIDPattern).MatchString(res.UploadID) {
		t.Errorf("uploadId %q does not match %s", res.UploadID, spec.UploadIDPattern)
	}
	sum := sha256.Sum256(body)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 %s, want %x", res.SHA256, sum)
	}
	if res.SizeBytes != 204800 {
		t.Errorf("sizeBytes %d, want 204800", res.SizeBytes)
	}
	if want := stagingT0.Add(time.Hour); !res.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt %s, want %s", res.ExpiresAt, want)
	}

	dir := filepath.Join(s.stagingDir(), res.UploadID)
	if m := modeOfPath(t, s.stagingDir()); m != 0o700 {
		t.Errorf("staging/ mode %o, want 700", m)
	}
	if m := modeOfPath(t, dir); m != 0o700 {
		t.Errorf("staging/<id>/ mode %o, want 700", m)
	}
	tarball := filepath.Join(dir, stagedFileName)
	if m := modeOfPath(t, tarball); m != 0o400 {
		t.Errorf("release.tar.gz mode %o, want 400", m)
	}
	got, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Error("release.tar.gz differs from the body")
	}
	if names := stagingEntries(t, dir); !slices.Equal(names, []string{stagedMetaName, stagedFileName}) {
		t.Errorf("staging/<id>/ holds %v, want meta.json and release.tar.gz only (no .partial)", names)
	}
	metaBytes, err := os.ReadFile(filepath.Join(dir, stagedMetaName))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"expires", "received", "sha256", "signature", "sizeBytes", "uploadId"}; !slices.Equal(keys, want) {
		t.Errorf("meta.json keys %v, want %v", keys, want)
	}
	m := decodeJSON[stagedMeta](t, metaBytes)
	if m.UploadID != res.UploadID || m.SHA256 != res.SHA256 || m.SizeBytes != 204800 || m.Signature != sig ||
		!m.Received.Equal(stagingT0) || !m.Expires.Equal(stagingT0.Add(time.Hour)) {
		t.Errorf("meta.json %+v does not match the upload %+v (signature %q, received %s)", m, res, sig, stagingT0)
	}
}

// TC-SM-72 (WI 5.1; FR-6): Re-uploading the same bytes and signature
// returns the existing uploadId.
func TestTCSM72_ReUploadingTheSameBytesAnd(t *testing.T) {
	s := newStack(t, stackConfig{section: uploadConfig(t, 1<<20)})
	body := randomBytes(t, 200<<10)
	sig, sig2 := newSignature(t), newSignature(t)

	first, _ := stored(t, s.upload(t, s.admin, sig, body))
	second, _ := stored(t, s.upload(t, s.admin, sig, body))
	if second != first {
		t.Errorf("re-upload answered %+v, want the first upload %+v", second, first)
	}
	if names := stagingEntries(t, s.stagingDir()); !slices.Equal(names, []string{first.UploadID}) {
		t.Errorf("staging holds %v, want only %s", names, first.UploadID)
	}
	third, _ := stored(t, s.upload(t, s.admin, sig2, body))
	if third.UploadID == first.UploadID {
		t.Error("another signature reused the upload ID")
	}
	if n := len(stagingEntries(t, s.stagingDir())); n != 2 {
		t.Errorf("staging holds %d dirs, want 2", n)
	}
	var storedRecs int
	for _, r := range s.auditOf(t, spec.Upload.Op().ID) {
		if r.Outcome == audit.OutcomeStored {
			storedRecs++
		}
	}
	if storedRecs != 3 {
		t.Errorf("%d stored records, want 3", storedRecs)
	}
}

// TC-SM-73 (WI 5.1; FR-6, NFR-4): Expiry deletes the staged upload, and a
// later update is upload-not-found: both the staging lookup and, over REST,
// the update op (WI 5.3).
func TestTCSM73_ExpiryDeletesTheStagedUploadAnd(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	clk := newFakeClock(stagingT0)
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}, opts: Options{Now: clk.Now}, clock: clk})
	s := u.stack
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")
	res, _ := stored(t, s.upload(t, s.admin, signBytes(k, pkg), pkg))
	dir := filepath.Join(s.stagingDir(), res.UploadID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	clk.Advance(t, time.Hour)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after expiry the dir is still there: %v", err)
	}
	if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
		t.Errorf("staging holds %v after expiry", names)
	}
	if _, _, ok := s.stg(t).get(res.UploadID); ok {
		t.Error("the expired upload is still found")
	}
	body := wantProblem(t, u.update(t, updateOf(res.UploadID, false)), http.StatusUnprocessableEntity,
		problem.UploadNotFound)
	if body["uploadId"] != res.UploadID {
		t.Errorf("problem %v, want uploadId %s", body, res.UploadID)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
}

// TC-SM-74 (WI 5.1; FR-6, NFR-4): The startup sweep removes invalid,
// expired and stray entries and re-arms live uploads.
func TestTCSM74_TheStartupSweepRemovesInvalidExpired(t *testing.T) {
	clk := newFakeClock(stagingT0)
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, stagingDirName)
	ids := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d"} {
		id, err := newUploadID()
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = id
		if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, stagedFileName), []byte("body"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	writeMeta := func(n string, expires time.Time) {
		t.Helper()
		b, err := json.Marshal(stagedMeta{UploadID: ids[n], SHA256: strings.Repeat("ab", 32), SizeBytes: 4,
			Signature: "s", Received: expires.Add(-time.Hour), Expires: expires})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ids[n], stagedMetaName), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a has no meta.json.
	if err := os.WriteFile(filepath.Join(dir, ids["b"], stagedMetaName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMeta("c", stagingT0.Add(-time.Second))
	writeMeta("d", stagingT0.Add(10*time.Minute))
	if err := os.Mkdir(filepath.Join(dir, ids["d"], "probe-x1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ids["d"], stagedFileName+partialSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(Options{Host: StackHost, Now: clk.Now})
	p.afterFunc = clk.AfterFunc
	p.UseDaemon(newFakeDaemon(fixture(t, "valid.yaml"), stateDir, stagingT0))

	if names := stagingEntries(t, dir); !slices.Equal(names, []string{ids["d"]}) {
		t.Fatalf("after the sweep staging holds %v, want only d (%s)", names, ids["d"])
	}
	if names := stagingEntries(t, filepath.Join(dir, ids["d"])); !slices.Equal(names,
		[]string{stagedMetaName, stagedFileName}) {
		t.Errorf("d holds %v, want meta.json and release.tar.gz only", names)
	}
	clk.Advance(t, 10*time.Minute)
	if names := stagingEntries(t, dir); len(names) != 0 {
		t.Errorf("after d's expiry staging holds %v", names)
	}
}

// TC-SM-75 (WI 5.1; FR-16, FR-6): The upload audit carries requested and
// stored records with facts and no key material.
func TestTCSM75_TheUploadAuditCarriesRequestedAnd(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(pub)
	s := newStack(t, stackConfig{section: uploadConfig(t, 1<<20, key)})
	if len(s.p.current().keys) != 1 {
		t.Fatal("the trusted key was not configured")
	}
	res, _ := stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 8192)))

	recs := s.auditOf(t, spec.Upload.Op().ID)
	if got := outcomes(recs); !slices.Equal(got, []string{audit.OutcomeRequested, audit.OutcomeStored}) {
		t.Fatalf("records %v, want requested then stored", got)
	}
	want := map[string]string{"uploadId": res.UploadID, "sha256": res.SHA256,
		"sizeBytes": fmt.Sprint(res.SizeBytes)}
	if got := recs[1].Facts; !mapsEqual(got, want) {
		t.Errorf("stored facts %v, want %v", got, want)
	}
	if recs[1].RequestID == "" || recs[1].RequestID != recs[0].ID {
		t.Errorf("stored record names request %q, want %q", recs[1].RequestID, recs[0].ID)
	}
	raw, err := os.ReadFile(filepath.Join(s.auditDir, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key)) {
		t.Error("audit.jsonl holds the trusted key")
	}
	if strings.Contains(s.logs.String(), key) {
		t.Error("the server log holds the trusted key")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// treeOf lists every path under root, relative to it.
func treeOf(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-SM-76 (WI 5.1; FR-6, CON-4): Upload IDs are server-generated, and no
// client-supplied name reaches the file system.
func TestTCSM76_UploadIDsAreServerGeneratedAnd(t *testing.T) {
	s := newStack(t, stackConfig{})
	idRE := regexp.MustCompile(spec.UploadIDPattern)
	seen := map[string]bool{}
	sig := newSignature(t)
	for i := range 200 {
		res, _ := stored(t, s.upload(t, s.admin, sig, []byte(fmt.Sprintf("body %d", i))))
		if !idRE.MatchString(res.UploadID) || seen[res.UploadID] {
			t.Fatalf("upload %d: ID %q is malformed or repeated", i, res.UploadID)
		}
		seen[res.UploadID] = true
		if err := s.stg(t).remove(res.UploadID); err != nil {
			t.Fatal(err)
		}
	}

	before := treeOf(t, s.stateDir)
	header := http.Header{"Content-Disposition": {`attachment; filename="../../etc/x"`}, "X-Upload-Name": {"../x"}}
	r, err := s.postUpload(t.Context(), s.admin, "signature="+url.QueryEscape(sig), "application/gzip",
		strings.NewReader("named body"), int64(len("named body")), header)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := stored(t, r)
	after := treeOf(t, s.stateDir)
	var added []string
	for p := range after {
		if !before[p] {
			added = append(added, p)
		}
	}
	slices.Sort(added)
	id := filepath.Join(stagingDirName, res.UploadID)
	want := []string{id, filepath.Join(id, stagedMetaName), filepath.Join(id, stagedFileName)}
	if !slices.Equal(added, want) {
		t.Errorf("new paths %v, want %v", added, want)
	}
	for p := range before {
		if !after[p] {
			t.Errorf("path %s disappeared", p)
		}
	}

	r, err = s.postUpload(t.Context(), s.admin, "signature="+url.QueryEscape(sig)+"&uploadId=../x",
		"application/gzip", strings.NewReader("x"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.want(t, http.StatusBadRequest)
	if slug := slugOf(problemOf(t, r.body)); slug != string(problem.ValidationFailed) {
		t.Errorf("slug %s, want validation-failed", slug)
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r  io.Reader
	mu *sync.Mutex
	n  *int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	*c.n += int64(n)
	c.mu.Unlock()
	return n, err
}

// countBodies makes the staging count every body byte Receive reads.
func countBodies(st *staging) func() int64 {
	var mu sync.Mutex
	var n int64
	st.setHooks(stagingHooks{wrapBody: func(r io.Reader) io.Reader { return countingReader{r: r, mu: &mu, n: &n} }})
	return func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// TC-SM-E3 (WI 5.1; FR-6): Re-uploading a staged upload while 2 are staged
// is refused 409 before the body is read.
func TestTCSME3_ReUploadingAStagedUploadWhile(t *testing.T) {
	clk := newFakeClock(stagingT0)
	s := newStack(t, stackConfig{opts: Options{Now: clk.Now}, clock: clk})
	bodyA, sigA := randomBytes(t, 4096), newSignature(t)
	a, expA := stored(t, s.upload(t, s.admin, sigA, bodyA))
	clk.Advance(t, 10*time.Minute)
	b, expB := stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 4096)))

	read := countBodies(s.stg(t))
	detail := wantConflict(t, s.upload(t, s.admin, sigA, bodyA))
	if !strings.Contains(detail, expA) {
		t.Errorf("detail %q does not name A's expiresAt %s", detail, expA)
	}
	if strings.Contains(detail, expB) {
		t.Errorf("detail %q names B's expiresAt %s", detail, expB)
	}
	want := []string{a.UploadID, b.UploadID}
	slices.Sort(want)
	if names := stagingEntries(t, s.stagingDir()); !slices.Equal(names, want) {
		t.Errorf("staging holds %v, want %v", names, want)
	}
	if n := read(); n != 0 {
		t.Errorf("Receive read %d body bytes, want 0", n)
	}
}

// TC-SM-E16 (WI 5.1; FR-1, FR-19): A disabled nexusd provider refuses every
// op, including the upload.
func TestTCSME16_ADisabledNexusdProviderRefusesEvery(t *testing.T) {
	dk := docker.New(docker.Options{Host: StackHost})
	s := newStack(t, stackConfig{disabled: true, disabledExtra: []provider.Provider{dk}})
	dockerTok := s.mint("docker-reader", dockerspec.ReadScope)
	ref := s.do(t, http.MethodGet, "/providers/docker/containers", dockerTok, "", nil)
	refP := problemOf(t, ref.body)
	if refP["provider"] != dockerspec.Name {
		t.Fatalf("docker refusal %s names no provider", ref.body)
	}
	refReason, _ := refP["reason"].(string)

	check := func(name string, r response) {
		t.Helper()
		m := problemOf(t, r.body)
		if r.status != ref.status || slugOf(m) != slugOf(refP) {
			t.Errorf("%s: %d %s, want the docker refusal %d %s", name, r.status, slugOf(m), ref.status, slugOf(refP))
		}
		if m["provider"] != spec.Name {
			t.Errorf("%s: provider %v, want nexusd", name, m["provider"])
		}
		if want := strings.ReplaceAll(refReason, dockerspec.Name, spec.Name); m["reason"] != want {
			t.Errorf("%s: reason %v, want %q", name, m["reason"], want)
		}
	}
	check("upload", s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
	check("get", s.do(t, http.MethodGet, "/providers/nexusd/daemons", s.reader, "", nil))
	check("validate-config", s.action(t, "validate-config", s.reader, nil))
	if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
		t.Errorf("staging holds %v", names)
	}
}

// TC-SM-E17 (WI 5.1; FR-6): Expiry boundary: an upload is found at 59:59
// and gone at 60:00, both by the staging lookup and, over REST, by the
// update op (WI 5.3): 202 at 59:59, upload-not-found at 60:00.
func TestTCSME17_ExpiryBoundaryAnUploadIsFound(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	clk := newFakeClock(stagingT0)
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}, opts: Options{Now: clk.Now}, clock: clk})
	s := u.stack
	body := updatePackage(t, "v9.9.2", "v9.9.2")
	sig := signBytes(k, body)
	first, _ := stored(t, s.upload(t, s.admin, sig, body))
	clk.Advance(t, 59*time.Minute+59*time.Second)
	if _, _, ok := s.stg(t).get(first.UploadID); !ok {
		t.Fatal("the upload is gone at 59:59")
	}
	id := accepted(t, u.update(t, updateOf(first.UploadID, false)))
	u.spy.await(t, id)
	// The update holds the lane while its unit runs; end it as the
	// supervisor would, and its Finalize deletes the upload.
	now := time.Now()
	if err := detach.WriteExit(filepath.Join(s.opsDir, id), detach.Exit{Started: now, Ended: now}); err != nil {
		t.Fatal(err)
	}
	s.awaitOp(t, id)
	if _, _, ok := s.stg(t).get(first.UploadID); ok {
		t.Fatal("the update's Finalize did not delete the upload")
	}

	second, _ := stored(t, s.upload(t, s.admin, sig, body))
	clk.Advance(t, time.Hour)
	if _, _, ok := s.stg(t).get(second.UploadID); ok {
		t.Error("the upload is still found at 60:00")
	}
	if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
		t.Errorf("staging holds %v at 60:00", names)
	}
	p := wantProblem(t, u.update(t, updateOf(second.UploadID, false)), http.StatusUnprocessableEntity,
		problem.UploadNotFound)
	if p["uploadId"] != second.UploadID {
		t.Errorf("problem %v, want uploadId %s", p, second.UploadID)
	}
}

// blockedUpload is an upload whose body is held mid-stream.
type blockedUpload struct {
	w    *io.PipeWriter
	done chan struct{}
	resp response
	err  error
}

// startBlocked starts an upload that sends 1 KiB and waits for finish.
func (s *stack) startBlocked(t *testing.T, sig string) *blockedUpload {
	t.Helper()
	pr, pw := io.Pipe()
	u := &blockedUpload{w: pw, done: make(chan struct{})}
	go func() {
		defer close(u.done)
		u.resp, u.err = s.postUpload(context.Background(), s.admin, "signature="+url.QueryEscape(sig),
			"application/gzip", pr, -1, nil)
	}()
	go func() { _, _ = pw.Write(bytes.Repeat([]byte("x"), 1024)) }()
	t.Cleanup(func() { _ = pw.CloseWithError(errors.New("test over")) })
	return u
}

// finish sends the rest of the body and returns the answer.
func (u *blockedUpload) finish(t *testing.T, rest []byte) response {
	t.Helper()
	if _, err := u.w.Write(rest); err != nil {
		t.Fatal(err)
	}
	if err := u.w.Close(); err != nil {
		t.Fatal(err)
	}
	<-u.done
	if u.err != nil {
		t.Fatal(u.err)
	}
	return u.resp
}

// invoke calls the upload through stack.disp as the admin.
func (s *stack) invoke(t *testing.T, sig string, body []byte) (core.InvokeResult, error) {
	t.Helper()
	in, err := json.Marshal(spec.UploadInput{Signature: sig})
	if err != nil {
		t.Fatal(err)
	}
	return s.disp.Invoke(t.Context(), core.InvokeRequest{
		Caller:  auth.Principal{TokenID: "direct-admin", Name: "admin", Scopes: []string{spec.AdminScope, spec.ReadScope}},
		Surface: core.SurfaceREST, Op: spec.Upload.Op(), Input: in,
		Upload: &provider.UploadBody{Reader: bytes.NewReader(body), ContentType: "application/gzip",
			ContentLength: int64(len(body))},
	})
}

// expiresOf is how the REST answer renders an upload's expiresAt.
func expiresOf(t *testing.T, res core.InvokeResult) string {
	t.Helper()
	b, err := json.Marshal(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	exp, _ := decodeJSON[map[string]any](t, b)["expiresAt"].(string)
	return exp
}

// TC-SM-ERR38 (WI 5.1; FR-6): A third concurrent or staged upload is
// refused 409.
func TestTCSMERR38_AThirdConcurrentOrStagedUpload(t *testing.T) {
	t.Run("A in flight", func(t *testing.T) {
		clk := newFakeClock(stagingT0)
		s := newStack(t, stackConfig{opts: Options{Now: clk.Now}, clock: clk})
		st := s.stg(t)
		u1 := s.startBlocked(t, newSignature(t))
		waitFor(t, "the first reservation", func() bool { return st.inflightCount() == 1 })
		u2 := s.startBlocked(t, newSignature(t))
		waitFor(t, "the second reservation", func() bool { return st.inflightCount() == 2 })

		wantConflict(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
		if names := stagingEntries(t, s.stagingDir()); len(names) != 2 {
			t.Errorf("staging holds %v, want the two in flight only", names)
		}

		_, exp1 := stored(t, u1.finish(t, randomBytes(t, 2048)))
		clk.Advance(t, 10*time.Minute)
		_, exp2 := stored(t, u2.finish(t, randomBytes(t, 2048)))
		dirs := stagingEntries(t, s.stagingDir())
		detail := wantConflict(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
		if !strings.Contains(detail, exp1) || strings.Contains(detail, exp2) {
			t.Errorf("detail %q, want %s and not %s", detail, exp1, exp2)
		}
		if names := stagingEntries(t, s.stagingDir()); !slices.Equal(names, dirs) {
			t.Errorf("staging %v, want %v", names, dirs)
		}
	})

	t.Run("B staged", func(t *testing.T) {
		clk := newFakeClock(stagingT0)
		s := newStack(t, stackConfig{opts: Options{Now: clk.Now}, clock: clk})
		_, exp1 := stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
		clk.Advance(t, 10*time.Minute)
		_, exp2 := stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
		dirs := stagingEntries(t, s.stagingDir())
		detail := wantConflict(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 1024)))
		if !strings.Contains(detail, exp1) || strings.Contains(detail, exp2) {
			t.Errorf("detail %q, want %s and not %s", detail, exp1, exp2)
		}
		if names := stagingEntries(t, s.stagingDir()); !slices.Equal(names, dirs) {
			t.Errorf("staging %v, want %v", names, dirs)
		}
	})

	t.Run("C requested write fails", func(t *testing.T) {
		clk := newFakeClock(stagingT0)
		s := newStack(t, stackConfig{opts: Options{Now: clk.Now}, clock: clk, failRequested: 2})
		st := s.stg(t)
		for i := range 2 {
			_, err := s.invoke(t, newSignature(t), randomBytes(t, 1024))
			var pe *problem.Error
			if !errors.As(err, &pe) || pe.Slug() != problem.AuditUnavailable || pe.Status != http.StatusServiceUnavailable {
				t.Fatalf("upload %d: %v, want 503 audit-unavailable", i, err)
			}
		}
		if s.failing.failures() != 2 {
			t.Fatalf("%d requested writes failed, want 2", s.failing.failures())
		}
		if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
			t.Errorf("staging holds %v", names)
		}
		if n := st.inflightCount(); n != 0 {
			t.Errorf("in flight %d after the refusals, want 0", n)
		}

		res1, err := s.invoke(t, newSignature(t), randomBytes(t, 1024))
		if err != nil || res1.Status != http.StatusCreated {
			t.Fatalf("third upload: %+v %v, want 201", res1, err)
		}
		clk.Advance(t, 10*time.Minute)
		res2, err := s.invoke(t, newSignature(t), randomBytes(t, 1024))
		if err != nil || res2.Status != http.StatusCreated {
			t.Fatalf("fourth upload: %+v %v, want 201", res2, err)
		}
		exp1, exp2 := expiresOf(t, res1), expiresOf(t, res2)
		_, err = s.invoke(t, newSignature(t), randomBytes(t, 1024))
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Slug() != problem.Conflict {
			t.Fatalf("fifth upload: %v, want conflict", err)
		}
		if !strings.Contains(pe.Detail, exp1) || strings.Contains(pe.Detail, exp2) ||
			strings.Contains(pe.Detail, "still receiving") {
			t.Errorf("detail %q, want %s, not %s and not still receiving", pe.Detail, exp1, exp2)
		}
	})

	t.Run("D reserve and release", func(t *testing.T) {
		clk := newFakeClock(stagingT0)
		st := newStaging(filepath.Join(t.TempDir(), stagingDirName), clk.Now, clk.AfterFunc)
		if err := st.open(); err != nil {
			t.Fatal(err)
		}
		release, err := st.reserve()
		if err != nil {
			t.Fatal(err)
		}
		if n := st.inflightCount(); n != 1 {
			t.Fatalf("in flight %d after reserve, want 1", n)
		}
		release()
		release()
		if n := st.inflightCount(); n != 0 {
			t.Fatalf("in flight %d after release twice, want 0", n)
		}

		if _, err := st.receive(t.Context(), newSignature(t), bytes.NewReader(randomBytes(t, 512))); err != nil {
			t.Fatal(err)
		}
		for rep := range 100 {
			start := make(chan struct{})
			type result struct {
				release func()
				err     error
			}
			results := make(chan result, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					<-start
					r, err := st.reserve()
					results <- result{r, err}
				})
			}
			close(start)
			wg.Wait()
			close(results)
			var ok, conflicts int
			for r := range results {
				var pe *problem.Error
				switch {
				case r.err == nil:
					ok++
					r.release()
				case errors.As(r.err, &pe) && pe.Slug() == problem.Conflict:
					conflicts++
				default:
					t.Fatalf("rep %d: %v", rep, r.err)
				}
			}
			if ok != 1 || conflicts != 1 {
				t.Fatalf("rep %d: %d succeeded and %d conflicted, want 1 and 1", rep, ok, conflicts)
			}
		}
		if n := st.inflightCount(); n != 0 {
			t.Errorf("in flight %d at the end, want 0", n)
		}
	})

	t.Run("E in flight returns to 0", func(t *testing.T) {
		s := newStack(t, stackConfig{section: uploadConfig(t, 1<<20)})
		st := s.stg(t)
		body, sig := randomBytes(t, 4096), newSignature(t)
		first, _ := stored(t, s.upload(t, s.admin, sig, body))
		if n := st.inflightCount(); n != 0 {
			t.Errorf("in flight %d after an upload, want 0", n)
		}
		dup, _ := stored(t, s.upload(t, s.admin, sig, body))
		if dup.UploadID != first.UploadID {
			t.Errorf("dedup answered %s, want %s", dup.UploadID, first.UploadID)
		}
		if n := st.inflightCount(); n != 0 {
			t.Errorf("in flight %d after a dedup upload, want 0", n)
		}
		s.abortedUpload(t, 1<<20, 512<<10)
		waitFor(t, "the aborted upload's release", func() bool { return st.inflightCount() == 0 })
		if names := stagingEntries(t, s.stagingDir()); !slices.Equal(names, []string{first.UploadID}) {
			t.Errorf("staging %v, want only %s", names, first.UploadID)
		}
	})
}

// abortedUpload declares a body of declared bytes, sends sent of them and
// closes the connection, then waits until the server has dropped it.
func (s *stack) abortedUpload(t *testing.T, declared, sent int) {
	t.Helper()
	st := s.stg(t)
	hold := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got int64
	st.setHooks(stagingHooks{wrapBody: func(r io.Reader) io.Reader {
		return readerFunc(func(p []byte) (int, error) {
			n, err := r.Read(p)
			mu.Lock()
			got += int64(n)
			reached := got >= int64(sent)
			mu.Unlock()
			if reached {
				once.Do(func() { close(hold) })
			}
			return n, err
		})
	}})
	defer st.setHooks(stagingHooks{})

	conn, err := net.Dial("tcp", strings.TrimPrefix(s.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(conn)
	fmt.Fprintf(w, "POST /v1%s?signature=%s HTTP/1.1\r\nHost: t0\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/gzip\r\nContent-Length: %d\r\n\r\n", uploadsPath,
		url.QueryEscape(newSignature(t)), s.admin, declared)
	if _, err := w.Write(randomBytes(t, sent)); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hold:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never read the sent bytes")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the aborted upload's cleanup", func() bool {
		return st.inflightCount() == 0 && len(stagingEntries(t, s.stagingDir())) == st.storedCount()
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TC-SM-ERR39 (WI 5.1; FR-6, NFR-4): Oversize and aborted uploads leave
// nothing in staging.
func TestTCSMERR39_OversizeAndAbortedUploadsLeaveNothing(t *testing.T) {
	s := newStack(t, stackConfig{section: uploadConfig(t, 1<<20)})
	st := s.stg(t)
	empty := func(step string) {
		t.Helper()
		if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
			t.Errorf("%s: staging holds %v", step, names)
		}
		if n := st.inflightCount(); n != 0 {
			t.Errorf("%s: in flight %d, want 0", step, n)
		}
	}

	r, err := s.postUpload(t.Context(), s.admin, "signature="+url.QueryEscape(newSignature(t)), "application/gzip",
		bytes.NewReader(randomBytes(t, 1<<20+1)), -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.want(t, http.StatusRequestEntityTooLarge)
	if slug := slugOf(problemOf(t, r.body)); slug != string(problem.UploadTooLarge) {
		t.Errorf("slug %s, want upload-too-large", slug)
	}
	waitFor(t, "the oversize upload's release", func() bool { return st.inflightCount() == 0 })
	empty("oversize")

	s.abortedUpload(t, 1<<20, 512<<10)
	empty("aborted")

	st.setHooks(stagingHooks{afterRename: func() error { return errors.New("injected fault after the rename") }})
	r = s.upload(t, s.admin, newSignature(t), randomBytes(t, 4096))
	if r.status < 500 {
		t.Errorf("faulted receive answered %d: %s", r.status, r.body)
	}
	empty("fault after rename")
	st.setHooks(stagingHooks{})

	stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 4096)))
	stored(t, s.upload(t, s.admin, newSignature(t), randomBytes(t, 4096)))
}

// TC-SM-ERR40 (WI 5.1; FR-6, FR-1): The upload requires nexusd:admin and
// working self-management.
func TestTCSMERR40_TheUploadRequiresNexusdAdminAnd(t *testing.T) {
	s := newStack(t, stackConfig{})
	none := func(step string) {
		t.Helper()
		if names := stagingEntries(t, s.stagingDir()); len(names) != 0 {
			t.Errorf("%s: staging holds %v", step, names)
		}
	}
	s.upload(t, "", newSignature(t), randomBytes(t, 1024)).want(t, http.StatusUnauthorized)
	none("no token")
	r := s.upload(t, s.reader, newSignature(t), randomBytes(t, 1024)).want(t, http.StatusForbidden)
	if got := problemOf(t, r.body)["requiredScope"]; got != spec.AdminScope {
		t.Errorf("requiredScope %v, want %s", got, spec.AdminScope)
	}
	none("reader")

	const reason = "systemd is not running"
	s2 := newStack(t, stackConfig{opts: Options{Systemd: func() detach.Availability {
		return detach.Availability{Reason: reason}
	}}})
	r = s2.upload(t, s2.admin, newSignature(t), randomBytes(t, 1024)).want(t, http.StatusServiceUnavailable)
	m := problemOf(t, r.body)
	if slugOf(m) != string(problem.CapabilityUnavailable) || m["reason"] != reason {
		t.Errorf("refusal %s, want capability-unavailable with reason %q", r.body, reason)
	}
	if names := stagingEntries(t, s2.stagingDir()); len(names) != 0 {
		t.Errorf("no systemd: staging holds %v", names)
	}
	if n := s2.stg(t).inflightCount(); n != 0 {
		t.Errorf("no systemd: in flight %d, want 0", n)
	}
}

// patternReader yields n bytes of a fixed pattern without allocating.
type patternReader struct{ left int64 }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	n = min(n, r.left)
	for i := range p[:n] {
		p[i] = byte(i)
	}
	r.left -= n
	return int(n), nil
}

// NFR-4: receiving 64 MiB allocates far less than the body: it streams
// through one 32 KiB buffer and is never held in memory.
func TestStagingReceiveStreams(t *testing.T) {
	st := newStaging(filepath.Join(t.TempDir(), stagingDirName), time.Now, func(d time.Duration, f func()) func() bool {
		return time.AfterFunc(d, f).Stop
	})
	if err := st.open(); err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m, err := st.receive(t.Context(), "sig", &patternReader{left: size})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if m.SizeBytes != size {
		t.Fatalf("size %d, want %d", m.SizeBytes, size)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("receiving 64 MiB allocated %d bytes, want under 8 MiB", grew)
	}
	if err := st.remove(m.UploadID); err != nil {
		t.Fatal(err)
	}
}

// remove is idempotent and ignores an ID that is not one.
func TestStagingRemove(t *testing.T) {
	clk := newFakeClock(stagingT0)
	st := newStaging(filepath.Join(t.TempDir(), stagingDirName), clk.Now, clk.AfterFunc)
	if err := st.open(); err != nil {
		t.Fatal(err)
	}
	m, err := st.receive(t.Context(), "sig", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := st.remove(m.UploadID); err != nil {
			t.Fatal(err)
		}
	}
	if clk.pending() != 0 {
		t.Error("the expiry timer is still armed after remove")
	}
	if err := st.remove("../../etc"); err != nil {
		t.Fatal(err)
	}
	if names := stagingEntries(t, st.dir); len(names) != 0 {
		t.Errorf("staging holds %v", names)
	}
}
