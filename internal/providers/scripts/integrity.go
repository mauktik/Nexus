package scripts

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/mauktik/nexus/internal/problem"
)

// Integrity check names: the check member of script-integrity-failed.
const (
	CheckRunAsUser              = "run-as-user"
	CheckPathResolution         = "path-resolution"
	CheckScriptExists           = "script-exists"
	CheckScriptRegularFile      = "script-regular-file"
	CheckScriptExecutable       = "script-executable"
	CheckScriptOwner            = "script-owner"
	CheckScriptWritable         = "script-writable"
	CheckDirectoryOwner         = "directory-owner"
	CheckDirectoryWorldWritable = "directory-world-writable"
	CheckDirectoryGroupWritable = "directory-group-writable"
	CheckGroupDatabase          = "group-database"
)

// Host account databases that decide whether a group is private (A-59).
const (
	groupFile  = "/etc/group"
	passwdFile = "/etc/passwd"
)

// maxLinks bounds symlink resolution, as the kernel's ELOOP does.
const maxLinks = 255

// account is a resolved run-as user.
type account struct {
	name string
	uid  uint32
	gid  uint32 // Primary group
}

// integrity is the FR-20 check. Its fields are the host seams: the run-as
// lookup, and the reader of the account databases. The filesystem is always
// the real one; ownership by another user cannot be staged without root, so
// T0 tests point lookupUser at a different UID instead.
type integrity struct {
	lookupUser func(name string) (account, error)
	readFile   func(path string) ([]byte, error)
}

// hostIntegrity checks against this host's accounts.
var hostIntegrity = integrity{lookupUser: lookupAccount, readFile: os.ReadFile}

func lookupAccount(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return account{}, fmt.Errorf("user %s has UID %q: %w", name, u.Uid, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return account{}, fmt.Errorf("user %s has GID %q: %w", name, u.Gid, err)
	}
	return account{name: name, uid: uint32(uid), gid: uint32(gid)}, nil
}

// check verifies the script at path may run as runAs (FR-20), and returns
// its resolved path. The script must exist, be a regular file executable by
// runAs, be owned by root or runAs, and be neither group- nor
// world-writable. Every ancestor of the resolved file, up to /, and every
// directory holding a symlink that resolution passed through, must be owned
// by root or runAs and must not be world-writable, sticky or not. A
// group-writable directory passes only when its group is private: every
// member, listed in /etc/group or holding it as primary group in
// /etc/passwd, is root or runAs (A-59).
//
// Each component is examined with lstat after resolution, so a symlink
// swapped in during the check fails it rather than being followed. The
// caller executes the returned path, never re-resolving the catalog path, so
// a link repointed after the check cannot redirect that run.
func (c integrity) check(path, runAs string) (string, error) {
	acct, err := c.lookupUser(runAs)
	if err != nil {
		return "", refuse(CheckRunAsUser, path, fmt.Sprintf("the run-as user %s cannot be resolved: %v. "+
			"Fix: create the user, or correct the catalog's runAs", runAs, err))
	}

	resolved, linkDirs, err := walkLinks(path)
	if err != nil {
		return "", resolutionFailure(path, err)
	}
	eval, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", resolutionFailure(path, err)
	}
	if eval != resolved {
		return "", refuse(CheckPathResolution, path, fmt.Sprintf("%s resolved to both %s and %s: "+
			"a link changed during the check", path, resolved, eval))
	}

	r := run{integrity: c, acct: acct}
	if err := r.file(resolved); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, start := range append([]string{filepath.Dir(resolved)}, linkDirs...) {
		for dir := start; !seen[dir]; dir = filepath.Dir(dir) {
			seen[dir] = true
			if err := r.directory(dir); err != nil {
				return "", err
			}
		}
	}
	return resolved, nil
}

// walkLinks resolves path one component at a time, as the kernel does, and
// returns the result and every directory that held a symlink on the way: a
// writer of such a directory could repoint the link.
func walkLinks(path string) (string, []string, error) {
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("%s is not absolute", path)
	}
	var linkDirs []string
	cur, rest, links := "/", splitPath(path), 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", nil, err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > maxLinks {
			return "", nil, fmt.Errorf("more than %d links resolving %s: %w", maxLinks, path, syscall.ELOOP)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", nil, err
		}
		if !slices.Contains(linkDirs, cur) {
			linkDirs = append(linkDirs, cur)
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		rest = append(splitPath(target), rest...)
	}
	return cur, linkDirs, nil
}

