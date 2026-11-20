// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

// Tests of planning/nexusd-remote-admin/test-cases.md TC-RA-84 to TC-RA-87
// (WI 4.4): config set end to end at T0. The REST rows run on the config-set
// stack (configSetStack, stack_test.go): the real server, Dispatcher, ops
// manager and detach launcher, whose Launch runs the real service helper in
// process on F-SYS with F-RELOAD. TC-RA-85's lane needs operations that stay
// Running, so it runs on the launch spy's stack instead.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/detach"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
	"github.com/mauktik/nexus/internal/systemspec"
)

// loopbackTLSOff is a valid file on loopback with TLS off, on which
// tls.mode=auto is a restart-required change.
const loopbackTLSOff = "listen: [\"127.0.0.1:7443\"]\ntls:\n  mode: \"off\"\nproviders:\n  nexusd:\n    enabled: true\n"

// tlsLockout is the lock-out warning of a tls.mode change (ASM-3).
const tlsLockout = "tls.mode takes effect when nexusd restarts; if it changes how this client connects " +
	"(certificate or pin), you may be locked out until a root login on the host fixes it"

// runOp POSTs a config set that changes something, waits for its operation
// and returns it with the helper's output on failure.
func (c *configSetStack) runOp(t *testing.T, changes ...string) apitypes.Operation {
	t.Helper()
	id := accepted(t, c.set(t, c.admin, nil, changes...))
	op := c.awaitOp(t, id)
	if n := c.spy.count(); n == 0 {
		t.Fatalf("operation %s ended without a launch", id)
	}
	if t.Failed() {
		t.Logf("helper output:\n%s", c.helperOutput())
	}
	return op
}

// wantSucceeded wants a succeeded operation and returns its result.
func wantSucceeded(t *testing.T, c *configSetStack, op apitypes.Operation) spec.ConfigUpdateResult {
	t.Helper()
	if op.Status != apitypes.StatusSucceeded {
		t.Fatalf("operation %s %+v, want succeeded\nhelper output:\n%s", op.Status, op.Error, c.helperOutput())
	}
	return decodeJSON[spec.ConfigUpdateResult](t, op.Result)
}

// wantFailed wants a failed operation with slug.
func wantFailed(t *testing.T, c *configSetStack, op apitypes.Operation, slug problem.Slug) *problem.Error {
	t.Helper()
	if op.Status != apitypes.StatusFailed || op.Error == nil || op.Error.Slug() != slug {
		t.Fatalf("operation %s %+v, want failed %s\nhelper output:\n%s", op.Status, op.Error, slug,
			c.helperOutput())
	}
	return op.Error
}

