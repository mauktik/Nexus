#!/bin/sh
# Regenerates the committed package fixtures (see README). Run from the
# repository root: internal/providers/nexusd/testdata/pkg/mkfixtures.sh
# GNU tar runs in the local debian:stable image; bsdtar is the host's
# (macOS /usr/bin/tar). The tar flags are the Makefile's TAR_FLAGS.
set -eu
out=internal/providers/nexusd/testdata/pkg
stage=build/pkg-fixtures
gnu_flags='--owner=0 --group=0 --numeric-owner --no-xattrs'
bsd_flags='--uid 0 --gid 0 --uname root --gname root --no-xattrs --no-mac-metadata'
rm -rf "$stage"
mkdir -p "$stage"
for arch in amd64 arm64; do
	t=nexusd-v9.9.2-linux-$arch
	d=$stage/$t
	# The make nexusd-tree recipe, with a stand-in binary and two docs pages.
	mkdir -p "$d/docs/cli"
	cp "$out/nexusd-standin.sh" "$d/nexusd"
	cp deploy/nexusd.service deploy/nexusd.default.yaml deploy/install.sh deploy/uninstall.sh README.md "$d/"
	cp docs/install.md "$d/docs/"
	cp docs/cli/nexus.md "$d/docs/cli/"
	chmod -R u=rwX,go=rX "$d"
	chmod 0755 "$d/nexusd" "$d/install.sh" "$d/uninstall.sh"
	(cd "$stage" && COPYFILE_DISABLE=1 /usr/bin/bsdtar $bsd_flags -czf "$OLDPWD/$out/bsd-$t.tar.gz" "$t")
	docker run --rm -v "$PWD/$stage:/stage:ro" -v "$PWD/$out:/out" -w /stage debian:stable \
		tar $gnu_flags -czf "/out/gnu-$t.tar.gz" "$t"
done
