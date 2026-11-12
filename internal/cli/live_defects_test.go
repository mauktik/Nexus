package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/problem"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// The live defects of planning/cli-live-output-defects: a server that
// calls itself liveServerName, reached through the client-side name
// liveClientName, as nexusd on home-server was reached as home.
const (
	liveServerName = "home-server"
	liveClientName = "home"
	liveOpID       = "01a1ce74-0074-73b4-a609-36cc2726083f"
)

var liveCheckedAt = time.Date(2026, 11, 12, 6, 31, 22, 0, time.UTC)

// liveUpdates are the check-updates result items, in the shape the live
// server sent: container resources labeled with the server's own name.
func liveUpdates() []dockerspec.ContainerUpdate {
	checked := liveCheckedAt
	item := func(name, status, reason string) dockerspec.ContainerUpdate {
		return apitypes.NewResource(liveServerName, dockerspec.Name, dockerspec.ResourceType, name,
			dockerspec.ContainerUpdateProperties{ContainerID: strings.Repeat("a", 64), ImageReference: name + ":latest",
				Update: dockerspec.UpdateCheck{Status: status, Reason: reason, CheckedAt: &checked}})
	}
	return []dockerspec.ContainerUpdate{
		item("fail2ban", "updateAvailable", ""),
		item("immich", "notUpdatable", "locallyBuilt"),
		item("web", "upToDate", ""),
	}
}

// liveOperation is the check-updates operation as GET /v1/operations/{id}
// answered once it ended with status.
func liveOperation(t *testing.T, status apitypes.OperationStatus) apitypes.Operation {
	t.Helper()
	op := apitypes.Operation{ID: liveOpID, Host: liveServerName, Action: dockerspec.CheckUpdates.Op().ID,
		Provider: dockerspec.Name, TokenName: "cli", Status: status, Created: liveCheckedAt}
	switch status {
	case apitypes.StatusSucceeded:
		res, err := json.Marshal(dockerspec.CheckUpdatesResult{CheckedAt: liveCheckedAt, Items: liveUpdates()})
		if err != nil {
			t.Fatal(err)
		}
		op.Result = res
	case apitypes.StatusFailed:
		op.Error = problem.New(problem.InternalError, "the registry did not answer")
	}
	return op
}

// startLiveServer plays nexusd's HTTP API for the live defects: check-updates
// accepts with 202, the operation reads as final, and container get
// answers for any name. It records the request paths.
func startLiveServer(t *testing.T, final apitypes.Operation) *captureServer {
	t.Helper()
	mux := http.NewServeMux()
	route := func(method, path string, status int, v any) {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
		})
	}
	accepted := apitypes.Operation{ID: liveOpID, Host: liveServerName, Action: final.Action, Provider: final.Provider,
		TokenName: final.TokenName, Status: apitypes.StatusQueued, Created: liveCheckedAt}
	method, path, err := openapi.Route(dockerspec.CheckUpdates.Op())
	if err != nil {
		t.Fatal(err)
	}
	route(method, path, http.StatusAccepted, accepted)
	if method, path, err = openapi.Route(systemspec.OperationsGet.Op()); err != nil {
		t.Fatal(err)
	}
	route(method, path, http.StatusOK, final)
	if method, path, err = openapi.Route(dockerspec.Get.Op()); err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) {
		c := apitypes.NewResource(liveServerName, dockerspec.Name, dockerspec.ResourceType, r.PathValue("name"),
			dockerspec.ContainerProperties{State: "running", Health: "none"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	})
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, capturedRequest{method: r.Method, path: r.URL.EscapedPath()})
		cs.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(cs.Close)
	return cs
}

// liveEnv configures the client-side name home for srv.
func liveEnv(t *testing.T, srv *captureServer) Env {
	t.Helper()
	return homeEnv(writeClientConfig(t, "", clientHost{liveClientName, srv.URL, "tok", nil}))
}

