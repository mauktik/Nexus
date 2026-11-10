package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/registry"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// updateEngine serves the Engine API calls of check-updates in the shapes of
// Engine API v1.52: GET /containers/json, /containers/{id}/json,
// /images/{id}/json (RepoDigests) and /distribution/{name}/json
// (DistributionInspect: {"Descriptor":{"digest"...},"Platforms":[]}, 401
// for a refused registry). It records each distribution call with the
// X-Registry-Auth it carried. T1 (TC-111 to TC-115, TC-ERR34, TC-ERR35)
// checks the same code against a real Engine and a real registry.
type updateEngine struct {
	socket     string
	containers map[string]map[string]any // Name -> inspect body
	images     map[string][]string       // Image ID -> RepoDigests
	// registry answers by reference: a digest, or an HTTP status with a
	// message.
	registry map[string]registryAnswer
	delay    time.Duration
	mu       sync.Mutex
	dist     []distCall
	inflight atomic.Int64
	peak     atomic.Int64
	imgCalls atomic.Int64
}

type registryAnswer struct {
	digest  string
	status  int
	message string
}

type distCall struct {
	ref  string
	auth string // X-Registry-Auth, "" when absent
}

const (
	digestA  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imgOne   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	imgTwo   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	imgThree = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func newUpdateEngine(t *testing.T) *updateEngine {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "du")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &updateEngine{socket: filepath.Join(dir, "d.sock"), containers: map[string]map[string]any{},
		images: map[string][]string{}, registry: map[string]registryAnswer{}}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *updateEngine) add(name, ref, image string, autoRemove bool) {
	f.containers[name] = map[string]any{
		"Id": strings.Repeat(name[:1], 4) + name, "Name": "/" + name, "Image": image,
		"State":      map[string]any{"Status": "running", "Running": true},
		"Config":     map[string]any{"Image": ref, "Labels": map[string]string{}},
		"HostConfig": map[string]any{"AutoRemove": autoRemove},
	}
}

func (f *updateEngine) id(name string) string { return f.containers[name]["Id"].(string) }

func (f *updateEngine) calls() []distCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dist)
}

func (f *updateEngine) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		w.Header().Set("Api-Version", "1.52")
		return
	}
	m := versioned.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodGet {
		http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
		return
	}
	path := m[1]
	switch {
	case path == "/containers/json":
		var out []map[string]any
		for name, c := range f.containers {
			out = append(out, map[string]any{"Id": c["Id"], "Names": []string{"/" + name}, "ImageID": c["Image"]})
		}
		writeJSON(w, out)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		for name, c := range f.containers {
			if name == ref || c["Id"] == ref {
				writeJSON(w, c)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"message": "No such container: " + ref})
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		f.imgCalls.Add(1)
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		d, ok := f.images[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "No such image: " + id})
			return
		}
		writeJSON(w, map[string]any{"Id": id, "RepoDigests": d})
	case strings.HasPrefix(path, "/distribution/") && strings.HasSuffix(path, "/json"):
		ref, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/distribution/"), "/json"))
		f.mu.Lock()
		f.dist = append(f.dist, distCall{ref: ref, auth: r.Header.Get(registry.AuthHeader)})
		f.mu.Unlock()
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			p := f.peak.Load()
			if n <= p || f.peak.CompareAndSwap(p, n) {
				break
			}
		}
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		a, ok := f.registry[ref]
		switch {
		case !ok:
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]string{"message": "no answer configured for " + ref})
		case a.status != 0:
			w.WriteHeader(a.status)
			writeJSON(w, map[string]string{"message": a.message})
		default:
			writeJSON(w, map[string]any{
				"Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": a.digest, "size": 1234},
				"Platforms":  []any{},
			})
		}
	default:
		http.Error(w, `{"message":"unexpected call"}`, http.StatusNotImplemented)
	}
}

