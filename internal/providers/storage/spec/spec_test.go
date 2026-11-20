// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package spec_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/def"
	"github.com/mauktik/nexus/internal/openapi"
	"github.com/mauktik/nexus/internal/providers/storage/spec"
)

// opRow is one row of the architecture's Operation catalog
// (planning/storage-provider/architecture.md, Operation catalog).
type opRow struct {
	id    string
	kind  def.Kind
	route string // Method and path under /v1
	mode  def.Mode
	risk  def.Risk
	scope string
	cli   string // Noun, verb and the positional field, if any
	tool  string
}

var catalog = []opRow{
	{"storage.health.get", def.KindList, "GET /providers/storage/health", def.ModeSync, def.RiskReadOnly, "storage:read", "storagehealth get", "get_storage_health"},
	{"storage.pools.list", def.KindList, "GET /providers/storage/pools", def.ModeSync, def.RiskReadOnly, "storage:read", "pool get", "list_zfs_pools"},
	{"storage.pools.get", def.KindGet, "GET /providers/storage/pools/{name}", def.ModeSync, def.RiskReadOnly, "storage:read", "pool get name", ""},
	{"storage.datasets.list", def.KindList, "GET /providers/storage/datasets", def.ModeSync, def.RiskReadOnly, "storage:read", "dataset get", "list_zfs_datasets"},
	{"storage.datasets.get", def.KindGet, "GET /providers/storage/datasets/{name}", def.ModeSync, def.RiskReadOnly, "storage:read", "dataset get name", ""},
	{"storage.snapshots.list", def.KindList, "GET /providers/storage/snapshots", def.ModeSync, def.RiskReadOnly, "storage:read", "snapshot get", ""},
	{"storage.backups.list", def.KindList, "GET /providers/storage/backups", def.ModeSync, def.RiskReadOnly, "storage:read", "backup get", ""},
	{"storage.backups.summary", def.KindList, "GET /providers/storage/backupsummary", def.ModeSync, def.RiskReadOnly, "storage:read", "backupsummary get", "get_backup_summary"},
	{"storage.timemachine.list", def.KindList, "GET /providers/storage/timemachine", def.ModeSync, def.RiskReadOnly, "storage:read", "timemachine get", "list_timemachine_shares"},
	{"storage.pools.scrub", def.KindResourceAction, "POST /providers/storage/pools/{name}/scrub", def.ModeLongRunning, def.RiskMutating, "storage:scrub", "scrub start name", ""},
	{"storage.pools.stop-scrub", def.KindResourceAction, "POST /providers/storage/pools/{name}/stop-scrub", def.ModeLongRunning, def.RiskMutating, "storage:scrub", "scrub stop name", ""},
	{"storage.datasets.snapshot", def.KindResourceAction, "POST /providers/storage/datasets/{name}/snapshot", def.ModeLongRunning, def.RiskMutating, "storage:snapshot", "snapshot add name", ""},
}

// columns is the architecture's "CLI columns of the reads" list.
var columns = map[string][]string{
	"storage.health.get": {"properties.counts.critical", "properties.counts.warning", "properties.counts.info", "properties.observedAt"},
	"storage.pools.list": {"name", "properties.health", "properties.capacityPercent", "properties.errors.checksum",
		"properties.scan.function", "properties.scan.state", "properties.scan.lastScrubAgeDays"},
	"storage.datasets.list": {"name", "properties.usedBytes", "properties.availableBytes", "properties.usedPercent",
		"properties.mountpoint", "properties.snapshotCount"},
	"storage.snapshots.list": {"name", "properties.createdAt", "properties.usedBytes", "properties.createdByNexus"},
	"storage.backups.list": {"name", "properties.guestType", "properties.vmid", "properties.modifiedAt",
		"properties.sizeBytes", "properties.logStatus"},
	"storage.backups.summary":  {"properties.guestCount", "properties.staleCount", "properties.failedCount"},
	"storage.timemachine.list": {"name", "properties.usedPercent", "properties.bundleCount", "properties.newestBackupAt"},
}

