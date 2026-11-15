package host

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/providers/host/hostparse"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// The CPU detail's sysfs sources (FR-6).
const (
	cpuDir     = "sys/devices/system/cpu"
	hwmonDir   = "sys/class/hwmon"
	thermalDir = "sys/class/thermal"
)

// Temperature sources (spec.Temperature.Source).
const (
	tempSourceHwmon   = "hwmon"
	tempSourceThermal = "thermal"
)

var (
	reasonNoSensors   = reasonNotReported("no temperature sensors (common in VMs)")
	reasonNoFrequency = reasonNotReported("no cpufreq and no cpu MHz in /proc/cpuinfo")
	reasonNoModel     = reasonNotReported("no model name in /proc/cpuinfo")
)

// cpu is host.cpu.get.
func (p *Provider) cpu(ctx context.Context, _ spec.NoInput) (apitypes.List[spec.CPU], error) {
	return currentList(p, spec.CPUType, p.collectCPU(ctx, p.run.Load().snapshot())), nil
}

// collectCPU reads the CPU detail live and merges in the sampler-derived
// usage from snap (nil when the sampler is not running). The model, counts,
// load, frequencies and temperatures never depend on the sampler (FR-11).
func (p *Provider) collectCPU(ctx context.Context, snap *snapshot) spec.CPUProperties {
	now := p.now()
	out := spec.CPUProperties{Sampler: snap.sampler(now)}
	un := &out.Unavailable

	var info *hostparse.CPUInfo
	var infoRaw []byte
	infoWhy := ""
	if b, err := p.src.read(pathCPUInfo); err != nil {
		infoWhy = reasonFor(pathCPUInfo, err)
	} else if ci, err := hostparse.Cpuinfo(b); err != nil {
		infoWhy = reasonFor(pathCPUInfo, err)
	} else {
		info, infoRaw = &ci, b
	}
	switch {
	case info == nil:
		setWhy(un, "model", infoWhy)
	case info.Model == "":
		setWhy(un, "model", reasonNoModel)
	default:
		m := info.Model
		out.Model = &m
	}

	// The logical CPUs are the cpuN lines of /proc/stat, read now.
	var cpus []hostparse.PerCPU
	statWhy := ""
	if b, err := p.src.read(pathStat); err != nil {
		statWhy = reasonFor(pathStat, err)
	} else if st, err := hostparse.ProcStat(b); err != nil {
		statWhy = reasonFor(pathStat, err)
	} else {
		cpus = st.CPUs
	}
	if statWhy != "" {
		setWhy(un, "logicalCpus", statWhy)
		setWhy(un, "perCpu", statWhy)
	} else {
		n := len(cpus)
		out.LogicalCPUs = &n
	}

	if n, why := p.physicalCores(cpus, statWhy, infoRaw); why != "" {
		setWhy(un, "physicalCores", why)
	} else {
		out.PhysicalCores = &n
	}

	out.Load = p.load()

	// The usage and its split come from the sampler's snapshot; val gives
	// the not-running reason for a nil one.
	var rates cpuRates
	if snap != nil {
		rates = snap.cpu
	}
	usage, why := snap.val(now, rates.usage)
	out.UsagePercent = usage
	if why != "" {
		setWhy(un, "usagePercent", why)
	}
	for _, f := range []struct {
		json string
		v    rateVal
		dst  **float64
	}{
		{"userPercent", rates.user, &out.Times.UserPercent},
		{"systemPercent", rates.system, &out.Times.SystemPercent},
		{"iowaitPercent", rates.iowait, &out.Times.IowaitPercent},
		{"stealPercent", rates.steal, &out.Times.StealPercent},
	} {
		v, why := snap.val(now, f.v)
		*f.dst = v
		if why != "" {
			setWhy(&out.Times.Unavailable, f.json, why)
		}
	}

	// The blocking sources (scaling_cur_freq and the sensors) run at once,
	// so the op takes at most about one sourceTimeout however many of them
	// are stuck (D-7).
	var freqs []freqRead
	var temps spec.Array[spec.Temperature]
	tempsWhy := ""
	var wg sync.WaitGroup
	wg.Go(func() { freqs = p.frequencies(ctx, cpus, info) })
	wg.Go(func() { temps, tempsWhy = p.temperatures(ctx) })
	wg.Wait()

	out.PerCPU = make(spec.Array[spec.PerCPU], 0, len(cpus))
	var sum float64
	var have int
	firstWhy := statWhy
	for i, c := range cpus {
		pc := spec.PerCPU{CPU: c.ID}
		u, why := perCPUUsage(snap, now, c.ID)
		pc.UsagePercent = u
		if why != "" {
			setWhy(&pc.Unavailable, "usagePercent", why)
		}
		if f := freqs[i]; f.why != "" {
			setWhy(&pc.Unavailable, "frequencyMhz", f.why)
			if firstWhy == "" {
				firstWhy = f.why
			}
		} else {
			mhz := f.mhz
			pc.FrequencyMHz = &mhz
			sum += mhz
			have++
		}
		out.PerCPU = append(out.PerCPU, pc)
	}
	if have > 0 {
		avg := sum / float64(have)
		out.FrequencyMHz = &avg
	} else {
		if firstWhy == "" {
			firstWhy = reasonNoFrequency
		}
		setWhy(un, "frequencyMhz", firstWhy)
	}

	out.Temperatures = temps
	if tempsWhy != "" {
		setWhy(un, "temperatures", tempsWhy)
	}
	return out
}

