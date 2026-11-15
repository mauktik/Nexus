//go:build !linux

package host

import "runtime"

// defaultUname is the Uname seam of hosts without Linux uname(2).
func defaultUname() (string, error) {
	return "", errNotSupported("uname on " + runtime.GOOS)
}
