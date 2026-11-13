package nginx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// entry is one directory entry of a siteTree: a file with content, a
// symlink, a directory or a FIFO.
type entry struct {
	content string
	link    string
	dir     bool
	fifo    bool
}

func file(content string) entry { return entry{content: content} }

// siteTree lays out entries in dir.
func siteTree(t *testing.T, dir string, entries map[string]entry) {
	t.Helper()
	for name, e := range entries {
		path := filepath.Join(dir, name)
		var err error
		switch {
		case e.link != "":
			err = os.Symlink(e.link, path)
		case e.dir:
			err = os.Mkdir(path, 0o755)
		case e.fifo:
			err = syscall.Mkfifo(path, 0o644)
		default:
			err = os.WriteFile(path, []byte(e.content), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// ownedByTester points the managedUID seam at the test's own UID, so files
// the test creates count as managed (test-strategy.md: T0 is not root).
func ownedByTester(t *testing.T) { setManagedUID(t, uint32(os.Getuid())) }

func setManagedUID(t *testing.T, uid uint32) {
	t.Helper()
	old := managedUID
	managedUID = uid
	t.Cleanup(func() { managedUID = old })
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func runningSystemd() fixtureOptions {
	return fixtureOptions{model: svcctltest.Model{State: svcctl.Running}}
}

// T0 and T1 are the StateChangeTimestamp values of the systemd captures
// show-running.txt and show-failed.txt (internal/svcctl/testdata).
var (
	captureT0 = time.Unix(1794584529, 0)
	captureT1 = time.Unix(1794584518, 0)
)

// statusItem GETs the status and returns its one item, raw and decoded.
func statusItem(t *testing.T, s *server) (map[string]any, spec.Service) {
	t.Helper()
	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	s.getJSON("/providers/nginx/service", http.StatusOK, &raw)
	if len(raw.Items) != 1 {
		t.Fatalf("status has %d items, want 1", len(raw.Items))
	}
	var m struct {
		Properties map[string]any `json:"properties"`
	}
	var svc spec.Service
	if err := json.Unmarshal(raw.Items[0], &m); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw.Items[0], &svc); err != nil {
		t.Fatal(err)
	}
	return m.Properties, svc
}

// nginx-provider TC-63: Status reports the live service state and follows changes
// Owner WI 4.5. Requirements: FR-3, FR-19.
func TestStatusFollowsServiceState(t *testing.T) {
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running, MainPID: 4242}})
	s := startNginx(t, f)

	_, svc := statusItem(t, s)
	if svc.ID != "/providers/nginx/service/nginx" || svc.Name != "nginx" {
		t.Errorf("status item id %q name %q", svc.ID, svc.Name)
	}
	p := svc.Properties
	if p.State != "running" || p.Since == nil || !p.Since.Equal(captureT0) || p.MainPID == nil || *p.MainPID != 4242 {
		t.Errorf("running status = %+v, want running since %v, mainPid 4242", p, captureT0)
	}
	if p.Version != "1.26.3" || p.InitSystem != "systemd" || p.Service != "nginx" || p.IncludeDir != f.dir ||
		p.IncludeSuffix != ".conf" || p.DisabledSuffix != ".disabled" {
		t.Errorf("status properties = %+v", p)
	}

	f.svc.Set(func(m *svcctltest.Model) { m.State, m.MainPID = svcctl.Failed, 0 })
	raw, svc := statusItem(t, s)
	p = svc.Properties
	if p.State != "failed" || p.Since == nil || !p.Since.Equal(captureT1) {
		t.Errorf("failed status = %+v, want failed since %v", p, captureT1)
	}
	if v, ok := raw["mainPid"]; !ok || v != nil {
		t.Errorf("failed status mainPid = %v (present %v), want null", v, ok)
	}
	for _, c := range fakeCalls(t, f.nginx) {
		if c != "-v" {
			t.Errorf("status ran nginx %s; it runs only -v", c)
		}
	}
}

// nginx-provider TC-64: Status succeeds for stopped nginx and on OpenRC with no since
// Owner WI 4.5. Requirements: FR-3.
func TestStatusStoppedAndOpenRC(t *testing.T) {
	f := newFixture(t, fixtureOptions{plat: alpine, model: svcctltest.Model{State: svcctl.Stopped}})
	s := startNginx(t, f)
	raw, svc := statusItem(t, s)
	if svc.Properties.State != "stopped" || svc.Properties.InitSystem != "openrc" {
		t.Errorf("status = %+v, want stopped on openrc", svc.Properties)
	}
	for _, k := range []string{"since", "mainPid"} {
		if v, ok := raw[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
}

// nginx-provider TC-65: The version cache refreshes when the nginx binary changes
// Owner WI 4.5. Requirements: FR-3.
func TestVersionCacheFollowsBinary(t *testing.T) {
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running}, preflight: enabledStatus})
	ctx := context.Background()
	for range 2 {
		l, err := f.p.status(ctx, spec.StatusInput{})
		if err != nil {
			t.Fatal(err)
		}
		if v := l.Items[0].Properties.Version; v != "1.26.3" {
			t.Errorf("version = %q, want 1.26.3", v)
		}
	}
	if n := len(fakeCalls(t, f.nginx)); n != 1 {
		t.Errorf("two status calls ran nginx -v %d times, want 1", n)
	}

	writeFakeNginx(t, f.nginx, "1.27.0", false, "a longer binary")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(f.nginx, later, later); err != nil {
		t.Fatal(err)
	}
	l, err := f.p.status(ctx, spec.StatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if v := l.Items[0].Properties.Version; v != "1.27.0" {
		t.Errorf("after the upgrade version = %q, want 1.27.0", v)
	}
	if n := len(fakeCalls(t, f.nginx)); n != 2 {
		t.Errorf("nginx -v ran %d times in total, want 2", n)
	}
}

// tc66Tree is TC-66's includeDir.
func tc66Tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	big := strings.Repeat("#", int(config.Default().Providers.Nginx.Settings.MaxConfigBytes)+1)
	contents := map[string]string{
		"a.conf": "server { listen 18080; }\n", "b.conf.disabled": "server { listen 18081; }\n",
		"c.conf": "server { listen 18082; }\n", "c.conf.disabled": "server { listen 18083; }\n",
		"bad name.conf": "server {}\n", ".hidden.conf": "x\n", ".x.nexus-01J.tmp": "x\n", "readme.txt": "x\n",
		"g.conf~": "x\n", "big.conf": big,
	}
	tree := map[string]entry{"d.conf": {link: "/etc/hosts"}, "e.conf": {dir: true}, "f.conf": {fifo: true}}
	for n, c := range contents {
		tree[n] = file(c)
	}
	siteTree(t, dir, tree)
	return contents
}

