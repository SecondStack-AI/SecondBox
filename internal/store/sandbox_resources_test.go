package store

import (
	"errors"
	"reflect"
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
			got, err := resolveSandboxResources(contracts.ProfileRevisionSpec{Resources: policy}, test.request)
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			if err == nil && got != test.want {
				t.Fatalf("resources = %+v, want %+v", got, test.want)
			}
			var exceeded *ports.ResourcesExceedProfileError
			if errors.As(err, &exceeded) && !reflect.DeepEqual(exceeded.Ceiling, contracts.SandboxResourceRequest{VCPUCount: &ceiling.VCPUCount, MemoryBytes: &ceiling.MemoryBytes, WorkspaceBytes: &ceiling.WorkspaceBytes}) {
				t.Fatalf("ceiling = %+v", exceeded.Ceiling)
			}
		})
	}
}

func TestResolveFlexibleSandboxResources(t *testing.T) {
	pointer := func(value int64) *int64 { return &value }
	policy := contracts.ResourcePolicy{VCPUCount: 1, MemoryBytes: 64 << 20, WorkspaceBytes: 50 << 30}
	for _, test := range []struct {
		name    string
		ceiling contracts.ProfileResourceCeiling
		request *contracts.SandboxResourceRequest
		disk    int64
		err     error
	}{
		{name: "unbounded continuous cpu and memory", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": pointer(256 << 30)}, request: &contracts.SandboxResourceRequest{VCPUCount: pointer(7), MemoryBytes: pointer(1<<30 + 1)}, disk: 50 << 30},
		{name: "round disk", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}, request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(20 << 30)}, disk: 32 << 30},
		{name: "round near minimum", request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(1<<20 + 1)}, disk: 2 << 20},
		{name: "fits under a non-power-of-two ceiling resolves to the ceiling", request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(50 << 30)}, disk: 50 << 30},
		{name: "fits under an explicit ceiling resolves to the ceiling", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": pointer(48 << 30)}, request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(33 << 30)}, disk: 48 << 30},
		{name: "above the ceiling is refused after rounding", request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(50<<30 + 1)}, err: ports.ErrResourcesExceedProfile},
		{name: "explicit upper bound", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": pointer(2), "memoryBytes": nil, "workspaceBytes": nil}, request: &contracts.SandboxResourceRequest{VCPUCount: pointer(3)}, err: ports.ErrResourcesExceedProfile},
		{name: "largest rounded capacity", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}, request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(1 << 62)}, disk: 1 << 62},
		{name: "round overflow", ceiling: contracts.ProfileResourceCeiling{"vcpuCount": nil, "memoryBytes": nil, "workspaceBytes": nil}, request: &contracts.SandboxResourceRequest{WorkspaceBytes: pointer(1<<62 + 1)}, err: ports.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := contracts.ProfileRevisionSpec{Resources: policy, ResourceCeiling: test.ceiling}
			got, err := resolveSandboxResources(spec, test.request)
			if !errors.Is(err, test.err) {
				t.Fatalf("error=%v want=%v", err, test.err)
			}
			if err == nil {
				if got.WorkspaceBytes != test.disk {
					t.Fatalf("disk=%d want=%d", got.WorkspaceBytes, test.disk)
				}
				if test.request.VCPUCount != nil && got.VCPUCount != *test.request.VCPUCount {
					t.Fatalf("cpu changed: %+v", got)
				}
				if test.request.MemoryBytes != nil && got.MemoryBytes != *test.request.MemoryBytes {
					t.Fatalf("memory changed: %+v", got)
				}
			}
			var exceeded *ports.ResourcesExceedProfileError
			if errors.As(err, &exceeded) && test.ceiling != nil && (exceeded.Ceiling.MemoryBytes != nil || exceeded.Ceiling.WorkspaceBytes != nil) {
				t.Fatalf("unbounded axes included: %+v", exceeded.Ceiling)
			}
		})
	}
}

func TestResolveResumeSandboxResources(t *testing.T) {
	pointer := func(value int64) *int64 { return &value }
	spec := contracts.ProfileRevisionSpec{Resources: contracts.ResourcePolicy{VCPUCount: 4, MemoryBytes: 1 << 30, WorkspaceBytes: 50 << 30}, Startup: contracts.StartupPolicy{Mode: contracts.StartupModeSnapshotResume}}
	// Malformed requests must not produce a fixed-size problem whose requested
	// resources violate the response schema minimums.
	for _, request := range []*contracts.SandboxResourceRequest{{VCPUCount: pointer(0)}, {MemoryBytes: pointer(-1)}, {WorkspaceBytes: pointer(1)}} {
		if _, err := resolveSandboxResources(spec, request); !errors.Is(err, ports.ErrInvalidRequest) {
			t.Fatalf("invalid resume resources error=%v", err)
		}
	}
	for _, request := range []*contracts.SandboxResourceRequest{nil, {}, {VCPUCount: pointer(4)}, {VCPUCount: pointer(4), MemoryBytes: pointer(1 << 30), WorkspaceBytes: pointer(50 << 30)}} {
		got, err := resolveSandboxResources(spec, request)
		if err != nil || got.WorkspaceBytes != 50<<30 {
			t.Fatalf("equal resume resources=%+v error=%v", got, err)
		}
	}
	for _, request := range []*contracts.SandboxResourceRequest{{VCPUCount: pointer(3)}, {MemoryBytes: pointer(512 << 20)}, {WorkspaceBytes: pointer(32 << 30)}, {WorkspaceBytes: pointer(64 << 30)}} {
		_, err := resolveSandboxResources(spec, request)
		var fixed *ports.ResourcesFixedByProfileError
		if !errors.Is(err, ports.ErrResourcesFixedByProfile) || !errors.As(err, &fixed) || *fixed.Fixed.WorkspaceBytes != 50<<30 {
			t.Fatalf("resume refusal=%v", err)
		}
	}
}
