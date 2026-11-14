#!/bin/sh
# Installs nexusd from an unpacked release directory (FR-36, FR-24):
#
#   sudo ./install.sh
#
# It needs only a POSIX sh (dash on Ubuntu, BusyBox ash on Alpine), the base
# tools of the host and systemd or OpenRC: no compiler or language runtime.
#
#   /usr/local/bin/nexusd               the binary, replaced on every run
#   /etc/systemd/system/nexusd.service  the hardened unit, replaced on every
#                                       run (systemd)
#   /etc/init.d/nexusd                  the service script, replaced on every
#                                       run (OpenRC)
#   /etc/nexus/nexusd.yaml              written from nexusd.default.yaml, root:root
#                                       0600, only when no config exists there: an
#                                       existing config is never changed
#
# The default config listens on 127.0.0.1:7443 only, and this script never
# writes any other listen address. A first install does not enable or start
# the service, and on OpenRC never adds it to a runlevel. On an upgrade a
# running nexusd is restarted onto the new binary; operations in progress
# keep running in their own units or supervisor processes (FR-6). On a host
# running neither systemd nor OpenRC it refuses and changes nothing.
set -eu

BIN=/usr/local/bin/nexusd
UNIT=/etc/systemd/system/nexusd.service
INITD=/etc/init.d/nexusd
CONF_DIR=/etc/nexus
CONF=$CONF_DIR/nexusd.yaml

SRC=$(cd "$(dirname "$0")" && pwd)

say() { printf '%s\n' "install.sh: $*"; }
die() {
	printf '%s\n' "install.sh: $*" >&2
	exit 1
}

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

# loopback_only FILE succeeds when FILE has exactly one top-level listen line,
# a flow list whose every address is 127.x.x.x:port or [::1]:port. The
# bracket "[ [:space:]]" is "[[:space:]]" written without a "[[".
loopback_only() {
	lo_lines=$(grep -E '^listen:' "$1" || true)
	[ -n "$lo_lines" ] && [ "$(printf '%s\n' "$lo_lines" | wc -l)" -eq 1 ] || return 1
	lo_addrs=$(printf '%s\n' "$lo_lines" | sed -nE 's/^listen:[ [:space:]]*\[([^]#]*)\][ [:space:]]*(#.*)?$/\1/p' | tr -d "\"' " | tr ',' ' ')
	[ -n "$lo_addrs" ] || return 1
	for lo_a in $lo_addrs; do
		case $lo_a in
		127.*:[0-9]* | '[::1]':[0-9]*) ;;
		*) return 1 ;;
		esac
	done
}

[ "$#" -eq 0 ] || die "usage: $0 (no arguments)"
[ "$(id -u)" -eq 0 ] || die "must run as root: sudo $0"
init=$(detect_init)
if [ "$init" = openrc ]; then
	service_file=nexusd.openrc
else
	service_file=nexusd.service
fi
for f in nexusd "$service_file" nexusd.default.yaml; do
	[ -f "$SRC/$f" ] || die "$f is missing next to install.sh in $SRC"
done
# nexusd runs only as a systemd service or an OpenRC service on the host
# (CON-4, FR-24).
case $init in
systemd)
	command -v systemctl >/dev/null ||
		die "systemd is not running on this host; nexusd runs only as a systemd service"
	;;
openrc) ;;
*) die "neither systemd nor OpenRC is running on this host; nexusd runs only as a systemd or OpenRC service. Nothing was changed" ;;
esac
version=$("$SRC/nexusd" -version 2>&1) ||
	die "$SRC/nexusd does not run on this host ($(uname -m)): $version"
loopback_only "$SRC/nexusd.default.yaml" ||
	die "the bundled nexusd.default.yaml does not listen on loopback only; refusing to install it"

# Rename into place, so a running nexusd or an operation that maps the old
# binary is never written to.
install -o root -g root -m 0755 "$SRC/nexusd" "$BIN.new"
mv -f "$BIN.new" "$BIN"
if [ "$init" = systemd ]; then
	install -o root -g root -m 0644 "$SRC/nexusd.service" "$UNIT.new"
	mv -f "$UNIT.new" "$UNIT"
	say "installed $version as $BIN and $UNIT"
else
	# A dot name, so OpenRC never sees the half-installed copy as a service.
	install -o root -g root -m 0755 "$SRC/nexusd.openrc" "/etc/init.d/.nexusd.new"
	mv -f "/etc/init.d/.nexusd.new" "$INITD"
	say "installed $version as $BIN and $INITD"
fi

if [ -e "$CONF" ] || [ -L "$CONF" ]; then
	say "kept the existing $CONF unchanged"
else
	[ -d "$CONF_DIR" ] || install -d -o root -g root -m 0700 "$CONF_DIR"
	tmp=$(mktemp "$CONF_DIR/.nexusd.yaml.XXXXXX")
	trap 'rm -f "$tmp"' EXIT
	cat "$SRC/nexusd.default.yaml" >"$tmp"
	chown root:root "$tmp"
	chmod 0600 "$tmp"
	# link(2) fails if a config appeared meanwhile, so nothing is overwritten.
	ln "$tmp" "$CONF" || die "$CONF appeared during the install; left it unchanged"
	say "wrote the default configuration to $CONF (root:root 0600, listen 127.0.0.1:7443, every provider disabled)"
fi

if [ "$init" = systemd ]; then
	systemctl daemon-reload
fi

if ! "$BIN" validate-config --config "$CONF"; then
	die "$CONF is not valid; fix it, then check it with: $BIN validate-config --config $CONF"
fi

if [ "$init" = systemd ]; then
	if systemctl is-active --quiet nexusd.service; then
		systemctl restart nexusd.service
		say "restarted the running nexusd.service onto the new binary"
	else
		say "next steps:"
		say "  enable providers in $CONF and check it with: $BIN validate-config"
		say "  create a token: $BIN token create --name NAME --scope SCOPE"
		say "  start nexusd: systemctl enable --now nexusd.service"
	fi
# rc-service exits 0 from status only when the service is started. This
# script never adds nexusd to a runlevel: that is the operator's choice.
elif rc-service nexusd status >/dev/null 2>&1; then
	rc-service nexusd restart
	say "restarted the running nexusd service onto the new binary"
else
	say "next steps:"
	say "  enable providers in $CONF and check it with: $BIN validate-config"
	say "  create a token: $BIN token create --name NAME --scope SCOPE"
	say "  start nexusd at boot: rc-update add nexusd default"
	say "  start nexusd now: rc-service nexusd start"
fi
