# The host provider on real hosts: Ubuntu/systemd and the Alpine VM (TC-HP-127 to TC-HP-130)

Work item 5.4 of system-provider. This page has the procedure, the script
that runs it (`test/live/host-check.sh`), the transcript template to fill
in from the real runs, and the results of the runs made so far.

**Status: `pending-live-gate`.** No real-host transcript exists yet. The
real hosts (a bare-metal Ubuntu host and the user's Alpine/OpenRC VM on
Proxmox) were not reachable from the session that wrote the script, and a
24 h soak could not finish in it. The runs below are **container runs in
the T1 harness images, not real-host evidence**. They show that the script
works, that it cleans up after itself, and what the provider answers in a
container. Each transcript says so in its own header (`CONTAINER DETECTED
... NOT real-host evidence`) and records `1-host FAIL`, so every full
container run exits 1. What is still owed:

| Item | TC | Status |
| --- | --- | --- |
| Every FR-4 to FR-10 field on a real Ubuntu host and on the Alpine VM, with `--with-faults` once per host | TC-HP-127 | **PENDING** (container runs pass, below) |
| NFR-3 on both real hosts | TC-HP-128 | **PENDING** (container runs pass) |
| NFR-1 p95 on both real hosts | TC-HP-129 | **PENDING** (container runs pass) |
| The hung-mount bound (an NFS mount with its server firewalled) | TC-HP-129 | **PENDING**, an optional manual step: not set up anywhere yet |
| NFR-2 at the defaults on both real hosts | TC-HP-130 | **PENDING** (container runs in [measurements.md](measurements.md#host-provider-wi-54)) |
| NFR-2 at 24 h / 2 s, a full 24 h fill | TC-HP-130 | **PENDING**: mandatory; only a short container soak was run |
| A btrfs live readback (A-21, A-38) | | **PENDING**: no btrfs host was available; every transcript says so |

- [What the check proves](#what-the-check-proves)
- [Before you start](#before-you-start)
- [What the script touches](#what-the-script-touches)
- [Procedure](#procedure)
- [How the readbacks are done](#how-the-readbacks-are-done)
- [Transcript template](#transcript-template)
- [Container runs](#container-runs)
- [Known before the real run](#known-before-the-real-run)

## What the check proves

The T1 suite (WI 5.3, [host-t1.md](host-t1.md)) ran the provider under the
exact hardened unit and the shipped OpenRC script in harness containers.
This check runs the release tarball on the real hosts (NFR-5) and records:

- every field of the ten operations, read back with the host's own tools,
  or reported unavailable with a reason (TC-HP-127), and the same walk
  TC-HP-115 makes: no null without a reason;
- the planted faults, seen and removed: a reboot-required marker
  (Debian and Ubuntu) and a failed (systemd) or crashed (OpenRC) service;
- NFR-3 accuracy, NFR-1 latency and NFR-2 overhead, with the numbers
  recorded in [measurements.md](measurements.md#host-provider-wi-54);
- on a btrfs host, that each btrfs filesystem is listed once, with `df`'s
  numbers, and never `not visible:` (A-21, A-38);
- which fields, if any, the systemd sandbox makes unavailable. That list
  goes into [host.md](../host.md#unavailable-under-the-systemd-sandbox).

## Before you start

On each host:

- nexusd installed from the release tarball with its `install.sh`
  ([install.md](../install.md)), started, with the host provider enabled
  in `/etc/nexus/nexusd.yaml` (`providers.host.enabled: true`, see
  [host.md](../host.md#configuration)) and reloaded. Leave
  `sampleInterval` and `historyDuration` at their defaults (10 s, 1 h):
  `--overhead` measures at the defaults and refuses otherwise.
- A token with `system:read`, created on the host:

  ```
  nexusd token create --name host-check --scope system:read --expires 1d
  ```

  Pass it in `NEXUS_TOKEN`. As `nginx-check.sh` does, the script first
  runs itself again, once, with `NEXUS_TOKEN` removed from its environment
  and the token handed over on file descriptor 3 (a here-document), so no
  process it starts has the token in its environment. The token then
  reaches only `nexus host add --token-stdin` (on standard input, into a
  throwaway client configuration the script deletes) and the transcript
  check. Two results check this: `token-env` and `token-absent`. Run the
  script as a file (`sh host-check.sh`). Revoke the token when you are
  done: `nexusd token revoke host-check`.
- The `nexus` client from the same release, either on the host or on the
  machine you run the script from. From another machine, use `--ssh
  root@<host>` (or a user with `sudo` without a password) and set
  `NEXUS_URL` to nexusd's address as that machine reaches it; the
  certificate is pinned to `nexusd tls fingerprint`, read over the same
  SSH connection. On the host, the script finds nexusd at the first
  `listen` address of the configuration. Extra ssh options go in
  `NEXUS_CHECK_SSH_OPTS`.
- For `--rss-24h`, the `nexus` client on the host as well (the poll runs
  there): on the PATH, `/usr/local/bin/nexus`, or `--host-nexus PATH`.

The tools the script reads with are the distributions' own. On a BusyBox
host without `vmstat` or `lsblk`, it falls back to `/proc/stat` and
`/sys/block`, and says which it used.

## What the script touches

Without `--yes`, the script changes nothing: it records the facts
(versions, nexusd's PID, the provider's configuration and capability) and
prints the plan, and exits 2. With `--yes` alone it runs the read-only
check, which also changes nothing on the host: every host command reads.
Each change below needs `--yes` and an opt-in mode; every function that
makes one is named `mut_*` and begins with the guard `mutating_allowed ||
refuse`, which TC-HP-132 checks.

| Mode | What it changes | Undone by |
| --- | --- | --- |
| `--with-faults` | Writes `/var/run/reboot-required` and `.pkgs` (Debian and Ubuntu, and only when no marker exists); runs a transient unit `nx-host-check-<8 hex>` that fails (`/bin/false`), or on OpenRC writes `/etc/init.d/nx-host-check-<8 hex>` (a `sleep`), starts it and kills it with SIGKILL so `rc-status --crashed` lists it | Removed at once after each readback: `rm` of the marker, `systemctl reset-failed`, or `rc-service ... zap` and `rm` of the script. On SIGINT or SIGTERM, the EXIT trap does the same; after SIGKILL, `--yes --cleanup` does it from the state file |
| `--overhead` | Saves `/etc/nexus/nexusd.yaml` (0600), sets `providers.host.enabled: false`, **restarts nexusd** (a clean baseline), and restores the file byte for byte, then reloads | The restore; on an interrupt, the EXIT trap; after SIGKILL, `--yes --cleanup` |
| `--rss-24h start` | As `--overhead` for the baseline, then `enabled: true`, `historyDuration: 24h`, `sampleInterval: 2s` and a reload; creates a token `host-check-soak` (`system:read`, 2 days) on the host, piped straight into the host's `nexus host add`, never printed; starts a poll on the host (`/var/lib/nexus-host-check/poller.sh`) | `--rss-24h finish`: stops the poll, revokes the token, restores the configuration and reloads. On an interrupt during the start, the EXIT trap restores the configuration |
| `--cleanup` | Nothing new | Undoes what an interrupted `--with-faults` or `--overhead` run left |

The state of an opt-in run is in `/var/lib/nexus-host-check/` (0700). A
run refuses while an earlier one has left state there, except a read-only
check while a soak runs. Values read back from the state file or from
nexusd that reach a host command must match an allowlist (the dummy
service's name, numbers, absolute paths of `[A-Za-z0-9_./@+-]`), and are
quoted.

## Procedure

On each host, in this order (`<target>` is `--ssh root@<host>` from another
machine, or nothing on the host itself):

```
sh host-check.sh <target>                                   # facts and plan; exit 2
NEXUS_TOKEN=... sh host-check.sh <target> --yes             # TC-HP-127, -128, -129
NEXUS_TOKEN=... sh host-check.sh <target> --yes --with-faults
NEXUS_TOKEN=... sh host-check.sh <target> --yes --overhead  # about 25 min with a full history
NEXUS_TOKEN=... sh host-check.sh <target> --yes --rss-24h start
#   ... 24 hours later, with the same nexusd (no restart, no reload):
NEXUS_TOKEN=... sh host-check.sh <target> --yes --rss-24h finish
```

Optionally, the hung-mount bound: mount an NFS export, block its server
(for example `iptables -A OUTPUT -d <server> -j DROP` on the host), then
`--yes --hung-mount /mnt/<export>`. Both `hostsummary` and `hostfs` must
answer in under 2.5 s, and the mount must be listed with `timed out after
2s: statfs ...` (or `still pending:`). Remove the rule and unmount
afterwards (`umount -f -l`).

The script's exit status: 0 every check passed, 1 a check failed, 2
refused, 3 nothing failed but a check was not run (NOT-RUN). On the
Alpine VM, `7a-reboot` is always NOT-RUN (see below), and without the
manual step so is `6-hung-mount`.

## How the readbacks are done

Every `nexus host*` command runs with `-o json`, and its output is in the
transcript. The script flattens the CLI's indented JSON into `path value`
lines with `awk`, so no `jq` is needed on either side.

| Step | Result IDs | Read back with |
| --- | --- | --- |
| 3 | `3-walk-<op>` | Every response: no null without a reason (a series value excepted), each `unavailable` key names a field that is null, `[]` or `"unknown"`, each reason starts with a reason prefix. `3-sandbox` counts the `permission denied:` and `command failed:` reasons |
| 4a | `4a-identity` | `/proc/sys/kernel/hostname` (and `hostnamectl`, logged), `/etc/os-release`, `uname -r` and `-m`, the init system, `btime` of `/proc/stat` (via `date -u -d @`), `/proc/uptime` (± 5 s), `systemd-detect-virt` (or `/.dockerenv`), `/sys/class/dmi/id/*`, `/etc/timezone` or the `/etc/localtime` link, `date +%z`, `timedatectl show -p NTPSynchronized` |
| 4b | `4b-cpu` | `nproc` (or `nproc --all` and the `cpuN` lines of `/proc/stat`), `/proc/cpuinfo`'s model name, `/proc/loadavg` (± 0.25 + 10%), cpufreq's range, the hwmon and thermal inputs |
| 4c | `4c-memory` | `free -b` (total and swap exact), `/proc/meminfo` (± 1% of total; cached is `Cached + SReclaimable`, `free`'s cache column) and `/proc/pressure/*` |
| 4d | `4d-filesystems`, `4d-btrfs` | `df -B1 -P` and `stat -f` at each mount point **just before and just after a fresh call**: the provider's value must lie in that bracket, ± 1 block for used and available bytes ("exactly at the same instant", NFR-3); `fsType` and `readOnly` from PID 1's `/proc/1/mountinfo`; coverage: every disk or network filesystem in PID 1's table is listed or shares a device with a listed item |
| 4e | `4e-disks` | `lsblk -b -d -r -o NAME,SIZE,ROTA,RM` (or `/sys/block`), each disk's model and partitions; coverage of `/sys/block` less `ram*`, `zram*` and unbacked `loop*` |
| 4f | `4f-interfaces` | `/sys/class/net/<if>/*` (MAC, MTU, operstate, type, speed) and `ip -o addr`; the byte counters lie between reads before and after the call; coverage of `/sys/class/net` |
| 4g | `4g-netconfig` | `ip -4` and `ip -6 route show default`; the nameservers of `dns.source` and `/etc/resolv.conf` |
| 4h | `4h-health` | `systemctl --failed --plain --no-legend` or `rc-status --crashed`, the reboot rule of [host.md](../host.md#reboot-required), `/proc/vmstat`'s `oom_kill`, and each top process's `comm`, uid, `/etc/passwd` user and `VmRSS`, read 0.3 s after a sampler tick and compared with that tick's scan (a process younger than a minute is shown, not compared) |
| 4i, 4j | `4i-summary`, `4j-metrics` | The summary against the per-area operations; the history's sampler state, points and series, and a 24 h window with `network` and `disk` |
| 5 | `5-cpu`, `5-memory`, `5-network` | NFR-3 over one sampler interval, aligned on its tick: the host command sleeps until 0.3 s after a tick, reads `/proc/uptime`, `/proc/stat` and every interface's counters, runs `vmstat <interval> 2`, and reads them again. The provider's next sample covers the same interval. CPU `usagePercent` against `100 − id − wa` (± 5 pp); `availableBytes` against `free -b` (± 1%); each interface carrying at least 1 KiB/s each way against the counter delta (± 5%). Traffic comes from read calls through the CLI for the whole window |
| 6 | `6-latency`, `6-hung-mount` | `--calls` (50) calls of each operation, each a new `nexus` process, timed by the CLI's own `--verbose` line (`< 200 OK (12ms)`: from the request to the status line, the TLS handshake included); p95 < 200 ms |
| 7 | `7a-reboot`, `7b-service` | `--with-faults`, above |
| 8 | `8-cpu`, `8-rss-1h` | `--overhead`: nexusd's `utime + stime` (`/proc/<pid>/stat`) over `--window` (600) seconds and `VmRSS` every 60 s, enabled with the 1 h history full, then disabled after a restart (60 s to settle). CPU: enabled − disabled < 1% of one CPU; RSS: the enabled maximum − the disabled mean < 8 MiB |
| 9 | `9-rss-24h` | `--rss-24h`: the disabled baseline (mean `VmRSS` over `--baseline` seconds after a restart), then the 24 h fill at 2 s with the poll; the poll logs `VmRSS` every 60 s; `finish` takes the maximum over the last 10 minutes. Less than 24 h elapsed is NOT-RUN, with the numbers so far |

The provider's alignment is checked: its sample must be the one that ends
the host's window, or the comparison fails rather than passing on
different intervals.

## Transcript template

Fill one per host from the real runs, and commit the transcripts next to
this page, in `host-live/`, as `real-<host>-<step>.txt`.

```
Host:                  <ubuntu|alpine-vm>, <hostname>, <CPU>, <RAM>, <kernel>, <init and version>
Release:               nexusd <version> from nexusd-<version>-linux-<arch>.tar.gz, sha256 <...>
Script:                test/live/host-check.sh sha256 <...> (second line of each transcript)
Read-only check:       exit <..>; 4a..4j PASS/FAIL; unavailable fields: <list from 3-sandbox>
Sandbox-caused fields: <none | field: reason>   -> docs/host.md "Unavailable under the systemd sandbox"
NFR-3:                 cpu <p> vs <v> pp; available <p> vs <v>; <if> rx/tx <p> vs <v>
NFR-1:                 worst p95 <..> ms (<op>); hung mount: <not set up | summary .. ms, hostfs .. ms, reason ..>
--with-faults:         7a <..>; 7b <..>; cleanup <..>
--overhead:            8-cpu <enabled>% - <disabled>% = <..>%; 8-rss-1h <max> - <mean> = <..> MiB
--rss-24h:             start <UTC>, finish <UTC>; max VmRSS last 10 min <..> kB - baseline <..> kB = <..> MiB
btrfs:                 <none on this host | devices, items, df numbers>
```

## Container runs

All runs used one script, `test/live/host-check.sh` with sha256
`281d577abca434c4664c63026f2c831926468788926470fca474e57e98a21778` (the
second line of each transcript), except 31 and 32 (below). nexusd and nexus
were built for linux/arm64 from **fb475bc** (`git rev-parse HEAD` in each
setup log), packed with `make nexusd-tree`, and installed by the release
tree's `install.sh`, as on a real host. They ran on 2026-11-15 (UTC) on
the local Docker host (colima, linux/aarch64, Apple Virtualization, 4
vCPU, 7.7 GiB, kernel 6.8.0-100-generic).

| Container | Image | Init | How the script ran |
| --- | --- | --- | --- |
| `nx-wi54-ubuntu-…` | `nexus-harness:ubuntu-latest` (`sha256:7435fd8cd66a…`), Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1); nexusd under the unchanged `deploy/nexusd.service` (sha256 `5dcacb08…`, no drop-in, `ubuntu-setup.log`) | On the host, as root, with the nexus CLI there |
| `nx-wi54-alpine-…` | `nexus-harness:alpine-alpine-latest` (`sha256:da04a04680b2…`), Alpine 3.24.4 | OpenRC 0.63.2; the shipped `nexusd.openrc` | From the Mac with `--ssh` (sshd added, ports published on the Mac's loopback) and the darwin nexus CLI, as the user will run it against the VM |
| `nx-wi54-soak-ubuntu-…`, `nx-wi54-soak-alpine-…` | As above | As above | On the host, for the soak |

The containers were started with the flags of `test/harness/run.sh`
(`--privileged`, `--tmpfs /run`, anonymous volumes for the nested Engine,
`--cgroupns=host` or `private`). The setup logs are in
[`host-live/`](host-live/) (`*-setup.log`); the token values are in none
of the committed files (checked with `grep -F -f` against the four token
files before committing).

| File | Command | Result |
| --- | --- | --- |
| `10-ubuntu-plan.txt` | (no `--yes`) | Facts and the plan; `1-provider` PASS (enabled, systemd, 10 s, 3600 s). Nothing changed. Exit 2 |
| `11-ubuntu-check.txt` | `--yes` | Every `4*` result PASS: identity 18 fields, CPU 10 (+3 unavailable), memory 13, filesystems 36 over 5 mounts, disks 13 (+3), interfaces 26 (+2), netconfig 2, health 16, summary 16, metrics 4; all ten walks PASS; 15 unavailable fields, all `not reported:`. NFR-3: CPU 49.33% vs vmstat 49; available 7,011,794,944 vs 7,011,033,088; lo 363,370 B/s vs 363,713 (rx) and 363,696 (tx). NFR-1: worst p95 7 ms (`hostsummary`, `hosthealth`). Exit 1, from `1-host` only |
| `12-ubuntu-faults.txt` | `--yes --with-faults` | As 11, then `7a-reboot` PASS (planted: `true`, packages `[nexus-host-check]`; removed: `false`, needrestart's `Ubuntu.pm` being installed) and `7b-service` PASS (`nx-host-check-7e34add1.service` listed, count 2 = `systemctl --failed`; gone after `reset-failed`). `cleanup` PASS. Exit 1, from `1-host` |
| `13-ubuntu-sigterm-marker.txt` | `--yes --with-faults`, SIGTERM once the marker was written (`interrupt.sh.txt`) | The EXIT trap removed the marker and the state; exit 130 |
| `14-ubuntu-sigkill-unit.txt` | `--yes --with-faults`, SIGKILL once the failing unit ran | No trap can run: the unit stayed failed and the state stayed (exit 137) |
| `15-ubuntu-refuse-pending.txt` | `--yes` after 14 | `REFUSED: an earlier run did not finish (... mode check+faults): run with --yes --cleanup first`. Exit 2 |
| `16-ubuntu-cleanup.txt` | `--yes --cleanup` | From the state: `reset-failed` of 14's unit, state removed; `systemctl --failed` afterwards lists only the harness's own `systemd-modules-load.service`. Exit 1, from `1-host` |
| `17-ubuntu-overhead.txt` | `--yes --overhead` | Waited until the 1 h history was full (08:17 UTC), then `8-cpu` PASS (enabled 0.022% − disabled 0.003% = 0.018% of one CPU) and `8-rss-1h` PASS (25,992 kB − 21,843 kB = 4.05 MiB); the configuration restored byte for byte. Exit 1, from `1-host`. Details in [measurements.md](measurements.md#host-provider-wi-54) |
| `20-alpine-plan.txt` | `--ssh` (no `--yes`) | Facts and the plan; `1-provider` PASS (openrc). Exit 2 |
| `21-alpine-check.txt` | `--ssh --yes` | Every `4*` PASS: identity 17 (+1: `clock.synchronized` unknown, `not supported:`, no timedatectl or chronyc), CPU 10 (+3), memory 13, filesystems 29 over 4 mounts, disks 13 (+3, `lsblk`), interfaces 26 (+2), netconfig 2, health 16 (+1: `rebootRequired` unknown in an Alpine container), summary 16, metrics 4; 19 unavailable fields, none `permission denied:` or `command failed:`. NFR-3: CPU 24.31% vs vmstat 24; available 7,132,397,568 vs 7,137,406,976; eth0 120,222 vs 119,713 B/s rx, 219,706 vs 218,992 B/s tx. NFR-1 (over the SSH port forward): worst p95 8 ms. Exit 1, from `1-host` |
| `22-alpine-faults.txt` | `--ssh --yes --with-faults` | `7a-reboot` NOT-RUN (see below); `7b-service` PASS (`nx-host-check-0e98632c` in `rc-status --crashed` and `failedServices`, count 1; gone after `zap`). Exit 1 |
| `23-alpine-overhead.txt` | `--ssh --yes --overhead` | `8-cpu` PASS (0.018% − 0.003% = 0.015%), `8-rss-1h` PASS (4.10 MiB); restored. Exit 1, from `1-host` |
| `30-ubuntu-soak-start.txt`, `30-alpine-soak-start.txt`, `33-…-soak-finish.txt` | `--yes --rss-24h start` on the soak containers, then `--yes --rss-24h finish` after 1.07 h | Baselines 22,077 and 22,103 kB; 2 s and 24 h from 07:34:58 UTC; the poll made 1,137 and 1,155 rounds, none failed. Finish: `9-rss-24h` **NOT-RUN** ("the history was not full: 1.07 h of 24 h"), 7.47 and 7.50 MiB so far; the token revoked, the configuration restored (`conf-restored` PASS). Exit 1, from `1-host`. **The 24 h run is pending** ([measurements.md](measurements.md#nfr-2-numbers)) |
| `31-ubuntu-soak-start-sigterm.txt`, `32-alpine-soak-start-sigterm.txt` | `--yes --rss-24h start`, SIGTERM during the disabled baseline (an earlier script, sha256 `28c99d83…`, which differs only in step 4h's process comparison) | The EXIT trap restored `/etc/nexus/nexusd.yaml` byte for byte (`conf-restored` PASS), reloaded, and removed the state. Exit 130 |

### What the container runs show

- **Every field read back.** On both images every field of the ten
  operations matched the host's tools, or was unavailable with a reason the
  host's state explains: no CPU model or frequency on this arm64 VM, no
  sensors, no disk model on virtio disks, no link speed on `docker0` and
  `lo`, no IPv6 address, and on Alpine no time-sync query and the reboot
  flag unknown in a container. No walk found a null without a reason.
- **No field is unavailable because of the sandbox** under the exact unit,
  in agreement with WI 5.3's TC-HP-115 list; TC-HP-131 checks
  [host.md](../host.md#unavailable-under-the-systemd-sandbox) against it.
- **Used bytes are exact.** An earlier draft compared `df` read some
  seconds after the call and failed by 13 blocks on the busy shared disk;
  the bracket of reads just before and after a fresh call put every used
  and available byte count inside ± 1 block.
- **A process's RSS must be compared at the scan.** An earlier draft
  compared `topByMemory`'s `rssBytes` with `VmRSS` read later, and the
  Alpine sshd listener, which serves the script's own SSH sessions, had
  grown from 1.8 MB to 3.7 MB in between. Read 0.3 s after the tick whose
  scan the answer carries, the two agree.
- **Cleanup holds.** A SIGTERM in the middle of `--with-faults` or of the
  soak's start undoes the change at once; after a SIGKILL, every run
  refuses until `--cleanup` has undone it.

## Known before the real run

- **The Alpine VM's reboot flag cannot be planted.** Alpine's rule is
  whether `/lib/modules/<running release>` exists; planting it would mean
  moving the running kernel's modules away, which the script never does.
  `7a-reboot` is NOT-RUN there, and `4h-health` checks the rule as the host
  is (on the VM, after a kernel upgrade without a reboot, it should be
  `true`).
- **The VM may lack `vmstat`, `lsblk` and `hostnamectl`.** The script then
  uses `/proc/stat`, `/sys/block` and the kernel hostname, and says so.
  BusyBox `date` has no `%N`; the script needs no sub-second time on the
  host (`/proc/uptime` has centiseconds).
- **Real interfaces have link speeds, real CPUs have models, real disks
  have models and maybe rotational media,** so fewer fields should be
  unavailable than in the containers, and the comparisons for them run.
- **The NFR-2 numbers here are a container's on an arm64 VM**, with few
  processes (16 and 21) and mounts (4 and 5) against the reference VM's
  200 and 10. The process scan's cost grows with the process count.
