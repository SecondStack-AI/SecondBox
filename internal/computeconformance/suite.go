// Package computeconformance defines reusable ComputeBackend contract checks.
package computeconformance

import (
	"context"
	"testing"
	"time"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

// Factory creates an isolated ComputeBackend for each conformance scenario.
type Factory func(*testing.T) ports.ComputeBackend

// Run verifies provider-neutral lifecycle and immutable evidence behavior.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) {
		backend := factory(t)
		ctx := context.Background()
		if err := backend.Ready(ctx); err != nil {
			t.Fatalf("Ready() error = %v", err)
		}
		request := sampleRequest()
		prepared, err := backend.Prepare(ctx, request)
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if prepared.OperationRef == "" {
			t.Fatal("Prepare() returned an empty operation reference")
		}
		running, err := backend.Start(ctx, prepared, request)
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if running.BackendRef == "" || running.State != contracts.InstanceStateReady {
			t.Fatalf("Start() = %#v, want ready opaque backend", running)
		}
		identity := ports.ComputeIdentity{
			EnvironmentID: request.Environment.ID, InstanceID: request.Instance.ID,
			Generation: request.Instance.Generation, BackendRef: running.BackendRef,
		}
		status, err := backend.Inspect(ctx, identity)
		if err != nil {
			t.Fatalf("Inspect() error = %v", err)
		}
		if status.State != contracts.InstanceStateReady {
			t.Fatalf("Inspect() state = %q, want ready", status.State)
		}
		executed, err := backend.Execute(ctx, ports.ExecuteInput{
			Identity: identity,
			Operation: contracts.ExecuteRequest{
				ContractVersion:    contracts.ContractVersionV1,
				ExpectedGeneration: identity.Generation, LeaseID: "lease_conformance",
				Operation: "exec", Command: "true",
			},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if executed.Stdout != "ok" {
			t.Fatalf("Execute() = %#v, want provider-neutral result", executed)
		}
		if err := backend.Stop(ctx, identity); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
		if err := backend.Destroy(ctx, identity); err != nil {
			t.Fatalf("Destroy() error = %v", err)
		}
		if err := backend.Purge(ctx, request.Environment, request.Workspace); err != nil {
			t.Fatalf("Purge() error = %v", err)
		}
	})

	t.Run("immutable evidence", func(t *testing.T) {
		backend := factory(t)
		ctx := context.Background()
		identity := ports.ComputeIdentity{
			EnvironmentID: "env_conformance", InstanceID: "ins_conformance",
			Generation: 7, BackendRef: "backend:opaque",
		}
		snapshot, err := backend.Checkpoint(ctx, identity)
		if err != nil {
			t.Fatalf("Checkpoint() error = %v", err)
		}
		if snapshot.OpaqueRef == "" || len(snapshot.ContentHash) != 64 {
			t.Fatalf("Checkpoint() = %#v, want opaque reference and SHA-256", snapshot)
		}
		workspaceSnapshot, err := backend.CheckpointWorkspace(ctx,
			contracts.Environment{ID: "env_conformance", TenantRef: "tenant", SubjectRef: "subject", EnvironmentKey: "environment"},
			contracts.Workspace{ID: "wsp_conformance"},
		)
		if err != nil || workspaceSnapshot.SizeBytes < 1 {
			t.Fatalf("CheckpointWorkspace() = %#v, %v", workspaceSnapshot, err)
		}
		if err := backend.MaterializeWorkspace(ctx,
			contracts.Environment{ID: "env_target", TenantRef: "tenant", SubjectRef: "subject", EnvironmentKey: "target"},
			contracts.Workspace{ID: "wsp_target"},
			contracts.Snapshot{OpaqueRef: snapshot.OpaqueRef, ContentHash: snapshot.ContentHash, SizeBytes: snapshot.SizeBytes},
		); err != nil {
			t.Fatalf("MaterializeWorkspace() error = %v", err)
		}
		artifact, err := backend.ExchangeArtifact(ctx, ports.ArtifactExchangeInput{
			Identity: identity, SourceRef: "workspace:/result.json",
			Name: "result.json", MimeType: "application/json", Metadata: map[string]string{"kind": "result"},
		})
		if err != nil {
			t.Fatalf("ExchangeArtifact() error = %v", err)
		}
		if artifact.OpaqueRef == "" || artifact.SizeBytes < 0 || len(artifact.SHA256) != 64 {
			t.Fatalf("ExchangeArtifact() = %#v, want bounded immutable evidence", artifact)
		}
	})
}

func sampleRequest() ports.ComputeRequest {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	return ports.ComputeRequest{
		Environment: contracts.Environment{
			ContractVersion: contracts.ContractVersionV1, ID: "env_conformance",
			TenantRef: "tenant", SubjectRef: "subject", EnvironmentKey: "environment",
			WorkspaceID: "wsp_conformance", ImageRef: "image@sha256:conformance",
			ToolchainRef: "toolchain:v1", ResourceClassID: contracts.ResourceClassAgentStandard,
			LifecyclePolicyID: contracts.LifecyclePolicyAgentCompartment,
			DesiredState:      contracts.DesiredStateRunning, State: contracts.EnvironmentStatePreparing,
			CurrentGeneration: 7, CurrentInstanceID: "ins_conformance",
			ExposedPorts: []contracts.ExposedPort{}, Metadata: map[string]string{},
			LastActivityAt: now, CreatedAt: now, UpdatedAt: now,
		},
		Workspace: contracts.Workspace{
			ContractVersion: contracts.ContractVersionV1, ID: "wsp_conformance",
			TenantRef: "tenant", SubjectRef: "subject", StorageRef: "workspace:wsp_conformance",
			Generation: 1, RetainUntil: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
		},
		ResourceClass: contracts.ResourceClass{
			ContractVersion: contracts.ContractVersionV1, ID: contracts.ResourceClassAgentStandard,
			CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 8 << 30, ProcessLimit: 256,
			CreatedAt: now, UpdatedAt: now,
		},
		Instance: contracts.Instance{
			ContractVersion: contracts.ContractVersionV1, ID: "ins_conformance",
			EnvironmentID: "env_conformance", Generation: 7,
			State: contracts.InstanceStatePreparing, PreparedAt: now, UpdatedAt: now,
		},
	}
}
