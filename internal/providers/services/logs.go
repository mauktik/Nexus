package services

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// logFileBytes bounds the text of one OpenRC file-log read: every line
// kept, with its newline, from both files together (DF-2 step 4).
const logFileBytes = 64 << 10

// logFileMinBytes is the least budget worth a TailFile call: one byte of
// text past the truncation marker and its newline.
const logFileMinBytes = len(svcctl.LineTruncatedMarker) + 2

// The frames' streams.
const (
	logStdout = "stdout"
	logStderr = "stderr"
)

// logQuery is a validated logs input.
type logQuery struct {
	tail     int        // 0 = none given
	since    *time.Time // Nil = none
	priority string
}

// logs streams a service's recent log lines as frames, oldest first (DF-2):
// from the journal on systemd, each frame with its time; on OpenRC from the
// output_log and error_log files the service declares, with no time. Every
// input is checked before anything runs. A service with no log source
// answers 422 logs-unavailable and streams nothing; a failing or hung
// tool answers 503 dependency-unavailable. The Dispatcher audits the call
// (Audited), with the fact service=<name>.
func (p *Provider) logs(ctx context.Context, in spec.LogsInput, emit func(apitypes.Frame) error) error {
	q, err := p.logCheck(in)
	if err != nil {
		return err
	}
	audit.AddFact(ctx, "service", in.Name)
	if p.plat.Init == platform.OpenRC {
		return p.logFiles(in.Name, q, emit)
	}
	return p.logJournal(ctx, in.Name, q, emit)
}

// logCheck applies the input rules of DF-2 step 2. since and priority
// apply to the journal only: file lines carry no time and no level.
func (p *Provider) logCheck(in spec.LogsInput) (logQuery, error) {
	var q logQuery
	if err := readCheckName(in.Name); err != nil {
		return q, err
	}
	if in.Tail != nil {
		if *in.Tail < 1 || *in.Tail > spec.MaxTail {
			return q, readInvalid("/tail", fmt.Sprintf("must be between 1 and %d", spec.MaxTail))
		}
		q.tail = *in.Tail
	}
	files := p.plat.Init == platform.OpenRC
	if in.Since != "" {
		if files {
			return q, readInvalid("/since", "does not apply to file logs: file logs carry no time")
		}
		t, err := p.logSince(in.Since)
		if err != nil {
			return q, err
		}
		q.since = &t
	}
	if in.Priority != "" {
		if files {
			return q, readInvalid("/priority", "does not apply to file logs: file lines carry no syslog level")
		}
		if !slices.Contains(spec.Priorities, in.Priority) {
			return q, readInvalid("/priority", "must be one of "+strings.Join(spec.Priorities, ", "))
		}
		q.priority = in.Priority
	}
	return q, nil
}

// logSince is since as a time: an RFC 3339 time, or a duration of at most
// spec.MaxSince before now.
func (p *Provider) logSince(s string) (time.Time, error) {
	const msg = "must be an RFC 3339 time or a duration such as 90m, at most 168h"
	if len(s) > spec.MaxSinceLength {
		return time.Time{}, readInvalid("/since", msg)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		if t.Unix() < 0 {
			return time.Time{}, readInvalid("/since", "must not be before 1970")
		}
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d > spec.MaxSince {
		return time.Time{}, readInvalid("/since", msg)
	}
	return p.now().Add(-d), nil
}

// logJournal streams name's journal. The service must be known first, so
// an unknown name answers 404 without running journalctl. With since and no
// tail it reads forward and stops after spec.MaxTail lines; with neither,
// the newest spec.DefaultTail.
func (p *Provider) logJournal(ctx context.Context, name string, q logQuery, emit func(apitypes.Frame) error) error {
	opts := p.svcctlOptions()
	d, err := svcctl.Describe(ctx, p.plat.Init, name, opts)
	if err != nil {
		return readFailure(ctx, err)
	}
	if !d.Known {
		return readNotFound(name)
	}
	jq := svcctl.JournalQuery{Tail: q.tail, Since: q.since, Priority: q.priority}
	switch {
	case q.since != nil && q.tail == 0:
		jq.MaxLines = spec.MaxTail
	case q.since == nil && q.tail == 0:
		jq.Tail = spec.DefaultTail
	}
	var emitErr error
	err = svcctl.Journal(ctx, name, jq, opts, func(l svcctl.JournalLine) bool {
		emitErr = emit(apitypes.Frame{T: l.T, S: l.S, D: l.D})
		return emitErr == nil
	})
	switch {
	case emitErr != nil:
		return emitErr
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return err
	case errors.Is(err, svcctl.ErrNotStarted) && errors.Is(err, fs.ErrNotExist):
		return logsUnavailable(name, spec.LogSourceJournal, svcctl.JournalctlPath+" is not present on this host")
	}
	return readFailure(ctx, err)
}

// logFiles streams the newest lines of the output_log and error_log an
// OpenRC service declares: at most tail lines of each (default
// spec.DefaultTail), and at most logFileBytes of text from both together,
// output lines first, then error lines. The lines carry no time, so their
// frames have none (A-38); the provider never reads one from the text. A
// declared file that does not exist yet has no lines.
func (p *Provider) logFiles(name string, q logQuery, emit func(apitypes.Frame) error) error {
	lf, err := svcctl.OpenRCLogFiles(p.etcRoot, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return readNotFound(name)
	case err != nil:
		return logsUnavailable(name, spec.LogSourceOpenRCFiles, "the service's log declarations cannot be read: "+readDetail(err))
	case lf.Output == "" && lf.Error == "":
		return logsUnavailable(name, spec.LogSourceOpenRCFiles, lf.Reason)
	}
	tail := q.tail
	if tail == 0 {
		tail = spec.DefaultTail
	}
	// The error log is read first with half the budget, the output log with
	// what that left, and the error log again with what the output log
	// left, so neither file can crowd the other out.
	errLines, err := logTail(lf.Error, tail, logFileBytes/2)
	if err != nil {
		return logsUnavailable(name, spec.LogSourceOpenRCFiles, err.Error())
	}
	outLines, err := logTail(lf.Output, tail, logFileBytes-logBytes(errLines))
	if err != nil {
		return logsUnavailable(name, spec.LogSourceOpenRCFiles, err.Error())
	}
	if rest := logFileBytes - logBytes(outLines); rest > logBytes(errLines) {
		if errLines, err = logTail(lf.Error, tail, rest); err != nil {
			return logsUnavailable(name, spec.LogSourceOpenRCFiles, err.Error())
		}
	}
	for _, f := range []struct {
		stream string
		lines  []string
	}{{logStdout, outLines}, {logStderr, errLines}} {
		for _, l := range f.lines {
			if err := emit(apitypes.Frame{S: f.stream, D: l}); err != nil {
				return err
			}
		}
	}
	return nil
}

// logTail is the newest lines of the log file at path, within lines and
// maxBytes; none for no path, a file that does not exist, or a budget too
// small for a line.
func logTail(path string, lines, maxBytes int) ([]string, error) {
	if path == "" || maxBytes < logFileMinBytes {
		return nil, nil
	}
	out, err := svcctl.TailFile(path, lines, maxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the log file cannot be read: %s", readDetail(err))
	}
	return out, nil
}

// logBytes is what lines count against the budget: each with its newline,
// as TailFile counts them.
func logBytes(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func logsUnavailable(name, source, reason string) *problem.Error {
	return problem.New(problem.LogsUnavailable, fmt.Sprintf("service %s has no readable logs: %s", name, reason)).
		With("service", name).With("logSource", source).With("reason", reason)
}
