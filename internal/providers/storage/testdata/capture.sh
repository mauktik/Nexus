#!/bin/sh
# capture.sh [--host-name N] [--out DIR] [--map FILE] [--sandbox-probe]
#            [--write-probe DATASET] [--error-pool] [DIR...]
#
# The storage provider's host capture (storage-provider WI 1.1, run by the
# operator as root in WI 1.2). The same script runs on Ubuntu (GNU), Debian
# and Alpine (BusyBox). DIR... are the vzdump and Time Machine directories
# to list (default /media/vzdump /media/vzdump/dump /media/timemachine
# /media/timemachine2).
#
# The default run only reads. The ZFS subcommands it runs are version,
# list, status and get. --sandbox-probe also only reads. --write-probe runs
# one `zfs snapshot` under the unit's sandbox, expecting a denial (A-7), and
# --error-pool creates, damages, scrubs and destroys a scratch pool on a
# file in /var/tmp (A-4); both are opt-in and live in their own functions.
#
# Every ZFS command runs as `/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin
# LC_ALL=C` followed by the absolute path of zpool or zfs, found in
# /usr/sbin then /sbin. Each command's stdout goes to its own file followed
# by an `Exit <code>` line (when stdout does not end in a newline, one is
# added before the Exit line and the command is marked [noeol] in
# PROVENANCE), and its stderr to <file>.stderr. A command absent from the
# host is recorded as Exit 127 with a .stderr of `not found`.
#
# The text form of zpool status prints its dates in the host's local zone, so
# the capture records that zone beside them (A-60): `timezone` (an IANA name,
# UTC or unknown) and `utc-offset` (date +%z) in PROVENANCE, and in `forms`
# how the zone was found (tz=localtime-link|etc-timezone|timedatectl|
# no-localtime|unknown).
#
# Redaction (A-55, default on): everything is captured into a private
# staging directory; before anything is written under --out, each bundle
# name becomes mac-<n> (one placeholder per distinct name, numbered in order
# of first appearance, A-62), each guest name (a .notes first line, a `VM Name:`
# or `CT Name:` value) becomes guest-<vmid>, every <string> value of a
# captured XML plist becomes redacted-<n>, and the id of a Time Machine
# `.com.apple.timemachine.supported-<id>` marker becomes id-<n>. A guest name
# is replaced in its own field always, and elsewhere only in the vzdump/
# files and only as a whole word, and only when it is a name that cannot be
# mistaken for the capture's own text (at least 4 bytes, not a keyword of the
# log or a tool name, not made of digits and punctuation alone), so a short
# or common name never rewrites `Exit 0` or a ZFS name (code review CR-3).
# The mapping goes to --map, which must lie outside --out and is never
# committed. MachineID.plist and the contents of a bundle's bands directory
# are never read.
set -u

PATH=/usr/sbin:/usr/bin:/sbin:/bin
LC_ALL=C
export PATH LC_ALL
umask 077

die() {
	echo "capture.sh: $*" 1>&2
	exit 2
}

usage() {
	die "usage: capture.sh [--host-name N] [--out DIR] [--map FILE] [--sandbox-probe] [--write-probe DATASET] [--error-pool] [DIR...]"
}

DAY=$(date -u +%Y%m%d)
HOST=$(uname -n)
OUT=
MAP=/root/nexus-capture-map-$DAY.txt
OPTS=
WRITE_DS=
while [ $# -gt 0 ]; do
	case $1 in
	--host-name)
		[ $# -ge 2 ] || usage
		HOST=$2
		shift 2
		;;
	--out)
		[ $# -ge 2 ] || usage
		OUT=$2
		shift 2
		;;
	--map)
		[ $# -ge 2 ] || usage
		MAP=$2
		shift 2
		;;
	--sandbox-probe)
		OPTS="$OPTS --sandbox-probe"
		shift
		;;
	--write-probe)
		[ $# -ge 2 ] || usage
		WRITE_DS=$2
		OPTS="$OPTS --write-probe"
		shift 2
		;;
	--error-pool)
		OPTS="$OPTS --error-pool"
		shift
		;;
	--)
		shift
		break
		;;
	-*) usage ;;
	*) break ;;
	esac
done
if [ $# -eq 0 ]; then
	set -- /media/vzdump /media/vzdump/dump /media/timemachine /media/timemachine2
fi

case $HOST in
'' | *[!A-Za-z0-9._-]*) die "--host-name must be letters, digits, dot, dash or underscore: '$HOST'" ;;
esac
case $WRITE_DS in
*[!A-Za-z0-9_.:/-]* | /* | */ | *//*) die "--write-probe DATASET is not a dataset name: '$WRITE_DS'" ;;
esac
[ -n "$OUT" ] || OUT=/root/nexus-capture-$HOST-$DAY
[ "$(id -u)" = 0 ] || die "run as root"

