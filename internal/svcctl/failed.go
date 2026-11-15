package svcctl

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/platform"
)

// rcStatusPath is the absolute path of rc-status (A-17, testdata/README.md).
const rcStatusPath = "/bin/rc-status"

// unitNamePattern is what ListFailed accepts as a systemd unit name.
const unitNamePattern = `^[A-Za-z0-9@._:\-]+\.[a-z]+$`

// unitNameMax is the longest unit name systemd accepts (testdata/README.md:
// 255 characters are accepted, 256 refused).
const unitNameMax = 255

var unitName = regexp.MustCompile(unitNamePattern)

// FailedServices is what the init system reports failed.
type FailedServices struct {
	Names []string // Sorted, each matching unitNamePattern (systemd) or ServiceNamePattern (OpenRC); never nil without an error
	Count int      // len(Names) before any caller-side cut
}

// ListFailed lists failed units (systemd: systemctl list-units --state=failed
// --plain --no-legend --no-pager --full, first field of each line) or
// crashed services (OpenRC: rc-status --crashed, one name per line; exit 1
// with empty output means none). Lines that are not a valid name are
// dropped. opts.Run and opts.Timeout apply; the other options are unused.
// An init system other than systemd or OpenRC is an error wrapping
// ErrNotStarted, and nothing is run.
//
// The architecture calls it Failed, which the package's State constant
// Failed already names.
func ListFailed(ctx context.Context, init platform.InitSystem, opts Options) (FailedServices, error) {
	var argv []string
	switch init {
	case platform.Systemd:
		argv = []string{systemctlPath, "list-units", "--state=failed", "--plain", "--no-legend", "--no-pager", "--full"}
	case platform.OpenRC:
		argv = []string{rcStatusPath, "--crashed"}
	default:
		return FailedServices{}, fmt.Errorf("%w: no failed-service listing for init system %q", ErrNotStarted, string(init))
	}
	res, err := newBase(opts).call(ctx, argv)
	if err != nil {
		return FailedServices{}, err
	}
	var names []string
	switch {
	case init == platform.OpenRC && res.ExitCode == 1 && strings.TrimSpace(string(res.Output)) == "":
		// rc-status exits 1 with no output when nothing crashed (A-16).
	case res.ExitCode != 0:
		return FailedServices{}, fmt.Errorf("svcctl: %s: exit %d", strings.Join(argv, " "), res.ExitCode)
	case init == platform.Systemd:
		names = systemdFailedNames(string(res.Output))
	default:
		names = openrcCrashedNames(string(res.Output))
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if names == nil {
		names = []string{}
	}
	return FailedServices{Names: names, Count: len(names)}, nil
}

// systemdFailedNames is the first field of each line of out that is a valid
// unit name. Any other line, such as a warning on stderr, is dropped.
func systemdFailedNames(out string) []string {
	var names []string
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) > 0 && len(fields[0]) <= unitNameMax && unitName.MatchString(fields[0]) {
			names = append(names, fields[0])
		}
	}
	return names
}

// openrcCrashedNames is each line of out that is a valid service name.
func openrcCrashedNames(out string) []string {
	var names []string
	for line := range strings.Lines(out) {
		if n := strings.TrimSpace(line); serviceName.MatchString(n) {
			names = append(names, n)
		}
	}
	return names
}
