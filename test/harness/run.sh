#!/bin/bash
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# T1 harness runner: `make test-harness [BASE=ubuntu:latest] [RUN=regex]`
# (architecture.md, Verification Architecture). `run.sh alpine`
# (make test-harness-alpine) is the same run on the Alpine/OpenRC harness:
# Dockerfile.alpine, BusyBox init with OpenRC as PID 1, --cgroupns=private.
# `run.sh emu` (make test-emu) is the T1-emu smoke of the release artifact
# instead; see emu_smoke.
#
# Builds the image FROM $BASE, cross-compiles nexusd, opsd and the
# integration test binary for the architecture `docker info` reports into
# build/harness/ (never dist/), starts a privileged systemd container with
# the exact flags, waits for readiness (running or degraded, then the
# required units), copies everything in with docker cp (no bind mount), runs
# the tests, and always collects the logs and removes the container. The
# release tree install.sh installs from (make nexusd-tree, with the harness
# nexusd) goes to /harness/release/nexusd.
#
# Before integration.test, the NFR-8 parity suites run in the container:
# `go test -c` binaries of ./internal/detach and ./internal/ops, as root
# (detach.log, ops.log) and as an unprivileged user (detach-nonroot.log,
# ops-nonroot.log), with the harness nexusd as their supervisor executable.
# After it, TestParitySuitesRanAsRoot reads those logs and test.log
# (parity-check.log). Last, internal/svcctl's TestLiveMatrix drives the
# container's real nginx through the detected init system as root
# (NEXUS_SVCCTL_LIVE=1, svcctl-live.log). Both images carry the
# distribution's nginx and curl for the nginx T1 suite (nginx-provider WI
# 7.1), whose TC-113 reads internal/svcctl's captures through the svcctltest
# fake: they are copied to /harness/src/<module path>/internal/svcctl/testdata,
# where the fake's -trimpath source path resolves from /harness/src.
# nginx's error.log is collected with the other logs (nginx-error.log).
# PARITY=0 leaves the parity suites and the live matrix out for a quick run.
#
# The self-update fixtures (WI 7.1) go to /harness/selfupdate: one signed
# nexusd-<v>-linux-<goarch>.tar.gz per fixture version (see build_fixtures),
# each with its .sig, and release-test.pub, the public half of a key made for
# this run only (build/harness/release-test.key; never the real key under
# ~/.config/nexus). Each signature is checked on the host before the copy-in.
# run.sh's own output is kept in logs/<stamp>/run.log and copied in as
# /harness/run.log just before the tests run.
#
# The Docker CLI's current context is used, but only when its endpoint is a
# local unix:// socket (CON-1, A-42): the container is privileged, and a
# context pointing at another host, such as the live server, must never
# receive it. Nothing but `docker context inspect` runs before that check.
#
# Pi-hole (WI 5.1): when BASE is an image labelled org.nexus.harness=pihole,
# which `make test-harness-pihole [INIT=native|sysv]` builds from
# test/harness/pihole, that image is the harness image as is. The required
# units are then the stub upstream and the fixture HTTP server, FTL is ready
# when its PID file names a live pihole-FTL that answers dig (never
# `systemctl is-active`, which the SysV wrapper keeps active after FTL dies),
# and pihole_setup runs once before any test.
#
# Environment: BASE (image base), RUN (-test.run regex), PARITY, NAME_PREFIX
# (container names), TESTFLAGS (more
# test binary flags), NEXUS_HARNESS_EXPECT_FAIL (passed to the tests: a
# deliberate failure that proves logs are collected and the container is
# removed when a test fails), NEXUS_HARNESS_ALLOW_REMOTE=1 (run on a
# non-local Docker endpoint anyway: only for a disposable test host).
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
MODE=systemd
if [ "${1:-}" = alpine ]; then
	MODE=openrc
fi
if [ $MODE = openrc ]; then
	BASE=${BASE:-alpine:latest}
else
	BASE=${BASE:-ubuntu:latest}
fi
RUN=${RUN:-}
PARITY_ARG=${PARITY:-}
PARITY=${PARITY:-1}
# Container names start with NAME_PREFIX: when several harness runs share
# one Docker host, each can sweep only its own.
NAME_PREFIX=${NAME_PREFIX:-nexus-harness}
TESTFLAGS=${TESTFLAGS:-}
OUT=$ROOT/build/harness
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LOGS=$OUT/logs/$STAMP
READY_TIMEOUT=${READY_TIMEOUT:-180}
# How long setup waits for FTL to finish reloading its lists after a command
# that reloads or restarts it (ftl_settle).
LISTS_TIMEOUT=${LISTS_TIMEOUT:-60}
REQUIRED_UNITS="systemd-journald.service dbus.service docker.service"
# Commands in the Pi-hole container, the setup's and the tests', run with a
# root login shell's environment, not the image's ENV (FTL_CMD, FTLCONF_*,
# DNSMASQ_USER and /opt/pihole on PATH belong to its s6 init), as on a host.
CENV=(env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/root LANG=C.UTF-8)

say() { echo "harness: $*"; }
die() {
	echo "harness: FAIL: $*" >&2
	exit 1
}

dist_state() {
	if [ -d dist ]; then
		find dist -type f -exec shasum -a 256 {} + | sort
	else
		echo "dist absent"
	fi
}

# docker_guard refuses a Docker endpoint that is not a local unix:// socket
# unless NEXUS_HARNESS_ALLOW_REMOTE=1. `docker context inspect` reads the
# CLI's own configuration (DOCKER_HOST and DOCKER_CONTEXT included) and
# connects to nothing, so a refused endpoint is never contacted.
docker_guard() {
	local endpoint
	endpoint=$(docker context inspect --format '{{.Endpoints.docker.Host}}') ||
		die "cannot read the endpoint of the current Docker context (docker context inspect)"
	case $endpoint in
	unix://?*)
		say "docker endpoint $endpoint (local socket)"
		;;
	*)
		[ "${NEXUS_HARNESS_ALLOW_REMOTE:-}" = 1 ] ||
			die "the current Docker context's endpoint '$endpoint' is not a local unix:// socket. The harness starts a privileged container and does not send it to another host (CON-1). Switch to a local context (docker context use <name>), or set NEXUS_HARNESS_ALLOW_REMOTE=1 if that endpoint is a disposable test host"
		echo "harness: WARNING: docker endpoint '$endpoint' is not a local socket; running anyway because NEXUS_HARNESS_ALLOW_REMOTE=1" >&2
		;;
	esac
	# Pin the endpoint that was checked: each later docker call would
	# otherwise resolve the CLI's current context again, and a
	# `docker context use` in another shell during the builds could send
	# the privileged container elsewhere (SEC-3).
	export DOCKER_HOST="$endpoint"
	unset DOCKER_CONTEXT
}
docker_guard

