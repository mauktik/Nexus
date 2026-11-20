// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
)

// Every detached nginx operation runs as `nexusd nginx-op run --dir
// ops/<id>`, outside nexusd's sandbox (CON-2). Prepare writes
// ops/<id>/input.json, the helper writes ops/<id>/result.json, and Finalize
// only reads it. A restart of nexusd does not interrupt the helper; a
// helper that ends without a result ends outcome-unknown, naming what may
// have changed.

// nginxLane is the one lane of every detached nginx operation, including
// the config test and whatIf writes: nginx's files and service are one
// resource, so its operations run one at a time, in submission order
// (FR-13).
var nginxLane = provider.Lane{Key: "nginx", Policy: provider.LaneQueue}

// lane is the Lane of every detached nginx binding.
func lane[In any](context.Context, In) (provider.Lane, error) { return nginxLane, nil }

// The bounds of the helper's spec (Interfaces). helperMargin is the room
// past the sum of the helper's own bounds, helperSlack how much earlier than
// MaxRuntime the supervisor stops it, so the watcher's backstop never races
// the supervisor's timeout.
const (
	helperMargin = 60 * time.Second
	helperSlack  = 30 * time.Second
	helperGrace  = 10 * time.Second
)

// maxRuntime bounds a helper that runs every step any action has: up to
// three config tests, two waits for the service's state and two reload
// verifications, plus helperMargin.
func maxRuntime(s Settings) time.Duration {
	return 3*s.TestTimeout + 2*s.StateTimeout + 2*s.ReloadVerifyTimeout + helperMargin
}

// opDir is operation id's directory.
func (p *Provider) opDir(id string) (string, error) {
	dir := p.opsDir.Load()
	if dir == nil || *dir == "" {
		return "", errors.New("nginx: no operations directory: UseOpsDir was not called")
	}
	return filepath.Join(*dir, id), nil
}

// prepare writes input.json for action, with in's action fields, the
// settings in force, the init system and the nginx binary, and returns the
// helper's spec. The argv is nexusd's absolute path and fixed words: no
// shell and nothing from the request.
func (p *Provider) prepare(action string, in helperInput, r *provider.Run) (detach.Spec, error) {
	dir, err := p.opDir(r.ID())
	if err != nil {
		return detach.Spec{}, err
	}
	exe, err := p.executable()
	if err != nil {
		return detach.Spec{}, fmt.Errorf("nginx: locate nexusd: %w", err)
	}
	s := p.Settings()
	in.OperationID, in.Action = r.ID(), action
	in.Settings, in.InitSystem, in.Nginx = snapshot(s), string(p.plat.Init), p.nginx
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return detach.Spec{}, fmt.Errorf("nginx: create %s: %w", dir, err)
	}
	if err := writeJSONAtomic(dir, inputFile, in); err != nil {
		p.log.Warn("nginx operation not prepared", "operationId", r.ID(), "action", action, "error", err.Error())
		return detach.Spec{}, err
	}
	if p.afterPrepare != nil {
		p.afterPrepare(dir)
	}
	say(r.Output(), "nginx %s runs in detached operation %s", action, r.ID())
	limit := maxRuntime(s)
	return detach.Spec{
		Argv:       []string{exe, HelperCommand, "run", "--dir", dir},
		Dir:        dir,
		Timeout:    limit - helperSlack,
		Grace:      helperGrace,
		KillMode:   detach.KillProcess,
		MaxRuntime: limit,
	}, nil
}

// dropContent removes a write's staged content from the operation's
// directory. It runs once the operation's unit has ended, when no helper is
// left to remove the content itself (it normally does, as soon as its action
// is done): a helper that was never started, that was killed, or whose
// unit was lost. A helper still running after its supervisor was lost reads
// the content first and removes it itself, and one that cannot read it
// fails with nothing changed, so removing it here is safe at any moment. A
// failure is logged, not returned: it must not change the operation's
// outcome.
func (p *Provider) dropContent(dir string, r *provider.Run) {
	if err := removeContent(dir); err != nil {
		p.log.Warn("nginx staged content not removed", "operationId", r.ID(), "error", err.Error())
	}
}

// finalize turns the helper's result.json into action's result, or its
// error into the problem with its extensions. Without a result.json the
// outcome is unknown, unless the helper never started. It reads files only,
// and removes a write's staged content, which the unit's end makes
// garbage whatever the outcome (CR-B4).
func finalize[Out any](p *Provider, action string, x detach.Exit, r *provider.Run) (Out, error) {
	var none Out
	dir, err := p.opDir(r.ID())
	if err != nil {
		return none, err
	}
	p.dropContent(dir, r)
	var res helperResult
	ok, err := readJSON(dir, resultFile, &res)
	if err != nil {
		return none, err
	}
	if !ok {
		if x.Error != "" {
			return none, problem.New(problem.InternalError, fmt.Sprintf("the nginx helper for action %s could not "+
				"be started (%s); nothing was changed", action, x.Error))
		}
		how := fmt.Sprintf("exited with code %d", x.ExitCode)
		if x.Signal != "" {
			how = "was killed by " + x.Signal
		}
		return none, problem.New(problem.OutcomeUnknown, fmt.Sprintf("the nginx helper for action %s %s without "+
			"reporting a result%s", action, how, previousClause(dir)))
	}
	if res.Error != nil {
		return none, res.Error.problem()
	}
	if res.Result == nil {
		return none, fmt.Errorf("nginx: %s holds neither a result nor an error", resultFile)
	}
	var out Out
	if err := decodeStrict(res.Result, &out); err != nil {
		return none, fmt.Errorf("nginx: decode the %s result: %w", action, err)
	}
	return out, nil
}

// lostDetail is added to outcome-unknown when the helper's unit or process
// ended without exit.json: it names the action and, once a file was backed
// up, where its previous content is kept.
func (p *Provider) lostDetail(action string, r *provider.Run) string {
	detail := fmt.Sprintf("the nginx helper for action %s stopped without reporting a result", action)
	if dir, err := p.opDir(r.ID()); err == nil {
		p.dropContent(dir, r)
		detail += previousClause(dir)
	}
	return detail
}

// previousClause names the file a change may have left partly changed and
// where its previous content is kept, from previous/meta.json; it is empty
// when nothing was backed up. A file that did not exist before has no
// previous content: the clause says so, and that removing it undoes the
// change.
func previousClause(dir string) string {
	var meta previousFile
	ok, err := readJSON(dir, previousMetaFile, &meta)
	switch {
	case err != nil:
		return fmt.Sprintf("; a file may be partly changed, and the record of its previous content, %s, cannot be "+
			"read (%v)", filepath.Join(dir, previousMetaFile), err)
	case !ok:
		return ""
	case !meta.Exists:
		return fmt.Sprintf("; %s did not exist before and may have been created: there is no previous content, "+
			"and removing %s restores the state before the operation", meta.FileName, meta.FileName)
	}
	return fmt.Sprintf("; %s may be partly changed: its previous content (sha256 %s) is kept at %s", meta.FileName,
		meta.SHA256, filepath.Join(dir, previousContentFile))
}

// say writes one progress line to the operation log. A failed write to
// the log cannot be reported anywhere else, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}
