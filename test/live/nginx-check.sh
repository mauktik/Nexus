#!/bin/sh
# Live check of the nginx provider (nginx-provider WI 7.2): TC-118 on the
# Alpine/OpenRC nginx VM and TC-119 on a real Ubuntu/systemd host, each
# running the release tarball. It drives the real `nexus` CLI against the
# host and reads every result back without Nexus: the init system
# (`rc-service nginx status` / `systemctl is-active`), /proc, `ls -ln`,
# `sha256sum`, and curl (or BusyBox wget) of a disposable test site on a
# spare loopback port. It is POSIX sh, and BusyBox ash runs it.
#
# Run it on the host itself, as root:
#   sh nginx-check.sh                               the facts and the plan;
#                                                   changes nothing
#   NEXUS_TOKEN=... sh nginx-check.sh --yes         the check
# or from another machine (a Mac) with the nexus CLI, over SSH:
#   NEXUS_TOKEN=... NEXUS_URL=https://vm.lan:7443 sh nginx-check.sh --ssh root@vm.lan --yes
#   sh nginx-check.sh --ssh root@vm.lan --nexus-host vm --yes   (a host
#                                                   already in your nexus config)
#   ... --yes --cleanup                             undo what an interrupted
#                                                   run left behind
#
# The procedure and the transcript template are in
# docs/verification/nginx-live.md. Every command and its output go to the
# transcript file (--transcript FILE). NEXUS_TOKEN is never written to it.
#
# What it touches on the host: ONE disposable site, nexus-check-<random>.conf
# in the include directory, listening on 127.0.0.1 at a spare port it checks
# is free first; a holder process on a second spare loopback port for the
# bind-conflict cases; and its state in /var/lib/nexus-nginx-check. It
# reloads nginx only for that site. It never edits nginx.conf or another
# site, and it records a sha256 of every other file under /etc/nginx and the
# include directory before and after, and fails loudly if any differs. Stop,
# start and restart of nginx interrupt production traffic, so they run only
# with --with-lifecycle.

# Functions run through the EXIT trap, which shellcheck cannot follow.
# shellcheck disable=SC2317
set -u
umask 022

PROG=nginx-check.sh
H_STATE=/var/lib/nexus-nginx-check
NGINX_CONF=/etc/nginx/nginx.conf
NGINX_BIN=/usr/sbin/nginx
# The svcctltest fake's reload exit codes (internal/svcctl/testdata/README.md,
# the Exit column, which svcctltest reads): systemd/reload-invalid.txt 1,
# systemd/reload-bind-conflict.txt 0, openrc/reload-invalid.txt 1,
# openrc/reload-bind-conflict.txt 0. A different real value is a defect in
# the fake (TC-11), to fix before completion.
FAKE_RELOAD_INVALID=1
FAKE_RELOAD_BIND=0

usage() {
	cat <<'EOF'
Usage: sh nginx-check.sh [--yes] [--cleanup] [OPTIONS]

Without --yes it checks the preconditions, records the facts (the include
line, nginx -t, the spare ports, the sha256 of every file under /etc/nginx)
and prints the plan. It changes nothing and exits 2.

Options:
  --yes              run the check (adds, changes and finally removes the
                     disposable site; reloads nginx for it)
  --with-lifecycle   also stop, start and restart nginx through Nexus. This
                     interrupts every site nginx serves; off by default
  --cleanup          with --yes: remove what an interrupted run left (the
                     disposable site, the holder process, the state), test
                     the configuration, reload if the site was ever enabled,
                     and start nginx if the run had stopped it
  --ssh TARGET       run the host commands over SSH (user@host), and the nexus
                     CLI here. TARGET must be root, or have sudo without a
                     password. Extra ssh options: NEXUS_CHECK_SSH_OPTS
  --nexus-host NAME  use the host NAME already configured in your nexus client
                     (no NEXUS_TOKEN needed); otherwise the script registers
                     the host in a throwaway client config with NEXUS_TOKEN
  --nexus PATH       the nexus CLI (default: nexus on PATH)
  --port N           the test site's port (default: a random free one in
                     18100-18999)
  --conflict-port N  the port the holder process takes (default: random)
  --transcript FILE  where to write the transcript (default:
                     ./nginx-check-<host>-<UTC time>.txt)
  -h, --help         this text

Environment:
  NEXUS_TOKEN  a token with nginx:read, nginx:write and nginx:config (not
               needed with --nexus-host). Create it on the host, and revoke it
               afterwards:
                 nexusd token create --name nginx-check --scope nginx:read --scope nginx:write --scope nginx:config --expires 1d
                 nexusd token revoke nginx-check
  NEXUS_URL    nexusd's URL, such as https://vm.lan:7443 (default on the
               host: the first listen address in /etc/nexus/nexusd.yaml;
               required with --ssh unless --nexus-host is given). The
               certificate is pinned to `nexusd tls fingerprint`, read on the
               host.
  NEXUS_CHECK_SSH_OPTS  extra ssh options, word-split, such as "-p 2222"

Exit status: 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or --yes is missing); 3 nothing failed but a check was
not run (NOT-RUN, such as the lifecycle steps without --with-lifecycle).
EOF
}

YES=0
MODE=main
LIFECYCLE=0
SSH_TARGET=
NXHOST_GIVEN=
NEXUS_CLI=nexus
PORT=
CPORT=
TRANSCRIPT=
ARGS="$*"
while [ $# -gt 0 ]; do
	case $1 in
	--yes) YES=1 ;;
	--with-lifecycle) LIFECYCLE=1 ;;
	--cleanup) MODE=cleanup ;;
	--ssh | --nexus-host | --nexus | --port | --conflict-port | --transcript)
		{ [ $# -ge 2 ] && [ -n "$2" ]; } || { echo "$PROG: $1 needs a value" >&2; exit 2; }
		case $1 in
		--ssh) SSH_TARGET=$2 ;;
		--nexus-host) NXHOST_GIVEN=$2 ;;
		--nexus) NEXUS_CLI=$2 ;;
		--port) PORT=$2 ;;
		--conflict-port) CPORT=$2 ;;
		--transcript) TRANSCRIPT=$2 ;;
		esac
		shift
		;;
	-h | --help) usage; exit 0 ;;
	*) echo "$PROG: unknown argument: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done
for _p in "$PORT" "$CPORT"; do
	case $_p in
	'') ;;
	*[!0-9]*) echo "$PROG: a port is a number: $_p" >&2; exit 2 ;;
	*) { [ "$_p" -ge 1024 ] && [ "$_p" -le 65535 ]; } || { echo "$PROG: port $_p is not in 1024-65535" >&2; exit 2; } ;;
	esac
done

if [ -z "$TRANSCRIPT" ]; then
	_h=$(hostname)
	[ -z "$SSH_TARGET" ] || _h=${SSH_TARGET#*@}
	TRANSCRIPT=./nginx-check-$_h-$(date -u +%Y%m%dT%H%M%SZ).txt
fi
if ! : >>"$TRANSCRIPT"; then
	echo "$PROG: cannot write the transcript $TRANSCRIPT" >&2
	exit 2
fi
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/nginx-check.XXXXXX") || { echo "$PROG: mktemp failed" >&2; exit 2; }

FAILS=0
INCONCL=0
RESULTS=
CHANGED=0 # the host holds something of this run, so an exit must undo it
CLEANED=0
HSUDO=0
NXHOST=
INIT=
SVC=nginx
INCDIR=
SUFFIX=.conf
DSUFFIX=.disabled
PIDFILE=
SITE=
RAND=
ENABLED_EVER=0
STOPPED_BY_US=0
HOLDER_PID=
INFLIGHT=
FETCH=
ERRLOG=/var/log/nginx/error.log
MASTER0=

# --- transcript ----------------------------------------------------------

log() { printf '%s\n' "$*" | tee -a "$TRANSCRIPT"; }
note() { log "[$(date -u +%H:%M:%S)] $*"; }

# shown ARG...: the arguments as a shell would need them typed.
shown() {
	_sh=
	for _a in "$@"; do
		case $_a in
		'' | *[!A-Za-z0-9_./:=@%+,-]*) _a="'$(printf '%s' "$_a" | sed "s/'/'\\\\''/g")'" ;;
		esac
		_sh="$_sh${_sh:+ }$_a"
	done
	printf '%s' "$_sh"
}

# result ID STATUS DETAIL: STATUS is PASS, FAIL, NOT-RUN or INFO.
result() {
	RESULTS="$RESULTS
$1|$2|$3"
	log "RESULT $1: $2 - $3"
	case $2 in
	FAIL) FAILS=$((FAILS + 1)) ;;
	NOT-RUN) INCONCL=$((INCONCL + 1)) ;;
	esac
}

refuse() {
	log "REFUSED: $*"
	exit 2
}

sha_stdin() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum | cut -d' ' -f1
	else
		shasum -a 256 | cut -d' ' -f1
	fi
}
sha_text() { printf '%s' "$1" | sha_stdin; } # the SHA-256 of the text $1, exactly as given

rand_hex8() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }
rand_port() { echo $((18100 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' \n') % 900)); }

# --- the host: local, or over SSH ----------------------------------------

ssh_() {
	# shellcheck disable=SC2086 # NEXUS_CHECK_SSH_OPTS is word-split on purpose.
	ssh -o BatchMode=yes ${NEXUS_CHECK_SSH_OPTS:-} "$SSH_TARGET" "$@"
}

