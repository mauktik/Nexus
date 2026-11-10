package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
	"github.com/mauktik/nexus/internal/providers/scripts"
)

// Compose-managed container update (WI 6.5, FR-12, CON-6, A-34). A
// container with the com.docker.compose.project and .service labels is
// updated through the Docker Compose plugin, never recreated by Nexus from
// its inspect: Compose owns the service's definition. Admission requires
// the project's working directory and files that the labels record
// (compose-files-missing, 422) and the plugin (compose-plugin-missing,
// 503, for these updates only). The helper, in the same detached unit as a
// standalone update, first asks the registry whether it serves the running
// image, as a standalone update does (CR-2), and then runs
//
//	docker compose --project-directory D -f F... [--env-file E...] -p P pull S
//
// and, only when the pulled image differs from the container's,
//
//	docker compose --project-directory D -f F... [--env-file E...] -p P up -d --no-deps S
//
// each as one exec of an argument vector. `pull SERVICE...` pulls the
// named services' images; `up` "(re)creates" a service's containers when
// "the service's configuration or image was changed after the container's
// creation", and --no-deps does not start linked services
// (docs.docker.com/reference/cli/docker/compose/pull/ and .../up/). It then
// waits for the new container's health as a standalone update does. A
// Compose update is never rolled back (FR-13).

// ModeCompose is the helper's mode for a Compose-managed container.
const ModeCompose = "compose"

// composePluginName is the plugin's file name in a plugin directory.
const composePluginName = "docker-compose"

// composePluginDirs are where the Docker CLI looks for plugins after
// <config dir>/cli-plugins, in its order (docker/cli
// cli-plugins/manager/manager_unix.go, defaultSystemPluginDirs).
var composePluginDirs = []string{
	"/usr/local/lib/docker/cli-plugins",
	"/usr/local/libexec/docker/cli-plugins",
	"/usr/lib/docker/cli-plugins",
	"/usr/libexec/docker/cli-plugins",
}

// composePath is the PATH of the Compose commands: the docker CLI is found
// in it, and so is any credential helper the CLI runs. The unit's
// environment is otherwise empty.
const composePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// composeGrace is how long past the maximum runtime the helper lets a
// Compose command run before it kills it. By then the operation has ended
// outcome-unknown naming the unit, which nothing signals (A-63), so the
// command is not cut short while the operation is still live.
const composeGrace = time.Minute

// composeOutputTail is how much of a failed Compose command's stderr its
// error quotes.
const composeOutputTail = 2048

// Compose's name rules: a project name is lowercase letters, digits, "_"
// and "-", starting with a letter or digit (compose-go
// loader.NormalizeProjectName); a service name never starts with "-" here,
// so neither can be read as a flag.
var (
	composeProjectRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	composeServiceRE = regexp.MustCompile(`^[a-zA-Z0-9._][a-zA-Z0-9._-]*$`)
)

// composeTools are the docker CLI and Compose plugin a Compose update
// runs, or why they are missing.
type composeTools struct {
	Docker  string
	Plugin  string
	Missing string // Empty when both are present
}

// findCompose looks for the docker CLI in composePath and the plugin where
// the CLI would, configDir's cli-plugins first.
func findCompose(configDir string) composeTools {
	return findComposeIn(configDir, filepath.SplitList(composePath), composePluginDirs)
}

