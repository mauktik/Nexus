#!/bin/bash
# T1 harness runner: `make test-harness [BASE=ubuntu:latest] [RUN=regex]`
# (architecture.md, Verification Architecture). `run.sh emu` (make test-emu)
# is the T1-emu smoke of the release artifact instead; see emu_smoke.
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
# Environment: BASE (image base), RUN (-test.run regex), TESTFLAGS (more
# test binary flags), NEXUS_HARNESS_EXPECT_FAIL (passed to the tests: a
# deliberate failure that proves logs are collected and the container is
# removed when a test fails), NEXUS_HARNESS_ALLOW_REMOTE=1 (run on a
# non-local Docker endpoint anyway: only for a disposable test host).
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
BASE=${BASE:-ubuntu:latest}
RUN=${RUN:-}
TESTFLAGS=${TESTFLAGS:-}
OUT=$ROOT/build/harness
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LOGS=$OUT/logs/$STAMP
READY_TIMEOUT=${READY_TIMEOUT:-180}
REQUIRED_UNITS="systemd-journald.service dbus.service docker.service"

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
}
docker_guard

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
say "docker context $(docker context show), daemon architecture $arch -> GOARCH=$goarch, BASE=$BASE"

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
tag="nexus-harness:$(echo "$BASE" | tr ':/@' '---')"
say "building image $tag"
docker build --quiet --build-arg "BASE=$BASE" -t "$tag" test/harness >/dev/null
say "building linux/$goarch test-only binaries into build/harness/"
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
go build -trimpath -o "$OUT/nexusd" ./cmd/nexusd
go build -trimpath -o "$OUT/opsd" ./test/integration/cmd/opsd
go test -c -trimpath -tags integration -o "$OUT/integration.test" ./test/integration
say "building the self-update fixtures ($FIXTURES) into ${FIX#"$ROOT"/}"
build_fixtures
unset CGO_ENABLED GOOS GOARCH
make --no-print-directory -s nexusd-tree TREE_BIN="$OUT/nexusd" TREE_DIR="$OUT/release/nexusd"
go build -o "$OUT/host/nexus-release" ./cmd/nexus-release
sign_fixtures "$OUT/host/nexus-release"

name="nexus-harness-$STAMP-$$"
container=""
status=1

collect_and_remove() {
	local rc=$?
	if [ -n "$container" ]; then
		say "collecting logs into ${LOGS#"$ROOT"/}"
		docker exec "$container" journalctl -b --no-pager -o short-precise >"$LOGS/journal.log" 2>&1 || true
		docker exec "$container" systemctl list-units --all --no-pager --plain >"$LOGS/units.log" 2>&1 || true
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

say "starting container $name"
# Anonymous volumes, not bind mounts, for both stores of the nested Engine:
# Docker 29 keeps images and snapshots in containerd's store, and overlayfs
# refuses an upper directory on the container's own overlay root.
container=$(docker run -d --name "$name" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock -v /var/lib/docker -v /var/lib/containerd "$tag")

mounts=$(docker inspect -f '{{range .Mounts}}{{.Type}}:{{.Destination}} {{end}}' "$container")
say "mounts: ${mounts:-none}"
case " $mounts" in
*" bind:"*) die "a bind mount is present: $mounts" ;;
esac

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
say "versions: $(docker exec "$container" sh -c 'systemctl --version | head -1; needrestart --version 2>&1 | head -1; dpkg-query -W -f="apt \${Version} " apt; dpkg-query -W -f="dpkg \${Version}" dpkg; echo; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"

say "copying binaries, unit, lib.sh and fixtures in with docker cp"
docker exec "$container" mkdir -p /harness/deploy
docker cp "$OUT/nexusd" "$container:/usr/local/bin/nexusd"
docker cp "$OUT/opsd" "$container:/usr/local/bin/opsd"
docker cp "$OUT/integration.test" "$container:/harness/integration.test"
docker cp deploy/nexusd.service "$container:/harness/deploy/nexusd.service"
docker cp test/harness/lib.sh "$container:/harness/lib.sh"
# Script fixtures (WI 5.3); tests install them root-owned where they run.
docker cp test/harness/fixtures "$container:/harness/fixtures"
# The release tree install.sh installs from (WI 10.1), root-owned as tar
# extracts it for root.
docker cp "$OUT/release" "$container:/harness/release"
docker exec "$container" chown -R root:root /harness/release
check_host_key
(cd "$FIX/pkg" && ls -l) | sed 's/^/harness: fixture file: /'
say "copying the self-update fixtures into /harness/selfupdate"
docker cp "$FIX/pkg" "$container:/harness/selfupdate"
docker exec "$container" chown -R root:root /harness/selfupdate
docker exec "$container" sh -c 'chown root:root /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh /harness/deploy/nexusd.service && chmod 0755 /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh'

say "running tests"
docker cp "$LOGS/run.log" "$container:/harness/run.log"
args=(-test.v -test.count=1 -test.timeout=30m)
[ -z "$RUN" ] || args+=(-test.run "$RUN")
# shellcheck disable=SC2206 # TESTFLAGS is word-split on purpose.
[ -z "$TESTFLAGS" ] || args+=($TESTFLAGS)
set +e
docker exec -w /harness -e NEXUS_HARNESS=1 -e "NEXUS_HARNESS_ARCH=$goarch" \
	-e "NEXUS_HARNESS_EXPECT_FAIL=${NEXUS_HARNESS_EXPECT_FAIL:-}" \
	"$container" /harness/integration.test "${args[@]}" 2>&1 | tee "$LOGS/test.log"
status=${PIPESTATUS[0]}
set -e
say "tests exited $status"
