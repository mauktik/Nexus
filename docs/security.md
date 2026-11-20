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

- **On the host, as root.** `nexusd token create`, `nexusd token list` and
  `nexusd token revoke` work on the token store directly, with no running
  `nexusd` and no existing token. They are how the first token is made and
  how a host is recovered when no remote token works any more.

  ```
  sudo nexusd token create --name laptop --scope containers:read --scope packages:read
  sudo nexusd token create --name agent --scope scripts:read --scope scripts:run:deploy-api --expires 30d
  sudo nexusd token list
  sudo nexusd token revoke agent
  ```

- **Remotely, with `nexusd:admin`.** With the `nexusd` provider enabled, a
  token holding `nexusd:admin` can also create, list and revoke tokens over
  REST or the CLI: `nexus nexusdtoken add`, `nexus nexusdtoken get` and
  `nexus nexusdtoken remove` ([install.md](install.md#manage-tokens-remotely)
  has the commands). These are the same records as the local command's,
  in the same `/var/lib/nexus/tokens.json`, and both serialize on its lock.
  Remote token management works on systemd and OpenRC hosts. It is not an
  MCP tool.
- **The one-login bootstrap.** The first `nexusd:admin` token is always
  minted on the host, as root: there is no enrollment, pairing or first-boot
  secret. One login is enough: set `providers.nexusd.enabled: true` in
  `/etc/nexus/nexusd.yaml`, reload, and run `sudo nexusd token create
  --scope nexusd:admin`. Every later token can be made remotely.
- **`sudo nexusd token` is the recovery path.** If every `nexusd:admin`
  token is lost, revoked or expired, or a configuration change locks the
  client out, log in to the host and mint a new one with `sudo nexusd token
  create`. Remote calls cannot lock root out of this path.
- **The secret is returned once.** A token is 256 random bits, printed once
  by `nexusd token create` and returned once by a remote add, in its
  response body only. It is never stored, logged, written to an operation
  record or the idempotency store, or shown again: a remote add creates no
  operation and refuses an `Idempotency-Key`, so nothing can replay it.
  `/var/lib/nexus/tokens.json` holds only its SHA-256 hash, its name,
  scopes, creation time, optional expiry and, for a remote add,
  `createdBy`: the ID and name of the token that added it. Tokens are
  compared in constant time.
- **Limits on remote changes.** Successful remote adds and removes together
  are limited to 10 per rolling minute per `nexusd`; the 11th answers 429
  `rate-limited` with `Retry-After` and changes nothing. Listing is not
  counted. A host holds at most 256 tokens, local ones included; a remote
  add past that answers 409 `token-limit-reached` with `maxTokens`. The
  local command has neither limit.
- **What a stolen `nexusd:admin` token can do.** A remote add may grant any
  scope, `nexusd:admin` included, so an admin token can be rotated without a
  login. It also means a stolen `nexusd:admin` token can mint another that
  outlives its own revocation or expiry. `createdBy` in `nexus nexusdtoken
  get` and the audit log show which token made which; after revoking a
  stolen token, revoke every token it created.
- **Names are not secrets.** A name shaped like, or containing, a token
  string (`nxs_<id>_` in any case, or a run of 43 base64url characters, the
  length of a secret) is refused without being quoted: by a remote add or
  remove and by the local `sudo nexusd token create`, with the same message.
  A remote not-found or conflict answer never repeats the name, so a secret
  pasted by mistake never reaches an error body or the audit log. A token
  already stored under such a name keeps working; list it and revoke it by
  its ID.
- A new token has no scopes unless `--scope` (local) or `--scopes`
  (remote) gives them. Revocation takes effect on the next request,
  without a restart. A remote remove of the token the request itself used
  is allowed; the answer says `revokedSelf: true` and the CLI warns that
  this client can no longer authenticate with it.
- More than `auth.failureLimitPerMinute` (default 10) authentication
  failures per minute from one source (an IPv6 /64 counts as one) answer
  `429` before any token is evaluated.
- Tokens never appear in logs, audit records, error bodies or operation
  records. The audit log records a remote add's new token ID, name, scopes
  and expiry, a remove's token ID and name and whether it was the caller's
  own, and each list, never a secret or a hash. The `nexus` CLI keeps its
  tokens in `~/.config/nexus/config.yaml` with mode 0600 and refuses a file
  that a group or others can read; it never takes a token as a
  command-line argument, and `--save-as-host` stores a new token there
  without printing it.
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
| `nginx:read` | nginx's status and configuration test, and the site list, each site and its configuration text |
| `nginx:write` | nginx start, stop, restart and reload, and site enable and disable |
| `nginx:config` | Site add and update: writing a site's configuration text |
| `pihole:read` | Pi-hole status, summary, blocking state, allow and deny entries, adlists, zones and records |
| `pihole:dns` | DNS zone and record changes, single or batched: can redirect any name for every LAN client |
| `pihole:write` | Pi-hole blocking, allow and deny entries, adlists, gravity, DNS restart and cache flush |
| `host:read` | The `host` provider's read-only overview of the host: host identity (hostname, OS, kernel, DMI vendor and model), network addresses and gateway, DNS resolvers, mount points, and process names and owners of the top processes, plus usage and metrics |
| `services:read` | The `services` provider's list of system services and each service's state, enabled state, description, unit file or script path, main PID and whether Nexus may act on it |
| `services:logs` | A service's recent log lines, from the journal or its declared OpenRC log files; logs can hold secrets, and every read is audited |
| `services:write` | Starting, stopping and restarting the services the allow-list in `/etc/nexus/nexusd.yaml` names, never a protected one: can take every allow-listed service down |
| `services:reboot` | Rebooting the host, behind its guards and only when the file on the host enables it: takes the host and every service on it down for the length of a boot |
| `storage:read` | The `storage` provider's reads: ZFS pools, datasets and snapshots, `vzdump` backup archives and Time Machine shares, and the storage health signals. Exposes dataset and snapshot names, mount points, Time Machine bundle names (usually the Macs' computer names) and backup notes |
| `storage:scrub` | Starting and stopping a scrub of an allow-listed pool: can put sustained read load on every disk of the pool, and so on the shared Proxmox storage underneath |
| `storage:snapshot` | Creating a snapshot of an allow-listed dataset: can fill a pool over time, because snapshots hold freed blocks |
| `audit:read` | Querying the audit log |
| `nexusd:read` | The running `nexusd`: version, uptime, rollback binary, whether updates are configured, and checking its configuration file |
| `nexusd:admin` | Reloading, restarting, updating and rolling back `nexusd`, and uploading a release; creating, listing and revoking tokens; reading the configuration file and setting its keys; **root-equivalent** |

The generated [API reference](api.md) lists the scope of every operation.

A request without the scope an operation needs is refused with
`forbidden` (403), whose `requiredScope` names the scope.

## Mutating scopes are potentially root-equivalent

