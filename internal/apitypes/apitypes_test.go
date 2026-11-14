package apitypes

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

func topLevelKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func wantKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s keys = %v, want exactly %v", what, got, want)
	}
}

// TC-6: wire types match the v1 contract.
func TestResourceWireShape(t *testing.T) {
	r := NewResource("nas", "docker", "containers", "web", map[string]string{"state": "running"})
	wantKeys(t, "Resource", topLevelKeys(t, r), "id", "type", "name", "host", "properties")
	if r.ID != "/providers/docker/containers/web" || r.Type != "containers" || r.Name != "web" || r.Host != "nas" {
		t.Errorf("NewResource = %+v", r)
	}
}

func TestResourceIDHelper(t *testing.T) {
	if got := ResourceID("docker", "containers", "web"); got != "/providers/docker/containers/web" {
		t.Fatalf("ResourceID = %q, want /providers/docker/containers/web", got)
	}
}

func TestOperationWireShape(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	succeeded := Operation{
		ID:            "0196f0c2-0000-7000-8000-000000000001",
		Host:          "nas",
		Action:        "docker.containers.restart",
		Provider:      "docker",
		Target:        "/providers/docker/containers/web",
		TokenName:     "laptop",
		Status:        StatusSucceeded,
		Created:       now,
		Started:       &now,
		Ended:         &now,
		Result:        json.RawMessage(`{"changed":true}`),
		OutputEvicted: false,
	}
	want := []string{"id", "host", "action", "provider", "target", "tokenName", "status",
		"created", "started", "ended", "result", "error", "outputEvicted"}
	wantKeys(t, "Operation", topLevelKeys(t, succeeded), want...)

	// The shape is stable: a queued operation has the same keys, with nulls.
	queued := Operation{ID: "x", Host: "nas", Action: "apt.actions.upgrade", Provider: "apt",
		TokenName: "ci", Status: StatusQueued, Created: now}
	wantKeys(t, "queued Operation", topLevelKeys(t, queued), want...)

	failed := queued
	failed.Status = StatusFailed
	failed.Error = problem.New(problem.OutcomeUnknown, "the unit died without exit.json")
	b, err := json.Marshal(failed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"error":{"type":"urn:nexus:problem:outcome-unknown"`) {
		t.Errorf("failed operation error not a problem object: %s", b)
	}
}

func TestOperationStatusTerminal(t *testing.T) {
	for s, terminal := range map[OperationStatus]bool{
		StatusQueued: false, StatusRunning: false,
		StatusSucceeded: true, StatusFailed: true, StatusCanceled: true,
	} {
		if s.Terminal() != terminal {
			t.Errorf("%s.Terminal() = %v, want %v", s, s.Terminal(), terminal)
		}
	}
}

func TestListWireShape(t *testing.T) {
	l := List[Resource[map[string]string]]{
		Items:      []Resource[map[string]string]{NewResource("nas", "apt", "packages", "curl", map[string]string{})},
		NextCursor: "abc",
	}
	wantKeys(t, "List", topLevelKeys(t, l), "items", "nextCursor")

	var empty List[Resource[any]]
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"items":[],"nextCursor":""}` {
		t.Errorf("empty list = %s, want items as [] and an empty nextCursor", b)
	}

	var back List[string]
	if err := json.Unmarshal([]byte(`{"items":["a"],"nextCursor":"n"}`), &back); err != nil || back.NextCursor != "n" || len(back.Items) != 1 {
		t.Errorf("List round trip = %+v, %v", back, err)
	}
}

func TestCapabilitiesWireShape(t *testing.T) {
	c := NewCapabilities("nas", "1.2.3", HostOS{ID: "alpine", VersionID: "3.21.2"}, "openrc", []ProviderCapability{{
		Name: "docker", Status: ProviderUnavailable, Reason: "socket unreachable",
		ResourceTypes: []string{"containers"}, Actions: []string{"check-updates"},
	}})
	if c.ID != "/capabilities" || CapabilitiesID != "/capabilities" {
		t.Errorf("Capabilities.ID = %q, want /capabilities", c.ID)
	}
	if c.APIVersion != "v1" {
		t.Errorf("APIVersion = %q, want v1", c.APIVersion)
	}
	wantKeys(t, "Capabilities", topLevelKeys(t, c), "id", "host", "apiVersion", "version", "os", "initSystem",
		"providers")

	none := NewCapabilities("nas", "dev", HostOS{}, "unknown", nil)
	b, err := json.Marshal(none)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"providers":[]`) {
		t.Errorf("capabilities with no providers = %s, want providers as []", b)
	}
}

func TestFrameWireShape(t *testing.T) {
	ts := time.Date(2026, 11, 7, 12, 0, 0, 5, time.UTC)
	data := DataFrame(42, ts, StreamStdout, "")
	wantKeys(t, "data Frame", topLevelKeys(t, data), "o", "t", "s", "d")
	b, _ := json.Marshal(data)
	if want := `{"o":42,"t":"2026-11-07T12:00:00.000000005Z","s":"out","d":""}`; string(b) != want {
		t.Errorf("data frame = %s, want %s", b, want)
	}

	noOffset := Frame{T: ts, S: StreamStderr, D: "x"}
	wantKeys(t, "container log Frame", topLevelKeys(t, noOffset), "t", "s", "d")

	marker := TruncatedFrame(1024)
	b, _ = json.Marshal(marker)
	if string(b) != `{"type":"truncated","lostBytes":1024}` {
		t.Errorf("truncation marker = %s", b)
	}

	var back Frame
	if err := json.Unmarshal(b, &back); err != nil || !back.Truncated() || back.LostBytes != 1024 {
		t.Errorf("marker round trip = %+v, %v", back, err)
	}
	b, _ = json.Marshal(data)
	back = Frame{}
	if err := json.Unmarshal(b, &back); err != nil || back.O == nil || *back.O != 42 || !back.T.Equal(ts) {
		t.Errorf("data frame round trip = %+v, %v", back, err)
	}
}
