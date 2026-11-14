# Part A on real hosts: Alpine with OpenRC (TC-51) and Ubuntu (TC-52)

Work item 3.5 of nginx-provider. This page has the procedure, the script
that runs it (`test/live/alpine-check.sh`), and the transcript template to
fill in from the real runs.

**Status: `pending-live-gate`.** No real-host transcript exists yet. The
only runs so far are container dry runs in the T1 harness, in
[alpine-openrc-dryrun/](alpine-openrc-dryrun/). They show the script works.
They are not evidence for TC-51 or TC-52.

- [Before you start](#before-you-start)
- [What the script changes, and what it never touches](#what-the-script-changes-and-what-it-never-touches)
- [Alpine procedure (TC-51)](#alpine-procedure-tc-51)
- [Ubuntu procedure (TC-52)](#ubuntu-procedure-tc-52)
- [Transcript template](#transcript-template)
- [Known before the real run](#known-before-the-real-run)

## Before you start

On the Alpine VM:

- Alpine 3.21 or later on x86_64, booted with OpenRC, and cgroup v2
  (unified, Alpine's default).
- nexusd installed from the release tarball with its `install.sh` (see
  [install.md](../install.md#install-nexusd-on-alpine-openrc)), and
  started: `rc-service nexusd start`. For the reboot step, also
  `rc-update add nexusd default`, so nexusd starts at boot. Without it,
  phase 2 starts nexusd by hand and says so.
- curl: `apk add curl`. The script uses curl for nexusd's API. Everything
  else it needs is a BusyBox applet.
- A root shell. Alpine has no `sudo` by default: use `su -`.
- A token with the scopes the test operation needs, created on the VM:

  ```
  nexusd token create --name alpine-check --scope scripts:read --scope scripts:run --expires 1d
  ```

  Pass it to the script in `NEXUS_TOKEN`. The token never appears in the
  transcript: commands that use it show `<NEXUS_TOKEN>`, and curl reads it
  from standard input, not from its arguments. Revoke it when you are done:
  `nexusd token revoke alpine-check`.

The script finds nexusd at the first `listen` address in
`/etc/nexus/nexusd.yaml`, or at 127.0.0.1:7443. With `tls.mode: auto`, it
trusts the generated certificate, `/var/lib/nexus/tls/cert.pem`, which
names `localhost`. To use another address, set `NEXUS_ADDR=host:port`. To
use another URL and certificate, set `NEXUS_URL` and `NEXUS_CACERT`.

## What the script changes, and what it never touches

Without `--yes`, the script changes nothing. It checks the preconditions,
records the step 1 facts and prints the plan, then exits with status 2.
Each change below happens only with `--yes`, and the script undoes each
one at the end. If the run is interrupted, an EXIT trap undoes them, and
`--yes --cleanup` undoes whatever is still left.

| What | Why | Undone by |
| --- | --- | --- |
| `/usr/local/libexec/nexus-alpine-check/op.sh` (root:root 0755) | The test operation. It only sleeps, writes files under `/var/lib/nexus-alpine-check/run/<tag>`, and in step 6 starts a `setsid sleep 1000` | Removed |
| `/var/lib/nexus-alpine-check/` (0700) | Backups, the reboot state, the operation markers | Removed |
| `/etc/nexus/nexusd.yaml` | A catalog entry, `nexus-alpine-check`, and `providers.scripts.enabled: true`. The file is copied first, the edit is checked with `nexusd validate-config`, and then `rc-service nexusd reload` applies it. A scripts section other than the default one (`enabled` plus `catalog: []`, the flow form, or none) is refused, and the script prints the entry for you to add by hand (`--catalog-ready`) | Put back byte for byte from the copy, then reloaded. The result `config-restored` compares the sha256 |
| `/etc/conf.d/nexusd` | Step 3 only: `rc_cgroup_cleanup="YES"` | Put back, or removed if it did not exist |
| `/usr/libexec/rc/sh/rc-cgroup.sh` | Step 3 with `--with-910-fix` only: upstream 1cf3944's paths in `cgroup_running()` | Put back from the copy |
| nexusd | Reloaded, restarted, stopped and started by the steps | Left started |
| Processes | Only the script's own: the canary, the test operations' supervisors and children. Step 4 kills an operation's supervisor and its processes with `kill -9`, read from that operation's `nexus-ops/<id>` cgroup | Test operations still running are canceled through the API; the canary is killed |

The script never edits nginx's configuration and never touches the nginx
service. The result `nginx-untouched` compares a digest of every file under
`/etc/nginx` from before and after the run. The script never reboots. For
step 5 it tells you when to reboot.

The test operations stay in nexusd's operation history and audit log,
under the token's name.

## Alpine procedure (TC-51)

From a checkout on the dev host:

```
scp test/live/alpine-check.sh root@<vm>:/root/alpine-check.sh
ssh root@<vm>
```

On the VM, as root, in `/root`:

1. Read the plan. This changes nothing:

   ```
   sh alpine-check.sh --transcript tc51-0-plan.txt
   ```

2. Steps 1 to 4 and 6:

   ```
   NEXUS_TOKEN=<token> sh alpine-check.sh --yes --transcript tc51-1-main.txt
   ```

   It took under 2 minutes in the dry run. On stock Alpine, step 3 ends
   `3a VACUOUS`.
   [Known before the real run](#known-before-the-real-run) explains why.

3. Step 3 with cleanup in effect (ASM-14). This is optional, and it changes
   an OpenRC file for the length of the step:

   ```
   NEXUS_TOKEN=<token> sh alpine-check.sh --yes --steps 3 --with-910-fix --transcript tc51-2-step3-fix.txt
   ```

   If the installed `rc-cgroup.sh` has OpenRC issue #910, this applies
   upstream 1cf3944's `cgroup_running()` for the step, shows the diff, and
   puts the original back afterwards. It is the same change TC-E14 makes in
   the harness. On an OpenRC that already has the fix, the flag changes
   nothing.

4. Step 5, the reboot:

   ```
   NEXUS_TOKEN=<token> sh alpine-check.sh --yes --reboot-phase1 --transcript tc51-3-reboot1.txt
   reboot
   ```

   Phase 1 starts a 2-hour test operation, records `boot_id`, leaves the
   catalog entry in place, and prints what to do next. Reboot the VM
   yourself. When it is back:

   ```
   NEXUS_TOKEN=<token> sh alpine-check.sh --yes --reboot-phase2 --transcript tc51-4-reboot2.txt
   ```

   Phase 2 refuses, and keeps its state, if `boot_id` is unchanged and the
   supervisor is still running. Expect `5a` PASS, `Failed outcome-unknown`:
   the reboot race the dry runs found is fixed (see
   [Known before the real run](#known-before-the-real-run)). After the checks it cleans up as step 2
   does. While phase 2 is pending, a full run refuses to start.

5. Revoke the token (`nexusd token revoke alpine-check`). Copy the
   transcripts back:
   `scp 'root@<vm>:/root/tc51-*.txt' docs/verification/alpine-openrc-live/`.
   Fill in the [template](#transcript-template).

If a run is interrupted and leaves something behind, or you want to give up
a pending reboot check, run:

```
NEXUS_TOKEN=<token> sh alpine-check.sh --yes --cleanup --transcript tc51-cleanup.txt
```

`--steps 2,6` runs only those steps; step 1 always runs. `sh
alpine-check.sh --help` lists every option.

**Exit status:**

- 0: every check passed.
- 1: a check failed.
- 2: refused, because a precondition failed or `--yes` is missing.
- 3: nothing failed, but a check was VACUOUS or NOT-RUN.

Each `RESULT <id>: <status> - <detail>` line in the transcript is one
Expected item, and the summary at the end lists them all.

**How readbacks are done.** Process facts come from `/proc` on the VM:
`/proc/<pid>/stat` for the state, PPID, session and start ticks;
`/proc/<pid>/cgroup` for the cgroup; `/proc/<pid>/cmdline`; and
`supervisor.json` and `exit.json` in `/var/lib/nexus/ops/<id>`. They are
not read through Nexus. Only operation status, problem type and operation
logs come from nexusd's API. A process counts as alive only if its PID
exists, it is not a zombie, and its start ticks are the ones recorded, so a
reused PID is not counted.

## Ubuntu procedure (TC-52)

On the Ubuntu host, which runs the release tarball:

```
sh alpine-check.sh --ubuntu --transcript tc52-ubuntu.txt
```

This mode is read-only and needs no token. It records:

1. `systemd-analyze security nexusd`. The overall exposure must be 2.6 or
   lower, rated `OK` or better (result `52-1`).
2. The sha256 of `/etc/systemd/system/nexusd.service`. It must equal
   `deploy/nexusd.service` at d04583a,
   `5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a`
   (result `52-2`). To compare the bytes directly, run this on the Ubuntu
   host from a checkout; no output means the files are identical:

   ```
   git show d04583a:deploy/nexusd.service | diff - /etc/systemd/system/nexusd.service
   ```

   Without a checkout on the host, run it from the dev host:

   ```
   git show d04583a:deploy/nexusd.service | ssh <ubuntu-host> 'diff - /etc/systemd/system/nexusd.service'
   ```

3. `make test-harness` (NFR-9). This runs on the dev host, because it needs
   Docker, and is not a step of the script (result `52-3 MANUAL`). Record
   its last line, `harness: exit 0`, and the commit it ran at.

## Transcript template

Copy this section into the live record, then fill in one row per Expected
item. "Line" is the `RESULT` line's file and line number in the transcript.
Every item needs its observed value, not only a verdict.

**Alpine VM (TC-51)**

- Host: ______
- Date (UTC): ______
- Release tarball: `nexusd-<version>-linux-amd64.tar.gz`, sha256 ______
- Script sha256 (transcript header): ______
- Transcripts: `tc51-0-plan.txt`, `tc51-1-main.txt`,
  `tc51-2-step3-fix.txt`, `tc51-3-reboot1.txt`, `tc51-4-reboot2.txt`

| TC-51 Expected | Result id | Observed | Line | Verdict |
| --- | --- | --- | --- | --- |
| 1. `uname -m` is `x86_64` (ASM-13) | `1-arch` | | | |
| 1. Alpine release 3.21 or later (ASM-15) | `1-release` | | | |
| 1. `/sys/fs/cgroup` is cgroup2 (`cgroup2fs`; BusyBox `stat` prints magic `63677270`) | `1-cgroup` | | | |
| 1. `rc_cgroup_cleanup` value, recorded | `1-rc_cgroup_cleanup` | | | |
| 1. OpenRC issue #910 in `rc-cgroup.sh`, recorded | `1-openrc-910` | | | |
| 2. Supervisor PID and `startTicks` unchanged across `rc-service nexusd restart` | `2a` | | | |
| 2. Supervisor session equals its own PID | `2b` | | | |
| 2. Supervisor PPID is no longer the old nexusd | `2c` | | | |
| 2. Marker exists; op `Succeeded`; ran at least 20 s | `2d` | | | |
| 2. Op log has no `outcome-unknown` | `2e` | | | |
| 3. Canary in nexusd's cgroup is gone within 5 s of `rc-service nexusd stop` (cleanup in effect) | `3a` (stock run and `--with-910-fix` run) | | | |
| 3. Only then: supervisor survives, PID and `startTicks` unchanged, cgroup `nexus-ops/<id>` | `3b` | | | |
| 3. The op still ends `Succeeded` after nexusd starts again (TC-E14 step 4) | `3c` | | | |
| 4. Supervisor killed while nexusd is down: op `outcome-unknown` | `4` | | | |
| 5. After the reboot, the op is `outcome-unknown` | `5a` | | | |
| 5. `boot_id` before and after differ (both recorded) | `5b` | before: ___ after: ___ | | |
| 6. Cancel with a `setsid` grandchild: op `Canceled` | `6a` | | | |
| 6. No descendant remains | `6b` | | | |
| 6. `/sys/fs/cgroup/nexus-ops/<id>` gone or empty | `6c` | | | |
| Housekeeping: `/etc/nexus/nexusd.yaml` restored byte for byte | `config-restored` | | | |
| Housekeeping: nothing under `/etc/nginx` changed | `nginx-untouched` | | | |

**Ubuntu host (TC-52)**

- Host: ______
- Date (UTC): ______
- Release tarball sha256: ______
- Transcript: `tc52-ubuntu.txt`

| TC-52 Expected | Result id | Observed | Line | Verdict |
| --- | --- | --- | --- | --- |
| 1. `systemd-analyze security nexusd`: exposure 2.6 or lower, `OK` | `52-1` | | | |
| 2. Unit sha256 equals d04583a's (and the `git show d04583a:deploy/nexusd.service \| diff - /etc/systemd/system/nexusd.service` output is empty) | `52-2` | | | |
| 3. `make test-harness` passes (dev host: commit, last line) | `52-3` | | | |

## Known before the real run

The container dry runs found the following. Each is something to watch for
in the real transcript.

- **Step 3 is VACUOUS on stock OpenRC.** In OpenRC issue #910,
  `cgroup_running()` looks for `/sys/fs/cgroup/<svc>` instead of
  `openrc.<svc>`, so `rc_cgroup_cleanup="YES"` does nothing on a unified
  host. The harness's OpenRC 0.63.2 (Alpine 3.24) still has it. 3.1 found
  it in 0.55.1 (Alpine 3.21). The canary survives the stop, so the script
  reports `3a VACUOUS` ("rc_cgroup_cleanup not in effect; the survival
  check would prove nothing") and does not count the supervisor. With
  `--with-910-fix` the dry run gave 3a, 3b and 3c PASS. Whether a run with
  that local patch is acceptable evidence for ASM-14 is for the reviewer to
  decide. The stock run is evidence of what an operator gets from YES:
  nothing.
- **`stat -fc %T` prints `UNKNOWN` with BusyBox.** TC-51 step 1 expects
  `cgroup2fs`, which is GNU coreutils' name. BusyBox `stat` has no name for
  cgroup2 and prints `UNKNOWN`. The script records `stat -fc '%t %T'` and
  the `/proc/mounts` line, and accepts the magic number `63677270`
  (`CGROUP2_SUPER_MAGIC`). TC-51's wording needs an erratum to match.
- **Step 5 ended differently each time before the fix (product finding,
  fixed).** At shutdown, BusyBox init sends SIGTERM to every process, then
  SIGKILL. The supervisor used to take any SIGTERM as the cancel signal, so
  three container runs of the same step ended `outcome-unknown`, `Canceled`
  and `Failed script-failed`
  ([before](alpine-openrc-dryrun/README.md#the-reboot-step-gives-a-different-answer-each-time-product-finding)).
  Fixed in `ba56e70`: a SIGTERM is a cancel only when nexusd recorded the
  request (`<op dir>/cancel`); otherwise `exit.json` says `interrupted` and
  the operation ends `Failed outcome-unknown`. After the fix, 8 of 8 Alpine
  and 3 of 3 Ubuntu container restarts ended `outcome-unknown`
  ([alpine-openrc-dryrun/reboot-race/after-fix/](alpine-openrc-dryrun/reboot-race/after-fix/README.md)).
  `5a` is expected to pass on the VM. Phase 2 still prints the operation's
  `exit.json`, so the real transcript shows which way the race went: none,
  or `"interrupted":true`. An operator's host-side cancel is now `SIGINT`
  (`kill -INT <pid>`), see [OpenRC hosts](../security.md#openrc-hosts-alpine).
- **The `outcome-unknown` detail named a systemd unit on OpenRC (fixed).**
  It read "unit nexus-op-<id>.service ended without reporting an outcome".
  Since `ba56e70` it names "the supervisor process of operation <id>" on
  OpenRC (`Options.Subject`), as the after-fix rounds show.
- **A container cannot show the reboot.** A container restart keeps the
  host kernel, so `boot_id` does not change (L-16). In the dry run, `5b`
  fails for that reason.
