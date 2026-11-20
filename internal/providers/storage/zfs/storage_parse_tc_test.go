// Tests of planning/storage-provider/test-cases.md TC-STG-60 to TC-STG-73
// (WI 3.2): the ZFS model, the JSON and text parsers and Argv.
//
// Vectors: testdata/captures/upstream-openzfs-2.4.1 and synthetic (WI 1.1),
// and the host captures (WI 1.2), gated with capturegate.Require; this
// internal package never imports storagetest, which imports zfs.
//
// Vectors derived in this file follow WI 1.1's rules for test-derived vectors
// (testdata/README.md): each helper below starts from a named committed
// vector and cites it, changes only values or adds or removes whole objects
// of a shape that vector already has, and adds no key name that OpenZFS 2.4.1
// does not print for the argv it models. No text form of an upstream or
// synthetic vector is committed, so each text twin is written in the format
// of the 2.4.1 print function it cites (cmd/zpool/zpool_main.c at commit
// 1c702dd: status_callback L10991, print_status_config L2873,
// print_scan_scrub_resilver_status L8937-L9029, print_error_log L10370), with
// its dates rendered and parsed in UTC (textLoc pinned by pinUTC). A twin is
// never the only evidence for a rule a host capture can show (A-43).

package zfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zones of host captures (A-60) and the non-UTC twin test
	"unicode/utf8"

	"github.com/mauktik/nexus/internal/providers/storage/capturegate"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

var (
	capRoot   = filepath.Join("..", "testdata", "captures")
	upstream  = filepath.Join(capRoot, "upstream-openzfs-2.4.1")
	synthetic = filepath.Join(capRoot, "synthetic")
)

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// captureStdout reads a capture file as the tool's stdout: a host capture's
// "Exit <code>" line is removed (testdata README) and must be 0.
func captureStdout(t testing.TB, path string) []byte {
	t.Helper()
	out, code := captureVersion(readFile(t, path))
	if code != 0 {
		t.Fatalf("%s: exit %d", path, code)
	}
	return out
}

// hostCaptures returns each host capture directory holding zfs/<file>,
// first gating on the home-server capture (WI 1.2).
func hostCaptures(t *testing.T, file string) []string {
	t.Helper()
	t.Run("home-server capture present", func(t *testing.T) {
		capturegate.Require(t, filepath.Join(capRoot, "home-server", "zfs", file))
	})
	entries, err := os.ReadDir(capRoot)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "upstream-openzfs-2.4.1" || e.Name() == "synthetic" {
			continue
		}
		d := filepath.Join(capRoot, e.Name())
		if _, err := os.Stat(filepath.Join(d, "zfs", file)); err == nil {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// ---- an order-keeping JSON tree, to derive vectors by changing values ----

type onode struct {
	keys []string
	vals map[string]any // *onode, []any, json.Number, string, bool or nil
}

func odecode(t testing.TB, b []byte) *onode {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := odecodeValue(dec)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.(*onode)
	if !ok {
		t.Fatal("not a JSON object")
	}
	return o
}

func odecodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch d := tok.(type) {
	case json.Delim:
		if d == '{' {
			o := &onode{vals: map[string]any{}}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := odecodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k.(string), v)
			}
			_, err := dec.Token()
			return o, err
		}
		var a []any
		for dec.More() {
			v, err := odecodeValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return tok, nil
}

func (o *onode) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *onode) at(t testing.TB, keys ...string) *onode {
	t.Helper()
	cur := o
	for _, k := range keys {
		n, ok := cur.vals[k].(*onode)
		if !ok {
			t.Fatalf("no object at %q", keys)
		}
		cur = n
	}
	return cur
}

func (o *onode) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// insertBefore adds k before the member named before, keeping tool order.
func (o *onode) insertBefore(before, k string, v any) {
	i := slices.Index(o.keys, before)
	if i < 0 {
		i = len(o.keys)
	}
	o.keys = slices.Insert(o.keys, i, k)
	o.vals[k] = v
}

func (o *onode) del(k string) {
	o.keys = slices.DeleteFunc(o.keys, func(x string) bool { return x == k })
	delete(o.vals, k)
}

func (o *onode) clone(t testing.TB) *onode { return odecode(t, oencode(t, o)) }

