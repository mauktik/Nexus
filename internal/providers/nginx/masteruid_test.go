// Security review SEC-2: the master the init system's pidfile names must be
// owned by root, or a process the nginx user started could stand in for it.

package nginx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownedProc is a ProcReader that reports its master's owner, as procDir
// does from /proc/<pid>.
type ownedProc struct {
	*replayReader
	uid uint32
}

func (o ownedProc) Owner(int) (uint32, error) { return o.uid, nil }

// nginx-provider SEC-2 B: ValidateMaster requires the master to be owned by the managed owner (root)
func TestValidateMasterRequiresRoot(t *testing.T) {
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	last := fx.pre[len(fx.pre)-1]
	exe := map[int]string{fx.header.MasterPID: testNginxPath}
	base := &replayReader{snaps: []map[int]Proc{snapshotOf(t, last)}, exe: exe}

	if _, err := ValidateMaster(ownedProc{base, managedUID}, fx.header.MasterPID, testNginxPath); err != nil {
		t.Fatalf("a master owned by the managed owner (%d) was refused: %v", managedUID, err)
	}
	_, err := ValidateMaster(ownedProc{base, managedUID + 33}, fx.header.MasterPID, testNginxPath)
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Errorf("a master owned by another user: %v, want a refusal naming its owner", err)
	}
}

// nginx-provider SEC-2 C: the real reader reports the owner of /proc/<pid>
func TestProcDirOwner(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "100"), 0o755); err != nil {
		t.Fatal(err)
	}
	uid, err := procDir(root).Owner(100)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("Owner(100) = %d, %v, want the test's own uid %d", uid, err, os.Getuid())
	}
	if _, err := procDir(root).Owner(101); err == nil {
		t.Error("Owner of a process that is not there succeeded")
	}
}
