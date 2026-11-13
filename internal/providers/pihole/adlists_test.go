// Cases: TC-88 to TC-95, TC-ERR18 and TC-ERR19 in
// planning/pihole-dns-zones/test-cases.md (WI 3.5), and TC-89 (as WI 5.4
// changed it), TC-145 and TC-ERR31 (WI 5.4): the
// v5 adlist and gravity methods against a FakeHost, and add-adlist,
// remove-adlist and update-gravity through the Dispatcher, the operation
// manager and the helper. Shared helpers are in service_test.go,
// helper_test.go and domains_test.go.
//
// Regression for the verified v5 hazard "pihole -a adlist always exits 0"
// (V-9, A-17): webpage.sh's CustomizeAdLists silently stores nothing for a
// URL CheckUrl refuses and the pihole wrapper exits 0 anyway, so success is
// decided by reading gravity.db back. FakeHost's IgnoreCommand reproduces it.
package pihole

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/gravitydb"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
)

const (
	listA = "https://lists.example/hosts.txt"
	listB = "https://lists.example/b.txt"
	listC = "http://lists.example:8080/c.txt"
)

// seedAdlist adds url as Pi-hole does (`pihole -a adlist add`) and forgets
// the argv, so a test sees only the commands it caused.
func seedAdlist(t *testing.T, h *v5test.FakeHost, url string) int64 {
	t.Helper()
	if code, err := h.Run(context.Background(), []string{piholeBin, "-a", "adlist", "add", url}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("seed %s: %d %v", url, code, err)
	}
	h.ResetArgvs()
	for _, r := range adlistRows(t, h) {
		if r.URL == url {
			return r.PiholeID
		}
	}
	t.Fatalf("seed %s: not stored", url)
	return 0
}

// adlistRows reads the adlist table back from the fake host's gravity.db.
func adlistRows(t *testing.T, h *v5test.FakeHost) []spec.AdlistProperties {
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
	return rows
}

func isGravity(a []string) bool { return len(a) == 2 && a[0] == piholeBin && a[1] == "-g" }

// TC-88 (FR-12, CON-7, A-17): add runs `pihole -a adlist add <url> <comment>`
// with the comment as its own argv element, returns the row read back,
// created true; adding the same URL again is created false with no command.
func TestTC88_AddAdlistCreatedThenIdempotent(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	got, created, err := m.AddAdlist(context.Background(), listA, "my list", &bytes.Buffer{})
	if err != nil || !created {
		t.Fatalf("add = %+v, %v, %v", got, created, err)
	}
	want := [][]string{{piholeBin, "-a", "adlist", "add", listA, "my list"}}
	if argvs := h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
	rows := adlistRows(t, h)
	if len(rows) != 1 || rows[0].URL != listA || rows[0].Comment != "my list" || rows[0].PiholeID != got.PiholeID {
		t.Fatalf("rows %+v, result %+v", rows, got)
	}
	if got.URL != listA || got.Comment != "my list" || !got.Enabled {
		t.Errorf("result %+v", got)
	}

	again, created, err := m.AddAdlist(context.Background(), listA, "my list", &bytes.Buffer{})
	if err != nil || created || again.PiholeID != got.PiholeID {
		t.Errorf("second add = %+v, created=%v, %v; want created false for %d", again, created, err, got.PiholeID)
	}
	if n := len(h.Argvs()); n != 1 {
		t.Errorf("second add ran a command: %q", h.Argvs())
	}

	// No comment: no trailing argv element.
	h.ResetArgvs()
	if _, created, err := m.AddAdlist(context.Background(), listB, "", &bytes.Buffer{}); err != nil || !created {
		t.Fatalf("add without comment: %v %v", created, err)
	}
	want = [][]string{{piholeBin, "-a", "adlist", "add", listB}}
	if argvs := h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
}

