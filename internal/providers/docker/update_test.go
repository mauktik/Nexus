package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// T0 parts of WI 6.4. The helper's Engine flow (pull, stop, rename,
// create, start, health wait, rollback) runs against the real Engine in T1
// (test/integration/docker_update_test.go, TC-116 to TC-ERR70).

const testOpID = "0193ab12-3456-7abc-8def-0123456789ab"

// The declaration: a detached, mutating resource action with its CLI verb
// and MCP tool, in Spec() between remove and check-updates.
func TestUpdateDeclaration(t *testing.T) {
	op := spec.Update.Op()
	if op.ID != "docker.containers.update" || op.Kind != def.KindResourceAction || op.Action != "update" ||
		op.Scope.Name != spec.WriteScope || op.Risk != def.RiskMutating || op.Mode != def.ModeLongRunning ||
		op.Cancel != def.CancelQueuedOnly || !op.Untrusted ||
		op.CLI != (def.CLIBinding{Noun: "container", Verb: "update", Positional: "name"}) ||
		op.MCP == nil || op.MCP.Tool != "update_container" {
		t.Fatalf("declaration %+v", op)
	}
	for _, s := range []string{"Side effects:", "Long-running", "409", "not-updatable", "rolledBack", "-nexus-old-"} {
		if !strings.Contains(op.Description, s) {
			t.Errorf("description lacks %q", s)
		}
	}
	ids := []string{}
	for _, o := range spec.Spec().Ops {
		ids = append(ids, o.ID)
	}
	if i := slices.Index(ids, op.ID); i < 0 || ids[i-1] != spec.Remove.Op().ID || ids[i+1] != spec.CheckUpdates.Op().ID {
		t.Fatalf("ops %v", ids)
	}
	s, err := op.InputSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{`{"name":"-web"}`, `{"name":"a b"}`, `{"name":"web","x":1}`, `{}`} {
		if validateJSON(t, s, in) == nil {
			t.Errorf("accepted %s", in)
		}
	}
	if err := validateJSON(t, s, `{"name":"web"}`); err != nil {
		t.Errorf("refused a name: %v", err)
	}
}

// The binding is Detached, the only kind that survives nexusd (D-8).
func TestUpdateBindingIsDetached(t *testing.T) {
	p := New(Options{})
	for _, b := range p.Bindings() {
		if b.Op().ID != spec.Update.Op().ID {
			continue
		}
		if _, ok := b.Detached(); !ok || b.Kind() != provider.BindDetached {
			t.Fatalf("update binding kind %s", b.Kind())
		}
		return
	}
	t.Fatal("no update binding")
}

func TestShortOpIDAndOldName(t *testing.T) {
	if got := oldName("web", testOpID); got != "web-nexus-old-0123456789ab" {
		t.Fatalf("old name %s", got)
	}
	if !nameRE.MatchString(oldName("web", testOpID)) {
		t.Fatal("old name is not a valid container name")
	}
}

// updProvider is a provider on f with an operations directory, systemd
// available and a fixed nexusd path.
func updProvider(t *testing.T, f *updateEngine) (*Provider, string) {
	t.Helper()
	p := updateProvider(t, f, t.TempDir(), nil)
	ops := t.TempDir()
	p.UseOpsDir(ops)
	p.systemd = func() detach.Availability { return detach.Availability{Available: true} }
	p.executable = func() (string, error) { return "/usr/local/bin/nexusd", nil }
	return p, ops
}