# hx CMD: run the shell command CMD as root on the host, unlogged. HX_OUT
# holds its output (stdout and stderr), HX_RC its status. hx_in does the
# same with this script's stdin as the command's: redirect it from a file,
# never pipe into it, or HX_OUT and HX_RC are set in a subshell and lost.
hx() { hx_in "$1" </dev/null; }
hx_in() {
	if [ -z "$SSH_TARGET" ]; then
		HX_OUT=$(sh -c "$1" 2>&1)
	elif [ "$HSUDO" = 1 ]; then
		HX_OUT=$(ssh_ "sudo -n sh -c $(shown "$1")" 2>&1)
	else
		HX_OUT=$(ssh_ "sh -c $(shown "$1")" 2>&1)
	fi
	HX_RC=$?
}

# hrun CMD: hx, logged as "$ [host] CMD" with the output and a non-zero exit.
hrun() {
	log "\$ [host] $1"
	hx "$1"
	[ -z "$HX_OUT" ] || printf '%s\n' "$HX_OUT" | tee -a "$TRANSCRIPT"
	[ "$HX_RC" -eq 0 ] || log "(exit $HX_RC)"
	return "$HX_RC"
}

# host_root: the host commands run as root (directly, or with sudo -n).
host_root() {
	if [ -z "$SSH_TARGET" ]; then
		[ "$(id -u)" = 0 ] || refuse "run this as root on the host (Alpine has no sudo by default: su -), or from another machine with --ssh"
		return
	fi
	hx 'id -u'
	if [ "$HX_RC" -ne 0 ]; then
		log "$HX_OUT"
		refuse "ssh $SSH_TARGET failed (exit $HX_RC); it must log in without a password prompt (BatchMode)"
	fi
	if [ "$HX_OUT" != 0 ]; then
		HSUDO=1
		hx 'id -u'
		{ [ "$HX_RC" -eq 0 ] && [ "$HX_OUT" = 0 ]; } ||
			refuse "$SSH_TARGET is not root and sudo -n does not work there: use root@host, or allow sudo without a password"
	fi
}

host_in_container() {
	hx '[ -e /.dockerenv ] || [ -e /run/.containerenv ] || grep -qa container= /proc/1/environ'
	[ "$HX_RC" -eq 0 ]
}

# --- host state (/var/lib/nexus-nginx-check/state) -----------------------

save_state() {
	printf 'site=%s\nport=%s\ncport=%s\nincdir=%s\ninit=%s\nsvc=%s\nenabled_ever=%s\nstopped_by_us=%s\nholder_pid=%s\ninflight=%s\nnxhost=%s\n' \
		"$SITE" "$PORT" "$CPORT" "$INCDIR" "$INIT" "$SVC" "$ENABLED_EVER" "$STOPPED_BY_US" "$HOLDER_PID" "$INFLIGHT" "$NXHOST" >"$TMPD/state"
	hx_in "umask 077 && mkdir -p $H_STATE && cat >$H_STATE/state.new && mv -f $H_STATE/state.new $H_STATE/state" <"$TMPD/state"
	[ "$HX_RC" -eq 0 ] || log "WARNING: could not write $H_STATE/state: $HX_OUT"
}

state_value() { printf '%s\n' "$STATE_TEXT" | sed -n "s/^$1=//p" | head -n 1; }

load_state() {
	hx "cat $H_STATE/state"
	[ "$HX_RC" -eq 0 ] || return 1
	STATE_TEXT=$HX_OUT
	SITE=$(state_value site) PORT=$(state_value port) CPORT=$(state_value cport)
	INCDIR=$(state_value incdir) INIT=$(state_value init) SVC=$(state_value svc)
	ENABLED_EVER=$(state_value enabled_ever) STOPPED_BY_US=$(state_value stopped_by_us)
	HOLDER_PID=$(state_value holder_pid) INFLIGHT=$(state_value inflight)
	[ -n "$NXHOST_GIVEN" ] || NXHOST_GIVEN=$(state_value nxhost)
	case $SITE in nexus-check-[0-9a-f]*) ;; *) return 1 ;; esac
}

# --- nginx on the host ---------------------------------------------------

detect_init() {
	hx '[ -d /run/systemd/system ] && echo systemd || { [ -e /run/openrc ] && [ -x /sbin/rc-service ] && echo openrc; }'
	INIT=$HX_OUT
	case $INIT in
	systemd | openrc) ;;
	*) refuse "the host runs neither systemd nor OpenRC" ;;
	esac
}

# init_reload: the init system's reload of nginx, with the argv svcctl uses.
init_cmd() {
	if [ "$INIT" = openrc ]; then
		echo "env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C /sbin/rc-service -C $SVC $1"
	else
		echo "env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C /usr/bin/systemctl $1 -- $SVC.service"
	fi
}

# init_state: INIT_STATE is started or stopped (OpenRC: rc-service status
# 0 or 3; systemd: is-active active or inactive), as the init system says.
init_state() {
	if [ "$INIT" = openrc ]; then
		hrun "$(init_cmd status)"
		case $HX_RC in 0) INIT_STATE=started ;; 3) INIT_STATE=stopped ;; *) INIT_STATE="unknown(exit $HX_RC)" ;; esac
	else
		hrun "/usr/bin/systemctl is-active -- $SVC.service"
		case $HX_OUT in active) INIT_STATE=started ;; inactive) INIT_STATE=stopped ;; *) INIT_STATE="unknown($HX_OUT)" ;; esac
	fi
}

# procs: MASTER and WORKERS (sorted, space-separated) from the pid file and
# /proc: the master's children titled exactly "nginx: worker process".
procs() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hx 'm=$(cat '"$PIDFILE"' 2>/dev/null); c=$(tr "\0" " " </proc/$m/cmdline 2>/dev/null); case $c in "nginx: master process"*) ;; *) m= ;; esac; w=; if [ -n "$m" ]; then for d in /proc/[0-9]*; do s=$(cat $d/stat 2>/dev/null) || continue; s=${s##*") "}; set -- $s; [ "$2" = "$m" ] || continue; case $(tr "\0" " " <$d/cmdline 2>/dev/null | sed "s/ *\$//") in "nginx: worker process") w="$w ${d#/proc/}" ;; esac; done; fi; echo "${m:-none}|$(echo $w | tr " " "\n" | sort -n | tr "\n" " ")"'
	MASTER=${HX_OUT%%|*} WORKERS=${HX_OUT#*|}
	WORKERS=${WORKERS% }
}

show_procs() {
	procs
	log "  (/proc) nginx master ${MASTER}, workers ${WORKERS:-none}"
}

# wait_reload OLD SECONDS: until the reload is applied as the provider's
# "confirmed" verdict defines it: no worker of OLD is still an active
# "nginx: worker process" (each exited or is shutting down), and there is a
# new one. 1 when SECONDS pass first.
wait_reload() {
	_wr=$(($(date +%s) + $2))
	while :; do
		procs
		_left=
		for _o in $1; do
			case " $WORKERS " in *" $_o "*) _left="$_left $_o" ;; esac
		done
		[ -n "$_left" ] || [ -z "$WORKERS" ] || return 0
		[ "$(date +%s)" -lt "$_wr" ] || return 1
		sleep 0.2
	done
}

fetch_tool() {
	hx 'command -v curl >/dev/null 2>&1 && echo curl || { command -v wget >/dev/null 2>&1 && echo wget; }'
	FETCH=$HX_OUT
	[ -n "$FETCH" ] || refuse "neither curl nor wget is on the host"
}

# fetch: FETCH_BODY and FETCH_STATE (served, refused or error) of
# http://127.0.0.1:PORT/ on the host, by curl or BusyBox wget.
fetch() {
	if [ "$FETCH" = curl ]; then
		hrun "curl -sS -m 5 http://127.0.0.1:$PORT/"
	else
		hrun "wget -q -T 5 -O - http://127.0.0.1:$PORT/"
	fi
	FETCH_BODY=$HX_OUT
	if [ "$HX_RC" -eq 0 ]; then
		FETCH_STATE=served
	elif [ "$FETCH" = curl ] && [ "$HX_RC" -eq 7 ]; then
		FETCH_STATE=refused
	else
		case $HX_OUT in *[Rr]efused*) FETCH_STATE=refused ;; *) FETCH_STATE="error(exit $HX_RC)" ;; esac
	fi
}

# site_files: SITE_ON and SITE_OFF are the sha256 of the enabled and the
# disabled file, or "absent"; logged with ls -ln and sha256sum.
site_files() {
	hrun "cd $(shown "$INCDIR") && ls -ln $SITE$SUFFIX $SITE$SUFFIX$DSUFFIX 2>&1; sha256sum $SITE$SUFFIX $SITE$SUFFIX$DSUFFIX 2>/dev/null; true"
	SITE_ON=absent SITE_OFF=absent SITE_MODE=
	_l=$(printf '%s\n' "$HX_OUT" | grep "  $SITE$SUFFIX\$" | cut -d' ' -f1)
	[ -z "$_l" ] || SITE_ON=$_l
	_l=$(printf '%s\n' "$HX_OUT" | grep "  $SITE$SUFFIX$DSUFFIX\$" | cut -d' ' -f1)
	[ -z "$_l" ] || SITE_OFF=$_l
	SITE_MODE=$(printf '%s\n' "$HX_OUT" | grep -E "^-.* $SITE$SUFFIX($DSUFFIX)?\$" | awk '{ print $1, $3, $4 }' | head -n 1)
}

