// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"io/fs"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The pressure stall files, by Pressure field.
var pressureFiles = []struct{ json, path string }{
	{"cpu", "proc/pressure/cpu"},
	{"memory", "proc/pressure/memory"},
	{"io", "proc/pressure/io"},
}

// memory is host.memory.get.
func (p *Provider) memory(_ context.Context, _ spec.NoInput) (apitypes.List[spec.Memory], error) {
	return currentList(p, spec.MemoryType, p.collectMemory()), nil
}

// collectMemory reads /proc/meminfo and /proc/pressure at the call, with
// procps-ng free's definitions (A-8):
//   - used = MemTotal − MemAvailable, usedPercent = used / total × 100;
//   - cached = Cached + SReclaimable, shared = Shmem;
//   - swap used = SwapTotal − SwapFree.
func (p *Provider) collectMemory() spec.MemoryProperties {
	var out spec.MemoryProperties
	un := &out.Unavailable

	mi, err := p.src.read(pathMeminfo)
	var m map[string]uint64
	fileWhy := ""
	if err != nil {
		fileWhy = reasonFor(pathMeminfo, err)
	} else if m, err = hostparse.Meminfo(mi); err != nil {
		fileWhy = reasonFor(pathMeminfo, err)
	}
	// get is one meminfo key, or the reason it is missing.
	get := func(key string) (uint64, string) {
		if fileWhy != "" {
			return 0, fileWhy
		}
		v, ok := m[key]
		if !ok {
			return 0, reasonNotReported("no " + key + " in /proc/meminfo")
		}
		return v, ""
	}
	set := func(json string, dst **uint64, v uint64, why string) {
		if why != "" {
			setWhy(un, json, why)
			return
		}
		*dst = &v
	}
	// both is a derived value of two keys: the first missing one's reason.
	both := func(a, b string) (uint64, uint64, string) {
		x, why := get(a)
		if why != "" {
			return 0, 0, why
		}
		y, why := get(b)
		return x, y, why
	}

	total, whyTotal := get("MemTotal")
	set("totalBytes", &out.TotalBytes, total, whyTotal)
	v, why := get("MemFree")
	set("freeBytes", &out.FreeBytes, v, why)
	avail, whyAvail := get("MemAvailable")
	set("availableBytes", &out.AvailableBytes, avail, whyAvail)
	v, why = get("Buffers")
	set("buffersBytes", &out.BuffersBytes, v, why)
	v, why = get("Shmem")
	set("sharedBytes", &out.SharedBytes, v, why)
	v, why = get("SwapTotal")
	set("swapTotalBytes", &out.SwapTotalBytes, v, why)
	v, why = get("SwapFree")
	set("swapFreeBytes", &out.SwapFreeBytes, v, why)

	cached, sreclaimable, why := both("Cached", "SReclaimable")
	set("cachedBytes", &out.CachedBytes, cached+sreclaimable, why)

	total, avail, why = both("MemTotal", "MemAvailable")
	if why == "" && avail > total {
		why = prefixUnrecognized + displayPath(pathMeminfo)
	}
	set("usedBytes", &out.UsedBytes, total-avail, why)
	switch {
	case why != "":
		setWhy(un, "usedPercent", why)
	case total == 0:
		setWhy(un, "usedPercent", prefixUnrecognized+displayPath(pathMeminfo))
	default:
		pct := float64(total-avail) / float64(total) * 100
		out.UsedPercent = &pct
	}

	swapTotal, swapFree, why := both("SwapTotal", "SwapFree")
	if why == "" && swapFree > swapTotal {
		why = prefixUnrecognized + displayPath(pathMeminfo)
	}
	set("swapUsedBytes", &out.SwapUsedBytes, swapTotal-swapFree, why)

	out.Pressure = p.pressure()
	return out
}

// pressure reads the three PSI files. A missing file is a kernel without
// PSI (CONFIG_PSI off, or psi=0).
func (p *Provider) pressure() spec.Pressure {
	var out spec.Pressure
	dst := map[string]**spec.PSI{"cpu": &out.CPU, "memory": &out.Memory, "io": &out.IO}
	for _, f := range pressureFiles {
		b, err := p.src.read(f.path)
		if err != nil {
			why := reasonFor(f.path, err)
			if errors.Is(err, fs.ErrNotExist) {
				why += " (kernel without PSI)"
			}
			setWhy(&out.Unavailable, f.json, why)
			continue
		}
		ps, err := hostparse.Pressure(b)
		if err != nil {
			setWhy(&out.Unavailable, f.json, reasonFor(f.path, err))
			continue
		}
		psi := &spec.PSI{Some: psiAverages(ps.Some)}
		if ps.Full != nil {
			psi.Full = psiAverages(ps.Full)
		} else {
			setWhy(&psi.Unavailable, "full", reasonNotReported("no full line in "+displayPath(f.path)))
		}
		*dst[f.json] = psi
	}
	return out
}

func psiAverages(l *hostparse.PressureLine) *spec.PSIAverages {
	a10, a60, a300 := l.Avg10, l.Avg60, l.Avg300
	return &spec.PSIAverages{Avg10: &a10, Avg60: &a60, Avg300: &a300}
}
