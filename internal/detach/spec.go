// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package detach runs long operations outside nexusd, so they survive a
// kill -9 or a restart of the daemon (FR-6). The launcher starts a systemd
// transient unit whose main process is `nexusd op-supervise`; the supervisor
// runs the child, frames its output into the operation's oplog and writes
// exit.json at the end; the watcher in nexusd turns exit.json, or the
// confirmed loss of the unit, into the operation's outcome.
//
// The only state shared between nexusd and a supervisor is the operation's
// directory: spec.json (0600, unlinked by the supervisor on start), output/,
// exit.json (written by atomic rename) and cancel, the record that nexusd,
// or an operator with SIGINT, asked for the cancel.
package detach

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File names inside an operation's directory.
const (
	SpecFile  = "spec.json"
	ExitFile  = "exit.json"
	OutputDir = "output"
	// CancelFile records that the operation's cancel was asked for. nexusd
	// writes it before the cancel signal, and a supervisor takes a SIGTERM as
	// the cancel only when it exists. A supervisor writes it itself on an
	// operator's SIGINT.
	CancelFile = "cancel"
)

// KillMode is the KillMode= of the operation's unit (systemd.kill(5)).
type KillMode string

const (
	// KillProcess kills only the supervisor on unit stop. Apt uses it, so a
	// dpkg run is never cut by a stop of its unit.
	KillProcess KillMode = "process"
	// KillControlGroup kills every remaining process of the unit when it
	// stops. Scripts and container updates use it.
	KillControlGroup KillMode = "control-group"
)

// Credential is the identity the child runs as.
type Credential struct {
	UID    uint32
	GID    uint32
	Groups []uint32 // Supplementary groups
}

// Spec is the process a detached operation runs. Env is the child's exact
// environment; it may hold sensitive values, so a Spec only ever reaches
// disk as the 0600 spec.json and never the unit's properties (FR-18).
type Spec struct {
	Argv           []string      // Argv[0] is an absolute path: there is no PATH lookup
	Env            []string      // KEY=VALUE, exactly; empty means an empty environment
	Dir            string        // Working directory; empty means the supervisor's
	Credential     *Credential   // Nil runs the child as the supervisor's user
	Timeout        time.Duration // Zero means none
	Grace          time.Duration // Between SIGTERM and SIGKILL of the child's group
	OutputCapBytes int64         // Zero means oplog.DefaultCapBytes
	KillMode       KillMode      // Of the unit
	MaxRuntime     time.Duration // Required (A-63): the watcher's backstop
}

// Validate reports a Spec that cannot be launched.
func (s Spec) Validate() error {
	switch {
	case len(s.Argv) == 0 || s.Argv[0] == "":
		return errors.New("detach: spec has no command")
	case !filepath.IsAbs(s.Argv[0]):
		return fmt.Errorf("detach: command %q is not an absolute path", s.Argv[0])
	case s.KillMode != KillProcess && s.KillMode != KillControlGroup:
		return fmt.Errorf("detach: unknown kill mode %q", s.KillMode)
	case s.MaxRuntime <= 0:
		return errors.New("detach: spec has no maximum runtime")
	case s.Timeout < 0 || s.Grace < 0 || s.OutputCapBytes < 0:
		return errors.New("detach: negative timeout, grace or output cap")
	}
	return nil
}

// specFile is the on-disk form of a Spec.
type specFile struct {
	Argv           []string        `json:"argv"`
	Env            []string        `json:"env"`
	Dir            string          `json:"dir,omitempty"`
	Credential     *credentialFile `json:"credential,omitempty"`
	TimeoutNS      int64           `json:"timeoutNs"`
	GraceNS        int64           `json:"graceNs"`
	OutputCapBytes int64           `json:"outputCapBytes"`
	KillMode       KillMode        `json:"killMode"`
	MaxRuntimeNS   int64           `json:"maxRuntimeNs"`
}

type credentialFile struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups,omitempty"`
}

func toFile(s Spec) specFile {
	f := specFile{
		Argv: s.Argv, Env: s.Env, Dir: s.Dir,
		TimeoutNS: int64(s.Timeout), GraceNS: int64(s.Grace), OutputCapBytes: s.OutputCapBytes,
		KillMode: s.KillMode, MaxRuntimeNS: int64(s.MaxRuntime),
	}
	if f.Env == nil {
		f.Env = []string{}
	}
	if c := s.Credential; c != nil {
		f.Credential = &credentialFile{UID: c.UID, GID: c.GID, Groups: c.Groups}
	}
	return f
}

func fromFile(f specFile) Spec {
	s := Spec{
		Argv: f.Argv, Env: f.Env, Dir: f.Dir,
		Timeout: time.Duration(f.TimeoutNS), Grace: time.Duration(f.GraceNS), OutputCapBytes: f.OutputCapBytes,
		KillMode: f.KillMode, MaxRuntime: time.Duration(f.MaxRuntimeNS),
	}
	if c := f.Credential; c != nil {
		s.Credential = &Credential{UID: c.UID, GID: c.GID, Groups: c.Groups}
	}
	return s
}

