// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package backend is the seam between the pihole provider and a Pi-hole
// major version: the Reader used in the nexusd sandbox, the Mutator used in
// the privileged helper, version detection, and Select. v1 ships the v5-cli
// backend (package v5) and Unsupported for v6.
package backend

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// Versions are the detected Pi-hole versions. Each is cleaned to
// ^v?[0-9]+(\.[0-9]+)* or nil. Major 0 means undetected.
type Versions struct {
	Core, Web, FTL *string
	Major          int
}

// Reader is every non-DNS-file read. It never executes a Pi-hole binary.
type Reader interface {
	Name() string
	Status(ctx context.Context) (spec.StatusProperties, error)
	Summary(ctx context.Context) (spec.SummaryProperties, error)
	Blocking(ctx context.Context) (spec.BlockingProperties, error)
	Domains(ctx context.Context) ([]spec.DomainProperties, error) // all rows, ordered by id
	Adlists(ctx context.Context) ([]spec.AdlistProperties, error)
}

// Mutator is the base every backend's mutation half implements. The area
// interfaces below are separate, so each one is added to a backend with its
// own compile-time assertion; a helper action obtains its area by type
// assertion. The Unsupported backend implements all of them.
type Mutator interface {
	FTLPID() (int, bool)
	WaitExternalGravity(ctx context.Context, out io.Writer) error // pihole-busy after 30 min
}

// ServiceMutator restarts and tests FTL; dnswrite uses it too.
type ServiceMutator interface {
	RestartDNS(ctx context.Context, out io.Writer) error
	FlushCache(ctx context.Context, out io.Writer) error
	TestConfig(ctx context.Context, out io.Writer) (exitCode int, err error)
}

// BlockingMutator stops and starts blocking.
type BlockingMutator interface {
	StopBlocking(ctx context.Context, d time.Duration, out io.Writer) (spec.BlockingProperties, error)
	StartBlocking(ctx context.Context, out io.Writer) (spec.BlockingProperties, error)
}

// DomainMutator adds and removes domainlist rows.
type DomainMutator interface {
	AddDomain(ctx context.Context, in spec.DomainAddInput, out io.Writer) (spec.DomainProperties, bool /*created*/, error)
	RemoveDomain(ctx context.Context, id int64, out io.Writer) (spec.DomainProperties, error)
}

// AdlistMutator adds and removes adlists and runs gravity.
type AdlistMutator interface {
	AddAdlist(ctx context.Context, url, comment string, out io.Writer) (spec.AdlistProperties, bool, error)
	RemoveAdlist(ctx context.Context, id int64, out io.Writer) (spec.AdlistProperties, error)
	UpdateGravity(ctx context.Context, out io.Writer) (spec.GravityResult, error)
}

// Deps are the seams Select passes to a backend.
type Deps struct {
	Runner    v5.Runner        // the exec runner in production
	DNSServer netip.AddrPort   // 127.0.0.1:53 in production
	Now       func() time.Time // the Mutator's clock; nil means time.Now
	Reload    v5.ReloadTiming  // the list reload barrier's bounds; zero means production's
}

// The v5-cli Mutator implements the base Mutator; each area interface is
// asserted beside it by the work item that adds its methods.
var _ Mutator = (*v5.Mutator)(nil)

// ErrNotDetected is Select's answer when no Pi-hole major was detected.
var ErrNotDetected = errors.New("pi-hole version not detected (no /etc/pihole/versions, no FTL API, no pihole.toml)")

// Select picks the backend from detected versions: Major 5 is v5-cli,
// Major 6 is Unsupported, anything else is (nil, nil, "", ErrNotDetected).
func Select(v Versions, p v5.Paths, deps Deps) (Reader, Mutator, string, error) {
	switch v.Major {
	case 5:
		return &v5.Reader{Paths: p, Runner: deps.Runner, DNSServer: deps.DNSServer},
			&v5.Mutator{Paths: p, Runner: deps.Runner, DNSServer: deps.DNSServer, Now: deps.Now,
				Reload: deps.Reload}, spec.BackendV5CLI, nil
	case 6:
		return Unsupported{}, Unsupported{}, spec.BackendUnsupported, nil
	}
	return nil, nil, "", ErrNotDetected
}

// Detect finds the Pi-hole versions (FR-3), in this order:
//  1. /etc/pihole/versions: the major of CORE_VERSION, else of FTL_VERSION
//     (V-8; exact keys, so GITHUB_* is never read);
//  2. the major of FTL's `>version` reply (V-3), within ftlTimeout;
//  3. /etc/pihole/pihole.toml exists: v6;
//  4. otherwise undetected (Major 0).
//
// A value that is not a version is unknown and falls through to the next
// source. FTL is asked only when the file has no usable version for it.
func Detect(ctx context.Context, p v5.Paths, ftlTimeout time.Duration) Versions {
	vf, err := v5.ReadVersions(p)
	if err != nil {
		// Missing (updatecheck.sh has not run) or unreadable: fall through.
		vf = v5.VersionFile{}
	}
	v := Versions{Core: vf.Core, Web: vf.Web, FTL: vf.FTL}
	if v.Major = v5.Major(v.Core); v.Major != 0 {
		return v
	}
	if v.Major = v5.Major(v.FTL); v.Major != 0 {
		return v
	}
	ftl, err := v5.FTLVersion(ctx, v5.FTLAddr(v5.ReadFTLConf(p).Port), ftlTimeout)
	if err == nil && v5.Major(ftl) != 0 {
		v.FTL, v.Major = ftl, v5.Major(ftl)
		return v
	}
	if _, err := os.Stat(p.PiholeTOMLPath()); err == nil {
		v.Major = 6
	}
	return v
}

// CheckV6 reports whether a v6 host loads /etc/dnsmasq.d, which the DNS-file
// support needs (ASM-6): misc.etc_dnsmasq_d = true in pihole.toml. It is a
// line scan of the [misc] section; '#' starts a comment, as in the
// `### CHANGED, default = …` suffix FTL v6 writes after a value. Otherwise
// it returns false and the reason the provider is unavailable.
func CheckV6(p v5.Paths) (bool, string) {
	const reason = "pi-hole v6: misc.etc_dnsmasq_d is not true in " + v5.PiholeTOMLFile
	b, err := os.ReadFile(p.PiholeTOMLPath())
	if err != nil {
		return false, reason
	}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "misc" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "etc_dnsmasq_d" && strings.TrimSpace(val) == "true" {
			return true, ""
		}
	}
	return false, reason
}
