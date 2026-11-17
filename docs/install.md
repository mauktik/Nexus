# Installing Nexus

Nexus has two parts: `nexusd`, the server, on each Ubuntu or Alpine host
you manage, and `nexus`, the command-line client, on your Mac or Linux
machine. Each
host runs its own `nexusd`; the client talks to one or many of them.

Read [security.md](security.md) first: Nexus is for the home LAN only.

## Requirements

- **Host:** the latest Ubuntu on x86-64 (amd64), with systemd, or Alpine
  3.21 or later on x86-64, with OpenRC (see
  [Install nexusd on Alpine](#install-nexusd-on-alpine-openrc)). No compiler
  or language runtime is needed. There is no arm64 build of `nexusd`.
- **For the `docker` provider:** Docker Engine with Compose v2, reachable
  through its local Unix socket (`/run/docker.sock` by default).
- **For the `apt` provider:** apt and dpkg, as Ubuntu ships them. It is
  unavailable on Alpine.
- **For the `scripts` provider:** systemd or OpenRC, and the scripts you
  will catalog.
- **For the `nginx` provider:** nginx from the distribution's package
  (`/usr/sbin/nginx`), its init service, and an `include` line for the
  include directory in `nginx.conf` (see [nginx](nginx.md)). Disabled by
  default.
- **Client:** macOS on Apple silicon (darwin/arm64) or Linux on amd64.

## Install nexusd on a host

These steps are for Ubuntu. For Alpine, see
[Install nexusd on Alpine](#install-nexusd-on-alpine-openrc).

1. Download `nexusd-<version>-linux-amd64.tar.gz` and `SHA256SUMS` from the
   release, check the tarball and unpack it:

   ```
   sha256sum --ignore-missing -c SHA256SUMS
   tar -xzf nexusd-<version>-linux-amd64.tar.gz
   cd nexusd-<version>-linux-amd64
   ```

2. Run the installer as root:

   ```
   sudo ./install.sh
   ```

   It installs `/usr/local/bin/nexusd` and the hardened unit
   `/etc/systemd/system/nexusd.service`. On a first install it writes the
   default configuration to `/etc/nexus/nexusd.yaml`, owned by `root:root`
   with mode 0600, listening on `127.0.0.1:7443` only and with every
   provider disabled. It never changes an existing configuration, and it
   does not enable or start the service on a first install. It finishes by
   checking the configuration with `nexusd validate-config`.

3. Edit `/etc/nexus/nexusd.yaml`. Every key is listed there with its
   default, and an unknown key is an error.

   - **Providers:** set `enabled: true` for each provider you want, under
     `providers`. Catalog scripts as [scripts.md](scripts.md) describes.
   - **Compose projects in a home directory:** a Compose update refuses a
     project that anyone but root owns, which includes one under
     `/home/<user>`. To update such projects, list their owner under
     `providers.docker.compose.trustedOwners`, for example
     `compose: {trustedOwners: [deploy]}` in the `docker` section. The user
     must exist on the host. This trusts that user as root: read
     [security.md](security.md#trusting-a-users-compose-projects) first.
   - **LAN access:** to reach the host from other machines, set `listen` to
     its LAN address, for example `listen: ["192.168.1.10:7443"]`, and keep
     `tls.mode: auto`. To connect by a DNS name such as `nas.lan`, add it to
     `tls.extraSANs` now: the certificate is generated on the first start
     and then kept, because clients pin it. Do not expose the port beyond
     the LAN (see [security.md](security.md)).
   - **On the host only:** the default `listen: ["127.0.0.1:7443"]` needs
     nothing else. `tls.mode: off` (plain HTTP) is accepted only when every
     listen address is a loopback IP: write `127.0.0.1` or `[::1]`, never
     `localhost`, which `nexusd` refuses for plain HTTP.

   The file stays `root:root` 0600: running as root, `nexusd` refuses a
   configuration that is not owned by root or that a group or others can
   read, because script environment values can be secrets. Check it:

   ```
   sudo nexusd validate-config
   ```

4. Start the service and note the certificate's fingerprint:

   ```
   sudo systemctl enable --now nexusd.service
   sudo nexusd tls fingerprint
   ```

   `nexusd` generates its self-signed certificate on the first start and
   logs its SHA-256 fingerprint (`journalctl -u nexusd`); `nexusd tls
   fingerprint` prints it again at any time.

5. Create a token for each person and agent, with only the scopes it needs
   (the scopes are listed in [security.md](security.md)):

   ```
   sudo nexusd token create --name laptop --scope containers:read --scope containers:write --scope packages:read
   ```

   The token is printed once. Copy it now; it is never shown again.
   `sudo nexusd token list` lists tokens without their secrets, and `sudo
   nexusd token revoke NAME` revokes one at once. `--expires 30d` limits a
   token's lifetime. To make later tokens from a client instead, without
   logging in again, see [Manage tokens remotely](#manage-tokens-remotely).

## Install nexusd on Alpine (OpenRC)

On Alpine, `nexusd` runs as an OpenRC service under `supervise-daemon`, as
unconfined root: none of the systemd unit's sandbox applies. Read
[OpenRC hosts](security.md#openrc-hosts-alpine) first. The steps below are
run as root; Alpine has no `sudo` by default.

1. Install what the providers you want need, from Alpine's repositories:

   ```
   apk add docker docker-cli-compose   # the docker provider
   rc-update add docker default
   rc-service docker start
   apk add nginx                       # the nginx provider
   ```

   The `apt` provider is unavailable on Alpine, because apt and dpkg are
   absent: leave it disabled (`nexus capability get` then reports it
   `disabled`; enabled, it would report `unavailable`). Alpine's own apk packages are not managed by Nexus: there is
   no apk provider yet (ASM-20). The `docker` and `scripts` providers work
   as on Ubuntu.

2. Download, check and unpack the same `nexusd-<version>-linux-amd64.tar.gz`
   as for Ubuntu. BusyBox `sha256sum` has no `--ignore-missing`, so check
   the tarball's own line:

   ```
   grep ' nexusd-<version>-linux-amd64.tar.gz$' SHA256SUMS | sha256sum -c -
   tar -xzf nexusd-<version>-linux-amd64.tar.gz
   cd nexusd-<version>-linux-amd64
   ./install.sh
   ```

   `install.sh` finds OpenRC and installs `/usr/local/bin/nexusd` and the
   service script `/etc/init.d/nexusd`. The configuration is handled as on
   Ubuntu: written only on a first install, `root:root` 0600, and checked
   with `nexusd validate-config`. It never adds `nexusd` to a runlevel.

3. Edit `/etc/nexus/nexusd.yaml` as in step 3 for Ubuntu, without the `apt`
   provider.

4. Start `nexusd` at boot and now, and note the certificate's fingerprint:

   ```
   rc-update add nexusd default
   rc-service nexusd start
   nexusd tls fingerprint
   ```

   `rc-service nexusd start` returns once `nexusd` is running, not once it
   is listening; a configuration error shows in the log below.

5. Create tokens as in step 5 for Ubuntu.

**Logs.** `nexusd` logs to syslog, tagged `nexusd`: its standard output at
`daemon.info` and its standard error at `daemon.err`. Alpine's BusyBox
`syslogd` (the `syslog` service) writes them to `/var/log/messages` by
default: `grep nexusd /var/log/messages`. The audit log is
`/var/log/nexus/audit.jsonl`, as on Ubuntu.

**Operate.** `rc-service nexusd reload` re-reads the configuration and
`rc-service nexusd restart` restarts `nexusd`. Operations run in detached
supervisor processes, not `nexus-op-*` units, and keep running across a
restart, a stop and an upgrade. A supervisor's diagnostics are in
`/var/lib/nexus/ops/<id>/supervisor.log`. Cancel an operation with
`nexus operation cancel <id>`; to cancel one on the host without the API,
send its supervisor `SIGINT`: `kill -INT <pid>`, with the `pid` from
`/var/lib/nexus/ops/<id>/supervisor.json`, after checking that
`/proc/<pid>/cmdline` contains `op-supervise`. The operation ends
`Canceled`. A `SIGTERM` is not a cancel: it ends the operation
`outcome-unknown` (see [OpenRC hosts](security.md#openrc-hosts-alpine)).

**`/etc/conf.d/nexusd`.** OpenRC reads this file before the service script,
and the installer does not create it. Do not set `rc_cgroup_cleanup="YES"`
there on a host without writable cgroup v2: operation supervisors then stay
in `nexusd`'s OpenRC cgroup, so `YES` kills every running supervisor when
`nexusd` stops or restarts, and those operations end `outcome-unknown`
(CON-10). The script's default is `NO`. With writable cgroup v2 (the default
on Alpine 3.21), supervisors are in their own `nexus-ops/<id>` cgroups and
survive `YES`; on stock Alpine 3.21, `YES` has no effect at all because of
an OpenRC 0.55.1 bug ([details](security.md#openrc-hosts-alpine)).

**Upgrade and uninstall.** Run `./install.sh` from the newer release
directory: it restarts a running `nexusd` onto the new binary with
`rc-service nexusd restart`, and running operations continue. `./uninstall.sh`
stops `nexusd`, removes it from every runlevel, and warns about operation
supervisors still running, which it leaves to finish. `--purge` also
deletes `/etc/conf.d/nexusd`, with `/etc/nexus`, `/var/lib/nexus` and
`/var/log/nexus`.

## Install from a checkout (deploy.sh)

Instead of a release tarball, you can build and install `nexusd` from a
clone of the repository on the host itself. This needs Go 1.21 or newer
(`sudo apt install golang-go`, or `sudo snap install go --classic`). Go then
downloads the exact toolchain that `go.mod` pins.

```
git clone https://github.com/mauktik/Nexus.git
cd Nexus
./deploy.sh
```

Run it as your normal user, not with `sudo`. It builds `nexusd` as you,
stages the same files the release tarball carries, and runs
`deploy/install.sh` with `sudo`, so it asks for your password once. Steps 3
to 5 above then apply as written.

To upgrade, run `git pull && ./deploy.sh` again. Like `install.sh`, it keeps
the configuration, tokens, certificate and history, and restarts a running
`nexusd`. `./deploy.sh --uninstall` removes it again, and `--uninstall
--purge` also deletes its configuration and state. `./deploy.sh --stage-only
DIR` only builds and stages the tree for linux/amd64, on any machine with
Go.

## Install the nexus client

1. Download `nexus-<version>-darwin-arm64.tar.gz` (Mac) or
   `nexus-<version>-linux-amd64.tar.gz` (Linux) and `SHA256SUMS`, check the
   tarball against its line in `SHA256SUMS` (`shasum -a 256` on a Mac), and
   unpack it. Put the `nexus` binary on your `PATH`, for example in
   `/usr/local/bin`.

2. Register each host. `nexus host add` connects, shows the certificate's
   fingerprint, asks you to confirm it against what `sudo nexusd tls
   fingerprint` printed on the host, then asks for the token without echoing
   it:

   ```
   nexus host add nas --url https://192.168.1.10:7443
   ```

   Without a terminal, pass both instead:

   ```
   nexus host add nas --url https://192.168.1.10:7443 --fingerprint <fingerprint> --token-stdin < token.txt
   ```

   On the host itself with `tls.mode: off`, use
   `--url http://127.0.0.1:7443`. The client accepts `http://` only for a
   loopback IP and refuses `http://localhost`.

   The client keeps hosts, pinned fingerprints and tokens in
   `~/.config/nexus/config.yaml` (or `$XDG_CONFIG_HOME/nexus/config.yaml`),
   mode 0600. It refuses a file that a group or others can read, and it
   never takes a token as an argument. `--default` makes a host the default;
   `--tag` tags it for `--tag` selection.

3. Try it:

   ```
   nexus capability get
   nexus container get
   nexus command get
   ```

   `--host nas,web` or `--host all` targets several hosts at once. The
   [CLI reference](cli/nexus.md) lists every command.

For agents, see [mcp.md](mcp.md).

## Change the configuration

`sudo systemctl reload nexusd` re-reads `/etc/nexus/nexusd.yaml`: provider
settings and the script catalog change at once. An invalid file is logged
and the running configuration stays. Listen addresses and TLS settings are
fixed at start-up; change them with `sudo systemctl restart nexusd`.
Operations that run in their own `nexus-op-*` units (apt changes, script
runs, container updates) keep running across a restart.

With the `nexusd` provider enabled, the same can be done from a client:
`nexus nexusd validate-config` runs the checks of `sudo nexusd
validate-config` on the file as it is on disk, without applying it, and
`nexus nexusd reload` reloads it as `systemctl reload` does, but answers
`config-invalid` with the errors instead of only logging them, and lists the
changed keys that take effect only after a restart. On a systemd host a
client can also set scalar keys of the file, as
[Change the configuration remotely](#change-the-configuration-remotely)
describes; lists, the script catalog, release keys and every key whose
value is a path or a service still change only by editing the file on the
host.

## Manage tokens remotely

Once a host has one `nexusd:admin` token, tokens can be added, listed and
revoked from a client, on systemd and OpenRC hosts alike. They are the same
tokens as `sudo nexusd token` manages, in the same store.

**Bootstrap: one login per host.** The first `nexusd:admin` token is always
made on the host, as root: there is no remote enrollment. On the host,
enable the `nexusd` provider in `/etc/nexus/nexusd.yaml`:

```yaml
providers:
  nexusd:
    enabled: true
```

then check, reload and mint the admin token:

```
sudo nexusd validate-config
sudo systemctl reload nexusd          # sudo rc-service nexusd reload on Alpine
sudo nexusd token create --name admin-laptop --scope nexusd:admin --expires 90d
```

`install.sh` prints the `nexusd token create` command at the end of an
install as a reminder; it never creates a token itself. On the client,
register the host with that token (`nexus host add`, see
[Install the nexus client](#install-the-nexus-client)). Everything below
then works without logging in to the host again. `nexusd:admin` is
root-equivalent: read [security.md](security.md#nexusdadmin-is-root-equivalent)
before handing one out.

**List.** `nexus nexusdtoken get` lists every token of the host, local ones
included: name, ID, scopes, creation time, expiry, `createdBy` (the token
that added it remotely) and `self`, true for the token you are using. It
never shows a secret or a hash.

```
nexus nexusdtoken get --host nas
nexus nexusdtoken get --host all --select name,properties.createdBy
```

**Add.** `nexus nexusdtoken add NAME` creates a token with the scopes given
by `--scopes` (none by default) and an optional `--expires` (a Go duration
such as `1h30m`, or days such as `90d`; never by default):

```
nexus nexusdtoken add agent --host nas --scopes containers:read --scopes scripts:run:deploy-api --expires 30d
```

The secret is printed once, on stdout, and never again: it is not stored on
the host, and an add creates no operation and takes no idempotency key, so
nothing can replay it. With `--host a,b` each host gets its own token, each
printed with its host. To keep the secret off the terminal, `--save-as-host
NAME` stores it in `~/.config/nexus/config.yaml` as host `NAME`, with the
target's URL and pinned fingerprint, and prints the result without it. It
takes exactly one target, and `NAME` must be a new host or the same host at
the same URL, which rotates that host's token:

```
nexus nexusdtoken add laptop-2 --host nas --scopes nexusd:admin --expires 90d --save-as-host nas
```

**Remove.** `nexus nexusdtoken remove NAME` revokes a token by name or
token ID, after asking for confirmation (`--yes` skips it, and is required
without a terminal). It is refused from the next request.

```
nexus nexusdtoken remove agent --host nas
```

Removing the token you are using is allowed: the result says `revokedSelf`
true, and the CLI warns on stderr that this client can no longer
authenticate to that host with it. Run `nexus nexusdtoken get` first and
check `self` to avoid it. A host's configuration change can also leave a
client locked out (see the lock-out warning below). Either way, the way
back is one login: `sudo nexusd token create` on the host still works with
no running daemon and no existing token.

**Limits.**

- Successful adds and removes together are limited to 10 per minute per
  host. The 11th answers 429 `rate-limited`, with a `Retry-After` header
  saying when to retry, and changes nothing.
- A host holds at most 256 tokens, local ones included. An add past that
  answers 409 `token-limit-reached`; remove unused tokens first. `sudo
  nexusd token create` is not limited.
- A name already taken answers 409 `conflict`, and an unknown name or ID
  answers 404 `not-found`.
- A name that looks like, or contains, a token string (`nxs_<id>_` in any
  case, or a run of 43 letters, digits, `_` or `-`) is refused without being
  echoed, and a not-found or conflict answer never repeats the name, so a
  secret pasted by mistake is never sent on in an error or the audit log.
  Pass the token's name or ID instead.

Every add, list and remove is audited: an add with the new token's name, ID,
scopes and expiry, a remove with the token's name and ID and whether it was
the caller's own. No audit record holds a secret.

## Change the configuration remotely

`nexus nexusdconfig get` reads `/etc/nexus/nexusd.yaml` and `nexus
nexusdconfig update` sets keys in it, from a client, with a `nexusd:admin`
token. Neither is an MCP tool.

**Read.** Without a key, `get` lists every key of the schema by dotted name;
with one, it returns that key, or every key of a section:

```
nexus nexusdconfig get --host nas
nexus nexusdconfig get providers.host --host nas
nexus nexusdconfig get audit.retentionDays --host nas
```

Each key shows its `value` as written in the file (null when absent), its
`effective` value (the default when absent) and its `source`, `file` or
`default`. Values come from the file on disk, which can differ from the
configuration in force until a reload or restart. Secrets, such as a
script's `env` values, are shown as `[REDACTED]`. Config get works on
systemd and OpenRC hosts.

**Set.** `update` takes one or more `KEY=VALUE` assignments, up to 32, and
applies them together or not at all:

```
nexus nexusdconfig update providers.host.enabled=true --host nas
nexus nexusdconfig update audit.retentionDays=30 providers.host.sampleInterval=5s --host nas,web
```

- **Keys** are dotted paths of the schema, as `nexusdconfig get` lists them.
  A value is parsed by the key's type: `true` or `false`, an integer, a
  duration such as `90s` or `1h`, or a string of at most 4096 bytes.
  Assigning a key the file does not have adds it, with any missing parent
  section. There is no unset: set a key to its default value instead.
- **Lists and mappings cannot be set:** `listen`, `tls.extraSANs`,
  `mcp.allowedOrigins`, `providers.docker.compose.trustedOwners`,
  `providers.nexusd.update.trustedKeys` and the script catalog, and whole
  sections such as `providers.host`. Edit them on the host.
- **Denied keys:** `providers.nexusd.update.trustedKeys`,
  `providers.nexusd.enabled=false`, anything under
  `providers.scripts.catalog`, and every key whose value names a file,
  directory or service: `tls.certFile`, `tls.keyFile`,
  `providers.docker.socket`, `providers.docker.configDir`,
  `providers.nginx.includeDir`, `providers.nginx.includeSuffix`,
  `providers.nginx.disabledSuffix`, `providers.nginx.service` and
  `providers.pihole.dnsFile`. [security.md](security.md#the-deny-list)
  gives the reason for each.

Before anything runs, the host checks the edited file exactly as `nexusd
validate-config` would. An unknown key (the error names the nearest valid
parent), a denied key, a list or mapping, a value of the wrong
type, or an edited file that would be invalid answers 422 `config-invalid`
with every error located by key, and nothing changes. A host on OpenRC
answers 503 `config-set-unavailable`: config set needs systemd, and `nexus
nexusd get` shows `configSet` unavailable there.

**No change.** When every assigned key is already in the file with an equal
value (durations compared as durations, so `60s` equals `1m`), the call
answers at once with `changed: false`: it creates no operation, writes
nothing and reloads nothing. An assignment to a key the file does not have
is a change, even when the value equals the default.

**A change** runs as an operation in its own `nexus-op-<id>` unit, as root,
and the CLI waits for it:

1. The current file is saved as `/etc/nexus/nexusd.yaml.bak`, mode 0600.
   There is one backup: each change overwrites it, and it is kept after
   success, so `sudo diff /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml`
   shows the last change.
2. The edited file replaces `/etc/nexus/nexusd.yaml` atomically, owned by
   root with mode 0600 (or the file's own mode if stricter).
3. `nexusd` reloads it, as `sudo systemctl reload nexusd` does.

The result has `changed`, each key's old and new value in `changes`
(redacted as `get` redacts them), `backupPath` and `restartRequired`. If the
file changed on the host since the request was checked, it fails with
`config-conflict` and nothing is written.

**It never restarts `nexusd`.** Some settings are fixed at start-up:
`listen`, `tls`, `host`, `limits`, `auth`, `audit` and `mcp`. A change to one
is written and reloaded, but takes effect only at the next restart;
`restartRequired` names those sections and the CLI prints `run nexus nexusd
restart to apply: ...` on stderr. Restart when ready:

```
nexus nexusd restart --host nas
```

**Lock-out warning.** `tls.mode` and `auth.failureLimitPerMinute` change how
clients connect once `nexusd` restarts. Switching `tls.mode` between `auto`
and `files` changes the certificate `nexusd` serves, which then no longer
matches the fingerprint each client pinned; `off` is accepted only when
every listen address is loopback, which leaves only clients on the host
itself. A lower `auth.failureLimitPerMinute` rate-limits a client with a bad
token sooner. For each changed `tls` or `auth` key the CLI prints a warning
on stderr that this client may be locked out after the restart. If that happens, log in to the host, fix
the file (`/etc/nexus/nexusd.yaml.bak` holds the previous version) and
restart; `sudo nexusd token create` makes a new token if one is needed.

**If the reload refuses the new file,** or does not confirm it within 12
seconds, the backup is put back and reloaded: the operation fails with
`config-apply-failed` and `restored: true`, and the configuration in force
is the one from before the change. If even that is not confirmed, it fails
with `config-restore-failed`: follow
[Recovering from a failed configuration change](#recovering-from-a-failed-configuration-change).

**Comments and layout are kept.** Setting a key the file already has
replaces its value in place, so the change is that one line. Adding a key
re-encodes the whole file instead, and this normalization happens only when
a key is added: blank lines are dropped, the spacing before line comments is collapsed to one space,
and indentation becomes 2 spaces. Every comment stays attached to its key,
every other key keeps its value and key order is unchanged; the new key
goes last in its section. The shipped default file lists every key, so on
an installed host a set is normally a one-line change.

**Several hosts.** `--host a,b` or `--host all` asks for confirmation, then
changes each host on its own, with a result per host. There is no rollback
across hosts: a host that refused keeps its file while the others change.

Config set takes turns with reload, restart, update and rollback: while one
of them is queued or running on a host, another answers 409 naming it, even
a set that would change nothing. Every set is audited with each key's old
and new value, the backup path and the outcome.

## Upgrade

There are two ways to upgrade `nexusd`: by hand, with `install.sh`, or from
a client, with `nexus nexusd update`. Upgrade the client by replacing the
`nexus` binary. A client works with any `nexusd` of the same API version, so
hosts can be upgraded one at a time.

### By hand

Unpack the newer `nexusd` tarball and run `sudo ./install.sh` again. It
replaces the binary and the unit, keeps the configuration, tokens,
certificate, audit log and operation history, and restarts a running
`nexusd` onto the new binary. Apt changes, script runs and container
updates in progress run in their own `nexus-op-*` units and keep running;
the new `nexusd` picks them up. `git pull && ./deploy.sh` does the same from
a checkout.

The first install of a version that includes the `nexusd` provider is
manual: install it this way, or with `deploy.sh`. A `nexusd` can update
itself only once it already has the provider, has it enabled and trusts at
least one release key. The manual path keeps working afterwards, for any
version.

### From a client: nexus nexusd update

`nexus nexusd update` uploads a signed release tarball to one host, which
checks the signature, installs the release with its own `install.sh`,
restarts onto it and checks that it is healthy, and restores the previous
version if it is not. Each host is updated on its own: there is no
coordinated rollout across hosts. It takes four steps the first time.

1. **Make a release signing key, once,** on the machine that builds
   releases:

   ```
   make release-key
   ```

   It writes an Ed25519 private key to
   `~/.config/nexus/release-signing.key` (or the path in
   `$NEXUS_RELEASE_KEY`), mode 0600, refuses to replace a key that is
   already there, and prints the line to add to each host:

   ```
   trustedKeys: ["<base64 public key>"]
   ```

   The private key stays on that machine. Never copy it to a host and never
   commit it: the default path is outside the repository, and
   `$NEXUS_RELEASE_KEY` must not point into a checkout either. Keep an
   offline backup; whoever holds it can sign a release every host accepts
   (see [security.md](security.md#the-release-signing-key)). `go run
   ./cmd/nexus-release pubkey` prints the line again.

2. **Trust the key on each host.** Add the line under
   `providers.nexusd.update` in `/etc/nexus/nexusd.yaml`, and enable the
   provider:

   ```yaml
   providers:
     nexusd:
       enabled: true
       update:
         trustedKeys: ["<base64 public key>"]
   ```

   Then check the file and reload:

   ```
   sudo nexusd validate-config
   sudo systemctl reload nexusd
   ```

   A malformed or duplicate key is an error. Keys change only by editing
   this file: no API, MCP or CLI call can add or remove one. To rotate,
   make a new key on the build machine at another path (for example
   `NEXUS_RELEASE_KEY=$HOME/.config/nexus/release-signing-2.key make
   release-key`), list both keys on every host and reload, sign releases
   with the new key (the same `NEXUS_RELEASE_KEY` for `make release`), then
   remove the old key from each host and reload.

   The token that updates needs `nexusd:admin`, which is root-equivalent
   (see [security.md](security.md)); `nexusd:read` is enough for `nexus
   nexusd get` and `nexus nexusd validate-config`. `nexus nexusd get` shows
   `updateConfigured` true once a key is trusted, and whether
   self-management is available: restart, update and rollback need
   `nexusd` to run as `nexusd.service` under systemd.

3. **Build a signed release:**

   ```
   make release VERSION=v1.4.0
   ```

   It refuses to start without a usable key (missing, or readable by a
   group or others), builds the tarballs into `dist/release/`, writes
   `nexusd-v1.4.0-linux-amd64.tar.gz.sig` next to the `nexusd` tarball and
   lists it in `SHA256SUMS`. Only the `nexusd` tarball is signed.

4. **Update:**

   ```
   nexus nexusd update --file dist/release/nexusd-v1.4.0-linux-amd64.tar.gz --host nas
   ```

   `--signature PATH` names the signature file when it is not `<file>.sig`.
   `--allow-downgrade` installs a release older than the running one.

Before anything runs, the host refuses the update, creates no operation and
changes nothing when:

- no key is trusted (`update-not-configured`);
- the upload is unknown or expired (`upload-not-found`);
- the signature does not verify under any trusted key
  (`update-signature-invalid`), checked before any byte of the tarball is
  unpacked or run;
- the tarball is not a `nexusd` release, or its `nexusd` does not report
  the version it should (`update-package-invalid`);
- the release is older than the running `nexusd` (409 `version-downgrade`)
  and `--allow-downgrade` was not given. A running version that is not a
  release version, such as `dev` or the `git describe` version of a
  `deploy.sh` build, cannot be compared with a release, so updating from
  one counts as a downgrade and needs `--allow-downgrade`.

A signature or package refusal also deletes the upload. The same version as
the running one succeeds with `changed` false and installs nothing.

Otherwise the update runs as an operation in its own `nexus-op-<id>` unit,
as root and outside the `nexusd` sandbox:

1. It saves the running binary as `/usr/local/bin/nexusd.prev` and the unit
   file as `/var/lib/nexus/rollback/nexusd.service`.
2. It runs the release's `install.sh`, which installs the binary and unit
   and restarts `nexusd`, keeping the configuration, tokens, certificate and
   history as a manual upgrade does.
3. It checks the new `nexusd`'s health: the unit is active, it reports the
   new version, `/healthz` answers, and it still does 5 seconds later, all
   within `providers.nexusd.healthTimeout` (default 30s).

The CLI waits for the operation and keeps reconnecting for up to 60 seconds
while `nexusd` restarts. Other providers' operations are neither waited for
nor canceled: those running in their own units carry on, and those running
inside `nexusd` end `outcome-unknown`.

If the install, the restart or the health check fails, the update rolls
back by itself: it copies `nexusd.prev` and the saved unit file back,
restarts `nexusd` and checks its health against the previous version. The
operation then fails with:

- `update-failed`, with `rolledBack` true: the previous version is running
  and healthy, and nothing is left to do but read why
  (`nexus operationlog get <id>`);
- `update-rollback-failed`: the restore failed too, and `nexusd` may be
  down. Follow [Recovering from a failed update](#recovering-from-a-failed-update)
  on the host.

An update whose unit stopped without a result ends `outcome-unknown`; its
detail names what to check, and the same section applies if `nexusd` is not
healthy.

### Restart and roll back

- `nexus nexusd restart` restarts `nexusd.service` and runs the same health
  check. An unhealthy restart fails with `restart-unhealthy` and is not
  rolled back. A configuration that `nexusd` would refuse to start with is
  refused first, with `config-invalid`.
- `nexus nexusd rollback` swaps `/usr/local/bin/nexusd` with `nexusd.prev`,
  and the unit file with its saved copy when there is one, then restarts and
  checks health against the previous version. Because it swaps, a second
  rollback returns to the version the first one replaced. It is refused
  with `rollback-unavailable` when there is no `nexusd.prev`, and with
  `config-invalid` when `nexusd.prev` rejects the current configuration.
  If the rolled-back `nexusd` is unhealthy, the swap is undone and the
  operation fails with `rollback-failed`.

Reload, restart, update and rollback take turns: while one is queued or
running, another answers 409 naming it. Each can be canceled only while
queued.

### Staged uploads

The CLI first uploads the tarball and its signature to the host, which
stores it under `/var/lib/nexus/staging/` and returns an upload ID, then
starts the update with that ID. A tarball larger than
`providers.nexusd.update.maxUploadBytes` (default 64 MiB, at most 256 MiB)
is refused with 413 `upload-too-large` and nothing is kept.

At most 2 uploads are staged at once. An upload is deleted when the update
that used it ends, or an hour after it was received. A third upload answers
409 `conflict`: its detail says when the earliest staged upload expires, or
that two uploads are still being received; retry after that. Uploading the
same tarball and signature again while it is staged reuses the staged
upload, so retrying a refused downgrade with `--allow-downgrade` takes no
second slot.

## Recovering from a failed update

An update that fails with `update-rollback-failed` could not restore the
previous version either, so `nexusd` may be down and cannot be reached with
`nexus`. Recover on the host, as root.

1. See what state it is in:

   ```
   sudo systemctl status nexusd.service
   sudo journalctl -u nexusd.service -n 100
   /usr/local/bin/nexusd -version
   /usr/local/bin/nexusd.prev -version
   ```

   `nexusd.prev` is the version that ran before the update.

2. If `/usr/local/bin/nexusd.prev` is missing and
   `/usr/local/bin/nexusd.swap.tmp` exists, a rollback was interrupted in
   the middle of its swap: `/usr/local/bin/nexusd` already holds the
   content of the former `nexusd.prev`, and `nexusd.swap.tmp` holds the
   binary it replaced. Rename it back first:

   ```
   sudo mv /usr/local/bin/nexusd.swap.tmp /usr/local/bin/nexusd.prev
   ```

   The unit file's swap works the same way, with
   `/var/lib/nexus/rollback/nexusd.service.swap.tmp` in place of a missing
   `/var/lib/nexus/rollback/nexusd.service`. A `.swap.tmp` file beside a
   file that is present is a leftover copy and can be deleted.

3. Put the previous version back, with its unit file when
   `/var/lib/nexus/rollback/nexusd.service` exists, and restart:

   ```
   sudo systemctl stop nexusd.service
   sudo cp /usr/local/bin/nexusd.prev /usr/local/bin/nexusd
   sudo cp /var/lib/nexus/rollback/nexusd.service /etc/systemd/system/nexusd.service
   sudo systemctl daemon-reload
   sudo systemctl reset-failed nexusd.service
   sudo systemctl restart nexusd.service
   ```

   The stop keeps `cp` from failing with "Text file busy" while systemd is
   restarting the failed `nexusd`.

4. Check it:

   ```
   /usr/local/bin/nexusd -version
   sudo systemctl status nexusd.service
   ```

   The version is the one `nexusd.prev` reported. The `nexusd` that starts
   records the update's outcome; `nexus nexusd get` then answers again.

If `nexusd.prev` does not start either, install a release by hand, as
[By hand](#by-hand) describes.

When `nexus package update` upgrades packages on a host, Ubuntu's
`needrestart` apt hook may restart `nexusd.service` along with other
services. That is expected: the upgrade itself runs in its own unit, which
`needrestart` does not stop, and `nexusd` reports its outcome when it is
back. Script runs survive it the same way.

## Recovering from a failed configuration change

A config set that fails with `config-restore-failed` changed
`/etc/nexus/nexusd.yaml`, saw the reload refuse it (or not answer), and then
could not confirm that the restored backup was applied either. The file on
disk may not be the configuration in force, and `nexusd` may not be
running. The same steps apply to a config set that ended `outcome-unknown`.
Recover on the host, as root. The detail names the two files; with the
default paths:

1. See what state it is in:

   ```
   sudo systemctl status nexusd.service
   sudo journalctl -u nexusd.service -n 100
   sudo nexusd validate-config
   sudo diff /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml
   ```

   `/etc/nexus/nexusd.yaml.bak` is the file from before the change. `sudo
   cat /var/lib/nexus/reload-status.json` shows the last reload's outcome:
   `applied`, its `error`, and the `sha256` of the file it read, which
   `sudo sha256sum /etc/nexus/nexusd.yaml` can be compared with.

2. If `nexusd validate-config` reports errors, put the backup back. `cp`
   onto the existing file keeps its owner and mode; check that they are
   still `root:root` and 0600:

   ```
   sudo cp /etc/nexus/nexusd.yaml.bak /etc/nexus/nexusd.yaml
   sudo chown root:root /etc/nexus/nexusd.yaml
   sudo chmod 0600 /etc/nexus/nexusd.yaml
   sudo nexusd validate-config
   ```

   If the file is valid, keep it: it is the change you asked for, and
   reloading it applies it.

3. Apply the file and check that the reload took it:

   ```
   sudo systemctl reload nexusd.service
   sudo journalctl -u nexusd.service -n 20
   ```

   If `nexusd.service` is not running, start it instead with `sudo systemctl
   restart nexusd.service`.

4. From a client, `nexus nexusd get` answers again, and `nexus nexusdconfig
   get` shows the file.

Another change made on the host while the set ran is kept, not restored:
the set then fails with `config-conflict`, and the backup holds the file
from before the set.

## Operate

- `nexusd` logs to the journal: `journalctl -u nexusd`.
- The audit log is `/var/log/nexus/audit.jsonl`; query it with
  `nexus auditrecord get` and a token with `audit:read`.
- State (operation history, tokens, the certificate) is in
  `/var/lib/nexus`. With default settings, state, audit and operation logs
  stay under 1 GiB (`limits.storageBudgetBytes`), enforced by rotation and
  retention.
- An `--idempotency-key` makes a retried mutating command safe: the same
  key and input return the original operation for 24 hours.
- `nexus package install NAME --version V` installs exactly that version and
  never downgrades: a version older than the installed one fails and
  changes nothing.
- Each long-running operation runs in `nexus-op-<id>.service`. Cancel it
  with `nexus operation cancel <id>`. `systemctl stop` on such a unit is
  refused; an operator on the host uses
  `sudo systemctl kill --kill-whom=main --signal=SIGINT nexus-op-<id>.service`
  instead, and the operation ends `Canceled` (see
  [security.md](security.md#operation-units)).

## Uninstall

From the unpacked release directory:

```
sudo ./uninstall.sh            # remove the binary and the unit; keep the data
sudo ./uninstall.sh --purge    # also delete /etc/nexus, /var/lib/nexus and /var/log/nexus
```

The binary includes the rollback copy that updates keep:
`/usr/local/bin/nexusd.prev` and `/var/lib/nexus/rollback/` are removed
either way, and `--purge` also removes the staged uploads.

It stops and disables `nexusd.service` first. Operation units still running
are listed and left to finish; `--purge` deletes their records while they
run.
