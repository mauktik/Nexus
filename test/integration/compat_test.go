//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/apt"
	aptspec "github.com/mauktik/nexus/internal/providers/apt/spec"
	"github.com/mauktik/nexus/internal/providers/docker"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts"
	"github.com/mauktik/nexus/internal/version"
)

// TC-192 (NFR-5). Both directions of an additive change within /v1, against
// real daemons:
//
//   - The older nexusd is this test binary run as a daemon (olderNexusdEnv):
//     the production daemon wiring, daemon.New with the real docker, apt
//     and scripts providers, built without apt.upgradable.list and with an
//     older build version. The current CLI, which has that op, is the newer
//     CLI.
//   - The newer server is the real nexusd behind a proxy that adds fields
//     to every response object, advertises a provider the CLI does not
//     know, and serves that provider's extra op. The current CLI is the
//     older CLI.

const (
	olderNexusdEnv = "NEXUS_INTEGRATION_AS_OLDER_NEXUSD"
	olderVersion   = "v0.9.0-compat-older"
	newerOnlyOp    = "apt.upgradable.list"
	newerField     = "addedInNewerServer"
)

func init() {
	if os.Getenv(olderNexusdEnv) == "1" {
		os.Exit(olderNexusd(os.Args[1:], os.Stdout, os.Stderr))
	}
}

