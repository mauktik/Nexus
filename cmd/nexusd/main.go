// Command nexusd is the Nexus server daemon.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/daemon"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/providers/docker"
	"github.com/mauktik/nexus/internal/providers/nexusd"
	"github.com/mauktik/nexus/internal/providers/nginx"
	"github.com/mauktik/nexus/internal/providers/pihole"
	"github.com/mauktik/nexus/internal/providers/services"
	storageprov "github.com/mauktik/nexus/internal/providers/storage"
	"github.com/mauktik/nexus/internal/tlsutil"
	"github.com/mauktik/nexus/internal/version"
)

const name = "nexusd"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "validate-config":
			return validateConfig(args[1:], stdout, stderr)
		case "serve":
			return serve(args[1:], stdout, stderr)
		case "token":
			return auth.TokenCommand(name, args[1:], stdout, stderr)
		case "tls":
			return tlsCommand(args[1:], stdout, stderr)
		case "op-supervise":
			// Hidden: the main process of an operation's unit (FR-6).
			return opSupervise(args[1:], stderr)
		case docker.HelperCommand:
			// Hidden: a container update, run by op-supervise in the
			// operation's unit (D-8). Its progress goes to stdout, which
			// the supervisor frames into the operation's output.
			return docker.UpdateHelper(args[1:], stdout, stderr)
		case nginx.HelperCommand:
			// Hidden: one nginx operation, run by op-supervise outside
			// nexusd's sandbox (CON-2). Its progress and nginx's output go
			// to stdout, which the supervisor frames into the operation's
			// output.
			return nginx.Helper(args[1:], stdout, stderr)
		case nexusd.HelperCommand:
			// Hidden: a restart, update, rollback or config set of nexusd
			// itself, run by op-supervise in the operation's unit, outside
			// nexusd.service. A config set re-validates with the daemon's
			// own check (D-6, ASM-10).
			return nexusd.Helper(args[1:], stdout, stderr, checkConfigData)
		case pihole.HelperCommand:
			// Hidden: a pihole mutation, run by op-supervise as root in the
			// operation's unit. Pi-hole command output goes to stdout.
			return pihole.Helper(args[1:], stdout, stderr)
		case services.HelperCommand:
			// Hidden: a service start, stop or restart, or the guarded
			// reboot, run by op-supervise outside nexusd's sandbox (D-5,
			// CON-2). Its progress goes to stdout, which the supervisor
			// frames into the operation's output.
			return services.Helper(args[1:], stdout, stderr)
		case storageprov.HelperCommand:
			// Hidden: a scrub start, scrub stop or snapshot, run by
			// op-supervise in the operation's unit outside nexusd's sandbox,
			// as the ZFS ioctls need CAP_SYS_ADMIN (D-4). Its progress goes
			// to stdout, which the supervisor frames into the operation's
			// output.
			return storageprov.Helper(args[1:], stdout, stderr)
		}
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage:\n  %[1]s -version\n  %[1]s serve [--config path] [--state-dir dir]\n  %[1]s validate-config [--config path]\n  %[1]s token create|list|revoke ...\n  %[1]s tls fingerprint [--config path] [--state-dir dir]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		if _, err := fmt.Fprintf(stdout, "%s %s\n", name, version.Version); err != nil {
			return 1
		}
		return 0
	}
	fs.Usage()
	return 2
}

// checkConfigData is daemon.CheckConfigData in the shape the service
// helper takes.
func checkConfigData(data []byte, file string) config.ErrorList {
	_, errs := daemon.CheckConfigData(data, file)
	return errs
}

// validateConfig checks the file with daemon.CheckConfig: it loads it
// exactly as startup does, runs the checks the providers make on their
// sections, prints every problem, one per line, and exits. It never opens a
// listener or starts anything.
func validateConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name+" validate-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file to check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s validate-config: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	if _, errs := daemon.CheckConfig(*path); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(stderr, e.Error())
		}
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s: configuration is valid\n", *path); err != nil {
		return 1
	}
	return 0
}