// TC-ERR18 (FR-12, A-17, V-9): an add Pi-hole silently ignores (exit 0, no
// row) is pihole-command-failed{command: "pihole -a adlist add", exitCode: 0}.
func TestTCERR18_IgnoredAdlistAddFails(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	h.IgnoreCommand([]string{piholeBin, "-a", "adlist", "add"})
	_, created, err := m.AddAdlist(context.Background(), listA, "", &bytes.Buffer{})
	slug, pe := slugOf(t, err)
	if slug != problem.PiholeCommandFailed || created {
		t.Fatalf("got %s %q created=%v, want pihole-command-failed", slug, pe.Detail, created)
	}
	if pe.Extensions["command"] != "pihole -a adlist add" || pe.Extensions["exitCode"] != 0 {
		t.Errorf("extensions %+v", pe.Extensions)
	}
	if len(adlistRows(t, h)) != 0 {
		t.Error("a row appeared")
	}
}

// RemoveAdlist deletes the row in gravity.db, reloads the lists once and
// returns the row; an id with no row is not-found with no command (A-58).
func TestRemoveAdlistByID(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	idA := seedAdlist(t, h, listA)
	idB := seedAdlist(t, h, listB)
	got, err := m.RemoveAdlist(context.Background(), idB, &bytes.Buffer{})
	if err != nil || got.PiholeID != idB || got.URL != listB {
		t.Fatalf("remove = %+v, %v", got, err)
	}
	want := [][]string{{piholeBin, "restartdns", "reload-lists"}}
	if argvs := h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
	if rows := adlistRows(t, h); len(rows) != 1 || rows[0].PiholeID != idA {
		t.Errorf("rows %+v, want only %d", rows, idA)
	}

	h.ResetArgvs()
	_, err = m.RemoveAdlist(context.Background(), idB, &bytes.Buffer{})
	if slug, _ := slugOf(t, err); slug != problem.NotFound || len(h.Argvs()) != 0 {
		t.Errorf("unknown id: %s, argvs %q", slug, h.Argvs())
	}
}

// TC-91 (FR-13): gravity's output reaches the writer in the fake's order;
// the result is info read back; exit 0; no failed lists, no warning.
func TestTC91_UpdateGravityStreamsAndReadsBack(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	for _, u := range []string{listA, listB, listC} {
		seedAdlist(t, h, u)
	}
	var out bytes.Buffer
	res, err := m.UpdateGravity(context.Background(), &out)
	if err != nil {
		t.Fatal(err)
	}
	log := out.String()
	last := -1
	for _, line := range []string{"pihole: running pihole -g", "Neutrino emissions", "Target: " + listA,
		"Target: " + listB, "Target: " + listC, "Number of gravity domains: 9", "[✓] Done."} {
		i := strings.Index(log, line)
		if i <= last {
			t.Fatalf("%q at %d, after %d: out of order in\n%s", line, i, last, log)
		}
		last = i
	}
	updated, count := h.GravityInfo()
	if res.GravityDomains == nil || *res.GravityDomains != count || count != 9 {
		t.Errorf("gravityDomains %v, info %d", res.GravityDomains, count)
	}
	if res.GravityUpdated == nil || !res.GravityUpdated.Equal(updated) || updated.IsZero() {
		t.Errorf("gravityUpdated %v, info %v", res.GravityUpdated, updated)
	}
	if res.ExitCode != 0 || res.FailedAdlists != 0 || len(res.Warnings) != 0 || !res.Changed {
		t.Errorf("result %+v", res)
	}
	if argvs := h.Argvs(); len(argvs) != 1 || !isGravity(argvs[0]) {
		t.Errorf("argvs %q", argvs)
	}
}

