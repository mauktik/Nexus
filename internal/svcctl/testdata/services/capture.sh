#!/bin/sh
# capture.sh NAME OUTDIR: the read-only capture of a host's init system that
# svcctl's services tests read (services-provider WI 3.1 for the T1
# containers, WI 6.0 for the real hosts). Run it as root.
#
# It only reads: listings, `show` of a fixed property list, is-active and
# is-enabled, nexusd's own journal, and copies of a few /proc and /run
# files. It never starts, stops, enables or changes anything, so the same
# script is safe on the live hosts. The planted services' actions are the T1
# driver's (test/integration/services_capture_test.go), never this script's.
#
# Every tool runs with svcctl's exact environment, env -i
# PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C, by its absolute path. A
# command's combined output goes to one file followed by an `Exit <code>`
# line. A journal command's stdout goes to NAME.jsonl and its stderr and
# `Exit` line to NAME.txt. A copy of a file (proc/..., run/..., etc/...)
# holds the file's bytes only, and its status line goes to copy-status.txt.
# PROVENANCE lists every output file and the argv that wrote it.
#
# Sanitizing: only the fixed -p properties of `show`, only MESSAGE and
# PRIORITY of nexusd.service's journal, and only the output_log, error_log,
# output_logger, error_logger and description lines of /etc/init.d and
# /etc/conf.d are read. No unit environment or command line is ever asked
# for (TC-SV-43 checks the committed tree).
set -u

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

die() {
	echo "capture.sh: $*" 1>&2
	exit 2
}

[ $# -eq 2 ] || die "usage: capture.sh NAME OUTDIR"
NAME=$1
OUT=$2
case $NAME in
'' | *[!A-Za-z0-9._-]*) die "NAME must be letters, digits, dot, dash or underscore: '$NAME'" ;;
esac
[ "$(id -u)" = 0 ] || die "run as root"
[ ! -e "$OUT/PROVENANCE" ] || die "$OUT already holds a capture"

# x runs a tool with svcctl's environment and nothing else.
x() {
	/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "$@"
}

# tool is the absolute path of a tool on svcctl's PATH.
tool() {
	command -v "$1" || die "no $1 on $PATH"
}

# note records one output file and its argv in PROVENANCE's commands.
note() {
	printf '  %s: %s\n' "$1" "$2" >>"$OUT/PROVENANCE"
}

# cap FILE ARGV...: combined output, then the Exit line.
cap() {
	f=$1
	shift
	x "$@" >"$OUT/$f" 2>&1
	rc=$?
	echo "Exit $rc" >>"$OUT/$f"
	note "$f" "$*"
}

# copy FILE ARGV...: the tool's stdout only (a file's bytes), its stderr and
# Exit line in copy-status.txt.
copy() {
	f=$1
	shift
	x "$@" >"$OUT/$f" 2>>"$OUT/copy-status.txt"
	rc=$?
	echo "$f: Exit $rc" >>"$OUT/copy-status.txt"
	note "$f" "$*"
}

# listed NAME: NAME is the first field of a listing line.
listed() {
	u=$1
	shift
	awk -v u="$u" '$1 == u { f = 1 } END { exit !f }' "$@"
}

if [ -x /usr/bin/systemctl ] && [ -d /run/systemd/system ]; then
	INIT=systemd
elif [ -x /sbin/openrc ] && [ -d /run/openrc ]; then
	INIT=openrc
else
	die "neither systemd nor OpenRC is running"
fi

mkdir -p "$OUT/oracle" "$OUT/proc/self" "$OUT/proc/sys/kernel/random" || die "cannot create $OUT"

if [ $INIT = systemd ]; then
	INITVER=$(x /usr/bin/systemctl --version | sed -n 1p)
else
	INITVER=$(x "$(tool openrc)" --version | sed -n 1p)
fi
SOURCE=${CAPTURE_SOURCE:-}
if [ -z "$SOURCE" ]; then
	SOURCE=host
	[ ! -e /.dockerenv ] || SOURCE=container
fi
PRETTY=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')

{
	echo "host: $NAME"
	echo "source: $SOURCE"
	echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "os-release: $PRETTY"
	echo "init-version: $INITVER"
	echo "kernel: $(uname -r)"
	echo "captured-as: root"
	[ -z "${CAPTURE_IMAGE:-}" ] || echo "image: $CAPTURE_IMAGE"
	echo "redaction: none needed; only the fixed show -p properties, MESSAGE and PRIORITY of nexusd.service's journal, and the output_log, error_log, output_logger, error_logger and description lines of /etc/init.d and /etc/conf.d are read"
	echo "commands:"
} >"$OUT/PROVENANCE"

