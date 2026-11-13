#!/bin/bash
# Container-side helpers of the T1 harness. run.sh copies this file to
# /harness/lib.sh with the shipped unit at /harness/deploy/nexusd.service;
# tests call `bash /harness/lib.sh <command> [args]`.
#
#   install-nexusd CONFIG   install the shipped unit unchanged, and CONFIG as
#                           /etc/nexus/nexusd.yaml, root:root 0600 (A-67)
#   install-config SRC DST  install SRC as DST, root:root 0600
#   start-nexusd            systemctl start nexusd: returns once READY=1
#                           arrived (Type=notify) or the start failed
#   stop-nexusd             stop nexusd and clear a failed state
#   dropin NAME FILE        install FILE as nexusd.service.d/NAME.conf
#   remove-dropins          remove every nexusd.service drop-in
#   install-release TREE    run TREE's install.sh as an operator does (bash
#                           install.sh) and exit with its status; TREE is an
#                           unpacked nexusd-<v>-linux-<goarch> tree, or such a
#                           .tar.gz (e.g. in /harness/selfupdate), unpacked
#                           into a temporary directory first
#   nexusd-version [BIN]    print BIN -version (default /usr/local/bin/nexusd)
set -euo pipefail

UNIT_SRC=/harness/deploy/nexusd.service
UNIT_DIR=/etc/systemd/system
DROPIN_DIR=$UNIT_DIR/nexusd.service.d

die() {
	echo "lib.sh: $*" >&2
	exit 1
}

install_config() {
	local src=$1 dst=$2
	install -d -o root -g root -m 0755 "$(dirname "$dst")"
	install -o root -g root -m 0600 "$src" "$dst"
}

install_nexusd() {
	local cfg=$1
	[ -f "$UNIT_SRC" ] || die "no shipped unit at $UNIT_SRC"
	install -o root -g root -m 0644 "$UNIT_SRC" "$UNIT_DIR/nexusd.service"
	install_config "$cfg" /etc/nexus/nexusd.yaml
	systemctl daemon-reload
}

start_nexusd() {
	if ! systemctl start nexusd.service; then
		systemctl --no-pager --full status nexusd.service >&2 || true
		journalctl -u nexusd.service --no-pager -n 50 >&2 || true
		die "nexusd.service did not start"
	fi
}

stop_nexusd() {
	systemctl stop nexusd.service 2>/dev/null || true
	systemctl reset-failed nexusd.service 2>/dev/null || true
}

dropin() {
	local name=$1 file=$2
	install -d -o root -g root -m 0755 "$DROPIN_DIR"
	install -o root -g root -m 0644 "$file" "$DROPIN_DIR/$name.conf"
	systemctl daemon-reload
}

remove_dropins() {
	rm -rf "$DROPIN_DIR"
	systemctl daemon-reload
}

install_release() {
	local src=$1 tmp="" tree rc=0
	if [ -d "$src" ]; then
		tree=$src
	else
		case $src in
		*.tar.gz) [ -f "$src" ] || die "no release tarball $src" ;;
		*) die "install-release: $src is neither a release tree nor a .tar.gz" ;;
		esac
		tmp=$(mktemp -d)
		tar -xzf "$src" -C "$tmp" || {
			rm -rf "$tmp"
			die "cannot unpack $src"
		}
		tree=$tmp/$(basename "$src" .tar.gz)
	fi
	if [ -f "$tree/install.sh" ]; then
		bash "$tree/install.sh" || rc=$?
	else
		echo "lib.sh: no install.sh in $tree" >&2
		rc=1
	fi
	[ -z "$tmp" ] || rm -rf "$tmp"
	return $rc
}

nexusd_version() {
	"${1:-/usr/local/bin/nexusd}" -version
}

cmd=${1:-}
[ -n "$cmd" ] || die "no command"
shift
case $cmd in
install-nexusd) [ $# -eq 1 ] || die "usage: install-nexusd CONFIG"; install_nexusd "$1" ;;
install-config) [ $# -eq 2 ] || die "usage: install-config SRC DST"; install_config "$1" "$2" ;;
start-nexusd) start_nexusd ;;
stop-nexusd) stop_nexusd ;;
dropin) [ $# -eq 2 ] || die "usage: dropin NAME FILE"; dropin "$1" "$2" ;;
remove-dropins) remove_dropins ;;
install-release) [ $# -eq 1 ] || die "usage: install-release TREE"; install_release "$1" ;;
nexusd-version) [ $# -le 1 ] || die "usage: nexusd-version [BIN]"; nexusd_version "$@" ;;
*) die "unknown command $cmd" ;;
esac
