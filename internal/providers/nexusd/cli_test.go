package nexusd

// CLI end-to-end tests (WI 6.1): the real nexus command tree, generated from
// catalog.All() with nexusd in it, run through cli.Run against the T0
// stack: TC-SM-97, TC-SM-98, TC-SM-E11 and TC-SM-E15.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/cli"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/version"
)

// cliHost is the name the client configuration gives the T0 stack.
const cliHost = "t0"

// cliResult is one nexus invocation's outcome.
type cliResult struct {
	code           int
	stdout, stderr string
}

// errNoTerminal is the OpenTerminal of a run without a controlling terminal.
var errNoTerminal = errors.New("no terminal")

// nexusCLI runs the nexus CLI against the stack as token, with a client
// configuration in a fresh HOME, no terminal and an empty stdin.
func (s *stack) nexusCLI(t *testing.T, token string, args ...string) cliResult {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "nexus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("defaultHost: %s\nhosts:\n  %s:\n    url: %s\n    token: %s\n", cliHost, cliHost, s.url, token)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(args, cli.Env{
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		OpenTerminal: func() (cli.Terminal, error) { return nil, errNoTerminal },
	})
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// jsonDaemons decodes `nexusd get -o json`: an array of daemons.
func jsonDaemons(t *testing.T, r cliResult) []spec.Daemon {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	return decodeJSON[[]spec.Daemon](t, []byte(r.stdout))
}

// cliProblem decodes the one JSON error line a machine-format run printed
// on stderr: the problem's fields next to exit, host and message.
func cliProblem(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	m := decodeJSON[map[string]any](t, []byte(lines[len(lines)-1]))
	if _, ok := m["type"]; !ok {
		t.Fatalf("stderr %q has no error line", r.stderr)
	}
	return m
}

// writePackage writes pkg as dir/name and its signature by k as
// dir/name.sig, the sidecar update reads by default, and returns the path.
func writePackage(t *testing.T, dir, name string, pkg []byte, k ed25519.PrivateKey) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, string(pkg))
	writeFile(t, path+".sig", signBytes(k, pkg)+"\n")
	return path
}