// updateProvider is a provider on f whose docker.configDir is configDir.
func updateProvider(t *testing.T, f *updateEngine, configDir string, logs *bytes.Buffer) *Provider {
	t.Helper()
	var w = &bytes.Buffer{}
	if logs != nil {
		w = logs
	}
	p := New(Options{Host: "testhost", Logger: slog.New(slog.NewJSONHandler(w, nil)), CallTimeout: 5 * time.Second})
	if err := p.Configure(func(into any) error {
		c := into.(*config.DockerConfig)
		c.Socket, c.ConfigDir = f.socket, configDir
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeConfig(t *testing.T, js string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCheck(t *testing.T, p *Provider) (map[string]spec.UpdateCheck, string) {
	t.Helper()
	r, out := newRun()
	res, err := p.checkUpdates(context.Background(), spec.CheckUpdatesInput{}, r)
	if err != nil {
		t.Fatalf("check-updates: %v", err)
	}
	got := map[string]spec.UpdateCheck{}
	for _, it := range res.Items {
		if it.Host != "testhost" || it.ID != "/providers/docker/containers/"+it.Name {
			t.Errorf("item %s: host %q id %q", it.Name, it.Host, it.ID)
		}
		got[it.Name] = it.Properties.Update
	}
	return got, out.String()
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// TC-111, TC-112 (T0 part): each classification from the Engine's answers:
// the RepoDigests entry for the reference's repository against the
// distribution descriptor's digest, never .Image; every notUpdatable reason
// is decided without a registry call.
func TestCheckUpdatesClassifies(t *testing.T) {
	f := newUpdateEngine(t)
	const repo = "reg.example:5000/team/app"
	f.images[imgOne] = []string{"other.example/mirror/app@" + digestB, repo + "@" + digestA}
	f.images[imgTwo] = []string{}
	f.add("current", repo+":v1", imgOne, false)
	f.add("stale", repo+":latest", imgOne, false)
	f.add("pinned", repo+"@"+digestA, imgOne, false)
	f.add("built", "localapp:dev", imgTwo, false)
	f.add("byid", imgOne, imgOne, false)
	f.add("byshortid", "111111111111", imgOne, false)
	f.add("rm", repo+":v1", imgOne, true)
	f.add("otherrepo", "other.example/mirror/app:latest", imgOne, false)
	// The containerd store gives a never-pushed local build a RepoDigest
	// (name@target); its registry has no such manifest.
	f.images[imgThree] = []string{"reg.example:5000/team/local@" + digestB}
	f.add("neverpushed", "reg.example:5000/team/local:dev", imgThree, false)
	f.registry["reg.example:5000/team/local:dev"] = registryAnswer{status: http.StatusNotFound,
		message: "manifest unknown: manifest unknown"}
	f.registry[repo+":v1"] = registryAnswer{digest: digestA}
	f.registry[repo+":latest"] = registryAnswer{digest: digestB}
	f.registry["other.example/mirror/app:latest"] = registryAnswer{digest: digestB}
	// A stale image is only reported updateAvailable when the registry also
	// serves the digest it runs (CR-2).
	f.registry[repo+"@"+digestA] = registryAnswer{digest: digestA}

	p := updateProvider(t, f, t.TempDir(), nil)
	got, out := runCheck(t, p)
	want := map[string]struct{ status, reason string }{
		"current":     {spec.UpdateUpToDate, ""},
		"stale":       {spec.UpdateAvailable, ""},
		"pinned":      {spec.UpdateNotUpdatable, spec.ReasonDigestPinned},
		"built":       {spec.UpdateNotUpdatable, spec.ReasonLocallyBuilt},
		"byid":        {spec.UpdateNotUpdatable, spec.ReasonNoRegistrySource},
		"byshortid":   {spec.UpdateNotUpdatable, spec.ReasonNoRegistrySource},
		"rm":          {spec.UpdateNotUpdatable, spec.ReasonAutoRemove},
		"otherrepo":   {spec.UpdateUpToDate, ""},
		"neverpushed": {spec.UpdateNotUpdatable, spec.ReasonLocallyBuilt},
	}
	for name, w := range want {
		g := got[name]
		if g.Status != w.status || g.Reason != w.reason || g.CheckedAt == nil {
			t.Errorf("%s: %+v, want %s %s", name, g, w.status, w.reason)
		}
	}
	if g := got["current"]; g.CurrentDigest != digestA || g.LatestDigest != digestA {
		t.Errorf("current digests %+v", g)
	}
	if g := got["stale"]; g.CurrentDigest != digestA || g.LatestDigest != digestB {
		t.Errorf("stale digests %+v", g)
	}
	if d := got["built"].Detail; !strings.Contains(d, "script") || !strings.Contains(d, "built") {
		t.Errorf("locally built detail does not point to scripts (ASM-8): %q", d)
	}
	// The tag lookups: one per reference. The probes for the running digest
	// (name@digest, CR-2) are counted apart.
	refs := map[string]bool{}
	var tagCalls, probes []string
	for _, c := range f.calls() {
		if strings.Contains(c.ref, "@") {
			probes = append(probes, c.ref)
			continue
		}
		refs[c.ref] = true
		tagCalls = append(tagCalls, c.ref)
	}
	if !slices.Equal(probes, []string{repo + "@" + digestA}) {
		t.Errorf("running-digest probes %v, want one, for the stale image only", probes)
	}
	if d := got["neverpushed"].Detail; !strings.Contains(d, "never pushed") || !strings.Contains(d, "script") {
		t.Errorf("never-pushed detail: %q", d)
	}
	if !maps(refs, repo+":v1", repo+":latest", "other.example/mirror/app:latest", "reg.example:5000/team/local:dev") {
		t.Errorf("registry calls %v: want one per checked reference and none for notUpdatable", refs)
	}
	if len(tagCalls) != 4 {
		t.Errorf("registry calls %v: the shared reference must be asked once", tagCalls)
	}
	if f.imgCalls.Load() > 3 {
		t.Errorf("%d image inspections for 3 images", f.imgCalls.Load())
	}
	if !strings.Contains(out, "stale: updateAvailable") || !strings.Contains(out, "built: notUpdatable (locallyBuilt)") {
		t.Errorf("progress output:\n%s", out)
	}
}

// CR-2: on the containerd image store a local build tagged with a name the
// registry also serves has a RepoDigest (name@local target) that differs
// from the registry's digest for the tag. It is not an update: the registry
// has never served the running digest, so the image is locally built. When
// the registry cannot say, the status is unknown, never updateAvailable.
func TestCheckUpdatesLocalBuildBehindAServedTag(t *testing.T) {
	f := newUpdateEngine(t)
	const repo = "reg.example:5000/team/app"
	f.images[imgOne] = []string{repo + "@" + digestA}
	f.images[imgTwo] = []string{"reg.example:5000/team/flaky@" + digestA}
	f.add("local", repo+":latest", imgOne, false)
	f.add("flaky", "reg.example:5000/team/flaky:latest", imgTwo, false)
	f.registry[repo+":latest"] = registryAnswer{digest: digestB}
	f.registry[repo+"@"+digestA] = registryAnswer{status: http.StatusNotFound, message: "manifest unknown: manifest unknown"}
	f.registry["reg.example:5000/team/flaky:latest"] = registryAnswer{digest: digestB}
	f.registry["reg.example:5000/team/flaky@"+digestA] = registryAnswer{status: http.StatusInternalServerError, message: "registry unavailable"}

	got, _ := runCheck(t, updateProvider(t, f, t.TempDir(), nil))
	if g := got["local"]; g.Status != spec.UpdateNotUpdatable || g.Reason != spec.ReasonLocallyBuilt || !strings.Contains(g.Detail, "script") {
		t.Errorf("local build: %+v, want notUpdatable locallyBuilt", g)
	}
	if g := got["flaky"]; g.Status != spec.UpdateUnknown || g.Reason != spec.ReasonRegistryError {
		t.Errorf("unanswered probe: %+v, want unknown registryError", g)
	}
}

func maps(m map[string]bool, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if !m[k] {
			return false
		}
	}
	return true
}

// TC-ERR34 (T0 part): with the registry digest equal to the running one, so
// a wrong path would say upToDate, every failure is unknown with a reason:
// a registry 500, an unreachable registry, a refusal without credential, a
// refusal with only a helper (named, never run), a refused auths
// credential, an unreadable config.json, a descriptor without a digest, and
// an Engine that cannot inspect the image.
func TestNoUpToDateAfterAnError(t *testing.T) {
	const ref = "reg.example:5000/team/app:v1"
	refusal := registryAnswer{status: http.StatusUnauthorized,
		message: "Head \"http://reg.example:5000/v2/team/app/manifests/v1\": no basic auth credentials"}
	for _, tc := range []struct {
		name, config string
		answer       registryAnswer
		noImage      bool
		reason       string
		detail       []string
	}{
		{name: "registry 500", answer: registryAnswer{status: 500, message: "received unexpected HTTP status: 500 Internal Server Error"},
			reason: spec.ReasonRegistryError, detail: []string{"500"}},
		{name: "registry down", answer: registryAnswer{status: 500, message: "Get \"http://reg.example:5000/v2/\": dial tcp 127.0.0.1:5000: connect: connection refused"},
			reason: spec.ReasonRegistryError, detail: []string{"connection refused"}},
		{name: "auth required", answer: refusal, reason: spec.ReasonAuthRequired, detail: []string{"docker login", "providers.docker.configDir"}},
		{name: "hub ambiguous", answer: registryAnswer{status: 500, message: "pull access denied for team/app, repository " +
			"does not exist or may require 'docker login': denied: requested access to the resource is denied"},
			reason: spec.ReasonAuthRequired, detail: []string{"never pushed", "script"}},
		{name: "cred helper", config: `{"auths":{"reg.example:5000":{}},"credHelpers":{"reg.example:5000":"nx"}}`, answer: refusal,
			reason: spec.ReasonCredentialHelper, detail: []string{"docker-credential-nx", "credHelpers", "never runs", "docker login", "providers.docker.configDir"}},
		{name: "creds store", config: `{"credsStore":"nx"}`, answer: refusal,
			reason: spec.ReasonCredentialHelper, detail: []string{"docker-credential-nx", "credsStore"}},
		{name: "credential refused", config: `{"auths":{"reg.example:5000":{"auth":"` + basicAuth("u", "wrong-secret") + `"}}}`,
			answer: registryAnswer{status: 401, message: "unauthorized: authentication required"}, reason: spec.ReasonCredentialRefused},
		{name: "bad config", config: `{"auths": {`, answer: registryAnswer{digest: digestA}, reason: spec.ReasonCredentialConfig, detail: []string{"not valid JSON"}},
		{name: "bad auth value", config: `{"auths":{"reg.example:5000":{"auth":"%%%"}}}`, answer: registryAnswer{digest: digestA}, reason: spec.ReasonCredentialConfig},
		{name: "no digest", answer: registryAnswer{digest: "not-a-digest"}, reason: spec.ReasonRegistryError},
		{name: "image gone", answer: registryAnswer{digest: digestA}, noImage: true, reason: spec.ReasonEngineError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateEngine(t)
			if !tc.noImage {
				f.images[imgOne] = []string{"reg.example:5000/team/app@" + digestA}
			}
			f.add("web", ref, imgOne, false)
			f.registry[ref] = tc.answer
			dir := t.TempDir()
			if tc.config != "" {
				dir = writeConfig(t, tc.config)
			}
			var logs bytes.Buffer
			got, _ := runCheck(t, updateProvider(t, f, dir, &logs))
			g := got["web"]
			if g.Status != spec.UpdateUnknown || g.Reason != tc.reason || g.Detail == "" {
				t.Fatalf("%+v, want unknown %s", g, tc.reason)
			}
			for _, d := range tc.detail {
				if !strings.Contains(g.Detail, d) {
					t.Errorf("detail %q does not mention %q", g.Detail, d)
				}
			}
			if strings.Contains(g.Detail+logs.String(), "wrong-secret") || strings.Contains(g.Detail+logs.String(), basicAuth("u", "wrong-secret")) {
				t.Errorf("the credential leaked into the reason or the log")
			}
		})
	}
}

// A-61, A-14: the credential from auths goes only with references of its
// own registry, as base64url JSON with its server address; Docker Hub
// matches its historical key; a registry without one is asked anonymously.
func TestRegistryAuthGoesOnlyToItsRegistry(t *testing.T) {
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"reg.example:5000/a@" + digestA, "other.example/b@" + digestA, "busybox@" + digestA,
		"reg.example:5001/c@" + digestA}
	f.add("mine", "reg.example:5000/a:v1", imgOne, false)
	f.add("theirs", "other.example/b:v1", imgOne, false)
	f.add("hub", "busybox:latest", imgOne, false)
	f.add("port", "reg.example:5001/c:v1", imgOne, false)
	for _, r := range []string{"reg.example:5000/a:v1", "other.example/b:v1", "docker.io/library/busybox:latest", "reg.example:5001/c:v1"} {
		f.registry[r] = registryAnswer{digest: digestA}
	}
	dir := writeConfig(t, `{"auths":{"https://reg.example:5000/v1/":{"auth":"`+basicAuth("alice", "s3cret:x")+`"},`+
		`"https://index.docker.io/v1/":{"username":"hubuser","password":"hubpass"},"other.example":{}},`+
		`"credsStore":"desktop"}`)
	var logs bytes.Buffer
	got, out := runCheck(t, updateProvider(t, f, dir, &logs))
	for name, g := range got {
		if g.Status != spec.UpdateUpToDate {
			t.Errorf("%s: %+v", name, g)
		}
	}
	auth := map[string]registry.AuthConfig{}
	for _, c := range f.calls() {
		if c.auth == "" {
			auth[c.ref] = registry.AuthConfig{}
			continue
		}
		b, err := base64.URLEncoding.DecodeString(c.auth)
		if err != nil {
			t.Fatalf("%s: X-Registry-Auth is not base64url: %v", c.ref, err)
		}
		var ac registry.AuthConfig
		if err := json.Unmarshal(b, &ac); err != nil {
			t.Fatal(err)
		}
		auth[c.ref] = ac
	}
	want := map[string]registry.AuthConfig{
		"reg.example:5000/a:v1":            {Username: "alice", Password: "s3cret:x", ServerAddress: "https://reg.example:5000/v1/"},
		"docker.io/library/busybox:latest": {Username: "hubuser", Password: "hubpass", ServerAddress: hubAuthKey},
		"other.example/b:v1":               {},
		"reg.example:5001/c:v1":            {},
	}
	if len(auth) != len(want) {
		t.Fatalf("calls %+v", auth)
	}
	for ref, w := range want {
		if auth[ref] != w {
			t.Errorf("%s: sent %+v, want %+v", ref, auth[ref], w)
		}
	}
	for _, secret := range []string{"s3cret", "hubpass", basicAuth("alice", "s3cret:x")} {
		if strings.Contains(logs.String()+out, secret) {
			t.Errorf("%q appears in the log or the output", secret)
		}
	}
}

// TC-115 (T0 part), A-13: list and get report the stored status, notChecked
// before a check and for a container whose image changed since; the status
// filter applies before paging; listing never calls the registry.
func TestStoredStatusMergesIntoInventory(t *testing.T) {
	f := newUpdateEngine(t)
	const repo = "reg.example:5000/team/app"
	f.images[imgOne] = []string{repo + "@" + digestA}
	f.images[imgTwo] = []string{repo + "@" + digestB}
	f.add("a-current", repo+":v1", imgOne, false)
	f.add("b-stale", repo+":latest", imgOne, false)
	f.add("c-built", "localapp:dev", imgTwo, false)
	f.add("d-stale", repo+":latest", imgOne, false)
	f.registry[repo+":v1"] = registryAnswer{digest: digestA}
	f.registry[repo+":latest"] = registryAnswer{digest: digestB}
	f.registry[repo+"@"+digestA] = registryAnswer{digest: digestA}
	p := updateProvider(t, f, t.TempDir(), nil)
	ctx := context.Background()

	names := func(in spec.ListInput) ([]string, string) {
		t.Helper()
		l, err := p.list(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range l.Items {
			out = append(out, c.Name+"="+c.Properties.Update.Status)
		}
		return out, l.NextCursor
	}
	all, _ := names(spec.ListInput{})
	if !slices.Equal(all, []string{"a-current=notChecked", "b-stale=notChecked", "c-built=notChecked", "d-stale=notChecked"}) {
		t.Fatalf("before a check: %v", all)
	}
	if got, _ := names(spec.ListInput{Filter: []string{"status=updateAvailable"}}); len(got) != 0 {
		t.Fatalf("filter before a check: %v", got)
	}
	runCheck(t, p)
	calls := len(f.calls())

	got, cursor := names(spec.ListInput{Filter: []string{"status=updateAvailable"}, Limit: 1})
	if !slices.Equal(got, []string{"b-stale=updateAvailable"}) || cursor == "" {
		t.Fatalf("filtered page 1: %v cursor %q", got, cursor)
	}
	got, cursor = names(spec.ListInput{Filter: []string{"status=updateAvailable"}, Limit: 1, Cursor: cursor})
	if !slices.Equal(got, []string{"d-stale=updateAvailable"}) || cursor != "" {
		t.Fatalf("filtered page 2: %v cursor %q", got, cursor)
	}
	got, _ = names(spec.ListInput{Filter: []string{"status=upToDate", "status=notUpdatable"}})
	if !slices.Equal(got, []string{"a-current=upToDate", "c-built=notUpdatable"}) {
		t.Fatalf("two statuses: %v", got)
	}
	one, err := p.get(ctx, spec.ContainerInput{Name: "c-built"})
	if err != nil || one.Properties.Update.Reason != spec.ReasonLocallyBuilt || one.Properties.Update.CheckedAt == nil {
		t.Fatalf("get: %+v %v", one.Properties.Update, err)
	}

	// d-stale now runs another image: its stored result no longer applies.
	f.containers["d-stale"]["Image"] = imgTwo
	if got, _ := names(spec.ListInput{Filter: []string{"status=notChecked"}}); !slices.Equal(got, []string{"d-stale=notChecked"}) {
		t.Fatalf("after the image changed: %v", got)
	}
	if len(f.calls()) != calls {
		t.Fatalf("listing called the registry: %d calls, was %d", len(f.calls()), calls)
	}
	if _, err := p.list(ctx, spec.ListInput{Filter: []string{"state=running"}}); slugOf(err) != problem.ValidationFailed {
		t.Fatalf("unknown filter key: %v", err)
	}
}

// The stored results go to the store the daemon hands over, and a second
// provider on the same store reads them (a restart).
func TestCheckResultsUseTheDaemonStore(t *testing.T) {
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"reg.example:5000/a@" + digestA}
	f.add("web", "reg.example:5000/a:v1", imgOne, false)
	f.registry["reg.example:5000/a:v1"] = registryAnswer{digest: digestB}
	f.registry["reg.example:5000/a@"+digestA] = registryAnswer{digest: digestA}
	kv := &memKV{}
	p := updateProvider(t, f, t.TempDir(), nil)
	p.UseKV(kv)
	runCheck(t, p)
	if _, ok, _ := kv.KVGet(checkKVKey); !ok {
		t.Fatalf("nothing stored under %s", checkKVKey)
	}
	again := updateProvider(t, f, t.TempDir(), nil)
	again.UseKV(kv)
	c, err := again.get(context.Background(), spec.ContainerInput{Name: "web"})
	if err != nil || c.Properties.Update.Status != spec.UpdateAvailable {
		t.Fatalf("after a restart: %+v %v", c.Properties.Update, err)
	}
	var _ provider.KVUser = again
}

// FR-11: registry calls run at most 8 at a time, each bounded by the call
// timeout; a registry that does not answer in time is unknown.
func TestRegistryCallsAreBounded(t *testing.T) {
	f := newUpdateEngine(t)
	f.delay = 150 * time.Millisecond
	for i := range 20 {
		ref := "reg.example:5000/app" + string(rune('a'+i)) + ":v1"
		img := "sha256:" + strings.Repeat(string(rune('a'+i%6)), 63) + string(rune('0'+i%10))
		f.images[img] = []string{strings.TrimSuffix(ref, ":v1") + "@" + digestA}
		f.add("c"+string(rune('a'+i)), ref, img, false)
		f.registry[ref] = registryAnswer{digest: digestA}
	}
	got, _ := runCheck(t, updateProvider(t, f, t.TempDir(), nil))
	if len(got) != 20 {
		t.Fatalf("%d results", len(got))
	}
	if peak := f.peak.Load(); peak > checkConcurrency || peak < 2 {
		t.Fatalf("peak concurrent registry calls %d, want 2..%d", peak, checkConcurrency)
	}

	slow := newUpdateEngine(t)
	slow.delay = 3 * time.Second
	slow.images[imgOne] = []string{"reg.example:5000/a@" + digestA}
	slow.add("web", "reg.example:5000/a:v1", imgOne, false)
	slow.registry["reg.example:5000/a:v1"] = registryAnswer{digest: digestA}
	p := updateProvider(t, slow, t.TempDir(), nil)
	p.timeout = 300 * time.Millisecond
	got, _ = runCheck(t, p)
	if g := got["web"]; g.Status != spec.UpdateUnknown || !strings.Contains(g.Detail, "did not answer within") {
		t.Fatalf("slow registry: %+v", g)
	}
}

// FR-11, A-13, TC-ERR35 (T0 part): check-updates is a read-only,
// long-running, in-process op on the reject lane docker-check, under the
// read scope, and the list declares the status filter.
func TestCheckUpdatesDeclaration(t *testing.T) {
	op := spec.CheckUpdates.Op()
	if op.Mode != def.ModeLongRunning || op.Risk != def.RiskReadOnly || op.Scope.Name != spec.ReadScope ||
		op.Kind != def.KindProviderAction || op.Action != "check-updates" || op.CLI.Noun != "containerupdate" ||
		op.CLI.Verb != "get" || op.MCP == nil || op.MCP.Tool != "check_container_updates" {
		t.Fatalf("declaration %+v", op)
	}
	for _, phrase := range []string{"Side effects:", "Long-running", "409", "never run", "unknown, never upToDate"} {
		if !strings.Contains(op.Description, phrase) {
			t.Errorf("description lacks %q", phrase)
		}
	}
	p := New(Options{})
	var bound bool
	for _, b := range p.Bindings() {
		if b.Op().ID != op.ID {
			continue
		}
		fns, ok := b.InProcess()
		if !ok {
			t.Fatalf("check-updates is bound %s, want in-process", b.Kind())
		}
		lane, err := fns.Lane(context.Background(), spec.CheckUpdatesInput{})
		if err != nil || lane.Key != "docker-check" || lane.Policy != provider.LaneReject {
			t.Fatalf("lane %+v %v", lane, err)
		}
		bound = true
	}
	if !bound {
		t.Fatal("check-updates is not bound")
	}
	list := spec.List.Op()
	if len(list.Filters) != 1 || list.Filters[0].Key != "status" {
		t.Fatalf("list filters %+v", list.Filters)
	}
	s, err := list.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		js string
		ok bool
	}{
		{`{"filter":["status=updateAvailable"]}`, true},
		{`{"filter":["status=notChecked","status=unknown"]}`, true},
		{`{"filter":["status=bogus"]}`, false},
		{`{"filter":["name=web"]}`, false},
	} {
		if err := validateJSON(t, s, tc.js); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.js, err)
		}
	}
	var _ apitypes.Resource[spec.ContainerUpdateProperties] = spec.ContainerUpdate{}
}
