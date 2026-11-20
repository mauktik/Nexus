package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	nexusdspec "github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/providers/scripts/spec"
	storagespec "github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/release"
	"github.com/mauktik/nexus/internal/svcctl"
)

// validate checks values after decoding. Each problem is located at the
// value in the file, or carries only its path when the value is a default.
func (c *Config) validate(errs *collector) {
	v := validator{errs: errs, locs: c.locs}

	if len(c.Listen) == 0 {
		v.fail("listen", "at least one address is required")
	}
	seen := map[string]bool{}
	for i, addr := range c.Listen {
		p := fmt.Sprintf("listen[%d]", i)
		if err := checkListen(addr); err != nil {
			v.fail(p, err.Error())
		} else if seen[addr] {
			v.failf(p, "duplicate address %q", addr)
		}
		seen[addr] = true
	}

	switch c.TLS.Mode {
	case TLSFiles:
		if c.TLS.CertFile == "" {
			v.fail("tls.certFile", "required when tls.mode is files")
		}
		if c.TLS.KeyFile == "" {
			v.fail("tls.keyFile", "required when tls.mode is files")
		}
		if c.TLS.CertFile != "" && c.TLS.KeyFile != "" {
			v.tlsFiles(c.TLS.CertFile, c.TLS.KeyFile)
		}
	case TLSAuto, TLSOff:
		if c.TLS.CertFile != "" {
			v.fail("tls.certFile", "only allowed when tls.mode is files")
		}
		if c.TLS.KeyFile != "" {
			v.fail("tls.keyFile", "only allowed when tls.mode is files")
		}
	default:
		v.failf("tls.mode", "must be one of auto, files or off, got %q", c.TLS.Mode)
	}
	// Plain HTTP only on loopback (FR-23, ASM-5). There is deliberately no
	// key or flag that relaxes this.
	if c.TLS.Mode == TLSOff {
		for i, addr := range c.Listen {
			if checkListen(addr) == nil && !isLoopbackListen(addr) {
				v.failf(fmt.Sprintf("listen[%d]", i), "%q is not a loopback address, and tls.mode off is allowed only "+
					"when every listen address is a loopback IP such as 127.0.0.1 or [::1]; use tls.mode auto or files", addr)
			}
		}
	}
	for i, san := range c.TLS.ExtraSANs {
		if san == "" {
			v.fail(fmt.Sprintf("tls.extraSANs[%d]", i), "must not be empty")
		}
	}

	v.positive("limits.maxBodyBytes", c.Limits.MaxBodyBytes)
	v.positive("limits.storageBudgetBytes", c.Limits.StorageBudgetBytes)
	v.positive("limits.outputCapBytes", c.Limits.OutputCapBytes)
	v.positive("auth.failureLimitPerMinute", int64(c.Auth.FailureLimitPerMinute))
	v.positive("audit.retentionDays", int64(c.Audit.RetentionDays))
	if c.Audit.RetentionDays > MaxAuditRetentionDays {
		v.failf("audit.retentionDays", "must be at most %d (100 years), got %d", MaxAuditRetentionDays, c.Audit.RetentionDays)
	}

	docker := c.Providers.Docker.Settings
	v.absolute("providers.docker.socket", docker.Socket)
	v.absolute("providers.docker.configDir", docker.ConfigDir)
	v.positiveDuration("providers.docker.updateHealthTimeout", docker.UpdateHealthTimeout)
	v.positiveDuration("providers.docker.updateMaxRuntime", docker.UpdateMaxRuntime)
	v.trustedOwners("providers.docker.compose.trustedOwners", docker.Compose.TrustedOwners)

	apt := c.Providers.Apt.Settings
	v.positiveDuration("providers.apt.lockWait", apt.LockWait)
	v.positiveDuration("providers.apt.maxRuntime", apt.MaxRuntime)

	v.catalog(c.Providers.Scripts.Settings.Catalog)

	v.nginx(c.Providers.Nginx.Settings, c.Limits.MaxBodyBytes)
	v.nexusd(c.Providers.Nexusd.Settings)
	v.piholeDNSFile("providers.pihole.dnsFile", c.Providers.Pihole.Settings.DNSFile)
	v.host(c.Providers.Host.Settings)
	v.storage(c.Providers.Storage.Settings)
}

