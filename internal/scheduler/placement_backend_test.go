package scheduler

import "testing"

// TestMaterializationMatchRequiresSameBackendKind pins the provenance
// invariant the gVisor host-mount design depends on: a materialization
// advertised for one backend kind can never satisfy placement onto a runner
// of another kind, so a gVisor runner can never become home to a Workspace
// created under a KVM backend, and relocation (which reuses this placement)
// can only pair runners of matching kinds.
func TestMaterializationMatchRequiresSameBackendKind(t *testing.T) {
	runtimeDigest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	toolchainDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	snapshot := RunnerSnapshot{
		BackendKind:  "gvisor",
		Architecture: "amd64",
		Materializations: []MaterializationSnapshot{{
			BackendKind:     "firecracker",
			Architecture:    "amd64",
			RuntimeDigest:   runtimeDigest,
			ToolchainDigest: toolchainDigest,
			Digest:          "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		}},
	}
	if hasMaterialization(snapshot, runtimeDigest, toolchainDigest) {
		t.Fatal("cross-backend materialization satisfied placement")
	}
	snapshot.Materializations[0].BackendKind = "gvisor"
	if !hasMaterialization(snapshot, runtimeDigest, toolchainDigest) {
		t.Fatal("matching-backend materialization did not satisfy placement")
	}
}
