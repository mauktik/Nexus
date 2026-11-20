// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/apt"
	dockerspec "github.com/mauktik/nexus/internal/providers/docker/spec"
)

const (
	nfrSamples   = 200
	nfrReadLimit = 200 * time.Millisecond
	nfrAckLimit  = 500 * time.Millisecond
	nfrGateSock  = "/run/nexus-nfr-docker-gate.sock"
)

// nfrRead is one read op and the path of its i-th sample.
type nfrRead struct {
	op   string
	path func(i int) string
}

func fixed(p string) func(int) string { return func(int) string { return p } }

// nfrReads measures each read samples times, sequentially on one
// keep-alive client, and fails any p95 at or over the read target.
func nfrReads(t *testing.T, n *nexusdInstance, label string, samples int, reads []nfrRead) map[string]nfrStats {
	t.Helper()
	out := map[string]nfrStats{}
	for _, r := range reads {
		d := make([]time.Duration, 0, samples)
		for i := range samples {
			took, code, b := n.timed(http.MethodGet, r.path(i), "")
			if code != http.StatusOK {
				t.Fatalf("%s %s: GET %s: %d %s", label, r.op, r.path(i), code, b)
			}
			d = append(d, took)
		}
		s := nfrSummarize(d)
		out[r.op] = s
		verdict := "PASS"
		if s.p95 >= nfrReadLimit {
			verdict = "MISS"
			t.Errorf("%s: %s read p95 %sms, want under 200 ms", label, r.op, ms(s.p95))
		}
		t.Logf("NFR %s read %-26s %s (target p95<200ms: %s)", label, r.op, s, verdict)
	}
	return out
}

// nfrAcks posts a long-running op samples times and times each 202; each
// operation runs to Succeeded before the next sample, so no lane or queue
// is in play. It returns the operation IDs.
func nfrAcks(t *testing.T, n *nexusdInstance, label, op string, samples int, at func(i int) string, body string) (nfrStats, []string) {
	t.Helper()
	d := make([]time.Duration, 0, samples)
	var ids []string
	for i := range samples {
		took, code, b := n.timed(http.MethodPost, at(i), body)
		// The previous operation already reads Succeeded, so it no longer
		// holds its lane: a 409 naming it is the ops-lane-release-race
		// regression, never retried.
		if code == http.StatusConflict && len(ids) > 0 &&
			strings.Contains(string(b), `"blockingOperationId":"`+ids[len(ids)-1]+`"`) {
			prev := n.operation(ids[len(ids)-1])
			t.Fatalf("%s %s: sample %d: 409 naming the previous operation %s, which reads %s (ended %v): %s",
				label, op, i, prev.ID, prev.Status, prev.Ended, b)
		}
		var ref opRef
		if err := json.Unmarshal(b, &ref); err != nil || code != http.StatusAccepted || ref.ID == "" {
			t.Fatalf("%s %s: POST %s: %d %s", label, op, at(i), code, b)
		}
		d = append(d, took)
		ids = append(ids, ref.ID)
		if end := n.waitOp(ref.ID, 3*time.Minute); end.Status != apitypes.StatusSucceeded {
			t.Fatalf("%s %s: operation %s ended %s %+v", label, op, ref.ID, end.Status, end.Error)
		}
	}
	s := nfrSummarize(d)
	verdict := "PASS"
	if s.p95 >= nfrAckLimit {
		verdict = "MISS"
		t.Errorf("%s: %s acknowledgement p95 %sms, want under 500 ms", label, op, ms(s.p95))
	}
	t.Logf("NFR %s ack  %-26s %s (target p95<500ms: %s)", label, op, s, verdict)
	return s, ids
}

