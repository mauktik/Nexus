//go:build integration

package integration

// nginx-provider WI 7.1: the nginx provider against the distribution's real
// nginx, under real systemd (Ubuntu, `make test-harness`) and real OpenRC
// (Alpine, `make test-harness-alpine`): TC-106 to TC-117 of
// planning/nginx-provider/test-cases.md.
//
// Every expected value is read back without Nexus: the init system's own
// CLI (`systemctl is-active nginx`, `rc-service nginx status`), /proc (the
// master's and workers' PIDs, exe, cgroup and Umask), `ls -ln` and
// `sha256sum` of the include directory, and `curl` on each test site's own
// port. Nothing here mocks nginx or the init system. The one "fake nginx" is
// test/harness/fixtures/nginx/nginx-wrapper.sh, which only delays or hangs
// `nginx -t` and execs the real binary otherwise (TC-110, TC-115); it is
// installed over /usr/sbin/nginx by rename and removed again in t.Cleanup,
// where the real binary's SHA-256 is checked.
//
// Ubuntu's nexusd configures includeDir: /etc/nginx/conf.d; Alpine's uses the
// default, /etc/nginx/http.d. Each test site has its own port in 18080 to
// 18199 and its own marker, nexus-<test>-<random>, and every site a test
// creates is removed in t.Cleanup with rm, since the provider cannot delete
// (OOS-4).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	ngxspec "github.com/mauktik/nexus/internal/providers/nginx/spec"
	scriptsspec "github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

const (
	ngxFixtures = "/harness/fixtures/nginx"
	ngxBin      = "/usr/sbin/nginx"
	// ngxRealCopy is the real binary while nginx-wrapper.sh is installed.
	ngxRealCopy = "/usr/sbin/nginx.t1-real"
	// ngxWrapCtl makes the wrapper delay (seconds) or hang ("hang") -t.
	ngxWrapCtl = "/run/nexus-t1/nginx-t"
	ngxAt      = "/v1/providers/nginx"
	ngxSites   = ngxAt + "/sites"
	// ngxConflictPort is the port port-conflict.conf listens on.
	ngxConflictPort = 18090
	// ngxErrorLog is nginx's error log on both distributions (Alpine's
	// compiled-in /var/lib/nginx/logs is a link to /var/log/nginx).
	ngxErrorLog = "/var/log/nginx/error.log"
)

// ngxScopes are the nginx scopes, read, write and config.
var ngxScopes = []string{ngxspec.ReadScope, ngxspec.WriteScope, ngxspec.ConfigScope}

// ngxIncludeDir is the include directory nginx's packaging reads sites from:
// Alpine's /etc/nginx/http.d, Debian's and Ubuntu's /etc/nginx/conf.d.
func ngxIncludeDir() string {
	if platform.Current().OS.IsAlpine() {
		return "/etc/nginx/http.d"
	}
	return "/etc/nginx/conf.d"
}

// ngxSection is the nginx provider's configuration: Ubuntu names its
// includeDir, Alpine leaves the default. extra is more flow-map members,
// each starting with ", ".
func ngxSection(extra string) string {
	if platform.Current().OS.IsAlpine() {
		return fmt.Sprintf("  nginx: {enabled: true%s}\n", extra)
	}
	return fmt.Sprintf("  nginx: {enabled: true, includeDir: %s%s}\n", ngxIncludeDir(), extra)
}

// --- nginx as the init system and /proc see it -------------------------

// ngxInitState is nginx's state from the init system's own CLI: running,
// stopped or failed, or the raw word for anything else.
func ngxInitState(t *testing.T) string {
	t.Helper()
	if detectedInit() == platform.OpenRC {
		out, _ := sh(30*time.Second, "rc-service", "nginx", "status")
		for word, st := range map[string]string{"started": "running", "stopped": "stopped", "crashed": "failed"} {
			if strings.Contains(out, "status: "+word) {
				return st
			}
		}
		return strings.TrimSpace(out)
	}
	out, _ := sh(30*time.Second, detach.Systemctl, "is-active", "nginx")
	switch s := strings.TrimSpace(out); s {
	case "active":
		return "running"
	case "inactive":
		return "stopped"
	default:
		return s
	}
}

// ngxInit runs verb (start, stop, restart, reload) on nginx through the init
// system directly, for a test's setup and cleanup.
func ngxInit(t *testing.T, verb string) {
	t.Helper()
	if detectedInit() == platform.OpenRC {
		mustSh(t, "rc-service", "nginx", verb)
		return
	}
	mustSh(t, detach.Systemctl, verb, "nginx")
}

// ngxProc is one nginx process from /proc.
type ngxProc struct {
	pid, ppid int
	title     string
}

// ngxProcs lists every process whose command line is an nginx title.
func ngxProcs() []ngxProc {
	ents, _ := os.ReadDir("/proc")
	var out []ngxProc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		title := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
		if !strings.HasPrefix(title, "nginx: ") || !pidAlive(pid) {
			continue
		}
		ppid, _ := strconv.Atoi(procStatusField(pid, "PPid"))
		out = append(out, ngxProc{pid: pid, ppid: ppid, title: title})
	}
	return out
}

// ngxGen is the running generation read from /proc: the master and its
// direct children titled exactly "nginx: worker process".
type ngxGen struct {
	master  int
	workers []int
}

func (g ngxGen) String() string { return fmt.Sprintf("master %d workers %v", g.master, g.workers) }

// ngxObserve reads the generation; master is 0 when no master runs. A
// process the master has just forked keeps the master's title until it
// sets its own, so the master is the one master-titled process whose parent
// is not nginx. More than one such master fails the test.
func ngxObserve(t *testing.T) ngxGen {
	t.Helper()
	procs := ngxProcs()
	isNginx := map[int]bool{}
	for _, p := range procs {
		isNginx[p.pid] = true
	}
	var g ngxGen
	for _, p := range procs {
		if strings.HasPrefix(p.title, "nginx: master process") && !isNginx[p.ppid] {
			if g.master != 0 {
				t.Fatalf("two nginx masters: %d and %d (%v)", g.master, p.pid, procs)
			}
			g.master = p.pid
		}
	}
	for _, p := range procs {
		if g.master != 0 && p.ppid == g.master && p.title == "nginx: worker process" {
			g.workers = append(g.workers, p.pid)
		}
	}
	slices.Sort(g.workers)
	return g
}

// ngxWaitRunning waits until the init system says running and /proc shows
// a master with workers.
func ngxWaitRunning(t *testing.T) ngxGen {
	t.Helper()
	var g ngxGen
	waitUntil(t, 30*time.Second, "nginx running with workers", func() (bool, string) {
		g = ngxObserve(t)
		st := ngxInitState(t)
		return st == "running" && g.master != 0 && len(g.workers) > 0, st + " " + g.String()
	})
	return g
}

// ngxEnsureRunning starts nginx through the init system if it is not
// running, and returns its generation.
func ngxEnsureRunning(t *testing.T) ngxGen {
	t.Helper()
	switch ngxInitState(t) {
	case "running":
	case "failed":
		if detectedInit() == platform.OpenRC {
			mustSh(t, "rc-service", "nginx", "zap")
		} else {
			mustSh(t, detach.Systemctl, "reset-failed", "nginx")
		}
		ngxInit(t, "start")
	default:
		ngxInit(t, "start")
	}
	return ngxWaitRunning(t)
}

// ngxWorkersReplaced waits until none of old's workers is a worker of the
// same master and at least one new worker runs: a reload that took.
func ngxWorkersReplaced(t *testing.T, old ngxGen) ngxGen {
	t.Helper()
	var g ngxGen
	waitUntil(t, 30*time.Second, "new workers under master "+strconv.Itoa(old.master), func() (bool, string) {
		g = ngxObserve(t)
		overlap := slices.ContainsFunc(g.workers, func(p int) bool { return slices.Contains(old.workers, p) })
		return g.master == old.master && len(g.workers) > 0 && !overlap, g.String()
	})
	return g
}

// ngxPidof is `pidof nginx`, logged next to the /proc reading.
func ngxPidof() string {
	out, _ := sh(10*time.Second, "pidof", "nginx")
	return strings.TrimSpace(out)
}

// --- sites ---------------------------------------------------------------

// ngxMarker is a test site's marker: nexus-<test>-<random>.
func ngxMarker(t *testing.T, test string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "nexus-" + test + "-" + hex.EncodeToString(b)
}

// ngxRender reads fixture and replaces each @KEY@ with its value.
func ngxRender(t *testing.T, fixture string, vars map[string]string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ngxFixtures, fixture))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "@"+k+"@", v)
	}
	if regexp.MustCompile(`@[A-Z]+@`).MatchString(s) {
		t.Fatalf("%s: unrendered placeholder in %q", fixture, s)
	}
	return s
}

// ngxSiteConf is site.conf.tmpl serving marker on port.
func ngxSiteConf(t *testing.T, port int, marker string) string {
	t.Helper()
	return ngxRender(t, "site.conf.tmpl", map[string]string{"PORT": strconv.Itoa(port), "MARKER": marker})
}

// ngxFixture is a fixture's text as is.
func ngxFixture(t *testing.T, name string) string {
	t.Helper()
	return ngxRender(t, name, nil)
}

// ngxForget removes every name of site name from includeDir at cleanup and,
// when nginx runs, reloads it through the init system so it stops serving
// them.
func ngxForget(t *testing.T, names ...string) {
	t.Helper()
	dir := ngxIncludeDir()
	rm := func() {
		for _, n := range names {
			for _, f := range []string{n + ".conf", n + ".conf.disabled"} {
				if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("remove %s: %v", f, err)
				}
			}
		}
	}
	rm()
	t.Cleanup(func() {
		rm()
		if ngxInitState(t) == "running" {
			if out, err := sh(time.Minute, "sh", "-c", ngxInitCmd("reload")); err != nil {
				t.Errorf("reload nginx after removing %v: %v %s", names, err, out)
			}
		}
	})
}

// ngxInitCmd is the init system's command line for verb on nginx.
func ngxInitCmd(verb string) string {
	if detectedInit() == platform.OpenRC {
		return "rc-service nginx " + verb
	}
	return "systemctl " + verb + " nginx"
}

