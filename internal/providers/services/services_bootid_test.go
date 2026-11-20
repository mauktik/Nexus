package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl/svcctltest"
)

// The boot IDs of the debian:12 T1 run (systemd 252.39-1~deb12u2, services-
// provider WI 5.3) where nexusd read the host kernel's boot ID: the
// kernel's, and the container's, bind-mounted from /run/nx-boot-id.
const (
	deb12HostBootID      = "a2e5b43d-6ecf-4b62-bbde-f26dfa9c7fcf"
	deb12ContainerBootID = "8f2fa550-e959-47f0-9914-7f25aa8d7365"
)

// deb12Layout plants, in a Docker container's F-ROOTS, the mount tables
// captured in that run under deploy/nexusd.service's hardening: PID 1's
// /proc/1/mountinfo has the bind mount of /run/nx-boot-id (device 0:60, the
// container's /run tmpfs) over /proc/sys/kernel/random/boot_id; nexusd's
// /proc/self/mountinfo has /proc/sys remounted read-only by
// ProtectKernelTunables=yes without it, and the same /run. nexusd's own
// boot_id is the host kernel's. Device 0:60 is the F-ROOTS run directory's,
// so the source resolves to RunRoot/nx-boot-id.
func deb12Layout(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".dockerenv"), "")
	writeFile(t, filepath.Join(root, "proc", procBootID), deb12HostBootID+"\n")
	writeFile(t, filepath.Join(root, "run", "nx-boot-id"), deb12ContainerBootID+"\n")
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(root, "run"), &st); err != nil {
		t.Fatal(err)
	}
	d := uint64(st.Dev) // int32 on darwin, uint64 on Linux
	dev := fmt.Sprintf("%d:%d", unix.Major(d), unix.Minor(d))
	captured, err := os.ReadFile(filepath.Join(root, "proc", "self", "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(captured), "boot_id") {
		t.Fatal("the captured mountinfo already has a boot_id mount")
	}
	sub := func(line string) string { return strings.Replace(line, " 0:60 ", " "+dev+" ", 1) }
	writeFile(t, filepath.Join(root, "proc", "1", "mountinfo"), string(captured)+
		sub("312 303 0:60 / /run rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,mode=755,inode64\n")+
		sub("321 305 0:60 /nx-boot-id /proc/sys/kernel/random/boot_id rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,mode=755,inode64\n"))
	writeFile(t, filepath.Join(root, "proc", "self", "mountinfo"), string(captured)+
		"487 459 0:74 /sys /proc/sys ro,nosuid,nodev,noexec,relatime shared:348 - proc proc rw\n"+
		"488 459 0:74 /sysrq-trigger /proc/sysrq-trigger ro,nosuid,nodev,noexec,relatime shared:349 - proc proc rw\n"+
		sub("473 458 0:60 / /run rw,nosuid,nodev,noexec,relatime shared:329 - tmpfs tmpfs rw,mode=755,inode64\n"))
}

// Regression (services-provider WI 5.3, T1 debian:12, systemd 252): under
// ProtectKernelTunables=yes nexusd's /proc/sys hid the container's boot-ID
// mount, so Preflight reported the reboot unavailable ("nothing is mounted
// over /proc/sys/kernel/random/boot_id") and nexusd read the host kernel's
// boot ID while PID 1 and the reboot helper read the container's. The
// reboot is available, from the container mount, and Prepare snapshots the
// container's boot ID.
func TestRebootUsesTheContainersBootIDWhenTheSandboxHidesItsMount(t *testing.T) {
	w := newRBWorld(t, rbOpts{edit: deb12Layout}) // Fails when Preflight reports the reboot unavailable
	st := w.p.Preflight(context.Background())
	if got := detail[string](t, st, DetailBootIDSource); got != BootIDContainerMount {
		t.Errorf("bootIdSource = %q, want container-mount", got)
	}
	w.ops.set("op-own")
	if _, err := w.prepare("op-own", `{"confirmHost":"h1"}`); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if in := w.input("op-own"); in.BootID != deb12ContainerBootID {
		t.Errorf("input.json bootId %q, want the container's %s, not the host kernel's %s",
			in.BootID, deb12ContainerBootID, deb12HostBootID)
	}
}

