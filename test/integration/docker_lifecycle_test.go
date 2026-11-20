// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// WI 6.2 against the real nested Docker Engine: every state is read back
// with `docker inspect` in the test, never through Nexus.

// lcTrapCmd stops promptly on SIGTERM; a bare `sleep` as PID 1 ignores
// SIGTERM, so Docker has to wait out the timeout and kill it.
var (
	lcTrapCmd     = []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 0.2; done"}
	lcStubbornCmd = []string{"sleep", "3600"}
)

// lcNexusd is a real nexusd whose token also holds containers:write. The
// token store is re-read on change, so the token added after the start is
// accepted.
func lcNexusd(t *testing.T, name string) *nexusdInstance {
	t.Helper()
	n := newNexusd(t, name, dockerSection(dockerSocket))
	tok, _, err := auth.Create(filepath.Join(nexusdState, "nexusd-"+name), auth.CreateRequest{Name: "lifecycle",
		Scopes: []string{spec.ReadScope, spec.WriteScope}})
	if err != nil {
		t.Fatal(err)
	}
	n.token = tok
	return n
}

func (n *nexusdInstance) post(path, body string) (int, []byte) {
	n.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+n.listen+path, strings.NewReader(body))
	if err != nil {
		n.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		n.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		n.t.Fatal(err)
	}
	return resp.StatusCode, b
}

// act posts a lifecycle action and returns the status and body.
func (n *nexusdInstance) act(name, action, body string) (int, []byte) {
	n.t.Helper()
	if body == "" {
		body = "{}"
	}
	return n.post(containersAt+"/"+name+"/"+action, body)
}

// startAction posts a lifecycle action and requires 202.
func (n *nexusdInstance) startAction(name, action, body string) apitypes.Operation {
	n.t.Helper()
	code, b := n.act(name, action, body)
	if code != http.StatusAccepted {
		n.t.Fatalf("%s %s: %d %s", action, name, code, b)
	}
	var op apitypes.Operation
	if err := json.Unmarshal(b, &op); err != nil {
		n.t.Fatal(err)
	}
	return op
}

func (n *nexusdInstance) operation(id string) apitypes.Operation {
	n.t.Helper()
	var op apitypes.Operation
	n.mustGet("/v1/operations/"+id, &op)
	return op
}

func (n *nexusdInstance) waitOp(id string, timeout time.Duration) apitypes.Operation {
	n.t.Helper()
	var op apitypes.Operation
	waitUntil(n.t, timeout, id+" to end", func() (bool, string) {
		op = n.operation(id)
		return terminal(op.Status), string(op.Status)
	})
	return op
}

// lcResult is the result of every lifecycle action, remove's fields
// included.
type lcResult struct {
	Changed        bool     `json:"changed"`
	Warnings       []string `json:"warnings"`
	ContainerID    string   `json:"containerId"`
	RemovedVolumes []string `json:"removedVolumes"`
}

// do runs a lifecycle action to its end and requires Succeeded.
func (n *nexusdInstance) do(name, action, body string) (lcResult, apitypes.Operation) {
	n.t.Helper()
	op := n.waitOp(n.startAction(name, action, body).ID, 2*time.Minute)
	if op.Status != apitypes.StatusSucceeded {
		n.t.Fatalf("%s %s: %s %+v", action, name, op.Status, op.Error)
	}
	var res lcResult
	if err := json.Unmarshal(op.Result, &res); err != nil {
		n.t.Fatalf("%s %s result %s: %v", action, name, op.Result, err)
	}
	n.t.Logf("%s %s: %s in %s, result %s", action, name, op.Status, op.Ended.Sub(*op.Started).Round(time.Millisecond), op.Result)
	return res, op
}

// lcState is a container's State read with docker inspect.
type lcState struct {
	Status     string
	Running    bool
	StartedAt  time.Time
	FinishedAt time.Time
}

func lcSame(a, b lcState) bool {
	return a.Status == b.Status && a.Running == b.Running && a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt)
}