capture_systemd() {
	SYSTEMCTL=/usr/bin/systemctl
	JOURNALCTL=/usr/bin/journalctl
	PROPS=Id,LoadState,ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp,InactiveEnterTimestamp,Result,NRestarts,MemoryCurrent,FragmentPath
	LU=$OUT/list-units.txt
	LF=$OUT/list-unit-files.txt

	cap systemctl-version.txt $SYSTEMCTL --version
	cap list-units.txt $SYSTEMCTL list-units --type=service --all --plain --no-legend --no-pager --full
	cap list-unit-files.txt $SYSTEMCTL list-unit-files --type=service --plain --no-legend --no-pager --full

	# DF-1's show for nexusd, ssh or sshd, a failed and an inactive unit,
	# a template instance, a masked unit and the planted alias, when each
	# is present.
	cap show-nexusd.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- nexusd.service
	for u in ssh sshd; do
		if listed $u.service "$LU" "$LF"; then
			cap show-$u.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- $u.service
		fi
	done
	u=$(awk '$3 == "failed" { print $1; exit }' "$LU")
	[ -z "$u" ] || cap show-failed.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- "$u"
	u=$(awk '$2 == "loaded" && $3 == "inactive" { print $1; exit }' "$LU")
	[ -z "$u" ] || cap show-inactive.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- "$u"
	u=$(awk '$1 ~ /@.+[.]service$/ { print $1; exit }' "$LU")
	[ -z "$u" ] || cap show-template-instance.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- "$u"
	u=$(awk '$2 == "masked" { print $1; exit }' "$LF")
	[ -z "$u" ] || cap show-masked.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- "$u"
	if listed nx-netalias.service "$LU" "$LF" || [ -L /etc/systemd/system/nx-netalias.service ]; then
		cap show-alias.txt $SYSTEMCTL show --timestamp=unix -p $PROPS -- nx-netalias.service
	fi

	cap is-enabled-nexusd.txt $SYSTEMCTL is-enabled -- nexusd.service
	# WI 3.4's exact Dependencies argv, and A-39's evidence on its own.
	cap deps-nexusd.txt $SYSTEMCTL show -p Requires -p BindsTo -p Requisite -p PartOf -- nexusd.service
	cap stop-propagated-from-nexusd.txt $SYSTEMCTL show -p StopPropagatedFrom -- nexusd.service

	# DF-2's argv for nexusd.service: -n 20, --since 10 minutes ago, -p err.
	since=$(($(date +%s) - 600))
	for j in "n20 -n 20" "since --since=@$since.5" "perr -p err"; do
		set -- $j
		b=journal-nexusd-$1
		shift
		x $JOURNALCTL --no-pager --quiet -o json --output-fields=MESSAGE,PRIORITY -u nexusd.service "$@" \
			>"$OUT/$b.jsonl" 2>"$OUT/$b.txt"
		echo "Exit $?" >>"$OUT/$b.txt"
		note "$b.jsonl" "$JOURNALCTL --no-pager --quiet -o json --output-fields=MESSAGE,PRIORITY -u nexusd.service $*"
	done

	cap mainpid-nexusd.txt $SYSTEMCTL show -p MainPID -- nexusd.service
	PID=$(sed -n 's/^MainPID=//p' "$OUT/mainpid-nexusd.txt")
	capture_proc

	cap readlink-sbin-reboot.txt "$(tool readlink)" -f /sbin/reboot
	cap ls-systemctl-journalctl.txt "$(tool ls)" -l $SYSTEMCTL $JOURNALCTL
	cap stat-etc-pve.txt "$(tool stat)" /etc/pve

	# The oracle (NFR-2): each tool's own word for every listed name.
	awk '$1 ~ /[.]service$/ { print $1 }' "$LU" "$LF" | sort -u >"$OUT/oracle/names.txt"
	while IFS= read -r n; do
		for v in is-active is-enabled; do
			w=$(x $SYSTEMCTL $v -- "$n" 2>>"$OUT/oracle/stderr.txt")
			rc=$?
			printf '%s\t%s\t%s\n' "$n" "$(echo "$w" | sed -n 1p)" "$rc" >>"$OUT/oracle/$v.tsv"
		done
	done <"$OUT/oracle/names.txt"
	note oracle/names.txt "awk '\$1 ~ /[.]service\$/ { print \$1 }' list-units.txt list-unit-files.txt | sort -u"
	note oracle/is-active.tsv "$SYSTEMCTL is-active -- NAME (each NAME of oracle/names.txt; stderr in oracle/stderr.txt)"
	note oracle/is-enabled.tsv "$SYSTEMCTL is-enabled -- NAME (each NAME of oracle/names.txt; stderr in oracle/stderr.txt)"
	cap oracle/failed.txt $SYSTEMCTL list-units --state=failed --type=service --plain --no-legend
}

