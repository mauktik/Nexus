// Cases: TC-104 to TC-108 and TC-ERR26 (WI 4.2), TC-109 to TC-114, TC-E13
// and TC-ERR27 (WI 4.3), TC-115 to TC-119, TC-E14 and TC-ERR28 (WI 4.4) in
// planning/pihole-dns-zones/test-cases.md: add-zone, zones.remove,
// add-record, records.update, records.remove and apply-dns-changes through
// the Dispatcher (schema, the shared DNS Admit, lane),
// the operation manager and the helper, whose dnswrite pipeline runs against
// FakeHost + FakeFTL. Also the worked example of A-44 and the helper's own
// re-check of conf-dir.
//
// FakeFTL encodes the verified v5 behavior the pipeline depends on: dnsmasq
// serves the conf files as read at (re)start, and a restart gives FTL a new
// PID. A zone add changes comment lines only, so it must leave both the
// restart count and the PID as they were (NFR-3, A-42).
package pihole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/auth"
	"github.com/mauktik/nexus/internal/core"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/ops"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/pihole/backend"
	"github.com/mauktik/nexus/internal/providers/pihole/dnscheck"
	"github.com/mauktik/nexus/internal/providers/pihole/dnsfile"
	"github.com/mauktik/nexus/internal/providers/pihole/dnswrite"
	"github.com/mauktik/nexus/internal/providers/pihole/spec"
	"github.com/mauktik/nexus/internal/providers/pihole/v5"
	"github.com/mauktik/nexus/internal/providers/pihole/v5/v5test"
	"github.com/mauktik/nexus/internal/store"
)

const dnsTestFile = "/etc/dnsmasq.d/03-custom-dns-names.conf"

var dnsCaller = auth.Principal{TokenID: "tok-dns", Name: "dns", Scopes: []string{spec.ReadScope, spec.DNSScope}}

func dnsFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("dnsfile", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dnsWorld runs the DNS mutations as the server does: the Dispatcher, the
// operation manager, and a detach launcher whose unit runs the helper
// in-process over the FakeHost, with the helper's DNS server set to the
// FakeFTL.
type dnsWorld struct {
	*helperWorld
	readyWindow time.Duration // The helper's readiness window; zero is production's
	st          *store.Store
	disp        *core.Dispatcher
	file        string // dnsTestFile on the fake host
}

// newDNSWorld seeds the DNS file with content, starts FTL on it and waits
// until that FTL answers and its PID file names it.
func newDNSWorld(t *testing.T, content []byte) *dnsWorld {
	t.Helper()
	w := &dnsWorld{helperWorld: newHelperWorld(t)}
	w.file = w.h.Paths().Host(dnsTestFile)
	if err := os.WriteFile(w.file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := w.h.Run(context.Background(), []string{piholeBin, "restartdns"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("seed restart: %d %v", code, err)
	}
	if _, err := dnscheck.WaitReady(context.Background(), w.h.FTL().Addr(), 5*time.Second); err != nil {
		t.Fatalf("seed FTL not ready: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if pid, ok := v5.FTLPID(w.h.Paths()); ok && pid == w.h.FTL().PID() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed FTL never wrote its PID file")
		}
	}
	w.h.ResetArgvs()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	launcher, err := detach.New(detach.Config{OpsDir: w.opsDir, Logger: log}, detach.Options{
		PollInterval: 20 * time.Millisecond,
		Launch: func(_ context.Context, inv detach.Invocation) error {
			if err := os.Remove(filepath.Join(inv.Dir, detach.SpecFile)); err != nil {
				return err
			}
			inj := w.inject()
			inj.DNSServer = w.h.FTL().Addr()
			inj.ReadyWindow = w.readyWindow
			started := time.Now()
			code := runHelper(context.Background(), inv.Dir, io.Discard, io.Discard, inj)
			return detach.WriteExit(inv.Dir, detach.Exit{ExitCode: code, Started: started, Ended: time.Now()})
		},
		UnitState: func(context.Context, string) (detach.UnitState, error) {
			return detach.UnitState{ActiveState: "inactive", LoadState: "not-found"}, nil
		},
		Signal:    func(context.Context, string, syscall.Signal) error { return errors.New("no signals") },
		ListUnits: func(context.Context) ([]string, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	alog, err := audit.Open(audit.Options{Dir: t.TempDir(), Host: "h", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = alog.Close() })
	st, err := store.Open(filepath.Dir(w.opsDir), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ops.New(ops.Options{Store: st, Audit: alog, Dir: w.opsDir, Host: "h", Logger: log, Detach: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("manager shutdown: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	reg, err := core.New([]provider.Provider{w.p}, core.Options{Host: "h", Logger: log, Audit: alog, Ops: mgr,
		Providers: pfConfigs(true, nil)})
	if err != nil {
		t.Fatal(err)
	}
	w.st, w.disp = st, core.NewDispatcher(reg, core.DispatcherOptions{Logger: log})
	return w
}

// invoke calls op with the JSON input as REST does. It returns the problem
// of a refusal, or else the finished operation's record.
func (w *dnsWorld) invoke(op def.Op, input string) (store.OpRecord, *problem.Error) {
	w.t.Helper()
	res, err := w.disp.Invoke(context.Background(), core.InvokeRequest{
		Caller: dnsCaller, Surface: core.SurfaceREST, Op: op, Input: json.RawMessage(input)})
	if err != nil {
		var pe *problem.Error
		if !errors.As(err, &pe) {
			w.t.Fatalf("%s: error %T %v is not a problem", op.ID, err, err)
		}
		return store.OpRecord{}, pe
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec, err := w.st.Get(res.OperationID)
		if err == nil && rec.Status.Terminal() {
			return rec, nil
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation %s: %+v (%v)", res.OperationID, rec, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *dnsWorld) opCount() int {
	w.t.Helper()
	page, err := w.st.List(store.ListQuery{Limit: store.MaxListLimit})
	if err != nil {
		w.t.Fatal(err)
	}
	return len(page.Records)
}

func (w *dnsWorld) live() []byte {
	w.t.Helper()
	b, err := os.ReadFile(w.file)
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

// zoneCounts is the zone list read through the provider, as zone=count.
func (w *dnsWorld) zoneCounts() []string {
	w.t.Helper()
	l, err := w.p.listZones(context.Background(), spec.NoInput{})
	if err != nil {
		w.t.Fatal(err)
	}
	out := []string{}
	for _, z := range l.Items {
		out = append(out, z.Name+"="+strconv.Itoa(z.Properties.Records))
	}
	return out
}

// ranConfigTest reports whether `pihole-FTL --test` ran.
func (w *dnsWorld) ranConfigTest() bool {
	return hasArgv(w.h, func(a []string) bool { return slices.Contains(a, "--test") })
}

// noLocalLine fails when the file holds a local= line, which would NXDOMAIN
// the public names of a split-horizon zone such as xi.example.
func noLocalLine(t *testing.T, b []byte) {
	t.Helper()
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "local=") {
			t.Errorf("the DNS file has a local= line: %q", l)
		}
	}
}

// TC-104 (FR-18, FR-19, NFR-3, FR-23, A-42): three zone adds on the FR-23
// 7-line file grow the zone list 0 -> 1 -> 2 -> 3 with record counts 1, 2,
// 2 and leave exactly the FR-23 rendering; no add tests or restarts DNS or
// changes FTL's PID; the first reorganizes the file and names the pre-Nexus
// backup. Each result.json, as runHelper wrote it, has a result and no
// error member (the typed-nil guard on the first real dnswrite caller).
func TestTC104_ZoneAddSecondState(t *testing.T) {
	t.Parallel()
	orig := dnsFixture(t, "fr23-seven-lines.conf")
	w := newDNSWorld(t, orig)
	pid, restarts := w.h.FTL().PID(), w.h.FTL().Restarts()
	if got := w.zoneCounts(); len(got) != 0 {
		t.Fatalf("zones before %v, want none", got)
	}
	steps := []struct {
		zone  string
		zones []string
	}{
		{"app.lan", []string{"app.lan=1"}},
		{"home.local", []string{"app.lan=1", "home.local=2"}},
		{"xi.example", []string{"app.lan=1", "home.local=2", "xi.example=2"}},
	}
	backup := w.p.backupPath(dnsTestFile)
	for i, s := range steps {
		rec, pe := w.invoke(spec.AddZone.Op(), `{"zone":"`+s.zone+`"}`)
		if pe != nil {
			t.Fatalf("add %s: %s %q", s.zone, pe.Slug(), pe.Detail)
		}
		raw := readResult(t, filepath.Join(w.opsDir, rec.ID))
		if _, ok := raw["error"]; ok {
			t.Errorf("add %s: result.json has an error member: %s", s.zone, raw["error"])
		}
		if _, ok := raw["result"]; !ok {
			t.Errorf("add %s: result.json has no result member: %v", s.zone, raw)
		}
		res := succeeded[spec.DNSChangeResult](t, rec)
		if !res.Changed || res.Restarted || res.FTLPidBefore == nil || *res.FTLPidBefore != pid ||
			res.FTLPidAfter == nil || *res.FTLPidAfter != pid || len(res.Verified) != 0 || res.Etag != etagOf(w.live()) {
			t.Errorf("add %s: result %+v, want changed, not restarted, pid %d throughout", s.zone, res, pid)
		}
		wantBackup := ""
		if i == 0 {
			wantBackup = backup
		}
		if res.PreNexusBackup != wantBackup {
			t.Errorf("add %s: preNexusBackup %q, want %q", s.zone, res.PreNexusBackup, wantBackup)
		}
		if got := w.zoneCounts(); !slices.Equal(got, s.zones) {
			t.Errorf("after %s: zones %v, want %v", s.zone, got, s.zones)
		}
		if w.h.FTL().Restarts() != restarts || w.h.FTL().PID() != pid {
			t.Errorf("after %s: restarts %d pid %d, want %d and %d", s.zone, w.h.FTL().Restarts(), w.h.FTL().PID(), restarts, pid)
		}
	}
	if got, want := w.live(), dnsFixture(t, "fr23-seven-lines.rendered.conf"); string(got) != string(want) {
		t.Errorf("final file:\n%s\nwant the FR-23 rendering:\n%s", got, want)
	}
	noLocalLine(t, w.live())
	if w.ranConfigTest() {
		t.Errorf("a zone add ran the configuration test: %q", w.h.Argvs())
	}
	if b, err := os.ReadFile(backup); err != nil || string(b) != string(orig) {
		t.Errorf("pre-Nexus backup %s: %v, want the original 7 lines, got %q", backup, err, b)
	}
}

// TC-105 (FR-19): adding a declared zone is 409 conflict at Admit; no
// operation; the file is unchanged.
func TestTC105_AddExistingZoneRefused(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	_, pe := w.invoke(spec.AddZone.Op(), `{"zone":"app.lan"}`)
	wantProblem(t, "add app.lan", pe, problem.Conflict, 409, "")
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("%d operations; file changed: %v", n, string(w.live()) != string(rendered))
	}
}

// TC-106 (FR-19, FR-24): removing a zone with records is 409 naming the
// count, with no operation; cascade deletes both records and the marker
// with one restart and verifies each name absent.
func TestTC106_RemoveZoneNonEmptyThenCascade(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	restarts := w.h.FTL().Restarts()

	_, pe := w.invoke(spec.ZonesRemove.Op(), `{"name":"home.local"}`)
	wantProblem(t, "remove home.local", pe, problem.Conflict, 409, "")
	if !strings.Contains(pe.Detail, "2 records") {
		t.Errorf("detail %q does not state 2 records", pe.Detail)
	}
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Fatalf("refusal: %d operations, file changed %v", n, string(w.live()) != string(rendered))
	}

	rec, pe := w.invoke(spec.ZonesRemove.Op(), `{"name":"home.local","cascade":true}`)
	if pe != nil {
		t.Fatalf("cascade: %s %q", pe.Slug(), pe.Detail)
	}
	res := succeeded[spec.DNSChangeResult](t, rec)
	if !res.Changed || !res.Restarted || w.h.FTL().Restarts() != restarts+1 {
		t.Errorf("cascade: %+v, restarts %d -> %d, want changed and one restart", res, restarts, w.h.FTL().Restarts())
	}
	var names []string
	for _, v := range res.Verified {
		names = append(names, v.FQDN)
		if !v.Passed || len(v.Expected) != 0 || len(v.Observed) != 0 {
			t.Errorf("verified %+v, want passed and absent", v)
		}
	}
	if want := []string{"kiwi.home.local", "photon.home.local"}; !slices.Equal(names, want) {
		t.Errorf("verified %v, want %v", names, want)
	}
	live := string(w.live())
	for _, gone := range []string{"# nexus:zone home.local", "kiwi.home.local", "photon.home.local"} {
		if strings.Contains(live, gone) {
			t.Errorf("%q is still in the file:\n%s", gone, live)
		}
	}
	if got, want := w.zoneCounts(), []string{"app.lan=1", "xi.example=2"}; !slices.Equal(got, want) {
		t.Errorf("zones %v, want %v", got, want)
	}
	noLocalLine(t, w.live())
}

// TC-107 (FR-19, NFR-3): removing an empty zone removes its marker only:
// no configuration test, no restart, the PID unchanged.
func TestTC107_RemoveEmptyZoneNoRestart(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, append(slices.Clone(rendered), "\n# nexus:zone empty.lan\n"...))
	pid, restarts := w.h.FTL().PID(), w.h.FTL().Restarts()

	rec, pe := w.invoke(spec.ZonesRemove.Op(), `{"name":"empty.lan"}`)
	if pe != nil {
		t.Fatalf("remove empty.lan: %s %q", pe.Slug(), pe.Detail)
	}
	res := succeeded[spec.DNSChangeResult](t, rec)
	if !res.Changed || res.Restarted || res.FTLPidAfter == nil || *res.FTLPidAfter != pid {
		t.Errorf("result %+v, want changed without a restart, pid %d", res, pid)
	}
	if string(w.live()) != string(rendered) {
		t.Errorf("file:\n%s\nwant the marker gone:\n%s", w.live(), rendered)
	}
	if w.ranConfigTest() || w.h.FTL().Restarts() != restarts || w.h.FTL().PID() != pid {
		t.Errorf("config test %v, restarts %d -> %d, pid %d -> %d", w.ranConfigTest(), restarts,
			w.h.FTL().Restarts(), pid, w.h.FTL().PID())
	}
}

// A-44 worked example: the unmanaged lines inside a removed zone's section
// move to the end of the preamble and are never dropped; the change is
// comment lines only, so nothing restarts.
func TestA44_RemovedSectionLinesMoveToPreamble(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	seed := "# hand-written header\n\n# nexus:zone empty.lan\n# printer, ask Bob\n\n" + string(rendered)
	w := newDNSWorld(t, []byte(seed))
	restarts := w.h.FTL().Restarts()

	rec, pe := w.invoke(spec.ZonesRemove.Op(), `{"name":"empty.lan"}`)
	if pe != nil {
		t.Fatalf("remove empty.lan: %s %q", pe.Slug(), pe.Detail)
	}
	if res := succeeded[spec.DNSChangeResult](t, rec); res.Restarted {
		t.Errorf("result %+v, want no restart", res)
	}
	want := "# hand-written header\n# printer, ask Bob\n\n" + string(rendered)
	if got := string(w.live()); got != want {
		t.Errorf("file:\n%s\nwant:\n%s", got, want)
	}
	if w.h.FTL().Restarts() != restarts {
		t.Error("DNS restarted")
	}
}

// TC-108 (FR-3, FR-22, ASM-6, A-19): DNS writes are refused at Admit with
// 503 capability-unavailable when dnsmasq would not load the DNS file's
// directory, checked live, and on the v6 backend; no operation either way.
func TestTC108_DNSWritesRefusedNoConfDirOrV6(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")

	w := newDNSWorld(t, rendered)
	if err := os.WriteFile(w.h.Paths().Host(v5.DnsmasqConfFile), []byte("# no conf-dir here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, pe := w.invoke(spec.AddZone.Op(), `{"zone":"new.lan"}`)
	wantProblem(t, "no conf-dir", pe, problem.CapabilityUnavailable, 503, "")
	const reason = "dnsmasq does not load /etc/dnsmasq.d (no conf-dir in /etc/dnsmasq.conf)"
	if pe.Detail != reason || pe.Extensions["reason"] != reason || pe.Extensions["provider"] != spec.Name {
		t.Errorf("no conf-dir: %q %+v, want %q", pe.Detail, pe.Extensions, reason)
	}
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("no conf-dir: %d operations, file changed %v", n, string(w.live()) != string(rendered))
	}

	w = newDNSWorld(t, rendered)
	w.p.reader.Store(&readerBox{r: backend.Unsupported{}}) // What Preflight selects on a v6 host
	_, pe = w.invoke(spec.AddZone.Op(), `{"zone":"new.lan"}`)
	wantProblem(t, "v6", pe, problem.CapabilityUnavailable, 503, "")
	if pe.Extensions["reason"] != backend.ReasonV6DNSWrites || !strings.Contains(pe.Detail, backend.ReasonV6DNSWrites) {
		t.Errorf("v6: %q %+v, want reason %q", pe.Detail, pe.Extensions, backend.ReasonV6DNSWrites)
	}
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("v6: %d operations, file changed %v", n, string(w.live()) != string(rendered))
	}
}

// TC-ERR26 (FR-18, FR-19, FR-22): removing an undeclared zone is 404; an
// add with a stale ifMatch is 409 dns-file-conflict naming both etags; no
// operation.
func TestTCERR26_ZoneRefusals(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)

	_, pe := w.invoke(spec.ZonesRemove.Op(), `{"name":"nope.lan"}`)
	wantProblem(t, "remove nope.lan", pe, problem.NotFound, 404, "")

	stale := "sha256:" + strings.Repeat("0", 64)
	_, pe = w.invoke(spec.AddZone.Op(), `{"zone":"new.lan","ifMatch":"`+stale+`"}`)
	wantProblem(t, "stale ifMatch", pe, problem.DNSFileConflict, 409, "")
	if pe.Extensions["expectedEtag"] != stale || pe.Extensions["currentEtag"] != etagOf(rendered) {
		t.Errorf("stale ifMatch: extensions %+v, want expected %s current %s", pe.Extensions, stale, etagOf(rendered))
	}
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("%d operations, file changed %v", n, string(w.live()) != string(rendered))
	}

	// The current etag passes.
	if _, pe := w.invoke(spec.AddZone.Op(), `{"zone":"new.lan","ifMatch":"`+etagOf(rendered)+`"}`); pe != nil {
		t.Errorf("current ifMatch: %s %q", pe.Slug(), pe.Detail)
	}
}

// The helper checks conf-dir again under the lane: a dnsmasq.conf changed
// after Admit fails the operation with capability-unavailable and nothing
// written.
func TestDNSHelperRechecksConfDir(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == spec.AddZone.Op().ID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		t.Fatal("add-zone is not bound")
	}
	const id = "op-zone-recheck"
	if _, err := df.Prepare(context.Background(), spec.ZoneAddInput{Zone: "new.lan"}, provider.NewRun(id, nil)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.h.Paths().Host(v5.DnsmasqConfFile), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inj := w.inject()
	inj.DNSServer = w.h.FTL().Addr()
	if code := runHelper(context.Background(), filepath.Join(w.opsDir, id), io.Discard, io.Discard, inj); code != 0 {
		t.Fatalf("helper exit %d", code)
	}
	_, err := df.Finalize(context.Background(), spec.ZoneAddInput{Zone: "new.lan"}, detach.Exit{}, provider.NewRun(id, nil))
	if slug, _ := slugOf(t, err); slug != problem.CapabilityUnavailable {
		t.Errorf("got %s, want capability-unavailable", slug)
	}
	if string(w.live()) != string(rendered) {
		t.Error("the DNS file changed")
	}
}

// add-zone and zones.remove run up to 10 min (A-3); a unit lost without a
// result is outcome-unknown naming the rollback copy in the op directory.
func TestZoneOpsMaxRuntimeAndLostDetail(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	check := func(name string, df provider.DetachedFuncs, in any) {
		t.Helper()
		checkDNSOpLifetime(t, w, name, df, in)
	}
	found := 0
	for _, b := range w.p.Bindings() {
		df, ok := b.Detached()
		switch b.Op().ID {
		case spec.AddZone.Op().ID:
			check("add-zone", df, spec.ZoneAddInput{Zone: "a.lan"})
		case spec.ZonesRemove.Op().ID:
			check("remove-zone", df, spec.ZoneRemoveInput{Name: "a.lan"})
		default:
			continue
		}
		if !ok {
			t.Errorf("%s is not detached", b.Op().ID)
		}
		found++
	}
	if found != 2 {
		t.Errorf("%d of add-zone and zones.remove bound, want 2", found)
	}
}

// checkDNSOpLifetime checks that a DNS write op runs up to 10 min (A-3) and
// that its unit, lost without a result, is outcome-unknown naming the
// rollback copy in the op directory.
func checkDNSOpLifetime(t *testing.T, w *helperWorld, name string, df provider.DetachedFuncs, in any) {
	t.Helper()
	id := "op-" + name
	run := provider.NewRun(id, nil)
	s, err := df.Prepare(context.Background(), in, run)
	if err != nil || s.MaxRuntime != 10*time.Minute {
		t.Errorf("%s: MaxRuntime %s, %v; want 10m", name, s.MaxRuntime, err)
	}
	want := "the DNS file may have been replaced; the previous bytes are at " +
		filepath.Join(w.opsDir, id, dnswrite.RollbackCopyName)
	if df.LostDetail == nil || !strings.HasPrefix(df.LostDetail(run, in), want) {
		t.Fatalf("%s: LostDetail does not start %q", name, want)
	}
	_, err = df.Finalize(context.Background(), in, detach.Exit{ExitCode: -1, Signal: "SIGKILL"}, run)
	if slug, pe := slugOf(t, err); slug != problem.OutcomeUnknown || !strings.Contains(pe.Detail, df.LostDetail(run, in)) {
		t.Errorf("%s: lost unit: %s %q", name, slug, pe.Detail)
	}
}

// records is the record list read through the provider, as fqdn=ipv4, with
// the etag every item carries.
func (w *dnsWorld) records(filter ...string) ([]string, string) {
	w.t.Helper()
	l, err := w.p.listRecords(context.Background(), spec.RecordListInput{Filter: filter})
	if err != nil {
		w.t.Fatal(err)
	}
	out, etag := []string{}, ""
	for _, r := range l.Items {
		out = append(out, r.Properties.FQDN+"="+r.Properties.IPv4)
		etag = r.Properties.Etag
	}
	return out, etag
}

// resolve asks FakeFTL for name's A records, as dig @<pihole> does.
func (w *dnsWorld) resolve(name string) []string {
	w.t.Helper()
	ans, err := dnscheck.Query(context.Background(), w.h.FTL().Addr(), name, 2*time.Second)
	if err != nil {
		w.t.Fatalf("query %s: %v", name, err)
	}
	out := []string{}
	for _, a := range ans.A {
		out = append(out, a.String())
	}
	return out
}

// restartdnsRuns counts the `pihole restartdns` argvs run since the last
// ResetArgvs.
func (w *dnsWorld) restartdnsRuns() int {
	n := 0
	for _, a := range w.h.Argvs() {
		if len(a) == 2 && a[0] == piholeBin && a[1] == "restartdns" {
			n++
		}
	}
	return n
}

// mustSucceed invokes op and returns its result, failing on a refusal.
func (w *dnsWorld) mustSucceed(op def.Op, input string) spec.DNSChangeResult {
	w.t.Helper()
	rec, pe := w.invoke(op, input)
	if pe != nil {
		w.t.Fatalf("%s %s: %s %q", op.ID, input, pe.Slug(), pe.Detail)
	}
	return succeeded[spec.DNSChangeResult](w.t, rec)
}

// verifiedOne is the one verified name of res, which must have passed with
// the expected addresses want.
func verifiedOne(t *testing.T, what string, res spec.DNSChangeResult, fqdn string, want []string) {
	t.Helper()
	if len(res.Verified) != 1 {
		t.Fatalf("%s: verified %+v, want only %s", what, res.Verified, fqdn)
	}
	v := res.Verified[0]
	if v.FQDN != fqdn || !v.Passed || !slices.Equal(v.Expected, want) || !slices.Equal(v.Observed, want) {
		t.Errorf("%s: verified %+v, want %s passed with %v", what, v, fqdn, want)
	}
}

// TC-109 (FR-20, FR-21, FR-24): an add under app.lan grows the zone's list
// from 1 to 2 records with the new etag; FakeFTL serves the commanded
// address; the result is verified with one restart.
func TestTC109_RecordAddSecondState(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	restarts := w.h.FTL().Restarts()
	if got, etag := w.records("zone=app.lan"); len(got) != 1 || etag != etagOf(rendered) {
		t.Fatalf("before: %v %s, want 1 record with etag %s", got, etag, etagOf(rendered))
	}

	res := w.mustSucceed(spec.AddRecord.Op(), `{"fqdn":"nexus-test.app.lan","ipv4":"192.168.7.9"}`)
	if !res.Changed || !res.Restarted || res.Etag != etagOf(w.live()) {
		t.Errorf("result %+v, want changed and restarted with the live etag", res)
	}
	verifiedOne(t, "add", res, "nexus-test.app.lan", []string{"192.168.7.9"})
	got, etag := w.records("zone=app.lan")
	slices.Sort(got) // The renderer orders a zone's records; TC-109 asks only for both
	if want := []string{"nexus-test.app.lan=192.168.7.9", "photon.app.lan=192.168.7.5"}; !slices.Equal(got, want) ||
		etag != res.Etag || etag == etagOf(rendered) {
		t.Errorf("after: %v %s, want %v with the new etag %s", got, etag, want, res.Etag)
	}
	if ips := w.resolve("nexus-test.app.lan"); !slices.Equal(ips, []string{"192.168.7.9"}) {
		t.Errorf("FakeFTL answers %v, want 192.168.7.9", ips)
	}
	if n := w.h.FTL().Restarts() - restarts; n != 1 {
		t.Errorf("%d restarts, want 1", n)
	}
	noLocalLine(t, w.live())
}

// TC-110 (FR-21, FR-22): updating a record to the address it has is
// changed:false with no write and no restart.
func TestTC110_UpdateSameIPNoOp(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	pid, restarts := w.h.FTL().PID(), w.h.FTL().Restarts()
	fi0, err := os.Stat(w.file)
	if err != nil {
		t.Fatal(err)
	}

	res := w.mustSucceed(spec.RecordsUpdate.Op(), `{"name":"kiwi.home.local~A","ipv4":"192.168.7.5"}`)
	if res.Changed || res.Restarted || res.Etag != etagOf(rendered) || len(res.Verified) != 0 {
		t.Errorf("result %+v, want changed false, not restarted, etag unchanged", res)
	}
	fi1, err := os.Stat(w.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(w.live()) != string(rendered) || !os.SameFile(fi0, fi1) {
		t.Error("the DNS file was written")
	}
	if w.restartdnsRuns() != 0 || w.ranConfigTest() || w.h.FTL().Restarts() != restarts || w.h.FTL().PID() != pid {
		t.Errorf("argvs %q, restarts %d -> %d: want no test and no restart", w.h.Argvs(), restarts, w.h.FTL().Restarts())
	}
}

// TC-111 (FR-21): an update rewrites the line in place in one atomic
// replacement: every read of the file during the op holds the name, and
// the final file has the new line where the old one was.
func TestTC111_UpdateInPlaceNeverAbsent(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)

	stop := make(chan struct{})
	done := make(chan []string)
	var reads int
	go func() {
		var bad []string
		defer func() { done <- bad }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(w.file)
			if err != nil {
				bad = append(bad, err.Error())
				continue
			}
			reads++
			m := dnsfile.Parse(b).Model()
			if !slices.ContainsFunc(m.Records, func(r dnsfile.Record) bool { return r.FQDN == "kiwi.home.local" }) {
				bad = append(bad, string(b))
			}
		}
	}()
	res := w.mustSucceed(spec.RecordsUpdate.Op(), `{"name":"kiwi.home.local~A","ipv4":"192.168.7.50"}`)
	close(stop)
	if bad := <-done; len(bad) != 0 || reads == 0 {
		t.Errorf("%d reads; %d without kiwi.home.local: %q", reads, len(bad), bad)
	}
	verifiedOne(t, "update", res, "kiwi.home.local", []string{"192.168.7.50"})

	oldLines := strings.Split(string(rendered), "\n")
	newLines := strings.Split(string(w.live()), "\n")
	i := slices.Index(oldLines, "address=/kiwi.home.local/192.168.7.5")
	if len(newLines) != len(oldLines) || i < 0 || newLines[i] != "address=/kiwi.home.local/192.168.7.50" {
		t.Errorf("final file:\n%s\nwant line %d rewritten and %d lines", w.live(), i+1, len(oldLines))
	}
	want := slices.Clone(oldLines)
	want[i] = "address=/kiwi.home.local/192.168.7.50"
	if !slices.Equal(newLines, want) {
		t.Errorf("final file:\n%s\nwant only the kiwi line changed", w.live())
	}
}

// TC-112 (FR-21): unzoned records can be updated and removed; verification
// uses the commanded address and absence; the updated one stays in the
// unzoned section.
func TestTC112_UnzonedRecordsManageable(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)

	res := w.mustSucceed(spec.RecordsUpdate.Op(), `{"name":"unity.local~A","ipv4":"192.168.7.2"}`)
	verifiedOne(t, "update unity.local", res, "unity.local", []string{"192.168.7.2"})
	res = w.mustSucceed(spec.RecordsRemove.Op(), `{"name":"unity.lan~A"}`)
	verifiedOne(t, "remove unity.lan", res, "unity.lan", []string{})

	live := string(w.live())
	if strings.Contains(live, "unity.lan") {
		t.Errorf("unity.lan is still in the file:\n%s", live)
	}
	if !strings.HasSuffix(live, "# nexus:unzoned\naddress=/unity.local/192.168.7.2\n") {
		t.Errorf("unity.local is not in the unzoned section:\n%s", live)
	}
	if got, _ := w.records("zone=unzoned"); !slices.Equal(got, []string{"unity.local=192.168.7.2"}) {
		t.Errorf("unzoned records %v", got)
	}
	if ips := w.resolve("unity.local"); !slices.Equal(ips, []string{"192.168.7.2"}) {
		t.Errorf("unity.local answers %v", ips)
	}
}

// TC-113 (FR-21): a name on two lines cannot be updated or removed: 409
// naming it, no operation, the file unchanged.
func TestTC113_DuplicatesRefused(t *testing.T) {
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	seed := strings.Replace(string(rendered), "address=/photon.app.lan/192.168.7.5\n",
		"address=/photon.app.lan/192.168.7.5\naddress=/dup.app.lan/192.168.7.8\naddress=/dup.app.lan/192.168.7.9\n", 1)
	w := newDNSWorld(t, []byte(seed))

	for _, c := range []struct {
		op    def.Op
		input string
	}{
		{spec.RecordsUpdate.Op(), `{"name":"dup.app.lan~A","ipv4":"192.168.7.10"}`},
		{spec.RecordsRemove.Op(), `{"name":"dup.app.lan~A"}`},
	} {
		_, pe := w.invoke(c.op, c.input)
		wantProblem(t, c.op.ID, pe, problem.Conflict, 409, "")
		if !strings.Contains(pe.Detail, "dup.app.lan") {
			t.Errorf("%s: detail %q does not name dup.app.lan", c.op.ID, pe.Detail)
		}
	}
	if n := w.opCount(); n != 0 || string(w.live()) != seed {
		t.Errorf("%d operations, file changed %v", n, string(w.live()) != seed)
	}
}

// TC-114 (NFR-2): an add, an update and a remove each run `pihole
// restartdns` exactly once.
func TestTC114_OneRestartPerRecordOp(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.rendered.conf"))
	for _, c := range []struct {
		op    def.Op
		input string
	}{
		{spec.AddRecord.Op(), `{"fqdn":"one.app.lan","ipv4":"192.168.7.11"}`},
		{spec.RecordsUpdate.Op(), `{"name":"one.app.lan~A","ipv4":"192.168.7.12"}`},
		{spec.RecordsRemove.Op(), `{"name":"one.app.lan~A"}`},
	} {
		w.h.ResetArgvs()
		restarts := w.h.FTL().Restarts()
		if res := w.mustSucceed(c.op, c.input); !res.Restarted {
			t.Errorf("%s: not restarted: %+v", c.op.ID, res)
		}
		if n, r := w.restartdnsRuns(), w.h.FTL().Restarts()-restarts; n != 1 || r != 1 {
			t.Errorf("%s: %d restartdns argvs, %d FTL restarts; want 1 and 1", c.op.ID, n, r)
		}
	}
}

// TC-E13 (FR-17, FR-21): a record named exactly like its zone belongs to
// it and covers the zone's names that have no line of their own.
func TestTCE13_RecordNamedLikeItsZone(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.rendered.conf"))
	res := w.mustSucceed(spec.AddRecord.Op(), `{"fqdn":"app.lan","ipv4":"192.168.7.3"}`)
	verifiedOne(t, "add app.lan", res, "app.lan", []string{"192.168.7.3"})
	r, err := w.p.getRecord(context.Background(), spec.NameInput{Name: "app.lan~A"})
	if err != nil || r.Properties.Zone == nil || *r.Properties.Zone != "app.lan" {
		t.Errorf("app.lan~A: %+v %v, want zone app.lan", r.Properties, err)
	}
	if ips := w.resolve("x.app.lan"); !slices.Equal(ips, []string{"192.168.7.3"}) {
		t.Errorf("x.app.lan answers %v, want 192.168.7.3", ips)
	}
	if ips := w.resolve("photon.app.lan"); !slices.Equal(ips, []string{"192.168.7.5"}) {
		t.Errorf("photon.app.lan answers %v, want its own 192.168.7.5", ips)
	}
}

// Removing a name that a shorter record name still covers verifies against
// that name's address, not absence (FR-24).
func TestRecordRemoveCoveredBySuffix(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	seed := strings.Replace(string(rendered), "# nexus:zone app.lan\n", "# nexus:zone app.lan\naddress=/app.lan/192.168.7.3\n", 1)
	w := newDNSWorld(t, []byte(seed))
	res := w.mustSucceed(spec.RecordsRemove.Op(), `{"name":"photon.app.lan~A"}`)
	verifiedOne(t, "remove photon.app.lan", res, "photon.app.lan", []string{"192.168.7.3"})
	if v := res.Verified[0]; v.RCode != "NOERROR" {
		t.Errorf("rcode %q, want NOERROR", v.RCode)
	}
	if strings.Contains(string(w.live()), "photon.app.lan") {
		t.Errorf("photon.app.lan is still in the file:\n%s", w.live())
	}
}

// TC-ERR27 (FR-21, ASM-9, ASM-11): an add under no zone is 422
// dns-name-unzoned naming it; an add of an existing name 409; an update of
// an unknown record 404; 0.0.0.0 400; a stale ifMatch 409
// dns-file-conflict. No operation is created and the file is unchanged.
func TestTCERR27_RecordRefusals(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)

	_, pe := w.invoke(spec.AddRecord.Op(), `{"fqdn":"new.example.org","ipv4":"192.168.7.9"}`)
	wantProblem(t, "unzoned add", pe, problem.DNSNameUnzoned, 422, "")
	if pe.Extensions["name"] != "new.example.org" {
		t.Errorf("unzoned add: extensions %+v, want name new.example.org", pe.Extensions)
	}
	_, pe = w.invoke(spec.AddRecord.Op(), `{"fqdn":"kiwi.home.local","ipv4":"192.168.7.9"}`)
	wantProblem(t, "existing add", pe, problem.Conflict, 409, "")
	_, pe = w.invoke(spec.RecordsUpdate.Op(), `{"name":"ghost.app.lan~A","ipv4":"192.168.7.9"}`)
	wantProblem(t, "ghost update", pe, problem.NotFound, 404, "")
	_, pe = w.invoke(spec.RecordsRemove.Op(), `{"name":"ghost.app.lan~A"}`)
	wantProblem(t, "ghost remove", pe, problem.NotFound, 404, "")
	_, pe = w.invoke(spec.AddRecord.Op(), `{"fqdn":"new.app.lan","ipv4":"0.0.0.0"}`)
	wantProblem(t, "0.0.0.0", pe, problem.ValidationFailed, 400, "")
	_, pe = w.invoke(spec.RecordsUpdate.Op(), `{"name":"kiwi.home.local~B","ipv4":"192.168.7.9"}`)
	wantProblem(t, "type B", pe, problem.ValidationFailed, 400, "")
	stale := "sha256:" + strings.Repeat("0", 64)
	_, pe = w.invoke(spec.RecordsRemove.Op(), `{"name":"kiwi.home.local~A","ifMatch":"`+stale+`"}`)
	wantProblem(t, "stale ifMatch", pe, problem.DNSFileConflict, 409, "")

	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("%d operations, file changed %v", n, string(w.live()) != string(rendered))
	}
}

