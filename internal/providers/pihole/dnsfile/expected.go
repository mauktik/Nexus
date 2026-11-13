package dnsfile

import "strings"

// Expected is the answer dnsmasq gives an A query for fqdn from m's record
// lines. An "address=/<name>/<ip>" line answers for name and every name
// under it on label boundaries, and the longest matching name wins (dnsmasq
// --address, which matches domains as --server does) (FR-24). A name no
// record line covers is not covered: dnsmasq forwards it upstream, which is
// what keeps a split-horizon zone's public names public (CON-6). For a
// duplicate name, whose lines dnsmasq answers together, the first line's
// address is returned.
func Expected(m Model, fqdn string) (ip string, covered bool) {
	name := strings.TrimSuffix(strings.ToLower(fqdn), ".")
	best := -1
	for i, r := range m.Records {
		if underSuffix(name, r.FQDN) && (best < 0 || len(r.FQDN) > len(m.Records[best].FQDN)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return m.Records[best].IPv4, true
}
