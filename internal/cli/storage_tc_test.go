// Tests of planning/storage-provider/test-cases.md TC-STG-206.
// Owner WIs: 5.1 (the storage commands the CLI generates from the catalog).

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
	storage "github.com/mauktik/nexus/internal/providers/storage/spec"
)

// storageFake is a storage API host: every read answers one item (the
// resource itself for a get) whose properties hold each column's field,
// every POST an operation that has already succeeded. It records every
// request but the operation polls.
type storageFake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
}

// storageItem is one resource of the route's type carrying every column
// of every storage read.
func storageItem(own, typ, name string) apitypes.Resource[map[string]any] {
	props := map[string]any{}
	for _, op := range storage.Spec().Ops {
		for _, col := range op.Columns {
			path, ok := strings.CutPrefix(col, "properties.")
			if !ok {
				continue
			}
			m := props
			segs := strings.Split(path, ".")
			for _, s := range segs[:len(segs)-1] {
				if m[s] == nil {
					m[s] = map[string]any{}
				}
				m = m[s].(map[string]any)
			}
			m[segs[len(segs)-1]] = "v-" + segs[len(segs)-1]
		}
	}
	return apitypes.NewResource(own, storage.Name, typ, name, props)
}

func startStorageFake(t *testing.T, own string) *storageFake {
	t.Helper()
	f := &storageFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/operations/"); ok {
			_ = json.NewEncoder(w).Encode(apitypes.Operation{ID: id, Host: own, Provider: storage.Name, Action: "fake",
				Status: apitypes.StatusSucceeded, Created: time.Now(), Result: json.RawMessage(`{"changed":true}`)})
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, capturedRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/providers/storage/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(apitypes.Operation{ID: "0190a5d2-7c3e-7f00-8a00-000000000001", Host: own,
				Provider: storage.Name, Action: "fake", Status: apitypes.StatusQueued, Created: time.Now()})
			return
		}
		typ, name, isGet := strings.Cut(rest, "/")
		if isGet {
			_ = json.NewEncoder(w).Encode(storageItem(own, typ, name))
			return
		}
		_ = json.NewEncoder(w).Encode(apitypes.List[apitypes.Resource[map[string]any]]{
			Items: []apitypes.Resource[map[string]any]{storageItem(own, typ, "current")}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *storageFake) take() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reqs
	f.reqs = nil
	return r
}

// header is the column header line of a table: the words of its first
// line.
func header(table string) []string {
	first, _, _ := strings.Cut(table, "\n")
	return strings.Fields(first)
}