// TC-SM-97 (WI 6.1; FR-13, FR-2, FR-3, FR-4): CLI end to end: get,
// validate-config and reload through cli.Run against the nexusd stack.
func TestTCSM97_CLIEndToEndGetValidate(t *testing.T) {
	s := newStack(t, stackConfig{})
	s.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		return provider.ReloadResult{RestartRequired: []string{"limits"}}, nil
	})

	// Step 1: the TC-SM-51 fields.
	ds := jsonDaemons(t, s.nexusCLI(t, s.admin, "nexusd", "get", "-o", "json"))
	if len(ds) != 1 {
		t.Fatalf("%d daemons, want 1", len(ds))
	}
	d := ds[0]
	pr := d.Properties
	if d.Host != cliHost || d.ID != "/providers/nexusd/daemons/nexusd" || d.Name != "nexusd" ||
		pr.Version != version.Version || pr.StartedAt.IsZero() || pr.UptimeSeconds < 59 ||
		pr.ConfigPath != s.daemon.ConfigPath() || pr.ExecutablePath != "/x/nexusd" ||
		pr.Previous != (spec.Previous{}) || pr.UpdateConfigured || !pr.SelfManagement.Available {
		t.Errorf("daemon %+v", d)
	}

	// Step 2: the table's columns.
	r := s.nexusCLI(t, s.admin, "nexusd", "get", "-o", "table")
	if r.code != 0 {
		t.Fatalf("table: exit %d: %s", r.code, r.stderr)
	}
	header := strings.Fields(strings.SplitN(r.stdout, "\n", 2)[0])
	if want := []string{"HOST", "NAME", "VERSION", "UPTIMESECONDS", "PREVIOUS.VERSION", "UPDATECONFIGURED"}; !slices.Equal(header, want) {
		t.Errorf("table header %q, want %q", header, want)
	}

	// Step 3: a rollback binary appears.
	writeScript(t, s.paths.Prev, `echo "nexusd v9.9.0"`)
	if prev := jsonDaemons(t, s.nexusCLI(t, s.admin, "nexusd", "get", "-o", "json"))[0].Properties.Previous; prev !=
		(spec.Previous{Present: true, Path: s.paths.Prev, Version: "v9.9.0"}) {
		t.Errorf("previous %+v, want v9.9.0 at %s", prev, s.paths.Prev)
	}

	// Step 4: valid, then invalid; both exit 0.
	r = s.nexusCLI(t, s.reader, "nexusd", "validate-config", "-o", "yaml")
	if r.code != 0 || !strings.Contains(r.stdout, "valid: true") || !strings.Contains(r.stdout, "errors: []") {
		t.Errorf("valid config: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	bad := fixture(t, "unknown-key.yaml")
	s.daemon.setConfigPath(bad)
	_, errs := checkConfig(t, bad)
	if len(errs) == 0 {
		t.Fatalf("%s has no errors", bad)
	}
	r = s.nexusCLI(t, s.reader, "nexusd", "validate-config", "-o", "yaml")
	if r.code != 0 || !strings.Contains(r.stdout, "valid: false") {
		t.Errorf("invalid config: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, e := range errs {
		for _, want := range []string{"file: " + e.File, fmt.Sprintf("line: %d", e.Line),
			fmt.Sprintf("column: %d", e.Column), "message: " + e.Msg} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("invalid config output lacks %q:\n%s", want, r.stdout)
			}
		}
	}

	// Step 5: the invalid file is refused with its first error located,
	// and nothing is created.
	before := len(s.operations(t))
	r = s.nexusCLI(t, s.admin, "nexusd", "reload")
	if r.code != 1 {
		t.Fatalf("invalid reload: exit %d, want 1: %s", r.code, r.stderr)
	}
	p := cliProblem(t, r)
	first, _ := p["errors"].([]any)
	if slugOf(p) != string(problem.ConfigInvalid) || len(first) == 0 {
		t.Fatalf("invalid reload: stderr %s, want config-invalid with errors", r.stderr)
	}
	loc, _ := first[0].(map[string]any)
	if got, want := fmt.Sprintf("%v:%v:%v", loc["file"], loc["line"], loc["column"]),
		fmt.Sprintf("%s:%d:%d", errs[0].File, errs[0].Line, errs[0].Column); got != want {
		t.Errorf("first error at %s, want %s", got, want)
	}
	if n := len(s.operations(t)); n != before {
		t.Errorf("the invalid reload created %d operations", n-before)
	}
	s.daemon.setConfigPath(fixture(t, "valid.yaml"))
	r = s.nexusCLI(t, s.admin, "nexusd", "reload", "-o", "json")
	if r.code != 0 {
		t.Fatalf("valid reload: exit %d: %s", r.code, r.stderr)
	}
	ops := decodeJSON[[]apitypes.Operation](t, []byte(r.stdout))
	if len(ops) != 1 || ops[0].Status != apitypes.StatusSucceeded {
		t.Fatalf("valid reload printed %s", r.stdout)
	}
	res := decodeJSON[spec.ReloadResult](t, ops[0].Result)
	if !res.Applied || !slices.Equal(res.RestartRequired, []string{"limits"}) {
		t.Errorf("reload result %s, want applied with restartRequired [limits]", ops[0].Result)
	}
}

