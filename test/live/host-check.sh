#!/bin/sh
# SPDX-FileCopyrightText: 2026 Mauktik Gandhi
# SPDX-License-Identifier: Apache-2.0
# Live check of the host provider (system-provider WI 5.4): TC-HP-127 to
# TC-HP-130 on a real Ubuntu/systemd host and on the Alpine/OpenRC VM, each
# running the release tarball. It drives the real `nexus` CLI (every
# `nexus host*` command, with -o json) and reads every field back without
# Nexus, from the host's own tools: hostnamectl or /etc/os-release, uname,
# nproc, free, df, stat -f, lsblk or /sys/block, ip, /sys/class/net,
# systemctl --failed or rc-status --crashed, systemd-detect-virt, vmstat and
# /proc. It is POSIX sh, and BusyBox ash runs it.
#
# Run it on the host itself, as root:
#   sh host-check.sh                          the facts and the plan; changes nothing
#   NEXUS_TOKEN=... sh host-check.sh --yes    the check (read-only on the host)
# or from another machine (a Mac) with the nexus CLI, over SSH:
#   NEXUS_TOKEN=... NEXUS_URL=https://vm.lan:7443 sh host-check.sh --ssh root@vm.lan --yes
#
# Opt-in modes that change the host (each also needs --yes):
#   --with-faults        plant and remove a reboot-required marker (Debian and
#                        Ubuntu) and a failed (systemd) or crashed (OpenRC)
#                        dummy service, and read each back
#   --overhead           NFR-2 at the defaults: nexusd's CPU and RSS with the
#                        provider enabled (history full), then disabled after
#                        a restart; the original configuration is restored
#   --rss-24h start      NFR-2 at the 24 h maximum: a disabled baseline, then
#                        historyDuration 24h at sampleInterval 2s with an
#                        agent-like poll on the host; finish reads it back
#   --rss-24h finish     ... and restores the original configuration
#   --cleanup            undo what an interrupted --with-faults or --overhead run left
#
# The procedure and the transcript template are in
# docs/verification/host-live.md. Every command and its output go to the
# transcript file (--transcript FILE). NEXUS_TOKEN is never written to it.

# Functions run through the EXIT trap, which shellcheck cannot follow
# (SC2317, SC2329). `[ test ] && ok ... || bad ...` is an if-then-else
# here, because ok, bad and unav always return 0 (SC2015).
# shellcheck disable=SC2317,SC2329,SC2015
set -u
# Data from the host, from nexusd and from the state file is never code
# here: no pathname expansion anywhere (set -f; the host commands run in
# their own sh, which globs as usual), no unquoted expansion of such data,
# a line is split into fields by awk (field), and a value reaches awk only
# through -v, never through program text (calc).
set -f
umask 022
# Byte order for every sort and comparison, as the provider sorts.
LC_ALL=C
export LC_ALL

PROG=host-check.sh

# The token must not stay in any environment: not this process's (the
# kernel keeps the environment a process was started with, and every fork
# of it shows that copy in /proc/<pid>/environ), and not a child's. So the
# script runs itself again, once, without NEXUS_TOKEN in its environment,
# and gets the token on file descriptor 3 from a here-document. From then
# on the token is an unexported variable, and it reaches only
# `nexus host add --token-stdin` and the transcript check, both on stdin.
if [ -n "${NEXUS_TOKEN:-}" ] && [ -z "${HOST_CHECK_TOKEN_FD:-}" ]; then
	if [ ! -r "$0" ]; then
		echo "$PROG: REFUSED: run it as a file (sh host-check.sh), not from stdin, so it can drop NEXUS_TOKEN from its environment" >&2
		exit 1
	fi
	unset CHECK_TOKEN
	CHECK_TOKEN=$NEXUS_TOKEN
	unset NEXUS_TOKEN
	HOST_CHECK_TOKEN_FD=3
	export HOST_CHECK_TOKEN_FD
	exec sh "$0" "$@" 3<<EOF
$CHECK_TOKEN
EOF
fi
unset CHECK_TOKEN
CHECK_TOKEN=
if [ "${HOST_CHECK_TOKEN_FD:-}" = 3 ]; then
	unset HOST_CHECK_TOKEN_FD
	IFS= read -r CHECK_TOKEN <&3 || CHECK_TOKEN=
	exec 3<&-
fi

H_STATE=/var/lib/nexus-host-check
CONF=/etc/nexus/nexusd.yaml
NEXUSD=/usr/local/bin/nexusd
SOAK_TOKEN_NAME=host-check-soak
# The ten operations, as CLI nouns (docs/host.md, Operations).
OPS='hostsummary hostidentity hostcpu hostmemory hostfs hostdisk hostnet hostnetconfig hosthealth hostmetric'

usage() {
	cat <<'EOF'
Usage: sh host-check.sh [--yes] [MODE] [OPTIONS]

Without --yes it records the facts (versions, nexusd's PID and the
provider's configuration) and prints the plan. It changes nothing and
exits 2. With --yes alone it runs the read-only check: it changes nothing
on the host either.

Modes (each changes the host, and only with --yes):
  --with-faults         plant and remove a reboot-required marker (Debian
                        and Ubuntu) and a failed (systemd) or crashed
                        (OpenRC) dummy service, each read back through nexus
  --overhead            NFR-2 at the defaults (10s, 1h): CPU over --window
                        seconds and RSS every 60 s, enabled (history full),
                        then disabled after a restart of nexusd; restores
                        the original configuration with a reload
  --rss-24h start       NFR-2 at the 24 h maximum: a disabled baseline after
                        a restart, then historyDuration 24h and
                        sampleInterval 2s, a reload, and an agent-like poll
                        on the host (hostsummary, hosthealth, a 24 h
                        hostmetric every 10 s) that logs nexusd's RSS every
                        60 s. Needs the nexus CLI on the host (--host-nexus)
  --rss-24h finish      read the soak back (the maximum RSS over its last
                        10 minutes against the baseline), stop the poll,
                        revoke its token and restore the configuration
  --cleanup             undo what an interrupted --with-faults or --overhead
                        run left

Options:
  --ssh TARGET          run the host commands over SSH (user@host), and the
                        nexus CLI here. TARGET must be root, or have sudo
                        without a password. Extra ssh options:
                        NEXUS_CHECK_SSH_OPTS
  --nexus-host NAME     use the host NAME already configured in your nexus
                        client (no NEXUS_TOKEN needed)
  --nexus PATH          the nexus CLI here (default: nexus on PATH)
  --host-nexus PATH     the nexus CLI on the host, for --rss-24h (default:
                        nexus on the host's PATH, else /usr/local/bin/nexus)
  --calls N             timed calls per operation for NFR-1 (default 50)
  --window SECONDS      each --overhead CPU window (default 600)
  --baseline SECONDS    the --rss-24h disabled baseline (default 600)
  --hung-mount PATH     a mount point whose server you have made unreachable
                        (the documented manual step): time hostsummary and
                        hostfs and read its reason
  --transcript FILE     where to write the transcript (default:
                        ./host-check-<host>-<UTC time>.txt)
  -h, --help            this text

Environment:
  NEXUS_TOKEN  a token with host:read (not needed with --nexus-host).
               Create it on the host, and revoke it afterwards:
                 nexusd token create --name host-check --scope host:read --expires 1d
                 nexusd token revoke host-check
  NEXUS_URL    nexusd's URL, such as https://vm.lan:7443 (default on the
               host: https:// and the first listen address in
               /etc/nexus/nexusd.yaml; required with --ssh unless
               --nexus-host is given). The certificate is pinned to
               `nexusd tls fingerprint`, read on the host.
  NEXUS_CHECK_SSH_OPTS  extra ssh options, word-split, such as "-p 2222"

Exit status: 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or --yes is missing); 3 nothing failed but a check was
not run (NOT-RUN, such as the faults on a host where they cannot be
planted, or a 24 h soak finished early).
EOF
}

YES=0
MODE=check
FAULTS=0
OVERHEAD=0
SSH_TARGET=
NXHOST_GIVEN=
NEXUS_CLI=nexus
HOST_NEXUS=
CALLS=50
WINDOW=600
BASELINE=600
HUNG_MOUNT=
TRANSCRIPT=
ARGS="$*"
while [ $# -gt 0 ]; do
	case $1 in
	--yes) YES=1 ;;
	--with-faults) FAULTS=1 ;;
	--overhead) OVERHEAD=1 ;;
	--cleanup) MODE=cleanup ;;
	--rss-24h | --ssh | --nexus-host | --nexus | --host-nexus | --calls | --window | --baseline | --hung-mount | --transcript)
		{ [ $# -ge 2 ] && [ -n "$2" ]; } || { echo "$PROG: $1 needs a value" >&2; exit 2; }
		case $1 in
		--rss-24h)
			case $2 in
			start) MODE=rss-start ;;
			finish) MODE=rss-finish ;;
			*) echo "$PROG: --rss-24h takes start or finish" >&2; exit 2 ;;
			esac
			;;
		--ssh) SSH_TARGET=$2 ;;
		--nexus-host) NXHOST_GIVEN=$2 ;;
		--nexus) NEXUS_CLI=$2 ;;
		--host-nexus) HOST_NEXUS=$2 ;;
		--calls) CALLS=$2 ;;
		--window) WINDOW=$2 ;;
		--baseline) BASELINE=$2 ;;
		--hung-mount) HUNG_MOUNT=$2 ;;
		--transcript) TRANSCRIPT=$2 ;;
		esac
		shift
		;;
	-h | --help) usage; exit 0 ;;
	*) echo "$PROG: unknown argument: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done
for _n in "$CALLS" "$WINDOW" "$BASELINE"; do
	case $_n in '' | *[!0-9]*) echo "$PROG: --calls, --window and --baseline take a whole number: $_n" >&2; exit 2 ;; esac
done
[ "$CALLS" -ge 20 ] || { echo "$PROG: --calls must be at least 20, for a p95" >&2; exit 2; }
[ "$WINDOW" -ge 60 ] && [ "$BASELINE" -ge 60 ] || { echo "$PROG: --window and --baseline must be at least 60 seconds" >&2; exit 2; }
_m=0
[ "$FAULTS" = 0 ] || _m=$((_m + 1))
[ "$OVERHEAD" = 0 ] || _m=$((_m + 1))
[ "$MODE" = check ] || _m=$((_m + 1))
[ "$_m" -le 1 ] || { echo "$PROG: --with-faults, --overhead, --rss-24h and --cleanup are separate runs: pick one" >&2; exit 2; }

if [ -z "$TRANSCRIPT" ]; then
	_h=$(hostname)
	[ -z "$SSH_TARGET" ] || _h=${SSH_TARGET#*@}
	TRANSCRIPT=./host-check-$_h-$(date -u +%Y%m%dT%H%M%SZ).txt
fi
# Created exclusively and private: a name that exists, a planted symlink
# included, is refused rather than appended to.
if [ -e "$TRANSCRIPT" ] || [ -L "$TRANSCRIPT" ]; then
	echo "$PROG: the transcript $TRANSCRIPT already exists; choose another --transcript or remove it" >&2
	exit 2
fi
if ! (umask 077; set -C; : >"$TRANSCRIPT") 2>/dev/null; then
	echo "$PROG: cannot create the transcript $TRANSCRIPT" >&2
	exit 2
fi
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/host-check.XXXXXX") || { echo "$PROG: mktemp failed" >&2; exit 2; }

FAILS=0
INCONCL=0
RESULTS=
HSUDO=0
NXHOST=
INIT=
IN_CONTAINER=0
CHANGED=0 # the host holds something of this run that an exit must undo
CLEANED=0
F_MARKER=0
F_UNIT=
F_SVC=
CONF_SAVED=0

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

# Values from nexusd, the host or the state file that reach a host command
# must match an allowlist, and are quoted (shown) as well.
valid_path() {
	case $1 in /*) ;; *) return 1 ;; esac
	case $1 in *[!A-Za-z0-9_./@+-]* | */../* | */..) return 1 ;; esac
}
valid_num() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }
# valid_dec: a decimal number as awk prints one (-1.5, 2, 1.2e+06).
valid_dec() {
	case $1 in '' | *[!0-9eE.+-]*) return 1 ;; esac
	awk -v x="$1" 'BEGIN { exit !(x ~ /^-?[0-9]+(\.[0-9]*)?([eE][-+]?[0-9]+)?$/) }'
}
# in_range N LO HI: N is a whole number from LO to HI.
in_range() { valid_num "$1" && [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]; }

# field TEXT N: the Nth blank-separated field of TEXT's first line; for N
# <= 0, the field -N places before the last one (0 is the last). Empty if
# there is no such field. A line from the host is split here, never by the
# shell, so a field holds no glob and cannot move the others.
field() { printf '%s\n' "$1" | awk -v n="$2" 'NR == 1 { i = (n > 0) ? n : NF + n; if (i >= 1 && i <= NF) print $i; exit }'; }

# kv TEXT KEY: the value of the first line KEY=... in TEXT. kvs: every one.
kv() { printf '%s\n' "$1" | awk -v k="$2=" 'index($0, k) == 1 { print substr($0, length(k) + 1); exit }'; }
kvs() { printf '%s\n' "$1" | awk -v k="$2=" 'index($0, k) == 1 { print substr($0, length(k) + 1) }'; }

rand_hex8() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

# --- the host: local, or over SSH ----------------------------------------

ssh_() {
	# shellcheck disable=SC2086 # NEXUS_CHECK_SSH_OPTS is word-split on purpose.
	ssh -o BatchMode=yes ${NEXUS_CHECK_SSH_OPTS:-} -o ConnectTimeout=15 -o ServerAliveInterval=10 -o ServerAliveCountMax=3 "$SSH_TARGET" "$@"
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

# hval KEY: the value of the line KEY=... in HX_OUT (the first one).
hval() { kv "$HX_OUT" "$1"; }
# hvals KEY: every such value, one per line.
hvals() { kvs "$HX_OUT" "$1"; }

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

detect_init() {
	hx '[ -d /run/systemd/system ] && echo systemd || { [ -e /run/openrc ] && [ -x /sbin/openrc-run ] && echo openrc; }'
	INIT=$HX_OUT
	case $INIT in
	systemd | openrc) ;;
	*) refuse "the host runs neither systemd nor OpenRC" ;;
	esac
}

# nexusd_pid: NXD_PID, the PID of `nexusd serve` (uid 0), from /proc.
nexusd_pid() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hx 'for d in /proc/[0-9]*; do case $(tr "\0" " " 2>/dev/null <$d/cmdline) in "/usr/local/bin/nexusd serve "*) echo "${d#/proc/} $(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" $d/status)" ;; esac; done'
	NXD_PID=$(printf '%s\n' "$HX_OUT" | head -n 1 | cut -d' ' -f1)
	NXD_UID=$(printf '%s\n' "$HX_OUT" | head -n 1 | cut -d' ' -f2)
	valid_num "$NXD_PID" || NXD_PID=
}

# proc_sample: P_UP (/proc/uptime, centiseconds), P_TICKS (nexusd's
# utime+stime, fields 14 and 15 of /proc/<pid>/stat after the comm), P_RSS
# (VmRSS, kB) and P_PID, in one host command.
proc_sample() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hx 'p='"$NXD_PID"'; u=$(cut -d" " -f1 /proc/uptime); s=$(cat /proc/$p/stat 2>/dev/null) || { echo gone; exit 1; }; s=${s##*") "}; set -- $s; echo "$u $((${12} + ${13})) $(sed -n "s/^VmRSS:[[:space:]]*\([0-9]*\).*/\1/p" /proc/$p/status) $p"'
	P_UP=$(field "$HX_OUT" 1) P_TICKS=$(field "$HX_OUT" 2) P_RSS=$(field "$HX_OUT" 3) P_PID=$(field "$HX_OUT" 4)
	valid_dec "$P_UP" || P_UP=
	valid_num "$P_TICKS" || P_TICKS=
	valid_num "$P_RSS" || P_RSS=
	valid_num "$P_PID" || P_PID=
}

# --- the nexus CLI -------------------------------------------------------

# with_timeout SECONDS CMD...: CMD, killed with SIGTERM after SECONDS. The
# only kill outside the opt-in modes: it ends this script's own CLI child,
# here, never a process on the host.
with_timeout() {
	_wt=$1
	shift
	"$@" &
	_wp=$!
	(
		_i=0
		while [ "$_i" -lt "$_wt" ]; do
			sleep 1
			kill -0 "$_wp" 2>/dev/null || exit 0
			_i=$((_i + 1))
		done
		kill "$_wp"
	) >/dev/null 2>&1 &
	wait "$_wp"
}

