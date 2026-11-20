// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// WI 6.4 against the real nested Engine, the real registry:2 registries of
// fixtures/registry/setup.sh and real nexus-op-<id> units. Every expected
// value comes from docker inspect, docker events, /proc or a registry HEAD
// in the test, never from Nexus. Test images are committed through the
// Engine API's POST /commit (Engine API v1.52: "containerConfig: The
// container configuration"), which, unlike docker commit --change, can set
// a HEALTHCHECK.

const (
	updBlobProxy  = "127.0.0.1:5097" // In-test proxy to regOpen whose blob GETs can fail (TC-ERR38)
	updOldMarker  = "-nexus-old-"
	updRestartMax = 60 * time.Second // FR-6: terminal within 60 s of a restart
)

// updQuick stops at once on SIGTERM, so stops in these tests take no
// stop timeout.
var updQuick = []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 0.2; done"}

// updHealthyAfter becomes healthy after secs seconds under updHealthFile.
func updHealthyAfter(secs int) imageDef {
	return imageDef{
		cmd: []string{"sh", "-c", fmt.Sprintf("trap 'exit 0' TERM; i=0; while [ $i -lt %d ]; do sleep 1; i=$((i+1)); done; "+
			"touch /tmp/ok; while :; do sleep 0.2; done", secs)},
		hc: updHealthFile,
	}
}

var (
	updHealthFile = map[string]any{"Test": []string{"CMD-SHELL", "test -f /tmp/ok"},
		"Interval": int64(time.Second), "Timeout": int64(time.Second), "Retries": 1}
	updHealthFalse = map[string]any{"Test": []string{"CMD", "false"},
		"Interval": int64(time.Second), "Timeout": int64(time.Second), "Retries": 1}
)

// imageDef is what a committed test image runs: its CMD and HEALTHCHECK.
type imageDef struct {
	cmd []string
	hc  map[string]any
}

var updPlain = imageDef{cmd: updQuick}

// updResult decodes docker.containers.update's result independently of
// the provider's types.
type updResult struct {
	Changed             bool     `json:"changed"`
	Warnings            []string `json:"warnings"`
	ContainerID         string   `json:"containerId"`
	PreviousContainerID string   `json:"previousContainerId"`
	PreviousDigest      string   `json:"previousDigest"`
	NewDigest           string   `json:"newDigest"`
	RolledBack          bool     `json:"rolledBack"`
}

// upNexusd is a real nexusd with containers:write whose docker section has
// an empty configDir plus extra settings (", key: value" pairs).
func upNexusd(t *testing.T, name, extra string) *nexusdInstance {
	t.Helper()
	dir := configDir(t, name, "")
	n := newNexusd(t, name, fmt.Sprintf("  docker: {enabled: true, socket: %s, configDir: %s%s}\n", dockerSocket, dir, extra))
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "update",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	return n
}

func updOpDir(nexusdName, id string) string {
	return filepath.Join(nexusdState, "nexusd-"+nexusdName, "ops", id)
}

// engineHTTP talks to the nested Engine's socket directly.
func engineHTTP() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", dockerSocket)
		}}}
}

