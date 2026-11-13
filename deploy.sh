#!/bin/sh
# Builds nexusd from this checkout and installs it on this host.
#
#   ./deploy.sh                 build, then install with sudo (install or upgrade)
#   ./deploy.sh --stage-only DIR
#                               build for linux/amd64 and stage the install tree
#                               in DIR, without sudo or installing (works on any
#                               OS with Go)
#   ./deploy.sh --uninstall [--purge]
#                               run deploy/uninstall.sh with sudo
#
# Run it as your normal user from the repository root on the x86-64 host,
# Ubuntu (systemd) or Alpine (OpenRC). It builds as you, so no Go cache or
# build output ends up owned by root, and calls sudo only for the install
# step. The install itself is deploy/install.sh, the same installer the
# release tarball carries:
#
#   - The first install writes /etc/nexus/nexusd.yaml (listening on loopback
#     only, every provider disabled). It does not start the service.
#   - Later runs replace the binary and the unit or service script, keep the
#     config, the tokens, the certificate and the history, and restart a
#     running nexusd.
#
# Needs Go 1.21 or newer on PATH. go.mod pins the exact toolchain, which Go
# downloads on first use. To install Go on Ubuntu, run one of:
#   sudo apt install golang-go
#   sudo snap install go --classic
# On Alpine: apk add go.
#
# NEXUS_HOST_ROOT, empty on a real host, is for test/buildsys only: the
# host check looks for the init-system markers under it instead of /.
set -eu

MODULE=github.com/mauktik/nexus
ROOT=$(cd "$(dirname "$0")" && pwd)
HOST_ROOT=${NEXUS_HOST_ROOT:-}

say() { printf '%s\n' "deploy.sh: $*"; }
die() {
	printf '%s\n' "deploy.sh: $*" >&2
	exit 1
}
usage() {
	printf '%s\n' "usage: $0 [--stage-only DIR | --uninstall [--purge]]"
}

is_exec() { [ -f "$1" ] && [ -x "$1" ]; }

# host_init prints systemd or openrc, found by the markers platform.DetectAt
# uses, or nothing for any other init system.
host_init() {
	if [ -d "$HOST_ROOT/run/systemd/system" ]; then
		echo systemd
	elif [ -e "$HOST_ROOT/run/openrc" ] && is_exec "$HOST_ROOT/sbin/openrc-run" &&
		is_exec "$HOST_ROOT/sbin/rc-service"; then
		echo openrc
	fi
}

mode=install
stage=
goarch=amd64
uninstall_arg=
case "${1:-}" in
"") ;;
--stage-only)
	[ "$#" -eq 2 ] || { usage >&2; exit 2; }
	mode=stage
	stage=$2
	;;
--uninstall)
	shift
	if [ "$#" -gt 1 ] || { [ "$#" -eq 1 ] && [ "$1" != --purge ]; }; then
		usage >&2
		exit 2
	fi
	mode=uninstall
	uninstall_arg=${1:-}
	;;
-h | --help)
	usage
	exit 0
	;;
*)
	usage >&2
	exit 2
	;;
esac

[ -f "$ROOT/go.mod" ] && [ -d "$ROOT/cmd/nexusd" ] && [ -f "$ROOT/deploy/install.sh" ] ||
	die "run this from a Nexus checkout: $ROOT has no go.mod, cmd/nexusd or deploy/install.sh"

if [ "$mode" = uninstall ]; then
	[ "$(id -u)" -ne 0 ] || die "run as your normal user; it calls sudo itself"
	exec sudo "$ROOT/deploy/uninstall.sh" ${uninstall_arg:+"$uninstall_arg"}
fi

if [ "$mode" = install ]; then
	[ "$(id -u)" -ne 0 ] ||
		die "run as your normal user, not with sudo: it builds as you and calls sudo only to install"
	[ "$(uname -s)" = Linux ] || die "nexusd installs only on Linux; use --stage-only DIR to build elsewhere"
	# Build for this host. x86-64 is the supported platform; arm64 also works
	# (the test harness runs it) but has had no real-hardware testing.
	case "$(uname -m)" in
	x86_64 | amd64) goarch=amd64 ;;
	aarch64 | arm64) goarch=arm64 ;;
	*) die "nexusd builds for x86-64 (or arm64) only; this host is $(uname -m)" ;;
	esac
	[ -n "$(host_init)" ] ||
		die "neither systemd nor OpenRC is running on this host; nexusd runs only as a systemd or OpenRC service"
	command -v sudo >/dev/null || die "sudo is not installed"
fi

command -v go >/dev/null ||
	die "go is not on PATH. Install it with: sudo apt install golang-go (or: sudo snap install go --classic)"

cd "$ROOT"
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
	say "warning: the checkout has uncommitted changes; building them as $version"
fi

if [ "$mode" = install ]; then
	work=$(mktemp -d "${TMPDIR:-/tmp}/nexus-deploy.XXXXXX")
	trap 'rm -rf "$work"' EXIT
	stage=$work/nexusd-$version-linux-$goarch
fi
rm -rf "$stage"
mkdir -p "$stage"
stage=$(cd "$stage" && pwd)

say "building nexusd $version for linux/$goarch (the first build downloads the pinned Go toolchain and modules)"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath \
	-ldflags="-s -w -X $MODULE/internal/version.Version=$version" \
	-o "$stage/nexusd" ./cmd/nexusd

# The same tree `make release` packs into the nexusd tarball.
cp deploy/nexusd.service deploy/nexusd.openrc deploy/nexusd.default.yaml deploy/install.sh deploy/uninstall.sh README.md "$stage/"
mkdir -p "$stage/docs"
cp -R docs/. "$stage/docs/"
chmod -R u=rwX,go=rX "$stage"
chmod 0755 "$stage/nexusd" "$stage/nexusd.openrc" "$stage/install.sh" "$stage/uninstall.sh"

if [ "$mode" = stage ]; then
	say "staged nexusd $version in $stage; install it on the host with: sudo $stage/install.sh"
	exit 0
fi

say "installing with sudo (you may be asked for your password)"
# The build tree is private to you. Hand the root-run installer a root-owned
# copy so it never executes files another account could change mid-install.
sudo install -d -m 0755 -o root -g root "$work/root"
sudo cp -R "$stage" "$work/root/"
sudo chown -R root:root "$work/root"
trap 'sudo rm -rf "$work/root"; rm -rf "$work"' EXIT
sudo "$work/root/$(basename "$stage")/install.sh"
