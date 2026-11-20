# Storage provider test data

Captures of the real tools that the storage provider's parsers, fakes and
oracles are built and checked on (storage-provider WI 1.1 and WI 1.2;
NFR-4, NFR-8, NFR-8a).

## Layout

```
testdata/
  capture.sh                       the host capture script (this file's subject)
  captures/
    upstream-openzfs-2.4.1/        OpenZFS 2.4.1 vectors (WI 1.1)
    synthetic/                     edge cases derived from those vectors (WI 1.1)
    home-server/                   host captures (WI 1.2, operator-run)
    pihole/
    public-nginx/
```

Every capture directory has a `PROVENANCE` file of `key: value` lines. The
`commands` key (and, for the upstream and synthetic directories, `files`)
is followed by indented `name: value` lines, one per file.

| Key | Meaning |
|---|---|
| `host` | the host name (kept: `storage:read` already exposes it) |
| `source` | `host`, `upstream` or `synthetic` |
| `date` | RFC 3339 UTC time of the capture (or of the fetch) |
| `timezone`, `utc-offset` | host captures only: the zone the text form's dates were printed in (see below) |
| `os-release`, `kernel`, `zfs-version` | `PRETTY_NAME`, `uname -r`, the first line of `zfs version` |
| `captured-as` | `root` for every host capture |
| `options` | the flags used (`--sandbox-probe`, `--write-probe`, `--error-pool`), or `none` |
| `forms` | which form of each GNU-only option the host accepted (see Portability), and `tz=`, how the time zone was found |
| `redaction` | the placeholder families and counts (`mac: 3; guest: 7; plist-strings: 12`), or `none` for upstream and synthetic |
| `snapshot-rows`, `snapshot-bytes-json`, `snapshot-bytes-text` | the snapshot listing's row count and stdout bytes in each form (A-39) |
| `commands` | each output file and the argv that wrote it, with the fallback form actually used |

The upstream directory adds `tag`, `commit` (the 40-hex commit the tag
points at), `tag-object`, and a `files` entry per vector: `verbatim:` (lines
copied from a man page, with the line range, the man page's and the file's
sha256 and the URL at the commit) or `source-derived:` (built from the
named source lines, each cited by URL at the commit). The synthetic
directory's `files` entry for each vector reads `derived-from <vector>;`
followed by what changed. Synthetic vectors change only values of their
source, or add or remove whole pool and vdev objects of the same shape.
They are provisional until the host captures exist and are never the only
evidence for a parser rule that a host capture can show.

### The time zone of a host capture (A-60)

`zpool status -p` prints its scan dates with `ctime(3)` in the local zone of
the `zpool` process, which runs with `env -i` and so reads `/etc/localtime`.
The text parser reads them in `time.Local`, the same file. A host capture
therefore records the zone its text was printed in, and the parity test
(TC-STG-71) parses the text in that zone:

- `utc-offset` is `date +%z` at the capture (`[+-]hhmm`);
- `timezone` is the first of these that answers: the part after the last
  `zoneinfo/` of `readlink /etc/localtime`; the first line of
  `/etc/timezone`; `timedatectl show -p Timezone --value`. It is `UTC` when
  `/etc/localtime` does not exist (glibc and musl then use UTC), else
  `unknown`;
- `forms` carries `tz=localtime-link`, `tz=etc-timezone`, `tz=timedatectl`,
  `tz=no-localtime` or `tz=unknown` for the source used.

The tests load `timezone` with `time.LoadLocation` (importing
`time/tzdata`), or use a fixed zone of `utc-offset` when it is `unknown`,
and first check that the zone's offset at `date` equals `utc-offset`.
Upstream and synthetic captures hold no text-form dates and carry neither
key.

## The Exit and .stderr conventions

For a host capture, every file named in `commands` holds the command's
stdout followed by one line `Exit <code>`. Its stderr is in
`<file>.stderr`, empty when the command wrote nothing there. When stdout
does not end in a newline, `capture.sh` adds one before the `Exit` line and
marks the command `[noeol]` in `commands`. A command that is not on the
host (`systemctl` on Alpine, `zpool` on pihole) is `Exit 127` with a
`.stderr` of `not found`; it never stops the run.

## How a fake reads a capture

A fake built on a capture directory replays a command by its argv:

1. find the argv in `PROVENANCE` `commands` (the argv without the
   `/usr/bin/env -i PATH=... LC_ALL=C` prefix is what the provider's `Argv`
   builds);
