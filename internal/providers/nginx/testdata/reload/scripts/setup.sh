#!/bin/sh
# usage: setup.sh <includeDir>
set -e
inc=$1
sed -i 's/^worker_processes .*/worker_processes 2;/' /etc/nginx/nginx.conf
mkdir -p /var/cache/nginx/nxcap /srv/nx
head -c 4194304 /dev/zero > /srv/nx/big
cat > $inc/00-capture.conf <<'C'
proxy_cache_path /var/cache/nginx/nxcap keys_zone=nxcap:1m;
server {
	listen 8080;
	root /srv/nx;
	location /big { limit_rate 1k; }
}
C
# A second, independent nginx that holds port 8181 (bind-rejected case).
mkdir -p /tmp/occ
cat > /tmp/occ/nginx.conf <<'C'
worker_processes 1;
pid /tmp/occ/nginx.pid;
error_log /tmp/occ/error.log;
events {}
http { access_log off; server { listen 8181; } }
C
grep -n '^worker_processes' /etc/nginx/nginx.conf
