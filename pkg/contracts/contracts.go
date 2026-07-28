// Package contracts defines the provider-neutral Sandbox Service contract.
package contracts

import "time"

const (
	// ContractVersionV1 is the only accepted Sandbox Service contract version.
	ContractVersionV1 = "sandbox.secondstack.ai/v1"

	EnvironmentStateStopped   = "stopped"
	EnvironmentStatePreparing = "preparing"
	EnvironmentStateReady     = "ready"
	EnvironmentStateStopping  = "stopping"
	EnvironmentStateLost      = "lost"
	EnvironmentStateFailed    = "failed"
	EnvironmentStatePurging   = "purging"

	DesiredStateStopped = "stopped"
	DesiredStateRunning = "running"

	InstanceStatePreparing = "preparing"
	InstanceStateReady     = "ready"
	InstanceStateStopping  = "stopping"
	InstanceStateStopped   = "stopped"
	InstanceStateLost      = "lost"
	InstanceStateFailed    = "failed"
	InstanceStateDestroyed = "destroyed"

	LeaseStateActive   = "active"
	LeaseStateReleased = "released"
	LeaseStateExpired  = "expired"

	LifecyclePolicyAgentCompartment  = "agent-compartment"
	LifecyclePolicyCodingEnvironment = "coding-environment"
	ResourceClassAgentStandard       = "agent-standard"
	ResourceClassCodingStandard      = "coding-standard"
)