// olderNexusd serves like `nexusd serve` with newerOnlyOp left out.
func olderNexusd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("older-nexusd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "configuration file")
	stateDir := fs.String("state-dir", "", "state directory")
	auditDir := fs.String("audit-dir", "", "audit log directory")
	if err := fs.Parse(args); err != nil || *path == "" || *stateDir == "" || *auditDir == "" {
		fmt.Fprintln(stderr, "usage: older-nexusd --config file --state-dir dir --audit-dir dir")
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Read by daemon.New into capabilities and the ready line.
	version.Version = olderVersion
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	providers := []provider.Provider{
		docker.New(docker.Options{Host: cfg.Host.Name, Logger: log}),
		withoutOp{Provider: apt.New(apt.Options{Host: cfg.Host.Name, Logger: log}), drop: newerOnlyOp},
		scripts.New(scripts.Options{Host: cfg.Host.Name}),
	}
	d, err := daemon.New(cfg, daemon.Options{Providers: providers, ConfigPath: *path, StateDir: *stateDir,
		AuditDir: *auditDir, Logger: log})
	if err != nil {
		fmt.Fprintf(stderr, "older-nexusd: %v\n", err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "older-nexusd: %v\n", err)
		return 1
	}
	return 0
}

// withoutOp is a provider as an older build had it: one op neither
// declared nor bound.
type withoutOp struct {
	provider.Provider
	drop string
}

func (w withoutOp) Spec() def.ProviderSpec {
	s := w.Provider.Spec()
	s.Ops = slices.DeleteFunc(slices.Clone(s.Ops), func(o def.Op) bool { return o.ID == w.drop })
	return s
}

func (w withoutOp) Bindings() []provider.Binding {
	return slices.DeleteFunc(w.Provider.Bindings(), func(b provider.Binding) bool { return b.Op().ID == w.drop })
}

func (w withoutOp) UseKV(kv provider.KV) {
	if u, ok := w.Provider.(provider.KVUser); ok {
		u.UseKV(kv)
	}
}

func (w withoutOp) UseOpsDir(dir string) {
	if u, ok := w.Provider.(provider.OpsDirUser); ok {
		u.UseOpsDir(dir)
	}
}

// compatDaemon is a real daemon run as a child process: bin with env.
type compatDaemon struct {
	listen string
	token  string
}

func startCompatDaemon(t *testing.T, name, bin string, env []string) *compatDaemon {
	t.Helper()
	nextPort++
	d := &compatDaemon{listen: fmt.Sprintf("127.0.0.1:%d", nextPort)}
	stateDir, auditDir, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli",
		Scopes: []string{dockerspec.ReadScope, dockerspec.WriteScope, aptspec.ReadScope}})
	if err != nil {
		t.Fatal(err)
	}
	d.token = tok
	cfg := filepath.Join(tmp, "nexusd.yaml")
	body := fmt.Sprintf("host: {name: %s}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n%s  apt: {enabled: true}\n  scripts: {enabled: false}\n",
		name, d.listen, dockerSection(dockerSocket))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--config", cfg, "--state-dir", stateDir, "--audit-dir", auditDir)
	if bin == os.Args[0] {
		cmd.Args = []string{bin, "--config", cfg, "--state-dir", stateDir, "--audit-dir", auditDir}
	}
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Logf("%s output:\n%s", name, out.String())
		}
	})
	waitUntil(t, 30*time.Second, name+" healthy", func() (bool, string) {
		resp, err := http.Get("http://" + d.listen + "/healthz")
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
	return d
}

// newerProxy is the newer server: target's responses with a new field in
// every response object, list item, item properties and provider entry; a
// provider "future" in capabilities; and the future provider's op.
func newerProxy(t *testing.T, target string) string {
	t.Helper()
	u, err := url.Parse("http://" + target)
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{
		Rewrite:        func(r *httputil.ProxyRequest) { r.SetURL(u) },
		ModifyResponse: addNewerFields,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/future/widgets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"/providers/future/widgets/w1","type":"widgets","name":"w1","host":"newer","properties":{}}],"nextCursor":""}`)
	})
	mux.Handle("/", rp)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func addNewerFields(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	isJSON := strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/problem+json")
	isNDJSON := strings.HasPrefix(ct, "application/x-ndjson")
	if !isJSON && !isNDJSON {
		return nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}
	var out bytes.Buffer
	enrich := func(raw []byte) error {
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return err
		}
		if obj, ok := v.(map[string]any); ok {
			obj[newerField] = "v1-additive"
			if items, ok := obj["items"].([]any); ok {
				for _, it := range items {
					if m, ok := it.(map[string]any); ok {
						m[newerField] = "v1-additive"
						if p, ok := m["properties"].(map[string]any); ok {
							p[newerField] = "v1-additive"
						}
					}
				}
			}
			if provs, ok := obj["providers"].([]any); ok && resp.Request.URL.Path == "/v1/capabilities" {
				for _, it := range provs {
					if m, ok := it.(map[string]any); ok {
						m[newerField] = "v1-additive"
					}
				}
				obj["providers"] = append(provs, map[string]any{"name": "future", "status": "enabled",
					"resourceTypes": []any{"widgets"}, "actions": []any{"frobnicate"}, newerField: "v1-additive"})
			}
		}
		enc, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out.Write(enc)
		out.WriteByte('\n')
		return nil
	}
	if isJSON {
		if err := enrich(b); err != nil {
			return err
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			if err := enrich(sc.Bytes()); err != nil {
				return err
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
	}
	resp.Body = io.NopCloser(&out)
	resp.ContentLength = int64(out.Len())
	resp.Header.Set("Content-Length", strconv.Itoa(out.Len()))
	return nil
}

// compatCLI runs the real CLI code with a config naming both hosts.
func compatCLI(t *testing.T, home string, args ...string) cliResult {
	t.Helper()
	r := runNexus(home, args...)
	t.Logf("nexus %s -> exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code,
		strings.TrimSpace(r.stdout), strings.TrimSpace(r.stderr))
	return r
}

func TestNFRAdditiveCompatibility(t *testing.T) {
	nfrLoad(t)
	const a, b = "compat-a", "compat-b"
	requireDocker(t, a, b)
	for _, name := range []string{a, b} {
		dockerCLI(t, "run", "-d", "--name", name, testImage, "sleep", "3600")
	}
	older := startCompatDaemon(t, "older", os.Args[0], []string{olderNexusdEnv + "=1"})
	current := startCompatDaemon(t, "current", nexusdBin, nil)
	newer := newerProxy(t, current.listen)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("defaultHost: older\nhosts:\n"+
		"  older: {url: \"http://%s\", token: %s}\n"+
		"  current: {url: \"http://%s\", token: %s}\n"+
		"  newer: {url: \"http://%s\", token: %s}\n",
		older.listen, older.token, current.listen, current.token, newer, current.token)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	capsOf := func(r cliResult) map[string]any {
		t.Helper()
		objs := jsonObjects(t, r.stdout)
		if len(objs) != 1 {
			t.Fatalf("capability get printed %d objects", len(objs))
		}
		return objs[0]
	}
	names := func(r cliResult) []string {
		t.Helper()
		var out []string
		for _, o := range jsonObjects(t, r.stdout) {
			if n, ok := o["name"].(string); ok {
				out = append(out, n)
			}
		}
		return out
	}
	mustExit := func(r cliResult, code int, what string) {
		t.Helper()
		if r.code != code {
			t.Fatalf("%s: exit %d, want %d\nstdout: %s\nstderr: %s", what, r.code, code, r.stdout, r.stderr)
		}
	}

	// The newer CLI against the older nexusd: existing commands work.
	r := compatCLI(t, home, "capability", "get", "--host", "older", "-o", "jsonl")
	mustExit(r, 0, "capability get on older")
	c := capsOf(r)
	if c["version"] != olderVersion || c["apiVersion"] != apitypes.APIVersion {
		t.Fatalf("older capabilities: version %v apiVersion %v, want %s and %s", c["version"], c["apiVersion"], olderVersion, apitypes.APIVersion)
	}
	t.Logf("NFR TC-192 older nexusd capabilities: apiVersion %v, version %v", c["apiVersion"], c["version"])
	r = compatCLI(t, home, "container", "get", "--host", "older", "-o", "jsonl")
	mustExit(r, 0, "container get on older")
	if got := names(r); !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("container get on older: %v lacks %s and %s", got, a, b)
	}
	r = compatCLI(t, home, "package", "get", "--host", "older", "-o", "jsonl", "--limit", "5")
	if detectedInit() == platform.OpenRC {
		// Alpine has no apt: the older nexusd answers as the newer one does.
		mustExit(r, 4, "package get on older (no apt on this host)")
		if !strings.Contains(r.stderr, "capability-unavailable") {
			t.Fatalf("package get on older: stderr %q, want capability-unavailable", r.stderr)
		}
	} else {
		mustExit(r, 0, "package get on older")
	}

	// The new op: a clear capability error, exit 4.
	r = compatCLI(t, home, "packageupdate", "get", "--host", "older")
	mustExit(r, 4, "packageupdate get on older")
	for _, want := range []string{"older lacks this capability", "route-not-found"} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("packageupdate get on older: stderr %q lacks %q", r.stderr, want)
		}
	}
	t.Logf("NFR TC-192 newer CLI, new op on the older nexusd: exit %d, %s", r.code, strings.TrimSpace(r.stderr))
	r = compatCLI(t, home, "packageupdate", "get", "--host", "older", "-o", "jsonl")
	mustExit(r, 4, "packageupdate get -o jsonl on older")
	if errs := jsonObjects(t, r.stderr); len(errs) != 1 || !strings.Contains(fmt.Sprint(errs[0]), "route-not-found") {
		t.Fatalf("packageupdate get -o jsonl on older: stderr %q, want one error object naming route-not-found", r.stderr)
	}
	// Control: the same command against the current nexusd, which serves
	// the op, succeeds.
	r = compatCLI(t, home, "packageupdate", "get", "--host", "current", "-o", "jsonl")
	if detectedInit() == platform.OpenRC {
		// Served, but apt is unavailable on Alpine: not route-not-found.
		mustExit(r, 4, "packageupdate get on current (no apt on this host)")
		if !strings.Contains(r.stderr, "capability-unavailable") || strings.Contains(r.stderr, "route-not-found") {
			t.Fatalf("packageupdate get on current: stderr %q, want capability-unavailable", r.stderr)
		}
	} else {
		mustExit(r, 0, "packageupdate get on current")
	}

	// The older CLI against the newer server: existing commands work and
	// the unknown fields and provider are tolerated.
	r = compatCLI(t, home, "capability", "get", "--host", "newer", "-o", "jsonl")
	mustExit(r, 0, "capability get on newer")
	c = capsOf(r)
	t.Logf("NFR TC-192 newer server capabilities: apiVersion %v, version %v", c["apiVersion"], c["version"])
	if c["apiVersion"] != apitypes.APIVersion || c["version"] == "" || c["version"] == nil {
		t.Fatalf("newer capabilities: %v", c)
	}
	r = compatCLI(t, home, "container", "get", "--host", "newer", "-o", "jsonl")
	mustExit(r, 0, "container get on newer")
	if got := names(r); !slices.Contains(got, a) || !slices.Contains(got, b) {
		t.Fatalf("container get on newer: %v lacks %s and %s", got, a, b)
	}
	t.Logf("NFR TC-192 older CLI json output passes the unknown field through: %v", strings.Contains(r.stdout, newerField))
	r = compatCLI(t, home, "container", "get", a, "--host", "newer", "-o", "jsonl")
	mustExit(r, 0, "container get NAME on newer")
	if got := names(r); !slices.Equal(got, []string{a}) {
		t.Fatalf("container get %s on newer: %v", a, got)
	}
	r = compatCLI(t, home, "container", "get", "--host", "newer", "-o", "table")
	mustExit(r, 0, "container get -o table on newer")
	if !strings.Contains(r.stdout, a) || strings.Contains(r.stdout, newerField) {
		t.Fatalf("table on newer: lacks %s or shows the unknown field:\n%s", a, r.stdout)
	}
	r = compatCLI(t, home, "package", "get", "--host", "newer", "-o", "jsonl", "--limit", "5")
	mustExit(r, 0, "package get on newer")
	// A long-running op: the 202 and every poll of the operation carry the
	// new field.
	r = compatCLI(t, home, "container", "stop", b, "--host", "newer", "-o", "jsonl")
	mustExit(r, 0, "container stop on newer")
	if st := dockerInspect(t, b).State.Status; st != "exited" {
		t.Fatalf("after stop through the newer server, %s is %s", b, st)
	}
	r = compatCLI(t, home, "operation", "get", "--host", "newer", "-o", "jsonl", "--limit", "5")
	mustExit(r, 0, "operation get on newer")
}
