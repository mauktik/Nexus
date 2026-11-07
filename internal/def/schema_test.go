package def

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mauktik/nexus/internal/apitypes"
)

type modeInput struct {
	Mode string `json:"mode"`
}

type nestedInput struct {
	Opts struct {
		Level int `json:"level"`
	} `json:"opts"`
}

func decodeInstance(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

// TC-5: schema generation applies Refine, closes objects, and caches.
func TestSchemaAppliesRefineAndClosesObjects(t *testing.T) {
	plain := Define[modeInput, struct{}](readOp("test.schema.plain"))

	calls := 0
	refinedOp := readOp("test.schema.refined")
	refinedOp.Refine = func(s *jsonschema.Schema) {
		calls++
		s.Properties["mode"].Enum = []any{"standard", "full"}
	}
	refined := Define[modeInput, struct{}](refinedOp)

	hostile := decodeInstance(t, `{"mode":"a; rm -rf /"}`)

	plainSchema, err := plain.Op().InputSchema()
	if err != nil {
		t.Fatalf("plain InputSchema: %v", err)
	}
	// Control that keeps A-42 honest: jsonschema.For alone derives no enum.
	if err := plainSchema.Validate(hostile); err != nil {
		t.Fatalf("unrefined schema rejected %v: %v; the A-42 control expects it to pass", hostile, err)
	}

	refinedSchema, err := refined.Op().InputSchema()
	if err != nil {
		t.Fatalf("refined InputSchema: %v", err)
	}
	if err := refinedSchema.Validate(hostile); err == nil {
		t.Fatal("refined schema accepted a value outside its enum")
	}
	if err := refinedSchema.Validate(decodeInstance(t, `{"mode":"standard"}`)); err != nil {
		t.Fatalf("refined schema rejected an enum value: %v", err)
	}
	if err := refinedSchema.Validate(decodeInstance(t, `{"mode":"standard","extra":1}`)); err == nil {
		t.Fatal("refined schema accepted an extra property; additionalProperties must be false")
	}
	if err := plainSchema.Validate(decodeInstance(t, `{"mode":"x","extra":1}`)); err == nil {
		t.Fatal("unrefined schema accepted an extra property; objects must be closed without Refine too")
	}

	again, err := refined.Op().InputSchema()
	if err != nil {
		t.Fatalf("second InputSchema: %v", err)
	}
	if again != refinedSchema {
		t.Fatal("second resolve returned a different schema; want the cached one")
	}
	if calls != 1 {
		t.Fatalf("Refine ran %d times across two resolves, want 1", calls)
	}
}

func TestSchemaClosesNestedObjects(t *testing.T) {
	op := Define[nestedInput, struct{}](readOp("test.schema.nested"))
	s, err := op.Op().InputSchema()
	if err != nil {
		t.Fatalf("InputSchema: %v", err)
	}
	if err := s.Validate(decodeInstance(t, `{"opts":{"level":1,"x":true}}`)); err == nil {
		t.Fatal("nested object accepted an extra property")
	}
}

func TestOutputSchemaDoesNotRunRefine(t *testing.T) {
	op := readOp("test.schema.output")
	op.Refine = func(*jsonschema.Schema) { t.Fatal("Refine ran for the output schema") }
	typed := Define[struct{}, modeInput](op)
	s, err := typed.Op().OutputSchema()
	if err != nil {
		t.Fatalf("OutputSchema: %v", err)
	}
	if err := s.Validate(decodeInstance(t, `{"mode":"anything"}`)); err != nil {
		t.Fatalf("output schema rejected a valid value: %v", err)
	}
}

func TestSchemaOfUndefinedOpFails(t *testing.T) {
	if _, err := (Op{ID: "never.defined"}).InputSchema(); err == nil {
		t.Fatal("InputSchema on an Op not built by Define returned no error")
	}
}

// Wire types are op outputs, so jsonschema.For must handle each of them.
func TestWireTypesGenerateOutputSchemas(t *testing.T) {
	ops := []Op{
		Define[struct{}, apitypes.Operation](readOp("test.wire.operation")).Op(),
		Define[struct{}, apitypes.List[apitypes.Resource[map[string]string]]](readOp("test.wire.list")).Op(),
		Define[struct{}, apitypes.Capabilities](readOp("test.wire.capabilities")).Op(),
		Define[struct{}, apitypes.Frame](readOp("test.wire.frame")).Op(),
	}
	for _, op := range ops {
		if _, err := op.OutputSchema(); err != nil {
			t.Errorf("%s: OutputSchema: %v", op.ID, err)
		}
	}
}
