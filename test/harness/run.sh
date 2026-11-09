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
# The Docker CLI's current context is used as is: no socket is hardcoded.
#
# Environment: BASE (image base), RUN (-test.run regex), TESTFLAGS (more
# test binary flags), NEXUS_HARNESS_EXPECT_FAIL (passed to the tests: a
# deliberate failure that proves logs are collected and the container is
# removed when a test fails).
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

dist_before=$(dist_state)
mkdir -p "$OUT" "$LOGS"

say "building linux/$goarch test-only binaries into build/harness/"
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
go build -trimpath -o "$OUT/nexusd" ./cmd/nexusd
go build -trimpath -o "$OUT/opsd" ./test/integration/cmd/opsd
go test -c -trimpath -tags integration -o "$OUT/integration.test" ./test/integration
unset CGO_ENABLED GOOS GOARCH
make --no-print-directory -s nexusd-tree TREE_BIN="$OUT/nexusd" TREE_DIR="$OUT/release/nexusd"

tag="nexus-harness:$(echo "$BASE" | tr ':/@' '---')"
say "building image $tag"
docker build --quiet --build-arg "BASE=$BASE" -t "$tag" test/harness >/dev/null

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
docker exec "$container" sh -c 'chown root:root /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh /harness/deploy/nexusd.service && chmod 0755 /usr/local/bin/nexusd /usr/local/bin/opsd /harness/integration.test /harness/lib.sh'

say "running tests"
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
