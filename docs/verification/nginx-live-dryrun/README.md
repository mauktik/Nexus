# CONTAINER DRY RUN of test/live/nginx-check.sh: NOT real-host evidence

These files come from running `test/live/nginx-check.sh` in the T1 harness
containers on the local Docker host (colima, linux/arm64). They show that
the script works: its preconditions, its transcript, every step, its
guards, and its cleanup after an interruption. They are **not** the
evidence that WI 7.2, TC-118 and TC-119 require. That evidence is a
transcript from the user's Alpine nginx VM on Proxmox and from a real
Ubuntu host, each running the release tarball. Until those exist, WI 7.2
is `pending-live-gate`. The procedure is in [../nginx-live.md](../nginx-live.md).

Each transcript says so in its own header (`CONTAINER DETECTED ... NOT
real-host evidence`), and the script records `1-host FAIL` in every
container run. That FAIL is expected here, so every full run exits 1.

All 30 runs below are one pass of the dry-run driver (stamp in
`exit-statuses.log`), with one script: the committed
`test/live/nginx-check.sh` at the commit and sha256 named on the second line
of `exit-statuses.log`.

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
  as the user will run it against the VM. `nginx.conf` was given the user's
  relative include line, `include http.d/*.conf;`.
- **Ubuntu:** `test/harness/Dockerfile` (Ubuntu 26.04, systemd 259, nginx
  1.28.3). The script ran **on the host**, as root, with the `nexus` CLI
  installed there.
- On both: nexusd and nexus built for linux/arm64 (nexus also for
  darwin/arm64) from a clean `git archive` of the commit named in
  `exit-statuses.log`, packed with `make nexusd-tree`, and installed by the
  release tree's `install.sh`, as on a real host. The nginx provider was
  enabled, nginx and nexusd started, and a token with `nginx:read`,
  `nginx:write` and `nginx:config` created.
- **Pre-existing "user" configuration,** which every run must leave
  byte-identical and serving:
  - `user-app.conf` (serves `user-app` on 127.0.0.1:18301);
  - `user-api.conf` (an `upstream` and a server on 18302 whose `location`
    comes from `include /srv/nginx-extra/locations.conf;`, a file **outside
    /etc/nginx**);
  - `user-linked.conf`, a **symlink** to `/srv/nginx-sites/linked.conf`
    (18304), as `sites-enabled` links and letsencrypt files are;
  - `user-old.conf.disabled`, `snippets/common.inc` and `README.local`,
    next to the distribution's `default.conf` on Alpine.

`after-readbacks.log` is an independent readback, made by the dry-run
driver outside the script, before any run and after each one: `ls -la` of
the include directory, `sha256sum` of every user file, the two files under
`/srv` and `nginx.conf`, `/var/lib/nexus-nginx-check`, curl of the three
user sites, the init system's nginx status, and any holder process still
running. Every section has the same hashes as its platform's baseline
(after the SEC-10 setup, the new baseline with the extra include in
`nginx.conf`), all three user sites answer, and nginx is started or
`active`. Something is left behind only where a run was killed on purpose
(04, 24, 27), after the pre-fix `--cleanup` (25, the red), and where the
driver planted another run's entry (28).

Two more files were copied into the Ubuntu container for the SEC-9 and
SEC-10 probes: the script as it was before those fixes (`git show
34e1c70:test/live/nginx-check.sh`), for the red runs 20 and 25, and a
fake nexusd (a small Perl http server, run as `nobody`; its request log is
in `exit-statuses.log`).

The tokens appear in none of these files: the driver checked each with
`grep -rF -f <token file>` before removing the containers
(`exit-statuses.log`, last lines).

## Runs and results

