// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"testing"
)

// The A-8 definitions on a synthetic meminfo, with every field present.
func TestMemoryDefinitions(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("proc/meminfo", []byte("MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 600 kB\n"+
		"Buffers: 50 kB\nCached: 300 kB\nSwapCached: 7 kB\nShmem: 20 kB\nSReclaimable: 40 kB\n"+
		"SwapTotal: 500 kB\nSwapFree: 380 kB\n"))
	p := s.memory()
	const k = 1024
	for name, c := range map[string]struct {
		got  *uint64
		want uint64
	}{
		"totalBytes": {p.TotalBytes, 1000 * k}, "freeBytes": {p.FreeBytes, 100 * k},
		"availableBytes": {p.AvailableBytes, 600 * k}, "usedBytes": {p.UsedBytes, 400 * k},
		"buffersBytes": {p.BuffersBytes, 50 * k}, "cachedBytes": {p.CachedBytes, 340 * k},
		"sharedBytes": {p.SharedBytes, 20 * k}, "swapTotalBytes": {p.SwapTotalBytes, 500 * k},
		"swapUsedBytes": {p.SwapUsedBytes, 120 * k}, "swapFreeBytes": {p.SwapFreeBytes, 380 * k},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %s, want %d", name, show(c.got), c.want)
		}
	}
	if p.UsedPercent == nil || *p.UsedPercent != 40 {
		t.Errorf("usedPercent = %s, want 40", show(p.UsedPercent))
	}
}

// A field the kernel does not print is null with a reason naming the key;
// cached needs both of its terms.
func TestMemoryMissingKeys(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("proc/meminfo", []byte("MemTotal: 1000 kB\nMemFree: 100 kB\nCached: 300 kB\n"))
	p := s.memory()
	for field, why := range map[string]string{
		"availableBytes": "not reported: no MemAvailable in /proc/meminfo",
		"usedBytes":      "not reported: no MemAvailable in /proc/meminfo",
		"usedPercent":    "not reported: no MemAvailable in /proc/meminfo",
		"cachedBytes":    "not reported: no SReclaimable in /proc/meminfo",
		"swapUsedBytes":  "not reported: no SwapTotal in /proc/meminfo",
	} {
		if got := p.Unavailable[field]; got != why {
			t.Errorf("%s reason %q, want %q", field, got, why)
		}
	}
	if p.TotalBytes == nil || p.FreeBytes == nil {
		t.Errorf("total %s free %s, want both", show(p.TotalBytes), show(p.FreeBytes))
	}
}

// An unreadable meminfo nulls every byte field with its reason; pressure is
// still read.
func TestMemoryMeminfoUnreadable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("proc/meminfo")
	p := s.memory()
	if p.TotalBytes != nil || p.Unavailable["totalBytes"] != "permission denied: /proc/meminfo" {
		t.Errorf("totalBytes %s with %q", show(p.TotalBytes), p.Unavailable["totalBytes"])
	}
	if p.Pressure.CPU == nil {
		t.Error("pressure.cpu is null")
	}
}

// A pressure file without a full line (cpu before Linux 5.13) reports full
// not reported; an unparsable one is unrecognized.
func TestMemoryPressureShapes(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("proc/pressure/cpu", []byte("some avg10=1.50 avg60=0.25 avg300=0.00 total=12\n"))
	s.ffs.replace("proc/pressure/io", []byte("garbage\n"))
	p := s.memory()
	c := p.Pressure.CPU
	if c == nil || c.Some == nil || *c.Some.Avg10 != 1.5 || *c.Some.Avg60 != 0.25 || *c.Some.Avg300 != 0 {
		t.Fatalf("pressure.cpu = %+v", c)
	}
	if c.Full != nil || c.Unavailable["full"] != "not reported: no full line in /proc/pressure/cpu" {
		t.Errorf("pressure.cpu.full %+v with %q", c.Full, c.Unavailable["full"])
	}
	if p.Pressure.IO != nil || p.Pressure.Unavailable["io"] != "unrecognized format: /proc/pressure/io" {
		t.Errorf("pressure.io %+v with %q", p.Pressure.IO, p.Pressure.Unavailable["io"])
	}
}
