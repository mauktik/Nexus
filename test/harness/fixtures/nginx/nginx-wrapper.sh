#!/bin/sh
# The T1 "fake nginx" (test-strategy.md): installed at /usr/sbin/nginx by
# test/integration/nginx_test.go, with the real binary copied to
# /usr/sbin/nginx.t1-real, and removed again in the test's cleanup. It
# changes only `nginx -t`, and only while /run/nexus-t1/nginx-t exists:
# that file holds a delay in seconds, or "hang" for a -t that never ends.
# Every other invocation, and -t without the file, execs the real binary
# with the same arguments.
real=/usr/sbin/nginx.t1-real
ctl=/run/nexus-t1/nginx-t
for a in "$@"; do
	if [ "$a" = -t ] && [ -f "$ctl" ]; then
		d=$(cat "$ctl")
		case $d in
		hang) exec sleep 2147483647 ;;
		*) sleep "$d" ;;
		esac
		break
	fi
done
exec "$real" "$@"
