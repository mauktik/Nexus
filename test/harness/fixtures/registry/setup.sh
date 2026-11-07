#!/bin/bash
# Registry fixtures of WI 6.3, run inside the T1 harness as root by
# test/integration/docker_updatecheck_test.go:
#
#   start               start two real registry:2 containers on the nested
#                       Engine: nx-registry, open, on 127.0.0.1:5000, and
#                       nx-registry-auth, htpasswd basic auth, on
#                       127.0.0.1:5001. Their data survives a restart and a
#                       change of image store. Installs the credential
#                       helper fixture. Idempotent.
#   stop-open           stop nx-registry (the registry-down failure mode)
#   image NAME CONTENT  build NAME (repo:tag) without network: a busybox
#                       container writes CONTENT to /nx-content and is
#                       committed with CMD sleep. Each CONTENT gives a new
#                       image and digest.
#   push NAME           push NAME; a 127.0.0.1:5001 name is pushed with the
#                       fixture login from a private DOCKER_CONFIG, so
#                       /root/.docker is never written.
#   helper-marker       print the marker path the helper writes if executed
#   remove              remove both registries and their data
#
# Images: registry:2 and busybox:latest come from a seeded tarball under
# $SEED once one exists. The first run pulls them and seeds the tarball, so
# a later image-store change (TC-113) loads them with no network.
set -euo pipefail

OPEN_PORT=5000
AUTH_PORT=5001
DATA=/var/lib/nx-registry
SEED=/var/cache/nx-registry-seed
PUSH_CONFIG=$DATA/pusher
# The fixture account of the authenticating registry. Tests use the same
# literal; it guards nothing outside the harness.
AUTH_USER=nxuser
AUTH_PASS=nx-fixture-pass-7Qk
# bcrypt of AUTH_PASS (htpasswd -nbB); registry:2 accepts only bcrypt.
HTPASSWD='nxuser:$2y$05$ECCQh5KGZW.WPK8kTqn4NOZ/NnLYCLM8PPQyoqmwKnZlKpEAARVK2'
# On the fixed lookup path: credential helpers are found on PATH as
# docker-credential-<name>.
HELPER=/usr/local/bin/docker-credential-nx
# Under nexusd's StateDirectory, so the sandboxed daemon could write it.
MARKER=/var/lib/nexus/docker-credential-nx.marker

die() {
	echo "setup.sh: $*" >&2
	exit 1
}

arch() {
	case $(uname -m) in
	aarch64 | arm64) echo arm64 ;;
	x86_64 | amd64) echo amd64 ;;
	*) uname -m ;;
	esac
}

# ensure_image IMAGE: present, else loaded from the seed, else pulled; the
# seed is written after the first pull.
ensure_image() {
	local image=$1 tar
	tar=$SEED/$(echo "$image" | tr ':/' '__').tar
	if ! docker image inspect "$image" >/dev/null 2>&1 && [ -s "$tar" ]; then
		docker load -i "$tar" >/dev/null 2>&1 || echo "setup.sh: loading $tar failed; pulling" >&2
	fi
	if ! docker image inspect "$image" >/dev/null 2>&1; then
		docker pull -q "$image" >/dev/null
	fi
	if [ ! -s "$tar" ]; then
		mkdir -p "$SEED"
		docker save --platform "linux/$(arch)" -o "$tar.tmp" "$image" 2>/dev/null ||
			docker save -o "$tar.tmp" "$image" 2>/dev/null || rm -f "$tar.tmp"
		[ ! -f "$tar.tmp" ] || mv "$tar.tmp" "$tar"
	fi
}

wait_port() {
	local port=$1 i
	for i in $(seq 1 100); do
		# 200 open, 401 authenticating: both mean the registry answers.
		if bash -c "exec 3<>/dev/tcp/127.0.0.1/$port" 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	die "registry on port $port did not open"
}

run_registry() {
	local name=$1 port=$2
	shift 2
	if [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null || true)" = true ]; then
		return 0
	fi
	docker rm -f "$name" >/dev/null 2>&1 || true
	mkdir -p "$DATA/$name"
	docker run -d --name "$name" --restart no -p "127.0.0.1:$port:5000" \
		-v "$DATA/$name:/var/lib/registry" "$@" registry:2 >/dev/null
	wait_port "$port"
}

start() {
	ensure_image registry:2
	ensure_image busybox:latest
	mkdir -p "$DATA/auth" "$PUSH_CONFIG"
	printf '%s\n' "$HTPASSWD" >"$DATA/auth/htpasswd"
	run_registry nx-registry $OPEN_PORT
	run_registry nx-registry-auth $AUTH_PORT -v "$DATA/auth:/auth:ro" \
		-e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=nx \
		-e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd
	cat >"$HELPER" <<EOF
#!/bin/sh
# Credential helper fixture (TC-114, TC-ERR34): Nexus must never run it.
date -u +%FT%TZ >>$MARKER 2>/dev/null
echo "\$@" >>$MARKER 2>/dev/null
echo '{"ServerURL":"127.0.0.1:$AUTH_PORT","Username":"$AUTH_USER","Secret":"$AUTH_PASS"}'
EOF
	chmod 0755 "$HELPER"
	printf '%s' "$AUTH_PASS" | DOCKER_CONFIG=$PUSH_CONFIG docker login -u "$AUTH_USER" --password-stdin \
		"127.0.0.1:$AUTH_PORT" >/dev/null 2>&1 || die "fixture login to 127.0.0.1:$AUTH_PORT failed"
}

image() {
	local name=$1 content=$2 tmp
	ensure_image busybox:latest
	tmp=nx-build-$$-$RANDOM
	docker run --name "$tmp" busybox:latest sh -c "printf '%s\n' \"\$1\" >/nx-content" sh "$content" >/dev/null
	docker commit --change 'CMD ["sleep","3600"]' "$tmp" "$name" >/dev/null
	docker rm -f "$tmp" >/dev/null
}

push() {
	local name=$1
	case $name in
	127.0.0.1:$AUTH_PORT/*) DOCKER_CONFIG=$PUSH_CONFIG docker push -q "$name" >/dev/null ;;
	*) docker push -q "$name" >/dev/null ;;
	esac
}

cmd=${1:-}
[ -n "$cmd" ] || die "no command"
shift
case $cmd in
start) start ;;
stop-open) docker stop nx-registry >/dev/null ;;
image) [ $# -eq 2 ] || die "usage: image NAME CONTENT"; image "$1" "$2" ;;
push) [ $# -eq 1 ] || die "usage: push NAME"; push "$1" ;;
helper-marker) echo "$MARKER" ;;
remove)
	docker rm -f -v nx-registry nx-registry-auth >/dev/null 2>&1 || true
	rm -rf "$DATA"
	;;
*) die "unknown command $cmd" ;;
esac
