// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package svcctl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"
)

// NoLogFileReason is LogFiles.Reason for a service whose scripts declare
// neither output_log nor error_log (nor a logger).
const NoLogFileReason = "this service declares no log file"

// LogFiles are the log files an OpenRC service declares (A-20).
type LogFiles struct {
	Output string // output_log, absolute and clean; "" when none resolves
	Error  string // error_log, likewise
	// Reason says why Output or Error is "": NoLogFileReason, or the
	// declaration that could not be resolved, by file and line. It is ""
	// when both resolved.
	Reason string
}

// scriptCap bounds what OpenRCLogFiles reads of one script.
const scriptCap = 1 << 20

// logAssign is a line assigning one of the four log keys, with what
// follows the = sign.
var logAssign = regexp.MustCompile(`^[ \t]*(output_log|error_log|output_logger|error_logger)=(.*)$`)

// assignment is the last assignment of one key in one file.
type assignment struct {
	where string // "<path>:<line>"
	text  string // The line, without leading blanks
	value string
}

// OpenRCLogFiles statically resolves the output_log and error_log of the
// OpenRC service name from etcRoot/init.d/NAME and etcRoot/conf.d/NAME. No
// script is run. Only literal assignments resolve, quoted or not, with the
// precedence of A-20 (openrc-run sources conf.d before the init script):
//   - the init script's plain assignment wins over conf.d's;
//   - an init script value of exactly ${KEY:-default} takes conf.d's value
//     of KEY, else the default;
//   - $RC_SVCNAME and ${RC_SVCNAME} expand to name;
//   - anything else (another variable, a command, a relative path) does not
//     resolve, and Reason names the line.
//
// A key with no assignment but an output_logger or error_logger does not
// resolve either: the output goes to a program. Reason never carries the
// value of a logger or of any other variable. A name that does not match
// ServiceNamePattern is refused with an error wrapping ErrNotStarted before
// any file is opened; a missing init script is an error wrapping
// fs.ErrNotExist.
func OpenRCLogFiles(etcRoot, name string) (LogFiles, error) {
	if err := checkService(name); err != nil {
		return LogFiles{}, err
	}
	initPath := filepath.Join(etcRoot, "init.d", name)
	initd, err := readAssignments(initPath, true)
	if err != nil {
		return LogFiles{}, err
	}
	confd, err := readAssignments(filepath.Join(etcRoot, "conf.d", name), false)
	if err != nil {
		return LogFiles{}, err
	}
	var lf LogFiles
	var reasons []string
	declared := false
	for _, key := range []string{"output_log", "error_log"} {
		path, reason, declares := resolveLog(key, initd, confd, name)
		declared = declared || declares
		if reason != "" {
			reasons = append(reasons, reason)
		}
		if key == "output_log" {
			lf.Output = path
		} else {
			lf.Error = path
		}
	}
	switch {
	case !declared:
		lf.Reason = NoLogFileReason
	case len(reasons) > 0:
		lf.Reason = strings.Join(reasons, "; ")
	}
	return lf, nil
}

