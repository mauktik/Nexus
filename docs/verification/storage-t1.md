# storage-provider WI 5.3: T1 on systemd and OpenRC

The installed `nexusd`, under the exact shipped unit (systemd; every test checks
that `/etc/systemd/system/nexusd.service` is byte-equal to `deploy/nexusd.service`,
sha256 `5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a`, with no
drop-in) or the shipped OpenRC script, driven over REST in privileged **harness
containers** on the local Docker host (colima, linux/aarch64, kernel
6.8.0-100-generic). These are container results, not live-host results. **No real
ZFS runs in T1 (A-42)**: `/usr/sbin/zpool` and `/usr/sbin/zfs` are the F-ZFS fake
replaying the committed `home-server` capture. The live checks (TC-STG-L*) are
WI 6.x's and are owed to the operator.

Commit under test: **4d41211** (`test(storage): WI 5.3 T1 integration …`), runs
on 2026-11-19 (UTC). Command, one image at a time:

```
PARITY=0 make test-harness                         RUN='^TestTCSTG|^TestStorageFakeZFS$'
PARITY=0 make test-harness BASE=debian:12          RUN='^TestTCSTG|^TestStorageFakeZFS$'
PARITY=0 make test-harness-alpine BASE=alpine:3.22 RUN='^TestTCSTG|^TestStorageFakeZFS$'
```

`PARITY=0` leaves out the NFR-8 parity suites, which this work item does not
touch (as the services T1 did).

## Results

| Image | Digest | Init | Stamp | EXIT | Pass / skip / fail | Logs |
|---|---|---|---|---|---|---|
| ubuntu:latest (Ubuntu 26.04.1 LTS) | `sha256:d66b366dac61…`; harness image `sha256:26294db777ad` | systemd 259 (259.9-0ubuntu0.1) | 20261119T182125Z | `harness: exit 0`, make 0 | 9 / 3 / 0 | `storage-t1/ubuntu/` |
| debian:12 (as pihole) | `sha256:268fb24c39bc…`; harness image `sha256:cf395285e61a` | systemd 252 (252.39-1~deb12u2) | 20261119T182020Z | `harness: exit 0`, make 0 | 9 / 3 / 0 | `storage-t1/debian12/` |
| alpine:3.22 (as Public-Nginx) | `sha256:49340b346798…`; harness image `sha256:a5706a38ebd0` | OpenRC 0.62.6 | 20261119T182103Z | `harness: exit 0`, make 0 | 2 / 10 / 0 | `storage-t1/alpine322/` |

The skips, every one named in `test.log`:

- `TestStorageFakeZFS` skips on every image: it is the fake's re-exec entry and
  runs only when a wrapper invokes it.
- systemd images: TC-STG-229 (`requires openrc`) and TC-STG-230 (see below).
- alpine:3.22: TC-STG-220 to 226 and 228 (`requires systemd`) and TC-STG-230.
- **TC-STG-230 is not fully verified anywhere.** On every image `capture.sh` ran
  as root and exited 0. It wrote 81 files under `/tmp/cap`, every one with an
  `Exit` line, and `/tmp/capmap.txt` with mode 600. Nothing else was written
  under the writable roots. The test then skips the file-set comparison:
  `captures/pihole` and `captures/public-nginx` are not committed yet (operator
  capture pending, WI 1.2). Ubuntu has no committed counterpart. The comparison
  is owed until those captures land.