// ngxPlace writes file in includeDir as root would place it by hand:
// content, mode 0644, owned by uid:0.
func ngxPlace(t *testing.T, file, content string, uid int) {
	t.Helper()
	p := filepath.Join(ngxIncludeDir(), file)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(p, uid, 0); err != nil {
		t.Fatal(err)
	}
}

// ngxCurl is curl's body from 127.0.0.1:port, and its error: exit 7 is a
// refused connection.
func ngxCurl(port int) (string, error) {
	out, err := sh(15*time.Second, "curl", "-sS", "--max-time", "5", fmt.Sprintf("http://127.0.0.1:%d/", port))
	return strings.TrimSpace(out), err
}

// ngxServes waits until curl on port returns marker.
func ngxServes(t *testing.T, port int, marker string) {
	t.Helper()
	waitUntil(t, 20*time.Second, fmt.Sprintf("curl :%d to return %s", port, marker), func() (bool, string) {
		out, err := ngxCurl(port)
		return err == nil && out == marker, fmt.Sprint(out, err)
	})
	t.Logf("curl :%d -> %s", port, marker)
}

// ngxRefused requires curl on port to fail to connect (curl exit 7).
func ngxRefused(t *testing.T, port int) {
	t.Helper()
	var ee *exec.ExitError
	waitUntil(t, 20*time.Second, fmt.Sprintf("curl :%d to be refused", port), func() (bool, string) {
		out, err := ngxCurl(port)
		return errors.As(err, &ee) && ee.ExitCode() == 7, fmt.Sprint(out, err)
	})
	out, err := ngxCurl(port)
	t.Logf("curl :%d -> %v: %s", port, err, out)
}

// ngxSha is the lower-case hex SHA-256 of s.
func ngxSha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ngxSha256sum is `sha256sum path`'s digest.
func ngxSha256sum(t *testing.T, path string) string {
	t.Helper()
	f := strings.Fields(mustSh(t, "sha256sum", path))
	if len(f) == 0 {
		t.Fatalf("sha256sum %s: no output", path)
	}
	return f[0]
}

// ngxLs is `ls -ln` of includeDir, for the log.
func ngxLs(t *testing.T) string {
	t.Helper()
	return mustSh(t, "ls", "-ln", ngxIncludeDir())
}

