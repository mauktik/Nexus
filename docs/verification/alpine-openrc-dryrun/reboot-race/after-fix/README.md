# Reboot race, after the fix: CONTAINER DRY RUN, NOT real-host evidence

The files one level up (`run-a-*`, `run-b-*`) are the "before" evidence:
the same reboot step ended `outcome-unknown`, `Canceled` or
`Failed script-failed` (see [../../README.md](../../README.md)). These are
the same step after commit `ba56e70` ("a SIGTERM nexusd did not request is
an interruption, not a cancel").

The fix: `Launcher.Cancel` records the cancel request (`<op dir>/cancel`,
atomic rename and fsync) before it signals. The supervisor still stops the
child on any SIGTERM, but reports `canceled` only when that request exists,
and `interrupted` otherwise; nexusd ends an interrupted operation
`Failed outcome-unknown` and never runs Finalize on it. A SIGTERM that comes
with the child's death, as init's SIGTERM to every process does, is
awaited for up to 500 ms. As a second guard, an `exit.json` from an earlier
boot whose child died of a signal its supervisor did not send is
`outcome-unknown` too.

As before, a container restart keeps the host kernel, so `boot_id` does not
change and `5b` fails in every run here; only a real VM reboot can show it.
`1-arch` and `1-host` fail for the same reasons as in the other dry runs.

## Alpine, OpenRC (TC-51 step 5)

`reboot-dryrun.sh` builds nexusd for the Docker host's architecture
(linux/arm64 here) from the committed tree (`ba56e70`, no local changes
under `internal/detach` or `internal/ops`), packs it with `make
nexusd-tree`, and installs it with the release tree's `install.sh` in the
Alpine harness image (Alpine 3.24.4, OpenRC 0.63.2, BusyBox init), started
with the flags of `test/harness/run.sh alpine`. Each round is
`alpine-check.sh --yes --reboot-phase1`, `docker restart -t 60` (BusyBox
init: shutdown runlevel, `Sent SIGTERM to all processes`, `Sent SIGKILL to
all processes`), then `alpine-check.sh --yes --reboot-phase2`. The token
appears in no file (checked with `grep -F` before the container was
removed).

| Set | Round | Operation | 5a | exit.json after the restart |
| --- | --- | --- | --- | --- |
| alpine-set1 | 1 | 01a1d8c1-3113-77ac-98bb-7a9d395b44bb | PASS, Failed outcome-unknown | `"canceled":false,"interrupted":true` (the supervisor handled init's SIGTERM before the SIGKILL) |
| alpine-set1 | 2 | 01a1d8c1-47f4-753e-a2f5-0cb4e702cbcf | PASS, Failed outcome-unknown | none (SIGKILLed first) |
| alpine-set1 | 3 | 01a1d8c1-6103-738e-84e6-efe32a22a97d | PASS, Failed outcome-unknown | none |
| alpine-set2 | 1 | 01a1d8c2-1dd8-75c4-a293-bcc3ee6e0531 | PASS, Failed outcome-unknown | none |
| alpine-set2 | 2 | 01a1d8c2-3919-747b-9d82-526eab43c977 | PASS, Failed outcome-unknown | none |
| alpine-set2 | 3 | 01a1d8c2-53cb-7657-ada4-445322e8d087 | PASS, Failed outcome-unknown | `"canceled":false,"interrupted":true` |
| alpine-set2 | 4 | 01a1d8c2-6868-75e3-9a17-4977bc4145e9 | PASS, Failed outcome-unknown | none |
| alpine-set2 | 5 | 01a1d8c2-80e4-7350-a39f-899c69c8716e | PASS, Failed outcome-unknown | none |

8 of 8 rounds ended `Failed outcome-unknown`. Both ways the race can go
were seen: an `exit.json` written before init's SIGKILL now says
`interrupted` (no `cancel` file exists), and its detail on OpenRC names
"the supervisor process of operation <id>", not a systemd unit. Files per
round: `after-fix-roundN-phase1.txt` and `-phase2.txt` (the script's
transcripts), `after-fix-roundN-op-dir.txt` (the operation's directory,
`exit.json`, `supervisor.log`, and BusyBox init's shutdown lines),
`setup.log`.

## Ubuntu, systemd

`alpine-check.sh` runs its reboot step on OpenRC only, so
`ubuntu-dryrun.sh` does the same check by hand in the systemd harness image
(Ubuntu 26.04, `STOPSIGNAL SIGRTMIN+3`, so `docker restart` halts systemd,
which stops `nexus-op-<id>.service` with SIGTERM): a scripts operation that
sleeps an hour, `docker restart -t 60`, then the operation's status once
nexusd answers. `ubuntu/ubuntu-after-fix.txt`: 3 of 3 rounds ended
`Failed outcome-unknown`, each with `exit.json`
`"canceled":false,"interrupted":true`, no `cancel` file, and the
supervisor's journal line "SIGTERM without a cancel request from nexusd".