// The record ops refuse a DNS write on the v6 backend at Admit, as every DNS
// write does.
func TestRecordOpsRefusedOnV6(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	w.p.reader.Store(&readerBox{r: backend.Unsupported{}})
	for _, c := range []struct {
		op    def.Op
		input string
	}{
		{spec.AddRecord.Op(), `{"fqdn":"new.app.lan","ipv4":"192.168.7.9"}`},
		{spec.RecordsUpdate.Op(), `{"name":"kiwi.home.local~A","ipv4":"192.168.7.9"}`},
		{spec.RecordsRemove.Op(), `{"name":"kiwi.home.local~A"}`},
	} {
		_, pe := w.invoke(c.op, c.input)
		wantProblem(t, c.op.ID, pe, problem.CapabilityUnavailable, 503, "")
	}
	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("%d operations, file changed %v", n, string(w.live()) != string(rendered))
	}
}

// add-record, records.update and records.remove run up to 10 min (A-3); a
// unit lost without a result is outcome-unknown naming the rollback copy.
func TestRecordOpsMaxRuntimeAndLostDetail(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	found := 0
	for _, b := range w.p.Bindings() {
		df, ok := b.Detached()
		switch b.Op().ID {
		case spec.AddRecord.Op().ID:
			checkDNSOpLifetime(t, w, "add-record", df, spec.RecordAddInput{FQDN: "a.app.lan", IPv4: "192.168.7.9"})
		case spec.RecordsUpdate.Op().ID:
			checkDNSOpLifetime(t, w, "update-record", df, spec.RecordUpdateInput{Name: "a.app.lan~A", IPv4: "192.168.7.9"})
		case spec.RecordsRemove.Op().ID:
			checkDNSOpLifetime(t, w, "remove-record", df, spec.RecordRemoveInput{Name: "a.app.lan~A"})
		default:
			continue
		}
		if !ok {
			t.Errorf("%s is not detached", b.Op().ID)
		}
		found++
	}
	if found != 3 {
		t.Errorf("%d of add-record, records.update and records.remove bound, want 3", found)
	}
}

