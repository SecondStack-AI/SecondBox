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
	problem := contracts.Problem{Type: "https://secondbox.dev/problems/resources_exceed_profile", Title: "Requested resources exceed the Profile ceiling", Status: 400, Code: "resources_exceed_profile", RequestID: "request-resources", Ceiling: &contracts.SandboxResourceRequest{VCPUCount: &ceiling.VCPUCount, MemoryBytes: &ceiling.MemoryBytes, WorkspaceBytes: &ceiling.WorkspaceBytes}, Requested: &requested}
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

func TestProfileResourceCeilingContract(t *testing.T) {
	document := loadOpenAPIContract(t)
	validator, err := openapicheck.Load(filepath.Join("..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec := componentSchema(t, document, "ProfileRevisionSpec")
	properties := object(t, spec["properties"], "ProfileRevisionSpec.properties")
	if object(t, properties["resourceCeiling"], "resourceCeiling")["$ref"] != "#/components/schemas/ProfileResourceCeiling" {
		t.Fatal("Profile ceiling schema not referenced")
	}
	for _, required := range array(t, spec["required"], "ProfileRevisionSpec.required") {
		if required == "resourceCeiling" {
			t.Fatal("ceiling must remain optional")
		}
	}
	ceiling := componentSchema(t, document, "ProfileResourceCeiling")
	for _, test := range []struct {
		name, input string
		valid       bool
	}{
		{"null axes", `{"vcpuCount":null,"memoryBytes":null,"workspaceBytes":null}`, true},
		{"mixed", `{"vcpuCount":1,"memoryBytes":null,"workspaceBytes":274877906944}`, true},
		{"finite", `{"vcpuCount":1,"memoryBytes":67108864,"workspaceBytes":1048576}`, true},
		{"empty", `{}`, false},
		{"missing cpu", `{"memoryBytes":null,"workspaceBytes":null}`, false},
		{"missing memory", `{"vcpuCount":null,"workspaceBytes":null}`, false},
		{"missing disk", `{"vcpuCount":null,"memoryBytes":null}`, false},
		{"zero", `{"vcpuCount":0,"memoryBytes":null,"workspaceBytes":null}`, false},
		{"fraction", `{"vcpuCount":1.5,"memoryBytes":null,"workspaceBytes":null}`, false},
		{"small memory", `{"vcpuCount":null,"memoryBytes":1,"workspaceBytes":null}`, false},
		{"small disk", `{"vcpuCount":null,"memoryBytes":null,"workspaceBytes":1}`, false},
		{"extra", `{"vcpuCount":null,"memoryBytes":null,"workspaceBytes":null,"other":null}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.input), &value); err != nil {
				t.Fatal(err)
			}
			if err := validator.ValidateAgainstSchema(ceiling, value); (err == nil) != test.valid {
				t.Fatalf("validation=%v valid=%t", err, test.valid)
			}
		})
	}
	for _, code := range []string{"resources_exceed_profile", "resources_fixed_by_profile"} {
		t.Run(code, func(t *testing.T) {
			memory := int64(1 << 30)
			problem := contracts.Problem{Type: "https://secondbox.dev/problems/" + code, Title: "Profile size bound", Status: 400, Code: code, RequestID: "request-resources", Ceiling: &contracts.SandboxResourceRequest{MemoryBytes: &memory}, Requested: &contracts.SandboxResources{VCPUCount: 1, MemoryBytes: 2 << 30, WorkspaceBytes: 1 << 20}}
			encoded, err := json.Marshal(problem)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			bounds := value["ceiling"].(map[string]any)
			if len(bounds) != 1 || bounds["memoryBytes"] != float64(memory) {
				t.Fatalf("unbounded axes included: %s", encoded)
			}
			if err := validator.ValidateAgainstSchema(componentSchema(t, document, "Problem"), value); err != nil {
				t.Fatal(err)
			}
		})
	}
}