# nx NAME ARG...: the nexus CLI against the host, with -o json, logged,
# bounded by NX_TIMEOUT seconds. The output goes to TMPD/NAME.json and is
# flattened into TMPD/NAME.flat; JF is that file. NX_RC is the status.
NX_TIMEOUT=60
nx() {
	_name=$1
	shift
	log "\$ $(shown nexus --host "$NXHOST" "$@" -o json)"
	with_timeout "$NX_TIMEOUT" "$NEXUS_CLI" --host "$NXHOST" "$@" -o json >"$TMPD/$_name.json" 2>"$TMPD/nxerr"
	NX_RC=$?
	tee -a "$TRANSCRIPT" <"$TMPD/$_name.json"
	[ ! -s "$TMPD/nxerr" ] || sed 's/^/(stderr) /' "$TMPD/nxerr" | tee -a "$TRANSCRIPT"
	[ "$NX_RC" -eq 0 ] || log "(exit $NX_RC)"
	jflat <"$TMPD/$_name.json" >"$TMPD/$_name.flat"
	JF=$TMPD/$_name.flat
	return "$NX_RC"
}

# nx_quiet NAME ARG...: as nx, logged on one line without the output.
nx_quiet() {
	_name=$1
	shift
	log "\$ $(shown nexus --host "$NXHOST" "$@" -o json) (output kept, not logged)"
	with_timeout "$NX_TIMEOUT" "$NEXUS_CLI" --host "$NXHOST" "$@" -o json >"$TMPD/$_name.json" 2>"$TMPD/nxerr"
	NX_RC=$?
	[ ! -s "$TMPD/nxerr" ] || sed 's/^/(stderr) /' "$TMPD/nxerr" | tee -a "$TRANSCRIPT"
	jflat <"$TMPD/$_name.json" >"$TMPD/$_name.flat"
	JF=$TMPD/$_name.flat
	return "$NX_RC"
}

# jflat: the CLI's indented JSON (json.MarshalIndent: one key or value per
# line) as "PATH<TAB>VALUE" lines, such as "[0].properties.hostname<TAB>"x"".
# VALUE is the scalar as printed (a string keeps its quotes), or [] or {}
# for an empty list or object.
jflat() {
	awk '
	{
		l = $0
		sub(/^[ \t]+/, "", l)
		sub(/,$/, "", l)
		if (l == "") next
		if (l == "}" || l == "]") { d--; next }
		if (d > 0 && ty[d] == "o") {
			if (!match(l, /^"[A-Za-z0-9_]+": /)) { print "jflat: unexpected line: " l > "/dev/stderr"; next }
			k = substr(l, 2, RLENGTH - 4)
			v = substr(l, RLENGTH + 1)
			p = (pa[d] == "") ? k : pa[d] "." k
		} else if (d > 0) {
			p = pa[d] "[" ix[d] "]"
			ix[d]++
			v = l
		} else {
			p = ""
			v = l
		}
		if (v == "{") { d++; ty[d] = "o"; pa[d] = p; next }
		if (v == "[") { d++; ty[d] = "a"; pa[d] = p; ix[d] = 0; next }
		print p "\t" v
	}'
}

# jv PATH: the raw value at PATH in JF, or <absent>. js PATH: a string
# without its quotes (null stays null).
jv() { awk -F'\t' -v p="$1" '$1 == p { print $2; f = 1; exit } END { if (!f) print "<absent>" }' "$JF"; }
js() { jv "$1" | sed -e 's/^"//' -e 's/"$//'; }
# jn PREFIX: how many items the list at PREFIX has ("" for the top level).
jn() { awk -F'\t' -v p="$1" 'index($1, p "[") == 1 { r = substr($1, length(p) + 2); sub(/\].*/, "", r); if (r + 1 > n) n = r + 1 } END { print n + 0 }' "$JF"; }
# jlist PREFIX: the string items of the list at PREFIX, one per line.
jlist() { awk -F'\t' -v p="$1" 'index($1, p "[") == 1 && substr($1, length(p) + 2) ~ /^[0-9]+\]$/ { v = $2; gsub(/^"|"$/, "", v); print v }' "$JF"; }
# reason PATH: the unavailable reason of the field PATH (a.b.c reads
# a.b.unavailable.c).
reason() {
	_k=${1##*.}
	_p=${1%."$_k"}
	[ "$_p" != "$1" ] || _p=
	js "${_p:+$_p.}unavailable.$_k"
}

# epoch RFC3339: seconds since 1970 (fraction kept), computed here (no
# date -d on macOS): days from the civil date (H. Hinnant's algorithm).
epoch() {
	printf '%s\n' "$1" | awk '{
		y = substr($0, 1, 4) + 0; m = substr($0, 6, 2) + 0; d = substr($0, 9, 2) + 0
		hh = substr($0, 12, 2) + 0; mi = substr($0, 15, 2) + 0; ss = substr($0, 18) ; sub(/Z.*/, "", ss); sub(/[+-].*/, "", ss)
		if (m <= 2) y--
		era = int(y / 400); yoe = y - era * 400
		mp = (m + 9) % 12
		doy = int((153 * mp + 2) / 5) + d - 1
		doe = yoe * 365 + int(yoe / 4) - int(yoe / 100) + doy
		printf "%.3f\n", (era * 146097 + doe - 719468) * 86400 + hh * 3600 + mi * 60 + ss
	}'
}

# calc EXPR NUMBER...: awk arithmetic, such as calc 'a * 1024' "$kb". EXPR
# is fixed text written at the call, in single quotes: the operands a to
# e, digits and + - * / ( ), so it can name no function or variable of
# awk's. Every value is an operand, passed with -v after valid_dec, never
# part of EXPR. Prints nothing and fails when EXPR has another character
# or an operand is not a number.
calc() {
	_ce=$1
	shift
	case $_ce in '' | *[!abcde0123456789.+*/\ \(\)-]*) return 1 ;; esac
	for _co in "$@"; do valid_dec "$_co" || return 1; done
	awk -v a="${1:-0}" -v b="${2:-0}" -v c="${3:-0}" -v d="${4:-0}" -v e="${5:-0}" "BEGIN { print $_ce }"
}
# within A B TOL: |A - B| <= TOL.
within() { awk -v a="$1" -v b="$2" -v t="$3" 'BEGIN { d = a - b; if (d < 0) d = -d; exit !(d <= t) }'; }

# setup_client: NXHOST, the host the CLI targets. With --nexus-host, the
# operator's own client config; otherwise a throwaway one in TMPD, with the
# certificate pinned to what `nexusd tls fingerprint` prints on the host and
# NEXUS_TOKEN passed on stdin (never in an argument or the transcript).
setup_client() {
	command -v "$NEXUS_CLI" >/dev/null 2>&1 || refuse "the nexus CLI ($NEXUS_CLI) is not found: install it from the release, or pass --nexus PATH"
	if [ -n "$NXHOST_GIVEN" ]; then
		NXHOST=$NXHOST_GIVEN
		note "nexus CLI: the configured host $NXHOST (your client config)"
		return
	fi
	[ -n "$CHECK_TOKEN" ] || return 1
	_url=${NEXUS_URL:-}
	if [ -z "$_url" ]; then
		[ -z "$SSH_TARGET" ] || refuse "with --ssh, set NEXUS_URL (such as https://vm.lan:7443) or pass --nexus-host"
		hx "sed -n 's/^listen:[[:space:]]*\\[[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' $CONF | head -n 1"
		_addr=${HX_OUT:-127.0.0.1:7443}
		_port=${_addr##*:} _host=${_addr%:*}
		case $_host in 0.0.0.0 | '' | '[::]') _host=127.0.0.1 ;; esac
		_url=https://$_host:$_port
	fi
	case $_url in
	https://*) ;;
	http://*) log "WARNING: NEXUS_URL is plain http, as you set it: nexusd is not authenticated and the token crosses unencrypted." ;;
	*) refuse "NEXUS_URL must start with https:// (or http://, set on purpose): $(shown "$_url")" ;;
	esac
	export XDG_CONFIG_HOME="$TMPD/xdg"
	mkdir -p "$XDG_CONFIG_HOME" && chmod 0700 "$XDG_CONFIG_HOME"
	NXHOST=host-check
	case $_url in
	https://*)
		hrun "$NEXUSD tls fingerprint" || refuse "could not read nexusd's certificate fingerprint on the host"
		_fp=$HX_OUT
		log "\$ (the token, on stdin) | $(shown nexus host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin)"
		printf '%s\n' "$CHECK_TOKEN" | "$NEXUS_CLI" host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin >"$TMPD/hostadd" 2>&1
		;;
	*)
		log "\$ (the token, on stdin) | $(shown nexus host add "$NXHOST" --url "$_url" --token-stdin)"
		printf '%s\n' "$CHECK_TOKEN" | "$NEXUS_CLI" host add "$NXHOST" --url "$_url" --token-stdin >"$TMPD/hostadd" 2>&1
		;;
	esac
	_rc=$?
	sed 's#[^ ]*/xdg/nexus/config.yaml#<throwaway client config>#' "$TMPD/hostadd" | tee -a "$TRANSCRIPT"
	[ "$_rc" -eq 0 ] || refuse "nexus host add failed (exit $_rc)"
}

# --- comparison bookkeeping ----------------------------------------------

# Per operation: C_OK fields matched, C_UN fields unavailable with a
# reason, C_BAD mismatches (C_WHY lists them).
c_begin() { C_OK=0 C_UN=0 C_BAD=0 C_WHY='' C_UNLIST=''; }
ok() { C_OK=$((C_OK + 1)); log "  ok    $1"; return 0; }
bad() { C_BAD=$((C_BAD + 1)); C_WHY="$C_WHY $1;"; log "  FAIL  $1"; return 0; }
# unav FIELD HOSTVALUE: FIELD is null with a reason; HOSTVALUE is what the
# host's own tool shows, if anything.
unav() {
	_r=$(reason "$1")
	C_UN=$((C_UN + 1))
	C_UNLIST="$C_UNLIST
$1: $_r"
	if [ -z "$_r" ]; then
		bad "$1 is null with no reason"
	else
		log "  unav  $1: $_r${2:+ (the host shows: $2)}"
	fi
	return 0
}
# eq LABEL FIELD HOSTVALUE: the provider's string FIELD equals HOSTVALUE.
eq() {
	_v=$(jv "$2")
	if [ "$_v" = null ]; then unav "$2" "$3"; return; fi
	_v=$(js "$2")
	if [ "$_v" = "$3" ]; then ok "$1 $_v = $3"; else bad "$1 provider '$_v', host '$3'"; fi
}
# near LABEL FIELD HOSTVALUE TOL: numerically within TOL.
near() {
	_v=$(jv "$2")
	if [ "$_v" = null ]; then unav "$2" "$3"; return; fi
	if [ -z "$3" ]; then bad "$1 provider $_v, the host gave no value"; return; fi
	if within "$_v" "$3" "$4"; then ok "$1 $_v ~ $3 (tolerance $4)"; else bad "$1 provider $_v, host $3 (tolerance $4)"; fi
}
c_end() {
	if [ "$C_BAD" -eq 0 ]; then
		result "$1" PASS "$C_OK fields match the host's tools, $C_UN unavailable with a reason${2:+; $2}"
	else
		result "$1" FAIL "$C_BAD mismatch(es):$C_WHY $C_OK match, $C_UN unavailable"
	fi
}

# walk OP: TC-HP-115's rule on JF: no null without a reason (but a series
# value); each unavailable key names a field that is null, [] or
# "unknown"; each reason starts with a reason prefix (docs/host.md). The
# (op, path, reason) list goes to TMPD/unavailable.
walk() {
	awk -F'\t' -v op="$1" '
	{ v[$1] = $2; o[n++] = $1 }
	END {
		pre = "warming up|not running|internal error: access not allow-listed|stale: last sample |permission denied: |not present: |timed out after |still pending: |not visible: |unrecognized format: |counter reset: |not reported: |command failed: |not supported: "
		np = split(pre, px, "|")
		for (i = 0; i < n; i++) {
			p = o[i]; val = v[p]
			if (val == "null") {
				if (p ~ /\]$/) {
					par = p; sub(/\[[0-9]+\]$/, "", par)
					if (par !~ /\.values$/) print "ERR\t" p ": a list item is null"
				} else {
					k = p; sub(/.*\./, "", k); par = substr(p, 1, length(p) - length(k) - 1)
					uk = (par == "" ? "" : par ".") "unavailable." k
					if (!(uk in v)) print "ERR\t" p " is null with no reason"
				}
			}
			if (p ~ /(^|\.)unavailable\.[A-Za-z0-9_]+$/) {
				k = p; sub(/.*\./, "", k); par = p; sub(/\.?unavailable\.[A-Za-z0-9_]+$/, "", par)
				f = (par == "" ? "" : par ".") k
				r = val; gsub(/^"|"$/, "", r)
				print "UNAV\t" op "\t" f "\t" r
				if (!(f in v)) print "ERR\t" p " names " k ", which is not null, [] or unknown (populated or not a field)"
				else if (v[f] != "null" && v[f] != "[]" && v[f] != "\"unknown\"") print "ERR\t" p " names " k ", which is populated: " v[f]
				okp = 0
				for (j = 1; j <= np; j++) if (index(r, px[j]) == 1) okp = 1
				if (!okp) print "ERR\t" p ": reason \"" r "\" starts with no reason prefix"
			}
		}
	}' "$JF" >"$TMPD/walk"
	grep '^UNAV' "$TMPD/walk" | cut -f2- >>"$TMPD/unavailable"
	WALK_ERR=$(grep '^ERR' "$TMPD/walk" | cut -f2-)
	WALK_UN=$(grep -c '^UNAV' "$TMPD/walk")
}

# --- the facts and the plan ----------------------------------------------

header() {
	log "# $PROG transcript: system-provider WI 5.4 (TC-HP-127 to TC-HP-130)"
	log "# started $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(hostname), mode $MODE$([ "$FAULTS" = 0 ] || echo ' --with-faults')$([ "$OVERHEAD" = 0 ] || echo ' --overhead'), host ${SSH_TARGET:-this machine}, arguments: ${ARGS:-(none)}"
	log "# script sha256 $( (command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256) <"$0" | cut -d' ' -f1)"
	hx '[ -e /.dockerenv ] || [ -e /run/.containerenv ] || grep -qa container= /proc/1/environ'
	if [ "$HX_RC" -eq 0 ]; then
		IN_CONTAINER=1
		log "# CONTAINER DETECTED on the host (/.dockerenv, /run/.containerenv or container= in PID 1's environment):"
		log "# this transcript is a container run, NOT real-host evidence."
	fi
}

facts() {
	note "== Step 1: facts"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'uname -a; [ ! -r /etc/alpine-release ] || echo "Alpine $(cat /etc/alpine-release)"; [ ! -r /etc/debian_version ] || echo "Debian base $(cat /etc/debian_version)"; sed -n "s/^PRETTY_NAME=//p" /etc/os-release; tr "\0" " " </proc/1/cmdline; echo; echo "nproc $(nproc); $(grep MemTotal /proc/meminfo)"'
	if [ "$INIT" = openrc ]; then hrun 'openrc --version'; else hrun 'systemctl --version | head -n 1'; fi
	hrun "$NEXUSD -version"
	log "\$ $(shown "$NEXUS_CLI" --version)"
	"$NEXUS_CLI" --version 2>&1 | tee -a "$TRANSCRIPT"
	nexusd_pid
	log "  nexusd: pid ${NXD_PID:-not found}, uid ${NXD_UID:-?}"
	[ -n "$NXD_PID" ] || refuse "no /usr/local/bin/nexusd serve process on the host"
	if [ "$INIT" = systemd ]; then
		hrun 'systemctl show -p MainPID,ActiveState,NRestarts nexusd.service; sha256sum /etc/systemd/system/nexusd.service; ls /etc/systemd/system/nexusd.service.d 2>&1 || true'
	fi
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun "sed -n '/^  host:\$/,/^  [a-z]/p' $CONF"
	PROVIDER_OK=0
	[ -n "$NXHOST" ] || { log "(no NEXUS_TOKEN and no --nexus-host: the provider was not asked)"; return; }
	nx_quiet caps capability get
	_i=$(awk -F'\t' '$1 ~ /\.providers\[[0-9]+\]\.name$/ && $2 == "\"host\"" { sub(/\.name$/, "", $1); print $1; exit }' "$JF")
	CAP_STATUS=$(js "$_i.status")
	CAP_INIT=$(js "$_i.details.initSystem")
	CAP_IV=$(jv "$_i.details.sampleIntervalSeconds")
	CAP_HD=$(jv "$_i.details.historyDurationSeconds")
	log "  capability: host provider $CAP_STATUS, initSystem $CAP_INIT, sampleIntervalSeconds $CAP_IV, historyDurationSeconds $CAP_HD"
	# Both reach awk and host commands later: whole seconds in the
	# provider's ranges (2s..60s, up to 24h), or empty (the defaults).
	in_range "$CAP_IV" 1 60 || { [ "$CAP_IV" = '<absent>' ] || log "  (sampleIntervalSeconds is not 1..60 whole seconds: not used)"; CAP_IV=; }
	in_range "$CAP_HD" 1 86400 || { [ "$CAP_HD" = '<absent>' ] || log "  (historyDurationSeconds is not 1..86400 whole seconds: not used)"; CAP_HD=; }
	if [ "$CAP_STATUS" = enabled ] && [ "$CAP_INIT" = "$INIT" ]; then
		result 1-provider PASS "the host provider is enabled; initSystem $CAP_INIT = the host's init ($INIT); sampleInterval ${CAP_IV}s, historyDuration ${CAP_HD}s"
		PROVIDER_OK=1
	else
		result 1-provider FAIL "host provider status '$CAP_STATUS', initSystem '$CAP_INIT' (the host runs $INIT): enable providers.host in $CONF and reload"
	fi
	if [ "$IN_CONTAINER" = 1 ]; then
		result 1-host FAIL "running in a container: not a real host"
	fi
}

