// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package pihole

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// MaxDNSFileBytes caps the DNS file the zone and record reads parse (A-10).
// A larger file answers dependency-unavailable; nothing of it is returned.
const MaxDNSFileBytes = 1 << 20

var (
	idRE     = regexp.MustCompile(spec.IDPattern)
	cursorRE = regexp.MustCompile(spec.CursorPattern)
	recordRE = regexp.MustCompile(spec.RecordNamePattern)
	zoneRE   = regexp.MustCompile(spec.FQDNPattern)
)

// readerBox holds the Reader the last preflight selected.
type readerBox struct{ r backend.Reader }

// backendReader is the Reader the last preflight selected. Without one (the
// T0 Preflight override), it detects the backend now; an undetected
// version answers capability-unavailable.
func (p *Provider) backendReader(ctx context.Context) (backend.Reader, error) {
	if b := p.reader.Load(); b != nil {
		return b.r, nil
	}
	v := backend.Detect(ctx, p.paths, ftlVersionTimeout)
	r, _, _, err := backend.Select(v, p.paths, p.deps())
	if err != nil {
		return nil, probErr(problem.New(problem.CapabilityUnavailable, err.Error()).
			With("provider", spec.Name).With("reason", err.Error()))
	}
	return r, nil
}

func (p *Provider) deps() backend.Deps {
	return backend.Deps{Runner: p.runner, DNSServer: p.dnsServer}
}

// readerFailed maps a Reader error: a problem, such as the unsupported
// backend's capability-unavailable, passes through; anything else is
// dependency-unavailable{provider}, with the cause in the log only.
func (p *Provider) readerFailed(what string, err error) error {
	var pe *problem.Error
	if errors.As(err, &pe) {
		return pe
	}
	p.log.Warn("pihole read failed", "read", what, "error", err.Error())
	detail := fmt.Sprintf("the pi-hole %s read failed", what)
	switch {
	case errors.Is(err, gravitydb.ErrUnavailable):
		detail = "gravity.db is unavailable: missing, unreadable, locked or being replaced"
	case errors.Is(err, gravitydb.ErrInvalid):
		detail = "gravity.db is not a usable Pi-hole database"
	}
	return problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name)
}

func dependencyUnavailable(format string, args ...any) *problem.Error {
	return problem.New(problem.DependencyUnavailable, fmt.Sprintf(format, args...)).With("provider", spec.Name)
}

func invalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, "the input is invalid").
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}

func notFound(detail string) *problem.Error { return problem.New(problem.NotFound, detail) }

// singleton is the one-item list of a status, summary or blocking read.
func singleton[P any](host, resourceType string, props P) apitypes.List[apitypes.Resource[P]] {
	return apitypes.List[apitypes.Resource[P]]{
		Items: []apitypes.Resource[P]{apitypes.NewResource(host, spec.Name, resourceType, spec.SingletonName, props)},
	}
}

func (p *Provider) status(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Status], error) {
	r, err := p.backendReader(ctx)
	if err != nil {
		return apitypes.List[spec.Status]{}, err
	}
	st, err := r.Status(ctx)
	if err != nil {
		return apitypes.List[spec.Status]{}, p.readerFailed("status", err)
	}
	return singleton(p.host, spec.StatusResourceType, st), nil
}

func (p *Provider) summary(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Summary], error) {
	r, err := p.backendReader(ctx)
	if err != nil {
		return apitypes.List[spec.Summary]{}, err
	}
	sum, err := r.Summary(ctx)
	if err != nil {
		return apitypes.List[spec.Summary]{}, p.readerFailed("summary", err)
	}
	return singleton(p.host, spec.SummaryResourceType, sum), nil
}

func (p *Provider) blocking(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.Blocking], error) {
	r, err := p.backendReader(ctx)
	if err != nil {
		return apitypes.List[spec.Blocking]{}, err
	}
	b, err := r.Blocking(ctx)
	if err != nil {
		return apitypes.List[spec.Blocking]{}, p.readerFailed("blocking", err)
	}
	return singleton(p.host, spec.BlockingResourceType, b), nil
}

// domainFilter is the parsed domain-list filter: within a key any value
// matches, and both keys must match. An absent key matches everything.
type domainFilter struct{ lists, kinds []string }

func parseDomainFilter(filters []string) (domainFilter, error) {
	var f domainFilter
	for i, s := range filters {
		k, v, _ := strings.Cut(s, "=")
		switch {
		case k == spec.FilterList && (v == spec.ListAllow || v == spec.ListDeny):
			f.lists = append(f.lists, v)
		case k == spec.FilterKind && (v == spec.KindExact || v == spec.KindRegex):
			f.kinds = append(f.kinds, v)
		default:
			return f, invalid(fmt.Sprintf("/filter/%d", i), "must match "+spec.DomainFilterPattern)
		}
	}
	return f, nil
}

func (f domainFilter) match(d spec.DomainProperties) bool {
	return (len(f.lists) == 0 || slices.Contains(f.lists, d.List)) &&
		(len(f.kinds) == 0 || slices.Contains(f.kinds, d.Kind))
}

