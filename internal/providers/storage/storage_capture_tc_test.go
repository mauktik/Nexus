// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

// Tests of planning/storage-provider/test-cases.md TC-STG-3 to TC-STG-6
// (WI 1.1). TC-STG-7 and TC-STG-8 (WI 1.2) are in
// storage_capture_host_tc_test.go.

package storage

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const captureRoot = "testdata/captures"

// captureProvenance is a PROVENANCE file: `key: value` lines, and for a
// key with an empty value (commands, files) the indented `name: value`
// lines under it.
type captureProvenance struct {
	keys  map[string]string
	lists map[string][]captureEntry
}

type captureEntry struct{ name, value string }

func readCaptureProvenance(t *testing.T, dir string) captureProvenance {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "PROVENANCE"))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	p := captureProvenance{keys: map[string]string{}, lists: map[string][]captureEntry{}}
	cur := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") {
			name, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
			if !ok || cur == "" {
				t.Errorf("%s/PROVENANCE: stray line %q", dir, line)
				continue
			}
			p.lists[cur] = append(p.lists[cur], captureEntry{name, value})
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Errorf("%s/PROVENANCE: not a key line: %q", dir, line)
			continue
		}
		cur = k
		p.keys[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s/PROVENANCE: %v", dir, err)
	}
	return p
}

func (p captureProvenance) entry(list, name string) (string, bool) {
	for _, e := range p.lists[list] {
		if e.name == name {
			return e.value, true
		}
	}
	return "", false
}

// captureDirs lists the directories under testdata/captures.
func captureDirs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(captureRoot)
	if err != nil {
		t.Fatalf("%s: %v", captureRoot, err)
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(captureRoot, e.Name()))
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no capture directory under %s", captureRoot)
	}
	return dirs
}

