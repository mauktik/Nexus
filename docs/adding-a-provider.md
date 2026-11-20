# Adding a provider

A provider is a set of operations on one kind of thing on the host: `docker`
for containers, `apt` for packages, `scripts` for cataloged scripts, `nexusd`
for the daemon itself, `pihole` for a Pi-hole and its local DNS names,
`host` for a read-only overview of the host itself, and `services` for the
host's system services and its guarded reboot. Every surface is
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
The `services` provider added `CompletesAcrossReboot` with `AfterReboot`
and the reboot record, for an operation that completes in the next boot;
audited streams; `TypedConfirmation`; `OperationsUser`; and untimed frames
in the MCP since-pager; see
[section 8](#8-generic-mechanisms-added-for-the-services-provider).
The `storage` provider added `provider.HealthSource`, through which a
provider's health signals reach the `host` provider's health read; see
[section 9](#9-generic-mechanism-added-for-the-storage-provider-providerhealthsource).
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
name (CON-3). Any provider may use them; a `SyncMutation` or
`CompletesAtAdmission` op may not be an MCP tool.

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

## 8. Generic mechanisms added for the services provider

The `services` provider (`internal/providers/services`, [services.md](services.md))
reboots the host `nexusd` runs on, streams logs whose reads must be audited,
and needs an operator to type the host's name before a reboot. Each need was
added to core as a generic mechanism, declared on `def.Op`, `def.CLIBinding`,
a binding or a provider interface, checked by `def.validate` or the binding,
and keyed on the declaration rather than on the provider's name (CON-4).

- **`def.Op.CompletesAcrossReboot` with `provider.DetachedSpec.AfterReboot`:
  an operation that completes in the next boot.** Normally a detached
  operation whose process a shutdown killed ends `outcome-unknown`. An op
  that declares `CompletesAcrossReboot` is decided in the boot after the one
  it started in. It must be `ModeLongRunning`, mutating or destructive, a
  resource or provider action, declare `CLI.ReconnectWindow`, and be neither
  `SyncMutation` nor `CompletesAtAdmission`. Its detached binding sets
  `DetachedSpec.AfterReboot` exactly when the op declares it; `Detached`
  panics on a mismatch either way. `AfterReboot(ctx, in, provider.Rebooted,
  run)` builds the output from what the manager knows
  (`PreviousBootID`, `BootID`, `RequestedAt` and `ReadyAt`, when this
  `nexusd` became ready), with `Finalize`'s rules: idempotent, reads files,
  never waits.
  - **The record.** The op's own helper calls `detach.RecordReboot` with a
    `detach.RebootRecord{BootID, RequestedAt}` just before it triggers the
    reboot. It writes `ops/<id>/reboot-requested`
    atomically, with fsync of the file and its directory. When the trigger
    fails, or the helper withdraws for a cancel, it calls
    `detach.RetractReboot`. `detach.ReadReboot` reads it, at most
    4 KiB, and refuses an empty boot ID.
  - **The manager's rules**, for an op that declares the flag:
    - **R1, launch:** the op is never launched while this boot's ID is
      unknown.
    - **R2, cancel:** a running op with a reboot record answers 409
      `not-cancelable`; so does one whose record exists but cannot be read.
      Without a record, the cancel goes through `Launcher.Cancel` as for any
      detached op.
    - **R3, hold:** when the unit ends interrupted, lost or canceled and a
      record from this boot exists, nothing is committed. The op stays
      `Running` and its lane stays held until `nexusd` stops, or until its
      `MaxRuntime` passes, which ends it `outcome-unknown`.
    - **R4, after reboot:** when the unit ended in any way other than a
      normal exit and the record's boot ID differs from this boot's,
      `AfterReboot` decides: its output makes the op `Succeeded`, its error
      `Failed`. The terminal audit record carries the facts `previousBootId`
      and `bootId`. A record that cannot be read ends the op
      `outcome-unknown` at once, with no hold and no `AfterReboot`.
    - **R5, normal exit:** a helper that wrote its result always goes to
      `Finalize`, record or not.

    R3 and R4 are decided before the canceled branch, so a canceled unit
    with no record still ends `Canceled`, and ops without the flag behave
    exactly as before.
  - **The helper's stop handling (S1 to S5).** A real cancel is the cancel
    file and then a SIGTERM, which the supervisor forwards to the helper and
    follows with SIGKILL after the spec's `Grace`. A helper that only polled
    the cancel file would be killed by that SIGTERM between writing the
    record and triggering, leaving a record with no reboot. So the helper
    handles SIGTERM itself for its whole run, through
    `signal.NotifyContext`'s stop context, and a check is "the stop context
    is done, or `detach.CancelRequested` is true"; a cancel file that cannot
    be checked counts as a cancel. Where two cases must be told apart it
    looks at the stop context first and the file after it, so a SIGTERM from
    `Launcher.Cancel` always finds its file.
    - **S1, a stop before the record:** no record, no trigger, exit. The
      supervisor reports canceled (file present) or interrupted.
    - **S2, the cancel file at the check after the record:** `RetractReboot`,
      no trigger, exit; the op ends `Canceled` at once.
    - **S3, a SIGTERM with no cancel file at that check** (an operator's
      `systemctl stop`, or a shutdown begun elsewhere): keep the record, no
      trigger, exit. R3 holds the op, and R4 completes it if the host does
      reboot.
    - **S4, a stop after that check, before or during the trigger:** the
      trigger still runs once, under `context.WithoutCancel` with its own
      30-second bound and in its own process group, so neither the stop
      context nor the supervisor's group SIGTERM kills it. Afterwards the
      helper keeps the record if the trigger succeeded, or retracts it and
      writes its failure if not.
    - **S5, a stop after a successful trigger:** exit at once and keep the
      record.

    The spec's `Grace` must exceed the trigger's bound (the reboot uses 40
    seconds), so SIGKILL never lands while the helper is still deciding.
- **Audited streams: `def.Op.Audited` on a `ModeStream` op.** `Audited`
  now also applies to a read-only `ModeStream` op. The Dispatcher writes
  `requested` before the stream starts (fail-closed), then exactly one
  terminal record naming it: `completed` when the stream ends without
  error; `completed` with the fact `clientDisconnected=true` when the client
  went away after at least one frame was delivered, since the data was
  read; `refused` with the problem's slug otherwise, including a client that
  went away before any frame, which also carries `clientDisconnected=true`.
  A consumer that ends the stream on purpose returns `core.ErrStreamStopped`
  from its `emit`, as the MCP pager does when a page is full: that is
  recorded `completed` without `clientDisconnected`. A handler's
  `audit.AddFact` facts land on the terminal record, because `requested` is
  written before the handler runs. Service logs use it.
- **`TypedConfirmation` on `def.CLIBinding`: a value the operator types.** Names
  a required, non-`omitempty`, non-positional string input field of a
  mutating op. The CLI never fills it from another flag, `--host` included,
  or from `--yes`. When its flag is absent it asks on the terminal ("Type
  the host name H to confirm:"); an explicit flag is the confirmation, so
  no y/N question follows; with no terminal and no flag the command is a
  usage error (exit 2) and nothing is sent. The command runs against exactly
  one host: `--host all`, a host list, a `--tag` selecting several, and
  `--stdin` are refused before anything is sent. The server still checks
  the value. `nexus machine restart --confirm-host` is built this way.
- **`OperationsUser` (`provider.OperationsUser`): the operations that have
  not ended.** A
  provider that implements `UseOperations(provider.Operations)` is given,
  once and before any request, a view whose `Active()` lists the `Queued`
  and `Running` operations (`provider.ActiveOperation{ID, Op, Status}`)
  from the store. It follows `KVUser`, `OpsDirUser` and `DaemonUser`. The
  reboot's no-running-operations guard uses it, and fails closed when it is
  not injected or returns an error.
- **Untimed frames in the MCP since-pager.** A log frame always carries
  `t`; the zero time, `0001-01-01T00:00:00Z`, means the line has no time of
  its own, as an OpenRC log file's lines have none. Consumers test
  `T.IsZero()`, and a provider never invents a time. The adapter's
  since-pager gives no `nextOffset` for a page that holds an untimed frame,
  because no time can continue it; a full page is then `truncated` with no
  `nextOffset`, and the agent asks again with a smaller `tail`. The rule is
  keyed on the frame, not the provider: frames that all carry a time, such
  as the journal's and Docker's, page exactly as before.

## 9. Generic mechanism added for the storage provider: `provider.HealthSource`

The `storage` provider's pool, backup and Time Machine findings belong beside
the host's own health, in `host.health.get` (`get_host_health`), without the
`host` provider knowing about storage. This was added to core as a generic
mechanism, `provider.HealthSource` (`internal/provider/health.go`), fanned in
by the daemon (`internal/daemon/health.go`). It is keyed on the interface,
not on a provider name: any provider that implements it contributes a block,
and nothing in core names `storage`.

- **The contract.** A provider that implements
  `HealthSignals(ctx context.Context) (provider.HealthReport, error)` is a
  `HealthSource`. `HealthSignals` must honour `ctx`'s deadline and never
  mutates. Its report carries `ObservedAt`, the signals (each with an id, a
  severity of `critical`, `warning` or `info`, a subject and a message),
  `Counts` over every signal the source found, including any past its own cap,
  `Truncated` when it capped its list, and `Unavailable`, a reason per
  feature it could not read. The fan-in uses the source's `Counts` as they
  are, so the counts agree with the provider's own health read however many
  signals there are; it re-sorts the signals critical, warning, info
  (stable) and keeps the first 10 (`signalsTruncated`).
- **The reader.** A provider that implements `HealthSourcesUser` has
  `UseHealthSources(provider.HealthSources)` called once by `daemon.New`,
  after the registry exists and before anything is served. The `host`
  provider is the reader: `Collect(ctx, timeout)` returns one
  `apitypes.ProviderSignals` block per enabled source, sorted by provider
  name, for its `providerSignals` field. Without an injected fan-in
  (validate-config, tests) that field is `[]`.
- **Enablement and availability.** Only enabled providers are read: a
  disabled provider contributes no block, and enabling it by a reload adds
  one at the next collect. An enabled source's availability is read at call
  time with `Registry.AvailableForCall`, inside the source's flight, so a
  source that was unavailable at startup (a missed 1-second startup
  preflight) is re-checked on the health path once its last preflight is
  older than 5 seconds, exactly as a direct call would re-check it, and the
  re-check's preflight is not cancelled by the collect's
  deadline. A source still unavailable gives a block with `counts: null` and
  `unavailable.signals` set to `capability-unavailable: <reason>`, and its
  `HealthSignals` is not called.
- **The bound.** A collect waits at most the host's `sourceTimeout`
  (default 2 s), and the `host` provider runs it beside its own reads, so a
  source delays `host.health.get` by at most that bound whatever the source
  does. `HealthSignals` gets a context with that deadline.
- **One call in flight per source.** The fan-in keeps one call in flight per
  source (availability, then `HealthSignals`); a later collect joins it
  until its own deadline rather than starting another. Each call runs in its
  own goroutine with a recover: a panic is logged at error with the
  provider's name and reported as `internal error`; an error is reported with its text. A source
  that ignores `ctx` keeps its one goroutine until it returns, and no second
  one is started beside it.
- **Late and stale reports.** The fan-in keeps each source's last completed
  report, also one that completed after the deadline of the collect that
  started it. When the current call errs or misses the deadline, the block is
  that last report with its own `observedAt`, `stale: true` and
  `unavailable.signals` naming why the current read did not complete ("the
  current read did not finish within 2s", or the error), provided the report
  is at most 5 minutes old; otherwise, or with no earlier report, the block
  has `counts: null`, `signals: []` and that reason.