// TC-STG-206: CLI grammar, escaping and columns for the storage nouns. Owner WI 5.1. Requirements: FR-17.
func TestTCSTG206_CLIGrammarEscapingAndColumnsForThe(t *testing.T) {
	f := startStorageFake(t, "h1")
	env := Env{Getenv: func(k string) string {
		switch k {
		case clientconfig.EnvHost:
			return f.URL
		case clientconfig.EnvToken:
			return "tok"
		}
		return ""
	}}
	columns := map[string][]string{}
	for _, op := range storage.Spec().Ops {
		if op.Kind == def.KindList {
			columns[op.CLI.Noun] = op.Columns
		}
	}

	const prefix = "/v1/providers/storage/"
	for _, c := range []struct {
		args          []string
		method, path  string
		query         string
		body          map[string]any
		columnsOfNoun string // A read whose table has its op's Columns
	}{
		{[]string{"storagehealth", "get"}, http.MethodGet, "health", "", nil, "storagehealth"},
		{[]string{"pool", "get"}, http.MethodGet, "pools", "", nil, "pool"},
		{[]string{"pool", "get", "z1"}, http.MethodGet, "pools/z1", "", nil, ""},
		{[]string{"dataset", "get", "z1/ds/appdata"}, http.MethodGet, "datasets/z1%2Fds%2Fappdata", "", nil, ""},
		{[]string{"snapshot", "get", "--dataset", "z1/ds/appdata", "--recursive"}, http.MethodGet, "snapshots",
			"dataset=z1%2Fds%2Fappdata&recursive=true", nil, "snapshot"},
		{[]string{"backup", "get", "--vmid", "101"}, http.MethodGet, "backups", "vmid=101", nil, "backup"},
		{[]string{"backupsummary", "get"}, http.MethodGet, "backupsummary", "", nil, "backupsummary"},
		{[]string{"timemachine", "get"}, http.MethodGet, "timemachine", "", nil, "timemachine"},
		{[]string{"scrub", "start", "z1", "--force"}, http.MethodPost, "pools/z1/scrub", "", map[string]any{"force": true}, ""},
		{[]string{"scrub", "stop", "z1"}, http.MethodPost, "pools/z1/stop-scrub", "", map[string]any{}, ""},
		{[]string{"snapshot", "add", "z1/ds/share", "--label", "pre"}, http.MethodPost, "datasets/z1%2Fds%2Fshare/snapshot",
			"", map[string]any{"label": "pre"}, ""},
	} {
		name := strings.Join(c.args, " ")
		r := runCLI(env, append(slices.Clone(c.args), "-o", "json")...)
		if r.code != 0 {
			t.Errorf("%s: exit %d: %s", name, r.code, r.stderr)
			continue
		}
		reqs := f.take()
		if len(reqs) != 1 {
			t.Errorf("%s: %d requests, want 1: %+v", name, len(reqs), reqs)
			continue
		}
		got := reqs[0]
		if got.method != c.method || got.path != prefix+c.path {
			t.Errorf("%s: %s %s, want %s %s", name, got.method, got.path, c.method, prefix+c.path)
		}
		if q := sortedQuery(t, got.rawQuery); q != sortedQuery(t, c.query) {
			t.Errorf("%s: query %q, want %q", name, got.rawQuery, c.query)
		}
		if c.method == http.MethodPost {
			var body map[string]any
			if err := json.Unmarshal(got.body, &body); err != nil && len(got.body) > 0 {
				t.Errorf("%s: body %s: %v", name, got.body, err)
			}
			if body == nil {
				body = map[string]any{}
			}
			if fmt.Sprint(body) != fmt.Sprint(c.body) {
				t.Errorf("%s: body %v, want %v", name, body, c.body)
			}
			// A write prints its operation, one per host.
			var ops []apitypes.Operation
			if err := json.Unmarshal([]byte(r.stdout), &ops); err != nil || len(ops) != 1 || ops[0].ID == "" {
				t.Errorf("%s: stdout %q is not one operation (%v)", name, r.stdout, err)
			}
		}
		if c.columnsOfNoun == "" {
			continue
		}
		// The table shows the op's Columns, named by their path under
		// properties.
		r = runCLI(env, append(slices.Clone(c.args), "-o", "table")...)
		f.take()
		if r.code != 0 {
			t.Errorf("%s -o table: exit %d: %s", name, r.code, r.stderr)
			continue
		}
		var want []string
		for _, col := range columns[c.columnsOfNoun] {
			want = append(want, strings.ToUpper(strings.TrimPrefix(col, "properties.")))
		}
		h := slices.DeleteFunc(header(r.stdout), func(s string) bool { return s == "HOST" })
		if !slices.Equal(h, want) {
			t.Errorf("%s -o table: columns %v, want %v\n%s", name, h, want, r.stdout)
		}
	}

	// Step 2: --host all fans a read out to both hosts.
	f1, f2 := startStorageFake(t, "h1"), startStorageFake(t, "h2")
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: f1.URL, token: "t1"},
		clientHost{name: "h2", url: f2.URL, token: "t2"})
	r := runCLI(homeEnv(home), "pool", "get", "--host", "all", "-o", "json")
	if r.code != 0 {
		t.Fatalf("pool get --host all: exit %d: %s", r.code, r.stderr)
	}
	for i, fk := range []*storageFake{f1, f2} {
		reqs := fk.take()
		if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].path != prefix+"pools" {
			t.Errorf("host h%d: requests %+v, want one GET %spools", i+1, reqs, prefix)
		}
	}
	for _, h := range []string{`"h1"`, `"h2"`} {
		if !strings.Contains(r.stdout, h) {
			t.Errorf("pool get --host all: output lacks host %s:\n%s", h, r.stdout)
		}
	}

	// Step 3: the storage commands use the existing verbs only.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Log("git not on PATH: the verbs.go diff is not checked")
		return
	}
	root := filepath.Join("..", "..")
	show := exec.Command(git, "show", "43e5792:internal/def/verbs.go")
	show.Dir = root
	base, err := show.Output()
	if err != nil {
		t.Logf("base 43e5792 not reachable (%v): the verbs.go diff is not checked", err)
		return
	}
	cur, err := os.ReadFile(filepath.Join(root, "internal", "def", "verbs.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(base) != string(cur) {
		t.Error("internal/def/verbs.go differs from base 43e5792")
	}
}

// sortedQuery is a query string with its parameters sorted.
func sortedQuery(t *testing.T, q string) string {
	t.Helper()
	if q == "" {
		return ""
	}
	parts := strings.Split(q, "&")
	slices.Sort(parts)
	return strings.Join(parts, "&")
}