// buildImage commits ref from busybox with /nx-content holding content and
// def's CMD and HEALTHCHECK. Each content gives a new image and digest.
func buildImage(t *testing.T, ref, content string, def imageDef) {
	t.Helper()
	tmp := fmt.Sprintf("nx-upd-build-%d", time.Now().UnixNano())
	dockerCLI(t, "run", "--name", tmp, testImage, "sh", "-c", `printf '%s\n' "$1" >/nx-content`, "sh", content)
	defer func() { _, _ = sh(time.Minute, "docker", "rm", "-f", tmp) }()
	cfg := map[string]any{"Cmd": def.cmd}
	if def.hc != nil {
		cfg["Healthcheck"] = def.hc
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndex(ref, ":")
	q := url.Values{"container": {tmp}, "repo": {ref[:i]}, "tag": {ref[i+1:]}}
	resp, err := engineHTTP().Post("http://docker/commit?"+q.Encode(), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /commit %s: %s %s", ref, resp.Status, b)
	}
}

// staged pushes v1 as host/repo:moving and :keep, runs name from :moving
// with args, then pushes v2 as :moving and removes it locally. It returns the reference and the registry's
// digests of v1 and v2.
func staged(t *testing.T, host, repo, name string, args []string, v1, v2 imageDef) (ref, oldD, newD string) {
	t.Helper()
	ref = host + "/" + repo + ":moving"
	buildImage(t, ref, repo+" one "+time.Now().String(), v1)
	regCmd(t, "push", ref)
	oldD = registryDigest(t, host, repo, "moving", false)
	// v1 keeps a tag of the same repository, pushed, so it keeps its
	// RepoDigest once v2 takes :moving (as on a host whose tag moves only
	// by the update's pull).
	keep := host + "/" + repo + ":keep"
	dockerCLI(t, "tag", ref, keep)
	regCmd(t, "push", keep)
	dockerCLI(t, slices.Concat([]string{"run", "-d", "--name", name}, args, []string{ref})...)
	buildImage(t, ref, repo+" two "+time.Now().String(), v2)
	regCmd(t, "push", ref)
	newD = registryDigest(t, host, repo, "moving", false)
	// v2 exists only in the registry, so the update really pulls it.
	dockerCLI(t, "image", "rm", ref)
	if oldD == newD {
		t.Fatalf("the second push did not change the digest: %s", newD)
	}
	return ref, oldD, newD
}

// removeUpdated removes name and every <name>-nexus-old-* container, before
// and after the test.
func removeUpdated(t *testing.T, names ...string) {
	t.Helper()
	requireDocker(t)
	rm := func() {
		out, _ := sh(time.Minute, "docker", "ps", "-a", "--format", "{{.Names}}")
		for _, c := range strings.Fields(out) {
			for _, n := range names {
				if c == n || strings.HasPrefix(c, n+updOldMarker) {
					_, _ = sh(time.Minute, "docker", "rm", "-f", "-v", c)
				}
			}
		}
	}
	rm()
	t.Cleanup(rm)
}

// oldContainers are the <name>-nexus-old-* containers that exist.
func oldContainers(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, c := range strings.Fields(dockerCLI(t, "ps", "-a", "--format", "{{.Names}}")) {
		if strings.HasPrefix(c, name+updOldMarker) {
			out = append(out, c)
		}
	}
	return out
}

// containerID is name's full ID, or "" when there is none.
func containerID(t *testing.T, name string) string {
	t.Helper()
	out, err := sh(time.Minute, "docker", "inspect", "--type", "container", "-f", "{{.Id}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// events lists the Engine's container events between since and until as
// "action name id" lines.
func events(t *testing.T, since, until time.Time, filters ...string) []string {
	t.Helper()
	args := []string{"events", "--since", since.Format(time.RFC3339Nano), "--until", until.Format(time.RFC3339Nano),
		"--format", `{{.Type}} {{.Action}} {{index .Actor.Attributes "name"}} {{.Actor.ID}}`}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out := dockerCLI(t, args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// waitEvent waits for a container event matching filters since a time.
func waitEvent(t *testing.T, since time.Time, timeout time.Duration, filters ...string) []string {
	t.Helper()
	var got []string
	waitUntil(t, timeout, "docker events "+strings.Join(filters, " "), func() (bool, string) {
		got = events(t, since, time.Now(), filters...)
		return len(got) > 0, ""
	})
	return got
}

// helperProc finds the docker-update helper of opDir by its argv in /proc.
func helperProc(opDir string) (pid int, argv []string) {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p))
		if err != nil || len(b) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(args) >= 2 && args[1] == "docker-update" && slices.Contains(args, opDir) {
			return p, args
		}
	}
	return 0, nil
}

func waitHelper(t *testing.T, opDir string) (int, []string) {
	t.Helper()
	var pid int
	var argv []string
	waitUntil(t, 60*time.Second, "the docker-update helper of "+opDir, func() (bool, string) {
		pid, argv = helperProc(opDir)
		return pid > 0, ""
	})
	return pid, argv
}

// unitStarts counts the journal's start lines for an operation unit: a
// unit that ran once has one. On OpenRC it counts the process backend's
// launch records: one supervisor.json, written once by the launcher, with
// spec.json consumed (a second supervisor of the operation finds no spec
// and runs nothing: internal/detach TestMissingSpecPreventsAnyRerun).
func unitStarts(t *testing.T, id string) int {
	t.Helper()
	if detectedInit() == platform.OpenRC {
		dir := opDirOf(id)
		if dir == "" || !exists(filepath.Join(dir, detach.IdentityFile)) || exists(filepath.Join(dir, detach.SpecFile)) {
			t.Logf("%s: op dir %q, supervisor.json %v, spec.json %v", id, dir,
				exists(filepath.Join(dir, detach.IdentityFile)), exists(filepath.Join(dir, detach.SpecFile)))
			return 0
		}
		return 1
	}
	out := mustSh(t, "journalctl", "--no-pager", "-o", "cat", "-u", "nexus-op-"+id+".service")
	return strings.Count(out, "Started nexus-op-"+id+".service")
}

func updResultOf(t *testing.T, op apitypes.Operation) updResult {
	t.Helper()
	var r updResult
	if err := json.Unmarshal(op.Result, &r); err != nil {
		t.Fatalf("result %s: %v", op.Result, err)
	}
	return r
}

func wantSucceeded(t *testing.T, op apitypes.Operation) updResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("update %s: %s %+v", op.ID, op.Status, op.Error)
	}
	r := updResultOf(t, op)
	t.Logf("update %s: %s in %s, result %s", op.ID, op.Status, op.Ended.Sub(*op.Started).Round(time.Millisecond), op.Result)
	return r
}

// wantFailed requires Failed with slug and returns the error's extensions.
func wantFailed(t *testing.T, op apitypes.Operation, slug problem.Slug) map[string]any {
	t.Helper()
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != slug {
		t.Fatalf("update %s: %s %+v, want Failed %s", op.ID, op.Status, op.Error, slug)
	}
	t.Logf("update %s: Failed %s: %s %v", op.ID, slug, op.Error.Detail, op.Error.Extensions)
	if op.Error.Extensions == nil {
		return map[string]any{}
	}
	return op.Error.Extensions
}

// repoDigest is image's RepoDigests digest for repository repo (host/path).
func repoDigest(t *testing.T, image, repo string) string {
	t.Helper()
	var imgs []struct{ RepoDigests []string }
	if err := json.Unmarshal([]byte(dockerCLI(t, "image", "inspect", image)), &imgs); err != nil || len(imgs) != 1 {
		t.Fatalf("docker image inspect %s: %v", image, err)
	}
	for _, rd := range imgs[0].RepoDigests {
		if r, d, ok := strings.Cut(rd, "@"); ok && r == repo {
			return d
		}
	}
	t.Fatalf("image %s has no RepoDigest for %s: %v", image, repo, imgs[0].RepoDigests)
	return ""
}

// inspectRaw is docker inspect of a container as generic JSON.
func inspectRaw(t *testing.T, name string) map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(dockerCLI(t, "inspect", "--type", "container", name)), &out); err != nil || len(out) != 1 {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	return out[0]
}

