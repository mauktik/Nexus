package catalog_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/catalog"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
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

// golden is the Operation catalog table, row for row (25 rows), then the nginx
// operations table (planning/nginx-provider/architecture.md, 13 rows). Any added
// op must update it on purpose.
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
	if len(golden) != 25+13 {
		t.Fatalf("golden table has %d rows, the architectures' have 25+13", len(golden))
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
