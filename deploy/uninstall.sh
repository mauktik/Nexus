#!/bin/bash
# Removes nexusd (FR-36):
#
#   sudo ./uninstall.sh            stop and disable nexusd.service, remove the
#                                  binary and the unit; keep the config, the
#                                  audit log and the operation history
#   sudo ./uninstall.sh --purge    also delete /etc/nexus, /var/lib/nexus,
#                                  /var/log/nexus and any nexusd.service drop-ins
#
# Operations run in their own nexus-op-* units, which uninstalling does not
# stop. The script lists any still running before it removes anything.
set -euo pipefail

BIN=/usr/local/bin/nexusd
UNIT=/etc/systemd/system/nexusd.service
DROPINS=/etc/systemd/system/nexusd.service.d
KEPT="/etc/nexus /var/lib/nexus /var/log/nexus"

say() { echo "uninstall.sh: $*"; }
die() {
	echo "uninstall.sh: $*" >&2
	exit 1
}
usage() { echo "usage: $0 [--purge]"; }

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
command -v systemctl >/dev/null || die "systemctl not found"

# Operation units still running: UNIT LOAD ACTIVE SUB DESCRIPTION.
running=$(systemctl list-units --all --type=service --no-legend --plain 'nexus-op-*' |
	awk '$3 != "inactive" && $3 != "failed" { print $1 }' | xargs)
if [ -n "$running" ]; then
	echo "uninstall.sh: warning: operation units still running: $running" >&2
	echo "uninstall.sh: warning: they are not stopped and run to completion; stop one with: systemctl stop UNIT" >&2
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

if [ $purge -eq 1 ]; then
	# shellcheck disable=SC2086 # KEPT is a list of fixed paths.
	rm -rf $KEPT
	say "purged $KEPT"
else
	say "kept the configuration (/etc/nexus), operation history, tokens and TLS key (/var/lib/nexus) and audit log (/var/log/nexus); delete them with --purge"
fi
