package vzdump

import (
	"regexp"
	"strconv"
	"time"
)

// The vzdump name grammar (FR-9, A-10, A-12), after Proxmox's own:
// PVE::Storage::archive_info matches
// `vzdump-${type}-([1-9][0-9]{2,8})-(\d{4})_(\d{2})_(\d{2})-(\d{2})_(\d{2})_(\d{2})`
// as the stem, and derives `<stem>.log` and `<archive>.notes` from it;
// protection_file_path is `<archive>.protected`. PVE::VZDump writes the
// archive as `<stem>.<vma|tar>[.<comp>]`, first under the name with its last
// extension replaced by `.dat`, with its temporary directory `<stem>.tmp`.
// FR-9 narrows Proxmox's set to qemu and lxc guests and the zst, gz and lzo
// compressors; `<stem>.notes` is also accepted (A-10).
var (
	stemRE   = regexp.MustCompile(`^(vzdump-(qemu|lxc)-([1-9][0-9]{2,8})-([0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}))\.(.+)$`)
	suffixRE = regexp.MustCompile(`^(vma|tar)(?:\.(zst|gz|lzo))?(?:\.(notes|protected))?$`)
)

// stampLayout is the archive name's time, strftime "%Y_%m_%d-%H_%M_%S" in
// the node's local time zone.
const stampLayout = "2006_01_02-15_04_05"

type nameKind int

const (
	kindOther nameKind = iota
	kindArchive
	kindLog
	kindNotes
	kindProtected
	kindPartial // <stem>.dat, <stem>.vma.dat, <stem>.tar.dat: an archive being written
	kindTmpDir  // <stem>.tmp: vzdump's temporary directory, when a directory
)

// vzName is a parsed vzdump file name.
type vzName struct {
	kind nameKind
	// stem is vzdump-<type>-<vmid>-<YYYY_MM_DD-HH_MM_SS>.
	stem string
	// archive is the archive's name: the name itself for kindArchive, the
	// name less `.notes` or `.protected` for the archive-form companions,
	// and "" otherwise.
	archive     string
	guestType   string
	vmid        int
	timestamp   time.Time
	format      string
	compression string
}

// parseName classifies a file name by the vzdump grammar. Anything else,
// including an impossible date such as 2026_13_40, is kindOther.
func parseName(name string) vzName {
	m := stemRE.FindStringSubmatch(name)
	if m == nil {
		return vzName{}
	}
	// The timestamp names no zone (A-13): it is kept naive, as UTC, and is
	// never converted.
	ts, err := time.ParseInLocation(stampLayout, m[4], time.UTC)
	if err != nil {
		return vzName{}
	}
	vmid, err := strconv.Atoi(m[3])
	if err != nil {
		return vzName{}
	}
	n := vzName{stem: m[1], guestType: m[2], vmid: vmid, timestamp: ts}
	suffix := m[5]
	switch suffix {
	case "log":
		n.kind = kindLog
		return n
	case "notes":
		n.kind = kindNotes
		return n
	case "dat":
		n.kind = kindPartial
		return n
	case "vma.dat", "tar.dat":
		n.kind, n.format = kindPartial, suffix[:3]
		return n
	case "tmp":
		n.kind = kindTmpDir
		return n
	}
	s := suffixRE.FindStringSubmatch(suffix)
	if s == nil {
		return vzName{}
	}
	n.format, n.compression = s[1], s[2]
	switch s[3] {
	case "":
		n.kind, n.archive = kindArchive, name
	case "notes":
		n.kind, n.archive = kindNotes, name[:len(name)-len(".notes")]
	case "protected":
		n.kind, n.archive = kindProtected, name[:len(name)-len(".protected")]
	}
	return n
}
