//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// The real nested Docker Engine of the harness (WI 6.1): every expected
// value comes from the docker CLI in the test, never from Nexus.

const (
	dockerSocket = "/run/docker.sock"
	testImage    = "busybox:latest"
	containersAt = "/v1/providers/docker/containers"
)

// nexusdInstance is the real nexusd run as nexusd.service under the
// shipped hardening, through a drop-in that replaces ExecStart with its own
// config, state and audit directories.
type nexusdInstance struct {
	t      *testing.T
	listen string
	token  string
	client *http.Client
}

func newNexusd(t *testing.T, name, providers string) *nexusdInstance {
	t.Helper()
	installShippedUnit(t)
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
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "harness",
		Scopes: []string{spec.ReadScope, "scripts:read"}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	tmp := t.TempDir()
	cfgSrc := filepath.Join(tmp, "nexusd.yaml")
	cfg := fmt.Sprintf("host: {name: harness}\nlisten: [%q]\ntls: {mode: off}\nproviders:\n%s", n.listen, providers)
	if err := os.WriteFile(cfgSrc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := "/etc/nexus/nexusd-" + name + ".yaml"
	lib(t, "install-config", cfgSrc, cfgPath)
	dropin := fmt.Sprintf("[Service]\nExecStart=\nExecStart=%s serve --config %s --state-dir %s --audit-dir %s\nRestart=no\n",
		nexusdBin, cfgPath, stateDir, auditDir)
	dropinSrc := filepath.Join(tmp, "nexusd.conf")
	if err := os.WriteFile(dropinSrc, []byte(dropin), 0o644); err != nil {
		t.Fatal(err)
	}
	lib(t, "stop-nexusd")
	lib(t, "remove-dropins")
	lib(t, "dropin", "nexusd-"+name, dropinSrc)
	t.Cleanup(func() {
		lib(t, "stop-nexusd")
		lib(t, "remove-dropins")
	})
	lib(t, "start-nexusd")
	if st := activeState(t, nexusdUnit); st != "active" {
		t.Fatalf("nexusd unit %s after start", st)
	}
	return n
}

func (n *nexusdInstance) mainPID() int {
	n.t.Helper()
	pid, err := strconv.Atoi(showUnit(n.t, nexusdUnit, "MainPID")["MainPID"])
	if err != nil {
		n.t.Fatal(err)
	}
	return pid
}

func (n *nexusdInstance) get(path string) (int, []byte) {
	n.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+n.listen+path, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	resp, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		n.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (n *nexusdInstance) mustGet(path string, into any) {
	n.t.Helper()
	code, b := n.get(path)
	if code != http.StatusOK {
		n.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	if err := json.Unmarshal(b, into); err != nil {
		n.t.Fatalf("GET %s: %v\n%s", path, err, b)
	}
}

func (n *nexusdInstance) problem(path string) (int, problem.Slug, problem.Error) {
	n.t.Helper()
	code, b := n.get(path)
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		n.t.Fatalf("GET %s: %d, body is not a problem: %s", path, code, b)
	}
	return code, p.Slug(), p
}

// frames reads a container log stream; it returns the frames and whether
// the stream ended by itself within timeout.
func (n *nexusdInstance) frames(path string, timeout time.Duration, each func(apitypes.Frame)) ([]apitypes.Frame, bool) {
	n.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.listen+path, nil)
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		n.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		b, _ := io.ReadAll(resp.Body)
		n.t.Fatalf("GET %s: %d %s %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	var out []apitypes.Frame
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var f apitypes.Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			n.t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		if bytes.Contains(sc.Bytes(), []byte(`"o":`)) {
			n.t.Fatalf("container log frame carries a byte offset: %s", sc.Text())
		}
		out = append(out, f)
		if each != nil {
			each(f)
		}
	}
	return out, sc.Err() == nil && ctx.Err() == nil
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	return strings.TrimSpace(mustSh(t, "docker", args...))
}

// requireDocker pulls the test image once and removes the named containers
// before and after the test.
var pullOnce sync.Once

func requireDocker(t *testing.T, names ...string) {
	t.Helper()
	var pullErr error
	pullOnce.Do(func() {
		out, err := sh(10*time.Minute, "docker", "pull", testImage)
		if err != nil {
			pullErr = fmt.Errorf("docker pull %s: %v\n%s", testImage, err, out)
		}
	})
	if pullErr != nil {
		t.Fatal(pullErr)
	}
	rm := func() {
		for _, name := range names {
			_, _ = sh(time.Minute, "docker", "rm", "-f", "-v", name)
		}
	}
	rm()
	t.Cleanup(rm)
}

// inspected is the subset of `docker inspect` the test compares.
type inspected struct {
	ID     string `json:"Id"`
	Name   string
	Image  string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status string
		Health *struct{ Status string }
	}
	HostConfig struct {
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
	}
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string
		}
	}
}

