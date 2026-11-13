# NFR measurements

Measured numbers for the non-functional requirements NFR-1 (footprint),
NFR-2 (responsiveness), NFR-3 (dependency failure) and NFR-5
(compatibility), from work item 10.3. The tests that take them are
`test/integration/footprint_test.go`, `latency_test.go` and
`compat_test.go` (TC-187 to TC-192). Run them with:

```
make nfr          # release binary sizes, then the T1 tests matching ^TestNFR
make nfr-sizes    # release binary sizes only
```

Every test logs its numbers as lines starting with `NFR`. The T1 numbers
below come from an **arm64** VM. They are repeated on the real x64 host at
work item 11.1. The section "Not representative of the x64 host" lists the
numbers that T1 cannot settle.

## Summary

T1, measurement run 1 (`make nfr`, 2026-11-10 03:41 to 03:55 UTC). Run 2
is the same tests inside the full `make test-harness` that closed Phase 10.
See "Run 2" below.

Run 2 met every target too. Its numbers are below. Its Docker reads were
faster because that Engine used the classic image store.

| NFR | Target | T1 run 1 | Verdict | Re-measure at 11.1 |
| --- | --- | --- | --- | --- |
| NFR-1 | Idle RSS under 50 MiB, 50 containers | max 19.70 MiB (mean 19.48, VmHWM 19.96) over 300 one-second samples | met | yes (amd64 heap and page sizes differ) |
| NFR-1 | Idle CPU under 1% of one core over 5 min | 0.0028% (cgroup `usage_usec` 8.26 ms in 300.008 s); /proc ticks 1 = 0.0033% | met | yes |
| NFR-1 | Startup to ready under 2 s | exec to READY=1: p50 71.4 ms, max 79.1 ms over 10 starts | met | yes |
| NFR-1 | Each binary under 40 MiB | nexusd linux/amd64 12.49 MiB; nexus darwin/arm64 9.03 MiB; nexus linux/amd64 9.53 MiB | met | no (these are the release artifacts) |
| NFR-1 | No background Docker or apt activity at idle | 0 Engine API requests, 0 child processes, 0 apt/dpkg processes in 5 min | met | yes |
| NFR-2 | Read p95 under 200 ms | slowest: `docker.containers.list` p95 112.2 ms; all 13 reads met | met | yes (see "docker.containers.list") |
| NFR-2 | Cached `apt.upgradable.list` p95 under 200 ms | p95 0.1 ms; cold first calls reported apart: p95 25.2 ms | met | yes, cold calls especially |
| NFR-2 | Acknowledgement p95 under 500 ms, audit and fsync included | slowest: `docker.containers.start` p95 11.1 ms; 600 of 600 acks audited | met | yes (fsync on the VM's disk) |
| NFR-2/3 | An in-flight apt operation does not block other providers | during a real gated upgrade: Docker list p95 115.7 ms, script start ack p95 8.8 ms | met | yes |
| NFR-3 | Failures stay in their provider | 6 failure modes: structured errors from the failing provider only, same PID, no panic | met | yes |
| NFR-5 | Additive changes do not break clients; clear capability error | existing commands work both ways; new op on the older nexusd exits 4 naming `route-not-found` | met | no (behavior, not timing) |

## Environment

- **Mac:** Apple M2 Max, 12 cores, 64 GiB. It was not idle. The 1-minute
  load average sampled every 30 s during run 1 ranged from 4.52 to 20.06,
  with an iOS simulator app, Spotlight indexing and a browser running.
  NFR-2 asks for "no other load". The Mac's load is outside the VM's
  CPUs, but it can still delay the VM's virtual CPUs.
- **VM:** Colima `default`, aarch64, 4 CPUs, 8 GiB, kernel
  6.8.0-100-generic, host Docker 29.2.1, Docker context `colima`.
- **T1 harness:** a privileged systemd container FROM `ubuntu:latest`
  (Ubuntu 26.04.1 LTS, systemd 259, apt 3.2.0, dpkg 1.23.7ubuntu1), with
  `--cgroupns=host`. The nested Engine is Ubuntu's `docker.io` 29.1.3 (API
  1.52, linux/arm64) on anonymous volumes. The VM's load average
  (`/proc/loadavg`, shared with the container) was 0.13 to 1.39 at the
  start and end of each test in run 1.
- **nexusd:** the harness build (`go build -trimpath`, linux/arm64,
  unstripped, 17,495,126 bytes, version `dev`), run as `nexusd.service`
  under the shipped unit's hardening. A drop-in replaces only ExecStart and
  sets Restart=no. The docker, apt and scripts providers are all enabled.
  The token holds every scope.
- **Containers:** 50 real containers, `busybox:latest` running
  `sleep 86400`, named `nfr-00` to `nfr-49`, plus whatever a test adds
  (the registry case adds one container and two registry containers).
