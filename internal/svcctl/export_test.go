// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package svcctl

import (
	"context"
	"time"
)

// RealCall runs argv through the real runner, bounded by timeout, as every
// Controller call is.
func RealCall(ctx context.Context, timeout time.Duration, argv []string) (Result, error) {
	return base{run: Run, timeout: timeout}.call(ctx, argv)
}
