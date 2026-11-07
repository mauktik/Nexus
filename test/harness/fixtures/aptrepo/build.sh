#!/bin/bash
# Fixture apt repository for the T1 apt tests (architecture.md, "How the Apt
# tests stay real and network-free"). Packages are built with dpkg-deb and
# indexed with dpkg-scanpackages into a flat repository that apt reads as
#
#   deb [trusted=yes] file:REPO ./
#
# Every directory on the way is mode 0755: a 0700 root (as mktemp -d makes)
# silently broke installs during the spike, because apt reads the repository
# as the unprivileged _apt user.
#
#   build.sh init REPO                    create REPO (0755) with an empty index
#   build.sh add REPO NAME VERSION        build NAME_VERSION_all.deb into REPO and
#                                         reindex; prints the .deb's path
#   build.sh deb OUT NAME VERSION         build the .deb into the directory OUT
#                                         without indexing it; prints its path
#   build.sh index REPO                   regenerate REPO/Packages
#
# NAME and VERSION are used as given; callers pass fixed fixture values.
set -euo pipefail

die() {
	echo "aptrepo/build.sh: $*" >&2
	exit 1
}

build_deb() {
	local out=$1 name=$2 version=$3
	local work
	work=$(mktemp -d)
	# The package's root entry takes this directory's mode.
	chmod 0755 "$work"
	install -d -m 0755 "$work/DEBIAN" "$work/usr/share/doc/$name"
	printf '%s\n' "$version" >"$work/usr/share/doc/$name/fixture-version"
	cat >"$work/DEBIAN/control" <<EOF
Package: $name
Version: $version
Architecture: all
Maintainer: Nexus fixtures <fixtures@nexus.invalid>
Section: misc
Priority: optional
Description: Nexus apt fixture package $name
 A package built by the Nexus T1 harness. It installs one file.
EOF
	install -d -m 0755 "$out"
	local deb="$out/${name}_${version}_all.deb"
	dpkg-deb --root-owner-group --build "$work" "$deb" >/dev/null
	rm -rf "$work"
	chmod 0644 "$deb"
	echo "$deb"
}

index() {
	local repo=$1
	[ -d "$repo" ] || die "no repository at $repo"
	# --multiversion keeps every version in the index, so a later version
	# added next to an earlier one is the candidate by version order.
	(cd "$repo" && dpkg-scanpackages --multiversion . /dev/null 2>/dev/null >Packages)
	chmod 0644 "$repo/Packages"
}

init() {
	local repo=$1
	install -d -m 0755 "$repo"
	chmod 0755 "$repo"
	index "$repo"
}

cmd=${1:-}
[ -n "$cmd" ] || die "no command"
shift
case $cmd in
init) [ $# -eq 1 ] || die "usage: init REPO"; init "$1" ;;
add)
	[ $# -eq 3 ] || die "usage: add REPO NAME VERSION"
	[ -d "$1" ] || init "$1"
	build_deb "$1" "$2" "$3"
	index "$1"
	;;
deb) [ $# -eq 3 ] || die "usage: deb OUT NAME VERSION"; build_deb "$1" "$2" "$3" ;;
index) [ $# -eq 1 ] || die "usage: index REPO"; index "$1" ;;
*) die "unknown command $cmd" ;;
esac