// readAssignments reads the last assignment of each log key in path. A
// missing file is an error only when required. A symlink is followed, but
// what it reaches must be a regular file: a FIFO there would otherwise block
// the daemon's request.
func readAssignments(path string, required bool) (map[string]assignment, error) {
	f, _, err := openRegularFollowing(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("svcctl: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, scriptCap))
	if err != nil {
		return nil, fmt.Errorf("svcctl: %w", err)
	}
	out := map[string]assignment{}
	n := 0
	for line := range bytes.Lines(b) {
		n++
		m := logAssign.FindSubmatch(bytes.TrimRight(line, "\r\n"))
		if m == nil {
			continue
		}
		key := string(m[1])
		out[key] = assignment{where: fmt.Sprintf("%s:%d", path, n), text: strings.TrimLeft(string(m[0]), " \t"), value: string(m[2])}
	}
	return out, nil
}

// resolveLog resolves key (output_log or error_log) from the two files. It
// returns the path, or a reason it does not resolve, and whether the
// service declares anything for key (the key or its logger).
func resolveLog(key string, initd, confd map[string]assignment, name string) (string, string, bool) {
	a, inInit := initd[key]
	fromConf := false
	if !inInit {
		a, fromConf = confd[key]
	}
	if !inInit && !fromConf {
		logger := strings.TrimSuffix(key, "_log") + "_logger"
		for _, m := range []map[string]assignment{initd, confd} {
			if l, ok := m[logger]; ok {
				return "", fmt.Sprintf("%s at %s sends the output to a program, not a file", logger, l.where), true
			}
		}
		return "", "", false
	}
	v, err := shellWord(a.value)
	if err == nil && inInit {
		if def, ok := deferredDefault(v, key); ok {
			v = def
			if c, ok := confd[key]; ok {
				if cv, cerr := shellWord(c.value); cerr != nil || cv != "" {
					a, v, err = c, cv, cerr
				}
			}
		}
	}
	if err == nil && v == "" {
		// An empty assignment declares no file.
		return "", "", false
	}
	if err == nil {
		v, err = literalPath(v, name)
	}
	if err != nil {
		return "", fmt.Sprintf("%s at %s does not resolve: %v", printable(a.text), a.where, err), true
	}
	return v, "", true
}

// shellWord is the single word of an assignment's right-hand side, with
// its quotes removed: unquoted, "double-quoted" or 'single-quoted', then
// nothing but blanks or a comment. Text in single quotes is marked so that
// literalPath does not expand it.
func shellWord(s string) (string, error) {
	var w strings.Builder
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		switch c := s[i]; c {
		case '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				return "", errors.New("an unclosed quote")
			}
			part := s[i+1 : i+1+j]
			if strings.ContainsAny(part, "\\`") {
				return "", errors.New("an escape or command in quotes")
			}
			w.WriteString(part)
			i += j + 2
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", errors.New("an unclosed quote")
			}
			part := s[i+1 : i+1+j]
			if strings.ContainsRune(part, '$') {
				return "", errors.New("a $ in single quotes")
			}
			w.WriteString(part)
			i += j + 2
		case ';', '&', '|', '<', '>', '(', ')', '`', '\\', '#', '*', '?', '[', '~':
			if c == '#' && i > 0 {
				w.WriteByte(c)
				i++
				continue
			}
			return "", fmt.Errorf("the shell character %q", c)
		default:
			w.WriteByte(c)
			i++
		}
	}
	if rest := strings.TrimLeft(s[i:], " \t"); rest != "" && rest[0] != '#' {
		return "", errors.New("more than one word")
	}
	return w.String(), nil
}

// deferredDefault is X when v is exactly ${key:-X}.
func deferredDefault(v, key string) (string, bool) {
	inner, ok := strings.CutPrefix(v, "${"+key+":-")
	if !ok {
		return "", false
	}
	def, ok := strings.CutSuffix(inner, "}")
	if !ok || strings.ContainsAny(strings.ReplaceAll(def, "${RC_SVCNAME}", ""), "{}") {
		return "", false
	}
	return def, true
}

// literalPath is v with $RC_SVCNAME and ${RC_SVCNAME} expanded to name,
// when what remains is an absolute, clean path with no other expansion.
func literalPath(v, name string) (string, error) {
	var w strings.Builder
	for i := 0; i < len(v); {
		if v[i] != '$' {
			if v[i] == '{' || v[i] == '}' {
				return "", errors.New("a brace outside ${RC_SVCNAME}")
			}
			w.WriteByte(v[i])
			i++
			continue
		}
		rest := v[i+1:]
		switch {
		case strings.HasPrefix(rest, "{RC_SVCNAME}"):
			w.WriteString(name)
			i += 1 + len("{RC_SVCNAME}")
		case strings.HasPrefix(rest, "RC_SVCNAME") && !isNameByte(rest, len("RC_SVCNAME")):
			w.WriteString(name)
			i += 1 + len("RC_SVCNAME")
		default:
			return "", errors.New("a variable other than RC_SVCNAME")
		}
	}
	p := w.String()
	switch {
	case p == "":
		return "", errors.New("an empty path")
	case !filepath.IsAbs(p):
		return "", errors.New("not an absolute path")
	case slices.Contains(strings.Split(p, "/"), ".."):
		return "", errors.New("a .. in the path")
	case !utf8.ValidString(p) || strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return "", errors.New("a control character in the path")
	}
	return filepath.Clean(p), nil
}

// isNameByte reports whether s[i] continues a shell variable name.
func isNameByte(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// printable is s as valid UTF-8 without control characters, at most 200
// bytes, for a reason.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	return cutText(s, 200)
}

