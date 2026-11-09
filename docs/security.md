# Security model

This page states what Nexus defends against and what it does not, how to
expose it, how tokens work, and how the `nexusd` systemd unit is hardened.
Read it before giving anyone, person or agent, a token.

## Threat assumptions

- **Nexus is LAN only.** `nexusd` is reached over the home LAN, through a
  self-signed certificate whose fingerprint clients pin (ASM-5). Nothing in
  Nexus was designed or tested for the internet: there is no mTLS, OIDC,
  IP allow list or abuse protection beyond a failure rate limit.
- **The people and machines on the LAN are not all trusted.** Every request
  except `GET /healthz` needs a bearer token, plain HTTP is refused on any
  address but loopback, and `/mcp` refuses a browser `Origin` that
  `mcp.allowedOrigins` does not list, so a web page open on the LAN cannot
  call the API.
- **A token holder is trusted with exactly its scopes,** and with everything
  those scopes can reach on the host, which for the mutating scopes is
  potentially everything (see below). There is no approval step: scope
  alone authorizes (ASM-4).
- **Root on the host is trusted.** Root can read the configuration, mint
  tokens, edit the audit log and replace the binary. Nexus does not defend
  against root.

## Network exposure

Keep `nexusd` LAN only.

> **Warning:** do not expose `nexusd` to the internet.
> Do not use port forwarding on the router to reach it.
> Do not publish it through the public side of the nginx gateway, or through any other reverse proxy. Reaching a host from off the LAN
> is not supported.

- The default configuration listens on `127.0.0.1:7443` only. Listening on
  a LAN address is an explicit change to `listen` in
  `/etc/nexus/nexusd.yaml`; `install.sh` never writes one.
- On any address other than a loopback IP, TLS is required: `tls.mode: off`
  is refused at startup and by `nexusd validate-config` unless every listen
  address is a loopback IP such as `127.0.0.1` or `[::1]`, and there is no
  override. `0.0.0.0`, `[::]` and host names, `localhost` included, are not
  loopback for this rule.
- TLS 1.2 is the minimum. In `tls.mode: auto`, `nexusd` generates a
  self-signed ECDSA P-256 certificate on first start, keeps it in
  `/var/lib/nexus/tls/` with mode 0600, and logs its SHA-256 fingerprint;
  `nexusd tls fingerprint` prints it again. The certificate is a plain leaf
  (`IsCA: false`), so its key cannot vouch for any other name. The `nexus`
  CLI pins the fingerprint (`nexus host add`); other clients trust that one
  certificate per client, as [mcp.md](mcp.md) describes.
- There are no CORS headers.
- **Forwarded headers are never honored.** `nexusd` takes a request's source
  address from the TCP peer only. `X-Forwarded-For`, `Forwarded` and
  `X-Real-IP` are never read, and there is no setting to trust a proxy
  (A-60). A proxy placed in front of `nexusd` would therefore hide every
  client's address: the audit log would record the proxy for every request,
  and the authentication failure limit would count every client's failures
  as one source, so one client's bad token would lock out all of them.

## Tokens

- Tokens are created, listed and revoked only on the host, as root, with
  `nexusd token create`, `nexusd token list` and `nexusd token revoke`. No
  API or MCP call creates one, so a stolen token cannot mint another.

  ```
  sudo nexusd token create --name laptop --scope containers:read --scope packages:read
  sudo nexusd token create --name agent --scope scripts:read --scope scripts:run:deploy-api --expires 30d
  sudo nexusd token list
  sudo nexusd token revoke agent
  ```

- A token is 256 random bits, printed once by `nexusd token create` and
  never shown again. `/var/lib/nexus/tokens.json` holds only its SHA-256
  hash, its name, scopes, creation time and optional expiry. Tokens are
  compared in constant time.
- A new token has no scopes unless `--scope` gives them. Revocation takes
  effect on the next request, without a restart.
- More than `auth.failureLimitPerMinute` (default 10) authentication
  failures per minute from one source (an IPv6 /64 counts as one) answer
  `429` before any token is evaluated.
- Tokens never appear in logs, audit records, error bodies or operation
  records. The `nexus` CLI keeps its tokens in
  `~/.config/nexus/config.yaml` with mode 0600 and refuses a file that a
  group or others can read; it never takes a token as a command-line
  argument.
- Give each person and each agent its own token, with the fewest scopes,
  and an expiry for anything temporary. For an agent, prefer read scopes
  plus `scripts:run:<id>` for the specific scripts it may run.

