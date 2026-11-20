// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/services-provider/test-cases.md TC-SV-115 (WI 4.4):
// host.RebootRequiredNow, the export the services provider's reboot reads
// the reboot-required state through, against the host provider's own rule.

package host

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// TC-SV-115: RebootRequiredNow equals the host provider's own rule. Owner WI 4.4. Requirements: FR-10, FR-11.
//
// Every variant runs both ways: RebootRequiredNow over the host stack's
// options (the same F-FS and platform), and the stack's provider's own
// rebootRequired(). TC-HP-93's apt parity is its own test, unchanged.
func TestTCSV115_RebootRequiredNowEqualsTheHostProvidersOwnRule(t *testing.T) {
	const pkgs = "linux-image-7.0.0-43-generic\nlinux-base\n"
	cases := []struct {
		name    string
		capture string
		setup   func(s *hostStack)
		state   string
	}{
		{"ubuntu as captured", capUBU, func(s *hostStack) { s.outsideContainer() }, spec.RebootFalse},
		{"ubuntu marker and pkgs", capUBU, func(s *hostStack) { s.outsideContainer(); s.rebootMarker(pkgs) },
			spec.RebootTrue},
		{"ubuntu marker in a container", capUBU, func(s *hostStack) { s.insideContainer(); s.rebootMarker(pkgs) },
			spec.RebootTrue},
		{"ubuntu no marker, no writer", capUBU, func(s *hostStack) { s.outsideContainer(); s.removeWriters() },
			spec.RebootUnknown},
		{"alpine as captured (container)", capALP, func(*hostStack) {}, spec.RebootUnknown},
		{"alpine VM, modules missing", capALP, func(s *hostStack) { s.outsideContainer() }, spec.RebootTrue},
		{"alpine VM, modules present", capALP, func(s *hostStack) {
			s.outsideContainer()
			s.ffs.replace(fmt.Sprintf("lib/modules/%s/modules.dep", osReleaseOf(s.t, capALP)), nil)
		}, spec.RebootFalse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newHostStack(t, c.capture)
			c.setup(s)
			plat := s.plat
			got := RebootRequiredNow(Options{FS: s.ffs, Platform: &plat, GOOS: "linux"})
			// The provider's public answer (health's rebootRequired), not
			// rebootRequired() itself, which RebootRequiredNow wraps.
			want := s.health().RebootRequired
			// The health item went through JSON: an empty list or map comes
			// back as nil, or the reverse.
			for _, r := range []*spec.RebootRequired{&got, &want} {
				r.Packages = append([]string{}, r.Packages...)
				if r.Unavailable == nil {
					r.Unavailable = map[string]string{}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("RebootRequiredNow = %+v\nhealth rebootRequired = %+v", got, want)
			}
			if c.state != "" && got.State != c.state {
				t.Errorf("state %q, want %q (%+v)", got.State, c.state, got)
			}
			if c.state == spec.RebootTrue && c.capture == capUBU && len(got.Packages) != 2 {
				t.Errorf("packages %q, want the two planted", got.Packages)
			}
		})
	}
}
