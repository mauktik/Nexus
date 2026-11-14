# Pi-hole live verification

Live checks of the Pi-hole provider on the real host (work items 6.1 and
6.2). The host is a Debian VM on Proxmox, `pihole`, 192.168.7.4.

Root SSH is disabled on the host. So step 0 and the deploy ran from one
script, typed into the Proxmox console as root. It downloaded the release
tree, ran the checks and posted its report back over the LAN. The script
has three parts, in order:

1. The read-only step 0 checks, written to `/tmp/nexus-step0.txt`.
2. The stop gates of WI 6.1.
3. `install.sh`, then two config edits, then `systemctl enable --now nexusd`.

Every later step goes through `nexusd`'s API from the Mac.

## Step 0: read-only host facts (2026-11-14 04:56 UTC)

| Check | Live value | Result |
| --- | --- | --- |
| Architecture | `x86_64` | matches the shipped `nexusd-linux-amd64` |
| Versions | core v5.18.4, web v5.21, FTL v5.25.2 (`8943e26`, 2024-05-08) | v5 backend |
| `dnsFile` | `/etc/dnsmasq.d/03-custom-dns-names.conf`, 1947 bytes, mtime 2026-10-15 22:03:16 -0700, 53 `address=` lines | |
| `dnsFile` sha256 | `35e3646ba1ca58c420b933d0b63df0dd3cba640d950a114dc3c9d03ed9f3bf2d` | **equals** the committed fixture `live-53-lines.conf`: no drift, fixture unchanged |
| `/etc/dnsmasq.d` | `01-pihole.conf`, `03-custom-dns-names.conf`, `06-rfc6761.conf` | no 02/04/05 files |
| `conf-dir` | `/etc/dnsmasq.conf:1:conf-dir=/etc/dnsmasq.d` | |
| FTL unit (A-55) | `FragmentPath=/etc/systemd/system/pihole-FTL.service`, `Type=simple`, `ExecStart=/usr/bin/pihole-FTL -f`, `User=pihole`; no `/etc/init.d/pihole-FTL` | **native unit**: the T1 native harness covers it, and the sysv run is not required |
| Start limit (A-60) | `StartLimitBurst=5`, `StartLimitIntervalUSec=1min`, `Restart=on-failure`, `RestartUSec=5s`, `Result=success` | 5 starts per 60 s, which confirms A-61's `reset-failed` is needed |
| Merged `/usr` (A-62) | `/bin -> usr/bin`, `/sbin -> usr/sbin`; `/usr/bin/systemctl` and `/usr/bin/systemd-run` present | pass |
| `pihole-FTL --test` | `dnsmasq: syntax check OK.` `EXIT=0` | pass |
| `>version` | `version v5.25.2 … hash 8943e26` | |
| `>stats` | `domains_being_blocked 78216`, `status enabled`, 111700 queries today, 30 clients | |
| `gravity.db` (read-only open) | `journal_mode=delete`; `gravity` 78216 rows, all in `adlist_id` 1; 0 enabled regex entries; 0 rows in `client` | A-59: a single adlist owns all 78216 rows |
| FTL log | `/etc/pihole/pihole-FTL.conf` sets no `LOGFILE`, `PIDFILE` or `FTLPORT`, so the defaults apply; `/var/log/pihole/FTL.log` is `-rw-r--r-- pihole:pihole` | readable: the WI 5.6 barrier can work |
| `database is locked` / `Gravity database not available` | 0 in `FTL.log`, `.1`, `.2.gz` and `.3.gz` | A-64's race has not happened on this host so far |
| Reload markers | `20:50:58.375 Reloading DNS cache` → `20:50:58.478 Compiled 0 whitelist and 0 blacklist regex filters for 30 clients in 0.4 msec` | the only reload in the current log took 103 ms (FTL started at 20:50:57 PST) |
| `06-rfc6761.conf` | line 25 `server=/invalid/` | pass: the barrier's `.invalid` probe stays local |
| Existing `nexusd` | none (no binary, no `/etc/nexus`, unit inactive) | first install |

All the gates of WI 6.1 passed:
- the architecture matches;
- merged `/usr`;
- `--test` exits 0;
- the log is readable;
- `server=/invalid/` is present.

