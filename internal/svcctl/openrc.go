package svcctl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/mauktik/nexus/internal/platform"
)

// rcServicePath is the absolute path of rc-service.
const rcServicePath = "/sbin/rc-service"

const (
	// maxRecordBytes bounds the read of one start-stop-daemon record.
	maxRecordBytes = 64 << 10
	// maxPIDFileBytes bounds the read of a pidfile.
	maxPIDFileBytes = 64
)

type openrc struct{ base }

func (*openrc) Init() platform.InitSystem { return platform.OpenRC }

func (o *openrc) Status(ctx context.Context, service string) (Status, error) {
	if err := checkService(service); err != nil {
		return Status{State: Unknown}, err
	}
	exists, err := o.call(ctx, []string{rcServicePath, "--exists", service})
	if err != nil {
		return Status{State: Unknown}, err
	}
	if exists.ExitCode != 0 {
		return Status{State: Unknown, Raw: fmt.Sprintf("rc-service --exists %s exited %d", service, exists.ExitCode)}, nil
	}
	res, err := o.call(ctx, []string{rcServicePath, "-C", service, "status"})
	if err != nil {
		return Status{State: Unknown}, err
	}
	out := strings.TrimSpace(string(res.Output))
	word, ok := statusWord(out)
	if !ok && res.ExitCode != 0 {
		return Status{State: Unknown}, fmt.Errorf("svcctl: rc-service -C %s status exited %d: %s", service, res.ExitCode, out)
	}
	st := Status{Known: true, State: Unknown, Raw: out}
	switch word {
	case "started":
		st.State = Running
	case "stopped":
		st.State = Stopped
	case "crashed":
		st.State = Failed
	case "starting":
		st.State = Starting
	case "stopping":
		st.State = Stopping
	}
	if st.State == Running {
		st.MainPID = o.mainPID(service)
	}
	return st, nil
}

func (o *openrc) Do(ctx context.Context, a Action, service string) (Result, error) {
	if err := checkAction(a); err != nil {
		return Result{}, err
	}
	if err := checkService(service); err != nil {
		return Result{}, err
	}
	return o.call(ctx, []string{rcServicePath, "-C", service, string(a)})
}

// statusWord is the word after the last "status:" in rc-service's output
// (" * status: started").
func statusWord(out string) (string, bool) {
	var word string
	found := false
	for line := range strings.Lines(out) {
		if _, rest, ok := strings.Cut(line, "status:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				word, found = f[0], true
			}
		}
	}
	return word, found
}

// mainPID is the PID in the pidfile named by the pidfile= line of
// start-stop-daemon's records in <RunRoot>/openrc/daemons/<service>/, kept
// only when <ProcRoot>/<pid> exists (L-20). Any failure gives 0: a missing
// PID is reported as unknown, never as an error.
func (o *openrc) mainPID(service string) int {
	dir := filepath.Join(o.runRoot, "openrc", "daemons", service)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, name := range names {
		pidfile := recordPIDFile(filepath.Join(dir, name))
		if pidfile == "" {
			continue
		}
		pid := readPIDFile(pidfile)
		if pid <= 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(o.procRoot, strconv.Itoa(pid))); err == nil {
			return pid
		}
	}
	return 0
}

// openRegular opens path for reading without following a symlink at its
// last element and without blocking on a FIFO or a device, and requires the
// opened file itself to be a regular file. Checking the descriptor, not the
// path, leaves no window between the check and the open for the entry to be
// swapped.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	return openRegularFlags(path, syscall.O_NOFOLLOW)
}

// openRegularFollowing is openRegular for a path that is legitimately a
// symlink (an OpenRC init script such as net.eth0 -> net.lo): the link is
// followed, and the file it reaches must still be a regular file, opened
// without blocking.
func openRegularFollowing(path string) (*os.File, os.FileInfo, error) {
	return openRegularFlags(path, 0)
}

func openRegularFlags(path string, extra int) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|extra, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, errors.Join(fmt.Errorf("%s is not a regular file", path), f.Close())
	}
	return f, fi, nil
}

// recordPIDFile is the absolute path on the pidfile= line of a
// start-stop-daemon record, or "".
func recordPIDFile(path string) string {
	f, _, err := openRegular(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxRecordBytes))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "pidfile="); ok && filepath.IsAbs(v) {
			return v
		}
	}
	return ""
}

// readPIDFile is the positive PID on the first line of a pidfile, or 0. The
// pidfile is in nginx's own directory, so it is untrusted: it is opened
// without following a symlink or blocking on a FIFO, must be a regular file
// of at most maxPIDFileBytes (a pid and a newline), and anything else is no
// pid. The caller checks the pid names a live process.
func readPIDFile(path string) int {
	f, fi, err := openRegular(path)
	if err != nil || fi.Size() > maxPIDFileBytes {
		return 0
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPIDFileBytes))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
