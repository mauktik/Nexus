package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

// The tp provider is the upload tests' provider (TC-SM-3 shape): a KindUpload
// op and a long-running update bound to it through CLI.Upload, served by the
// real core and server.
const (
	tpName       = "tp"
	tpAdminScope = "tp:admin"
	tpReadScope  = "tp:read"
	tpCeiling    = 256 << 20
	tpUploadPath = "/v1/providers/tp/uploads"
	tpUpdatePath = "/v1/providers/tp/actions/update"
)

type tpUploadIn struct {
	Signature string `json:"signature" jsonschema:"Detached signature, the .sig file's content."`
}

type tpUploadOut struct {
	UploadID  string `json:"uploadId"`
	SizeBytes int64  `json:"sizeBytes"`
}

type tpUpdateIn struct {
	UploadID       string `json:"uploadId" jsonschema:"ID of a staged upload."`
	AllowDowngrade bool   `json:"allowDowngrade,omitempty" jsonschema:"Install an older version."`
}

type tpUpdateOut struct {
	apitypes.ActionResult
	UploadID       string `json:"uploadId"`
	AllowDowngrade bool   `json:"allowDowngrade"`
}

var (
	tpUpload = def.Define[tpUploadIn, tpUploadOut](def.Op{
		ID: tpName + ".uploads.add", Provider: tpName, Kind: def.KindUpload, ResourceType: "uploads",
		Summary: "Stage a package", Description: "Stages a package for the upload tests. Test only.",
		Scope: def.Scope{Name: tpAdminScope}, Risk: def.RiskMutating, Mode: def.ModeSync,
		Upload: &def.Upload{ContentTypes: []string{"application/octet-stream", "application/gzip"}, MaxBytes: tpCeiling},
	})
	tpUpdate = def.Define[tpUpdateIn, tpUpdateOut](def.Op{
		ID: tpName + ".actions.update", Provider: tpName, Kind: def.KindProviderAction, Action: "update",
		Summary: "Update from a package", Description: "Installs a staged package. Test only. Long-running.",
		Scope: def.Scope{Name: tpAdminScope}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
		CLI: def.CLIBinding{Noun: tpName, Verb: "update", Upload: &def.CLIUpload{
			Op: tpName + ".uploads.add", Field: "uploadId", FileFlag: "file", FileUsage: "Package to install.",
			Sidecars: []def.CLISidecar{{Param: "signature", Flag: "signature", Suffix: ".sig", MaxBytes: 4096,
				Usage: "Signature file of the package. Default <file>.sig."}},
		}},
	})
)

func tpSpec() def.ProviderSpec {
	return def.ProviderSpec{Name: tpName, ReadScope: tpReadScope, Ops: []def.Op{tpUpdate.Op(), tpUpload.Op()}}
}

func tpSpecs() []def.ProviderSpec { return append(catalog.All(), tpSpec()) }

// tpProv is one server's tp provider. Its upload cap is maxBytes; it
// issues upload IDs prefixed with its host name, and the update completes
// at once with its input echoed.
type tpProv struct {
	own      string
	maxBytes atomic.Int64

	mu      sync.Mutex
	issued  []string
	bodies  [][]byte
	updates []tpUpdateIn
}

func newTP(own string) *tpProv {
	p := &tpProv{own: own}
	p.maxBytes.Store(tpCeiling)
	return p
}

func (*tpProv) Spec() def.ProviderSpec          { return tpSpec() }
func (*tpProv) Configure(func(any) error) error { return nil }
func (*tpProv) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (p *tpProv) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Upload(tpUpload, provider.UploadSpec[tpUploadIn, tpUploadOut]{
			MaxBytes: p.maxBytes.Load,
			Receive: func(_ context.Context, _ tpUploadIn, b provider.UploadBody) (tpUploadOut, error) {
				data, err := io.ReadAll(b.Reader)
				if err != nil {
					return tpUploadOut{}, err
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				id := fmt.Sprintf("%s-up-%d", p.own, len(p.issued)+1)
				p.issued = append(p.issued, id)
				p.bodies = append(p.bodies, data)
				return tpUploadOut{UploadID: id, SizeBytes: int64(len(data))}, nil
			},
		}),
		provider.InProcess(tpUpdate, provider.InProcessSpec[tpUpdateIn, tpUpdateOut]{
			Run: func(_ context.Context, in tpUpdateIn, _ *provider.Run) (tpUpdateOut, error) {
				p.mu.Lock()
				p.updates = append(p.updates, in)
				p.mu.Unlock()
				return tpUpdateOut{ActionResult: apitypes.ActionResult{Changed: true}, UploadID: in.UploadID,
					AllowDowngrade: in.AllowDowngrade}, nil
			},
		}),
	}
}

