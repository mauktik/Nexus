package detach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
)

// nginx-provider TC-22: PlatformOptions selects real systemd seams or the
// process backend. Systemd and Unknown give the zero Options, so New uses
// real systemd exactly as before; OpenRC gives all four process seams.
func TestPlatformOptionsSelection(t *testing.T) {
	cfg := ProcessConfig{OpsDir: t.TempDir(), CgroupRoot: "-"}
	for _, init := range []platform.InitSystem{platform.Systemd, platform.Unknown} {
		o := PlatformOptions(platform.Info{Init: init}, cfg)
		if o.Launch != nil || o.UnitState != nil || o.Signal != nil || o.ListUnits != nil || o.Now != nil {
			t.Errorf("%s: PlatformOptions is not the zero Options", init)
		}
		l, err := New(Config{OpsDir: cfg.OpsDir, Executable: "/x/nexusd"}, o)
		if err != nil {
			t.Fatal(err)
		}
		if !l.RealLauncher() {
			t.Errorf("%s: the launcher does not use the real systemd-run", init)
		}
	}
	o := PlatformOptions(platform.Info{Init: platform.OpenRC}, cfg)
	if o.Launch == nil || o.UnitState == nil || o.Signal == nil || o.ListUnits == nil {
		t.Fatalf("OpenRC: missing process seams: launch %t, unitState %t, signal %t, listUnits %t",
			o.Launch != nil, o.UnitState != nil, o.Signal != nil, o.ListUnits != nil)
	}
	l, err := New(Config{OpsDir: cfg.OpsDir, Executable: "/x/nexusd"}, o)
	if err != nil {
		t.Fatal(err)
	}
	if l.RealLauncher() {
		t.Error("OpenRC: the launcher uses the real systemd-run")
	}
}

// nginx-provider TC-24: Available is platform-neutral.
func TestAvailableIsPlatformNeutral(t *testing.T) {
	procRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(procRoot, "sys", "kernel", "random"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"),
		[]byte("1f0e3c3a-63f4-4a54-9e43-5b0e8b0d1a77\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"self", "1"} {
		if err := os.MkdirAll(filepath.Join(procRoot, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procRoot, pid, "mountinfo"),
			[]byte("22 1 0:20 / /run rw - tmpfs tmpfs rw\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := availableAt(platform.Info{Init: platform.Systemd}, procRoot), SystemdAvailable(); got != want {
		t.Errorf("systemd: %+v, want SystemdAvailable() %+v", got, want)
	}
	if got := availableAt(platform.Info{Init: platform.OpenRC}, procRoot); !got.Available || got.Reason != "" {
		t.Errorf("OpenRC with a boot id: %+v, want available", got)
	}
	// SEC-1: a boot ID read past a mount table that cannot be read is not
	// a boot ID the supervisors can record.
	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, "sys", "kernel", "random"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "sys", "kernel", "random", "boot_id"),
		[]byte("1f0e3c3a-63f4-4a54-9e43-5b0e8b0d1a77\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if got := availableAt(platform.Info{Init: platform.OpenRC}, bare); got.Available || !strings.Contains(got.Reason, "cannot be verified") {
		t.Errorf("OpenRC with a boot id and no mount tables: %+v, want unavailable, the boot ID unverifiable", got)
	}
	got := availableAt(platform.Info{Init: platform.OpenRC}, t.TempDir())
	if got.Available || !strings.Contains(got.Reason, "boot_id") {
		t.Errorf("OpenRC without a boot id: %+v, want unavailable naming boot_id", got)
	}
	got = availableAt(platform.Info{Init: platform.Unknown}, procRoot)
	if got.Available || !strings.Contains(got.Reason, "no supported init system (systemd or OpenRC)") {
		t.Errorf("unknown: %+v, want unavailable naming no supported init system (systemd or OpenRC)", got)
	}
}
