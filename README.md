# Nexus

Nexus is a lightweight server manager for a home Ubuntu host: a server daemon with an API and an agent-callable surface, plus a command-line client. It ships two binaries:

- `nexusd`, the server daemon, built for linux/amd64.
- `nexus`, the command-line client, built for darwin/arm64 and linux/amd64.

Module path: `github.com/mauktik/nexus`. Go 1.26 is required.

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
