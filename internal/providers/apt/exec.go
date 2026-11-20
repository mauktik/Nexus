// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package apt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"time"
)

// The fixed binaries. Nothing is looked up in PATH (D-7).
const (
	AptGet    = "/usr/bin/apt-get"
	AptCache  = "/usr/bin/apt-cache"
	DpkgQuery = "/usr/bin/dpkg-query"
	Dpkg      = "/usr/bin/dpkg"
)

// Binaries are what preflight requires.
var Binaries = []string{AptGet, AptCache, DpkgQuery, Dpkg}

// env is the exact environment of every apt and dpkg command: the C locale,
// so the parsed text is the untranslated one (A-27), and no prompts.
var env = []string{
	"LC_ALL=C",
	"LANG=C",
	"DEBIAN_FRONTEND=noninteractive",
	"APT_LISTCHANGES_FRONTEND=none",
}

// Env returns a copy of the exact environment every command runs with.
func Env() []string { return slices.Clone(env) }

// noCacheFiles makes apt build its package cache in memory instead of in
// /var/cache/apt, which the shipped unit's ProtectSystem=strict makes
// read-only (A-23).
var noCacheFiles = []string{"-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache="}

// dpkgFormat is the dpkg-query output format: one line per package, with
// tab-separated fields that never contain a tab or newline.
const dpkgFormat = `${Package}\t${Architecture}\t${Version}\t${db:Status-Status}\n`

// command is one apt or dpkg invocation: Argv[0] is an absolute path and
// every package name follows "--", so none is read as an option.
type command struct {
	Argv []string
}

func newCommand(bin string, args ...string) command {
	return command{Argv: append([]string{bin}, args...)}
}

func cmdDpkgQueryAll() command {
	return newCommand(DpkgQuery, "-W", "-f="+dpkgFormat)
}

// cmdDpkgQuery queries one name. Package names hold no glob character, so
// dpkg-query matches it exactly.
func cmdDpkgQuery(name string) command {
	return newCommand(DpkgQuery, "-W", "-f="+dpkgFormat, "--", name)
}

func cmdArchitecture() command {
	return newCommand(Dpkg, "--print-architecture")
}

// cmdPkgNames lists the package names apt knows that start with prefix: a
// prefix match, never a regular expression, so it is how a name resolves
// exactly before it reaches any other apt command (A-28).
func cmdPkgNames(prefix string) command {
	return newCommand(AptCache, slices.Concat(noCacheFiles, []string{"pkgnames", "--", prefix})...)
}

// cmdPkgNamesAll lists every package name apt knows, one per line.
func cmdPkgNamesAll() command {
	return newCommand(AptCache, slices.Concat(noCacheFiles, []string{"pkgnames"})...)
}

// cmdPolicy reads Installed and Candidate of names that resolved exactly.
func cmdPolicy(names ...string) command {
	return newCommand(AptCache, slices.Concat(noCacheFiles, []string{"policy", "--"}, names)...)
}

// cmdShow reads the candidate's record of a name that resolved exactly.
func cmdShow(name string) command {
	return newCommand(AptCache, slices.Concat(noCacheFiles, []string{"show", "--no-all-versions", "--", name})...)
}

// cmdSimulate is the full-upgrade simulation whose Inst lines carry each
// candidate's origins. As root it does not wait on the dpkg lock (A-29).
func cmdSimulate() command {
	return newCommand(AptGet, slices.Concat(noCacheFiles, []string{"-s", "-o", "APT::Color=0", "dist-upgrade"})...)
}

// RefreshArgv is what the refresh unit runs.
func RefreshArgv() []string {
	return []string{AptGet, "-o", "APT::Color=0", "update"}
}

// Limits of one synchronous command.
const (
	commandTimeout = 2 * time.Minute
	maxOutputBytes = 64 << 20
)

// result is a finished command.
type result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// runner runs a command. The real one is execRun; tests replace it.
type runner func(ctx context.Context, c command) (result, error)

var errOutputTooLarge = errors.New("output exceeds the limit")

// execRun runs c with the exact environment, stdin on /dev/null, a bounded
// time and bounded output. A non-zero exit is a result, not an error: the
// caller decides what each code means.
func execRun(ctx context.Context, c command) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = Env()
	cmd.Dir = "/"
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxOutputBytes, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if stdout.over {
		return res, fmt.Errorf("%s: %w", c.Argv[0], errOutputTooLarge)
	}
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.Exited()) {
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s: %w", c.Argv[0], ctx.Err())
		}
		return res, fmt.Errorf("%s: %w", c.Argv[0], err)
	}
	return res, nil
}

// limitedBuffer keeps at most max bytes and records that more arrived.
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)
