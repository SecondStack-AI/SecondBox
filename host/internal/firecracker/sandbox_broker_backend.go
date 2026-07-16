package microvm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agentcy/internal/runtimemanager"
	"agentcy/internal/sandboxbroker"
)

type SandboxBrokerBackend struct {
	manager *Manager

	mu       sync.Mutex
	runtimes map[string]sandboxBrokerRuntime
}

type sandboxBrokerRuntime struct {
	identity      sandboxbroker.WorkspaceIdentity
	agentID       string
	compartmentID string
	startOpts     runtimemanager.StartOpts
}

func NewSandboxBrokerBackend(manager *Manager) (*SandboxBrokerBackend, error) {
	if manager == nil {
		return nil, errors.New("microVM manager is required")
	}
	return &SandboxBrokerBackend{manager: manager, runtimes: map[string]sandboxBrokerRuntime{}}, nil
}

func (b *SandboxBrokerBackend) Acquire(ctx context.Context, identity sandboxbroker.WorkspaceIdentity, policy sandboxbroker.LeasePolicy) (sandboxbroker.RuntimeHandle, error) {
	runtime, err := b.runtimeFor(identity, policy)
	if err != nil {
		return sandboxbroker.RuntimeHandle{}, err
	}
	lease, err := b.manager.acquireWarmToolVM(ctx, runtime.agentID, runtime.compartmentID, runtime.startOpts)
	if err != nil {
		return sandboxbroker.RuntimeHandle{}, err
	}
	b.manager.releaseWarmToolVM(lease.instanceID)
	address := sandboxBrokerRuntimeAddress(identity)
	b.mu.Lock()
	b.runtimes[address] = runtime
	b.mu.Unlock()
	return sandboxbroker.RuntimeHandle{Address: address}, nil
}

func (b *SandboxBrokerBackend) Execute(ctx context.Context, handle sandboxbroker.RuntimeHandle, operation sandboxbroker.Operation) (sandboxbroker.OperationResult, error) {
	b.mu.Lock()
	runtime, ok := b.runtimes[handle.Address]
	b.mu.Unlock()
	if !ok {
		return sandboxbroker.OperationResult{}, errors.New("sandbox broker runtime handle is not active")
	}
	request, err := sandboxBrokerToolRequest(operation)
	if err != nil {
		return sandboxbroker.OperationResult{}, err
	}
	_, response, err := b.manager.ExecuteToolLeased(ctx, runtime.agentID, runtime.compartmentID, runtime.startOpts, request)
	if err != nil {
		return sandboxbroker.OperationResult{}, err
	}
	response, err = sandboxBrokerToolResponse(operation.Kind, response)
	if err != nil {
		return sandboxbroker.OperationResult{}, err
	}
	if operation.Kind == sandboxbroker.OperationExport {
		if strings.TrimSpace(response.Error) != "" {
			return sandboxbroker.OperationResult{}, fmt.Errorf("sandbox artifact export failed: %s", response.Error)
		}
		content, err := base64.StdEncoding.DecodeString(response.ContentBase64)
		if err != nil {
			return sandboxbroker.OperationResult{}, fmt.Errorf("decode sandbox artifact export: %w", err)
		}
		if len(content) > sandboxbroker.MaxReadBytes {
			return sandboxbroker.OperationResult{}, fmt.Errorf("sandbox artifact export exceeds %d bytes", sandboxbroker.MaxReadBytes)
		}
		return sandboxbroker.NewArtifactContentResult(content), nil
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return sandboxbroker.OperationResult{}, fmt.Errorf("encode sandbox tool response: %w", err)
	}
	if len(encoded) > sandboxbroker.MaxOperationOutputBytes {
		return sandboxbroker.OperationResult{}, fmt.Errorf("sandbox tool response exceeds %d bytes", sandboxbroker.MaxOperationOutputBytes)
	}
	return sandboxbroker.OperationResult{Output: string(encoded)}, nil
}

func sandboxBrokerToolResponse(kind sandboxbroker.OperationKind, response ToolExecResponse) (ToolExecResponse, error) {
	if kind == sandboxbroker.OperationExec {
		response.Stdout = truncateSandboxOutput(response.Stdout, sandboxbroker.MaxExecStreamBytes)
		response.Stderr = truncateSandboxOutput(response.Stderr, sandboxbroker.MaxExecStreamBytes)
	}
	if kind == sandboxbroker.OperationReadFile || kind == sandboxbroker.OperationExport {
		if len(response.Content) > sandboxbroker.MaxReadBytes {
			return ToolExecResponse{}, fmt.Errorf("sandbox file read exceeds %d bytes", sandboxbroker.MaxReadBytes)
		}
		if response.ContentBase64 != "" {
			decoded, err := base64.StdEncoding.DecodeString(response.ContentBase64)
			if err != nil {
				return ToolExecResponse{}, fmt.Errorf("decode sandbox file response: %w", err)
			}
			if len(decoded) > sandboxbroker.MaxReadBytes {
				return ToolExecResponse{}, fmt.Errorf("sandbox file read exceeds %d bytes", sandboxbroker.MaxReadBytes)
			}
		}
	}
	return response, nil
}

