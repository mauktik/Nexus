package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/systemspec"
)

// readScoped is a provider with only a read scope, the provider named by the
// operation records below.
type readScoped struct{}

func (readScoped) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: "recs", ReadScope: "recs:read"}
}
func (readScoped) Configure(func(any) error) error { return nil }
func (readScoped) Preflight(context.Context) provider.Status {
	return provider.Status{State: apitypes.ProviderEnabled}
}
func (readScoped) Bindings() []provider.Binding { return nil }

const recID = "01890a5d-ac96-774b-bcce-b302099a8057"

// fromTargetFixture binds system.operations.get and .cancel ahead of 3.2,
// with spy handlers, so the FromTarget path can be exercised now.
func fromTargetFixture(t *testing.T, load TargetLoader) (*Dispatcher, *atomic.Int64) {
	t.Helper()
	// Cancel is mutating, so it needs an audit log to write requested to.
	lg, err := audit.Open(audit.Options{Dir: t.TempDir(), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	reg, err := New([]provider.Provider{readScoped{}}, Options{
		Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Providers: map[string]ProviderConfig{"recs": {Enabled: true}},
		Audit:     lg,
	})
	if err != nil {
		t.Fatal(err)
	}
	var handled atomic.Int64
	h := func(_ context.Context, in systemspec.OperationInput) (apitypes.Operation, error) {
		handled.Add(1)
		return apitypes.Operation{ID: in.ID}, nil
	}
	reg.entries = append(reg.entries,
		Entry{Provider: def.SystemProvider, Binding: provider.Sync(systemspec.OperationsGet, h)},
		Entry{Provider: def.SystemProvider, Binding: provider.Sync(systemspec.OperationsCancel, h)},
	)
	return NewDispatcher(reg, DispatcherOptions{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), LoadTarget: load}), &handled
}

func call(d *Dispatcher, op def.Op, id string, scopes ...string) error {
	_, err := d.Invoke(context.Background(), InvokeRequest{
		Caller: auth.Principal{TokenID: "a1b2c3d4e5f6", Scopes: scopes},
		Op:     op, Target: id, Input: json.RawMessage(`{"id":"` + id + `"}`),
	})
	return err
}

func problemOf(t *testing.T, err error, status int) *problem.Error {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) || p.Status != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
	return p
}

// A FromTarget op loads the record first (not-found when absent), then
// requires the read scope of the record's provider, or for cancel the
// record's StartScope, which scripts:run satisfies for any script.
func TestFromTargetLoadsTheRecordBeforeTheScopeCheck(t *testing.T) {
	var loads atomic.Int64
	var loadedID atomic.Value
	load := func(_ context.Context, id string) (TargetRecord, error) {
		loads.Add(1)
		loadedID.Store(id)
		switch id {
		case recID:
			return TargetRecord{Provider: "recs", StartScope: "scripts:run:deploy"}, nil
		case "01890a5d-ac96-774b-bcce-b302099a8058":
			return TargetRecord{}, errors.New("bbolt: database not open")
		}
		return TargetRecord{}, ErrTargetNotFound
	}
	d, handled := fromTargetFixture(t, load)
	get, cancel := systemspec.OperationsGet.Op(), systemspec.OperationsCancel.Op()

	// Absent record: not-found, even for a caller with no scopes at all.
	problemOf(t, call(d, get, "01890a5d-ac96-774b-bcce-b302099a8059"), http.StatusNotFound)
	if loads.Load() != 1 {
		t.Fatalf("loader called %d times, want 1 before the scope check", loads.Load())
	}
	problemOf(t, call(d, get, "01890a5d-ac96-774b-bcce-b302099a8058", "recs:read"), http.StatusInternalServerError)

	p := problemOf(t, call(d, get, recID, "scripts:run"), http.StatusForbidden)
	if p.Extensions["requiredScope"] != "recs:read" {
		t.Fatalf("requiredScope = %v, want recs:read", p.Extensions["requiredScope"])
	}
	if err := call(d, get, recID, "recs:read"); err != nil {
		t.Fatalf("get with the provider's read scope: %v", err)
	}

	p = problemOf(t, call(d, cancel, recID, "recs:read"), http.StatusForbidden)
	if p.Extensions["requiredScope"] != "scripts:run:deploy" {
		t.Fatalf("requiredScope = %v, want scripts:run:deploy", p.Extensions["requiredScope"])
	}
	problemOf(t, call(d, cancel, recID, "scripts:run:other"), http.StatusForbidden)
	for _, s := range []string{"scripts:run:deploy", "scripts:run"} {
		if err := call(d, cancel, recID, s); err != nil {
			t.Fatalf("cancel with %s: %v", s, err)
		}
	}

	// The record is loaded by the ID the input names, not by Target, so the
	// authorized record and the handled record cannot differ.
	_, err := d.Invoke(context.Background(), InvokeRequest{
		Caller: auth.Principal{TokenID: "a1b2c3d4e5f6", Scopes: []string{"recs:read"}},
		Op:     get, Target: recID, Input: json.RawMessage(`{"id":"01890a5d-ac96-774b-bcce-b302099a8059"}`),
	})
	problemOf(t, err, http.StatusNotFound)
	if got := loadedID.Load(); got != "01890a5d-ac96-774b-bcce-b302099a8059" {
		t.Fatalf("loaded %v, want the input's ID", got)
	}
	if n := handled.Load(); n != 3 {
		t.Fatalf("handler ran %d times, want 3", n)
	}
}

// Without a loader (before 3.2 wires the store) no record exists.
func TestFromTargetWithoutALoaderIsNotFound(t *testing.T) {
	d, handled := fromTargetFixture(t, nil)
	problemOf(t, call(d, systemspec.OperationsGet.Op(), recID, "recs:read"), http.StatusNotFound)
	problemOf(t, call(d, systemspec.OperationsCancel.Op(), recID, "scripts:run"), http.StatusNotFound)
	if handled.Load() != 0 {
		t.Fatal("handler ran")
	}
}