// The helper parses the record ID again and refuses one that is not
// <fqdn>~A, writing nothing.
func TestRecordHelperRefusesBadRecordID(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == spec.RecordsRemove.Op().ID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		t.Fatal("records.remove is not bound")
	}
	const id = "op-record-bad-id"
	in := spec.RecordRemoveInput{Name: "kiwi.home.local"}
	if _, err := df.Prepare(context.Background(), in, provider.NewRun(id, nil)); err != nil {
		t.Fatal(err)
	}
	inj := w.inject()
	inj.DNSServer = w.h.FTL().Addr()
	if code := runHelper(context.Background(), filepath.Join(w.opsDir, id), io.Discard, io.Discard, inj); code != 0 {
		t.Fatalf("helper exit %d", code)
	}
	_, err := df.Finalize(context.Background(), in, detach.Exit{}, provider.NewRun(id, nil))
	if slug, _ := slugOf(t, err); slug != problem.ValidationFailed {
		t.Errorf("got %s, want validation-failed", slug)
	}
	if string(w.live()) != string(rendered) {
		t.Error("the DNS file changed")
	}
}

// batch invokes apply-dns-changes with the changes, in order.
func (w *dnsWorld) batch(changes ...string) (store.OpRecord, *problem.Error) {
	w.t.Helper()
	b, err := json.Marshal(spec.DNSBatchInput{Changes: changes})
	if err != nil {
		w.t.Fatal(err)
	}
	return w.invoke(spec.ApplyDNSChanges.Op(), string(b))
}

