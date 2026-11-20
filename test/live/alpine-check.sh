#!/bin/sh
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# Live check of nginx-provider Part A (WI 3.5): TC-51 on a real Alpine host
# with OpenRC, and TC-52 (--ubuntu) on a real Ubuntu host. Each host runs
# the release tarball. Copy this file to the host and run it there as root.
# It is POSIX sh, and BusyBox ash runs it.
#
#   sh alpine-check.sh                          preconditions and step 1 facts,
#                                               then the plan; changes nothing
#   NEXUS_TOKEN=... sh alpine-check.sh --yes    TC-51 steps 1 to 4 and 6
#   NEXUS_TOKEN=... sh alpine-check.sh --yes --reboot-phase1
#       (then reboot the host yourself; this script never reboots)
#   NEXUS_TOKEN=... sh alpine-check.sh --yes --reboot-phase2
#   NEXUS_TOKEN=... sh alpine-check.sh --yes --cleanup
#                                               undo whatever an interrupted
#                                               run left behind
#   sh alpine-check.sh --ubuntu                 TC-52, read-only
#
# The procedure and the transcript template are in
# docs/verification/alpine-openrc.md. Every command the script runs and its
# output go to the transcript file (--transcript FILE). NEXUS_TOKEN is
# never written to it.
#
# This script never edits nginx's configuration and never touches the nginx
# service. It runs a harmless test operation: a scripts-provider script it
# installs, which sleeps and writes only under /var/lib/nexus-alpine-check.
# That script needs a catalog entry in /etc/nexus/nexusd.yaml. The script
# backs the file up and adds the entry, then puts the file back byte for
# byte at the end, and reloads nexusd both times.

# Functions run through "step$N" and the EXIT trap, which shellcheck
# cannot follow.
# shellcheck disable=SC2317
set -u
umask 022

PROG=alpine-check.sh
STATE_DIR=/var/lib/nexus-alpine-check
LIBEXEC=/usr/local/libexec/nexus-alpine-check
OP_SCRIPT=$LIBEXEC/op.sh
CATALOG_ID=nexus-alpine-check
NEXUS_CONFIG=/etc/nexus/nexusd.yaml
CONFIG_BAK=$STATE_DIR/nexusd.yaml.orig
CONFD=/etc/conf.d/nexusd
CONFD_BAK=$STATE_DIR/conf.d-nexusd.orig
CONFD_ABSENT=$STATE_DIR/conf.d-nexusd.absent
RC_CGROUP_SH=/usr/libexec/rc/sh/rc-cgroup.sh
RC_CGROUP_BAK=$STATE_DIR/rc-cgroup.sh.orig
REBOOT_STATE=$STATE_DIR/reboot.state
OPS_LIST=$STATE_DIR/ops
CANARY_LIST=$STATE_DIR/canaries
NEXUSD_BIN=/usr/local/bin/nexusd
NEXUSD_STATE=/var/lib/nexus
CGROOT=/sys/fs/cgroup
BOOT_ID=/proc/sys/kernel/random/boot_id
UNIT=/etc/systemd/system/nexusd.service
# SHA-256 of deploy/nexusd.service at d04583a, which TC-52 requires the
# installed unit to equal. Re-derive it in a checkout with:
#   git show d04583a:deploy/nexusd.service | sha256sum
UNIT_SHA256_D04583A=5dcacb089d8ff0d0dfcae1f5d1f5e40b42437690930aa3a9787e3d8ff629e76a
# OpenRC issue #910: rc-cgroup.sh's cgroup_running() looks for
# /sys/fs/cgroup/<svc>, so rc_cgroup_cleanup="YES" does nothing on a unified
# host, where the group is openrc.<svc>. Upstream commit 1cf3944 fixed it.
# shellcheck disable=SC2016 # The lines as rc-cgroup.sh has them, unexpanded.
RC910_BUGGY='[ -d "/sys/fs/cgroup/${RC_SVCNAME}" ] ||'
# shellcheck disable=SC2016
RC910_FIXED='[ -d "/sys/fs/cgroup/openrc.${RC_SVCNAME}" ] ||'

usage() {
	cat <<'EOF'
Usage: sh alpine-check.sh [--yes] [MODE] [OPTIONS]

Modes (default: TC-51 steps 1, 2, 3, 4 and 6):
  --reboot-phase1   start a long test operation, record boot_id, and print
                    how to reboot (TC-51 step 5, part 1). Never reboots.
  --reboot-phase2   after the reboot: check the operation ended
                    outcome-unknown and that boot_id changed, then clean up
  --cleanup         cancel this script's test operations, kill its canary,
                    restore every file it changed, and start nexusd if an
                    interrupted step left it stopped
  --ubuntu          TC-52 on an Ubuntu host (read-only)

Options:
  --yes             required for anything that changes the host: restarting,
                    stopping or reloading nexusd, editing /etc/nexus/nexusd.yaml
                    and /etc/conf.d/nexusd, starting test operations. Without
                    it, the script checks preconditions, records the step 1
                    facts and prints its plan.
  --steps LIST      run only these TC-51 steps, e.g. --steps 2,6 (1 always runs)
  --with-910-fix    step 3 only: if rc-cgroup.sh has OpenRC issue #910, apply
                    upstream 1cf3944's cgroup_running() for the step, and put
                    the original file back afterwards
  --catalog-ready   do not edit /etc/nexus/nexusd.yaml; you have added the
                    catalog entry yourself (the refusal message shows it)
  --transcript FILE where to write the transcript (default:
                    ./alpine-check-<host>-<UTC time>.txt)
  -h, --help        this text

Environment:
  NEXUS_TOKEN   a token with scopes scripts:read and scripts:run (required
                except with --ubuntu). Create one, and revoke it afterwards:
                  nexusd token create --name alpine-check --scope scripts:read --scope scripts:run --expires 1d
                  nexusd token revoke alpine-check
  NEXUS_ADDR    host:port nexusd listens on (default: the first listen
                address in /etc/nexus/nexusd.yaml, else 127.0.0.1:7443)
  NEXUS_URL     the full base URL instead, such as https://nas.lan:7443
  NEXUS_CACERT  the certificate to trust for HTTPS (default:
                /var/lib/nexus/tls/cert.pem, the one tls.mode auto generates)

Exit status: 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or --yes is missing); 3 nothing failed but a check was
inconclusive (VACUOUS or NOT-RUN).
EOF
}

YES=0
MODE=main
WITH_FIX=0
CATALOG_READY=0
STEPS="2 3 4 6"
TRANSCRIPT=
ARGS="$*"
while [ $# -gt 0 ]; do
	case $1 in
	--yes) YES=1 ;;
	--reboot-phase1) MODE=reboot1 ;;
	--reboot-phase2) MODE=reboot2 ;;
	--cleanup) MODE=cleanup ;;
	--ubuntu) MODE=ubuntu ;;
	--with-910-fix) WITH_FIX=1 ;;
	--catalog-ready) CATALOG_READY=1 ;;
	--steps)
		[ $# -ge 2 ] || { echo "$PROG: --steps needs a list, such as 2,6" >&2; exit 2; }
		STEPS=
		for s in $(echo "$2" | tr ',' ' '); do
			case $s in
			1) ;;
			2 | 3 | 4 | 6) STEPS="$STEPS $s" ;;
			5) echo "$PROG: step 5 is --reboot-phase1 and --reboot-phase2" >&2; exit 2 ;;
			*) echo "$PROG: unknown step $s (steps are 1, 2, 3, 4 and 6)" >&2; exit 2 ;;
			esac
		done
		shift
		;;
	--transcript)
		[ $# -ge 2 ] || { echo "$PROG: --transcript needs a file name" >&2; exit 2; }
		TRANSCRIPT=$2
		shift
		;;
	-h | --help) usage; exit 0 ;;
	*) echo "$PROG: unknown argument: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done

RUN_ID=$(date -u +%Y%m%d%H%M%S)
if [ -z "$TRANSCRIPT" ]; then
	prefix=alpine-check
	[ "$MODE" != ubuntu ] || prefix=ubuntu-check
	TRANSCRIPT=./$prefix-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).txt
fi
if ! : >>"$TRANSCRIPT"; then
	echo "$PROG: cannot write the transcript $TRANSCRIPT" >&2
	exit 2
fi
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/alpine-check.XXXXXX") || { echo "$PROG: mktemp failed" >&2; exit 2; }

FAILS=0
INCONCL=0
RESULTS=
CHANGED=0 # this run changed the host, so an early exit must undo it
KEEP=0    # reboot phase 1 finished: leave everything for phase 2
CLEANED=0
WAS_STARTED=0
BASE_URL=
CACERT=
CONNECT_TO=
NEXUSD_PID=

# --- transcript ----------------------------------------------------------

log() { printf '%s\n' "$*" | tee -a "$TRANSCRIPT"; }
note() { log "[$(date -u +%H:%M:%S)] $*"; }