// forbiddenWords is internal/conformance's list of op ID and route words
// outside v1 (conformance_test.go, forbiddenWords), copied because a test
// cannot import another package's test file.
var forbiddenWords = []string{
	"create", "exec", "execute", "run", "shell", "reboot", "shutdown", "poweroff", "halt",
	"repository", "repositories", "repo", "repos", "sources", "key", "keys",
	"reconcile", "drift", "deploy", "deployment", "deployments", "template", "templates",
	"schedule", "schedules", "cron", "self", "selfupdate", "token", "tokens",
	"metrics",
}

func rowOf(t *testing.T, op def.Op) opRow {
	t.Helper()
	method, path, err := openapi.Route(op)
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	cli := op.CLI.Noun + " " + op.CLI.Verb
	if op.CLI.Positional != "" {
		cli += " " + op.CLI.Positional
	}
	tool := ""
	if op.MCP != nil {
		tool = op.MCP.Tool
	}
	return opRow{id: op.ID, kind: op.Kind, route: method + " " + strings.TrimPrefix(path, "/v1"), mode: op.Mode,
		risk: op.Risk, scope: op.Scope.Name, cli: cli, tool: tool}
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '.' || r == '/' || r == '-' || r == '_' || r == '{' || r == '}'
	})
}

func opByID(t *testing.T, id string) def.Op {
	t.Helper()
	for _, op := range spec.Spec().Ops {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("no op %s", id)
	return def.Op{}
}

func inputSchema(t *testing.T, op def.Op) *jsonschema.Schema {
	t.Helper()
	r, err := op.InputSchema()
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	return r.Schema()
}

// accepts reports whether op's input schema accepts the JSON object body.
func accepts(t *testing.T, op def.Op, body map[string]any) bool {
	t.Helper()
	r, err := op.InputSchema()
	if err != nil {
		t.Fatalf("%s: %v", op.ID, err)
	}
	// Validate a JSON round trip, as the Dispatcher validates decoded JSON.
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return r.Validate(v) == nil
}

// TC-STG-10: the twelve operations are declared with their catalog fields.
func TestTCSTG10_TheTwelveOperationsAreDeclaredWithTheir(t *testing.T) {
	s := spec.Spec()
	if s.Name != "storage" || s.ReadScope != "storage:read" {
		t.Errorf("spec %q read scope %q, want storage and storage:read", s.Name, s.ReadScope)
	}
	if len(s.Ops) != len(catalog) {
		t.Fatalf("spec has %d ops, the catalog %d", len(s.Ops), len(catalog))
	}
	scopes := []string{spec.ReadScope, spec.ScrubScope, spec.SnapshotScope}
	if !slices.Equal(scopes, []string{"storage:read", "storage:scrub", "storage:snapshot"}) {
		t.Errorf("scopes %v", scopes)
	}
	for i, op := range s.Ops {
		if got, want := rowOf(t, op), catalog[i]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i+1, got, want)
		}
		if op.Provider != spec.Name {
			t.Errorf("%s: provider %q", op.ID, op.Provider)
		}
		if !op.Untrusted {
			t.Errorf("%s: not Untrusted", op.ID)
		}
		if want := op.ID != "storage.datasets.snapshot"; op.Idempotent != want {
			t.Errorf("%s: Idempotent %v, want %v", op.ID, op.Idempotent, want)
		}
		if !slices.Contains(scopes, op.Scope.Name) || op.Scope.PerTarget != "" || op.Scope.FromTarget {
			t.Errorf("%s: scope %+v, want exactly one of %v", op.ID, op.Scope, scopes)
		}
		if !def.IsApprovedVerb(op.CLI.Verb) {
			t.Errorf("%s: verb %q is not approved", op.ID, op.CLI.Verb)
		}
		if n := op.CLI.Noun; n == "" || n != strings.ToLower(n) || strings.ContainsAny(n, " -_.") {
			t.Errorf("%s: noun %q is not one lower-case word", op.ID, n)
		}
		if op.Mutating() != (op.Mode == def.ModeLongRunning) {
			t.Errorf("%s: risk %s with mode %s", op.ID, op.Risk, op.Mode)
		}
		if op.Mode == def.ModeLongRunning {
			if op.Cancel != def.CancelQueuedOnly {
				t.Errorf("%s: Cancel %q, want queued-only", op.ID, op.Cancel)
			}
			if len(op.Columns) != 0 {
				t.Errorf("%s: a long-running op declares Columns %v", op.ID, op.Columns)
			}
			if op.SyncMutation || op.CompletesAtAdmission || op.WhatIf || op.CLI.WaitedResultItems {
				t.Errorf("%s: unexpected declarations %+v", op.ID, op)
			}
		} else if !slices.Equal(op.Columns, columns[op.ID]) {
			t.Errorf("%s: columns %v, want %v", op.ID, op.Columns, columns[op.ID])
		}
		if len(op.Sensitive) != 0 || op.Audited {
			t.Errorf("%s: Sensitive %v Audited %v", op.ID, op.Sensitive, op.Audited)
		}
		_, route, _ := openapi.Route(op)
		for _, w := range slices.Concat(words(op.ID), words(route)) {
			if slices.Contains(forbiddenWords, w) {
				t.Errorf("%s: forbidden word %q in its ID or route %s", op.ID, w, route)
			}
		}
	}
	for id := range columns {
		opByID(t, id)
	}

	// Each read's columns name a field of one output item.
	for _, op := range s.Ops {
		if len(op.Columns) == 0 {
			continue
		}
		r, err := op.OutputSchema()
		if err != nil {
			t.Fatalf("%s: %v", op.ID, err)
		}
		items := r.Schema().Properties["items"]
		if items == nil || items.Items == nil {
			t.Fatalf("%s: output has no items array", op.ID)
		}
		for _, col := range op.Columns {
			sc := items.Items
			for _, seg := range strings.Split(col, ".") {
				if sc = sc.Properties[seg]; sc == nil {
					t.Errorf("%s: column %q names no field of the output item", op.ID, col)
					break
				}
			}
		}
	}
}

