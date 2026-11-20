// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package dnsfile

import (
	"slices"
	"sort"
	"strings"
)

// Render writes the FR-23 layout of m: the preamble's unmanaged lines, then
// one section per declared zone in order (its marker, its records sorted by
// name with ties in file order, then that section's unmanaged lines), then
// "# nexus:unzoned" with the unzoned records and that section's unmanaged
// lines, only if any. Blocks are separated by one blank line, the output
// ends with "\n", and an empty model renders as 0 bytes. There is no
// header line.
//
// The output holds only record lines, the two marker forms and f's
// unmanaged lines (CON-6). The unmanaged lines of a section whose zone m no
// longer declares move to the end of the preamble (A-44). Blank lines a
// block would end with are dropped, because the next parse would read them
// as layout (A-43); that keeps Render idempotent.
func Render(f *File, m Model) []byte {
	var preamble, moved, unzoned []string
	byZone := map[string][]string{}
	for _, s := range f.sections {
		texts := unmanagedTexts(s)
		switch {
		case s.kind == sectionPreamble:
			preamble = append(preamble, texts...)
		case s.kind == sectionUnzoned:
			unzoned = append(unzoned, texts...)
		case slices.Contains(m.Zones, s.zone) && !slices.Contains(m.detached, s.zone):
			byZone[s.zone] = append(byZone[s.zone], texts...)
		default:
			moved = append(moved, texts...)
		}
	}

	recs := slices.Clone(m.Records)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].FQDN < recs[j].FQDN })
	recordLines := func(zone string) []string {
		var out []string
		for _, r := range recs {
			if r.Zone == zone {
				out = append(out, recordPrefix+r.FQDN+"/"+r.IPv4)
			}
		}
		return out
	}

	var blocks [][]string
	if b := trimBlank(append(preamble, moved...)); len(b) > 0 {
		blocks = append(blocks, b)
	}
	for _, z := range m.Zones {
		b := append([]string{zoneMarker + z}, recordLines(z)...)
		blocks = append(blocks, trimBlank(append(b, byZone[z]...)))
	}
	if b := trimBlank(append(recordLines(""), unzoned...)); len(b) > 0 {
		blocks = append(blocks, append([]string{unzonedMarker}, b...))
	}

	var sb strings.Builder
	for i, b := range blocks {
		if i > 0 {
			sb.WriteByte('\n')
		}
		for _, l := range b {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	return []byte(sb.String())
}

func unmanagedTexts(s section) []string {
	var out []string
	for _, l := range s.lines {
		if l.kind == kindUnmanaged {
			out = append(out, l.text)
		}
	}
	return out
}

func trimBlank(ls []string) []string {
	n := len(ls)
	for n > 0 && ls[n-1] == "" {
		n--
	}
	return ls[:n]
}

// DNSEquivalent reports whether a and b hold the same multiset of lines
// that dnsmasq reads as options: every line except blank ones and those
// whose first non-space character is "#" (A-42).
func DNSEquivalent(a, b []byte) bool {
	count := map[string]int{}
	for _, l := range optionLines(a) {
		count[l]++
	}
	for _, l := range optionLines(b) {
		count[l]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}

func optionLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimLeft(l, " \t")
		if t != "" && t[0] != '#' {
			out = append(out, l)
		}
	}
	return out
}
