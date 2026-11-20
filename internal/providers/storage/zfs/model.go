package zfs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// The model below is what the parsers return for both output forms (D-5).
// It is a domain model: the provider converts it into the spec's wire types
// (WI 4.2), so it carries no JSON tags and is never marshaled itself.

// Normalized pool health words (FR-4). RawHealth keeps the tool's own word.
const (
	HealthOnline    = "online"
	HealthDegraded  = "degraded"
	HealthFaulted   = "faulted"
	HealthOffline   = "offline"
	HealthUnavail   = "unavail"
	HealthRemoved   = "removed"
	HealthSuspended = "suspended"
	HealthUnknown   = "unknown"
)

// Vdev type words (FR-5, spec.Vdev.Type).
const (
	VdevRoot    = "root"
	VdevDisk    = "disk"
	VdevFile    = "file"
	VdevMirror  = "mirror"
	VdevRaidz1  = "raidz1"
	VdevRaidz2  = "raidz2"
	VdevRaidz3  = "raidz3"
	VdevDraid   = "draid"
	VdevSpare   = "spare"
	VdevLog     = "log"
	VdevCache   = "cache"
	VdevSpecial = "special"
	VdevDedup   = "dedup"
	VdevUnknown = "unknown"
)

// Scan function and state words (FR-6).
const (
	ScanScrub      = "scrub"
	ScanResilver   = "resilver"
	ScanErrorScrub = "errorscrub"
	ScanNone       = "none"
	ScanScanning   = "scanning"
	ScanPaused     = "paused"
	ScanFinished   = "finished"
	ScanCanceled   = "canceled"
	ScanUnknown    = "unknown"
)

// TruncationMarker ends a text the parsers cut at spec.StatusTextMaxBytes.
const TruncationMarker = "…[truncated]"

// ErrorCounts is a pool's read, write and checksum error counts (A-52).
type ErrorCounts struct{ Read, Write, Checksum uint64 }

// Pool is one pool as zpool list (the size fields) or zpool status (the vdev
// tree, scan, texts and permanent errors) reports it. A field the parsed
// command does not carry is nil; a field it should carry but did not is nil
// with a reason in Unavailable, keyed by the spec's wire field name.
type Pool struct {
	Name, Health, RawHealth string
	Size, Alloc, Free       *uint64
	Capacity, Frag          *int
	Dedup                   *float64
	ReadOnly                *bool
	Altroot                 *string
	// Vdevs is the vdev tree flattened in zpool status order (D-8).
	Vdevs []Vdev
	// VdevCount is the number of depth-1 entries: the top-level vdevs of
	// every class.
	VdevCount *int
	// Errors is, per counter, the sum over leaf vdevs, else the root vdev's
	// own count (DF-2 step 3, A-52).
	Errors         *ErrorCounts
	Scan           *Scan
	Status, Action *string
	// ErrorCount is the number of files with permanent errors; ErrorPaths
	// holds at most spec.MaxErrorPaths of them in tool order.
	ErrorCount          *uint64
	ErrorPaths          []string
	ErrorPathsTruncated bool
	Unavailable         map[string]string
}

// Vdev is one entry of the flat vdev list. Depth 0 is the root vdev named
// after the pool; Parent is the name of the entry it sits under. State is the
// state word zpool status prints, lower-cased (online, degraded, faulted,
// offline, removed, unavail, split, avail, inuse), else unknown.
type Vdev struct {
	Name, Type, State     string
	Depth                 int
	Parent                string
	Read, Write, Checksum *uint64
}

// Scan is a pool's scrub or resilver state (FR-6). Processed is the bytes
// repaired and Errors the errors found (A-5).
type Scan struct {
	Function, State                        string
	Start, End                             *time.Time
	Examined, ToExamine, Processed, Errors *uint64
}

// Dataset is one filesystem or volume (FR-7). Quota and Refquota are nil for
// none; Mountpoint and Mounted are nil where zfs list prints "-" (volumes).
type Dataset struct {
	Name, Pool, Type                    string
	Used, Avail, Refer, UsedBySnapshots *uint64
	Quota, Refquota, Reservation        *uint64
	Compression                         *string
	CompressRatio                       *float64
	Recordsize                          *uint64
	Mountpoint                          *string
	Mounted                             *bool
	Encryption, KeyStatus               *string
	Unavailable                         map[string]string
}

// Snapshot is one snapshot (FR-8). CreatedBy is the nexus:created-by
// property, empty when unset. Used, Refer and Holds are nil in the
// name,creation listing, which carries only the name and creation time.
type Snapshot struct {
	Name, Dataset      string
	CreatedAt          time.Time
	Used, Refer, Holds *uint64
	CreatedBy          string
	Unavailable        map[string]string
}