var (
	name255    = "a" + strings.Repeat("b", 254)
	name256    = "a" + strings.Repeat("b", 255)
	dataset255 = "z1/" + strings.Repeat("d", 252)
	dataset256 = "z1/" + strings.Repeat("d", 253)
)

// TC-STG-11: the pool-name pattern accepts OpenZFS names and refuses
// everything else, in every input that takes a pool name.
func TestTCSTG11_ThePoolNamePatternAcceptsOpenZFSNames(t *testing.T) {
	if spec.PoolNamePattern != `^[A-Za-z][A-Za-z0-9_.:-]{0,254}$` {
		t.Errorf("PoolNamePattern = %q", spec.PoolNamePattern)
	}
	good := []string{"z1", "tank", "a", "tank.v2:a_b-c", name255}
	bad := []string{"", "1pool", "-x", "_x", "../x", "a b", "a;b", "a/b", "a@b", "a#b", "a%b", name256,
		"a\x00b", "a\nb", "z1\n", "pöol", "é"}
	fields := []struct{ op, field string }{
		{"storage.pools.get", "name"}, {"storage.pools.scrub", "name"}, {"storage.pools.stop-scrub", "name"},
		{"storage.pools.list", "name"}, {"storage.datasets.list", "pool"},
	}
	for _, f := range fields {
		op := opByID(t, f.op)
		for _, v := range good {
			if !accepts(t, op, map[string]any{f.field: v}) {
				t.Errorf("%s %s refuses %q", f.op, f.field, v)
			}
		}
		for _, v := range bad {
			if accepts(t, op, map[string]any{f.field: v}) {
				t.Errorf("%s %s accepts %q", f.op, f.field, v)
			}
		}
	}
}

