# host provider captures

Real `/proc`, `/sys`, `/etc` and `/run` files from two hosts, with the hosts'
own tool outputs beside them, for the host provider's T0 tests (WI 2.1,
NFR-4, NFR-5, CON-5). Every file was copied from a running host by
`capture.sh` in this directory. Nothing was hand-written: the only rewrite is
the redaction below, which `capture.sh` applies itself and records.

**These files are vectors, not proofs (NFR-5).** Tests compare the parsers'
output with the same host's tools captured in the same run: for example,
MemAvailable from `fs/proc/meminfo` equals `free -b`'s `available` in
`tools/free`, each mount's statfs answer gives `df`'s numbers in `tools/df`,
and the CPU % from `fs/proc/stat` and `t1/proc/stat` agrees with
`tools/vmstat`. A T0 pass on these files is necessary, not sufficient: the T1
harness and the live readbacks are the proof.

## The two captures

| Directory | Host | Init | Under nexusd's unit |
|---|---|---|---|
| `ubuntu-systemd/` | Ubuntu 26.04.1 LTS, kernel 6.8.0-100-generic, aarch64 | systemd (PID 1) | yes |
| `alpine-openrc/` | Alpine Linux 3.24, kernel 6.8.0-100-generic, aarch64 | OpenRC under BusyBox init (PID 1) | no (OpenRC runs nexusd unconfined) |

**These are container captures, not bare-metal ones.** Both hosts are the T1
harness images (`nexus-harness:ubuntu-latest` and
`nexus-harness:alpine-alpine-latest`, built from `test/harness/Dockerfile`
and `Dockerfile.alpine`), started with the flags `test/harness/run.sh` uses,
with the distribution's init as PID 1 and nginx running. Docker ran in a
colima VM (Ubuntu 24.04 kernel, aarch64, 4 CPUs) on macOS, so both captures
share that VM's kernel, its virtio disks, its DMI identity (Apple
Virtualization) and the absence of hwmon sensors, thermal zones and cpufreq.
They are not the Ubuntu host and the Alpine nginx VM of
`docs/verification/nginx-live.md`; a capture from those hosts replaces these
with the same command lines. Each `PROVENANCE` has the image digests and the
exact `docker run` flags in its `note:` line.

- **Ubuntu** ran `capture.sh` as a transient systemd service carrying every
  `[Service]` directive of `deploy/nexusd.service` except `Type`,
  `ExecStart`, `ExecReload` and `Restart` (so `ProtectSystem=strict`,
  `PrivateTmp=yes`, `CapabilityBoundingSet=CAP_DAC_READ_SEARCH`,
  `SystemCallFilter=@system-service` and the rest applied), writing into its
  `StateDirectory`:

  ```sh
  props=$(sed -n '/^\[Service\]/,/^\[Install\]/p' deploy/nexusd.service |
      grep -E '^[A-Z][A-Za-z]*=' | grep -vE '^(Type|ExecStart|ExecReload|Restart)=')
  # each line becomes one -p argument
  systemd-run --wait --pipe --collect -E UNDER_UNIT=... -E CAPTURE_NOTE=... \
      -p StateDirectory=nexus ... /bin/sh capture.sh / /var/lib/nexus/capture
  ```

  So `fs/proc/self/mountinfo` is the sandbox's view (43 lines, with the
  private `/tmp` and the read-only binds) while `fs/proc/1/mountinfo` is the
  host's (24 lines), and `tools/` ran with the unit's capabilities.
  `PROVENANCE`'s `process:` line shows `CapEff: 0000000000000004`
  (`CAP_DAC_READ_SEARCH`), `NoNewPrivs: 1` and `Seccomp: 2`.
- **Alpine** ran `capture.sh` as root through `docker exec`.
  `deploy/nexusd.openrc` starts nexusd as unconfined root (supervise-daemon,
  `command_user root:root`, no capability, namespace or syscall restriction;
  ASM-5), so that process sees the same files.

## Layout

Each capture directory holds:

- `fs/`: the allow-listed paths (architecture.md WI 2.2), at their own paths
  below `fs/`, by access kind:
  - **read** entries copied with `cat` into flat files, so sysfs symlinks
    resolve (`fs/sys/class/net/eth0/address` is a file in a plain directory);
  - **stat-only** entries present on the host as empty placeholders of the
    type `stat` reports after following symlinks; their contents are never
    copied, so no binary and no module tree is here. An entry absent on the
    host is absent here;
  - **list** entries (`proc`, `sys/class/net`, `sys/block`, `sys/block/*`,
    `sys/class/hwmon`, `sys/class/hwmon/*`, `sys/class/thermal`) are the
    directories the entries above create, or empty when the host's is empty.
    They hold only the allow-listed children, not every host entry;
  - the **link** entry `etc/localtime` is copied as a flat file (read) and its
    target is in `tools/readlink-localtime` (absent when `/etc/localtime` is
    not a link or not present, as on the Alpine capture).
