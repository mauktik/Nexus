package nexusd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/version"
)

// DaemonName is the name of the host's one daemon resource, so its id is
// /providers/nexusd/daemons/nexusd.
const DaemonName = "nexusd"

// VersionUnknown is a rollback binary's version when "-version" fails, is
// not "nexusd <version>", or takes longer than versionTimeout.
const VersionUnknown = "unknown"

// versionTimeout bounds a "-version" run (FR-2).
const versionTimeout = 5 * time.Second

// versionLineRE is what "nexusd -version" prints (cmd/nexusd: "nexusd " +
// version.Version + "\n").
var versionLineRE = regexp.MustCompile(`^nexusd ([0-9A-Za-z][0-9A-Za-z.+_-]{0,127})\n?$`)

// parseVersion returns the version "nexusd -version" printed, or false.
func parseVersion(out []byte) (string, bool) {
	m := versionLineRE.FindSubmatch(out)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// get returns the host's daemon as a one-item list (F1, A-14).
func (p *Provider) get(ctx context.Context, _ spec.ListInput) (apitypes.List[spec.Daemon], error) {
	d, err := p.daemonOf()
	if err != nil {
		return apitypes.List[spec.Daemon]{}, err
	}
	exe, err := p.executable()
	if err != nil {
		return apitypes.List[spec.Daemon]{}, fmt.Errorf("nexusd: running executable: %w", err)
	}
	started := d.StartedAt()
	props := spec.DaemonProperties{
		Version:          version.Version,
		StartedAt:        started,
		UptimeSeconds:    max(0, int64(p.now().Sub(started)/time.Second)),
		ConfigPath:       d.ConfigPath(),
		ExecutablePath:   exe,
		Previous:         p.previous(ctx),
		UpdateConfigured: len(p.current().keys) > 0,
		SelfManagement:   p.selfManagementReady(),
		ConfigSet:        p.configSetReady(),
	}
	r := apitypes.NewResource(p.host, spec.Name, spec.DaemonsType, DaemonName, props)
	return apitypes.List[spec.Daemon]{Items: []spec.Daemon{r}}, nil
}

// fileKey identifies one content of a file: a rename over it, a rewrite or
// a touch changes the key (NFR-2).
type fileKey struct {
	dev, ino uint64
	size     int64
	mtime    int64
}

func keyOf(fi fs.FileInfo) fileKey {
	k := fileKey{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.dev, k.ino = uint64(st.Dev), uint64(st.Ino) // Dev is int32 on darwin, uint64 on linux
	}
	return k
}

// prevCache remembers the version of the rollback binary per file key, so
// get runs "-version" only when the file changed. sem lets one probe run
// at a time; a get that waits for it gives up when its context ends.
type prevCache struct {
	sem chan struct{}

	mu      sync.Mutex
	key     fileKey
	version string
	valid   bool
}

func (c *prevCache) lookup(k fileKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.key == k {
		return c.version, true
	}
	return "", false
}

func (c *prevCache) store(k fileKey, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.version, c.valid = k, v, true
}

// previous reports the rollback binary. It never fails: what cannot be
// known is reported as VersionUnknown.
func (p *Provider) previous(ctx context.Context) spec.Previous {
	path := p.paths.Prev
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return spec.Previous{}
	}
	unknown := spec.Previous{Present: true, Path: path, Version: VersionUnknown}
	if err != nil {
		slog.Warn("nexusd: cannot stat the rollback binary", "path", path, "error", err)
		return unknown
	}
	if !fi.Mode().IsRegular() {
		return unknown
	}
	k := keyOf(fi)
	if v, ok := p.prev.lookup(k); ok {
		return spec.Previous{Present: true, Path: path, Version: v}
	}
	select {
	case p.prev.sem <- struct{}{}:
	case <-ctx.Done():
		return unknown
	}
	defer func() { <-p.prev.sem }()
	// Another get may have probed the same file while this one waited.
	if v, ok := p.prev.lookup(k); ok {
		return spec.Previous{Present: true, Path: path, Version: v}
	}
	v, cache := p.probeVersion(ctx, path)
	if cache {
		p.prev.store(k, v)
	}
	return spec.Previous{Present: true, Path: path, Version: v}
}

// errVersionTimeout ends a "-version" run that took versionTimeout.
var errVersionTimeout = errors.New("-version took longer than 5s")

// probeVersion runs "path -version" for at most versionTimeout and returns
// the version, or VersionUnknown. cache is false when the caller's context
// ended first: that says nothing about the file.
func (p *Provider) probeVersion(ctx context.Context, path string) (v string, cache bool) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := p.afterFunc(versionTimeout, func() { cancel(errVersionTimeout) })
	defer stop()
	res, err := p.run.Run(runCtx, []string{path, "-version"})
	if ctx.Err() != nil {
		return VersionUnknown, false
	}
	if err != nil || res.ExitCode != 0 {
		slog.Warn("nexusd: the rollback binary's -version failed", "path", path, "exitCode", res.ExitCode,
			"error", fmt.Sprint(err))
		return VersionUnknown, true
	}
	if v, ok := parseVersion(res.Stdout); ok {
		return v, true
	}
	return VersionUnknown, true
}

// validate checks the configuration file the daemon was started with, on
// disk now, with exactly the checks of `nexusd validate-config` (F2). An
// invalid file is a result with valid false, not an error (FR-3).
func (p *Provider) validate(context.Context, spec.ValidateInput) (spec.ValidateResult, error) {
	d, err := p.daemonOf()
	if err != nil {
		return spec.ValidateResult{}, err
	}
	errs := configErrors(d.CheckConfig().Errors)
	return spec.ValidateResult{
		Host:       p.host,
		Valid:      len(errs) == 0,
		ConfigPath: d.ConfigPath(),
		Errors:     errs,
	}, nil
}

// configErrors maps a check's errors to the wire type. A zero location
// field is omitted from the JSON. It never returns nil.
func configErrors(list config.ErrorList) []spec.ConfigError {
	out := make([]spec.ConfigError, 0, len(list))
	for _, e := range list {
		if e == nil {
			continue
		}
		out = append(out, spec.ConfigError{File: e.File, Line: e.Line, Column: e.Column, Path: e.Path, Message: e.Msg})
	}
	return out
}
