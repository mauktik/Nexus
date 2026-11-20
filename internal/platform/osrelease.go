// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxOSReleaseSize bounds how much of an os-release file is read. Real files
// are well under 1 KiB.
const maxOSReleaseSize = 64 << 10

// readOSRelease reads <root>/etc/os-release, falling back to
// <root>/usr/lib/os-release only when the former does not exist, as
// os-release(5) directs. Any other failure gives an empty OS: identification
// is best effort and never fails detection.
func readOSRelease(root string) OS {
	for _, rel := range []string{"etc/os-release", "usr/lib/os-release"} {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return OS{}
		}
		o := parseOSRelease(io.LimitReader(f, maxOSReleaseSize))
		_ = f.Close() // read-only; a close error loses nothing
		return o
	}
	return OS{}
}

// parseOSRelease parses the shell-compatible assignments of os-release(5):
// comments, blank lines, single- and double-quoted or bare values, backslash
// escapes, and CRLF line endings. A repeated key takes its last value.
// Malformed lines are skipped.
func parseOSRelease(r io.Reader) OS {
	var o OS
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		val := unquote(raw)
		switch key {
		case "ID":
			o.ID = val
		case "ID_LIKE":
			o.IDLike = strings.Fields(val)
		case "VERSION_ID":
			o.VersionID = val
		case "PRETTY_NAME":
			o.PrettyName = val
		}
	}
	// A scan error (an over-long line) keeps what was parsed before it.
	return o
}

// unquote returns the value of one shell word. In double quotes a backslash
// escapes only $ ` " \ and is otherwise literal; in single quotes nothing is
// escaped; bare, a backslash escapes any character. Text after the closing
// quote is ignored, since os-release does not allow concatenation.
func unquote(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	switch s[0] {
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return s[1:]
		}
		return s[1 : 1+end]
	case '"':
		for i := 1; i < len(s); i++ {
			c := s[i]
			switch {
			case c == '"':
				return b.String()
			case c == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\", s[i+1]) >= 0:
				i++
				b.WriteByte(s[i])
			default:
				b.WriteByte(c)
			}
		}
		return b.String()
	default:
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				c = s[i]
			}
			b.WriteByte(c)
		}
		return b.String()
	}
}
