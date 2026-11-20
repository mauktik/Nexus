// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"strconv"
	"testing"
)

// The captures share one VM kernel with no hwmon, thermal or cpufreq data
// (WI 2.1 capture note e), so those sources are synthetic here, written in
// the kernel's formats: millidegrees and kHz, each value ending in "\n".

// synthSensor is one synthetic sensor.
type synthSensor struct {
	path     string // the input file
	milliC   int64
	label    string // the label or zone type; "" writes none
	labelFor string // the label file
}

// synthSensors writes two hwmon chips and one thermal zone. hwmon0
// (coretemp) has a labelled temp1 and an unlabelled temp2, hwmon1 (nvme) one
// unlabelled temp1; thermal_zone0 is x86_pkg_temp. It returns them in the
// order the CPU op lists them.
func synthSensors(f *fakeFS) []synthSensor {
	f.replace("sys/class/hwmon/hwmon0/name", []byte("coretemp\n"))
	f.replace("sys/class/hwmon/hwmon1/name", []byte("nvme\n"))
	f.replace("sys/class/thermal/thermal_zone0/type", []byte("x86_pkg_temp\n"))
	f.replace("sys/class/thermal/cooling_device0/type", []byte("Processor\n"))
	ss := []synthSensor{
		{path: "sys/class/hwmon/hwmon0/temp1_input", milliC: 45000, label: "Package id 0",
			labelFor: "sys/class/hwmon/hwmon0/temp1_label"},
		{path: "sys/class/hwmon/hwmon0/temp2_input", milliC: 47500},
		{path: "sys/class/hwmon/hwmon0/temp10_input", milliC: -1250},
		{path: "sys/class/hwmon/hwmon1/temp1_input", milliC: 38850},
		{path: "sys/class/thermal/thermal_zone0/temp", milliC: 46000},
	}
	for _, s := range ss {
		f.replace(s.path, []byte(strconv.FormatInt(s.milliC, 10)+"\n"))
		if s.labelFor != "" {
			f.replace(s.labelFor, []byte(s.label+"\n"))
		}
	}
	return ss
}

// synthCPUFreq writes scaling_cur_freq for cpu0..n-1: (1000 + 100·i) MHz
// in kHz. It returns the MHz values.
func synthCPUFreq(f *fakeFS, n int) []float64 {
	var mhz []float64
	for i := range n {
		khz := (1000 + 100*i) * 1000
		f.replace(fmt.Sprintf("sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", i), []byte(strconv.Itoa(khz)+"\n"))
		mhz = append(mhz, float64(khz)/1000)
	}
	return mhz
}

// synthCPUInfo is an x86 /proc/cpuinfo for n processors on one package with
// cores cores: model name, cpu MHz (2400.5 + i) and cpu cores.
func synthCPUInfo(n, cores int) ([]byte, []float64) {
	var b []byte
	var mhz []float64
	for i := range n {
		m := 2400.5 + float64(i)
		mhz = append(mhz, m)
		b = fmt.Appendf(b, "processor\t: %d\nvendor_id\t: GenuineIntel\nmodel name\t: Synthetic(R) CPU @ 2.40GHz\n"+
			"cpu MHz\t\t: %.3f\nphysical id\t: 0\ncpu cores\t: %d\nflags\t\t: fpu vme hypervisor\n\n", i, m, cores)
	}
	return b, mhz
}

