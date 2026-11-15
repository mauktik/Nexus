# system-provider WI 5.3: T1 host provider runs on Ubuntu/systemd and Alpine/OpenRC

Behavioral evidence for work item 5.3 (`real-device-exchange` and
`cross-platform-matrix`). Both runs use **harness containers**: a privileged
container on the local Docker host (colima, linux/aarch64), started by
`test/harness/run.sh` through the make targets with the default
`PARITY=1`. They are not physical hosts.

Both runs used the shared checkout at **c97deeb** (`test: implement work
item 5.3 — host provider T1 on real systemd and OpenRC`). They ran on
2026-11-15 (UTC).

| Run | Command | Image | Result |
| --- | --- | --- | --- |
| Ubuntu | `make test-harness RUN='^TestTCHP\|^TestEveryProviderOpUnderExactUnit'` | `ubuntu:latest` (`sha256:6a06111c3b0a…`) = Ubuntu 26.04.1 LTS, systemd 259 (259.9-0ubuntu0.1); harness image `sha256:7435fd8cd66a…` | `harness: exit 0`, `make` exit 0 |
| Alpine | `make test-harness-alpine RUN='^TestTCHP'` | `alpine:latest` (`sha256:f59abe656b69…`) = Alpine Linux v3.24 (3.24.4), OpenRC 0.63.2; harness image `sha256:da04a04680b2…` | `harness: exit 0`, `make` exit 0 |

nexusd ran under the unchanged shipped units: on Ubuntu, install.sh
installed `deploy/nexusd.service` and every systemd test checks that the
installed file is byte-equal to it, with no drop-in (`requireExactUnit`;
sha256 `5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a`
on both, logged by each test). On Alpine, install.sh installed
`deploy/nexusd.openrc`. Neither file changed in this work item.

The complete logs are in [`host-t1/`](host-t1/):

- `ubuntu/make-test-harness.log` and `alpine/make-test-harness-alpine.log`
  hold run.sh's full output, including the whole `integration.test -test.v`
  output, which is also in `test.log`.
- `ubuntu/host-unavailable-systemd.txt` and
  `alpine/host-unavailable-openrc.txt`: TC-HP-115's and TC-HP-122's
  (op, JSON path, reason) lists. run.sh now copies `/harness/out` into the
  run's log directory to collect them.
- The parity suites, TC-46's check, `svcctl-live.log`; `journal.log` and
  `units.log` on Ubuntu; `rc.log`, `messages.log` and `rc-status.log` on
  Alpine.
- `red-private-root/`: the earlier TC-HP-117 run that failed on the
  harness's private `/` (see [A-13 in a container](#a-13-in-a-container)).

## Results

| | Ubuntu (systemd) | Alpine (OpenRC) |
| --- | --- | --- |
| integration.test | 13 PASS, 4 SKIP, 0 FAIL | 5 PASS, 11 SKIP, 0 FAIL |
| internal/detach as root / non-root | 59 PASS 1 SKIP / 54 PASS 6 SKIP | 60 PASS / 55 PASS 5 SKIP |
| internal/ops as root / non-root | 97 PASS / 97 PASS | 97 PASS / 97 PASS |
| TC-46 parity check | PASS | PASS |
| svcctl TestLiveMatrix | PASS | PASS |

The Ubuntu SKIPs are TC-HP-122 to TC-HP-124 (`requires openrc (detected
systemd)`) and TC-HP-131, WI 5.4's stub. The Alpine SKIPs are the
systemd-only TCs (`requires systemd (detected openrc)`) and TC-HP-131.

