// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	gomaps "maps"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

// Recreating a standalone container (FR-12, A-35). The new container is
// created from the old one's inspected Config, HostConfig and networks
// with only the image changed, by the Engine API's POST /containers/create
// (Engine API v1.52: body ContainerConfig with HostConfig and
// NetworkingConfig; "name" query parameter) and POST
// /networks/{id}/connect.
//
// The inspected Config is the container's own settings merged with its
// image's. Values equal to the old image's are dropped, so the new image's
// own CMD, ENTRYPOINT, ENV, labels, exposed ports, volumes, working
// directory, user, health check and stop signal take effect, as when the
// container was first created; what the user set is kept.

// createSpec is the create request of the new container and the networks
// to connect after it is created.
type createSpec struct {
	Config     *container.Config
	HostConfig *container.HostConfig
	Networking *network.NetworkingConfig
	// Connect are the networks beyond the first, connected after create
	// and before start, sorted by name.
	Connect []networkEndpoint
}

type networkEndpoint struct {
	Name     string
	Endpoint *network.EndpointSettings
}

// recreateSpec builds the new container's create request from the old
// container c and its image img (whose Config nil drops nothing).
func recreateSpec(c container.InspectResponse, img image.InspectResponse) createSpec {
	cfg := &container.Config{}
	if c.Config != nil {
		cp := *c.Config
		cfg = &cp
	}
	cfg.Env = slices.Clone(cfg.Env)
	cfg.Labels = gomaps.Clone(cfg.Labels)
	cfg.ExposedPorts = gomaps.Clone(cfg.ExposedPorts)
	cfg.Volumes = gomaps.Clone(cfg.Volumes)
	if img.Config != nil {
		dropImageDefaults(cfg, img)
	}
	// A hostname Docker generated is the old container's short ID; the new
	// container gets its own. An explicit hostname is kept.
	if len(c.ID) >= 12 && cfg.Hostname == c.ID[:12] {
		cfg.Hostname = ""
	}

	hc := &container.HostConfig{}
	if c.HostConfig != nil {
		cp := *c.HostConfig
		hc = &cp
	}
	// --mount type=volume,target=<t> without a source is anonymous too: it
	// gets the old volume's name in place.
	hc.Mounts = slices.Clone(hc.Mounts)
	for i, m := range hc.Mounts {
		if m.Type == mount.TypeVolume && m.Source == "" {
			hc.Mounts[i].Source = volumeAt(c, m.Target)
		}
	}
	hc.Mounts = append(hc.Mounts, anonymousRemounts(c, hc)...)

	out := createSpec{Config: cfg, HostConfig: hc}
	primary := primaryNetwork(hc.NetworkMode)
	if primary == "" || c.NetworkSettings == nil {
		return out
	}
	short := ""
	if len(c.ID) >= 12 {
		short = c.ID[:12]
	}
	for _, name := range slices.Sorted(gomaps.Keys(c.NetworkSettings.Networks)) {
		ep := endpointConfig(c.NetworkSettings.Networks[name], c.ID, short)
		if name == primary {
			out.Networking = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{name: ep}}
			continue
		}
		out.Connect = append(out.Connect, networkEndpoint{Name: name, Endpoint: ep})
	}
	return out
}

// dropImageDefaults removes from cfg what the old image supplied.
func dropImageDefaults(cfg *container.Config, i image.InspectResponse) {
	img := i.Config
	// The image's CMD applies only with the image's ENTRYPOINT: an
	// overridden entrypoint keeps the container's command as it is.
	if sameStrings(cfg.Entrypoint, img.Entrypoint) {
		cfg.Entrypoint = nil
		if sameStrings(cfg.Cmd, img.Cmd) {
			cfg.Cmd = nil
		}
	}
	cfg.Env = slices.DeleteFunc(cfg.Env, func(e string) bool { return slices.Contains(img.Env, e) })
	for k, v := range img.Labels {
		if got, ok := cfg.Labels[k]; ok && got == v {
			delete(cfg.Labels, k)
		}
	}
	for p := range img.ExposedPorts {
		if port, err := network.ParsePort(p); err == nil {
			delete(cfg.ExposedPorts, port)
		}
	}
	for v := range img.Volumes {
		delete(cfg.Volumes, v)
	}
	if cfg.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == img.User {
		cfg.User = ""
	}
	if cfg.StopSignal == img.StopSignal {
		cfg.StopSignal = ""
	}
	if sameHealth(cfg.Healthcheck, img.Healthcheck) {
		cfg.Healthcheck = nil
	}
	if sameStrings(cfg.OnBuild, img.OnBuild) {
		cfg.OnBuild = nil
	}
	if sameStrings(cfg.Shell, img.Shell) {
		cfg.Shell = nil
	}
}

