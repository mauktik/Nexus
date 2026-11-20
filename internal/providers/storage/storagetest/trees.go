package storagetest

// F-TREE (test-strategy.md): builders that plant real directories, files,
// symlinks, modes and mtimes in a temp directory, from a table or from a
// captured `ls -lan --time-style=full-iso` listing. The vzdump and
// timemachine internal tests use them, so this file imports neither package.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// NodeType is the kind of a planted Node.
type NodeType int

const (
	// File is a regular file (the zero value).
	File NodeType = iota
	// Dir is a directory.
	Dir
	// Symlink is a symbolic link holding Target verbatim.
	Symlink
)

// Node is one entry of a fixture tree. Parent directories are created as
// needed.
type Node struct {
	// Path is slash-separated and relative to the tree's directory.
	Path string
	Type NodeType
	// Content is a File's bytes. When it is nil the file is sparse, Size
	// bytes long (Truncate), so a multi-gigabyte archive costs no disk.
	Content []byte
	Size    int64
	// Target is a Symlink's link text, written as is: it may be absolute
	// or point outside the tree.
	Target string
	// ModTime, when not zero, is set with os.Chtimes after every node is
	// planted, so a directory keeps its time after its children are made.
	// A symlink's own time is not set.
	ModTime time.Time
	// Mode's permission bits are applied last when SetMode is true (so
	// mode 000 can be asked for). A directory's mode is set back to 0755 at
	// cleanup so t.TempDir can remove it.
	Mode    fs.FileMode
	SetMode bool
}