func splitPath(p string) []string {
	return slices.DeleteFunc(strings.Split(p, "/"), func(s string) bool { return s == "" })
}

func resolutionFailure(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return refuse(CheckScriptExists, path, fmt.Sprintf("%s does not exist: %v. "+
			"Fix: install the script there, or correct the catalog's path", path, err))
	}
	return refuse(CheckPathResolution, path, fmt.Sprintf("%s cannot be resolved: %v", path, err))
}

// run is one check against one run-as user. The account databases are read
// at most once, and only when a group must be judged.
type run struct {
	integrity
	acct   account
	db     *accountDB
	dbErr  error
	dbRead bool
}

func (r *run) file(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return resolutionFailure(p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return changed(p)
	}
	if !fi.Mode().IsRegular() {
		return refuse(CheckScriptRegularFile, p, fmt.Sprintf("%s is not a regular file (%s). "+
			"Fix: point the catalog's path at the script file", p, fi.Mode()))
	}
	uid, gid := owner(fi)
	perm := fi.Mode().Perm()

	ok, err := r.executable(perm, uid, gid)
	if err != nil {
		return err
	}
	if !ok {
		fix := "chmod a+x " + quote(p)
		if uid == r.acct.uid {
			fix = "chmod u+x " + quote(p)
		}
		return refuse(CheckScriptExecutable, p, fmt.Sprintf("%s (%s) is not executable by %s. Fix: %s",
			p, fi.Mode(), r.acct.name, fix))
	}
	if uid != 0 && uid != r.acct.uid {
		return refuse(CheckScriptOwner, p, fmt.Sprintf("%s is owned by UID %d, not root or %s. Fix: chown root %s",
			p, uid, r.acct.name, quote(p)))
	}
	if perm&0o022 != 0 {
		return refuse(CheckScriptWritable, p, fmt.Sprintf("%s (%s) is %s. Fix: chmod %s %s",
			p, fi.Mode(), writableBy(perm&0o022), removeWrite(perm&0o022), quote(p)))
	}
	return nil
}

// executable reports whether the run-as user may execute a file with perm,
// owned by uid and gid: the owner, group or other class applies, as the
// kernel decides it.
func (r *run) executable(perm fs.FileMode, uid, gid uint32) (bool, error) {
	switch {
	case r.acct.uid == 0:
		return perm&0o111 != 0, nil
	case uid == r.acct.uid:
		return perm&0o100 != 0, nil
	case gid == r.acct.gid:
		return perm&0o010 != 0, nil
	}
	db, err := r.accounts()
	if err != nil {
		return false, err
	}
	if slices.Contains(db.groups[gid].members, r.acct.name) {
		return perm&0o010 != 0, nil
	}
	return perm&0o001 != 0, nil
}

func (r *run) directory(d string) error {
	fi, err := os.Lstat(d)
	if err != nil {
		return resolutionFailure(d, err)
	}
	if !fi.IsDir() {
		return changed(d)
	}
	uid, gid := owner(fi)
	if uid != 0 && uid != r.acct.uid {
		return refuse(CheckDirectoryOwner, d, fmt.Sprintf("directory %s is owned by UID %d, not root or %s. "+
			"Fix: chown root %s", d, uid, r.acct.name, quote(d)))
	}
	perm := fi.Mode().Perm()
	var others []string
	groupShared := false
	if perm&0o020 != 0 {
		db, err := r.accounts()
		if err != nil {
			return err
		}
		var known bool
		others, known = db.otherMembers(gid, r.acct)
		groupShared = !known || len(others) > 0
	}
	if perm&0o002 != 0 {
		bits := perm & 0o002
		if groupShared {
			bits |= 0o020
		}
		sticky := ""
		if fi.Mode()&fs.ModeSticky != 0 {
			sticky = "; the sticky bit does not exempt it"
		}
		return refuse(CheckDirectoryWorldWritable, d, fmt.Sprintf("directory %s (%s) is world-writable%s. Fix: chmod %s %s",
			d, fi.Mode(), sticky, removeWrite(bits), quote(d)))
	}
	if groupShared {
		who := "it is absent from " + groupFile
		if len(others) > 0 {
			who = "it includes " + strings.Join(others, ", ")
		}
		return refuse(CheckDirectoryGroupWritable, d, fmt.Sprintf("directory %s (%s) is writable by group %d, "+
			"and %s, not only root or %s. Fix: chmod g-w %s", d, fi.Mode(), gid, who, r.acct.name, quote(d)))
	}
	return nil
}