// TC-SM-98 (WI 6.1; FR-7, FR-13, FR-16): CLI end to end: update refusals
// map to problem types and exit codes, and only the admitted update
// creates an operation.
func TestTCSM98_CLIEndToEndUpdateRefusals(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k, k2 := pkgKey(seedTrusted), pkgKey(seedUntrusted)
	dir := t.TempDir()
	good := writePackage(t, dir, "good-v9.9.2.tar.gz", updatePackage(t, "v9.9.2", "v9.9.2"), k)
	untrusted := writePackage(t, dir, "untrusted.tar.gz", updatePackage(t, "v9.9.2", "v9.9.2"), k2)
	traversal := writePackage(t, dir, "traversal.tar.gz", updatePackage(t, "v9.9.2", "v9.9.2",
		pkgFile(updateTop("v9.9.2")+"/../../evil", "x", 0o644)), k)
	old := writePackage(t, dir, "old-v9.9.0.tar.gz", updatePackage(t, "v9.9.0", "v9.9.0"), k)

	old2 := writePackage(t, dir, "old-v9.8.0.tar.gz", updatePackage(t, "v9.8.0", "v9.8.0"), k)

	// Step 1 runs on a host with no trusted key, and steps 2 to 5 on one
	// with K trusted. Step 4's and step 4b's refused uploads stay staged
	// (version-downgrade keeps the upload), so step 5's retry meets the bound
	// of 2 staged uploads and is answered by the staged copy (A-26, TC-SM-E3).
	unconfigured := newUpdateStack(t, updateStackConfig{})
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	update := func(t *testing.T, args ...string) cliResult {
		t.Helper()
		return u.nexusCLI(t, u.admin, append([]string{"nexusd", "update", "--yes"}, args...)...)
	}
	refused := func(t *testing.T, r cliResult, slug problem.Slug) map[string]any {
		t.Helper()
		if r.code != 1 {
			t.Fatalf("exit %d, want 1: %s", r.code, r.stderr)
		}
		p := cliProblem(t, r)
		if slugOf(p) != string(slug) {
			t.Fatalf("stderr %s, want %s", r.stderr, slug)
		}
		return p
	}

	// Step 1: no trusted key.
	refused(t, unconfigured.nexusCLI(t, unconfigured.admin, "nexusd", "update", "--yes", "--file", good),
		problem.UpdateNotConfigured)
	if ops := unconfigured.operations(t); len(ops) != 0 {
		t.Fatalf("the unconfigured update created operations %+v", ops)
	}

	// Steps 2 to 4, with K trusted.
	refused(t, update(t, "--file", untrusted), problem.UpdateSignatureInvalid)
	if p := refused(t, update(t, "--file", traversal), problem.UpdatePackageInvalid); p["check"] != "path" {
		t.Errorf("traversal: check %v, want path", p["check"])
	}
	p := refused(t, update(t, "--file", old), problem.VersionDowngrade)
	if p["runningVersion"] != "v9.9.1" || p["packageVersion"] != "v9.9.0" {
		t.Errorf("downgrade problem %v, want v9.9.1 and v9.9.0", p)
	}
	// Step 4b: a second distinct refused upload, so 2 are staged.
	refused(t, update(t, "--file", old2), problem.VersionDowngrade)
	if ops := u.operations(t); len(ops) != 0 {
		t.Fatalf("refused updates created operations %+v", ops)
	}
	if n := len(stagingEntries(t, u.stagingDir())); n != 2 {
		t.Fatalf("staging holds %d uploads, want the 2 refused ones", n)
	}

	// Step 5: at the bound, the retry reuses the staged upload (A-26).
	r := update(t, "--file", old, "--allow-downgrade", "--no-wait")
	if r.code != 0 {
		t.Fatalf("downgrade with the flag: exit %d: %s", r.code, r.stderr)
	}
	op := decodeJSON[apitypes.Operation](t, []byte(r.stdout))
	if op.ID == "" || op.Action != spec.Update.Op().ID || op.Status.Terminal() {
		t.Errorf("printed %s, want the queued update operation", r.stdout)
	}

	// Only the step-5 operation exists, through the CLI too.
	r = u.nexusCLI(t, u.admin, "operation", "get")
	if r.code != 0 {
		t.Fatalf("operation get: exit %d: %s", r.code, r.stderr)
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		o := decodeJSON[apitypes.Operation](t, []byte(l))
		if o.Provider == spec.Name {
			ids = append(ids, o.ID)
		}
	}
	if !slices.Equal(ids, []string{op.ID}) {
		t.Errorf("nexusd operations %v, want only %s", ids, op.ID)
	}
}

