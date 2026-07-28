// Package ports defines Sandbox Service interfaces in Sandbox domain language.
package ports

import (
	"context"
	"errors"
	"io"
	"time"

	"secondstack/sandbox-service/pkg/contracts"
)

var (
	ErrEnvironmentNotFound = errors.New("sandbox environment not found")
	ErrLeaseNotFound       = errors.New("sandbox lease not found")
	ErrGenerationFenced    = errors.New("sandbox generation fenced")
	ErrLeaseExpired        = errors.New("sandbox lease expired")
	ErrLeaseReleased       = errors.New("sandbox lease released")
	ErrEnvironmentBusy     = errors.New("sandbox environment has active leases")
)

// ResolveEnvironmentInput captures validated durable Environment intent.
type ResolveEnvironmentInput struct {
	Environment contracts.Environment
	Workspace   contracts.Workspace
}

// StartGenerationResult reserves one fenced generation for a replaceable Instance.
type StartGenerationResult struct {
	Environment contracts.Environment
	Instance    contracts.Instance
	Created     bool
}

// ExecutionRevoker fences Agent execution after Sandbox commits an Agent-compartment stop.
type ExecutionRevoker interface {
	RevokeEnvironmentExecutions(context.Context, string) error
}

// EnvironmentStore persists Environment intent and fenced lifecycle evidence.
type EnvironmentStore interface {
	Ping(context.Context) error
	CountRetainedWorkspaces(context.Context) (int64, error)
	GetWorkspaceUsage(context.Context, string, string) (contracts.WorkspaceUsage, error)
	ResolveEnvironment(context.Context, ResolveEnvironmentInput) (contracts.Environment, bool, error)
	GetEnvironment(context.Context, string) (contracts.Environment, error)
	GetWorkspace(context.Context, string) (contracts.Workspace, error)
	GetCurrentInstance(context.Context, string) (*contracts.Instance, error)
	ListInstances(context.Context, string) ([]contracts.Instance, error)
	GetResourceClass(context.Context, string) (contracts.ResourceClass, error)
	ListResourceClasses(context.Context) ([]contracts.ResourceClass, error)
	GetLifecyclePolicy(context.Context, string) (contracts.LifecyclePolicy, error)
	ListLifecyclePolicies(context.Context) ([]contracts.LifecyclePolicy, error)
	BeginStart(context.Context, string, int64, string, time.Time) (StartGenerationResult, error)
	MarkInstanceReady(context.Context, string, string, int64, string, time.Time) (contracts.Environment, error)
	MarkInstanceFailed(context.Context, string, string, int64, string, time.Time) error
	BeginStop(context.Context, string, int64, time.Time) (contracts.Environment, *contracts.Instance, error)
	CompleteStop(context.Context, string, string, int64, string, time.Time) (contracts.Environment, error)
	MarkInstanceLost(context.Context, string, string, int64, time.Time) (contracts.Environment, error)
	TouchEnvironment(context.Context, string, int64, time.Time) error
	CreateLease(context.Context, contracts.Lease, time.Time) (contracts.Lease, error)
	GetLease(context.Context, string) (contracts.Lease, error)
	HasActiveLease(context.Context, string, time.Time) (bool, error)
	RenewLease(context.Context, string, time.Time, time.Time) (contracts.Lease, error)
	ReleaseLease(context.Context, string, time.Time) (contracts.Lease, error)
	SaveSnapshot(context.Context, contracts.Snapshot, time.Time) (contracts.Snapshot, error)
	GetSnapshot(context.Context, string) (contracts.Snapshot, error)
	CommitWorkspaceVersion(context.Context, contracts.WorkspaceVersion) (contracts.WorkspaceVersion, error)
	GetCurrentWorkspaceVersion(context.Context, string) (*contracts.WorkspaceVersion, error)
	GetWorkspaceVersion(context.Context, string, int64) (contracts.WorkspaceVersion, error)
	SaveArtifact(context.Context, contracts.Artifact, time.Time) (contracts.Artifact, error)
	ListLifecycleCandidates(context.Context, time.Time, int) ([]contracts.Environment, error)
	PurgeEnvironment(context.Context, string, int64, time.Time) error
}

// ComputeRequest contains only provider-neutral, bounded compute intent.
type ComputeRequest struct {
	Environment   contracts.Environment   `json:"environment"`
	Workspace     contracts.Workspace     `json:"workspace"`
	ResourceClass contracts.ResourceClass `json:"resourceClass"`
	Instance      contracts.Instance      `json:"instance"`
}

// PreparedCompute is opaque evidence returned by Sandbox Host preparation.
type PreparedCompute struct {
	OperationRef string `json:"operationRef"`
}

// RunningCompute is opaque evidence for compute accepted by Sandbox Host.
type RunningCompute struct {
	BackendRef string `json:"backendRef"`
	State      string `json:"state"`
}

// ComputeIdentity selects one exact replaceable Instance generation.
type ComputeIdentity struct {
	EnvironmentID string `json:"environmentId"`
	InstanceID    string `json:"instanceId"`
	Generation    int64  `json:"generation"`
	BackendRef    string `json:"backendRef"`
}

// ComputeStatus is provider-neutral Sandbox Host lifecycle state.
type ComputeStatus struct {
	State string `json:"state"`
}

// CheckpointResult is immutable opaque snapshot evidence returned by Sandbox Host.
type CheckpointResult struct {
	OpaqueRef   string `json:"opaqueRef"`
	ContentHash string `json:"contentHash"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// ArtifactExchangeInput selects a bounded artifact from one exact generation.
type ArtifactExchangeInput struct {
	Identity  ComputeIdentity   `json:"identity"`
	SourceRef string            `json:"sourceRef"`
	Name      string            `json:"name"`
	MimeType  string            `json:"mimeType"`
	Metadata  map[string]string `json:"metadata"`
}

// ArtifactExchangeResult is opaque immutable artifact evidence returned by Sandbox Host.
type ArtifactExchangeResult struct {
	OpaqueRef string `json:"opaqueRef"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type ExecuteInput struct {
	Identity  ComputeIdentity          `json:"identity"`
	Operation contracts.ExecuteRequest `json:"operation"`
}

type WorkspaceFileInput struct {
	Identity ComputeIdentity
	Path     string
}

type WorkspaceFileWriteResult struct {
	SizeBytes int64
	SHA256    string
}

// ComputeBackend controls replaceable compute without exposing provider implementation types.
type ComputeBackend interface {
	Ready(context.Context) error
	Prepare(context.Context, ComputeRequest) (PreparedCompute, error)
	Start(context.Context, PreparedCompute, ComputeRequest) (RunningCompute, error)
	Inspect(context.Context, ComputeIdentity) (ComputeStatus, error)
	Stop(context.Context, ComputeIdentity) error
	Destroy(context.Context, ComputeIdentity) error
	Purge(context.Context, contracts.Environment, contracts.Workspace) error
	Checkpoint(context.Context, ComputeIdentity) (CheckpointResult, error)
	CheckpointWorkspace(context.Context, contracts.Environment, contracts.Workspace) (CheckpointResult, error)
	MaterializeWorkspace(context.Context, contracts.Environment, contracts.Workspace, contracts.Snapshot) error
	ExchangeArtifact(context.Context, ArtifactExchangeInput) (ArtifactExchangeResult, error)
	Execute(context.Context, ExecuteInput) (contracts.ExecuteResult, error)
	OpenWorkspaceFile(context.Context, WorkspaceFileInput) (io.ReadCloser, int64, error)
	PutWorkspaceFile(context.Context, WorkspaceFileInput, io.Reader) (WorkspaceFileWriteResult, error)
}
