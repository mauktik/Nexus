#!/bin/bash
# T1 harness runner: `make test-harness [BASE=ubuntu:latest] [RUN=regex]`
# (architecture.md, Verification Architecture).
#
# Builds the image FROM $BASE, cross-compiles nexusd, opsd and the
# integration test binary for the architecture `docker info` reports into
# build/harness/ (never dist/), starts a privileged systemd container with
# the exact flags, waits for readiness (running or degraded, then the
# required units), copies everything in with docker cp (no bind mount), runs
# the tests, and always collects the logs and removes the container.
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

dist_before=$(dist_state)
mkdir -p "$OUT" "$LOGS"

say "building linux/$goarch test-only binaries into build/harness/"
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
go build -trimpath -o "$OUT/nexusd" ./cmd/nexusd
go build -trimpath -o "$OUT/opsd" ./test/integration/cmd/opsd
go test -c -trimpath -tags integration -o "$OUT/integration.test" ./test/integration
unset CGO_ENABLED GOOS GOARCH

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
container=$(docker run -d --name "$name" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock -v /var/lib/docker "$tag")

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
say "versions: $(docker exec "$container" sh -c 'systemctl --version | head -1; needrestart --version 2>&1 | head -1; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"

say "copying binaries, unit and lib.sh in with docker cp"
docker exec "$container" mkdir -p /harness/deploy
docker cp "$OUT/nexusd" "$container:/usr/local/bin/nexusd"
docker cp "$OUT/opsd" "$container:/usr/local/bin/opsd"
docker cp "$OUT/integration.test" "$container:/harness/integration.test"
docker cp deploy/nexusd.service "$container:/harness/deploy/nexusd.service"
docker cp test/harness/lib.sh "$container:/harness/lib.sh"
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
