# nexusd remote admin: T1 results and the live gate (TC-RA-108, TC-RA-109)

Work item 5.4 of nexusd-remote-admin. This page has the T1 harness results
for remote token management and remote config set, on systemd and on
OpenRC side by side, and the checklist for the live gate on the three real
hosts, with an empty observed slot for every step.

**Status: `pending-live-gate`.** No real-host transcript exists yet. The
session that wrote this page had no access to the hosts (no SSH, no admin
token), and the live gate runs only after the user's explicit go-ahead
(CON-6). Everything under [T1 results](#t1-results) comes from container
runs of the T1 harness on an arm64 Mac. It is **not real-host evidence**.
The feature is not complete until both live read-backs pass:

| Item | TC | Host | Status |
| --- | --- | --- | --- |
| Remote config set enables the host provider, read back three ways and the `.bak` checked | TC-RA-108 | `home` (192.168.7.5) or `pihole` (192.168.7.4), systemd | **PENDING** (T1 systemd passes: TC-RA-94) |
| Remote token add, get (`self`), remove and 401; config set refused with `config-set-unavailable` | TC-RA-109 | `nginx` (192.168.7.7), OpenRC | **PENDING** (T1 OpenRC passes: TC-RA-104, TC-RA-ERR37) |

When every observed slot below is filled in and every expected outcome
holds, change the status line to `passed`. `go test ./test/buildsys/`
refuses `passed` while any slot still reads `______`.

- [T1 results](#t1-results)
- [Live gate checklist](#live-gate-checklist)
- [Sign-off](#sign-off)

## T1 results

### The runs

Each run's full `run.log` is committed under
[nexusd-remote-admin/](nexusd-remote-admin/), with every token secret
replaced by `<REDACTED>` (the secrets belonged to throw-away containers, and
the redaction keeps the token ID). Line numbers below are lines of those
copies.

| Run | Command | Base | Result | Log |
| --- | --- | --- | --- | --- |
| `20261117T120059Z` | `make test-harness RUN='^TestTCRA'` | Ubuntu 26.04.1, systemd 259 (259.9-0ubuntu0.1) | 20 PASS, 6 SKIP, 0 FAIL in `test.log`; parity check **FAIL**; `harness: exit 1` | [systemd, first run](nexusd-remote-admin/20261117T120059Z-systemd-run.log) |
| `20261117T121435Z` | `make test-harness-alpine` (every suite) | Alpine Linux v3.24, OpenRC 0.63.2 | 99 top-level PASS, 107 SKIP, 0 FAIL in `test.log`; parity PASS; svcctl PASS; `harness: exit 0` | [OpenRC](nexusd-remote-admin/20261117T121435Z-alpine-openrc-run.log) |
| `20261117T123827Z` | `make test-harness RUN='^TestTCRA'` (re-run) | Ubuntu 26.04.1, systemd 259 (259.9-0ubuntu0.1) | 20 PASS, 6 SKIP, 0 FAIL in `test.log`; parity PASS; svcctl PASS; `harness: exit 0` | [systemd, re-run](nexusd-remote-admin/20261117T123827Z-systemd-run.log) |
| `20261118T044518Z` | `make test-harness RUN='^TestTCRA'` (after the SEC-2 key swap and the CR-18 wording; covers TC-RA-98, 99, 102, ERR32, ERR33, E33) | Ubuntu 26.04.1, systemd 259 (259.9-0ubuntu0.1) | 21 PASS, 6 SKIP, 0 FAIL in `test.log`; parity PASS; svcctl PASS; `harness: exit 0` | [systemd, after SEC-2](nexusd-remote-admin/20261118T044518Z-systemd-run.log) |

Counting the harness's own checks (`TestParitySuitesRanAsRoot` and the
svcctl `TestLiveMatrix`) with the suites, `run.log` holds 22 `--- PASS`
lines for the systemd re-run and 101 for the OpenRC run.

**Why the first systemd run exits 1.** It ran on the WI 5.2 tree, before WI
5.3 replaced the OpenRC stubs. The six OpenRC cases were still `SKIP-only`
stubs that skipped on every platform, and the harness's parity check
refuses a case that skips on its own platform
(20261117T120059Z-systemd-run.log:1275-1294):

```
    platform_test.go:298: test.log: TestTCRA104_OnOpenRCTokenAddGetAndRemoveRoundTripAndApplyOn skipped on its own platform: remoteadmin_openrc_test.go:34: TC-RA-104: SKIP-only stub; un-skip when WI 5.3 lands
...
--- FAIL: TestParitySuitesRanAsRoot (0.00s)
FAIL
harness: parity check exited 1, parity suites 0
```

Every systemd case passed in that run too. The re-run on the WI 5.3 tree
gates the six cases to OpenRC with `requireInit`, and the parity check
passes (20261117T123827Z-systemd-run.log:1286-1288):

```
    platform_test.go:301: test.log: 26 results; 6 gated to openrc or to apt: TestTCRA104_OnOpenRCTokenAddGetAndRemoveRoundTripAndApplyOn, TestTCRA105_OnOpenRCConfigGetWorks, TestTCRA106_OnOpenRCNexusNexusdGetReportsConfigSet, TestTCRA107_OnOpenRCTokenSecretsReachNoSink, TestTCRAERR37_OnOpenRCConfigSetIsRefusedWithConfigSet, TestTCRAERR38_OnOpenRCThe429PathAndTheTokenCapBehaveAsOn
--- PASS: TestParitySuitesRanAsRoot (0.00s)
harness: parity check exited 0, parity suites 0
```

### Side by side

Results from the systemd re-run (`20261117T123827Z`) and the OpenRC run
(`20261117T121435Z`). A case gated to the other init system skips there
through `requireInit`, with the reason in `test.log`.

| Case | What it shows | systemd | OpenRC |
| --- | --- | --- | --- |
| TC-RA-93 | The installed `nexusd.service` is byte-identical to `deploy/nexusd.service` | PASS | skip (systemd only) |
| TC-RA-94 | Config set applies `providers.host.enabled=true`; `hostsummary get` then answers | PASS | skip |
| TC-RA-95, 96 | Comments survive a set; adding an absent key | PASS | skip |
| TC-RA-97 | A set that changes nothing touches nothing | PASS | skip |
| TC-RA-98 | A restart-only key is written and reported, never applied | PASS | skip |
| TC-RA-99 | The nexusd lane refuses a reload while a config set runs | PASS | skip |
| TC-RA-100 | Token secrets reach no sink (state DB, audit log, journal) | PASS | skip |
| TC-RA-101 | A remote add and a local `sudo nexusd token create` of one name race | PASS | skip |
| TC-RA-102 | NFR-2 latencies | PASS | skip |
| TC-RA-103 | No sandbox denial, crash or restart during the run | PASS | skip |
| TC-RA-E26, ERR34, ERR36, ERR39 | Unconfirmed or refused reloads are restored; a restore that cannot be confirmed ends `config-restore-failed` | PASS | skip |
| TC-RA-E28 | The hold drop-in applies to `nexus-op-*` units and never to `nexusd.service` (A-34) | PASS | skip |
| TC-RA-ERR32, ERR33 | Validation and deny-list refusals start no unit | PASS | skip |
| TC-RA-ERR35, ERR44 | A file changed under a set ends `config-conflict` | PASS | skip |
| TC-RA-104 | Token add, get (`self`) and remove round trip | skip (OpenRC only) | PASS |
| TC-RA-105 | Config get works | skip | PASS |
| TC-RA-106 | `nexus nexusd get` reports `configSet` unavailable | skip | PASS |
| TC-RA-107 | Token secrets reach no sink | skip | PASS |
| TC-RA-ERR37 | Config set is refused with `config-set-unavailable` | skip | PASS |
| TC-RA-ERR38 | The 429 path and the token cap behave as on systemd | skip | PASS |

### NFR-2 on systemd

`TestTCRA102_NFR2LatenciesOnTheRealHost`, both systemd runs, verbatim
(lines 774-842 of each log; 50 samples per p95):

| Measurement | Bound | `20261117T120059Z` | `20261117T123827Z` |
| --- | --- | --- | --- |
| token list p95 | 200 ms | 4 ms | 3 ms |
| config get p95 | 200 ms | 4 ms | 4 ms |
| token add p95 | 200 ms | 10 ms | 9 ms |
| token remove p95 | 200 ms | 7 ms | 6 ms |
| config set with a passing reload, 202 to ended | 15 s + the reload's own time | 534 ms (bound 15.02718628s) | 532 ms (bound 15.041526247s) |
| config set refused at load and restored, tOpen to ended | 30 s | 1.132 s | 1.157 s |
| config set unconfirmed and restored, tOpen to ended | 30 s | 12.64 s | 12.647 s |
| the same, TC-RA-E26 | 30 s | 12.651 s | 12.639 s |
| restore not confirmed (TC-RA-ERR36), tOpen to ended | 30 s | 24.68 s | 24.666 s |
| refused at load and restored (TC-RA-ERR39) | 30 s | 1.134 s | 1.138 s |

From the re-run (20261117T123827Z-systemd-run.log:774-842):

```
    nexusd_remoteadmin_test.go:1659: NFR NFR-2 token list p95 (50): 3ms (bound 200ms)
    nexusd_remoteadmin_test.go:1659: NFR NFR-2 config get p95 (50): 4ms (bound 200ms)
    nexusd_remoteadmin_test.go:1659: NFR NFR-2 token add p95 (50): 9ms (bound 200ms)
    nexusd_remoteadmin_test.go:1659: NFR NFR-2 token remove p95 (50): 6ms (bound 200ms)
    nexusd_remoteadmin_test.go:1684: NFR NFR-2 config set reload's own time: 42ms (journal Reloading 2026-11-17T12:41:19.07204Z to reload-status.json at 2026-11-17T12:41:19.113566247Z)
    nexusd_remoteadmin_test.go:1686: NFR NFR-2 config set with a passing reload, 202 to ended: 532ms (bound 15.041526247s)
    nexusd_remoteadmin_test.go:1691: NFR NFR-2 config set refused at load and restored, tOpen to ended: 1.157s (bound 30s)
    nexusd_remoteadmin_test.go:1694: NFR NFR-2 config set unconfirmed and restored, tOpen to ended: 12.647s (bound 30s)
```

The 30 s restore bound is met with room to spare: the slowest restore, an
unconfirmed reload, is the 12 s confirmation deadline (A-3) plus about
0.65 s. These are arm64 container figures; the real hosts are amd64.

### The hold drop-in on systemd 259 (A-34)

The held cases depend on a prefix drop-in,
`/run/systemd/system/nexus-op-.service.d/ra-hold.conf`, reaching the
transient units `systemd-run --unit=nexus-op-<id>` creates, and never
`nexusd.service`. TC-RA-E28 checks both on the harness's systemd
(`harness: versions: systemd 259 (259.9-0ubuntu0.1)`, line 21), from the
re-run (20261117T123827Z-systemd-run.log:1160-1195, abridged):

```
=== RUN   TestTCRAE28_TheHoldDropInAppliesToNexusOpUnitsAndNever
    nexusd_remoteadmin_test.go:1974: hold armed: /run/systemd/system/nexus-op-.service.d/ra-hold.conf, nexusd.service still exact with no drop-in
    nexusd_remoteadmin_test.go:1979: gate: 01a1e987-a2a7-761f-bd11-4172251fc3c5 reached; nexus-op-01a1e987-a2a7-761f-bd11-4172251fc3c5.service DropInPaths=/run/systemd/system/nexus-op-.service.d/ra-hold.conf ActiveState=activating SubState=start-pre
    nexusd_remoteadmin_test.go:1988: gate open at 2026-11-17T12:42:22.797576104Z
    nexusd_remoteadmin_test.go:1991: PC: sha256(disk) 91b30c763b9bff73a3bc32b1e34f203935c498a6a9c72630c5c53e5a04993dd9 == reload-status.json sha256, applied, seq 1
    nexusd_remoteadmin_test.go:531: hold released: /run/systemd/system/nexus-op-.service.d/ra-hold.conf removed, nexusd.service exact with no drop-in
--- PASS: TestTCRAE28_TheHoldDropInAppliesToNexusOpUnitsAndNever (1.49s)
```

Every other held case logs the same `DropInPaths=` line before it acts, so
none of them fell back to timing.

### The unit is unchanged (TC-RA-93)

Both systemd runs, line 61:

```
    nexusd_remoteadmin_test.go:891: sha256 /etc/systemd/system/nexusd.service 5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a; /harness/deploy/nexusd.service (the repository's deploy/nexusd.service) 5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a
```

Config set adds no directive to `nexusd.service`; it runs in its own
`nexus-op-<id>` unit.

### OpenRC (TC-RA-104 to TC-RA-107, TC-RA-ERR37, TC-RA-ERR38)

From 20261117T121435Z-alpine-openrc-run.log. The token secret printed by the
add is redacted in the copy.

TC-RA-104 (lines 1795-1872): `nexus nexusdtoken add oc --scopes nexusd:read
--yes` exits 0 with `"changed": true`; `nexus nexusdtoken get` lists the
CLI's token `ra` with `"self": true` (line 1835) and `oc` with `"self":
false` and `"createdBy": {"name": "ra", ...}`; `nexus nexusdtoken remove oc
--yes` exits 0 with `"changed": true, "revokedSelf": false`.

TC-RA-105 (line 1929): `nexus nexusdconfig get` lists every key:

```
    remoteadmin_openrc_test.go:284: step 1: 42 rows for the 42 leaf keys of the installed file
```

TC-RA-106: `nexus nexusd get` shows `"configSet": {"available": false,
"reason": "the host was not booted with systemd (/run/systemd/system is
missing)"}`.

TC-RA-107 (line 1990):

```
    remoteadmin_openrc_test.go:415: swept 5 sinks for the token, its secret and its hash: 0 occurrences expected
```

TC-RA-ERR37 (lines 2036-2037): `nexus nexusdconfig update --yes
providers.host.enabled=true` exits 4 with HTTP 503 and type
`urn:nexus:problem:config-set-unavailable`, and the file is untouched:

```
        stderr: {"detail":"nexusd config set is unavailable: the host was not booted with systemd (/run/systemd/system is missing)","exit":4,"host":"t1","level":"error","message":"host t1: config-set-unavailable: nexusd config set is unavailable: the host was not booted with systemd (/run/systemd/system is missing)", ...
    remoteadmin_openrc_test.go:478: after the refusals: /etc/nexus/nexusd.yaml unchanged (sha256 80c389be70b3d3dbfc5af9554f85de400c2b9583ee1e40055434e28b5cf8a89d, inode 3673693, mtime 2026-11-17T12:36:31.364606982Z), .bak present false, reload status seq 0
```

TC-RA-ERR38 (line 2050):

```
    remoteadmin_openrc_test.go:579: 10 adds then a 429 in 36ms, Retry-After 60
```

### What T1 does not show

- **Real hardware and amd64.** T1 runs linux/arm64 binaries in containers.
  The hosts run the linux/amd64 release.
- **A real OpenRC boot.** The Alpine image runs OpenRC inside Docker
  (`openrc (OpenRC [DOCKER]) 0.63.2`). `nginx` boots it for real.
- **An upgrade in place.** Every T1 case starts from a fresh `install.sh`
  install. The hosts carry their own `/etc/nexus/nexusd.yaml` and tokens
  from earlier releases.
- **A restore from a client alone.** Restoring needs a reload to refuse a
  file that passed validation, which the T1 cases cause on the host itself
  (a mode change or a stopped daemon inside a held window). A client cannot
  cause it: an invalid value is refused before anything is written. The live
  checklist therefore shows the refusal path, and the restore path rests on
  TC-RA-E26, ERR34, ERR36 and ERR39 above.

## Live gate checklist

Run this only after the user's explicit go-ahead (CON-6). Record each step's
UTC time (`date -u +%FT%TZ`), exit status and output verbatim in its
observed slot. Redact a token secret to its ID (`nxs_<id>_<REDACTED>`)
before pasting: never paste a secret here.

| Host | Address | Init | Checks |
| --- | --- | --- | --- |
| `home` | 192.168.7.5 | Ubuntu, systemd | Bootstrap, TC-RA-108, cleanup |
| `pihole` | 192.168.7.4 | Ubuntu, systemd | Bootstrap, TC-RA-108, cleanup |
| `nginx` | 192.168.7.7 | Alpine, OpenRC | Bootstrap, TC-RA-109, cleanup |

TC-RA-108 needs one of `home` and `pihole`; running it on both is better,
and the steps are the same with the host name changed.

### Before you start

- A signed release built from this branch, `nexusd-<version>-linux-amd64.tar.gz`,
  newer than the version each host runs.
- **Client host names.** The steps use new client host entries, `<h>-boot`
  and `<h>-ra` (for example `home-boot`, `home-ra`). Do not reuse the
  existing `home`, `pihole` or `nginx` entries: `--save-as-host` with an
  existing name at the same URL replaces that entry's token, and the
  existing `nginx` entry's token has no `nexusd:admin`. Where TC-RA-109 says
  `--host nginx`, this checklist uses `--host nginx-ra`.
- `nexus host get <h>` shows each host's URL and pinned fingerprint; the
  steps reuse them as `<url>` and `<fingerprint>`.
- `providers.nexusd.enabled: true` in each host's `/etc/nexus/nexusd.yaml`
  (the earlier rollout enabled it). If not, enable it, run `nexusd
  validate-config` and reload before step B1.

### Bootstrap (every host, one login)

#### B1. Mint the client's admin token on the host

On `home` and `pihole`:

```
sudo nexusd token create --name mac-cli --scope nexusd:read --scope nexusd:admin --scope host:read --expires 1d
```

On `nginx` (Alpine has no `sudo` by default; use a root shell from `su -`):

```
nexusd token create --name mac-cli --scope nexusd:read --scope nexusd:admin --expires 1d
```

`host:read` lets the same token read `nexus hostsummary get` in TC-RA-108.

**Expected:** exit 0; the token string `nxs_<12 hex>_<secret>` printed once,
with its name, ID, scopes and expiry. Copy the secret straight into step B2.

**Observed** (per host; UTC time, exit status, output with the secret redacted):

```
home:   ______
pihole: ______
nginx:  ______
```

#### B2. Register the host on the Mac

```
nexus host add <h>-boot --url <url> --fingerprint <fingerprint> --token-stdin
```

with the secret from B1 on standard input. Then check the token works:

```
nexus nexusdtoken get --host <h>-boot -o table
```

**Expected:** `host add` exits 0. `nexusdtoken get` exits 0 and lists every
token of the host, local ones included, with `mac-cli` `self` true and every
other token `self` false. No secret or hash appears.

**Observed:**

```
home-boot:   ______
pihole-boot: ______
nginx-boot:  ______
```

#### B3. Deploy the build under test

On `home` and `pihole`, from the Mac, through the existing self-update path:

```
nexus nexusd update --host <h>-boot --file nexusd-<version>-linux-amd64.tar.gz --yes
nexus nexusd get --host <h>-boot -o json
```

On `nginx`, which cannot update itself (OpenRC), install the same version
with the tarball's `install.sh` on the host, as for earlier releases, then
`rc-service nexusd restart`, and from the Mac:

```
nexus nexusd get --host nginx-boot -o json
```

**Expected:** the update operation ends `Succeeded`. `nexus nexusd get` shows
`"version": "<version>"`, and `configSet` is `{"available": true}` on `home`
and `pihole` and `{"available": false, "reason": "the host was not booted
with systemd (/run/systemd/system is missing)"}` on `nginx`.

**Observed:**

```
home:   ______
pihole: ______
nginx:  ______
```

#### B4. Add a token remotely and save it as a client host

```
nexus nexusdtoken add live-admin --host <h>-boot --scopes nexusd:read --scopes nexusd:admin --scopes host:read --expires 1d --save-as-host <h>-ra --yes -o json
```

(On `nginx`, `--scopes host:read` may be left out.)

**Expected:** exit 0. The result has `"changed": true`, the new `tokenId`,
the scopes and the expiry, and **no `token` field**: the secret went only
into `~/.config/nexus/config.yaml` as host `<h>-ra`, with the same URL and
fingerprint as `<h>-boot`. `nexus host get <h>-ra` shows that entry.

**Observed:**

```
home:   ______
pihole: ______
nginx:  ______
```

#### B5. Read the configuration remotely

```
nexus nexusdconfig get --host <h>-ra -o table
```

**Expected:** exit 0 on all three hosts (config get works on OpenRC too).
One row per key: `HOST  NAME  VALUE  EFFECTIVE  SOURCE`, `source` `file` or
`default`, and secrets shown as `[REDACTED]`.

**Observed:**

```
home:   ______
pihole: ______
nginx:  ______
```

### TC-RA-108: config set on home or pihole

Run on `home`, `pihole` or both; `<h>` is the host.

#### L1. Read the key before the change

```
nexus nexusdconfig get providers.host.enabled --host <h>-ra -o table
```

On the host, read only:

```
sudo sha256sum /etc/nexus/nexusd.yaml
sudo ls -l /etc/nexus/nexusd.yaml.bak
```

**Expected:** the row `providers.host.enabled` with `VALUE` `false` or
empty (absent from the file). If it is already `true`, the update in L2
answers `changed: false` and proves nothing: first run
`nexus nexusdconfig update providers.host.enabled=false --host <h>-ra --yes`
and record it here. The `.bak` may or may not exist yet.

**Observed:**

```
______
```

#### L2. Set the key remotely

```
nexus nexusdconfig update providers.host.enabled=true --host <h>-ra --yes -o json
```

**Expected:** exit 0 within a few seconds. One operation
`nexusd.actions.update-config` with `"status": "Succeeded"`,
`"error": null` and a result of this shape:

```
"result": {
  "backupPath": "/etc/nexus/nexusd.yaml.bak",
  "changed": true,
  "changes": [{"key": "providers.host.enabled", "new": true, "old": false}],
  "restartRequired": [],
  "restored": false
}
```

(`"old": null` when the key was absent.) No `run nexus nexusd restart`
line on stderr: `providers.host` applies on reload.

**Observed:**

```
______
```

#### L3. The host provider answers

```
nexus hostsummary get --host <h>-ra -o json
```

**Expected:** exit 0, with the real host's data:
`"id": "/providers/host/summary/current"` and properties `cpu`
(`logicalCpus`, `load`), `memory` and `filesystems` with the host's own
mount points. `cpu.usagePercent` may be null with `"warming up"` in the
first seconds.

**Observed:**

```
______
```

#### L4. Read the key back

```
nexus nexusdconfig get providers.host.enabled --host <h>-ra -o table
```

**Expected:**

```
HOST    NAME                    VALUE  EFFECTIVE  SOURCE
<h>-ra  providers.host.enabled  true   true       file
```

**Observed:**

```
______
```

#### L5. The backup on the host

On the host, read only:

```
sudo ls -l /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml
sudo diff /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml
```

**Expected:** `/etc/nexus/nexusd.yaml.bak` exists, `-rw-------`, owner and
group `root`, modified at L2's time. The diff shows only the
`providers.host.enabled` line (or, when the key was added, the added key
plus the documented normalization: blank lines dropped, comment spacing
collapsed).

**Observed:**

```
______
```

#### L6. The same set again changes nothing

```
nexus nexusdconfig update providers.host.enabled=true --host <h>-ra --yes -o json
```

**Expected:** exit 0 at once with `"changed": false` and no operation; the
`.bak`'s modification time from L5 is unchanged (`sudo ls -l
/etc/nexus/nexusd.yaml.bak`).

**Observed:**

```
______
```

#### L7. A deliberately failing update is refused and changes nothing

```
nexus nexusdconfig update providers.host.enabled=yes --host <h>-ra --yes -o json
sudo sha256sum /etc/nexus/nexusd.yaml        # on the host
```

**Expected:** exit 1. Standard error holds a `config-invalid` problem,
HTTP 422, `"detail": "the change to /etc/nexus/nexusd.yaml has 1 error(s);
nothing was changed"`, with one error at path `providers.host.enabled`:
`invalid value: expected a boolean, true or false`. No operation is
created; the file's sha256 equals the one after L2, and the `.bak` is
unchanged. This is the refusal path. A restore (`config-apply-failed` with
`"restored": true`) needs a reload to refuse a file that passed validation,
which a client cannot cause; T1 covers it (see
[What T1 does not show](#what-t1-does-not-show)).

**Observed:**

```
______
```

### TC-RA-109: nginx round-trips tokens and refuses config set

#### N1. Add a token remotely

```
nexus nexusdtoken add live-rt --host nginx-ra --scopes nexusd:read --expires 1h --save-as-host nginx-rt --yes -o json
nexus nexusd get --host nginx-rt -o json
```

`--save-as-host` keeps the secret off the terminal and lets N3 show the
removed token's 401.

**Expected:** the add exits 0 with `"changed": true`, a `tokenId`, the
scope `nexusd:read`, and no `token` field. `nexus nexusd get --host
nginx-rt` exits 0: the new token authenticates.

**Observed:**

```
______
```

#### N2. List the tokens

```
nexus nexusdtoken get --host nginx-ra -o json
```

**Expected:** exit 0. `live-admin` (the CLI's token) has `"self": true`;
`live-rt` is listed with `"self": false`, `"createdBy": {"name":
"live-admin", ...}` and its `tokenId` from N1; `mac-cli` and any older local
tokens are listed with `"self": false`. No secret or hash appears.

**Observed:**

```
______
```

#### N3. Remove it, then the removed token is refused

```
nexus nexusdtoken remove live-rt --host nginx-ra --yes -o json
nexus nexusd get --host nginx-rt -o json
```

**Expected:** the remove exits 0 with `"changed": true`, `"name":
"live-rt"` and `"revokedSelf": false`. The `nexusd get` through the removed
token then fails with HTTP 401 (`unauthorized`, CLI exit 3). Then
`nexus host remove nginx-rt`.

**Observed:**

```
______
```

#### N4. Config set is refused on OpenRC

```
sha256sum /etc/nexus/nexusd.yaml; ls -l /etc/nexus/nexusd.yaml.bak   # on the host, root shell, before
nexus nexusdconfig update providers.host.enabled=true --host nginx-ra --yes -o json
nexus nexusdconfig get providers.host.enabled --host nginx-ra -o table
sha256sum /etc/nexus/nexusd.yaml; ls -l /etc/nexus/nexusd.yaml.bak   # on the host, after
```

**Expected:** the update exits 4. Standard error holds the problem
`urn:nexus:problem:config-set-unavailable`, HTTP 503: `nexusd config set is
unavailable: the host was not booted with systemd (/run/systemd/system is
missing)`. The config get shows the same value as before the update. The
file's sha256 is the same before and after, and no `.bak` was created or
changed.

**Observed:**

```
______
```

### Cleanup (every host)

#### C1. Revoke the test tokens

Through the bootstrap entry, remove the saved admin token, then the
bootstrap token itself:

```
nexus nexusdtoken remove live-admin --host <h>-boot --yes -o json
nexus nexusdtoken remove mac-cli --host <h>-boot --yes -o json
nexus nexusdtoken get --host <h>-ra
nexus host remove <h>-ra
nexus host remove <h>-boot
```

**Expected:** the first remove exits 0 with `"revokedSelf": false`. The
second exits 0 with `"revokedSelf": true`, and the CLI warns on stderr that
this client can no longer authenticate to that host with it. The
`nexusdtoken get --host <h>-ra` that follows fails with 401 (exit 3). The
`host remove` commands exit 0. If a later step needs admin access again,
the way back is one login: `sudo nexusd token create` on the host.

**Observed:**

```
home:   ______
pihole: ______
nginx:  ______
```

#### C2. Leave the host provider as you want it

`providers.host.enabled` stays `true` on the host(s) used for TC-RA-108.
To turn it off again, before C1:
`nexus nexusdconfig update providers.host.enabled=false --host <h>-ra --yes`.

**Expected:** either left enabled on purpose, or the update ends
`Succeeded` with `"changed": true` and `nexus nexusdconfig get
providers.host.enabled` shows `false`.

**Observed:**

```
______
```

## Sign-off

| Check | Result |
| --- | --- |
| TC-RA-108 on `home`: L2 Succeeded, L3 exit 0, L4 `file true`, L5 `.bak` root 0600 | ______ |
| TC-RA-108 on `pihole`: the same | ______ |
| TC-RA-109 on `nginx`: N2 `self`, N3 401, N4 `config-set-unavailable` with the file unchanged | ______ |
| Cleanup done on every host | ______ |

Final verdict: ______ (`pending-live-gate` until this table is complete)
