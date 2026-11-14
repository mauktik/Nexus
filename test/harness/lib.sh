#!/bin/bash
# Container-side helpers of the T1 harnesses. run.sh copies this file to
# /harness/lib.sh with the shipped unit at /harness/deploy/nexusd.service
# and the shipped OpenRC script at /harness/deploy/nexusd.openrc; tests call
# `bash /harness/lib.sh <command> [args]`. The init system is detected with
# the markers platform.DetectAt uses, and each command has a systemd and an
# OpenRC form.
#
#   install-nexusd CONFIG   install the shipped unit (systemd) or script
#                           (OpenRC) unchanged, and CONFIG as
#                           /etc/nexus/nexusd.yaml, root:root 0600 (A-67)
#   install-config SRC DST  install SRC as DST, root:root 0600
#   start-nexusd            systemd: systemctl start nexusd, which returns
#                           once READY=1 arrived (Type=notify) or the start
#                           failed. OpenRC: rc-service nexusd start, then
#                           wait for the daemon's "nexusd ready" line in
#                           syslog (OpenRC has no readiness protocol)
#   stop-nexusd             stop nexusd and clear a failed (crashed) state
#   dropin NAME FILE        systemd: install FILE as nexusd.service.d/NAME.conf.
#                           OpenRC: FILE is the same systemd drop-in, of
#                           ExecStart= and Restart=no only; the script
#                           becomes the shipped one with command and
#                           command_args from ExecStart, and with
#                           Restart=no, start-stop-daemon instead of
#                           supervise-daemon (no respawn)
#   remove-dropins          restore the shipped unit or script
#   init                    print the detected init system
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
RC_SRC=/harness/deploy/nexusd.openrc
RC_SCRIPT=/etc/init.d/nexusd
READY_TIMEOUT=60

die() {
	echo "lib.sh: $*" >&2
	exit 1
}

# The same markers as platform.DetectAt.
detect_init() {
	if [ -d /run/systemd/system ]; then
		echo systemd
	elif [ -e /run/openrc ] && [ -x /sbin/openrc-run ] && [ -x /sbin/rc-service ]; then
		echo openrc
	else
		echo unknown
	fi
}

INIT=$(detect_init)

install_config() {
	local src=$1 dst=$2
	install -d -o root -g root -m 0755 "$(dirname "$dst")"
	install -o root -g root -m 0600 "$src" "$dst"
}

install_nexusd() {
	local cfg=$1
	case $INIT in
	systemd)
		[ -f "$UNIT_SRC" ] || die "no shipped unit at $UNIT_SRC"
		install -o root -g root -m 0644 "$UNIT_SRC" "$UNIT_DIR/nexusd.service"
		install_config "$cfg" /etc/nexus/nexusd.yaml
		systemctl daemon-reload
		;;
	openrc)
		[ -f "$RC_SRC" ] || die "no shipped OpenRC script at $RC_SRC"
		install -o root -g root -m 0755 "$RC_SRC" "$RC_SCRIPT"
		install_config "$cfg" /etc/nexus/nexusd.yaml
		;;
	*) die "no supported init system" ;;
	esac
}

# rc_status prints started, stopped, crashed, starting, stopping or
# inactive for the nexusd service.
rc_status() {
	local out
	out=$(rc-service nexusd status 2>&1 || true)
	case $out in
	*crashed*) echo crashed ;;
	*started*) echo started ;;
	*starting*) echo starting ;;
	*stopping*) echo stopping ;;
	*inactive*) echo inactive ;;
	*) echo stopped ;;
	esac
}

messages_lines() {
	if [ -f /var/log/messages ]; then
		wc -l </var/log/messages
	else
		echo 0
	fi
}