// WriteSpec writes s as dir/spec.json, mode 0600, by atomic rename, so a
// supervisor never reads a partial spec.
func WriteSpec(dir string, s Spec) error {
	b, err := json.Marshal(toFile(s))
	if err != nil {
		return fmt.Errorf("detach: encode spec: %w", err)
	}
	return writeAtomic(dir, SpecFile, b)
}

// takeSpec reads dir/spec.json and unlinks it before anything runs, so a
// restarted unit finds no spec and cannot run the operation again. The
// error wraps fs.ErrNotExist when there is none.
func takeSpec(dir string) (Spec, error) {
	path := filepath.Join(dir, SpecFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	if err := os.Remove(path); err != nil {
		return Spec{}, fmt.Errorf("unlink %s: %w", path, err)
	}
	// A crash after this must not bring spec.json back: a restarted unit
	// would run the operation again, and its sensitive values would stay.
	if err := syncDir(dir); err != nil {
		return Spec{}, fmt.Errorf("sync %s after unlinking %s: %w", dir, path, err)
	}
	var f specFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Spec{}, fmt.Errorf("decode %s: %w", path, err)
	}
	s := fromFile(f)
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Exit is exit.json: how the child ended. ExitCode is -1 when the child was
// killed by a signal, named in Signal, or never started, explained in Error.
//
// Canceled and Interrupted say why the supervisor stopped the child on a
// SIGTERM or SIGINT: Canceled when nexusd asked for it (CancelFile exists)
// or an operator sent SIGINT, Interrupted when nobody asked, as at a host
// shutdown, when init or systemd sends SIGTERM to every process or to the
// unit. An interrupted operation's
// outcome is unknown: the child's exit status says how the stop ended it,
// not what it had done.
type Exit struct {
	ExitCode    int       `json:"exitCode"`
	Signal      string    `json:"signal,omitempty"` // "SIGKILL"
	Started     time.Time `json:"started"`
	Ended       time.Time `json:"ended"`
	TimedOut    bool      `json:"timedOut"`
	Canceled    bool      `json:"canceled"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Error       string    `json:"error,omitempty"`  // The child could not be started
	BootID      string    `json:"bootId,omitempty"` // The boot exit.json was written in; empty when unknown
}

// RequestCancel records in dir, by atomic rename and fsync, that nexusd asks
// for the operation's cancel. Launcher.Cancel calls it before it sends the
// cancel signal, so the supervisor that receives the signal finds it.
func RequestCancel(dir string) error {
	return writeAtomic(dir, CancelFile, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"))
}

// CancelRequested reports whether dir holds a cancel request, nexusd's or
// one a supervisor recorded for an operator's SIGINT. An error means
// unknown.
func CancelRequested(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, CancelFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("detach: check %s: %w", CancelFile, err)
}

// WriteExit writes x as dir/exit.json through a temporary file, fsync,
// rename and a directory fsync, so a reader sees no exit.json or a complete
// one, and a crash after the rename keeps it.
func WriteExit(dir string, x Exit) error {
	b, err := json.Marshal(x)
	if err != nil {
		return fmt.Errorf("detach: encode exit: %w", err)
	}
	return writeAtomic(dir, ExitFile, b)
}

// ReadExit reads dir/exit.json. ok is false when there is none yet.
func ReadExit(dir string) (x Exit, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, ExitFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Exit{}, false, nil
	}
	if err != nil {
		return Exit{}, false, fmt.Errorf("detach: read exit: %w", err)
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return Exit{}, false, fmt.Errorf("detach: decode exit: %w", err)
	}
	return x, true, nil
}

// errRenamedNotSynced is what writeAtomic wraps around the error of the
// directory sync that follows a successful rename: the file is in place and
// readable by anyone, and only its durability is in doubt.
var errRenamedNotSynced = errors.New("renamed into place, not synced to disk")

// writeAtomic replaces dir/name with b, mode 0600. An error that wraps
// errRenamedNotSynced means the new file is in place.
func writeAtomic(dir, name string, b []byte) (err error) {
	f, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("detach: write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	// CreateTemp already uses 0600; Chmod states the contract.
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("detach: write %s: %w", name, err), f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("detach: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("detach: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("detach: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("detach: rename %s: %w", name, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", errRenamedNotSynced, err)
	}
	return nil
}

// syncDir is syncDirOS; a test makes it fail.
var syncDir = syncDirOS

func syncDirOS(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("detach: open %s: %w", dir, err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return fmt.Errorf("detach: sync %s: %w", dir, serr)
	}
	if cerr != nil {
		return fmt.Errorf("detach: close %s: %w", dir, cerr)
	}
	return nil
}