// configTestRuns counts the `pihole-FTL --test` argvs run since the last
// ResetArgvs.
func (w *dnsWorld) configTestRuns() int {
	n := 0
	for _, a := range w.h.Argvs() {
		if slices.Contains(a, "--test") {
			n++
		}
	}
	return n
}

// wantBatchRejected checks a dns-batch-rejected problem: 422, the index and
// cause as extensions, and a detail that starts "changes[<index>]:".
func wantBatchRejected(t *testing.T, what string, pe *problem.Error, index int, cause problem.Slug) {
	t.Helper()
	wantProblem(t, what, pe, problem.DNSBatchRejected, 422, "")
	// After result.json, changeIndex is a JSON number (float64); compare text.
	if got := fmt.Sprint(pe.Extensions["changeIndex"]); got != strconv.Itoa(index) {
		t.Errorf("%s: changeIndex %v, want %d", what, pe.Extensions["changeIndex"], index)
	}
	if got := fmt.Sprint(pe.Extensions["cause"]); got != string(cause) {
		t.Errorf("%s: cause %q, want %s", what, got, cause)
	}
	if prefix := fmt.Sprintf("changes[%d]:", index); !strings.HasPrefix(pe.Detail, prefix) {
		t.Errorf("%s: detail %q does not start %q", what, pe.Detail, prefix)
	}
}

