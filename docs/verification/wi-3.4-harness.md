# WI 3.4: Alpine/OpenRC and Ubuntu/systemd T1 harness runs

Behavioral evidence for work item 3.4 (`real-device-exchange`). Both runs
are **harness containers**: a privileged container on the local Docker
host (colima, linux/arm64), started by `test/harness/run.sh`. They are
not a physical host. Both ran from a clean `git worktree` checked out at
**8fc9001** (`test(harness): WI 3.4 — registry fixture waits for an HTTP
200/401 from /v2/, not an open port`), with `NAME_PREFIX=nx-wi34` and
`PARITY=1` (the default), on 2026-11-14.

| Run | Command | Image | Result |
| --- | --- | --- | --- |
| Alpine | `make test-harness-alpine` | `alpine:latest` = Alpine Linux v3.24 (3.24.4), OpenRC 0.63.2, BusyBox init PID 1, Docker 29.9.3, nginx 1.30.6 | `harness: exit 0` |
| Ubuntu | `make test-harness` | `ubuntu:latest` = Ubuntu 26.04.1 LTS, systemd 259, apt 3.2.0 | `harness: exit 0` |

The logs are under [`wi-3.4/`](wi-3.4/):

- `alpine/make-test-harness-alpine.log` and `ubuntu/make-test-harness.log`
  hold the complete run.sh output. Each one includes the whole
  `integration.test -test.v` output (test.log).
- The NFR-8 parity suites as root (`detach.log`, `ops.log`) and as an
  unprivileged user (`*-nonroot.log`).
- TC-46's check of those logs (`parity-check.log`).
- `internal/svcctl` `TestLiveMatrix` against the container's real nginx
  (`svcctl-live.log`).
- On Alpine, OpenRC's `rc.log`, `/var/log/messages` and `rc-status -a`.
- On Ubuntu, `systemctl list-units` (`units.log`) and the nginx install
  the live matrix needed (`svcctl-nginx-install.log`). The Ubuntu
  journal (13 MB) was collected but is not committed.

## Results

| | Alpine (OpenRC) | Ubuntu (systemd) |
| --- | --- | --- |
| integration.test | 72 PASS, 63 SKIP, 0 FAIL | 112 PASS, 23 SKIP, 0 FAIL |
| internal/detach as root | 44 PASS, 0 SKIP | 43 PASS, 1 SKIP (TestSystemdAvailabilityRefusesSilentDowngrade needs a host without systemd; it passes on Alpine) |
| internal/ops as root | 124 PASS | 124 PASS |
| internal/detach as non-root | 40 PASS, 4 SKIP (root-only) | 39 PASS, 5 SKIP |
| internal/ops as non-root | 124 PASS | 124 PASS |
| TC-46 parity check | PASS | PASS |
| svcctl TestLiveMatrix | PASS (rc-service stop/start/reload/restart/stop, MainPID = /run/nginx/nginx.pid) | PASS (systemctl) |

Every Alpine SKIP is a `requireInit(systemd)` or `requireApt` gate, or an
nginx stub of a later work item. Every Ubuntu SKIP is a
`requireInit(openrc)` gate or an nginx stub. TC-46 checks the gates
against the detected init system and finds no other skip that names root,
cgroups or an init system.

Selected readings from the Alpine log:

- **TC-37:** PID 1 is `/sbin/init` (BusyBox), docker is started in the
  default runlevel, cgroup v2 is writable, and `platform.Current()`
  reports `{Init:openrc OS:{ID:alpine VersionID:3.24.4}}`.
- **TC-38 (L-11):** `pidfile names: supervise-daemon (pid 63501), parent of
  the answering nexusd 63502`. The shipped script's pidfile names the
  supervisor, not nexusd.
- **TC-E14 / issue #910:**
  - With the stock `rc-cgroup.sh` (`issue #910 present: true`), the canary
    is still alive 5 s after `rc-service nexusd stop` with
    `rc_cgroup_cleanup=YES`. On stock OpenRC 0.63.2, YES is a no-op.
  - With upstream 1cf3944's `cgroup_running()` applied in the container:
    - **Case A** (the script's default, NO): the canary survives.
    - **Case B** (YES): the canary in nexusd's cgroup is gone after the
      stop, so cleanup is in effect. Only after that is the supervisor
      counted: it is alive with the same start ticks in
      `/nexus-ops/<id>`, and the op ends Succeeded.
- **TC-45 (Alpine side):**
  - One running op adds 16.6 context switches/s to nexusd, 8.3 per
    watcher poll at 2.0 polls/s, and 0.13% of one core.
  - At idle: 0.0032% CPU over 5 min, RSS max 21.35 MiB.
  - Startup from exec to the ready line: p50 40.4 ms.
- **TC-49:** `/usr/local/bin/nexusd` has no PT_INTERP and no DT_NEEDED, and
  gcompat is absent. The script `runAs nexusrun` ran as `uid=1003`, and an
  unknown runAs was refused with 422 `run-as-user`.

Ubuntu TC-179 to TC-182 ran against a tree with WI 4.5 (4e67de4):
`providers: ["apt=disabled" "docker=disabled" "nginx=disabled"
"scripts=disabled"]`, all four PASS.

## Found while running

- **Kernel (harness host 6.8.0-100-generic):** once OpenRC's cleanup has
  written `cgroup.kill` to `openrc.nexusd`, every clone3
  `CLONE_INTO_CGROUP` into or out of that group is SIGKILLed. This was
  seen with strace and with a `/bin/true` probe. A nexusd started in that
  group afterwards cannot launch any operation (`supervisor exited at
  once`). TC-E14 now removes the group when it finishes. On a real host
  with 1cf3944 and `rc_cgroup_cleanup=YES`, the next start would hit the
  same failure. This needs a product decision (reported to the
  coordinator).
- **Product race (reported, not fixed here):** a SIGTERM sent to a
  process-backend supervisor before its `signal.Notify` kills it with no
  exit.json. The detach TC-16 cleanup now waits for spec.json to be
  consumed.
