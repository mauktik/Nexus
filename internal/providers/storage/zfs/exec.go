// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package zfs runs and reads the ZFS tools for the storage provider: the
// tool resolution, the one runner every zpool and zfs call goes through
// (storage-provider architecture D-3), the in-flight table over it, and the
// version that picks the output format (D-5).
package zfs

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// StdoutCap is the most standard output one call may write; more is
	// ErrStdoutTooLarge, never a truncated success (NFR-1).
	StdoutCap = 4 << 20
	// StderrCap is the most standard error kept from one call; the rest is
	// discarded.
	StderrCap = 64 << 10
	// waitDelay bounds the wait for the output pipes after the tool exited
	// or was killed, in case a descendant outside its process group still
	// holds them.
	waitDelay = time.Second
)

var (
	// ErrNotStarted wraps every failure before the tool's process started:
	// a refused argv, a missing or non-executable program.
	ErrNotStarted = errors.New("zfs: the command was not started")
	// ErrStdoutTooLarge is a call whose standard output exceeded StdoutCap.
	ErrStdoutTooLarge = errors.New("zfs: standard output exceeded 4 MiB")
)

// Result is what one finished call wrote and how it exited. A non-zero
// exit is a Result, not an error.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Runner runs one argv. Exec is the real one; tests plant others.
type Runner func(ctx context.Context, argv []string) (Result, error)

// env is the whole environment of every call, the same list as svcctl's
// (internal/svcctl/svcctl.go, env), pinned by TC-STG-51.
func env() []string { return []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"} }

// Exec executes argv[0], which must be absolute, with argv[1:] as its
// arguments: no shell, no stdin, the environment exactly env(), stdout
// capped at StdoutCap (exceeding it is ErrStdoutTooLarge) and stderr kept to
// StderrCap. Neither cap ever blocks the tool. The tool runs in its own
// process group, which is killed when ctx is done; Exec then returns an
// error wrapping ctx.Err(). A failure before the tool started wraps
// ErrNotStarted.
func Exec(ctx context.Context, argv []string) (Result, error) {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return Result{}, fmt.Errorf("%w: refusing to run %q: the program must be an absolute path", ErrNotStarted, argv)
	}
	stdout := &capWriter{max: StdoutCap}
	stderr := &capWriter{max: StderrCap}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env()
	cmd.Stdin = nil
	// Two writers make exec copy each stream on its own goroutine; each
	// writer is touched by only that goroutine until Wait returns.
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("zfs: %s: %w", argv[0], ctx.Err())
		}
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		return Result{}, fmt.Errorf("zfs: %s was stopped: %w", argv[0], ctx.Err())
	}
	var res Result
	var exitErr *exec.ExitError
	switch {
	case (err == nil || errors.Is(err, exec.ErrWaitDelay)) && cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		res.ExitCode = exitErr.ExitCode()
	default:
		// Wait leaves ProcessState nil when it fails without an exit status.
		if err == nil {
			err = errors.New("the tool ended without a process state")
		}
		return Result{}, fmt.Errorf("zfs: %s: %w", argv[0], err)
	}
	if res.ExitCode < 0 {
		return Result{}, fmt.Errorf("zfs: %s was terminated: %v", argv[0], cmd.ProcessState)
	}
	if stdout.over {
		return Result{}, fmt.Errorf("%w: %s wrote more than %d bytes", ErrStdoutTooLarge, argv[0], StdoutCap)
	}
	res.Stdout, res.Stderr = stdout.buf, stderr.buf
	return res, nil
}

// capWriter keeps the first max bytes written to it, discards the rest and
// records that it did, so the tool never blocks on a full pipe.
type capWriter struct {
	buf  []byte
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - len(w.buf)
	if len(p) > room {
		w.over = true
	}
	if room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
