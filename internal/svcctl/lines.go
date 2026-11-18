package svcctl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
)

const (
	// LineCap is the most bytes of one line RunLines hands to onLine, and
	// the most text of one journal frame (DF-2).
	LineCap = 64 << 10
	// StderrCap is the most stderr RunLines keeps, for errors.
	StderrCap = 4 << 10
	// LineTruncatedMarker ends a line that RunLines or Journal cut at its
	// cap, and starts a TailFile line kept only in part. The rest of such a
	// line is dropped, never joined to the next one.
	LineTruncatedMarker = " [line truncated]"
)

// RunLines runs argv with Run's call rules (argv[0] absolute, no shell, no
// stdin, the environment exactly env(), its own process group) and hands
// each line of its stdout to onLine, without the newline. A line longer
// than maxLine (LineCap when maxLine is not above the marker's length) is
// cut to maxLine bytes ending with LineTruncatedMarker, and the rest of it
// is dropped.
//
// When onLine returns false, RunLines kills the process group, waits for
// it and returns the Result with no error: ExitCode is -1 when the kill
// ended it. When ctx is done the group is killed and the error is ctx's.
// Result.Output is the tool's stderr, at most StderrCap bytes, with
// Truncated set when more was dropped. A non-zero exit is a Result, not an
// error. A failure before the tool started wraps ErrNotStarted.
func RunLines(ctx context.Context, argv []string, maxLine int, onLine func([]byte) bool) (Result, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return Result{}, fmt.Errorf("%w: refusing to run %q: the program must be an absolute path", ErrNotStarted, argv)
	}
	if maxLine <= len(LineTruncatedMarker) {
		maxLine = LineCap
	}
	stderr := &capWriter{max: StderrCap}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env()
	cmd.Stdin = nil
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	// A descendant that left the process group can hold stdout open after
	// the kill; closing our end when ctx is done ends the read regardless.
	stopClose := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	stopped := readLines(stdout, maxLine, onLine)
	stopClose()
	if stopped {
		// ESRCH only means the group already exited.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	res := Result{Output: stderr.buf, Truncated: stderr.dropped}
	var exitErr *exec.ExitError
	switch {
	case (err == nil || errors.Is(err, exec.ErrWaitDelay)) && cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		if err == nil {
			err = errors.New("the tool ended without a process state")
		}
		return Result{}, err
	}
	if res.ExitCode < 0 && !stopped {
		return Result{}, fmt.Errorf("%s was terminated: %v", argv[0], cmd.ProcessState)
	}
	return res, nil
}

// readLines hands each line of r to onLine, cut at maxLine, and reports
// whether onLine stopped it. A read error ends it as EOF does: the caller
// learns how the tool ended from Wait.
func readLines(r io.Reader, maxLine int, onLine func([]byte) bool) bool {
	br := bufio.NewReaderSize(r, LineCap)
	line := make([]byte, 0, 4096)
	cut := false
	deliver := func() bool {
		if cut {
			line = append(line[:maxLine-len(LineTruncatedMarker)], LineTruncatedMarker...)
		}
		ok := onLine(line)
		line, cut = line[:0], false
		return ok
	}
	for {
		frag, err := br.ReadSlice('\n')
		full := err == nil
		if full {
			frag = frag[:len(frag)-1]
		}
		if !cut {
			if room := maxLine - len(line); len(frag) > room {
				line = append(line, frag[:room]...)
				cut = true
			} else {
				line = append(line, frag...)
			}
		}
		switch {
		case full:
			if !deliver() {
				return true
			}
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			if len(line) > 0 || cut {
				return !deliver()
			}
			return false
		}
	}
}
