// Security review SEC-2: the master the init system's pidfile names must be
// owned by root, or a process the nginx user started could stand in for it.

package nginx

import (
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