2. stdout is the file up to, not including, its last line, less the one
   newline `capture.sh` added when the command is marked `[noeol]`;
3. the exit code is the number on the last line;
4. stderr is `<file>.stderr`, byte for byte.

An argv that is not in `commands` fails the test (TC-STG-75). Upstream and
synthetic vectors are stdout only: their exit code is 0 and their stderr
empty. The upstream man-page examples are the string form (no
`--json-int`); the source-derived status vector and the synthetic status
vectors are the `--json-int` form the provider runs.

## capture.sh

The same script runs on all three hosts: home-server (Ubuntu, GNU), pihole
(Debian) and Public-Nginx (Alpine, BusyBox; its capture directory and
`--host-name` are `public-nginx`). Run it as root:

```
sh capture.sh --host-name home-server --out /root/nexus-capture-home-server \
    --map /root/nexus-capture-map-home-server.txt [--sandbox-probe] [DIR...]
```

`DIR...` are the vzdump and Time Machine directories to list (default
`/media/vzdump /media/vzdump/dump /media/timemachine /media/timemachine2`).

The default run only reads. The ZFS subcommands it runs are `version`,
`list`, `status` and `get`, each as `/usr/bin/env -i
PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C` followed by the absolute path
of `zpool` or `zfs` (found in `/usr/sbin`, then `/sbin`). It records the
host's identity and time zone, the presence of `/dev/zfs`, the ZFS tools and
Proxmox, the pool and dataset reads in both forms (the JSON `zpool list` with
`-o`, as `zfs.Argv` builds it, A-58), the snapshot listing in both forms,
three negative cases, a listing of each directory and of each bundle's root,
the first 8 bytes of each bundle's `com.apple.TimeMachine.SnapshotHistory.plist`
(the whole file when it is XML and at most 64 KiB), the first and last 40
lines of the newest vzdump archive's log and the first line of its notes,
the `df` and `du` oracles, and the schedules. `ls` always runs with `-n`, so
no user name is captured. A bundle's `MachineID.plist` is never read, and
the bundle's band files are only measured with `du`, never listed or read.

### Portability

Each GNU-only option is tried once, and the fallback is used when it exits
non-zero; `forms` in `PROVENANCE` and each argv in `commands` record the
form used:

| GNU form | Fallbacks |
|---|---|
| `ls -lan --time-style=full-iso` | `ls -lan --full-time` (BusyBox 1.37), `ls -lane` (older BusyBox), `ls -lan` |
| `du -s --block-size=1` | `du -sk` (the oracle then compares at 1 KiB precision) |
| `df -B1` | `df -k` |
| `timeout 120 CMD` | `timeout -t 120 CMD` (older BusyBox), no timeout |

### Redaction (A-55)

The captures are committed to a public repository, so redaction is on by
default. Everything is captured into a private staging directory; before
anything is written under `--out`:

- each distinct `*.sparsebundle` and `*.backupbundle` name becomes one
  `mac-<n>` placeholder with its extension (`mac-1.sparsebundle`), numbered
  in order of first appearance across the directories. The same name in two
  shares gets one placeholder (A-62), so a captured directory name is the
  placeholder its files' contents use;
- the newest archive's `.notes` first line, and each `VM Name:` or
  `CT Name:` value in its captured log lines, become `guest-<vmid>`;
- every `<string>` value of a captured XML plist becomes `redacted-<n>`,
  numbered in order of first appearance (keys and `<date>` values are kept).

A bundle name is replaced everywhere, in every captured file and file name,
as a fixed string (also in the form BusyBox `ls` prints, with each byte
outside printable ASCII shown as `?`): it is long, unique and ends in
`.sparsebundle` or `.backupbundle`. A guest name is not: a name of any length
can be a substring of the capture's own text (`xi` in `Exit`, `zfs` in
`zfs-2.4.1` or a dataset name), so it is replaced

- in its own field, always: the value after `VM Name:` or `CT Name:` in the
  captured log lines, and the first line of the `.notes` file;
- elsewhere only in the `vzdump/` files, only as a whole word (no letter,
  digit or underscore touches the match), and only when the name is at least
  4 bytes, is not made of digits and punctuation alone, and is not one of
  `Exit`, `INFO`, `WARN`, `ERROR`, `Finished`, `Backup`, `Starting`, `Name`,
  `zfs`, `zpool`, `vzdump`, `guest`, `mac` and a few more keywords (compared
  without case). A name that fails these is kept out of the whole-word pass:
  the script says how many names were field-only, and the operator checks
  `vzdump/` by eye.