# abspath PATH: PATH made absolute through its nearest existing directory.
abspath() {
	p=$1
	case $p in /*) ;; *) p=$(pwd -P)/$p ;; esac
	rest=
	while [ ! -d "$p" ]; do
		rest=/${p##*/}$rest
		p=$(dirname -- "$p")
	done
	p=$(cd -- "$p" && pwd -P) || exit 2
	echo "${p%/}$rest"
}
OUT=$(abspath "$OUT") || die "cannot resolve --out"
MAP=$(abspath "$MAP") || die "cannot resolve --map"
case $MAP/ in
"$OUT"/*) die "--map $MAP lies inside --out $OUT; the mapping must never be committed" ;;
esac
if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then
	die "$OUT is not empty"
fi
[ ! -e "$MAP" ] || die "$MAP already exists"

S=$(mktemp -d /tmp/nexus-capture.XXXXXX) || die "cannot create a staging directory"
trap 'rm -rf "$S"' EXIT
trap 'exit 130' INT TERM HUP
mkdir -p "$S/out" || die "cannot create $S/out"
O=$S/out
: >"$S/commands"
: >"$S/map"
: >"$S/bundles"
: >"$S/guest-tokens"
: >"$S/markers"
WROTE_SNAPSHOT=

ZPOOL=
for p in /usr/sbin/zpool /sbin/zpool; do
	if [ -x "$p" ]; then
		ZPOOL=$p
		break
	fi
done
[ -n "$ZPOOL" ] || ZPOOL=/usr/sbin/zpool
ZFS=
for p in /usr/sbin/zfs /sbin/zfs; do
	if [ -x "$p" ]; then
		ZFS=$p
		break
	fi
done
[ -n "$ZFS" ] || ZFS=/usr/sbin/zfs

# run NAME ARGV...: ARGV's stdout to $O/NAME followed by an `Exit <code>`
# line, its stderr to $O/NAME.stderr. A program that is not on the host is
# Exit 127 with a .stderr of `not found`. It sets RC, BYTES (stdout's
# length) and NOEOL.
run() {
	f=$O/$1
	shift
	mkdir -p "$(dirname "$f")"
	prog=$1
	[ "$prog" != /usr/bin/env ] || prog=${5:-}
	NOEOL=
	if command -v "$prog" >/dev/null 2>&1; then
		"$@" >"$f" 2>"$f.stderr"
		RC=$?
	else
		: >"$f"
		echo "not found" >"$f.stderr"
		RC=127
	fi
	BYTES=$(wc -c <"$f" | tr -d ' ')
	if [ -s "$f" ] && [ "$(tail -c 1 "$f" | wc -l | tr -d ' ')" = 0 ]; then
		echo >>"$f"
		NOEOL=" [noeol]"
	fi
	echo "Exit $RC" >>"$f"
	return $RC
}

# note NAME ARGV...: NAME and the argv that wrote it, in PROVENANCE's commands.
note() {
	n=$1
	shift
	printf '  %s: %s%s\n' "$n" "$*" "$NOEOL" >>"$S/commands"
}

# cap NAME ARGV...: run, then note.
cap() {
	run "$@"
	rc=$RC
	note "$@"
	return $rc
}

# The forms of the GNU-only options, each tried once on / and the fallback
# used when the GNU form exits non-zero. The form used is in PROVENANCE.
if ls -lan --time-style=full-iso -d / >/dev/null 2>&1; then
	LSFORM=gnu
elif ls -lan --full-time -d / >/dev/null 2>&1; then
	LSFORM=full-time
elif ls -lane -d / >/dev/null 2>&1; then
	LSFORM=busybox
else
	LSFORM=plain
fi
if du -s --block-size=1 /dev/null >/dev/null 2>&1; then
	DUFORM=gnu
else
	DUFORM=kib
fi
if df -B1 / >/dev/null 2>&1; then
	DFFORM=gnu
else
	DFFORM=kib
fi
if timeout 120 true >/dev/null 2>&1; then
	TIMEOUT="timeout 120"
elif timeout -t 120 true >/dev/null 2>&1; then
	TIMEOUT="timeout -t 120"
else
	TIMEOUT=
fi

# listing NAME DIR: `ls -lan` with full times, numeric owners (no user names).
listing() {
	case $LSFORM in
	gnu) cap "$1" ls -lan --time-style=full-iso "$2" ;;
	full-time) cap "$1" ls -lan --full-time "$2" ;;
	busybox) cap "$1" ls -lane "$2" ;;
	*) cap "$1" ls -lan "$2" ;;
	esac
}

# space NAME DIR: the df oracle of a share.
space() {
	if [ $DFFORM = gnu ]; then
		cap "$1" df -B1 "$2"
	else
		cap "$1" df -k "$2"
	fi
}

# bundlesize NAME BUNDLE: the du oracle of a bundle's allocated size, never
# a listing or read of what is inside it.
bundlesize() {
	# shellcheck disable=SC2086 # $TIMEOUT is a command prefix or empty
	if [ $DUFORM = gnu ]; then
		cap "$1" $TIMEOUT du -s --block-size=1 "$2/bands"
	else
		cap "$1" $TIMEOUT du -sk "$2/bands"
	fi
}

# readable FILE: refuses the files never to be read (NFR-5).
readable() {
	case ${1##*/} in
	MachineID.plist) return 1 ;;
	esac
	[ -f "$1" ]
}