func oencode(t testing.TB, o *onode) []byte {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func num(n uint64) json.Number { return json.Number(strconv.FormatUint(n, 10)) }

func setCounts(o *onode, r, w, c uint64) {
	o.set("read_errors", num(r))
	o.set("write_errors", num(w))
	o.set("checksum_errors", num(c))
}

// leafVdev is a leaf vdev object of the source-derived vector's shape.
func leafVdev(name, state string, counters bool) *onode {
	o := &onode{vals: map[string]any{}}
	o.set("name", name)
	o.set("vdev_type", "disk")
	o.set("guid", num(1000+uint64(len(name))))
	o.set("path", "/dev/"+name)
	o.set("state", state)
	if counters {
		setCounts(o, 0, 0, 0)
		o.set("slow_ios", num(0))
	}
	return o
}

func group(members ...*onode) *onode {
	g := &onode{vals: map[string]any{}}
	for _, m := range members {
		g.set(m.vals["name"].(string), m)
	}
	return g
}

const (
	leafA = "ca1eb824-c371-491d-ac13-37637e35c683"
	leafB = "97cd98fb-8fb8-4ac4-bc84-bd8950a7ace7"
)

// mirrorWithClasses derives from synthetic/zpool-status-degraded-mirror.json
// (itself derived from upstream zpool-status-jv.source-derived.json):
// non-zero read, write and checksum counts on the root, on mirror-0 and on
// both leaves (TC-STG-63 (a)), and a logs, an l2cache and a spares group
// added whole in the shape of the vector's leaves (spares without counters,
// as vdev_stats_nvlist adds none for a spare). The JSON groups are the ones
// status_callback_json (zpool_main.c L10868) nests under the pool.
func mirrorWithClasses(t testing.TB) []byte {
	t.Helper()
	doc := odecode(t, readFile(t, filepath.Join(synthetic, "zpool-status-degraded-mirror.json")))
	pool := doc.at(t, "pools", "tank")
	root := pool.at(t, "vdevs", "tank")
	setCounts(root, 2, 0, 5)
	mirror := root.at(t, "vdevs", "mirror-0")
	setCounts(mirror, 3, 1, 24)
	setCounts(mirror.at(t, "vdevs", leafA), 1, 2, 6)
	pool.insertBefore("error_count", "logs", group(leafVdev("sdl", "ONLINE", true)))
	pool.insertBefore("error_count", "l2cache", group(leafVdev("sdm", "ONLINE", true)))
	pool.insertBefore("error_count", "spares", group(leafVdev("sdn", "AVAIL", false)))
	return oencode(t, doc)
}

// mirrorWithClassesText is `zpool status -p -v` of the same pool as
// mirrorWithClasses (source vector: synthetic/zpool-status-degraded-mirror.json),
// as the 2.4.1 source prints it: the header of status_callback (L10991), the
// scan line of print_scan_scrub_resilver_status (L8937-L9029), the config
// rows of print_status_config (L2873) with the logs, cache and spares groups
// of print_class_vdevs (L3319), print_l2cache (L10422) and print_spares
// (L10402), and the errors line of print_error_log (L10370). The scan line's
// date is in UTC (textLoc is pinned to UTC for the fixtures).
const mirrorWithClassesText = `  pool: tank
 state: DEGRADED
status: One or more devices has experienced an unrecoverable error.  An
	attempt was made to correct the error.  Applications are unaffected.
action: Determine if the device needs to be replaced, and clear the errors
	using 'zpool clear' or replace the device with 'zpool replace'.
   see: https://openzfs.github.io/openzfs-docs/msg/ZFS-8000-9P
  scan: scrub repaired 0B in 03:00:17 with 0 errors on Sun Nov 15 05:00:17 2026
config:

	NAME                                      STATE     READ WRITE CKSUM
	tank                                      DEGRADED     2     0     5
	  mirror-0                                DEGRADED     3     1    24
	    ca1eb824-c371-491d-ac13-37637e35c683  ONLINE       1     2     6
	    97cd98fb-8fb8-4ac4-bc84-bd8950a7ace7  DEGRADED     3     1    24  too many errors
	logs
	  sdl                                     ONLINE       0     0     0
	cache
	  sdm                                     ONLINE       0     0     0
	spares
	  sdn                                     AVAIL

errors: No known data errors
`

func pinUTC(t *testing.T) {
	t.Helper()
	old := textLoc
	textLoc = time.UTC
	t.Cleanup(func() { textLoc = old })
}

// statusTextFor renders the text form of a single-pool status vector's
// header, scan and errors sections around the given config rows, so the scan
// and texts of the synthetic vectors have a text twin. Source vector: the
// doc passed, always one of the synthetic/zpool-status-*.json vectors or a
// value-only edit of one. It follows status_callback (zpool_main.c L10991)
// for the header, status and action lines, print_scan_scrub_resilver_status
// (L8937-L9029) through scanTextFor, and print_error_log (L10370) for the
// errors section.
func statusTextFor(t testing.TB, doc *onode, configRows string) string {
	t.Helper()
	pools := doc.at(t, "pools")
	if len(pools.keys) != 1 {
		t.Fatal("statusTextFor takes one pool")
	}
	p := pools.at(t, pools.keys[0])
	var b strings.Builder
	fmt.Fprintf(&b, "  pool: %s\n state: %s\n", p.vals["name"], p.vals["state"])
	if s, ok := p.vals["status"].(string); ok {
		b.WriteString("status: " + s)
	}
	if s, ok := p.vals["action"].(string); ok {
		b.WriteString("action: " + s)
	}
	if s, ok := p.vals["msgid"].(string); ok {
		b.WriteString("   see: https://openzfs.github.io/openzfs-docs/msg/" + s + "\n")
	}
	if _, ok := p.vals["scan_stats"]; ok {
		b.WriteString(scanTextFor(t, p.at(t, "scan_stats")))
	}
	b.WriteString("config:\n\n\tNAME        STATE     READ WRITE CKSUM\n" + configRows + "\n")
	if n, ok := p.vals["error_count"].(json.Number); ok {
		if paths, ok := p.vals["errlist"].([]any); ok && n.String() != "0" {
			b.WriteString("errors: Permanent errors have been detected in the following files:\n\n")
			for _, x := range paths {
				fmt.Fprintf(&b, "%7s %s\n", "", x)
			}
		} else if n.String() == "0" {
			b.WriteString("errors: No known data errors\n")
		} else {
			fmt.Fprintf(&b, "errors: %s data errors, use '-v' for a list\n", n)
		}
	}
	return b.String()
}

func ounum(t testing.TB, o *onode, k string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(string(o.vals[k].(json.Number)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// twinZone is the zone scanTextFor prints ctime(3) dates in: UTC for the
// derived twins (textLoc is pinned to UTC), and one other zone in the test of
// A-60's zone mechanism.
var twinZone = time.UTC

// scanTextFor prints a scan_stats object as print_scan_scrub_resilver_status
// does (zpool_main.c L8937-L9029), with ctime(3) in twinZone. Source vector:
// the scan_stats object of the synthetic/zpool-status-scrub-*.json and
// zpool-status-resilver-scanning.json vector passed (all derived from
// upstream zpool-status-jv.source-derived.json); the sizes are printed by
// niceBytesFor, rounded as zfs_nicebytes does even with -p.
func scanTextFor(t testing.TB, s *onode) string {
	t.Helper()
	fn, st := s.vals["function"].(string), s.vals["state"].(string)
	if fn != "SCRUB" && fn != "RESILVER" {
		return "  scan: none requested\n"
	}
	ctime := func(sec uint64) string { return time.Unix(int64(sec), 0).In(twinZone).Format(time.ANSIC) }
	start, end, pause := ounum(t, s, "start_time"), ounum(t, s, "end_time"), ounum(t, s, "scrub_pause")
	processed := niceBytesFor(ounum(t, s, "processed"))
	scrub := fn == "SCRUB"
	switch st {
	case "FINISHED":
		d := end - start
		dur := fmt.Sprintf("%02d:%02d:%02d", d/3600%24, d/60%60, d%60)
		if d/86400 > 0 {
			dur = fmt.Sprintf("%d days %s", d/86400, dur)
		}
		verb := "resilvered"
		if scrub {
			verb = "scrub repaired"
		}
		return fmt.Sprintf("  scan: %s %s in %s with %d errors on %s\n", verb, processed, dur, ounum(t, s, "errors"), ctime(end))
	case "CANCELED":
		return fmt.Sprintf("  scan: %s canceled on %s\n", strings.ToLower(fn), ctime(end))
	}
	var b strings.Builder
	switch {
	case scrub && pause == 0:
		fmt.Fprintf(&b, "  scan: scrub in progress since %s\n", ctime(start))
	case scrub:
		fmt.Fprintf(&b, "  scan: scrub paused since %s\n\tscrub started on %s\n", ctime(pause), ctime(start))
	default:
		fmt.Fprintf(&b, "  scan: resilver in progress since %s\n", ctime(start))
	}
	toExamine := ounum(t, s, "to_examine")
	issued, totalI := ounum(t, s, "issued"), toExamine-ounum(t, s, "skipped")
	fmt.Fprintf(&b, "\t%s / %s scanned, %s / %s issued\n", niceBytesFor(ounum(t, s, "examined")), niceBytesFor(toExamine), niceBytesFor(issued), niceBytesFor(totalI))
	verb := "resilvered"
	if scrub {
		verb = "repaired"
	}
	fmt.Fprintf(&b, "\t%s %s, %.2f%% done, no estimated completion time\n", processed, verb, 100*float64(issued)/float64(totalI))
	return b.String()
}

// niceBytesFor is zfs_nicenum_format(…, ZFS_NICENUM_BYTES)
// (lib/libzutil/zutil_nicenum.c L62-L140), which zfs_nicebytes (L182) calls
// for every scan size print_scan_scrub_resilver_status prints, even with -p.
func niceBytesFor(num uint64) string {
	units := "BKMGTPE"
	n, index := num, 0
	for n >= 1024 && index < 6 {
		n /= 1024
		index++
	}
	u := string(units[index])
	div := math.Pow(1024, float64(index))
	if index == 0 || num%uint64(div) == 0 {
		return fmt.Sprintf("%d%s", n, u)
	}
	var s string
	for i := 2; i >= 0; i-- {
		s = fmt.Sprintf("%.*f%s", i, float64(num)/div, u)
		if len(s) <= 5 {
			break
		}
	}
	return s
}

// ---- generic oracles over the JSON vectors ----

// gvdev is one vdev of a generic (map[string]any) decode with its depth.
type gvdev struct {
	name     string
	depth    int
	children int
	counts   [3]uint64
	has      [3]bool
}

func anyUint(v any) (uint64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := strconv.ParseUint(string(x), 10, 64)
		return n, err == nil
	case string:
		n, err := strconv.ParseUint(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// genericVdevs decodes a pool's vdev tree with encoding/json into maps; a
// leaf is a vdev without children, which in tree order is the entry whose
// next entry is not deeper (A-52).
func genericVdevs(pool map[string]any) []gvdev {
	var out []gvdev
	var walk func(v map[string]any, depth int)
	walk = func(v map[string]any, depth int) {
		g := gvdev{name: fmt.Sprint(v["name"]), depth: depth}
		for i, k := range []string{"read_errors", "write_errors", "checksum_errors"} {
			g.counts[i], g.has[i] = anyUint(v[k])
		}
		kids, _ := v["vdevs"].(map[string]any)
		g.children = len(kids)
		out = append(out, g)
		for _, c := range kids {
			walk(c.(map[string]any), depth+1)
		}
	}
	if root, ok := pool["vdevs"].(map[string]any); ok {
		for _, r := range root {
			walk(r.(map[string]any), 0)
		}
	}
	for _, class := range []string{"dedup", "special", "logs", "l2cache", "spares"} {
		if g, ok := pool[class].(map[string]any); ok {
			for _, c := range g {
				walk(c.(map[string]any), 1)
			}
		}
	}
	return out
}

// a52 is A-52's rule over generic vdevs.
func a52(vs []gvdev) (ErrorCounts, ErrorCounts) {
	var leaves, all [3]uint64
	var root [3]uint64
	for _, v := range vs {
		for i := range 3 {
			all[i] += v.counts[i]
			if v.children == 0 {
				leaves[i] += v.counts[i]
			}
			if v.depth == 0 {
				root[i] = v.counts[i]
			}
		}
	}
	for i := range 3 {
		if leaves[i] == 0 {
			leaves[i] = root[i]
		}
	}
	return ErrorCounts{leaves[0], leaves[1], leaves[2]}, ErrorCounts{all[0], all[1], all[2]}
}

func genericPools(t testing.TB, b []byte) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Pools map[string]map[string]any `json:"pools"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.Pools
}

func findPool(t testing.TB, pools []Pool, name string) Pool {
	t.Helper()
	for _, p := range pools {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("pool %q not parsed", name)
	return Pool{}
}

func eqPtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func show[T any](p *T) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

// tabOracle splits -H -p text into rows keyed by their first column.
func tabOracle(t testing.TB, b []byte, columns string) map[string]map[string]string {
	t.Helper()
	cols := strings.Split(columns, ",")
	out := map[string]map[string]string{}
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != len(cols) {
			t.Fatalf("oracle row %q: %d fields, want %d", l, len(f), len(cols))
		}
		row := map[string]string{}
		for i, c := range cols {
			row[c] = f[i]
		}
		out[f[0]] = row
	}
	return out
}

func oracleUint(w string) *uint64 {
	n, err := strconv.ParseUint(w, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func oracleInt(w string) *int {
	n, err := strconv.Atoi(strings.TrimSuffix(w, "%"))
	if err != nil {
		return nil
	}
	return &n
}

func oracleFloat(w string) *float64 {
	f, err := strconv.ParseFloat(strings.TrimSuffix(w, "x"), 64)
	if err != nil {
		return nil
	}
	return &f
}

func checkPoolListRow(t *testing.T, p Pool, row map[string]string) {
	t.Helper()
	var altroot *string
	if a, ok := row["altroot"]; ok && a != "-" {
		altroot = &a
	}
	var ro *bool
	switch row["readonly"] {
	case "on":
		ro = ptr(true)
	case "off":
		ro = ptr(false)
	}
	checks := []struct {
		name string
		ok   bool
		got  string
		want string
	}{
		{"Size", eqPtr(p.Size, oracleUint(row["size"])), show(p.Size), row["size"]},
		{"Alloc", eqPtr(p.Alloc, oracleUint(row["allocated"])), show(p.Alloc), row["allocated"]},
		{"Free", eqPtr(p.Free, oracleUint(row["free"])), show(p.Free), row["free"]},
		{"Capacity", eqPtr(p.Capacity, oracleInt(row["capacity"])), show(p.Capacity), row["capacity"]},
		{"Frag", eqPtr(p.Frag, oracleInt(row["fragmentation"])), show(p.Frag), row["fragmentation"]},
		{"Dedup", eqPtr(p.Dedup, oracleFloat(row["dedupratio"])), show(p.Dedup), row["dedupratio"]},
		{"RawHealth", p.RawHealth == row["health"], p.RawHealth, row["health"]},
		{"Altroot", eqPtr(p.Altroot, altroot), show(p.Altroot), row["altroot"]},
		{"ReadOnly", eqPtr(p.ReadOnly, ro), show(p.ReadOnly), row["readonly"]},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("pool %s %s = %s; oracle %q", p.Name, c.name, c.got, c.want)
		}
	}
}

func TestTCSTG60_ParsePoolListJSONAgreesWithTheSameInstantText(t *testing.T) {
	t.Run("upstream zpool-list.8 example (generic oracle)", func(t *testing.T) {
		b := readFile(t, filepath.Join(upstream, "zpool-list.8.example3.json"))
		pools, err := ParsePoolListJSON(b)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Pools map[string]struct {
				Properties map[string]struct{ Value any } `json:"properties"`
			} `json:"pools"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if len(pools) != len(doc.Pools) {
			t.Fatalf("parsed %d pools; oracle %d", len(pools), len(doc.Pools))
		}
		for name, op := range doc.Pools {
			row := map[string]string{}
			for k, v := range op.Properties {
				row[k] = fmt.Sprint(v.Value)
			}
			// The man-page example is the string form: "111G" is no exact
			// byte count, so the oracle and the parser both leave it nil;
			// "27%" and "1.00x" carry their exact values.
			p := findPool(t, pools, name)
			checkPoolListRow(t, p, row)
			// The example was taken without -o, and zpool list -j prints
			// readonly only when asked (A-58; default_props of zpool_do_list,
			// cmd/zpool/zpool_main.c L7395-L7397, has no readonly), so the
			// pool has none: nil, with a reason that names zpool list.
			if _, has := op.Properties["readonly"]; has {
				t.Fatalf("pool %s: the upstream example has a readonly property", name)
			}
			if p.ReadOnly != nil || !strings.Contains(p.Unavailable["readOnly"], "zpool list") {
				t.Errorf("pool %s: ReadOnly %s, Unavailable[readOnly] %q; want nil and a reason naming zpool list", name, show(p.ReadOnly), p.Unavailable["readOnly"])
			}
		}
	})
	for _, dir := range hostCaptures(t, "zpool-list.json") {
		t.Run(dir, func(t *testing.T) {
			pools, err := ParsePoolListJSON(captureStdout(t, filepath.Join(dir, "zfs", "zpool-list.json")))
			if err != nil {
				t.Fatal(err)
			}
			oracle := tabOracle(t, captureStdout(t, filepath.Join(dir, "zfs", "zpool-list.txt")), PoolListColumns)
			if len(pools) != len(oracle) {
				t.Fatalf("parsed %d pools; text capture has %d", len(pools), len(oracle))
			}
			for _, p := range pools {
				row, ok := oracle[p.Name]
				if !ok {
					t.Fatalf("pool %q not in the text capture", p.Name)
				}
				checkPoolListRow(t, p, row)
			}
		})
	}
}

// healthStatusText is a minimal zpool status text of one pool in the given
// state word, for the health table: the text twin of
// synthetic/zpool-status-unknown-health.json with its pool state word
// changed, in the header and config rows of status_callback (L10991) and
// print_status_config (L2873).
func healthStatusText(word string) string {
	return "  pool: tank\n state: " + word + "\nconfig:\n\n\tNAME  STATE     READ WRITE CKSUM\n\ttank  ONLINE       0     0     0\n\nerrors: No known data errors\n"
}

func TestTCSTG61_HealthWordsNormalizeToTheEightStates(t *testing.T) {
	src := readFile(t, filepath.Join(synthetic, "zpool-status-unknown-health.json"))
	want := map[string]string{
		"ONLINE": "online", "DEGRADED": "degraded", "FAULTED": "faulted", "OFFLINE": "offline",
		"UNAVAIL": "unavail", "REMOVED": "removed", "SUSPENDED": "suspended",
		"FROBNICATED": "unknown", "": "unknown", "NEXUS_SYNTHETIC_STATE": "unknown",
		"online": "unknown", "Online": "unknown", "ONLINE ": "unknown",
	}
	for word, health := range want {
		t.Run(fmt.Sprintf("%q", word), func(t *testing.T) {
			// Derived from synthetic/zpool-status-unknown-health.json: the
			// pool's state word changed.
			doc := odecode(t, src)
			doc.at(t, "pools", "tank").set("state", word)
			pools, err := ParseStatusJSON(oencode(t, doc))
			if err != nil {
				t.Fatal(err)
			}
			if p := pools[0]; p.Health != health || p.RawHealth != word {
				t.Errorf("JSON: Health, RawHealth = %q, %q; want %q, %q", p.Health, p.RawHealth, health, word)
			}
			if strings.TrimSpace(word) == word {
				pools, err = ParseStatusText([]byte(healthStatusText(word)))
				if err != nil {
					t.Fatal(err)
				}
				if p := pools[0]; p.Health != health || p.RawHealth != word {
					t.Errorf("text: Health, RawHealth = %q, %q; want %q, %q", p.Health, p.RawHealth, health, word)
				}
			}
			list := fmt.Sprintf("tank\t100\t10\t90\t10\t0\t1.00\t%s\t-\toff\n", word)
			lp, err := ParsePoolListText([]byte(list))
			if err != nil {
				t.Fatal(err)
			}
			if lp[0].Health != health || lp[0].RawHealth != word {
				t.Errorf("list text: Health, RawHealth = %q, %q; want %q, %q", lp[0].Health, lp[0].RawHealth, health, word)
			}
		})
	}
}

type vdevWant struct {
	name, typ, parent string
	depth             int
}

func checkVdevs(t *testing.T, form string, got []Vdev, want []vdevWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d vdevs, want %d: %+v", form, len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.name || g.Type != w.typ || g.Parent != w.parent || g.Depth != w.depth {
			t.Errorf("%s vdev %d = {%s %s parent %q depth %d}; want {%s %s parent %q depth %d}",
				form, i, g.Name, g.Type, g.Parent, g.Depth, w.name, w.typ, w.parent, w.depth)
		}
	}
}

func TestTCSTG62_TheVdevTreeBecomesAFlatList(t *testing.T) {
	pinUTC(t)
	t.Run("upstream source-derived vector", func(t *testing.T) {
		pools, err := ParseStatusJSON(readFile(t, filepath.Join(upstream, "zpool-status-jv.source-derived.json")))
		if err != nil {
			t.Fatal(err)
		}
		checkVdevs(t, "JSON", pools[0].Vdevs, []vdevWant{
			{"tank", "root", "", 0},
			{"raidz1-0", "raidz1", "tank", 1},
			{"ca1eb824-c371-491d-ac13-37637e35c683", "disk", "raidz1-0", 2},
			{"97cd98fb-8fb8-4ac4-bc84-bd8950a7ace7", "disk", "raidz1-0", 2},
			{"e9ddba5f-f948-4734-a472-cb8aa5f0ff65", "disk", "raidz1-0", 2},
			{"mirror-2", "mirror", "tank", 1},
			{"db017360-d8e9-4163-961b-144ca75293a3", "disk", "mirror-2", 2},
			{"952c3baf-b08a-4a8c-b7fa-33a07af5fe6f", "disk", "mirror-2", 2},
			{"25d418f8-92bd-4327-b59f-7ef5d5f50d81", "special", "tank", 1},
		})
		if got := pools[0].VdevCount; got == nil || *got != 3 {
			t.Errorf("VdevCount = %s; want 3", show(got))
		}
	})
	mirror := []vdevWant{
		{"tank", "root", "", 0},
		{"mirror-0", "mirror", "tank", 1},
		{leafA, "disk", "mirror-0", 2},
		{leafB, "disk", "mirror-0", 2},
		{"sdl", "log", "tank", 1},
		{"sdm", "cache", "tank", 1},
		{"sdn", "spare", "tank", 1},
	}
	t.Run("synthetic degraded mirror with logs, cache and spares", func(t *testing.T) {
		jp, err := ParseStatusJSON(mirrorWithClasses(t))
		if err != nil {
			t.Fatal(err)
		}
		tp, err := ParseStatusText([]byte(mirrorWithClassesText))
		if err != nil {
			t.Fatal(err)
		}
		checkVdevs(t, "JSON", jp[0].Vdevs, mirror)
		checkVdevs(t, "text", tp[0].Vdevs, mirror)
		for _, p := range [][]Pool{jp, tp} {
			if p[0].VdevCount == nil || *p[0].VdevCount != 4 {
				t.Errorf("VdevCount = %s; want 4 (mirror-0, log, cache, spare)", show(p[0].VdevCount))
			}
		}
		if s := jp[0].Vdevs[3].State; s != "degraded" {
			t.Errorf("leaf B state = %q; want degraded", s)
		}
	})
	t.Run("an unrecognized vdev_type maps to unknown", func(t *testing.T) {
		// Derived: one leaf's vdev_type changed to a word 2.4.1 never prints.
		doc := odecode(t, mirrorWithClasses(t))
		doc.at(t, "pools", "tank", "vdevs", "tank", "vdevs", "mirror-0", "vdevs", leafA).set("vdev_type", "frobnicator")
		doc.at(t, "pools", "tank", "vdevs", "tank", "vdevs", "mirror-0").set("vdev_type", "replacing")
		pools, err := ParseStatusJSON(oencode(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		if v := pools[0].Vdevs[2]; v.Type != "unknown" {
			t.Errorf("leaf type = %q; want unknown", v.Type)
		}
		if v := pools[0].Vdevs[1]; v.Type != "unknown" {
			t.Errorf("replacing type = %q; want unknown", v.Type)
		}
	})
	for _, dir := range hostCaptures(t, "zpool-status-v.json") {
		t.Run(dir, func(t *testing.T) {
			jp, err := ParseStatusJSON(captureStdout(t, filepath.Join(dir, "zfs", "zpool-status-v.json")))
			if err != nil {
				t.Fatal(err)
			}
			tp, err := ParseStatusText(captureStdout(t, filepath.Join(dir, "zfs", "zpool-status-v.txt")))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range jp {
				if len(p.Vdevs) == 0 || p.Vdevs[0].Name != p.Name || p.Vdevs[0].Depth != 0 || p.Vdevs[0].Parent != "" {
					t.Errorf("pool %s: entry 0 is not the root: %+v", p.Name, p.Vdevs)
				}
				tpool := findPool(t, tp, p.Name)
				var jn, tn []string
				for _, v := range p.Vdevs {
					jn = append(jn, v.Name)
				}
				for _, v := range tpool.Vdevs {
					tn = append(tn, v.Name)
				}
				if !slices.Equal(jn, tn) {
					t.Errorf("pool %s: JSON order %q; text order %q", p.Name, jn, tn)
				}
				d1 := 0
				for _, v := range p.Vdevs {
					if v.Depth == 1 {
						d1++
					}
				}
				if p.VdevCount == nil || *p.VdevCount != d1 {
					t.Errorf("pool %s: VdevCount = %s; depth-1 entries %d", p.Name, show(p.VdevCount), d1)
				}
				if filepath.Base(dir) == "home-server" && d1 != 1 {
					t.Errorf("home-server pool %s has %d depth-1 vdevs; the live fact is one disk (TA-3)", p.Name, d1)
				}
			}
		})
	}
}

// textCounts reads the per-row counts of a zpool status -p text with
// strings.Fields, the TC's text oracle; a row without counts is nil.
func textCounts(text string) []gvdev {
	var out []gvdev
	in := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "\tNAME") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(l) == "" {
			break
		}
		body := strings.TrimPrefix(l, "\t")
		f := strings.Fields(body)
		if len(f) == 1 {
			continue
		}
		g := gvdev{name: f[0], depth: (len(body) - len(strings.TrimLeft(body, " "))) / 2}
		if len(f) >= 5 {
			for i := range 3 {
				g.counts[i], g.has[i] = anyUint(f[2+i])
			}
		}
		out = append(out, g)
	}
	for i := range out {
		if i+1 < len(out) && out[i+1].depth > out[i].depth {
			out[i].children = 1
		}
	}
	return out
}

func checkOwnCounts(t *testing.T, form string, got []Vdev, want []gvdev) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d vdevs; oracle %d", form, len(got), len(want))
	}
	byName := map[string]gvdev{}
	for _, w := range want {
		byName[w.name] = w
	}
	for _, v := range got {
		w := byName[v.Name]
		for i, c := range []*uint64{v.Read, v.Write, v.Checksum} {
			if (c != nil) != w.has[i] || c != nil && *c != w.counts[i] {
				t.Errorf("%s vdev %s counter %d = %s; oracle %d (present %v)", form, v.Name, i, show(c), w.counts[i], w.has[i])
			}
		}
	}
}