// TempDir is t.TempDir with its symlinks resolved (darwin's /var is a
// symlink to /private/var), so os.Root and the paths a test compares agree.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// Build plants nodes under dir, in order, failing the test on any error.
func Build(t testing.TB, dir string, nodes ...Node) {
	t.Helper()
	for _, n := range nodes {
		p := filepath.Join(dir, filepath.FromSlash(n.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("plant %s: %v", n.Path, err)
		}
		if err := plant(p, n); err != nil {
			t.Fatalf("plant %s: %v", n.Path, err)
		}
	}
	// Times after every node is planted: planting a child changes its
	// directory's mtime, Chtimes does not.
	for _, n := range nodes {
		if n.ModTime.IsZero() || n.Type == Symlink {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(n.Path))
		if err := os.Chtimes(p, n.ModTime, n.ModTime); err != nil {
			t.Fatalf("chtimes %s: %v", n.Path, err)
		}
	}
	for _, n := range slices.Backward(nodes) {
		if !n.SetMode || n.Type == Symlink {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(n.Path))
		if err := os.Chmod(p, n.Mode.Perm()); err != nil {
			t.Fatalf("chmod %s: %v", n.Path, err)
		}
		if n.Type == Dir {
			t.Cleanup(func() {
				if err := os.Chmod(p, 0o755); err != nil {
					t.Errorf("restore mode of %s: %v", p, err)
				}
			})
		}
	}
}

func plant(p string, n Node) error {
	switch n.Type {
	case Dir:
		return os.MkdirAll(p, 0o755)
	case Symlink:
		return os.Symlink(n.Target, p)
	case File:
		if n.Content != nil {
			return os.WriteFile(p, n.Content, 0o644)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if err := f.Truncate(n.Size); err != nil {
			return errors.Join(err, f.Close())
		}
		return f.Close()
	default:
		return fmt.Errorf("unknown node type %d", n.Type)
	}
}

// CaptureStdout returns a host capture file's stdout: the file without its
// last line, the `Exit <code>` line capture.sh appends (testdata/README.md,
// "The Exit and .stderr conventions"), and the exit code. The one newline
// capture.sh adds to a `[noeol]` command is kept: a caller that needs the
// exact bytes reads PROVENANCE.
func CaptureStdout(path string) ([]byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	body := bytes.TrimSuffix(b, []byte("\n"))
	i := bytes.LastIndexByte(body, '\n')
	last := body[i+1:]
	code, ok := bytes.CutPrefix(last, []byte("Exit "))
	if !ok {
		return nil, 0, fmt.Errorf("%s: last line %q is not an Exit line", path, last)
	}
	exit, err := strconv.Atoi(string(code))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: exit code %q: %w", path, code, err)
	}
	return b[:i+1], exit, nil
}

// Listed is one entry of a captured long listing.
type Listed struct {
	Name string
	// Type is the listing's type character: '-', 'd', 'l', 'p', 's', 'c'
	// or 'b'.
	Type    byte
	Size    int64
	ModTime time.Time
	// Target is a symlink's link text (after " -> ").
	Target string
}

// listingLine is one `ls -lan --time-style=full-iso` (or `--full-time`)
// entry: mode, links, uid, gid, size, date, time with fraction, zone, name.
var listingLine = regexp.MustCompile(
	`^([-dlpscb])[-rwxsStT]{9}[.+@]?\s+\d+\s+\d+\s+\d+\s+(\d+)\s+` +
		`(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) ([+-]\d{4}) (.+)$`)

// ParseListing parses the stdout of `ls -lan --time-style=full-iso DIR`.
// The `total` line, `.` and `..` are dropped. A line in another form is an
// error naming it, so a capture taken in a form this parser does not know
// fails the test rather than silently losing entries.
func ParseListing(stdout []byte) ([]Listed, error) {
	var out []Listed
	for line := range strings.Lines(string(stdout)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		m := listingLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("listing line not in ls -lan --time-style=full-iso form: %q", line)
		}
		size, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("listing line %q: size: %w", line, err)
		}
		mt, err := time.Parse("2006-01-02 15:04:05.999999999 -0700", m[3]+" "+m[4])
		if err != nil {
			return nil, fmt.Errorf("listing line %q: time: %w", line, err)
		}
		e := Listed{Name: m[5], Type: m[1][0], Size: size, ModTime: mt}
		if e.Type == 'l' {
			name, target, ok := strings.Cut(e.Name, " -> ")
			if !ok {
				return nil, fmt.Errorf("listing line %q: symlink without a target", line)
			}
			e.Name, e.Target = name, target
		}
		if e.Name == "." || e.Name == ".." {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// ListingNodes turns listed entries into nodes under prefix ("" for the
// tree's top): files sparse at their size, directories, and symlinks with
// their captured link text, each with its mtime. An entry of another type
// (a FIFO, socket or device) is an error: the tree could not show it to
// the reader the way the host did.
func ListingNodes(entries []Listed, prefix string) ([]Node, error) {
	nodes := make([]Node, 0, len(entries))
	for _, e := range entries {
		n := Node{Path: e.Name, ModTime: e.ModTime}
		if prefix != "" {
			n.Path = prefix + "/" + e.Name
		}
		switch e.Type {
		case '-':
			n.Type, n.Size = File, e.Size
		case 'd':
			n.Type = Dir
		case 'l':
			n.Type, n.Target, n.ModTime = Symlink, e.Target, time.Time{}
		default:
			return nil, fmt.Errorf("listed %q has type %q, which a fixture tree cannot plant", e.Name, e.Type)
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// Time Machine fixtures (TC-STG-100 to TC-STG-109). A network Time Machine
// destination is a `<name>.sparsebundle` (or `.backupbundle`) directory
// holding Info.plist, its copy Info.bckup, an empty token, a zero-byte
// lock, the bands/ directory of band files named in hex, and, written by
// backupd, com.apple.TimeMachine.SnapshotHistory.plist.

// BinaryPlistMagic is the header of a binary property list.
const BinaryPlistMagic = "bplist00"

// SnapshotHistoryXML is an XML com.apple.TimeMachine.SnapshotHistory.plist
// holding one snapshot dict per date, in the order given, each nested in
// the Snapshots array the way backupd writes it.
func SnapshotHistoryXML(dates ...time.Time) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n\t<key>Snapshots</key>\n\t<array>\n")
	for i, d := range dates {
		fmt.Fprintf(&b, "\t\t<dict>\n\t\t\t<key>com.apple.backupd.SnapshotCompletionDate</key>\n\t\t\t<date>%s</date>\n", d.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "\t\t\t<key>com.apple.backupd.SnapshotName</key>\n\t\t\t<string>snapshot-%d.backup</string>\n\t\t</dict>\n", i)
	}
	b.WriteString("\t</array>\n</dict>\n</plist>\n")
	return []byte(b.String())
}

// BundleNodes is the at-rest layout of a sparsebundle directory named dir
// (relative to the tree): Info.plist, Info.bckup, token, a zero-byte lock
// and an empty bands/. Callers add band files and plists beside them.
func BundleNodes(dir string) []Node {
	info := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		"<plist version=\"1.0\">\n<dict>\n\t<key>band-size</key>\n\t<integer>8388608</integer>\n</dict>\n</plist>\n")
	return []Node{
		{Path: dir, Type: Dir},
		{Path: dir + "/Info.plist", Content: info},
		{Path: dir + "/Info.bckup", Content: info},
		{Path: dir + "/token", Content: []byte{}},
		{Path: dir + "/lock", Content: []byte{}},
		{Path: dir + "/bands", Type: Dir},
	}
}

// BandNodes is n band files bands/0 … bands/<n-1 in hex> under the bundle
// directory dir, each holding content.
func BandNodes(dir string, n int, content []byte) []Node {
	nodes := make([]Node, 0, n)
	for i := range n {
		nodes = append(nodes, Node{Path: fmt.Sprintf("%s/bands/%x", dir, i), Content: content})
	}
	return nodes
}