plan() {
	log ""
	log "== Plan. With --yes the script will, on ${SSH_TARGET:-this host}:"
	case $MODE in
	cleanup)
		log "  - read $H_STATE/state and undo what an interrupted --with-faults or --overhead run left:"
		log "    remove the reboot-required marker it wrote, reset or remove its dummy service, and"
		log "    restore $CONF from the saved copy (then reload)."
		return
		;;
	rss-start)
		log "  - save $CONF to $H_STATE (0600), set providers.host.enabled false, restart nexusd,"
		log "    and sample nexusd's RSS and CPU every 60 s for $BASELINE s (the disabled baseline);"
		log "  - set enabled true, historyDuration 24h, sampleInterval 2s, validate and reload;"
		log "  - create the token $SOAK_TOKEN_NAME (host:read, 2 days) on the host, in a client"
		log "    config under $H_STATE, and start a poll on the host: hostsummary, hosthealth and a"
		log "    24 h hostmetric every 10 s, nexusd's VmRSS and CPU logged every 60 s to $H_STATE/rss.log;"
		log "  - leave it running: finish with --rss-24h finish after 24 h (or earlier, recorded NOT-RUN)."
		return
		;;
	rss-finish)
		log "  - stop the poll, read $H_STATE/rss.log: the maximum RSS over its last 10 minutes against"
		log "    the baseline, the start and end times; read hostmetric's historyStart and points;"
		log "  - revoke $SOAK_TOKEN_NAME, restore $CONF byte for byte, validate and reload."
		return
		;;
	esac
	log "  - call every nexus host* command with -o json and read each field back with the host's"
	log "    tools (TC-HP-127); walk every response: no null without a reason (TC-HP-115's rule);"
	log "  - NFR-3 over one sampler interval aligned on its tick: CPU against vmstat (or /proc/stat),"
	log "    memory available against free -b, rx/tx against /sys/class/net/*/statistics (TC-HP-128);"
	log "  - NFR-1: $CALLS timed calls per operation, the p95 from the CLI's --verbose timing (TC-HP-129);"
	[ -z "$HUNG_MOUNT" ] || log "  - the hung mount $HUNG_MOUNT: hostsummary and hostfs timed, its reason read;"
	log "  - btrfs (A-21, A-38): each btrfs filesystem listed once with df's numbers, if there is one."
	if [ "$FAULTS" = 1 ]; then
		log "  - --with-faults: write /var/run/reboot-required (Debian and Ubuntu, only if absent) and"
		if [ "$INIT" = systemd ]; then
			log "    run a transient unit nx-host-check-<random> that runs /bin/false and fails; read"
			log "    both back through hosthealth, then remove the marker and reset-failed the unit."
		else
			log "    start a dummy service /etc/init.d/nx-host-check-<random> (sleep) and end it with"
			log "    SIGKILL, so rc-status --crashed lists it; read it back, then zap it and remove it."
		fi
		log "    If the run is interrupted, the EXIT trap undoes both; --cleanup does it later."
	fi
	if [ "$OVERHEAD" = 1 ]; then
		log "  - --overhead: wait until the default 1 h history is full; nexusd's CPU (utime+stime) over"
		log "    $WINDOW s and its RSS every 60 s; then save $CONF, set the provider disabled, restart"
		log "    nexusd, measure the same; restore $CONF byte for byte and reload."
	fi
	if [ "$FAULTS" = 0 ] && [ "$OVERHEAD" = 0 ]; then
		log "  It changes nothing on the host: every host command reads."
	fi
}

# --- TC-HP-127: every field, read back ------------------------------------

check_all() {
	: >"$TMPD/unavailable"
	for _op in $OPS; do
		case $_op in
		hostfs | hostdisk | hostnet) nx "$_op" "$_op" get --limit 1000 ;;
		*) nx "$_op" "$_op" get ;;
		esac
		[ "$NX_RC" -eq 0 ] || { result "4-$_op" FAIL "nexus $_op get failed (exit $NX_RC)"; continue; }
		walk "$_op"
		if [ -n "$WALK_ERR" ]; then
			printf '%s\n' "$WALK_ERR" | sed 's/^/  WALK  /' | tee -a "$TRANSCRIPT"
			result "3-walk-$_op" FAIL "$(printf '%s\n' "$WALK_ERR" | wc -l | tr -d ' ') rule violation(s): $(printf '%s\n' "$WALK_ERR" | head -n 2 | tr '\n' ';')"
		else
			result "3-walk-$_op" PASS "no null without a reason; $WALK_UN field(s) unavailable, each with a reason prefix"
		fi
		cp "$JF" "$TMPD/$_op.flat.keep"
	done
	JF=$TMPD/hostidentity.flat.keep && check_identity
	JF=$TMPD/hostcpu.flat.keep && check_cpu
	JF=$TMPD/hostmemory.flat.keep && check_memory
	JF=$TMPD/hostfs.flat.keep && check_fs
	JF=$TMPD/hostdisk.flat.keep && check_disks
	JF=$TMPD/hostnet.flat.keep && check_net
	JF=$TMPD/hostnetconfig.flat.keep && check_netconfig
	JF=$TMPD/hosthealth.flat.keep && check_health
	JF=$TMPD/hostsummary.flat.keep && check_summary
	JF=$TMPD/hostmetric.flat.keep && check_metrics
	log ""
	log "  every unavailable field (op, path, reason):"
	sed 's/^/    /' "$TMPD/unavailable" | tee -a "$TRANSCRIPT"
	_sb=$(grep -cE '	(permission denied: |command failed: )' "$TMPD/unavailable")
	result 3-sandbox INFO "$(wc -l <"$TMPD/unavailable" | tr -d ' ') unavailable field(s); $_sb with permission denied: or command failed: (the reasons a sandbox gives)"
}

host_identity_facts() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'q() { awk -v k="$1=" "index(\$0, k) == 1 { print substr(\$0, length(k) + 1); exit }" /etc/os-release | sed -e "s/^\"//" -e "s/\"\$//"; }
echo "kernel_hostname=$(cat /proc/sys/kernel/hostname)"
command -v hostnamectl >/dev/null 2>&1 && echo "hostnamectl=$(hostnamectl hostname 2>/dev/null || hostnamectl --transient 2>/dev/null)"
echo "os_id=$(q ID)"; echo "os_version_id=$(q VERSION_ID)"; echo "os_pretty=$(q PRETTY_NAME)"
echo "uname_r=$(uname -r)"; echo "uname_m=$(uname -m)"
b=$(sed -n "s/^btime //p" /proc/stat); echo "btime=$b"; echo "btime_utc=$(date -u -d @$b +%Y-%m-%dT%H:%M:%SZ)"
echo "uptime=$(cut -d" " -f1 /proc/uptime)"
if command -v systemd-detect-virt >/dev/null 2>&1; then echo "detect_virt=$(systemd-detect-virt 2>/dev/null)"; fi
[ ! -e /.dockerenv ] || echo dockerenv=1
for f in sys_vendor product_name product_version bios_vendor bios_version; do [ ! -r /sys/class/dmi/id/$f ] || echo "dmi_$f=$(cat /sys/class/dmi/id/$f)"; done
[ ! -r /etc/timezone ] || echo "etc_timezone=$(head -n 1 /etc/timezone)"
[ ! -L /etc/localtime ] || echo "localtime_link=$(readlink /etc/localtime)"
[ -e /etc/localtime ] || echo "no_localtime=1"
echo "date_z=$(date +%z)"
if [ -d /run/systemd/system ] && [ -x /usr/bin/timedatectl ]; then echo "ntp_sync=$(/usr/bin/timedatectl show --property=NTPSynchronized --value 2>&1)"; elif [ -x /usr/bin/chronyc ]; then echo "chrony=$(/usr/bin/chronyc -c tracking 2>&1 | cut -d, -f14)"; fi'
}

check_identity() {
	log ""
	note "== Step 4a: hostidentity against hostnamectl, /etc/os-release, uname, /proc, systemd-detect-virt"
	host_identity_facts
	c_begin
	P='[0].properties'
	eq hostname "$P.hostname" "$(hval kernel_hostname)"
	_hc=$(hval hostnamectl)
	[ -z "$_hc" ] || log "  (hostnamectl hostname: $_hc)"
	eq os.id "$P.os.id" "$(hval os_id)"
	eq os.versionId "$P.os.versionId" "$(hval os_version_id)"
	eq os.prettyName "$P.os.prettyName" "$(hval os_pretty)"
	eq kernelRelease "$P.kernelRelease" "$(hval uname_r)"
	eq architecture "$P.architecture" "$(hval uname_m)"
	eq initSystem "$P.initSystem" "$INIT"
	eq bootTime "$P.bootTime" "$(hval btime_utc)"
	near uptimeSeconds "$P.uptimeSeconds" "$(hval uptime)" 5
	_vt=$(js "$P.virtualization.type") _vk=$(js "$P.virtualization.kind")
	if printf '%s\n' "$HX_OUT" | grep -q '^detect_virt='; then
		_dv=$(hval detect_virt)
		[ -n "$_dv" ] || _dv=none
		if [ "$_vt" = "$_dv" ]; then ok "virtualization.type $_vt = systemd-detect-virt ($_vk, evidence $(js "$P.virtualization.evidence"))"; else bad "virtualization.type '$_vt', systemd-detect-virt '$_dv'"; fi
	elif [ "$(hval dockerenv)" = 1 ]; then
		if [ "$_vt" = docker ]; then ok "virtualization.type docker: /.dockerenv exists (no systemd-detect-virt)"; else bad "virtualization.type '$_vt' with /.dockerenv present"; fi
	else
		log "  info  virtualization $_vt/$_vk (evidence $(js "$P.virtualization.evidence")): no systemd-detect-virt on this host to compare with"
	fi
	for _f in systemVendor:sys_vendor productName:product_name productVersion:product_version biosVendor:bios_vendor biosVersion:bios_version; do
		_hv=$(hval "dmi_${_f#*:}")
		if [ "$(jv "$P.dmi.${_f%%:*}")" = null ]; then unav "$P.dmi.${_f%%:*}" "$_hv"; else eq "dmi.${_f%%:*}" "$P.dmi.${_f%%:*}" "$_hv"; fi
	done
	_tz=$(hval etc_timezone)
	if [ -z "$_tz" ]; then
		_tz=$(hval localtime_link)
		_tz=${_tz#*zoneinfo/}
	fi
	[ "$(hval no_localtime)" != 1 ] || [ -n "$_tz" ] || _tz=UTC
	if [ -n "$_tz" ]; then eq timezone.name "$P.timezone.name" "$_tz"; else unav "$P.timezone.name"; fi
	_z=$(hval date_z)
	_zs=$(printf '%s\n' "$_z" | awk '{ s = (substr($0, 1, 1) == "-") ? -1 : 1; print s * (substr($0, 2, 2) * 3600 + substr($0, 4, 2) * 60) }')
	near timezone.offsetSeconds "$P.timezone.offsetSeconds" "$_zs" 0
	_sync=$(js "$P.clock.synchronized")
	if printf '%s\n' "$HX_OUT" | grep -q '^ntp_sync='; then
		case $(hval ntp_sync) in yes) _want=true ;; no) _want=false ;; *) _want=unknown ;; esac
		if [ "$_sync" = "$_want" ]; then ok "clock.synchronized $_sync = timedatectl NTPSynchronized $(hval ntp_sync)"; else bad "clock.synchronized '$_sync', timedatectl '$(hval ntp_sync)'"; fi
	elif printf '%s\n' "$HX_OUT" | grep -q '^chrony='; then
		log "  info  clock.synchronized $_sync; chronyc leap status '$(hval chrony)'"
		ok "clock.synchronized $_sync (chronyc present)"
	elif [ "$_sync" = unknown ]; then
		unav "$P.clock.synchronized" ""
	else
		bad "clock.synchronized '$_sync' with neither timedatectl (systemd) nor chronyc on the host"
	fi
	c_end 4a-identity
}

check_cpu() {
	log ""
	note "== Step 4b: hostcpu against nproc, /proc/cpuinfo, /proc/loadavg, cpufreq, hwmon"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'echo "nproc=$(nproc)"; echo "nproc_all=$(nproc --all 2>/dev/null)"; echo "stat_cpus=$(grep -c "^cpu[0-9]" /proc/stat)"
echo "model=$(sed -n "s/^model name[[:space:]]*:[[:space:]]*//p" /proc/cpuinfo | head -n 1)"
echo "loadavg=$(cat /proc/loadavg)"
f=/sys/devices/system/cpu/cpu0/cpufreq; [ ! -r $f/cpuinfo_min_freq ] || echo "freq_range=$(cat $f/cpuinfo_min_freq) $(cat $f/cpuinfo_max_freq)"
echo "cpuinfo_mhz=$(sed -n "s/^cpu MHz[[:space:]]*:[[:space:]]*//p" /proc/cpuinfo | head -n 1)"
n=0; for t in /sys/class/hwmon/hwmon*/temp*_input /sys/class/thermal/thermal_zone*/temp; do [ ! -r "$t" ] || n=$((n + 1)); done; echo "temp_inputs=$n"'
	c_begin
	P='[0].properties'
	_n=$(hval nproc)
	_lc=$(jv "$P.logicalCpus")
	if [ "$_lc" = "$_n" ]; then ok "logicalCpus $_lc = nproc"
	elif [ "$_lc" = "$(hval nproc_all)" ] || [ "$_lc" = "$(hval stat_cpus)" ]; then ok "logicalCpus $_lc = nproc --all / the cpuN lines of /proc/stat (nproc $_n is limited by affinity)"
	else bad "logicalCpus $_lc, nproc $_n, /proc/stat cpuN lines $(hval stat_cpus)"; fi
	_pc=$(jn "$P.perCpu")
	if [ "$_pc" = "$_lc" ]; then ok "perCpu has $_pc entries = logicalCpus"; else bad "perCpu has $_pc entries, logicalCpus $_lc"; fi
	_m=$(hval model)
	if [ -n "$_m" ]; then eq model "$P.model" "$_m"; elif [ "$(jv "$P.model")" = null ]; then unav "$P.model" ""; else bad "model '$(js "$P.model")' but /proc/cpuinfo has no model name"; fi
	_la=$(hval loadavg)
	for _l in 1:load1 2:load5 3:load15; do
		_hl=$(field "$_la" "${_l%%:*}")
		near "load.${_l#*:}" "$P.load.${_l#*:}" "$_hl" "$(calc '0.25 + 0.1 * a' "$_hl")"
	done
	_fr=$(hval freq_range) _fq=$(jv "$P.frequencyMhz")
	if [ "$_fq" = null ]; then
		unav "$P.frequencyMhz" "$_fr$(hval cpuinfo_mhz)"
	elif [ -n "$_fr" ]; then
		if awk -v f="$_fq" -v lo="${_fr% *}" -v hi="${_fr#* }" 'BEGIN { exit !(f >= lo / 1000 - 1 && f <= hi / 1000 + 1) }'; then ok "frequencyMhz $_fq within cpufreq's range $_fr kHz"; else bad "frequencyMhz $_fq outside cpufreq's range $_fr kHz"; fi
	else
		near frequencyMhz "$P.frequencyMhz" "$(hval cpuinfo_mhz)" "$(calc '0.5 * a' "$(hval cpuinfo_mhz)")"
	fi
	_nt=$(jn "$P.temperatures") _ht=$(hval temp_inputs)
	if [ "$_nt" -eq 0 ]; then
		if [ "$_ht" -eq 0 ]; then unav "$P.temperatures" "no hwmon temp*_input and no thermal zone"; else bad "temperatures [] but the host has $_ht temperature inputs"; fi
	elif [ "$_ht" -gt 0 ]; then ok "temperatures: $_nt sensor(s); the host has $_ht temperature inputs"
	else bad "temperatures lists $_nt sensor(s), the host has no temperature input"; fi
	_pcores=$(jv "$P.physicalCores")
	if [ "$_pcores" = null ]; then unav "$P.physicalCores" ""; elif [ "$_pcores" -ge 1 ] && [ "$_pcores" -le "$_lc" ]; then ok "physicalCores $_pcores within 1..logicalCpus (range check)"; else bad "physicalCores $_pcores outside 1..$_lc"; fi
	for _t in userPercent systemPercent iowaitPercent stealPercent; do
		_tv=$(jv "$P.times.$_t")
		if [ "$_tv" = null ]; then unav "$P.times.$_t" ""; elif within "$_tv" 50 50; then ok "times.$_t $_tv within 0..100 (range check; usage is NFR-3's)"; else bad "times.$_t $_tv outside 0..100"; fi
	done
	c_end 4b-cpu
}