// opSupervise runs one detached operation from its directory's spec.json.
// systemd-run starts it as the main process of nexus-op-<id>.service; the
// process backend starts it with --process on a host without systemd.
func opSupervise(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(name+" op-supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the operation's directory")
	process := fs.Bool("process", false, "started by the process backend, not as a systemd unit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s op-supervise: usage: %s op-supervise --dir dir [--process]\n", name, name)
		return 2
	}
	mode := detach.ModeUnit
	if *process {
		mode = detach.ModeProcess
	}
	return detach.Supervise(*dir, stderr, mode)
}

// serve runs the daemon until SIGTERM. Logs go to stdout as JSON; only
// failures to start are written to stderr.
func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name+" serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file")
	stateDir := fs.String("state-dir", auth.DefaultStateDir, "state directory (tokens.json, state.db)")
	auditDir := fs.String("audit-dir", audit.DefaultDir, "audit log directory (audit.jsonl and its rotations)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s serve: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stdout, nil))
	tlsConf, err := loadTLS(cfg, *stateDir, log)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	d, err := daemon.New(cfg, daemon.Options{ConfigPath: *path, StateDir: *stateDir, AuditDir: *auditDir,
		Logger: log, TLS: tlsConf})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// loadTLS returns the certificate to serve, or nil in tls.mode off. In auto
// mode it is generated on first start under the state directory. Its SHA-256
// fingerprint goes to stdout in a log record, for clients to pin.
func loadTLS(cfg *config.Config, stateDir string, log *slog.Logger) (*tls.Config, error) {
	var cert tls.Certificate
	var certPath string
	generated := false
	switch cfg.TLS.Mode {
	case config.TLSOff:
		return nil, nil
	case config.TLSAuto:
		dir := tlsutil.Dir(stateDir)
		certPath = filepath.Join(dir, tlsutil.CertFileName)
		var err error
		cert, generated, err = tlsutil.LoadOrCreate(dir, cfg.Host.Name, cfg.TLS.ExtraSANs, time.Now())
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
	case config.TLSFiles:
		certPath = cfg.TLS.CertFile
		var err error
		cert, err = tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
	default:
		return nil, fmt.Errorf("tls: unknown mode %q", cfg.TLS.Mode)
	}
	msg := "serving TLS certificate"
	if generated {
		msg = "generated a self-signed TLS certificate; clients pin its fingerprint"
	}
	log.Info(msg, "mode", cfg.TLS.Mode, "certFile", certPath, "fingerprint", tlsutil.Fingerprint(cert.Certificate[0]),
		"generated", generated)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// tlsCommand is `nexusd tls fingerprint`: it prints the SHA-256 fingerprint
// of the certificate nexusd serves, to compare out of band with what a
// client was shown. It never generates a certificate.
func tlsCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "fingerprint" {
		fmt.Fprintf(stderr, "Usage:\n  %s tls fingerprint [--config path] [--state-dir dir]\n", name)
		return 2
	}
	fs := flag.NewFlagSet(name+" tls fingerprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "configuration file")
	stateDir := fs.String("state-dir", auth.DefaultStateDir, "state directory holding tls/cert.pem in tls.mode auto")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s tls fingerprint: unexpected argument %q\n", name, fs.Arg(0))
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var certPath string
	switch cfg.TLS.Mode {
	case config.TLSOff:
		fmt.Fprintf(stderr, "%s: tls.mode is off in %s, so there is no certificate\n", name, *path)
		return 1
	case config.TLSFiles:
		certPath = cfg.TLS.CertFile
	default:
		certPath = filepath.Join(tlsutil.Dir(*stateDir), tlsutil.CertFileName)
	}
	fp, err := tlsutil.FileFingerprint(certPath)
	if errors.Is(err, os.ErrNotExist) && cfg.TLS.Mode == config.TLSAuto {
		fmt.Fprintf(stderr, "%s: no certificate at %s yet; nexusd serve generates it on first start\n", name, certPath)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, fp); err != nil {
		return 1
	}
	return 0
}
