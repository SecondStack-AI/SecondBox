package store

import (
	"fmt"
	"math/bits"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// resolveSandboxResources runs against the revision locked by the create transaction.
func resolveSandboxResources(spec contracts.ProfileRevisionSpec, request *contracts.SandboxResourceRequest) (contracts.SandboxResources, error) {
	policy := spec.Resources
	defaults := contracts.SandboxResources{VCPUCount: policy.VCPUCount, MemoryBytes: policy.MemoryBytes, WorkspaceBytes: policy.WorkspaceBytes}
	resolved := defaults
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
	ceiling := contracts.SandboxResourceRequest{VCPUCount: &policy.VCPUCount, MemoryBytes: &policy.MemoryBytes, WorkspaceBytes: &policy.WorkspaceBytes}
	// Resume identity is exact, including non-power-of-two Profile defaults. Equal
	// explicit values are accepted unchanged; rounding cannot alter that identity.
	if spec.Startup.Mode == contracts.StartupModeSnapshotResume {
		if resolved != defaults {
			return contracts.SandboxResources{}, &ports.ResourcesFixedByProfileError{Fixed: ceiling, Requested: resolved}
		}
		return resolved, nil
	}
	if spec.ResourceCeiling != nil {
		ceiling = contracts.SandboxResourceRequest{VCPUCount: spec.ResourceCeiling["vcpuCount"], MemoryBytes: spec.ResourceCeiling["memoryBytes"], WorkspaceBytes: spec.ResourceCeiling["workspaceBytes"]}
	}
	if request != nil && request.WorkspaceBytes != nil {
		// The next power of two must remain representable as a positive int64.
		if resolved.WorkspaceBytes > 1<<62 {
			return contracts.SandboxResources{}, fmt.Errorf("%w: SecondBox Sandbox rounded workspaceBytes exceeds int64 capacity", ports.ErrInvalidRequest)
		}
		requested := resolved.WorkspaceBytes
		resolved.WorkspaceBytes = int64(1) << bits.Len64(uint64(requested-1))
		// Rounding never turns a request that fits into a refusal: a bounded
		// axis whose ceiling is not a power of two (durable-coding's 50 GiB)
		// resolves to the ceiling itself, which the Runner also serves.
		if ceiling.WorkspaceBytes != nil && requested <= *ceiling.WorkspaceBytes && resolved.WorkspaceBytes > *ceiling.WorkspaceBytes {
			resolved.WorkspaceBytes = *ceiling.WorkspaceBytes
		}
	}
	if (ceiling.VCPUCount != nil && resolved.VCPUCount > *ceiling.VCPUCount) ||
		(ceiling.MemoryBytes != nil && resolved.MemoryBytes > *ceiling.MemoryBytes) ||
		(ceiling.WorkspaceBytes != nil && resolved.WorkspaceBytes > *ceiling.WorkspaceBytes) {
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