func TestTCSTG63_PoolErrorCountsAreTheLeafVdev(t *testing.T) {
	pinUTC(t)
	check := func(t *testing.T, b []byte, text string) ErrorCounts {
		t.Helper()
		pools, err := ParseStatusJSON(b)
		if err != nil {
			t.Fatal(err)
		}
		g := genericPools(t, b)
		var last ErrorCounts
		for _, p := range pools {
			vs := genericVdevs(g[p.Name])
			checkOwnCounts(t, "JSON", p.Vdevs, vs)
			want, _ := a52(vs)
			if p.Errors == nil || *p.Errors != want {
				t.Errorf("pool %s JSON Errors = %+v; A-52 oracle %+v", p.Name, p.Errors, want)
			}
			last = want
		}
		if text != "" {
			tp, err := ParseStatusText([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			tvs := textCounts(text)
			checkOwnCounts(t, "text", tp[0].Vdevs, tvs)
			twant, _ := a52(tvs)
			if tp[0].Errors == nil || *tp[0].Errors != twant || twant != last {
				t.Errorf("text Errors = %+v; text oracle %+v; JSON %+v", tp[0].Errors, twant, last)
			}
		}
		return last
	}

	t.Run("(a) degraded mirror: counted once at the leaves", func(t *testing.T) {
		b := mirrorWithClasses(t)
		got := check(t, b, mirrorWithClassesText)
		if want := (ErrorCounts{4, 3, 30}); got != want {
			t.Errorf("Errors = %+v; want the two leaves' sums %+v", got, want)
		}
		_, all := a52(genericVdevs(genericPools(t, b)["tank"]))
		if got == all {
			t.Errorf("Errors %+v equal the all-levels sum %+v", got, all)
		}
	})

	t.Run("(b) errors on the root vdev only", func(t *testing.T) {
		// Derived from (a): every non-root count set to 0.
		doc := odecode(t, mirrorWithClasses(t))
		root := doc.at(t, "pools", "tank", "vdevs", "tank")
		mirror := root.at(t, "vdevs", "mirror-0")
		setCounts(mirror, 0, 0, 0)
		setCounts(mirror.at(t, "vdevs", leafA), 0, 0, 0)
		setCounts(mirror.at(t, "vdevs", leafB), 0, 0, 0)
		text := mirrorWithClassesText
		for _, r := range [][2]string{
			{"DEGRADED     3     1    24\n", "DEGRADED     0     0     0\n"},
			{"ONLINE       1     2     6", "ONLINE       0     0     0"},
			{"DEGRADED     3     1    24  too", "DEGRADED     0     0     0  too"},
		} {
			if strings.Count(text, r[0]) != 1 {
				t.Fatalf("fixture edit %q is not unique", r[0])
			}
			text = strings.Replace(text, r[0], r[1], 1)
		}
		got := check(t, oencode(t, doc), text)
		if want := (ErrorCounts{2, 0, 5}); got != want {
			t.Errorf("Errors = %+v; want the root's own %+v", got, want)
		}
	})

	t.Run("(c) every other status vector", func(t *testing.T) {
		files, _ := filepath.Glob(filepath.Join(synthetic, "zpool-status-*.json"))
		files = append(files, filepath.Join(upstream, "zpool-status-jv.source-derived.json"), filepath.Join(upstream, "zpool-status.8.example2.json"))
		for _, f := range files {
			t.Run(filepath.Base(f), func(t *testing.T) { check(t, readFile(t, f), "") })
		}
		for _, dir := range hostCaptures(t, "zpool-status-v.json") {
			t.Run(dir, func(t *testing.T) {
				b := captureStdout(t, filepath.Join(dir, "zfs", "zpool-status-v.json"))
				got := check(t, b, "")
				text := string(captureStdout(t, filepath.Join(dir, "zfs", "zpool-status-v.txt")))
				jp, _ := ParseStatusJSON(b)
				tp, err := ParseStatusText([]byte(text))
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range jp {
					if tq := findPool(t, tp, p.Name); !reflect.DeepEqual(p.Errors, tq.Errors) {
						t.Errorf("pool %s: JSON Errors %+v; text %+v", p.Name, p.Errors, tq.Errors)
					}
				}
				if filepath.Base(dir) == "home-server" && got != (ErrorCounts{}) {
					t.Errorf("home-server Errors = %+v; want 0, 0, 0", got)
				}
			})
		}
	})
}

// scanVector loads a synthetic status vector (the file named, a scan state
// derived from upstream zpool-status-jv.source-derived.json) with edit applied
// to its scan_stats, and the text twin of that document (statusTextFor).
func scanVector(t *testing.T, file string, edit func(s *onode)) ([]byte, string) {
	t.Helper()
	doc := odecode(t, readFile(t, filepath.Join(synthetic, file)))
	if edit != nil {
		edit(doc.at(t, "pools", "tank", "scan_stats"))
	}
	return oencode(t, doc), statusTextFor(t, doc, "\ttank        ONLINE       0     0     0\n")
}

func TestTCSTG64_ScanStatsMapToTheFR6(t *testing.T) {
	pinUTC(t)
	cases := []struct {
		file      string
		edit      func(s *onode)
		fn, state string
	}{
		{"zpool-status-scrub-scanning.json", nil, "scrub", "scanning"},
		{"zpool-status-scrub-finished-repairs.json", nil, "scrub", "finished"},
		{"zpool-status-scrub-canceled.json", nil, "scrub", "canceled"},
		{"zpool-status-scrub-paused.json", nil, "scrub", "paused"},
		{"zpool-status-resilver-scanning.json", nil, "resilver", "scanning"},
		// Derived: a pool never scrubbed (function and state NONE).
		{"zpool-status-scrub-scanning.json", func(s *onode) { s.set("function", "NONE"); s.set("state", "NONE") }, "none", "none"},
		// Derived: function ERRORSCRUB, a state word 2.4.1 prints for an
		// error scrub (pool_scan_state_str ERRORSCRUBBING), and an unknown
		// function word.
		{"zpool-status-scrub-scanning.json", func(s *onode) { s.set("function", "ERRORSCRUB"); s.set("state", "ERRORSCRUBBING") }, "errorscrub", "unknown"},
		{"zpool-status-scrub-scanning.json", func(s *onode) { s.set("function", "FROBNICATE") }, "unknown", "scanning"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s %s %s", c.file, c.fn, c.state), func(t *testing.T) {
			b, text := scanVector(t, c.file, c.edit)
			pools, err := ParseStatusJSON(b)
			if err != nil {
				t.Fatal(err)
			}
			s := pools[0].Scan
			if s.Function != c.fn || s.State != c.state {
				t.Fatalf("Function, State = %q, %q; want %q, %q", s.Function, s.State, c.fn, c.state)
			}
			raw := genericPools(t, b)["tank"]["scan_stats"].(map[string]any)
			u := func(k string) uint64 { n, _ := anyUint(raw[k]); return n }
			tm := func(k string) *time.Time {
				if u(k) == 0 {
					return nil
				}
				return ptr(time.Unix(int64(u(k)), 0).UTC())
			}
			if !eqPtr(s.Start, tm("start_time")) || !eqPtr(s.End, tm("end_time")) {
				t.Errorf("Start, End = %s, %s; want %s, %s", show(s.Start), show(s.End), show(tm("start_time")), show(tm("end_time")))
			}
			for name, got := range map[string]*uint64{"examined": s.Examined, "to_examine": s.ToExamine, "processed": s.Processed, "errors": s.Errors} {
				if got == nil || *got != u(name) {
					t.Errorf("%s = %s; want %d", name, show(got), u(name))
				}
			}
			if c.fn != "scrub" && c.fn != "resilver" {
				return
			}
			if s.ToExamine != nil && *s.ToExamine > 0 {
				pct := float64(*s.Examined) / float64(*s.ToExamine) * 100
				if want := float64(u("examined")) / float64(u("to_examine")) * 100; pct != want {
					t.Errorf("percentDone basis %v; want %v", pct, want)
				}
			}
			tp, err := ParseStatusText([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			ts := tp[0].Scan
			if ts.Function != s.Function || ts.State != s.State {
				t.Errorf("text Function, State = %q, %q; JSON %q, %q", ts.Function, ts.State, s.Function, s.State)
			}
			if c.state == "finished" {
				// A-5: the text's "scrub repaired <processed> in … with
				// <errors> errors" line carries bytesRepaired and
				// errorsRepaired.
				if !eqPtr(ts.Processed, s.Processed) || !eqPtr(ts.Errors, s.Errors) {
					t.Errorf("text Processed, Errors = %s, %s; JSON %s, %s", show(ts.Processed), show(ts.Errors), show(s.Processed), show(s.Errors))
				}
				if !eqPtr(ts.Start, s.Start) || !eqPtr(ts.End, s.End) {
					t.Errorf("text Start, End = %s, %s; JSON %s, %s", show(ts.Start), show(ts.End), show(s.Start), show(s.End))
				}
			}
			// In the text form the values follow A-59, exactly as TC-STG-71's
			// list allows: sizes rounded, and nil what the state does not print.
			j := *s
			normalizeScan(&j, ts, false)
			if !reflect.DeepEqual(&j, ts) {
				t.Errorf("text scan differs from JSON beyond A-59's list:\nJSON %s\ntext %s", dump(&j), dump(ts))
			}
		})
	}
	t.Run("home-server-scrub (WI 6.2)", func(t *testing.T) {
		f := filepath.Join(capRoot, "home-server-scrub", "zfs", "zpool-status-v.json")
		if _, err := os.Stat(f); err != nil {
			t.Skipf("WI 6.2 capture absent: %s", f)
		}
		pools, err := ParseStatusJSON(captureStdout(t, f))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range pools {
			found = found || p.Scan.Function == "scrub" && p.Scan.State == "scanning"
		}
		if !found {
			t.Error("no pool shows scrub scanning")
		}
	})
}

// longText is 3 KiB of text with a 4-byte rune straddling byte 2048.
func longText(lead string) string {
	s := lead + strings.Repeat("a", 2046-len(lead)) + "𝄞é" + strings.Repeat("ü", 500)
	return s
}

func checkBoundedText(t *testing.T, what string, got *string, full string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil", what)
	}
	s := *got
	body, ok := strings.CutSuffix(s, TruncationMarker)
	if !ok || len(body) > spec.StatusTextMaxBytes || !utf8.ValidString(s) || !strings.HasPrefix(full, body) {
		t.Errorf("%s: %d bytes, marker %v, valid %v, a prefix %v", what, len(s), ok, utf8.ValidString(s), strings.HasPrefix(full, body))
	}
}

func TestTCSTG65_PermanentErrorsStatusAndActionTextAre(t *testing.T) {
	// Derived from synthetic/zpool-status-permanent-errors-60.json: status
	// and action replaced by 3 KiB texts with multi-byte runes at 2 KiB.
	doc := odecode(t, readFile(t, filepath.Join(synthetic, "zpool-status-permanent-errors-60.json")))
	pool := doc.at(t, "pools", "tank")
	status, action := longText("Status "), longText("Action ")
	pool.set("status", status+"\n")
	pool.set("action", action+"\n")
	var paths []string
	for _, x := range pool.vals["errlist"].([]any) {
		paths = append(paths, x.(string))
	}
	text := statusTextFor(t, doc, "\ttank        DEGRADED     0     0     0\n")
	for form, parse := range map[string]func([]byte) ([]Pool, error){"JSON": ParseStatusJSON, "text": ParseStatusText} {
		t.Run(form, func(t *testing.T) {
			in := oencode(t, doc)
			if form == "text" {
				in = []byte(text)
			}
			pools, err := parse(in)
			if err != nil {
				t.Fatal(err)
			}
			p := pools[0]
			if p.ErrorCount == nil || *p.ErrorCount != 60 {
				t.Errorf("ErrorCount = %s; want 60", show(p.ErrorCount))
			}
			if !slices.Equal(p.ErrorPaths, paths[:50]) || !p.ErrorPathsTruncated {
				t.Errorf("ErrorPaths = %d (first 50 in order: %v), truncated %v", len(p.ErrorPaths), slices.Equal(p.ErrorPaths, paths[:50]), p.ErrorPathsTruncated)
			}
			checkBoundedText(t, "Status", p.Status, status)
			checkBoundedText(t, "Action", p.Action, action)
		})
	}
}

func TestTCSTG66_ASuspendedPoolIsParsedWithThe(t *testing.T) {
	t.Run("zpool list", func(t *testing.T) {
		// Derived from upstream zpool-list.8.example3.json (WI 1.1's rules
		// for test-derived vectors; its text row follows the -H -p -o
		// PoolListColumns row of collect_pool / print_pool,
		// cmd/zpool/zpool_main.c L6831-L6943, where an unavailable
		// property prints "-"): the size, allocated, free, capacity,
		// fragmentation and dedupratio values set to "-", health to
		// SUSPENDED, and a readonly property added whole, in the shape of
		// the source's {value, source} property objects (a clone of health,
		// as the -o argv of A-58 requests the column), with value "-".
		doc := odecode(t, readFile(t, filepath.Join(upstream, "zpool-list.8.example3.json")))
		props := doc.at(t, "pools", "tank", "properties")
		for _, k := range []string{"size", "allocated", "free", "capacity", "fragmentation", "dedupratio"} {
			props.at(t, k).set("value", "-")
		}
		props.at(t, "health").set("value", "SUSPENDED")
		ro := props.at(t, "health").clone(t)
		ro.set("value", "-")
		props.set("readonly", ro)
		doc.at(t, "pools", "tank").set("state", "SUSPENDED")
		jp, err := ParsePoolListJSON(oencode(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		tp, err := ParsePoolListText([]byte("tank\t-\t-\t-\t-\t-\t-\tSUSPENDED\t-\t-\n"))
		if err != nil {
			t.Fatal(err)
		}
		for form, p := range map[string]Pool{"JSON": jp[0], "text": tp[0]} {
			if p.Health != "suspended" {
				t.Errorf("%s Health = %q", form, p.Health)
			}
			// readonly is "-" in both rows (the JSON row carries it, A-58);
			// altroot "-" is "no altroot", not an unreadable field, so it
			// has no reason.
			nils := map[string]bool{
				"sizeBytes": p.Size == nil, "allocatedBytes": p.Alloc == nil, "freeBytes": p.Free == nil,
				"capacityPercent": p.Capacity == nil, "fragmentationPercent": p.Frag == nil,
				"dedupRatio": p.Dedup == nil, "readOnly": p.ReadOnly == nil,
			}
			for k, isNil := range nils {
				if !isNil || p.Unavailable[k] == "" {
					t.Errorf("%s %s: nil %v, reason %q", form, k, isNil, p.Unavailable[k])
				}
			}
			if p.Altroot != nil {
				t.Errorf("%s Altroot = %q", form, *p.Altroot)
			}
		}
	})
	t.Run("zpool status", func(t *testing.T) {
		// Derived from synthetic/zpool-status-suspended.json: the vdev tree
		// and error_count removed (no vdev stats).
		doc := odecode(t, readFile(t, filepath.Join(synthetic, "zpool-status-suspended.json")))
		pool := doc.at(t, "pools", "tank")
		pool.del("vdevs")
		pool.del("dedup")
		pool.del("special")
		pool.del("error_count")
		pools, err := ParseStatusJSON(oencode(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		p := pools[0]
		if p.Health != "suspended" {
			t.Errorf("Health = %q", p.Health)
		}
		nils := map[string]bool{
			"vdevs": p.Vdevs == nil, "vdevCount": p.VdevCount == nil, "errors": p.Errors == nil,
			"permanentErrors": p.ErrorCount == nil,
		}
		for k, isNil := range nils {
			if !isNil || p.Unavailable[k] == "" {
				t.Errorf("%s: nil %v, reason %q", k, isNil, p.Unavailable[k])
			}
		}
		full, err := ParseStatusJSON(readFile(t, filepath.Join(synthetic, "zpool-status-suspended.json")))
		if err != nil {
			t.Fatal(err)
		}
		if full[0].Health != "suspended" || full[0].Errors == nil || full[0].Errors.Write != 21 {
			t.Errorf("full suspended vector: Health %q, Errors %+v; want suspended, 21 write errors on the 3 leaves", full[0].Health, full[0].Errors)
		}
	})
}

func TestTCSTG67_AnUnknownOutputVersionMajorIsRefused(t *testing.T) {
	unknown := readFile(t, filepath.Join(synthetic, "zpool-list-unknown-major.json"))
	if _, err := ParsePoolListJSON(unknown); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown major: %v; want ErrUnknownVersion", err)
	}
	doc := odecode(t, unknown)
	doc.at(t, "output_version").set("vers_minor", num(9))
	if _, err := ParsePoolListJSON(oencode(t, doc)); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("unknown major, higher minor: %v; want ErrUnknownVersion", err)
	}
	doc.at(t, "output_version").set("vers_major", num(KnownMajor))
	if pools, err := ParsePoolListJSON(oencode(t, doc)); err != nil || len(pools) != 1 {
		t.Errorf("known major, minor 9: %d pools, %v; want 1, nil", len(pools), err)
	}
	// Derived: the other commands' vectors with vers_major 1.
	others := []struct {
		file  string
		parse func([]byte) error
	}{
		{filepath.Join(upstream, "zpool-status-jv.source-derived.json"), func(b []byte) error { _, err := ParseStatusJSON(b); return err }},
		{filepath.Join(upstream, "zfs-list.8.example2.json"), func(b []byte) error { _, err := ParseDatasetsJSON(b); return err }},
		{filepath.Join(upstream, "zfs-list.8.example2.json"), func(b []byte) error { _, err := ParseSnapshotsJSON(b); return err }},
	}
	for _, o := range others {
		d := odecode(t, readFile(t, o.file))
		d.at(t, "output_version").set("vers_major", num(1))
		if err := o.parse(oencode(t, d)); !errors.Is(err, ErrUnknownVersion) {
			t.Errorf("%s major 1: %v; want ErrUnknownVersion", o.file, err)
		}
	}
	gd := odecode(t, []byte(poolGetJSON))
	gd.at(t, "output_version").set("vers_major", num(1))
	if _, err := ParsePoolGetJSON(oencode(t, gd)); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("zpool get major 1: %v; want ErrUnknownVersion", err)
	}
	// The wrong command is an error, not ErrUnknownVersion.
	list := readFile(t, filepath.Join(upstream, "zpool-list.8.example3.json"))
	if _, err := ParseStatusJSON(list); err == nil || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("zpool list document to ParseStatusJSON: %v; want a command error", err)
	}
	if _, err := ParsePoolGetJSON(list); err == nil || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("zpool list document to ParsePoolGetJSON: %v; want a command error", err)
	}
}

// poolGetJSON and poolGetText are `zpool get -j --json-int all tank` and
// `zpool get -H -p -o name,property,value,source all tank` of one pool and a
// few of zpool get's properties, until the host captures exist. No zpool get
// vector is upstream (zpool-get.8 has no JSON example, upstream PROVENANCE),
// so the source vector is upstream/zpool-list.8.example3.json, whose
// pools.<name>.properties.<prop>.{value, source} objects zpool get prints in
// the same shape; they are built by zprop_nvlist_one_property
// (lib/libzfs/libzfs_util.c L1423-L1520: an all-digit value is a JSON number
// with --json-int). The text rows are the -H -p -o name,property,value,source
// rows of get_callback (zpool_main.c L12653).
const poolGetJSON = `{"output_version":{"command":"zpool get","vers_major":0,"vers_minor":1},"pools":{"tank":{"name":"tank","type":"POOL","state":"ONLINE","guid":15220353080205405147,"txg":2671,"spa_version":5000,"zpl_version":5,"properties":{` +
	`"size":{"value":119185342464,"source":{"type":"NONE","data":"-"}},` +
	`"capacity":{"value":27,"source":{"type":"NONE","data":"-"}},` +
	`"altroot":{"value":"-","source":{"type":"DEFAULT","data":"-"}},` +
	`"health":{"value":"ONLINE","source":{"type":"NONE","data":"-"}},` +
	`"guid":{"value":15220353080205405147,"source":{"type":"NONE","data":"-"}},` +
	`"dedupratio":{"value":"1.00","source":{"type":"NONE","data":"-"}},` +
	`"feature@async_destroy":{"value":"enabled","source":{"type":"LOCAL","data":"-"}},` +
	`"feature@empty_bpobj":{"value":"active","source":{"type":"LOCAL","data":"-"}}}}}}`

const poolGetText = "tank\tsize\t119185342464\t-\ntank\tcapacity\t27\t-\ntank\taltroot\t-\tdefault\ntank\thealth\tONLINE\t-\n" +
	"tank\tguid\t15220353080205405147\t-\ntank\tdedupratio\t1.00\t-\ntank\tfeature@async_destroy\tenabled\tlocal\ntank\tfeature@empty_bpobj\tactive\tlocal\n"

func getOracle(t testing.TB, text []byte) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSuffix(string(text), "\n"), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			t.Fatalf("oracle row %q", l)
		}
		m[f[1]] = f[2]
	}
	return m
}

