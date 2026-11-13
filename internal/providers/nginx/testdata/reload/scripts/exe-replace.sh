#!/bin/sh
# usage: exe-replace.sh ubuntu|alpine
plat=$1
if [ "$plat" = ubuntu ]; then m=$(systemctl show -p MainPID --value nginx.service); else m=$(cat /run/nginx/nginx.pid); fi
echo "\$ readlink /proc/$m/exe   # master $m, before"
readlink /proc/$m/exe
if [ "$plat" = alpine ]; then
	echo "\$ apk upgrade nginx"; apk upgrade nginx 2>&1; echo "exit $?"
	echo "\$ readlink /proc/$m/exe"; readlink /proc/$m/exe
	echo "\$ apk fix nginx"; apk fix nginx 2>&1; echo "exit $?"
else
	echo "\$ cp -p /usr/sbin/nginx /usr/sbin/nginx.nx-new && mv /usr/sbin/nginx.nx-new /usr/sbin/nginx"
	cp -p /usr/sbin/nginx /usr/sbin/nginx.nx-new && mv /usr/sbin/nginx.nx-new /usr/sbin/nginx; echo "exit $?"
fi
echo "\$ readlink /proc/$m/exe   # master $m, after"
readlink /proc/$m/exe
printf '%s\n' "\$ head -c 64 /proc/$m/cmdline | tr '\\0' ' '"
head -c 64 /proc/$m/cmdline | tr '\0' ' '; echo