# shown ARG...: the arguments as a shell would need them typed, quoted where
# they hold anything but plain characters.
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

# run CMD...: log the command, run it, log its output and a non-zero exit
# status. RUN_OUT and RUN_RC hold the output and status.
run() {
	log "\$ $(shown "$@")"
	RUN_OUT=$("$@" 2>&1)
	RUN_RC=$?
	[ -z "$RUN_OUT" ] || printf '%s\n' "$RUN_OUT" | tee -a "$TRANSCRIPT"
	[ "$RUN_RC" -eq 0 ] || log "(exit $RUN_RC)"
	return "$RUN_RC"
}

# result ID STATUS DETAIL: STATUS is PASS, FAIL, VACUOUS, NOT-RUN, INFO or
# MANUAL.
result() {
	RESULTS="$RESULTS
$1|$2|$3"
	log "RESULT $1: $2 - $3"
	case $2 in
	FAIL) FAILS=$((FAILS + 1)) ;;
	VACUOUS | NOT-RUN) INCONCL=$((INCONCL + 1)) ;;
	esac
}

refuse() {
	log "REFUSED: $*"
	exit 2
}

in_container() {
	[ -e /.dockerenv ] || [ -e /run/.containerenv ] || grep -qa 'container=' /proc/1/environ 2>/dev/null
}

header() {
	log "# $PROG transcript: nginx-provider WI 3.5 (TC-51, TC-52)"
	log "# started $(date -u +%Y-%m-%dT%H:%M:%SZ) on host $(hostname), mode $MODE, arguments: ${ARGS:-(none)}"
	log "# script sha256 $(sha256sum "$0" 2>/dev/null | cut -d' ' -f1)"
	if in_container; then
		log "# CONTAINER DETECTED (/.dockerenv, /run/.containerenv or container= in PID 1's environment):"
		log "# this transcript is a dry run, NOT real-host evidence."
	fi
	log ""
}

summary() {
	log ""
	log "== Summary ($MODE)"
	printf '%s\n' "$RESULTS" | while IFS='|' read -r id st detail; do
		[ -z "$id" ] || log "  $id  $st  $detail"
	done
	if in_container; then
		log "  (container: not real-host evidence)"
	fi
	log "transcript: $TRANSCRIPT"
}

# --- processes and cgroups -----------------------------------------------

# proc_fields PID: PS_STATE, PS_PPID, PS_SESSION and PS_TICKS (fields 3,
# 4, 6 and 22 of /proc/PID/stat, read after the last ')'). 1 when gone.
proc_fields() {
	_ps=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
	_ps=${_ps##*') '}
	# shellcheck disable=SC2086 # Split into the stat fields on purpose.
	set -- $_ps
	PS_STATE=$1 PS_PPID=$2 PS_SESSION=$4 PS_TICKS=${20}
}

# alive PID [TICKS]: PID exists, is not a zombie and, given TICKS, started
# at that time.
alive() {
	proc_fields "$1" || return 1
	[ "$PS_STATE" != Z ] || return 1
	[ -z "${2:-}" ] || [ "$PS_TICKS" = "$2" ]
}

cgroup_of() { sed -n 's/^0:://p' "/proc/$1/cgroup" 2>/dev/null; }

cmdline_of() { tr '\0' ' ' 2>/dev/null <"/proc/$1/cmdline"; }

# nexusd_pid: the one process running "$NEXUSD_BIN serve ...". Operation
# supervisors are also nexusd, but run op-supervise.
nexusd_pid() {
	_np=
	for _d in /proc/[0-9]*; do
		case $(cmdline_of "${_d#/proc/}") in
		"$NEXUSD_BIN serve "*) _np="$_np ${_d#/proc/}" ;;
		esac
	done
	# shellcheck disable=SC2086
	set -- $_np
	[ $# -eq 1 ] && echo "$1"
}

# show_procs PATTERN: log every process whose command line contains
# PATTERN, from /proc: pid, ppid, session, state, cgroup, command line.
show_procs() {
	log "\$ (read /proc/<pid>/stat, cgroup and cmdline of each process matching '$1')"
	_n=0
	for _d in /proc/[0-9]*; do
		_p=${_d#/proc/}
		_c=$(cmdline_of "$_p") || continue
		case $_c in *"$1"*) ;; *) continue ;; esac
		proc_fields "$_p" || continue
		log "  pid $_p ppid $PS_PPID sid $PS_SESSION state $PS_STATE cgroup $(cgroup_of "$_p") : $_c"
		_n=$((_n + 1))
	done
	[ "$_n" -gt 0 ] || log "  (none)"
}

# descendants PID: every descendant of PID, from /proc.
descendants() {
	_root=$1
	for _d in /proc/[0-9]*; do
		_s=$(cat "$_d/stat" 2>/dev/null) || continue
		_s=${_s##*') '}
		echo "${_d#/proc/} ${_s#* }"
	done | awk -v root="$_root" '{ parent[$1] = $2 + 0 }
		END { n = 1; q[1] = root + 0
			for (i = 1; i <= n; i++) for (p in parent) if (parent[p] == q[i]) { q[++n] = p; print p } }'
}

# op_cgroup_pids OP: the processes in nexus-ops/OP and its work group.
op_cgroup_pids() {
	[ -d "$CGROOT/nexus-ops/$1" ] || return 0
	find "$CGROOT/nexus-ops/$1" -name cgroup.procs -exec cat {} + 2>/dev/null
}

# --- nexusd's API (curl; the token goes through stdin, never argv) -------

listen_from_config() {
	sed -n 's/^listen:[[:space:]]*\[[[:space:]]*"\([^"]*\)".*/\1/p' "$NEXUS_CONFIG" 2>/dev/null | head -n 1
}

ncurl() {
	if [ -n "$CONNECT_TO" ]; then
		curl --connect-to "$CONNECT_TO" --cacert "$CACERT" "$@"
	elif [ -n "$CACERT" ]; then
		curl --cacert "$CACERT" "$@"
	else
		curl "$@"
	fi
}

curl_shown() { echo "curl -sS${CONNECT_TO:+ --connect-to $CONNECT_TO}${CACERT:+ --cacert $CACERT}"; }

# setup_api: BASE_URL, and how to trust nexusd's certificate. A plain HTTP
# /healthz that answers 200 means tls.mode off; anything else is HTTPS with
# the generated certificate, which names localhost, so the request goes to
# https://localhost:PORT, connected to the listen address.
setup_api() {
	if [ -n "${NEXUS_URL:-}" ]; then
		BASE_URL=${NEXUS_URL%/} CACERT=${NEXUS_CACERT:-} CONNECT_TO=
	else
		_addr=${NEXUS_ADDR:-$(listen_from_config)}
		[ -n "$_addr" ] || _addr=127.0.0.1:7443
		_port=${_addr##*:} _host=${_addr%:*}
		_end=$(($(date +%s) + 60))
		while :; do
			_code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://$_addr/healthz" 2>/dev/null)
			[ "$_code" = 000 ] || break
			[ "$(date +%s)" -lt "$_end" ] || break
			sleep 1
		done
		if [ "$_code" = 200 ]; then
			BASE_URL=http://$_addr CACERT='' CONNECT_TO=''
		else
			BASE_URL=https://localhost:$_port CACERT=${NEXUS_CACERT:-$NEXUSD_STATE/tls/cert.pem}
			CONNECT_TO=localhost:$_port:$_host:$_port
		fi
	fi
	note "API: $BASE_URL ($(curl_shown))"
}

# api METHOD PATH [BODY]: API_CODE and API_BODY, logged with the token
# shown as <NEXUS_TOKEN>. api_quiet does the same without logging.
api_quiet() {
	_resp=$TMPD/resp _err=$TMPD/err
	rm -f "$_resp" "$_err"
	if [ -n "${3:-}" ]; then
		API_CODE=$(printf 'header = "Authorization: Bearer %s"\n' "${NEXUS_TOKEN:-}" |
			ncurl -sS -m 30 -K - -o "$_resp" -w '%{http_code}' -X "$1" \
				-H 'Content-Type: application/json' --data "$3" "$BASE_URL$2" 2>"$_err")
	else
		API_CODE=$(printf 'header = "Authorization: Bearer %s"\n' "${NEXUS_TOKEN:-}" |
			ncurl -sS -m 30 -K - -o "$_resp" -w '%{http_code}' -X "$1" "$BASE_URL$2" 2>"$_err")
	fi
	API_BODY=$(cat "$_resp" 2>/dev/null)
	API_ERR=$(cat "$_err" 2>/dev/null)
}

api() {
	if [ -n "${3:-}" ]; then
		log "\$ $(curl_shown) -X $1 -H 'Authorization: Bearer <NEXUS_TOKEN>' -H 'Content-Type: application/json' --data '$3' $BASE_URL$2"
	else
		log "\$ $(curl_shown) -X $1 -H 'Authorization: Bearer <NEXUS_TOKEN>' $BASE_URL$2"
	fi
	api_quiet "$@"
	log "HTTP $API_CODE ${API_BODY}${API_ERR:+ (curl: $API_ERR)}"
}

json_str() { printf '%s' "$2" | grep -o "\"$1\":\"[^\"]*\"" | head -n 1 | cut -d'"' -f4; }
json_num() { printf '%s' "$2" | grep -o "\"$1\":[0-9]*" | head -n 1 | cut -d: -f2; }
# An operation's own status is its first string "status"; a problem's
# status is a number.
op_status() { json_str status "$1"; }
op_problem() { printf '%s' "$1" | grep -o 'urn:nexus:problem:[a-z-]*' | head -n 1 | sed 's/.*://'; }

# epoch_of RFC3339: seconds since the epoch of a UTC time nexusd wrote, or
# nothing.
epoch_of() {
	case $1 in
	*Z) date -u -d "$(echo "$1" | sed -e 's/T/ /' -e 's/\..*//' -e 's/Z$//')" +%s 2>/dev/null ;;
	esac
}

wait_healthy() { # TIMEOUT [NOT_PID]
	_wend=$(($(date +%s) + $1))
	while :; do
		NEXUSD_PID=$(nexusd_pid)
		if [ -n "$NEXUSD_PID" ] && [ "$NEXUSD_PID" != "${2:-}" ] &&
			[ "$(ncurl -s -m 2 -o /dev/null -w '%{http_code}' "$BASE_URL/healthz" 2>/dev/null)" = 200 ]; then
			note "nexusd pid $NEXUSD_PID answers $BASE_URL/healthz"
			return 0
		fi
		if [ "$(date +%s)" -ge "$_wend" ]; then
			note "nexusd did not answer /healthz within $1 s (nexusd pid: ${NEXUSD_PID:-none})"
			return 1
		fi
		sleep 0.5
	done
}

# invoke MODE SECONDS TAG: start the test operation; OP is its ID.
invoke() {
	OP=
	api POST "/v1/providers/scripts/scripts/$CATALOG_ID/invoke" \
		"{\"parameters\":{\"mode\":\"$1\",\"seconds\":\"$2\",\"tag\":\"$3\"}}"
	[ "$API_CODE" = 202 ] || return 1
	OP=$(json_str id "$API_BODY")
	[ -n "$OP" ] || return 1
	echo "$OP" >>"$OPS_LIST"
	note "operation $OP: $1 $2 s, tag $3"
}

# wait_op OP TIMEOUT WANT...: poll the operation until its status is one of
# WANT or terminal, logging each new status, then log it in full. OP_STATUS,
# OP_PROBLEM, OP_BODY. 0 when the status is one of WANT.
wait_op() {
	_op=$1 _t=$2
	shift 2
	_end=$(($(date +%s) + _t)) _last=
	while :; do
		api_quiet GET "/v1/operations/$_op"
		OP_STATUS=$(op_status "$API_BODY")
		if [ "$OP_STATUS" != "$_last" ]; then
			note "operation $_op: ${OP_STATUS:-? (HTTP $API_CODE $API_ERR)}"
			_last=$OP_STATUS
		fi
		for _w in "$@"; do [ "$OP_STATUS" != "$_w" ] || break 2; done
		case $OP_STATUS in Succeeded | Failed | Canceled) break ;; esac
		if [ "$(date +%s)" -ge "$_end" ]; then
			note "timed out after $_t s waiting for $*"
			break
		fi
		sleep 1
	done
	api GET "/v1/operations/$_op"
	OP_BODY=$API_BODY OP_STATUS=$(op_status "$API_BODY") OP_PROBLEM=$(op_problem "$API_BODY")
	for _w in "$@"; do [ "$OP_STATUS" != "$_w" ] || return 0; done
	return 1
}