| File | Command | Result |
| --- | --- | --- |
| `00-alpine-plan.txt` | `--ssh` (no `--yes`) | Facts and the plan: `1-include` PASS on `include http.d/*.conf;` (relative), `1-asm1` PASS, the snapshot. Nothing changed. Exit 2 |
| `01-alpine-full-lifecycle.txt` | `--ssh --yes --with-lifecycle` | All 28 checks PASS, `token-env` and `cleanup-removed` included. `rc-service nginx reload` exits 1 (invalid) and 0 (bind conflict), as svcctltest has; the invalid text was in the include directory for 0.02 s and the bind-conflict text for 0.04 s. Exit 1, from `1-host` only |
| `02-alpine-default.txt` | `--ssh --yes` | As 01 without stop, start and restart (`NOT-RUN`), and `master` PASS: the master at the end is the one from the start. Exit 1, from `1-host` |
| `03-alpine-ctrl-c-step5.txt` | `--ssh --yes`, then SIGINT to the script's process group (as Ctrl-C sends it) once step 5b's holder was up | The EXIT trap killed the holder (and BusyBox's `timeout` watcher), removed the site first, ran `nginx -t`, reloaded (the site had been enabled), saw the port refused, and found every other file byte-identical. Exit 130 |
| `04-alpine-sigkill-step5.txt` | `--ssh --yes`, then SIGKILL to the process group in step 5b | No trap can run. "after 04" shows what was left: the test site, the holder (and its `timeout`), and the state directory. nginx kept serving the user sites |
| `05-alpine-refuse-pending.txt` | `--ssh --yes` after 04 | Refused before any change: an earlier run did not finish. Exit 2 |
| `06-alpine-cleanup.txt` | `--ssh --yes --cleanup` | From the state file: killed the holder by its recorded PID, removed the site, `nginx -t`, reloaded, port refused, snapshot identical, state removed. Exit 0 |
| `07-alpine-cleanup-noop.txt` | `--ssh --yes --cleanup` with nothing left | Nothing to remove. Exit 0 |
| `08-alpine-refuse-invalid-config.txt` | `--ssh --yes`, with a broken user file planted (`zz-user-broken.conf`) | Refused at `nginx -t`, before any change; "after 08" shows no file or state of the script. Exit 2 |
| `09-alpine-guard-no-include-line.txt` | `--ssh` (plan), with the `http.d` include removed from `nginx.conf` | Planted defect for the include-line check: `1-include` FAIL ("sites written there are never served (ASM-12)"). Exit 2. The driver put `nginx.conf` back (same sha256) |
| `10-alpine-guard-user-file-edited.txt` | `--ssh --yes`, while the driver appended a line to `user-app.conf` during step 2 | Planted defect for the snapshot check: `untouched` FAIL, with the diff of `user-app.conf`'s line (size 130 to 170, new sha256). Every other check PASS. Exit 1 |
| `11-alpine-refuse-no-listener-tool.txt` | `--ssh --yes`, with `/sbin/ss` and BusyBox `/bin/netstat` moved away | Planted defect for the free-port check, which must fail closed: `REFUSED: cannot list the host's listening TCP sockets (... neither ss nor netstat ran successfully ...)`, before any change; "after 11" shows no file or state of the script. Exit 2 |
| `12-alpine-guard-outside-include-edited.txt` | `--ssh --yes`, while the driver appended a line to `/srv/nginx-extra/locations.conf` (outside /etc/nginx, included by `user-api.conf`) during step 2 | Planted defect for the snapshot's coverage: `untouched` FAIL, the diff naming `/srv/nginx-extra/locations.conf` (size 39 to 79, new sha256). The file is in the snapshot only because `nginx -T` names it. Exit 1 |
| `13-ubuntu-plan.txt` | on the host (no `--yes`) | Facts and the plan: `1-include` PASS on `include /etc/nginx/conf.d/*.conf;`. Exit 2 |
| `14-ubuntu-full-lifecycle.txt` | on the host, `--yes --with-lifecycle`, with the environment probe | All 28 checks PASS. `systemctl reload nginx` exits 1 (invalid) and 0 (bind conflict), as svcctltest has; in place 0.01 s and 0.02 s. `14-ubuntu-full-lifecycle.envprobe.log`: the script's own `/proc/<pid>/environ`, read 1362 times during the run, never held the token, nor did any of the 83 descendants whose environment was read (19 more exited before it could be read, and are listed as not counted). Exit 1, from `1-host` |
| `15-ubuntu-sigint-op-in-flight.txt` | `--yes`, SIGINT to the script from an in-container watcher at the first `--no-wait` submission (the add's whatIf) | The trap removed the site, saw the operation open, waited for it (`Succeeded`), removed the site again, ran `nginx -t`; nginx `active`, port refused, `untouched`, `nginx-conf`, `master` PASS. Exit 130 |
| `16-ubuntu-sigterm-stop-in-flight.txt` | `--yes --with-lifecycle`, SIGTERM when `nginx stop` was submitted | The trap removed the site, waited for the stop (`Succeeded`), ran `nginx -t`, found nginx `inactive` and **started it** (`active`), port refused, snapshot identical. Exit 130 |
| `17-ubuntu-sigterm-nginx-stopped.txt` | `--yes --with-lifecycle`, SIGTERM once `systemctl is-active` printed `inactive` after the stop | No operation open; removed the site, `nginx -t`, **started nginx** (`active`), port refused, snapshot identical. Exit 130 |
| `18-ubuntu-restart-fails-sigterm.txt` | `--yes --with-lifecycle`; after `2e-start` a port grabber spun on binding the test site's port, took it when the restart stopped nginx (`exit-statuses.log`), and held it 5 s; SIGTERM when `2f-restart` was reported | The restart **failed midway**: `2f-restart FAIL ... Failed dependency-unavailable ... systemd unknown(failed); master ... -> none` (the provider's error names systemd's `ActiveState=failed`). The trap removed the site, `nginx -t` passed, and since this run had stopped or restarted nginx and it was not started, it **started nginx** (`active`), port refused, snapshot identical. Exit 130 |
| `19-ubuntu-token-probe-planted.txt` | plan mode, on a planted copy whose token block is replaced by `CHECK_TOKEN=${NEXUS_TOKEN:-}` (no unset, no re-exec; the diff is in `exit-statuses.log`), with the environment probe | Planted defect for the token checks: `token-env` FAIL ("a child process's environment holds the token"). `19-ubuntu-token-probe-planted.envprobe.log`: the script's own environment held the token in 12 of 12 reads, and 3 of 3 descendants did. Exit 2 |
| `20-ubuntu-sec9-fake-http-before-fix.txt` | **The script before the SEC-9 fix** (34e1c70, `nginx-check-old.sh` in the container), plan mode, no `NEXUS_URL`; nexusd stopped and an impostor running as `nobody` on 127.0.0.1:7443 over plain http (`/healthz` 200, nexusd's real `/v1/capabilities`, an nginx resource whose service is `x;touch /tmp/pwned-sec9;#`) | Red for SEC-9: it took plain http because `/healthz` answered there (`nexus host add ... --url http://127.0.0.1:7443`, so the token went to the impostor), printed `1-provider` PASS with the payload as the service, and ran `systemctl is-active -- x;touch /tmp/pwned-sec9;#.service` as root: `/tmp/pwned-sec9` exists afterwards, owned by uid 0 (`exit-statuses.log`). It refused only after that, because `is-active` of the nonsense unit said inactive. Exit 2 |
| `21-ubuntu-sec9-fake-http.txt` | The fixed script, same impostor, plan mode, no `NEXUS_URL` | https, pinned to `nexusd tls fingerprint`: `nexus host add` fails (`connection failed`), and the script refuses before any nginx call. No `/tmp/pwned-sec9`. Exit 2 |
| `22-ubuntu-sec9-explicit-http-payload.txt` | The fixed script, `NEXUS_URL=http://127.0.0.1:7443` set on purpose (so the impostor's answer is read), plan mode | A WARNING that `NEXUS_URL` is plain http, then `REFUSED: nexus nginx get gave a service name outside [a-z0-9][a-z0-9_.@-]*: 'x;touch /tmp/pwned-sec9;#'`, before any host command used it. No `/tmp/pwned-sec9`. Exit 2 |
| `23-ubuntu-sec9-explicit-http-unknown-service.txt` | As 22, the impostor naming a well-formed service that does not exist, `apache9` | `REFUSED: nexusd names the service apache9, but systemd on the host has no such unit (LoadState: not-found)`: a well-formed name is still checked against the host. Exit 2 |
| `24-ubuntu-sec10-sigkill-site-enabled.txt` | nexusd's `providers.nginx.includeSuffix` set to `.site` and `include /etc/nginx/conf.d/*.site;` added to `nginx.conf` (`exit-statuses.log`); `--yes`, SIGKILL once `3b-add` passed | `1-include` PASS on the `*.site` line; the site `nexus-check-<8 hex>.site` added and enabled, then the kill. Exit 137 |
| `25-ubuntu-sec10-cleanup-before-fix.txt` | **The script before the SEC-10 fix**, `--yes --cleanup` | Red for SEC-10: its state had no suffix, so it removed the `.conf` names (none existed), reloaded nginx with the `.site` file still enabled, removed its state and exited 0. "after 25" still lists `nexus-check-<8 hex>.site`. Exit 0 |
| `26-ubuntu-sec10-cleanup-no-state.txt` | The fixed script, `--yes --cleanup`, with 25's leftover and no state | No state: it asked the provider (`includeSuffix` `.site`), found the site, removed every `nexus-check-<8 hex>.*`, reloaded; `cleanup-removed` PASS, `untouched` PASS. Exit 0 |
| `27-ubuntu-sec10-sigkill-again.txt` | As 24 | Another `.site` left, with its state. Exit 137. The driver then planted `nexus-check-0badc0de.site` (a comment) as another run's leftover |
| `28-ubuntu-sec10-cleanup-other-entry-left.txt` | The fixed script, `--yes --cleanup`, with 27's state | Removed its own `.site` (the suffix from the state, `suffix=.site`); `cleanup-removed` FAIL naming `nexus-check-0badc0de.site`, with a loud error on standard error; `untouched` FAIL too, as a new file appeared in the include directory; the state is kept. Exit 1 |
| `29-ubuntu-sec10-cleanup-again.txt` | After the driver removed the planted file, `--yes --cleanup` with the state 28 kept | Nothing left: `cleanup-removed` PASS, `untouched` PASS, the state removed. Exit 0 |

## What the runs show

- **Both platforms, every check.** With the include directory, mode and
  owner, sha256, the served marker and the master and workers all read back
  without Nexus, every operation's result matched what the host showed. No
  product defect was found.
- **ASM-7, as svcctltest has it, with the invalid text in place for
  hundredths of a second.** `rc-service nginx reload` and `systemctl reload
  nginx` exit 1 for an invalid configuration and 0 for a bind conflict; in
  each case nginx keeps its master and workers. The text is staged as a
  hidden file, renamed over the site, reloaded once, and renamed back in
  one host command with HUP, INT and TERM ignored; the measured time in
  place was 0.01 to 0.04 s (`BAD_SECONDS` in each transcript). The provider's
  `observed.initExitCode` for the bind conflict was 0 on both, the same as
  the direct reload. These are containers (OpenRC 0.63.2 and systemd 259),
  so they confirm only that the script compares correctly; the VM's values
  are the evidence.
- **The token is in no environment.** The script runs itself again once
  without `NEXUS_TOKEN` and reads the token from file descriptor 3. In 14
  the probe read the script's own `/proc/<pid>/environ` and that of every
  descendant it saw (`nexus`, `sh -c`, `nginx -T`, `systemctl`, `sleep`,
  forks of the script itself) while the run lasted, and found the token in
  none. With the re-exec planted out (19) the same probe finds it in the
  script and its children, and `token-env` fails, so neither check passes
  vacuously. The probe counts only reads that returned an environment; a
  process that exited first is listed as not counted.
- **nginx is not left stopped.** With `--with-lifecycle`, the cleanup
  starts nginx whenever it is not started at the end: after a SIGTERM
  during the stop (16), while it was stopped (17), and after a restart that
  failed and left systemd `failed` (18). If `nginx -t` failed at cleanup,
  the script would remove the disposable site, test again, and if it still
  failed print a loud error naming the failure; no run reached that.
- **The snapshot covers what nginx reads.** It takes every path under
  `/etc/nginx` and the include directory with `find -L`, plus every file
  `nginx -T` names (only the `# configuration file` lines; the dumped
  content is not kept), and hashes the targets of symlinks. An edit to a
  file under `/srv` that only an `include` reaches (12) fails `untouched`.
- **No http downgrade, and nothing from nexusd reaches a shell
  unchecked (SEC-9).** Without `NEXUS_URL` the script always uses https pinned to `nexusd tls fingerprint`, so an impostor on nexusd's port is refused before it is asked anything (21), where the script before the fix sent it the token and ran its service name as root (20). With http set on purpose, the service name, the suffixes and the include directory must match strict allowlists (22), and the service must exist on the host (23). Every such value is also quoted where it is used.
- **`--cleanup` cleans whatever the suffix (SEC-10).** The state records the suffixes the run used; the site's removal also takes every `nexus-check-<8 hex>.*`; and every cleanup ends with `cleanup-removed`, which lists the include directory and fails (exit 1, state kept) if any `nexus-check-*` entry remains (28). The script before the fix left a `.site` site enabled and exited 0 (25).
- **The free-port check fails closed** (11): with neither `ss` nor
  `netstat` able to run, the script refuses before any change.
- **Cleanup removes the site first** and bounds every `nexus` call (20 s
  each in cleanup, 60 s otherwise), so a slow or dead nexusd cannot keep
  the disposable site in place.
- **A reload is graceful.** Right after a reload, the old workers can still
  answer on a port the new configuration dropped. The script waits until no
  old worker is still active, the provider's own "confirmed" rule, before it
  reads the port back.
- **Signals and exit codes.** The script exits 130 after any trapped
  signal (SIGINT or SIGTERM), after its cleanup has run.
- **The real VM may differ.** It may lack `curl` and `ss`; the script then
  uses BusyBox `wget` and `netstat`. These runs used curl.