check_memory() {
	log ""
	note "== Step 4c: hostmemory against free -b and /proc/meminfo"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'free -b; awk "/^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SReclaimable|Shmem|SwapTotal|SwapFree):/ { sub(\":\", \"\", \$1); print \"mi_\" \$1 \"=\" \$2 * 1024 }" /proc/meminfo; for f in cpu memory io; do [ ! -r /proc/pressure/$f ] || echo "psi_$f=1"; done'
	c_begin
	P='[0].properties'
	_mem=$(printf '%s\n' "$HX_OUT" | awk '$1 == "Mem:"')
	_swap=$(printf '%s\n' "$HX_OUT" | awk '$1 == "Swap:"')
	_mt=$(hval mi_MemTotal) _ma=$(hval mi_MemAvailable)
	_tol=$(calc '0.01 * a' "$_mt")
	near totalBytes "$P.totalBytes" "$(field "$_mem" 2)" 0
	near freeBytes "$P.freeBytes" "$(hval mi_MemFree)" "$_tol"
	near usedBytes "$P.usedBytes" "$(calc 'a - b' "$_mt" "$_ma")" "$_tol"
	near buffersBytes "$P.buffersBytes" "$(hval mi_Buffers)" "$_tol"
	near "cachedBytes (Cached + SReclaimable, free's cache)" "$P.cachedBytes" "$(calc 'a + b' "$(hval mi_Cached)" "$(hval mi_SReclaimable)")" "$_tol"
	near sharedBytes "$P.sharedBytes" "$(hval mi_Shmem)" "$_tol"
	near usedPercent "$P.usedPercent" "$(calc '100 * (a - b) / a' "$_mt" "$_ma")" 1
	_st=$(field "$_swap" 2)
	_tol=$(calc '0.01 * a + 1048576' "${_st:-0}")
	near swapTotalBytes "$P.swapTotalBytes" "$_st" 0
	near swapUsedBytes "$P.swapUsedBytes" "$(field "$_swap" 3)" "$_tol"
	near swapFreeBytes "$P.swapFreeBytes" "$(field "$_swap" 4)" "$_tol"
	for _f in cpu memory io; do
		if [ "$(hval "psi_$_f")" = 1 ]; then
			if [ "$(jv "$P.pressure.$_f")" = null ]; then unav "$P.pressure.$_f" "/proc/pressure/$_f is readable"; else ok "pressure.$_f present; /proc/pressure/$_f is readable"; fi
		elif [ "$(jv "$P.pressure.$_f")" = null ]; then unav "$P.pressure.$_f" ""
		else bad "pressure.$_f present but the host has no /proc/pressure/$_f"; fi
	done
	c_end 4c-memory "availableBytes is NFR-3's (step 5)"
}

# bracket LABEL FIELD BEFORE AFTER SLACK: the provider's FIELD lies between
# the host's reads before and after the call, widened by SLACK.
bracket() {
	_v=$(jv "$2")
	if [ "$_v" = null ]; then unav "$2" "$3"; return 0; fi
	if [ -z "$3" ] || [ -z "$4" ]; then bad "$1 provider $_v, the host gave no value"; return 0; fi
	if awk -v v="$_v" -v b="$3" -v a="$4" -v s="$5" 'BEGIN { lo = (b < a ? b : a) - s; hi = (b > a ? b : a) + s; exit !(v >= lo && v <= hi) }'; then
		ok "$1 $_v within the host's $3 .. $4 (± $5)"
	else
		bad "$1 provider $_v, the host read $3 before and $4 after the call (± $5)"
	fi
	return 0
}

check_fs() {
	log ""
	note "== Step 4d: hostfs against df -B1, stat -f and PID 1's mount table"
	hx 'cat /proc/1/mountinfo'
	printf '%s\n' "$HX_OUT" >"$TMPD/mountinfo"
	c_begin
	_n=$(jn "")
	log "  $_n filesystem(s) listed"
	: >"$TMPD/fs-listed"
	: >"$TMPD/fs-mps"
	_cmd=
	_i=0
	while [ "$_i" -lt "$_n" ]; do
		_mp=$(js "[$_i].properties.mountPoint")
		printf '%s\n' "$_mp" >>"$TMPD/fs-listed"
		_i=$((_i + 1))
		if ! valid_path "$_mp"; then
			log "  info  $(shown "$_mp"): not compared (characters outside the allowlist)"
			continue
		fi
		printf '%s\n' "$_mp" >>"$TMPD/fs-mps"
		_k=$(wc -l <"$TMPD/fs-mps" | tr -d ' ')
		# df's size, used and available, and statfs's fragment size, inode
		# total and free, at the same mount point.
		_cmd="$_cmd echo \"df$_k=\$(df -B1 -P $(shown "$_mp") | tail -n 1)\"; echo \"st$_k=\$(stat -f -c '%S %c %d' $(shown "$_mp"))\";"
	done
	# Used bytes are exact "at the same instant": df and statfs just before
	# and just after a fresh call, and the provider's value within that
	# bracket, ± 1 block for a busy filesystem.
	hx "$_cmd"
	_before=$HX_OUT
	nx_quiet hostfs-fresh hostfs get --limit 1000
	hx "$_cmd"
	log "\$ [host] df -B1 -P and stat -f -c '%S %c %d' at each mount point, just before and just after that call:"
	printf '%s\n-- after:\n%s\n' "$_before" "$HX_OUT" | sed 's/^/    /' | tee -a "$TRANSCRIPT"
	_k=0
	while IFS= read -r _mp; do
		_k=$((_k + 1))
		_ix=$(awk -F'\t' -v m="\"$_mp\"" '$1 ~ /^\[[0-9]+\]\.properties\.mountPoint$/ && $2 == m { sub(/\.mountPoint$/, "", $1); print $1; exit }' "$JF")
		[ -n "$_ix" ] || { bad "$_mp is not in the fresh hostfs answer"; continue; }
		P=$_ix
		_ty=$(js "$P.fsType")
		_mi=$(awk -v mp="$_mp" '$5 == mp { l = $0 } END { print l }' "$TMPD/mountinfo")
		_hty=$(printf '%s\n' "$_mi" | sed 's/.* - //' | cut -d' ' -f1)
		_hro=$(printf '%s\n' "$_mi" | cut -d' ' -f6 | tr ',' '\n' | grep -qx ro && echo true || echo false)
		[ "$_ty" = "$_hty" ] && ok "$_mp fsType $_ty = /proc/1/mountinfo" || bad "$_mp fsType '$_ty', /proc/1/mountinfo '$_hty'"
		if [ "$(jv "$P.readOnly")" = null ]; then unav "$P.readOnly" "$_hro"; else eq "$_mp readOnly" "$P.readOnly" "$_hro"; fi
		if [ "$(jv "$P.sizeBytes")" = null ]; then
			unav "$P.sizeBytes" ""
			continue
		fi
		# df -P: SOURCE SIZE USED AVAILABLE CAPACITY MOUNTPOINT. The source
		# (a FUSE mount's is the user's choice) may hold blanks, so the
		# numbers are counted from the end: the mount point passed
		# valid_path and has none.
		_l=$(kv "$_before" "df$_k")
		_s0=$(field "$_l" -4) _u0=$(field "$_l" -3) _a0=$(field "$_l" -2)
		_l=$(kv "$HX_OUT" "df$_k")
		_s1=$(field "$_l" -4) _u1=$(field "$_l" -3) _a1=$(field "$_l" -2)
		_l=$(kv "$_before" "st$_k")
		_bs=$(field "$_l" 1) _f0=$(field "$_l" 2) _ff0=$(field "$_l" 3)
		_l=$(kv "$HX_OUT" "st$_k")
		_f1=$(field "$_l" 2) _ff1=$(field "$_l" 3)
		valid_num "$_bs" || _bs=4096
		bracket "$_mp sizeBytes (df)" "$P.sizeBytes" "$_s0" "$_s1" 0
		bracket "$_mp usedBytes (df)" "$P.usedBytes" "$_u0" "$_u1" "$_bs"
		bracket "$_mp availableBytes (df)" "$P.availableBytes" "$_a0" "$_a1" "$_bs"
		if [ "$_f0" = 0 ]; then
			[ "$(jv "$P.inodesTotal")" = null ] && unav "$P.inodesTotal" "statfs: 0 inodes" || bad "$_mp inodesTotal set, statfs reports 0 inodes"
		else
			bracket "$_mp inodesTotal (statfs)" "$P.inodesTotal" "$_f0" "$_f1" 0
			bracket "$_mp inodesFree (statfs)" "$P.inodesFree" "$_ff0" "$_ff1" 1
		fi
	done <"$TMPD/fs-mps"
	# Coverage: every mount of a disk or network filesystem in PID 1's table
	# is one of the listed items or shares a device (maj:min) with one.
	_miss=$(awk -v list="$TMPD/fs-listed" '
		BEGIN { while ((getline l < list) > 0) want[l] = 1 }
		{ t = $0; sub(/.* - /, "", t); split(t, a, " "); dev[NR] = $3; mp[NR] = $5; ty[NR] = a[1]; if ($5 in want) have[$3] = 1 }
		END {
			for (i = 1; i <= NR; i++) {
				if (ty[i] !~ /^(ext[234]|xfs|btrfs|vfat|exfat|f2fs|zfs|nfs|nfs4|cifs|smb3|fuseblk|ntfs3?|jfs|reiserfs)$/) continue
				if (mp[i] ~ /^\/(snap|var\/lib\/docker|var\/lib\/containerd|run\/containerd|var\/lib\/containers|run\/containers)\//) continue
				if (!(dev[i] in have)) print mp[i] " (" ty[i] " " dev[i] ")"
			}
		}' "$TMPD/mountinfo")
	if [ -z "$_miss" ]; then ok "coverage: every disk or network filesystem mount in /proc/1/mountinfo is listed, or shares its device with a listed item"; else bad "not listed: $(printf '%s\n' "$_miss" | tr '\n' ' ')"; fi
	c_end 4d-filesystems
	check_btrfs
}

# A-21, A-38: a btrfs filesystem is one item, with df's numbers, never "not
# visible:".
check_btrfs() {
	_bdev=$(awk '{ t = $0; sub(/.* - /, "", t); split(t, a, " "); if (a[1] == "btrfs") print $3 }' "$TMPD/mountinfo" | sort -u)
	if [ -z "$_bdev" ]; then
		result 4d-btrfs INFO "no btrfs filesystem on this host: the A-21/A-38 live readback was not possible here"
		return
	fi
	_nb=$(awk -F'\t' '$1 ~ /\.properties\.fsType$/ && $2 == "\"btrfs\"" { n++ } END { print n + 0 }' "$JF")
	_hb=$(printf '%s\n' "$_bdev" | wc -l | tr -d ' ')
	_vis=$(awk -F'\t' '$1 ~ /\.unavailable\.sizeBytes$/ && $2 ~ /^"not visible:/' "$JF" | wc -l | tr -d ' ')
	if [ "$_nb" = "$_hb" ]; then
		result 4d-btrfs PASS "$_hb btrfs filesystem(s) in /proc/1/mountinfo (devices $(printf '%s\n' "$_bdev" | tr '\n' ' ')), $_nb item(s) listed, each compared with df above; not-visible reasons in the list: $_vis"
	else
		result 4d-btrfs FAIL "$_hb btrfs filesystem(s) in /proc/1/mountinfo, $_nb item(s) listed"
	fi
}

check_disks() {
	log ""
	note "== Step 4e: hostdisk against lsblk (or /sys/block)"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'if command -v lsblk >/dev/null 2>&1; then lsblk -b -d -n -r -o NAME,SIZE,ROTA,RM | sed "s/^/lsblk=/"; fi
for d in /sys/block/*; do n=${d##*/}; m=; [ ! -r $d/device/model ] || m=$(sed "s/[[:space:]]*$//" $d/device/model); p=; for q in $d/$n*; do [ ! -e $q/partition ] || p="$p ${q##*/}"; done; echo "sys=$n $(($(cat $d/size) * 512)) $(cat $d/queue/rotational 2>/dev/null || echo -) $(cat $d/removable) bf=$( [ -r $d/loop/backing_file ] && echo 1 || echo 0) parts=$p model=$m"; done'
	c_begin
	_n=$(jn "")
	_i=0
	: >"$TMPD/disks-listed"
	while [ "$_i" -lt "$_n" ]; do
		P="[$_i].properties"
		_d=$(js "[$_i].name")
		printf '%s\n' "$_d" >>"$TMPD/disks-listed"
		_i=$((_i + 1))
		_s=$(printf '%s\n' "$HX_OUT" | awk -v d="$_d" '$1 == "sys=" d { print; exit }')
		[ -n "$_s" ] || { bad "disk $_d is not in /sys/block"; continue; }
		# sys=NAME SIZE ROTATIONAL REMOVABLE ..., or lsblk=NAME SIZE ROTA RM.
		_l=$(printf '%s\n' "$HX_OUT" | awk -v d="$_d" '$1 == "lsblk=" d { print; exit }')
		if [ -n "$_l" ]; then
			_src=lsblk
		else
			_l=$_s
			_src=/sys/block
		fi
		near "$_d sizeBytes ($_src)" "$P.sizeBytes" "$(field "$_l" 2)" 0
		case $(field "$_l" 3) in 1) _rv=true ;; 0) _rv=false ;; *) _rv= ;; esac
		[ -z "$_rv" ] && [ "$(jv "$P.rotational")" = null ] && unav "$P.rotational" "" || eq "$_d rotational ($_src)" "$P.rotational" "$_rv"
		case $(field "$_l" 4) in 1) _rv=true ;; *) _rv=false ;; esac
		eq "$_d removable ($_src)" "$P.removable" "$_rv"
		_hm=$(printf '%s\n' "$_s" | sed -n 's/.* model=//p')
		if [ -n "$_hm" ]; then eq "$_d model" "$P.model" "$_hm"; else [ "$(jv "$P.model")" = null ] && unav "$P.model" "" || bad "$_d model set, no /sys/block/$_d/device/model"; fi
		_hp=$(printf '%s\n' "$_s" | sed -n 's/.* parts=\(.*\) model=.*/\1/p' | tr ' ' '\n' | grep . | sort | tr '\n' ' ')
		_pp=$(awk -F'\t' -v p="$P.partitions[" 'index($1, p) == 1 && $1 ~ /\]\.name$/ { v = $2; gsub(/"/, "", v); print v }' "$JF" | sort | tr '\n' ' ')
		[ "$_hp" = "$_pp" ] && ok "$_d partitions [${_pp% }] = /sys/block" || bad "$_d partitions [$_pp], /sys/block [$_hp]"
	done
	# Coverage: every /sys/block entry but ram*, zram* and unbacked loop*.
	_miss=$(printf '%s\n' "$HX_OUT" | awk -v list="$TMPD/disks-listed" '
		BEGIN { while ((getline l < list) > 0) have[l] = 1 }
		/^sys=/ { n = substr($1, 5); if (n ~ /^(ram|zram)/) next; if (n ~ /^loop/ && $5 != "bf=1") next; if (!(n in have)) print n }')
	[ -z "$_miss" ] && ok "coverage: every disk in /sys/block is listed (ram, zram and unbacked loop devices are left out)" || bad "not listed: $(printf '%s\n' "$_miss" | tr '\n' ' ')"
	c_end 4e-disks
}

host_net_facts() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hx 'for i in /sys/class/net/*; do n=${i##*/}; s=$(cat "$i/speed" 2>/dev/null) || s=; echo "if=$n mac=$(cat "$i/address") mtu=$(cat "$i/mtu") state=$(cat "$i/operstate") type=$(cat "$i/type") speed=$s rx=$(cat "$i/statistics/rx_bytes") tx=$(cat "$i/statistics/tx_bytes")"; ip -o addr show dev "$n" 2>/dev/null | awk -v n="$n" "{ print \"addr=\" n \" \" \$3 \" \" \$4 }"; done'
}