# sup_ident OP: SUP_PID and SUP_TICKS from the operation's
# supervisor.json, waited for up to 30 s.
sup_ident() {
	_f=$NEXUSD_STATE/ops/$1/supervisor.json
	_i=0
	while [ ! -s "$_f" ] && [ "$_i" -lt 60 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	run cat "$_f" || return 1
	SUP_PID=$(json_num pid "$RUN_OUT") SUP_TICKS=$(json_num startTicks "$RUN_OUT")
	[ -n "$SUP_PID" ] && [ -n "$SUP_TICKS" ]
}

# wait_file FILE TIMEOUT
wait_file() {
	_i=0
	while [ ! -s "$1" ] && [ "$_i" -lt "$(($2 * 2))" ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	[ -s "$1" ]
}

# op_log_has_outcome_unknown OP: the operation log mentions outcome-unknown.
op_log_has_outcome_unknown() {
	api GET "/v1/operations/$1/logs"
	[ "$API_CODE" = 200 ] || return 2
	printf '%s' "$API_BODY" | grep -q 'outcome-unknown'
}

# --- the test operation and its catalog entry ----------------------------

write_op_script() {
	mkdir -p "$LIBEXEC" || return 1
	chown root:root "$LIBEXEC" && chmod 0755 "$LIBEXEC" || return 1
	cat >"$OP_SCRIPT.new" <<'EOF'
#!/bin/sh
# Test operation of test/live/alpine-check.sh (nexus WI 3.5). It is
# harmless: it sleeps, and writes only under /var/lib/nexus-alpine-check/run.
#   mark SECONDS TAG    sleep SECONDS, then write the marker
#   escape 0 TAG        start a setsid grandchild, then wait to be canceled
set -eu
mode=$1 seconds=$2 tag=$3
d=/var/lib/nexus-alpine-check/run/$tag
mkdir -p "$d"
echo "$$" >"$d/child"
date +%s >"$d/started"
case $mode in
mark)
	echo "alpine-check op: pid $$ sleeping ${seconds}s"
	sleep "$seconds"
	date +%s >"$d/ended"
	echo done >"$d/marker"
	echo "alpine-check op: marked"
	;;
escape)
	setsid sleep 1000 &
	echo "$!" >"$d/grandchild"
	echo "alpine-check op: pid $$, setsid grandchild $!"
	while :; do sleep 1; done
	;;
esac
EOF
	chown root:root "$OP_SCRIPT.new" && chmod 0755 "$OP_SCRIPT.new" && mv -f "$OP_SCRIPT.new" "$OP_SCRIPT"
}

catalog_entry() {
	cat <<EOF
      - id: $CATALOG_ID
        description: Test op of alpine-check.sh, sleeps and writes only under $STATE_DIR
        path: $OP_SCRIPT
        runAs: root
        workingDirectory: $LIBEXEC
        timeout: 3h
        concurrency: 4
        terminationGrace: 5s
        parameters:
          - {name: mode, type: string, enum: [mark, escape]}
          - {name: seconds, type: integer}
          - {name: tag, type: string, pattern: '[a-z0-9][a-z0-9-]{0,40}'}
EOF
}

