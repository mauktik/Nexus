#!/bin/bash
# Captures the upgrade testdata of WI 7.3 from real apt and dpkg (TC-128,
# TC-129, TC-130, TC-ERR71). Never edit the captured files by hand: rerun
# this script.
#
#   internal/providers/apt/testdata/capture-upgrade.sh BASE [BASE...]
#
# For each BASE image it starts a throwaway container with network access
# (for dpkg-dev), builds the fixture repository with
# test/harness/fixtures/aptrepo/build.sh, and writes to
# testdata/upgrade/<base>/. The fixture world, each package at 1.0 installed
# and 2.0 in the repository:
#
#   nxplain, nxother  plain upgrades
#   nxnewdep          2.0 depends on nxdep, which is not installed
#   nxconfl           2.0 conflicts with nxvictim (installed)
#   nxheld            held with apt-mark hold
#   nxconf            a conffile edited locally
#
# For each MODE (upgrade, dist-upgrade, only-standard: install
# --only-upgrade --no-remove -- nxplain nxconfl, only-full: install
# --only-upgrade -- nxplain nxconfl), from that world freshly installed:
#
#   MODE.policy           apt-cache policy of every installed package
#   MODE.dpkg-before      dpkg-query of every package
#   MODE.sim.*            the provider's preview command (previewArgv)
#   MODE.run.*            the provider's mutation command (mutationArgv)
#   MODE.dpkg-after       dpkg-query after the run
#
# and, once: names.* (dpkg-query of the hostile names and nxplain), and
# hostile-*.* (apt-get -s install --only-upgrade with each hostile name: the
# S-4 behavior the name resolution exists for). Each command's stdout is
# <name>.stdout, its stderr <name>.stderr, and its exit status <name>.exit.
set -euo pipefail

cd "$(dirname "$0")/../../../.."
ROOT=$(pwd)
OUTROOT=$ROOT/internal/providers/apt/testdata/upgrade
[ $# -gt 0 ] || set -- ubuntu:latest debian:stable

inner=$(
	cat <<'INNER'
set -euo pipefail
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none
O=/capture/out
mkdir -p "$O"
FMT='${Package}\t${Architecture}\t${Version}\t${db:Status-Status}\n'
NOCACHE=(-o Dir::Cache::pkgcache= -o Dir::Cache::srcpkgcache=)
# The provider's commands (upgrade.go previewArgv, lane.go mutationArgv).
SIM=(/usr/bin/apt-get -s -y "${NOCACHE[@]}" -o APT::Color=0)
RUN=(/usr/bin/apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold
	-o DPkg::Lock::Timeout=30 -o APT::Color=0)
FIXTURES="nxplain nxother nxnewdep nxdep nxconfl nxvictim nxheld nxconf"

/usr/bin/apt-get update -qq >/dev/null
/usr/bin/apt-get install -y -qq --no-install-recommends dpkg-dev >/dev/null

R=/srv/nexus-aptrepo
B=/capture/build.sh
bash "$B" init "$R"
bash "$B" add "$R" nxplain 2.0 >/dev/null
bash "$B" add "$R" nxother 2.0 >/dev/null
bash "$B" add "$R" nxdep 1.0 >/dev/null
bash "$B" add "$R" nxnewdep 2.0 --depends nxdep >/dev/null
bash "$B" add "$R" nxconfl 2.0 --conflicts nxvictim >/dev/null
bash "$B" add "$R" nxheld 2.0 >/dev/null
bash "$B" add "$R" nxconf 2.0 --conffile >/dev/null
mkdir -p /capture/sources-off
mv /etc/apt/sources.list.d/* /capture/sources-off/ 2>/dev/null || true
[ ! -f /etc/apt/sources.list ] || mv /etc/apt/sources.list /capture/sources-off/
echo "deb [trusted=yes] file:$R ./" >/etc/apt/sources.list.d/nexus-fixture.list
/usr/bin/apt-get update -qq >/dev/null

# cap NAME CMD...: stdout, stderr and exit status of CMD.
cap() {
	local name=$1
	shift
	set +e
	"$@" >"$O/$name.stdout" 2>"$O/$name.stderr"
	echo $? >"$O/$name.exit"
	set -e
}

world() {
	apt-mark unhold nxheld >/dev/null 2>&1 || true
	# shellcheck disable=SC2086 # One argument per name.
	dpkg --purge $FIXTURES >/dev/null 2>&1 || true
	rm -f /etc/nxconf.conf
	for n in nxplain nxother nxnewdep nxconfl nxvictim nxheld; do
		dpkg -i "$(bash "$B" deb /capture/debs "$n" 1.0)" >/dev/null
	done
	dpkg -i "$(bash "$B" deb /capture/debs nxconf 1.0 --conffile)" >/dev/null
	echo "nxconf local" >/etc/nxconf.conf
	apt-mark hold nxheld >/dev/null
}

for mode in upgrade dist-upgrade only-standard only-full; do
	if [ "$mode" = only-standard ]; then
		args=(install --only-upgrade --no-remove -- nxplain nxconfl)
	elif [ "$mode" = only-full ]; then
		args=(install --only-upgrade -- nxplain nxconfl)
	else
		args=("$mode")
	fi
	world
	/usr/bin/dpkg-query -W -f="$FMT" >"$O/$mode.dpkg-before"
	# shellcheck disable=SC2046 # One argument per name.
	/usr/bin/apt-cache "${NOCACHE[@]}" policy -- $(dpkg-query -W -f='${Package}\n') >"$O/$mode.policy"
	cap "$mode.sim" "${SIM[@]}" "${args[@]}"
	cap "$mode.run" "${RUN[@]}" "${args[@]}"
	/usr/bin/dpkg-query -W -f="$FMT" >"$O/$mode.dpkg-after"
done

world
cap names /usr/bin/dpkg-query -W -f="$FMT" -- nxplain nxplain- nxnewdep+ nxplai.
for h in nxplain- nxnewdep+ nxplai.; do
	cap "hostile-$h" "${SIM[@]}" install --only-upgrade -- "$h"
done

{
	echo "captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "image: $BASE_IMAGE"
	echo "image-id: $BASE_ID"
	echo "arch: $(/usr/bin/dpkg --print-architecture)"
	echo "apt: $(/usr/bin/apt-get --version | head -n 1)"
	echo "dpkg: $(/usr/bin/dpkg-query --version | head -n 1)"
	echo "script: internal/providers/apt/testdata/capture-upgrade.sh"
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
