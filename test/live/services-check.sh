#!/bin/sh
# Live check of the services provider (services-provider WI 6.1): TC-SV-L1
# to TC-SV-L6 and TC-SV-L13, and the read-only boot-ID checks of WI 6.1 (f),
# on home-server (Ubuntu VM, systemd), pihole (Debian 12 LXC, systemd) and
# Public-Nginx (Alpine LXC, OpenRC), each running the release candidate.
# It drives the real `nexus` CLI against the local nexusd and reads every
# result back without Nexus, from the host's own tools: systemctl,
# journalctl, rc-status, rc-update, rc-service, /run/openrc and /proc. It is
# POSIX sh: Debian's dash and BusyBox ash run it.
#
# It never reboots anything. It sends no reboot, no machine restart and no
# reboot what-if, and it does not enable the reboot (the user chose on
# 2026-11-18 to skip the real reboots; WI 6.2 and 6.3 are owed).
#
# Run it on the host itself, as root, with the linux `nexus` CLI copied there:
#   sh services-check.sh --test-service nx-live-test
#             the facts and the plan; changes nothing, exits 2
#   NEXUS_TOKEN=... sh services-check.sh --test-service nx-live-test --yes
#             the check
#   sh services-check.sh --print-test-service nx-live-test
#             how to install (and later remove) the harmless test service
#
# What it changes on the host: only the state of the test service named by
# --test-service, through `nexus service start|stop|restart`, and only when
# that service is allow-listed, not protected, and runs `sleep` (it refuses
# otherwise). It leaves it in the state it found it in. It also sends the
# FR-9 probes (`restart nexusd`, `stop ssh`, `stop sshd`, `stop
# <selfService>`) that nexusd must refuse with 409 protected-service; each
# is sent only after capabilities list the name as protected and the
# allow-list does not name it, so nexusd's own allow-list check would
# refuse it too. Every host command it runs reads.
#
# The procedure and the transcript template are in
# docs/verification/services-live.md. Every command and its output go to the
# transcript file (--transcript FILE). NEXUS_TOKEN is never written to it.

# Functions run through the EXIT trap, which shellcheck cannot follow
# (SC2317, SC2329). `[ test ] && ok ... || bad ...` is an if-then-else
# here, because ok and bad always return 0 (SC2015).
# shellcheck disable=SC2317,SC2329,SC2015
set -u
# Data from the host and from nexusd is never code here: no pathname
# expansion (set -f; host commands run in their own sh, which globs as
# usual), a line is split into fields by awk, and a value reaches awk only
# through -v or a file, never through program text.
set -f
umask 022
LC_ALL=C
export LC_ALL

PROG=services-check.sh

# The token must not stay in any environment (see host-check.sh): the
# script runs itself again, once, without NEXUS_TOKEN, and reads the token
# from file descriptor 3. From then on it is an unexported variable that
# reaches only `nexus host add --token-stdin` and the transcript check.
if [ -n "${NEXUS_TOKEN:-}" ] && [ -z "${SERVICES_CHECK_TOKEN_FD:-}" ]; then
	if [ ! -r "$0" ]; then
		echo "$PROG: REFUSED: run it as a file (sh services-check.sh), not from stdin, so it can drop NEXUS_TOKEN from its environment" >&2
		exit 1
	fi
	unset CHECK_TOKEN
	CHECK_TOKEN=$NEXUS_TOKEN
	unset NEXUS_TOKEN
	SERVICES_CHECK_TOKEN_FD=3
	export SERVICES_CHECK_TOKEN_FD
	exec sh "$0" "$@" 3<<EOF
$CHECK_TOKEN
EOF
fi
unset CHECK_TOKEN
CHECK_TOKEN=
if [ "${SERVICES_CHECK_TOKEN_FD:-}" = 3 ]; then
	unset SERVICES_CHECK_TOKEN_FD
	IFS= read -r CHECK_TOKEN <&3 || CHECK_TOKEN=
	exec 3<&-
fi

CONF=/etc/nexus/nexusd.yaml
NEXUSD=/usr/local/bin/nexusd
OPS_DIR=/var/lib/nexus/ops
BOOT_ID_PATH=/proc/sys/kernel/random/boot_id
# The built-in protected lists (docs/services.md, Protected services).
STATIC_SYSTEMD='ssh sshd dbus dbus-broker systemd-journald systemd-logind systemd-networkd systemd-resolved NetworkManager networking nexusd getty@* serial-getty@* nexus-op-*'
STATIC_OPENRC='sshd dropbear networking syslog udev devfs dmesg localmount nexusd'

usage() {
	cat <<'EOF'
Usage: sh services-check.sh [--test-service NAME] [--yes] [OPTIONS]
       sh services-check.sh --print-test-service NAME

Without --yes it records the facts (versions, nexusd's PID, the services
provider's configuration and capabilities) and prints the plan. It changes
nothing and exits 2. With --yes it runs the check: read-only on the host,
except start, stop and restart of the --test-service service.

It never reboots: no reboot, machine restart or reboot what-if is sent.

Options:
  --test-service NAME   the harmless, allow-listed test service (a `sleep`
                        unit or OpenRC script you installed). Without it the
                        control check (TC-SV-L4) is NOT-RUN
  --print-test-service NAME
                        print how to install, allow-list and later remove
                        NAME on this host's init system, and exit
  --nexus PATH          the nexus CLI (default: nexus on PATH, else ./nexus
                        next to this script, else /usr/local/bin/nexus)
  --nexus-host NAME     use the host NAME already configured in your nexus
                        client on this machine (no NEXUS_TOKEN needed)
  --calls N             timed calls per read for NFR-1 (default 20, at
                        least 20)
  --transcript FILE     where to write the transcript (default:
                        ./services-check-<host>-<UTC time>.txt)
  -h, --help            this text

Environment:
  NEXUS_TOKEN  a token with services:read, services:logs, services:write,
               host:read and audit:read (not services:reboot: no reboot is
               sent). Create it on the host, and revoke it afterwards:
                 nexusd token create --name services-check --scope services:read --scope services:logs --scope services:write --scope host:read --scope audit:read --expires 1d
                 nexusd token revoke services-check
  NEXUS_URL    nexusd's URL (default: https:// and the first listen
               address in /etc/nexus/nexusd.yaml, 0.0.0.0 read as
               127.0.0.1). The certificate is pinned to
               `nexusd tls fingerprint`.

Exit status: 0 every check passed; 1 a check failed; 2 refused (a
precondition failed, or --yes is missing); 3 nothing failed but a check was
not run (NOT-RUN, such as TC-SV-L4 without --test-service).
EOF
}

YES=0
TEST_SVC=
PRINT_SVC=
NXHOST_GIVEN=
NEXUS_CLI=
CALLS=20
TRANSCRIPT=
ARGS="$*"
while [ $# -gt 0 ]; do
	case $1 in
	--yes) YES=1 ;;
	--test-service | --print-test-service | --nexus | --nexus-host | --calls | --transcript)
		{ [ $# -ge 2 ] && [ -n "$2" ]; } || { echo "$PROG: $1 needs a value" >&2; exit 2; }
		case $1 in
		--test-service) TEST_SVC=$2 ;;
		--print-test-service) PRINT_SVC=$2 ;;
		--nexus) NEXUS_CLI=$2 ;;
		--nexus-host) NXHOST_GIVEN=$2 ;;
		--calls) CALLS=$2 ;;
		--transcript) TRANSCRIPT=$2 ;;
		esac
		shift
		;;
	-h | --help) usage; exit 0 ;;
	*) echo "$PROG: unknown argument: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done
case $CALLS in '' | *[!0-9]*) echo "$PROG: --calls takes a whole number: $CALLS" >&2; exit 2 ;; esac
[ "$CALLS" -ge 20 ] && [ "$CALLS" -le 1000 ] || { echo "$PROG: --calls must be 20 to 1000, for a p95" >&2; exit 2; }

# valid_name: the provider's service name pattern
# (^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$), not ending in .service.
valid_name() {
	case $1 in '' | [!A-Za-z0-9]* | *[!A-Za-z0-9@._-]* | *.service) return 1 ;; esac
	[ "${#1}" -le 64 ]
}
for _n in "$TEST_SVC" "$PRINT_SVC"; do
	[ -z "$_n" ] || valid_name "$_n" || { echo "$PROG: not a valid service name (^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}\$, without .service): $_n" >&2; exit 2; }
done

detect_init_local() {
	if [ -d /run/systemd/system ]; then
		echo systemd
	elif [ -e /run/openrc ] && [ -x /sbin/openrc-run ]; then
		echo openrc
	fi
}

# --print-test-service: the install and removal steps, for the operator to
# run by hand. Nothing here is run by the script.
if [ -n "$PRINT_SVC" ]; then
	_init=$(detect_init_local)
	N=$PRINT_SVC
	case $_init in
	systemd)
		cat <<EOF
# Install the harmless test service $N on this systemd host (run by hand, as root):
cat >/etc/systemd/system/$N.service <<'UNIT'
[Unit]
Description=Nexus live check test service (harmless: it only sleeps)

[Service]
Type=simple
ExecStart=/bin/sleep infinity
UNIT
systemctl daemon-reload
EOF
		;;
	openrc)
		cat <<EOF
# Install the harmless test service $N on this OpenRC host (run by hand, as root):
cat >/etc/init.d/$N <<'SCRIPT'
#!/sbin/openrc-run
description="Nexus live check test service (harmless: it only sleeps)"
supervisor=supervise-daemon
command=/bin/sleep
command_args=86400
SCRIPT
chmod 0755 /etc/init.d/$N
EOF
		;;
	*)
		echo "$PROG: this host runs neither systemd nor OpenRC" >&2
		exit 2
		;;
	esac
	cat <<EOF

# Allow-list it: add this entry under providers.services in $CONF
# (keep any entries already there), then check the file and reload:
#   providers:
#     services:
#       enabled: true
#       control:
#         - name: $N
nexusd validate-config
EOF
	if [ "$_init" = systemd ]; then echo "systemctl reload nexusd"; else echo "rc-service nexusd reload"; fi
	cat <<EOF

