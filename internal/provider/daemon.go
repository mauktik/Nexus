package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/mauktik/nexus/internal/config"
)

// Daemon is what a provider may know of the nexusd process serving it: where
// its configuration and state live, when it started, the FR-3 check of its
// configuration file or of candidate bytes, and a reload through the
// daemon's own loop.
type Daemon interface {
	ConfigPath() string
	StateDir() string
	StartedAt() time.Time
	// CheckConfig is daemon.CheckConfig(ConfigPath()): the same check as
	// `nexusd validate-config`.
	CheckConfig() ConfigCheck
	// CheckConfigData is daemon.CheckConfigData(data, ConfigPath()): the
	// same check on bytes that are not on disk yet. ConfigPath only names
	// the source in the errors.
	CheckConfigData(data []byte) ConfigCheck
	// Reload re-reads the configuration file and applies it through the
	// daemon's run loop, serialized with SIGHUP. A reload the loop has
	// taken runs to completion even if ctx ends first; Reload then returns
	// ctx.Err() and the result is dropped.
	Reload(ctx context.Context) (ReloadResult, error)
}

// ConfigCheck is the outcome of Daemon.CheckConfig and CheckConfigData.
// Config is nil when the file or bytes cannot be loaded; Errors is empty only for a file nexusd would start
// with.
type ConfigCheck struct {
	Config *config.Config
	Errors config.ErrorList
}

// ReloadResult is the outcome of a reload that was applied.
type ReloadResult struct {
	// RestartRequired lists the top-level keys that changed but take effect
	// only after a restart, sorted.
	RestartRequired []string
}

// ReloadError is a reload that was not applied; the configuration in force
// is unchanged. Stage is "load" when the file could not be loaded or
// validated, and "apply" when a provider refused its section.
type ReloadError struct {
	Stage string
	Err   error
}

func (e *ReloadError) Error() string { return fmt.Sprintf("reload %s: %v", e.Stage, e.Err) }

func (e *ReloadError) Unwrap() error { return e.Err }

// DaemonUser is a provider that needs the Daemon. The daemon calls UseDaemon
// once, after it is constructed and before serving any request.
type DaemonUser interface {
	UseDaemon(Daemon)
}
