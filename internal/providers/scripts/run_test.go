// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"context"
	"encoding/json"
	"errors"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
)

// runnerFixture is a provider whose catalog holds one clean script, run as
// the current user, with a staged identity.
type runnerFixture struct {
	p      *Provider
	path   string // The script, resolved
	id     identity
	euid   int
	exists map[string]bool
}

func newRunnerFixture(t *testing.T, entry config.Script) *runnerFixture {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	base := cleanBase(t)
	path := script(t, filepath.Join(base, "deploy.sh"), 0o755)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	f := &runnerFixture{
		path: resolved,
		id: identity{name: u.Username, uid: uint32(uid), gid: uint32(gid), groups: []uint32{uint32(gid), 4242},
			home: "/home/" + u.Username, shell: "/bin/bash"},
		euid:   0,
		exists: map[string]bool{},
	}
	entry.ID, entry.Description, entry.Path, entry.RunAs = "deploy", "Deploy", path, u.Username
	if entry.WorkingDirectory == "" {
		entry.WorkingDirectory = base
	}
	c, err := NewCatalog([]config.Script{entry})
	if err != nil {
		t.Fatal(err)
	}
	f.p = New(Options{Host: "testhost"})
	f.p.catalog.Store(c)
	f.p.machine = machine{
		identify: func(name string) (identity, error) {
			if name != f.id.name {
				return identity{}, errors.New("unknown user " + name)
			}
			return f.id, nil
		},
		exists:  func(p string) bool { return f.exists[p] },
		isDir:   realMachine.isDir,
		geteuid: func() int { return f.euid },
	}
	return f
}