| TC | Test | Ubuntu | Alpine | What was observed |
| --- | --- | --- | --- | --- |
| TC-HP-114 | `TestTCHP114_EveryOpUnderTheExactHardenedUnit` | PASS | gated | hostname, os-release ID/VERSION_ID/PRETTY_NAME, `uname -r`/`-m`, `nproc` exact; memory total = `free -b`, available within 1%; `/`, `/dev/shm`, `/run`, `/tmp`, `/var/lib/docker` sizes = `df -a -B1`, used within the df bracket ± 1 block; vda, vdb, vdc size/rota/rm = `lsblk -J`; every interface's addresses = `ip -j addr`; gateways = `ip -j route` |
| TC-HP-115 | `TestTCHP115_…` | PASS | gated | 15 unavailable fields, all `not reported:` (no CPU model or frequency, no sensors, no disk model, no link speed on docker0 and lo, no IPv6). **None is `permission denied:` or `command failed:`**: the sandbox makes no field unavailable here (input for WI 5.4's TC-HP-131) |
| TC-HP-116 | `TestTCHP116_…` | PASS | gated | after three rounds of the ten ops: MainPID unchanged, `NRestarts=0`, `active`; no denial line in the journal window; no new kernel line; the window holds `nexusd ready` and `starting background provider` `provider=host` |
| TC-HP-117 | `TestTCHP117_…` | PASS | gated | `readOnly` read for every item, `/` false; `/mnt/tc-hp` mounted after the start listed as ext4 with df's size (27234304); `/tmp/tc-hp-hidden` listed ext4 with null usage and `not visible: /tmp/tc-hp-hidden is hidden in nexusd's mount namespace`; `nsenter … stat` gave ENOENT; after `nsenter … mkdir` (statfs there reaches the private tmpfs) still not visible with null usage |
| TC-HP-118 | `TestTCHP118_…` | PASS | gated | `nx-host-failed.service` in `failedServices.names`; count 2 = `systemctl --failed` lines (the harness's own `systemd-modules-load.service` is the other) |
| TC-HP-119 | `TestTCHP119_…` | PASS | gated | marker present: `true`, packages `[linux-image-tc-hp]`; removed: `false` (needrestart's `Ubuntu.pm` is installed); apt's capabilities agreed both times (apt enabled in this test) |
| TC-HP-120 | `TestTCHP120_…` | PASS | gated | burner in `topByCpu` after 3.9 s: `sh`, root, 99.6%; `ps -o pcpu=` 99.7 |
| TC-HP-121 | `TestTCHP121_…` | PASS | gated | disabled: all ten ops 503 `capability-unavailable`; `stopping background provider` 29 ms after the reload; re-enabled: warming-up, cpu `usagePercent` null `warming up`, 1 point (see [deviations](#deviations-from-the-test-cases)); 2 intervals later ready; MainPID unchanged |
| TC-HP-122 | `TestTCHP122_…` | gated | PASS | the TC-HP-114 comparisons with BusyBox `df -a -B1 -P` and `/sys/block` (vda, vdb, vdc); walker passed, 19 fields; `rc-service nexusd` started with the same PID |
| TC-HP-123 | `TestTCHP123_…` | gated | PASS | after `kill -9`, `rc-status --crashed` = `[nx-host-crash]`; `failedServices` count 1, names `[nx-host-crash]`, source openrc; `/bin/rc-status` exists |
| TC-HP-124 | `TestTCHP124_…` | gated | PASS | `rebootRequired` unknown, evidence "in a docker container: the running kernel … is the container host's", `not supported:` reason; virtualization container/docker with `/.dockerenv` present |
| TC-HP-125 | `TestTCHP125_…` (runs TC-182) | PASS | gated | 5 providers enabled; each host op 200; 42 of the catalog's 42 ops of docker, apt, scripts, nginx and host exercised; no denial, MainPID unchanged, directive audit passed. `TestEveryProviderOpUnderExactUnit` then reported this same run |
| TC-HP-126 | `TestTCHP126_…` | PASS | PASS | `initSystem` = `detectedInit()` and `/v1/capabilities` (systemd / openrc); `os.id` = os-release ID (ubuntu / alpine) = capabilities |
| TC-HP-E28 | `TestTCHPE28_…` | PASS | gated | burner as nobody in `topByCpu` after 4.0 s: uid 65534, `nobody`, 100.1% |
| TC-HP-E29 | `TestTCHPE29_…` | PASS | PASS | systemd: `synchronized` `true` = `timedatectl` `yes`, no SIGSYS, no restart; OpenRC (no chronyc): `unknown`, `not supported: no supported time-sync query (systemd-timedated, chrony)` |

## A-13 in a container

The first TC-HP-117 run failed (`red-private-root/test.log`): `/mnt/tc-hp`,
mounted on the host after nexusd started, was listed `not visible:` with
null usage. The evidence in that log:

```text
/proc/1/mountinfo: 280 304 7:0 / /mnt/tc-hp rw,relatime - ext4 /dev/loop0 rw
/proc/491/mountinfo: no line at /mnt/tc-hp or /mnt
nsenter -m -t 491 stat -f /mnt/tc-hp: <nil> "overlayfs 15402443 blocks of 4096"
```

The mount never reached nexusd's namespace, because the container's `/` is
`private` (no `shared:` tag; `findmnt -o PROPAGATION /` says private). On a
host, systemd makes `/` shared at boot, but not in a container:
`src/shared/mount-setup.c` guards the `MS_REC|MS_SHARED` mount of `/` with
`if (detect_container() <= 0 && !skip_setup)`. A-13 holds for hosts, not for
this harness as it boots.

The provider's answer was right, and it is the D-9 safeguard working: a
statfs of `/mnt/tc-hp` in nexusd's namespace would have reported the root
overlay's numbers, and the provider reported none. So TC-HP-117 now makes
`/` rshared before nexusd starts, as on a host, and restores the original
propagation at cleanup. The unit is not changed. With that, nexusd's
`/` is `shared:289 master:264`, and the later mount reaches it
(`/proc/<pid>/mountinfo: 282 336 7:0 / /mnt/tc-hp … master:333`), as A-13
expects. This is not a production bug.

## Deviations from the test cases

- **TC-HP-116 and TC-HP-121, the "sampler started" and "sampler stopped"
  lines.** The provider logs no such lines. The daemon's background manager
  (`internal/daemon/background.go`) logs `starting background provider` and
  `stopping background provider` with `provider=host` around
  `RunBackground`, and the tests read those.
- **TC-HP-121, "first `points 0`".** The sampler samples at once when it
  starts, and that sample is the history's first point. The first call
  after the re-enable came 36 ms after the reload and saw 1 point
  (`historyStart` 06:13:44.167, after the reload), sampler warming-up and
  cpu `usagePercent` null with `warming up`. Points 0 is observable only in
  the window before the first sample. The test asserts at most one point
  and a `historyStart` after the reload (nothing from the earlier run
  survives, ASM-3), and logs the count it sees.
- **TC-HP-119** enables the apt provider too, so "apt agrees (if enabled)"
  is checked rather than skipped.
- **TC-HP-122** reads addresses with `ip -j addr` and routes with
  `ip -j route` (iproute2 in the Alpine image) instead of `ip -o addr`, and
  df with BusyBox `df -a -B1 -P` (exact bytes, not `df -k` × 1024).
- **TC-HP-125**: TC-182 runs once per test process. With both RUN patterns,
  `TestTCHP125_…` runs it and `TestEveryProviderOpUnderExactUnit` reports
  that run's result. Either test alone runs it in full.
- **Harness script**: `test/harness/run.sh` copies `/harness/out` into the
  run's log directory, so TC-HP-115's list lands in
  `build/harness/logs/<stamp>/` as the TC says.
- TC-HP-E28's log line `burner /proc/<pid>/status Uid: 0 …` is read before
  setpriv has switched the user; the provider's entry, taken later, is uid
  65534.
