# CONTAINER DRY RUN of test/live/nginx-check.sh: NOT real-host evidence

These files come from running `test/live/nginx-check.sh` in the T1 harness
containers on the local Docker host (colima, linux/arm64, kernel
6.8.0-100-generic). They show that the script works: its preconditions, its
transcript, every step, its guards, and its cleanup after an interruption.
They are **not** the evidence that WI 7.2, TC-118 and TC-119 require. That
evidence is a transcript from the user's Alpine nginx VM on Proxmox and
from a real Ubuntu host, each running the release tarball. Until those
exist, WI 7.2 is `pending-live-gate`. The procedure is in
[../nginx-live.md](../nginx-live.md).

Each transcript says so in its own header (`CONTAINER DETECTED ... NOT
real-host evidence`), and the script records `1-host FAIL` in every
container run. That FAIL is expected here, so every full run exits 1.

## Setup (alpine-setup.log, ubuntu-setup.log)

Two containers, `nx-wi72-alpine-<stamp>` and `nx-wi72-ubuntu-<stamp>`
(`NAME_PREFIX=nx-wi72`), each with init as PID 1, started with the flags of
`test/harness/run.sh`: `--privileged --tmpfs /run --tmpfs /run/lock`,
anonymous volumes for the nested Engine, and `--cgroupns=private` (Alpine)
or `--cgroupns=host` (Ubuntu).

- **Alpine:** `test/harness/Dockerfile.alpine` (Alpine 3.24.4, BusyBox
  init, OpenRC 0.63.2, nginx 1.30.6). sshd was added (`apk add
  openssh-server`), and its port and nexusd's were published on the Mac's
  loopback (2222 and 17443), so the script ran **on the Mac with `--ssh`**,
  as the user will run it against the VM. nexusd listened on
  `0.0.0.0:7443` for that. `nginx.conf` was given the user's relative
  include line, `include http.d/*.conf;`.
- **Ubuntu:** `test/harness/Dockerfile` (Ubuntu 26.04, systemd 259, nginx
  1.28.3). The script ran **on the host**, as root, with the `nexus` CLI
  installed there.
- On both: nexusd and nexus built for linux/arm64 (nexus also for
  darwin/arm64) at the commit named in `exit-statuses.log`, packed with
  `make nexusd-tree`, and installed by the release tree's `install.sh`,
  as on a real host. The nginx provider was enabled, nginx and nexusd
  started, and a token with `nginx:read`, `nginx:write` and `nginx:config`
  created.
- **Pre-existing "user" sites,** which every run must leave byte-identical:
  `user-app.conf` (serving `user-app` on 127.0.0.1:18301), `user-api.conf`
  (an `upstream` and a server on 18302), `user-old.conf.disabled`, a
  `snippets/common.inc` subdirectory file and a `README.local`, next to the
  distribution's `default.conf` on Alpine.

`after-readbacks.log` is an independent readback, made by the dry-run
driver outside the script, before any run and after each one: `ls -la` of
the include directory, `sha256sum` of every user file and `nginx.conf`,
`/var/lib/nexus-nginx-check`, curl of the two user sites, the init system's
nginx status, and any holder process still running.

Every transcript ran the same script; its sha256 is in each header and in
`exit-statuses.log`, and equals the committed `test/live/nginx-check.sh`.
The tokens appear in none of these files: the driver checked each with
`grep -rF -f <token file>` before removing the containers
(`exit-statuses.log`, last lines).

## Runs and results

