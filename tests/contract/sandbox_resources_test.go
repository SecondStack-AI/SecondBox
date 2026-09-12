package contract_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/tests/openapicheck"
)

func TestSandboxResourceContract(t *testing.T) {
	document := loadOpenAPIContract(t)
	validator, err := openapicheck.Load(filepath.Join("..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := componentSchema(t, document, "SandboxResourceRequest")
	policy := object(t, componentSchema(t, document, "ResourcePolicy")["properties"], "ResourcePolicy.properties")
	properties := object(t, request["properties"], "SandboxResourceRequest.properties")
	if len(properties) != 3 || request["additionalProperties"] != false {
		t.Fatalf("request shape = %+v", request)
	}
	for _, axis := range []string{"vcpuCount", "memoryBytes", "workspaceBytes"} {
		if !reflect.DeepEqual(properties[axis], policy[axis]) {
			t.Fatalf("%s limits differ from Profile", axis)
		}
	}
	for _, test := range []struct {
		name  string
		value any
		valid bool
	}{
		{"empty", map[string]any{}, true},
		{"partial", map[string]any{"memoryBytes": float64(64 << 20)}, true},
		{"minimum", map[string]any{"vcpuCount": float64(1), "memoryBytes": float64(64 << 20), "workspaceBytes": float64(1 << 20)}, true},
		{"zero", map[string]any{"vcpuCount": float64(0)}, false},
		{"small memory", map[string]any{"memoryBytes": float64((64 << 20) - 1)}, false},
		{"small disk", map[string]any{"workspaceBytes": float64((1 << 20) - 1)}, false},
		{"fraction", map[string]any{"vcpuCount": 1.5}, false},
		{"policy override", map[string]any{"concurrentOperations": float64(1)}, false},
		{"backend override", map[string]any{"backend": "firecracker"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validator.ValidateAgainstSchema(request, test.value)
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, valid=%t", err, test.valid)
			}
		})
	}
	resolved := componentSchema(t, document, "SandboxResources")
	if err := validator.ValidateAgainstSchema(resolved, map[string]any{"vcpuCount": float64(1)}); err == nil {
		t.Fatal("resolved resources accepted missing axes")
	}
	ceiling := contracts.SandboxResources{VCPUCount: 4, MemoryBytes: 8 << 30, WorkspaceBytes: 50 << 30}
	requested := ceiling
	requested.VCPUCount = 5
	problem := contracts.Problem{Type: "https://secondbox.dev/problems/resources_exceed_profile", Title: "Requested resources exceed the Profile ceiling", Status: 400, Code: "resources_exceed_profile", RequestID: "request-resources", Ceiling: &ceiling, Requested: &requested}
	encoded, err := json.Marshal(problem)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateAgainstSchema(componentSchema(t, document, "Problem"), value); err != nil {
		t.Fatal(err)
	}
}
