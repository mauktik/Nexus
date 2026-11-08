#!/bin/bash
# Captures the install and remove testdata of WI 7.4 from real apt and dpkg
# (TC-136, TC-137, TC-ERR53, TC-ERR54, TC-ERR55). Never edit the captured
# files by hand: rerun this script.
#
#   internal/providers/apt/testdata/capture-install.sh BASE [BASE...]
#
# For each BASE image it starts a throwaway container with network access
# (for dpkg-dev), builds the fixture repository with
# test/harness/fixtures/aptrepo/build.sh, and writes to
# testdata/install/<base>/. The repository:
#
#   nxplain 1.0, 2.0    installed at 1.0 where a step says so
#   nxg++ 1.0           a legal name holding "+"
#   nxnewdep 1.0        never installed
#   nxapp 1.0           a conffile, and Depends: nxdep
#   nxdep 1.0           installed only as nxapp's dependency (automatic)
#   nxconfl 1.0         Conflicts: nxvictim, which is installed
#
# Captured, each as <name>.stdout, <name>.stderr and <name>.exit:
#
#   show-<n>         apt-cache show -- <n> (the provider's cmdShowAll), for
#                    exact, hostile, qualified and unknown names; show-batch
#                    for several names at once
#   names, names-rc  dpkg-query of names, as remove resolves them
#   hostile-<cmd>-<n>  apt-get -s <cmd> -- <n>: the S-4 behavior the name
#                    resolution exists for
#   STEP.sim, STEP.run, STEP.dpkg-before, STEP.dpkg-after
#                    the provider's preview command (previewArgv) and
#                    mutation command (mutationArgv) for each STEP, from the
#                    same state, with dpkg-query around the run
set -euo pipefail

cd "$(dirname "$0")/../../../.."
ROOT=$(pwd)
OUTROOT=$ROOT/internal/providers/apt/testdata/install
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

/usr/bin/apt-get update -qq >/dev/null
/usr/bin/apt-get install -y -qq --no-install-recommends dpkg-dev >/dev/null

R=/srv/nexus-aptrepo
B=/capture/build.sh
bash "$B" init "$R"
bash "$B" add "$R" nxplain 1.0 >/dev/null
bash "$B" add "$R" nxplain 2.0 >/dev/null
bash "$B" add "$R" nxg++ 1.0 >/dev/null
bash "$B" add "$R" nxnewdep 1.0 >/dev/null
bash "$B" add "$R" nxdep 1.0 >/dev/null
bash "$B" add "$R" nxapp 1.0 --conffile --depends nxdep >/dev/null
bash "$B" add "$R" nxconfl 1.0 --conflicts nxvictim >/dev/null
mkdir -p /capture/sources-off
mv /etc/apt/sources.list.d/* /capture/sources-off/ 2>/dev/null || true
[ ! -f /etc/apt/sources.list ] || mv /etc/apt/sources.list /capture/sources-off/
echo "deb [trusted=yes] file:$R ./" >/etc/apt/sources.list.d/nexus-fixture.list
/usr/bin/apt-get update -qq >/dev/null
NATIVE=$(/usr/bin/dpkg --print-architecture)
/usr/bin/dpkg --print-architecture >"$O/print-architecture.txt"

# cap NAME CMD...: stdout, stderr and exit status of CMD.
cap() {
	local name=$1
	shift
	set +e
	"$@" >"$O/$name.stdout" 2>"$O/$name.stderr"
	echo $? >"$O/$name.exit"
	set -e
}

# step NAME ARGS...: the preview and the run of ARGS from the same state.
step() {
	local name=$1
	shift
	/usr/bin/dpkg-query -W -f="$FMT" >"$O/$name.dpkg-before"
	cap "$name.sim" "${SIM[@]}" "$@"
	cap "$name.run" "${RUN[@]}" "$@"
	/usr/bin/dpkg-query -W -f="$FMT" >"$O/$name.dpkg-after"
}

world() {
	dpkg --purge nxplain nxg++ nxnewdep nxapp nxdep nxconfl nxvictim >/dev/null 2>&1 || true
	rm -f /etc/nxapp.conf
	dpkg -i "$(bash "$B" deb /capture/debs nxplain 1.0)" >/dev/null
	dpkg -i "$(bash "$B" deb /capture/debs nxvictim 1.0)" >/dev/null
}

world
for n in nxplain nxg++ nxnewdep nxplai. nxplain- nxnewdep+ nxg+ "nxplain:$NATIVE" nxplain:all nxplain:i386 nxnone; do
	cap "show-$n" /usr/bin/apt-cache "${NOCACHE[@]}" show -- "$n"
done
# Several names at once, one of them unknown: the unknown one is skipped.
cap show-batch /usr/bin/apt-cache "${NOCACHE[@]}" show -- nxplain nxnone nxg++ nxplai.
cap names /usr/bin/dpkg-query -W -f="$FMT" -- nxplain nxplain- nxnewdep nxnewdep+ nxplai.
cap hostile-install-nxplain- "${SIM[@]}" install -- nxplain-
cap hostile-remove-nxnewdep+ "${SIM[@]}" remove -- nxnewdep+
cap hostile-install-nxplai. "${SIM[@]}" install -- nxplai.

# Install: a "+" name, a pinned older version, a conflict, an unknown
# version, and a pin below the installed version.
step install-plus install --no-remove -- nxg++
dpkg --purge nxplain >/dev/null
step install-pin install --no-remove -- nxplain=1.0
step install-conflict install --no-remove -- nxconfl
step install-badversion install --no-remove -- nxplain=9.9
/usr/bin/apt-get install -y -qq -- nxplain=2.0 >/dev/null
step install-downgrade install --no-remove -- nxplain=1.0

# Remove: default, purge, autoremove, purge with autoremove; nxapp is
# installed through apt each time, so nxdep is automatic.
for mode in remove purge autoremove purge-autoremove; do
	world
	/usr/bin/apt-get install -y -qq -- nxapp >/dev/null
	if [ "$mode" = remove ]; then
		step "remove-$mode" remove -- nxapp
	elif [ "$mode" = purge ]; then
		step "remove-$mode" purge -- nxapp
	elif [ "$mode" = autoremove ]; then
		step "remove-$mode" remove --autoremove -- nxapp
	else
		step "remove-$mode" purge --autoremove -- nxapp
	fi
done
# Purge of a package left in config-files state.
world
/usr/bin/apt-get install -y -qq -- nxapp >/dev/null
/usr/bin/apt-get remove -y -qq -- nxapp >/dev/null
cap names-rc /usr/bin/dpkg-query -W -f="$FMT" -- nxapp
step remove-purge-rc purge -- nxapp

{
	echo "captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "image: $BASE_IMAGE"
	echo "image-id: $BASE_ID"
	echo "arch: $NATIVE"
	echo "apt: $(/usr/bin/apt-get --version | head -n 1)"
	echo "dpkg: $(/usr/bin/dpkg-query --version | head -n 1)"
	echo "script: internal/providers/apt/testdata/capture-install.sh"
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