## Step 1: deploy (approved: the user deployed it)

- **Build:** `nexusd d23f764`, built for linux/amd64 by `./deploy.sh --stage-only`. The tarball's sha256 is `e6ec70d90dca87b3768ffd4686fc7a6a0c3aba1b83d567097880ff6a2973e782`, checked on the host before unpacking.
- **Install:** `install.sh` wrote the default config.
- **Config edits:** the script made exactly two.
  - `listen: ["127.0.0.1:7443", "192.168.7.4:7443"]` (line 14), so the Mac can reach it.
  - `providers.pihole.enabled: true` (line 89).
- **Start:** `validate-config` reported the configuration valid, and `systemctl enable --now nexusd` started it.
- **TLS:** the fingerprint is `00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF`. The Mac's `openssl s_client` saw the same fingerprint, and an unauthenticated request returned `401`.
- **Token:** `nexus-mac-test`, with scopes `pihole:read`, `pihole:write` and `pihole:dns`, expiring after 7 days. It is not recorded here.

## Step 2: read-only reads compared with the host (2026-11-14, from the Mac)

These come from `nexus --host pihole …` against `nexusd d23f764`. "Host view" is the step 0 report, or `dig @192.168.7.4` from the Mac.

| Read | Nexus | Host view | Match |
| --- | --- | --- | --- |
| `capability get` | pihole `enabled`, backend `v5-cli`, core v5.18.4, FTL v5.25.2, web v5.21, `dnsWrites: available`, `preNexusBackupExists: false` | `/etc/pihole/versions`; no backup yet | yes |
| `pihole get` | blocking `enabled`, `ftlRunning`, `dnsAnswering`, `gravityDomains` 78216, `gravityUpdated` 2026-11-08T12:48:01Z | `>stats`: `status enabled`, `domains_being_blocked 78216` | yes |
| `piholeblocking get` | `enabled`, `reenableAt: null` | `status enabled` | yes |
| `piholesummary get` | 78216 domains, 30 clients, `ftlApiReachable` | `>stats` (the query counts moved on between the two reads) | yes |
| `piholeadlist get` | 1 adlist, id 1, StevenBlack hosts, 78216 domains, enabled | `gravity`: 78216 rows, all in `adlist_id` 1 | yes |
| `piholedomain get` | 4 entries, all allow / exact / enabled | 0 enabled regex entries | yes |
| `piholezone get` | no zones | no `# nexus:zone` markers in the file | yes |
| `piholerecord get` | 53 A records, all `zone: null`, `duplicate: false`, etag `sha256:35e3646b…bf2d` | the 53 `address=` lines: same names, IPs and order; the etag is the file's sha256 | yes |
| DNS | — | `dig @192.168.7.4` returns the file's IP for each of the 53 names (53/53); `pi.hole` → 192.168.7.4; `nexus-test.app.lan` → no answer (the name is free); `nexus-probe-x.invalid` → NXDOMAIN in 1 ms (answered locally) | yes |

Read latency from the Mac was 23–30 ms per CLI call, end to end, across those six reads.

## WI 6.2 step 1: first write, `piholezone add --zone app.lan` (approved 2026-11-14)

```
nexus --host pihole piholezone add --zone app.lan --if-match sha256:35e3646ba1ca58c420b933d0b63df0dd3cba640d950a114dc3c9d03ed9f3bf2d --follow --yes
```

- **Operation:** `01a1d86e-68b9-745d-ae59-5276849e0868` ended `Succeeded`; 609 ms end to end from the Mac.
- **Op log:** it saved the pre-Nexus backup, wrote the temporary file and saved the rollback copy. It then replaced the file and logged `only comment lines changed: no configuration test and no DNS restart`.
- **Result:**
  - `restarted: false`;
  - `ftlPidBefore` 176, `ftlPidAfter` 176: FTL was not restarted;
  - new etag `sha256:097da9de517d5227574c241282a30d2f807da33c6143c90775d2ae7862264dfe`.
- **File contents:** the new etag equals the sha256 of a local dry run, `dnsfile.Parse` → `AddZone{app.lan}` → `Render` on `live-53-lines.conf`, which reported `DNSEquivalent=true`. So the live file is byte-for-byte the predicted reorganization (A-42):
  - `# nexus:zone app.lan` with its 24 records, sorted;
  - `# nexus:unzoned` with the other 29.
