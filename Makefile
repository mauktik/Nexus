# Nexus build. `make check` (vet + test + vuln) is the gate; hosted CI is not
# assumed. `make vuln` needs network access to fetch the pinned govulncheck.

MODULE  := github.com/mauktik/nexus
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

BUILD_DIR := build
DIST_DIR  := dist
# make release writes the tarballs and SHA256SUMS here, and stages their
# trees in STAGE_DIR.
RELEASE_DIR := $(DIST_DIR)/release
STAGE_DIR   := $(BUILD_DIR)/release

VERSION_LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION)
RELEASE_LDFLAGS := -s -w $(VERSION_LDFLAGS)
# Release binaries are static (FR-38), so the same nexusd runs on glibc and
# musl (Alpine) hosts. Under CGO_ENABLED=0 the standard library takes its
# pure-Go paths: os/user parses /etc/passwd and /etc/group itself, and net
# uses the pure-Go DNS resolver, which reads /etc/resolv.conf, /etc/hosts and
# /etc/nsswitch.conf. Neither calls libc, so lookups behave the same on musl.
# build-release checks the linux binaries with STATICCHECK.
RELEASE_BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags='$(RELEASE_LDFLAGS)'
# Fails when an ELF has a PT_INTERP program header or any DT_NEEDED entry.
STATICCHECK     := go run ./test/buildsys/staticcheck

# Everything the nexusd tarball carries besides the binary and the docs.
NEXUSD_FILES := deploy/nexusd.service deploy/nexusd.openrc deploy/nexusd.default.yaml deploy/install.sh deploy/uninstall.sh

# Tarball entries are owned by root, without extended attributes or macOS
# metadata, whichever tar builds them.
ifneq (,$(findstring GNU,$(shell tar --version 2>/dev/null)))
TAR_FLAGS := --owner=0 --group=0 --numeric-owner --no-xattrs
else
TAR_FLAGS := --uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata
endif
SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo shasum -a 256)

# Build-system tests that run make skip when this is set, so `make test` never
# recurses into make.
export NEXUS_IN_MAKE := 1

.PHONY: build build-release test vet vuln check docs docs-check test-harness test-harness-alpine test-harness-pihole release release-version nexusd-tree test-emu nfr nfr-sizes

# Host-arch binaries for local use.
build:
	go build -ldflags='$(VERSION_LDFLAGS)' -o $(BUILD_DIR)/ ./cmd/nexusd ./cmd/nexus

# Exactly the shipped artifacts: nexusd linux/amd64, nexus darwin/arm64 and
# linux/amd64. There is deliberately no linux/arm64 artifact. The linux
# binaries are built and checked static first, so a dynamically linked one
# fails the target before anything else is built.
build-release:
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	GOOS=linux  GOARCH=amd64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexusd-linux-amd64 ./cmd/nexusd
	GOOS=linux  GOARCH=amd64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexus-linux-amd64  ./cmd/nexus
	$(STATICCHECK) $(DIST_DIR)/nexusd-linux-amd64 $(DIST_DIR)/nexus-linux-amd64
	GOOS=darwin GOARCH=arm64 $(RELEASE_BUILD) -o $(DIST_DIR)/nexus-darwin-arm64 ./cmd/nexus

# Go's default per-package timeout is 10m. test/buildsys runs `make check`
# (which runs this whole suite) inside TestMakeCheckIsTheGate and takes
# ~7-8 minutes unloaded, so under load it overran the default and panicked
# "test timed out". 30m leaves headroom without hiding a real hang.
TEST_TIMEOUT ?= 30m

test:
	go test -race -timeout $(TEST_TIMEOUT) ./...

# Linux-only code sits behind build tags, so vet the release target too.
vet:
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

check: vet test vuln

# The CLI reference (docs/cli/) and the API reference (docs/api.md), generated
# from the operation declarations. Commit the result: `make test` fails while
# they are stale, and docs-check names each stale file without writing.
docs:
	go run ./cmd/nexus-gendocs -dir docs

docs-check:
	go run ./cmd/nexus-gendocs -dir docs -check