// TC-92 (FR-13, A-48, V-12): lists that fail to download do not fail the
// update; they are counted, with a warning.
func TestTC92_PartialListFailuresWarn(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	for _, u := range []string{listA, listB, listC} {
		seedAdlist(t, h, u)
	}
	h.FailAdlistDownload(listA)
	h.FailAdlistDownload(listC)
	res, err := m.UpdateGravity(context.Background(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("partial failure failed the update: %v", err)
	}
	if res.FailedAdlists != 2 || !slices.Equal(res.Warnings, []string{"2 blocklists failed to download"}) {
		t.Errorf("failedAdlists %d warnings %q", res.FailedAdlists, res.Warnings)
	}
	if res.ExitCode != 0 || res.GravityDomains == nil || *res.GravityDomains != 3 {
		t.Errorf("result %+v", res)
	}
}

// failingGravity is a Runner whose `pihole -g` prints what an offline
// v5.18.4 gravity printed on the Phase-0 host, then exits 1; every other
// command goes to the fake host.
type failingGravity struct{ *v5test.FakeHost }

func (f failingGravity) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	if isGravity(argv) {
		fmt.Fprintln(stdout, "  [i] Neutrino emissions detected...")
		fmt.Fprintln(stderr, "  [✗] DNS resolution is not available")
		return 1, nil
	}
	return f.FakeHost.Run(ctx, argv, stdout, stderr)
}

// TC-ERR19 (FR-13): gravity exiting 1 is gravity-failed{exitCode: 1}; what
// it printed so far stays in the log.
func TestTCERR19_GravityExit1(t *testing.T) {
	t.Parallel()
	h, m := newV5Lists(t)
	h.FailCommand([]string{piholeBin, "-g"}, 1)
	var out bytes.Buffer
	_, err := m.UpdateGravity(context.Background(), &out)
	slug, pe := slugOf(t, err)
	if slug != problem.GravityFailed || pe.Extensions["exitCode"] != 1 {
		t.Fatalf("got %s %q %+v, want gravity-failed exitCode 1", slug, pe.Detail, pe.Extensions)
	}
	if !strings.Contains(out.String(), "pihole: running pihole -g") {
		t.Errorf("log %q", out.String())
	}

	// The real offline failure: its output is kept, its exit code surfaced.
	m.Runner = failingGravity{h}
	out.Reset()
	_, err = m.UpdateGravity(context.Background(), &out)
	slug, pe = slugOf(t, err)
	if slug != problem.GravityFailed || pe.Extensions["exitCode"] != 1 || !strings.Contains(pe.Detail, "exited with code 1") {
		t.Errorf("offline: %s %q %+v", slug, pe.Detail, pe.Extensions)
	}
	if !strings.Contains(out.String(), "DNS resolution is not available") {
		t.Errorf("gravity's output was not kept: %q", out.String())
	}
}

