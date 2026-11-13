package nexusd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// admitReload refuses, before an operation exists, a reload of a file that
// fails the FR-3 check: config-invalid with the path and every error (F3).
func (p *Provider) admitReload(context.Context, spec.ReloadInput) error {
	d, err := p.daemonOf()
	if err != nil {
		return err
	}
	return configValid(d.ConfigPath(), d.CheckConfig().Errors)
}

// configValid returns nil for a file with no errors, and config-invalid
// carrying configPath and the located errors otherwise. It returns a plain
// error, never a typed nil *problem.Error.
func configValid(path string, list config.ErrorList) error {
	errs := configErrors(list)
	if len(errs) == 0 {
		return nil
	}
	detail := fmt.Sprintf("%s has %d error(s); nothing was applied", path, len(errs))
	return problem.New(problem.ConfigInvalid, detail).With("configPath", path).With("errors", errs)
}

// reload applies the configuration file through the daemon's run loop,
// serialized with SIGHUP (F3). A reload the daemon refused leaves the
// configuration in force unchanged and fails config-apply-failed.
func (p *Provider) reload(ctx context.Context, _ spec.ReloadInput, r *provider.Run) (spec.ReloadResult, error) {
	d, err := p.daemonOf()
	if err != nil {
		return spec.ReloadResult{}, err
	}
	fmt.Fprintf(r.Output(), "reloading %s\n", d.ConfigPath())
	res, err := d.Reload(ctx)
	if err != nil {
		var re *provider.ReloadError
		if errors.As(err, &re) {
			return spec.ReloadResult{}, problem.New(problem.ConfigApplyFailed, fmt.Sprintf(
				"the configuration was not applied (%s stage): %v; the configuration in force is unchanged",
				re.Stage, re.Err))
		}
		return spec.ReloadResult{}, fmt.Errorf("nexusd reload: %w", err)
	}
	keys := slices.Clone(res.RestartRequired)
	if keys == nil {
		keys = []string{}
	}
	fmt.Fprintln(r.Output(), "configuration applied")
	if len(keys) > 0 {
		fmt.Fprintf(r.Output(), "takes effect after a restart: %s\n", strings.Join(keys, ", "))
	}
	return spec.ReloadResult{
		ActionResult:    apitypes.ActionResult{Changed: true},
		Applied:         true,
		RestartRequired: keys,
	}, nil
}
