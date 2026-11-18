# services-provider WI 5.3: T1 runs on systemd and OpenRC

The real `nexusd` binary, installed under the shipped hardened unit (systemd) or the shipped OpenRC script, driven over HTTP in privileged harness containers. No mocks: expected values come from `systemctl`, `journalctl`, `rc-status`, `rc-update`, `rc-service`, `/proc` and the audit file.

Command, one image at a time:

```
PARITY=0 make test-harness            [BASE=debian:stable | BASE=debian:12] RUN='^TestTCSV1(3[5-9]|4[0-7])_'
PARITY=0 make test-harness-alpine     [BASE=alpine:3.22]                    RUN='^TestTCSV1(3[5-9]|4[0-7])_'
```

## Results (final matrix, after the two fixes below)

| Image | Init | Stamp | Result | Logs |
|---|---|---|---|---|
| ubuntu:latest (26.04) | systemd 259 (259.9-0ubuntu0.1) | 20261118T190836Z | exit 0; 12 pass, TC-SV-136 skipped (OpenRC only) | `services-t1/ubuntu/` |
| debian:stable (13) | systemd 257 (257.13-1~deb13u1) | 20261118T191112Z | exit 0; 12 pass, TC-SV-136 skipped | `services-t1/debian-stable/` |
| debian:12 | systemd 252 (252.39-1~deb12u2) | 20261118T191222Z | exit 0; 12 pass, TC-SV-136 skipped | `services-t1/debian12/` |
| alpine:latest (3.24) | OpenRC 0.63.2 | 20261118T190949Z | exit 0; 11 pass, TC-SV-135/146 skipped (systemd only) | `services-t1/alpine/` |
| alpine:3.22 | OpenRC 0.62.6 | 20261118T191330Z | exit 0; 11 pass, TC-SV-135/146 skipped | `services-t1/alpine322/` |

debian:12 and alpine:3.22 match pihole (Debian 12 LXC) and Public-Nginx (Alpine 3.22 LXC). These are container results, not live-host results; the live gates are WI 6.0–6.3.

## Product bugs found by the first matrix and fixed

The first run (test commit 6c82288, on 9010b2f) passed on ubuntu and debian:stable and failed on the other three:

1. **systemd 252: the boot-ID bind mount is hidden from nexusd** (TC-SV-142..145 failed on debian:12, stamp 20261118T185039Z). Under `ProtectKernelTunables=yes`, systemd 252 gives nexusd (and op units carrying the same property) a read-only `/proc/sys` that drops the container's boot-ID mount entirely: nexusd's table held only `413 372 0:51 /sys /proc/sys ro,…` and it read the host kernel's ID `a2e5b43d-…` while the container's was `8f2fa550-…`. The reboot was unavailable; had it run, the helper and the ops manager would have compared different IDs. systemd 257 and 259 keep the mount (read-only). Fix: `detach.ReadBoot` reads PID 1's mounted boot ID through a same-device mount in nexusd's own table, failing closed (regression tests 635cba2, fix ee262c6). After the fix: `bootIdSource container-mount`, `rebootAvailable true`, what-if boot ID equals the mounted container ID.
2. **OpenRC: `mainPid` was the supervise-daemon PID** (TC-SV-138 failed on both Alpine images): `mainPid 2125 (exe /sbin/supervise-daemon)` vs the service's `2126 (exe /bin/busybox, PPid 2125)`. Fix: a service with `/run/openrc/options/<name>/child_pid` reports that child, never the supervisor (regression tests 112ae2f, fix 5abcfa9).

## Deviations from the test-case text

- **TC-SV-138 journal evidence.** The helper writes to the operation log, not the journal, so the evidence is systemd's own `UNIT=nexus-op-<id>.service` start/stop records (logged under `_SYSTEMD_UNIT=init.scope`) plus no helper line in `nexusd.service`'s journal.
- **TC-SV-140 on OpenRC.** The protected `net` provider is taken from OpenRC's deptree. In Docker, nexusd's `need net` resolves to nothing (`networking` carries `keyword -docker`), so `networking` stands in; real `net` resolution is checked live on Public-Nginx (TC-SV-L3/L5).
- **Token scopes.** `host:read` (the host provider's read scope since 7a8db68) replaces the stale `system:read`.
- **TC-SV-141** plants its own OpenRC `nx-svc-other`, because the shared `plantServices` helper has none for OpenRC.
- **TC-SV-145** (cancel in the delay window) ended `Canceled` in about 0.5 s on every image; no `reboot-requested` file appeared and PID 1 never changed.

Paths in `run.log` were rewritten to `<repo>` and `~/` before committing.
