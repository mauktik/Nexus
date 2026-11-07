package scripts

import (
	"fmt"
	"maps"
	"os"
	"os/user"
	"slices"
	"strconv"
)

// DefaultPath is a script's PATH unless its catalog entry sets env.PATH
// (A-8). Shell profiles are not sourced, so a directory a profile adds,
// such as ~/.local/bin, is absent until the catalog entry names it.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"

// fallbackShell is SHELL for a run-as user that /etc/passwd does not list,
// such as one known only through NSS on a cgo-less build's host.
const fallbackShell = "/bin/sh"

// identity is a resolved run-as account: who the script runs as, and the
// passwd fields its environment comes from.
type identity struct {
	name   string
	uid    uint32
	gid    uint32
	groups []uint32 // Supplementary groups, from the group database
	home   string
	shell  string
}

// machine is what the runner reads from the host. Its fields are seams: T0
// tests stage an identity and a runtime directory without root.
type machine struct {
	identify func(name string) (identity, error)
	exists   func(path string) bool
	isDir    func(path string) bool
	geteuid  func() int
}

var realMachine = machine{
	identify: lookupIdentity,
	exists: func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	},
	isDir: func(path string) bool {
		fi, err := os.Stat(path)
		return err == nil && fi.IsDir()
	},
	geteuid: os.Geteuid,
}

// lookupIdentity resolves name through os/user: UID, primary GID, every
// supplementary group, so membership of a group such as docker carries over
// (A-7), and the home directory. The login shell comes from /etc/passwd,
// which os/user does not report.
func lookupIdentity(name string) (identity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return identity{}, err
	}
	uid, err := parseID(u.Uid)
	if err != nil {
		return identity{}, fmt.Errorf("user %s has UID %q: %w", name, u.Uid, err)
	}
	gid, err := parseID(u.Gid)
	if err != nil {
		return identity{}, fmt.Errorf("user %s has GID %q: %w", name, u.Gid, err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return identity{}, fmt.Errorf("groups of user %s: %w", name, err)
	}
	groups := make([]uint32, 0, len(gids))
	for _, g := range gids {
		n, err := parseID(g)
		if err != nil {
			return identity{}, fmt.Errorf("user %s is in group %q: %w", name, g, err)
		}
		if !slices.Contains(groups, n) {
			groups = append(groups, n)
		}
	}
	shell, err := passwdShell(name, os.ReadFile)
	if err != nil {
		return identity{}, err
	}
	return identity{name: name, uid: uid, gid: gid, groups: groups, home: u.HomeDir, shell: shell}, nil
}

func parseID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err
}

// passwdShell is name's login shell from /etc/passwd, or fallbackShell when
// the file does not list name or leaves the field empty.
func passwdShell(name string, readFile func(string) ([]byte, error)) (string, error) {
	b, err := readFile(passwdFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", passwdFile, err)
	}
	users, err := records(b, 7)
	if err != nil {
		return "", fmt.Errorf("%s: %w", passwdFile, err)
	}
	for _, f := range users {
		if f[0] == name && f[6] != "" {
			return f[6], nil
		}
	}
	return fallbackShell, nil
}

// runtimeDir is the run-as user's XDG runtime directory.
func runtimeDir(uid uint32) string { return fmt.Sprintf("/run/user/%d", uid) }

// environment is a script's exact environment, built from nothing rather
// than inherited from nexusd or systemd (A-8): HOME, USER, LOGNAME and SHELL
// from the run-as account, the default PATH, TERM=dumb, LANG=C.UTF-8, and
// XDG_RUNTIME_DIR only when withRuntimeDir says the directory exists. The
// catalog entry's env overrides these, and the env parameters override
// that. It is sorted by name, as KEY=VALUE.
func environment(id identity, withRuntimeDir bool, catalog, params map[string]string) []string {
	env := map[string]string{
		"HOME":    id.home,
		"USER":    id.name,
		"LOGNAME": id.name,
		"SHELL":   id.shell,
		"PATH":    DefaultPath,
		"TERM":    "dumb",
		"LANG":    "C.UTF-8",
	}
	if withRuntimeDir {
		env["XDG_RUNTIME_DIR"] = runtimeDir(id.uid)
	}
	maps.Copy(env, catalog)
	maps.Copy(env, params)
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
