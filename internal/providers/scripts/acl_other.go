// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package scripts

// systemACL: only Linux keeps POSIX ACLs in the attribute this check reads,
// and Nexus targets Linux hosts. Elsewhere, as on a development machine,
// there is nothing to inspect.
func systemACL(string) ([]byte, error) { return nil, nil }