func (p *Provider) listDomains(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Domain], error) {
	var none apitypes.List[spec.Domain]
	f, err := parseDomainFilter(in.Filter)
	if err != nil {
		return none, err
	}
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	rows, err := p.domains(ctx)
	if err != nil {
		return none, err
	}
	rows = slices.DeleteFunc(rows, func(d spec.DomainProperties) bool { return !f.match(d) })
	page, next := paginate(rows, after, in.Limit, func(d spec.DomainProperties) int64 { return d.PiholeID })
	out := apitypes.List[spec.Domain]{Items: make([]spec.Domain, len(page)), NextCursor: next}
	for i, d := range page {
		out.Items[i] = p.domainResource(d)
	}
	return out, nil
}

func (p *Provider) getDomain(ctx context.Context, in spec.NameInput) (spec.Domain, error) {
	id, err := parseID(in.Name)
	if err != nil {
		return spec.Domain{}, err
	}
	rows, err := p.domains(ctx)
	if err != nil {
		return spec.Domain{}, err
	}
	i := slices.IndexFunc(rows, func(d spec.DomainProperties) bool { return d.PiholeID == id })
	if i < 0 {
		return spec.Domain{}, notFound("no pi-hole domain entry has that ID")
	}
	return p.domainResource(rows[i]), nil
}

func (p *Provider) domains(ctx context.Context) ([]spec.DomainProperties, error) {
	r, err := p.backendReader(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.Domains(ctx)
	if err != nil {
		return nil, p.readerFailed("domains", err)
	}
	return rows, nil
}

func (p *Provider) domainResource(d spec.DomainProperties) spec.Domain {
	return apitypes.NewResource(p.host, spec.Name, spec.DomainResourceType, strconv.FormatInt(d.PiholeID, 10), d)
}

func (p *Provider) listAdlists(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Adlist], error) {
	var none apitypes.List[spec.Adlist]
	if len(in.Filter) > 0 {
		return none, invalid("/filter", "the adlist list takes no filter")
	}
	after, err := decodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	rows, err := p.adlists(ctx)
	if err != nil {
		return none, err
	}
	page, next := paginate(rows, after, in.Limit, func(a spec.AdlistProperties) int64 { return a.PiholeID })
	out := apitypes.List[spec.Adlist]{Items: make([]spec.Adlist, len(page)), NextCursor: next}
	for i, a := range page {
		out.Items[i] = p.adlistResource(a)
	}
	return out, nil
}

func (p *Provider) getAdlist(ctx context.Context, in spec.NameInput) (spec.Adlist, error) {
	id, err := parseID(in.Name)
	if err != nil {
		return spec.Adlist{}, err
	}
	rows, err := p.adlists(ctx)
	if err != nil {
		return spec.Adlist{}, err
	}
	i := slices.IndexFunc(rows, func(a spec.AdlistProperties) bool { return a.PiholeID == id })
	if i < 0 {
		return spec.Adlist{}, notFound("no pi-hole adlist has that ID")
	}
	return p.adlistResource(rows[i]), nil
}

func (p *Provider) adlists(ctx context.Context) ([]spec.AdlistProperties, error) {
	r, err := p.backendReader(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.Adlists(ctx)
	if err != nil {
		return nil, p.readerFailed("adlists", err)
	}
	return rows, nil
}

func (p *Provider) adlistResource(a spec.AdlistProperties) spec.Adlist {
	return apitypes.NewResource(p.host, spec.Name, spec.AdlistResourceType, strconv.FormatInt(a.PiholeID, 10), a)
}

func parseID(name string) (int64, error) {
	id, err := strconv.ParseInt(name, 10, 64)
	if !idRE.MatchString(name) || err != nil {
		return 0, invalid("/name", "must match "+spec.IDPattern)
	}
	return id, nil
}

// paginate returns the rows, ordered by id, after the id after, up to limit
// of them (spec.DefaultLimit when zero), and the cursor of the next page:
// base64url of the page's last id, as apt encodes its last name.
func paginate[T any](rows []T, after int64, limit int, id func(T) int64) ([]T, string) {
	if limit <= 0 {
		limit = spec.DefaultLimit
	}
	start := slices.IndexFunc(rows, func(r T) bool { return id(r) > after })
	if start < 0 {
		start = len(rows)
	}
	end := min(start+limit, len(rows))
	page := rows[start:end]
	next := ""
	if end < len(rows) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id(page[len(page)-1]), 10)))
	}
	return page, next
}

// decodeCursor returns the id a cursor this server issued names; 0 for
// none.
func decodeCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !cursorRE.MatchString(c) || !idRE.Match(b) {
		return 0, invalid("/cursor", "is not a cursor this server issued")
	}
	id, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, invalid("/cursor", "is not a cursor this server issued")
	}
	return id, nil
}

