// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package ops_test

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/systemspec"
)

// TC-50: LaneQueue runs serially in ID order, LaneReject refuses past its
// limit naming the blocker, and a new lane is not blocked by held ones.
func TestLanePolicies(t *testing.T) {
	w := newWorld(t)
	p := who("ops", "test:write")
	op := tpOps.run.Op()
	var gates []*gate
	defer func() {
		for _, g := range gates {
			g.open()
		}
	}()
	gated := func(name string) *gate {
		g := w.tp.gated(name)
		gates = append(gates, g)
		return g
	}

	// Step 1: five on q.
	var qIDs, qNames []string
	var qGates []*gate
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("q%d", i)
		qGates = append(qGates, gated(name))
		o := w.start(p, op, `{"name":"`+name+`","lane":"q","policy":"queue"}`, "")
		qIDs, qNames = append(qIDs, o.ID), append(qNames, name)
	}
	if !slices.IsSorted(qIDs) {
		t.Fatalf("IDs not in creation order: %v", qIDs)
	}
	for i, g := range qGates {
		g.waitStarted(t)
		// Only this one runs: the rest have not started.
		time.Sleep(20 * time.Millisecond)
		if n := len(w.tp.runOrder()); n != i+1 {
			t.Fatalf("after q%d started, %d runs, want %d (not serial)", i+1, n, i+1)
		}
		g.open()
		w.waitStatus(qIDs[i], apitypes.StatusSucceeded)
	}
	if got := w.tp.runOrder(); !slices.Equal(got, qNames) {
		t.Fatalf("run order %v, want %v", got, qNames)
	}

	// Step 2: two on r, limit 1.
	gated("r1")
	r1 := w.start(p, op, `{"name":"r1","lane":"r","policy":"reject"}`, "")
	_, err := w.call(p, op, `{"name":"r2","lane":"r","policy":"reject"}`, "")
	pe := wantProblem(t, err, problem.Conflict)
	if pe.Status != http.StatusConflict || pe.Extensions["blockingOperationId"] != r1.ID {
		t.Fatalf("r2: %+v, want blocker %s", pe, r1.ID)
	}

	// Step 3: three on s, limit 2.
	gated("s1")
	gated("s2")
	s1 := w.start(p, op, `{"name":"s1","lane":"s","policy":"reject","limit":2}`, "")
	w.start(p, op, `{"name":"s2","lane":"s","policy":"reject","limit":2}`, "")
	_, err = w.call(p, op, `{"name":"s3","lane":"s","policy":"reject","limit":2}`, "")
	if pe := wantProblem(t, err, problem.Conflict); pe.Extensions["blockingOperationId"] != s1.ID {
		t.Fatalf("s3 blocker = %v, want %s", pe.Extensions["blockingOperationId"], s1.ID)
	}

	// Step 4: a new lane while all are held.
	gated("n1")
	gated("q6")
	w.start(p, op, `{"name":"q6","lane":"q","policy":"queue"}`, "")
	begin := time.Now()
	w.start(p, op, `{"name":"n1","lane":"n","policy":"reject"}`, "")
	if d := time.Since(begin); d >= 500*time.Millisecond {
		t.Fatalf("a new lane took %v while others were held", d)
	}
}

// TC-209: concurrent starts on a LaneReject lane admit exactly its limit,
// over the real HTTP server, while another lane answers promptly.
func TestConcurrentStartsOnARejectLaneAdmitTheLimit(t *testing.T) {
	w := newWorld(t)
	w.serve()
	tok := w.mint("burst", "test:write")
	const rounds, burst = 20, 50

	type answer struct {
		status  int
		blocker string
		id      string
	}
	post := func(path, body string) answer {
		resp, b := w.do(http.MethodPost, path, tok, body)
		a := answer{status: resp.StatusCode}
		switch resp.StatusCode {
		case http.StatusAccepted:
			a.id = decodeOp(t, b).ID
		case http.StatusConflict:
			a.blocker, _ = decodeProblem(t, b)["blockingOperationId"].(string)
		}
		return a
	}
	run := func(round, limit int) {
		lane := fmt.Sprintf("lim%d-%d", limit, round)
		name := fmt.Sprintf("w%d-%d", limit, round)
		g := w.tp.gated(name)
		defer g.open()
		other := fmt.Sprintf("o%d-%d", limit, round)
		w.tp.gated(other).open()
		runsBefore, opsBefore := w.tp.runs.Load(), w.countOps()
		refusedBefore := len(filterRecs(w.records(), outcome(audit.OutcomeRefused)))

		answers := make([]answer, burst)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range burst {
			wg.Go(func() {
				<-start
				answers[i] = post("/v1/providers/tp/things/"+name+"/run",
					fmt.Sprintf(`{"lane":%q,"policy":"reject","limit":%d}`, lane, limit))
			})
		}
		var otherAns answer
		wg.Go(func() {
			<-start
			// Fire once a winner is running and held, so the other lane is
			// asked while a lane is occupied; the rest of the burst is
			// still in flight. An event, not a sleep, so it holds at any speed.
			select {
			case <-g.started:
			case <-time.After(30 * time.Second):
			}
			otherAns = post("/v1/providers/tp/things/"+other+"/run", `{"lane":"`+lane+`-other","policy":"reject"}`)
		})
		close(start)
		// The winner's gate stays closed until this round ends, so the other
		// lane answering at all proves it is not blocked behind a held lane.
		// The bound only detects a deadlock; it is not a latency assertion, so
		// CPU contention under -race cannot trip it.
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d limit %d: the burst or the other lane never answered while the winner was held", round, limit)
		}

		var winners []string
		conflicts := 0
		for _, a := range answers {
			switch a.status {
			case http.StatusAccepted:
				winners = append(winners, a.id)
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("round %d limit %d: status %d", round, limit, a.status)
			}
		}
		if len(winners) != limit || conflicts != burst-limit {
			t.Fatalf("round %d limit %d: %d accepted, %d conflicts", round, limit, len(winners), conflicts)
		}
		for _, a := range answers {
			if a.status == http.StatusConflict && !slices.Contains(winners, a.blocker) {
				t.Fatalf("round %d: blocker %q is not a winner %v", round, a.blocker, winners)
			}
		}
		if otherAns.status != http.StatusAccepted {
			t.Fatalf("round %d: the other lane answered %d, want 202", round, otherAns.status)
		}
		if limit == 1 {
			g.waitStarted(t)
			// The winner and the other lane's op.
			if n := w.countOps() - opsBefore; n != 2 {
				t.Fatalf("round %d: %d records, want the winner and the other lane's", round, n)
			}
			w.waitStatus(otherAns.id, apitypes.StatusSucceeded)
			if n := w.tp.runs.Load() - runsBefore; n != 2 {
				t.Fatalf("round %d: %d runs, want the winner's and the other lane's", round, n)
			}
			refused := filterRecs(w.records(), outcome(audit.OutcomeRefused))[refusedBefore:]
			n := len(filterRecs(refused, func(r systemspec.AuditRecord) bool {
				return r.Problem == string(problem.Conflict) && r.Target == name
			}))
			if n != burst-limit {
				t.Fatalf("round %d: %d refused records, want %d", round, n, burst-limit)
			}
		}
	}
	for round := range rounds {
		run(round, 1)
	}
	for round := range rounds {
		run(round, 2)
	}
}
