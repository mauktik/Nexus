package nginx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	nexusserver "github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

var (
	ubuntu = platform.Info{Init: platform.Systemd, OS: platform.OS{ID: "ubuntu", VersionID: "24.04"}}
	alpine = platform.Info{Init: platform.OpenRC, OS: platform.OS{ID: "alpine", VersionID: "3.21.3"}}
	// debianOpenRC is TC-3's Debian booted with OpenRC.
	debianOpenRC = platform.Info{Init: platform.OpenRC, OS: platform.OS{ID: "debian", VersionID: "12"}}
)

func detachAvailable() detach.Availability { return detach.Availability{Available: true} }

// writeFakeNginx installs at path a stand-in for nginx that answers -v the
// way nginx does (`nginx version: nginx/<version>` on stderr, exit 0) and
// refuses every other flag with exit 2, recording it in <path>.denied. Every
// invocation's arguments are appended to <path>.calls. With hang set, -v
// writes its PID to <path>.pid and blocks. The script is written beside the
// target and renamed over it, as a package upgrade replaces a binary.
func writeFakeNginx(t *testing.T, path, version string, hang bool, padding string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("echo \"$*\" >> \"$0.calls\"\n")
	b.WriteString("if [ \"$1\" = -v ] && [ $# -eq 1 ]; then\n")
	if hang {
		b.WriteString("  echo $$ > \"$0.pid\"\n  sleep 600\n")
	}
	fmt.Fprintf(&b, "  echo 'nginx version: nginx/%s' >&2\n  exit 0\nfi\n", version)
	b.WriteString("echo \"$*\" >> \"$0.denied\"\nexit 2\n")
	if padding != "" {
		b.WriteString("# " + padding + "\n")
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// fakeCalls returns the argument lists the fake nginx at path recorded.
func fakeCalls(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path + ".calls")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// fixture is a provider over a temp includeDir, the fake nginx, and the
// svcctl fake.
type fixture struct {
	p       *Provider
	dir     string // includeDir
	nginx   string // The fake nginx's path
	svc     *svcctltest.Fake
	section config.NginxConfig // What Configure decoded
}

type fixtureOptions struct {
	plat      platform.Info // Zero: ubuntu
	model     svcctltest.Model
	preflight func(context.Context) provider.Status
	noNginx   bool // Leave the nginx path empty
	hangV     bool
}

func newFixture(t *testing.T, o fixtureOptions) *fixture {
	t.Helper()
	if o.plat.Init == "" {
		o.plat = ubuntu
	}
	root := t.TempDir()
	f := &fixture{dir: filepath.Join(root, "conf.d"), nginx: filepath.Join(root, "bin", "nginx")}
	if err := os.Mkdir(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(f.nginx), 0o755); err != nil {
		t.Fatal(err)
	}
	if !o.noNginx {
		writeFakeNginx(t, f.nginx, "1.26.3", o.hangV, "")
	}
	f.svc = svcctltest.New(t, o.plat.Init, o.model)
	ctl, err := svcctl.New(o.plat.Init, svcctl.Options{Run: f.svc.Run, RunRoot: filepath.Join(root, "run"),
		ProcRoot: filepath.Join(root, "proc")})
	if err != nil {
		t.Fatal(err)
	}
	plat := o.plat
	f.p = New(Options{Host: "h1", Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Platform: &plat,
		Services: ctl, Detach: detachAvailable, Nginx: f.nginx, Preflight: o.preflight})
	if err := f.p.Configure(f.decode); err != nil {
		t.Fatal(err)
	}
	return f
}

// decode is the provider's config section: includeDir set to the temp
// directory, everything else the defaults.
func (f *fixture) decode(into any) error {
	c, ok := into.(*config.NginxConfig)
	if !ok {
		return fmt.Errorf("decode into %T, want *config.NginxConfig", into)
	}
	c.Enabled, c.IncludeDir = true, f.dir
	f.section = *c
	return nil
}

func enabledStatus(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

// stub is a provider with no operations that is always enabled: the
// scripts-like neighbour whose state must not follow nginx's (NFR-4).
type stub struct{}

func (stub) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "neighbour", ReadScope: "neighbour:read"}
}
func (stub) Configure(func(any) error) error               { return nil }
func (stub) Preflight(ctx context.Context) provider.Status { return enabledStatus(ctx) }
func (stub) Bindings() []provider.Binding                  { return nil }

// server is a real nexus server, core.New and server.New, over the
// providers given.
type server struct {
	t     *testing.T
	url   string
	token string
	reg   *core.Registry
	http  *http.Client
}

func startServer(t *testing.T, providers []provider.Provider, cfgs map[string]core.ProviderConfig) *server {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: "h1", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := core.New(providers, core.Options{Host: "h1", Version: "v0.0.0-test", Logger: log, Audit: alog,
		Providers: cfgs, Platform: ubuntu})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "reader",
		Scopes: []string{spec.ReadScope, "neighbour:read"}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nexusserver.New(reg, nexusserver.Options{Logger: log,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 100000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	s := &server{t: t, url: "http://" + ln.Addr().String(), token: tok, reg: reg,
		http: &http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}}
	t.Cleanup(func() {
		s.http.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	return s
}

// startNginx serves f's provider and the always-enabled neighbour.
func startNginx(t *testing.T, f *fixture) *server {
	t.Helper()
	return startServer(t, []provider.Provider{f.p, stub{}}, map[string]core.ProviderConfig{
		spec.Name: {Enabled: true, Decode: f.decode}, "neighbour": {Enabled: true}})
}

// get answers GET path (under /v1) with the status and body.
func (s *server) get(path string) (int, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url+"/v1"+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// getJSON GETs path, requires want, and decodes the body into out.
func (s *server) getJSON(path string, want int, out any) {
	s.t.Helper()
	code, body := s.get(path)
	if code != want {
		s.t.Fatalf("GET %s = %d %s, want %d", path, code, body, want)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			s.t.Fatalf("GET %s: %v in %s", path, err, body)
		}
	}
}

// problemBody is the part of a problem response the tests read.
type problemBody struct {
	Type     string `json:"type"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Reason   string `json:"reason"`
	FileName string `json:"fileName"`
	Provider string `json:"provider"`
}

// problemOf GETs path and requires a problem of slug with HTTP status code.
func (s *server) problemOf(path string, code int, slug string) problemBody {
	s.t.Helper()
	var p problemBody
	s.getJSON(path, code, &p)
	if p.Type != "urn:nexus:problem:"+slug {
		s.t.Fatalf("GET %s: problem %+v, want %s", path, p, slug)
	}
	return p
}

func (s *server) capability(name string) apitypes.ProviderCapability {
	s.t.Helper()
	var caps apitypes.Capabilities
	s.getJSON("/capabilities", http.StatusOK, &caps)
	for _, c := range caps.Providers {
		if c.Name == name {
			return c
		}
	}
	s.t.Fatalf("capabilities have no provider %q: %+v", name, caps.Providers)
	return apitypes.ProviderCapability{}
}

// nginx-provider TC-61: Preflight reports nginx available with details when every condition holds
// Owner WI 4.5. Requirements: FR-2a, FR-1.
func TestPreflightAvailable(t *testing.T) {
	ownedByTester(t) // includeDir is a temp directory the test owns
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running}})
	st := f.p.Preflight(context.Background())
	if st.State != apitypes.ProviderEnabled {
		t.Fatalf("Preflight = %+v, want enabled", st)
	}
	want := map[string]any{"initSystem": "systemd", "service": "nginx", "includeDir": f.dir,
		"includeSuffix": ".conf", "disabledSuffix": ".disabled", "version": "1.26.3"}
	if len(st.Details) != len(want) {
		t.Errorf("Details = %v, want exactly %v", st.Details, want)
	}
	for k, v := range want {
		if st.Details[k] != v {
			t.Errorf("Details[%s] = %v, want %v", k, st.Details[k], v)
		}
	}
	if calls := fakeCalls(t, f.nginx); len(calls) != 1 || calls[0] != "-v" {
		t.Errorf("nginx calls = %q, want one -v", calls)
	}
}

// nginx-provider TC-62: A SIGHUP reload re-runs preflight and picks up nginx installed later
// Owner WI 4.5. Requirements: FR-2a.
//
// The daemon's SIGHUP handler (internal/daemon/reload.go) loads the file
// and calls Registry.Apply with the sections; this package cannot import
// internal/daemon (it composes this provider), so the test drives the same
// two steps: config.Load, then Apply.
func TestPreflightRecheckedOnReload(t *testing.T) {
	ownedByTester(t) // includeDir is a temp directory the test owns
	f := newFixture(t, fixtureOptions{noNginx: true, model: svcctltest.Model{Absent: true}})
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	yaml := fmt.Sprintf("listen: [\"127.0.0.1:1\"]\ntls: {mode: off}\nproviders:\n  nginx:\n    enabled: true\n    includeDir: %s\n", f.dir)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	sections := func() map[string]core.ProviderConfig {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		n := cfg.Providers.Nginx
		return map[string]core.ProviderConfig{spec.Name: {Enabled: n.Settings.Enabled, Decode: n.Decode},
			"neighbour": {Enabled: true}}
	}
	s := startServer(t, []provider.Provider{f.p, stub{}}, sections())

	c := s.capability(spec.Name)
	if c.Status != apitypes.ProviderUnavailable || !strings.Contains(c.Reason, f.nginx) {
		t.Fatalf("before install: nginx = %s %q, want unavailable naming %s", c.Status, c.Reason, f.nginx)
	}
	if n := s.capability("neighbour"); n.Status != apitypes.ProviderEnabled {
		t.Errorf("neighbour = %s, want enabled", n.Status)
	}
	s.problemOf("/providers/nginx/service", http.StatusServiceUnavailable, "capability-unavailable")

	writeFakeNginx(t, f.nginx, "1.26.3", false, "")
	f.svc.Set(func(m *svcctltest.Model) { m.Absent, m.State = false, svcctl.Running })
	if err := s.reg.Apply(context.Background(), sections()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if c := s.capability(spec.Name); c.Status != apitypes.ProviderEnabled {
		t.Fatalf("after reload: nginx = %s %q, want enabled", c.Status, c.Reason)
	}
	if n := s.capability("neighbour"); n.Status != apitypes.ProviderEnabled {
		t.Errorf("neighbour = %s, want enabled", n.Status)
	}
	var l apitypes.List[spec.Service]
	s.getJSON("/providers/nginx/service", http.StatusOK, &l)
	if len(l.Items) != 1 || l.Items[0].Properties.State != "running" {
		t.Errorf("status = %+v, want one running item", l.Items)
	}
}

// nginx-provider TC-72: The effective includeDir follows the distribution (worked example)
// Owner WI 4.5. Requirements: FR-2, ASM-3, ASM-16, L-2.
func TestEffectiveIncludeDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		plat     platform.Info
		explicit string
		want     string
	}{
		{"alpine", alpine, "", "/etc/nginx/http.d"},
		{"ubuntu", ubuntu, "", "/etc/nginx/conf.d"},
		{"debian-openrc", debianOpenRC, "", "/etc/nginx/conf.d"},
		{"alpine-explicit", alpine, "/srv/nginx", "/srv/nginx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := svcctltest.Model{State: svcctl.Running}
			fx := newFixture(t, fixtureOptions{plat: tc.plat, model: model})
			// The section as config.Load leaves it: includeDir as written.
			section := config.Default().Providers.Nginx
			section.Settings.IncludeDir = tc.explicit
			if err := fx.p.Configure(func(into any) error {
				*into.(*config.NginxConfig) = section.Settings
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s := fx.p.Settings()
			if s.IncludeDir != tc.want {
				t.Errorf("effective includeDir = %q, want %q", s.IncludeDir, tc.want)
			}
			if section.Settings.IncludeDir != tc.explicit {
				t.Errorf("the config section's includeDir became %q; it is not written back", section.Settings.IncludeDir)
			}
			if tc.name == "alpine" {
				if got := filepath.Join(s.IncludeDir, s.EnabledFile("example.com")); got != "/etc/nginx/http.d/example.com.conf" {
					t.Errorf("enabled path = %s", got)
				}
				if got := filepath.Join(s.IncludeDir, s.DisabledFile("example.com")); got != "/etc/nginx/http.d/example.com.conf.disabled" {
					t.Errorf("disabled path = %s", got)
				}
			}
			l, err := fx.p.status(context.Background(), spec.StatusInput{})
			if err != nil {
				t.Fatal(err)
			}
			if got := l.Items[0].Properties.IncludeDir; got != tc.want {
				t.Errorf("status includeDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// nginx-provider TC-ERR9: Preflight names exactly what is missing
// Owner WI 4.5. Requirements: FR-2a, NFR-4.
func TestPreflightFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model svcctltest.Model
		setup func(t *testing.T, f *fixture)
		// want lists fragments of the reason; "%n" is the nginx path and
		// "%d" the includeDir.
		want []string
	}{
		{"binary missing", svcctltest.Model{}, func(t *testing.T, f *fixture) { rm(t, f.nginx) },
			[]string{"%n", "missing"}},
		{"binary not executable", svcctltest.Model{}, func(t *testing.T, f *fixture) { chmod(t, f.nginx, 0o644) },
			[]string{"%n", "not executable"}},
		{"binary is a directory", svcctltest.Model{}, func(t *testing.T, f *fixture) {
			rm(t, f.nginx)
			if err := os.Mkdir(f.nginx, 0o755); err != nil {
				t.Fatal(err)
			}
		}, []string{"%n", "not executable"}},
		{"detach unavailable", svcctltest.Model{}, func(t *testing.T, f *fixture) {
			f.p.detach = func() detach.Availability {
				return detach.Availability{Reason: "R: no supported init system"}
			}
		}, []string{"R: no supported init system"}},
		{"service unknown", svcctltest.Model{Absent: true}, nil, []string{"nginx", "not known to systemd"}},
		{"includeDir missing", svcctltest.Model{}, func(t *testing.T, f *fixture) { rm(t, f.dir) },
			[]string{"%d", "does not exist"}},
		{"includeDir is a file", svcctltest.Model{}, func(t *testing.T, f *fixture) {
			rm(t, f.dir)
			if err := os.WriteFile(f.dir, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"%d", "not a directory"}},
		{"includeDir is a symlink", svcctltest.Model{}, func(t *testing.T, f *fixture) {
			real := f.dir + ".real"
			if err := os.Rename(f.dir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, f.dir); err != nil {
				t.Fatal(err)
			}
		}, []string{"%d", "symlink"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{model: tc.model})
			if tc.setup != nil {
				tc.setup(t, f)
			}
			st := f.p.Preflight(context.Background())
			if st.State != apitypes.ProviderUnavailable {
				t.Fatalf("Preflight = %+v, want unavailable", st)
			}
			for _, w := range tc.want {
				w = strings.NewReplacer("%n", f.nginx, "%d", f.dir).Replace(w)
				if !strings.Contains(st.Reason, w) {
					t.Errorf("reason %q does not contain %q", st.Reason, w)
				}
			}

			s := startNginx(t, f)
			p := s.problemOf("/providers/nginx/service", http.StatusServiceUnavailable, "capability-unavailable")
			if p.Reason != st.Reason {
				t.Errorf("status reason = %q, want the preflight's %q", p.Reason, st.Reason)
			}
			if n := s.capability("neighbour"); n.Status != apitypes.ProviderEnabled {
				t.Errorf("neighbour = %s %q, want enabled", n.Status, n.Reason)
			}
		})
	}
}

func rm(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
