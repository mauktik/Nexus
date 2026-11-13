#!/bin/bash
# WI 0.1 / TC-120a: the T1 feasibility spike. Real Pi-hole v5 (FTL v5.25.2)
# under systemd in a privileged container, and the pure-Go SQLite probe
# under deploy/nexusd.service's sandbox. Evidence, not a test suite: there
# is no make target. Each check writes a transcript under transcripts/ with
# every command, its exit code and its UTC time.
#
#   test/harness/pihole/spike.sh             route 1, INIT=sysv, checks 1-6
#   INIT=native test/harness/pihole/spike.sh checks 1-3 with the native unit
#                                            v5.18.x basic-install.sh installs
#                                            on systemd hosts (native-*.txt)
#
# The Docker CLI's current context is used as is, as test/harness/run.sh
# does: no socket is hardcoded, and nothing is bind-mounted.
#
# Exit: 0 every Expected met; 1 a blocking outcome (WI 0.1 Outcomes: check
# 1-2 fail, check 3's bad line exits 0 or FTL's PID changes or it stops
# answering, check 5 fails); 3 an Expected not met with no blocking outcome.
# Check 6 is record-only and never changes the exit code.
set -euo pipefail

cd "$(dirname "$0")/../../.."
ROOT=$(pwd)
HERE=test/harness/pihole
TX=$ROOT/$HERE/transcripts
OUT=$ROOT/build/harness/pihole
INIT=${INIT:-sysv}
READY_TIMEOUT=${READY_TIMEOUT:-180}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
FIXTURE_PORT=8099
SCRATCH=/etc/dnsmasq.d/99-nexus-spike.conf
# A root login shell's environment, not the image's ENV (FTL_CMD,
# FTLCONF_*, DNSMASQ_USER and /opt/pihole on PATH belong to its s6 init).
CENV=(env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/root LANG=C.UTF-8)

case $INIT in
sysv) P="" ;;
native) P="native-" ;;
*)
	echo "spike: INIT must be sysv or native, not $INIT" >&2
	exit 1
	;;
esac

say() { echo "spike: $*"; }
die() {
	echo "spike: FAIL: $*" >&2
	exit 1
}
utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

blocking=0
unmet=0
verdict() { # file status text
	echo "# verdict: $2: $3" >>"$1"
	say "$(basename "$1"): $2: $3"
	case $2 in
	FAIL-BLOCKING) blocking=1 ;;
	FAIL) unmet=1 ;;
	esac
}

arch=$(docker info --format '{{.Architecture}}')
case $arch in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) die "unsupported docker architecture $arch" ;;
esac
ctx=$(docker context show)
server=$(docker info --format '{{.ServerVersion}}')
say "docker context $ctx, server $server, daemon architecture $arch -> GOARCH=$goarch, INIT=$INIT"

mkdir -p "$OUT" "$TX"

if [ "$INIT" = sysv ]; then
	say "building sqliteprobe for linux/$goarch (CGO_ENABLED=0)"
	(cd "$HERE/sqliteprobe" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -o "$OUT/sqliteprobe" .)
fi

tag=nexus-pihole-spike:$INIT
say "building image $tag"
docker build --quiet --build-arg "INIT=$INIT" -t "$tag" "$HERE" >/dev/null
base=$(sed -n 's/^FROM //p' "$HERE/Dockerfile")

name="nexus-pihole-spike-$INIT-$STAMP-$$"
container=""
cleanup() {
	local rc=$?
	if [ -n "$container" ]; then
		mkdir -p "$OUT/logs/$STAMP"
		docker exec "$container" journalctl -b --no-pager -o short-precise >"$OUT/logs/$STAMP/journal-$INIT.log" 2>&1 || true
		docker rm -f -v "$container" >/dev/null 2>&1 || true
		say "container $name removed; journal in ${OUT#"$ROOT"/}/logs/$STAMP"
	fi
	exit $rc
}
trap cleanup EXIT