# capture_proc copies the running nexusd's (PID's) own view.
capture_proc() {
	case ${PID:-0} in
	'' | 0 | *[!0-9]*) echo "no running nexusd (MainPID '${PID:-}')" >>"$OUT/copy-status.txt" ;;
	*)
		CAT=$(tool cat)
		copy proc/self/cgroup "$CAT" /proc/"$PID"/cgroup
		copy proc/self/mountinfo "$(tool grep)" -F boot_id /proc/"$PID"/mountinfo
		copy proc/sys/kernel/osrelease "$CAT" /proc/"$PID"/root/proc/sys/kernel/osrelease
		copy proc/uptime "$CAT" /proc/"$PID"/root/proc/uptime
		;;
	esac
	copy proc/sys/kernel/random/boot_id "$(tool cat)" /proc/sys/kernel/random/boot_id
}

capture_openrc() {
	RCSERVICE=/sbin/rc-service
	GREP=$(tool grep)
	KEYS='^[	 ]*(output_log|error_log|output_logger|error_logger|description)='

	cap openrc-version.txt "$(tool openrc)" --version
	cap rc-status.txt "$(tool rc-status)" -s -C
	cap rc-update.txt "$(tool rc-update)" show -v
	cap rc-service-nexusd-status.txt $RCSERVICE -C nexusd status

	# The whole deptree, which holds service names and dependency words
	# only: nexusd's and net's entries (the net provider, A-9) and the
	# entries a resolver walks past to find them. In a Docker container
	# OpenRC leaves nexusd's `need net` out of the deptree and no script
	# provides net (networking has `keyword -docker`), so the other
	# entries are the only ineed, iprovide and providedby keys there.
	mkdir -p "$OUT/run/openrc/options/nexusd"
	copy run/openrc/deptree "$(tool cat)" /run/openrc/deptree

	cap ls-options-nexusd.txt "$(tool ls)" /run/openrc/options/nexusd
	copy run/openrc/options/nexusd/child_pid "$(tool cat)" /run/openrc/options/nexusd/child_pid
	PID=$(sed -n 1p "$OUT/run/openrc/options/nexusd/child_pid")
	capture_proc

	cap readlink-sbin-reboot.txt "$(tool readlink)" /sbin/reboot
	cap readlink-sbin-init.txt "$(tool readlink)" /sbin/init
	cap inittab-shutdown.txt "$GREP" -e :shutdown: /etc/inittab

	# Only the log and description lines of each script and conf.d file.
	mkdir -p "$OUT/etc/init.d" "$OUT/etc/conf.d"
	for d in init.d conf.d; do
		for f in /etc/$d/*; do
			[ -f "$f" ] || continue
			copy "etc/$d/${f##*/}" "$GREP" -E "$KEYS" "$f"
		done
	done

	# The oracle (NFR-2): rc-service's own word for every script.
	for f in /etc/init.d/*; do
		[ -f "$f" ] && [ -x "$f" ] || continue
		n=${f##*/}
		o=$(x $RCSERVICE -C "$n" status 2>&1)
		rc=$?
		w=$(echo "$o" | sed -n 's/.*status: *//p' | sed -n '$p')
		[ -n "$w" ] || w=$(echo "$o" | sed -n 1p)
		printf '%s\t%s\t%s\n' "$n" "$w" "$rc" >>"$OUT/oracle/rc-service-status.tsv"
	done
	note oracle/rc-service-status.tsv "$RCSERVICE -C NAME status (each executable /etc/init.d/NAME; the word after 'status:')"
}

capture_$INIT
echo "capture.sh: $NAME ($INIT, $INITVER) written to $OUT"
