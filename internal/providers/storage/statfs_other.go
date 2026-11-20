//go:build !linux

package storage

import (
	"errors"
	"runtime"
)

// defaultStatfs is the Statfs seam of hosts without Linux statfs. The
// provider is Linux only (Preflight refuses any other GOOS, CON-3); this
// keeps the package building on the macOS development host.
func defaultStatfs(string) (Usage, error) {
	return Usage{}, errors.New("statfs is not supported on " + runtime.GOOS)
}