# Afterwards, remove it: take the entry out of $CONF, then
nexusd validate-config
EOF
	if [ "$_init" = systemd ]; then
		echo "systemctl reload nexusd"
		echo "systemctl stop $N.service; rm /etc/systemd/system/$N.service; systemctl daemon-reload"
	else
		echo "rc-service nexusd reload"
		echo "rc-service $N stop; rm /etc/init.d/$N"
	fi
	exit 0
fi

if [ -z "$TRANSCRIPT" ]; then
	TRANSCRIPT=./services-check-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ).txt
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
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/services-check.XXXXXX") || { echo "$PROG: mktemp failed" >&2; exit 2; }

FAILS=0
INCONCL=0
RESULTS=
NXHOST=
INIT=
DOCKER=0
LXC=0
NXD_PID=
LOGS_CALLS=0
LOGS_OK=0
REFUSALS_SENT=0
CTL_SENT=
NX_TIMEOUT=60
TOKEN_ID=

# --- transcript ----------------------------------------------------------

log() { printf '%s\n' "$*" | tee -a "$TRANSCRIPT"; }
note() { log "[$(date -u +%H:%M:%S)] $*"; }
logf() { tee -a "$TRANSCRIPT" <"$1"; }

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

valid_num() { case $1 in '' | *[!0-9]*) return 1 ;; esac; }

# word_in WORD LIST: WORD is one of LIST's blank-separated words, a word
# ending in * matching every name that starts with the part before it.
word_in() {
	for _w in $2; do
		case $_w in
		*\*) case $1 in "${_w%\*}"*) return 0 ;; esac ;;
		*) [ "$1" = "$_w" ] && return 0 ;;
		esac
	done
	return 1
}

# --- the host ------------------------------------------------------------

# hx CMD: run the shell command CMD on this host, unlogged. HX_OUT holds its
# output (stdout and stderr), HX_RC its status.
hx() {
	HX_OUT=$(sh -c "$1" 2>&1 </dev/null)
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

# hkv KEY: the value of the first line KEY=... in HX_OUT.
hkv() { printf '%s\n' "$HX_OUT" | awk -v k="$1=" 'index($0, k) == 1 { print substr($0, length(k) + 1); exit }'; }

# hfile NAME CMD: run CMD, unlogged, its stdout to TMPD/NAME; logged as one
# line with its line count. HX_RC is its status.
hfile() {
	sh -c "$2" >"$TMPD/$1" 2>"$TMPD/$1.err" </dev/null
	HX_RC=$?
	log "\$ [host] $2 ($(wc -l <"$TMPD/$1" | tr -d ' ') lines, kept)"
	[ ! -s "$TMPD/$1.err" ] || sed 's/^/(stderr) /' "$TMPD/$1.err" | tee -a "$TRANSCRIPT"
	[ "$HX_RC" -eq 0 ] || log "(exit $HX_RC)"
}

# --- the nexus CLI -------------------------------------------------------

# with_timeout SECONDS CMD...: CMD, killed with SIGTERM after SECONDS. It
# ends this script's own CLI child, never a process of the host's.
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

# nx NAME ARG...: the nexus CLI with -o json, logged with its output,
# bounded by NX_TIMEOUT seconds. Output in TMPD/NAME.json (flattened into
# TMPD/NAME.flat, which JF names), stderr in TMPD/NAME.err. NX_RC is the
# status.
nx() {
	_name=$1
	shift
	log "\$ $(shown nexus --host "$NXHOST" "$@" -o json)"
	nx_exec "$_name" "$@"
	tee -a "$TRANSCRIPT" <"$TMPD/$_name.json"
	nx_after "$_name"
}

# nx_quiet NAME ARG...: as nx, logged on one line without the output.
nx_quiet() {
	_name=$1
	shift
	log "\$ $(shown nexus --host "$NXHOST" "$@" -o json) (output kept, not logged)"
	nx_exec "$_name" "$@"
	nx_after "$_name"
}

nx_exec() {
	_name=$1
	shift
	with_timeout "$NX_TIMEOUT" "$NEXUS_CLI" --host "$NXHOST" "$@" -o json >"$TMPD/$_name.json" 2>"$TMPD/$_name.err"
	NX_RC=$?
}

nx_after() {
	[ ! -s "$TMPD/$1.err" ] || sed 's/^/(stderr) /' "$TMPD/$1.err" | tee -a "$TRANSCRIPT"
	[ "$NX_RC" -eq 0 ] || log "(exit $NX_RC)"
	jflat <"$TMPD/$1.json" >"$TMPD/$1.flat"
	JF=$TMPD/$1.flat
	return "$NX_RC"
}

# jflat: the CLI's indented JSON (one key or value per line) as
# "PATH<TAB>VALUE" lines, such as "[0].properties.state<TAB>"running"".
# VALUE is the scalar as printed (a string keeps its quotes), or [] or {}.
jflat() {
	awk '
	{
		l = $0
		sub(/^[ \t]+/, "", l)
		sub(/,$/, "", l)
		if (l == "") next
		if (l == "}" || l == "]") { d--; next }
		if (d > 0 && ty[d] == "o") {
			if (!match(l, /^"[^"]+": /)) { print "jflat: unexpected line: " l > "/dev/stderr"; next }
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
# jso PATH: js, but empty for an absent key (an optional field).
jso() { js "$1" | sed 's/^<absent>$//'; }
# jlist PREFIX: the string items of the list at PREFIX, one per line.
jlist() { awk -F'\t' -v p="$1" 'index($1, p "[") == 1 && substr($1, length(p) + 2) ~ /^[0-9]+\]$/ { v = $2; gsub(/^"|"$/, "", v); print v }' "$JF"; }

# The JSON string unescape, for awk programs that read flattened values.
JUNQ='function junq(s,   o, i, n, c) {
	if (s == "null") return ""
	sub(/^"/, "", s); sub(/"$/, "", s)
	o = ""; n = length(s)
	for (i = 1; i <= n; i++) {
		c = substr(s, i, 1)
		if (c == "\\" && i < n) {
			i++; c = substr(s, i, 1)
			if (c == "n") c = "\n"; else if (c == "t") c = "\t"; else if (c == "r") c = "\r"
			else if (c == "u") { c = "\\u" substr(s, i + 1, 4); i += 4 }
		}
		o = o c
	}
	return o
}'

# slug_in NAME SLUG: the CLI's stderr for NAME names the problem SLUG.
slug_in() { grep -q "$2" "$TMPD/$1.err"; }

# setup_client: NXHOST, the host the CLI targets. With --nexus-host, the
# operator's own client config; otherwise a throwaway one in TMPD, with the
# certificate pinned to what `nexusd tls fingerprint` prints and the token
# on stdin (never in an argument or the transcript).
setup_client() {
	if [ -z "$NEXUS_CLI" ]; then
		if command -v nexus >/dev/null 2>&1; then
			NEXUS_CLI=nexus
		elif [ -x "$(dirname "$0")/nexus" ]; then
			NEXUS_CLI=$(dirname "$0")/nexus
		else
			NEXUS_CLI=/usr/local/bin/nexus
		fi
	fi
	command -v "$NEXUS_CLI" >/dev/null 2>&1 || [ -x "$NEXUS_CLI" ] ||
		refuse "the nexus CLI ($NEXUS_CLI) is not found: copy the linux nexus binary from the release next to this script, or pass --nexus PATH"
	if [ -n "$NXHOST_GIVEN" ]; then
		NXHOST=$NXHOST_GIVEN
		note "nexus CLI: the configured host $NXHOST (your client config)"
		return
	fi
	[ -n "$CHECK_TOKEN" ] || return 1
	# The token's ID (nxs_<id>_<secret>): it names this run's audit records.
	_t=${CHECK_TOKEN#nxs_}
	_t=${_t%%_*}
	case $_t in '' | *[!0-9a-f]*) ;; *) TOKEN_ID=$_t ;; esac
	_url=${NEXUS_URL:-}
	if [ -z "$_url" ]; then
		hx "sed -n 's/^listen:[[:space:]]*\\[[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' $CONF | head -n 1"
		_addr=${HX_OUT:-127.0.0.1:7443}
		_port=${_addr##*:} _host=${_addr%:*}
		case $_host in 0.0.0.0 | '' | '[::]') _host=127.0.0.1 ;; esac
		_url=https://$_host:$_port
	fi
	case $_url in
	https://*) ;;
	*) refuse "NEXUS_URL must start with https://: $(shown "$_url")" ;;
	esac
	XDG_CONFIG_HOME="$TMPD/xdg"
	export XDG_CONFIG_HOME
	mkdir -p "$XDG_CONFIG_HOME" && chmod 0700 "$XDG_CONFIG_HOME"
	NXHOST=services-check
	hrun "$NEXUSD tls fingerprint" || refuse "could not read nexusd's certificate fingerprint"
	_fp=$HX_OUT
	log "\$ (the token, on stdin) | $(shown nexus host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin)"
	printf '%s\n' "$CHECK_TOKEN" | "$NEXUS_CLI" host add "$NXHOST" --url "$_url" --fingerprint "$_fp" --token-stdin >"$TMPD/hostadd" 2>&1
	_rc=$?
	sed 's#[^ ]*/xdg/nexus/config.yaml#<throwaway client config>#' "$TMPD/hostadd" | tee -a "$TRANSCRIPT"
	[ "$_rc" -eq 0 ] || refuse "nexus host add failed (exit $_rc)"
}

# --- facts -----------------------------------------------------------------

header() {
	log "# $PROG transcript: services-provider WI 6.1 (TC-SV-L1 to L6, L13, and (f) the boot-ID source)"
	log "# no reboot, machine restart or reboot what-if is sent (WI 6.2 and 6.3 skipped by the user on 2026-11-18)"
	log "# started $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(hostname), arguments: ${ARGS:-(none)}"
	log "# script sha256 $( (command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256) <"$0" | cut -d' ' -f1)"
	[ "$(id -u)" = 0 ] || refuse "run this as root on the host (Alpine has no sudo by default: su -)"
	hx '[ -e /.dockerenv ] || [ -e /run/.containerenv ] || tr "\0" "\n" </proc/1/environ | grep -qE "^container=(docker|podman|oci)$"'
	if [ "$HX_RC" -eq 0 ]; then
		DOCKER=1
		log "# DOCKER/PODMAN CONTAINER DETECTED (/.dockerenv, /run/.containerenv or container=docker|podman in PID 1's environment):"
		log "# this transcript is a dry run, NOT real-host evidence."
		result host FAIL "a Docker/Podman container: a dry run, not real-host evidence"
	fi
	hx 'tr "\0" "\n" </proc/1/environ | grep -qx container=lxc'
	if [ "$HX_RC" -eq 0 ]; then
		LXC=1
		log "# LXC guest (container=lxc in PID 1's environment), as pihole and Public-Nginx are."
	fi
}

detect_init() {
	INIT=$(detect_init_local)
	case $INIT in
	systemd | openrc) ;;
	*) refuse "the host runs neither systemd nor OpenRC" ;;
	esac
}