// TC-ERR36 (T0 part), A-36: every notUpdatable kind is refused at
// admission with 422 not-updatable and its reason, the locally built one
// with a hint naming scripts, with no registry call; without systemd the
// update is capability-unavailable, never run in nexusd.
func TestAdmitUpdateRefusals(t *testing.T) {
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"registry.example/app@" + digestA}
	f.images[imgTwo] = []string{}
	f.add("ok", "registry.example/app:1", imgOne, false)
	f.add("pinned", "registry.example/app@"+digestA, imgOne, false)
	f.add("rm", "registry.example/app:1", imgOne, true)
	f.add("byid", strings.TrimPrefix(imgOne, "sha256:")[:12], imgOne, false)
	f.add("built", "registry.example/local:dev", imgTwo, false)
	f.add("compose", "registry.example/app:1", imgOne, false)
	f.containers["compose"]["Config"].(map[string]any)["Labels"] = map[string]string{spec.ComposeProjectLabel: "media"}
	p, _ := updProvider(t, f)
	ctx := context.Background()

	if err := p.admitUpdate(ctx, spec.ContainerInput{Name: "ok"}); err != nil {
		t.Fatalf("ok: %v", err)
	}
	for name, reason := range map[string]string{"pinned": spec.ReasonDigestPinned, "rm": spec.ReasonAutoRemove,
		"byid": spec.ReasonNoRegistrySource, "built": spec.ReasonLocallyBuilt, "compose": spec.ReasonComposeManaged} {
		err := p.admitUpdate(ctx, spec.ContainerInput{Name: name})
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Slug() != problem.NotUpdatable || pe.Status != 422 || pe.Extensions["reason"] != reason {
			t.Errorf("%s: %v, want not-updatable %s", name, err, reason)
			continue
		}
		hint, _ := pe.Extensions["hint"].(string)
		if (name == "built") != strings.Contains(hint, "script") {
			t.Errorf("%s: hint %q", name, hint)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("%d registry calls at admission", n)
	}

	// A stored notUpdatable (the containerd store's local build, known only
	// from the registry) is refused too.
	b, _ := json.Marshal(storedChecks{Containers: map[string]storedCheck{f.id("ok"): {ImageID: imgOne,
		Update: spec.UpdateCheck{Status: spec.UpdateNotUpdatable, Reason: spec.ReasonLocallyBuilt, Detail: "never pushed"}}}})
	if err := p.store().KVPut(checkKVKey, b); err != nil {
		t.Fatal(err)
	}
	if err := p.admitUpdate(ctx, spec.ContainerInput{Name: "ok"}); slugOf(err) != problem.NotUpdatable {
		t.Errorf("stored notUpdatable: %v", err)
	}

	p.systemd = func() detach.Availability { return detach.Availability{Reason: "no systemd here"} }
	err := p.admitUpdate(ctx, spec.ContainerInput{Name: "ok"})
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.CapabilityUnavailable || pe.Extensions["reason"] != "no systemd here" {
		t.Fatalf("without systemd: %v", err)
	}
}