nginx_t() { hrun "$NGINX_BIN -t"; }

# snap_cmd: one line per entry under /etc/nginx and the include directory
# (type, mode, uid, gid, size and sha256 for files, the target for symlinks),
# leaving out only this run's site and the provider's temporary files of it.
snap_cmd() {
	# shellcheck disable=SC2016 # Expanded on the host.
	printf '%s' 'S='"$SITE"'; for r in /etc/nginx '"$(shown "$INCDIR")"'; do [ -e "$r" ] && find "$r" -print; done | sort -u | while IFS= read -r f; do b=${f##*/}; case $b in "$S".* | ."$S".*) continue ;; esac; if [ -L "$f" ]; then echo "L $(stat -c "%a %u %g" "$f") $f -> $(readlink "$f")"; elif [ -f "$f" ]; then echo "F $(stat -c "%a %u %g %s" "$f") $(sha256sum <"$f" | cut -d" " -f1) $f"; elif [ -d "$f" ]; then echo "D $(stat -c "%a %u %g" "$f") $f"; else echo "O $(stat -c "%a %u %g" "$f") $f"; fi; done'
}

# include_lines FILE: "LINE|CONTEXT|ARGUMENT" for every include statement in
# FILE, CONTEXT being the enclosing blocks (main, http, ...), comments out.
include_lines() {
	awk '
	function ctx(  s, i) { s = "main"; for (i = 1; i <= depth; i++) s = s "/" stack[i]; return s }
	{
		l = $0; sub(/#.*/, "", l)
		n = length(l)
		for (i = 1; i <= n; i++) {
			c = substr(l, i, 1)
			if (c == "{") {
				s = stmt; gsub(/^[ \t]+/, "", s); split(s, w, /[ \t]+/)
				stack[++depth] = w[1]; stmt = ""
			} else if (c == "}") {
				if (depth > 0) depth--; stmt = ""
			} else if (c == ";") {
				s = stmt; gsub(/^[ \t]+/, "", s); split(s, w, /[ \t]+/)
				if (w[1] == "include") { a = w[2]; gsub(/["\047]/, "", a); print start "|" ctx() "|" a }
				stmt = ""
			} else {
				if (stmt ~ /^[ \t]*$/ && c !~ /[ \t]/) start = NR
				stmt = stmt c
			}
		}
		stmt = stmt " "
	}' "$1"
}

pid_path() {
	hx "awk '\$1 == \"pid\" { sub(/;.*/, \"\", \$2); print \$2; exit }' $NGINX_CONF"
	PIDFILE=$HX_OUT
	if [ -z "$PIDFILE" ]; then
		hx "$NGINX_BIN -V 2>&1 | tr ' ' '\n' | sed -n 's/^--pid-path=//p'"
		PIDFILE=${HX_OUT:-/run/nginx.pid}
	fi
}

# --- the nexus CLI -------------------------------------------------------

# nx ARG...: the nexus CLI against the host, logged. NX_OUT is stdout, NX_ERR
# stderr, NX_RC the status.
nx() {
	log "\$ $(shown nexus --host "$NXHOST" "$@")"
	NX_OUT=$("$NEXUS_CLI" --host "$NXHOST" "$@" 2>"$TMPD/nxerr")
	NX_RC=$?
	NX_ERR=$(cat "$TMPD/nxerr")
	[ -z "$NX_OUT" ] || printf '%s\n' "$NX_OUT" | tee -a "$TRANSCRIPT"
	[ -z "$NX_ERR" ] || printf '%s\n' "$NX_ERR" | sed 's/^/(stderr) /' | tee -a "$TRANSCRIPT"
	[ "$NX_RC" -eq 0 ] || log "(exit $NX_RC)"
	return "$NX_RC"
}

json_str() { printf '%s' "$2" | grep -o "\"$1\":\"[^\"]*\"" | head -n 1 | cut -d'"' -f4; }
json_num() { printf '%s' "$2" | grep -oE "\"$1\":-?[0-9]+" | head -n 1 | cut -d: -f2; }
json_bool() { printf '%s' "$2" | grep -oE "\"$1\":(true|false)" | head -n 1 | cut -d: -f2; }
# The n-th (1-based) "verdict": observed's first, then rollbackReload's.
json_nth_str() { printf '%s' "$3" | grep -o "\"$1\":\"[^\"]*\"" | sed -n "${2}p" | cut -d'"' -f4; }
op_problem() { printf '%s' "$1" | grep -o 'urn:nexus:problem:[a-z-]*' | head -n 1 | sed 's/.*://'; }

# setup_client: NXHOST, the host the CLI targets. With --nexus-host, the
# operator's own client config; otherwise a throwaway one in TMPD, with the
# certificate pinned to what `nexusd tls fingerprint` prints on the host and
# NEXUS_TOKEN passed on stdin (never in an argument or the transcript).
setup_client() {
	command -v "$NEXUS_CLI" >/dev/null 2>&1 || refuse "the nexus CLI ($NEXUS_CLI) is not found: install it from the release (nexus-<version>-<os>-<arch>.tar.gz), or pass --nexus PATH"
	if [ -n "$NXHOST_GIVEN" ]; then
		NXHOST=$NXHOST_GIVEN
		note "nexus CLI: the configured host $NXHOST (your client config)"
		return
	fi
	[ -n "${NEXUS_TOKEN:-}" ] || return 1
	_url=${NEXUS_URL:-}
	if [ -z "$_url" ]; then
		[ -z "$SSH_TARGET" ] || refuse "with --ssh, set NEXUS_URL (such as https://vm.lan:7443) or pass --nexus-host"
		hx "sed -n 's/^listen:[[:space:]]*\\[[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' /etc/nexus/nexusd.yaml | head -n 1"
		_addr=${HX_OUT:-127.0.0.1:7443}
		_port=${_addr##*:} _host=${_addr%:*}
		case $_host in 0.0.0.0 | '' | '[::]') _host=127.0.0.1 ;; esac
		if [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$_host:$_port/healthz" 2>/dev/null)" = 200 ]; then
			_url=http://$_host:$_port
		else
			_url=https://$_host:$_port
		fi
	fi
	export XDG_CONFIG_HOME="$TMPD/xdg"
	mkdir -p "$XDG_CONFIG_HOME" && chmod 0700 "$XDG_CONFIG_HOME"
	NXHOST=nginx-check
	case $_url in
	https://*)
		hrun "/usr/local/bin/nexusd tls fingerprint" || refuse "could not read nexusd's certificate fingerprint on the host"
		_fp=$HX_OUT
		log "\$ printf '%s\\n' \"\$NEXUS_TOKEN\" | $(shown nexus host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin)"
		printf '%s\n' "$NEXUS_TOKEN" | "$NEXUS_CLI" host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin >"$TMPD/hostadd" 2>&1
		;;
	*)
		log "\$ printf '%s\\n' \"\$NEXUS_TOKEN\" | $(shown nexus host add "$NXHOST" --url "$_url" --token-stdin)"
		printf '%s\n' "$NEXUS_TOKEN" | "$NEXUS_CLI" host add "$NXHOST" --url "$_url" --token-stdin >"$TMPD/hostadd" 2>&1
		;;
	esac
	_rc=$?
	sed 's#[^ ]*/xdg/nexus/config.yaml#<throwaway client config>#' "$TMPD/hostadd" | tee -a "$TRANSCRIPT"
	[ "$_rc" -eq 0 ] || refuse "nexus host add failed (exit $_rc)"
}

# op ARG...: submit an nginx operation with --no-wait, record it in the host
# state while it runs, and wait for its end. OP, OPJ (the operation's JSON
# line), OPS (its status) and OPP (its problem). 1 when no operation was
# created (refused before it existed).
op() {
	OP='' OPJ='' OPS='' OPP=''
	nx "$@" --no-wait -o jsonl
	OP=$(json_str id "$(printf '%s\n' "$NX_OUT" | head -n 1)")
	if [ -z "$OP" ]; then
		OPP=$(op_problem "$NX_OUT $NX_ERR")
		return 1
	fi
	INFLIGHT=$OP
	save_state
	op_wait "$OP" 300
	INFLIGHT=
	save_state
}

# op_wait ID SECONDS: poll the operation until it ends; log it once at the end.
op_wait() {
	_owend=$(($(date +%s) + $2))
	while :; do
		OPJ=$("$NEXUS_CLI" --host "$NXHOST" operation get "$1" -o jsonl 2>/dev/null | head -n 1)
		OPS=$(json_str status "$OPJ")
		case $OPS in Succeeded | Failed | Canceled) break ;; esac
		if [ "$(date +%s)" -ge "$_owend" ]; then
			note "operation $1 has not ended after $2 s (status ${OPS:-?})"
			break
		fi
		sleep 1
	done
	log "\$ $(shown nexus --host "$NXHOST" operation get "$1" -o jsonl)"
	log "$OPJ"
	OPP=$(op_problem "$OPJ")
	note "operation $1: $OPS${OPP:+ $OPP}"
}

# --- the disposable site's contents ---------------------------------------

# content MARK [EXTRA_LISTEN]: a server on 127.0.0.1:PORT that answers
# "nexus-check RAND MARK".
content() {
	printf 'server {\n    listen 127.0.0.1:%s;\n%s    server_name %s.invalid;\n    default_type text/plain;\n    location / { return 200 "nexus-check %s %s\\n"; }\n}\n' \
		"$PORT" "${2:+    listen 127.0.0.1:$2;
}" "$SITE" "$RAND" "$1"
}
marker() { echo "nexus-check $RAND $1"; }
BAD_CONTENT='nexus_check_unknown_directive on;
'

# write_site TEXT: write TEXT as the enabled file directly on the host
# (root:root 0644, by rename), for the init-system reloads of step 5.
write_site() {
	printf '%s' "$1" >"$TMPD/site"
	hx_in "cd $(shown "$INCDIR") && cat >.$SITE.check-tmp && chown root:root .$SITE.check-tmp && chmod 0644 .$SITE.check-tmp && mv -f .$SITE.check-tmp $SITE$SUFFIX && sha256sum $SITE$SUFFIX" <"$TMPD/site"
	log "\$ [host] (write $(printf '%s' "$1" | wc -c | tr -d ' ') bytes, sha256 $(sha_text "$1"), to $INCDIR/$SITE$SUFFIX by a rename)"
	log "$HX_OUT"
	return "$HX_RC"
}

# --- the holder of the conflict port --------------------------------------

# holder_start: a process listening on 127.0.0.1:CPORT, which ends by itself
# after 600 s. perl (Debian's perl-base), python3, or BusyBox nc.
holder_start() {
	# shellcheck disable=SC2016 # Perl and Python code, expanded by neither shell.
	hx 'if command -v perl >/dev/null 2>&1 && perl -MSocket -e1 2>/dev/null; then echo perl; elif command -v python3 >/dev/null 2>&1; then echo python3; elif busybox nc --help 2>&1 | grep -q -- "-l.*Listen"; then echo busybox-nc; else echo none; fi'
	case $HX_OUT in
	perl)
		# shellcheck disable=SC2016
		_hc='perl -MSocket -e '"'"'socket(S,PF_INET,SOCK_STREAM,0)||die;bind(S,sockaddr_in($ARGV[0],inet_aton("127.0.0.1")))||die "bind: $!\n";listen(S,5)||die;sleep 600'"'"' '"$CPORT"
		;;
	python3)
		_hc='python3 -c '"'"'import socket,sys,time;s=socket.socket();s.bind(("127.0.0.1",int(sys.argv[1])));s.listen(5);time.sleep(600)'"'"' '"$CPORT"
		;;
	busybox-nc) _hc="timeout 600 busybox nc -l -s 127.0.0.1 -p $CPORT" ;;
	*)
		log "no perl, python3 or BusyBox nc on the host to hold port $CPORT"
		return 1
		;;
	esac
	hrun "nohup $_hc </dev/null >/dev/null 2>&1 & echo \$!" || return 1
	HOLDER_PID=$HX_OUT
	save_state
	_i=0
	while [ "$_i" -lt 25 ]; do
		port_listening "$CPORT" && break
		sleep 0.2
		_i=$((_i + 1))
	done
	hrun "tr '\\0' ' ' </proc/$HOLDER_PID/cmdline; echo; (ss -Htln 2>/dev/null || netstat -tln 2>/dev/null) | grep ':$CPORT '"
	port_listening "$CPORT"
}

