// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package vzdump

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Log verdicts (LogVerdict.Status).
const (
	LogOK      = "ok"
	LogError   = "error"
	LogUnknown = "unknown"
)

// truncMarker ends a log line cut at its bound (NFR-3).
const truncMarker = "…[truncated]"

// Proxmox writes each vzdump log line as `%F %H:%M:%S <LEVEL>: <text>`
// (PVE::VZDump::Plugin::debugmsg), the levels being `ERROR:`, `INFO:` and
// `WARN:`; a successful guest backup logs `Finished Backup of VM <id>` at
// INFO (PVE::VZDump::exec_backup_task). The timestamp is optional here.
var stampPrefix = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} `)

var (
	finishPrefix = []byte("INFO: Finished Backup of VM")
	errorPrefix  = []byte("ERROR:")
)

// LogVerdict is a log's status (ok | error | unknown, A-11) and its last
// ERROR lines, oldest first.
type LogVerdict struct {
	Status string
	Errors []string
}

// Notes returns the first max bytes of the notes file name (relative to
// the root, "dump/<file>" for an archive in dump/) as valid UTF-8, invalid
// bytes replaced with U+FFFD and cut on a rune boundary, and whether the
// file held more. It reads at most max+3 bytes.
func Notes(root Opener, name string, limit int) (string, bool, error) {
	limit = max(limit, 0)
	f, _, err := openRegular(root, name)
	if err != nil {
		return "", false, fmt.Errorf("notes: %w", err)
	}
	defer f.Close()
	// Three bytes past the bound complete a rune that straddles it.
	buf := make([]byte, limit+utf8.UTFMax-1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, fmt.Errorf("notes %s: %w", name, err)
	}
	text, cut := clip(buf[:n], limit)
	return text, n > limit || cut, nil
}

// ReadLog reads at most the last tail bytes of the log file name (relative
// to the root) and decides its status (A-11): ok when the last
// `INFO: Finished Backup of VM` line follows the last `ERROR:` line, error
// when an `ERROR:` line follows it or there are `ERROR:` lines and no
// finish line, unknown when neither appears. Errors holds the last
// maxLines ERROR lines, each valid UTF-8 and at most maxLine bytes (a cut
// line ends with the truncation marker). A line the tail starts inside is
// dropped.
func ReadLog(root Opener, name string, tail int64, maxLines, maxLine int) (LogVerdict, error) {
	f, info, err := openRegular(root, name)
	if err != nil {
		return LogVerdict{}, fmt.Errorf("log: %w", err)
	}
	defer f.Close()
	size := info.Size()
	off := max(size-max(tail, 0), 0)
	buf := make([]byte, size-off)
	n, err := readAt(f, buf, off)
	if err != nil {
		return LogVerdict{}, fmt.Errorf("log %s: %w", name, err)
	}
	buf = buf[:n]
	if off > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			buf = nil
		} else {
			buf = buf[i+1:]
		}
	}
	return verdict(buf, maxLines, maxLine), nil
}

func verdict(log []byte, maxLines, maxLine int) LogVerdict {
	lastFinish, lastErr := -1, -1
	var errLines [][]byte
	i := 0
	for line := range bytes.Lines(log) {
		line = bytes.TrimRight(line, "\r\n")
		body := line
		if loc := stampPrefix.FindIndex(line); loc != nil {
			body = line[loc[1]:]
		}
		switch {
		case bytes.HasPrefix(body, finishPrefix):
			lastFinish = i
		case bytes.HasPrefix(body, errorPrefix):
			lastErr = i
			errLines = append(errLines, line)
		}
		i++
	}
	v := LogVerdict{Status: LogError}
	switch {
	case lastFinish < 0 && lastErr < 0:
		v.Status = LogUnknown
	case lastFinish > lastErr:
		v.Status = LogOK
	}
	maxLines = max(maxLines, 0)
	if len(errLines) > maxLines {
		errLines = errLines[len(errLines)-maxLines:]
	}
	for _, l := range errLines {
		v.Errors = append(v.Errors, clipLine(l, maxLine))
	}
	return v
}

// openRegular opens name after checking, through the root, that its own
// entry (Lstat) is a regular file, then checks that the open file is that
// same file. The open is O_NONBLOCK, so a FIFO swapped in after the check
// does not block open(2). os.Root.OpenFile ignores O_NOFOLLOW and follows a
// symlink that stays inside the root, so a symlink swapped in is caught by
// os.SameFile on the descriptor, not by a flag (SEC-9); the descriptor is
// what is judged, not the name (B-SEC-2). A symlink that leaves the root is
// refused by the root.
func openRegular(root Opener, name string) (fs.File, fs.FileInfo, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: not a regular file", name)
	}
	of, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := of.Stat()
	if err == nil && (!info.Mode().IsRegular() || !os.SameFile(lfi, info)) {
		err = fmt.Errorf("%s: not a regular file", name)
	}
	if err != nil {
		return nil, nil, errors.Join(err, of.Close())
	}
	var f fs.File = of
	if w, ok := root.(interface {
		WrapFile(name string, f *os.File) fs.File
	}); ok {
		f = w.WrapFile(name, of)
	}
	return f, info, nil
}

// readAt reads len(buf) bytes at off, or fewer when the file shrank, with
// a positioned read when the file offers one, else a seek. It never reads
// the bytes before off.
func readAt(f fs.File, buf []byte, off int64) (int, error) {
	var r io.Reader
	switch ff := f.(type) {
	case io.ReaderAt:
		r = io.NewSectionReader(ff, off, int64(len(buf)))
	case io.Seeker:
		if _, err := ff.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		r = f
	default:
		return 0, errors.New("file cannot be read from an offset")
	}
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return n, err
}

// clip returns b cut to at most limit bytes on a rune boundary, as valid
// UTF-8, and whether anything was cut.
func clip(b []byte, limit int) (string, bool) {
	cut := false
	if len(b) > limit {
		b, cut = b[:runeCut(b, limit)], true
	}
	s := strings.ToValidUTF8(string(b), "�")
	if len(s) > limit {
		// Replacements can lengthen the text.
		s, cut = s[:runeCut([]byte(s), limit)], true
	}
	return s, cut
}

// clipLine is a log line as valid UTF-8 of at most limit bytes, ending
// with the truncation marker when cut.
func clipLine(b []byte, limit int) string {
	s := strings.ToValidUTF8(string(b), "�")
	if len(s) <= limit {
		return s
	}
	if limit < len(truncMarker) {
		return s[:runeCut([]byte(s), limit)]
	}
	keep := limit - len(truncMarker)
	return s[:runeCut([]byte(s), keep)] + truncMarker
}

// runeCut is the largest index ≤ i, at most three bytes back, at which b
// can be cut without splitting a rune; when b holds only continuation
// bytes there (invalid UTF-8), it is i, and ToValidUTF8 replaces them.
func runeCut(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	for j := i; j >= 0 && j > i-utf8.UTFMax; j-- {
		if utf8.RuneStart(b[j]) {
			return j
		}
	}
	return i
}