# header file title: starts a transcript.
header() {
	cat >"$1" <<EOF
# $2
# spike: $HERE/spike.sh (WI 0.1, TC-120a), route 1, INIT=$INIT
# image: $tag built FROM $base
# docker: context $ctx, server $server, daemon architecture $arch
# container: $name
# Each command below runs as root in the container through
# \`docker exec <container> ${CENV[*]} bash -c '<command>'\`
# unless marked [host]. Format: \$ command / # utc / output / # exit.
EOF
}

# x file command: runs command in the container, appends it, its UTC start
# time, its output (stdout and stderr) and its exit code to file. Sets OUTP
# and RC.
x() {
	local f=$1 cmd=$2 t
	t=$(utc)
	set +e
	OUTP=$(docker exec "$container" "${CENV[@]}" bash -c "$cmd" 2>&1)
	RC=$?
	set -e
	{
		printf '\n$ %s\n# utc: %s\n' "$cmd" "$t"
		[ -z "$OUTP" ] || printf '%s\n' "$OUTP"
		printf '# exit: %s\n' "$RC"
	} >>"$f"
}

# h file command...: the same for a command run on the host.
h() {
	local f=$1 t
	shift
	t=$(utc)
	set +e
	OUTP=$("$@" 2>&1)
	RC=$?
	set -e
	{
		printf '\n$ [host] %s\n# utc: %s\n' "$*" "$t"
		[ -z "$OUTP" ] || printf '%s\n' "$OUTP"
		printf '# exit: %s\n' "$RC"
	} >>"$f"
}

say "starting container $name"
started=$(utc)
container=$(docker run -d --name "$name" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock "$tag")

# --- Check 1: boot, FTL active, dig answers ---------------------------------
f=$TX/${P}check1-boot.txt
header "$f" "Check 1: the container boots to running or degraded, pihole-FTL is active and FTL answers"
printf '\n$ [host] docker run -d --name %s --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock %s\n# utc: %s\n%s\n# exit: 0\n' \
	"$name" "$tag" "$started" "$container" >>"$f"
deadline=$((SECONDS + READY_TIMEOUT))
while :; do
	state=$(docker exec "$container" systemctl is-system-running 2>/dev/null || true)
	case $state in
	running | degraded) break ;;
	esac
	[ $SECONDS -lt $deadline ] || break
	sleep 1
done
x "$f" 'systemctl is-system-running'
sysstate=$OUTP
x "$f" 'systemctl --failed --no-legend --plain'
x "$f" 'cat /etc/pihole-spike-init; systemctl show -p FragmentPath,SourcePath,Type,RemainAfterExit,MainPID pihole-FTL.service'
# The generated SysV unit has RemainAfterExit=yes: `active` alone does not
# prove FTL runs, so the PID file, the process and dig are recorded too.
until [ "$(docker exec "$container" systemctl is-active pihole-FTL 2>/dev/null || true)" = active ]; do
	[ $SECONDS -lt $deadline ] || break
	sleep 1
done
x "$f" 'systemctl is-active pihole-FTL'
ftlstate=$OUTP
x "$f" 'for i in $(seq 1 40); do p=$(cat /run/pihole-FTL.pid 2>/dev/null); [ -n "$p" ] && [ -d "/proc/$p" ] && break; sleep 0.5; done; cat /run/pihole-FTL.pid; echo; ps -o pid,user,args -C pihole-FTL'
x "$f" 'for i in $(seq 1 20); do dig +time=2 +tries=1 @127.0.0.1 pi.hole >/dev/null && break; sleep 1; done; dig +time=2 +tries=1 @127.0.0.1 pi.hole'
digrc=$RC
dig1=$OUTP
x "$f" 'systemctl --version | head -n 1; . /etc/os-release; echo "$PRETTY_NAME"; pihole-FTL -v'
if { [ "$sysstate" = running ] || [ "$sysstate" = degraded ]; } && [ "$ftlstate" = active ] &&
	[ "$digrc" -eq 0 ] && grep -q 'status: ' <<<"$dig1"; then
	verdict "$f" PASS "system $sysstate, pihole-FTL $ftlstate, dig answered ($(grep -o 'status: [A-Z]*' <<<"$dig1")); route 1 (pihole/pihole:2024.07.0), INIT=$INIT"
