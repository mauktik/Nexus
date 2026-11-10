package scripts

import (
	"fmt"
	"io/fs"
	"os"
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
	return hostIntegrity.checkRootOwned(path, wantDir)
}

func (c integrity) checkRootOwned(path string, wantDir bool) error {
	resolved, linkDirs, err := walkLinks(path)
	if err != nil {
		return resolutionFailure(path, err)
	}
	r := run{integrity: c, acct: account{name: "root"}}
	if wantDir {
		if err := r.directory(resolved); err != nil {
			return err
		}
	} else if err := r.dataFile(resolved); err != nil {
		return err
	}
	return r.ancestors(resolved, linkDirs)
}

// dataFile judges a regular file root reads: owned by root, and written by
// no one else.
func (r *run) dataFile(p string) error {
	fi, err := lstatNoLink(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return refuse(CheckScriptRegularFile, p, fmt.Sprintf("%s is not a regular file (%s)", p, fi.Mode()))
	}
	uid, gid := owner(fi)
	if uid != 0 {
		return refuse(CheckScriptOwner, p, fmt.Sprintf("%s is owned by UID %d, not root. Fix: chown root %s", p, uid, quote(p)))
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
			return refuse(CheckScriptWritable, p, fmt.Sprintf("%s (%s) is writable by group %d, which is not private to root. "+
				"Fix: chmod g-w %s", p, fi.Mode(), gid, quote(p)))
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
