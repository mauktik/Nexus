// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Stub-style: RED-by-design (WI 1.1). Fails to compile until
// Providers.Pihole / PiholeConfig exist; GREEN when WI 1.1 lands. The
// developer may fold these cases into config_test.go.
// Cases: TC-1, TC-E1, TC-ERR1, TC-ERR2 in planning/pihole-dns-zones/test-cases.md.
package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const piholeDefaultDNSFile = "/etc/dnsmasq.d/03-custom-dns-names.conf"

// TC-1: providers.pihole defaults to disabled with the live host's file, in
// Default() and in the shipped default config.
func TestTC1_PiholeDefaults(t *testing.T) {
	d := Default().Providers.Pihole.Settings
	if d.Enabled || d.DNSFile != piholeDefaultDNSFile {
		t.Errorf("Default() pihole = %+v, want disabled with dnsFile %s", d, piholeDefaultDNSFile)
	}
	data, err := os.ReadFile(defaultConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data, defaultConfigPath)
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	got := Default().Providers.Pihole.Settings
	if err := cfg.Providers.Pihole.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Enabled || got.DNSFile != piholeDefaultDNSFile {
		t.Errorf("shipped default pihole = %+v", got)
	}
	if !strings.Contains(string(data), "\n  pihole:\n") {
		t.Error("deploy/nexusd.default.yaml has no providers.pihole section")
	}
}

func piholeYAML(dnsFile string) string {
	return "providers:\n  pihole:\n    enabled: true\n    dnsFile: \"" + dnsFile + "\"\n"
}

// TC-ERR1: validate-config rejects every dnsFile that is not a loadable,
// Nexus-owned file directly in /etc/dnsmasq.d (FR-2, V-11).
func TestTCERR1_PiholeDNSFileRejected(t *testing.T) {
	for _, p := range []string{
		"03-custom-dns-names.conf",                   // not absolute
		"/etc/03-custom-dns-names.conf",              // wrong directory
		"/etc/dnsmasq.d/sub/03-custom.conf",          // not a direct child
		"/etc/dnsmasq.d/../dnsmasq.d.conf",           // traversal out after Clean
		"/etc/dnsmasq.d/../pihole/custom.list",       // traversal to Pi-hole's own file
		"/etc/dnsmasq.d/.03-custom.conf",             // dnsmasq skips leading '.'
		"/etc/dnsmasq.d/03-custom.conf~",             // dnsmasq skips trailing '~'
		"/etc/dnsmasq.d/#03-custom.conf#",            // dnsmasq skips #...#
		"/etc/dnsmasq.d/01-pihole.conf",              // Pi-hole managed
		"/etc/dnsmasq.d/02-pihole-dhcp.conf",         // Pi-hole managed
		"/etc/dnsmasq.d/04-pihole-static-dhcp.conf",  // Pi-hole managed
		"/etc/dnsmasq.d/05-pihole-custom-cname.conf", // Pi-hole managed
		"/etc/dnsmasq.d/06-rfc6761.conf",             // Pi-hole managed
		"/etc/dnsmasq.d/sub/../01-pihole.conf",       // managed after Clean
		"/etc/dnsmasq.d",                             // the directory itself
		"/etc/dnsmasq.d/",                            // the directory itself
	} {
		_, err := Parse([]byte(piholeYAML(p)), "c.yaml")
		var list ErrorList
		if !errors.As(err, &list) {
			t.Errorf("dnsFile %q: err=%v, want an ErrorList", p, err)
			continue
		}
		found := false
		for _, e := range list {
			if e.Path == "providers.pihole.dnsFile" && e.Line != 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("dnsFile %q: no located error at providers.pihole.dnsFile in %v", p, err)
		}
	}
}

// TC-E1: names dnsmasq does load, and that Pi-hole does not manage, pass.
func TestTCE1_PiholeDNSFileAccepted(t *testing.T) {
	for _, p := range []string{
		piholeDefaultDNSFile,
		"/etc/dnsmasq.d/99-nexus.conf",
		"/etc/dnsmasq.d//03-custom-dns-names.conf", // Clean folds the double slash
		"/etc/dnsmasq.d/#03-custom.conf",           // starts with # but does not end with it
		"/etc/dnsmasq.d/03-custom.conf#",           // ends with # but does not start with it
	} {
		cfg, err := Parse([]byte(piholeYAML(p)), "c.yaml")
		if err != nil {
			t.Errorf("dnsFile %q: %v", p, err)
			continue
		}
		var pc PiholeConfig
		if err := cfg.Providers.Pihole.Decode(&pc); err != nil || !pc.Enabled || pc.DNSFile != p {
			t.Errorf("dnsFile %q decoded %+v, %v", p, pc, err)
		}
	}
}

// TC-ERR2: decoding is strict, so a misspelled key is an error, not ignored.
func TestTCERR2_PiholeUnknownKey(t *testing.T) {
	_, err := Parse([]byte("providers:\n  pihole:\n    enabled: true\n    dnsfile: /etc/dnsmasq.d/x.conf\n"), "c.yaml")
	var list ErrorList
	if !errors.As(err, &list) {
		t.Fatalf("err=%v, want an ErrorList", err)
	}
	for _, e := range list {
		if e.Path == "providers.pihole.dnsfile" {
			return
		}
	}
	t.Errorf("no error at providers.pihole.dnsfile in %v", err)
}