func findComposeIn(configDir string, binDirs, pluginDirs []string) composeTools {
	var t composeTools
	for _, d := range binDirs {
		if p := filepath.Join(d, "docker"); isExecutable(p) {
			t.Docker = p
			break
		}
	}
	if t.Docker == "" {
		t.Missing = "the docker CLI, which runs the Docker Compose plugin, is not installed in " + strings.Join(binDirs, ":")
		return t
	}
	dirs := pluginDirs
	if configDir != "" {
		dirs = append([]string{filepath.Join(configDir, "cli-plugins")}, pluginDirs...)
	}
	for _, d := range dirs {
		if p := filepath.Join(d, composePluginName); isExecutable(p) {
			t.Plugin = p
			return t
		}
	}
	t.Missing = fmt.Sprintf("the Docker Compose plugin (%s) is not installed in any of %s", composePluginName,
		strings.Join(dirs, ", "))
	return t
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// composeDetail is the capabilities detail for tools.
func composeDetail(t composeTools) map[string]any {
	if t.Missing != "" {
		return map[string]any{"available": false, "reason": t.Missing}
	}
	return map[string]any{"available": true, "path": t.Plugin}
}

// pluginMissing is compose-plugin-missing for a Compose-managed container.
func pluginMissing(t composeTools) error {
	return problem.New(problem.ComposePluginMissing, "the container is managed by Docker Compose, which Nexus "+
		"updates through the Compose plugin, and "+t.Missing+". Install it (the docker-compose-plugin or "+
		"docker-compose-v2 package); every other docker function is unaffected").With("provider", spec.Name)
}

// composeTarget is what the helper needs of a Compose-managed container:
// its project, service and replica, and the project's files, all from its
// labels.
type composeTarget struct {
	Project     string   `json:"project"`
	Service     string   `json:"service"`
	Number      string   `json:"number,omitempty"` // com.docker.compose.container-number
	WorkingDir  string   `json:"workingDir"`
	ConfigFiles []string `json:"configFiles"`
	EnvFiles    []string `json:"envFiles,omitempty"`
}

// filesMissing is compose-files-missing naming path ("" when a label is
// absent).
func filesMissing(path, detail string) error {
	p := problem.New(problem.ComposeFilesMissing, detail+"; nothing was changed. Nexus updates a Compose service "+
		"with the project directory and files its containers record; restore them, or update the project with "+
		"docker compose by hand")
	if path != "" {
		p.With("path", path)
	}
	return p
}

// composeTargetOf reads c's Compose labels and checks that the project's
// working directory, configuration files and environment files exist. It
// changes nothing and contacts no registry. Labels Compose would not write
// are refused as not-updatable.
func composeTargetOf(c container.InspectResponse) (composeTarget, error) {
	var l map[string]string
	if c.Config != nil {
		l = c.Config.Labels
	}
	t := composeTarget{Project: l[spec.ComposeProjectLabel], Service: l[spec.ComposeServiceLabel],
		Number: l[spec.ComposeNumberLabel]}
	if !composeProjectRE.MatchString(t.Project) || !composeServiceRE.MatchString(t.Service) {
		return t, refuseUpdate(spec.ReasonComposeManaged, fmt.Sprintf("its Compose labels (project %q, service %q) "+
			"are not ones Docker Compose writes", t.Project, t.Service))
	}
	if strings.EqualFold(l[spec.ComposeOneoffLabel], "true") {
		return t, refuseUpdate(spec.ReasonComposeManaged, "it is a one-off container of docker compose run, "+
			"which docker compose up does not manage")
	}
	t.WorkingDir = l[spec.ComposeWorkingDirLabel]
	switch {
	case t.WorkingDir == "":
		return t, filesMissing("", fmt.Sprintf("the container's labels do not record Compose project %q's "+
			"working directory (%s)", t.Project, spec.ComposeWorkingDirLabel))
	case !filepath.IsAbs(t.WorkingDir):
		return t, filesMissing(t.WorkingDir, fmt.Sprintf("Compose project %q's working directory %q is not an "+
			"absolute path", t.Project, t.WorkingDir))
	}
	if st, err := os.Stat(t.WorkingDir); err != nil || !st.IsDir() {
		return t, filesMissing(t.WorkingDir, fmt.Sprintf("Compose project %q's working directory %s no longer "+
			"exists", t.Project, t.WorkingDir))
	}
	files := l[spec.ComposeConfigFilesLabel]
	if files == "" {
		return t, filesMissing("", fmt.Sprintf("the container's labels do not record Compose project %q's "+
			"configuration files (%s)", t.Project, spec.ComposeConfigFilesLabel))
	}
	var err error
	if t.ConfigFiles, err = existingFiles(t.Project, "configuration file", files); err != nil {
		return t, err
	}
	if env := l[spec.ComposeEnvFileLabel]; env != "" {
		if t.EnvFiles, err = existingFiles(t.Project, "environment file", env); err != nil {
			return t, err
		}
	}
	if err := checkComposeTrust(t); err != nil {
		return t, err
	}
	return t, nil
}

// composeTrust judges one path of a Compose project: a script-integrity
// walk with root as the user, so that only root can write the path or any
// directory above it. It is a variable so that T0 tests can stage projects
// without root.
var composeTrust = scripts.CheckRootOwned

// checkComposeTrust refuses a Compose project that someone other than root
// can change (SEC-1, CWE-829). The helper runs docker compose as root, and
// Compose reads the project's files when it runs, so a file or directory an
// unprivileged user can write decides what root creates: privileged mode, a
// bind mount of /, and so on. It checks the working directory and its
// ancestors, every -f and --env-file file, and the .env file Compose reads
// from the working directory. Files that include: or extends: name are not
// known from the labels, so they are not checked; they sit under the
// working directory, whose owner and mode are.
func checkComposeTrust(t composeTarget) error {
	type item struct {
		kind, path string
		dir        bool
	}
	items := []item{{"working directory", t.WorkingDir, true}}
	for _, f := range t.ConfigFiles {
		items = append(items, item{"configuration file", f, false})
	}
	for _, f := range t.EnvFiles {
		items = append(items, item{"environment file", f, false})
	}
	if dotenv := filepath.Join(t.WorkingDir, ".env"); !slices.Contains(t.EnvFiles, dotenv) {
		if _, err := os.Lstat(dotenv); err == nil {
			items = append(items, item{"environment file", dotenv, false})
		}
	}
	for _, it := range items {
		err := composeTrust(it.path, it.dir)
		if err == nil {
			continue
		}
		detail, path := err.Error(), it.path
		var pe *problem.Error
		if errors.As(err, &pe) {
			detail = pe.Detail
			if v, ok := pe.Extensions["path"].(string); ok {
				path = v
			}
		}
		return problem.New(problem.ComposeFilesMissing, fmt.Sprintf("Compose project %q's %s %s is not trusted: %s. "+
			"Nexus runs docker compose as root on the project's files, so a path that anyone but root can write "+
			"would decide what root creates. Fix the ownership and mode as stated, or update the project with "+
			"docker compose by hand; nothing was changed", t.Project, it.kind, it.path, detail)).With("path", path)
	}
	return nil
}

// existingFiles splits a label's comma-joined absolute paths (as Compose
// joins them) and requires each to be a file.
func existingFiles(project, kind, label string) ([]string, error) {
	var out []string
	for _, f := range strings.Split(label, ",") {
		if !filepath.IsAbs(f) {
			return nil, filesMissing(f, fmt.Sprintf("Compose project %q's %s %q is not an absolute path", project, kind, f))
		}
		if st, err := os.Stat(f); err != nil || !st.Mode().IsRegular() {
			return nil, filesMissing(f, fmt.Sprintf("Compose project %q's %s %s no longer exists", project, kind, f))
		}
		out = append(out, f)
	}
	return out, nil
}

// composeAdmissible refuses a Compose update this host cannot run now: no
// plugin (503), or the project's files gone (422).
func (p *Provider) composeAdmissible(c container.InspectResponse) (composeTarget, error) {
	if t := p.composeTools(p.Settings().ConfigDir); t.Missing != "" {
		return composeTarget{}, pluginMissing(t)
	}
	return composeTargetOf(c)
}

// composeArgv is the argument vector of one Compose command on t's
// service: every path and name is its own element, never a shell string
// (FR-25).
func composeArgv(docker string, t composeTarget, verb ...string) []string {
	argv := []string{docker, "compose", "--project-directory", t.WorkingDir}
	for _, f := range t.ConfigFiles {
		argv = append(argv, "-f", f)
	}
	for _, f := range t.EnvFiles {
		argv = append(argv, "--env-file", f)
	}
	argv = append(argv, "-p", t.Project)
	argv = append(argv, verb...)
	return append(argv, t.Service)
}

// composeEnv is the Compose commands' whole environment: DOCKER_CONFIG is
// docker.configDir, so Compose resolves registry credentials itself,
// credential helpers included; DOCKER_HOST is docker.socket.
func composeEnv(configDir, socket string) []string {
	home := "/"
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	env := []string{"PATH=" + composePath, "HOME=" + home, "DOCKER_HOST=unix://" + socket}
	if configDir != "" {
		env = append(env, "DOCKER_CONFIG="+configDir)
	}
	return env
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (w *tailBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, p...)
	if len(w.b) > w.max {
		w.b = append([]byte(nil), w.b[len(w.b)-w.max:]...)
	}
	return len(p), nil
}

func (w *tailBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(bytes.TrimSpace(w.b))
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// compose runs one Compose command with its output in the operation's
// output, bounded by the maximum runtime plus composeGrace. Its process
// group is killed on the timeout.
func (u *updater) compose(ctx context.Context, docker string, t composeTarget, verb ...string) error {
	timeout := u.in.ComposeTimeout
	if timeout <= 0 {
		timeout = time.Hour + composeGrace
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := composeArgv(docker, t, verb...)
	u.say("running docker compose %s %s: %q", strings.Join(verb, " "), t.Service, argv)
	tail := &tailBuffer{max: composeOutputTail}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = composeEnv(u.in.ConfigDir, u.in.Socket), t.WorkingDir
	// One lock for both streams: exec copies each in its own goroutine
	// unless the writer is an *os.File.
	out := &lockedWriter{w: u.out}
	cmd.Stdout, cmd.Stderr = out, io.MultiWriter(out, tail)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("docker compose %s did not finish within %s", strings.Join(verb, " "), timeout)
	case err != nil && tail.String() != "":
		return fmt.Errorf("docker compose %s failed (%v): %s", strings.Join(verb, " "), err, registryText(tail.String()))
	case err != nil:
		return fmt.Errorf("docker compose %s failed: %v", strings.Join(verb, " "), err)
	}
	return nil
}

// helperComposeTools finds the docker CLI and Compose plugin the helper
// runs. It is a variable so that T0 tests can run the helper's flow with a
// fake docker CLI.
var helperComposeTools = findCompose

// runCompose updates the Compose-managed container in, writing progress
// to out, and returns the result to store.
func runCompose(ctx context.Context, in helperInput, out io.Writer) helperResult {
	return runUpdater(ctx, in, out, (*updater).runCompose)
}

func (u *updater) runCompose(ctx context.Context) helperResult {
	if u.in.Compose == nil {
		return u.fail(problem.InternalError, "input.json has no Compose project; nothing was changed")
	}
	want := *u.in.Compose
	name := u.in.Name
	c, err := u.inspect(ctx, u.in.ContainerID)
	switch {
	case cerrdefs.IsNotFound(err):
		return u.fail(problem.NotFound, fmt.Sprintf("container %s no longer exists; nothing was changed", name))
	case err != nil:
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the container: "+u.engine(err))
	}
	t, err := composeTargetOf(c)
	if err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) {
			return u.fail(pe.Slug(), pe.Detail)
		}
		return u.fail(problem.InternalError, err.Error())
	}
	if t.Project != want.Project || t.Service != want.Service {
		return u.fail(problem.Conflict, fmt.Sprintf("container %s now belongs to Compose project %q service %q, "+
			"not %q %q as when the update was requested; nothing was changed", name, t.Project, t.Service,
			want.Project, want.Service))
	}
	tools := helperComposeTools(u.in.ConfigDir)
	if tools.Missing != "" {
		return u.fail(problem.ComposePluginMissing, tools.Missing+"; nothing was changed")
	}
	ref := ""
	if c.Config != nil {
		ref = c.Config.Image
	}
	if chk, done := classifyLocal(c, ref); done {
		return u.fail(problem.NotUpdatable, chk.Detail+"; nothing was changed")
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return u.fail(problem.NotUpdatable, fmt.Sprintf("the image reference %q names no registry repository; nothing was changed", ref))
	}
	callCtx, cancel := u.call(ctx)
	oldImg, err := u.cli.ImageInspect(callCtx, c.Image)
	cancel()
	if err != nil {
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the container's image: "+
			u.engine(err)+"; nothing was changed")
	}
	current := repoDigestsFor(named, oldImg.RepoDigests)
	if len(current) == 0 {
		return u.fail(problem.NotUpdatable, fmt.Sprintf("the image has no registry digest for %s: it was built on "+
			"this host. %s; nothing was changed", named.Name(), byScript))
	}
	u.res.PreviousDigest = current[0]

	// The registry must serve the running image, as for a standalone update
	// (CR-2): docker compose pull would otherwise replace a local build whose
	// tag the registry also serves.
	reg, err := u.registryAccess(named)
	if err != nil {
		return u.fail(problem.DependencyUnavailable, err.Error()+"; nothing was changed")
	}
	u.say("checking that the registry %s serves the running image of %s", reg.domain, named.Name())
	if f := u.unserved(ctx, named, current, reg); f != nil {
		if f.Slug == problem.DependencyUnavailable {
			// Compose resolves credential helpers when it pulls, but this
			// check uses only auths (A-61).
			f.Detail += fmt.Sprintf(". Before a Compose pull, Nexus asks the registry with the credential in auths "+
				"of %s (providers.docker.configDir) and never runs a credential helper", reg.path)
		}
		return u.fail(f.Slug, f.Detail+"; nothing was changed")
	}

	if detail := u.composeChanged(ctx, tools.Docker, t, c); detail != "" {
		return u.fail(problem.NotUpdatable, detail+"; nothing was changed")
	}

	u.say("pulling the image of service %s of Compose project %s (%s)", t.Service, t.Project, ref)
	if err := u.compose(ctx, tools.Docker, t, "pull"); err != nil {
		return u.fail(problem.DependencyUnavailable, err.Error()+"; nothing was changed")
	}
	callCtx, cancel = u.call(ctx)
	newImg, err := u.cli.ImageInspect(callCtx, ref)
	cancel()
	if err != nil {
		return u.fail(problem.DependencyUnavailable, "the Docker Engine could not describe the pulled image: "+
			u.engine(err)+"; nothing was changed")
	}
	u.res.NewDigest = u.res.PreviousDigest
	if d := repoDigestsFor(named, newImg.RepoDigests); len(d) > 0 && !slices.Contains(d, u.res.PreviousDigest) {
		u.res.NewDigest = d[0]
	}
	if newImg.ID == c.Image {
		u.res.ContainerID = c.ID
		u.say("container %s already runs the newest image %s (%s); nothing to do", name, ref, u.res.PreviousDigest)
		return u.res
	}

	// The project's files are read again by up, after the pull has taken
	// time: they must still be ones only root can write.
	if err := checkComposeTrust(t); err != nil {
		var pe *problem.Error
		if errors.As(err, &pe) {
			return u.fail(pe.Slug(), pe.Detail)
		}
		return u.fail(problem.InternalError, err.Error())
	}
	u.say("recreating service %s with the pulled image (%s)", t.Service, u.res.NewDigest)
	if !running(c) {
		u.say("container %s was not running; docker compose up starts the service's new container", name)
	}
	if err := u.compose(ctx, tools.Docker, t, "up", "-d", "--no-deps"); err != nil {
		return u.fail(problem.DependencyUnavailable, fmt.Sprintf("%s; service %s may be partly updated and nothing "+
			"was rolled back: check it with docker compose ps in %s", err, t.Service, t.WorkingDir))
	}
	nc, err := u.composeContainer(ctx, t, c.ID)
	if err != nil {
		return u.fail(problem.DependencyUnavailable, err.Error()+"; nothing was rolled back")
	}
	u.res.ContainerID = nc.ID
	if nc.Image != newImg.ID {
		return u.fail(problem.DependencyUnavailable, fmt.Sprintf("after docker compose up, container %s (%s) runs "+
			"image %s, not the pulled %s; nothing was rolled back", displayName(nc), short(nc.ID), nc.Image, newImg.ID))
	}
	u.say("Compose created container %s (%s); waiting up to %s for it to be running, and healthy if it has a "+
		"health check", displayName(nc), short(nc.ID), u.in.HealthTimeout)
	if err := waitReady(ctx, u.inspect, nc.ID, u.in.HealthTimeout); err != nil {
		return u.fail(problem.UpdateHealthFailed, err.Error()+"; a Compose update is not rolled back: the new "+
			"container is left in place")
	}
	u.res.Changed = true
	u.say("updated service %s of Compose project %s to %s", t.Service, t.Project, u.res.NewDigest)
	return u.res
}

