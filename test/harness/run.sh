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
# Pi-hole (WI 5.1): when BASE is an image labelled org.nexus.harness=pihole,
# which `make test-harness-pihole [INIT=native|sysv]` builds from
# test/harness/pihole, that image is the harness image as is. The required
# units are then the stub upstream and the fixture HTTP server, FTL is ready
# when its PID file names a live pihole-FTL that answers dig (never
# `systemctl is-active`, which the SysV wrapper keeps active after FTL dies),
# and pihole_setup runs once before any test.
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
# How long setup waits for FTL to finish reloading its lists after a command
# that reloads or restarts it (ftl_settle).
LISTS_TIMEOUT=${LISTS_TIMEOUT:-60}
REQUIRED_UNITS="systemd-journald.service dbus.service docker.service"
PIHOLE=""
if [ "$(docker image inspect -f '{{index .Config.Labels "org.nexus.harness"}}' "$BASE" 2>/dev/null || true)" = pihole ]; then
	PIHOLE=1
	REQUIRED_UNITS="systemd-journald.service dbus.service stub-upstream.service fixture-http.service"
fi
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

if [ -n "$PIHOLE" ]; then
	tag=$BASE
	say "Pi-hole harness image $tag (INIT=$(docker image inspect -f '{{index .Config.Labels "org.nexus.harness.init"}}' "$BASE"))"
else
	tag="nexus-harness:$(echo "$BASE" | tr ':/@' '---')"
	say "building image $tag"
	docker build --quiet --build-arg "BASE=$BASE" -t "$tag" test/harness >/dev/null
fi

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
if [ -n "$PIHOLE" ]; then
	pihole_setup
else
	say "versions: $(docker exec "$container" sh -c 'systemctl --version | head -1; needrestart --version 2>&1 | head -1; dpkg-query -W -f="apt \${Version} " apt; dpkg-query -W -f="dpkg \${Version}" dpkg; echo; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"
fi

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