# A Pi-hole harness image (WI 5.1) is recognised by its label, read only once
# the endpoint has passed docker_guard.
PIHOLE=""
if [ "$(docker image inspect -f '{{index .Config.Labels "org.nexus.harness"}}' "$BASE" 2>/dev/null || true)" = pihole ]; then
	PIHOLE=1
	REQUIRED_UNITS="systemd-journald.service dbus.service stub-upstream.service fixture-http.service"
	# The Pi-hole image has no nginx or nested Engine for the parity suites
	# and the live matrix; PARITY=1 still runs them on request.
	PARITY=${PARITY_ARG:-0}
fi

# Everything from here on is also kept in logs/<stamp>/run.log.
mkdir -p "$LOGS"
exec > >(tee -a "$LOGS/run.log") 2> >(tee -a "$LOGS/run.log" >&2)

# The real release-signing key must not change during a run: run.sh only
# ever names its own per-run keys.
HOST_KEY=${HOME:-}/.config/nexus/release-signing.key
host_key_state() {
	if [ -e "$HOST_KEY" ] || [ -L "$HOST_KEY" ]; then
		echo "mtime $(stat -c %Y "$HOST_KEY" 2>/dev/null || stat -f %m "$HOST_KEY")"
	else
		echo absent
	fi
}
host_key_before=$(host_key_state)
check_host_key() {
	[ "$(host_key_state)" = "$host_key_before" ] ||
		die "host signing key $HOST_KEY changed during the run (was $host_key_before, now $(host_key_state))"
	say "host signing key $HOST_KEY unchanged ($host_key_before)"
}

# Architecture of the daemon that runs the container, not of this machine.
arch=$(docker info --format '{{.Architecture}}')
case $arch in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) die "unsupported docker architecture $arch" ;;
esac
say "docker context $(docker context show), daemon architecture $arch -> GOARCH=$goarch, BASE=$BASE, init $MODE"

# emu_smoke is TC-184 (T1-emu): the linux/amd64 nexusd from the tarball that
# `make release` wrote, checked against SHA256SUMS, in a plain $BASE
# container under --platform linux/amd64 emulation. There is no systemd and
# no nested Docker there (A-25), so this is a smoke test, not live proof:
# validate-config on the shipped default config, then serve and GET /healthz.
emu_smoke() {
	local rel=$ROOT/dist/release tarball top c rc=0
	[ -f "$rel/SHA256SUMS" ] || die "no $rel/SHA256SUMS: run make release VERSION=... first"
	(cd "$rel" && grep -- '-linux-amd64.tar.gz$' SHA256SUMS | grep '  nexusd-' | shasum -a 256 -c -) ||
		die "the nexusd tarball does not match SHA256SUMS"
	tarball=$(cd "$rel" && ls nexusd-*-linux-amd64.tar.gz)
	[ "$(echo "$tarball" | wc -w)" -eq 1 ] || die "want one nexusd tarball in dist/release, found: $tarball"
	top=${tarball%.tar.gz}
	say "T1-emu: $tarball under --platform linux/amd64, BASE=$BASE"
	# Unpacked here: the emulated GNU tar fails with ENOSYS creating files.
	local unpacked=$OUT/emu
	rm -rf "$unpacked"
	mkdir -p "$unpacked"
	tar -xzf "$rel/$tarball" -C "$unpacked"
	c=nexus-emu-$STAMP-$$
	docker run -d --name "$c" --platform linux/amd64 "$BASE" sleep infinity >/dev/null
	docker cp "$unpacked/$top" "$c:/root/$top"
	# shellcheck disable=SC2016 # Expanded in the container.
	docker exec -e "TOP=$top" "$c" bash -euo pipefail -c '
		chown -R root:root "/root/$TOP" && cd "/root/$TOP"
		echo "emu: uname -m $(uname -m); $(./nexusd -version)"
		[ "$(uname -m)" = x86_64 ] || { echo "emu: not amd64" >&2; exit 1; }
		install -d -m 0700 /etc/nexus
		install -o root -g root -m 0600 nexusd.default.yaml /etc/nexus/nexusd.yaml
		./nexusd validate-config --config /etc/nexus/nexusd.yaml
		# The default config with tls.mode auto: serve generates the certificate.
		./nexusd serve --config /etc/nexus/nexusd.yaml >/root/serve-tls.log 2>&1 &
		pid=$!
		for i in $(seq 1 100); do grep -q "nexusd ready" /root/serve-tls.log && break; sleep 0.2; done
		grep -q "nexusd ready" /root/serve-tls.log && grep -m1 "fingerprint" /root/serve-tls.log ||
			{ cat /root/serve-tls.log >&2; exit 1; }
		kill -TERM "$pid"; wait "$pid" || true
		./nexusd tls fingerprint --config /etc/nexus/nexusd.yaml
		# Plain HTTP on loopback, so bash can send the request: the image has
		# no TLS client.
		sed "s/^  mode: auto /  mode: off  /" nexusd.default.yaml >/root/plain.yaml
		install -o root -g root -m 0600 /root/plain.yaml /etc/nexus/plain.yaml
		./nexusd validate-config --config /etc/nexus/plain.yaml
		install -d -m 0700 /root/state /root/audit
		./nexusd serve --config /etc/nexus/plain.yaml --state-dir /root/state --audit-dir /root/audit >/root/serve.log 2>&1 &
		pid=$!
		for i in $(seq 1 100); do grep -q "nexusd ready" /root/serve.log && break; sleep 0.2; done
		grep -m1 "nexusd ready" /root/serve.log || { cat /root/serve.log >&2; exit 1; }
		exec 3<>/dev/tcp/127.0.0.1/7443
		printf "GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n" >&3
		resp=$(tr -d "\r" <&3)
		printf "emu: GET /healthz -> %s, body %s\n" "$(printf "%s\n" "$resp" | head -n 1)" "$(printf "%s\n" "$resp" | tail -n 1)"
		kill -TERM "$pid"; wait "$pid"; echo "emu: serve exited $? after SIGTERM"
		case $resp in "HTTP/1.1 200 "*) ;; *) echo "emu: /healthz failed: $resp" >&2; cat /root/serve.log >&2; exit 1 ;; esac
	' || rc=$?
	docker rm -f "$c" >/dev/null 2>&1 || true
	say "T1-emu exit $rc (smoke only: emulation is not live proof)"
	exit $rc
}
if [ "${1:-}" = emu ]; then
	emu_smoke
