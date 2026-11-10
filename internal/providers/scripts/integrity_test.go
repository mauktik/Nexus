package scripts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
)

// me is the account running the tests: the run-as user of every T0 row.
func me(t *testing.T) account {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	return account{name: u.Username, uid: uint32(uid), gid: uint32(gid)}
}

// cleanBase returns a resolved, 0755 directory whose every ancestor passes
// the check for the current user. /tmp is sticky and world-writable, so the
// ancestor walk rightly fails there: when the temporary directory is under
// it, the base is made in the package directory instead.
func cleanBase(t *testing.T) string {
	t.Helper()
	acct := me(t)
	c := hostIntegrity
	c.lookupUser = func(string) (account, error) { return acct, nil }
	candidates := []func() (string, error){
		func() (string, error) { return t.TempDir(), nil },
		func() (string, error) {
			d, err := os.MkdirTemp(".", ".integrity-test-")
			if err == nil {
				t.Cleanup(func() { _ = os.RemoveAll(d) })
			}
			return d, err
		},
	}
	var reasons []string
	for _, mk := range candidates {
		d, err := mk()
		if err != nil {
			t.Fatal(err)
		}
		if d, err = filepath.Abs(d); err != nil {
			t.Fatal(err)
		}
		if d, err = filepath.EvalSymlinks(d); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
		r := run{integrity: c, acct: acct}
		clean := true
		for dir := d; ; dir = filepath.Dir(dir) {
			if err := r.directory(dir); err != nil {
				reasons = append(reasons, err.Error())
				clean = false
				break
			}
			if dir == "/" {
				break
			}
		}
		if clean {
			return d
		}
	}
	t.Fatalf("no base directory with clean ancestors: %s", strings.Join(reasons, "; "))
	return ""
}

// CleanBase lets the external test package stage scripts the real check
// accepts.
func CleanBase(t *testing.T) string { return cleanBase(t) }

func mkdir(t *testing.T, path string, mode fs.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, mode)
	return path
}

func script(t *testing.T, path string, mode fs.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, mode)
	return path
}

// chmod sets mode, including the sticky bit, which os.Chmod passes through
// as fs.ModeSticky.
func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func gidOf(t *testing.T, path string) uint32 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Gid
}

// accounts are /etc/group and /etc/passwd contents for one row.
type accounts struct{ group, passwd string }

// privateGroup holds only the run-as user: the directory's group as
// Ubuntu's umask 0002 leaves it (S-6).
func privateGroup(acct account, gid uint32) accounts {
	return accounts{
		group: fmt.Sprintf("# comment\nwheel:x:0:root\npriv:x:%d:%s\n", gid, acct.name),
		passwd: fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d::/home/%[1]s:/bin/sh\n",
			acct.name, acct.uid, gid),
	}
}

func checker(acct account, db accounts) integrity {
	return integrity{
		lookupUser: func(name string) (account, error) {
			if name != acct.name {
				return account{}, user.UnknownUserError(name)
			}
			return acct, nil
		},
		readFile: func(path string) ([]byte, error) {
			switch path {
			case groupFile:
				return []byte(db.group), nil
			case passwdFile:
				return []byte(db.passwd), nil
			}
			return nil, fs.ErrNotExist
		},
	}
}

// refusal asserts err is script-integrity-failed with check and path, and a
// detail that contains each of want.
func refusal(t *testing.T, err error, check, path string, want ...string) {
	t.Helper()
	var p *problem.Error
	if !errors.As(err, &p) {
		t.Fatalf("got %v, want script-integrity-failed %s at %s", err, check, path)
	}
	if p.Slug() != problem.ScriptIntegrityFailed || p.Status != 422 {
		t.Fatalf("got %s %d, want 422 script-integrity-failed", p.Slug(), p.Status)
	}
	if p.Extensions["check"] != check || p.Extensions["path"] != path {
		t.Fatalf("check %v path %v, want %s %s\ndetail: %s", p.Extensions["check"], p.Extensions["path"],
			check, path, p.Detail)
	}
	for _, w := range want {
		if !strings.Contains(p.Detail, w) {
			t.Errorf("detail %q does not contain %q", p.Detail, w)
		}
	}
}

