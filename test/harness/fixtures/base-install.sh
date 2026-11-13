#!/bin/bash
# Installs nexusd from an unpacked release directory (FR-36):
#
#   sudo ./install.sh
#
# It needs only bash, coreutils and systemd: no compiler or language runtime.
#
#   /usr/local/bin/nexusd               the binary, replaced on every run
#   /etc/systemd/system/nexusd.service  the hardened unit, replaced on every run
#   /etc/nexus/nexusd.yaml              written from nexusd.default.yaml, root:root
#                                       0600, only when no config exists there: an
#                                       existing config is never changed
#
# The default config listens on 127.0.0.1:7443 only, and this script never
# writes any other listen address. A first install does not enable or start
# the service. On an upgrade a running nexusd is restarted onto the new
# binary; operations in progress keep running in their own units (FR-6).
set -euo pipefail

BIN=/usr/local/bin/nexusd
UNIT=/etc/systemd/system/nexusd.service
CONF_DIR=/etc/nexus
CONF=$CONF_DIR/nexusd.yaml

SRC=$(cd "$(dirname "$0")" && pwd)

say() { echo "install.sh: $*"; }
die() {
	echo "install.sh: $*" >&2
	exit 1
}

# loopback_only FILE succeeds when FILE has exactly one top-level listen line,
# a flow list whose every address is 127.x.x.x:port or [::1]:port.
loopback_only() {
	local lines addrs a
	lines=$(grep -E '^listen:' "$1" || true)
	[ -n "$lines" ] && [ "$(printf '%s\n' "$lines" | wc -l)" -eq 1 ] || return 1
	addrs=$(printf '%s\n' "$lines" | sed -nE 's/^listen:[[:space:]]*\[([^]#]*)\][[:space:]]*(#.*)?$/\1/p' | tr -d "\"' " | tr ',' ' ')
	[ -n "$addrs" ] || return 1
	for a in $addrs; do
		case $a in
		127.*:[0-9]* | '[::1]':[0-9]*) ;;
		*) return 1 ;;
		esac
	done
}

[ "$#" -eq 0 ] || die "usage: $0 (no arguments)"
[ "$(id -u)" -eq 0 ] || die "must run as root: sudo $0"
for f in nexusd nexusd.service nexusd.default.yaml; do
	[ -f "$SRC/$f" ] || die "$f is missing next to install.sh in $SRC"
done
# nexusd runs only as a systemd service on the host (CON-4).
[ -d /run/systemd/system ] && command -v systemctl >/dev/null ||
	die "systemd is not running on this host; nexusd runs only as a systemd service"
version=$("$SRC/nexusd" -version 2>&1) ||
	die "$SRC/nexusd does not run on this host ($(uname -m)): $version"
loopback_only "$SRC/nexusd.default.yaml" ||
	die "the bundled nexusd.default.yaml does not listen on loopback only; refusing to install it"

# Rename into place, so a running nexusd or an operation unit that maps the
# old binary is never written to.
install -o root -g root -m 0755 "$SRC/nexusd" "$BIN.new"
mv -f "$BIN.new" "$BIN"
install -o root -g root -m 0644 "$SRC/nexusd.service" "$UNIT.new"
mv -f "$UNIT.new" "$UNIT"
say "installed $version as $BIN and $UNIT"

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

systemctl daemon-reload

if ! "$BIN" validate-config --config "$CONF"; then
	die "$CONF is not valid; fix it, then check it with: $BIN validate-config --config $CONF"
fi

if systemctl is-active --quiet nexusd.service; then
	systemctl restart nexusd.service
	say "restarted the running nexusd.service onto the new binary"
else
	say "next steps:"
	say "  enable providers in $CONF and check it with: $BIN validate-config"
	say "  create a token: $BIN token create --name NAME --scope SCOPE"
	say "  start nexusd: systemctl enable --now nexusd.service"
fi
