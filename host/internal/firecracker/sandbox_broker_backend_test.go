package microvm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	managedagents "agentcy/contracts/managed-agents/v1/gen/go/managedagents"
	"agentcy/internal/runtimemanager"
	"agentcy/internal/sandboxbroker"
)

func TestSandboxBrokerMicroVMBackendIsLazyReusableAndResetFenced(t *testing.T) {
	manager := newWarmToolTestManager(t)
	var boots atomic.Int32
	manager.startCompartment = func(_ context.Context, agentID, compartmentID string, opts runtimemanager.StartOpts) (string, error) {
		n := boots.Add(1)
		id := fmt.Sprintf("fc-%s-%s-%d", agentID, compartmentID, n)
		fingerprint, err := manager.startupFingerprint(agentID, compartmentID, opts)
		if err != nil {
			return "", err
		}
		manager.mu.Lock()
		manager.addInstanceLocked(&instance{
			id:                 id,
			agentID:            agentID,
			compartmentID:      compartmentID,
			startupFingerprint: fingerprint,
			done:               make(chan struct{}),
		})
		manager.mu.Unlock()
		return id, nil
	}
	var executions atomic.Int32
	manager.executeTool = func(_ context.Context, _ string, request ToolExecRequest) (ToolExecResponse, error) {
		executions.Add(1)
		return ToolExecResponse{Exists: boolPointer(request.Path == "state.txt")}, nil
	}
	manager.freezeWorkspace = func(context.Context, string) (BackupResponse, error) {
		return BackupResponse{Frozen: true}, nil
	}
	manager.removeInstance = func(_ context.Context, instanceID string) error {
		manager.finishInstance(manager.lookup(instanceID))
		return nil
	}

	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatalf("new sandbox broker backend: %v", err)
	}
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	nextLease := 0
	service, err := sandboxbroker.New(sandboxbroker.Config{
		Backend:          backend,
		ArtifactExporter: rejectingArtifactExporter{},
		Generations:      sandboxbroker.NewMemoryGenerationStore(),
		Activity:         sandboxbroker.NewMemoryActivityStore(),
		LeaseTTL:         time.Minute,
		CleanupInterval:  time.Second,
		IdleTTL: map[sandboxbroker.SubjectKind]time.Duration{
			sandboxbroker.SubjectAgent: 10 * time.Minute,
			sandboxbroker.SubjectChat:  2 * time.Minute,
		},
		RetentionTTL: map[sandboxbroker.SubjectKind]time.Duration{
			sandboxbroker.SubjectAgent: 30 * 24 * time.Hour,
			sandboxbroker.SubjectChat:  24 * time.Hour,
		},
		Now: func() time.Time { return now },
		NewLeaseID: func() string {
			nextLease++
			return fmt.Sprintf("lease-%d", nextLease)
		},
	})
	if err != nil {
		t.Fatalf("new sandbox broker: %v", err)
	}
	workspace := sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: "agent-1", CompartmentID: "cmp-1"}
	policy := sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 128 << 20, DiskBytes: 1 << 20, ProcessLimit: 16},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: true, SharedReadOnly: true},
	}

	inspection, err := service.Inspect(context.Background(), workspace)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspection.ComputeActive || boots.Load() != 0 || len(manager.instances) != 0 {
		t.Fatalf("inspection started compute: inspection=%+v boots=%d instances=%d", inspection, boots.Load(), len(manager.instances))
	}

	for i := 0; i < 2; i++ {
		_, err := service.Execute(context.Background(), sandboxbroker.ExecuteRequest{
			Workspace: workspace,
			Policy:    policy,
			Operation: sandboxbroker.Operation{Kind: sandboxbroker.OperationExists, Path: "state.txt"},
		})
		if err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	if boots.Load() != 1 || executions.Load() != 2 {
		t.Fatalf("boots=%d executions=%d, want 1/2", boots.Load(), executions.Load())
	}
	manager.mu.Lock()
	var firstGenerationCompartment string
	for _, inst := range manager.instances {
		firstGenerationCompartment = inst.compartmentID
	}
	manager.mu.Unlock()

	oldLease, err := service.Acquire(context.Background(), sandboxbroker.AcquireRequest{Workspace: workspace, Policy: policy})
	if err != nil {
		t.Fatalf("acquire old lease: %v", err)
	}
	reset, err := service.Reset(context.Background(), workspace)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if reset.Generation != 2 {
		t.Fatalf("reset generation=%d", reset.Generation)
	}
	if _, err := service.Renew(context.Background(), string(oldLease.LeaseId)); !errors.Is(err, sandboxbroker.ErrLeaseFenced) {
		t.Fatalf("old lease renew err=%v, want fenced", err)
	}
	manager.mu.Lock()
	remaining := len(manager.instances)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("reset left %d warm instances", remaining)
	}

	if _, err := service.Execute(context.Background(), sandboxbroker.ExecuteRequest{
		Workspace: workspace,
		Policy:    policy,
		Operation: sandboxbroker.Operation{Kind: sandboxbroker.OperationExists, Path: "state.txt"},
	}); err != nil {
		t.Fatalf("execute after reset: %v", err)
	}
	if boots.Load() != 2 {
		t.Fatalf("boots after reset=%d, want 2", boots.Load())
	}
	manager.mu.Lock()
	var secondGenerationCompartment string
	for _, inst := range manager.instances {
		secondGenerationCompartment = inst.compartmentID
	}
	manager.mu.Unlock()
	if firstGenerationCompartment == "" || secondGenerationCompartment == "" || firstGenerationCompartment == secondGenerationCompartment {
		t.Fatalf("workspace generation was reused: first=%q second=%q", firstGenerationCompartment, secondGenerationCompartment)
	}
	if len(secondGenerationCompartment) > 14 {
		t.Fatalf("generation compartment %q exceeds Firecracker-safe length", secondGenerationCompartment)
	}
}