// FeaturesUpgradable reports whether any feature@ property of a zpool get
// map is disabled: a feature zpool upgrade would enable (A-37).
func FeaturesUpgradable(props map[string]string) bool {
	for k, v := range props {
		if strings.HasPrefix(k, "feature@") && v == "disabled" {
			return true
		}
	}
	return false
}

var healthWords = map[string]string{
	"ONLINE":    HealthOnline,
	"DEGRADED":  HealthDegraded,
	"FAULTED":   HealthFaulted,
	"OFFLINE":   HealthOffline,
	"UNAVAIL":   HealthUnavail,
	"REMOVED":   HealthRemoved,
	"SUSPENDED": HealthSuspended,
}

// normalizeHealth maps a pool health word to its FR-4 word; anything else,
// including the empty word and other cases, is unknown.
func normalizeHealth(raw string) string {
	if h, ok := healthWords[raw]; ok {
		return h
	}
	return HealthUnknown
}

var vdevStates = map[string]bool{
	"online": true, "degraded": true, "faulted": true, "offline": true, "removed": true,
	"unavail": true, "split": true, "avail": true, "inuse": true,
}

// vdevStateWord lower-cases a state word zpool status prints in its text
// form, or returns unknown.
func vdevStateWord(w string) string {
	l := strings.ToLower(w)
	if w == strings.ToUpper(w) && vdevStates[l] {
		return l
	}
	return HealthUnknown
}

// vdevStateJSON maps the JSON form's state word (vdev_state_str) and aux word
// to the word the text form prints for the same vdev (zpool_state_to_name).
func vdevStateJSON(state, aux string) string {
	switch state {
	case "CLOSED":
		return HealthOffline
	case "CANT_OPEN":
		switch aux {
		case "CORRUPT_DATA", "BAD_LOG":
			return HealthFaulted
		case "SPLIT_POOL":
			return "split"
		}
		return HealthUnavail
	}
	return vdevStateWord(state)
}

// classWords maps a vdev class to the type of its top-level leaf vdevs. The
// JSON form keys classes dedup, special, logs, l2cache and spares; the text
// form heads them dedup, special, logs, cache and spares.
var classWords = map[string]string{
	"dedup":   VdevDedup,
	"special": VdevSpecial,
	"logs":    VdevLog,
	"l2cache": VdevCache,
	"cache":   VdevCache,
	"spares":  VdevSpare,
}

// vdevType returns the FR-5 type of a vdev from the JSON vdev_type (empty in
// the text form, which then goes by the name zpool_vdev_name gives).
func vdevType(raw, name, class string, depth int, leaf bool) string {
	if depth == 0 {
		return VdevRoot
	}
	if class != "" && depth == 1 && leaf {
		return class
	}
	switch raw {
	case "":
		return typeFromName(name, leaf)
	case VdevDisk, VdevFile, VdevMirror, VdevDraid, VdevSpare:
		return raw
	case "raidz":
		if t := raidzFromName(name); t != "" {
			return t
		}
	}
	return VdevUnknown
}

func typeFromName(name string, leaf bool) string {
	if leaf {
		if strings.HasPrefix(name, "/") {
			return VdevFile
		}
		return VdevDisk
	}
	switch {
	case hasIDSuffix(name, "mirror-"):
		return VdevMirror
	case hasIDSuffix(name, "spare-"):
		return VdevSpare
	case strings.HasPrefix(name, "draid"):
		return VdevDraid
	}
	if t := raidzFromName(name); t != "" {
		return t
	}
	return VdevUnknown
}

func raidzFromName(name string) string {
	for _, t := range []string{VdevRaidz1, VdevRaidz2, VdevRaidz3} {
		if hasIDSuffix(name, t+"-") {
			return t
		}
	}
	return ""
}

// hasIDSuffix reports whether name is prefix followed by decimal digits.
func hasIDSuffix(name, prefix string) bool {
	id, ok := strings.CutPrefix(name, prefix)
	return ok && id != "" && strings.Trim(id, "0123456789") == ""
}

var scanFunctions = map[string]string{
	"SCRUB": ScanScrub, "RESILVER": ScanResilver, "ERRORSCRUB": ScanErrorScrub, "NONE": ScanNone,
}

var scanStates = map[string]string{
	"SCANNING": ScanScanning, "FINISHED": ScanFinished, "CANCELED": ScanCanceled, "NONE": ScanNone,
}

func scanWord(m map[string]string, raw string) string {
	if w, ok := m[raw]; ok {
		return w
	}
	return ScanUnknown
}

