# Adding a provider

A provider is a set of operations on one kind of thing on the host: `docker`
for containers, `apt` for packages, `scripts` for cataloged scripts, `pihole`
for a Pi-hole and its local DNS names. Every
surface is derived from the provider's declarations: the REST routes, the
OpenAPI document, the MCP tools, the `nexus` commands, authorization, audit
and idempotency. Adding a provider therefore means writing a **spec**
package, an **implementation** with its **bindings**, and adding **two
composition lines**, plus a third, optional helper line (§3) when the
provider's detached operations need a multi-step Go helper. Core, server,
OpenAPI, MCP and CLI code do not change.

The `scripts` provider is the smallest complete example:
`internal/providers/scripts/spec/spec.go` and
`internal/providers/scripts/provider.go`.

Pi-hole hosts are served by the `pihole` provider
([pihole.md](pihole.md)), which runs on the Debian-based Pi-hole host itself.
Proxmox hosts are an expected future target, without a provider so far; the
`apt` provider avoids Ubuntu-only assumptions so it can run on Debian-based
hosts such as these.

## 1. The spec package: declarations only

Create `internal/providers/<name>/spec`. It holds only data and types, so
the `nexus` CLI can import every declaration, through `internal/catalog`,
without linking any provider code. It may import `internal/def`,
`internal/apitypes`, `internal/problem` and the JSON Schema package, never
the implementation or daemon packages: a test in `internal/catalog` fails
if a spec package reaches the Docker client, bbolt, `internal/detach`,
`internal/ops`, `internal/core` or `internal/provider`.

It declares:

- the provider's `Name`, its read scope and its other scopes, such as
  `widgets:read` and `widgets:write`. A provider declares its own scopes;
  reading any of its operations needs its read scope;
- the input and output types of each operation, with `json` tags and
  `jsonschema` descriptions, which become the API, MCP and CLI help;
- each operation, as a package-level variable built with `def.Define`;
- `Spec()`, which returns a `def.ProviderSpec` listing the operations.

```go
var List = def.Define[ListInput, apitypes.List[Widget]](def.Op{
	ID: Name + ".widgets.list", Provider: Name, Kind: def.KindList, ResourceType: "widgets",
	Summary:     "List widgets",
	Description: "Lists the widgets on this host. Read-only.",
	Scope:       def.Scope{Name: ReadScope}, Risk: def.RiskReadOnly, Mode: def.ModeSync, Idempotent: true,
	CLI:         def.CLIBinding{Noun: "widget", Verb: "get"},
	MCP:         &def.MCPBinding{Tool: "list_widgets", Description: "List the widgets on this host."},
})

func Spec() def.ProviderSpec {
	return def.ProviderSpec{Name: Name, ReadScope: ReadScope, Ops: []def.Op{List.Op()}}
}
```

The fields of `def.Op` (see `internal/def/def.go`) decide everything about
an operation:

- `Kind` places it in the resource model and so decides its route: `list`,
  `get` and `subresource` are `GET`, `resource-action` and
  `provider-action` are `POST`, under `/v1/providers/<name>/`.
- `Risk` is `read-only`, `mutating` or `destructive`. Destructive commands
  ask for confirmation in the CLI, and MCP reports the class as hints.
- `Mode` is `sync`, `stream` (NDJSON frames) or `long-running` (202 and an
  operation to poll). Mutating long-running operations accept an
  `Idempotency-Key`.
- `Description` is written for an agent: what it does, its side effects,
  whether it is long-running. `MCP.Description` is the short form an MCP
  client loads into the model's context, and it counts against the tool
  budget in [mcp.md](mcp.md).
- `CLI` names the generated `nexus <noun> <verb>` command. The noun is one
  lower-case word, and the verb is one of the approved verbs in
  `internal/def/verbs.go`: `get`, `start`, `stop`, `restart`, `update`,
  `install`, `remove`, `invoke`, `test`, `add` and `cancel`.
- `Sensitive` lists input paths that are redacted from audit and never
  persisted; `Untrusted` marks output that carries host-originated text.
- No operation may accept a command, a shell string, an executable path or
  a container specification (FR-25).

`def.Define` validates the declaration and panics at start-up on a bad one,
so a mistake stops the binary rather than misbehaving at run time.

## 2. The implementation and its bindings

Create `internal/providers/<name>` with a type that implements
`provider.Provider` (`internal/provider/provider.go`):

- `Spec()` returns the spec package's `Spec()`.
- `Configure(decode)` strictly decodes the provider's configuration
  section; it runs at start-up and on every reload.
- `Preflight(ctx)` reports whether the provider's dependency is there. An
  unavailable provider answers `capability-unavailable` and leaves the rest
  of `nexusd` unaffected.
- `Bindings()` ties each declaration to its handler.

