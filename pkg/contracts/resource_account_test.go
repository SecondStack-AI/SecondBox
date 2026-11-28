package contracts

import (
	"encoding/json"
	"testing"
)

func TestResourceAccountRequiresCompleteLimitsOnCreateAndStart(t *testing.T) {
	for _, field := range []string{
		"null",
		`{"key":"test"}`,
		`{"key":"test","limits":{"maxActiveInstances":null,"maxVcpuCount":0}}`,
		`{"key":"test","limits":null}`,
		`{"key":"test","limits":{"maxActiveInstances":-1,"maxVcpuCount":0,"maxMemoryBytes":0}}`,
		`{"key":"test","limits":{"maxActiveInstances":0.5,"maxVcpuCount":0,"maxMemoryBytes":0}}`,
		`{"key":"test","limits":{"maxActiveInstances":0,"maxVcpuCount":0,"maxMemoryBytes":0,"extra":1}}`,
		`{"key":"bad/key","limits":{"maxActiveInstances":0,"maxVcpuCount":0,"maxMemoryBytes":0}}`,
	} {
		body := []byte(`{"resourceAccount":` + field + "}")
		for _, request := range []any{new(CreateSandboxRequest), new(StartSandboxRequest)} {
			if err := json.Unmarshal(body, request); err == nil {
				t.Errorf("%T accepted invalid resource account %s", request, field)
			}
		}
	}
	var start StartSandboxRequest
	if err := json.Unmarshal([]byte(`{"resourceAccount":{"key":"human:123","limits":{"maxActiveInstances":null,"maxVcpuCount":0,"maxMemoryBytes":1048576}}}`), &start); err != nil {
		t.Fatal(err)
	}
	if start.ResourceAccount == nil || start.ResourceAccount.Limits.MaxActiveInstances != Unlimited || start.ResourceAccount.Limits.MaxVCPUCount != 0 {
		t.Fatalf("nullable limits: %+v", start.ResourceAccount)
	}
}