// editOf is config.Edit's output for one assignment.
func editOf(t *testing.T, data []byte, path, key, value string) []byte {
	t.Helper()
	out, _, err := config.Edit(data, path, []config.Assignment{{Key: key, Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TC-RA-84: The T0 end-to-end matrix holds the FR-8 post-condition for every outcome. Owner WI 4.4. Requirements: FR-7, FR-8, NFR-4, NFR-7.
func TestTCRA84_TheT0EndToEndMatrixHoldsTheFR8PostConditionFor(t *testing.T) {
	t.Run("a success", func(t *testing.T) {
		data := defaultConfigWithNexusd(t)
		c := newConfigSetStack(t, data, stackConfig{})
		op := c.runOp(t, "providers.host.enabled=true")
		res := wantSucceeded(t, c, op)
		if !res.Changed || res.RestartRequired == nil || len(res.RestartRequired) != 0 ||
			!strings.Contains(string(op.Result), `"restartRequired":[]`) || len(res.Warnings) != 0 {
			t.Errorf("result %s, want changed, restartRequired [] and no warnings", op.Result)
		}
		if res.BackupPath != c.cfgPath+".bak" || !bytes.Equal(readBytes(t, res.BackupPath), data) {
			t.Errorf("backupPath %q, want %s.bak holding the original bytes", res.BackupPath, c.cfgPath)
		}
		if want := editOf(t, data, c.cfgPath, "providers.host.enabled", "true"); !bytes.Equal(readBytes(t,
			c.cfgPath), want) {
			t.Error("the disk does not hold config.Edit's output")
		}
		if n := c.spy.count(); n != 1 {
			t.Errorf("launches %d, want 1", n)
		}
		if n := c.reloadCalls(); n != 1 {
			t.Errorf("reloadCalls %d, want 1", n)
		}
		c.wantPC(t, op)
	})

	t.Run("b success with restart required", func(t *testing.T) {
		c := newConfigSetStack(t, []byte(loopbackTLSOff), stackConfig{})
		op := c.runOp(t, "tls.mode=auto")
		res := wantSucceeded(t, c, op)
		if !res.Changed || !slices.Equal(res.RestartRequired, []string{"tls"}) {
			t.Errorf("result %s, want changed and restartRequired [tls]", op.Result)
		}
		if want := []string{tlsLockout, "run nexus nexusd restart to apply: tls"}; !slices.Equal(res.Warnings, want) {
			t.Errorf("warnings\n got %q\nwant %q", res.Warnings, want)
		}
		if len(res.Changes) != 1 || res.Changes[0].Key != "tls.mode" || res.Changes[0].Old != "off" ||
			res.Changes[0].New != "auto" {
			t.Errorf("changes %+v, want tls.mode off -> auto", res.Changes)
		}
		c.wantPC(t, op)
	})

	// CR-11: the daemon compares each reload with the configuration it
	// started with, which a reload never replaces, and sorts the keys. The
	// fake's rule must too, or a restart-only change that "went away" after
	// a second set, or a key order the daemon never produces, passes here.
	t.Run("b2 restart-only keys persist across sets and are sorted", func(t *testing.T) {
		c := newConfigSetStack(t, defaultConfigWithNexusd(t), stackConfig{})
		res := wantSucceeded(t, c, c.runOp(t, "tls.mode=off", "limits.outputCapBytes=20971520"))
		if want := []string{"limits", "tls"}; !slices.Equal(res.RestartRequired, want) {
			t.Errorf("restartRequired %v, want %v, sorted", res.RestartRequired, want)
		}
		res = wantSucceeded(t, c, c.runOp(t, "providers.host.enabled=true"))
		if want := []string{"limits", "tls"}; !slices.Equal(res.RestartRequired, want) {
			t.Errorf("after a second set restartRequired %v, want %v: the running daemon still has the "+
				"startup values", res.RestartRequired, want)
		}
	})

	t.Run("c validation refusal", func(t *testing.T) {
		data := defaultConfigWithNexusd(t)
		c := newConfigSetStack(t, data, stackConfig{})
		errs := c.invalid(t, c.set(t, c.admin, nil, "limits.outputCapBytes=0"))
		if len(errs) == 0 || errs[0].Path != "limits.outputCapBytes" {
			t.Errorf("errors %+v, want errors[0] at limits.outputCapBytes", errs)
		}
		c.wantNoSideEffects(t, shaHex(data))
		if n := c.reloadCalls(); n != 0 {
			t.Errorf("reloadCalls %d, want 0", n)
		}
	})

	t.Run("d conflict at the helper", func(t *testing.T) {
		data := defaultConfigWithNexusd(t)
		c := newConfigSetStack(t, data, stackConfig{})
		other := editOf(t, data, c.cfgPath, "host.name", "edited-by-hand")
		c.withFake(func(_ *fakeSystem, rl *fakeReload) { rl.replaceBefore = other })
		op := c.runOp(t, "providers.host.enabled=true")
		perr := wantFailed(t, c, op, problem.ConfigConflict)
		if perr.Extensions["restored"] == true {
			t.Errorf("problem %+v, want no restore", perr)
		}
		if !bytes.Equal(readBytes(t, c.cfgPath), other) {
			t.Error("the disk does not hold the replacing file")
		}
		if n := c.reloadCalls(); n != 1 {
			t.Errorf("reloadCalls %d, want 1: a conflict is never restored", n)
		}
		c.wantPC(t, op)
	})

	t.Run("h conflict at Prepare", func(t *testing.T) {
		// TC-RA-E22's direct-call method: the file must change between
		// admission and Prepare, which REST cannot step between.
		c := newCSDirect(t)
		in := c.admit(t, "providers.host.enabled=true")
		other := c.rig.otherValid(t)
		if err := os.WriteFile(c.rig.cfgPath, other, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := c.funcs.Prepare(t.Context(), in, c.run)
		if perr := problemOfErr(t, err); perr.Slug() != problem.ConfigConflict {
			t.Errorf("Prepare = %s %q, want config-conflict", perr.Slug(), perr.Detail)
		}
		// No spec means no unit, so the helper never runs.
		c.wantNothingPrepared(t, s, shaHex(other))
		if _, err := os.Lstat(filepath.Join(c.rig.dir, resultFileName)); err == nil {
			t.Error("result.json exists: the helper ran")
		}
	})

	t.Run("e refused reload then restored", func(t *testing.T) {
		data := defaultConfigWithNexusd(t)
		c := newConfigSetStack(t, data, stackConfig{})
		c.withFake(func(_ *fakeSystem, rl *fakeReload) { rl.refuseApply = 1 })
		op := c.runOp(t, "providers.host.enabled=true")
		perr := wantFailed(t, c, op, problem.ConfigApplyFailed)
		if perr.Extensions["restored"] != true {
			t.Errorf("problem %+v, want restored true", perr)
		}
		if !bytes.Equal(readBytes(t, c.cfgPath), data) {
			t.Error("the disk does not hold the original bytes")
		}
		if n := c.reloadCalls(); n != 2 {
			t.Errorf("reloadCalls %d, want 2: the change's and the restore's", n)
		}
		c.wantPC(t, op)
	})

	t.Run("f restore failed", func(t *testing.T) {
		c := newConfigSetStack(t, defaultConfigWithNexusd(t), stackConfig{})
		c.withFake(func(_ *fakeSystem, rl *fakeReload) { rl.noAnswer = 2 })
		op := c.runOp(t, "providers.host.enabled=true")
		perr := wantFailed(t, c, op, problem.ConfigRestoreFailed)
		if perr.Extensions["backupPath"] != c.cfgPath+".bak" {
			t.Errorf("backupPath %v, want %s.bak", perr.Extensions["backupPath"], c.cfgPath)
		}
		if !strings.Contains(perr.Detail, "docs/install.md#recovering-from-a-failed-configuration-change") {
			t.Errorf("detail %q lacks the recovery section", perr.Detail)
		}
		// The PC is exempt for config-restore-failed; wantPC says so.
		c.wantPC(t, op)
	})

	t.Run("g no change", func(t *testing.T) {
		data := defaultConfigWithNexusd(t)
		c := newConfigSetStack(t, data, stackConfig{})
		c.noChange(t, c.set(t, c.admin, nil, "providers.nexusd.enabled=true"))
		if names := opsEntries(t, c.opsDir); len(names) != 0 {
			t.Errorf("ops dir holds %v, want nothing", names)
		}
		c.wantNoSideEffects(t, shaHex(data))
		if n := c.reloadCalls(); n != 0 {
			t.Errorf("reloadCalls %d, want 0", n)
		}
		if n := c.statusesWritten(); n != 1 {
			t.Errorf("reload statuses %d, want only the baseline's", n)
		}
		if got := outcomes(c.auditOf(t, spec.UpdateConfig.Op().ID)); !slices.Equal(got,
			[]string{audit.OutcomeRequested, audit.OutcomeCompleted}) {
			t.Errorf("audit %v, want requested then completed", got)
		}
	})
}

// TC-RA-85: The lane serializes config set with reload, restart, update and rollback in both directions. Owner WI 4.4. Requirements: FR-9, FR-7.
func TestTCRA85_TheLaneSerializesConfigSetWithReloadRestartUpdate(t *testing.T) {
	setRunningVersion(t, "v9.9.1")
	k := pkgKey(seedTrusted)
	u := newUpdateStack(t, updateStackConfig{keys: []ed25519.PrivateKey{k}})
	writeScript(t, u.paths.Prev, `case "$1" in validate-config) exit 0;; -version) echo "nexusd v9.9.0";; esac`)
	pkg := updatePackage(t, "v9.9.2", "v9.9.2")
	cfgPath := filepath.Join(t.TempDir(), "nexusd.yaml")
	if err := os.WriteFile(cfgPath, defaultConfigWithNexusd(t), 0o600); err != nil {
		t.Fatal(err)
	}
	u.daemon.setConfigPath(cfgPath)
	c := &csStack{stack: u.stack, spy: u.spy, cfgPath: cfgPath}

	var mu sync.Mutex
	var release chan struct{}
	running := make(chan struct{}, 1)
	u.daemon.onReload(func(context.Context) (provider.ReloadResult, error) {
		mu.Lock()
		rel := release
		mu.Unlock()
		running <- struct{}{}
		<-rel
		return provider.ReloadResult{}, nil
	})
	// hold makes the next reload block until the returned func is called.
	hold := func() func() {
		mu.Lock()
		defer mu.Unlock()
		release = make(chan struct{})
		rel := release
		var once sync.Once
		unblock := func() { once.Do(func() { close(rel) }) }
		t.Cleanup(unblock)
		return unblock
	}
	post := func(name string) response {
		switch name {
		case "update":
			return u.update(t, updateOf(u.stage(t, pkg, k), false))
		case "change":
			return c.set(t, c.admin, nil, "providers.host.enabled=true")
		case "no-change":
			return c.set(t, c.admin, nil, "providers.nexusd.enabled=true")
		}
		return u.action(t, name, u.admin, nil)
	}
	// start starts a lane op and waits until it is running.
	start := func(name string) (id string, end func()) {
		t.Helper()
		var unblock func()
		if name == "reload" {
			unblock = hold()
		}
		id = accepted(t, post(name))
		if name == "reload" {
			select {
			case <-running:
			case <-time.After(10 * time.Second):
				t.Fatal("the reload did not start")
			}
			return id, func() {
				unblock()
				if op := u.awaitOp(t, id); op.Status != apitypes.StatusSucceeded {
					t.Fatalf("reload %s", op.Status)
				}
			}
		}
		u.spy.await(t, id)
		return id, func() {
			now := time.Now()
			if err := detach.WriteExit(filepath.Join(u.opsDir, id), detach.Exit{Started: now, Ended: now}); err != nil {
				t.Fatal(err)
			}
			if op := u.awaitOp(t, id); !op.Status.Terminal() {
				t.Fatalf("%s did not end", name)
			}
		}
	}
	conflict := func(what, req, blocker string) {
		t.Helper()
		body := problemOf(t, post(req).want(t, http.StatusConflict).body)
		if slugOf(body) != string(problem.Conflict) || body["blockingOperationId"] != blocker {
			t.Errorf("%s, request %s: %v, want conflict naming %s", what, req, body, blocker)
		}
	}
	configRecs := func() []systemspec.AuditRecord { return c.auditOf(t, spec.UpdateConfig.Op().ID) }
	count := func(recs []systemspec.AuditRecord, outcome string) int {
		n := 0
		for _, r := range recs {
			if r.Outcome == outcome {
				n++
			}
		}
		return n
	}
	launches, started := 0, 0

	// Step 1.
	for _, b := range []string{"reload", "restart", "update", "rollback"} {
		hid, end := start(b)
		started++
		if b != "reload" {
			launches++
		}
		before := configRecs()
		conflict("blocker "+b, "change", hid)
		conflict("blocker "+b, "no-change", hid)
		after := configRecs()[len(before):]
		if got := outcomes(after); !slices.Equal(got, []string{"refused:" + string(problem.Conflict),
			"refused:" + string(problem.Conflict)}) {
			t.Errorf("blocker %s: config set records %v, want two refused conflicts", b, got)
		}
		end()
		if n := u.spy.count(); n != launches {
			t.Errorf("blocker %s: launches %d, want %d", b, n, launches)
		}
	}

	// Step 2.
	cid, end := start("change")
	started++
	launches++
	for _, req := range []string{"reload", "restart", "update", "rollback", "change", "no-change"} {
		conflict("config set running", req, cid)
	}
	if n := u.spy.count(); n != launches {
		t.Errorf("step 2: launches %d, want %d", n, launches)
	}
	end()

	// Step 3.
	completed := count(configRecs(), audit.OutcomeCompleted)
	for _, b := range []string{"reload", "restart", "update", "rollback"} {
		c.noChange(t, post("no-change"))
		_, end := start(b)
		started++
		if b != "reload" {
			launches++
		}
		end()
	}
	recs := configRecs()
	if n := count(recs, audit.OutcomeCompleted) - completed; n != 4 {
		t.Errorf("step 3: %d completed config set records, want 4", n)
	}

	// No side effects.
	if n := u.spy.count(); n != launches {
		t.Errorf("launches %d, want only the %d ops started on purpose", n, launches)
	}
	if ops := u.operations(t); len(ops) != started {
		t.Errorf("operations %d, want only the %d started on purpose", len(ops), started)
	}
	// Refused: two per blocker in step 1, two config sets in step 2; the
	// only completed no-change records are step 3's.
	if n := count(recs, audit.OutcomeRefused); n != 10 {
		t.Errorf("refused config set records %d, want 10: %v", n, outcomes(recs))
	}
	if n := count(recs, audit.OutcomeCompleted); n != 4 {
		t.Errorf("completed config set records %d, want step 3's 4: %v", n, outcomes(recs))
	}
}

// trail is one request's audit records: the requested record and every
// record that answers it or belongs to its operation.
func trail(recs []systemspec.AuditRecord, req systemspec.AuditRecord) []systemspec.AuditRecord {
	out := []systemspec.AuditRecord{req}
	opID := ""
	for _, r := range recs {
		if r.ID == req.ID {
			continue
		}
		if r.RequestID == req.ID || (opID != "" && r.OperationID == opID) {
			out = append(out, r)
			if r.OperationID != "" {
				opID = r.OperationID
			}
		}
	}
	return out
}

// TC-RA-86: A config set leaves a complete audit trail. Owner WI 4.4. Requirements: FR-13.
func TestTCRA86_AConfigSetLeavesACompleteAuditTrail(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newConfigSetStack(t, data, stackConfig{})
	bak := c.cfgPath + ".bak"

	// Step 1: rows (a), (e) and (g).
	wantSucceeded(t, c, c.runOp(t, "providers.host.enabled=true"))
	c.withFake(func(_ *fakeSystem, rl *fakeReload) { rl.refuseApply = 1 })
	wantFailed(t, c, c.runOp(t, "limits.outputCapBytes=20971520"), problem.ConfigApplyFailed)
	c.noChange(t, c.set(t, c.admin, nil, "providers.host.enabled=true"))

	// Step 2.
	recs := c.auditOf(t, spec.UpdateConfig.Op().ID)
	var reqs []systemspec.AuditRecord
	for _, r := range recs {
		if r.Outcome == audit.OutcomeRequested {
			reqs = append(reqs, r)
		}
	}
	if len(reqs) != 3 {
		t.Fatalf("requested records %d, want 3: %v", len(reqs), outcomes(recs))
	}
	queued, running := string(apitypes.StatusQueued), string(apitypes.StatusRunning)
	for i, row := range []struct {
		name     string
		change   string
		old      map[string]string
		outcomes []string
	}{
		{"a", "providers.host.enabled=true", map[string]string{"old.providers.host.enabled": "false"},
			[]string{audit.OutcomeRequested, queued, running, string(apitypes.StatusSucceeded)}},
		{"e", "limits.outputCapBytes=20971520", map[string]string{"old.limits.outputCapBytes": "10485760"},
			[]string{audit.OutcomeRequested, queued, running,
				string(apitypes.StatusFailed) + ":" + string(problem.ConfigApplyFailed)}},
		{"g", "providers.host.enabled=true", map[string]string{"old.providers.host.enabled": "true"},
			[]string{audit.OutcomeRequested, audit.OutcomeCompleted}},
	} {
		tr := trail(recs, reqs[i])
		if got := outcomes(tr); !slices.Equal(got, row.outcomes) {
			t.Errorf("row %s: trail %v, want %v", row.name, got, row.outcomes)
			continue
		}
		var params struct {
			Changes []string `json:"changes"`
		}
		if err := json.Unmarshal(tr[0].Params, &params); err != nil || !slices.Equal(params.Changes,
			[]string{row.change}) {
			t.Errorf("row %s: requested params %s (%v), want the new value %s", row.name, tr[0].Params, err,
				row.change)
		}
		last := tr[len(tr)-1]
		if row.name == "g" {
			want := map[string]string{"changed": "false"}
			for k, v := range row.old {
				want[k] = v
			}
			if !maps.Equal(last.Facts, want) || last.RequestID != tr[0].ID || last.OperationID != "" {
				t.Errorf("row g: completed %+v, want facts %v, linked by requestId, no operationId", last, want)
			}
			if _, has := last.Facts[factBackupPath]; has {
				t.Errorf("row g: completed facts %v carry a backupPath", last.Facts)
			}
			continue
		}
		want := map[string]string{factBackupPath: bak}
		for k, v := range row.old {
			want[k] = v
		}
		if !maps.Equal(tr[0].Facts, want) {
			t.Errorf("row %s: requested facts %v, want %v", row.name, tr[0].Facts, want)
		}
		// The ops manager writes the state records without facts
		// (architecture.md WI 4.4: requested carries the facts, then
		// Queued, Running and the terminal state with the slug), so the
		// terminal record names the backup path through the trail: its
		// operationId is the Queued record's, whose requestId is the
		// requested record carrying backupPath.
		if tr[1].RequestID != tr[0].ID || tr[1].OperationID == "" || last.OperationID != tr[1].OperationID {
			t.Errorf("row %s: queued requestId %q operationId %q, terminal operationId %q; want linked", row.name,
				tr[1].RequestID, tr[1].OperationID, last.OperationID)
		}
	}
}

// commentsAt are the comments at one key path, key and value merged.
type commentsAt struct{ Head, Line, Foot string }

// yamlComments maps each key path of data, plus "<doc>" and "" for the
// document and root mapping, to its comments, leaving out paths without
// any (as internal/config's commentMap does for NFR-3).
func yamlComments(t *testing.T, data []byte) map[string]commentsAt {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	m := map[string]commentsAt{}
	put := func(path string, ns ...*yaml.Node) {
		var c commentsAt
		for _, n := range ns {
			c.Head += n.HeadComment
			c.Line += n.LineComment
			c.Foot += n.FootComment
		}
		if c != (commentsAt{}) {
			m[path] = c
		}
	}
	put("<doc>", &doc)
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				p := n.Content[i].Value
				if path != "" {
					p = path + "." + p
				}
				put(p, n.Content[i], n.Content[i+1])
				walk(n.Content[i+1], p)
			}
		case yaml.SequenceNode:
			for i, e := range n.Content {
				p := path + "[" + strconv.Itoa(i) + "]"
				put(p, e)
				walk(e, p)
			}
		}
	}
	if len(doc.Content) > 0 {
		put("", doc.Content[0])
		walk(doc.Content[0], "")
	}
	return m
}

// TC-RA-87: Through the full path, the shipped default file changes by one line. Owner WI 4.4. Requirements: NFR-3.
func TestTCRA87_ThroughTheFullPathTheShippedDefaultFileChangesBy(t *testing.T) {
	data := defaultConfigWithNexusd(t)
	c := newConfigSetStack(t, data, stackConfig{})

	// Step 1.
	op := c.runOp(t, "providers.host.enabled=true")
	res := wantSucceeded(t, c, op)
	c.wantPC(t, op)

	disk, bak := readBytes(t, c.cfgPath), readBytes(t, res.BackupPath)
	if !bytes.Equal(bak, data) {
		t.Fatal(".bak does not hold the shipped default file")
	}
	dl, bl := strings.Split(string(disk), "\n"), strings.Split(string(bak), "\n")
	if len(dl) != len(bl) {
		t.Fatalf("line count %d -> %d, want the same", len(bl), len(dl))
	}
	var diff []int
	for i := range dl {
		if dl[i] != bl[i] {
			diff = append(diff, i+1)
		}
	}
	if len(diff) != 1 {
		t.Fatalf("lines %v differ, want exactly 1", diff)
	}
	line := dl[diff[0]-1]
	if !strings.Contains(line, "enabled: true") || !strings.Contains(bl[diff[0]-1], "enabled: false") {
		t.Errorf("line %d %q -> %q, want providers.host.enabled false -> true", diff[0], bl[diff[0]-1], line)
	}
	if got, want := yamlComments(t, disk), yamlComments(t, bak); !maps.Equal(got, want) {
		t.Errorf("comment maps differ:\n got %v\nwant %v", got, want)
	}
	if len(yamlComments(t, bak)) == 0 {
		t.Error("the shipped file has no comments: the comment check is vacuous")
	}
}
