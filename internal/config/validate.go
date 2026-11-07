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
	"strconv"
	"strings"
	"time"
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

// catalog applies the Configuration rules for script entries. Catalog-wide
// semantics beyond these belong to the scripts provider.
func (v validator) catalog(entries []Script) {
	ids := map[string]bool{}
	for i, s := range entries {
		p := fmt.Sprintf("providers.scripts.catalog[%d]", i)
		switch {
		case s.ID == "":
			v.fail(p+".id", "required")
		case ids[s.ID]:
			v.failf(p+".id", "duplicate script id %q", s.ID)
		}
		ids[s.ID] = true
		v.absolute(p+".path", s.Path)
		if s.WorkingDirectory != "" {
			v.absolute(p+".workingDirectory", s.WorkingDirectory)
		}
		v.nonNegativeDuration(p+".timeout", s.Timeout)
		v.nonNegativeDuration(p+".terminationGrace", s.TerminationGrace)
		v.nonNegative(p+".concurrency", int64(s.Concurrency))

		names := map[string]bool{}
		for j, prm := range s.Parameters {
			pp := fmt.Sprintf("%s.parameters[%d]", p, j)
			switch {
			case prm.Name == "":
				v.fail(pp+".name", "required")
			case names[prm.Name]:
				v.failf(pp+".name", "duplicate parameter name %q", prm.Name)
			}
			names[prm.Name] = true
			// The catalog author decides about leading dashes and
			// metacharacters, so a string parameter must say what it accepts.
			if prm.Type == "string" && prm.Pattern == "" {
				v.fail(pp+".pattern", "required for a string parameter")
			}
			if prm.Pattern != "" {
				if _, err := regexp.Compile(prm.Pattern); err != nil {
					v.failf(pp+".pattern", "not a valid regular expression: %v", err)
				}
			}
			// argv is visible in /proc, so sensitive values travel by env.
			if prm.Sensitive && prm.Env == "" {
				v.fail(pp+".env", "required because the parameter is sensitive")
			}
			if prm.Sensitive && prm.Arg != "" {
				v.fail(pp+".arg", "not allowed because the parameter is sensitive")
			}
		}
	}
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

func (v validator) nonNegativeDuration(path string, d time.Duration) {
	if d < 0 {
		v.failf(path, "must not be negative, got %s", d)
	}
}

func (v validator) nonNegative(path string, n int64) {
	if n < 0 {
		v.failf(path, "must not be negative, got %d", n)
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
