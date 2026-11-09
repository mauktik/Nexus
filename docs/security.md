# Security: the nexusd systemd unit

This page records the hardening of `deploy/nexusd.service`: the directives it
applies, the directives removed since the unit was created, and every
`systemd-analyze security` check the unit does not meet, with the reason.

`nexusd` runs as root under this unit, because Docker socket access and apt
are root-equivalent anyway. The privileged work (apt changes, script runs,
container updates) runs in `nexus-op-*` transient units that `nexusd` starts
with `systemd-run`. Those units are outside this sandbox. The sandbox limits
what the `nexusd` process itself can do if it is compromised. It is not a
boundary between a token holder and root: a token with a mutating `docker`,
`apt` or `scripts` scope is potentially root-equivalent.

## Directives in force

Every directive below has been in the unit since it was created (WI 3.5).
Each provider's T1 suite (Docker, apt and scripts against the real
dependency) runs `nexusd` under this unit, and one test runs every catalog
operation under the unit exactly as `install.sh` installs it, with no
drop-in, and finds no seccomp or permission denial in the journal.

| Directive | Effect |
| --- | --- |
| `Type=notify` | `systemctl start` returns only after `nexusd` reports `READY=1`, once every listener is open |
| `ExecReload=/bin/kill -HUP $MAINPID` | `systemctl reload` re-reads the config; an invalid config is logged and the running one is kept |
| `Restart=on-failure` | A crashed `nexusd` restarts; operations keep running in their own units |
| `StateDirectory=nexus`, `StateDirectoryMode=0700` | `/var/lib/nexus` is the only writable state location |
| `LogsDirectory=nexus`, `LogsDirectoryMode=0700` | `/var/log/nexus` holds the audit log |
| `NoNewPrivileges=yes` | No set-user-ID or file-capability gain |
| `ProtectSystem=strict` | The whole file system is read-only except the two directories above |
| `ProtectHome=read-only` | Home directories are visible but read-only |
| `PrivateTmp=yes` | A private `/tmp` and `/var/tmp` |
| `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, `ProtectKernelLogs=yes` | No writes to `/proc/sys` or `/sys`, no module loading, no kernel log |
| `ProtectControlGroups=yes`, `ProtectClock=yes`, `ProtectHostname=yes` | No cgroup, clock or hostname changes |
| `RestrictNamespaces=yes` | No new namespaces |
| `RestrictRealtime=yes`, `RestrictSUIDSGID=yes`, `LockPersonality=yes` | No realtime scheduling, no set-user-ID files, fixed ABI personality |
| `MemoryDenyWriteExecute=yes` | No writable and executable memory |
| `SystemCallArchitectures=native`, `SystemCallFilter=@system-service` | Native system calls only, from systemd's set for system services |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` | No other socket family |
| `CapabilityBoundingSet=CAP_DAC_READ_SEARCH` | Every capability is dropped except reading files regardless of permissions |

## Directives removed since WI 3.5

None. The unit applies every directive of the WI 3.5 set. A directive removed
later must be listed here with its reason; the T1 audit fails when a WI 3.5
directive is missing from the unit and not listed here.

## `systemd-analyze security` review

`systemd-analyze security nexusd` on systemd 259 (Ubuntu 26.04.1) rates the
unit `2.6 OK`. Each check it reports as not met is listed here, by the name it
prints, with the reason it is not applied.

Needed by what `nexusd` does:

- `User=/DynamicUser=`: `nexusd` runs as root (ASM-6). It talks to the Docker Engine socket and starts root `nexus-op-*` units, so a non-root user would add a broker without a real boundary.
- `PrivateUsers=`: A user namespace maps root to an unprivileged user. Root-owned host files, such as the Docker socket, the dpkg database and the config, would then be unowned. The config ownership check and the script integrity checks also read the real owners.
- `RootDirectory=/RootImage=`: `nexusd` reads the host's apt and dpkg state, runs the host's `apt-get`, `apt-cache`, `dpkg-query`, `dpkg`, `systemctl` and `systemd-run`, and reads scripts and Compose files anywhere on the host.
- `PrivateNetwork=`: `nexusd` serves the API over TCP to LAN clients and reaches container registries for update checks.
- `IPAddressDeny=`: Clients are on whatever LAN addresses the operator uses, and registries can be anywhere. An operator can add an allow list for their LAN with a drop-in, for example `IPAddressAllow=192.168.1.0/24 localhost` and `IPAddressDeny=any`, at the cost of registry checks from `nexusd`.
- `RestrictAddressFamilies=~AF_(INET|INET6)`: The TCP listener and registry clients.
- `RestrictAddressFamilies=~AF_UNIX`: The Docker Engine socket, systemd's bus for `systemctl` and `systemd-run`, `sd_notify` and journald.
- `RestrictAddressFamilies=~AF_NETLINK`: Go's `net.Interfaces` reads the interface addresses over netlink for the generated certificate's names.
- `ProtectHome=`: Read-only, not inaccessible: scripts and Compose files can live in home directories, and the integrity checks and Compose updates read them.
- `CapabilityBoundingSet=~CAP_(DAC_*|FOWNER|IPC_OWNER)`: `CAP_DAC_READ_SEARCH` is kept so that root can read scripts, Compose files and registry credentials in other users' private directories. `CAP_DAC_OVERRIDE`, `CAP_FOWNER` and `CAP_IPC_OWNER` are dropped.

Not applied in this release:

- `SystemCallFilter=~@privileged`: The allow list is systemd's `@system-service` set, which includes `@chown` and `@setuid`. Host tools run inside this sandbox for reads and previews (`apt-get -s`, `apt-cache`, `dpkg-query`, `dpkg --audit`, `systemctl`, `systemd-run`), and a call outside the filter kills the process with `SIGSYS`. A narrower filter has not been verified against every one of them.
- `SystemCallFilter=~@resources`: As above: `@system-service` includes `@resources` (`setrlimit`, `setpriority` and others), and a narrower filter has not been verified against the host tools that run in the sandbox.
- `PrivateDevices=`: Not part of the verified set. `nexusd` itself needs only `/dev/null` and `/dev/urandom`; the host tools that run in the sandbox have not been verified without the host's `/dev`.
- `DeviceAllow=`: Follows from `PrivateDevices=` above.
- `ProtectProc=`: Not part of the verified set. Hiding other users' processes in `/proc` has not been verified against `systemctl`, `systemd-run` and apt inside the sandbox.
- `ProcSubset=`: Not part of the verified set. Hiding non-process files in `/proc`, such as `/proc/sys/kernel/random/boot_id`, has not been verified against the same tools.
- `UMask=`: Not part of the verified set. The unit keeps systemd's default `0022`. `nexusd` creates every file it keeps with an explicit mode: the config is `0600` (written by `install.sh`), and `state.db`, `tokens.json`, the TLS key and the audit log are `0600` in `0700` directories.

A directive in this second list can be added only after every provider's T1
suite has passed under it.

To repeat the review on a host:

```
systemd-analyze verify /etc/systemd/system/nexusd.service
systemd-analyze security nexusd
```