else
	verdict "$f" FAIL-BLOCKING "system '$sysstate', pihole-FTL '$ftlstate', dig exit $digrc"
fi

# --- Check 2: pihole restartdns changes the PID, FTL answers again (A-32) ---
f=$TX/${P}check2-restartdns.txt
header "$f" "Check 2: pihole restartdns changes the PID in /run/pihole-FTL.pid and FTL answers again (A-32)"
x "$f" 'cat /run/pihole-FTL.pid'
pid1=$OUTP
x "$f" 'pihole restartdns'
x "$f" "for i in \$(seq 1 40); do p=\$(cat /run/pihole-FTL.pid 2>/dev/null); [ -n \"\$p\" ] && [ \"\$p\" != '$pid1' ] && [ -d \"/proc/\$p\" ] && break; sleep 0.5; done; cat /run/pihole-FTL.pid"
pid2=$OUTP
x "$f" 'ps -o pid,user,lstart,args -C pihole-FTL; systemctl is-active pihole-FTL'
x "$f" 'for i in $(seq 1 20); do dig +time=2 +tries=1 @127.0.0.1 pi.hole >/dev/null && break; sleep 1; done; dig +time=2 +tries=1 @127.0.0.1 pi.hole'
digrc=$RC
if [ -n "$pid1" ] && [ -n "$pid2" ] && [ "$pid1" != "$pid2" ] && [ "$digrc" -eq 0 ] && grep -q 'status: ' <<<"$OUTP"; then
	verdict "$f" PASS "PID $pid1 -> $pid2, dig answered after the restart"
else
	verdict "$f" FAIL-BLOCKING "PID '$pid1' -> '$pid2', dig exit $digrc"
fi

# --- Check 3: pihole-FTL --test on a planted bad address= line (A-19, A-21) -
f=$TX/${P}check3-ftl-test.txt
header "$f" "Check 3: a planted invalid address= line makes pihole-FTL --test fail; FTL is undisturbed (A-19, A-21)"
x "$f" 'grep -n "^conf-dir" /etc/dnsmasq.conf; ls -la /etc/dnsmasq.d'
x "$f" 'cat /run/pihole-FTL.pid'
pidA=$OUTP
x "$f" "echo 'address=/x.lan/300.1.1.1' >$SCRATCH && cat $SCRATCH"
x "$f" 'pihole-FTL --test'
badrc=$RC
x "$f" 'cat /run/pihole-FTL.pid'
pidB=$OUTP
x "$f" 'dig +time=2 +tries=1 @127.0.0.1 pi.hole'
digrc=$RC
digout=$OUTP
x "$f" "rm $SCRATCH"
x "$f" 'pihole-FTL --test'
goodrc=$RC
x "$f" 'cat /run/pihole-FTL.pid; echo; tail -n 5 /var/log/pihole/FTL.log; ls -la /core 2>&1 || true'
if [ "$badrc" -eq 0 ]; then
	verdict "$f" FAIL-BLOCKING "bad line: --test exited 0, A-19 is false"
elif [ "$badrc" -eq 1 ]; then
	verdict "$f" PASS "bad line: --test exited 1 (A-19)"
else
	verdict "$f" FAIL "bad line: --test exited $badrc, not the expected 1 (non-zero, so A-19's falsifier, exit 0, is not met)"
fi
if [ "$goodrc" -eq 0 ]; then
	verdict "$f" PASS "scratch file removed: --test exited 0"
else
	verdict "$f" FAIL-BLOCKING "scratch file removed: --test exited $goodrc"
fi
if [ -n "$pidA" ] && [ "$pidA" = "$pidB" ] && [ "$digrc" -eq 0 ] && grep -q 'status: ' <<<"$digout"; then
	verdict "$f" PASS "FTL PID $pidA unchanged across the failed test and dig answered (A-21)"
else
	verdict "$f" FAIL-BLOCKING "FTL PID '$pidA' -> '$pidB', dig exit $digrc: A-21 is false"
fi

