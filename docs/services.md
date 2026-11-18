# System services and reboot

The `services` provider lists the host's system services and their state,
shows one service, and returns its recent log lines. It starts, stops and
restarts the services that the root-owned configuration file allow-lists,
and it reboots the host behind a set of guards. It works through systemd
(Ubuntu and Debian, in a VM or an LXC guest) and through OpenRC (Alpine in
an LXC guest). It is never available on a Proxmox VE node, and it never
talks to the hypervisor.

Rebooting the host `nexusd` runs on must not lose the operation, and no
call may leave the caller unable to reach `nexusd` again. That is why
`nexusd`, the services it needs and the services a caller needs to reach the
host are protected, why the reboot checks that `nexusd` will come back, and
why the reboot operation completes in the boot after the one it started in.

This page covers the configuration and the allow-list, the protected
services, where logs come from, the reboot guards, how the reboot is
recovered and waited for, LXC guests, and the services worth a second
thought before allow-listing them. [security.md](security.md#services-and-reboot)
explains what each scope can do, [mcp.md](mcp.md) covers the agent tools,
and the generated [CLI reference](cli/nexus.md) and [API reference](api.md)
list every flag and field.

> **Status.** Verification on real hosts, including the reboots of an LXC
> guest and of a VM, is still pending, and
> [Measured timings](#measured-timings) has no figures yet.

## Naming and scopes

The provider is named `services`, and its operations are `services.*`:

| Operation | CLI | MCP tool | Scope |
| --- | --- | --- | --- |
| `services.services.list` | `nexus service get` | `list_services` | `services:read` |
| `services.services.get` | `nexus service get NAME` | `get_service` | `services:read` |
| `services.services.logs` | `nexus servicelog get NAME` | `get_service_logs` | `services:logs` |
| `services.services.start` | `nexus service start NAME` | – | `services:write` |
| `services.services.stop` | `nexus service stop NAME` | – | `services:write` |
| `services.services.restart` | `nexus service restart NAME` | – | `services:write` |
| `services.services.control` | `nexus service invoke NAME --action A` | `control_service` | `services:write` |
| `services.actions.reboot` | `nexus machine restart` | none: never a tool | `services:reboot` |

Logs have a scope of their own, `services:logs`, because service logs can
hold tokens, passwords and personal data; every log read is audited. The
noun `host` is taken by the client's list of configured hosts, and
`system` is core's provider, so the reboot is `nexus machine restart`.
Start, stop and restart are one MCP tool, `control_service`, which takes
an `action`, so the four tools fit the tool budget.

## Configuration

```yaml
providers:
  services:
    enabled: false             # Off until enabled here
    control: []                # The allow-list: see below
    reboot:
      enabled: false           # Set only on the host: config set refuses true
      timeout: 5m              # 1m to 30m: how long after the trigger the host must go down
```

| Key | Default | Range | Meaning |
| --- | --- | --- | --- |
| `enabled` | `false` | | Whether the provider serves requests, like every provider |
| `control` | `[]` | | The services start, stop and restart may act on |
| `control[].name` | required | `^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$` | The service: the systemd unit without `.service`, or the OpenRC script name |
| `control[].actions` | all three | `start`, `stop`, `restart` | The actions allowed on it |
| `control[].timeout` | `90s` | above `0s`, at most `10m` | How long an action waits for the service to reach its target state |
| `reboot.enabled` | `false` | | Whether the reboot is offered at all |
| `reboot.timeout` | `5m` | `1m` to `30m` | How long after the reboot is triggered the host must go down |

Check the file with `sudo nexusd validate-config`, then apply it with a
reload (`sudo systemctl reload nexusd`, or `rc-service nexusd reload` on
OpenRC). A reload applies a changed section without a restart. An
operation already admitted keeps the settings it started with.
[install.md](install.md#enable-the-services-provider) has the steps.

### The allow-list

`providers.services.control` lists the only services that start, stop and
restart may act on. Reads cover every service; actions cover only these.

```yaml
providers:
  services:
    enabled: true
    control:
      - name: nx-app           # Any action, 90s each
      - name: mediasvc
        actions: [restart]     # Restart only
        timeout: 30s
```

- **Only the file changes it.** No API, MCP or CLI call adds, changes or
  removes an entry, and remote config set refuses lists
  ([security.md](security.md#lists-and-mappings-are-not-settable)).
- **Defaults.** An entry without `actions` allows all three; `actions: []`
  allows none. An entry without `timeout` waits 90 seconds. An explicit
  `timeout: 0s` is refused, as is anything over `10m`.
- **`nexusd validate-config` refuses** a name that does not match the
  pattern, a name ending in `.service` (write the name without it), a
  duplicate name, an action other than the three, and a name on either
  built-in protected list below, on every host.
- **An entry for a service the host does not have is accepted.** Capabilities
  report it as `missing`, and an action on it answers 404 `not-found`.
- **Capabilities show each entry's status.** `nexus capability get` lists
  `controlAllowList`, each entry with `name`, `actions`, `timeoutSeconds` and
  a `status`: `ok`, `missing`, `protected` (with the reason, for a protection
  `validate-config` cannot see, such as a dependency of `nexusd` or an alias
  of a protected unit), or `unknown` (the check failed, with the reason).

A request for a service that is not listed, or for an action its entry does
not allow, answers 409 `not-allowed` with `configKey:
providers.services.control`, before any operation exists.

### Enabling the reboot

The reboot is off until `providers.services.reboot.enabled: true` is set in
`/etc/nexus/nexusd.yaml` on the host. Remote config set refuses to set it
to `true` ([security.md](security.md#the-deny-list)), so a stolen
`nexusd:admin` token cannot switch the reboot on; setting it to `false`
remotely is allowed. The reboot also needs a token with `services:reboot`,
which no other scope includes.

`reboot.timeout` bounds the wait after the trigger: if the host has not gone
down by then, the operation fails `reboot-did-not-happen`
([Recovery](#recovery-after-the-reboot)).

## Protected services

No `services` operation starts, stops or restarts a protected service, and
none can be allow-listed. The list is fixed in code and cannot be
overridden by configuration: there is no flag that allows acting on one.

| Init system | Built-in protected services |
| --- | --- |
| systemd | `ssh`, `sshd`, `dbus`, `dbus-broker`, `systemd-journald`, `systemd-logind`, `systemd-networkd`, `systemd-resolved`, `NetworkManager`, `networking`, `nexusd`, `getty@*`, `serial-getty@*`, `nexus-op-*` |
| OpenRC | `sshd`, `dropbear`, `networking`, `syslog`, `udev`, `devfs`, `dmesg`, `localmount`, `nexusd` |

An entry ending in `*` protects every name that starts with the part before
it, so `getty@*` covers `getty@tty1`. Matching is case-sensitive, as
service names are. These are the services a caller needs to reach or
recover the host, plus `nexusd` itself.

`nexus-op-*` are the transient units Nexus's own long-running operations
run in. Stopping one would cut its operation short and report it
interrupted; `nexus operation cancel` is the way to stop an operation.

Besides the static list, the provider resolves at startup and at every
reload:

- **The service running `nexusd`.** On systemd it is the last `.service`
  element of the `0::` line of `/proc/self/cgroup`; on OpenRC it is the
  script whose `supervise-daemon` `child_pid` file under
  `/run/openrc/options/` holds `nexusd`'s PID. A renamed unit or script is
  therefore protected too. Capabilities report it as `selfService`.
- **Its dependencies.** On systemd, the `.service` units in that unit's
  `Requires=`, `BindsTo=`, `Requisite=` and `PartOf=` (a local drop-in may
  add any of them); systemd passes a stop or restart of such a unit on to
  `nexusd`. On OpenRC, the providers of each service `nexusd` needs, read
  from `/run/openrc/deptree`: for `net`, the service that provides it, such
  as `networking`.
- **Aliases.** On systemd, an action names a unit by the name given and is
  also checked by its canonical unit, the `Id` that `systemctl show`
  reports, so an alias of a protected unit is protected (`sshd` for `ssh`
  on Ubuntu).

If the service running `nexusd` cannot be identified, the provider stays
available: the protected set is then the static list plus the literal
`nexusd`, capabilities report `selfService: null` with `selfServiceReason`,
and `dependencyResolution` is `skipped`. If the OpenRC dependency tree can be
read only in part, `dependencyResolution` is `partial` with
`dependencyResolutionReason`; every needed name stays protected. Otherwise
it is `complete`. Capabilities list the resolved set as `protected`.

A refusal answers 409 `protected-service` with the reason, before any
operation exists and with nothing sent to the init system. The operation's
helper checks the name and its canonical unit again, against the resolved
set as it was when the operation started, before it acts.

Restarting `nexusd` stays with the `nexusd` provider (`nexus nexusd
restart`, `nexusd:admin`).

## Reading services

List and get run inside `nexusd`, under the systemd sandbox where there is
one, and change nothing.

- **List** returns every service the init system knows, sorted by name: on
  systemd every loaded `.service` unit and every installed unit file
  (templates only as unit files), on OpenRC every script in `/etc/init.d`
  with its runlevels. Filter with `state`, `enabled` and `controllable`;
  page with `limit` (default 100, at most 1000) and `cursor`.
- **Get** adds `since`, `mainPid`, `subState`, `result`, `restarts`,
  `memoryBytes`, `unitFilePath` or `scriptPath`, `canonicalName` (an
  alias's real unit) and `logs` (whether logs can be read, and from where).
  A field the init system does not report is `null`, with the reason in
  `unavailable`.

`state` is one of `running`, `stopped`, `failed`, `starting`, `stopping` and
`unknown`; a word the init system uses that Nexus does not know is
`unknown`, never a guess. `rawState` is the init system's own words, such as
`active (running)` or `crashed`. It is empty for a systemd alias seen only
among the unit files, whose state is `unknown`, because systemd reports the
state of the alias's target, not of the alias. `enabledRaw` is empty, and
`enabled` is `unknown`, for a loaded unit that has no unit file.
`controllable` is true when the service is allow-listed and not protected.
In the list, both go by the listed name alone and do not resolve aliases:
an allow-listed systemd alias of a protected unit is listed as controllable,
while get reports it protected (by its canonical unit) and an action on it
is refused with 409 `protected-service`.

Names that do not match the name pattern, such as systemd-escaped
`\x2d` units, are listed but cannot be named by get, logs or an action.
Environment values, command lines and OpenRC `conf.d` values other than the
two log keys are never read into any output.

## Log sources

`nexus servicelog get NAME` and `get_service_logs` return the service's most
recent log lines, newest last. They need `services:logs`, and every read is
audited with the service's name. `tail` is 1 to 1000 lines, default 200.
There is no follow. Where the lines come from depends on the init system.

### systemd: the journal

The lines come from `journalctl -u NAME.service`, with a fixed argument
list, bounded by 30 seconds. A volatile journal in `/run/log/journal`, as
in an LXC guest, counts as a journal. Each frame carries the line's time.
`since` takes an RFC 3339 time or a duration up to `168h` (7 days); with
`since` and no `tail` the lines are read forward from that time, at most
1000. `priority` keeps lines of that syslog level or more severe.

### OpenRC: declared log files only

The lines come only from the files the service's own configuration declares
as `output_log` and `error_log`, in `/etc/init.d/NAME` or `/etc/conf.d/NAME`.
The path never comes from the request.

- Only a literal path is resolved, with `$RC_SVCNAME` expanded. A path built
  from another variable (Docker's `$DOCKER_OUTFILE`, for example), an empty
  `output_log=`, or a `*_logger` line declares no file the provider can
  read.
- Each file must be a regular file with one hard link, and no component of
  its path, a directory or the file, may be a symbolic link: `nexusd` reads
  it as root, in a directory the service's user often owns, and a hard link
  to another file or a directory swapped for a link would otherwise stream a
  file the user cannot read. A declared path through a link, such as
  `/var/run/app.log` on Alpine, where `/var/run` is a link to `/run`, reports
  its logs unavailable; declare the real path.
- The newest `tail` lines of each file are returned, at most 64 KiB of text
  from the two together, output lines first, then error lines.
- File lines carry no time. Their frames carry the zero time,
  `0001-01-01T00:00:00Z`, which means untimed and is never a real line
  time; the provider never reads a time out of the text. `since` and
  `priority` are refused with 400 `validation-failed`.

Syslog is not searched. Most Alpine services log to BusyBox `syslogd`
(`/var/log/messages`) unless their `conf.d` declares a file, and filtering
syslog by program tag is unreliable, so such a service reports its logs
unavailable.

Over MCP, an untimed page cannot be continued by time, so `get_service_logs`
on OpenRC returns one page with no `nextOffset`; if that page is
`truncated`, ask again with a smaller `tail` ([mcp.md](mcp.md#tool-semantics)).

### When there are no logs

A service with no log source (no `journalctl`, no declared file, or a path
that cannot be resolved) answers 422 `logs-unavailable` with the reason and
streams nothing. `get` reports the same in `logs: {available, source,
reason}`, so a caller can check first. Capabilities report the host's
`logSource`: `journal`, `openrc-files` or `unavailable`.

## Start, stop and restart

`nexus service start|stop|restart NAME`, or `nexus service invoke NAME
--action start|stop|restart` (the same operation as the `control_service`
tool), act on one allow-listed service. Before any operation exists, and with
nothing sent to the init system, each is refused in this order:

1. a name that does not match the pattern, or ends in `.service`: 400
   `validation-failed`;
2. a protected name: 409 `protected-service`;
3. a name the init system does not know: 404 `not-found`;
4. a canonical unit that is protected (an alias): 409 `protected-service`;
5. a name not in the allow-list, or an action its entry does not allow: 409
   `not-allowed`.

An admitted action is a long-running operation. It runs detached from
`nexusd` (in a `nexus-op-<id>` unit outside the sandbox on systemd, under
the operation supervisor `op-supervise` on OpenRC), so it survives a restart
of `nexusd`. One
action per service runs at a time: a second answers 409 `conflict` naming
the running operation.

The operation checks the protected set and the allow-list again when it
starts, reads the state, sends the action through the init system and
polls the state until the target (`running`, or `stopped` for stop) or the
entry's `timeout`:

- start on a running service, or stop on a stopped one, sends nothing and
  succeeds with `changed: false`. A stop of a failed service that stays
  failed with no main process also succeeds with `changed: false`;
- the target state succeeds with the final state. A restart whose tool
  exits non-zero while the same instance (main PID and start time) keeps
  running did not restart it, and fails `service-action-failed` (systemd
  refuses to start or restart a masked unit, which capabilities report as
  `unknown`);
- a different definite state fails `service-action-failed`, with `state`,
  `rawState`, `exitCode` and up to 2 KiB of the init system's output; the
  full output is in the operation log. On OpenRC, `rc-service` runs the
  init script in the foreground, so its output is the script's and can hold
  what `services:logs` protects, while the detail and the operation log are
  read with `services:read`: only OpenRC's own line ` * ERROR: NAME failed to
  start` (or `stop`) is quoted and logged, and the log says that the rest was
  not recorded. systemctl's output is its job message and is kept whole;
- a service still changing at the timeout fails `outcome-unknown`;
- an init tool that cannot be started fails `dependency-unavailable` and
  changes nothing.

Every action is audited `requested`, with the service and the action, and
then with the operation's final state.

## Reboot guards

`nexus machine restart --host H` reboots host H (`services.actions.reboot`,
`services:reboot`). Every guard is checked before any operation exists, in
this order, and named in the what-if report:

1. **`confirm-host`.** `confirmHost` must equal the host's name as
   `nexus capability get` reports it, exactly and case-sensitively.
   Otherwise 400 `validation-failed`. The CLI fills it only from the
   operator (see [CLI wait and output](#cli-wait-and-output)), never from
   `--host`.
2. **`reboot-enabled`.** `providers.services.reboot.enabled` must be true,
   and the reboot must be available on this host
   ([LXC guests](#lxc-guests)). Otherwise 409 `reboot-unavailable` with the
   reason and `configKey`.
3. **`nexusd-enabled-at-boot`.** The service running `nexusd` must start at
   boot: on systemd `systemctl is-enabled` reports `enabled`, on OpenRC it
   is in the `default` or `boot` runlevel. Otherwise 409
   `nexusd-not-enabled-at-boot`, because the host would come back without
   the API.
4. **`no-running-operations`.** No other operation may be queued or
   running, because a reboot would end it `outcome-unknown`. Otherwise 409
   `operations-running` with their `operationIds`. `force` skips this guard
   only, and is audited.
5. **`config-valid`.** The configuration file on disk must pass the same
   check as `nexusd validate-config`, so `nexusd` starts after the boot.
   Otherwise 422 `config-invalid`.

A guard that cannot read what it checks (whether `nexusd` starts at boot,
the running operations, or the configuration check) refuses the reboot with
503 `dependency-unavailable`; `force` does not override that.

`--what-if` runs every guard and refuses none: it reports each guard with
whether it passed and why, the boot ID, the uptime, the reboot-required state
and packages (as the `host` provider reports them), the other running
operations and whether `nexusd` is enabled at boot, and reboots nothing.

An admitted reboot is audited `requested` with `confirmHost`, `force` and
the current boot ID. One reboot runs at a time. The guards run again when
the operation starts, excluding the reboot itself.

The reboot command is `systemctl reboot` on systemd, issued from the
operation's `nexus-op-*` unit outside the sandbox, and `/sbin/reboot` with no
flags on Alpine, where BusyBox init runs OpenRC's shutdown (stopping
`nexusd` first) and then reboots. Capabilities report it as `rebootCommand`.
Shutdown, halt, poweroff, kexec and soft-reboot are not offered.

### Delay and cancel

The reboot is triggered `delaySeconds` after the operation is recorded:
default 10, from 5 to 300 (`--delay-seconds`). The delay lets the 202
answer, the audit record and the operation record reach the client and the
disk before the host goes down.

- **Cancel during the delay.** `nexus operation cancel <id>` ends the
  operation `Canceled`, and nothing is sent to the init system.
- **Cancel once the reboot is requested.** Just before triggering, the
  operation durably records the boot ID and the time in its operation
  directory. From then on a cancel answers 409 `not-cancelable` ("the
  reboot was already requested at T"), and the reboot goes ahead.
- **A cancel that races the record** by a few milliseconds may be accepted
  and still not stop the reboot: once started, the reboot command is never
  killed. The operation then ends `Succeeded` in the next boot, not
  `Canceled`.

**Limitation: the delay window is not a freeze (A-27).** The guards run at
admission and when the operation starts, not again during the delay.
Requests admitted during the delay are not refused: an apt upgrade, a script
run or a service restart can be admitted after the reboot was, and one
still running when the host goes down ends `outcome-unknown`, as any
operation cut short by a shutdown does. Do not start other work on a host
while its reboot is pending.

### Recovery after the reboot

The reboot operation is the documented exception to the rule that an
operation whose process was killed by a shutdown ends `outcome-unknown`
([security.md](security.md#operation-units)). When `nexusd` starts in the
new boot, recovery finds the record, sees that the boot ID changed, and ends
the operation `Succeeded`, with:

- `previousBootId` and `bootId`;
- `rebootRequestedAt` and `upAt`, when `nexusd` was ready again;
- `previousKernelRelease` and `kernelRelease`;
- `rebootRequired`, the `host` provider's reboot-required state after the
  boot.

Its audit record carries both boot IDs. Other outcomes:

- **The trigger failed** (the command could not start, or exited non-zero):
  the record is withdrawn and the operation fails `reboot-did-not-happen`
  with `stage: trigger` and the output. The host keeps running.
- **The host did not go down** within `reboot.timeout` (an inhibitor, or a
  container manager that refused): `reboot-did-not-happen` with
  `stage: wait`.
- **The operation was stopped without a cancel** after the record (an
  operator's `systemctl stop` of its unit, or a shutdown begun elsewhere):
  it stays `Running` until the host does reboot, which completes it
  `Succeeded`, or until its maximum run time, `delaySeconds` +
  `reboot.timeout` + 120 seconds (7 min 10 s with the defaults), which ends
  it `outcome-unknown`.
- **The record cannot be read:** the operation ends `outcome-unknown` at
  once, naming the read error. If the operation's own process ended
  without a result for any other reason, its `outcome-unknown` detail says
  that the host may or may not have rebooted, and to compare
  `/proc/sys/kernel/random/boot_id` with `previousBootId`.

## CLI wait and output

```
nexus machine restart --host nas --confirm-host nas
```

- **Typed confirmation.** Without `--confirm-host`, the CLI fetches the
  host's capabilities and asks on the terminal: "Type the host name nas to
  confirm:", naming the host by its own name, as `nexus capability get`
  reports it. That name may differ from the name the client configuration
  uses for the host (an entry `nas-admin` for the host `nas`), and the host
  compares the answer with its own. If the capabilities cannot be fetched,
  the command fails without asking, and nothing is sent. The same happens
  when the name they report is not plain printable text (a control,
  format or separator character, such as an escape sequence or a
  right-to-left override) or is not one the server would accept: the name is
  never written to the terminal raw, the error shows it quoted, and the
  operator checks the host and passes `--confirm-host`. An explicit
  `--confirm-host` is the confirmation, sent as given for the host to
  check, and no y/N question follows. `--yes` neither fills nor skips it.
  Without a terminal and without the flag, the command is a usage error
  (exit 2) and nothing is sent.
- **One host.** `--host all`, a list of hosts or a `--tag` that selects
  more than one is refused before anything is sent; so is `--stdin`.
- **It waits by default.** The CLI follows the operation, keeps reconnecting
  while the host is down, and prints the operation once `nexusd` answers
  again with it ended. Transport errors and 503 answers are retried every
  second. A certificate pin mismatch is never retried. The reconnect window
  is 10 minutes of continuous unreachability, so the time `nexusd` is up
  before and after the outage does not count against it.
- **When the window is spent** the CLI exits 5 with the operation ID and the
  command to check it later: `nexus operation get <id> --host nas`. The
  reboot itself is not affected.
- `--no-wait` prints the operation as accepted and returns. `--timeout`
  stops waiting after that long and exits 7, without canceling.

**Output.** Like every long-running command (`service start`, `stop`,
`restart`, `invoke` and `machine restart`), the table shows the operation's
row: its ID, status and times. The outcome is the operation's `result`:
`-o json` prints all of it, and `--select` puts chosen fields in the table:

```
nexus machine restart --host nas --confirm-host nas --select status,result.previousBootId,result.bootId,result.upAt,result.rebootRequired.state
nexus service restart nx-app --host nas --select status,result.changed,result.state
```

## LXC guests

A reboot is confirmed by a change of the boot ID,
`/proc/sys/kernel/random/boot_id`. In a VM or on bare metal it is the
kernel's (`bootIdSource: kernel`). In a container, a restart of the
container changes it only when a per-container boot ID is mounted over that
path, as Proxmox's LXC does, which `/proc/self/mountinfo` shows
(`bootIdSource: container-mount`). A container without such a mount keeps
the host kernel's boot ID across its restart, so a reboot could not be
confirmed: the reboot is unavailable there, with the reason in
`rebootUnavailableReason`, and the guard answers `reboot-unavailable`. The
reads, logs and service actions still work.

Whether a boot ID is mounted over that path is read from the mount tables of
`nexusd` and of PID 1 (`/proc/self/mountinfo` and `/proc/1/mountinfo`). When
either table cannot be read, the boot ID is not used, in a container or
anywhere else: the ID `nexusd` read could then differ from the one the
reboot helper records, and a reboot that never happened could look
confirmed. The reboot is unavailable, with that reason; the helper sends
nothing; and an operation recovered while the boot ID cannot be read ends
`outcome-unknown`. On OpenRC, where the supervisors record the boot ID,
detached operations are unavailable while it cannot be read.

A guest reboots itself: `systemctl reboot` on Debian, `/sbin/reboot` on
Alpine, issued inside the guest. The container manager is expected to start
the container again, with a new boot ID inside it. This has not yet been
verified on a real Proxmox guest; if a guest stays stopped instead, start it
from Proxmox, and the operation completes `Succeeded` when `nexusd` comes
back. The provider calls no Proxmox API, `pct`, `qm` or `pvesh`, holds no
hypervisor credential, and is unavailable on a Proxmox VE node itself
(`/etc/pve` or the `pve-manager` package present).

The reboot-required state of an LXC guest is usually `unknown`, because the
guest runs the host's kernel.

## Warnings: services that take others down

Nothing below is protected, because each is a legitimate maintenance
action. Whether to allow-list it is the operator's decision; think about
what else stops with it first.

- **`docker` and `containerd`.** Stopping either stops every container on
  the host, and restarting either restarts them all, Compose projects
  included.
- **`pihole-FTL`.** Stopping it stops DNS for every LAN client that uses
  the Pi-hole. The `pihole` provider restarts it through Pi-hole's own
  `pihole restartdns` (`nexus piholeresolver restart`).
- **An entry that overlaps the nginx or pihole provider.** `nginx` and
  `pihole-FTL` are also managed by the `nginx` and `pihole` providers. An
  allow-list entry for either is a second way to act on it: the `services`
  provider does not run `nginx -t` before a restart, as the `nginx`
  provider does, and its per-service lane does not serialize with those
  providers' operations. Prefer the provider's own operations, and
  allow-list the service only if you need this path too.

`services:write` can take every allow-listed service down
([security.md](security.md#services-and-reboot)); keep the list to what a
token holder should be able to stop.

## Measured timings

With the default `delaySeconds` of 10, the 202 answer reaches the client
before the reboot is triggered. The targets are that `nexusd` answers again
within 120 seconds of the trigger on an LXC guest and within 300 seconds on
a VM, both inside the CLI's 10-minute window. If a host takes longer, the
defaults change, not the guard.

The figures below are filled in by live reboots of real hosts. None has
been measured yet.

| Host | Trigger to `nexusd` answering | Target |
| --- | --- | --- |
| LXC guest, Debian 12, systemd | not yet measured | 120 s |
| LXC guest, Alpine, OpenRC | not yet measured | 120 s |
| VM, Ubuntu, systemd (Docker, Compose stacks, ZFS) | not yet measured | 300 s |
