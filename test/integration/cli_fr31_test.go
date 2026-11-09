//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// asCLIEnv makes the integration test binary the nexus CLI (TestMain).
const asCLIEnv = "NEXUS_INTEGRATION_AS_CLI"

// fr31 is FR-31's example, verbatim but for the binary's path.
const fr31 = `"$NX" container get --filter status=updateAvailable --host all | "$NX" container update --stdin`

// authProxy is a registry in front of nx-registry that requires its own
// basic-auth account, unless open. Two of them give two nexusd sharing the
// harness Engine different views: each has credentials for one only, so
// each finds only its own container updateAvailable, and the other's
// unknown.
type authProxy struct {
	host       string // 127.0.0.1:port, the registry part of image references
	user, pass string
	open       atomic.Bool
}

func startAuthProxy(t *testing.T, user, pass string) *authProxy {
	t.Helper()
	target, _ := url.Parse("http://" + regOpen)
	rp := httputil.NewSingleHostReverseProxy(target)
	p := &authProxy{user: user, pass: pass}
	want := "Basic " + b64(user+":"+pass)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.open.Load() && r.Header.Get("Authorization") != want {
			w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
			w.Header().Set("WWW-Authenticate", `Basic realm="nx-tc170"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		rp.ServeHTTP(w, r)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.host = ln.Addr().String()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

// dockerNexusd is a real `nexusd serve` with only the docker provider,
// run as a child process so two run beside each other. Its state is under
// /var/lib/nexus, where the docker-update helper's transient unit reads it.
func dockerNexusd(t *testing.T, name, configDir string) *nexusdInstance {
	t.Helper()
	nextPort++
	n := &nexusdInstance{t: t, listen: fmt.Sprintf("127.0.0.1:%d", nextPort), client: &http.Client{Timeout: 60 * time.Second}}
	stateDir := filepath.Join(nexusdState, "nexusd-"+name)
	auditDir := filepath.Join("/var/log/nexus", "nexusd-"+name)
	for _, d := range []string{stateDir, auditDir} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli", Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	cfg := filepath.Join(t.TempDir(), "nexusd.yaml")
	body := fmt.Sprintf("host: {name: %s}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n  scripts: {enabled: false}\n"+
		"  docker: {enabled: true, socket: %s, configDir: %s}\n", name, n.listen, dockerSocket, configDir)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nexusdBin, "serve", "--config", cfg, "--state-dir", stateDir, "--audit-dir", auditDir)
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
		t.Logf("%s output:\n%s", name, out.String())
	})
	waitUntil(t, 15*time.Second, name+" healthy", func() (bool, string) {
		resp, err := http.Get("http://" + n.listen + "/healthz")
		if err != nil {
			return false, err.Error()
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
	return n
}

// checkUpdates runs check-updates on n to its end (A-13).
func checkUpdates(t *testing.T, n *nexusdInstance) {
	t.Helper()
	code, b := n.post(checkAt, "{}")
	if code != http.StatusAccepted {
		t.Fatalf("check-updates: %d %s", code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	if op = n.waitOp(op.ID, 3*time.Minute); op.Status != apitypes.StatusSucceeded {
		t.Fatalf("check-updates ended %s: %+v", op.Status, op.Error)
	}
}

// updates counts the container update operations n has recorded.
func updates(t *testing.T, n *nexusdInstance) int {
	t.Helper()
	var page apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations?limit=1000", &page)
	c := 0
	for _, op := range page.Items {
		if op.Action == spec.Name+".containers.update" {
			c++
		}
	}
	return c
}

// fr31Host is one side of TC-170: a nexusd, its container and its proxy.
type fr31Host struct {
	n          *nexusdInstance
	p          *authProxy
	name, repo string
	newD       string // The registry's digest after staging
}

// stage gives h a fresh out-of-date container, as staged() does for the
// update tests, through h's proxy opened for the staging.
func (h *fr31Host) stage(t *testing.T) {
	t.Helper()
	removeUpdated(t, h.name)
	h.p.open.Store(true)
	defer h.p.open.Store(false)
	_, _, h.newD = staged(t, h.p.host, h.repo, h.name, nil, updPlain, updPlain)
}

// digest is the repository digest the container h.name runs now, by
// docker inspect.
func (h *fr31Host) digest(t *testing.T) string {
	t.Helper()
	return repoDigest(t, dockerInspectField(t, h.name, "{{.Image}}"), h.p.host+"/"+h.repo)
}

// pipelineEnv is the environment of the CLI processes: this binary as
// nexus, with home's configuration.
func pipelineEnv(home string) []string {
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "NEXUS_") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") || strings.HasPrefix(e, "HOME=")
	})
	return append(env, asCLIEnv+"=1", "HOME="+home, "NX="+os.Args[0])
}

// expectFR31 answers every [y/N] prompt on the spawned pipeline's own
// pseudo-terminal, its controlling terminal, and exits with its status.
const expectFR31 = `set timeout 600
set prompts 0
spawn -noecho sh -c $env(NX_CMD)
expect {
	-re {\[y/N\] $} { incr prompts; send -- "$env(NX_ANSWER)\r"; exp_continue }
	timeout { puts "\nNX_TIMEOUT"; exit 99 }
	eof
}
set r [wait]
puts "\nNX_PROMPTS=$prompts"
exit [lindex $r 3]
`

// TC-170: FR-31's example works verbatim against two real nexusd with
// Docker: under expect it asks once on the controlling terminal and, on y,
// updates each host's container to the registry's digest; with no
// controlling terminal it exits 2 and sends no mutation; with --yes it runs
// with no prompt.
func TestFR31ExampleVerbatim(t *testing.T) {
	requireDocker(t)
	startRegistries(t)
	if _, err := exec.LookPath("expect"); err != nil {
		t.Fatal("expect is not installed in the harness image")
	}
	pa, pb := startAuthProxy(t, "nxa", "pass-a-170"), startAuthProxy(t, "nxb", "pass-b-170")
	creds := func(p *authProxy) string {
		return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, p.host, b64(p.user+":"+p.pass))
	}
	hosts := []*fr31Host{
		{n: dockerNexusd(t, "tc170a", configDir(t, "tc170a", creds(pa))), p: pa, name: "tc170-a", repo: "nx/tc170a"},
		{n: dockerNexusd(t, "tc170b", configDir(t, "tc170b", creds(pb))), p: pb, name: "tc170-b", repo: "nx/tc170b"},
	}
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("hosts:\n  alpha: {url: \"http://%s\", token: %s}\n  beta: {url: \"http://%s\", token: %s}\n",
		hosts[0].n.listen, hosts[0].n.token, hosts[1].n.listen, hosts[1].n.token)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"alpha": hosts[0].name, "beta": hosts[1].name}

	// stageAll returns both containers to an old digest and checks (A-13),
	// and requires each host to list exactly its own container.
	stageAll := func() (old map[string]string) {
		old = map[string]string{}
		for _, h := range hosts {
			h.stage(t)
			old[h.name] = h.digest(t)
		}
		for _, h := range hosts {
			checkUpdates(t, h.n)
		}
		for i, h := range hosts {
			var page apitypes.List[spec.Container]
			h.n.mustGet(containersAt+"?filter=status%3DupdateAvailable", &page)
			var names []string
			for _, c := range page.Items {
				names = append(names, c.Name)
			}
			if !slices.Equal(names, []string{h.name}) {
				t.Fatalf("host %d lists %v updateAvailable, want only %s", i, names, h.name)
			}
		}
		return old
	}
	wantUpdated := func(what string) {
		t.Helper()
		for _, h := range hosts {
			got := h.digest(t)
			t.Logf("%s: %s runs %s; registry %s", what, h.name, got, h.newD)
			if got != h.newD {
				t.Errorf("%s: %s runs %s, want the registry's %s", what, h.name, got, h.newD)
			}
		}
	}
	checkOutput := func(what, stdout string) {
		t.Helper()
		var lines []map[string]any
		for l := range strings.Lines(stdout) {
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("%s: stdout line %q: %v", what, l, err)
			}
			lines = append(lines, m)
		}
		seen := map[string]bool{}
		for _, m := range lines {
			h, _ := m["host"].(string)
			seen[h] = true
			if m["status"] != "Succeeded" || !strings.HasSuffix(fmt.Sprint(m["target"]), labels[h]) {
				t.Errorf("%s: output %v, want %s's update, Succeeded", what, m, labels[h])
			}
		}
		if len(lines) != 2 || !seen["alpha"] || !seen["beta"] {
			t.Errorf("%s: %d output lines for hosts %v, want one per input line, alpha and beta", what, len(lines), seen)
		}
	}

	// Step 1: under expect, answer y.
	stageAll()
	dir := t.TempDir()
	script := filepath.Join(dir, "fr31.exp")
	if err := os.WriteFile(script, []byte(expectFR31), 0o600); err != nil {
		t.Fatal(err)
	}
	outF, errF := filepath.Join(dir, "out"), filepath.Join(dir, "err")
	cmd := exec.Command("expect", script)
	cmd.Env = append(pipelineEnv(home), "NX_ANSWER=y", "NX_CMD="+fr31+" >"+outF+" 2>"+errF)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	tty, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	stdout, _ := os.ReadFile(outF)
	stderr, _ := os.ReadFile(errF)
	t.Logf("step 1 (terminal, y): exit %d\nterminal: %q\nstdout:\n%sstderr: %q", code, tty, stdout, stderr)
	m := regexp.MustCompile(`NX_PROMPTS=(\d+)`).FindSubmatch(tty)
	prompts := -1
	if m != nil {
		prompts, _ = strconv.Atoi(string(m[1]))
	}
	if code != 0 || prompts != 1 || !strings.Contains(string(tty), "Update 2 containers on 2 hosts (alpha, beta)? [y/N] ") {
		t.Errorf("step 1: exit %d, %d prompts, want 0 and exactly one", code, prompts)
	}
	checkOutput("step 1", string(stdout))
	wantUpdated("step 1")

	// Step 3: the same with no controlling terminal: exit 2, nothing sent.
	old := stageAll()
	before := []int{updates(t, hosts[0].n), updates(t, hosts[1].n)}
	run := func(line string) (int, string, string) {
		var out, errb bytes.Buffer
		c := exec.Command("sh", "-c", line)
		c.Env = pipelineEnv(home)
		c.Stdout, c.Stderr = &out, &errb
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err := c.Run()
		if _, ok := err.(*exec.ExitError); err != nil && !ok {
			t.Fatal(err)
		}
		return c.ProcessState.ExitCode(), out.String(), errb.String()
	}
	code, out, errs := run(fr31)
	t.Logf("step 3 (no terminal): exit %d\nstdout: %q\nstderr: %s", code, out, errs)
	if code != cli.ExitUsage || out != "" {
		t.Errorf("step 3: exit %d, stdout %q; want 2 and nothing", code, out)
	}
	for i, h := range hosts {
		n := updates(t, h.n)
		got := h.digest(t)
		t.Logf("step 3: %s: update operations %d -> %d; runs %s (before %s)", h.name, before[i], n, got, old[h.name])
		if n != before[i] || got != old[h.name] {
			t.Errorf("step 3: %s received a mutation or changed digest", h.name)
		}
	}

	// Step 4: --yes with no controlling terminal: no prompt, both updated.
	code, out, errs = run(fr31 + " --yes")
	t.Logf("step 4 (no terminal, --yes): exit %d\nstdout:\n%sstderr: %q", code, out, errs)
	if code != 0 || strings.Contains(errs, "[y/N]") {
		t.Errorf("step 4: exit %d, stderr %q", code, errs)
	}
	checkOutput("step 4", out)
	wantUpdated("step 4")
}
