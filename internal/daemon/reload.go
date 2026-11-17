package daemon

import (
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/provider"
)

// errStopping is Reload's error once Run has left its loop.
var errStopping = errors.New("nexusd is stopping; the configuration was not reloaded")

// reloadRequest is one Reload call handed to the Run loop. reply has a
// buffer of exactly one and the loop sends on it exactly once, so that send
// never blocks, whether or not the caller is still waiting.
type reloadRequest struct{ reply chan reloadReply }

type reloadReply struct {
	res provider.ReloadResult
	err error
}

// Reload implements provider.Daemon. The request goes through the Run loop,
// which runs the same reload as SIGHUP under its own context, so the two
// never overlap. A reload the loop has taken runs to completion even if ctx
// ends first, since applying is all or nothing; Reload then returns
// ctx.Err() at once and the result is dropped. Once Run has left its loop,
// Reload returns an error without reloading.
func (d *Daemon) Reload(ctx context.Context) (provider.ReloadResult, error) {
	// A context already done never hands a request to the loop, even when
	// the loop is ready to take it.
	if err := ctx.Err(); err != nil {
		return provider.ReloadResult{}, err
	}
	req := reloadRequest{reply: make(chan reloadReply, 1)}
	select {
	case d.reloads <- req:
	case <-ctx.Done():
		return provider.ReloadResult{}, ctx.Err()
	case <-d.stopping:
		return provider.ReloadResult{}, errStopping
	}
	select {
	case r := <-req.reply:
		return r.res, r.err
	case <-ctx.Done():
		return provider.ReloadResult{}, ctx.Err()
	case <-d.stopping:
		// The loop replies before it can leave, so a reply sent just
		// before stopping closed is still delivered.
		select {
		case r := <-req.reply:
			return r.res, r.err
		default:
			return provider.ReloadResult{}, errStopping
		}
	}
}

// reload re-reads and validates the configuration, on SIGHUP and for
// Reload. A file that fails to load or validate, or a section a provider
// rejects, is logged and the running configuration stays; the error is a
// *provider.ReloadError of stage load or apply. On success provider
// enablement is swapped atomically and the MCP tool list is rebuilt to
// match, and the background work of providers follows their enablement
// (provider.Background). A reload is all or nothing across providers:
// providers are configured one by one, so when one refuses its section, any
// that accepted theirs are configured again from the sections in force.
// Settings fixed at startup, such as the listen addresses, are not applied:
// each changed one is logged as needing a restart and reported, sorted, in
// the result. Only the Run loop calls it.
func (d *Daemon) reload(ctx context.Context) (provider.ReloadResult, error) {
	d.log.Info("reloading configuration", "path", d.path)
	next, err := config.Load(d.path)
	if err != nil {
		d.log.Error("configuration reload failed; keeping the current configuration",
			"path", d.path, "error", err.Error())
		return provider.ReloadResult{}, &provider.ReloadError{Stage: "load", Err: err}
	}
	keys := restartRequired(d.cfg, next)
	for _, key := range keys {
		d.log.Warn("configuration change takes effect only after a restart; keeping the running value",
			"key", key, "path", d.path)
	}
	if err := d.reg.Apply(ctx, providerConfigs(next)); err != nil {
		d.log.Error("configuration reload failed; keeping the current configuration",
			"path", d.path, "error", err.Error())
		if rerr := d.reg.Apply(ctx, providerConfigs(d.applied)); rerr != nil {
			d.log.Error("restoring the provider sections in force failed", "path", d.path, "error", rerr.Error())
		}
		return provider.ReloadResult{}, &provider.ReloadError{Stage: "apply", Err: err}
	}
	d.applied = next
	// Only after a successful apply: a refused or restored one changes no
	// background run.
	d.bg.reconcile(enabledOf(next))
	// The MCP tool list follows provider enablement. The new list is swapped
	// in whole; calls in flight finish on the one they started with.
	if err := d.srv.RefreshMCP(); err != nil {
		d.log.Error("rebuilding the MCP tool list failed; keeping the current list",
			"path", d.path, "error", err.Error())
	}
	d.log.Info("configuration reloaded", "path", d.path)
	// Sorted, and empty rather than nil when nothing needs a restart.
	sorted := append([]string{}, keys...)
	slices.Sort(sorted)
	return provider.ReloadResult{RestartRequired: sorted}, nil
}

// restartRequired lists the top-level keys that differ between the running
// configuration and next but are fixed at startup.
func restartRequired(running, next *config.Config) []string {
	var keys []string
	if !slices.Equal(running.Listen, next.Listen) {
		keys = append(keys, "listen")
	}
	if !reflect.DeepEqual(running.TLS, next.TLS) {
		keys = append(keys, "tls")
	}
	if running.Host != next.Host {
		keys = append(keys, "host")
	}
	if running.Limits != next.Limits {
		keys = append(keys, "limits")
	}
	// The authenticator and its rate limiter are built once at startup.
	if running.Auth != next.Auth {
		keys = append(keys, "auth")
	}
	// The audit log is opened once at startup with its retention.
	if running.Audit != next.Audit {
		keys = append(keys, "audit")
	}
	// The /mcp Origin guard is built once at startup.
	if !slices.Equal(running.MCP.AllowedOrigins, next.MCP.AllowedOrigins) {
		keys = append(keys, "mcp")
	}
	return keys
}