// Prepare writes input.json and returns the helper's spec: argv is the
// absolute nexusd path and three fixed words, never a shell string
// (FR-25), KillMode control-group, MaxRuntime updateMaxRuntime (A-63).
func TestPrepareUpdate(t *testing.T) {
	f := newUpdateEngine(t)
	f.images[imgOne] = []string{"registry.example/app@" + digestA}
	f.add("web", "registry.example/app:1", imgOne, false)
	p, ops := updProvider(t, f)
	r := provider.NewRun(testOpID, nil)

	s, err := p.prepareUpdate(context.Background(), spec.ContainerInput{Name: f.id("web")}, r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ops, testOpID)
	if want := []string{"/usr/local/bin/nexusd", HelperCommand, "--dir", dir}; !slices.Equal(s.Argv, want) {
		t.Fatalf("argv %q, want %q", s.Argv, want)
	}
	for _, a := range s.Argv {
		if strings.ContainsAny(a, " ;|&$`'\"") || a == "sh" || a == "-c" {
			t.Fatalf("argv element %q looks like a shell string", a)
		}
	}
	if s.KillMode != detach.KillControlGroup || s.MaxRuntime != time.Hour || len(s.Env) != 0 || s.Credential != nil ||
		s.Validate() != nil {
		t.Fatalf("spec %+v", s)
	}
	b, err := os.ReadFile(filepath.Join(dir, inputFile))
	if err != nil {
		t.Fatal(err)
	}
	var hin helperInput
	if err := json.Unmarshal(b, &hin); err != nil {
		t.Fatal(err)
	}
	want := helperInput{OperationID: testOpID, Mode: ModeStandalone, Name: "web", ContainerID: f.id("web"),
		OldName: "web-nexus-old-0123456789ab", HealthTimeout: 60 * time.Second, CallTimeout: 5 * time.Second,
		PullTimeout: 30 * time.Minute, Socket: f.socket, ConfigDir: p.Settings().ConfigDir}
	if hin != want {
		t.Fatalf("input.json %+v, want %+v", hin, want)
	}
	if st, err := os.Stat(filepath.Join(dir, inputFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("input.json mode %v %v", st, err)
	}

	// Prepare checks again: a container that became not updatable fails.
	f.containers["web"]["HostConfig"] = map[string]any{"AutoRemove": true}
	if _, err := p.prepareUpdate(context.Background(), spec.ContainerInput{Name: "web"}, provider.NewRun(testOpID+"x", nil)); slugOf(err) != problem.NotUpdatable {
		t.Fatalf("prepare of a now auto-removed container: %v", err)
	}
}

// Finalize states each outcome from result.json, adds rolledBack and the
// digests to a failure, and without result.json ends outcome-unknown naming
// the previous container. It reads files only, and twice is the same.
func TestFinalizeUpdate(t *testing.T) {
	f := newUpdateEngine(t)
	p, ops := updProvider(t, f)
	r := provider.NewRun(testOpID, nil)
	dir := filepath.Join(ops, testOpID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	in := spec.ContainerInput{Name: "web"}
	exit0 := detach.Exit{ExitCode: 0}
	write := func(v any) {
		t.Helper()
		if err := writeJSONAtomic(dir, resultFile, v); err != nil {
			t.Fatal(err)
		}
	}

	// No result: outcome-unknown naming web-nexus-old-<short id>.
	for _, x := range []detach.Exit{{ExitCode: -1, Signal: "SIGKILL"}, {ExitCode: 2}} {
		_, err := p.finalizeUpdate(context.Background(), in, x, r)
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Slug() != problem.OutcomeUnknown || !strings.Contains(pe.Detail, "web-nexus-old-0123456789ab") {
			t.Fatalf("no result.json, exit %+v: %v", x, err)
		}
	}
	if d := p.lostUpdateDetail(r, in); !strings.Contains(d, "web-nexus-old-0123456789ab") || !strings.Contains(d, "by hand") {
		t.Fatalf("lost detail %q", d)
	}
	// input.json names the resolved container when the input was an ID.
	if err := writeJSONAtomic(dir, inputFile, helperInput{Name: "real", OldName: "real-nexus-old-x"}); err != nil {
		t.Fatal(err)
	}
	if d := p.lostUpdateDetail(r, spec.ContainerInput{Name: "abc123abc123"}); !strings.Contains(d, "real-nexus-old-x") {
		t.Fatalf("lost detail %q", d)
	}

	write(helperResult{Changed: true, ContainerID: "new", PreviousContainerID: "old", PreviousDigest: digestA,
		NewDigest: digestB, Warnings: []string{"w"}})
	for range 2 {
		res, err := p.finalizeUpdate(context.Background(), in, exit0, r)
		if err != nil || !res.Changed || res.RolledBack || res.ContainerID != "new" || res.PreviousContainerID != "old" ||
			res.PreviousDigest != digestA || res.NewDigest != digestB || !slices.Equal(res.Warnings, []string{"w"}) {
			t.Fatalf("success: %+v %v", res, err)
		}
	}

	write(helperResult{RolledBack: true, PreviousDigest: digestA, NewDigest: digestB,
		Error: &helperError{Slug: problem.UpdateHealthFailed, Detail: "unhealthy; the previous container was restored"}})
	_, err := p.finalizeUpdate(context.Background(), in, exit0, r)
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Slug() != problem.UpdateHealthFailed || pe.Extensions[spec.ExtRolledBack] != true ||
		pe.Extensions[spec.ExtPreviousDigest] != digestA || pe.Extensions[spec.ExtNewDigest] != digestB {
		t.Fatalf("health failure: %v %+v", err, pe)
	}

	write(helperResult{PreviousDigest: digestA, Error: &helperError{Slug: problem.DependencyUnavailable, Detail: "pull failed"}})
	_, err = p.finalizeUpdate(context.Background(), in, exit0, r)
	if !errors.As(err, &pe) || pe.Slug() != problem.DependencyUnavailable || pe.Extensions[spec.ExtRolledBack] != false ||
		pe.Extensions["provider"] != spec.Name || pe.Extensions[spec.ExtNewDigest] != nil {
		t.Fatalf("pull failure: %v %+v", err, pe)
	}

	write(helperResult{Error: &helperError{Slug: problem.NotFound, Detail: "gone"}})
	if _, err = p.finalizeUpdate(context.Background(), in, exit0, r); slugOf(err) != problem.NotFound {
		t.Fatalf("not found: %v", err)
	}
}

func TestUpdateHelperUsage(t *testing.T) {
	var out, errb strings.Builder
	if code := UpdateHelper(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--dir") {
		t.Fatalf("no --dir: %d %q", code, errb.String())
	}
	dir := t.TempDir()
	if code := UpdateHelper([]string{"--dir", dir}, &out, &errb); code != 1 {
		t.Fatalf("no input.json: %d", code)
	}
	// A result already there is never redone.
	if err := writeJSONAtomic(dir, inputFile, helperInput{Mode: ModeStandalone}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(dir, resultFile, helperResult{Changed: true}); err != nil {
		t.Fatal(err)
	}
	if code := UpdateHelper([]string{"--dir", dir}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "already") {
		t.Fatalf("existing result: %d %q", code, errb.String())
	}
}

// A-35: the new container is the old one's create configuration with the
// image's own defaults dropped, a generated hostname cleared, anonymous
// volumes remounted by name, and each network's configured endpoint.
func TestRecreateSpec(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var img image.InspectResponse
	if err := json.Unmarshal([]byte(`{"Config":{"Env":["PATH=/bin","IMG=1"],"Cmd":["sleep","1"],
		"Labels":{"img":"x","both":"img"},"ExposedPorts":{"80/tcp":{}},"Volumes":{"/imgvol":{}},"WorkingDir":"/srv"}}`), &img); err != nil {
		t.Fatal(err)
	}
	p80, _ := network.ParsePort("80/tcp")
	p81, _ := network.ParsePort("81/tcp")
	c := container.InspectResponse{
		ID: id,
		Config: &container.Config{
			Hostname: id[:12], Image: "registry.example/app:1",
			Env:          []string{"PATH=/bin", "IMG=1", "USER_SET=yes"},
			Cmd:          []string{"sleep", "1"},
			Labels:       map[string]string{"img": "x", "both": "user", "mine": "1"},
			ExposedPorts: network.PortSet{p80: {}, p81: {}},
			Volumes:      map[string]struct{}{"/imgvol": {}, "/anon": {}},
			WorkingDir:   "/srv",
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "front",
			Binds:       []string{"named:/named", "/host:/bind:ro"},
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Target: "/mountanon"}},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "named", Destination: "/named", RW: true},
			{Type: mount.TypeBind, Source: "/host", Destination: "/bind"},
			{Type: mount.TypeVolume, Name: "anonhash", Destination: "/anon", RW: true},
			{Type: mount.TypeVolume, Name: "imghash", Destination: "/imgvol", RW: true},
			{Type: mount.TypeVolume, Name: "mounthash", Destination: "/mountanon", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"front": {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("172.30.0.10")},
				Aliases: []string{"web", id[:12]}, IPAddress: netip.MustParseAddr("172.30.0.10"), EndpointID: "e1",
				DNSNames: []string{"web", id[:12]}},
			"back": {Aliases: []string{"api"}, IPAddress: netip.MustParseAddr("10.0.0.5"), EndpointID: "e2"},
		}},
	}
	cs := recreateSpec(c, img)
	cfg := cs.Config
	if cfg.Hostname != "" || cfg.Image != "registry.example/app:1" || cfg.Cmd != nil || cfg.WorkingDir != "" ||
		!slices.Equal(cfg.Env, []string{"USER_SET=yes"}) {
		t.Fatalf("config %+v", cfg)
	}
	if len(cfg.Labels) != 2 || cfg.Labels["both"] != "user" || cfg.Labels["mine"] != "1" {
		t.Fatalf("labels %v", cfg.Labels)
	}
	if _, ok := cfg.ExposedPorts[p81]; !ok || len(cfg.ExposedPorts) != 1 {
		t.Fatalf("exposed ports %v", cfg.ExposedPorts)
	}
	if _, ok := cfg.Volumes["/anon"]; !ok || len(cfg.Volumes) != 1 {
		t.Fatalf("volumes %v", cfg.Volumes)
	}
	// The old container's own config is untouched.
	if c.Config.Hostname != id[:12] || len(c.Config.Env) != 3 || len(c.Config.Labels) != 3 {
		t.Fatal("recreateSpec changed the inspected container")
	}
	want := []mount.Mount{
		{Type: mount.TypeVolume, Source: "mounthash", Target: "/mountanon"},
		{Type: mount.TypeVolume, Source: "anonhash", Target: "/anon"},
		{Type: mount.TypeVolume, Source: "imghash", Target: "/imgvol"},
	}
	if !slices.EqualFunc(cs.HostConfig.Mounts, want, func(a, b mount.Mount) bool {
		return a.Type == b.Type && a.Source == b.Source && a.Target == b.Target && !a.ReadOnly
	}) {
		t.Fatalf("mounts %+v", cs.HostConfig.Mounts)
	}
	if c.HostConfig.Mounts[0].Source != "" {
		t.Fatal("recreateSpec changed the inspected HostConfig")
	}
	front := cs.Networking.EndpointsConfig["front"]
	if len(cs.Networking.EndpointsConfig) != 1 || front.IPAMConfig.IPv4Address.String() != "172.30.0.10" ||
		!slices.Equal(front.Aliases, []string{"web"}) || front.EndpointID != "" || front.IPAddress.IsValid() || front.DNSNames != nil {
		t.Fatalf("primary endpoint %+v", front)
	}
	if len(cs.Connect) != 1 || cs.Connect[0].Name != "back" || !slices.Equal(cs.Connect[0].Endpoint.Aliases, []string{"api"}) ||
		cs.Connect[0].Endpoint.IPAddress.IsValid() {
		t.Fatalf("connect %+v", cs.Connect)
	}

	// An explicit hostname stays; an overridden entrypoint keeps its
	// command; a container:<x> network mode carries no endpoints.
	c.Config.Hostname, c.Config.Entrypoint = "myhost", []string{"/init"}
	c.HostConfig.NetworkMode = "container:other"
	cs = recreateSpec(c, img)
	if cs.Config.Hostname != "myhost" || !slices.Equal(cs.Config.Cmd, []string{"sleep", "1"}) || cs.Networking != nil || cs.Connect != nil {
		t.Fatalf("explicit settings %+v %+v %+v", cs.Config, cs.Networking, cs.Connect)
	}
	// The default network mode is the bridge network.
	c.HostConfig.NetworkMode = "default"
	c.NetworkSettings.Networks = map[string]*network.EndpointSettings{"bridge": {}}
	if cs = recreateSpec(c, img); cs.Networking == nil || cs.Networking.EndpointsConfig["bridge"] == nil {
		t.Fatalf("default mode %+v", cs.Networking)
	}
}

