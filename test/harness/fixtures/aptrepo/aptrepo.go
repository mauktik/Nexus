// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Package aptrepo carries build.sh into the T1 test binary, so the apt
// tests build their fixture repository from the same script the parser
// captures use (internal/providers/apt/testdata/capture.sh), whatever
// run.sh copies into the container.
package aptrepo

import _ "embed"

// BuildScript is build.sh.
//
//go:embed build.sh
var BuildScript []byte