A binding is built from the typed declaration, so a handler whose input or
output type differs from the declaration does not compile:

| Operation mode | Binding |
| --- | --- |
| `sync` | `provider.Sync(spec.List, p.list)` |
| `stream` | `provider.Stream(spec.Logs, p.logs)` |
| `long-running`, inside `nexusd` | `provider.InProcess(spec.Op, provider.InProcessSpec[In, Out]{Admit, Lane, Run})`; a crash of `nexusd` ends it `outcome-unknown` |
| `long-running`, in its own unit | `provider.Detached(spec.Op, provider.DetachedSpec[In, Out]{Admit, Lane, Prepare, Finalize, LostDetail})`; it runs in a `nexus-op-<id>` systemd unit and survives a restart of `nexusd` |

```go
func (p *Provider) Bindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.list),
	}
}
```

Handlers return a `*problem.Error` of a registered type for every failure a
caller should see. `Admit` runs before an operation exists, so a refusal
there creates nothing; `Lane` serializes operations that must not overlap.

A provider with settings also needs its configuration section: a field in
`Providers` in `internal/config/config.go`, its entry in `providerConfigs`
in `internal/daemon/providers.go`, and its keys, with their defaults, in
`deploy/nexusd.default.yaml`. Every provider is disabled until its
`enabled` key is set.

## 3. The two composition lines

`internal/catalog/catalog.go` and `internal/daemon/providers.go` are the
composition roots, one for the declarations and one for the implementations
`nexusd` runs. Each gets one line.

In `internal/catalog/catalog.go`, add the spec to `All()`, from which the
`nexus` command tree and the generated reference documentation are built:

```go
func All() []def.ProviderSpec {
	return []def.ProviderSpec{
		systemspec.Spec(),
		docker.Spec(),
		apt.Spec(),
		scripts.Spec(),
		pihole.Spec(),
		widgets.Spec(), // the new line
	}
}
```

In `internal/daemon/providers.go`, add the implementation to
`builtinProviders`:

```go
func builtinProviders(host string, log *slog.Logger) []provider.Provider {
	return []provider.Provider{
		docker.New(docker.Options{Host: host, Logger: log}),
		apt.New(apt.Options{Host: host, Logger: log}),
		scripts.New(scripts.Options{Host: host}),
		pihole.New(pihole.Options{Host: host, Logger: log}),
		widgets.New(widgets.Options{Host: host}), // the new line
	}
}
```

### The optional third line: a helper subcommand

A detached operation whose work is one host command needs nothing more:
`Prepare` returns that command's argv in its `detach.Spec`. A provider whose
detached operations need a multi-step Go helper instead, such as a
container update that pulls, recreates and waits for health, or a Pi-hole
DNS change that writes, tests, restarts, verifies and rolls back, adds a
third line: a hidden subcommand `case` in `run` in `cmd/nexusd/main.go`.
`docker` and `pihole` both do:

```go
		case docker.HelperCommand: // "docker-update"
			return docker.UpdateHelper(args[1:], stdout, stderr)
		case pihole.HelperCommand: // "pihole-helper"
			return pihole.Helper(args[1:], stdout, stderr)
```

`Prepare` writes the operation's validated input to a file in the
operation's directory and returns
`detach.Spec{Argv: []string{exe, HelperCommand, "--dir", dir}, ...}`, where
`exe` is the running `nexusd`. `op-supervise` then runs the helper in the
operation's `nexus-op-<id>` unit, outside the `nexusd` sandbox. The helper:

- reads its input only from that directory, and writes its result there for
  `Finalize` to read;
- writes its progress to stdout, which the supervisor frames into the
  operation's output;
- never takes a shell string: it passes every value to a host command as
  one argv element.

The subcommand is not listed in `nexusd`'s usage; only `op-supervise` runs
it.

## 4. Tests and documentation

- **Catalog table:** `internal/catalog/golden_test.go` lists every
  operation; add the new rows on purpose.
- **Conformance:** add a stub for the provider to `catalogStubs` in
  `internal/conformance/conformance_test.go`, one binding per operation.
  The suite then checks every operation generically: routes, OpenAPI,
  scopes, validation, audit and the negative inputs of FR-25.
- **Tool budget:** `TestToolBudget` in `internal/mcpadapter` fails if the
  tool names and descriptions pass the budget; keep `MCP.Description`
  short.
- **Real dependency:** each provider has integration tests against the real
  thing it manages, run by the T1 harness (`make test-harness`), and is
  complete only when a live check on a real host shows the observed state
  equals the commanded state (NFR-7). A mock must encode the dependency's
  known behavior and is never proof on its own.
- **Reference docs:** run `make docs` and commit `docs/cli/` and
  `docs/api.md`. `make test` fails while they are stale.