// TC-115 (FR-26, NFR-3, FR-23): the three FR-23 zones declared in one batch
// leave exactly the FR-23 rendering; zone markers are comment lines, so no
// test, no restart and the same PID.
func TestTC115_FR23ZonesInOneBatch(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.conf"))
	pid, restarts := w.h.FTL().PID(), w.h.FTL().Restarts()

	rec, pe := w.batch("add-zone:home.local", "add-zone:app.lan", "add-zone:xi.example")
	if pe != nil {
		t.Fatalf("batch: %s %q", pe.Slug(), pe.Detail)
	}
	res := succeeded[spec.DNSChangeResult](t, rec)
	if !res.Changed || res.Restarted || len(res.Verified) != 0 || res.FTLPidAfter == nil || *res.FTLPidAfter != pid ||
		res.Etag != etagOf(w.live()) || res.PreNexusBackup != w.p.backupPath(dnsTestFile) {
		t.Errorf("result %+v, want changed, not restarted, pid %d, the pre-Nexus backup named", res, pid)
	}
	if got, want := w.live(), dnsFixture(t, "fr23-seven-lines.rendered.conf"); string(got) != string(want) {
		t.Errorf("file:\n%s\nwant the FR-23 rendering:\n%s", got, want)
	}
	if got, want := w.zoneCounts(), []string{"app.lan=1", "home.local=2", "xi.example=2"}; !slices.Equal(got, want) {
		t.Errorf("zones %v, want %v", got, want)
	}
	if w.ranConfigTest() || w.restartdnsRuns() != 0 || w.h.FTL().Restarts() != restarts || w.h.FTL().PID() != pid {
		t.Errorf("argvs %q, restarts %d -> %d, pid %d -> %d: want no test and no restart", w.h.Argvs(),
			restarts, w.h.FTL().Restarts(), pid, w.h.FTL().PID())
	}
	noLocalLine(t, w.live())
}

// TC-116 (FR-26): a change refused at index 2 of 3 refuses the batch at
// Admit with 422 dns-batch-rejected naming the index and the cause; no
// operation and the file unchanged. The changes are checked in order
// against one model, and a one-change batch reports its refusal the same
// way.
func TestTC116_FailureAtIndex2WritesNothing(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)

	_, pe := w.batch("add-record:a.app.lan=192.168.7.9", "add-record:b.app.lan=192.168.7.10",
		"add-record:c.nozone.example=192.168.7.11")
	wantBatchRejected(t, "index 2", pe, 2, problem.DNSNameUnzoned)

	_, pe = w.batch("add-record:a.app.lan=192.168.7.9", "add-record:a.app.lan=192.168.7.10")
	wantBatchRejected(t, "add after add", pe, 1, problem.Conflict)
	_, pe = w.batch("remove-record:kiwi.home.local", "update-record:kiwi.home.local=192.168.7.9")
	wantBatchRejected(t, "update after remove", pe, 1, problem.NotFound)
	_, pe = w.batch("add-record:c.nozone.example=192.168.7.11")
	wantBatchRejected(t, "one change", pe, 0, problem.DNSNameUnzoned)

	if n := w.opCount(); n != 0 || string(w.live()) != string(rendered) {
		t.Errorf("%d operations, file changed %v", n, string(w.live()) != string(rendered))
	}
}

