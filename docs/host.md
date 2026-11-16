# Host overview

The `host` provider is a read-only overview of the Linux host `nexusd`
runs on, in the spirit of Cockpit's Overview page: identity, CPU, memory,
filesystems, block devices, network interfaces, gateways and DNS
resolvers, health flags, and a short metrics history. It reads `/proc`,
`/sys`, a few files under `/etc` and `/run`, `statfs` on mount points, and
four fixed read-only commands. It changes nothing on the host: it writes no
file, signals no process and changes no setting.

While the provider is enabled, one background sampler reads the host's
counters every `sampleInterval`. Usage percentages, rates and the metrics
history come from it; everything else is read at the call. The history is
kept in memory only.

This page covers the naming, the configuration, each operation's fields,
how a field that cannot be read is reported, the sampler and the history,
and the rules behind the filesystem list, virtualization, the reboot flag
and the clock. [security.md](security.md#what-the-host-provider-reads)
explains what the provider may read, and the generated
[CLI reference](cli/nexus.md) and [API reference](api.md) list every flag
and field.

## Naming

The provider is named `host`, its operations are `host.*`, and its one
scope is `host:read`. The name `system` is reserved by core for the
built-in operations `system.capabilities.get`, `system.operations.*` and
`system.audit.list`, so the provider cannot use it (ASM-1), and its scope
is named after the provider, as `nginx:read` and `pihole:read` are.

The CLI nouns all start with `host`, because `nexus host` is the client's
list of configured hosts: `hostsummary`, `hostidentity`, `hostcpu`,
`hostmemory`, `hostfs`, `hostdisk`, `hostnet`, `hostnetconfig`,
`hosthealth` and `hostmetric`.

## Configuration

```yaml
providers:
  host:
    enabled: false             # Off until enabled here
    sampleInterval: 10s        # 2s..60s: how often the sampler reads the host
    historyDuration: 1h        # sampleInterval..24h: how far back the in-memory history goes
    sourceTimeout: 2s          # 250ms..5s: bound on one read of a file, statfs call or command
```

| Key | Default | Range | Meaning |
| --- | --- | --- | --- |
| `enabled` | `false` | | Whether the provider serves requests and the sampler runs, like every provider |
| `sampleInterval` | `10s` | `2s` to `60s` | How often the sampler reads the host's counters |
| `historyDuration` | `1h` | `sampleInterval` to `24h` | How far back the metrics history goes |
| `sourceTimeout` | `2s` | `250ms` to `5s` | The bound on one blocking source: `statfs` of one mount point, one command, one hardware sensor or NIC speed read |

`nexusd validate-config` refuses a value outside its range, naming the key
(`providers.host.sampleInterval` and so on). The defaults keep 360 points
of history (1 h at 10 s).

Check the file with `sudo nexusd validate-config`, then apply it with a
reload (`sudo systemctl reload nexusd`, or `rc-service nexusd reload` on
OpenRC):

- A reload that enables the provider starts the sampler, with an empty
  history.
- A reload that disables it stops the sampler and drops the history.
- A reload that keeps it enabled keeps the sampler and its history. A new
  `sampleInterval` or `historyDuration` takes effect at the sampler's next
  tick: the history is resized to the new duration and interval and keeps
  its newest points.
- A new `sourceTimeout` applies to the next bounded call.

### When the provider is unavailable

The provider is checked at startup, at each reload, and when capabilities
are read with a refresh. It reports `unavailable`, with the reason, when:

- the host is not Linux;
- `/proc/stat`, `/proc/meminfo` or `/proc/self/mountinfo` cannot be read.

An unavailable `host` provider answers `capability-unavailable` and leaves
every other provider working. Any other source that fails later makes only
its own fields unavailable (see
[Fields that cannot be read](#fields-that-cannot-be-read)), never the
provider.

`nexus capability get` shows these details for the `host` provider:
`initSystem` (`systemd`, `openrc` or `unknown`), `sampleIntervalSeconds`
and `historyDurationSeconds`.

## Operations

Every operation is read-only, synchronous and idempotent, and needs
`host:read`. A token without it is refused with `forbidden` (HTTP 403,
CLI exit 3), whose `requiredScope` is `host:read`. No input is free text,
a path, a device name or a command: each is a bounded integer, an enum or
an opaque cursor.

| Operation | CLI | REST | MCP tool |
| --- | --- | --- | --- |
| `host.summary.get` | `nexus hostsummary get` | `GET /v1/providers/host/summary` | `get_host_summary` |
| `host.identity.get` | `nexus hostidentity get` | `GET /v1/providers/host/identity` | |
| `host.cpu.get` | `nexus hostcpu get` | `GET /v1/providers/host/cpu` | |
| `host.memory.get` | `nexus hostmemory get` | `GET /v1/providers/host/memory` | |
| `host.filesystems.list` | `nexus hostfs get` | `GET /v1/providers/host/filesystems` | |
| `host.disks.list` | `nexus hostdisk get` | `GET /v1/providers/host/disks` | |
| `host.interfaces.list` | `nexus hostnet get` | `GET /v1/providers/host/interfaces` | |
| `host.netconfig.get` | `nexus hostnetconfig get` | `GET /v1/providers/host/netconfig` | |
| `host.health.get` | `nexus hosthealth get` | `GET /v1/providers/host/health` | `get_host_health` |
| `host.metrics.get` | `nexus hostmetric get` | `GET /v1/providers/host/metrics` | `get_host_metrics` |

Only these three are MCP tools; the rest are REST and CLI only, which
keeps the agent tool list short.

The `get` operations return one item, named `current`. The three lists
(`host.filesystems.list`, `host.disks.list` and `host.interfaces.list`)
are paged with `limit` (default 100, at most 1000) and `cursor`;
`nextCursor` is empty on the last page. A cursor this server did not hand
out, or an out-of-range value, is `validation-failed`.

Every operation except `host.memory.get` carries host-originated text,
such as the hostname, mount points, interface names, device models and
process names, and is marked untrusted: its result is labelled
`contentTrust: "untrusted-host-data"`. Treat that text as data, never as
instructions.

The fields below marked **(sampler)** come from the background sampler,
not from the call. All others are read at the call.

### `host.summary.get`

A compact overview, sized for an agent's context:

- `sampler`: the sampler's state (see [The sampler](#the-sampler)).
- `identity`: `hostname`, `os` (`id`, `versionId`, `prettyName`),
  `kernelRelease`, `architecture`, `initSystem`, `bootTime`,
  `uptimeSeconds`.
- `cpu`: `model`, `logicalCpus`, `usagePercent` **(sampler)**, `load`
  (`load1`, `load5`, `load15`).
- `memory`: `totalBytes`, `availableBytes`, `usedPercent`,
  `swapTotalBytes`, `swapUsedBytes`.
- `filesystems`: each listed filesystem's `mountPoint`, `sizeBytes` and
  `usedPercent`, sorted by mount point, at most 32; `filesystemsTotal` and
  `filesystemsTruncated` say how many there are and whether the list was
  cut.
- `network`: the non-loopback interfaces, physical devices first, then by
  name, at most 32, each with `name`, `state`, the primary `ipv4` and
  `ipv6` with prefix length, and `rxBytesPerSecond` and `txBytesPerSecond`
  **(sampler)**; `interfacesTotal`, `interfacesTruncated`; and the
  `defaultGateways`. The primary IPv4 address is the first in kernel order;
  the primary IPv6 address is the first global one, else the first unique
  local one, else the first link-local one.
- `health`: `rebootRequired` (`true`, `false` or `unknown`),
  `failedServicesCount` and `oomKills`.

### `host.identity.get`

- `hostname`: see [Hostname](#hostname).
- `os` (`id`, `versionId`, `prettyName`) and `initSystem`: from the same
  code as `system.capabilities.get`, so the two never disagree.
- `kernelRelease` (as `uname -r`), `architecture` (as `uname -m`),
  `bootTime`, `uptimeSeconds`.
- `virtualization`: `type`, `kind` and `evidence`; see
  [Virtualization](#virtualization).
- `dmi`: `systemVendor`, `productName`, `productVersion`, `biosVendor`,
  `biosVersion`, from `/sys/class/dmi/id`.
- `timezone`: `name` (IANA), `offsetSeconds` (the current offset from UTC)
  and `source`. The name comes from `/etc/timezone`, else from the target
  of the `/etc/localtime` link; the offset from `/etc/localtime`. Without
  `/etc/localtime` the zone is UTC with offset 0.
- `clock`: `synchronized` (`true`, `false` or `unknown`) and `source`; see
  [Clock synchronization](#clock-synchronization).

### `host.cpu.get`

- `model`, `physicalCores`, `logicalCpus` (threads online).
- `usagePercent` **(sampler)**: busy time over the last interval, where
  busy is total time less idle and iowait (iowait counts as idle, as
  `vmstat` does).
- `times` **(sampler)**: `userPercent`, `systemPercent`, `iowaitPercent`,
  `stealPercent`, each a share of the interval's total time.
- `perCpu`: for each CPU, `cpu` (its number in `/proc/stat`),
  `usagePercent` **(sampler)** and `frequencyMhz`.
- `load`: `load1`, `load5`, `load15`.
- `frequencyMhz`: the average current frequency.
- `temperatures`: each sensor of `/sys/class/hwmon` and
  `/sys/class/thermal`, with `source` (`hwmon` or `thermal`), `chip`,
  `label` and `celsius`. It is `[]` with a reason on a host without
  sensors, as in most VMs.

### `host.memory.get`

Read from `/proc/meminfo` and `/proc/pressure` at the call. Nothing comes
from the sampler.

- Bytes: `totalBytes`, `freeBytes`, `availableBytes` (`MemAvailable`),
  `usedBytes` (total less available, as procps `free` reports it),
  `buffersBytes`, `cachedBytes` (`Cached` plus `SReclaimable`),
  `sharedBytes` (`Shmem`), `swapTotalBytes`, `swapUsedBytes`,
  `swapFreeBytes`.
- `usedPercent`: `usedBytes` as a percentage of `totalBytes`.
- `pressure`: pressure stall information for `cpu`, `memory` and `io`, each
  `some` and `full` with `avg10`, `avg60` and `avg300` in percent. A
  resource the kernel does not report is null with the reason.

### `host.filesystems.list`

One item per real filesystem, named by its path-escaped mount point and
sorted by mount point:

- `mountPoint`, `source` (usually the device) and `fsType`.
- `sizeBytes`, `usedBytes`, `availableBytes` (available to unprivileged
  users), `usedPercent` (used as a percentage of used plus available, as
  `df` reports it, so reserved blocks are left out).
- `inodesTotal`, `inodesUsed`, `inodesFree`: null with a `not reported:`
  reason for a filesystem without inode counts, such as btrfs or vfat.
- `readOnly`: from the host's `/proc/1/mountinfo`.

Each mount's usage costs at most one `sourceTimeout`, and the mounts are
read at once, so a hung NFS mount is listed with its usage null and
`timed out after` (or `still pending:`) while the rest answer in time. See
[Filesystems](#filesystems) for what is listed.

When neither `/proc/1/mountinfo` nor `/proc/self/mountinfo` can be read,
the whole list fails with `dependency-unavailable`.

### `host.disks.list`

One item per whole block device (disks and NVMe namespaces), named by its
kernel name and sorted by it. Partitions are not items, and neither are
`ram*` and `zram*` devices or loop devices without a backing file.

- `model`, `sizeBytes`, `rotational`, `removable`.
- `readBytesPerSecond`, `writeBytesPerSecond`, `readIops`, `writeIops`
  **(sampler)**.
- `sampler`: the sampler's state.
- `partitions`: each with `name`, `sizeBytes` and `mountPoints` (from the
  host's mount table; `[]` when it is not mounted).

When `/sys/block` cannot be listed, the whole list fails with
`dependency-unavailable` rather than answering `[]`, since an empty list
would claim the host has no disks.

### `host.interfaces.list`

One item per network interface, loopback included, named by the interface
name and sorted by it:

- `loopback`, `state`, `mac`, `mtu`, `speedMbps` (null with a reason when
  unknown or virtual).
- `ipv4` and `ipv6`: each `address` with its `prefixLength`.
- `rxBytes`, `txBytes`: the cumulative counters, read at the call.
- `rxBytesPerSecond`, `txBytesPerSecond` **(sampler)**.
- `rxErrors`, `txErrors`, `rxDropped`, `txDropped`.
- `sampler`: the sampler's state.

`state` is `up`, `down`, `unknown` or `dormant`. The kernel's `notpresent`
and `lowerlayerdown` are reported as `down`, and any other value as
`unknown`.

When `/sys/class/net` cannot be listed, the whole list fails with
`dependency-unavailable` rather than answering `[]`.

### `host.netconfig.get`

- `defaultGateways`: each IPv4 and IPv6 default route, from
  `/proc/net/route` and `/proc/net/ipv6_route`, with `family` (`ipv4` or
  `ipv6`), `address`, `interface` and `metric`.
- `dns`: `source`, the file read, and `servers`, each an `address` and
  `stub`. When `/etc/resolv.conf` lists only systemd-resolved's local
  listeners (`127.0.0.53`, `127.0.0.54`) and
  `/run/systemd/resolve/resolv.conf` exists, the servers are that file's
  upstream servers, followed by the local listeners marked `stub`, and
  `source` is `/run/systemd/resolve/resolv.conf`. Otherwise they are the
  servers of `/etc/resolv.conf` in file order, and any loopback address is
  marked `stub`.

### `host.health.get`

- `rebootRequired`: `state` (`true`, `false` or `unknown`), `check`,
  `evidence` and `packages`; see [Reboot required](#reboot-required).
- `failedServices`: `count`, `names` (sorted, at most 20),
  `namesTruncated` and `source`. On systemd these are the failed units of
  `systemctl list-units --state=failed`; on OpenRC the crashed services of
  `rc-status --crashed`.
- `oomKills`: OOM kills since boot, from `oom_kill` in `/proc/vmstat`.
  Kernels before 4.13 do not count them, and the field is then null with a
  `not present:` reason.
- `topByCpu` and `topByMemory` **(sampler)**: the 5 processes using the
  most CPU and the 5 with the largest resident memory, each with `pid`,
  `command` (the 15-character command name), `uid`, `user`, `cpuPercent`
  and `rssBytes`. `cpuPercent` is a share of one CPU, so it can exceed
  100, as in `top`. `user` comes from `/etc/passwd`.
- `processSampleSeconds` **(sampler)**: the interval the process CPU
  percentages cover.
- `sampler`: the sampler's state.

### `host.metrics.get`

The history over a window, on a regular grid. Inputs:

- `windowSeconds`: 60 to 86400, default 3600.
- `stepSeconds`: 0 (the default) for the stored resolution, or 2 to 3600.
- `metric`: any of `cpu`, `load`, `memory`, `swap`, `network` and `disk`,
  each at most once. The default is `cpu`, `load`, `memory` and `swap`.

Output:

- `from`: the time of each series' first value, the later of the window's
  start and the oldest point held, aligned down to the step.
- `stepSeconds`: the effective step, `points`: the values per series, and
  `historyStart`: the oldest point held.
- `series`: each with `metric`, `name`, `entity`, `unit` and `values`.
  Each value is the mean of the stored points in its step, and `null` where
  the step has none. This is the one place a `null` needs no reason.
- `entitiesTruncated`: the interfaces and disks that had no history slot.
- `sampler`: the sampler's state.

| `metric` | Series (`name`) | `unit` | `entity` |
| --- | --- | --- | --- |
| `cpu` | `usagePercent` | `percent` | empty |
| `load` | `load1` | `load` | empty |
| `memory` | `usedBytes`, `availableBytes` | `bytes` | empty |
| `swap` | `usedBytes` | `bytes` | empty |
| `network` | `rxBytesPerSecond`, `txBytesPerSecond` | `bytes/s` | the interface name |
| `disk` | `readBytesPerSecond`, `writeBytesPerSecond` | `bytes/s` | the disk name |

A series holds at most 500 values and a response at most 20,000, so the
step is raised as needed: to at least `windowSeconds` / 500, then further
until both bounds hold. See [Metrics history](#metrics-history) for what is
stored.

## Fields that cannot be read

A field the provider cannot read is never zero-filled. It is `null`, or
`[]` for a list, and its JSON name is a key of the `unavailable` map of the
object it belongs to, whose value is the reason. Every `null` field of an
object is listed there; the one exception is a `null` value in a metrics
series, which marks a step with no sample. A failing source makes only its
own fields unavailable: the operation still answers, and a total failure
gives a resource whose fields are all null with reasons.

For example, on a VM without sensors:

```json
"temperatures": [],
"unavailable": {"temperatures": "not reported: no temperature sensors (common in VMs)"}
```

Each reason starts with one of these prefixes, which are stable:

| Reason | Meaning |
| --- | --- |
| `warming up` | Fewer than two samples exist, for the sampler or for this entity. A new interface, disk or process is `warming up`, never `counter reset:` |
| `stale: last sample Ns old` | The sampler's newest sample is older than three intervals |
| `not running` | The sampler is not running |
| `permission denied: <path>` | `EACCES` or `EPERM`, most likely `nexusd`'s sandbox |
| `not present: <path>` | The file does not exist, or the kernel or device lacks the feature |
| `not present: <command>` | The command's fixed absolute path does not exist |
| `timed out after <d>: <what>` | A blocking source overran `sourceTimeout` |
| `still pending: <what>` | The same source has been stuck for `sourceTimeout` or longer since an earlier call, so nothing new was started |
| `not visible: <mount> is hidden in nexusd's mount namespace` | The host mount is not the filesystem a lookup of that path reaches in `nexusd`'s namespace, so no syscall was made on it |
| `unrecognized format: <path>` | The source was read but did not parse |
| `counter reset: <entity>` | A counter decreased (for a CPU, a column other than idle and iowait, or the total), or the entity changed identity (interface index, device number, process start time), between two samples |
| `not reported: <why>` | Legitimately absent, such as no inode counts, a virtual NIC's speed or no sensors |
| `command failed: <command> exit N` | A command ran and exited non-zero |
| `command failed: <command> not started` | A command could not be started for another reason |
| `not supported: <what>` | No such source on this platform or OS family |
| `internal error: access not allow-listed` | The provider tried a path outside its allow-list: a bug, which a test keeps from shipping |

`<what>` is the absolute path (`/sys/...`), `statfs <mount>` or the
command's path. A reason names only a fixed path, a fixed command or a
mount point from the mount table; it never quotes file contents or command
output.

A field reading `warming up` or `counter reset:` is always null, never 0.

### Whole-operation errors

- `capability-unavailable`: the provider is disabled or unavailable.
- `forbidden`: the token lacks `host:read` (`requiredScope` names it).
- `validation-failed`: an input out of range, or a cursor this server did
  not issue.
- `dependency-unavailable`: only a list's own source is missing:
  `/sys/block` for the disks, `/sys/class/net` for the interfaces, and both
  mount tables for the filesystems.

Nothing else fails a read.

## The sampler

The sampler runs inside `nexusd` only while the provider is enabled. It
takes one sample at once, then one every `sampleInterval`, and computes
CPU usage, interface and disk rates, and process CPU from the difference
between the last two samples. Each response that uses these values has a
`sampler` object:

- `state`: `ready`, `warming-up` (fewer than two samples since start or
  enable), `stale` (the newest sample is older than three intervals) or
  `not-running`. Clients must accept values they do not know.
- `sampledAt`: when the newest sample used was taken.
- `intervalSeconds`: the actual seconds between the two samples the rates
  came from.
- `ageSeconds`: the seconds since `sampledAt`.

The values that come from the sampler are null with `warming up` until a
second sample exists (one `sampleInterval` after start or enable), with
`stale: last sample Ns old` when the sampler has fallen behind by more than
three intervals, and with `not running` when it is not running.

A counter that decreases, an interface whose index changes, a disk whose
device number changes or a PID reused by a new process is a reset: that
interval gives no value for it (`counter reset:`), and the next one does.
The CPU `idle` and `iowait` columns are the exception: the kernel can step
them back ([proc(5)](https://man7.org/linux/man-pages/man5/proc_stat.5.html)
says of iowait "The value in this field may decrease in certain
conditions"), so a decrease in either counts as no idle or iowait time for
that interval and the CPU percentages are still given. A decrease in any
other CPU column, or in the CPU total, is a reset.

The process scan reads `/proc/<pid>/stat` and `/proc/<pid>/status` for
every process. When one scan costs more than 0.5% of the interval, it runs
only every few ticks, at least every sixth, and logs that once at info;
`processSampleSeconds` then shows the longer interval.

A sampler tick that panics is recovered and logged at error with its
stack, and the next tick starts from the last good sample.

## Metrics history

Every sample is appended to the history, which `host.metrics.get` reads.

- **Memory only.** The history is never written to disk. It starts empty
  when `nexusd` starts and each time a reload enables the provider, and is
  dropped when a reload disables it. `historyStart` is the oldest point
  held.
- **Size.** The history covers `historyDuration` at `sampleInterval`: 360
  points by default. At most **21,600** points are stored. When
  `historyDuration` / `sampleInterval` exceeds that, which is possible only
  with an interval under 4 seconds (24 hours at 2 seconds is 43,200
  samples), each stored point is the mean of a pair of samples, and the
  stored resolution is twice the interval.
- **Per-entity series.** Interface and disk series are kept for at most
  **32 interfaces** and **32 disks**. Physical devices take a slot first,
  then the rest by name; an entity seen in none of the points held, that
  is, for a whole `historyDuration`, frees its slot. The disk slots go only to the whole devices `host.disks.list`
  lists, never to a partition. The entities left without a slot are
  counted in `entitiesTruncated`.
- **Default metrics.** `cpu`, `load`, `memory` and `swap`. `network` and
  `disk` are returned only when asked for.

## Hostname

`identity.hostname` (and the summary's) is the kernel hostname in
`nexusd`'s UTS namespace, read from `/proc/sys/kernel/hostname`. The
`host` field of `nexus capability get`, and of every resource, is the
Nexus host label from `host.name` in `/etc/nexus/nexusd.yaml`, which
defaults to the OS hostname. The two agree unless `host.name` is set.

Under the systemd unit, `ProtectHostname=yes` gives `nexusd` a private UTS
namespace holding the hostname as it was at start. A rename of the host
therefore shows in `hostname` only after `nexusd` restarts. This reading
of FR-5 is to be confirmed before merge (A-20).

## Filesystems

### What is excluded

The list is the mounts of the host's `/proc/1/mountinfo` less a fixed
list, which is not configurable:

- Pseudo and virtual types: `proc`, `sysfs`, `devtmpfs`, `devpts`,
  `cgroup`, `cgroup2`, `securityfs`, `debugfs`, `tracefs`, `bpf`,
  `pstore`, `mqueue`, `hugetlbfs`, `configfs`, `fusectl`, `autofs`,
  `binfmt_misc`, `nsfs`, `efivarfs`, `rpc_pipefs`, `nfsd`, `selinuxfs`,
  `ramfs`, `fuse.lxcfs`, `fuse.gvfsd-fuse` and `fuse.portal`.
- `overlay` mounts under the container runtimes' directories:
  `/var/lib/docker/`, `/var/lib/containerd/`, `/run/containerd/`,
  `/var/lib/containers/` and `/run/containers/`.
- `squashfs` mounts under `/snap/` (snap packages).
- `tmpfs`, except at `/tmp`, `/run` and `/dev/shm`.
- A bind mount of a single file, which is not a filesystem of its own.

### One item per filesystem

Mounts are grouped by device number (`maj:min`), and each group gives one
item. Bind mounts of a listed filesystem, and the subvolumes of one btrfs
filesystem, are therefore listed once. The item is the preferred mount
point: one that mounts the filesystem's root first, then the shortest mount
point, then the first by name. A btrfs filesystem's numbers are the same
for every subvolume, so the one item shows them once.

### Read-only flags and hidden mounts

Under the systemd unit, `ProtectSystem=strict` gives `nexusd` its own
mount namespace in which every mount is read-only, so `readOnly` comes from
the host's view in `/proc/1/mountinfo`, never from `nexusd`'s own. When
`/proc/1/mountinfo` cannot be read, the list falls back to
`/proc/self/mountinfo` and every `readOnly` is null with
`permission denied: /proc/1/mountinfo (read-only flags in nexusd's own namespace would be wrong)`
rather than wrong.

`statfs` runs on a mount point only when `/proc/self/mountinfo` shows that
a lookup of that path in `nexusd`'s namespace reaches the same filesystem.
A host mount below `/tmp` or `/var/tmp`, which `PrivateTmp=yes` hides, or
one over-mounted at its path or an ancestor's, is listed with its usage
null and `not visible:`, and no syscall is made on it: `statfs` there would
measure another filesystem.

## Virtualization

`virtualization` is detected from files only; `systemd-detect-virt` is
never run. `type` uses `systemd-detect-virt`'s words, `kind` is `vm`,
`container` or `none`, and `evidence` names the marker used. Containers are
checked first, because a container on a VM sees the VM's DMI:

| Marker | `type` | `kind` |
| --- | --- | --- |
| `/.dockerenv` exists | `docker` | `container` |
| `/run/.containerenv` exists | `podman` | `container` |
| `/run/systemd/container` holds a name systemd knows (`lxc`, `lxc-libvirt`, `systemd-nspawn`, `docker`, `podman`, `rkt`, `wsl`, `proot`, `pouch`) | that name | `container` |
| `/run/systemd/container` holds another value | `container-other` | `container` |
| `fuse.lxcfs` mounted over a `/proc` file | `lxc` | `container` |
| `/proc/vz` without `/proc/bc` | `openvz` | `container` |
| The kernel release contains `microsoft` | `wsl` | `container` |
| `/sys/hypervisor/type` is `xen` | `xen` | `vm` |
| DMI `sys_vendor` `Microsoft Corporation` and `product_name` `Virtual Machine` | `microsoft` | `vm` |
| DMI `product_name`, then `sys_vendor`, starting with `KVM` or `OpenStack` | `kvm` | `vm` |
| ... with `QEMU` | `qemu` | `vm` |
| ... with `VMware` | `vmware` | `vm` |
| ... with `innotek GmbH` or `VirtualBox` | `oracle` | `vm` |
| ... with `Xen` | `xen` | `vm` |
| ... with `Amazon EC2` | `amazon` | `vm` |
| ... with `Google Compute Engine` | `google` | `vm` |
| ... with `Parallels` | `parallels` | `vm` |
| ... with `BHYVE` | `bhyve` | `vm` |
| ... with `Apple Virtualization` | `apple` | `vm` |
| The `hypervisor` CPU flag in `/proc/cpuinfo` | `unknown` | `vm` |
| None of these | `none` | `none` |

The first row that matches wins. The DMI strings are those of systemd's
own table (`src/basic/virt.c`), matched as prefixes, so a Chromebook
(vendor `Google`) or an Oracle server (vendor `Oracle Corporation`) is not a
VM. systemd also lists `Oracle Corporation`, for VirtualBox's board vendor;
the provider does not read the board vendor and leaves that row out, so an
Oracle bare-metal server is `none` here where `systemd-detect-virt` would
say `oracle`. A hypervisor not in the table is `none` unless the CPU flag
is set.
When `nexusd` runs in a container, `/proc` and `/sys` describe what the
container sees, and the provider reports that as it is.

## Reboot required

`rebootRequired` uses the first rule that applies to the host:

1. **Debian and Ubuntu**, in a container or not: the apt provider's rule.
   `/var/run/reboot-required` present is `true`, with `packages` from
   `/var/run/reboot-required.pkgs`. Absent, it is `false` when a program
   that writes the marker is installed
   (`/usr/share/update-notifier/notify-reboot-required` or needrestart's
   Ubuntu hook), and `unknown` when none is, since nothing would ever
   create it. `check` is `debian-reboot-required-marker`.
2. **Alpine in a container**: `unknown`, `not supported:`, because the
   running kernel is the container host's. `check` is `none`.
3. **Alpine on a VM or bare metal**: Alpine's kernel package removes the
   old kernel's modules on upgrade, so when `/lib/modules/<running release>`
   is missing a newer kernel is installed and not yet booted: `true`.
   When it exists: `false`. `check` is `alpine-running-kernel-modules`.
4. **Any other OS family**: `unknown`, `not supported:`. `check` is `none`.

`evidence` says what the check found.

## Clock synchronization

`clock` never comes from a clock system call: under the unit's system call
filter one would kill `nexusd`. It comes from one fixed read-only command:

- **systemd:** `/usr/bin/timedatectl show --property=NTPSynchronized --value`,
  `yes` or `no`. `source` names the time-sync client whose marker exists:
  `systemd-timesyncd` (`/run/systemd/timesync`), `chrony`
  (`/run/chrony/chronyd.pid`) or `ntpd` (`/run/ntpd.pid`), checked in that
  order, else `kernel`.
- **Other init systems, with `/usr/bin/chronyc` installed:**
  `/usr/bin/chronyc -c tracking`, whose leap status `Normal`,
  `Insert second` or `Delete second` is synchronized and
  `Not synchronised` is not. `source` is `chrony`.
- **Otherwise:** `synchronized` is `unknown`, and `source` is null, both
  `not supported:`.

A command that fails leaves `synchronized` `unknown` with the reason. The
command runs for at most `sourceTimeout`.

## Unavailable under the systemd sandbox

Observed under the unchanged hardened unit (`deploy/nexusd.service`, no
drop-in) on Ubuntu 26.04.1 with systemd 259: **no field is unavailable
because of the sandbox.** No field was reported with `permission denied:`
or `command failed:`, the two reasons a sandbox gives. The fields that were
unavailable there were all `not reported:`, and are the same without the
unit: no CPU model or frequency, no temperature sensors, no disk model, no
link speed on a virtual interface, and no IPv6 address. These observations
come from the T1 harness (WI 5.3, [verification/host-t1.md](verification/host-t1.md),
TC-HP-115 and TC-HP-131) and from the container run of
`test/live/host-check.sh` ([verification/host-live.md](verification/host-live.md)).
Both are containers; the run on a real Ubuntu host is still pending. If it
shows a field unavailable because of the sandbox, that field is added to
the table below with its reason and cause.

`CAP_DAC_READ_SEARCH` lets `nexusd` read root-only files, so the unit
removes nothing the allow-list permits. On OpenRC `nexusd` runs as
unconfined root, so nothing is lost to a sandbox there either; the
allow-list is the only limit.

The unit does change what some fields mean. Each of these was observed
readable:

| Field | Observed under the unit | Cause |
| --- | --- | --- |
| Filesystems below `/tmp` or `/var/tmp` (`host.filesystems.list`, and their usage in the summary) | Listed, with usage null and `not visible:` (TC-HP-117, a mount below `/tmp`). This is by design, not a denial | `PrivateTmp=yes` hides them in `nexusd`'s namespace, and the provider makes no `statfs` there |
| `identity.hostname` | Readable, equal to the kernel hostname; the name at `nexusd`'s start | `ProtectHostname=yes`: a private UTS namespace |
| `readOnly` (`host.filesystems.list`) | Readable for every item, from PID 1's mount table (TC-HP-117) | `/proc/1/mountinfo` is PID 1's; were it denied, `readOnly` would be null with `permission denied: /proc/1/mountinfo ...` |
| `clock.synchronized` | Readable, equal to `timedatectl`'s `NTPSynchronized` (TC-HP-E29), with no system call killed | `ProtectClock=yes` and the system call filter rule out a clock system call, so only the `timedatectl` command is used; were it to fail, `unknown` with `command failed:` |
| `topByCpu`, `topByMemory` | Readable for every user's processes, `nobody` included (TC-HP-E28) | `CAP_DAC_READ_SEARCH`; a `/proc` mounted with `hidepid` would give `permission denied:` |
| `failedServices` | Readable, equal to `systemctl --failed` (TC-HP-118) | `systemctl list-units` reaches systemd over D-Bus, as `nexusd`'s other `systemctl` queries do |

## What is never returned

The provider never returns, and never reads:

- process command lines or environments (`/proc/<pid>/cmdline` and
  `/proc/<pid>/environ` are not on the allow-list);
- DMI serial numbers, UUIDs or asset tags;
- the kernel log;
- the ARP or neighbour tables, so no other host's MAC address;
- file contents beyond the fixed system files it parses. Reasons name a
  path, a command or a mount point, never what a file or command printed.

It takes no path, device name or command as input. The only commands it
runs are `timedatectl show`, `chronyc -c tracking`,
`systemctl list-units --state=failed` and `rc-status --crashed`, each with
a fixed argument list and bounded by `sourceTimeout`.