Treat any token that holds a mutating scope for `docker`, `apt` or
`scripts` (`containers:write`, `packages:write`, `packages:install`,
`scripts:run` or any `scripts:run:<id>`), or `nginx:config`, as potentially
root-equivalent:

- Package upgrades and installs run maintainer scripts as root, from
  whatever the configured apt sources serve.
- A standalone container update recreates a container from the newest image
  its tag points to, with the container's existing privileges and mounts. A
  Compose update runs `docker compose` as root on the project's files, so it
  applies what those files say, not what the old container had; see
  [Compose projects](#compose-projects-the-files-decide-what-root-creates).
- A script runs as its run-as user with whatever that user can do, and that
  user can be root.
- `nginx:config` writes a site's configuration text, and nginx's master
  process runs as root and reads every included file at `nginx -t` and at
  reload (CON-6). With a code module such as `ngx_http_perl_module` (`perl`)
  or njs loaded by `nginx.conf`, configuration text runs code
  during `nginx -t` or the reload. Without one, a server block can still
  serve any file the worker user can read through `alias` or `root`, and
  proxy to any local service. The master also opens the files the text
  names, such as an `include`, as root while it tests, and `nginx -t`'s
  error output, which the operation returns as `testOutput`, can quote what
  it read. Nexus does not parse or sanitize directives: `nginx -t` alone
  decides what is valid. See [nginx.md](nginx.md#security).

`nginx:write` is not in this list: it starts, stops, reloads and renames
root-owned files that already exist, so it decides whether sites are served,
not what they say. It can still take every site offline. Nor are
`services:write` and `services:reboot`; see
[Services and reboot](#services-and-reboot).

`storage:scrub` and `storage:snapshot` are not in this list either. Neither
runs caller-supplied code: each runs one fixed `zpool` or `zfs` command, and
only on a pool or dataset that the root-owned configuration lists, after
the configuration enables it ([storage.md](storage.md#guarded-writes)). A
scrub can still load a pool's disks for hours, and snapshots can fill a
pool, so give these scopes only to whoever may do that.

Nexus narrows what callers can ask for: no operation accepts a command,
shell string, executable path or container specification, every parameter
is validated, and scripts are defined only in the root-owned configuration
file. That limits what a token can request, not what the requested work can
do once it runs. For nginx, the service and the include directory come
only from the configuration file and the binary is fixed at
`/usr/sbin/nginx`; a request names a site and its content, never a path.

## Services and reboot

The `services` provider ([services.md](services.md)) has four scopes, and
none of them is potentially root-equivalent: no services operation runs code
that the caller supplies, takes a path or a command, or changes which
services it may act on.

- **`services:write` is not root-equivalent**, for the same reason
  `nginx:write` is not: it starts, stops and restarts services that already
  exist, through the init system, and only those the root-owned allow-list
  `providers.services.control` names. Only an edit of the file on the host
  changes that list; remote config set refuses lists. But it can take every
  allow-listed service down, and a stop of `docker`, `containerd` or
  `pihole-FTL` takes down everything that depends on it
  ([services.md](services.md#warnings-services-that-take-others-down)).
  Allow-list only what a holder of this scope should be able to stop.
- **`services:reboot` takes the host down,** with every service on it,
  `nexusd` included, for the length of a boot. It is a scope of its own,
  which no other scope implies. The reboot is off until
  `providers.services.reboot.enabled: true` is set in the file on the host,
  and remote config set refuses to set it to `true`, so a stolen
  `nexusd:admin` token cannot switch it on ([the deny-list](#the-deny-list)).
  The caller must type the host's own name (`confirmHost`), and the reboot
  is refused while `nexusd` would not start at boot, while other operations
  are queued or running (unless `force`, which is audited), or while the
  configuration is invalid. It is not an MCP tool, so an agent that reaches
  Nexus only through MCP cannot reboot a host; keep `services:reboot` off
  agents' tokens all the same.
- **No lock-out.** `nexusd`, the unit running it and its dependencies, and
  the services a caller needs to reach or recover the host (`ssh`, `dbus`,
  networking, the gettys and the others [services.md](services.md#protected-services)
  lists) are protected: no services operation acts on them, the list is
  fixed in code, and nothing can override it. `nexus-op-*` operation units
  are protected too.
- **`services:logs` is kept apart from `services:read`,** because service
  logs can hold tokens, passwords and personal data. Every log read is
  audited with the service's name, so the audit log shows who read which
  service's logs. Log text, service names, descriptions and init output are
  host data and are marked untrusted for agents. An operation's problem
  detail and log are read with `services:read`, so on OpenRC, where
  `rc-service` runs the init script in the foreground, a failed action
  records only OpenRC's own `ERROR: NAME failed to start` line, never the
  script's output; systemctl's job message is kept whole. The state read
  that follows an action, and `get`, report a failed `rc-service status` by
  its exit code, never by the script's text.
- **What runs where.** List, get and logs run inside `nexusd`, under the
  sandbox on systemd, with fixed `systemctl` and `journalctl` argument
  lists. Start, stop, restart and the reboot run as detached operations
  outside the sandbox, as nginx's and Pi-hole's changes do. OpenRC log files
  are read only from the `output_log` and `error_log` paths the service's
  own configuration declares, never from a request, and only when the file
  has one hard link (checked again by name after the open, so a link
  removed in between does not pass; a root-only file is refused outright in a
  directory another user can write) and no component of its path is a symbolic link (read as
  root, in directories the service's user owns). Environment values,
  command lines and other `conf.d` values are never returned. The hardened
  unit is unchanged.
- **Never the hypervisor.** The provider is unavailable on a Proxmox VE
  node, and a guest's reboot is the guest's own: no Proxmox API, `pct`,
  `qm` or `pvesh`, and no hypervisor credential.

## nexusd:admin is root-equivalent

`nexusd:admin` is not potentially but plainly root-equivalent. An update
replaces the `nexusd` binary, which runs as root, and runs the release's
`install.sh` as root; a rollback puts the previous binary back, which may be
older and have known flaws; a restart takes the API down while it runs.
A remote config set rewrites the root-owned `/etc/nexus/nexusd.yaml`, and a
remote token add can mint another `nexusd:admin` token. Give `nexusd:admin`
only to whoever you would give a root shell on the host, keep it off agents,
and give it an expiry. Reading `nexusd` needs only `nexusd:read`.

No `nexusd:admin` operation is an MCP tool. Only `get_nexusd` and
`validate_nexusd_config`, both read-only, are exposed over MCP; reload,
restart, update, rollback, the upload, token add, list and remove, and
config get and set are reachable only through REST and the `nexus` CLI.

An update runs code a caller sent, which FR-25 otherwise rules out. It is
allowed because the caller does not decide what runs. The update takes only
an upload ID that the host generated, never a path, URL or command, and the
host runs nothing from the upload unless its signature verifies under a key
listed in the root-owned configuration file. A token can therefore install
only a release that the holder of a trusted signing key has signed: the key
holder and the root who listed the key decide what code may run, and the
token decides only when.

## The release signing key

`nexusd` installs only releases signed by a key it trusts.

- **Where it lives.** `make release-key` writes the Ed25519 private key to
  `~/.config/nexus/release-signing.key` on the machine that builds
  releases (or the path in `$NEXUS_RELEASE_KEY`), mode 0600, and never
  replaces an existing key. `make release` refuses a key that a group or
  others can read. The key never goes to a host, into the repository or into
  a release.
- **What is signed.** The signature is Ed25519 (RFC 8032) over a
  domain-separated message: the fixed context `nexus-release-signature-v1`
  and a NUL byte, then the SHA-256 digest of the tarball. The context keeps a
  release signature from being valid for anything else the key might sign.
  It is written beside the tarball as `<tarball>.sig`, in base64.
- **Pinned keys.** A host trusts the base64 public keys listed in
  `providers.nexusd.update.trustedKeys` in `/etc/nexus/nexusd.yaml`, and no
  others. The list changes only by editing that root-owned file: no API, MCP
  or CLI call can add or remove a key, and a remote config set refuses
  `trustedKeys` (see [the deny-list](#the-deny-list)). With no key listed, update is refused
  with `update-not-configured`.
- **`SHA256SUMS` is integrity, not authentication.** It detects a corrupted
  download, but anyone who can change a tarball can change `SHA256SUMS` with
  it. A matching digest is never accepted in place of the signature, which
  is checked before any byte of the tarball is unpacked or run.
- **Rotation.** Make a new key at another path, list both the old and the
  new public key in `trustedKeys` on every host and reload, sign releases
  with the new key, then remove the old key from every host and reload.
  [install.md](install.md#from-a-client-nexus-nexusd-update) has the
  commands.
- **A stolen key** lets anyone who also holds a `nexusd:admin` token, on any
  host that trusts it, install arbitrary code that runs as root. If the key
  is lost or may be copied, remove its public key from
  `trustedKeys` on every host and reload, then make a new key.

## Updates: uploads and the out-of-sandbox helper

- **Upload limits.** A release reaches the host as the body of
  `POST /v1/providers/nexusd/uploads`, which needs `nexusd:admin`. The body
  is streamed to `/var/lib/nexus/staging/<uploadId>/` (mode 0700), never
  held in memory whole, and a body over
  `providers.nexusd.update.maxUploadBytes` (default 64 MiB, at most 256 MiB)
  is refused with 413 `upload-too-large` and deleted. At most 2 uploads are
  staged at once, and each is deleted after an hour, or when the update that
  used it ends.
- **Package checks.** After the signature verifies, the tarball is read
  before anything is extracted: at most 1024 entries and 256 MiB of
  content, only regular files and directories under the one top-level
  `nexusd-<version>-linux-amd64/` directory, no absolute path, `..`,
  symbolic link, hard link, device node or duplicate name, and the files a
  release must hold. Every read re-hashes the file, so a file swapped after
  verification is refused. The package's `nexusd` must then report the
  version its directory names. A failure is `update-package-invalid`.
- **The helper.** Restart, update and rollback must write
  `/usr/local/bin/nexusd` and `/etc/systemd/system/nexusd.service` and
  restart `nexusd.service`, which the sandbox forbids. They run as a
  `nexus-op-<id>` unit, like other long-running operations, outside the
  sandbox and outside `nexusd.service`, as root. The unit runs the installed
  `nexusd` binary's hidden `service-helper` command, which saves the running
  binary as `nexusd.prev` and the unit file under `/var/lib/nexus/rollback/`,
  runs `install.sh` from the verified, extracted package as root, runs
  `systemctl daemon-reload`, `reset-failed` and `restart` on
  `nexusd.service` only, checks health, and restores the previous version on
  failure. It takes no command or path from the caller: its input is
  written by `nexusd` from the verified package and the configuration.
- **Audit.** Each upload, reload, restart, update and rollback is audited
  before it runs, with the upload ID, the tarball's SHA-256, the package
  version, `allowDowngrade` and the signature verdict where they apply,
  never key material. Refusals are audited with their problem type.

## Remote configuration changes: the config-set helper

`nexus nexusdconfig update KEY=VALUE...` (`nexusd.actions.update-config`,
`nexusd:admin`) writes `/etc/nexus/nexusd.yaml`, a file only root could
change before. [install.md](install.md#change-the-configuration-remotely)
describes using it; this section is its threat model.

- **Admission, inside the sandbox.** `nexusd` reads the file, applies the
  assignments in memory and runs the same checks as `nexusd
  validate-config` on the result, before any operation exists. A key that
  is unknown, on the deny-list below, a list or a mapping, a value of the
  wrong type, or an invalid result is refused with 422 `config-invalid` and
  changes nothing. At most 32 assignments are accepted per call, each value
  at most 4096 bytes. A key is a dotted path in the configuration schema,
  never a file path.
- **The helper, outside the sandbox.** The sandbox forbids writing
  `/etc/nexus`, and it is not widened: the write runs as a `nexus-op-<id>`
  unit, as root, outside the sandbox and outside `nexusd.service`, like
  update and rollback. The unit runs the installed `nexusd` binary's hidden
  `service-helper` command, whose input `nexusd` writes: the assignments
  admission validated and the SHA-256 of the file it read. The helper
  refuses with `config-conflict` if the file changed since, checks the
  edited file again with the daemon's own validation, saves the current
  file as `/etc/nexus/nexusd.yaml.bak`, replaces the file atomically
  (`root:root`, mode 0600 or the original mode if stricter), and runs
  `systemctl reload nexusd.service`. It writes nothing but the
  configuration file, its backup and their temporary files.
- **The reload is the judge.** The daemon records each reload's outcome in
  `/var/lib/nexus/reload-status.json`, and the helper waits for it to
  report the new file's digest applied. If the reload refuses the file, or
  does not answer within 12 seconds, the helper restores the backup and
  reloads again: the operation fails with `config-apply-failed` and
  `restored: true`. If that is not confirmed either, it fails with
  `config-restore-failed`, and the file on disk may not be the
  configuration in force; [install.md](install.md#recovering-from-a-failed-configuration-change)
  has the manual recovery.
- **It never restarts `nexusd`.** Keys fixed at start-up (`listen`, `tls`,
  `host`, `limits`, `auth`, `audit`, `mcp`) are written, listed in
  `restartRequired`, and applied only by a separate `nexus nexusd restart`.
- **The hardened unit is unchanged.** `deploy/nexusd.service` gained no
  writable path and lost no directive: `/etc/nexus` stays read-only to
  `nexusd` itself.
- **Audit.** Each set is audited before it runs, with every key's old and
  new value (redacted as config get redacts them), the backup path and the
  outcome. A set that changes nothing is audited too, though it creates no
  operation.

### The deny-list

Config set refuses these keys at admission, before the type rules, with
`config-invalid` naming the key and the reason. They change only by editing
the file on the host. The list is `spec.ConfigDenyList` in
`internal/providers/nexusd/spec/spec.go`.

| Key | Refused | Reason |
| --- | --- | --- |
| `providers.nexusd.update.trustedKeys` | any value | Release keys change only by editing the file, so a stolen `nexusd:admin` token cannot also trust its own signing key: an update keeps needing both a token and a trusted key |
| `providers.nexusd.enabled` | `false` only | It would disable remote administration with no remote way back |
| `providers.scripts.catalog` | the key and every key under it | The catalog defines commands run as root |
| `tls.certFile` | any value | A file path: the certificate `nexusd` serves |
| `tls.keyFile` | any value | A file path: the certificate's private key |
| `providers.docker.socket` | any value | A file path: the socket root talks to as the Docker Engine |
| `providers.docker.configDir` | any value | A directory: where root reads registry credentials |
| `providers.nginx.includeDir` | any value | A directory: where root writes nginx site files |
| `providers.nginx.includeSuffix` | any value | A file-name suffix: with `includeDir`, it decides which file root writes |
| `providers.nginx.disabledSuffix` | any value | A file-name suffix: what root renames a disabled site to |
| `providers.nginx.service` | any value | A service name: the service root starts, stops and reloads |
| `providers.pihole.dnsFile` | any value | A file path: the dnsmasq file root writes |
| `audit.retentionDays` | any value | Audit history: a stolen `nexusd:admin` token could otherwise shorten retention and, after a restart, prune the record of its own use |
| `auth.failureLimitPerMinute` | any value | Brute-force protection: a stolen `nexusd:admin` token could otherwise raise the limit on guessing tokens |
| `providers.services.reboot.enabled` | `true` only | The reboot is switched on only by editing the file on the host, so a stolen `nexusd:admin` token cannot also enable rebooting it; switching it off stays settable |

### Why path-valued keys are denied

Every key whose value names a file, a directory, a file name suffix or a
service is on the deny-list. NFR-5 and NSM FR-25 say no input carries a file
system path, command or URL; a config value that names a path would carry
one into a root-owned file, one step removed. The concrete hole it closes is
a root file write: set `providers.nginx.includeDir` to any directory, such as
`/etc/cron.d` or `/root/.ssh`, and `providers.nginx.includeSuffix` to any
suffix, then `nexus nginxsite add` writes caller-chosen text as root to a
caller-chosen file there. `providers.nginx.service` would likewise point
nginx's start, stop and reload at any other service, and
`providers.pihole.dnsFile` and `tls.keyFile` at any other file.

The only string keys that stay settable are `host.name` and `tls.mode`. A
test fails when a new string key is added to the schema without being
classified as denied or settable, so a later path-valued key is never
settable by default.

### Lists and mappings are not settable

A key is set only when its value is a scalar: a boolean, an integer, a
duration or a string. Every list-valued key is refused, and so is every
mapping (OOS-3): `listen`, `tls.extraSANs`, `mcp.allowedOrigins`,
`providers.docker.compose.trustedOwners`, `providers.nexusd.update.trustedKeys`
and `providers.scripts.catalog`. There is no way to add, remove or replace a
list element remotely. In particular, `listen` is not settable remotely:
which addresses `nexusd` listens on changes only on the host.

The settable key that can still change how a client connects is `tls.mode`.
It takes effect only at the next restart, and a set of it answers with a
warning that the client may be locked out then; `sudo nexusd token` and an
edit on the host are the way back. `auth.failureLimitPerMinute` is on the
deny-list, so the authentication limit changes only on the host.

### Config set on OpenRC

Config set needs systemd: the helper runs in a transient unit, and the
reload is confirmed through `systemctl reload nexusd.service`. On an OpenRC
host, such as Alpine, it is refused before any operation exists with 503
`config-set-unavailable`, and `nexus nexusd get` reports `configSet`
unavailable with the reason. Config get and remote token management work on
OpenRC: they run inside `nexusd`, which is unconfined there (see
[OpenRC hosts](#openrc-hosts-alpine)).

## nexusd runs as root; the sandbox is not a security boundary

`nexusd` runs as root under a hardened systemd unit (ASM-6), with no
privilege-separation helper, because Docker socket access and apt are
root-equivalent anyway: a helper would add a process without a real
boundary. The sandbox described below limits what the `nexusd` process
itself can do if a bug is exploited. It is defense in depth, **not a
security boundary**: code running inside `nexusd` can still ask systemd to
start work outside the sandbox, which is exactly how operations run (D-5).
On OpenRC hosts, such as Alpine, there is no sandbox at all: see
[OpenRC hosts](#openrc-hosts-alpine).

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

## Compose projects: the files decide what root creates

Updating a container that Docker Compose manages runs `docker compose pull`
and `docker compose up -d --no-deps` as root, outside the sandbox. Compose
reads the project's files when it runs, so whoever can write them decides
what root creates: `privileged: true`, a bind mount of `/`, another image.
Before the pull, and again before `up`, `nexusd` therefore refuses the update
unless the project is owned by root, or by a user you have chosen to trust,
with no other writers (SEC-1). It checks:

- the project's working directory and every directory above it;
- every `-f` configuration file and every `--env-file`, and the `.env` file
  in the working directory when there is one.

By default each must be owned by root, and neither group-writable nor
world-writable, unless its group is private to root. A POSIX ACL entry that
lets another user or group write also fails. Symbolic links are resolved,
and the directories they pass through are checked too. A failed check is
refused with `compose-files-missing` (HTTP 422) naming the path and the fix,
and nothing is changed. A project in an ordinary home directory, owned by
its user, is therefore refused by default: the home directory is one of the
directories above it. It works when every directory above it, the home
directory included, is root-owned (for example a root-owned directory
directly under `/home`), or when you trust the user who owns it.

### Trusting a user's Compose projects

`providers.docker.compose.trustedOwners` lists users, by name, who may own a
project's files and directories as root may. With
`trustedOwners: [deploy]`, a project in `/home/deploy/stacks/app` that
`deploy` owns updates. The list is empty by default, which keeps the
root-only rule above. Each name must be a user on the host:
`nexusd validate-config` refuses an unknown name, and `nexusd` resolves the
names to UIDs when it loads or reloads the configuration. Listing `root`
changes nothing. When the list is empty, an ownership refusal names this
key as the alternative to `chown root`.

Only ownership is relaxed. No one but the owner may write:

- each path may be owned by root or by a listed user;
- a group-writable path passes only when its group holds no one but root
  and that path's owner, as for a script's directories;
- a POSIX ACL entry that lets anyone other than root or that path's owner
  write fails, even when the entry names another listed user;
- a world-writable directory fails, sticky bit or not, so a project under
  `/tmp` or below a world-writable directory is still refused.

Trusting a user is a deliberate choice to make that user root-equivalent on
this host. Whoever can write a Compose project that root runs decides what
root creates: `privileged: true`, a bind mount of `/`, any image. Members of
the `docker` group are already root-equivalent through the Docker socket,
and the user who keeps Compose projects in their home directory usually is
one, so for such a user the option records a fact rather than granting new
power. Do not list a user who is not already trusted as root.

The trust also covers anything that can write as that user. A process or
container running under the user's UID, for example a container that runs
as that UID with the home directory bind-mounted, can edit the project and
so becomes root-equivalent too. Before you list a user, check what runs as
that user and what has their home directory mounted.

Files that a project pulls in with `include:` or `extends:`, the
`env_file:` files a service names, and files in subdirectories are not named
by the container's labels, so they are not checked on their own (SEC-7, a
known limitation). Keep them under the working directory, whose owner and
mode are checked, and do not let anyone but root or the trusted owner write
inside it. Otherwise update the project with `docker compose` by hand.

A Compose update also recreates the service from the files as they are now,
evaluated in `nexusd`'s minimal environment (`PATH`, `HOME`, `DOCKER_HOST`
and `DOCKER_CONFIG`), not the shell that first ran `docker compose up`. To
stop an unrelated edit or a blank `${VAR}` from being deployed with the
image, the update hashes the service's definition the way `docker compose
up` does, from Compose's own resolved definition (`docker compose config
--format json <service>`, with the `env_file:` files' variables merged into
its environment), and compares that with the `com.docker.compose.config-hash`
label that Compose recorded on the container. It refuses a service whose
definition was edited since `up` (CR-29), and that includes an edit to the
contents of an `env_file:` the service names. `docker compose config --hash`
is not the comparison: for a service with `env_file:` it hashes the file
entry rather than the file's variables, so it never matches the label and
does not change when the file does. It is consulted only when the resolved
definition does not match, and its agreement with the label lets the update
through. The resolved definition carries the env files' values, so it is
never written to the operation log or to an error. The refusal is
`not-updatable` (HTTP 422) and changes nothing: apply the edit with `docker
compose up -d` yourself, then update again. A container without the label,
or a definition that cannot be read, is let through, and the operation log
says so.

## Pi-hole

The `pihole` provider ([pihole.md](pihole.md)) controls the DNS resolver
the whole LAN uses. Its scopes are not root on the host through any path
Nexus offers, but they reach every client on the network, so grant them as
carefully as a mutating scope above.

- **`pihole:dns` can redirect any name for every LAN client.** A token with
  it can declare any zone, such as `github.com` or your bank's domain, and
  add a record that sends that name, and every name under it, to an address
  of its choosing. Every device that uses Pi-hole for DNS, this host
  included when it does, then gets that answer. Software that does not
  verify what it connects to (plain HTTP, update channels without
  signatures, devices that skip certificate checks) can be steered to an
  attacker's server. Every record change also interrupts DNS for the whole
  LAN for a moment while FTL restarts.
- **`pihole:write` can switch protection off and make the host fetch URLs
  as root.** A token with it can turn blocking off, allow a known-bad
  domain, block everything with a deny regex, and restart DNS at will. It
  can also add an adlist URL, which gravity (`pihole -g`, run by the token
  or by Pi-hole's weekly cron job) downloads as root from the Pi-hole host.
  That URL may point at any address the host can reach, a router's admin
  page or a service bound to the LAN, so adding an adlist is a server-side
  request forgery (SSRF) on the LAN: the response is not returned, but the
  adlist's `status` and `domains` count reveal whether the fetch worked.
  Nexus accepts only `http` and `https` URLs without userinfo, which limits
  the scheme but not the destination.
- **`pihole:read` exposes your list and record contents,** not the query
  log: no read returns per-client or per-domain query history. The values,
  URLs and comments it returns may have been written by anyone with the
  Pi-hole web interface or `pihole` command, so they are marked untrusted
  for agents ([mcp.md](mcp.md)).

### Pi-hole mutations run as root outside the sandbox

Every Pi-hole change runs as root in its own `nexus-op-<id>` unit, outside
the `nexusd` sandbox, like the other operations (see Operation units). The
unit runs the `nexusd` binary's hidden `pihole-helper` subcommand, which
reads the operation's validated input from the operation's directory and
makes the change. It runs there because the change needs what the sandbox refuses:

- the `pihole` commands write `/etc/pihole` and `/etc/dnsmasq.d`, which
  `ProtectSystem=strict` makes read-only for `nexusd`;
- `pihole restartdns` restarts the `pihole-FTL` service;
- `pihole-FTL` carries file capabilities that the unit's
  `CapabilityBoundingSet` would refuse, so `nexusd` never runs a Pi-hole
  binary itself.

`deploy/nexusd.service` is unchanged for this provider: no directive was
removed and no capability added. The helper never takes a shell string:
every value reaches a command as one argument, and no operation accepts a
dnsmasq line or option, a file path, SQL or a `pihole` argument list. The
only file it writes in `/etc/dnsmasq.d` is `providers.pihole.dnsFile`, and
the dot-prefixed temporary file it renames over it. Since the helper is
root, trust in it rests on those input checks, described next, and on the
scopes above.

### Pi-hole's scripts and SQL injection

Pi-hole v5's own scripts build SQL by pasting values between single
quotes, without escaping: `list.sh` (behind `pihole -w`, `-b`, `--regex`
and `--white-regex`) does so for domains, regexes and comments, and
`webpage.sh` (behind `pihole -a adlist`) for adlist URLs and comments.
`list.sh` does not check regexes at all, so a `'` in a regex there is SQL
injection, run as root on `gravity.db`. Nexus avoids both:

- **Allow and deny entries never go through `list.sh`.** The helper writes
  them to `gravity.db` itself, in one transaction, with the value and
  comment as bound SQL parameters, then runs
  `pihole restartdns reload-lists`. A regex can therefore contain the `\`
  and `;` that FTL's regex syntax needs. Quotes and backticks are still
  refused.
- **Adlists go through `pihole -a adlist`,** but only with values that
  cannot break its SQL: a URL must match a strict subset of what
  `webpage.sh` accepts, and a comment may hold only letters, digits, space
  and `_ . , : # -`. Neither can contain `'`, `;`, `\`, a newline or a
  control character, and neither can start with `-`.

Every input pattern is anchored and is a strict subset of what Pi-hole's
own validators accept, so a value Nexus admits is never dropped or mangled
by them.

### What Pi-hole reads touch

Reads run inside the sandbox and never run a Pi-hole command.

- `gravity.db` is opened read-only (`mode=ro` with `query_only`), so a read
  cannot change it and needs no write access to `/etc/pihole`. Pi-hole
  keeps the database in rollback-journal mode, so a read creates no
  `-journal`, `-wal` or `-shm` file.
- Reads never touch `WEBPASSWORD`. From `setupVars.conf`, which holds the
  web interface's password hash, the status read keeps only the
  `BLOCKING_ENABLED` line and discards every other line as it reads. No
  Pi-hole password, hash or API token is ever placed in a response, a log,
  an operation record or an audit record.
- Today's summary comes from FTL's local API on 127.0.0.1, which needs no
  credentials, and the DNS health check queries 127.0.0.1:53.

## What the host provider reads

The `host` provider ([host.md](host.md)) is read-only, but it runs inside
`nexusd`, which holds `CAP_DAC_READ_SEARCH` under the systemd unit and
every root capability on OpenRC, so it could read any file. Its own rules
limit it:

- **An allow-list.** Every file access goes through a fixed allow-list of
  paths under `/proc`, `/sys`, `/etc` and `/run`, each with the kinds of
  access it permits (read, list, existence only, link). An access outside
  it is refused before it reaches the file system. Mount points are never
  opened: they get only `stat` and `statfs`, and only when they come from
  the mount table. No request supplies a path, device name or command.
- **Never** `/proc/<pid>/cmdline` or `/proc/<pid>/environ`: processes are
  reported by PID, 15-character command name, owner, CPU and memory only.
- **Never** DMI serial numbers, UUIDs or asset tags: only the vendor,
  product and BIOS files of `/sys/class/dmi/id` are on the list.
- **Never** the kernel log, nor the ARP or neighbour tables.
- **No `@clock` system calls.** The unit's `SystemCallFilter` would kill
  `nexusd` on one, so clock synchronization comes from the fixed commands
  `timedatectl show --property=NTPSynchronized --value` and
  `chronyc -c tracking`. The other commands it runs are
  `systemctl list-units --state=failed` and `rc-status --crashed`. Each
  has a fixed argument list and is bounded by `sourceTimeout`.
- **Nothing is changed.** The provider writes no file, signals no process
  and changes no setting. Its metrics history stays in memory.
- **Reasons quote no content.** A field that cannot be read carries a
  reason naming a fixed path, a fixed command or a mount point, never what
  a file or a command printed.

`CAP_DAC_READ_SEARCH` is what lets it read every process's `stat` and
`status` and the root-only files on the list. On OpenRC, `nexusd` runs as
unconfined root (ASM-5), so every source is readable there and the
allow-list is the only limit.

What `host:read` exposes is listed in [Scopes](#scopes): host identity,
network addresses and gateway, DNS resolvers, mount points, and process
names and owners. Give it to an agent that should see the host's shape and
load, not to one that should not learn its layout.

## What the storage provider reads and writes

The `storage` provider ([storage.md](storage.md)) reads inside the `nexusd`
sandbox and writes outside it.

**Reads** run inside the unchanged unit, under `storage:read`:

- **Two tools only.** Every ZFS fact comes from `zpool` and `zfs`, found at
  `/usr/sbin` or `/sbin` when the provider starts. The runner refuses any
  other program before it runs anything, runs each with a fixed argument
  list, the environment `PATH=/usr/sbin:/usr/bin:/sbin:/bin` and `LC_ALL=C`
  only, and no stdin, and caps its output. Only read subcommands are run
  (`zpool list`, `zpool status`, `zpool get`, `zfs list`, `zfs version`).
  It never runs a Proxmox command (`pvesm`, `vzdump`, `qm`, `pct`,
  `pvesh`), calls no Proxmox API and holds no hypervisor credential.
- **Configured directories only.** Every file it reads is under a path of
  `providers.storage.backups.vzdumpPaths` or
  `providers.storage.timeMachine.paths`, which only the root-owned file
  sets. Each is opened as a root that a symlink cannot leave. No request
  supplies a path, a command or a property name.
- **Bounded file contents.** The only file contents read are the first
  1 KiB of each `vzdump` notes file, the last 256 KiB of each `vzdump` log
  (of which at most 10 `ERROR:` lines are returned), and each Time Machine
  bundle's `com.apple.TimeMachine.SnapshotHistory.plist`, from which only the
  completion dates are kept; a plist that does not parse gives a fixed reason
  and never quotes the file. Each file is opened without blocking, and the
  open file must be the regular file that was checked: a symlink at the
  file's own name is refused, even one that stays inside the directory, and
  so is a FIFO; a symlinked `dump/` directory is followed only inside the
  configured directory. Each directory
  it lists is opened as a directory without blocking. Everything else is
  listed or `stat`ed only.
- **Never read:** `MachineID.plist` (the Mac's host UUID, serial and model
  identifiers), the contents of band files or of anything else in a bundle,
  `keylocation` or any key material or passphrase, and any user property
  other than the provider's own `nexus:created-by`. Encryption and key status
  are reported, never acted on.
- **Untrusted text.** Names (pool, dataset, snapshot, guest, share and
  Time Machine bundle names), `zpool status` text, notes and log lines are
  host data, labelled `contentTrust: "untrusted-host-data"`. Bundle names
  also appear in health signal subjects and messages, and in `unavailable`
  reasons, which can carry the name of a failing path or a parser or tool
  message. Every reason is cut to 256 bytes, with control characters
  replaced, and never holds the contents of a file.

`storage:read` therefore exposes dataset and snapshot names, mount points,
Time Machine bundle names, which are usually the Macs' computer names, and
backup notes, which usually name the guests. Give it to an agent that may
learn what the host stores and backs up.

**Writes** run outside the sandbox. Scrub start and stop (`storage:scrub`)
and snapshot create (`storage:snapshot`) need `CAP_SYS_ADMIN` for the ZFS
ioctl, which the unit does not grant, so each runs the `nexusd` binary's
hidden `storage-helper` subcommand as root in its own `nexus-op-<id>` unit
(see [Operation units](#operation-units)). The unit was not widened. Each
write acts only when its switch is on and its pool or dataset is
allow-listed, checked at admission and again when the operation starts. The
helper re-checks every value in its input file that reaches a command: the
action, the operation ID, which must match its directory, and the pool,
dataset and snapshot names against their patterns. It finds the tools
itself (the input names no tool path) and refuses an input with an unknown
member. It does not re-read the configuration: `nexusd` checks the switch
and the allow-list at admission and when the operation starts. Like every
operation unit, the helper runs what `nexusd` hands `op-supervise`, so
these checks keep a damaged input file from reaching an argument list. They
do not contain a compromised `nexusd`. It runs only these fixed argument
lists: `zpool scrub <pool>`, `zpool scrub -s <pool>` and
`zfs snapshot -o nexus:created-by=<operation ID> [-r] <dataset>@<name>`,
plus `zpool status` and `zfs list` to confirm. The provider names every
snapshot itself. No write destroys, rolls back, clones, sends or holds
anything, and no write is an MCP tool.

## Audit log and its limits

Every authenticated mutating request, every denied request and every
operation state change is written to `/var/log/nexus/audit.jsonl` before the
action runs; if the record cannot be written, the action is refused. Records
carry the token's name and ID, the source address, the surface, the action,
the target, redacted parameters and the outcome, and never a token or a
value marked sensitive. Query them with `nexus auditrecord get` and a token
with `audit:read`. The log rotates by size, and records younger than
`audit.retentionDays` (default 90) are never deleted to make room.

Floods are recorded as aggregates rather than one record each: one record
per source and per minute, with outcome `denied`, problem `rate-limited` and
a `count`. Two kinds of flood are collected this way:

- **Rate-limited authentication.** The requests from an address refused
  with 429 after too many failed authentications. `source` is the address
  the limiter counts: an IPv4 address or an IPv6 /64.
- **One token's refusals.** Past 60 a minute, a token's denied requests
  (a missing scope) and its mutating requests refused before they were
  recorded as requested (for example `validation-failed` or
  `storage-limit-reached`). `source` is then `token:<id>`, not an address.
  These clients received the original refusal, such as 403, 422 or 507,
  not 429. Read the aggregate as "this token caused `count` more refusals",
  not as a record that its requests were rate-limited.

**nginx content (FR-15).** An nginx site add or update is audited with the
site's name and its `content` replaced by `"[REDACTED]"`, so configuration
text never reaches the audit log. The content's size in bytes and its
SHA-256 digest are in the operation log (`content: <n> bytes, sha256
<digest>`) and the operation's result (`sizeBytes`, `sha256`), not in the
audit record. The operation ID and site name link the two. Operation
retention can delete the operation, and its log, while the audit record is
kept, so copy the log first if you need the digest for longer.

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

Long-running work (apt changes, script runs, container updates, Pi-hole
and nginx changes, and `nexusd`'s own restart, update and rollback) runs in a
transient `nexus-op-<id>.service` unit outside the `nexusd` sandbox, so it
keeps running when `nexusd` restarts or crashes. The units are started with
`RefuseManualStop=yes`, so `systemctl stop` on one is refused; this is also
what keeps `needrestart`, Ubuntu's apt hook, from stopping a running script
or upgrade when it restarts services after an upgrade (it may restart
`nexusd` itself, which then picks the operations up again).

Cancel an operation through Nexus with `nexus operation cancel <id>`. An
operator on the host who must stop one without the API uses `systemctl
kill` to send `SIGINT`, the operator's cancel signal, to the unit's
supervisor:

```
sudo systemctl kill --kill-whom=main --signal=SIGINT nexus-op-<id>.service
```

The supervisor records the cancel request in
`/var/lib/nexus/ops/<id>/cancel`, then stops the work as a cancel does,
SIGTERM to the work's process group and after the grace period SIGKILL, and
the operation ends `Canceled`.

Do not use `SIGTERM` for this. The supervisor takes a `SIGTERM` as a cancel
only when Nexus asked for one: `nexus operation cancel` records the request
before it sends the signal. A `SIGTERM` without that request is what a host
shutdown sends, systemd to each unit and init to every process. The
supervisor still stops the work, but reports it interrupted, and the
operation ends `Failed` with `outcome-unknown`, as an operation running at a
shutdown or reboot does: its work was cut short, and Nexus cannot know what
it had done. Neither systemd nor BusyBox init nor OpenRC sends `SIGINT` at
shutdown, so a `SIGINT` is always an operator's cancel.

**The documented exception: `CompletesAcrossReboot`.** An operation that
declares `CompletesAcrossReboot` is decided in the boot after the one it
started in, not ended `outcome-unknown`. The `services` provider's reboot
is the only such operation. Just before it triggers the reboot, its helper
durably records the boot ID in `/var/lib/nexus/ops/<id>/reboot-requested`.
When the operation's process is ended by the shutdown, `nexusd` holds the
operation `Running` instead of failing it; in the next boot, recovery finds
the record with a different boot ID and ends the operation `Succeeded`
with both boot IDs. Without a record, such an operation ends as any other
does. The boot ID is read only when both mount tables, `nexusd`'s and
PID 1's, can be read, by `nexusd`, the operation manager, the supervisors and
the helper alike. A boot ID read past an unreadable table could be the
kernel's in one process and the container's in another, and the difference
would pass for a reboot, so such a read is an error: the reboot is
unavailable, the helper sends nothing, and an operation recovered without the
boot ID ends `outcome-unknown`. A record that cannot be read ends it
`outcome-unknown` at once, and an
operation held without a reboot ends `outcome-unknown` at its maximum run
time ([services.md](services.md#recovery-after-the-reboot)). A cancel is
refused with `not-cancelable` once the record exists, because the reboot has
been requested.

## The nexusd systemd unit

The next sections record the hardening of `deploy/nexusd.service`: the
directives it applies, the directives removed since the unit was created,
and every `systemd-analyze security` check the unit does not meet, with the
reason. [OpenRC hosts](#openrc-hosts-alpine) records what each directive
becomes on Alpine, where there is no unit.

`nexusd` runs as root under this unit, because Docker socket access and apt
are root-equivalent anyway. The privileged work (apt changes, script runs,
container updates, Pi-hole and nginx changes, ZFS scrub start and stop and snapshot
create, and restarting, updating and rolling back `nexusd`) runs in `nexus-op-*` transient units that `nexusd` starts with
`systemd-run`. Those units are outside this sandbox. The sandbox limits what
the `nexusd` process itself can do if it is compromised. It is not a boundary
between a token holder and root: a token with a mutating `docker`, `apt` or
`scripts` scope, or `nginx:config`, is potentially root-equivalent, and `nexusd:admin` is
root-equivalent. Self-update did not relax any directive: everything
`nexusd` writes for it inside the sandbox (staged uploads, the extracted
package, the helper's input) is under `/var/lib/nexus`.

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

None. The unit applies every directive of the WI 3.5 set, and the `pihole`
provider added none and removed none. A directive removed
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
- `ProtectHome=`: Read-only, not inaccessible: a script can live in its run-as user's own home directory, and `nexusd`'s integrity checks read it there. A Compose project under `/home` is read the same way, but only a project whose every directory above it is root-owned is admitted, so a project in a home directory owned by its user is refused (see Compose projects).
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

## OpenRC hosts (Alpine)

On an OpenRC host, such as Alpine, `nexusd` runs as **unconfined root**
under `supervise-daemon`, from the service script `/etc/init.d/nexusd`
(`deploy/nexusd.openrc`). None of the sandbox above applies there
(ASM-19).

The reason is where the work runs (CON-9). Under systemd, privileged work
leaves the `nexusd` sandbox because PID 1 starts each `nexus-op-*` unit
fresh. OpenRC has no transient units: `nexusd` starts each operation's
supervisor itself, so the supervisor and the work under it inherit
whatever confines `nexusd`. Apt-equivalent work, Compose updates and
scripts need full root, so any confinement of `nexusd` would break them,
unless a privileged broker ran outside it, which this release does not
add (base ASM-6).

The sandbox was never a security boundary, on either platform: a token
with a mutating scope is potentially root-equivalent on Ubuntu too. The
threat model and the scopes are therefore unchanged on OpenRC. Only
defense in depth is reduced: a bug exploited inside `nexusd` reaches
everything root can do, without the limits the unit puts on the process
itself.

Every directive of `deploy/nexusd.service` is **replaced** (with what),
**lost** (for the reason above), or **not applicable** on OpenRC.
`deploy/nexusd.service` itself is unchanged.

| Directive | OpenRC | How, or what is lost |
| --- | --- | --- |
| `Description` | replaced | The script's `description` |
| `Documentation` | not applicable | OpenRC scripts have no documentation link |
| `After` | replaced | `depend()`: `need net` and `after firewall` |
| `Wants` | replaced | `depend()`: `need net` |
| `Type` | replaced | Pidfile readiness: `supervise-daemon` writes the pidfile `/run/nexusd.pid`, which names `supervise-daemon` itself; `nexusd`'s own PID is in `/run/openrc/options/nexusd/child_pid`. A start returns once the process is running, not once every listener is open |
| `ExecStart` | replaced | The script's `command` and `command_args`: the same binary, arguments and configuration file |
| `ExecReload` | replaced | The script's `reload`, which sends `SIGHUP` through `supervise-daemon` |
| `Restart` | replaced | `supervise-daemon` respawn: a `nexusd` that exits is started again after 2 s, at most 5 times in 60 s. A stop sends `SIGTERM` and waits up to 15 s, then `SIGKILL` |
| `StateDirectory` | replaced | `checkpath` in `start_pre` creates `/var/lib/nexus`, owned by root |
| `StateDirectoryMode` | replaced | `checkpath --mode 0700`, plus the explicit mode of every file `nexusd` creates |
| `LogsDirectory` | replaced | `checkpath` in `start_pre` creates `/var/log/nexus`, owned by root |
| `LogsDirectoryMode` | replaced | `checkpath --mode 0700`, plus the explicit mode of the audit log |
| `NoNewPrivileges` | lost | Set-user-ID and file-capability gains are possible |
| `ProtectSystem` | lost | The whole file system is writable |
| `ProtectHome` | lost | Home directories are writable |
| `PrivateTmp` | lost | `/tmp` and `/var/tmp` are the host's |
| `ProtectKernelTunables` | lost | `/proc/sys` and `/sys` are writable |
| `ProtectKernelModules` | lost | Kernel modules can be loaded |
| `ProtectKernelLogs` | lost | The kernel log is readable |
| `ProtectControlGroups` | lost | The cgroup hierarchy is writable; `nexusd` uses this to create `nexus-ops/<id>` |
| `ProtectClock` | lost | The clock can be set |
| `ProtectHostname` | lost | The hostname can be changed |
| `RestrictNamespaces` | lost | New namespaces can be created |
| `RestrictRealtime` | lost | Realtime scheduling is allowed |
| `RestrictSUIDSGID` | lost | Set-user-ID and set-group-ID files can be created |
| `LockPersonality` | lost | The execution domain can be changed |
| `MemoryDenyWriteExecute` | lost | Writable and executable memory is allowed |
| `SystemCallArchitectures` | lost | Non-native system call ABIs are allowed |
| `SystemCallFilter` | lost | No system call filter |
| `RestrictAddressFamilies` | lost | Every socket family is allowed |
| `CapabilityBoundingSet` | lost | `nexusd` keeps every root capability, not only `CAP_DAC_READ_SEARCH` |
| `WantedBy` | replaced | `rc-update add nexusd default`, which the operator runs: the installer never adds `nexusd` to a runlevel |
| `RefuseManualStop` (set on `nexus-op-*` units, not in this unit) | not applicable | There are no transient units, and Alpine has no `needrestart` hook that stops services after an upgrade (ASM-18) |

**Umask.** The unit has no `UMask=` line, so on systemd `nexusd` runs with
systemd's default umask `0022`. On OpenRC, the script sets `umask=077`,
which is stricter and applies to `nexusd` only. Every file and directory
Nexus itself creates has an explicit mode, such as `0600` or `0700`, so the
difference changes no Nexus file. Detached work (scripts, Compose updates,
apt changes, the nginx helper, and an nginx that Nexus starts or restarts)
runs with umask `0022` on both platforms: on systemd because `nexus-op-*`
units get systemd's default, and on OpenRC because the operation supervisor
sets `0022` before it starts the work.

**Where operations run.** Each operation runs under a detached supervisor
process (`op-supervise`) rather than a `nexus-op-*` unit. When the cgroup v2
hierarchy is mounted and writable, `nexusd` starts each supervisor in its own
cgroup, `nexus-ops/<id>`, outside `nexusd`'s OpenRC cgroup
(`openrc.nexusd`). For a script run or a Compose update, cancel and timeout
kill every process in that cgroup. Without
writable cgroup v2, supervisors run in process groups only: cancel and
timeout kill the work's process group, and a descendant that calls `setsid`
itself escapes (CON-10). The operation log says so.

**`rc_cgroup_cleanup`.** The script defaults it to `NO`, so OpenRC does not
kill what is left in `openrc.nexusd` when `nexusd` stops, and running
operations survive `rc-service nexusd stop` and `rc-service nexusd restart`
(ASM-14). An operator can set `rc_cgroup_cleanup="YES"` in
`/etc/conf.d/nexusd`:

- With writable cgroup v2, `YES` kills only what is left in
  `openrc.nexusd`. The supervisors are in `nexus-ops/<id>` and survive.
- Without writable cgroup v2, the supervisors stay in `openrc.nexusd`, so
  `YES` kills every running supervisor whenever `nexusd` stops or restarts,
  and those operations end `outcome-unknown` (CON-10). On such hosts, leave
  `rc_cgroup_cleanup` at the script's default, `NO`.
- On stock Alpine 3.21 (OpenRC 0.55.1), `YES` has no effect at all: OpenRC's
  `rc-cgroup.sh` looks for the service's cgroup at `/sys/fs/cgroup/nexusd`
  instead of `/sys/fs/cgroup/openrc.nexusd`, finds nothing, and kills
  nothing. OpenRC fixed this upstream (commit `1cf3944`, issue #910), so
  check the host's OpenRC version (`openrc --version`) before relying on
  either behavior.

**Stopping an operation by hand.** Cancel through Nexus with
`nexus operation cancel <id>`. On the host, the supervisor's PID is in
`/var/lib/nexus/ops/<id>/supervisor.json`. Check that
`/proc/<pid>/cmdline` contains `op-supervise`, then send it `SIGINT`
(`kill -INT <pid>`): it records the cancel request, stops the work as a
cancel does, and the operation ends `Canceled`. A `SIGTERM` (`kill <pid>`)
also stops the work, but without a cancel request from Nexus it ends the
operation `outcome-unknown`, as at a shutdown (see
[Operation units](#operation-units)). A supervisor killed with `SIGKILL`
leaves its operation `outcome-unknown`. Its diagnostics are in
`/var/lib/nexus/ops/<id>/supervisor.log`, not the journal.
