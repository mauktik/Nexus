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
# add and deb take options after VERSION:
#
#   --gate FILE    the postinst waits until FILE exists (at most 600 s), so
#                  dpkg holds its frontend lock mid-postinst until the test
#                  creates FILE
#   --conffile     the package ships /etc/NAME.conf as a conffile holding
#                  "NAME VERSION"
#   --depends LIST    Depends: LIST (a control field value, e.g. "nxdep")
#   --conflicts LIST  Conflicts: LIST
#   --fail-postinst   the postinst exits 1, so configuring the package fails
#   --reboot-marker   the postinst creates /var/run/reboot-required and adds
#                     NAME to /var/run/reboot-required.pkgs, as Ubuntu's
#                     notify-reboot-required does
#
# NAME and VERSION are used as given; callers pass fixed fixture values.
set -euo pipefail

die() {
	echo "aptrepo/build.sh: $*" >&2
	exit 1
}

build_deb() {
	local out=$1 name=$2 version=$3
	shift 3
	local gate="" conffile="" depends="" conflicts="" failpostinst="" reboot=""
	while [ $# -gt 0 ]; do
		case $1 in
		--depends)
			[ $# -ge 2 ] || die "--depends needs a value"
			depends=$2
			shift 2
			;;
		--conflicts)
			[ $# -ge 2 ] || die "--conflicts needs a value"
			conflicts=$2
			shift 2
			;;
		--fail-postinst)
			failpostinst=1
			shift
			;;
		--reboot-marker)
			reboot=1
			shift
			;;
		--gate)
			[ $# -ge 2 ] || die "--gate needs a file"
			gate=$2
			shift 2
			;;
		--conffile)
			conffile=1
			shift
			;;
		*) die "unknown option $1" ;;
		esac
	done
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
	[ -z "$depends" ] || echo "Depends: $depends" >>"$work/DEBIAN/control"
	[ -z "$conflicts" ] || echo "Conflicts: $conflicts" >>"$work/DEBIAN/control"
	if [ -n "$conffile" ]; then
		install -d -m 0755 "$work/etc"
		printf '%s %s\n' "$name" "$version" >"$work/etc/$name.conf"
		chmod 0644 "$work/etc/$name.conf"
		echo "/etc/$name.conf" >"$work/DEBIAN/conffiles"
		chmod 0644 "$work/DEBIAN/conffiles"
	fi
	if [ -n "$gate$failpostinst$reboot" ]; then
		echo '#!/bin/sh' >"$work/DEBIAN/postinst"
		if [ -n "$gate" ]; then
			cat >>"$work/DEBIAN/postinst" <<EOF
# Holds dpkg mid-postinst until the test creates the gate file.
i=0
while [ ! -e '$gate' ] && [ \$i -lt 3000 ]; do
	sleep 0.2
	i=\$((i + 1))
done
EOF
		fi
		if [ -n "$reboot" ]; then
			cat >>"$work/DEBIAN/postinst" <<EOF
# Asks for a reboot as notify-reboot-required does; never reboots.
touch /var/run/reboot-required
echo '$name' >>/var/run/reboot-required.pkgs
EOF
		fi
		if [ -n "$failpostinst" ]; then
			echo 'echo "fixture postinst fails on purpose" >&2; exit 1' >>"$work/DEBIAN/postinst"
		else
			echo 'exit 0' >>"$work/DEBIAN/postinst"
		fi
		chmod 0755 "$work/DEBIAN/postinst"
	fi
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
	[ $# -ge 3 ] || die "usage: add REPO NAME VERSION [options]"
	[ -d "$1" ] || init "$1"
	build_deb "$@"
	index "$1"
	;;
deb) [ $# -ge 3 ] || die "usage: deb OUT NAME VERSION [options]"; build_deb "$@" ;;
index) [ $# -eq 1 ] || die "usage: index REPO"; index "$1" ;;
*) die "unknown command $cmd" ;;
esac