// TC-ERR26: the non-root integrity matrix. Each failing row is a 422
// script-integrity-failed naming the check and the offending resolved path,
// and each mode failure states the exact chmod. The passing rows pass.
func TestIntegrityMatrix(t *testing.T) {
	acct := me(t)

	type row struct {
		name string
		// stage builds the row under base, and returns the catalog path, the
		// accounts, and the check and path expected (check "" passes, with
		// path the resolved script).
		stage func(t *testing.T, base string) (script string, db accounts, check, path string, detail []string)
	}
	private := func(base string) accounts { return privateGroup(acct, gidOf(t, base)) }
	rows := []row{
		{"clean control", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := script(t, filepath.Join(mkdir(t, filepath.Join(base, "a", "b"), 0o755), "ok.sh"), 0o755)
			return p, private(base), "", p, nil
		}},
		{"private-group parent drwxrwxr-x (S-6)", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "home"), 0o775)
			p := script(t, filepath.Join(dir, "ok.sh"), 0o755)
			return p, private(base), "", p, nil
		}},
		{"symlink to a clean script", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			target := script(t, filepath.Join(mkdir(t, filepath.Join(base, "real"), 0o755), "ok.sh"), 0o755)
			link := symlink(t, target, filepath.Join(mkdir(t, filepath.Join(base, "links"), 0o755), "ok.sh"))
			return link, private(base), "", target, nil
		}},
		{"missing", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := filepath.Join(base, "missing.sh")
			return p, private(base), CheckScriptExists, p, nil
		}},
		{"directory", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := mkdir(t, filepath.Join(base, "dir.sh"), 0o755)
			return p, private(base), CheckScriptRegularFile, p, nil
		}},
		{"not executable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := script(t, filepath.Join(base, "noexec.sh"), 0o644)
			return p, private(base), CheckScriptExecutable, p, []string{"chmod u+x " + p}
		}},
		{"script group-writable under a private group", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := script(t, filepath.Join(base, "gw.sh"), 0o775)
			return p, private(base), CheckScriptWritable, p, []string{"chmod g-w " + p}
		}},
		{"script world-writable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := script(t, filepath.Join(base, "ow.sh"), 0o757)
			return p, private(base), CheckScriptWritable, p, []string{"chmod o-w " + p}
		}},
		{"script group- and world-writable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			p := script(t, filepath.Join(base, "gow.sh"), 0o777)
			return p, private(base), CheckScriptWritable, p, []string{"chmod g-w,o-w " + p}
		}},
		{"parent group-writable by a shared group", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "shared"), 0o775)
			p := script(t, filepath.Join(dir, "s.sh"), 0o755)
			db := private(base)
			db.group = fmt.Sprintf("shared:x:%d:%s,alice\n", gidOf(t, dir), acct.name)
			db.passwd += "alice:x:4242:4242::/home/alice:/bin/sh\n"
			return p, db, CheckDirectoryGroupWritable, dir, []string{"chmod g-w " + dir, "alice"}
		}},
		{"parent group-writable by a member only through a primary GID", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "family"), 0o775)
			p := script(t, filepath.Join(dir, "s.sh"), 0o755)
			db := private(base)
			db.passwd += fmt.Sprintf("bob:x:4243:%d::/home/bob:/bin/sh\n", gidOf(t, dir))
			return p, db, CheckDirectoryGroupWritable, dir, []string{"chmod g-w " + dir, "bob"}
		}},
		{"parent group-writable by a group absent from /etc/group", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "nogroup"), 0o775)
			p := script(t, filepath.Join(dir, "s.sh"), 0o755)
			db := private(base)
			db.group = "wheel:x:0:root\n"
			return p, db, CheckDirectoryGroupWritable, dir, []string{"chmod g-w " + dir, "absent"}
		}},
		{"parent world-writable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "open"), 0o777)
			p := script(t, filepath.Join(dir, "s.sh"), 0o755)
			return p, private(base), CheckDirectoryWorldWritable, dir, []string{"chmod o-w " + dir}
		}},
		{"parent world-writable and group-writable by a shared group", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "open"), 0o777)
			p := script(t, filepath.Join(dir, "s.sh"), 0o755)
			db := private(base)
			db.group = fmt.Sprintf("shared:x:%d:%s,alice\n", gidOf(t, dir), acct.name)
			return p, db, CheckDirectoryWorldWritable, dir, []string{"chmod g-w,o-w " + dir}
		}},
		{"higher ancestor world-writable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			high := mkdir(t, filepath.Join(base, "high"), 0o755)
			p := script(t, filepath.Join(mkdir(t, filepath.Join(high, "mid", "low"), 0o755), "s.sh"), 0o755)
			chmod(t, high, 0o777)
			return p, private(base), CheckDirectoryWorldWritable, high, []string{"chmod o-w " + high}
		}},
		{"sticky world-writable ancestor", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			sticky := mkdir(t, filepath.Join(base, "sticky"), 0o755)
			p := script(t, filepath.Join(mkdir(t, filepath.Join(sticky, "sub"), 0o755), "s.sh"), 0o755)
			chmod(t, sticky, 0o777|fs.ModeSticky)
			return p, private(base), CheckDirectoryWorldWritable, sticky,
				[]string{"chmod o-w " + sticky, "sticky bit does not exempt"}
		}},
		{"symlink to a world-writable target", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			target := script(t, filepath.Join(mkdir(t, filepath.Join(base, "real"), 0o755), "s.sh"), 0o777)
			link := symlink(t, target, filepath.Join(mkdir(t, filepath.Join(base, "links"), 0o755), "s.sh"))
			return link, private(base), CheckScriptWritable, target, []string{"chmod g-w,o-w " + target}
		}},
		{"symlink whose real parent is world-writable", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			dir := mkdir(t, filepath.Join(base, "real"), 0o777)
			target := script(t, filepath.Join(dir, "s.sh"), 0o755)
			link := symlink(t, target, filepath.Join(mkdir(t, filepath.Join(base, "links"), 0o755), "s.sh"))
			return link, private(base), CheckDirectoryWorldWritable, dir, []string{"chmod o-w " + dir}
		}},
		{"symlink held in a world-writable directory", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			target := script(t, filepath.Join(mkdir(t, filepath.Join(base, "real"), 0o755), "s.sh"), 0o755)
			links := mkdir(t, filepath.Join(base, "links"), 0o777)
			link := symlink(t, target, filepath.Join(links, "s.sh"))
			return link, private(base), CheckDirectoryWorldWritable, links, []string{"chmod o-w " + links}
		}},
		{"directory symlink held in a world-writable directory", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			realDir := mkdir(t, filepath.Join(base, "real"), 0o755)
			script(t, filepath.Join(realDir, "s.sh"), 0o755)
			hop := mkdir(t, filepath.Join(base, "hop"), 0o777)
			symlink(t, realDir, filepath.Join(hop, "dir"))
			// The catalog path's own parent is a symlink in a clean
			// directory, which leads through hop.
			link := symlink(t, filepath.Join(hop, "dir"), filepath.Join(base, "entry"))
			return filepath.Join(link, "s.sh"), private(base), CheckDirectoryWorldWritable, hop,
				[]string{"chmod o-w " + hop}
		}},
		{"symlink loop", func(t *testing.T, base string) (string, accounts, string, string, []string) {
			a := filepath.Join(base, "loop-a")
			symlink(t, filepath.Join(base, "loop-b"), a)
			symlink(t, a, filepath.Join(base, "loop-b"))
			return a, private(base), CheckPathResolution, a, nil
		}},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			base := cleanBase(t)
			path, db, check, want, detail := r.stage(t, base)
			got, err := checker(acct, db).check(path, acct.name)
			if check == "" {
				if err != nil {
					t.Fatalf("want a pass, got %v", err)
				}
				if got != want {
					t.Fatalf("resolved %s, want %s", got, want)
				}
				return
			}
			refusal(t, err, check, want, detail...)
		})
	}
}

