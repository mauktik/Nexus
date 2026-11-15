package svcctl

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/mauktik/nexus/internal/platform"
)

// The fixed argvs of ListFailed (system-provider test-strategy.md, F-RUN).
var (
	wantListUnitsArgv = []string{"/usr/bin/systemctl", "list-units", "--state=failed", "--plain", "--no-legend", "--no-pager", "--full"}
	wantRCStatusArgv  = []string{"/bin/rc-status", "--crashed"}
)

// fAnswer is what fRun returns for one argv.
type fAnswer struct {
	out  string
	exit int
	hang bool // block until ctx is done, then return ctx.Err(), as Run does
}

// fRun is F-RUN: a runner seam answering exact argvs from a table and
// recording every call. It mirrors Run: a non-zero exit is a result, a
// relative argv[0] or an argv not in the table is an ErrNotStarted error
// (the latter also fails the test).
type fRun struct {
	t     *testing.T
	table map[string]fAnswer
	mu    sync.Mutex
	calls [][]string
}

func newFRun(t *testing.T) *fRun { return &fRun{t: t, table: map[string]fAnswer{}} }

func argvKey(argv []string) string { return strings.Join(argv, "\x00") }

func (f *fRun) answer(argv []string, a fAnswer) *fRun {
	f.table[argvKey(argv)] = a
	return f
}

func (f *fRun) Run(ctx context.Context, argv []string) (Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(argv))
	f.mu.Unlock()
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		return Result{}, fmt.Errorf("%w: refusing to run %q: the program must be an absolute path", ErrNotStarted, argv)
	}
	a, ok := f.table[argvKey(argv)]
	if !ok {
		f.t.Errorf("unexpected argv %q", argv)
		return Result{}, fmt.Errorf("%w: %w", ErrNotStarted, &fs.PathError{Op: "fork/exec", Path: argv[0], Err: syscall.ENOENT})
	}
	if a.hang {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	return Result{ExitCode: a.exit, Output: []byte(a.out)}, nil
}

func (f *fRun) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// capture is a testdata file with the exit code and command its README row
// records.
type capture struct {
	out     string
	exit    int
	command string
}

// readCapture reads testdata/<name> and its README provenance row. This is
// the internal test package, which cannot import svcctltest (it imports
// svcctl), so it parses the row itself, the same way svcctltest.Provenance
// does.
func readCapture(t *testing.T, name string) capture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join("testdata", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| `"+name+"` ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		if len(cells) != 8 {
			t.Fatalf("README row for %s has %d cells, want 8", name, len(cells))
		}
		code, err := strconv.Atoi(strings.TrimSpace(cells[6]))
		if err != nil {
			t.Fatalf("README row %s: exit %q", name, cells[6])
		}
		return capture{out: string(b), exit: code, command: cells[5]}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("README has no row for %s", name)
	return capture{}
}

// specUnitPattern is the unit-name pattern of the work item, written
// independently of failed.go's.
var specUnitPattern = regexp.MustCompile(`^[A-Za-z0-9@._:\-]+\.[a-z]+$`)

// wantSystemdNames is the first field of each line of out that matches the
// unit-name pattern, deduped and sorted.
func wantSystemdNames(out string) []string {
	names := []string{}
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) > 0 && specUnitPattern.MatchString(fields[0]) && len(fields[0]) <= 255 {
			names = append(names, fields[0])
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// wantOpenRCNames is each line of out that matches ServiceNamePattern,
// deduped and sorted.
func wantOpenRCNames(out string) []string {
	re := regexp.MustCompile(ServiceNamePattern)
	names := []string{}
	for line := range strings.Lines(out) {
		if n := strings.TrimSpace(line); re.MatchString(n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// The non-empty name lists of the captures pin the parsers above to what
// the real tools print, so a capture that parsed to nothing cannot make
// TC-HP-16 or TC-HP-18 vacuous.
func TestFailedCapturesParseToNames(t *testing.T) {
	if got := wantSystemdNames(readCapture(t, "systemd/list-units-failed.txt").out); len(got) < 2 {
		t.Errorf("systemd capture parses to %q, want at least the planted unit and one other", got)
	}
	if got := wantOpenRCNames(readCapture(t, "openrc/rc-status-crashed.txt").out); len(got) < 2 {
		t.Errorf("OpenRC capture parses to %q, want at least the planted service and nginx", got)
	}
}

// A unit name longer than systemd's limit (255 characters, see
// testdata/README.md) is dropped, and one at the limit is kept.
func TestFailedSystemdUnitNameLength(t *testing.T) {
	at := strings.Repeat("a", 255-len(".service")) + ".service"
	over := strings.Repeat("a", 256-len(".service")) + ".service"
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{out: over + " loaded failed failed X\n" + at + " loaded failed failed Y\n"})
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Names, []string{at}) || got.Count != 1 {
		t.Errorf("got %d names %q, want only the 255-character one", got.Count, got.Names)
	}
}

// Duplicate names are reported once on both init systems, and every
// non-zero systemd exit is an error, empty output or not.
func TestFailedDedupesAndSystemdNeedsExitZero(t *testing.T) {
	frun := newFRun(t).answer(wantListUnitsArgv, fAnswer{out: "b.service x\na.service y\nb.service z\n"})
	got, err := ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run})
	if err != nil || !slices.Equal(got.Names, []string{"a.service", "b.service"}) || got.Count != 2 {
		t.Errorf("systemd: %+v, %v; want [a.service b.service]", got, err)
	}
	frun = newFRun(t).answer(wantRCStatusArgv, fAnswer{out: "nginx\nnginx\nacpid\n"})
	got, err = ListFailed(t.Context(), platform.OpenRC, Options{Run: frun.Run})
	if err != nil || !slices.Equal(got.Names, []string{"acpid", "nginx"}) || got.Count != 2 {
		t.Errorf("openrc: %+v, %v; want [acpid nginx]", got, err)
	}
	frun = newFRun(t).answer(wantListUnitsArgv, fAnswer{exit: 1})
	got, err = ListFailed(t.Context(), platform.Systemd, Options{Run: frun.Run})
	if err == nil || !strings.Contains(err.Error(), "exit 1") || got.Names != nil {
		t.Errorf("systemd exit 1 empty: %+v, %v; want an error naming exit 1", got, err)
	}
}