// NginxSuffixPattern is what includeSuffix and disabledSuffix must match: a
// dot then letters or digits, so neither can carry a path separator or a
// glob character.
const NginxSuffixPattern = `^\.[A-Za-z0-9]+$`

var (
	nginxSuffixRE = regexp.MustCompile(NginxSuffixPattern)
	// The service controller refuses any other name, so validate-config
	// applies its pattern rather than a copy that could drift.
	nginxServiceRE = regexp.MustCompile(svcctl.ServiceNamePattern)
)

// nginx applies the FR-2 rules for the nginx section. A disabled site is
// <name><includeSuffix><disabledSuffix>, so a disabledSuffix ending in
// includeSuffix would leave the disabled file matched by the include glob.
func (v validator) nginx(n NginxConfig, maxBodyBytes int64) {
	const p = "providers.nginx."
	if n.IncludeDir != "" {
		v.absolute(p+"includeDir", n.IncludeDir)
	}
	v.nginxSuffix(p+"includeSuffix", n.IncludeSuffix)
	if v.nginxSuffix(p+"disabledSuffix", n.DisabledSuffix) && n.IncludeSuffix != "" &&
		strings.HasSuffix(n.DisabledSuffix, n.IncludeSuffix) {
		v.failf(p+"disabledSuffix", "must not end in includeSuffix %q, or a disabled file would still match the "+
			"include glob, got %q", n.IncludeSuffix, n.DisabledSuffix)
	}
	if !nginxServiceRE.MatchString(n.Service) {
		v.failf(p+"service", "must match %s, got %q", svcctl.ServiceNamePattern, n.Service)
	}
	v.positiveDuration(p+"testTimeout", n.TestTimeout)
	v.positiveDuration(p+"stateTimeout", n.StateTimeout)
	v.positiveDuration(p+"reloadVerifyTimeout", n.ReloadVerifyTimeout)
	v.positive(p+"maxConfigBytes", n.MaxConfigBytes)
	// A non-positive limits.maxBodyBytes is already reported on its own.
	if maxBodyBytes > 0 && n.MaxConfigBytes > maxBodyBytes {
		v.failf(p+"maxConfigBytes", "must be at most limits.maxBodyBytes (%d), got %d", maxBodyBytes, n.MaxConfigBytes)
	}
}

// nginxSuffix reports whether suffix is non-empty, failing path when it is
// empty or not of the NginxSuffixPattern form.
func (v validator) nginxSuffix(path, suffix string) bool {
	switch {
	case suffix == "":
		v.failf(path, "required: a dot then letters or digits, matching %s", NginxSuffixPattern)
		return false
	case !nginxSuffixRE.MatchString(suffix):
		v.failf(path, "must match %s, got %q", NginxSuffixPattern, suffix)
	}
	return true
}

// nexusd checks the nexusd section. A trusted key is never quoted: the
// message names only its index, so validate-config output and logs carry no
// key material.
func (v validator) nexusd(n NexusdConfig) {
	const p = "providers.nexusd"
	if n.HealthTimeout < MinNexusdHealthTimeout || n.HealthTimeout > MaxNexusdHealthTimeout {
		v.failf(p+".healthTimeout", "must be between %s and %s, got %s",
			MinNexusdHealthTimeout, MaxNexusdHealthTimeout, n.HealthTimeout)
	}
	if b := n.Update.MaxUploadBytes; b < MinNexusdMaxUploadBytes || b > nexusdspec.MaxUploadBytesCeiling {
		v.failf(p+".update.maxUploadBytes", "must be between %d (1 MiB) and %d (256 MiB), got %d",
			MinNexusdMaxUploadBytes, nexusdspec.MaxUploadBytesCeiling, b)
	}
	first := map[string]int{}
	for i, k := range n.Update.TrustedKeys {
		path := fmt.Sprintf("%s.update.trustedKeys[%d]", p, i)
		if _, err := release.ParsePublicKey(k); err != nil {
			v.failf(path, "trustedKeys[%d] is not a valid key: want the standard base64 of a raw 32-byte "+
				"Ed25519 public key, as make release-key prints", i)
			continue
		}
		// The encoding is strict, so equal keys are equal strings.
		if j, dup := first[k]; dup {
			v.failf(path, "trustedKeys[%d] is the same key as trustedKeys[%d]", i, j)
			continue
		}
		first[k] = i
	}
}

