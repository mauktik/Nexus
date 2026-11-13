#!/bin/sh
# usage: run-case.sh ubuntu|alpine valid|long-conn|invalid|bind-rejected|master-killed
plat=$1 case=$2
if [ "$plat" = ubuntu ]; then
	inc=/etc/nginx/conf.d
	stop() { systemctl stop nginx; }
	start() { systemctl start nginx; }
	master() { systemctl show -p MainPID --value nginx.service; }
	set -- /usr/bin/systemctl reload -- nginx.service
else
	inc=/etc/nginx/http.d
	stop() { rc-service nginx stop; }
	start() { rc-service nginx start; }
	master() { cat /run/nginx/nginx.pid; }
	set -- /sbin/rc-service -C nginx reload
fi
mkdir -p /out
rm -f $inc/zz-*.conf
stop >/dev/null 2>&1
pkill -9 -x nginx; pkill -9 -f '^nginx: '; pkill -x curl; sleep 0.5
[ "$plat" = alpine ] && rc-service nginx zap >/dev/null 2>&1
start >/dev/null 2>&1 || { echo "start failed"; exit 1; }
sleep 1
kill=
case $case in
valid) ;;
long-conn)
	curl -s -o /dev/null http://127.0.0.1:8080/big &
	sleep 1 ;;
invalid) echo 'bogus_directive;' > $inc/zz-bad.conf ;;
bind-rejected)
	nginx -c /tmp/occ/nginx.conf
	echo 'server { listen 8181; }' > $inc/zz-bind.conf ;;
master-killed) kill=-kill-master ;;
esac
nginx -t >/out/$plat-$case.nginx-t.txt 2>&1; echo "nginx -t exit $?" >>/out/$plat-$case.nginx-t.txt
/capture -comm nginx -name $plat-$case -out /out/$plat-$case.jsonl -master "$(master)" $kill -- "$@"
ps -o pid,ppid,stat,args -e 2>/dev/null | grep '[n]ginx' || ps -o pid,ppid,stat,args | grep '[n]ginx'
pkill -x curl
[ -f /tmp/occ/nginx.pid ] && kill "$(cat /tmp/occ/nginx.pid)"
rm -f $inc/zz-*.conf
tail -n 3 /var/log/nginx/error.log > /out/$plat-$case.error-log.txt 2>/dev/null
exit 0
