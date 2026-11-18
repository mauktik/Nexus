// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-16, TC-RA-17,
// TC-RA-18 and TC-RA-114 (WI 1.5), on planted ops of a test provider
// "tcra", whose output types embed apitypes.ActionResult as the real token
// and config ops do.

package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/clientconfig"
	"github.com/mauktik/nexus/internal/def"
)

type tcraUpdateInput struct {
	Changes []string `json:"changes" jsonschema:"Changes as key=value."`
}

type tcraResult struct {
	apitypes.ActionResult
}

type tcraAddInput struct {
	Name string `json:"name" jsonschema:"The token name."`
}

type tcraAddResult struct {
	apitypes.ActionResult
	Token   string `json:"token"`
	TokenID string `json:"tokenId"`
}

// tcraUpdate is TC-RA-4's op: long-running, answered at admission when
// nothing changes, its trailing arguments bound to changes.
var tcraUpdate = def.Define[tcraUpdateInput, tcraResult](def.Op{
	ID: "tcra.actions.update", Provider: "tcra", Kind: def.KindProviderAction, Action: "update",
	Summary: "Update tcra", Description: "Updates tcra. Test only.",
	Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	CompletesAtAdmission: true,
	CLI:                  def.CLIBinding{Noun: "tcra", Verb: "update", Variadic: "changes", EchoWarnings: true},
})

// tcraStart is long-running, never answered at admission.
var tcraStart = def.Define[struct{}, tcraResult](def.Op{
	ID: "tcra.actions.start", Provider: "tcra", Kind: def.KindProviderAction, Action: "start",
	Summary: "Start tcra", Description: "Starts tcra. Test only.",
	Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	CLI: def.CLIBinding{Noun: "tcra", Verb: "start", EchoWarnings: true},
})

// tcraRestart is a sync mutation that echoes its warnings.
var tcraRestart = def.Define[struct{}, tcraResult](def.Op{
	ID: "tcra.actions.restart", Provider: "tcra", Kind: def.KindProviderAction, Action: "restart",
	Summary: "Restart tcra", Description: "Restarts tcra. Test only.",
	Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync, SyncMutation: true,
	CLI: def.CLIBinding{Noun: "tcra", Verb: "restart", EchoWarnings: true},
})

// tcraAdd returns a secret once, as nexusd.tokens.add does.
var tcraAdd = def.Define[tcraAddInput, tcraAddResult](def.Op{
	ID: "tcra.tokens.add", Provider: "tcra", Kind: def.KindResourceAction, ResourceType: "tokens", Action: "add",
	Summary: "Add a tcra token", Description: "Adds a token. Test only.",
	Scope: def.Scope{Name: "tcra:admin"}, Risk: def.RiskMutating, Mode: def.ModeSync, SyncMutation: true,
	SecretOutput: "token",
	CLI:          def.CLIBinding{Noun: "tcra", Verb: "add", Positional: "name"},
})

func tcraSpecs() []def.ProviderSpec {
	return append(catalog.All(), def.ProviderSpec{Name: "tcra", ReadScope: "tcra:read",
		Ops: []def.Op{tcraUpdate.Op(), tcraStart.Op(), tcraRestart.Op(), tcraAdd.Op()}})
}

const tcraOpID = "0190a5d2-7c3e-7f00-8a00-0000000000aa"

// tcraServer counts requests per "METHOD path", records POST bodies, and
// answers each planted op as the real server would.
type tcraServer struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
	bodies []string
}

