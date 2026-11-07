// Package catalog lists every operation declaration compiled into Nexus: each
// provider's spec package and, from 1.5, internal/systemspec. It is the
// composition root for the CLI and the documentation generator, so it must
// import declarations only, never implementations (CON-5, D-1). Adding a
// provider adds one line to All.
package catalog

import (
	"github.com/mauktik/nexus/internal/def"
	apt "github.com/mauktik/nexus/internal/providers/apt/spec"
	docker "github.com/mauktik/nexus/internal/providers/docker/spec"
	scripts "github.com/mauktik/nexus/internal/providers/scripts/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// All returns every provider's declarations, the system operations first.
func All() []def.ProviderSpec {
	return []def.ProviderSpec{
		systemspec.Spec(),
		docker.Spec(),
		apt.Spec(),
		scripts.Spec(),
	}
}

// Ops returns every declared operation across all providers.
func Ops() []def.Op {
	var ops []def.Op
	for _, spec := range All() {
		ops = append(ops, spec.Ops...)
	}
	return ops
}