// A boot-ID mount PID 1 has, hidden by the sandbox, whose source cannot be
// found leaves the reboot unavailable with the reason: the host kernel's
// boot ID is never used in its place.
func TestRebootIsUnavailableWhenAHiddenBootIDMountCannotBeRead(t *testing.T) {
	root := testRoot(t, rbCapture)
	deb12Layout(t, root)
	if err := os.Remove(filepath.Join(root, "run", "nx-boot-id")); err != nil {
		t.Fatal(err)
	}
	fh := svcctltest.NewHost(t, rbCapture)
	p := newOn(t, root, fh)
	configure(t, p, section(t, "enabled: true\nreboot:\n  enabled: true\n"))
	st := p.Preflight(context.Background())
	wantAvailable(t, st)
	if detail[bool](t, st, DetailRebootAvailable) {
		t.Fatal("rebootAvailable = true with a hidden boot-ID mount that cannot be read")
	}
	reason := detail[string](t, st, DetailRebootUnavailableReason)
	for _, w := range []string{"boot ID cannot be read", "PID 1 has a boot ID mounted over /proc/sys/kernel/random/boot_id",
		"/run/nx-boot-id", "could not be confirmed"} {
		if !strings.Contains(reason, w) {
			t.Errorf("rebootUnavailableReason = %q, want it to name %q", reason, w)
		}
	}
	if st.Details[DetailBootIDSource] != nil {
		t.Errorf("bootIdSource = %v, want null", st.Details[DetailBootIDSource])
	}
}

// SEC-1 (services-provider security review): a boot ID read past a mount
// table that cannot be read is the kernel's whether or not a container's is
// mounted over it, so nexusd, the ops manager and the reboot helper could
// disagree about it, and R4 could confirm a reboot that never happened. A
// reboot is unavailable, with the reason, until every table can be read.
func TestSEC1_TheRebootIsUnavailableWhenAMountTableCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove []string // Under the proc root
	}{
		{"this process's table", []string{"self/mountinfo"}},
		{"PID 1's table", []string{"1/mountinfo"}},
		{"both tables", []string{"self/mountinfo", "1/mountinfo"}},
	} {
		for _, container := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, container %v", tc.name, container), func(t *testing.T) {
				root := testRoot(t, rbCapture)
				deb12Layout(t, root) // Both tables, and a container's boot ID mounted for PID 1
				if !container {
					if err := os.Remove(filepath.Join(root, ".dockerenv")); err != nil {
						t.Fatal(err)
					}
				}
				for _, f := range tc.remove {
					if err := os.Remove(filepath.Join(root, "proc", f)); err != nil {
						t.Fatal(err)
					}
				}
				p := newOn(t, root, svcctltest.NewHost(t, rbCapture))
				configure(t, p, section(t, "enabled: true\nreboot:\n  enabled: true\n"))
				st := p.Preflight(context.Background())
				wantAvailable(t, st)
				if detail[bool](t, st, DetailRebootAvailable) {
					t.Fatal("rebootAvailable = true with a mount table that cannot be read")
				}
				reason := detail[string](t, st, DetailRebootUnavailableReason)
				for _, w := range []string{"boot ID cannot be read", "cannot be verified", "could not be confirmed"} {
					if !strings.Contains(reason, w) {
						t.Errorf("rebootUnavailableReason = %q, want it to name %q", reason, w)
					}
				}
				if st.Details[DetailBootIDSource] != nil {
					t.Errorf("bootIdSource = %v, want null", st.Details[DetailBootIDSource])
				}
			})
		}
	}
}

// A table that becomes unreadable after Preflight leaves admission and
// Prepare refusing: neither sends, nor snapshots a boot ID, on an ID the
// helper might not record.
func TestSEC1_AdmissionAndPrepareRefuseWhenAMountTableCannotBeRead(t *testing.T) {
	w := newRBWorld(t, rbOpts{})
	for _, f := range []string{"self", "1"} {
		if err := os.Remove(filepath.Join(w.root, "proc", f, "mountinfo")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	err := w.p.admitReboot(context.Background(), spec.RebootInput{ConfirmHost: rbHost})
	wantStatus(t, err, 409, problem.RebootUnavailable)
	w.ops.set("op-own")
	if _, err := w.prepare("op-own", `{"confirmHost":"h1"}`); err == nil {
		t.Fatal("Prepare = nil, want a refusal")
	}
	if ok, _ := readHelperJSON(filepath.Join(w.opsDir, "op-own"), helperInputFile, &rebootHelperInput{}); ok {
		t.Error("Prepare wrote input.json")
	}
}
