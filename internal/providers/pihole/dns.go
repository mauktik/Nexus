package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/dnswrite"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

var _ dnswrite.Mutator = (*v5.Mutator)(nil)

// dnsBindings binds add-zone, zones.remove, add-record, records.update,
// records.remove and apply-dns-changes. Every DNS write goes through
// dnswrite.Apply in the helper; nothing else replaces the DNS file.
func (p *Provider) dnsBindings() []provider.Binding {
	return []provider.Binding{
		detachedOp(p, spec.AddZone, p.admitAddZone, maxRuntimeDefault, dnsLost[spec.ZoneAddInput](p)),
		detachedOp(p, spec.ZonesRemove, p.admitRemoveZone, maxRuntimeDefault, dnsLost[spec.ZoneRemoveInput](p)),
		detachedOp(p, spec.AddRecord, p.admitAddRecord, maxRuntimeDefault, dnsLost[spec.RecordAddInput](p)),
		detachedOp(p, spec.RecordsUpdate, p.admitUpdateRecord, maxRuntimeDefault, dnsLost[spec.RecordUpdateInput](p)),
		detachedOp(p, spec.RecordsRemove, p.admitRemoveRecord, maxRuntimeDefault, dnsLost[spec.RecordRemoveInput](p)),
		detachedOp(p, spec.ApplyDNSChanges, p.admitApplyDNSChanges, maxRuntimeDefault, dnsLost[spec.DNSBatchInput](p)),
	}
}

// registerDNSActions adds the helper actions of the DNS writes.
func registerDNSActions(m map[string]helperAction) {
	m[spec.AddZone.Op().ID] = runAddZone
	m[spec.ZonesRemove.Op().ID] = runRemoveZone
	m[spec.AddRecord.Op().ID] = runAddRecord
	m[spec.RecordsUpdate.Op().ID] = runUpdateRecord
	m[spec.RecordsRemove.Op().ID] = runRemoveRecord
	m[spec.ApplyDNSChanges.Op().ID] = runApplyDNSChanges
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
	return p.admitDNS(ctx, in.IfMatch, singleRefused, dnsfile.AddZone{Zone: in.Zone})
}

func (p *Provider) admitRemoveZone(ctx context.Context, in spec.ZoneRemoveInput) error {
	return p.admitDNS(ctx, in.IfMatch, singleRefused, dnsfile.RemoveZone{Zone: in.Name, Cascade: in.Cascade})
}

func (p *Provider) admitAddRecord(ctx context.Context, in spec.RecordAddInput) error {
	return p.admitDNS(ctx, in.IfMatch, singleRefused, dnsfile.AddRecord{FQDN: in.FQDN, IPv4: in.IPv4})
}

func (p *Provider) admitUpdateRecord(ctx context.Context, in spec.RecordUpdateInput) error {
	fqdn, err := recordFQDN(in.Name)
	if err != nil {
		return err
	}
	return p.admitDNS(ctx, in.IfMatch, singleRefused, dnsfile.UpdateRecord{FQDN: fqdn, IPv4: in.IPv4})
}

func (p *Provider) admitRemoveRecord(ctx context.Context, in spec.RecordRemoveInput) error {
	fqdn, err := recordFQDN(in.Name)
	if err != nil {
		return err
	}
	return p.admitDNS(ctx, in.IfMatch, singleRefused, dnsfile.RemoveRecord{FQDN: fqdn})
}

// admitApplyDNSChanges admits a batch (FR-26): every item parses, and the
// changes apply in order to one model of the file, each against the file as
// the earlier ones left it.
func (p *Provider) admitApplyDNSChanges(ctx context.Context, in spec.DNSBatchInput) error {
	changes, pe := parseBatch(in.Changes)
	if pe != nil {
		return probErr(pe)
	}
	return p.admitDNS(ctx, in.IfMatch, batchRefused, changes...)
}