func dockerInspect(t *testing.T, name string) inspected {
	t.Helper()
	var out []inspected
	if err := json.Unmarshal([]byte(dockerCLI(t, "inspect", "--type", "container", name)), &out); err != nil || len(out) != 1 {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	return out[0]
}

// expected builds the properties from `docker inspect` and `docker image
// inspect`, independently of the provider's code.
func expected(t *testing.T, name string) spec.ContainerProperties {
	t.Helper()
	in := dockerInspect(t, name)
	var images []struct{ RepoDigests []string }
	if err := json.Unmarshal([]byte(dockerCLI(t, "image", "inspect", in.Image)), &images); err != nil || len(images) != 1 {
		t.Fatalf("docker image inspect %s: %v", in.Image, err)
	}
	// The test image comes from one registry repository: one digest.
	if len(images[0].RepoDigests) != 1 {
		t.Fatalf("image %s RepoDigests %v: want exactly one", in.Image, images[0].RepoDigests)
	}
	_, digest, _ := strings.Cut(images[0].RepoDigests[0], "@")
	p := spec.ContainerProperties{
		ContainerID:    in.ID,
		ImageReference: in.Config.Image,
		ImageDigest:    digest,
		State:          in.State.Status,
		Health:         "none",
		RestartPolicy:  spec.RestartPolicy{Name: in.HostConfig.RestartPolicy.Name, MaximumRetryCount: in.HostConfig.RestartPolicy.MaximumRetryCount},
		Ports:          []spec.Port{},
		ComposeProject: in.Config.Labels["com.docker.compose.project"],
		ComposeService: in.Config.Labels["com.docker.compose.service"],
		// Not from docker inspect: a fresh nexusd has stored no
		// check-updates result (WI 6.3, A-13).
		Update: spec.UpdateCheck{Status: spec.UpdateNotChecked},
	}
	if in.State.Health != nil {
		p.Health = in.State.Health.Status
	}
	for key, bindings := range in.NetworkSettings.Ports {
		num, proto, _ := strings.Cut(key, "/")
		cp, err := strconv.Atoi(num)
		if err != nil {
			t.Fatalf("port %q: %v", key, err)
		}
		if len(bindings) == 0 {
			p.Ports = append(p.Ports, spec.Port{ContainerPort: cp, Protocol: proto})
		}
		for _, b := range bindings {
			hp, err := strconv.Atoi(b.HostPort)
			if err != nil {
				t.Fatalf("host port %q: %v", b.HostPort, err)
			}
			p.Ports = append(p.Ports, spec.Port{ContainerPort: cp, Protocol: proto, HostIP: b.HostIP, HostPort: hp})
		}
	}
	sortPorts(p.Ports)
	return p
}

func sortPorts(ps []spec.Port) {
	slices.SortFunc(ps, func(a, b spec.Port) int {
		return strings.Compare(fmt.Sprintf("%05d/%s/%s/%05d", a.ContainerPort, a.Protocol, a.HostIP, a.HostPort),
			fmt.Sprintf("%05d/%s/%s/%05d", b.ContainerPort, b.Protocol, b.HostIP, b.HostPort))
	})
}

func dockerSection(socket string) string {
	return fmt.Sprintf("  docker: {enabled: true, socket: %s}\n", socket)
}

// TC-107: the inventory, through the real API against the real Engine,
// matches docker inspect for a running, a stopped, a Compose-managed and a
// health-checked container with ports; the client negotiated the daemon's
// API version without a pinned one.
func TestDockerInventoryMatchesInspect(t *testing.T) {
	if _, err := sh(time.Minute, "docker", "compose", "version"); err != nil {
		t.Skip("the Docker Compose v2 plugin is not installed on this base")
	}
	const project = "tc107proj"
	running, stopped, health, composed := "tc107-running", "tc107-stopped", "tc107-health", project+"-app-1"
	requireDocker(t, running, stopped, health, composed)
	dockerCLI(t, "run", "-d", "--name", running, "--restart", "unless-stopped", testImage, "sleep", "3600")
	dockerCLI(t, "run", "--name", stopped, "--restart", "on-failure:3", testImage, "true")
	dockerCLI(t, "run", "-d", "--name", health, "--health-cmd", "true", "--health-interval", "1s",
		"-p", "18080:80", "-p", "127.0.0.1:18443:443/udp", "--expose", "9000", testImage, "httpd", "-f", "-p", "80", "-h", "/tmp")
	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(composeFile, []byte("services:\n  app:\n    image: "+testImage+"\n    command: [sleep, \"3600\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerCLI(t, "compose", "-p", project, "-f", composeFile, "up", "-d")
	t.Cleanup(func() { _, _ = sh(2*time.Minute, "docker", "compose", "-p", project, "-f", composeFile, "down", "-v") })
	waitUntil(t, 60*time.Second, health+" healthy", func() (bool, string) {
		st := dockerCLI(t, "inspect", "-f", "{{.State.Health.Status}}", health)
		return st == "healthy", st
	})

	n := newNexusd(t, "tc107", dockerSection(dockerSocket))

	// Negotiation: the client, built without a version, talks the daemon's
	// version (lower than the client's maximum).
	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities", &caps)
	daemonAPI := dockerCLI(t, "version", "--format", "{{.Server.APIVersion}}")
	for _, p := range caps.Providers {
		if p.Name == spec.Name {
			t.Logf("docker capability: %s %v (daemon API %s)", p.Status, p.Details, daemonAPI)
			if p.Status != apitypes.ProviderEnabled || p.Details["apiVersion"] != daemonAPI {
				t.Fatalf("docker capability %+v, want enabled with negotiated apiVersion %s", p, daemonAPI)
			}
		}
	}

	var list apitypes.List[spec.Container]
	n.mustGet(containersAt+"?limit=1000", &list)
	byName := map[string]spec.Container{}
	for _, c := range list.Items {
		byName[c.Name] = c
	}
	for _, name := range []string{running, stopped, health, composed} {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("%s is not listed; listed: %v", name, slices.Collect(func(yield func(string) bool) {
				for k := range byName {
					if !yield(k) {
						return
					}
				}
			}))
		}
		want := expected(t, name)
		sortPorts(got.Properties.Ports)
		if !equalProps(got.Properties, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got.Properties, want)
		}
		if got.ID != "/providers/docker/containers/"+name || got.Type != "containers" || got.Name != name || got.Host != "harness" {
			t.Errorf("%s FR-1 fields: id %q type %q name %q host %q", name, got.ID, got.Type, got.Name, got.Host)
		}
		var one spec.Container
		n.mustGet(containersAt+"/"+name, &one)
		a, _ := json.Marshal(one)
		b, _ := json.Marshal(got)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: get differs from list:\n get  %s\n list %s", name, a, b)
		}
		t.Logf("%s: %s", name, a)
	}
	if byName[stopped].Properties.State != "exited" || byName[composed].Properties.ComposeService != "app" ||
		byName[health].Properties.Health != "healthy" || len(byName[health].Properties.Ports) == 0 {
		t.Fatalf("the setup does not cover every case: %+v", byName)
	}
}

