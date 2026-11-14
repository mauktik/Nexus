# nginx-provider WI 7.1: T1 nginx runs on Ubuntu/systemd and Alpine/OpenRC

Behavioral evidence for work item 7.1 (`cross-platform-matrix`). Both runs
use **harness containers**: a privileged container on the local Docker host
(colima, linux/arm64), started by `test/harness/run.sh` with
`NAME_PREFIX=nx-wi71` and `PARITY=1`. They are not physical hosts; WI 7.2
covers the live real-host checks.

Both runs used a clean `git worktree` checked out at **b527bf9**
(`test(harness): WI 7.1 — integration.test bound 60m; the Ubuntu suite
outgrew 30m`). The test code is from 56c8848 (`test(nginx): implement work
item 7.1`). They ran on 2026-11-14.

| Run | Command | Image | Result |
| --- | --- | --- | --- |
| Ubuntu | `make test-harness` | `ubuntu:latest` = Ubuntu 26.04.1 LTS, systemd 259, nginx 1.28.3 (Ubuntu), curl 8.18.0 | `harness: exit 1`: one failure, `TestNFRIdleFootprint` (TC-45), not an nginx test. See [the Ubuntu failure](#the-ubuntu-failure-tc-45-cpu-factor) |
| Alpine | `make test-harness-alpine` | `alpine:latest` = Alpine Linux v3.24, OpenRC 0.63.2, nginx 1.30.6, curl 8.23.0, Docker 29.9.3 | `harness: exit 0` |

The complete logs are in [`nginx-t1/`](nginx-t1/):

- `ubuntu/make-test-harness.log` and `alpine/make-test-harness-alpine.log`
  hold run.sh's full output. Each includes the whole
  `integration.test -test.v` output, which is also in `test.log`.
- The parity suites as root (`detach.log`, `ops.log`) and as an
  unprivileged user (`*-nonroot.log`), and TC-46's check (`parity-check.log`).
- `svcctl-live.log` and nginx's own `nginx-error.log`.
- On Alpine, `rc.log`, `messages.log` and `rc-status.log`. On Ubuntu,
  `units.log`. The Ubuntu journal (14 MB) was collected but is not committed.

## Results

| | Ubuntu (systemd) | Alpine (OpenRC) |
| --- | --- | --- |
| integration.test | 123 PASS, 11 SKIP, **1 FAIL** (`TestNFRIdleFootprint`) | 83 PASS, 52 SKIP, 0 FAIL |
| nginx T1 suite (TC-106 to TC-117) | 12 of 12 PASS | 10 PASS. TC-112's two tests skip with `requires systemd (detected openrc)` |
| TC-182 with every nginx op | PASS | gated (systemd) |
| internal/detach as root | 48 PASS, 1 SKIP | 49 PASS |
| internal/ops as root | 124 PASS | 124 PASS |
| internal/detach as non-root | 43 PASS, 6 SKIP | 44 PASS, 5 SKIP |
| internal/ops as non-root | 124 PASS | 124 PASS |
| TC-46 parity check | FAIL, only because test.log holds the `TestNFRIdleFootprint` FAIL | PASS |
| svcctl TestLiveMatrix | PASS | PASS |

Every Ubuntu SKIP is a `requireInit(openrc)` gate, plus
`TestHarnessFailurePathIsCollected`, which runs only when asked. Every
Alpine SKIP is a `requireInit(systemd)` or `requireApt` gate, plus the same
opt-in test.

### The Ubuntu failure: TC-45 CPU factor

The failure is `--- FAIL: TestNFRIdleFootprint (417.72s)` at b527bf9
(`ubuntu/test.log`, line 705). Only TC-45's CPU-factor assertion failed.
The wakeup bound (63 added against 60 polls), the idle RSS (max 21.47 MiB),
the idle CPU (0.0017%) and startup (p95 269.7 ms) all passed. The line is:

```text
footprint_test.go:824: TC-45: nexusd's CPU time over the one-op window is 9.259ms, 0.40x the systemd launcher's mean 23ms on Ubuntu (-nfr.systemd-op-cpu), want within a factor of 2 (TC-45 erratum: one window's spread on one harness is past 10%)
footprint_test.go:824: NFR TC-45 one op adds 63 wakeups (2.10/s, 1.05 per poll) and 794 context switches (13.23 per poll; the reference 8.68 per poll); CPU over the op window 9.259ms (9.259ms over idle), 0.40x the systemd launcher's mean 23ms
footprint_test.go:637: NFR load at start: VM loadavg 6.40 5.11 3.66 9/367 57989, 4 CPUs
```

The bound comes from 9c2e6c5 (WI 3.4's TC-45 fix), which landed after
56c8848. The value is below the bound's floor (11.5 ms). WI 3.4's eight
Ubuntu windows ran from 17.2 ms to 29.5 ms (`wi-3.4-harness.md`).

Why WI 7.1's changes cannot plausibly move this value:

- **nginx enabled in a configuration.** TC-45's nexusd is `nfrStart`
  (`test/integration/footprint_test.go:142` at b527bf9). It configures
  docker, apt and scripts only. The nginx provider is not configured in
  that daemon, so it runs no nginx preflight, `nginx -v` or status work.
  WI 7.1 enables nginx only in its own daemons and in
  `TestEveryProviderOpUnderExactUnit`.
- **Test order.** `TestNFRIdleFootprint` runs at test.log line 686, before
  TC-182 (line 847) and every nginx test (line 1064 onward).
- **The nginx package.** Ubuntu's image now carries the distribution's
  `nginx.service`, enabled. It is idle during the window: a master and
  four workers serving only the default site. They are other processes,
  and the assertion measures nexusd's own CPU. Extra contention would
  more plausibly raise the cost per poll than cut it to 0.40x.
- **The 60m bound.** It changes only integration.test's `-test.timeout`.

What differs is the load: the Ubuntu window ran at VM loadavg 6.40 on 4
CPUs, against 1.02 for Alpine's passing window at the same SHA (15.091 ms,
0.66x). Another agent's harness ran on the same VM during this work. The
test.log with the failure is committed unchanged. footprint_test.go was not
touched; it belongs to the TC-45 fixer.

## nginx readings (both platforms, from test.log)

Each value is read back without Nexus: the init system's CLI, `pidof` and
/proc, `ls -ln`, `sha256sum`, `curl` on the site's own port, and the journal.

- **TC-106, lifecycle.** For stop, start, restart and reload, the init
  system and /proc match the commanded state and the Nexus result's
  `state` and `mainPid`. Restart changes the master; reload keeps it and
  replaces every worker.
  - Step 3, the package-upgrade model: `readlink /proc/<master>/exe`
    printed `"/usr/sbin/nginx (deleted)"` on both. This is the source of
    the TC-ERR15 row 4 value. The reload after it was `confirmed`, with
    the master unchanged and new workers.
- **TC-107, toggle.** `t1.conf` is on disk and `curl :18081` returns the
  marker after enable. After disable, `t1.conf.disabled` is on disk and
  curl exits 7 (connection refused).
  - FR-10 with the upstream pair: disabling `t1def` fails
    `nginx-config-invalid` with `rolledBack: true` and
    `host not found in upstream "nexus_t1_up"`. includeDir is unchanged and
    the proxy on :18087 still serves.
- **TC-108, write.** `sha256sum` equals the result's `sha256`, which equals
  the SHA-256 of the content sent, for both add and update. The file is
  `0:0 0644` and curl returns M2.
- **TC-109, rollback.**
  - `invalid.conf` fails `nginx-config-invalid` with `rolledBack: true`.
  - With port 18090 held, `port-conflict.conf` fails `nginx-reload-failed`
    with `rolledBack: true`, `observed.verdict: rejected` and
    `rollbackReload.verdict: confirmed`. The error log shows
    `bind() to 0.0.0.0:18090 failed (98: Address in use)`.
  - After each, includeDir's names, modes, owners and hashes equal the
    snapshot, and curl returns M1.
  - **ASM-7 on the real hosts: `observed.initExitCode` is 0 on both**
    (`systemctl reload` and `rc-service nginx reload` exit 0 when the
    master rejects the bind).
- **TC-110, restart cases.**
  - **A:** the helper was alive across `systemctl restart nexusd` and
    `rc-service nexusd restart`. The op ended `Succeeded`, and
    `sha256sum` equals the new content's hash.
  - **B:** the supervisor got `kill -9` after `previous/meta.json`. The op
    ended `outcome-unknown`. The op log holds
    `previous content of t4.conf is kept at /var/lib/nexus/nexusd-tc110/ops/<id>/previous/content`,
    and that file's sha256sum equals the pre-op hash.
  - **C:** an update queued behind a running test op ended
    `outcome-unknown` with `never started: its sensitive input was not
    persisted across the nexusd restart`. includeDir is unchanged.
  - The window is widened with the TC-115 wrapper (a 6-10 s delay on
    `-t` only). The 2,000-include alternative is too fast: real `nginx -t`
    with `include` of 2,000 files took 8-9 ms on Ubuntu and about 10 ms
    on Alpine (20 runs in 0.19 s), measured in the dev containers.
- **TC-111, whatIf.** For both the valid and the invalid content, `valid`
  equals a manual `nginx -t`. That manual run used a copy of includeDir
  bind-mounted in a private `unshare -m` namespace. `/proc/1/mountinfo`
  is byte-identical (4560 bytes on Ubuntu, 3866 on Alpine), the hashes are
  identical, and the master and workers are unchanged.
- **TC-112, exact unit (Ubuntu).**
  - All 13 nginx ops ran under the shipped unit with no drop-in. The test
    op returned `valid: true`, and status `version` 1.28.3 equals
    `nginx -v` run as root.
  - The window of `nexusd.service` and 11 `nexus-op-*` units holds no
    `SECCOMP`, `audit: type=1326`, `Permission denied` or `Read-only file
    system` line. nexusd's MainPID was unchanged with NRestarts 0.
  - TC-182 passed with all four providers enabled and every nginx op.
- **TC-113, fakes against the real systems.** On each init system, every
  scenario's real exit code equals svcctltest's: reload with an invalid
  config (1/1), with a bound port (0/0), valid (0/0); start when started
  (0/0); status of a stopped service (`show`: 0/0; `status`: 3/3); an
  unknown service (`show`: 0/0 `LoadState=not-found`; `--exists`: 1/1).
  Each real output holds the fake's phrases, and the real generation
  moved exactly when the fake's model did.
  - **Step 3, read-only error log, and a fake-fidelity finding.** Both
    builds fail `nginx -t` (exit 1) with `[emerg] open()
    "/var/log/nginx/error.log" failed (30: Read-only file system)`.
    Alpine's real output also carries fakenginx's
    `nginx: [alert] could not open error log file: open()
    "/var/lib/nginx/logs/error.log" failed (30: Read-only file system)`
    byte for byte.
  - Ubuntu builds nginx with `--error-log-path=stderr` (`nginx -V`), so it
    has no compiled-in log file to open and prints **no `[alert]` line**.
    fakenginx always prints it, in Alpine's form. The verdict and the
    `[emerg]` line match; fakenginx's header should record this Ubuntu
    difference.
- **TC-114, latency and footprint (50 sites: 25 enabled, 25 disabled).**

  | | Ubuntu | Alpine |
  | --- | --- | --- |
  | status p95 (200 requests) | 3.98 ms | 12.16 ms |
  | list p95 | 0.84 ms | 2.40 ms |
  | get p95 | 0.17 ms | 0.23 ms |
  | 202 of the test op (max of 5) | 34.6 ms | 4.1 ms |
  | idle RSS, nginx disabled / enabled (60 s each) | 17.21 / 19.56 MiB | 17.18 / 19.10 MiB |
  | NFR-5 RSS delta | **2.36 MiB** | **1.92 MiB** |
  | threads, CPU over the idle window | 9 -> 9, 0 ticks | 10 -> 10, 0 ticks |

- **TC-115, isolation (testTimeout 5 s).**
  - Stopped: status is 200 `stopped`.
  - Broken (`invalid.conf` placed by root): the test op is `valid: false`.
  - Hung (the wrapper's `-t` sleeps): the test op failed
    `dependency-unavailable` with `nginx -t did not finish within 5s`,
    after 5.34 s (Ubuntu) and 5.40 s (Alpine). No `sleep` was left behind.
  - In every state, scripts list, docker list and capabilities answered
    200 in under 200 ms, and nginx stayed `enabled`.
  - The real `/usr/sbin/nginx` was restored with its sha256sum checked.
- **TC-116, survives the op's end.** The master was alive 5 s after
  `nexus nginx start`, and the init system says running.
  - Its cgroup is `0::/openrc.nginx` on Alpine, not under `nexus-ops/`
    (L-41), and `nexus-ops/<id>` is gone. On Ubuntu it is
    `system.slice/nginx.service`, and the op unit is `not-found`.
  - `Umask` is `0000` after a login-shell start and after the Nexus start,
    on both, while nexusd's own is 0077 on Alpine. This equality cannot
    show L-47: nginx's `ngx_daemon()` calls `umask(0)` after `setsid()`
    (`src/os/unix/ngx_daemon.c`), so a daemonized master always shows
    0000.
- **TC-117, root ownership.** `own.conf` (root) is managed. `user.conf` and
  `userd.conf.disabled` (owned by `nobody`) are `managed: false` with
  `not-root-owned`. Enabling either is refused 422
  `nginx-config-unmanaged`, includeDir is unchanged and no operation
  exists.

## How the runs were made

- The test subset was iterated in two persistent dev containers built from
  the same Dockerfiles (`-run TestNginx`), not by repeating full harnesses.
- A full run at 56c8848 ended in two ways:
  - Alpine: `exit 0`.
  - Ubuntu: integration.test hit run.sh's own `-test.timeout=30m` inside
    `TestNFRDependencyFailureMatrix`, with 1,714 s of tests finished
    before nginx_test.go ran.
- b527bf9 raised that bound to 60m, and both harnesses ran once more there.
  Those runs' logs are the ones committed.