// flatten maps every leaf of v to its dotted path. Mounts are sorted by
// destination, and each network's aliases lose the container's own short
// ID, which an older Engine adds by itself.
func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, e, out)
		}
	case []any:
		if len(x) == 0 {
			out[prefix] = "[]"
		}
		for i, e := range x {
			flatten(prefix+"."+strconv.Itoa(i), e, out)
		}
	default:
		b, _ := json.Marshal(x)
		out[prefix] = string(b)
	}
}

// normalized is c flattened. Each mount at a destination in remounted
// loses Mode: the Engine reports "" for an anonymous volume and "z" for
// the same volume mounted by name (observed on Docker 29.2.1), and naming
// it is the only way to attach it to the new container.
func normalized(c map[string]any, remounted []string) map[string]string {
	if ms, ok := c["Mounts"].([]any); ok {
		sort.Slice(ms, func(i, j int) bool {
			return fmt.Sprint(ms[i].(map[string]any)["Destination"]) < fmt.Sprint(ms[j].(map[string]any)["Destination"])
		})
		for _, m := range ms {
			mm := m.(map[string]any)
			if slices.Contains(remounted, fmt.Sprint(mm["Destination"])) {
				delete(mm, "Mode")
			}
		}
	}
	short := c["Id"].(string)[:12]
	if ns, ok := c["NetworkSettings"].(map[string]any); ok {
		if nets, ok := ns["Networks"].(map[string]any); ok {
			for _, n := range nets {
				ep := n.(map[string]any)
				if al, ok := ep["Aliases"].([]any); ok {
					ep["Aliases"] = slices.DeleteFunc(al, func(a any) bool { return a == short })
				}
			}
		}
	}
	out := map[string]string{}
	flatten("", c, out)
	return out
}

// updAllowed are the runtime-generated paths an update may change (TC-116,
// A-35): "*" is one segment, and a pattern also covers everything below it.
var updAllowed = []string{
	"Id", "Created", "State", "Image", "ResolvConfPath", "HostnamePath", "HostsPath", "LogPath", "RestartCount",
	"GraphDriver", "Storage", "SizeRw", "SizeRootFs", "ExecIDs", "ImageManifestDescriptor", "Config.Image",
	"NetworkSettings.SandboxID", "NetworkSettings.SandboxKey",
	"NetworkSettings.Networks.*.EndpointID", "NetworkSettings.Networks.*.IPAddress",
	"NetworkSettings.Networks.*.MacAddress", "NetworkSettings.Networks.*.DNSNames",
	"Mounts.*.Driver",
}

