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
