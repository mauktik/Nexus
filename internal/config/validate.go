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
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/providers/scripts/spec"
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

	apt := c.Providers.Apt.Settings
	v.positiveDuration("providers.apt.lockWait", apt.LockWait)
	v.positiveDuration("providers.apt.maxRuntime", apt.MaxRuntime)

	v.catalog(c.Providers.Scripts.Settings.Catalog)
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
		for _, name := range names {
			if !envNameRE.MatchString(name) {
				failf(p+".env", "variable name %q must match %s", name, EnvNamePattern)
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
				var err error
				if re, err = regexp.Compile(prm.Pattern); err != nil {
					failf(pp+".pattern", "not a valid regular expression: %v", err)
					patternOK = false
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