type rejectingArtifactExporter struct{}

func (rejectingArtifactExporter) Supports(sandboxbroker.SubjectKind) bool { return true }

func (rejectingArtifactExporter) Export(context.Context, sandboxbroker.ArtifactExportInput) (sandboxbroker.StoredArtifact, error) {
	return sandboxbroker.StoredArtifact{}, errors.New("artifact export is not used by this test")
}

func TestSandboxBrokerRevisionChangesWithSelectedToolImage(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.cfg.MicroVMToolRootfsPath = manager.cfg.MicroVMRootfsPath
	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectAgent, SubjectID: "agent-1", CompartmentID: "cmp-1"},
		Generation:   1,
	}
	policy := sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 128 << 20, DiskBytes: 1 << 20, ProcessLimit: 16},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: true, SharedReadOnly: true},
	}
	first, err := backend.Revision(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	nextTime := time.Now().Add(time.Second)
	if err := os.WriteFile(manager.cfg.MicroVMToolRootfsPath, []byte("changed-tool-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(manager.cfg.MicroVMToolRootfsPath, nextTime, nextTime); err != nil {
		t.Fatal(err)
	}
	second, err := backend.Revision(context.Background(), identity, policy)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("selected tool image change did not change sandbox broker revision")
	}
}

func TestSandboxBrokerDestroyRemovesOnlyDerivedWorkspaceImage(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.cfg.MicroVMWorkspaceDir = filepath.Join(t.TempDir(), "workspaces")
	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectChat, SubjectID: "thread-delete"},
		Generation:   1,
	}
	runtime, err := sandboxBrokerRuntimeFor(identity, sandboxbroker.LeasePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(manager.cfg.MicroVMWorkspaceDir, runtime.agentID, runtime.compartmentID+"."+workspaceName)
	sibling := filepath.Join(manager.cfg.MicroVMWorkspaceDir, runtime.agentID, "other."+workspaceName)
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{workspace, sibling} {
		if err := os.WriteFile(path, []byte("workspace"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := backend.Destroy(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed workspace stat err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling workspace was removed: %v", err)
	}
}

func TestSandboxBrokerRuntimeUsesFirecrackerSafeManagerIdentity(t *testing.T) {
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{
			SubjectKind:   sandboxbroker.SubjectAgent,
			SubjectID:     "11111111-1111-4111-8111-111111111111",
			CompartmentID: "22222222-2222-4222-8222-222222222222",
		},
		Generation: 1,
	}
	runtime, err := sandboxBrokerRuntimeFor(identity, sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 4 << 30, ProcessLimit: 128},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := newInstanceID(runtime.agentID, runtime.compartmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instanceID) > 64 {
		t.Fatalf("instance ID is %d bytes: %q", len(instanceID), instanceID)
	}
}

func TestSandboxBrokerRuntimeTranslatesEnforceablePolicy(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.cfg.MicroVMVCPUs = 2
	manager.cfg.MicroVMMemoryMiB = 512
	manager.cfg.MicroVMWorkspaceSizeMiB = 4096
	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectChat, SubjectID: "thread-1"},
		Generation:   1,
	}
	runtime, err := backend.runtimeFor(identity, sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 128 << 20, DiskBytes: 64 << 20, ProcessLimit: 16},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: false, SharedReadOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runtime.startOpts.SandboxPolicy
	if got == nil || got.VCPUs != 1 || got.MemoryMiB != 128 || got.WorkspaceSizeMiB != 64 || got.ProcessLimit != 16 || got.WorkspaceWritable || !got.SharedReadOnly {
		t.Fatalf("sandbox runtime policy = %+v", got)
	}
}

