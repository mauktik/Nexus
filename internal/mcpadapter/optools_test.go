package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/problem"
	docker "github.com/mauktik/nexus/internal/providers/docker/spec"
)

// logPage is a get_operation_logs result.
type logPage struct {
	Frames       []apitypes.Frame `json:"frames"`
	NextOffset   *int64           `json:"nextOffset"`
	Truncated    *bool            `json:"truncated"`
	ContentTrust string           `json:"contentTrust"`
}

// TC-148: the operation handle loop.
func TestOperationHandleLoop(t *testing.T) {
	w, _, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)

	// Step 1: the long-running tool answers at once with the operation.
	start := time.Now()
	res := callTool(t, cs, "run_job", map[string]any{"name": "first", "bytes": 5000})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("run_job took %v, want under 500ms", d)
	}
	op := operation(t, res)

	// get_operation to terminal reaches the true result.
	done := waitStatus(t, cs, op.ID, apitypes.StatusSucceeded)
	var result JobResult
	if err := json.Unmarshal(done.Result, &result); err != nil || result.Name != "first" || result.Written != 5000 ||
		!result.Changed {
		t.Errorf("result %s, want first with 5000 bytes written", done.Result)
	}
	rest := restDo(t, http.MethodGet, w.base+"/v1/operations/"+op.ID, w.full, nil)
	var viaREST apitypes.Operation
	if err := json.Unmarshal(rest.body, &viaREST); err != nil || !jsonEqual(t, viaREST.Result, done.Result) ||
		viaREST.Status != done.Status {
		t.Errorf("REST operation %s differs from get_operation %+v", rest.body, done)
	}

	// get_operation_logs is one bounded page whose frames are the REST log.
	logs := callTool(t, cs, "get_operation_logs", map[string]any{"id": op.ID})
	var page logPage
	if err := json.Unmarshal(structured(t, logs), &page); err != nil || logs.IsError {
		t.Fatalf("get_operation_logs: %v %s", err, structured(t, logs))
	}
	want := restFrames(t, w, op.ID)
	if len(want) < 2 || !reflect.DeepEqual(page.Frames, want) {
		t.Errorf("page frames %+v\nREST frames %+v", page.Frames, want)
	}
	if page.Truncated == nil || *page.Truncated || page.NextOffset == nil || *page.NextOffset != 5000 {
		t.Errorf("page truncated %v nextOffset %v, want false and 5000", page.Truncated, page.NextOffset)
	}

	// cancel_operation on another gives the outcome REST gives.
	blocker := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "blocker", "hold": true}))
	waitStatus(t, cs, blocker.ID, apitypes.StatusRunning)
	viaMCP := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "mcp-cancel"}))
	viaRESTq := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "rest-cancel"}))
	mc := operation(t, callTool(t, cs, "cancel_operation", map[string]any{"id": viaMCP.ID}))
	rc := restDo(t, http.MethodPost, w.base+"/v1/operations/"+viaRESTq.ID+"/cancel", w.full, strings.NewReader(`{}`))
	var restCanceled apitypes.Operation
	if rc.status != http.StatusOK || json.Unmarshal(rc.body, &restCanceled) != nil {
		t.Fatalf("REST cancel: %d %s", rc.status, rc.body)
	}
	if mc.Status != apitypes.StatusCanceled || restCanceled.Status != apitypes.StatusCanceled ||
		(mc.Error == nil) != (restCanceled.Error == nil) || mc.Ended == nil || restCanceled.Ended == nil {
		t.Errorf("MCP cancel %+v, REST cancel %+v: want the same Canceled outcome", mc, restCanceled)
	}
	// The running blocker cannot be canceled, on either surface.
	refused := callTool(t, cs, "cancel_operation", map[string]any{"id": blocker.ID})
	rr := restDo(t, http.MethodPost, w.base+"/v1/operations/"+blocker.ID+"/cancel", w.full, strings.NewReader(`{}`))
	if p := structuredProblem(t, structured(t, refused)); !refused.IsError || p.Slug() != problem.NotCancelable ||
		rr.problem(t).Slug() != problem.NotCancelable {
		t.Errorf("cancel of a running op: MCP %s, REST %d %s, want not-cancelable on both",
			structured(t, refused), rr.status, rr.body)
	}
}