# mapname PLACEHOLDER VALUE: one mapping line (placeholder, tab, real
# value), and a second for the value as BusyBox ls prints it, with each
# byte outside printable ASCII shown as `?`.
mapname() {
	case $2 in
	*'	'* | '') return 0 ;;
	esac
	grep -qxF "$1	$2" "$S/map" || printf '%s\t%s\n' "$1" "$2" >>"$S/map"
	q=$(printf '%s' "$2" | tr -c '\040-\176' '?')
	[ "$q" = "$2" ] || grep -qxF "$1	$q" "$S/map" || printf '%s\t%s\n' "$1" "$q" >>"$S/map"
}

# guestname PLACEHOLDER VALUE: mapname, and, when VALUE can be replaced
# elsewhere in the vzdump/ files without mistaking the capture's own text for
# a name, a line in $S/guest-tokens (CR-3). Its own field is always replaced,
# where it is read; a shorter or structural name is not replaced anywhere else.
guestname() {
	mapname "$1" "$2"
	case $2 in
	*'	'* | '') return 0 ;;
	esac
	[ ${#2} -ge 4 ] || return 0
	case $2 in *[!0-9._:-]*) ;; *) return 0 ;; esac
	lc=$(printf '%s' "$2" | tr 'A-Z' 'a-z')
	case $lc in
	exit | info | warn | warning | error | finished | backup | starting | name | status | zfs | zpool | vzdum[p] | guest | mac | true | false | none | null) return 0 ;;
	esac
	grep -qxF "$1	$2" "$S/guest-tokens" || printf '%s\t%s\n' "$1" "$2" >>"$S/guest-tokens"
}

# ---- host ----
cap host/id-u.txt id -u
cap host/os-release.txt sed -n 's/^PRETTY_NAME=//p' /etc/os-release
cap host/uname-r.txt uname -r
cap host/date.txt date -u +%FT%TZ
cap host/utc-offset.txt date +%z
cap host/localtime-link.txt readlink /etc/localtime
cap host/etc-timezone.txt head -n 1 /etc/timezone
cap host/timedatectl-timezone.txt timedatectl show -p Timezone --value
cap host/ls-dev-zfs.txt ls -ln /dev/zfs
for p in /usr/sbin/zpool /sbin/zpool /usr/sbin/zfs /sbin/zfs; do
	cap "host/stat$(echo "$p" | tr / -).txt" stat "$p"
done
cap host/ls-etc-pve.txt ls -ldn /etc/pve
cap host/ls-pve-manager-list.txt ls -ldn /var/lib/dpkg/info/pve-manager.list

# ---- time zone (A-60) ----

# zone_name WORD: WORD when it is a zone name, else nothing.
zone_name() {
	case $1 in
	'' | *[!A-Za-z0-9_+/.-]*) ;;
	*) printf '%s' "$1" ;;
	esac
}

# first_line FILE: the first line of a capture's stdout when the command
# exited 0, else nothing.
first_line() {
	[ "$(tail -n 1 "$O/$1")" = "Exit 0" ] || return 0
	sed -n '1{p;q;}' "$O/$1"
}

