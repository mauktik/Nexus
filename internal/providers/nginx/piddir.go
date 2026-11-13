package nginx

import (
	"os/user"
	"strconv"
)

// pidRunDirs are the directories in which the helper makes nginx's missing
// pid directory, one level down. Only tests replace it.
var pidRunDirs = []string{"/run", "/var/run"}

// pidDirOwner is the owner a pid directory the helper makes gets. Only
// tests replace it.
var pidDirOwner = nginxOwner

// nginxOwner is the uid of user nginx and the gid of group nginx, each root
// where there is no such user or group: the init script's `checkpath
// --owner nginx:nginx`.
func nginxOwner() (uid, gid int) {
	if u, err := user.Lookup("nginx"); err == nil {
		if id, err := strconv.Atoi(u.Uid); err == nil {
			uid = id
		}
	}
	if g, err := user.LookupGroup("nginx"); err == nil {
		if id, err := strconv.Atoi(g.Gid); err == nil {
			gid = id
		}
	}
	return uid, gid
}

// pidPathFromV is the compiled-in pid path in `nginx -V`'s output.
func pidPathFromV(out []byte) string { return "" }
