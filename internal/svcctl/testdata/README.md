# svcctl fixtures

Outputs of the init systems' tools for nginx, captured for svcctl's T0 tests
and for the `svcctltest` fake, which answers every call with these files and
the exit codes in the table below (the `Exit` column is the source of truth
for exit codes; `svcctltest` reads it).

**These are container captures, not bare-metal host captures.** Both hosts
were Docker containers on Docker Desktop for macOS (arm64, Linux kernel
6.8.0-100-generic), each running the distribution's own packages:

- **ubuntu-container**: image `nexus-harness:ubuntu-latest`
  (`sha256:46e19aa69b870ac8aae351ec652b0cd2ded844c4e398282c6612e066c157be84`),
  started like `test/harness/run.sh` does (`docker run -d --privileged
  --cgroupns=host --tmpfs /run --tmpfs /run/lock`), so systemd runs as PID 1.
  nginx installed with `apt-get install nginx` from the Ubuntu archive.
- **alpine-container**: image `alpine:3.21`
  (`sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab`),
  started with `docker run -d --init --privileged --cgroupns=private` (tini as
  PID 1 reaps nginx's exited processes; OpenRC is not PID 1), then
  `apk add openrc nginx`, `mkdir -p /run/openrc`, `touch
  /run/openrc/softlevel` and `openrc sysinit; openrc boot; openrc default`.

Every command ran through `docker exec` as root with exactly the environment
svcctl uses, `env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C`, with stdout
and stderr captured together into the file and `$?` recorded as `Exit`.
Tool paths were absolute (`/usr/bin/systemctl`, `/sbin/rc-service`); they are
shortened in the table.

How the preconditions were produced:

- *unknown directive*: `echo 'bogus_directive;' > /etc/nginx/conf.d/zz-bad.conf`
  (Ubuntu) or `/etc/nginx/http.d/zz-bad.conf` (Alpine).
- *port 8181 held by another process*: a second, independent nginx
  (`nginx -c /tmp/occ/nginx.conf`, listening on 8181) plus
  `echo 'server { listen 8181; }' > <include dir>/zz-bind.conf`. `nginx -t`
  passes; the managed master fails `bind()` when it applies the config.
- *crashed* (OpenRC): `pkill -9 -f "nginx: (master|worker) process"` while
  OpenRC had nginx started.
- *purged* (systemd): `apt-get purge nginx nginx-common; systemctl daemon-reload`.

What the captures establish (ASM-7, L-26, L-27):

- `systemctl reload` runs Ubuntu's `ExecReload=/usr/sbin/nginx -g 'daemon on;
  master_process on;' -s reload`, which parses the config first: an invalid
  config exits 1, but a config the master rejects after parsing (bind
  conflict) exits 0 with the old workers kept.
- The worst case, a HUP-style unit: nginx.org's packaging has `ExecReload=/bin/sh
  -c "/bin/kill -s HUP $(/bin/cat /var/run/nginx.pid)"`, which only signals the
  master. The `systemd/reload-hup*.txt` rows are container captures on the
  Ubuntu container above, with that line installed as a drop-in
  (`/etc/systemd/system/nginx.service.d/hup.conf`: `ExecReload=` then the HUP
  line; `systemctl daemon-reload`), not a capture of nginx.org's own package.
  `systemctl reload` exits 0 with empty output for an invalid config and for a
  bind conflict, and the master keeps the old workers (the parse failure is only
  in `/var/log/nginx/error.log`); only the valid config replaced the workers.
- Alpine's `reload()` runs `nginx -t -q` before `start-stop-daemon --signal
  HUP`: an invalid config exits 1 with the configtest text, a bind conflict
  exits 0 with the old workers kept.
- `rc-service ... start` of a started (or crashed) service prints a warning
  and exits 0 without changing state.
- `rc-service -C nginx status` exits 0 (started), 3 (stopped) or 32 (crashed).

Failed-service listings (system-provider WI 1.4, captured 2026-11-15 UTC).
These rows come from the current T1 harness images, not the hosts above:

- **ubuntu-harness**: image `nexus-harness:ubuntu-latest`
  (`sha256:8151dbce5e8fcd2a47632dc861e92a82159c1707cfddc0db4aab0ac4a780bacd`,
  linux/arm64, kernel 6.8.0-100-generic), started with `docker run -d
  --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock`, so systemd
  runs as PID 1. In that container `getty@tty1.service` and
  `systemd-modules-load.service` fail on their own (`systemctl
  is-system-running` printed `degraded`).
  - *failed*: `systemd-run --unit tc-hp-fail /bin/false` (it printed `Running
    as unit: tc-hp-fail.service`; `systemctl show -p ActiveState -p Result
    tc-hp-fail.service` then printed `ActiveState=failed`, `Result=exit-code`),
    then the capture.
  - *none*: `systemctl reset-failed` (`is-system-running` then printed
    `running`), then the capture.
  - Unit-name limit: in a fresh container of the same image, `systemd-run
    --unit <name> /bin/false` accepted a 255-character `<a...>.service` name
    (listed by `list-units --state=failed` with a first field of 255
    characters) and refused 256 and 300 characters with `Failed to mangle unit
    name: Invalid argument`. ListFailed drops names longer than 255.
- **alpine-harness**: image `nexus-harness:alpine-alpine-latest`
  (`sha256:a084582138555accaec580b638c1452c887ef3129265301fd65c4ddeb4eca609`,
  linux/arm64, kernel 6.8.0-100-generic), started with `docker run -d
  --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock`; BusyBox
  init is PID 1 and runs OpenRC (`rc-status -r` printed `default`).
  `rc-status --version` printed `rc-status (OpenRC [DOCKER]) 0.63.2`;
  `command -v rc-status` printed `/bin/rc-status` (A-17).
  - *none*: the capture on the fresh container, before anything was planted.
  - *crashed*: `/etc/init.d/tc-hp-crash`, a `start-stop-daemon` service
    (`command=/bin/sleep`, `command_args=3600`, `command_background=yes`,
    `pidfile=/run/tc-hp-crash.pid`), `rc-service tc-hp-crash start`, then
    `kill -9` of its pid (`rc-service tc-hp-crash status` printed `* status:
    crashed`, exit 32); and `rc-service nginx start`, then `pkill -9 -f
    "nginx: (master|worker) process"` (`status: crashed`, exit 32); then the
    capture.
  - So A-16 holds on OpenRC 0.63.2: none crashed is exit 1 with empty output,
    and crashed services are exit 0 with one bare name per line.

The commands ran like the ones above: `docker exec <container> env -i
PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C <tool> ...`, combined output into
the file, the `docker exec` exit status recorded as `Exit`. Both containers
were removed afterwards.

| File | Host | Distribution | Init | nginx | Command | Exit | Date |
|---|---|---|---|---|---|---|---|
| `systemd/show-running.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl show --timestamp=unix -p LoadState -p ActiveState -p SubState -p MainPID -p StateChangeTimestamp -- nginx.service` (nginx running) | 0 | 2026-11-13 |
| `systemd/show-stopped.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl show --timestamp=unix -p LoadState -p ActiveState -p SubState -p MainPID -p StateChangeTimestamp -- nginx.service` (nginx stopped after a stop) | 0 | 2026-11-13 |
| `systemd/show-failed.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl show --timestamp=unix -p LoadState -p ActiveState -p SubState -p MainPID -p StateChangeTimestamp -- nginx.service` (after a restart that failed on a bind conflict) | 0 | 2026-11-13 |
| `systemd/show-not-found.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl show --timestamp=unix -p LoadState -p ActiveState -p SubState -p MainPID -p StateChangeTimestamp -- nginx.service` (nginx and nginx-common purged) | 0 | 2026-11-13 |
| `systemd/start.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx stopped, valid config) | 0 | 2026-11-13 |
| `systemd/start-already.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx running) | 0 | 2026-11-13 |
| `systemd/start-failed.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx failed, valid config) | 0 | 2026-11-13 |
| `systemd/start-invalid.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx stopped, unknown directive in conf.d) | 1 | 2026-11-13 |
| `systemd/start-bind-conflict.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx stopped, port 8181 held by another process) | 1 | 2026-11-13 |
| `systemd/start-not-found.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl start -- nginx.service` (nginx purged) | 5 | 2026-11-13 |
| `systemd/restart.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl restart -- nginx.service` (nginx running, valid config) | 0 | 2026-11-13 |
| `systemd/restart-invalid.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl restart -- nginx.service` (nginx running, unknown directive in conf.d) | 1 | 2026-11-13 |
| `systemd/restart-bind-conflict.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl restart -- nginx.service` (nginx running, port 8181 held by another process) | 1 | 2026-11-13 |
| `systemd/restart-not-found.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl restart -- nginx.service` (nginx purged) | 5 | 2026-11-13 |
| `systemd/stop.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl stop -- nginx.service` (nginx running) | 0 | 2026-11-13 |
| `systemd/stop-already.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl stop -- nginx.service` (nginx stopped) | 0 | 2026-11-13 |
| `systemd/stop-failed.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl stop -- nginx.service` (nginx failed (stays failed)) | 0 | 2026-11-13 |
| `systemd/stop-not-found.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl stop -- nginx.service` (nginx purged) | 5 | 2026-11-13 |
| `systemd/reload.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, valid config) | 0 | 2026-11-13 |
| `systemd/reload-invalid.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, unknown directive in conf.d (old workers kept)) | 1 | 2026-11-13 |
| `systemd/reload-bind-conflict.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, port 8181 held by another process (master logs bind() emerg, old workers kept)) | 0 | 2026-11-13 |
| `systemd/reload-stopped.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx stopped) | 1 | 2026-11-13 |
| `systemd/reload-failed.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx failed) | 1 | 2026-11-13 |
| `systemd/reload-not-found.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx purged) | 5 | 2026-11-13 |
| `systemd/reload-hup.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, valid config; ExecReload overridden to the nginx.org HUP form (new workers spawned, old ones gone)) | 0 | 2026-11-13 |
| `systemd/reload-hup-invalid.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, unknown directive in conf.d; ExecReload overridden to the nginx.org HUP form (exit 0, old workers kept)) | 0 | 2026-11-13 |
| `systemd/reload-hup-bind-conflict.txt` | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` (nginx running, port 8181 held by another process; ExecReload overridden to the nginx.org HUP form (exit 0, old workers kept)) | 0 | 2026-11-13 |
| `openrc/exists.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service --exists nginx` (nginx installed) | 0 | 2026-11-13 |
| `openrc/exists-absent.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service --exists nginx` (/etc/init.d/nginx moved away) | 1 | 2026-11-13 |
| `openrc/absent.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx {start,stop,restart,reload,status}` (/etc/init.d/nginx moved away; all five outputs identical) | 1 | 2026-11-13 |
| `openrc/status-started.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx status` (nginx started) | 0 | 2026-11-13 |
| `openrc/status-stopped.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx status` (nginx stopped) | 3 | 2026-11-13 |
| `openrc/status-crashed.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx status` (nginx master and workers killed with SIGKILL behind OpenRC) | 32 | 2026-11-13 |
| `openrc/start.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx start` (nginx stopped, valid config) | 0 | 2026-11-13 |
| `openrc/start-already.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx start` (nginx started) | 0 | 2026-11-13 |
| `openrc/start-crashed.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx start` (nginx crashed (stays crashed)) | 0 | 2026-11-13 |
| `openrc/start-invalid.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx start` (nginx stopped, unknown directive in http.d) | 1 | 2026-11-13 |
| `openrc/start-bind-conflict.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx start` (nginx stopped, port 8181 held by another process) | 1 | 2026-11-13 |
| `openrc/restart.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx started, valid config) | 0 | 2026-11-13 |
| `openrc/restart-invalid.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx started, unknown directive in http.d (ends stopped)) | 1 | 2026-11-13 |
| `openrc/restart-bind-conflict.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx started, port 8181 held by another process (ends stopped)) | 1 | 2026-11-13 |
| `openrc/restart-stopped.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx stopped, valid config) | 0 | 2026-11-13 |
| `openrc/restart-stopped-invalid.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx stopped, unknown directive in http.d) | 1 | 2026-11-13 |
| `openrc/restart-stopped-bind-conflict.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx stopped, port 8181 held by another process) | 1 | 2026-11-13 |
| `openrc/restart-crashed.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx restart` (nginx crashed, valid config (ends started)) | 0 | 2026-11-13 |
| `openrc/stop.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx stop` (nginx started) | 0 | 2026-11-13 |
| `openrc/stop-already.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx stop` (nginx stopped) | 0 | 2026-11-13 |
| `openrc/stop-crashed.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx stop` (nginx crashed (ends stopped)) | 0 | 2026-11-13 |
| `openrc/reload.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` (nginx started, valid config) | 0 | 2026-11-13 |
| `openrc/reload-invalid.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` (nginx started, unknown directive in http.d (old workers kept)) | 1 | 2026-11-13 |
| `openrc/reload-bind-conflict.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` (nginx started, port 8181 held by another process (master logs bind() emerg, old workers kept)) | 0 | 2026-11-13 |
| `openrc/reload-stopped.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` (nginx stopped) | 1 | 2026-11-13 |
| `openrc/reload-crashed.txt` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` (nginx crashed) | 1 | 2026-11-13 |
| `openrc/daemon-record` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `cp /run/openrc/daemons/nginx/001` (nginx started with rc-service; master PID 190) | - | 2026-11-13 |
| `openrc/nginx.pid` | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `cp /run/nginx/nginx.pid` (same start as daemon-record) | - | 2026-11-13 |
| `systemd/list-units-failed.txt` | ubuntu-harness | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl list-units --state=failed --plain --no-legend --no-pager --full` (transient unit tc-hp-fail.service planted with `systemd-run --unit tc-hp-fail /bin/false`; getty@tty1 and systemd-modules-load failed on their own) | 0 | 2026-11-15 |
| `systemd/list-units-failed-none.txt` | ubuntu-harness | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl list-units --state=failed --plain --no-legend --no-pager --full` (after `systemctl reset-failed`; no failed unit) | 0 | 2026-11-15 |
| `openrc/rc-status-crashed.txt` | alpine-harness | Alpine Linux 3.24.4 | OpenRC 0.63.2-r0 | nginx 1.30.6-r0 | `rc-status --crashed` (start-stop-daemon service tc-hp-crash and nginx started, then killed with SIGKILL behind OpenRC) | 0 | 2026-11-15 |
| `openrc/rc-status-crashed-none.txt` | alpine-harness | Alpine Linux 3.24.4 | OpenRC 0.63.2-r0 | nginx 1.30.6-r0 | `rc-status --crashed` (no service crashed) | 1 | 2026-11-15 |