// TC-221: MCP log tools return one bounded page.
func TestLogToolsReturnOneBoundedPage(t *testing.T) {
	w, j, spy := catalogWorld(t)
	cs := w.session(t, w.full, nil)

	// Step 1: neither schema has follow, and passing it is refused.
	for _, tool := range listTools(t, cs) {
		if tool.Name != "get_container_logs" && tool.Name != "get_operation_logs" {
			continue
		}
		if _, ok := properties(schemaMap(t, tool.InputSchema))["follow"]; ok {
			t.Errorf("%s: input schema has follow", tool.Name)
		}
	}
	const size = 200 << 10
	op := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "big", "bytes": size}))
	waitStatus(t, cs, op.ID, apitypes.StatusSucceeded)
	for tool, args := range map[string]map[string]any{
		"get_operation_logs": {"id": op.ID, "follow": true},
		"get_container_logs": {"name": "web", "follow": true},
	} {
		res := callTool(t, cs, tool, args)
		p := structuredProblem(t, structured(t, res))
		fe, _ := json.Marshal(p.Extensions["errors"])
		if !res.IsError || p.Slug() != problem.ValidationFailed || !strings.Contains(string(fe), `"/follow"`) {
			t.Errorf("%s with follow: %s, want validation-failed at /follow", tool, structured(t, res))
		}
	}

	// Step 2: page from each nextOffset until a page is not truncated.
	var got strings.Builder
	offset, pages := int64(0), 0
	for {
		res := callTool(t, cs, "get_operation_logs", map[string]any{"id": op.ID, "offset": offset})
		var page logPage
		if err := json.Unmarshal(structured(t, res), &page); err != nil || res.IsError {
			t.Fatalf("page at %d: %v %s", offset, err, structured(t, res))
		}
		if page.Frames == nil || page.NextOffset == nil || page.Truncated == nil {
			t.Fatalf("page at %d lacks frames, nextOffset or truncated: %s", offset, structured(t, res))
		}
		if n := len(text(page.Frames)); n > pageLimit {
			t.Errorf("page at %d holds %d bytes, over 64 KiB", offset, n)
		}
		got.WriteString(text(page.Frames))
		pages++
		if !*page.Truncated {
			break
		}
		if *page.NextOffset <= offset || pages > 10 {
			t.Fatalf("no progress: offset %d nextOffset %d after %d pages", offset, *page.NextOffset, pages)
		}
		offset = *page.NextOffset
	}
	if want := text(restFrames(t, w, op.ID)); got.String() != want || len(want) != size {
		t.Errorf("pages hold %d bytes, REST %d (want %d): gap or duplicate", got.Len(), len(want), size)
	}
	if pages != 4 {
		t.Errorf("%d pages for 200 KiB, want 4 of at most 64 KiB", pages)
	}

	// Step 3: a running op answers at once and keeps running.
	running := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "running", "bytes": 1000, "hold": true}))
	waitStatus(t, cs, running.ID, apitypes.StatusRunning)
	start := time.Now()
	res := callTool(t, cs, "get_operation_logs", map[string]any{"id": running.ID})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("get_operation_logs on a running op took %v", d)
	}
	var page logPage
	if err := json.Unmarshal(structured(t, res), &page); err != nil || res.IsError || page.Truncated == nil || *page.Truncated {
		t.Errorf("running op page: %v %s", err, structured(t, res))
	}
	if now := operation(t, callTool(t, cs, "get_operation", map[string]any{"id": running.ID})); now.Status != apitypes.StatusRunning {
		t.Errorf("op is %s after reading its log, want Running", now.Status)
	}
	j.release("running")

	// Container logs: without tail or since the tool asks for the last 200
	// lines; with since it does not add tail.
	for _, c := range []struct {
		args map[string]any
		tail *int
	}{
		{map[string]any{"name": "web"}, func() *int { n := 200; return &n }()},
		{map[string]any{"name": "web2", "since": "2026-01-02T03:04:05Z"}, nil},
		{map[string]any{"name": "web3", "tail": 5}, func() *int { n := 5; return &n }()},
	} {
		res := callTool(t, cs, "get_container_logs", c.args)
		var page map[string]any
		if err := json.Unmarshal(structured(t, res), &page); err != nil || res.IsError {
			t.Fatalf("get_container_logs %v: %v %s", c.args, err, structured(t, res))
		}
		if page["truncated"] != false || page[trustField] != untrusted {
			t.Errorf("get_container_logs page %v", page)
		}
		var in docker.LogsInput
		for _, call := range spy.Calls() {
			if l, ok := call.Input.(docker.LogsInput); ok && l.Name == c.args["name"] {
				in = l
			}
		}
		if in.Name == "" || !reflect.DeepEqual(in.Tail, c.tail) || in.Follow {
			t.Errorf("get_container_logs %v reached the handler as %+v, want tail %v", c.args, in, c.tail)
		}
	}
}
