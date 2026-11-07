package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach/detachtest"
	"github.com/mauktik/nexus/internal/provider"
)

// The authenticator is built once at startup, so a changed
// auth.failureLimitPerMinute must be reported as needing a restart rather
// than silently kept.
func TestRestartRequiredReportsAuthChanges(t *testing.T) {
	running := config.Default()
	same := config.Default()
	if keys := restartRequired(&running, &same); len(keys) != 0 {
		t.Fatalf("identical configurations: %v, want none", keys)
	}
	next := config.Default()
	next.Auth.FailureLimitPerMinute = 3
	if keys := restartRequired(&running, &next); !slices.Equal(keys, []string{"auth"}) {
		t.Fatalf("changed auth.failureLimitPerMinute: %v, want [auth]", keys)
	}
}

// The audit log is opened once at startup with its retention, so a changed
// audit.retentionDays must be reported as needing a restart.
func TestRestartRequiredReportsAuditChanges(t *testing.T) {
	running := config.Default()
	next := config.Default()
	next.Audit.RetentionDays = 30
	if keys := restartRequired(&running, &next); !slices.Equal(keys, []string{"audit"}) {
		t.Fatalf("changed audit.retentionDays: %v, want [audit]", keys)
	}
}

// sectionProvider records the last section it accepted. It takes the name
// of a real configuration section and decodes it into that section's type,
// so the real loader and the real reload path run. A docker section whose
// socket is rejectSocket is refused, as a provider may refuse what
// validate-config accepts.
type sectionProvider struct {
	name     string
	accepted atomic.Value // string: the field the test changes
}

const rejectSocket = "/refused-by-provider.sock"

func (p *sectionProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read", Ops: []def.Op{probeOps[p.name].Op()}}
}

func (p *sectionProvider) Configure(decode func(any) error) error {
	switch p.name {
	case "apt":
		var c config.AptConfig
		if err := decode(&c); err != nil {
			return err
		}
		p.accepted.Store(c.LockWait.String())
	case "docker":
		var c config.DockerConfig
		if err := decode(&c); err != nil {
			return err
		}
		if c.Socket == rejectSocket {
			return fmt.Errorf("providers.docker.socket: refused")
		}
		p.accepted.Store(c.Socket)
	}
	return nil
}

func (p *sectionProvider) Preflight(context.Context) provider.Status { return ok() }

func (p *sectionProvider) Bindings() []provider.Binding {
	return []provider.Binding{provider.Sync(probeOps[p.name], func(_ context.Context, in probeIn) (probeOut, error) {
		return probeOut(in), nil
	})}
}

// A reload is all or nothing across providers: when one provider refuses
// its section, a provider that accepted its new section first is put back
// to the configuration in force, which is the last one applied, not the
// one nexusd started with.
func TestReloadIsAllOrNothingAcrossProviders(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	write := func(lockWait, socket string) {
		t.Helper()
		body := fmt.Sprintf("listen: [\"127.0.0.1:7443\"]\ntls: {mode: off}\nproviders:\n"+
			"  apt: {enabled: true, lockWait: %s}\n  docker: {enabled: true, socket: %s}\n", lockWait, socket)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("100s", "/run/docker.sock")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	apt, docker := &sectionProvider{name: "apt"}, &sectionProvider{name: "docker"}
	d, err := New(cfg, Options{Providers: []provider.Provider{apt, docker}, ConfigPath: cfgPath,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), StateDir: t.TempDir(), AuditDir: t.TempDir(),
		Detach: detachtest.Local()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.store.Close(); err != nil {
			t.Error(err)
		}
		if err := d.audit.Close(); err != nil {
			t.Error(err)
		}
	})
	check := func(step, wantApt, wantDocker string) {
		t.Helper()
		if got := apt.accepted.Load(); got != wantApt {
			t.Errorf("%s: apt section in force %v, want %s", step, got, wantApt)
		}
		if got := docker.accepted.Load(); got != wantDocker {
			t.Errorf("%s: docker section in force %v, want %s", step, got, wantDocker)
		}
	}
	check("startup", "1m40s", "/run/docker.sock")

	ctx := context.Background()
	write("200s", rejectSocket)
	d.reload(ctx)
	check("docker refused", "1m40s", "/run/docker.sock")

	write("300s", "/var/run/docker.sock")
	d.reload(ctx)
	check("accepted", "5m0s", "/var/run/docker.sock")

	write("400s", rejectSocket)
	d.reload(ctx)
	check("refused after an accepted reload", "5m0s", "/var/run/docker.sock")
}
