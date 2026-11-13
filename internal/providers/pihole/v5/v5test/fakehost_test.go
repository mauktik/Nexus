// Cases: TC-70 (a-h) in planning/pihole-dns-zones/test-cases.md (WI 3.1),
// TC-146 (WI 5.4), plus self-tests of FakeHost's fault injection, FTL events
// and planted gravity process.
//
// These are the fake-fidelity self-tests: each asserts that FakeHost
// reproduces a verified Pi-hole v5.18.4 behavior (architecture V-9, V-12,
// V-13, V-14), including the hazardous ones, so a production path that relies
// on the hazard fails against the fake instead of passing. API names follow
// the "v5test.FakeHost" row of the test-seam contract in test-strategy.md.
package v5test_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const (
	pihole     = "/usr/local/bin/pihole"
	systemdRun = "/usr/bin/systemd-run"
	systemctl  = "/usr/bin/systemctl"
)

func run(t *testing.T, h *v5test.FakeHost, argv ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := h.Run(context.Background(), argv, &out, &out)
	if err != nil {
		t.Fatalf("Run(%q): %v", argv, err)
	}
	return code, out.String()
}

// TC-70a (V-13): enable and disable always exit 0 and flip BLOCKING_ENABLED;
// each sends reload-lists (SIGRTMIN) to FTL.
func TestTC70a_EnableDisableAlwaysExitZero(t *testing.T) {
	h := v5test.NewFakeHost(t)
	if got := h.BlockingEnabled(); got != "true" {
		t.Fatalf("seed BLOCKING_ENABLED=%q, want true", got)
	}
	if code, _ := run(t, h, pihole, "disable"); code != 0 || h.BlockingEnabled() != "false" {
		t.Errorf("disable: exit %d, BLOCKING_ENABLED=%s", code, h.BlockingEnabled())
	}
	if code, _ := run(t, h, pihole, "disable"); code != 0 {
		t.Errorf("disable when already disabled: exit %d, want 0", code)
	}
	if code, _ := run(t, h, pihole, "enable"); code != 0 || h.BlockingEnabled() != "true" {
		t.Errorf("enable: exit %d, BLOCKING_ENABLED=%s", code, h.BlockingEnabled())
	}
	if n := countEvents(h, "reload-lists"); n != 3 {
		t.Errorf("reload-lists sent %d times, want 3", n)
	}
}

func countEvents(h *v5test.FakeHost, ev string) int {
	n := 0
	for _, e := range h.FTLEvents() {
		if e == ev {
			n++
		}
	}
	return n
}

// TC-70b (hazard control, V-13, FR-8): `pihole disable 60s` starts a nohup
// re-enable child in the calling unit's cgroup. While the unit lives it
// re-enables; once the unit ends (KillMode=control-group) the child is gone
// and blocking stays off forever.
func TestTC70b_TimedDisableChildDiesWithUnit(t *testing.T) {
	h := v5test.NewFakeHost(t)
	run(t, h, pihole, "disable", "60s")
	h.Advance(61 * time.Second)
	if h.BlockingEnabled() != "true" {
		t.Fatalf("unit alive: child should have re-enabled, BLOCKING_ENABLED=%s", h.BlockingEnabled())
	}

	h = v5test.NewFakeHost(t)
	run(t, h, pihole, "disable", "60s")
	h.EndUnit()
	h.Advance(10 * time.Minute)
	if h.BlockingEnabled() != "false" {
		t.Fatalf("unit ended: the child must die with it and never re-enable, BLOCKING_ENABLED=%s", h.BlockingEnabled())
	}
}

