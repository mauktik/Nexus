// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// sysfsValue is the content of a one-value sysfs attribute: the kernel ends
// it with "\n" (sysfs_emit), which is dropped with any other surrounding
// space.
func sysfsValue(b []byte) string {
	return strings.TrimSpace(string(b))
}

// ScalarInt parses a decimal integer attribute: mtu, ifindex, type,
// topology core_id and physical_package_id, cpufreq scaling_cur_freq (kHz,
// Documentation/admin-guide/pm/cpufreq.rst).
func ScalarInt(b []byte) (int64, error) {
	v, err := strconv.ParseInt(sysfsValue(b), 10, 64)
	if err != nil {
		return 0, errors.New("sysfs: not a decimal integer")
	}
	return v, nil
}

// ScalarString parses a text attribute (hwmon name and temp*_label, thermal
// zone type, DMI fields, operstate): the value without surrounding space,
// with any invalid UTF-8 replaced.
func ScalarString(b []byte) string {
	return strings.ToValidUTF8(sysfsValue(b), "�")
}

// Millidegrees parses a temperature in millidegree Celsius, the unit of
// hwmon tempY_input (Documentation/ABI/testing/sysfs-class-hwmon) and
// thermal_zoneX/temp (Documentation/ABI/testing/sysfs-class-thermal), and
// returns degrees Celsius. Values can be negative.
func Millidegrees(b []byte) (float64, error) {
	v, err := ScalarInt(b)
	if err != nil {
		return 0, err
	}
	return float64(v) / 1000, nil
}

// Operstate maps /sys/class/net/<iface>/operstate (RFC 2863 states,
// Documentation/networking/operstates.rst) to up, down, unknown or dormant
// (A-25): notpresent and lowerlayerdown are down; testing and anything else
// is unknown.
func Operstate(b []byte) string {
	switch s := sysfsValue(b); s {
	case "up", "down", "unknown", "dormant":
		return s
	case "notpresent", "lowerlayerdown":
		return "down"
	default:
		return "unknown"
	}
}

// LinkSpeed parses /sys/class/net/<iface>/speed in Mbit/s. The kernel prints
// the ethtool speed with "%d", so SPEED_UNKNOWN is -1 (net/core/net-sysfs.c
// speed_show); the read itself fails with EINVAL for a link without
// get_link_ksettings. ok is false, "not reported", for -1, any value ≤ 0 or
// anything that is not an integer.
func LinkSpeed(b []byte) (mbps int64, ok bool) {
	v, err := ScalarInt(b)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// BlockSize parses /sys/block/<disk>/size (and a partition's size), which
// the kernel prints in 512-byte sectors whatever the logical block size
// (block/genhd.c part_size_show prints bdev_nr_sectors; SECTOR_SHIFT is 9 in
// include/linux/blk_types.h), and returns bytes.
func BlockSize(b []byte) (uint64, error) {
	v, err := strconv.ParseUint(sysfsValue(b), 10, 64)
	if err != nil {
		return 0, errors.New("sysfs: block size is not a sector count")
	}
	if v > math.MaxUint64/SectorSize {
		return 0, errors.New("sysfs: block size overflows")
	}
	return v * SectorSize, nil
}

// Rotational parses /sys/block/<disk>/queue/rotational: "1" is rotational,
// "0" is not.
func Rotational(b []byte) (bool, error) {
	return flag01(b, "rotational")
}

// Removable parses /sys/block/<disk>/removable, which the kernel prints as
// "%d\n" of 0 or 1 (block/genhd.c disk_removable_show).
func Removable(b []byte) (bool, error) {
	return flag01(b, "removable")
}

func flag01(b []byte, name string) (bool, error) {
	switch sysfsValue(b) {
	case "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, errors.New("sysfs: " + name + " is not 0 or 1")
}