TZNAME=
TZFORM=
link=$(first_line host/localtime-link.txt)
case $link in
*zoneinfo/*)
	TZNAME=$(zone_name "${link##*zoneinfo/}")
	[ -z "$TZNAME" ] || TZFORM=localtime-link
	;;
esac
if [ -z "$TZNAME" ]; then
	TZNAME=$(zone_name "$(first_line host/etc-timezone.txt)")
	[ -z "$TZNAME" ] || TZFORM=etc-timezone
fi
if [ -z "$TZNAME" ]; then
	TZNAME=$(zone_name "$(first_line host/timedatectl-timezone.txt)")
	[ -z "$TZNAME" ] || TZFORM=timedatectl
fi
if [ -z "$TZNAME" ]; then
	# Without /etc/localtime glibc and musl use UTC; otherwise the zone is
	# only known by its offset.
	if [ ! -e /etc/localtime ]; then
		TZNAME=UTC
		TZFORM=no-localtime
	else
		TZNAME=unknown
		TZFORM=unknown
	fi
fi
UTC_OFFSET=$(first_line host/utc-offset.txt)

# ---- zfs ----
PLCOLS=name,size,allocated,free,capacity,fragmentation,dedupratio,health,altroot,readonly
cap zfs/zfs-version.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" version
cap zfs/zfs-version.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" version -j
cap zfs/zpool-list.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -j --json-int \
	-o "$PLCOLS"
cap zfs/zpool-list.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H -p -o "$PLCOLS"
cap zfs/zpool-list-names.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H -o name
cap zfs/zpool-status.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int
cap zfs/zpool-status-v.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int -v
cap zfs/zpool-status-v.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -p -v
POOLS=$(sed '$d' "$O/zfs/zpool-list-names.txt")
for P in $POOLS; do
	case $P in *[!A-Za-z0-9_.:-]*) continue ;; esac
	cap "zfs/zpool-get-$P.json" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" get -j --json-int all "$P"
	cap "zfs/zpool-get-$P.txt" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" get -H -p \
		-o name,property,value,source all "$P"
done
DSCOLS=name,type,used,avail,refer,usedbysnapshots,quota,refquota,reservation,compression,compressratio,recordsize,mountpoint,mounted,encryption,keystatus
SNCOLS=name,creation,used,refer,userrefs,nexus:created-by
cap zfs/zfs-list-datasets.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int \
	-t filesystem,volume -o "$DSCOLS"
cap zfs/zfs-list-datasets.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p \
	-t filesystem,volume -o "$DSCOLS"
cap zfs/zfs-list-snapshots.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int \
	-t snapshot -o "$SNCOLS"
SNAP_JSON_BYTES=$BYTES
cap zfs/zfs-list-snapshots.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p \
	-t snapshot -o "$SNCOLS"
SNAP_TEXT_BYTES=$BYTES
SNAP_ROWS=$(sed '$d' "$O/zfs/zfs-list-snapshots.txt" | wc -l | tr -d ' ')
cap zfs/zfs-list-snapshots-creation.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p \
	-t snapshot -o name,creation
cap zfs/negative-zpool-list.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -j --json-int \
	-o "$PLCOLS" nexus-capture-absent
cap zfs/negative-zpool-status.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int \
	nexus-capture-absent
cap zfs/negative-zfs-list.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int \
	nexus-capture-absent/x

# ---- shares: listings, bundles, the newest vzdump archive, oracles ----
MACS=0
NEWEST=
NEWEST_DIR=
d_i=0
for D in "$@"; do
	d_i=$((d_i + 1))
	listing "shares/$d_i/ls.txt" "$D"
	# A Time Machine share root lists one `.com.apple.timemachine.supported-<id>`
	# marker per Mac: the <id> is a per-Mac identifier of the MachineID.plist
	# family, so each marker name gets its own placeholder (CR-7).
	sed -n 's/^.* \(\.com\.apple\.timemachine\.supported-[^ ]*\)$/\1/p' "$O/shares/$d_i/ls.txt" | sort -u | while IFS= read -r mk; do
		grep -q "^$(printf '%s' "$mk" | sed 's/[][\.*^$/]/\\&/g')	" "$S/markers" ||
			printf '%s\t.com.apple.timemachine.supported-id-%s\n' "$mk" "$(($(wc -l <"$S/markers") + 1))" >>"$S/markers"
	done
	space "shares/$d_i/df.txt" "$D"
	[ -d "$D" ] || continue
	first=1
	for B in "$D"/*.sparsebundle "$D"/*.backupbundle; do
		[ -d "$B" ] || continue
		bn=${B##*/}
		# One name, one placeholder (A-62): the same Mac backs up to more than
		# one share under one bundle name, and the directory name must be the
		# placeholder the fixed-string replacement writes in the contents.
		ph=$(BN=$bn awk -F '\t' 'BEGIN { n = ENVIRON["BN"] } substr($0, 1, length(n) + 1) == n "\t" { print $NF; exit }' "$S/bundles")
		if [ -z "$ph" ]; then
			MACS=$((MACS + 1))
			ph=mac-$MACS.${bn##*.}
			printf '%s\t%s\n' "$bn" "$ph" >>"$S/bundles"
		fi
		mapname "$ph" "$bn"
		listing "shares/$d_i/$ph/ls.txt" "$B"
		h=$B/com.apple.TimeMachine.SnapshotHistory.plist
		if readable "$h"; then
			cap "shares/$d_i/$ph/SnapshotHistory.head8" head -c 8 "$h"
			size=$(wc -c <"$h" | tr -d ' ')
			if [ "$(head -c 5 "$h")" = "<?xml" ] && [ "$size" -le 65536 ]; then
				cap "shares/$d_i/$ph/SnapshotHistory.plist" cat "$h"
			fi
		fi
		if [ $first = 1 ]; then
			bundlesize "shares/$d_i/$ph/du.txt" "$B"
			first=0
		fi
	done
	for A in "$D"/vzdump-qemu-*-????_??_??-??_??_??.* "$D"/vzdump-lxc-*-????_??_??-??_??_??.*; do
		[ -f "$A" ] || continue
		case $A in *.log | *.notes | *.protected | *.dat | *.tmp) continue ;; esac
		stamp=$(echo "${A##*/}" | sed -n 's/^vzdump-[a-z]*-[0-9]*-\([0-9_]*-[0-9_]*\)\..*/\1/p')
		[ -z "$stamp" ] || printf '%s\t%s\t%s\n' "$stamp" "$D" "${A##*/}" >>"$S/archives"
	done
done
if [ -s "$S/archives" ]; then
	last=$(sort "$S/archives" | tail -n 1)
	NEWEST_DIR=$(echo "$last" | cut -f2)
	NEWEST=$NEWEST_DIR/$(echo "$last" | cut -f3)