fi

# Self-update fixtures (WI 7.1). Every tree is a `make nexusd-tree` tree
# named nexusd-<v>-linux-<goarch> (A-13) with a harness-arch nexusd built as
# <v>, changed only as its T1 case needs:
#   v9.9.1  the base, installed by hand
#   v9.9.2  a good update
#   v9.9.3  install.sh exits 3 right after the binary mv
#   v9.9.4  nexusd is a stub: -version prints nexusd v9.9.4, validate-config
#           succeeds, serve exits 1
#   v9.9.5  install.sh installs the bundled nexusd-alt, built as v9.9.8
#   v9.9.6  signed by a second key, which the per-run public key does not
#           trust
#   v9.9.7  the restore fails (FR-8, A-44): before the binary mv, install.sh
#           copies the bundled nexusd-restore-stub (-version prints nexusd
#           v9.9.1, validate-config succeeds, serve exits 1) over
#           /usr/local/bin/nexusd.prev, then exits 3; the helper's backup
#           ran before install.sh, so its restore installs the stub
FIXTURES="v9.9.1 v9.9.2 v9.9.3 v9.9.4 v9.9.5 v9.9.6 v9.9.7"
UNTRUSTED_FIXTURE=v9.9.6
FIX=$OUT/selfupdate
KEY=$OUT/release-test.key
UNTRUSTED_KEY=$OUT/release-untrusted.key
VERSION_VAR=github.com/mauktik/nexus/internal/version.Version
# The install.sh lines the fixture patches anchor on.
# shellcheck disable=SC2016 # Literal lines of install.sh.
INSTALL_BIN_LINE='install -o root -g root -m 0755 "$SRC/nexusd" "$BIN.new"'
# shellcheck disable=SC2016
MV_BIN_LINE='mv -f "$BIN.new" "$BIN"'

# write_stub FILE VERSION writes a serve-failing nexusd stand-in, mode 0755.
write_stub() {
	cat >"$1" <<STUB
#!/bin/sh
# T1 self-update fixture: a nexusd that reports $2 and never serves.
case \${1:-} in
-version | --version) echo "nexusd $2" ;;
validate-config) exit 0 ;;
*) echo "nexusd stub $2: '\${1:-}' fails on purpose" >&2; exit 1 ;;
esac
STUB
	chmod 0755 "$1"
}

# patch_install TREE MODE LINE TEXT rewrites TREE/install.sh, inserting TEXT
# before or after LINE, or replacing it (MODE before, after or replace).
# LINE must occur exactly once; a \n in TEXT is a line break.
patch_install() {
	local f=$1/install.sh
	awk -v mode="$2" -v line="$3" -v text="$4" '
		$0 == line {
			n++
			if (mode == "before") print text
			if (mode == "replace") print text; else print
			if (mode == "after") print text
			next
		}
		{ print }
		END { exit n == 1 ? 0 : 1 }' "$f" >"$f.new" ||
		die "patching $(basename "$1")/install.sh: the line '$3' does not occur exactly once"
	mv -f "$f.new" "$f"
	chmod 0755 "$f"
}

# build_fixtures builds the fixture binaries for the harness architecture
# (the caller exports GOOS, GOARCH and CGO_ENABLED), then the trees and their
# tarballs in $FIX.
build_fixtures() {
	local v tree
	rm -rf "$FIX"
	mkdir -p "$FIX/bin" "$FIX/trees" "$FIX/pkg"
	for v in v9.9.1 v9.9.2 v9.9.3 v9.9.5 v9.9.6 v9.9.7 v9.9.8; do
		go build -trimpath -ldflags "-X $VERSION_VAR=$v" -o "$FIX/bin/nexusd-$v" ./cmd/nexusd
	done
	write_stub "$FIX/bin/nexusd-v9.9.4" v9.9.4
	for v in $FIXTURES; do
		tree=$FIX/trees/nexusd-$v-linux-$goarch
		make --no-print-directory -s nexusd-tree TREE_BIN="$FIX/bin/nexusd-$v" TREE_DIR="$tree"
		case $v in
		v9.9.3)
			patch_install "$tree" after "$MV_BIN_LINE" 'exit 3'
			;;
		v9.9.5)
			install -m 0755 "$FIX/bin/nexusd-v9.9.8" "$tree/nexusd-alt"
			# shellcheck disable=SC2016
			patch_install "$tree" replace "$INSTALL_BIN_LINE" 'install -o root -g root -m 0755 "$SRC/nexusd-alt" "$BIN.new"'
			;;
		v9.9.7)
			write_stub "$tree/nexusd-restore-stub" v9.9.1
			# shellcheck disable=SC2016
			patch_install "$tree" before "$INSTALL_BIN_LINE" 'install -o root -g root -m 0755 "$SRC/nexusd-restore-stub" /usr/local/bin/nexusd.prev\nexit 3'
			;;
		esac
		make --no-print-directory -s nexusd-tarball TREE_DIR="$tree" TARBALL="$FIX/pkg/nexusd-$v-linux-$goarch.tar.gz"
	done
}

