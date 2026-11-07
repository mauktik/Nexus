package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// fakeEngine serves the Engine API endpoints the provider calls, over a
// Unix socket, in the wire shapes of the Engine API v1.52 reference: a
// /_ping answering Api-Version, versioned paths /v<version>/..., 404 with
// {"message"} for an unknown container, and logs multiplexed with the
// 8-byte stream header unless the container has a TTY. T1 (TC-107, TC-108)
// checks the same code against a real daemon.
type fakeEngine struct {
	socket     string
	containers map[string]fakeContainer // By name
	images     map[string][]string      // Image ID -> RepoDigests
	hang       atomic.Bool              // Accept and never answer (except /_ping when pingOK)
	pingOK     atomic.Bool
	calls      atomic.Int64 // Versioned API calls served
	mu         sync.Mutex
	paths      []string
	srv        *http.Server
}

type fakeContainer struct {
	id, image, ref string
	tty            bool
	logs           []fakeLine
}

type fakeLine struct {
	stream byte // 1 stdout, 2 stderr
	ts     string
	text   string
}

var versioned = regexp.MustCompile(`^/v1\.[0-9]+(/.*)$`)

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	// Unix socket paths are short: t.TempDir under /var/folders is too long.
	dir, err := os.MkdirTemp("/tmp", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeEngine{socket: filepath.Join(dir, "d.sock"), containers: map[string]fakeContainer{}, images: map[string][]string{}}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		if f.hang.Load() && !f.pingOK.Load() {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Api-Version", "1.52")
		_, _ = io.WriteString(w, "OK")
		return
	}
	m := versioned.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
		return
	}
	f.calls.Add(1)
	f.mu.Lock()
	f.paths = append(f.paths, m[1])
	f.mu.Unlock()
	if f.hang.Load() {
		<-r.Context().Done()
		return
	}
	path := m[1]
	switch {
	case path == "/containers/json":
		if r.URL.Query().Get("all") != "1" {
			http.Error(w, "the provider must list stopped containers", http.StatusBadRequest)
			return
		}
		var out []map[string]any
		for name, c := range f.containers {
			out = append(out, map[string]any{"Id": c.id, "Names": []string{"/" + name}, "Image": c.ref, "ImageID": c.image})
		}
		writeJSON(w, out)
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		c, name, ok := f.lookup(strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "No such container: x"})
			return
		}
		writeJSON(w, map[string]any{
			"Id": c.id, "Name": "/" + name, "Image": c.image,
			"State":      map[string]any{"Status": "running", "Running": true},
			"Config":     map[string]any{"Image": c.ref, "Tty": c.tty, "Labels": map[string]string{}},
			"HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": "always", "MaximumRetryCount": 0}},
			"NetworkSettings": map[string]any{"Ports": map[string]any{
				"80/tcp":  []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "8080"}, {"HostIp": "::", "HostPort": "8080"}},
				"443/tcp": nil,
			}},
		})
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		d, ok := f.images[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "No such image: " + id})
			return
		}
		writeJSON(w, map[string]any{"Id": id, "RepoDigests": d})
	case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/logs"):
		c, _, ok := f.lookup(strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/logs"))
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "No such container"})
			return
		}
		if r.URL.Query().Get("timestamps") != "1" {
			http.Error(w, "the provider must ask for timestamps", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, l := range c.logs {
			payload := []byte(l.ts + " " + l.text)
			if c.tty {
				_, _ = w.Write(payload)
				continue
			}
			hdr := [8]byte{l.stream}
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
			_, _ = w.Write(append(hdr[:], payload...))
		}
	default:
		http.Error(w, `{"message":"unexpected call"}`, http.StatusNotImplemented)
	}
}

func (f *fakeEngine) lookup(ref string) (fakeContainer, string, bool) {
	for name, c := range f.containers {
		if name == ref || c.id == ref {
			return c, name, true
		}
	}
	return fakeContainer{}, "", false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func section(socket string) func(any) error {
	return func(into any) error {
		into.(*config.DockerConfig).Socket = socket
		return nil
	}
}

func newProvider(t *testing.T, socket string, timeout time.Duration) *Provider {
	t.Helper()
	p := New(Options{Host: "testhost", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), CallTimeout: timeout})
	if err := p.Configure(section(socket)); err != nil {
		t.Fatal(err)
	}
	return p
}

func slugOf(err error) problem.Slug {
	var p *problem.Error
	if errors.As(err, &p) {
		return p.Slug()
	}
	return ""
}