check_net() {
	log ""
	note "== Step 4f: hostnet against /sys/class/net and ip addr"
	host_net_facts
	_before=$HX_OUT
	nx_quiet hostnet2 hostnet get --limit 1000
	host_net_facts
	log "\$ [host] (each interface's /sys/class/net attributes and ip -o addr, read before and after that call)"
	printf '%s\n' "$HX_OUT" | tee -a "$TRANSCRIPT"
	c_begin
	_n=$(jn "")
	_i=0
	: >"$TMPD/if-listed"
	while [ "$_i" -lt "$_n" ]; do
		P="[$_i].properties"
		_d=$(js "[$_i].name")
		printf '%s\n' "$_d" >>"$TMPD/if-listed"
		_i=$((_i + 1))
		_s=$(printf '%s\n' "$HX_OUT" | awk -v d="$_d" '$1 == "if=" d { print; exit }')
		[ -n "$_s" ] || { bad "interface $_d is not in /sys/class/net"; continue; }
		_hv() { kv "$(printf '%s\n' "$_s" | tr ' ' '\n')" "$1"; }
		eq "$_d mac" "$P.mac" "$(_hv mac)"
		near "$_d mtu" "$P.mtu" "$(_hv mtu)" 0
		eq "$_d state" "$P.state" "$(_hv state)"
		[ "$(_hv type)" = 772 ] && _lo=true || _lo=false
		eq "$_d loopback" "$P.loopback" "$_lo"
		_sp=$(_hv speed)
		case $_sp in '' | -*) [ "$(jv "$P.speedMbps")" = null ] && unav "$P.speedMbps" "" || bad "$_d speedMbps set, /sys/class/net/$_d/speed unreadable or negative" ;; *) near "$_d speedMbps" "$P.speedMbps" "$_sp" 0 ;; esac
		# Counters: between the reads before and after the call.
		_b=$(printf '%s\n' "$_before" | awk -v d="$_d" '$1 == "if=" d { print; exit }' | tr ' ' '\n' | sed -n 's/^rx=//p')
		_rx=$(jv "$P.rxBytes")
		if awk -v a="$_b" -v v="$_rx" -v z="$(_hv rx)" 'BEGIN { exit !(v >= a && v <= z) }'; then ok "$_d rxBytes $_rx within the host's reads $_b..$(_hv rx)"; else bad "$_d rxBytes $_rx outside the host's reads $_b..$(_hv rx)"; fi
		_b=$(printf '%s\n' "$_before" | awk -v d="$_d" '$1 == "if=" d { print; exit }' | tr ' ' '\n' | sed -n 's/^tx=//p')
		_tx=$(jv "$P.txBytes")
		if awk -v a="$_b" -v v="$_tx" -v z="$(_hv tx)" 'BEGIN { exit !(v >= a && v <= z) }'; then ok "$_d txBytes $_tx within the host's reads $_b..$(_hv tx)"; else bad "$_d txBytes $_tx outside the host's reads $_b..$(_hv tx)"; fi
		for _fam in ipv4:inet ipv6:inet6; do
			_ha=$(printf '%s\n' "$HX_OUT" | awk -v d="$_d" -v f="${_fam#*:}" '$1 == "addr=" d && $2 == f { print $3 }' | sort | tr '\n' ' ')
			_pa=$(awk -F'\t' -v p="$P.${_fam%%:*}[" 'index($1, p) == 1 { split(substr($1, length(p) + 1), a, "]."); v = $2; gsub(/"/, "", v); x[a[1]] = x[a[1]] (a[2] == "address" ? v : "") ; if (a[2] == "prefixLength") y[a[1]] = v } END { for (k in x) print x[k] "/" y[k] }' "$JF" | sort | tr '\n' ' ')
			if [ "$(jv "$P.${_fam%%:*}")" = "[]" ] && [ -z "$_ha" ]; then
				ok "$_d ${_fam%%:*} [] = ip addr (none)"
			elif [ "$_ha" = "$_pa" ]; then ok "$_d ${_fam%%:*} [${_pa% }] = ip addr"
			else bad "$_d ${_fam%%:*} [$_pa], ip addr [$_ha]"; fi
		done
	done
	_miss=$(printf '%s\n' "$HX_OUT" | awk -v list="$TMPD/if-listed" 'BEGIN { while ((getline l < list) > 0) have[l] = 1 } /^if=/ { n = substr($1, 4); if (!(n in have)) print n }')
	[ -z "$_miss" ] && ok "coverage: every interface in /sys/class/net is listed" || bad "not listed: $(printf '%s\n' "$_miss" | tr '\n' ' ')"
	c_end 4f-interfaces "rates are NFR-3's (step 5)"
}

check_netconfig() {
	log ""
	note "== Step 4g: hostnetconfig against ip route and the resolver file"
	P='[0].properties'
	_src=$(js "$P.dns.source")
	_q=/etc/resolv.conf
	valid_path "$_src" && _q=$_src
	hrun "ip -4 route show default; ip -6 route show default; echo '-- $_q:'; sed -n 's/^nameserver[[:space:]]*//p' $(shown "$_q"); echo '-- /etc/resolv.conf:'; sed -n 's/^nameserver[[:space:]]*//p' /etc/resolv.conf"
	c_begin
	_hg=$(printf '%s\n' "$HX_OUT" | awk '$1 == "default" { a = ""; d = ""; m = 0; for (i = 2; i < NF; i++) { if ($i == "via") a = $(i + 1); if ($i == "dev") d = $(i + 1); if ($i == "metric") m = $(i + 1) } if (a != "") print a, d, m }' | sort | tr '\n' ';')
	_pg=$(awk -F'\t' -v p="$P.defaultGateways[" 'index($1, p) == 1 { split(substr($1, length(p) + 1), a, "]."); v = $2; gsub(/"/, "", v); f[a[1], a[2]] = v; k[a[1]] = 1 } END { for (i in k) print f[i, "address"], f[i, "interface"], f[i, "metric"] }' "$JF" | sort | tr '\n' ';')
	[ "$_hg" = "$_pg" ] && ok "defaultGateways [$_pg] = ip route show default (address dev metric)" || bad "defaultGateways [$_pg], ip route [$_hg]"
	_fns=$(printf '%s\n' "$HX_OUT" | awk -v h="-- $_q:" '$0 == h { f = 1; next } /^-- / { f = 0 } f' | sort -u | tr '\n' ' ')
	_rns=$(printf '%s\n' "$HX_OUT" | sed -n '/^-- \/etc\/resolv.conf:$/,$p' | grep -v '^-- ' | sort -u | tr '\n' ' ')
	_up=$(awk -F'\t' -v p="$P.dns.servers[" 'index($1, p) == 1 { split(substr($1, length(p) + 1), a, "]."); v = $2; gsub(/"/, "", v); f[a[1], a[2]] = v; k[a[1]] = 1 } END { for (i in k) if (f[i, "stub"] == "false") print f[i, "address"] }' "$JF" | sort -u | tr '\n' ' ')
	_st=$(awk -F'\t' -v p="$P.dns.servers[" 'index($1, p) == 1 { split(substr($1, length(p) + 1), a, "]."); v = $2; gsub(/"/, "", v); f[a[1], a[2]] = v; k[a[1]] = 1 } END { for (i in k) if (f[i, "stub"] == "true") print f[i, "address"] }' "$JF" | sort -u | tr '\n' ' ')
	log "  dns.source $_src; upstream servers [$_up]; stubs [$_st]; nameservers in $_q [$_fns], in /etc/resolv.conf [$_rns]"
	if [ "$_q" = /etc/resolv.conf ]; then
		[ "$(printf '%s %s' "$_up" "$_st" | tr ' ' '\n' | grep . | sort -u | tr '\n' ' ')" = "$_rns" ] && ok "dns.servers = the nameservers of /etc/resolv.conf" || bad "dns.servers [$_up$_st], /etc/resolv.conf [$_rns]"
	else
		[ "$_up" = "$_fns" ] && ok "dns.servers (stub false) = the nameservers of $_src" || bad "dns upstreams [$_up], $_src [$_fns]"
		[ -z "$_st" ] || { [ "$(printf '%s\n' "$_st" | tr ' ' '\n' | grep . | grep -vE '^(127\.|::1$)')" = "" ] && ok "stubs [$_st] are loopback nameservers" || bad "stubs [$_st] are not all loopback"; }
	fi
	c_end 4g-netconfig
}

host_health_facts() {
	if [ "$INIT" = systemd ]; then
		hrun "systemctl --failed --plain --no-legend --no-pager | awk '{ print \"failed=\" \$1 }'; awk '/^oom_kill / { print \"oom=\" \$2 }' /proc/vmstat; [ ! -e /var/run/reboot-required ] || { echo marker=1; cat /var/run/reboot-required.pkgs 2>/dev/null | sed 's/^/pkg=/'; }; for w in /usr/share/update-notifier/notify-reboot-required /usr/share/perl5/NeedRestart/UI/Ubuntu.pm; do [ ! -e \$w ] || echo writer=\$w; done; [ ! -r /etc/debian_version ] || echo debian=1"
	else
		hrun "rc-status --crashed | sed 's/^/failed=/'; awk '/^oom_kill / { print \"oom=\" \$2 }' /proc/vmstat; [ -d /lib/modules/\$(uname -r) ] && echo modules=1 || echo modules=0; [ ! -r /etc/alpine-release ] || echo alpine=1; [ ! -e /var/run/reboot-required ] || echo marker=1; [ ! -r /etc/debian_version ] || echo debian=1"
	fi
}

# want_reboot: WANT_RB, the rebootRequired state the rules in docs/host.md
# give for what host_health_facts read.
want_reboot() {
	if [ "$(hval debian)" = 1 ]; then
		if [ "$(hval marker)" = 1 ]; then WANT_RB=true
		elif [ -n "$(hval writer)" ]; then WANT_RB=false
		else WANT_RB=unknown; fi
	elif [ "$(hval alpine)" = 1 ]; then
		if [ "$IN_CONTAINER" = 1 ]; then WANT_RB=unknown
		elif [ "$(hval modules)" = 1 ]; then WANT_RB=false
		else WANT_RB=true; fi
	else
		WANT_RB=unknown
	fi
}

check_health() {
	log ""
	note "== Step 4h: hosthealth against $([ "$INIT" = systemd ] && echo 'systemctl --failed' || echo 'rc-status --crashed'), the reboot rule, /proc"
	host_health_facts
	c_begin
	P='[0].properties'
	_hf=$(hvals failed | grep . | sort)
	_hn=$(printf '%s\n' "$_hf" | grep -c .)
	near failedServices.count "$P.failedServices.count" "$_hn" 0
	_pn=$(jlist "$P.failedServices.names" | sort)
	_hf20=$(printf '%s\n' "$_hf" | head -n 20)
	[ "$_pn" = "$_hf20" ] && ok "failedServices.names [$(printf '%s\n' "$_pn" | tr '\n' ' ')] = the host's list" || bad "failedServices.names [$(printf '%s\n' "$_pn" | tr '\n' ' ')], host [$(printf '%s\n' "$_hf20" | tr '\n' ' ')]"
	eq failedServices.source "$P.failedServices.source" "$INIT"
	near oomKills "$P.oomKills" "$(hval oom)" 0
	want_reboot
	_rs=$(js "$P.rebootRequired.state")
	[ "$_rs" = "$WANT_RB" ] && ok "rebootRequired.state $_rs, as the rule for this host gives ($(js "$P.rebootRequired.check"): $(js "$P.rebootRequired.evidence"))" || bad "rebootRequired.state '$_rs', the rule gives '$WANT_RB'"
	[ "$_rs" != unknown ] || unav "$P.rebootRequired.state" ""
	# Each top process against its /proc entry, read 0.3 s after a sampler
	# tick, and the answer that tick's process scan gives: rssBytes is the
	# scan's, so a read at another time would compare different moments.
	_pids=$(awk -F'\t' '$1 ~ /\.(topByCpu|topByMemory)\[[0-9]+\]\.pid$/ { print $2 }' "$JF" | sort -un | tr '\n' ' ')
	_hj=$JF
	align
	log "  the processes are read on the host ${AL_WAIT}s from now, 0.3 s after the sampler's next tick"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'sleep '"$AL_WAIT"'; hz=$(getconf CLK_TCK 2>/dev/null || echo 100); up=$(cut -d" " -f1 /proc/uptime); for p in '"$(printf '%s' "$_pids" | tr -cd '0-9 ')"'; do st=$(cat /proc/$p/stat 2>/dev/null) || { echo "proc=$p gone"; continue; }; st=${st##*") "}; set -- $st; u=$(sed -n "s/^Uid:[[:space:]]*\([0-9]*\).*/\1/p" /proc/$p/status); r=$(sed -n "s/^VmRSS:[[:space:]]*\([0-9]*\).*/\1/p" /proc/$p/status); c=$(cat /proc/$p/comm 2>/dev/null); echo "proc=$p ${u:--} ${r:--} $((${up%.*} - ${20} / hz)) $(printf "%s" "$c" | tr -c "A-Za-z0-9._:+@-" "_")"; done; awk -F: "{ print \"user=\" \$3 \" \" \$1 }" /etc/passwd'
	_procs=$HX_OUT
	nx_quiet health-aligned hosthealth get
	log "  hosthealth's processes after that tick (sampledAt $(js "$P.sampler.sampledAt"), processSampleSeconds $(jv "$P.processSampleSeconds")):"
	awk -F'\t' '$1 ~ /\.(topByCpu|topByMemory)\[[0-9]+\]\.(pid|command|rssBytes)$/ { printf "%s=%s ", substr($1, 15), $2; if ($1 ~ /rssBytes$/) print "" }' "$JF" | sed 's/^/    /' | tee -a "$TRANSCRIPT"
	HX_OUT=$_procs
	for _lst in topByCpu topByMemory; do
		_k=$(jn "$P.$_lst")
		_j=0
		while [ "$_j" -lt "$_k" ]; do
			_q="$P.${_lst}[$_j]"
			_j=$((_j + 1))
			_pid=$(jv "$_q.pid") _cmd=$(js "$_q.command") _uid=$(jv "$_q.uid") _usr=$(js "$_q.user") _rss=$(jv "$_q.rssBytes")
			# proc=PID UID VMRSS-kB AGE-seconds COMM, or proc=PID gone. Any
			# user names their own process (comm), so the host prints it
			# last, each byte outside A-Za-z0-9._:+@- made _: one field,
			# with no blank, quote or glob. The provider's command (its
			# JSON escapes \" and \\ undone) is mapped the same way.
			_pl=$(printf '%s\n' "$HX_OUT" | awk -v p="$_pid" '$1 == "proc=" p { print; exit }')
			_huid=$(field "$_pl" 2) _hkb=$(field "$_pl" 3) _hage=$(field "$_pl" 4) _hcomm=$(field "$_pl" 5)
			if [ -z "$_pl" ] || [ "$_huid" = gone ]; then log "  info  $_lst pid $_pid ($_cmd) has exited since"; continue; fi
			valid_num "$_huid" || _huid=
			valid_num "$_hkb" || _hkb=
			valid_num "$_hage" || _hage=
			_cs=$(printf '%s' "$_cmd" | sed 's/\\\(["\\]\)/\1/g' | tr -c 'A-Za-z0-9._:+@-' '_')
			_hu=
			[ -z "$_huid" ] || _hu=$(printf '%s\n' "$HX_OUT" | awk -v u="$_huid" '$1 == "user=" u { print $2; exit }')
			_e=
			[ "$_cs" = "$_hcomm" ] || _e="$_e command '$_cmd' vs comm '$_hcomm';"
			[ "$_uid" = "$_huid" ] || _e="$_e uid $_uid vs ${_huid:-?};"
			[ -z "$_hu" ] || [ "$_usr" = "$_hu" ] || _e="$_e user '$_usr' vs /etc/passwd '$_hu';"
			# A process younger than a minute (an SSH session of this
			# script) is shown, not compared: its RSS is still moving.
			if [ "$_lst" = topByMemory ] && [ -n "$_hkb" ]; then
				if [ -z "$_hage" ] || [ "$_hage" -lt 60 ]; then
					log "  info  $_lst pid $_pid $_cmd is ${_hage:-?} s old: rssBytes $_rss, VmRSS now $_hkb kB (not compared)"
				elif ! within "$_rss" "$(calc 'a * 1024' "$_hkb")" "$(calc '0.2 * a * 1024 + 1048576' "$_hkb")"; then
					_e="$_e rssBytes $_rss vs VmRSS $_hkb kB (±20% + 1 MiB);"
				fi
			fi
			[ -z "$_e" ] && ok "$_lst pid $_pid $_cmd uid $_uid $_usr = /proc/$_pid$([ "$_lst" != topByMemory ] || echo "; rssBytes $_rss, VmRSS ${_hkb:-?} kB")" || bad "$_lst pid $_pid:$_e"
		done
	done
	JF=$_hj
	_ps=$(jv "$P.processSampleSeconds")
	if [ "$_ps" = null ]; then unav "$P.processSampleSeconds" ""; elif awk -v s="$_ps" -v i="${CAP_IV:-10}" 'BEGIN { exit !(s >= i * 0.9) }'; then ok "processSampleSeconds $_ps >= the interval (${CAP_IV:-10} s)"; else bad "processSampleSeconds $_ps below the interval"; fi
	c_end 4h-health
}

