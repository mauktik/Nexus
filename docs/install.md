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
   token's lifetime.

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
`/var/lib/nexus/ops/<id>/supervisor.log`; to stop one by hand, see
[OpenRC hosts](security.md#openrc-hosts-alpine).

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
