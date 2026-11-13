#!/bin/bash
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
# (parity-check.log). PARITY=0 leaves all of that out for a quick run.
#
# The Docker CLI's current context is used as is: no socket is hardcoded.
#
# Environment: BASE (image base), RUN (-test.run regex), PARITY, NAME_PREFIX
# (container names), TESTFLAGS (more
# test binary flags), NEXUS_HARNESS_EXPECT_FAIL (passed to the tests: a
# deliberate failure that proves logs are collected and the container is
# removed when a test fails).
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
PARITY=${PARITY:-1}
# Container names start with NAME_PREFIX: when several harness runs share
# one Docker host, each can sweep only its own.
NAME_PREFIX=${NAME_PREFIX:-nexus-harness}
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

dist_before=$(dist_state)
mkdir -p "$OUT" "$LOGS"

say "building linux/$goarch test-only binaries into build/harness/"
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
go build -trimpath -o "$OUT/nexusd" ./cmd/nexusd
go build -trimpath -o "$OUT/opsd" ./test/integration/cmd/opsd
go test -c -trimpath -tags integration -o "$OUT/integration.test" ./test/integration
if [ "$PARITY" = 1 ]; then
	go test -c -trimpath -o "$OUT/detach.test" ./internal/detach
	go test -c -trimpath -o "$OUT/ops.test" ./internal/ops
fi
unset CGO_ENABLED GOOS GOARCH
make --no-print-directory -s nexusd-tree TREE_BIN="$OUT/nexusd" TREE_DIR="$OUT/release/nexusd"

if [ $MODE = openrc ]; then
	tag="nexus-harness:alpine-$(echo "$BASE" | tr ':/@' '---')"
	dockerfile=test/harness/Dockerfile.alpine
	name="$NAME_PREFIX-alpine-$STAMP-$$"
else
	tag="nexus-harness:$(echo "$BASE" | tr ':/@' '---')"
	dockerfile=test/harness/Dockerfile
	name="$NAME_PREFIX-$STAMP-$$"
fi
say "building image $tag from $dockerfile"
docker build --quiet --build-arg "BASE=$BASE" -f "$dockerfile" -t "$tag" test/harness >/dev/null

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
	say "versions: $(docker exec "$container" sh -c 'openrc --version; nginx -v 2>&1; docker version --format "docker {{.Server.Version}}"; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"
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
	say "versions: $(docker exec "$container" sh -c 'systemctl --version | head -1; needrestart --version 2>&1 | head -1; dpkg-query -W -f="apt \${Version} " apt; dpkg-query -W -f="dpkg \${Version}" dpkg; echo; . /etc/os-release; echo "$PRETTY_NAME"' | xargs)"
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
# The release tree install.sh installs from (WI 10.1), root-owned as tar
# extracts it for root.
docker cp "$OUT/release" "$container:/harness/release"
docker exec "$container" chown -R root:root /harness/release
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
args=(-test.v -test.count=1 -test.timeout=30m)
[ -z "$RUN" ] || args+=(-test.run "$RUN")
# TC-46 reads the whole test.log, so it runs after this, on its own.
args+=(-test.skip '^TestParitySuitesRanAsRoot$')
# shellcheck disable=SC2206 # TESTFLAGS is word-split on purpose.
[ -z "$TESTFLAGS" ] || args+=($TESTFLAGS)
set +e
docker exec -w /harness -e NEXUS_HARNESS=1 -e "NEXUS_HARNESS_ARCH=$goarch" \
	-e "NEXUS_HARNESS_EXPECT_FAIL=${NEXUS_HARNESS_EXPECT_FAIL:-}" \
	"$container" /harness/integration.test "${args[@]}" 2>&1 | tee "$LOGS/test.log"
status=${PIPESTATUS[0]}
set -e
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