fi
GUESTS=0
if [ -n "$NEWEST" ]; then
	an=${NEWEST##*/}
	vmid=$(echo "$an" | sed -n 's/^vzdump-[a-z]*-\([0-9]*\)-.*/\1/p')
	stem=$an
	case $stem in *.zst | *.gz | *.lzo) stem=${stem%.*} ;; esac
	case $stem in *.vma | *.tar) stem=${stem%.*} ;; esac
	log=$NEWEST_DIR/$stem.log
	notes=$NEWEST_DIR/$an.notes
	[ -f "$notes" ] || notes=$NEWEST_DIR/$stem.notes
	if readable "$log"; then
		cap vzdump/newest-log-head.txt head -n 40 "$log"
		cap vzdump/newest-log-tail.txt tail -n 40 "$log"
		for f in "$O/vzdump/newest-log-head.txt" "$O/vzdump/newest-log-tail.txt"; do
			sed -n -e 's/.*VM Name: //p' -e 's/.*CT Name: //p' "$f" | while IFS= read -r g; do
				guestname "guest-$vmid" "$g"
			done
			# The name's own field, whatever its length (CR-3).
			sed -e "s/\(VM Name: \).*/\1guest-$vmid/" -e "s/\(CT Name: \).*/\1guest-$vmid/" "$f" >"$S/log.tmp" &&
				mv "$S/log.tmp" "$f"
		done
	fi
	if readable "$notes"; then
		cap vzdump/newest-notes-line1.txt head -n 1 "$notes"
		g=
		[ "$BYTES" = 0 ] || g=$(sed -n '1{p;q;}' "$O/vzdump/newest-notes-line1.txt")
		[ -z "$g" ] || guestname "guest-$vmid" "$g"
		# The notes capture is the placeholder whatever the line was.
		{
			[ -z "$g" ] || echo "guest-$vmid"
			sed -n '$p' "$O/vzdump/newest-notes-line1.txt"
		} >"$S/notes.tmp" && mv "$S/notes.tmp" "$O/vzdump/newest-notes-line1.txt"
	fi
	GUESTS=$(cut -f1 "$S/map" | grep '^guest-' | sort -u | wc -l | tr -d ' ')
fi

# ---- schedules ----
# Only the ZFS-related timers are kept (A-20 needs the scrub and snapshot
# ones); every other unit name on the host stays out of the capture (CR-7).
run schedules/systemctl-list-timers.txt systemctl list-timers --all --no-pager
awk 'NR == 1 || /zfs|zpool|scrub|snapshot/ || /^Exit /' "$O/schedules/systemctl-list-timers.txt" >"$S/timers.tmp" &&
	mv "$S/timers.tmp" "$O/schedules/systemctl-list-timers.txt"
note schedules/systemctl-list-timers.txt systemctl list-timers --all --no-pager "(kept: the header and the zfs, zpool, scrub and snapshot lines)"
for f in /etc/cron.d/zfsutils-linux /etc/cron.d/zfs-auto-snapshot; do
	[ -e "$f" ] && cap "schedules/cron.d-${f##*/}" cat "$f"
done
for f in /etc/cron.*/zfs-auto-snapshot; do
	[ -e "$f" ] || continue
	cap schedules/ls-cron-zfs-auto-snapshot.txt ls -ln /etc/cron.*/zfs-auto-snapshot
	break
done

# ---- the unit's sandbox (--sandbox-probe, --write-probe) ----

# hardening: the installed unit's lines between `# Hardening` and
# `[Install]`, one property per line, in $S/hardening.
hardening() {
	[ -s "$S/hardening" ] && return 0
	cap sandbox-probe/unit-cat.txt systemctl cat nexusd
	sed -n '/^# Hardening/,/^\[Install\]/p' "$O/sandbox-probe/unit-cat.txt" |
		awk 'NF && $1 !~ /^#/ && $1 !~ /^\[/' >"$S/hardening"
	cp "$S/hardening" "$O/sandbox-probe/hardening"
	[ -s "$S/hardening" ] || die "no hardening lines in the installed nexusd unit"
}

# probe NAME ARGV...: ARGV under systemd-run with one -p per hardening line,
# so it runs with the unit's sandbox (ASM-3, A-6).
probe() {
	pf=$1
	shift
	n=$#
	while IFS= read -r prop; do
		set -- "$@" -p "$prop"
	done <"$S/hardening"
	while [ $n -gt 0 ]; do
		set -- "$@" "$1"
		shift
		n=$((n - 1))
	done
	run "sandbox-probe/$pf" systemd-run --wait --pipe --collect --quiet "$@"
	rc=$RC
	printf '%s: %s%s\n' "$pf" "$*" "$NOEOL" >>"$O/sandbox-probe/commands"
	return $rc
}

