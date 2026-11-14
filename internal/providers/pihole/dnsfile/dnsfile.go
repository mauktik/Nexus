// Package dnsfile is the model of Pi-hole's local DNS records file,
// /etc/dnsmasq.d/03-custom-dns-names.conf: parsing, zone membership, the
// batch changes, rendering and the answers dnsmasq is expected to give. It is
// the single source of DNS-file semantics and does no I/O.
//
// A line is a record ("address=/<fqdn>/<ipv4>"), a zone marker
// ("# nexus:zone <fqdn>"), the unzoned marker ("# nexus:unzoned") or
// unmanaged content, which is preserved byte for byte and never interpreted
// (FR-16). Zones exist only where a marker declares them (ASM-10), and a
// record's zone follows from its name, never from where its line sits
// (FR-17). The renderer adds nothing but the two marker forms (CON-6).
package dnsfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/mauktik/nexus/internal/providers/pihole/spec"
)

const (
	recordPrefix   = "address=/"
	zoneMarker     = "# nexus:zone "
	unzonedMarker  = "# nexus:unzoned"
	recordTypeA    = "A"
	recordIDSuffix = "~A"
)

var (
	fqdnRE   = regexp.MustCompile(spec.FQDNPattern)
	ipv4RE   = regexp.MustCompile(spec.IPv4Pattern)
	changeRE = regexp.MustCompile(spec.ChangePattern)
)

// Typed errors of Apply (FR-19, FR-21, ASM-11).
var (
	ErrZoneExists     = errors.New("zone already declared")
	ErrZoneNotFound   = errors.New("zone not declared")
	ErrUnzoned        = errors.New("record name is under no declared zone")
	ErrRecordExists   = errors.New("record already exists")
	ErrRecordNotFound = errors.New("record not found")
	ErrDuplicate      = errors.New("record name is on more than one line")
	// ErrInvalidChange is a change whose name or address the input patterns
	// refuse. Apply checks it too, so no caller can make the renderer emit
	// a line other than a record or a marker (CON-6).
	ErrInvalidChange = errors.New("invalid change")
)

// ErrZoneNotEmpty refuses removing a zone that still holds records of its own
// without Cascade.
type ErrZoneNotEmpty struct{ Records int }

func (e ErrZoneNotEmpty) Error() string {
	return fmt.Sprintf("zone holds %d records; remove them or cascade", e.Records)
}

// Record is one record line. Zone is the longest declared zone suffix of
// FQDN, "" when unzoned.
type Record struct {
	ID, FQDN, Type, IPv4, Zone string
	// Duplicate is set on every record whose (FQDN, A) pair is on more than
	// one line (FR-20); such records are immutable (FR-21).
	Duplicate bool
}

// Model is the file's managed content. Records keep file order, with added
// records last.
type Model struct {
	Zones     []string // Declared zones, distinct and sorted
	Records   []Record
	Unmanaged int    // Unmanaged lines of the parsed file
	Etag      string // "sha256:" + hex of the parsed bytes

	// detached are the zones RemoveZone removed: their sections' unmanaged
	// lines render at the end of the preamble (A-44).
	detached []string
}

type lineKind int

const (
	kindUnmanaged lineKind = iota
	kindRecord
	kindZone
	kindUnzoned
)

type line struct {
	kind lineKind
	text string
	name string // the record's FQDN, or the zone marker's zone
	ip   string
}

type sectionKind int

const (
	sectionPreamble sectionKind = iota
	sectionZone
	sectionUnzoned
)

type section struct {
	kind  sectionKind
	zone  string
	lines []line // records and unmanaged lines, trailing blanks dropped
}

// File is a parsed DNS file: its lines grouped by the section they sit in.
type File struct {
	etag     string
	sections []section
}

// Parse reads a DNS file. Lines are split on "\n" only, so a line ending
// in "\r" is unmanaged. Trailing blank lines of a section are layout and
// are dropped; every other blank line is unmanaged content (A-43).
func Parse(b []byte) *File {
	sum := sha256.Sum256(b)
	f := &File{etag: "sha256:" + hex.EncodeToString(sum[:])}
	s := strings.TrimSuffix(string(b), "\n")
	cur := section{kind: sectionPreamble}
	if len(b) > 0 {
		for _, text := range strings.Split(s, "\n") {
			l := classify(text)
			switch l.kind {
			case kindZone:
				f.sections = append(f.sections, trimSection(cur))
				cur = section{kind: sectionZone, zone: l.name}
			case kindUnzoned:
				f.sections = append(f.sections, trimSection(cur))
				cur = section{kind: sectionUnzoned}
			default:
				cur.lines = append(cur.lines, l)
			}
		}
	}
	f.sections = append(f.sections, trimSection(cur))
	return f
}