check_summary() {
	log ""
	note "== Step 4i: hostsummary against the per-area operations and the host"
	c_begin
	P='[0].properties'
	_ij=$TMPD/hostidentity.flat.keep
	for _f in hostname os.id os.versionId os.prettyName kernelRelease architecture initSystem bootTime; do
		_a=$(jv "$P.identity.$_f")
		_b=$(JF=$_ij jv "[0].properties.$_f")
		[ "$_a" = "$_b" ] && ok "identity.$_f $_a = hostidentity" || bad "identity.$_f $_a, hostidentity $_b"
	done
	_b=$(JF=$TMPD/hostcpu.flat.keep jv "[0].properties.logicalCpus")
	[ "$(jv "$P.cpu.logicalCpus")" = "$_b" ] && ok "cpu.logicalCpus = hostcpu ($_b)" || bad "cpu.logicalCpus $(jv "$P.cpu.logicalCpus"), hostcpu $_b"
	for _f in totalBytes swapTotalBytes; do
		_b=$(JF=$TMPD/hostmemory.flat.keep jv "[0].properties.$_f")
		[ "$(jv "$P.memory.$_f")" = "$_b" ] && ok "memory.$_f = hostmemory ($_b)" || bad "memory.$_f $(jv "$P.memory.$_f"), hostmemory $_b"
	done
	_a=$(awk -F'\t' -v p="$P.filesystems[" 'index($1, p) == 1 && $1 ~ /\]\.mountPoint$/ { print $2 }' "$JF" | sort | tr '\n' ' ')
	_b=$(awk -F'\t' '$1 ~ /^\[[0-9]+\]\.properties\.mountPoint$/ { print $2 }' "$TMPD/hostfs.flat.keep" | sort | tr '\n' ' ')
	[ "$_a" = "$_b" ] && ok "filesystems' mount points = hostfs" || bad "filesystems [$_a], hostfs [$_b]"
	# The summary's interfaces are the non-loopback ones (at most 32).
	_a=$(awk -F'\t' -v p="$P.network.interfaces[" 'index($1, p) == 1 && $1 ~ /\]\.name$/ { print $2 }' "$JF" | sort | tr '\n' ' ')
	_b=$(awk -F'\t' '$1 ~ /^\[[0-9]+\]\.name$/ { n[substr($1, 1, index($1, "]"))] = $2 } $1 ~ /^\[[0-9]+\]\.properties\.loopback$/ { l[substr($1, 1, index($1, "]"))] = $2 } END { for (k in n) if (l[k] != "true") print n[k] }' "$TMPD/hostnet.flat.keep" | sort | head -n 32 | tr '\n' ' ')
	[ "$_a" = "$_b" ] && ok "network.interfaces' names [$_a] = hostnet's non-loopback interfaces" || bad "network.interfaces [$_a], hostnet's non-loopback [$_b]"
	_hj=$TMPD/hosthealth.flat.keep
	_b=$(JF=$_hj js "[0].properties.rebootRequired.state")
	[ "$(js "$P.health.rebootRequired")" = "$_b" ] && ok "health.rebootRequired $_b = hosthealth" || bad "health.rebootRequired $(js "$P.health.rebootRequired"), hosthealth $_b"
	_b=$(JF=$_hj jv "[0].properties.failedServices.count")
	[ "$(jv "$P.health.failedServicesCount")" = "$_b" ] && ok "health.failedServicesCount $_b = hosthealth" || bad "health.failedServicesCount $(jv "$P.health.failedServicesCount"), hosthealth $_b"
	_b=$(JF=$_hj jv "[0].properties.oomKills")
	[ "$(jv "$P.health.oomKills")" = "$_b" ] && ok "health.oomKills $_b = hosthealth" || bad "health.oomKills $(jv "$P.health.oomKills"), hosthealth $_b"
	c_end 4i-summary
}

check_metrics() {
	log ""
	note "== Step 4j: hostmetric (the history)"
	c_begin
	P='[0].properties'
	_st=$(js "$P.sampler.state") _pt=$(jv "$P.points")
	[ "$_st" = ready ] && ok "sampler.state ready" || bad "sampler.state '$_st'"
	[ "$_pt" -ge 1 ] 2>/dev/null && ok "points $_pt, historyStart $(js "$P.historyStart"), stepSeconds $(jv "$P.stepSeconds")" || bad "points '$_pt'"
	_names=$(awk -F'\t' '$1 ~ /\.series\[[0-9]+\]\.(metric|name)$/ { v = $2; gsub(/"/, "", v); if ($1 ~ /metric$/) m = v; else print m "." v }' "$JF" | sort | tr '\n' ' ')
	[ "$_names" = "cpu.usagePercent load.load1 memory.availableBytes memory.usedBytes swap.usedBytes " ] && ok "default series: $_names" || bad "default series: [$_names]"
	nx_quiet metric-net hostmetric get --metric network --metric disk --window-seconds 86400
	_nn=$(awk -F'\t' '$1 ~ /\.series\[[0-9]+\]\.metric$/ { print $2 }' "$JF" | sort | uniq -c | tr -s ' ' | tr '\n' ';')
	[ "$NX_RC" -eq 0 ] && ok "--metric network --metric disk --window-seconds 86400 answers: series per metric$_nn; truncated $(jv "$P.entitiesTruncated.interfaces") interfaces, $(jv "$P.entitiesTruncated.disks") disks" || bad "hostmetric with network and disk failed (exit $NX_RC)"
	c_end 4j-metrics
}

# --- TC-HP-128: NFR-3 accuracy ---------------------------------------------

# align: AL_WAIT, the seconds until 0.3 s after the sampler's next tick,
# from hostcpu's sampler (ageSeconds and intervalSeconds).
align() {
	nx_quiet align hostcpu get
	_age=$(jv "[0].properties.sampler.ageSeconds") _iv=$(jv "[0].properties.sampler.intervalSeconds")
	AL_IV=$(printf '%.0f' "$_iv" 2>/dev/null) || AL_IV=${CAP_IV:-10}
	# It reaches the host's command line (vmstat, sleep) and $((...)).
	if ! in_range "$AL_IV" 1 60; then
		log "  (sampler.intervalSeconds $(shown "$_iv") is not 1..60 seconds: 10 assumed)"
		AL_IV=10
	fi
	AL_WAIT=$(awk -v a="$_age" -v i="$AL_IV" 'BEGIN { w = i - a + 0.3; while (w < 0.3) w += i; printf "%.2f", w }')
}

check_nfr3() {
	log ""
	note "== Step 5: NFR-3 accuracy over one sampler interval, aligned on its tick (TC-HP-128)"
	align
	hx 'command -v vmstat >/dev/null 2>&1 && echo vmstat'
	_vm=$HX_OUT
	log "  sampler interval ${AL_IV}s; the window starts ${AL_WAIT}s from now, 0.3 s after a tick; CPU from ${_vm:-/proc/stat (no vmstat on the host)}"
	# Traffic for the rate comparison: read calls through the CLI for the
	# whole window (over SSH this crosses the host's network interface;
	# on the host itself, lo).
	(
		_e=$(($(date +%s) + ${AL_IV%.*} + 4))
		while [ "$(date +%s)" -lt "$((_e + ${AL_WAIT%.*}))" ]; do
			"$NEXUS_CLI" --host "$NXHOST" hostsummary get -o json >/dev/null 2>&1
		done
	) &
	_tp=$!
	# shellcheck disable=SC2016 # Expanded on the host.
	_w='r() { echo "up$1=$(cut -d" " -f1 /proc/uptime)"; echo "stat$1=$(head -n 1 /proc/stat)"; for i in /sys/class/net/*; do echo "net$1=${i##*/} $(cat $i/statistics/rx_bytes) $(cat $i/statistics/tx_bytes)"; done; echo "at$1=$(date -u +%Y-%m-%dT%H:%M:%SZ)"; }; sleep '"$AL_WAIT"'; r 0; '
	if [ -n "$_vm" ]; then _w="$_w vmstat $AL_IV 2 | tail -n 1 | sed 's/^/vmstat=/'; r 1"; else _w="$_w sleep $AL_IV; r 1"; fi
	hrun "$_w"
	_out=$HX_OUT
	sleep 1
	nx nfr3-cpu hostcpu get
	_pcpu=$(jv "[0].properties.usagePercent") _sat=$(js "[0].properties.sampler.sampledAt") _siv=$(jv "[0].properties.sampler.intervalSeconds")
	nx_quiet nfr3-net hostnet get --limit 1000
	nx nfr3-mem hostmemory get
	_pavail=$(jv "[0].properties.availableBytes")
	hrun 'free -b'
	_havail=$(printf '%s\n' "$HX_OUT" | awk '$1 == "Mem:" { print $7 }')
	wait "$_tp" 2>/dev/null
	HX_OUT=$_out
	_u0=$(hval up0) _u1=$(hval up1)
	_dt=$(calc 'a - b' "$_u1" "$_u0")
	_at1=$(epoch "$(hval at1)")
	_se=$(epoch "$_sat")
	log "  host window ${_dt}s ($(hval at0) .. $(hval at1)); provider sample at $_sat over ${_siv}s"
	if awk -v s="$_se" -v e="$_at1" 'BEGIN { exit !(s >= e - 2.5 && s <= e + 1) }'; then
		_aligned=1
	else
		_aligned=0
		log "  the provider's sample ($_sat) is not the one that ends the host's window (at $(hval at1)): the comparison is not over the same interval"
	fi
	# CPU: busy = total - idle - iowait (A-6), as vmstat's 100 - id - wa.
	_hcpu=$(printf '%s\n%s\n' "$(hval stat0)" "$(hval stat1)" | awk '{ t = 0; for (i = 2; i <= NF; i++) t += $i; idle = $5 + $6; if (NR == 1) { t0 = t; i0 = idle } else { printf "%.2f", 100 * ((t - t0) - (idle - i0)) / (t - t0) } }')
	_vmc=
	if [ -n "$_vm" ]; then
		_vl=$(hval vmstat)
		# id and wa: the 15th and 16th columns of procps vmstat.
		_vmc=$(printf '%s\n' "$_vl" | awk '{ print 100 - $15 - $16 }')
	fi
	_ref=${_vmc:-$_hcpu}
	_refn=$([ -n "$_vmc" ] && echo "vmstat 100-id-wa" || echo "/proc/stat delta")
	if [ "$_pcpu" = null ]; then
		result 5-cpu FAIL "usagePercent is null ($(JF=$TMPD/nfr3-cpu.flat reason "[0].properties.usagePercent"))"
	elif [ "$_aligned" = 1 ] && within "$_pcpu" "$_ref" 5; then
		result 5-cpu PASS "usagePercent $_pcpu, $_refn $_ref (within ±5 pp); /proc/stat over the same window $_hcpu"
	else
		result 5-cpu FAIL "usagePercent $_pcpu, $_refn $_ref, /proc/stat $_hcpu (aligned $_aligned)"
	fi
	if within "$_pavail" "$_havail" "$(calc '0.01 * a' "$_havail")"; then
		result 5-memory PASS "availableBytes $_pavail, free -b available $_havail (within ±1%)"
	else
		result 5-memory FAIL "availableBytes $_pavail, free -b available $_havail"
	fi
	# Rates: each interface with at least 1 KiB/s each way on the host.
	JF=$TMPD/nfr3-net.flat
	_cmp=0 _rbad=
	# net0= and net1= lines: NAME RX-BYTES TX-BYTES.
	hvals net0 >"$TMPD/net0"
	while IFS= read -r _l0; do
		_if=$(field "$_l0" 1)
		_l1=$(hvals net1 | awk -v d="$_if" '$1 == d { print; exit }')
		_hrx=$(calc '(a - b) / c' "$(field "$_l1" 2)" "$(field "$_l0" 2)" "$_dt")
		_htx=$(calc '(a - b) / c' "$(field "$_l1" 3)" "$(field "$_l0" 3)" "$_dt")
		_ix=$(awk -F'\t' -v d="\"$_if\"" '$1 ~ /^\[[0-9]+\]\.name$/ && $2 == d { sub(/\.name$/, "", $1); print $1; exit }' "$JF")
		[ -n "$_ix" ] || continue
		_prx=$(jv "$_ix.properties.rxBytesPerSecond") _ptx=$(jv "$_ix.properties.txBytesPerSecond")
		if awk -v a="$_hrx" -v b="$_htx" 'BEGIN { exit !(a >= 1024 && b >= 1024) }'; then
			_cmp=$((_cmp + 1))
			if within "$_prx" "$_hrx" "$(calc '0.05 * a' "$_hrx")" && within "$_ptx" "$_htx" "$(calc '0.05 * a' "$_htx")"; then
				log "  ok    $_if rx $_prx B/s ~ $_hrx, tx $_ptx B/s ~ $_htx (within ±5%)"
			else
				log "  FAIL  $_if rx $_prx B/s vs $_hrx, tx $_ptx B/s vs $_htx"
				_rbad="$_rbad $_if;"
			fi
		else
			log "  info  $_if rx $_prx vs $_hrx B/s, tx $_ptx vs $_htx B/s (under 1 KiB/s each way: not compared)"
		fi
	done <"$TMPD/net0"
	if [ "$_cmp" -eq 0 ]; then
		result 5-network NOT-RUN "no interface carried 1 KiB/s each way during the window"
	elif [ -z "$_rbad" ] && [ "$_aligned" = 1 ]; then
		result 5-network PASS "$_cmp interface(s) within ±5% of the /sys/class/net statistics delta over ${_dt}s"
	else
		result 5-network FAIL "outside ±5%:${_rbad:- none} (aligned $_aligned)"
	fi
	log "  usedBytes: exact to one block against df -B1 at each mount point in step 4d (4d-filesystems)"
}

# --- TC-HP-129: NFR-1 latency ----------------------------------------------

# ms_of: the CLI's --verbose duration ("< 200 OK (12ms)") in milliseconds.
ms_of() {
	sed -n 's/^< [0-9][0-9]* .*(\([0-9.]*\)\([mµun]*s\))$/\1 \2/p' | awk '{ if ($2 == "ms") print $1; else if ($2 == "s") print $1 * 1000; else print 0 }'
}

check_nfr1() {
	log ""
	note "== Step 6: NFR-1 latency, $CALLS calls per operation (TC-HP-129)"
	log "  each call is a new nexus process (TLS handshake included); the time is the CLI's own"
	log "  --verbose request time, from the request to the response's status line"
	_worst=0 _bad=
	for _op in $OPS; do
		: >"$TMPD/ms"
		_k=0 _err=0
		while [ "$_k" -lt "$CALLS" ]; do
			"$NEXUS_CLI" --verbose --host "$NXHOST" "$_op" get -o json >/dev/null 2>"$TMPD/verbose" || _err=$((_err + 1))
			ms_of <"$TMPD/verbose" | tail -n 1 >>"$TMPD/ms"
			_k=$((_k + 1))
		done
		_st=$(sort -n "$TMPD/ms" | awk '{ v[NR] = $1 } END { i = int(0.95 * NR); if (i < 0.95 * NR) i++; printf "n=%d p50=%s p95=%s max=%s", NR, v[int((NR + 1) / 2)], v[i], v[NR] }')
		log "  $_op: $_st ms, errors $_err"
		_p95=$(printf '%s\n' "$_st" | sed 's/.*p95=\([0-9.]*\).*/\1/')
		_n=$(printf '%s\n' "$_st" | sed 's/^n=\([0-9]*\).*/\1/')
		if [ "$_err" -ne 0 ] || [ "$_n" -ne "$CALLS" ] || ! awk -v p="$_p95" 'BEGIN { exit !(p < 200) }'; then _bad="$_bad $_op p95 ${_p95}ms errors $_err;"; fi
		awk -v p="$_p95" -v w="$_worst" 'BEGIN { exit !(p > w) }' && _worst=$_p95
	done
	if [ -z "$_bad" ]; then
		result 6-latency PASS "p95 < 200 ms for each of the ten operations ($CALLS calls each); the worst p95 ${_worst} ms"
	else
		result 6-latency FAIL "$_bad"
	fi
	if [ -z "$HUNG_MOUNT" ]; then
		result 6-hung-mount NOT-RUN "the optional manual step (an NFS mount with its server firewalled; --hung-mount PATH) was not set up"
		return
	fi
	valid_path "$HUNG_MOUNT" || refuse "--hung-mount must be an absolute path of letters, digits and _./@+-"
	_hb=
	for _op in hostsummary hostfs; do
		"$NEXUS_CLI" --verbose --host "$NXHOST" "$_op" get -o json >"$TMPD/hung.json" 2>"$TMPD/verbose"
		_ms=$(ms_of <"$TMPD/verbose" | tail -n 1)
		log "  $_op with $HUNG_MOUNT hung: ${_ms} ms"
		awk -v p="$_ms" 'BEGIN { exit !(p < 2500) }' || _hb="$_hb $_op ${_ms}ms;"
	done
	jflat <"$TMPD/hung.json" >"$TMPD/hung.flat"
	_r=$(awk -F'\t' -v mp="\"$HUNG_MOUNT\"" '$1 ~ /\.mountPoint$/ && $2 == mp { sub(/\.mountPoint$/, "", $1); p = $1 } p != "" && $1 == p ".unavailable.sizeBytes" { print $2; exit }' "$TMPD/hung.flat")
	log "  $HUNG_MOUNT sizeBytes reason: ${_r:-none}"
	case $_r in
	'"timed out after '* | '"still pending: '*) [ -z "$_hb" ] && result 6-hung-mount PASS "both under 2.5 s; $HUNG_MOUNT listed with $_r" || result 6-hung-mount FAIL "$_hb" ;;
	*) result 6-hung-mount FAIL "$HUNG_MOUNT reason '${_r:-none}' (want timed out after 2s: statfs ...);$_hb" ;;
	esac
}