// TC-89 (FR-12, A-58): remove-adlist by id (a gravity-processed list) and by
// URL (a never-downloaded one, which Admit resolves to its id) runs no
// `pihole -a adlist del`, reloads the lists once per remove; the rows are
// gone; the results hold them.
func TestTC89_RemoveAdlistByIDOrURL(t *testing.T) {
	t.Parallel()
	w := newDomainWorld(t)
	idA := seedAdlist(t, w.h, listA)
	if code, err := w.h.Run(context.Background(), []string{piholeBin, "-g"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("gravity: %d %v", code, err)
	}
	idB := seedAdlist(t, w.h, listB)
	if g, _ := w.h.AdlistRefs(idA); g == 0 {
		t.Fatal("setup: adlist A has no gravity rows")
	}

	rec, pe := w.invoke(spec.RemoveAdlist.Op(), `{"id":`+strconv.FormatInt(idA, 10)+`}`)
	if pe != nil {
		t.Fatalf("remove by id: %v", pe)
	}
	rm := succeeded[spec.AdlistRemoveResult](t, rec)
	if !rm.Changed || rm.Removed.PiholeID != idA || rm.Removed.URL != listA || rm.Gravity != nil {
		t.Errorf("by id: %+v", rm)
	}
	rec, pe = w.invoke(spec.RemoveAdlist.Op(), `{"url":"`+listB+`"}`)
	if pe != nil {
		t.Fatalf("remove by url: %v", pe)
	}
	rm = succeeded[spec.AdlistRemoveResult](t, rec)
	if !rm.Changed || rm.Removed.PiholeID != idB || rm.Removed.URL != listB {
		t.Errorf("by url: %+v", rm)
	}
	if hasArgv(w.h, isAdlistDel) {
		t.Errorf("pihole -a adlist del was run: %q", w.h.Argvs())
	}
	want := [][]string{{piholeBin, "restartdns", "reload-lists"}, {piholeBin, "restartdns", "reload-lists"}}
	if argvs := w.h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
	if rows := adlistRows(t, w.h); len(rows) != 0 {
		t.Errorf("rows %+v remain", rows)
	}
}

// TC-90 (FR-12, FR-13): updateGravity decides whether `pihole -g` runs,
// after the add or remove, with its result nested.
func TestTC90_UpdateGravityFlag(t *testing.T) {
	t.Parallel()
	w := newDomainWorld(t)
	rec, pe := w.invoke(spec.AddAdlist.Op(), `{"url":"`+listA+`","updateGravity":false}`)
	if pe != nil {
		t.Fatalf("add: %v", pe)
	}
	res := succeeded[spec.AdlistResult](t, rec)
	if !res.Created || !res.Changed || res.Gravity != nil || res.Adlist.Host != "h" ||
		res.Adlist.Type != spec.AdlistResourceType || res.Adlist.Name != strconv.FormatInt(res.Adlist.Properties.PiholeID, 10) {
		t.Errorf("add without gravity: %+v", res)
	}
	if hasArgv(w.h, isGravity) {
		t.Fatalf("gravity ran without updateGravity: %q", w.h.Argvs())
	}

	w.h.ResetArgvs()
	rec, pe = w.invoke(spec.AddAdlist.Op(), `{"url":"`+listB+`","comment":"second","updateGravity":true}`)
	if pe != nil {
		t.Fatalf("add with gravity: %v", pe)
	}
	res = succeeded[spec.AdlistResult](t, rec)
	_, count := w.h.GravityInfo()
	if !res.Created || res.Gravity == nil || res.Gravity.GravityDomains == nil || *res.Gravity.GravityDomains != count ||
		res.Adlist.Properties.Comment != "second" {
		t.Errorf("add with gravity: %+v (gravity %+v)", res, res.Gravity)
	}
	want := [][]string{{piholeBin, "-a", "adlist", "add", listB, "second"}, {piholeBin, "-g"}}
	if argvs := w.h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}

	w.h.ResetArgvs()
	rec, pe = w.invoke(spec.RemoveAdlist.Op(), `{"url":"`+listA+`","updateGravity":true}`)
	if pe != nil {
		t.Fatalf("remove with gravity: %v", pe)
	}
	rm := succeeded[spec.AdlistRemoveResult](t, rec)
	if rm.Gravity == nil || rm.Removed.URL != listA || rm.Gravity.ExitCode != 0 {
		t.Errorf("remove with gravity: %+v", rm)
	}
	want = [][]string{{piholeBin, "restartdns", "reload-lists"}, {piholeBin, "-g"}}
	if argvs := w.h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
}

// An add whose gravity fails is gravity-failed, and its detail says the
// adlist was added: the row stays.
func TestAddAdlistGravityFailureSaysAdded(t *testing.T) {
	t.Parallel()
	w := newDomainWorld(t)
	w.h.FailCommand([]string{piholeBin, "-g"}, 1)
	rec, pe := w.invoke(spec.AddAdlist.Op(), `{"url":"`+listA+`","updateGravity":true}`)
	if pe != nil {
		t.Fatalf("refused: %v", pe)
	}
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.GravityFailed ||
		fmt.Sprint(rec.Error.Extensions["exitCode"]) != "1" || !strings.Contains(rec.Error.Detail, "was added") {
		t.Fatalf("got %s %+v", rec.Status, rec.Error)
	}
	if rows := adlistRows(t, w.h); len(rows) != 1 || rows[0].URL != listA {
		t.Errorf("rows %+v", rows)
	}
}