// parseBatch reads a batch's items with dnsfile.ParseChange (FR-26, A-36).
// The schema bounds the count and each item's shape; the helper's input does
// not pass through it, so both are checked again here. An item ParseChange
// refuses refuses the batch at its index.
func parseBatch(items []string) ([]dnsfile.Change, *problem.Error) {
	if len(items) == 0 || len(items) > spec.MaxChanges {
		return nil, invalid("/changes", fmt.Sprintf("must hold 1 to %d changes, not %d", spec.MaxChanges, len(items)))
	}
	changes := make([]dnsfile.Change, 0, len(items))
	for i, s := range items {
		c, err := dnsfile.ParseChange(s)
		if err != nil {
			return nil, batchRejected(i, problem.ValidationFailed, err.Error()+"; nothing was changed")
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// refusal maps the model's refusal of changes[i].
type refusal func(i int, c dnsfile.Change, err error) *problem.Error

// singleRefused is the refusal of a single zone or record operation.
func singleRefused(_ int, c dnsfile.Change, err error) *problem.Error { return changeRefused(c, err) }

// batchRefused is the refusal of a batch at any size: dns-batch-rejected
// naming the failing change's index and, as cause, the slug the single
// operation would have returned. The detail is the pipeline's
// (dnswrite changeProblem), so Admit and the helper read alike.
func batchRefused(i int, c dnsfile.Change, err error) *problem.Error {
	return batchRejected(i, changeRefused(c, err).Slug(), err.Error()+"; nothing was changed")
}

func batchRejected(i int, cause problem.Slug, detail string) *problem.Error {
	return problem.New(problem.DNSBatchRejected, fmt.Sprintf("changes[%d]: %s", i, detail)).
		With("changeIndex", i).With("cause", string(cause))
}

// oneChangeBatchRefused makes the pipeline's refusal of a one-change batch
// the batch's own. dnswrite.Apply names the index only when it applies more
// than one change, but a batch reports dns-batch-rejected at any size
// (TC-ERR28). The model refusals (dnswrite changeProblem) are the only
// not-found, conflict, dns-name-unzoned and validation-failed problems the
// pipeline returns; every other problem passes through.
func oneChangeBatchRefused(pe *problem.Error) *problem.Error {
	if pe == nil {
		return nil
	}
	switch pe.Slug() {
	case problem.NotFound, problem.Conflict, problem.DNSNameUnzoned, problem.ValidationFailed:
		return batchRejected(0, pe.Slug(), pe.Detail)
	}
	return pe
}

// recordFQDN is the FQDN of a record ID, "<fqdn>~A" (FR-20); A is the only
// record type, so any other ID is invalid.
func recordFQDN(name string) (string, error) {
	if len(name) > spec.MaxRecordNameLength || !recordRE.MatchString(name) {
		return "", probErr(invalid("/name", "must match "+spec.RecordNamePattern))
	}
	return strings.TrimSuffix(name, spec.RecordIDSeparator+spec.RecordTypeA), nil
}

// admitDNS is the shared DNS Admit, run in nexusd before an operation
// exists: the backend can write the DNS file, dnsmasq loads it (checked
// live, never from the cached dnsWrites detail, A-53), ifMatch is the file's
// etag, and the model accepts the changes in order; refuse maps the first
// refusal. The helper's pipeline checks all of it again under the lane.
func (p *Provider) admitDNS(ctx context.Context, ifMatch string, refuse refusal, changes ...dnsfile.Change) error {
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
	for i, c := range changes {
		if err := m.Apply(c); err != nil {
			return probErr(refuse(i, c, err))
		}
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

func runAddRecord(ctx context.Context, c *helperCall) (any, error) {
	var in spec.RecordAddInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the add-record input: %v; nothing was changed", err)))
	}
	return applyDNS(ctx, c, in.IfMatch, dnsfile.AddRecord{FQDN: in.FQDN, IPv4: in.IPv4})
}

// runUpdateRecord rewrites the record's one line in place: the pipeline
// renders the whole file and swaps it in one rename, so the name is never
// absent from it (FR-21).
func runUpdateRecord(ctx context.Context, c *helperCall) (any, error) {
	var in spec.RecordUpdateInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the records.update input: %v; nothing was changed", err)))
	}
	fqdn, err := recordFQDN(in.Name)
	if err != nil {
		return nil, err
	}
	return applyDNS(ctx, c, in.IfMatch, dnsfile.UpdateRecord{FQDN: fqdn, IPv4: in.IPv4})
}

func runRemoveRecord(ctx context.Context, c *helperCall) (any, error) {
	var in spec.RecordRemoveInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the records.remove input: %v; nothing was changed", err)))
	}
	fqdn, err := recordFQDN(in.Name)
	if err != nil {
		return nil, err
	}
	return applyDNS(ctx, c, in.IfMatch, dnsfile.RemoveRecord{FQDN: fqdn})
}

// runApplyDNSChanges applies the whole batch in one dnswrite.Apply call:
// one read and model of the file, one swap, at most one test and one
// restart, and one verification pass over every changed name, including
// the names a cascade removes (FR-26, NFR-2). A refusal writes nothing.
func runApplyDNSChanges(ctx context.Context, c *helperCall) (any, error) {
	var in spec.DNSBatchInput
	if err := json.Unmarshal(c.In.Input, &in); err != nil {
		return nil, probErr(problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole helper cannot decode the apply-dns-changes input: %v; nothing was changed", err)))
	}
	changes, pe := parseBatch(in.Changes)
	if pe != nil {
		return nil, probErr(pe)
	}
	res, pe := applyDNSChanges(ctx, c, in.IfMatch, changes)
	if len(changes) == 1 {
		pe = oneChangeBatchRefused(pe)
	}
	return res, probErr(pe)
}

// applyDNS runs the DNS write pipeline on the DNS file Prepare recorded.
// The pipeline's problem crosses into the error only through probErr, so a
// success is never a non-nil error holding a nil *problem.Error.
func applyDNS(ctx context.Context, c *helperCall, ifMatch string, changes ...dnsfile.Change) (any, error) {
	res, pe := applyDNSChanges(ctx, c, ifMatch, changes)
	return res, probErr(pe)
}

// applyDNSChanges is applyDNS before its problem becomes an error.
func applyDNSChanges(ctx context.Context, c *helperCall, ifMatch string,
	changes []dnsfile.Change) (spec.DNSChangeResult, *problem.Error) {
	if pe := dnsWritable(c.Paths, c.In.Backend, c.In.DNSFile); pe != nil {
		return spec.DNSChangeResult{}, pe
	}
	dm, ok := c.Mutator.(dnswrite.Mutator)
	if !ok {
		return spec.DNSChangeResult{}, problem.New(problem.InternalError,
			fmt.Sprintf("the pi-hole backend %q cannot write the DNS file; nothing was changed", c.In.Backend))
	}
	return dnswrite.Apply(ctx, dnswrite.Request{
		DNSFile: c.Paths.Host(c.In.DNSFile), StateDir: c.In.StateDir, OpDir: c.In.OpDir,
		IfMatch: ifMatch, Changes: changes,
	}, dnswrite.Deps{Mutator: dm, DNSServer: c.DNSServer, Out: c.Out, Now: c.Now})
}