| TC | Ubuntu | debian:12 | alpine:3.22 | Observed (Ubuntu unless noted) |
|---|---|---|---|---|
| TC-STG-220 | PASS | PASS | gated | The nine reads answered 200: 3 pools, 18 datasets, 635 snapshots, field by field equal to the capture's text files. `reads.log` had 17 read processes, all `/usr/sbin/zpool` or `/usr/sbin/zfs`. `calls.log` was empty |
| TC-STG-221 | PASS | PASS | gated | The three archives' `sizeBytes` equal `stat -c %s`. The 106 archive's `.notes -> /etc/shadow` gave `notes: null`, reason `no notes file`, and no `/etc/shadow` line appeared in any response. The directory had 3 archives and 2 unrecognized entries (`escape -> /etc` and the escaping notes). The sparsebundle's `lastBackupAt` was 2026-11-18T21:40:00Z from the plist. Its size was 16781312, equal to `du -s --block-size=1 bands`. The backupbundle reported `binary plist not parsed`. The share's `sizeBytes` equals `df -B1` |
| TC-STG-222 | PASS | PASS | gated | No denial pattern in nexusd's journal. MainPID `CapEff 0000000000000004` (only `CAP_DAC_READ_SEARCH`); every read process's CapEff lacked bit 21 |
| TC-STG-223 | PASS | PASS | gated | (1) `pools.list` took 5.013 s, scan null with `zpool status did not finish within 5s`. The killed status process stayed alive in state `D` with SIGKILL pending (`ShdPnd/SigPnd 0x100`): this is **A-31 observed** (a killed tool keeps its slot until its process exits). (2) The second call took 29 ms with `is still running since …`, and no new status process started. (3) Of five parallel reads, at most 4 fake processes ran across 87 samples. (4) `host.health.get` took 2.012 s; the storage block carried `the current read did not finish within 2s`. (5) identity answered 200, and `storage.health.get` raised a critical `zfs-unresponsive` |
| TC-STG-224 | PASS | PASS | gated | Scrub then stop both `Succeeded`, with `scanning` then `canceled`. `calls.log` held exactly `[/usr/sbin/zpool scrub z1]` and `[/usr/sbin/zpool scrub -s z1]`, euid 0, CapEff `000001ffffffffff` (bit 21 set), with parents `nexusd storage-helper run` ← `nexusd op-supervise`. The audit trail was `requested Queued Running Succeeded`, facts `pool: z1`, `force: false` |
| TC-STG-225 | PASS | PASS | gated | `calls.log` held `[/usr/sbin/zfs snapshot -o nexus:created-by=<op id> z1/ds/share@nexus-20261119T182203Z-t1]` with bit 21. The result's item equals the `createdByNexus=true` listing byte for byte. Facts were `dataset`, `snapshot`, `recursive: false` |
| TC-STG-226 | PASS | PASS | gated | Four refusals: 409 `not-allowed` (z2), 409 `scan-in-progress`, 409 `not-allowed` (appdata), and 409 `not-allowed` with `configKey providers.storage.scrub.enabled`. The `refused` audit count rose by one each time (1 to 4). `calls.log` held only the first z1 scrub |
| TC-STG-227 | PASS | PASS | PASS | The status was `unavailable` with the exact three-part reason, and all 12 ops answered 503 `capability-unavailable` under both configurations. The `host.health.get` storage block had `counts: null` and the reason. Identity answered 200. The preflight line count went 1 → 1 → 2 over 20 requests (about 1.1 s after the start) and one reload, and the other storage lines stayed at 2 → 2. The host health shape is unchanged with storage disabled |
| TC-STG-228 | PASS | PASS | gated | `features.zfs.reason` was `zfs: /dev/zfs not found (the zfs kernel module is not loaded)`, and `reads.log` was empty |
| TC-STG-229 | gated | gated | PASS | Scrub and stop both `Succeeded`. Two `calls.log` lines had the exact argv, euid 0, and PPid = `nexusd storage-helper run`, whose parent is `nexusd op-supervise --dir … --process` |
| TC-STG-230 | SKIP | SKIP | SKIP | See above: everything except the owed file-set comparison |

## How the fake runs under the unit

- `/usr/sbin/zpool` and `/usr/sbin/zfs` are two-line sh wrappers:
  `NEXUS_HARNESS=1 exec /harness/integration.test -test.run='^TestStorageFakeZFS$' -- nx-fakezfs /run/nx-fakezfs <capture> /dev/shm/nx-fakezfs/reads.log <tool> "$@"`.
  The capture root and the state directory are passed as arguments, not as
  environment variables.
- The entry, `storagetest.Main`, applies `/run/nx-fakezfs/state.json`, replays F-ZFS
  and saves the state after an accepted write. It decides the A-7 denial from its
  own `/proc/self/status` CapEff. Each write is appended to `calls.log` with its
  argv, euid, environment, CapEff, PPid and parent command lines.
- `run.sh` copies `internal/providers/storage/testdata` to
  `/harness/src/github.com/mauktik/nexus/internal/providers/storage/testdata`,
  beside the svcctl line.
- The colima VM has the zfs module loaded, so a privileged container already sees
  `/dev/zfs` (c 10,249). The no-`/dev/zfs` cases rename it aside and restore it.

## Deviations from the test-case text

1. **`reads.log` is on `/dev/shm`, not in `/run/nx-fakezfs`.** Under the unit,
   `ProtectSystem=strict` makes `/run` read-only for nexusd's reads, and
   `PrivateTmp=yes` hides `/tmp`. `/dev/shm` is shared and writable. `state.json`
   and `calls.log` stay in `/run/nx-fakezfs`, so only writes outside the sandbox
   can change them, as specified.
2. **The exact-environment check removes what the wrapper adds.** The wrapper
   adds `NEXUS_HARNESS=1` (so the test binary's TestMain runs). Its shell exports
   `PWD`, and BusyBox ash also exports `SHLVL=1`. The rest is exactly
   `LC_ALL=C`, `PATH=/usr/sbin:/usr/bin:/sbin:/bin` on every image (see
   `calls.log` lines in `test.log`). Because the shell sets these three names
   itself, an `Exec` leak of exactly those three would not be seen.
3. **TC-STG-223's hang is real kernel state, not a killable sleep.** With a
   killable hang, step (2) cannot hold: SIGKILL frees the slot at once
   (test-strategy.md says so for `Hang`). Here the fake stats a name on a FUSE
   mount the test serves, whose `LOOKUP` it reads and never answers. Once SIGKILL
   arrives, the caller waits uninterruptibly (`fs/fuse/dev.c`
   `request_wait_answer`). Cleanup aborts the connection by closing `/dev/fuse`,
   and the processes exit. A Go process whose leader thread exited keeps no
   command line, so `pgrep -f` can lose it. The count therefore reads every
   thread's cmdline under `/proc`, and `pgrep -fc` is logged beside it (both
   gave 4).
