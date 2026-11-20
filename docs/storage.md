# Storage, backups and Time Machine

The `storage` provider tells you whether a host's data is healthy and backed
up: the state of its ZFS pools, scrubs, datasets and snapshots; the Proxmox
`vzdump` archives that land in a directory on the host; and the usage and
last backup of its Time Machine shares. It raises storage health signals,
which also appear in `get_host_health`. It is read-mostly: its only writes
are starting or stopping a scrub and creating a snapshot, each disabled by
default, allowed only on pools and datasets you list, and audited. It never
destroys, rolls back, clones or sends anything.

This page covers the configuration, the three features and why each can be
unavailable, each operation, the health signals and their thresholds, the
guarded writes and how to enable them, the `vzdump` layout, the Time Machine
reading rules, the LXC guests, and how the provider lives beside
`zfs-auto-snapshot`. [security.md](security.md#what-the-storage-provider-reads-and-writes)
explains what the provider reads and writes, and the generated
[CLI reference](cli/nexus.md) and [API reference](api.md) list every flag
and field.

## Naming

The provider is named `storage`, its operations are `storage.*` and its
configuration section is `providers.storage`. Its three scopes are
`storage:read` (every read), `storage:scrub` (scrub start and stop) and
`storage:snapshot` (snapshot create); no operation needs more than one.

`internal/storage` is unrelated: it is `nexusd`'s own accountant for its
state, audit and operation logs (`limits.storageBudgetBytes`). The provider
lives in `internal/providers/storage` and `internal/providers/storage/spec`,
and code that imports both packages names them with an explicit alias.

## Configuration

```yaml
providers:
  storage:
    enabled: false               # Off until enabled here
    commandTimeout: 30s          # 5s..120s: bound on one zpool or zfs call
    backups:                     # Proxmox vzdump archives
      vzdumpPaths: []            # Absolute directories holding Proxmox vzdump archives (the path and its dump/)
      maxAge: 192h               # 1h..8760h: a guest's latest archive older than this is stale (8 days)
      expectedGuests: []         # vmids (100..999999999) that must have archives; missing ones are stale
    timeMachine:                 # Time Machine shares
      paths: []                  # Absolute Time Machine share directories
      maxAge: 168h               # 1h..8760h: a bundle whose last backup is older than this is stale (7 days)
      sizeInterval: 1h           # 10m..24h: how often bundle sizes are recomputed in the background
    thresholds:                  # When health signals are raised
      poolCapacityWarn: 80       # percent; warn < crit, each 1..100
      poolCapacityCrit: 90       # percent; 1..100
      scrubMaxAgeDays: 35        # 1..366: a pool not scrubbed for this many days is overdue
      quotaWarn: 85              # percent of a dataset's quota
      shareWarn: 85              # percent of a Time Machine share; warn < crit, each 1..100
      shareCrit: 95              # percent of a Time Machine share; 1..100
    scrub:                       # Scrub start and stop (storage:scrub)
      enabled: false             # Scrub start and stop act only when true
      pools: []                  # Pool names scrub start and stop may act on
    snapshots:                   # Snapshot create (storage:snapshot)
      enabled: false             # Snapshot create acts only when true
      datasets: []               # Each: name (required), recursive (default false)
      maxPoolCapacityPercent: 90 # 1..100: refuse a snapshot when the pool is this full
```

Every key below is under `providers.storage`.

| Key | Default | Range | Meaning |
| --- | --- | --- | --- |
| `enabled` | `false` | | Whether the provider serves requests and runs its background size walk |
| `commandTimeout` | `30s` | `5s` to `120s` | The bound on one `zpool` or `zfs` call, and on each filesystem access of a configured path |
| `backups.vzdumpPaths` | `[]` | absolute paths | The directories holding `vzdump` archives; each is read with its `dump/` |
| `backups.maxAge` | `192h` | `1h` to `8760h` | A guest whose latest completed archive is older than this is stale (8 days) |
| `backups.expectedGuests` | `[]` | vmids `100` to `999999999` | Guests that must have archives; one with none is listed as stale |
| `timeMachine.paths` | `[]` | absolute paths | The Time Machine share directories |
| `timeMachine.maxAge` | `168h` | `1h` to `8760h` | A bundle whose last backup is older than this is stale (7 days) |
| `timeMachine.sizeInterval` | `1h` | `10m` to `24h` | How often bundle sizes are recomputed in the background |
| `thresholds.poolCapacityWarn` | `80` | `1` to `100`, below `poolCapacityCrit` | Percent full at which `pool-capacity` is a warning |
| `thresholds.poolCapacityCrit` | `90` | `1` to `100` | Percent full at which `pool-capacity` is critical |
| `thresholds.scrubMaxAgeDays` | `35` | `1` to `366` | Days after the last finished scrub at which `scrub-overdue` is raised |
| `thresholds.quotaWarn` | `85` | `1` to `100` | Percent of a dataset's quota (or refquota) at which `dataset-quota` is raised |
| `thresholds.shareWarn` | `85` | `1` to `100`, below `shareCrit` | Percent full at which `timemachine-share-full` is a warning |
| `thresholds.shareCrit` | `95` | `1` to `100` | Percent full at which `timemachine-share-full` is critical |
| `scrub.enabled` | `false` | | Scrub start and stop act only when true |
| `scrub.pools` | `[]` | pool names | The pools scrub start and stop may act on |
| `snapshots.enabled` | `false` | | Snapshot create acts only when true |
| `snapshots.datasets` | `[]` | `{name, recursive}` entries | The datasets a snapshot may be taken of; `recursive` (default `false`) allows a recursive snapshot of that dataset |
| `snapshots.maxPoolCapacityPercent` | `90` | `1` to `100` | A snapshot is refused when the dataset's pool is at least this full |

So by default the provider is off, both kinds of write are off, and every
list (directories, shares, expected guests, pools and datasets) is empty.

Configured paths must be absolute and clean, must not be `/`, and are listed
once. `nexusd validate-config` refuses a value outside its range or a list
entry that breaks its rule, naming the key (`providers.storage.backups.maxAge`
and so on). The lists are not remotely settable: they change only by editing
`/etc/nexus/nexusd.yaml` on the host (see
[security.md](security.md#lists-and-mappings-are-not-settable)).

For home-server, whose Proxmox node writes its backups to `/media/vzdump` and
whose Macs back up to two Samba shares, the reads are enabled with:

```yaml
providers:
  storage:
    enabled: true
    backups:
      vzdumpPaths: ["/media/vzdump"]
      expectedGuests: [101, 103]   # the vmids whose backups must exist
    timeMachine:
      paths: ["/media/timemachine", "/media/timemachine2"]
```

Check the file with `sudo nexusd validate-config`, then apply it with a
reload (`sudo systemctl reload nexusd`). A reload applies a changed section
without a restart; an operation already running keeps the settings it
started with.

## Features and why they are unavailable

The provider has three features, each available or not on its own:

- **`zfs`**: `zpool` and `zfs` exist as executable regular files at
  `/usr/sbin`, else `/sbin`; `/dev/zfs` exists and is a character device;
  and `zpool list -H -o name` succeeds inside the sandbox. A host with no
  pool imported has `zfs` available with no pools; that is a state, not an
  error. `zfs version` then decides the output format: OpenZFS 2.3 or later
  is read as JSON (`-j --json-int`), older releases as the parsable text form.
  Snapshot listings always use the text form, which is smaller per row.
- **`backups`**: at least one `backups.vzdumpPaths` entry opens and is a
  directory.
- **`timeMachine`**: at least one `timeMachine.paths` entry opens and is a
  directory.

The provider is available when any feature is. A call whose feature is
unavailable answers 503 `capability-unavailable` with that feature's reason,
never an empty list that could read as "no pools" or "no backups". An
unavailable feature is checked again when a call or the health read needs it
and its last check is more than 5 seconds old, so a feature that comes back
(a pool imported, a share mounted) is picked up without a reload.
Availability changes are logged at info, once each, never per request.

These are the reasons, as the provider words them:

| Reason | Feature | Why |
| --- | --- | --- |
| `storage provider requires Linux` | all | The host is not Linux |
| `Proxmox VE node: the storage provider never runs on the hypervisor` | all | `/etc/pve` or `/var/lib/dpkg/info/pve-manager.list` exists |
| `the Proxmox VE marker <path> cannot be checked: <error>` | all | A marker could not be checked, so the host is refused rather than guessed about |
| `zfs: /usr/sbin/zpool not found (nor /sbin/zpool)` | `zfs` | The tool is not installed; the same for `zfs` |
| `zfs: <path> is a directory`, `is not a regular file`, `is not executable (mode <mode>)` or `cannot be checked: <error>` | `zfs` | A tool exists but cannot be used; every path tried is named |
| `zfs: /dev/zfs not found (the zfs kernel module is not loaded)` | `zfs` | The tools exist but the device does not |
| `zfs: /dev/zfs is not a character device (mode <mode>)` | `zfs` | |
| `zfs: zpool list exited <n>: <first stderr line>` | `zfs` | The first line is cut at 200 bytes |
| `zfs: zpool list did not finish within <bound>` | `zfs` | The bound is `commandTimeout`, or the startup check's own shorter one |
| `not configured` | `backups`, `timeMachine` | The list of paths is empty |
| `<path>: <error>`, joined with `; ` | `backups`, `timeMachine` | No configured path could be opened as a directory; each path's error, such as `no such file or directory` or `not a directory` |
| `not checked yet` | any | The feature has not been checked since `nexusd` started |

When no feature is available, the provider's own reason joins the three, for
example `zfs: /usr/sbin/zpool not found (nor /sbin/zpool); backups: not
configured; timeMachine: not configured`. When the host itself is refused
(not Linux, or a Proxmox VE node), that one reason is the provider's.

A `zfs version` that fails or cannot be parsed leaves `zfs` available, read
in the text form, with `zfsVersionReason` saying why.

`nexus capability get` shows these details for the `storage` provider:
`features` (each feature's `available` and `reason`), `zfsVersion`
(`userland`, `kernel` and `format`, `json` or `text`) or `zfsVersionReason`,
`pools` (the imported pool names), `zpoolPath` and `zfsPath`,
`scrubControl` (`enabled` and each listed pool with `ok` or
`not-imported`) and `snapshotControl` (`enabled`,
`maxPoolCapacityPercent`, and each listed dataset with its `recursive` and
`ok`, `not-found` or `unknown`).

## Operations

| Operation | Scope | CLI | REST | MCP tool |
| --- | --- | --- | --- | --- |
| `storage.health.get` | `storage:read` | `nexus storagehealth get` | `GET /v1/providers/storage/health` | `get_storage_health` |
| `storage.pools.list` | `storage:read` | `nexus pool get` | `GET /v1/providers/storage/pools` | `list_zfs_pools` |
| `storage.pools.get` | `storage:read` | `nexus pool get NAME` | `GET /v1/providers/storage/pools/{name}` | |
| `storage.datasets.list` | `storage:read` | `nexus dataset get` | `GET /v1/providers/storage/datasets` | `list_zfs_datasets` |
| `storage.datasets.get` | `storage:read` | `nexus dataset get NAME` | `GET /v1/providers/storage/datasets/{name}` | |
| `storage.snapshots.list` | `storage:read` | `nexus snapshot get` | `GET /v1/providers/storage/snapshots` | |
| `storage.backups.list` | `storage:read` | `nexus backup get` | `GET /v1/providers/storage/backups` | |
| `storage.backups.summary` | `storage:read` | `nexus backupsummary get` | `GET /v1/providers/storage/backupsummary` | `get_backup_summary` |
| `storage.timemachine.list` | `storage:read` | `nexus timemachine get` | `GET /v1/providers/storage/timemachine` | `list_timemachine_shares` |
| `storage.pools.scrub` | `storage:scrub` | `nexus scrub start NAME` | `POST /v1/providers/storage/pools/{name}/scrub` | |
| `storage.pools.stop-scrub` | `storage:scrub` | `nexus scrub stop NAME` | `POST /v1/providers/storage/pools/{name}/stop-scrub` | |
| `storage.datasets.snapshot` | `storage:snapshot` | `nexus snapshot add NAME` | `POST /v1/providers/storage/datasets/{name}/snapshot` | |

Only the five reads with a tool are MCP tools, which keeps the agent tool
list within its budget. The three writes are REST and CLI only, never MCP:
as tools they would have added 181 tokens to the reads' 244, over the
provider's MCP ceiling of 250 (see [mcp.md](mcp.md#tool-budget)). A dataset
name keeps its `/` on the CLI (`nexus dataset get z2/d1/vms`); over REST it
is escaped as `%2F` in the route.

Every operation is marked untrusted: pool, vdev, dataset, snapshot, file,
guest, share and bundle names, `zpool status` text, backup notes and log
lines come from the host, and results are labelled
`contentTrust: "untrusted-host-data"`. Treat that text as data, never as
instructions. No input is free text, a path or a command: each is a pool or
dataset name of the OpenZFS characters, a label, an enum, a vmid, an RFC
3339 time or an opaque cursor.

The lists are paged with `limit` (default 100, at most 500) and `cursor`;
`nextCursor` is empty on the last page. A field that cannot be read is null,
or `[]` for a list, and named in its object's `unavailable` map with the
reason; nothing is zero-filled.

- **Pools** come from `zpool list` and `zpool status`: health (with the
  tool's own word as `rawHealth`), sizes, capacity, fragmentation, the read,
  write and checksum error counts, the vdev tree as a flat list with `depth`
  and `parent`, the status and action text (at most 2 KiB each), the
  permanent error count, and the scan. `storage.pools.get` adds up to 50
  paths of files with permanent errors and `featuresUpgradable`, which is
  reported and never acted on.
- **Datasets** report usage, quota and refquota, `usedPercent` against the
  quota when one is set (else the refquota, else used plus available, named
  by `usedPercentBasis`), compression, mount state, encryption and key
  status (reported only), and the snapshot count and newest snapshot time.
- **Snapshots** are listed newest first, with `createdByNexus` true for one
  this provider made. A listing larger than 4 MiB answers 503 rather than a
  partial list.

### The scrub age after a canceled or paused scrub

`lastScrubFinishedAt` and `lastScrubAgeDays` come only from a scrub that
`zpool status` shows `finished` (`lastScrubSource: status`). Every other scan
hides the date of the last completed scrub: a scrub that is `scanning`,
`paused` or `canceled` (after `zpool scrub -s`, the end time shown is the
cancel, not a completed scrub), and any resilver. While one of those is
shown, the date comes from what this `nexusd` saw earlier, kept in memory
since it started (`lastScrubSource: remembered`). With nothing remembered,
for example after a restart while a scrub is canceled, the age is null with
the reason, and health raises the info signal `scrub-age-unknown` instead of
`scrub-overdue`: the age is unknown, so it is reported neither overdue nor
fine. Only a pool whose scan is `none` has never been scrubbed. The memo is
not persisted; the next finished scrub sets the date again.

## Health signals

`storage.health.get` (`get_storage_health`) returns one item, `current`:
the signals sorted critical, warning, info, then by id and subject, at most
500 (`signalsTruncated` beyond); `counts` counts every signal found;
`checked` lists the features that were read; and `unavailable` names each
feature, or part of one, that could not be read, with the reason. A feature
that could not be read contributes no signal, so nothing unread is reported
healthy.

| Signal | Severity | Subject | Raised when |
| --- | --- | --- | --- |
| `pool-not-online` | critical | `pool:<name>` | The pool's health is not `online` |
| `pool-errors` | critical | `pool:<name>` | Any read, write or checksum error count, or the permanent error count, is above 0 |
| `pool-capacity` | warning, critical | `pool:<name>` | Capacity is at least `thresholds.poolCapacityWarn` (80%), critical at `thresholds.poolCapacityCrit` (90%) |
| `scrub-overdue` | warning | `pool:<name>` | The last finished scrub is more than `thresholds.scrubMaxAgeDays` (35) days old, or the pool has never been scrubbed |
| `scrub-age-unknown` | info | `pool:<name>` | The scan shown hides the last finished scrub and none was seen since `nexusd` started |
| `scrub-errors` | critical | `pool:<name>` | The last finished scrub repaired bytes or found errors |
| `dataset-quota` | warning | `dataset:<name>` | Use is at least `thresholds.quotaWarn` (85%) of the dataset's quota or refquota |
| `backup-stale` | warning | `guest:<type>/<vmid>` or `guest:<vmid>` | The guest's latest completed archive is older than `backups.maxAge` (192 h), it has only archives still being written, or it is an expected guest with no archive |
| `backup-failed` | critical | `guest:<type>/<vmid>` | The log of the guest's latest completed archive reports an error |
| `timemachine-stale` | warning | `bundle:<path>/<name>` | The bundle's last backup is older than `timeMachine.maxAge` (168 h), or it has no date at all |
| `timemachine-share-full` | warning, critical | `share:<path>` | The share is at least `thresholds.shareWarn` (85%) full, critical at `thresholds.shareCrit` (95%) |
| `zfs-unresponsive` | critical | `zfs` or `pool:<name>` | A ZFS health read did not answer: it timed out, was still pending from an earlier call, found no free process slot, could not start, or exited non-zero |

The subjects take these forms:

- `pool:<name>` and `dataset:<name>`;
- `guest:<type>/<vmid>`, such as `guest:lxc/103`, for a guest that has
  archives. An expected guest with no archive at all is `guest:<vmid>`, such
  as `guest:999`, because without an archive nothing names its type;
- `share:<path>` and `bundle:<path>/<name>`, such as
  `bundle:/media/timemachine/mac-1`;
- `zfs` for a `zfs-unresponsive` read whose command names no pool, which is
  every health read today, else `pool:<name>`.

A parse failure or an output over its cap is only a reason in `unavailable`,
not `zfs-unresponsive`. Names in subjects and messages are host data.

### In `get_host_health`

While the provider is enabled, `host.health.get` (`get_host_health`) shows
its counts and first 10 signals as a `providerSignals` block with
`source: "storage"` (see [host.md](host.md#hosthealthget)). The block is
collected beside the host's own reads and waits at most the host provider's
`sourceTimeout` (default 2 s). A storage read takes longer than that on a
busy pool, so a block can be **stale**: when the current read misses the
deadline or fails, the block is the last completed storage report, if it is
at most 5 minutes old, with its own `observedAt`, `stale: true` and the
reason in `unavailable.signals`. The read that missed the deadline keeps
running and its report serves the next call. With no report from the last 5
minutes, the block has `counts: null` and the reason; a provider that is
enabled but unavailable gives `counts: null` and
`capability-unavailable: <reason>`. A disabled provider gives no block. For
the current signals, ask `get_storage_health`.

## Guarded writes

The three writes act only when both their switch and their allow-list say
so. They are reachable only through REST and the `nexus` CLI, never MCP.

| Write | Switch | Allow-list | Scope |
| --- | --- | --- | --- |
| Scrub start, scrub stop | `providers.storage.scrub.enabled` | `providers.storage.scrub.pools` | `storage:scrub` |
| Snapshot create | `providers.storage.snapshots.enabled` | `providers.storage.snapshots.datasets` | `storage:snapshot` |

To enable them, edit `/etc/nexus/nexusd.yaml` on the host, for example:

```yaml
providers:
  storage:
    scrub:
      enabled: true
      pools: [z1]
    snapshots:
      enabled: true
      datasets:
        - name: z1/ds/appdata
        - name: z2/d1/vms
          recursive: true
```

then `sudo nexusd validate-config` and `sudo systemctl reload nexusd`, and
mint a token with the scope, for example
`sudo nexusd token create --name laptop --scope storage:read --scope storage:scrub`.
`nexus capability get` then shows each allow-listed pool or dataset with
`ok`, or `not-imported` and `not-found` for a name the host does not have.
The lists change only by editing the file. A token with `nexusd:admin` can
flip `scrub.enabled` or `snapshots.enabled` remotely, as it can any scalar
key, but never add a pool or a dataset.

A write that the configuration does not allow is refused with 409
`not-allowed`, whose `configKey` names the key, such as
`providers.storage.scrub.enabled`, `providers.storage.scrub.pools` or
`providers.storage.snapshots.datasets[0].recursive`. It is refused before
any operation exists, and nothing is sent to ZFS. The configuration is
checked again when the operation starts, so a reload in between is
honoured.

Each write is long-running: it answers 202 with an operation, runs detached
from `nexusd` and survives a `nexusd` restart. Writes on the same pool (scrub
start and stop) or the same dataset (snapshots) run one at a time; another
answers 409 `conflict`. Each accepts an idempotency key. A failed command
fails the operation with `zfs-command-failed`, quoting up to 2 KiB of the
tool's output. Each is audited `requested` with its facts (`pool` and
`force`; `dataset`, `snapshot` and `recursive`) and then its end, or
`refused` at admission.

### Scrub start and stop

`nexus scrub start z1` runs `zpool scrub z1`, or resumes a paused scrub
(`resumed: true`). It succeeds once `zpool status` shows the scrub scanning,
within 60 seconds, and returns the pool's scan; it does not wait for the
scrub to finish, which takes hours and loads every disk of the pool. A scrub
already running reports `changed: false`. Start is refused with 409
`scan-in-progress` while a scrub or resilver runs, and with 409
`pool-unhealthy` unless the pool is `online`.

`force` (`--force`) overrides one health only: a `degraded` pool may be
scrubbed with it. A `suspended`, `faulted`, `unavail`, `removed`, `offline`
or `unknown` pool is refused whatever `force` says, because `zpool scrub`
cannot run there or can block in the kernel. `force` is recorded in the
audit.

`nexus scrub stop z1` runs `zpool scrub -s z1` and succeeds once the scrub
shows canceled, within 60 seconds. A pool with no scrub reports
`changed: false` and nothing is sent. A resilver is never canceled. A later
scrub starts from the beginning, and the scrub age is then reported as the
rule above describes.

### Snapshot create

`nexus snapshot add z1/ds/appdata --label before-upgrade` creates
`z1/ds/appdata@nexus-20261119T093000Z-before-upgrade`. The provider names
every snapshot: `nexus-<UTC YYYYMMDDTHHMMSSZ>`, with `-<label>` appended when
a label (lowercase letters, digits and `-`, at most 32 characters) is given;
the caller never names it. The snapshot carries the user property
`nexus:created-by` set to the operation ID, which the operation record and
the audit log tie to the token, and `storage.snapshots.list` reports it as
`createdByNexus: true`. `--recursive` also snapshots the descendants, only
when the dataset's allow-list entry has `recursive: true`.

A snapshot is refused with 409 `pool-low-space` when the pool is at or above
`snapshots.maxPoolCapacityPercent` full, and with 400 `validation-failed`
when the full name would exceed 255 bytes. Two snapshots of one dataset with
the same label in the same second collide, and the second fails. Nexus never
removes a snapshot: remove one by hand with `zfs destroy` when you no longer
need it. A snapshot holds the blocks it references, so snapshots fill a pool
over time.

### When a write is stopped

A running write cannot be canceled through Nexus (`nexus operation cancel`
answers `not-cancelable` once it runs); stop a scrub with scrub stop. A
write is stopped only by its supervisor, and the operation then says what
reached ZFS:

- **At its timeout, before the command was sent:** it fails
  `dependency-unavailable` with "the storage helper was stopped before
  `<command>` was sent; nothing was sent".
- **At its timeout, after the command was sent:** it fails
  `outcome-unknown`, naming the command and the check to run by hand
  (`zpool status <pool>` or `zfs list -t snapshot <dataset>`), never a
  success. The write may or may not have happened.
- **A host shutdown, or a `SIGTERM` Nexus did not send:** it fails
  `outcome-unknown`, "the storage helper ... stopped without a result",
  with the same check, whether or not the command was sent.
- **An operator's `SIGINT` on the host**
  (`sudo systemctl kill --kill-whom=main --signal=SIGINT nexus-op-<id>.service`)
  ends the operation `Canceled`, even when the command was already sent:
  `Canceled` here does not mean nothing changed. Check the pool or dataset
  by hand.

The supervisor's timeout is twice `commandTimeout` plus 90 seconds: 150
seconds by default.

### Why the writes run outside the sandbox

`zpool scrub` and `zfs snapshot` need `CAP_SYS_ADMIN`, which the hardened
`nexusd` unit does not have. Each write therefore runs the `nexusd` binary's
hidden `storage-helper` subcommand in its own `nexus-op-<id>` unit (or under
`op-supervise` on OpenRC, where writes are not expected to be enabled),
outside the sandbox, as the Pi-hole and apt changes do. The unit was not
widened. The helper re-checks everything it reads from the operation's
directory, finds the tools itself and runs only fixed argument lists; see
[security.md](security.md#what-the-storage-provider-reads-and-writes).

## Proxmox vzdump backups

The provider reads only the files the Proxmox node has already written to a
directory the host can see; it never calls Proxmox, `vzdump`, `pvesm`, `qm`
or `pct`. For each `backups.vzdumpPaths` entry it lists the path and its
`dump/` subdirectory, the layout of a Proxmox directory or CIFS storage, and
nothing deeper. Symlinks out of the configured path are never followed. A
listing stops at 10,000 entries per directory (`truncated`).

A file is recognized by the `vzdump` name grammar:

```
vzdump-<qemu|lxc>-<vmid>-<YYYY_MM_DD-HH_MM_SS>.<vma|tar>[.<zst|gz|lzo>]
```

where `<vmid>` is 100 to 999999999. Its companions share its stem:

- `<stem>.log`: the backup log;
- `<archive>.notes` (or `<stem>.notes`): the notes, whose first line is
  usually the guest's name;
- `<archive>.protected`: present when the archive is protected.

An archive still being written is `<stem>.vma.dat`, `<stem>.tar.dat` or
`<stem>.dat`, or a `<stem>.tmp` directory; it is listed with
`inProgress: true`. Anything else is counted as unrecognized in
`storage.backups.summary`'s `directories`.

The timestamp in the name is the Proxmox node's local time and is reported
as written, without a zone (`timestampZone: node-local`). Staleness, ages
and `latestAt` use the file's modification time instead, which is UTC.

Each archive's `logStatus` is `ok` when the log's last `INFO: Finished
Backup of VM` line follows its last `ERROR:` line, `error` when an `ERROR:`
line follows it or there are errors and no finish line, and `unknown`
otherwise or with no log. Only the last 256 KiB of a log are read, and at
most 10 `ERROR:` lines of at most 512 bytes each are returned. Only the first
1 KiB of a notes file is read (`notesTruncated` beyond).

### The summary per guest

`storage.backups.summary` (`get_backup_summary`) groups the archives per
guest (guest type and vmid): the latest archive's file, time, size and log
status, its age in hours, the archive count and total bytes, the guest name
from the latest notes' first line, and `stale` when the age exceeds
`backups.maxAge`. `directories` gives each configured directory's status and
counts. Guests are capped at 500 (`guestsTruncated`); `guestCount`,
`staleCount` and `failedCount` count every guest.

An archive still being written counts in its guest's `count` and
`totalBytes`, but is never the guest's latest: `latestAt`,
`latestSizeBytes`, `latestLogStatus`, `ageHours`, `guestName` and `stale`
come from the newest archive that is not in progress. A partial or
abandoned archive (a `.tmp` directory left by a backup that never finished)
is not a completed backup, and taking it as the latest would hide a stale
guest. A guest whose only archives are in progress has `latestAt: null` and
is stale.

### Making a missing backup job visible

A guest whose backup job was removed or never ran has no archive, so nothing
in the directory mentions it. List the vmids that must be backed up in
`backups.expectedGuests`: each one with no archive at all is listed with
`expected: true`, an empty `guestType`, `latestAt: null` and `stale: true`,
and raises `backup-stale` with the subject `guest:<vmid>`. An expected guest
that has archives is reported like any other, with `expected: true`.

### Paging the archive list

`storage.backups.list` is paged by bytes as well as by count. Notes and log
lines are host text whose size no item count bounds, so a page also ends
early, before `limit` items, when the next item would take the page's items
past 896 KiB of JSON. Such a page still returns a `nextCursor`, so keep
paging while `nextCursor` is set, rather than until a page holds fewer than
`limit` items. A page always holds at least one item.

## Time Machine shares

For each `timeMachine.paths` entry, `storage.timemachine.list`
(`list_timemachine_shares`) reports the share's size, used and available
bytes and used percent from `statfs`; the ZFS dataset mounted there, matched
by its mountpoint, with its quota and quota-based used percent; and each
bundle, at most 256 per share (`bundlesTruncated`).

A bundle is a `<name>.sparsebundle` or `<name>.backupbundle` directory
directly under the share. Its `name`, without the suffix, is usually the
Mac's computer name: host data, which appears in examples here as `mac-1`,
`mac-2` and so on. For each bundle:

- `lastBackupAt` is the latest `com.apple.backupd.SnapshotCompletionDate` in
  the bundle's `com.apple.TimeMachine.SnapshotHistory.plist`, read only when
  it is an XML plist of at most 1 MiB. A binary plist is not parsed
  (`binary plist not parsed`).
- Without that date, `lastModifiedAt` is the newest modification time of
  that plist, `com.apple.TimeMachine.Results.plist` and `Info.plist`, and
  `lastBackupSource` is `mtime-best-effort`.
- `stale` is true when the date used is older than `timeMachine.maxAge`, or
  when there is no date at all.
- `sizeBytes` is the allocated size of the files under `bands/`, from a
  background walk (below).
- `inProgress` is always null, with the reason `not determinable from the
  share`. A sparsebundle keeps its zero-byte `lock` file whether or not a
  backup runs, so nothing on the share marks a backup in progress.

What is read: the share's directory listing, the bundle directories' names,
the `SnapshotHistory.plist` contents, the modification times of the three
plists, and the names and allocated blocks of the files under `bands/`.
What is never read: the contents of band files or of anything else in a
bundle, `MachineID.plist` (which holds the Mac's host UUID, serial and model
identifiers), and the `lock` file. The SnapshotHistory plist is the only file
whose contents are read.

### Bundle sizes

A bundle's size needs a walk of thousands of band files, so no request walks.
While the provider is enabled, a background walk sums `st_blocks × 512` over
each bundle's `bands/` at start and then every `timeMachine.sizeInterval`,
at most 500,000 entries and 20 seconds per share, one walk per share at a
time. A request returns the cached figure with `sizeComputedAt`, or null with
`not computed yet` before the first walk. A walk that hits its bound stores a
partial figure with `sizeTruncated: true`. The cache is in memory only.

## A stuck pool

Every configured path on a ZFS host is usually a dataset, so a suspended pool
blocks file access to it in the kernel just as it blocks `zpool`. Every
`zpool` and `zfs` call, and every file access of a configured path, is
bounded by `commandTimeout`. A call still stuck past its bound answers at
once with its reason ("... is still running since <time>") and starts
nothing new, at most 4 tool processes run at a time, and at most 4 file
accesses run under one configured path. A stuck path or pool makes only its
own fields or items unavailable; the other paths and features still answer.

## LXC guests

On an LXC guest with no ZFS and nothing of storage interest, such as a
Pi-hole or nginx container, leave the provider disabled, the default: it
then adds no load, no background walk, no tools and no `providerSignals`
block. If it is enabled there, it reports itself unavailable with the three
reasons, for example `zfs: /usr/sbin/zpool not found (nor /sbin/zpool);
backups: not configured; timeMachine: not configured`, answers its
operations with 503 `capability-unavailable`, adds a block with
`counts: null` and that reason to `get_host_health`, and logs one line at
each preflight, not one per request.

A guest sees only its own filesystems: the Proxmox node's pools are not
visible from inside a container, and the provider never reaches past the
guest to the node. On an Alpine guest under OpenRC, the writes would run
under `op-supervise`; they are not expected to be enabled there.

The provider never runs on the Proxmox VE node itself: a host with `/etc/pve`
or the `pve-manager` package is refused as a whole.

## Beside zfs-auto-snapshot

On a host that runs `zfs-auto-snapshot`, snapshots are numerous, and
`storage.snapshots.list` pages through them; `createdByNexus=false` keeps
only the others. The two never interfere:

- `zfs-auto-snapshot` names its snapshots `zfs-auto-snap_<label>-<date>` and
  prunes only snapshots that match its own prefix and label, so it never
  removes a `nexus-` snapshot.
- A `nexus-` name never collides with a `zfs-auto-snap_` name.
- A scrub or snapshot does not change its schedule; the zfsutils monthly
  scrub cron keeps running, and a scrub it starts is reported like any
  other.

Nexus snapshots stay until you remove them with `zfs destroy`.