# nexusd_pid: NXD_PID, the PID of `/usr/local/bin/nexusd serve`, from /proc.
nexusd_pid() {
	# shellcheck disable=SC2016 # Expanded on the host.
	hx 'for d in /proc/[0-9]*; do case $(tr "\0" " " 2>/dev/null <$d/cmdline) in "/usr/local/bin/nexusd serve"*) echo "${d#/proc/}" ;; esac; done'
	NXD_PID=$(printf '%s\n' "$HX_OUT" | head -n 1)
	valid_num "$NXD_PID" || NXD_PID=
}

facts() {
	note "== Step 1: facts"
	# shellcheck disable=SC2016 # Expanded on the host.
	hrun 'uname -a; [ ! -r /etc/alpine-release ] || echo "Alpine $(cat /etc/alpine-release)"; [ ! -r /etc/debian_version ] || echo "Debian base $(cat /etc/debian_version)"; sed -n "s/^PRETTY_NAME=//p" /etc/os-release; tr "\0" " " </proc/1/cmdline; echo'
	if [ "$INIT" = openrc ]; then hrun 'openrc --version'; else hrun 'systemctl --version | head -n 1'; fi
	hrun "$NEXUSD -version"
	log "\$ $(shown "$NEXUS_CLI" --version)"
	"$NEXUS_CLI" --version 2>&1 | tee -a "$TRANSCRIPT"
	nexusd_pid
	log "  nexusd: pid ${NXD_PID:-not found}"
	[ -n "$NXD_PID" ] || refuse "no /usr/local/bin/nexusd serve process on this host"
	hrun "sed -n '/^  services:\$/,/^  [a-z]/p' $CONF"
	SVC_OK=0
	[ -n "$NXHOST" ] || { log "(no NEXUS_TOKEN and no --nexus-host: the provider was not asked)"; return; }
	NX_TIMEOUT=60
	nx caps capability get --refresh
	read_caps
}

# read_caps: the services provider's capabilities from JF.
read_caps() {
	SVC_I=$(awk -F'\t' '$1 ~ /\.providers\[[0-9]+\]\.name$/ && $2 == "\"services\"" { sub(/\.name$/, "", $1); print $1; exit }' "$JF")
	CAP_STATUS=$(js "$SVC_I.status")
	CAP_REASON=$(jso "$SVC_I.reason")
	D="$SVC_I.details"
	CAP_INIT=$(js "$D.initSystem")
	CAP_LOGSRC=$(js "$D.logSource")
	CAP_SELF=$(jso "$D.selfService")
	CAP_SELF_REASON=$(jso "$D.selfServiceReason")
	CAP_BOOTSRC=$(jso "$D.bootIdSource")
	CAP_RBCMD=$(js "$D.rebootCommand")
	CAP_RBAVAIL=$(js "$D.rebootAvailable")
	CAP_RBENABLED=$(js "$D.rebootEnabled")
	CAP_RBREASON=$(jso "$D.rebootUnavailableReason")
	CAP_DEPRES=$(js "$D.dependencyResolution")
	CAP_DEPREASON=$(jso "$D.dependencyResolutionReason")
	jlist "$D.protected" >"$TMPD/protected"
	# name<TAB>status<TAB>actions (comma-joined)<TAB>reason, one per entry.
	awk -F'\t' -v p="$D.controlAllowList[" '
	index($1, p) == 1 {
		r = substr($1, length(p) + 1); i = r; sub(/\].*/, "", i); k = r; sub(/^[0-9]+\]\./, "", k)
		v = $2; gsub(/^"|"$/, "", v)
		if (!(i in seen)) { seen[i] = 1; ord[n++] = i }
		if (k == "name") nm[i] = v
		else if (k == "status") st[i] = v
		else if (k == "reason") rs[i] = v
		else if (k ~ /^actions\[[0-9]+\]$/) ac[i] = ac[i] (ac[i] == "" ? "" : ",") v
	}
	END { for (j = 0; j < n; j++) { i = ord[j]; print nm[i] "\t" st[i] "\t" ac[i] "\t" rs[i] } }' "$JF" >"$TMPD/allowlist"
	log "  services provider: $CAP_STATUS${CAP_REASON:+ ($CAP_REASON)}; initSystem $CAP_INIT, logSource $CAP_LOGSRC, selfService $CAP_SELF${CAP_SELF_REASON:+ ($CAP_SELF_REASON)}"
	log "  bootIdSource $CAP_BOOTSRC, rebootAvailable $CAP_RBAVAIL, rebootEnabled $CAP_RBENABLED, rebootCommand $CAP_RBCMD${CAP_RBREASON:+, rebootUnavailableReason: $CAP_RBREASON}"
	log "  dependencyResolution $CAP_DEPRES${CAP_DEPREASON:+ ($CAP_DEPREASON)}"
	log "  protected: $(tr '\n' ' ' <"$TMPD/protected")"
	log "  controlAllowList (name, status, actions, reason):"
	sed 's/^/    /' "$TMPD/allowlist" | tee -a "$TRANSCRIPT"
	[ "$CAP_STATUS" = enabled ] && SVC_OK=1
}

protected_has() { grep -qxF -- "$1" "$TMPD/protected"; }
allowlisted() { awk -F'\t' -v n="$1" '$1 == n { f = 1 } END { exit !f }' "$TMPD/allowlist"; }

# --- the plan ----------------------------------------------------------------

plan() {
	log ""
	log "== Plan. With --yes the script will, on this host ($INIT):"
	log "  - TC-SV-L3: read the services capabilities (refreshed) against this host's init system,"
	log "    the service whose cgroup or child_pid holds nexusd, and nexusd's dependencies;"
	log "  - WI 6.1 (f): read the boot-ID mount lines of PID 1 and of nexusd, the boot ID, and"
	log "    compare them with bootIdSource and rebootAvailable (read-only; no reboot of any kind);"
	log "  - TC-SV-L1: list every service through nexus and compare names, states and enabled"
	if [ "$INIT" = systemd ]; then
		log "    words with systemctl list-units and list-unit-files; get nexusd against systemctl show;"
		log "    the last 20 log lines of nexusd against journalctl -u nexusd -n 20 -o cat;"
	else
		log "    words with rc-status -s -C and rc-update show -v; get nexusd against rc-service status"
		log "    and /run/openrc/options/nexusd/child_pid; servicelog nexusd (expected 422 logs-unavailable);"
	fi
	log "    the failed set against hosthealth's failedServices;"
	log "  - TC-SV-L5: send restart nexusd, stop ssh/sshd and stop <selfService>, each only after"
	log "    capabilities list it as protected and the allow-list does not name it; expect 409"
	log "    protected-service for each, the init system's view of each unchanged, and no operation;"
	if [ -n "$TEST_SVC" ]; then
		log "  - TC-SV-L4: start, restart, stop, start, start of $TEST_SVC (only if allow-listed, not"
		log "    protected, and its definition runs sleep), each read back with the init system;"
		log "    then leave $TEST_SVC in the state it was found in;"
	else
		log "  - TC-SV-L4: NOT-RUN (no --test-service). See --print-test-service NAME."
	fi
	log "  - TC-SV-L2 (NFR-1): $CALLS timed calls each of list (all pages), get nexusd and, where"
	log "    nexusd's logs are readable, servicelog nexusd --tail 200;"
	log "  - TC-SV-L13: read the services audit records since the start and match them to the calls;"
	if [ "$INIT" = systemd ]; then
		log "  - TC-SV-L6: journalctl -u nexusd and the nexus-op-* units, and the kernel log, since the"
		log "    start: no SECCOMP, Operation not permitted, Permission denied or seccomp audit line."
	fi
	log "  It never sends a reboot, machine restart or reboot what-if, and never changes $CONF."
	log "  Every host command it runs reads."
}

# --- TC-SV-L3: capabilities -------------------------------------------------

# self_on_host: SELF_HOST, the service whose cgroup (systemd) or child_pid
# (OpenRC) holds nexusd's PID, computed here without Nexus.
self_on_host() {
	if [ "$INIT" = systemd ]; then
		hrun "cat /proc/$NXD_PID/cgroup"
		SELF_HOST=$(printf '%s\n' "$HX_OUT" | awk -F: '$1 == "0" { n = split($3, e, "/"); for (i = n; i >= 1; i--) if (e[i] ~ /\.service$/) { sub(/\.service$/, "", e[i]); print e[i]; exit } }')
	else
		# shellcheck disable=SC2016 # Expanded on the host.
		hrun 'for f in /run/openrc/options/*/child_pid; do [ -r "$f" ] && echo "$(cat "$f") $f"; done'
		SELF_HOST=$(printf '%s\n' "$HX_OUT" | awk -v p="$NXD_PID" '$1 == p { n = split($2, e, "/"); print e[n - 1]; exit }')
	fi
	valid_name "$SELF_HOST" || SELF_HOST=
	log "  the service running nexusd, from the host: ${SELF_HOST:-not found}"
}