func lcInspect(t *testing.T, name string) lcState {
	t.Helper()
	var out []struct{ State lcState }
	if err := json.Unmarshal([]byte(dockerCLI(t, "inspect", "--type", "container", name)), &out); err != nil || len(out) != 1 {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	return out[0].State
}

func lcRun(t *testing.T, name string, args []string, cmd []string) {
	t.Helper()
	dockerCLI(t, slices.Concat([]string{"run", "-d", "--name", name}, args, []string{testImage}, cmd)...)
}

func lcGone(t *testing.T, kind, name string) bool {
	t.Helper()
	_, err := sh(time.Minute, "docker", kind, "inspect", name)
	return err != nil
}

// TC-109: stop with timeoutSeconds, start, then restart with
// timeoutSeconds, each read back by docker inspect: Running and StartedAt
// follow the command and each result reports changed. On a container that
// ignores SIGTERM, stop and restart take about the given timeout.
func TestDockerLifecycleChangesState(t *testing.T) {
	requireDocker(t, "tc109", "tc109-stubborn")
	lcRun(t, "tc109", nil, lcTrapCmd)
	lcRun(t, "tc109-stubborn", nil, lcStubbornCmd)
	n := lcNexusd(t, "tc109")

	s0 := lcInspect(t, "tc109")
	if !s0.Running {
		t.Fatalf("tc109 before: %+v", s0)
	}
	res, op := n.do("tc109", "stop", `{"timeoutSeconds":10}`)
	s1 := lcInspect(t, "tc109")
	t.Logf("after stop: %+v", s1)
	if !res.Changed || s1.Running || !s1.FinishedAt.After(s0.StartedAt) || !s1.StartedAt.Equal(s0.StartedAt) {
		t.Fatalf("stop: result %+v, state %+v", res, s1)
	}
	// It honors SIGTERM, so it did not wait for the 10 seconds.
	if d := op.Ended.Sub(*op.Started); d > 8*time.Second {
		t.Errorf("stop of a SIGTERM-honoring container took %s", d)
	}

	res, _ = n.do("tc109", "start", "")
	s2 := lcInspect(t, "tc109")
	t.Logf("after start: %+v", s2)
	if !res.Changed || !s2.Running || !s2.StartedAt.After(s1.FinishedAt) {
		t.Fatalf("start: result %+v, state %+v", res, s2)
	}

	res, _ = n.do("tc109", "restart", `{"timeoutSeconds":10}`)
	s3 := lcInspect(t, "tc109")
	t.Logf("after restart: %+v", s3)
	if !res.Changed || !s3.Running || !s3.StartedAt.After(s2.StartedAt) {
		t.Fatalf("restart: result %+v, state %+v", res, s3)
	}

	// SIGTERM ignored: Docker waits the timeout, then kills.
	const timeout = 4 * time.Second
	b0 := lcInspect(t, "tc109-stubborn")
	res, op = n.do("tc109-stubborn", "stop", `{"timeoutSeconds":4}`)
	b1 := lcInspect(t, "tc109-stubborn")
	d := op.Ended.Sub(*op.Started)
	t.Logf("stubborn stop: %s, state %+v", d, b1)
	if !res.Changed || b1.Running || d < timeout-500*time.Millisecond || d > timeout+10*time.Second {
		t.Fatalf("stubborn stop: result %+v in %s, state %+v (before %+v)", res, d, b1, b0)
	}
	n.do("tc109-stubborn", "start", "")
	b2 := lcInspect(t, "tc109-stubborn")
	res, op = n.do("tc109-stubborn", "restart", `{"timeoutSeconds":4}`)
	b3 := lcInspect(t, "tc109-stubborn")
	d = op.Ended.Sub(*op.Started)
	t.Logf("stubborn restart: %s, state %+v", d, b3)
	if !res.Changed || !b3.Running || !b3.StartedAt.After(b2.StartedAt) ||
		d < timeout-500*time.Millisecond || d > timeout+10*time.Second {
		t.Fatalf("stubborn restart: result %+v in %s, state %+v (before %+v)", res, d, b3, b2)
	}
}

// TC-E19: start on a running container and stop on a stopped one succeed
// with changed false and leave StartedAt and FinishedAt as they were.
func TestDockerLifecycleIdempotentTargetStates(t *testing.T) {
	requireDocker(t, "tc-e19-up", "tc-e19-down")
	lcRun(t, "tc-e19-up", nil, lcTrapCmd)
	lcRun(t, "tc-e19-down", nil, lcTrapCmd)
	dockerCLI(t, "stop", "tc-e19-down")
	n := lcNexusd(t, "tce19")

	up0, down0 := lcInspect(t, "tc-e19-up"), lcInspect(t, "tc-e19-down")
	t.Logf("before: up %+v, down %+v", up0, down0)
	if !up0.Running || down0.Running || down0.FinishedAt.IsZero() {
		t.Fatalf("setup: up %+v, down %+v", up0, down0)
	}
	if res, _ := n.do("tc-e19-up", "start", ""); res.Changed {
		t.Errorf("start on running: %+v", res)
	}
	if res, _ := n.do("tc-e19-down", "stop", `{"timeoutSeconds":5}`); res.Changed {
		t.Errorf("stop on stopped: %+v", res)
	}
	up1, down1 := lcInspect(t, "tc-e19-up"), lcInspect(t, "tc-e19-down")
	t.Logf("after: up %+v, down %+v", up1, down1)
	if !lcSame(up0, up1) || !lcSame(down0, down1) {
		t.Fatalf("state changed: up %+v -> %+v, down %+v -> %+v", up0, up1, down0, down1)
	}
}

func lcVolumes(t *testing.T, name string) (named, anonymous []string) {
	t.Helper()
	var out []struct {
		Mounts []struct{ Type, Name, Destination string }
	}
	if err := json.Unmarshal([]byte(dockerCLI(t, "inspect", "--type", "container", name)), &out); err != nil || len(out) != 1 {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	for _, m := range out[0].Mounts {
		if m.Type != "volume" {
			continue
		}
		if m.Destination == "/anon" {
			anonymous = append(anonymous, m.Name)
		} else {
			named = append(named, m.Name)
		}
	}
	return named, anonymous
}

// TC-110: remove keeps named volumes unless removeVolumes; with it, named
// and anonymous volumes go, except one another running container uses,
// which is a warning; a Compose-labeled container's result names the next
// Compose deploy. The control shows plain `docker rm -v` keeps a named
// volume (S-5), which is why removeVolumes deletes named ones itself.
func TestDockerRemovePreservesNamedVolumes(t *testing.T) {
	vols := []string{"tc110-a-vol", "tc110-b-vol", "tc110-shared", "tc110-ctl-vol"}
	t.Cleanup(func() {
		for _, v := range vols {
			_, _ = sh(time.Minute, "docker", "volume", "rm", "-f", v)
		}
	})
	requireDocker(t, "tc110-a", "tc110-b", "tc110-d", "tc110-e", "tc110-c", "tc110-ctl")
	for _, v := range vols {
		_, _ = sh(time.Minute, "docker", "volume", "rm", "-f", v)
	}

	// Control (S-5).
	dockerCLI(t, "create", "--name", "tc110-ctl", "-v", "tc110-ctl-vol:/data", "-v", "/anon", testImage, "true")
	_, ctlAnon := lcVolumes(t, "tc110-ctl")
	dockerCLI(t, "rm", "-v", "tc110-ctl")
	if lcGone(t, "volume", "tc110-ctl-vol") || len(ctlAnon) != 1 || !lcGone(t, "volume", ctlAnon[0]) {
		t.Fatalf("control: docker rm -v did not keep the named volume and delete the anonymous one %v", ctlAnon)
	}
	t.Logf("control: docker rm -v kept tc110-ctl-vol and deleted anonymous %s", ctlAnon[0])

	dockerCLI(t, "create", "--name", "tc110-a", "-v", "tc110-a-vol:/data", testImage, "true")
	dockerCLI(t, "create", "--name", "tc110-b", "-v", "tc110-b-vol:/data", "-v", "/anon", testImage, "true")
	dockerCLI(t, "create", "--name", "tc110-d", "-v", "tc110-shared:/data", testImage, "true")
	lcRun(t, "tc110-e", []string{"-v", "tc110-shared:/data"}, lcTrapCmd)
	dockerCLI(t, "create", "--name", "tc110-c", "--label", spec.ComposeProjectLabel+"=tc110proj",
		"--label", spec.ComposeServiceLabel+"=app", testImage, "true")
	_, bAnon := lcVolumes(t, "tc110-b")
	if len(bAnon) != 1 {
		t.Fatalf("tc110-b anonymous volumes %v", bAnon)
	}
	n := lcNexusd(t, "tc110")

	res, _ := n.do("tc110-a", "remove", "")
	if !res.Changed || !lcGone(t, "container", "tc110-a") || lcGone(t, "volume", "tc110-a-vol") || len(res.RemovedVolumes) != 0 {
		t.Fatalf("remove A: %+v; container gone %v, volume gone %v", res,
			lcGone(t, "container", "tc110-a"), lcGone(t, "volume", "tc110-a-vol"))
	}

	res, _ = n.do("tc110-b", "remove", `{"removeVolumes":true}`)
	if !res.Changed || !lcGone(t, "container", "tc110-b") || !lcGone(t, "volume", "tc110-b-vol") || !lcGone(t, "volume", bAnon[0]) ||
		!slices.Contains(res.RemovedVolumes, "tc110-b-vol") || !slices.Contains(res.RemovedVolumes, bAnon[0]) || len(res.Warnings) != 0 {
		t.Fatalf("remove B: %+v", res)
	}

	res, _ = n.do("tc110-d", "remove", `{"removeVolumes":true}`)
	if !res.Changed || !lcGone(t, "container", "tc110-d") || lcGone(t, "volume", "tc110-shared") ||
		len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "tc110-shared") || !strings.Contains(res.Warnings[0], "in use") {
		t.Fatalf("remove D: %+v; volume gone %v", res, lcGone(t, "volume", "tc110-shared"))
	}
	if !lcInspect(t, "tc110-e").Running {
		t.Fatal("tc110-e stopped")
	}

	res, _ = n.do("tc110-c", "remove", "")
	if !res.Changed || !lcGone(t, "container", "tc110-c") || len(res.Warnings) != 1 ||
		!strings.Contains(res.Warnings[0], "next Compose deploy") || !strings.Contains(res.Warnings[0], "tc110proj") {
		t.Fatalf("remove C: %+v", res)
	}
}

func lcProblem(t *testing.T, code int, b []byte) problem.Error {
	t.Helper()
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%d, body is not a problem: %s", code, b)
	}
	return p
}

