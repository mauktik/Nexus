// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/problem"
)

// refs collects every $ref in a schema.
func refs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "$ref" {
				*out = append(*out, s)
			}
			refs(e, out)
		}
	case []any:
		for _, e := range x {
			refs(e, out)
		}
	}
}

// TC-147: tool annotations derive from risk; every tool has self-contained
// input and output schemas.
func TestToolAnnotationsDeriveFromRisk(t *testing.T) {
	w, _, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)
	byTool := opsByTool()
	tools := listTools(t, cs)
	if len(tools) < 20 {
		t.Fatalf("only %d tools listed: the catalog is not enabled", len(tools))
	}
	for _, tool := range tools {
		op, ok := byTool[tool.Name]
		if !ok {
			t.Errorf("tool %s has no op", tool.Name)
			continue
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}
		if a.ReadOnlyHint != (op.Risk == def.RiskReadOnly) {
			t.Errorf("%s: readOnlyHint %v for risk %s", tool.Name, a.ReadOnlyHint, op.Risk)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != (op.Risk == def.RiskDestructive) {
			t.Errorf("%s: destructiveHint %v for risk %s, want explicit %v", tool.Name, a.DestructiveHint, op.Risk,
				op.Risk == def.RiskDestructive)
		}
		if a.IdempotentHint != op.Idempotent {
			t.Errorf("%s: idempotentHint %v, op Idempotent %v", tool.Name, a.IdempotentHint, op.Idempotent)
		}
		in, out := schemaMap(t, tool.InputSchema), schemaMap(t, tool.OutputSchema)
		if in["type"] != "object" {
			t.Errorf("%s: inputSchema type %v", tool.Name, in["type"])
		}
		if out == nil || out["type"] != "object" {
			t.Errorf("%s: outputSchema %v, want an object schema", tool.Name, out)
		}
		var rs []string
		refs(in, &rs)
		refs(out, &rs)
		for _, r := range rs {
			if !strings.HasPrefix(r, "#") {
				t.Errorf("%s: external $ref %q", tool.Name, r)
			}
		}
		for name, s := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var js jsonschema.Schema
			if err := json.Unmarshal(b, &js); err != nil {
				t.Errorf("%s: %s schema does not parse: %v", tool.Name, name, err)
			} else if _, err := js.Resolve(nil); err != nil {
				t.Errorf("%s: %s schema does not resolve on its own: %v", tool.Name, name, err)
			}
		}
	}

	// The hints are explicit on the wire, false included, so a client never
	// falls back to the spec defaults (destructiveHint true).
	ex := w.send(t, rpc{token: w.full, version: modern, method: "tools/list"})
	var raw struct {
		Tools []struct {
			Name        string                     `json:"name"`
			Annotations map[string]json.RawMessage `json:"annotations"`
		} `json:"tools"`
	}
	ex.result(t, &raw)
	for _, tool := range raw.Tools {
		for _, k := range []string{"readOnlyHint", "destructiveHint", "idempotentHint"} {
			if _, ok := tool.Annotations[k]; !ok {
				t.Errorf("%s: %s absent on the wire: %v", tool.Name, k, tool.Annotations)
			}
		}
	}
}

