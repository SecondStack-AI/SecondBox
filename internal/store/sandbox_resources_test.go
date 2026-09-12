package store

import (
	"errors"
	"testing"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestResolveSandboxResources(t *testing.T) {
	policy := contracts.ResourcePolicy{VCPUCount: 4, MemoryBytes: 8 << 30, WorkspaceBytes: 50 << 30, ConcurrentOperations: 10}
	ceiling := contracts.SandboxResources{VCPUCount: 4, MemoryBytes: 8 << 30, WorkspaceBytes: 50 << 30}
	pointer := func(n int64) *int64 { return &n }
	for _, test := range []struct {
		name    string
		request *contracts.SandboxResourceRequest
		want    contracts.SandboxResources
		err     error
	}{
		{name: "omitted", want: ceiling},
		{name: "empty", request: &contracts.SandboxResourceRequest{}, want: ceiling},
		{name: "partial", request: &contracts.SandboxResourceRequest{MemoryBytes: pointer(2 << 30)}, want: contracts.SandboxResources{VCPUCount: 4, MemoryBytes: 2 << 30, WorkspaceBytes: 50 << 30}},
		{name: "minimum", request: &contracts.SandboxResourceRequest{VCPUCount: pointer(1), MemoryBytes: pointer(64 << 20), WorkspaceBytes: pointer(1 << 20)}, want: contracts.SandboxResources{VCPUCount: 1, MemoryBytes: 64 << 20, WorkspaceBytes: 1 << 20}},
		{name: "cpu ceiling", request: &contracts.SandboxResourceRequest{VCPUCount: pointer(5)}, err: ports.ErrResourcesExceedProfile},
		{name: "memory ceiling", request: &contracts.SandboxResourceRequest{MemoryBytes: pointer(9 << 30)}, err: ports.ErrResourcesExceedProfile},
		{name: "disk ceiling", request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(51 << 30)}, err: ports.ErrResourcesExceedProfile},
		{name: "zero cpu", request: &contracts.SandboxResourceRequest{VCPUCount: pointer(0)}, err: ports.ErrInvalidRequest},
		{name: "negative memory", request: &contracts.SandboxResourceRequest{MemoryBytes: pointer(-1)}, err: ports.ErrInvalidRequest},
		{name: "small disk", request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer((1 << 20) - 1)}, err: ports.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveSandboxResources(policy, test.request)
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			if err == nil && got != test.want {
				t.Fatalf("resources = %+v, want %+v", got, test.want)
			}
			var exceeded *ports.ResourcesExceedProfileError
			if errors.As(err, &exceeded) && exceeded.Ceiling != ceiling {
				t.Fatalf("ceiling = %+v", exceeded.Ceiling)
			}
		})
	}
}