// TC-117 (FR-26): 100 changes, a zone and 99 records under it, succeed with
// one restart and 99 verified names; 101 and 0 changes are 400
// validation-failed with no operation.
func TestTC117_BatchSizeBounds(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.conf"))
	restarts := w.h.FTL().Restarts()
	changes := []string{"add-zone:app.lan"}
	var names []string
	for i := range 99 {
		name := fmt.Sprintf("n%02d.app.lan", i)
		names = append(names, name)
		changes = append(changes, fmt.Sprintf("add-record:%s=192.168.1.%d", name, i+1))
	}

	rec, pe := w.batch(changes...)
	if pe != nil {
		t.Fatalf("100 changes: %s %q", pe.Slug(), pe.Detail)
	}
	res := succeeded[spec.DNSChangeResult](t, rec)
	if !res.Changed || !res.Restarted {
		t.Errorf("result changed %v restarted %v, want both", res.Changed, res.Restarted)
	}
	if n, r, c := w.restartdnsRuns(), w.h.FTL().Restarts()-restarts, w.configTestRuns(); n != 1 || r != 1 || c != 1 {
		t.Errorf("%d restartdns argvs, %d FTL restarts, %d config tests; want 1, 1 and 1", n, r, c)
	}
	var got []string
	for i, v := range res.Verified {
		got = append(got, v.FQDN)
		if want := fmt.Sprintf("192.168.1.%d", i+1); !v.Passed || !slices.Equal(v.Observed, []string{want}) {
			t.Errorf("verified %+v, want passed with %s", v, want)
		}
	}
	if !slices.Equal(got, names) {
		t.Errorf("verified %d names %v, want the 99 added", len(got), got)
	}
	if ips := w.resolve("n42.app.lan"); !slices.Equal(ips, []string{"192.168.1.43"}) {
		t.Errorf("n42.app.lan answers %v", ips)
	}

	after := w.live()
	_, pe = w.batch(append(changes, "add-record:n99.app.lan=192.168.1.100")...)
	wantProblem(t, "101 changes", pe, problem.ValidationFailed, 400, "")
	_, pe = w.invoke(spec.ApplyDNSChanges.Op(), `{"changes":[]}`)
	wantProblem(t, "0 changes", pe, problem.ValidationFailed, 400, "")
	if n := w.opCount(); n != 1 || string(w.live()) != string(after) {
		t.Errorf("%d operations, file changed %v; want only the 100-change one", n, string(w.live()) != string(after))
	}
}

// TC-118 (FR-26, FR-24, NFR-2): a record add that depends on the batch's
// own zone add, an update and a remove run one test and one restart and
// verify every changed name; the op is destructive, so the CLI asks first.
func TestTC118_DependentChangesOneRestart(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.conf"))
	restarts := w.h.FTL().Restarts()

	rec, pe := w.batch("add-zone:app.lan", "add-record:x.app.lan=192.168.7.9",
		"update-record:photon.app.lan=192.168.7.6", "remove-record:unity.lan")
	if pe != nil {
		t.Fatalf("batch: %s %q", pe.Slug(), pe.Detail)
	}
	res := succeeded[spec.DNSChangeResult](t, rec)
	if !res.Changed || !res.Restarted || res.Etag != etagOf(w.live()) {
		t.Errorf("result %+v, want changed and restarted with the live etag", res)
	}
	if n, r, c := w.restartdnsRuns(), w.h.FTL().Restarts()-restarts, w.configTestRuns(); n != 1 || r != 1 || c != 1 {
		t.Errorf("%d restartdns argvs, %d FTL restarts, %d config tests; want 1, 1 and 1", n, r, c)
	}
	want := map[string][]string{"photon.app.lan": {"192.168.7.6"}, "unity.lan": {}, "x.app.lan": {"192.168.7.9"}}
	var got []string
	for _, v := range res.Verified {
		got = append(got, v.FQDN)
		if !v.Passed || !slices.Equal(v.Expected, want[v.FQDN]) || !slices.Equal(v.Observed, want[v.FQDN]) {
			t.Errorf("verified %+v, want passed with %v", v, want[v.FQDN])
		}
	}
	if !slices.Equal(got, []string{"photon.app.lan", "unity.lan", "x.app.lan"}) {
		t.Errorf("verified %v, want photon.app.lan, unity.lan and x.app.lan", got)
	}
	for name, ips := range want {
		if got := w.resolve(name); !slices.Equal(got, ips) {
			t.Errorf("%s answers %v, want %v", name, got, ips)
		}
	}
	if op := spec.ApplyDNSChanges.Op(); op.Risk != def.RiskDestructive || !op.Mutating() {
		t.Errorf("apply-dns-changes risk %q, want destructive (the CLI asks without --yes)", op.Risk)
	}
}

// TC-E14 (FR-26, NFR-3): a batch of zone changes only is comment-only: no
// test, no restart, the PID unchanged.
func TestTCE14_ZoneOnlyBatchCommentOnly(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, append(slices.Clone(rendered), "\n# nexus:zone old.lan\n"...))
	pid, restarts := w.h.FTL().PID(), w.h.FTL().Restarts()

	rec, pe := w.batch("add-zone:new.lan", "remove-zone:old.lan")
	if pe != nil {
		t.Fatalf("batch: %s %q", pe.Slug(), pe.Detail)
	}
	if res := succeeded[spec.DNSChangeResult](t, rec); !res.Changed || res.Restarted || len(res.Verified) != 0 {
		t.Errorf("result %+v, want changed without a restart", res)
	}
	if got, want := w.zoneCounts(), []string{"app.lan=1", "home.local=2", "new.lan=0", "xi.example=2"}; !slices.Equal(got, want) {
		t.Errorf("zones %v, want %v", got, want)
	}
	if w.ranConfigTest() || w.restartdnsRuns() != 0 || w.h.FTL().Restarts() != restarts || w.h.FTL().PID() != pid {
		t.Errorf("argvs %q, restarts %d -> %d, pid %d -> %d", w.h.Argvs(), restarts, w.h.FTL().Restarts(),
			pid, w.h.FTL().PID())
	}
}

// TC-ERR28 (FR-26): the helper applies the batch to the file as it is under
// the lane; a hand edit after Admit that removes app.lan's marker fails the
// operation with dns-batch-rejected at index 0, cause dns-name-unzoned, and
// nothing written.
func TestTCERR28_HelperBatchRejectionCarriesCause(t *testing.T) {
	t.Parallel()
	rendered := dnsFixture(t, "fr23-seven-lines.rendered.conf")
	w := newDNSWorld(t, rendered)
	var df provider.DetachedFuncs
	for _, b := range w.p.Bindings() {
		if b.Op().ID == spec.ApplyDNSChanges.Op().ID {
			df, _ = b.Detached()
		}
	}
	if df.Prepare == nil {
		t.Fatal("apply-dns-changes is not bound")
	}
	in := spec.DNSBatchInput{Changes: []string{"add-record:x.app.lan=192.168.7.9"}}
	if err := df.Admit(context.Background(), in); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	const id = "op-batch-unzoned"
	if _, err := df.Prepare(context.Background(), in, provider.NewRun(id, nil)); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(rendered), "# nexus:zone app.lan\n", "", 1)
	if edited == string(rendered) {
		t.Fatal("the fixture has no app.lan marker")
	}
	if err := os.WriteFile(w.file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	inj := w.inject()
	inj.DNSServer = w.h.FTL().Addr()
	if code := runHelper(context.Background(), filepath.Join(w.opsDir, id), io.Discard, io.Discard, inj); code != 0 {
		t.Fatalf("helper exit %d", code)
	}
	_, err := df.Finalize(context.Background(), in, detach.Exit{}, provider.NewRun(id, nil))
	_, pe := slugOf(t, err)
	wantBatchRejected(t, "helper", pe, 0, problem.DNSNameUnzoned)
	if string(w.live()) != edited {
		t.Error("the DNS file changed")
	}
}