func equalProps(a, b spec.ContainerProperties) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func waitLogLines(t *testing.T, name string, n int) {
	t.Helper()
	waitUntil(t, 60*time.Second, fmt.Sprintf("%d log lines of %s", n, name), func() (bool, string) {
		out, _ := sh(30*time.Second, "docker", "logs", name)
		got := strings.Count(out, "\n")
		return got >= n, strconv.Itoa(got)
	})
}

func lines(fs []apitypes.Frame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.S + ":" + f.D
	}
	return out
}

// TC-108: tail, since and follow against the real Engine, with stdout and
// stderr demuxed for a non-TTY container and a TTY container's output kept
// intact.
func TestDockerContainerLogs(t *testing.T) {
	plain, follow, tty := "tc108-plain", "tc108-follow", "tc108-tty"
	requireDocker(t, plain, follow, tty)
	op := spec.Logs.Op()
	if !op.Untrusted || op.Kind != def.KindSubresource || op.Mode != def.ModeStream {
		t.Fatalf("logs op: untrusted %v kind %s mode %s", op.Untrusted, op.Kind, op.Mode)
	}
	bound := false
	for _, b := range docker.New(docker.Options{}).Bindings() {
		if b.Op().ID == op.ID {
			bound = b.Kind() == provider.BindStream
		}
	}
	if !bound {
		t.Fatal("logs is not bound with provider.Stream")
	}

	// Odd lines on stdout, even lines on stderr, 100 ms apart.
	script := `for i in 1 2 3 4 5 6 7 8 9 10; do if [ $((i % 2)) = 1 ]; then echo line$i; else echo line$i >&2; fi; sleep 0.1; done; sleep 3600`
	dockerCLI(t, "run", "-d", "--name", plain, testImage, "sh", "-c", script)
	dockerCLI(t, "run", "-d", "-t", "--name", tty, testImage, "sh", "-c", "echo tty1; echo tty2; sleep 3600")
	waitLogLines(t, plain, 10)
	waitLogLines(t, tty, 2)
	n := newNexusd(t, "tc108", dockerSection(dockerSocket))

	want := func(i int) string {
		if i%2 == 1 {
			return fmt.Sprintf("out:line%d\n", i)
		}
		return fmt.Sprintf("err:line%d\n", i)
	}
	all, ended := n.frames(containersAt+"/"+plain+"/logs", 30*time.Second, nil)
	if !ended || len(all) != 10 {
		t.Fatalf("all lines: ended %v, %q", ended, lines(all))
	}
	for i, f := range all {
		if got := f.S + ":" + f.D; got != want(i+1) {
			t.Fatalf("frame %d = %q, want %q", i, got, want(i+1))
		}
	}

	tail, _ := n.frames(containersAt+"/"+plain+"/logs?tail=5", 30*time.Second, nil)
	if got := lines(tail); !slices.Equal(got, []string{want(6), want(7), want(8), want(9), want(10)}) {
		t.Fatalf("tail=5: %q", got)
	}

	since := all[5].T.Format(time.RFC3339Nano)
	sinced, _ := n.frames(containersAt+"/"+plain+"/logs?since="+since, 30*time.Second, nil)
	if got := lines(sinced); len(got) == 0 || got[0] != want(6) || slices.ContainsFunc(got, func(s string) bool {
		return slices.Contains([]string{want(1), want(2), want(3), want(4), want(5)}, s)
	}) {
		t.Fatalf("since=%s (line6's time): %q", since, got)
	}

	ttyFrames, _ := n.frames(containersAt+"/"+tty+"/logs", 30*time.Second, nil)
	if got := lines(ttyFrames); !slices.Equal(got, []string{"out:tty1\r\n", "out:tty2\r\n"}) {
		t.Fatalf("TTY frames: %q", got)
	}

	// Follow: a container writing out and err lines forever; the stream
	// carries both tags and ends when the container is stopped.
	dockerCLI(t, "run", "-d", "--name", follow, testImage, "sh", "-c",
		`i=0; while true; do i=$((i+1)); echo out$i; echo err$i >&2; sleep 0.2; done`)
	got := make(chan apitypes.Frame, 1024)
	done := make(chan struct {
		frames []apitypes.Frame
		ended  bool
	}, 1)
	go func() {
		fs, ended := n.frames(containersAt+"/"+follow+"/logs?follow=true", 2*time.Minute, func(f apitypes.Frame) {
			select {
			case got <- f:
			default:
			}
		})
		done <- struct {
			frames []apitypes.Frame
			ended  bool
		}{fs, ended}
	}()
	seen := map[string]bool{}
	deadline := time.After(30 * time.Second)
	for !(seen["out"] && seen["err"]) || len(seen) < 2 {
		select {
		case f := <-got:
			seen[f.S] = true
		case <-deadline:
			t.Fatalf("follow: streams seen %v", seen)
		}
	}
	stopAt := time.Now()
	dockerCLI(t, "stop", "-t", "1", follow)
	var res struct {
		frames []apitypes.Frame
		ended  bool
	}
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("follow did not end within 60s of the container stopping")
	}
	if !res.ended {
		t.Fatal("follow stream was cut, not ended by the container stopping")
	}
	t.Logf("follow: %d frames, ended %v after the stop", len(res.frames), time.Since(stopAt).Round(time.Millisecond))
	for _, f := range res.frames {
		if (strings.HasPrefix(f.D, "out") && f.S != "out") || (strings.HasPrefix(f.D, "err") && f.S != "err") || f.T.IsZero() {
			t.Fatalf("follow frame mis-tagged: %+v", f)
		}
	}
}