# sign_fixtures TOOL signs each fixture tarball with a key made for this run
# (the untrusted fixture with a second one), writes the trusted public key to
# release-test.pub, and checks every signature on the host: each trusted
# fixture must verify against that key, and the untrusted one must not.
sign_fixtures() {
	local tool=$1 v t pub untrusted_pub out trusted=()
	rm -f "$KEY" "$UNTRUSTED_KEY"
	"$tool" keygen --key "$KEY" >/dev/null 2>&1 || die "nexus-release keygen --key $KEY failed"
	"$tool" keygen --key "$UNTRUSTED_KEY" >/dev/null 2>&1 || die "nexus-release keygen --key $UNTRUSTED_KEY failed"
	pub=$("$tool" pubkey --key "$KEY")
	untrusted_pub=$("$tool" pubkey --key "$UNTRUSTED_KEY")
	printf '%s\n' "$pub" >"$FIX/pkg/release-test.pub"
	say "per-run signing key ${KEY#"$ROOT"/}, public key $pub; untrusted key ${UNTRUSTED_KEY#"$ROOT"/}"
	for v in $FIXTURES; do
		[ "$v" = "$UNTRUSTED_FIXTURE" ] || trusted+=("$FIX/pkg/nexusd-$v-linux-$goarch.tar.gz")
	done
	"$tool" sign --key "$KEY" "${trusted[@]}" >/dev/null
	"$tool" sign --key "$UNTRUSTED_KEY" "$FIX/pkg/nexusd-$UNTRUSTED_FIXTURE-linux-$goarch.tar.gz" >/dev/null
	for v in $FIXTURES; do
		t=$FIX/pkg/nexusd-$v-linux-$goarch.tar.gz
		if [ "$v" = "$UNTRUSTED_FIXTURE" ]; then
			if "$tool" verify --pubkey "$pub" "$t" >/dev/null 2>&1; then
				die "fixture $v verifies against the per-run key; it must not"
			fi
			"$tool" verify --pubkey "$untrusted_pub" "$t" >/dev/null || die "fixture $v does not verify against its own key"
			say "fixture $v: refused by the per-run key, as intended (signed by ${UNTRUSTED_KEY#"$ROOT"/})"
		else
			out=$("$tool" verify --pubkey "$pub" "$t") || die "fixture $v does not verify against the per-run key"
			say "fixture $v: $out"
		fi
	done
}

dist_before=$(dist_state)
mkdir -p "$OUT" "$LOGS"

# The image first: a Docker problem fails the run before the slower Go builds.
if [ -n "$PIHOLE" ]; then
	tag=$BASE
	say "Pi-hole harness image $tag (INIT=$(docker image inspect -f '{{index .Config.Labels "org.nexus.harness.init"}}' "$BASE"))"
	name="$NAME_PREFIX-$STAMP-$$"
elif [ $MODE = openrc ]; then
	tag="nexus-harness:alpine-$(echo "$BASE" | tr ':/@' '---')"
	dockerfile=test/harness/Dockerfile.alpine
	name="$NAME_PREFIX-alpine-$STAMP-$$"
else
	tag="nexus-harness:$(echo "$BASE" | tr ':/@' '---')"
	dockerfile=test/harness/Dockerfile
	name="$NAME_PREFIX-$STAMP-$$"
fi
if [ -z "$PIHOLE" ]; then
	say "building image $tag from $dockerfile"
	docker build --quiet --build-arg "BASE=$BASE" -f "$dockerfile" -t "$tag" test/harness >/dev/null
fi
say "building linux/$goarch test-only binaries into build/harness/"
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
go build -trimpath -o "$OUT/nexusd" ./cmd/nexusd
go build -trimpath -o "$OUT/opsd" ./test/integration/cmd/opsd
go test -c -trimpath -tags integration -o "$OUT/integration.test" ./test/integration
if [ "$PARITY" = 1 ]; then
	go test -c -trimpath -o "$OUT/detach.test" ./internal/detach
	go test -c -trimpath -o "$OUT/ops.test" ./internal/ops
	go test -c -trimpath -o "$OUT/svcctl.test" ./internal/svcctl
fi
say "building the self-update fixtures ($FIXTURES) into ${FIX#"$ROOT"/}"
build_fixtures
unset CGO_ENABLED GOOS GOARCH
make --no-print-directory -s nexusd-tree TREE_BIN="$OUT/nexusd" TREE_DIR="$OUT/release/nexusd"
go build -o "$OUT/host/nexus-release" ./cmd/nexus-release
sign_fixtures "$OUT/host/nexus-release"

container=""
status=1

collect_and_remove() {
	local rc=$?
	if [ -n "$container" ]; then
		say "collecting logs into ${LOGS#"$ROOT"/}"
		if [ $MODE = openrc ]; then
			docker exec "$container" cat /var/log/rc.log >"$LOGS/rc.log" 2>&1 || true
			docker exec "$container" cat /var/log/messages >"$LOGS/messages.log" 2>&1 || true
			docker exec "$container" rc-status -a >"$LOGS/rc-status.log" 2>&1 || true
			for f in rc.log messages.log; do
				if [ -s "$LOGS/$f" ]; then
					say "collected $f ($(wc -l <"$LOGS/$f" | tr -d ' ') lines)"
				else
					echo "harness: FAIL: $f was not collected" >&2
					rc=1
				fi
			done
		else
			docker exec "$container" journalctl -b --no-pager -o short-precise >"$LOGS/journal.log" 2>&1 || true
			docker exec "$container" systemctl list-units --all --no-pager --plain >"$LOGS/units.log" 2>&1 || true
		fi
		docker exec "$container" cat /var/log/nginx/error.log >"$LOGS/nginx-error.log" 2>&1 || true
		# Files the tests write for the logs (system-provider TC-HP-115 and
		# TC-HP-122: host-unavailable-<init>.txt), when any test wrote one.
		if docker exec "$container" test -d /harness/out; then
			docker cp "$container:/harness/out/." "$LOGS/" >/dev/null 2>&1 || true
		fi
		docker logs "$container" >"$LOGS/container.log" 2>&1 || true
		docker rm -f -v "$container" >/dev/null 2>&1 || true
		if [ -n "$(docker ps -aq --filter "name=^${name}\$")" ]; then
			echo "harness: FAIL: container $name was not removed" >&2
			rc=1
		else
			say "container $name removed"
		fi
	fi
	if [ "$(host_key_state)" != "$host_key_before" ]; then
		echo "harness: FAIL: host signing key $HOST_KEY changed during the run" >&2
		rc=1
	fi
	if [ "$(dist_state)" != "$dist_before" ]; then
		echo "harness: FAIL: dist/ changed during the harness run" >&2
		rc=1
	else
		say "dist/ untouched"
	fi
	if [ $rc -eq 0 ] && [ $status -ne 0 ]; then
		rc=$status
	fi
	say "exit $rc"
	exit $rc
}
trap collect_and_remove EXIT

