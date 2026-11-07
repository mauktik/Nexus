// Package daemon wires nexusd together: configuration, providers, the core
// registry and the HTTP server, plus the process lifecycle: readiness
// notification, SIGHUP reload and bounded SIGTERM shutdown (FR-37).
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/server"
	"github.com/mauktik/nexus/internal/store"
	"github.com/mauktik/nexus/internal/version"
)

// ShutdownTimeout bounds graceful shutdown after SIGTERM (A-46).
const ShutdownTimeout = 10 * time.Second

// Options configures New. There is no second constructor: tests inject
// through these fields.
type Options struct {
	// Providers, when non-nil, replaces the providers compiled in through
	// providers.go, so daemon-level tests can run test providers.
	Providers []provider.Provider
	// ConfigPath is the file re-read on SIGHUP. Empty means
	// config.DefaultPath.
	ConfigPath string
	// Logger receives every log record. Nil means slog JSON on stdout.
	Logger *slog.Logger
	// StateDir holds tokens.json, state.db and the ops directory. Empty
	// means auth.DefaultStateDir.
	StateDir string
	// AuditDir holds audit.jsonl and its rotations. Empty means
	// audit.DefaultDir.
	AuditDir string
	// TLS holds the certificate to serve. It is required unless tls.mode is
	// off; cmd/nexusd loads or generates it (2.4).
	TLS *tls.Config
}

// Daemon is one nexusd serve process.
type Daemon struct {
	cfg    *config.Config // As started: listener-level settings stay as they were
	path   string
	log    *slog.Logger
	reg    *core.Registry
	audit  *audit.Log
	store  *store.Store
	ops    *ops.Manager
	srv    *server.Server
	notify *notifier
	ready  chan struct{}

	mu    sync.Mutex
	addrs []net.Addr
}

