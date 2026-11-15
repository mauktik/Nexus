#!/bin/sh
# capture.sh [--no-redact] HOST-ROOT OUTDIR
#
# Copies the host provider's allow-listed files (architecture.md WI 2.2, the
# allow-list table) from HOST-ROOT into OUTDIR/fs/<same path>, takes a second
# sample of the dynamic files 10 s later into OUTDIR/t1/, and records the
# host's own tools into OUTDIR/tools/. Then it redacts host identifiers
# (MACs, public IPv4, global IPv6, the hostname) with one consistent mapping
# per run and checks that none is left. README.md in this directory documents
# the layout; PROVENANCE in OUTDIR records host, kernel, date, the unit and
# the redaction. POSIX sh; run as root on the host being captured.
#
# Environment (optional): UNDER_UNIT overrides the detected `under-unit:`
# PROVENANCE value; CAPTURE_NOTE adds a `note:` line describing the host.
#
# --no-redact is for local debugging only: its output must never be
# committed (TC-HP-135 fails on it).
set -eu

log() { printf 'capture: %s\n' "$*" >&2; }
die() {
	log "FAIL: $*"
	exit 1
}

REDACT=1
while [ $# -gt 0 ]; do
	case $1 in
	--no-redact) REDACT=0 ;;
	-*) die "unknown option $1" ;;
	*) break ;;
	esac
	shift
done
[ $# -eq 2 ] || die "usage: capture.sh [--no-redact] HOST-ROOT OUTDIR"
[ "$(id -u)" = 0 ] || die "run as root"
[ -d "$1/proc" ] || die "$1/proc is not a directory"
mkdir -p "$2"
[ -z "$(ls -A "$2")" ] || die "OUTDIR $2 is not empty"
SCRIPT=$(cd "$(dirname "$0")" && pwd -P)/$(basename "$0")
OUT=$(cd "$2" && pwd -P)
cd "$1"
WORK=$OUT/.work
mkdir -p "$OUT/fs" "$OUT/t1" "$OUT/tools" "$WORK"
: >"$OUT/UNREADABLE"
: >"$WORK/tools"

all_digits() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
}

# copy_read REL DEST [quiet]: cat HOST-ROOT/REL into DEST/REL, a flat file,
# so sysfs symlinks resolve. A path absent on the host stays absent; a read
# error (EINVAL from a down NIC's speed, EACCES) is listed in UNREADABLE
# unless quiet (a process that exited between the glob and the read).
copy_read() {
	[ -e "$1" ] || return 0
	[ ! -d "$1" ] || return 0
	mkdir -p "$2/$(dirname "$1")"
	if cat "$1" >"$2/$1.part" 2>"$WORK/err"; then
		mv "$2/$1.part" "$2/$1"
	else
		rm -f "$2/$1.part"
		[ -n "${3:-}" ] || printf '%s: %s\n' "$1" "$(tr '\n' ' ' <"$WORK/err")" >>"$OUT/UNREADABLE"
	fi
}

# copy_dynamic DEST: the files the sampler diffs between ticks.
copy_dynamic() {
	for f in proc/stat proc/diskstats proc/net/dev; do
		copy_read "$f" "$1"
	done
	for d in proc/[0-9]*; do
		all_digits "${d#proc/}" || continue
		copy_read "$d/stat" "$1" quiet
		copy_read "$d/status" "$1" quiet
	done
}