# deps_on_host: TMPD/deps, the services nexusd's service depends on, read
# without Nexus: systemd's Requires, BindsTo, Requisite and PartOf
# .service units; OpenRC's deptree ineed names, each through its providers.
deps_on_host() {
	: >"$TMPD/deps"
	[ -n "$SELF_HOST" ] || return
	if [ "$INIT" = systemd ]; then
		hrun "systemctl show -p Requires -p BindsTo -p Requisite -p PartOf -- $SELF_HOST.service"
		printf '%s\n' "$HX_OUT" | awk '{ sub(/^[A-Za-z]+=/, ""); for (i = 1; i <= NF; i++) if ($i ~ /\.service$/) { sub(/\.service$/, "", $i); print $i } }' | sort -u >"$TMPD/deps"
	else
		hfile deptree "cat /run/openrc/deptree"
		awk -v self="$SELF_HOST" -F"'" '
		{ split($1, k, "_"); id = k[2]; key = k[3] }
		key == "service=" { svc[id] = $2; byname[$2] = id }
		key == "ineed" { need[id] = need[id] " " $2 }
		key == "providedby" { prov[id] = prov[id] " " $2 }
		END {
			s = byname[self]
			n = split(need[s], w, " ")
			for (i = 1; i <= n; i++) {
				e = (w[i] in byname) ? byname[w[i]] : ""
				if (e != "" && prov[e] != "") { m = split(prov[e], pv, " "); for (j = 1; j <= m; j++) print pv[j] "\t" w[i] }
				else print w[i] "\t" w[i]
			}
		}' "$TMPD/deptree" | sort -u >"$TMPD/deps.via"
		log "  deptree: $SELF_HOST needs (provider, needed name): $(tr '\t\n' '< ' <"$TMPD/deps.via")"
		cut -f1 "$TMPD/deps.via" | sort -u >"$TMPD/deps"
	fi
	log "  dependencies from the host: $(tr '\n' ' ' <"$TMPD/deps")"
}

check_caps() {
	log ""
	note "== TC-SV-L3: capabilities details"
	self_on_host
	deps_on_host
	_bad=
	[ "$CAP_STATUS" = enabled ] || _bad="$_bad status '$CAP_STATUS';"
	[ "$CAP_INIT" = "$INIT" ] || _bad="$_bad initSystem '$CAP_INIT', the host runs $INIT;"
	if [ "$INIT" = systemd ]; then _want_log=journal _want_cmd='/usr/bin/systemctl reboot'; else _want_log=openrc-files _want_cmd=/sbin/reboot; fi
	[ "$CAP_LOGSRC" = "$_want_log" ] || _bad="$_bad logSource '$CAP_LOGSRC', expected $_want_log;"
	[ "$CAP_RBCMD" = "$_want_cmd" ] || _bad="$_bad rebootCommand '$CAP_RBCMD', expected $_want_cmd;"
	[ -n "$SELF_HOST" ] && [ "$CAP_SELF" = "$SELF_HOST" ] || _bad="$_bad selfService '$CAP_SELF', the host says '$SELF_HOST';"
	[ "$CAP_DEPRES" = complete ] || _bad="$_bad dependencyResolution '$CAP_DEPRES' ($CAP_DEPREASON): control would answer 503;"
	if [ "$INIT" = systemd ]; then _static=$STATIC_SYSTEMD; else _static=$STATIC_OPENRC; fi
	for _p in $_static; do protected_has "$_p" || _bad="$_bad '$_p' (built-in) not in protected;"; done
	[ -z "$CAP_SELF" ] || [ "$CAP_SELF" = null ] || protected_has "$CAP_SELF" || _bad="$_bad selfService $CAP_SELF not in protected;"
	while IFS= read -r _d; do
		[ -n "$_d" ] || continue
		protected_has "$_d" || _bad="$_bad dependency $_d not in protected;"
	done <"$TMPD/deps"
	if [ "$INIT" = openrc ]; then
		_net=$(awk -F'\t' '$2 == "net" { printf "%s ", $1 }' "$TMPD/deps.via")
		log "  the net provider(s) nexusd's deptree entry resolves to: ${_net:-none (nexusd has no ineed net)}"
	fi
	if [ -z "$_bad" ]; then
		result L3-capabilities PASS "enabled; initSystem $CAP_INIT, logSource $CAP_LOGSRC, selfService $CAP_SELF, rebootCommand $CAP_RBCMD, dependencyResolution complete; protected holds the built-in list, $CAP_SELF and the host's dependencies ($(tr '\n' ' ' <"$TMPD/deps"))"
	else
		result L3-capabilities FAIL "$_bad"
	fi
	if [ "$CAP_RBENABLED" = false ]; then
		result L3-reboot-disabled PASS "rebootEnabled false: the reboot stays off, as the user chose on 2026-11-18"
	else
		result L3-reboot-disabled INFO "rebootEnabled '$CAP_RBENABLED': this script sends no reboot either way"
	fi
}

# --- WI 6.1 (f): the boot-ID source, read-only ---------------------------------

check_bootid() {
	log ""
	note "== WI 6.1 (f): boot-ID source (read-only; no reboot, no what-if)"
	hrun "grep -F boot_id /proc/1/mountinfo"
	_p1=$(printf '%s\n' "$HX_OUT" | awk -v m="$BOOT_ID_PATH" '$5 == m { print; exit }')
	hrun "grep -F boot_id /proc/$NXD_PID/mountinfo"
	_nx=$(printf '%s\n' "$HX_OUT" | awk -v m="$BOOT_ID_PATH" '$5 == m { print; exit }')
	hrun "cat $BOOT_ID_PATH"
	_boot=$HX_OUT
	if command -v systemd-detect-virt >/dev/null 2>&1; then
		hrun 'systemd-detect-virt; systemd-detect-virt -c' || true
	fi
	_bad=
	_why=
	if [ -n "$_p1" ]; then
		_root=$(printf '%s\n' "$_p1" | awk '{ print $4 }')
		_why="PID 1 has a mount at $BOOT_ID_PATH, root $_root"
		[ "$_root" = /.lxc-boot-id ] || _why="$_why (not LXC's /.lxc-boot-id: recorded)"
		[ "$CAP_BOOTSRC" = container-mount ] || _bad="$_bad bootIdSource '$CAP_BOOTSRC' with PID 1's mount (expected container-mount);"
		if [ -z "$_nx" ]; then
			_why="$_why; nexusd's own table has no such line"
			[ "$INIT" = systemd ] || _bad="$_bad nexusd's table lacks the mount on OpenRC (no sandbox: expected PID 1's line);"
		elif [ "$_nx" = "$_p1" ]; then
			_why="$_why; nexusd's table has the same line"
		else
			_why="$_why; nexusd's table has a different line: $_nx"
			[ "$INIT" = systemd ] || _bad="$_bad nexusd's line differs from PID 1's on OpenRC;"
		fi
	else
		_why="no mount at $BOOT_ID_PATH in PID 1's table"
		if [ "$LXC" = 1 ] || [ "$DOCKER" = 1 ]; then
			_bad="$_bad a container without a per-container boot ID: the reboot cannot be confirmed here (WI 6.2's stop condition);"
		else
			[ "$CAP_BOOTSRC" = kernel ] || _bad="$_bad bootIdSource '$CAP_BOOTSRC' on a VM or bare metal (expected kernel);"
		fi
	fi
	[ "$CAP_RBAVAIL" = true ] || _bad="$_bad rebootAvailable '$CAP_RBAVAIL' ($CAP_RBREASON);"
	case $CAP_RBREASON in *"PID 1 has a boot ID mounted"*) _bad="$_bad the unavailable reason names PID 1's boot-ID mount (the pre-fix defect);" ;; esac
	log "  the boot ID in this root shell (PID 1's view), for WI 6.2: $_boot"
	if [ -z "$_bad" ]; then
		result F-bootid PASS "bootIdSource $CAP_BOOTSRC, rebootAvailable true; $_why; boot ID $_boot"
	else
		result F-bootid FAIL "$_bad $_why; boot ID $_boot. This stops WI 6.2 on this host."
	fi
}

# --- TC-SV-L1: reads against the host's tools -----------------------------------

# nexus_list FILE: every service through nexus, all pages, as
# "name<TAB>state<TAB>enabled" lines in FILE, sorted.
nexus_list() {
	: >"$1.raw"
	_cur=
	_pg=0
	while :; do
		if [ -z "$_cur" ]; then
			nx_quiet "list-$_pg" service get --limit 1000
		else
			nx_quiet "list-$_pg" service get --limit 1000 --cursor "$_cur"
		fi
		[ "$NX_RC" -eq 0 ] || return 1
		awk -F'\t' '
		match($1, /^\[[0-9]+\]\./) {
			i = substr($1, 2, RLENGTH - 3); k = substr($1, RLENGTH + 1)
			v = $2; gsub(/^"|"$/, "", v)
			if (!(i in seen)) { seen[i] = 1; ord[n++] = i }
			if (k == "name") nm[i] = v; else if (k == "properties.state") st[i] = v; else if (k == "properties.enabled") en[i] = v
		}
		END { for (j = 0; j < n; j++) { i = ord[j]; print nm[i] "\t" st[i] "\t" en[i] } }' "$JF" >>"$1.raw"
		_cur=$(sed -n 's/.*"nextCursor":"\([^"]*\)".*/\1/p' "$TMPD/list-$_pg.err" | head -n 1)
		[ -n "$_cur" ] || break
		_pg=$((_pg + 1))
		[ "$_pg" -lt 50 ] || { log "  (more than 50 pages: stopped)"; return 1; }
	done
	sort "$1.raw" >"$1"
	log "  nexus: $(wc -l <"$1" | tr -d ' ') services in $((_pg + 1)) page(s)"
}