// TC-94 (FR-12): remove-adlist with both id and url, or neither, is 400;
// an unknown id or url is 404; none creates an operation.
func TestTC94_RemoveAdlistAdmit(t *testing.T) {
	t.Parallel()
	w := newDomainWorld(t)
	id := seedAdlist(t, w.h, listA)
	for _, c := range []struct {
		in     string
		slug   problem.Slug
		status int
	}{
		{`{"id":` + strconv.FormatInt(id, 10) + `,"url":"` + listA + `"}`, problem.ValidationFailed, 400},
		{`{}`, problem.ValidationFailed, 400},
		{`{"updateGravity":true}`, problem.ValidationFailed, 400},
		{`{"id":999}`, problem.NotFound, 404},
		{`{"url":"` + listB + `"}`, problem.NotFound, 404},
	} {
		_, pe := w.invoke(spec.RemoveAdlist.Op(), c.in)
		wantProblem(t, c.in, pe, c.slug, c.status, "")
		if c.status == 400 {
			errs, _ := pe.Extensions["errors"].([]problem.FieldError)
			if len(errs) != 1 || !strings.Contains(errs[0].Message, "give either id or url") {
				t.Errorf("%s: errors %+v do not name the rule", c.in, errs)
			}
		}
	}
	if n := w.opCount(); n != 0 || len(adlistRows(t, w.h)) != 1 || len(w.h.Argvs()) != 0 {
		t.Errorf("%d operations, rows %+v, argvs %q", n, adlistRows(t, w.h), w.h.Argvs())
	}
}

// TC-93 (FR-13, A-3): MaxRuntime is 1 h for update-gravity and for an
// adlist op with updateGravity, 10 min otherwise; a lost unit is
// outcome-unknown with the op's LostDetail.
func TestTC93_AdlistMaxRuntime(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	check := func(id string, df provider.DetachedFuncs, in any, want time.Duration) {
		t.Helper()
		run := provider.NewRun("op-"+id, nil)
		s, err := df.Prepare(context.Background(), in, run)
		if err != nil || s.MaxRuntime != want {
			t.Errorf("%s: MaxRuntime %s, %v; want %s", id, s.MaxRuntime, err, want)
		}
		if df.LostDetail == nil || df.LostDetail(run, in) == "" {
			t.Errorf("%s: no LostDetail", id)
		}
		_, err = df.Finalize(context.Background(), in, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
		if slug, pe := slugOf(t, err); slug != problem.OutcomeUnknown || !strings.Contains(pe.Detail, df.LostDetail(run, in)) {
			t.Errorf("%s: lost unit: %s %q", id, slug, pe.Detail)
		}
	}
	found := 0
	for _, b := range w.p.Bindings() {
		df, ok := b.Detached()
		switch b.Op().ID {
		case spec.UpdateGravity.Op().ID:
			check("gravity", df, spec.NoInput{}, time.Hour)
		case spec.AddAdlist.Op().ID:
			check("add", df, spec.AdlistAddInput{URL: listA}, 10*time.Minute)
			check("add-gravity", df, spec.AdlistAddInput{URL: listA, UpdateGravity: true}, time.Hour)
		case spec.RemoveAdlist.Op().ID:
			check("remove", df, spec.AdlistRemoveInput{ID: 1}, 10*time.Minute)
			check("remove-gravity", df, spec.AdlistRemoveInput{ID: 1, UpdateGravity: true}, time.Hour)
		default:
			continue
		}
		if !ok {
			t.Errorf("%s is not detached", b.Op().ID)
		}
		found++
	}
	if found != 3 {
		t.Errorf("%d of add-adlist, remove-adlist and update-gravity bound, want 3", found)
	}
}

// passthrough is a Runner that records whether `pihole -g` was handed the
// helper's own stdout as both its stdout and stderr, not a buffer.
type passthrough struct {
	*v5test.FakeHost
	want   io.Writer
	direct bool
}

func (p *passthrough) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	if isGravity(argv) {
		p.direct = stdout == p.want && stderr == p.want
	}
	return p.FakeHost.Run(ctx, argv, stdout, stderr)
}

