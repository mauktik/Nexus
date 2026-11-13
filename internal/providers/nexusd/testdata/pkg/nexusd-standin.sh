#!/bin/sh
# Test stand-in for the nexusd binary in the package fixtures. It answers
# only "-version", so a successful probe shows the argv was exactly that.
if [ "$#" -ne 1 ] || [ "$1" != "-version" ]; then
	echo "usage: nexusd -version" >&2
	exit 2
fi
echo "nexusd v9.9.2"
