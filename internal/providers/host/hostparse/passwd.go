// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"strconv"
	"strings"
)

// Passwd maps each uid in /etc/passwd to its login name. A line is
// name:password:UID:GID:GECOS:directory:shell (passwd(5)); a line without
// seven fields, with an empty name, a NIS compat entry ("+" or "-" name), a
// comment or a uid that is not a 32-bit decimal is skipped. When two lines
// share a uid the first wins, as getpwuid(3) returns the first match.
func Passwd(b []byte) map[uint32]string {
	out := map[uint32]string{}
	for l := range lines(b) {
		f := strings.Split(l, ":")
		if len(f) != 7 {
			continue
		}
		name := f[0]
		if name == "" || strings.HasPrefix(name, "#") || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		if _, dup := out[uint32(uid)]; dup {
			continue
		}
		out[uint32(uid)] = strings.ToValidUTF8(name, "�")
	}
	return out
}
