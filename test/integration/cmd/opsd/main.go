// Command opsd is the T1 survival harness daemon (WI 3.5): the production
// daemon wiring (daemon.New, so the production ops.Manager and detach
// launcher with nil seams) with one test provider whose detached operation
// is a controllable shell child. It runs as a real systemd unit inside the
// harness container, and the op units it launches run `opsd op-supervise`,
// which is detach.Supervise exactly as `nexusd op-supervise` is.
//
// It is a test-only binary: it is built to build/harness/ for the harness
// and never shipped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// MarkerRoot is where a job's child writes its pid and signal markers, one
// directory per job name. The child runs in its op unit, outside the
// daemon's PrivateTmp, so the harness test reads the real directory.
const MarkerRoot = "/run/nexus-harness"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: opsd serve|op-supervise ...")
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "op-supervise":
		fs := flag.NewFlagSet("opsd op-supervise", flag.ContinueOnError)
		fs.SetOutput(stderr)
		dir := fs.String("dir", "", "the operation's directory")
		if err := fs.Parse(args[1:]); err != nil || *dir == "" || fs.NArg() > 0 {
			fmt.Fprintln(stderr, "usage: opsd op-supervise --dir dir")
			return 2
		}
		return detach.Supervise(*dir, stderr)
	}
	fmt.Fprintf(stderr, "opsd: unknown command %q\n", args[0])
	return 2
}

func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("opsd serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file")
	stateDir := fs.String("state-dir", auth.DefaultStateDir, "state directory")
	auditDir := fs.String("audit-dir", "", "audit log directory")
	setsidChild := fs.String("setsid-child", "",
		"start a setsid child in this unit's own cgroup and write its pid here (the TC-77 control)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "opsd serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	if *setsidChild != "" {
		if err := startSetsidChild(*setsidChild); err != nil {
			fmt.Fprintf(stderr, "opsd: %v\n", err)
			return 1
		}
	}
	d, err := daemon.New(cfg, daemon.Options{
		Providers:  []provider.Provider{harnessProvider{}},
		ConfigPath: *path,
		StateDir:   *stateDir,
		AuditDir:   *auditDir,
		Logger:     log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "opsd: %v\n", err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "opsd: %v\n", err)
		return 1
	}
	return 0
}

// startSetsidChild starts a long sleep in a new session, which stays in
// this process's cgroup: unlike an op unit, it must die with the unit.
func startSetsidChild(pidFile string) error {
	cmd := exec.Command("/usr/bin/sleep", "3600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the setsid child: %w", err)
	}
	go func() {
		// Reaped only so it never lingers as a zombie of a live opsd.
		if err := cmd.Wait(); err != nil {
			return
		}
	}()
	return os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
}

// jobIn is the harness job's input. Name is also the marker directory.
type jobIn struct {
	Name     string `json:"name"`
	Behavior string `json:"behavior,omitempty"` // gate (default), trap or quick
	ExitCode int    `json:"exitCode,omitempty"`
	KillMode string `json:"killMode,omitempty"` // control-group (default) or process
	Lane     string `json:"lane,omitempty"`
	Policy   string `json:"policy,omitempty"`
}

type jobOut struct {
	apitypes.ActionResult
	ExitCode int `json:"exitCode"`
}

var harnessJob = def.Define[jobIn, jobOut](def.Op{
	ID: "scripts.harnessjobs.run", Provider: "scripts", Kind: def.KindResourceAction, ResourceType: "harnessjobs",
	Action: "run", Summary: "Run a harness job",
	Description: "Detached shell job for the T1 survival harness. Side effects: runs a shell child in a " +
		"transient unit; long-running.",
	Scope: def.Scope{Name: "scripts:run"}, Risk: def.RiskMutating, Mode: def.ModeLongRunning,
	Cancel: def.CancelRunning,
	Refine: func(s *jsonschema.Schema) {
		if p := s.Properties["name"]; p != nil {
			p.Pattern = `^[a-z0-9][a-z0-9-]*$`
		}
		if p := s.Properties["behavior"]; p != nil {
			p.Enum = []any{"", "gate", "trap", "quick"}
		}
		if p := s.Properties["killMode"]; p != nil {
			p.Enum = []any{"", string(detach.KillControlGroup), string(detach.KillProcess)}
		}
		if p := s.Properties["lane"]; p != nil {
			p.Pattern = `^[a-z0-9:-]*$`
		}
		if p := s.Properties["policy"]; p != nil {
			p.Enum = []any{"", string(provider.LaneQueue), string(provider.LaneReject)}
		}
		if p := s.Properties["exitCode"]; p != nil {
			zero, max := 0.0, 125.0
			p.Minimum, p.Maximum = &zero, &max
		}
	},
	CLI: def.CLIBinding{Noun: "harnessjob", Verb: "start", Positional: "name"},
})