// perCPUUsage is one CPU's usage from the snapshot. A CPU the snapshot does
// not know yet (onlined since) is warming up.
func perCPUUsage(snap *snapshot, now time.Time, id int) (*float64, string) {
	if why := snap.blocked(now); why != "" {
		return nil, why
	}
	if snap.perCPU == nil {
		if snap.perCPUWhy != "" {
			return nil, snap.perCPUWhy
		}
		return nil, reasonWarmingUp
	}
	r, ok := snap.perCPU[id]
	if !ok {
		return nil, reasonWarmingUp
	}
	return snap.val(now, r.usage)
}

// physicalCores counts the distinct (physical_package_id, core_id) pairs
// of the CPUs, falling back to cpuinfo's "cpu cores" when the topology
// cannot be read. Without either the reason is the topology's.
func (p *Provider) physicalCores(cpus []hostparse.PerCPU, statWhy string, cpuinfo []byte) (int, string) {
	why := statWhy
	if why == "" {
		pairs := make(map[[2]int64]bool, len(cpus))
		for _, c := range cpus {
			pkg, w := p.topology(c.ID, "physical_package_id")
			if w == "" {
				var core int64
				core, w = p.topology(c.ID, "core_id")
				pairs[[2]int64{pkg, core}] = true
			}
			if w != "" {
				why = w
				break
			}
		}
		if why == "" && len(pairs) > 0 {
			return len(pairs), ""
		}
	}
	if n, ok := cpuinfoCores(cpuinfo); ok {
		return n, ""
	}
	if why == "" {
		why = reasonNotReported("no CPU topology")
	}
	return 0, why
}

// topology reads one integer of a CPU's topology directory.
func (p *Provider) topology(id int, attr string) (int64, string) {
	name, err := segPath(segName, cpuDir, "cpu"+strconv.Itoa(id), "topology", attr)
	if err != nil {
		return 0, reasonFor(cpuDir, err)
	}
	b, err := p.src.read(name)
	if err != nil {
		return 0, reasonFor(name, err)
	}
	v, err := hostparse.ScalarInt(b)
	if err != nil {
		return 0, reasonFor(name, err)
	}
	return v, ""
}

// cpuinfoCores is the first "cpu cores" value of /proc/cpuinfo: the cores
// of one physical package (x86; other architectures print none).
// hostparse.Cpuinfo does not keep it, so it is read here.
func cpuinfoCores(b []byte) (int, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(k) != "cpu cores" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil && n > 0
	}
	return 0, false
}

// load is /proc/loadavg, read now.
func (p *Provider) load() spec.Load {
	var out spec.Load
	why := ""
	if b, err := p.src.read(pathLoadavg); err != nil {
		why = reasonFor(pathLoadavg, err)
	} else if l, err := hostparse.Loadavg(b); err != nil {
		why = reasonFor(pathLoadavg, err)
	} else {
		out.Load1, out.Load5, out.Load15 = &l.Load1, &l.Load5, &l.Load15
	}
	if why != "" {
		for _, k := range []string{"load1", "load5", "load15"} {
			setWhy(&out.Unavailable, k, why)
		}
	}
	return out
}

// freqRead is one CPU's frequency, or the reason there is none.
type freqRead struct {
	mhz float64
	why string
}

// frequencies reads each CPU's scaling_cur_freq (kHz) under bounded, all at
// once, falling back to the cpuinfo MHz of the same position. cpufreq
// absent and no cpuinfo MHz is not reported; any other cpufreq failure
// without a fallback is that failure's reason.
func (p *Provider) frequencies(ctx context.Context, cpus []hostparse.PerCPU, info *hostparse.CPUInfo) []freqRead {
	out := make([]freqRead, len(cpus))
	var wg sync.WaitGroup
	for i, c := range cpus {
		wg.Go(func() {
			name, err := segPath(segName, cpuDir, "cpu"+strconv.Itoa(c.ID), "cpufreq", "scaling_cur_freq")
			if err == nil {
				var b []byte
				if b, err = p.src.readBounded(ctx, name); err == nil {
					var khz int64
					if khz, err = hostparse.ScalarInt(b); err == nil && khz <= 0 {
						err = errors.New("scaling_cur_freq: not a positive frequency")
					}
					if err == nil {
						out[i] = freqRead{mhz: float64(khz) / 1000}
						return
					}
				}
			}
			if info != nil && i < len(info.MHz) {
				out[i] = freqRead{mhz: info.MHz[i]}
				return
			}
			if errors.Is(err, fs.ErrNotExist) {
				out[i] = freqRead{why: reasonNoFrequency}
				return
			}
			out[i] = freqRead{why: reasonFor(name, err)}
		})
	}
	wg.Wait()
	return out
}