# host_list FILE: the same from the init system's own tools, by the
# TC-SV-46 (systemd) or TC-SV-49 (OpenRC) mapping.
host_list() {
	if [ "$INIT" = systemd ]; then
		hfile units "systemctl list-units --type=service --all --plain --no-legend --no-pager --full"
		hfile files "systemctl list-unit-files --type=service --plain --no-legend --no-pager --full"
		awk '
		function st(w) {
			if (w == "active" || w == "reloading") return "running"
			if (w == "activating") return "starting"
			if (w == "deactivating") return "stopping"
			if (w == "inactive") return "stopped"
			if (w == "failed") return "failed"
			return "unknown"
		}
		function en(w) { return (w ~ /^(enabled|disabled|static|masked|indirect|generated|alias)$/) ? w : "unknown" }
		FNR == 1 { f++ }
		f == 1 {
			if ($1 == "\342\227\217" || $1 == "*") { $1 = ""; $0 = $0 }
			if ($1 !~ /\.service$/ || NF < 4) next
			n = $1; sub(/\.service$/, "", n)
			if (n == "" || n ~ /@$/ || (n in u)) next
			u[n] = st($3); uo[nu++] = n
		}
		f == 2 {
			if ($1 !~ /\.service$/ || NF < 2) next
			n = $1; sub(/\.service$/, "", n)
			if (n == "" || (n in fs)) next
			fs[n] = $2; fo[nf++] = n
		}
		END {
			for (i = 0; i < nu; i++) {
				n = uo[i]; e = "unknown"
				if (n in fs) e = en(fs[n])
				else if (index(n, "@") > 0) { t = substr(n, 1, index(n, "@")); if (substr(n, index(n, "@") + 1) != "" && (t in fs)) e = en(fs[t]) }
				print n "\t" u[n] "\t" e
			}
			for (i = 0; i < nf; i++) {
				n = fo[i]
				if (n in u) continue
				s = (fs[n] == "alias" && n !~ /@$/) ? "unknown" : "stopped"
				print n "\t" s "\t" en(fs[n])
			}
		}' "$TMPD/units" "$TMPD/files" | sort >"$1"
	else
		hfile rcstatus "rc-status -s -C"
		hfile rcupdate "rc-update show -v"
		awk '
		function st(w) {
			if (w == "started") return "running"
			if (w == "stopped") return "stopped"
			if (w == "crashed") return "failed"
			if (w == "starting") return "starting"
			if (w == "stopping") return "stopping"
			return "unknown"
		}
		FNR == 1 { f++ }
		f == 1 {
			o = index($0, "["); c = length($0)
			l = $0; sub(/[ \t]+$/, "", l)
			if (o <= 1 || substr(l, length(l), 1) != "]") next
			n = substr(l, 1, o - 1); gsub(/^[ \t]+|[ \t]+$/, "", n)
			w = substr(l, o + 1, length(l) - o - 1); split(w, ws, " ")
			if (n == "" || n ~ /[ \t]/ || ws[1] == "" || (n in s)) next
			s[n] = st(ws[1]); so[ns++] = n
		}
		f == 2 {
			p = index($0, "|"); if (p == 0) next
			n = substr($0, 1, p - 1); gsub(/^[ \t]+|[ \t]+$/, "", n)
			r = substr($0, p + 1); gsub(/^[ \t]+|[ \t]+$/, "", r)
			if (n == "" || n ~ /[ \t]/ || (n in rl)) next
			rl[n] = r; ro[nr++] = n
		}
		END {
			for (i = 0; i < ns; i++) { n = so[i]; print n "\t" s[n] "\t" ((rl[n] != "") ? "enabled" : "disabled") }
			for (i = 0; i < nr; i++) { n = ro[i]; if (n in s) continue; print n "\t" "stopped" "\t" ((rl[n] != "") ? "enabled" : "disabled") }
		}' "$TMPD/rcstatus" "$TMPD/rcupdate" | sort >"$1"
	fi
	log "  host: $(wc -l <"$1" | tr -d ' ') services"
}

# list_diff A B: names only in one, and per name the state and enabled
# words that differ, written to TMPD/listdiff. Returns 1 when any.
list_diff() {
	awk -F'\t' '
	FNR == 1 { f++ }
	f == 1 { a[$1] = $2 "\t" $3; next }
	{
		if (!($1 in a)) { print "only on the host: " $1 " (" $2 ", " $3 ")"; next }
		split(a[$1], x, "\t")
		if (x[1] != $2) print $1 ": state nexus " x[1] ", host " $2
		if (x[2] != $3) print $1 ": enabled nexus " x[2] ", host " $3
		delete a[$1]
	}
	END { for (n in a) { split(a[n], x, "\t"); print "only in nexus: " n " (" x[1] ", " x[2] ")" } }' "$1" "$2" | sort >"$TMPD/listdiff"
	[ ! -s "$TMPD/listdiff" ]
}

check_list() {
	log ""
	note "== TC-SV-L1 (1): list against the init system's own listings"
	NX_TIMEOUT=60
	if ! nexus_list "$TMPD/nx.tsv"; then
		result L1-list FAIL "nexus service get failed (exit $NX_RC)"
		return
	fi
	host_list "$TMPD/host.tsv"
	if list_diff "$TMPD/nx.tsv" "$TMPD/host.tsv"; then
		result L1-list PASS "$(wc -l <"$TMPD/nx.tsv" | tr -d ' ') services: the name sets are equal and every state and enabled word agrees"
	else
		log "  differences on the first read:"
		sed 's/^/    /' "$TMPD/listdiff" | tee -a "$TRANSCRIPT"
		log "  re-reading both once (a unit may have changed between the reads):"
		nexus_list "$TMPD/nx.tsv" && host_list "$TMPD/host.tsv"
		if list_diff "$TMPD/nx.tsv" "$TMPD/host.tsv"; then
			result L1-list PASS "$(wc -l <"$TMPD/nx.tsv" | tr -d ' ') services agree on the re-read ($(wc -l <"$TMPD/listdiff" | tr -d ' ') difference(s) on the first read changed between the reads)"
		else
			sed 's/^/    /' "$TMPD/listdiff" | tee -a "$TRANSCRIPT"
			result L1-list FAIL "$(wc -l <"$TMPD/listdiff" | tr -d ' ') difference(s) after a re-read: $(head -n 3 "$TMPD/listdiff" | tr '\n' ';')"
		fi
	fi
	awk -F'\t' '$2 == "failed" { print $1 }' "$TMPD/nx.tsv" | sort >"$TMPD/nx.failed"
	log "  failed services in the list: $(tr '\n' ' ' <"$TMPD/nx.failed")"
	if nx hosthealth hosthealth get; then
		_cnt=$(jv '[0].properties.failedServices.count')
		jlist '[0].properties.failedServices.names' >"$TMPD/hh.names"
		if [ "$INIT" = systemd ]; then
			sed -n 's/\.service$//p' "$TMPD/hh.names" | sort >"$TMPD/hh.failed"
		else
			sort "$TMPD/hh.names" >"$TMPD/hh.failed"
		fi
		_nn=$(wc -l <"$TMPD/hh.names" | tr -d ' ')
		if [ "$_cnt" != "$_nn" ]; then
			result L1-failed INFO "hosthealth lists $_nn of $_cnt failed units (capped): not compared"
		elif cmp -s "$TMPD/nx.failed" "$TMPD/hh.failed"; then
			result L1-failed PASS "the list's failed set [$(tr '\n' ' ' <"$TMPD/nx.failed")] equals hosthealth's .service subset"
		else
			result L1-failed FAIL "the list's failed set [$(tr '\n' ' ' <"$TMPD/nx.failed")], hosthealth's .service subset [$(tr '\n' ' ' <"$TMPD/hh.failed")]"
		fi
	else
		result L1-failed NOT-RUN "nexus hosthealth get failed (exit $NX_RC): is the host provider enabled, and does the token hold host:read?"
	fi
}

check_get() {
	log ""
	note "== TC-SV-L1 (2): get nexusd against the init system"
	NX_TIMEOUT=60
	nx get-nexusd service get nexusd || { result L1-get FAIL "nexus service get nexusd failed (exit $NX_RC)"; return; }
	cp "$JF" "$TMPD/get-nexusd.keep"
	P='[0].properties'
	_bad=
	_ok=0
	if [ "$INIT" = systemd ]; then
		hrun "systemctl show --timestamp=unix -p Id -p ActiveState -p SubState -p MainPID -p NRestarts -p UnitFileState -p Result -p FragmentPath -p ActiveEnterTimestamp -- nexusd.service"
		_as=$(hkv ActiveState)
		case $_as in active | reloading) _st=running ;; activating) _st=starting ;; deactivating) _st=stopping ;; inactive) _st=stopped ;; failed) _st=failed ;; *) _st=unknown ;; esac
		for _c in "state:$_st" "rawState:$_as ($(hkv SubState))" "subState:$(hkv SubState)" "mainPid:$(_m=$(hkv MainPID); [ "$_m" = 0 ] && echo null || echo "$_m")" "restarts:$(hkv NRestarts)" "enabledRaw:$(hkv UnitFileState)" "result:$(hkv Result)" "unitFilePath:$(hkv FragmentPath)"; do
			_k=${_c%%:*} _w=${_c#*:}
			_v=$(js "$P.$_k")
			if [ "$_v" = "$_w" ]; then _ok=$((_ok + 1)); log "  ok    $_k $_v"; else _bad="$_bad $_k nexus '$_v', host '$_w';"; log "  FAIL  $_k nexus '$_v', host '$_w'"; fi
		done
		_ae=$(hkv ActiveEnterTimestamp)
		_since=$(js "$P.since")
		log "  since: nexus $_since, host ActiveEnterTimestamp $_ae"
	else
		hrun "rc-service nexusd status; echo child_pid=\$(cat /run/openrc/options/nexusd/child_pid); rc-update show -v | grep -E '^[[:space:]]*nexusd[[:space:]]*\\|'"
		_w=$(printf '%s\n' "$HX_OUT" | sed -n 's/^ \* status: \([a-z]*\).*/\1/p' | head -n 1)
		case $_w in started) _st=running ;; stopped) _st=stopped ;; crashed) _st=failed ;; starting) _st=starting ;; stopping) _st=stopping ;; *) _st=unknown ;; esac
		_cp=$(hkv child_pid)
		_rl=$(printf '%s\n' "$HX_OUT" | awk -F'|' '/\|/ { r = $2; gsub(/^[ \t]+|[ \t]+$/, "", r); print r; exit }')
		_en=disabled
		[ -z "$_rl" ] || _en=enabled
		for _c in "state:$_st" "rawState:$_w" "mainPid:$_cp" "enabled:$_en" "enabledRaw:$_rl" "scriptPath:/etc/init.d/nexusd"; do
			_k=${_c%%:*} _wv=${_c#*:}
			_v=$(js "$P.$_k")
			if [ "$_v" = "$_wv" ]; then _ok=$((_ok + 1)); log "  ok    $_k $_v"; else _bad="$_bad $_k nexus '$_v', host '$_wv';"; log "  FAIL  $_k nexus '$_v', host '$_wv'"; fi
		done
	fi
	if [ -z "$_bad" ]; then
		result L1-get PASS "get nexusd: $_ok fields equal the init system's"
	else
		result L1-get FAIL "$_bad"
	fi
}

