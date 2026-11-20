// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"strings"
)

// NetDevStat is one interface of /proc/net/dev: the bytes, packets, errs and
// drop columns of Receive and Transmit.
type NetDevStat struct {
	Name                               string
	RxBytes, RxPackets, RxErrs, RxDrop uint64
	TxBytes, TxPackets, TxErrs, TxDrop uint64
}

// NetDev parses /proc/net/dev. Each interface line is "name:" and 16
// counters (net/core/net-procfs.c dev_seq_printf_stats). The name is cut at
// the first colon, since an interface name cannot hold one, and the counters
// are split on whitespace, so a large counter printed against the colon
// ("eth0:1234") reads like a spaced one. The two header lines have no colon.
// At least one interface is the required anchor.
func NetDev(b []byte) ([]NetDevStat, error) {
	var out []NetDevStat
	for l := range lines(b) {
		name, rest, ok := strings.Cut(l, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.ContainsAny(name, " \t|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		var v [8]uint64
		valid := true
		for i, col := range []int{0, 1, 2, 3, 8, 9, 10, 11} {
			if v[i], ok = parseU64(f[col]); !ok {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		out = append(out, NetDevStat{Name: name,
			RxBytes: v[0], RxPackets: v[1], RxErrs: v[2], RxDrop: v[3],
			TxBytes: v[4], TxPackets: v[5], TxErrs: v[6], TxDrop: v[7]})
	}
	if len(out) == 0 {
		return nil, errors.New("proc/net/dev: no interface line")
	}
	return out, nil
}
