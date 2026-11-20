// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
)

// Files of the services helper in the operation's directory, beside
// spec.json, exit.json and output/. Both are 0600 in the 0700 ops/<id>/.
// Their types only ever gain fields: a helper of one build may read what
// nexusd of another wrote across an upgrade.
const (
	helperInputFile  = "input.json"  // Written by Prepare in nexusd
	helperResultFile = "result.json" // Written by the helper, read by Finalize
)

// The helper's modes, by input.json's mode.
const helperModeControl = "control"

// helperHeader is what every input.json holds, whatever its mode.
type helperHeader struct {
	Mode        string `json:"mode"`
	OperationID string `json:"operationId"`
}

// controlHelperInput is input.json of start, stop, restart and control: the
// allow-list entry, the init system, the canonical unit and the resolved
// protected set, all as they were when the operation was prepared (DF-3
// step 3). The helper acts on these alone, so a reload while the operation
// runs changes nothing about it.
type controlHelperInput struct {
	Mode        string `json:"mode"`
	OperationID string `json:"operationId"`
	Service     string `json:"service"`
	Action      string `json:"action"`
	Init        string `json:"init"`
	// CanonicalName is the unit's Id without .service (systemd), or the
	// script name (OpenRC), as Prepare read it.
	CanonicalName  string   `json:"canonicalName"`
	TimeoutSeconds float64  `json:"timeoutSeconds"`
	Actions        []string `json:"actions"`
	// Protected is the resolved protected set: the static list, nexusd, the
	// unit running it and its dependencies, each with its reason (D-6).
	Protected []protectEntry `json:"protected"`
}

// helperResult is result.json: the mode's result, or the problem it failed
// with. The helper always writes one or the other.
type helperResult struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *helperError    `json:"error,omitempty"`
}

// helperError is a failed helper's problem: its slug, detail and
// extensions. Finalize keeps the extensions the problem type declares.
type helperError struct {
	Slug   problem.Slug               `json:"slug"`
	Detail string                     `json:"detail"`
	Ext    map[string]json.RawMessage `json:"ext,omitempty"`
}

// helperFailure is a helperError of slug with detail and no extensions.
func helperFailure(slug problem.Slug, format string, args ...any) *helperError {
	return &helperError{Slug: slug, Detail: fmt.Sprintf(format, args...)}
}

// with sets extension key of e to v.
func (e *helperError) with(key string, v any) *helperError {
	b, err := json.Marshal(v)
	if err != nil {
		e.Detail += fmt.Sprintf(" (its %s cannot be encoded: %v)", key, err)
		return e
	}
	if e.Ext == nil {
		e.Ext = map[string]json.RawMessage{}
	}
	e.Ext[key] = b
	return e
}

// problem is the operation's problem: the type's own slug and detail, with
// each extension the type declares that the helper gave, and the provider
// where the type declares one.
func (e *helperError) problem() *problem.Error {
	t, ok := problem.Lookup(e.Slug)
	if !ok {
		return problem.New(problem.InternalError, e.Detail)
	}
	p := problem.New(t.Slug, e.Detail)
	for _, ext := range t.Extensions {
		if v, ok := e.Ext[ext]; ok {
			p.With(ext, v)
		} else if ext == "provider" {
			p.With(ext, spec.Name)
		}
	}
	return p
}

// finalizeHelper turns dir's result.json into Out, or its error into the
// problem with its extensions. Without a result.json the outcome is
// unknown, unless the helper never started. what names the operation in
// those details ("start of nx-svc-test"). It reads files only.
func finalizeHelper[Out any](dir, what string, x detach.Exit) (Out, error) {
	var none Out
	var res helperResult
	ok, err := readHelperJSON(dir, helperResultFile, &res)
	if err != nil {
		return none, err
	}
	if !ok {
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("the services helper for the %s could not be "+
				"started (%s); nothing was changed", what, x.Error))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		return none, problem.New(problem.OutcomeUnknown, fmt.Sprintf("the services helper for the %s %s without "+
			"reporting a result, so whether it changed anything is unknown", what, how))
	}
	if res.Error != nil {
		return none, res.Error.problem()
	}
	if res.Result == nil {
		return none, fmt.Errorf("services: %s holds neither a result nor an error", helperResultFile)
	}
	var out Out
	if err := decodeHelperStrict(res.Result, &out); err != nil {
		return none, fmt.Errorf("services: decode the %s result: %w", what, err)
	}
	return out, nil
}

// maxHelperFileBytes bounds a read of the helper's input.json or
// result.json: far above what either holds, and a limit on what a damaged
// or replaced file can make nexusd read.
const maxHelperFileBytes = 1 << 20

// readHelperFile is the content of dir/name. ok is false when the file does
// not exist; any other failure is an error. The file is opened without
// following a final symlink and without blocking, and must be a regular
// file of at most maxHelperFileBytes (readRegular).
func readHelperFile(dir, name string) (b []byte, ok bool, err error) {
	b, err = readRegular(filepath.Join(dir, name), maxHelperFileBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("services: read %s: %w", name, err)
	}
	return b, true, nil
}

// readHelperJSON decodes dir/name into v. ok is false when the file does
// not exist; any other failure is an error.
func readHelperJSON(dir, name string, v any) (ok bool, err error) {
	b, ok, err := readHelperFile(dir, name)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("services: decode %s: %w", name, err)
	}
	return true, nil
}

// decodeHelperStrict decodes b into v, refusing a member v does not have:
// a result written by another build is an error, not a quiet gap.
func decodeHelperStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeHelperJSON replaces dir/name with v as JSON, mode 0600, through
// dir/.<name>.tmp, fsync, rename and a directory fsync: a reader sees no
// file or a complete one. The temporary name is fixed (the directory has
// one writer) and opened without following a symlink.
func writeHelperJSON(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("services: encode %s: %w", name, err)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("services: write %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, rerr)
			}
		}
	}()
	// A file left by an earlier attempt keeps its mode through O_TRUNC.
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("services: write %s: %w", name, err), f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("services: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("services: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("services: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("services: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("services: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("services: sync %s: %w", dir, serr)
	}
	return nil
}