// tailBlock is the block TailFile reads backwards by.
const tailBlock = 64 << 10

// TailFile returns the newest lines of the file at path, oldest first:
// at most lines of them, and at most maxBytes bytes counting each line's
// newline. The file is opened without following a symbolic link in any
// component of path (openat2 RESOLVE_NO_SYMLINKS, or a walk of O_NOFOLLOW
// opens) and without blocking, and must be a regular file with one hard
// link, so a symlink (ELOOP), a hard-linked file, a FIFO or a directory is
// an error and nothing blocks. path is absolute and clean. Lines are made valid UTF-8 and carry no time. When
// the newest line alone exceeds maxBytes, its newest part is returned,
// starting with LineTruncatedMarker.
func TailFile(path string, lines int, maxBytes int) ([]string, error) {
	if lines <= 0 || maxBytes <= len(LineTruncatedMarker)+1 {
		return nil, fmt.Errorf("svcctl: invalid tail bounds: %d lines, %d bytes", lines, maxBytes)
	}
	fd, err := openNoLinks(path)
	if err != nil {
		return nil, fmt.Errorf("svcctl: open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("svcctl: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("svcctl: %s is not a regular file (%s)", path, st.Mode().Type())
	}
	// A hard link to a file the service's user could not read is still that
	// file, and the link is the user's to make in a directory it owns
	// (CWE-62): a log file has one name.
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && uint64(sys.Nlink) > 1 {
		return nil, fmt.Errorf("svcctl: %s has %d hard links; a log file is read only when it has one", path, uint64(sys.Nlink))
	}
	// The count above was read from the descriptor, after the open. A service
	// user that links a root-only file into its directory and removes the
	// link between the open and that read leaves the count at 1 for a file
	// that is not the log (CWE-367). So the name is looked at again, in its
	// parent directory and without following a link: it must still be the
	// descriptor's file, and that file's only name. That narrows the window
	// and does not close it, because unlink(2) drops the count before it
	// drops the name, and a look that lands inside an unlink sees the name
	// with a count of 1 (observed, on overlayfs, about once in a thousand
	// reads against a process that does nothing else). What closes it is
	// the owner check in soleName: a root-only file is refused in a
	// directory that anyone else can write, whatever its link count.
	if err := soleName(fd, path); err != nil {
		return nil, fmt.Errorf("svcctl: %s: %w", path, err)
	}
	out := []string{}
	total := 0
	// keep adds line (newest first) and reports whether there is room for
	// another.
	keep := func(line []byte) bool {
		s := strings.ToValidUTF8(string(line), "�")
		if total+len(s)+1 > maxBytes {
			if len(out) == 0 {
				s = LineTruncatedMarker + s[len(s)-cutTail(s, maxBytes-1-len(LineTruncatedMarker)):]
				out = append(out, s)
			}
			return false
		}
		out = append(out, s)
		total += len(s) + 1
		return len(out) < lines
	}
	pos := st.Size()
	var carry []byte // The newest partial line read so far
	skipNL := true   // The file's final newline ends a line; it does not start one
	buf := make([]byte, tailBlock)
	for pos > 0 {
		n := int64(tailBlock)
		if pos < n {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("svcctl: %w", err)
		}
		chunk := append(slices.Clone(buf[:n]), carry...)
		if skipNL {
			chunk = bytes.TrimSuffix(chunk, []byte("\n"))
			skipNL = false
		}
		for {
			i := bytes.LastIndexByte(chunk, '\n')
			if i < 0 {
				break
			}
			if !keep(chunk[i+1:]) {
				return reversed(out), nil
			}
			chunk = chunk[:i]
		}
		carry = chunk
		// A partial line that cannot fit is cut now, so carry stays bounded.
		if len(carry) > maxBytes {
			keep(carry)
			return reversed(out), nil
		}
	}
	// What is left is the file's first line.
	if st.Size() > 0 {
		keep(carry)
	}
	return reversed(out), nil
}

// cutTail is how many of s's last bytes, at most n, start at a character
// boundary.
func cutTail(s string, n int) int {
	if len(s) <= n {
		return len(s)
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return len(s) - i
}

func reversed(s []string) []string {
	slices.Reverse(s)
	return s
}