func tcraHandler(ts *tcraServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.mu.Lock()
		ts.counts[r.Method+" "+r.URL.Path]++
		if r.Method == http.MethodPost {
			ts.bodies = append(ts.bodies, string(body))
		}
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		write := func(status int, v any) {
			data, err := json.Marshal(v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write(data)
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/providers/tcra/actions/update":
			// Nothing changes: answered at admission with the output.
			write(http.StatusOK, tcraResult{apitypes.ActionResult{Changed: false, Warnings: []string{"w1"}}})
		case "POST /v1/providers/tcra/actions/restart":
			write(http.StatusOK, tcraResult{apitypes.ActionResult{Changed: true, Warnings: []string{"w-one", "w-two"}}})
		case "POST /v1/providers/tcra/actions/start":
			write(http.StatusAccepted, map[string]any{"id": tcraOpID, "action": "tcra.actions.start", "status": "Queued"})
		case "GET /v1/operations/" + tcraOpID:
			result, _ := json.Marshal(tcraResult{apitypes.ActionResult{Changed: true, Warnings: []string{"w-three"}}})
			write(http.StatusOK, map[string]any{"id": tcraOpID, "action": "tcra.actions.start", "status": "Succeeded",
				"result": json.RawMessage(result)})
		case "POST /v1/providers/tcra/tokens/ci/add", "POST /v1/providers/tcra/tokens/h1/add", "POST /v1/providers/tcra/tokens/x/add":
			write(http.StatusOK, tcraAddResult{ActionResult: apitypes.ActionResult{Changed: true}, Token: tcraSecret, TokenID: "aaaaaaaaaaaa"})
		default:
			http.NotFound(w, r)
		}
	})
}

func startTCRAServer(t *testing.T) *tcraServer {
	t.Helper()
	ts := &tcraServer{counts: map[string]int{}}
	ts.Server = httptest.NewServer(tcraHandler(ts))
	t.Cleanup(ts.Close)
	return ts
}

// take returns the counts and bodies so far, and resets them.
func (ts *tcraServer) take() (map[string]int, []string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	c, b := ts.counts, ts.bodies
	ts.counts, ts.bodies = map[string]int{}, nil
	return c, b
}

func (ts *tcraServer) total() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := 0
	for _, c := range ts.counts {
		n += c
	}
	return n
}

func tcraEnv(url string) Env {
	return Env{Getenv: func(k string) string {
		switch k {
		case clientconfig.EnvHost:
			return url
		case clientconfig.EnvToken:
			return "tok"
		}
		return ""
	}}
}

// tcraCommand finds the generated command for noun and verb.
func tcraCommand(t *testing.T, a *app, noun, verb string) *cobra.Command {
	t.Helper()
	cmd, _, err := a.newRoot().Find([]string{noun, verb})
	if err != nil || a.generated[cmd] == nil {
		t.Fatalf("%s %s: %v", noun, verb, err)
	}
	return cmd
}

func TestTCRA16_CLITrailingArgumentsBindToTheVariadicField(t *testing.T) {
	ts := startTCRAServer(t)
	specs := tcraSpecs()
	r := runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "update", "a.b=1", "c.d=x=y", "--yes")
	if r.code != 0 {
		t.Fatalf("tcra update exited %d: %s", r.code, r.stderr)
	}
	if _, bodies := ts.take(); len(bodies) != 1 || bodies[0] != `{"changes":["a.b=1","c.d=x=y"]}` {
		t.Errorf("request bodies %q, want one {\"changes\":[\"a.b=1\",\"c.d=x=y\"]}", bodies)
	}

	r = runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "update", "--help")
	if r.code != 0 {
		t.Fatalf("tcra update --help exited %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "nexus tcra update CHANGES... [flags]") {
		t.Errorf("usage line lacks CHANGES...:\n%s", r.stdout)
	}
	cmd := tcraCommand(t, testApp(specs), "tcra", "update")
	if !strings.Contains(cmd.Example, "nexus tcra update CHANGES...") || strings.Contains(cmd.Example, "<name>") {
		t.Errorf("examples do not print the CHANGES placeholder:\n%s", cmd.Example)
	}

	// An existing spec whose trailing field is names keeps NAMES.
	install := tcraCommand(t, testApp(specs), "package", "install")
	if !strings.HasPrefix(install.Use, "install NAMES...") || !strings.Contains(install.Example, "nexus package install <name>...") {
		t.Errorf("package install: use %q, examples:\n%s", install.Use, install.Example)
	}
}