// TC-94, TC-97 and A-63 at T0: Prepare re-checks integrity and executes
// the resolved path with the mapped argv, the exact defined environment,
// the run-as credential with its supplementary groups, the catalog's
// timeout and grace, control-group kill mode and timeout + grace + 60s.
func TestPrepareBuildsTheRunSpec(t *testing.T) {
	f := newRunnerFixture(t, config.Script{
		Timeout: 2 * time.Second, TerminationGrace: time.Second,
		Env: map[string]string{"PATH": "/opt/tools/bin:/usr/bin:/bin", "API_TOKEN": "tok"},
		Parameters: []config.Parameter{
			{Name: "branch", Type: "string", Pattern: `^[a-z]+$`, Arg: "--branch"},
			{Name: "target", Type: "string", Pattern: `^[a-z]+$`},
			{Name: "apiKey", Type: "string", Pattern: `^[A-Za-z0-9]+$`, Sensitive: true, Env: "API_KEY"},
		},
	})
	f.exists[runtimeDir(f.id.uid)] = true
	in := spec.InvokeInput{Name: "deploy", Parameters: map[string]string{"branch": "main", "target": "api", "apiKey": "k3y"}}
	if err := f.p.admitInvoke(context.Background(), in); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	got, err := f.p.prepareInvoke(context.Background(), in, provider.NewRun("op-1", nil))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := []string{f.path, "--branch", "main", "api"}; !slices.Equal(got.Argv, want) {
		t.Errorf("argv %q, want %q", got.Argv, want)
	}
	want := []string{
		"API_KEY=k3y", "API_TOKEN=tok", "HOME=/home/" + f.id.name, "LANG=C.UTF-8", "LOGNAME=" + f.id.name,
		"PATH=/opt/tools/bin:/usr/bin:/bin", "SHELL=/bin/bash", "TERM=dumb", "USER=" + f.id.name,
		"XDG_RUNTIME_DIR=" + runtimeDir(f.id.uid),
	}
	if !slices.Equal(got.Env, want) {
		t.Errorf("env\n got %q\nwant %q", got.Env, want)
	}
	if got.Credential == nil || got.Credential.UID != f.id.uid || got.Credential.GID != f.id.gid ||
		!slices.Equal(got.Credential.Groups, f.id.groups) {
		t.Errorf("credential %+v, want uid %d gid %d groups %v", got.Credential, f.id.uid, f.id.gid, f.id.groups)
	}
	if got.Timeout != 2*time.Second || got.Grace != time.Second || got.KillMode != detach.KillControlGroup ||
		got.MaxRuntime != 63*time.Second || got.Dir == "" {
		t.Errorf("spec %+v, want timeout 2s, grace 1s, control-group, max runtime 63s and a directory", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TC-97 at T0: with no catalog env and no runtime directory, the
// environment is exactly the defaults; nothing of this process leaks in.
func TestPrepareEnvironmentIsBuiltFromNothing(t *testing.T) {
	t.Setenv("NEXUS_LEAK_CHECK", "leaked")
	f := newRunnerFixture(t, config.Script{})
	got, err := f.p.prepareInvoke(context.Background(), spec.InvokeInput{Name: "deploy"}, provider.NewRun("op-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"HOME=/home/" + f.id.name, "LANG=C.UTF-8", "LOGNAME=" + f.id.name, "PATH=" + DefaultPath,
		"SHELL=/bin/bash", "TERM=dumb", "USER=" + f.id.name,
	}
	if !slices.Equal(got.Env, want) {
		t.Errorf("env\n got %q\nwant %q", got.Env, want)
	}
}

// A run-as user that is this unprivileged process itself runs without a
// credential switch (T0 only: nexusd runs as root); any other is switched.
func TestPrepareCredential(t *testing.T) {
	f := newRunnerFixture(t, config.Script{})
	f.euid = int(f.id.uid)
	if f.id.uid == 0 {
		t.Skip("running as root")
	}
	got, err := f.p.prepareInvoke(context.Background(), spec.InvokeInput{Name: "deploy"}, provider.NewRun("op-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Credential != nil {
		t.Errorf("credential %+v for the process's own user, want none", got.Credential)
	}
}

// Prepare repeats every Admit check, so a change between them refuses the
// run: an invalid parameter, a script made writable (the check-to-exec
// gap), a script removed by a reload, and an unresolvable run-as user.
func TestPrepareRechecks(t *testing.T) {
	run := provider.NewRun("op-1", nil)
	t.Run("parameters", func(t *testing.T) {
		f := newRunnerFixture(t, config.Script{})
		_, err := f.p.prepareInvoke(context.Background(), spec.InvokeInput{Name: "deploy",
			Parameters: map[string]string{"x": "1"}}, run)
		wantSlug(t, err, problem.ValidationFailed)
	})
	t.Run("integrity", func(t *testing.T) {
		f := newRunnerFixture(t, config.Script{})
		in := spec.InvokeInput{Name: "deploy"}
		if err := f.p.admitInvoke(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		chmod(t, f.path, 0o757)
		_, err := f.p.prepareInvoke(context.Background(), in, run)
		p := wantSlug(t, err, problem.ScriptIntegrityFailed)
		if p.Extensions["check"] != CheckScriptWritable {
			t.Errorf("check %v", p.Extensions["check"])
		}
	})
	t.Run("removed", func(t *testing.T) {
		f := newRunnerFixture(t, config.Script{})
		f.p.catalog.Store(&Catalog{index: map[string]int{}})
		_, err := f.p.prepareInvoke(context.Background(), spec.InvokeInput{Name: "deploy"}, run)
		wantSlug(t, err, problem.NotFound)
	})
	t.Run("identity", func(t *testing.T) {
		f := newRunnerFixture(t, config.Script{})
		f.p.machine.identify = func(string) (identity, error) { return identity{}, errors.New("gone") }
		_, err := f.p.prepareInvoke(context.Background(), spec.InvokeInput{Name: "deploy"}, run)
		p := wantSlug(t, err, problem.ScriptIntegrityFailed)
		if p.Extensions["check"] != CheckRunAsUser {
			t.Errorf("check %v", p.Extensions["check"])
		}
	})
}

// Admit refuses an unknown script, invalid parameters and a failed
// integrity check, in that order, before any operation exists.
func TestAdmitInvoke(t *testing.T) {
	f := newRunnerFixture(t, config.Script{Parameters: []config.Parameter{
		{Name: "branch", Type: "string", Pattern: `^[a-z]+$`, Arg: "--branch"},
	}})
	ctx := context.Background()
	wantSlug(t, f.p.admitInvoke(ctx, spec.InvokeInput{Name: "nope"}), problem.NotFound)
	wantSlug(t, f.p.admitInvoke(ctx, spec.InvokeInput{Name: "deploy",
		Parameters: map[string]string{"branch": "$(id)"}}), problem.ValidationFailed)
	if err := f.p.admitInvoke(ctx, spec.InvokeInput{Name: "deploy", Parameters: map[string]string{"branch": "main"}}); err != nil {
		t.Fatalf("clean: %v", err)
	}
	chmod(t, f.path, 0o775)
	wantSlug(t, f.p.admitInvoke(ctx, spec.InvokeInput{Name: "deploy"}), problem.ScriptIntegrityFailed)
}

// FR-19, FR-7: the lane is script:<id>, reject, with the catalog's
// concurrency as its limit.
func TestInvokeLane(t *testing.T) {
	f := newRunnerFixture(t, config.Script{Concurrency: 3})
	l, err := f.p.invokeLane(context.Background(), spec.InvokeInput{Name: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if l != (provider.Lane{Key: "script:deploy", Policy: provider.LaneReject, Limit: 3}) {
		t.Errorf("lane %+v", l)
	}
	_, err = f.p.invokeLane(context.Background(), spec.InvokeInput{Name: "nope"})
	wantSlug(t, err, problem.NotFound)
}

// FR-19, A-6: Finalize maps exit.json alone. Exit 0 is success whatever
// the script printed; non-zero and a signal are script-failed, a timeout
// script-timeout, a start failure script-failed naming the cause, and each
// carries exitCode, signal and durationSeconds.
func TestFinalizeOutcomes(t *testing.T) {
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	in := spec.InvokeInput{Name: "deploy"}
	run := provider.NewRun("op-1", nil)

	out, err := finalizeInvoke(context.Background(), in, detach.Exit{ExitCode: 0, Started: start,
		Ended: start.Add(1500 * time.Millisecond)}, run)
	if err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	if out.ExitCode != 0 || out.Signal != "" || out.DurationSeconds != 1.5 || !out.Changed {
		t.Errorf("exit 0: %+v", out)
	}
	b, _ := json.Marshal(out)
	for _, k := range []string{`"exitCode":0`, `"signal":""`, `"durationSeconds":1.5`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("result %s lacks %s", b, k)
		}
	}

	cases := []struct {
		name   string
		x      detach.Exit
		slug   problem.Slug
		code   int
		signal string
		detail string
	}{
		{"exit 3", detach.Exit{ExitCode: 3}, problem.ScriptFailed, 3, "", "exited with code 3"},
		{"killed", detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, problem.ScriptFailed, -1, "SIGKILL", "SIGKILL"},
		{"timeout", detach.Exit{ExitCode: -1, Signal: "SIGTERM", TimedOut: true}, problem.ScriptTimeout, -1, "SIGTERM", "timeout"},
		{"timeout exiting 0", detach.Exit{ExitCode: 0, TimedOut: true}, problem.ScriptTimeout, 0, "", "timeout"},
		{"not started", detach.Exit{ExitCode: -1, Error: "chdir /missing: no such file or directory"},
			problem.ScriptFailed, -1, "", "chdir /missing: no such file or directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.x.Started, tc.x.Ended = start, start.Add(2*time.Second)
			_, err := finalizeInvoke(context.Background(), in, tc.x, run)
			p := wantSlug(t, err, tc.slug)
			if p.Extensions["exitCode"] != tc.code || p.Extensions["signal"] != tc.signal ||
				p.Extensions["durationSeconds"] != 2.0 {
				t.Errorf("extensions %v, want exitCode %d signal %q durationSeconds 2", p.Extensions, tc.code, tc.signal)
			}
			if !strings.Contains(p.Detail, tc.detail) || !strings.Contains(p.Detail, "deploy") {
				t.Errorf("detail %q lacks %q or the script", p.Detail, tc.detail)
			}
			if _, err := json.Marshal(p); err != nil {
				t.Errorf("marshal: %v", err)
			}
		})
	}
}

// NFR-4, A-66: the redactor hook names exactly the invoked script's
// sensitive parameters, and every supplied parameter when the script is
// not in the catalog, so nothing sensitive is kept by mistake.
func TestSensitivePaths(t *testing.T) {
	f := newRunnerFixture(t, config.Script{Parameters: []config.Parameter{
		{Name: "branch", Type: "string", Pattern: `^[a-z]+$`, Arg: "--branch"},
		{Name: "apiKey", Type: "string", Pattern: `^[A-Za-z0-9]+$`, Sensitive: true, Env: "API_KEY"},
		{Name: "pin", Type: "integer", Sensitive: true, Env: "PIN"},
	}})
	invoke := spec.Invoke.Op().ID
	got := f.p.SensitivePaths(invoke, json.RawMessage(`{"name":"deploy","parameters":{"branch":"main","apiKey":"x"}}`))
	if want := []string{"parameters.apiKey", "parameters.pin"}; !slices.Equal(got, want) {
		t.Errorf("deploy: %q, want %q", got, want)
	}
	got = f.p.SensitivePaths(invoke, json.RawMessage(`{"name":"gone","parameters":{"b":"1","a":"2"}}`))
	if want := []string{"parameters.a", "parameters.b"}; !slices.Equal(got, want) {
		t.Errorf("unknown script: %q, want %q", got, want)
	}
	if got := f.p.SensitivePaths(spec.Get.Op().ID, json.RawMessage(`{"name":"deploy"}`)); got != nil {
		t.Errorf("get: %q", got)
	}
}

// LostDetail names the script, so an outcome-unknown says what may have
// run in part.
func TestLostDetail(t *testing.T) {
	if d := lostDetail(provider.NewRun("op-1", nil), spec.InvokeInput{Name: "deploy"}); !strings.Contains(d, "deploy") {
		t.Errorf("detail %q", d)
	}
}

func wantSlug(t *testing.T, err error, slug problem.Slug) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != slug {
		t.Fatalf("error %v, want %s", err, slug)
	}
	return p
}
