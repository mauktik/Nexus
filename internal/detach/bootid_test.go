package detach

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The boot IDs of the debian:12 T1 run (systemd 252.39-1~deb12u2) where
// nexusd's sandbox read the host kernel's boot ID: the kernel's, and the
// container's, bind-mounted from /run/nx-boot-id.
const (
	hostBootID      = "a2e5b43d-6ecf-4b62-bbde-f26dfa9c7fcf"
	containerBootID = "8f2fa550-e959-47f0-9914-7f25aa8d7365"
)

// Mountinfo lines captured in that container (device 0:60 is its /run
// tmpfs). PID 1's table has the boot-ID bind mount; the table of a process
// under nexusd.service's ProtectKernelTunables=yes has /proc/sys remounted
// read-only without it, and the same /run.
const (
	pid1BootIDLine = "321 305 0:60 /nx-boot-id /proc/sys/kernel/random/boot_id rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,mode=755,inode64\n"
	pid1RunLine    = "312 303 0:60 / /run rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,mode=755,inode64\n"
	sandboxSysLine = "487 459 0:74 /sys /proc/sys ro,nosuid,nodev,noexec,relatime shared:348 - proc proc rw\n" +
		"488 459 0:74 /sysrq-trigger /proc/sysrq-trigger ro,nosuid,nodev,noexec,relatime shared:349 - proc proc rw\n"
	sandboxRunLine = "473 458 0:60 / /run rw,nosuid,nodev,noexec,relatime shared:329 - tmpfs tmpfs rw,mode=755,inode64\n"
	// systemd 259 (ubuntu T1): the sandbox keeps the submount, read-only.
	sandbox259BootIDLine = "496 494 0:72 /nx-boot-id /proc/sys/kernel/random/boot_id ro,nosuid,nodev,noexec,relatime shared:354 - tmpfs tmpfs rw,mode=755,inode64\n"
)

// bootFixture is a /proc whose boot_id is the host kernel's, and a
// directory standing for the container's /run, holding nx-boot-id.
type bootFixture struct {
	proc, run string
	dev       string // The run directory's device, as mountinfo writes it
}

func newBootFixture(t *testing.T) bootFixture {
	t.Helper()
	dir := t.TempDir()
	f := bootFixture{proc: filepath.Join(dir, "proc"), run: filepath.Join(dir, "the run")}
	bootWrite(t, filepath.Join(f.proc, "sys", "kernel", "random", "boot_id"), hostBootID+"\n")
	bootWrite(t, filepath.Join(f.run, "nx-boot-id"), containerBootID+"\n")
	var st syscall.Stat_t
	if err := syscall.Stat(f.run, &st); err != nil {
		t.Fatal(err)
	}
	dev := uint64(st.Dev) // int32 on darwin, uint64 on Linux
	f.dev = fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
	return f
}

// line is a captured line with its 0:60 device made the run directory's and
// its /run mount point the run directory, escaped as the kernel does.
func (f bootFixture) line(captured string) string {
	captured = strings.Replace(captured, " 0:60 ", " "+f.dev+" ", 1)
	captured = strings.Replace(captured, " 0:72 ", " "+f.dev+" ", 1)
	return strings.Replace(captured, " / /run ", " / "+strings.ReplaceAll(f.run, " ", `\040`)+" ", 1)
}

func (f bootFixture) mountinfo(t *testing.T, pid, content string) {
	t.Helper()
	bootWrite(t, filepath.Join(f.proc, pid, "mountinfo"), content)
}

func bootWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Regression (services-provider WI 5.3, T1 debian:12): under systemd 252's
// ProtectKernelTunables=yes nexusd's /proc/sys hides the container's
// boot-ID mount, so nexusd read the host kernel's boot ID while PID 1 and
// the reboot helper read the container's. The boot ID is the one PID 1
// sees, read through the mount's source.
func TestBootIDIsPID1sWhenTheSandboxHidesTheMount(t *testing.T) {
	f := newBootFixture(t)
	f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
	f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))

	got, err := bootID(f.proc)
	if err != nil || got != containerBootID {
		t.Fatalf("bootID = %q, %v; want the container's %s, not the host kernel's %s", got, err, containerBootID, hostBootID)
	}
	b, err := ReadBoot(f.proc, nil)
	want := Boot{ID: containerBootID, Mounted: true, Via: filepath.Join(f.run, "nx-boot-id")}
	if err != nil || b != want {
		t.Fatalf("ReadBoot = %+v, %v; want %+v", b, err, want)
	}
}

// CR-3 (services-provider code review): mountinfo also lists a mount that a
// later mount over /proc/sys shadows. The path then reads procfs, the host
// kernel's boot ID, and the table's boot-ID line says nothing about the
// file that was read: it is not "a boot ID mounted here". ReadBoot goes on
// to PID 1's table, and never reports Mounted with the kernel's ID.
func TestReadBootIgnoresAShadowedBootIDMount(t *testing.T) {
	shadowed := func(f bootFixture) string {
		// The boot-ID mount of device 0:99, then /proc/sys mounted over it
		// again (procfs, 0:74): the own boot_id file is on neither.
		return strings.Replace(f.line(sandbox259BootIDLine), f.dev, "0:99", 1) + sandboxSysLine
	}
	t.Run("PID 1 has the mount: its source is read", func(t *testing.T) {
		f := newBootFixture(t)
		f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
		f.mountinfo(t, "self", shadowed(f)+f.line(sandboxRunLine))
		b, err := ReadBoot(f.proc, nil)
		want := Boot{ID: containerBootID, Mounted: true, Via: filepath.Join(f.run, "nx-boot-id")}
		if err != nil || b != want {
			t.Fatalf("ReadBoot = %+v, %v; want %+v", b, err, want)
		}
	})
	t.Run("PID 1 has none: the kernel's ID, not mounted", func(t *testing.T) {
		f := newBootFixture(t)
		f.mountinfo(t, "1", f.line(pid1RunLine))
		f.mountinfo(t, "self", shadowed(f)+f.line(sandboxRunLine))
		b, err := ReadBoot(f.proc, nil)
		if err != nil || b != (Boot{ID: hostBootID}) {
			t.Fatalf("ReadBoot = %+v, %v; want the kernel's %s with Mounted false", b, err, hostBootID)
		}
	})
}

func TestReadBoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, f bootFixture)
		want    Boot   // With Via relative to the fixture's directory
		wantErr string // Substring of the error, when one is wanted
	}{{
		name: "systemd 257 and later keep the mount in the sandbox",
		setup: func(t *testing.T, f bootFixture) {
			bootWrite(t, filepath.Join(f.proc, "sys", "kernel", "random", "boot_id"), containerBootID+"\n")
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandbox259BootIDLine)+f.line(sandboxRunLine))
		},
		want: Boot{ID: containerBootID, Mounted: true},
	}, {
		name: "no boot-ID mount: the kernel's",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "1", f.line(pid1RunLine))
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))
		},
		want: Boot{ID: hostBootID},
	}, {
		// SEC-1 (services-provider security review): a boot ID read past a
		// mount table that cannot be read is the kernel's whether or not a
		// container's is mounted over it, so it can disagree with the one
		// the reboot helper records. It is an error, never the fallback.
		name:    "no mount tables: unverifiable",
		setup:   func(t *testing.T, f bootFixture) {},
		wantErr: "cannot be verified",
	}, {
		name: "this process's table unreadable: unverifiable",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
		},
		wantErr: "cannot be verified",
	}, {
		name: "this process's table over the cap: unverifiable",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", strings.Repeat("x", maxBootMountinfo+1))
		},
		wantErr: "cannot be verified",
	}, {
		name: "PID 1's table unreadable: unverifiable",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))
		},
		wantErr: "cannot be verified",
	}, {
		name: "PID 1's table is a directory: unverifiable",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))
			if err := os.MkdirAll(filepath.Join(f.proc, "1", "mountinfo"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		wantErr: "cannot be verified",
	}, {
		name: "the source's device is not mounted here",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", sandboxSysLine)
		},
		wantErr: "no mount of device",
	}, {
		name: "the mount here holds another part of the device",
		setup: func(t *testing.T, f bootFixture) {
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", sandboxSysLine+strings.Replace(f.line(sandboxRunLine), " / ", " /other ", 1))
		},
		wantErr: "no mount of device",
	}, {
		name: "the file found is on another device",
		setup: func(t *testing.T, f bootFixture) {
			other := "0:1"
			if f.dev == other {
				other = "0:2"
			}
			f.mountinfo(t, "1", strings.Replace(f.line(pid1BootIDLine), f.dev, other, 1))
			f.mountinfo(t, "self", sandboxSysLine+strings.Replace(f.line(sandboxRunLine), f.dev, other, 1))
		},
		wantErr: "is on device",
	}, {
		name: "the source is not a UUID",
		setup: func(t *testing.T, f bootFixture) {
			bootWrite(t, filepath.Join(f.run, "nx-boot-id"), "not-a-boot-id\n")
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))
		},
		wantErr: "does not hold a boot ID",
	}, {
		name: "the source is a symlink",
		setup: func(t *testing.T, f bootFixture) {
			if err := os.Rename(filepath.Join(f.run, "nx-boot-id"), filepath.Join(f.run, "real")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real", filepath.Join(f.run, "nx-boot-id")); err != nil {
				t.Fatal(err)
			}
			f.mountinfo(t, "1", f.line(pid1RunLine)+f.line(pid1BootIDLine))
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine))
		},
		wantErr: "symbolic links", // ELOOP: O_NOFOLLOW refused the link
	}, {
		name: "the longest root holding the source is used",
		setup: func(t *testing.T, f bootFixture) {
			const other = "0a1b2c3d-0000-4000-8000-000000000001"
			bootWrite(t, filepath.Join(filepath.Dir(f.run), "elsewhere", "nx-boot-id"), other+"\n")
			f.mountinfo(t, "1", strings.Replace(f.line(pid1BootIDLine), " /nx-boot-id ", " /sub/nx-boot-id ", 1))
			f.mountinfo(t, "self", sandboxSysLine+f.line(sandboxRunLine)+
				fmt.Sprintf("490 458 %s /sub %s rw - tmpfs tmpfs rw\n", f.dev, filepath.Join(filepath.Dir(f.run), "elsewhere")))
		},
		want: Boot{ID: "0a1b2c3d-0000-4000-8000-000000000001", Mounted: true, Via: filepath.Join("elsewhere", "nx-boot-id")},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootFixture(t)
			tc.setup(t, f)
			b, err := ReadBoot(f.proc, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), BootIDPath) {
					t.Fatalf("ReadBoot = %+v, %v; want an error naming %s and %q", b, err, BootIDPath, tc.wantErr)
				}
				if b.ID != "" {
					t.Errorf("ReadBoot ID %q with its error, want none: an untrusted boot ID is never replaced by the kernel's", b.ID)
				}
				return
			}
			want := tc.want
			if want.Via != "" {
				want.Via = filepath.Join(filepath.Dir(f.run), want.Via)
			}
			if err != nil || b != want {
				t.Fatalf("ReadBoot = %+v, %v; want %+v", b, err, want)
			}
		})
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	for in, want := range map[string]string{
		`/run`:             "/run",
		`/a\040b`:          "/a b",
		`/a\011b\012c\134`: "/a\tb\nc\\",
		`/short\04`:        `/short\04`,
		`/bad\999`:         `/bad\999`,
	} {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}