# edit_config IN OUT: OUT is IN with the scripts provider enabled and the
# catalog entry added. Exit status 3 when the scripts section has a shape
# this does not recognize; nothing else in the file changes.
#   - the default block: "  scripts:" with "    enabled: true|false" and
#     "    catalog: []" (comments and blank lines kept in place);
#   - the flow form "  scripts: {enabled: false}" (optionally with
#     "catalog: []");
#   - no scripts key under a providers: section indented by 2;
#   - no providers: section at all.
edit_config() {
	awk -v entry="$(catalog_entry)" '
	function block() { print "  scripts:"; print "    enabled: true"; print "    catalog:"; print entry }
	function close_scripts() {
		if (!en) print "    enabled: true"
		if (!cat) { print "    catalog:"; print entry }
		insc = 0; done = 1
	}
	{
		line = $0
		if (insc) {
			if (line ~ /^[ \t]*$/ || line ~ /^[ \t]*#/) { print; next }
			if (line ~ /^    enabled:[ \t]*(true|false)[ \t]*(#.*)?$/) { print "    enabled: true"; en = 1; next }
			if (line ~ /^    catalog:[ \t]*\[[ \t]*\][ \t]*(#.*)?$/) { print "    catalog:"; print entry; cat = 1; next }
			if (line ~ /^    /) { bad = 1; exit }
			close_scripts()
		}
		if (line ~ /^[^ \t#]/) {
			if (inprov && !done && sib2) { block(); done = 1 }
			inprov = (line ~ /^providers:[ \t]*(#.*)?$/)
			if (inprov) sawprov = 1
			print; next
		}
		if (inprov && !done) {
			if (line ~ /^  scripts:[ \t]*(#.*)?$/) { print; insc = 1; next }
			if (line ~ /^  scripts:[ \t]*\{[ \t]*enabled:[ \t]*(true|false)[ \t]*(,[ \t]*catalog:[ \t]*\[[ \t]*\][ \t]*)?\}[ \t]*(#.*)?$/) {
				block(); done = 1; next
			}
			if (line ~ /^  scripts:/) { bad = 1; exit }
			if (line ~ /^  [A-Za-z]/) sib2 = 1
		}
		print
	}
	END {
		if (bad) exit 3
		if (insc) close_scripts()
		if (!done && inprov && sib2) { block(); done = 1 }
		if (!done && !sawprov) { print "providers:"; block(); done = 1 }
		if (!done) exit 3
	}' "$1" >"$2"
}

install_catalog() {
	if grep -q "^[[:space:]]*- id: ${CATALOG_ID}[[:space:]]*\$" "$NEXUS_CONFIG"; then
		note "$NEXUS_CONFIG already has the catalog entry $CATALOG_ID: using it, not editing the file"
	else
		[ "$CATALOG_READY" = 0 ] || refuse "--catalog-ready, but $NEXUS_CONFIG has no entry '- id: $CATALOG_ID'"
		note "adding the catalog entry $CATALOG_ID to $NEXUS_CONFIG (backup: $CONFIG_BAK)"
		run cp -p "$NEXUS_CONFIG" "$CONFIG_BAK" || refuse "could not back up $NEXUS_CONFIG"
		_new=$(dirname "$NEXUS_CONFIG")/.nexusd.yaml.alpine-check
		(umask 077 && edit_config "$CONFIG_BAK" "$_new")
		_rc=$?
		if [ "$_rc" -ne 0 ]; then
			rm -f "$_new"
			log "The scripts section of $NEXUS_CONFIG has a shape this script does not edit (a"
			log "non-empty catalog, or another layout). Add this entry to providers.scripts.catalog"
			log "yourself, set providers.scripts.enabled: true, check with nexusd validate-config,"
			log "run rc-service nexusd reload, and re-run with --catalog-ready. Remove it afterwards."
			catalog_entry | tee -a "$TRANSCRIPT"
			refuse "$NEXUS_CONFIG was not changed"
		fi
		chown root:root "$_new" && chmod 0600 "$_new"
		run diff -u "$CONFIG_BAK" "$_new"
		if ! run "$NEXUSD_BIN" validate-config --config "$_new"; then
			rm -f "$_new"
			refuse "the edited configuration does not validate; $NEXUS_CONFIG was not changed"
		fi
		mv -f "$_new" "$NEXUS_CONFIG" || refuse "could not install the edited configuration"
		run rc-service nexusd reload || refuse "rc-service nexusd reload failed"
	fi
	_end=$(($(date +%s) + 30))
	while :; do
		api_quiet GET "/v1/providers/scripts/scripts/$CATALOG_ID"
		[ "$API_CODE" != 200 ] || break
		[ "$(date +%s)" -lt "$_end" ] || break
		sleep 1
	done
	api GET "/v1/providers/scripts/scripts/$CATALOG_ID"
	case $API_CODE in
	200) ;;
	401 | 403) refuse "NEXUS_TOKEN was refused ($API_CODE): it needs scopes scripts:read and scripts:run" ;;
	*) refuse "the catalog entry $CATALOG_ID is not served (HTTP $API_CODE); see /var/log/messages" ;;
	esac
}

restore_config() {
	[ -f "$CONFIG_BAK" ] || return 0
	if cmp -s "$CONFIG_BAK" "$NEXUS_CONFIG"; then
		note "$NEXUS_CONFIG already equals the backup"
	else
		note "restoring $NEXUS_CONFIG from $CONFIG_BAK"
		_tmp=$(dirname "$NEXUS_CONFIG")/.nexusd.yaml.alpine-check
		cp -p "$CONFIG_BAK" "$_tmp" && mv -f "$_tmp" "$NEXUS_CONFIG"
		run sha256sum "$CONFIG_BAK" "$NEXUS_CONFIG"
		if rc-service nexusd status >/dev/null 2>&1; then
			run rc-service nexusd reload
			if [ -n "$BASE_URL" ]; then
				_i=0
				# 000 is a nexusd that is not listening yet.
				while [ "$_i" -lt 30 ]; do
					api_quiet GET "/v1/providers/scripts/scripts/$CATALOG_ID"
					case $API_CODE in 200 | 000) ;; *) break ;; esac
					sleep 1
					_i=$((_i + 1))
				done
				api GET "/v1/providers/scripts/scripts/$CATALOG_ID"
			fi
		fi
	fi
	cmp -s "$CONFIG_BAK" "$NEXUS_CONFIG" && rm -f "$CONFIG_BAK"
}

# --- /etc/conf.d/nexusd and rc-cgroup.sh ---------------------------------

set_confd_yes() {
	if [ ! -f "$CONFD_BAK" ] && [ ! -f "$CONFD_ABSENT" ]; then
		if [ -f "$CONFD" ]; then
			run cp -p "$CONFD" "$CONFD_BAK" || return 1
		else
			: >"$CONFD_ABSENT"
			note "$CONFD does not exist; it will be removed again afterwards"
		fi
	fi
	{
		[ ! -f "$CONFD_BAK" ] || grep -v '^[[:space:]]*rc_cgroup_cleanup=' "$CONFD_BAK"
		echo 'rc_cgroup_cleanup="YES"'
	} >"$CONFD.alpine-check" && chmod 0644 "$CONFD.alpine-check" && mv -f "$CONFD.alpine-check" "$CONFD"
	run cat "$CONFD"
}

restore_confd() {
	if [ -f "$CONFD_BAK" ]; then
		note "restoring $CONFD"
		cp -p "$CONFD_BAK" "$CONFD.alpine-check" && mv -f "$CONFD.alpine-check" "$CONFD" && rm -f "$CONFD_BAK"
		run cat "$CONFD"
	elif [ -f "$CONFD_ABSENT" ]; then
		note "removing $CONFD, which did not exist before"
		rm -f "$CONFD" "$CONFD_ABSENT"
	fi
}

# issue910: present, fixed or unknown, for the installed rc-cgroup.sh.
issue910() {
	if [ ! -r "$RC_CGROUP_SH" ]; then
		echo unknown
	elif grep -qF "$RC910_BUGGY" "$RC_CGROUP_SH"; then
		echo present
	elif grep -qF "$RC910_FIXED" "$RC_CGROUP_SH"; then
		echo fixed
	else
		echo unknown
	fi
}

# apply_910_fix: upstream 1cf3944's paths in cgroup_running(), the change
# test/integration TestCgroupCleanupDoesNotKillSupervisor applies.
apply_910_fix() {
	[ -f "$RC_CGROUP_BAK" ] || run cp -p "$RC_CGROUP_SH" "$RC_CGROUP_BAK" || return 1
	# shellcheck disable=SC2016 # ${RC_SVCNAME} is text in rc-cgroup.sh.
	sed -e '/^cgroup_running()/,/^}/ s#"/sys/fs/cgroup/unified/${RC_SVCNAME}" \]#"/sys/fs/cgroup/unified/openrc.${RC_SVCNAME}" ]#' \
		-e '/^cgroup_running()/,/^}/ s#"/sys/fs/cgroup/${RC_SVCNAME}" \]#"/sys/fs/cgroup/openrc.${RC_SVCNAME}" ]#' \
		"$RC_CGROUP_BAK" >"$RC_CGROUP_SH.alpine-check" || return 1
	chmod 0644 "$RC_CGROUP_SH.alpine-check" && mv -f "$RC_CGROUP_SH.alpine-check" "$RC_CGROUP_SH" || return 1
	run diff -u "$RC_CGROUP_BAK" "$RC_CGROUP_SH"
	[ "$(issue910)" = fixed ]
}

restore_910_fix() {
	[ -f "$RC_CGROUP_BAK" ] || return 0
	note "restoring $RC_CGROUP_SH"
	cp -p "$RC_CGROUP_BAK" "$RC_CGROUP_SH.alpine-check" && mv -f "$RC_CGROUP_SH.alpine-check" "$RC_CGROUP_SH" && rm -f "$RC_CGROUP_BAK"
	run grep -n 'sys/fs/cgroup/.*RC_SVCNAME' "$RC_CGROUP_SH"
}

# place_canary CGROUP: a sleep moved into CGROUP (nexusd's), started through
# a second shell so that init, not this script, is its parent. CANARY and
# CANARY_TICKS.
place_canary() {
	_pf=$TMPD/canary.pid
	rm -f "$_pf"
	run sh -c "sh -c 'echo \$\$ > $CGROOT$1/cgroup.procs; echo \$\$ > $_pf; exec sleep 1000' >/dev/null 2>&1 &"
	wait_file "$_pf" 5 || return 1
	CANARY=$(cat "$_pf")
	_i=0
	while [ "$(cgroup_of "$CANARY")" != "$1" ] && [ "$_i" -lt 10 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	proc_fields "$CANARY" || return 1
	CANARY_TICKS=$PS_TICKS
	echo "$CANARY $CANARY_TICKS" >>"$CANARY_LIST"
	run cat "/proc/$CANARY/cgroup"
	[ "$(cgroup_of "$CANARY")" = "$1" ]
}

kill_canaries() {
	[ -f "$CANARY_LIST" ] || return 0
	while read -r _cp _ct; do
		if alive "$_cp" "$_ct"; then
			run kill -9 "$_cp"
		fi
	done <"$CANARY_LIST"
	rm -f "$CANARY_LIST"
}

# remove_service_cgroup CGROUP: rmdir nexusd's OpenRC group once it is
# empty after a stop whose cleanup ran. On some kernels (seen on 6.8 in the
# T1 harness) a group OpenRC wrote cgroup.kill to SIGKILLs every later
# process cloned into or out of it, which the next nexusd would inherit.
# OpenRC creates a fresh group at the next start.
remove_service_cgroup() {
	_g=$CGROOT$1
	[ -d "$_g" ] || { note "$_g is already gone"; return 0; }
	_i=0
	while ! grep -q '^populated 0' "$_g/cgroup.events" 2>/dev/null && [ "$_i" -lt 20 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	run cat "$_g/cgroup.events"
	run rmdir "$_g"
}

# --- preconditions, facts, plan ------------------------------------------

pre_alpine() {
	[ "$(id -u)" = 0 ] || refuse "run this as root (Alpine has no sudo by default: su -, then run it)"
	[ -r /etc/alpine-release ] || refuse "/etc/alpine-release is missing: this is not Alpine (use --ubuntu on Ubuntu)"
	{ [ -e /run/openrc ] && [ -x /sbin/openrc-run ] && [ -x /sbin/rc-service ]; } ||
		refuse "OpenRC is not the running init system (/run/openrc, /sbin/openrc-run, /sbin/rc-service)"
	[ -x "$NEXUSD_BIN" ] || refuse "$NEXUSD_BIN is missing: install the release tarball with its install.sh first"
	[ -x /etc/init.d/nexusd ] || refuse "/etc/init.d/nexusd is missing: the release's install.sh installs it"
	[ -f "$NEXUS_CONFIG" ] || refuse "$NEXUS_CONFIG is missing"
	for _c in curl setsid awk sed grep find cmp diff sha256sum mktemp tee; do
		command -v "$_c" >/dev/null 2>&1 || refuse "$_c is not installed (on Alpine: apk add curl; the others are BusyBox applets)"
	done
}

# pre_disruptive: what the steps need beyond the facts.
pre_disruptive() {
	[ -n "${NEXUS_TOKEN:-}" ] || refuse "NEXUS_TOKEN is not set. Create a token: nexusd token create --name alpine-check --scope scripts:read --scope scripts:run --expires 1d"
	[ -f "$CGROOT/cgroup.controllers" ] ||
		refuse "/sys/fs/cgroup is not cgroup v2 (unified): TC-51 expects cgroup2fs, and on another layout supervisors stay in nexusd's cgroup (CON-10)"
	if ! rc-service nexusd status >/dev/null 2>&1; then
		refuse "nexusd is not started: rc-service nexusd start, then run this again"
	fi
	WAS_STARTED=1
	setup_api
	wait_healthy 30 || refuse "nexusd does not answer /healthz at $BASE_URL (set NEXUS_ADDR, or NEXUS_URL and NEXUS_CACERT)"
	api GET /v1/capabilities
	case $API_CODE in
	200) ;;
	401) refuse "NEXUS_TOKEN was refused (401)" ;;
	*) refuse "GET /v1/capabilities answered $API_CODE" ;;
	esac
}

nginx_digest() {
	[ -d /etc/nginx ] || { echo absent; return; }
	find /etc/nginx -type f -exec sha256sum {} + 2>/dev/null | sort | sha256sum | cut -d' ' -f1
}

facts() {
	note "== Step 1: host facts (TC-51 step 1)"
	run uname -m
	_arch=$RUN_OUT
	run cat /etc/alpine-release
	_rel=$RUN_OUT
	run uname -r
	run stat -fc '%t %T' "$CGROOT"
	_cgt=$RUN_OUT
	run grep ' /sys/fs/cgroup' /proc/mounts
	run cat "$BOOT_ID"
	run openrc --version
	run "$NEXUSD_BIN" -version
	run grep -n -E '^[[:space:]]*rc_cgroup_(mode|cleanup)=' /etc/rc.conf
	if [ -f "$CONFD" ]; then
		run cat "$CONFD"
	else
		log "(no $CONFD)"
	fi
	run sh -c '[ ! -r /etc/rc.conf ] || . /etc/rc.conf; [ ! -r /etc/conf.d/nexusd ] || . /etc/conf.d/nexusd; echo "rc_cgroup_cleanup=${rc_cgroup_cleanup:-NO}"'
	_rcc=${RUN_OUT#rc_cgroup_cleanup=}
	run grep -n 'sys/fs/cgroup/.*RC_SVCNAME' "$RC_CGROUP_SH"
	ISSUE910=$(issue910)

	if [ "$_arch" = x86_64 ]; then
		result 1-arch PASS "uname -m $_arch"
	else
		result 1-arch FAIL "uname -m $_arch, want x86_64"
	fi
	_maj=${_rel%%.*} _rest=${_rel#*.}
	_min=${_rest%%.*}
	case $_maj$_min in
	*[!0-9]*) result 1-release FAIL "Alpine release '$_rel' is not MAJOR.MINOR" ;;
	*)
		if [ "$_maj" -gt 3 ] || { [ "$_maj" -eq 3 ] && [ "$_min" -ge 21 ]; }; then
			result 1-release PASS "Alpine $_rel (3.21 or later)"
		else
			result 1-release FAIL "Alpine $_rel, want 3.21 or later"
		fi
		;;
	esac
	# BusyBox stat prints UNKNOWN for %T on cgroup2; its magic number,
	# 0x63677270, is CGROUP2_SUPER_MAGIC in linux/magic.h.
	case $_cgt in
	"63677270 "* | *cgroup2fs*) result 1-cgroup PASS "cgroup2 at $CGROOT (stat -f: $_cgt)" ;;
	*) result 1-cgroup FAIL "stat -f of $CGROOT: $_cgt, want cgroup2fs (magic 63677270)" ;;
	esac
	result 1-rc_cgroup_cleanup INFO "effective rc_cgroup_cleanup for nexusd: $_rcc"
	result 1-openrc-910 INFO "OpenRC issue #910 in $RC_CGROUP_SH: $ISSUE910"
	if in_container; then
		result 1-host FAIL "running in a container: not a real host"
	fi
}

plan() {
	log ""
	log "== Plan ($MODE). With --yes the script will:"
	case $MODE in
	main | reboot1)
		log "  - create $STATE_DIR (0700) and $OP_SCRIPT (root:root 0755), the test operation:"
		log "    it only sleeps and writes under $STATE_DIR/run;"
		log "  - add the catalog entry $CATALOG_ID to $NEXUS_CONFIG (backup in $STATE_DIR,"
		log "    checked with nexusd validate-config), then rc-service nexusd reload;"
		;;
	esac
	case $MODE in
	main)
		case " $STEPS " in *" 2 "*)
			log "  - step 2: start a 20 s test operation, then rc-service nexusd restart;" ;;
		esac
		case " $STEPS " in *" 3 "*)
			log "  - step 3: write rc_cgroup_cleanup=\"YES\" to $CONFD (backup kept), rc-service nexusd"
			log "    restart, put a canary sleep in nexusd's cgroup, start a 45 s test operation,"
			log "    rc-service nexusd stop, check the canary then the supervisor, put $CONFD back,"
			log "    rc-service nexusd start;"
			if [ "$WITH_FIX" = 1 ]; then
				log "    --with-910-fix: patch cgroup_running() in $RC_CGROUP_SH for this step if it has"
				log "    OpenRC issue #910, then put the original back;"
			fi
			;;
		esac
		case " $STEPS " in *" 4 "*)
			log "  - step 4: start a 1 h test operation, rc-service nexusd stop, kill -9 that"
			log "    operation's supervisor and its processes (only those), rc-service nexusd start;" ;;
		esac
		case " $STEPS " in *" 6 "*)
			log "  - step 6: start a test operation that leaves a setsid grandchild, then cancel it;" ;;
		esac
		log "  - at the end: cancel any test operation still running, kill the canary, put"
		log "    $NEXUS_CONFIG back byte for byte and reload nexusd, remove $OP_SCRIPT and $STATE_DIR."
		;;
	reboot1)
		log "  - start a 2 h test operation and record boot_id, then print how to reboot."
		log "    It never reboots: you reboot the host yourself, then run --reboot-phase2."
		;;
	reboot2)
		log "  - read boot_id, start nexusd if it did not start at boot, check the phase 1"
		log "    operation ended outcome-unknown, then clean up as a full run does."
		;;
	cleanup)
		log "  - start nexusd if it is stopped, cancel this script's test operations still running,"
		log "    kill its canary, put back $NEXUS_CONFIG, $CONFD and $RC_CGROUP_SH from the"
		log "    backups in $STATE_DIR, and remove $OP_SCRIPT and $STATE_DIR."
		;;
	esac
	log "  It never edits nginx's configuration or touches the nginx service, and never reboots."
}

