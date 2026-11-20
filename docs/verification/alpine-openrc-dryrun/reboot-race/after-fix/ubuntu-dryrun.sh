#!/bin/bash
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# Ubuntu (systemd) reboot-race dry run, after the fix: nexusd from this
# tree installed by its release install.sh in the systemd harness image, a
# scripts op that sleeps an hour, then `docker restart -t 60` (STOPSIGNAL
# SIGRTMIN+3: systemd halts, stopping nexus-op-<id>.service with SIGTERM),
# then the op's status once nexusd is back. ROUNDS times.
set -euo pipefail
REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
OUTDIR=$1
ROUNDS=${ROUNDS:-3}
NAME_PREFIX=${NAME_PREFIX:-nx-reboot}
cd "$REPO"
mkdir -p "$OUTDIR"
BUILD=$(mktemp -d "${TMPDIR:-/tmp}/nx-reboot-ubuntu.XXXXXX")
name="$NAME_PREFIX-ubuntu-$(date -u +%Y%m%dT%H%M%SZ)"
T="$OUTDIR/ubuntu-after-fix.txt"
: >"$T"
log() { echo "$*" | tee -a "$T"; }
cleanup() {
	docker rm -f -v "$name" >/dev/null 2>&1 || true
	rm -rf "$BUILD"
	[ -z "$(docker ps -aq --filter "name=^${name}\$")" ] && echo "container $name removed" | tee -a "$T"
}
trap cleanup EXIT

arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64 | arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac
log "# CONTAINER DRY RUN, NOT real-host evidence. Ubuntu systemd harness, simulated reboot by docker restart."
log "# git HEAD $(git rev-parse HEAD), GOARCH=$goarch"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -o "$BUILD/nexusd" ./cmd/nexusd
make --no-print-directory -s nexusd-tree TREE_BIN="$BUILD/nexusd" TREE_DIR="$BUILD/release/nexusd"
docker build --quiet --build-arg BASE=ubuntu:latest -f test/harness/Dockerfile -t nexus-harness:ubuntu-latest test/harness >/dev/null
log "+ docker run -d --name $name --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock -v /var/lib/docker -v /var/lib/containerd nexus-harness:ubuntu-latest"
docker run -d --name "$name" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock \
	-v /var/lib/docker -v /var/lib/containerd nexus-harness:ubuntu-latest >/dev/null

wait_ready() {
	local deadline=$((SECONDS + 180)) st
	while :; do
		st=$(docker exec "$name" systemctl is-system-running 2>/dev/null || true)
		case $st in running | degraded) return ;; esac
		[ $SECONDS -lt $deadline ] || { echo "systemd not ready: $st" >&2; exit 1; }
		sleep 1
	done
}
wait_ready
docker cp "$BUILD/release" "$name:/harness-release"
docker exec "$name" bash -c 'cd /harness-release/nexusd && ./install.sh' >>"$T" 2>&1
docker exec "$name" bash -c '
set -e
install -d -o root -g root -m 0755 /usr/local/libexec/nx-reboot
cat >/usr/local/libexec/nx-reboot/op.sh <<EOF
#!/bin/sh
echo "nx-reboot op: pid \$\$ sleeping \$1 s"
sleep "\$1"
echo done
EOF
chmod 0755 /usr/local/libexec/nx-reboot/op.sh
awk '"'"'
/^  scripts:/ {print; insc=1; next}
insc && /^    enabled:/ {print "    enabled: true"; next}
insc && /^    catalog: \[\]/ {print "    catalog:"; print "      - id: nx-reboot"; print "        description: Sleeps, for the reboot dry run"; print "        path: /usr/local/libexec/nx-reboot/op.sh"; print "        runAs: root"; print "        workingDirectory: /usr/local/libexec/nx-reboot"; print "        timeout: 3h"; print "        terminationGrace: 5s"; print "        parameters:"; print "          - {name: seconds, type: integer}"; insc=0; next}
{print}
'"'"' /etc/nexus/nexusd.yaml >/root/nexusd.yaml.new
install -o root -g root -m 0600 /root/nexusd.yaml.new /etc/nexus/nexusd.yaml
/usr/local/bin/nexusd validate-config
systemctl enable --now nexusd.service
' >>"$T" 2>&1
TOKEN=$(docker exec "$name" /usr/local/bin/nexusd token create --name nx-reboot --scope scripts:read --scope scripts:run --expires 1d 2>/dev/null)
api() { # METHOD PATH [BODY]
	docker exec -i "$name" bash -c 'read -r tok; curl -sS --connect-to localhost:7443:127.0.0.1:7443 --cacert /var/lib/nexus/tls/cert.pem -H "Authorization: Bearer $tok" -H "Content-Type: application/json" -X "$0" ${2:+--data "$2"} "https://localhost:7443$1"' "$1" "$2" "${3:-}" <<<"$TOKEN"
}
wait_health() {
	local deadline=$((SECONDS + 60))
	until docker exec "$name" curl -sf --connect-to localhost:7443:127.0.0.1:7443 --cacert /var/lib/nexus/tls/cert.pem https://localhost:7443/healthz >/dev/null 2>&1; do
		[ $SECONDS -lt $deadline ] || { echo "nexusd not healthy" >&2; exit 1; }
		sleep 1
	done
}
wait_health
for r in $(seq 1 "$ROUNDS"); do
	log "== round $r"
	body=$(api POST /v1/providers/scripts/scripts/nx-reboot/invoke '{"parameters":{"seconds":"3600"}}' 2>&1 || true)
	log "invoke: $body"
	op=$(printf '%s' "$body" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4 || true)
	[ -n "$op" ] || { log "no operation id"; exit 1; }
	for i in $(seq 1 60); do
		st=$(api GET "/v1/operations/$op" | grep -o '"status":"[^"]*"' | head -1 | cut -d'"' -f4 || true)
		[ "$st" = Running ] && docker exec "$name" test -e "/var/lib/nexus/ops/$op/output" && break
		sleep 0.5
	done
	log "before: $(api GET "/v1/operations/$op")"
	log "unit: $(docker exec "$name" systemctl show -p ActiveState -p MainPID -p KillMode "nexus-op-$op.service" | xargs)"
	log "+ $(date -u +%T) docker restart -t 60 $name"
	docker restart -t 60 "$name" >/dev/null
	wait_ready
	wait_health
	for i in $(seq 1 60); do
		after=$(api GET "/v1/operations/$op" 2>&1 || true)
		case $after in *'"status":"Running"'*) sleep 0.5 ;; *) break ;; esac
	done
	log "after: $after"
	log "exit.json: $(docker exec "$name" cat "/var/lib/nexus/ops/$op/exit.json" 2>&1)"
	log "cancel marker: $(docker exec "$name" ls "/var/lib/nexus/ops/$op/cancel" 2>&1)"
	log "journal (op unit, previous boot):"
	docker exec "$name" journalctl --no-pager -o short-precise -u "nexus-op-$op.service" 2>&1 | tail -8 | tee -a "$T"
	case $after in
	*'"status":"Failed"'*'urn:nexus:problem:outcome-unknown'*) log "RESULT round $r: PASS Failed outcome-unknown" ;;
	*) log "RESULT round $r: FAIL" ;;
	esac
done
if grep -qF "$TOKEN" "$T"; then echo "token leaked into transcript" >&2; exit 1; fi
log "token absent from the transcript (grep -F)"