// Ownership by another user cannot be staged without root on T0, so these
// rows run as a run-as account with another UID: the test user's files are
// then owned by someone other than root or the run-as user. TC-ERR27 (5.3)
// repeats them as root with real users.
func TestIntegrityOwnershipThroughSeam(t *testing.T) {
	acct := me(t)
	other := account{name: "other", uid: acct.uid + 4242, gid: acct.gid + 4242}

	t.Run("script owned by another user", func(t *testing.T) {
		base := cleanBase(t)
		p := script(t, filepath.Join(base, "s.sh"), 0o755)
		_, err := checker(other, privateGroup(other, gidOf(t, base))).check(p, other.name)
		refusal(t, err, CheckScriptOwner, p, "chown root "+p)
	})
	t.Run("directory owned by another user", func(t *testing.T) {
		base := cleanBase(t)
		r := run{integrity: checker(other, privateGroup(other, gidOf(t, base))), acct: other}
		refusal(t, r.directory(base), CheckDirectoryOwner, base, "chown root "+base)
	})
	t.Run("execute class of a run-as user who does not own the script", func(t *testing.T) {
		const gid = 777
		member := accounts{group: fmt.Sprintf("ops:x:%d:other\n", gid)}
		for _, c := range []struct {
			name string
			db   accounts
			perm fs.FileMode
			want bool
		}{
			{"group member, g+x", member, 0o750, true},
			{"group member, g-x o+x: the group class applies", member, 0o701, false},
			{"not a member, g+x only", accounts{group: "ops:x:777:alice\n"}, 0o750, false},
			{"not a member, o+x", accounts{group: "ops:x:777:alice\n"}, 0o701, true},
		} {
			r := run{integrity: checker(other, c.db), acct: other}
			ok, err := r.executable(c.perm, acct.uid, gid)
			if err != nil || ok != c.want {
				t.Errorf("%s: executable %v, %v; want %v", c.name, ok, err, c.want)
			}
		}
	})
	t.Run("script not executable by the run-as user", func(t *testing.T) {
		base := cleanBase(t)
		p := script(t, filepath.Join(base, "s.sh"), 0o700)
		_, err := checker(other, privateGroup(other, gidOf(t, base))).check(p, other.name)
		refusal(t, err, CheckScriptExecutable, p, "chmod a+x "+p)
	})
}

