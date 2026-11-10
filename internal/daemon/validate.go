package daemon

import (
	"io"
	"log/slog"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/core"
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