func TestTCSTG68_ZpoolGetParsingAndFeaturesUpgradable(t *testing.T) {
	check := func(t *testing.T, j, text []byte) map[string]string {
		t.Helper()
		jm, err := ParsePoolGetJSON(j)
		if err != nil {
			t.Fatal(err)
		}
		tm, err := ParsePoolGetText(text)
		if err != nil {
			t.Fatal(err)
		}
		oracle := getOracle(t, text)
		if !poolGetParity(jm, oracle) || !reflect.DeepEqual(tm, oracle) {
			t.Errorf("JSON map %v\ntext map %v\noracle %v", jm, tm, oracle)
		}
		return jm
	}
	t.Run("fixture", func(t *testing.T) {
		m := check(t, []byte(poolGetJSON), []byte(poolGetText))
		if FeaturesUpgradable(m) {
			t.Error("all enabled/active: FeaturesUpgradable true")
		}
		// Derived: one feature@ value set to disabled.
		doc := odecode(t, []byte(poolGetJSON))
		doc.at(t, "pools", "tank", "properties", "feature@async_destroy").set("value", "disabled")
		m = check(t, oencode(t, doc), []byte(strings.Replace(poolGetText, "async_destroy\tenabled", "async_destroy\tdisabled", 1)))
		if !FeaturesUpgradable(m) {
			t.Error("a disabled feature: FeaturesUpgradable false")
		}
	})
	t.Run("FeaturesUpgradable", func(t *testing.T) {
		for _, c := range []struct {
			m    map[string]string
			want bool
		}{
			{map[string]string{"feature@a": "enabled", "feature@b": "active"}, false},
			{map[string]string{"feature@a": "disabled"}, true},
			{map[string]string{"comment": "disabled", "feature@a": "active"}, false},
			{map[string]string{}, false},
		} {
			if got := FeaturesUpgradable(c.m); got != c.want {
				t.Errorf("FeaturesUpgradable(%v) = %v; want %v", c.m, got, c.want)
			}
		}
	})
	files, _ := filepath.Glob(filepath.Join(capRoot, "*", "zfs", "zpool-get-*.json"))
	t.Run("home-server capture present", func(t *testing.T) {
		capturegate.Require(t, filepath.Join(capRoot, "home-server", "zfs"))
		if len(files) == 0 {
			t.Fatal("no zpool-get-<pool>.json in the host captures")
		}
	})
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			text := captureStdout(t, strings.TrimSuffix(f, ".json")+".txt")
			m := check(t, captureStdout(t, f), text)
			want := false
			for k, v := range getOracle(t, text) {
				want = want || strings.HasPrefix(k, "feature@") && v == "disabled"
			}
			if FeaturesUpgradable(m) != want {
				t.Errorf("FeaturesUpgradable = %v; oracle %v", !want, want)
			}
		})
	}
}

