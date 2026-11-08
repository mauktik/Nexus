#!/bin/bash
# Captures the lock, audit and Finalize testdata of WI 7.2 from real apt and
# dpkg (TC-ERR48). Never edit the captured files by hand: rerun this script.
#
#   internal/providers/apt/testdata/capture-lock.sh BASE [BASE...]
#
# For each BASE image it starts a throwaway container with network access
# (for dpkg-dev), builds the fixture repository with
# test/harness/fixtures/aptrepo/build.sh, and writes to testdata/lock/<base>/:
#
#   frontend-timeout.*   apt-get install with DPkg::Lock::Timeout=2 while a
#                        real dpkg -i holds the frontend lock mid-postinst
#   frontend-nowait.*    the same without the timeout option
#   unrelated.*          apt-get install of a package no source has: exit
#                        100 for a reason other than a lock
#   waited.*             an apt-get install that waited for the lock, got it
#                        and succeeded
#   audit-idle.*, audit-hold.*, audit-unpacked.*
#                        dpkg --audit idle, during the hold, and after a real
#                        dpkg --unpack (an interrupted transaction)
#   dpkg-query-before.txt, dpkg-query-after.txt, status-after.txt,
#   dpkg-log.txt         the snapshot before an apt-get install that upgrades
#                        nxplain and nxconf (whose conffile was edited
#                        locally) and installs nxnew, the state after it, and
#                        the dpkg.log lines it appended
#
# Each command's stdout is <name>.stdout, its stderr <name>.stderr, and its
# exit status <name>.exit.
set -euo pipefail

cd "$(dirname "$0")/../../../.."
ROOT=$(pwd)
OUTROOT=$ROOT/internal/providers/apt/testdata/lock
[ $# -gt 0 ] || set -- ubuntu:latest

inner=$(
	cat <<'INNER'
set -euo pipefail
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none
O=/capture/out
mkdir -p "$O"
FMT='${Package}\t${Architecture}\t${Version}\t${db:Status-Status}\n'
# The provider's mutation options (lane.go).
OPTS=(-y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o APT::Color=0)

/usr/bin/apt-get update -qq >/dev/null
/usr/bin/apt-get install -y -qq --no-install-recommends dpkg-dev >/dev/null

R=/srv/nexus-aptrepo
B=/capture/build.sh
bash "$B" init "$R"
dpkg -i "$(bash "$B" deb /capture/debs nxplain 1.0)" >/dev/null
dpkg -i "$(bash "$B" deb /capture/debs nxconf 1.0 --conffile)" >/dev/null
bash "$B" add "$R" nxplain 2.0 >/dev/null
bash "$B" add "$R" nxconf 2.0 --conffile >/dev/null
bash "$B" add "$R" nxnew 1.0 >/dev/null
mkdir -p /capture/sources-off
mv /etc/apt/sources.list.d/* /capture/sources-off/ 2>/dev/null || true
[ ! -f /etc/apt/sources.list ] || mv /etc/apt/sources.list /capture/sources-off/
echo "deb [trusted=yes] file:$R ./" >/etc/apt/sources.list.d/nexus-fixture.list
/usr/bin/apt-get update -qq >/dev/null
echo "nxconf local" >/etc/nxconf.conf

# cap NAME CMD...: stdout, stderr and exit status of CMD.
cap() {
	local name=$1
	shift
	set +e
	"$@" >"$O/$name.stdout" 2>"$O/$name.stderr"
	echo $? >"$O/$name.exit"
	set -e
}

cap audit-idle /usr/bin/dpkg --audit

# A real dpkg holding the frontend lock mid-postinst.
gate=/capture/gate
dpkg -i "$(bash "$B" deb /capture/debs nxgate 1.0 --gate "$gate")" >/capture/hold.out 2>&1 &
holder=$!
for _ in $(seq 100); do
	grep -q 'Setting up nxgate' /capture/hold.out 2>/dev/null && break
	sleep 0.1
done
cap audit-hold /usr/bin/dpkg --audit
cap frontend-timeout /usr/bin/apt-get "${OPTS[@]}" -o DPkg::Lock::Timeout=2 install -- nxplain
cap frontend-nowait /usr/bin/apt-get "${OPTS[@]}" install -- nxplain
# Waits, then gets the lock once the gate opens.
(sleep 3 && touch "$gate") &
cap waited /usr/bin/apt-get "${OPTS[@]}" -o DPkg::Lock::Timeout=30 install -- nxnew
wait "$holder"
dpkg --purge nxnew >/dev/null

cap unrelated /usr/bin/apt-get "${OPTS[@]}" -o DPkg::Lock::Timeout=2 install -- nxnosuch

dpkg --unpack "$(bash "$B" deb /capture/debs nxhalf 1.0)" >/dev/null
cap audit-unpacked /usr/bin/dpkg --audit
dpkg --purge nxhalf >/dev/null

/usr/bin/dpkg-query -W -f="$FMT" >"$O/dpkg-query-before.txt"
size=$(stat -c %s /var/log/dpkg.log)
/usr/bin/apt-get "${OPTS[@]}" -o DPkg::Lock::Timeout=30 install -- nxplain nxconf nxnew >/dev/null 2>&1
/usr/bin/dpkg-query -W -f="$FMT" >"$O/dpkg-query-after.txt"
cp /var/lib/dpkg/status "$O/status-after.txt"
tail -c +$((size + 1)) /var/log/dpkg.log >"$O/dpkg-log.txt"

{
	echo "captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "image: $BASE_IMAGE"
	echo "image-id: $BASE_ID"
	echo "arch: $(/usr/bin/dpkg --print-architecture)"
	echo "apt: $(/usr/bin/apt-get --version | head -n 1)"
	echo "dpkg: $(/usr/bin/dpkg-query --version | head -n 1)"
	echo "script: internal/providers/apt/testdata/capture-lock.sh"
} >"$O/PROVENANCE"
INNER
)

for base in "$@"; do
	dir=$OUTROOT/$(echo "$base" | tr ':/@' '---')
	echo "capture: $base -> ${dir#"$ROOT"/}"
	docker pull -q "$base" >/dev/null
	id=$(docker image inspect -f '{{.Id}}' "$base")
	c=$(docker run -d -e "BASE_IMAGE=$base" -e "BASE_ID=$id" "$base" sleep 3600)
	trap 'docker rm -f "$c" >/dev/null 2>&1 || true' EXIT
	docker exec "$c" mkdir -p /capture
	docker cp test/harness/fixtures/aptrepo/build.sh "$c:/capture/build.sh"
	docker exec -i "$c" bash -s <<<"$inner"
	rm -rf "$dir"
	mkdir -p "$dir"
	docker cp "$c:/capture/out/." "$dir/"
	docker rm -f "$c" >/dev/null
	trap - EXIT
done