func (p *tpProv) state() (issued []string, bodies [][]byte, updates []tpUpdateIn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.issued), slices.Clone(p.bodies), slices.Clone(p.updates)
}

// upRecorded is one request the upload proxy saw, with its headers and body.
type upRecorded struct {
	method, path, query string
	header              http.Header
	contentLength       int64
	body                []byte
}

// upProxy is a reverse proxy in front of a real server that records every
// request whole.
type upProxy struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []upRecorded
}

func startUpProxy(t *testing.T, upstream string) *upProxy {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	up := &upProxy{}
	rev := httputil.NewSingleHostReverseProxy(u)
	rev.FlushInterval = -1
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		up.mu.Lock()
		up.reqs = append(up.reqs, upRecorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), r.ContentLength, body})
		up.mu.Unlock()
		rev.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	return up
}

func (up *upProxy) requests() []upRecorded {
	up.mu.Lock()
	defer up.mu.Unlock()
	return slices.Clone(up.reqs)
}

func (up *upProxy) posts(path string) []upRecorded {
	var out []upRecorded
	for _, r := range up.requests() {
		if r.method == http.MethodPost && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// tpHost is one tp server behind an upload proxy.
type tpHost struct {
	p     *tpProv
	s     *providerServer
	proxy *upProxy
}

func startTPHost(t *testing.T, own string) *tpHost {
	t.Helper()
	p := newTP(own)
	s := startProviderServer(t, own, p, true, tpAdminScope, tpReadScope)
	return &tpHost{p: p, s: s, proxy: startUpProxy(t, s.url)}
}

// writeFile writes n bytes of a repeating pattern to dir/name.
func writeFile(t *testing.T, dir, name string, n int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeText(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sig88 is an 88-character signature, the length of a base64 ed25519 one.
var sig88 = strings.Repeat("A", 86) + "=="

// TC-SM-31 (WI 2.4; FR-13, FR-7): the CLI uploads, then posts the op with
// the returned uploadId.
func TestTCSM31_TheCLIUploadsThenPostsThe(t *testing.T) {
	h := startTPHost(t, "h1")
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: h.s.token})
	f := writeFile(t, t.TempDir(), "pkg.tar.gz", 3<<20)
	writeText(t, f+".sig", sig88+"\n")

	r := runSpecsCLI(homeEnv(home), tpSpecs(), "tp", "update", "--file", f, "--idempotency-key", "k1", "-o", "json")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	reqs := h.proxy.requests()
	if len(reqs) < 2 {
		t.Fatalf("proxy saw %d requests, want the upload and the op POST first", len(reqs))
	}
	up, op := reqs[0], reqs[1]
	if up.method != http.MethodPost || up.path != tpUploadPath || up.query != "signature="+url.QueryEscape(sig88) {
		t.Errorf("request 1 = %s %s?%s, want POST %s?signature=<trimmed sig>", up.method, up.path, up.query, tpUploadPath)
	}
	if ct := up.header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("upload Content-Type = %q, want application/octet-stream", ct)
	}
	if up.contentLength != 3<<20 {
		t.Errorf("upload Content-Length = %d, want %d", up.contentLength, 3<<20)
	}
	want, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(up.body, want) {
		t.Errorf("upload body (%d bytes) differs from the file", len(up.body))
	}
	if k := up.header.Get("Idempotency-Key"); k != "" {
		t.Errorf("the upload carries Idempotency-Key %q, want none", k)
	}

	issued, bodies, updates := h.p.state()
	if len(issued) != 1 || len(bodies) != 1 || !bytes.Equal(bodies[0], want) {
		t.Fatalf("provider issued %v, want one upload of the file", issued)
	}
	if op.method != http.MethodPost || op.path != tpUpdatePath {
		t.Errorf("request 2 = %s %s, want POST %s", op.method, op.path, tpUpdatePath)
	}
	var body map[string]any
	if err := json.Unmarshal(op.body, &body); err != nil {
		t.Fatalf("op body %q: %v", op.body, err)
	}
	if body["uploadId"] != issued[0] {
		t.Errorf("op body uploadId = %v, want the upload's %q", body["uploadId"], issued[0])
	}
	if k := op.header.Get("Idempotency-Key"); k != "k1" {
		t.Errorf("op Idempotency-Key = %q, want k1", k)
	}
	if len(updates) != 1 || updates[0].UploadID != issued[0] {
		t.Errorf("update ran with %+v, want uploadId %q", updates, issued[0])
	}

	if !strings.Contains(r.stderr, "uploading "+f+" (3 MiB) to host") {
		t.Errorf("stderr lacks the upload line: %s", r.stderr)
	}
	var printed []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil || len(printed) != 1 {
		t.Fatalf("stdout %q is not one operation: %v", r.stdout, err)
	}
	final := printed[0]
	result, _ := final["result"].(map[string]any)
	if final["status"] != string(apitypes.StatusSucceeded) || result["uploadId"] != issued[0] {
		t.Errorf("stdout = %s, want the succeeded operation echoing uploadId %q", r.stdout, issued[0])
	}
}

// TC-SM-32 (WI 2.4; FR-13): the signature sidecar defaults to <file>.sig,
// and --signature overrides it; whitespace and newlines are trimmed.
func TestTCSM32_TheSignatureSidecarDefaultsToFile(t *testing.T) {
	h := startTPHost(t, "h1")
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: h.s.token})
	dir := t.TempDir()
	f := writeFile(t, dir, "pkg.tar.gz", 1024)
	s1, s2 := "S1"+strings.Repeat("a", 86), "S2"+strings.Repeat("b", 86)
	writeText(t, f+".sig", "  "+s1+"\n\n")
	other := filepath.Join(dir, "other.sig")
	writeText(t, other, "\t"+s2+"\r\n")

	for i, run := range []struct {
		args []string
		want string
	}{
		{[]string{"tp", "update", "--file", f, "-o", "json"}, s1},
		{[]string{"tp", "update", "--file", f, "--signature", other, "-o", "json"}, s2},
	} {
		r := runSpecsCLI(homeEnv(home), tpSpecs(), run.args...)
		if r.code != 0 {
			t.Fatalf("run %d: exit %d: %s", i+1, r.code, r.stderr)
		}
		ups := h.proxy.posts(tpUploadPath)
		if len(ups) != i+1 {
			t.Fatalf("run %d: %d uploads, want %d", i+1, len(ups), i+1)
		}
		q, err := url.ParseQuery(ups[i].query)
		if err != nil {
			t.Fatal(err)
		}
		if got := q.Get("signature"); got != run.want {
			t.Errorf("run %d: signature = %q, want %q", i+1, got, run.want)
		}
	}
}