- **Apt:** the fixture repository is the only apt source (`file:`, a
  handful of packages). `nxplain` 1.0 is installed and 2.0 is upgradable.
  The image ships no other indexes.
- **Runs:** two dry runs with a 30 s idle window (not measurements; used to
  check the tests), then measurement run 1 (`make nfr`), then run 2 inside
  the full harness run. The release sizes come from `make nfr-sizes` at
  commit 3fcd155 plus this work item's uncommitted files.

## Method

- **Startup (TC-187):** nexusd.service is stopped and started 10 times.
  The value is systemd's own `ActiveEnterTimestampMonotonic` minus
  `ExecMainStartTimestampMonotonic`, that is, from the main process's exec
  to READY=1 (Type=notify). The wall time of `systemctl start` is also
  logged.
- **Idle RSS and CPU (TC-187):** after the last start, a warm-up acts as a
  client: one read of each provider, a script run and a Docker start, each
  run to its end. After a 30 s settle, the window is 5 minutes. RSS is
  `VmRSS` from `/proc/<pid>/status`, sampled every second. CPU is the
  unit cgroup's `cpu.stat usage_usec` delta divided by the window, which
  counts children too, cross-checked with utime+stime from
  `/proc/<pid>/stat` (USER_HZ 100).
- **No background activity (TC-188), in the same window:**
  - **Docker:** the nested Engine's debug log is turned on by a reload
    (`debug: true` in daemon.json, SIGHUP, so the containers keep
    running). Every API request it serves is then a `module=api` debug
    line in docker.service's journal (`msg="handling GET request"` on
    Engine 29). The test runs no `docker` command during the window.
  - **Processes:**
    - `cminflt` and `cutime+cstime` of nexusd. These grow whenever nexusd
      reaps a child, however short-lived.
    - The unit cgroup's `cgroup.procs`, sampled every 50 ms.
    - A 50 ms scan of `/proc` for any process named like an apt or dpkg
      binary, whatever its parent.
    - The journals of nexusd and of any `nexus-op-*` unit.
  - **Controls:** the same detectors must see activity during the warm-up,
    or the test fails. In run 1 they saw 56 Engine requests and cminflt
    +11,755.
  - **Planted defects:** a red check in a scratch copy added a background
    loop to nexusd: a Docker `/_ping` and a `dpkg-query` every 5 s, 64 MiB
    of live ballast, and about 20 ms of spinning per second. It turned
    every guard red: RSS 86.80 MiB, CPU 2.13%, 6 Engine requests,
    cminflt +3,252. The 50 ms `/proc` scan missed the short `dpkg-query`
    runs, and cminflt caught them. The scan is a supplement; cminflt is
    the detector that decides.
- **Latency (TC-189):** 200 sequential samples per op, one keep-alive
  HTTP/1.1 client over loopback, plain HTTP (`tls.mode: off`). The time
  runs from sending the request to reading the whole body. The p95 is the
  nearest rank: the ⌈0.95·200⌉ = 190th smallest sample. All 200 samples
  count, including the first on a new connection.
  - **Acknowledgements:** the time to the 202. Each operation runs to
    Succeeded before the next sample, so no lane or queue is in play. Each
    acknowledged operation's ID is then looked up in `audit.jsonl`, which
    shows that the 202 followed an appended and fsynced audit record.
  - **Cold `apt.upgradable.list` (A-38):** the first call after start, then
    10 calls each made right after the mtime of `/var/lib/dpkg/status` is
    bumped, which invalidates the stat-validated cache.
- **No blocking (TC-190):** an upgrade of `nxplain` to a 3.0 whose postinst
  waits on a gate file. It is started through the API and confirmed
  Running, with dpkg holding the frontend lock, before and after the
  measurements. During it: 200 samples each of Docker list and get and
  scripts list and get, and 200 script start acknowledgements.
- **Failure matrix (TC-191):** one nexusd, with the same PID asserted after
  every case, takes four failures in turn. In each case, every other
  provider's read is sampled 20 times (p95 under 200 ms), and nexusd's
  journal is checked for "panic". Docker hangs are tested on separate
  nexusd instances behind a gate socket, `providers.docker.socket`
  pointing at a unix socket the test serves.
  - **Docker stopped:** `systemctl stop docker.socket docker.service`,
    then started again. The recovery is timed without restarting nexusd.
  - **Registry stopped:** a container from the local registry, which is
    then stopped, and `check-updates`.
  - **dpkg lock held:** a real `dpkg -i` waiting in a gated postinst, and
    an upgrade with `lockWait: 5s`.
  - **Failing script:** the `fail.sh` fixture.
  - **Docker hung after a healthy preflight:** the gate forwards to the
    Engine until it is switched to hang. Hung, it accepts and never
    answers, and connections already forwarded are cut from the Engine and
    left open. Three Docker reads are sent at once while the other
    providers are sampled. Afterwards, nexusd's open fds and the gate's
    open connections are compared with the baseline, and the gate is
    switched back.
  - **Docker hung from the start:** the gate hangs before nexusd starts.
