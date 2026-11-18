package svcctl

import (
	"fmt"

	"github.com/mauktik/nexus/internal/platform"
)

// rebootPath is the absolute path of OpenRC's reboot (busybox on Alpine,
// A-5).
const rebootPath = "/sbin/reboot"

// RebootArgv is the fixed argv that reboots the host: `systemctl reboot` on
// systemd and `/sbin/reboot` on OpenRC, with no other argument (no -f: the
// init system runs its shutdown). Any other init system is an error naming
// it, wrapping ErrNotStarted. Each call returns a new slice.
func RebootArgv(init platform.InitSystem) ([]string, error) {
	switch init {
	case platform.Systemd:
		return []string{systemctlPath, "reboot"}, nil
	case platform.OpenRC:
		return []string{rebootPath}, nil
	}
	return nil, fmt.Errorf("%w: no reboot command for init system %q", ErrNotStarted, string(init))
}
