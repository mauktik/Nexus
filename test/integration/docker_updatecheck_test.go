//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// WI 6.3 against the real nested Engine and real registry:2 registries
// started by test/harness/fixtures/registry/setup.sh. Every expected digest
// comes from the registry itself, by an HTTP HEAD of the manifest in the
// test (OCI Distribution Spec, "Pulling manifests": the response carries
// Docker-Content-Digest), never from Nexus.

const (
	regSetup   = "/harness/fixtures/registry/setup.sh"
	regOpen    = "127.0.0.1:5000"
	regAuth    = "127.0.0.1:5001"
	regProxy   = "127.0.0.1:5098" // In-test proxy to regOpen (TC-ERR34, TC-ERR35)
	regUser    = "nxuser"
	regPass    = "nx-fixture-pass-7Qk" // setup.sh's fixture account
	checkAt    = "/v1/providers/docker/actions/check-updates"
	dockerConf = "/root/.docker"
)

// manifestAccept is every manifest type a registry may serve for a tag.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

func regCmd(t *testing.T, args ...string) string {
	t.Helper()
	out, err := sh(10*time.Minute, "bash", append([]string{regSetup}, args...)...)
	if err != nil {
		t.Fatalf("setup.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// startRegistries starts both registries (idempotent) and waits until each
// answers /v2/ over HTTP.
func startRegistries(t *testing.T) {
	t.Helper()
	regCmd(t, "start")
	for _, host := range []string{regOpen, regAuth} {
		waitUntil(t, 30*time.Second, host+" /v2/", func() (bool, string) {
			resp, err := http.Get("http://" + host + "/v2/")
			if err != nil {
				return false, err.Error()
			}
			resp.Body.Close()
			return resp.StatusCode == 200 || resp.StatusCode == 401, resp.Status
		})
	}
}

// registryDigest is the independent expected digest: a HEAD of the
// manifest with the manifest Accept types.
func registryDigest(t *testing.T, host, repo, tag string, auth bool) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, "http://"+host+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", manifestAccept)
	if auth {
		req.SetBasicAuth(regUser, regPass)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("HEAD %s/%s:%s: %s digest %q", host, repo, tag, resp.Status, d)
	}
	return d
}

// pushed builds an image with content, tags it name and pushes it.
func pushed(t *testing.T, name, content string) {
	t.Helper()
	regCmd(t, "image", name, content)
	regCmd(t, "push", name)
}

// runFrom runs a container named name from ref.
func runFrom(t *testing.T, name, ref string, extra ...string) {
	t.Helper()
	dockerCLI(t, slices.Concat([]string{"run", "-d", "--name", name}, extra, []string{ref, "sleep", "3600"})...)
}

// configDir writes a Docker CLI config.json into a fresh directory under
// /etc/nexus, which nexusd's sandbox can read (PrivateTmp hides /tmp).
// An empty js leaves the directory empty.
func configDir(t *testing.T, name, js string) string {
	t.Helper()
	dir := "/etc/nexus/dockercfg-" + name
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if js != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(js), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func authsConfig(host string) string {
	return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64(regUser+":"+regPass))
}

// ucNexusd is a nexusd whose docker section reads credentials from dir
// ("" keeps the default, /root/.docker).
func ucNexusd(t *testing.T, name, dir string) *nexusdInstance {
	t.Helper()
	section := fmt.Sprintf("  docker: {enabled: true, socket: %s}\n", dockerSocket)
	if dir != "" {
		section = fmt.Sprintf("  docker: {enabled: true, socket: %s, configDir: %s}\n", dockerSocket, dir)
	}
	return newNexusd(t, name, section)
}

type checkResult struct {
	op      apitypes.Operation
	updates map[string]spec.UpdateCheck
	raw     string
}

// startCheck posts check-updates and requires 202.
func (n *nexusdInstance) startCheck() apitypes.Operation {
	n.t.Helper()
	code, b := n.post(checkAt, "{}")
	if code != http.StatusAccepted {
		n.t.Fatalf("check-updates: %d %s", code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		n.t.Fatal(err)
	}
	return op
}

// check runs check-updates to its end, requires Succeeded, and returns
// the update of each container.
func (n *nexusdInstance) check() checkResult {
	n.t.Helper()
	return n.finish(n.startCheck().ID)
}

func (n *nexusdInstance) finish(id string) checkResult {
	n.t.Helper()
	op := n.waitOp(id, 3*time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		n.t.Fatalf("check-updates %s: %s %+v", id, op.Status, op.Error)
	}
	var res spec.CheckUpdatesResult
	if err := json.Unmarshal(op.Result, &res); err != nil {
		n.t.Fatalf("result %s: %v", op.Result, err)
	}
	out := checkResult{op: op, updates: map[string]spec.UpdateCheck{}, raw: string(op.Result)}
	for _, it := range res.Items {
		if it.Host != "harness" || it.ID != "/providers/docker/containers/"+it.Name {
			n.t.Errorf("item %s: host %q id %q", it.Name, it.Host, it.ID)
		}
		out.updates[it.Name] = it.Properties.Update
	}
	return out
}

func (n *nexusdInstance) opLogs(id string) string {
	n.t.Helper()
	code, b := n.get("/v1/operations/" + id + "/logs")
	if code != http.StatusOK {
		n.t.Fatalf("logs %s: %d %s", id, code, b)
	}
	return string(b)
}

// listUpdates lists containers with the filters and returns name -> status
// of those named in names.
func (n *nexusdInstance) listUpdates(filters []string, names ...string) map[string]string {
	n.t.Helper()
	q := url.Values{"limit": {"1000"}}
	for _, f := range filters {
		q.Add("filter", f)
	}
	var l apitypes.List[spec.Container]
	n.mustGet(containersAt+"?"+q.Encode(), &l)
	out := map[string]string{}
	for _, c := range l.Items {
		if slices.Contains(names, c.Name) {
			out[c.Name] = c.Properties.Update.Status
		}
	}
	return out
}

func wantStatus(t *testing.T, got map[string]spec.UpdateCheck, name, status, reason string) spec.UpdateCheck {
	t.Helper()
	u, ok := got[name]
	if !ok {
		t.Fatalf("%s has no result", name)
	}
	if u.Status != status || u.Reason != reason || u.CheckedAt == nil {
		t.Fatalf("%s: %+v, want %s %s", name, u, status, reason)
	}
	return u
}

// imageState is every image with its digests and layers: what a pull would
// change.
func imageState(t *testing.T) string {
	t.Helper()
	ids := strings.Fields(dockerCLI(t, "images", "-a", "-q", "--no-trunc"))
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return ""
	}
	return dockerCLI(t, append([]string{"image", "inspect", "--format",
		"{{.Id}} {{.RepoTags}} {{.RepoDigests}} {{.RootFS.Layers}}"}, ids...)...)
}

func pullEvents(t *testing.T, since, until time.Time) string {
	t.Helper()
	return dockerCLI(t, "events", "--since", since.Format(time.RFC3339Nano), "--until", until.Format(time.RFC3339Nano),
		"--filter", "type=image", "--filter", "event=pull", "--format", "{{.Action}} {{.Actor.ID}}")
}

// upToDateAndStale pushes repo:current and repo:moving with one image, runs
// a container from each, then pushes a newer image as repo:moving. It
// returns the expected (registry) digests of both tags.
func upToDateAndStale(t *testing.T, host, repo, current, stale string) (curDigest, newDigest string) {
	t.Helper()
	requireDocker(t, current, stale)
	base := fmt.Sprintf("%s/%s", host, repo)
	pushed(t, base+":current", repo+" one "+time.Now().String())
	dockerCLI(t, "tag", base+":current", base+":moving")
	regCmd(t, "push", base+":moving")
	runFrom(t, current, base+":current")
	runFrom(t, stale, base+":moving")
	pushed(t, base+":moving", repo+" two "+time.Now().String())
	curDigest = registryDigest(t, host, repo, "current", host == regAuth)
	newDigest = registryDigest(t, host, repo, "moving", host == regAuth)
	if curDigest == newDigest {
		t.Fatalf("the newer push did not change the digest: %s", newDigest)
	}
	return curDigest, newDigest
}

// TC-111: against a real registry, the current container is upToDate and
// the older one updateAvailable, each against the registry's own digest;
// the check pulls nothing and changes no image or layer.
func TestDockerUpdateClassificationFromRealRegistry(t *testing.T) {
	startRegistries(t)
	cur, latest := upToDateAndStale(t, regOpen, "nx/tc111", "tc111-current", "tc111-stale")
	n := ucNexusd(t, "tc111", configDir(t, "tc111", ""))

	before := imageState(t)
	since := time.Now()
	res := n.check()
	until := time.Now().Add(time.Second)
	t.Logf("result: %s", res.raw)

	u := wantStatus(t, res.updates, "tc111-current", spec.UpdateUpToDate, "")
	if u.LatestDigest != cur || u.CurrentDigest != cur {
		t.Fatalf("current: %+v, registry digest %s", u, cur)
	}
	u = wantStatus(t, res.updates, "tc111-stale", spec.UpdateAvailable, "")
	if u.LatestDigest != latest || u.CurrentDigest != cur {
		t.Fatalf("stale: %+v, registry digests current %s latest %s", u, cur, latest)
	}
	if after := imageState(t); after != before {
		t.Fatalf("images or layers changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	if ev := pullEvents(t, since, until); ev != "" {
		t.Fatalf("docker events shows a pull during the check: %s", ev)
	}
	logs := n.opLogs(res.op.ID)
	if !strings.Contains(logs, "tc111-stale: updateAvailable") {
		t.Fatalf("operation output: %s", logs)
	}
}

// TC-112 (ASM-8, A-36): digest-pinned, locally built (pointing to the
// scripts that build it), image-ID and auto-removed containers are each
// notUpdatable with their reason. The locally built image is named for the
// local registry and never pushed: on the classic store it has no
// RepoDigests and needs no registry call; on the containerd store every
// tagged image has a RepoDigest (name@target), and the registry's "manifest
// unknown" identifies it. A Docker Hub-named local build is logged: Hub
// answers a missing repository as it answers a private one.
func TestDockerUpdateNotUpdatableReasons(t *testing.T) {
	startRegistries(t)
	pinned, built, byID, rm, hubNamed := "tc112-pinned", "tc112-built", "tc112-byid", "tc112-rm", "tc112-hubnamed"
	requireDocker(t, pinned, built, byID, rm, hubNamed)
	pushed(t, regOpen+"/nx/tc112:v1", "tc112 "+time.Now().String())
	d := registryDigest(t, regOpen, "nx/tc112", "v1", false)
	runFrom(t, pinned, regOpen+"/nx/tc112@"+d)
	local := regOpen + "/nx/tc112-local:dev"
	regCmd(t, "image", local, "built here "+time.Now().String())
	t.Logf("image store %s; the local build's RepoDigests %s", dockerCLI(t, "info", "--format", "{{json .DriverStatus}}"),
		dockerCLI(t, "image", "inspect", "-f", "{{.RepoDigests}}", local))
	runFrom(t, built, local)
	regCmd(t, "image", "nx-local/tc112:dev", "built here too "+time.Now().String())
	runFrom(t, hubNamed, "nx-local/tc112:dev")
	runFrom(t, byID, dockerCLI(t, "image", "inspect", "-f", "{{.Id}}", regOpen+"/nx/tc112:v1"))
	runFrom(t, rm, regOpen+"/nx/tc112:v1", "--rm")

	n := ucNexusd(t, "tc112", configDir(t, "tc112", ""))
	since := time.Now()
	res := n.check()
	t.Logf("result: %s", res.raw)
	wantStatus(t, res.updates, pinned, spec.UpdateNotUpdatable, spec.ReasonDigestPinned)
	u := wantStatus(t, res.updates, built, spec.UpdateNotUpdatable, spec.ReasonLocallyBuilt)
	if !strings.Contains(u.Detail, "script") || !strings.Contains(u.Detail, "built") {
		t.Fatalf("locally built detail does not point to scripts: %q", u.Detail)
	}
	wantStatus(t, res.updates, byID, spec.UpdateNotUpdatable, spec.ReasonNoRegistrySource)
	wantStatus(t, res.updates, rm, spec.UpdateNotUpdatable, spec.ReasonAutoRemove)
	if hits := registryRequests(t, "nx-registry", since, "nx/tc112"); hits != 0 {
		t.Fatalf("%d registry requests for the pinned, image-ID and auto-removed containers", hits)
	}
	h := res.updates[hubNamed]
	t.Logf("Docker Hub-named local build: %s %s: %s", h.Status, h.Reason, h.Detail)
	if h.Status == spec.UpdateUpToDate || h.Status == spec.UpdateAvailable || h.CheckedAt == nil {
		t.Fatalf("Docker Hub-named local build: %+v", h)
	}
}

// registryRequests counts the access-log lines of registry container name
// since a time that mention repo.
func registryRequests(t *testing.T, name string, since time.Time, repo string) int {
	t.Helper()
	out := dockerCLI(t, "logs", "--since", since.Format(time.RFC3339Nano), name)
	n := 0
	for line := range strings.Lines(out) {
		if strings.Contains(line, "/v2/"+repo+"/") {
			n++
		}
	}
	return n
}

// TC-113 (A-32): the comparison is identical and correct on the containerd
// image store and on the classic one, with no false updateAvailable on
// classic, where .Image is not the manifest digest.
func TestDockerUpdateBothImageStores(t *testing.T) {
	startRegistries(t)
	orig, hadOrig := readFile(t, "/etc/docker/daemon.json")
	t.Cleanup(func() {
		if hadOrig {
			_ = os.WriteFile("/etc/docker/daemon.json", orig, 0o644)
		} else {
			_ = os.Remove("/etc/docker/daemon.json")
		}
		restartDocker(t)
		_, _ = sh(5*time.Minute, "bash", regSetup, "start")
	})
	results := map[bool]map[string]string{}
	for _, containerd := range []bool{false, true} {
		js := fmt.Sprintf(`{"features":{"containerd-snapshotter":%v}}`, containerd)
		if err := os.WriteFile("/etc/docker/daemon.json", []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
		restartDocker(t)
		status := dockerCLI(t, "info", "--format", "{{json .DriverStatus}}")
		if got := strings.Contains(status, "io.containerd.snapshotter"); got != containerd {
			t.Fatalf("containerd-snapshotter %v, but docker info DriverStatus is %s", containerd, status)
		}
		store := map[bool]string{true: "containerd", false: "classic"}[containerd]
		t.Logf("store %s: docker info DriverStatus %s, driver %s", store, status, dockerCLI(t, "info", "--format", "{{.Driver}}"))
		startRegistries(t)
		cur, stale, built := "tc113-"+store+"-current", "tc113-"+store+"-stale", "tc113-"+store+"-built"
		curD, newD := upToDateAndStale(t, regOpen, "nx/tc113-"+store, cur, stale)
		requireDocker(t, built)
		local := regOpen + "/nx/tc113-" + store + "-local:dev"
		regCmd(t, "image", local, "built "+store+" "+time.Now().String())
		runFrom(t, built, local)
		var imageID string
		var ids []string
		for _, c := range []string{cur, stale} {
			id := dockerCLI(t, "inspect", "-f", "{{.Image}}", c)
			ids = append(ids, id)
			if c == cur {
				imageID = id
			}
		}
		t.Logf("store %s: .Image of the current container %s, registry digest %s (equal: %v)", store, imageID, curD, imageID == curD)

		n := ucNexusd(t, "tc113-"+store, configDir(t, "tc113-"+store, ""))
		res := n.check()
		t.Logf("store %s result: %s", store, res.raw)
		u := wantStatus(t, res.updates, cur, spec.UpdateUpToDate, "")
		if u.LatestDigest != curD {
			t.Fatalf("store %s current: %+v, registry %s", store, u, curD)
		}
		u = wantStatus(t, res.updates, stale, spec.UpdateAvailable, "")
		if u.LatestDigest != newD || u.CurrentDigest != curD {
			t.Fatalf("store %s stale: %+v, registry %s -> %s", store, u, curD, newD)
		}
		wantStatus(t, res.updates, built, spec.UpdateNotUpdatable, spec.ReasonLocallyBuilt)
		results[containerd] = map[string]string{"current": res.updates[cur].Status, "stale": res.updates[stale].Status,
			"built": res.updates[built].Status + "/" + res.updates[built].Reason}
		for _, c := range []string{cur, stale, built} {
			_, _ = sh(time.Minute, "docker", "rm", "-f", "-v", c)
		}
	}
	if fmt.Sprint(results[true]) != fmt.Sprint(results[false]) {
		t.Fatalf("results differ by store: containerd %v classic %v", results[true], results[false])
	}
}

// restartDocker restarts the nested Engine and waits for it. docker.service
// limits starts to 3 a minute, so the failed-start counter is reset first.
func restartDocker(t *testing.T) {
	t.Helper()
	_, _ = sh(time.Minute, "systemctl", "reset-failed", "docker.service")
	if out, err := sh(3*time.Minute, "systemctl", "restart", "docker.service"); err != nil {
		t.Errorf("restart docker: %v\n%s", err, out)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := sh(10*time.Second, "docker", "info"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("docker did not answer after a restart")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

// noHelperRun requires the helper's marker to be absent.
func noHelperRun(t *testing.T) {
	t.Helper()
	marker := strings.TrimSpace(regCmd(t, "helper-marker"))
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the credential helper was executed: %s", b)
	}
}

func clearHelperMarker(t *testing.T) {
	t.Helper()
	if err := os.Remove(strings.TrimSpace(regCmd(t, "helper-marker"))); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// noSecret requires the fixture password, raw or base64, to be absent from
// the result, the operation output and nexusd's journal.
func noSecret(t *testing.T, n *nexusdInstance, res checkResult, since time.Time) {
	t.Helper()
	journal := string(systemLog(t)) // On OpenRC, all of /var/log/messages
	if detectedInit() != platform.OpenRC {
		journal = mustSh(t, "journalctl", "-u", nexusdUnit, "--no-pager", "--since", since.Format("2006-01-02 15:04:05"))
	}
	all := res.raw + n.opLogs(res.op.ID) + journal
	for _, s := range []string{regPass, b64(regUser + ":" + regPass)} {
		if strings.Contains(all, s) {
			t.Fatalf("the credential %q appears in the result, output or journal", s)
		}
	}
}

// TC-114 (A-14, A-61): auths gives a real comparison; credHelpers and
// credsStore give unknown naming the helper, which is never run; the
// default path is /root/.docker; an empty configDir is unknown; the
// credential appears in no log or result.
func TestDockerUpdateRegistryCredentialSources(t *testing.T) {
	startRegistries(t)
	clearHelperMarker(t)
	cur, latest := upToDateAndStale(t, regAuth, "nx/tc114", "tc114-current", "tc114-stale")
	since := time.Now().Add(-time.Second)

	n := ucNexusd(t, "tc114-auths", configDir(t, "tc114-auths", authsConfig(regAuth)))
	res := n.check()
	t.Logf("auths: %s", res.raw)
	if u := wantStatus(t, res.updates, "tc114-current", spec.UpdateUpToDate, ""); u.LatestDigest != cur {
		t.Fatalf("auths current: %+v, registry %s", u, cur)
	}
	if u := wantStatus(t, res.updates, "tc114-stale", spec.UpdateAvailable, ""); u.LatestDigest != latest {
		t.Fatalf("auths stale: %+v, registry %s", u, latest)
	}
	noSecret(t, n, res, since)

	for _, tc := range []struct{ name, js, source string }{
		{"helpers", fmt.Sprintf(`{"auths":{%q:{}},"credHelpers":{%q:"nx"}}`, regAuth, regAuth), "credHelpers"},
		{"store", `{"auths":{"127.0.0.1:5001":{}},"credsStore":"nx"}`, "credsStore"},
	} {
		n := ucNexusd(t, "tc114-"+tc.name, configDir(t, "tc114-"+tc.name, tc.js))
		res := n.check()
		t.Logf("%s: %s", tc.name, res.raw)
		for _, c := range []string{"tc114-current", "tc114-stale"} {
			u := wantStatus(t, res.updates, c, spec.UpdateUnknown, spec.ReasonCredentialHelper)
			if !strings.Contains(u.Detail, "docker-credential-nx") || !strings.Contains(u.Detail, tc.source) ||
				!strings.Contains(u.Detail, "docker login") || !strings.Contains(u.Detail, "providers.docker.configDir") {
				t.Fatalf("%s %s: detail %q", tc.name, c, u.Detail)
			}
		}
		noHelperRun(t)
	}

	// The default configDir is /root/.docker.
	orig, hadOrig := readFile(t, filepath.Join(dockerConf, "config.json"))
	t.Cleanup(func() {
		if hadOrig {
			_ = os.WriteFile(filepath.Join(dockerConf, "config.json"), orig, 0o600)
		} else {
			_ = os.Remove(filepath.Join(dockerConf, "config.json"))
		}
	})
	if err := os.MkdirAll(dockerConf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dockerConf, "config.json"), []byte(authsConfig("http://"+regAuth+"/v2/")), 0o600); err != nil {
		t.Fatal(err)
	}
	n = ucNexusd(t, "tc114-default", "")
	res = n.check()
	t.Logf("default: %s", res.raw)
	wantStatus(t, res.updates, "tc114-current", spec.UpdateUpToDate, "")
	wantStatus(t, res.updates, "tc114-stale", spec.UpdateAvailable, "")
	noSecret(t, n, res, since)

	n = ucNexusd(t, "tc114-empty", configDir(t, "tc114-empty", ""))
	res = n.check()
	t.Logf("empty: %s", res.raw)
	u := wantStatus(t, res.updates, "tc114-current", spec.UpdateUnknown, spec.ReasonAuthRequired)
	if !strings.Contains(u.Detail, "docker login") {
		t.Fatalf("empty config detail %q", u.Detail)
	}
	noHelperRun(t)
}

// TC-115 (A-13, NFR-1): before a check the filter returns none and every
// container is notChecked; after it, exactly the out-of-date container;
// listing again later sends nothing to the registry.
func TestDockerUpdateStoredStatusMerges(t *testing.T) {
	startRegistries(t)
	cur, stale := "tc115-current", "tc115-stale"
	upToDateAndStale(t, regOpen, "nx/tc115", cur, stale)
	n := ucNexusd(t, "tc115", configDir(t, "tc115", ""))

	if got := n.listUpdates([]string{"status=updateAvailable"}, cur, stale); len(got) != 0 {
		t.Fatalf("filter before a check: %v", got)
	}
	if got := n.listUpdates(nil, cur, stale); got[cur] != spec.UpdateNotChecked || got[stale] != spec.UpdateNotChecked {
		t.Fatalf("unfiltered before a check: %v", got)
	}
	n.check()
	step2 := n.listUpdates([]string{"status=updateAvailable"}, cur, stale)
	if len(step2) != 1 || step2[stale] != spec.UpdateAvailable {
		t.Fatalf("filter after the check: %v", step2)
	}
	var one spec.Container
	n.mustGet(containersAt+"/"+cur, &one)
	if one.Properties.Update.Status != spec.UpdateUpToDate {
		t.Fatalf("get %s: %+v", cur, one.Properties.Update)
	}

	quiet := time.Now()
	time.Sleep(5 * time.Second)
	step3 := n.listUpdates([]string{"status=updateAvailable"}, cur, stale)
	if fmt.Sprint(step3) != fmt.Sprint(step2) {
		t.Fatalf("step 3 %v differs from step 2 %v", step3, step2)
	}
	if hits := registryRequests(t, "nx-registry", quiet, "nx/tc115"); hits != 0 {
		t.Fatalf("%d registry requests after the check: a list or a background check reached the registry", hits)
	}

	// The result is in state.db: a restarted nexusd still reports it.
	lib(t, "stop-nexusd")
	lib(t, "start-nexusd")
	if got := n.listUpdates([]string{"status=updateAvailable"}, cur, stale); fmt.Sprint(got) != fmt.Sprint(step2) {
		t.Fatalf("after a nexusd restart: %v, want %v", got, step2)
	}
	if hits := registryRequests(t, "nx-registry", quiet, "nx/tc115"); hits != 0 {
		t.Fatalf("%d registry requests after the restart", hits)
	}
}

// slowProxy is a reverse proxy to the open registry whose manifest
// requests can be delayed or answered 500.
type slowProxy struct {
	delay atomic.Int64 // Nanoseconds
	fail  atomic.Bool
	srv   *http.Server
}

var (
	proxyOnce sync.Once
	proxy     *slowProxy
	proxyErr  error
)

func startProxy(t *testing.T) *slowProxy {
	t.Helper()
	proxyOnce.Do(func() {
		target, _ := url.Parse("http://" + regOpen)
		rp := httputil.NewSingleHostReverseProxy(target)
		p := &slowProxy{}
		p.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/manifests/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				if p.fail.Load() {
					http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"injected failure"}]}`, http.StatusInternalServerError)
					return
				}
				select {
				case <-time.After(time.Duration(p.delay.Load())):
				case <-r.Context().Done():
					return
				}
			}
			rp.ServeHTTP(w, r)
		})}
		ln, err := net.Listen("tcp", regProxy)
		if err != nil {
			proxyErr = err
			return
		}
		go func() { _ = p.srv.Serve(ln) }()
		proxy = p
	})
	if proxyErr != nil {
		t.Fatal(proxyErr)
	}
	proxy.delay.Store(0)
	proxy.fail.Store(false)
	t.Cleanup(func() { proxy.delay.Store(0); proxy.fail.Store(false) })
	return proxy
}

// TC-ERR34 (FR-11, NFR-3): a container that is really up to date is
// unknown with a reason under each failure, never upToDate, while the
// operation succeeds; once the registry is back it is upToDate.
func TestDockerUpdateErrorsAreUnknown(t *testing.T) {
	startRegistries(t)
	p := startProxy(t)
	clearHelperMarker(t)
	open, viaProxy, private := "tcerr34-open", "tcerr34-proxy", "tcerr34-auth"
	requireDocker(t, open, viaProxy, private)
	pushed(t, regOpen+"/nx/tcerr34:v1", "err34 "+time.Now().String())
	runFrom(t, open, regOpen+"/nx/tcerr34:v1")
	pushed(t, regProxy+"/nx/tcerr34p:v1", "err34p "+time.Now().String())
	runFrom(t, viaProxy, regProxy+"/nx/tcerr34p:v1")
	pushed(t, regAuth+"/nx/tcerr34a:v1", "err34a "+time.Now().String())
	runFrom(t, private, regAuth+"/nx/tcerr34a:v1")

	// Each is really up to date: the registry's digest is the running one.
	authDir := configDir(t, "tcerr34-auths", authsConfig(regAuth))
	good := ucNexusd(t, "tcerr34-good", authDir).check()
	for _, c := range []string{open, viaProxy, private} {
		wantStatus(t, good.updates, c, spec.UpdateUpToDate, "")
	}

	unknownWith := func(res checkResult, name, reason string, mentions ...string) {
		t.Helper()
		u := wantStatus(t, res.updates, name, spec.UpdateUnknown, reason)
		for _, m := range mentions {
			if !strings.Contains(u.Detail, m) {
				t.Fatalf("%s: detail %q does not mention %q", name, u.Detail, m)
			}
		}
		t.Logf("%s: %s: %s", name, u.Reason, u.Detail)
	}

	// Registry returns 500.
	p.fail.Store(true)
	res := ucNexusd(t, "tcerr34-500", authDir).check()
	unknownWith(res, viaProxy, spec.ReasonRegistryError)
	p.fail.Store(false)

	// Auth required with no credentials.
	res = ucNexusd(t, "tcerr34-noauth", configDir(t, "tcerr34-noauth", "")).check()
	unknownWith(res, private, spec.ReasonAuthRequired, "docker login")

	// A credential helper names the authenticating registry: never run.
	res = ucNexusd(t, "tcerr34-helper", configDir(t, "tcerr34-helper",
		fmt.Sprintf(`{"credHelpers":{%q:"nx"}}`, regAuth))).check()
	unknownWith(res, private, spec.ReasonCredentialHelper, "docker-credential-nx")
	noHelperRun(t)

	// The registry is stopped.
	regCmd(t, "stop-open")
	res = ucNexusd(t, "tcerr34-down", authDir).check()
	unknownWith(res, open, spec.ReasonRegistryError)
	unknownWith(res, viaProxy, spec.ReasonRegistryError)

	// Restored: upToDate again.
	startRegistries(t)
	res = ucNexusd(t, "tcerr34-restored", authDir).check()
	for _, c := range []string{open, viaProxy, private} {
		wantStatus(t, res.updates, c, spec.UpdateUpToDate, "")
	}
}

// TC-ERR35 (FR-7, A-49): with a slow registry, a second check-updates
// while the first runs answers 409 naming the first; both under
// containers:read only.
func TestDockerUpdateSecondCheckConflicts(t *testing.T) {
	startRegistries(t)
	p := startProxy(t)
	name := "tcerr35-slow"
	requireDocker(t, name)
	pushed(t, regProxy+"/nx/tcerr35:v1", "err35 "+time.Now().String())
	runFrom(t, name, regProxy+"/nx/tcerr35:v1")
	n := ucNexusd(t, "tcerr35", configDir(t, "tcerr35", ""))
	p.delay.Store(int64(8 * time.Second))

	first := n.startCheck()
	code, b := n.post(checkAt, "{}")
	if code != http.StatusConflict {
		t.Fatalf("second check-updates: %d %s, want 409", code, b)
	}
	var prob problem.Error
	if err := json.Unmarshal(b, &prob); err != nil || prob.Extensions["blockingOperationId"] != first.ID {
		t.Fatalf("409 body %s: blockingOperationId want %s (%v)", b, first.ID, err)
	}
	if st := n.operation(first.ID).Status; terminal(st) {
		t.Fatalf("the first check had already ended (%s): the conflict was not tested", st)
	}
	res := n.finish(first.ID)
	wantStatus(t, res.updates, name, spec.UpdateUpToDate, "")
	t.Logf("first check took %s", res.op.Ended.Sub(*res.op.Started).Round(time.Millisecond))
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