4. **TC-STG-227: `refused` audit records do exist.** The text says no `refused`
   record is written. The core, however, audits the capability refusal of every
   mutating request (`internal/core/dispatcher.go` `refuse`, pinned by
   `internal/core/dispatcher_audit_test.go`). Six such records exist (three
   writes × two configurations), each with `capability-unavailable`. None is an
   admission refusal (`not-allowed`), which is the finding's point. The test
   asserts exactly that, and requires at least one new `capability-unavailable`
   record per write refused in step 2. The TC text was amended in `636bc99` to
   name the core's own `capability-unavailable` record.
5. **TC-STG-227: "exactly two preflight lines"** holds only when the 20 requests
   finish within 5 s of the start. The registry re-checks an unavailable provider
   on a call every 5 s (TC-STG-118), and a re-check is a preflight. The test
   asserts exactly 1 then 2 inside that window (every run took about 1.1 s). On a
   slower run it would assert at most one line per 5 s. The core's per-request
   `request refused` line is not counted as a storage line.
6. **TC-STG-227 runs on every image**, Ubuntu included, and passes there. TC-STG-220
   to 226 and 228 also run on debian:12 (systemd 252) and pass.
7. **TC-STG-221 places `escape -> /etc` in `dump/`**, beside the escaping notes.
   Both are counted unrecognized (2).
8. The nexusd provider is enabled in these configs so that
   `nexusd.actions.reload` can run (token: `nexusd:read`, `nexusd:admin`).

## Rerun of TC-STG-227 (review fix, debian:12)

After the spec review (B1, M1) the audit assertion of TC-STG-227 now requires, per
write refused in step 2, at least one new `capability-unavailable` record. Run on
debian:12 (systemd 252), `PARITY=0 make test-harness BASE=debian:12
RUN='^TestTCSTG227'`: stamp **20261119T183014Z**, `harness: exit 0`, PASS
(`storage-t1/debian12-tc227-rerun/`). Observed: `audit: 3 capability-unavailable
records for the 3 writes of step 2 (6 refused storage records in all, none
not-allowed)`.

Red: with the lookup key mutated so no record can match (`after[op.ID+"x"]`), the
same run, stamp 20261119T182951Z, failed with `0 new capability-unavailable audit
records, want at least 1` for all three writes (`debian12-tc227-rerun/red-mutant/`).

The FUSE wire constants of the fake were checked against `include/uapi/linux/fuse.h`
at Linux v6.8; none changed, so the other images were not rerun.

## Red

The first Ubuntu run (`storage-t1/red-ubuntu-run1/test.log`, stamp
20261119T181435Z) failed TC-STG-226 and 227:

- `nexusd.actions.reload` answered 503 `provider nexusd is disabled`.
- TC-STG-227 counted 2 preflight lines before any request, because the journal's
  whole-second `--since` window held the previous test's daemon.

Fixes: the nexusd provider is enabled, the journal is filtered by MainPID, and the
token gained `nexusd:read`. Paths in the logs were rewritten to `<repo>` and `~`.

## Owed

- TC-STG-230's file-set comparison: after the pihole and public-nginx captures are
  committed (WI 1.2).
- A-7 `real-device-exchange`: the fake's write denial checked against WI 1.2's
  `--write-probe` capture, when the operator opts in.
- The live gates TC-STG-L0 to L11 (WI 6.x). Run T1 is no substitute for them.

## Rerun of TC-STG-223 (code review CR-12, ubuntu:latest)

CR-12 moved the five parallel reads of step 3 off `t.Fatal`: the goroutines call
a non-fatal GET (`tryGet`) and the test goroutine asserts each answer is 200, or
503 naming the pending or timeout reason. The same run carries the code review
fixes of part B (the helper's pool-health re-check, the confirm poll's bounds,
zfs-unresponsive by read kind), so the installed `nexusd` is the fixed one.
`PARITY=0 make test-harness RUN='^TestTCSTG223'` on ubuntu:latest: stamp
**20261119T185811Z**, `harness: exit 0`, PASS (`storage-t1/ubuntu-tc223-rerun/`).
Observed: `(3) five reads answered map[datasets.get z1/ds/share:200 pools.get z1:200
pools.get z2:200 pools.get z3:200 snapshots.list:200]; at most 4 fake processes over
86 samples`. No red run: the defect was a latent misuse of `testing` that no input
makes fail, and the test's new assertions pass on the five answers observed.
