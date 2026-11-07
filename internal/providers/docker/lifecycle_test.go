package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/moby/moby/api/types/container"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// lifecycleEngine serves the Engine API calls of the lifecycle actions in
// the shapes and status codes of Engine API v1.52: POST
// /containers/{id}/start (204, 304 already started, 404), /stop (204, 304
// already stopped, 404, query t), /restart (204, 404, query t), DELETE
// /containers/{id} (204, 404, 409 running, query v) and DELETE
// /volumes/{name} (204, 404, 409 in use). It records each call as
// "METHOD path?query". T1 (TC-109, TC-110, TC-E19, TC-ERR33) checks the
// same code against a real daemon.
type lifecycleEngine struct {
	socket     string
	mu         sync.Mutex
	calls      []string
	containers map[string]map[string]any // Name -> inspect body
	volumes    map[string]int            // Volume -> status DELETE answers
}

func newLifecycleEngine(t *testing.T) *lifecycleEngine {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &lifecycleEngine{socket: filepath.Join(dir, "d.sock"), containers: map[string]map[string]any{}, volumes: map[string]int{}}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *lifecycleEngine) add(name, id string, running bool, labels map[string]string, mounts []map[string]any) {
	status := "exited"
	if running {
		status = "running"
	}
	f.containers[name] = map[string]any{
		"Id": id, "Name": "/" + name,
		"State":  map[string]any{"Status": status, "Running": running, "StartedAt": "2026-01-01T00:00:00Z"},
		"Config": map[string]any{"Image": "busybox:latest", "Labels": labels},
		"Mounts": mounts,
	}
}

func (f *lifecycleEngine) byRef(ref string) (map[string]any, bool) {
	for name, c := range f.containers {
		if name == ref || c["Id"] == ref {
			return c, true
		}
	}
	return nil, false
}

func (f *lifecycleEngine) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// mutations are the recorded calls other than inspections.
func (f *lifecycleEngine) mutations() []string {
	return slices.DeleteFunc(f.log(), func(c string) bool { return strings.HasPrefix(c, "GET ") })
}