// finishPool computes VdevCount and Errors from the flat vdev list, once for
// both forms (DF-2 step 3). Each vdev's own counts are left as they are.
func finishPool(p *Pool) {
	if len(p.Vdevs) == 0 {
		return
	}
	n := 0
	for _, v := range p.Vdevs {
		if v.Depth == 1 {
			n++
		}
	}
	p.VdevCount = &n

	var leaves, root ErrorCounts
	rootSeen := false
	for i, v := range p.Vdevs {
		if v.Depth == 0 && !rootSeen {
			rootSeen = true
			root = ErrorCounts{val(v.Read), val(v.Write), val(v.Checksum)}
		}
		if i+1 == len(p.Vdevs) || p.Vdevs[i+1].Depth <= v.Depth {
			leaves.Read += val(v.Read)
			leaves.Write += val(v.Write)
			leaves.Checksum += val(v.Checksum)
		}
	}
	if leaves.Read == 0 {
		leaves.Read = root.Read
	}
	if leaves.Write == 0 {
		leaves.Write = root.Write
	}
	if leaves.Checksum == 0 {
		leaves.Checksum = root.Checksum
	}
	p.Errors = &leaves
}

func val(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

// boundText returns s as valid UTF-8 of at most spec.StatusTextMaxBytes,
// cut on a rune boundary and ended with TruncationMarker when it was longer.
// Every string the parsers return passes through it (NFR-3, FR-18).
func boundText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) <= spec.StatusTextMaxBytes {
		return s
	}
	cut := spec.StatusTextMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncationMarker
}

// keepPaths bounds a permanent-error path list to spec.MaxErrorPaths.
func keepPaths(p *Pool, paths []string) {
	n := min(len(paths), spec.MaxErrorPaths)
	p.ErrorPaths = make([]string, n)
	for i := range n {
		p.ErrorPaths[i] = boundText(paths[i])
	}
	p.ErrorPathsTruncated = len(paths) > spec.MaxErrorPaths
}

func unavailable(m *map[string]string, key, reason string) {
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[key] = reason
}

func ptr[T any](v T) *T { return &v }

// getter returns one column of a parsed row in the word form zfs and zpool
// print with -p (a JSON number as its decimal text), and whether the row has
// it. Both forms fill the model through the same fill functions, so the
// conversions agree by construction (NFR-4, A-43).
type getter func(col string) (string, bool)

// reasons collects Unavailable entries; src names the command.
type reasons struct {
	m   *map[string]string
	src string
}

func (r reasons) absent(key string) { unavailable(r.m, key, "not reported by "+r.src) }
func (r reasons) dash(key string)   { unavailable(r.m, key, r.src+" reported -") }
func (r reasons) bad(key, w, what string) {
	unavailable(r.m, key, boundText(fmt.Sprintf("%s reported %q, not %s", r.src, w, what)))
}

// uintField reads an exact integer. An absent column gets a reason; "-" is
// nil, with a reason only when dashReason is set.
func (r reasons) uintField(get getter, col, key string, dashReason bool) *uint64 {
	w, ok := get(col)
	switch {
	case !ok:
		r.absent(key)
		return nil
	case w == "-":
		if dashReason {
			r.dash(key)
		}
		return nil
	}
	n, err := strconv.ParseUint(w, 10, 64)
	if err != nil {
		r.bad(key, w, "an integer")
		return nil
	}
	return &n
}

// percentField reads capacity or fragmentation: an integer, with the % the
// string form adds.
func (r reasons) percentField(get getter, col, key string) *int {
	w, ok := get(col)
	switch {
	case !ok:
		r.absent(key)
		return nil
	case w == "-":
		r.dash(key)
		return nil
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(w, "%"), 10, 31)
	if err != nil {
		r.bad(key, w, "a percentage")
		return nil
	}
	return ptr(int(n))
}