// datasetsText and datasetsJSONFrom are the DF-2 dataset listing of three
// datasets in both forms (zfs list -j --json-int and -H -p), until the host
// captures exist. Source vector: upstream/zfs-list.8.example2.json, whose
// datasets.<name>.{name, type, pool, properties.<prop>.{value, source}}
// objects the JSON form repeats for each row; the JSON is built by
// collect_dataset (cmd/zfs/zfs_main.c L3710) and zprop_nvlist_one_property,
// the values by zfs_prop_get with literal (lib/libzfs/libzfs_dataset.c: quota
// 0 for none, compressratio "1.50", mounted yes/no), and the text rows are
// the -H -p rows of zfs_do_list (zfs_main.c L3884).
var datasetsText = "z1\tfilesystem\t1000\t9000\t96\t0\t0\t0\t0\tlz4\t1.00\t131072\t/z1\tyes\toff\t-\n" +
	"z1/tm\tfilesystem\t500\t1500\t400\t100\t2000\t-\t0\tzstd\t1.50\t1048576\t/media/timemachine\tno\taes-256-gcm\tavailable\n" +
	"z1/vol\tvolume\t300\t9000\t200\t0\t-\t-\t300\tlz4\t2.25\t-\t-\t-\toff\t-\n"

func datasetsJSONFrom(t testing.TB, text string) []byte {
	t.Helper()
	cols := strings.Split(DatasetColumns, ",")
	doc := odecode(t, []byte(`{"output_version":{"command":"zfs list","vers_major":0,"vers_minor":1},"datasets":{}}`))
	ds := doc.at(t, "datasets")
	for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		f := strings.Split(l, "\t")
		item := &onode{vals: map[string]any{}}
		item.set("name", f[0])
		item.set("type", strings.ToUpper(f[1]))
		item.set("pool", poolOf(f[0]))
		item.set("createtxg", num(1))
		props := &onode{vals: map[string]any{}}
		for i, c := range cols[1:] {
			v := any(f[i+1])
			if strings.Trim(f[i+1], "0123456789") == "" {
				v = json.Number(f[i+1])
			}
			p := &onode{vals: map[string]any{}}
			p.set("value", v)
			src := &onode{vals: map[string]any{}}
			src.set("type", "NONE")
			src.set("data", "-")
			p.set("source", src)
			name := c
			if alt, ok := jsonColumns[c]; ok {
				name = alt
			}
			props.set(name, p)
		}
		item.set("properties", props)
		ds.set(f[0], item)
	}
	return oencode(t, doc)
}

func checkDatasets(t *testing.T, ds []Dataset, text []byte) {
	t.Helper()
	oracle := tabOracle(t, text, DatasetColumns)
	if len(ds) != len(oracle) {
		t.Fatalf("parsed %d datasets; text has %d", len(ds), len(oracle))
	}
	for _, d := range ds {
		row := oracle[d.Name]
		quota := func(w string) *uint64 {
			if w == "0" || w == "-" || w == "none" {
				return nil
			}
			return oracleUint(w)
		}
		word := func(w string) *string {
			if w == "-" {
				return nil
			}
			return &w
		}
		var mounted *bool
		switch row["mounted"] {
		case "yes":
			mounted = ptr(true)
		case "no":
			mounted = ptr(false)
		}
		ok := d.Type == row["type"] && d.Pool == poolOf(d.Name) &&
			eqPtr(d.Used, oracleUint(row["used"])) && eqPtr(d.Avail, oracleUint(row["avail"])) &&
			eqPtr(d.Refer, oracleUint(row["refer"])) && eqPtr(d.UsedBySnapshots, oracleUint(row["usedbysnapshots"])) &&
			eqPtr(d.Quota, quota(row["quota"])) && eqPtr(d.Refquota, quota(row["refquota"])) &&
			eqPtr(d.Reservation, oracleUint(row["reservation"])) && eqPtr(d.Compression, word(row["compression"])) &&
			eqPtr(d.CompressRatio, oracleFloat(row["compressratio"])) && eqPtr(d.Recordsize, oracleUint(row["recordsize"])) &&
			eqPtr(d.Mountpoint, word(row["mountpoint"])) && eqPtr(d.Mounted, mounted) &&
			eqPtr(d.Encryption, word(row["encryption"])) && eqPtr(d.KeyStatus, word(row["keystatus"]))
		if !ok {
			t.Errorf("dataset %s = %+v; oracle %v", d.Name, d, row)
		}
		if d.Type == "volume" && d.Mountpoint != nil {
			t.Errorf("volume %s has a mountpoint", d.Name)
		}
	}
}

func TestTCSTG69_DatasetParsingAgreesWithTheTextCapture(t *testing.T) {
	if _, ok := reflect.TypeFor[Dataset]().FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, "keylocation") }); ok {
		t.Error("Dataset has a keylocation field")
	}
	t.Run("fixture", func(t *testing.T) {
		jd, err := ParseDatasetsJSON(datasetsJSONFrom(t, datasetsText))
		if err != nil {
			t.Fatal(err)
		}
		td, err := ParseDatasetsText([]byte(datasetsText))
		if err != nil {
			t.Fatal(err)
		}
		checkDatasets(t, jd, []byte(datasetsText))
		checkDatasets(t, td, []byte(datasetsText))
		tm := td[1]
		if tm.Quota == nil || *tm.Quota != 2000 || tm.Refquota != nil || tm.CompressRatio == nil || *tm.CompressRatio != 1.5 ||
			tm.Mounted == nil || *tm.Mounted || *tm.Encryption != "aes-256-gcm" || *tm.KeyStatus != "available" {
			t.Errorf("z1/tm = %+v", tm)
		}
		if td[0].Quota != nil || td[0].Mounted == nil || !*td[0].Mounted || td[2].Mounted != nil || td[2].Mountpoint != nil {
			t.Errorf("z1 quota 0 or the volume's mount fields: %+v %+v", td[0], td[2])
		}
	})
	t.Run("upstream zfs-list.8 example", func(t *testing.T) {
		ds, err := ParseDatasetsJSON(readFile(t, filepath.Join(upstream, "zfs-list.8.example2.json")))
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) == 0 || ds[0].Name != "pool" || ds[0].Mountpoint == nil || *ds[0].Mountpoint != "/pool" {
			t.Errorf("upstream datasets: %+v", ds)
		}
		// The example asks only used, available, referenced and mountpoint:
		// every other column is nil with a reason.
		if ds[0].Quota != nil || ds[0].Unavailable["compressRatio"] == "" {
			t.Errorf("missing columns: %+v", ds[0])
		}
	})
	for _, dir := range hostCaptures(t, "zfs-list-datasets.json") {
		t.Run(dir, func(t *testing.T) {
			text := captureStdout(t, filepath.Join(dir, "zfs", "zfs-list-datasets.txt"))
			jd, err := ParseDatasetsJSON(captureStdout(t, filepath.Join(dir, "zfs", "zfs-list-datasets.json")))
			if err != nil {
				t.Fatal(err)
			}
			td, err := ParseDatasetsText(text)
			if err != nil {
				t.Fatal(err)
			}
			checkDatasets(t, jd, text)
			checkDatasets(t, td, text)
		})
	}
}

// snapshotsText is a snapshots.list text of two snapshots: a
// zfs-auto-snapshot one without the property and one Nexus made. Its JSON twin
// (snapshotsJSONFrom) has the shape of upstream/zfs-list.8.example2.json's
// datasets.<name> objects (zfs list -j -t snapshot, collect_dataset,
// cmd/zfs/zfs_main.c L3710) with each property reduced to its value (the
// parser reads only values); the text rows are the -H -p rows of zfs_do_list
// (L3884).
const snapshotsText = "z1/tm@zfs-auto-snap_daily-2026-11-18-0425\t1795494300\t0\t400\t0\t-\n" +
	"z1@nexus-20261118T120000Z\t1795521600\t4096\t96\t1\top-0123456789abcdef\n"

func snapshotsJSONFrom(t testing.TB, text string, dropProperty bool) []byte {
	t.Helper()
	cols := strings.Split(SnapshotColumns, ",")
	doc := odecode(t, []byte(`{"output_version":{"command":"zfs list","vers_major":0,"vers_minor":1},"datasets":{}}`))
	for _, l := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		f := strings.Split(l, "\t")
		ds, snap, _ := strings.Cut(f[0], "@")
		item := &onode{vals: map[string]any{}}
		for k, v := range map[string]any{"name": f[0], "type": "SNAPSHOT", "pool": poolOf(f[0]), "createtxg": num(1), "dataset": ds, "snapshot_name": snap} {
			item.set(k, v)
		}
		props := &onode{vals: map[string]any{}}
		for i, c := range cols[1:] {
			if c == spec.CreatedByProperty && dropProperty {
				continue
			}
			v := any(f[i+1])
			if strings.Trim(f[i+1], "0123456789") == "" {
				v = json.Number(f[i+1])
			}
			p := &onode{vals: map[string]any{}}
			p.set("value", v)
			name := c
			if alt, ok := jsonColumns[c]; ok {
				name = alt
			}
			props.set(name, p)
		}
		item.set("properties", props)
		doc.at(t, "datasets").set(f[0], item)
	}
	return oencode(t, doc)
}

func checkSnapshots(t *testing.T, ss []Snapshot, text []byte) {
	t.Helper()
	oracle := tabOracle(t, text, SnapshotColumns)
	if len(ss) != len(oracle) {
		t.Fatalf("parsed %d snapshots; text has %d lines", len(ss), len(oracle))
	}
	for _, s := range ss {
		row := oracle[s.Name]
		sec, _ := strconv.ParseInt(row["creation"], 10, 64)
		by := row[spec.CreatedByProperty]
		if by == "-" {
			by = ""
		}
		ds, _, _ := strings.Cut(s.Name, "@")
		if s.Dataset != ds || !s.CreatedAt.Equal(time.Unix(sec, 0)) || s.CreatedAt.Location() != time.UTC ||
			!eqPtr(s.Used, oracleUint(row["used"])) || !eqPtr(s.Refer, oracleUint(row["refer"])) ||
			!eqPtr(s.Holds, oracleUint(row["userrefs"])) || s.CreatedBy != by {
			t.Errorf("snapshot %s = %+v; oracle %v", s.Name, s, row)
		}
	}
}

func TestTCSTG70_SnapshotParsingSplitsNamesAndReadsThe(t *testing.T) {
	t.Run("fixture", func(t *testing.T) {
		ts, err := ParseSnapshotsText([]byte(snapshotsText))
		if err != nil {
			t.Fatal(err)
		}
		checkSnapshots(t, ts, []byte(snapshotsText))
		if ts[1].CreatedBy != "op-0123456789abcdef" || ts[0].CreatedBy != "" {
			t.Errorf("CreatedBy = %q, %q", ts[0].CreatedBy, ts[1].CreatedBy)
		}
		for _, drop := range []bool{false, true} {
			js, err := ParseSnapshotsJSON(snapshotsJSONFrom(t, snapshotsText, drop))
			if err != nil {
				t.Fatal(err)
			}
			if drop {
				// An absent property reads as unset.
				if js[0].CreatedBy != "" || js[1].CreatedBy != "" {
					t.Errorf("absent property: CreatedBy = %q, %q", js[0].CreatedBy, js[1].CreatedBy)
				}
				continue
			}
			checkSnapshots(t, js, []byte(snapshotsText))
		}
		times, err := ParseSnapshotsText([]byte("z1/tm@a\t1795494300\nz1@b\t1795521600\n"))
		if err != nil || len(times) != 2 || times[1].Dataset != "z1" || times[1].Used != nil {
			t.Errorf("name,creation listing: %+v, %v", times, err)
		}
	})
	for _, dir := range hostCaptures(t, "zfs-list-snapshots.txt") {
		t.Run(dir, func(t *testing.T) {
			text := captureStdout(t, filepath.Join(dir, "zfs", "zfs-list-snapshots.txt"))
			ss, err := ParseSnapshotsText(text)
			if err != nil {
				t.Fatal(err)
			}
			checkSnapshots(t, ss, text)
			if lines := bytes.Count(text, []byte("\n")); len(ss) != lines {
				t.Errorf("%d snapshots; %d lines", len(ss), lines)
			}
		})
	}
}

