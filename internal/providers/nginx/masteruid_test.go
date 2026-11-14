// Security review SEC-2 and SEC-7: the master the init system's pidfile names
// must run as root, or a process the nginx user started could stand in for
// it. The uid is the real uid on the Uid: line of /proc/<pid>/status, not the
// owner of /proc/<pid>, which a process that is not dumpable always shows as
// root.

package nginx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ownedProc is a ProcReader that reports its master's real uid, as procDir
// does from /proc/<pid>/status.
type ownedProc struct {
	*replayReader
	uid uint32
}

func (o ownedProc) RealUID(int) (uint32, error) { return o.uid, nil }

// writeProcStatus writes the status file of a fake /proc/<pid> directory
// with uid as its real, effective, saved and filesystem uid.
func writeProcStatus(t *testing.T, dir string, uid uint32) {
	t.Helper()
	writeProcStatusLine(t, dir, fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d", uid, uid, uid, uid))
}

func writeProcStatusLine(t *testing.T, dir, uidLine string) {
	t.Helper()
	status := "Name:\tnginx\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t100\n" + uidLine + "\nGid:\t0\t0\t0\t0\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nginx-provider SEC-2 B: ValidateMaster requires the master to run as the managed owner (root)
func TestValidateMasterRequiresRoot(t *testing.T) {
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	last := fx.pre[len(fx.pre)-1]
	exe := map[int]string{fx.header.MasterPID: testNginxPath}
	base := &replayReader{snaps: []map[int]Proc{snapshotOf(t, last)}, exe: exe}

	if _, err := ValidateMaster(ownedProc{base, managedUID}, fx.header.MasterPID, testNginxPath); err != nil {
		t.Fatalf("a master running as the managed owner (%d) was refused: %v", managedUID, err)
	}
	_, err := ValidateMaster(ownedProc{base, managedUID + 33}, fx.header.MasterPID, testNginxPath)
	if err == nil || !strings.Contains(err.Error(), "runs as uid") {
		t.Errorf("a master running as another user: %v, want a refusal naming its uid", err)
	}
}

// nginx-provider SEC-7: a master that is not dumpable shows /proc/<pid> as
// root-owned; its Uid line says who it really is, and that decides.
func TestValidateMasterIgnoresProcDirOwner(t *testing.T) {
	ownedByTester(t) // /proc/<pid> below is a temp directory the test owns, so it stands for "stat says managed"
	const pid = 100
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := strconv.Itoa(pid) + " (nginx) S 1 100 100 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 12345 1000000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n"
	cmdline := "nginx: master process /usr/sbin/nginx" + strings.Repeat("\x00", 20)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testNginxPath, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	writeProcNS(t, dir, hostPIDNS)
	if err := os.Mkdir(filepath.Join(root, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProcNS(t, filepath.Join(root, "1"), hostPIDNS)
	r := procDir(root)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s is owned by the managed owner %d; its status says otherwise", fi.Name(), managedUID)

	writeProcStatus(t, dir, managedUID+1000)
	if _, err := ValidateMaster(r, pid, testNginxPath); err == nil || !strings.Contains(err.Error(), "runs as uid") {
		t.Errorf("a master whose /proc entry is root-owned but whose Uid is %d: %v, want a refusal naming its uid", managedUID+1000, err)
	}

	// Real uid root, effective uid another user: the real uid decides, and
	// a root-owned process is still accepted by it.
	writeProcStatusLine(t, dir, fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d", managedUID, managedUID+1000, managedUID, managedUID))
	if _, err := ValidateMaster(r, pid, testNginxPath); err != nil {
		t.Errorf("a master with real uid %d: %v", managedUID, err)
	}
	// Real uid another user, effective uid root: refused.
	writeProcStatusLine(t, dir, fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d", managedUID+1000, managedUID, managedUID, managedUID))
	if _, err := ValidateMaster(r, pid, testNginxPath); err == nil {
		t.Error("a master with another user's real uid and root's effective uid was accepted")
	}
}

// nginx-provider SEC-7: the real reader reads the real uid from status
func TestProcDirRealUID(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "100")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProcStatus(t, dir, 4242)
	uid, err := procDir(root).RealUID(100)
	if err != nil || uid != 4242 {
		t.Errorf("RealUID(100) = %d, %v, want 4242", uid, err)
	}
	if _, err := procDir(root).RealUID(101); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("RealUID of a process that is not there: %v, want not-exist", err)
	}
	for name, line := range map[string]string{
		"no fields":     "Uid:",
		"one field":     "Uid:\t0",
		"not a number":  "Uid:\troot\t0\t0\t0",
		"negative":      "Uid:\t-1\t0\t0\t0",
		"bad effective": "Uid:\t0\tx\t0\t0",
	} {
		writeProcStatusLine(t, dir, line)
		if uid, err := procDir(root).RealUID(100); err == nil {
			t.Errorf("%s: RealUID = %d, want an error", name, uid)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tnginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := procDir(root).RealUID(100); err == nil {
		t.Error("a status with no Uid line was accepted")
	}
}