// TC-95 (FR-13): the update-gravity helper action hands gravity the
// helper's stdout directly, so `pihole: …` step lines and gravity's lines
// interleave in order with nothing buffered until the end.
func TestTC95_GravityOutputReachesLogInOrder(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	seedAdlist(t, w.h, listA)
	seedAdlist(t, w.h, listB)
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == spec.UpdateGravity.Op().ID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		t.Fatal("update-gravity is not bound")
	}
	const id = "op-gravity-log"
	if _, err := df.Prepare(context.Background(), spec.NoInput{}, provider.NewRun(id, nil)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	pr := &passthrough{FakeHost: w.h, want: &out}
	inj := w.inject()
	inj.Runner = pr
	if code := runHelper(context.Background(), filepath.Join(w.opsDir, id), &out, &out, inj); code != 0 {
		t.Fatalf("helper exit %d: %s", code, out.String())
	}
	if !pr.direct {
		t.Error("pihole -g did not get the helper's stdout and stderr directly")
	}
	log := out.String()
	last := -1
	for _, line := range []string{"pihole: running pihole -g", "Neutrino emissions", "Target: " + listA,
		"Target: " + listB, "[✓] Done.", "pihole: gravity has 6 domains"} {
		i := strings.Index(log, line)
		if i <= last {
			t.Fatalf("%q at %d, after %d: out of order in\n%s", line, i, last, log)
		}
		last = i
	}
	res := readResult(t, filepath.Join(w.opsDir, id))
	if _, ok := res["error"]; ok {
		t.Errorf("result.json has an error: %s", res["error"])
	}
}

// removeAdlistOp runs remove-adlist as the core does: Admit, Prepare, the
// helper on the operation's directory, then Finalize. It returns the typed
// result or the operation's problem, and the operation log.
func removeAdlistOp(w *helperWorld, id string, in spec.AdlistRemoveInput) (spec.AdlistRemoveResult, string, error) {
	w.t.Helper()
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == spec.RemoveAdlist.Op().ID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		w.t.Fatal("remove-adlist is not bound")
	}
	ctx := context.Background()
	if err := df.Admit(ctx, in); err != nil {
		w.t.Fatalf("Admit: %v", err)
	}
	if _, err := df.Prepare(ctx, in, provider.NewRun(id, nil)); err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	var out bytes.Buffer
	code := runHelper(ctx, filepath.Join(w.opsDir, id), &out, &out, w.inject())
	res, err := df.Finalize(ctx, in, detach.Exit{ExitCode: code}, provider.NewRun(id, nil))
	if err != nil {
		return spec.AdlistRemoveResult{}, out.String(), err
	}
	return res.(spec.AdlistRemoveResult), out.String(), nil
}

func isAdlistDel(a []string) bool {
	return slices.Contains(a, "-a") && slices.Contains(a, "adlist") && slices.Contains(a, "del")
}