# recover: undo what an interrupted earlier run left, before a new one.
recover() {
	if [ -f "$REBOOT_STATE" ]; then
		refuse "a reboot check is pending ($REBOOT_STATE): run --yes --reboot-phase2 after the reboot, or --yes --cleanup to abandon it"
	fi
	if [ -d "$STATE_DIR" ] || [ -d "$LIBEXEC" ]; then
		note "an earlier run did not finish: undoing what it left first"
		restore_all
		CLEANED=0
	fi
}

prepare() {
	{ mkdir -p "$STATE_DIR" && chmod 0700 "$STATE_DIR"; } || refuse "cannot create $STATE_DIR"
	CHANGED=1
	note "installing the test operation $OP_SCRIPT"
	write_op_script || refuse "cannot install $OP_SCRIPT"
	run ls -l "$OP_SCRIPT"
	install_catalog
}

# restore_all: undo everything this script changes. Safe to run twice.
restore_all() {
	note "== Cleanup"
	kill_canaries
	restore_confd
	restore_910_fix
	# An interrupted step can leave nexusd stopped; it must answer to cancel
	# the test operations.
	if [ "$WAS_STARTED" = 1 ] && ! rc-service nexusd status >/dev/null 2>&1; then
		run rc-service nexusd start
		[ -z "$BASE_URL" ] || wait_healthy 30
	fi
	if [ -s "$OPS_LIST" ]; then
		if [ -n "${NEXUS_TOKEN:-}" ] && [ -n "$BASE_URL" ]; then
			while read -r _o; do
				api_quiet GET "/v1/operations/$_o"
				case $(op_status "$API_BODY") in
				Queued | Running)
					api POST "/v1/operations/$_o/cancel"
					wait_op "$_o" 60 Canceled
					;;
				esac
			done <"$OPS_LIST"
		else
			log "cannot cancel the test operations without NEXUS_TOKEN and a running nexusd: $(tr '\n' ' ' <"$OPS_LIST")"
		fi
	fi
	for _g in "$STATE_DIR"/run/*/grandchild; do
		[ -f "$_g" ] || continue
		_p=$(cat "$_g")
		case $(cmdline_of "$_p") in
		"sleep 1000 ") alive "$_p" && run kill -9 "$_p" ;;
		esac
	done
	restore_config
	if [ -f "$CONFIG_BAK" ] || [ -f "$CONFD_BAK" ] || [ -f "$RC_CGROUP_BAK" ]; then
		log "a backup could not be restored; $STATE_DIR is kept: $(ls "$STATE_DIR")"
	else
		run rm -rf "$LIBEXEC" "$STATE_DIR"
	fi
	CLEANED=1
}

on_exit() {
	_rc=$?
	trap - EXIT
	if [ "$CHANGED" = 1 ] && [ "$KEEP" = 0 ] && [ "$CLEANED" = 0 ]; then
		note "stopped early (exit $_rc): undoing this run's changes"
		restore_all
	fi
	rm -rf "$TMPD"
	exit "$_rc"
}

# --- TC-51 steps -----------------------------------------------------------

step2() {
	log ""
	note "== Step 2: an operation survives rc-service nexusd restart (TC-51 step 2, as TC-39)"
	_tag=s2-$RUN_ID
	if ! invoke mark 20 "$_tag"; then
		result 2 FAIL "the test operation did not start (HTTP $API_CODE)"
		return
	fi
	_op=$OP
	if ! wait_op "$_op" 30 Running || ! sup_ident "$_op"; then
		result 2 FAIL "operation $_op is $OP_STATUS, with no supervisor to watch"
		return
	fi
	proc_fields "$SUP_PID"
	_bt=$PS_TICKS _bs=$PS_SESSION _bp=$PS_PPID
	_old=$(nexusd_pid)
	run cat "/proc/$SUP_PID/stat"
	run cat "/proc/$SUP_PID/cgroup"
	run cat "/proc/$_old/cgroup"
	note "supervisor $SUP_PID before: session $_bs ppid $_bp startTicks $_bt (supervisor.json: $SUP_TICKS) cgroup $(cgroup_of "$SUP_PID"); nexusd $_old"
	run rc-service nexusd restart
	wait_healthy 30 "$_old"
	if alive "$SUP_PID"; then
		run cat "/proc/$SUP_PID/stat"
		note "supervisor $SUP_PID after the restart (nexusd $_old -> $NEXUSD_PID): session $PS_SESSION ppid $PS_PPID startTicks $PS_TICKS cgroup $(cgroup_of "$SUP_PID")"
		if [ "$PS_TICKS" = "$SUP_TICKS" ] && [ "$PS_TICKS" = "$_bt" ]; then
			result 2a PASS "supervisor $SUP_PID alive across the restart, startTicks $PS_TICKS unchanged"
		else
			result 2a FAIL "pid $SUP_PID has startTicks $PS_TICKS, was $_bt (supervisor.json $SUP_TICKS): not the same process"
		fi
		if [ "$PS_SESSION" = "$SUP_PID" ]; then
			result 2b PASS "session $PS_SESSION is the supervisor's own pid"
		else
			result 2b FAIL "session $PS_SESSION, want $SUP_PID"
		fi
		if [ "$PS_PPID" != "$_old" ]; then
			result 2c PASS "ppid $PS_PPID, not the old nexusd $_old"
		else
			result 2c FAIL "ppid is still the old nexusd $_old"
		fi
	else
		result 2a FAIL "supervisor $SUP_PID died with the restart"
		result 2b NOT-RUN "the supervisor is gone"
		result 2c NOT-RUN "the supervisor is gone"
	fi
	wait_op "$_op" 90 Succeeded
	_d=$STATE_DIR/run/$_tag
	run ls -l "$_d"
	_dur=
	if [ -f "$_d/started" ] && [ -f "$_d/ended" ]; then
		_dur=$(($(cat "$_d/ended") - $(cat "$_d/started")))
	fi
	_s=$(epoch_of "$(json_str started "$OP_BODY")") _e=$(epoch_of "$(json_str ended "$OP_BODY")")
	_ndur=
	[ -z "$_s" ] || [ -z "$_e" ] || _ndur=$((_e - _s))
	if [ "$OP_STATUS" = Succeeded ] && [ -f "$_d/marker" ] && [ -n "$_dur" ] && [ "$_dur" -ge 20 ] &&
		{ [ -z "$_ndur" ] || [ "$_ndur" -ge 20 ]; }; then
		result 2d PASS "operation $_op Succeeded, marker written, ran ${_dur} s by the script's clock, ${_ndur:-?} s by started/ended"
	else
		result 2d FAIL "operation $_op $OP_STATUS${OP_PROBLEM:+ $OP_PROBLEM}, marker $([ -f "$_d/marker" ] && echo present || echo missing), ${_dur:-?} s / ${_ndur:-?} s"
	fi
	op_log_has_outcome_unknown "$_op"
	case $? in
	0) result 2e FAIL "the operation log of $_op mentions outcome-unknown" ;;
	1) result 2e PASS "the operation log of $_op has no outcome-unknown" ;;
	*) result 2e FAIL "could not read the operation log of $_op (HTTP $API_CODE)" ;;
	esac
}

step3() {
	log ""
	note "== Step 3: stop with rc_cgroup_cleanup=\"YES\" (TC-51 step 3, ASM-14)"
	run grep -n 'sys/fs/cgroup/.*RC_SVCNAME' "$RC_CGROUP_SH"
	if [ "$ISSUE910" = present ]; then
		if [ "$WITH_FIX" = 1 ]; then
			note "applying upstream 1cf3944's cgroup_running() for this step (--with-910-fix)"
			if ! apply_910_fix; then
				restore_910_fix
				result 3a FAIL "could not apply the 1cf3944 change to $RC_CGROUP_SH"
				result 3b NOT-RUN "not checked"
				return
			fi
			ISSUE910="fixed for this step (--with-910-fix)"
		else
			note "$RC_CGROUP_SH has OpenRC issue #910: rc_cgroup_cleanup=YES is expected to do nothing here; the canary will show it (re-run with --with-910-fix to apply upstream 1cf3944 for the step)"
		fi
	fi
	set_confd_yes || { result 3a FAIL "could not write $CONFD"; return; }
	run rc-service nexusd restart
	if ! wait_healthy 30; then
		result 3a FAIL "nexusd did not come back after the restart"
		restore_confd
		restore_910_fix
		return
	fi
	_nx=$NEXUSD_PID
	_cg=$(cgroup_of "$_nx")
	run cat "/proc/$_nx/cgroup"
	if [ -z "$_cg" ] || [ "$_cg" = / ]; then
		result 3a VACUOUS "nexusd $_nx is in cgroup '${_cg:-?}', not an OpenRC group of its own: the cleanup has nothing to act on"
		result 3b NOT-RUN "not checked: survival would be vacuous"
		restore_confd
		restore_910_fix
		return
	fi
	if ! place_canary "$_cg"; then
		result 3a FAIL "the canary is not in nexusd's cgroup $_cg"
		result 3b NOT-RUN "not checked"
		kill_canaries
		restore_confd
		restore_910_fix
		return
	fi
	note "canary $CANARY in nexusd $_nx's cgroup $_cg"
	_tag=s3-$RUN_ID
	if ! invoke mark 45 "$_tag" || ! wait_op "$OP" 30 Running || ! sup_ident "$OP"; then
		result 3a FAIL "the test operation did not start"
		kill_canaries
		restore_confd
		restore_910_fix
		return
	fi
	_op=$OP
	_supcg=$(cgroup_of "$SUP_PID")
	run cat "/proc/$SUP_PID/cgroup"
	note "supervisor $SUP_PID startTicks $SUP_TICKS cgroup $_supcg; nexusd cgroup $_cg"
	run rc-service nexusd stop
	_i=0
	while alive "$CANARY" "$CANARY_TICKS" && [ "$_i" -lt 10 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	show_procs 'sleep 1000'
	if alive "$CANARY" "$CANARY_TICKS"; then
		run cat "/proc/$CANARY/stat"
		result 3a VACUOUS "rc_cgroup_cleanup not in effect; the survival check would prove nothing (canary $CANARY alive 5 s after the stop; OpenRC issue #910: $ISSUE910)"
		result 3b NOT-RUN "the supervisor was not checked, because the canary survived"
		kill_canaries
	else
		result 3a PASS "canary $CANARY gone within $((_i / 2)).$((_i % 2 * 5)) s of the stop: cleanup was in effect"
		if alive "$SUP_PID" "$SUP_TICKS"; then
			run cat "/proc/$SUP_PID/stat"
			run cat "/proc/$SUP_PID/cgroup"
			_now=$(cgroup_of "$SUP_PID")
			if [ "$_supcg" = "/nexus-ops/$_op" ] && [ "$_now" = "/nexus-ops/$_op" ] && [ "$_supcg" != "$_cg" ]; then
				result 3b PASS "supervisor $SUP_PID survived, startTicks $SUP_TICKS unchanged, cgroup $_now"
			else
				result 3b FAIL "supervisor $SUP_PID survived but its cgroup is $_supcg (now $_now), want /nexus-ops/$_op"
			fi
		else
			result 3b FAIL "supervisor $SUP_PID (startTicks $SUP_TICKS) was killed by the stop"
		fi
		remove_service_cgroup "$_cg"
	fi
	restore_confd
	restore_910_fix
	run rc-service nexusd start
	wait_healthy 30
	wait_op "$_op" 120 Succeeded
	if [ "$OP_STATUS" = Succeeded ] && [ -f "$STATE_DIR/run/$_tag/marker" ]; then
		result 3c PASS "operation $_op Succeeded after nexusd started again, marker written"
	else
		result 3c FAIL "operation $_op ended $OP_STATUS${OP_PROBLEM:+ $OP_PROBLEM}"
	fi
}

step4() {
	log ""
	note "== Step 4: a supervisor killed while nexusd is down (TC-51 step 4, as TC-40)"
	_tag=s4-$RUN_ID
	if ! invoke mark 3600 "$_tag" || ! wait_op "$OP" 30 Running || ! sup_ident "$OP"; then
		result 4 FAIL "the test operation did not start"
		return
	fi
	_op=$OP
	run rc-service nexusd stop
	_victims=$({
		echo "$SUP_PID"
		op_cgroup_pids "$_op"
		descendants "$SUP_PID"
	} | sort -un | tr '\n' ' ')
	show_procs "$OP_SCRIPT"
	show_procs 'op-supervise'
	run find "$CGROOT/nexus-ops/$_op" -name cgroup.procs -exec grep -H . {} +
	# shellcheck disable=SC2086
	set -- $_victims
	if [ $# -lt 2 ]; then
		result 4 FAIL "supervisor $SUP_PID has no child: $_victims"
		run rc-service nexusd start
		wait_healthy 30
		return
	fi
	note "killing operation $_op's supervisor $SUP_PID and its processes: $_victims"
	run kill -9 "$@"
	_i=0
	while [ "$_i" -lt 30 ]; do
		_left=
		for _v in "$@"; do alive "$_v" && _left="$_left $_v"; done
		[ -n "$_left" ] || break
		sleep 0.5
		_i=$((_i + 1))
	done
	show_procs "$OP_SCRIPT"
	run ls -l "$NEXUSD_STATE/ops/$_op"
	if [ -n "$_left" ]; then
		result 4 FAIL "still alive after kill -9:$_left"
	elif [ -e "$NEXUSD_STATE/ops/$_op/exit.json" ]; then
		result 4 FAIL "exit.json exists: the supervisor finished before it was killed"
	fi
	run rc-service nexusd start
	wait_healthy 30
	_t0=$(date +%s)
	wait_op "$_op" 60 Failed
	_took=$(($(date +%s) - _t0))
	if [ -z "$_left" ] && [ "$OP_STATUS" = Failed ] && [ "$OP_PROBLEM" = outcome-unknown ]; then
		result 4 PASS "operation $_op Failed outcome-unknown, $_took s after nexusd started"
	elif [ -z "$_left" ]; then
		result 4 FAIL "operation $_op ended $OP_STATUS${OP_PROBLEM:+ $OP_PROBLEM}, want Failed outcome-unknown"
	fi
}

step6() {
	log ""
	note "== Step 6: cancel leaves no descendant (TC-51 step 6, as TC-41)"
	_tag=s6-$RUN_ID
	if ! invoke escape 0 "$_tag"; then
		result 6a FAIL "the test operation did not start (HTTP $API_CODE)"
		return
	fi
	_op=$OP
	_d=$STATE_DIR/run/$_tag
	if ! wait_file "$_d/grandchild" 30 || ! wait_file "$_d/child" 30; then
		result 6a FAIL "operation $_op wrote no grandchild pid"
		return
	fi
	_gc=$(cat "$_d/grandchild") _ch=$(cat "$_d/child")
	_i=0
	while ! { alive "$_gc" && [ "$PS_SESSION" = "$_gc" ]; } && [ "$_i" -lt 20 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	proc_fields "$_gc"
	_gct=$PS_TICKS
	proc_fields "$_ch"
	_cht=$PS_TICKS
	run cat "/proc/$_gc/stat"
	show_procs 'sleep 1000'
	note "child $_ch, setsid grandchild $_gc (session $(proc_fields "$_gc" && echo "$PS_SESSION"), cgroup $(cgroup_of "$_gc"))"
	api POST "/v1/operations/$_op/cancel"
	if [ "$API_CODE" != 200 ]; then
		result 6a FAIL "cancel answered $API_CODE"
	fi
	wait_op "$_op" 60 Canceled
	if [ "$OP_STATUS" = Canceled ]; then
		result 6a PASS "operation $_op Canceled"
	else
		result 6a FAIL "operation $_op ended $OP_STATUS${OP_PROBLEM:+ $OP_PROBLEM}, want Canceled"
	fi
	_i=0
	while { alive "$_gc" "$_gct" || alive "$_ch" "$_cht" || [ -n "$(op_cgroup_pids "$_op")" ]; } && [ "$_i" -lt 30 ]; do
		sleep 0.5
		_i=$((_i + 1))
	done
	show_procs 'sleep 1000'
	run ps -o pid,ppid,args
	if alive "$_gc" "$_gct" || alive "$_ch" "$_cht"; then
		result 6b FAIL "a descendant survived the cancel: grandchild $_gc $(alive "$_gc" "$_gct" && echo alive), child $_ch $(alive "$_ch" "$_cht" && echo alive)"
	else
		result 6b PASS "neither the setsid grandchild $_gc nor the child $_ch remains"
	fi
	if [ -d "$CGROOT/nexus-ops/$_op" ]; then
		run find "$CGROOT/nexus-ops/$_op" -name cgroup.procs -exec grep -H . {} +
		if [ -z "$(op_cgroup_pids "$_op")" ]; then
			result 6c PASS "$CGROOT/nexus-ops/$_op remains, empty"
		else
			result 6c FAIL "$CGROOT/nexus-ops/$_op still holds: $(op_cgroup_pids "$_op" | tr '\n' ' ')"
		fi
	else
		log "$CGROOT/nexus-ops/$_op does not exist"
		result 6c PASS "$CGROOT/nexus-ops/$_op is gone"
	fi
}

reboot_phase1() {
	log ""
	note "== Step 5, phase 1: a Running operation before a reboot (TC-51 step 5)"
	_tag=s5-$RUN_ID
	if ! invoke mark 7200 "$_tag" || ! wait_op "$OP" 30 Running || ! sup_ident "$OP"; then
		result 5-phase1 FAIL "the test operation did not start"
		return
	fi
	run cat "$BOOT_ID"
	_boot=$RUN_OUT
	run cat /proc/uptime
	run cat "/proc/$SUP_PID/stat"
	run rc-update show default
	case $RUN_OUT in
	*nexusd*) _runlevel=yes ;;
	*) _runlevel=no ;;
	esac
	{
		echo "op=$OP"
		echo "boot=$_boot"
		echo "sup_pid=$SUP_PID"
		echo "sup_ticks=$SUP_TICKS"
		echo "tag=$_tag"
		echo "run_id=$RUN_ID"
		echo "runlevel=$_runlevel"
		echo "config_sha=$CONFIG_SHA_BEFORE"
		echo "nginx_digest=$NGINX_BEFORE"
	} >"$REBOOT_STATE"
	chmod 0600 "$REBOOT_STATE"
	run cat "$REBOOT_STATE"
	KEEP=1
	result 5-phase1 PASS "operation $OP Running (supervisor $SUP_PID), boot_id $_boot recorded"
	log ""
	log "NEXT: reboot this host yourself now, for example with: reboot"
	if [ "$_runlevel" = no ]; then
		log "      nexusd is not in the default runlevel, so it will not start at boot; phase 2 starts it."
	fi
	log "      When it is back, run as root:"
	log "        NEXUS_TOKEN=... sh $0 --yes --reboot-phase2"
	log "      The catalog entry stays in $NEXUS_CONFIG until phase 2 removes it."
	log "      To abandon instead: NEXUS_TOKEN=... sh $0 --yes --cleanup"
}

state_value() { sed -n "s/^$1=//p" "$REBOOT_STATE"; }

reboot_phase2() {
	log ""
	note "== Step 5, phase 2: after the reboot (TC-51 step 5)"
	_op=$(state_value op) _boot=$(state_value boot) _sp=$(state_value sup_pid) _st=$(state_value sup_ticks)
	run cat "$REBOOT_STATE"
	run cat "$BOOT_ID"
	_now=$RUN_OUT
	run cat /proc/uptime
	if [ "$_now" = "$_boot" ] && alive "$_sp" "$_st"; then
		KEEP=1
		refuse "the host has not rebooted since phase 1 (boot_id $_now unchanged, supervisor $_sp still running): reboot it, then run --reboot-phase2 again"
	fi
	if ! rc-service nexusd status >/dev/null 2>&1; then
		note "nexusd is not started (not in a runlevel, or it failed at boot): starting it"
		run rc-service nexusd start
	fi
	WAS_STARTED=1
	setup_api
	wait_healthy 60
	if alive "$_sp" "$_st"; then
		log "supervisor $_sp with startTicks $_st is alive"
	else
		log "supervisor $_sp with startTicks $_st is not running ($(cmdline_of "$_sp" || true))"
	fi
	_t0=$(date +%s)
	wait_op "$_op" 120 Failed
	# What the supervisor recorded, if anything: an exit.json written during
	# the shutdown is what the status comes from.
	run ls -l "$NEXUSD_STATE/ops/$_op"
	[ ! -f "$NEXUSD_STATE/ops/$_op/exit.json" ] || run cat "$NEXUSD_STATE/ops/$_op/exit.json"
	if [ "$OP_STATUS" = Failed ] && [ "$OP_PROBLEM" = outcome-unknown ]; then
		result 5a PASS "operation $_op Failed outcome-unknown, $(($(date +%s) - _t0)) s after nexusd answered"
	else
		result 5a FAIL "operation $_op is $OP_STATUS${OP_PROBLEM:+ $OP_PROBLEM}, want Failed outcome-unknown"
	fi
	if [ "$_now" != "$_boot" ]; then
		result 5b PASS "boot_id before $_boot, after $_now"
	else
		result 5b FAIL "boot_id unchanged ($_now): the host did not reboot (a container restart keeps the host kernel's boot_id)"
	fi
	rm -f "$REBOOT_STATE"
}

# check_restored: after the cleanup, /etc/nexus/nexusd.yaml is as it was
# before the run (phase 1, for a reboot check), and nothing under
# /etc/nginx changed.
check_restored() {
	if [ "$MODE" != reboot1 ]; then
		_sha_after=$(sha256sum <"$NEXUS_CONFIG" | cut -d' ' -f1)
		if [ "$_sha_after" = "$CONFIG_SHA_BEFORE" ]; then
			result config-restored PASS "$NEXUS_CONFIG sha256 $_sha_after, as before the run"
		else
			result config-restored FAIL "$NEXUS_CONFIG sha256 $_sha_after, was ${CONFIG_SHA_BEFORE:-?}"
		fi
	fi
	_nginx_after=$(nginx_digest)
	if [ "$_nginx_after" = "$NGINX_BEFORE" ]; then
		result nginx-untouched PASS "digest of every file under /etc/nginx unchanged ($_nginx_after)"
	else
		result nginx-untouched FAIL "/etc/nginx changed during the run: ${NGINX_BEFORE:-?} -> $_nginx_after"
	fi
}

# --- TC-52 ---------------------------------------------------------------

ubuntu_checks() {
	[ -d /run/systemd/system ] || refuse "systemd is not running: --ubuntu is for the Ubuntu host"
	note "== TC-52: Ubuntu host"
	run uname -m
	run systemctl --version
	run "$NEXUSD_BIN" -version
	run systemd-analyze security nexusd
	_line=$(printf '%s\n' "$RUN_OUT" | grep 'Overall exposure level' | tail -n 1)
	_score=$(printf '%s\n' "$_line" | sed -n 's/.*: *\([0-9][0-9.]*\) *\([A-Z][A-Z]*\).*/\1/p')
	_rating=$(printf '%s\n' "$_line" | sed -n 's/.*: *\([0-9][0-9.]*\) *\([A-Z][A-Z]*\).*/\2/p')
	if [ -n "$_score" ] && awk -v s="$_score" 'BEGIN { exit !(s <= 2.6) }'; then
		result 52-1 PASS "exposure $_score $_rating (2.6 OK or better)"
	else
		result 52-1 FAIL "exposure '${_score:-?} ${_rating:-?}', want 2.6 or lower"
	fi
	run sha256sum "$UNIT"
	_sum=${RUN_OUT%% *}
	if [ "$_sum" = "$UNIT_SHA256_D04583A" ]; then
		result 52-2 PASS "$UNIT sha256 $_sum equals deploy/nexusd.service at d04583a"
	else
		result 52-2 FAIL "$UNIT sha256 ${_sum:-?}, d04583a's is $UNIT_SHA256_D04583A"
	fi
	log "Byte comparison from a checkout on the dev host (no output means identical):"
	log "  git show d04583a:deploy/nexusd.service | ssh <host> 'diff - $UNIT'"
	result 52-3 MANUAL "make test-harness runs on the dev host (Docker): record its exit status in the template"
	if in_container; then
		result 52-host FAIL "running in a container: not a real host"
	fi
}

