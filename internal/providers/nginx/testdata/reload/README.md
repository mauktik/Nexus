# Reload verification fixtures

`/proc` scans of real nginx around one reload each, captured for reload
verification's T0 tests (`reloadverify_test.go`) and for the fake
`ProcReader` that the lifecycle, toggle and write tests replay. The table
below is the provenance of every file; `TestReloadFixtureProvenance` parses
it.

**These are container captures, not bare-metal host captures.** Both hosts
were Docker containers on Docker Desktop for macOS (arm64, Linux kernel
6.8.0-100-generic, `CLK_TCK` 100), each running the distribution's own
packages:

- **ubuntu-container**: image `nexus-harness:ubuntu-latest`
  (`sha256:a8f6d1fc35185a76e405c59a56d96828294f0c45e4bd25e5be3d910228a3442a`),
  started like `test/harness/run.sh` does (`docker run -d --privileged
  --cgroupns=host --tmpfs /run --tmpfs /run/lock`), so systemd runs as PID 1.
  nginx and curl installed with `apt-get install nginx curl` from the Ubuntu
  archive. The unit's reload is `ExecReload=/usr/sbin/nginx -g 'daemon on;
  master_process on;' -s reload`.
- **alpine-container**: image `alpine:3.21`
  (`sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab`),
  started with `docker run -d --init --privileged --cgroupns=private` (tini
  as PID 1; OpenRC is not PID 1), then `apk add openrc nginx curl`, `mkdir -p
  /run/openrc`, `touch /run/openrc/softlevel` and `openrc sysinit; openrc
  boot; openrc default`.

## How the captures were made

1. `scripts/setup.sh <includeDir>` (`/etc/nginx/conf.d` on Ubuntu,
   `/etc/nginx/http.d` on Alpine) sets `worker_processes 2;`, so every
   generation has two workers and a crashed-and-respawned worker cannot look
   like a new generation. It adds `00-capture.conf`: a `proxy_cache_path`, so
   the master also has a cache manager and a cache loader (which a classifier
   must not count as workers), and a server on port 8080 serving a 4 MiB file
   at `limit_rate 1k`. It also writes the configuration of a second,
   independent nginx that listens on 8181.
2. `capture/main.go` was built with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0
   go build` and copied to `/capture`. It scans `/proc/<pid>` every 100 ms
   and keeps the raw `stat` line and raw `cmdline` (NULs included) of every
   process whose stat comm is `nginx` (`-comm nginx`). It scans for 500 ms,
   runs the init system's reload with svcctl's exact argv and environment
   (`env` `PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C`), records its exit
   status and output, and scans for 3 s more.
3. `scripts/run-case.sh <platform> <case>` runs one case through `docker
   exec` as root: it removes earlier `zz-*.conf` files, stops nginx, kills
   any leftover nginx process, starts nginx fresh, waits 1 s, sets up the
   case, records `nginx -t`, and runs the capture against the init system's
   main PID (`systemctl show -p MainPID --value nginx.service` on Ubuntu,
   `/run/nginx/nginx.pid` on Alpine). The cases:
   - *valid*: nothing changed; the reload applies the same valid config.
   - *long-conn*: `curl -s -o /dev/null http://127.0.0.1:8080/big` started
     1 s before, so one old worker is still sending at `limit_rate 1k` and
     stays `nginx: worker process is shutting down`.
   - *invalid*: `echo 'bogus_directive;' > <includeDir>/zz-bad.conf`.
     `nginx -t` fails, and so does the reload.
   - *bind-rejected*: the second nginx (`nginx -c /tmp/occ/nginx.conf`)
     holds 8181, and `echo 'server { listen 8181; }' >
     <includeDir>/zz-bind.conf`. `nginx -t` passes (it does not bind); the
     master logs `bind() to 0.0.0.0:8181 failed (98: Address already in
     use)` (Ubuntu) or `(98: Address in use)` (Alpine) and `still could not
     bind()`, and keeps its old workers. The second master and its worker
     are in the scans, so a classifier that counts any `nginx: worker
     process` fails.
   - *master-killed*: `capture -kill-master` sends SIGKILL to the master as
     soon as the reload command returns. systemd then removes the rest of the
     unit's cgroup; on Alpine the workers are orphaned to PID 1.
   The Ubuntu cases ran before two cleanup lines were added to
   `run-case.sh` for Alpine (`pkill -9 -f '^nginx: '` and `rc-service nginx
   zap`: busybox `pkill -x nginx` does not match the retitled orphans of a
   killed master); they change only the cleanup between cases.
