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
    watcher poll at 2.0 polls/s, and 0.13% of one core. Superseded:
    context switches are not wakeups, see
    [TC-45: wakeups and CPU](#tc-45-wakeups-and-cpu-spec-check-b1).
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

## TC-45: wakeups and CPU (spec check B1)

The 3.4 spec check failed TC-45 because the test asserted neither of its
bounds. 52bca7d counted context switches, saw 8.3 per watcher poll, and
46f8e2f replaced the bound with NFR-1's idle CPU bound. This section is
the investigation and the restored assertions in
`test/integration/footprint_test.go` (`nfrOpWindowCPU`). The test code
was run from a clean worktree at 1a8aada plus that file; nexusd is the
same as at 1a8aada. `NAME_PREFIX=nx-tc45`, `PARITY=0`,
`RUN='^TestNFRIdleFootprint$'`, on 2026-11-14, colima linux/arm64,
4 CPUs. The logs are under [`wi-3.4/tc45/`](wi-3.4/tc45/).

### What was measured

Per thread, before and after each 30 s window: `voluntary_ctxt_switches`
plus `nonvoluntary_ctxt_switches` from `/proc/<pid>/task/<tid>/status`,
and run time (ns) and timeslices from `/proc/<pid>/task/<tid>/schedstat`.
The kernel's sched-stats document gives the three schedstat fields as
"time spent on the cpu (in nanoseconds)", "time spent waiting on a
runqueue (in nanoseconds)" and "# of timeslices run on this cpu"
(https://docs.kernel.org/scheduler/sched-stats.html).

During each window the test also reads the summed run time of every
thread about once per millisecond. A **wakeup** is a run of samples in
which the run time grew, with no gap over 20 ms. Run time is cumulative,
so no wakeup is missed, however short; two wakeups closer than 20 ms are
counted as one.

Three processes were measured:

- **nexusd idle:** 30 s, after the 30 s settle and the NFR-1 idle window.
- **nexusd with one op running:** 30 s. The op is the `hold` script under
  the process backend (OpenRC) or the systemd launcher (Ubuntu), held at
  its gate for the whole window.
- **Reference process:** measured in the idle window. It is a copy of the
  test binary (`NEXUS_NFR_TICKER=1`) doing only the watcher's per-poll
  work, `detach.ReadExit` of an absent exit.json every
  `detach.PollInterval` (500 ms), on the same Go runtime (go1.26.11).

### Finding: no extra product wakeups; switches were the wrong proxy

Investigation runs (`alpine-investigate.log`, `ubuntu-investigate.log`):

| 30 s window | Alpine wakeups (20 ms gap) | Alpine switches per poll | Ubuntu wakeups | Ubuntu switches per poll |
| --- | --- | --- | --- | --- |
| nexusd idle | 2 | 0.10 | 2 | 0.10 |
| nexusd, one op running | 62 | 7.73 | 62 | 11.77 |
| Reference (one ReadExit per 500 ms) | 61 | 7.87 | 60 | 7.17 |

The watcher polls 60 times in 30 s, and nexusd with one op running wakes
60 times at a 100 ms gap (62 at 20 ms). The reference process, with one
500 ms timer and nothing else, has the same switch count per tick as
nexusd on Alpine (7.87 vs 7.73). The roughly 8 switches per poll are the
Go runtime's cost of one timer wakeup that makes a syscall, not extra
product work. Source reading of go1.26.11 `runtime/proc.go` is consistent
with that:

- `sysmon` (line 6486) deep-sleeps until the next timer
  (`notetsleep`, line 6542).
- Any syscall entry wakes it (`entersyscallWakeSysmon`, line 4746), and
  the exit.json open is one. It then restarts its 20 us `usleep` loop
  (line 6506, up to 10 ms at line 6504) until every P is idle again.
- Each of those sleeps is a context switch of the sysmon thread.

On Ubuntu the extra switches per poll (11.77 vs the reference's 7.17)
come from the systemd launcher's D-Bus liveness call every 10 s. They do
not add wakeups: still 62 at 20 ms, 60 at 100 ms.

The gap histograms (in the logs) show why the gap is 20 ms. The
reference's wakeups split at 2 ms (75 on Alpine) and 10 ms (64), and stay
at 61 or 60 from 20 ms up. Sysmon's tail of one wakeup is up to 10 ms
behind it.

No product loop runs faster than the documented poll rate. The only
timers while an op runs are `internal/detach/watcher.go:83`
(`PollInterval`) and the liveness query it starts every
`LivenessInterval`. `internal/ops/endpoints.go:139` runs only for a
client waiting on an op, and no client waited during the window. There
is no NFR-11 defect and no fix in internal/ops.

### Restored assertions

- **Control (Fatal):** the reference process wakes `polls ± 10%` times
  (54 to 66 in 30 s). This proves the detector counts one wakeup per
  timer, so the next bound cannot pass vacuously.
- **TC-45 wakeups:** the wakeups one op adds (one-op window minus idle
  window) are at most the watcher's polls + 10%, which is 66 in 30 s.
  The 10% covers the liveness check's own wakeup, which can land apart
  from a poll (`PollInterval/LivenessInterval` = 5%), and the window's
  edges.
- **TC-45 CPU:** nexusd's CPU time over the one-op window (schedstat ns,
  every thread) is within a factor of 2 of the systemd launcher's mean on
  Ubuntu, 23 ms (`-nfr.systemd-op-cpu`). This is the proposed erratum
  below, not the TC's 10%.

Red first, with defects planted in a scratch worktree only (never
committed):

| Planted defect | Log | Result |
| --- | --- | --- |
| A 250 ms ticker in `internal/ops` runDetached while a detached op is watched | `alpine-red-planted-ticker.log` | FAIL: `one running op adds 118 wakeups to nexusd in 30s (3.93/s), want no more than the watcher's 60 polls`; CPU 68.281 ms, which is +173% of the then 25 ms reference and 2.97x of 23 ms, so the factor-of-2 bound also fails |
| Detector gap set to 2 ms | `alpine-red-planted-gap.log` | FAIL: `the reference process with one timer every 500ms woke 67 times in 30s, want 60 within 10%` |

### CPU: the 10% comparison cannot be resolved on this harness

Every run of the same nexusd, CPU time over the 30 s one-op window, in
order, the last being the confirmation run at 9c2e6c5:

| Harness | Runs (ms) | Mean | SD (CV) | Against 23 ms |
| --- | --- | --- | --- | --- |
| Ubuntu / systemd launcher (8) | 25.111, 18.396, 29.504, 26.066, 17.218, 21.731, 23.233, 24.194 | **23.18 ms** | 4.03 ms (17%) | 0.75x to 1.28x |
| Alpine / process backend (6) | 25.551, 26.225, 13.800, 18.250, 34.409, 35.147 | **25.56 ms** | 8.51 ms (33%) | 0.60x to 1.53x |

Measurement resolution is not the limit, because schedstat is in ns.
The limit is the spread of single 30 s windows.

- **Single runs:** of the three Ubuntu runs asserted at 10% of 25 ms
  (`ubuntu-10pct-*.log`), two failed on noise: 18.396 ms (−26.4%) and
  29.504 ms (+18.0%).
- **Means:** the Alpine mean is +10.3% of the Ubuntu mean, or +9.7%
  without the planted-gap run, whose nexusd is unchanged. The standard
  error of that difference is about ±16% (3.48 ms and 1.42 ms standard
  errors), so these runs can neither confirm nor refute 10%.
- **Runs needed:** with a coefficient of variation of about 30%, the
  error of a difference of means is about 30% × √(2/n). Resolving 10%
  at one standard error would take about 20 windows per harness, and at
  two about 40.

After the first five Alpine windows the means had looked within 3%.
The last two Alpine windows moved them, which shows how small samples mislead here.

The spread is in the host, not in nexusd. The reference process, on the
same runtime with fixed work, also took 8.101 to 16.685 ms per window (13 windows).
The median CPU per wakeup was logged in the final runs and spreads just
as widely: 150 to 333 us for nexusd and 94 to 200 us for the reference.
VM load during the runs was 0.1 to 1.4, with other agents' harness
containers on the same colima VM.

### Stability: the committed bounds, 3 runs per harness plus a confirmation run

| Log | Init | Idle window | One op adds (wakeups) | Op / idle / reference wakeups | Switches per poll, op (reference) | CPU over op window | Result |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `alpine-1.log` | OpenRC | 5 min | 60 | 61 / 1 / 61 | 7.90 (7.28) | 13.800 ms (0.60x) | PASS |
| `alpine-2.log` | OpenRC | 30 s | 60 | 62 / 2 / 60 | 8.57 (8.53) | 18.250 ms (0.79x) | PASS |
| `alpine-3.log` | OpenRC | 30 s | 59 | 61 / 2 / 60 | 9.97 (8.53) | 34.409 ms (1.50x) | PASS |
| `ubuntu-1.log` | systemd | 5 min | 59 | 60 / 1 / 60 | 8.52 (7.42) | 17.218 ms (0.75x) | PASS |
| `ubuntu-2.log` | systemd | 30 s | 61 | 63 / 2 / 60 | 11.63 (8.22) | 21.731 ms (0.94x) | PASS |
| `ubuntu-3.log` | systemd | 30 s | 58 | 60 / 2 / 60 | 10.67 (9.25) | 23.233 ms (1.01x) | PASS |

| `alpine-9c2e6c5.log` | OpenRC | 30 s | 58 | 60 / 2 / 61 | 8.35 (8.57) | 35.147 ms (1.53x) | PASS |
| `ubuntu-9c2e6c5.log` | systemd | 30 s | 59 | 61 / 2 / 60 | 10.37 (8.17) | 24.194 ms (1.05x) | PASS |

The first six ran with the CPU bound at ±50% of 25 ms; the committed
bound, a factor of 2 of 23 ms, admits each of their values (0.60x to
1.50x), and the wakeup bounds are unchanged. The last two are the exact
committed tree at 9c2e6c5, which also carries the product changes of
56c8848 and a77ffd7, one run per harness. The 30 s idle runs log
`NFR DRY RUN` for NFR-1's idle window, which does not change TC-45
step 2. Every run also passed TC-45 step 1: idle RSS, startup and binary
size, as before.

Margins: added wakeups were 58 to 61 against a bound of 66. The control
was 60 or 61 against 54 to 66. CPU was 0.60x to 1.53x against 0.5x to
2x; the narrowest CPU margin is the low side (13.800 ms against 11.5 ms).

### Proposed TC-45 erratum (for the orchestrator)

Replace TC-45 Expected bullet 2 with:

> During step 2, the wakeups one running op adds to `nexusd` (a wakeup is
> a return from idle: a run of CPU-time growth summed over its threads'
> schedstat, sampled every 1 ms, with no gap over 20 ms; the one-op 30 s
> window minus an idle 30 s window) are no more than the watcher's polls
> in the window plus 10%. A reference process with one `PollInterval`
> timer is counted as one wakeup per timer, within 10%, as the
> detector's control. Context switches are logged and are not wakeups.
> `nexusd`'s CPU time over the one-op window (schedstat ns) is within a
> factor of 2 of the systemd launcher's mean on Ubuntu. Each harness's
> windows and their means are recorded in
> docs/verification/wi-3.4-harness.md. A 10% comparison is not asserted,
> because one window's CPU time varies by 17% to 33% (coefficient of
> variation) on the shared T1 VM.

Why:

- **Per run:** 2 of 3 Ubuntu runs failed a per-run 10% bound on noise.
- **Between means:** at these sample sizes, Alpine at +10.3% has a
  standard error of about ±16%. It would take about 20 to 40 windows per
  harness to resolve 10%.
- **What the factor of 2 still catches:** a 250 ms timer added next to
  the watcher (2.97x, measured).
- **The precise test of NFR-11 is the wakeup bound:** that same defect
  is 118 wakeups against a bound of 66, while every clean run was 58 to
  61.

If the orchestrator wants the 10% kept as written, the alternative is a
dedicated measurement of at least 20 one-op windows per harness on an
otherwise idle VM, compared by mean. That cannot be a per-run assertion
in this test.