func trimSection(s section) section {
	n := len(s.lines)
	for n > 0 && s.lines[n-1].kind == kindUnmanaged && s.lines[n-1].text == "" {
		n--
	}
	s.lines = s.lines[:n:n]
	return s
}

func classify(text string) line {
	if rest, ok := strings.CutPrefix(text, recordPrefix); ok {
		if i := strings.LastIndexByte(rest, '/'); i >= 0 {
			name, ip := rest[:i], rest[i+1:]
			if validName(name) && validIPv4(ip) {
				return line{kind: kindRecord, text: text, name: name, ip: ip}
			}
		}
	}
	if zone, ok := strings.CutPrefix(text, zoneMarker); ok && validName(zone) {
		return line{kind: kindZone, text: text, name: zone}
	}
	if text == unzonedMarker {
		return line{kind: kindUnzoned, text: text}
	}
	return line{kind: kindUnmanaged, text: text}
}

func validName(s string) bool { return len(s) <= spec.MaxFQDNLength && fqdnRE.MatchString(s) }

func validIPv4(s string) bool {
	return ipv4RE.MatchString(s) && !slices.Contains(spec.BlockedIPv4s, s)
}

// Model returns the file's zones, records and unmanaged-line count.
func (f *File) Model() Model {
	m := Model{Etag: f.etag}
	for _, s := range f.sections {
		if s.kind == sectionZone && !slices.Contains(m.Zones, s.zone) {
			m.Zones = append(m.Zones, s.zone)
		}
		for _, l := range s.lines {
			if l.kind == kindRecord {
				m.Records = append(m.Records, newRecord(l.name, l.ip))
			} else {
				m.Unmanaged++
			}
		}
	}
	sort.Strings(m.Zones)
	count := map[string]int{}
	for _, r := range m.Records {
		count[r.FQDN]++
	}
	for i := range m.Records {
		m.Records[i].Duplicate = count[m.Records[i].FQDN] > 1
	}
	m.assignZones()
	return m
}

func newRecord(fqdn, ip string) Record {
	return Record{ID: fqdn + recordIDSuffix, FQDN: fqdn, Type: recordTypeA, IPv4: ip}
}

// zoneOf is the declared zone that is the longest suffix of name on label
// boundaries, an exact match included; "" when there is none (FR-17).
func zoneOf(zones []string, name string) string {
	best := ""
	for _, z := range zones {
		if len(z) > len(best) && underSuffix(name, z) {
			best = z
		}
	}
	return best
}

// underSuffix reports whether name is suffix or a name under it.
func underSuffix(name, suffix string) bool {
	return name == suffix || strings.HasSuffix(name, "."+suffix)
}

func (m *Model) assignZones() {
	for i := range m.Records {
		m.Records[i].Zone = zoneOf(m.Zones, m.Records[i].FQDN)
	}
}

// Change is one batch change (FR-26): AddZone, RemoveZone, AddRecord,
// UpdateRecord or RemoveRecord.
type Change interface {
	apply(m *Model) error
}

// AddZone declares a zone.
type AddZone struct{ Zone string }

// RemoveZone removes a zone declaration. Without Cascade it refuses a zone
// that still holds records of its own; with it, those records are deleted.
type RemoveZone struct {
	Zone    string
	Cascade bool
}

// AddRecord adds a record under a declared zone.
type AddRecord struct{ FQDN, IPv4 string }

// UpdateRecord changes a record's address; the record may be unzoned.
type UpdateRecord struct{ FQDN, IPv4 string }

// RemoveRecord deletes a record; the record may be unzoned.
type RemoveRecord struct{ FQDN string }

// Apply applies one change, or returns a typed error and leaves m as it
// was. It never writes through to a slice another copy of m shares.
func (m *Model) Apply(c Change) error {
	if c == nil {
		return fmt.Errorf("%w: no change", ErrInvalidChange)
	}
	return c.apply(m)
}

func (c AddZone) apply(m *Model) error {
	if !validName(c.Zone) {
		return fmt.Errorf("%w: zone %q", ErrInvalidChange, c.Zone)
	}
	if slices.Contains(m.Zones, c.Zone) {
		return fmt.Errorf("%w: %s", ErrZoneExists, c.Zone)
	}
	zones := append(slices.Clone(m.Zones), c.Zone)
	sort.Strings(zones)
	m.Zones, m.Records = zones, slices.Clone(m.Records)
	m.assignZones()
	return nil
}

