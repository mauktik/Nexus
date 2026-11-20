// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package svcctl

// openNoLinks opens the absolute, clean path for reading without blocking and
// without following a symbolic link in any component (CWE-59), by the
// component walk: only Linux has openat2(2).
func openNoLinks(path string) (int, error) { return walkNoLinks(path) }