func TestSandboxBrokerRuntimeRejectsPolicyFirecrackerCannotEnforce(t *testing.T) {
	manager := newWarmToolTestManager(t)
	manager.cfg.MicroVMVCPUs = 2
	manager.cfg.MicroVMMemoryMiB = 512
	manager.cfg.MicroVMWorkspaceSizeMiB = 4096
	backend, err := NewSandboxBrokerBackend(manager)
	if err != nil {
		t.Fatal(err)
	}
	identity := sandboxbroker.WorkspaceIdentity{
		WorkspaceRef: sandboxbroker.WorkspaceRef{SubjectKind: sandboxbroker.SubjectChat, SubjectID: "thread-1"},
		Generation:   1,
	}
	base := sandboxbroker.LeasePolicy{
		Resource: managedagents.ResourcePolicy{CpuMillis: 1000, MemoryBytes: 128 << 20, DiskBytes: 64 << 20, ProcessLimit: 16},
		Mount:    managedagents.MountPolicy{WorkspaceWritable: true},
	}
	tests := []struct {
		name   string
		mutate func(*sandboxbroker.LeasePolicy)
	}{
		{name: "fractional vcpu", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.CpuMillis = 1500 }},
		{name: "cpu above manager maximum", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.CpuMillis = 3000 }},
		{name: "fractional memory MiB", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.MemoryBytes++ }},
		{name: "memory above manager maximum", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.MemoryBytes = 513 << 20 }},
		{name: "fractional disk MiB", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.DiskBytes++ }},
		{name: "disk above manager maximum", mutate: func(policy *sandboxbroker.LeasePolicy) { policy.Resource.DiskBytes = 4097 << 20 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := base
			test.mutate(&policy)
			if _, err := backend.runtimeFor(identity, policy); !errors.Is(err, sandboxbroker.ErrInvalidPolicy) {
				t.Fatalf("policy error = %v, want invalid policy", err)
			}
		})
	}
}

func TestSandboxBrokerToolResponseBoundsModelVisibleOutput(t *testing.T) {
	response := ToolExecResponse{
		Stdout: strings.Repeat("o", sandboxbroker.MaxExecStreamBytes+100),
		Stderr: strings.Repeat("e", sandboxbroker.MaxExecStreamBytes+100),
	}
	bounded, err := sandboxBrokerToolResponse(sandboxbroker.OperationExec, response)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.Stdout) > sandboxbroker.MaxExecStreamBytes || len(bounded.Stderr) > sandboxbroker.MaxExecStreamBytes {
		t.Fatalf("bounded response lengths = %d/%d", len(bounded.Stdout), len(bounded.Stderr))
	}
	if !strings.Contains(bounded.Stdout, "truncated") || !strings.Contains(bounded.Stderr, "truncated") {
		t.Fatalf("truncation was not disclosed: %+v", bounded)
	}

	_, err = sandboxBrokerToolResponse(sandboxbroker.OperationReadFile, ToolExecResponse{
		ContentBase64: strings.Repeat("A", ((sandboxbroker.MaxReadBytes+1)+2)/3*4),
	})
	if err == nil {
		t.Fatal("expected oversized read response rejection")
	}
}

func TestSandboxBrokerArtifactExportUsesBoundedGuestBufferRead(t *testing.T) {
	request, err := sandboxBrokerToolRequest(sandboxbroker.Operation{Kind: sandboxbroker.OperationExport, Path: "reports/final.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation != ToolOpReadFileBuffer || request.Path != "reports/final.bin" {
		t.Fatalf("artifact export tool request = %+v", request)
	}
	oversized := ToolExecResponse{ContentBase64: base64.StdEncoding.EncodeToString(make([]byte, sandboxbroker.MaxReadBytes+1))}
	if _, err := sandboxBrokerToolResponse(sandboxbroker.OperationExport, oversized); err == nil {
		t.Fatal("oversized artifact export response unexpectedly passed")
	}
}

func boolPointer(value bool) *bool {
	return &value
}
