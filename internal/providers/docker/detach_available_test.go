package docker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// nginx-provider TC-27 (docker part, FR-26, FR-18): container updates are
// admitted when detached operations are available on any init system and
// refused with the detach reason otherwise; the seam's default is the
// platform-neutral detach.Available.
func TestDockerUpdateGateUsesDetachAvailable(t *testing.T) {
	if got, want := reflect.ValueOf(New(Options{}).systemd).Pointer(), reflect.ValueOf(detach.Available).Pointer(); got != want {
		t.Fatal("the availability seam's default is not detach.Available")
	}
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"registry.example/app@" + digestA}
	f.add("ok", "registry.example/app:1", imgOne, false)
	p, _ := updProvider(t, f)
	ctx := context.Background()

	// OpenRC's process backend: available, with no systemd anywhere.
	p.systemd = func() detach.Availability { return detach.Availability{Available: true} }
	if err := p.admitUpdate(ctx, spec.ContainerInput{Name: "ok"}); err != nil {
		t.Fatalf("available: %v, want admitted", err)
	}
	const reason = "no supported init system (systemd or OpenRC)"
	p.systemd = func() detach.Availability { return detach.Availability{Reason: reason} }
	err := p.admitUpdate(ctx, spec.ContainerInput{Name: "ok"})
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.CapabilityUnavailable ||
		pe.Detail != "container updates run only as detached operations: "+reason || pe.Extensions["reason"] != reason {
		t.Fatalf("unavailable: %v, want capability-unavailable with the detach reason", err)
	}

	if refs := systemdOnlyReferences(t, ".."); len(refs) > 0 {
		t.Errorf("files under internal/providers/ still gate on systemd alone: %q", refs)
	}
}

// systemdOnlyReferences lists the Go files under root that name the
// systemd-only availability check.
func systemdOnlyReferences(t *testing.T, root string) []string {
	t.Helper()
	needle := "detach." + "SystemdAvailable" // Split so this file does not match itself
	var refs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		// Pi-hole stays systemd-only on purpose: a timed stop-blocking
		// creates a transient systemd timer, and Pi-hole is not supported
		// on OpenRC. Its descriptions and Preflight reason say so.
		if filepath.Base(filepath.Dir(path)) == "pihole" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			refs = append(refs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return refs
}
