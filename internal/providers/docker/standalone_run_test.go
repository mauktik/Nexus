package docker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// CR-31: the standalone update's whole flow, and every step of its
// rollback, against a stateful fake Engine that can fail each call. Each
// case asserts the end state the Engine is left in: the previous container
// under its name and running as it was, the new container gone, and
// rolledBack. T1 (TC-116 to TC-ERR70) runs the same code against a real
// Engine for the health and pull failures.

const (
	runRepo   = "reg.example:5000/team/app"
	runTag    = runRepo + ":latest"
	runOldID  = "oooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooo"
	runOldImg = imgOne
	runNewImg = imgTwo
)

type runCtr struct {
	id, name, image string
	running         bool
	networks        []string
}

type runEngine struct {
	socket string
	mu     sync.Mutex
	ctrs   map[string]*runCtr // By ID
	// failures: call -> HTTP status. Calls: stop, rename, create, connect,
	// start, remove, and start-new (the start of the new container only). A call failing with the "also" mode still takes effect.
	fail map[string]int
	also map[string]bool // The failing call took effect before it failed
	// registry: the digests the registry serves, and the tag's digest.
	known   map[string]bool
	latest  string
	calls   []string
	nextNew int
	// failRenameTo fails a rename to this name only, such as the rename
	// back of a rollback.
	failRenameTo string
}

func newRunEngine(t *testing.T, oldRunning bool, networks ...string) *runEngine {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &runEngine{socket: filepath.Join(dir, "d.sock"), ctrs: map[string]*runCtr{}, fail: map[string]int{},
		also: map[string]bool{}, known: map[string]bool{digestA: true}, latest: digestB}
	f.ctrs[runOldID] = &runCtr{id: runOldID, name: "app", image: runOldImg, running: oldRunning, networks: networks}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *runEngine) find(ref string) *runCtr {
	for _, c := range f.ctrs {
		if c.id == ref || c.name == ref {
			return c
		}
	}
	return nil
}

// state is name -> running, for the containers that exist.
func (f *runEngine) state() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for _, c := range f.ctrs {
		out[c.name] = c.running
	}
	return out
}

// mutations are the recorded calls that change the Engine.
func (f *runEngine) mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return strings.HasPrefix(c, "GET ") })
}

func (c *runCtr) body() map[string]any {
	nets := map[string]any{}
	for _, n := range c.networks {
		nets[n] = map[string]any{"NetworkID": "id-" + n}
	}
	mode := ""
	if len(c.networks) > 0 {
		mode = c.networks[0]
	}
	status := "exited"
	if c.running {
		status = "running"
	}
	return map[string]any{
		"Id": c.id, "Name": "/" + c.name, "Image": c.image,
		"State":           map[string]any{"Status": status, "Running": c.running, "StartedAt": "2026-01-01T00:00:00Z"},
		"Config":          map[string]any{"Image": runTag, "Labels": map[string]string{}},
		"HostConfig":      map[string]any{"NetworkMode": mode},
		"NetworkSettings": map[string]any{"Networks": nets},
	}
}