It never rewrites `zfs/`, `host/`, `schedules/`, `sandbox-probe/` or
`PROVENANCE`, so each file still ends in its `Exit <code>` line. TC-STG-6
checks the committed tree, and `test/buildsys` runs the script on a temp tree
with the names `xi`, `zfs` and `Alice Smith`.

A Time Machine share root also lists one
`.com.apple.timemachine.supported-<id>` marker per Mac; each name becomes
`.com.apple.timemachine.supported-id-<n>`. Plist strings are replaced inside
`<string>` elements only, including a value that spans lines. The only
`systemctl list-timers` lines kept are the header and the lines naming `zfs`,
`zpool`, `scrub` or `snapshot`, which is what A-20 needs. Pool, dataset and
snapshot names, sizes, dates and the host's own name are kept: `storage:read`
already exposes them and the parsers' oracles need them. Any other free-form
name, such as a personal directory in a share root or a text line in a log
head or tail, is not redacted by the script: the operator reviews the output
before handing it over, and TC-STG-6 refuses a share-root listing entry that
is not `.`, `..`, `dump`, `.DS_Store`, a placeholder bundle, a marker
placeholder or a `vzdump-` file. Parser tests compare structure, counts and
values, never the literal placeholders beyond their grammar.

The mapping (each placeholder and the real value it replaced) goes to
`--map FILE` (default `/root/nexus-capture-map-<date>.txt`, mode 0600). It
must lie outside `--out` (the script refuses otherwise) and is never
committed. `PROVENANCE` `redaction` lists the families and counts only. The
operator reviews the redacted directory before handing it over (WI 1.2).
TC-STG-6 checks the committed tree.

### --sandbox-probe

Read-only. It reads the installed unit's hardening directives (`systemctl
cat nexusd`, the lines between `# Hardening` and `[Install]`, saved as
`sandbox-probe/hardening`) and runs, under `systemd-run --wait --pipe
--collect` with one `-p` per directive, every read argv `zfs.Argv` builds
for the provider, exactly as it builds it, in both formats (every `Command`
except the four writes and `CmdSnapshotGet`, the helper's readback, which
runs outside the sandbox with the writes). Each runs unnamed where it allows
that, and named with `P1`, the first pool of `zpool list -H -o name`. `P1`
names the pool for pool commands and the pool's root dataset for dataset and
snapshot commands (`zfs list ... P1`, `-d 1 P1`, `-r P1`), which is always
present, so no dataset has to be chosen. TC-STG-72 compares every `Argv` read,
with its name replaced by `P1`, to the end of a line of
`sandbox-probe/commands`, so the match is exact. The unnamed list is `zfs
version`; `zpool list -H -o name`; `zpool list` in both formats with `-o
PoolListColumns`; `zpool status` and `zpool status -v` in both formats;
`zfs list -t filesystem,volume -o DatasetColumns` in both formats; `zfs list
-H -p -t snapshot` with `-o name,creation` and with `-o SnapshotColumns`. The
named list is `zpool list`, `zpool status`, `zpool status -v` and `zpool get
all` for `P1` in both formats, `zfs list -t filesystem,volume` for `P1` in
both formats, and `zfs list -H -p -t snapshot` with `-o name,creation -d 1
P1`, `-o SnapshotColumns -d 1 P1` and `-o SnapshotColumns -r P1`. As A-39
evidence only (not an `Argv` shape) it also runs `zfs list -j --json-int -t
snapshot -o SnapshotColumns`. Each output is a capture file (stdout, `Exit`
line, `.stderr`), so the named reads are replayable captures of the host. It
also runs `stat -f` of each directory, bounded reads of one
log, one notes file and one `SnapshotHistory.plist` piped to `wc -c` (only
the exit code and the byte count are kept), and `zfs list -H -o
name,mounted,mountpoint` inside the probe (`mounts-inside`) and outside it
(`mounts-outside`) with their `mounts.diff` (A-8). Each probe's argv is in
`sandbox-probe/commands`, and any `Operation not permitted`, `Permission
denied`, `SECCOMP` or `Bad system call` text in `sandbox-probe/denials.txt`
(A-6).

### --write-probe DATASET

Opt-in. Runs `zfs snapshot DATASET@nexus-capture-probe` under the same
sandbox properties, expecting a permission denial (A-7). If it succeeds,
the capture is still written, the script prints the snapshot's name for the
operator to remove by hand, and exits 3.

### --error-pool

Opt-in; it creates and destroys a scratch pool on the operator's host, the
only source of a real pool with errors (NFR-8, A-4). Its safety steps:

1. It refuses to start when a pool named `nxcapture` is imported or
   importable (`zpool list`, `zpool import`, `zpool import -d /var/tmp`), or
   `/var/tmp/nxcapture.img` exists.
2. It prints a notice that ZED (`zfs-zed`) may send an alert for the
   injected checksum errors on `nxcapture`; stop `zfs-zed` for the run or
   expect the alert.
3. It sets a trap on `EXIT INT TERM HUP` that runs `zpool destroy -f
   nxcapture` (ignoring "no such pool") and removes the image, so a run that
   dies midway leaves nothing behind.
4. Steps: a 256 MiB `/var/tmp/nxcapture.img`; `zpool create -o
   cachefile=none nxcapture /var/tmp/nxcapture.img` (never in
   `/etc/zfs/zpool.cache`); a 96 MiB file written; `zpool export`; 64 MiB in
   the middle of the image overwritten with `dd`; `zpool import -o
   cachefile=none -d /var/tmp nxcapture`; `zpool scrub -w`; `zpool status -j
   --json-int -v nxcapture` and its text form captured under `error-pool/`;
   then the pool is destroyed and the image removed.

A failed step stops the error pool (the trap still cleans up) but not the
rest of the capture.

## Vectors derived inside tests

Some vectors no host can produce, and WI 1.1 does not commit them: the text
twins of the synthetic JSON vectors, health words such as `FROBNICATED`, a
3 KiB `status` text, a suspended pool's `zpool list` row, a mirror with
`logs`, `cache` and `spares` groups. A test may derive them in code from a
committed vector under these rules:

- each derivation is a function or constant in the test file that starts from
  a named committed vector and cites it, and cites the OpenZFS print function
  it follows (a text twin follows `status_callback`, `print_status_config` or
  `print_scan_scrub_resilver_status`, by file and line);
- it changes only values, or adds or removes whole objects of a shape the
  source vector already has (pools, vdevs, class groups, `{value, source}`
  property objects), and adds no key name that OpenZFS 2.4.1 does not print
  for the argv being modelled;
- a text twin has its dates rendered and parsed in UTC (`textLoc` pinned);
- it is never the only evidence for a rule a host capture can show: the text
  captures of WI 1.2 (`zpool status -p -v`, `zpool list -H -p`, `zpool get -H
  -p`, `zfs list -H -p`) and the `--error-pool` capture join the same tables
  unchanged and decide any disagreement (A-43).

## The capture gate

Phase 3's parsers may be built on the upstream and synthetic vectors, but
Phase 4 starts only after the host captures pass the same tables (A-41).
Every capture-driven test asks for a host capture through
`capturegate.Require(t, path)` (package
`internal/providers/storage/capturegate`): while the capture is absent the
test skips with `operator capture pending (WI 1.2): <path>`, and with
`NEXUS_STORAGE_REQUIRE_CAPTURES=1` it fails with the same message instead.
TC-STG-L0 records the `NEXUS_STORAGE_REQUIRE_CAPTURES=1` run before
Phase 4.

## WI 1.2 answers (home-server)

What the committed `captures/home-server/` shows for the assumptions of the
architecture's ledger. TC-STG-7 reads this section: it checks that each
answer below exists, that the A-16 and snapshot figures equal what the files
show, and that the option list is the one `PROVENANCE` records. pihole and
Public-Nginx have no answers here yet: their captures are owed, and TC-STG-7
fails for them under `NEXUS_STORAGE_REQUIRE_CAPTURES=1` until they land.

