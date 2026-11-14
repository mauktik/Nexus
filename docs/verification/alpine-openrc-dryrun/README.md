# CONTAINER DRY RUN of test/live/alpine-check.sh: NOT real-host evidence

These files come from running `test/live/alpine-check.sh` in the T1
harness containers on the local Docker host (colima, linux/arm64, kernel
6.8.0-100-generic). They show that the script works: preconditions,
transcript, every step, its guards, and its cleanup. They are **not** the
evidence that WI 3.5, TC-51 and TC-52 require. That evidence is a transcript
from a real Alpine VM (3.21 or later, x86_64) and from a real Ubuntu host,
each running the release tarball. Until those exist, WI 3.5 is
`pending-live-gate`. The procedure is in [../alpine-openrc.md](../alpine-openrc.md).

Each transcript says so in its own header (`CONTAINER DETECTED ... NOT
real-host evidence`), and the script records `1-host FAIL` (or `52-host
FAIL`) in every container run.

## Setup (setup.log, ubuntu-setup.log)

- **Alpine:** `test/harness/Dockerfile.alpine` (`alpine:latest` = Alpine
  3.24.4, OpenRC 0.63.2, BusyBox init as PID 1), started with the flags of
  `test/harness/run.sh alpine`: `--privileged --cgroupns=private --tmpfs
  /run --tmpfs /run/lock`, with anonymous volumes for the nested Engine.
  The container was `nx-wi35-alpine-20261114T060236Z`.
  - nexusd was built for linux/arm64 from the tree at 286bfdd (its product
    code is that of b35e87a, the work item's base; the two commits between
    them change only tests), and packed with `make nexusd-tree`.
  - The release tree's `install.sh` installed it, as on a real host. The
    nginx provider was enabled in `/etc/nexus/nexusd.yaml`, to match the
    user's VM. nginx and nexusd were added to the default runlevel and
    started.
  - A token with `scripts:read` and `scripts:run` was created.
- **Ubuntu:** `test/harness/Dockerfile` (`ubuntu:latest` = Ubuntu 26.04,
  systemd 259), with `run.sh`'s systemd flags. `install.sh` from the same
  release tree installed the unit.

Every transcript below ran the same script, sha256
`b77bdb0e75d221bf16dfa7bc226821b3b600457b70f6cbfd5c8aa860db7ca8c3`, which
is the committed `test/live/alpine-check.sh`. The exit statuses are in
`exit-statuses.log`. The token appears in none of these files: each was
checked with `grep -F` for the token's value before the container was
removed.

## Runs and results

| File | Command | Result |
| --- | --- | --- |
| `00-plan.txt` | `sh alpine-check.sh` (no `--yes`) | Step 1 facts and the plan. Nothing changed; exit 2 |
| `01-full-stock.txt` | `--yes` (steps 1 to 4 and 6, stock OpenRC) | 2a to 2e PASS. 3a **VACUOUS**: canary alive, OpenRC #910. 3b NOT-RUN, 3c PASS, 4 PASS, 6a to 6c PASS. Config restored, /etc/nginx untouched |
| `02-step3-with-910-fix.txt` | `--yes --steps 3 --with-910-fix` | 3a PASS: the canary was gone at the first check, under 0.5 s. 3b PASS: the supervisor survived with the same startTicks, in `/nexus-ops/<id>`. 3c PASS. rc-cgroup.sh was put back |
| `03-reboot-phase1.txt` | `--yes --reboot-phase1` | A 2 h operation Running, boot_id recorded, the reboot instruction printed. No reboot was done |
| `04-guard-reboot-pending.txt` | `--yes` while phase 2 is pending | Refused, exit 2 |
| `05-guard-not-rebooted.txt` | `--yes --reboot-phase2` before any restart | Refused (boot_id unchanged, supervisor alive), state kept, exit 2 |
| `simulated-reboot.log` | `docker restart -t 60` | BusyBox init: shutdown runlevel, `Sent SIGTERM to all processes`, `Sent SIGKILL to all processes`. Same boot_id after |
| `06-reboot-phase2.txt` | `--yes --reboot-phase2` | 5a **FAIL**: `Failed script-failed` (exit.json `signal SIGTERM`, `canceled false`), not `outcome-unknown`. 5b **FAIL**: the boot_id is unchanged, as a container restart cannot change it. Cleanup done |
| `07-interrupted-while-stopped.txt`, `07-interrupt-and-after.log` | `--yes --steps 3`, sent SIGTERM while nexusd was stopped | The EXIT trap killed the canary, removed `/etc/conf.d/nexusd`, started nexusd, canceled the test operation, restored the config byte for byte and removed its files. Exit 130 |
| `08-cleanup-noop.txt` | `--yes --cleanup` with nothing left | A no-op that removes nothing that exists, exit 0 |
| `09-refuse-no-token.txt` | `--yes` without `NEXUS_TOKEN` | Refused, exit 2 |
| `10-ubuntu.txt` | `--ubuntu` (TC-52) in the Ubuntu container | 52-1 PASS (`2.6 OK`), 52-2 PASS (sha256 equals d04583a's unit), 52-3 MANUAL |

`1-arch FAIL` (aarch64) and `1-host FAIL` (container) are expected in
every run here.

## The reboot step gives a different answer each time (product finding)

TC-51 Expected 5, and architecture.md ("a reboot changes `boot_id`, so
every Running record ends `outcome-unknown`"; Error Handling, "Helper
lost"), expect `outcome-unknown` after a reboot. Three phase-2 runs of the
same step, each after `docker restart`, ended three different ways:

| Run | File | Operation status | Why |
| --- | --- | --- | --- |
| A | `reboot-race/run-a-phase2-outcome-unknown.txt` (an earlier script version) | `Failed outcome-unknown` | No exit.json: the supervisor was SIGKILLed first |
| B | `reboot-race/run-b-phase2-canceled.txt`, `reboot-race/run-b-op-dir.txt` | `Canceled` | exit.json `{"signal":"SIGTERM","canceled":true}`: the supervisor took init's SIGTERM as a cancel |
| C | `06-reboot-phase2.txt` | `Failed script-failed`, "killed by SIGTERM" | exit.json `{"signal":"SIGTERM","canceled":false}`: init's SIGTERM reached the child before the supervisor marked a cancel |

At shutdown, BusyBox init sends SIGTERM to every process, then SIGKILL.
Its log shows this: `Sent SIGTERM to all processes`, then `Sent SIGKILL to
all processes`. The supervisor (`internal/detach/supervisor.go`, which
handles SIGTERM as "the cancel signal") writes exit.json if it can before
the SIGKILL. `internal/ops/manager.go` `detachedEnd` then trusts that
exit.json at the next start: `canceled` becomes Canceled, and any other
record goes through Finalize. A real Alpine `reboot` follows the same init
sequence, so the real-VM step 5 is likely to show the same race. This is
reported for a product decision and is not fixed here. On systemd hosts a
shutdown also stops the transient unit with SIGTERM, which suggests the same
result there, but that was not run.