# Read entries other than the dynamic ones, as globs relative to HOST-ROOT.
# `*` in proc/*/ is the digits-only pid (copy_dynamic); cpu* is digits-only.
READ_GLOBS='
proc/meminfo proc/loadavg proc/uptime proc/vmstat proc/cpuinfo
proc/self/mountinfo proc/1/mountinfo
proc/net/route proc/net/ipv6_route
proc/pressure/cpu proc/pressure/memory proc/pressure/io
proc/sys/kernel/hostname proc/sys/kernel/osrelease
sys/class/net/*/operstate sys/class/net/*/address sys/class/net/*/mtu
sys/class/net/*/speed sys/class/net/*/type sys/class/net/*/ifindex
sys/block/*/size sys/block/*/removable sys/block/*/dev
sys/block/*/queue/rotational sys/block/*/device/model sys/block/*/loop/backing_file
sys/block/*/*/size sys/block/*/*/dev sys/block/*/*/partition
sys/class/hwmon/*/name sys/class/hwmon/*/temp*_input sys/class/hwmon/*/temp*_label
sys/class/thermal/thermal_zone*/type sys/class/thermal/thermal_zone*/temp
sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq
sys/devices/system/cpu/cpu*/topology/core_id
sys/devices/system/cpu/cpu*/topology/physical_package_id
sys/class/dmi/id/sys_vendor sys/class/dmi/id/product_name
sys/class/dmi/id/product_version sys/class/dmi/id/bios_vendor
sys/class/dmi/id/bios_version sys/hypervisor/type
etc/os-release etc/timezone etc/localtime etc/resolv.conf etc/passwd
run/systemd/resolve/resolv.conf run/systemd/container
var/run/reboot-required.pkgs
'