- `t1/`: the dynamic files sampled again 10 s after `fs/`: `proc/stat`,
  `proc/diskstats`, `proc/net/dev`, `proc/<pid>/stat` and `proc/<pid>/status`.
  `fs/` holds the same files from the first sample.
- `tools/`: the host's own tools, from the same run (`PROVENANCE` has each
  file's exact argv and exit code in a `tool:` line):

  | File | Command |
  |---|---|
  | `vmstat` | `vmstat 10 2`, started with the first sample, so its second line covers the `fs/` to `t1/` interval |
  | `free` | `free -b` |
  | `df` | `df -B1 --output=target,size,used,avail,pcent,itotal,iused,iavail` (GNU); on BusyBox `df -B1`, with `df -i` in `df-i` |
  | `lsblk` | `lsblk -b -J -o NAME,SIZE,ROTA,RM,MODEL,TYPE,MOUNTPOINTS` (both captures) |
  | `ip-addr`, `ip-route` | `ip -j addr`, `ip -j route` (both hosts have iproute2; `ip addr` / `ip route` text where `-j` fails) |
  | `uname-rm`, `nproc`, `hostname`, `os-release` | `uname -rm`, `nproc`, `hostname`, the host's `/etc/os-release` |
  | `systemctl-failed` (Ubuntu) | `systemctl --failed --plain --no-legend` |
  | `rc-status-crashed` (Alpine) | `rc-status --crashed` (exit 1 with no output: nothing crashed) |
  | `systemd-detect-virt` (Ubuntu) | `systemd-detect-virt` |
  | `timedatectl` (Ubuntu) | `timedatectl show -p NTPSynchronized --value`; `chronyc-tracking` (`chronyc -c tracking`) instead where chronyc is installed (neither capture) |
  | `readlink-localtime` (Ubuntu) | `readlink etc/localtime` |
  | `statfs.txt` | per mount point of `proc/1/mountinfo` (octal escapes decoded, each once, in file order): `stat -f -c '%b %f %a %S %c %d %n'`, that is `Blocks Bfree Bavail Frsize Files Ffree <mount point>`; the F-STATFS `Usage` source |
  | `stat-mounts.txt` | per mount point as above: `stat -c '%d %F %n'`. `%F` (`directory`, `regular file`, ...; it can hold a space) is F-STATFS's `IsDir` source. `%d` is A-38 evidence only (does `st_dev` differ from mountinfo's `maj:min`, as on btrfs); production and the fake never read it |

  The mount point is appended (`%n`) to both lines so the fake can key them.
  The stat calls ran in the capturing process's mount namespace, as the
  production seam does. A mount point that `stat` could not reach there has
  no line and is listed in `UNREADABLE` instead: under the Ubuntu unit,
  `/proc/sys/fs/binfmt_misc` (ELOOP) and the two `/run/credentials/...`
  mounts, which the sandbox hides.
- `PROVENANCE`: one `key: value` line per fact: `host:`, `kernel:`, `date:`,
  `under-unit:`, `process:` (the capturing process's capabilities,
  no_new_privs and seccomp mode), `note:`, `redaction:`, `redaction-counts:`,
  `kept:`, `passwd:`, `mountinfo:`, `script:` (the sha256 of the
  `capture.sh` that ran) and one `tool:` line per `tools/` file.
- `PLACEHOLDERS`: every stat-only placeholder file (`file <path>`) and every
  empty directory (`dir <path>`). **Git does not keep empty directories**, so
  the `dir` entries are missing from a checkout; F-FS (WI 2.2) recreates
  them from this list. Stat-only directory placeholders are always empty, so
  they are all here.
- `UNREADABLE`: each allow-listed path that exists but failed to read
  (`sys/class/net/lo/speed` gives EINVAL on both, as on any virtual link),
  and each mount point `stat` could not reach.

### Placeholders

| Capture | Kind | Path |
|---|---|---|
| ubuntu-systemd | stat-only file | `fs/.dockerenv` |
| ubuntu-systemd | stat-only file | `fs/usr/share/perl5/NeedRestart/UI/Ubuntu.pm` (an apt reboot-marker writer: needrestart is installed) |
| ubuntu-systemd | empty list directory | `fs/sys/class/hwmon`, `fs/sys/class/thermal` (no hwmon devices or thermal zones in the VM) |
| alpine-openrc | stat-only file | `fs/.dockerenv` |
| alpine-openrc | empty list directory | `fs/sys/class/hwmon`, `fs/sys/class/thermal` |