func TestDependsOn(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for mode, want := range map[string]bool{
		"container:web": true, "container:" + id: true, "container:" + id[:12]: true, "container:/web": true,
		"container:" + id[:5]: false, "container:webx": false, "web": false, "host": false, "": false, "container:": false,
	} {
		if got := dependsOn(mode, "web", id); got != want {
			t.Errorf("dependsOn(%q) = %v", mode, got)
		}
	}
}

// The health wait: healthy, or running for the settle time without a
// health check, succeeds; an exit, a restart or the timeout fails.
func TestWaitReady(t *testing.T) {
	seq := func(states ...container.InspectResponse) func(context.Context, string) (container.InspectResponse, error) {
		i := 0
		return func(context.Context, string) (container.InspectResponse, error) {
			s := states[min(i, len(states)-1)]
			i++
			return s, nil
		}
	}
	run := func(started string, restarts int, h *container.Health) container.InspectResponse {
		return container.InspectResponse{RestartCount: restarts,
			State: &container.State{Running: true, StartedAt: started, Health: h}}
	}
	exited := container.InspectResponse{State: &container.State{ExitCode: 3}}
	ctx := context.Background()

	if err := waitReady(ctx, seq(run("a", 0, &container.Health{Status: container.Starting}),
		run("a", 0, &container.Health{Status: container.Healthy})), "x", 10*time.Second); err != nil {
		t.Errorf("healthy: %v", err)
	}
	start := time.Now()
	if err := waitReady(ctx, seq(run("a", 0, nil)), "x", 10*time.Second); err != nil || time.Since(start) < readySettle {
		t.Errorf("stays running: %v after %s", err, time.Since(start))
	}
	if err := waitReady(ctx, seq(run("a", 0, nil), exited), "x", 10*time.Second); err == nil || !strings.Contains(err.Error(), "code 3") {
		t.Errorf("exits: %v", err)
	}
	if err := waitReady(ctx, seq(run("a", 0, nil), run("b", 1, nil)), "x", 10*time.Second); err == nil {
		t.Error("a restart passed")
	}
	if err := waitReady(ctx, seq(run("a", 0, &container.Health{Status: container.Unhealthy})), "x", time.Second); err == nil ||
		!strings.Contains(err.Error(), "within 1s") {
		t.Errorf("unhealthy until the timeout: %v", err)
	}
}