// vectorFiles lists the files of a capture directory other than PROVENANCE.
func vectorFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() != "PROVENANCE" {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeVector(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func hasOutputVersion(m map[string]any) bool {
	ov, ok := m["output_version"].(map[string]any)
	if !ok {
		return false
	}
	_, cmd := ov["command"].(string)
	_, major := ov["vers_major"].(json.Number)
	return cmd && major
}

var (
	reExitLine  = regexp.MustCompile(`^Exit [0-9]+$`)
	reUTCOffset = regexp.MustCompile(`^[+-][0-9]{4}$`)
	// An IANA zone name (Europe/Berlin, America/Argentina/Buenos_Aires,
	// Etc/GMT+5), UTC, or unknown.
	reTimezone = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(/[A-Za-z0-9_+.-]+)*$`)
	reHex40    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// TC-STG-3: every capture directory has a complete PROVENANCE and the Exit and stderr conventions. Owner WI 1.1. Requirements: NFR-4, NFR-8, NFR-8a.
func TestTCSTG3_EveryCaptureDirectoryHasACompletePROVENANCE(t *testing.T) {
	required := []string{"host", "source", "date", "os-release", "zfs-version", "kernel", "captured-as", "commands", "options", "redaction"}
	for _, dir := range captureDirs(t) {
		p := readCaptureProvenance(t, dir)
		for _, k := range required {
			if _, ok := p.keys[k]; !ok {
				t.Errorf("%s/PROVENANCE: no %s", dir, k)
			}
		}
		if len(p.lists["commands"]) == 0 {
			t.Errorf("%s/PROVENANCE: commands lists nothing", dir)
		}
		switch src := p.keys["source"]; src {
		case "host":
			if p.keys["captured-as"] != "root" {
				t.Errorf("%s: captured-as %q, want root", dir, p.keys["captured-as"])
			}
			if _, err := time.Parse(time.RFC3339, p.keys["date"]); err != nil {
				t.Errorf("%s: date %q is not RFC 3339: %v", dir, p.keys["date"], err)
			}
			// The zone the capture's text-form dates were printed in (A-60):
			// an IANA name, UTC or unknown, and the offset at `date`.
			if tz := p.keys["timezone"]; !reTimezone.MatchString(tz) {
				t.Errorf("%s: timezone %q is not an IANA name, UTC or unknown", dir, tz)
			}
			if off := p.keys["utc-offset"]; !reUTCOffset.MatchString(off) {
				t.Errorf("%s: utc-offset %q does not match %s", dir, off, reUTCOffset)
			}
			for _, e := range p.lists["commands"] {
				out := filepath.Join(dir, filepath.FromSlash(e.name))
				b, err := os.ReadFile(out)
				if err != nil {
					t.Errorf("%s: commands names %s: %v", dir, e.name, err)
					continue
				}
				lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
				if !reExitLine.MatchString(lines[len(lines)-1]) {
					t.Errorf("%s: %s does not end with an `Exit <integer>` line", dir, e.name)
				}
				if _, err := os.Stat(out + ".stderr"); err != nil {
					t.Errorf("%s: %s has no .stderr sibling: %v", dir, e.name, err)
				}
			}
		case "upstream":
		case "synthetic":
			for _, f := range vectorFiles(t, dir) {
				v, ok := p.entry("files", f)
				src, found := strings.CutPrefix(v, "derived-from ")
				if !ok || !found {
					t.Errorf("%s: PROVENANCE files names no source vector for %s", dir, f)
					continue
				}
				src, _, _ = strings.Cut(src, ";")
				if _, err := os.Stat(filepath.Join(captureRoot, filepath.FromSlash(src))); err != nil {
					t.Errorf("%s: %s derives from %s: %v", dir, f, src, err)
				}
			}
		default:
			t.Errorf("%s: source %q, want host, upstream or synthetic", dir, src)
		}
	}
}

// TC-STG-4: upstream OpenZFS 2.4.1 vectors are verbatim and carry their source. Owner WI 1.1. Requirements: NFR-4, NFR-8.
func TestTCSTG4_UpstreamOpenZFS241VectorsAre(t *testing.T) {
	dir := filepath.Join(captureRoot, "upstream-openzfs-2.4.1")
	p := readCaptureProvenance(t, dir)
	if p.keys["source"] != "upstream" {
		t.Errorf("source %q, want upstream", p.keys["source"])
	}
	if p.keys["tag"] != "zfs-2.4.1" {
		t.Errorf("tag %q, want zfs-2.4.1", p.keys["tag"])
	}
	commit := p.keys["commit"]
	if !reHex40.MatchString(commit) {
		t.Fatalf("commit %q is not a 40-hex SHA", commit)
	}
	blob := "https://github.com/openzfs/zfs/blob/" + commit + "/"
	reMan := regexp.MustCompile(regexp.QuoteMeta(blob) + `man/man8/(zpool-list|zpool-status|zpool-get|zfs-list)\.8\b`)
	reSum := regexp.MustCompile(`file sha256 ([0-9a-f]{64})`)
	files := vectorFiles(t, dir)
	version := false
	for _, f := range files {
		v, ok := p.entry("files", f)
		if !ok {
			t.Errorf("%s: PROVENANCE files has no entry", f)
			continue
		}
		if !strings.HasPrefix(v, "verbatim: ") && !strings.HasPrefix(v, "source-derived: ") {
			t.Errorf("%s: entry is neither verbatim nor source-derived: %q", f, v)
		}
		if !strings.Contains(v, blob) {
			t.Errorf("%s: no URL under %s", f, blob)
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		// The recorded digest pins the bytes copied or derived at fetch time.
		sum := sha256.Sum256(b)
		if m := reSum.FindStringSubmatch(v); m == nil || m[1] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: PROVENANCE digest %v does not match the file's %x", f, m, sum)
		}
		switch {
		case strings.HasSuffix(f, ".json"):
			if !reMan.MatchString(v) {
				t.Errorf("%s: no URL at %s pointing at man/man8/zpool-list.8, zpool-status.8, zpool-get.8 or zfs-list.8", f, commit)
			}
			if !hasOutputVersion(decodeVector(t, filepath.Join(dir, f))) {
				t.Errorf("%s: no output_version.command and output_version.vers_major", f)
			}
		case f == "zfs-version.txt":
			version = true
			lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			if len(lines) != 2 || !regexp.MustCompile(`^zfs-[0-9]+\.[0-9]+\.[0-9]+\S*$`).MatchString(lines[0]) ||
				!regexp.MustCompile(`^zfs-kmod-[0-9]+\.[0-9]+\.[0-9]+\S*$`).MatchString(lines[1]) {
				t.Errorf("zfs-version.txt %q: want the two lines zfs-<ver> and zfs-kmod-<ver>", lines)
			}
		default:
			t.Errorf("%s: unexpected upstream file", f)
		}
	}
	if !version {
		t.Error("no zfs version vector (zfs-version.txt)")
	}
	for _, want := range []string{"zpool-list.8", "zpool-status.8", "zfs-list.8"} {
		found := false
		for _, f := range files {
			v, _ := p.entry("files", f)
			if strings.HasPrefix(v, "verbatim: ") && strings.Contains(v, "man/man8/"+want+"#") {
				found = true
			}
		}
		if !found {
			t.Errorf("no verbatim vector from man/man8/%s", want)
		}
	}
}

// nameKeyed are the JSON objects whose keys are names (pools, vdevs,
// datasets), not key names: their members are compared as one shape.
var nameKeyed = map[string]bool{
	"pools": true, "vdevs": true, "dedup": true, "special": true, "logs": true,
	"l2cache": true, "spares": true, "datasets": true, "rebuild_stats": true,
}

// vectorShape adds every key path of v to set, with the members of a
// name-keyed object as `*` and array elements as `[]`.
func vectorShape(prefix string, v any, set map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		named := nameKeyed[prefix[strings.LastIndex(prefix, ".")+1:]]
		for k, c := range x {
			if named {
				k = "*"
			}
			path := prefix + "." + k
			set[path] = true
			vectorShape(path, c, set)
		}
	case []any:
		for _, c := range x {
			set[prefix+"[]"] = true
			vectorShape(prefix+"[]", c, set)
		}
	}
}

// vectorFlat maps every leaf path (with real names) of v to its value.
func vectorFlat(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			vectorFlat(prefix+"."+k, c, out)
		}
	case []any:
		for i, c := range x {
			vectorFlat(prefix+"["+strconv.Itoa(i)+"]", c, out)
		}
	default:
		out[prefix] = jsonText(x)
	}
}

func jsonText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// unset is a time or count the tools print as 0 (--json-int) or "-".
func unset(v any) bool {
	s := jsonText(v)
	return s == "" || s == "0" || s == "-"
}

func countNonZero(v any) bool { return !unset(v) }

// firstPool returns the vector's first pool by name, or nil.
func firstPool(m map[string]any) map[string]any {
	pools, _ := m["pools"].(map[string]any)
	names := make([]string, 0, len(pools))
	for n := range pools {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	p, _ := pools[names[0]].(map[string]any)
	return p
}

// scanOf returns the first pool's scan_stats, or nil.
func scanOf(m map[string]any) map[string]any {
	p := firstPool(m)
	if p == nil {
		return nil
	}
	s, _ := p["scan_stats"].(map[string]any)
	return s
}

// walkVdevs calls fn for every vdev object of a pool.
func walkVdevs(pool map[string]any, fn func(map[string]any)) {
	var walk func(any)
	walk = func(v any) {
		m, _ := v.(map[string]any)
		for _, c := range m {
			vd, ok := c.(map[string]any)
			if !ok {
				continue
			}
			fn(vd)
			walk(vd["vdevs"])
		}
	}
	for _, k := range []string{"vdevs", "dedup", "special", "logs", "l2cache", "spares"} {
		walk(pool[k])
	}
}

// TC-STG-5: synthetic vectors differ from their source vectors only in values. Owner WI 1.1. Requirements: NFR-4, NFR-8.
func TestTCSTG5_SyntheticVectorsDifferFromTheirSourceVectors(t *testing.T) {
	dir := filepath.Join(captureRoot, "synthetic")
	p := readCaptureProvenance(t, dir)
	knownHealth := map[string]bool{"ONLINE": true, "DEGRADED": true, "FAULTED": true, "OFFLINE": true, "REMOVED": true, "UNAVAIL": true, "SUSPENDED": true}
	type vec struct {
		name, src string
		doc       map[string]any
	}
	var vecs []vec
	for _, f := range vectorFiles(t, dir) {
		if !strings.HasSuffix(f, ".json") {
			t.Errorf("%s: a synthetic vector is JSON", f)
			continue
		}
		v, _ := p.entry("files", f)
		src, _ := strings.CutPrefix(v, "derived-from ")
		src, _, _ = strings.Cut(src, ";")
		syn := decodeVector(t, filepath.Join(dir, f))
		orig := decodeVector(t, filepath.Join(captureRoot, filepath.FromSlash(src)))
		have, base := map[string]bool{}, map[string]bool{}
		vectorShape("", syn, have)
		vectorShape("", orig, base)
		for k := range have {
			if !base[k] {
				t.Errorf("%s: key path %s is not in its source %s", f, k, src)
			}
		}
		vecs = append(vecs, vec{f, src, syn})
	}

	found := map[string]string{}
	mark := func(kind, f string) {
		if prev, ok := found[kind]; ok && prev != f {
			return
		}
		found[kind] = f
	}
	for _, v := range vecs {
		if ov, ok := v.doc["output_version"].(map[string]any); ok && jsonText(ov["vers_major"]) != "0" {
			mark("unknown output_version major", v.name)
		}
		pool := firstPool(v.doc)
		if pool == nil {
			continue
		}
		switch st := jsonText(pool["state"]); {
		case st == "SUSPENDED":
			mark("SUSPENDED pool", v.name)
		case !knownHealth[st]:
			mark("unknown health word", v.name)
		}
		walkVdevs(pool, func(vd map[string]any) {
			if jsonText(vd["vdev_type"]) != "mirror" || jsonText(vd["state"]) != "DEGRADED" {
				return
			}
			errs := false
			walkVdevs(map[string]any{"vdevs": map[string]any{"m": vd}}, func(x map[string]any) {
				for _, k := range []string{"read_errors", "write_errors", "checksum_errors"} {
					errs = errs || countNonZero(x[k])
				}
			})
			if errs {
				mark("DEGRADED mirror with error counts", v.name)
			}
		})
		if el, ok := pool["errlist"].([]any); ok && len(el) == 60 {
			mark("permanent errors with 60 paths", v.name)
		}
		s := scanOf(v.doc)
		if s == nil {
			continue
		}
		fn, state := jsonText(s["function"]), jsonText(s["state"])
		switch {
		case fn == "SCRUB" && state == "SCANNING" && unset(s["scrub_pause"]) && unset(s["end_time"]):
			mark("scrub SCANNING", v.name)
		case fn == "SCRUB" && state == "SCANNING" && !unset(s["scrub_pause"]) && unset(s["end_time"]):
			mark("scrub paused", v.name)
		case fn == "SCRUB" && state == "FINISHED" && countNonZero(s["processed"]) && unset(s["errors"]):
			mark("scrub FINISHED with repairs", v.name)
		case fn == "SCRUB" && state == "CANCELED" && !unset(s["end_time"]):
			mark("scrub CANCELED", v.name)
		case fn == "RESILVER" && state == "SCANNING":
			mark("resilver SCANNING", v.name)
		}
	}
	for _, kind := range []string{
		"DEGRADED mirror with error counts", "scrub SCANNING", "scrub FINISHED with repairs", "scrub CANCELED",
		"scrub paused", "resilver SCANNING", "SUSPENDED pool", "unknown health word",
		"permanent errors with 60 paths", "unknown output_version major",
	} {
		if _, ok := found[kind]; !ok {
			t.Errorf("no synthetic vector for: %s", kind)
		}
	}

	// The canceled and paused scrubs come from the finished scrub's source
	// and differ from the finished scrub only in state, times and the pause.
	byName := map[string]vec{}
	for _, v := range vecs {
		byName[v.name] = v
	}
	fin, ok := byName[found["scrub FINISHED with repairs"]]
	if !ok {
		return
	}
	allowed := map[string]bool{"state": true, "start_time": true, "end_time": true, "pass_start": true, "scrub_pause": true, "scrub_spent_paused": true}
	finFlat := map[string]string{}
	vectorFlat("", fin.doc, finFlat)
	for _, kind := range []string{"scrub CANCELED", "scrub paused"} {
		v, ok := byName[found[kind]]
		if !ok {
			continue
		}
		if v.src != fin.src {
			t.Errorf("%s derives from %s, the finished scrub %s from %s", v.name, v.src, fin.name, fin.src)
		}
		flat := map[string]string{}
		vectorFlat("", v.doc, flat)
		keys := map[string]bool{}
		for k := range flat {
			keys[k] = true
		}
		for k := range finFlat {
			keys[k] = true
		}
		for k := range keys {
			if flat[k] == finFlat[k] {
				continue
			}
			i := strings.LastIndex(k, ".")
			if !strings.HasSuffix(k[:i], ".scan_stats") || !allowed[k[i+1:]] {
				t.Errorf("%s differs from %s at %s (%q vs %q): only scan_stats state, times and the pause may differ", v.name, fin.name, k, flat[k], finFlat[k])
			}
		}
	}
}

var (
	reBundle      = regexp.MustCompile(`[^\s/"'<>]+\.(sparsebundle|backupbundle)`)
	reBundleOK    = regexp.MustCompile(`^mac-[0-9]+\.(sparsebundle|backupbundle)$`)
	reGuestName   = regexp.MustCompile(`(?:VM|CT) Name: (.*)$`)
	reGuestOK     = regexp.MustCompile(`^guest-[0-9]+$`)
	rePlistString = regexp.MustCompile(`<string>([^<]*)</string>`)
	reRedactedOK  = regexp.MustCompile(`^redacted-[0-9]+$`)
	reRedaction   = regexp.MustCompile(`^(mac|guest|plist-strings): [0-9]+(; (mac|guest|plist-strings): [0-9]+)*$`)
	// A share-root listing (shares/<n>/ls.txt) and the name column of its ls
	// -lan lines: a mode and the numeric columns, then a time (full-iso with a
	// zone, BusyBox -e with a year, or plain), then the name.
	reShareRootListing = regexp.MustCompile(`^shares/[0-9]+/ls\.txt$`)
	reLsEntry          = regexp.MustCompile(`^[-dlcbps][-rwxsStT]{9}[+.@]? +[0-9]+ +[0-9]+ +[0-9]+ +(?:[0-9]+, +)?[0-9]+ +(?:[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:.]+ [-+][0-9]{4}|[A-Z][a-z]{2} +[0-9]+ +(?:[0-9]{2}:[0-9]{2}(?::[0-9]{2})?(?: [0-9]{4})?|[0-9]{4})) (.+)$`)
	reShareEntryOK     = regexp.MustCompile(`^(\.|\.\.|dump|\.DS_Store|mac-[0-9]+\.(sparsebundle|backupbundle)|\.com\.apple\.timemachine\.supported-id-[0-9]+|vzdump-(qemu|lxc)-[0-9]+-[0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}\.[A-Za-z0-9._]+|vzdump-(qemu|lxc)-[0-9]+-[0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}\.tmp)$`)
	reMapLine          = regexp.MustCompile(`(?m)^(mac-[0-9]+\.(sparsebundle|backupbundle)|guest-[0-9]+|redacted-[0-9]+)\t.+`)
	reMinusOColumn     = regexp.MustCompile(`\s-o\s+(\S+)`)
)

// TC-STG-6: no committed capture holds sensitive data. Owner WI 1.1. Requirements: NFR-5.
func TestTCSTG6_NoCommittedCaptureHoldsSensitiveData(t *testing.T) {
	err := filepath.WalkDir("testdata", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "MachineID.plist" {
			t.Errorf("%s: a MachineID.plist is committed", p)
		}
		if strings.Contains(filepath.ToSlash(p), "/bands/") {
			t.Errorf("%s: a path inside a bundle's bands", p)
		}
		if strings.HasPrefix(d.Name(), "nexus-capture-map-") {
			t.Errorf("%s: a --map file is committed", p)
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if reMapLine.Match(b) {
			t.Errorf("%s: a line pairs a placeholder with another value (a --map file)", p)
		}
		if !strings.HasPrefix(filepath.ToSlash(p), captureRoot+"/") {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "/bands/") && !strings.HasPrefix(d.Name(), "du") {
				t.Errorf("%s:%d: a bands path outside a du output: %q", p, i+1, line)
			}
			if strings.Contains(line, "keylocation") {
				t.Errorf("%s:%d: keylocation is captured", p, i+1)
			}
		}
		if strings.HasSuffix(p, ".json") {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				ov, _ := m["output_version"].(map[string]any)
				if c := jsonText(ov["command"]); c == "zfs list" || c == "zfs get" {
					ds, _ := m["datasets"].(map[string]any)
					for name, v := range ds {
						props, _ := v.(map[string]any)["properties"].(map[string]any)
						for k := range props {
							if strings.Contains(k, ":") && k != "nexus:created-by" {
								t.Errorf("%s: dataset %s carries the user property %s", p, name, k)
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	hosts := 0
	for _, dir := range captureDirs(t) {
		prov := readCaptureProvenance(t, dir)
		red, ok := prov.keys["redaction"]
		if !ok {
			t.Errorf("%s/PROVENANCE: no redaction", dir)
		}
		for _, e := range prov.lists["commands"] {
			if !strings.Contains(e.value, "zfs list") && !strings.Contains(e.value, "zfs get") {
				continue
			}
			for _, m := range reMinusOColumn.FindAllStringSubmatch(e.value, -1) {
				for _, col := range strings.Split(m[1], ",") {
					if strings.Contains(col, ":") && col != "nexus:created-by" {
						t.Errorf("%s: %s asks for the user property %s", dir, e.name, col)
					}
				}
			}
		}
		if prov.keys["source"] != "host" {
			continue
		}
		hosts++
		if red == "none" || !reRedaction.MatchString(red) {
			t.Errorf("%s: redaction %q does not list the placeholder families with counts", dir, red)
		}
		for _, f := range vectorFiles(t, dir) {
			for _, part := range strings.Split(f, "/") {
				if strings.HasSuffix(part, ".sparsebundle") || strings.HasSuffix(part, ".backupbundle") {
					if !reBundleOK.MatchString(part) {
						t.Errorf("%s: file name part %q is not mac-<n>", dir, part)
					}
				}
			}
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range reBundle.FindAllString(string(b), -1) {
				if !reBundleOK.MatchString(m) {
					t.Errorf("%s/%s: bundle name %q is not mac-<n>", dir, f, m)
				}
			}
			lines := strings.Split(string(b), "\n")
			for _, line := range lines {
				if m := reGuestName.FindStringSubmatch(line); m != nil && !reGuestOK.MatchString(m[1]) {
					t.Errorf("%s/%s: guest name %q is not guest-<vmid>", dir, f, m[1])
				}
			}
			if strings.HasSuffix(f, ".notes") || strings.Contains(filepath.Base(f), "notes-line1") {
				if len(lines) > 1 && !reExitLine.MatchString(lines[0]) && !reGuestOK.MatchString(lines[0]) {
					t.Errorf("%s/%s: notes first line %q is not guest-<vmid>", dir, f, lines[0])
				}
			}
			if reShareRootListing.MatchString(f) {
				// CR-7: a share root lists more than bundles; a name outside the
				// known grammars is free-form text capture.sh cannot redact.
				for _, line := range lines {
					if m := reLsEntry.FindStringSubmatch(line); m != nil && !reShareEntryOK.MatchString(m[1]) {
						t.Errorf("%s/%s: share-root entry %q is not a known name (., .., dump, .DS_Store, a mac-<n> bundle, a marker id-<n>, a vzdump file)", dir, f, m[1])
					}
				}
			}
			if strings.Contains(f, "SnapshotHistory") && bytes.HasPrefix(b, []byte("<?xml")) {
				for _, m := range rePlistString.FindAllStringSubmatch(string(b), -1) {
					if !reRedactedOK.MatchString(m[1]) {
						t.Errorf("%s/%s: plist <string> %q is not redacted-<n>", dir, f, m[1])
					}
				}
			}
		}
	}
	if hosts == 0 {
		t.Log("no source: host capture is committed yet (WI 1.2): the host redaction checks pass vacuously")
	}
}