| File | Command | Result |
| --- | --- | --- |
| `00-alpine-plan.txt` | `--ssh` (no `--yes`) | Facts and the plan: `1-include` PASS on `include http.d/*.conf;` (relative), `1-asm1` PASS, the snapshot. Nothing changed; exit 2 |
| `01-alpine-full-lifecycle.txt` | `--ssh --yes --with-lifecycle` | All 26 checks PASS (`1-*`, `2a` to `2f`, `3a` to `3g`, `4a`, `4b`, `4b-initexit`, `5a`, `5b`, `6-removed`, `untouched`, `nginx-conf`, `token-absent`). `rc-service nginx reload` exits 1 (invalid) and 0 (bind conflict), as svcctltest has. Exit 1, from `1-host` only |
| `02-alpine-default.txt` | `--ssh --yes` | As 01 without stop, start and restart (`NOT-RUN`), and `master` PASS: the master at the end is the one from the start. Exit 1, from `1-host` |
| `03-alpine-ctrl-c-step5.txt` | `--ssh --yes`, then SIGINT to the script's process group (as Ctrl-C sends it) once step 5b's holder was up | The EXIT trap killed the holder (and BusyBox's `timeout` watcher), removed the site, ran `nginx -t`, reloaded (the site had been enabled), saw the port refused, and found every other file byte-identical. Exit 130 |
| `04-alpine-sigkill-step5.txt` | `--ssh --yes`, then SIGKILL to the process group in step 5b | No trap can run. `after-readbacks.log` ("after 04") shows what was left: the site holding the bind-conflict text (200 bytes), the holder, and the state directory. nginx kept serving the user sites |
| `05-alpine-refuse-pending.txt` | `--ssh --yes` after 04 | Refused before any change: an earlier run did not finish. Exit 2 |
| `06-alpine-cleanup.txt` | `--ssh --yes --cleanup` | From the state file: killed the holder by its recorded PID, removed the site, `nginx -t`, reloaded, port refused, snapshot identical, state removed. Exit 0 |
| `07-alpine-cleanup-noop.txt` | `--ssh --yes --cleanup` with nothing left | Nothing to remove. Exit 0 |
| `08-alpine-refuse-invalid-config.txt` | `--ssh --yes`, with a broken user file planted (`zz-user-broken.conf`) | Refused at `nginx -t`, before any change; "after 08" shows no file or state of the script. Exit 2. The driver then removed the planted file |
| `09-alpine-guard-no-include-line.txt` | `--ssh` (plan), with the `http.d` include removed from `nginx.conf` | Planted defect for the include-line check: `1-include` FAIL ("sites written there are never served (ASM-12)"). Exit 2. The driver put `nginx.conf` back (same sha256) |
| `10-alpine-guard-user-file-edited.txt` | `--ssh --yes`, while the driver appended a line to `user-app.conf` during step 2 | Planted defect for the snapshot check: `untouched` FAIL, with the diff of `user-app.conf`'s line (size 130 to 170, new sha256). Every other check PASS. Exit 1. The driver then put the file back |
| `11-ubuntu-plan.txt` | on the host (no `--yes`) | Facts and the plan: `1-include` PASS on `include /etc/nginx/conf.d/*.conf;`. Exit 2 |
| `12-ubuntu-full-lifecycle.txt` | on the host, `--yes --with-lifecycle` | All 26 checks PASS. `systemctl reload nginx` exits 1 (invalid) and 0 (bind conflict), as svcctltest has; ASM-7's expectation of 0 for a bind conflict holds. Exit 1, from `1-host` |
| `13-ubuntu-sigint-op-in-flight.txt` | `--yes`, then SIGINT to the script once the add was submitted | The trap waited for the add operation to end (`Succeeded`), checked for other open nginx operations, removed the site, reloaded, port refused, snapshot identical. Exit 130 |
| `14-ubuntu-sigterm-nginx-stopped.txt` | `--yes --with-lifecycle`, then SIGTERM once the `nginx stop` was submitted | The trap waited for the stop to end, found nginx `inactive`, removed the site, ran `nginx -t`, **started nginx** (`systemctl is-active`: active), port refused, snapshot identical. Exit 130 |
| `15-ubuntu-sigterm-after-stop.txt` | `--yes --with-lifecycle`, with a SIGTERM meant for when `systemctl is-active` printed `inactive` | **The kill missed**: the driver's PID lookup matched the script and one of its command-substitution subshells, so `kill` got two PIDs on one argument and failed (`exit-statuses.log` shows both). The run completed undisturbed: a second full Ubuntu run, all checks PASS. Exit 1, from `1-host` |

## What the runs show

- **Both platforms, every check.** With the include directory, mode and
  owner, sha256, the served marker and the master and workers all read back
  without Nexus, every operation's result matched what the host showed. No
  product defect was found.
- **ASM-7, as svcctltest has it.** `rc-service nginx reload` and
  `systemctl reload nginx` exit 1 for an invalid configuration and 0 for a
  bind conflict; in each case nginx keeps its master and workers. The
  provider's `observed.initExitCode` for the bind conflict was 0 on both,
  the same as the direct reload. These are containers (OpenRC 0.63.2 and
  systemd 259), so they confirm only that the script compares correctly;
  the VM's values are the evidence.
- **Ubuntu logs the parse error to the journal.** nginx on Ubuntu is built
  with `--error-log-path=stderr`, so after an invalid `systemctl reload`
  `/var/log/nginx/error.log` has no new line, and the script waits its full
  5 s before writing the valid text back. It then shows
  `journalctl -u nginx.service`, which holds the `[emerg]` line. The
  bind failure is in the error log on both.
- **A reload is graceful.** Right after a reload, the old workers can still
  answer on a port the new configuration dropped. The first version of the
  script checked the port at once and saw it served; it now waits until no
  old worker is still active, the provider's own "confirmed" rule, before it
  reads the port back.
- **Interrupts.** Ctrl-C (03), SIGINT with an operation in flight (13) and
  SIGTERM with nginx being stopped (14) each ended with the site gone, nginx
  running, no holder, and every user file byte-identical (the script's
  `untouched` and the driver's "after" readbacks agree). After a SIGKILL
  (04), the next run refused and `--cleanup` (06) recovered the worst state:
  the bind-conflict text on disk and the holder running.
- **The real VM may differ.** It may lack `curl` and `ss`. An earlier
  iteration of the script (before `1-asm1` and the holder lookup were
  added), with both hidden on an Alpine container, used BusyBox `wget` and
  `netstat` and passed every check. That run is not among these files.
