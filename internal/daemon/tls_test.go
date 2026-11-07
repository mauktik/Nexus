package daemon

import (
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/config"
)

// With TLS configured, the daemon refuses to start without a certificate
// rather than fall back to plain HTTP.
func TestNewRequiresCertificateUnlessTLSOff(t *testing.T) {
	cfg := config.Default()
	for _, mode := range []string{config.TLSAuto, config.TLSFiles} {
		cfg.TLS.Mode = mode
		_, err := New(&cfg, Options{AuditDir: t.TempDir(), StateDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "no certificate") {
			t.Errorf("tls.mode %s without a certificate: %v, want a refusal", mode, err)
		}
	}
}
