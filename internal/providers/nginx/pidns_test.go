// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostPIDNS and containerPIDNS stand for the targets of /proc/<pid>/ns/pid.
const (
	hostPIDNS      = "pid:[4026531836]"
	containerPIDNS = "pid:[4026532811]"
)

// writeProcNS writes the ns/pid link of a fake /proc/<pid> directory.
func writeProcNS(t *testing.T, dir, ns string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ns, filepath.Join(dir, "ns", "pid")); err != nil {
		t.Fatal(err)
	}
}

// nginx-provider CR-B28: the real reader reads the ns/pid link
func TestProcDirPIDNamespace(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "100")
	writeProcNS(t, dir, containerPIDNS)
	ns, err := procDir(root).PIDNamespace(100)
	if err != nil || ns != containerPIDNS {
		t.Errorf("PIDNamespace(100) = %q, %v, want %q", ns, err, containerPIDNS)
	}
	if _, err := procDir(root).PIDNamespace(101); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("PIDNamespace of a process that is not there: %v, want not-exist", err)
	}
}

// nsProc is a ProcReader whose PID-namespace links are set per process. A
// process with no entry in ns reads as hostPIDNS; one in errs fails.
type nsProc struct {
	*replayReader
	ns   map[int]string
	errs map[int]error
}

func (n nsProc) PIDNamespace(pid int) (string, error) {
	if err, ok := n.errs[pid]; ok {
		return "", err
	}
	if s, ok := n.ns[pid]; ok {
		return s, nil
	}
	return hostPIDNS, nil
}

// nginx-provider CR-B28: ValidateMaster requires the master to be in PID 1's
// PID namespace. A root nginx master of another container, which an nginx
// user's pidfile can name on OpenRC, has the exe, the title and the uid of
// the host's master but a PID namespace of its own.
func TestValidateMasterRequiresPIDNamespaceOfInit(t *testing.T) {
	fx := loadCapture(t, "ubuntu-valid.jsonl")
	last := fx.pre[len(fx.pre)-1]
	pid := fx.header.MasterPID
	base := &replayReader{
		snaps: []map[int]Proc{snapshotOf(t, last)},
		exe:   map[int]string{pid: testNginxPath},
	}
	gone := fs.ErrNotExist
	denied := errors.New("permission denied")

	for _, c := range []struct {
		name string
		r    nsProc
		want string // "" for accepted, else a word the error names
	}{
		{"same namespace as init", nsProc{base, nil, nil}, ""},
		{"another container's namespace", nsProc{base, map[int]string{pid: containerPIDNS}, nil}, "PID namespace"},
		{"init is in a namespace the master is not in", nsProc{base, map[int]string{1: containerPIDNS}, nil}, "PID namespace"},
		{"master link unreadable", nsProc{base, nil, map[int]error{pid: denied}}, "PID namespace"},
		{"init link unreadable", nsProc{base, nil, map[int]error{1: denied}}, "PID namespace"},
		{"init link not there", nsProc{base, nil, map[int]error{1: gone}}, "PID namespace"},
		{"master gone while reading", nsProc{base, nil, map[int]error{pid: gone}}, "gone"},
	} {
		m, err := ValidateMaster(c.r, pid, testNginxPath)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want == "" && m.PID != pid:
			t.Errorf("%s: master %+v, want pid %d", c.name, m, pid)
		case c.want != "" && err == nil:
			t.Errorf("%s: accepted, want an error naming %q", c.name, c.want)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: error %q does not name %q", c.name, err, c.want)
		}
	}
}
