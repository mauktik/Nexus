package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// T0 parts of WI 6.5. The flow against the real Compose plugin (pull,
// compare, up --no-deps, health) runs in T1
// (test/integration/docker_compose_test.go: TC-118, TC-119, TC-216,
// TC-226, TC-E23, TC-ERR41 to TC-ERR44).

// composeProjectDir makes a project directory with compose.yaml and
// nx.env, and returns it with labels as Compose writes them (observed on
// Compose 2.40.3 in T1).
func composeProjectDir(t *testing.T, name string) (string, map[string]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"compose.yaml", "override.yaml", "nx.env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, map[string]string{
		spec.ComposeProjectLabel:     "media",
		spec.ComposeServiceLabel:     "app",
		spec.ComposeNumberLabel:      "1",
		spec.ComposeOneoffLabel:      "False",
		spec.ComposeWorkingDirLabel:  dir,
		spec.ComposeConfigFilesLabel: filepath.Join(dir, "compose.yaml") + "," + filepath.Join(dir, "override.yaml"),
		spec.ComposeEnvFileLabel:     filepath.Join(dir, "nx.env"),
	}
}

func labeled(labels map[string]string) container.InspectResponse {
	return container.InspectResponse{Config: &container.Config{Labels: labels}}
}

func problemOf(t *testing.T, err error) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) {
		t.Fatalf("not a problem: %v", err)
	}
	return p
}

// TC-ERR41 (T0 part): the labels give the project, service, directory and
// files; each missing directory, file or label is compose-files-missing
// (422) naming the path; labels Compose would not write, and a one-off run
// container, are not-updatable.
func TestComposeTargetOf(t *testing.T) {
	dir, labels := composeProjectDir(t, "proj")
	got, err := composeTargetOf(labeled(labels), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := composeTarget{Project: "media", Service: "app", Number: "1", WorkingDir: dir,
		ConfigFiles: []string{filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "override.yaml")},
		EnvFiles:    []string{filepath.Join(dir, "nx.env")}}
	if got.Project != want.Project || got.Service != want.Service || got.Number != want.Number ||
		got.WorkingDir != want.WorkingDir || !slices.Equal(got.ConfigFiles, want.ConfigFiles) ||
		!slices.Equal(got.EnvFiles, want.EnvFiles) {
		t.Fatalf("target %+v, want %+v", got, want)
	}
	noEnv := clone(labels)
	delete(noEnv, spec.ComposeEnvFileLabel)
	if got, err := composeTargetOf(labeled(noEnv), nil); err != nil || got.EnvFiles != nil {
		t.Fatalf("without --env-file: %+v %v", got, err)
	}

	for _, tc := range []struct {
		name   string
		change func(map[string]string)
		slug   problem.Slug
		path   string
	}{
		{"no working dir label", func(l map[string]string) { delete(l, spec.ComposeWorkingDirLabel) }, problem.ComposeFilesMissing, ""},
		{"relative working dir", func(l map[string]string) { l[spec.ComposeWorkingDirLabel] = "proj" }, problem.ComposeFilesMissing, "proj"},
		{"working dir gone", func(l map[string]string) { l[spec.ComposeWorkingDirLabel] = dir + ".moved" }, problem.ComposeFilesMissing, dir + ".moved"},
		{"no config files label", func(l map[string]string) { delete(l, spec.ComposeConfigFilesLabel) }, problem.ComposeFilesMissing, ""},
		{"config file gone", func(l map[string]string) {
			l[spec.ComposeConfigFilesLabel] = filepath.Join(dir, "compose.yaml") + "," + filepath.Join(dir, "gone.yaml")
		}, problem.ComposeFilesMissing, filepath.Join(dir, "gone.yaml")},
		{"config file is a directory", func(l map[string]string) { l[spec.ComposeConfigFilesLabel] = dir }, problem.ComposeFilesMissing, dir},
		{"env file gone", func(l map[string]string) { l[spec.ComposeEnvFileLabel] = filepath.Join(dir, "gone.env") }, problem.ComposeFilesMissing, filepath.Join(dir, "gone.env")},
		{"flag-like service", func(l map[string]string) { l[spec.ComposeServiceLabel] = "-f" }, problem.NotUpdatable, ""},
		{"bad project", func(l map[string]string) { l[spec.ComposeProjectLabel] = "Media;x" }, problem.NotUpdatable, ""},
		{"one-off run", func(l map[string]string) { l[spec.ComposeOneoffLabel] = "True" }, problem.NotUpdatable, ""},
	} {
		l := clone(labels)
		tc.change(l)
		_, err := composeTargetOf(labeled(l), nil)
		p := problemOf(t, err)
		if p.Slug() != tc.slug {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.slug)
			continue
		}
		if tc.slug == problem.ComposeFilesMissing {
			if p.Status != 422 || fmtExt(p, "path") != tc.path || !strings.Contains(p.Detail, "nothing was changed") {
				t.Errorf("%s: status %d path %q detail %q, want path %q", tc.name, p.Status, fmtExt(p, "path"), p.Detail, tc.path)
			}
		} else if p.Extensions["reason"] != spec.ReasonComposeManaged {
			t.Errorf("%s: reason %v", tc.name, p.Extensions["reason"])
		}
	}
}

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func fmtExt(p *problem.Error, k string) string {
	if v, ok := p.Extensions[k]; ok {
		return v.(string)
	}
	return ""
}