func sameStrings(a, b []string) bool { return len(a) == len(b) && slices.Equal(a, b) }

func sameHealth(a, b *container.HealthConfig) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	}
	return sameStrings(a.Test, b.Test) && a.Interval == b.Interval && a.Timeout == b.Timeout &&
		a.StartPeriod == b.StartPeriod && a.StartInterval == b.StartInterval && a.Retries == b.Retries
}

// anonymousRemounts mounts each anonymous volume of c again by its name,
// so its data is kept: the Engine gives an anonymous volume no name to
// reuse, and an unnamed one would be created empty. A volume already
// mounted by a HostConfig bind or mount stays as it is.
func anonymousRemounts(c container.InspectResponse, hc *container.HostConfig) []mount.Mount {
	var out []mount.Mount
	for _, m := range c.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" || explicitTarget(hc, m.Destination) {
			continue
		}
		out = append(out, mount.Mount{Type: mount.TypeVolume, Source: m.Name, Target: m.Destination, ReadOnly: !m.RW})
	}
	return out
}

// explicitTarget reports whether hc already mounts something at dest.
func explicitTarget(hc *container.HostConfig, dest string) bool {
	for _, b := range hc.Binds {
		// name-or-path:dest[:mode]
		if parts := strings.Split(b, ":"); len(parts) >= 2 && parts[1] == dest {
			return true
		}
	}
	return slices.ContainsFunc(hc.Mounts, func(m mount.Mount) bool { return m.Target == dest })
}

// volumeAt is the name of c's volume mounted at dest, or "".
func volumeAt(c container.InspectResponse, dest string) string {
	for _, m := range c.Mounts {
		if m.Type == mount.TypeVolume && m.Destination == dest {
			return m.Name
		}
	}
	return ""
}

// primaryNetwork is the network the container is created on, from its
// network mode: "" for none, host and container:<x>, which have no
// endpoints to carry.
func primaryNetwork(mode container.NetworkMode) string {
	switch {
	case mode.IsNone(), mode.IsHost(), mode.IsContainer():
		return ""
	case mode == "" || mode.IsDefault():
		return "bridge"
	}
	return string(mode)
}

// endpointConfig is the configured part of an inspected endpoint: static
// addresses, links, aliases, driver options, gateway priority and MAC
// address. The runtime part (IDs, assigned addresses, DNS names) is left to
// the Engine. An alias that is the old container's ID, which an older
// Engine adds by itself, is dropped.
func endpointConfig(ep *network.EndpointSettings, id, short string) *network.EndpointSettings {
	if ep == nil {
		return &network.EndpointSettings{}
	}
	out := &network.EndpointSettings{
		IPAMConfig: ep.IPAMConfig.Copy(),
		Links:      slices.Clone(ep.Links),
		Aliases: slices.DeleteFunc(slices.Clone(ep.Aliases), func(a string) bool {
			return a == short || a == id
		}),
		DriverOpts: gomaps.Clone(ep.DriverOpts),
		GwPriority: ep.GwPriority,
		MacAddress: ep.MacAddress,
	}
	return out
}

// dependsOn reports whether a network or PID mode joins the container
// named name with ID id: container:<name>, container:<id> or a prefix of
// the ID of at least 12 characters.
func dependsOn(mode, name, id string) bool {
	target, ok := strings.CutPrefix(mode, "container:")
	if !ok || target == "" {
		return false
	}
	target = strings.TrimPrefix(target, "/")
	return target == name || (len(target) >= 12 && strings.HasPrefix(id, target))
}
