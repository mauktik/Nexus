// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// Files of the storage helper in the operation's directory, beside
// spec.json, exit.json and output/. Both are 0600 in the 0700 ops/<id>/.
const (
	inputFile  = "input.json"  // Written by Prepare in nexusd
	resultFile = "result.json" // Written by the helper, read by Finalize
)

// The helper's actions, input.json's action.
const (
	actionScrubStart = "scrub-start"
	actionScrubStop  = "scrub-stop"
	actionSnapshot   = "snapshot"
)

// scrubHelperInput is input.json of scrub-start and scrub-stop (DF-7 step
// 3). It names no tool path: the helper resolves zpool itself (D-16).
type scrubHelperInput struct {
	OperationID    string    `json:"operationId"`
	Action         string    `json:"action"`
	Pool           string    `json:"pool"`
	Force          bool      `json:"force"`
	Format         string    `json:"format"`         // json or text (D-5)
	CommandTimeout string    `json:"commandTimeout"` // A Go duration, 5s..120s
	CommandedAt    time.Time `json:"commandedAt"`    // The provider's clock at Prepare (A-25)
}

// snapshotHelperInput is input.json of snapshot (DF-8 step 3).
type snapshotHelperInput struct {
	OperationID    string `json:"operationId"`
	Action         string `json:"action"`
	Dataset        string `json:"dataset"`
	Snapshot       string `json:"snapshot"` // The name chosen at admission, without the dataset (D-11)
	Recursive      bool   `json:"recursive"`
	Format         string `json:"format"`
	CommandTimeout string `json:"commandTimeout"`
}

// inputFields are the members of each action's input.json, which the
// helper's strict reader accepts and nothing else (D-16).
var inputFields = map[string][]string{
	actionScrubStart: {"operationId", "action", "pool", "force", "format", "commandTimeout", "commandedAt"},
	actionScrubStop:  {"operationId", "action", "pool", "force", "format", "commandTimeout", "commandedAt"},
	actionSnapshot:   {"operationId", "action", "dataset", "snapshot", "recursive", "format", "commandTimeout"},
}

// helperResult is result.json: the action's result, or the problem it
// failed with. The helper always writes one or the other.
type helperResult struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *helperError    `json:"error,omitempty"`
}

// helperError is a failed action's problem: its slug, detail and
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
		e.Detail += fmt.Sprintf(" (its %s cannot be encoded: %s)", key, boundReason(err.Error()))
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
// where the type declares one. A slug the registry does not know is an
// internal error with the helper's detail.
func (e *helperError) problem() *problem.Error {
	t, ok := problem.Lookup(e.Slug)
	if !ok {
		return problem.New(problem.InternalError, e.Detail)
	}
	p := problem.New(t.Slug, e.Detail)
	for _, ext := range t.Extensions {
		if raw, ok := e.Ext[ext]; ok {
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				p.With(ext, v)
			}
		} else if ext == "provider" {
			p.With(ext, spec.Name)
		}
	}
	return p
}

// readJSON decodes dir/name into v. ok is false when the file does not
// exist; any other failure is an error.
func readJSON(dir, name string, v any) (ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: read %s: %w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("storage: decode %s: %w", name, err)
	}
	return true, nil
}

// decodeStrict decodes b into v, refusing a member v does not have and
// anything after the value.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("data after the JSON value")
	}
	return nil
}

// writeJSONAtomic replaces dir/name with v as JSON, mode 0600, through
// dir/.<name>.tmp, fsync, rename and a directory fsync: a reader sees no
// file or a complete one. The temporary name is fixed (the directory has one
// writer) and opened without following a symlink.
func writeJSONAtomic(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("storage: encode %s: %w", name, err)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("storage: write %s: %w", name, err)
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
		return errors.Join(fmt.Errorf("storage: write %s: %w", name, err), f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("storage: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("storage: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("storage: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("storage: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("storage: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("storage: sync %s: %w", dir, serr)
	}
	return nil
}
