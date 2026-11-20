// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

// HostProcReader reads the host's /proc. The helper, which runs as root
// outside nexusd's sandbox, uses it: reading /proc/<pid>/exe of another
// root process needs ptrace-read access (Data Flow 3).
func HostProcReader() ProcReader { return procDir("/proc") }
