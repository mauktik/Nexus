// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package svcctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// JournalctlPath is the journal's tool, always called by this path.
const JournalctlPath = "/usr/bin/journalctl"

// DefaultJournalTail is the -n Journal sends when the query has neither a
// tail nor a since.
const DefaultJournalTail = 200

// JournalPriorities are the -p values Journal accepts: journalctl's level
// names, most important first. Numbers and ranges are refused.
var JournalPriorities = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// JournalQuery selects journal lines of one unit (DF-2).
type JournalQuery struct {
	Tail     int        // -n; 0 = DefaultJournalTail unless Since is set
	Since    *time.Time // --since=@<sec>.<usec>
	Priority string     // -p, one of JournalPriorities; "" = none
	MaxLines int        // Journal stops after this many lines; 0 = no limit
}

// JournalLine is one journal entry as a frame: T from
// __REALTIME_TIMESTAMP, S "stderr" for PRIORITY 0 to 3 and "stdout"
// otherwise, and D the MESSAGE as valid UTF-8 of at most LineCap bytes.
type JournalLine struct {
	T time.Time
	S string
	D string
}

// journalArgv is the DF-2 argv of name and q, or an error wrapping
// ErrNotStarted for an input the argv must not carry.
func journalArgv(name string, q JournalQuery) ([]string, error) {
	if err := checkService(name); err != nil {
		return nil, err
	}
	if q.Tail < 0 || q.MaxLines < 0 {
		return nil, fmt.Errorf("%w: invalid journal query: tail %d, max lines %d", ErrNotStarted, q.Tail, q.MaxLines)
	}
	if q.Priority != "" && !slices.Contains(JournalPriorities, q.Priority) {
		return nil, fmt.Errorf("%w: invalid priority %q: it must be one of %s", ErrNotStarted, q.Priority,
			strings.Join(JournalPriorities, ", "))
	}
	argv := []string{JournalctlPath, "--no-pager", "--quiet", "-o", "json", "--output-fields=MESSAGE,PRIORITY",
		"-u", name + ".service"}
	tail := q.Tail
	if tail == 0 && q.Since == nil {
		tail = DefaultJournalTail
	}
	if tail > 0 {
		argv = append(argv, "-n", strconv.Itoa(tail))
	}
	if q.Since != nil {
		if q.Since.Unix() < 0 {
			return nil, fmt.Errorf("%w: invalid since %s: before the epoch", ErrNotStarted, q.Since.UTC().Format(time.RFC3339))
		}
		argv = append(argv, fmt.Sprintf("--since=@%d.%06d", q.Since.Unix(), q.Since.Nanosecond()/1000))
	}
	if q.Priority != "" {
		argv = append(argv, "-p", q.Priority)
	}
	return argv, nil
}

// Journal reads name's journal with the DF-2 argv through opts.RunLines
// (nil: RunLines), bounded by opts.Timeout, and hands each line to emit as
// a JournalLine, oldest first. It stops reading when emit returns false or
// after q.MaxLines lines, which is not an error. A line that is not a
// journal entry ends it with an error; a non-zero exit is an error naming
// journalctl and the first line of its stderr. Errors are as Run's: an
// argument refused before any exec, or a missing journalctl, wraps
// ErrNotStarted, and an expired bound wraps context.DeadlineExceeded.
func Journal(ctx context.Context, name string, q JournalQuery, opts Options, emit func(JournalLine) bool) error {
	argv, err := journalArgv(name, q)
	if err != nil {
		return err
	}
	runLines := opts.RunLines
	if runLines == nil {
		runLines = RunLines
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		n         int
		stopped   bool
		decodeErr error
	)
	res, err := runLines(ctx, argv, LineCap, func(line []byte) bool {
		l, err := decodeJournalLine(line)
		if err != nil {
			decodeErr = err
			return false
		}
		n++
		if !emit(l) || q.MaxLines > 0 && n >= q.MaxLines {
			stopped = true
			return false
		}
		return true
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("svcctl: %s did not finish within %s: %w", JournalctlPath, timeout, ctxErr)
		}
		return fmt.Errorf("svcctl: %s: %w", JournalctlPath, ctxErr)
	}
	switch {
	case err != nil:
		return fmt.Errorf("svcctl: %s: %w", JournalctlPath, err)
	case decodeErr != nil:
		return fmt.Errorf("svcctl: %s printed a line that is not a journal entry: %w", JournalctlPath, decodeErr)
	case stopped:
		return nil
	case res.ExitCode != 0:
		msg, _, _ := strings.Cut(strings.TrimSpace(strings.ToValidUTF8(string(res.Output), "�")), "\n")
		return fmt.Errorf("svcctl: %s exited %d: %s", JournalctlPath, res.ExitCode, msg)
	}
	return nil
}