holder_stop() {
	[ -n "$CPORT" ] || return 0
	# Only a process this run started: the recorded PID naming the port or,
	# when an interrupt cut the recording short, a holder command line on it.
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'P='"$CPORT"' H='"${HOLDER_PID:-none}"'; n=0; for d in /proc/[0-9]*; do p=${d#/proc/}; c=$(tr "\0" " " <$d/cmdline 2>/dev/null) || continue; h=; case $c in "busybox nc -l -s 127.0.0.1 -p $P "* | "timeout 600 busybox nc -l -s 127.0.0.1 -p $P "* | "perl -MSocket -e "*" $P " | "python3 -c import socket"*" $P ") h=1 ;; *"$P"*) [ "$p" != "$H" ] || h=1 ;; esac; [ -z "$h" ] || { kill $p && echo "killed holder $p: $c" && n=$((n + 1)); }; done; echo "holders of port $P killed: $n"'
	HOLDER_PID=
	save_state
}

port_listening() {
	hx "(ss -Htln 2>/dev/null || netstat -tln 2>/dev/null) | awk '{ print \$4 }' | grep -q ':$1\$'"
	[ "$HX_RC" -eq 0 ]
}

# port_free P: nothing listens on P, and no file under /etc/nginx or the
# include directory has a listen directive naming it.
port_free() {
	port_listening "$1" && return 1
	hx "grep -rsEn 'listen[[:space:]]([^;]*[^0-9])?$1([^0-9]|\$)' /etc/nginx $(shown "$INCDIR")"
	[ "$HX_RC" -ne 0 ]
}

pick_ports() {
	if [ -n "$PORT" ]; then
		port_free "$PORT" || refuse "port $PORT is in use, or named by a listen directive (see: ss -tln; grep -rn $PORT /etc/nginx)"
	else
		_i=0
		while :; do
			PORT=$(rand_port)
			port_free "$PORT" && break
			_i=$((_i + 1))
			[ "$_i" -lt 30 ] || refuse "found no free port in 18100-18999; pass --port"
		done
	fi
	if [ -n "$CPORT" ]; then
		[ "$CPORT" != "$PORT" ] || refuse "--conflict-port equals the test port"
		port_free "$CPORT" || refuse "port $CPORT is in use, or named by a listen directive"
	else
		_i=0
		while :; do
			CPORT=$(rand_port)
			[ "$CPORT" != "$PORT" ] && port_free "$CPORT" && break
			_i=$((_i + 1))
			[ "$_i" -lt 30 ] || refuse "found no second free port in 18100-18999; pass --conflict-port"
		done
	fi
	hrun "(ss -Htln 2>/dev/null || netstat -tln 2>/dev/null) | awk '{ print \$4 }' | grep -E ':($PORT|$CPORT)\$'; grep -rsEn 'listen[[:space:]]([^;]*[^0-9])?($PORT|$CPORT)([^0-9]|\$)' /etc/nginx $(shown "$INCDIR"); echo 'no listener and no listen directive on either port'"
}

# --- preconditions, facts and the plan ------------------------------------

header() {
	log "# $PROG transcript: nginx-provider WI 7.2 (TC-118 Alpine/OpenRC, TC-119 Ubuntu/systemd)"
	log "# started $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(hostname), mode $MODE, host ${SSH_TARGET:-this machine}, arguments: ${ARGS:-(none)}"
	log "# script sha256 $(sha_stdin <"$0")"
	host_in_container && IN_CONTAINER=1 || IN_CONTAINER=0
	if [ "$IN_CONTAINER" = 1 ]; then
		log "# CONTAINER DETECTED on the host (/.dockerenv, /run/.containerenv or container= in PID 1's environment):"
		log "# this transcript is a dry run, NOT real-host evidence."
	fi
}