sandbox_probe() {
	hardening
	: >"$O/sandbox-probe/commands"
	# Every read argv zfs.Argv builds for the provider, exactly as it builds
	# it (TC-STG-72 compares them line by line), in both formats; the helper's
	# snapshot readback runs outside the sandbox with the writes and is not
	# here. P1, the first pool, names the pool for pool commands and its root
	# dataset for dataset and snapshot commands; the root dataset is always
	# present.
	P1=$(echo "$POOLS" | sed -n 1p)
	case $P1 in *[!A-Za-z0-9_.:-]*) P1= ;; esac
	# Unnamed.
	probe zfs-version.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" version
	probe zpool-list-names.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H -o name
	probe zpool-list.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -j --json-int -o "$PLCOLS"
	probe zpool-list.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H -p -o "$PLCOLS"
	probe zpool-status.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int
	probe zpool-status.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -p
	probe zpool-status-v.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int -v
	probe zpool-status-v.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -p -v
	probe zfs-list-datasets.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int -t filesystem,volume -o "$DSCOLS"
	probe zfs-list-datasets.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t filesystem,volume -o "$DSCOLS"
	probe zfs-list-snapshots-creation.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t snapshot -o name,creation
	probe zfs-list-snapshots.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t snapshot -o "$SNCOLS"
	# A-39 evidence only, not an Argv shape: the snapshot listing in JSON.
	probe zfs-list-snapshots.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int -t snapshot -o "$SNCOLS"
	# Named with P1.
	if [ -n "$P1" ]; then
		probe zpool-list-pool.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -j --json-int -o "$PLCOLS" "$P1"
		probe zpool-list-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H -p -o "$PLCOLS" "$P1"
		probe zpool-status-pool.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int "$P1"
		probe zpool-status-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -p "$P1"
		probe zpool-status-v-pool.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -j --json-int -v "$P1"
		probe zpool-status-v-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status -p -v "$P1"
		probe zpool-get-pool.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" get -j --json-int all "$P1"
		probe zpool-get-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" get -H -p -o name,property,value,source all "$P1"
		probe zfs-list-datasets-pool.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -j --json-int -t filesystem,volume -o "$DSCOLS" "$P1"
		probe zfs-list-datasets-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t filesystem,volume -o "$DSCOLS" "$P1"
		probe zfs-list-snapshots-creation-d1-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t snapshot -o name,creation -d 1 "$P1"
		probe zfs-list-snapshots-d1-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t snapshot -o "$SNCOLS" -d 1 "$P1"
		probe zfs-list-snapshots-r-pool.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -p -t snapshot -o "$SNCOLS" -r "$P1"
	fi
	i=0
	for D in "$@"; do
		i=$((i + 1))
		probe "stat-f-$i.txt" /usr/bin/stat -f "$D"
	done
	# Bounded reads, piped to wc -c: only the exit code and the byte count.
	# shellcheck disable=SC2016 # $1 is the inner shell's
	if [ -n "$NEWEST" ]; then
		[ ! -f "$log" ] || probe read-log.txt /bin/sh -c 'tail -c 4096 "$1" | wc -c' sh "$log"
		[ ! -f "$notes" ] || probe read-notes.txt /bin/sh -c 'head -c 1024 "$1" | wc -c' sh "$notes"
	fi
	for D in "$@"; do
		for B in "$D"/*.sparsebundle "$D"/*.backupbundle; do
			h=$B/com.apple.TimeMachine.SnapshotHistory.plist
			readable "$h" || continue
			# shellcheck disable=SC2016 # $1 is the inner shell's
			probe read-plist.txt /bin/sh -c 'head -c 8 "$1" | wc -c' sh "$h"
			break 2
		done
	done
	# A-8: mounted and mountpoint inside the sandbox equal those outside.
	probe mounts-inside /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H -o name,mounted,mountpoint
	cap sandbox-probe/mounts-outside /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" list -H \
		-o name,mounted,mountpoint
	diff "$O/sandbox-probe/mounts-inside" "$O/sandbox-probe/mounts-outside" >"$O/sandbox-probe/mounts.diff"
	grep -H -i -e 'Operation not permitted' -e 'Permission denied' -e 'SECCOMP' -e 'Bad system call' \
		"$O"/sandbox-probe/*.stderr >"$O/sandbox-probe/denials.txt"
	return 0
}

# write_probe DATASET: one snapshot under the unit's sandbox, expected to be
# denied (A-7). If it succeeds the snapshot is left for the operator to
# remove by hand, its name is printed and the script exits 3.
write_probe() {
	hardening
	[ -e "$O/sandbox-probe/commands" ] || : >"$O/sandbox-probe/commands"
	if probe write-probe.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZFS" snapshot \
		"$1@nexus-capture-probe"; then
		WROTE_SNAPSHOT=$1@nexus-capture-probe
	fi
	return 0
}

# error_pool: a real pool with checksum and permanent errors (NFR-8, A-4),
# on a 256 MiB file, never in /etc/zfs/zpool.cache, removed by the trap
# whatever happens.
error_pool() {
	img=/var/tmp/nxcapture.img
	cap error-pool/precheck-list.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" list -H \
		-o name nxcapture
	[ "$RC" != 0 ] || die "a pool named nxcapture is imported; --error-pool refuses to touch it"
	cap error-pool/precheck-import.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" import
	cap error-pool/precheck-import-d.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" import \
		-d /var/tmp
	if grep -qx ' *pool: nxcapture' "$O/error-pool/precheck-import.txt" "$O/error-pool/precheck-import-d.txt"; then
		die "a pool named nxcapture is importable; --error-pool refuses to touch it"
	fi
	[ ! -e "$img" ] || die "$img exists; --error-pool refuses to overwrite it"
	echo "capture.sh: --error-pool: ZED (zfs-zed) may send an alert for the checksum errors injected into the scratch pool nxcapture; stop zfs-zed for the run or expect the alert." 1>&2
	# shellcheck disable=SC2016 # expanded when the trap runs
	trap '/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" destroy -f nxcapture >/dev/null 2>&1; rm -f /var/tmp/nxcapture.img; rm -rf "$S"' EXIT
	trap 'exit 130' INT TERM HUP
	# A failed step stops the error pool, not the capture: the EXIT trap
	# still removes the pool and the image when the script ends.
	ep_failed="capture.sh: --error-pool stopped; see error-pool/ in the capture"
	cap error-pool/image.txt dd if=/dev/zero of="$img" bs=1048576 count=256 || {
		echo "$ep_failed" 1>&2
		return 0
	}
	cap error-pool/create.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" create \
		-o cachefile=none nxcapture "$img" || {
		echo "$ep_failed" 1>&2
		return 0
	}
	cap error-pool/write.txt dd if=/dev/urandom of=/nxcapture/data bs=1048576 count=96
	sync
	cap error-pool/export.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" export nxcapture || {
		echo "$ep_failed" 1>&2
		return 0
	}
	cap error-pool/damage.txt dd if=/dev/urandom of="$img" bs=1048576 seek=96 count=64 conv=notrunc
	cap error-pool/import.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" import \
		-o cachefile=none -d /var/tmp nxcapture || {
		echo "$ep_failed" 1>&2
		return 0
	}
	cap error-pool/scrub.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" scrub -w nxcapture
	cap error-pool/zpool-status-v.json /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status \
		-j --json-int -v nxcapture
	cap error-pool/zpool-status-v.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" status \
		-p -v nxcapture
	cap error-pool/destroy.txt /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$ZPOOL" destroy \
		-f nxcapture
	rm -f "$img"
	rmdir /nxcapture 2>/dev/null
	trap 'rm -rf "$S"' EXIT
	return 0
}

for opt in --sandbox-probe --write-probe --error-pool; do
	case " $OPTS " in *" $opt "*) ;; *) continue ;; esac
	case $opt in
	--sandbox-probe) sandbox_probe "$@" ;;
	--write-probe) write_probe "$WRITE_DS" ;;
	--error-pool) error_pool ;;
	esac
done

# ---- redaction (A-55) ----
# Every <string> value of a captured XML plist becomes redacted-<n>, numbered
# in order of first appearance; the pairs join the mapping.
PLISTS=$(find "$O" -type f -name SnapshotHistory.plist | sort)
PLIST_STRINGS=0
if [ -n "$PLISTS" ]; then
	echo "$PLISTS" | while IFS= read -r f; do
		# The whole file is one string, so a <string> value that spans lines is
		# replaced too (CR-7); a multi-line value is recorded in the mapping
		# with its newlines written as \n.
		awk -v strs="$S/plist-strings" -v dst="$f.red" '
		BEGIN { n = 0; while ((getline l < strs) > 0) { i = index(l, "\t"); v[substr(l, i + 1)] = substr(l, 1, i - 1); n++ } close(strs) }
		{ buf = buf $0 "\n" }
		END {
			out = ""; s = buf
			while (match(s, /<string>[^<]*<\/string>/)) {
				val = substr(s, RSTART + 8, RLENGTH - 17)
				rec = val; gsub(/\n/, "\\n", rec)
				if (!(rec in v)) { n++; v[rec] = "redacted-" n; print v[rec] "\t" rec >> strs }
				out = out substr(s, 1, RSTART - 1) "<string>" v[rec] "</string>"
				s = substr(s, RSTART + RLENGTH)
			}
			printf "%s", out s > dst
		}' "$f" && mv "$f.red" "$f"
	done
	if [ -f "$S/plist-strings" ]; then
		cat "$S/plist-strings" >>"$S/map"
		PLIST_STRINGS=$(wc -l <"$S/plist-strings" | tr -d ' ')
	fi
fi

# The Time Machine markers join the mapping under their placeholders.
tab=$(printf '\t')
while IFS=$tab read -r mk ph; do
	[ -z "$mk" ] || mapname "$ph" "$mk"
done <"$S/markers"

# Two kinds of mapping line (CR-3). A bundle name and a marker name are
# replaced everywhere as fixed strings: each is long, unique and ends in a
# suffix of its own. A guest name is replaced in its own field where it was
# read, and elsewhere only by `redact ... guests`, as a whole word, for the
# names $S/guest-tokens lists. The longest real value first, so a name inside
# a longer one never splits it.
grep -v "^guest-" "$S/map" | grep -v "^redacted-" |
	awk -F '\t' '{ print length($2) "\t" $0 }' | sort -t "$tab" -k1,1nr | cut -f2- >"$S/map.sorted"
awk -F '\t' '{ print length($2) "\t" $0 }' "$S/guest-tokens" | sort -t "$tab" -k1,1nr | cut -f2- >"$S/map.guest"

# redact SRC DST SCOPE: SRC with every real value replaced by its placeholder.
# SCOPE guests (the files under the capture's vzdump directory) also replaces
# the guest names of map.guest, whole words only.
redact() {
	awk -v mapf="$S/map.sorted" -v guestf="$S/map.guest" -v scope="${3:-}" '
	BEGIN {
		n = 0; while ((getline l < mapf) > 0) { i = index(l, "\t"); n++; ph[n] = substr(l, 1, i - 1); rv[n] = substr(l, i + 1) }
		g = 0
		if (scope == "guests") while ((getline l < guestf) > 0) { i = index(l, "\t"); g++; gph[g] = substr(l, 1, i - 1); grv[g] = substr(l, i + 1) }
	}
	{
		s = $0
		for (k = 1; k <= n; k++) {
			o = ""
			while ((i = index(s, rv[k])) > 0) { o = o substr(s, 1, i - 1) ph[k]; s = substr(s, i + length(rv[k])) }
			s = o s
		}
		for (k = 1; k <= g; k++) {
			# A match is a name only when no letter, digit or underscore touches it.
			o = ""
			while ((i = index(s, grv[k])) > 0) {
				pre = (i > 1) ? substr(s, i - 1, 1) : (length(o) > 0 ? substr(o, length(o), 1) : "")
				post = substr(s, i + length(grv[k]), 1)
				if (pre ~ /[A-Za-z0-9_]/ || post ~ /[A-Za-z0-9_]/) { o = o substr(s, 1, i); s = substr(s, i + 1) }
				else { o = o substr(s, 1, i - 1) gph[k]; s = substr(s, i + length(grv[k])) }
			}
			s = o s
		}
		print s
	}' "$1" >"$2"
}

REDACTION="mac: $MACS; guest: $GUESTS; plist-strings: $PLIST_STRINGS"
{
	echo "host: $HOST"
	echo "source: host"
	echo "date: $(sed -n 1p "$O/host/date.txt")"
	echo "timezone: $TZNAME"
	echo "utc-offset: $UTC_OFFSET"
	echo "os-release: $(sed -n 1p "$O/host/os-release.txt" | tr -d '"')"
	v=$(sed -n 1p "$O/zfs/zfs-version.txt")
	case $v in
	"Exit 127") v="absent (Exit 127)" ;;
	"Exit "*) v="unknown ($v)" ;;
	esac
	echo "zfs-version: $v"
	echo "kernel: $(sed -n 1p "$O/host/uname-r.txt")"
	echo "captured-as: root"
	echo "options:${OPTS:- none}"
	echo "forms: ls=$LSFORM du=$DUFORM df=$DFFORM timeout=${TIMEOUT:-none} tz=$TZFORM"
	echo "redaction: $REDACTION"
	echo "snapshot-rows: $SNAP_ROWS"
	echo "snapshot-bytes-json: $SNAP_JSON_BYTES"
	echo "snapshot-bytes-text: $SNAP_TEXT_BYTES"
	echo "commands:"
	cat "$S/commands"
} >"$O/PROVENANCE"

# The mapping goes to --map (outside --out) before anything under --out.
cp "$S/map" "$MAP" || die "cannot write $MAP"
chmod 600 "$MAP" || die "cannot chmod $MAP"
mkdir -p "$OUT" || die "cannot create $OUT"
chmod 755 "$OUT"
(cd "$O" && find . -type f) | sort | while IFS= read -r rel; do
	rel=${rel#./}
	printf '%s\n' "$rel" >"$S/rel"
	redact "$S/rel" "$S/rel.red"
	dst=$OUT/$(cat "$S/rel.red")
	mkdir -p "$(dirname "$dst")"
	scope=
	case $rel in vzdump/*) scope=guests ;; esac
	redact "$O/$rel" "$dst" "$scope"
	chmod 644 "$dst"
done
find "$OUT" -type d -exec chmod 755 {} \;

FIELD_ONLY=$(grep '^guest-' "$S/map" | cut -f2 | sort -u | wc -l | tr -d ' ')
FIELD_ONLY=$((FIELD_ONLY - $(cut -f2 "$S/guest-tokens" | sort -u | wc -l | tr -d ' ')))
echo "capture.sh: $HOST written to $OUT (redaction: $REDACTION); mapping in $MAP, keep it on this host"
[ "$FIELD_ONLY" -le 0 ] || echo "capture.sh: $FIELD_ONLY guest name(s) were short or common words and were replaced only in their own field; check $OUT/vzdump by eye"
echo "capture.sh: review $OUT before handing it over"
if [ -n "$WROTE_SNAPSHOT" ]; then
	echo "capture.sh: --write-probe SUCCEEDED inside the sandbox (A-7 is wrong): remove $WROTE_SNAPSHOT by hand" 1>&2
	exit 3
fi
exit 0
