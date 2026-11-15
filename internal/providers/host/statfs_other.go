//go:build !linux

package host

import "runtime"

// defaultStatfs is the Statfs seam of hosts without Linux statfs. The
// provider is Linux only; this keeps the package building on the macOS dev
// host.
func defaultStatfs(string) (Usage, error) {
	return Usage{}, errNotSupported("statfs on " + runtime.GOOS)
}