// TC-SM-33 (WI 2.4; FR-13, OOS-1): a multi-host update uploads once per host
// and posts each host's own uploadId.
func TestTCSM33_MultiHostUpdateUploadsOncePer(t *testing.T) {
	h1, h2 := startTPHost(t, "h1"), startTPHost(t, "h2")
	home := writeClientConfig(t, "h1",
		clientHost{name: "h1", url: h1.proxy.URL, token: h1.s.token, tags: []string{"fleet"}},
		clientHost{name: "h2", url: h2.proxy.URL, token: h2.s.token, tags: []string{"fleet"}})
	f := writeFile(t, t.TempDir(), "pkg.tar.gz", 64<<10)
	writeText(t, f+".sig", sig88+"\n")

	// --host takes names; the hosts tagged fleet are --tag fleet.
	r := runSpecsCLI(homeEnv(home), tpSpecs(), "tp", "update", "--file", f, "--tag", "fleet", "--yes", "-o", "json")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s", r.code, r.stderr)
	}
	var printed []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil || len(printed) != 2 {
		t.Fatalf("stdout %q is not two operations: %v", r.stdout, err)
	}
	results := map[string]string{}
	for _, obj := range printed {
		if obj["status"] != string(apitypes.StatusSucceeded) {
			t.Errorf("operation %v did not succeed", obj)
		}
		host, _ := obj["host"].(string)
		result, _ := obj["result"].(map[string]any)
		id, _ := result["uploadId"].(string)
		results[host] = id
	}
	for _, h := range []struct {
		name string
		h    *tpHost
	}{{"h1", h1}, {"h2", h2}} {
		ups, ops := h.h.proxy.posts(tpUploadPath), h.h.proxy.posts(tpUpdatePath)
		if len(ups) != 1 || len(ops) != 1 {
			t.Errorf("%s: %d uploads and %d op POSTs, want 1 and 1", h.name, len(ups), len(ops))
			continue
		}
		issued, _, _ := h.h.p.state()
		var body map[string]any
		if err := json.Unmarshal(ops[0].body, &body); err != nil {
			t.Fatal(err)
		}
		if len(issued) != 1 || body["uploadId"] != issued[0] || !strings.HasPrefix(issued[0], h.name+"-") {
			t.Errorf("%s: op uploadId %v, want its own upload's %v", h.name, body["uploadId"], issued)
		}
		if len(issued) == 1 && results[h.name] != issued[0] {
			t.Errorf("%s: printed result uploadId %q, want %q", h.name, results[h.name], issued[0])
		}
	}
}