// TC-70c (A-27): a transient systemd timer is its own unit, so it survives the
// op unit's end and fires `pihole enable`; show reports it while pending;
// stopping a timer that is not loaded exits 5.
func TestTC70c_TransientTimerSurvivesUnit(t *testing.T) {
	h := v5test.NewFakeHost(t)
	at := h.Now().Add(60 * time.Second).UTC().Format(time.RFC3339)
	code, _ := run(t, h, systemdRun, "--unit=nexus-pihole-reenable", "--on-active=60s",
		"--timer-property=AccuracySec=1s", "--timer-property=RemainAfterElapse=no",
		"--description=Nexus: re-enable Pi-hole blocking at "+at, pihole, "enable")
	if code != 0 {
		t.Fatalf("systemd-run exit %d", code)
	}
	run(t, h, pihole, "disable")
	_, show := run(t, h, systemctl, "show", "nexus-pihole-reenable.timer", "--property=SubState,Description")
	if !strings.Contains(show, "SubState=waiting") || !strings.Contains(show, at) {
		t.Errorf("show while pending:\n%s", show)
	}
	h.EndUnit()
	h.Advance(59 * time.Second)
	if h.BlockingEnabled() != "false" {
		t.Fatalf("timer fired early")
	}
	h.Advance(2 * time.Second)
	if h.BlockingEnabled() != "true" {
		t.Fatalf("timer did not fire after the op unit ended")
	}
	if _, ok := h.TimerPending(); ok {
		t.Error("timer still pending after firing (RemainAfterElapse=no)")
	}
	if code, _ := run(t, h, systemctl, "stop", "nexus-pihole-reenable.timer"); code != 5 {
		t.Errorf("stop of an unloaded timer: exit %d, want 5", code)
	}
}

// TC-70d (hazard control, V-14): list.sh adds a domain already in the opposite
// list by silently changing that row's type, exits 0, and does not reload; a
// normal add reloads.
func TestTC70d_ListShMovesSilently(t *testing.T) {
	h := v5test.NewFakeHost(t)
	id := h.SeedDomain(0 /* allow exact */, "ads.example.com", "seeded")
	before := len(h.FTLEvents())
	if code, _ := run(t, h, pihole, "-b", "ads.example.com"); code != 0 {
		t.Fatalf("pihole -b exit %d, want 0", code)
	}
	rows := h.DomainRows()
	if len(rows) != 1 || rows[0].ID != id || rows[0].Type != 1 {
		t.Fatalf("rows %+v: want the same row (id %d) moved to type 1", rows, id)
	}
	if len(h.FTLEvents()) != before {
		t.Errorf("the move sent FTL events %v; list.sh's move path does not reload", h.FTLEvents()[before:])
	}
	run(t, h, pihole, "-b", "new.example.com")
	if countEvents(h, "reload-lists") != 1 {
		t.Errorf("a normal add should reload-lists once, events %v", h.FTLEvents())
	}
	// A quote in a regex breaks list.sh's interpolated SQL: exit 0, nothing stored.
	if code, _ := run(t, h, pihole, "--regex", "a'b"); code != 0 {
		t.Errorf("--regex with a quote: exit %d, want 0 (wrapper always exits 0)", code)
	}
	for _, r := range h.DomainRows() {
		if strings.Contains(r.Domain, "'") {
			t.Errorf("row %+v stored a quoted regex; list.sh cannot", r)
		}
	}
}

// TC-70e (V-9): `pihole -a adlist add|del` always exits 0, also when CheckUrl
// refuses the URL and nothing is stored.
func TestTC70e_AdlistAlwaysExitZero(t *testing.T) {
	h := v5test.NewFakeHost(t)
	if code, _ := run(t, h, pihole, "-a", "adlist", "add", "https://lists.example/hosts.txt", "my list"); code != 0 {
		t.Fatalf("add exit %d", code)
	}
	if !slices.Contains(h.Adlists(), "https://lists.example/hosts.txt") {
		t.Fatalf("adlists %v", h.Adlists())
	}
	if code, _ := run(t, h, pihole, "-a", "adlist", "add", "https://lists.example/<bad>"); code != 0 {
		t.Errorf("refused URL: exit %d, want 0", code)
	}
	if slices.Contains(h.Adlists(), "https://lists.example/<bad>") {
		t.Error("CheckUrl-refused URL was stored")
	}
	if code, _ := run(t, h, pihole, "-a", "adlist", "del", "https://not.present/x"); code != 0 {
		t.Errorf("del of a missing URL: exit %d, want 0", code)
	}
	if countEvents(h, "reload-lists") != 0 {
		t.Error("adlist add/del does not reload FTL (V-9)")
	}
}

