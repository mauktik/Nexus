package spec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	nexusserver "github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
)

// asCLIEnv makes this test binary the nexus CLI, cli.Run over the real
// catalog, so a test can run it as a process, on a terminal or not.
const asCLIEnv = "NGINX_SPEC_TEST_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(asCLIEnv) == "1" {
		os.Exit(cli.Run(os.Args[1:], cli.OSEnv()))
	}
	os.Exit(m.Run())
}

// call is one handler call a recorder saw.
type call struct {
	op    string
	input map[string]any
}

// recorder records the handler calls of one server.
type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) record(op string, in any) {
	b, _ := json.Marshal(in)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{op: op, input: m})
}

// of returns the calls of op.
func (r *recorder) of(op string) []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []call
	for _, c := range r.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

func syncRec[In, Out any](r *recorder, op def.Typed[In, Out], out func(In) Out) provider.Binding {
	id := op.Op().ID
	return provider.Sync(op, func(_ context.Context, in In) (Out, error) {
		r.record(id, in)
		return out(in), nil
	})
}

func runRec[In, Out any](r *recorder, op def.Typed[In, Out]) provider.Binding {
	id := op.Op().ID
	return provider.InProcess(op, provider.InProcessSpec[In, Out]{
		Run: func(_ context.Context, in In, _ *provider.Run) (Out, error) {
			r.record(id, in)
			var out Out
			return out, nil
		},
	})
}

// stub is the nginx declarations bound to recording handlers: four sync
// reads and nine in-process stand-ins for the detached ops.
type stub struct {
	own string
	rec *recorder
}

func (s *stub) Spec() def.ProviderSpec          { return spec.Spec() }
func (s *stub) Configure(func(any) error) error { return nil }
func (s *stub) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (s *stub) Bindings() []provider.Binding {
	r, own := s.rec, s.own
	return []provider.Binding{
		syncRec(r, spec.Status, func(spec.StatusInput) apitypes.List[spec.Service] {
			return apitypes.List[spec.Service]{Items: []spec.Service{apitypes.NewResource(own, spec.Name,
				spec.ServiceResourceType, "nginx", spec.ServiceProperties{State: "running", Service: "nginx"})}}
		}),
		runRec(r, spec.Test),
		runRec(r, spec.Start),
		runRec(r, spec.Stop),
		runRec(r, spec.Restart),
		runRec(r, spec.Reload),
		syncRec(r, spec.List, func(spec.ListInput) apitypes.List[spec.Site] { return apitypes.List[spec.Site]{} }),
		syncRec(r, spec.Get, func(in spec.SiteInput) spec.Site {
			return apitypes.NewResource(own, spec.Name, spec.SitesResourceType, in.Name,
				spec.SiteProperties{Enabled: true, FileName: in.Name + ".conf", Managed: true, FileType: spec.FileTypeFile})
		}),
		syncRec(r, spec.Content, func(in spec.SiteInput) spec.SiteContent {
			return spec.SiteContent{ID: apitypes.ResourceID(spec.Name, spec.SitesResourceType, in.Name), Name: in.Name}
		}),
		runRec(r, spec.Enable),
		runRec(r, spec.Disable),
		runRec(r, spec.Add),
		runRec(r, spec.Update),
	}
}

// server is a real nexus server, core.New and server.New over the nginx
// stub, with its own token store, audit log, operations and recorder.
type server struct {
	url, token string
	rec        *recorder
}