# frames_to FILE: the d (message) of each frame in JF, decoded, one per
# line, in order.
frames_to() {
	awk -F'\t' "$JUNQ"'
	$1 ~ /^\[[0-9]+\]\.d$/ { print junq($2) }' "$JF" >"$1"
}

check_logs() {
	log ""
	note "== TC-SV-L1 (3): servicelog nexusd against the host"
	JF=$TMPD/get-nexusd.keep
	_avail=$(jv '[0].properties.logs.available')
	_src=$(js '[0].properties.logs.source')
	_why=$(jso '[0].properties.logs.reason')
	log "  get nexusd says logs: available $_avail, source $_src${_why:+, reason: $_why}"
	NX_TIMEOUT=60
	LOGS_CALLS=$((LOGS_CALLS + 1))
	if [ "$INIT" = systemd ]; then
		nx_quiet logs20 servicelog get nexusd --tail 20 || { result L1-logs FAIL "servicelog get nexusd failed (exit $NX_RC)"; return; }
		LOGS_OK=$((LOGS_OK + 1))
		frames_to "$TMPD/nx.log"
		hfile journal "journalctl -u nexusd.service -n 100 -o cat --no-pager"
		log "  nexus's frames (messages):"
		sed 's/^/    | /' "$TMPD/nx.log" | tee -a "$TRANSCRIPT"
		# The newest lines journalctl has that came after nexus's block: 0
		# when the two reads saw the same tail.
		_k=$(awk '
		FNR == 1 { f++ }
		f == 1 { a[++na] = $0; next }
		{ b[++nb] = $0 }
		END {
			if (na == 0) { print "none"; exit }
			for (k = 0; k + na <= nb; k++) {
				ok = 1
				for (i = 1; i <= na; i++) if (a[i] != b[nb - k - na + i]) { ok = 0; break }
				if (ok) { print k; exit }
			}
			print "none"
		}' "$TMPD/nx.log" "$TMPD/journal")
		_n=$(wc -l <"$TMPD/nx.log" | tr -d ' ')
		if [ "$_n" -eq 0 ]; then
			result L1-logs FAIL "servicelog get nexusd returned no frames"
		elif [ "$_k" = 0 ]; then
			result L1-logs PASS "the $_n messages equal journalctl -u nexusd's last $_n, in order"
		elif [ "$_k" = none ]; then
			log "  journalctl's last lines:"
			tail -n 25 "$TMPD/journal" | sed 's/^/    | /' | tee -a "$TRANSCRIPT"
			result L1-logs FAIL "the $_n messages are not a block of journalctl -u nexusd's last 100 lines"
		else
			log "  journalctl had $_k newer line(s) after the block, logged between the two reads:"
			tail -n "$_k" "$TMPD/journal" | sed 's/^/    | /' | tee -a "$TRANSCRIPT"
			result L1-logs PASS "the $_n messages equal journalctl's, in order, followed by $_k line(s) logged between the two reads"
		fi
	elif [ "$_avail" = false ]; then
		nx_quiet logs20 servicelog get nexusd --tail 20
		if [ "$NX_RC" -ne 0 ] && slug_in logs20 logs-unavailable; then
			result L1-logs PASS "servicelog get nexusd answers logs-unavailable, as get reports (source $_src: $_why); the expected readback on OpenRC with output_logger"
		else
			result L1-logs FAIL "servicelog get nexusd: exit $NX_RC, expected a 422 logs-unavailable"
		fi
	else
		nx logs20 servicelog get nexusd --tail 20 || { result L1-logs FAIL "servicelog get nexusd failed (exit $NX_RC)"; return; }
		LOGS_OK=$((LOGS_OK + 1))
		hrun "grep -E '^[[:space:]]*(output_log|error_log|output_logger|error_logger)=' /etc/init.d/nexusd /etc/conf.d/nexusd" || true
		result L1-logs INFO "nexusd's logs are readable from $_src on OpenRC: compare the frames above with the declared files by hand"
	fi
}

# --- TC-SV-L5: refusals ---------------------------------------------------

# prot_snapshot FILE: the init system's view of every probed service and
# the count of operations, for a byte comparison before and after.
prot_snapshot() {
	if [ "$INIT" = systemd ]; then
		_u=
		for _s in $PROBE_NAMES; do _u="$_u $_s.service"; done
		sh -c "systemctl show -p Id -p LoadState -p ActiveState -p SubState -p MainPID -p NRestarts -p ActiveEnterTimestampMonotonic -- $_u; echo nexus-op-units=\$(systemctl list-units --all --plain --no-legend --no-pager 'nexus-op-*' | wc -l); echo ops=\$(ls $OPS_DIR 2>/dev/null | wc -l)" >"$1" 2>&1 </dev/null
	else
		# PROBE_NAMES is word-split on purpose: valid service names, one
		# argument each. The program text is expanded on the host.
		# shellcheck disable=SC2016,SC2086
		sh -c 'for s in "$@"; do echo "== $s"; rc-service "$s" status 2>&1; f=/run/openrc/options/$s/child_pid; if [ -r "$f" ]; then p=$(cat "$f"); echo "child_pid=$p"; s2=$(cat /proc/$p/stat 2>/dev/null) && { s2=${s2##*") "}; set -- $s2; echo "starttime=${20}"; }; fi; done; echo ops=$(ls '"$OPS_DIR"' 2>/dev/null | wc -l)' sh $PROBE_NAMES >"$1" 2>&1 </dev/null
	fi
}

check_refusals() {
	log ""
	note "== TC-SV-L5: every FR-9 refusal sends nothing"
	if [ "$INIT" = systemd ]; then
		_probes="restart:nexusd stop:ssh stop:sshd"
	else
		_probes="restart:nexusd stop:sshd"
	fi
	if valid_name "$CAP_SELF"; then
		_probes="$_probes stop:$CAP_SELF"
	elif [ -n "$CAP_SELF" ] && [ "$CAP_SELF" != null ]; then
		log "  selfService '$CAP_SELF' is not a valid name: not probed"
	fi
	PROBE_NAMES=
	for _p in $_probes; do
		word_in "${_p#*:}" "$PROBE_NAMES" || PROBE_NAMES="$PROBE_NAMES ${_p#*:}"
	done
	T5=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	prot_snapshot "$TMPD/prot.before"
	log "  before (the init system's view, and the operation counts):"
	sed 's/^/    /' "$TMPD/prot.before" | tee -a "$TRANSCRIPT"
	_bad=
	_sent=0
	NX_TIMEOUT=60
	for _p in $_probes; do
		_a=${_p%%:*} _n=${_p#*:}
		valid_name "$_n" || { _bad="$_bad $_n is not a valid name: not sent;"; continue; }
		if ! protected_has "$_n"; then
			_bad="$_bad $_n is not in capabilities' protected list: NOT sent;"
			log "  NOT SENT: $_a $_n (capabilities do not list $_n as protected)"
			continue
		fi
		if allowlisted "$_n"; then
			_bad="$_bad $_n is in the allow-list: NOT sent;"
			log "  NOT SENT: $_a $_n (the allow-list names it)"
			continue
		fi
		nx "ref-$_a-$_n" service "$_a" "$_n" --yes
		_sent=$((_sent + 1))
		if [ "$NX_RC" -ne 0 ] && slug_in "ref-$_a-$_n" protected-service; then
			log "  ok    $_a $_n: 409 protected-service"
		else
			_bad="$_bad $_a $_n: exit $NX_RC without protected-service;"
			log "  FAIL  $_a $_n: exit $NX_RC, protected-service not in the answer"
		fi
	done
	REFUSALS_SENT=$_sent
	prot_snapshot "$TMPD/prot.after"
	log "  after:"
	sed 's/^/    /' "$TMPD/prot.after" | tee -a "$TRANSCRIPT"
	if [ -z "$_bad" ]; then
		result L5-refusals PASS "$_sent request(s) ($_probes), each 409 protected-service"
	else
		result L5-refusals FAIL "$_bad"
	fi
	if cmp -s "$TMPD/prot.before" "$TMPD/prot.after"; then
		result L5-unchanged PASS "the init system's view of$PROBE_NAMES and the operation counts are byte-identical before and after"
	else
		diff "$TMPD/prot.before" "$TMPD/prot.after" | sed 's/^/    /' | tee -a "$TRANSCRIPT"
		result L5-unchanged FAIL "the readbacks differ before and after the refused requests"
	fi
	if [ -n "$TOKEN_ID" ]; then
		nx_quiet audit-l5 auditrecord get --filter provider=services --filter outcome=refused --filter "after=$T5" --filter "tokenId=$TOKEN_ID" --limit 1000
	else
		nx_quiet audit-l5 auditrecord get --filter provider=services --filter outcome=refused --filter "after=$T5" --limit 1000
	fi
	if [ "$NX_RC" -eq 0 ]; then
		_r=$(awk -F'\t' '$1 ~ /^\[[0-9]+\]\.problem$/ && $2 == "\"protected-service\"" { n++ } END { print n + 0 }' "$JF")
		awk -F'\t' '$1 ~ /^\[[0-9]+\]\.(action|outcome|problem|target)$/' "$JF" | sed 's/^/    /' | tee -a "$TRANSCRIPT"
		if [ "$_r" -eq "$_sent" ]; then
			result L5-audit PASS "$_r refused record(s) with problem protected-service since $T5, one per request"
		else
			result L5-audit FAIL "$_r refused protected-service record(s) since $T5, $_sent request(s) sent"
		fi
	else
		result L5-audit NOT-RUN "nexus auditrecord get failed (exit $NX_RC): does the token hold audit:read?"
	fi
	log "  SSH: the ssh/sshd lines above are unchanged; confirm from your own session that SSH still logs in."
}

# --- TC-SV-L4: control of the test service -----------------------------------

# tsvc_read: TS_STATE (the init system's word), TS_PID and TS_START (the
# main process and when it entered its state), logged.
tsvc_read() {
	if [ "$INIT" = systemd ]; then
		hrun "systemctl show -p ActiveState -p SubState -p MainPID -p ActiveEnterTimestampMonotonic -- $TEST_SVC.service; systemctl is-active -- $TEST_SVC.service" || true
		TS_STATE=$(hkv ActiveState)
		TS_PID=$(hkv MainPID)
		TS_START=$(hkv ActiveEnterTimestampMonotonic)
		[ "$TS_PID" != 0 ] || TS_PID=
	else
		# shellcheck disable=SC2016 # Expanded on the host.
		hrun 'rc-service '"$TEST_SVC"' status; f=/run/openrc/options/'"$TEST_SVC"'/child_pid; if [ -r "$f" ]; then p=$(cat "$f"); echo "child_pid=$p"; s=$(cat /proc/$p/stat 2>/dev/null) && { s=${s##*") "}; set -- $s; echo "starttime=${20}"; }; fi' || true
		TS_STATE=$(printf '%s\n' "$HX_OUT" | sed -n 's/^ \* status: \([a-z]*\).*/\1/p' | head -n 1)
		TS_PID=$(hkv child_pid)
		TS_START=$(hkv starttime)
		[ "$TS_STATE" = started ] || { TS_PID='' TS_START=''; }
	fi
}

# ts_running: the init system's word means running (active / started).
ts_running() { [ "$TS_STATE" = active ] || [ "$TS_STATE" = started ]; }
ts_stopped() { [ "$TS_STATE" = inactive ] || [ "$TS_STATE" = stopped ]; }

# ctl ID ACTION: one control action through nexus on the test service,
# then the readback. CTL_* hold the result.
ctl() {
	NX_TIMEOUT=240
	CTL_SENT="$CTL_SENT $2"
	nx "$1" service "$2" "$TEST_SVC" --yes
	CTL_RC=$NX_RC
	CTL_STATUS=$(js '[0].status')
	CTL_STATE=$(js '[0].result.state')
	CTL_CHANGED=$(js '[0].result.changed')
	CTL_PID=$(js '[0].result.mainPid')
	tsvc_read
}

# test_service_ok: the guards before any action on TEST_SVC.
test_service_ok() {
	_why=
	if [ "$INIT" = systemd ]; then _static=$STATIC_SYSTEMD; else _static=$STATIC_OPENRC; fi
	word_in "$TEST_SVC" "$_static" && _why="$_why it is on the built-in protected list;"
	for _x in nexusd ssh sshd "$CAP_SELF" "$SELF_HOST"; do
		[ -n "$_x" ] && [ "$TEST_SVC" = "$_x" ] && _why="$_why it is $_x;"
	done
	protected_has "$TEST_SVC" && _why="$_why capabilities list it as protected;"
	_e=$(awk -F'\t' -v n="$TEST_SVC" '$1 == n { print; exit }' "$TMPD/allowlist")
	if [ -z "$_e" ]; then
		_why="$_why it is not in providers.services.control (see --print-test-service);"
	else
		_st=$(printf '%s\n' "$_e" | cut -f2)
		_ac=$(printf '%s\n' "$_e" | cut -f3)
		[ "$_st" = ok ] || _why="$_why its allow-list status is '$_st' ($(printf '%s\n' "$_e" | cut -f4));"
		for _x in start stop restart; do
			case ",$_ac," in *",$_x,"*) ;; *) _why="$_why its entry does not allow $_x;" ;; esac
		done
	fi
	if [ "$INIT" = systemd ]; then
		hrun "systemctl show -p Id -p LoadState -p FragmentPath -p ExecStart -- $TEST_SVC.service" || true
		[ "$(hkv LoadState)" = loaded ] || _why="$_why systemd has not loaded $TEST_SVC.service;"
		[ "$(hkv Id)" = "$TEST_SVC.service" ] || _why="$_why its Id is '$(hkv Id)' (an alias?);"
		case $(hkv ExecStart) in *sleep*) ;; *) _why="$_why its ExecStart does not run sleep;" ;; esac
	else
		hrun "cat /etc/init.d/$TEST_SVC" || _why="$_why /etc/init.d/$TEST_SVC cannot be read;"
		case $HX_OUT in *sleep*) ;; *) _why="$_why its script does not run sleep;" ;; esac
		case $HX_OUT in *supervise-daemon*) ;; *) _why="$_why its script does not use supervise-daemon;" ;; esac
	fi
	if [ -n "$_why" ]; then
		result L4-control FAIL "refused to act on $TEST_SVC:$_why"
		return 1
	fi
	return 0
}