// ratioField reads dedupratio or compressratio: "1.50" with -p, "1.50x"
// without.
func (r reasons) ratioField(get getter, col, key string) *float64 {
	w, ok := get(col)
	switch {
	case !ok:
		r.absent(key)
		return nil
	case w == "-":
		r.dash(key)
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(w, "x"), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || strings.ContainsAny(w, "eEpP_") {
		r.bad(key, w, "a ratio")
		return nil
	}
	return &f
}

// wordField reads a word; "-" is nil without a reason (not applicable).
func (r reasons) wordField(get getter, col, key string) *string {
	w, ok := get(col)
	switch {
	case !ok:
		r.absent(key)
		return nil
	case w == "-":
		return nil
	}
	return ptr(boundText(w))
}

// fillPoolList sets the zpool list fields (PoolListColumns) of p.
func fillPoolList(p *Pool, get getter, rawState string) {
	r := reasons{&p.Unavailable, "zpool list"}
	p.Size = r.uintField(get, "size", "sizeBytes", true)
	p.Alloc = r.uintField(get, "allocated", "allocatedBytes", true)
	p.Free = r.uintField(get, "free", "freeBytes", true)
	p.Capacity = r.percentField(get, "capacity", "capacityPercent")
	p.Frag = r.percentField(get, "fragmentation", "fragmentationPercent")
	p.Dedup = r.ratioField(get, "dedupratio", "dedupRatio")
	if h, ok := get("health"); ok {
		rawState = h
	}
	p.RawHealth = boundText(rawState)
	p.Health = normalizeHealth(rawState)
	// altroot "-" is "no altroot", not a field that could not be read.
	if a, ok := get("altroot"); ok && a != "-" {
		p.Altroot = ptr(boundText(a))
	}
	switch w, ok := get("readonly"); {
	case !ok:
		r.absent("readOnly")
	case w == "on":
		p.ReadOnly = ptr(true)
	case w == "off":
		p.ReadOnly = ptr(false)
	case w == "-":
		r.dash("readOnly")
	default:
		r.bad("readOnly", w, "on or off")
	}
}

// fillDataset sets the zfs list fields (DatasetColumns) of d. "-" is nil
// without a reason throughout: zfs list prints it where a property does not
// apply (a volume's mountpoint, mounted and recordsize, an unencrypted
// dataset's keystatus).
func fillDataset(d *Dataset, get getter, rawType string) {
	r := reasons{&d.Unavailable, "zfs list"}
	if t, ok := get("type"); ok {
		rawType = t
	}
	d.Type = boundText(strings.ToLower(rawType))
	d.Used = r.uintField(get, "used", "usedBytes", false)
	d.Avail = r.uintField(get, "avail", "availableBytes", false)
	d.Refer = r.uintField(get, "refer", "referencedBytes", false)
	d.UsedBySnapshots = r.uintField(get, "usedbysnapshots", "usedBySnapshotsBytes", false)
	d.Quota = r.quota(get, "quota", "quotaBytes")
	d.Refquota = r.quota(get, "refquota", "refquotaBytes")
	if w, ok := get("reservation"); ok && w == "none" {
		d.Reservation = ptr(uint64(0))
	} else {
		d.Reservation = r.uintField(get, "reservation", "reservationBytes", false)
	}
	d.Compression = r.wordField(get, "compression", "compression")
	d.CompressRatio = r.ratioField(get, "compressratio", "compressRatio")
	d.Recordsize = r.uintField(get, "recordsize", "recordsize", false)
	d.Mountpoint = r.wordField(get, "mountpoint", "mountpoint")
	switch w, ok := get("mounted"); {
	case !ok:
		r.absent("mounted")
	case w == "yes":
		d.Mounted = ptr(true)
	case w == "no":
		d.Mounted = ptr(false)
	case w == "-":
	default:
		r.bad("mounted", w, "yes or no")
	}
	d.Encryption = r.wordField(get, "encryption", "encryption")
	d.KeyStatus = r.wordField(get, "keystatus", "keyStatus")
}

// quota reads quota or refquota: 0 (-p), none and "-" are no quota (nil).
func (r reasons) quota(get getter, col, key string) *uint64 {
	if w, ok := get(col); ok && (w == "none" || w == "0") {
		return nil
	}
	n := r.uintField(get, col, key, false)
	if n != nil && *n == 0 {
		return nil
	}
	return n
}

// fillSnapshot sets a snapshot's fields from a row of SnapshotColumns or of
// SnapshotTimeColumns. It fails on a name without "@" or an unreadable
// creation time: such a row is not a snapshot.
func fillSnapshot(s *Snapshot, name string, get getter, full bool) error {
	ds, _, ok := strings.Cut(name, "@")
	if !ok || ds == "" {
		return fmt.Errorf("zfs list: snapshot name %q has no dataset@ part", boundText(name))
	}
	s.Name, s.Dataset = boundText(name), boundText(ds)
	c, ok := get("creation")
	if !ok {
		return fmt.Errorf("zfs list: snapshot %q has no creation time", s.Name)
	}
	sec, err := strconv.ParseInt(c, 10, 64)
	if err != nil || sec < 0 || sec > maxUnix {
		return fmt.Errorf("zfs list: snapshot %q creation %q is not epoch seconds", s.Name, boundText(c))
	}
	s.CreatedAt = time.Unix(sec, 0).UTC()
	if !full {
		return nil
	}
	r := reasons{&s.Unavailable, "zfs list"}
	s.Used = r.uintField(get, "used", "usedBytes", true)
	s.Refer = r.uintField(get, "refer", "referencedBytes", true)
	s.Holds = r.uintField(get, "userrefs", "holds", true)
	if v, ok := get(spec.CreatedByProperty); ok && v != "-" {
		s.CreatedBy = boundText(v)
	}
	return nil
}

// maxUnix bounds an epoch time to year 9999, so time values stay printable.
const maxUnix = 253402300799

func unixPtr(sec uint64) *time.Time {
	if sec == 0 || sec > maxUnix {
		return nil
	}
	return ptr(time.Unix(int64(sec), 0).UTC())
}