func (f *lifecycleEngine) serve(w http.ResponseWriter, r *http.Request) {
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
	call := r.Method + " " + path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	noSuch := func() {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"message": "No such container: x"})
	}
	rest, isContainer := strings.CutPrefix(path, "/containers/")
	ref, action, _ := strings.Cut(rest, "/")
	switch {
	case r.Method == http.MethodGet && isContainer && action == "json":
		c, ok := f.byRef(ref)
		if !ok {
			noSuch()
			return
		}
		writeJSON(w, c)
	case r.Method == http.MethodPost && isContainer && slices.Contains([]string{"start", "stop", "restart"}, action):
		if _, ok := f.byRef(ref); !ok {
			noSuch()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && isContainer && action == "":
		c, ok := f.byRef(ref)
		if !ok {
			noSuch()
			return
		}
		if c["State"].(map[string]any)["Running"] == true {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]string{"message": "You cannot remove a running container"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/volumes/"):
		status, ok := f.volumes[strings.TrimPrefix(path, "/volumes/")]
		if !ok {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			writeJSON(w, map[string]string{"message": "volume"})
		}
	default:
		http.Error(w, `{"message":"unexpected call"}`, http.StatusNotImplemented)
	}
}

func volumeMount(name string) map[string]any {
	return map[string]any{"Type": "volume", "Name": name, "Destination": "/data/" + name}
}

func newRun() (*provider.Run, *bytes.Buffer) {
	var b bytes.Buffer
	return provider.NewRun("op-1", &b), &b
}

func intp(n int) *int { return &n }

// A-56: a Compose-labeled container's lane is its project, a standalone
// container's its name, both reject; an ID resolves to the same lane as the
// name; an absent container is not-found at admission and an invalid name
// never reaches the daemon.
func TestContainerLane(t *testing.T) {
	f := newLifecycleEngine(t)
	f.add("web", "aaa111", true, nil, nil)
	f.add("media-app-1", "bbb222", true, map[string]string{spec.ComposeProjectLabel: "media", spec.ComposeServiceLabel: "app"}, nil)
	f.add("media-db-1", "ccc333", false, map[string]string{spec.ComposeProjectLabel: "media", spec.ComposeServiceLabel: "db"}, nil)
	p := newProvider(t, f.socket, 5*time.Second)
	ctx := context.Background()

	for ref, want := range map[string]provider.Lane{
		"web":         {Key: "container:web", Policy: provider.LaneReject},
		"aaa111":      {Key: "container:web", Policy: provider.LaneReject},
		"media-app-1": {Key: "compose:media", Policy: provider.LaneReject},
		"media-db-1":  {Key: "compose:media", Policy: provider.LaneReject},
	} {
		got, err := p.containerLane(ctx, ref)
		if err != nil || got != want {
			t.Errorf("lane of %s = %+v, %v; want %+v", ref, got, err, want)
		}
	}
	if _, err := p.containerLane(ctx, "nope"); slugOf(err) != problem.NotFound {
		t.Errorf("unknown container: %v, want not-found", err)
	}
	before := len(f.log())
	if _, err := p.containerLane(ctx, "-x"); slugOf(err) != problem.ValidationFailed {
		t.Errorf("invalid name: %v, want validation-failed", err)
	}
	if len(f.log()) != before {
		t.Errorf("an invalid name reached the daemon: %q", f.log()[before:])
	}

	// Every lifecycle binding resolves its lane with the same function.
	bs := map[string]provider.Binding{}
	for _, b := range p.Bindings() {
		bs[b.Op().ID] = b
	}
	for id, in := range map[string]any{
		spec.Start.Op().ID:   spec.ContainerInput{Name: "media-app-1"},
		spec.Stop.Op().ID:    spec.StopInput{Name: "media-app-1"},
		spec.Restart.Op().ID: spec.StopInput{Name: "media-app-1"},
		spec.Remove.Op().ID:  spec.RemoveInput{Name: "media-app-1"},
	} {
		funcs, ok := bs[id].InProcess()
		if !ok {
			t.Errorf("%s is not bound in-process", id)
			continue
		}
		if got, err := funcs.Lane(ctx, in); err != nil || got.Key != "compose:media" || got.Policy != provider.LaneReject {
			t.Errorf("%s lane = %+v, %v", id, got, err)
		}
	}
}

// TC-E19 (T0 half): start on a running container and stop on a stopped one
// send no state-changing call and report changed false.
func TestIdempotentTargetStatesSendNothing(t *testing.T) {
	f := newLifecycleEngine(t)
	f.add("up", "aaa111", true, nil, nil)
	f.add("down", "bbb222", false, nil, nil)
	p := newProvider(t, f.socket, 5*time.Second)
	ctx := context.Background()

	r, _ := newRun()
	res, err := p.start(ctx, spec.ContainerInput{Name: "up"}, r)
	if err != nil || res.Changed || res.ContainerID != "aaa111" {
		t.Errorf("start running: %+v, %v", res, err)
	}
	res, err = p.stop(ctx, spec.StopInput{Name: "down", TimeoutSeconds: intp(3)}, r)
	if err != nil || res.Changed || res.ContainerID != "bbb222" {
		t.Errorf("stop stopped: %+v, %v", res, err)
	}
	if got := f.mutations(); len(got) != 0 {
		t.Errorf("state-changing calls sent: %q", got)
	}
}

// TC-109 (T0 half): each action reaches the Engine by container ID, with
// the timeout as t only when given, and reports changed true.
func TestLifecycleCallsReachTheEngine(t *testing.T) {
	f := newLifecycleEngine(t)
	f.add("up", "aaa111", true, nil, nil)
	f.add("down", "bbb222", false, nil, nil)
	p := newProvider(t, f.socket, 5*time.Second)
	ctx := context.Background()

	r, out := newRun()
	steps := []struct {
		name string
		run  func() (spec.ActionResult, error)
		want string
	}{
		{"stop t=7", func() (spec.ActionResult, error) {
			return p.stop(ctx, spec.StopInput{Name: "up", TimeoutSeconds: intp(7)}, r)
		},
			"POST /containers/aaa111/stop?t=7"},
		{"stop t=0", func() (spec.ActionResult, error) {
			return p.stop(ctx, spec.StopInput{Name: "up", TimeoutSeconds: intp(0)}, r)
		},
			"POST /containers/aaa111/stop?t=0"},
		{"stop default", func() (spec.ActionResult, error) { return p.stop(ctx, spec.StopInput{Name: "up"}, r) },
			"POST /containers/aaa111/stop"},
		{"start", func() (spec.ActionResult, error) { return p.start(ctx, spec.ContainerInput{Name: "down"}, r) },
			"POST /containers/bbb222/start"},
		{"restart t=2", func() (spec.ActionResult, error) {
			return p.restart(ctx, spec.StopInput{Name: "down", TimeoutSeconds: intp(2)}, r)
		},
			"POST /containers/bbb222/restart?t=2"},
		{"restart default", func() (spec.ActionResult, error) { return p.restart(ctx, spec.StopInput{Name: "up"}, r) },
			"POST /containers/aaa111/restart"},
	}
	for _, s := range steps {
		before := len(f.mutations())
		res, err := s.run()
		if err != nil || !res.Changed {
			t.Errorf("%s: %+v, %v", s.name, res, err)
		}
		if got := f.mutations()[before:]; !slices.Equal(got, []string{s.want}) {
			t.Errorf("%s: sent %q, want %q", s.name, got, s.want)
		}
	}
	if !strings.Contains(out.String(), "stopping container up") || !strings.Contains(out.String(), "started container down") {
		t.Errorf("progress output:\n%s", out)
	}

	if _, err := p.restart(ctx, spec.StopInput{Name: "gone"}, r); slugOf(err) != problem.NotFound {
		t.Errorf("restart unknown: %v, want not-found", err)
	}
}

// The call deadline of stop and restart covers the stop timeout: the
// Engine answers only after it, so a 30-second stop must not fail at the
// call timeout.
func TestStopWait(t *testing.T) {
	c := func(own *int) containerWith { return containerWith{own: own} }
	for _, tc := range []struct {
		requested *int
		own       containerWith
		want      time.Duration
	}{
		{nil, c(nil), 10 * time.Second},
		{intp(30), c(intp(5)), 30 * time.Second},
		{intp(0), c(nil), 0},
		{nil, c(intp(45)), 45 * time.Second},
		{nil, c(intp(-1)), spec.MaxTimeoutSeconds * time.Second},
	} {
		if got := stopWait(tc.requested, tc.own.inspect()); got != tc.want {
			t.Errorf("stopWait(%v, own %v) = %s, want %s", tc.requested, tc.own.own, got, tc.want)
		}
	}
}

// TC-110 (T0 half): remove keeps volumes by default; with removeVolumes it
// sends v=1, then deletes each volume mount, counting not-found as removed
// and turning in-use into a warning; a Compose container's result warns of
// the next deploy; a running container is a conflict and is never deleted.
func TestRemove(t *testing.T) {
	f := newLifecycleEngine(t)
	f.add("ca", "aaa111", false, nil, []map[string]any{volumeMount("a-data"),
		{"Type": "bind", "Source": "/srv/a", "Destination": "/srv"}})
	f.add("cb", "bbb222", false, nil, []map[string]any{volumeMount("b-data"), volumeMount("0123abcd")})
	f.add("cd", "ddd444", false, nil, []map[string]any{volumeMount("shared")})
	f.add("cc", "ccc333", false, map[string]string{spec.ComposeProjectLabel: "media", spec.ComposeServiceLabel: "app"}, nil)
	f.add("run", "eee555", true, nil, nil)
	f.volumes["b-data"] = http.StatusNoContent // 0123abcd is anonymous: v=1 already deleted it
	f.volumes["shared"] = http.StatusConflict
	p := newProvider(t, f.socket, 5*time.Second)
	ctx := context.Background()
	r, _ := newRun()

	res, err := p.remove(ctx, spec.RemoveInput{Name: "ca"}, r)
	if err != nil || !res.Changed || len(res.RemovedVolumes) != 0 || len(res.Warnings) != 0 {
		t.Errorf("remove a: %+v, %v", res, err)
	}
	if got := f.mutations(); !slices.Equal(got, []string{"DELETE /containers/aaa111"}) {
		t.Errorf("remove a sent %q: volumes must be kept by default", got)
	}

	before := len(f.mutations())
	res, err = p.remove(ctx, spec.RemoveInput{Name: "cb", RemoveVolumes: true}, r)
	if err != nil || !res.Changed || !slices.Equal(res.RemovedVolumes, []string{"0123abcd", "b-data"}) || len(res.Warnings) != 0 {
		t.Errorf("remove b: %+v, %v", res, err)
	}
	if got, want := f.mutations()[before:], []string{"DELETE /containers/bbb222?v=1",
		"DELETE /volumes/0123abcd", "DELETE /volumes/b-data"}; !slices.Equal(got, want) {
		t.Errorf("remove b sent %q, want %q", got, want)
	}

	res, err = p.remove(ctx, spec.RemoveInput{Name: "cd", RemoveVolumes: true}, r)
	if err != nil || !res.Changed || len(res.RemovedVolumes) != 0 || len(res.Warnings) != 1 ||
		!strings.Contains(res.Warnings[0], "shared") || !strings.Contains(res.Warnings[0], "in use") {
		t.Errorf("remove d: %+v, %v", res, err)
	}

	res, err = p.remove(ctx, spec.RemoveInput{Name: "cc"}, r)
	if err != nil || !res.Changed || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "next Compose deploy") ||
		!strings.Contains(res.Warnings[0], `"media"`) {
		t.Errorf("remove c: %+v, %v", res, err)
	}

	before = len(f.mutations())
	if _, err := p.remove(ctx, spec.RemoveInput{Name: "run"}, r); slugOf(err) != problem.Conflict {
		t.Errorf("remove running: %v, want conflict", err)
	}
	if got := f.mutations()[before:]; len(got) != 0 {
		t.Errorf("remove running sent %q", got)
	}
}

