// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package detach

import (
	"context"
	"strings"
	"syscall"
	"testing"
)

// nginx-provider TC-ERR5: the process backend refuses on non-Linux hosts
// (FR-20, OOS-12).
func TestProcessBackendNeedsLinux(t *testing.T) {
	b := ProcessBackend(ProcessConfig{})
	ctx := context.Background()
	const want = "detached processes need Linux"
	_, stateErr := b.UnitState(ctx, Unit("x"))
	_, listErr := b.ListUnits(ctx)
	for name, err := range map[string]error{
		"Launch":    b.Launch(ctx, Invocation{ID: "x", Unit: Unit("x")}),
		"UnitState": stateErr,
		"Signal":    b.Signal(ctx, Unit("x"), syscall.SIGTERM),
		"ListUnits": listErr,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error containing %q", name, err, want)
		}
	}
}