# cx runs a command in the Pi-hole container under CENV.
cx() { docker exec "$container" "${CENV[@]}" bash -c "$1"; }

# ftl_pid [OLD] waits until /run/pihole-FTL.pid names a live pihole-FTL,
# other than OLD, that answers `dig pi.hole` on 127.0.0.1:53, and prints its
# PID.
ftl_pid() {
	local old=${1:-} pid until=$((SECONDS + READY_TIMEOUT))
	while :; do
		# shellcheck disable=SC2016 # Expanded in the container.
		pid=$(cx 'p=$(cat /run/pihole-FTL.pid 2>/dev/null) && [ -n "$p" ] &&
			[ "$(cat /proc/$p/comm 2>/dev/null)" = pihole-FTL ] &&
			[ -n "$(dig +short +time=1 +tries=1 @127.0.0.1 pi.hole)" ] && echo "$p"' || true)
		[ -z "$pid" ] || [ "$pid" = "$old" ] || break
		[ $SECONDS -lt $until ] || die "pihole-FTL not answering (PID file '$pid', not '$old')"
		sleep 1
	done
	echo "$pid"
}

# ftl_log_mark prints the size of FTL's log in bytes: the offset after which
# ftl_settle reads what a command made FTL log.
ftl_log_mark() { cx 'stat -c %s /var/log/pihole/FTL.log'; }

# ftl_settle MARK [reload] waits, in the container, until FTL has finished
# reloading its lists, so the next command may write gravity.db. FTL v5.25.2
# re-reads gravity.db on its database thread after a `pihole restartdns
# reload-lists` (which `pihole -g` ends with) or a start, and a gravity.db
# commit meanwhile fails that reload and leaves FTL answering pi.hole only
# (PROVENANCE "FTL RELOAD RACE", the race WI 5.6's barrier closes in the
# product). It returns once, after MARK (an ftl_log_mark made before the
# command), with `reload` given: FTL logged the end of a reload's regex
# compilation ("Compiled N whitelist and N blacklist regex filters", the last
# step it logs before its quiet reads); and the log has not grown for 2 s;
# and FTL answers a nexus-probe-*.invalid name NXDOMAIN, which a stuck FTL
# does not. It dies after LISTS_TIMEOUT s, with the log's tail.
ftl_settle() {
	local mark=$1 reload=${2:-}
	# shellcheck disable=SC2016 # Expanded in the container.
	cx "mark=$mark want=$reload limit=$LISTS_TIMEOUT"'
		log=/var/log/pihole/FTL.log
		end=$((SECONDS + limit)) last=-1 since=$SECONDS
		while :; do
			size=$(stat -c %s "$log" 2>/dev/null || echo 0)
			if [ "$size" != "$last" ]; then last=$size since=$SECONDS; fi
			ok=1
			if [ -n "$want" ]; then
				tail -c +$((mark + 1)) "$log" |
					grep -Eq "Compiled [0-9]+ whitelist and [0-9]+ blacklist regex filters for" || ok=0
			fi
			# SECONDS counts whole seconds: 3 apart is at least 2 s of quiet.
			if [ $ok = 1 ] && [ $((SECONDS - since)) -ge 3 ] &&
				dig +time=2 +tries=1 @127.0.0.1 "nexus-probe-$RANDOM$RANDOM.invalid" 2>&1 | grep -q "status: NXDOMAIN"; then
				exit 0
			fi
			[ $SECONDS -lt $end ] || { echo "FTL has not settled its lists after ${limit}s; log tail:" >&2; tail -n 12 "$log" >&2; exit 1; }
			sleep 0.5
		done' || die "FTL did not finish reloading its lists"
}

# pihole_setup is the one-time setup of the Pi-hole harness, after the
# required units are active and before any test (WI 5.1). Offline gravity
# (A-33): gravity.sh's gravity_CheckDNSResolutionAvailable looks up
# raw.githubusercontent.com through /etc/resolv.conf whenever
# /etc/pihole/local.list is non-empty, as it is here and on a real host, and
# exits 1 after 120 s without an answer. local.list is left alone; the stub
# upstream answers that name instead. Every step's result is checked, and
# pihole -g's output goes to pihole-setup.log with the other logs.
pihole_setup() {
	local pid0 pid1 pid2 out flags updated0 updated1 g rc lists mark setup_mark locked
	local stock=https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
	local baseline=http://127.0.0.1:8099/baseline.txt
	local db=/etc/pihole/gravity.db
	say "versions: $(cx 'pihole -v | xargs; echo "INIT $(cat /etc/pihole-spike-init);"; systemctl show -p FragmentPath -p Type pihole-FTL | xargs; systemctl --version | head -1' | xargs)"
	pid0=$(ftl_pid)
	say "pihole-FTL PID $pid0 answers on 127.0.0.1:53"
	ftl_settle "$(ftl_log_mark)"
	setup_mark=$(ftl_log_mark)
	out=$(cx 'dig +short @127.0.0.1 -p 5353 www.xi.example A')
	[ "$out" = 203.0.113.10 ] || die "stub upstream: www.xi.example gave '$out'"
	say "stub upstream: www.xi.example A $out"
	cx "curl -fsS $baseline >/dev/null" || die "fixture-http does not serve $baseline"
	say "fixture-http serves $baseline"

	# 1. Docker writes /etc/resolv.conf at container start through a bind
	# mount, so the image cannot carry it: truncate and write in place,
	# never rename over the mount.
	cx 'printf "nameserver 127.0.0.1\n" >/etc/resolv.conf'
	out=$(cx 'grep nameserver /etc/resolv.conf')
	[ "$out" = "nameserver 127.0.0.1" ] || die "/etc/resolv.conf nameservers: $out"
	say "resolv.conf: $out"

	# The FR-23 seven-line fixture as the dnsFile, loaded by setdns's restart.
	docker cp internal/providers/pihole/dnsfile/testdata/fr23-seven-lines.conf \
		"$container:/etc/dnsmasq.d/03-custom-dns-names.conf"
	cx 'chown root:root /etc/dnsmasq.d/03-custom-dns-names.conf && chmod 0644 /etc/dnsmasq.d/03-custom-dns-names.conf'
	say "seeded /etc/dnsmasq.d/03-custom-dns-names.conf with the FR-23 fixture ($(cx 'wc -l </etc/dnsmasq.d/03-custom-dns-names.conf') lines)"

	# 2. v5's own writer sets PIHOLE_DNS_1, regenerates 01-pihole.conf and
	# restarts FTL. Its later arguments reset DNS_FQDN_REQUIRED,
	# DNS_BOGUS_PRIV and DNSSEC to false unless given, so the image's
	# current values are passed back.
	# shellcheck disable=SC2016 # Expanded in the container.
	flags=$(cx '. /etc/pihole/setupVars.conf
		[ "${REV_SERVER:-false}" = false ] || { echo "REV_SERVER=$REV_SERVER is not supported" >&2; exit 1; }
		echo "$([ "${DNS_FQDN_REQUIRED:-}" = true ] && echo domain-needed || echo off)" \
			"$([ "${DNS_BOGUS_PRIV:-}" = true ] && echo bogus-priv || echo off)" \
			"$([ "${DNSSEC:-}" = true ] && echo dnssec || echo off)"') || die "setupVars.conf"
	mark=$(ftl_log_mark)
	cx "pihole -a setdns '127.0.0.1#5353' $flags" || die "pihole -a setdns 127.0.0.1#5353 $flags failed"
	pid1=$(ftl_pid "$pid0")
	ftl_settle "$mark" reload
	say "pihole -a setdns 127.0.0.1#5353 $flags: FTL PID $pid0 -> $pid1"
	out=$(cx "grep '^PIHOLE_DNS_' /etc/pihole/setupVars.conf")
	[ "$out" = "PIHOLE_DNS_1=127.0.0.1#5353" ] || die "setupVars.conf PIHOLE_DNS_ lines: $out"
	say "setupVars.conf: $out"
	out=$(cx "grep '^server=' /etc/dnsmasq.d/01-pihole.conf")
	[ "$out" = "server=127.0.0.1#5353" ] || die "01-pihole.conf server= lines: $out"
	say "01-pihole.conf: $out"
	out=$(cx 'getent hosts raw.githubusercontent.com' | xargs)
	[ "$out" = "192.0.2.1 raw.githubusercontent.com" ] || die "getent hosts raw.githubusercontent.com: '$out'"
	say "getent hosts raw.githubusercontent.com: $out"

	# 3. The local baseline list replaces the image's default adlist. The
	# default cannot be deleted yet: its 165,493 gravity rows reference it
	# without ON DELETE CASCADE, pihole-FTL sqlite3 enforces foreign keys, and
	# `pihole -a adlist del` exits 0 with the row still there (PROVENANCE).
	# Disabled, gravity neither downloads nor keeps it, and it is deleted
	# after the run.
	# Each of them writes gravity.db, which FTL's start-up reload just read.
	cx "pihole -a adlist disable '$stock' && pihole -a adlist add '$baseline' harness-baseline"
	ftl_settle "$(ftl_log_mark)"
	updated0=$(cx "pihole-FTL sqlite3 $db \"SELECT value FROM info WHERE property = 'updated'\"")

	# 4. One gravity run, against loopback lists only.
	g=$LOGS/pihole-setup.log
	mark=$(ftl_log_mark)
	set +e
	cx 'pihole -g' >"$g" 2>&1
	rc=$?
	set -e
	updated1=$(cx "pihole-FTL sqlite3 $db \"SELECT value FROM info WHERE property = 'updated'\"")
	say "pihole -g exit $rc; info.updated $updated0 -> $updated1; $(grep -o 'Number of gravity domains: [0-9]*' "$g" || true)"
	[ $rc -eq 0 ] || die "pihole -g exited $rc (${g#"$ROOT"/})"
	! grep -q 'DNS resolution is not available' "$g" || die "pihole -g: DNS resolution is not available"
	[ "$updated1" -gt "$updated0" ] || die "pihole -g did not advance info.updated"
	# pihole -g ended with a list reload; deleting the adlist writes
	# gravity.db, so FTL must have finished that reload first.
	ftl_settle "$mark" reload
	cx "pihole -a adlist del '$stock'"
	ftl_settle "$(ftl_log_mark)"
	lists=$(cx "pihole-FTL sqlite3 $db 'SELECT address, enabled, status, number FROM adlist'")
	say "adlist (address|enabled|status|number): $(echo "$lists" | xargs)"
	[ "$lists" = "$baseline|1|1|3" ] || die "adlist table is not only the baseline list, enabled at status 1"
	say "gravity_count $(cx "pihole-FTL sqlite3 $db \"SELECT value FROM info WHERE property = 'gravity_count'\"")"
	pid2=$(ftl_pid)
	# No setup step may have raced a list reload.
	locked=$(cx "tail -c +$((setup_mark + 1)) /var/log/pihole/FTL.log | grep -c 'database is locked' || true")
	say "FTL.log lines 'database is locked' since the setup began: $locked"
	[ "$locked" = 0 ] || die "FTL logged $locked 'database is locked' lines during setup: a gravity.db write raced a list reload"
	say "pihole-FTL PID $pid2 answers on 127.0.0.1:53; setup done"
}

say "starting container $name"
# Anonymous volumes, not bind mounts, for both stores of the nested Engine:
# Docker 29 keeps images and snapshots in containerd's store, and overlayfs
# refuses an upper directory on the container's own overlay root. The
# Alpine harness has a private cgroup namespace: its cgroup v2 root is the
# container's own, writable, so OpenRC's openrc.<service> groups and the
# process backend's nexus-ops groups are real.
cgroupns=host
[ $MODE = systemd ] || cgroupns=private
# /harness-tmp is the parity suites' TMPDIR: they fsync thousands of small
# records, and on the VM's shared disk one fsync stalled a run for minutes.
container=$(docker run -d --name "$name" --privileged "--cgroupns=$cgroupns" \
	--tmpfs /run --tmpfs /run/lock --tmpfs /harness-tmp:rw,exec,nosuid,size=2g,mode=1777 \
	-v /var/lib/docker -v /var/lib/containerd "$tag")

mounts=$(docker inspect -f '{{range .Mounts}}{{.Type}}:{{.Destination}} {{end}}' "$container")
say "mounts: ${mounts:-none}"
case " $mounts" in
*" bind:"*) die "a bind mount is present: $mounts" ;;
esac

