// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package v5 is the v5-cli backend for Pi-hole v5 (core v5.18.x, FTL
// v5.25.x): the host paths, the host-file readers, the FTL text-API client
// and the Reader that runs inside the nexusd sandbox. The Reader never
// executes a Pi-hole binary (A-26): pihole-FTL carries file capabilities the
// sandbox's bounding set would refuse.
package v5

import "path/filepath"

// Fixed production paths (architecture "v5 host paths").
const (
	PiholeBin         = "/usr/local/bin/pihole"
	PiholeFTLBin      = "/usr/bin/pihole-FTL"
	VersionsFile      = "/etc/pihole/versions"
	SetupVarsFile     = "/etc/pihole/setupVars.conf"
	FTLConfFile       = "/etc/pihole/pihole-FTL.conf"
	GravityDBFile     = "/etc/pihole/gravity.db"
	DnsmasqConfFile   = "/etc/dnsmasq.conf"
	PiholeTOMLFile    = "/etc/pihole/pihole.toml" // v6 only
	DefaultFTLPIDFile = "/run/pihole-FTL.pid"     // V-4
	// DefaultFTLLogFile is FTL's log when pihole-FTL.conf has no LOGFILE=
	// (FTL v5.25.2 src/config.c getLogFilePath; the image's FTL logs "Using
	// log file /var/log/pihole/FTL.log", WI 5.6 PROVENANCE "FTL LIST RELOAD
	// SIGNALS").
	DefaultFTLLogFile = "/var/log/pihole/FTL.log"
	ProcDir           = "/proc"
)

// Paths locates the Pi-hole files. Root is "/" in production and a temp
// directory in tests; every file path is resolved under it. A zero-valued
// field means the fixed production path above.
type Paths struct {
	Root        string
	Pihole      string
	PiholeFTL   string
	Versions    string
	SetupVars   string
	FTLConf     string
	GravityDB   string
	DnsmasqConf string
	PiholeTOML  string
	// FTLLog overrides FTL's log; empty means pihole-FTL.conf's LOGFILE=,
	// else DefaultFTLLogFile.
	FTLLog string
}

// Host resolves an absolute host path under Root.
func (p Paths) Host(abs string) string {
	root := p.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, abs)
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// PiholeCmd is the pihole argv[0]. It is the host path, not resolved under
// Root, because it is what a Runner executes; use Host to stat it.
func (p Paths) PiholeCmd() string { return or(p.Pihole, PiholeBin) }

// PiholeFTLCmd is the pihole-FTL argv[0], like PiholeCmd.
func (p Paths) PiholeFTLCmd() string { return or(p.PiholeFTL, PiholeFTLBin) }

// VersionsPath is /etc/pihole/versions under Root.
func (p Paths) VersionsPath() string { return p.Host(or(p.Versions, VersionsFile)) }

// SetupVarsPath is /etc/pihole/setupVars.conf under Root.
func (p Paths) SetupVarsPath() string { return p.Host(or(p.SetupVars, SetupVarsFile)) }

// FTLConfPath is /etc/pihole/pihole-FTL.conf under Root.
func (p Paths) FTLConfPath() string { return p.Host(or(p.FTLConf, FTLConfFile)) }

// GravityDBPath is /etc/pihole/gravity.db under Root.
func (p Paths) GravityDBPath() string { return p.Host(or(p.GravityDB, GravityDBFile)) }

// DnsmasqConfPath is /etc/dnsmasq.conf under Root.
func (p Paths) DnsmasqConfPath() string { return p.Host(or(p.DnsmasqConf, DnsmasqConfFile)) }

// PiholeTOMLPath is /etc/pihole/pihole.toml (v6) under Root.
func (p Paths) PiholeTOMLPath() string { return p.Host(or(p.PiholeTOML, PiholeTOMLFile)) }

// FTLLogPath is FTL's log under Root: FTLLog when set, else the first
// LOGFILE= of pihole-FTL.conf, else /var/log/pihole/FTL.log (A-65).
func (p Paths) FTLLogPath() string {
	if p.FTLLog != "" {
		return p.Host(p.FTLLog)
	}
	return p.Host(ReadFTLConf(p).LogFile)
}