func TestVolumeNames(t *testing.T) {
	got := volumeNames(nil)
	if len(got) != 0 {
		t.Errorf("no mounts: %q", got)
	}
	got = volumeNames(inspectMounts(t, `[{"Type":"volume","Name":"z"},{"Type":"bind","Source":"/x"},`+
		`{"Type":"tmpfs"},{"Type":"volume","Name":"a"},{"Type":"volume","Name":"z"}]`))
	if !slices.Equal(got, []string{"a", "z"}) {
		t.Errorf("volumeNames = %q", got)
	}
}

// The declarations follow the catalog table: resource actions under
// containers:write, long-running, start and stop idempotent, remove
// destructive; each states its side effects; timeoutSeconds is bounded
// 0..3600 so a negative one is a 400 before any call.
func TestLifecycleDeclarations(t *testing.T) {
	for _, tc := range []struct {
		op         def.Op
		action     string
		risk       def.Risk
		idempotent bool
		tool       string
	}{
		{spec.Start.Op(), "start", def.RiskMutating, true, "start_container"},
		{spec.Stop.Op(), "stop", def.RiskMutating, true, "stop_container"},
		{spec.Restart.Op(), "restart", def.RiskMutating, false, "restart_container"},
		{spec.Remove.Op(), "remove", def.RiskDestructive, false, "remove_container"},
	} {
		op := tc.op
		if op.ID != "docker.containers."+tc.action || op.Kind != def.KindResourceAction || op.Action != tc.action ||
			op.ResourceType != spec.ResourceType || op.Scope.Name != spec.WriteScope || op.Risk != tc.risk ||
			op.Mode != def.ModeLongRunning || op.Idempotent != tc.idempotent || op.WhatIf ||
			op.CLI != (def.CLIBinding{Noun: "container", Verb: tc.action, Positional: "name"}) ||
			op.MCP == nil || op.MCP.Tool != tc.tool {
			t.Errorf("%s: declaration %+v", op.ID, op)
		}
		if !strings.Contains(op.Description, "Side effects:") || !strings.Contains(op.Description, "Long-running") ||
			!strings.Contains(op.Description, "409") {
			t.Errorf("%s: description %q", op.ID, op.Description)
		}
		if !slices.ContainsFunc(spec.Spec().Ops, func(o def.Op) bool { return o.ID == op.ID }) {
			t.Errorf("%s is not in Spec()", op.ID)
		}
	}

	for _, op := range []def.Op{spec.Stop.Op(), spec.Restart.Op()} {
		s, err := op.InputSchema()
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range []string{`{"name":"web","timeoutSeconds":-1}`, `{"name":"web","timeoutSeconds":3601}`,
			`{"name":"-web"}`, `{"name":"web","timeoutSeconds":"5"}`} {
			if err := validateJSON(t, s, in); err == nil {
				t.Errorf("%s accepted %s", op.ID, in)
			}
		}
		for _, in := range []string{`{"name":"web"}`, `{"name":"web","timeoutSeconds":0}`, `{"name":"web","timeoutSeconds":3600}`} {
			if err := validateJSON(t, s, in); err != nil {
				t.Errorf("%s refused %s: %v", op.ID, in, err)
			}
		}
	}
	s, err := spec.Remove.Op().InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJSON(t, s, `{"name":"web","removeVolumes":"yes"}`); err == nil {
		t.Error("remove accepted a non-boolean removeVolumes")
	}
	if err := validateJSON(t, s, `{"name":"web","removeVolumes":true}`); err != nil {
		t.Errorf("remove refused removeVolumes: %v", err)
	}
}

// containerWith is an inspected container with its own stop timeout.
type containerWith struct{ own *int }

func (c containerWith) inspect() container.InspectResponse {
	return container.InspectResponse{Config: &container.Config{StopTimeout: c.own}}
}

func inspectMounts(t *testing.T, js string) []container.MountPoint {
	t.Helper()
	var out []container.MountPoint
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func validateJSON(t *testing.T, s *jsonschema.Resolved, js string) error {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatal(err)
	}
	return s.Validate(v)
}