facts() {
	note "== Step 1: facts, the include line (ASM-12, ASM-3) and nginx -t"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'uname -a; [ ! -r /etc/alpine-release ] || echo "Alpine $(cat /etc/alpine-release)"; [ ! -r /etc/debian_version ] || echo "Debian base $(cat /etc/debian_version)"; tr "\0" " " </proc/1/cmdline; echo'
	if [ "$INIT" = openrc ]; then hrun 'openrc --version'; else hrun 'systemctl --version | head -n 1'; fi
	hrun "$NGINX_BIN -v"
	hrun '/usr/local/bin/nexusd -version'
	log "\$ $(shown "$NEXUS_CLI" --version)"
	"$NEXUS_CLI" --version 2>&1 | tee -a "$TRANSCRIPT"
	fetch_tool
	log "  readback of the test port with: $FETCH"

	PROVIDER_OK=0
	if [ -n "$NXHOST" ]; then
		nx nginx get -o jsonl
		_st=$(json_str state "$NX_OUT")
		INCDIR=$(json_str includeDir "$NX_OUT")
		SUFFIX=$(json_str includeSuffix "$NX_OUT")
		DSUFFIX=$(json_str disabledSuffix "$NX_OUT")
		SVC=$(json_str service "$NX_OUT")
		_pi=$(json_str initSystem "$NX_OUT")
		{ [ "$NX_RC" -eq 0 ] && [ -n "$INCDIR" ]; } || refuse "nexus nginx get failed: is the nginx provider enabled in /etc/nexus/nexusd.yaml, and does the token have nginx:read?"
		if [ "$_pi" = "$INIT" ] && [ "$_st" = running ]; then
			result 1-provider PASS "nexus nginx get: running, initSystem $_pi, includeDir $INCDIR, service $SVC"
			PROVIDER_OK=1
		else
			result 1-provider FAIL "nexus nginx get: state $_st, initSystem $_pi (the host runs $INIT)"
		fi
	else
		hx '[ -r /etc/alpine-release ] && echo /etc/nginx/http.d || echo /etc/nginx/conf.d'
		INCDIR=$HX_OUT
		log "(no NEXUS_TOKEN and no --nexus-host: the provider was not asked; assuming the distribution's include directory $INCDIR)"
	fi
	pid_path
	log "  nginx pid file: $PIDFILE"

	hrun "grep -n include $NGINX_CONF"
	hx "cat $NGINX_CONF"
	printf '%s\n' "$HX_OUT" >"$TMPD/nginx.conf"
	include_lines "$TMPD/nginx.conf" >"$TMPD/includes"
	log "  include statements (line|context|argument):"
	sed 's/^/    /' "$TMPD/includes" | tee -a "$TRANSCRIPT"
	_want="$INCDIR/*$SUFFIX"
	_hit=
	while IFS='|' read -r _ln _ctx _arg; do
		case $_arg in /*) _abs=$_arg ;; *) _abs=/etc/nginx/$_arg ;; esac
		if [ "$_abs" = "$_want" ] && [ "$_ctx" = main/http ]; then
			_hit="line $_ln: include $_arg; (in http)"
			break
		fi
	done <"$TMPD/includes"
	if [ -n "$_hit" ]; then
		if [ "$INIT" = openrc ]; then
			case $_want in */http.d/\*.conf) result 1-include PASS "$NGINX_CONF $_hit names http.d/*.conf" ;;
			*) result 1-include FAIL "$NGINX_CONF $_hit, but the Alpine include directory is not http.d/*.conf" ;; esac
		else
			result 1-include PASS "$NGINX_CONF $_hit names $_want"
		fi
	else
		result 1-include FAIL "no include of $_want inside the http block of $NGINX_CONF: sites written there are never served (ASM-12)"
	fi

	nginx_t
	if [ "$HX_RC" -ne 0 ]; then
		case $HX_OUT in
		*'nginx.pid" failed (2:'*) refuse "nginx -t fails only because nginx's pid directory is missing (it is made when nginx starts): start nginx, then run this again" ;;
		esac
		refuse "nginx -t fails on the host's current configuration: fix it first; this script changes nothing on an invalid configuration"
	fi
	result 1-nginx-t PASS "nginx -t passes on the host's configuration before any change"

	init_state
	show_procs
	{ [ "$INIT_STATE" = started ] && [ "$MASTER" != none ]; } ||
		refuse "nginx is not running ($INIT: $INIT_STATE, master $MASTER): this check needs it running, and does not start it"
	MASTER0=$MASTER
	log "  nginx.conf sha256: $(hx "sha256sum $NGINX_CONF | cut -d' ' -f1" && echo "$HX_OUT")"
	if [ "${IN_CONTAINER:-0}" = 1 ]; then
		result 1-host FAIL "running in a container: not a real host"
	fi
}

plan() {
	log ""
	log "== Plan. With --yes the script will, on ${SSH_TARGET:-this host}:"
	case $MODE in
	cleanup)
		log "  - wait for (or cancel, if queued) the operation an interrupted run left, kill its"
		log "    holder process, remove $INCDIR/nexus-check-<random>$SUFFIX (and ${SUFFIX}$DSUFFIX), run nginx -t,"
		log "    reload nginx through $INIT if the site was ever enabled, start nginx if the run"
		log "    stopped it, compare the sha256 of every other file, and remove $H_STATE."
		return
		;;
	esac
	log "  - use one disposable site, $SITE ($INCDIR/$SITE$SUFFIX), serving"
	log "    \"$(marker M1)\" on 127.0.0.1:$PORT, a port nothing listens on or names;"
	log "  - through nexus: whatIf, add, update, disable, update while disabled, whatIf an invalid"
	log "    text, enable, config test, start (a no-op, nginx is running) and reload, each read back"
	log "    with $INIT, /proc, ls -ln, sha256sum and $FETCH on 127.0.0.1:$PORT;"
	log "  - an invalid write and a write whose port, 127.0.0.1:$CPORT, a holder process"
	log "    (self-ending after 600 s) has taken: both must be rolled back;"
	log "  - ASM-7: write an invalid text, then a bind-conflict text, to $SITE$SUFFIX directly"
	log "    and run $(init_cmd reload) after each, recording the exit codes; the"
	log "    valid text goes back at once, and nginx keeps its running configuration;"
	if [ "$LIFECYCLE" = 1 ]; then
		log "  - --with-lifecycle: nexus nginx stop, start and restart. WARNING: this takes EVERY"
		log "    site on this nginx offline for a few seconds; production traffic is interrupted;"
	else
		log "  - not stop, start or restart nginx (they interrupt production traffic; opt in with"
		log "    --with-lifecycle; without it those results are NOT-RUN and the exit status is 3);"
	fi
	log "  - remove $SITE's file (rm, as root on the host: the provider cannot delete, OOS-4),"
	log "    run nginx -t, reload, and check that every other file under /etc/nginx and"
	log "    $INCDIR is byte-identical to the snapshot taken first."
	log "  It never edits nginx.conf or any other site. If it is interrupted (Ctrl-C once, or"
	log "  SIGTERM), its EXIT trap waits for a running operation, removes the site, runs nginx -t,"
	log "  reloads only if the site was ever enabled, and starts nginx if it had stopped it."
	log "  If even that is cut short: sh $PROG --yes --cleanup${SSH_TARGET:+ --ssh $SSH_TARGET}"
}

# --- cleanup (the EXIT trap, the end of a run, and --cleanup) -------------

cleanup() {
	trap '' INT TERM HUP
	note "== Cleanup"
	CLEANED=1
	# A submitted operation must end before the file is removed, or its
	# rollback could put it back.
	if [ -n "$INFLIGHT" ] && [ -n "$NXHOST" ]; then
		note "operation $INFLIGHT was submitted: waiting for it to end"
		_cj=$("$NEXUS_CLI" --host "$NXHOST" operation get "$INFLIGHT" -o jsonl 2>/dev/null | head -n 1)
		if [ "$(json_str status "$_cj")" = Queued ]; then
			nx operation cancel "$INFLIGHT" --yes -o jsonl
		fi
		op_wait "$INFLIGHT" 300
		case $OPS in
		Succeeded | Failed | Canceled) INFLIGHT= ;;
		*)
			result cleanup FAIL "operation $INFLIGHT has not ended: the site was NOT removed; run --yes --cleanup once it has"
			return 1
			;;
		esac
	elif [ -n "$INFLIGHT" ]; then
		log "operation $INFLIGHT may still run, and there is no nexus client to ask: waiting 240 s, the longest an nginx operation runs with the default timeouts"
		sleep 240
		INFLIGHT=
	fi
	# An interrupt can land after the CLI created an operation and before
	# its ID was recorded: wait for every nginx operation still open.
	if [ -n "$NXHOST" ]; then
		nx operation get --filter provider=nginx --filter status=Queued --filter status=Running -o jsonl
		for _o in $(printf '%s\n' "$NX_OUT" | grep -o '"id":"[^"]*"' | cut -d'"' -f4); do
			op_wait "$_o" 300
			case $OPS in
			Succeeded | Failed | Canceled) ;;
			*)
				result cleanup FAIL "nginx operation $_o has not ended: the site was NOT removed; run --yes --cleanup once it has"
				return 1
				;;
			esac
		done
	fi
	holder_stop
	if [ -n "$SITE" ]; then
		hrun "cd $(shown "$INCDIR") && ls -ln $SITE$SUFFIX $SITE$SUFFIX$DSUFFIX .$SITE.check-tmp 2>/dev/null; rm -f -- $SITE$SUFFIX $SITE$SUFFIX$DSUFFIX .$SITE.check-tmp; ls -la | grep -F $SITE || echo '$SITE: no file left'"
	fi
	nginx_t
	_trc=$HX_RC
	init_state
	if [ "$_trc" -ne 0 ]; then
		result cleanup FAIL "nginx -t fails after removing the site: nginx was NOT reloaded or started; look at the output above"
	elif [ "$STOPPED_BY_US" = 1 ] && [ "$INIT_STATE" != started ]; then
		note "this run stopped nginx: starting it"
		hrun "$(init_cmd start)"
		init_state
		STOPPED_BY_US=0
	elif [ "$ENABLED_EVER" = 1 ] && [ "$INIT_STATE" = started ]; then
		procs
		_cw=$WORKERS
		note "the site was enabled during the run: reloading nginx to drop it"
		hrun "$(init_cmd reload)"
		if wait_reload "$_cw" 15; then
			log "  (/proc) master $MASTER, workers $_cw -> $WORKERS: the reload was applied"
		else
			log "  (/proc) master $MASTER, workers $_cw -> $WORKERS: the reload was NOT applied within 15 s"
		fi
	fi
	if [ -n "$PORT" ] && [ "$INIT_STATE" = started ]; then
		fetch
		log "  127.0.0.1:$PORT: $FETCH_STATE"
		CLEAN_FETCH=$FETCH_STATE
	fi
	hrun "[ ! -f $H_STATE/snap.before ] || { $(snap_cmd); } >$H_STATE/snap.after; [ ! -f $H_STATE/snap.before ] || diff $H_STATE/snap.before $H_STATE/snap.after"
	SNAP_RC=$HX_RC
	hx "[ -f $H_STATE/snap.before ] && echo yes"
	SNAP_HAD=$HX_OUT
	hx "[ ! -f $H_STATE/snap.before ] || { grep ' $NGINX_CONF\$' $H_STATE/snap.before; grep ' $NGINX_CONF\$' $H_STATE/snap.after; }"
	CONF_LINES=$HX_OUT
	hrun "rm -rf $H_STATE"
	return 0
}