# --- opt-in: state, faults, configuration ------------------------------------

# mutating_allowed: a host change is permitted only with --yes and an
# opt-in mode. Every mut_* function starts with it.
mutating_allowed() {
	[ "$YES" = 1 ] || return 1
	[ "$FAULTS" = 1 ] || [ "$OVERHEAD" = 1 ] || [ "$MODE" != check ]
}

mut_save_state() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	printf 'mode=%s\ninit=%s\nf_marker=%s\nf_unit=%s\nf_svc=%s\nconf_saved=%s\nrss_start=%s\nrss_base=%s\nrss_base_cpu=%s\npoller=%s\nnxhost=%s\n' \
		"$MODE$([ "$FAULTS" = 0 ] || echo +faults)$([ "$OVERHEAD" = 0 ] || echo +overhead)" "$INIT" "$F_MARKER" "$F_UNIT" "$F_SVC" "$CONF_SAVED" "${RSS_START:-}" "${RSS_BASE:-}" "${RSS_BASE_CPU:-}" "${POLLER:-}" "$NXHOST" >"$TMPD/state"
	hx_in "umask 077 && mkdir -p $H_STATE && cat >$H_STATE/state.new && mv -f $H_STATE/state.new $H_STATE/state" <"$TMPD/state"
	[ "$HX_RC" -eq 0 ] || log "WARNING: could not write $H_STATE/state: $HX_OUT"
}

state_value() { kv "$STATE_TEXT" "$1"; }

load_state() {
	hx "cat $H_STATE/state"
	[ "$HX_RC" -eq 0 ] || return 1
	STATE_TEXT=$HX_OUT
	F_MARKER=$(state_value f_marker) F_UNIT=$(state_value f_unit) F_SVC=$(state_value f_svc)
	CONF_SAVED=$(state_value conf_saved) RSS_START=$(state_value rss_start) RSS_BASE=$(state_value rss_base)
	RSS_BASE_CPU=$(state_value rss_base_cpu) POLLER=$(state_value poller) STATE_MODE=$(state_value mode)
	case $F_UNIT in '' | nx-host-check-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) refuse "$H_STATE/state names an unexpected unit: $(shown "$F_UNIT")" ;; esac
	case $F_SVC in '' | nx-host-check-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) refuse "$H_STATE/state names an unexpected service: $(shown "$F_SVC")" ;; esac
	for _v in "$RSS_START" "$RSS_BASE" "$POLLER"; do
		[ -z "$_v" ] || valid_num "$_v" || refuse "$H_STATE/state has a value that is not a number: $(shown "$_v")"
	done
	return 0
}

mut_fault_reboot() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	log ""
	note "== Step 7a: the reboot-required marker (--with-faults)"
	hx '[ -r /etc/debian_version ] && echo debian; [ ! -e /var/run/reboot-required ] || echo present'
	case $HX_OUT in
	*present*)
		result 7a-reboot NOT-RUN "/var/run/reboot-required already exists on this host: it is not this script's to plant or remove"
		return
		;;
	debian*) ;;
	*)
		if [ "$INIT" = openrc ]; then
			result 7a-reboot NOT-RUN "Alpine's rule reads /lib/modules/<running release>; planting it would mean moving the running kernel's modules away, which this script never does (4h-health checked the rule as the host is)"
		else
			result 7a-reboot NOT-RUN "not a Debian or Ubuntu host: no marker rule to plant"
		fi
		return
		;;
	esac
	F_MARKER=1
	mut_save_state
	CHANGED=1
	hrun "printf '%s\n' '*** System restart required ***' >/var/run/reboot-required && printf '%s\n' nexus-host-check >/var/run/reboot-required.pkgs && ls -l /var/run/reboot-required /var/run/reboot-required.pkgs"
	nx f-rb1 hosthealth get
	_s1=$(js "[0].properties.rebootRequired.state") _p1=$(jlist "[0].properties.rebootRequired.packages" | tr '\n' ' ')
	hrun 'rm -f /var/run/reboot-required /var/run/reboot-required.pkgs; ls /var/run/reboot-required 2>&1; true'
	F_MARKER=0
	mut_save_state
	nx f-rb2 hosthealth get
	_s2=$(js "[0].properties.rebootRequired.state")
	host_health_facts
	want_reboot
	if [ "$_s1" = true ] && [ "$_p1" = "nexus-host-check " ] && [ "$_s2" = "$WANT_RB" ]; then
		result 7a-reboot PASS "marker planted: state true, packages [nexus-host-check]; removed: $_s2 (the rule gives $WANT_RB)"
	else
		result 7a-reboot FAIL "planted: state '$_s1' packages [$_p1]; removed: '$_s2' (want $WANT_RB)"
	fi
}

mut_fault_service() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	log ""
	_name=nx-host-check-$(rand_hex8)
	if [ "$INIT" = systemd ]; then
		note "== Step 7b: a failed transient unit (--with-faults)"
		F_UNIT=$_name
		mut_save_state
		CHANGED=1
		hrun "systemd-run --unit=$F_UNIT --quiet /bin/false; i=0; while [ \"\$(systemctl is-failed $F_UNIT.service)\" != failed ] && [ \$i -lt 50 ]; do sleep 0.2; i=\$((i + 1)); done; systemctl is-failed $F_UNIT.service"
		_want=$F_UNIT.service
	else
		note "== Step 7b: a crashed OpenRC service (--with-faults)"
		F_SVC=$_name
		mut_save_state
		CHANGED=1
		hrun "printf '#!/sbin/openrc-run\ncommand=/bin/sleep\ncommand_args=3600\ncommand_background=yes\npidfile=/run/$F_SVC.pid\n' >/etc/init.d/$F_SVC && chmod 0755 /etc/init.d/$F_SVC && rc-service $F_SVC start && sleep 1 && kill -9 \$(cat /run/$F_SVC.pid) && i=0; while ! rc-status --crashed | grep -qx $F_SVC && [ \$i -lt 50 ]; do sleep 0.2; i=\$((i + 1)); done; rc-status --crashed"
		_want=$F_SVC
	fi
	host_health_facts
	_hn=$(hvals failed | grep -c .)
	nx f-svc1 hosthealth get
	_n1=$(jlist "[0].properties.failedServices.names" | tr '\n' ' ') _c1=$(jv "[0].properties.failedServices.count")
	mut_fault_undo_service
	nx f-svc2 hosthealth get
	_n2=$(jlist "[0].properties.failedServices.names" | tr '\n' ' ')
	case " $_n1" in *" $_want "*) _in1=1 ;; *) _in1=0 ;; esac
	case " $_n2" in *" $_want "*) _in2=1 ;; *) _in2=0 ;; esac
	if [ "$_in1" = 1 ] && [ "$_c1" = "$_hn" ] && [ "$_in2" = 0 ]; then
		result 7b-service PASS "$_want in failedServices.names (count $_c1 = the host's $_hn); gone after the cleanup"
	else
		result 7b-service FAIL "planted: names [$_n1] count $_c1 (host $_hn); after the cleanup: [$_n2]"
	fi
}

mut_fault_undo_service() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	if [ -n "$F_UNIT" ]; then
		hrun "systemctl reset-failed $F_UNIT.service; systemctl is-failed $F_UNIT.service; true"
		F_UNIT=
	fi
	if [ -n "$F_SVC" ]; then
		hrun "rc-service $F_SVC zap; rm -f /etc/init.d/$F_SVC /run/$F_SVC.pid; rc-status --crashed; true"
		F_SVC=
	fi
	mut_save_state
}

# mut_cleanup: undo what this run (or, with --cleanup, an interrupted one)
# left: the marker it wrote, its dummy service, the saved configuration,
# the soak's token; then remove the state.
mut_cleanup() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	note "== Cleanup"
	CLEANED=1
	_keep=0
	if [ "$F_MARKER" = 1 ]; then
		hrun 'rm -f /var/run/reboot-required /var/run/reboot-required.pkgs; ls /var/run/reboot-required 2>&1; true'
		F_MARKER=0
	fi
	[ -z "$F_UNIT" ] && [ -z "$F_SVC" ] || mut_fault_undo_service
	if [ "$CONF_SAVED" = 1 ]; then
		mut_conf_restore || _keep=1
	fi
	hx "[ -d $H_STATE/xdg ]"
	[ "$HX_RC" -ne 0 ] || hrun "$NEXUSD token revoke $SOAK_TOKEN_NAME; rm -rf $H_STATE/xdg"
	if [ "$_keep" = 0 ]; then
		hrun "rm -rf $H_STATE"
		result cleanup PASS "no planted marker, dummy service or changed configuration is left; $H_STATE removed"
	else
		result cleanup FAIL "the configuration could not be restored; $H_STATE (with nexusd.yaml.orig) is kept"
	fi
	CHANGED=0
}

mut_conf_save() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	hx "sed -n '/^  host:\$/,/^  [a-z]/p' $CONF | grep -cE '^    (enabled|sampleInterval|historyDuration):'"
	[ "$HX_OUT" = 3 ] || refuse "$CONF has no block-style providers.host section with enabled, sampleInterval and historyDuration (as the default configuration has): edit the values by hand instead"
	hrun "umask 077 && mkdir -p $H_STATE && cp -p $CONF $H_STATE/nexusd.yaml.orig && sha256sum $CONF | cut -d' ' -f1 >$H_STATE/nexusd.yaml.sha256 && cat $H_STATE/nexusd.yaml.sha256"
	[ "$HX_RC" -eq 0 ] || refuse "could not save $CONF"
	CONF_SAVED=1
	CHANGED=1
	mut_save_state
}

# mut_conf_set ENABLED INTERVAL DURATION: those three keys of providers.host.
mut_conf_set() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	case $1 in true | false) ;; *) refuse "internal: enabled $(shown "$1")" ;; esac
	for _cv in "$2" "$3"; do
		case $_cv in '' | *[!0-9smh]*) refuse "internal: duration $(shown "$_cv")" ;; esac
	done
	hrun "sed -i -e '/^  host:\$/,/^  [a-z]/ s/^\\(    enabled:[[:space:]]*\\)[a-z]*/\\1$1/' -e '/^  host:\$/,/^  [a-z]/ s/^\\(    sampleInterval:[[:space:]]*\\)[0-9a-z]*/\\1$2/' -e '/^  host:\$/,/^  [a-z]/ s/^\\(    historyDuration:[[:space:]]*\\)[0-9a-z]*/\\1$3/' $CONF && $NEXUSD validate-config && sed -n '/^  host:\$/,/^  [a-z]/p' $CONF"
	[ "$HX_RC" -eq 0 ] || { mut_conf_restore; refuse "the edited configuration did not validate; the original is back"; }
}

mut_conf_restore() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	[ "$CONF_SAVED" = 1 ] || return 0
	hrun "cat $H_STATE/nexusd.yaml.orig >$CONF && [ \"\$(sha256sum $CONF | cut -d' ' -f1)\" = \"\$(cat $H_STATE/nexusd.yaml.sha256)\" ] && echo 'restored, sha256 equal to the saved one' && $NEXUSD validate-config"
	if [ "$HX_RC" -ne 0 ]; then
		result conf-restored FAIL "$CONF could not be restored from $H_STATE/nexusd.yaml.orig: restore it by hand and reload nexusd"
		return 1
	fi
	CONF_SAVED=0
	mut_nexusd reload
	result conf-restored PASS "$CONF restored byte for byte (sha256 $(hx "cat $H_STATE/nexusd.yaml.sha256" && printf '%s' "$HX_OUT")) and reloaded"
}

# mut_nexusd reload|restart: through the init system; then wait for the
# API, and refresh NXD_PID.
mut_nexusd() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	if [ "$INIT" = systemd ]; then hrun "systemctl $1 nexusd.service"; else hrun "rc-service nexusd $1"; fi
	_i=0
	while [ "$_i" -lt 30 ]; do
		sleep 1
		"$NEXUS_CLI" --host "$NXHOST" capability get -o json >/dev/null 2>&1 && break
		_i=$((_i + 1))
	done
	sleep 2
	nexusd_pid
	nx_quiet caps2 capability get
	_i=$(awk -F'\t' '$1 ~ /\.providers\[[0-9]+\]\.name$/ && $2 == "\"host\"" { sub(/\.name$/, "", $1); print $1; exit }' "$JF")
	log "  after the $1: nexusd pid $NXD_PID; host provider $(js "$_i.status"), sampleIntervalSeconds $(jv "$_i.details.sampleIntervalSeconds"), historyDurationSeconds $(jv "$_i.details.historyDurationSeconds")"
	CAP_STATUS=$(js "$_i.status")
}

# window SECONDS: nexusd's CPU (% of one CPU) and RSS (kB, every 60 s) over
# SECONDS: W_CPU, W_RSS_MAX, W_RSS_MIN, W_RSS_MEAN; 1 if nexusd restarted.
window() {
	proc_sample
	_u0=$P_UP _t0=$P_TICKS _p0=$P_PID
	_rs=$P_RSS
	log "  t=0 pid $P_PID ticks $P_TICKS VmRSS $P_RSS kB"
	_k=0
	while [ "$_k" -lt "$(($1 / 60))" ]; do
		sleep 60
		_k=$((_k + 1))
		proc_sample
		log "  t=$((_k * 60)) pid $P_PID ticks $P_TICKS VmRSS $P_RSS kB"
		_rs="$_rs $P_RSS"
	done
	hx 'getconf CLK_TCK 2>/dev/null || echo 100'
	_hz=$HX_OUT
	W_CPU=$(calc '100 * (a - b) / c / (d - e)' "$P_TICKS" "$_t0" "$_hz" "$P_UP" "$_u0")
	_w=$(printf '%s\n' "$_rs" | tr ' ' '\n' | awk 'NF { n++; s += $1; if (n == 1 || $1 > mx) mx = $1; if (n == 1 || $1 < mn) mn = $1 } END { if (n) printf "%d %d %.0f", mx, mn, s / n }')
	W_RSS_MAX=$(field "$_w" 1) W_RSS_MIN=$(field "$_w" 2) W_RSS_MEAN=$(field "$_w" 3)
	log "  CPU $(printf '%.3f' "$W_CPU")% of one CPU over $(calc 'a - b' "$P_UP" "$_u0") s (CLK_TCK $_hz); VmRSS max $W_RSS_MAX, min $W_RSS_MIN, mean $W_RSS_MEAN kB"
	[ "$_p0" = "$P_PID" ]
}

# history_full: 0 when the history covers historyDuration.
history_full() {
	nx_quiet hist hostmetric get --window-seconds 86400
	_hs=$(js "[0].properties.historyStart") _sa=$(js "[0].properties.sampler.sampledAt")
	H_SPAN=$(calc 'a - b' "$(epoch "$_sa")" "$(epoch "$_hs")")
	log "  historyStart $_hs, sampledAt $_sa: $(printf '%.0f' "$H_SPAN") s of ${CAP_HD:-3600} s; points $(jv "[0].properties.points"), stepSeconds $(jv "[0].properties.stepSeconds")"
	awk -v s="$H_SPAN" -v d="${CAP_HD:-3600}" -v i="${CAP_IV:-10}" 'BEGIN { exit !(s >= d - i) }'
}

