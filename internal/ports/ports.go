// Package ports defines SecondBox control-plane persistence boundaries.
package ports

import (
	"errors"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const (
	TenantControllerBearerTokenPrefix = "secondbox_tenant_controller_"
	ApplicationBearerTokenPrefix      = "secondbox_application_"
)

var (
	ErrAuthenticationFailed        = errors.New("SecondBox credential authentication failed")
	ErrAuthorizationDenied         = errors.New("SecondBox authorization denied")
	ErrManagementUnavailable       = errors.New("SecondBox durable management store is unavailable")
	ErrManagementNotFound          = errors.New("SecondBox management resource not found")
	ErrManagementConflict          = errors.New("SecondBox management resource conflicts with current state")
	ErrInvalidLifecycleTransition  = errors.New("SecondBox management lifecycle transition is invalid")
	ErrResourceExpired             = errors.New("SecondBox management resource is expired")
	ErrTenantSuspended             = errors.New("SecondBox Tenant is suspended")
	ErrTenantEgressContextRequired = errors.New("SecondBox Profile requires a Tenant egress context")
	ErrGrantEscalationDenied       = errors.New("SecondBox management grant exceeds its Tenant ceiling")
	ErrInvalidRequest              = errors.New("SecondBox request is invalid")
	ErrProfileNotFound             = errors.New("SecondBox Profile not found")
	ErrProfileDisabled             = errors.New("SecondBox Profile is disabled")
	ErrRunnerPoolNotFound          = errors.New("SecondBox RunnerPool not found")
	ErrRunnerPoolExists            = errors.New("SecondBox RunnerPool already exists")
	ErrRunnerNotFound              = errors.New("SecondBox Runner not found")
	ErrRunnerPoolUnavailable       = errors.New("SecondBox compatible runner pool unavailable")
	// ErrStartupModeUnsupported reports that the Profile's RunnerPool does not
	// declare the capability its startup mode requires. It is not retryable:
	// no Runner in that pool is admissible until an operator either declares the
	// capability on the pool or revises the Profile's startup mode.
	ErrStartupModeUnsupported        = errors.New("SecondBox RunnerPool does not support the Profile startup mode")
	ErrSandboxNotFound               = errors.New("SecondBox Sandbox not found")
	ErrSandboxNameConflict           = errors.New("SecondBox Sandbox name is already in use")
	ErrIdempotencyConflict           = errors.New("SecondBox idempotency key payload conflict")
	ErrCredentialResponseUnavailable = errors.New("SecondBox credential response is unavailable")
	ErrQuotaExceeded                 = errors.New("SecondBox quota exceeded")
	ErrRevisionConflict              = errors.New("SecondBox resource revision conflict")
	ErrLifecycleUnavailable          = errors.New("SecondBox lifecycle unavailable without a runner assignment")
	ErrHomeRunnerUnavailable         = errors.New("SecondBox Sandbox home runner is unavailable")
	ErrWorkspaceMutation             = errors.New("SecondBox Workspace has a conflicting local mutation")
	ErrSandboxNotStopped             = errors.New("SecondBox Workspace relocation requires a stopped Sandbox")
	ErrRelocationTargetUnavailable   = errors.New("SecondBox Workspace relocation target is unavailable or incompatible")
	ErrRelocationSnapshotsPresent    = errors.New("SecondBox Workspace relocation requires all Snapshots to be deleted")
	// ErrSerializationContention reports that a transaction lost a serialization
	// race and the caller should try again later. It is an ordinary outcome of
	// serializable isolation under concurrency, not a fault: a caller that treats
	// it as one will fail whenever load rises.
	ErrSerializationContention = errors.New("SecondBox transaction lost a serialization race")
	ErrWorkspaceHomeConflict   = errors.New("SecondBox Workspace home runner differs from assigned authority")
	ErrGenerationFenced        = errors.New("SecondBox Sandbox generation is fenced")
	ErrLeaseNotFound           = errors.New("SecondBox Lease not found")
	ErrLeaseAlreadyActive      = errors.New("SecondBox Sandbox already has an active Lease")
	ErrLeaseInactive           = errors.New("SecondBox Lease is not active")
	ErrActivitySessionNotFound = errors.New("SecondBox activity session not found")
	ErrSnapshotNotFound        = errors.New("SecondBox Snapshot not found")
	ErrSnapshotUnavailable     = errors.New("SecondBox Snapshot requires stopped committed disk state")
	ErrPortSessionNotFound     = errors.New("SecondBox PortSession not found")
	ErrPortPolicyDenied        = errors.New("SecondBox exposed port is not approved by the pinned Profile")
	ErrPortTokenInvalid        = errors.New("SecondBox port tunnel token is invalid")
	ErrPortTokenConsumed       = errors.New("SecondBox port tunnel token was already consumed")
	ErrPortBackpressure        = errors.New("SecondBox port tunnel has no available byte credit")
	ErrWaitExpired             = errors.New("SecondBox Sandbox wait deadline expired")
)

// AuthenticatedApplicationAuthority is the verified non-secret application authority used by HTTP admission.
type AuthenticatedApplicationAuthority struct {
	ID            string
	TenantRef     string
	SubjectRef    string
	Scopes        []string
	ProfileGrants []string
}

// AdminIdempotencyInput binds one administrative mutation to an exact durable outcome.
type AdminIdempotencyInput struct {
	TenantRef   string
	SubjectRef  string
	Operation   string
	TargetID    string
	Key         string
	RequestHash string
	Now         time.Time
	Ends        time.Time
	AuditEvent  *contracts.AuditEvent
}

// AdminIdempotencyResult reports whether a matching durable outcome was found.
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

// WorkspaceRelocationInput admits one stopped-Sandbox home transfer.
type WorkspaceRelocationInput struct {
	Principal        contracts.Principal
	SandboxID        string
	TargetRunnerID   string
	RunnerPool       string
	Operation        contracts.Operation
	RelocationID     string
	ExportCommandID  string
	FencingToken     []byte
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
	ReplaceActive   bool
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

// LifecycleWakeTrigger names why the lifecycle worker was running when it
// claimed a cohort. It is attribution evidence only: PostgreSQL remains the
// sole work authority, and every trigger reaches the same fenced claim query.
type LifecycleWakeTrigger string

const (
	// LifecycleWakeTriggerNotify means a PostgreSQL commit notification made
	// the worker run before its poll deadline.
	LifecycleWakeTriggerNotify LifecycleWakeTrigger = "notify"
	// LifecycleWakeTriggerDeadline means no notification arrived and the
	// bounded recovery poll interval elapsed. A transition that leaves work
	// immediately available should never be followed by this trigger.
	LifecycleWakeTriggerDeadline LifecycleWakeTrigger = "deadline"
	// LifecycleWakeTriggerImmediate means the worker never waited: it had just
	// completed a claim and re-ran its claim query directly.
	LifecycleWakeTriggerImmediate LifecycleWakeTrigger = "immediate"
)

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
