# Adding a provider

A provider is a set of operations on one kind of thing on the host: `docker`
for containers, `apt` for packages, `scripts` for cataloged scripts, `nexusd`
for the daemon itself, `pihole` for a Pi-hole and its local DNS names, and
`host` for a read-only overview of the host itself. Every surface is
derived from the provider's declarations: the REST routes, the
OpenAPI document, the MCP tools, the `nexus` commands, authorization, audit
and idempotency. Adding a provider therefore means writing a **spec**
package, an **implementation** with its **bindings**, and adding **two
composition lines**, plus a third, optional helper line (§3) when the
provider's detached operations need a multi-step Go helper. Core, server,
OpenAPI, MCP and CLI code do not change for a provider that uses the
mechanisms they already have. Those include four generic mechanisms added
for the `nexusd` provider: `def.KindUpload` with `CLI.Upload`,
`CLI.ReconnectWindow`, `provider.DaemonUser` and audit facts
(`audit.AddFact`); see
[section 5](#5-generic-mechanisms-added-for-nexusd). The `host` provider
added one more, `provider.Background`, for work that runs between requests;
see [section 6](#6-generic-mechanism-added-for-the-host-provider-providerbackground).
Remote token and configuration management added the declarations
`SyncMutation`, `SecretOutput`, `Audited` and `CompletesAtAdmission`, two CLI
declarations and a few smaller pieces; see
[section 7](#7-generic-mechanisms-added-for-remote-administration).
A provider that needs
a new kind of mechanism changes core, and that change must be generic, not
keyed on the provider's name, and documented here.

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
  `provider-action` are `POST`, under `/v1/providers/<name>/`. `upload` is
  a `POST` whose body is raw bytes (section 5).
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
  `install`, `remove`, `invoke`, `test`, `add`, `cancel`, `reload`,
  `rollback` and `validate-config`. The last three were added for the
  `nexusd` provider (`reload` also by the nginx provider): `reload` re-reads a configuration, `rollback` returns
  to a previous version, and `validate-config` checks a configuration
  without applying it.
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
| `sync`, `Kind: def.KindUpload` | `provider.Upload(spec.Upload, provider.UploadSpec[In, Out]{MaxBytes, Admit, Receive})`; see section 5 |

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
		nexusd.Spec(),
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
		nexusd.New(nexusd.Options{Host: host}),
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
`docker`, `nexusd` and `pihole` all do:

```go
		case docker.HelperCommand: // "docker-update"
			return docker.UpdateHelper(args[1:], stdout, stderr)
		case nexusd.HelperCommand: // "service-helper"
			return nexusd.Helper(args[1:], stdout, stderr)
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

## 5. Generic mechanisms added for nexusd

The `nexusd` provider (`internal/providers/nexusd`) manages the daemon
itself, and needed four things no earlier provider did. Each was added to
core as a generic mechanism, keyed on declarations rather than on the
provider's name, so any provider can use it.

- **`def.KindUpload` with `CLI.Upload`: a raw upload.** An operation with
  `Kind: def.KindUpload` is served at `POST /v1/providers/<name>/<type>`
  and takes its input from the query and a raw request body, streamed and
  never held in memory whole. It must be `sync` and `mutating`, have a
  scope, declare `Upload` (`def.Upload{ContentTypes, MaxBytes}`, the ceiling
  of its body), have no `MCP` binding and an empty `CLI` binding; the
  `system` provider may not declare one. Bind it with `provider.Upload`, whose
  `MaxBytes` can lower the ceiling at run time from configuration, whose
  `Admit` runs before the body is read, and whose `Receive` stores the body.
  The route answers 413 `upload-too-large` past the limit. The CLI reaches an
  upload only through a long-running operation's `CLI.Upload`
  (`def.CLIUpload`): the command gets a required `--<FileFlag>` flag and a
  flag per sidecar (a small file such as a detached signature, default path
  `<file><Suffix>`), uploads them first, and sets the operation's input field
  `Field` from the upload's result. `nexus nexusd update --file` is built this
  way.
- **`CLI.ReconnectWindow`: waiting across a restart.** A long-running
  operation that restarts `nexusd` declares how long a waited command keeps
  reconnecting while the daemon is away, at most 10 minutes. A certificate
  pin mismatch is never retried.
- **`provider.DaemonUser`: the daemon's own facts.** A provider that
  implements `UseDaemon(provider.Daemon)` is given, once and before any
  request, the configuration path, the state directory and start time of
  the `nexusd` serving it, the same configuration check as `nexusd
  validate-config` (`CheckConfig`), and a reload through the daemon's own
  loop, serialized with SIGHUP (`Reload`).
- **Audit facts: `audit.AddFact`.** Provider code may add `key=value` facts
  to the request's context, and they are written into that request's audit
  records, such as an upload's SHA-256 or a signature verdict. They are
  bounded in number and length and written as they are, so they must never
  hold a secret or key material.

## 6. Generic mechanism added for the host provider: `provider.Background`

The `host` provider samples the host every few seconds to compute rates and
keep a metrics history, so it needed work that runs inside `nexusd` between
requests, which no earlier provider did. It was added to core as a generic
mechanism, `provider.Background` (`internal/provider/background.go`), run by
the daemon (`internal/daemon/background.go`). It is keyed on configuration
enablement, not on the provider's name, so any provider can use it.

- **The contract.** A provider that implements
  `RunBackground(ctx context.Context)` has it called by the daemon in a
  goroutine of its own. `RunBackground` must return promptly once `ctx` is
  done, recover its own panics per unit of work, and release what it holds
  when it returns. A panic that still escapes is recovered and logged at
  error with the provider's name and stack; the provider stays enabled and
  serving, and its run starts again only when a reload next enables it.
- **When it runs.** A run starts when the provider's section is
  `enabled: true`: at startup, after READY=1 and restart recovery, and after
  each successful reload (SIGHUP or the `nexusd` provider's `reload`) that
  enables it. A reload that disables it cancels `ctx` and waits up to
  5 seconds for the run to return before the reload answers. A reload that
  keeps it enabled leaves the run alone, so its state survives, and a reload
  that fails changes nothing. At shutdown, runs are cancelled after the HTTP
  server has stopped and before the operation manager and the state
  database, and waited for at most 5 seconds in all, inside the 10-second
  shutdown deadline. A run still going after its bound is logged at warn and
  left to return; a newer run of the same provider owns its state.
- **When it never runs.** It is never called for a disabled provider, by
  `nexusd validate-config` or `CheckConfig` (which configure throwaway
  providers), or when `nexusd` cannot open its listeners. Preflight does not
  gate it: an enabled provider whose preflight failed still has its run, so
  `RunBackground` checks its own prerequisites.

## 7. Generic mechanisms added for remote administration

Managing `nexusd`'s tokens and configuration file remotely
(`nexus nexusdtoken`, `nexus nexusdconfig`) needed operations that fit none
of the existing shapes. Each was added as a declared, generic mechanism on
`def.Op`, `def.CLIBinding` or a provider interface, checked by
`def.validate` and keyed on the declaration rather than on the provider's
name (CON-3). Any provider may use them; none may be an MCP tool.

- **`def.Op.SyncMutation`: a mutation that answers at once.** A mutating op
  is normally long-running and answers 202 with an operation. A
  `SyncMutation` op answers 200 with its output, with no operation record,
  no lane and no idempotency replay, and its output is never persisted: it
  reaches only the response body. It is the third exception to the
  long-running rule, after `OperationControl` and `def.KindUpload`. It must
  be `ModeSync`, mutating or destructive, a resource or provider action with
  a scope, and must not set `MCP`, `WhatIf`, `Idempotent`, `CLI.Upload`,
  `CLI.ReconnectWindow` or `CLI.WaitedResultItems`; the `system` provider
  may not declare one. It is audited like any mutation, `requested` before
  the handler (fail-closed) and then `completed` or `refused`, and an
  `Idempotency-Key` is refused with `validation-failed`. `nexusd.tokens.add`
  and `nexusd.tokens.remove` use it.
- **`def.Op.SecretOutput`: a credential returned once.** The JSON name of a
  string field of the output that carries a secret, such as `"token"`.
  It requires `SyncMutation`, so the secret is never stored in an operation
  record or the idempotency store. The REST handler sets
  `Cache-Control: no-store` on every answer of such an op, and the CLI gains
  `--save-as-host NAME`, which stores the field in the client configuration
  instead of printing it.
- **`def.Op.Audited`: an audited read.** Reads are not audited. A read-only
  `ModeSync` op that declares `Audited` is audited as a mutation is:
  `requested` before the handler, fail-closed, then `completed` or
  `refused`. The token list and the config reads use it, because who looked
  at the token list or the configuration file matters.
- **`def.Op.CompletesAtAdmission` with `provider.DetachedSpec.Admission`:
  ending a long-running call at admission.** A long-running op that declares
  it may answer a call with 200 and its output before any operation exists:
  no record, lane hold, Prepare or unit. It must be mutating,
  `ModeLongRunning`, a resource or provider action, and not `SyncMutation`,
  `MCP`, `WhatIf` or `CLI.Upload`. Its detached binding provides
  `DetachedSpec.Admission` in place of `Admit` (setting both panics); the
  hook returns a `provider.Admitted` that is either `Done` with the output
  or the input `In` that Prepare runs on. A `Done` answer still passes the
  lane step, so it answers 409 while the lane is busy, and is audited
  `requested` then `completed`. The hook may also carry what admission
  derived into Prepare: a field of `In` tagged `json:"-"` is never part of
  the request schema or the stored operation record, so it reaches Prepare
  only on this call and is empty when recovery re-runs a queued operation.
  Config set uses this: a set that changes nothing answers `changed: false`
  at admission, and a change carries the SHA-256 of the file admission
  validated (`BaseSHA256`) to Prepare as its conflict baseline.
- **`def.CLIBinding.Variadic`: trailing arguments.** Names the `[]string`
  input field that trailing command-line arguments bind to, as in `nexus
  nexusdconfig update KEY=VALUE...`. It cannot be set with `Positional`.
  Empty keeps the `names` convention.
- **`def.CLIBinding.EchoWarnings`: warnings on stderr.** After printing the
  result, the CLI also prints its `warnings` (`apitypes.ActionResult`) on
  stderr, so a warning is seen even when stdout goes to a pipe or a file.
  The output must have a `[]string` field `warnings`. Token remove uses it
  for the self-revocation warning, and config set for the lock-out and
  restart warnings.
- **`problem.Error.RetryAfter`: a `Retry-After` header.** A
  `time.Duration` that is never marshaled into the problem body;
  `problem.Write` sends it as the `Retry-After` header, in whole seconds,
  on a 429. The token rate limit sets it.
- **`provider.Daemon.CheckConfigData`: validating bytes.** The daemon's
  configuration check, the same as `nexusd validate-config`
  (`CheckConfig`), run on bytes that are not on disk yet: `config.Parse`
  plus each built-in provider's `Configure`. Config set's admission uses it
  on the edited file inside the sandbox. The helper outside the sandbox
  cannot import `internal/daemon`, so `cmd/nexusd` passes
  `daemon.CheckConfigData` to it, and both check with the same code.
- **Reload status: `<stateDir>/reload-status.json`.** The daemon writes the
  outcome of every reload, SIGHUP or API, atomically, from the one reload
  function: a per-start `boot` value, a `seq`, the SHA-256 of the bytes it
  read, `applied`, the failing `stage` and `error`, and `restartRequired`.
  Package `internal/reloadstatus` reads and writes it and imports nothing of
  `nexusd`'s, so a helper outside the daemon can learn whether a
  `systemctl reload` it triggered applied the file it wrote, without an API
  call or a credential. Config set's helper polls it for a status newer
  than the one it read before the reload.