// composeHashLabel is the hash of the service definition that Compose
// recorded on the container when it created it.
const composeHashLabel = "com.docker.compose.config-hash"

// composeChanged returns why the project's definition of t's service is no
// longer the one c was created from, or "" when it is the same or cannot be
// compared (CR-29). docker compose up recreates the service from the files
// as they are now, in the helper's minimal environment, so an edit made
// since, or a ${VAR} the original deploy shell supplied, would be deployed
// along with the new image. `docker compose config --hash SERVICE` prints
// the service's current hash, the value Compose stores in the label
// (docs.docker.com/reference/cli/docker/compose/config/, --hash). Output
// that cannot be read, or a container without the label, is reported and
// let through: the check refuses only on a hash it has read.
func (u *updater) composeChanged(ctx context.Context, docker string, t composeTarget, c container.InspectResponse) string {
	var recorded string
	if c.Config != nil {
		recorded = c.Config.Labels[composeHashLabel]
	}
	if recorded == "" {
		u.say("container %s has no %s label; its Compose definition cannot be compared", u.in.Name, composeHashLabel)
		return ""
	}
	// The service is --hash's value: "config --hash SERVICE".
	out, err := u.composeOutput(ctx, docker, t, "config", "--hash")
	if err != nil {
		return err.Error()
	}
	current := ""
	lines := strings.Fields(out)
	switch {
	case len(lines) == 1:
		current = lines[0]
	case len(lines) == 2 && lines[0] == t.Service:
		current = lines[1]
	}
	if current == "" {
		u.say("docker compose config --hash did not print a hash for service %s; its definition cannot be compared", t.Service)
		return ""
	}
	if current != recorded {
		return fmt.Sprintf("the Compose definition of service %s changed since container %s was created (its hash "+
			"was %s, and is now %s), and an update would deploy the current files, evaluated without the "+
			"environment of the original deploy, along with the new image. Apply the change with docker compose "+
			"up -d in %s first", t.Service, u.in.Name, short(recorded), short(current), t.WorkingDir)
	}
	return ""
}

