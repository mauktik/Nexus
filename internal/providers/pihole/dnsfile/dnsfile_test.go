// Stub-style: RED-by-design (WI 1.5). This file fails to compile until
// internal/providers/pihole/dnsfile exists; it is GREEN when WI 1.5 lands.
// Cases: TC-15 to TC-31, TC-E5, TC-E6 in planning/pihole-dns-zones/test-cases.md.
// The symbol names follow the "dnsfile" row of the test-seam contract in
// test-strategy.md; the owning developer may rename a symbol to the one
// implemented, but must not weaken an assertion.
//
// Fixtures: testdata/fr23-seven-lines*.conf are verbatim from requirements.md
// FR-23; testdata/live-53-lines.conf is the read-only capture of the user's
// live file (see testdata/PROVENANCE). Expected values are computed from the
// fixture bytes with the rule the requirement states, never typed in twice.
package dnsfile_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func model(t *testing.T, b []byte) (*dnsfile.File, dnsfile.Model) {
	t.Helper()
	f := dnsfile.Parse(b)
	return f, f.Model()
}

func apply(t *testing.T, m *dnsfile.Model, cs ...dnsfile.Change) {
	t.Helper()
	for _, c := range cs {
		if err := m.Apply(c); err != nil {
			t.Fatalf("Apply(%#v): %v", c, err)
		}
	}
}

var fr23Zones = []dnsfile.Change{
	dnsfile.AddZone{Zone: "home.local"}, dnsfile.AddZone{Zone: "app.lan"}, dnsfile.AddZone{Zone: "xi.example"},
}