// TC-SM-34 (WI 2.4; FR-13): generated help shows --file, --signature and
// --allow-downgrade, and the upload op has no command.
func TestTCSM34_GeneratedHelpShowsFileSignatureAnd(t *testing.T) {
	specs := tpSpecs()
	groups, err := groupOps(specs)
	if err != nil {
		t.Fatalf("groupOps: %v", err)
	}
	for _, g := range groups {
		for _, op := range g.ops {
			if op.ID == tpUpload.Op().ID {
				t.Errorf("command %s %s is bound to the upload op", g.noun, g.verb)
			}
		}
	}

	a := testApp(specs)
	root := a.newRoot()
	cmd, _, err := root.Find([]string{"tp", "update"})
	if err != nil || cmd.Name() != "update" {
		t.Fatalf("tp update: %v", err)
	}
	r := runSpecsCLI(Env{}, specs, "tp", "update", "--help")
	if r.code != 0 {
		t.Fatalf("help exited %d: %s", r.code, r.stderr)
	}
	help := r.stdout
	for _, want := range []string{"--file", "--signature", "--allow-downgrade"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %s:\n%s", want, help)
		}
	}
	if !strings.Contains(cmd.Flags().Lookup("file").Usage, "Required") {
		t.Errorf("--file usage %q does not say it is required", cmd.Flags().Lookup("file").Usage)
	}
	if strings.Contains(help, "--upload-id") || cmd.Flags().Lookup("upload-id") != nil {
		t.Errorf("help offers --upload-id:\n%s", help)
	}
	if !strings.Contains(cmd.Example, "--file") {
		t.Errorf("no example has --file:\n%s", cmd.Example)
	}

	r = runSpecsCLI(Env{}, specs, "command", "get")
	if r.code != 0 {
		t.Fatalf("command get exited %d: %s", r.code, r.stderr)
	}
	sawUpdate := false
	for line := range strings.Lines(r.stdout) {
		var row commandRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("command get line %q: %v", line, err)
		}
		if row.Command == "nexus tp update" {
			sawUpdate = true
		}
		if slices.Contains(row.Ops, tpUpload.Op().ID) {
			t.Errorf("command get lists %s for the upload op", row.Command)
		}
	}
	if !sawUpdate {
		t.Errorf("command get does not list tp update:\n%s", r.stdout)
	}
}

// preCheckEnv is a client configuration for h, with FIFOs at the release
// signing key paths: a run that opened either for reading would block.
func preCheckEnv(t *testing.T, h *tpHost) Env {
	t.Helper()
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: h.s.token})
	keyFIFO := filepath.Join(home, ".config", "nexus", "release-signing.key")
	envFIFO := filepath.Join(t.TempDir(), "env.key")
	for _, p := range []string{keyFIFO, envFIFO} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Env{Getenv: func(k string) string {
		switch k {
		case "HOME":
			return home
		case "NEXUS_RELEASE_KEY":
			return envFIFO
		}
		return ""
	}}
}

// runBounded runs the CLI and fails the test if it takes over 5 s.
func runBounded(t *testing.T, env Env, args ...string) runResult {
	t.Helper()
	done := make(chan runResult, 1)
	go func() { done <- runSpecsCLI(env, tpSpecs(), args...) }()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%v did not return within 5s", args)
	}
	return runResult{}
}

// TC-SM-E12 (WI 2.4; FR-13): an explicit --signature path that is missing
// does not fall back to <file>.sig.
func TestTCSME12_AnExplicitSignaturePathThatIs(t *testing.T) {
	h := startTPHost(t, "h1")
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: h.s.token})
	f := writeFile(t, t.TempDir(), "pkg.tar.gz", 1024)
	writeText(t, f+".sig", sig88)
	missing := filepath.Join(t.TempDir(), "nonexistent.sig")

	r := runSpecsCLI(homeEnv(home), tpSpecs(), "tp", "update", "--file", f, "--signature", missing)
	if r.code != ExitUsage {
		t.Errorf("exit %d, want 2: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, missing) {
		t.Errorf("stderr does not name %s: %s", missing, r.stderr)
	}
	if n := len(h.proxy.requests()); n != 0 {
		t.Errorf("%d requests reached the server, want 0", n)
	}
}

