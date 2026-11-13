package detach

import (
	"fmt"
	"path/filepath"

	"github.com/mauktik/nexus/internal/platform"
)

// PlatformOptions returns the launcher seams for platform p. On systemd, and
// on a host with no supported init system, it is the zero Options, so New
// selects the real systemd-run, systemctl show, systemctl kill and
// systemctl list-units exactly as before. On OpenRC it is the process
// backend (FR-20, CON-11).
func PlatformOptions(p platform.Info, cfg ProcessConfig) Options {
	if p.Init == platform.OpenRC {
		return ProcessBackend(cfg)
	}
	return Options{}
}

// Available reports whether detached operations can run on this host,
// whatever its init system: systemd's own checks on a systemd host, the
// process backend's on OpenRC, and unavailable anywhere else. Providers
// whose operations are detached report unavailable with the reason when it
// is not (FR-26).
func Available() Availability { return availableAt(platform.Current(), "/proc") }

// availableAt is Available for platform info, with /proc at procRoot.
// Platform detection reports OpenRC on Linux only. cgroup v2 is not
// required: without it the supervisors run uncontained (CON-10).
func availableAt(info platform.Info, procRoot string) Availability {
	switch info.Init {
	case platform.Systemd:
		return SystemdAvailable()
	case platform.OpenRC:
		if _, err := bootID(procRoot); err != nil {
			return Availability{Reason: fmt.Sprintf(
				"detached operations on OpenRC record each supervisor's boot from %s, which cannot be read: %v",
				filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"), err)}
		}
		return Availability{Available: true}
	default:
		return Availability{Reason: "no supported init system (systemd or OpenRC) was detected; detached operations need one"}
	}
}
