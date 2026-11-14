package nginx

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
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
)

// Files of the nginx helper in the operation's directory, beside spec.json,
// exit.json and output/. All are 0600 in the 0700 ops/<id>/.
const (
	inputFile  = "input.json"  // Written by Prepare in nexusd
	resultFile = "result.json" // Written by the helper, read by Finalize
	// The backup of a file a change replaces (files.go): its bytes, and
	// meta.json, which appears last, once the backup is complete.
	previousMetaFile    = "previous/meta.json"
	previousContentFile = "previous/content"
)

// settingsSnapshot is the nginx section in force when the operation was
// prepared. The helper runs with these values even if nexusd reloads its
// configuration meanwhile.
type settingsSnapshot struct {
	IncludeDir          string        `json:"includeDir"`
	IncludeSuffix       string        `json:"includeSuffix"`
	DisabledSuffix      string        `json:"disabledSuffix"`
	Service             string        `json:"service"`
	TestTimeout         time.Duration `json:"testTimeout"`
	StateTimeout        time.Duration `json:"stateTimeout"`
	ReloadVerifyTimeout time.Duration `json:"reloadVerifyTimeout"`
	MaxConfigBytes      int64         `json:"maxConfigBytes"`
}

func snapshot(s Settings) settingsSnapshot {
	return settingsSnapshot{
		IncludeDir: s.IncludeDir, IncludeSuffix: s.IncludeSuffix, DisabledSuffix: s.DisabledSuffix, Service: s.Service,
		TestTimeout: s.TestTimeout, StateTimeout: s.StateTimeout, ReloadVerifyTimeout: s.ReloadVerifyTimeout,
		MaxConfigBytes: s.MaxConfigBytes,
	}
}

// settings is the snapshot as Settings, the type the helper's building
// blocks take.
func (s settingsSnapshot) settings() Settings {
	return Settings{
		Enabled: true, IncludeDir: s.IncludeDir, IncludeSuffix: s.IncludeSuffix, DisabledSuffix: s.DisabledSuffix,
		Service: s.Service, TestTimeout: s.TestTimeout, StateTimeout: s.StateTimeout,
		ReloadVerifyTimeout: s.ReloadVerifyTimeout, MaxConfigBytes: s.MaxConfigBytes,
	}
}

// helperInput is input.json: everything the helper acts on. It comes only
// from code and the root-owned configuration (CON-3). Each action's work
// item adds the fields it needs.
type helperInput struct {
	OperationID string           `json:"operationId"`
	Action      string           `json:"action"`
	Settings    settingsSnapshot `json:"settings"`
	InitSystem  string           `json:"initSystem"`
	Nginx       string           `json:"nginx"`          // The nginx binary's absolute path
	Name        string           `json:"name,omitempty"` // The site of enable, disable, add and update
	// Add and update (write.go). Their content is in the operation's
	// content file, never here.
	Create  bool   `json:"create,omitempty"`  // add: the site must not exist yet
	IfMatch string `json:"ifMatch,omitempty"` // update: the current SHA-256, or none
	Enabled *bool  `json:"enabled,omitempty"` // The state of a site the write creates
	WhatIf  bool   `json:"whatIf,omitempty"`  // Test on a shadow copy only
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

// failure is a helperError of slug with detail and no extensions.
func failure(slug problem.Slug, format string, args ...any) *helperError {
	return &helperError{Slug: slug, Detail: fmt.Sprintf(format, args...)}
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

// previousFile is the part of previous/meta.json that names what a change
// replaced, so an operator can restore it by hand if the helper is lost.
// Exists is false when there was no file, so no previous content is kept.
type previousFile struct {
	FileName string `json:"fileName"`
	Exists   bool   `json:"exists"`
	SHA256   string `json:"sha256"`
}

// readJSON decodes dir/name into v. ok is false when the file does not
// exist; any other failure is an error.
func readJSON(dir, name string, v any) (ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("nginx: read %s: %w", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("nginx: decode %s: %w", name, err)
	}
	return true, nil
}

// decodeStrict decodes b into v, refusing a member v does not have: a
// result.json written by another build is an error, not a quiet gap.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeJSONAtomic replaces dir/name with v as JSON, mode 0600, through
// dir/.<name>.tmp, fsync, rename and a directory fsync: a reader sees no
// file or a complete one. The temporary name is fixed (the directory has one
// writer) and opened without following a symlink.
func writeJSONAtomic(dir, name string, v any) (err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("nginx: encode %s: %w", name, err)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("nginx: write %s: %w", name, err)
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
		return errors.Join(fmt.Errorf("nginx: write %s: %w", name, err), f.Close())
	}
	if _, err := f.Write(b); err != nil {
		return errors.Join(fmt.Errorf("nginx: write %s: %w", name, err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("nginx: sync %s: %w", name, err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("nginx: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("nginx: rename %s: %w", name, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("nginx: open %s: %w", dir, err)
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	if serr != nil {
		return fmt.Errorf("nginx: sync %s: %w", dir, serr)
	}
	return nil
}
