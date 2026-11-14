package nexusd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// reloadAccepted POSTs reload and returns the operation ID of its 202.
func (s *stack) reloadAccepted(t *testing.T, header http.Header) string {
	t.Helper()
	r := s.action(t, "reload", s.admin, header).want(t, http.StatusAccepted)
	op := decodeJSON[apitypes.Operation](t, r.body)
	if op.ID == "" || op.Action != spec.Reload.Op().ID {
		t.Fatalf("202 body %s, want a reload operation", r.body)
	}
	return op.ID
}

// TC-SM-54 (WI 3.5; FR-4): reload over REST applies a valid config and reports restartRequired
func TestTCSM54_ReloadOverRESTAppliesAValid(t *testing.T) {
	s := newStack(t, stackConfig{})
	s.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		return provider.ReloadResult{RestartRequired: []string{"limits"}}, nil
	})

	// Step 1.
	id := s.reloadAccepted(t, nil)

	// Step 2.
	op := s.awaitOp(t, id)
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("status %s, error %+v", op.Status, op.Error)
	}
	var got map[string]any
	if err := json.Unmarshal(op.Result, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"changed": true, "applied": true, "restartRequired": []any{"limits"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result %v, want %v", got, want)
	}
	if n := s.daemon.reloadCount(); n != 1 {
		t.Errorf("daemon saw %d reloads, want 1", n)
	}
	if ops := s.operations(t); len(ops) != 1 || ops[0].ID != id {
		t.Errorf("operations %+v, want only %s", ops, id)
	}
	outs := outcomes(s.auditOf(t, spec.Reload.Op().ID))
	if len(outs) == 0 || outs[0] != "requested" || outs[len(outs)-1] != string(apitypes.StatusSucceeded) {
		t.Errorf("audit outcomes %v, want requested first and %s last", outs, apitypes.StatusSucceeded)
	}
}

// A reload with nothing to restart reports restartRequired as [], never
// null.
func TestReloadRestartRequiredIsNeverNull(t *testing.T) {
	s := newStack(t, stackConfig{})
	op := s.awaitOp(t, s.reloadAccepted(t, nil))
	if op.Status != apitypes.StatusSucceeded || !strings.Contains(string(op.Result), `"restartRequired":[]`) {
		t.Errorf("status %s result %s, want restartRequired []", op.Status, op.Result)
	}
}