Captured 2026-11-19T15:35:31Z as root, `capture.sh --host-name home-server
--out DIR --map FILE --sandbox-probe`, with the share directories
`/media/vzdump`, `/media/vzdump/dump`, `/media/timemachine` and
`/media/timemachine2`. Options used: `--host-name`, `--out`, `--map`,
`--sandbox-probe`. Not used: `--write-probe` (A-7 is open), `--error-pool`
(A-4's real error list is open).

- A-1: holds. `zfs version` is `zfs-2.4.1-1ubuntu5.1` with the kmod
  `zfs-kmod-2.4.1-1ubuntu5.1`; every `-j --json-int` command other than the three negative cases exited 0.
- A-2: holds on the host. `zpool status -j --json-int` has `output_version`
  (`zpool status`, 0.1), a `pools` map, per pool `state`, `status`,
  `action`, `error_count`, `vdevs` and `scan_stats` with `function`,
  `state`, `start_time`, `end_time`, `to_examine`, `examined`, `processed`,
  `errors` and `scrub_pause` (0 for all three finished scrubs).
- A-3: holds. `zpool list -j`, `zpool get -j` and `zfs list -j` give
  `{value, source: {type, data}}` for every property, and `zpool list -j -o`
  reports exactly the columns asked for, including `readonly` (A-58).
- A-4: partly open. No pool has errors: `error_count` is 0 and no `errlist`
  is present. A real error list needs `--error-pool`, which was not run.
- A-5: holds on the host. Each pool's last scrub is `SCRUB` `FINISHED` with
  `processed` 0 and `errors` 0, and the text form prints "scrub repaired 0B
  in … with 0 errors".
- A-6: holds. All 34 probe commands of `sandbox-probe/commands` exited 0
  under the unit's hardening, with an empty `.stderr` each, and
  `denials.txt` is empty. TC-STG-8 asserts it.
- A-8: holds. `mounts-inside` and `mounts-outside` (19 lines each) are equal
  and `mounts.diff` is empty. TC-STG-8 asserts it.
- A-10: holds. `/media/vzdump/dump` holds
  `vzdump-<qemu|lxc>-<vmid>-<YYYY_MM_DD-HH_MM_SS>.<vma.zst|tar.zst>` with a
  `.log` and a `.notes` beside each archive, in `dump/` below the share root
  `/media/vzdump`.
- A-11: holds on the one log captured. Its tail ends in
  `INFO: Finished Backup of VM 103 (00:00:07)` behind a
  `YYYY-MM-DD HH:MM:SS` prefix; no failed job's log was in the capture, so
  the `ERROR:` shape is still open.
- A-12: holds. `/media/vzdump/dump` lists an unfinished qemu-101 backup as a
  `vzdump-qemu-101-….tmp` directory and a `….vma.dat` file.
- A-14: holds. Bundles are `*.sparsebundle` directories directly under
  `/media/timemachine` and `/media/timemachine2`; no `.backupbundle`, and no
  nesting. The share root also holds `.DS_Store` and one
  `.com.apple.timemachine.supported-<id>` file per Mac.
- A-15: holds. The first 8 bytes of every captured `SnapshotHistory.plist`
  are `<?xml ve`, so the XML reader applies; no binary plist was seen.
- A-16: holds. Every captured bundle root has a zero-byte `lock` file with
  an old mtime, so `lock` is not an in-progress marker. Per bundle root:
  - shares/3/mac-1.sparsebundle: lock present (0 bytes, 2026-08-07); backup running: no
  - shares/3/mac-2.sparsebundle: lock present (0 bytes, 2026-08-07); backup running: no
  - shares/4/mac-1.sparsebundle: lock present (0 bytes, 2025-08-10); backup running: no
  - shares/4/mac-2.sparsebundle: lock present (0 bytes, 2024-10-02); backup running: no

  "Backup running: no" is read from the files, not observed: every bundle
  root's newest write (`Results.plist`, `SnapshotHistory.plist`) is hours to
  months before the capture's `date`, and the capture was taken at 07:35 in
  the morning.
- A-17: open. `du --block-size=1` of `bands/` is recorded for the first
  bundle of each share (1215150797824 and 585770062848 bytes); the
  `WalkBands` sum is compared with `du` on temp trees by TC-STG-106, and on
  these bundles by WI 6.1.
- A-20: holds. `/etc/cron.d/zfsutils-linux` scrubs on the second Sunday of
  each month (`24 0 8-14 * * root if [ $(date +\%w) -eq 0 ] …`) and `trim`
  on the first; `systemctl list-timers` shows no ZFS timer, so cron is the
  schedule.
- A-22: holds. `zfs-auto-snapshot` is scheduled in `/etc/cron.d`, and the
  capture lists 635 snapshots, all named `zfs-auto-snap_*`, at most 71 for
  one dataset, well under 5,000; none is a `nexus-` snapshot.
- A-36: holds. A missing pool exits 1 with `cannot open 'nexus-capture-absent':
  no such pool` (`zpool list`, `zpool status`) and a missing dataset with
  `cannot open 'nexus-capture-absent/x': dataset does not exist` (`zfs list`).
- A-39: measured. Bytes per row is the stdout length divided by the row
  count, rounded down:
  - snapshot rows (json): 635; bytes per row: 575
  - snapshot rows (text): 635; bytes per row: 77

  A JSON listing of 5,000 snapshots would be about 2.7 MiB and a text one
  about 0.4 MiB, both under the 4 MiB cap.
