// Tests for planning/system-provider/test-cases.md TC-HP-109 (WI 5.1): the
// host commands against servers running the real host provider.

package cli

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

	"github.com/mauktik/nexus/internal/platform"
	hostprov "github.com/mauktik/nexus/internal/providers/host"
	hostspec "github.com/mauktik/nexus/internal/providers/host/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

// ubuntuCapture is the host provider's Ubuntu capture: real files from a
// running host (internal/providers/host/testdata/README.md).
var ubuntuCapture = filepath.Join("..", "providers", "host", "testdata", "ubuntu-systemd", "fs")

// realHost is the real host provider, as nexusd builds it, reading the
// Ubuntu capture through its allow-list. The seams that would reach this
// machine (statfs, uname, the interfaces' addresses, commands) are fixed:
// every mount point statfs-es as a 10 GiB filesystem, half used.
func realHost(t *testing.T, own string) *hostprov.Provider {
	t.Helper()
	plat := platform.DetectAt("linux", ubuntuCapture)
	plat.Init = platform.Systemd
	return hostprov.New(hostprov.Options{
		Host: own, Platform: &plat, GOOS: "linux", FS: os.DirFS(ubuntuCapture),
		Statfs: func(string) (hostprov.Usage, error) {
			return hostprov.Usage{IsDir: true, Blocks: 2621440, Bfree: 1310720, Bavail: 1310720, Files: 655360,
				Ffree: 327680, Frsize: 4096}, nil
		},
		Uname: func() (string, error) { return "aarch64", nil },
		Addrs: func() (map[string][]netip.Prefix, error) { return map[string][]netip.Prefix{}, nil },
		Run: func(context.Context, []string) (svcctl.Result, error) {
			return svcctl.Result{}, errors.New("no commands in this test")
		},
	})
}

// TC-HP-109: CLI surface against a server running the real provider. Owner WI 5.1. Requirements: FR-14.
func TestTCHP109_CLISurfaceAgainstAServerRunningThe(t *testing.T) {
	h1 := startProviderServer(t, "h1", realHost(t, "h1"), true, hostspec.ReadScope)
	h2 := startProviderServer(t, "h2", realHost(t, "h2"), true, hostspec.ReadScope)
	home := writeClientConfig(t, "h1", clientHost{name: "h1", url: h1.url, token: h1.token},
		clientHost{name: "h2", url: h2.url, token: h2.token})
	env := homeEnv(home)

	// Step 1: one item, named current.
	r := runCLI(env, "hostsummary", "get", "-o", "json")
	if r.code != 0 {
		t.Fatalf("hostsummary get -o json: exit %d: %s", r.code, r.stderr)
	}
	var items []struct {
		Name string `json:"name"`
		Host string `json:"host"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &items); err != nil {
		t.Fatalf("hostsummary get -o json: %v\n%s", err, r.stdout)
	}
	if len(items) != 1 || items[0].Name != hostspec.CurrentName || items[0].Type != hostspec.SummaryType {
		t.Errorf("hostsummary get -o json = %+v, want one %s item named %s", items, hostspec.SummaryType,
			hostspec.CurrentName)
	}

	// Step 2: the Columns table, one row per filesystem.
	r = runCLI(env, "hostfs", "get", "-o", "json")
	if r.code != 0 {
		t.Fatalf("hostfs get -o json: exit %d: %s", r.code, r.stderr)
	}
	var fss []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &fss); err != nil || len(fss) == 0 {
		t.Fatalf("hostfs get -o json: %v, %d items\n%s", err, len(fss), r.stdout)
	}
	r = runCLI(env, "hostfs", "get", "-o", "table")
	if r.code != 0 {
		t.Fatalf("hostfs get -o table: exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if got, want := strings.Fields(lines[0]), []string{"HOST", "MOUNTPOINT", "FSTYPE", "SIZEBYTES", "USEDPERCENT",
		"READONLY"}; !slices.Equal(got, want) {
		t.Errorf("hostfs get header %q, want %q", got, want)
	}
	if rows := len(lines) - 1; rows != len(fss) {
		t.Errorf("hostfs get has %d rows, want one per filesystem (%d):\n%s", rows, len(fss), r.stdout)
	}

	// Step 3: items from both hosts.
	r = runCLI(env, "hostnet", "get", "--host", "all", "-o", "json")
	if r.code != 0 {
		t.Fatalf("hostnet get --host all: exit %d: %s", r.code, r.stderr)
	}
	var ifaces []struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &ifaces); err != nil {
		t.Fatalf("hostnet get --host all: %v\n%s", err, r.stdout)
	}
	hosts := map[string]int{}
	for _, i := range ifaces {
		hosts[i.Host]++
	}
	if len(hosts) != 2 || hosts["h1"] == 0 || hosts["h2"] == 0 || hosts["h1"] != hosts["h2"] {
		t.Errorf("hostnet get --host all: items per host %v, want the same interfaces from h1 and h2", hosts)
	}

	// Step 4: a NoInput get takes no --stdin.
	r = runCLI(env, "hostsummary", "get", "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "hostsummary get") || strings.Contains(r.stdout, "--stdin") {
		t.Errorf("hostsummary get --help: exit %d, want usage without --stdin:\n%s", r.code, r.stdout)
	}

	// Step 5: host is still the configured-hosts command.
	r = runCLI(env, "host", "get")
	if r.code != 0 || !strings.Contains(r.stdout, h1.url) || !strings.Contains(r.stdout, h2.url) {
		t.Errorf("host get: exit %d, want the configured hosts h1 and h2:\n%s%s", r.code, r.stdout, r.stderr)
	}
}