on_exit() {
	_rc=$?
	trap - EXIT
	trap '' INT TERM HUP
	if [ "$CHANGED" = 1 ] && [ "$CLEANED" = 0 ]; then
		note "stopped early (exit $_rc): undoing this run's changes"
		cleanup
		[ "${SNAP_HAD:-}" != yes ] || final_checks
		token_check
		summary
	fi
	rm -rf "$TMPD"
	exit "$_rc"
}

summary() {
	log ""
	log "== Summary ($MODE)"
	printf '%s\n' "$RESULTS" | while IFS='|' read -r id st detail; do
		[ -z "$id" ] || log "  $id  $st  $detail"
	done
	[ "${IN_CONTAINER:-0}" = 0 ] || log "  (container: not real-host evidence)"
	log "transcript: $TRANSCRIPT"
}

# --- the steps -----------------------------------------------------------

# expect_site ID WHAT ON OFF BODY: the readback after a site change. ON and
# OFF are the wanted sha256 of the enabled and disabled file (or absent),
# BODY the wanted marker on the port, or "refused". OK is 1 when it matched.
readback() {
	site_files
	fetch
	show_procs
	_rb=
	[ "$SITE_ON" = "$1" ] || _rb="$_rb enabled file $SITE_ON (want $1);"
	[ "$SITE_OFF" = "$2" ] || _rb="$_rb disabled file $SITE_OFF (want $2);"
	if [ "$1" != absent ] || [ "$2" != absent ]; then
		[ "$SITE_MODE" = "-rw-r--r-- 0 0" ] || _rb="$_rb mode/owner '$SITE_MODE' (want -rw-r--r-- 0 0);"
	fi
	case $3 in
	refused) [ "$FETCH_STATE" = refused ] || _rb="$_rb port $PORT $FETCH_STATE '$FETCH_BODY' (want refused);" ;;
	*) [ "$FETCH_STATE" = served ] && [ "$FETCH_BODY" = "$(marker "$3")" ] || _rb="$_rb port $PORT $FETCH_STATE '$FETCH_BODY' (want '$(marker "$3")');" ;;
	esac
	[ "$MASTER" = "$MASTER0" ] || _rb="$_rb master $MASTER (want $MASTER0, unchanged);"
}

step3_first() {
	log ""
	note "== Step 3: add, update with readback (TC-118 step 3)"
	M1=$(content M1) M2=$(content M2) M3=$(content M3)
	S1=$(sha_text "$M1") S2=$(sha_text "$M2") S3=$(sha_text "$M3")
	log "  M1 (sha256 $S1):"
	printf '%s\n' "$M1" | sed 's/^/    | /' | tee -a "$TRANSCRIPT"
	log "  M2 sha256 $S2, M3 sha256 $S3: the same with M2 and M3 as the mark"

	procs
	_w=$WORKERS
	op nginxsite add "$SITE" --content "$M1" --what-if
	readback absent absent refused
	if [ "$OPS" = Succeeded ] && [ "$(json_bool valid "$OPJ")" = true ] && [ "$(json_bool whatIf "$OPJ")" = true ] &&
		[ "$WORKERS" = "$_w" ] && [ -z "$_rb" ]; then
		result 3a-whatif PASS "add --what-if: Succeeded, valid true; no file, $PORT refused, workers unchanged"
	else
		result 3a-whatif FAIL "add --what-if: $OPS${OPP:+ $OPP} valid $(json_bool valid "$OPJ");$_rb workers $_w -> $WORKERS"
	fi

	ENABLED_EVER=1
	save_state
	procs
	_w=$WORKERS
	op nginxsite add "$SITE" --content "$M1"
	readback "$S1" absent M1
	_rs=$(json_str sha256 "$OPJ") _v=$(json_str verdict "$OPJ")
	if [ "$OPS" = Succeeded ] && [ "$_rs" = "$S1" ] && [ "$(json_bool created "$OPJ")" = true ] &&
		[ "$(json_bool reloaded "$OPJ")" = true ] && [ "$_v" = confirmed ] && [ "$WORKERS" != "$_w" ] && [ -z "$_rb" ]; then
		result 3b-add PASS "add: Succeeded; result sha256 = sha256 of M1 = sha256sum on the host ($S1); root 0644; $PORT serves M1; reload confirmed, workers $_w -> $WORKERS, master $MASTER unchanged"
	else
		result 3b-add FAIL "add: $OPS${OPP:+ $OPP}, result sha256 $_rs (want $S1), verdict $_v;$_rb workers $_w -> $WORKERS"
	fi

	procs
	_w=$WORKERS
	op nginxsite update "$SITE" --content "$M2" --if-match "$S1"
	readback "$S2" absent M2
	_rs=$(json_str sha256 "$OPJ") _ps=$(json_str previousSha256 "$OPJ")
	if [ "$OPS" = Succeeded ] && [ "$_rs" = "$S2" ] && [ "$_ps" = "$S1" ] && [ "$(json_bool reloaded "$OPJ")" = true ] &&
		[ "$WORKERS" != "$_w" ] && [ -z "$_rb" ]; then
		result 3c-update PASS "update --if-match: Succeeded; sha256 $S2 on the host and in the result, previousSha256 $S1; $PORT serves M2"
	else
		result 3c-update FAIL "update: $OPS${OPP:+ $OPP}, sha256 $_rs previous $_ps;$_rb"
	fi
}

step3_second() {
	log ""
	note "== Step 3 (continued): disable, update while disabled, whatIf invalid, enable"
	procs
	_w=$WORKERS
	op nginxsite stop "$SITE" --yes
	readback absent "$S2" refused
	if [ "$OPS" = Succeeded ] && [ "$(json_bool reloaded "$OPJ")" = true ] && [ "$WORKERS" != "$_w" ] && [ -z "$_rb" ]; then
		result 3d-disable PASS "disable: Succeeded; only $SITE$SUFFIX$DSUFFIX on disk (sha256 $S2, unchanged); $PORT refused; reloaded"
	else
		result 3d-disable FAIL "disable: $OPS${OPP:+ $OPP};$_rb workers $_w -> $WORKERS"
	fi

	procs
	_w=$WORKERS
	op nginxsite update "$SITE" --content "$M3" --if-match "$S2"
	readback absent "$S3" refused
	if [ "$OPS" = Succeeded ] && [ "$(json_str sha256 "$OPJ")" = "$S3" ] && [ "$(json_bool reloaded "$OPJ")" = false ] &&
		[ "$WORKERS" = "$_w" ] && [ -z "$_rb" ]; then
		result 3e-update-disabled PASS "update of the disabled site: Succeeded, reloaded false; $SITE$SUFFIX$DSUFFIX sha256 $S3; $PORT still refused; workers unchanged"
	else
		result 3e-update-disabled FAIL "update while disabled: $OPS${OPP:+ $OPP} reloaded $(json_bool reloaded "$OPJ");$_rb workers $_w -> $WORKERS"
	fi

	op nginxsite update "$SITE" --content "$BAD_CONTENT" --what-if
	readback absent "$S3" refused
	if [ "$OPS" = Succeeded ] && [ "$(json_bool valid "$OPJ")" = false ] && [ -z "$_rb" ]; then
		result 3f-whatif-invalid PASS "update --what-if of an invalid text: Succeeded, valid false; nothing changed"
	else
		result 3f-whatif-invalid FAIL "whatIf invalid: $OPS${OPP:+ $OPP} valid $(json_bool valid "$OPJ");$_rb"
	fi

	procs
	_w=$WORKERS
	op nginxsite start "$SITE"
	readback "$S3" absent M3
	if [ "$OPS" = Succeeded ] && [ "$(json_bool reloaded "$OPJ")" = true ] && [ "$WORKERS" != "$_w" ] && [ -z "$_rb" ]; then
		result 3g-enable PASS "enable: Succeeded; $SITE$SUFFIX sha256 $S3; $PORT serves M3; reloaded"
	else
		result 3g-enable FAIL "enable: $OPS${OPP:+ $OPP};$_rb workers $_w -> $WORKERS"
	fi
}

