// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package pihole

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
)

// LaneKey is the lane every pihole mutation queues on, FIFO: one Pi-hole
// change at a time, in arrival order.
const LaneKey = "pihole"

// The MaxRuntime of each mutation (A-3).
const (
	maxRuntimeDefault = 10 * time.Minute
	maxRuntimeGravity = time.Hour
)

// helperDeadlineMargin is how long before MaxRuntime the helper's own
// context ends, so it can still write result.json before the watcher's
// backstop.
const helperDeadlineMargin = time.Minute

// piholeLane is the lane of every pihole mutation.
func piholeLane[In any](context.Context, In) (provider.Lane, error) {
	return provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}, nil
}

// detachedOp binds op as a detached pihole mutation: admit (optional) runs
// in nexusd before the operation exists; Prepare writes input.json and
// returns the helper's unit spec; Finalize maps result.json; lost
// (optional) is the outcome-unknown detail of a unit lost without a result.
func detachedOp[In, Out any](p *Provider, op def.Typed[In, Out], admit func(context.Context, In) error,
	maxRuntime time.Duration, lost func(*provider.Run, In) string) provider.Binding {
	id := op.Op().ID
	return provider.Detached(op, provider.DetachedSpec[In, Out]{
		Admit: admit,
		Lane:  piholeLane[In],
		Prepare: func(ctx context.Context, in In, r *provider.Run) (detach.Spec, error) {
			return p.prepareHelper(ctx, id, in, maxRuntime, r)
		},
		Finalize: func(_ context.Context, in In, x detach.Exit, r *provider.Run) (Out, error) {
			return finalizeHelper[Out](p, x, r, func() string {
				if lost == nil {
					return ""
				}
				return lost(r, in)
			})
		},
		LostDetail: lost,
	})
}

func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("pihole: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// prepareHelper writes input.json (0600, atomic) and returns the helper's
// spec. The input travels only in the file; the argv is fixed words and the
// operation's directory (CON-2).
func (p *Provider) prepareHelper(ctx context.Context, op string, in any, maxRuntime time.Duration,
	r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	rd, err := p.backendReader(ctx)
	if err != nil {
		return detach.Spec{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("pihole: locate nexusd: %w", err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return detach.Spec{}, fmt.Errorf("pihole: encode input: %w", err)
	}
	hin := helperInput{
		OperationID: r.ID(), Op: op, Backend: rd.Name(), Input: raw,
		DNSFile: p.Settings().DNSFile, StateDir: p.stateDir(), OpDir: dir,
		Deadline: time.Now().Add(maxRuntime - helperDeadlineMargin).UTC(),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("pihole: create %s: %w", dir, err)
	}
	if err := writeJSONAtomic(dir, inputFile, hin); err != nil {
		return detach.Spec{}, err
	}
	fmt.Fprintf(r.Output(), "pihole: running %s in unit %s\n", op, detach.Unit(r.ID()))
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "--dir", dir},
		Env:        v5.Env(),
		Dir:        "/",
		KillMode:   detach.KillProcess,
		MaxRuntime: maxRuntime,
	}, nil
}

// finalizeHelper states the outcome from result.json. Without one it is
// internal-error when the helper never started, else outcome-unknown with
// the op's LostDetail. It only reads files.
func finalizeHelper[Out any](p *Provider, x detach.Exit, r *provider.Run, lost func() string) (Out, error) {
	var none Out
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	b, err := os.ReadFile(filepath.Join(dir, resultFile))
	if errors.Is(err, fs.ErrNotExist) {
		if x.Error != "" {
			return none, probErr(problem.New(problem.InternalError, fmt.Sprintf("the pi-hole helper could not be "+
				"started (%s); nothing was changed", x.Error)))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		detail := fmt.Sprintf("the pi-hole helper %s without writing %s", how, resultFile)
		if l := lost(); l != "" {
			detail += "; " + l
		}
		return none, probErr(problem.New(problem.OutcomeUnknown, detail))
	}
	if err != nil {
		return none, fmt.Errorf("pihole: read %s: %w", resultFile, err)
	}
	var res helperResult
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return none, fmt.Errorf("pihole: decode %s: %w", resultFile, err)
	}
	if res.Error != nil {
		return none, probErr(res.Error.problem())
	}
	var out Out
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return none, fmt.Errorf("pihole: decode the result in %s: %w", resultFile, err)
	}
	return out, nil
}

// problem is the helper's error as a problem of its type, with each
// extension its type declares. An unknown slug is internal-error.
func (e *helperError) problem() *problem.Error {
	t, ok := problem.Lookup(e.Slug)
	if !ok {
		return problem.New(problem.InternalError, e.Detail)
	}
	pe := problem.New(t.Slug, e.Detail)
	keys := make([]string, 0, len(e.Ext))
	for k := range e.Ext {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if slices.Contains(t.Extensions, k) {
			pe.With(k, jsonValue(e.Ext[k]))
		}
	}
	return pe
}

// jsonValue turns the json.Number values of a decoded extension back into
// an int when integral, else a float64, so an exitCode reads as the int
// the helper set.
func jsonValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonValue(e)
		}
		return out
	}
	return v
}
