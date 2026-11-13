//go:build pihole_red

// Stub-style: SKIP-only (WI 3.1). Excluded from `make test` by the
// pihole_red build tag; the WI 3.1 developer deletes the //go:build line and
// these must pass. Cases: TC-70 (a-h) in planning/pihole-dns-zones/test-cases.md.
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
	"slices"
	"strings"
	"testing"
	"time"

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