step2() {
	log ""
	note "== Step 2: lifecycle with $INIT readback (TC-118 step 2); the site serves M2"
	op nginxconfig test
	nginx_t
	if [ "$OPS" = Succeeded ] && [ "$(json_bool valid "$OPJ")" = true ] && [ "$HX_RC" -eq 0 ]; then
		result 2a-test PASS "nginxconfig test: valid true; nginx -t on the host exits 0"
	else
		result 2a-test FAIL "nginxconfig test: $OPS${OPP:+ $OPP} valid $(json_bool valid "$OPJ"); nginx -t exit $HX_RC"
	fi

	op nginx start
	init_state
	show_procs
	if [ "$OPS" = Succeeded ] && [ "$(json_bool changed "$OPJ")" = false ] && [ "$INIT_STATE" = started ] && [ "$MASTER" = "$MASTER0" ]; then
		result 2b-start-running PASS "start of a running nginx: Succeeded, changed false; $INIT says started; master $MASTER unchanged"
	else
		result 2b-start-running FAIL "start (running): $OPS${OPP:+ $OPP} changed $(json_bool changed "$OPJ"); $INIT $INIT_STATE; master $MASTER (was $MASTER0)"
	fi

	procs
	_w=$WORKERS
	op nginx reload
	init_state
	procs
	fetch
	_v=$(json_str verdict "$OPJ") _mp=$(json_num masterPid "$OPJ") _ie=$(json_num initExitCode "$OPJ")
	log "  (/proc) master $MASTER, workers $_w -> $WORKERS; result masterPid $_mp, initExitCode $_ie, verdict $_v"
	if [ "$OPS" = Succeeded ] && [ "$_v" = confirmed ] && [ "$_mp" = "$MASTER" ] && [ "$MASTER" = "$MASTER0" ] &&
		[ "$WORKERS" != "$_w" ] && [ "$INIT_STATE" = started ] && [ "$FETCH_BODY" = "$(marker M2)" ]; then
		result 2c-reload PASS "reload: Succeeded, verdict confirmed, initExitCode $_ie; same master $MASTER, every worker replaced ($_w -> $WORKERS); $PORT serves M2"
	else
		result 2c-reload FAIL "reload: $OPS${OPP:+ $OPP} verdict $_v master $_mp/$MASTER (was $MASTER0) workers $_w -> $WORKERS; $INIT $INIT_STATE; $PORT $FETCH_STATE"
	fi

	if [ "$LIFECYCLE" != 1 ]; then
		for _id in 2d-stop 2e-start 2f-restart; do
			result "$_id" NOT-RUN "opt-in: --with-lifecycle (interrupts production traffic)"
		done
		return
	fi
	log "WARNING: --with-lifecycle: stopping nginx now. Every site on this host is offline until the start below."
	STOPPED_BY_US=1
	save_state
	op nginx stop --yes
	init_state
	show_procs
	fetch
	if [ "$OPS" = Succeeded ] && [ "$(json_str state "$OPJ")" = stopped ] && [ "$INIT_STATE" = stopped ] &&
		[ "$MASTER" = none ] && [ "$FETCH_STATE" = refused ]; then
		result 2d-stop PASS "stop: Succeeded, state stopped; $INIT says stopped; no master in /proc; $PORT refused"
	else
		result 2d-stop FAIL "stop: $OPS${OPP:+ $OPP} state $(json_str state "$OPJ"); $INIT $INIT_STATE; master $MASTER; $PORT $FETCH_STATE"
	fi

	op nginx start
	init_state
	show_procs
	fetch
	_mp=$(json_num mainPid "$OPJ")
	if [ "$OPS" = Succeeded ] && [ "$(json_str state "$OPJ")" = running ] && [ "$INIT_STATE" = started ] &&
		[ "$MASTER" != none ] && [ "$_mp" = "$MASTER" ] && [ "$FETCH_BODY" = "$(marker M2)" ]; then
		STOPPED_BY_US=0
		save_state
		result 2e-start PASS "start: Succeeded, state running, mainPid $_mp = the master in /proc; $INIT says started; $PORT serves M2"
	else
		result 2e-start FAIL "start: $OPS${OPP:+ $OPP} state $(json_str state "$OPJ") mainPid $_mp; $INIT $INIT_STATE; master $MASTER; $PORT $FETCH_STATE"
	fi
	[ "$INIT_STATE" != started ] || { STOPPED_BY_US=0; save_state; }

	_m=$MASTER
	op nginx restart
	init_state
	show_procs
	fetch
	_mp=$(json_num mainPid "$OPJ")
	if [ "$OPS" = Succeeded ] && [ "$(json_str state "$OPJ")" = running ] && [ "$INIT_STATE" = started ] &&
		[ "$MASTER" != none ] && [ "$MASTER" != "$_m" ] && [ "$_mp" = "$MASTER" ] && [ "$FETCH_BODY" = "$(marker M2)" ]; then
		result 2f-restart PASS "restart: Succeeded; a new master ($_m -> $MASTER = mainPid); $INIT says started; $PORT serves M2"
	else
		result 2f-restart FAIL "restart: $OPS${OPP:+ $OPP} mainPid $_mp; $INIT $INIT_STATE; master $_m -> $MASTER; $PORT $FETCH_STATE"
	fi
	MASTER0=$MASTER
}

step4() {
	log ""
	note "== Step 4: an invalid write and a port-conflict write are rolled back (TC-118 step 4)"
	procs
	_w=$WORKERS
	op nginxsite update "$SITE" --content "$BAD_CONTENT"
	readback "$S3" absent M3
	if [ "$OPS" = Failed ] && [ "$OPP" = nginx-config-invalid ] && [ "$(json_bool rolledBack "$OPJ")" = true ] &&
		[ "$WORKERS" = "$_w" ] && [ -z "$_rb" ]; then
		result 4a-invalid PASS "invalid write: Failed nginx-config-invalid, rolledBack true; sha256 still $S3; $PORT serves the previous content M3; workers unchanged"
	else
		result 4a-invalid FAIL "invalid write: $OPS${OPP:+ $OPP} rolledBack $(json_bool rolledBack "$OPJ");$_rb workers $_w -> $WORKERS"
	fi

	if ! holder_start; then
		result 4b-conflict FAIL "could not hold 127.0.0.1:$CPORT on the host"
		holder_stop
		return
	fi
	M4=$(content M4 "$CPORT")
	procs
	_w=$WORKERS
	op nginxsite update "$SITE" --content "$M4"
	holder_stop
	readback "$S3" absent M3
	_v1=$(json_nth_str verdict 1 "$OPJ") _v2=$(json_nth_str verdict 2 "$OPJ")
	PROVIDER_BIND_EXIT=$(json_num initExitCode "$OPJ")
	if [ "$OPS" = Failed ] && [ "$OPP" = nginx-reload-failed ] && [ "$(json_bool rolledBack "$OPJ")" = true ] &&
		[ "$_v1" = rejected ] && [ "$_v2" = confirmed ] && [ -z "$_rb" ]; then
		result 4b-conflict PASS "port-conflict write: Failed nginx-reload-failed, rolledBack true, observed.verdict rejected, rollbackReload.verdict confirmed; sha256 still $S3; $PORT serves the previous content M3"
	else
		result 4b-conflict FAIL "port-conflict write: $OPS${OPP:+ $OPP} rolledBack $(json_bool rolledBack "$OPJ") verdicts $_v1/$_v2;$_rb"
	fi
	if [ "$PROVIDER_BIND_EXIT" = "$FAKE_RELOAD_BIND" ]; then
		result 4b-initexit PASS "observed.initExitCode $PROVIDER_BIND_EXIT for the bind conflict = svcctltest's $INIT reload-bind-conflict ($FAKE_RELOAD_BIND)"
	else
		result 4b-initexit FAIL "observed.initExitCode ${PROVIDER_BIND_EXIT:-?} for the bind conflict, svcctltest has $FAKE_RELOAD_BIND: a defect in the fake (TC-11)"
	fi
}

# errlog_size: ERRLOG_SIZE, the size of nginx's error log (0 when absent).
errlog_size() {
	hx "wc -c <$ERRLOG 2>/dev/null || echo 0"
	ERRLOG_SIZE=$(echo "$HX_OUT" | tr -d ' ')
}

# wait_emerg SINCE PATTERN: wait up to 5 s for a new error-log line holding
# PATTERN after byte SINCE (the master has acted on the reload). Logs it.
wait_emerg() {
	_we=$(($(date +%s) + 5))
	EMERG_SEEN=no
	while :; do
		hx "tail -c +$(($1 + 1)) $ERRLOG 2>/dev/null | grep -F -- $(shown "$2")"
		[ "$HX_RC" -ne 0 ] || { EMERG_SEEN=yes; break; }
		[ "$(date +%s)" -lt "$_we" ] || break
		sleep 0.2
	done
	hrun "tail -c +$(($1 + 1)) $ERRLOG 2>/dev/null | tail -n 5"
}

# direct_reload KIND TEXT PATTERN: ASM-7 on the disposable site only: write
# TEXT, reload through the init system, record its exit code, put M3 back as
# soon as nginx has acted on it, and check nginx kept its configuration.
direct_reload() {
	procs
	_w=$WORKERS _m=$MASTER
	errlog_size
	_es=$ERRLOG_SIZE
	if ! write_site "$2"; then
		write_site "$M3"
		return 1
	fi
	nginx_t
	BAD_T=$HX_RC
	hrun "$(init_cmd reload)"
	RELOAD_RC=$HX_RC
	wait_emerg "$_es" "$3"
	write_site "$M3"
	if [ "$INIT" = systemd ] && [ "$RELOAD_RC" -ne 0 ]; then
		hrun "journalctl -u $SVC.service -n 6 --no-pager -o cat"
	fi
	note "the valid text M3 is back in $SITE$SUFFIX"
	sleep 1
	procs
	log "  (/proc) master $_m -> $MASTER, workers $_w -> $WORKERS"
	KEPT=no
	[ "$MASTER" != "$_m" ] || [ "$WORKERS" != "$_w" ] || KEPT=yes
	nginx_t
	DIRECT_T=$HX_RC
	fetch
}