// host checks the host section against the spec's bounds (FR-11, FR-12,
// FR-13). The history must hold at least one sample, so its lower bound is
// sampleInterval; an out-of-range sampleInterval is reported on its own.
func (v validator) host(h HostProviderConfig) {
	const p = "providers.host"
	if h.SampleInterval < hostspec.MinSampleInterval || h.SampleInterval > hostspec.MaxSampleInterval {
		v.failf(p+".sampleInterval", "must be between %s and %s, got %s", shortDuration(hostspec.MinSampleInterval),
			shortDuration(hostspec.MaxSampleInterval), shortDuration(h.SampleInterval))
	}
	if h.HistoryDuration < h.SampleInterval || h.HistoryDuration > hostspec.MaxHistoryDuration {
		v.failf(p+".historyDuration", "must be between sampleInterval (%s) and %s, got %s",
			shortDuration(h.SampleInterval), shortDuration(hostspec.MaxHistoryDuration), shortDuration(h.HistoryDuration))
	}
	if h.SourceTimeout < hostspec.MinSourceTimeout || h.SourceTimeout > hostspec.MaxSourceTimeout {
		v.failf(p+".sourceTimeout", "must be between %s and %s, got %s", shortDuration(hostspec.MinSourceTimeout),
			shortDuration(hostspec.MaxSourceTimeout), shortDuration(h.SourceTimeout))
	}
}

var (
	storagePoolRE       = regexp.MustCompile(storagespec.PoolNamePattern)
	storageDatasetRE    = regexp.MustCompile(storagespec.DatasetNamePattern)
	storageDatasetNotRE = regexp.MustCompile(storagespec.DatasetNameNotPattern)
)

// Bounds of the storage section that the spec does not declare.
const (
	minStoragePercent      = 1
	maxStoragePercent      = 100
	minStorageScrubMaxDays = 1
	maxStorageScrubMaxDays = 366
)

// storage checks the storage section against the spec's patterns and bounds
// (FR-9 to FR-15, CON-7). Every rule applies whether or not the section is
// enabled, so a file is valid or not before it is switched on.
func (v validator) storage(s StorageConfig) {
	const p = "providers.storage."
	v.storageDuration(p+"commandTimeout", s.CommandTimeout, storagespec.MinCommandTimeout, storagespec.MaxCommandTimeout)
	v.storagePaths(p+"backups.vzdumpPaths", s.Backups.VzdumpPaths)
	v.storageDuration(p+"backups.maxAge", s.Backups.MaxAge, storagespec.MinMaxAge, storagespec.MaxMaxAge)
	first := map[int]int{}
	for i, id := range s.Backups.ExpectedGuests {
		path := fmt.Sprintf("%sbackups.expectedGuests[%d]", p, i)
		if id < storagespec.MinVMID || id > storagespec.MaxVMID {
			v.failf(path, "must be a vmid between %d and %d, got %d", storagespec.MinVMID, storagespec.MaxVMID, id)
			continue
		}
		if j, dup := first[id]; dup {
			v.failf(path, "duplicate of expectedGuests[%d]: %d", j, id)
			continue
		}
		first[id] = i
	}

	v.storagePaths(p+"timeMachine.paths", s.TimeMachine.Paths)
	v.storageDuration(p+"timeMachine.maxAge", s.TimeMachine.MaxAge, storagespec.MinMaxAge, storagespec.MaxMaxAge)
	v.storageDuration(p+"timeMachine.sizeInterval", s.TimeMachine.SizeInterval, storagespec.MinSizeInterval,
		storagespec.MaxSizeInterval)

	t := s.Thresholds
	pcw := v.storagePercent(p+"thresholds.poolCapacityWarn", t.PoolCapacityWarn)
	pcc := v.storagePercent(p+"thresholds.poolCapacityCrit", t.PoolCapacityCrit)
	if pcw && pcc && t.PoolCapacityWarn >= t.PoolCapacityCrit {
		v.failf(p+"thresholds.poolCapacityWarn", "must be less than poolCapacityCrit (%d), got %d",
			t.PoolCapacityCrit, t.PoolCapacityWarn)
	}
	if t.ScrubMaxAgeDays < minStorageScrubMaxDays || t.ScrubMaxAgeDays > maxStorageScrubMaxDays {
		v.failf(p+"thresholds.scrubMaxAgeDays", "must be between %d and %d, got %d", minStorageScrubMaxDays,
			maxStorageScrubMaxDays, t.ScrubMaxAgeDays)
	}
	v.storagePercent(p+"thresholds.quotaWarn", t.QuotaWarn)
	sw := v.storagePercent(p+"thresholds.shareWarn", t.ShareWarn)
	sc := v.storagePercent(p+"thresholds.shareCrit", t.ShareCrit)
	if sw && sc && t.ShareWarn >= t.ShareCrit {
		v.failf(p+"thresholds.shareWarn", "must be less than shareCrit (%d), got %d", t.ShareCrit, t.ShareWarn)
	}

	pools := map[string]int{}
	for i, name := range s.Scrub.Pools {
		path := fmt.Sprintf("%sscrub.pools[%d]", p, i)
		if !storagePoolRE.MatchString(name) {
			v.failf(path, "must be a pool name matching %s, got %q", storagespec.PoolNamePattern, name)
			continue
		}
		if j, dup := pools[name]; dup {
			v.failf(path, "duplicate of pools[%d]: %q", j, name)
			continue
		}
		pools[name] = i
	}

	datasets := map[string]int{}
	for i, d := range s.Snapshots.Datasets {
		path := fmt.Sprintf("%ssnapshots.datasets[%d]", p, i)
		if d.Name == "" {
			v.fail(path+".name", "required: the dataset snapshots may be taken of")
			continue
		}
		if !storageDatasetName(d.Name) {
			v.failf(path+".name", "must be a dataset name matching %s, with no \".\" or \"..\" component and at "+
				"most %d bytes, got %q", storagespec.DatasetNamePattern, storagespec.MaxNameLength, d.Name)
			continue
		}
		if j, dup := datasets[d.Name]; dup {
			v.failf(path, "duplicate of datasets[%d]: %q", j, d.Name)
			continue
		}
		datasets[d.Name] = i
	}
	v.storagePercent(p+"snapshots.maxPoolCapacityPercent", s.Snapshots.MaxPoolCapacityPercent)
}