func avgOf(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Sensors are listed hwmon first, by chip then by input number, then the
// thermal zones; a cooling device is not a zone. Labels are temp*_label,
// else the hwmon name; a zone's label and chip are its type.
func TestCPUTemperaturesFromSynthSensors(t *testing.T) {
	s := newHostStack(t, capUBU)
	ss := synthSensors(s.ffs)
	p := s.cpu()
	type want struct {
		source, chip, label string
		c                   float64
	}
	wants := []want{
		{"hwmon", "coretemp", "Package id 0", 45},
		{"hwmon", "coretemp", "coretemp", 47.5},
		{"hwmon", "coretemp", "coretemp", -1.25},
		{"hwmon", "nvme", "nvme", 38.85},
		{"thermal", "x86_pkg_temp", "x86_pkg_temp", 46},
	}
	if len(p.Temperatures) != len(ss) {
		t.Fatalf("temperatures %+v, want %d", p.Temperatures, len(ss))
	}
	for i, w := range wants {
		g := p.Temperatures[i]
		if g.Source != w.source || g.Chip != w.chip || g.Label != w.label || g.Celsius == nil || *g.Celsius != w.c {
			t.Errorf("temperatures[%d] = %+v (celsius %s), want %+v", i, g, show(g.Celsius), w)
		}
	}
	if _, ok := p.Unavailable["temperatures"]; ok {
		t.Errorf("temperatures has a reason %q with sensors present", p.Unavailable["temperatures"])
	}
	// Each input and zone temp was read through bounded: once, by Open.
	for _, sn := range ss {
		if n := s.ffs.count("open", sn.path); n != 1 {
			t.Errorf("%s opened %d times, want 1", sn.path, n)
		}
	}
}

// A sensor that fails to read stays listed with celsius null and the reason.
func TestCPUTemperatureReadFailure(t *testing.T) {
	s := newHostStack(t, capUBU)
	synthSensors(s.ffs)
	s.ffs.einval("sys/class/hwmon/hwmon1/temp1_input")
	p := s.cpu()
	var found bool
	for _, g := range p.Temperatures {
		if g.Chip == "nvme" {
			found = true
			if g.Celsius != nil || g.Unavailable["celsius"] != "not present: /sys/class/hwmon/hwmon1/temp1_input" {
				t.Errorf("nvme celsius %s with %q", show(g.Celsius), g.Unavailable["celsius"])
			}
		}
	}
	if !found {
		t.Errorf("nvme sensor missing from %+v", p.Temperatures)
	}
}

// Physical cores fall back to cpuinfo's cpu cores when the topology is
// absent, and are null with the topology's reason when both are.
func TestCPUPhysicalCoresFallback(t *testing.T) {
	s := newHostStack(t, capUBU)
	for i := range 4 {
		s.ffs.remove(fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology", i))
	}
	info, _ := synthCPUInfo(4, 2)
	s.ffs.replace("proc/cpuinfo", info)
	p := s.cpu()
	if p.PhysicalCores == nil || *p.PhysicalCores != 2 {
		t.Errorf("physicalCores %s, want cpuinfo's cpu cores 2", show(p.PhysicalCores))
	}

	s = newHostStack(t, capUBU)
	for i := range 4 {
		s.ffs.remove(fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology", i))
	}
	p = s.cpu()
	if p.PhysicalCores != nil ||
		p.Unavailable["physicalCores"] != "not present: /sys/devices/system/cpu/cpu0/topology/physical_package_id" {
		t.Errorf("physicalCores %s with %q", show(p.PhysicalCores), p.Unavailable["physicalCores"])
	}
}

// Two hyperthreads on one core count once; two packages with the same
// core id count twice.
func TestCPUPhysicalCoresDistinctPairs(t *testing.T) {
	s := newHostStack(t, capUBU)
	for i, pc := range [][2]int{{0, 0}, {0, 0}, {1, 0}, {1, 1}} {
		dir := fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/", i)
		s.ffs.replace(dir+"physical_package_id", []byte(strconv.Itoa(pc[0])+"\n"))
		s.ffs.replace(dir+"core_id", []byte(strconv.Itoa(pc[1])+"\n"))
	}
	if p := s.cpu(); p.PhysicalCores == nil || *p.PhysicalCores != 3 {
		t.Errorf("physicalCores %s, want 3", show(p.PhysicalCores))
	}
}

// Without cpufreq or cpuinfo MHz (the aarch64 captures) every frequency is
// null with a reason, and so is the average.
func TestCPUFrequencyNotReported(t *testing.T) {
	s := newHostStack(t, capUBU)
	p := s.cpu()
	const why = "not reported: no cpufreq and no cpu MHz in /proc/cpuinfo"
	for _, c := range p.PerCPU {
		if c.FrequencyMHz != nil || c.Unavailable["frequencyMhz"] != why {
			t.Errorf("cpu%d frequency %s with %q, want null with %q", c.CPU, show(c.FrequencyMHz),
				c.Unavailable["frequencyMhz"], why)
		}
	}
	if p.FrequencyMHz != nil || p.Unavailable["frequencyMhz"] != why {
		t.Errorf("frequencyMhz %s with %q, want null with %q", show(p.FrequencyMHz), p.Unavailable["frequencyMhz"], why)
	}
	if p.Model != nil || p.Unavailable["model"] != "not reported: no model name in /proc/cpuinfo" {
		t.Errorf("model %s with %q", show(p.Model), p.Unavailable["model"])
	}
}

// A denied cpufreq file with no cpuinfo MHz gives that file's reason.
func TestCPUFrequencyDenied(t *testing.T) {
	s := newHostStack(t, capUBU)
	synthCPUFreq(s.ffs, 4)
	s.ffs.deny("sys/devices/system/cpu/cpu2/cpufreq/scaling_cur_freq")
	p := s.cpu()
	c := p.PerCPU[2]
	if c.FrequencyMHz != nil ||
		c.Unavailable["frequencyMhz"] != "permission denied: /sys/devices/system/cpu/cpu2/cpufreq/scaling_cur_freq" {
		t.Errorf("cpu2 frequency %s with %q", show(c.FrequencyMHz), c.Unavailable["frequencyMhz"])
	}
	if p.FrequencyMHz == nil || *p.FrequencyMHz != avgOf([]float64{1000, 1100, 1300}) {
		t.Errorf("frequencyMhz %s, want the mean of the three readable CPUs", show(p.FrequencyMHz))
	}
}

// An unreadable /proc/stat leaves logical CPUs null, per-CPU [] with the
// reason, and the fields from other files intact.
func TestCPUStatUnreadable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("proc/stat")
	p := s.cpu()
	if p.LogicalCPUs != nil || p.Unavailable["logicalCpus"] != "permission denied: /proc/stat" {
		t.Errorf("logicalCpus %s with %q", show(p.LogicalCPUs), p.Unavailable["logicalCpus"])
	}
	if len(p.PerCPU) != 0 || p.Unavailable["perCpu"] != "permission denied: /proc/stat" {
		t.Errorf("perCpu %+v with %q", p.PerCPU, p.Unavailable["perCpu"])
	}
	if p.Load.Load1 == nil {
		t.Error("load1 is null")
	}
}
