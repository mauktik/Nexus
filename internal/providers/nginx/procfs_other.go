//go:build !linux

package nginx

import "errors"

// errNoProc is what reading processes gives where there is no Linux /proc.
var errNoProc = errors.New("reload verification needs Linux /proc")

// noProc is the ProcReader of hosts without Linux /proc. nginx is managed
// only on Linux; this keeps the package building on the macOS dev host.
type noProc struct{}

func (noProc) Snapshot() (map[int]Proc, error) { return nil, errNoProc }
func (noProc) Exe(int) (string, error)         { return "", errNoProc }
func (noProc) RealUID(int) (uint32, error)     { return 0, errNoProc }

// HostProcReader reads the host's processes. On this platform every read
// fails with "reload verification needs Linux /proc".
func HostProcReader() ProcReader { return noProc{} }