// TC-STG-12: the dataset-name pattern accepts dataset paths and refuses
// traversal, snapshots and bookmarks.
func TestTCSTG12_TheDatasetNamePatternAcceptsDatasetPaths(t *testing.T) {
	if len(dataset255) != 255 || len(dataset256) != 256 {
		t.Fatal("test names have the wrong length")
	}
	good := []string{"z1", "z1/ds/appdata", "z2/d1/timemachine", "z1/a.b/c:d_e-f", dataset255, "z1/.x", "z1/x..y", "z1/..."}
	bad := []string{"z1/../x", "z1/./x", "z1/..", "z1/.", "z1//x", "/z1", "z1/", "z1/x@y", "z1/x#y", "1z/x", "z1/a b",
		dataset256, "z1/\n", "z1/x\n", "", "../x", "z1/é"}
	fields := []struct{ op, field string }{
		{"storage.datasets.get", "name"}, {"storage.datasets.snapshot", "name"}, {"storage.snapshots.list", "dataset"},
	}
	for _, f := range fields {
		op := opByID(t, f.op)
		p := inputSchema(t, op).Properties[f.field]
		if p == nil || p.Pattern != spec.DatasetNamePattern || p.Not == nil || p.Not.Pattern != spec.DatasetNameNotPattern {
			t.Errorf("%s %s: schema %+v", f.op, f.field, p)
		}
		for _, v := range good {
			if !accepts(t, op, map[string]any{f.field: v}) {
				t.Errorf("%s %s refuses %q", f.op, f.field, v)
			}
		}
		for _, v := range bad {
			if accepts(t, op, map[string]any{f.field: v}) {
				t.Errorf("%s %s accepts %q", f.op, f.field, v)
			}
		}
	}
	// The pool filter of datasets.list takes only a pool name.
	list := opByID(t, "storage.datasets.list")
	for _, v := range []string{"z1/ds", "z1/ds/appdata"} {
		if accepts(t, list, map[string]any{"pool": v}) {
			t.Errorf("datasets.list pool accepts %q", v)
		}
	}
}

// TC-STG-13: the snapshot label pattern.
func TestTCSTG13_TheSnapshotLabelPattern(t *testing.T) {
	if spec.SnapshotLabelPattern != `^[a-z0-9][a-z0-9-]{0,31}$` {
		t.Errorf("SnapshotLabelPattern = %q", spec.SnapshotLabelPattern)
	}
	op := opByID(t, "storage.datasets.snapshot")
	label32 := "a" + strings.Repeat("0-", 15) + "b"
	if len(label32) != 32 {
		t.Fatalf("label32 has %d characters", len(label32))
	}
	for _, v := range []string{"a", "0", "pre-upgrade", label32} {
		if !accepts(t, op, map[string]any{"name": "z1/ds", "label": v}) {
			t.Errorf("label %q refused", v)
		}
	}
	for _, v := range []string{"", "Bad", "-x", "a_b", "a.b", "a/b", "a@b", label32 + "c", "é", "a\n"} {
		if accepts(t, op, map[string]any{"name": "z1/ds", "label": v}) {
			t.Errorf("label %q accepted", v)
		}
	}
	if !accepts(t, op, map[string]any{"name": "z1/ds"}) || !accepts(t, op, map[string]any{"name": "z1/ds", "recursive": true}) {
		t.Error("a snapshot without a label is refused")
	}
	if accepts(t, op, map[string]any{"label": "a"}) {
		t.Error("a snapshot without a dataset name is accepted")
	}
}

// visit calls f with every property name of s and its nested schemas.
func visit(s *jsonschema.Schema, f func(name string)) {
	if s == nil {
		return
	}
	for name, p := range s.Properties {
		f(name)
		visit(p, f)
	}
	visit(s.Items, f)
}

// TC-STG-14: no input field name contains a conformance-forbidden part, and
// the snapshot name is not an input.
func TestTCSTG14_NoInputFieldNameContainsAConformance(t *testing.T) {
	for _, op := range spec.Spec().Ops {
		visit(inputSchema(t, op), func(name string) {
			n := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
			for _, part := range []string{"path", "command", "exec", "shell"} {
				if strings.Contains(n, part) {
					t.Errorf("%s: input field %q contains %q", op.ID, name, part)
				}
			}
		})
	}

	f, ok := reflect.TypeFor[spec.SnapshotInput]().FieldByName("Snapshot")
	if !ok || f.Tag.Get("json") != "-" {
		t.Errorf("SnapshotInput.Snapshot: present %v, json tag %q, want -", ok, f.Tag.Get("json"))
	}
	op := opByID(t, "storage.datasets.snapshot")
	s := inputSchema(t, op)
	if _, ok := s.Properties["snapshot"]; ok {
		t.Error("the snapshot input schema has a snapshot property")
	}
	if accepts(t, op, map[string]any{"name": "z1/ds/share", "snapshot": "evil"}) {
		t.Error("a body naming the snapshot is accepted")
	}
	if !accepts(t, op, map[string]any{"name": "z1/ds/share"}) {
		t.Error("a body with only the dataset name is refused")
	}
	b, err := json.Marshal(spec.SnapshotInput{Name: "z1/ds", Snapshot: "nexus-20261118T093000Z"})
	if err != nil || strings.Contains(string(b), "nexus-2026") {
		t.Errorf("SnapshotInput marshals %s (%v)", b, err)
	}
}