// composeOutput runs a Compose command that only reads, and returns its
// standard output. Its failure is reported with the tail of its stderr.
func (u *updater) composeOutput(ctx context.Context, docker string, t composeTarget, verb ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	argv := composeArgv(docker, t, verb...)
	var stdout bytes.Buffer
	tail := &tailBuffer{max: composeOutputTail}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = composeEnv(u.in.ConfigDir, u.in.Socket), t.WorkingDir
	cmd.Stdout, cmd.Stderr = &stdout, tail
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker compose %s failed (%v): %s", strings.Join(verb, " "), err, registryText(tail.String()))
	}
	return stdout.String(), nil
}

// composeContainer is t's container after docker compose up: the
// service's container with t's replica number, preferring one other than
// old.
func (u *updater) composeContainer(ctx context.Context, t composeTarget, old string) (container.InspectResponse, error) {
	f := client.Filters{}.Add("label", spec.ComposeProjectLabel+"="+t.Project, spec.ComposeServiceLabel+"="+t.Service)
	callCtx, cancel := u.call(ctx)
	list, err := u.cli.ContainerList(callCtx, client.ContainerListOptions{All: true, Filters: f})
	cancel()
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("the Docker Engine could not list service %s's containers: %s",
			t.Service, u.engine(err))
	}
	var ids []string
	for _, s := range list.Items {
		if strings.EqualFold(s.Labels[spec.ComposeOneoffLabel], "true") ||
			(t.Number != "" && s.Labels[spec.ComposeNumberLabel] != t.Number) {
			continue
		}
		ids = append(ids, s.ID)
	}
	slices.SortStableFunc(ids, func(a, b string) int {
		// The new container first.
		switch {
		case a == old && b != old:
			return 1
		case b == old && a != old:
			return -1
		}
		return 0
	})
	if len(ids) == 0 {
		return container.InspectResponse{}, fmt.Errorf("after docker compose up, service %s of Compose project %s "+
			"has no container", t.Service, t.Project)
	}
	nc, err := u.inspect(ctx, ids[0])
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("the Docker Engine could not describe service %s's new "+
			"container: %s", t.Service, u.engine(err))
	}
	return nc, nil
}

// composeByHand is the outcome-unknown detail of a Compose update that
// stopped without a result.
func composeByHand(t composeTarget) string {
	return fmt.Sprintf("service %s of Docker Compose project %s may be partly updated: check it with docker compose "+
		"ps in %s, and run docker compose up -d %s there to finish. Nothing was rolled back automatically",
		t.Service, t.Project, t.WorkingDir, t.Service)
}
