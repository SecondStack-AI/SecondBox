// Package ports defines SecondBox control-plane persistence boundaries.
package ports

import (
	"context"
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

// StoredAPIKey carries the keyed hash only across the private persistence port.
type StoredAPIKey struct {
	APIKey         contracts.APIKey
	ProjectID      string
	CredentialHash []byte
	UpdatedAt      time.Time
}

// AdminIdempotencyInput binds one administrative mutation to an exact durable response.
type AdminIdempotencyInput struct {
	ProjectID      string
	Operation      string
	TargetID       string
	Key            string
	RequestHash    string
	ResponseSecret []byte
	Now            time.Time
	Ends           time.Time
}

// AdminIdempotencyResult reports whether a stored response was replayed.
type AdminIdempotencyResult struct {
	Replayed       bool
	ResponseSecret []byte
}

// CreateSandboxInput contains server-resolved identity and transaction evidence.
type CreateSandboxInput struct {
	Principal       contracts.Principal
	Sandbox         contracts.Sandbox
	Workspace       contracts.Workspace
	Operation       contracts.Operation
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Audit           contracts.AuditEvent
}

// LifecycleIntentInput records desired state and one durable asynchronous operation.
type LifecycleIntentInput struct {
	Principal        contracts.Principal
	SandboxID        string
	DesiredState     string
	Operation        contracts.Operation
	Audit            contracts.AuditEvent
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
	Audit            contracts.AuditEvent
}

// SnapshotRetentionInput ends one immutable metadata root idempotently.
type SnapshotRetentionInput struct {
	ProjectID       string
	SnapshotID      string
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Now             time.Time
	Audit           contracts.AuditEvent
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
	Audit              contracts.AuditEvent
}

// ArtifactObject binds public metadata to its private immutable provider key.
type ArtifactObject struct {
	Artifact   contracts.Artifact
	StorageKey string
}

// ArtifactRetentionInput ends public reachability idempotently before provider garbage collection.
type ArtifactRetentionInput struct {
	ProjectID       string
	ArtifactID      string
	IdempotencyKey  string
	RequestHash     string
	IdempotencyEnds time.Time
	Now             time.Time
	Audit           contracts.AuditEvent
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

// LifecycleStore persists generation-fenced lifecycle and durability evidence.
type LifecycleStore interface {
	GetSandboxLifecyclePolicy(context.Context, string, string) (contracts.LifecyclePolicy, contracts.CheckpointPolicy, error)
	SetSandboxDesiredState(context.Context, LifecycleIntentInput) (contracts.Operation, error)
	AcquireLease(context.Context, LeaseInput) (contracts.Lease, error)
	GetLease(context.Context, string, string, string) (contracts.Lease, error)
	GetLeaseByID(context.Context, string, string) (contracts.Lease, error)
	RenewLease(context.Context, LeaseInput) (contracts.Lease, error)
	ReleaseLease(context.Context, LeaseInput) (contracts.Lease, error)
	PingGuest(context.Context, GenerationInput, string) (contracts.Instance, error)
	ReadSandboxInspection(context.Context, GenerationInput) (contracts.SandboxInspection, error)
	TouchActivity(context.Context, ActivityInput) (time.Time, error)
	OpenActivitySession(context.Context, ActivityInput) (contracts.ActivitySession, error)
	CloseActivitySession(context.Context, ActivityInput) (contracts.ActivitySession, error)
	AcquireMaterialization(context.Context, MaterializationInput) (contracts.WorkspaceMaterialization, error)
	ConfirmMaterialization(context.Context, MaterializationInput, time.Time) (contracts.WorkspaceMaterialization, error)
	ReleaseMaterialization(context.Context, MaterializationInput, map[string]string, time.Time) (contracts.WorkspaceMaterialization, error)
	StageCheckpoint(context.Context, CheckpointPublicationInput) (contracts.WorkspaceCheckpoint, error)
	VerifyCheckpoint(context.Context, CheckpointPublicationInput, time.Time) (contracts.WorkspaceCheckpoint, error)
	PublishCheckpoint(context.Context, CheckpointPublicationInput, time.Time) (contracts.WorkspaceCheckpoint, error)
	CreateSnapshot(context.Context, SnapshotCreationInput) (contracts.Snapshot, error)
	ListSnapshots(context.Context, string, string, int, string, time.Time) (contracts.SnapshotPage, error)
	GetSnapshot(context.Context, string, string, time.Time) (contracts.Snapshot, error)
	EndSnapshotRetention(context.Context, SnapshotRetentionInput) error
	StageArtifact(context.Context, ArtifactPublicationInput) (contracts.Artifact, error)
	PublishArtifact(context.Context, ArtifactPublicationInput, time.Time) (contracts.Artifact, error)
	ListArtifacts(context.Context, string, string, int, string, time.Time) (contracts.ArtifactPage, error)
	GetArtifactObject(context.Context, string, string, time.Time) (ArtifactObject, error)
	EndArtifactRetention(context.Context, ArtifactRetentionInput) error
	ListGarbageObjectsDue(context.Context, time.Time, time.Duration, int) ([]GarbageObject, error)
	CompleteGarbageObject(context.Context, GarbageObject, time.Time) error
	ClaimLifecycle(context.Context, string, time.Time, time.Duration) (LifecycleReconcileClaim, bool, error)
	ApplyLifecycleAction(context.Context, LifecycleReconcileClaim, string, string, time.Time, time.Time) error
}

// ControlPlaneStore persists standalone SecondBox identity, policy, and Sandbox intent.
type ControlPlaneStore interface {
	LifecycleStore
	Ping(context.Context) error
	Close()
	InitializeBootstrapAdmin(context.Context, []byte, time.Time, contracts.AuditEvent) error
	AuthenticateBootstrapAdmin(context.Context, []byte, time.Time, contracts.AuditEvent) (contracts.Principal, error)

	CreateProject(context.Context, contracts.Project, contracts.QuotaLimits, AdminIdempotencyInput, contracts.AuditEvent) (contracts.Project, AdminIdempotencyResult, error)
	UpdateProject(context.Context, string, contracts.UpdateProjectRequest, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.Project, AdminIdempotencyResult, error)
	GetProject(context.Context, string) (contracts.Project, error)
	ListProjects(context.Context, int, string) (contracts.ProjectPage, error)

	CreateServiceAccount(context.Context, contracts.ServiceAccount, AdminIdempotencyInput, contracts.AuditEvent) (contracts.ServiceAccount, AdminIdempotencyResult, error)
	UpdateServiceAccount(context.Context, string, string, contracts.UpdateServiceAccountRequest, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.ServiceAccount, AdminIdempotencyResult, error)
	GetServiceAccount(context.Context, string, string) (contracts.ServiceAccount, error)
	ListServiceAccounts(context.Context, string, int, string) (contracts.ServiceAccountPage, error)

	CreateAPIKey(context.Context, StoredAPIKey, AdminIdempotencyInput, contracts.AuditEvent) (contracts.APIKey, AdminIdempotencyResult, error)
	RotateAPIKey(context.Context, string, string, string, string, []byte, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.APIKey, AdminIdempotencyResult, error)
	RevokeAPIKey(context.Context, string, string, string, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.APIKey, AdminIdempotencyResult, error)
	GetAPIKey(context.Context, string, string, string) (contracts.APIKey, error)
	ListAPIKeys(context.Context, string, string, int, string) (contracts.APIKeyPage, error)
	AuthenticateAPIKey(context.Context, string, []byte, time.Time, contracts.AuditEvent) (contracts.Principal, error)

	CreateProfile(context.Context, contracts.Profile, contracts.QuotaLimits, AdminIdempotencyInput, contracts.AuditEvent) (contracts.Profile, AdminIdempotencyResult, error)
	ReviseProfile(context.Context, string, contracts.ProfileRevision, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.Profile, AdminIdempotencyResult, error)
	DisableProfile(context.Context, string, int64, time.Time, AdminIdempotencyInput, contracts.AuditEvent) (contracts.Profile, AdminIdempotencyResult, error)
	GetProfile(context.Context, string) (contracts.Profile, error)
	ListProfiles(context.Context, int, string) (contracts.ProfilePage, error)
	CreateRunnerPool(context.Context, contracts.RunnerPool, contracts.AuditEvent) (contracts.RunnerPool, error)
	UpdateRunnerPool(context.Context, string, contracts.UpdateRunnerPoolRequest, int64, time.Time, contracts.AuditEvent) (contracts.RunnerPool, error)
	GetRunnerPool(context.Context, string) (contracts.RunnerPool, error)
	ListRunnerPools(context.Context, int, string) (contracts.RunnerPoolPage, error)
	GetRunner(context.Context, string) (contracts.Runner, error)
	ListRunners(context.Context, string, int, string) (contracts.RunnerPage, error)
	RegisterRunnerPool(context.Context, contracts.RunnerPool) error

	CreateSandbox(context.Context, CreateSandboxInput) (contracts.Sandbox, contracts.Operation, bool, error)
	GetSandbox(context.Context, string, string) (contracts.Sandbox, error)
	ListSandboxes(context.Context, string, int, string) (contracts.SandboxPage, error)
	GetOperation(context.Context, string, string) (contracts.Operation, error)

	ListAuditEvents(context.Context, string, int) ([]contracts.AuditEvent, error)
	ReadMetricsSnapshot(context.Context) (contracts.MetricsSnapshot, error)
}