func allowed(path string) bool {
	segs := strings.Split(path, ".")
	for _, p := range updAllowed {
		ps := strings.Split(p, ".")
		if len(ps) > len(segs) {
			continue
		}
		ok := true
		for i, s := range ps {
			if s != "*" && s != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// hostMounts is HostConfig.Mounts as "Type Source Target" lines.
func hostMounts(c map[string]any) []string {
	hc, _ := c["HostConfig"].(map[string]any)
	ms, _ := hc["Mounts"].([]any)
	var out []string
	for _, m := range ms {
		mm := m.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v", mm["Type"], mm["Source"], mm["Target"]))
	}
	return out
}

// anonymousVolumes are the volume mounts of c that no HostConfig entry
// names: Docker's anonymous volumes, as "volume <name> <destination>".
func anonymousVolumes(c map[string]any) []string {
	hc, _ := c["HostConfig"].(map[string]any)
	binds := fmt.Sprint(hc["Binds"])
	var out []string
	for _, m := range c["Mounts"].([]any) {
		mm := m.(map[string]any)
		name := fmt.Sprint(mm["Name"])
		if mm["Type"] == "volume" && !strings.Contains(binds, name+":") &&
			!slices.ContainsFunc(hostMounts(c), func(s string) bool { return strings.Contains(s, " "+name+" ") }) {
			out = append(out, fmt.Sprintf("volume %s %v", name, mm["Destination"]))
		}
	}
	return out
}

// TC-116, TC-117 (FR-12, NFR-7, A-35): a standalone container with env,
// bind, named and anonymous mounts, a published port, two networks with
// aliases and a static IP, a restart policy, labels, CPU and memory limits
// and a custom hostname is updated by the docker-update helper in its
// nexus-op-<id> unit; docker inspect differs only in the allow-listed
// runtime paths; the anonymous volume keeps its name and its data; the
// result's digests equal the registry's and docker inspect's.
func TestDockerUpdateStandaloneAcceptance(t *testing.T) {
	startRegistries(t)
	name := "tc116-web"
	removeUpdated(t, name)
	for _, nw := range []string{"nx-tc116-a", "nx-tc116-b"} {
		_, _ = sh(time.Minute, "docker", "network", "rm", nw)
		t.Cleanup(func() { _, _ = sh(time.Minute, "docker", "network", "rm", nw) })
	}
	dockerCLI(t, "network", "create", "--subnet", "172.30.116.0/24", "nx-tc116-a")
	dockerCLI(t, "network", "create", "nx-tc116-b")
	_, _ = sh(time.Minute, "docker", "volume", "rm", "nx-tc116-named")
	t.Cleanup(func() { _, _ = sh(time.Minute, "docker", "volume", "rm", "nx-tc116-named") })
	bind := "/var/lib/nx-tc116-bind"
	if err := os.MkdirAll(bind, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bind) })

	args := []string{"--hostname", "tc116-host", "-e", "NX_FOO=bar", "-e", "NX_EMPTY=",
		"-v", bind + ":/bind:ro", "-v", "nx-tc116-named:/named", "-v", "/anon",
		"-p", "127.0.0.1:18116:8080", "--network", "nx-tc116-a", "--ip", "172.30.116.10", "--network-alias", "web-a",
		"--restart", "unless-stopped", "--label", "nx.test=tc116", "--label", "nx.role=web",
		"--cpus", "0.5", "--memory", "64m"}
	ref, oldD, newD := staged(t, regOpen, "nx/tc116", name, args, updPlain, updPlain)
	dockerCLI(t, "network", "connect", "--alias", "web-b", "nx-tc116-b", name)
	marker := fmt.Sprintf("marker-%d", time.Now().UnixNano())
	dockerCLI(t, "exec", name, "sh", "-c", "echo "+marker+" >/anon/marker")
	before := inspectRaw(t, name)
	prev := repoDigest(t, before["Image"].(string), regOpen+"/nx/tc116")
	if prev != oldD {
		t.Fatalf("pre-update RepoDigest %s, registry digest of v1 %s", prev, oldD)
	}
	anon := anonymousVolumes(before)
	if len(anon) != 1 {
		t.Fatalf("anonymous volumes before: %v", anon)
	}

	n := upNexusd(t, "tc116", "")
	op := n.startAction(name, "update", "{}")
	opDir := updOpDir("tc116", op.ID)
	pid, argv := waitHelper(t, opDir)
	want := []string{nexusdBin, "docker-update", "--dir", opDir}
	if !slices.Equal(argv, want) {
		t.Fatalf("helper argv %q, want %q", argv, want)
	}
	if cg := pidCgroup(t, pid); !inOpCgroup(cg, op.ID) {
		t.Fatalf("helper %d runs in cgroup %s, not operation %s's", pid, cg, op.ID)
	}
	opSupervisor(t, op.ID, opDir, string(detach.KillControlGroup), pid)
	t.Logf("helper pid %d argv %q in %s", pid, argv, pidCgroup(t, pid))
	res := wantSucceeded(t, n.waitOp(op.ID, 3*time.Minute))
	after := inspectRaw(t, name)

	// TC-117: the result, docker inspect and the registry agree.
	got := repoDigest(t, after["Image"].(string), regOpen+"/nx/tc116")
	if !res.Changed || res.RolledBack || res.NewDigest != newD || got != newD || res.PreviousDigest != prev {
		t.Fatalf("result %+v; inspect RepoDigest %s, registry %s, previous %s", res, got, newD, prev)
	}
	if res.ContainerID != after["Id"] || res.PreviousContainerID != before["Id"] || after["Id"] == before["Id"] {
		t.Fatalf("result ids %s <- %s, inspect %v <- %v", res.ContainerID, res.PreviousContainerID, after["Id"], before["Id"])
	}
	if ref != after["Config"].(map[string]any)["Image"] {
		t.Fatalf("Config.Image %v, want %s", after["Config"].(map[string]any)["Image"], ref)
	}

	// The full inspect diff: only allow-listed runtime paths differ.
	// HostConfig.Mounts may gain exactly the remount of each anonymous
	// volume by its name, which is how the Engine API attaches an existing
	// volume (POST /containers/create, HostConfig.Mounts); nothing else in
	// HostConfig may change.
	bm, am := hostMounts(before), hostMounts(after)
	var added []string
	for _, m := range am {
		if !slices.Contains(bm, m) {
			added = append(added, m)
		}
	}
	for _, m := range bm {
		if !slices.Contains(am, m) {
			t.Errorf("HostConfig.Mounts lost %s", m)
		}
	}
	if !slices.Equal(added, anon) {
		t.Errorf("HostConfig.Mounts gained %v, want exactly the anonymous volumes %v", added, anon)
	}
	t.Logf("A-35 observation: HostConfig.Mounts before %v, after %v (anonymous volume remounted by name)", bm, am)
	var remounted []string
	for _, a := range anon {
		remounted = append(remounted, strings.Fields(a)[2])
	}
	for _, c := range []map[string]any{before, after} {
		for _, m := range c["Mounts"].([]any) {
			if mm := m.(map[string]any); slices.Contains(remounted, fmt.Sprint(mm["Destination"])) {
				t.Logf("A-35 observation: mount %v Mode %q", mm["Destination"], mm["Mode"])
			}
		}
	}
	fb, fa := normalized(before, remounted), normalized(after, remounted)
	var diffs []string
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		seen := map[string]bool{}
		for _, m := range []map[string]string{fb, fa} {
			for k := range m {
				if !seen[k] {
					seen[k] = true
					if !yield(k) {
						return
					}
				}
			}
		}
	}) {
		if fb[p] == fa[p] || allowed(p) || strings.HasPrefix(p, "HostConfig.Mounts") {
			continue
		}
		diffs = append(diffs, fmt.Sprintf("%s: %s -> %s", p, fb[p], fa[p]))
	}
	if len(diffs) > 0 {
		t.Fatalf("docker inspect differs outside the allow-list:\n%s", strings.Join(diffs, "\n"))
	}
	for _, p := range []string{"Config.Hostname", "NetworkSettings.Networks.nx-tc116-a.IPAMConfig.IPv4Address",
		"NetworkSettings.Networks.nx-tc116-a.IPAddress"} {
		t.Logf("%s: %s -> %s", p, fb[p], fa[p])
	}
	if fa["NetworkSettings.Networks.nx-tc116-a.IPAddress"] != `"172.30.116.10"` || fa["Config.Hostname"] != `"tc116-host"` {
		t.Fatalf("static IP or explicit hostname not preserved")
	}

	// The anonymous volume: same name, data intact.
	if got := anonymousVolumes(after); len(got) != 0 {
		t.Fatalf("after the update a volume is still anonymous: %v", got)
	}
	if out := dockerCLI(t, "exec", name, "cat", "/anon/marker"); out != marker {
		t.Fatalf("marker %q, want %q", out, marker)
	}
	if old := oldContainers(t, name); len(old) != 0 {
		t.Fatalf("previous container left behind: %v", old)
	}
	if !strings.Contains(n.opLogs(op.ID), "renamed") {
		t.Fatalf("operation output lacks the helper's progress: %s", n.opLogs(op.ID))
	}
	if c := unitStarts(t, op.ID); c != 1 {
		t.Fatalf("nexus-op-%s.service started %d times", op.ID, c)
	}
}