check_control() {
	log ""
	note "== TC-SV-L4: start, stop and restart of $TEST_SVC"
	test_service_ok || return
	tsvc_read
	_init_state=$TS_STATE
	log "  initial state: $_init_state"

	ctl l4-start start
	if [ "$CTL_RC" -eq 0 ] && [ "$CTL_STATUS" = Succeeded ] && [ "$CTL_STATE" = running ] && ts_running && [ "$CTL_PID" = "$TS_PID" ]; then
		result L4-1-start PASS "Succeeded, state running (changed $CTL_CHANGED), mainPid $CTL_PID; the init system says $TS_STATE, main PID $TS_PID"
	else
		result L4-1-start FAIL "exit $CTL_RC, $CTL_STATUS, state '$CTL_STATE', mainPid '$CTL_PID'; the init system says '$TS_STATE', main PID '$TS_PID'"
	fi
	_pid1=$TS_PID

	ctl l4-restart restart
	if [ "$CTL_RC" -eq 0 ] && [ "$CTL_STATUS" = Succeeded ] && [ "$CTL_STATE" = running ] && ts_running && [ -n "$TS_PID" ] && [ "$TS_PID" != "$_pid1" ] && [ "$CTL_PID" = "$TS_PID" ]; then
		result L4-2-restart PASS "Succeeded, state running, mainPid $_pid1 -> $TS_PID (the init system agrees: $TS_STATE)"
	else
		result L4-2-restart FAIL "exit $CTL_RC, $CTL_STATUS, state '$CTL_STATE', mainPid '$CTL_PID'; the init system says '$TS_STATE', main PID '$_pid1' -> '$TS_PID'"
	fi

	ctl l4-stop stop
	if [ "$CTL_RC" -eq 0 ] && [ "$CTL_STATUS" = Succeeded ] && [ "$CTL_STATE" = stopped ] && ts_stopped; then
		result L4-3-stop PASS "Succeeded, state stopped; the init system says $TS_STATE"
	else
		result L4-3-stop FAIL "exit $CTL_RC, $CTL_STATUS, state '$CTL_STATE'; the init system says '$TS_STATE'"
	fi

	ctl l4-start-a start
	_ok_a=0
	if [ "$CTL_RC" -eq 0 ] && [ "$CTL_STATUS" = Succeeded ] && [ "$CTL_STATE" = running ] && [ "$CTL_CHANGED" = true ] && ts_running; then _ok_a=1; fi
	_pid_a=$TS_PID _start_a=$TS_START _a="exit $CTL_RC, $CTL_STATUS, state '$CTL_STATE', changed $CTL_CHANGED, init '$TS_STATE'"
	ctl l4-start-b start
	if [ "$_ok_a" = 1 ] && [ "$CTL_RC" -eq 0 ] && [ "$CTL_STATUS" = Succeeded ] && [ "$CTL_STATE" = running ] && [ "$CTL_CHANGED" = false ] && ts_running && [ "$TS_PID" = "$_pid_a" ] && [ -n "$TS_START" ] && [ "$TS_START" = "$_start_a" ]; then
		result L4-4-start-twice PASS "first start changed true; second changed false, main PID $TS_PID and start time $TS_START unchanged"
	else
		result L4-4-start-twice FAIL "first: $_a; second: exit $CTL_RC, $CTL_STATUS, state '$CTL_STATE', changed $CTL_CHANGED, init '$TS_STATE', PID '$_pid_a' -> '$TS_PID', start '$_start_a' -> '$TS_START'"
	fi

	if [ "$_init_state" = inactive ] || [ "$_init_state" = stopped ]; then
		log "  restoring the initial state ($_init_state):"
		ctl l4-restore stop
		ts_stopped && log "  $TEST_SVC is stopped again" || result L4-restore FAIL "$TEST_SVC did not stop again (the init system says '$TS_STATE')"
	else
		log "  left $TEST_SVC running, as it was found ($_init_state)"
	fi
	log "  remove $TEST_SVC and its allow-list entry when the runs are done (--print-test-service $TEST_SVC)."
}

# --- TC-SV-L2: NFR-1 timings -----------------------------------------------------