func (c RemoveZone) apply(m *Model) error {
	if !slices.Contains(m.Zones, c.Zone) {
		return fmt.Errorf("%w: %s", ErrZoneNotFound, c.Zone)
	}
	var own int
	for _, r := range m.Records {
		if r.Zone != c.Zone {
			continue
		}
		own++
		if c.Cascade && r.Duplicate {
			return fmt.Errorf("%w: %s", ErrDuplicate, r.FQDN)
		}
	}
	if own > 0 && !c.Cascade {
		return ErrZoneNotEmpty{Records: own}
	}
	records := make([]Record, 0, len(m.Records)-own)
	for _, r := range m.Records {
		if r.Zone != c.Zone {
			records = append(records, r)
		}
	}
	m.Zones = slices.DeleteFunc(slices.Clone(m.Zones), func(z string) bool { return z == c.Zone })
	m.Records = records
	if !slices.Contains(m.detached, c.Zone) {
		m.detached = append(slices.Clone(m.detached), c.Zone)
	}
	m.assignZones()
	return nil
}

func (c AddRecord) apply(m *Model) error {
	if !validName(c.FQDN) || !validIPv4(c.IPv4) {
		return fmt.Errorf("%w: record %q=%q", ErrInvalidChange, c.FQDN, c.IPv4)
	}
	if m.find(c.FQDN) >= 0 {
		return fmt.Errorf("%w: %s", ErrRecordExists, c.FQDN)
	}
	zone := zoneOf(m.Zones, c.FQDN)
	if zone == "" {
		return fmt.Errorf("%w: %s", ErrUnzoned, c.FQDN)
	}
	r := newRecord(c.FQDN, c.IPv4)
	r.Zone = zone
	m.Records = append(slices.Clone(m.Records), r)
	return nil
}

func (c UpdateRecord) apply(m *Model) error {
	if !validName(c.FQDN) || !validIPv4(c.IPv4) {
		return fmt.Errorf("%w: record %q=%q", ErrInvalidChange, c.FQDN, c.IPv4)
	}
	i, err := m.mutable(c.FQDN)
	if err != nil {
		return err
	}
	m.Records = slices.Clone(m.Records)
	m.Records[i].IPv4 = c.IPv4
	return nil
}

func (c RemoveRecord) apply(m *Model) error {
	if !validName(c.FQDN) {
		return fmt.Errorf("%w: record %q", ErrInvalidChange, c.FQDN)
	}
	i, err := m.mutable(c.FQDN)
	if err != nil {
		return err
	}
	m.Records = slices.Delete(slices.Clone(m.Records), i, i+1)
	return nil
}

func (m *Model) find(fqdn string) int {
	return slices.IndexFunc(m.Records, func(r Record) bool { return r.FQDN == fqdn })
}

// mutable returns the index of fqdn's one record, refusing a missing or
// duplicate name (FR-21).
func (m *Model) mutable(fqdn string) (int, error) {
	i := m.find(fqdn)
	if i < 0 {
		return 0, fmt.Errorf("%w: %s", ErrRecordNotFound, fqdn)
	}
	if m.Records[i].Duplicate {
		return 0, fmt.Errorf("%w: %s", ErrDuplicate, fqdn)
	}
	return i, nil
}

// ParseChange reads one item of the batch grammar (FR-26, A-36):
// "<verb>:<fqdn>" for add-zone, remove-zone, remove-zone-cascade and
// remove-record, and "<verb>:<fqdn>=<ipv4>" for add-record and
// update-record.
func ParseChange(s string) (Change, error) {
	if len(s) > spec.MaxChangeLength || !changeRE.MatchString(s) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidChange, s)
	}
	verb, arg, _ := strings.Cut(s, ":")
	name, ip, _ := strings.Cut(arg, "=")
	if !validName(name) || (ip != "" && !validIPv4(ip)) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidChange, s)
	}
	switch verb {
	case spec.ChangeAddZone:
		return AddZone{Zone: name}, nil
	case spec.ChangeRemoveZone:
		return RemoveZone{Zone: name}, nil
	case spec.ChangeRemoveZoneCascade:
		return RemoveZone{Zone: name, Cascade: true}, nil
	case spec.ChangeAddRecord:
		return AddRecord{FQDN: name, IPv4: ip}, nil
	case spec.ChangeUpdateRecord:
		return UpdateRecord{FQDN: name, IPv4: ip}, nil
	case spec.ChangeRemoveRecord:
		return RemoveRecord{FQDN: name}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrInvalidChange, s)
}