func writeExec(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TC-ERR42 (T0 part), CON-6: the docker CLI is looked up in the fixed
// PATH and the plugin where the CLI looks, configDir's cli-plugins first;
// a missing or non-executable one is reported missing with a reason that
// names docker-compose.
func TestFindComposeIn(t *testing.T) {
	root := t.TempDir()
	bin1, bin2 := filepath.Join(root, "bin1"), filepath.Join(root, "bin2")
	sys1, sys2 := filepath.Join(root, "sys1"), filepath.Join(root, "sys2")
	cfg := filepath.Join(root, "cfg")
	dirs := []string{sys1, sys2}

	if got := findComposeIn(cfg, []string{bin1, bin2}, dirs); got.Missing == "" || !strings.Contains(got.Missing, "docker CLI") {
		t.Fatalf("no docker: %+v", got)
	}
	writeExec(t, filepath.Join(bin2, "docker"), "#!/bin/true\n", 0o755)
	got := findComposeIn(cfg, []string{bin1, bin2}, dirs)
	if got.Docker != filepath.Join(bin2, "docker") || !strings.Contains(got.Missing, "docker-compose") ||
		!strings.Contains(got.Missing, filepath.Join(cfg, "cli-plugins")) || !strings.Contains(got.Missing, sys2) {
		t.Fatalf("no plugin: %+v", got)
	}
	writeExec(t, filepath.Join(sys2, "docker-compose"), "x", 0o644)
	if got := findComposeIn(cfg, []string{bin1, bin2}, dirs); got.Missing == "" {
		t.Fatalf("a non-executable plugin counted: %+v", got)
	}
	writeExec(t, filepath.Join(sys2, "docker-compose"), "x", 0o755)
	if got := findComposeIn(cfg, []string{bin1, bin2}, dirs); got.Missing != "" || got.Plugin != filepath.Join(sys2, "docker-compose") {
		t.Fatalf("system plugin: %+v", got)
	}
	writeExec(t, filepath.Join(cfg, "cli-plugins", "docker-compose"), "x", 0o755)
	if got := findComposeIn(cfg, []string{bin1, bin2}, dirs); got.Plugin != filepath.Join(cfg, "cli-plugins", "docker-compose") {
		t.Fatalf("config dir plugin first: %+v", got)
	}
	d := composeDetail(composeTools{Missing: "the Docker Compose plugin (docker-compose) is not installed"})
	if d["available"] != false || !strings.Contains(d["reason"].(string), "docker-compose") {
		t.Fatalf("detail %v", d)
	}
	got = findComposeIn(cfg, []string{bin1, bin2}, dirs)
	if d := composeDetail(got); d["available"] != true || d["path"] != got.Plugin {
		t.Fatalf("detail %v", d)
	}
	if !slices.Equal(composePluginDirs, []string{"/usr/local/lib/docker/cli-plugins", "/usr/local/libexec/docker/cli-plugins",
		"/usr/lib/docker/cli-plugins", "/usr/libexec/docker/cli-plugins"}) {
		t.Fatalf("plugin dirs %v differ from docker/cli's defaultSystemPluginDirs", composePluginDirs)
	}
}

// TC-226 (T0 part), FR-25: each Compose command is an argument vector in
// which a hostile directory, file or service name is exactly one element;
// pull and up have the declared shapes.
func TestComposeArgv(t *testing.T) {
	dir := "/srv/nx;touch /tmp/nx-pwned;x y"
	tg := composeTarget{Project: "media", Service: "app", WorkingDir: dir,
		ConfigFiles: []string{dir + "/compose.yaml", dir + "/b c.yaml"}, EnvFiles: []string{dir + "/$(id).env"}}
	pre := []string{"/usr/bin/docker", "compose", "--project-directory", dir, "-f", dir + "/compose.yaml",
		"-f", dir + "/b c.yaml", "--env-file", dir + "/$(id).env", "-p", "media"}
	if got, want := composeArgv("/usr/bin/docker", tg, "pull"), append(slices.Clone(pre), "pull", "app"); !slices.Equal(got, want) {
		t.Fatalf("pull argv %q, want %q", got, want)
	}
	if got, want := composeArgv("/usr/bin/docker", tg, "up", "-d", "--no-deps"),
		append(slices.Clone(pre), "up", "-d", "--no-deps", "app"); !slices.Equal(got, want) {
		t.Fatalf("up argv %q, want %q", got, want)
	}
	tg.EnvFiles = nil
	if got := composeArgv("/usr/bin/docker", tg, "pull"); slices.Contains(got, "--env-file") {
		t.Fatalf("argv without env files %q", got)
	}
}

// The Compose commands' environment is exactly PATH, HOME, DOCKER_HOST and
// DOCKER_CONFIG (docker.configDir): nothing is inherited.
func TestComposeEnv(t *testing.T) {
	t.Setenv("NX_LEAK", "1")
	env := composeEnv("/etc/nexus/docker", "/run/docker.sock")
	keys := []string{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"DOCKER_CONFIG", "DOCKER_HOST", "HOME", "PATH"}) ||
		!slices.Contains(env, "DOCKER_CONFIG=/etc/nexus/docker") || !slices.Contains(env, "DOCKER_HOST=unix:///run/docker.sock") ||
		!slices.Contains(env, "PATH="+composePath) {
		t.Fatalf("env %q", env)
	}
}