// dockerProxy forwards a Unix socket to the Engine's and records every byte
// a client sends, so the test sees which calls reached the daemon.
type dockerProxy struct {
	path string
	mu   sync.Mutex
	sent bytes.Buffer
}

func newDockerProxy(t *testing.T) *dockerProxy {
	t.Helper()
	p := &dockerProxy{path: "/run/nexus-tc-err30.sock"}
	_ = os.Remove(p.path)
	ln, err := net.Listen("unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close(); _ = os.Remove(p.path) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.forward(c)
		}
	}()
	return p
}

func (p *dockerProxy) forward(c net.Conn) {
	defer c.Close()
	d, err := net.Dial("unix", dockerSocket)
	if err != nil {
		return
	}
	defer d.Close()
	go func() { _, _ = io.Copy(c, d) }()
	_, _ = io.Copy(d, io.TeeReader(c, p))
}

func (p *dockerProxy) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent.Write(b)
}

func (p *dockerProxy) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent.String()
}

// TC-ERR30: an unknown container is 404 not-found from the real daemon;
// invalid names are 400 validation-failed and never reach the daemon.
func TestDockerNameValidationAndNotFound(t *testing.T) {
	requireDocker(t)
	proxy := newDockerProxy(t)
	n := newNexusd(t, "tcerr30", dockerSection(proxy.path))

	if code, slug, _ := n.problem(containersAt + "/nope"); code != 404 || slug != problem.NotFound {
		t.Fatalf("unknown container: %d %s", code, slug)
	}
	if !strings.Contains(proxy.text(), "/containers/nope/json") {
		t.Fatalf("the not-found did not come from the daemon; sent:\n%s", proxy.text())
	}
	before := len(proxy.text())
	for _, name := range []string{"..%2Fx", "-x", "a%20b"} {
		for _, path := range []string{containersAt + "/" + name, containersAt + "/" + name + "/logs"} {
			code, slug, p := n.problem(path)
			if code != 400 || slug != problem.ValidationFailed {
				t.Fatalf("GET %s: %d %s %+v", path, code, slug, p)
			}
		}
	}
	if after := proxy.text()[before:]; after != "" {
		t.Fatalf("invalid names reached the daemon:\n%s", after)
	}
}