start_nexusd() {
	case $INIT in
	systemd)
		if ! systemctl start nexusd.service; then
			systemctl --no-pager --full status nexusd.service >&2 || true
			journalctl -u nexusd.service --no-pager -n 50 >&2 || true
			die "nexusd.service did not start"
		fi
		;;
	openrc)
		local before deadline st
		# A crashed service is still marked started: zap it first.
		[ "$(rc_status)" != crashed ] || rc-service nexusd zap >/dev/null
		before=$(messages_lines)
		if ! rc-service nexusd start; then
			tail -n 50 /var/log/messages >&2 || true
			die "rc-service nexusd start failed"
		fi
		deadline=$((SECONDS + READY_TIMEOUT))
		until tail -n "+$((before + 1))" /var/log/messages 2>/dev/null | grep -q 'nexusd: .*nexusd ready'; do
			st=$(rc_status)
			if [ "$st" = crashed ] || [ "$st" = stopped ] || [ $SECONDS -ge $deadline ]; then
				tail -n "+$((before + 1))" /var/log/messages >&2 || true
				die "nexusd not ready (status $st) within ${READY_TIMEOUT}s"
			fi
			sleep 0.1
		done
		;;
	*) die "no supported init system" ;;
	esac
}

stop_nexusd() {
	case $INIT in
	systemd)
		systemctl stop nexusd.service 2>/dev/null || true
		systemctl reset-failed nexusd.service 2>/dev/null || true
		;;
	openrc)
		[ -x "$RC_SCRIPT" ] || return 0
		rc-service nexusd stop >/dev/null 2>&1 || true
		case $(rc_status) in
		stopped) ;;
		*) rc-service nexusd zap >/dev/null 2>&1 || true ;;
		esac
		;;
	esac
}

# openrc_dropin rewrites the shipped script from a systemd drop-in that
# holds only [Service], ExecStart= and Restart=no.
openrc_dropin() {
	local file=$1 line exec="" norestart=0 cmd args
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		"" | "[Service]" | "#"* | "ExecStart=") ;;
		ExecStart=*) exec=${line#ExecStart=} ;;
		Restart=no) norestart=1 ;;
		*) die "dropin: no OpenRC form for directive: $line" ;;
		esac
	done <"$file"
	[ -n "$exec" ] || die "dropin: no ExecStart in $file"
	case $exec in
	*\"* | *\'* | *\$* | *\\*) die "dropin: ExecStart needs no quoting here: $exec" ;;
	esac
	cmd=${exec%% *}
	args=${exec#"$cmd"}
	args=${args# }
	[ -f "$RC_SRC" ] || die "no shipped OpenRC script at $RC_SRC"
	local tmp=$RC_SCRIPT.harness-new
	sed -e "s|^command=.*|command=$cmd|" -e "s|^command_args=.*|command_args=\"$args\"|" "$RC_SRC" >"$tmp"
	if [ $norestart -eq 1 ]; then
		sed -i -e 's|^supervisor=supervise-daemon$|command_background=yes|' \
			-e 's|^\([[:space:]]*\)supervise-daemon "$RC_SVCNAME" --signal HUP$|\1start-stop-daemon --signal HUP --pidfile "$pidfile"|' "$tmp"
		grep -q '^command_background=yes$' "$tmp" || die "dropin: the shipped script has no supervisor line to replace"
		grep -q 'start-stop-daemon --signal HUP' "$tmp" || die "dropin: the shipped script has no reload line to replace"
	fi
	chmod 0755 "$tmp"
	mv "$tmp" "$RC_SCRIPT"
}

dropin() {
	local name=$1 file=$2
	case $INIT in
	systemd)
		install -d -o root -g root -m 0755 "$DROPIN_DIR"
		install -o root -g root -m 0644 "$file" "$DROPIN_DIR/$name.conf"
		systemctl daemon-reload
		;;
	openrc) openrc_dropin "$file" ;;
	*) die "no supported init system" ;;
	esac
}

remove_dropins() {
	case $INIT in
	systemd)
		rm -rf "$DROPIN_DIR"
		systemctl daemon-reload
		;;
	openrc)
		[ -f "$RC_SRC" ] || die "no shipped OpenRC script at $RC_SRC"
		[ ! -e "$RC_SCRIPT" ] || install -o root -g root -m 0755 "$RC_SRC" "$RC_SCRIPT"
		;;
	esac
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
init) echo "$INIT" ;;
install-release) [ $# -eq 1 ] || die "usage: install-release TREE"; install_release "$1" ;;
nexusd-version) [ $# -le 1 ] || die "usage: nexusd-version [BIN]"; nexusd_version "$@" ;;
*) die "unknown command $cmd" ;;
esac