// TC-70f (V-12): `pihole -g` streams output, updates info.updated and
// gravity_count, and exits 0 even when a list fails (status 4).
func TestTC70f_GravityExitsZeroOnListFailure(t *testing.T) {
	h := v5test.NewFakeHost(t)
	run(t, h, pihole, "-a", "adlist", "add", "https://lists.example/ok.txt")
	run(t, h, pihole, "-a", "adlist", "add", "https://lists.example/fails.txt")
	h.FailAdlistDownload("https://lists.example/fails.txt")
	code, out := run(t, h, pihole, "-g")
	if code != 0 {
		t.Fatalf("gravity exit %d, want 0 with one failed list", code)
	}
	if !strings.Contains(out, "lists.example/ok.txt") || len(strings.Split(strings.TrimSpace(out), "\n")) < 3 {
		t.Errorf("gravity output:\n%s", out)
	}
	if st := h.AdlistStatus("https://lists.example/fails.txt"); st != 4 {
		t.Errorf("failed list status %d, want 4", st)
	}
	if updated, count := h.GravityInfo(); !updated.Equal(h.Now().Truncate(time.Second)) || count <= 0 {
		t.Errorf("info updated=%v count=%d", updated, count)
	}
}

// TC-70g/h: every argv is recorded in order; an unknown command exits 127 and
// is recorded, so an accidental call is visible to assertions.
func TestTC70g_ArgvRecordedUnknownIs127(t *testing.T) {
	h := v5test.NewFakeHost(t)
	run(t, h, pihole, "enable")
	if code, _ := run(t, h, "/bin/sh", "-c", "pihole enable"); code != 127 {
		t.Errorf("/bin/sh: exit %d, want 127 (no shell exists in the fake)", code)
	}
	want := [][]string{{pihole, "enable"}, {"/bin/sh", "-c", "pihole enable"}}
	if got := h.Argvs(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("argvs %q, want %q", got, want)
	}
}