// storageDatasetName reports whether name is a dataset name the spec accepts.
func storageDatasetName(name string) bool {
	return len(name) <= storagespec.MaxNameLength && storageDatasetRE.MatchString(name) &&
		!storageDatasetNotRE.MatchString(name)
}

// storagePaths requires each path to be absolute, clean, not / and listed
// once. A path that Clean would change, such as /media/../etc or a trailing
// slash, is refused rather than cleaned, so the provider reads exactly what
// the file says.
func (v validator) storagePaths(path string, paths []string) {
	key := path[strings.LastIndexByte(path, '.')+1:]
	first := map[string]int{}
	for i, dir := range paths {
		p := fmt.Sprintf("%s[%d]", path, i)
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
			v.failf(p, "must be an absolute, clean path other than /, got %q", dir)
			continue
		}
		if j, dup := first[dir]; dup {
			v.failf(p, "duplicate of %s[%d]: %q", key, j, dir)
			continue
		}
		first[dir] = i
	}
}

// storageDuration requires lo <= d <= hi, both bounds inclusive (A-56).
func (v validator) storageDuration(path string, d, lo, hi time.Duration) {
	if d < lo || d > hi {
		v.failf(path, "must be between %s and %s, got %s", storageDurationText(lo), storageDurationText(hi),
			storageDurationText(d))
	}
}

// storagePercent requires a percentage of 1..100 and reports whether it is.
func (v validator) storagePercent(path string, n int) bool {
	if n < minStoragePercent || n > maxStoragePercent {
		v.failf(path, "must be between %d and %d, got %d", minStoragePercent, maxStoragePercent, n)
		return false
	}
	return true
}