// nfrAudited requires an audit record for each operation ID: the 202 was
// sent after the record was appended and fsynced.
func nfrAudited(t *testing.T, n *nfrDaemon, ids []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(n.auditDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for _, id := range ids {
		if !strings.Contains(string(b), `"`+id+`"`) {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d acknowledged operations have no audit record", missing, len(ids))
	}
	t.Logf("audit: %d of %d acknowledged operations recorded in audit.jsonl", len(ids)-missing, len(ids))
}

func containerAt(names []string) func(int) string {
	return func(i int) string { return containersAt + "/" + names[i%len(names)] }
}

// TC-189: 200 samples per read and per acknowledgement with 50 containers
// and every provider enabled; the cold apt.upgradable.list is reported on
// its own (A-38).
func TestNFRLatency(t *testing.T) {
	requireApt(t)
	nfrLoad(t)
	containers := nfrContainers(t)
	n := nfrSetup(t, "nfr-latency", dockerSocket, "")

	cold, code, b := n.timed(http.MethodGet, upgradableAt, "")
	if code != http.StatusOK {
		t.Fatalf("upgradable: %d %s", code, b)
	}
	var page apitypes.List[json.RawMessage]
	n.mustGet(containersAt+"?limit=1000", &page)
	if len(page.Items) < nfrContainerCount {
		t.Fatalf("the inventory lists %d containers, want at least %d", len(page.Items), nfrContainerCount)
	}
	ref := startOp(t, n.nexusdInstance, scriptsAt+"/ok/invoke", "{}")
	if end := n.waitOp(ref.ID, time.Minute); end.Status != apitypes.StatusSucceeded {
		t.Fatalf("script: %s %+v", end.Status, end.Error)
	}

	nfrReads(t, n.nexusdInstance, "TC-189", nfrSamples, []nfrRead{
		{"system.capabilities.get", fixed("/v1/capabilities")},
		{"system.operations.list", fixed("/v1/operations")},
		{"system.operations.get", fixed("/v1/operations/" + ref.ID)},
		{"system.operations.logs", fixed("/v1/operations/" + ref.ID + "/logs")},
		{"system.audit.list", fixed("/v1/audit")},
		{"docker.containers.list", fixed(containersAt)},
		{"docker.containers.get", containerAt(containers)},
		{"docker.containers.logs", func(i int) string { return containerAt(containers)(i) + "/logs" }},
		{"apt.packages.list", fixed(packagesAt)},
		{"apt.packages.get", fixed(packagesAt + "/" + aptPlain)},
		{"apt.upgradable.list(cached)", fixed(upgradableAt)},
		{"scripts.scripts.list", fixed(scriptsAt)},
		{"scripts.scripts.get", fixed(scriptsAt + "/ok")},
	})

	// A-38: cold calls, each after the dpkg status changed.
	colds := []time.Duration{cold}
	for range 10 {
		now := time.Now()
		if err := os.Chtimes("/var/lib/dpkg/status", now, now); err != nil {
			t.Fatal(err)
		}
		d, code, b := n.timed(http.MethodGet, upgradableAt, "")
		if code != http.StatusOK {
			t.Fatalf("upgradable: %d %s", code, b)
		}
		colds = append(colds, d)
	}
	t.Logf("NFR TC-189 cold apt.upgradable.list (first call after start, then 10 after touching the dpkg status; reported apart, A-38): %s; each %v",
		nfrSummarize(colds), colds)

	var acked []string
	_, ids := nfrAcks(t, n.nexusdInstance, "TC-189", "docker.containers.start", nfrSamples,
		func(i int) string { return containerAt(containers)(i) + "/start" }, "{}")
	acked = append(acked, ids...)
	_, ids = nfrAcks(t, n.nexusdInstance, "TC-189", "scripts.scripts.invoke", nfrSamples, fixed(scriptsAt+"/ok/invoke"), "{}")
	acked = append(acked, ids...)
	_, ids = nfrAcks(t, n.nexusdInstance, "TC-189", "apt.actions.refresh", nfrSamples, fixed(refreshAt), "{}")
	acked = append(acked, ids...)
	nfrAudited(t, n, acked)
}

// TC-190: a real apt upgrade, its dpkg held in a gated postinst, runs
// while Docker and scripts reads and script starts are measured.
func TestNFRProvidersDoNotBlock(t *testing.T) {
	requireApt(t)
	nfrLoad(t)
	containers := nfrContainers(t)
	n := nfrSetup(t, "nfr-noblock", dockerSocket, "    lockWait: 300s\n")
	gate, open := aptGate(t, aptPlain)
	aptBuild(t, "add", aptRepo, aptPlain, "3.0", "--gate", gate)
	mustSh(t, apt.AptGet, "update", "-qq")

	up := startOp(t, n.nexusdInstance, upgradeAt, `{"names":["nxplain"]}`)
	running := func() (bool, string) {
		st := n.operation(up.ID).Status
		holder := frontendHolder(t)
		return st == apitypes.StatusRunning && holder != 0, fmt.Sprintf("upgrade %s, frontend lock holder %d", st, holder)
	}
	waitUntil(t, 2*time.Minute, "the upgrade's dpkg holding the lock in the gated postinst", running)
	_, state := running()
	t.Logf("NFR TC-190 in flight: %s", state)

	nfrReads(t, n.nexusdInstance, "TC-190", nfrSamples, []nfrRead{
		{"docker.containers.list", fixed(containersAt)},
		{"docker.containers.get", containerAt(containers)},
		{"scripts.scripts.list", fixed(scriptsAt)},
		{"scripts.scripts.get", fixed(scriptsAt + "/ok")},
	})
	nfrAcks(t, n.nexusdInstance, "TC-190", "scripts.scripts.invoke", nfrSamples, fixed(scriptsAt+"/ok/invoke"), "{}")

	ok, state := running()
	t.Logf("NFR TC-190 after the measurements: %s", state)
	if !ok {
		t.Fatalf("TC-190: the upgrade was no longer in flight at the end of the measurements (%s)", state)
	}
	open()
	if end := n.waitOp(up.ID, 3*time.Minute); end.Status != apitypes.StatusSucceeded {
		t.Fatalf("the upgrade ended %s %+v", end.Status, end.Error)
	}
}

// nfrSlug is the problem slug of a body, or "" when it is none.
func nfrSlug(b []byte) problem.Slug {
	var p problem.Error
	if err := json.Unmarshal(b, &p); err != nil {
		return ""
	}
	return p.Slug()
}

// nfrOthers checks the providers other than skip: each read answers 200
// with p95 under the read target over 20 samples.
func nfrOthers(t *testing.T, n *nexusdInstance, label string, skip ...string) {
	t.Helper()
	var reads []nfrRead
	for _, r := range []struct {
		provider string
		read     nfrRead
	}{
		{"system", nfrRead{"system.capabilities.get", fixed("/v1/capabilities")}},
		{"docker", nfrRead{"docker.containers.list", fixed(containersAt)}},
		{"apt", nfrRead{"apt.packages.list", fixed(packagesAt)}},
		{"scripts", nfrRead{"scripts.scripts.list", fixed(scriptsAt)}},
	} {
		if !slices.Contains(skip, r.provider) {
			reads = append(reads, r.read)
		}
	}
	nfrReads(t, n, label, 20, reads)
}

// nfrNoPanic requires no panic in the unit's journal since since.
func nfrNoPanic(t *testing.T, since time.Time) {
	t.Helper()
	for _, line := range journalWindow(t, since, time.Now().Add(time.Second), "-u", nexusdUnit) {
		if strings.Contains(strings.ToLower(line), "panic") {
			t.Errorf("panic in nexusd's journal: %s", line)
		}
	}
}

// dockerGate is a unix socket in front of the real Engine. Passing, it
// forwards each connection to the Engine; hung, it accepts and never
// answers: new connections are read and never written, and connections
// already forwarded are cut from the Engine and left open, unanswered.
type dockerGate struct {
	ln       net.Listener
	hung     atomic.Bool
	open     atomic.Int64 // Client connections not yet closed by the client
	accepted atomic.Int64
	mu       sync.Mutex
	upstream map[net.Conn]bool
}

func startDockerGate(t *testing.T, hung bool) *dockerGate {
	t.Helper()
	_ = os.Remove(nfrGateSock)
	ln, err := net.Listen("unix", nfrGateSock)
	if err != nil {
		t.Fatal(err)
	}
	g := &dockerGate{ln: ln, upstream: map[net.Conn]bool{}}
	g.hung.Store(hung)
	t.Cleanup(func() {
		_ = ln.Close()
		g.setHung(true) // Cut the Engine side; the clients are gone with nexusd
		_ = os.Remove(nfrGateSock)
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			g.accepted.Add(1)
			g.open.Add(1)
			go g.serve(c)
		}
	}()
	return g
}

func (g *dockerGate) serve(c net.Conn) {
	defer g.open.Add(-1)
	defer c.Close()
	if !g.hung.Load() {
		up, err := net.Dial("unix", dockerSocket)
		if err == nil {
			g.mu.Lock()
			g.upstream[up] = true
			g.mu.Unlock()
			go func() {
				// Engine to client; a cut leaves the client open.
				_, _ = io.Copy(c, up)
			}()
			// Client to Engine until the Engine side is cut.
			_, _ = io.Copy(up, c)
			g.mu.Lock()
			delete(g.upstream, up)
			g.mu.Unlock()
			_ = up.Close()
		}
	}
	// Hung: read until the client gives up; never write.
	_, _ = io.Copy(io.Discard, c)
}

// setHung switches the gate; hanging cuts every forwarded connection from
// the Engine.
func (g *dockerGate) setHung(h bool) {
	g.hung.Store(h)
	if !h {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for up := range g.upstream {
		_ = up.Close()
	}
}

// TC-191: the dependency failure matrix, one failure at a time, on one
// nexusd for the failures of a real dependency (same PID throughout), and
// on others for a Docker socket that accepts and never answers.
func TestNFRDependencyFailureMatrix(t *testing.T) {
	requireApt(t)
	nfrLoad(t)
	containers := nfrContainers(t)
	since := time.Now().Add(-time.Second)
	n := nfrSetup(t, "nfr-matrix", dockerSocket, "    lockWait: 5s\n")
	pid := n.mainPID()
	samePID := func(step string) {
		t.Helper()
		if got := n.mainPID(); got != pid {
			t.Fatalf("%s: nexusd pid %d, was %d", step, got, pid)
		}
	}
	nfrOthers(t, n.nexusdInstance, "TC-191 baseline")

	t.Run("docker-stopped", func(t *testing.T) {
		mustSh(t, "systemctl", "stop", "docker.socket", "docker.service")
		t.Cleanup(func() {
			_, _ = sh(time.Minute, "systemctl", "start", "docker.socket")
			restartDocker(t)
		})
		took, code, b := n.timed(http.MethodGet, containersAt, "")
		slug := nfrSlug(b)
		t.Logf("NFR TC-191 docker stopped: GET containers %d %s in %sms", code, slug, ms(took))
		if code != http.StatusServiceUnavailable || (slug != problem.DependencyUnavailable && slug != problem.CapabilityUnavailable) {
			t.Errorf("docker stopped: %d %s, want 503 dependency-unavailable or capability-unavailable", code, b)
		}
		var caps apitypes.Capabilities
		n.mustGet("/v1/capabilities?refresh=true", &caps)
		for _, p := range caps.Providers {
			t.Logf("NFR TC-191 docker stopped: capabilities %s %s %s", p.Name, p.Status, p.Reason)
		}
		nfrOthers(t, n.nexusdInstance, "TC-191 docker-stopped", "docker")
		samePID("docker stopped")

		// Recovered without a restart of nexusd.
		if out, err := sh(time.Minute, "systemctl", "start", "docker.socket", "docker.service"); err != nil {
			t.Fatalf("start docker: %v\n%s", err, out)
		}
		start := time.Now()
		waitUntil(t, time.Minute, "docker reads to recover", func() (bool, string) {
			_, code, b := n.timed(http.MethodGet, containersAt, "")
			return code == http.StatusOK, fmt.Sprintf("%d %s", code, b)
		})
		t.Logf("NFR TC-191 docker restarted: reads recovered %sms after the Engine's start returned", ms(time.Since(start)))
		samePID("docker restarted")
	})

	t.Run("registry-stopped", func(t *testing.T) {
		startRegistries(t)
		const name = "nfr-registry"
		requireDocker(t, name)
		ref := regOpen + "/nx/nfr:v1"
		pushed(t, ref, "nfr "+time.Now().String())
		runFrom(t, name, ref)
		regCmd(t, "stop-open")
		t.Cleanup(func() { startRegistries(t) })
		res := n.check()
		u := wantStatus(t, res.updates, name, dockerspec.UpdateUnknown, dockerspec.ReasonRegistryError)
		t.Logf("NFR TC-191 registry stopped: check-updates %s, %s: %s %s (%s)", res.op.Status, name, u.Status, u.Reason, u.Detail)
		nfrOthers(t, n.nexusdInstance, "TC-191 registry-stopped")
		samePID("registry stopped")
	})

	t.Run("dpkg-lock-held", func(t *testing.T) {
		h := aptHold(t, "nxnfrhold")
		start := time.Now()
		ref := startOp(t, n.nexusdInstance, upgradeAt, "{}")
		op := n.waitOp(ref.ID, 2*time.Minute)
		t.Logf("NFR TC-191 dpkg lock held by %d: upgrade %s %s after %sms", h.pid, op.Status, problemSlug(op), ms(time.Since(start)))
		if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.PackageManagerBusy) {
			t.Errorf("lock held: upgrade %+v, want Failed package-manager-busy", op)
		}
		if !h.running() {
			t.Error("the holder stopped holding the lock")
		}
		took, code, b := n.timed(http.MethodGet, packagesAt, "")
		t.Logf("NFR TC-191 dpkg lock held: apt.packages.list %d in %sms", code, ms(took))
		if code != http.StatusOK {
			t.Errorf("apt read under the held lock: %d %s", code, b)
		}
		nfrOthers(t, n.nexusdInstance, "TC-191 dpkg-lock-held", "apt")
		h.release()
		samePID("dpkg lock held")
	})

	t.Run("failing-script", func(t *testing.T) {
		ref := startOp(t, n.nexusdInstance, scriptsAt+"/fail/invoke", "{}")
		op := n.waitOp(ref.ID, time.Minute)
		t.Logf("NFR TC-191 failing script: %s %s", op.Status, problemSlug(op))
		if op.Status != apitypes.StatusFailed || problemSlug(op) != string(problem.ScriptFailed) {
			t.Errorf("failing script: %+v, want Failed script-failed", op)
		}
		nfrOthers(t, n.nexusdInstance, "TC-191 failing-script")
		samePID("failing script")
	})
	nfrNoPanic(t, since)

	// Docker hung: the provider passed its preflight through the gate, then
	// the socket stops answering. Then the same with the socket hung from
	// the start, which the preflight's own bound decides.
	t.Run("docker-hung", func(t *testing.T) {
		g := startDockerGate(t, false)
		hsince := time.Now().Add(-time.Second)
		hn := nfrStart(t, "nfr-hung", nfrGateSock, "")
		hpid := hn.mainPID()
		if _, code, b := hn.timed(http.MethodGet, containersAt, ""); code != http.StatusOK {
			t.Fatalf("docker read through the passing gate: %d %s", code, b)
		}
		idle := func() {
			hn.client.CloseIdleConnections()
			http.DefaultClient.CloseIdleConnections()
		}
		idle()
		time.Sleep(time.Second)
		base, err := nfrReadProc(hpid)
		if err != nil {
			t.Fatal(err)
		}
		baseOpen := g.open.Load()
		t.Logf("NFR TC-191 docker hung, baseline: nexusd fds %d, threads %d; gate connections open %d (%d accepted)",
			base.fds, base.threads, g.open.Load(), g.accepted.Load())

		g.setHung(true)
		type call struct {
			what string
			took time.Duration
			code int
			body []byte
		}
		paths := []string{containersAt, containersAt + "/" + containers[0], containersAt + "/" + containers[1]}
		calls := make(chan call, len(paths))
		var wg sync.WaitGroup
		clients := make([]*http.Client, len(paths))
		for i, path := range paths {
			clients[i] = &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{}}
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequest(http.MethodGet, "http://"+hn.listen+path, nil)
				if err != nil {
					calls <- call{what: path, body: []byte(err.Error())}
					return
				}
				req.Header.Set("Authorization", "Bearer "+hn.token)
				start := time.Now()
				resp, err := clients[i].Do(req)
				if err != nil {
					calls <- call{what: path, took: time.Since(start), body: []byte(err.Error())}
					return
				}
				b, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					b = []byte(err.Error())
				}
				calls <- call{what: path, took: time.Since(start), code: resp.StatusCode, body: b}
			}()
		}
		time.Sleep(2 * time.Second)
		during, err := nfrReadProc(hpid)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("NFR TC-191 docker hung: 3 docker reads waiting; gate connections open %d; nexusd fds %d, threads %d",
			g.open.Load(), during.fds, during.threads)
		nfrOthers(t, hn.nexusdInstance, "TC-191 docker-hung", "docker")
		nfrAcks(t, hn.nexusdInstance, "TC-191 docker-hung", "scripts.scripts.invoke", 20, fixed(scriptsAt+"/ok/invoke"), "{}")
		wg.Wait()
		close(calls)
		for c := range calls {
			slug := nfrSlug(c.body)
			t.Logf("NFR TC-191 docker hung: GET %s -> %d %s after %sms", c.what, c.code, slug, ms(c.took))
			if c.code != http.StatusServiceUnavailable || slug != problem.DependencyUnavailable {
				t.Errorf("hung docker: GET %s: %d %s, want 503 dependency-unavailable", c.what, c.code, c.body)
			}
			if c.took > 35*time.Second {
				t.Errorf("hung docker: GET %s took %v, want within the 30 s deadline plus 5 s", c.what, c.took)
			}
		}
		for _, c := range clients {
			c.CloseIdleConnections()
		}
		_, code, b := hn.timed(http.MethodGet, containersAt, "")
		t.Logf("NFR TC-191 docker hung: a docker read after the deadlines: %d %s", code, nfrSlug(b))
		idle()
		var after nfrProc
		waitUntil(t, 30*time.Second, "nexusd's fds back to the baseline and the hung connections closed", func() (bool, string) {
			after, err = nfrReadProc(hpid)
			return err == nil && after.fds <= base.fds && g.open.Load() <= baseOpen,
				fmt.Sprintf("fds %d (baseline %d), gate connections open %d (baseline %d)", after.fds, base.fds, g.open.Load(), baseOpen)
		})
		t.Logf("NFR TC-191 docker hung, afterwards: nexusd fds %d (baseline %d), threads %d (baseline %d); gate connections open %d (baseline %d, %d accepted)",
			after.fds, base.fds, after.threads, base.threads, g.open.Load(), baseOpen, g.accepted.Load())

		// Self-healing once the socket answers again.
		g.setHung(false)
		start := time.Now()
		waitUntil(t, time.Minute, "docker reads to recover after the hang", func() (bool, string) {
			_, code, b := hn.timed(http.MethodGet, containersAt, "")
			return code == http.StatusOK, fmt.Sprintf("%d %s", code, b)
		})
		t.Logf("NFR TC-191 docker hung: reads recovered %sms after the socket answered again", ms(time.Since(start)))
		if hn.mainPID() != hpid {
			t.Errorf("nexusd restarted under the hung socket")
		}
		nfrNoPanic(t, hsince)
	})

	t.Run("docker-hung-at-start", func(t *testing.T) {
		g := startDockerGate(t, true)
		hsince := time.Now().Add(-time.Second)
		hn := nfrStart(t, "nfr-hungstart", nfrGateSock, "")
		var startup time.Duration
		if detectedInit() == platform.OpenRC {
			startup = openrcReadyAfterExec(t)
		} else {
			p := showUnit(t, nexusdUnit, "ExecMainStartTimestampMonotonic", "ActiveEnterTimestampMonotonic")
			var exec, ready int64
			_, _ = fmt.Sscan(p["ExecMainStartTimestampMonotonic"], &exec)
			_, _ = fmt.Sscan(p["ActiveEnterTimestampMonotonic"], &ready)
			startup = time.Duration(ready-exec) * time.Microsecond
		}
		took, code, b := hn.timed(http.MethodGet, containersAt, "")
		t.Logf("NFR TC-191 docker hung at start: exec->ready %sms; GET containers %d %s in %sms; gate accepted %d, open %d",
			ms(startup), code, b, ms(took), g.accepted.Load(), g.open.Load())
		if code != http.StatusServiceUnavailable || (nfrSlug(b) != problem.CapabilityUnavailable && nfrSlug(b) != problem.DependencyUnavailable) {
			t.Errorf("hung at start: GET containers %d %s, want a 503 problem", code, b)
		}
		nfrOthers(t, hn.nexusdInstance, "TC-191 docker-hung-at-start", "docker")
		nfrNoPanic(t, hsince)
	})
}