// TC-145 (FR-12, CON-2, CON-7, A-58): regression for the remove-adlist
// foreign-key bug found on the real v5 image (WI 5.1 PROVENANCE "ADLIST
// REPLACEMENT"): gravity.adlist_id REFERENCES adlist(id), pihole-FTL sqlite3
// enforces foreign keys, so `pihole -a adlist del` cannot delete a list gravity
// has processed, yet exits 0. remove-adlist, run end to end through the
// helper action, must delete the list's gravity rows, group links and the
// list in one transaction, then reload the lists once.
func TestTC145_RemoveAdlistDeletesGravityRowsInOneTransaction(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	idA := seedAdlist(t, w.h, listA)
	idB := seedAdlist(t, w.h, listB)
	if code, err := w.h.Run(context.Background(), []string{piholeBin, "-g"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("gravity: %d %v", code, err)
	}
	for _, id := range []int64{idA, idB} {
		if g, grp := w.h.AdlistRefs(id); g != 3 || grp != 1 {
			t.Fatalf("setup: adlist %d has %d gravity rows, %d group links; want 3, 1", id, g, grp)
		}
	}
	w.h.ResetArgvs()

	res, log, err := removeAdlistOp(w, "op-tc145", spec.AdlistRemoveInput{ID: idA})
	if err != nil {
		t.Fatalf("remove-adlist of a gravity-processed list failed: %v\nlog:\n%s", err, log)
	}
	if !res.Changed || res.Removed.PiholeID != idA || res.Removed.URL != listA || res.Gravity != nil {
		t.Errorf("result %+v, want adlist %d (%s)", res, idA, listA)
	}

	if slices.Contains(w.h.Adlists(), listA) {
		t.Error("adlist A is still in gravity.db")
	}
	if g, grp := w.h.AdlistRefs(idA); g != 0 || grp != 0 {
		t.Errorf("adlist A refs: gravity %d, groups %d; want 0, 0", g, grp)
	}
	if g, grp := w.h.AdlistRefs(idB); g != 3 || grp != 1 || !slices.Contains(w.h.Adlists(), listB) {
		t.Errorf("adlist B touched: gravity %d, groups %d, adlists %q", g, grp, w.h.Adlists())
	}
	if n := w.h.ForeignKeyViolations(); n != 0 {
		t.Errorf("foreign_key_check reports %d rows", n)
	}

	if hasArgv(w.h, isAdlistDel) {
		t.Errorf("pihole -a adlist del was run: %q", w.h.Argvs())
	}
	want := [][]string{{piholeBin, "restartdns", "reload-lists"}}
	if argvs := w.h.Argvs(); !slices.EqualFunc(argvs, want, slices.Equal[[]string]) {
		t.Errorf("argvs %q, want %q", argvs, want)
	}
	line := fmt.Sprintf("pihole: removed adlist %d and its 3 gravity rows from gravity.db", idA)
	committed, reloaded := strings.Index(log, line), strings.Index(log, "pihole: running pihole restartdns reload-lists")
	if committed < 0 || reloaded < committed {
		t.Errorf("log does not show the commit (%q) before the reload:\n%s", line, log)
	}
}

// reinsertOnReload is a Runner that puts an adlist row back just before
// `pihole restartdns reload-lists`, as a gravity run that copied the
// database before the delete and swapped it in afterwards would (V-7).
type reinsertOnReload struct {
	*v5test.FakeHost
	id  int64
	url string
}

func (r reinsertOnReload) Run(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, error) {
	if slices.Equal(argv, []string{piholeBin, "restartdns", "reload-lists"}) {
		r.ExecGravityDB(`INSERT INTO adlist (id, address) VALUES (?, ?)`, r.id, r.url)
	}
	return r.FakeHost.Run(ctx, argv, stdout, stderr)
}

// TC-ERR31 (FR-12, A-58): remove-adlist failure paths.
func TestTCERR31_RemoveAdlistFailurePaths(t *testing.T) {
	t.Parallel()
	// processed is a FakeHost with one gravity-processed adlist.
	processed := func(t *testing.T) (*v5test.FakeHost, *v5.Mutator, int64) {
		h, m := newV5Lists(t)
		id := seedAdlist(t, h, listA)
		if code, err := h.Run(context.Background(), []string{piholeBin, "-g"}, io.Discard, io.Discard); code != 0 || err != nil {
			t.Fatalf("gravity: %d %v", code, err)
		}
		h.ResetArgvs()
		return h, m, id
	}

	t.Run("row restored after the commit", func(t *testing.T) {
		h, m, id := processed(t)
		m.Runner = reinsertOnReload{FakeHost: h, id: id, url: listA}
		_, err := m.RemoveAdlist(context.Background(), id, &bytes.Buffer{})
		slug, pe := slugOf(t, err)
		if slug != problem.PiholeCommandFailed || pe.Extensions["command"] != "gravity.db adlist delete" ||
			pe.Extensions["exitCode"] != 0 {
			t.Fatalf("got %s %q %+v", slug, pe.Detail, pe.Extensions)
		}
		want := fmt.Sprintf("the delete was committed and lists were reloaded, but adlist %d is in gravity.db again; "+
			"a gravity run that started meanwhile may have restored it (V-7); remove it again", id)
		if pe.Detail != want {
			t.Errorf("detail %q, want %q", pe.Detail, want)
		}
	})

	t.Run("reload-lists fails", func(t *testing.T) {
		h, m, id := processed(t)
		h.FailCommand([]string{piholeBin, "restartdns", "reload-lists"}, 1)
		_, err := m.RemoveAdlist(context.Background(), id, &bytes.Buffer{})
		slug, pe := slugOf(t, err)
		if slug != problem.PiholeCommandFailed || pe.Extensions["command"] != "pihole restartdns reload-lists" ||
			pe.Extensions["exitCode"] != 1 {
			t.Fatalf("got %s %q %+v", slug, pe.Detail, pe.Extensions)
		}
		if want := fmt.Sprintf("adlist %d was removed, but lists may not be reloaded", id); !strings.Contains(pe.Detail, want) {
			t.Errorf("detail %q does not say %q", pe.Detail, want)
		}
		if slices.Contains(h.Adlists(), listA) {
			t.Error("the committed delete should stand (the detail reports it)")
		}
	})

	t.Run("undeclared reference", func(t *testing.T) {
		h, m, id := processed(t)
		h.ExecGravityDB(`CREATE TABLE planted (adlist_id INTEGER NOT NULL REFERENCES adlist (id))`)
		h.ExecGravityDB(`INSERT INTO planted (adlist_id) VALUES (?)`, id)
		_, err := m.RemoveAdlist(context.Background(), id, &bytes.Buffer{})
		slug, pe := slugOf(t, err)
		if slug != problem.DependencyUnavailable || !strings.Contains(pe.Detail, "not a usable Pi-hole database; nothing was changed") {
			t.Fatalf("got %s %q", slug, pe.Detail)
		}
		if g, grp := h.AdlistRefs(id); !slices.Contains(h.Adlists(), listA) || g != 3 || grp != 1 {
			t.Errorf("rolled back state: adlists %q, gravity %d, groups %d; want the list, 3, 1", h.Adlists(), g, grp)
		}
		if argvs := h.Argvs(); len(argvs) != 0 {
			t.Errorf("argvs %q, want none", argvs)
		}
	})

	t.Run("external gravity run", func(t *testing.T) {
		h, m, id := processed(t)
		_, stop := h.PlantGravityProcess()
		var out bytes.Buffer
		done := make(chan error, 1)
		go func() {
			_, err := m.RemoveAdlist(context.Background(), id, &out)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("remove finished during a gravity run: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		if !slices.Contains(h.Adlists(), listA) {
			t.Fatal("adlist removed during an external gravity run (it would be lost at the swap)")
		}
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("remove did not resume after the gravity run ended")
		}
		if !strings.Contains(out.String(), "pihole: waiting for a gravity run") {
			t.Errorf("log %q", out.String())
		}
	})

	t.Run("unknown id after Admit", func(t *testing.T) {
		h, m, id := processed(t)
		_, err := m.RemoveAdlist(context.Background(), id+100, &bytes.Buffer{})
		slug, pe := slugOf(t, err)
		if slug != problem.NotFound || pe.Detail != "no pi-hole adlist has that ID; nothing was changed" {
			t.Fatalf("got %s %q", slug, pe.Detail)
		}
		if g, grp := h.AdlistRefs(id); !slices.Contains(h.Adlists(), listA) || g != 3 || grp != 1 {
			t.Errorf("something was deleted: adlists %q, gravity %d, groups %d", h.Adlists(), g, grp)
		}
		if argvs := h.Argvs(); len(argvs) != 0 {
			t.Errorf("argvs %q, want no reload", argvs)
		}
	})
}
