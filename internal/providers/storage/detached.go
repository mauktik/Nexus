// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
	"github.com/mauktik/nexus/internal/providers/storage/zfs"
)

// The three writes, scrub start, scrub stop and snapshot create, run as
// `nexusd storage-helper run --dir ops/<id>` outside nexusd's sandbox, as
// the ZFS ioctls need CAP_SYS_ADMIN (D-4). Prepare writes
// ops/<id>/input.json, the helper writes ops/<id>/result.json, and Finalize
// only reads it. None is cancelable once running (def.CancelQueuedOnly,
// D-17): the supervisor alone stops a helper, and how the operation then
// ends follows the core's rules (internal/ops/manager.go detachedEnd).

// The bounds of the helper's spec (DF-7 step 3). Its Timeout covers two
// commands (the pre-read and the write) and the confirm poll, plus
// helperMargin. Each read of the poll is bound by the confirm time left, so
// the poll ends within ConfirmTimeout and the helper within 2T +
// ConfirmTimeout; the watcher's MaxRuntime backstop is helperSlack later, so
// it never races the supervisor's own Timeout.
const (
	helperMargin = 30 * time.Second
	helperSlack  = 30 * time.Second
	helperGrace  = 10 * time.Second
)

// helperTimeout is the supervisor's Timeout of a helper whose every
// command is bounded by commandTimeout.
func helperTimeout(commandTimeout time.Duration) time.Duration {
	return 2*commandTimeout + spec.ConfirmTimeout + helperMargin
}

// writeBindings binds the three detached writes. Bindings composes them
// with the reads (WI 5.1).
func (p *Provider) writeBindings() []provider.Binding {
	return []provider.Binding{
		provider.Detached(spec.PoolsScrub, p.scrubStartSpec()),
		provider.Detached(spec.PoolsStopScrub, p.stopScrubSpec()),
		provider.Detached(spec.DatasetsSnapshot, p.snapshotSpec()),
	}
}

// UseOpsDir is called by the daemon with the operations directory; it
// implements provider.OpsDirUser.
func (p *Provider) UseOpsDir(dir string) { p.opsDir.Store(&dir) }

var _ provider.OpsDirUser = (*Provider)(nil)

// opDir is operation id's directory.
func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("storage: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// helperFormat is the form the helper asks the tools for: the last zfs
// check's (D-5), or text once the reads fell back to it for cmd (ASM-2).
func (p *Provider) helperFormat(cmd zfs.Command) zfs.Format {
	z := p.zfs.Load()
	if z == nil {
		return zfs.Text
	}
	if _, ok := p.reads.textOnly.Load(cmd); ok {
		return zfs.Text
	}
	return z.format
}

// launchHelper writes in as the operation's input.json and returns the
// helper's spec. The argv is nexusd's absolute path and fixed words: no
// shell and nothing from the request.
func (p *Provider) launchHelper(r *provider.Run, in any, commandTimeout time.Duration, subject string) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("storage: locate nexusd: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("storage: create %s: %w", dir, err)
	}
	if err := writeJSONAtomic(dir, inputFile, in); err != nil {
		return detach.Spec{}, err
	}
	sayOp(r.Output(), "%s runs in detached operation %s", subject, r.ID())
	timeout := helperTimeout(commandTimeout)
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "run", "--dir", dir},
		Dir:        dir,
		Timeout:    timeout,
		Grace:      helperGrace,
		KillMode:   detach.KillProcess,
		MaxRuntime: timeout + helperSlack,
	}, nil
}

// finalizeHelper turns the helper's result.json into the operation's
// result, or its error into the problem with its extensions (DF-7 step 7).
// Without a result.json, the outcome is unknown unless the helper never
// started, when nothing was sent. It reads files only.
func finalizeHelper[Out any](p *Provider, subject, check string, x detach.Exit, r *provider.Run) (Out, error) {
	var none Out
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	var res helperResult
	ok, err := readJSON(dir, resultFile, &res)
	if err != nil {
		return none, err
	}
	if !ok {
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("%s could not be started (%s); nothing was sent",
				subject, x.Error))
		}
		return none, problem.New(problem.OutcomeUnknown, lostDetail(subject, check))
	}
	if res.Error != nil {
		return none, res.Error.problem()
	}
	if res.Result == nil {
		return none, fmt.Errorf("storage: %s holds neither a result nor an error", resultFile)
	}
	var out Out
	if err := decodeStrict(res.Result, &out); err != nil {
		return none, fmt.Errorf("storage: decode the helper's result: %w", err)
	}
	return out, nil
}

// lostDetail is what a helper that stopped without a result leaves for the
// operator: which helper, and the command that shows what it did. The core
// appends it to its outcome-unknown when the unit was interrupted or lost,
// and Finalize gives it when no result.json exists.
func lostDetail(subject, check string) string {
	return fmt.Sprintf("%s stopped without a result; check `%s`", subject, check)
}

// sayOp writes one progress line to the operation log. A failed write to
// the log cannot be reported anywhere else, so it is dropped.
func sayOp(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}
