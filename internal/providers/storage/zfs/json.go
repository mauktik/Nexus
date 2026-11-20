package zfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KnownMajor is the output_version.vers_major OpenZFS 2.3 and 2.4 print for
// every JSON command (zpool_json_schema(0, 1) in cmd/zpool/zpool_main.c, the
// same in cmd/zfs/zfs_main.c). A minor above the known one parses normally.
const KnownMajor = 0

// ErrUnknownVersion is returned by a JSON parser whose document reports an
// output_version.vers_major other than KnownMajor; the provider then reads
// the text form of the same command (D-5, ASM-2).
var ErrUnknownVersion = errors.New("zfs: unknown JSON output_version major")

// maxVdevDepth bounds the vdev tree's nesting (real trees are 3 or 4 deep).
const maxVdevDepth = 32

// member is one key and value of a JSON object, in document order: the tools
// emit nvlists in insertion order, which is zpool status's tree order, and a
// Go map would lose it.
type member struct {
	key string
	val json.RawMessage
}

type object []member

func (o object) get(k string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == k {
			return m.val, true
		}
	}
	return nil, false
}

// str returns a string member; a number member is its decimal text, the
// word form -p prints for it.
func (o object) str(k string) (string, bool) {
	raw, ok := o.get(k)
	if !ok {
		return "", false
	}
	return word(raw)
}

// uint returns an integer member given as a JSON number (--json-int) or as a
// decimal string (the string form).
func (o object) uint(k string) (uint64, bool) {
	w, ok := o.str(k)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(w, 10, 64)
	return n, err == nil
}

func (o object) uintPtr(k string) *uint64 {
	if n, ok := o.uint(k); ok {
		return &n
	}
	return nil
}

// word returns a JSON string, or a JSON number's literal text.
func word(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return string(raw), true
	}
	return "", false
}