// dnsModel reads the DNS file in force (FR-2) and parses it. Any content
// parses: what is not a record or a marker is unmanaged (NFR-5). A file
// that is not a regular file, cannot be read or is larger than
// MaxDNSFileBytes answers dependency-unavailable, with nothing of it.
func (p *Provider) dnsModel() (dnsfile.Model, error) {
	name := p.Settings().DNSFile
	b, err := readCapped(p.paths.Host(name), MaxDNSFileBytes)
	switch {
	case errors.Is(err, errTooLarge):
		return dnsfile.Model{}, dependencyUnavailable("providers.pihole.dnsFile %s is larger than 1 MiB", name)
	case err != nil:
		p.log.Warn("pihole DNS file read failed", "dnsFile", name, "error", err.Error())
		return dnsfile.Model{}, dependencyUnavailable("providers.pihole.dnsFile %s cannot be read", name)
	}
	return dnsfile.Parse(b).Model(), nil
}

var errTooLarge = errors.New("file too large")

// readCapped reads a regular file of at most limit bytes. It is not
// followed through a symlink and never opened when it is not a regular
// file, so a FIFO cannot block the read.
func readCapped(path string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w: not a regular file", path, fs.ErrInvalid)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

func (p *Provider) zoneResource(m dnsfile.Model, zone string) spec.Zone {
	n := 0
	for _, r := range m.Records {
		if r.Zone == zone {
			n++
		}
	}
	return apitypes.NewResource(p.host, spec.Name, spec.ZoneResourceType, zone,
		spec.ZoneProperties{Zone: zone, Records: n, Etag: m.Etag})
}

func (p *Provider) listZones(context.Context, spec.NoInput) (apitypes.List[spec.Zone], error) {
	m, err := p.dnsModel()
	if err != nil {
		return apitypes.List[spec.Zone]{}, err
	}
	out := apitypes.List[spec.Zone]{Items: make([]spec.Zone, len(m.Zones))}
	for i, z := range m.Zones { // Model.Zones is sorted
		out.Items[i] = p.zoneResource(m, z)
	}
	return out, nil
}

func (p *Provider) getZone(_ context.Context, in spec.NameInput) (spec.Zone, error) {
	if !zoneRE.MatchString(in.Name) || len(in.Name) > spec.MaxFQDNLength {
		return spec.Zone{}, invalid("/name", "must match "+spec.FQDNPattern)
	}
	m, err := p.dnsModel()
	if err != nil {
		return spec.Zone{}, err
	}
	if !slices.Contains(m.Zones, in.Name) {
		return spec.Zone{}, notFound("no DNS zone with that name is declared")
	}
	return p.zoneResource(m, in.Name), nil
}

func (p *Provider) recordResource(m dnsfile.Model, r dnsfile.Record) spec.Record {
	props := spec.RecordProperties{FQDN: r.FQDN, Type: r.Type, IPv4: r.IPv4, Duplicate: r.Duplicate, Etag: m.Etag}
	if r.Zone != "" {
		props.Zone = &r.Zone
	}
	return apitypes.NewResource(p.host, spec.Name, spec.RecordResourceType, r.ID, props)
}

// listRecords lists every record line, duplicates included, in file order.
// Each zone filter selects the records of that zone, or of none for
// zone=unzoned; repeated, any of them matches.
func (p *Provider) listRecords(_ context.Context, in spec.RecordListInput) (apitypes.List[spec.Record], error) {
	var none apitypes.List[spec.Record]
	var zones []string
	for i, s := range in.Filter {
		k, v, _ := strings.Cut(s, "=")
		if k != spec.FilterZone || (v != spec.Unzoned && (!zoneRE.MatchString(v) || len(v) > spec.MaxFQDNLength)) {
			return none, invalid(fmt.Sprintf("/filter/%d", i), "must match "+spec.RecordFilterPattern)
		}
		if v == spec.Unzoned {
			v = ""
		}
		zones = append(zones, v)
	}
	m, err := p.dnsModel()
	if err != nil {
		return none, err
	}
	out := apitypes.List[spec.Record]{Items: []spec.Record{}}
	for _, r := range m.Records {
		if len(zones) == 0 || slices.Contains(zones, r.Zone) {
			out.Items = append(out.Items, p.recordResource(m, r))
		}
	}
	return out, nil
}

// getRecord returns the record by ID; for a duplicate name, the first line,
// marked duplicate (FR-20).
func (p *Provider) getRecord(_ context.Context, in spec.NameInput) (spec.Record, error) {
	if !recordRE.MatchString(in.Name) || len(in.Name) > spec.MaxRecordNameLength {
		return spec.Record{}, invalid("/name", "must match "+spec.RecordNamePattern)
	}
	m, err := p.dnsModel()
	if err != nil {
		return spec.Record{}, err
	}
	i := slices.IndexFunc(m.Records, func(r dnsfile.Record) bool { return r.ID == in.Name })
	if i < 0 {
		return spec.Record{}, notFound("no local A record has that ID")
	}
	return p.recordResource(m, m.Records[i]), nil
}
