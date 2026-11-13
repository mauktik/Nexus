//go:build !linux

package detach

import (
	"context"
	"errors"
	"syscall"
)

// errNeedLinux is every process seam's answer off Linux: the identity,
// session and cgroup handling read /proc and the cgroup v2 hierarchy.
var errNeedLinux = errors.New("detach: detached processes need Linux")

// ProcessBackend returns seams that fail on this host (OOS-12).
func ProcessBackend(ProcessConfig) Options {
	return Options{
		Launch:    func(context.Context, Invocation) error { return errNeedLinux },
		UnitState: func(context.Context, string) (UnitState, error) { return UnitState{}, errNeedLinux },
		Signal:    func(context.Context, string, syscall.Signal) error { return errNeedLinux },
		ListUnits: func(context.Context) ([]string, error) { return nil, errNeedLinux },
	}
}