mut_overhead() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	log ""
	note "== Step 8: NFR-2 at the defaults (--overhead, TC-HP-130)"
	if [ "$CAP_IV" != 10 ] || [ "$CAP_HD" != 3600 ]; then
		result 8-overhead NOT-RUN "the provider runs at ${CAP_IV}s/${CAP_HD}s, not the defaults 10s/1h: set them, reload, and wait an hour"
		return
	fi
	while ! history_full; do
		note "the 1 h history is not full yet: waiting 60 s"
		sleep 60
	done
	note "enabled, history full: $WINDOW s window"
	window "$WINDOW" || { result 8-overhead FAIL "nexusd restarted during the enabled window"; return; }
	_ecpu=$W_CPU _erss=$W_RSS_MAX
	mut_conf_save
	mut_conf_set false 10s 1h
	mut_nexusd restart
	[ "$CAP_STATUS" != enabled ] || { result 8-overhead FAIL "the provider is still enabled after the restart"; mut_conf_restore; return; }
	note "disabled, after a restart: 60 s to settle, then a $WINDOW s window"
	sleep 60
	window "$WINDOW" || { result 8-overhead FAIL "nexusd restarted during the disabled window"; mut_conf_restore; return; }
	_dcpu=$W_CPU _drss=$W_RSS_MEAN
	mut_conf_restore
	_d=$(calc 'a - b' "$_ecpu" "$_dcpu")
	if awk -v d="$_d" 'BEGIN { exit !(d < 1) }'; then
		result 8-cpu PASS "enabled $(printf '%.3f' "$_ecpu")% - disabled $(printf '%.3f' "$_dcpu")% = $(printf '%.3f' "$_d")% of one CPU over $WINDOW s each (< 1%)"
	else
		result 8-cpu FAIL "enabled $_ecpu% - disabled $_dcpu% = $_d% of one CPU (want < 1%)"
	fi
	_rd=$(calc '(a - b) / 1024' "$_erss" "$_drss")
	if awk -v d="$_rd" 'BEGIN { exit !(d < 8) }'; then
		result 8-rss-1h PASS "VmRSS max $_erss kB enabled (1 h history full) - mean $_drss kB disabled = $(printf '%.2f' "$_rd") MiB (< 8 MiB)"
	else
		result 8-rss-1h FAIL "VmRSS max $_erss kB enabled - mean $_drss kB disabled = $_rd MiB (want < 8 MiB)"
	fi
}

# The poll on the host during the 24 h fill: hostsummary, hosthealth and a
# 24 h hostmetric every 10 s, and nexusd's VmRSS and ticks every 60 s.
poller_script() {
	# shellcheck disable=SC2016 # Expanded on the host, by the poller.
	printf '%s\n' '#!/bin/sh' 'S='"$H_STATE" 'NX='"$HOST_NEXUS" 'export XDG_CONFIG_HOME=$S/xdg' \
		'end=$(($(date +%s) + 93600)); next=0; ok=0; bad=0' \
		'pid() { for d in /proc/[0-9]*; do case $(tr "\0" " " 2>/dev/null <$d/cmdline) in "/usr/local/bin/nexusd serve "*) echo ${d#/proc/}; return ;; esac; done; }' \
		'while [ ! -e $S/stop ] && [ -d $S ] && [ "$(date +%s)" -lt $end ]; do' \
		'  for c in "hostsummary get" "hosthealth get" "hostmetric get --window-seconds 86400"; do' \
		'    if $NX --host soak $c -o json >/dev/null 2>&1; then ok=$((ok + 1)); else bad=$((bad + 1)); fi' \
		'  done' \
		'  now=$(date +%s)' \
		'  if [ $now -ge $next ]; then p=$(pid); s=$(cat /proc/$p/stat 2>/dev/null); s=${s##*") "}; set -- $s; echo "$now $(cut -d" " -f1 /proc/uptime) $p $(sed -n "s/^VmRSS:[[:space:]]*\([0-9]*\).*/\1/p" /proc/$p/status) $((${12} + ${13})) $ok $bad" >>$S/rss.log; next=$((now + 60)); fi' \
		'  sleep 10' \
		'done'
}

mut_rss_start() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	log ""
	note "== Step 9: NFR-2 at the 24 h maximum, start (--rss-24h start, TC-HP-130)"
	if [ -z "$HOST_NEXUS" ]; then
		hx 'command -v nexus || { [ -x /usr/local/bin/nexus ] && echo /usr/local/bin/nexus; }'
		HOST_NEXUS=$(printf '%s\n' "$HX_OUT" | head -n 1)
	fi
	valid_path "$HOST_NEXUS" || refuse "no nexus CLI on the host (install it from the release, or pass --host-nexus PATH): the poll runs there"
	hrun "$(shown "$HOST_NEXUS") --version" || refuse "$HOST_NEXUS does not run on the host"
	mut_conf_save
	mut_conf_set false 10s 1h
	mut_nexusd restart
	[ "$CAP_STATUS" != enabled ] || { mut_conf_restore; refuse "the provider is still enabled after the restart"; }
	note "disabled baseline: 60 s to settle, then $BASELINE s"
	sleep 60
	window "$BASELINE" || { mut_conf_restore; refuse "nexusd restarted during the baseline"; }
	RSS_BASE=$W_RSS_MEAN
	RSS_BASE_CPU=$(printf '%.4f' "$W_CPU")
	mut_conf_set true 2s 24h
	mut_nexusd reload
	[ "$CAP_STATUS" = enabled ] || { mut_conf_restore; refuse "the provider is not enabled after the reload"; }
	hx "sed -n 's/^listen:[[:space:]]*\\[[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' $CONF | head -n 1"
	_addr=${HX_OUT:-127.0.0.1:7443}
	_port=${_addr##*:} _h=${_addr%:*}
	case $_h in 0.0.0.0 | '' | '[::]') _h=127.0.0.1 ;; esac
	valid_num "$_port" || { mut_conf_restore; refuse "nexusd's listen port is not a number: $(shown "$_port")"; }
	# The soak's own token: created on the host and handed to the host's
	# nexus CLI on a pipe, never printed or carried here.
	hrun "$NEXUSD token revoke $SOAK_TOKEN_NAME >/dev/null 2>&1; mkdir -p $H_STATE/xdg && chmod 0700 $H_STATE/xdg && $NEXUSD token create --name $SOAK_TOKEN_NAME --scope host:read --expires 2d 2>/dev/null | XDG_CONFIG_HOME=$H_STATE/xdg $(shown "$HOST_NEXUS") host add soak --url https://$(shown "$_h"):$_port --fingerprint \"\$($NEXUSD tls fingerprint)\" --token-stdin 2>&1 | tail -n 1"
	[ "$HX_RC" -eq 0 ] || { mut_conf_restore; refuse "could not set up the soak's client on the host"; }
	poller_script >"$TMPD/poller.sh"
	hx_in "cat >$H_STATE/poller.sh && chmod 0700 $H_STATE/poller.sh" <"$TMPD/poller.sh"
	log "\$ [host] (the poll, $H_STATE/poller.sh):"
	sed 's/^/    | /' "$TMPD/poller.sh" | tee -a "$TRANSCRIPT"
	hrun "rm -f $H_STATE/stop $H_STATE/rss.log; nohup sh $H_STATE/poller.sh </dev/null >/dev/null 2>&1 & echo \$!"
	POLLER=$HX_OUT
	valid_num "$POLLER" || { mut_conf_restore; refuse "the poll did not start"; }
	hx 'date +%s'
	RSS_START=$HX_OUT
	valid_num "$RSS_START" || { log "WARNING: the host's date +%s is not a number: $(shown "$RSS_START"); finish will record the soak as not run"; RSS_START=; }
	mut_save_state
	sleep 15
	hrun "cat $H_STATE/rss.log"
	CHANGED=0 # the soak stays on purpose; --rss-24h finish ends it
	result 9-rss-24h-start PASS "baseline (provider disabled, after a restart): VmRSS mean $RSS_BASE kB, CPU $RSS_BASE_CPU%; historyDuration 24h, sampleInterval 2s since $(date -u +%Y-%m-%dT%H:%M:%SZ) (epoch $RSS_START on the host); poll pid $POLLER. Finish after 24 h: sh $PROG --rss-24h finish --yes${SSH_TARGET:+ --ssh $SSH_TARGET}"
}

mut_rss_finish() {
	mutating_allowed || refuse "internal: a host change outside an opt-in mode"
	log ""
	note "== Step 9: NFR-2 at the 24 h maximum, finish (--rss-24h finish, TC-HP-130)"
	hx 'date +%s'
	_now=$HX_OUT
	if valid_num "$_now" && valid_num "$RSS_START"; then
		_el=$((_now - RSS_START))
	else
		log "  the start ($(shown "$RSS_START")) or the host's time now ($(shown "$_now")) is not a number: elapsed taken as 0"
		_el=0
	fi
	log "  started at epoch $RSS_START, now $_now: $(calc 'a / 3600' "$_el") h elapsed"
	nx_quiet hist hostmetric get --window-seconds 86400
	log "  hostmetric: historyStart $(js "[0].properties.historyStart"), points $(jv "[0].properties.points"), stepSeconds $(jv "[0].properties.stepSeconds"), sampler $(js "[0].properties.sampler.state")"
	# The stop file ends the poll within 10 s. The pid is signalled only if
	# it is still the poll (its command line, read on the host): after the
	# poll ended, or a reboot, the number may be another process's.
	valid_num "$POLLER" || POLLER=0
	hrun "touch $H_STATE/stop; sleep 12; case \$(tr '\\0' ' ' 2>/dev/null </proc/$POLLER/cmdline) in 'sh $H_STATE/poller.sh '*) kill $POLLER ;; *) echo 'the poll (pid $POLLER) has ended' ;; esac; wc -l <$H_STATE/rss.log; tail -n 3 $H_STATE/rss.log"
	hx "cat $H_STATE/rss.log"
	printf '%s\n' "$HX_OUT" >"$TMPD/rss.log"
	log "  rss.log (epoch uptime pid VmRSS-kB ticks polls-ok polls-failed), every line:"
	sed 's/^/    /' "$TMPD/rss.log" | tee -a "$TRANSCRIPT"
	_rl=$(awk 'NF >= 7 { l = $1; r[NR] = $4; e[NR] = $1; p[NR] = $3; n = NR } END { mx = 0; for (i = 1; i <= n; i++) if (e[i] >= l - 600 && r[i] > mx) mx = r[i]; pc = 0; for (i = 2; i <= n; i++) if (p[i] != p[1]) pc = 1; print mx, n, pc }' "$TMPD/rss.log")
	_mx=$(field "$_rl" 1) _nl=$(field "$_rl" 2) _pc=$(field "$_rl" 3)
	valid_num "$_mx" || _mx=0
	valid_num "$_nl" || _nl=0
	_last=$(tail -n 1 "$TMPD/rss.log")
	_d=$(calc '(a - b) / 1024' "$_mx" "$RSS_BASE")
	log "  maximum VmRSS over the last 10 minutes $_mx kB; baseline $RSS_BASE kB; delta $(printf '%.2f' "$_d") MiB; $_nl samples; polls ok/failed: $(printf '%s\n' "$_last" | cut -d' ' -f6,7)"
	hrun "$NEXUSD token revoke $SOAK_TOKEN_NAME; rm -rf $H_STATE/xdg"
	mut_conf_restore
	if [ "$_pc" = 1 ]; then
		result 9-rss-24h FAIL "nexusd's PID changed during the soak (it restarted)"
	elif [ "$_el" -lt 86400 ]; then
		result 9-rss-24h NOT-RUN "the history was not full: $(printf '%.2f' "$(calc 'a / 3600' "$_el")") h of 24 h. So far: max VmRSS over the last 10 min $_mx kB - baseline $RSS_BASE kB = $(printf '%.2f' "$_d") MiB"
	elif awk -v d="$_d" 'BEGIN { exit !(d < 32) }'; then
		result 9-rss-24h PASS "24 h history full; max VmRSS over the last 10 min $_mx kB - baseline $RSS_BASE kB = $(printf '%.2f' "$_d") MiB (< 32 MiB)"
	else
		result 9-rss-24h FAIL "max VmRSS over the last 10 min $_mx kB - baseline $RSS_BASE kB = $_d MiB (want < 32 MiB)"
	fi
	hrun "rm -rf $H_STATE"
	CONF_SAVED=0
}

# --- exit ------------------------------------------------------------------

token_in() {
	{
		printf '%s\n' "$CHECK_TOKEN"
		cat "$@"
	} | awk 'NR == 1 { t = $0; next } index($0, t) { f = 1 } END { exit !f }'
}

token_check() {
	[ -n "$CHECK_TOKEN" ] || return 0
	if token_in "$TRANSCRIPT"; then
		result token-absent FAIL "the token appears in the transcript: delete it and revoke the token"
	else
		result token-absent PASS "the token does not appear in the transcript"
	fi
}

token_env_check() {
	[ -n "$CHECK_TOKEN" ] || return 0
	if env | token_in; then
		result token-env FAIL "a child process's environment holds the token"
	else
		result token-env PASS "no child process's environment holds the token (env, run as a child, does not show it)"
	fi
}

summary() {
	log ""
	log "== Summary ($MODE$([ "$FAULTS" = 0 ] || echo ' --with-faults')$([ "$OVERHEAD" = 0 ] || echo ' --overhead'))"
	printf '%s\n' "$RESULTS" | while IFS='|' read -r id st detail; do
		[ -z "$id" ] || log "  $id  $st  $detail"
	done
	[ "$IN_CONTAINER" = 0 ] || log "  (container: not real-host evidence)"
	log "transcript: $TRANSCRIPT"
}

on_exit() {
	_rc=$?
	trap - EXIT
	trap '' INT TERM HUP
	if [ "$CHANGED" = 1 ] && [ "$CLEANED" = 0 ]; then
		note "stopped early (exit $_rc): undoing this run's changes"
		mut_cleanup
		token_check
		summary
	fi
	rm -rf "$TMPD"
	exit "$_rc"
}

# --- main ------------------------------------------------------------------

trap on_exit EXIT
trap 'exit 130' INT TERM HUP

host_root
header
detect_init
setup_client_rc=0
setup_client || setup_client_rc=1
token_env_check
[ "$setup_client_rc" = 0 ] || [ "$YES" = 0 ] || refuse "NEXUS_TOKEN is not set (or pass --nexus-host). Create one on the host: nexusd token create --name host-check --scope host:read --expires 1d"

hx "[ -e $H_STATE/state ]"
_state=$HX_RC
case $MODE in
cleanup | rss-finish)
	[ "$_state" -eq 0 ] && load_state || refuse "no $H_STATE/state: nothing to $MODE"
	log "state ($H_STATE/state):"
	printf '%s\n' "$STATE_TEXT" | sed 's/^/  /' | tee -a "$TRANSCRIPT"
	;;
*)
	if [ "$_state" -eq 0 ]; then
		load_state
		case $STATE_MODE in
		rss-start)
			[ "$MODE" = check ] && [ "$FAULTS" = 0 ] && [ "$OVERHEAD" = 0 ] ||
				refuse "a 24 h soak is running (started at epoch $RSS_START): finish it first with --rss-24h finish"
			log "note: a 24 h soak is running on this host (started at epoch $RSS_START); this read-only check runs alongside it"
			;;
		*) refuse "an earlier run did not finish ($H_STATE/state, mode $STATE_MODE): run with --yes --cleanup first" ;;
		esac
	fi
	;;
esac

facts
plan
if [ "$YES" != 1 ]; then
	log ""
	refuse "nothing was changed: re-run with --yes to do the above"
fi
[ "$PROVIDER_OK" = 1 ] || [ "$MODE" = cleanup ] || [ "$MODE" = rss-finish ] || refuse "the host provider is not enabled under $INIT (1-provider)"

case $MODE in
cleanup)
	case $STATE_MODE in rss-start) refuse "the state is a running 24 h soak: end it with --rss-24h finish" ;; esac
	CHANGED=1
	mut_cleanup
	;;
rss-start)
	mut_rss_start
	;;
rss-finish)
	CONF_SAVED=1
	mut_rss_finish
	;;
check)
	check_all
	check_nfr3
	check_nfr1
	if [ "$FAULTS" = 1 ]; then
		mut_fault_reboot
		mut_fault_service
	fi
	[ "$OVERHEAD" = 0 ] || mut_overhead
	;;
esac
[ "$CHANGED" = 0 ] || mut_cleanup
token_check
summary
[ "$FAILS" -eq 0 ] || exit 1
[ "$INCONCL" -eq 0 ] || exit 3
exit 0