// Fault injection reports through the real runner's contract (A-19): a
// death by signal is (128+sig, nil) with the signal named on stderr, a
// start failure is (-1, err); the one-shot forms fail once, then the
// command runs. A faulted command has no effect.
func TestFakeHostFaults(t *testing.T) {
	h := v5test.NewFakeHost(t)
	ftlTest := []string{"/usr/bin/pihole-FTL", "--test"}
	h.FailCommandSignal(ftlTest, syscall.SIGSEGV)
	for range 2 {
		var errb bytes.Buffer
		code, err := h.Run(context.Background(), ftlTest, &bytes.Buffer{}, &errb)
		if code != 139 || err != nil || !strings.Contains(errb.String(), "killed by signal SIGSEGV") {
			t.Errorf("signal fault: (%d, %v) %q, want (139, nil) naming SIGSEGV", code, err, errb.String())
		}
	}

	h.FailCommandSignalOnce([]string{pihole, "restartdns"}, syscall.SIGKILL)
	if code, _ := run(t, h, pihole, "restartdns"); code != 137 {
		t.Errorf("one-shot signal: exit %d, want 137", code)
	}
	if code, _ := run(t, h, pihole, "restartdns"); code != 0 || countEvents(h, v5test.EventRestart) != 1 {
		t.Errorf("after the one-shot: exit %d, events %v", code, h.FTLEvents())
	}

	h.FailCommandStartOnce([]string{pihole, "enable"})
	if code, err := h.Run(context.Background(), []string{pihole, "enable"}, &bytes.Buffer{}, &bytes.Buffer{}); code != -1 || err == nil {
		t.Errorf("one-shot start failure: (%d, %v), want (-1, err)", code, err)
	}
	if code, _ := run(t, h, pihole, "enable"); code != 0 {
		t.Errorf("after the one-shot start failure: exit %d", code)
	}

	h.FailCommandStart([]string{systemdRun})
	for range 2 {
		if code, err := h.Run(context.Background(), []string{systemdRun, "--unit=x", "--on-active=5s", pihole, "enable"},
			&bytes.Buffer{}, &bytes.Buffer{}); code != -1 || err == nil {
			t.Errorf("start failure: (%d, %v), want (-1, err)", code, err)
		}
	}
	if _, ok := h.TimerPending(); ok {
		t.Error("a command that failed to start created a timer")
	}

	h.FailCommandOnce([]string{pihole, "disable"}, 1)
	if code, _ := run(t, h, pihole, "disable"); code != 1 || h.BlockingEnabled() != "true" {
		t.Errorf("one-shot exit fault: exit %d, BLOCKING_ENABLED=%s; want 1 with no effect", code, h.BlockingEnabled())
	}
	h.IgnoreCommand([]string{pihole, "disable"})
	if code, _ := run(t, h, pihole, "disable"); code != 0 || h.BlockingEnabled() != "true" {
		t.Errorf("ignored command: exit %d, BLOCKING_ENABLED=%s; want 0 with no effect", code, h.BlockingEnabled())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, err := h.Run(ctx, []string{pihole, "enable"}, &bytes.Buffer{}, &bytes.Buffer{}); code != -1 || !errors.Is(err, context.Canceled) {
		t.Errorf("ended ctx: (%d, %v), want (-1, context.Canceled)", code, err)
	}
}

// restartdns and its reload variants signal FTL; `pihole flush` empties the
// query log and does not touch FTL (FR-15).
func TestFakeHostRestartFlush(t *testing.T) {
	h := v5test.NewFakeHost(t)
	run(t, h, pihole, "restartdns")
	run(t, h, pihole, "restartdns", "reload")
	run(t, h, pihole, "restartdns", "reload-lists")
	want := []string{v5test.EventRestart, v5test.EventReload, v5test.EventReloadLists}
	if got := h.FTLEvents(); !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
	if h.QueryLogLines() == 0 {
		t.Fatal("the seeded query log is empty")
	}
	pid := h.FTL().PID()
	if pid == v5test.FakeFTLPID {
		t.Errorf("FTL PID %d unchanged by restartdns (WI 0.1 check 2)", pid)
	}
	if code, _ := run(t, h, pihole, "flush"); code != 0 || h.QueryLogLines() != 0 || len(h.FTLEvents()) != 3 {
		t.Errorf("flush: exit %d, %d log lines, events %v", code, h.QueryLogLines(), h.FTLEvents())
	}
	if h.FTL().PID() != pid || h.FTL().CacheFlushes() != 1 {
		t.Errorf("flush touched FTL: pid %d->%d, cache flushes %d", pid, h.FTL().PID(), h.FTL().CacheFlushes())
	}
}

// A second systemd-run of a loaded unit fails as systemd's does; show of an
// unloaded unit reports dead; reset-failed of one is not loaded.
func TestFakeHostTimerUnit(t *testing.T) {
	h := v5test.NewFakeHost(t)
	args := []string{systemdRun, "--unit=nexus-pihole-reenable", "--on-active=30s", pihole, "enable"}
	if code, _ := run(t, h, args...); code != 0 {
		t.Fatalf("systemd-run exit %d", code)
	}
	if code, _ := run(t, h, args...); code == 0 {
		t.Error("a second timer of the same unit was created")
	}
	if code, _ := run(t, h, systemctl, "stop", "nexus-pihole-reenable.timer"); code != 0 {
		t.Errorf("stop of the pending timer: exit %d", code)
	}
	if _, show := run(t, h, systemctl, "show", "nexus-pihole-reenable.timer", "--property=SubState,Description"); !strings.Contains(show, "SubState=dead") {
		t.Errorf("show of an unloaded timer:\n%s", show)
	}
	if code, _ := run(t, h, systemctl, "reset-failed", "nexus-pihole-reenable.timer"); code == 0 {
		t.Error("reset-failed of an unloaded unit exited 0")
	}
	h.Advance(time.Minute)
	if h.BlockingEnabled() != "true" || countEvents(h, v5test.EventReloadLists) != 0 {
		t.Errorf("a stopped timer fired: events %v", h.FTLEvents())
	}
}

// A planted gravity run is a /proc/<pid>/cmdline naming gravity.sh until
// stopped (A-16).
func TestFakeHostGravityProcess(t *testing.T) {
	h := v5test.NewFakeHost(t)
	if _, ok := v5.GravityRunning(h.Paths()); ok {
		t.Fatal("a gravity run before any was planted")
	}
	pid, stop := h.PlantGravityProcess()
	if got, ok := v5.GravityRunning(h.Paths()); !ok || got != pid {
		t.Errorf("GravityRunning = %d, %v; want %d", got, ok, pid)
	}
	stop()
	if _, ok := v5.GravityRunning(h.Paths()); ok {
		t.Error("the run is still visible after stop")
	}
}

// adlistID is the id of the adlist with url, read back independently.
func adlistID(t *testing.T, h *v5test.FakeHost, url string) int64 {
	t.Helper()
	db, err := gravitydb.OpenReadOnly(context.Background(), h.Paths().GravityDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Adlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.URL == url {
			return r.PiholeID
		}
	}
	t.Fatalf("no adlist %s", url)
	return 0
}

// TC-146 (NFR-7, A-58): FakeHost encodes the adlist foreign key as the v5
// image showed it (WI 5.1 PROVENANCE "ADLIST REPLACEMENT"; webpage.sh
// CustomizeAdLists L555-577). Its handle enforces foreign keys; `pihole -g`
// replaces the gravity rows, giving each downloaded list its own; `pihole -a
// adlist del` of a list gravity references fails the constraint, prints
// sqlite3's error, keeps the row and still exits 0; one with no gravity rows
// is deleted together with its group link.
func TestTC146_FakeHostEncodesAdlistForeignKey(t *testing.T) {
	const (
		ok    = "https://lists.example/ok.txt"
		fails = "https://lists.example/fails.txt"
		never = "https://lists.example/never.txt"
	)
	h := v5test.NewFakeHost(t)
	if on := h.ForeignKeys(); on != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 (pihole-FTL sqlite3 enforces them)", on)
	}

	run(t, h, pihole, "-a", "adlist", "add", ok)
	run(t, h, pihole, "-a", "adlist", "add", fails)
	idOK, idFails := adlistID(t, h, ok), adlistID(t, h, fails)
	h.FailAdlistDownload(fails)
	if code, _ := run(t, h, pihole, "-g"); code != 0 {
		t.Fatalf("gravity exit %d", code)
	}
	if g, _ := h.AdlistRefs(idOK); g != 3 {
		t.Errorf("downloaded list has %d gravity rows, want 3", g)
	}
	if g, _ := h.AdlistRefs(idFails); g != 0 {
		t.Errorf("failed list has %d gravity rows, want 0", g)
	}
	// A second run replaces the rows rather than adding to them.
	h.ExecGravityDB(`INSERT INTO gravity (domain, adlist_id) VALUES ('stale.example', ?)`, idOK)
	run(t, h, pihole, "-g")
	if g, _ := h.AdlistRefs(idOK); g != 3 {
		t.Errorf("after a second run the list has %d gravity rows, want 3 (replaced)", g)
	}

	code, out := run(t, h, pihole, "-a", "adlist", "del", ok)
	if code != 0 || !strings.Contains(out, "Error: stepping, FOREIGN KEY constraint failed (19)") {
		t.Errorf("del of a processed list: exit %d, output %q; want exit 0 and the FK error", code, out)
	}
	if !slices.Contains(h.Adlists(), ok) {
		t.Error("the processed list was deleted; the real del is refused by the foreign key")
	}
	if g, grp := h.AdlistRefs(idOK); g != 3 || grp != 1 {
		t.Errorf("processed list refs after the refused del: gravity %d, groups %d; want 3, 1", g, grp)
	}

	run(t, h, pihole, "-a", "adlist", "add", never)
	idNever := adlistID(t, h, never)
	if _, grp := h.AdlistRefs(idNever); grp != 1 {
		t.Fatalf("new list has %d group links, want 1 (tr_adlist_add)", grp)
	}
	code, out = run(t, h, pihole, "-a", "adlist", "del", never)
	if code != 0 || strings.Contains(out, "FOREIGN KEY") {
		t.Errorf("del of a never-downloaded list: exit %d, output %q", code, out)
	}
	if slices.Contains(h.Adlists(), never) {
		t.Error("the never-downloaded list was not deleted")
	}
	if _, grp := h.AdlistRefs(idNever); grp != 0 {
		t.Errorf("its group links %d, want 0 (tr_adlist_delete)", grp)
	}
	if n := h.ForeignKeyViolations(); n != 0 {
		t.Errorf("foreign_key_check reports %d rows", n)
	}
}
