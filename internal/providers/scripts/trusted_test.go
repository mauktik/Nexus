// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// SEC-1: a path owned by someone other than root is refused, whether it is
// the file, or the directory that holds it.
func TestCheckRootOwnedRefusesNonRootOwners(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("files made by root are root-owned")
	}
	base := cleanBase(t)
	f := script(t, filepath.Join(base, "compose.yaml"), 0o644)
	refusal(t, CheckRootOwned(f, false), CheckScriptOwner, f, "chown root "+f)
	refusal(t, CheckRootOwned(base, true), CheckDirectoryOwner, base, "chown root")
	l := symlink(t, f, filepath.Join(base, "link.yaml"))
	refusal(t, CheckRootOwned(l, false), CheckScriptOwner, f)
	if err := CheckRootOwned(filepath.Join(base, "absent.yaml"), false); err == nil {
		t.Error("a missing file passed")
	}
}

// A root-owned file in a root-owned tree passes: the control for the above.
func TestCheckRootOwnedAcceptsRootOwnedPaths(t *testing.T) {
	if err := CheckRootOwned("/etc/hosts", false); err != nil {
		t.Skipf("this host's /etc/hosts is not a clean root-owned file: %v", err)
	}
	if err := CheckRootOwned("/etc", true); err != nil {
		t.Errorf("/etc: %v", err)
	}
}

// ownedByChecker stages the Compose check with the test user as a trusted owner.
// The account databases are the row's; ownership by another real user
// cannot be staged without root, so an untrusted owner is the test user
// with the trusted list naming someone else.
func ownedByChecker(t *testing.T, db accounts) integrity {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("files made by root are root-owned")
	}
	return checker(me(t), db)
}

// providers.docker.compose.trustedOwners: a path the trusted user owns
// passes, the file and the directories alike; a user not in the list is
// still refused, and the refusal names who is trusted.
func TestCheckOwnedByTrustedOwner(t *testing.T) {
	acct := me(t)
	base := cleanBase(t)
	c := ownedByChecker(t, privateGroup(acct, gidOf(t, base)))
	dir := mkdir(t, filepath.Join(base, "stacks", "app"), 0o755)
	f := script(t, filepath.Join(dir, "compose.yaml"), 0o644)
	mine := []Owner{{Name: acct.name, UID: acct.uid}}

	if err := c.checkOwnedBy(f, false, mine); err != nil {
		t.Errorf("file of a trusted owner: %v", err)
	}
	if err := c.checkOwnedBy(dir, true, mine); err != nil {
		t.Errorf("directory of a trusted owner: %v", err)
	}
	// Through a symlink to the trusted file, too.
	l := symlink(t, f, filepath.Join(base, "link.yaml"))
	if err := c.checkOwnedBy(l, false, mine); err != nil {
		t.Errorf("link to a trusted file: %v", err)
	}

	alice := []Owner{{Name: "alice", UID: acct.uid + 4242}}
	refusal(t, c.checkOwnedBy(f, false, alice), CheckScriptOwner, f, "not root or alice", "chown root "+f)
	refusal(t, c.checkOwnedBy(dir, true, alice), CheckDirectoryOwner, dir, "not root or alice", "chown root "+dir)
	refusal(t, c.checkOwnedBy(f, false, nil), CheckScriptOwner, f, "not root.", "chown root "+f)
}