func (r *run) accounts() (*accountDB, error) {
	if !r.dbRead {
		r.dbRead = true
		r.db, r.dbErr = r.loadAccounts()
	}
	if r.dbErr != nil {
		return nil, refuse(CheckGroupDatabase, groupFile, fmt.Sprintf("group membership cannot be read: %v", r.dbErr))
	}
	return r.db, nil
}

func owner(fi fs.FileInfo) (uid, gid uint32) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Not reachable on Linux or macOS. An impossible owner fails closed.
		return ^uint32(0), ^uint32(0)
	}
	return st.Uid, st.Gid
}

func changed(p string) error {
	return refuse(CheckPathResolution, p, fmt.Sprintf("%s changed during the check: it is now a symlink "+
		"or of another type", p))
}

func refuse(check, path, detail string) error {
	return problem.New(problem.ScriptIntegrityFailed, detail).With("check", check).With("path", path)
}

func writableBy(bits fs.FileMode) string {
	switch bits {
	case 0o020:
		return "group-writable"
	case 0o002:
		return "world-writable"
	default:
		return "group- and world-writable"
	}
}

// removeWrite is the chmod mode that clears exactly bits.
func removeWrite(bits fs.FileMode) string {
	var m []string
	if bits&0o020 != 0 {
		m = append(m, "g-w")
	}
	if bits&0o002 != 0 {
		m = append(m, "o-w")
	}
	return strings.Join(m, ",")
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9_./+@%:=-]+$`)

// quote makes p a single shell word, so the stated fix can be pasted.
func quote(p string) string {
	if plainPath.MatchString(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// accountDB is the part of /etc/group and /etc/passwd that decides group
// membership.
type accountDB struct {
	groups map[uint32]group
	uids   map[string]uint32 // User name -> UID, from passwd
}

type group struct {
	members []string // Listed in /etc/group, plus passwd users whose primary group it is
}

func (c integrity) loadAccounts() (*accountDB, error) {
	gb, err := c.readFile(groupFile)
	if err != nil {
		return nil, err
	}
	pb, err := c.readFile(passwdFile)
	if err != nil {
		return nil, err
	}
	groups, err := records(gb, 4)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", groupFile, err)
	}
	users, err := records(pb, 7)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", passwdFile, err)
	}
	db := &accountDB{groups: map[uint32]group{}, uids: map[string]uint32{}}
	// Two group lines can share a GID; their members add up.
	for _, f := range groups {
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		g := db.groups[uint32(gid)]
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				g.members = append(g.members, m)
			}
		}
		db.groups[uint32(gid)] = g
	}
	for _, f := range users {
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		db.uids[f[0]] = uint32(uid)
		// A primary group missing from /etc/group stays absent: it is
		// unknown, so it counts as shared.
		if g, ok := db.groups[uint32(gid)]; ok {
			g.members = append(g.members, f[0])
			db.groups[uint32(gid)] = g
		}
	}
	return db, nil
}

// otherMembers returns the members of gid other than root and acct, and
// whether the group is listed at all.
func (db *accountDB) otherMembers(gid uint32, acct account) ([]string, bool) {
	g, ok := db.groups[gid]
	if !ok {
		return nil, false
	}
	var others []string
	for _, m := range g.members {
		if m == "root" || m == acct.name || slices.Contains(others, m) {
			continue
		}
		if uid, ok := db.uids[m]; ok && (uid == 0 || uid == acct.uid) {
			continue
		}
		others = append(others, m)
	}
	return others, true
}

// records splits colon-separated lines with at least n fields, skipping
// blank lines and comments. A line it cannot read fails the whole file,
// since a skipped line could hide a group member.
func records(b []byte, n int) ([][]string, error) {
	var out [][]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Split(line, ":"); len(f) >= n {
			out = append(out, f)
		}
	}
	return out, sc.Err()
}