if [ $MODE = openrc ]; then
	say "waiting for OpenRC: docker started in the default runlevel"
	deadline=$((SECONDS + READY_TIMEOUT))
	until docker exec "$container" rc-status default 2>/dev/null | grep -Eq '^ docker +\[ +started'; do
		[ $SECONDS -lt $deadline ] || die "docker not started in the default runlevel after ${READY_TIMEOUT}s"
		sleep 1
	done
	until docker exec "$container" docker info >/dev/null 2>&1; do
		[ $SECONDS -lt $deadline ] || die "the nested Engine does not answer"
		sleep 1
	done
	say "rc-status default: $(docker exec "$container" rc-status default | tr -s ' ' | xargs)"
	say "PID 1: $(docker exec "$container" cat /proc/1/cmdline | tr '\0' ' ')"
	say "versions: $(docker exec "$container" sh -c 'openrc --version; nginx -v 2>&1; curl --version | head -1; docker version --format "docker {{.Server.Version}}"; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"
else
	say "waiting for systemd (running or degraded)"
	deadline=$((SECONDS + READY_TIMEOUT))
	while :; do
		state=$(docker exec "$container" systemctl is-system-running 2>/dev/null || true)
		case $state in
		running | degraded) break ;;
		esac
		[ $SECONDS -lt $deadline ] || die "systemd not ready after ${READY_TIMEOUT}s (state '$state')"
		sleep 1
	done
	say "system state: $state"
	failed=$(docker exec "$container" systemctl --failed --no-legend --plain 2>/dev/null | awk '{print $1}' | xargs)
	say "failed units: ${failed:-none}"
	for u in $REQUIRED_UNITS; do
		until [ "$(docker exec "$container" systemctl is-active "$u" 2>/dev/null || true)" = active ]; do
			[ $SECONDS -lt $deadline ] || die "required unit $u is not active"
			sleep 1
		done
		say "required unit $u active"
	done
	if [ -n "$PIHOLE" ]; then
		pihole_setup
	else
		say "versions: $(docker exec "$container" sh -c 'systemctl --version | head -1; needrestart --version 2>&1 | head -1; dpkg-query -W -f="apt \${Version} " apt; dpkg-query -W -f="dpkg \${Version}" dpkg; echo; nginx -v 2>&1; curl --version | head -1; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"
	fi
