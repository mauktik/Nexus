// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"slices"
	"strings"
)

// Virtualization kinds.
const (
	VirtKindVM        = "vm"
	VirtKindContainer = "container"
	VirtKindNone      = "none"
)

// VirtFacts are the virtualization markers the host provider gathers from
// files (A-33; systemd-detect-virt is never run). A string is the file's
// content, "" when absent or unreadable.
type VirtFacts struct {
	DockerEnv        bool    // /.dockerenv exists
	ContainerEnv     bool    // /run/.containerenv exists
	SystemdContainer string  // /run/systemd/container
	Mounts           []Mount // /proc/self/mountinfo
	ProcVZ, ProcBC   bool    // /proc/vz and /proc/bc exist
	OSRelease        string  // /proc/sys/kernel/osrelease
	HypervisorType   string  // /sys/hypervisor/type
	SysVendor        string  // /sys/class/dmi/id/sys_vendor
	ProductName      string  // /sys/class/dmi/id/product_name
	HypervisorFlag   bool    // the cpuinfo flags hold "hypervisor"
}

// Virtualization is Classify's answer. Type is a systemd-detect-virt word,
// "none" or "unknown"; Kind is vm, container or none; Evidence names the
// marker used.
type Virtualization struct {
	Type, Kind, Evidence string
}

// containerNames are the values of /run/systemd/container that systemd
// knows (src/basic/virt.c container_table); another value is
// container-other, as systemd-detect-virt reports it.
var containerNames = []string{"lxc", "lxc-libvirt", "systemd-nspawn", "docker", "podman", "rkt", "wsl", "proot", "pouch"}

// dmiTable maps a DMI value prefix to its type, as systemd's
// dmi_vendor_table does (src/basic/virt.c, systemd commit 7386493f), restricted
// to the rows A-33 lists. The Google and Oracle rows are systemd's exact
// strings ("Google Compute Engine"; "innotek GmbH", "VirtualBox"), not a
// bare vendor prefix, so a Chromebook (sys_vendor Google) is not a VM.
// systemd's "Oracle Corporation" row, meant for VirtualBox's board_vendor,
// is left out: board_vendor is not read here, a VirtualBox guest already
// matches on product_name VirtualBox, and the row would call an Oracle
// bare-metal server (sys_vendor Oracle Corporation) a VM. Microsoft is
// handled apart, since a Surface laptop has the same sys_vendor as a
// Hyper-V guest.
var dmiTable = []struct{ prefix, typ string }{
	{"KVM", "kvm"},
	{"OpenStack", "kvm"},
	{"QEMU", "qemu"},
	{"VMware", "vmware"},
	{"innotek GmbH", "oracle"},
	{"VirtualBox", "oracle"},
	{"Xen", "xen"},
	{"Amazon EC2", "amazon"},
	{"Google Compute Engine", "google"},
	{"Parallels", "parallels"},
	{"BHYVE", "bhyve"},
	{"Apple Virtualization", "apple"},
}

// Classify turns markers into a virtualization answer. Containers are
// checked first, since a container on a VM sees the VM's DMI: /.dockerenv,
// /run/.containerenv, /run/systemd/container, fuse.lxcfs mounted over a
// /proc file, /proc/vz without /proc/bc (openvz) and an osrelease containing
// "microsoft" (wsl). Then VMs: /sys/hypervisor/type "xen", then DMI
// product_name and sys_vendor against dmiTable (product_name first, so a KVM
// guest whose sys_vendor is QEMU is kvm, as in systemd), then the cpuinfo
// hypervisor flag (unknown). With no marker the answer is none.
func Classify(f VirtFacts) Virtualization {
	container := func(typ, ev string) Virtualization {
		return Virtualization{Type: typ, Kind: VirtKindContainer, Evidence: ev}
	}
	vm := func(typ, ev string) Virtualization {
		return Virtualization{Type: typ, Kind: VirtKindVM, Evidence: ev}
	}
	if f.DockerEnv {
		return container("docker", "/.dockerenv")
	}
	if f.ContainerEnv {
		return container("podman", "/run/.containerenv")
	}
	if c := strings.TrimSpace(f.SystemdContainer); c != "" {
		typ := "container-other"
		if slices.Contains(containerNames, c) {
			typ = c
		}
		return container(typ, "/run/systemd/container: "+evidenceValue(c))
	}
	for _, m := range f.Mounts {
		if m.FSType == "fuse.lxcfs" && strings.HasPrefix(m.MountPoint, "/proc/") {
			return container("lxc", "fuse.lxcfs on "+evidenceValue(m.MountPoint))
		}
	}
	if f.ProcVZ && !f.ProcBC {
		return container("openvz", "/proc/vz without /proc/bc")
	}
	if strings.Contains(strings.ToLower(f.OSRelease), "microsoft") {
		return container("wsl", "/proc/sys/kernel/osrelease")
	}

	if strings.TrimSpace(f.HypervisorType) == "xen" {
		return vm("xen", "/sys/hypervisor/type")
	}
	product, vendor := strings.TrimSpace(f.ProductName), strings.TrimSpace(f.SysVendor)
	if strings.HasPrefix(vendor, "Microsoft Corporation") && strings.HasPrefix(product, "Virtual Machine") {
		return vm("microsoft", "/sys/class/dmi/id/sys_vendor: "+evidenceValue(vendor)+", product_name: "+evidenceValue(product))
	}
	for _, d := range []struct{ value, path string }{
		{product, "/sys/class/dmi/id/product_name"},
		{vendor, "/sys/class/dmi/id/sys_vendor"},
	} {
		for _, row := range dmiTable {
			if d.value != "" && strings.HasPrefix(d.value, row.prefix) {
				return vm(row.typ, d.path+": "+evidenceValue(d.value))
			}
		}
	}
	if f.HypervisorFlag {
		return vm("unknown", "cpuinfo hypervisor flag")
	}
	return Virtualization{Type: "none", Kind: VirtKindNone, Evidence: "no virtualization marker"}
}

// evidenceValue is a marker's value as quoted in Evidence: valid UTF-8, at
// most 64 bytes.
func evidenceValue(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}