Absent on both hosts, so absent here: `run/.containerenv`, `proc/vz`,
`proc/bc`, `var/run/reboot-required` (and `.pkgs`), `lib/modules/*`,
`usr/bin/chronyc`, `run/systemd/timesync`, `run/chrony/chronyd.pid`,
`run/ntpd.pid`, `sys/class/net/*/device` (veth and bridge links have no
device), `sys/hypervisor/type`, `etc/timezone`. `etc/localtime` is absent on
Alpine only.

## What is never captured, and passwd

`capture.sh` never copies `product_serial`, `product_uuid`, `board_serial`,
`chassis_serial`, `chassis_asset_tag`, `/proc/*/cmdline`, `/proc/*/environ`,
`kmsg`, `shadow`, `gshadow` or `/proc/net/arp`. Its `check_sensitive` guard
exits non-zero if any of those names appears in OUTDIR, and TC-HP-22 checks
the committed tree.

`/etc/passwd` is reduced to system accounts: only entries with uid < 1000 or
uid 65534 (`nobody`) are kept (`awk -F: '$3 + 0 < 1000 || $3 + 0 == 65534'`).
On Ubuntu that dropped the image's `ubuntu` user (uid 1000); `PROVENANCE`'s
`passwd:` line has the counts.

## Redaction (A-41)

On by default. After copying, `capture.sh` rewrites host identifiers across
`fs/`, `t1/` and `tools/` with one mapping per run (files in sorted path
order, first seen first), so every cross-file comparison still holds:

- **MAC addresses** become `02:00:00:00:00:NN` (hex, first-seen order) in
  every file, including `sys/class/net/*/address` and the `ip` outputs.
  `00:00:00:00:00:00` and `ff:ff:ff:ff:ff:ff` stay. This maps every other
  MAC, not only universally administered ones: Docker gives each container a
  random locally administered MAC, which identifies the instance as well.
- **Public IPv4**, any address outside 10/8, 172.16/12, 192.168/16, 127/8,
  169.254/16, 100.64/10 and 192.0.2.0/24, becomes `192.0.2.N` per distinct
  address, in dotted text and in the little-endian hex Destination and
  Gateway columns of `proc/net/route`. `0.0.0.0`, `255.255.255.255` and
  contiguous netmasks are not addresses and stay.
- **Global IPv6** (2000::/3 outside 2001:db8::/32): the /64 prefix becomes
  `2001:db8:0:N::/64` per distinct prefix and the interface ID `::M` per
  distinct ID (a zero ID stays zero), in text and in the destination, source
  and next-hop columns of `proc/net/ipv6_route`.
- **Hostname** becomes `<os-release ID>-capture` (`ubuntu-capture`,
  `alpine-capture`) in `proc/sys/kernel/hostname` and `tools/hostname`.
- **Shapes are kept**: route and ipv6_route columns keep their widths, a file
  without a final newline keeps it missing, binary files (`etc/localtime`)
  are not touched, and private, ULA, link-local and loopback addresses do not
  change, so parsers see real formats.
- **Final check**: the run fails if any MAC other than `02:00:00:00:00:NN`,
  all-zero or broadcast, any public IPv4, or any 2000::/3 address outside
  2001:db8::/32 is left, or if a hostname file is not the new name.

Kept on purpose, as non-sensitive lab data: private, ULA and link-local
addresses (both captures are on Docker's 172.17.0.0/16 and 172.18.0.0/16),
DNS servers (the colima VM's 192.168.5.1), mount sources and process `comm`
names. The committed `docs/verification/pihole-live.md` already carries the
lab's private addresses. Neither capture had a public IPv4 or a global IPv6
address, so only MACs and the hostname were mapped
(`redaction-counts: mac=2 ipv4=0 ipv6-prefix=0 ipv6-iid=0 hostname=1`).

`capture.sh --no-redact` writes `redaction: off` and exists only for local
debugging. **Never commit its output**: TC-HP-135 fails on it.

## Recapturing

```sh
# On the host, as root (POSIX sh; GNU or BusyBox tools):
sh capture.sh / /some/empty/dir
# Then copy the directory here as ubuntu-systemd/ or alpine-openrc/.
```

Run it under nexusd's unit where the host has one (the `systemd-run` line
above), set `UNDER_UNIT` to say how it ran when that is not a systemd unit,
and `CAPTURE_NOTE` to describe the host. TC-HP-21, TC-HP-22 and TC-HP-135
(`hostprov_source_tc_test.go`) check a committed capture.
