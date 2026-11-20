// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"strconv"
	"strings"
)

// Mount is one line of /proc/<pid>/mountinfo (proc_pid_mountinfo(5)). Root,
// MountPoint and Source have the kernel's octal escapes decoded. Options are
// the per-mount options; Optional the tagged fields before the separator
// (shared:N, master:N, ...), nil when there are none; SuperOptions the
// per-superblock options as printed.
type Mount struct {
	ID, ParentID int
	Major, Minor uint32
	Root         string
	MountPoint   string
	Options      []string
	Optional     []string
	FSType       string
	Source       string
	SuperOptions string
}

// Mountinfo parses /proc/<pid>/mountinfo. Fields are split on single
// spaces, since the kernel escapes the space, tab, newline and backslash in
// paths (fs/proc_namespace.c show_mountinfo); the optional fields end at the
// first lone "-" from field 7 on. At least one mount is the required anchor.
func Mountinfo(b []byte) ([]Mount, error) {
	var out []Mount
	for l := range lines(b) {
		if m, ok := mountLine(l); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("mountinfo: no mount line")
	}
	return out, nil
}

func mountLine(l string) (Mount, bool) {
	f := strings.Split(l, " ")
	if len(f) < 10 {
		return Mount{}, false
	}
	sep := -1
	for i := 6; i < len(f); i++ {
		if f[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || len(f) < sep+4 {
		return Mount{}, false
	}
	id, err1 := strconv.Atoi(f[0])
	parent, err2 := strconv.Atoi(f[1])
	maj, mn, ok := strings.Cut(f[2], ":")
	major, err3 := strconv.ParseUint(maj, 10, 32)
	minor, err4 := strconv.ParseUint(mn, 10, 32)
	if err1 != nil || err2 != nil || !ok || err3 != nil || err4 != nil || id < 0 || parent < 0 || f[4] == "" {
		return Mount{}, false
	}
	m := Mount{
		ID: id, ParentID: parent, Major: uint32(major), Minor: uint32(minor),
		Root:         unescapeOctal(f[3]),
		MountPoint:   unescapeOctal(f[4]),
		Options:      strings.Split(f[5], ","),
		FSType:       f[sep+1],
		Source:       unescapeOctal(f[sep+2]),
		SuperOptions: f[sep+3],
	}
	if sep > 6 {
		m.Optional = append([]string(nil), f[6:sep]...)
	}
	return m, true
}

// unescapeOctal decodes the kernel's \ooo escapes (seq_escape: \040 space,
// \011 tab, \012 newline, \134 backslash, \043 #). Anything else, including
// a backslash not followed by three octal digits of at most \377, is kept as
// is.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && isOctal(s[i+2]) && isOctal(s[i+3]) {
			sb.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
