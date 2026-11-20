# Storage provider — live verification (WI 6.1 / 6.2)

## Build
v0.1.0-rc7 from the local integration branch `integ/rc7` (5c2399f = feat/services-provider + feat/storage-provider; not pushed), signed, staged at the operator's file share. home-server self-updated rc6 → rc7 on 2026-11-20T04:19Z (`nexusd.actions.update`, Succeeded, rolledBack false). `providers.storage.enabled=true` set remotely (`nexusd.actions.update-config`, no restart). Writes stay disabled (scrub.enabled, snapshots.enabled false; allow-lists empty).

## WI 6.1 live reads (from the Mac, token `claude-storage`: storage:read + host:read)
| Read | Result |
|---|---|
| `nexus pool get` | z1/z2/z3 `online`, capacity 16/34/36 %, readOnly false; z1 scan scrub finished, `lastScrubFinishedAt` 2026-11-08T08:25:49Z (11.8 days), source `status` — matches the committed capture |
| `nexus dataset get` | 18 datasets |
| `nexus snapshot get --limit 500` | 500 items (page full; capture shows 635 snapshots) |
| `nexus storagehealth get` | checked [zfs], 0 signals; backups / timeMachine `not configured` (path lists not yet set on the host) |
| `nexus hosthealth get` | `providerSignals` has the storage block (stale false, same counts) |
| regression | services provider still answers on rc7 (372 services) |

Observation (Low): `scan.percentDone` reads 100.06 for a finished scrub (examined > to-examine, as `zpool status` reports it); passed through unclamped.

## Owed
- Backups and Time Machine live reads: need `backups.vzdumpPaths` and `timeMachine.paths` added on the host (list keys are file-edit only, A-38), then a reload.
- Sandbox denial check in the journal after those reads (operator, as root).
- pihole / public-nginx degradation captures (WI 1.2) and their capture-gated tests.
- WI 6.2 live writes: skipped at Step 9 (2026-11-19).
