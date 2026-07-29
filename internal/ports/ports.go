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
	ErrProjectNotFound         = errors.New("SecondBox Project not found")
	ErrServiceAccountNotFound  = errors.New("SecondBox ServiceAccount not found")
	ErrAPIKeyNotFound          = errors.New("SecondBox APIKey not found")
	ErrProfileNotFound         = errors.New("SecondBox Profile not found")
	ErrProfileDisabled         = errors.New("SecondBox Profile is disabled")
	ErrProfileNotGranted       = errors.New("SecondBox Profile is not granted")
	ErrRunnerPoolNotFound      = errors.New("SecondBox RunnerPool not found")
	ErrRunnerPoolExists        = errors.New("SecondBox RunnerPool already exists")
	ErrRunnerNotFound          = errors.New("SecondBox Runner not found")
	ErrRunnerPoolUnavailable   = errors.New("SecondBox compatible runner pool unavailable")
	ErrSandboxNotFound         = errors.New("SecondBox Sandbox not found")
	ErrIdempotencyConflict     = errors.New("SecondBox idempotency key payload conflict")
	ErrQuotaExceeded           = errors.New("SecondBox quota exceeded")
	ErrRevisionConflict        = errors.New("SecondBox resource revision conflict")
	ErrLifecycleUnavailable    = errors.New("SecondBox lifecycle unavailable without a runner assignment")
	ErrGenerationFenced        = errors.New("SecondBox Sandbox generation is fenced")
	ErrLeaseNotFound           = errors.New("SecondBox Lease not found")
	ErrLeaseInactive           = errors.New("SecondBox Lease is not active")
	ErrActivitySessionNotFound = errors.New("SecondBox activity session not found")
	ErrMaterializationConflict = errors.New("SecondBox Workspace already has an active materialization")
	ErrCheckpointIntegrity     = errors.New("SecondBox Workspace checkpoint integrity failed")
	ErrCheckpointNotFound      = errors.New("SecondBox Workspace checkpoint not found")
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
	ProjectID   string
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
	Principal       contracts.Principal
	SubjectQuota    contracts.QuotaLimits
	Sandbox         contracts.Sandbox
	Workspace       contracts.Workspace
	Operation       contracts.Operation
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
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
	Lease            contracts.Lease
	ProjectID        string
	TenantRef        string
	SubjectRef       string
	SandboxID        string
	Generation       int64
	ServiceAccountID string
	ExpiresAt        time.Time
	Now              time.Time
	IdempotencyKey   string
	RequestHash      string
	IdempotencyEnds  time.Time
}

// GenerationInput fences a lifecycle report to current Sandbox authority.
type GenerationInput struct {
	ProjectID  string
	TenantRef  string
	SubjectRef string
	SandboxID  string
	Generation int64
	Now        time.Time
}

// ActivityInput records useful work without conflating guest liveness.
type ActivityInput struct {
	GenerationInput
	Session          contracts.ActivitySession
	LeaseID          string
	ServiceAccountID string
	IdempotencyKey   string
	RequestHash      string
}

// MaterializationInput creates exclusive runner-local writer authority.
type MaterializationInput struct {
	Materialization             contracts.WorkspaceMaterialization
	ExpectedWorkspaceGeneration int64
}

// CheckpointPublicationInput atomically publishes verified immutable bytes.
type CheckpointPublicationInput struct {
	Checkpoint                  contracts.WorkspaceCheckpoint
	StorageKey                  string
	ExpectedWorkspaceGeneration int64
}

// SnapshotCreationInput retains the current published checkpoint under immutable metadata.
type SnapshotCreationInput struct {
	Snapshot         contracts.Snapshot
	IdempotencyKey   string
	RequestHash      string
	IdempotencyEnds  time.Time
	ExpectedRevision int64
}

// SnapshotRetentionInput ends one immutable metadata root idempotently.
type SnapshotRetentionInput struct {
	ProjectID       string
	TenantRef       string
	SubjectRef      string
	SnapshotID      string
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Now             time.Time
}

// ArtifactPublicationInput publishes immutable application exchange evidence.
type ArtifactPublicationInput struct {
	Artifact           contracts.Artifact
	StorageKey         string
	ExpectedGeneration int64
	ServiceAccountID   string
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
	ProjectID       string
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
	MaterializationState      string
	CheckpointState           string
	StopEffectState           string
	GuestLiveness             string
	InstanceTerminationReason string
	HasInstance               bool
	ActiveSessions            int64
	CheckpointOnStop          bool
	ForceCheckpoint           bool
	DrainStartedAt            *time.Time
	ReadyAt                   *time.Time
	LastActivityAt            *time.Time
	DrainGraceSeconds         int64
	IdleSeconds               int64
	MaximumDurationSeconds    int64
}