// sensor is one temperature input found by listing.
type sensor struct {
	t     spec.Temperature
	input string // read under bounded
}

// temperatures lists every hwmon temp*_input and every thermal zone, and
// reads their values under bounded, all at once (D-7). A sensor that fails
// to read stays listed with celsius null and the reason. With no sensor at
// all the list is [] with not reported, or the listing's reason when a
// listing failed for any reason but absence.
func (p *Provider) temperatures(ctx context.Context) (spec.Array[spec.Temperature], string) {
	hw, hwErr := p.hwmonSensors()
	th, thErr := p.thermalSensors()
	sensors := append(hw, th...)
	if len(sensors) == 0 {
		for _, l := range []struct {
			dir string
			err error
		}{{hwmonDir, hwErr}, {thermalDir, thErr}} {
			if l.err != nil && !errors.Is(l.err, fs.ErrNotExist) {
				return spec.Array[spec.Temperature]{}, reasonFor(l.dir, l.err)
			}
		}
		return spec.Array[spec.Temperature]{}, reasonNoSensors
	}

	out := make(spec.Array[spec.Temperature], len(sensors))
	var wg sync.WaitGroup
	for i, s := range sensors {
		wg.Go(func() {
			t := s.t
			b, err := p.src.readBounded(ctx, s.input)
			var c float64
			if err == nil {
				c, err = hostparse.Millidegrees(b)
			}
			if err != nil {
				setWhy(&t.Unavailable, "celsius", reasonFor(s.input, err))
			} else {
				t.Celsius = &c
			}
			out[i] = t
		})
	}
	wg.Wait()
	return out, ""
}

// hwmonSensors lists sys/class/hwmon/<h>/temp<N>_input, by chip then by N.
// The chip is the hwmon name (else <h>); the label is temp<N>_label, else
// the chip.
func (p *Provider) hwmonSensors() ([]sensor, error) {
	chips, err := p.src.readDir(hwmonDir)
	if err != nil {
		return nil, err
	}
	var out []sensor
	for _, c := range chips {
		dir, err := segPath(segName, hwmonDir, c.Name())
		if err != nil {
			continue
		}
		entries, err := p.src.readDir(dir)
		if err != nil {
			continue
		}
		chip := c.Name()
		if v, why := p.readString(dir + "/name"); why == "" && v != "" {
			chip = v
		}
		var inputs []int
		for _, e := range entries {
			if n, ok := tempInputNumber(e.Name()); ok {
				inputs = append(inputs, n)
			}
		}
		slices.Sort(inputs)
		for _, n := range inputs {
			num := strconv.Itoa(n)
			label := chip
			if v, why := p.readString(dir + "/temp" + num + "_label"); why == "" && v != "" {
				label = v
			}
			out = append(out, sensor{
				t:     spec.Temperature{Source: tempSourceHwmon, Chip: chip, Label: label},
				input: dir + "/temp" + num + "_input",
			})
		}
	}
	return out, nil
}

// tempInputNumber is N of "temp<N>_input".
func tempInputNumber(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, "temp")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, "_input")
	if !ok || !allDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// thermalSensors lists sys/class/thermal/thermal_zone<N>, by N. The chip
// and label are the zone's type, else its directory name.
func (p *Provider) thermalSensors() ([]sensor, error) {
	entries, err := p.src.readDir(thermalDir)
	if err != nil {
		return nil, err
	}
	type zone struct {
		n    int
		name string
	}
	var zones []zone
	for _, e := range entries {
		num, ok := strings.CutPrefix(e.Name(), "thermal_zone")
		if !ok || !allDigits(num) {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		zones = append(zones, zone{n, e.Name()})
	}
	slices.SortFunc(zones, func(a, b zone) int { return cmp.Compare(a.n, b.n) })
	var out []sensor
	for _, z := range zones {
		dir, err := segPath(segName, thermalDir, z.name)
		if err != nil {
			continue
		}
		typ := z.name
		if v, why := p.readString(dir + "/type"); why == "" && v != "" {
			typ = v
		}
		out = append(out, sensor{
			t:     spec.Temperature{Source: tempSourceThermal, Chip: typ, Label: typ},
			input: dir + "/temp",
		})
	}
	return out, nil
}
