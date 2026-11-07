# Nexus build. `make check` (vet + test + vuln) is the gate; hosted CI is not
# assumed. `make vuln` needs network access to fetch the pinned govulncheck.

MODULE  := github.com/mauktik/nexus
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

BUILD_DIR := build
DIST_DIR  := dist

VERSION_LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)
RELEASE_LDFLAGS := -s -w $(VERSION_LDFLAGS)
RELEASE_BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags='$(RELEASE_LDFLAGS)'

# Build-system tests that run make skip when this is set, so `make test` never
# recurses into make.
export NEXUS_IN_MAKE := 1

.PHONY: build build-release test vet vuln check test-harness

# Host-arch binaries for local use.
build:
	go build -ldflags='$(VERSION_LDFLAGS)' -o $(BUILD_DIR)/ ./cmd/nexusd ./cmd/nexus

# Exactly the shipped artifacts: nexusd linux/amd64, nexus darwin/arm64 and
# linux/amd64. There is deliberately no linux/arm64 artifact.
build-release:
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	GOOS=linux  GOARCH=amd64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexusd-linux-amd64 ./cmd/nexusd
	GOOS=darwin GOARCH=arm64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexus-darwin-arm64 ./cmd/nexus
	GOOS=linux  GOARCH=amd64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexus-linux-amd64  ./cmd/nexus

test:
	go test -race ./...

# Linux-only code sits behind build tags, so vet the release target too.
vet:
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

check: vet test vuln

# T1: real systemd in a privileged container on the Docker CLI's current
# context, native architecture. Test-only binaries go to build/harness/,
# never dist/. BASE defaults to ubuntu:latest; RUN narrows the tests.
test-harness:
	BASE='$(BASE)' RUN='$(RUN)' test/harness/run.sh