// A Compose command's output reaches the operation's output; a failure
// quotes its stderr; a command past its timeout is killed with its whole
// process group.
func TestUpdaterCompose(t *testing.T) {
	dir := t.TempDir()
	argvFile, childPID := filepath.Join(dir, "argv"), filepath.Join(dir, "child.pid")
	fake := filepath.Join(dir, "docker")
	writeExec(t, fake, `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done >`+argvFile+`
echo "pulling on stdout"
echo "env DOCKER_CONFIG=$DOCKER_CONFIG"
case "$*" in
*fail*) echo "manifest unknown on stderr" >&2; exit 3 ;;
*hang*) sleep 30 & echo $! >`+childPID+`; wait ;;
esac
`, 0o755)
	var out bytes.Buffer
	u := &updater{in: helperInput{ComposeTimeout: time.Minute, ConfigDir: "/etc/nexus/docker", Socket: "/run/docker.sock"}, out: &out}
	tg := composeTarget{Project: "media", Service: "app", WorkingDir: dir, ConfigFiles: []string{dir + "/compose.yaml"}}
	if err := u.compose(context.Background(), fake, tg, "pull"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argvFile)
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); !slices.Equal(got, composeArgv(fake, tg, "pull")[1:]) {
		t.Fatalf("argv %q", got)
	}
	if s := out.String(); !strings.Contains(s, "pulling on stdout") || !strings.Contains(s, "env DOCKER_CONFIG=/etc/nexus/docker") ||
		!strings.Contains(s, "docker compose pull app") {
		t.Fatalf("output %q", s)
	}

	tg.Service = "fail"
	err := u.compose(context.Background(), fake, tg, "pull")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown on stderr") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("failure: %v", err)
	}

	tg.Service = "hang"
	u.in.ComposeTimeout = 500 * time.Millisecond
	start := time.Now()
	err = u.compose(context.Background(), fake, tg, "pull")
	if err == nil || !strings.Contains(err.Error(), "did not finish within 500ms") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	b, _ = os.ReadFile(childPID)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for pid > 0 && syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pid <= 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("the command's child %d outlived the timeout", pid)
	}
}

// composeEngine is a fake engine with a Compose-managed container "app" of
// project media whose files exist, and a standalone "solo".
func composeEngine(t *testing.T) (*updateEngine, string) {
	t.Helper()
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"registry.example/app@" + digestA}
	f.add("app", "registry.example/app:1", imgOne, false)
	f.add("solo", "registry.example/app:1", imgOne, false)
	dir, labels := composeProjectDir(t, "media")
	f.containers["app"]["Config"].(map[string]any)["Labels"] = labels
	return f, dir
}

var (
	toolsPresent = func(string) composeTools {
		return composeTools{Docker: "/usr/bin/docker", Plugin: "/usr/libexec/docker/cli-plugins/docker-compose"}
	}
	toolsMissing = func(string) composeTools {
		return composeTools{Docker: "/usr/bin/docker", Missing: "the Docker Compose plugin (docker-compose) is not installed in any of /x"}
	}
)