# --- main ----------------------------------------------------------------

trap on_exit EXIT
trap 'exit 130' INT TERM HUP

header

if [ "$MODE" = ubuntu ]; then
	ubuntu_checks
	summary
	[ "$FAILS" -eq 0 ] || exit 1
	[ "$INCONCL" -eq 0 ] || exit 3
	exit 0
fi

pre_alpine
case $MODE in
main | reboot1) facts ;;
esac
plan
if [ "$YES" != 1 ]; then
	log ""
	refuse "nothing was changed: re-run with --yes to do the above"
fi

case $MODE in
cleanup)
	# Every run starts with nexusd started, so the cleanup leaves it so, and
	# it must answer to cancel the test operations.
	WAS_STARTED=1
	if ! rc-service nexusd status >/dev/null 2>&1; then
		run rc-service nexusd start
	fi
	if [ -n "${NEXUS_TOKEN:-}" ]; then
		setup_api
	fi
	CHANGED=1
	restore_all
	rm -f "$REBOOT_STATE"
	summary
	exit 0
	;;
reboot2)
	[ -f "$REBOOT_STATE" ] || refuse "no phase 1 state ($REBOOT_STATE): run --yes --reboot-phase1 first"
	[ -n "${NEXUS_TOKEN:-}" ] || refuse "NEXUS_TOKEN is not set"
	CHANGED=1
	CONFIG_SHA_BEFORE=$(state_value config_sha)
	NGINX_BEFORE=$(state_value nginx_digest)
	reboot_phase2
	restore_all
	check_restored
	;;
main | reboot1)
	recover
	pre_disruptive
	CONFIG_SHA_BEFORE=$(sha256sum <"$NEXUS_CONFIG" | cut -d' ' -f1)
	NGINX_BEFORE=$(nginx_digest)
	prepare
	if [ "$MODE" = reboot1 ]; then
		reboot_phase1
		check_restored
	else
		for _step in $STEPS; do
			"step$_step"
		done
		restore_all
		check_restored
	fi
	;;
esac

summary
[ "$FAILS" -eq 0 ] || exit 1
[ "$INCONCL" -eq 0 ] || exit 3
exit 0
