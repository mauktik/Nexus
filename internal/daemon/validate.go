// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

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
// validate-config` and the nexusd provider's validate-config:
// config.ReadFile, then CheckConfigData on its bytes. A file that cannot be
// read yields its error and no configuration.
func CheckConfig(path string) (*config.Config, config.ErrorList) {
	return checkConfigWith(path, builtinProviders)
}

// CheckConfigData is the FR-3 check of configuration bytes, which need not
// be on disk; file only names the source in the errors. It is config.Parse,
// then ValidateProviders, as one list. Bytes config.Parse refuses yield its
// errors and no configuration. Otherwise the configuration comes back with
// the providers' errors, if any: those that are an ErrorList keep their
// own locations, and any other is reported at providers.<name>. The list is
// empty only for bytes nexusd would start with.
func CheckConfigData(data []byte, file string) (*config.Config, config.ErrorList) {
	return checkConfigDataWith(data, file, builtinProviders)
}

// checkConfigWith is CheckConfig over the providers build returns, so a
// test can put a provider with background work behind it and see that
// validation never starts it.
func checkConfigWith(path string, build func(host string, log *slog.Logger) []provider.Provider) (*config.Config, config.ErrorList) {
	data, err := config.ReadFile(path)
	if err != nil {
		return nil, errorList(path, err)
	}
	return checkConfigDataWith(data, path, build)
}

// checkConfigDataWith is CheckConfigData over the providers build returns:
// the one check both CheckConfig and CheckConfigData run.
func checkConfigDataWith(data []byte, file string, build func(host string, log *slog.Logger) []provider.Provider) (*config.Config, config.ErrorList) {
	cfg, err := config.Parse(data, file)
	if err != nil {
		return nil, errorList(file, err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfgs := providerConfigs(cfg)
	var list config.ErrorList
	// One provider at a time, so each error is known to be that provider's.
	for _, p := range build("", log) {
		err := core.ValidateProviders([]provider.Provider{p}, cfgs)
		list = append(list, providerErrors(file, p.Spec().Name, err)...)
	}
	return cfg, list
}

// errorList is err as an ErrorList: its own entries when it is one, or one
// entry naming file.
func errorList(file string, err error) config.ErrorList {
	var list config.ErrorList
	if errors.As(err, &list) {
		return list
	}
	return config.ErrorList{{File: file, Msg: err.Error()}}
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