// New builds the daemon from a loaded configuration. Provider preflights run
// here; a failed one makes that provider unavailable and never fails New.
func New(cfg *config.Config, opts Options) (*Daemon, error) {
	if cfg.TLS.Mode != config.TLSOff && opts.TLS == nil {
		return nil, fmt.Errorf("tls.mode is %s but no certificate was loaded", cfg.TLS.Mode)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	path := opts.ConfigPath
	if path == "" {
		path = config.DefaultPath
	}
	providers := opts.Providers
	if providers == nil {
		providers = builtinProviders()
	}
	// The audit log opens first: no request is served without it, and a
	// directory it cannot use stops startup rather than every mutation.
	auditLog, err := audit.Open(audit.Options{
		Dir:             opts.AuditDir,
		Host:            cfg.Host.Name,
		RetentionDays:   cfg.Audit.RetentionDays,
		AggregateWindow: auth.FailureWindow,
		Logger:          log,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = auth.DefaultStateDir
	}
	// state.db is locked while open, so a second daemon on the same state
	// directory stops here (ErrLocked).
	db, err := store.Open(stateDir, store.Options{})
	if err != nil {
		if cerr := auditLog.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, fmt.Errorf("state: %w", err)
	}
	fail := func(err error) (*Daemon, error) {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		if cerr := auditLog.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, err
	}
	mgr, err := ops.New(ops.Options{
		Store:          db,
		Audit:          auditLog,
		Dir:            filepath.Join(stateDir, "ops"),
		Host:           cfg.Host.Name,
		OutputCapBytes: cfg.Limits.OutputCapBytes,
		Logger:         log,
	})
	if err != nil {
		return fail(fmt.Errorf("operations: %w", err))
	}
	reg, err := core.New(providers, core.Options{
		Host:      cfg.Host.Name,
		Version:   version.Version,
		Providers: providerConfigs(cfg),
		Logger:    log,
		Audit:     auditLog,
		Ops:       mgr,
	})
	if err != nil {
		return fail(fmt.Errorf("providers: %w", err))
	}
	authn := auth.New(stateDir, auth.Options{FailureLimitPerMinute: cfg.Auth.FailureLimitPerMinute, Logger: log,
		Audit: auditLog})
	srv, err := server.New(reg, server.Options{MaxBodyBytes: cfg.Limits.MaxBodyBytes, Logger: log, Auth: authn,
		TLS: opts.TLS})
	if err != nil {
		return fail(fmt.Errorf("server: %w", err))
	}
	return &Daemon{
		cfg:    cfg,
		path:   path,
		log:    log,
		reg:    reg,
		audit:  auditLog,
		store:  db,
		ops:    mgr,
		srv:    srv,
		notify: newNotifier(os.Getenv("NOTIFY_SOCKET")),
		ready:  make(chan struct{}),
	}, nil
}

// Ready is closed once every listener is open and READY=1 has been sent.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Addrs returns the bound listener addresses. It is empty before Ready.
func (d *Daemon) Addrs() []net.Addr {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

// Run opens the listeners, serves, and handles signals until SIGTERM, SIGINT
// or the end of ctx, then shuts down within ShutdownTimeout. It returns nil
// after a clean stop and an error when a listener cannot be opened or fails.
// Nothing runs in the background between requests (NFR-1).
func (d *Daemon) Run(ctx context.Context) error {
	// Before the listener opens: SIGHUP's default action would kill the
	// process if a reload arrived right after READY=1.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	listeners, err := d.listen(ctx)
	if err != nil {
		if cerr := d.store.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		if cerr := d.audit.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return err
	}
	serveErr := make(chan error, len(listeners))
	var wg sync.WaitGroup
	for _, ln := range listeners {
		wg.Go(func() {
			if err := d.srv.Serve(ln); err != nil {
				serveErr <- err
			}
		})
	}

	addrs := make([]string, len(listeners))
	for i, ln := range listeners {
		addrs[i] = ln.Addr().String()
	}
	d.log.Info("nexusd ready", "version", version.Version, "host", d.cfg.Host.Name, "listen", addrs)
	close(d.ready)
	if err := d.notify.notify("READY=1"); err != nil {
		d.log.Warn("readiness notification failed", "error", err.Error())
	}

	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			d.log.Info("stopping", "reason", "context canceled")
			break loop
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				d.reload(ctx)
				continue
			}
			d.log.Info("stopping", "signal", sig.String())
			break loop
		case err := <-serveErr:
			runErr = err
			break loop
		}
	}

	d.shutdown()
	wg.Wait()
	for _, ln := range listeners {
		// Normally already closed by Shutdown.
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			d.log.Debug("close listener", "addr", ln.Addr().String(), "error", err.Error())
		}
	}
	return runErr
}

// listen opens every configured address, or none.
func (d *Daemon) listen(ctx context.Context) ([]net.Listener, error) {
	var lc net.ListenConfig
	var listeners []net.Listener
	for _, addr := range d.cfg.Listen {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, open := range listeners {
				if cerr := open.Close(); cerr != nil {
					err = errors.Join(err, cerr)
				}
			}
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		listeners = append(listeners, ln)
	}
	d.mu.Lock()
	for _, ln := range listeners {
		d.addrs = append(d.addrs, ln.Addr())
	}
	d.mu.Unlock()
	return listeners, nil
}

// shutdown stops intake and waits for in-flight requests, at most
// ShutdownTimeout. The server cancels every request's base context first, so
// long-lived streams end at once instead of holding the deadline.
func (d *Daemon) shutdown() {
	if err := d.notify.notify("STOPPING=1"); err != nil {
		d.log.Warn("stopping notification failed", "error", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := d.srv.Shutdown(ctx); err != nil {
		d.log.Warn("graceful shutdown incomplete; remaining connections closed", "error", err.Error())
	}
	// After the server, so no request admits an operation meanwhile. Queued
	// operations stay queued in state.db for restart recovery.
	if err := d.ops.Shutdown(ctx); err != nil {
		d.log.Warn("operations still running at shutdown", "error", err.Error())
	}
	if err := d.store.Close(); err != nil {
		d.log.Error("closing the state database failed", "error", err.Error())
	}
	// After the server: in-flight requests may still write records.
	if err := d.audit.Close(); err != nil {
		d.log.Error("closing the audit log failed", "error", err.Error())
	}
	d.log.Info("nexusd stopped")
}
