package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/dnswrite"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ dnswrite.Mutator = (*v5.Mutator)(nil)

// dnsBindings binds add-zone and zones.remove. Every DNS write goes through
// dnswrite.Apply in the helper; nothing else replaces the DNS file.
func (p *Provider) dnsBindings() []provider.Binding {
	return []provider.Binding{
		detachedOp(p, spec.AddZone, p.admitAddZone, maxRuntimeDefault, dnsLost[spec.ZoneAddInput](p)),
		detachedOp(p, spec.ZonesRemove, p.admitRemoveZone, maxRuntimeDefault, dnsLost[spec.ZoneRemoveInput](p)),
	}
}

// registerDNSActions adds the helper actions of add-zone and zones.remove.
func registerDNSActions(m map[string]helperAction) {
	m[spec.AddZone.Op().ID] = runAddZone
	m[spec.ZonesRemove.Op().ID] = runRemoveZone
}

// dnsLost is the outcome-unknown detail of a DNS write whose unit was lost
// without a result: the swap may have happened, and the pipeline kept the
// previous bytes in the operation's directory.
func dnsLost[In any](p *Provider) func(*provider.Run, In) string {
	return func(r *provider.Run, _ In) string {
		dir, err := p.opDir(r.ID())
		if err != nil {
			dir = "the operation's directory"
		}
		return fmt.Sprintf("the DNS file may have been replaced; the previous bytes are at %s; check `pihole get`",
			filepath.Join(dir, dnswrite.RollbackCopyName))
	}
}

func (p *Provider) admitAddZone(ctx context.Context, in spec.ZoneAddInput) error {
	return p.admitDNS(ctx, in.IfMatch, dnsfile.AddZone{Zone: in.Zone})
}

func (p *Provider) admitRemoveZone(ctx context.Context, in spec.ZoneRemoveInput) error {
	return p.admitDNS(ctx, in.IfMatch, dnsfile.RemoveZone{Zone: in.Name, Cascade: in.Cascade})
}

// admitDNS is the shared DNS Admit, run in nexusd before an operation
// exists: the backend can write the DNS file, dnsmasq loads it (checked
// live, never from the cached dnsWrites detail, A-53), ifMatch is the file's
// etag, and the model accepts the change. The helper's pipeline checks all
// of it again under the lane.
func (p *Provider) admitDNS(ctx context.Context, ifMatch string, c dnsfile.Change) error {
	rd, err := p.backendReader(ctx)
	if err != nil {
		return err
	}
	if pe := dnsWritable(p.paths, rd.Name(), p.Settings().DNSFile); pe != nil {
		return probErr(pe)
	}
	m, err := p.dnsModel()
	if err != nil {
		return err
	}
	if ifMatch != "" && ifMatch != m.Etag {
		return probErr(problem.New(problem.DNSFileConflict,
			"the DNS file changed since it was read (ifMatch is not its current etag); nothing was changed").
			With("expectedEtag", ifMatch).With("currentEtag", m.Etag))
	}
	if err := m.Apply(c); err != nil {
		return probErr(changeRefused(c, err))
	}
	return nil
}

// confDirReason is why dnsFile cannot be written when /etc/dnsmasq.conf
// does not load its directory.
func confDirReason(dnsFile string) string {
	return fmt.Sprintf("dnsmasq does not load %s (no conf-dir in %s)", filepath.Dir(dnsFile), v5.DnsmasqConfFile)
}

// dnsWritable refuses a DNS write on the v6 backend (ASM-6) and when
// dnsmasq does not load dnsFile's directory (V-10), as
// capability-unavailable.
func dnsWritable(paths v5.Paths, backendName, dnsFile string) *problem.Error {
	reason := ""
	switch {
	case backendName == spec.BackendUnsupported:
		reason = backend.ReasonV6DNSWrites
	case !v5.ConfDirLoads(paths, dnsFile):
		reason = confDirReason(dnsFile)
	default:
		return nil
	}
	return problem.New(problem.CapabilityUnavailable, reason).With("provider", spec.Name).With("reason", reason)
}

// changeRefused maps a model refusal (FR-19, FR-21): an existing zone or
// record, a zone that still has records, or a duplicate name is conflict;
// an undeclared zone or a missing record is not-found; a record name under
// no zone is dns-name-unzoned.
func changeRefused(c dnsfile.Change, err error) *problem.Error {
	detail := err.Error() + "; nothing was changed"
	var notEmpty dnsfile.ErrZoneNotEmpty
	switch {
	case errors.Is(err, dnsfile.ErrZoneNotFound), errors.Is(err, dnsfile.ErrRecordNotFound):
		return notFound(detail)
	case errors.Is(err, dnsfile.ErrUnzoned):
		pe := problem.New(problem.DNSNameUnzoned, detail)
		if ar, ok := c.(dnsfile.AddRecord); ok {
			pe = pe.With("name", ar.FQDN)
		}
		return pe
	case errors.Is(err, dnsfile.ErrZoneExists), errors.Is(err, dnsfile.ErrRecordExists),
		errors.Is(err, dnsfile.ErrDuplicate), errors.As(err, &notEmpty):
		return problem.New(problem.Conflict, detail)
	case errors.Is(err, dnsfile.ErrInvalidChange):
		return invalid("", err.Error())
	}
	return problem.New(problem.InternalError, detail)
}

func runAddZone(ctx context.Context, c *helperCall) (any, error) {
	var in spec.ZoneAddInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the add-zone input: %v; nothing was changed", err)))
	}
	return applyDNS(ctx, c, in.IfMatch, dnsfile.AddZone{Zone: in.Zone})
}

func runRemoveZone(ctx context.Context, c *helperCall) (any, error) {
	var in spec.ZoneRemoveInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the zones.remove input: %v; nothing was changed", err)))
	}
	return applyDNS(ctx, c, in.IfMatch, dnsfile.RemoveZone{Zone: in.Name, Cascade: in.Cascade})
}

// applyDNS runs the DNS write pipeline on the DNS file Prepare recorded.
// The pipeline's problem crosses into the error only through probErr, so a
// success is never a non-nil error holding a nil *problem.Error.
func applyDNS(ctx context.Context, c *helperCall, ifMatch string, changes ...dnsfile.Change) (any, error) {
	if pe := dnsWritable(c.Paths, c.In.Backend, c.In.DNSFile); pe != nil {
		return nil, probErr(pe)
	}
	dm, ok := c.Mutator.(dnswrite.Mutator)
	if !ok {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot write the DNS file; nothing was changed", c.In.Backend)))
	}
	res, pe := dnswrite.Apply(ctx, dnswrite.Request{
		DNSFile: c.Paths.Host(c.In.DNSFile), StateDir: c.In.StateDir, OpDir: c.In.OpDir,
		IfMatch: ifMatch, Changes: changes,
	}, dnswrite.Deps{Mutator: dm, DNSServer: c.DNSServer, Out: c.Out, Now: c.Now})
	return res, probErr(pe)
}
