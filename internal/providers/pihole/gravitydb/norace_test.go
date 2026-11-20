// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package gravitydb

// raceEnabled is false when the tests are built without the race detector.
const raceEnabled = false