func startServer(t *testing.T, own string) *server {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	stateDir, auditDir := t.TempDir(), t.TempDir()
	alog, err := audit.Open(audit.Options{Dir: auditDir, Host: own, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(stateDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: filepath.Join(stateDir, "ops"), Host: own,
		Logger: log, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	reg, err := core.New([]provider.Provider{&stub{own: own, rec: rec}}, core.Options{Host: own,
		Version: "v0.0.0-test", Logger: log, Audit: alog, Ops: mgr,
		Providers: map[string]core.ProviderConfig{spec.Name: {Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := auth.Create(stateDir, auth.CreateRequest{Name: "cli",
		Scopes: []string{spec.ReadScope, spec.WriteScope, spec.ConfigScope}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := nexusserver.New(reg, nexusserver.Options{Logger: log,
		Auth: auth.New(stateDir, auth.Options{Logger: log, Audit: alog, FailureLimitPerMinute: 1000})})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("ops shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
		if err := alog.Close(); err != nil {
			t.Errorf("audit close: %v", err)
		}
	})
	return &server{url: "http://" + ln.Addr().String(), token: tok, rec: rec}
}

// writeClientConfig writes a client configuration naming each server under
// its key, in a fresh HOME, and returns that HOME.
func writeClientConfig(t *testing.T, defaultHost string, hosts map[string]*server) string {
	t.Helper()
	home := t.TempDir()
	var b strings.Builder
	if defaultHost != "" {
		fmt.Fprintf(&b, "defaultHost: %s\n", defaultHost)
	}
	b.WriteString("hosts:\n")
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %s:\n    url: %s\n    token: %s\n", n, hosts[n].url, hosts[n].token)
	}
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func cliEnv(home string, extra ...string) []string {
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		return strings.HasPrefix(e, "NEXUS_") || strings.HasPrefix(e, "XDG_CONFIG_HOME=") || strings.HasPrefix(e, "HOME=")
	})
	return append(append(env, asCLIEnv+"=1", "HOME="+home), extra...)
}

type cliResult struct {
	code           int
	stdout, stderr string
}

// noTTY runs the CLI in a session of its own, with no controlling
// terminal, stdin from stdin.
func noTTY(t *testing.T, home, stdin string, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = cliEnv(home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatal(err)
	}
	return cliResult{code: cmd.ProcessState.ExitCode(), stdout: out.String(), stderr: errb.String()}
}

// expectScript spawns NX_CMD on a new pseudo-terminal, answers NX_ANSWER
// to every [y/N] prompt, and exits with the command's exit status.
const expectScript = `set timeout 30
set prompts 0
spawn -noecho sh -c $env(NX_CMD)
expect {
	-re {\[y/N\] $} { incr prompts; send -- "$env(NX_ANSWER)\r"; exp_continue }
	timeout { puts "\nNX_TIMEOUT"; exit 99 }
	eof
}
set r [wait]
puts "\nNX_PROMPTS=$prompts"
exit [lindex $r 3]
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// onTTY runs the CLI with args on a terminal under expect, answering
// answer, and returns its exit code, the prompts asked and the terminal.
func onTTY(t *testing.T, home, answer string, args ...string) (int, int, string) {
	t.Helper()
	expectBin, err := exec.LookPath("expect")
	if err != nil {
		t.Skip("expect is not installed")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.exp")
	if err := os.WriteFile(script, []byte(expectScript), 0o600); err != nil {
		t.Fatal(err)
	}
	q := make([]string, 0, len(args)+1)
	for _, a := range append([]string{os.Args[0]}, args...) {
		q = append(q, shellQuote(a))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, expectBin, script)
	cmd.Env = cliEnv(home, "NX_CMD="+strings.Join(q, " ")+" </dev/null", "NX_ANSWER="+answer)
	// expect itself has no terminal; spawn gives the command its own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	tty, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	m := regexp.MustCompile(`NX_PROMPTS=(\d+)`).FindStringSubmatch(string(tty))
	if m == nil {
		t.Fatalf("expect did not finish: %s", tty)
	}
	n, _ := strconv.Atoi(m[1])
	return code, n, string(tty)
}

// jsonObjects decodes one JSON object per line.
func jsonObjects(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(s) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON object: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// subcommands lists the commands of a cobra help text's Available
// Commands section.
func subcommands(help string) []string {
	var out []string
	in := false
	for line := range strings.Lines(help) {
		switch {
		case strings.HasPrefix(line, "Available Commands:"):
			in = true
		case in && strings.HasPrefix(line, "  "):
			out = append(out, strings.Fields(line)[0])
		case in:
			in = false
		}
	}
	slices.Sort(out)
	return out
}

// nginx-provider TC-60: the generated CLI grammar for nginx, the prompt of
// a destructive site change, the worked examples of L-35, L-36, L-38 and
// L-45, --stdin, and --host all over two real servers, with no
// nginx-specific CLI code.
func TestNginxCLIGrammar(t *testing.T) {
	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	solo := startServer(t, "solo")
	home := writeClientConfig(t, "solo", map[string]*server{"solo": solo})

	// Step 1.
	for noun, want := range map[string][]string{
		"nginx":            {"get", "reload", "restart", "start", "stop"},
		"nginxsite":        {"add", "get", "start", "stop", "update"},
		"nginxconfig":      {"test"},
		"nginxsitecontent": {"get"},
	} {
		r := noTTY(t, home, "", noun, "--help")
		if r.code != 0 {
			t.Fatalf("%s --help exited %d: %s", noun, r.code, r.stderr)
		}
		if got := subcommands(r.stdout); !slices.Equal(got, want) {
			t.Errorf("%s --help lists %v, want %v\n%s", noun, got, want, r.stdout)
		}
	}

	// Step 2: disable is destructive, so it asks; n sends nothing.
	code, prompts, tty := onTTY(t, home, "n", "nginxsite", "stop", "example.com")
	if prompts != 1 {
		t.Errorf("nginxsite stop on a terminal asked %d times, want 1:\n%s", prompts, tty)
	}
	if code == 0 {
		t.Errorf("declined nginxsite stop exited 0:\n%s", tty)
	}
	if n := len(solo.rec.of(spec.Disable.Op().ID)); n != 0 {
		t.Errorf("declined: the stub was called %d times", n)
	}

	// Step 3.
	code, prompts, tty = onTTY(t, home, "n", "nginxsite", "stop", "example.com", "--yes")
	if code != 0 || prompts != 0 {
		t.Errorf("nginxsite stop --yes: exit %d, %d prompts:\n%s", code, prompts, tty)
	}
	if c := solo.rec.of(spec.Disable.Op().ID); len(c) != 1 || c[0].input["name"] != "example.com" {
		t.Errorf("nginxsite stop --yes: calls %v, want one for example.com", c)
	}

	// Step 4: update is mutating, so it never asks, and passes ifMatch.
	r := noTTY(t, home, "", "nginxsite", "update", "example.com", "--content", "x", "--if-match", hex64)
	if r.code != 0 {
		t.Fatalf("nginxsite update exited %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "[y/N]") {
		t.Errorf("nginxsite update prompted: %s", r.stderr)
	}
	c := solo.rec.of(spec.Update.Op().ID)
	if len(c) != 1 || c[0].input["name"] != "example.com" || c[0].input["content"] != "x" || c[0].input["ifMatch"] != hex64 {
		t.Errorf("nginxsite update calls %v", c)
	}
	// --if-match none is create-only, passed as the literal.
	if r := noTTY(t, home, "", "nginxsite", "update", "new.example", "--content", "y", "--if-match", "none"); r.code != 0 {
		t.Errorf("--if-match none exited %d: %s", r.code, r.stderr)
	}
	if c := solo.rec.of(spec.Update.Op().ID); len(c) != 2 || c[1].input["ifMatch"] != "none" {
		t.Errorf("--if-match none calls %v", c)
	}
	// An upper-case digest is refused before anything is called.
	if r := noTTY(t, home, "", "nginxsite", "update", "example.com", "--content", "x", "--if-match", strings.ToUpper(hex64)); r.code == 0 {
		t.Errorf("an upper-case ifMatch was accepted")
	}
	if n := len(solo.rec.of(spec.Update.Op().ID)); n != 2 {
		t.Errorf("a refused update reached the stub: %d calls", n)
	}

	// Step 5.
	r = noTTY(t, home, "", "nginx", "reload", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("nginx reload exited %d: %s", r.code, r.stderr)
	}
	var op map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &op); err != nil || op["id"] == "" || op["id"] == nil {
		t.Errorf("nginx reload printed %q, want an operation with an ID", r.stdout)
	}
	if n := len(solo.rec.of(spec.Reload.Op().ID)); n != 1 {
		t.Errorf("reload calls %d, want 1", n)
	}

	// Step 6.
	r = noTTY(t, home, `{"name":"a"}`+"\n", "nginxsite", "get", "--stdin", "-o", "jsonl")
	if r.code != 0 {
		t.Fatalf("nginxsite get --stdin exited %d: %s", r.code, r.stderr)
	}
	if objs := jsonObjects(t, r.stdout); len(objs) != 1 || objs[0]["name"] != "a" || objs[0]["host"] != "solo" {
		t.Errorf("nginxsite get --stdin printed %v", objs)
	}
	if c := solo.rec.of(spec.Get.Op().ID); len(c) != 1 || c[0].input["name"] != "a" {
		t.Errorf("get calls %v", c)
	}

	// Step 7: --host all over two real servers.
	alpha, beta := startServer(t, "srv-a"), startServer(t, "srv-b")
	fleet := writeClientConfig(t, "", map[string]*server{"alpha": alpha, "beta": beta})
	for _, args := range [][]string{
		{"nginx", "get", "--host", "all", "-o", "jsonl"},
		// A mutation of two hosts needs --yes without a terminal (base TC-175).
		{"nginx", "reload", "--host", "all", "--yes", "-o", "jsonl"},
		{"nginxsite", "stop", "example.com", "--host", "all", "--yes", "-o", "jsonl"},
	} {
		r := noTTY(t, fleet, "", args...)
		if r.code != 0 {
			t.Errorf("%v exited %d: %s", args, r.code, r.stderr)
			continue
		}
		objs := jsonObjects(t, r.stdout)
		var hosts []string
		for _, o := range objs {
			h, _ := o["host"].(string)
			hosts = append(hosts, h)
			if args[1] == "reload" {
				if id, _ := o["id"].(string); id == "" {
					t.Errorf("%v: object %v has no operation ID", args, o)
				}
			}
		}
		slices.Sort(hosts)
		if !slices.Equal(hosts, []string{"alpha", "beta"}) {
			t.Errorf("%v printed %d objects for hosts %v, want one each for alpha and beta:\n%s", args, len(objs), hosts, r.stdout)
		}
	}
	for name, s := range map[string]*server{"alpha": alpha, "beta": beta} {
		if n := len(s.rec.of(spec.Status.Op().ID)); n != 1 {
			t.Errorf("%s: %d status calls, want 1", name, n)
		}
		if n := len(s.rec.of(spec.Reload.Op().ID)); n != 1 {
			t.Errorf("%s: %d reload calls, want 1", name, n)
		}
		if c := s.rec.of(spec.Disable.Op().ID); len(c) != 1 || c[0].input["name"] != "example.com" {
			t.Errorf("%s: disable calls %v, want one for example.com", name, c)
		}
		s.rec.mu.Lock()
		if len(s.rec.calls) != 3 {
			t.Errorf("%s: %d calls in all, want exactly the three", name, len(s.rec.calls))
		}
		s.rec.mu.Unlock()
	}

	// No nginx-specific CLI code (FR-16).
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "cli", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("internal/cli sources: %v %v", files, err)
	}
	sub, err := filepath.Glob(filepath.Join("..", "..", "..", "cli", "*", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(files, sub...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(b), []byte("nginx")) {
			t.Errorf("%s mentions nginx: the CLI must stay provider-neutral", f)
		}
	}
}
