// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package nginx

import "errors"

// errRenameNeedsLinux is every rename's error where there is no
// renameat2(RENAME_NOREPLACE). nginx is managed only on Linux; this keeps
// the package building on the macOS dev host.
var errRenameNeedsLinux = errors.New("enable and disable need Linux: they rename with renameat2(RENAME_NOREPLACE)")

// RenameNoReplace is files_linux.go's rename without replacing. On this
// platform it renames nothing and fails.
func RenameNoReplace(int, string, string) error { return errRenameNeedsLinux }