// scanTextRounding is the largest relative difference between a scan size
// the text form prints and the JSON integer: zfs_nicebytes (zfs_nicenum_format,
// lib/libzutil/zutil_nicenum.c L62-L140) prints at most 5 characters, so a
// value of 1.00 to 9.99 of a unit is off by at most 0.005/1.00 = 0.5%, and
// 0.6% bounds every unit. A printed value without a fraction stands for the
// integer it names (an exact multiple of its unit), which this bound also
// covers.
const scanTextRounding = 0.006

// normalizeScan changes the JSON form's scan, in place, where the text form
// cannot carry the same value. The list is exactly the source-backed one of
// A-59 (TC-STG-71), from OpenZFS 2.4.1 print_scan_scrub_resilver_status
// (cmd/zpool/zpool_main.c L8937-L9029, commit 1c702dd):
//
// Not printed (the JSON value is set to nil):
//   - Examined and ToExamine of a finished or canceled scan: the finished and
//     canceled branches (L8970-L8993) return before the "scanned" line is
//     printed (L9018 on);
//   - Processed of a canceled scan: the canceled branch (L8983-L8993) prints
//     no processed size, while the finished branch prints processed_buf
//     (L8970-L8981);
//   - Errors of any scan that is not finished: only the finished branch
//     prints pss_errors (L8973-L8981);
//   - Start of a canceled scan: the canceled branch prints only the end
//     (L8984-L8992).
//
// Rounded (the text value replaces the JSON value when they agree):
//   - Processed, Examined and ToExamine are printed by zfs_nicebytes even with
//     -p (processed_buf, scanned_buf, total_s_buf; L8948, L9018 on), so the
//     text value stands for the JSON one when it is within scanTextRounding
//     of it. When presenceOnly is set, any value stands: a host capture's
//     JSON and text commands run seconds apart while a scan advances, so only
//     whether each side has the value is compared.
//
// Dates are not normalized: each text is parsed in the zone it was printed in.
func normalizeScan(j, txt *Scan, presenceOnly bool) {
	if j == nil || txt == nil {
		return
	}
	switch j.State {
	case ScanFinished:
		j.Examined, j.ToExamine = nil, nil
	case ScanCanceled:
		j.Examined, j.ToExamine, j.Processed, j.Start = nil, nil, nil, nil
	}
	if j.State != ScanFinished {
		j.Errors = nil
	}
	for _, pair := range [][2]**uint64{{&j.Processed, &txt.Processed}, {&j.Examined, &txt.Examined}, {&j.ToExamine, &txt.ToExamine}} {
		a, b := *pair[0], *pair[1]
		if a == nil || b == nil {
			continue
		}
		if presenceOnly || math.Abs(float64(*a)-float64(*b)) <= scanTextRounding*float64(*a) {
			*pair[0] = b
		}
	}
}

// provenanceKeys reads the `key: value` lines of a capture's PROVENANCE
// (indented lines belong to the list above them and are skipped).
func provenanceKeys(t testing.TB, dir string) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, l := range strings.Split(string(readFile(t, filepath.Join(dir, "PROVENANCE"))), "\n") {
		if l == "" || l[0] == ' ' {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			keys[k] = strings.TrimSpace(v)
		}
	}
	return keys
}

// zoneOf is the zone a host capture's text dates were printed in (A-60): its
// PROVENANCE `timezone` loaded with time.LoadLocation (UTC included), or a
// fixed zone of `utc-offset` when the timezone is `unknown`. The zone's offset
// at the capture's `date` must equal `utc-offset`, so a wrong zone is refused
// here and never becomes a silent shift in the parity test.
func zoneOf(keys map[string]string) (*time.Location, error) {
	off := keys["utc-offset"]
	if len(off) != 5 || off[0] != '+' && off[0] != '-' {
		return nil, fmt.Errorf("utc-offset %q is not [+-]hhmm", off)
	}
	h, errH := strconv.Atoi(off[1:3])
	m, errM := strconv.Atoi(off[3:])
	if errH != nil || errM != nil || h > 23 || m > 59 {
		return nil, fmt.Errorf("utc-offset %q is not [+-]hhmm", off)
	}
	secs := h*3600 + m*60
	if off[0] == '-' {
		secs = -secs
	}
	loc := time.FixedZone(off, secs)
	if name := keys["timezone"]; name == "" {
		return nil, errors.New("no timezone")
	} else if name != "unknown" {
		l, err := time.LoadLocation(name)
		if err != nil {
			return nil, fmt.Errorf("timezone %q: %v", name, err)
		}
		loc = l
	}
	date, err := time.Parse(time.RFC3339, keys["date"])
	if err != nil {
		return nil, fmt.Errorf("date %q: %v", keys["date"], err)
	}
	if _, got := date.In(loc).Zone(); got != secs {
		return nil, fmt.Errorf("zone %s has offset %+d s at %s; utc-offset %s says %+d s", loc, got, keys["date"], off, secs)
	}
	return loc, nil
}

// withTextZone runs f with textLoc set to loc.
func withTextZone(loc *time.Location, f func()) {
	old := textLoc
	textLoc = loc
	defer func() { textLoc = old }()
	f()
}