func bound(p *jsonschema.Schema) (lo, hi float64, ok bool) {
	if p == nil || p.Minimum == nil || p.Maximum == nil {
		return 0, 0, false
	}
	return *p.Minimum, *p.Maximum, true
}

func enumOf(p *jsonschema.Schema) []any {
	if p == nil {
		return nil
	}
	return p.Enum
}

// TC-STG-15: Refine sets enums, ranges and lengths, and every schema is
// generated without a cycle.
func TestTCSTG15_RefineSetsEnumsRangesAndLengthsAnd(t *testing.T) {
	lists := 0
	for _, op := range spec.Spec().Ops {
		if _, err := op.OutputSchema(); err != nil {
			t.Errorf("%s: output schema: %v", op.ID, err)
		}
		s := inputSchema(t, op)
		if p := s.Properties["limit"]; p != nil {
			lists++
			if lo, hi, ok := bound(p); !ok || lo != 1 || hi != 500 {
				t.Errorf("%s: limit %v..%v", op.ID, p.Minimum, p.Maximum)
			}
			c := s.Properties["cursor"]
			if c == nil || c.MaxLength == nil || *c.MaxLength != 512 || c.Pattern != spec.CursorPattern {
				t.Errorf("%s: cursor %+v", op.ID, c)
			}
		}
		for _, field := range []string{"name", "pool", "dataset"} {
			if p := s.Properties[field]; p != nil && (p.MaxLength == nil || *p.MaxLength != 255) {
				t.Errorf("%s: %s maxLength %v, want 255", op.ID, field, p.MaxLength)
			}
		}
		if p := s.Properties["since"]; p != nil && (p.Format != "date-time" || p.Pattern != spec.SincePattern) {
			t.Errorf("%s: since format %q pattern %q", op.ID, p.Format, p.Pattern)
		}
		// Every string input is constrained by a pattern or an enum, as the
		// conformance suite requires of catalogued ops.
		for name, p := range s.Properties {
			if (p.Type == "string" || slices.Contains(p.Types, "string")) && p.Pattern == "" && len(p.Enum) == 0 {
				t.Errorf("%s: string input %s is unconstrained", op.ID, name)
			}
		}
	}
	if lists != 5 {
		t.Errorf("%d ops take limit, want 5 (the pools, datasets, snapshots, backups and timemachine lists)", lists)
	}

	if p := inputSchema(t, opByID(t, "storage.datasets.snapshot")).Properties["label"]; p == nil || p.MaxLength == nil || *p.MaxLength != 32 {
		t.Errorf("label: %+v", p)
	}
	if got := enumOf(inputSchema(t, opByID(t, "storage.datasets.list")).Properties["type"]); !slices.Equal(got, []any{"filesystem", "volume"}) {
		t.Errorf("type enum %v", got)
	}
	backups := opByID(t, "storage.backups.list")
	bs := inputSchema(t, backups)
	if got := enumOf(bs.Properties["guestType"]); !slices.Equal(got, []any{"qemu", "lxc"}) {
		t.Errorf("guestType enum %v", got)
	}
	if lo, hi, ok := bound(bs.Properties["vmid"]); !ok || lo != 100 || hi != 999999999 {
		t.Errorf("vmid %v", bs.Properties["vmid"])
	}
	for v, want := range map[int]bool{99: false, 100: true, 999999999: true, 1000000000: false} {
		if got := accepts(t, backups, map[string]any{"vmid": v}); got != want {
			t.Errorf("vmid %d accepted %v, want %v", v, got, want)
		}
	}
	snaps := opByID(t, "storage.snapshots.list")
	if got := enumOf(inputSchema(t, snaps).Properties["createdByNexus"]); !slices.Equal(got, []any{"true", "false"}) {
		t.Errorf("createdByNexus enum %v", got)
	}
	if accepts(t, snaps, map[string]any{"createdByNexus": true}) || !accepts(t, snaps, map[string]any{"createdByNexus": "false"}) {
		t.Error("createdByNexus is not the string true or false")
	}
	for v, want := range map[string]bool{"2026-11-18T09:30:00Z": true, "2026-11-18T09:30:00.5+01:00": true,
		"2026-11-18": false, "yesterday": false} {
		if got := accepts(t, snaps, map[string]any{"since": v}); got != want {
			t.Errorf("since %q accepted %v, want %v", v, got, want)
		}
	}
	for _, v := range []int{0, 501} {
		if accepts(t, snaps, map[string]any{"limit": v}) {
			t.Errorf("limit %d accepted", v)
		}
	}
	if accepts(t, snaps, map[string]any{"cursor": strings.Repeat("a", 513)}) || accepts(t, snaps, map[string]any{"cursor": "-x"}) {
		t.Error("a bad cursor is accepted")
	}

	// The pool's vdevs are a flat list of Vdev with depth and parent.
	r, err := opByID(t, "storage.pools.get").OutputSchema()
	if err != nil {
		t.Fatal(err)
	}
	vdevs := r.Schema().Properties["properties"].Properties["vdevs"]
	if vdevs == nil || vdevs.Items == nil {
		t.Fatalf("pool vdevs schema %+v", vdevs)
	}
	item := vdevs.Items.Properties
	if item["depth"] == nil || item["parent"] == nil || item["name"] == nil {
		t.Errorf("vdev item properties %v", slices.Sorted(func(yield func(string) bool) {
			for k := range item {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for k, p := range item {
		if p.Type == "array" || slices.Contains(p.Types, "array") || p.Ref != "" {
			t.Errorf("vdev field %s nests: %+v", k, p)
		}
	}
}

// mcpDescriptions are the architecture's measured MCP descriptions (D-2).
var mcpDescriptions = map[string]string{
	"get_storage_health":      "Storage health signals: pool state and errors, capacity, scrub age, quotas, stale or failed backups.",
	"list_zfs_pools":          "ZFS pools: health, capacity, errors, scrub state and vdev tree; filter by name.",
	"list_zfs_datasets":       "ZFS datasets: usage, quota, compression, mount, snapshot count and newest snapshot.",
	"get_backup_summary":      "Proxmox vzdump backups per guest: latest time, size, log status, staleness.",
	"list_timemachine_shares": "Time Machine shares: usage, quota, per-Mac bundle size and last backup time.",
}

// TC-STG-16: MCP declarations: five read tools with the measured
// descriptions, no write tool.
func TestTCSTG16_MCPDeclarationsFiveReadToolsWithThe(t *testing.T) {
	want := map[string]string{
		"get_storage_health": "storage.health.get", "list_zfs_pools": "storage.pools.list",
		"list_zfs_datasets": "storage.datasets.list", "get_backup_summary": "storage.backups.summary",
		"list_timemachine_shares": "storage.timemachine.list",
	}
	got := map[string]string{}
	for _, op := range spec.Spec().Ops {
		if op.Mutating() {
			if op.MCP != nil {
				t.Errorf("%s: write exposed as MCP tool %q", op.ID, op.MCP.Tool)
			}
			if !strings.Contains(op.Description, "Side effects:") || !strings.Contains(op.Description, "Long-running") {
				t.Errorf("%s: description lacks Side effects: or Long-running: %s", op.ID, op.Description)
			}
		} else {
			if !strings.Contains(op.Description, "Read-only") {
				t.Errorf("%s: description lacks Read-only", op.ID)
			}
		}
		if !strings.Contains(op.Description, "are host-originated and untrusted") {
			t.Errorf("%s: description does not name its untrusted fields", op.ID)
		}
		if op.MCP == nil {
			continue
		}
		got[op.MCP.Tool] = op.ID
		if op.MCP.Description != mcpDescriptions[op.MCP.Tool] {
			t.Errorf("%s: MCP description\n got %q\nwant %q", op.MCP.Tool, op.MCP.Description, mcpDescriptions[op.MCP.Tool])
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("MCP tools %v, want %v", got, want)
	}
}

// propertiesTypes are the …Properties types and the objects they nest.
var propertiesTypes = []any{
	spec.PoolProperties{}, spec.ErrorCounts{}, spec.Scan{}, spec.Vdev{}, spec.DatasetProperties{},
	spec.SnapshotProperties{}, spec.BackupProperties{}, spec.GuestBackups{}, spec.BackupDirectory{},
	spec.BackupSummaryProperties{}, spec.BundleProperties{}, spec.ShareProperties{}, spec.HealthProperties{},
}

// TC-STG-17: spec wire types marshal lists as [] and nulls with reasons as
// declared.
func TestTCSTG17_SpecWireTypesMarshalListsAsAnd(t *testing.T) {
	for _, v := range propertiesTypes {
		typ := reflect.TypeOf(v)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		if _, ok := m["unavailable"]; ok {
			t.Errorf("%v: an empty unavailable is not omitted: %s", typ, b)
		}
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				t.Errorf("%v.%s: no JSON name", typ, f.Name)
				continue
			}
			if name == "unavailable" {
				if f.Type != reflect.TypeFor[spec.Unavailable]() {
					t.Errorf("%v.%s is %v, want spec.Unavailable", typ, f.Name, f.Type)
				}
				continue
			}
			raw, ok := m[name]
			if !ok {
				t.Errorf("%v: %s missing from %s", typ, name, b)
				continue
			}
			array := strings.HasPrefix(f.Type.Name(), "Array[")
			switch {
			case f.Type.Kind() == reflect.Pointer && string(raw) != "null":
				t.Errorf("%v: pointer %s marshals %s, want null", typ, name, raw)
			case array && string(raw) != "[]":
				t.Errorf("%v: list %s marshals %s, want []", typ, name, raw)
			case f.Type.Kind() == reflect.Slice && !array:
				t.Errorf("%v: list %s is a plain slice, want spec.Array", typ, name)
			}
		}
		with := reflect.New(typ).Elem()
		if f := with.FieldByName("Unavailable"); f.IsValid() {
			f.Set(reflect.ValueOf(spec.Unavailable{"x": "reason"}))
			b, err := json.Marshal(with.Interface())
			if err != nil || !strings.Contains(string(b), `"unavailable":{"x":"reason"}`) {
				t.Errorf("%v: unavailable with a reason marshals %s (%v)", typ, b, err)
			}
		}
	}

	b, err := json.Marshal(spec.BackupProperties{Timestamp: "2026-11-17T03:00:02", TimestampZone: spec.TimestampZoneNodeLocal,
		ModifiedAt: time.Date(2026, 11, 17, 2, 14, 9, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var backup struct {
		Timestamp     string `json:"timestamp"`
		TimestampZone string `json:"timestampZone"`
		ModifiedAt    string `json:"modifiedAt"`
	}
	if err := json.Unmarshal(b, &backup); err != nil {
		t.Fatal(err)
	}
	if backup.Timestamp != "2026-11-17T03:00:02" || backup.TimestampZone != "node-local" || backup.ModifiedAt != "2026-11-17T02:14:09Z" {
		t.Errorf("backup timestamps %+v", backup)
	}
	if _, err := time.Parse(time.RFC3339, backup.Timestamp); err == nil {
		t.Errorf("timestamp %q carries a zone", backup.Timestamp)
	}

	// A list item and a one-item read wrap their properties in a resource.
	list := apitypes.List[spec.Pool]{}
	if b, _ := json.Marshal(list); string(b) != `{"items":[],"nextCursor":""}` {
		t.Errorf("an empty pool list marshals %s", b)
	}
	if b, _ := json.Marshal(spec.Unavailable{"x": "r"}); string(b) != `{"x":"r"}` {
		t.Errorf("Unavailable marshals %s", b)
	}
}