// membership returns zone -> sorted record names; "" is unzoned.
func membership(m dnsfile.Model) map[string][]string {
	out := map[string][]string{}
	for _, r := range m.Records {
		out[r.Zone] = append(out[r.Zone], r.FQDN)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// TC-15: with no zones declared, the FR-23 fixture parses to 7 unzoned A
// records and 0 unmanaged lines.
func TestTC15_ParseFR23WithoutZones(t *testing.T) {
	_, m := model(t, read(t, "fr23-seven-lines.conf"))
	if len(m.Records) != 7 || m.Unmanaged != 0 || len(m.Zones) != 0 {
		t.Fatalf("records=%d unmanaged=%d zones=%v, want 7, 0, none", len(m.Records), m.Unmanaged, m.Zones)
	}
	for _, r := range m.Records {
		if r.Type != "A" || r.Zone != "" || r.Duplicate {
			t.Errorf("record %+v: want type A, unzoned, not duplicate", r)
		}
	}
}

// TC-16: membership after declaring the three implied zones is exactly the
// FR-23 table.
func TestTC16_MembershipFR23(t *testing.T) {
	_, m := model(t, read(t, "fr23-seven-lines.conf"))
	apply(t, &m, fr23Zones...)
	want := map[string][]string{
		"home.local": {"kiwi.home.local", "photon.home.local"},
		"app.lan":    {"photon.app.lan"},
		"xi.example":   {"photos.xi.example", "router.xi.example"},
		"":           {"unity.lan", "unity.local"},
	}
	if got := membership(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("membership\n got %v\nwant %v", got, want)
	}
	if !slices.Equal(m.Zones, []string{"app.lan", "home.local", "xi.example"}) {
		t.Errorf("zones %v, want sorted app.lan, home.local, xi.example", m.Zones)
	}
}

// TC-17: the rendering is byte-identical to the FR-23 rendering.
func TestTC17_RenderFR23Exact(t *testing.T) {
	f, m := model(t, read(t, "fr23-seven-lines.conf"))
	apply(t, &m, fr23Zones...)
	got, want := dnsfile.Render(f, m), read(t, "fr23-seven-lines.rendered.conf")
	if !bytes.Equal(got, want) {
		t.Fatalf("render\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func rerender(b []byte) []byte {
	f := dnsfile.Parse(b)
	return dnsfile.Render(f, f.Model())
}

// TC-18: rendering an already-rendered file is byte-identical.
func TestTC18_RenderIdempotent(t *testing.T) {
	for _, name := range []string{"fr23-seven-lines.conf", "fr23-seven-lines.rendered.conf"} {
		f, m := model(t, read(t, name))
		if name == "fr23-seven-lines.conf" {
			apply(t, &m, fr23Zones...)
		}
		r1 := dnsfile.Render(f, m)
		if r2 := rerender(r1); !bytes.Equal(r1, r2) {
			t.Errorf("%s: render(parse(render)) differs\n--- r1 ---\n%s--- r2 ---\n%s", name, r1, r2)
		}
	}
	if r := rerender(tc22Input); !bytes.Equal(r, rerender(r)) {
		t.Errorf("TC-22 input: not idempotent")
	}
}

// FuzzTC18 (TC-18, TC-29 property): for any input, rendering is idempotent,
// keeps the multiset of record lines, and never emits a non-blank line that
// was not already in the input, except exactly "# nexus:unzoned", which FR-23
// requires whenever a record is unzoned (TC-28, TC-E6). Zone markers appear
// only if the input had them: the fuzz path declares no zones. So the
// renderer can never invent local=/server= or any other line (CON-6).
func FuzzTC18_RenderIdempotent(f *testing.F) {
	for _, s := range [][]byte{tc22Input, []byte("# nexus:zone a.b\naddress=/x.a.b/1.2.3.4\n\n\n# x\n"), []byte("")} {
		f.Add(s)
	}
	if b, err := os.ReadFile("testdata/fr23-seven-lines.rendered.conf"); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		r1 := rerender(in)
		if r2 := rerender(r1); !bytes.Equal(r1, r2) {
			t.Fatalf("not idempotent:\n%q\n%q", r1, r2)
		}
		inSet := map[string]bool{}
		for _, l := range strings.Split(string(in), "\n") {
			inSet[l] = true
		}
		for _, l := range lines(r1) {
			if l != "" && l != "# nexus:unzoned" && !inSet[l] {
				t.Fatalf("render invented line %q", l)
			}
		}
		if !dnsfile.DNSEquivalent(in, r1) {
			t.Fatalf("render changed DNS-relevant lines")
		}
	})
}

// TC-19: a record line moved by hand into another section keeps its
// name-derived membership, and the next render puts it back.
func TestTC19_HandMovedLineKeepsMembership(t *testing.T) {
	rendered := string(read(t, "fr23-seven-lines.rendered.conf"))
	moved := strings.Replace(rendered, "address=/unity.local/192.168.7.1\n", "", 1)
	moved = strings.Replace(moved, "# nexus:zone home.local\n", "# nexus:zone home.local\naddress=/unity.local/192.168.7.1\n", 1)
	moved = strings.Replace(moved, "address=/kiwi.home.local/192.168.7.5\n", "", 1)
	moved += "address=/kiwi.home.local/192.168.7.5\n" // into the unzoned section
	f, m := model(t, []byte(moved))
	got := membership(m)
	if !slices.Contains(got[""], "unity.local") || slices.Contains(got["home.local"], "unity.local") {
		t.Errorf("unity.local moved under home.local changed membership: %v", got)
	}
	if !slices.Contains(got["home.local"], "kiwi.home.local") {
		t.Errorf("kiwi.home.local moved into unzoned lost its zone: %v", got)
	}
	if r := dnsfile.Render(f, m); string(r) != rendered {
		t.Errorf("render after hand move\n%s\nwant\n%s", r, rendered)
	}
}

// TC-20: nested zones resolve by longest label-boundary suffix; a record named
// exactly like a zone belongs to it; a suffix that is not on a label boundary
// does not match.
func TestTC20_NestedZonesLongestMatch(t *testing.T) {
	in := "# nexus:zone home.local\n# nexus:zone a.home.local\n" +
		"address=/x.a.home.local/192.168.7.1\naddress=/a.home.local/192.168.7.2\n" +
		"address=/b.home.local/192.168.7.3\naddress=/home.local/192.168.7.4\naddress=/xhome.local/192.168.7.5\n"
	_, m := model(t, []byte(in))
	want := map[string][]string{
		"a.home.local": {"a.home.local", "x.a.home.local"},
		"home.local":   {"b.home.local", "home.local"},
		"":             {"xhome.local"},
	}
	if got := membership(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("membership %v, want %v", got, want)
	}
}

// TC-21: a (name, A) pair on more than one line marks every such record
// duplicate, and any mutation of it is refused.
func TestTC21_Duplicates(t *testing.T) {
	in := "address=/dup.home.local/192.168.7.1\naddress=/ok.home.local/192.168.7.2\naddress=/dup.home.local/192.168.7.9\n"
	_, m := model(t, []byte(in))
	n := 0
	for _, r := range m.Records {
		if r.FQDN == "dup.home.local" {
			n++
			if !r.Duplicate {
				t.Errorf("%+v not marked duplicate", r)
			}
		} else if r.Duplicate {
			t.Errorf("%+v marked duplicate", r)
		}
	}
	if n != 2 {
		t.Fatalf("listed %d dup.home.local records, want 2", n)
	}
	for _, c := range []dnsfile.Change{
		dnsfile.UpdateRecord{FQDN: "dup.home.local", IPv4: "192.168.7.3"},
		dnsfile.RemoveRecord{FQDN: "dup.home.local"},
	} {
		mm := m
		if err := mm.Apply(c); !errors.Is(err, dnsfile.ErrDuplicate) {
			t.Errorf("Apply(%#v) = %v, want ErrDuplicate", c, err)
		}
	}
}

// tc22Input: 20 unmanaged lines (every FR-16 "anything else" form plus A-9
// names) and 2 records, with no markers.
var tc22Unmanaged = []string{
	"# my comment",
	"",
	"# second comment",
	"server=/corp.example/10.0.0.1",
	"address=/a.lan/b.lan/192.168.7.9",
	"address=/ads.example/",
	"address=/ads2.example/#",
	"local=/corp.example/",
	"address=/UPPER.home.local/192.168.7.6",
	"address=/x.home.local/0.0.0.0",
	"address=/y.home.local/192.168.007.5",
	"address=/z.home.local/192.168.7.5  ",
	"address=/crlf.home.local/192.168.7.5\r",
	"address=/single/192.168.7.8",
	"address=/under_score.home.local/192.168.7.8",
	"address=/bcast.home.local/255.255.255.255",
	"address=/trail.home.local./192.168.7.5",
	"#nexus:zone foo.lan",
	"# nexus:zone Foo.LAN",
	" address=/lead.home.local/192.168.7.5",
}

var tc22Input = []byte(strings.Join(tc22Unmanaged[:15], "\n") + "\n" +
	"address=/kiwi.home.local/192.168.7.5\naddress=/photon.home.local/192.168.7.5\n" +
	strings.Join(tc22Unmanaged[15:], "\n") + "\n")

// TC-22: unmanaged lines are counted, never interpreted, and preserved byte
// for byte in their original order; the rendering is exactly preamble, then
// the zone section.
func TestTC22_UnmanagedPreserved(t *testing.T) {
	f, m := model(t, tc22Input)
	if m.Unmanaged != len(tc22Unmanaged) || len(m.Records) != 2 {
		t.Fatalf("unmanaged=%d records=%d, want %d and 2", m.Unmanaged, len(m.Records), len(tc22Unmanaged))
	}
	apply(t, &m, dnsfile.AddZone{Zone: "home.local"})
	want := strings.Join(tc22Unmanaged, "\n") + "\n\n# nexus:zone home.local\n" +
		"address=/kiwi.home.local/192.168.7.5\naddress=/photon.home.local/192.168.7.5\n"
	if got := string(dnsfile.Render(f, m)); got != want {
		t.Fatalf("render\n%q\nwant\n%q", got, want)
	}
}

// TC-23: Expected applies dnsmasq address= semantics: the longest matching
// suffix line wins and covers subdomains; a name no line covers is uncovered.
func TestTC23_Expected(t *testing.T) {
	_, m := model(t, read(t, "fr23-seven-lines.conf"))
	apply(t, &m, dnsfile.AddZone{Zone: "xi.example"}, dnsfile.AddRecord{FQDN: "xi.example", IPv4: "192.168.7.9"})
	cases := []struct {
		name    string
		ip      string
		covered bool
	}{
		{"kiwi.home.local", "192.168.7.5", true},
		{"deep.kiwi.home.local", "192.168.7.5", true},
		{"home.local", "", false},
		{"photos.xi.example", "192.168.7.7", true},
		{"a.photos.xi.example", "192.168.7.7", true},
		{"www.xi.example", "192.168.7.9", true},
		{"xi.example", "192.168.7.9", true},
		{"xunity.lan", "", false},
		{"example.com", "", false},
	}
	for _, c := range cases {
		ip, cov := dnsfile.Expected(m, c.name)
		if ip != c.ip || cov != c.covered {
			t.Errorf("Expected(%s) = %q,%v, want %q,%v", c.name, ip, cov, c.ip, c.covered)
		}
	}
}

// TC-24: DNS-equivalence compares only the multiset of non-comment,
// non-blank lines.
func TestTC24_DNSEquivalent(t *testing.T) {
	orig := read(t, "fr23-seven-lines.conf")
	rendered := read(t, "fr23-seven-lines.rendered.conf")
	if !dnsfile.DNSEquivalent(orig, rendered) {
		t.Error("reorganization only: want equivalent")
	}
	if !dnsfile.DNSEquivalent(orig, append([]byte("# a comment\n\n"), orig...)) {
		t.Error("added comment and blank: want equivalent")
	}
	changed := bytes.Replace(orig, []byte("192.168.7.7"), []byte("192.168.7.8"), 1)
	if dnsfile.DNSEquivalent(orig, changed) {
		t.Error("IP change: want not equivalent")
	}
	doubled := append(append([]byte{}, orig...), []byte("address=/unity.lan/192.168.7.1\n")...)
	if dnsfile.DNSEquivalent(orig, doubled) {
		t.Error("duplicated record line: multiset differs, want not equivalent")
	}
}

// TC-25: Apply's typed errors and success paths.
func TestTC25_ApplySemantics(t *testing.T) {
	base := func() dnsfile.Model {
		_, m := model(t, read(t, "fr23-seven-lines.conf"))
		apply(t, &m, fr23Zones...)
		return m
	}
	m := base()
	if err := m.Apply(dnsfile.AddZone{Zone: "app.lan"}); !errors.Is(err, dnsfile.ErrZoneExists) {
		t.Errorf("add existing zone = %v, want ErrZoneExists", err)
	}
	var ne dnsfile.ErrZoneNotEmpty
	if err := m.Apply(dnsfile.RemoveZone{Zone: "home.local"}); !errors.As(err, &ne) || ne.Records != 2 {
		t.Errorf("remove non-empty zone = %v, want ErrZoneNotEmpty{Records:2}", err)
	}
	if err := m.Apply(dnsfile.RemoveZone{Zone: "nope.lan"}); !errors.Is(err, dnsfile.ErrZoneNotFound) {
		t.Errorf("remove unknown zone = %v, want ErrZoneNotFound", err)
	}
	if err := m.Apply(dnsfile.AddRecord{FQDN: "new.example.org", IPv4: "192.168.7.9"}); !errors.Is(err, dnsfile.ErrUnzoned) {
		t.Errorf("add unzoned = %v, want ErrUnzoned", err)
	}
	if err := m.Apply(dnsfile.AddRecord{FQDN: "kiwi.home.local", IPv4: "192.168.7.9"}); !errors.Is(err, dnsfile.ErrRecordExists) {
		t.Errorf("add existing = %v, want ErrRecordExists", err)
	}
	if err := m.Apply(dnsfile.UpdateRecord{FQDN: "ghost.home.local", IPv4: "192.168.7.9"}); !errors.Is(err, dnsfile.ErrRecordNotFound) {
		t.Errorf("update unknown = %v, want ErrRecordNotFound", err)
	}
	if err := m.Apply(dnsfile.RemoveRecord{FQDN: "ghost.home.local"}); !errors.Is(err, dnsfile.ErrRecordNotFound) {
		t.Errorf("remove unknown = %v, want ErrRecordNotFound", err)
	}

	m = base()
	apply(t, &m,
		dnsfile.UpdateRecord{FQDN: "unity.local", IPv4: "192.168.7.2"}, // unzoned update works (FR-21)
		dnsfile.RemoveRecord{FQDN: "unity.lan"},                        // unzoned remove works
		dnsfile.AddRecord{FQDN: "nexus-test.app.lan", IPv4: "192.168.7.9"},
		dnsfile.RemoveZone{Zone: "xi.example", Cascade: true},
	)
	got := map[string]string{}
	for _, r := range m.Records {
		got[r.FQDN] = r.IPv4 + "|" + r.Zone
	}
	want := map[string]string{
		"unity.local": "192.168.7.2|", "photon.home.local": "192.168.7.5|home.local", "kiwi.home.local": "192.168.7.5|home.local",
		"photon.app.lan": "192.168.7.5|app.lan", "nexus-test.app.lan": "192.168.7.9|app.lan",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after changes %v, want %v", got, want)
	}
	if slices.Contains(m.Zones, "xi.example") {
		t.Errorf("cascade left the zone declared: %v", m.Zones)
	}
}

// TC-26: the batch grammar parses to the five change kinds, rejects anything
// else, and changes apply progressively to one model.
func TestTC26_ParseChangeAndProgressiveApply(t *testing.T) {
	good := map[string]dnsfile.Change{
		"add-zone:app.lan":                     dnsfile.AddZone{Zone: "app.lan"},
		"remove-zone:app.lan":                  dnsfile.RemoveZone{Zone: "app.lan"},
		"remove-zone-cascade:app.lan":          dnsfile.RemoveZone{Zone: "app.lan", Cascade: true},
		"add-record:x.app.lan=192.168.7.9":     dnsfile.AddRecord{FQDN: "x.app.lan", IPv4: "192.168.7.9"},
		"update-record:x.app.lan=192.168.7.10": dnsfile.UpdateRecord{FQDN: "x.app.lan", IPv4: "192.168.7.10"},
		"remove-record:x.app.lan":              dnsfile.RemoveRecord{FQDN: "x.app.lan"},
	}
	for s, want := range good {
		got, err := dnsfile.ParseChange(s)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseChange(%q) = %#v, %v; want %#v", s, got, err, want)
		}
	}
	for _, s := range []string{"", "add-zone:", "add-record:x.app.lan", "remove-record:x.app.lan=1.2.3.4", "frobnicate:a.b",
		"ADD-ZONE:a.b", "add-record:x.app.lan=0.0.0.0", "add-record:x.app.lan=255.255.255.255", "add-zone:a.b\nadd-zone:c.d", "add-zone:local"} {
		if c, err := dnsfile.ParseChange(s); err == nil {
			t.Errorf("ParseChange(%q) = %#v, want an error", s, c)
		}
	}

	_, m := model(t, read(t, "fr23-seven-lines.conf"))
	seq := []string{"add-zone:app.lan", "add-record:new.app.lan=192.168.7.9", "update-record:new.app.lan=192.168.7.10", "remove-zone:app.lan"}
	for i, s := range seq {
		c, err := dnsfile.ParseChange(s)
		if err != nil {
			t.Fatal(err)
		}
		err = m.Apply(c)
		if i < 3 && err != nil {
			t.Fatalf("change %d (%s): %v; it depends only on earlier changes", i, s, err)
		}
		var ne dnsfile.ErrZoneNotEmpty
		if i == 3 && (!errors.As(err, &ne) || ne.Records != 2) {
			t.Fatalf("change 3 = %v, want ErrZoneNotEmpty{2} (photon.app.lan and new.app.lan)", err)
		}
	}
}

// TC-27 (A-44): unmanaged lines inside a removed zone's section move to the
// end of the preamble and are never dropped.
func TestTC27_RemovedZoneUnmanagedMoveToPreamble(t *testing.T) {
	in := "# top\n\n# nexus:zone app.lan\naddress=/photon.app.lan/192.168.7.5\n# keep me\n\n" +
		"# nexus:zone home.local\naddress=/kiwi.home.local/192.168.7.5\n"
	f, m := model(t, []byte(in))
	apply(t, &m, dnsfile.RemoveZone{Zone: "app.lan", Cascade: true})
	want := "# top\n# keep me\n\n# nexus:zone home.local\naddress=/kiwi.home.local/192.168.7.5\n"
	if got := string(dnsfile.Render(f, m)); got != want {
		t.Fatalf("render\n%q\nwant\n%q", got, want)
	}
}

// TC-28 (A-43): trailing blank lines of a section are layout and normalize to
// one separator; inner blank lines are content; empty renders as 0 bytes; the
// output always ends with a newline.
func TestTC28_BlankLineLayout(t *testing.T) {
	rendered := read(t, "fr23-seven-lines.rendered.conf")
	padded := bytes.ReplaceAll(rendered, []byte("\n\n#"), []byte("\n\n\n\n#"))
	padded = append(padded, "\n\n\n"...)
	if got := rerender(padded); !bytes.Equal(got, rendered) {
		t.Errorf("extra trailing blanks not normalized:\n%q", got)
	}
	inner := "# nexus:zone app.lan\naddress=/photon.app.lan/192.168.7.5\n\n# note\n\n"
	if got, want := string(rerender([]byte(inner))), "# nexus:zone app.lan\naddress=/photon.app.lan/192.168.7.5\n\n# note\n"; got != want {
		t.Errorf("inner blank: got %q, want %q", got, want)
	}
	for _, in := range []string{"", "\n", "\n\n\n"} {
		if got := rerender([]byte(in)); len(got) != 0 {
			t.Errorf("render(%q) = %q, want 0 bytes", in, got)
		}
	}
	if got := rerender([]byte("address=/kiwi.home.local/192.168.7.5")); string(got) != "# nexus:unzoned\naddress=/kiwi.home.local/192.168.7.5\n" {
		t.Errorf("no trailing newline input rendered %q", got)
	}
}

var resolutionChanging = regexp.MustCompile(`(?m)^\s*(local|server|domain|rev-server|address=/[^/\n]*/(#)?$)`)

// TC-29 (regression: split-horizon xi.example, CON-6): declaring xi.example and
// adding a record under it never produces local=/server=/domain= or an
// NXDOMAIN-form address line, and a public name under xi.example stays
// uncovered (forwarded upstream).
func TestTC29_SplitHorizonNeverLocal(t *testing.T) {
	f, m := model(t, read(t, "fr23-seven-lines.conf"))
	apply(t, &m, dnsfile.AddZone{Zone: "xi.example"}, dnsfile.AddRecord{FQDN: "nas.xi.example", IPv4: "192.168.7.20"})
	out := dnsfile.Render(f, m)
	if loc := resolutionChanging.Find(out); loc != nil {
		t.Fatalf("render wrote %q:\n%s", loc, out)
	}
	recordOrMarker := regexp.MustCompile(`^(address=/[a-z0-9.-]+/[0-9.]+|# nexus:zone [a-z0-9.-]+|# nexus:unzoned|)$`)
	for _, l := range lines(out) {
		if !recordOrMarker.MatchString(l) {
			t.Errorf("render emitted %q, which is neither a record line nor a marker", l)
		}
	}
	for _, name := range []string{"www.xi.example", "xi.example", "mail.xi.example"} {
		if ip, cov := dnsfile.Expected(m, name); cov {
			t.Errorf("public name %s is covered locally (%s): split-horizon broken", name, ip)
		}
	}
}

var recLine = regexp.MustCompile(`^address=/([a-z0-9-]+(?:\.[a-z0-9-]+)+)/((?:[0-9]{1,3}\.){3}[0-9]{1,3})$`)

// TC-30 (regression: the user's real file): the read-only capture of the
// 53-line live file parses to 53 A records and 0 unmanaged lines; with the
// three implied zones declared, membership follows the suffix rule, the
// split-horizon zone gets no local=, unity.local and unity.lan stay unzoned,
// and the reorganized file is DNS-equivalent to the original.
func TestTC30_Live53LineFile(t *testing.T) {
	prov := string(read(t, "PROVENANCE"))
	live := prov[strings.Index(prov, "live-53-lines.conf"):]
	orig := read(t, "live-53-lines.conf")
	sum := sha256.Sum256(orig)
	if !strings.Contains(live, hex.EncodeToString(sum[:])) {
		t.Fatalf("fixture sha256 %x is not the one recorded in PROVENANCE", sum)
	}
	raw := lines(orig)
	type rec struct{ name, ip string }
	var want []rec
	for i, l := range raw {
		mm := recLine.FindStringSubmatch(l)
		if mm == nil {
			t.Errorf("line %d %q is not address=/<fqdn>/<ipv4>; requirements say every line is", i+1, l)
			continue
		}
		want = append(want, rec{mm[1], mm[2]})
	}
	if len(raw) != 53 || len(want) != 53 {
		t.Fatalf("fixture has %d lines, %d records; requirements say 53 and 53", len(raw), len(want))
	}

	f, m := model(t, orig)
	if len(m.Records) != 53 || m.Unmanaged != 0 {
		t.Fatalf("parsed %d records, %d unmanaged; want 53, 0 (a name outside A-9 falsifies A-9)", len(m.Records), m.Unmanaged)
	}
	zones := []string{"home.local", "app.lan", "xi.example"}
	apply(t, &m, fr23Zones...)
	zoneOf := func(name string) string { // the FR-17 rule, restated independently
		best := ""
		for _, z := range zones {
			if (name == z || strings.HasSuffix(name, "."+z)) && len(z) > len(best) {
				best = z
			}
		}
		return best
	}
	got := map[string]string{}
	for _, r := range m.Records {
		got[r.FQDN] = r.Zone
		if r.Zone != zoneOf(r.FQDN) {
			t.Errorf("%s in zone %q, want %q", r.FQDN, r.Zone, zoneOf(r.FQDN))
		}
	}
	for _, loose := range []string{"unity.local", "unity.lan"} {
		if z, ok := got[loose]; !ok || z != "" {
			t.Errorf("%s: present=%v zone=%q, want present and unzoned", loose, ok, z)
		}
	}

	out := dnsfile.Render(f, m)
	if loc := resolutionChanging.Find(out); loc != nil {
		t.Fatalf("render wrote %q", loc)
	}
	if !dnsfile.DNSEquivalent(orig, out) {
		t.Error("declaring zones changed DNS-relevant lines (must be comment-only, NFR-3)")
	}
	if n := strings.Count(string(out), "# nexus:zone "); n != 3 {
		t.Errorf("%d zone markers, want 3", n)
	}
	if !bytes.Equal(rerender(out), out) {
		t.Error("render not idempotent on the live file")
	}
	if _, m2 := model(t, out); len(m2.Records) != 53 {
		t.Errorf("re-parse has %d records, want 53", len(m2.Records))
	}
	for _, r := range want {
		if ip, cov := dnsfile.Expected(m, r.name); !cov || (ip != r.ip && !isDup(m, r.name)) {
			t.Errorf("Expected(%s) = %q,%v, want %q", r.name, ip, cov, r.ip)
		}
	}
}

func isDup(m dnsfile.Model, name string) bool {
	for _, r := range m.Records {
		if r.FQDN == name && r.Duplicate {
			return true
		}
	}
	return false
}

// TC-31: record IDs are "<fqdn>~A" and the etag is "sha256:" + hex of the raw
// bytes.
func TestTC31_IDsAndEtag(t *testing.T) {
	b := read(t, "fr23-seven-lines.conf")
	_, m := model(t, b)
	sum := sha256.Sum256(b)
	if want := "sha256:" + hex.EncodeToString(sum[:]); m.Etag != want {
		t.Errorf("etag %q, want %q", m.Etag, want)
	}
	for _, r := range m.Records {
		if r.ID != r.FQDN+"~A" {
			t.Errorf("record %s has ID %q", r.FQDN, r.ID)
		}
	}
}

// TC-E5: zone markers must be exact; near-misses are unmanaged and declare
// nothing (no inference, ASM-10).
func TestTCE5_MarkerNearMissesAreUnmanaged(t *testing.T) {
	in := "#nexus:zone a.lan\n# nexus:zone A.lan\n# nexus:zone  a.lan\n# nexus:zone a.lan \n# nexus:unzoned x\n# Nexus:zone a.lan\n" +
		"address=/x.a.lan/192.168.7.1\n"
	_, m := model(t, []byte(in))
	if len(m.Zones) != 0 || m.Unmanaged != 6 {
		t.Errorf("zones=%v unmanaged=%d, want none and 6", m.Zones, m.Unmanaged)
	}
}

// TC-E6: a declared zone with no records renders as a bare marker, and
// declaring zones never auto-declares others (ASM-10).
func TestTCE6_EmptyZoneAndNoInference(t *testing.T) {
	f, m := model(t, read(t, "fr23-seven-lines.conf"))
	apply(t, &m, dnsfile.AddZone{Zone: "empty.lan"})
	if !slices.Equal(m.Zones, []string{"empty.lan"}) {
		t.Errorf("zones %v, want only empty.lan", m.Zones)
	}
	if out := string(dnsfile.Render(f, m)); !strings.HasPrefix(out, "# nexus:zone empty.lan\n\n# nexus:unzoned\n") {
		t.Errorf("render %q", out)
	}
}