// TC-SM-56 (WI 3.5; FR-11): A reload with an Idempotency-Key replays the same operation
func TestTCSM56_AReloadWithAnIdempotencyKey(t *testing.T) {
	s := newStack(t, stackConfig{})
	h := http.Header{"Idempotency-Key": {"r1"}}
	first := s.reloadAccepted(t, h)
	second := s.reloadAccepted(t, h)
	if first != second {
		t.Errorf("op IDs %s and %s, want the same", first, second)
	}
	s.awaitOp(t, first)
	if n := s.daemon.reloadCount(); n != 1 {
		t.Errorf("daemon saw %d reloads, want 1", n)
	}
	outs := outcomes(s.auditOf(t, spec.Reload.Op().ID))
	if n := countOf(outs, "requested"); n != 1 {
		t.Errorf("audit outcomes %v: %d requested records, want 1", outs, n)
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// TC-SM-ERR27 (WI 3.5; FR-4, FR-3, FR-16): Reload with an invalid file is refused config-invalid with no op
func TestTCSMERR27_ReloadWithAnInvalidFileIs(t *testing.T) {
	bad := fixture(t, "unknown-key.yaml")
	s := newStack(t, stackConfig{daemon: newFakeDaemon(bad, t.TempDir(), time.Now())})

	r := s.action(t, "reload", s.admin, nil).want(t, http.StatusUnprocessableEntity)
	body := problemOf(t, r.body)
	if slug := slugOf(body); slug != string(problem.ConfigInvalid) {
		t.Fatalf("slug %s, want config-invalid", slug)
	}
	if body["configPath"] != bad {
		t.Errorf("configPath %v, want %s", body["configPath"], bad)
	}
	_, list := checkConfig(t, bad)
	want := make([]any, 0, len(list))
	for _, e := range list {
		m := map[string]any{"message": e.Msg}
		if e.File != "" {
			m["file"] = e.File
		}
		if e.Line != 0 {
			m["line"] = float64(e.Line)
		}
		if e.Column != 0 {
			m["column"] = float64(e.Column)
		}
		if e.Path != "" {
			m["path"] = e.Path
		}
		want = append(want, m)
	}
	if len(want) == 0 || !reflect.DeepEqual(body["errors"], want) {
		t.Errorf("errors %v, want CheckConfig's %v", body["errors"], want)
	}

	if ops := s.operations(t); len(ops) != 0 {
		t.Errorf("operations %+v, want none", ops)
	}
	if n := s.daemon.reloadCount(); n != 0 {
		t.Errorf("daemon saw %d reloads, want 0", n)
	}
	outs := outcomes(s.auditOf(t, spec.Reload.Op().ID))
	if !slices.Contains(outs, "refused:config-invalid") {
		t.Errorf("audit outcomes %v, want a refused config-invalid record", outs)
	}
}

// TC-SM-ERR28 (WI 3.5; FR-4): A provider refusal during apply fails the op with config-apply-failed
func TestTCSMERR28_AProviderRefusalDuringApplyFails(t *testing.T) {
	s := newStack(t, stackConfig{})
	s.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		return provider.ReloadResult{}, &provider.ReloadError{Stage: "apply", Err: errors.New("docker: socket missing")}
	})

	op := s.awaitOp(t, s.reloadAccepted(t, nil))
	if op.Status != apitypes.StatusFailed || op.Error == nil {
		t.Fatalf("status %s error %+v, want Failed with an error", op.Status, op.Error)
	}
	if !strings.HasSuffix(op.Error.Type, string(problem.ConfigApplyFailed)) ||
		!strings.Contains(op.Error.Detail, "docker: socket missing") {
		t.Errorf("error %s %q, want config-apply-failed naming the provider's refusal", op.Error.Type, op.Error.Detail)
	}
}

// A reload the daemon could not run for another reason (it is stopping, or
// the caller left) is not a config-apply-failed.
func TestReloadOtherErrorsAreNotApplyFailures(t *testing.T) {
	p := New(Options{})
	d := newFakeDaemon("/nonexistent", t.TempDir(), time.Now())
	stopping := errors.New("nexusd is stopping")
	d.onReload(func(context.Context) (provider.ReloadResult, error) { return provider.ReloadResult{}, stopping })
	p.UseDaemon(d)
	_, err := p.reload(t.Context(), spec.ReloadInput{}, provider.NewRun("op", nil))
	var perr *problem.Error
	if !errors.Is(err, stopping) || errors.As(err, &perr) {
		t.Errorf("reload = %v, want the daemon's error unmapped", err)
	}

	if _, err := New(Options{}).reload(t.Context(), spec.ReloadInput{}, provider.NewRun("op", nil)); err == nil {
		t.Error("reload without a daemon succeeded")
	}
	if err := New(Options{}).admitReload(t.Context(), spec.ReloadInput{}); err == nil {
		t.Error("admitReload without a daemon succeeded")
	}
}

// Reload holds the nexusd lane with the reject policy (FR-11).
func TestReloadLane(t *testing.T) {
	for _, b := range New(Options{}).Bindings() {
		if b.Op().ID != spec.Reload.Op().ID {
			continue
		}
		funcs, ok := b.InProcess()
		if !ok {
			t.Fatal("reload is not in-process")
		}
		l, err := funcs.Lane(t.Context(), spec.ReloadInput{})
		if err != nil || l != (provider.Lane{Key: spec.LaneKey, Policy: provider.LaneReject}) {
			t.Errorf("lane %+v %v, want nexusd/reject", l, err)
		}
		return
	}
	t.Fatal("reload is not bound")
}
