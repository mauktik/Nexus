#!/bin/bash
# Captures the parser testdata from real apt (TC-120, TC-121). Never edit the
# captured files by hand: rerun this script.
#
#   internal/providers/apt/testdata/capture.sh BASE [BASE...]
#
# For each BASE image (ubuntu:latest, debian:stable) it starts a throwaway
# container with network access, runs the exact commands the provider runs,
# with the provider's environment, and writes their output to
# testdata/<base>/, with the date, apt and dpkg versions in
# testdata/<base>/PROVENANCE. Two sets per base:
#
#   archive-*  the distribution's own archive after apt-get update: real
#              origin text for the security classification
#   fixture-*  the fixture repository built by test/harness/fixtures/aptrepo
#              (nxplain 1.0 installed, 2.0 in the repository; nxavail only
#              in the repository; nxlocal installed and in no repository)
set -euo pipefail

cd "$(dirname "$0")/../../../.."
ROOT=$(pwd)
OUTROOT=$ROOT/internal/providers/apt/testdata
[ $# -gt 0 ] || set -- ubuntu:latest debian:stable

inner=$(
	cat <<'INNER'
set -euo pipefail
export LC_ALL=C LANG=C DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none
O=/capture/out
mkdir -p "$O"
# The provider's dpkgFormat and options (exec.go).
FMT='${Package}\t${Architecture}\t${Version}\t${db:Status-Status}\n'
NOCACHE=(-o Dir::Cache::pkgcache= -o Dir::Cache::srcpkgcache=)

/usr/bin/apt-get update -qq >/dev/null
/usr/bin/apt-get install -y -qq --no-install-recommends dpkg-dev >/dev/null

# The archive: whatever is upgradable today, with its real origins.
/usr/bin/apt-get "${NOCACHE[@]}" -s -o APT::Color=0 dist-upgrade >"$O/archive-sim-dist-upgrade.txt"
names=$(awk '$1 == "Inst" { print $2 }' "$O/archive-sim-dist-upgrade.txt" | head -n 40)
if [ -n "$names" ]; then
	# shellcheck disable=SC2086 # One argument per name.
	/usr/bin/apt-cache "${NOCACHE[@]}" policy -- $names >"$O/archive-policy.txt"
else
	: >"$O/archive-policy.txt"
fi
/usr/bin/dpkg-query -W -f="$FMT" >"$O/archive-dpkg-query.txt"
/usr/bin/dpkg --print-architecture >"$O/print-architecture.txt"

# The fixture repository, with only its own source.
R=/srv/nexus-aptrepo
B=/capture/build.sh
bash "$B" init "$R"
dpkg -i "$(bash "$B" deb /capture/debs nxplain 1.0)" >/dev/null
dpkg -i "$(bash "$B" deb /capture/debs nxlocal 1.0)" >/dev/null
bash "$B" add "$R" nxplain 2.0 >/dev/null
bash "$B" add "$R" nxavail 1.0 >/dev/null
mkdir -p /capture/sources-off
mv /etc/apt/sources.list.d/* /capture/sources-off/ 2>/dev/null || true
[ ! -f /etc/apt/sources.list ] || mv /etc/apt/sources.list /capture/sources-off/
echo "deb [trusted=yes] file:$R ./" >/etc/apt/sources.list.d/nexus-fixture.list
/usr/bin/apt-get update -qq >/dev/null
/usr/bin/apt-get "${NOCACHE[@]}" -s -o APT::Color=0 dist-upgrade >"$O/fixture-sim-dist-upgrade.txt"
/usr/bin/apt-get "${NOCACHE[@]}" -s -o APT::Color=0 remove nxplain nxlocal >"$O/fixture-sim-remove.txt"
/usr/bin/apt-cache "${NOCACHE[@]}" policy -- nxplain nxavail nxlocal dpkg >"$O/fixture-policy.txt"
set +e
/usr/bin/apt-cache "${NOCACHE[@]}" policy -- nxnosuch >"$O/fixture-policy-unknown.txt" 2>&1
echo "exit $?" >>"$O/fixture-policy-unknown.txt"
/usr/bin/apt-cache "${NOCACHE[@]}" show -- nxplain >"$O/fixture-show-all-nxplain.txt" 2>&1
for n in nxplain nxlocal nxavail; do
	/usr/bin/apt-cache "${NOCACHE[@]}" show --no-all-versions -- "$n" >"$O/fixture-show-$n.txt" 2>&1
done
/usr/bin/apt-cache "${NOCACHE[@]}" pkgnames -- nxpl >"$O/fixture-pkgnames-nxpl.txt" 2>&1
/usr/bin/apt-cache "${NOCACHE[@]}" pkgnames -- nxlocal >"$O/fixture-pkgnames-nxlocal.txt" 2>&1
/usr/bin/apt-cache "${NOCACHE[@]}" policy -- 'nxplai.' >"$O/fixture-policy-regex.txt" 2>&1
echo "exit $?" >>"$O/fixture-policy-regex.txt"
/usr/bin/dpkg-query -W -f="$FMT" -- nxnosuch >"$O/fixture-dpkg-query-unknown.txt" 2>&1
echo "exit $?" >>"$O/fixture-dpkg-query-unknown.txt"
set -e
/usr/bin/dpkg-query -W -f="$FMT" >"$O/fixture-dpkg-query.txt"
/usr/bin/dpkg-query -W -f="$FMT" -- nxplain >"$O/fixture-dpkg-query-nxplain.txt"

{
	echo "captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "image: $BASE_IMAGE"
	echo "image-id: $BASE_ID"
	echo "arch: $(/usr/bin/dpkg --print-architecture)"
	echo "apt: $(/usr/bin/apt-get --version | head -n 1)"
	echo "dpkg: $(/usr/bin/dpkg-query --version | head -n 1)"
	echo "script: internal/providers/apt/testdata/capture.sh"
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