step5() {
	log ""
	note "== Step 5: ASM-7, the init system's reload exit codes (TC-118 step 5 / TC-119)"
	log "  The disposable site only. Each bad text stays in place until nginx has acted on the"
	log "  reload (its error-log line, at most 5 s), then the valid M3 is written back."

	if ! direct_reload invalid "$BAD_CONTENT" 'nexus_check_unknown_directive'; then
		result 5a-invalid-exit FAIL "could not write the invalid text to $SITE$SUFFIX"
		return
	fi
	_note="$(init_cmd reload) exit $RELOAD_RC with an invalid configuration (nginx -t exit $BAD_T); svcctltest has $FAKE_RELOAD_INVALID"
	if [ "$RELOAD_RC" = "$FAKE_RELOAD_INVALID" ] && [ "$BAD_T" -ne 0 ] && [ "$KEPT" = yes ] && [ "$DIRECT_T" -eq 0 ] && [ "$FETCH_BODY" = "$(marker M3)" ]; then
		result 5a-invalid-exit PASS "$_note; nginx kept its workers; M3 restored, nginx -t passes, $PORT serves M3"
	elif [ "$RELOAD_RC" != "$FAKE_RELOAD_INVALID" ]; then
		result 5a-invalid-exit FAIL "$_note: a defect in the fake (TC-11)"
	else
		result 5a-invalid-exit FAIL "$_note; nginx -t on it exit $BAD_T, workers kept $KEPT, nginx -t exit $DIRECT_T, $PORT $FETCH_STATE '$FETCH_BODY'"
	fi

	if ! holder_start; then
		result 5b-bind-exit FAIL "could not hold 127.0.0.1:$CPORT on the host"
		holder_stop
		return
	fi
	M4=$(content M4 "$CPORT")
	if ! direct_reload bind "$M4" "127.0.0.1:$CPORT failed"; then
		holder_stop
		result 5b-bind-exit FAIL "could not write the bind-conflict text to $SITE$SUFFIX"
		return
	fi
	holder_stop
	_note="$(init_cmd reload) exit $RELOAD_RC with a bind conflict on 127.0.0.1:$CPORT (nginx -t exit $BAD_T: only the bind fails); svcctltest has $FAKE_RELOAD_BIND"
	if [ "$RELOAD_RC" = "$FAKE_RELOAD_BIND" ] && [ "$BAD_T" -eq 0 ] && [ "$KEPT" = yes ] && [ "$DIRECT_T" -eq 0 ] && [ "$FETCH_BODY" = "$(marker M3)" ]; then
		result 5b-bind-exit PASS "$_note; the master kept its workers (bind() failure in $ERRLOG: $EMERG_SEEN), which the exit code does not show; M3 restored, $PORT serves M3"
	elif [ "$RELOAD_RC" != "$FAKE_RELOAD_BIND" ]; then
		result 5b-bind-exit FAIL "$_note: a defect in the fake (TC-11)"
	else
		result 5b-bind-exit FAIL "$_note; workers kept $KEPT (nginx -t on it exit $BAD_T), nginx -t exit $DIRECT_T, $PORT $FETCH_STATE '$FETCH_BODY'"
	fi
	if [ -n "${PROVIDER_BIND_EXIT:-}" ] && [ "$PROVIDER_BIND_EXIT" != "$RELOAD_RC" ]; then
		result 5b-agrees FAIL "the provider observed initExitCode $PROVIDER_BIND_EXIT for the same case, the direct reload $RELOAD_RC"
	fi
}

step6() {
	log ""
	note "== Step 6: remove the site (rm on the host, OOS-4), test, reload (TC-118 step 6)"
	cleanup
	hx "cd $(shown "$INCDIR") && ls -a | grep -F $SITE"
	if [ "$HX_RC" -ne 0 ] && [ "${CLEAN_FETCH:-}" = refused ]; then
		result 6-removed PASS "no file of $SITE is left in $INCDIR; $PORT refused after the reload"
	else
		result 6-removed FAIL "left in $INCDIR: ${HX_OUT:-none}; $PORT ${CLEAN_FETCH:-not checked}"
	fi
}

final_checks() {
	if [ "${SNAP_HAD:-}" = yes ] && [ "${SNAP_RC:-1}" -eq 0 ]; then
		result untouched PASS "every other file under /etc/nginx and $INCDIR is byte-identical to the snapshot (type, mode, owner, size, sha256)"
	else
		result untouched FAIL "files under /etc/nginx or $INCDIR DIFFER from the snapshot (the diff is above) - check them now"
	fi
	_c1=$(printf '%s\n' "$CONF_LINES" | sed -n 1p) _c2=$(printf '%s\n' "$CONF_LINES" | sed -n 2p)
	if [ -n "$_c1" ] && [ "$_c1" = "$_c2" ]; then
		result nginx-conf PASS "$NGINX_CONF unchanged: $_c1"
	else
		result nginx-conf FAIL "$NGINX_CONF before '$_c1', after '$_c2'"
	fi
	procs
	if [ "$LIFECYCLE" = 0 ] && [ -n "$MASTER0" ]; then
		if [ "$MASTER" = "$MASTER0" ]; then
			result master PASS "nginx's master is the one from the start ($MASTER): no restart, only reloads"
		else
			result master FAIL "nginx's master changed ($MASTER0 -> $MASTER) without --with-lifecycle"
		fi
	fi
}

token_check() {
	[ -n "${NEXUS_TOKEN:-}" ] || return 0
	if awk 'index($0, ENVIRON["NEXUS_TOKEN"]) { f = 1 } END { exit !f }' "$TRANSCRIPT"; then
		result token-absent FAIL "NEXUS_TOKEN appears in the transcript: delete it and revoke the token"
	else
		result token-absent PASS "NEXUS_TOKEN does not appear in the transcript"
	fi
}

# --- main ----------------------------------------------------------------

trap on_exit EXIT
trap 'exit 130' INT TERM HUP

host_root
header
detect_init
if [ "$MODE" = cleanup ]; then
	fetch_tool
	if load_state; then
		log "state of the interrupted run ($H_STATE/state):"
		printf '%s\n' "$STATE_TEXT" | sed 's/^/  /' | tee -a "$TRANSCRIPT"
		setup_client || log "(no NEXUS_TOKEN and no --nexus-host: an operation a run left cannot be asked about)"
	else
		setup_client || log "(no NEXUS_TOKEN and no --nexus-host: the provider cannot be asked)"
		[ -n "$NXHOST" ] && { nx nginx get -o jsonl; INCDIR=$(json_str includeDir "$NX_OUT"); SUFFIX=$(json_str includeSuffix "$NX_OUT"); DSUFFIX=$(json_str disabledSuffix "$NX_OUT"); SVC=$(json_str service "$NX_OUT"); }
		if [ -z "$INCDIR" ]; then
			hx '[ -r /etc/alpine-release ] && echo /etc/nginx/http.d || echo /etc/nginx/conf.d'
			INCDIR=$HX_OUT
		fi
		hx "cd $(shown "$INCDIR") && ls -a | grep -E '^\\.?nexus-check-[0-9a-f]{8}\\.' | sed -n 's/^\\.\\{0,1\\}\\(nexus-check-[0-9a-f]\\{8\\}\\)\\..*/\\1/p' | sort -u"
		SITE=$(printf '%s\n' "$HX_OUT" | head -n 1)
		log "no state in $H_STATE; a disposable site in $INCDIR: ${SITE:-none}"
		# Without state, a site of this script that exists may have been
		# enabled: reload to be safe.
		[ -z "$SITE" ] || ENABLED_EVER=1
	fi
	pid_path
	plan
	[ "$YES" = 1 ] || { log ""; refuse "nothing was changed: re-run with --yes to do the above"; }
	CHANGED=1
	cleanup
	[ "${SNAP_HAD:-}" != yes ] || final_checks
	token_check
	CHANGED=0
	summary
	[ "$FAILS" -eq 0 ] || exit 1
	exit 0
fi

hx "[ -e $H_STATE ]"
if [ "$HX_RC" -eq 0 ]; then
	hrun "ls -la $H_STATE; cat $H_STATE/state"
	refuse "an earlier run did not finish ($H_STATE exists): run with --yes --cleanup first"
fi
if ! setup_client; then
	[ "$YES" = 0 ] || refuse "NEXUS_TOKEN is not set (or pass --nexus-host). Create one on the host: nexusd token create --name nginx-check --scope nginx:read --scope nginx:write --scope nginx:config --expires 1d"
fi
facts
RAND=$(rand_hex8)
SITE=nexus-check-$RAND
hx "cd $(shown "$INCDIR") && ls -a | grep -F $SITE"
[ "$HX_RC" -ne 0 ] || refuse "$SITE already exists in $INCDIR"
pick_ports
log "  disposable site $SITE, port $PORT, conflict port $CPORT"
hx "{ $(snap_cmd); }"
SNAP0=$HX_OUT
log "  snapshot: $(printf '%s\n' "$SNAP0" | grep -c .) entries under /etc/nginx and $INCDIR, every other file's sha256:"
printf '%s\n' "$SNAP0" | sed 's/^/    /' | tee -a "$TRANSCRIPT"
plan
if [ "$YES" != 1 ]; then
	log ""
	refuse "nothing was changed: re-run with --yes to do the above"
fi
[ "$PROVIDER_OK" = 1 ] || refuse "the provider is not running nginx under $INIT (1-provider)"

CHANGED=1
save_state
hrun "{ $(snap_cmd); } >$H_STATE/snap.before && wc -l <$H_STATE/snap.before"
step3_first
step2
step3_second
step4
step5
step6
final_checks
token_check
CHANGED=0
summary
[ "$FAILS" -eq 0 ] || exit 1
[ "$INCONCL" -eq 0 ] || exit 3
exit 0
