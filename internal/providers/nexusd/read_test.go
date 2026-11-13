package nexusd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/release"
	"github.com/mauktik/nexus/internal/version"
)

// secretSentinel is the catalog env value no output may echo (test-strategy
// Test Data).
const secretSentinel = "SENTINEL-7f3a9c-DO-NOT-ECHO"

// trustedKey is the encoded public key of a fixed test seed.
func trustedKey(t *testing.T) string {
	t.Helper()
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	return release.EncodePublicKey(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
}

// writeConfig writes a config file in a temp dir from the valid fixture
// plus extra YAML under providers.nexusd.
func writeConfig(t *testing.T, nexusdSection string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: auto\nproviders:\n  nexusd:\n" + nexusdSection
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TC-SM-51 (WI 3.5; FR-2, FR-15): get over REST returns the daemon fields and reflects a second state
func TestTCSM51_GetOverRESTReturnsTheDaemon(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	cfgPath := fixture(t, "valid.yaml")
	d := newFakeDaemon(cfgPath, t.TempDir(), t0)
	s := newStack(t, stackConfig{
		daemon:  d,
		section: writeConfig(t, "    enabled: true\n    update:\n      trustedKeys: []\n"),
		opts:    Options{Now: func() time.Time { return t0.Add(90 * time.Second) }},
	})

	// Step 1.
	got := s.getDaemon(t, s.reader)
	if got.ID != "/providers/nexusd/daemons/nexusd" || got.Type != spec.DaemonsType || got.Name != "nexusd" ||
		got.Host != StackHost {
		t.Errorf("resource %s %s %s on %s", got.ID, got.Type, got.Name, got.Host)
	}
	pr := got.Properties
	if pr.Version != version.Version || !pr.StartedAt.Equal(t0) || pr.UptimeSeconds != 90 ||
		pr.ConfigPath != cfgPath || pr.ExecutablePath != "/x/nexusd" {
		t.Errorf("properties %+v, want version %s, startedAt %s, uptime 90, config %s, executable /x/nexusd",
			pr, version.Version, t0, cfgPath)
	}
	if pr.Previous != (spec.Previous{}) || pr.UpdateConfigured || !pr.SelfManagement.Available {
		t.Errorf("previous %+v updateConfigured %v selfManagement %+v, want absent, false, available",
			pr.Previous, pr.UpdateConfigured, pr.SelfManagement)
	}

	// Step 2: a rollback binary appears and a trusted key is configured,
	// applied through Configure as a reload does.
	writeScript(t, s.paths.Prev, `echo "nexusd v9.9.0"`)
	s.apply(t, writeConfig(t, "    enabled: true\n    update:\n      trustedKeys: [\""+trustedKey(t)+"\"]\n"))

	// Step 3.
	pr = s.getDaemon(t, s.reader).Properties
	if want := (spec.Previous{Present: true, Path: s.paths.Prev, Version: "v9.9.0"}); pr.Previous != want {
		t.Errorf("previous %+v, want %+v", pr.Previous, want)
	}
	if !pr.UpdateConfigured {
		t.Error("updateConfigured false after a trusted key was applied")
	}
}

// TC-SM-52 (WI 3.5; FR-2, NFR-2): The prev-version lookup is cached by file identity and reports unknown after 5 s
func TestTCSM52_ThePrevVersionLookupIsCached(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	run := newFakeRunner(stdoutReply("nexusd v9.9.0\n"))
	s := newStack(t, stackConfig{opts: Options{Runner: run}, clock: clk})
	prev := s.paths.Prev
	if err := os.WriteFile(prev, []byte("binary one"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Step 1.
	for range 2 {
		if v := s.getDaemon(t, s.reader).Properties.Previous.Version; v != "v9.9.0" {
			t.Fatalf("version %q, want v9.9.0", v)
		}
	}
	if n := run.execs(prev); n != 1 {
		t.Fatalf("step 1: %d execs of prev, want 1", n)
	}

	// Step 2.
	fi, err := os.Stat(prev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(prev, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.getDaemon(t, s.reader)
	if n := run.execs(prev); n != 2 {
		t.Fatalf("step 2: %d execs of prev, want 2", n)
	}

	// Step 3: "-version" hangs; the 5 s timer on the virtual clock ends it.
	entered := make(chan struct{}, 1)
	run.setReply(func(ctx context.Context, _ []string) (RunResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return RunResult{ExitCode: -1}, context.Cause(ctx)
	})
	if err := os.WriteFile(prev, []byte("binary two, longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	type result struct {
		d   spec.Daemon
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := s.rawGet()
		done <- result{r, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe never ran")
	}
	clk.Advance(t, versionTimeout-time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("get returned before the 5 s timer fired: %+v %v", r.d, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	clk.Advance(t, time.Millisecond)
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if want := (spec.Previous{Present: true, Path: prev, Version: VersionUnknown}); r.d.Properties.Previous != want {
			t.Errorf("previous %+v, want %+v", r.d.Properties.Previous, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("get did not return after the 5 s timer fired")
	}
	if n := run.execs(prev); n != 3 {
		t.Errorf("step 3: %d execs of prev, want 3", n)
	}
	if n := clk.pending(); n != 0 {
		t.Errorf("%d timers still armed after the probe ended", n)
	}
}

// rawGet is getDaemon for another goroutine: it reports failures as an
// error instead of failing the test.
func (s *stack) rawGet() (spec.Daemon, error) {
	req, err := http.NewRequest(http.MethodGet, s.url+"/v1/providers/nexusd/daemons", nil)
	if err != nil {
		return spec.Daemon{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.reader)
	resp, err := s.http.Do(req)
	if err != nil {
		return spec.Daemon{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return spec.Daemon{}, errors.New(resp.Status)
	}
	var l apitypes.List[spec.Daemon]
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return spec.Daemon{}, err
	}
	if len(l.Items) != 1 {
		return spec.Daemon{}, errors.New("not one item")
	}
	return l.Items[0], nil
}

// The cache is keyed by file identity: a rename over the file with the same
// size and time is a new key, and a canceled caller does not cache unknown.
func TestPrevCacheKey(t *testing.T) {
	run := newFakeRunner(stdoutReply("nexusd v9.9.0\n"))
	s := newStack(t, stackConfig{opts: Options{Runner: run}})
	prev := s.paths.Prev
	if err := os.WriteFile(prev, []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.getDaemon(t, s.reader)
	fi, err := os.Stat(prev)
	if err != nil {
		t.Fatal(err)
	}
	tmp := prev + ".tmp"
	if err := os.WriteFile(tmp, []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, prev); err != nil {
		t.Fatal(err)
	}
	s.getDaemon(t, s.reader)
	if n := run.execs(prev); n != 2 {
		t.Errorf("%d execs after a rename over prev, want 2", n)
	}

	// A caller that leaves mid-probe caches nothing.
	run.setReply(func(ctx context.Context, _ []string) (RunResult, error) {
		<-ctx.Done()
		return RunResult{ExitCode: -1}, ctx.Err()
	})
	if err := os.WriteFile(prev, []byte("third content"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if got := s.p.previous(ctx); got.Version != VersionUnknown {
		t.Errorf("version %q, want unknown", got.Version)
	}
	run.setReply(stdoutReply("nexusd v9.9.3\n"))
	if got := s.p.previous(t.Context()); got.Version != "v9.9.3" {
		t.Errorf("version %q after a canceled probe, want v9.9.3 (unknown was cached)", got.Version)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		out  string
		want string
		ok   bool
	}{
		{"nexusd v9.9.1\n", "v9.9.1", true},
		{"nexusd v1.2.3-rc.1", "v1.2.3-rc.1", true},
		{"nexusd dev\n", "dev", true},
		{"hello\n", "", false},
		{"nexusd \n", "", false},
		{"nexusd v1 extra\n", "", false},
		{"nexusd v1\nnexusd v2\n", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := parseVersion([]byte(tc.out))
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseVersion(%q) = %q, %v; want %q, %v", tc.out, got, ok, tc.want, tc.ok)
		}
	}
}

// TC-SM-53 (WI 3.5; FR-3): validate-config over REST returns valid:false with status 200 for an invalid file
func TestTCSM53_ValidateConfigOverRESTReturnsValid(t *testing.T) {
	valid := fixture(t, "valid.yaml")
	s := newStack(t, stackConfig{daemon: newFakeDaemon(valid, t.TempDir(), time.Now())})

	// Step 1.
	r := s.action(t, "validate-config", s.reader, nil).want(t, http.StatusOK)
	if !bytes.Contains(r.body, []byte(`"errors":[]`)) {
		t.Errorf("errors is not an empty array: %s", r.body)
	}
	if got := decodeJSON[spec.ValidateResult](t, r.body); !reflect.DeepEqual(got,
		spec.ValidateResult{Host: StackHost, Valid: true, ConfigPath: valid, Errors: []spec.ConfigError{}}) {
		t.Errorf("result %+v", got)
	}

	// Step 2.
	bad := fixture(t, "unknown-key.yaml")
	s.daemon.setConfigPath(bad)
	_, errs := checkConfig(t, bad)
	if len(errs) == 0 || errs[0].Line == 0 || errs[0].Column == 0 {
		t.Fatalf("CheckConfig(%s) = %v, want a located error", bad, errs)
	}
	r = s.action(t, "validate-config", s.reader, nil).want(t, http.StatusOK)
	got := decodeJSON[spec.ValidateResult](t, r.body)
	want := spec.ConfigError{File: bad, Line: errs[0].Line, Column: errs[0].Column, Path: "bogus", Message: errs[0].Msg}
	if got.Valid || got.ConfigPath != bad || len(got.Errors) != len(errs) || got.Errors[0] != want {
		t.Errorf("result %+v, want valid false and errors[0] %+v", got, want)
	}

	// Location fields that are 0 are absent: a provider-level error has no
	// line or column.
	perr := fixture(t, "provider-error.yaml")
	s.daemon.setConfigPath(perr)
	raw := decodeJSON[struct {
		Valid  bool             `json:"valid"`
		Errors []map[string]any `json:"errors"`
	}](t, s.action(t, "validate-config", s.reader, nil).want(t, http.StatusOK).body)
	if raw.Valid || len(raw.Errors) != 1 {
		t.Fatalf("provider-error.yaml: %+v, want one error", raw)
	}
	e := raw.Errors[0]
	if _, has := e["line"]; has {
		t.Errorf("line present: %v", e)
	}
	if _, has := e["column"]; has {
		t.Errorf("column present: %v", e)
	}
	if e["path"] != "providers.docker" || e["file"] != perr || e["message"] == "" {
		t.Errorf("provider error %v, want file %s and path providers.docker", e, perr)
	}

	// Step 3: the op takes no input, so a path is refused (NSM FR-25).
	r = s.do(t, http.MethodPost, "/providers/nexusd/actions/validate-config?path=/etc/shadow", s.reader, "", nil).
		want(t, http.StatusBadRequest)
	if slug := slugOf(problemOf(t, r.body)); slug != string(problem.ValidationFailed) {
		t.Errorf("slug %s, want validation-failed", slug)
	}
}

// TC-SM-ERR24 step 3 (WI 3.2, carried to 3.5; FR-3): nexusd validate-config over REST never echoes the secret sentinel
func TestTCSMERR24_ProviderLevelErrorsAreMappedTo(t *testing.T) {
	s := newStack(t, stackConfig{})
	for _, name := range []string{"secret-sentinel.yaml", "secret-sentinel-provider.yaml"} {
		t.Run(name, func(t *testing.T) {
			path := fixture(t, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(secretSentinel)) {
				t.Fatalf("%s does not hold the sentinel", name)
			}
			s.daemon.setConfigPath(path)
			r := s.action(t, "validate-config", s.reader, nil).want(t, http.StatusOK)
			if bytes.Contains(r.body, []byte(secretSentinel)) {
				t.Fatalf("the sentinel is in the output: %s", r.body)
			}
			got := decodeJSON[spec.ValidateResult](t, r.body)
			if got.Valid || len(got.Errors) == 0 {
				t.Fatalf("result %+v, want invalid", got)
			}
			if name == "secret-sentinel-provider.yaml" {
				want := spec.ConfigError{File: path, Path: "providers.docker"}
				found := false
				for _, e := range got.Errors {
					found = found || (e.File == want.File && e.Path == want.Path && e.Line == 0 && e.Message != "")
				}
				if !found {
					t.Errorf("errors %+v, want an entry at %s with no line", got.Errors, want.Path)
				}
			}
		})
	}
}

// TC-SM-55 (WI 3.5; FR-1): Preflight is always enabled with selfManagement details, and reads work without systemd
func TestTCSM55_PreflightIsAlwaysEnabledWithSelfManagement(t *testing.T) {
	const reason = "systemd not running (/run/systemd/system missing)"
	for _, run := range []struct {
		name    string
		systemd detach.Availability
	}{
		{"A", detach.Availability{Available: true}},
		{"B", detach.Availability{Reason: reason}},
	} {
		t.Run(run.name, func(t *testing.T) {
			s := newStack(t, stackConfig{opts: Options{Systemd: func() detach.Availability { return run.systemd }}})

			// Step 1.
			caps := decodeJSON[apitypes.Capabilities](t, s.do(t, http.MethodGet, "/capabilities", s.reader, "", nil).
				want(t, http.StatusOK).body)
			var pc *apitypes.ProviderCapability
			for i := range caps.Providers {
				if caps.Providers[i].Name == spec.Name {
					pc = &caps.Providers[i]
				}
			}
			if pc == nil {
				t.Fatalf("no nexusd in %+v", caps.Providers)
			}
			if pc.Status != apitypes.ProviderEnabled {
				t.Errorf("status %s, want enabled", pc.Status)
			}
			wantDetails := map[string]any{DetailSelfManagement: "available"}
			if !run.systemd.Available {
				wantDetails = map[string]any{DetailSelfManagement: "unavailable", DetailReason: reason}
			}
			if !reflect.DeepEqual(pc.Details, wantDetails) {
				t.Errorf("details %v, want %v", pc.Details, wantDetails)
			}

			// Step 2.
			sm := s.getDaemon(t, s.reader).Properties.SelfManagement
			if want := (spec.Availability{Available: run.systemd.Available, Reason: run.systemd.Reason}); sm != want {
				t.Errorf("selfManagement %+v, want %+v", sm, want)
			}

			// Step 3.
			s.action(t, "validate-config", s.reader, nil).want(t, http.StatusOK)
		})
	}
}

// TC-SM-E7 (WI 3.5; FR-2): A prev binary that is not executable or prints garbage reports previous.version unknown
func TestTCSME7_APrevBinaryThatIsNot(t *testing.T) {
	s := newStack(t, stackConfig{})
	prev := s.paths.Prev
	cases := []struct {
		name  string
		write func()
	}{
		{"mode 0644", func() {
			if err := os.WriteFile(prev, []byte("#!/bin/sh\necho \"nexusd v9.9.0\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"prints hello", func() { writeScript(t, prev, "echo hello") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			tc.write()
			got := s.getDaemon(t, s.reader).Properties.Previous
			if want := (spec.Previous{Present: true, Path: prev, Version: VersionUnknown}); got != want {
				t.Errorf("previous %+v, want %+v", got, want)
			}
		})
	}
}

// The real Runner runs a stand-in with the fixed environment and reports a
// non-zero exit as a result.
func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "stand-in")
	writeScript(t, bin, `echo "nexusd v9.9.1"; echo "PATH=$PATH" >&2; exit 3`)
	res, err := execRunner{}.Run(t.Context(), []string{bin, "-version"})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "nexusd v9.9.1\n" || res.ExitCode != 3 ||
		!strings.Contains(string(res.Stderr), "PATH=/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Errorf("result %+v (stdout %q stderr %q)", res, res.Stdout, res.Stderr)
	}
	if _, err := (execRunner{}).Run(t.Context(), []string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("running a missing program succeeded")
	}
}

// TC-SM-E14 (WI 3.5; FR-1): Not running as nexusd.service makes self-management unavailable with a reason
func TestTCSME14_NotRunningAsNexusdServiceMakes(t *testing.T) {
	const reason = "not running in nexusd.service (cgroup /user.slice/x.scope)"
	s := newStack(t, stackConfig{opts: Options{InService: func() error { return errors.New(reason) }}})

	// Step 1.
	sm := s.getDaemon(t, s.reader).Properties.SelfManagement
	if sm.Available || sm.Reason != reason || !strings.Contains(sm.Reason, "cgroup") {
		t.Errorf("selfManagement %+v, want unavailable with %q", sm, reason)
	}

	// Step 2, at the shared admission check: restart (WI 4.2), update,
	// rollback and the upload refuse through requireSelfManagement, which
	// answers 503 capability-unavailable with the same reason. The REST
	// restart request itself is asserted by WI 4.2, which binds restart.
	err := s.p.requireSelfManagement()
	var perr *problem.Error
	if !errors.As(err, &perr) {
		t.Fatalf("requireSelfManagement = %v, want a problem", err)
	}
	if perr.Slug() != problem.CapabilityUnavailable || perr.Status != http.StatusServiceUnavailable ||
		perr.Extensions["reason"] != reason || perr.Extensions["provider"] != spec.Name {
		t.Errorf("problem %+v, want 503 capability-unavailable with reason %q", perr, reason)
	}

	ok := newStack(t, stackConfig{})
	if err := ok.p.requireSelfManagement(); err != nil {
		t.Errorf("requireSelfManagement with systemd and the unit = %v, want nil", err)
	}
}

// The default InService check reads the process's cgroups (A-32).
func TestCheckCgroup(t *testing.T) {
	tests := []struct {
		name, content string
		ok            bool
		reason        string
	}{
		{"v2 in the unit", "0::/system.slice/nexusd.service\n", true, ""},
		{"v2 elsewhere", "0::/user.slice/user-1000.slice/session-2.scope\n", false,
			"not running in nexusd.service (cgroup /user.slice/user-1000.slice/session-2.scope)"},
		{"v2 another unit", "0::/system.slice/nexusd.service.d/x\n", false,
			"not running in nexusd.service (cgroup /system.slice/nexusd.service.d/x)"},
		{"suffix only", "0::/system.slice/notnexusd.service\n", false,
			"not running in nexusd.service (cgroup /system.slice/notnexusd.service)"},
		{"hybrid", "12:cpu,cpuacct:/\n1:name=systemd:/system.slice/nexusd.service\n0::/system.slice/nexusd.service\n", true, ""},
		{"v1 elsewhere, v2 line reported", "1:name=systemd:/init.scope\n0::/init.scope\n", false,
			"not running in nexusd.service (cgroup /init.scope)"},
		{"empty", "", false, "no cgroup listed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cgroup")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := checkCgroup(path, "nexusd.service")
			if (err == nil) != tc.ok || (err != nil && !strings.Contains(err.Error(), tc.reason)) {
				t.Errorf("checkCgroup = %v, want ok %v reason %q", err, tc.ok, tc.reason)
			}
		})
	}
	err := checkCgroup(filepath.Join(t.TempDir(), "missing"), "nexusd.service")
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether nexusd runs as nexusd.service") {
		t.Errorf("missing file: %v", err)
	}
}

// Configure keeps the parsed keys, and a later section replaces them.
func TestConfigureKeepsParsedKeys(t *testing.T) {
	key := trustedKey(t)
	p := New(Options{})
	if len(p.current().keys) != 0 || p.current().cfg.HealthTimeout != spec.DefaultHealthTimeout {
		t.Fatalf("defaults %+v", p.current())
	}
	if err := p.Configure(func(into any) error {
		into.(*config.NexusdConfig).Update.TrustedKeys = []string{key}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want, err := release.ParsePublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if ks := p.current().keys; len(ks) != 1 || !ks[0].Equal(want) {
		t.Errorf("keys %v, want the parsed key", ks)
	}
	if err := p.Configure(func(any) error { return errors.New("refused") }); err == nil {
		t.Fatal("Configure accepted a refused section")
	}
	if len(p.current().keys) != 1 {
		t.Error("a refused section changed the settings in force")
	}
	if err := p.Configure(func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(p.current().keys) != 0 {
		t.Error("a section with no keys kept the old ones")
	}
}