// storageDurationText spells d as the storage section's comments do: whole
// hours as 192h, whole minutes of ten or more as 10m, and whole seconds
// below that as 120s. Anything else is left to String.
func storageDurationText(d time.Duration) string {
	switch {
	case d != 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= 10*time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d != 0 && d < 10*time.Minute && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// shortDuration spells d as it is written in the file: whole hours as 24h
// and whole seconds as 60s, where time.Duration.String gives 24h0m0s and
// 1m0s. Anything else, such as 250ms, is left to String.
func shortDuration(d time.Duration) string {
	switch {
	case d != 0 && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d != 0 && d%time.Second == 0:
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

// PiholeDNSDir is the directory dnsmasq, run by Pi-hole FTL, loads extra
// configuration from. providers.pihole.dnsFile must be directly in it.
const PiholeDNSDir = "/etc/dnsmasq.d"

// piholeManagedFiles are the files Pi-hole itself writes in PiholeDNSDir,
// which Nexus must never own.
var piholeManagedFiles = []string{
	"01-pihole.conf",
	"02-pihole-dhcp.conf",
	"04-pihole-static-dhcp.conf",
	"05-pihole-custom-cname.conf",
	"06-rfc6761.conf",
}

// piholeDNSFile requires a file dnsmasq loads and Pi-hole does not manage:
// directly in PiholeDNSDir once cleaned, and not a name dnsmasq's conf-dir
// skips, which is one starting with ".", ending with "~", or both starting
// and ending with "#", a lone "#" included (V-11: FTL src/dnsmasq/option.c).
func (v validator) piholeDNSFile(path, p string) {
	if !filepath.IsAbs(p) {
		v.failf(path, "must be an absolute path, got %q", p)
		return
	}
	clean := filepath.Clean(p)
	base := filepath.Base(clean)
	switch {
	case filepath.Dir(clean) != PiholeDNSDir:
		v.failf(path, "must be a file directly in %s, got %q", PiholeDNSDir, p)
	case strings.HasPrefix(base, "."), strings.HasSuffix(base, "~"),
		strings.HasPrefix(base, "#") && strings.HasSuffix(base, "#"):
		v.failf(path, "%q is a name dnsmasq skips in %s: it must not start with \".\", end with \"~\", "+
			"or both start and end with \"#\"", base, PiholeDNSDir)
	case slices.Contains(piholeManagedFiles, base):
		v.failf(path, "%q is managed by Pi-hole and must not be used, got %q", base, p)
	}
}

// catalog applies the Configuration rules for script entries, located in the
// file. Catalog-wide semantics beyond these belong to the scripts provider.
func (v validator) catalog(entries []Script) {
	CheckCatalog(entries, v.fail)
}

var (
	scriptIDRE      = regexp.MustCompile(spec.IDPattern)
	parameterNameRE = regexp.MustCompile(spec.ParameterNamePattern)
	envNameRE       = regexp.MustCompile(EnvNamePattern)
)

// EnvNamePattern is what an environment variable name in the catalog must
// match: a portable shell name, so it cannot carry "=" or a NUL.
const EnvNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`

// AnchoredPattern is the pattern that matches only a value the whole of
// which matches pattern. A parameter's pattern is applied through it
// wherever it is applied, here and in the scripts provider: an unanchored
// one would let "--upload-pack=x" through a pattern meant to keep out
// leading dashes. Compile pattern alone first: a valid pattern stays valid
// when wrapped, but "a)(b" only becomes valid by it.
func AnchoredPattern(pattern string) string { return "^(?:" + pattern + ")$" }

// CheckCatalog applies the validate-config rules for script catalog entries
// and calls fail once per problem, with the key path under
// providers.scripts.catalog. validate-config and the scripts provider's
// Configure both use it, so the two cannot disagree. Env values can be
// secrets and are never part of a message.
func CheckCatalog(entries []Script, fail func(path, msg string)) {
	failf := func(path, format string, args ...any) { fail(path, fmt.Sprintf(format, args...)) }
	required := func(path, value string) bool {
		if value == "" {
			fail(path, "required")
			return false
		}
		return true
	}
	absolute := func(path, value string) {
		if required(path, value) && !filepath.IsAbs(value) {
			failf(path, "must be an absolute path, got %q", value)
		}
	}
	ids := map[string]bool{}
	for i, s := range entries {
		p := fmt.Sprintf("providers.scripts.catalog[%d]", i)
		switch {
		case !required(p+".id", s.ID):
		case !scriptIDRE.MatchString(s.ID):
			failf(p+".id", "must match %s, got %q", spec.IDPattern, s.ID)
		case ids[s.ID]:
			failf(p+".id", "duplicate script id %q", s.ID)
		}
		ids[s.ID] = true
		required(p+".description", s.Description)
		absolute(p+".path", s.Path)
		required(p+".runAs", s.RunAs)
		absolute(p+".workingDirectory", s.WorkingDirectory)
		// Zero means the provider's default.
		if s.Timeout < 0 {
			failf(p+".timeout", "must not be negative, got %s", s.Timeout)
		}
		if s.TerminationGrace < 0 {
			failf(p+".terminationGrace", "must not be negative, got %s", s.TerminationGrace)
		}
		if s.Concurrency < 0 {
			failf(p+".concurrency", "must not be negative, got %d", s.Concurrency)
		}
		names := make([]string, 0, len(s.Env))
		for name := range s.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		for i, name := range names {
			if !envNameRE.MatchString(name) {
				failf(p+".env", "%s must match %s", envNameRef(name, i), EnvNamePattern)
			}
		}

		params := map[string]bool{}
		for j, prm := range s.Parameters {
			pp := fmt.Sprintf("%s.parameters[%d]", p, j)
			switch {
			case !required(pp+".name", prm.Name):
			case !parameterNameRE.MatchString(prm.Name):
				failf(pp+".name", "must match %s, got %q", spec.ParameterNamePattern, prm.Name)
			case params[prm.Name]:
				failf(pp+".name", "duplicate parameter name %q", prm.Name)
			}
			params[prm.Name] = true
			typeOK := true
			switch prm.Type {
			case ParamString, ParamInteger, ParamBoolean:
			case "":
				fail(pp+".type", "required: one of string, integer or boolean")
				typeOK = false
			default:
				failf(pp+".type", "must be one of string, integer or boolean, got %q", prm.Type)
				typeOK = false
			}
			// The catalog author decides about leading dashes and
			// metacharacters, so a string parameter must say what it accepts:
			// a pattern, or the list of its values.
			if prm.Type == ParamString && prm.Pattern == "" && len(prm.Enum) == 0 {
				fail(pp+".pattern", "required for a string parameter without an enum")
			}
			var re *regexp.Regexp
			patternOK := true
			if prm.Pattern != "" {
				// Compiled bare first, so the message quotes the pattern as
				// written, and because a pattern like "a)(b" is invalid alone
				// but compiles once wrapped. Then as it is applied, to the
				// whole value (SEC-3).
				if _, err := regexp.Compile(prm.Pattern); err != nil {
					failf(pp+".pattern", "not a valid regular expression: %v", err)
					patternOK = false
				} else {
					re = regexp.MustCompile(AnchoredPattern(prm.Pattern))
				}
			}
			values := map[string]bool{}
			for k, v := range prm.Enum {
				ep := fmt.Sprintf("%s.enum[%d]", pp, k)
				switch {
				case values[v]:
					fail(ep, "duplicate enum value")
				case typeOK && patternOK:
					if msg := CheckParameterValue(prm.Type, re, nil, v); msg != "" {
						fail(ep, msg)
					}
				}
				values[v] = true
			}
			if prm.Env != "" && !envNameRE.MatchString(prm.Env) {
				failf(pp+".env", "must match %s, got %q", EnvNamePattern, prm.Env)
			}
			// argv is visible in /proc, so sensitive values travel by env.
			if prm.Sensitive && prm.Env == "" {
				fail(pp+".env", "required because the parameter is sensitive")
			}
			if prm.Sensitive && prm.Arg != "" {
				fail(pp+".arg", "not allowed because the parameter is sensitive")
			}
		}
	}
}

// envNameRef refers to an invalid script environment variable name in a
// message. A name is a key, so it is quoted, except when it holds "=": a
// NAME=value pair written as a flow-mapping key puts the value, a secret, in
// the name, and a report any nexusd:read caller can fetch would echo it. Such
// a name is cut at the "=" (the part before it is the name the author meant),
// or, if that part is not a short valid name, identified by its position
// among the sorted names.
func envNameRef(name string, index int) string {
	if !strings.Contains(name, "=") {
		return fmt.Sprintf("variable name %q", name)
	}
	if k, _, _ := strings.Cut(name, "="); len(k) <= 64 && envNameRE.MatchString(k) {
		return fmt.Sprintf("variable name %q is followed by a value (write NAME: value, not a NAME=value pair), and the name", k)
	}
	return fmt.Sprintf("variable name #%d in name order", index+1)
}

// Parameter types a script catalog may declare.
const (
	ParamString  = "string"
	ParamInteger = "integer"
	ParamBoolean = "boolean"
)

var integerRE = regexp.MustCompile(`^-?[0-9]+$`)

// CheckParameterValue reports why v is not a value of a parameter of type
// typ, with the declared enum (nil: any) and the compiled pattern re (nil:
// none), or "" when it is. validate-config checks enum values with it, and
// the scripts provider every invocation, so the two cannot disagree. The
// message never quotes v, the pattern or the enum: the parameter may be
// sensitive.
func CheckParameterValue(typ string, re *regexp.Regexp, enum []string, v string) string {
	// argv and environment entries are C strings.
	if strings.IndexByte(v, 0) >= 0 {
		return "must not contain a NUL byte"
	}
	switch typ {
	case ParamInteger:
		if !integerRE.MatchString(v) {
			return "must be an integer in decimal digits"
		}
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return "must be an integer that fits in 64 bits"
		}
	case ParamBoolean:
		if v != "true" && v != "false" {
			return "must be true or false"
		}
	case ParamString:
	default:
		return fmt.Sprintf("has an unknown declared type %q", typ)
	}
	if enum != nil && !slices.Contains(enum, v) {
		return "is not one of the declared values"
	}
	if re != nil && !re.MatchString(v) {
		return "does not match the declared pattern"
	}
	return ""
}

type validator struct {
	errs *collector
	locs locations
}

// fail locates the error at the value, or at the nearest enclosing value in
// the file when the key itself is absent and its default is at fault.
func (v validator) fail(path, msg string) {
	pos, ok := v.locs[path]
	for p := path; !ok && p != ""; {
		p = parentPath(p)
		pos, ok = v.locs[p]
	}
	v.errs.add(pos.line, pos.col, path, msg)
}

func parentPath(p string) string {
	if i := strings.LastIndexAny(p, ".["); i >= 0 {
		return p[:i]
	}
	return ""
}

func (v validator) failf(path, format string, args ...any) {
	v.fail(path, fmt.Sprintf(format, args...))
}

func (v validator) positive(path string, n int64) {
	if n <= 0 {
		v.failf(path, "must be greater than zero, got %d", n)
	}
}

func (v validator) positiveDuration(path string, d time.Duration) {
	if d <= 0 {
		v.failf(path, "must be greater than zero, got %s", d)
	}
}

// trustedOwners requires each name to be a user on this host, as the docker
// provider resolves them to UIDs when it is configured. root is a user, so
// listing it passes, and changes nothing.
func (v validator) trustedOwners(path string, names []string) {
	for i, name := range names {
		p := fmt.Sprintf("%s[%d]", path, i)
		if name == "" {
			v.fail(p, "must not be empty")
			continue
		}
		if _, err := user.Lookup(name); err != nil {
			var unknown user.UnknownUserError
			if errors.As(err, &unknown) {
				v.failf(p, "unknown user %q: no such user on this host. Fix: create the user, or remove it from "+
					"the list", name)
			} else {
				v.failf(p, "unknown user %q: it cannot be looked up: %v", name, err)
			}
		}
	}
}

func (v validator) absolute(path, p string) {
	if !filepath.IsAbs(p) {
		v.failf(path, "must be an absolute path, got %q", p)
	}
}

// tlsFiles loads the configured pair as startup will, so validate-config
// reports a missing, unparsable or mismatched pair.
func (v validator) tlsFiles(certPath, keyPath string) {
	certPEM, certErr := os.ReadFile(certPath)
	if certErr != nil {
		v.failf("tls.certFile", "cannot read %s: %v", certPath, unwrapPath(certErr))
	}
	keyPEM, keyErr := os.ReadFile(keyPath)
	if keyErr != nil {
		v.failf("tls.keyFile", "cannot read %s: %v", keyPath, unwrapPath(keyErr))
	}
	if certErr != nil || keyErr != nil {
		return
	}
	if err := checkCertificatePEM(certPEM); err != nil {
		v.failf("tls.certFile", "%s: %v", certPath, err)
		return
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		v.failf("tls.keyFile", "cannot use %s with %s: %v", keyPath, certPath, err)
	}
}

func checkCertificatePEM(data []byte) error {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return errors.New("no PEM CERTIFICATE block")
		}
		if block.Type == "CERTIFICATE" {
			_, err := x509.ParseCertificate(block.Bytes)
			return err
		}
	}
}

// isLoopbackListen reports whether a valid listen address is a loopback IP.
// A host name, localhost included, is not: what it resolves to is outside
// the file.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.Unmap().IsLoopback()
}

func checkListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a host:port address", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host; use 0.0.0.0 or [::] to mean every interface", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has an invalid port", addr)
	}
	return nil
}