### Scopes

| Scope | Allows |
| --- | --- |
| `containers:read` | Container inventory, logs and update checks |
| `containers:write` | Container start, stop, restart, remove and update |
| `packages:read` | Installed and upgradable packages |
| `packages:write` | Index refresh and package upgrades |
| `packages:install` | Package install and remove |
| `scripts:read` | The script catalog, without environment values |
| `scripts:run` | Running any cataloged script |
| `scripts:run:<id>` | Running the one script `<id>` |
| `audit:read` | Querying the audit log |

The generated [API reference](api.md) lists the scope of every operation.

## Mutating scopes are potentially root-equivalent

Treat any token that holds a mutating scope for `docker`, `apt` or
`scripts` (`containers:write`, `packages:write`, `packages:install`,
`scripts:run` or any `scripts:run:<id>`) as potentially root-equivalent:

- Package upgrades and installs run maintainer scripts as root, from
  whatever the configured apt sources serve.
- A container update recreates a container from the newest image its tag
  points to, with the container's existing privileges and mounts.
- A script runs as its run-as user with whatever that user can do, and that
  user can be root.

Nexus narrows what callers can ask for: no operation accepts a command,
shell string, executable path or container specification, every parameter
is validated, and scripts are defined only in the root-owned configuration
file. That limits what a token can request, not what the requested work can
do once it runs.

## nexusd runs as root; the sandbox is not a security boundary

`nexusd` runs as root under a hardened systemd unit (ASM-6), with no
privilege-separation helper, because Docker socket access and apt are
root-equivalent anyway: a helper would add a process without a real
boundary. The sandbox described below limits what the `nexusd` process
itself can do if a bug is exploited. It is defense in depth, **not a
security boundary**: code running inside `nexusd` can still ask systemd to
start work outside the sandbox, which is exactly how operations run (D-5).

## Scripts: repository write access is code execution

A script runs whatever its file contains when it starts. A deploy script
that pulls a branch and builds or runs what it pulled executes whatever that
branch contains, as the script's run-as user. Anyone with write access to
that repository, or to that branch, therefore has code execution on the
host as that user (FR-20). Protect the branch as you would a root shell,
keep checkouts that hold scripts out of reach of other users, and choose the
least privileged run-as user that works. Before each run `nexusd` checks the
script's ownership and permissions, and those of every directory above it;
[scripts.md](scripts.md) lists the checks and the fix each refusal names.

## Audit log and its limits

Every authenticated mutating request, every denied request and every
operation state change is written to `/var/log/nexus/audit.jsonl` before the
action runs; if the record cannot be written, the action is refused. Records
carry the token's name and ID, the source address, the surface, the action,
the target, redacted parameters and the outcome, and never a token or a
value marked sensitive. Query them with `nexus auditrecord get` and a token
with `audit:read`. The log rotates by size, and records younger than
`audit.retentionDays` (default 90) are never deleted to make room.

The limits (OOS-12):

- The log is append-only by convention. It protects against API callers,
  who have no operation that changes it, and not against root on the host,
  who can edit or delete it.
- There is no remote forwarding and no cryptographic tamper evidence. Copy
  the log off the host if you need either.
- Work an operation does on the host, such as the commands a script runs, is
  recorded only as that operation's start and outcome; its output is in the
  operation log, not the audit log.

## Operation units

Long-running work (apt changes, script runs, container updates) runs in a
transient `nexus-op-<id>.service` unit outside the `nexusd` sandbox, so it
keeps running when `nexusd` restarts or crashes. The units are started with
`RefuseManualStop=yes`, so `systemctl stop` on one is refused; this is also
what keeps `needrestart`, Ubuntu's apt hook, from stopping a running script
or upgrade when it restarts services after an upgrade (it may restart
`nexusd` itself, which then picks the operations up again).

Cancel an operation through Nexus with `nexus operation cancel <id>`. An
operator on the host who must stop one without the API uses `systemctl
kill`, which sends the cancel signal to the unit's supervisor:

```
sudo systemctl kill --kill-whom=main --signal=SIGTERM nexus-op-<id>.service
```

The supervisor then stops the work as a cancel does, SIGTERM and after the
grace period SIGKILL, and the operation ends `Canceled`.

## The nexusd systemd unit

The rest of this page records the hardening of `deploy/nexusd.service`: the
directives it applies, the directives removed since the unit was created,
and every `systemd-analyze security` check the unit does not meet, with the
reason.

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