// ngxSnapshot is every entry of includeDir: its name, type, mode, owner,
// group and SHA-256, from stat(1) and sha256sum(1).
func ngxSnapshot(t *testing.T) string {
	t.Helper()
	dir := ngxIncludeDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		st := strings.TrimSpace(mustSh(t, "stat", "-c", "%n %F %a %u %g", p))
		sum := "-"
		if e.Type().IsRegular() {
			sum = ngxSha256sum(t, p)
		}
		lines = append(lines, st+" "+sum)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ngxSameSnapshot fails the test when includeDir differs from before.
func ngxSameSnapshot(t *testing.T, what, before string) {
	t.Helper()
	after := ngxSnapshot(t)
	if after != before {
		t.Fatalf("%s: includeDir changed:\nbefore:\n%s\nafter:\n%s\nls -ln:\n%s", what, before, after, ngxLs(t))
	}
	t.Logf("%s: includeDir hashes, names, modes and owners unchanged (%d entries)", what, strings.Count(after, "\n")+1)
}

// --- the nginx-wrapper.sh fake ------------------------------------------

// ngxWrap installs nginx-wrapper.sh over /usr/sbin/nginx by rename, with the
// real binary copied to ngxRealCopy, and restores the real binary at
// cleanup, checking its SHA-256. A running master then shows the kernel's
// "(deleted)" exe, which the provider accepts, as after a package upgrade.
// At cleanup a running nginx is restarted, so later tests see a normal exe.
func ngxWrap(t *testing.T) {
	t.Helper()
	if exists(ngxRealCopy) {
		t.Fatalf("%s exists: an earlier wrapper was not removed", ngxRealCopy)
	}
	orig := ngxSha256sum(t, ngxBin)
	mustSh(t, "cp", "-p", ngxBin, ngxRealCopy)
	mustSh(t, "install", "-o", "root", "-g", "root", "-m", "0755", filepath.Join(ngxFixtures, "nginx-wrapper.sh"), ngxBin+".t1-wrap")
	mustSh(t, "mv", "-f", ngxBin+".t1-wrap", ngxBin)
	if err := os.MkdirAll(filepath.Dir(ngxWrapCtl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("nginx-wrapper.sh installed at %s; the real binary (sha256 %s) is %s", ngxBin, orig, ngxRealCopy)
	t.Cleanup(func() {
		_ = os.Remove(ngxWrapCtl)
		// A -t the wrapper hung is killed with its helper's process group;
		// any left over is killed here so nothing holds the old binary.
		_, _ = sh(10*time.Second, "pkill", "-f", "^sleep 2147483647$")
		if out, err := sh(time.Minute, "mv", "-f", ngxRealCopy, ngxBin); err != nil {
			t.Errorf("restore %s: %v %s", ngxBin, err, out)
			return
		}
		if got := ngxSha256sum(t, ngxBin); got != orig {
			t.Errorf("restored %s has sha256 %s, want the real binary's %s", ngxBin, got, orig)
		} else {
			t.Logf("restored the real %s: sha256sum %s", ngxBin, got)
		}
		if ngxInitState(t) == "running" {
			if out, err := sh(time.Minute, "sh", "-c", ngxInitCmd("restart")); err != nil {
				t.Errorf("restart nginx after the wrapper: %v %s", err, out)
			}
		}
	})
}

// ngxWrapMode sets what the wrapper does with -t: a delay in seconds,
// "hang", or "" for the real -t at once.
func ngxWrapMode(t *testing.T, mode string) {
	t.Helper()
	if mode == "" {
		if err := os.Remove(ngxWrapCtl); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(ngxWrapCtl, []byte(mode+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- nexusd with the nginx provider -------------------------------------

// ngxDaemon is a real nexusd (newNexusd: nexusd.service under the shipped
// hardening, ExecStart replaced by a drop-in) with the nginx provider, a
// token holding the nginx scopes, and a CLI home whose default host it is.
type ngxDaemon struct {
	*nexusdInstance
	name     string
	stateDir string
	home     string
}

// newNgxDaemon starts nexusd with the nginx section (ngxSection(extra)) and
// others, more provider sections.
func newNgxDaemon(t *testing.T, name, extra, others string) *ngxDaemon {
	t.Helper()
	n := newNexusd(t, name, ngxSection(extra)+others)
	d := &ngxDaemon{nexusdInstance: n, name: name, stateDir: filepath.Join(nexusdState, "nexusd-"+name)}
	tok, _, err := auth.Create(d.stateDir, auth.CreateRequest{Name: "nginx-t1",
		Scopes: append(slices.Clone(ngxScopes), dockerspec.ReadScope, scriptsspec.ReadScope)})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	d.home = t.TempDir()
	cfgDir := filepath.Join(d.home, ".config", "nexus")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("defaultHost: t1\nhosts:\n  t1: {url: \"http://%s\", token: %s}\n", n.listen, tok)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	for _, pc := range caps.Providers {
		if pc.Name == ngxspec.Name && pc.Status != apitypes.ProviderEnabled {
			t.Fatalf("nginx provider %s: %s", pc.Status, pc.Reason)
		}
	}
	return d
}

// cliOp runs the nexus CLI (cli.Run in process) with -o json against this
// daemon, requires exit 0 and returns the one operation it printed (-o
// json prints a list).
func (d *ngxDaemon) cliOp(t *testing.T, args ...string) apitypes.Operation {
	t.Helper()
	args = append(args, "-o", "json")
	r := runNexus(d.home, args...)
	t.Logf("nexus %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, strings.TrimSpace(r.stdout),
		strings.TrimSpace(r.stderr))
	if r.code != 0 {
		t.Fatalf("nexus %s exited %d: %s", strings.Join(args, " "), r.code, r.stderr)
	}
	var ops []apitypes.Operation
	if err := json.Unmarshal([]byte(r.stdout), &ops); err != nil || len(ops) != 1 || ops[0].ID == "" {
		t.Fatalf("nexus %s printed %q, not one operation (%v)", strings.Join(args, " "), r.stdout, err)
	}
	return ops[0]
}

// begin POSTs body to path and requires 202 with an operation.
func (d *ngxDaemon) begin(t *testing.T, path, body string) apitypes.Operation {
	t.Helper()
	code, b := d.post(path, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST %s: %d %s", path, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	return op
}

// run POSTs body to path and returns the operation once it ended.
func (d *ngxDaemon) run(t *testing.T, path, body string) apitypes.Operation {
	t.Helper()
	op := d.waitOp(d.begin(t, path, body).ID, 3*time.Minute)
	t.Logf("POST %s: %s in %s, result %s, error %s", path, op.Status, ngxTook(op), op.Result, ngxProblem(op))
	return op
}

func ngxTook(op apitypes.Operation) time.Duration {
	if op.Started == nil || op.Ended == nil {
		return 0
	}
	return op.Ended.Sub(*op.Started).Round(time.Millisecond)
}

func ngxProblem(op apitypes.Operation) string {
	if op.Error == nil {
		return "none"
	}
	b, _ := json.Marshal(op.Error)
	return string(b)
}

// ngxResult requires op Succeeded and decodes its result.
func ngxResult[T any](t *testing.T, what string, op apitypes.Operation) T {
	t.Helper()
	var r T
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("%s: %s %s", what, op.Status, ngxProblem(op))
	}
	if err := json.Unmarshal(op.Result, &r); err != nil {
		t.Fatalf("%s: result %s: %v", what, op.Result, err)
	}
	return r
}

// ngxFailed requires op Failed with slug and returns its problem.
func ngxFailed(t *testing.T, what string, op apitypes.Operation, slug problem.Slug) *problem.Error {
	t.Helper()
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != slug {
		t.Fatalf("%s: %s %s, want Failed %s", what, op.Status, ngxProblem(op), slug)
	}
	return op.Error
}

// ngxObservation decodes a ReloadObservation extension.
func ngxObservation(t *testing.T, p *problem.Error, key string) ngxspec.ReloadObservation {
	t.Helper()
	var o ngxspec.ReloadObservation
	v, ok := p.Extensions[key]
	if !ok {
		t.Fatalf("problem has no %s: %+v", key, p.Extensions)
	}
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, &o)
	}
	if err != nil {
		t.Fatalf("%s %v: %v", key, v, err)
	}
	return o
}

// opLog is operation id's whole log, frame data concatenated.
func (d *ngxDaemon) opLog(t *testing.T, id string) string {
	t.Helper()
	code, b := d.get("/v1/operations/" + id + "/logs")
	if code != http.StatusOK {
		t.Fatalf("logs %s: %d %s", id, code, b)
	}
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		out.WriteString(f.D)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// healthy reports whether /healthz answers 200.
func (d *ngxDaemon) healthy() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + d.listen + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// restartNexusd restarts nexusd through the init system (`systemctl
// restart nexusd`, `rc-service nexusd restart`) and waits for /healthz.
func (d *ngxDaemon) restartNexusd(t *testing.T) {
	t.Helper()
	before := d.mainPID()
	if detectedInit() == platform.OpenRC {
		mustSh(t, "rc-service", "nexusd", "restart")
	} else {
		mustSh(t, detach.Systemctl, "restart", nexusdUnit)
	}
	waitUntil(t, time.Minute, "nexusd healthy after the restart", func() (bool, string) {
		return d.healthy(), "not answering"
	})
	t.Logf("nexusd restarted through %s: main pid %d -> %d", detectedInit(), before, d.mainPID())
}

// ngxHelperPIDs are the live `nexusd nginx-op run --dir <dir>` processes.
func ngxHelperPIDs(dir string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		argv := procArgv(pid)
		if len(argv) >= 5 && argv[1] == "nginx-op" && argv[2] == "run" && argv[4] == dir && pidAlive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// ngxSiteOp posts action on site name.
func ngxSiteOp(name, action string) string { return ngxSites + "/" + name + "/" + action }

// ngxWriteBody is an add or update body.
func ngxWriteBody(t *testing.T, content string, more map[string]any) string {
	t.Helper()
	m := map[string]any{"content": content}
	for k, v := range more {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ngxAdd adds site name with content through the API and requires success.
func (d *ngxDaemon) add(t *testing.T, name, content string) ngxspec.WriteResult {
	t.Helper()
	return ngxResult[ngxspec.WriteResult](t, "add "+name, d.run(t, ngxSiteOp(name, "add"), ngxWriteBody(t, content, nil)))
}

// --- TC-106 ---------------------------------------------------------------

// nginx-provider TC-106: Lifecycle commanded state equals the init system's
// observed state. Each op runs through the CLI; the readback is the init
// system's CLI, pidof and /proc.
func TestNginxLifecycleObserved(t *testing.T) {
	d := newNgxDaemon(t, "tc106", "", "")
	ngxEnsureRunning(t)
	t.Cleanup(func() { ngxEnsureRunning(t) })

	check := func(verb, want string) (ngxspec.ServiceResult, ngxGen) {
		t.Helper()
		args := []string{"nginx", verb}
		if verb == "stop" {
			args = append(args, "--yes")
		}
		res := ngxResult[ngxspec.ServiceResult](t, "nginx "+verb, d.cliOp(t, args...))
		var g ngxGen
		if want == "running" {
			g = ngxWaitRunning(t)
		} else {
			waitUntil(t, 30*time.Second, "nginx stopped", func() (bool, string) {
				g = ngxObserve(t)
				return ngxInitState(t) == "stopped" && g.master == 0, ngxInitState(t) + " " + g.String()
			})
		}
		st := ngxInitState(t)
		mainPID := 0
		if res.MainPID != nil {
			mainPID = *res.MainPID
		}
		t.Logf("%s: commanded %s; Nexus state %s mainPid %d; %s says %s; /proc %s; pidof nginx: %q", verb, want,
			res.State, mainPID, detectedInit(), st, g, ngxPidof())
		if st != want || res.State != want || mainPID != g.master {
			t.Fatalf("%s: init system %s, Nexus state %s mainPid %d, /proc master %d; want %s", verb, st, res.State,
				mainPID, g.master, want)
		}
		return res, g
	}

	check("stop", "stopped")
	_, g1 := check("start", "running")
	_, g2 := check("restart", "running")
	if g2.master == g1.master {
		t.Fatalf("restart: master %d unchanged", g2.master)
	}
	t.Logf("restart: master %d -> %d", g1.master, g2.master)
	res, _ := check("reload", "running")
	g3 := ngxWorkersReplaced(t, g2)
	if res.Reload == nil || res.Reload.Verdict != ngxspec.VerdictConfirmed || res.Reload.MasterPID != g2.master {
		t.Fatalf("reload: observation %+v, want confirmed for master %d", res.Reload, g2.master)
	}
	t.Logf("reload: master %d unchanged, workers %v -> %v (Nexus saw %v -> %v)", g3.master, g2.workers, g3.workers,
		res.Reload.OldWorkers, res.Reload.NewWorkers)

	// Step 3: a package upgrade without a restart replaces the binary by a
	// rename; the running master's exe then carries the kernel's suffix.
	t.Cleanup(func() {
		if out, err := sh(time.Minute, "sh", "-c", ngxInitCmd("restart")); err != nil {
			t.Errorf("restart nginx after the upgrade model: %v %s", err, out)
		}
	})
	mustSh(t, "sh", "-c", "cp -p "+ngxBin+" "+ngxBin+".tmp && mv "+ngxBin+".tmp "+ngxBin)
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", g3.master))
	t.Logf("TC-ERR15 row 4 source: readlink /proc/%d/exe = %q (%v)", g3.master, exe, err)
	if exe != ngxBin+" (deleted)" {
		t.Fatalf("readlink /proc/%d/exe = %q, want %q", g3.master, exe, ngxBin+" (deleted)")
	}
	res, _ = check("reload", "running")
	g4 := ngxWorkersReplaced(t, g3)
	if res.Reload == nil || res.Reload.Verdict != ngxspec.VerdictConfirmed || g4.master != g3.master {
		t.Fatalf("reload after the upgrade: observation %+v, master %d -> %d", res.Reload, g3.master, g4.master)
	}
	t.Logf("reload with a (deleted) exe: confirmed, master %d unchanged, workers %v -> %v", g4.master, g3.workers, g4.workers)
}

// --- TC-107 ---------------------------------------------------------------

// nginx-provider TC-107: Enable and disable are observed on disk and over
// HTTP. Then FR-10's disable rollback on the real nginx: disabling a site
// whose upstream another site uses fails the live nginx -t, and the file
// keeps its name and the proxy keeps serving.
func TestNginxToggleObserved(t *testing.T) {
	const port = 18081
	d := newNgxDaemon(t, "tc107", "", "")
	ngxEnsureRunning(t)
	dir := ngxIncludeDir()
	ngxForget(t, "t1", "t1def", "t1use")
	marker := ngxMarker(t, "tc107")
	ngxPlace(t, "t1.conf.disabled", ngxSiteConf(t, port, marker), 0)
	ngxRefused(t, port)

	// Step 1.
	res := ngxResult[ngxspec.SiteToggleResult](t, "enable t1", d.cliOp(t, "nginxsite", "start", "t1"))
	t.Logf("ls -ln %s:\n%s", dir, ngxLs(t))
	if !exists(filepath.Join(dir, "t1.conf")) || exists(filepath.Join(dir, "t1.conf.disabled")) {
		t.Fatal("after enable: want t1.conf and no t1.conf.disabled")
	}
	ngxServes(t, port, marker)
	if !res.Enabled || !res.Reloaded || res.FileName != "t1.conf" {
		t.Fatalf("enable result %+v", res)
	}

	// Step 2.
	res = ngxResult[ngxspec.SiteToggleResult](t, "disable t1", d.cliOp(t, "nginxsite", "stop", "t1", "--yes"))
	t.Logf("ls -ln %s:\n%s", dir, ngxLs(t))
	if exists(filepath.Join(dir, "t1.conf")) || !exists(filepath.Join(dir, "t1.conf.disabled")) {
		t.Fatal("after disable: want t1.conf.disabled and no t1.conf")
	}
	ngxRefused(t, port)
	if res.Enabled || !res.Reloaded || res.FileName != "t1.conf.disabled" {
		t.Fatalf("disable result %+v", res)
	}

	// FR-10: the upstream-dependency pair.
	const defPort, usePort = 18086, 18087
	up := "nexus_t1_up"
	defMarker := ngxMarker(t, "tc107def")
	ngxPlace(t, "t1def.conf", ngxRender(t, "upstream-def.conf",
		map[string]string{"UPSTREAM": up, "PORT": strconv.Itoa(defPort), "MARKER": defMarker}), 0)
	ngxPlace(t, "t1use.conf", ngxRender(t, "upstream-use.conf",
		map[string]string{"UPSTREAM": up, "PORT": strconv.Itoa(usePort)}), 0)
	ngxInit(t, "reload")
	ngxServes(t, usePort, defMarker)
	before := ngxSnapshot(t)
	op := d.run(t, ngxSiteOp("t1def", "disable"), "{}")
	p := ngxFailed(t, "disable t1def", op, problem.NginxConfigInvalid)
	out, _ := p.Extensions["testOutput"].(string)
	if p.Extensions["rolledBack"] != true || !strings.Contains(out, `host not found in upstream "`+up+`"`) {
		t.Fatalf("disable t1def: rolledBack %v, testOutput %q", p.Extensions["rolledBack"], out)
	}
	ngxSameSnapshot(t, "disable t1def rolled back", before)
	ngxServes(t, usePort, defMarker)
}

// --- TC-108 ---------------------------------------------------------------

// nginx-provider TC-108: A write is observed by hash and over HTTP.
func TestNginxWriteObserved(t *testing.T) {
	const port = 18082
	d := newNgxDaemon(t, "tc108", "", "")
	ngxEnsureRunning(t)
	ngxForget(t, "t2")
	file := filepath.Join(ngxIncludeDir(), "t2.conf")
	m1, m2 := ngxMarker(t, "tc108m1"), ngxMarker(t, "tc108m2")
	c1, c2 := ngxSiteConf(t, port, m1), ngxSiteConf(t, port, m2)

	// Step 1.
	add := ngxResult[ngxspec.WriteResult](t, "add t2", d.cliOp(t, "nginxsite", "add", "t2", "--content", c1))
	if got := ngxSha256sum(t, file); add.SHA256 != ngxSha(c1) || got != add.SHA256 || !add.Created || !add.Reloaded {
		t.Fatalf("add: result %+v, sha256sum %s, content sha256 %s", add, got, ngxSha(c1))
	}
	ngxServes(t, port, m1)

	// Steps 2 and 3.
	upd := ngxResult[ngxspec.WriteResult](t, "update t2", d.cliOp(t, "nginxsite", "update", "t2", "--content", c2,
		"--if-match", ngxSha(c1)))
	got := ngxSha256sum(t, file)
	t.Logf("update: result sha256 %s, content sha256 %s, sha256sum %s; previous %s\nls -ln:\n%s", upd.SHA256, ngxSha(c2),
		got, upd.PreviousSHA256, ngxLs(t))
	if upd.SHA256 != ngxSha(c2) || got != upd.SHA256 || upd.PreviousSHA256 != ngxSha(c1) || !upd.Reloaded {
		t.Fatalf("update: result %+v, sha256sum %s, content sha256 %s", upd, got, ngxSha(c2))
	}
	if uid, gid, perm := fileStat(t, file); uid != 0 || gid != 0 || perm != 0o644 {
		t.Fatalf("t2.conf: %d:%d %04o, want 0:0 0644", uid, gid, perm)
	}
	ngxServes(t, port, m2)
}

// --- TC-109 ---------------------------------------------------------------

// nginx-provider TC-109: Rolled-back writes leave the previous content
// served. Port 18090 is held by a listener of this test process (in place
// of `nc -l`, which the Ubuntu image lacks): any process holding it makes
// nginx's bind fail when it applies the configuration.
func TestNginxRollbackObserved(t *testing.T) {
	const port = 18083
	d := newNgxDaemon(t, "tc109", "", "")
	ngxEnsureRunning(t)
	ngxForget(t, "t3")
	m1 := ngxMarker(t, "tc109")
	d.add(t, "t3", ngxSiteConf(t, port, m1))
	ngxServes(t, port, m1)
	before := ngxSnapshot(t)
	t.Logf("before: ls -ln:\n%s", ngxLs(t))

	// Step 1: invalid.
	op := d.run(t, ngxSiteOp("t3", "update"), ngxWriteBody(t, ngxFixture(t, "invalid.conf"), nil))
	p := ngxFailed(t, "update t3 with invalid.conf", op, problem.NginxConfigInvalid)
	if p.Extensions["rolledBack"] != true {
		t.Fatalf("invalid: rolledBack %v", p.Extensions["rolledBack"])
	}
	t.Logf("step 1: ls -ln:\n%s", ngxLs(t))
	ngxSameSnapshot(t, "invalid.conf rolled back", before)
	ngxServes(t, port, m1)

	// Step 2: port conflict.
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", ngxConflictPort))
	if err != nil {
		t.Fatalf("hold port %d: %v", ngxConflictPort, err)
	}
	defer ln.Close()
	g := ngxObserve(t)
	op = d.run(t, ngxSiteOp("t3", "update"), ngxWriteBody(t, ngxFixture(t, "port-conflict.conf"), nil))
	p = ngxFailed(t, "update t3 with port-conflict.conf", op, problem.NginxReloadFailed)
	obs, rb := ngxObservation(t, p, "observed"), ngxObservation(t, p, "rollbackReload")
	t.Logf("ASM-7 on %s: the reload of the conflicting configuration: observed %+v (initExitCode %d); rollback reload %+v",
		detectedInit(), obs, obs.InitExitCode, rb)
	if p.Extensions["rolledBack"] != true || obs.Verdict != ngxspec.VerdictRejected || rb.Verdict != ngxspec.VerdictConfirmed {
		t.Fatalf("port conflict: rolledBack %v, observed %s, rollbackReload %s", p.Extensions["rolledBack"],
			obs.Verdict, rb.Verdict)
	}
	if obs.MasterPID != g.master {
		t.Fatalf("observed master %d, /proc master %d", obs.MasterPID, g.master)
	}
	if b, err := os.ReadFile(ngxErrorLog); err == nil {
		re := regexp.MustCompile(`(?m)^.*bind\(\) to 0\.0\.0\.0:` + strconv.Itoa(ngxConflictPort) + ` failed.*$`)
		t.Logf("%s: %q", ngxErrorLog, re.FindAllString(string(b), -1))
	}
	t.Logf("step 2: ls -ln:\n%s", ngxLs(t))
	ngxSameSnapshot(t, "port-conflict.conf rolled back", before)
	ngxServes(t, port, m1)
}

// --- TC-110 ---------------------------------------------------------------

// nginx-provider TC-110: Writes across a restart, with a killed supervisor,
// and queued across a restart. The real nginx -t of the harness finishes in
// milliseconds even with 2,000 includes (measured: see
// docs/verification/nginx-t1.md), so the window is widened with the TC-115
// wrapper, which delays only -t.
func TestNginxWriteRestartCases(t *testing.T) {
	const port = 18084
	d := newNgxDaemon(t, "tc110", "", "")
	ngxEnsureRunning(t)
	ngxForget(t, "t4")
	file := filepath.Join(ngxIncludeDir(), "t4.conf")
	m := []string{ngxMarker(t, "tc110m1"), ngxMarker(t, "tc110m2"), ngxMarker(t, "tc110m3"), ngxMarker(t, "tc110m4")}
	d.add(t, "t4", ngxSiteConf(t, port, m[0]))
	ngxServes(t, port, m[0])
	ngxWrap(t)

	metaOf := func(op apitypes.Operation) string {
		t.Helper()
		var dir string
		waitUntil(t, time.Minute, "previous/meta.json of "+op.ID, func() (bool, string) {
			dir = opDirOf(op.ID)
			return dir != "" && exists(filepath.Join(dir, "previous", "meta.json")), dir
		})
		return dir
	}

	t.Run("A restart", func(t *testing.T) {
		ngxWrapMode(t, "6")
		c := ngxSiteConf(t, port, m[1])
		op := d.begin(t, ngxSiteOp("t4", "update"), ngxWriteBody(t, c, nil))
		dir := metaOf(op)
		helpers := ngxHelperPIDs(dir)
		t.Logf("previous/meta.json written; helper %v running", helpers)
		if len(helpers) != 1 {
			t.Fatalf("helper processes %v, want one", helpers)
		}
		d.restartNexusd(t)
		t.Logf("helper %d alive after the restart: %v", helpers[0], pidAlive(helpers[0]))
		end := d.waitOp(op.ID, 3*time.Minute)
		res := ngxResult[ngxspec.WriteResult](t, "update across a restart", end)
		got := ngxSha256sum(t, file)
		t.Logf("A: %s, result sha256 %s, sha256sum %s", end.Status, res.SHA256, got)
		if got != ngxSha(c) || res.SHA256 != got {
			t.Fatalf("A: sha256sum %s, result %s, content %s", got, res.SHA256, ngxSha(c))
		}
		ngxWrapMode(t, "")
		ngxServes(t, port, m[1])
	})

	t.Run("B supervisor killed", func(t *testing.T) {
		pre := ngxSha256sum(t, file)
		ngxWrapMode(t, "8")
		op := d.begin(t, ngxSiteOp("t4", "update"), ngxWriteBody(t, ngxSiteConf(t, port, m[2]), nil))
		dir := metaOf(op)
		helpers := ngxHelperPIDs(dir)
		sup, err := strconv.Atoi(showUnit(t, detach.Unit(op.ID), "MainPID")["MainPID"])
		if err != nil || sup <= 0 || len(helpers) != 1 {
			t.Fatalf("supervisor %d (%v), helpers %v", sup, err, helpers)
		}
		t.Logf("kill -9 supervisor %d (%q) after the backup; helper %d", sup, procArgv(sup), helpers[0])
		if err := syscall.Kill(sup, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		end := d.waitOp(op.ID, 3*time.Minute)
		p := ngxFailed(t, "update with the supervisor killed", end, problem.OutcomeUnknown)
		log := d.opLog(t, op.ID)
		prev := filepath.Join(dir, "previous", "content")
		want := "previous content of t4.conf is kept at " + prev
		t.Logf("B: %s %s; op log:\n%s", end.Status, p.Detail, log)
		if !strings.Contains(log, want) {
			t.Fatalf("B: the op log lacks %q", want)
		}
		if got := ngxSha256sum(t, prev); got != pre {
			t.Fatalf("B: %s has sha256 %s, the pre-op t4.conf %s", prev, got, pre)
		}
		t.Logf("B: sha256sum %s = %s, the pre-op t4.conf", prev, pre)
		waitUntil(t, 2*time.Minute, "the orphaned helper to exit", func() (bool, string) {
			return len(ngxHelperPIDs(dir)) == 0, fmt.Sprint(ngxHelperPIDs(dir))
		})
		ngxWrapMode(t, "")
		t.Logf("B: after the helper exited, t4.conf sha256 %s", ngxSha256sum(t, file))
	})

	t.Run("C queued across a restart", func(t *testing.T) {
		ngxWrapMode(t, "10")
		before := ngxSnapshot(t)
		test := d.begin(t, ngxAt+"/actions/test", "{}")
		waitUntil(t, time.Minute, "the test op to run", func() (bool, string) {
			s := d.operation(test.ID).Status
			return s == apitypes.StatusRunning, string(s)
		})
		upd := d.begin(t, ngxSiteOp("t4", "update"), ngxWriteBody(t, ngxSiteConf(t, port, m[3]), nil))
		if s := d.operation(upd.ID).Status; s != apitypes.StatusQueued {
			t.Fatalf("C: the update is %s behind the running test, want queued", s)
		}
		d.restartNexusd(t)
		end := d.waitOp(upd.ID, 3*time.Minute)
		p := ngxFailed(t, "queued update across a restart", end, problem.OutcomeUnknown)
		if !strings.Contains(p.Detail, "never started") {
			t.Fatalf("C: detail %q, want never started", p.Detail)
		}
		tr := ngxResult[ngxspec.TestResult](t, "the running test across the restart", d.waitOp(test.ID, 3*time.Minute))
		t.Logf("C: update %s %q; the test op ended valid=%v", end.Status, p.Detail, tr.Valid)
		ngxSameSnapshot(t, "queued update never started", before)
		ngxWrapMode(t, "")
	})
}

// --- TC-111 ---------------------------------------------------------------

// ngxManualTest is the independent verdict of nginx -t on a copy: in a
// private mount namespace (unshare -m), a copy of includeDir with file
// holding content is bind-mounted over includeDir and the real nginx -t
// runs against it. Nothing outside that namespace sees the mount.
func ngxManualTest(t *testing.T, file, content string) (bool, string) {
	t.Helper()
	cp := t.TempDir()
	mustSh(t, "cp", "-a", ngxIncludeDir()+"/.", cp+"/")
	if err := os.WriteFile(filepath.Join(cp, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := sh(time.Minute, "unshare", "-m", "--propagation", "private", "sh", "-c",
		`mount --bind "$1" "$2" && exec `+ngxBin+` -t`, "sh", cp, ngxIncludeDir())
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("manual nginx -t: %v %s", err, out)
	}
	return err == nil, strings.TrimSpace(out)
}

// nginx-provider TC-111: whatIf leaves the mount table and includeDir
// unchanged.
func TestNginxWhatIfObserved(t *testing.T) {
	const port = 18085
	d := newNgxDaemon(t, "tc111", "", "")
	ngxEnsureRunning(t)
	ngxForget(t, "t5")
	m1 := ngxMarker(t, "tc111")
	d.add(t, "t5", ngxSiteConf(t, port, m1))
	ngxServes(t, port, m1)
	g := ngxWaitRunning(t)
	mountinfo := func() []byte {
		b, err := os.ReadFile("/proc/1/mountinfo")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mi, before := mountinfo(), ngxSnapshot(t)

	for _, c := range []struct{ what, content string }{
		{"valid", ngxSiteConf(t, port, ngxMarker(t, "tc111new"))},
		{"invalid", ngxFixture(t, "invalid.conf")},
	} {
		res := ngxResult[ngxspec.WriteResult](t, "whatIf "+c.what,
			d.run(t, ngxSiteOp("t5", "update"), ngxWriteBody(t, c.content, map[string]any{"whatIf": true})))
		manual, out := ngxManualTest(t, "t5.conf", c.content)
		t.Logf("whatIf %s: Nexus valid=%v, manual nginx -t on a copy valid=%v:\n%s", c.what, res.Valid != nil && *res.Valid,
			manual, out)
		if !res.WhatIf || res.Valid == nil || *res.Valid != manual || manual != (c.what == "valid") {
			t.Fatalf("whatIf %s: result %+v, manual verdict %v", c.what, res, manual)
		}
	}
	if after := mountinfo(); !bytes.Equal(after, mi) {
		t.Fatalf("/proc/1/mountinfo changed:\nbefore:\n%s\nafter:\n%s", mi, after)
	}
	t.Logf("/proc/1/mountinfo byte-identical (%d bytes)", len(mi))
	ngxSameSnapshot(t, "whatIf", before)
	if now := ngxObserve(t); now.master != g.master || !slices.Equal(now.workers, g.workers) {
		t.Fatalf("nginx reloaded: %s -> %s", g, now)
	}
	t.Logf("nginx not reloaded: %s", g)
	ngxServes(t, port, m1)
}

// --- TC-112 ---------------------------------------------------------------

// ngxVersion is the version `nginx -v` prints when run as root.
func ngxVersion(t *testing.T) string {
	t.Helper()
	out := mustSh(t, ngxBin, "-v")
	m := regexp.MustCompile(`nginx/([0-9.]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("nginx -v: %q", out)
	}
	return m[1]
}

// ngxEveryOp calls every nginx operation through n and reads each back
// without Nexus. ok records one op. The test site is tc112 on port 18092.
func ngxEveryOp(t *testing.T, n *nexusdInstance, ok func(op, format string, args ...any)) {
	t.Helper()
	const port = 18092
	d := &ngxDaemon{nexusdInstance: n}
	ngxEnsureRunning(t)
	ngxForget(t, "tc112")
	file := filepath.Join(ngxIncludeDir(), "tc112.conf")

	var svc apitypes.List[ngxspec.Service]
	n.mustGet(ngxAt+"/service", &svc)
	g := ngxObserve(t)
	if len(svc.Items) != 1 {
		t.Fatalf("service.list: %d items", len(svc.Items))
	}
	sp := svc.Items[0].Properties
	if v := ngxVersion(t); sp.Version == "" || sp.Version != v || sp.State != "running" || sp.MainPID == nil ||
		*sp.MainPID != g.master || sp.IncludeDir != ngxIncludeDir() {
		t.Fatalf("status %+v; nginx -v as root %s, /proc master %d, includeDir %s", sp, v, g.master, ngxIncludeDir())
	}
	ok("nginx.service.list", "running, mainPid %d = /proc, version %s = nginx -v as root, includeDir %s", g.master,
		sp.Version, sp.IncludeDir)

	tr := ngxResult[ngxspec.TestResult](t, "test", d.run(t, ngxAt+"/actions/test", "{}"))
	if !tr.Valid {
		t.Fatalf("test of the stock configuration: valid false:\n%s", tr.Output)
	}
	ok("nginx.actions.test", "valid true for the stock configuration (nginx -t ran outside the sandbox)")

	svcOp := func(verb, want string) ngxspec.ServiceResult {
		t.Helper()
		res := ngxResult[ngxspec.ServiceResult](t, verb, d.run(t, ngxAt+"/actions/"+verb, "{}"))
		if st := ngxInitState(t); st != want || res.State != want {
			t.Fatalf("%s: init system %s, Nexus %s, want %s", verb, st, res.State, want)
		}
		return res
	}
	svcOp("stop", "stopped")
	if g := ngxObserve(t); g.master != 0 {
		t.Fatalf("stop: master %d still runs", g.master)
	}
	ok("nginx.actions.stop", "%s says stopped, no master in /proc", detectedInit())
	svcOp("start", "running")
	g1 := ngxWaitRunning(t)
	ok("nginx.actions.start", "%s says running, master %d", detectedInit(), g1.master)
	svcOp("restart", "running")
	g2 := ngxWaitRunning(t)
	if g2.master == g1.master {
		t.Fatalf("restart: master %d unchanged", g2.master)
	}
	ok("nginx.actions.restart", "master %d -> %d", g1.master, g2.master)
	rl := svcOp("reload", "running")
	g3 := ngxWorkersReplaced(t, g2)
	if rl.Reload == nil || rl.Reload.Verdict != ngxspec.VerdictConfirmed {
		t.Fatalf("reload: %+v", rl.Reload)
	}
	ok("nginx.actions.reload", "confirmed: master %d, workers %v -> %v", g3.master, g2.workers, g3.workers)

	m1, m2 := ngxMarker(t, "tc112m1"), ngxMarker(t, "tc112m2")
	c1, c2 := ngxSiteConf(t, port, m1), ngxSiteConf(t, port, m2)
	add := d.add(t, "tc112", c1)
	if got := ngxSha256sum(t, file); got != add.SHA256 || got != ngxSha(c1) {
		t.Fatalf("add: sha256sum %s, result %s", got, add.SHA256)
	}
	ngxServes(t, port, m1)
	ok("nginx.sites.add", "sha256sum %s; curl :%d -> %s", add.SHA256[:12], port, m1)

	var list apitypes.List[ngxspec.Site]
	n.mustGet(ngxSites+"?limit=1000", &list)
	i := slices.IndexFunc(list.Items, func(s ngxspec.Site) bool { return s.Name == "tc112" })
	if i < 0 || !list.Items[i].Properties.Managed || !list.Items[i].Properties.Enabled ||
		list.Items[i].Properties.SHA256 == nil || *list.Items[i].Properties.SHA256 != add.SHA256 {
		t.Fatalf("sites.list: tc112 at %d in %+v", i, list.Items)
	}
	ok("nginx.sites.list", "%d sites, tc112 managed and enabled with the sha256sum", len(list.Items))
	var site ngxspec.Site
	n.mustGet(ngxSites+"/tc112", &site)
	if site.Properties.SHA256 == nil || *site.Properties.SHA256 != ngxSha256sum(t, file) {
		t.Fatalf("sites.get: %+v", site.Properties)
	}
	ok("nginx.sites.get", "sha256 = sha256sum")
	var content ngxspec.SiteContent
	n.mustGet(ngxSites+"/tc112/content", &content)
	if onDisk, err := os.ReadFile(file); err != nil || content.Content != string(onDisk) || content.SHA256 != ngxSha256sum(t, file) {
		t.Fatalf("sites.content: %+v (%v)", content, err)
	}
	ok("nginx.sites.content", "content = the file's bytes, sha256 = sha256sum")

	upd := ngxResult[ngxspec.WriteResult](t, "update tc112", d.run(t, ngxSiteOp("tc112", "update"),
		ngxWriteBody(t, c2, map[string]any{"ifMatch": content.SHA256})))
	if got := ngxSha256sum(t, file); got != upd.SHA256 || got != ngxSha(c2) {
		t.Fatalf("update: sha256sum %s, result %s", got, upd.SHA256)
	}
	ngxServes(t, port, m2)
	ok("nginx.sites.update", "ifMatch: sha256sum %s; curl :%d -> %s", upd.SHA256[:12], port, m2)

	ngxResult[ngxspec.SiteToggleResult](t, "disable tc112", d.run(t, ngxSiteOp("tc112", "disable"), "{}"))
	if exists(file) || !exists(file+".disabled") {
		t.Fatalf("disable: ls -ln:\n%s", ngxLs(t))
	}
	ngxRefused(t, port)
	ok("nginx.sites.disable", "tc112.conf.disabled on disk; curl :%d refused", port)
	ngxResult[ngxspec.SiteToggleResult](t, "enable tc112", d.run(t, ngxSiteOp("tc112", "enable"), "{}"))
	if !exists(file) || exists(file+".disabled") {
		t.Fatalf("enable: ls -ln:\n%s", ngxLs(t))
	}
	ngxServes(t, port, m2)
	ok("nginx.sites.enable", "tc112.conf on disk; curl :%d -> %s", port, m2)
}

// nginx-provider TC-112: Every nginx op runs under the exact hardened unit
// with no denial. The journal of nexusd.service and of every nexus-op-*
// unit in the window is read for seccomp and permission denials.
func TestNginxUnderExactUnit(t *testing.T) {
	requireInit(t, platform.Systemd)
	freshHost(t)
	runInstall(t)
	writeConfig(t, fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n%s", exactAddr,
		ngxSection("")))
	ngxEnsureRunning(t)
	dmesgBefore := kernelLines(t)
	since := time.Now().Add(-time.Second)
	pid := startExact(t)
	n := exactNexusd(t, ngxScopes...)
	var done []string
	ngxEveryOp(t, n, func(op, format string, args ...any) {
		done = append(done, op)
		t.Logf("%-24s %s", op, fmt.Sprintf(format, args...))
	})
	if len(done) != len(ngxspec.Spec().Ops) {
		t.Fatalf("%d nginx operations exercised (%q), the provider has %d", len(done), done, len(ngxspec.Spec().Ops))
	}

	sinceArg := "@" + strconv.FormatInt(since.Unix(), 10)
	journal := mustSh(t, "journalctl", "-u", nexusdUnit, "-u", "nexus-op-*", "--since", sinceArg, "--no-pager")
	newKernel := kernelLines(t)
	if len(newKernel) >= len(dmesgBefore) {
		newKernel = newKernel[len(dmesgBefore):]
	}
	var faults []string
	for _, d := range denialPatterns {
		if d.pattern != "SECCOMP" && d.pattern != "audit: type=1326" && d.pattern != "Permission denied" &&
			d.pattern != "Read-only file system" {
			continue
		}
		for line := range strings.Lines(journal) {
			if strings.Contains(line, d.pattern) {
				faults = append(faults, fmt.Sprintf("journal (suspect %s): %s", d.directives, strings.TrimSpace(line)))
			}
		}
		for _, line := range newKernel {
			if strings.Contains(line, d.pattern) && slices.ContainsFunc(append(sandboxComms, `comm="nginx"`),
				func(c string) bool { return strings.Contains(line, c) }) {
				faults = append(faults, fmt.Sprintf("kernel (suspect %s): %s", d.directives, strings.TrimSpace(line)))
			}
		}
	}
	u := showUnit(t, nexusdUnit, "MainPID", "NRestarts", "ActiveState")
	opUnits := regexp.MustCompile(`nexus-op-[0-9a-z-]+\.service`).FindAllString(journal, -1)
	slices.Sort(opUnits)
	t.Logf("journal window: %d lines from nexusd.service and %d nexus-op units %v; %d new kernel lines; nexusd.service %v",
		strings.Count(journal, "\n"), len(slices.Compact(opUnits)), slices.Compact(opUnits), len(newKernel), u)
	if u["MainPID"] != strconv.Itoa(pid) || u["NRestarts"] != "0" || u["ActiveState"] != "active" {
		faults = append(faults, fmt.Sprintf("nexusd.service %v, started as MainPID %d", u, pid))
	}
	if len(faults) > 0 {
		t.Fatalf("denials:\n%s\n--- journal ---\n%s", strings.Join(faults, "\n"), journal)
	}
}

// --- TC-113 ---------------------------------------------------------------

// ngxScenario is one svcctltest scenario run against the real init system.
type ngxScenario struct {
	name  string
	model svcctltest.Model
	argv  []string // The fake's argv: svcctl's call for the nginx service
	real  []string // The real argv, when it differs (an unknown service)
	// phrases must be in both the fake's and the real output.
	phrases []string
	setup   func(t *testing.T) // The real host state
	reload  bool               // Compare the generation: fake's model, real workers
}

// fakenginxErrorLogLine is the read-only error-log line fakenginx's header
// lists (internal/providers/nginx/testdata/fakenginx/main.go), captured
// from nginx 1.26.3 on Alpine; the path is the build's error log.
const fakenginxErrorLogLine = `nginx: [alert] could not open error log file: open() "/var/lib/nginx/logs/error.log" failed (30: Read-only file system)`

// nginx-provider TC-113: The fakes match the real init systems and nginx.
// Each scenario's real exit code must equal svcctltest's for this init
// system, and its output must hold the fake's key phrases; a reload
// scenario must also move the real generation exactly as the fake's model
// does. The svcctltest fake runs here as it does at T0, answering from the
// captures in internal/svcctl/testdata, which run.sh copies to where its
// source path resolves from /harness/src.
func TestFakesMatchRealBehavior(t *testing.T) {
	t.Chdir("/harness/src")
	initSys := detectedInit()
	ngxEnsureRunning(t)
	ngxForget(t, "zz-t1-bad", "zz-t1-bind")
	dir := ngxIncludeDir()
	held := func(t *testing.T) {
		ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", ngxConflictPort))
		if err != nil {
			t.Fatalf("hold port %d: %v", ngxConflictPort, err)
		}
		t.Cleanup(func() { _ = ln.Close() })
	}
	place := func(file, content string) func(t *testing.T) {
		return func(t *testing.T) {
			ngxPlace(t, file, content, 0)
			t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, file)) })
		}
	}
	stopped := func(t *testing.T) {
		ngxInit(t, "stop")
		t.Cleanup(func() { ngxEnsureRunning(t) })
	}
	var scenarios []ngxScenario
	if initSys == platform.OpenRC {
		rc := func(verb string) []string { return []string{"/sbin/rc-service", "-C", "nginx", verb} }
		scenarios = []ngxScenario{
			{name: "reload with an invalid config", model: svcctltest.Model{State: svcctl.Running, Config: svcctltest.Invalid},
				argv: rc("reload"), phrases: []string{`unknown directive "bogus_directive"`, "test failed"},
				setup: place("zz-t1-bad.conf", "bogus_directive;\n"), reload: true},
			{name: "reload with a bound port", model: svcctltest.Model{State: svcctl.Running, Config: svcctltest.BindConflict},
				argv: rc("reload"), phrases: []string{"Reloading nginx configuration"},
				setup: func(t *testing.T) { held(t); place("zz-t1-bind.conf", ngxFixture(t, "port-conflict.conf"))(t) }, reload: true},
			{name: "reload valid", model: svcctltest.Model{State: svcctl.Running}, argv: rc("reload"),
				phrases: []string{"Reloading nginx configuration"}, reload: true},
			{name: "start when started", model: svcctltest.Model{State: svcctl.Running}, argv: rc("start"),
				phrases: []string{"WARNING: nginx has already been started"}},
			{name: "status of a stopped service", model: svcctltest.Model{State: svcctl.Stopped}, argv: rc("status"),
				phrases: []string{"status: stopped"}, setup: stopped},
			{name: "--exists of an unknown service", model: svcctltest.Model{Absent: true},
				argv: []string{"/sbin/rc-service", "--exists", "nginx"}, real: []string{"/sbin/rc-service", "--exists", "nxt1absent"}},
		}
	} else {
		sc := func(verb string) []string { return []string{"/usr/bin/systemctl", verb, "--", "nginx.service"} }
		show := func(unit string) []string {
			return []string{"/usr/bin/systemctl", "show", "--timestamp=unix", "-p", "LoadState", "-p", "ActiveState",
				"-p", "SubState", "-p", "MainPID", "-p", "StateChangeTimestamp", "--", unit}
		}
		scenarios = []ngxScenario{
			{name: "reload with an invalid config", model: svcctltest.Model{State: svcctl.Running, Config: svcctltest.Invalid},
				argv: sc("reload"), phrases: []string{"Job for nginx.service failed."},
				setup: place("zz-t1-bad.conf", "bogus_directive;\n"), reload: true},
			{name: "reload with a bound port", model: svcctltest.Model{State: svcctl.Running, Config: svcctltest.BindConflict},
				argv: sc("reload"), setup: func(t *testing.T) { held(t); place("zz-t1-bind.conf", ngxFixture(t, "port-conflict.conf"))(t) },
				reload: true},
			{name: "reload valid", model: svcctltest.Model{State: svcctl.Running}, argv: sc("reload"), reload: true},
			{name: "start when started", model: svcctltest.Model{State: svcctl.Running}, argv: sc("start")},
			{name: "status of a stopped service", model: svcctltest.Model{State: svcctl.Stopped}, argv: show("nginx.service"),
				phrases: []string{"LoadState=loaded", "ActiveState=inactive", "SubState=dead", "MainPID=0"}, setup: stopped},
			{name: "show of an unknown service", model: svcctltest.Model{Absent: true}, argv: show("nginx.service"),
				real: show("nxt1absent.service"), phrases: []string{"LoadState=not-found", "ActiveState=inactive", "MainPID=0"}},
		}
	}

	var mismatches []string
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if s.setup != nil {
				s.setup(t)
			}
			fake := svcctltest.New(t, initSys, s.model)
			fres, ferr := fake.Run(context.Background(), s.argv)
			if ferr != nil {
				t.Fatalf("fake %q: %v", s.argv, ferr)
			}
			argv := s.argv
			if s.real != nil {
				argv = s.real
			}
			var before ngxGen
			if s.reload {
				before = ngxWaitRunning(t)
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
			out, err := cmd.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				code = ee.ExitCode()
			case err != nil:
				t.Fatalf("%q: %v", argv, err)
			}
			t.Logf("%s on %s\n  real %q: exit %d\n%s\n  fake %q: exit %d\n%s", s.name, initSys, argv, code,
				ngxIndent(string(out)), s.argv, fres.ExitCode, ngxIndent(string(fres.Output)))
			var bad []string
			if code != fres.ExitCode {
				bad = append(bad, fmt.Sprintf("exit %d, the fake's %d", code, fres.ExitCode))
			}
			for _, p := range s.phrases {
				if !strings.Contains(string(fres.Output), p) {
					t.Fatalf("the scenario's phrase %q is not in the fake's own output %q: fix the table", p, fres.Output)
				}
				if !strings.Contains(string(out), p) {
					bad = append(bad, fmt.Sprintf("output lacks the fake's %q", p))
				}
			}
			if s.reload {
				fakeMoved := fake.Model().Generation != s.model.Generation
				var after ngxGen
				if fakeMoved {
					after = ngxWorkersReplaced(t, before)
				} else {
					time.Sleep(3 * time.Second)
					after = ngxObserve(t)
				}
				realMoved := after.master == before.master &&
					!slices.ContainsFunc(after.workers, func(p int) bool { return slices.Contains(before.workers, p) })
				t.Logf("generation: fake %d -> %d; real %s -> %s", s.model.Generation, fake.Model().Generation, before, after)
				if realMoved != fakeMoved || after.master != before.master {
					bad = append(bad, fmt.Sprintf("real generation moved %v (%s -> %s), the fake's %v", realMoved, before,
						after, fakeMoved))
				}
			}
			if len(bad) > 0 {
				mismatches = append(mismatches, s.name+": "+strings.Join(bad, "; "))
				t.Errorf("MISMATCH %s: %s\n  real:\n%s\n  fake:\n%s", s.name, strings.Join(bad, "; "), ngxIndent(string(out)),
					ngxIndent(string(fres.Output)))
			}
		})
	}

	// Step 3: nginx -t with its error-log directory read-only, through a
	// read-only bind mount in a private mount namespace.
	t.Run("nginx -t with a read-only error log", func(t *testing.T) {
		out, err := sh(time.Minute, "unshare", "-m", "--propagation", "private", "sh", "-c",
			"mount --bind /var/log/nginx /var/log/nginx && mount -o remount,bind,ro /var/log/nginx && exec "+ngxBin+" -t")
		t.Logf("unshare -m: read-only /var/log/nginx; nginx -t: %v\n%s", err, ngxIndent(out))
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("nginx -t with a read-only error log: %v, want exit 1", err)
		}
		// fakenginx fails an unwritable error-log directory with the [emerg]
		// open() of <dir>/error.log after the parse (its probes), as both
		// builds do at the error_log directive of nginx.conf. Ubuntu's
		// prefixes the timestamp and PID, which fakenginx's header records.
		emerg := regexp.MustCompile(`\[emerg\] (?:\d+#\d+: )?open\(\) "` + regexp.QuoteMeta(ngxErrorLog) +
			`" failed \(30: Read-only file system\)`)
		if !emerg.MatchString(out) || !strings.Contains(out, "test failed") {
			t.Fatalf("nginx -t lacks fakenginx's [emerg] open() %q failed (30: Read-only file system) and test failed", ngxErrorLog)
		}
		// The [alert] line is nginx opening the build's own error log before
		// the parse. A build whose --error-log-path is stderr (Ubuntu's)
		// has none to open, so it prints no [alert]; fakenginx always
		// prints it, in Alpine's form (fake-fidelity note, reported).
		buildLog := ""
		for _, f := range strings.Fields(mustSh(t, ngxBin, "-V")) {
			if v, ok := strings.CutPrefix(f, "--error-log-path="); ok {
				buildLog = v
			}
		}
		if buildLog == "" {
			buildLog = "logs/error.log under --prefix"
		}
		re := regexp.MustCompile(`nginx: \[alert\] could not open error log file: open\(\) "([^"]+)" failed \(30: Read-only file system\)`)
		m := re.FindStringSubmatch(out)
		if buildLog == "stderr" {
			if m != nil {
				t.Fatalf("a build logging to stderr printed %q", m[0])
			}
			t.Logf("FAKE-FIDELITY: this build's --error-log-path is stderr, so nginx -t prints no [alert] line; fakenginx "+
				"prints %q (Alpine's form) in this case. The verdict (exit 1) and the [emerg] error-log line match.",
				fakenginxErrorLogLine)
			return
		}
		if m == nil {
			t.Fatalf("nginx -t (build error log %s) lacks fakenginx's line %q", buildLog, fakenginxErrorLogLine)
		}
		if got := strings.Replace(m[0], m[1], "/var/lib/nginx/logs/error.log", 1); got != fakenginxErrorLogLine {
			t.Fatalf("real %q, fakenginx %q", m[0], fakenginxErrorLogLine)
		}
		t.Logf("real line %q = fakenginx's %q with the build's error log %s", m[0], fakenginxErrorLogLine, m[1])
	})
	if len(mismatches) > 0 {
		t.Errorf("the svcctltest fake does not match the real %s; fix the fake before trusting its results:\n%s", initSys,
			strings.Join(mismatches, "\n"))
	}
}

func ngxIndent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "    (no output)"
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

// --- TC-114 ---------------------------------------------------------------

// ngxTimed is one request with its wall time, the body read to the end.
func ngxTimed(t *testing.T, n *nexusdInstance, method, path, body string) (time.Duration, int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+n.listen+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := n.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	took := time.Since(start)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return took, resp.StatusCode, b
}

// ngxPct is the nearest-rank p-th percentile.
func ngxPct(ds []time.Duration, p int) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	i := (p*len(s)+99)/100 - 1
	return s[max(i, 0)]
}

// ngxProcSample is nexusd's RSS, thread count and CPU ticks from /proc.
type ngxProcSample struct {
	rss     int64 // bytes
	threads int
	ticks   int64 // utime + stime
}

func ngxSample(t *testing.T, pid int) ngxProcSample {
	t.Helper()
	var s ngxProcSample
	if v := procStatusField(pid, "VmRSS"); v != "" {
		kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
		s.rss = kb << 10
	}
	s.threads, _ = strconv.Atoi(procStatusField(pid, "Threads"))
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
	ut, _ := strconv.ParseInt(f[11], 10, 64) // utime, field 14
	st, _ := strconv.ParseInt(f[12], 10, 64) // stime, field 15
	s.ticks = ut + st
	if s.rss == 0 || s.threads == 0 {
		t.Fatalf("/proc/%d: %+v", pid, s)
	}
	return s
}

const ngxIdle = 60 * time.Second

// nginx-provider TC-114: Latency and footprint with real nginx and 50
// sites. NFR-5's RSS delta is nexusd idle with nginx disabled against the
// same configuration with it enabled, each after 60 s idle.
func TestNginxLatencyAndFootprint(t *testing.T) {
	ngxEnsureRunning(t)
	var names []string
	for i := range 50 {
		names = append(names, fmt.Sprintf("lat%02d", i))
	}
	ngxForget(t, names...)
	for i, name := range names {
		file := name + ".conf"
		if i%2 == 1 {
			file += ".disabled"
		}
		ngxPlace(t, file, ngxSiteConf(t, 18100+i, ngxMarker(t, "tc114")), 0)
	}
	ngxInit(t, "reload")
	ngxWaitRunning(t)
	t.Logf("50 sites in %s (25 enabled on 18100-18148, 25 disabled)", ngxIncludeDir())

	// Step 3, disabled then enabled.
	off := newNexusd(t, "tc114off", "  nginx: {enabled: false}\n")
	time.Sleep(ngxIdle)
	offRSS := ngxSample(t, off.mainPID()).rss
	d := newNgxDaemon(t, "tc114", "", "")
	pid := d.mainPID()
	start := ngxSample(t, pid)
	time.Sleep(ngxIdle)
	end := ngxSample(t, pid)
	delta := end.rss - offRSS
	cpu := float64(end.ticks-start.ticks) * 10 / float64(ngxIdle.Milliseconds()) * 100
	t.Logf("NFR-5: idle RSS nginx disabled %.2f MiB, enabled %.2f MiB, delta %.2f MiB; over the enabled idle window "+
		"threads %d -> %d, CPU %d ticks (%.3f%% of one core)", float64(offRSS)/(1<<20), float64(end.rss)/(1<<20),
		float64(delta)/(1<<20), start.threads, end.threads, end.ticks-start.ticks, cpu)
	if delta >= 5<<20 {
		t.Errorf("RSS delta %d bytes, want under 5 MiB", delta)
	}
	if end.threads > start.threads || cpu >= 1 {
		t.Errorf("idle not flat: threads %d -> %d, CPU %.3f%% of one core", start.threads, end.threads, cpu)
	}

	// Step 1.
	for _, r := range []struct{ what, path string }{
		{"status", ngxAt + "/service"},
		{"list", ngxSites + "?limit=1000"},
		{"get", ngxSites + "/lat07"},
	} {
		var ds []time.Duration
		for range 200 {
			took, code, b := ngxTimed(t, d.nexusdInstance, http.MethodGet, r.path, "")
			if code != http.StatusOK {
				t.Fatalf("%s: %d %s", r.what, code, b)
			}
			ds = append(ds, took)
		}
		p95 := ngxPct(ds, 95)
		t.Logf("NFR-1 %s: 200 requests, p50 %v p95 %v max %v", r.what, ngxPct(ds, 50), p95, slices.Max(ds))
		if p95 >= 200*time.Millisecond {
			t.Errorf("%s p95 %v, want under 200 ms", r.what, p95)
		}
	}
	var list apitypes.List[ngxspec.Site]
	d.mustGet(ngxSites+"?limit=1000", &list)
	if n := len(slices.DeleteFunc(list.Items, func(s ngxspec.Site) bool { return !strings.HasPrefix(s.Name, "lat") })); n != 50 {
		t.Fatalf("list: %d lat sites, want 50", n)
	}

	// Step 2.
	var ds []time.Duration
	for range 5 {
		took, code, b := ngxTimed(t, d.nexusdInstance, http.MethodPost, ngxAt+"/actions/test", "{}")
		if code != http.StatusAccepted {
			t.Fatalf("test: %d %s", code, b)
		}
		var op apitypes.Operation
		if err := json.Unmarshal(b, &op); err != nil {
			t.Fatal(err)
		}
		ngxResult[ngxspec.TestResult](t, "test", d.waitOp(op.ID, time.Minute))
		ds = append(ds, took)
	}
	t.Logf("NFR-1 202 of the test op: %v", ds)
	if m := slices.Max(ds); m >= 500*time.Millisecond {
		t.Errorf("202 took %v, want under 500 ms", m)
	}
}

// --- TC-115 ---------------------------------------------------------------

// nginx-provider TC-115: A broken or hung nginx affects only nginx
// operations.
func TestNginxIsolation(t *testing.T) {
	d := newNgxDaemon(t, "tc115", ", testTimeout: 5s", dockerSection(dockerSocket)+"  scripts: {enabled: true}\n")
	ngxEnsureRunning(t)
	ngxForget(t, "zz-t1-broken")
	t.Cleanup(func() { ngxEnsureRunning(t) })

	others := func(state string) {
		t.Helper()
		for _, r := range []struct{ what, path string }{
			{"scripts list", "/v1/providers/scripts/scripts"},
			{"docker list", containersAt},
			{"capabilities", "/v1/capabilities"},
		} {
			took, code, b := ngxTimed(t, d.nexusdInstance, http.MethodGet, r.path, "")
			t.Logf("%s: %s %d in %v", state, r.what, code, took)
			if code != http.StatusOK || took >= 200*time.Millisecond {
				t.Fatalf("%s: %s %d in %v: %s", state, r.what, code, took, b)
			}
		}
		var caps apitypes.Capabilities
		d.mustGet("/v1/capabilities", &caps)
		for _, pc := range caps.Providers {
			if slices.Contains([]string{ngxspec.Name, "docker", "scripts"}, pc.Name) && pc.Status != apitypes.ProviderEnabled {
				t.Fatalf("%s: provider %s %s (%s)", state, pc.Name, pc.Status, pc.Reason)
			}
		}
	}
	status := func(state string) ngxspec.ServiceProperties {
		t.Helper()
		took, code, b := ngxTimed(t, d.nexusdInstance, http.MethodGet, ngxAt+"/service", "")
		var l apitypes.List[ngxspec.Service]
		if code != http.StatusOK || json.Unmarshal(b, &l) != nil || len(l.Items) != 1 {
			t.Fatalf("%s: status %d %s", state, code, b)
		}
		t.Logf("%s: status 200 in %v: %s", state, took, b)
		return l.Items[0].Properties
	}

	t.Run("stopped", func(t *testing.T) {
		ngxInit(t, "stop")
		if st := status("stopped"); st.State != "stopped" {
			t.Fatalf("status %s with nginx stopped (%s says %s)", st.State, detectedInit(), ngxInitState(t))
		}
		tr := ngxResult[ngxspec.TestResult](t, "test", d.run(t, ngxAt+"/actions/test", "{}"))
		t.Logf("stopped: test valid=%v", tr.Valid)
		others("stopped")
		ngxEnsureRunning(t)
	})

	t.Run("broken", func(t *testing.T) {
		ngxPlace(t, "zz-t1-broken.conf", ngxFixture(t, "invalid.conf"), 0)
		status("broken")
		tr := ngxResult[ngxspec.TestResult](t, "test", d.run(t, ngxAt+"/actions/test", "{}"))
		if tr.Valid || !strings.Contains(tr.Output, "nexus_t1_unknown_directive") {
			t.Fatalf("broken: test valid=%v output %q", tr.Valid, tr.Output)
		}
		others("broken")
		if err := os.Remove(filepath.Join(ngxIncludeDir(), "zz-t1-broken.conf")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("hung", func(t *testing.T) {
		ngxWrap(t)
		ngxWrapMode(t, "hang")
		op := d.begin(t, ngxAt+"/actions/test", "{}")
		waitUntil(t, time.Minute, "the test op to run", func() (bool, string) {
			s := d.operation(op.ID).Status
			return s == apitypes.StatusRunning, string(s)
		})
		time.Sleep(time.Second)
		status("hung")
		others("hung, -t running")
		end := d.waitOp(op.ID, 2*time.Minute)
		p := ngxFailed(t, "test with a hung nginx -t", end, problem.DependencyUnavailable)
		took := ngxTook(end)
		t.Logf("hung: test %s %s after %v: %s", end.Status, p.Slug(), took, p.Detail)
		if took < 5*time.Second || took > time.Minute || !strings.Contains(p.Detail, "5s") {
			t.Fatalf("hung: ended after %v with %q, want dependency-unavailable after testTimeout 5s", took, p.Detail)
		}
		others("hung, after the test")
		if out, _ := sh(10*time.Second, "pgrep", "-f", "^sleep 2147483647$"); strings.TrimSpace(out) != "" {
			t.Fatalf("the hung -t survived its operation: %s", out)
		}
	})
}

// --- TC-116 ---------------------------------------------------------------

// nginx-provider TC-116: nginx started through Nexus survives the
// operation's end, outside the operation's cgroup, with a login shell's
// umask (L-41, L-47).
func TestNginxSurvivesOpEnd(t *testing.T) {
	d := newNgxDaemon(t, "tc116", "", "")
	ngxEnsureRunning(t)
	t.Cleanup(func() { ngxEnsureRunning(t) })
	ngxInit(t, "stop")

	// Step 1: the baseline from a login shell.
	mustSh(t, "sh", "-lc", ngxInitCmd("start"))
	base := ngxWaitRunning(t)
	baseUmask := procStatusField(base.master, "Umask")
	t.Logf("baseline (sh -lc '%s'): master %d Umask %s cgroup %q", ngxInitCmd("start"), base.master, baseUmask,
		pidCgroup(t, base.master))
	mustSh(t, "sh", "-lc", ngxInitCmd("stop"))
	if st := ngxInitState(t); st != "stopped" {
		t.Fatalf("after the baseline stop: %s", st)
	}

	// Steps 2 to 4.
	op := d.cliOp(t, "nginx", "start")
	ngxResult[ngxspec.ServiceResult](t, "nginx start", op)
	time.Sleep(5 * time.Second)
	g := ngxObserve(t)
	st := ngxInitState(t)
	if g.master == 0 || !pidAlive(g.master) || st != "running" {
		t.Fatalf("5 s after the op: master %d, %s says %s", g.master, detectedInit(), st)
	}
	cg := pidCgroup(t, g.master)
	umask := procStatusField(g.master, "Umask")
	t.Logf("after nexus nginx start: master %d alive, %s says %s, cgroup %q, Umask %s (baseline %s, nexusd %s)",
		g.master, detectedInit(), st, cg, umask, baseUmask, procStatusField(d.mainPID(), "Umask"))
	if detectedInit() == platform.OpenRC {
		if strings.Contains(cg, "/nexus-ops/") {
			t.Fatalf("L-41: the nginx master %d is in the operation's cgroup %q", g.master, cg)
		}
		if p := filepath.Join("/sys/fs/cgroup/nexus-ops", op.ID); exists(p) {
			t.Fatalf("%s exists after the operation", p)
		}
		if umask == "0077" {
			t.Fatalf("L-47: the nginx master's Umask is 0077: it inherited nexusd's umask through the helper")
		}
	} else {
		if strings.Contains(cg, detach.Unit(op.ID)) {
			t.Fatalf("the nginx master %d is in the operation's unit: %q", g.master, cg)
		}
		if u := showUnit(t, detach.Unit(op.ID), "LoadState")["LoadState"]; u != "not-found" {
			t.Fatalf("%s is %s after the operation", detach.Unit(op.ID), u)
		}
	}
	if umask != baseUmask {
		t.Fatalf("Umask %s after the Nexus start, %s from a login shell", umask, baseUmask)
	}
	if umask == "0000" {
		// nginx's ngx_daemon() calls umask(0) after setsid()
		// (src/os/unix/ngx_daemon.c), so a daemonized master shows 0000
		// whatever umask its starter had: the equality holds, but it cannot
		// tell an inherited 0077 from a login shell's 0022.
		t.Logf("the master's Umask is 0000 both ways: nginx's ngx_daemon() resets it with umask(0)")
	}
}

// --- TC-117 ---------------------------------------------------------------

// nginx-provider TC-117: Real root ownership decides managed. user.conf is
// the TC's enabled file; userd.conf.disabled is the same owner on a
// disabled site, which an enable would otherwise rename.
func TestNginxManagedRequiresRootOwner(t *testing.T) {
	d := newNgxDaemon(t, "tc117", "", "")
	ngxEnsureRunning(t)
	ngxForget(t, "own", "user", "userd")
	nobody, err := strconv.Atoi(strings.TrimSpace(mustSh(t, "id", "-u", "nobody")))
	if err != nil || nobody == 0 {
		t.Fatalf("id -u nobody: %d %v", nobody, err)
	}
	ngxPlace(t, "own.conf", "# nexus TC-117: root-owned\n", 0)
	ngxPlace(t, "user.conf", "# nexus TC-117: owned by nobody\n", nobody)
	ngxPlace(t, "userd.conf.disabled", "# nexus TC-117: owned by nobody, disabled\n", nobody)
	t.Logf("ls -ln:\n%s", ngxLs(t))

	var list apitypes.List[ngxspec.Site]
	d.mustGet(ngxSites+"?limit=1000", &list)
	props := map[string]ngxspec.SiteProperties{}
	for _, s := range list.Items {
		props[s.Name] = s.Properties
	}
	t.Logf("list: own %+v; user %+v; userd %+v", props["own"], props["user"], props["userd"])
	if p := props["own"]; !p.Managed || p.UnmanagedReason != "" {
		t.Fatalf("own: %+v, want managed", p)
	}
	for _, name := range []string{"user", "userd"} {
		if p := props[name]; p.Managed || p.UnmanagedReason != ngxspec.ReasonNotRootOwned {
			t.Fatalf("%s: %+v, want managed false, not-root-owned", name, p)
		}
	}

	before := ngxSnapshot(t)
	opsBefore := opIDs(t, d.nexusdInstance)
	for _, name := range []string{"user", "userd"} {
		code, b := d.post(ngxSiteOp(name, "enable"), "{}")
		var p problem.Error
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatalf("enable %s: %d %s", name, code, b)
		}
		t.Logf("enable %s: %d %s", name, code, b)
		if code != http.StatusUnprocessableEntity || p.Slug() != problem.NginxConfigUnmanaged ||
			p.Extensions["reason"] != ngxspec.ReasonNotRootOwned {
			t.Fatalf("enable %s: %d %s %v, want 422 nginx-config-unmanaged not-root-owned", name, code, p.Slug(), p.Extensions)
		}
	}
	ngxSameSnapshot(t, "refused enables", before)
	if now := opIDs(t, d.nexusdInstance); len(now) != len(opsBefore) {
		t.Fatalf("operations %v after the refusals, %v before", now, opsBefore)
	}
}