# release VERSION=vX.Y.Z[-pre]: the gate (vet, test, govulncheck), the
# release build, then one tarball per target and SHA256SUMS in dist/release/.
# A failing step stops the build before any tarball is written.
release: release-version check build-release
	rm -rf $(RELEASE_DIR) $(STAGE_DIR)
	mkdir -p $(RELEASE_DIR) $(STAGE_DIR)
	$(MAKE) --no-print-directory nexusd-tree TREE_BIN=$(DIST_DIR)/nexusd-linux-amd64 TREE_DIR=$(STAGE_DIR)/nexusd-$(VERSION)-linux-amd64
	for t in darwin-arm64 linux-amd64; do \
		d=$(STAGE_DIR)/nexus-$(VERSION)-$$t; \
		mkdir -p $$d/docs && cp $(DIST_DIR)/nexus-$$t $$d/nexus && cp README.md $$d/ && cp -R docs/. $$d/docs/ && \
		chmod -R u=rwX,go=rX $$d && chmod 0755 $$d/nexus || exit 1; \
	done
	for t in nexusd-$(VERSION)-linux-amd64 nexus-$(VERSION)-darwin-arm64 nexus-$(VERSION)-linux-amd64; do \
		(cd $(STAGE_DIR) && COPYFILE_DISABLE=1 tar $(TAR_FLAGS) -czf $(CURDIR)/$(RELEASE_DIR)/$$t.tar.gz $$t) || exit 1; \
	done
	cd $(RELEASE_DIR) && $(SHA256) *.tar.gz >SHA256SUMS && $(SHA256) -c SHA256SUMS

release-version:
	@echo '$(VERSION)' | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || \
		{ echo "release: VERSION='$(VERSION)' is not vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2; exit 1; }

# nexusd-tree TREE_BIN=<binary> TREE_DIR=<dir>: the nexusd release tree
# (binary, systemd unit, OpenRC script, default config, install and
# uninstall scripts, docs). The release packs it; the T1 harness installs a
# harness-arch one with install.sh.
nexusd-tree:
	@[ -n '$(TREE_BIN)' ] && [ -n '$(TREE_DIR)' ] || { echo "nexusd-tree: set TREE_BIN and TREE_DIR" >&2; exit 2; }
	rm -rf $(TREE_DIR)
	mkdir -p $(TREE_DIR)/docs
	cp $(TREE_BIN) $(TREE_DIR)/nexusd
	cp $(NEXUSD_FILES) README.md $(TREE_DIR)/
	cp -R docs/. $(TREE_DIR)/docs/
	chmod -R u=rwX,go=rX $(TREE_DIR)
	chmod 0755 $(TREE_DIR)/nexusd $(TREE_DIR)/nexusd.openrc $(TREE_DIR)/install.sh $(TREE_DIR)/uninstall.sh

# T1: real systemd in a privileged container on the Docker CLI's current
# context, native architecture. Test-only binaries go to build/harness/,
# never dist/. BASE defaults to ubuntu:latest; RUN narrows the tests.
test-harness:
	BASE='$(BASE)' RUN='$(RUN)' test/harness/run.sh

# T1 Alpine (WI 3.4): the same run on real OpenRC, BusyBox init as PID 1 in
# a privileged container with a private cgroup namespace (cgroup v2
# writable). BASE defaults to alpine:latest; RUN narrows the tests.
test-harness-alpine:
	BASE='$(BASE)' RUN='$(RUN)' test/harness/run.sh alpine
# T1 Pi-hole v5 (WI 5.1): the test/harness/pihole image (FTL v5.25.2 under
# systemd, with the stub upstream and the fixture HTTP server), built with
# pihole-FTL's native systemd unit or, with INIT=sysv, the SysV script; then
# the ^TestPihole tests in it.
INIT ?= native
# RUN narrows the tests here too (default ^TestPihole); it is target-specific
# so the other T1 targets keep their own default.
test-harness-pihole: RUN ?= ^TestPihole
test-harness-pihole:
	docker build --quiet --build-arg INIT='$(INIT)' -t 'nexus-harness-pihole:$(INIT)' test/harness/pihole
	BASE='nexus-harness-pihole:$(INIT)' RUN='$(RUN)' test/harness/run.sh

# T1-emu (TC-184): the linux/amd64 nexusd from the tarball `make release`
# wrote, in a plain container under amd64 emulation: no systemd, a smoke test
# only (A-25).
test-emu:
	BASE='$(BASE)' test/harness/run.sh emu

# NFR measurements (WI 10.3): the release binaries' sizes (NFR-1: each under
# 40 MiB), then the T1 measurement tests, TC-187 to TC-192, with 50 real
# containers. Record the numbers in docs/verification/measurements.md.
NFR_MAX_BYTES := 41943040
nfr: nfr-sizes
	$(MAKE) --no-print-directory test-harness RUN='^TestNFR'

nfr-sizes: build-release
	@for f in $(DIST_DIR)/nexusd-linux-amd64 $(DIST_DIR)/nexus-darwin-arm64 $(DIST_DIR)/nexus-linux-amd64; do \
		s=$$(wc -c <$$f | tr -d ' '); \
		echo "nfr: $$f $$s bytes ($$(awk -v s=$$s 'BEGIN { printf "%.2f", s / 1048576 }' </dev/null) MiB)"; \
		[ $$s -lt $(NFR_MAX_BYTES) ] || { echo "nfr: $$f is not under 40 MiB" >&2; exit 1; }; \
	done