fi

say "copying binaries, unit, lib.sh and fixtures in with docker cp"
docker exec "$container" mkdir -p /harness/deploy
docker cp "$OUT/nexusd" "$container:/usr/local/bin/nexusd"
docker cp "$OUT/opsd" "$container:/usr/local/bin/opsd"
docker cp "$OUT/integration.test" "$container:/harness/integration.test"
docker cp deploy/nexusd.service "$container:/harness/deploy/nexusd.service"
docker cp deploy/nexusd.openrc "$container:/harness/deploy/nexusd.openrc"
docker cp test/harness/lib.sh "$container:/harness/lib.sh"
# Script fixtures (WI 5.3); tests install them root-owned where they run.
docker cp test/harness/fixtures "$container:/harness/fixtures"
# The svcctl captures the svcctltest fake answers with (TC-113).
docker exec "$container" mkdir -p /harness/src/github.com/mauktik/nexus/internal/svcctl
docker cp internal/svcctl/testdata "$container:/harness/src/github.com/mauktik/nexus/internal/svcctl/testdata"
# The storage captures the T1 fake zpool and zfs replay, and capture.sh
# (storage-provider WI 5.3, TC-STG-220 to TC-STG-230).
docker exec "$container" mkdir -p /harness/src/github.com/mauktik/nexus/internal/providers/storage
docker cp internal/providers/storage/testdata "$container:/harness/src/github.com/mauktik/nexus/internal/providers/storage/testdata"
# The release tree install.sh installs from (WI 10.1), root-owned as tar
# extracts it for root.
docker cp "$OUT/release" "$container:/harness/release"
docker exec "$container" chown -R root:root /harness/release
check_host_key
(cd "$FIX/pkg" && ls -l) | sed 's/^/harness: fixture file: /'
say "copying the self-update fixtures into /harness/selfupdate"
docker cp "$FIX/pkg" "$container:/harness/selfupdate"
docker exec "$container" chown -R root:root /harness/selfupdate
docker exec "$container" sh -c 'chown root:root /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh /harness/deploy/nexusd.service /harness/deploy/nexusd.openrc && chmod 0755 /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh'