// dockerEngine stops or starts the nested Engine: docker.service and
// docker.socket under systemd, the docker service under OpenRC.
func dockerEngine(verb string) error {
	var out string
	var err error
	if detectedInit() == platform.OpenRC {
		out, err = sh(2*time.Minute, "rc-service", "docker", verb)
	} else if verb == "stop" {
		out, err = sh(2*time.Minute, "systemctl", "stop", "docker.service", "docker.socket")
	} else {
		out, err = sh(2*time.Minute, "systemctl", "start", "docker.socket", "docker.service")
	}
	if err != nil {
		return fmt.Errorf("%s the Engine: %v\n%s", verb, err, out)
	}
	return nil
}

// TC-ERR31 (second-state): with the real Engine stopped, container calls
// fail with a 503 while scripts keep answering and capabilities show the
// reason; once the Engine is back, the same nexusd process serves
// containers again.
func TestDockerOutageIsIsolatedAndSelfHealing(t *testing.T) {
	requireDocker(t, "tc-err31")
	dockerCLI(t, "run", "-d", "--name", "tc-err31", testImage, "sleep", "3600")
	n := newNexusd(t, "tcerr31", dockerSection(dockerSocket)+"  scripts: {enabled: true}\n")
	pid := n.mainPID()
	t.Cleanup(func() {
		_ = dockerEngine("start")
	})

	var list apitypes.List[spec.Container]
	n.mustGet(containersAt, &list)

	// Step 1.
	if err := dockerEngine("stop"); err != nil {
		t.Fatal(err)
	}
	code, slug, p := n.problem(containersAt)
	t.Logf("container list with Docker stopped: %d %s %v", code, slug, p.Extensions)
	if code != 503 || (slug != problem.DependencyUnavailable && slug != problem.CapabilityUnavailable) ||
		p.Extensions["provider"] != spec.Name {
		t.Fatalf("container list with Docker stopped: %d %s %+v", code, slug, p)
	}
	var scripts apitypes.List[json.RawMessage]
	n.mustGet("/v1/providers/scripts/scripts", &scripts)
	var caps apitypes.Capabilities
	n.mustGet("/v1/capabilities?refresh=true", &caps)
	for _, c := range caps.Providers {
		switch c.Name {
		case spec.Name:
			t.Logf("docker capability with Docker stopped: %s: %s", c.Status, c.Reason)
			if c.Status != apitypes.ProviderUnavailable || !strings.Contains(c.Reason, dockerSocket) {
				t.Fatalf("docker capability: %+v", c)
			}
		case "scripts":
			if c.Status != apitypes.ProviderEnabled {
				t.Fatalf("scripts affected by the Docker outage: %+v", c)
			}
		}
	}
	if code, slug, _ := n.problem(containersAt); code != 503 || slug != problem.CapabilityUnavailable {
		t.Fatalf("after the refresh: %d %s, want capability-unavailable", code, slug)
	}

	// Step 2.
	if err := dockerEngine("start"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Minute, "docker info", func() (bool, string) {
		out, err := sh(30*time.Second, "docker", "info", "--format", "{{.ServerVersion}}")
		return err == nil, out
	})
	waitUntil(t, 60*time.Second, "container list to succeed again", func() (bool, string) {
		code, b := n.get(containersAt)
		return code == http.StatusOK, fmt.Sprintf("%d %s", code, b)
	})
	n.mustGet("/v1/capabilities", &caps)
	for _, c := range caps.Providers {
		if c.Name == spec.Name && c.Status != apitypes.ProviderEnabled {
			t.Fatalf("docker after recovery: %+v", c)
		}
	}
	n.mustGet("/v1/providers/scripts/scripts", &scripts)
	if got := n.mainPID(); got != pid || got == 0 {
		t.Fatalf("nexusd MainPID %d before, %d after: restarted", pid, got)
	}
	t.Logf("recovered with the same nexusd PID %d", pid)
}
