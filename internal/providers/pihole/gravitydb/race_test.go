// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build race

package gravitydb

// raceEnabled is true when the tests are built with the race detector, whose
// instrumentation makes wall-clock budgets meaningless.
const raceEnabled = true