# The parity suites (NFR-8, TC-46): as root, then as an unprivileged user,
# each package's binary in a directory of its own holding its testdata
# (internal/detach's goldens, internal/ops's test sources).
# NEXUS_TEST_SUPERVISOR is the supervisor executable of their process
# backend: the harness has no Go toolchain to build one.
parity_status=0
# The one test left out reads the package's API with `go doc`: it needs
# the Go toolchain, so it runs in T0 only, and the cases it covers are not
# root, cgroup or init specific.
parity_skip() {
	case $1 in
	detach) echo '^TestSingleConstructorWithTestInjection$' ;;
	*) echo '^$' ;;
	esac
}
run_parity() {
	local who=$1 pkg log rc skip
	for pkg in detach ops; do
		skip=$(parity_skip "$pkg")
		log=$pkg.log
		[ "$who" = root ] || log=$pkg-nonroot.log
		say "parity suite internal/$pkg as $who -> $log"
		set +e
		if [ "$who" = root ]; then
			docker exec -w "/harness/parity/$pkg" -e NEXUS_TEST_SUPERVISOR=/usr/local/bin/nexusd -e TMPDIR=/harness-tmp/root \
				"$container" "/harness/parity/$pkg/$pkg.test" -test.v -test.count=1 -test.timeout=20m \
				-test.skip "$skip" >"$LOGS/$log" 2>&1
		else
			docker exec -w "/harness/parity/$pkg" "$container" setpriv --reuid=nexusparity --regid=nexusparity \
				--clear-groups env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
				HOME=/harness-tmp/nexusparity TMPDIR=/harness-tmp/nexusparity NEXUS_TEST_SUPERVISOR=/usr/local/bin/nexusd \
				"/harness/parity/$pkg/$pkg.test" -test.v -test.count=1 -test.timeout=20m -test.skip "$skip" \
				>"$LOGS/$log" 2>&1
		fi
		rc=$?
		set -e
		say "parity suite internal/$pkg as $who exited $rc: $(grep -c -- '^--- PASS\|^    --- PASS' "$LOGS/$log") PASS, $(grep -c -- '--- SKIP' "$LOGS/$log") SKIP, $(grep -c -- '--- FAIL' "$LOGS/$log") FAIL"
		if [ $rc -ne 0 ]; then
			tail -n 40 "$LOGS/$log" >&2
			parity_status=$rc
		fi
	done
}
if [ "$PARITY" = 1 ]; then
	say "copying the parity suites in"
	docker exec "$container" mkdir -p /harness/parity/detach /harness/parity/ops /harness/logs
	docker cp "$OUT/detach.test" "$container:/harness/parity/detach/detach.test"
	docker cp "$OUT/ops.test" "$container:/harness/parity/ops/ops.test"
	docker cp internal/detach/testdata "$container:/harness/parity/detach/testdata"
	# TC-25 parses the ops behavioral suites' source to check that each runs
	# per backend.
	for f in internal/ops/*_test.go; do
		docker cp "$f" "$container:/harness/parity/ops/${f##*/}"
	done
	if [ $MODE = openrc ]; then
		docker exec "$container" adduser -D -H -s /sbin/nologin nexusparity
	else
		docker exec "$container" useradd -M -s /usr/sbin/nologin nexusparity
	fi
	docker exec "$container" sh -c 'chown -R root:root /harness/parity && chmod -R a+rX /harness/parity &&
		install -d -m 0700 /harness-tmp/root && install -d -o nexusparity -g nexusparity -m 0700 /harness-tmp/nexusparity'
	run_parity root
	run_parity nonroot
fi

say "running tests"
docker cp "$LOGS/run.log" "$container:/harness/run.log"
# The Ubuntu suite outgrew 30m with the nginx T1 suite (WI 7.1): at 56c8848
# it reached 1,714 s of finished tests before nginx_test.go ran.
args=(-test.v -test.count=1 -test.timeout=60m)
[ -z "$RUN" ] || args+=(-test.run "$RUN")
# TC-46 reads the whole test.log, so it runs after this, on its own. The
# Pi-hole tests need the Pi-hole image (make test-harness-pihole), so every
# other image skips them; -test.skip takes one pattern.
skip='^TestParitySuitesRanAsRoot$'
[ -n "$PIHOLE" ] || skip="$skip|^TestPihole"
args+=(-test.skip "$skip")
# shellcheck disable=SC2206 # TESTFLAGS is word-split on purpose.
[ -z "$TESTFLAGS" ] || args+=($TESTFLAGS)
set +e
if [ -n "$PIHOLE" ]; then
	docker exec -w /harness "$container" "${CENV[@]}" NEXUS_HARNESS=1 "NEXUS_HARNESS_ARCH=$goarch" \
		"NEXUS_HARNESS_EXPECT_FAIL=${NEXUS_HARNESS_EXPECT_FAIL:-}" \
		/harness/integration.test "${args[@]}" 2>&1 | tee "$LOGS/test.log"
else
	docker exec -w /harness -e NEXUS_HARNESS=1 -e "NEXUS_HARNESS_ARCH=$goarch" \
		-e "NEXUS_HARNESS_EXPECT_FAIL=${NEXUS_HARNESS_EXPECT_FAIL:-}" \
		"$container" /harness/integration.test "${args[@]}" 2>&1 | tee "$LOGS/test.log"
fi
status=${PIPESTATUS[0]}
set -e
# A skipped test is a green exit 0 that proves nothing (TC-159 skips when it
# cannot make FTL stuck); say so loudly, and do not fail the run.
if grep -q '^--- SKIP:' "$LOGS/test.log"; then
	say "WARNING: $(grep -c '^--- SKIP:' "$LOGS/test.log") test(s) SKIPPED, not verified on this run:"
	grep '^--- SKIP:' "$LOGS/test.log" | sed 's/^/    /' >&2
fi
say "tests exited $status"

if [ "$PARITY" = 1 ]; then
	say "checking the parity logs and test.log skips (TC-46)"
	for f in detach.log ops.log detach-nonroot.log ops-nonroot.log test.log; do
		docker cp "$LOGS/$f" "$container:/harness/logs/$f"
	done
	set +e
	docker exec -w /harness -e NEXUS_HARNESS=1 -e "NEXUS_HARNESS_ARCH=$goarch" -e NEXUS_HARNESS_LOGS=/harness/logs \
		"$container" /harness/integration.test -test.v -test.count=1 -test.run '^TestParitySuitesRanAsRoot$' 2>&1 |
		tee "$LOGS/parity-check.log"
	check=${PIPESTATUS[0]}
	set -e
	say "parity check exited $check, parity suites $parity_status"
	[ $status -ne 0 ] || status=$check
	[ $status -ne 0 ] || status=$parity_status
fi

if [ "$PARITY" = 1 ]; then
	say "svcctl TestLiveMatrix against the container's nginx ($MODE) -> svcctl-live.log"
	set +e
	docker cp "$OUT/svcctl.test" "$container:/harness/parity/svcctl.test"
	docker exec -w /harness/parity -e NEXUS_SVCCTL_LIVE=1 "$container" /harness/parity/svcctl.test \
		-test.v -test.count=1 -test.run '^TestLiveMatrix$' >"$LOGS/svcctl-live.log" 2>&1
	live=$?
	set -e
	cat "$LOGS/svcctl-live.log"
	if [ $live -ne 0 ] || ! grep -q '^--- PASS: TestLiveMatrix' "$LOGS/svcctl-live.log"; then
		say "svcctl TestLiveMatrix did not pass (exit $live)"
		[ $status -ne 0 ] || status=1
	fi
fi