// D1: the user's command prints one row per container, with its name and
// update status, not one Operation row holding only the host.
func TestWaitedCheckUpdatesPrintsContainerRows(t *testing.T) {
	srv := startLiveServer(t, liveOperation(t, apitypes.StatusSucceeded))
	env, specs := liveEnv(t, srv), catalog.All()
	items := liveUpdates()

	r := runSpecsCLI(env, specs, "--host", liveClientName, "containerupdate", "get", "-o", "table",
		"--select", "name,properties.update.status,properties.update.reason")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 1+len(items) {
		t.Fatalf("table has %d lines, want a header and one row per container (%d):\n%s", len(lines), len(items), r.stdout)
	}
	for _, it := range items {
		var row string
		for _, l := range lines[1:] {
			if strings.Contains(l, " "+it.Name+" ") {
				row = l
			}
		}
		if !strings.HasPrefix(row, liveClientName+" ") || !strings.Contains(row, it.Properties.Update.Status) ||
			!strings.Contains(row, it.Properties.Update.Reason) {
			t.Errorf("no row for container %s with status %s on host %s:\n%s", it.Name, it.Properties.Update.Status, liveClientName, r.stdout)
		}
	}
	// The operation is not lost: its id and checkedAt are on stderr.
	if !strings.Contains(r.stderr, liveOpID) || !strings.Contains(r.stderr, liveCheckedAt.Format(time.RFC3339)) {
		t.Errorf("stderr %q does not name operation %s and its checkedAt", r.stderr, liveOpID)
	}

	// In a machine format: one object per container, each with the
	// container's id and the client-side host name.
	r = runSpecsCLI(env, specs, "--host", liveClientName, "containerupdate", "get", "-o", "jsonl")
	objs := jsonLines(t, "jsonl", r.stdout)
	if r.code != 0 || len(objs) != len(items) {
		t.Fatalf("jsonl: exit %d, %d objects, want %d: %s", r.code, len(objs), len(items), r.stdout)
	}
	for i, o := range objs {
		if o["id"] != items[i].ID || o["host"] != liveClientName || o["name"] != items[i].Name {
			t.Errorf("jsonl[%d] = %v, want id %s, host %s", i, o, items[i].ID, liveClientName)
		}
	}

	// --no-wait still prints the operation as accepted (FR-33).
	r = runSpecsCLI(env, specs, "--host", liveClientName, "containerupdate", "get", "-o", "jsonl", "--no-wait")
	if objs := jsonLines(t, "no-wait", r.stdout); r.code != 0 || len(objs) != 1 || objs[0]["id"] != liveOpID ||
		objs[0]["status"] != string(apitypes.StatusQueued) {
		t.Errorf("--no-wait: exit %d, stdout %q, want the queued operation", r.code, r.stdout)
	}

	// --no-wait in a table has the operation's columns, not the items'.
	r = runSpecsCLI(env, specs, "--host", liveClientName, "containerupdate", "get", "-o", "table", "--no-wait")
	nw := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if r.code != 0 || len(nw) != 2 || !slices.Contains(strings.Fields(nw[0]), "STATUS") ||
		!slices.Equal(strings.Fields(nw[1])[:2], []string{liveClientName, liveOpID}) || !strings.Contains(nw[1], string(apitypes.StatusQueued)) {
		t.Errorf("--no-wait table: exit %d, stdout %q, want the operation's ID and STATUS columns and its queued row", r.code, r.stdout)
	}

	// A failed wait still prints the operation, and exits 1.
	failed := startLiveServer(t, liveOperation(t, apitypes.StatusFailed))
	r = runSpecsCLI(liveEnv(t, failed), specs, "--host", liveClientName, "containerupdate", "get", "-o", "jsonl")
	if objs := jsonLines(t, "failed", r.stdout); r.code != ExitFailure || len(objs) != 1 || objs[0]["id"] != liveOpID ||
		objs[0]["status"] != string(apitypes.StatusFailed) {
		t.Errorf("failed wait: exit %d, stdout %q, want the failed operation and exit 1", r.code, r.stdout)
	}
}

