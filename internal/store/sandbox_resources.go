package store

import (
	"fmt"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// resolveSandboxResources runs against the revision locked by the create transaction.
func resolveSandboxResources(policy contracts.ResourcePolicy, request *contracts.SandboxResourceRequest) (contracts.SandboxResources, error) {
	ceiling := contracts.SandboxResources{VCPUCount: policy.VCPUCount, MemoryBytes: policy.MemoryBytes, WorkspaceBytes: policy.WorkspaceBytes}
	resolved := ceiling
	if request != nil {
		if request.VCPUCount != nil {
			resolved.VCPUCount = *request.VCPUCount
		}
		if request.MemoryBytes != nil {
			resolved.MemoryBytes = *request.MemoryBytes
		}
		if request.WorkspaceBytes != nil {
			resolved.WorkspaceBytes = *request.WorkspaceBytes
		}
	}
	if resolved.VCPUCount < 1 || resolved.MemoryBytes < 67108864 || resolved.WorkspaceBytes < 1048576 {
		return contracts.SandboxResources{}, fmt.Errorf("%w: SecondBox Sandbox resources are below their minimum", ports.ErrInvalidRequest)
	}
	if resolved.VCPUCount > ceiling.VCPUCount || resolved.MemoryBytes > ceiling.MemoryBytes || resolved.WorkspaceBytes > ceiling.WorkspaceBytes {
		return contracts.SandboxResources{}, &ports.ResourcesExceedProfileError{Ceiling: ceiling, Requested: resolved}
	}
	return resolved, nil
}

// sandboxPlacementSpec combines revision-owned policy with the Sandbox allocation.
func sandboxPlacementSpec(spec contracts.ProfileRevisionSpec, resources contracts.SandboxResources) contracts.ProfileRevisionSpec {
	spec.Resources.VCPUCount = resources.VCPUCount
	spec.Resources.MemoryBytes = resources.MemoryBytes
	spec.Resources.WorkspaceBytes = resources.WorkspaceBytes
	return spec
}
