package daemon

import (
	"errors"
	"io"
	"log/slog"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/provider"
)

// ValidateProviders applies to cfg the checks nexusd's providers make on
// their own sections at startup and reload (core.New, Registry.Apply):
// each built-in provider's Configure, enabled or not, with no preflight. It
// is what validate-config adds to config.Load, so a file that passes both
// cannot make nexusd exit at boot.
func ValidateProviders(cfg *config.Config) error {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return core.ValidateProviders(builtinProviders("", log), providerConfigs(cfg))
}

// CheckConfig is the FR-3 check of the file at path, shared by `nexusd
// validate-config` and the nexusd provider's validate-config: config.Load,
// then ValidateProviders, as one list. A file config.Load refuses yields
// its errors and no configuration. Otherwise the configuration comes back
// with the providers' errors, if any: those that are an ErrorList keep
// their own locations, and any other is reported at providers.<name>. The
// list is empty only for a file nexusd would start with.
func CheckConfig(path string) (*config.Config, config.ErrorList) {
	return checkConfigWith(path, builtinProviders)
}

// checkConfigWith is CheckConfig over the providers build returns, so a
// test can put a provider with background work behind it and see that
// validation never starts it.
func checkConfigWith(path string, build func(host string, log *slog.Logger) []provider.Provider) (*config.Config, config.ErrorList) {
	cfg, err := config.Load(path)
	if err != nil {
		var list config.ErrorList
		if errors.As(err, &list) {
			return nil, list
		}
		return nil, config.ErrorList{{File: path, Msg: err.Error()}}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfgs := providerConfigs(cfg)
	var list config.ErrorList
	// One provider at a time, so each error is known to be that provider's.
	for _, p := range build("", log) {
		err := core.ValidateProviders([]provider.Provider{p}, cfgs)
		list = append(list, providerErrors(path, p.Spec().Name, err)...)
	}
	return cfg, list
}

// providerErrors flattens err, which core.ValidateProviders returned for
// the provider name, into entries for file. Each errors.Join member that is
// or wraps an ErrorList contributes its entries; any other member becomes
// one entry at providers.<name>.
func providerErrors(file, name string, err error) config.ErrorList {
	if err == nil {
		return nil
	}
	members := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		members = j.Unwrap()
	}
	var list config.ErrorList
	for _, m := range members {
		var located config.ErrorList
		if errors.As(m, &located) {
			list = append(list, located...)
			continue
		}
		list = append(list, &config.Error{File: file, Path: "providers." + name, Msg: m.Error()})
	}
	return list
}
