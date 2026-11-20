package catalog_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	pihole "github.com/mauktik/nexus/internal/providers/pihole/spec"
)

// row is one line of the Operation catalog table in architecture.md. Scope
// is the declaration: "" for any valid token, "target" for Scope.FromTarget,
// and "a | b" for a per-target alternative. CLI is "noun verb" plus the
// positional input field, if any; MCP is the tool name or "".
type row struct {
	id, route, scope string
	risk             def.Risk
	mode             def.Mode
	cli, tool        string
}

// golden is the Operation catalog table, row for row (99 rows: the 25 of
// the base table, the 7 nexusd rows of the self-management table and the 26
// pihole rows of the pihole-dns-zones table, then the nginx operations table
// (planning/nginx-provider/architecture.md, 13 rows), the host table
// (planning/system-provider/architecture.md, Operation catalog, 10 rows),
// the nexusd token, config read and config set rows of the remote-admin
// table (planning/nexusd-remote-admin/architecture.md, Operation catalog,
// 3 + 2 + 1 rows, after nexusd.uploads.add) and the storage table
// (planning/storage-provider/architecture.md, Operation catalog, 12 rows)).
// Rows are in catalog.All() order, so the host rows come after pihole's and
// the storage rows after host's. Any added op must update it on purpose.
var golden = []row{
	{"system.capabilities.get", "GET /capabilities", "", def.RiskReadOnly, def.ModeSync, "capability get", "get_capabilities"},
	{"system.operations.list", "GET /operations", "", def.RiskReadOnly, def.ModeSync, "operation get", "list_operations"},
	{"system.operations.get", "GET /operations/{id}", "target", def.RiskReadOnly, def.ModeSync, "operation get id", "get_operation"},
	{"system.operations.logs", "GET /operations/{id}/logs", "target", def.RiskReadOnly, def.ModeStream, "operationlog get id", "get_operation_logs"},
	{"system.operations.cancel", "POST /operations/{id}/cancel", "target", def.RiskMutating, def.ModeSync, "operation cancel id", "cancel_operation"},
	{"system.audit.list", "GET /audit", "audit:read", def.RiskReadOnly, def.ModeSync, "auditrecord get", ""},
	{"docker.containers.list", "GET /providers/docker/containers", "containers:read", def.RiskReadOnly, def.ModeSync, "container get", "list_containers"},
	{"docker.containers.get", "GET /providers/docker/containers/{name}", "containers:read", def.RiskReadOnly, def.ModeSync, "container get name", "get_container"},
	{"docker.containers.logs", "GET /providers/docker/containers/{name}/logs", "containers:read", def.RiskReadOnly, def.ModeStream, "containerlog get name", "get_container_logs"},
	{"docker.containers.start", "POST /providers/docker/containers/{name}/start", "containers:write", def.RiskMutating, def.ModeLongRunning, "container start name", "start_container"},
	{"docker.containers.stop", "POST /providers/docker/containers/{name}/stop", "containers:write", def.RiskMutating, def.ModeLongRunning, "container stop name", "stop_container"},
	{"docker.containers.restart", "POST /providers/docker/containers/{name}/restart", "containers:write", def.RiskMutating, def.ModeLongRunning, "container restart name", "restart_container"},
	{"docker.containers.remove", "POST /providers/docker/containers/{name}/remove", "containers:write", def.RiskDestructive, def.ModeLongRunning, "container remove name", "remove_container"},
	{"docker.containers.update", "POST /providers/docker/containers/{name}/update", "containers:write", def.RiskMutating, def.ModeLongRunning, "container update name", "update_container"},
	{"docker.actions.check-updates", "POST /providers/docker/actions/check-updates", "containers:read", def.RiskReadOnly, def.ModeLongRunning, "containerupdate get", "check_container_updates"},
	{"apt.packages.list", "GET /providers/apt/packages", "packages:read", def.RiskReadOnly, def.ModeSync, "package get", "list_packages"},
	{"apt.packages.get", "GET /providers/apt/packages/{name}", "packages:read", def.RiskReadOnly, def.ModeSync, "package get name", "get_package"},
	{"apt.upgradable.list", "GET /providers/apt/upgradable", "packages:read", def.RiskReadOnly, def.ModeSync, "packageupdate get", "list_package_updates"},
	{"apt.actions.refresh", "POST /providers/apt/actions/refresh", "packages:write", def.RiskMutating, def.ModeLongRunning, "packageindex update", "refresh_package_index"},
	// The table's [names] and <names> are the names input, not a
	// positional binding.
	{"apt.actions.upgrade", "POST /providers/apt/actions/upgrade", "packages:write", def.RiskDestructive, def.ModeLongRunning, "package update", "upgrade_packages"},
	{"apt.actions.install", "POST /providers/apt/actions/install", "packages:install", def.RiskMutating, def.ModeLongRunning, "package install", "install_packages"},
	{"apt.actions.remove", "POST /providers/apt/actions/remove", "packages:install", def.RiskDestructive, def.ModeLongRunning, "package remove", "remove_packages"},
	{"scripts.scripts.list", "GET /providers/scripts/scripts", "scripts:read", def.RiskReadOnly, def.ModeSync, "script get", "list_scripts"},
	{"scripts.scripts.get", "GET /providers/scripts/scripts/{name}", "scripts:read", def.RiskReadOnly, def.ModeSync, "script get name", "get_script"},
	{"scripts.scripts.invoke", "POST /providers/scripts/scripts/{name}/invoke", "scripts:run | scripts:run:{name}", def.RiskDestructive, def.ModeLongRunning, "script invoke name", "run_script"},
	{"nginx.service.list", "GET /providers/nginx/service", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginx get", "get_nginx_status"},
	{"nginx.actions.test", "POST /providers/nginx/actions/test", "nginx:read", def.RiskReadOnly, def.ModeLongRunning, "nginxconfig test", "test_nginx_config"},
	{"nginx.actions.start", "POST /providers/nginx/actions/start", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx start", "start_nginx"},
	{"nginx.actions.stop", "POST /providers/nginx/actions/stop", "nginx:write", def.RiskDestructive, def.ModeLongRunning, "nginx stop", "stop_nginx"},
	{"nginx.actions.restart", "POST /providers/nginx/actions/restart", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx restart", "restart_nginx"},
	{"nginx.actions.reload", "POST /providers/nginx/actions/reload", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginx reload", "reload_nginx"},
	{"nginx.sites.list", "GET /providers/nginx/sites", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsite get", "list_nginx_sites"},
	{"nginx.sites.get", "GET /providers/nginx/sites/{name}", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsite get name", "get_nginx_site"},
	{"nginx.sites.content", "GET /providers/nginx/sites/{name}/content", "nginx:read", def.RiskReadOnly, def.ModeSync, "nginxsitecontent get name", "get_nginx_site_content"},
	{"nginx.sites.enable", "POST /providers/nginx/sites/{name}/enable", "nginx:write", def.RiskMutating, def.ModeLongRunning, "nginxsite start name", "enable_nginx_site"},
	{"nginx.sites.disable", "POST /providers/nginx/sites/{name}/disable", "nginx:write", def.RiskDestructive, def.ModeLongRunning, "nginxsite stop name", "disable_nginx_site"},
	{"nginx.sites.add", "POST /providers/nginx/sites/{name}/add", "nginx:config", def.RiskMutating, def.ModeLongRunning, "nginxsite add name", "add_nginx_site"},
	{"nginx.sites.update", "POST /providers/nginx/sites/{name}/update", "nginx:config", def.RiskMutating, def.ModeLongRunning, "nginxsite update name", "update_nginx_site"},
	{"nexusd.daemons.list", "GET /providers/nexusd/daemons", "nexusd:read", def.RiskReadOnly, def.ModeSync, "nexusd get", "get_nexusd"},
	{"nexusd.actions.validate-config", "POST /providers/nexusd/actions/validate-config", "nexusd:read", def.RiskReadOnly, def.ModeSync, "nexusd validate-config", "validate_nexusd_config"},
	{"nexusd.actions.reload", "POST /providers/nexusd/actions/reload", "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd reload", ""},
	{"nexusd.actions.restart", "POST /providers/nexusd/actions/restart", "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd restart", ""},
	{"nexusd.actions.update", "POST /providers/nexusd/actions/update", "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusd update", ""},
	{"nexusd.actions.rollback", "POST /providers/nexusd/actions/rollback", "nexusd:admin", def.RiskDestructive, def.ModeLongRunning, "nexusd rollback", ""},
	// The upload has no command of its own (empty noun and verb) and no
	// tool: the CLI reaches it only through update's CLI.Upload.
	{"nexusd.uploads.add", "POST /providers/nexusd/uploads", "nexusd:admin", def.RiskMutating, def.ModeSync, " ", ""},
	{"nexusd.tokens.list", "GET /providers/nexusd/tokens", "nexusd:admin", def.RiskReadOnly, def.ModeSync, "nexusdtoken get", ""},
	{"nexusd.tokens.add", "POST /providers/nexusd/tokens/{name}/add", "nexusd:admin", def.RiskMutating, def.ModeSync, "nexusdtoken add name", ""},
	{"nexusd.tokens.remove", "POST /providers/nexusd/tokens/{name}/remove", "nexusd:admin", def.RiskDestructive, def.ModeSync, "nexusdtoken remove name", ""},
	{"nexusd.config.list", "GET /providers/nexusd/config", "nexusd:admin", def.RiskReadOnly, def.ModeSync, "nexusdconfig get", ""},
	{"nexusd.config.get", "GET /providers/nexusd/config/{name}", "nexusd:admin", def.RiskReadOnly, def.ModeSync, "nexusdconfig get name", ""},
	{"nexusd.actions.update-config", "POST /providers/nexusd/actions/update-config", "nexusd:admin", def.RiskMutating, def.ModeLongRunning, "nexusdconfig update", ""},
	{"pihole.status.get", "GET /providers/pihole/status", "pihole:read", def.RiskReadOnly, def.ModeSync, "pihole get", "get_pihole_status"},
	{"pihole.summary.get", "GET /providers/pihole/summary", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholesummary get", "get_pihole_summary"},
	{"pihole.blocking.get", "GET /providers/pihole/blocking", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholeblocking get", "get_pihole_blocking"},
	{"pihole.actions.stop-blocking", "POST /providers/pihole/actions/stop-blocking", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholeblocking stop", "stop_pihole_blocking"},
	{"pihole.actions.start-blocking", "POST /providers/pihole/actions/start-blocking", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholeblocking start", "start_pihole_blocking"},
	{"pihole.domains.list", "GET /providers/pihole/domains", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholedomain get", "list_pihole_domains"},
	{"pihole.domains.get", "GET /providers/pihole/domains/{name}", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholedomain get name", "get_pihole_domain"},
	{"pihole.actions.add-domain", "POST /providers/pihole/actions/add-domain", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholedomain add", "add_pihole_domain"},
	{"pihole.actions.remove-domain", "POST /providers/pihole/actions/remove-domain", "pihole:write", def.RiskDestructive, def.ModeLongRunning, "piholedomain remove", "remove_pihole_domain"},
	{"pihole.adlists.list", "GET /providers/pihole/adlists", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholeadlist get", "list_pihole_adlists"},
	{"pihole.adlists.get", "GET /providers/pihole/adlists/{name}", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholeadlist get name", "get_pihole_adlist"},
	{"pihole.actions.add-adlist", "POST /providers/pihole/actions/add-adlist", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholeadlist add", "add_pihole_adlist"},
	{"pihole.actions.remove-adlist", "POST /providers/pihole/actions/remove-adlist", "pihole:write", def.RiskDestructive, def.ModeLongRunning, "piholeadlist remove", "remove_pihole_adlist"},
	{"pihole.actions.update-gravity", "POST /providers/pihole/actions/update-gravity", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholegravity update", "update_pihole_gravity"},
	{"pihole.actions.restart-dns", "POST /providers/pihole/actions/restart-dns", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholeresolver restart", "restart_pihole_dns"},
	{"pihole.actions.flush-cache", "POST /providers/pihole/actions/flush-cache", "pihole:write", def.RiskMutating, def.ModeLongRunning, "piholecache remove", "flush_pihole_cache"},
	{"pihole.zones.list", "GET /providers/pihole/zones", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholezone get", "list_pihole_zones"},
	{"pihole.zones.get", "GET /providers/pihole/zones/{name}", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholezone get name", "get_pihole_zone"},
	{"pihole.actions.add-zone", "POST /providers/pihole/actions/add-zone", "pihole:dns", def.RiskMutating, def.ModeLongRunning, "piholezone add", "add_pihole_zone"},
	{"pihole.zones.remove", "POST /providers/pihole/zones/{name}/remove", "pihole:dns", def.RiskDestructive, def.ModeLongRunning, "piholezone remove name", "remove_pihole_zone"},
	{"pihole.records.list", "GET /providers/pihole/records", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholerecord get", "list_pihole_records"},
	{"pihole.records.get", "GET /providers/pihole/records/{name}", "pihole:read", def.RiskReadOnly, def.ModeSync, "piholerecord get name", "get_pihole_record"},
	{"pihole.actions.add-record", "POST /providers/pihole/actions/add-record", "pihole:dns", def.RiskMutating, def.ModeLongRunning, "piholerecord add", "add_pihole_record"},
	{"pihole.records.update", "POST /providers/pihole/records/{name}/update", "pihole:dns", def.RiskMutating, def.ModeLongRunning, "piholerecord update name", "update_pihole_record"},
	{"pihole.records.remove", "POST /providers/pihole/records/{name}/remove", "pihole:dns", def.RiskDestructive, def.ModeLongRunning, "piholerecord remove name", "remove_pihole_record"},
	{"pihole.actions.apply-dns-changes", "POST /providers/pihole/actions/apply-dns-changes", "pihole:dns", def.RiskDestructive, def.ModeLongRunning, "piholednsbatch invoke", "apply_pihole_dns_changes"},
	{"host.summary.get", "GET /providers/host/summary", "host:read", def.RiskReadOnly, def.ModeSync, "hostsummary get", "get_host_summary"},
	{"host.identity.get", "GET /providers/host/identity", "host:read", def.RiskReadOnly, def.ModeSync, "hostidentity get", ""},
	{"host.cpu.get", "GET /providers/host/cpu", "host:read", def.RiskReadOnly, def.ModeSync, "hostcpu get", ""},
	{"host.memory.get", "GET /providers/host/memory", "host:read", def.RiskReadOnly, def.ModeSync, "hostmemory get", ""},
	{"host.filesystems.list", "GET /providers/host/filesystems", "host:read", def.RiskReadOnly, def.ModeSync, "hostfs get", ""},
	{"host.disks.list", "GET /providers/host/disks", "host:read", def.RiskReadOnly, def.ModeSync, "hostdisk get", ""},
	{"host.interfaces.list", "GET /providers/host/interfaces", "host:read", def.RiskReadOnly, def.ModeSync, "hostnet get", ""},
	{"host.netconfig.get", "GET /providers/host/netconfig", "host:read", def.RiskReadOnly, def.ModeSync, "hostnetconfig get", ""},
	{"host.health.get", "GET /providers/host/health", "host:read", def.RiskReadOnly, def.ModeSync, "hosthealth get", "get_host_health"},
	{"host.metrics.get", "GET /providers/host/metrics", "host:read", def.RiskReadOnly, def.ModeSync, "hostmetric get", "get_host_metrics"},
	{"storage.health.get", "GET /providers/storage/health", "storage:read", def.RiskReadOnly, def.ModeSync, "storagehealth get", "get_storage_health"},
	{"storage.pools.list", "GET /providers/storage/pools", "storage:read", def.RiskReadOnly, def.ModeSync, "pool get", "list_zfs_pools"},
	{"storage.pools.get", "GET /providers/storage/pools/{name}", "storage:read", def.RiskReadOnly, def.ModeSync, "pool get name", ""},
	{"storage.datasets.list", "GET /providers/storage/datasets", "storage:read", def.RiskReadOnly, def.ModeSync, "dataset get", "list_zfs_datasets"},
	{"storage.datasets.get", "GET /providers/storage/datasets/{name}", "storage:read", def.RiskReadOnly, def.ModeSync, "dataset get name", ""},
	{"storage.snapshots.list", "GET /providers/storage/snapshots", "storage:read", def.RiskReadOnly, def.ModeSync, "snapshot get", ""},
	{"storage.backups.list", "GET /providers/storage/backups", "storage:read", def.RiskReadOnly, def.ModeSync, "backup get", ""},
	{"storage.backups.summary", "GET /providers/storage/backupsummary", "storage:read", def.RiskReadOnly, def.ModeSync, "backupsummary get", "get_backup_summary"},
	{"storage.timemachine.list", "GET /providers/storage/timemachine", "storage:read", def.RiskReadOnly, def.ModeSync, "timemachine get", "list_timemachine_shares"},
	{"storage.pools.scrub", "POST /providers/storage/pools/{name}/scrub", "storage:scrub", def.RiskMutating, def.ModeLongRunning, "scrub start name", ""},
	{"storage.pools.stop-scrub", "POST /providers/storage/pools/{name}/stop-scrub", "storage:scrub", def.RiskMutating, def.ModeLongRunning, "scrub stop name", ""},
	{"storage.datasets.snapshot", "POST /providers/storage/datasets/{name}/snapshot", "storage:snapshot", def.RiskMutating, def.ModeLongRunning, "snapshot add name", ""},
}

func rowOf(t *testing.T, op def.Op) row {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatal(err)
	}
	scope := op.Scope.Name
	switch {
	case op.Scope.FromTarget:
		scope = "target"
	case op.Scope.PerTarget != "":
		scope += " | " + op.Scope.PerTarget
	}
	cli := op.CLI.Noun + " " + op.CLI.Verb
	if op.CLI.Positional != "" {
		cli += " " + op.CLI.Positional
	}
	tool := ""
	if op.MCP != nil {
		tool = op.MCP.Tool
	}
	// The table's routes are under /v1.
	return row{id: op.ID, route: method + " " + strings.TrimPrefix(path, "/v1"), scope: scope, risk: op.Risk, mode: op.Mode, cli: cli, tool: tool}
}

// TC-146: the catalog matches the Operation catalog table exactly.
func TestCatalogMatchesGoldenTable(t *testing.T) {
	ops := catalog.Ops()
	if len(golden) != 58+13+10+3+2+1+12 {
		t.Fatalf("golden table has %d rows, the architectures' have 58+13+10+3+2+1+12", len(golden))
	}
	if len(ops) != len(golden) {
		t.Errorf("catalog has %d ops, the table %d", len(ops), len(golden))
	}
	for i := range min(len(ops), len(golden)) {
		if got := rowOf(t, ops[i]); got != golden[i] {
			t.Errorf("row %d:\n got %s\nwant %s", i+1, fmt.Sprint(got), fmt.Sprint(golden[i]))
		}
	}
	for _, op := range ops {
		if op.OperationControl && op.ID != "system.operations.cancel" {
			t.Errorf("%s: OperationControl outside cancel", op.ID)
		}
	}
}

// TC-10 (pihole-dns-zones): docs/cli holds exactly one nexus_<noun>.md per
// distinct pihole CLI noun and one nexus_<noun>_<verb>.md per distinct
// (noun, verb) pair, both derived from the spec, and no other nexus_pihole*
// page.
func TestPiholeCLIReferencePages(t *testing.T) {
	want := map[string]bool{}
	nouns := map[string]bool{}
	for _, op := range pihole.Spec().Ops {
		nouns[op.CLI.Noun] = true
		want["nexus_"+op.CLI.Noun+".md"] = true
		want["nexus_"+op.CLI.Noun+"_"+op.CLI.Verb+".md"] = true
	}
	if len(nouns) != 11 || len(want) != 11+22 {
		t.Errorf("spec derives %d nouns and %d pages, want 11 and 33", len(nouns), len(want))
	}
	got, err := filepath.Glob(filepath.Join("..", "..", "docs", "cli", "nexus_pihole*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range got {
		name := filepath.Base(path)
		if !want[name] {
			t.Errorf("docs/cli/%s is no pihole noun or (noun, verb) page", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("docs/cli/%s is missing; run make docs", name)
	}
}