// parseBatch refuses, in the helper as at Admit, a change list outside 1 to
// 100 as validation-failed, and an item the grammar refuses (here an
// ASM-9 blocking address the schema would refuse) as dns-batch-rejected
// at its index.
func TestParseBatchBounds(t *testing.T) {
	for _, n := range []int{0, spec.MaxChanges + 1} {
		items := slices.Repeat([]string{"add-zone:a.lan"}, n)
		if _, pe := parseBatch(items); pe == nil || pe.Slug() != problem.ValidationFailed {
			t.Errorf("%d changes: %v, want validation-failed", n, pe)
		}
	}
	_, pe := parseBatch([]string{"add-zone:a.lan", "add-record:x.a.lan=0.0.0.0"})
	wantBatchRejected(t, "0.0.0.0", pe, 1, problem.ValidationFailed)
	cs, pe := parseBatch([]string{"add-zone:a.lan", "remove-zone-cascade:b.lan", "update-record:x.a.lan=192.168.7.9"})
	want := []dnsfile.Change{dnsfile.AddZone{Zone: "a.lan"}, dnsfile.RemoveZone{Zone: "b.lan", Cascade: true},
		dnsfile.UpdateRecord{FQDN: "x.a.lan", IPv4: "192.168.7.9"}}
	if pe != nil || !slices.Equal(cs, want) {
		t.Errorf("parse: %v %v, want %v", cs, pe, want)
	}
}

// TC-119 (FR-5, FR-27, CON-1): with every work item composed, the provider
// binds all 26 declarations: the 11 reads Sync, the 15 mutations Detached on
// the FIFO pihole lane with KillMode=process, MaxRuntime 1 h when they run
// gravity and 10 min otherwise, and a LostDetail; the DNS writes' names the
// rollback copy and the blocking ops' the re-enable timer.
func TestTC119_BindingInventory(t *testing.T) {
	t.Parallel()
	w := newHelperWorld(t)
	dnsOps := map[string]bool{}
	for _, op := range []def.Op{spec.AddZone.Op(), spec.ZonesRemove.Op(), spec.AddRecord.Op(), spec.RecordsUpdate.Op(),
		spec.RecordsRemove.Op(), spec.ApplyDNSChanges.Op()} {
		dnsOps[op.ID] = true
	}
	blockingOps := map[string]bool{spec.StopBlocking.Op().ID: true, spec.StartBlocking.Op().ID: true}
	// The inputs whose MaxRuntime is 1 h: update-gravity always, and an
	// adlist change that runs gravity.
	type probe struct {
		input   string
		runtime time.Duration
	}
	probes := map[string][]probe{
		spec.UpdateGravity.Op().ID: {{`{}`, time.Hour}},
		spec.AddAdlist.Op().ID:     {{`{}`, 10 * time.Minute}, {`{"updateGravity":true}`, time.Hour}},
		spec.RemoveAdlist.Op().ID:  {{`{}`, 10 * time.Minute}, {`{"updateGravity":true}`, time.Hour}},
	}

	bindings := w.p.Bindings()
	bound := map[string]bool{}
	reads, mutations := 0, 0
	for i, b := range bindings {
		op := b.Op()
		if bound[op.ID] {
			t.Errorf("%s is bound twice", op.ID)
		}
		bound[op.ID] = true
		if !op.Mutating() {
			reads++
			if b.Kind() != provider.BindSync {
				t.Errorf("read %s is %s, want sync", op.ID, b.Kind())
			}
			continue
		}
		mutations++
		df, ok := b.Detached()
		if !ok || b.Kind() != provider.BindDetached {
			t.Errorf("mutation %s is %s, want detached", op.ID, b.Kind())
			continue
		}
		ps, found := probes[op.ID]
		if !found {
			ps = []probe{{`{}`, 10 * time.Minute}}
		}
		for j, pr := range ps {
			in, err := b.Decode(json.RawMessage(pr.input))
			if err != nil {
				t.Fatalf("%s: decode %s: %v", op.ID, pr.input, err)
			}
			lane, err := df.Lane(context.Background(), in)
			if err != nil || lane != (provider.Lane{Key: LaneKey, Policy: provider.LaneQueue}) {
				t.Errorf("%s: lane %+v %v, want {pihole, queue}", op.ID, lane, err)
			}
			run := provider.NewRun(fmt.Sprintf("op-inv-%d-%d", i, j), nil)
			s, err := df.Prepare(context.Background(), in, run)
			if err != nil {
				t.Fatalf("%s: Prepare: %v", op.ID, err)
			}
			if s.KillMode != detach.KillProcess || s.MaxRuntime != pr.runtime {
				t.Errorf("%s %s: KillMode %q MaxRuntime %s, want process and %s", op.ID, pr.input, s.KillMode,
					s.MaxRuntime, pr.runtime)
			}
			lost := ""
			if df.LostDetail != nil {
				lost = df.LostDetail(run, in)
			}
			switch {
			case lost == "":
				t.Errorf("%s: no LostDetail", op.ID)
			case dnsOps[op.ID] && !strings.Contains(lost, dnswrite.RollbackCopyName):
				t.Errorf("%s: LostDetail %q does not name %s", op.ID, lost, dnswrite.RollbackCopyName)
			case blockingOps[op.ID] && !strings.Contains(lost, "nexus-pihole-reenable.timer"):
				t.Errorf("%s: LostDetail %q does not name the re-enable timer", op.ID, lost)
			}
		}
	}
	if len(bindings) != 26 || reads != 11 || mutations != 15 {
		t.Errorf("%d bindings, %d reads, %d mutations; want 26, 11 and 15", len(bindings), reads, mutations)
	}
	for _, op := range spec.Spec().Ops {
		if !bound[op.ID] {
			t.Errorf("%s is declared but not bound", op.ID)
		}
	}
}

// CR-5: the helper's readiness window reaches the DNS pipeline, for the
// change's restart and for the rollback's. FTL never answers after the
// restart, so the write is rolled back, and the rollback's restart does not
// answer either: dns-rollback-failed, within the test's 300 ms windows
// instead of two production ones of 15 s.
func TestDNSWriteReadinessWindowReachesThePipeline(t *testing.T) {
	t.Parallel()
	w := newDNSWorld(t, dnsFixture(t, "fr23-seven-lines.rendered.conf"))
	w.readyWindow = 300 * time.Millisecond
	w.h.FTL().SetFault(v5test.FaultNeverReady)
	rec, pe := w.invoke(spec.AddRecord.Op(), `{"fqdn":"seam.app.lan","ipv4":"192.168.7.9"}`)
	if pe != nil {
		t.Fatalf("refused at Admit: %s %q", pe.Slug(), pe.Detail)
	}
	if rec.Status != apitypes.StatusFailed || rec.Error == nil || rec.Error.Slug() != problem.DNSRollbackFailed {
		t.Fatalf("got %s %+v, want failed with dns-rollback-failed", rec.Status, rec.Error)
	}
}
