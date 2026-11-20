package zfs

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is what `zfs version` reports: the userland and kernel module
// versions after their "zfs-" and "zfs-kmod-" prefixes, and the userland's
// numeric major, minor and patch.
type Version struct {
	Userland, Kernel    string
	Major, Minor, Patch int
}

// Format is the output form the provider asks the tools for (D-5).
type Format int

const (
	// Text is the -H -p tab-separated form, and the text of zpool status.
	// It is the zero value, so an unknown version selects it (TC-STG-116).
	Text Format = iota
	// JSON is the -j --json-int form of OpenZFS 2.3 and later.
	JSON
)

func (f Format) String() string {
	if f == JSON {
		return "json"
	}
	return "text"
}

// Format is JSON for a userland of 2.3 or later, else Text.
func (v Version) Format() Format {
	if v.Major > 2 || v.Major == 2 && v.Minor >= 3 {
		return JSON
	}
	return Text
}

var versionNumber = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

// ParseVersion reads the output of `zfs version`: a line "zfs-<version>"
// (the userland, required) and a line "zfs-kmod-<version>" (the kernel
// module, empty when absent, as when the module is not loaded). The
// userland must start with a numeric major.minor.
func ParseVersion(out []byte) (Version, error) {
	var v Version
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if k, ok := strings.CutPrefix(line, "zfs-kmod-"); ok {
			if v.Kernel == "" {
				v.Kernel = k
			}
		} else if u, ok := strings.CutPrefix(line, "zfs-"); ok && v.Userland == "" {
			v.Userland = u
		}
	}
	if v.Userland == "" {
		return Version{}, errors.New("zfs: no zfs-<version> line in the output of zfs version")
	}
	m := versionNumber.FindStringSubmatch(v.Userland)
	if m == nil {
		return Version{}, fmt.Errorf("zfs: version %q does not start with major.minor", v.Userland)
	}
	nums := [3]int{}
	for i, s := range m[1:] {
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return Version{}, fmt.Errorf("zfs: version %q: %w", v.Userland, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}