// TC-SM-E11 (WI 6.1; FR-11, FR-6): a CLI update retried with the same
// idempotency key replays the same operation; the retry's upload is
// deduplicated onto the staged one.
func TestTCSME11_ACLIUpdateRetriedWithThe(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	good := writePackage(t, t.TempDir(), "good-v9.9.2.tar.gz", updatePackage(t, "v9.9.2", "v9.9.2"), k)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})

	var ids []string
	for i := range 2 {
		r := u.nexusCLI(t, u.admin, "nexusd", "update", "--yes", "--file", good, "--idempotency-key", "k1", "--no-wait")
		if r.code != 0 {
			t.Fatalf("run %d: exit %d: %s", i+1, r.code, r.stderr)
		}
		ids = append(ids, decodeJSON[apitypes.Operation](t, []byte(r.stdout)).ID)
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("operation IDs %v, want the same one twice", ids)
	}
	if entries := stagingEntries(t, u.stagingDir()); len(entries) != 1 {
		t.Errorf("staging holds %v, want one upload", entries)
	}
	if ops := u.operations(t); len(ops) != 1 || ops[0].ID != ids[0] {
		t.Errorf("operations %+v, want only %s", ops, ids[0])
	}
	if outs := outcomes(u.auditOf(t, spec.Upload.Op().ID)); countOf(outs, "stored") != 2 {
		t.Errorf("upload audit outcomes %v, want a stored record per run", outs)
	}
	if outs := outcomes(u.auditOf(t, spec.Update.Op().ID)); countOf(outs, "requested") != 1 {
		t.Errorf("update audit outcomes %v, want one requested record", outs)
	}
}

// TC-SM-E15 (WI 6.1; FR-12, FR-13): rollback is destructive, so without a
// terminal it needs --yes and sends nothing without it.
func TestTCSME15_CLIRollbackIsDestructiveAndRequires(t *testing.T) {
	spy := &launchSpy{}
	s := newStack(t, stackConfig{launch: spy.launch})
	writeScript(t, s.paths.Prev, `case "$1" in validate-config) exit 0;; -version) echo "nexusd v9.9.0";; esac`)

	// Step 1.
	r := s.nexusCLI(t, s.admin, "nexusd", "rollback")
	if r.code != 2 || !strings.Contains(r.stderr, "needs confirmation") || !strings.Contains(r.stderr, "nothing was sent") {
		t.Errorf("without --yes: exit %d %s, want 2 and the confirmation message", r.code, r.stderr)
	}
	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if recs := s.auditOf(t, spec.Rollback.Op().ID); len(recs) != 0 {
		t.Errorf("rollback audit records %v, want none: a request was sent", outcomes(recs))
	}

	// Step 2.
	r = s.nexusCLI(t, s.admin, "nexusd", "rollback", "--yes", "--no-wait")
	if r.code != 0 {
		t.Fatalf("with --yes: exit %d: %s", r.code, r.stderr)
	}
	op := decodeJSON[apitypes.Operation](t, []byte(r.stdout))
	if op.Action != spec.Rollback.Op().ID {
		t.Errorf("printed %s, want the rollback operation", r.stdout)
	}
	if ops := s.operations(t); len(ops) != 1 || ops[0].ID != op.ID {
		t.Errorf("operations %+v, want only %s", ops, op.ID)
	}
	spy.await(t, op.ID)
	if outs := outcomes(s.auditOf(t, spec.Rollback.Op().ID)); !reflect.DeepEqual(outs[:1], []string{"requested"}) {
		t.Errorf("rollback audit outcomes %v, want requested first", outs)
	}
}