func TestTCSTG71_TheJSONAndTextParsersProduceThe(t *testing.T) {
	pinUTC(t)
	// statusPair parses both forms and compares them after normalizeScan.
	// host is set for a host capture, whose scanning scan is compared by
	// presence only; the text's zone is textLoc, set by the caller.
	statusPair := func(t *testing.T, j, text []byte, host bool) {
		t.Helper()
		jp, err := ParseStatusJSON(j)
		if err != nil {
			t.Fatal(err)
		}
		tp, err := ParseStatusText(text)
		if err != nil {
			t.Fatal(err)
		}
		if len(jp) != len(tp) {
			t.Fatalf("JSON %d pools; text %d", len(jp), len(tp))
		}
		for i := range jp {
			presenceOnly := host && jp[i].Scan != nil && jp[i].Scan.State == ScanScanning
			normalizeScan(jp[i].Scan, tp[i].Scan, presenceOnly)
			if !reflect.DeepEqual(jp[i], tp[i]) {
				t.Errorf("pool %s:\nJSON %s\ntext %s", jp[i].Name, dump(jp[i]), dump(tp[i]))
			}
		}
	}
	t.Run("status: degraded mirror with classes", func(t *testing.T) {
		statusPair(t, mirrorWithClasses(t), []byte(mirrorWithClassesText), false)
	})
	scanFiles := []string{"zpool-status-scrub-scanning.json", "zpool-status-scrub-finished-repairs.json", "zpool-status-scrub-canceled.json", "zpool-status-scrub-paused.json", "zpool-status-resilver-scanning.json"}
	rootOnly := func(t *testing.T, f string, edit func(s *onode)) ([]byte, string) {
		b, text := scanVector(t, f, edit)
		// The fixture's config table has the root only.
		doc := odecode(t, b)
		pool := doc.at(t, "pools", "tank")
		root := pool.at(t, "vdevs", "tank")
		root.del("vdevs")
		setCounts(root, 0, 0, 0)
		root.set("state", "ONLINE")
		pool.del("dedup")
		pool.del("special")
		return oencode(t, doc), text
	}
	for _, f := range scanFiles {
		t.Run("status: "+f, func(t *testing.T) {
			b, text := rootOnly(t, f, nil)
			statusPair(t, b, []byte(text), false)
		})
	}
	t.Run("status: dates in a non-UTC zone", func(t *testing.T) {
		// The finished and the paused scrub's text twins printed in
		// America/New_York and parsed in it (textLoc), as a host capture's
		// are in its recorded zone (A-60): equal with no date normalized. The
		// same text parsed in UTC differs, so the zone is exercised.
		ny, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"zpool-status-scrub-finished-repairs.json", "zpool-status-scrub-paused.json"} {
			old := twinZone
			twinZone = ny
			b, text := rootOnly(t, f, nil)
			twinZone = old
			withTextZone(ny, func() { statusPair(t, b, []byte(text), false) })
			jp, _ := ParseStatusJSON(b)
			tp, err := ParseStatusText([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			same := func(a, b *time.Time) bool { return a != nil && b != nil && a.Equal(*b) }
			if same(jp[0].Scan.End, tp[0].Scan.End) || same(jp[0].Scan.Start, tp[0].Scan.Start) {
				t.Errorf("%s: a date parsed in UTC equals the one printed in %s", f, ny)
			}
		}
	})
	t.Run("zone of a host capture", func(t *testing.T) {
		good := map[string]string{"timezone": "Europe/Berlin", "utc-offset": "+0100", "date": "2026-11-19T10:00:00Z"}
		if loc, err := zoneOf(good); err != nil || loc.String() != "Europe/Berlin" {
			t.Errorf("Berlin in November: %v, %v", loc, err)
		}
		summer := map[string]string{"timezone": "Europe/Berlin", "utc-offset": "+0200", "date": "2026-07-19T10:00:00Z"}
		if _, err := zoneOf(summer); err != nil {
			t.Errorf("Berlin in July at +0200: %v", err)
		}
		if loc, err := zoneOf(map[string]string{"timezone": "UTC", "utc-offset": "+0000", "date": "2026-11-19T10:00:00Z"}); err != nil || loc != time.UTC {
			t.Errorf("UTC: %v, %v", loc, err)
		}
		if loc, err := zoneOf(map[string]string{"timezone": "unknown", "utc-offset": "-0330", "date": "2026-11-19T10:00:00Z"}); err != nil || loc.String() != "-0330" {
			t.Errorf("unknown zone, fixed offset: %v, %v", loc, err)
		}
		for name, keys := range map[string]map[string]string{
			"offset disagrees with the zone": {"timezone": "Europe/Berlin", "utc-offset": "+0200", "date": "2026-11-19T10:00:00Z"},
			"no such zone":                   {"timezone": "Mars/Olympus", "utc-offset": "+0000", "date": "2026-11-19T10:00:00Z"},
			"no timezone key":                {"utc-offset": "+0000", "date": "2026-11-19T10:00:00Z"},
			"bad utc-offset":                 {"timezone": "UTC", "utc-offset": "0000", "date": "2026-11-19T10:00:00Z"},
			"bad date":                       {"timezone": "UTC", "utc-offset": "+0000", "date": "yesterday"},
		} {
			if loc, err := zoneOf(keys); err == nil {
				t.Errorf("%s: zone %v accepted", name, loc)
			}
		}
	})
	t.Run("normalization is exactly A-59's list", func(t *testing.T) {
		u := func(n uint64) *uint64 { return &n }
		full := func(state string) *Scan {
			return &Scan{Function: ScanScrub, State: state, Start: ptr(time.Unix(1000, 0).UTC()), End: ptr(time.Unix(2000, 0).UTC()),
				Examined: u(1000), ToExamine: u(2000), Processed: u(3000), Errors: u(4)}
		}
		// A text scan that prints nothing the state hides, and sizes off by
		// 0.5%: equal after normalizing; off by 1%, or an extra printed
		// value, not equal.
		for _, c := range []struct {
			state                              string
			wantExamined, wantProc, wantErrors bool
		}{
			{ScanScanning, true, true, false}, {ScanPaused, true, true, false},
			{ScanFinished, false, true, true}, {ScanCanceled, false, false, false},
		} {
			j, txt := full(c.state), full(c.state)
			if !c.wantExamined {
				txt.Examined, txt.ToExamine = nil, nil
			}
			if !c.wantProc {
				txt.Processed = nil
			}
			if !c.wantErrors {
				txt.Errors = nil
			}
			if c.state == ScanCanceled {
				txt.Start = nil
			}
			if c.wantExamined {
				txt.Examined, txt.ToExamine = u(995), u(2010)
			}
			if c.wantProc {
				txt.Processed = u(2985)
			}
			normalizeScan(j, txt, false)
			if !reflect.DeepEqual(j, txt) {
				t.Errorf("%s: JSON %s\ntext %s", c.state, dump(j), dump(txt))
			}
			j, txt = full(c.state), full(c.state)
			if c.wantProc {
				txt.Processed = u(2950) // 1.7% off: not a rounding
				normalizeScan(j, txt, false)
				if reflect.DeepEqual(j, txt) {
					t.Errorf("%s: a Processed 1.7%% off was normalized", c.state)
				}
			}
		}
		// Nothing else is normalized: a date, a function or an error count
		// the text prints that differs from the JSON value stays different.
		j, txt := full(ScanFinished), full(ScanFinished)
		j.Examined, j.ToExamine = nil, nil
		txt.Examined, txt.ToExamine = nil, nil
		txt.End = ptr(time.Unix(2001, 0).UTC())
		normalizeScan(j, txt, false)
		if reflect.DeepEqual(j, txt) {
			t.Error("a different End was normalized")
		}
		j, txt = full(ScanFinished), full(ScanFinished)
		txt.Errors = u(5)
		normalizeScan(j, txt, false)
		if reflect.DeepEqual(j, txt) {
			t.Error("a different Errors of a finished scan was normalized")
		}
		// presenceOnly: a host scanning capture compares presence, not size.
		j, txt = full(ScanScanning), full(ScanScanning)
		txt.Examined = u(7777)
		normalizeScan(j, txt, true)
		if !reflect.DeepEqual(j.Examined, txt.Examined) {
			t.Error("presence-only did not accept a different size")
		}
		j, txt = full(ScanScanning), full(ScanScanning)
		txt.Examined = nil
		normalizeScan(j, txt, true)
		if reflect.DeepEqual(j, txt) {
			t.Error("presence-only accepted a missing value")
		}
	})
	t.Run("list", func(t *testing.T) {
		// The text row and the JSON document of one pool: the JSON carries
		// readonly because the argv requests it (A-58).
		text := "tank\t119185342464\t33071248179\t86114094285\t27\t0\t1.00\tONLINE\t-\toff\n"
		doc := odecode(t, readFile(t, filepath.Join(upstream, "zpool-list.8.example3.json")))
		props := doc.at(t, "pools", "tank", "properties")
		for k, v := range map[string]any{"size": num(119185342464), "allocated": num(33071248179), "free": num(86114094285), "capacity": num(27), "fragmentation": num(0), "dedupratio": "1.00"} {
			props.at(t, k).set("value", v)
		}
		ro := props.at(t, "health").clone(t)
		ro.set("value", "off")
		props.set("readonly", ro)
		jp, err := ParsePoolListJSON(oencode(t, doc))
		if err != nil {
			t.Fatal(err)
		}
		tp, err := ParsePoolListText([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(jp, tp) {
			t.Errorf("JSON %s\ntext %s", dump(jp), dump(tp))
		}
	})
	t.Run("get", func(t *testing.T) {
		jm, _ := ParsePoolGetJSON([]byte(poolGetJSON))
		tm, _ := ParsePoolGetText([]byte(poolGetText))
		if !reflect.DeepEqual(jm, tm) {
			t.Errorf("JSON %v\ntext %v", jm, tm)
		}
	})
	t.Run("datasets", func(t *testing.T) {
		jd, _ := ParseDatasetsJSON(datasetsJSONFrom(t, datasetsText))
		td, _ := ParseDatasetsText([]byte(datasetsText))
		if !reflect.DeepEqual(jd, td) {
			t.Errorf("JSON %s\ntext %s", dump(jd), dump(td))
		}
	})
	t.Run("snapshots", func(t *testing.T) {
		js, _ := ParseSnapshotsJSON(snapshotsJSONFrom(t, snapshotsText, false))
		ts, _ := ParseSnapshotsText([]byte(snapshotsText))
		if !reflect.DeepEqual(js, ts) {
			t.Errorf("JSON %s\ntext %s", dump(js), dump(ts))
		}
	})
	pairs := []struct{ json, text string }{
		{"zpool-list.json", "zpool-list.txt"},
		{"zpool-status-v.json", "zpool-status-v.txt"},
		{"zfs-list-datasets.json", "zfs-list-datasets.txt"},
		{"zfs-list-snapshots.json", "zfs-list-snapshots.txt"},
	}
	for _, dir := range hostCaptures(t, "zpool-list.json") {
		// The text's dates were printed in the capture's recorded zone
		// (A-60); a missing or wrong zone fails here, not in a date.
		loc, err := zoneOf(provenanceKeys(t, dir))
		if err != nil {
			t.Fatalf("%s/PROVENANCE: %v", dir, err)
		}
		for _, p := range pairs {
			t.Run(filepath.Join(dir, p.json), func(t *testing.T) {
				j := captureStdout(t, filepath.Join(dir, "zfs", p.json))
				text := captureStdout(t, filepath.Join(dir, "zfs", p.text))
				var jv, tv any
				var jerr, terr error
				switch p.json {
				case "zpool-list.json":
					jv, jerr = ParsePoolListJSON(j)
					tv, terr = ParsePoolListText(text)
				case "zpool-status-v.json":
					withTextZone(loc, func() { statusPair(t, j, text, true) })
					return
				case "zfs-list-datasets.json":
					jv, jerr = ParseDatasetsJSON(j)
					tv, terr = ParseDatasetsText(text)
				default:
					jv, jerr = ParseSnapshotsJSON(j)
					tv, terr = ParseSnapshotsText(text)
				}
				if jerr != nil || terr != nil {
					t.Fatal(jerr, terr)
				}
				if !reflect.DeepEqual(jv, tv) {
					t.Errorf("JSON %s\ntext %s", dump(jv), dump(tv))
				}
			})
		}
		gets, _ := filepath.Glob(filepath.Join(dir, "zfs", "zpool-get-*.json"))
		for _, g := range gets {
			t.Run(g, func(t *testing.T) {
				jm, err := ParsePoolGetJSON(captureStdout(t, g))
				if err != nil {
					t.Fatal(err)
				}
				tm, err := ParsePoolGetText(captureStdout(t, strings.TrimSuffix(g, ".json")+".txt"))
				if err != nil {
					t.Fatal(err)
				}
				if !poolGetParity(jm, tm) {
					t.Errorf("JSON %v\ntext %v", jm, tm)
				}
			})
		}
	}
}

// poolGetParity is the JSON-versus-text parity rule of zpool get (TC-STG-68,
// TC-STG-71) with the one A-61 exception: `zpool get -j --json-int` prints a
// uint64 property above 2^63-1 (guid, load_guid) as 9223372036854775807,
// while the text form prints the real value. A JSON value of exactly INT64_MAX
// is accepted where the text value is a decimal integer above INT64_MAX;
// every other key must be equal and both maps must have the same keys.
func poolGetParity(jsonMap, textMap map[string]string) bool {
	if len(jsonMap) != len(textMap) {
		return false
	}
	for k, tv := range textMap {
		jv, ok := jsonMap[k]
		if !ok {
			return false
		}
		if jv == tv {
			continue
		}
		if jv != "9223372036854775807" {
			return false
		}
		if n, err := strconv.ParseUint(tv, 10, 64); err != nil || n <= math.MaxInt64 {
			return false
		}
	}
	return true
}

// A-61: the parity helper accepts the saturated uint64 and nothing else.
func TestPoolGetParityA61(t *testing.T) {
	const sat = "9223372036854775807"
	for _, c := range []struct {
		name       string
		json, text map[string]string
		want       bool
	}{
		{"saturated vs big", map[string]string{"guid": sat, "size": "5"}, map[string]string{"guid": "16746263287341631117", "size": "5"}, true},
		{"equal", map[string]string{"guid": "16746263287341631117"}, map[string]string{"guid": "16746263287341631117"}, true},
		{"saturated vs small", map[string]string{"guid": sat}, map[string]string{"guid": "5"}, false},
		{"saturated vs INT64_MAX text", map[string]string{"guid": sat}, map[string]string{"guid": sat}, true},
		{"saturated vs INT64_MAX+1 text", map[string]string{"guid": sat}, map[string]string{"guid": "9223372036854775808"}, true},
		{"saturated vs non-numeric", map[string]string{"guid": sat}, map[string]string{"guid": "-"}, false},
		{"other key mismatch", map[string]string{"guid": sat, "size": "6"}, map[string]string{"guid": "16746263287341631117", "size": "5"}, false},
		{"non-saturated big mismatch", map[string]string{"guid": "16746263287341631116"}, map[string]string{"guid": "16746263287341631117"}, false},
		{"missing in json", map[string]string{"guid": sat}, map[string]string{"guid": "16746263287341631117", "size": "5"}, false},
		{"extra in json", map[string]string{"guid": sat, "size": "5"}, map[string]string{"guid": "16746263287341631117"}, false},
		{"same count different keys", map[string]string{"guid": sat, "a": "1"}, map[string]string{"guid": "16746263287341631117", "b": "1"}, false},
	} {
		if got := poolGetParity(c.json, c.text); got != c.want {
			t.Errorf("%s: poolGetParity = %v; want %v", c.name, got, c.want)
		}
	}
}

func dump(v any) string {
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

// probedArgvs reads sandbox-probe/commands: each probe line is
// `<file>: -p <property>... /usr/bin/env -i PATH=... LC_ALL=C <argv>`, and the
// argv, the text after `LC_ALL=C `, is what the provider's Argv must equal.
// Lines without that prefix (stat -f, the bounded reads) are not argv.
func probedArgvs(commands string) map[string]bool {
	const marker = " LC_ALL=C "
	out := map[string]bool{}
	for _, l := range strings.Split(commands, "\n") {
		l = strings.TrimSuffix(l, " [noeol]")
		if i := strings.Index(l, marker); i >= 0 {
			out[l[i+len(marker):]] = true
		}
	}
	return out
}

func TestTCSTG72_ArgvBuildsEveryDF2AndHelper(t *testing.T) {
	p := Paths{Zpool: "/usr/sbin/zpool", Zfs: "/usr/sbin/zfs"}
	const plc = "name,size,allocated,free,capacity,fragmentation,dedupratio,health,altroot,readonly"
	ds := "-t filesystem,volume -o name,type,used,avail,refer,usedbysnapshots,quota,refquota,reservation,compression,compressratio,recordsize,mountpoint,mounted,encryption,keystatus"
	snapAll := "list -H -p -t snapshot -o name,creation,used,refer,userrefs,nexus:created-by"
	type c struct {
		cmd        Command
		args       []string
		json, text string // after argv[0]
		tool       string
		// probed is set for a read the unit's sandbox probe must have run
		// (every Command but the four writes and CmdSnapshotGet, the
		// helper's readback, which runs outside the sandbox, D-4).
		probed bool
	}
	cases := []c{
		{CmdVersion, nil, "version", "version", p.Zfs, true},
		{CmdPoolNames, nil, "list -H -o name", "list -H -o name", p.Zpool, true},
		// zpool list -j prints only its default columns unless -o is given
		// (A-58), so both forms request the same columns.
		{CmdPoolList, nil, "list -j --json-int -o " + plc, "list -H -p -o " + plc, p.Zpool, true},
		{CmdPoolList, []string{"z1"}, "list -j --json-int -o " + plc + " z1", "list -H -p -o " + plc + " z1", p.Zpool, true},
		{CmdPoolStatus, nil, "status -j --json-int", "status -p", p.Zpool, true},
		{CmdPoolStatus, []string{"z1"}, "status -j --json-int z1", "status -p z1", p.Zpool, true},
		{CmdPoolStatusVerbose, nil, "status -j --json-int -v", "status -p -v", p.Zpool, true},
		{CmdPoolStatusVerbose, []string{"z1"}, "status -j --json-int -v z1", "status -p -v z1", p.Zpool, true},
		{CmdPoolGet, []string{"z1"}, "get -j --json-int all z1", "get -H -p -o name,property,value,source all z1", p.Zpool, true},
		{CmdDatasetList, nil, "list -j --json-int " + ds, "list -H -p " + ds, p.Zfs, true},
		{CmdDatasetList, []string{"z1/tm"}, "list -j --json-int " + ds + " z1/tm", "list -H -p " + ds + " z1/tm", p.Zfs, true},
		{CmdSnapshotTimes, nil, "list -H -p -t snapshot -o name,creation", "list -H -p -t snapshot -o name,creation", p.Zfs, true},
		{CmdSnapshotTimes, []string{"z1/tm"}, "list -H -p -t snapshot -o name,creation -d 1 z1/tm", "list -H -p -t snapshot -o name,creation -d 1 z1/tm", p.Zfs, true},
		{CmdSnapshotList, nil, snapAll, snapAll, p.Zfs, true},
		{CmdSnapshotList, []string{"z1/tm"}, snapAll + " -d 1 z1/tm", snapAll + " -d 1 z1/tm", p.Zfs, true},
		{CmdSnapshotListRecursive, []string{"z1"}, snapAll + " -r z1", snapAll + " -r z1", p.Zfs, true},
		{CmdSnapshotGet, []string{"z1@nexus-x"}, snapAll + " z1@nexus-x", snapAll + " z1@nexus-x", p.Zfs, false},
		{CmdScrubStart, []string{"z1"}, "scrub z1", "scrub z1", p.Zpool, false},
		{CmdScrubStop, []string{"z1"}, "scrub -s z1", "scrub -s z1", p.Zpool, false},
		{CmdSnapshotCreate, []string{"op-0123456789abcdef", "z1/tm@nexus-x"}, "snapshot -o nexus:created-by=op-0123456789abcdef z1/tm@nexus-x", "snapshot -o nexus:created-by=op-0123456789abcdef z1/tm@nexus-x", p.Zfs, false},
		{CmdSnapshotCreateRecursive, []string{"op-0123456789abcdef", "z1@nexus-x"}, "snapshot -o nexus:created-by=op-0123456789abcdef -r z1@nexus-x", "snapshot -o nexus:created-by=op-0123456789abcdef -r z1@nexus-x", p.Zfs, false},
	}
	type read struct {
		argv  []string
		named bool
	}
	var reads []read
	for _, tc := range cases {
		for _, f := range []Format{JSON, Text} {
			want := tc.text
			if f == JSON {
				want = tc.json
			}
			got := Argv(p, f, tc.cmd, tc.args...)
			if len(got) == 0 || got[0] != tc.tool || strings.Join(got[1:], " ") != want {
				t.Errorf("Argv(%v, %d, %q) = %q; want %s %s", f, tc.cmd, tc.args, got, tc.tool, want)
				continue
			}
			if n := len(tc.args); n > 0 && !slices.Equal(got[len(got)-1:], tc.args[n-1:]) {
				t.Errorf("%q: the name is not the last element", got)
			}
			for _, a := range got {
				if strings.Contains(a, "keylocation") || strings.Contains(a, ":") && !strings.Contains(a, spec.CreatedByProperty) {
					t.Errorf("%q: %q", got, a)
				}
			}
			if got[0] == p.Zfs && slices.Contains(got, "get") {
				t.Errorf("%q: zfs get is never run", got)
			}
			if tc.probed {
				reads = append(reads, read{got, len(tc.args) > 0})
			}
		}
	}
	// Every Command is in the table, and every probed read is there in both
	// formats, unnamed where Argv allows it and named where it takes a name.
	for cmd := CmdVersion; cmd <= CmdSnapshotCreateRecursive; cmd++ {
		if !slices.ContainsFunc(cases, func(x c) bool { return x.cmd == cmd }) {
			t.Errorf("Command %d is not in the table", cmd)
		}
		if cmd == CmdSnapshotGet || cmd >= CmdScrubStart {
			continue
		}
		for _, f := range []Format{JSON, Text} {
			for _, args := range [][]string{nil, {"z1"}} {
				if Argv(p, f, cmd, args...) == nil {
					continue
				}
				if !slices.ContainsFunc(cases, func(x c) bool { return x.cmd == cmd && x.probed && len(x.args) == len(args) }) {
					t.Errorf("read Command %d with %d names is built by Argv (format %d) but is not in the table", cmd, len(args), f)
				}
			}
		}
	}
	for _, bad := range [][]string{{"-z1"}, {""}, {"a", "b"}} {
		if got := Argv(p, JSON, CmdPoolList, bad...); got != nil {
			t.Errorf("Argv(CmdPoolList, %q) = %q; want nil", bad, got)
		}
	}
	if Argv(p, JSON, CmdPoolGet) != nil || Argv(p, JSON, CmdSnapshotCreate, "op-1", "z1") != nil || Argv(p, JSON, Command(999)) != nil {
		t.Error("a missing name, a snapshot name without @ or an unknown command built an argv")
	}

	t.Run("the probe match is exact", func(t *testing.T) {
		// probedArgvs and the comparison below accept an argv only when a
		// probe line ends with exactly that argv after the fixed-environment
		// prefix: no shape match, no prefix match, no match without the -o
		// columns, and a named read is not matched by its unnamed twin.
		const env = "x.json: -p NoNewPrivileges=yes /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C "
		probed := probedArgvs(env + "/usr/sbin/zpool list -j --json-int -o " + plc + " P1\n" +
			env + "/usr/sbin/zpool list -H -o name [noeol]\nstat-f-1.txt: -p X /usr/bin/stat -f /media/vzdump\n")
		for argv, want := range map[string]bool{
			"/usr/sbin/zpool list -j --json-int -o " + plc + " P1": true,
			"/usr/sbin/zpool list -H -o name":                      true,
			"/usr/sbin/zpool list -j --json-int -o " + plc:         false, // unnamed twin
			"/usr/sbin/zpool list -j --json-int P1":                false, // no -o columns
			"/usr/sbin/zpool list -j --json-int -o " + plc + " P2": false, // another name
			"/usr/sbin/zpool list -j --json-int":                   false,
			"/usr/sbin/zpool list -H -o":                           false, // a prefix of a line
			"/usr/bin/stat -f /media/vzdump":                       false, // not an Argv line
		} {
			if probed[argv] != want {
				t.Errorf("probe match of %q = %v; want %v", argv, probed[argv], want)
			}
		}
	})

	t.Run("every read argv ran under the sandbox probe", func(t *testing.T) {
		probe := capturegate.Require(t, filepath.Join(capRoot, "home-server", "sandbox-probe", "commands"))
		names := captureStdout(t, filepath.Join(capRoot, "home-server", "zfs", "zpool-list-names.txt"))
		p1, _, _ := strings.Cut(string(names), "\n")
		if p1 == "" {
			t.Fatal("the capture lists no pool")
		}
		probed := probedArgvs(string(readFile(t, probe)))
		for _, r := range reads {
			a := slices.Clone(r.argv)
			// WI 1.1's probe names the first pool for a pool read and its root
			// dataset for a dataset or snapshot read, so every name is P1 and
			// is the last element (or follows -d 1 or -r, the argv's own
			// order).
			if r.named {
				a[len(a)-1] = p1
			}
			if !probed[strings.Join(a, " ")] {
				t.Errorf("read argv %q is not, exactly, in %s", a, probe)
			}
		}
	})
}

// ---- TC-STG-73: fuzz targets, one per parser ----

// checkBounded fails on a returned string over the bound or an error path
// list over 50.
func checkBounded(t *testing.T, v any) {
	t.Helper()
	var walk func(r reflect.Value)
	walk = func(r reflect.Value) {
		switch r.Kind() {
		case reflect.String:
			if len(r.String()) > spec.StatusTextMaxBytes+len(TruncationMarker) {
				t.Fatalf("a returned string is %d bytes", len(r.String()))
			}
		case reflect.Pointer:
			if !r.IsNil() {
				walk(r.Elem())
			}
		case reflect.Struct:
			if r.Type() == reflect.TypeFor[time.Time]() {
				return
			}
			for i := range r.NumField() {
				walk(r.Field(i))
			}
		case reflect.Slice:
			for i := range r.Len() {
				walk(r.Index(i))
			}
		case reflect.Map:
			for _, k := range r.MapKeys() {
				walk(k)
				walk(r.MapIndex(k))
			}
		}
	}
	walk(reflect.ValueOf(v))
	if pools, ok := v.([]Pool); ok {
		for _, p := range pools {
			if len(p.ErrorPaths) > spec.MaxErrorPaths {
				t.Fatalf("%d error paths", len(p.ErrorPaths))
			}
		}
	}
}

type parseTarget struct {
	name  string
	json  bool
	parse func([]byte) (any, error)
	seeds []string // capture file names
}

func wrap[T any](f func([]byte) (T, error)) func([]byte) (any, error) {
	return func(b []byte) (any, error) { v, err := f(b); return v, err }
}

var parsers = []parseTarget{
	{"PoolListJSON", true, wrap(ParsePoolListJSON), []string{"zpool-list.json", "zpool-list.8.example3.json", "zpool-list-unknown-major.json"}},
	{"PoolListText", false, wrap(ParsePoolListText), []string{"zpool-list.txt"}},
	{"StatusJSON", true, wrap(ParseStatusJSON), []string{"zpool-status.json", "zpool-status-v.json", "zpool-status-*.json"}},
	{"StatusText", false, wrap(ParseStatusText), []string{"zpool-status-v.txt"}},
	{"PoolGetJSON", true, wrap(ParsePoolGetJSON), []string{"zpool-get-*.json"}},
	{"PoolGetText", false, wrap(ParsePoolGetText), []string{"zpool-get-*.txt"}},
	{"DatasetsJSON", true, wrap(ParseDatasetsJSON), []string{"zfs-list-datasets.json", "zfs-list.8.example2.json"}},
	{"DatasetsText", false, wrap(ParseDatasetsText), []string{"zfs-list-datasets.txt"}},
	{"SnapshotsJSON", true, wrap(ParseSnapshotsJSON), []string{"zfs-list-snapshots.json"}},
	{"SnapshotsText", false, wrap(ParseSnapshotsText), []string{"zfs-list-snapshots.txt", "zfs-list-snapshots-creation.txt"}},
}

// seeds returns every capture file a parser reads, plus the fixtures.
func (p parseTarget) seedInputs(t testing.TB) [][]byte {
	t.Helper()
	var out [][]byte
	for _, pat := range p.seeds {
		for _, dir := range []string{"*", filepath.Join("*", "zfs")} {
			files, _ := filepath.Glob(filepath.Join(capRoot, dir, pat))
			for _, f := range files {
				b, _ := captureVersion(readFile(t, f))
				out = append(out, b)
			}
		}
	}
	switch p.name {
	case "StatusJSON":
		out = append(out, mirrorWithClasses(t))
	case "StatusText":
		out = append(out, []byte(mirrorWithClassesText))
	case "PoolGetJSON":
		out = append(out, []byte(poolGetJSON))
	case "PoolGetText":
		out = append(out, []byte(poolGetText))
	case "DatasetsJSON":
		out = append(out, datasetsJSONFrom(t, datasetsText))
	case "DatasetsText":
		out = append(out, []byte(datasetsText))
	case "SnapshotsJSON":
		out = append(out, snapshotsJSONFrom(t, snapshotsText, false))
	case "SnapshotsText":
		out = append(out, []byte(snapshotsText))
	case "PoolListText":
		out = append(out, []byte("tank\t100\t10\t90\t10\t0\t1.00\tONLINE\t-\toff\n"))
	}
	return out
}

func TestTCSTG73_EveryParserSurvivesFuzzingAndKeepsIts(t *testing.T) {
	pinUTC(t)
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			seeds := p.seedInputs(t)
			if len(seeds) == 0 {
				t.Fatal("no seeds")
			}
			for i, s := range seeds {
				v, err := p.parse(s)
				if err == nil {
					checkBounded(t, v)
				}
				if !p.json || len(s) < 2 {
					continue
				}
				// A JSON document cut short is an error, never a partial
				// model.
				for _, cut := range []int{len(s) / 2, bytes.LastIndexByte(s, '}')} {
					if v, err := p.parse(s[:cut]); err == nil {
						t.Errorf("seed %d cut at %d: no error, model %v", i, cut, v)
					}
				}
			}
		})
	}
	t.Run("long names and texts are bounded", func(t *testing.T) {
		long := strings.Repeat("ü", 3000)
		pools, err := ParseStatusText([]byte(strings.ReplaceAll(mirrorWithClassesText, "sdl", long)))
		if err != nil {
			t.Fatal(err)
		}
		checkBounded(t, pools)
	})
}

func fuzzParser(f *testing.F, name string) {
	i := slices.IndexFunc(parsers, func(p parseTarget) bool { return p.name == name })
	p := parsers[i]
	for _, s := range p.seedInputs(f) {
		f.Add(s)
	}
	textLoc = time.UTC
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := p.parse(b)
		if err == nil {
			checkBounded(t, v)
		}
	})
}

