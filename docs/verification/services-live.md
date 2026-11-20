# The services provider on real hosts: home-server, pihole and Public-Nginx (WI 6.0 to 6.3)

Phase 6 of services-provider: the live done-gate (NFR-4). This page has the
procedure, the script that runs WI 6.1 (`test/live/services-check.sh`), the
places where the WI 6.0 captures and the WI 6.1 transcripts go, and what is
still owed.

**Status: `pending-live-gate`.** No real-host capture or transcript exists
yet. The only runs so far are container dry runs in the T1 harness images
([Container dry run](#container-dry-run)). They show that the script works;
they are not evidence for any TC-SV-L case.

**Scope decision (the user, 2026-11-18).** Deploy and run the live checks
without the real reboots. WI 6.1 runs in full except what needs a reboot:
the script sends no reboot, no `nexus machine restart` and no reboot
what-if, and the reboot stays disabled. WI 6.2 and WI 6.3 are **owed — user
chose to skip real reboots on 2026-11-18**.

| Work item | TC | Status |
| --- | --- | --- |
| 6.0 Real-host captures, TC-SV-45 without its skip | TC-SV-L0 | **PENDING** |
| 6.1 (a) list, get and logs against the host's tools | TC-SV-L1 | **PENDING** (container dry runs pass) |
| 6.1 NFR-1 p95 over 20 calls | TC-SV-L2 | **PENDING** (container dry runs pass) |
| 6.1 capabilities details | TC-SV-L3 | **PENDING** (container dry runs pass) |
| 6.1 (b) start, stop, restart of the test service | TC-SV-L4 | **PENDING** (container dry runs pass) |
| 6.1 (c) refusals for `nexusd`, `ssh`/`sshd`, `selfService` | TC-SV-L5 | **PENDING** (container dry runs pass) |
| 6.1 (e) no sandbox denial in the journal (systemd) | TC-SV-L6 | **PENDING** (container dry run passes on Debian 12) |
| 6.1 (f) boot-ID source, read-only | WI 6.1 (f) | **PENDING** (container dry runs pass, with a T1-style bind mount) |
| 6.1 the audit trail of the actions above | TC-SV-L13 | **PENDING** (container dry runs pass); the reboot rows are owed with WI 6.2 and 6.3 |
| 6.2 pre-reboot LXC checks, guarded reboots of pihole and Public-Nginx, what-if and delay-window cancel | TC-SV-L7 to L10, L12 | **owed — user chose to skip real reboots on 2026-11-18** |
| 6.3 guarded reboot of home-server in a maintenance window | TC-SV-L11, L12 | **owed — user chose to skip real reboots on 2026-11-18** |

- [The hosts](#the-hosts)
- [Before you start](#before-you-start)
- [WI 6.0: captures](#wi-60-captures)
- [WI 6.1: the live check](#wi-61-the-live-check)
- [What the script touches](#what-the-script-touches)
- [Results: home-server](#results-home-server)
- [Results: pihole](#results-pihole)
- [Results: Public-Nginx](#results-public-nginx)
- [NFR-1 timings](#nfr-1-timings)
- [WI 6.2 and 6.3: the reboots (owed)](#wi-62-and-63-the-reboots-owed)
- [Container dry run](#container-dry-run)

## The hosts

| Host | Machine | Init | Expected `bootIdSource` | nexusd logs |
| --- | --- | --- | --- | --- |
| home-server | Ubuntu VM | systemd | `kernel` | journal |
| pihole | Debian 12 LXC guest on Proxmox | systemd 252 | `container-mount` | journal |
| Public-Nginx | Alpine 3.22 LXC guest on Proxmox | OpenRC | `container-mount` | `logs-unavailable` (the shipped script uses `output_logger`) |

## Before you start

On each host:

1. **The release candidate.** A signed release candidate of this branch,
   deployed with `nexus nexusd update`, the established rollout path. Record
   `nexusd -version` here.
2. **The provider.** `providers.services.enabled: true` (and the host
   provider, `providers.host.enabled: true`, for the failed-set check), set
   by editing `/etc/nexus/nexusd.yaml` on the host, checked with `sudo
   nexusd validate-config`, applied with a reload. Leave
   `providers.services.reboot.enabled` **false**.
3. **The test service.** `sh services-check.sh --print-test-service
   nx-live-test` prints, for the host's init system, the unit (systemd: a
   `Type=simple` unit running `/bin/sleep infinity`) or the init script
   (OpenRC: `supervise-daemon` running `/bin/sleep 86400`), the allow-list
   entry to add under `providers.services.control`, and the removal steps.
   The script only prints them; run them by hand, as root, then
   `validate-config` and reload.
4. **The nexus CLI and the script.** Copy the release's linux `nexus`
   binary and `test/live/services-check.sh` to the same directory on the
   host (the script finds `./nexus` next to itself, or pass `--nexus PATH`).
5. **A token**, created on the host and revoked afterwards. It needs no
   `services:reboot`, because nothing reboots:

   ```
   nexusd token create --name services-check --scope services:read --scope services:logs --scope services:write --scope host:read --scope audit:read --expires 1d
   nexusd token revoke services-check
   ```

   The script drops `NEXUS_TOKEN` from its environment at once (it runs
   itself again with the token on file descriptor 3, as `host-check.sh`
   does), pins the certificate to `nexusd tls fingerprint`, and checks at
   the end that the token is in neither a child's environment nor the
   transcript.

## WI 6.0: captures

The read-only capture script, `internal/svcctl/testdata/services/capture.sh`,
on each host as root:

```
sudo sh capture.sh home-server /tmp/svc-capture-home-server
sudo sh capture.sh pihole /tmp/svc-capture-pihole
sudo sh capture.sh public-nginx /tmp/svc-capture-public-nginx
```

Copy each output to `internal/svcctl/testdata/services/<host>/` (each with
its `PROVENANCE`, `source: host`), then run TC-SV-L0's test command. Record
here, per host:

| Host | Capture date | `init-version` | TC-SV-45 | A-ledger findings |
| --- | --- | --- | --- | --- |
| home-server | | | PENDING | |
| pihole | | | PENDING | |
| Public-Nginx | | | PENDING | |

The A-ledger rows TC-SV-L0 reads: A-3 (pihole's and Public-Nginx's boot IDs
differ; a boot-ID mount point in `proc/1/mountinfo` or nexusd's own table),
A-5 (Public-Nginx's `/sbin/reboot` is BusyBox and its inittab runs
`/sbin/openrc shutdown`), A-8 (`options/nexusd/child_pid`), A-9 (the `net`
provider), A-11 (home-server's cgroup line), A-22 (pihole's journalctl
accepts `--since=@s.us`), A-39 (`StopPropagatedFrom=` empty on home-server
and pihole).

## WI 6.1: the live check

On each host, as root, from the directory holding `nexus` and the script:

```
sh services-check.sh --test-service nx-live-test
NEXUS_TOKEN=<token> sh services-check.sh --test-service nx-live-test --yes
```

The first command records the facts and prints the plan; it changes
nothing and exits 2. The second runs the check and writes
`./services-check-<hostname>-<UTC time>.txt` (or `--transcript FILE`).
Copy that file back and paste its summary into the host's section below;
commit the full transcript next to this page as
`services-live/<host>.txt`. Run pihole first: its (f) row gates WI 6.2.

Exit status: 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or `--yes` is missing); 3 nothing failed but a check
was not run.

The script, per host, in order (each row is one `RESULT` line):

| Result | TC | What is compared |
| --- | --- | --- |
| `L3-capabilities` | TC-SV-L3 | `nexus capability get --refresh`: status `enabled`, `initSystem`, `logSource`, `rebootCommand`, `selfService` against the service whose cgroup (systemd) or `child_pid` (OpenRC) holds nexusd's PID, `dependencyResolution complete`, and `protected` holding the built-in list, the self service and every dependency read from `systemctl show -p Requires -p BindsTo -p Requisite -p PartOf` or `/run/openrc/deptree` (the `net` provider on Public-Nginx is logged) |
| `L3-reboot-disabled` | | `rebootEnabled: false`, as chosen |
| `F-bootid` | WI 6.1 (f) | `grep -F boot_id /proc/1/mountinfo` and nexusd's own table, `cat /proc/sys/kernel/random/boot_id` (recorded for WI 6.2), against `bootIdSource` and `rebootAvailable`; no `rebootUnavailableReason` naming PID 1's mount |
| `L1-list` | TC-SV-L1 (1) | every page of `nexus service get` against `systemctl list-units` plus `list-unit-files`, or `rc-status -s -C` plus `rc-update show -v`, by the TC-SV-46 or TC-SV-49 mapping; one re-read of both when they differ |
| `L1-failed` | TC-SV-L1 (4) | the list's failed set against `nexus hosthealth get`'s `failedServices` (`.service` subset) |
| `L1-get` | TC-SV-L1 (2) | `nexus service get nexusd` against `systemctl show` or `rc-service nexusd status` and `child_pid` |
| `L1-logs` | TC-SV-L1 (3) | systemd: `nexus servicelog get nexusd --tail 20` against `journalctl -u nexusd -o cat`, in order; OpenRC: 422 `logs-unavailable` |
| `L5-refusals`, `L5-unchanged`, `L5-audit` | TC-SV-L5 | `restart nexusd`, `stop ssh`, `stop sshd` (systemd), `stop <selfService>`: each 409 `protected-service`, the init system's view of each and the `nexus-op-*` and operation counts byte-identical before and after, one `refused` audit record each |
| `L4-1-start` to `L4-4-start-twice` | TC-SV-L4 | start, restart, stop, start, start of the test service, each read back with `systemctl show` / `is-active` or `rc-service status` and `child_pid`: commanded state equals observed state, a new main PID after the restart, and `changed: false` with an unchanged start time on the second start |
| `L2-list`, `L2-get`, `L2-logs` | TC-SV-L2 | 20 wall-clock timed calls each: p95 list < 1 s, get < 300 ms, logs `tail=200` < 2 s (systemd) |
| `L13-audit` | TC-SV-L13 | this run's token's `services` audit records since the start: `Succeeded` per action sent, `refused` per refusal, `requested` and `completed` per log read, none for list or get |
| `L6-journal` | TC-SV-L6 | systemd: `journalctl -u nexusd -u 'nexus-op-*'` and `journalctl -k` since the start: no `SECCOMP`, `Operation not permitted`, `Permission denied` or `audit: type=1326` line |

## What the script touches

- **It never reboots.** No reboot, `nexus machine restart` or reboot
  what-if is sent; `/etc/nexus/nexusd.yaml` is never changed.
- **Every host command reads.** `systemctl show`, `list-units`,
  `list-unit-files`, `is-active`, `journalctl`, `rc-status`, `rc-update
  show`, `rc-service … status`, and files under `/proc`, `/run/openrc` and
  `/etc/init.d`.
- **The only state it changes** is the test service named by
  `--test-service`, through `nexus service start|stop|restart`, and only
  after it checked that the service is allow-listed with all three actions
  and status `ok`, is not on the built-in protected list or in
  capabilities' `protected`, is not `nexusd`, `ssh`, `sshd` or the self
  service, and that its unit's `ExecStart` (or its init script, with
  `supervise-daemon`) runs `sleep`. Otherwise `L4-control` fails and
  nothing is sent. The service is left in the state it was found in.
- **The FR-9 probes** (`restart nexusd`, `stop ssh`, `stop sshd`, `stop
  <selfService>`) are requests nexusd must refuse. Each is sent only after
  capabilities list the name as protected and the allow-list does not name
  it, so nexusd's own allow-list check would refuse it as well.

## Results: home-server

- Release candidate: `nexusd -version` = _pending_
- Transcript: _pending_ (`services-live/home-server.txt`)
- Run: `NEXUS_TOKEN=… sh services-check.sh --test-service nx-live-test --yes`, exit _pending_

| Result | Status | Detail |
| --- | --- | --- |
| L3-capabilities | PENDING | expected `systemd`, `journal`, `nexusd`, `/usr/bin/systemctl reboot` |
| L3-reboot-disabled | PENDING | |
| F-bootid | PENDING | expected `bootIdSource: kernel`, no boot-ID mount in PID 1's table |
| L1-list | PENDING | |
| L1-failed | PENDING | |
| L1-get | PENDING | |
| L1-logs | PENDING | |
| L5-refusals / L5-unchanged / L5-audit | PENDING | |
| L4-1 to L4-4 | PENDING | |
| L2-list / L2-get / L2-logs | PENDING | |
| L13-audit | PENDING | |
| L6-journal | PENDING | |

## Results: pihole

Run first: its `F-bootid` row gates WI 6.2.

- Release candidate: `nexusd -version` = _pending_
- Transcript: _pending_ (`services-live/pihole.txt`)
- Run: `NEXUS_TOKEN=… sh services-check.sh --test-service nx-live-test --yes`, exit _pending_
- Root-shell boot ID (for WI 6.2): _pending_

| Result | Status | Detail |
| --- | --- | --- |
| L3-capabilities | PENDING | expected `systemd`, `journal`, `nexusd`, `/usr/bin/systemctl reboot` |
| L3-reboot-disabled | PENDING | |
| F-bootid | PENDING | expected PID 1's bind of `/.lxc-boot-id`, nexusd's own table **without** it (systemd 252), `bootIdSource: container-mount`, `rebootAvailable: true` |
| L1-list | PENDING | |
| L1-failed | PENDING | |
| L1-get | PENDING | |
| L1-logs | PENDING | |
| L5-refusals / L5-unchanged / L5-audit | PENDING | |
| L4-1 to L4-4 | PENDING | |
| L2-list / L2-get / L2-logs | PENDING | |
| L13-audit | PENDING | |
| L6-journal | PENDING | |

## Results: Public-Nginx

- Release candidate: `nexusd -version` = _pending_
- Transcript: _pending_ (`services-live/public-nginx.txt`)
- Run: `NEXUS_TOKEN=… sh services-check.sh --test-service nx-live-test --yes`, exit _pending_
- Root-shell boot ID (for WI 6.2): _pending_
- The `net` provider nexusd's deptree entry resolves to (the first real
  `net` resolution; expected `networking`): _pending_

| Result | Status | Detail |
| --- | --- | --- |
| L3-capabilities | PENDING | expected `openrc`, `openrc-files`, `nexusd`, `/sbin/reboot`, `protected` holding the `net` provider |
| L3-reboot-disabled | PENDING | |
| F-bootid | PENDING | expected PID 1's bind of `/.lxc-boot-id` and the same line in nexusd's table (no sandbox), `bootIdSource: container-mount`, `rebootAvailable: true` |
| L1-list | PENDING | |
| L1-failed | PENDING | |
| L1-get | PENDING | |
| L1-logs | PENDING | expected 422 `logs-unavailable` |
| L5-refusals / L5-unchanged / L5-audit | PENDING | |
| L4-1 to L4-4 | PENDING | |
| L2-list / L2-get | PENDING | `L2-logs` is INFO on OpenRC |
| L13-audit | PENDING | |
| L6-journal | n/a | OpenRC: no systemd sandbox |

## NFR-1 timings

Wall clock per call, a new `nexus` process each time (TLS handshake
included), 20 calls; copy the `L2-*` lines. The figures also go into
[services.md](../services.md#measured-timings) once measured.

| Host | list p95 (< 1 s) | get p95 (< 300 ms) | logs `tail=200` p95 (< 2 s) |
| --- | --- | --- | --- |
| home-server | pending | pending | pending |
| pihole | pending | pending | pending |
| Public-Nginx | pending | pending | n/a (logs unavailable) |

## WI 6.2 and 6.3: the reboots (owed)

**Owed — user chose to skip real reboots on 2026-11-18.** None of the
following has run, and `services-check.sh` does none of it:

- **WI 6.2** (TC-SV-L7 to L10, L12): the pre-reboot LXC checks on pihole and
  Public-Nginx, the what-if boot ID against the root shell's (pihole), the
  reboot enabled by a file edit, `nexus machine restart --confirm-host` on
  each guest with the 202, trigger and `upAt` times, a what-if and a
  delay-window cancel on one guest, and the timings in
  [services.md](../services.md#measured-timings).
- **WI 6.3** (TC-SV-L11, L12): the guarded reboot of home-server in a
  maintenance window the user approves, with the kernel change, host health
  before and after, and every Compose stack's containers back.
- **TC-SV-L13's reboot rows**: `requested` with `confirmHost`, `force` and
  `bootId`, and a terminal record with `previousBootId` and `bootId`.

Until they run, the reboot stays disabled on every host, and the feature's
reboot path has only container (T1) evidence
([services-t1.md](services-t1.md)).

## Container dry run

**Not real-host evidence.** `services-check.sh` (sha256
`10fd84d6bfb9e6b70f10cf01d56eea4592cadb8f3d822e8346031b4f2c3c9869`) was run
in the T1 harness images `nexus-harness:debian-12` (systemd 252) and
`nexus-harness:alpine-alpine-3.22` (OpenRC 0.62.6), on linux/arm64, with
nexusd and nexus built from a clean `git archive` of 7b2834e and installed by
the release tree's `install.sh`. The test service was installed from the
script's own `--print-test-service` output, and a per-container boot ID was
bind-mounted over `/proc/sys/kernel/random/boot_id` before nexusd started,
as T1 does, to stand in for LXC's.

| Run | Debian 12 | Alpine 3.22 |
| --- | --- | --- |
| No `--yes`: facts and plan | exit 2, nothing changed | exit 2, nothing changed |
| Full check with `--test-service nx-live-test` | exit 1: every row PASS except `host FAIL` (the Docker marker every container run records) | exit 1: every row PASS except `host FAIL`; `L2-logs` and `L6-journal` INFO (OpenRC) |
| No `--test-service` | `L4-control NOT-RUN` | `L4-control NOT-RUN` |
| `--test-service nx-unlisted` (not allow-listed, not installed) | `L4-control FAIL`, nothing sent | `L4-control FAIL`, nothing sent |
| No token | exit 2, refused | exit 2, refused |

In both, the test service was left stopped, as found, and the token
appeared in no transcript or log. On Debian 12 nexusd's own mount table had
no boot-ID line and `bootIdSource` was still `container-mount` (pihole's
expected case); on Alpine nexusd's table had PID 1's line. Every nexusd
journal in the dry run shows one `http: TLS handshake error … bad
certificate` warning at the start of each run, when the script's `nexus
host add --fingerprint` connects; it is not a sandbox denial and does not
trip `L6-journal`.

## Remote readbacks from the dev machine (2026-11-19, v0.1.0-rc6 at 2e28645)

Not the operator check: these are reads through the `nexus` CLI with a `services:read`+`services:logs` token (`claude-services`, 90 d), after `nexus nexusd update` (pihole then home-server, both `Succeeded`, `rolledBack false`) and `nexus nexusdconfig update providers.services.enabled=true` (both `Succeeded`, no restart required). Public-Nginx (OpenRC) is still on rc5: it cannot self-update, and config set is unavailable there.

| Host | Services listed | States | `bootIdSource` | `rebootAvailable` / `rebootEnabled` | `dependencyResolution` | Logs |
|---|---|---|---|---|---|---|
| pihole | 148 | 30 running, 108 stopped, 10 unknown | `container-mount` | true / false | complete | journal; `pihole-FTL` tail 3 read |
| home-server | 372 | 82 running, 267 stopped, 23 unknown | `kernel` | true / false | complete | journal; `nexusd` tail 3 read |

- pihole's `container-mount` is the live confirmation of the systemd 252 boot-ID fix (D-11, `detach.ReadBoot`): rc5-era code would have read the host kernel's ID there. WI 6.1 (f)'s root-shell comparison (`/proc/1/mountinfo`, `cat boot_id`) is still the operator's.
- `nexusd`, `ssh` and `systemd-journald` are `protected: true` on both; with an empty allow-list nothing is controllable.
- The last 1000 lines of nexusd's own log on each host hold no `SECCOMP`, `Operation not permitted`, `Permission denied` or `ERROR` line (a partial look at A-1/A-2; the operator's journalctl check (e) is authoritative).
