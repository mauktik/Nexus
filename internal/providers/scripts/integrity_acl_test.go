package scripts

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// aclEntry is one entry of a system.posix_acl_access attribute.
type aclEntry struct {
	tag, perm uint16
	id        uint32
}

// Entry tags of the attribute; the unnamed ones carry id 0xffffffff.
const (
	tagUserObj  = 0x01
	tagGroupObj = 0x04
	tagOther    = 0x20
	noID        = 0xffffffff
)

// aclBlob encodes entries as the kernel stores them (acl(5)).
func aclBlob(entries ...aclEntry) []byte {
	b := binary.LittleEndian.AppendUint32(nil, aclVersion)
	for _, e := range entries {
		b = binary.LittleEndian.AppendUint16(b, e.tag)
		b = binary.LittleEndian.AppendUint16(b, e.perm)
		b = binary.LittleEndian.AppendUint32(b, e.id)
	}
	return b
}

// CR-19: setfacl -m u:mallory:rwx on a root-owned 0755 directory shows as
// 0775 with group root, which the mode check passes. The ACL is judged on
// its own: a named user or group that can write, and is not root or the
// run-as user, refuses.
func TestIntegrityDirectoryACL(t *testing.T) {
	acct := me(t)
	base := cleanBase(t)
	dir := mkdir(t, filepath.Join(base, "deploy"), 0o775)
	p := script(t, filepath.Join(dir, "s.sh"), 0o755)
	db := privateGroup(acct, gidOf(t, base))
	db.group += "shared:x:5000:" + acct.name + ",alice\nsolo:x:5001:" + acct.name + "\n"
	db.passwd += "alice:x:4242:4242::/home/alice:/bin/sh\n"

	base3 := []aclEntry{{tagUserObj, 7, noID}, {tagGroupObj, 5, noID}, {tagOther, 5, noID}}
	with := func(extra ...aclEntry) []byte {
		return aclBlob(append(append([]aclEntry{}, base3...), extra...)...)
	}
	mask := func(perm uint16) aclEntry { return aclEntry{aclMask, perm, noID} }

	rows := []struct {
		name  string
		acl   []byte
		err   error
		check string
		want  []string
	}{
		{name: "no ACL"},
		{name: "a minimal ACL", acl: aclBlob(base3...)},
		{name: "named user who can write", acl: with(aclEntry{aclUser, 7, 4242}, mask(7)),
			check: CheckDirectoryACL, want: []string{"UID 4242", "setfacl -x u:4242 " + dir}},
		{name: "named user whose write the mask removes", acl: with(aclEntry{aclUser, 7, 4242}, mask(5))},
		{name: "named user who can only read", acl: with(aclEntry{aclUser, 5, 4242}, mask(7))},
		{name: "named root", acl: with(aclEntry{aclUser, 7, 0}, mask(7))},
		{name: "the run-as user", acl: with(aclEntry{aclUser, 7, acct.uid}, mask(7))},
		{name: "named group with another member", acl: with(aclEntry{aclGroup, 7, 5000}, mask(7)),
			check: CheckDirectoryACL, want: []string{"group 5000", "alice", "setfacl -x g:5000 " + dir}},
		{name: "named group with only the run-as user", acl: with(aclEntry{aclGroup, 7, 5001}, mask(7))},
		{name: "named group absent from /etc/group", acl: with(aclEntry{aclGroup, 2, 6000}, mask(7)),
			check: CheckDirectoryACL, want: []string{"group 6000", "absent"}},
		{name: "named entry without a mask", acl: with(aclEntry{aclUser, 7, 4242}),
			check: CheckDirectoryACL, want: []string{"cannot be parsed", "setfacl -b " + dir}},
		{name: "truncated", acl: aclBlob(base3...)[:10],
			check: CheckDirectoryACL, want: []string{"cannot be parsed"}},
		{name: "unknown version", acl: append([]byte{9, 0, 0, 0}, aclBlob(base3...)[4:]...),
			check: CheckDirectoryACL, want: []string{"cannot be parsed"}},
		{name: "unreadable", err: errors.New("input/output error"),
			check: CheckDirectoryACL, want: []string{"cannot be read", "input/output error"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := checker(acct, db)
			c.readACL = func(path string) ([]byte, error) {
				if path == dir {
					return row.acl, row.err
				}
				return nil, nil
			}
			_, err := c.check(p, acct.name)
			if row.check == "" {
				if err != nil {
					t.Fatalf("got %v, want a pass", err)
				}
				return
			}
			refusal(t, err, row.check, dir, row.want...)
		})
	}
}

// The ACL of an ancestor is judged too, not only the script's own directory.
func TestIntegrityAncestorACL(t *testing.T) {
	acct := me(t)
	base := cleanBase(t)
	top := mkdir(t, filepath.Join(base, "top"), 0o775)
	p := script(t, filepath.Join(mkdir(t, filepath.Join(top, "inner"), 0o755), "s.sh"), 0o755)
	c := checker(acct, privateGroup(acct, gidOf(t, base)))
	c.readACL = func(path string) ([]byte, error) {
		if path != top {
			return nil, nil
		}
		return aclBlob(aclEntry{tagUserObj, 7, noID}, aclEntry{aclUser, 2, 4242}, aclEntry{tagGroupObj, 5, noID},
			aclEntry{aclMask, 7, noID}, aclEntry{tagOther, 5, noID}), nil
	}
	_, err := c.check(p, acct.name)
	refusal(t, err, CheckDirectoryACL, top, fmt.Sprint("UID ", 4242))
}