// TC-SM-ERR18 (WI 2.4; FR-13, FR-15): CLI pre-check failures exit 2 with no
// network, and the private key is never read.
func TestTCSMERR18_CLIPreCheckFailuresExit2(t *testing.T) {
	h := startTPHost(t, "h1")
	env := preCheckEnv(t, h)
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.tar.gz")
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(dir, "sparse.tar.gz")
	sf, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Truncate(tpCeiling + 1); err != nil {
		t.Fatal(err)
	}
	if err := sf.Close(); err != nil {
		t.Fatal(err)
	}
	writeText(t, sparse+".sig", sig88)
	noSig := writeFile(t, dir, "nosig.tar.gz", 1024)
	bigSig := writeFile(t, dir, "bigsig.tar.gz", 1024)
	writeText(t, bigSig+".sig", strings.Repeat("A", 4097))

	for _, tc := range []struct {
		name, file, names, reason string
	}{
		{"missing file", missing, missing, "no such file"},
		{"directory", sub, sub, "not a regular file"},
		{"over the ceiling", sparse, sparse, "limit"},
		{"no .sig", noSig, noSig + ".sig", "no such file"},
		{"oversize .sig", bigSig, bigSig + ".sig", "limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runBounded(t, env, "tp", "update", "--file", tc.file)
			if r.code != ExitUsage {
				t.Errorf("exit %d, want 2: %s", r.code, r.stderr)
			}
			if !strings.Contains(r.stderr, tc.names) || !strings.Contains(r.stderr, tc.reason) {
				t.Errorf("stderr does not name %s and %q: %s", tc.names, tc.reason, r.stderr)
			}
		})
	}
	t.Run("no --file", func(t *testing.T) {
		if r := runBounded(t, env, "tp", "update", "-o", "json"); r.code != ExitUsage || !strings.Contains(r.stderr, "--file") {
			t.Errorf("exit %d, want 2 naming --file: %s", r.code, r.stderr)
		}
	})
	if n := len(h.proxy.requests()); n != 0 {
		t.Errorf("%d requests reached the proxy, want 0", n)
	}
}

// TC-SM-ERR19 (WI 2.4; FR-13, FR-6): a server 413 or 403 on the upload
// stops the CLI before the op POST.
func TestTCSMERR19_AServer413Or403On(t *testing.T) {
	h := startTPHost(t, "h1")
	h.p.maxBytes.Store(1024)
	f := writeFile(t, t.TempDir(), "pkg.tar.gz", 2048)
	writeText(t, f+".sig", sig88)

	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: h.s.token})
	r := runSpecsCLI(homeEnv(home), tpSpecs(), "tp", "update", "--file", f, "-o", "json")
	if r.code != ExitUsage {
		t.Errorf("413: exit %d, want 2: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatalf("413: stderr %q does not end with an error object: %v", r.stderr, err)
	}
	if e["type"] != "urn:nexus:problem:upload-too-large" || e["maxBytes"] != float64(1024) {
		t.Errorf("413: error = %v, want upload-too-large with maxBytes 1024", e)
	}

	readOnly, _, err := auth.Create(h.s.stateDir, auth.CreateRequest{Name: "ro", Scopes: []string{tpReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	home = writeClientConfig(t, "h1", clientHost{name: "h1", url: h.proxy.URL, token: readOnly})
	h.p.maxBytes.Store(tpCeiling)
	r = runSpecsCLI(homeEnv(home), tpSpecs(), "tp", "update", "--file", f)
	if r.code != ExitAuth || !strings.Contains(r.stderr, "forbidden") {
		t.Errorf("403: exit %d, want 3 forbidden: %s", r.code, r.stderr)
	}
	if ops := h.proxy.posts(tpUpdatePath); len(ops) != 0 {
		t.Errorf("%d POSTs reached %s, want 0", len(ops), tpUpdatePath)
	}
	if ups := h.proxy.posts(tpUploadPath); len(ups) != 2 {
		t.Errorf("%d uploads, want 2", len(ups))
	}
}
