// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

// Usage is one stat-plus-statfs of a mount point (D-3, review Finding 2).
// IsDir false means the mount point is not a directory (a file bind such as
// /etc/hosts); statfs is then not called and the counts are zero. Blocks,
// Bfree and Bavail are in units of Frsize.
type Usage struct {
	IsDir                                       bool
	Blocks, Bfree, Bavail, Files, Ffree, Frsize uint64
}