func (f *runEngine) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		w.Header().Set("Api-Version", "1.52")
		return
	}
	m := versioned.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
		return
	}
	path := m[1]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+path)
	msg := func(status int, text string) {
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"message": text})
	}
	// failed answers an injected failure of call; the call still takes
	// effect first when also[call] is set, which the caller does before.
	failed := func(call string) bool {
		if st := f.fail[call]; st != 0 {
			msg(st, "injected failure of "+call)
			return true
		}
		return false
	}
	rest, isCtr := strings.CutPrefix(path, "/containers/")
	ref, action, _ := strings.Cut(rest, "/")
	switch {
	case path == "/containers/json":
		var out []map[string]any
		for _, c := range f.ctrs {
			out = append(out, map[string]any{"Id": c.id, "Names": []string{"/" + c.name}})
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && isCtr && action == "json":
		c := f.find(ref)
		if c == nil {
			msg(http.StatusNotFound, "No such container: "+ref)
			return
		}
		writeJSON(w, c.body())
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		switch name {
		case runOldImg:
			writeJSON(w, map[string]any{"Id": runOldImg, "RepoDigests": []string{runRepo + "@" + digestA}})
		case runTag:
			writeJSON(w, map[string]any{"Id": runNewImg, "RepoDigests": []string{runRepo + "@" + f.latest}})
		default:
			msg(http.StatusNotFound, "No such image: "+name)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/distribution/"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/distribution/"), "/json")
		answer := ""
		if d, ok := strings.CutPrefix(ref, runRepo+"@"); ok {
			if !f.known[d] {
				msg(http.StatusNotFound, "manifest unknown: manifest unknown")
				return
			}
			answer = d
		} else if ref == runTag {
			answer = f.latest
		} else {
			msg(http.StatusNotFound, "repository does not exist: "+ref)
			return
		}
		writeJSON(w, map[string]any{"Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json",
			"digest": answer, "size": 1}, "Platforms": []any{}})
	case r.Method == http.MethodPost && path == "/images/create":
		_, _ = fmt.Fprintln(w, `{"status":"Status: Downloaded newer image"}`)
	case r.Method == http.MethodPost && path == "/containers/create":
		name := r.URL.Query().Get("name")
		if f.find(name) != nil {
			msg(http.StatusConflict, "Conflict. The container name is already in use")
			return
		}
		f.nextNew++
		c := &runCtr{id: fmt.Sprintf("new%061d", f.nextNew), name: name, image: runNewImg}
		if f.fail["create"] == 0 || f.also["create"] {
			f.ctrs[c.id] = c
		}
		if failed("create") {
			return
		}
		writeJSON(w, map[string]any{"Id": c.id, "Warnings": []string{}})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/networks/") && strings.HasSuffix(path, "/connect"):
		if failed("connect") {
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && isCtr && slices.Contains([]string{"stop", "start", "rename"}, action):
		c := f.find(ref)
		if c == nil {
			msg(http.StatusNotFound, "No such container: "+ref)
			return
		}
		if action == "start" && f.fail["start-new"] != 0 && strings.HasPrefix(c.id, "new") {
			msg(f.fail["start-new"], "injected failure of start")
			return
		}
		if action == "rename" && f.failRenameTo != "" && r.URL.Query().Get("name") == f.failRenameTo {
			msg(http.StatusInternalServerError, "injected failure of rename to "+f.failRenameTo)
			return
		}
		if f.fail[action] == 0 || f.also[action] {
			switch action {
			case "stop":
				c.running = false
			case "start":
				c.running = true
			case "rename":
				c.name = r.URL.Query().Get("name")
			}
		}
		if failed(action) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && isCtr && action == "":
		c := f.find(ref)
		if c == nil {
			msg(http.StatusNotFound, "No such container: "+ref)
			return
		}
		if failed("remove") {
			return
		}
		delete(f.ctrs, c.id)
		w.WriteHeader(http.StatusNoContent)
	default:
		msg(http.StatusNotImplemented, "unexpected call "+r.Method+" "+path)
	}
}

func (f *runEngine) input() helperInput {
	return helperInput{Mode: ModeStandalone, Name: "app", ContainerID: runOldID, OldName: "app-nexus-old-abc123",
		HealthTimeout: 3 * time.Second, CallTimeout: 5 * time.Second, PullTimeout: 10 * time.Second,
		Socket: f.socket, ConfigDir: ""}
}

func runUpdate(t *testing.T, f *runEngine) (helperResult, string) {
	t.Helper()
	var out bytes.Buffer
	res := runStandalone(context.Background(), f.input(), &out)
	return res, out.String()
}

// wantState requires exactly these containers, with these running states.
func (f *runEngine) wantState(t *testing.T, want map[string]bool) {
	t.Helper()
	if got := f.state(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("containers %v, want %v\ncalls: %v", got, want, f.mutations())
	}
}

