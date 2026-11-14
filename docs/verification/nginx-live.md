# nginx on real hosts: the Alpine VM (TC-118) and Ubuntu (TC-119)

Work item 7.2 of nginx-provider. This page has the procedure, the script
that runs it (`test/live/nginx-check.sh`), and the transcript template to
fill in from the real runs.

**Status: `pending-live-gate`.** No real-host transcript exists yet. The
only runs so far are container dry runs in the T1 harnesses, in
[nginx-live-dryrun/](nginx-live-dryrun/). They show that the script works
and cleans up after itself. They are not evidence for TC-118 or TC-119.

- [What the check proves](#what-the-check-proves)
- [Before you start](#before-you-start)
- [What the script touches, and what it never touches](#what-the-script-touches-and-what-it-never-touches)
- [Alpine procedure (TC-118)](#alpine-procedure-tc-118)
- [Ubuntu procedure (TC-119)](#ubuntu-procedure-tc-119)
- [If a run is interrupted](#if-a-run-is-interrupted)
- [How the readbacks are done](#how-the-readbacks-are-done)
- [Transcript template](#transcript-template)
- [Known before the real run](#known-before-the-real-run)

## What the check proves

The T1 suite (WI 7.1, [nginx-t1.md](nginx-t1.md)) ran the provider against
the distributions' nginx in harness containers. This check runs the release
tarball against the real hosts: the user's Alpine/OpenRC nginx VM on Proxmox
and a real Ubuntu/systemd host. It records:

- the actual `include` line in `nginx.conf` (ASM-12), which on the VM must
  name `http.d/*.conf` (ASM-3);
- that nexusd, running as root on the VM, manages that VM's own nginx
  (ASM-1): the provider's `mainPid` is the master in the VM's `/proc`;
- every lifecycle and site operation, each read back without Nexus;
- the exit codes of `rc-service nginx reload` and `systemctl reload nginx`
  for an invalid configuration and for a bind conflict (ASM-7), compared
  with the table the `svcctltest` fake answers from
  (`internal/svcctl/testdata/README.md`, the `Exit` column). A different
  real value is a defect in the fake (TC-11), to fix before completion;
- the reload observations: the master PID and the worker PIDs before and
  after each reload, from `/proc`.

## Before you start

On each host:

- nexusd installed from the release tarball with its `install.sh`
  ([install.md](../install.md)), started, with the nginx provider enabled
  in `/etc/nexus/nexusd.yaml` (`providers.nginx.enabled: true`, see
  [nginx.md](../nginx.md#configuration)) and reloaded.
- nginx from the distribution's package, **running**. The script refuses
  to run against a stopped nginx, and never starts one it did not stop.
- `nginx -t` passing. The script refuses to change anything otherwise.
- curl, or BusyBox `wget` (Alpine has it), for the readback of the test
  port. For the bind-conflict cases, one of `perl` (Debian and Ubuntu
  always have it), `python3`, or BusyBox `nc` (Alpine has it) holds a port.
- A token with the nginx scopes, created on the host:

  ```
  nexusd token create --name nginx-check --scope nginx:read --scope nginx:write --scope nginx:config --expires 1d
  ```

  Pass it in `NEXUS_TOKEN`. The script never puts it in an argument or the
  transcript: it reaches `nexus host add --token-stdin` on standard input,
  in a throwaway client configuration that the script deletes at the end.
  The script also checks that the token's value is nowhere in the
  transcript (result `token-absent`). Revoke it when you are done:
  `nexusd token revoke nginx-check`.

The `nexus` client from the same release runs either on the host or on the
machine you run the script from:

- **From the Mac, over SSH** (`--ssh root@<vm>`): the host commands run
  over `ssh -o BatchMode=yes`, as root (or a user with `sudo` that needs no
  password). The `nexus` CLI runs on the Mac. Set `NEXUS_URL` to nexusd's
  address as the Mac reaches it, for example `https://<vm>:7443`; nexusd
  must listen there (`listen` in `/etc/nexus/nexusd.yaml`). The certificate
  is pinned to what `nexusd tls fingerprint` prints on the host, read over
  the same SSH connection. If the host is already in your client
  configuration, pass `--nexus-host <name>` instead of `NEXUS_TOKEN` and
  `NEXUS_URL`. Extra ssh options go in `NEXUS_CHECK_SSH_OPTS`, such as
  `"-p 2222 -i ~/.ssh/vm"`.
- **On the host**, as root: put `nexus` on the `PATH`, or pass
  `--nexus PATH`. The script finds nexusd at the first `listen` address in
  `/etc/nexus/nexusd.yaml`.

## What the script touches, and what it never touches

Without `--yes`, the script changes nothing. It checks the preconditions,
records the facts (the include line, `nginx -t`, the two spare ports, and a
sha256 of every file under `/etc/nginx` and the include directory), prints
the plan, and exits with status 2.

| What | When | Undone by |
| --- | --- | --- |
| One disposable site, `nexus-check-<8 hex>.conf` in the include directory (`/etc/nginx/http.d` on Alpine, `/etc/nginx/conf.d` on Ubuntu), listening only on `127.0.0.1:<port>` with `server_name nexus-check-<8 hex>.invalid`. The port is checked first: nothing listens on it (`ss -tln`, or `netstat -tln`) and no file under `/etc/nginx` or the include directory names it in a `listen` directive | Steps 3 to 6 | Removed with `rm` as root on the host (the provider cannot delete, OOS-4), then `nginx -t` and a reload |
| nginx reloads | Every site change of steps 3 and 4 (through Nexus), `nexus nginx reload` in step 2, the two ASM-7 reloads of step 5, and one after removing the site | Not undone. A reload keeps the master and replaces the workers gracefully: connections are not dropped |
| A holder process on a second spare loopback port, so that a write needing that port is a bind conflict. It ends by itself after 600 s | Steps 4 and 5 | Killed as soon as the step ends |
| `/var/lib/nexus-nginx-check/` (0700): the run's state and the snapshot | The whole run | Removed |
| `nexus nginx stop`, `start` and `restart` | **Only with `--with-lifecycle`.** These take every site on this nginx offline for a few seconds. Run them in a maintenance window | nginx is left running. If the run stops while nginx is stopped, the EXIT trap starts it |

Hard rules the script enforces:

- It never edits `nginx.conf` or any other site, and never disables,
  restarts or stops anything without `--with-lifecycle`.
- It records a sha256 (with type, mode, owner and size) of every other file
  under `/etc/nginx` and the include directory before the first change, and
  compares after the site is gone. Any difference fails `untouched`
  loudly, with the diff in the transcript. `nginx-conf` compares
  `nginx.conf` on its own.
- Without `--with-lifecycle`, nginx's master must be the same process at
  the end as at the start (`master`).
- The ASM-7 cases of step 5 write only to the disposable site. Each bad text
  stays in place only until nginx has acted on the reload (its error-log
  line, at most 5 s); then the valid text is written back at once.

The operations stay in nexusd's operation history and audit log, under the
token's name.

## Alpine procedure (TC-118)

From a checkout on the Mac, with the `nexus` client of the release on the
`PATH`:

1. Read the plan. This changes nothing:

   ```
   NEXUS_TOKEN=<token> NEXUS_URL=https://<vm>:7443 \
     sh test/live/nginx-check.sh --ssh root@<vm> --transcript tc118-0-plan.txt
   ```

   Check the chosen ports, the include line it found (`1-include`) and the
   snapshot of `/etc/nginx`.

2. The check without the lifecycle steps. This only reloads nginx:

   ```
   NEXUS_TOKEN=<token> NEXUS_URL=https://<vm>:7443 \
     sh test/live/nginx-check.sh --ssh root@<vm> --yes --transcript tc118-1-check.txt
   ```

   It took about 40 seconds in the dry runs. It exits 3, not 0, because
   `2d-stop`, `2e-start` and `2f-restart` are `NOT-RUN`.

3. The lifecycle steps, in a maintenance window. **Every site on the VM is
   offline from the stop until the start, a few seconds:**

   ```
   NEXUS_TOKEN=<token> NEXUS_URL=https://<vm>:7443 \
     sh test/live/nginx-check.sh --ssh root@<vm> --yes --with-lifecycle --transcript tc118-2-lifecycle.txt
   ```

   This repeats step 2's checks and adds stop, start and restart. Skip it if
   a short outage is not acceptable; TC-118's step 2 then stays partly
   unverified, and the template says so.

4. Revoke the token. Copy the transcripts into
   `docs/verification/nginx-live/` and fill in the
   [template](#transcript-template).

To run it on the VM instead: copy the script and the `nexus` client there,
then, as root, `NEXUS_TOKEN=<token> sh nginx-check.sh --yes ...` without
`--ssh` and without `NEXUS_URL`.

## Ubuntu procedure (TC-119)

The same script and the same steps, on a real Ubuntu host running the
release tarball, with `systemctl` as the init system and
`/etc/nginx/conf.d` as the include directory:

```
NEXUS_TOKEN=<token> sh nginx-check.sh --transcript tc119-0-plan.txt
NEXUS_TOKEN=<token> sh nginx-check.sh --yes --transcript tc119-1-check.txt
NEXUS_TOKEN=<token> sh nginx-check.sh --yes --with-lifecycle --transcript tc119-2-lifecycle.txt
```

(as root on the host, or from the Mac with `--ssh` and `NEXUS_URL` as
above). The ASM-7 expectation is that `systemctl reload nginx` exits 0 for
the bind conflict (`5b-bind-exit`).

**Exit status:** 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or `--yes` is missing); 3 nothing failed but a check
was `NOT-RUN`. Each `RESULT <id>: <status> - <detail>` line is one Expected
item, and the summary at the end lists them all.

## If a run is interrupted

Press Ctrl-C **once**. The script's EXIT trap then ignores further
interrupts and:

1. waits for the nginx operation it submitted, and any other nginx operation
   still queued or running, to end (a queued one it submitted is canceled),
   so that no rollback can put the site back after it is removed;
2. kills the holder process, by its recorded PID or, if the interrupt cut
   the recording short, by its exact command line on the conflict port;
3. removes the disposable site's files;
4. runs `nginx -t`. If it passes: starts nginx if this run had stopped it;
   otherwise reloads nginx, **only if the site was ever enabled**. If it
   fails, it does neither and says so;
5. compares the snapshot, prints the summary, and removes its state.

If even that is cut short (a second terminal closed, a `kill -9`, the SSH
link lost), the state stays in `/var/lib/nexus-nginx-check`. A new run then
refuses to start, and this undoes it:

```
NEXUS_TOKEN=<token> NEXUS_URL=https://<vm>:7443 \
  sh test/live/nginx-check.sh --ssh root@<vm> --yes --cleanup --transcript tc118-cleanup.txt
```

Without the state file, `--cleanup` looks for `nexus-check-<8 hex>` files
in the include directory and removes those only.

## How the readbacks are done

Every result is read back without Nexus, on the host:

- **The init system:** `rc-service -C nginx status` (0 started, 3 stopped)
  on OpenRC; `systemctl is-active nginx.service` on systemd. The script
  calls them, and the ASM-7 reloads, with the argv and environment svcctl
  uses (`env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C`), so the exit
  codes compare directly with the fake's.
- **Processes:** the master's PID from nginx's pid file (the `pid`
  directive, or `nginx -V`'s `--pid-path`), checked to be titled
  `nginx: master process` in `/proc/<pid>/cmdline`; the workers are its
  children titled exactly `nginx: worker process`. A reload is applied when
  the master stays and no old worker is still an active worker.
- **Files:** `ls -ln` (mode and owner `-rw-r--r-- 0 0`) and `sha256sum` of
  `<site>.conf` and `<site>.conf.disabled`. The sha256 must equal both the
  result's `sha256` and the sha256 of the text the script sent, computed on
  the machine running the script.
- **Serving:** `curl` (or `wget`) of `http://127.0.0.1:<port>/`, which
  answers `nexus-check <8 hex> M<n>` for the text `M<n>` that is live.
  `refused` means nothing listens: the site is disabled, removed, or nginx
  is stopped.
- **The include line:** the script reads `nginx.conf`, drops comments, and
  tracks the `{ }` blocks, so it reports each `include` with its context. It
  passes when one inside `http` names the include directory with the suffix
  glob, an absolute path or one relative to `/etc/nginx` (the user's
  `include http.d/*.conf;`). It does not follow nested includes.

The steps run in this order: 1 (facts), 3a to 3c (whatIf, add, update), 2
(the lifecycle, while the site serves `M2`), 3d to 3g (disable, update while
disabled, whatIf of an invalid text, enable), 4, 5, 6.

## Transcript template

Fill in one table per host from the real transcripts. "Observed" is the
value in the `RESULT` line; "Line" is its line number in the transcript.

**Alpine VM (TC-118)**

- Host: ______ (Proxmox VM; Alpine release, kernel and nginx version from step 1)
- Date (UTC): ______
- Release tarball sha256 (nexusd and nexus): ______
- Script: `test/live/nginx-check.sh` sha256 ______ (transcript header)
- Transcripts: `tc118-0-plan.txt`, `tc118-1-check.txt`, `tc118-2-lifecycle.txt`
- Run from: ☐ the Mac with `--ssh` ☐ the VM
- Final verdict: ______ (`pending-live-gate` until this table is complete)

| TC-118 Expected | Result id | Observed | Line | Verdict |
| --- | --- | --- | --- | --- |
| Step 1. The include line names `http.d/*.conf` (ASM-3, ASM-12): record it verbatim | `1-include` | line ___: `include ______;` | | |
| Step 1. The provider reports `running` under OpenRC, with `includeDir` `/etc/nginx/http.d` | `1-provider` | | | |
| Step 1. nexusd runs as root on the VM and manages its own nginx (ASM-1): `mainPid` = the VM's master | `1-asm1` | | | |
| Step 1. `nginx -t` passes before any change | `1-nginx-t` | | | |
| Step 2. Config test: `valid: true` = host `nginx -t` exit 0 | `2a-test` | | | |
| Step 2. Start of a running nginx: `changed: false`, `rc-service` started, master unchanged | `2b-start-running` | | | |
| Step 2. Reload: verdict `confirmed`, same master, every worker replaced; `initExitCode` recorded | `2c-reload` | initExitCode ___ | | |
| Step 2. Stop: `rc-service` stopped, no master, port refused (`--with-lifecycle`) | `2d-stop` | | | |
| Step 2. Start: running, `mainPid` = the new master, port serves (`--with-lifecycle`) | `2e-start` | | | |
| Step 2. Restart: a new master, running, port serves (`--with-lifecycle`) | `2f-restart` | | | |
| Step 3. whatIf add: `valid: true`, no file, workers unchanged | `3a-whatif` | | | |
| Step 3. Add: result sha256 = sent text's = `sha256sum`; `root 0644`; curl serves M1; reload confirmed | `3b-add` | | | |
| Step 3. Update with `--if-match`: new sha256 everywhere, `previousSha256` = M1's; curl serves M2 | `3c-update` | | | |
| Step 3. Disable: only `.conf.disabled`, same sha256; curl refused | `3d-disable` | | | |
| Step 3. Update while disabled: `reloaded: false`, new sha256 on the disabled file; still refused | `3e-update-disabled` | | | |
| Step 3. whatIf of an invalid text: `valid: false`, nothing changed | `3f-whatif-invalid` | | | |
| Step 3. Enable: `.conf` with M3's sha256; curl serves M3 | `3g-enable` | | | |
| Step 4. Invalid write: `nginx-config-invalid`, `rolledBack: true`; curl shows the previous content (M3) | `4a-invalid` | | | |
| Step 4. Port-conflict write: `nginx-reload-failed`, `rolledBack`, `observed` rejected, `rollbackReload` confirmed; curl shows M3 | `4b-conflict` | | | |
| Step 4. The provider's `observed.initExitCode` for the bind conflict vs the fake (0) | `4b-initexit` | ___ vs 0 | | |
| Step 5. `rc-service nginx reload` exit code, invalid config, vs svcctltest `openrc/reload-invalid.txt` (1) | `5a-invalid-exit` | ___ vs 1 | | |
| Step 5. `rc-service nginx reload` exit code, bind conflict, vs svcctltest `openrc/reload-bind-conflict.txt` (0); workers kept | `5b-bind-exit` | ___ vs 0 | | |
| Step 6. The site is gone; the port is refused after the reload | `6-removed` | | | |
| Every other file under `/etc/nginx` and `http.d` byte-identical | `untouched` | | | |
| `nginx.conf` unchanged | `nginx-conf` | | | |
| nginx's master unchanged (runs without `--with-lifecycle`) | `master` | | | |
| The token is not in the transcript | `token-absent` | | | |
| Commanded equals observed for every step | all of the above | | | |

**Ubuntu host (TC-119)**

- Host: ______ (Ubuntu release, kernel, systemd and nginx version from step 1)
- Date (UTC): ______
- Release tarball sha256: ______
- Script sha256: ______
- Transcripts: `tc119-0-plan.txt`, `tc119-1-check.txt`, `tc119-2-lifecycle.txt`

| TC-119 Expected | Result id | Observed | Line | Verdict |
| --- | --- | --- | --- | --- |
| As TC-118, with `systemctl` readback and `conf.d`: the include line names `/etc/nginx/conf.d/*.conf` | `1-include` | | | |
| `1-provider`, `1-asm1`, `1-nginx-t`, `2a` to `2f`, `3a` to `3g`, `4a`, `4b`, `6-removed`, `untouched`, `nginx-conf`, `master`, `token-absent` | as TC-118 | | | |
| `4b-initexit`: the provider's `observed.initExitCode` for the bind conflict vs the fake (0) | `4b-initexit` | ___ vs 0 | | |
| `systemctl reload nginx` exit code, invalid config, vs svcctltest `systemd/reload-invalid.txt` (1) | `5a-invalid-exit` | ___ vs 1 | | |
| `systemctl reload nginx` exit code, bind conflict: **ASM-7 expects 0**, svcctltest `systemd/reload-bind-conflict.txt` (0) | `5b-bind-exit` | ___ vs 0 | | |

A mismatch in any `vs` row is a defect in the svcctltest fake (TC-11): fix
`internal/svcctl/testdata/README.md` and its capture before completion.

## Known before the real run

From the container dry runs ([nginx-live-dryrun/](nginx-live-dryrun/README.md)):

- Every check passed on both harnesses (Alpine 3.24.4, OpenRC 0.63.2, nginx
  1.30.6, run from the Mac over SSH; Ubuntu 26.04, systemd 259, nginx
  1.28.3, run on the host), except `1-host`, which fails in every container
  by design. The reload exit codes matched the svcctltest table on both:
  invalid 1, bind conflict 0. No product defect was found.
- On Ubuntu, nginx is built with `--error-log-path=stderr`, so the parse
  error of an invalid reload goes to the journal, not to
  `/var/log/nginx/error.log`. The script then waits its full 5 s before it
  writes the valid text back, and shows `journalctl -u nginx.service`. The
  bind failure is in the error log on both.
- A reload is graceful: right after it, old workers can still answer on a
  port the new configuration dropped. The script waits for the reload to be
  applied (no old worker still active) before it checks that the port is
  refused.
- Interrupted runs were exercised: Ctrl-C at step 5 with the holder up,
  SIGINT with an operation in flight, SIGTERM with nginx being stopped by
  `--with-lifecycle` (the trap started it again), and `kill -9` followed by
  a refused run and `--cleanup`. Each ended with the site gone, nginx
  running, no holder process, and the "user" sites byte-identical.
- The two guards were shown to fail when they should: a user file edited
  during the run fails `untouched` with the diff, and an `nginx.conf`
  without the include fails `1-include`. A broken user configuration is
  refused before any change.
- The VM may lack `curl` and `ss`. The script then uses BusyBox `wget` and
  `netstat`; an earlier iteration passed every check on an Alpine container
  with both hidden.