// The no-foreign-writers rule still holds for a trusted owner's paths: a
// group that can write must hold no one but root and that owner, a named
// ACL writer must be root or that owner (another trusted owner included),
// and a world-writable directory, sticky or not, refuses.
func TestCheckOwnedByKeepsTheWriterRules(t *testing.T) {
	acct := me(t)
	mine := []Owner{{Name: acct.name, UID: acct.uid}}

	t.Run("group-writable directory, private group", func(t *testing.T) {
		base := cleanBase(t)
		dir := mkdir(t, filepath.Join(base, "app"), 0o775)
		f := script(t, filepath.Join(dir, "compose.yaml"), 0o664)
		c := ownedByChecker(t, privateGroup(acct, gidOf(t, base)))
		if err := c.checkOwnedBy(f, false, mine); err != nil {
			t.Fatalf("group private to the owner: %v", err)
		}
	})
	t.Run("group-writable directory with a foreign member", func(t *testing.T) {
		base := cleanBase(t)
		dir := mkdir(t, filepath.Join(base, "app"), 0o775)
		f := script(t, filepath.Join(dir, "compose.yaml"), 0o644)
		db := privateGroup(acct, gidOf(t, base))
		db.group = fmt.Sprintf("shared:x:%d:%s,alice\n", gidOf(t, dir), acct.name)
		db.passwd += "alice:x:4242:4242::/home/alice:/bin/sh\n"
		c := ownedByChecker(t, db)
		refusal(t, c.checkOwnedBy(f, false, mine), CheckDirectoryGroupWritable, dir, "alice", "chmod g-w "+dir)
		// Trusting alice too does not make her a writer of acct's directory.
		both := append([]Owner{{Name: "alice", UID: 4242}}, mine...)
		refusal(t, c.checkOwnedBy(f, false, both), CheckDirectoryGroupWritable, dir, "alice")
	})
	t.Run("group-writable file with a foreign member", func(t *testing.T) {
		base := cleanBase(t)
		f := script(t, filepath.Join(base, "compose.yaml"), 0o664)
		db := privateGroup(acct, gidOf(t, base))
		db.group = fmt.Sprintf("shared:x:%d:%s,alice\n", gidOf(t, f), acct.name)
		c := ownedByChecker(t, db)
		refusal(t, c.checkOwnedBy(f, false, mine), CheckScriptWritable, f, "chmod g-w "+f)
	})
	acl := func(writer uint32) []byte {
		return aclBlob(aclEntry{tagUserObj, 7, noID}, aclEntry{aclUser, 7, writer}, aclEntry{tagGroupObj, 5, noID},
			aclEntry{aclMask, 7, noID}, aclEntry{tagOther, 5, noID})
	}
	for _, row := range []struct {
		name   string
		writer uint32
		owners []Owner
		refuse bool
	}{
		{"ACL writer who is the owner", acct.uid, mine, false},
		{"ACL writer who is root", 0, mine, false},
		{"ACL writer who is someone else", 4242, mine, true},
		{"ACL writer who is another trusted owner", 4242, append([]Owner{{Name: "alice", UID: 4242}}, mine...), true},
	} {
		t.Run(row.name, func(t *testing.T) {
			base := cleanBase(t)
			dir := mkdir(t, filepath.Join(base, "app"), 0o755)
			f := script(t, filepath.Join(dir, "compose.yaml"), 0o644)
			c := ownedByChecker(t, privateGroup(acct, gidOf(t, base)))
			c.readACL = func(path string) ([]byte, error) {
				if path == dir {
					return acl(row.writer), nil
				}
				return nil, nil
			}
			err := c.checkOwnedBy(f, false, row.owners)
			if !row.refuse {
				if err != nil {
					t.Fatalf("got %v, want a pass", err)
				}
				return
			}
			refusal(t, err, CheckDirectoryACL, dir, "UID 4242", "setfacl -x u:4242 "+dir)
		})
	}
	t.Run("sticky world-writable ancestor", func(t *testing.T) {
		base := cleanBase(t)
		sticky := mkdir(t, filepath.Join(base, "tmp"), 0o755)
		f := script(t, filepath.Join(mkdir(t, filepath.Join(sticky, "app"), 0o755), "compose.yaml"), 0o644)
		chmod(t, sticky, 0o777|fs.ModeSticky)
		c := ownedByChecker(t, privateGroup(acct, gidOf(t, base)))
		refusal(t, c.checkOwnedBy(f, false, mine), CheckDirectoryWorldWritable, sticky, "sticky bit does not exempt")
	})
}

// Names resolve to UIDs; root is always trusted, so listing it adds
// nothing; an unknown name is an error naming it.
func TestResolveOwners(t *testing.T) {
	acct := me(t)
	got, err := ResolveOwners([]string{"root", acct.name})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Owner{{Name: acct.name, UID: acct.uid}}; acct.uid != 0 && !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got, err := ResolveOwners([]string{"root"}); err != nil || len(got) != 0 {
		t.Fatalf("root alone: %+v, %v; want none", got, err)
	}
	if _, err := ResolveOwners([]string{"nx-no-such-user-182"}); err == nil ||
		!strings.Contains(err.Error(), `"nx-no-such-user-182"`) {
		t.Fatalf("unknown user: %v", err)
	}
}
