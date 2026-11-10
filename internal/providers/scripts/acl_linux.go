package scripts

import (
	"errors"
	"syscall"
)

// systemACL returns the raw system.posix_acl_access attribute of path, or nil
// when it has none or the file system keeps no ACLs. The attribute is read
// with a size probe and retried if an ACL changes between the two calls.
func systemACL(path string) ([]byte, error) {
	const attr = "system.posix_acl_access"
	for range 3 {
		n, err := syscall.Getxattr(path, attr, nil)
		if err != nil {
			return nil, aclAbsent(err)
		}
		buf := make([]byte, n)
		n, err = syscall.Getxattr(path, attr, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue
		}
		if err != nil {
			return nil, aclAbsent(err)
		}
		return buf[:n], nil
	}
	return nil, syscall.ERANGE
}

// aclAbsent maps "no ACL here" answers to nil, and keeps every other error.
func aclAbsent(err error) error {
	if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