// Environment is durable intent for one retained workspace and replaceable compute lineage.
type Environment struct {
	ContractVersion   string            `json:"contractVersion"`
	ID                string            `json:"id"`
	TenantRef         string            `json:"tenantRef"`
	SubjectRef        string            `json:"subjectRef"`
	EnvironmentKey    string            `json:"environmentKey"`
	WorkspaceID       string            `json:"workspaceId"`
	ImageRef          string            `json:"imageRef"`
	ToolchainRef      string            `json:"toolchainRef"`
	ResourceClassID   string            `json:"resourceClassId"`
	LifecyclePolicyID string            `json:"lifecyclePolicyId"`
	DesiredState      string            `json:"desiredState"`
	State             string            `json:"state"`
	CurrentGeneration int64             `json:"currentGeneration"`
	CurrentInstanceID string            `json:"currentInstanceId,omitempty"`
	SnapshotID        string            `json:"snapshotId,omitempty"`
	ExposedPorts      []ExposedPort     `json:"exposedPorts"`
	Metadata          map[string]string `json:"metadata"`
	LastActivityAt    time.Time         `json:"lastActivityAt"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
}

// Instance is replaceable compute attached to exactly one Environment generation.
type Instance struct {
	ContractVersion string    `json:"contractVersion"`
	ID              string    `json:"id"`
	EnvironmentID   string    `json:"environmentId"`
	Generation      int64     `json:"generation"`
	State           string    `json:"state"`
	BackendRef      string    `json:"backendRef,omitempty"`
	FailureCode     string    `json:"failureCode,omitempty"`
	PreparedAt      time.Time `json:"preparedAt"`
	ReadyAt         time.Time `json:"readyAt,omitempty"`
	StoppedAt       time.Time `json:"stoppedAt,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Lease fences one caller's access to an exact Environment generation.
type Lease struct {
	ContractVersion string    `json:"contractVersion"`
	ID              string    `json:"id"`
	EnvironmentID   string    `json:"environmentId"`
	Generation      int64     `json:"generation"`
	HolderRef       string    `json:"holderRef"`
	State           string    `json:"state"`
	ExpiresAt       time.Time `json:"expiresAt"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Workspace is durable storage identity retained independently from compute Instances.
type Workspace struct {
	ContractVersion string    `json:"contractVersion"`
	ID              string    `json:"id"`
	TenantRef       string    `json:"tenantRef"`
	SubjectRef      string    `json:"subjectRef"`
	StorageRef      string    `json:"storageRef"`
	Generation      int64     `json:"generation"`
	RetainUntil     time.Time `json:"retainUntil"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// WorkspaceUsage is aggregate retained storage evidence for one logical subject.
type WorkspaceUsage struct {
	ContractVersion  string `json:"contractVersion"`
	TenantRef        string `json:"tenantRef"`
	SubjectRef       string `json:"subjectRef"`
	EnvironmentCount int64  `json:"environmentCount"`
	QuotaBytes       int64  `json:"quotaBytes"`
	UsageBytes       int64  `json:"usageBytes"`
}

// Snapshot is immutable checkpoint evidence for one Environment generation.
type Snapshot struct {
	ContractVersion  string            `json:"contractVersion"`
	ID               string            `json:"id"`
	EnvironmentID    string            `json:"environmentId"`
	WorkspaceID      string            `json:"workspaceId"`
	Generation       int64             `json:"generation"`
	ParentSnapshotID string            `json:"parentSnapshotId,omitempty"`
	OpaqueRef        string            `json:"opaqueRef"`
	ContentHash      string            `json:"contentHash"`
	SizeBytes        int64             `json:"sizeBytes"`
	Metadata         map[string]string `json:"metadata"`
	CreatedAt        time.Time         `json:"createdAt"`
}

const (
	WorkspaceTerminalCompleted = "completed"
	WorkspaceTerminalFailed    = "failed"
	WorkspaceTerminalCancelled = "cancelled"
)

// WorkspaceVersion is immutable terminal-turn evidence owned by Sandbox Service.
type WorkspaceVersion struct {
	ContractVersion        string    `json:"contractVersion"`
	EnvironmentID          string    `json:"environmentId"`
	LogicalVersion         int64     `json:"logicalVersion"`
	SourceGeneration       int64     `json:"sourceGeneration"`
	TerminalTurnID         string    `json:"terminalTurnId"`
	TerminalStatus         string    `json:"terminalStatus"`
	WorkspacePresent       bool      `json:"workspacePresent"`
	Dirty                  bool      `json:"dirty"`
	ContentHash            string    `json:"contentHash"`
	SnapshotID             string    `json:"snapshotId,omitempty"`
	SnapshotLogicalVersion int64     `json:"snapshotLogicalVersion,omitempty"`
	CreatedAt              time.Time `json:"createdAt"`
}

// CommitWorkspaceVersionRequest records exactly one terminal turn.
type CommitWorkspaceVersionRequest struct {
	ContractVersion    string `json:"contractVersion"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
	TerminalTurnID     string `json:"terminalTurnId"`
	TerminalStatus     string `json:"terminalStatus"`
}

// MaterializeWorkspaceVersionRequest copies one immutable source version into an empty target Environment.
type MaterializeWorkspaceVersionRequest struct {
	ContractVersion          string `json:"contractVersion"`
	SourceEnvironmentID      string `json:"sourceEnvironmentId"`
	SourceLogicalVersion     int64  `json:"sourceLogicalVersion"`
	ExpectedTargetGeneration int64  `json:"expectedTargetGeneration"`
}

// PurgeEnvironmentRequest permanently removes Environment intent and retained workspace.
type PurgeEnvironmentRequest struct {
	ContractVersion    string `json:"contractVersion"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
}

// Artifact is immutable opaque exchange evidence produced by one Environment generation.
type Artifact struct {
	ContractVersion string            `json:"contractVersion"`
	ID              string            `json:"id"`
	EnvironmentID   string            `json:"environmentId"`
	Generation      int64             `json:"generation"`
	Name            string            `json:"name"`
	MimeType        string            `json:"mimeType"`
	SizeBytes       int64             `json:"sizeBytes"`
	SHA256          string            `json:"sha256"`
	OpaqueRef       string            `json:"opaqueRef"`
	Metadata        map[string]string `json:"metadata"`
	CreatedAt       time.Time         `json:"createdAt"`
}

// ResourceClass describes enforceable quotas without naming a compute provider.
type ResourceClass struct {
	ContractVersion string    `json:"contractVersion"`
	ID              string    `json:"id"`
	CPUMillis       int64     `json:"cpuMillis"`
	MemoryBytes     int64     `json:"memoryBytes"`
	DiskBytes       int64     `json:"diskBytes"`
	ProcessLimit    int64     `json:"processLimit"`
	MaxExposedPorts int64     `json:"maxExposedPorts"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// LifecyclePolicy controls independent compute idling and durable Environment retention.
type LifecyclePolicy struct {
	ContractVersion        string    `json:"contractVersion"`
	ID                     string    `json:"id"`
	IdleStopAfterSeconds   int64     `json:"idleStopAfterSeconds"`
	RetentionSeconds       int64     `json:"retentionSeconds"`
	StopComputeWhenIdle    bool      `json:"stopComputeWhenIdle"`
	RetainOnExplicitStop   bool      `json:"retainOnExplicitStop"`
	KeepRunningWithoutWake bool      `json:"keepRunningWithoutWake"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

// ExposedPort is reserved Environment-scoped metadata, not an interactive sharing API.
type ExposedPort struct {
	Name       string `json:"name"`
	Port       int64  `json:"port"`
	Protocol   string `json:"protocol"`
	Visibility string `json:"visibility"`
}

// ResolveEnvironmentRequest creates or returns durable Environment intent.
type ResolveEnvironmentRequest struct {
	ContractVersion   string            `json:"contractVersion"`
	TenantRef         string            `json:"tenantRef"`
	SubjectRef        string            `json:"subjectRef"`
	EnvironmentKey    string            `json:"environmentKey"`
	ImageRef          string            `json:"imageRef"`
	ToolchainRef      string            `json:"toolchainRef"`
	ResourceClassID   string            `json:"resourceClassId"`
	LifecyclePolicyID string            `json:"lifecyclePolicyId"`
	Metadata          map[string]string `json:"metadata"`
}

// ResolveEnvironmentResponse reports whether durable Environment intent was created.
type ResolveEnvironmentResponse struct {
	Environment Environment `json:"environment"`
	Created     bool        `json:"created"`
}

// EnvironmentGenerationRequest fences a lifecycle operation to current intent when supplied.
type EnvironmentGenerationRequest struct {
	ContractVersion    string `json:"contractVersion"`
	ExpectedGeneration int64  `json:"expectedGeneration,omitempty"`
}

// AcquireLeaseRequest acquires bounded access to an exact Environment generation.
type AcquireLeaseRequest struct {
	ContractVersion string `json:"contractVersion"`
	HolderRef       string `json:"holderRef"`
	TTLSeconds      int64  `json:"ttlSeconds,omitempty"`
}

// RenewLeaseRequest extends an active, unfenced lease.
type RenewLeaseRequest struct {
	ContractVersion string `json:"contractVersion"`
	TTLSeconds      int64  `json:"ttlSeconds,omitempty"`
}

// CheckpointRequest checkpoints the exact current generation.
type CheckpointRequest struct {
	ContractVersion    string            `json:"contractVersion"`
	ExpectedGeneration int64             `json:"expectedGeneration"`
	Metadata           map[string]string `json:"metadata"`
}

// ExchangeArtifactRequest exchanges one opaque artifact from the exact current generation.
type ExchangeArtifactRequest struct {
	ContractVersion    string            `json:"contractVersion"`
	ExpectedGeneration int64             `json:"expectedGeneration"`
	SourceRef          string            `json:"sourceRef"`
	Name               string            `json:"name"`
	MimeType           string            `json:"mimeType"`
	Metadata           map[string]string `json:"metadata"`
}

// ExecuteRequest runs one bounded workspace operation under an active fenced lease.
type ExecuteRequest struct {
	ContractVersion      string            `json:"contractVersion"`
	ExpectedGeneration   int64             `json:"expectedGeneration"`
	LeaseID              string            `json:"leaseId"`
	Operation            string            `json:"operation"`
	Command              string            `json:"command,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Cwd                  string            `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
	TimeoutMillis        int64             `json:"timeoutMillis,omitempty"`
	Path                 string            `json:"path,omitempty"`
	Content              string            `json:"content,omitempty"`
	ContentBase64        string            `json:"contentBase64,omitempty"`
	Encoding             string            `json:"encoding,omitempty"`
	Recursive            bool              `json:"recursive,omitempty"`
	Force                bool              `json:"force,omitempty"`
	AllowedConnectionIDs []string          `json:"allowedConnectionIds"`
}

// ExecuteResult is the bounded provider-neutral workspace operation result.
type ExecuteResult struct {
	InstanceID    string           `json:"instanceId"`
	Stdout        string           `json:"stdout,omitempty"`
	Stderr        string           `json:"stderr,omitempty"`
	ExitCode      int              `json:"exitCode,omitempty"`
	TimedOut      bool             `json:"timedOut,omitempty"`
	Content       string           `json:"content,omitempty"`
	ContentBase64 string           `json:"contentBase64,omitempty"`
	Stat          map[string]any   `json:"stat,omitempty"`
	Entries       []map[string]any `json:"entries,omitempty"`
	Exists        *bool            `json:"exists,omitempty"`
	Error         string           `json:"error,omitempty"`
}

// LifecycleResponse returns the durable Environment and its current Instance.
type LifecycleResponse struct {
	Environment Environment `json:"environment"`
	Instance    *Instance   `json:"instance,omitempty"`
}

// ErrorResponse is the stable failure envelope for every Sandbox Service operation.
type ErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