// A host whose check failed has no container rows: in a table it adds no
// row, rather than a row of blank cells, and its failure is on stderr; the
// other hosts' rows are intact and the exit is 6. A machine format keeps
// the failed operation (FR-33).
func TestWaitedCheckUpdatesTableOmitsFailedHost(t *testing.T) {
	good := startLiveServer(t, liveOperation(t, apitypes.StatusSucceeded))
	bad := startLiveServer(t, liveOperation(t, apitypes.StatusFailed))
	env := homeEnv(writeClientConfig(t, "", clientHost{"a", good.URL, "tok", nil}, clientHost{"b", bad.URL, "tok", nil}))
	specs := catalog.All()

	r := runSpecsCLI(env, specs, "--host", "a,b", "containerupdate", "get", "-o", "table")
	if r.code != 6 {
		t.Errorf("exit %d, want 6: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if len(lines) != 1+len(liveUpdates()) {
		t.Errorf("table has %d lines, want a header and one row per container of host a:\n%s", len(lines), r.stdout)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "a ") || len(strings.Fields(l)) < 3 {
			t.Errorf("row %q is not a container row of host a:\n%s", l, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, "the registry did not answer") || !strings.Contains(r.stderr, liveOpID) {
		t.Errorf("stderr %q does not name host b's failure and operation", r.stderr)
	}

	r = runSpecsCLI(env, specs, "--host", "a,b", "containerupdate", "get", "-o", "jsonl")
	var failedOps int
	for _, o := range jsonLines(t, "jsonl", r.stdout) {
		if o["status"] == string(apitypes.StatusFailed) && o["host"] == "b" {
			failedOps++
		}
	}
	if r.code != 6 || failedOps != 1 {
		t.Errorf("jsonl: exit %d, %d failed operations of host b, want exit 6 and one: %s", r.code, failedOps, r.stdout)
	}
}

// D2: an operation's nested result items carry the client-side host name,
// so a result item piped into --stdin binds back to the same host.
func TestNestedResultItemsCarryClientHostName(t *testing.T) {
	srv := startLiveServer(t, liveOperation(t, apitypes.StatusSucceeded))
	env, specs := liveEnv(t, srv), catalog.All()

	r := runSpecsCLI(env, specs, "--host", liveClientName, "operation", "get", liveOpID, "-o", "json")
	var ops []struct {
		Host   string `json:"host"`
		Result struct {
			Items []map[string]any `json:"items"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &ops); err != nil || r.code != 0 || len(ops) != 1 {
		t.Fatalf("operation get: exit %d, stdout %q (%v): %s", r.code, r.stdout, err, r.stderr)
	}
	items := ops[0].Result.Items
	if ops[0].Host != liveClientName || len(items) != len(liveUpdates()) {
		t.Fatalf("operation get: host %q, %d result items", ops[0].Host, len(items))
	}
	for i, it := range items {
		if it["host"] != liveClientName {
			t.Errorf("result.items[%d].host = %v, want the client-side name %s", i, it["host"], liveClientName)
		}
	}

	line, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	srv.take()
	env.Stdin = strings.NewReader(string(line) + "\n")
	r = runSpecsCLI(env, specs, "container", "get", "--stdin", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("container get --stdin of a result item: exit %d: %s", r.code, r.stderr)
	}
	objs := jsonLines(t, "stdin", r.stdout)
	if len(objs) != 1 || objs[0]["host"] != liveClientName || objs[0]["name"] != items[0]["name"] {
		t.Errorf("container get --stdin printed %v, want %v on host %s", objs, items[0]["name"], liveClientName)
	}
	if reqs := srv.take(); len(reqs) != 1 || reqs[0].path != "/v1/providers/docker/containers/"+fmt.Sprint(items[0]["name"]) {
		t.Errorf("requests %+v, want one container get", reqs)
	}
}

// D3: an unknown noun or verb is reported as an unknown command, exit 2,
// whatever flags follow it, --help included; known commands are unchanged.
func TestUnknownCommandWithFlagsReportsUnknownCommand(t *testing.T) {
	env := Env{Getenv: func(string) string { return "" }}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"upgradable", "get", "-o", "table"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "-o", "table"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "get", "-x"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "get", "--select", "name"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "get", "--bogus"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "get", "--help"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "-h"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"--host", "home", "upgradable", "get", "-o", "table"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"upgradable", "get"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"container", "bogus", "-o", "table"}, `unknown command "bogus" for "nexus container"`},
		{[]string{"container", "bogus", "--help"}, `unknown command "bogus" for "nexus container"`},
		{[]string{"container", "bogus"}, `unknown command "bogus" for "nexus container"`},
		{[]string{"host", "bogus", "-o", "json"}, `unknown command "bogus" for "nexus host"`},
		{[]string{"command", "bogus", "-h"}, `unknown command "bogus" for "nexus command"`},
		{[]string{"--help", "upgradable", "get"}, `unknown command "upgradable" for "nexus"`},
		{[]string{"-h", "container", "bogus"}, `unknown command "bogus" for "nexus container"`},
		{[]string{"container", "--help", "bogus"}, `unknown command "bogus" for "nexus container"`},
	} {
		r := runCLI(env, tc.args...)
		if r.code != ExitUsage || r.stdout != "" || !strings.Contains(r.stderr, "nexus: "+tc.want) {
			t.Errorf("nexus %s: exit %d, stdout %q, stderr %q; want exit 2 and %s", strings.Join(tc.args, " "), r.code, r.stdout, r.stderr, tc.want)
		}
	}

	// Not regressed.
	for _, tc := range []struct {
		args   []string
		code   int
		stdout string // Substring of stdout
		stderr string // Substring of stderr
	}{
		{nil, ExitUsage, "", "nexus needs a command"},
		{[]string{"--help"}, ExitOK, "Usage:", ""},
		{[]string{"-h"}, ExitOK, "Usage:", ""},
		{[]string{"--version"}, ExitOK, "nexus ", ""},
		{[]string{"container"}, ExitUsage, "", "nexus container needs a command"},
		{[]string{"container", "--help"}, ExitOK, "Usage:", ""},
		// A help or version flag before the command it is for.
		{[]string{"--help", "container"}, ExitOK, "Usage:", ""},
		{[]string{"-h", "container"}, ExitOK, "Usage:", ""},
		{[]string{"--help", "container", "get"}, ExitOK, "nexus container get [NAME]", ""},
		{[]string{"container", "--help", "get"}, ExitOK, "Usage:", ""},
		{[]string{"container", "-h", "get"}, ExitOK, "Usage:", ""},
		{[]string{"--version", "container"}, ExitOK, "nexus ", ""},
		{[]string{"container", "get", "--help"}, ExitOK, "nexus container get [NAME]", ""},
		{[]string{"help", "container", "get"}, ExitOK, "nexus container get [NAME]", ""},
		{[]string{"command", "get", "--noun", "container"}, ExitOK, `"noun":"container"`, ""},
		{[]string{"completion", "get", "--shell", "bash"}, ExitOK, "bash completion", ""},
		{[]string{"__complete", "contain"}, ExitOK, "container", ""},
		{[]string{"container", "get", "--bogusflag", "-o", "table"}, ExitUsage, "", "unknown flag: --bogusflag"},
	} {
		r := runCLI(env, tc.args...)
		if r.code != tc.code || !strings.Contains(r.stdout, tc.stdout) || !strings.Contains(r.stderr, tc.stderr) {
			t.Errorf("nexus %s: exit %d, stdout %q, stderr %q; want exit %d", strings.Join(tc.args, " "), r.code, r.stdout, r.stderr, tc.code)
		}
	}
}

// D1 cause 2: a --select path no printed object has is warned about on
// stderr, once, and the exit code is unchanged; host and id never are.
func TestSelectWarnsOnUnmatchedPath(t *testing.T) {
	srv := cannedServer(t, http.StatusOK, `{"items":[`+
		`{"id":"/providers/docker/containers/web","host":"nas","name":"web","type":"containers","properties":{"state":"running"}},`+
		`{"id":"/providers/docker/containers/db","host":"nas","name":"db","type":"containers","properties":{"state":"exited"}}]}`)
	env, specs := urlEnv(srv), catalog.All()

	r := runSpecsCLI(env, specs, "container", "get", "-o", "table", "--select", "name,doesnotexist")
	if r.code != 0 {
		t.Fatalf("table: exit %d: %s", r.code, r.stderr)
	}
	if n := strings.Count(r.stderr, "warning"); n != 1 || !strings.Contains(r.stderr, `--select path "doesnotexist" matched no field`) {
		t.Errorf("table: stderr %q, want one warning naming --select path doesnotexist", r.stderr)
	}

	r = runSpecsCLI(env, specs, "container", "get", "-o", "jsonl", "--select", "name,properties.nope,doesnotexist")
	if r.code != 0 || len(jsonLines(t, "stdout", r.stdout)) != 2 {
		t.Fatalf("jsonl: exit %d, stdout %q: %s", r.code, r.stdout, r.stderr)
	}
	var warned []string
	for _, w := range jsonLines(t, "stderr", r.stderr) {
		if w["level"] != "warning" {
			t.Errorf("stderr line %v is not a warning", w)
		}
		warned = append(warned, fmt.Sprint(w["select"]))
	}
	if strings.Join(warned, ",") != "properties.nope,doesnotexist" {
		t.Errorf("jsonl: warnings for %v, want properties.nope and doesnotexist: %q", warned, r.stderr)
	}

	// Paths that match, and host and id, are not warned about.
	r = runSpecsCLI(env, specs, "container", "get", "-o", "table", "--select", "host,id,name,properties.state")
	if r.code != 0 || r.stderr != "" {
		t.Errorf("matched paths: exit %d, stderr %q", r.code, r.stderr)
	}
}

// D1 cause 3: a --select table keeps id as the help promises, right after
// host; a table without --select keeps its declared columns.
func TestSelectTableKeepsID(t *testing.T) {
	srv := cannedServer(t, http.StatusOK, `{"items":[`+
		`{"id":"/providers/docker/containers/web","host":"nas","name":"web","type":"containers","properties":{"state":"running"}}]}`)
	env, specs := urlEnv(srv), catalog.All()

	r := runSpecsCLI(env, specs, "container", "get", "-o", "table", "--select", "name,properties.state")
	lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 2 {
		t.Fatalf("select table: exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "HOST ID NAME STATE" {
		t.Errorf("select table header %v, want HOST ID NAME STATE", got)
	}
	if got := strings.Fields(lines[1]); strings.Join(got, " ") != urlTargetHost+" /providers/docker/containers/web web running" {
		t.Errorf("select table row %v", got)
	}

	// Selecting id itself does not repeat the column.
	r = runSpecsCLI(env, specs, "container", "get", "-o", "table", "--select", "id,name")
	if got := strings.Fields(strings.SplitN(r.stdout, "\n", 2)[0]); strings.Join(got, " ") != "HOST ID NAME" {
		t.Errorf("--select id,name header %v, want HOST ID NAME", got)
	}

	r = runSpecsCLI(env, specs, "container", "get", "-o", "table")
	if got := strings.Fields(strings.SplitN(r.stdout, "\n", 2)[0]); r.code != 0 || got[0] != "HOST" || got[1] != "NAME" {
		t.Errorf("default table header %v, want the declared columns unchanged", got)
	}
}
