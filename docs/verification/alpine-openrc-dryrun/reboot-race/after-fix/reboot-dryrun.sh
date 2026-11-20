#!/bin/bash
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# Reboot-race dry run, after the fix: the Alpine OpenRC harness container
# with nexusd from this tree installed by its release install.sh, then
# test/live/alpine-check.sh --reboot-phase1, `docker restart -t 60`, and
# --reboot-phase2, ROUNDS times. Transcripts and the op dirs go to $OUTDIR.
set -euo pipefail
REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
OUTDIR=$1
ROUNDS=${ROUNDS:-3}
NAME_PREFIX=${NAME_PREFIX:-nx-reboot}
cd "$REPO"
mkdir -p "$OUTDIR"
BUILD=$(mktemp -d "${TMPDIR:-/tmp}/nx-reboot-build.XXXXXX")
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
name="$NAME_PREFIX-alpine-$STAMP"
SETUP="$OUTDIR/setup.log"
: >"$SETUP"
log() { echo "+ $*" | tee -a "$SETUP"; }

cleanup() {
	docker rm -f -v "$name" >/dev/null 2>&1 || true
	rm -rf "$BUILD"
	if [ -n "$(docker ps -aq --filter "name=^${name}\$")" ]; then
		echo "FAIL: container $name not removed" >&2
	else
		echo "container $name removed" | tee -a "$SETUP"
	fi
}
trap cleanup EXIT

arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64 | arm64) goarch=arm64 ;; x86_64 | amd64) goarch=amd64 ;; *) echo "arch $arch" >&2; exit 1 ;; esac
log "git rev-parse HEAD: $(git rev-parse HEAD)"
log "git status --short -- internal/detach internal/ops: $(git status --short -- internal/detach internal/ops | tr '\n' ' ')"
log "docker daemon architecture $arch -> GOARCH=$goarch"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -o "$BUILD/nexusd" ./cmd/nexusd
make --no-print-directory -s nexusd-tree TREE_BIN="$BUILD/nexusd" TREE_DIR="$BUILD/release/nexusd"
log "nexusd sha256 $(shasum -a 256 "$BUILD/nexusd" | cut -d' ' -f1)"
log "docker build -f test/harness/Dockerfile.alpine -t nexus-harness:alpine-alpine-latest test/harness"
docker build --quiet --build-arg BASE=alpine:latest -f test/harness/Dockerfile.alpine -t nexus-harness:alpine-alpine-latest test/harness | tee -a "$SETUP"
log "docker run -d --name $name --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock -v /var/lib/docker -v /var/lib/containerd nexus-harness:alpine-alpine-latest"
docker run -d --name "$name" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
	-v /var/lib/docker -v /var/lib/containerd nexus-harness:alpine-alpine-latest | tee -a "$SETUP"

wait_ready() {
	local deadline=$((SECONDS + 180))
	until docker exec "$name" rc-status default 2>/dev/null | grep -Eq '^ docker +\[ +started'; do
		[ $SECONDS -lt $deadline ] || { echo "OpenRC not ready" >&2; exit 1; }
		sleep 1
	done
}
wait_ready
log "PID 1: $(docker exec "$name" cat /proc/1/cmdline | tr '\0' ' ')"
log "$(docker exec "$name" sh -c 'cat /etc/alpine-release; openrc --version; uname -m' | xargs)"
docker cp "$BUILD/release" "$name:/harness-release"
docker cp test/live/alpine-check.sh "$name:/root/alpine-check.sh"
docker exec "$name" sh -c 'cd /harness-release/nexusd && ./install.sh' 2>&1 | tee -a "$SETUP"
docker exec "$name" sh -c 'rc-update add nginx default && rc-service nginx start && rc-update add nexusd default && rc-service nexusd start' 2>&1 | tee -a "$SETUP"
TOKEN=$(docker exec "$name" /usr/local/bin/nexusd token create --name alpine-check --scope scripts:read --scope scripts:run --expires 1d 2>>"$SETUP")
[ -n "$TOKEN" ] || { echo "no token" >&2; exit 1; }
log "token created (value withheld)"

for r in $(seq 1 "$ROUNDS"); do
	log "== round $r"
	docker exec -e NEXUS_TOKEN="$TOKEN" "$name" sh /root/alpine-check.sh --yes --reboot-phase1 --transcript "/root/r$r-phase1.txt" >/dev/null 2>&1 || log "round $r phase 1 exit $?"
	op=$(docker exec "$name" sh -c 'sed -n "s/^op=//p" /var/lib/nexus-alpine-check/reboot.state')
	log "round $r op $op; $(date -u +%T) docker restart -t 60 $name (simulated reboot)"
	docker restart -t 60 "$name" >/dev/null
	wait_ready
	log "round $r back $(date -u +%T): $(docker exec "$name" rc-status default | tr -s ' ' | xargs)"
	docker exec -e NEXUS_TOKEN="$TOKEN" "$name" sh /root/alpine-check.sh --yes --reboot-phase2 --transcript "/root/r$r-phase2.txt" >/dev/null 2>&1 || log "round $r phase 2 exit $?"
	{
		echo "CONTAINER DRY RUN, NOT real-host evidence. Round $r, operation $op, after the fix."
		echo "\$ ls -la /var/lib/nexus/ops/$op"
		docker exec "$name" ls -la "/var/lib/nexus/ops/$op" 2>&1 || true
		for f in exit.json cancel supervisor.json supervisor.log; do
			echo "== $f"
			docker exec "$name" cat "/var/lib/nexus/ops/$op/$f" 2>&1 || true
			echo
		done
		echo "== docker logs (BusyBox init's shutdown lines, last restart)"
		docker logs "$name" 2>&1 | grep -i 'going down\|Sent SIG\|Requesting' | tail -4
	} >"$OUTDIR/after-fix-round$r-op-dir.txt"
	docker cp "$name:/root/r$r-phase1.txt" "$OUTDIR/after-fix-round$r-phase1.txt"
	docker cp "$name:/root/r$r-phase2.txt" "$OUTDIR/after-fix-round$r-phase2.txt"
	log "round $r: $(grep -E '^  5a ' "$OUTDIR/after-fix-round$r-phase2.txt" || echo 'no 5a line')"
done
for f in "$OUTDIR"/*; do
	if grep -qF "$TOKEN" "$f"; then
		echo "FAIL: token found in $f" >&2
		exit 1
	fi
done
log "token absent from every collected file (grep -F)"