// TC-ERR33: while a slow stop runs, a restart of the same container is 409
// naming it, and a stop of another container proceeds; a negative timeout
// is 400 and an unknown container 404 not-found.
func TestDockerLifecycleConflictsAndBadInput(t *testing.T) {
	requireDocker(t, "tc-err33-slow", "tc-err33-other")
	lcRun(t, "tc-err33-slow", nil, lcStubbornCmd)
	lcRun(t, "tc-err33-other", nil, lcTrapCmd)
	n := lcNexusd(t, "tcerr33")

	slow := n.startAction("tc-err33-slow", "stop", `{"timeoutSeconds":15}`)
	waitUntil(t, 10*time.Second, "the slow stop to run", func() (bool, string) {
		s := n.operation(slow.ID).Status
		return s == apitypes.StatusRunning, string(s)
	})
	code, b := n.act("tc-err33-slow", "restart", `{"timeoutSeconds":1}`)
	p := lcProblem(t, code, b)
	t.Logf("restart during the stop: %d %s %v", code, p.Slug(), p.Extensions)
	if code != http.StatusConflict || p.Slug() != problem.Conflict || p.Extensions["blockingOperationId"] != slow.ID {
		t.Fatalf("restart during the stop: %d %s", code, b)
	}

	res, other := n.do("tc-err33-other", "stop", `{"timeoutSeconds":1}`)
	if s := n.operation(slow.ID).Status; s != apitypes.StatusRunning {
		t.Fatalf("the slow stop is %s once the other stop ended: the other did not run beside it", s)
	}
	if !res.Changed || lcInspect(t, "tc-err33-other").Running {
		t.Fatalf("other stop: %+v", res)
	}
	t.Logf("other container stopped by %s while %s still ran", other.ID, slow.ID)

	for _, body := range []string{`{"timeoutSeconds":-1}`, `{"timeoutSeconds":3601}`} {
		code, b = n.act("tc-err33-other", "restart", body)
		if p := lcProblem(t, code, b); code != http.StatusBadRequest || p.Slug() != problem.ValidationFailed {
			t.Fatalf("restart %s: %d %s", body, code, b)
		}
	}
	for _, action := range []string{"start", "stop", "restart", "remove"} {
		code, b = n.act("tc-err33-nope", action, "")
		if p := lcProblem(t, code, b); code != http.StatusNotFound || p.Slug() != problem.NotFound {
			t.Fatalf("%s unknown: %d %s", action, code, b)
		}
	}

	end := n.waitOp(slow.ID, time.Minute)
	if end.Status != apitypes.StatusSucceeded || lcInspect(t, "tc-err33-slow").Running {
		t.Fatalf("slow stop: %+v", end)
	}
}

// A-56: one lane per Compose project, so a stop of one service while
// another service of the same project is being stopped is 409.
func TestDockerLifecycleComposeProjectLane(t *testing.T) {
	requireDocker(t, "tc-lane-app", "tc-lane-db")
	labels := func(service string) []string {
		return []string{"--label", spec.ComposeProjectLabel + "=tclane", "--label", spec.ComposeServiceLabel + "=" + service}
	}
	lcRun(t, "tc-lane-app", labels("app"), lcStubbornCmd)
	lcRun(t, "tc-lane-db", labels("db"), lcTrapCmd)
	n := lcNexusd(t, "tclane")

	slow := n.startAction("tc-lane-app", "stop", `{"timeoutSeconds":10}`)
	code, b := n.act("tc-lane-db", "stop", "")
	p := lcProblem(t, code, b)
	if code != http.StatusConflict || p.Extensions["blockingOperationId"] != slow.ID {
		t.Fatalf("sibling stop during the stop: %d %s", code, b)
	}
	if !lcInspect(t, "tc-lane-db").Running {
		t.Fatal("the refused sibling stop changed the sibling")
	}
	n.waitOp(slow.ID, time.Minute)
}
