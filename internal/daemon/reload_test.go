package daemon

import (
	"slices"
	"testing"

	"github.com/mauktik/nexus/internal/config"
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
