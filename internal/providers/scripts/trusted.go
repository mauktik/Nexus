// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package scripts

import (
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// CheckRootOwned is the FR-20 walk for a path that root reads and acts on
// but does not execute, such as a Docker Compose project's directory and
// files: only root, or a private group of root, may write them or any
// directory above them. A file or directory (wantDir) must be owned by root
// and neither group- nor world-writable unless its group is private; every
// ancestor of the resolved path, and every directory holding a symlink the
// resolution passed through, is judged as a script's ancestors are. A POSIX
// ACL naming another writer refuses. It returns a script-integrity-failed
// problem naming the path.
func CheckRootOwned(path string, wantDir bool) error {
	return hostIntegrity.checkOwnedBy(path, wantDir, nil)
}

// Owner is a user, besides root, trusted to own what CheckOwnedBy judges:
// providers.docker.compose.trustedOwners, resolved when the configuration
// is loaded.
type Owner struct {
	Name string `json:"name"`
	UID  uint32 `json:"uid"`
}

// ResolveOwners resolves user names to Owners. Root (any name with UID 0)
// is always trusted, so it is left out; an unknown name is an error.
func ResolveOwners(names []string) ([]Owner, error) {
	var out []Owner
	for _, name := range names {
		a, err := lookupAccount(name)
		if err != nil {
			return nil, fmt.Errorf("unknown user %q: %w", name, err)
		}
		if a.uid == 0 || slices.ContainsFunc(out, func(o Owner) bool { return o.UID == a.uid }) {
			continue
		}
		out = append(out, Owner{Name: a.name, UID: a.uid})
	}
	return out, nil
}

// CheckOwnedBy is CheckRootOwned with owners trusted as root is for
// ownership: each file and directory it judges may be owned by root or by
// one of owners. Who else may write it does not widen: a group that can
// write must hold no one but root and that path's own owner, a named ACL
// writer must be root or that owner, and a world-writable directory, sticky
// or not, refuses. Another trusted owner is not that path's owner, so it
// is not a permitted writer of it.
func CheckOwnedBy(path string, wantDir bool, owners []Owner) error {
	return hostIntegrity.checkOwnedBy(path, wantDir, owners)
}

func (c integrity) checkOwnedBy(path string, wantDir bool, owners []Owner) error {
	resolved, linkDirs, err := walkLinks(path)
	if err != nil {
		return resolutionFailure(path, err)
	}
	r := run{integrity: c, acct: account{name: "root"}, owners: owners}
	if wantDir {
		if err := r.directory(resolved); err != nil {
			return err
		}
	} else if err := r.dataFile(resolved); err != nil {
		return err
	}
	return r.ancestors(resolved, linkDirs)
}

// dataFile judges a regular file root reads: owned by root or a trusted
// owner, and written by no one else.
func (r *run) dataFile(p string) error {
	fi, err := lstatNoLink(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return refuse(CheckScriptRegularFile, p, fmt.Sprintf("%s is not a regular file (%s)", p, fi.Mode()))
	}
	uid, gid := owner(fi)
	defer r.judgeAs(uid)()
	if uid != 0 && uid != r.acct.uid {
		return refuse(CheckScriptOwner, p, fmt.Sprintf("%s is owned by UID %d, not %s. Fix: chown root %s",
			p, uid, r.mayOwn(), quote(p)))
	}
	perm := fi.Mode().Perm()
	if perm&0o002 != 0 {
		return refuse(CheckScriptWritable, p, fmt.Sprintf("%s (%s) is world-writable. Fix: chmod o-w %s", p, fi.Mode(), quote(p)))
	}
	if perm&0o020 != 0 {
		db, err := r.accounts()
		if err != nil {
			return err
		}
		if others, known := db.otherMembers(gid, r.acct); !known || len(others) > 0 {
			return refuse(CheckScriptWritable, p, fmt.Sprintf("%s (%s) is writable by group %d, which is not private to %s. "+
				"Fix: chmod g-w %s", p, fi.Mode(), gid, r.trusted(), quote(p)))
		}
	}
	return r.aclWriters(p, "file")
}

// lstatNoLink is lstat for a component the resolution said is not a link: a
// link now means the path changed during the check.
func lstatNoLink(p string) (fs.FileInfo, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, resolutionFailure(p, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, changed(p)
	}
	return fi, nil
}

// judgeAs makes the account of the path being judged its owner when the
// owner is trusted, so the owner, group and ACL rules all name that owner
// rather than root, and returns the function that restores root. The
// script check has no owners, so it is unchanged there.
func (r *run) judgeAs(uid uint32) func() {
	i := slices.IndexFunc(r.owners, func(o Owner) bool { return o.UID == uid })
	if uid == 0 || i < 0 {
		return func() {}
	}
	prev := r.acct
	r.acct = account{name: r.owners[i].Name, uid: uid}
	return func() { r.acct = prev }
}

// mayOwn names who may own a path: "root, a or b" for the Compose check
// with trusted owners, else root and the run-as user (trusted). Who may
// write a path is narrower, and trusted names it: root and that path's
// owner.
func (r *run) mayOwn() string {
	if len(r.owners) == 0 {
		return r.trusted()
	}
	names := []string{"root"}
	for _, o := range r.owners {
		names = append(names, o.Name)
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