// TC-ERR29: only the absolute path of a Unix socket is accepted; every other
// value is a configuration error naming the key, and leaves the running
// client in place.
func TestConfigureAcceptsOnlyAUnixSocketPath(t *testing.T) {
	for _, tc := range []struct {
		socket string
		ok     bool
	}{
		{"tcp://1.2.3.4:2375", false},
		{"http://x", false},
		{"unix:///run/docker.sock", false},
		{"docker.sock", false},
		{"run/docker.sock", false},
		{"", false},
		{"/run/docker.sock", true},
	} {
		t.Run(tc.socket, func(t *testing.T) {
			p := New(Options{})
			err := p.Configure(section(tc.socket))
			if tc.ok {
				if err != nil {
					t.Fatalf("Configure(%q) = %v, want accepted", tc.socket, err)
				}
				if got := p.Settings().Socket; got != tc.socket {
					t.Fatalf("socket in force %q", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "providers.docker.socket") {
				t.Fatalf("Configure(%q) = %v, want a configuration error naming providers.docker.socket", tc.socket, err)
			}
			if p.engine.Load().cli != nil {
				t.Fatal("a refused section left a client")
			}
		})
	}

	p := New(Options{})
	if err := p.Configure(section("/run/docker.sock")); err != nil {
		t.Fatal(err)
	}
	before := p.engine.Load()
	if err := p.Configure(section("tcp://1.2.3.4:2375")); err == nil {
		t.Fatal("tcp accepted on reload")
	}
	if p.engine.Load() != before {
		t.Fatal("a refused reload replaced the running client")
	}
}

// The configuration defaults are those of the shipped file when the
// section sets only some keys.
func TestConfigureAppliesDefaults(t *testing.T) {
	p := New(Options{})
	if err := p.Configure(func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := p.Settings()
	if got.Socket != "/run/docker.sock" || got.ConfigDir != "/root/.docker" ||
		got.UpdateHealthTimeout != 60*time.Second || got.UpdateMaxRuntime != time.Hour {
		t.Fatalf("defaults %+v", got)
	}
}

// Preflight pings and negotiates; with no daemon it is unavailable with a
// reason naming the socket, and calls answer dependency-unavailable.
func TestPreflightAndAbsentDaemon(t *testing.T) {
	f := newFakeEngine(t)
	p := newProvider(t, f.socket, 2*time.Second)
	st := p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled || st.Details["apiVersion"] != "1.52" {
		t.Fatalf("preflight against the fake engine: %+v", st)
	}

	missing := filepath.Join(filepath.Dir(f.socket), "absent.sock")
	q := newProvider(t, missing, 2*time.Second)
	st = q.Preflight(context.Background())
	if st.State != apitypes.ProviderUnavailable || !strings.Contains(st.Reason, missing) ||
		!strings.Contains(st.Reason, "no such file or directory") {
		t.Fatalf("preflight without a daemon: %+v", st)
	}
	_, err := q.list(context.Background(), spec.ListInput{})
	if slugOf(err) != problem.DependencyUnavailable {
		t.Fatalf("list without a daemon: %v", err)
	}
	var perr *problem.Error
	if errors.As(err, &perr); perr.Extensions["provider"] != spec.Name {
		t.Fatalf("problem does not name the provider: %+v", perr)
	}
}

// NFR-3: a daemon that accepts and never answers fails each call within the
// call timeout with dependency-unavailable, and the next call after it
// answers again succeeds on the same provider.
func TestHungDaemonIsBoundedAndRecovers(t *testing.T) {
	f := newFakeEngine(t)
	f.containers["web"] = fakeContainer{id: strings.Repeat("a", 64), image: "sha256:img", ref: "nginx:1",
		logs: []fakeLine{{1, "2026-11-07T10:00:00.000000001Z", "x\n"}}}
	f.images["sha256:img"] = []string{"nginx@sha256:" + strings.Repeat("1", 64)}
	const timeout = 300 * time.Millisecond
	p := newProvider(t, f.socket, timeout)
	f.hang.Store(true)
	f.pingOK.Store(true)

	for name, call := range map[string]func() error{
		"list": func() error { _, err := p.list(context.Background(), spec.ListInput{}); return err },
		"get":  func() error { _, err := p.get(context.Background(), spec.ContainerInput{Name: "web"}); return err },
		"logs": func() error {
			return p.logs(context.Background(), spec.LogsInput{Name: "web"}, func(apitypes.Frame) error { return nil })
		},
	} {
		start := time.Now()
		err := call()
		elapsed := time.Since(start)
		if slugOf(err) != problem.DependencyUnavailable || !strings.Contains(err.Error(), timeout.String()) {
			t.Errorf("%s against a hung daemon: %v", name, err)
		}
		if elapsed > timeout+2*time.Second {
			t.Errorf("%s took %v, want about %v", name, elapsed, timeout)
		}
	}

	f.hang.Store(false)
	got, err := p.get(context.Background(), spec.ContainerInput{Name: "web"})
	if err != nil || got.Name != "web" {
		t.Fatalf("after the daemon answers again: %+v %v", got, err)
	}
}

// Invalid names never reach the daemon; unknown ones are not-found.
func TestNameValidationAndNotFound(t *testing.T) {
	f := newFakeEngine(t)
	p := newProvider(t, f.socket, 2*time.Second)
	for _, name := range []string{"../x", "-x", "a b", "a", "/web", strings.Repeat("a", spec.MaxNameLength+1)} {
		_, err := p.get(context.Background(), spec.ContainerInput{Name: name})
		if slugOf(err) != problem.ValidationFailed {
			t.Errorf("get %q: %v, want validation-failed", name, err)
		}
		err = p.logs(context.Background(), spec.LogsInput{Name: name}, func(apitypes.Frame) error { return nil })
		if slugOf(err) != problem.ValidationFailed {
			t.Errorf("logs %q: %v, want validation-failed", name, err)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("invalid names reached the daemon: %d calls %v", n, f.paths)
	}
	if _, err := p.get(context.Background(), spec.ContainerInput{Name: "nope"}); slugOf(err) != problem.NotFound {
		t.Fatalf("unknown container: %v", err)
	}
	err := p.logs(context.Background(), spec.LogsInput{Name: "nope"}, func(apitypes.Frame) error { return nil })
	if slugOf(err) != problem.NotFound {
		t.Fatalf("logs of an unknown container: %v", err)
	}
}

func TestListPagesInspectsAndCachesImages(t *testing.T) {
	f := newFakeEngine(t)
	digest := "sha256:" + strings.Repeat("1", 64)
	for i := range 5 {
		name := fmt.Sprintf("c%d", i)
		f.containers[name] = fakeContainer{id: strings.Repeat(fmt.Sprint(i), 64), image: "sha256:img", ref: "nginx:1"}
	}
	f.images["sha256:img"] = []string{"nginx@" + digest}
	p := newProvider(t, f.socket, 2*time.Second)

	var names []string
	cursor := ""
	for range 4 {
		page, err := p.list(context.Background(), spec.ListInput{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Items {
			names = append(names, c.Name)
			if c.ID != "/providers/docker/containers/"+c.Name || c.Type != "containers" || c.Host != "testhost" {
				t.Fatalf("FR-1 fields: %+v", c)
			}
			if c.Properties.ImageDigest != digest || c.Properties.Health != spec.HealthNone ||
				c.Properties.RestartPolicy.Name != "always" {
				t.Fatalf("properties: %+v", c.Properties)
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(names, []string{"c0", "c1", "c2", "c3", "c4"}) {
		t.Fatalf("paged names %v", names)
	}
	f.mu.Lock()
	imageCalls := 0
	for _, path := range f.paths {
		if strings.HasPrefix(path, "/images/") {
			imageCalls++
		}
	}
	f.mu.Unlock()
	// One image inspection per page of a list (three pages), not one per
	// container.
	if imageCalls != 3 {
		t.Fatalf("%d image inspections for 3 pages sharing one image", imageCalls)
	}
	if _, err := p.list(context.Background(), spec.ListInput{Cursor: "!!"}); slugOf(err) != problem.ValidationFailed {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestPorts(t *testing.T) {
	f := newFakeEngine(t)
	f.containers["web"] = fakeContainer{id: strings.Repeat("a", 64), image: "sha256:gone", ref: "nginx:1"}
	p := newProvider(t, f.socket, 2*time.Second)
	c, err := p.get(context.Background(), spec.ContainerInput{Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	want := []spec.Port{
		{ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0", HostPort: 8080},
		{ContainerPort: 80, Protocol: "tcp", HostIP: "::", HostPort: 8080},
		{ContainerPort: 443, Protocol: "tcp"},
	}
	if !slices.Equal(c.Properties.Ports, want) {
		t.Fatalf("ports %+v, want %+v", c.Properties.Ports, want)
	}
	// The image was removed: no digest, not an error.
	if c.Properties.ImageDigest != "" {
		t.Fatalf("digest of a removed image %q", c.Properties.ImageDigest)
	}
}

func TestRepoDigestMatchesTheReferenceRepository(t *testing.T) {
	d1, d2 := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	for _, tc := range []struct {
		ref     string
		digests []string
		want    string
	}{
		{"nginx", []string{"nginx@" + d1}, d1},
		{"nginx:1.27", []string{"docker.io/library/nginx@" + d1}, d1},
		{"ghcr.io/o/app:v1", []string{"nginx@" + d1, "ghcr.io/o/app@" + d2}, d2},
		{"nginx@" + d1, []string{"nginx@" + d1}, d1},
		{"mybox:latest", []string{"busybox@" + d1}, ""},                    // Retagged locally
		{"local/built", nil, ""},                                           // Built locally
		{"sha256:" + strings.Repeat("3", 64), []string{"nginx@" + d1}, ""}, // Image ID
	} {
		if got := repoDigest(tc.ref, tc.digests); got != tc.want {
			t.Errorf("repoDigest(%q, %v) = %q, want %q", tc.ref, tc.digests, got, tc.want)
		}
	}
}

func TestPrimaryName(t *testing.T) {
	if got := primaryName([]string{"/other/alias", "/web"}); got != "web" {
		t.Fatalf("primaryName = %q", got)
	}
}

func collect(t *testing.T, p *Provider, in spec.LogsInput) []apitypes.Frame {
	t.Helper()
	var frames []apitypes.Frame
	if err := p.logs(context.Background(), in, func(f apitypes.Frame) error {
		frames = append(frames, f)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return frames
}

// Non-TTY logs are demuxed into out and err by the stream header; TTY logs
// are raw and all out, so a byte that looks like a header is kept. Each
// frame carries Docker's timestamp and no byte offset.
func TestLogsDemuxAndTimestamps(t *testing.T) {
	f := newFakeEngine(t)
	f.containers["plain"] = fakeContainer{id: strings.Repeat("a", 64), image: "sha256:i", ref: "x", logs: []fakeLine{
		{1, "2026-11-07T10:00:00.000000001Z", "one\n"},
		{2, "2026-11-07T10:00:01.5Z", "two\n"},
		{1, "2026-11-07T10:00:02Z", "bad \xff\n"},
	}}
	f.containers["tty"] = fakeContainer{id: strings.Repeat("b", 64), image: "sha256:i", ref: "x", tty: true, logs: []fakeLine{
		{0, "2026-11-07T10:00:00Z", "\x01\x00\x00\x00 raw\r\n"},
		{0, "2026-11-07T10:00:03Z", "end"},
	}}
	p := newProvider(t, f.socket, 2*time.Second)

	got := collect(t, p, spec.LogsInput{Name: "plain"})
	want := []apitypes.Frame{
		{T: time.Date(2026, 11, 7, 10, 0, 0, 1, time.UTC), S: "out", D: "one\n"},
		{T: time.Date(2026, 11, 7, 10, 0, 1, 5e8, time.UTC), S: "err", D: "two\n"},
		{T: time.Date(2026, 11, 7, 10, 0, 2, 0, time.UTC), S: "out", D: "bad �\n"},
	}
	if !slices.EqualFunc(got, want, frameEqual) {
		t.Fatalf("non-TTY frames\n got  %+v\n want %+v", got, want)
	}
	got = collect(t, p, spec.LogsInput{Name: "tty"})
	want = []apitypes.Frame{
		{T: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC), S: "out", D: "\x01\x00\x00\x00 raw\r\n"},
		{T: time.Date(2026, 11, 7, 10, 0, 3, 0, time.UTC), S: "out", D: "end"},
	}
	if !slices.EqualFunc(got, want, frameEqual) {
		t.Fatalf("TTY frames\n got  %+v\n want %+v", got, want)
	}
	for _, fr := range got {
		if fr.O != nil {
			t.Fatal("a container log frame carries a byte offset")
		}
	}

	// An emit failure (the client left) stops the stream and is returned.
	stop := errors.New("client gone")
	err := p.logs(context.Background(), spec.LogsInput{Name: "plain"}, func(apitypes.Frame) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("emit error: %v", err)
	}
}

func frameEqual(a, b apitypes.Frame) bool {
	return a.T.Equal(b.T) && a.S == b.S && a.D == b.D && a.O == nil && a.Type == ""
}

// tail and since reach the daemon as Docker expects them.
func TestLogsQuery(t *testing.T) {
	f := newFakeEngine(t)
	f.containers["web"] = fakeContainer{id: strings.Repeat("a", 64), image: "sha256:i", ref: "x"}
	p := newProvider(t, f.socket, 2*time.Second)
	var queries []string
	f.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			queries = append(queries, r.URL.RawQuery)
		}
		f.serve(w, r)
	})
	tail := 5
	collect(t, p, spec.LogsInput{Name: "web", Tail: &tail, Since: "2026-11-07T10:00:00.25+01:00"})
	if len(queries) != 1 {
		t.Fatalf("queries %v", queries)
	}
	for _, want := range []string{"tail=5", "since=1794042000.250000000", "stdout=1", "stderr=1", "timestamps=1"} {
		if !strings.Contains(queries[0], want) {
			t.Errorf("logs query %q lacks %s", queries[0], want)
		}
	}
	if strings.Contains(queries[0], "follow") {
		t.Errorf("follow sent without being asked: %s", queries[0])
	}
}