func TestTCRA17_TheIdempotencyFlagIsAbsentOnSyncMutationOnly(t *testing.T) {
	a := testApp(tcraSpecs())
	add := tcraCommand(t, a, "tcra", "add")
	if add.Flags().Lookup(flagIdempotencyKey) != nil {
		t.Error("tcra add (SyncMutation only) has --idempotency-key")
	}
	if add.Flags().Lookup(flagYes) == nil {
		t.Error("tcra add has no --yes")
	}
	if strings.Contains(add.Example, "safe to retry") || strings.Contains(add.Example, flagIdempotencyKey) {
		t.Errorf("tcra add examples offer a retry key:\n%s", add.Example)
	}
	for _, nv := range [][2]string{{"operation", "cancel"}, {"tcra", "update"}, {"tcra", "start"}} {
		cmd := tcraCommand(t, a, nv[0], nv[1])
		if cmd.Flags().Lookup(flagIdempotencyKey) == nil {
			t.Errorf("%s %s lost --idempotency-key", nv[0], nv[1])
		}
	}
	if up := tcraCommand(t, a, "tcra", "update"); !strings.Contains(up.Example, "safe to retry") {
		t.Errorf("tcra update examples lost the retry line:\n%s", up.Example)
	}
	// A key on a SyncMutation command is an unknown flag: exit 2, nothing sent.
	ts := startTCRAServer(t)
	r := runSpecsCLI(tcraEnv(ts.URL), tcraSpecs(), "tcra", "restart", "--idempotency-key", "K")
	if r.code != ExitUsage || ts.total() != 0 {
		t.Errorf("tcra restart --idempotency-key: exit %d, %d requests: %s", r.code, ts.total(), r.stderr)
	}
}

// warningLines are the a.warn lines on stderr: text lines carrying
// "warning: host", or JSON lines with level warning.
func warningLines(t *testing.T, stderr string, machine bool) []string {
	t.Helper()
	var out []string
	if machine {
		if strings.TrimSpace(stderr) == "" {
			return nil
		}
		for _, l := range jsonLines(t, "stderr", stderr) {
			if l["level"] == "warning" {
				m, _ := l["message"].(string)
				h, _ := l["host"].(string)
				out = append(out, h+": "+m)
			}
		}
		return out
	}
	for l := range strings.Lines(stderr) {
		if strings.Contains(l, ": warning: host ") {
			out = append(out, strings.TrimSuffix(l, "\n"))
		}
	}
	return out
}

func TestTCRA18_EchoWarningsPrintsResultWarningsOnStderrAfterThe(t *testing.T) {
	ts := startTCRAServer(t)
	host := strings.TrimPrefix(ts.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	specs := tcraSpecs()

	// Step 1: a sync result, in a human and a machine format.
	r := runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "restart", "-o", "table")
	if r.code != 0 {
		t.Fatalf("tcra restart -o table exited %d: %s", r.code, r.stderr)
	}
	want := []string{"nexus: warning: host " + host + ": w-one", "nexus: warning: host " + host + ": w-two"}
	if got := warningLines(t, r.stderr, false); !slices.Equal(got, want) {
		t.Errorf("table: warning lines %q, want %q (stderr %q)", got, want, r.stderr)
	}
	r = runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "restart", "-o", "json")
	if r.code != 0 {
		t.Fatalf("tcra restart -o json exited %d: %s", r.code, r.stderr)
	}
	if got := warningLines(t, r.stderr, true); !slices.Equal(got, []string{host + ": w-one", host + ": w-two"}) {
		t.Errorf("json: warning lines %q (stderr %q)", got, r.stderr)
	}
	var printed []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil || len(printed) != 1 {
		t.Fatalf("json stdout %q: %v", r.stdout, err)
	}
	if w, _ := printed[0]["warnings"].([]any); len(w) != 2 || printed[0]["changed"] != true {
		t.Errorf("stdout result %v, want the unmodified result with its warnings", printed[0])
	}

	// Step 2: a waited operation whose result has warnings.
	r = runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "start", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("tcra start exited %d: %s", r.code, r.stderr)
	}
	if got := warningLines(t, r.stderr, true); !slices.Equal(got, []string{host + ": w-three"}) {
		t.Errorf("waited: warning lines %q (stderr %q)", got, r.stderr)
	}
	if !strings.Contains(r.stdout, `"w-three"`) || !strings.Contains(r.stdout, `"status":"Succeeded"`) {
		t.Errorf("waited: stdout %q lacks the operation and its result", r.stdout)
	}
	r = runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "start", "-o", "table")
	if got := warningLines(t, r.stderr, false); r.code != 0 || !slices.Equal(got, []string{"nexus: warning: host " + host + ": w-three"}) {
		t.Errorf("waited table: exit %d, warning lines %q (stderr %q)", r.code, got, r.stderr)
	}

	// Step 3: --no-wait prints the operation as accepted, with no warning.
	for _, f := range []string{"table", "jsonl"} {
		r = runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "start", "--no-wait", "-o", f)
		if r.code != 0 {
			t.Fatalf("tcra start --no-wait exited %d: %s", r.code, r.stderr)
		}
		if strings.Contains(r.stderr, "warning") || strings.Contains(r.stderr, "w-three") {
			t.Errorf("--no-wait -o %s: stderr %q has a warning", f, r.stderr)
		}
	}
}