func truncateSandboxOutput(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	const marker = "\n[output truncated by sandbox broker]"
	return value[:limit-len(marker)] + marker
}

func (b *SandboxBrokerBackend) Drain(ctx context.Context, handle sandboxbroker.RuntimeHandle) error {
	b.mu.Lock()
	runtime, ok := b.runtimes[handle.Address]
	if ok {
		delete(b.runtimes, handle.Address)
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	return b.manager.drainSandboxBrokerRuntime(ctx, runtime.agentID, runtime.compartmentID)
}

func (b *SandboxBrokerBackend) Destroy(ctx context.Context, identity sandboxbroker.WorkspaceIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.EqualFold(b.manager.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return fmt.Errorf("%w: dm-thin broker workspace destruction is not implemented", sandboxbroker.ErrInvalidPolicy)
	}
	runtime, err := sandboxBrokerRuntimeFor(identity, sandboxbroker.LeasePolicy{})
	if err != nil {
		return err
	}
	workspaceDir := filepath.Join(b.manager.cfg.MicroVMWorkspaceDir, runtime.agentID)
	workspacePath := filepath.Join(workspaceDir, runtime.compartmentID+"."+workspaceName)
	directoryInfo, err := os.Lstat(workspaceDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox broker workspace directory before destruction: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("sandbox broker workspace destruction requires a real workspace directory")
	}
	info, err := os.Lstat(workspacePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox broker workspace before destruction: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("sandbox broker workspace destruction requires a regular workspace image")
	}
	if err := os.Remove(workspacePath); err != nil {
		return fmt.Errorf("destroy sandbox broker workspace: %w", err)
	}
	directory, err := os.Open(workspaceDir)
	if err != nil {
		return fmt.Errorf("open sandbox broker workspace directory after destruction: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync sandbox broker workspace directory after destruction: %w", err)
	}
	return nil
}

func sandboxBrokerRuntimeFor(identity sandboxbroker.WorkspaceIdentity, policy sandboxbroker.LeasePolicy) (sandboxBrokerRuntime, error) {
	agentID := identity.SubjectID
	compartmentID := sandboxBrokerPhysicalCompartment(identity)
	if identity.SubjectKind == sandboxbroker.SubjectChat {
		digest := sha256.Sum256([]byte(identity.SubjectID))
		agentID = "sandbox-chat-" + hex.EncodeToString(digest[:12])
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return sandboxBrokerRuntime{}, fmt.Errorf("encode sandbox broker policy fingerprint: %w", err)
	}
	fingerprintInput := fmt.Sprintf("%s\x00%d\x00%s", identity.NamespaceKey(), identity.Generation, policyJSON)
	fingerprint := sha256.Sum256([]byte(fingerprintInput))
	return sandboxBrokerRuntime{
		identity:      identity,
		agentID:       agentID,
		compartmentID: compartmentID,
		startOpts: runtimemanager.StartOpts{
			CompartmentID:    compartmentID,
			ShapeFingerprint: hex.EncodeToString(fingerprint[:]),
			RuntimeClass:     runtimemanager.RuntimeClassToolExecutor,
		},
	}, nil
}

func (b *SandboxBrokerBackend) runtimeFor(identity sandboxbroker.WorkspaceIdentity, policy sandboxbroker.LeasePolicy) (sandboxBrokerRuntime, error) {
	runtime, err := sandboxBrokerRuntimeFor(identity, policy)
	if err != nil {
		return sandboxBrokerRuntime{}, err
	}
	const mib int64 = 1 << 20
	resource := policy.Resource
	if strings.EqualFold(b.manager.cfg.MicroVMWorkspaceBackend, "dm-thin") {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: dm-thin workspace backend cannot enforce broker disk policies", sandboxbroker.ErrInvalidPolicy)
	}
	if policy.Mount.SharedReadOnly && strings.TrimSpace(firstNonEmpty(b.manager.cfg.MicroVMToolSharedImagePath, b.manager.cfg.MicroVMSharedImagePath)) == "" {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: requested shared read-only media is not configured", sandboxbroker.ErrInvalidPolicy)
	}
	if resource.CpuMillis%1000 != 0 {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: CPU must use whole Firecracker vCPUs", sandboxbroker.ErrInvalidPolicy)
	}
	if resource.MemoryBytes%mib != 0 || resource.DiskBytes%mib != 0 {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: memory and disk must use whole MiB units", sandboxbroker.ErrInvalidPolicy)
	}
	vcpus := resource.CpuMillis / 1000
	memoryMiB := resource.MemoryBytes / mib
	workspaceMiB := resource.DiskBytes / mib
	if vcpus > int64(^uint(0)>>1) || memoryMiB > int64(^uint(0)>>1) || workspaceMiB > int64(^uint(0)>>1) || resource.ProcessLimit > int64(^uint(0)>>1) {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: resources exceed host integer range", sandboxbroker.ErrInvalidPolicy)
	}
	if b.manager.cfg.MicroVMVCPUs > 0 && int(vcpus) > b.manager.cfg.MicroVMVCPUs {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: CPU exceeds the manager maximum", sandboxbroker.ErrInvalidPolicy)
	}
	if b.manager.cfg.MicroVMMemoryMiB > 0 && int(memoryMiB) > b.manager.cfg.MicroVMMemoryMiB {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: memory exceeds the manager maximum", sandboxbroker.ErrInvalidPolicy)
	}
	if b.manager.cfg.MicroVMWorkspaceSizeMiB > 0 && int(workspaceMiB) > b.manager.cfg.MicroVMWorkspaceSizeMiB {
		return sandboxBrokerRuntime{}, fmt.Errorf("%w: disk exceeds the manager maximum", sandboxbroker.ErrInvalidPolicy)
	}
	runtime.startOpts.SandboxPolicy = &runtimemanager.SandboxRuntimePolicy{
		VCPUs:             int(vcpus),
		MemoryMiB:         int(memoryMiB),
		WorkspaceSizeMiB:  int(workspaceMiB),
		ProcessLimit:      int(resource.ProcessLimit),
		WorkspaceWritable: policy.Mount.WorkspaceWritable,
		SharedReadOnly:    policy.Mount.SharedReadOnly,
	}
	return runtime, nil
}

func sandboxBrokerPhysicalCompartment(identity sandboxbroker.WorkspaceIdentity) string {
	if identity.Generation == 1 {
		if identity.SubjectKind == sandboxbroker.SubjectChat {
			return "thread"
		}
		return identity.CompartmentID
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", identity.NamespaceKey(), identity.Generation)))
	// Keep the physical compartment short enough that the complete Firecracker
	// instance ID remains within Firecracker's 64-byte limit even for the
	// synthetic 37-byte Chat manager identity.
	return "g-" + hex.EncodeToString(digest[:6])
}

func sandboxBrokerRuntimeAddress(identity sandboxbroker.WorkspaceIdentity) string {
	return fmt.Sprintf("sandbox://%s/generation/%d", identity.NamespaceKey(), identity.Generation)
}

func sandboxBrokerToolRequest(operation sandboxbroker.Operation) (ToolExecRequest, error) {
	request := ToolExecRequest{
		Path:          operation.Path,
		TimeoutMillis: operation.TimeoutMillis,
		Recursive:     operation.Recursive,
		Force:         operation.Force,
	}
	switch operation.Kind {
	case sandboxbroker.OperationExec:
		request.Operation = ToolOpExec
		request.Command = operation.Command[0]
		request.Args = append([]string(nil), operation.Command[1:]...)
	case sandboxbroker.OperationReadFile:
		request.Operation = ToolOpReadFileBuffer
	case sandboxbroker.OperationStat:
		request.Operation = ToolOpStat
	case sandboxbroker.OperationList:
		request.Operation = ToolOpReaddir
	case sandboxbroker.OperationWriteFile:
		request.Operation = ToolOpWriteFile
		request.ContentBase64 = base64.StdEncoding.EncodeToString(operation.Content)
		request.Encoding = "base64"
	case sandboxbroker.OperationRemove:
		request.Operation = ToolOpRm
	case sandboxbroker.OperationExists:
		request.Operation = ToolOpExists
	case sandboxbroker.OperationExport:
		request.Operation = ToolOpReadFileBuffer
	default:
		return ToolExecRequest{}, fmt.Errorf("unsupported sandbox broker operation %q", operation.Kind)
	}
	return request, nil
}

func (m *Manager) drainSandboxBrokerRuntime(ctx context.Context, agentID, compartmentID string) error {
	key := runtimeInstanceKey{agentID: strings.TrimSpace(agentID), compartmentID: normalizeRuntimeCompartmentID(compartmentID)}
	m.mu.Lock()
	instanceID := m.instancesByKey[key]
	inst := m.instances[instanceID]
	if inst == nil {
		delete(m.instancesByKey, key)
		m.mu.Unlock()
		return nil
	}
	inst.draining = true
	done := inst.done
	m.promoteWarmTeardownLocked(inst)
	m.mu.Unlock()
	return waitWarmInstanceDone(ctx, done)
}