// TC-ERR41, TC-ERR42, TC-ERR44 (T0 parts): a Compose-managed container is
// admitted when the plugin and its files are present; without the plugin
// it is 503 compose-plugin-missing naming the provider while a standalone
// update is admitted (CON-6); with a file gone it is 422
// compose-files-missing; its lane is compose:<project> (A-56); admission
// makes no registry call.
func TestAdmitComposeUpdate(t *testing.T) {
	f, dir := composeEngine(t)
	p, _ := updProvider(t, f)
	ctx := context.Background()
	p.composeTools = toolsPresent
	if err := p.admitUpdate(ctx, spec.ContainerInput{Name: "app"}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	lane, err := p.containerLane(ctx, "app")
	if err != nil || lane != (provider.Lane{Key: "compose:media", Policy: provider.LaneReject}) {
		t.Fatalf("lane %+v %v", lane, err)
	}

	var gotDir string
	p.composeTools = func(d string) composeTools { gotDir = d; return toolsMissing(d) }
	err = p.admitUpdate(ctx, spec.ContainerInput{Name: "app"})
	pe := problemOf(t, err)
	if pe.Slug() != problem.ComposePluginMissing || pe.Status != 503 || pe.Extensions["provider"] != spec.Name ||
		!strings.Contains(pe.Detail, "docker-compose") || gotDir != p.Settings().ConfigDir {
		t.Fatalf("plugin missing: %v (config dir %q)", err, gotDir)
	}
	if err := p.admitUpdate(ctx, spec.ContainerInput{Name: "solo"}); err != nil {
		t.Fatalf("standalone without the plugin: %v", err)
	}

	p.composeTools = toolsPresent
	if err := os.Remove(filepath.Join(dir, "override.yaml")); err != nil {
		t.Fatal(err)
	}
	pe = problemOf(t, p.admitUpdate(ctx, spec.ContainerInput{Name: "app"}))
	if pe.Slug() != problem.ComposeFilesMissing || pe.Status != 422 || pe.Extensions["path"] != filepath.Join(dir, "override.yaml") {
		t.Fatalf("file gone: %+v", pe)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("%d registry calls at admission", n)
	}
}

// Prepare writes a compose-mode input.json with the project, service and
// files from the labels and a per-command bound of updateMaxRuntime plus
// composeGrace, and the same helper argv and unit as a standalone update.
func TestPrepareComposeUpdate(t *testing.T) {
	f, dir := composeEngine(t)
	p, ops := updProvider(t, f)
	p.composeTools = toolsPresent
	r := provider.NewRun(testOpID, nil)
	s, err := p.prepareUpdate(context.Background(), spec.ContainerInput{Name: "app"}, r)
	if err != nil {
		t.Fatal(err)
	}
	opDir := filepath.Join(ops, testOpID)
	if want := []string{"/usr/local/bin/nexusd", HelperCommand, "--dir", opDir}; !slices.Equal(s.Argv, want) ||
		s.KillMode != "control-group" || s.MaxRuntime != time.Hour || len(s.Env) != 0 {
		t.Fatalf("spec %+v", s)
	}
	b, err := os.ReadFile(filepath.Join(opDir, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	var hin helperInput
	if err := json.Unmarshal(b, &hin); err != nil {
		t.Fatal(err)
	}
	if hin.Mode != ModeCompose || hin.OldName != "" || hin.Compose == nil || hin.Compose.Project != "media" ||
		hin.Compose.Service != "app" || hin.Compose.WorkingDir != dir || len(hin.Compose.ConfigFiles) != 2 ||
		len(hin.Compose.EnvFiles) != 1 || hin.ComposeTimeout != time.Hour+composeGrace || hin.ContainerID != f.id("app") {
		t.Fatalf("input %s", b)
	}

	// Lost without a result: the detail names the service and directory,
	// not a -nexus-old- container.
	d := p.lostUpdateDetail(r, spec.ContainerInput{Name: "app"})
	if !strings.Contains(d, "service app of Docker Compose project media") || !strings.Contains(d, dir) ||
		strings.Contains(d, oldNameInfix) {
		t.Fatalf("lost detail %q", d)
	}
	_, err = p.finalizeUpdate(context.Background(), spec.ContainerInput{Name: "app"}, detach.Exit{ExitCode: 1}, r)
	if pe := problemOf(t, err); pe.Slug() != problem.OutcomeUnknown || !strings.Contains(pe.Detail, "docker compose ps") {
		t.Fatalf("finalize without a result: %v", err)
	}

	// A health failure from result.json is Failed with the digests and
	// rolledBack false (FR-13).
	if err := writeJSONAtomic(opDir, resultFile, helperResult{PreviousDigest: digestA, NewDigest: digestB,
		ContainerID: "new", Error: &helperError{Slug: problem.UpdateHealthFailed, Detail: "unhealthy"}}); err != nil {
		t.Fatal(err)
	}
	_, err = p.finalizeUpdate(context.Background(), spec.ContainerInput{Name: "app"}, detach.Exit{ExitCode: 0}, r)
	pe := problemOf(t, err)
	if pe.Slug() != problem.UpdateHealthFailed || pe.Extensions[spec.ExtRolledBack] != false ||
		pe.Extensions[spec.ExtPreviousDigest] != digestA || pe.Extensions[spec.ExtNewDigest] != digestB {
		t.Fatalf("health failure: %+v", pe)
	}
}

// Preflight reports whether the Compose plugin is installed, and stays
// enabled without it (CON-6).
func TestPreflightReportsComposePlugin(t *testing.T) {
	f := newUpdateEngine(t)
	p := updateProvider(t, f, t.TempDir(), nil)
	for _, tc := range []struct {
		tools func(string) composeTools
		avail bool
	}{{toolsPresent, true}, {toolsMissing, false}} {
		p.composeTools = tc.tools
		st := p.Preflight(context.Background())
		d, _ := st.Details[spec.DetailComposePlugin].(map[string]any)
		if st.State != apitypes.ProviderEnabled || d == nil || d["available"] != tc.avail {
			t.Fatalf("preflight %+v", st)
		}
		if !tc.avail && !strings.Contains(d["reason"].(string), "docker-compose") {
			t.Fatalf("reason %v", d)
		}
	}
}

// The helper accepts the compose mode.
func TestUpdateHelperComposeMode(t *testing.T) {
	dir := t.TempDir()
	// No Compose section: the helper fails the update without changing
	// anything, and still writes result.json.
	if err := writeJSONAtomic(dir, inputFile, helperInput{Mode: ModeCompose, Socket: "/nonexistent.sock"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := UpdateHelper([]string{"--dir", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	res, ok, err := readResult(dir)
	if err != nil || !ok || res.Error == nil || res.Error.Slug != problem.InternalError || res.Changed {
		t.Fatalf("result %+v %v %v", res, ok, err)
	}
}

// CR-91: the Compose helper asks the registry about the running digest, as
// the standalone helper does (CR-2). A local build whose tag the registry
// also serves is refused before docker compose pull runs, and nothing
// changes; one the registry serves goes on to the pull.
func TestComposeUpdateRefusesALocalBuildBehindAServedTag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		known    map[string]bool
		slug     problem.Slug
		detail   string
		wantPull bool
	}{
		{"local build", map[string]bool{}, problem.NotUpdatable, "built or retagged on this host", false},
		{"served", map[string]bool{digestA: true}, problem.DependencyUnavailable, "the fake pull failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunEngine(t, true)
			f.known = tc.known
			_, labels := composeProjectDir(t, "media")
			f.ctrs[runOldID].labels = labels

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fake := filepath.Join(dir, "docker")
			writeExec(t, fake, `#!/bin/sh
echo "$*" >>`+calls+`
case "$*" in
*" pull "*) echo "the fake pull failed" >&2; exit 3 ;;
esac
`, 0o755)
			old := helperComposeTools
			helperComposeTools = func(string) composeTools { return composeTools{Docker: fake, Plugin: fake} }
			t.Cleanup(func() { helperComposeTools = old })

			in := f.input()
			in.Mode, in.ComposeTimeout = ModeCompose, time.Minute
			in.Compose = &composeTarget{Project: "media", Service: "app"}
			var out bytes.Buffer
			res := runCompose(context.Background(), in, &out)
			if res.Error == nil || res.Error.Slug != tc.slug || res.Changed ||
				!strings.Contains(res.Error.Detail, tc.detail) || !strings.Contains(res.Error.Detail, "nothing was changed") {
				t.Fatalf("result %+v %+v\n%s", res, res.Error, out.String())
			}
			b, _ := os.ReadFile(calls)
			if pulled := strings.Contains(string(b), " pull "); pulled != tc.wantPull {
				t.Fatalf("docker compose pull ran: %v, want %v; calls:\n%s", pulled, tc.wantPull, b)
			}
			if got := f.mutations(); len(got) != 0 {
				t.Errorf("the helper changed the Engine: %v", got)
			}
			f.wantState(t, map[string]bool{"app": true})
		})
	}
}
