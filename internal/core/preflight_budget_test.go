// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/provider"
)

// probeProvider is a provider with no operations whose preflight is
// scripted by call number.
type probeProvider struct {
	name    string
	calls   atomic.Int32
	preflit func(call int32) provider.Status
}

func (p *probeProvider) Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: p.name, ReadScope: p.name + ":read"}
}
func (*probeProvider) Configure(func(any) error) error { return nil }
func (p *probeProvider) Preflight(context.Context) provider.Status {
	return p.preflit(p.calls.Add(1))
}
func (*probeProvider) Bindings() []provider.Binding { return nil }

var (
	downStatus = provider.Status{State: apitypes.ProviderUnavailable, Reason: "down"}
	upStatus   = provider.Status{State: apitypes.ProviderEnabled}
)

type steppedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *steppedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// CR-10: a hung dependency delays startup by the startup budget, not by the
// full preflight timeout, and the provider is picked up by the call-time
// re-check once it answers.
func TestStartupPreflightIsBoundedByTheStartupBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	clk := &steppedClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	hung := &probeProvider{name: "hung", preflit: func(call int32) provider.Status {
		if call == 1 {
			<-release // A dependency that never answers; ignores the context
		}
		return upStatus
	}}
	t0 := time.Now()
	reg, err := New([]provider.Provider{hung}, Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Now: clk.Now,
		Providers:               map[string]ProviderConfig{"hung": {Enabled: true}},
		StartupPreflightTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(t0); took > time.Second {
		t.Fatalf("New took %v with a hung preflight and a 100ms startup budget", took)
	}
	if ok, st := reg.Available("hung"); ok || st.State != apitypes.ProviderUnavailable {
		t.Fatalf("hung provider after startup: %v %+v, want unavailable", ok, st)
	}
	if reg.preflightTimeout != DefaultPreflightTimeout {
		t.Fatalf("preflight timeout after New = %v, want the full %v", reg.preflightTimeout, DefaultPreflightTimeout)
	}
	clk.Advance(RecheckInterval + time.Second)
	if ok, _ := reg.availableForCall(context.Background(), "hung"); !ok {
		t.Fatal("the call-time re-check did not pick the provider up")
	}
}

// CR-10: one provider's hung re-check does not hold up another provider's.
func TestRecheckOfOneProviderDoesNotBlockAnother(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	clk := &steppedClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	hung := &probeProvider{name: "hung", preflit: func(call int32) provider.Status {
		if call == 1 {
			return downStatus
		}
		close(entered)
		<-release
		return upStatus
	}}
	fine := &probeProvider{name: "fine", preflit: func(call int32) provider.Status {
		if call == 1 {
			return downStatus
		}
		return upStatus
	}}
	reg, err := New([]provider.Provider{hung, fine}, Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Now: clk.Now,
		Providers: map[string]ProviderConfig{"hung": {Enabled: true}, "fine": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(RecheckInterval + time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.availableForCall(context.Background(), "hung")
	}()
	<-entered
	got := make(chan bool, 1)
	go func() {
		ok, _ := reg.availableForCall(context.Background(), "fine")
		got <- ok
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Error("the healthy provider was not re-checked to available")
		}
	case <-time.After(2 * time.Second):
		t.Error("the healthy provider's re-check waited on the hung provider's")
	}
	close(release)
	<-done
}

// CR-95: a re-check whose preflight finishes after a Refresh stored a newer
// result does not overwrite it.
func TestRecheckDoesNotOverwriteANewerRefresh(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	clk := &steppedClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	p := &probeProvider{name: "p", preflit: func(call int32) provider.Status {
		switch call {
		case 2: // The re-check: answers up, but only after the Refresh
			close(entered)
			<-release
			return upStatus
		default: // Startup and the Refresh
			return downStatus
		}
	}}
	reg, err := New([]provider.Provider{p}, Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Now: clk.Now,
		Providers: map[string]ProviderConfig{"p": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(RecheckInterval + time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.availableForCall(context.Background(), "p")
	}()
	<-entered
	clk.Advance(time.Second)
	reg.Refresh(context.Background())
	close(release)
	<-done
	if ok, st := reg.Available("p"); ok || st.State != apitypes.ProviderUnavailable {
		t.Fatalf("after a re-check that started before the Refresh: %v %+v, want the Refresh's unavailable", ok, st)
	}
}