# Stat-only entries: recorded as empty placeholders of the type stat reports
# after following symlinks, never copied. The two apt paths mirror
# apt.RebootMarkerWriters (internal/providers/apt/provider.go).
STAT_GLOBS='
sys/class/net/*/device
run/.containerenv .dockerenv proc/vz proc/bc
var/run/reboot-required
usr/share/update-notifier/notify-reboot-required
usr/share/perl5/NeedRestart/UI/Ubuntu.pm
lib/modules/*
usr/bin/chronyc run/systemd/timesync run/chrony/chronyd.pid run/ntpd.pid
'

# List entries: directories created by the entries above, or empty.
LIST_GLOBS='proc sys/class/net sys/block sys/block/* sys/class/hwmon sys/class/hwmon/* sys/class/thermal'

# run_tool NAME CMD...: the host's own tool output into tools/NAME, with
# stderr; the argv and exit code go to PROVENANCE.
run_tool() {
	name=$1
	shift
	rc=0
	"$@" >"$OUT/tools/$name" 2>&1 || rc=$?
	printf 'tool: %s = %s (exit %s)\n' "$name" "$*" "$rc" >>"$WORK/tools"
}

# check_sensitive is the NFR-6 guard: none of these names may ever appear in
# OUTDIR (TC-HP-22).
check_sensitive() {
	bad=$(find "$OUT" \( -name product_serial -o -name product_uuid -o -name board_serial \
		-o -name chassis_serial -o -name chassis_asset_tag -o -name cmdline -o -name environ \
		-o -name kmsg -o -name shadow -o -name gshadow -o -name arp \) -print)
	if [ -n "$bad" ]; then
		log "FAIL: sensitive file in OUTDIR: $bad"
		exit 1
	fi
}

# 1. vmstat over the same 10 s as the two samples; first sample; second.
log "sampling (10 s)"
vmstat 10 2 >"$OUT/tools/vmstat" 2>&1 &
VMSTAT=$!
copy_dynamic "$OUT/fs"
sleep 10
copy_dynamic "$OUT/t1"

# 2. The other read entries.
log "copying allow-listed files"
for g in $READ_GLOBS; do
	for f in $g; do
		case $f in
		sys/devices/system/cpu/cpu*)
			c=${f#sys/devices/system/cpu/cpu}
			all_digits "${c%%/*}" || continue
			;;
		esac
		copy_read "$f" "$OUT/fs"
	done
done

# /etc/passwd keeps system accounts only: uid < 1000, and 65534 (nobody).
if [ -f "$OUT/fs/etc/passwd" ]; then
	PASSWD_ALL=$(grep -c . "$OUT/fs/etc/passwd" || true)
	awk -F: '$3 + 0 < 1000 || $3 + 0 == 65534' "$OUT/fs/etc/passwd" >"$WORK/passwd"
	cat "$WORK/passwd" >"$OUT/fs/etc/passwd"
	PASSWD_KEPT=$(grep -c . "$OUT/fs/etc/passwd" || true)
fi

# 3. Stat-only placeholders, then list directories.
: >"$WORK/placeholders"
for g in $STAT_GLOBS; do
	for f in $g; do
		if [ -d "$f" ]; then
			mkdir -p "$OUT/fs/$f"
		elif [ -e "$f" ]; then
			mkdir -p "$OUT/fs/$(dirname "$f")"
			: >"$OUT/fs/$f"
			printf 'file fs/%s\n' "$f" >>"$WORK/placeholders"
		fi
	done
done
for g in $LIST_GLOBS; do
	for d in $g; do
		[ ! -d "$d" ] || mkdir -p "$OUT/fs/$d"
	done
done

# 4. The host's tools.
log "recording tools"
if [ -L etc/localtime ]; then
	run_tool readlink-localtime readlink etc/localtime
fi
run_tool free free -b
if df -B1 --output=target / >/dev/null 2>&1; then
	run_tool df df -B1 --output=target,size,used,avail,pcent,itotal,iused,iavail
else
	run_tool df df -B1
	run_tool df-i df -i
fi
if command -v lsblk >/dev/null 2>&1; then
	run_tool lsblk lsblk -b -J -o NAME,SIZE,ROTA,RM,MODEL,TYPE,MOUNTPOINTS
fi
if ip -j addr >/dev/null 2>&1; then
	run_tool ip-addr ip -j addr
	run_tool ip-route ip -j route
else
	run_tool ip-addr ip addr
	run_tool ip-route ip route
fi
run_tool uname-rm uname -rm
run_tool nproc nproc
run_tool hostname hostname
cp etc/os-release "$OUT/tools/os-release"
printf 'tool: os-release = cat etc/os-release (exit 0)\n' >>"$WORK/tools"
if [ -d run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	run_tool systemctl-failed systemctl --failed --plain --no-legend
elif command -v rc-status >/dev/null 2>&1; then
	run_tool rc-status-crashed rc-status --crashed
fi
if command -v systemd-detect-virt >/dev/null 2>&1; then
	run_tool systemd-detect-virt systemd-detect-virt
fi
if command -v timedatectl >/dev/null 2>&1; then
	run_tool timedatectl timedatectl show -p NTPSynchronized --value
elif command -v chronyc >/dev/null 2>&1; then
	run_tool chronyc-tracking chronyc -c tracking
fi

# Mount points of host mountinfo (octal escapes decoded), in file order,
# each once: statfs.txt is the F-STATFS Usage source, stat-mounts.txt its
# IsDir source (%F) and A-38 evidence (%d, never read by production).
MI=proc/1/mountinfo
[ -r "$MI" ] || MI=proc/self/mountinfo
awk '
function unoct(s,   out) {
	out = ""
	while (match(s, /\\[0-7][0-7][0-7]/)) {
		out = out substr(s, 1, RSTART - 1) sprintf("%c", substr(s, RSTART + 1, 1) * 64 + substr(s, RSTART + 2, 1) * 8 + substr(s, RSTART + 3, 1))
		s = substr(s, RSTART + 4)
	}
	return out s
}
{ m = unoct($5); if (!(m in seen)) { seen[m] = 1; print m } }' "$MI" >"$WORK/mounts"
ROOTPFX=$(pwd -P)
ROOTPFX=${ROOTPFX%/}
: >"$OUT/tools/statfs.txt"
: >"$OUT/tools/stat-mounts.txt"
while IFS= read -r mp; do
	if ! stat -f -c '%b %f %a %S %c %d %n' "$ROOTPFX$mp" >>"$OUT/tools/statfs.txt" 2>"$WORK/err"; then
		printf 'statfs %s: %s\n' "$mp" "$(tr '\n' ' ' <"$WORK/err")" >>"$OUT/UNREADABLE"
	fi
	if ! stat -c '%d %F %n' "$ROOTPFX$mp" >>"$OUT/tools/stat-mounts.txt" 2>"$WORK/err"; then
		printf 'stat %s: %s\n' "$mp" "$(tr '\n' ' ' <"$WORK/err")" >>"$OUT/UNREADABLE"
	fi
done <"$WORK/mounts"
printf "tool: statfs.txt = stat -f -c '%%b %%f %%a %%S %%c %%d %%n' <each %s mount point> (exit 0)\n" "$MI" >>"$WORK/tools"
printf "tool: stat-mounts.txt = stat -c '%%d %%F %%n' <each %s mount point> (exit 0)\n" "$MI" >>"$WORK/tools"

wait "$VMSTAT" || true
printf 'tool: vmstat = vmstat 10 2, concurrent with fs/ and t1/ (exit 0)\n' >>"$WORK/tools"
check_sensitive

# 5. Redaction (A-41). One awk run over every text file of fs/, t1/ and
# tools/ in sorted path order, so the mapping is first-seen and consistent
# across files. Binary files (with a NUL, such as etc/localtime) are skipped;
# a file without a final newline keeps it missing.
ID=$(sed -n 's/^ID="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' etc/os-release)
NEWHOST=${ID:-host}-capture

# shellcheck disable=SC2016 # An awk program, not shell.
REDACT_AWK='
function hv(c) { return index("0123456789abcdef", tolower(c)) - 1 }
function hn(s,   i, n) { n = 0; for (i = 1; i <= length(s); i++) n = n * 16 + hv(substr(s, i, 1)); return n }
function bad(what) { if (mode == "check") { print FILENAME ": " what; nbad++ } }
function ismac(t) { return t ~ macre }
function mac(t,   l) {
	l = tolower(t)
	if (l == "00:00:00:00:00:00" || l == "ff:ff:ff:ff:ff:ff") return t
	if (mode == "check") { if (l !~ /^02:00:00:00:00:/) bad("MAC " t); return t }
	if (!(l in macmap)) {
		if (++cnt["mac"] > 255) { print "too many MACs" > "/dev/stderr"; exit 2 }
		macmap[l] = sprintf("02:00:00:00:00:%02x", cnt["mac"])
	}
	return macmap[l]
}
function v4ok(o,   v, inv, a, b) {
	a = o[1] + 0; b = o[2] + 0
	if (a == 10 || a == 127) return 1
	if (a == 172 && b >= 16 && b <= 31) return 1
	if (a == 192 && b == 168) return 1
	if (a == 169 && b == 254) return 1
	if (a == 100 && b >= 64 && b <= 127) return 1
	if (a == 192 && b == 0 && o[3] + 0 == 2) return 1
	# Contiguous netmasks, 0.0.0.0 and 255.255.255.255 are not addresses.
	v = ((a * 256 + b) * 256 + o[3]) * 256 + o[4]
	inv = 4294967295 - v + 1
	while (inv > 1 && inv % 2 == 0) inv /= 2
	return inv == 1
}
function v4split(t, o,   i, n) {
	n = split(t, o, ".")
	if (n != 4) return 0
	for (i = 1; i <= 4; i++) if (o[i] !~ /^[0-9]+$/ || length(o[i]) > 3 || o[i] + 0 > 255) return 0
	return 1
}
function v4(t,   o, k) {
	if (!v4split(t, o) || v4ok(o)) return t
	k = (o[1] + 0) "." (o[2] + 0) "." (o[3] + 0) "." (o[4] + 0)
	if (mode == "check") { bad("IPv4 " t); return t }
	if (!(k in v4map)) {
		if (++cnt["ipv4"] > 254) { print "too many IPv4 addresses" > "/dev/stderr"; exit 2 }
		v4map[k] = "192.0.2." cnt["ipv4"]
	}
	return v4map[k]
}
# v6full expands a textual IPv6 address to 32 lower-case hex digits, or "".
function v6full(t,   p, n, L, R, nl, nr, i, g, out) {
	if (t ~ /:::/) return ""
	n = split(t, p, "::")
	if (n > 2) return ""
	if (n == 2) {
		nl = p[1] == "" ? 0 : split(p[1], L, ":")
		nr = p[2] == "" ? 0 : split(p[2], R, ":")
		if (nl + nr > 7) return ""
		for (i = 1; i <= 8 - nl - nr; i++) L[nl + i] = "0"
		for (i = 1; i <= nr; i++) L[8 - nr + i] = R[i]
	} else if (split(t, L, ":") != 8) return ""
	out = ""
	for (i = 1; i <= 8; i++) {
		g = L[i]
		if (g !~ /^[0-9A-Fa-f]+$/ || length(g) > 4) return ""
		out = out substr("0000", 1, 4 - length(g)) tolower(g)
	}
	return out
}
# v6map maps a 2000::/3 address outside 2001:db8::/32 (32 hex) to
# 2001:db8:0:N::M, N per distinct /64 prefix, M per distinct interface ID.
function v6map(f,   p, i) {
	if (f !~ /^[23]/ || f ~ /^20010db8/) return f
	if (mode == "check") { bad("IPv6 " f); return f }
	p = substr(f, 1, 16); i = substr(f, 17, 16)
	if (!(p in pfxmap)) pfxmap[p] = sprintf("%04x", ++cnt["ipv6-prefix"])
	if (i != "0000000000000000" && !(i in iidmap)) iidmap[i] = sprintf("%016x", ++cnt["ipv6-iid"])
	return "20010db80000" pfxmap[p] (i == "0000000000000000" ? i : iidmap[i])
}
function v6text(f,   m) {
	m = hn(substr(f, 17, 16))
	return sprintf("2001:db8:0:%x::", hn(substr(f, 13, 4))) (m ? sprintf("%x", m) : "")
}
function hexcolon(t,   f, g) {
	if (ismac(t)) return mac(t)
	if (index(t, ":") == 0) return t
	f = v6full(t)
	if (f == "") return t
	g = v6map(f)
	return g == f ? t : v6text(g)
}
function scan(s, re, kind,   out, tok) {
	out = ""
	while (match(s, re)) {
		tok = substr(s, RSTART, RLENGTH)
		out = out substr(s, 1, RSTART - 1) (kind == "hc" ? hexcolon(tok) : v4(tok))
		s = substr(s, RSTART + RLENGTH)
	}
	return out s
}
# route4 maps one little-endian hex column of /proc/net/route.
function route4(h,   o, t, r) {
	if (h !~ /^[0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f]$/) return h
	t = hn(substr(h, 7, 2)) "." hn(substr(h, 5, 2)) "." hn(substr(h, 3, 2)) "." hn(substr(h, 1, 2))
	r = v4(t)
	if (r == t) return h
	split(r, o, ".")
	return sprintf("%02X%02X%02X%02X", o[4], o[3], o[2], o[1])
}
function flush() {
	if (cur == "") return
	if (mode == "redact") {
		if (!(cur in nonl)) printf "\n" > (cur ".rd")
		close(cur ".rd")
	}
}
BEGIN {
	H = "[0-9A-Fa-f]"
	macre = "^" H H ":" H H ":" H H ":" H H ":" H H ":" H H "$"
	while ((getline l < nonlfile) > 0) nonl[l] = 1
	cur = ""
}
FNR == 1 { flush(); cur = FILENAME }
{
	line = $0
	if (FILENAME ~ /\/fs\/proc\/net\/route$/ && FNR > 1) {
		n = split(line, a, "\t")
		if (n >= 3) { a[2] = route4(a[2]); a[3] = route4(a[3]) }
		line = a[1]
		for (i = 2; i <= n; i++) line = line "\t" a[i]
	} else if (FILENAME ~ /\/fs\/proc\/net\/ipv6_route$/) {
		n = split(line, a, "[ ]")
		for (i = 1; i <= 5 && i <= n; i += 2)
			if (a[i] ~ /^[0-9a-f]+$/ && length(a[i]) == 32) a[i] = v6map(a[i])
		line = a[1]
		for (i = 2; i <= n; i++) line = line " " a[i]
	}
	line = scan(line, "[0-9A-Fa-f:]+", "hc")
	line = scan(line, "[0-9.]+", "v4")
	if (mode == "redact") printf "%s%s", (FNR > 1 ? "\n" : ""), line > (cur ".rd")
}
END {
	flush()
	if (mode == "check") exit (nbad > 0 ? 1 : 0)
	printf "mac=%d ipv4=%d ipv6-prefix=%d ipv6-iid=%d", cnt["mac"], cnt["ipv4"], cnt["ipv6-prefix"], cnt["ipv6-iid"] > statsfile
}'

text_files() {
	find "$OUT/fs" "$OUT/t1" "$OUT/tools" -type f | LC_ALL=C sort | while IFS= read -r f; do
		# shellcheck disable=SC2094 # Both only read $f.
		if tr -d '\000' <"$f" | cmp -s - "$f"; then
			printf '%s\n' "$f"
		fi
	done
}

if [ "$REDACT" = 1 ]; then
	log "redacting"
	text_files >"$WORK/text"
	: >"$WORK/nonl"
	while IFS= read -r f; do
		if [ -s "$f" ] && [ -n "$(tail -c 1 "$f")" ]; then
			printf '%s\n' "$f" >>"$WORK/nonl"
		fi
	done <"$WORK/text"
	# shellcheck disable=SC2046 # One path per line, no spaces (proc, sys, tools names).
	awk -v mode=redact -v nonlfile="$WORK/nonl" -v statsfile="$WORK/stats" "$REDACT_AWK" $(cat "$WORK/text")
	while IFS= read -r f; do
		if [ -f "$f.rd" ]; then
			cmp -s "$f" "$f.rd" || cat "$f.rd" >"$f"
			rm -f "$f.rd"
		fi
	done <"$WORK/text"
	for f in "$OUT/fs/proc/sys/kernel/hostname" "$OUT/tools/hostname"; do
		[ ! -f "$f" ] || printf '%s\n' "$NEWHOST" >"$f"
	done
	# Final check: nothing identifying is left.
	text_files >"$WORK/text"
	# shellcheck disable=SC2046 # As above.
	if ! awk -v mode=check -v nonlfile=/dev/null "$REDACT_AWK" $(cat "$WORK/text") >"$WORK/left"; then
		cat "$WORK/left" >&2
		die "redaction left identifiers in OUTDIR"
	fi
	for f in "$OUT/fs/proc/sys/kernel/hostname" "$OUT/tools/hostname"; do
		[ ! -f "$f" ] || [ "$(cat "$f")" = "$NEWHOST" ] || die "$f is not $NEWHOST"
	done
	REDACTION="mac,ipv4,ipv6,hostname"
	REDACTION_COUNTS="$(cat "$WORK/stats") hostname=1"
else
	log "WARNING: --no-redact: this output must not be committed"
	REDACTION=off
	REDACTION_COUNTS=none
fi

# 6. Placeholders: stat-only empty files, and every empty directory (git
# keeps neither an empty directory nor, so, these; F-FS recreates them).
(cd "$OUT" && find fs t1 -type d -empty | LC_ALL=C sort | sed 's/^/dir /') >>"$WORK/placeholders"
LC_ALL=C sort "$WORK/placeholders" >"$OUT/PLACEHOLDERS"

# 7. PROVENANCE: one key: value line per fact.
if [ -n "${UNDER_UNIT:-}" ]; then
	under=$UNDER_UNIT
elif [ -n "${INVOCATION_ID:-}" ]; then
	under="yes: systemd unit $(sed -n 's|^0::||p' /proc/self/cgroup)"
else
	under="no"
fi
pretty=$(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' etc/os-release)
virt=none
if [ -s "$OUT/tools/systemd-detect-virt" ]; then
	virt=$(cat "$OUT/tools/systemd-detect-virt")
elif [ -e .dockerenv ]; then
	virt="docker (/.dockerenv)"
fi
{
	printf 'host: %s (%s, %s, virtualization %s)\n' "$NEWHOST" "$pretty" "$(uname -m)" "$virt"
	printf 'kernel: %s\n' "$(uname -r)"
	printf 'date: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf 'under-unit: %s\n' "$under"
	printf 'process: %s\n' "$(grep -E '^(CapEff|CapBnd|NoNewPrivs|Seccomp):' /proc/self/status | tr -s '\t\n' '  ')"
	[ -z "${CAPTURE_NOTE:-}" ] || printf 'note: %s\n' "$CAPTURE_NOTE"
	printf 'redaction: %s\n' "$REDACTION"
	printf 'redaction-counts: %s\n' "$REDACTION_COUNTS"
	printf 'kept: private, ULA, link-local and loopback addresses, DNS servers, mount sources and process comm names (not sensitive lab data; docs/verification/pihole-live.md already carries the lab private addresses)\n'
	printf 'passwd: reduced to uid < 1000 or uid 65534 (%s of %s entries kept)\n' "${PASSWD_KEPT:-0}" "${PASSWD_ALL:-0}"
	printf 'mountinfo: %s\n' "$MI"
	printf 'script: capture.sh sha256 %s\n' "$(sha256sum "$SCRIPT" | cut -d' ' -f1)"
	cat "$WORK/tools"
} >"$OUT/PROVENANCE"

rm -rf "$WORK"
check_sensitive
log "done: $OUT"
