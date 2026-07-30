// Package ports defines SecondBox control-plane persistence boundaries.
package ports

import (
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

var (
	ErrAuthenticationFailed    = errors.New("SecondBox credential authentication failed")
	ErrAuthorizationDenied     = errors.New("SecondBox authorization denied")
	ErrProfileNotFound         = errors.New("SecondBox Profile not found")
	ErrProfileDisabled         = errors.New("SecondBox Profile is disabled")
	ErrRunnerPoolNotFound      = errors.New("SecondBox RunnerPool not found")
	ErrRunnerPoolExists        = errors.New("SecondBox RunnerPool already exists")
	ErrRunnerNotFound          = errors.New("SecondBox Runner not found")
	ErrRunnerPoolUnavailable   = errors.New("SecondBox compatible runner pool unavailable")
	ErrSandboxNotFound         = errors.New("SecondBox Sandbox not found")
	ErrSandboxNameConflict     = errors.New("SecondBox Sandbox name is already in use")
	ErrIdempotencyConflict     = errors.New("SecondBox idempotency key payload conflict")
	ErrQuotaExceeded           = errors.New("SecondBox quota exceeded")
	ErrRevisionConflict        = errors.New("SecondBox resource revision conflict")
	ErrLifecycleUnavailable    = errors.New("SecondBox lifecycle unavailable without a runner assignment")
	ErrHomeRunnerUnavailable   = errors.New("SecondBox Sandbox home runner is unavailable")
	ErrWorkspaceMutation       = errors.New("SecondBox Workspace has a conflicting local mutation")
	ErrWorkspaceHomeConflict   = errors.New("SecondBox Workspace home runner is immutable")
	ErrGenerationFenced        = errors.New("SecondBox Sandbox generation is fenced")
	ErrLeaseNotFound           = errors.New("SecondBox Lease not found")
	ErrLeaseAlreadyActive      = errors.New("SecondBox Sandbox already has an active Lease")
	ErrLeaseInactive           = errors.New("SecondBox Lease is not active")
	ErrActivitySessionNotFound = errors.New("SecondBox activity session not found")
	ErrSnapshotNotFound        = errors.New("SecondBox Snapshot not found")
	ErrSnapshotUnavailable     = errors.New("SecondBox Snapshot requires stopped committed disk state")
	ErrArtifactIntegrity       = errors.New("SecondBox Artifact integrity failed")
	ErrArtifactNotFound        = errors.New("SecondBox Artifact not found")
	ErrArtifactStorage         = errors.New("SecondBox Artifact storage unavailable")
	ErrPortSessionNotFound     = errors.New("SecondBox PortSession not found")
	ErrPortPolicyDenied        = errors.New("SecondBox exposed port is not approved by the pinned Profile")
	ErrPortTokenInvalid        = errors.New("SecondBox port tunnel token is invalid")
	ErrPortTokenConsumed       = errors.New("SecondBox port tunnel token was already consumed")
	ErrPortBackpressure        = errors.New("SecondBox port tunnel has no available byte credit")
	ErrWaitExpired             = errors.New("SecondBox Sandbox wait deadline expired")
)

// AdminIdempotencyInput binds one administrative mutation to an exact durable response.
type AdminIdempotencyInput struct {
	TenantRef   string
	SubjectRef  string
	Operation   string
	TargetID    string
	Key         string
	RequestHash string
	Now         time.Time
	Ends        time.Time
}

// AdminIdempotencyResult reports whether a stored response was replayed.
type AdminIdempotencyResult struct {
	Replayed bool
}

// CreateSandboxInput contains server-resolved identity and transaction evidence.
type CreateSandboxInput struct {
	Principal          contracts.Principal
	SubjectQuota       contracts.QuotaLimits
	Sandbox            contracts.Sandbox
	Workspace          contracts.Workspace
	Operation          contracts.Operation
	IdempotencyKey     string
	RequestHash        string
	IdempotencyEnds    time.Time
	WorkspaceEffectID  string
	WorkspaceCommandID string
	FencingToken       []byte
	SourceSnapshotID   string
}

// UpdateSandboxMetadataInput replaces consumer correlation metadata at one revision.
type UpdateSandboxMetadataInput struct {
	Principal        contracts.Principal
	SandboxID        string
	Metadata         map[string]string
	ExpectedRevision int64
	Now              time.Time
}

// HomeWorkspace is private durable ownership and local-store reconciliation state.
// It must never be projected into a public Sandbox or Workspace representation.
type HomeWorkspace struct {
	ID                   string
	SandboxID            string
	HomeRunnerID         string
	State                string
	LogicalCapacityBytes int64
	Generation           int64
	Mutation             WorkspaceMutation
	LocalReceipt         map[string]any
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// WorkspaceMutation is the single durable local-storage mutation barrier.
type WorkspaceMutation struct {
	Kind               string
	ID                 string
	EffectID           string
	OperationID        string
	ExpectedGeneration int64
	TargetGeneration   int64
	State              string
}

// WorkspaceMutationInput acquires or replays the one Workspace mutation slot.
type WorkspaceMutationInput struct {
	TenantRef          string
	SubjectRef         string
	SandboxID          string
	WorkspaceID        string
	SnapshotID         string
	HomeRunnerID       string
	Kind               string
	MutationID         string
	EffectID           string
	OperationID        string
	ExpectedGeneration int64
	TargetGeneration   int64
	Now                time.Time
}

// WorkspaceMutationCompletion atomically records durable runner evidence and
// releases the matching mutation barrier.
type WorkspaceMutationCompletion struct {
	WorkspaceMutationInput
	WorkspaceState      string
	CommittedGeneration int64
	LocalReceipt        map[string]any
}

// LifecycleIntentInput records desired state and one durable asynchronous operation.
type LifecycleIntentInput struct {
	Principal        contracts.Principal
	SandboxID        string
	DesiredState     string
	Operation        contracts.Operation
	Now              time.Time
	IdempotencyKey   string
	RequestHash      string
	IdempotencyEnds  time.Time
	ExpectedRevision int64
}

// LeaseInput creates or renews bounded activity authority.
type LeaseInput struct {
	Lease           contracts.Lease
	TenantRef       string
	SubjectRef      string
	SandboxID       string
	Generation      int64
	ExpiresAt       time.Time
	Now             time.Time
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
}

// GenerationInput fences a lifecycle report to current Sandbox authority.
type GenerationInput struct {
	TenantRef  string
	SubjectRef string
	SandboxID  string
	Generation int64
	Now        time.Time
}

// ActivityInput records useful work without conflating guest liveness.
type ActivityInput struct {
	GenerationInput
	Session        contracts.ActivitySession
	LeaseID        string
	IdempotencyKey string
	RequestHash    string
}

// SnapshotCreationInput admits one asynchronous runner-local Snapshot clone.
type SnapshotCreationInput struct {
	Snapshot         contracts.Snapshot
	Operation        contracts.Operation
	EffectID         string
	CommandID        string
	FencingToken     []byte
	IdempotencyKey   string
	RequestHash      string
	IdempotencyEnds  time.Time
	ExpectedRevision int64
}

// SnapshotDeletionInput admits one asynchronous runner-local Snapshot deletion.
type SnapshotDeletionInput struct {
	TenantRef       string
	SubjectRef      string
	SnapshotID      string
	Operation       contracts.Operation
	EffectID        string
	CommandID       string
	FencingToken    []byte
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Now             time.Time
}

// SnapshotRetentionInput identifies one internally generated expiration
// deletion attempt. The store selects the due Snapshot while preserving the
// ordinary asynchronous delete transaction and lock order.
type SnapshotRetentionInput struct {
	OperationID  string
	EffectID     string
	CommandID    string
	RequestID    string
	FencingToken []byte
	Now          time.Time
}

// SnapshotRestoreInput admits one stopped-Sandbox in-place restore.
type SnapshotRestoreInput struct {
	TenantRef         string
	SubjectRef        string
	SandboxID         string
	SnapshotID        string
	Operation         contracts.Operation
	RestoreID         string
	PrepareEffectID   string
	SwapEffectID      string
	FinalizeEffectID  string
	AbortEffectID     string
	PrepareCommandID  string
	SwapCommandID     string
	FinalizeCommandID string
	AbortCommandID    string
	FencingToken      []byte
	IdempotencyKey    string
	RequestHash       string
	IdempotencyEnds   time.Time
	ExpectedRevision  int64
	Now               time.Time
}

// ArtifactPublicationInput publishes immutable application exchange evidence.
type ArtifactPublicationInput struct {
	Artifact           contracts.Artifact
	StorageKey         string
	ExpectedGeneration int64
	LeaseID            string
	IdempotencyKey     string
	RequestHash        string
	IdempotencyEnds    time.Time
}

// ArtifactObject binds public metadata to its private immutable provider key.
type ArtifactObject struct {
	Artifact   contracts.Artifact
	StorageKey string
}

// ArtifactRetentionInput ends public reachability idempotently before provider garbage collection.
type ArtifactRetentionInput struct {
	TenantRef       string
	SubjectRef      string
	ArtifactID      string
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Now             time.Time
}

// GarbageObject is private provider evidence for one unreachable immutable object.
type GarbageObject struct {
	Kind       string
	ID         string
	StorageKey string
	SHA256     string
	SizeBytes  int64
}

// LifecycleReconcileClaim is one revision-fenced desired-state work item.
type LifecycleReconcileClaim struct {
	SandboxID                 string
	WorkerID                  string
	Revision                  int64
	ObservedState             string
	DesiredState              string
	IntentKind                string
	IntentTerminationReason   string
	StopEffectState           string
	GuestLiveness             string
	InstanceTerminationReason string
	HasInstance               bool
	ActiveSessions            int64
	DrainStartedAt            *time.Time
	ReadyAt                   *time.Time
	LastActivityAt            *time.Time
	DrainGraceSeconds         int64
	IdleSeconds               int64
	MaximumDurationSeconds    int64
}