- **Compatibility (TC-192):**
  - **Older nexusd:** the integration test binary run as a daemon
    (`NEXUS_INTEGRATION_AS_OLDER_NEXUSD=1`). It is the production
    `daemon.New` with the real docker, apt and scripts providers, the apt
    provider built without `apt.upgradable.list` (neither declared nor
    bound), and build version `v0.9.0-compat-older`. The current CLI,
    which has that op (`nexus packageupdate get`), is the newer CLI.
  - **Newer server:** the real current nexusd behind a proxy that adds a
    field to every response object, list item, item `properties` and
    capabilities provider entry. It also advertises a provider `future`
    and serves its extra op. The current CLI is the older CLI.
  - **Limitation:** the newer server's extra fields are added on the wire,
    not by a real newer build.

## Run 1 numbers

### TC-187: footprint

| Measure | Value |
| --- | --- |
| Startup exec to READY=1, 10 starts | 65.5, 69.6, 71.4, 76.8, 79.1, 71.4, 70.7, 79.0, 67.6, 72.6 ms: p50 71.4, max 79.1 |
| `systemctl start` wall time | p50 77.8 ms, max 85.8 ms |
| RSS right after start, before any request | 18.4 MiB, 8 threads, 10 fds |
| Idle window | 300.008 s, 300 RSS samples |
| Idle RSS | max 19.70 MiB, mean 19.48 MiB, start 19.70, end 19.33, VmHWM 19.96 MiB |
| Idle CPU | cgroup 8.26 ms = 0.0028% of one core; /proc 1 tick = 0.0033% |
| Threads, fds during the window | 10 to 10; 10 to 9 (the HTTP server closed the test's idle keep-alive connection) |
| Release binaries | nexusd-linux-amd64 13,095,074 B (12.49 MiB); nexus-darwin-arm64 9,463,762 B (9.03 MiB); nexus-linux-amd64 9,990,306 B (9.53 MiB) |

### TC-188: idle activity in the same window

| Detector | Warm-up (control) | Idle window |
| --- | --- | --- |
| Engine API requests (debug log) | 56 | 0 |
| nexusd `cminflt` delta | +11,755 | +0 |
| nexusd `cutime+cstime` delta | (not logged) | +0 |
| Other processes in nexusd.service's cgroup | | 0 |
| apt/dpkg processes, any parent | | 0 |
| nexusd journal lines; `nexus-op-*` journal lines | | 0; 0 |

### TC-189: latency with 50 containers, 200 samples each

| Op | p50 | p95 | max | mean |
| --- | --- | --- | --- | --- |
| system.capabilities.get | 0.2 | 0.5 | 1.4 | 0.3 |
| system.operations.list | 0.1 | 0.2 | 0.5 | 0.1 |
| system.operations.get | 0.1 | 0.2 | 0.4 | 0.1 |
| system.operations.logs | 0.2 | 0.4 | 0.6 | 0.2 |
| system.audit.list | 0.1 | 0.2 | 0.3 | 0.1 |
| docker.containers.list | 89.4 | 112.2 | 129.4 | 92.3 |
| docker.containers.get | 17.3 | 21.5 | 29.3 | 17.1 |
| docker.containers.logs | 0.5 | 0.7 | 1.1 | 0.5 |
| apt.packages.list | 2.0 | 2.4 | 2.7 | 2.0 |
| apt.packages.get | 17.1 | 20.9 | 23.7 | 17.7 |
| apt.upgradable.list (cached) | 0.1 | 0.1 | 0.3 | 0.1 |
| scripts.scripts.list | 0.1 | 0.1 | 0.2 | 0.1 |
| scripts.scripts.get | 0.1 | 0.1 | 0.3 | 0.1 |
| **ack** docker.containers.start | 5.9 | 11.1 | 52.2 | 6.7 |
| **ack** scripts.scripts.invoke | 4.0 | 7.3 | 11.5 | 4.4 |
| **ack** apt.actions.refresh | 3.5 | 7.8 | 20.4 | 4.3 |

All values are in ms. The cold `apt.upgradable.list` calls (A-38, not
counted in the p95 above), first call then 10 after invalidation, were
25.2, 24.3, 22.0, 21.7, 21.5, 21.4, 21.5, 21.2, 20.9, 22.2 and 22.0 ms:
p50 21.7, p95 25.2. 600 of 600 acknowledged operations had an audit
record.

### TC-190: during a real in-flight apt upgrade

The upgrade was Running, and dpkg (pid 24828) held the frontend lock,
before and after these samples. It then ended Succeeded once its gate
opened.

| Op | p50 | p95 | max |
| --- | --- | --- | --- |
| docker.containers.list | 91.8 | 115.7 | 128.3 |
| docker.containers.get | 14.5 | 18.3 | 20.4 |
| scripts.scripts.list | 0.1 | 0.2 | 0.9 |
| scripts.scripts.get | 0.1 | 0.2 | 0.6 |
| **ack** scripts.scripts.invoke | 4.5 | 8.8 | 17.2 |

### TC-191: dependency failure matrix

In the table, p95 is in ms over 20 samples.

| Failure | Failing provider's answer | Other providers (p95) | Same PID, no panic |
| --- | --- | --- | --- |
| Docker stopped | 503 `dependency-unavailable` in 0.6 ms; capabilities: docker `unavailable` ("the Docker Engine is not reachable at /run/docker.sock ...") | capabilities 0.2, apt list 2.4, scripts list 0.2 | yes; Docker reads recovered 4,727 ms after the Engine's start returned, no restart of nexusd |
| Registry stopped | check-updates Succeeded, the container `unknown` / `registryError` ("dial tcp 127.0.0.1:5000: connect: connection refused") | capabilities 0.8, Docker list 110.8, apt list 2.4, scripts list 0.2 | yes |
| dpkg lock held | upgrade Failed `package-manager-busy` after 5,019 ms (`lockWait: 5s`); apt list still 200 in 5.3 ms | capabilities 0.8, Docker list 138.1, scripts list 0.2 | yes |
| Failing script | Failed `script-failed` | capabilities 0.7, Docker list 138.4, apt list 2.5, scripts list 0.2 | yes |
| Docker hung after a healthy preflight | 3 concurrent reads: 503 `dependency-unavailable` after 30,004.6, 30,004.7 and 30,004.8 ms (deadline 30 s + 5 s) | capabilities 1.3, apt list 5.1, scripts list 0.2; script ack 7.2 | yes; afterwards nexusd fds 10 (baseline 11), threads 11 (baseline 10), gate connections open 0 (baseline 2); reads recovered 146.5 ms after the socket answered again |
| Docker hung from the start | 503 `capability-unavailable` ("preflight did not finish within 5s") in 0.8 ms | capabilities 0.4, apt list 2.9, scripts list 0.2 | yes; **exec to READY=1 5,070.8 ms** (see findings) |

Goroutines are not observable from outside nexusd, which has no pprof
endpoint. The "back to baseline" check uses what is observable:
- nexusd's open fds (each hung Docker connection is one);
- the gate's count of connections nexusd still holds;
- the thread count, reported only, since Go does not give threads back.

### TC-192: compatibility

| Direction | Command | Result |
| --- | --- | --- |
| Newer CLI, older nexusd | `capability get` | exit 0; apiVersion `v1`, version `v0.9.0-compat-older`; apt lists resource types `[packages]` only |
| | `container get`, `package get` | exit 0 |
| | `packageupdate get` (the new op) | exit 4: "host older lacks this capability: its nexusd does not serve this operation (route-not-found); upgrade nexusd on that host" |
| | `packageupdate get -o jsonl` | exit 4; one JSON error object, slug route-not-found, status 404 |
| | Control: `packageupdate get` on the current nexusd | exit 0 |
| Older CLI, newer server | `capability get` | exit 0; apiVersion `v1`, version `dev`; the unknown provider `future` is listed |
| | `container get` (jsonl and table), `container get NAME`, `package get`, `operation get` | exit 0. jsonl passes the unknown field through; the table does not show it |
| | `container stop NAME` (long-running, polled to its end) | exit 0; `docker inspect` shows the container exited |

## Run 2

Run 2 is the TestNFR tests inside the full `make test-harness` run that
closed Phase 10 (2026-11-10). They ran among the other T1 tests, with the
same environment. The full log is appended to
`planning/nexus-server-manager/context/behavioral-verification-10.3.md`.

Engine state differed from run 1 in one way that matters. Earlier tests
in the full run (TC-113, `TestDockerUpdateBothImageStores`) leave the
nested Engine on the classic image store. The journal shows
`containerd-snapshotter=false storage-driver=overlay2`. Run 1 used Docker
29's default containerd image store (`containerd-snapshotter=true
storage-driver=overlayfs`). The Engine had 52 containers in all, the 50 plus
2 registry containers. The Mac's 1-minute load average ranged from 3.24 to
13.42. All TestNFR tests passed, as did the rest of the suite: 105 passed,
1 skipped by design (`TestHarnessFailurePathIsCollected`), exit 0.

| Measure | Run 2 |
| --- | --- |
| Startup exec to READY=1, 10 starts | p50 70.7 ms, max 75.5 ms (`systemctl start` p50 76.9, max 80.8) |
| Idle RSS, 300 samples over 300.012 s | max 19.62 MiB, mean 18.82, start 19.62, end 17.93, VmHWM 19.85 |
| Idle CPU | cgroup 9.936 ms = 0.0033% of one core; /proc 0 ticks |
| Idle activity | 0 Engine requests (control: 59 in the warm-up), cminflt +0 (control +11,896), 0 children, 0 apt/dpkg processes, 0 journal lines |

TC-189 reads and acknowledgements (ms, 200 samples each):

| Op | p50 | p95 | max |
| --- | --- | --- | --- |
| system.capabilities.get | 0.1 | 0.2 | 1.0 |
| system.operations.list | 0.1 | 0.1 | 0.4 |
| system.operations.get | 0.1 | 0.1 | 0.3 |
| system.operations.logs | 0.1 | 0.3 | 0.6 |
| system.audit.list | 0.1 | 0.2 | 0.6 |
| docker.containers.list | 6.6 | 8.9 | 11.0 |
| docker.containers.get | 0.5 | 0.7 | 1.8 |
| docker.containers.logs | 0.4 | 0.6 | 1.5 |
| apt.packages.list | 1.9 | 2.2 | 2.8 |
| apt.packages.get | 16.9 | 22.1 | 27.7 |
| apt.upgradable.list (cached) | 0.1 | 0.1 | 0.4 |
| scripts.scripts.list | 0.1 | 0.1 | 0.3 |
| scripts.scripts.get | 0.1 | 0.1 | 0.4 |
| **ack** docker.containers.start | 6.2 | 9.3 | 18.3 |
| **ack** scripts.scripts.invoke | 4.0 | 6.9 | 24.4 |
| **ack** apt.actions.refresh | 3.7 | 6.5 | 8.6 |

The cold `apt.upgradable.list` calls were 21.3, 21.7, 21.1, 22.8, 21.6,
21.7, 20.4, 20.0, 20.2, 19.9 and 23.1 ms: p50 21.3, p95 23.1.

The rest of run 2:
- **TC-190,** with the upgrade Running and dpkg (pid 81314) holding the
  lock throughout: Docker list p95 8.4 ms, get 0.8, scripts list and get
  0.1, script start ack p95 8.6 ms.
- **TC-191:** the same answers as run 1.
  - Docker stopped: 503 `dependency-unavailable` in 0.6 ms; recovered
    4,564 ms after the Engine's start returned.
  - Registry stopped: `unknown` / `registryError`.
  - dpkg lock held: `package-manager-busy` after 5,068.6 ms.
  - Failing script: `script-failed`.
  - Docker hung after a healthy preflight: three 503
    `dependency-unavailable` after 30,006.8 to 30,007.6 ms. Afterwards
    nexusd fds 9 (baseline 11), threads 10 (baseline 10), gate
    connections open 0 (baseline 2). Reads recovered 21.0 ms after the
    socket answered.
  - Docker hung from the start: exec to READY=1 5,076.4 ms.
  - Every other provider's p95 stayed at or under 12.2 ms throughout.
- **TC-192:** the same results as run 1.

Run 1 and run 2 agree on everything except the Docker reads, which were
about 13 times faster on the classic image store. See finding 3.

## Findings for a decision

1. **The lane slot is freed after the operation already reads Succeeded.**
   In dry run 2, a script started right after the previous one read
   Succeeded got `409 conflict` with `blockingOperationId` naming that
   already-Succeeded operation. `ops.Manager` commits the terminal status
   in the worker (`commitEnd` from `runDetached`, which calls
   `CommitTerminal`). It releases the lane slot afterwards, in the
   deferred `finish` (`internal/ops/manager.go`, `runDetached` and
   `finish`). A client that polls to Succeeded and immediately starts the
   next run on a reject lane can be refused. The ack tests count these
   409s and retry. Run 1 counted 0. This is reported for the debug
   pipeline and not fixed here.
2. **A Docker socket that hangs at startup delays readiness of the whole
   daemon by 5 s.** exec to READY=1 was 5,070.8 ms, against about 70 ms
   normally, because startup waits for the Docker preflight's 5 s bound.
   NFR-1's 2 s applies to a healthy reference host, so this is not an
   NFR-1 miss. Under NFR-3, it does make one provider's dependency delay
   every provider at startup. A decision is needed on whether that is
   acceptable.
3. **`docker.containers.list` has the thinnest margin, and it depends on
   the Engine's image store.**
   - **Cost:** one list is one `containers/json` plus one inspect per
     container. The Engine's debug log counted 21,100 inspects for 426
     lists in dry run 1, so 51 Engine calls for 50 containers.
   - **Containerd image store** (Docker 29's default, run 1): inspects
     cost about 2 ms each. List p50 is about 90 ms and p95 112 to 138 ms.
   - **Classic overlay2 store** (run 2): list p50 6.6 ms, p95 8.9 ms; get
     p95 0.7 ms against 21.5 ms.
   - **Contended run:** dry run 2 used the containerd store, with
     `go test -race ./...` running on the Mac at the same time and a VM
     load of up to 2.6. There, list p95 rose to 374 ms in TC-189, 316 ms
     in TC-190 and 374 ms in TC-191, all over 200 ms.
   - **Verdict:** this is not the "no other load" condition of NFR-2, and
     the target is unchanged. At 11.1, record the host's image store
     (`docker info`: `containerd-snapshotter`, storage driver) with the
     numbers. The margin matters most if the host uses the containerd
     store.

## Not representative of the x64 host

These are marked for re-measurement at 11.1:

- **All timings and RSS:** T1 is arm64 under the macOS hypervisor, with
  nested Docker on overlay and anonymous volumes. The real host is x64 on
  bare metal.
- **Cold `apt.upgradable.list` (A-38):** the harness's only apt source is
  the small fixture repository, so `apt-get -s` takes about 22 ms here.
  A-38 recorded 1.0 to 1.2 s for `apt-get -s` on a VM with real
  sources. The cached path (0.1 ms) is what NFR-2 counts. The cold numbers
  need the real indexes.
- **`apt.packages.list` and `apt.packages.get`:** these depend on how many
  packages are installed. The harness image has a minimal package set.
- **Acknowledgement latency:** this includes the audit append and the
  bbolt commit, each fsynced. The VM's virtual disk sits on the Mac's SSD.
  The host's disk decides the real fsync cost.
- **`docker.containers.list` and `get`:** these depend on the Engine's
  image store (13 times apart between runs 1 and 2) and on its inspect cost
  on the real host. They also depend on the real containers, which have
  more labels, mounts and ports than busybox `sleep`.
- **Startup to ready:** with real containers, real apt state and the
  host's Docker.

## nexusd self-management (WI 7.2)

The T1 matrix of nexusd self-management, TC-SM-109 to TC-SM-129, in
`test/integration/nexusd_selfmgmt_test.go`. Its NFR lines start with
`NFR NSM-NFR-`. Run it with:

```
make test-harness RUN='^TestTCSM1(09|1[0-9]|2[0-9])_'
```

The figures below are from one such run on 2026-11-13, 21:03 to 21:10 UTC
(harness logs `build/harness/logs/20261113T210341Z`, not committed). The
Mac, the VM and the harness image are those of "Environment" above:
Colima arm64, systemd 259 (259.9-0ubuntu0.1), Ubuntu 26.04.1 LTS.

Every case starts from a wiped host. The v9.9.1 fixture is installed by
its own `install.sh`, and nexusd runs under the shipped unit with no
drop-in. The nexusd provider is enabled with the run's own release key
trusted and the default `healthTimeout` of 30 s, with TLS auto. The CLI is
the test binary, over the pinned certificate. The fixtures are WI 7.1's
real `make nexusd-tree` trees (more than 64 entries each) with harness-arch
binaries.

### Summary

| NFR | Target | T1 | Verdict | Re-measure on the x64 host |
| --- | --- | --- | --- | --- |
| NSM NFR-1 | Update: `restart-started` to `healthy`, p95 of 5 runs under 15 s | 6.21, 6.22, 6.25, 6.25, 6.25 s; p95 (the max) 6.25 s | met | yes |
| NSM NFR-1 | Automatic rollback: `restart-started` to the post-restore `healthy` within healthTimeout + 20 s = 50 s | (b) v9.9.3 7.01 s; (c) v9.9.4 6.02 s; (d) v9.9.5 38.32 s, of which the restore took 7.01 s | met | yes |
| NSM NFR-2 | `get` p95 under 200 ms | p95 0.16 ms (p50 0.10 ms, max 2.87 ms) over 50 calls | met | yes |
| NSM NFR-2 | `validate-config` p95 under 200 ms | p95 0.30 ms (p50 0.13 ms, max 0.30 ms) over 50 calls | met | yes |
| NSM NFR-2 | nexusd.prev executed at most once for repeated reads | 1 exec (`-version`) over 101 reads | met | no |
| NSM NFR-4 | VmRSS during a 64 MiB upload under idle + 8 MiB | idle 18,468 KiB; sampled peak 18,456 KiB; VmHWM 18,560 KiB before and after: no measurable growth | met | yes |
| NSM NFR-4 | `staging/` empty after a refused update and after an expiry sweep | empty both times | met | no |

### Method

- **NFR-1 (TC-SM-126):** the update helper writes `restart-started <ts>`
  and `healthy <ts>` lines to the op log from its own clock. When
  `install.sh` restarts the unit itself (A-33), `restart-started` is the
  start of `install.sh`, so the figure is an upper bound on the outage.
  About 5 s of every figure is the FR-10 stability window: health holds
  only after the same MainPID and NRestarts stay healthy for 5 s. Case (d)
  includes the full 30 s health timeout spent on the wrong version
  (`v9.9.8` reported, `v9.9.5` expected) before the restore.
- **NFR-2 (TC-SM-127):** 50 sequential REST calls of each on loopback,
  over the pinned TLS connection with keep-alive, timed in the client,
  with nexusd.prev present (after an update to v9.9.2). A shell wrapper
  stood in for nexusd.prev and appended each exec to a file under
  `/var/lib/nexus`, the only place the sandboxed nexusd may write. The
  wrapper's own file identity is cached after the first `-version`.
- **NFR-4 (TC-SM-128):** VmRSS of the MainPID is the mean of 20 idle
  samples taken 50 ms apart, then sampled every 50 ms during the upload.
  The 64 MiB upload took 105 ms on loopback, so only 3 samples fell in it.
  VmHWM, the process's resident high-water mark, was read before and after
  and did not move, which bounds the peak better than the samples. Step 2
  refused the same upload (its signature is random) with
  `update-signature-invalid`, and `staging/` was empty. Step 3 staged a
  second upload, set its `meta.json` `expires` in the past, restarted
  nexusd, and `staging/` was empty after the startup sweep.

### T1 matrix

The values read back in the run above. "MainPID a -> b" is
`systemctl show -p MainPID` before and after.

| Case | TC | Outcome | Read back |
| --- | --- | --- | --- |
| (a) update v9.9.2 | TC-SM-109 | pass | exit 0; `Succeeded {previousVersion v9.9.1, newVersion v9.9.2, changed, rolledBack false}`; `nexusd -version` v9.9.2, `nexusd.prev -version` v9.9.1, the saved unit equals the pre-update unit; 1 `Started` line; MainPID 472 -> 633, `/proc/633/exe -version` v9.9.2; op log `backup: ... from /proc/472/exe` (A-20), `installed nexusd v9.9.2`, `install.sh restarted nexusd.service (MainPID 633, was 472)` (A-33); upload `201 Created`; `staging/` empty; the op unit stayed active with MainPID 519 across nexusd 472 -> 633 (A-19) |
| (b) v9.9.3 | TC-SM-110 | pass | exit 1; `Failed update-failed`, `rolledBack true`, `step install`, detail "install.sh failed with exit code 3"; binary v9.9.1; unit file and saved unit equal the shipped unit; active with a new MainPID |
| (c) v9.9.4 | TC-SM-111 | **fail** (one assertion) | exit 1; `update-failed`, `rolledBack true`, `step install` (install.sh's blocking restart failed, exit 1); binary v9.9.1; active, `Result=success`; the journal shows the stub's `'serve' fails on purpose`. The op log has no `reset-failed` line: see finding 1 |
| (d) v9.9.5 | TC-SM-112 | pass | exit 1; `update-failed`, `rolledBack true`, `step health`, "condition 2 ... MainPID 2604 runs nexusd v9.9.8, want v9.9.5"; binary v9.9.1. A-17 directly: with v9.9.2 renamed over the binary, `/proc/2407/exe` (`/usr/local/bin/nexusd (deleted)`) still reported v9.9.1 |
| (e) v9.9.6 | TC-SM-113 | pass | exit 1 `update-signature-invalid`; no op; `staging/` empty; last audit record `refused`, `update-signature-invalid`, `facts.signature invalid`; MainPID unchanged |
| (f) restart | TC-SM-114 | pass | exit 0; `Succeeded {version v9.9.1, changed}`; MainPID 4337 -> 4423; op unit active across the restart (A-19); `operation get` after the restart shows Succeeded |
| (g) rollback twice | TC-SM-115 | pass | `{fromVersion v9.9.2, toVersion v9.9.1}` (MainPID -> 5395), then `{v9.9.1 -> v9.9.2}` (-> 5758); binaries swapped each time; both unit files' hashes back to their originals |
| (h) fresh rollback | TC-SM-116 | pass | exit 1 `rollback-unavailable`, `path /usr/local/bin/nexusd.prev`; no op; MainPID unchanged |
| (i) reload | TC-SM-117 | pass | `bogusKey` refused `config-invalid`, `errors[0]` at `/etc/nexus/nexusd.yaml:<last line + 1>:1`, no op; then `Succeeded {applied, restartRequired [limits]}`; MainPID 6483 throughout |
| (j) concurrent | TC-SM-118 | pass | 27 restart attempts while the update ran: all 409 `conflict` naming the update, 25 of them answered by the new MainPID 6897 (was 6754); no restart op |
| (k) `--follow` | TC-SM-119 | pass | exit 0; "likely restarting" at 21:05:53.6, then the followed `healthy 2026-11-13T21:05:59.80Z` at 21:05:59.9; no line twice. The followed lines are on stderr (FR-33) |
| same version | TC-SM-120 | pass | exit 0 `Succeeded {changed false}`; MainPID 7856 unchanged; binary hash unchanged; no nexusd.prev; `staging/` empty |
| downgrade | TC-SM-121 | pass | without the flag: exit 1 `version-downgrade` (v9.9.2, v9.9.1), upload kept, no op; with it: `Succeeded {v9.9.2 -> v9.9.1}`, binary v9.9.1 |
| flipped byte | TC-SM-122 | pass | byte 4,649,826 of 9,299,652 flipped: exit 1 `update-signature-invalid`; no op; `staging/` empty; binary unchanged |
| (l) v9.9.7 | TC-SM-123 | pass | CLI exit 5 after the 60 s window; before recovery `nexusd.prev -version` v9.9.1 (the stub) and nexusd.service `failed`, `Result=exit-code`, `NRestarts=5`; `exit.json` and `result.json` present; after `lib.sh install-release`, `reset-failed` and `start`: active, MainPID 9634; `Failed update-rollback-failed`, `rolledBack false`, detail with "may be down" and the recovery anchor; never `outcome-unknown` |
| prev rejects config | TC-SM-124 | pass | exit 1 `config-invalid` with the stub's two stderr lines in order; no op; binaries unchanged (A-31: prev executed under the sandbox) |
| reads | TC-SM-125 | pass | `get` v9.9.1, no previous, the config and executable paths, self-management available; after the update v9.9.2 with previous v9.9.1 and uptime reset; `validate-config` valid; a `nexusd:read` token gets exit 3 naming `nexusd:admin` for update and restart |
| NFR-1, -2, -4 | TC-SM-126 to 128 | pass | above |
| fidelity | TC-SM-129 | **fail** (by design) | real systemd differs from F-SYS: see finding 2 |

### Findings for a decision

1. **The update helper does not log `reset-failed` (TC-SM-111).**
   `update_helper.go` `restore` runs `systemctl reset-failed` without a
   step line, so the op log cannot show it. TC-SM-111 asserts one and
   fails on that alone. In the real case (c), the restore ran at restart
   counter 1, before the start limit, so the rollback did not depend on
   reset-failed this time.
2. **F-SYS does not match systemd 259 at the start limit (TC-SM-129).**
   The captures are in `internal/providers/nexusd/testdata/systemctl-show`.
   - A binary that exits at start ends `failed` with `Result=exit-code`
     and `NRestarts=5`. F-SYS encodes `Result=start-limit-hit` and
     `NRestarts=4`. The journal logs "Start request repeated too quickly"
     and then "Failed with result 'exit-code'". systemd 259 never showed
     `start-limit-hit` in this path, even after a further restart was
     refused.
   - A refused blocking `systemctl restart` exits 1 with "the control
     process exited with error code". A refused `systemctl restart
     --no-block` exits 0, and the start then fails on its own. F-SYS's
     `Restart` returns an error instead.
   - The limit lifts once StartLimitIntervalSec has passed since the
     window began, without reset-failed. F-SYS keeps it until
     `ResetFailed`.
   - `systemctl reset-failed` on a stopped, unloaded unit exits 1 ("not
     loaded"). F-SYS never fails it.
   - Confirmed as encoded: `restart --no-block` returns in about 3 ms and
     the new MainPID comes later; a refused start logs "Start request
     repeated too quickly"; reset-failed clears the limit and the next
     restart starts; `install.sh` starts nothing when the unit is
     inactive (0 `Started` lines).

   TC-SM-63 steps 3 and 4, TC-SM-ERR52 (its setup "F-SYS reaches
   `start-limit-hit`" and its planted variant's detail), and
   `TestRestartRefusedBySystemd` encode the F-SYS behaviour, and so does
   TC-SM-129's own step 2 expectation in `test-cases.md`. Correcting F-SYS
   changes those Expected values, which needs a test-case amendment. The
   product helper worked on real systemd in every case.
3. **An update to the running version reports empty versions.** TC-SM-120's
   result was `{"changed":false,"previousVersion":"","newVersion":""}`,
   the gap recorded at Phase 5.
4. **TC-SM-119 says stdout, but `--follow` writes the followed lines to
   stderr (FR-33).** The test checks the followed `healthy` line wherever
   it is, and logs which stream it was on.
5. **The 50 ms RSS sampling sees little of a 64 MiB loopback upload,**
   which takes about 100 ms. The VmHWM read-back covers it. On the real
   host, over the network, the upload is slower and the samples will
   count.
