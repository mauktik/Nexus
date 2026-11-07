package daemon

import (
	"context"
	"reflect"
	"slices"

	"github.com/mauktik/nexus/internal/config"
)

// reload re-reads and validates the configuration on SIGHUP. A file that
// fails to load or validate, or a section a provider rejects, is logged and
// the running configuration stays. On success provider enablement is swapped
// atomically. A reload is all or nothing across providers: providers are
// configured one by one, so when one refuses its section, any that accepted
// theirs are configured again from the sections in force. Settings fixed at
// startup, such as the listen addresses, are not applied: each changed one
// is logged as needing a restart.
func (d *Daemon) reload(ctx context.Context) {
	d.log.Info("reloading configuration", "path", d.path)
	next, err := config.Load(d.path)
	if err != nil {
		d.log.Error("configuration reload failed; keeping the current configuration",
			"path", d.path, "error", err.Error())
		return
	}
	for _, key := range restartRequired(d.cfg, next) {
		d.log.Warn("configuration change takes effect only after a restart; keeping the running value",
			"key", key, "path", d.path)
	}
	if err := d.reg.Apply(ctx, providerConfigs(next)); err != nil {
		d.log.Error("configuration reload failed; keeping the current configuration",
			"path", d.path, "error", err.Error())
		if rerr := d.reg.Apply(ctx, providerConfigs(d.applied)); rerr != nil {
			d.log.Error("restoring the provider sections in force failed", "path", d.path, "error", rerr.Error())
		}
		return
	}
	d.applied = next
	d.log.Info("configuration reloaded", "path", d.path)
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
	return keys
}