# up_cs: /proc/uptime in centiseconds (a builtin read: no fork).
up_cs() {
	read -r _up _rest </proc/uptime
	_up=${_up%.*}${_up#*.}
	printf '%s' "$_up"
}

# timed KIND ARG...: one timed CLI call (all pages for the list); the
# wall-clock milliseconds go to TMPD/ms.KIND, a failure to TMPD/err.KIND.
timed() {
	_kind=$1
	shift
	_t0=$(up_cs)
	_cur=
	_pages=0
	while :; do
		if [ -z "$_cur" ]; then
			"$NEXUS_CLI" --host "$NXHOST" "$@" -o json >/dev/null 2>"$TMPD/timed.err" || echo x >>"$TMPD/err.$_kind"
		else
			"$NEXUS_CLI" --host "$NXHOST" "$@" --cursor "$_cur" -o json >/dev/null 2>"$TMPD/timed.err" || echo x >>"$TMPD/err.$_kind"
		fi
		_pages=$((_pages + 1))
		[ "$_kind" = list ] || break
		_cur=$(sed -n 's/.*"nextCursor":"\([^"]*\)".*/\1/p' "$TMPD/timed.err" | head -n 1)
		[ -n "$_cur" ] && [ "$_pages" -lt 50 ] || break
	done
	_t1=$(up_cs)
	echo $(((_t1 - _t0) * 10)) >>"$TMPD/ms.$_kind"
}

# p95_of KIND TARGET_MS LABEL: the result line for one read.
p95_of() {
	_st=$(sort -n "$TMPD/ms.$1" | awk '{ v[NR] = $1 } END { i = int(0.95 * NR); if (i < 0.95 * NR) i++; printf "n=%d p50=%d p95=%d max=%d", NR, v[int((NR + 1) / 2)], v[i], v[NR] }')
	_p95=$(printf '%s\n' "$_st" | sed 's/.*p95=\([0-9]*\).*/\1/')
	_err=0
	[ ! -f "$TMPD/err.$1" ] || _err=$(wc -l <"$TMPD/err.$1" | tr -d ' ')
	log "  $3: $_st ms (wall clock, centisecond resolution), errors $_err"
	if [ "$_err" -eq 0 ] && [ "$_p95" -lt "$2" ]; then
		result "L2-$1" PASS "$3: p95 ${_p95} ms < $2 ms ($_st ms)"
	else
		result "L2-$1" FAIL "$3: p95 ${_p95} ms (target < $2 ms), errors $_err ($_st ms)"
	fi
}

check_timings() {
	log ""
	note "== TC-SV-L2 (NFR-1): $CALLS timed calls of each read"
	log "  each call is a new nexus process (TLS handshake included), timed from before its start"
	log "  to its exit with /proc/uptime: the whole response, the log stream included"
	_k=0
	while [ "$_k" -lt "$CALLS" ]; do
		timed list service get --limit 1000
		timed get service get nexusd
		if [ "$LOGS_OK" -gt 0 ]; then
			timed logs servicelog get nexusd --tail 200
			LOGS_CALLS=$((LOGS_CALLS + 1))
		fi
		_k=$((_k + 1))
	done
	p95_of list 1000 "list (all pages)"
	p95_of get 300 "get nexusd"
	if [ "$LOGS_OK" -gt 0 ]; then
		p95_of logs 2000 "servicelog nexusd --tail 200"
	elif [ "$INIT" = openrc ]; then
		result L2-logs INFO "OpenRC: nexusd's logs are not readable through nexus here (TC-SV-L1 (3)); TC-SV-L2 times logs on systemd only"
	else
		result L2-logs NOT-RUN "nexusd's logs could not be read through nexus (TC-SV-L1 (3)): no log timing"
	fi
}

# --- TC-SV-L13: the audit trail -------------------------------------------------

# cnt ACTION OUTCOME PROBLEM: the audit records of TMPD/audit.tsv with them.
cnt() { awk -F'\t' -v a="$1" -v o="$2" -v p="$3" '$1 == a && $2 == o && $3 == p { n++ } END { print n + 0 }' "$TMPD/audit.tsv"; }

check_audit() {
	log ""
	note "== TC-SV-L13: the services audit records since $T0"
	NX_TIMEOUT=60
	if [ -n "$TOKEN_ID" ]; then
		log "  only this run's token's records (tokenId $TOKEN_ID, filtered by the CLI)"
		nx_quiet audit-all auditrecord get --filter provider=services --filter "after=$T0" --filter "tokenId=$TOKEN_ID" --limit 1000
	else
		log "  every token's records: run this when nothing else uses the services provider"
		nx_quiet audit-all auditrecord get --filter provider=services --filter "after=$T0" --limit 1000
	fi
	if [ "$NX_RC" -ne 0 ]; then
		result L13-audit NOT-RUN "nexus auditrecord get failed (exit $NX_RC): does the token hold audit:read?"
		return
	fi
	grep -q nextCursor "$TMPD/audit-all.err" && log "  (more than 1000 records: only the first page is counted)"
	awk -F'\t' '
	match($1, /^\[[0-9]+\]\./) {
		i = substr($1, 2, RLENGTH - 3); k = substr($1, RLENGTH + 1); v = $2; gsub(/^"|"$/, "", v)
		if (!(i in seen)) { seen[i] = 1; ord[n++] = i }
		if (k == "action") a[i] = v; else if (k == "outcome") o[i] = v; else if (k == "problem") p[i] = v; else if (k == "target") t[i] = v
	}
	END { for (j = 0; j < n; j++) { i = ord[j]; print a[i] "\t" o[i] "\t" p[i] "\t" t[i] } }' "$JF" >"$TMPD/audit.tsv"
	log "  count  action  outcome  problem  target:"
	sort "$TMPD/audit.tsv" | uniq -c | sed 's/^/    /' | tee -a "$TRANSCRIPT"
	_bad=
	_rw=$(awk -F'\t' '$1 ~ /^services\.services\.(list|get)$/' "$TMPD/audit.tsv" | wc -l | tr -d ' ')
	[ "$_rw" -eq 0 ] || _bad="$_bad $_rw record(s) for list or get;"
	for _a in start stop restart; do
		_want=0
		for _s in $CTL_SENT; do [ "$_s" != "$_a" ] || _want=$((_want + 1)); done
		_req=$(cnt "services.services.$_a" requested "")
		_suc=$(cnt "services.services.$_a" Succeeded "")
		log "  $_a: $_want sent to $TEST_SVC; $_req requested, $_suc Succeeded"
		[ "$_suc" -eq "$_want" ] || _bad="$_bad $_a: $_suc Succeeded for $_want sent;"
		[ "$_req" -ge "$_want" ] || _bad="$_bad $_a: $_req requested for $_want sent;"
	done
	_ref=$(awk -F'\t' '$2 == "refused" && $3 == "protected-service"' "$TMPD/audit.tsv" | wc -l | tr -d ' ')
	[ "$_ref" -eq "$REFUSALS_SENT" ] || _bad="$_bad $_ref refused protected-service for $REFUSALS_SENT refusals sent;"
	_lr=$(cnt services.services.logs requested "")
	_lc=$(cnt services.services.logs completed "")
	log "  logs: $LOGS_CALLS servicelog call(s); $_lr requested, $_lc completed"
	if [ "$INIT" = systemd ] || [ "$LOGS_OK" -gt 0 ]; then
		[ "$_lr" -eq "$LOGS_CALLS" ] && [ "$_lc" -eq "$LOGS_CALLS" ] || _bad="$_bad logs: $_lr requested and $_lc completed for $LOGS_CALLS calls;"
	else
		[ "$_lr" -eq "$LOGS_CALLS" ] || _bad="$_bad logs: $_lr requested for $LOGS_CALLS calls;"
	fi
	if [ -z "$_bad" ]; then
		result L13-audit PASS "each action, refusal and log read of this run is audited as expected; no record for list or get"
	else
		result L13-audit FAIL "$_bad"
	fi
}

# --- TC-SV-L6: no sandbox denial --------------------------------------------

check_journal() {
	log ""
	note "== TC-SV-L6: no sandbox denial in the journal since $T0_LOCAL"
	if [ "$INIT" != systemd ]; then
		result L6-journal INFO "OpenRC: no systemd sandbox; not applicable"
		return
	fi
	hrun "journalctl -u nexusd.service -u 'nexus-op-*' --since '$T0_LOCAL' --no-pager | grep -E 'SECCOMP|Operation not permitted|Permission denied'"
	_rc1=$HX_RC
	hrun "journalctl -k --since '$T0_LOCAL' --no-pager | grep -E 'audit: type=1326.*(nexusd|journalctl|systemctl)'"
	_rc2=$HX_RC
	if [ "$_rc1" -eq 1 ] && [ "$_rc2" -eq 1 ] && [ "$LOGS_OK" -gt 0 ]; then
		result L6-journal PASS "no SECCOMP, Operation not permitted or Permission denied from nexusd or its nexus-op-* units, and no seccomp audit line in the kernel log, since $T0_LOCAL; servicelog nexusd returned frames"
	else
		result L6-journal FAIL "a denial line was found (or the grep failed: exits $_rc1, $_rc2), or servicelog nexusd returned no frames. Record it; do NOT widen the unit (CON-1)"
	fi
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
		result token-env PASS "no child process's environment holds the token"
	fi
}

summary() {
	log ""
	log "== Summary"
	printf '%s\n' "$RESULTS" | while IFS='|' read -r id st detail; do
		[ -z "$id" ] || log "  $id  $st  $detail"
	done
	[ "$DOCKER" = 0 ] || log "  (Docker/Podman container: a dry run, not real-host evidence)"
	log "transcript: $TRANSCRIPT"
}

on_exit() {
	_rc=$?
	trap - EXIT
	rm -rf "$TMPD"
	exit "$_rc"
}

# --- main ------------------------------------------------------------------

trap on_exit EXIT
trap 'exit 130' INT TERM HUP

header
detect_init
setup_client_rc=0
setup_client || setup_client_rc=1
token_env_check
[ "$setup_client_rc" = 0 ] || [ "$YES" = 0 ] || refuse "NEXUS_TOKEN is not set (or pass --nexus-host). Create one on the host: nexusd token create --name services-check --scope services:read --scope services:logs --scope services:write --scope host:read --scope audit:read --expires 1d"

facts
plan
if [ "$YES" != 1 ]; then
	log ""
	refuse "nothing was changed: re-run with --yes to do the above"
fi
[ "$SVC_OK" = 1 ] || refuse "the services provider is not enabled (status '$CAP_STATUS'${CAP_REASON:+: $CAP_REASON}): set providers.services.enabled: true in $CONF and reload"

T0=$(date -u +%Y-%m-%dT%H:%M:%SZ)
T0_LOCAL=$(date '+%Y-%m-%d %H:%M:%S')
log ""
log "T0 (the start of the checks): $T0 ($T0_LOCAL local)"
check_caps
check_bootid
check_list
check_get
check_logs
check_refusals
if [ -n "$TEST_SVC" ]; then
	check_control
else
	result L4-control NOT-RUN "no --test-service: install one (--print-test-service NAME), allow-list it, reload, and run again"
fi
check_timings
check_audit
check_journal
token_check
summary
[ "$FAILS" -eq 0 ] || exit 1
[ "$INCONCL" -eq 0 ] || exit 3
exit 0