// parseObject decodes one JSON object's members in order. The caller has
// checked the whole document with json.Valid.
func parseObject(raw json.RawMessage) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := t.(string)
		if !ok {
			return nil, errors.New("JSON object key is not a string")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, member{k, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

// document checks a JSON document's size, validity and output_version, and
// returns the members of its container object (pools or datasets). A
// document without the container lists nothing.
func document(b []byte, command, container string) (object, error) {
	if len(b) > StdoutCap {
		return nil, fmt.Errorf("zfs: %s JSON is %d bytes, over %d", command, len(b), StdoutCap)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("zfs: %s output is not a complete JSON document", command)
	}
	doc, err := parseObject(b)
	if err != nil {
		return nil, fmt.Errorf("zfs: %s JSON: %v", command, err)
	}
	ovRaw, ok := doc.get("output_version")
	if !ok {
		return nil, fmt.Errorf("zfs: %s JSON has no output_version", command)
	}
	ov, err := parseObject(ovRaw)
	if err != nil {
		return nil, fmt.Errorf("zfs: %s JSON output_version: %v", command, err)
	}
	if c, _ := ov.str("command"); c != command {
		return nil, fmt.Errorf("zfs: JSON output_version.command is %q, not %q", boundText(c), command)
	}
	major, ok := ov.uint("vers_major")
	if !ok {
		return nil, fmt.Errorf("zfs: %s JSON output_version has no vers_major", command)
	}
	if major != KnownMajor {
		return nil, fmt.Errorf("%w: %s reports %d, this nexusd reads %d", ErrUnknownVersion, command, major, KnownMajor)
	}
	cRaw, ok := doc.get(container)
	if !ok {
		return nil, nil
	}
	items, err := parseObject(cRaw)
	if err != nil {
		return nil, fmt.Errorf("zfs: %s JSON %s: %v", command, container, err)
	}
	return items, nil
}

// properties returns an item's properties as word getters: each property is
// {"value": …, "source": …}.
func properties(item object, command string) (map[string]string, error) {
	raw, ok := item.get("properties")
	if !ok {
		return map[string]string{}, nil
	}
	props, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("zfs: %s JSON properties: %v", command, err)
	}
	m := make(map[string]string, len(props))
	for _, p := range props {
		po, err := parseObject(p.val)
		if err != nil {
			return nil, fmt.Errorf("zfs: %s JSON property %q: %v", command, boundText(p.key), err)
		}
		v, ok := po.str("value")
		if !ok {
			return nil, fmt.Errorf("zfs: %s JSON property %q has no value", command, boundText(p.key))
		}
		m[p.key] = v
	}
	return m, nil
}

// jsonColumns maps a -o column to the property name the JSON form keys it by
// (zfs_prop_to_name gives the full names).
var jsonColumns = map[string]string{"avail": "available", "refer": "referenced"}

func propGetter(m map[string]string) getter {
	return func(col string) (string, bool) {
		if p, ok := jsonColumns[col]; ok {
			col = p
		}
		v, ok := m[col]
		return v, ok
	}
}

func items(b []byte, command, container string, each func(key string, item object) error) error {
	doc, err := document(b, command, container)
	if err != nil {
		return err
	}
	for _, m := range doc {
		item, err := parseObject(m.val)
		if err != nil {
			return fmt.Errorf("zfs: %s JSON %q: %v", command, boundText(m.key), err)
		}
		if err := each(m.key, item); err != nil {
			return err
		}
	}
	return nil
}

func itemName(key string, item object) string {
	if n, ok := item.str("name"); ok && n != "" {
		return n
	}
	return key
}

// ParsePoolListJSON reads `zpool list -j --json-int -o PoolListColumns [NAME]`
// (A-58: without -o the tool prints its default columns, which lack readonly,
// so Argv always requests the columns). A document without a column, such as
// the upstream man-page example taken without -o, leaves that field nil with
// a reason naming zpool list.
func ParsePoolListJSON(b []byte) ([]Pool, error) {
	var pools []Pool
	err := items(b, "zpool list", "pools", func(key string, item object) error {
		props, err := properties(item, "zpool list")
		if err != nil {
			return err
		}
		p := Pool{Name: boundText(itemName(key, item))}
		state, _ := item.str("state")
		fillPoolList(&p, propGetter(props), state)
		pools = append(pools, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pools, nil
}

// ParsePoolGetJSON reads `zpool get -j --json-int all NAME`: the property
// name to value map of the one pool it names.
func ParsePoolGetJSON(b []byte) (map[string]string, error) {
	var out map[string]string
	err := items(b, "zpool get", "pools", func(key string, item object) error {
		if out != nil {
			return errors.New("zfs: zpool get JSON lists more than one pool")
		}
		props, err := properties(item, "zpool get")
		if err != nil {
			return err
		}
		out = make(map[string]string, len(props))
		for k, v := range props {
			out[boundText(k)] = boundText(v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("zfs: zpool get JSON lists no pool")
	}
	return out, nil
}

// ParseDatasetsJSON reads `zfs list -j --json-int -t filesystem,volume -o
// DatasetColumns [NAME]`.
func ParseDatasetsJSON(b []byte) ([]Dataset, error) {
	var out []Dataset
	err := items(b, "zfs list", "datasets", func(key string, item object) error {
		props, err := properties(item, "zfs list")
		if err != nil {
			return err
		}
		name := itemName(key, item)
		d := Dataset{Name: boundText(name), Pool: boundText(poolOf(name))}
		t, _ := item.str("type")
		fillDataset(&d, propGetter(props), t)
		out = append(out, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ParseSnapshotsJSON reads `zfs list -j --json-int -t snapshot -o
// SnapshotColumns`. The provider lists snapshots in the text form (D-5
// exception); this parser is the parity oracle of ParseSnapshotsText.
func ParseSnapshotsJSON(b []byte) ([]Snapshot, error) {
	var out []Snapshot
	err := items(b, "zfs list", "datasets", func(key string, item object) error {
		props, err := properties(item, "zfs list")
		if err != nil {
			return err
		}
		var s Snapshot
		if err := fillSnapshot(&s, itemName(key, item), propGetter(props), true); err != nil {
			return err
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func poolOf(dataset string) string {
	p, _, _ := strings.Cut(dataset, "/")
	return p
}

// ParseStatusJSON reads `zpool status -j --json-int [-v] [NAME]`.
func ParseStatusJSON(b []byte) ([]Pool, error) {
	var pools []Pool
	err := items(b, "zpool status", "pools", func(key string, item object) error {
		p, err := statusPool(key, item)
		if err != nil {
			return err
		}
		pools = append(pools, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pools, nil
}

func statusPool(key string, item object) (Pool, error) {
	p := Pool{Name: boundText(itemName(key, item))}
	r := reasons{&p.Unavailable, "zpool status"}
	state, ok := item.str("state")
	if !ok {
		r.absent("health")
	}
	p.RawHealth, p.Health = boundText(state), normalizeHealth(state)
	if s, ok := item.str("status"); ok {
		p.Status = ptr(boundText(strings.TrimRight(s, "\n")))
	}
	if a, ok := item.str("action"); ok {
		p.Action = ptr(boundText(strings.TrimRight(a, "\n")))
	}

	p.Scan = &Scan{Function: ScanNone, State: ScanNone}
	if raw, ok := item.get("scan_stats"); ok {
		s, err := scanJSON(raw)
		if err != nil {
			return Pool{}, err
		}
		p.Scan = s
	}

	// The root vdev tree, then the class groups, in the tool's order.
	rootName := p.Name
	sawVdevs := false
	for _, m := range item {
		class := ""
		switch m.key {
		case "vdevs":
			sawVdevs = true
		case "dedup", "special", "logs", "l2cache", "spares":
			class = classWords[m.key]
		default:
			continue
		}
		group, err := parseObject(m.val)
		if err != nil {
			return Pool{}, fmt.Errorf("zfs: zpool status JSON %q %s: %v", p.Name, m.key, err)
		}
		for _, g := range group {
			depth, parent := 0, ""
			if class != "" {
				depth, parent = 1, rootName
			}
			if err := walkJSON(&p, g, depth, parent, class, state); err != nil {
				return Pool{}, err
			}
			if class == "" && len(p.Vdevs) > 0 {
				rootName = p.Vdevs[0].Name
			}
		}
	}
	if !sawVdevs {
		r.absent("vdevs")
		r.absent("vdevCount")
		r.absent("errors")
	}

	if n, ok := item.uint("error_count"); ok {
		p.ErrorCount = &n
	} else {
		r.absent("permanentErrors")
	}
	if raw, ok := item.get("errlist"); ok {
		var paths []string
		if json.Unmarshal(raw, &paths) == nil {
			keepPaths(&p, paths)
		} else if msg, ok := word(raw); ok {
			// errors_nvlist adds strerror(errno) when the error log could
			// not be read.
			unavailable(&p.Unavailable, "permanentErrorPaths", boundText("zpool status: "+msg))
		} else {
			return Pool{}, fmt.Errorf("zfs: zpool status JSON %q errlist is neither a list nor a message", p.Name)
		}
	}
	finishPool(&p)
	return p, nil
}

// walkJSON appends one vdev and its children in tree order.
func walkJSON(p *Pool, m member, depth int, parent, class, poolState string) error {
	if depth > maxVdevDepth {
		return fmt.Errorf("zfs: zpool status JSON %q vdev tree is deeper than %d", p.Name, maxVdevDepth)
	}
	v, err := parseObject(m.val)
	if err != nil {
		return fmt.Errorf("zfs: zpool status JSON %q vdev %q: %v", p.Name, boundText(m.key), err)
	}
	name := itemName(m.key, v)
	var children object
	if raw, ok := v.get("vdevs"); ok {
		if children, err = parseObject(raw); err != nil {
			return fmt.Errorf("zfs: zpool status JSON %q vdev %q: %v", p.Name, boundText(name), err)
		}
	}
	raw, _ := v.str("vdev_type")
	state, ok := v.str("state")
	aux, _ := v.str("aux")
	vd := Vdev{
		Name:     boundText(name),
		Type:     vdevType(raw, name, class, depth, len(children) == 0),
		State:    vdevStateJSON(state, aux),
		Depth:    depth,
		Parent:   boundText(parent),
		Read:     v.uintPtr("read_errors"),
		Write:    v.uintPtr("write_errors"),
		Checksum: v.uintPtr("checksum_errors"),
	}
	if !ok && depth == 0 {
		// The root entry may carry no state of its own; the pool's state
		// word stands for it.
		vd.State = vdevStateWord(poolState)
	}
	p.Vdevs = append(p.Vdevs, vd)
	for _, c := range children {
		if err := walkJSON(p, c, depth+1, name, "", poolState); err != nil {
			return err
		}
	}
	return nil
}

func scanJSON(raw json.RawMessage) (*Scan, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("zfs: zpool status JSON scan_stats: %v", err)
	}
	fn, _ := o.str("function")
	st, _ := o.str("state")
	s := &Scan{Function: scanWord(scanFunctions, fn), State: scanWord(scanStates, st)}
	// A paused scrub is SCANNING with a pause time (pss_pass_scrub_pause).
	if pause, _ := o.uint("scrub_pause"); pause != 0 && s.State == ScanScanning {
		s.State = ScanPaused
	}
	if n, ok := o.uint("start_time"); ok {
		s.Start = unixPtr(n)
	}
	if n, ok := o.uint("end_time"); ok {
		s.End = unixPtr(n)
	}
	s.Examined = o.uintPtr("examined")
	s.ToExamine = o.uintPtr("to_examine")
	s.Processed = o.uintPtr("processed")
	s.Errors = o.uintPtr("errors")
	return s, nil
}