// The run-as user and the account databases fail closed.
func TestIntegrityFailsClosed(t *testing.T) {
	acct := me(t)

	t.Run("unresolvable run-as user", func(t *testing.T) {
		base := cleanBase(t)
		p := script(t, filepath.Join(base, "s.sh"), 0o755)
		_, err := checker(acct, privateGroup(acct, gidOf(t, base))).check(p, "no-such-user")
		refusal(t, err, CheckRunAsUser, p, "no-such-user")
	})
	t.Run("unreadable group database for a group-writable directory", func(t *testing.T) {
		base := cleanBase(t)
		dir := mkdir(t, filepath.Join(base, "home"), 0o775)
		p := script(t, filepath.Join(dir, "s.sh"), 0o755)
		c := checker(acct, accounts{})
		c.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
		_, err := c.check(p, acct.name)
		refusal(t, err, CheckGroupDatabase, groupFile)
	})
	t.Run("relative path", func(t *testing.T) {
		_, err := checker(acct, accounts{}).check("relative/s.sh", acct.name)
		refusal(t, err, CheckPathResolution, "relative/s.sh")
	})
}

// A component that is a symlink when lstat examines it, after resolution
// said it was not, means the path changed during the check.
func TestIntegritySwappedComponentFails(t *testing.T) {
	acct := me(t)
	base := cleanBase(t)
	realDir := mkdir(t, filepath.Join(base, "real"), 0o755)
	dir := filepath.Join(base, "dir")
	symlink(t, realDir, dir)
	r := run{integrity: checker(acct, privateGroup(acct, gidOf(t, base))), acct: acct}
	refusal(t, r.directory(dir), CheckPathResolution, dir, "changed during the check")
	f := script(t, filepath.Join(realDir, "s.sh"), 0o755)
	l := symlink(t, f, filepath.Join(base, "l.sh"))
	refusal(t, r.file(l), CheckPathResolution, l, "changed during the check")
}

func TestQuoteMakesOneShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/deploy/deploy-api.sh": "/opt/deploy/deploy-api.sh",
		"/opt/my scripts/a.sh":      "'/opt/my scripts/a.sh'",
		"/opt/it's/a.sh":            `'/opt/it'\''s/a.sh'`,
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The Compose check's account is root, so its refusals must not say "root or
// root"; a script's refusals keep naming the run-as user beside root.
func TestIntegrityMessagesNameTheTrustedWriters(t *testing.T) {
	mine := me(t)
	root := account{name: "root"}
	svc := account{name: "svc", uid: mine.uid + 1000, gid: mine.gid}

	// A directory and a script the test user owns are foreign to both accounts.
	// Running as root, they are made foreign by giving them to another UID.
	base := cleanBase(t)
	dir := mkdir(t, filepath.Join(base, "d"), 0o755)
	f := script(t, filepath.Join(base, "s.sh"), 0o755)
	if os.Geteuid() == 0 {
		for _, p := range []string{dir, f} {
			if err := os.Chown(p, 12345, 12345); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A directory group-writable by a group that also holds alice, owned by
	// the account itself, reaches the group-writable message.
	shared := mkdir(t, filepath.Join(base, "shared"), 0o775)
	sharedDB := accounts{group: fmt.Sprintf("shared:x:%d:alice\n", gidOf(t, shared))}

	for _, row := range []struct {
		name string
		acct account
		want string // "not <want>." and "not only <want>."
		bad  string // a phrase the message must not contain; empty for none
	}{
		{"root", root, "root", "root or root"},
		{"run-as user", svc, "root or svc", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := run{integrity: checker(row.acct, accounts{}), acct: row.acct}
			refusal(t, r.directory(dir), CheckDirectoryOwner, dir, "not "+row.want+". Fix: chown root "+dir)
			refusal(t, r.file(f), CheckScriptOwner, f, "not "+row.want+". Fix: chown root "+f)

			owned := row.acct
			owned.uid = mine.uid
			if os.Geteuid() == 0 {
				// The shared directory is root's here, which both accounts accept as the owner.
				owned.uid = 0
			}
			g := run{integrity: checker(owned, sharedDB), acct: owned}
			refusal(t, g.directory(shared), CheckDirectoryGroupWritable, shared, "not only "+row.want+". Fix: chmod g-w "+shared)

			for _, fn := range []func() error{func() error { return r.directory(dir) }, func() error { return r.file(f) }, func() error { return g.directory(shared) }} {
				if err := fn(); err != nil && row.bad != "" && strings.Contains(err.Error(), row.bad) {
					t.Errorf("message %q repeats %q", err, row.bad)
				}
			}
		})
	}
}