- **Readback:**
  - `piholezone get` lists `app.lan` with 24 records;
  - `piholerecord get` lists 53 records: 24 in `app.lan` and 29 with no zone;
  - `dig @192.168.7.4` still returns the expected IP for 53 of the 53 names.
- **Backup:**
  - Plain `capability get` still showed `preNexusBackupExists: false`, the snapshot from nexusd's start, as documented.
  - `capability get --refresh` showed `true`.
  - On the host console, `sha256sum` of the backup printed `35e3646ba1ca58c420b933d0b63df0dd3cba640d950a114dc3c9d03ed9f3bf2d`, equal to step 0's. The pre-Nexus backup is the original file byte for byte.

## WI 6.2 step 2: record add, update and remove of `nexus-test.app.lan` (approved 2026-11-14)

The test IPs are 192.0.2.10 and 192.0.2.11, from TEST-NET-1. After each op, the Mac ran `dig @192.168.7.4` for the test name and for all 53 original names.

| Op | id | FTL pid | Restart → FTL answering | Nexus verify | `dig` from the Mac | 53 names | End to end |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `piholerecord add --fqdn nexus-test.app.lan --ipv4 192.0.2.10` | `01a1d870-74af-…1141` | 176 → 792 | 401 ms | expected [192.0.2.10], observed [192.0.2.10], NOERROR | `192.0.2.10` | 53/53 | 609 ms |
| `piholerecord update nexus-test.app.lan~A --ipv4 192.0.2.11` | `01a1d870-790b-…6eb2` | 792 → 858 | 401 ms | expected [192.0.2.11], observed [192.0.2.11], NOERROR | `192.0.2.11` | 53/53 | 571 ms |
| `piholerecord remove nexus-test.app.lan~A` | `01a1d870-7d31-…15c6` | 858 → 924 | 401 ms | expected [], observed [], NXDOMAIN | no answer | 53/53 | 564 ms |

Each op logged the full pipeline: temp file, rollback copy, atomic replace, `pihole-FTL --test` (`syntax check OK`), `pihole restartdns`, the readiness wait and the per-name verify. Commanded matched observed both in Nexus's own verify and in an independent `dig` from the Mac.

After the remove, the file's etag was back to `sha256:097da9de…`, byte-for-byte the file step 1 left. `piholerecord get` lists 53 records, and `app.lan` holds 24 records.

## WI 6.2 step 3: blocking stop, start and a 60 s timed stop (approved 2026-11-14)

The probe was `doubleclick.net`, which is on the StevenBlack list. Each probe was `dig @192.168.7.4` from the Mac, together with `piholeblocking get`.

| Time (UTC) | Action | op | `doubleclick.net` | `piholeblocking get` |
| --- | --- | --- | --- | --- |
| 05:04:54 | before | — | `0.0.0.0` | enabled, reenableAt null |
| 05:04:54 | `piholeblocking stop` (565 ms) | `01a1d871-ba22-…c5aa` | `142.250.73.110` | disabled, reenableAt null |
| 05:04:55 | `piholeblocking start` (372 ms) | `01a1d871-bcd2-…ff31` | `0.0.0.0` | enabled |
| 05:04:55 | `piholeblocking stop --duration-seconds 60` (414 ms) | `01a1d871-bea5-…c7a8` | `142.250.73.110` | disabled, reenableAt `2026-11-14T05:05:55.507Z` |
| +5 s … +56 s | (no Nexus action) | — | `142.250.73.110` on every 5 s probe | — |
| +61 s, 05:05:56 | systemd timer `nexus-pihole-reenable.timer` | — | `0.0.0.0` | enabled, reenableAt null |

- **Timed stop:** blocking came back on schedule, run by the systemd timer, between the +56 s and +61 s probes. reenableAt was 60 s after the op, with no nexusd involvement (A-5).
- **Commanded matched observed** in all four transitions.
- **Cosmetic:** each op's log carries `Failed to stop nexus-pihole-reenable.timer: Unit … not loaded` and `reset-failed … exited 1; continuing`. These come from clearing a timer that does not exist. They are harmless, and the op logs that it continues.
