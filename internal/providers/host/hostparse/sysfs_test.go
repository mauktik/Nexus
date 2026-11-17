package hostparse

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Synthetic sysfs scalars. Both captures share one VM kernel without hwmon,
// thermal zones or cpufreq (testdata/README.md), so those shapes come from
// the kernel ABI documents:
//   - hwmon tempY_input: "Unit: millidegree Celsius"
//     (Documentation/ABI/testing/sysfs-class-hwmon);
//   - thermal_zoneX/temp: "Unit: millidegree Celsius"
//     (Documentation/ABI/testing/sysfs-class-thermal);
//   - scaling_cur_freq: "Current frequency of all of the CPUs belonging to
//     this policy (in kHz)" (Documentation/admin-guide/pm/cpufreq.rst).

func TestMillidegrees(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"45000\n", 45},    // hwmon temp1_input
		{"52500\n", 52.5},  // thermal_zone0/temp
		{"-5250\n", -5.25}, // a sensor below zero
		{"0\n", 0},         // a zone that reads 0
		{"  38125 \n", 38.125},
	} {
		got, err := Millidegrees([]byte(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("Millidegrees(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "\n", "45.0\n", "45 C\n", "abc", "99999999999999999999\n"} {
		if got, err := Millidegrees([]byte(in)); err == nil {
			t.Errorf("Millidegrees(%q) = %v, want an error", in, got)
		}
	}
}

func TestScalarInt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1500\n", 1500},       // net mtu
		{"2\n", 2},             // net ifindex
		{"772\n", 772},         // net type (loopback)
		{"1800000\n", 1800000}, // cpufreq scaling_cur_freq, kHz
		{"0\n", 0},             // topology core_id
		{"-1\n", -1},
	} {
		got, err := ScalarInt([]byte(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("ScalarInt(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "253:0\n", "0x10", "1e3", "9223372036854775808"} {
		if got, err := ScalarInt([]byte(in)); err == nil {
			t.Errorf("ScalarInt(%q) = %v, want an error", in, got)
		}
	}
	// Every captured integer attribute parses.
	for _, c := range captures {
		for _, g := range []string{"fs/sys/class/net/*/mtu", "fs/sys/class/net/*/ifindex", "fs/sys/class/net/*/type",
			"fs/sys/devices/system/cpu/cpu*/topology/*"} {
			files := globCap(t, c, g)
			if len(files) == 0 {
				t.Fatalf("%s: no %s", c, g)
			}
			for _, p := range files {
				want, err := strconv.ParseInt(strings.TrimSpace(string(readFile(t, p))), 10, 64)
				if err != nil {
					t.Fatalf("test oracle: %s: %v", p, err)
				}
				if got, err := ScalarInt(readFile(t, p)); err != nil || got != want {
					t.Errorf("%s: ScalarInt = %d, %v; want %d", p, got, err, want)
				}
			}
		}
	}
}

func TestScalarString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"coretemp\n", "coretemp"},         // hwmon name
		{"Package id 0\n", "Package id 0"}, // hwmon temp1_label
		{"x86_pkg_temp\n", "x86_pkg_temp"}, // thermal zone type
		{"\n", ""},
		{"a\xffb\n", "a�b"},
	} {
		if got := ScalarString([]byte(tc.in)); got != tc.want {
			t.Errorf("ScalarString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, c := range captures {
		p := capPath(c, "fs/sys/class/dmi/id/product_name")
		want := strings.TrimSuffix(string(readFile(t, p)), "\n")
		if got := ScalarString(readFile(t, p)); got != want || !utf8.ValidString(got) {
			t.Errorf("%s: ScalarString = %q, want %q", p, got, want)
		}
	}
}

func TestBlockScalars(t *testing.T) {
	if got, err := BlockSize([]byte("41943040\n")); err != nil || got != 41943040*512 {
		t.Errorf("BlockSize = %d, %v", got, err)
	}
	if got, err := BlockSize([]byte("0\n")); err != nil || got != 0 {
		t.Errorf("BlockSize(0) = %d, %v", got, err)
	}
	for _, in := range []string{"", "-1", "abc", strconv.FormatUint(math.MaxUint64/512+1, 10), "18446744073709551616"} {
		if got, err := BlockSize([]byte(in)); err == nil {
			t.Errorf("BlockSize(%q) = %d, want an error", in, got)
		}
	}
	for _, f := range []struct {
		name string
		fn   func([]byte) (bool, error)
	}{{"Rotational", Rotational}, {"Removable", Removable}} {
		if v, err := f.fn([]byte("1\n")); err != nil || !v {
			t.Errorf("%s(1) = %v, %v", f.name, v, err)
		}
		if v, err := f.fn([]byte("0\n")); err != nil || v {
			t.Errorf("%s(0) = %v, %v", f.name, v, err)
		}
		for _, in := range []string{"", "2", "true", "01", "-1"} {
			if v, err := f.fn([]byte(in)); err == nil {
				t.Errorf("%s(%q) = %v, want an error", f.name, in, v)
			}
		}
	}
}
