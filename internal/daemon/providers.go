package daemon

import (
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/scripts"
)

// builtinProviders returns the providers compiled into nexusd, for host. This
// is a composition root: adding a provider adds one line here (D-1).
func builtinProviders(host string) []provider.Provider {
	return []provider.Provider{
		scripts.New(scripts.Options{Host: host}),
	}
}

// providerConfigs maps each provider name to its configuration section.
func providerConfigs(cfg *config.Config) map[string]core.ProviderConfig {
	p := cfg.Providers
	return map[string]core.ProviderConfig{
		"docker":  {Enabled: p.Docker.Settings.Enabled, Decode: p.Docker.Decode},
		"apt":     {Enabled: p.Apt.Settings.Enabled, Decode: p.Apt.Decode},
		"scripts": {Enabled: p.Scripts.Settings.Enabled, Decode: p.Scripts.Decode},
	}
}