4. `scripts/exe-replace.sh <platform>` records `readlink /proc/<master>/exe`
   before and after the nginx binary is replaced in place without a restart
   (L-53): `apk upgrade nginx` (nothing to upgrade on that day, link
   unchanged) and then `apk fix nginx`, which reinstalls the package, on
   Alpine; `cp -p` and `mv` over `/usr/sbin/nginx` on Ubuntu. Both show
   `/usr/sbin/nginx (deleted)` afterwards, with the master's title
   unchanged.

Each `.jsonl` file is one header line, then one line per scan:

```
{"capture":"ubuntu-valid","comm":"nginx","intervalMs":100,"masterPid":768,"masterExe":"/usr/sbin/nginx","reloadArgv":[...],"reloadSentMs":500,"reloadDoneMs":519,"initExitCode":0,"reloadOutput":"",...}
{"seq":0,"ms":0,"procs":[{"pid":768,"stat":"768 (nginx) S 1 ...","cmdline":"nginx: master process ...\u0000..."},...]}
```

`ms` counts from the first scan; scans before `reloadSentMs` are the
generation before the reload. `procs` is `null` when no nginx process was
left. The header's `initExitCode` equals the `Exit` column below (the test
checks it).

What the captures establish (FR-6, ASM-7, L-26, L-53):

- A reload that applies starts new workers within one scan (100 ms) and the
  old workers either exit within the next scan or, with a connection still
  open, stay titled `nginx: worker process is shutting down`. Between the
  two, one scan shows old and new workers side by side, all titled `nginx:
  worker process`.
- A rejected reload (bind conflict) leaves every worker's (pid, start time)
  and title unchanged, and both init systems exit 0. Only the invalid config
  gives a non-zero exit (1 on both), with the generation unchanged.
- A replaced binary shows as `<path> (deleted)` in the master's `exe` link.

Synthetic sequences are edits of a captured file, made by the named script;
the `Edited line` column gives the only line that differs from `Base`.

| File | Kind | Host | Distribution | Init | nginx | Reload command | Exit | Capture command | Base | Edited line | Date |
|---|---|---|---|---|---|---|---|---|---|---|---|
| `ubuntu-valid.jsonl` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 0 | `scripts/run-case.sh ubuntu valid` | - | - | 2026-11-13 |
| `ubuntu-long-conn.jsonl` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 0 | `scripts/run-case.sh ubuntu long-conn` | - | - | 2026-11-13 |
| `ubuntu-invalid.jsonl` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 1 | `scripts/run-case.sh ubuntu invalid` | - | - | 2026-11-13 |
| `ubuntu-bind-rejected.jsonl` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 0 | `scripts/run-case.sh ubuntu bind-rejected` | - | - | 2026-11-13 |
| `ubuntu-master-killed.jsonl` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 0 | `scripts/run-case.sh ubuntu master-killed` | - | - | 2026-11-13 |
| `ubuntu-respawn.jsonl` | synthetic | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | `systemctl reload -- nginx.service` | 0 | `python3 -I scripts/make-respawn.py` | `ubuntu-bind-rejected.jsonl` | 37 | 2026-11-13 |
| `alpine-valid.jsonl` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` | 0 | `scripts/run-case.sh alpine valid` | - | - | 2026-11-13 |
| `alpine-long-conn.jsonl` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` | 0 | `scripts/run-case.sh alpine long-conn` | - | - | 2026-11-13 |
| `alpine-invalid.jsonl` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` | 1 | `scripts/run-case.sh alpine invalid` | - | - | 2026-11-13 |
| `alpine-bind-rejected.jsonl` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` | 0 | `scripts/run-case.sh alpine bind-rejected` | - | - | 2026-11-13 |
| `alpine-master-killed.jsonl` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | `rc-service -C nginx reload` | 0 | `scripts/run-case.sh alpine master-killed` | - | - | 2026-11-13 |
| `exe-after-replace-ubuntu.txt` | captured | ubuntu-container | Ubuntu 26.04.1 LTS | systemd 259 (259.9-0ubuntu0.1) | nginx 1.28.3-2ubuntu1.12 | - | - | `scripts/exe-replace.sh ubuntu` | - | - | 2026-11-13 |
| `exe-after-replace-alpine.txt` | captured | alpine-container | Alpine Linux 3.21.10 | OpenRC 0.55.1-r2 | nginx 1.26.3-r3 | - | - | `scripts/exe-replace.sh alpine` | - | - | 2026-11-13 |
