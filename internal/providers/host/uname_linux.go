// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package host

import (
	"io/fs"
	"syscall"
)

// defaultUname returns uname(2)'s machine field, e.g. x86_64 or aarch64.
func defaultUname() (string, error) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", &fs.PathError{Op: "uname", Path: "machine", Err: err}
	}
	return utsString(u.Machine[:]), nil
}

// utsString converts a NUL-terminated utsname field. Its element type is
// int8 on some architectures (amd64, arm64) and uint8 on others (arm,
// riscv64, s390x; syscall/ztypes_linux_*.go).
func utsString[T int8 | uint8](field []T) string {
	b := make([]byte, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