func TestTCRA114_TheCLITreatsA200FromALongRunningOpAsTheFinal(t *testing.T) {
	ts := startTCRAServer(t)
	host := strings.TrimPrefix(ts.URL, "http://")
	host = host[:strings.LastIndex(host, ":")]
	specs := tcraSpecs()
	// The header a waited operation of the same command prints.
	waited := runSpecsCLI(tcraEnv(ts.URL), specs, "tcra", "start", "-o", "table")
	if waited.code != 0 {
		t.Fatalf("tcra start exited %d: %s", waited.code, waited.stderr)
	}
	waitedHeader := strings.Fields(strings.SplitN(waited.stdout, "\n", 2)[0])
	ts.take()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"table", []string{"-o", "table"}},
		{"no-wait", []string{"--no-wait", "-o", "table"}},
		{"json", []string{"--output", "json"}},
	} {
		args := append([]string{"tcra", "update", "a.b=1", "--yes"}, tc.args...)
		r := runSpecsCLI(tcraEnv(ts.URL), specs, args...)
		if r.code != 0 {
			t.Fatalf("%s: exit %d: %s", tc.name, r.code, r.stderr)
		}
		counts, _ := ts.take()
		for k, n := range counts {
			if strings.HasPrefix(k, "GET /v1/operations") {
				t.Errorf("%s: %d requests to %s", tc.name, n, k)
			}
		}
		if counts["POST /v1/providers/tcra/actions/update"] != 1 {
			t.Errorf("%s: counts %v, want one POST", tc.name, counts)
		}
		if tc.name == "json" {
			var printed []map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &printed); err != nil || len(printed) != 1 {
				t.Fatalf("json stdout %q: %v", r.stdout, err)
			}
			if printed[0]["changed"] != false || printed[0]["host"] != host {
				t.Errorf("json: printed %v, want the result object", printed[0])
			}
			if w, _ := printed[0]["warnings"].([]any); len(w) != 1 || w[0] != "w1" {
				t.Errorf("json: warnings %v", printed[0]["warnings"])
			}
			if got := warningLines(t, r.stderr, true); !slices.Equal(got, []string{host + ": w1"}) {
				t.Errorf("json: warning lines %q (stderr %q)", got, r.stderr)
			}
			continue
		}
		// A table with the result's own columns (user decision at
		// confirm-before-merge, 2026-11-17), not a waited operation's,
		// which would be mostly blank: one row, changed false.
		lines := strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s: stdout %q, want a header and one row", tc.name, r.stdout)
		}
		if header := strings.Fields(lines[0]); !slices.Equal(header, []string{"HOST", "CHANGED"}) ||
			slices.Equal(header, waitedHeader) {
			t.Errorf("%s: header %q, want the result's [HOST CHANGED], not a waited operation's %q", tc.name, header,
				waitedHeader)
		}
		if row := strings.Fields(lines[1]); !slices.Equal(row, []string{host, "false"}) {
			t.Errorf("%s: row %q, want [%s false]", tc.name, row, host)
		}
		if got := warningLines(t, r.stderr, false); !slices.Equal(got, []string{"nexus: warning: host " + host + ": w1"}) {
			t.Errorf("%s: warning lines %q (stderr %q)", tc.name, got, r.stderr)
		}
	}
}
