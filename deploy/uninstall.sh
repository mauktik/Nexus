#!/bin/sh
# Removes nexusd (FR-36, FR-24):
#
#   sudo ./uninstall.sh            stop nexusd and take it out of boot, remove
#                                  the binary and the unit (systemd) or
#                                  /etc/init.d/nexusd (OpenRC); keep the config,
#                                  the audit log and the operation history
#   sudo ./uninstall.sh --purge    also delete /etc/nexus, /var/lib/nexus,
#                                  /var/log/nexus and any nexusd.service
#                                  drop-ins (systemd) or /etc/conf.d/nexusd
#                                  (OpenRC)
#
# Operations run in their own nexus-op-* units (systemd) or supervisor
# processes (OpenRC), which uninstalling does not stop. The script lists any
# still running before it removes anything. On a host running neither
# systemd nor OpenRC it refuses and changes nothing.
set -eu

BIN=/usr/local/bin/nexusd
UNIT=/etc/systemd/system/nexusd.service
DROPINS=/etc/systemd/system/nexusd.service.d
INITD=/etc/init.d/nexusd
CONFD=/etc/conf.d/nexusd
OPS=/var/lib/nexus/ops
KEPT="/etc/nexus /var/lib/nexus /var/log/nexus"

say() { printf '%s\n' "uninstall.sh: $*"; }
die() {
	printf '%s\n' "uninstall.sh: $*" >&2
	exit 1
}
usage() { printf '%s\n' "usage: $0 [--purge]"; }

is_exec() { [ -f "$1" ] && [ -x "$1" ]; }

# detect_init prints systemd or openrc, found by the markers
# platform.DetectAt uses, or nothing for any other init system.
detect_init() {
	if [ -d /run/systemd/system ]; then
		echo systemd
	elif [ -e /run/openrc ] && is_exec /sbin/openrc-run && is_exec /sbin/rc-service; then
		echo openrc
	fi
}

# live_supervisors prints, one per line, the ID of each operation whose
# supervisor.json names a process that is still an op-supervise process.
live_supervisors() {
	for sv_file in "$OPS"/*/supervisor.json; do
		[ -f "$sv_file" ] || continue
		sv_pid=$(sed -n 's/.*"pid": *\([0-9][0-9]*\).*/\1/p' "$sv_file")
		[ -n "$sv_pid" ] && [ -r "/proc/$sv_pid/cmdline" ] || continue
		# cmdline separates the arguments with NULs.
		if tr '\000' ' ' <"/proc/$sv_pid/cmdline" | grep -q op-supervise; then
			sv_dir=${sv_file%/supervisor.json}
			printf '%s\n' "${sv_dir##*/}"
		fi
	done
}

purge=0
case $# in
0) ;;
1)
	case $1 in
	--purge) purge=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
	;;
*)
	usage >&2
	exit 2
	;;
esac
[ "$(id -u)" -eq 0 ] || die "must run as root: sudo $0"
init=$(detect_init)
case $init in
systemd) command -v systemctl >/dev/null || die "systemctl not found" ;;
openrc) ;;
*) die "neither systemd nor OpenRC is running on this host; nexusd runs only as a systemd or OpenRC service. Nothing was changed" ;;
esac

if [ "$init" = systemd ]; then
	# Operation units still running: UNIT LOAD ACTIVE SUB DESCRIPTION. The
	# listing is taken first so that a systemctl failure stops the script.
	units=$(systemctl list-units --all --type=service --no-legend --plain 'nexus-op-*')
	running=$(printf '%s\n' "$units" | awk '$3 != "inactive" && $3 != "failed" { print $1 }' | xargs)
	if [ -n "$running" ]; then
		echo "uninstall.sh: warning: operation units still running: $running" >&2
		echo "uninstall.sh: warning: they are not stopped and run to completion; stop one with: nexus operation cancel ID, or systemctl kill --kill-whom=main --signal=SIGTERM UNIT (systemctl stop is refused)" >&2
		if [ $purge -eq 1 ]; then
			echo "uninstall.sh: warning: --purge deletes their records and output while they run" >&2
		else
			echo "uninstall.sh: warning: their records stay in /var/lib/nexus, and a reinstalled nexusd reports their outcome" >&2
		fi
	fi

	if [ "$(systemctl show -p LoadState --value nexusd.service)" = loaded ]; then
		systemctl disable --now nexusd.service
		say "stopped and disabled nexusd.service"
	fi
	rm -f "$UNIT" "$BIN"
	if [ $purge -eq 1 ]; then
		rm -rf "$DROPINS"
	fi
	systemctl daemon-reload
	systemctl reset-failed nexusd.service 2>/dev/null || true
	say "removed $BIN and $UNIT"
	removed_extra=
else
	running=$(live_supervisors | xargs)
	if [ -n "$running" ]; then
		echo "uninstall.sh: warning: operation supervisors still running: $running" >&2
		echo "uninstall.sh: warning: they are not stopped and run to completion; stop one with: nexus operation cancel ID" >&2
		if [ $purge -eq 1 ]; then
			echo "uninstall.sh: warning: --purge deletes their records and output while they run" >&2
		else
			echo "uninstall.sh: warning: their records stay in /var/lib/nexus, and a reinstalled nexusd reports their outcome" >&2
		fi
	fi

	# rc-service exits 0 from status only when the service is started.
	if [ -e "$INITD" ] && rc-service nexusd status >/dev/null 2>&1; then
		rc-service nexusd stop
		say "stopped nexusd"
	fi
	# A runlevel lists nexusd by a link in its /etc/runlevels directory.
	for link in /etc/runlevels/*/nexusd; do
		[ -L "$link" ] || [ -e "$link" ] || continue
		level=${link%/nexusd}
		level=${level##*/}
		rc-update del nexusd "$level"
		say "removed nexusd from the $level runlevel"
	done
	rm -f "$INITD" "$BIN"
	say "removed $BIN and $INITD"
	removed_extra=" $CONFD"
fi

if [ $purge -eq 1 ]; then
	# shellcheck disable=SC2086 # KEPT and removed_extra are lists of fixed paths.
	rm -rf $KEPT $removed_extra
	say "purged $KEPT$removed_extra"
else
	say "kept the configuration (/etc/nexus), operation history, tokens and TLS key (/var/lib/nexus) and audit log (/var/log/nexus); delete them with --purge"
fi
