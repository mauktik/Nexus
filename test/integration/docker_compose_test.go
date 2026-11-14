//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
)

// WI 6.5 against the real nested Engine, the harness's real Docker Compose
// plugin (docker-compose-v2) and the registry:2 registries of
// fixtures/registry/setup.sh. Every expected value comes from docker
// inspect, docker events, /proc, systemctl or a registry HEAD in the test,
// never from Nexus. The Compose labels are read with their literal names
// (docker/compose v2.40.3 pkg/api/labels.go).

const (
	composeFixture = "/harness/fixtures/compose/compose.yaml"
	// composeRoot holds the projects: nexusd's sandbox sees it, while
	// PrivateTmp hides the host's /tmp.
	composeRoot   = "/srv/nx-compose"
	holdProxyAddr = "127.0.0.1:5095" // In-test proxy to regOpen whose manifest requests wait until released
	pwnedMarker   = "/tmp/nx-pwned"
	labelProject  = "com.docker.compose.project"
)

// composePluginDirs are where the Docker CLI looks for plugins after its
// config directory (docker/cli cli-plugins/manager/manager_unix.go,
// defaultSystemPluginDirs).
var composePluginDirs = []string{"/usr/local/lib/docker/cli-plugins", "/usr/local/libexec/docker/cli-plugins",
	"/usr/lib/docker/cli-plugins", "/usr/libexec/docker/cli-plugins"}

// holdProxy is a reverse proxy to the open registry whose manifest GETs and
// HEADs by tag wait while it holds, until released, so a pull, or a
// standalone update's digest check, stays in flight as long as a test
// needs. A manifest asked for by digest passes: that is the helpers' check
// that the registry serves the running image (CR-2, CR-91), which comes
// before a Compose update's pull.
type holdProxy struct {
	mu   sync.Mutex
	gate chan struct{} // Nil while open
	held atomic.Int64  // Requests waiting now
}

var (
	holdProxyOnce sync.Once
	theHoldProxy  *holdProxy
	holdProxyErr  error
)

func (p *holdProxy) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gate == nil {
		p.gate = make(chan struct{})
	}
}

func (p *holdProxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gate != nil {
		close(p.gate)
		p.gate = nil
	}
}

func (p *holdProxy) wait(ctx context.Context) {
	p.mu.Lock()
	g := p.gate
	p.mu.Unlock()
	if g == nil {
		return
	}
	p.held.Add(1)
	defer p.held.Add(-1)
	select {
	case <-g:
	case <-ctx.Done():
	}
}

func startHoldProxy(t *testing.T) *holdProxy {
	t.Helper()
	holdProxyOnce.Do(func() {
		target, _ := url.Parse("http://" + regOpen)
		rp := httputil.NewSingleHostReverseProxy(target)
		p := &holdProxy{}
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, ref, isManifest := strings.Cut(r.URL.Path, "/manifests/")
			if isManifest && !strings.HasPrefix(ref, "sha256:") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				p.wait(r.Context())
			}
			rp.ServeHTTP(w, r)
		})}
		ln, err := net.Listen("tcp", holdProxyAddr)
		if err != nil {
			holdProxyErr = err
			return
		}
		go func() { _ = srv.Serve(ln) }()
		theHoldProxy = p
	})
	if holdProxyErr != nil {
		t.Fatal(holdProxyErr)
	}
	theHoldProxy.release()
	t.Cleanup(theHoldProxy.release)
	return theHoldProxy
}

// waitHeld waits until a request is held at the proxy.
func (p *holdProxy) waitHeld(t *testing.T) {
	t.Helper()
	waitUntil(t, 2*time.Minute, "a registry request held at the proxy", func() (bool, string) {
		return p.held.Load() > 0, ""
	})
}

// composeProject is a real Compose project with services app and other,
// started with docker compose up -d and an environment file.
type composeProject struct {
	name, dir, file, envFile string
	host                     string // Registry host of the images: regOpen or the hold proxy
	appRepo, appRef          string
	oldDigest                string // Registry digest of the image app runs at the start
}

func (c *composeProject) container(service string) string { return c.name + "-" + service + "-1" }

// compose runs docker compose on the project with an argument vector.
func (c *composeProject) compose(t *testing.T, args ...string) string {
	t.Helper()
	return dockerCLI(t, slices.Concat([]string{"compose", "--project-directory", c.dir, "-f", c.file,
		"--env-file", c.envFile, "-p", c.name}, args)...)
}

