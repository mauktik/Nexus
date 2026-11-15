# Nexus

Nexus is a lightweight server manager for a home Linux host (Ubuntu/Debian with systemd, or Alpine with OpenRC): a server daemon with an API and an agent-callable surface, plus a command-line client. It ships two binaries:

- `nexusd`, the server daemon, built for linux/amd64.
- `nexus`, the command-line client, built for darwin/arm64 and linux/amd64.

Each host runs its own `nexusd`, as root: under a hardened systemd unit on Ubuntu, or as an OpenRC service under `supervise-daemon` on Alpine, where it has no sandbox. It manages existing Docker containers (Compose and standalone), apt packages, scripts cataloged in its configuration, and the host's nginx service and site configurations; it also gives a read-only overview of the host itself (identity, CPU, memory, filesystems, disks, network, health flags and a short metrics history). It serves all of this three ways from one set of operation declarations: a REST API described by OpenAPI 3.1 under `/v1`, MCP tools at `/mcp` for agents, and the `nexus` CLI, whose commands are noun first (`nexus container get`) and can fan out across several hosts. A token's scopes alone decide what it may do. Nexus is for the home LAN only, with a pinned self-signed certificate.

Module path: `github.com/mauktik/nexus`. Go 1.26 is required.

## Documentation

| Page | Contents |
|---|---|
| [docs/install.md](docs/install.md) | Installing `nexusd` and `nexus`, tokens, pinning, upgrades, uninstalling |
| [docs/security.md](docs/security.md) | The security model: threat assumptions, LAN-only exposure, tokens and scopes (including `system:read`), the systemd unit and OpenRC hosts |
| [docs/scripts.md](docs/scripts.md) | Cataloging and running scripts: what changes from a terminal, integrity checks, the onboarding checklist |
| [docs/mcp.md](docs/mcp.md) | MCP client setup, tool semantics and the tool budget |
| [docs/nginx.md](docs/nginx.md) | The nginx provider: configuration, the include line, sites and the rename model, writes with backup and rollback, reload verification, the CLI's `--content` limit |
| [docs/pihole.md](docs/pihole.md) | The Pi-hole provider: DNS zones and records, blocking, domains, blocklists, safe writes and rollback |
| [docs/host.md](docs/host.md) | The host overview provider and its `system:read` scope: fields and units, unavailable fields and their reasons, the sampler and in-memory metrics history, configuration |
| [docs/cli/nexus.md](docs/cli/nexus.md) | The CLI reference, generated from the command definitions |
| [docs/api.md](docs/api.md) | The REST API reference, generated from the OpenAPI document |
| [docs/adding-a-provider.md](docs/adding-a-provider.md) | Adding a provider: spec, binding, the two composition lines |

## Quick start

On the host, from a clone of this repository (needs Go; see
[docs/install.md](docs/install.md#install-from-a-checkout-deploysh)):

```sh
./deploy.sh                            # as your normal user: builds, then installs with sudo
```

or from the unpacked `nexusd` release:

```sh
sudo ./install.sh
```

Then:

```sh
sudo nexusd validate-config            # after enabling providers in /etc/nexus/nexusd.yaml
sudo systemctl enable --now nexusd.service
sudo nexusd tls fingerprint
sudo nexusd token create --name laptop --scope containers:read
```

On your machine:

```sh
nexus host add nas --url https://192.168.1.10:7443
nexus container get
```

[docs/install.md](docs/install.md) has the details, including listening on the LAN.

### CLI exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | General or operation failure |
| 2 | Usage error |
| 3 | Authentication or authorization failure |
| 4 | Resource not found, or capability unavailable |
| 5 | Connection or TLS failure |
| 6 | Partial failure across hosts or stdin items |
| 7 | The client stopped waiting (`--timeout`); the operation is still running |
| 130 | Detached by an interrupt; the operation is still running |

## Building

| Command | Result |
|---|---|
| `make build` | Host-arch `nexusd` and `nexus` in `build/` |
| `make build-release` | Static release binaries in `dist/`: `nexusd-linux-amd64`, `nexus-darwin-arm64`, `nexus-linux-amd64` |

Release binaries are built with `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`. No linux/arm64 artifact is produced.

The version comes from `git describe` and is stamped into `internal/version` with `-ldflags -X`. Override it with `make build-release VERSION=v1.2.3`. Both binaries print it with `--version`.

## The gate: `make check`

`make check` is the CI gate. Hosted CI is not assumed, so a change is ready only when `make check` passes on a developer machine. It runs, in order:

1. `make vet`: `go vet ./...`, then the same for `GOOS=linux GOARCH=amd64`.
2. `make test`: `go test -race ./...`.
3. `make vuln`: `govulncheck` at a pinned version, run with `go run`.

**Prerequisite: network access.** `make vuln` downloads the pinned `govulncheck` module and queries the Go vulnerability database, so `make check` fails without network access.

The race detector needs cgo, so `make test` needs a C toolchain (Xcode command-line tools on macOS).

### Build-system tests

The tests in `test/buildsys` that run `make` themselves (the release-artifact check and the `make check` pass/fail check) skip when `NEXUS_IN_MAKE` is set. The Makefile exports it so `make test` never recurses into `make`. Run them directly with:

```sh
go test ./test/buildsys/
```

## Generated documentation: `make docs`

`docs/cli/` and `docs/api.md` are generated by `cmd/nexus-gendocs` from the operation declarations: the CLI reference from the `nexus` command tree, and the API reference from the OpenAPI document. After changing a declaration or a command, run:

```sh
make docs
```

and commit `docs/`. `make test` fails while the generated files are stale, naming each one, and `make docs-check` runs the same check without writing anything. The other pages in `docs/` are written by hand.

## T1 integration tests: `make test-harness`

`make test-harness` runs the integration tests against real systemd, Docker and apt in a privileged container on the Docker CLI's current context. It needs Docker and is not part of `make check`. `RUN` narrows the tests with a `go test -run` regular expression; make expands `$`, so write a `$` anchor as `$$`:

```sh
make test-harness RUN='^TestScriptOutcomes$$'
```
