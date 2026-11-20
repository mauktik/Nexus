package storage

import (
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// Bindings binds the twelve storage operations in the spec's order: the
// nine sync reads, then the three detached writes.
func (p *Provider) Bindings() []provider.Binding {
	return append(p.readBindings(), p.writeBindings()...)
}

// readBindings binds the nine sync reads.
func (p *Provider) readBindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.HealthGet, p.healthGet),
		provider.Sync(spec.PoolsList, p.poolsList),
		provider.Sync(spec.PoolsGet, p.poolsGet),
		provider.Sync(spec.DatasetsList, p.datasetsList),
		provider.Sync(spec.DatasetsGet, p.datasetsGet),
		provider.Sync(spec.SnapshotsList, p.snapshotsList),
		provider.Sync(spec.BackupsList, p.backupsList),
		provider.Sync(spec.BackupsSummary, p.backupsSummary),
		provider.Sync(spec.TimeMachineList, p.timemachineList),
	}
}
