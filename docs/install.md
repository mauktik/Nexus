# Installing Nexus

Nexus has two parts: `nexusd`, the server, on each Ubuntu host you manage,
and `nexus`, the command-line client, on your Mac or Linux machine. Each
host runs its own `nexusd`; the client talks to one or many of them.

Read [security.md](security.md) first: Nexus is for the home LAN only.

## Requirements

- **Host:** the latest Ubuntu on x86-64 (amd64), with systemd. No compiler
  or language runtime is needed. There is no arm64 build of `nexusd`.
- **For the `docker` provider:** Docker Engine with Compose v2, reachable
  through its local Unix socket (`/run/docker.sock` by default).
- **For the `apt` provider:** apt and dpkg, as Ubuntu ships them.
- **For the `scripts` provider:** systemd, and the scripts you will catalog.
- **Client:** macOS on Apple silicon (darwin/arm64) or Linux on amd64.

## Install nexusd on a host

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
   token's lifetime.

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
changed keys that take effect only after a restart. No call writes the file:
it is changed only by editing it on the host.

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
  `sudo systemctl kill --kill-whom=main --signal=SIGTERM nexus-op-<id>.service`
  instead (see [security.md](security.md)).

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