// harnessProvider registers as "scripts", the config section that enables
// it; nexusd has no scripts provider at 3.5.
type harnessProvider struct{}

func (harnessProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "scripts", ReadScope: "scripts:read", Ops: []def.Op{harnessJob.Op()}}
}

func (harnessProvider) Configure(decode func(any) error) error {
	var c struct {
		Enabled bool `yaml:"enabled"`
	}
	return decode(&c)
}

func (harnessProvider) Preflight(context.Context) provider.Status {
	if a := detach.SystemdAvailable(); !a.Available {
		return provider.Status{State: apitypes.ProviderUnavailable, Reason: a.Reason}
	}
	return provider.Status{State: apitypes.ProviderEnabled}
}

func (harnessProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Detached(harnessJob, provider.DetachedSpec[jobIn, jobOut]{
		Lane: func(_ context.Context, in jobIn) (provider.Lane, error) {
			if in.Lane == "" {
				return provider.NoLane, nil
			}
			return provider.Lane{Key: in.Lane, Policy: provider.LanePolicy(in.Policy)}, nil
		},
		Prepare: func(_ context.Context, in jobIn, _ *provider.Run) (detach.Spec, error) {
			km := detach.KillMode(in.KillMode)
			if km == "" {
				km = detach.KillControlGroup
			}
			return detach.Spec{
				Argv:       []string{"/bin/sh", "-c", script(in), "job", filepath.Join(MarkerRoot, in.Name)},
				Env:        []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
				Grace:      5 * time.Second,
				KillMode:   km,
				MaxRuntime: time.Hour,
			}, nil
		},
		Finalize: func(_ context.Context, _ jobIn, x detach.Exit, _ *provider.Run) (jobOut, error) {
			if x.Error != "" {
				return jobOut{}, errors.New(x.Error)
			}
			if x.Signal != "" {
				return jobOut{}, problem.New(problem.ScriptFailed, "the child was killed by "+x.Signal)
			}
			if x.ExitCode != 0 {
				return jobOut{}, problem.New(problem.ScriptFailed, fmt.Sprintf("the child exited with code %d", x.ExitCode))
			}
			return jobOut{ActionResult: apitypes.ActionResult{Changed: true}}, nil
		},
		LostDetail: func(_ *provider.Run, in jobIn) string { return "harness job " + in.Name },
	})}
}

// script is the child's shell program; $1 is the marker directory.
//
//   - gate writes its pid, prints a line on each stream, waits until the
//     test creates $1/release, prints another line and exits ExitCode.
//   - trap starts a grandchild in the same process group and a bystander in
//     a session of its own; the child and grandchild record a SIGTERM in
//     <name>.term, the bystander too, so a signal that reached more than
//     the main process's group shows.
//   - quick prints a line and exits ExitCode.
func script(in jobIn) string {
	code := strconv.Itoa(in.ExitCode)
	switch in.Behavior {
	case "quick":
		return `echo "quick job"; exit ` + code
	case "trap":
		return `D="$1"; mkdir -p "$D"
setsid sh -c 'trap "echo TERM > $0/bystander.term; exit 143" TERM; echo $$ > $0/bystander.pid; while :; do sleep 0.2; done' "$D" &
sh -c 'trap "echo TERM > $0/grandchild.term; exit 143" TERM; echo $$ > $0/grandchild.pid; while :; do sleep 0.2; done' "$D" &
trap 'echo TERM > "$D/child.term"; exit 143' TERM
echo $$ > "$D/child.pid"
echo "trap job ready"
while :; do sleep 0.2; done`
	}
	return `D="$1"; mkdir -p "$D"
echo $$ > "$D/child.pid"
echo "gate: before"
echo "gate: stderr before" >&2
while [ ! -e "$D/release" ]; do sleep 0.2; done
echo "gate: after"
exit ` + code
}