// Every failing step of an update of a running container is rolled back to
// the state it started in: the previous container named app and running,
// nothing else left (FR-13).
func TestStandaloneUpdateRollsBackEveryFailedStep(t *testing.T) {
	cases := []struct {
		name     string
		networks []string
		fail     string
		took     bool // The failing call took effect before it failed
		slug     problem.Slug
	}{
		{"stop fails and the container keeps running", nil, "stop", false, problem.DependencyUnavailable},
		{"stop fails after the container stopped", nil, "stop", true, problem.DependencyUnavailable},
		{"rename fails", nil, "rename", false, problem.DependencyUnavailable},
		{"create fails", nil, "create", false, problem.DependencyUnavailable},
		{"create fails and leaves a container under the name", nil, "create", true, problem.DependencyUnavailable},
		{"a network connect fails", []string{"net1", "net2"}, "connect", false, problem.DependencyUnavailable},
		{"the new container fails to start", nil, "start-new", false, problem.UpdateHealthFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunEngine(t, true, tc.networks...)
			f.fail[tc.fail], f.also[tc.fail] = http.StatusInternalServerError, tc.took
			res, out := runUpdate(t, f)
			if res.Error == nil || res.Error.Slug != tc.slug || !res.RolledBack || res.Changed {
				t.Fatalf("result %+v %+v\n%s", res, res.Error, out)
			}
			if res.ContainerID != runOldID || res.PreviousContainerID != runOldID {
				t.Errorf("result names container %s (previous %s), want the previous %s", res.ContainerID, res.PreviousContainerID, runOldID)
			}
			f.wantState(t, map[string]bool{"app": true})
			if c := f.find("app"); c == nil || c.id != runOldID {
				t.Errorf("app is not the previous container")
			}
			if !strings.Contains(res.Error.Detail, "previous container was restored") {
				t.Errorf("detail %q", res.Error.Detail)
			}

			// Running the rollback again changes nothing.
			u := &updater{in: f.input(), out: &bytes.Buffer{}, wasRunning: true, stopped: true, renamed: true}
			c, err := newClient(f.socket)
			if err != nil {
				t.Fatal(err)
			}
			u.cli = c
			defer c.Close()
			before := len(f.mutations())
			if err := u.rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			if after := f.mutations(); len(after) != before {
				t.Errorf("a second rollback changed the Engine: %v", after[before:])
			}
			f.wantState(t, map[string]bool{"app": true})
		})
	}
}

// A previous container that cannot be renamed back is left for the user to
// restore by hand, and the result says rolledBack false and how (FR-13).
func TestStandaloneUpdateReportsARollbackThatFails(t *testing.T) {
	f := newRunEngine(t, true)
	f.fail["create"] = http.StatusInternalServerError
	f.failRenameTo = "app"
	res, out := runUpdate(t, f)
	if res.Error == nil || res.RolledBack || res.Changed || !strings.Contains(res.Error.Detail, "the rollback failed") ||
		!strings.Contains(res.Error.Detail, "app-nexus-old-abc123") {
		t.Fatalf("result %+v %+v\n%s", res, res.Error, out)
	}
	// The previous container is where the rename left it: under the old name.
	f.wantState(t, map[string]bool{"app-nexus-old-abc123": false})
}

// A stopped container is replaced and left stopped, its previous container
// is removed, and the new one is connected to the networks beyond the first.
func TestStandaloneUpdateReplacesAStoppedContainer(t *testing.T) {
	f := newRunEngine(t, false, "net1", "net2")
	res, out := runUpdate(t, f)
	if res.Error != nil || !res.Changed || res.RolledBack || res.PreviousDigest != digestA || res.NewDigest != digestB {
		t.Fatalf("result %+v %+v\n%s", res, res.Error, out)
	}
	f.wantState(t, map[string]bool{"app": false})
	if c := f.find("app"); c == nil || c.id == runOldID || res.ContainerID != c.id || res.PreviousContainerID != runOldID {
		t.Errorf("app is %+v, result %+v", c, res)
	}
	want := []string{"POST /images/create", "POST /containers/" + runOldID + "/rename", "POST /containers/create",
		"POST /networks/net2/connect", "DELETE /containers/" + runOldID}
	if got := f.mutations(); !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// CR-2: an image the registry has never served at the running digest is a
// local build, even when the registry serves the tag: the helper refuses
// before it pulls, and changes nothing.
func TestStandaloneUpdateRefusesALocalBuildBehindAServedTag(t *testing.T) {
	f := newRunEngine(t, true)
	f.known = map[string]bool{}
	res, out := runUpdate(t, f)
	if res.Error == nil || res.Error.Slug != problem.NotUpdatable || res.Changed || res.RolledBack ||
		!strings.Contains(res.Error.Detail, "built or retagged on this host") || !strings.Contains(res.Error.Detail, "nothing was changed") {
		t.Fatalf("result %+v %+v\n%s", res, res.Error, out)
	}
	if got := f.mutations(); len(got) != 0 {
		t.Errorf("the helper changed the Engine: %v", got)
	}
	f.wantState(t, map[string]bool{"app": true})

}