func FuzzParsePoolListJSON(f *testing.F)  { fuzzParser(f, "PoolListJSON") }
func FuzzParsePoolListText(f *testing.F)  { fuzzParser(f, "PoolListText") }
func FuzzParseStatusJSON(f *testing.F)    { fuzzParser(f, "StatusJSON") }
func FuzzParseStatusText(f *testing.F)    { fuzzParser(f, "StatusText") }
func FuzzParsePoolGetJSON(f *testing.F)   { fuzzParser(f, "PoolGetJSON") }
func FuzzParsePoolGetText(f *testing.F)   { fuzzParser(f, "PoolGetText") }
func FuzzParseDatasetsJSON(f *testing.F)  { fuzzParser(f, "DatasetsJSON") }
func FuzzParseDatasetsText(f *testing.F)  { fuzzParser(f, "DatasetsText") }
func FuzzParseSnapshotsJSON(f *testing.F) { fuzzParser(f, "SnapshotsJSON") }
func FuzzParseSnapshotsText(f *testing.F) { fuzzParser(f, "SnapshotsText") }

// TC-STG-64 (code review CR-4): a later section of print_scan_status never
// replaces the pool's scrub. Source vector: mirrorWithClassesText above, whose
// one scan line is replaced by the lines print_scan_status prints: the scan
// line of print_scan_scrub_resilver_status (L8937-L9029), the error scrub's
// " scrub:" line of print_err_scrub_status (L8873-L8935) and the rebuild line
// of print_rebuild_status_impl ("resilvered (%s) %s in %s with %llu errors on
// %s", L9081+), all in cmd/zpool/zpool_main.c at tag zfs-2.4.1; print_scan_status
// is L10086-L10135.
func TestTCSTG64_ALaterScanSectionNeverReplacesTheScrub(t *testing.T) {
	pinUTC(t)
	const scrub = "  scan: scrub repaired 0B in 03:00:17 with 0 errors on Sun Nov 15 05:00:17 2026\n"
	const errScrub = " scrub: scrubbed 3 error blocks in 0 days 00:00:05 on Sun Nov 15 06:00:00 2026\n"
	const rebuild = "  scan: resilvered (mirror-0) 1.00G in 00:00:05 with 0 errors on Sun Nov 15 07:00:00 2026\n"
	const resilver = "  scan: resilvered 1.00G in 00:00:05 with 0 errors on Sun Nov 15 07:00:00 2026\n"
	cases := []struct {
		name, scan, fn, state string
	}{
		{"scrub then a past rebuild", scrub + rebuild, ScanScrub, ScanFinished},
		{"error scrub then a past rebuild", errScrub + rebuild, ScanErrorScrub, ScanFinished},
		{"error scrub then a resilver", errScrub + resilver, ScanResilver, ScanFinished},
		{"a rebuild alone", rebuild, ScanUnknown, ScanUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(mirrorWithClassesText, scrub, c.scan, 1)
			pools, err := ParseStatusText([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			if s := pools[0].Scan; s.Function != c.fn || s.State != c.state {
				t.Errorf("scan = %s/%s, want %s/%s", s.Function, s.State, c.fn, c.state)
			}
		})
	}
}