// TC-E20 (FR-12): a container already on the newest digest is reported
// changed false and is not touched.
func TestDockerUpdateAlreadyCurrent(t *testing.T) {
	startRegistries(t)
	name := "tce20-current"
	removeUpdated(t, name)
	ref := regOpen + "/nx/tce20:moving"
	buildImage(t, ref, "tce20 "+time.Now().String(), updPlain)
	regCmd(t, "push", ref)
	d := registryDigest(t, regOpen, "nx/tce20", "moving", false)
	dockerCLI(t, "run", "-d", "--name", name, ref)
	id, started := containerID(t, name), lcInspect(t, name).StartedAt

	n := upNexusd(t, "tce20", "")
	since := time.Now()
	res := wantSucceeded(t, n.waitOp(n.startAction(name, "update", "{}").ID, 2*time.Minute))
	until := time.Now().Add(time.Second)
	if res.Changed || res.RolledBack || res.NewDigest != d || res.PreviousDigest != d || res.ContainerID != id {
		t.Fatalf("result %+v, registry digest %s, id %s", res, d, id)
	}
	if containerID(t, name) != id || !lcInspect(t, name).StartedAt.Equal(started) {
		t.Fatalf("the container changed: id %s started %s", containerID(t, name), lcInspect(t, name).StartedAt)
	}
	if ev := events(t, since, until, "type=container", "event=create", "event=stop", "event=die", "event=rename"); len(ev) != 0 {
		t.Fatalf("docker events during an up-to-date update: %v", ev)
	}
}