// decodeJournalLine decodes one line of `journalctl -o json`. A whole line
// is decoded strictly: an object whose __REALTIME_TIMESTAMP is a decimal
// string, whose PRIORITY (when present) is "0" to "7", and whose MESSAGE
// (when present) is a string, a byte array, null, or an array of those. A
// line RunLines cut (it ends with LineTruncatedMarker) is decoded as far
// as it goes: its D is what was read of MESSAGE, marked, and a field the
// cut removed is left zero.
//
// journalctl prints a field it holds as non-UTF-8 or binary as an array of
// its bytes, a field over 4096 bytes as null (no --all), and a field with
// several values as an array of them (journalctl(1), -o json).
func decodeJournalLine(line []byte) (JournalLine, error) {
	var fields map[string]json.RawMessage
	cut := false
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		head, ok := bytes.CutSuffix(line, []byte(LineTruncatedMarker))
		if !ok {
			if err == nil {
				err = errors.New("not a JSON object")
			}
			return JournalLine{}, err
		}
		fields, cut = salvage(head), true
	}
	var l JournalLine
	if raw, ok := fields["__REALTIME_TIMESTAMP"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return JournalLine{}, fmt.Errorf("__REALTIME_TIMESTAMP: %w", err)
		}
		us, err := strconv.ParseInt(s, 10, 64)
		if err != nil || us < 0 {
			return JournalLine{}, fmt.Errorf("__REALTIME_TIMESTAMP %q is not a time in microseconds", s)
		}
		l.T = time.UnixMicro(us)
	} else if !cut {
		return JournalLine{}, errors.New("no __REALTIME_TIMESTAMP")
	}
	l.S = "stdout"
	if raw, ok := fields["PRIORITY"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || len(s) != 1 || s[0] < '0' || s[0] > '7' {
			return JournalLine{}, fmt.Errorf("PRIORITY %s is not 0 to 7", raw)
		}
		if s[0] <= '3' {
			l.S = "stderr"
		}
	}
	if raw, ok := fields["MESSAGE"]; ok {
		d, err := journalMessage(raw)
		if err != nil {
			return JournalLine{}, fmt.Errorf("MESSAGE: %w", err)
		}
		l.D = d
	}
	l.D = strings.ToValidUTF8(l.D, "�")
	if cut || len(l.D) > LineCap {
		l.D = cutText(l.D, LineCap-len(LineTruncatedMarker)) + LineTruncatedMarker
	}
	return l, nil
}

// journalMessage is a MESSAGE value as text: a string, a byte array, null
// (""), or an array of those joined by newlines.
func journalMessage(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case bytes.Equal(raw, []byte("null")):
		return "", nil
	case len(raw) > 0 && raw[0] == '"':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	case len(raw) > 0 && raw[0] == '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return "", err
		}
		if b, ok := byteArray(elems); ok {
			return string(b), nil
		}
		parts := make([]string, 0, len(elems))
		for _, e := range elems {
			if t := bytes.TrimSpace(e); len(t) > 0 && t[0] != '"' && t[0] != '[' && !bytes.Equal(t, []byte("null")) {
				return "", fmt.Errorf("an array element %s is not a value", e)
			}
			s, err := journalMessage(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", fmt.Errorf("%.40s is not a string, byte array or null", raw)
}

// byteArray is elems as bytes when every element is an integer 0 to 255.
// An empty array is not a byte array.
func byteArray(elems []json.RawMessage) ([]byte, bool) {
	if len(elems) == 0 {
		return nil, false
	}
	b := make([]byte, 0, len(elems))
	for _, e := range elems {
		v, err := strconv.ParseUint(string(bytes.TrimSpace(e)), 10, 8)
		if err != nil {
			return nil, false
		}
		b = append(b, byte(v))
	}
	return b, true
}

// salvage reads the fields of a JSON object cut short: every whole member,
// and the member the cut fell in when it is MESSAGE, as far as it was
// read (a string up to its last whole character, a byte array up to its
// last whole element).
func salvage(head []byte) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(head))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fields
	}
	for {
		t, err := dec.Token()
		if err != nil {
			return fields
		}
		key, ok := t.(string)
		if !ok {
			return fields
		}
		at := dec.InputOffset()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			fields[key] = raw
			continue
		}
		if key == "MESSAGE" {
			if v, ok := partialValue(head[at:]); ok {
				fields[key] = v
			}
		}
		return fields
	}
}

// partialValue closes the cut value at the start of rest (after its
// colon): a string, or an array of numbers, made whole by dropping what
// follows its last whole part.
func partialValue(rest []byte) (json.RawMessage, bool) {
	rest = bytes.TrimLeft(rest, " \t\r\n")
	rest, ok := bytes.CutPrefix(rest, []byte(":"))
	if !ok {
		return nil, false
	}
	rest = bytes.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 {
		return nil, false
	}
	switch rest[0] {
	case '"':
		// A cut can fall inside an escape (\, \u, \u12): drop up to the six
		// bytes an escape spans until the string closes.
		for drop := 0; drop <= 6 && drop < len(rest); drop++ {
			v := append(slices.Clone(rest[:len(rest)-drop]), '"')
			if json.Valid(v) {
				return v, true
			}
		}
	case '[':
		// The last element may be a number cut short: keep the elements
		// before the last comma.
		i := bytes.LastIndexByte(rest, ',')
		if i < 0 {
			return json.RawMessage("[]"), true
		}
		v := append(slices.Clone(rest[:i]), ']')
		if json.Valid(v) {
			return v, true
		}
	}
	return nil, false
}

// cutText is s cut to at most n bytes at a character boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
