// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Code review B, CR-B4 and security review SEC-4: the staged content of a
// write (the site's text, mode 0600) is removed when the operation's unit
// has ended without the helper removing it, not left until retention.

package nginx

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nginx/spec"
	"github.com/mauktik/nexus/internal/svcctl"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// preparedWrite is an update that was admitted and prepared, so its content
// is staged, and whose helper never ran.
func preparedWrite(t *testing.T) (w *writeRig, in any, d provider.DetachedFuncs, r *provider.Run, dir string) {
	t.Helper()
	w = newWriteLC(t, ubuntu, svcctltest.Model{State: svcctl.Running})
	writeTree(t, w.f.dir, map[string]string{"a.conf": validSite})
	b, d := w.binding(spec.Update.Op())
	raw, err := json.Marshal(spec.WriteInput{Name: "a", Content: "server {\n    listen 8241;\n}\n"})
	if err != nil {
		t.Fatal(err)
	}
	in, err = b.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	r = provider.NewRun("op-staged", nil)
	sp, err := d.Prepare(context.Background(), in, r)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(sp.Dir, "content")
	if got := readString(t, staged); got == "" {
		t.Fatalf("Prepare staged no content at %s", staged)
	}
	return w, in, d, r, sp.Dir
}

// nginx-provider CR-B4 A: Finalize of a helper that could not be started removes the content
func TestFinalizeRemovesContentOfAHelperThatNeverStarted(t *testing.T) {
	_, in, d, r, dir := preparedWrite(t)

	_, err := d.Finalize(context.Background(), in, detach.Exit{Error: "fork/exec: no such file or directory"}, r)
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != problem.InternalError {
		t.Fatalf("Finalize = %v, want internal-error for a helper that was not started", err)
	}
	absent(t, filepath.Join(dir, "content"))
}

// nginx-provider CR-B4 B: Finalize of a helper that was killed removes the content
func TestFinalizeRemovesContentOfAKilledHelper(t *testing.T) {
	_, in, d, r, dir := preparedWrite(t)

	_, err := d.Finalize(context.Background(), in, detach.Exit{ExitCode: -1, Signal: "killed"}, r)
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != problem.OutcomeUnknown {
		t.Fatalf("Finalize = %v, want outcome-unknown for a helper that was killed", err)
	}
	absent(t, filepath.Join(dir, "content"))
}

// nginx-provider CR-B4 C: the lost-helper detail removes the content
func TestLostDetailRemovesContent(t *testing.T) {
	w, _, _, r, dir := preparedWrite(t)

	if d := w.f.p.lostDetail("update", r); d == "" {
		t.Error("lostDetail is empty")
	}
	absent(t, filepath.Join(dir, "content"))
}