// TC-E21 (FR-13): an image without a health check that stays up, and one
// whose health check passes after a delay, both succeed, the second only
// once healthy; the helper ran with the default 60 s health timeout.
func TestDockerUpdateHealthGate(t *testing.T) {
	startRegistries(t)
	plain, delayed := "tce21-plain", "tce21-delayed"
	removeUpdated(t, plain, delayed)
	staged(t, regOpen, "nx/tce21p", plain, nil, updPlain, updPlain)
	staged(t, regOpen, "nx/tce21d", delayed, nil, updPlain, updHealthyAfter(4))
	n := upNexusd(t, "tce21", "")

	op := n.startAction(plain, "update", "{}")
	wantSucceeded(t, n.waitOp(op.ID, 2*time.Minute))
	var in map[string]any
	b, err := os.ReadFile(filepath.Join(updOpDir("tce21", op.ID), "input.json"))
	if err != nil || json.Unmarshal(b, &in) != nil {
		t.Fatalf("input.json: %v %s", err, b)
	}
	if in["healthTimeout"] != float64(60*time.Second) {
		t.Fatalf("helper input %s: healthTimeout is not the 60 s default", b)
	}
	t.Logf("helper input: %s", b)

	op = wantOp(t, n, delayed)
	res := wantSucceeded(t, op)
	var st []struct {
		State struct {
			Health struct {
				Status string
				Log    []struct {
					End      time.Time
					ExitCode int
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(dockerCLI(t, "inspect", "--type", "container", delayed)), &st); err != nil || len(st) != 1 {
		t.Fatal(err)
	}
	h := st[0].State.Health
	first := time.Time{}
	for _, l := range h.Log {
		if l.ExitCode == 0 && (first.IsZero() || l.End.Before(first)) {
			first = l.End
		}
	}
	if !res.Changed || h.Status != "healthy" || first.IsZero() || first.After(*op.Ended) {
		t.Fatalf("health %s, first healthy probe %s, operation ended %s", h.Status, first, op.Ended)
	}
	t.Logf("first healthy probe %s, operation ended %s (%s later)", first, op.Ended, op.Ended.Sub(first).Round(time.Millisecond))
}

func wantOp(t *testing.T, n *nexusdInstance, name string) apitypes.Operation {
	t.Helper()
	return n.waitOp(n.startAction(name, "update", "{}").ID, 3*time.Minute)
}

// TC-E22 (FR-12): a generated hostname is not carried over, and a
// container sharing the updated one's network namespace is warned about.
func TestDockerUpdateGeneratedHostnameAndDependents(t *testing.T) {
	startRegistries(t)
	name, dep := "tce22-web", "tce22-dep"
	removeUpdated(t, name, dep)
	staged(t, regOpen, "nx/tce22", name, nil, updPlain, updPlain)
	dockerCLI(t, "run", "-d", "--name", dep, "--network", "container:"+name, testImage, "sleep", "3600")
	oldHost := dockerInspectField(t, name, "{{.Config.Hostname}}")
	if oldHost != containerID(t, name)[:12] {
		t.Fatalf("hostname %s is not the generated short ID", oldHost)
	}
	res := wantSucceeded(t, wantOp(t, upNexusd(t, "tce22", ""), name))
	newHost := dockerInspectField(t, name, "{{.Config.Hostname}}")
	if newHost == oldHost || newHost != res.ContainerID[:12] {
		t.Fatalf("hostname %s after the update (old %s, new id %s)", newHost, oldHost, res.ContainerID)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, dep) }) {
		t.Fatalf("warnings %q do not name %s", res.Warnings, dep)
	}
	t.Logf("old containers left: %v", oldContainers(t, name))
}

func dockerInspectField(t *testing.T, name, format string) string {
	t.Helper()
	return dockerCLI(t, "inspect", "--type", "container", "-f", format, name)
}

// TC-ERR36 (FR-12, A-36): locally built, digest-pinned and auto-removed
// containers are refused with 422 not-updatable and a reason before
// anything happens: no operation, no Engine event. (Compose-managed
// containers are updated since WI 6.5; their refusals are TC-ERR41 and
// TC-ERR42 in docker_compose_test.go.) The locally built one has a hint naming scripts. The check
// runs first: on the containerd image store only the registry tells a
// local build, and update reads the stored status (A-13).
func TestDockerUpdateRefusesNotUpdatable(t *testing.T) {
	startRegistries(t)
	built, pinned, rm := "tcerr36-built", "tcerr36-pinned", "tcerr36-rm"
	removeUpdated(t, built, pinned, rm)
	local := regOpen + "/nx/tcerr36-local:dev"
	buildImage(t, local, "built here "+time.Now().String(), updPlain)
	dockerCLI(t, "run", "-d", "--name", built, local)
	ref := regOpen + "/nx/tcerr36:v1"
	buildImage(t, ref, "tcerr36 "+time.Now().String(), updPlain)
	regCmd(t, "push", ref)
	dockerCLI(t, "run", "-d", "--name", pinned, regOpen+"/nx/tcerr36@"+registryDigest(t, regOpen, "nx/tcerr36", "v1", false))
	dockerCLI(t, "run", "-d", "--rm", "--name", rm, ref)

	n := upNexusd(t, "tcerr36", "")
	n.check()
	since := time.Now()
	for _, tc := range []struct{ name, reason string }{
		{built, spec.ReasonLocallyBuilt}, {pinned, spec.ReasonDigestPinned}, {rm, spec.ReasonAutoRemove},
	} {
		code, b := n.act(tc.name, "update", "{}")
		p := lcProblem(t, code, b)
		if code != http.StatusUnprocessableEntity || p.Slug() != problem.NotUpdatable || p.Extensions["reason"] != tc.reason {
			t.Fatalf("%s: %d %s, want 422 not-updatable %s", tc.name, code, b, tc.reason)
		}
		hint, _ := p.Extensions["hint"].(string)
		if tc.name == built && !strings.Contains(hint, "script") {
			t.Fatalf("%s: hint %q does not name scripts", tc.name, hint)
		}
		t.Logf("%s: %d %s", tc.name, code, b)
	}
	until := time.Now().Add(time.Second)
	var ops apitypes.List[apitypes.Operation]
	n.mustGet("/v1/operations?filter=provider%3Ddocker&limit=1000", &ops)
	for _, op := range ops.Items {
		if op.Action == "docker.containers.update" {
			t.Fatalf("an update operation was created: %+v", op)
		}
	}
	if ev := events(t, since, until); len(ev) != 0 {
		t.Fatalf("Engine events during refused updates: %v", ev)
	}
}

// TC-ERR37 (FR-13): a new image whose health check fails within a short
// timeout, and one that exits at once without a health check, each end
// Failed rolledBack true with both digests; the original container runs
// under its name and ID, and no new or -nexus-old- container remains.
func TestDockerUpdateHealthFailureRollsBack(t *testing.T) {
	startRegistries(t)
	unhealthy, exits := "tcerr37-unhealthy", "tcerr37-exits"
	removeUpdated(t, unhealthy, exits)
	n := upNexusd(t, "tcerr37", ", updateHealthTimeout: 8s")
	for _, tc := range []struct {
		name string
		v2   imageDef
	}{
		{unhealthy, imageDef{cmd: updQuick, hc: updHealthFalse}},
		{exits, imageDef{cmd: []string{"true"}}},
	} {
		_, oldD, newD := staged(t, regOpen, "nx/"+tc.name, tc.name, []string{"--restart", "unless-stopped"}, updPlain, tc.v2)
		id := containerID(t, tc.name)
		since := time.Now()
		ext := wantFailed(t, wantOp(t, n, tc.name), problem.UpdateHealthFailed)
		until := time.Now().Add(time.Second)
		if ext["rolledBack"] != true || ext["previousDigest"] != oldD || ext["newDigest"] != newD {
			t.Fatalf("%s: extensions %v, want rolledBack and digests %s -> %s", tc.name, ext, oldD, newD)
		}
		if containerID(t, tc.name) != id || !lcInspect(t, tc.name).Running {
			t.Fatalf("%s: running %v id %s, want the original %s", tc.name, lcInspect(t, tc.name).Running,
				containerID(t, tc.name), id)
		}
		if old := oldContainers(t, tc.name); len(old) != 0 {
			t.Fatalf("%s: left %v", tc.name, old)
		}
		created := events(t, since, until, "type=container", "event=create")
		if len(created) != 1 {
			t.Fatalf("%s: create events %v, want the one new container", tc.name, created)
		}
		f := strings.Fields(created[0])
		if newID := f[len(f)-1]; newID == id || containerID(t, newID) != "" {
			t.Fatalf("%s: the new container %s still exists", tc.name, newID)
		}
	}
}

// blobProxy is a reverse proxy to the open registry whose blob GETs answer
// 500 while fail is set: the digest check passes and the pull fails.
type blobProxy struct{ fail atomic.Bool }

var (
	blobProxyOnce sync.Once
	theBlobProxy  *blobProxy
	blobProxyErr  error
)

func startBlobProxy(t *testing.T) *blobProxy {
	t.Helper()
	blobProxyOnce.Do(func() {
		target, _ := url.Parse("http://" + regOpen)
		rp := httputil.NewSingleHostReverseProxy(target)
		p := &blobProxy{}
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p.fail.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"injected blob failure"}]}`, http.StatusInternalServerError)
				return
			}
			rp.ServeHTTP(w, r)
		})}
		ln, err := net.Listen("tcp", updBlobProxy)
		if err != nil {
			blobProxyErr = err
			return
		}
		go func() { _ = srv.Serve(ln) }()
		theBlobProxy = p
	})
	if blobProxyErr != nil {
		t.Fatal(blobProxyErr)
	}
	theBlobProxy.fail.Store(false)
	t.Cleanup(func() { theBlobProxy.fail.Store(false) })
	return theBlobProxy
}

// TC-ERR38 (FR-13, NFR-3): with the registry stopped after the check, and
// with a registry whose layers fail to download, the update ends Failed
// dependency-unavailable, not rolled back, and the container never
// stopped.
func TestDockerUpdatePullFailureChangesNothing(t *testing.T) {
	startRegistries(t)
	t.Cleanup(func() { _, _ = sh(5*time.Minute, "bash", regSetup, "start") })
	bp := startBlobProxy(t)
	down, blobs := "tcerr38-down", "tcerr38-blobs"
	removeUpdated(t, down, blobs)
	staged(t, regOpen, "nx/tcerr38", down, nil, updPlain, updPlain)
	staged(t, updBlobProxy, "nx/tcerr38b", blobs, nil, updPlain, updPlain)
	n := upNexusd(t, "tcerr38", "")
	n.check()

	for _, tc := range []struct {
		name    string
		breakIt func()
	}{
		{blobs, func() { bp.fail.Store(true) }},
		{down, func() { regCmd(t, "stop-open") }},
	} {
		id, started := containerID(t, tc.name), lcInspect(t, tc.name).StartedAt
		tc.breakIt()
		since := time.Now()
		op := wantOp(t, n, tc.name)
		until := time.Now().Add(time.Second)
		ext := wantFailed(t, op, problem.DependencyUnavailable)
		if ext["rolledBack"] == true {
			t.Fatalf("%s: rolledBack after a failure that changed nothing", tc.name)
		}
		if containerID(t, tc.name) != id || !lcInspect(t, tc.name).StartedAt.Equal(started) {
			t.Fatalf("%s: the container changed", tc.name)
		}
		if ev := events(t, since, until, "container="+tc.name); len(ev) != 0 {
			t.Fatalf("%s: container events %v", tc.name, ev)
		}
		t.Logf("%s: output\n%s", tc.name, n.opLogs(op.ID))
	}
}

// kill9Nexusd kills nexusd's main process with SIGKILL and returns once it
// is gone.
func kill9Nexusd(t *testing.T, n *nexusdInstance) {
	t.Helper()
	pid := n.mainPID()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, "nexusd to die", func() (bool, string) { return !pidAlive(pid), "" })
}

func restartNexusd(t *testing.T) time.Time {
	t.Helper()
	lib(t, "stop-nexusd")
	lib(t, "start-nexusd")
	return time.Now()
}

// TC-ERR39 (FR-6, FR-13, NFR-7): nexusd killed with SIGKILL after the
// rename, after the create and after the new container is healthy: the
// helper keeps running with the same PID, nothing rolls back at restart,
// and the update reaches its real outcome within 60 s of the restart. A
// second run's new image fails its health check: the helper's own rollback
// ends it Failed rolledBack true with the original running.
func TestDockerUpdateSurvivesNexusdKill(t *testing.T) {
	startRegistries(t)
	// A short health timeout, so the failing run ends within 60 s of the
	// restart.
	n := upNexusd(t, "tcerr39", ", updateHealthTimeout: 15s")
	type point struct {
		name  string
		wait  func(t *testing.T, since time.Time, name, id string)
		v2    imageDef
		fails bool
	}
	onEvent := func(event string) func(*testing.T, time.Time, string, string) {
		return func(t *testing.T, since time.Time, name, _ string) {
			waitEvent(t, since, 2*time.Minute, "type=container", "event="+event, "container="+name)
		}
	}
	onHealthy := func(t *testing.T, _ time.Time, _, id string) {
		waitUntil(t, 2*time.Minute, "the helper to report healthy", func() (bool, string) {
			return strings.Contains(n.opLogs(id), "is healthy"), ""
		})
	}
	for _, p := range []point{
		{"tcerr39-rename", func(t *testing.T, since time.Time, name, _ string) {
			waitEvent(t, since, 2*time.Minute, "type=container", "event=rename")
		}, updHealthyAfter(5), false},
		{"tcerr39-create", onEvent("create"), updHealthyAfter(5), false},
		{"tcerr39-healthy", onHealthy, updHealthyAfter(2), false},
		{"tcerr39-unhealthy", onEvent("create"), imageDef{cmd: updQuick, hc: updHealthFalse}, true},
	} {
		t.Run(p.name, func(t *testing.T) {
			removeUpdated(t, p.name)
			_, oldD, newD := staged(t, regOpen, "nx/"+p.name, p.name, nil, updPlain, p.v2)
			id := containerID(t, p.name)
			since := time.Now()
			op := n.startAction(p.name, "update", "{}")
			opDir := updOpDir("tcerr39", op.ID)
			hpid, _ := waitHelper(t, opDir)
			p.wait(t, since, p.name, op.ID)
			kill9Nexusd(t, n)
			alive := pidAlive(hpid)
			t.Logf("%s: nexusd killed; helper %d alive %v", p.name, hpid, alive)
			if alive {
				if again, _ := helperProc(opDir); again != hpid {
					t.Fatalf("helper pid %d, was %d", again, hpid)
				}
			}
			restarted := restartNexusd(t)
			var got apitypes.Operation
			waitUntil(t, updRestartMax, "the update to end after the restart", func() (bool, string) {
				got = n.operation(op.ID)
				return terminal(got.Status), string(got.Status)
			})
			t.Logf("%s: terminal %s after %s", p.name, got.Status, time.Since(restarted).Round(time.Millisecond))
			if p.fails {
				ext := wantFailed(t, got, problem.UpdateHealthFailed)
				if ext["rolledBack"] != true || containerID(t, p.name) != id || !lcInspect(t, p.name).Running {
					t.Fatalf("%s: extensions %v; running id %s, original %s", p.name, ext, containerID(t, p.name), id)
				}
			} else {
				res := wantSucceeded(t, got)
				if res.RolledBack || res.NewDigest != newD || res.PreviousDigest != oldD || containerID(t, p.name) == id {
					t.Fatalf("%s: result %+v, registry %s -> %s", p.name, res, oldD, newD)
				}
			}
			if old := oldContainers(t, p.name); len(old) != 0 {
				t.Fatalf("%s: left %v", p.name, old)
			}
			if c := unitStarts(t, op.ID); c != 1 {
				t.Fatalf("%s: unit started %d times", p.name, c)
			}
		})
	}
}

// TC-ERR40 (FR-7, G-5): while an update holds container:<name>, held by
// the delaying registry proxy, a stop of the same container answers 409
// naming the update.
func TestDockerUpdateBlocksLifecycle(t *testing.T) {
	startRegistries(t)
	p := startProxy(t)
	name := "tcerr40-held"
	removeUpdated(t, name)
	staged(t, regProxy, "nx/tcerr40", name, nil, updPlain, updPlain)
	n := upNexusd(t, "tcerr40", "")
	p.delay.Store(int64(8 * time.Second))
	op := n.startAction(name, "update", "{}")
	waitHelper(t, updOpDir("tcerr40", op.ID))
	code, b := n.act(name, "stop", "{}")
	prob := lcProblem(t, code, b)
	if code != http.StatusConflict || prob.Extensions["blockingOperationId"] != op.ID {
		t.Fatalf("stop during the update: %d %s, want 409 naming %s", code, b, op.ID)
	}
	if st := n.operation(op.ID).Status; terminal(st) {
		t.Fatalf("the update had ended (%s): the conflict was not tested", st)
	}
	p.delay.Store(0)
	wantSucceeded(t, n.waitOp(op.ID, 3*time.Minute))
}

// TC-ERR70 (FR-6, FR-13, NFR-7): the helper killed after the rename with
// nexusd up ends outcome-unknown within 60 s, naming the
// <name>-nexus-old-* container, which still exists; nothing retries or
// rolls back. Killing only the helper leaves the supervisor to write
// exit.json; killing the whole unit leaves no evidence at all.
func TestDockerUpdateKilledHelperIsOutcomeUnknown(t *testing.T) {
	startRegistries(t)
	n := upNexusd(t, "tcerr70", ", updateHealthTimeout: 300s")
	for _, tc := range []struct {
		name string
		kill func(t *testing.T, id string, helper int)
	}{
		{"tcerr70-helper", func(t *testing.T, _ string, helper int) {
			if err := syscall.Kill(helper, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}},
		{"tcerr70-unit", func(t *testing.T, id string, _ int) {
			killOpUnit(t, detach.Unit(id))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removeUpdated(t, tc.name)
			staged(t, regOpen, "nx/"+tc.name, tc.name, nil, updPlain, updHealthyAfter(600))
			since := time.Now()
			op := n.startAction(tc.name, "update", "{}")
			opDir := updOpDir("tcerr70", op.ID)
			helper, _ := waitHelper(t, opDir)
			waitEvent(t, since, 2*time.Minute, "type=container", "event=rename")
			tc.kill(t, op.ID, helper)
			killed := time.Now()
			var got apitypes.Operation
			waitUntil(t, updRestartMax, "outcome-unknown after the kill", func() (bool, string) {
				got = n.operation(op.ID)
				return terminal(got.Status), string(got.Status)
			})
			t.Logf("%s: %s after %s", tc.name, got.Status, time.Since(killed).Round(time.Millisecond))
			wantFailed(t, got, problem.OutcomeUnknown)
			old := oldContainers(t, tc.name)
			if len(old) != 1 || !strings.Contains(got.Error.Detail, old[0]) {
				t.Fatalf("%s: detail %q, old containers %v", tc.name, got.Error.Detail, old)
			}
			_, hasResult := os.Stat(filepath.Join(opDir, "result.json"))
			_, hasExit := os.Stat(filepath.Join(opDir, "exit.json"))
			t.Logf("%s: result.json absent %v, exit.json present %v", tc.name, hasResult != nil, hasExit == nil)
			if hasResult == nil {
				t.Fatalf("%s: result.json exists", tc.name)
			}
			mark := time.Now()
			time.Sleep(5 * time.Second)
			if ev := events(t, mark, time.Now(), "type=container", "event=rename", "event=create", "event=start"); len(ev) != 0 {
				t.Fatalf("%s: something retried or rolled back: %v", tc.name, ev)
			}
			if again := oldContainers(t, tc.name); !slices.Equal(again, old) {
				t.Fatalf("%s: old containers %v, were %v", tc.name, again, old)
			}
		})
	}
}
