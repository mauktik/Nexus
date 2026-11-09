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

## Upgrade

Unpack the newer `nexusd` tarball and run `sudo ./install.sh` again. It
replaces the binary and the unit, keeps the configuration, tokens,
certificate, audit log and operation history, and restarts a running
`nexusd` onto the new binary. Apt changes, script runs and container
updates in progress run in their own `nexus-op-*` units and keep running;
the new `nexusd` picks them up. Nexus
does not update itself. Upgrade the client by replacing the `nexus` binary.
A client works with any `nexusd` of the same API version, so hosts can be
upgraded one at a time.

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

It stops and disables `nexusd.service` first. Operation units still running
are listed and left to finish; `--purge` deletes their records while they
run.