// TC-149: mutating tools accept an idempotency key and whatIf per Op.WhatIf.
func TestMutatingToolsAcceptIdempotencyKeyAndWhatIf(t *testing.T) {
	w, j, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)
	byTool := opsByTool()

	// Step 1: the schema walk.
	sawCancel, sawWhatIf := false, false
	for _, tool := range listTools(t, cs) {
		op := byTool[tool.Name]
		props := properties(schemaMap(t, tool.InputSchema))
		key, hasKey := props["idempotencyKey"].(map[string]any)
		if hasKey != op.Mutating() {
			t.Errorf("%s (%s): idempotencyKey present %v", tool.Name, op.Risk, hasKey)
		}
		if hasKey && key["type"] != "string" {
			t.Errorf("%s: idempotencyKey schema %v", tool.Name, key)
		}
		if _, ok := props["whatIf"]; ok != op.WhatIf {
			t.Errorf("%s: whatIf present %v, Op.WhatIf %v", tool.Name, ok, op.WhatIf)
		}
		if tool.Name == "cancel_operation" && hasKey {
			sawCancel = true
		}
		sawWhatIf = sawWhatIf || op.WhatIf
	}
	if !sawCancel || !sawWhatIf {
		t.Fatalf("schema walk saw cancel_operation with a key %v, a whatIf tool %v", sawCancel, sawWhatIf)
	}

	// Step 2: a retry with the same key after a broken stream.
	args := map[string]any{"name": "retry", "bytes": 10, "idempotencyKey": "retry-key-1"}
	first := operation(t, callTool(t, cs, "run_job", args))
	second := operation(t, callTool(t, cs, "run_job", args))
	if first.ID != second.ID {
		t.Errorf("retry with the same key started operation %s after %s", second.ID, first.ID)
	}
	waitStatus(t, cs, first.ID, apitypes.StatusSucceeded)
	if n := j.count("retry"); n != 1 {
		t.Errorf("job ran %d times, want 1", n)
	}
	// The key never reached validation or the record: the requested record
	// holds the input without it, and the REST replay with the same key as
	// the header and the same body answers with the same operation.
	page, err := w.audit.Query(audit.Query{Action: jobRun.Op().ID, Outcome: audit.OutcomeRequested, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || strings.Contains(string(page.Items[0].Params), "idempotencyKey") {
		t.Errorf("requested records = %+v, want one without idempotencyKey", page.Items)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+w.full)
	h.Set("Content-Type", "application/json")
	h.Set("Idempotency-Key", "retry-key-1")
	rest := post(t, w.base+"/v1/providers/mcptest/jobs/retry/run", h, strings.NewReader(`{"name":"retry","bytes":10}`))
	var viaREST apitypes.Operation
	if rest.status != http.StatusAccepted || json.Unmarshal(rest.body, &viaREST) != nil || viaREST.ID != first.ID {
		t.Errorf("REST with the same key: %d %s, want operation %s", rest.status, rest.body, first.ID)
	}

	// A key that is not a string, one that differs from the header, and one
	// on a read-only tool are validation errors, and nothing runs.
	bad := func(cs *mcp.ClientSession, tool string, args map[string]any, path string) {
		t.Helper()
		res := callTool(t, cs, tool, args)
		if !res.IsError {
			t.Errorf("%s %v: isError false: %s", tool, args, structured(t, res))
			return
		}
		p := structuredProblem(t, structured(t, res))
		fe, _ := json.Marshal(p.Extensions["errors"])
		if p.Slug() != problem.ValidationFailed || !strings.Contains(string(fe), `"`+path+`"`) {
			t.Errorf("%s %v: %s %s, want validation-failed at %s", tool, args, p.Slug(), fe, path)
		}
	}
	bad(cs, "run_job", map[string]any{"name": "typed", "idempotencyKey": 7}, "/idempotencyKey")
	bad(cs, "run_job", map[string]any{"name": "long", "idempotencyKey": strings.Repeat("k", 256)}, "/idempotencyKey")
	withHeader := w.session(t, w.full, map[string]string{"Idempotency-Key": "header-key"})
	bad(withHeader, "run_job", map[string]any{"name": "mismatch", "idempotencyKey": "other-key"}, "/idempotencyKey")
	bad(cs, "get_note", map[string]any{"name": "n", "idempotencyKey": "k"}, "/idempotencyKey")
	for _, n := range []string{"typed", "long", "mismatch"} {
		if c := j.count(n); c != 0 {
			t.Errorf("job %s ran %d times after a refused key", n, c)
		}
	}

	// Step 3: cancel_operation accepts the key and ignores it.
	blocker := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "blocker", "hold": true}))
	waitStatus(t, cs, blocker.ID, apitypes.StatusRunning)
	q1 := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "q1"}))
	q2 := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "q2"}))
	if q1.Status != apitypes.StatusQueued || q2.Status != apitypes.StatusQueued {
		t.Fatalf("jobs behind the blocker are %s and %s, want Queued", q1.Status, q2.Status)
	}
	cancel := func(id string) *mcp.CallToolResult {
		return callTool(t, cs, "cancel_operation", map[string]any{"id": id, "idempotencyKey": "cancel-key"})
	}
	c1, c2 := cancel(q1.ID), cancel(q1.ID)
	if c1.IsError || c2.IsError || !jsonEqual(t, structured(t, c1), structured(t, c2)) {
		t.Errorf("repeat cancels differ:\n%s\n%s", structured(t, c1), structured(t, c2))
	}
	if op := operation(t, c1); op.Status != apitypes.StatusCanceled {
		t.Errorf("canceled op is %s", op.Status)
	}
	c3 := cancel(q2.ID)
	if c3.IsError {
		t.Fatalf("cancel of a second op with the same key: %s", structured(t, c3))
	}
	if op := operation(t, c3); op.ID != q2.ID || op.Status != apitypes.StatusCanceled {
		t.Errorf("second cancel answered %s %s, want %s Canceled", op.ID, op.Status, q2.ID)
	}
	reused, err := w.audit.Query(audit.Query{Outcome: audit.OutcomeRefused, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reused.Items {
		if r.Problem == string(problem.IdempotencyKeyReused) {
			t.Errorf("idempotency-key-reused recorded: %+v", r)
		}
	}
	j.release("blocker")
}

// TC-150: untrusted host text is labeled in the description and the result,
// and stays in a data field.
func TestUntrustedHostTextIsLabeled(t *testing.T) {
	w, _, _ := catalogWorld(t)
	cs := w.session(t, w.full, nil)
	tools := map[string]*mcp.Tool{}
	for _, tool := range listTools(t, cs) {
		tools[tool.Name] = tool
	}
	byTool := opsByTool()

	// Step 1: descriptions and output schemas.
	for _, name := range []string{"get_container_logs", "get_operation_logs", "get_package", "get_note"} {
		tool := tools[name]
		if tool == nil {
			t.Errorf("%s is not listed", name)
			continue
		}
		if !byTool[name].Untrusted {
			t.Errorf("%s: op is not Untrusted", name)
		}
		if d := tool.Description; !strings.Contains(strings.ToLower(d), "untrusted") || !strings.Contains(d, trustField) {
			t.Errorf("%s: description %q does not say the content is untrusted", name, tool.Description)
		}
		out := schemaMap(t, tool.OutputSchema)
		ct, _ := properties(out)[trustField].(map[string]any)
		req, _ := out["required"].([]any)
		if ct == nil || !slices.Contains(req, any(trustField)) {
			t.Errorf("%s: outputSchema lacks a required %s: %v", name, trustField, out)
		}
	}
	for name, tool := range tools {
		if !byTool[name].Untrusted && strings.Contains(strings.ToLower(tool.Description), "untrusted") {
			t.Errorf("%s: trusted tool says untrusted: %q", name, tool.Description)
		}
		if strings.Contains(tool.Description, injection) {
			t.Errorf("%s: host text in the description", name)
		}
	}

	// Step 1: the call.
	res := callTool(t, cs, "get_note", map[string]any{"name": "n1"})
	if res.IsError {
		t.Fatalf("get_note: %s", structured(t, res))
	}
	var note map[string]any
	if err := json.Unmarshal(structured(t, res), &note); err != nil {
		t.Fatal(err)
	}
	if note[trustField] != untrusted || note["text"] != injection {
		t.Errorf("get_note result %v, want %s %q and the text in its data field", note, trustField, untrusted)
	}
	if tc, ok := res.Content[0].(*mcp.TextContent); !ok || !jsonEqual(t, []byte(tc.Text), structured(t, res)) {
		t.Errorf("text content does not match the labeled structured result: %#v", res.Content[0])
	}

	// A bounded log page keeps host text inside frames, with the label.
	op := operation(t, callTool(t, cs, "run_job", map[string]any{"name": "inject", "text": injection + "\n"}))
	if v, _ := json.Marshal(structured(t, callTool(t, cs, "run_job", map[string]any{"name": "inject2"}))); !strings.Contains(string(v), untrusted) {
		t.Errorf("an Untrusted long-running tool's operation is not labeled: %s", v)
	}
	waitStatus(t, cs, op.ID, apitypes.StatusSucceeded)
	logs := callTool(t, cs, "get_operation_logs", map[string]any{"id": op.ID})
	var page struct {
		Frames       []apitypes.Frame `json:"frames"`
		ContentTrust string           `json:"contentTrust"`
	}
	if err := json.Unmarshal(structured(t, logs), &page); err != nil || logs.IsError {
		t.Fatalf("get_operation_logs: %v %s", err, structured(t, logs))
	}
	if page.ContentTrust != untrusted || text(page.Frames) != injection+"\n" {
		t.Errorf("log page %s, want the text in frames and %s %s", structured(t, logs), trustField, untrusted)
	}

	// get_operation labels the operation of an Untrusted op, whose result
	// can quote host text, and only that.
	var got map[string]any
	if err := json.Unmarshal(structured(t, callTool(t, cs, "get_operation", map[string]any{"id": op.ID})), &got); err != nil {
		t.Fatal(err)
	}
	if got[trustField] != untrusted {
		t.Errorf("get_operation of an Untrusted op: %v, want %s", got, trustField)
	}
	restart := operation(t, callTool(t, cs, "restart_widget", map[string]any{"name": "w1"}))
	got = nil
	if err := json.Unmarshal(structured(t, callTool(t, cs, "get_operation", map[string]any{"id": restart.ID})), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[trustField]; ok {
		t.Errorf("get_operation of a trusted op is labeled: %v", got)
	}
}