// removeComposeProject removes the project's containers, volumes and
// networks by label, which works when its files are gone too.
func removeComposeProject(name string) {
	out, _ := sh(time.Minute, "docker", "ps", "-aq", "--filter", "label="+labelProject+"="+name)
	for _, id := range strings.Fields(out) {
		_, _ = sh(time.Minute, "docker", "rm", "-f", "-v", id)
	}
	out, _ = sh(time.Minute, "docker", "network", "ls", "-q", "--filter", "label="+labelProject+"="+name)
	for _, id := range strings.Fields(out) {
		_, _ = sh(time.Minute, "docker", "network", "rm", id)
	}
}

// newComposeProject copies the fixture into dir, pushes the images of app
// (from def, as host/nx/<name>-app:moving, also kept as :keep so it keeps
// its RepoDigest once :moving moves) and other, writes the environment
// file and starts the project.
func newComposeProject(t *testing.T, name, dir, host string, def imageDef) *composeProject {
	t.Helper()
	requireDocker(t)
	startRegistries(t)
	removeComposeProject(name)
	t.Cleanup(func() { removeComposeProject(name) })
	for _, d := range []string{dir, dir + ".moved"} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(composeFixture)
	if err != nil {
		t.Fatal(err)
	}
	c := &composeProject{name: name, dir: dir, file: filepath.Join(dir, "compose.yaml"),
		envFile: filepath.Join(dir, "nx.env"), host: host, appRepo: "nx/" + name + "-app"}
	if err := os.WriteFile(c.file, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	c.appRef = host + "/" + c.appRepo + ":moving"
	buildImage(t, c.appRef, name+" app one "+time.Now().String(), def)
	regCmd(t, "push", c.appRef)
	keep := host + "/" + c.appRepo + ":keep"
	dockerCLI(t, "tag", c.appRef, keep)
	regCmd(t, "push", keep)
	otherRef := host + "/nx/" + name + "-other:v1"
	buildImage(t, otherRef, name+" other "+time.Now().String(), updPlain)
	regCmd(t, "push", otherRef)
	env := fmt.Sprintf("NX_APP_IMAGE=%s\nNX_OTHER_IMAGE=%s\n", c.appRef, otherRef)
	if err := os.WriteFile(c.envFile, []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	c.compose(t, "up", "-d")
	c.oldDigest = registryDigest(t, regOpen, c.appRepo, "moving", false)
	labels := dockerInspectField(t, c.container("app"), "{{json .Config.Labels}}")
	t.Logf("project %s in %q: app labels %s", name, dir, labels)
	return c
}

// pushApp pushes a new app image as :moving, removes it locally so an
// update really pulls it, and returns its registry digest. The image app
// runs first gets a pushed tag of its own, so it keeps its RepoDigest once
// :moving moves (as staged does for standalone containers).
func (c *composeProject) pushApp(t *testing.T, def imageDef) string {
	t.Helper()
	keep := fmt.Sprintf("%s/%s:keep-%d", c.host, c.appRepo, time.Now().UnixNano())
	dockerCLI(t, "tag", dockerInspectField(t, c.container("app"), "{{.Image}}"), keep)
	regCmd(t, "push", keep)
	buildImage(t, c.appRef, c.name+" app next "+time.Now().String(), def)
	regCmd(t, "push", c.appRef)
	d := registryDigest(t, regOpen, c.appRepo, "moving", false)
	dockerCLI(t, "image", "rm", c.appRef)
	if d == c.oldDigest {
		t.Fatalf("the push did not change the digest: %s", d)
	}
	return d
}

// procArgv is /proc/<pid>/cmdline split at NUL.
func procArgv(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

func procComm(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSpace(string(b))
}

// unitPIDs are the processes in nexus-op-<id>.service's cgroup, or on
// OpenRC its nexus-ops group, by /proc/<pid>/cgroup.
func unitPIDs(id string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err == nil && inOpCgroup(string(b), id) {
			out = append(out, pid)
		}
	}
	return out
}

// composeProc waits for the docker CLI running `compose ... verb` in the
// operation's unit and returns its PID and argv.
func composeProc(t *testing.T, id, verb string) (int, []string) {
	t.Helper()
	var pid int
	var argv []string
	waitUntil(t, 2*time.Minute, "docker compose "+verb+" in nexus-op-"+id, func() (bool, string) {
		for _, p := range unitPIDs(id) {
			a := procArgv(p)
			if len(a) > 2 && filepath.Base(a[0]) == "docker" && a[1] == "compose" && slices.Contains(a, verb) {
				pid, argv = p, a
				return true, ""
			}
		}
		return false, ""
	})
	return pid, argv
}

// ppid is pid's parent.
func ppid(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
	p, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// startedAt is a container's State.StartedAt, as docker inspect reports it.
func startedAt(t *testing.T, name string) string {
	t.Helper()
	return dockerInspectField(t, name, "{{.State.StartedAt}}")
}

// TC-118 (FR-12, CON-6, A-34, NFR-7): updating app recreates only app, on
// the registry's new digest by independent docker inspect; other keeps its
// ID and StartedAt; the result has both digests, Compose's output is in
// the operation's log, and the unit (KillMode=control-group) ran the
// docker-update helper under op-supervise, which ran docker compose.
func TestDockerComposeUpdateChangesOneService(t *testing.T) {
	hp := startHoldProxy(t)
	c := newComposeProject(t, "nxc118", filepath.Join(composeRoot, "tc118"), holdProxyAddr, updPlain)
	app, other := c.container("app"), c.container("other")
	appID, otherID, otherStarted := containerID(t, app), containerID(t, other), startedAt(t, other)
	newD := c.pushApp(t, updPlain)
	n := upNexusd(t, "tc118", "")

	// Held at the pull just long enough to read the live unit.
	hp.hold()
	op := n.startAction(app, "update", "{}")
	opDir := updOpDir("tc118", op.ID)
	hpid, argv := waitHelper(t, opDir)
	if want := []string{nexusdBin, "docker-update", "--dir", opDir}; !slices.Equal(argv, want) {
		t.Fatalf("helper argv %q, want %q", argv, want)
	}
	cpid, cargv := composeProc(t, op.ID, "pull")
	mainPID := opSupervisor(t, op.ID, opDir, string(detach.KillControlGroup), hpid)
	t.Logf("supervisor %d; helper %d %q; compose %d %q", mainPID, hpid, argv, cpid, cargv)
	sup := ppid(t, hpid)
	if sa := procArgv(sup); sup != mainPID || len(sa) < 2 || sa[1] != "op-supervise" {
		t.Fatalf("helper's parent %d %q, supervisor %d", sup, sa, mainPID)
	}
	if ppid(t, cpid) != hpid {
		t.Fatalf("docker compose pull %d is not the helper's child", cpid)
	}
	hp.release()

	res := wantSucceeded(t, n.waitOp(op.ID, 3*time.Minute))
	newID := containerID(t, app)
	got := repoDigest(t, dockerInspectField(t, app, "{{.Image}}"), c.host+"/"+c.appRepo)
	if !res.Changed || res.RolledBack || res.NewDigest != newD || got != newD || res.PreviousDigest != c.oldDigest {
		t.Fatalf("result %+v; inspect RepoDigest %s, registry %s -> %s", res, got, c.oldDigest, newD)
	}
	if newID == appID || res.ContainerID != newID || res.PreviousContainerID != appID {
		t.Fatalf("result ids %s <- %s, inspect %s <- %s", res.ContainerID, res.PreviousContainerID, newID, appID)
	}
	if containerID(t, other) != otherID || startedAt(t, other) != otherStarted {
		t.Fatalf("other changed: id %s started %s, was %s %s", containerID(t, other), startedAt(t, other), otherID, otherStarted)
	}
	if svc := dockerInspectField(t, app, `{{index .Config.Labels "com.docker.compose.service"}}`); svc != "app" {
		t.Fatalf("the new container's service label %q", svc)
	}
	logs := n.opLogs(op.ID)
	t.Logf("operation output:\n%s", logs)
	if !strings.Contains(logs, app) || !strings.Contains(logs, "docker compose") {
		t.Fatalf("operation output lacks Compose's output")
	}
	if s := unitStarts(t, op.ID); s != 1 {
		t.Fatalf("unit started %d times", s)
	}
}

// TC-E23 (FR-12): a service already on the newest digest succeeds with
// changed false; its container keeps its ID and StartedAt, and Compose
// recreates nothing.
func TestDockerComposeUpdateWhenCurrent(t *testing.T) {
	c := newComposeProject(t, "nxce23", filepath.Join(composeRoot, "tce23"), regOpen, updPlain)
	app := c.container("app")
	id, started := containerID(t, app), startedAt(t, app)
	n := upNexusd(t, "tce23", "")
	since := time.Now()
	op := n.waitOp(n.startAction(app, "update", "{}").ID, 3*time.Minute)
	until := time.Now().Add(time.Second)
	res := wantSucceeded(t, op)
	if res.Changed || res.RolledBack || res.PreviousDigest != c.oldDigest || res.NewDigest != c.oldDigest || res.ContainerID != id {
		t.Fatalf("result %+v, registry digest %s, id %s", res, c.oldDigest, id)
	}
	if containerID(t, app) != id || startedAt(t, app) != started {
		t.Fatalf("app changed: %s %s", containerID(t, app), startedAt(t, app))
	}
	if ev := events(t, since, until, "type=container", "event=create", "event=stop", "event=die", "event=destroy"); len(ev) != 0 {
		t.Fatalf("container events during an up-to-date update: %v", ev)
	}
	logs := n.opLogs(op.ID)
	if strings.Contains(logs, `"up"`) {
		t.Fatalf("docker compose up ran for a current service:\n%s", logs)
	}
	t.Logf("operation output:\n%s", logs)
}

// TC-ERR43 (FR-13): a new app image whose health check fails ends Failed
// update-health-failed with both digests and no rollback: the new
// container stays in place.
func TestDockerComposeHealthFailureNoRollback(t *testing.T) {
	c := newComposeProject(t, "nxcerr43", filepath.Join(composeRoot, "tcerr43"), regOpen, updPlain)
	app := c.container("app")
	id := containerID(t, app)
	newD := c.pushApp(t, imageDef{cmd: updQuick, hc: updHealthFalse})
	n := upNexusd(t, "tcerr43", ", updateHealthTimeout: 8s")
	ext := wantFailed(t, wantOp(t, n, app), problem.UpdateHealthFailed)
	if rb, ok := ext["rolledBack"]; (ok && rb != false) || ext["previousDigest"] != c.oldDigest || ext["newDigest"] != newD {
		t.Fatalf("extensions %v, want rolledBack false or absent and digests %s -> %s", ext, c.oldDigest, newD)
	}
	now := containerID(t, app)
	if now == "" || now == id {
		t.Fatalf("app is %q, was %s: the new container is not in place", now, id)
	}
	if got := repoDigest(t, dockerInspectField(t, app, "{{.Image}}"), c.host+"/"+c.appRepo); got != newD {
		t.Fatalf("app runs %s, want the new %s", got, newD)
	}
	t.Logf("app %s (was %s) health %s", now, id, dockerInspectField(t, app, "{{.State.Health.Status}}"))
}

// composeProcs are the processes whose argv names docker compose.
func composeProcs() []string {
	var out []string
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		a := procArgv(pid)
		if len(a) > 1 && (a[1] == "compose" || strings.HasSuffix(a[0], "docker-compose")) {
			out = append(out, fmt.Sprintf("%d %q", pid, a))
		}
	}
	return out
}

// noUpdateOps requires that no update operation exists.
func noUpdateOps(t *testing.T, n *nexusdInstance) {
	t.Helper()
	var ops apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations?filter=provider%3Ddocker&limit=1000", &ops)
	for _, op := range ops.Items {
		if op.Action == "docker.containers.update" {
			t.Fatalf("an update operation was created: %+v", op)
		}
	}
}

// refused requires status and slug, and the CLI's exit code for the body.
func refused(t *testing.T, code int, b []byte, status int, slug problem.Slug, exit int) problem.Error {
	t.Helper()
	p := lcProblem(t, code, b)
	if code != status || p.Slug() != slug {
		t.Fatalf("%d %s, want %d %s", code, b, status, slug)
	}
	if got := cli.ExitCode(&p); got != exit {
		t.Fatalf("nexus exits %d for %s, want %d", got, b, exit)
	}
	return p
}

// TC-ERR41 (FR-12): with the working directory moved away, with a
// configuration file removed, and on a container whose labels lack
// .project.config_files, the update is 422 compose-files-missing (nexus
// exits 1) before anything happens: no operation, no docker compose
// process, no Engine event; each container keeps running.
func TestDockerComposeFilesMissing(t *testing.T) {
	moved := newComposeProject(t, "nxcerr41a", filepath.Join(composeRoot, "tcerr41a"), regOpen, updPlain)
	gone := newComposeProject(t, "nxcerr41b", filepath.Join(composeRoot, "tcerr41b"), regOpen, updPlain)
	bare := "tcerr41-nolabels"
	requireDocker(t, bare)
	dockerCLI(t, "run", "-d", "--name", bare, "--label", labelProject+"=nxcerr41c", "--label", "com.docker.compose.service=web",
		"--label", "com.docker.compose.project.working_dir="+gone.dir, gone.appRef)
	if err := os.Rename(moved.dir, moved.dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone.file); err != nil {
		t.Fatal(err)
	}
	n := upNexusd(t, "tcerr41", "")
	since := time.Now()
	for _, tc := range []struct{ name, path string }{
		{moved.container("app"), moved.dir}, {gone.container("app"), gone.file}, {bare, ""},
	} {
		code, b := n.act(tc.name, "update", "{}")
		p := refused(t, code, b, http.StatusUnprocessableEntity, problem.ComposeFilesMissing, 1)
		if tc.path != "" && p.Extensions["path"] != tc.path {
			t.Fatalf("%s: path %v, want %s", tc.name, p.Extensions["path"], tc.path)
		}
		if ps := composeProcs(); len(ps) != 0 {
			t.Fatalf("%s: docker compose processes %v", tc.name, ps)
		}
		if !lcInspect(t, tc.name).Running {
			t.Fatalf("%s is not running", tc.name)
		}
		t.Logf("%s: %d %s", tc.name, code, b)
	}
	until := time.Now().Add(time.Second)
	noUpdateOps(t, n)
	if ev := events(t, since, until); len(ev) != 0 {
		t.Fatalf("Engine events during refused updates: %v", ev)
	}
}

// TC-ERR42 (CON-6, NFR-3): with the Compose plugin moved away, a Compose
// update is 503 compose-plugin-missing (nexus exits 4), while a standalone
// update, list and restart of the Compose container succeed, and
// capabilities reports the missing plugin.
func TestDockerComposePluginMissing(t *testing.T) {
	c := newComposeProject(t, "nxcerr42", filepath.Join(composeRoot, "tcerr42"), regOpen, updPlain)
	solo := "tcerr42-solo"
	removeUpdated(t, solo)
	_, _, newD := staged(t, regOpen, "nx/tcerr42", solo, nil, updPlain, updPlain)
	n := upNexusd(t, "tcerr42", "")

	var movedPlugins []string
	for _, d := range composePluginDirs {
		p := filepath.Join(d, "docker-compose")
		if exists(p) {
			if err := os.Rename(p, p+".nx-moved"); err != nil {
				t.Fatal(err)
			}
			movedPlugins = append(movedPlugins, p)
		}
	}
	t.Cleanup(func() {
		for _, p := range movedPlugins {
			if err := os.Rename(p+".nx-moved", p); err != nil {
				t.Errorf("restore %s: %v", p, err)
			}
		}
	})
	if len(movedPlugins) == 0 {
		t.Fatal("no Compose plugin found to move")
	}
	if out, err := sh(time.Minute, "docker", "compose", "version"); err == nil {
		t.Fatalf("docker compose still works after moving %v: %s", movedPlugins, out)
	}
	t.Logf("moved %v", movedPlugins)

	app := c.container("app")
	code, b := n.act(app, "update", "{}")
	refused(t, code, b, http.StatusServiceUnavailable, problem.ComposePluginMissing, 4)
	t.Logf("compose update: %d %s", code, b)

	res := wantSucceeded(t, wantOp(t, n, solo))
	if !res.Changed || res.NewDigest != newD {
		t.Fatalf("standalone update %+v, want %s", res, newD)
	}
	var list apitypes.List[map[string]any]
	n.mustGet(containersAt+"?limit=1000", &list)
	if len(list.Items) == 0 {
		t.Fatal("list returned no containers")
	}
	before := startedAt(t, app)
	n.do(app, "restart", "{}")
	if startedAt(t, app) == before {
		t.Fatal("restart did not restart the Compose container")
	}

	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	for _, p := range caps.Providers {
		if p.Name != "docker" {
			continue
		}
		d, _ := p.Details["composePlugin"].(map[string]any)
		t.Logf("docker capability: %s %v", p.Status, p.Details)
		if p.Status != apitypes.ProviderEnabled || d == nil || d["available"] != false ||
			!strings.Contains(fmt.Sprint(d["reason"]), "docker-compose") {
			t.Fatalf("capabilities do not report the missing plugin: %v", p.Details)
		}
		return
	}
	t.Fatal("no docker provider in capabilities")
}

// TC-ERR44 (FR-7, A-56, G-5): while an update of app is held at the pull,
// a second update in the project, and a stop of app or of its sibling
// other, answer 409 naming the update; a stop of a standalone container
// proceeds.
func TestDockerComposeConflicts(t *testing.T) {
	hp := startHoldProxy(t)
	c := newComposeProject(t, "nxcerr44", filepath.Join(composeRoot, "tcerr44"), holdProxyAddr, updPlain)
	c.pushApp(t, updPlain)
	solo := "tcerr44-solo"
	requireDocker(t, solo)
	dockerCLI(t, "run", "-d", "--name", solo, testImage, "sleep", "3600")
	n := upNexusd(t, "tcerr44", "")
	hp.hold()
	op := n.startAction(c.container("app"), "update", "{}")
	composeProc(t, op.ID, "pull")
	hp.waitHeld(t)

	for _, step := range []struct{ name, action string }{
		{c.container("other"), "update"}, {c.container("app"), "update"},
		{c.container("app"), "stop"}, {c.container("other"), "stop"},
	} {
		code, b := n.act(step.name, step.action, "{}")
		p := lcProblem(t, code, b)
		if code != http.StatusConflict || p.Slug() != problem.Conflict || p.Extensions["blockingOperationId"] != op.ID {
			t.Fatalf("%s %s: %d %s, want 409 naming %s", step.action, step.name, code, b, op.ID)
		}
		t.Logf("%s %s: %d %s", step.action, step.name, code, b)
	}
	if st := n.operation(op.ID).Status; terminal(st) {
		t.Fatalf("the update had ended (%s): the conflicts were not tested", st)
	}
	n.do(solo, "stop", `{"timeoutSeconds": 0}`)
	if lcInspect(t, solo).Running {
		t.Fatal("the standalone control is still running")
	}
	hp.release()
	wantSucceeded(t, n.waitOp(op.ID, 3*time.Minute))
}

// TC-119 (FR-6, FR-12): nexusd killed with SIGKILL while the Compose pull
// is held: the unit keeps its PID across the restart and the update
// completes with the registry's digests. Then, released while nexusd is
// down and restarted only once result.json exists, the operation is
// terminal within 2 s of the restart: Finalize reads files only.
func TestDockerComposeSurvivesNexusdRestart(t *testing.T) {
	hp := startHoldProxy(t)
	c := newComposeProject(t, "nxc119", filepath.Join(composeRoot, "tc119"), holdProxyAddr, updPlain)
	app := c.container("app")
	n := upNexusd(t, "tc119", "")

	run := func(t *testing.T, newD string, releaseWhileDown bool) {
		prevD := repoDigest(t, dockerInspectField(t, app, "{{.Image}}"), c.host+"/"+c.appRepo)
		hp.hold()
		op := n.startAction(app, "update", "{}")
		opDir := updOpDir("tc119", op.ID)
		unit := "nexus-op-" + op.ID + ".service"
		composeProc(t, op.ID, "pull")
		hp.waitHeld(t)
		mainPID := showUnit(t, unit, "MainPID")["MainPID"]
		kill9Nexusd(t, n)
		var restarted time.Time
		if releaseWhileDown {
			hp.release()
			waitUntil(t, 3*time.Minute, "result.json while nexusd is down", func() (bool, string) {
				return exists(filepath.Join(opDir, "result.json")), ""
			})
			lib(t, "stop-nexusd")
			lib(t, "start-nexusd")
			restarted = time.Now()
		} else {
			restarted = restartNexusd(t)
			if got := showUnit(t, unit, "MainPID")["MainPID"]; got != mainPID {
				t.Fatalf("unit MainPID %s after the restart, was %s", got, mainPID)
			}
			hp.release()
		}
		var got apitypes.Operation
		waitUntil(t, updRestartMax, "the update to end after the restart", func() (bool, string) {
			got = n.operation(op.ID)
			return terminal(got.Status), string(got.Status)
		})
		took := time.Since(restarted)
		t.Logf("terminal %s %s after the restart; unit MainPID was %s", got.Status, took.Round(time.Millisecond), mainPID)
		if releaseWhileDown && took > 2*time.Second {
			t.Fatalf("terminal %s after the restart, want within 2 s", took)
		}
		res := wantSucceeded(t, got)
		if !res.Changed || res.NewDigest != newD || res.PreviousDigest != prevD {
			t.Fatalf("result %+v, registry %s -> %s", res, prevD, newD)
		}
		if d := repoDigest(t, dockerInspectField(t, app, "{{.Image}}"), c.host+"/"+c.appRepo); d != newD {
			t.Fatalf("app runs %s, want %s", d, newD)
		}
		if s := unitStarts(t, op.ID); s != 1 {
			t.Fatalf("unit started %d times", s)
		}
	}
	// Not subtests: n reports failures on this test's t.
	t.Log("step 1: restart, then release")
	run(t, c.pushApp(t, updPlain), false)
	t.Log("step 2: release while nexusd is down, restart once result.json exists")
	run(t, c.pushApp(t, updPlain), true)
}

// TC-216 (FR-6, FR-12, A-63): with updateMaxRuntime 5s, a Compose update
// and a standalone update held at the registry each end Failed
// outcome-unknown naming nexus-op-<id>, while the unit stays active and
// the pulling process keeps its PID: nothing is signaled.
func TestDockerUpdateMaxRuntime(t *testing.T) {
	hp := startHoldProxy(t)
	c := newComposeProject(t, "nxc216", filepath.Join(composeRoot, "tc216"), holdProxyAddr, updPlain)
	c.pushApp(t, updPlain)
	solo := "tc216-solo"
	removeUpdated(t, solo)
	staged(t, holdProxyAddr, "nx/tc216", solo, nil, updPlain, updPlain)
	n := upNexusd(t, "tc216", ", updateMaxRuntime: 5s")
	hp.hold()

	type held struct {
		op   apitypes.Operation
		pid  int
		argv []string
	}
	var ops []held
	cop := n.startAction(c.container("app"), "update", "{}")
	pid, argv := composeProc(t, cop.ID, "pull")
	ops = append(ops, held{cop, pid, argv})
	sop := n.startAction(solo, "update", "{}")
	pid, argv = waitHelper(t, updOpDir("tc216", sop.ID))
	ops = append(ops, held{sop, pid, argv})
	t.Cleanup(func() {
		// Let both units finish before their containers are removed.
		hp.release()
		for _, h := range ops {
			unit := "nexus-op-" + h.op.ID + ".service"
			waitUntil(t, 3*time.Minute, unit+" to finish", func() (bool, string) {
				st := activeState(t, unit)
				return st != "active" && st != "deactivating", st
			})
		}
	})

	for _, h := range ops {
		var got apitypes.Operation
		waitUntil(t, 30*time.Second, "outcome-unknown past the maximum runtime", func() (bool, string) {
			got = n.operation(h.op.ID)
			return terminal(got.Status), string(got.Status)
		})
		unit := "nexus-op-" + h.op.ID + ".service"
		wantFailed(t, got, problem.OutcomeUnknown)
		// The detail names the operation's runner: its unit on systemd, its
		// supervisor process on OpenRC's process backend.
		runner := "nexus-op-" + h.op.ID
		if detectedInit() == platform.OpenRC {
			runner = "the supervisor process of operation " + h.op.ID
		}
		if !strings.Contains(got.Error.Detail, runner) {
			t.Fatalf("detail %q does not name %s", got.Error.Detail, runner)
		}
		if st := activeState(t, unit); st != "active" {
			t.Fatalf("%s is %s, want still active", unit, st)
		}
		if !pidAlive(h.pid) || !slices.Equal(procArgv(h.pid), h.argv) {
			t.Fatalf("the pulling process %d %q is gone or changed: %q", h.pid, h.argv, procArgv(h.pid))
		}
		t.Logf("%s: %s after %s; %s active; pulling process %d %q alive", h.op.ID, got.Status,
			got.Ended.Sub(*got.Started).Round(time.Millisecond), unit, h.pid, h.argv)
	}
}

// TC-226 (FR-25, FR-12, CON-6): a Compose project in a directory named
// `nx;touch /tmp/nx-pwned;x y`, and a standalone container, each updated
// while held at the registry: the unit runs `docker-update --dir <D>` as
// separate elements, Compose gets the directory as one argv element, no
// process in the unit is a shell or has a -c argument, both succeed, and
// the marker never appears.
func TestDockerUpdateNoShellString(t *testing.T) {
	hp := startHoldProxy(t)
	_ = os.Remove(pwnedMarker)
	dir := filepath.Join(composeRoot, "nx;touch "+pwnedMarker+";x y")
	c := newComposeProject(t, "nxc226", dir, holdProxyAddr, updPlain)
	c.pushApp(t, updPlain)
	solo := "tc226-solo"
	removeUpdated(t, solo)
	staged(t, holdProxyAddr, "nx/tc226", solo, nil, updPlain, updPlain)
	n := upNexusd(t, "tc226", "")

	check := func(t *testing.T, name string, compose bool) {
		hp.hold()
		op := n.startAction(name, "update", "{}")
		opDir := updOpDir("tc226", op.ID)
		_, argv := waitHelper(t, opDir)
		if want := []string{nexusdBin, "docker-update", "--dir", opDir}; !slices.Equal(argv, want) {
			t.Fatalf("helper argv %q, want %q", argv, want)
		}
		if compose {
			_, cargv := composeProc(t, op.ID, "pull")
			if i := slices.Index(cargv, "--project-directory"); i < 0 || i+1 >= len(cargv) || cargv[i+1] != dir {
				t.Fatalf("docker compose argv %q does not hold %q as one element", cargv, dir)
			}
		}
		hp.waitHeld(t)
		opSupervisor(t, op.ID, opDir, string(detach.KillControlGroup), 0)
		pids := unitPIDs(op.ID)
		if len(pids) < 2 {
			t.Fatalf("unit processes %v", pids)
		}
		for _, p := range pids {
			a, comm := procArgv(p), procComm(p)
			t.Logf("  pid %d comm %s argv %q", p, comm, a)
			if slices.Contains([]string{"sh", "bash", "dash", "zsh", "ash", "busybox"}, comm) || slices.Contains(a, "-c") ||
				(len(a) > 0 && slices.Contains([]string{"sh", "bash", "dash"}, filepath.Base(a[0]))) {
				t.Fatalf("process %d (%s) %q is a shell or has -c", p, comm, a)
			}
		}
		hp.release()
		wantSucceeded(t, n.waitOp(op.ID, 3*time.Minute))
	}
	// Not subtests: n reports failures on this test's t.
	t.Log("compose update")
	check(t, c.container("app"), true)
	t.Log("standalone update")
	check(t, solo, false)
	if exists(pwnedMarker) {
		t.Fatalf("%s exists: a shell ran the directory name", pwnedMarker)
	}
}

// CR-29 (FR-12): a service whose definition was edited after its container
// was created is not updated: docker compose up would deploy the edit with
// the new image. The update ends Failed not-updatable naming the change,
// before anything is pulled or recreated.
func TestDockerComposeUpdateRefusesAnEditedDefinition(t *testing.T) {
	c := newComposeProject(t, "nxcr29", filepath.Join(composeRoot, "tcr29"), regOpen, updPlain)
	app := c.container("app")
	id := containerID(t, app)
	c.pushApp(t, updPlain)
	b, err := os.ReadFile(c.file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "stop_grace_period: 3s", "stop_grace_period: 4s", 1)
	if edited == string(b) {
		t.Fatal("the fixture has no stop_grace_period to edit")
	}
	if err := os.WriteFile(c.file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	n := upNexusd(t, "tcr29", "")
	op := wantOp(t, n, app)
	wantFailed(t, op, problem.NotUpdatable)
	if containerID(t, app) != id {
		t.Fatalf("app was recreated: %s, was %s", containerID(t, app), id)
	}
	if logs := n.opLogs(op.ID); strings.Contains(logs, `"up"`) {
		t.Fatalf("docker compose up ran:\n%s", logs)
	}
}

// SEC-1 (CWE-829): a Compose project that someone other than root owns is
// refused before an operation exists, since root would run docker compose
// on files that user can rewrite.
func TestDockerComposeRefusesAProjectNotOwnedByRoot(t *testing.T) {
	c := newComposeProject(t, "nxsec1", filepath.Join(composeRoot, "tsec1"), regOpen, updPlain)
	app := c.container("app")
	id := containerID(t, app)
	if err := os.Chown(c.file, 12345, 12345); err != nil {
		t.Fatal(err)
	}
	n := upNexusd(t, "tsec1", "")
	code, b := n.act(app, "update", "{}")
	p := refused(t, code, b, http.StatusUnprocessableEntity, problem.ComposeFilesMissing, 1)
	if p.Extensions["path"] != c.file || !strings.Contains(p.Detail, "chown root") {
		t.Fatalf("path %v detail %q, want %s and the chown", p.Extensions["path"], p.Detail, c.file)
	}
	if containerID(t, app) != id || len(composeProcs()) != 0 {
		t.Fatalf("app %s (was %s), compose processes %v", containerID(t, app), id, composeProcs())
	}
	noUpdateOps(t, n)
}