type listPage struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"nextCursor"`
}

func decodeSites(t *testing.T, raw []json.RawMessage) []spec.Site {
	t.Helper()
	out := make([]spec.Site, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal(r, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func siteNames(sites []spec.Site) []string {
	var names []string
	for _, s := range sites {
		names = append(names, s.Name)
	}
	return names
}

// nginx-provider TC-66: List classifies every entry in includeDir
// Owner WI 4.5. Requirements: FR-7, ASM-11.
func TestListClassifiesEntries(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	contents := tc66Tree(t, f.dir)
	s := startNginx(t, f)

	var page listPage
	s.getJSON("/providers/nginx/sites", http.StatusOK, &page)
	sites := decodeSites(t, page.Items)
	if want := []string{"a", "b", "bad name", "big", "c", "d", "e", "f"}; !slices.Equal(siteNames(sites), want) {
		t.Fatalf("names = %q, want %q", siteNames(sites), want)
	}
	by := map[string]spec.SiteProperties{}
	for _, it := range sites {
		by[it.Name] = it.Properties
		if it.ID != "/providers/nginx/sites/"+it.Name || it.Type != "sites" || it.Host != "h1" {
			t.Errorf("%s: id %q type %q host %q", it.Name, it.ID, it.Type, it.Host)
		}
	}

	a := by["a"]
	fi, err := os.Lstat(filepath.Join(f.dir, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Enabled || !a.Managed || a.FileType != "file" || a.FileName != "a.conf" || a.Conflict ||
		a.UnmanagedReason != "" || a.SHA256 == nil || *a.SHA256 != sha(contents["a.conf"]) ||
		a.SizeBytes != fi.Size() || !a.ModTime.Equal(fi.ModTime()) {
		t.Errorf("a = %+v, want enabled managed file with sha, size %d and mtime %v", a, fi.Size(), fi.ModTime())
	}
	if b := by["b"]; b.Enabled || b.FileName != "b.conf.disabled" || !b.Managed || b.SHA256 == nil ||
		*b.SHA256 != sha(contents["b.conf.disabled"]) {
		t.Errorf("b = %+v, want disabled managed b.conf.disabled", b)
	}
	if c := by["c"]; !c.Conflict || c.Managed || c.UnmanagedReason != "conflict" || !c.Enabled ||
		c.FileName != "c.conf" || c.FileType != "file" || c.SHA256 == nil || *c.SHA256 != sha(contents["c.conf"]) {
		t.Errorf("c = %+v, want a conflict with c.conf's properties", c)
	}
	if d := by["d"]; d.FileType != "symlink" || d.Managed || d.UnmanagedReason != "symlink" || d.SHA256 != nil {
		t.Errorf("d = %+v, want an unmanaged symlink without sha256", d)
	}
	if e := by["e"]; e.FileType != "directory" || e.Managed || e.UnmanagedReason != "not-regular" || e.SHA256 != nil {
		t.Errorf("e = %+v, want an unmanaged directory", e)
	}
	if fifo := by["f"]; fifo.FileType != "other" || fifo.Managed || fifo.UnmanagedReason != "not-regular" ||
		fifo.SHA256 != nil {
		t.Errorf("f = %+v, want an unmanaged other", fifo)
	}
	if bad := by["bad name"]; bad.Managed || bad.UnmanagedReason != "invalid-name" {
		t.Errorf("bad name = %+v, want invalid-name", bad)
	}
	if big := by["big"]; !big.Managed || big.SHA256 != nil || big.SizeBytes != int64(len(contents["big.conf"])) {
		t.Errorf("big = %+v, want managed without sha256", big)
	}
}

// nginx-provider TC-67: List paging uses limit and cursor
// Owner WI 4.5. Requirements: FR-7.
func TestListPaging(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	tree := map[string]entry{}
	for i := 1; i <= 5; i++ {
		tree[fmt.Sprintf("s%d.conf", i)] = file(fmt.Sprintf("server { listen %d; }\n", 18090+i))
	}
	siteTree(t, f.dir, tree)
	s := startNginx(t, f)

	var pages [][]string
	cursor := ""
	for range 10 {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page listPage
		s.getJSON("/providers/nginx/sites?"+q.Encode(), http.StatusOK, &page)
		names := siteNames(decodeSites(t, page.Items))
		pages = append(pages, names)
		if page.NextCursor == "" {
			break
		}
		if want := base64.RawURLEncoding.EncodeToString([]byte(names[len(names)-1])); page.NextCursor != want {
			t.Errorf("cursor after %q = %q, want %q", names, page.NextCursor, want)
		}
		cursor = page.NextCursor
	}
	if want := [][]string{{"s1", "s2"}, {"s3", "s4"}, {"s5"}}; !slices.EqualFunc(pages, want, slices.Equal) {
		t.Errorf("pages = %q, want %q", pages, want)
	}
	s.problemOf("/providers/nginx/sites?cursor=!!", http.StatusBadRequest, "validation-failed")
	if _, err := f.p.list(context.Background(), spec.ListInput{Cursor: "!!"}); err == nil {
		t.Error("the handler accepted cursor !!")
	}
}

// nginx-provider TC-68: List reflects a rename between calls
// Owner WI 4.5. Requirements: FR-7.
func TestListFollowsRenames(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	siteTree(t, f.dir, map[string]entry{"a.conf": file("server { listen 18080; }\n")})
	s := startNginx(t, f)
	list := func() spec.SiteProperties {
		t.Helper()
		var page listPage
		s.getJSON("/providers/nginx/sites", http.StatusOK, &page)
		sites := decodeSites(t, page.Items)
		if len(sites) != 1 || sites[0].Name != "a" {
			t.Fatalf("list = %q, want [a]", siteNames(sites))
		}
		return sites[0].Properties
	}
	if p := list(); !p.Enabled || p.FileName != "a.conf" {
		t.Errorf("before = %+v, want enabled a.conf", p)
	}
	disabled := filepath.Join(f.dir, "a.conf.disabled")
	if err := os.Rename(filepath.Join(f.dir, "a.conf"), disabled); err != nil {
		t.Fatal(err)
	}
	if p := list(); p.Enabled || p.FileName != "a.conf.disabled" {
		t.Errorf("after the rename = %+v, want disabled a.conf.disabled", p)
	}
	const next = "server { listen 18081; }\n# rewritten\n"
	if err := os.WriteFile(disabled, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := list(); p.SHA256 == nil || *p.SHA256 != sha(next) {
		t.Errorf("after the rewrite sha256 = %v, want %s", p.SHA256, sha(next))
	}
}

// nginx-provider TC-69: Get returns one config by name
// Owner WI 4.5. Requirements: FR-8.
func TestGetByName(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	tc66Tree(t, f.dir)
	s := startNginx(t, f)
	var page listPage
	s.getJSON("/providers/nginx/sites", http.StatusOK, &page)
	items := map[string]json.RawMessage{}
	for i, it := range decodeSites(t, page.Items) {
		items[it.Name] = page.Items[i]
	}
	for _, name := range []string{"a", "b", "c"} {
		code, body := s.get("/providers/nginx/sites/" + name)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", name, code, body)
		}
		var got, want bytes.Buffer
		if err := json.Compact(&got, body); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&want, items[name]); err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() {
			t.Errorf("GET %s =\n%s\nwant the list item\n%s", name, got.String(), want.String())
		}
	}
}

// nginx-provider TC-70: Content returns text and digest for a managed config
// Owner WI 4.5. Requirements: FR-8, FR-15.
func TestContentReturnsTextAndDigest(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	const a, b = "server { listen 18080; }\n", "server { listen 18081; }\n# off\n"
	siteTree(t, f.dir, map[string]entry{"a.conf": file(a), "b.conf.disabled": file(b)})
	s := startNginx(t, f)
	for _, tc := range []struct{ name, fileName, content string }{
		{"a", "a.conf", a}, {"b", "b.conf.disabled", b},
	} {
		var c spec.SiteContent
		s.getJSON("/providers/nginx/sites/"+tc.name+"/content", http.StatusOK, &c)
		want := spec.SiteContent{ID: "/providers/nginx/sites/" + tc.name, Name: tc.name, FileName: tc.fileName,
			SHA256: sha(tc.content), SizeBytes: int64(len(tc.content)), Content: tc.content}
		if c != want {
			t.Errorf("content of %s = %+v, want %+v", tc.name, c, want)
		}
	}
	if !spec.Content.Op().Untrusted {
		t.Error("nginx.sites.content is not marked Untrusted")
	}
}

// nginx-provider TC-73: Reads meet the latency smoke target and add no background work
// Owner WI 4.5. Requirements: NFR-1, NFR-5.
func TestReadLatencyAndNoBackgroundWork(t *testing.T) {
	ownedByTester(t)
	block := strings.Repeat(strings.Repeat("#", 63)+"\n", 64) // 4 KiB
	ctx := context.Background()

	// Goroutines: the provider alone, before Configure, after Preflight,
	// and after reads and 2 s of idle time.
	before := runtime.NumGoroutine()
	f := newFixture(t, runningSystemd())
	tree := map[string]entry{}
	for i := range 50 {
		tree[fmt.Sprintf("site%02d.conf", i)] = file(block)
	}
	siteTree(t, f.dir, tree)
	if st := f.p.Preflight(ctx); st.State != apitypes.ProviderEnabled {
		t.Fatalf("Preflight = %+v", st)
	}
	afterPreflight := settledGoroutines()
	for range 10 {
		if _, err := f.p.status(ctx, spec.StatusInput{}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.list(ctx, spec.ListInput{Limit: 50}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.get(ctx, spec.SiteInput{Name: "site07"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(2 * time.Second)
	if idle := settledGoroutines(); idle != afterPreflight {
		t.Errorf("goroutines: %d before Configure, %d after Preflight, %d idle; want idle == after Preflight",
			before, afterPreflight, idle)
	}

	// Latency through the real server.
	s := startNginx(t, f)
	for _, path := range []string{"/providers/nginx/service", "/providers/nginx/sites?limit=50",
		"/providers/nginx/sites/site07"} {
		var d []time.Duration
		for range 100 {
			start := time.Now()
			code, body := s.get(path)
			d = append(d, time.Since(start))
			if code != http.StatusOK {
				t.Fatalf("GET %s = %d %s", path, code, body)
			}
		}
		slices.Sort(d)
		if p95 := d[94]; p95 >= 200*time.Millisecond {
			t.Errorf("GET %s p95 = %v, want under 200ms", path, p95)
		}
	}
	s.http.CloseIdleConnections()
	time.Sleep(2 * time.Second)
	if frames := nginxFrames(); len(frames) > 0 {
		t.Errorf("idle goroutines run nginx package code:\n%s", strings.Join(frames, "\n\n"))
	}
}

// settledGoroutines is the goroutine count once it stops changing: os/exec's
// goroutines of a finished nginx -v can still be ending when Run returns
// (seen on Linux), and they are not work the provider left behind.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m == n {
			return n
		}
		n = m
	}
	return n
}

// nginxFrame matches a stack frame's source line in a non-test file of this
// package: test files have an underscore in their name, and spec is a
// subdirectory.
var nginxFrame = regexp.MustCompile(`internal/providers/nginx/[a-z]+\.go:`)

// nginxFrames returns the goroutines that run, or were started by, this
// package's non-test code.
func nginxFrames() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if nginxFrame.MatchString(g) {
			out = append(out, g)
		}
	}
	return out
}

// nginx-provider TC-E16: A hanging nginx -v is bounded and yields an empty version
// Owner WI 4.5. Requirements: FR-3, NFR-2, NFR-4.
func TestVersionHangBounded(t *testing.T) {
	if VersionTimeout != 5*time.Second {
		t.Errorf("VersionTimeout = %v, want 5s", VersionTimeout)
	}
	f := newFixture(t, fixtureOptions{model: svcctltest.Model{State: svcctl.Running}, hangV: true,
		preflight: enabledStatus})
	f.p.versionTimeout = 300 * time.Millisecond
	s := startNginx(t, f)

	start := time.Now()
	_, svc := statusItem(t, s)
	if took := time.Since(start); took >= time.Second {
		t.Errorf("status took %v, want under 1s", took)
	}
	if svc.Properties.Version != "" || svc.Properties.State != "running" {
		t.Errorf("status = %+v, want running with version \"\"", svc.Properties)
	}
	b, err := os.ReadFile(f.nginx + ".pid")
	if err != nil {
		t.Fatalf("the hanging -v never ran: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Errorf("fake nginx %d is still there after the bound: kill 0 = %v", pid, err)
	}
}

// nginx-provider TC-ERR10: Get and content answer not-found for a missing name
// Owner WI 4.5. Requirements: FR-8, FR-17.
func TestGetNotFound(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	siteTree(t, f.dir, map[string]entry{"x.txt": file("x\n"), ".y.conf": file("y\n")})
	s := startNginx(t, f)
	for _, path := range []string{"/sites/zz", "/sites/zz/content", "/sites/x", "/sites/y", "/sites/x/content",
		"/sites/y/content"} {
		s.problemOf("/providers/nginx"+path, http.StatusNotFound, "not-found")
	}
}

// nginx-provider TC-ERR11: Content refuses symlinks, unmanaged entries and oversize files
// Owner WI 4.5. Requirements: FR-8, FR-17, ASM-11.
func TestContentRefusals(t *testing.T) {
	ownedByTester(t)
	f := newFixture(t, runningSystemd())
	const secret = "S-the-target-of-the-symlink"
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	max := int(f.p.Settings().MaxConfigBytes)
	siteTree(t, f.dir, map[string]entry{
		"d.conf": {link: target}, "c.conf": file("c\n"), "c.conf.disabled": file("c off\n"),
		"big.conf": file(strings.Repeat("#", max+1)), "u.conf": file("u\n"),
	})
	s := startNginx(t, f)

	code, body := s.get("/providers/nginx/sites/d/content")
	if strings.Contains(string(body), secret) {
		t.Errorf("the symlink refusal carries its target's content: %s", body)
	}
	if code != http.StatusUnprocessableEntity {
		t.Errorf("symlink content = %d %s, want 422", code, body)
	}
	if p := s.problemOf("/providers/nginx/sites/d/content", 422, "nginx-config-unmanaged"); p.Reason != "symlink" || p.FileName != "d.conf" {
		t.Errorf("symlink problem = %+v", p)
	}
	if p := s.problemOf("/providers/nginx/sites/c/content", 422, "nginx-config-unmanaged"); p.Reason != "conflict" {
		t.Errorf("conflict problem = %+v", p)
	}
	code, body = s.get("/providers/nginx/sites/big/content")
	if len(body) > max {
		t.Errorf("the oversize refusal is %d bytes: it carries content", len(body))
	}
	if code != http.StatusBadRequest || !strings.Contains(string(body), "larger than maxConfigBytes") ||
		!strings.Contains(string(body), "validation-failed") {
		t.Errorf("oversize content = %d %s, want validation-failed naming maxConfigBytes", code, body)
	}

	// The real rule: only a root-owned file is managed. As root, the file
	// is given away so it is not.
	setManagedUID(t, 0)
	if os.Getuid() == 0 {
		if err := os.Lchown(filepath.Join(f.dir, "u.conf"), 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	if p := s.problemOf("/providers/nginx/sites/u/content", 422, "nginx-config-unmanaged"); p.Reason != "not-root-owned" {
		t.Errorf("non-root-owned problem = %+v", p)
	}
}