if [ "$INIT" = sysv ]; then
	# --- Check 4: FTL text API, versions file, pihole -v (A-11, V-8) ----------
	f=$TX/check4-pihole-v.txt
	header "$f" "Check 4: FTL >stats and >version over TCP, /etc/pihole/versions, pihole -v (A-11, V-8)"
	cat >>"$f" <<EOF
#
# The three fixtures WI 2.2 copies are written raw, byte for byte, with no
# header, so a parser test can read them as they came off the wire:
#   transcripts/ftl-v5.25.2-stats.txt    the >stats response
#   transcripts/ftl-v5.25.2-version.txt  the >version response
#   transcripts/versions                 /etc/pihole/versions after updatechecker
# Their commands, exit codes, UTC times and sha256 sums are recorded here.
EOF
	x "$f" 'test -e /etc/pihole/versions && echo "versions exists before updatechecker" || echo "versions absent before updatechecker"'
	x "$f" 'ls -la --time-style=full-iso /etc/pihole/versions && cat /etc/pihole/versions'
	x "$f" 'pihole updatechecker'
	x "$f" 'ls -la --time-style=full-iso /etc/pihole/versions && cat /etc/pihole/versions'
	x "$f" 'grep -E "^FTLPORT=" /etc/pihole/pihole-FTL.conf || echo "FTLPORT not set: default 4711"; cat /run/pihole-FTL.port 2>/dev/null'
	port=$(docker exec "$container" sh -c 'sed -n "s/^FTLPORT=//p" /etc/pihole/pihole-FTL.conf' | tail -n 1)
	port=${port:-4711}
	# Send the command, copy the reply through ---EOM---, send >quit and copy
	# what is left until FTL closes the connection (the blank line after
	# ---EOM---, V-1).
	ftlq='exec 3<>/dev/tcp/127.0.0.1/'$port'; printf ">%s\n" "$1" >&3; while IFS= read -r l <&3; do printf "%s\n" "$l"; [ "$l" = "---EOM---" ] && break; done; printf ">quit\n" >&3; cat <&3; exec 3<&-'
	for q in stats version; do
		raw=$TX/ftl-v5.25.2-$q.txt
		t=$(utc)
		set +e
		docker exec "$container" "${CENV[@]}" bash -c "$ftlq" ftlq "$q" >"$raw"
		RC=$?
		set -e
		{
			printf '\n$ %s\n# utc: %s\n' "bash -c '$ftlq' ftlq $q >transcripts/ftl-v5.25.2-$q.txt" "$t"
			printf '# exit: %s\n' "$RC"
			printf '# %s bytes, sha256 %s, last line %s\n' "$(wc -c <"$raw" | tr -d ' ')" \
				"$(shasum -a 256 "$raw" | cut -d' ' -f1)" "$(grep -v '^$' "$raw" | tail -n 1)"
			cat "$raw"
		} >>"$f"
	done
	t=$(utc)
	docker exec "$container" cat /etc/pihole/versions >"$TX/versions"
	printf '\n$ cat /etc/pihole/versions >transcripts/versions\n# utc: %s\n# exit: 0\n# sha256 %s\n' \
		"$t" "$(shasum -a 256 "$TX/versions" | cut -d' ' -f1)" >>"$f"
	x "$f" 'pihole -v'
	pv=$OUTP
	x "$f" 'pihole-FTL -vv'
	stats=$TX/ftl-v5.25.2-stats.txt
	missing=""
	for k in domains_being_blocked dns_queries_today ads_blocked_today ads_percentage_today unique_clients status; do
		grep -q "^$k " "$stats" || missing="$missing $k"
	done
	eom_ok=1
	for q in stats version; do
		[ "$(grep -v '^$' "$TX/ftl-v5.25.2-$q.txt" | tail -n 1)" = "---EOM---" ] || eom_ok=0
	done
	if [ -z "$missing" ] && [ $eom_ok -eq 1 ] &&
		grep -q 'Pi-hole version is v5.18.3' <<<"$pv" && grep -q 'web version is v5.21' <<<"$pv" &&
		grep -q 'FTL version is v5.25.2' <<<"$pv"; then
		verdict "$f" PASS "core v5.18.3, web v5.21, FTL v5.25.2; both FTL replies end with ---EOM---; every >stats key WI 2.2 reads is present"
	else
		verdict "$f" FAIL "missing >stats keys:${missing:- none}; EOM ok $eom_ok; pihole -v: $(tr '\n' ' ' <<<"$pv")"
	fi

	# --- Check 5: sandboxed SQLite probe (A-14, A-39) ------------------------
	f=$TX/check5-sqliteprobe.txt
	header "$f" "Check 5: modernc.org/sqlite reads gravity.db under every deploy/nexusd.service sandboxing directive (A-14, A-39)"
	# Every key=value line of [Service] except the non-sandbox keys: an
	# exclusion list, so a directive added to the unit later is picked up.
	exclude='^(Type|ExecStart|ExecReload|Restart|StateDirectory|StateDirectoryMode|LogsDirectory|LogsDirectoryMode)='
	props=()
	while IFS= read -r line; do
		props+=(-p "$line")
	done < <(awk '/^\[/ { s = ($0 == "[Service]") } s && /^[A-Za-z]+=/' deploy/nexusd.service | grep -Ev "$exclude")
	nprops=$((${#props[@]} / 2))
	h "$f" sh -c "sed -n '/^\[Service\]/,/^\[/p' deploy/nexusd.service | grep -E '^[A-Za-z]+=' | grep -cEv '$exclude'"
	want=$OUTP
	{
		echo "# generated properties: $nprops (grep -c of the same filter: $want)"
		for ((i = 1; i < ${#props[@]}; i += 2)); do echo "#   ${props[i]}"; done
	} >>"$f"
	h "$f" go version -m "${OUT#"$ROOT"/}/sqliteprobe"
	ver=$(grep -E '^[[:space:]]+dep[[:space:]]+modernc.org/sqlite[[:space:]]' <<<"$OUTP" | awk '{print $3}')
	docker cp "$OUT/sqliteprobe" "$container:/usr/local/bin/sqliteprobe"
	docker exec "$container" sh -c 'chown root:root /usr/local/bin/sqliteprobe && chmod 0755 /usr/local/bin/sqliteprobe'
	echo "# sqliteprobe copied in with docker cp as /usr/local/bin/sqliteprobe (root:root 0755), modernc.org/sqlite $ver" >>"$f"
	unit=nexus-sqliteprobe-$STAMP
	x "$f" 'ls -la --time-style=full-iso /etc/pihole/ | grep gravity.db'
	x "$f" 'date -u "+%Y-%m-%d %H:%M:%S"'
	since=$OUTP
	run=$(printf '%q ' systemd-run --wait --pipe "--unit=$unit" "${props[@]}" /usr/local/bin/sqliteprobe /etc/pihole/gravity.db)
	x "$f" "$run"
	proberc=$RC
	probe=$OUTP
	x "$f" 'ls -la --time-style=full-iso /etc/pihole/ | grep gravity.db'
	listing=$OUTP
	x "$f" "journalctl --no-pager -o short-precise -u $unit"
	jl=$OUTP
	x "$f" "journalctl --no-pager -o short-precise -k --since '$since'"
	jk=$OUTP
	x "$f" "journalctl --no-pager -o short-precise --since '$since' | grep -iE 'SIGSYS|EPERM|audit|seccomp|operation not permitted' || echo 'no SIGSYS, EPERM, audit or seccomp line in the journal since $since'"
	hits=$(printf '%s\n%s\n%s\n' "$probe" "$jl" "$jk" | grep -iE 'SIGSYS|EPERM|audit|seccomp|operation not permitted' || true)
	mode=$(sed -n 's/^journal_mode //p' <<<"$probe")
	why=""
	[ "$nprops" -gt 0 ] && [ "$nprops" = "$want" ] || why="$why property count $nprops != grep -c $want;"
	[ "$proberc" -eq 0 ] || why="$why probe exit $proberc;"
	[ -n "$mode" ] && [ "$mode" != wal ] || why="$why journal_mode '$mode';"
	grep -q '^domainlist_count ' <<<"$probe" && grep -q '^info ' <<<"$probe" || why="$why no info rows or row count;"
	! grep -qE 'gravity\.db-(journal|wal|shm)' <<<"$listing" || why="$why sidecar file present;"
	[ -z "$hits" ] || why="$why journal: $hits;"
	if [ -z "$why" ]; then
		verdict "$f" PASS "$nprops properties; exit 0; journal_mode $mode; $(grep '^domainlist_count' <<<"$probe"); no sidecar; no SIGSYS/EPERM/seccomp; modernc.org/sqlite $ver"
	else
		verdict "$f" FAIL-BLOCKING "${why% ;}"
	fi

	# --- Check 6 (record only): gravity offline against a loopback list (A-33)
	f=$TX/check6-gravity-offline.txt
	header "$f" "Check 6 (record only): pihole -g offline against a fixture list on a loopback HTTP port (A-33)"
	x "$f" "pihole-FTL sqlite3 /etc/pihole/gravity.db \"SELECT value FROM info WHERE property='updated'\""
	before=$OUTP
	x "$f" "mkdir -p /root/spike-fixture && printf '%s\n' blocked-one.example.com blocked-two.example.com blocked-three.example.com >/root/spike-fixture/list.txt && printf '%s\n' 'server.document-root = \"/root/spike-fixture\"' 'server.bind = \"127.0.0.1\"' 'server.port = $FIXTURE_PORT' 'mimetype.assign = (\".txt\" => \"text/plain\")' >/root/spike-fixture/lighttpd.conf && cat /root/spike-fixture/lighttpd.conf"
	x "$f" 'systemd-run --unit=spike-fixture-http /usr/sbin/lighttpd -D -f /root/spike-fixture/lighttpd.conf'
	x "$f" "for i in \$(seq 1 20); do curl -fsS http://127.0.0.1:$FIXTURE_PORT/list.txt && break; sleep 0.5; done"
	# Offline: the container leaves its only network, so only loopback is left.
	h "$f" docker network disconnect bridge "$container"
	# What gravity's DNS-availability check resolves, and through what.
	x "$f" 'ip -br addr; cat /etc/resolv.conf; grep -n "lookupDomain=" /opt/pihole/gravity.sh; timeout 4 getent hosts raw.githubusercontent.com; echo "getent exit $?"'
	x "$f" "pihole -a adlist add http://127.0.0.1:$FIXTURE_PORT/list.txt spike-fixture"
	x "$f" 'pihole-FTL sqlite3 /etc/pihole/gravity.db "SELECT id, enabled, address FROM adlist"'
	x "$f" 'timeout 900 pihole -g >/root/spike-gravity.log 2>&1'
	grc=$RC
	# gravity redraws its countdown with ESC[K: one line per redraw.
	x "$f" "sed -e 's/\x1b\[K/\n/g' /root/spike-gravity.log | tail -n 30"
	x "$f" "pihole-FTL sqlite3 /etc/pihole/gravity.db \"SELECT value FROM info WHERE property='updated'\""
	after=$OUTP
	x "$f" "pihole-FTL sqlite3 /etc/pihole/gravity.db \"SELECT a.address, count(g.domain) FROM adlist a LEFT JOIN gravity g ON g.adlist_id = a.id GROUP BY a.id\""
	if [ "$grc" -eq 0 ] && [ "$after" != "$before" ]; then
		echo "# verdict: RECORDED: pihole -g exit 0; info.updated $before -> $after" >>"$f"
		say "check6-gravity-offline.txt: RECORDED: pihole -g exit 0; info.updated $before -> $after"
	else
		echo "# verdict: RECORDED (failed, not an escalation; input for WI 5.1): pihole -g exit $grc; info.updated $before -> $after" >>"$f"
		say "check6-gravity-offline.txt: RECORDED (failed, record only): pihole -g exit $grc; info.updated $before -> $after"
	fi
fi

if [ $blocking -ne 0 ]; then
	say "a blocking outcome was observed (see the verdict lines); exit 1"
	exit 1
fi
if [ $unmet -ne 0 ]; then
	say "an Expected was not met, with no blocking outcome (see the verdict lines); exit 3"
	exit 3
fi
say "every Expected met"
