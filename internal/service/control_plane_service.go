// Package service validates and coordinates standalone SecondBox authority.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/worknotify"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const idempotencyRetention = 24 * time.Hour

var (
	profileNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)
	digestPattern         = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~:+/=-]+$`)
)

// HealthStore exposes only readiness authority.
type HealthStore interface {
	Ping(ctx context.Context) error
}

// ProfileStore owns immutable Profile revisions.
type ProfileStore interface {
	CreateProfile(ctx context.Context, profile contracts.Profile, idempotency ports.AdminIdempotencyInput) (contracts.Profile, ports.AdminIdempotencyResult, error)
	EnsureBuiltInProfile(ctx context.Context, profile contracts.Profile) (contracts.Profile, error)
	ReviseProfile(ctx context.Context, name string, revision contracts.ProfileRevision, expectedRevision int64, now time.Time, idempotency ports.AdminIdempotencyInput) (contracts.Profile, ports.AdminIdempotencyResult, error)
	DisableProfile(ctx context.Context, name string, expectedRevision int64, now time.Time, idempotency ports.AdminIdempotencyInput) (contracts.Profile, ports.AdminIdempotencyResult, error)
	GetProfile(ctx context.Context, name string) (contracts.Profile, error)
	ListProfiles(ctx context.Context, limit int, cursor string) (contracts.ProfilePage, error)
}

// RunnerAdminStore owns runner-pool and runner administration.
type RunnerAdminStore interface {
	CreateRunnerPool(ctx context.Context, pool contracts.RunnerPool) (contracts.RunnerPool, error)
	UpdateRunnerPool(ctx context.Context, name string, request contracts.UpdateRunnerPoolRequest, expectedRevision int64, now time.Time) (contracts.RunnerPool, error)
	GetRunnerPool(ctx context.Context, name string) (contracts.RunnerPool, error)
	ListRunnerPools(ctx context.Context, limit int, cursor string) (contracts.RunnerPoolPage, error)
	GetRunner(ctx context.Context, runnerID string) (contracts.Runner, error)
	ListRunners(ctx context.Context, poolName string, limit int, cursor string) (contracts.RunnerPage, error)
}

// SandboxStore owns public Sandbox and Operation records.
type SandboxStore interface {
	CreateSandbox(ctx context.Context, input ports.CreateSandboxInput) (contracts.Sandbox, contracts.Operation, bool, error)
	UpdateSandboxMetadata(ctx context.Context, input ports.UpdateSandboxMetadataInput) (contracts.Sandbox, error)
	GetSandbox(ctx context.Context, tenantRef, subjectRef, sandboxID string) (contracts.Sandbox, error)
	ListSandboxes(ctx context.Context, tenantRef, subjectRef string, limit int, cursor string, metadata map[string]string) (contracts.SandboxPage, error)
	GetOperation(ctx context.Context, tenantRef, subjectRef, operationID string) (contracts.Operation, error)
	GetSubjectUsage(ctx context.Context, tenantRef, subjectRef string) (contracts.SubjectUsage, error)
}

// ActivityStore owns lifecycle intent, leases, and useful-activity evidence.
type ActivityStore interface {
	GetSandboxLifecyclePolicy(ctx context.Context, tenantRef, subjectRef, sandboxID string) (contracts.LifecyclePolicy, contracts.RetentionPolicy, error)
	SetSandboxDesiredState(ctx context.Context, input ports.LifecycleIntentInput) (contracts.Operation, error)
	AcquireLease(ctx context.Context, input ports.LeaseInput) (contracts.Lease, error)
	GetLeaseByID(ctx context.Context, tenantRef, subjectRef, leaseID string) (contracts.Lease, error)
	RenewLease(ctx context.Context, input ports.LeaseInput) (contracts.Lease, error)
	ReleaseLease(ctx context.Context, input ports.LeaseInput) (contracts.Lease, error)
	PingGuest(ctx context.Context, input ports.GenerationInput, serviceAccountID string) (contracts.Instance, error)
	ReadSandboxInspection(ctx context.Context, input ports.GenerationInput) (contracts.SandboxInspection, error)
	TouchActivity(ctx context.Context, input ports.ActivityInput) (time.Time, error)
	OpenActivitySession(ctx context.Context, input ports.ActivityInput) (contracts.ActivitySession, error)
	CloseActivitySession(ctx context.Context, input ports.ActivityInput) (contracts.ActivitySession, error)
}

// SnapshotStore owns durable user snapshots.
type SnapshotStore interface {
	CreateSnapshot(ctx context.Context, input ports.SnapshotCreationInput) (contracts.Operation, error)
	DeleteSnapshot(ctx context.Context, input ports.SnapshotDeletionInput) (contracts.Operation, error)
	RestoreSnapshot(ctx context.Context, input ports.SnapshotRestoreInput) (contracts.Operation, error)
	ListSnapshots(ctx context.Context, tenantRef, subjectRef, sandboxID string, limit int, cursor string, now time.Time) (contracts.SnapshotPage, error)
	GetSnapshot(ctx context.Context, tenantRef, subjectRef, snapshotID string, now time.Time) (contracts.Snapshot, error)
}

// ArtifactStore owns immutable artifact publication and retention.
type ArtifactStore interface {
	StageArtifact(ctx context.Context, input ports.ArtifactPublicationInput) (contracts.Artifact, error)
	PublishArtifact(ctx context.Context, input ports.ArtifactPublicationInput, now time.Time) (contracts.Artifact, error)
	ListArtifacts(ctx context.Context, tenantRef, subjectRef, sandboxID string, limit int, cursor string, now time.Time) (contracts.ArtifactPage, error)
	GetArtifactObject(ctx context.Context, tenantRef, subjectRef, artifactID string, now time.Time) (ports.ArtifactObject, error)
	EndArtifactRetention(ctx context.Context, input ports.ArtifactRetentionInput) error
}

// ObservabilityStore owns audit and metrics reads/writes.
type ObservabilityStore interface {
	AppendAuditEvent(ctx context.Context, event contracts.AuditEvent) error
	ReadMetricsSnapshot(ctx context.Context) (contracts.MetricsSnapshot, error)
	ReadSandboxTiming(ctx context.Context, tenantRef, subjectRef, sandboxID string, limit int) (contracts.SandboxTiming, error)
	ReadOperationTiming(ctx context.Context, tenantRef, subjectRef, operationID string) (contracts.OperationTiming, error)
	ReadDeploymentTiming(ctx context.Context, since, until time.Time) (contracts.DeploymentTimingSummary, error)
}

// ControlPlaneStore is the service's composite consumer-side store contract.
type ControlPlaneStore interface {
	HealthStore
	ProfileStore
	RunnerAdminStore
	SandboxStore
	ActivityStore
	SnapshotStore
	ArtifactStore
	ObservabilityStore
}

// ControlPlaneConfig contains explicit authority, quota, time, and identity dependencies.
type ControlPlaneConfig struct {
	Store                 ControlPlaneStore
	PlatformToken         string
	DefaultSubjectQuota   contracts.QuotaLimits
	Now                   func() time.Time
	NewID                 func(string) string
	NewCredentialMaterial func() string
	ArtifactObjectStore   objectstore.Store
	DataPlaneRelay        DataPlaneRelay
	DataPlanePollInterval time.Duration
	// DataPlaneWakeups is optional. Without it the caller-facing loops fall back
	// to DataPlanePollInterval, which remains their recovery bound either way.
	DataPlaneWakeups worknotify.Source
	PortSessionRelay runnercontrol.PortSessionRelay
	PublicBaseURL    string
	BuiltInProfiles  []contracts.Profile
}

// ControlPlaneService owns validation, authentication, and transaction inputs.
type ControlPlaneService struct {
	store                 ControlPlaneStore
	credentialSealSecret  []byte
	defaultSubjectQuota   contracts.QuotaLimits
	now                   func() time.Time
	newID                 func(string) string
	newCredentialMaterial func() string
	artifactObjectStore   objectstore.Store
	dataPlaneRelay        DataPlaneRelay
	dataPlanePollInterval time.Duration
	dataPlaneWakeups      worknotify.Source
	portSessionRelay      runnercontrol.PortSessionRelay
	publicBaseURL         string
	builtInProfiles       map[string]contracts.Profile
}

// SystemClock returns the current time for production control-plane wiring.
func SystemClock() time.Time {
	return time.Now()
}

// NewControlPlaneService constructs the standalone SecondBox coordinator.
func NewControlPlaneService(config ControlPlaneConfig) (*ControlPlaneService, error) {
	if config.Store == nil {
		return nil, errors.New("SecondBox ControlPlaneStore is required")
	}
	if len(config.PlatformToken) < 24 {
		return nil, errors.New("SecondBox platform token must contain at least 24 bytes")
	}
	if err := validateQuotaLimits("default subject", config.DefaultSubjectQuota); err != nil {
		return nil, err
	}
	if config.Now == nil || config.NewID == nil || config.NewCredentialMaterial == nil {
		return nil, errors.New("SecondBox clock, identifier, and credential generators are required")
	}
	builtInProfiles, err := resolveBuiltInProfiles(config.BuiltInProfiles)
	if err != nil {
		return nil, err
	}
	controlPlane := &ControlPlaneService{
		store:                config.Store,
		credentialSealSecret: []byte(config.PlatformToken),
		defaultSubjectQuota:  config.DefaultSubjectQuota,
		now:                  config.Now, newID: config.NewID, newCredentialMaterial: config.NewCredentialMaterial,
		artifactObjectStore: config.ArtifactObjectStore,
		dataPlaneRelay:      config.DataPlaneRelay, dataPlanePollInterval: config.DataPlanePollInterval,
		dataPlaneWakeups: config.DataPlaneWakeups,
		portSessionRelay: config.PortSessionRelay, publicBaseURL: config.PublicBaseURL,
		builtInProfiles: builtInProfiles,
	}
	if config.DataPlaneRelay != nil && config.DataPlanePollInterval <= 0 {
		return nil, errors.New("SecondBox data-plane poll interval is required with the relay")
	}
	if config.PortSessionRelay != nil {
		if _, err := validatedPublicBaseURL(config.PublicBaseURL); err != nil {
			return nil, err
		}
	}
	return controlPlane, nil
}

// NewOpaqueID returns a random, prefix-qualified public identifier.
func NewOpaqueID(prefix string) string {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("SecondBox random identifier generation failed: %v", err))
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(random)
}

// NewCredentialMaterial returns high-entropy plaintext used exactly once.
func NewCredentialMaterial() string {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("SecondBox credential material generation failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(random)
}

// CreateProfile creates one explicit Profile and immutable revision.
func (service *ControlPlaneService) CreateProfile(
	ctx context.Context,
	principal contracts.Principal,
	request contracts.CreateProfileRequest,
) (contracts.Profile, error) {
	profile, _, err := service.createProfile(ctx, principal, "", request)
	return profile, err
}

// CreateProfileIdempotent creates or replays one exact Profile response.
func (service *ControlPlaneService) CreateProfileIdempotent(
	ctx context.Context,
	principal contracts.Principal,
	idempotencyKey string,
	request contracts.CreateProfileRequest,
) (contracts.Profile, bool, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Profile{}, false, err
	}
	return service.createProfile(ctx, principal, idempotencyKey, request)
}

func (service *ControlPlaneService) createProfile(
	ctx context.Context,
	principal contracts.Principal,
	idempotencyKey string,
	request contracts.CreateProfileRequest,
) (contracts.Profile, bool, error) {
	if !profileNamePattern.MatchString(request.Name) {
		return contracts.Profile{}, false, errors.New("SecondBox Profile name must match ^[a-z][a-z0-9-]{0,79}$")
	}
	if service.isBuiltInProfile(request.Name) {
		return contracts.Profile{}, false, errors.New("SecondBox built-in Profile cannot be created or mutated")
	}
	if err := validateProfileRevisionSpec(request.Spec); err != nil {
		return contracts.Profile{}, false, err
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(
		principal, "profile.create", principal.ID, idempotencyKey, request, now,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	profile := contracts.Profile{
		Name: request.Name, State: contracts.ProfileStateEnabled, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
		CurrentRevision: contracts.ProfileRevision{
			ID: service.newID("prv"), Number: 1, Spec: request.Spec, CreatedAt: now,
		},
	}
	audit := service.newAudit(ctx, principal, "profile.created", "profile", profile.Name, "", now)
	profile, result, err := service.store.CreateProfile(
		ctx, profile, idempotency,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Profile{}, false, err
	}
	return profile, result.Replayed, nil
}

// ReviseProfile appends immutable policy without mutating pinned Sandboxes.
func (service *ControlPlaneService) ReviseProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	request contracts.ReviseProfileRequest,
) (contracts.Profile, error) {
	profile, _, err := service.reviseProfileAtRevision(ctx, principal, name, "", request, 0)
	return profile, err
}

// ReviseProfileAtRevisionIdempotent appends or replays one exact immutable Profile revision.
func (service *ControlPlaneService) ReviseProfileAtRevisionIdempotent(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	idempotencyKey string,
	request contracts.ReviseProfileRequest,
	expectedRevision int64,
) (contracts.Profile, bool, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Profile{}, false, err
	}
	return service.reviseProfileAtRevision(
		ctx, principal, name, idempotencyKey, request, expectedRevision,
	)
}

func (service *ControlPlaneService) reviseProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	idempotencyKey string,
	request contracts.ReviseProfileRequest,
	expectedRevision int64,
) (contracts.Profile, bool, error) {
	if service.isBuiltInProfile(name) {
		return contracts.Profile{}, false, errors.New("SecondBox built-in Profile cannot be created or mutated")
	}
	if err := validateProfileRevisionSpec(request.Spec); err != nil {
		return contracts.Profile{}, false, err
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(
		principal, "profile.revise", name, idempotencyKey,
		struct {
			Request          contracts.ReviseProfileRequest `json:"request"`
			ExpectedRevision int64                          `json:"expectedRevision"`
		}{Request: request, ExpectedRevision: expectedRevision},
		now,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	revision := contracts.ProfileRevision{
		ID: service.newID("prv"), Spec: request.Spec, CreatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "profile.revised", "profile", name, "", now)
	profile, result, err := service.store.ReviseProfile(
		ctx, name, revision, expectedRevision, now, idempotency,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Profile{}, false, err
	}
	return profile, result.Replayed, nil
}

// DisableProfile blocks future creation without rewriting pinned Sandboxes.
func (service *ControlPlaneService) DisableProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
) (contracts.Profile, error) {
	profile, _, err := service.disableProfileAtRevision(ctx, principal, name, "", 0)
	return profile, err
}

// DisableProfileAtRevisionIdempotent disables or replays one exact fenced Profile response.
func (service *ControlPlaneService) DisableProfileAtRevisionIdempotent(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Profile, bool, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Profile{}, false, err
	}
	return service.disableProfileAtRevision(
		ctx, principal, name, idempotencyKey, expectedRevision,
	)
}

func (service *ControlPlaneService) disableProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Profile, bool, error) {
	if service.isBuiltInProfile(name) {
		return contracts.Profile{}, false, errors.New("SecondBox built-in Profile cannot be created or mutated")
	}
	now := service.now().UTC()
	idempotency, err := service.adminIdempotency(
		principal, "profile.disable", name, idempotencyKey,
		struct {
			ExpectedRevision int64 `json:"expectedRevision"`
		}{ExpectedRevision: expectedRevision},
		now,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	audit := service.newAudit(ctx, principal, "profile.disabled", "profile", name, "", now)
	profile, result, err := service.store.DisableProfile(
		ctx, name, expectedRevision, now, idempotency,
	)
	if err != nil {
		return contracts.Profile{}, false, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Profile{}, false, err
	}
	return profile, result.Replayed, nil
}

// GetProfile returns a Profile head and its current immutable revision.
func (service *ControlPlaneService) GetProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
) (contracts.Profile, error) {
	if builtIn, ok := service.builtInProfiles[name]; ok {
		return service.store.EnsureBuiltInProfile(ctx, builtIn)
	}
	return service.store.GetProfile(ctx, name)
}

// ListProfiles returns a bounded stable Profile page.
func (service *ControlPlaneService) ListProfiles(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
	cursor string,
) (contracts.ProfilePage, error) {
	if err := service.ensureAllBuiltInProfiles(ctx); err != nil {
		return contracts.ProfilePage{}, err
	}
	return service.store.ListProfiles(ctx, boundedLimit(limit), cursor)
}

// CreateSandbox transactionally resolves authorization, compatibility, quota, and idempotency.
func (service *ControlPlaneService) CreateSandbox(
	ctx context.Context,
	principal contracts.Principal,
	idempotencyKey string,
	request contracts.CreateSandboxRequest,
) (contracts.Sandbox, bool, error) {
	sandbox, _, created, err := service.createSandboxOperation(ctx, principal, idempotencyKey, request)
	return sandbox, created, err
}

// CreateSandboxOperation creates durable Sandbox intent and returns its observable operation.
func (service *ControlPlaneService) CreateSandboxOperation(
	ctx context.Context,
	principal contracts.Principal,
	idempotencyKey string,
	request contracts.CreateSandboxRequest,
) (contracts.Operation, bool, error) {
	_, operation, created, err := service.createSandboxOperation(ctx, principal, idempotencyKey, request)
	return operation, created, err
}

func (service *ControlPlaneService) createSandboxOperation(
	ctx context.Context,
	principal contracts.Principal,
	idempotencyKey string,
	request contracts.CreateSandboxRequest,
) (contracts.Sandbox, contracts.Operation, bool, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Sandbox{}, contracts.Operation{}, false, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if !profileNamePattern.MatchString(request.Profile) {
		return contracts.Sandbox{}, contracts.Operation{}, false, errors.New("SecondBox Sandbox profile name is invalid")
	}
	if err := validateSandboxMetadata(request.Metadata); err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if len(request.SourceSnapshotID) > 128 {
		return contracts.Sandbox{}, contracts.Operation{}, false,
			errors.New("SecondBox source Snapshot ID exceeds its bound")
	}
	if builtIn, ok := service.builtInProfiles[request.Profile]; ok {
		if _, err := service.store.EnsureBuiltInProfile(ctx, builtIn); err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, err
		}
	}
	canonicalRequest, err := json.Marshal(request)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox request canonicalization failed: %w", err)
	}
	requestHash := sha256.Sum256(canonicalRequest)
	now := service.now().UTC()
	sandboxID := service.newID("sbx")
	workspaceID := service.newID("wsp")
	operationID := service.newID("op")
	workspaceEffectID := service.newID("effect")
	workspaceCommandID := service.newID("command")
	workspaceFence := []byte(service.newCredentialMaterial())
	requestID := service.requestID(ctx)
	sandbox := contracts.Sandbox{
		ID: sandboxID, Profile: request.Profile, State: contracts.SandboxStateCreating,
		DesiredState: contracts.SandboxDesiredStateStopped, Generation: 1,
		Workspace: contracts.Workspace{
			ID: workspaceID, Generation: 1, State: "creating",
			CreatedAt: now, UpdatedAt: now,
		},
		Metadata: cloneMetadata(request.Metadata), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	operation := contracts.Operation{
		ID: operationID, SandboxID: sandboxID, Kind: "create", State: contracts.OperationStatePending,
		RequestID: requestID, CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "sandbox.created", "sandbox", sandboxID, principal.TenantRef, now)
	storedSandbox, storedOperation, created, err := service.store.CreateSandbox(ctx, ports.CreateSandboxInput{
		Principal: principal, SubjectQuota: service.defaultSubjectQuota,
		Sandbox: sandbox, Workspace: sandbox.Workspace, Operation: operation,
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyEnds:   now.Add(idempotencyRetention),
		WorkspaceEffectID: workspaceEffectID, WorkspaceCommandID: workspaceCommandID,
		FencingToken: workspaceFence, SourceSnapshotID: request.SourceSnapshotID,
	})
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, err
	}
	if created {
		if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
			return contracts.Sandbox{}, contracts.Operation{}, false, err
		}
	}
	return storedSandbox, storedOperation, created, nil
}

// GetSandbox enforces non-enumerating Project isolation.
func (service *ControlPlaneService) GetSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
) (contracts.Sandbox, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Sandbox{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetSandbox(ctx, principal.TenantRef, principal.SubjectRef, sandboxID)
}

// UpdateSandboxMetadata replaces bounded consumer correlation metadata without
// changing lifecycle, workspace, generation, placement, or profile authority.
func (service *ControlPlaneService) UpdateSandboxMetadata(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	expectedRevision int64,
	request contracts.UpdateSandboxMetadataRequest,
) (contracts.Sandbox, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Sandbox{}, ports.ErrAuthorizationDenied
	}
	if expectedRevision < 1 {
		return contracts.Sandbox{}, errors.New("SecondBox Sandbox expected revision must be positive")
	}
	if err := validateSandboxMetadata(request.Metadata); err != nil {
		return contracts.Sandbox{}, err
	}
	now := service.now().UTC()
	sandbox, err := service.store.UpdateSandboxMetadata(ctx, ports.UpdateSandboxMetadataInput{
		Principal: principal, SandboxID: sandboxID,
		Metadata: cloneMetadata(request.Metadata), ExpectedRevision: expectedRevision,
		Now: now,
	})
	if err != nil {
		return contracts.Sandbox{}, err
	}
	audit := service.newAudit(
		ctx,
		principal,
		"sandbox.metadata.updated",
		"sandbox",
		sandboxID,
		principal.TenantRef,
		now,
	)
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Sandbox{}, err
	}
	return sandbox, nil
}

// ListSandboxes returns only the authenticated Project projection.
func (service *ControlPlaneService) ListSandboxes(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
	cursor string,
	metadata map[string]string,
) (contracts.SandboxPage, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.SandboxPage{}, ports.ErrAuthorizationDenied
	}
	return service.store.ListSandboxes(
		ctx, principal.TenantRef, principal.SubjectRef, boundedLimit(limit), cursor, metadata,
	)
}

// GetSubjectUsage returns aggregate quota reservations for the asserted subject.
func (service *ControlPlaneService) GetSubjectUsage(
	ctx context.Context,
	principal contracts.Principal,
) (contracts.SubjectUsage, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.SubjectUsage{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetSubjectUsage(ctx, principal.TenantRef, principal.SubjectRef)
}

// GetOperation returns one durable mutation observation inside the authenticated Project.
func (service *ControlPlaneService) GetOperation(
	ctx context.Context,
	principal contracts.Principal,
	operationID string,
) (contracts.Operation, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Operation{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetOperation(
		ctx, principal.TenantRef, principal.SubjectRef, operationID,
	)
}

// StartSandbox records running intent for asynchronous reconciliation.
func (service *ControlPlaneService) StartSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Operation, error) {
	return service.setSandboxDesiredState(
		ctx, principal, sandboxID, "start", contracts.SandboxDesiredStateRunning,
		idempotencyKey, expectedRevision, nil, nil,
	)
}

// StopSandbox converges the Sandbox to stopped after durable local generation advance.
func (service *ControlPlaneService) StopSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Operation, error) {
	return service.setSandboxDesiredState(
		ctx, principal, sandboxID, "stop", contracts.SandboxDesiredStateStopped,
		idempotencyKey, expectedRevision, nil, nil,
	)
}

// DeleteSandbox makes deletion dominate future lifecycle and activity admission.
func (service *ControlPlaneService) DeleteSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Operation, error) {
	return service.setSandboxDesiredState(
		ctx, principal, sandboxID, "delete", contracts.SandboxDesiredStateDeleted,
		idempotencyKey, expectedRevision, nil, nil,
	)
}

func (service *ControlPlaneService) setSandboxDesiredState(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	kind string,
	desiredState string,
	idempotencyKey string,
	expectedRevision int64,
	requestMetadata map[string]string,
	replayed *bool,
) (contracts.Operation, error) {
	if principal.TenantRef == "" {
		return contracts.Operation{}, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Operation{}, err
	}
	if expectedRevision < 1 {
		return contracts.Operation{}, errors.New("SecondBox lifecycle If-Match revision must be positive")
	}
	canonicalRequest, err := json.Marshal(struct {
		Kind     string            `json:"kind"`
		Metadata map[string]string `json:"metadata"`
	}{Kind: kind, Metadata: requestMetadata})
	if err != nil {
		return contracts.Operation{}, fmt.Errorf("SecondBox lifecycle request canonicalization failed: %w", err)
	}
	requestHash := sha256.Sum256(canonicalRequest)
	now := service.now().UTC()
	operation := contracts.Operation{
		ID: service.newID("op"), SandboxID: sandboxID, Kind: kind,
		State: contracts.OperationStatePending, RequestID: service.requestID(ctx),
		RequestMetadata: cloneMetadata(requestMetadata), CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(
		ctx, principal, "sandbox."+kind, "sandbox", sandboxID, principal.TenantRef, now,
	)
	storedOperation, err := service.store.SetSandboxDesiredState(ctx, ports.LifecycleIntentInput{
		Principal: principal, SandboxID: sandboxID, DesiredState: desiredState,
		Operation: operation, Now: now,
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyEnds: now.Add(idempotencyRetention), ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return contracts.Operation{}, err
	}
	if err := service.store.AppendAuditEvent(ctx, audit); err != nil {
		return contracts.Operation{}, err
	}
	if replayed != nil {
		*replayed = storedOperation.ID != operation.ID
	}
	return storedOperation, nil
}

// MutateSandbox exposes one exact HTTP lifecycle action with replay evidence.
func (service *ControlPlaneService) MutateSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	kind string,
	idempotencyKey string,
	expectedRevision int64,
	metadata map[string]string,
) (contracts.Operation, bool, error) {
	desiredState := contracts.SandboxDesiredStateStopped
	switch kind {
	case "start":
		desiredState = contracts.SandboxDesiredStateRunning
	case "drain", "stop":
	case "delete":
		desiredState = contracts.SandboxDesiredStateDeleted
	default:
		return contracts.Operation{}, false, errors.New("SecondBox lifecycle action is invalid")
	}
	var replayed bool
	operation, err := service.setSandboxDesiredState(
		ctx, principal, sandboxID, kind, desiredState, idempotencyKey,
		expectedRevision, metadata, &replayed,
	)
	return operation, replayed, err
}

// AcquireSandboxLease creates profile-bounded authority for useful activity.
func (service *ControlPlaneService) AcquireSandboxLease(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	idempotencyKey string,
	durationSeconds int64,
) (contracts.Lease, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if generation < 1 {
		return contracts.Lease{}, errors.New("SecondBox Lease generation must be positive")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	policy, _, err := service.store.GetSandboxLifecyclePolicy(
		ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if durationSeconds < 1 || durationSeconds > 86400 || durationSeconds > policy.LeaseSeconds {
		return contracts.Lease{}, errors.New("SecondBox Lease duration exceeds the pinned lifecycle policy")
	}
	requestHash, err := hashCanonicalRequest(struct {
		DurationSeconds int64 `json:"durationSeconds"`
	}{DurationSeconds: durationSeconds})
	if err != nil {
		return contracts.Lease{}, err
	}
	now := service.now().UTC()
	return service.store.AcquireLease(ctx, ports.LeaseInput{
		Lease:     contracts.Lease{ID: service.newID("lea")},
		TenantRef: principal.TenantRef, SandboxID: sandboxID, Generation: generation,
		SubjectRef: principal.SubjectRef,
		ExpiresAt:  now.Add(time.Duration(durationSeconds) * time.Second), Now: now,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention),
	})
}

// GetSandboxLease returns one Lease inside the authenticated Project.
func (service *ControlPlaneService) GetSandboxLease(
	ctx context.Context,
	principal contracts.Principal,
	leaseID string,
) (contracts.Lease, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetLeaseByID(ctx, principal.TenantRef, principal.SubjectRef, leaseID)
}

// RenewSandboxLease renews only current, active generation authority.
func (service *ControlPlaneService) RenewSandboxLease(
	ctx context.Context,
	principal contracts.Principal,
	leaseID string,
	idempotencyKey string,
	durationSeconds int64,
) (contracts.Lease, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := service.store.GetLeaseByID(
		ctx, principal.TenantRef, principal.SubjectRef, leaseID,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.SubjectRef != principal.SubjectRef {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	policy, _, err := service.store.GetSandboxLifecyclePolicy(
		ctx, principal.TenantRef, principal.SubjectRef, lease.SandboxID,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if durationSeconds < 1 || durationSeconds > 86400 || durationSeconds > policy.LeaseSeconds {
		return contracts.Lease{}, errors.New("SecondBox Lease duration exceeds the pinned lifecycle policy")
	}
	requestHash, err := hashCanonicalRequest(struct {
		DurationSeconds int64 `json:"durationSeconds"`
	}{DurationSeconds: durationSeconds})
	if err != nil {
		return contracts.Lease{}, err
	}
	now := service.now().UTC()
	return service.store.RenewLease(ctx, ports.LeaseInput{
		Lease: lease, TenantRef: principal.TenantRef, SandboxID: lease.SandboxID,
		SubjectRef: principal.SubjectRef,
		Generation: lease.Generation,
		ExpiresAt:  now.Add(time.Duration(durationSeconds) * time.Second), Now: now,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention),
	})
}

// ReleaseSandboxLease ends bounded authority without deleting the Sandbox.
func (service *ControlPlaneService) ReleaseSandboxLease(
	ctx context.Context,
	principal contracts.Principal,
	leaseID string,
	idempotencyKey string,
) (contracts.Lease, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := service.store.GetLeaseByID(
		ctx, principal.TenantRef, principal.SubjectRef, leaseID,
	)
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.SubjectRef != principal.SubjectRef {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	requestHash, err := hashCanonicalRequest(struct {
		LeaseID string `json:"leaseId"`
	}{LeaseID: leaseID})
	if err != nil {
		return contracts.Lease{}, err
	}
	now := service.now().UTC()
	return service.store.ReleaseLease(ctx, ports.LeaseInput{
		Lease: lease, TenantRef: principal.TenantRef, SandboxID: lease.SandboxID,
		SubjectRef: principal.SubjectRef,
		Generation: lease.Generation,
		Now:        now, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
		IdempotencyEnds: now.Add(idempotencyRetention),
	})
}

// ReportGuestLiveness persists runner-reported guest health without renewing useful activity.
func (service *ControlPlaneService) ReportGuestLiveness(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	liveness string,
) (contracts.Instance, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Instance{}, ports.ErrAuthorizationDenied
	}
	return service.store.PingGuest(ctx, ports.GenerationInput{
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		Generation: generation, Now: service.now().UTC(),
	}, liveness)
}

// InspectSandbox returns current persisted guest and session evidence for one generation.
func (service *ControlPlaneService) InspectSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
) (contracts.SandboxInspection, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.SandboxInspection{}, ports.ErrAuthorizationDenied
	}
	if generation < 1 {
		return contracts.SandboxInspection{}, errors.New("SecondBox inspection generation must be positive")
	}
	return service.store.ReadSandboxInspection(ctx, ports.GenerationInput{
		TenantRef: principal.TenantRef, SandboxID: sandboxID,
		SubjectRef: principal.SubjectRef,
		Generation: generation, Now: service.now().UTC(),
	})
}

// PingSandbox returns persisted health evidence without changing activity or liveness.
func (service *ControlPlaneService) PingSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
) (contracts.PingResult, error) {
	inspection, err := service.InspectSandbox(ctx, principal, sandboxID, generation)
	if err != nil {
		return contracts.PingResult{}, err
	}
	return contracts.PingResult{
		SandboxID: sandboxID, Generation: generation,
		Healthy: inspection.GuestHealthy, ObservedAt: inspection.ObservedAt,
	}, nil
}

// TouchSandbox records only explicit useful client activity.
func (service *ControlPlaneService) TouchSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	leaseID string,
	idempotencyKey string,
) (contracts.TouchResult, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.TouchResult{}, ports.ErrAuthorizationDenied
	}
	if generation < 1 {
		return contracts.TouchResult{}, errors.New("SecondBox touch generation must be positive")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.TouchResult{}, err
	}
	requestHash, err := hashCanonicalRequest(struct {
		Generation int64  `json:"generation"`
		LeaseID    string `json:"leaseId"`
	}{Generation: generation, LeaseID: leaseID})
	if err != nil {
		return contracts.TouchResult{}, err
	}
	touchedAt, err := service.store.TouchActivity(ctx, ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			TenantRef: principal.TenantRef, SandboxID: sandboxID,
			SubjectRef: principal.SubjectRef,
			Generation: generation, Now: service.now().UTC(),
		},
		LeaseID:        leaseID,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash,
	})
	if err != nil {
		return contracts.TouchResult{}, err
	}
	return contracts.TouchResult{
		SandboxID: sandboxID, Generation: generation, LastActivityAt: touchedAt,
	}, nil
}

// WaitSandbox blocks until the persisted Sandbox reaches one requested state.
func (service *ControlPlaneService) WaitSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	request contracts.WaitSandboxRequest,
) (contracts.Sandbox, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.Sandbox{}, ports.ErrAuthorizationDenied
	}
	if request.DeadlineMilliseconds < 1 || request.DeadlineMilliseconds > 60000 {
		return contracts.Sandbox{}, errors.New("SecondBox wait deadlineMilliseconds must be between 1 and 60000")
	}
	if len(request.States) < 1 || len(request.States) > 10 {
		return contracts.Sandbox{}, errors.New("SecondBox wait states must contain between 1 and 10 values")
	}
	requested := make(map[string]struct{}, len(request.States))
	for _, state := range request.States {
		if !validSandboxState(state) {
			return contracts.Sandbox{}, errors.New("SecondBox wait state is invalid")
		}
		if _, duplicate := requested[state]; duplicate {
			return contracts.Sandbox{}, errors.New("SecondBox wait states must be unique")
		}
		requested[state] = struct{}{}
	}
	deadline := time.NewTimer(time.Duration(request.DeadlineMilliseconds) * time.Millisecond)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		sandbox, err := service.store.GetSandbox(
			ctx, principal.TenantRef, principal.SubjectRef, sandboxID,
		)
		if err != nil {
			return contracts.Sandbox{}, err
		}
		if _, reached := requested[sandbox.State]; reached {
			return sandbox, nil
		}
		select {
		case <-ctx.Done():
			return contracts.Sandbox{}, ctx.Err()
		case <-deadline.C:
			return contracts.Sandbox{}, ports.ErrWaitExpired
		case <-poll.C:
		}
	}
}

// OpenActivitySession admits generation-bound exec, file, PTY, or port work.
func (service *ControlPlaneService) OpenActivitySession(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	leaseID string,
	kind string,
) (contracts.ActivitySession, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.ActivitySession{}, ports.ErrAuthorizationDenied
	}
	now := service.now().UTC()
	return service.store.OpenActivitySession(ctx, ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			TenantRef: principal.TenantRef, SandboxID: sandboxID, Generation: generation, Now: now,
			SubjectRef: principal.SubjectRef,
		},
		Session: contracts.ActivitySession{ID: service.newID("act"), Kind: kind},
		LeaseID: leaseID,
	})
}

// CloseActivitySession releases idle suppression for completed useful work.
func (service *ControlPlaneService) CloseActivitySession(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	generation int64,
	sessionID string,
) (contracts.ActivitySession, error) {
	if principal.TenantRef == "" || principal.SubjectRef == "" {
		return contracts.ActivitySession{}, ports.ErrAuthorizationDenied
	}
	return service.store.CloseActivitySession(ctx, ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			TenantRef: principal.TenantRef, SandboxID: sandboxID,
			SubjectRef: principal.SubjectRef,
			Generation: generation, Now: service.now().UTC(),
		},
		Session: contracts.ActivitySession{ID: sessionID},
	})
}

// Ready proves the database authority is reachable.
func (service *ControlPlaneService) Ready(ctx context.Context) error {
	return service.store.Ping(ctx)
}

// Metrics returns only fixed-cardinality state counts.
func (service *ControlPlaneService) Metrics(ctx context.Context) (contracts.MetricsSnapshot, error) {
	return service.store.ReadMetricsSnapshot(ctx)
}

func (service *ControlPlaneService) adminIdempotency(
	principal contracts.Principal,
	operation string,
	targetID string,
	idempotencyKey string,
	request any,
	now time.Time,
) (ports.AdminIdempotencyInput, error) {
	if idempotencyKey == "" {
		return ports.AdminIdempotencyInput{}, nil
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return ports.AdminIdempotencyInput{}, err
	}
	requestHash, err := hashCanonicalRequest(request)
	if err != nil {
		return ports.AdminIdempotencyInput{}, err
	}
	return ports.AdminIdempotencyInput{
		TenantRef: principal.TenantRef, Operation: operation, TargetID: targetID,
		Key: idempotencyKey, RequestHash: requestHash,
		Now: now.UTC(), Ends: now.UTC().Add(idempotencyRetention),
	}, nil
}

func (service *ControlPlaneService) newAudit(
	ctx context.Context,
	principal contracts.Principal,
	action string,
	resourceKind string,
	resourceID string,
	projectID string,
	now time.Time,
) contracts.AuditEvent {
	tenantRef, subjectRef := principal.TenantRef, principal.SubjectRef
	if tenantRef == "" {
		tenantRef = projectID
	}
	if tenantRef == "" {
		tenantRef = "secondbox"
	}
	if subjectRef == "" {
		subjectRef = principal.ID
	}
	if subjectRef == "" {
		subjectRef = "secondbox"
	}
	return contracts.AuditEvent{
		ID: service.newID("aud"), TenantRef: projectID,
		SubjectRef: subjectRef, ActorKind: principal.Kind,
		ActorID: principal.ID, Action: action, ResourceKind: resourceKind,
		ResourceID: resourceID, Outcome: "accepted", RequestID: service.requestID(ctx),
		Details: map[string]string{}, CreatedAt: now,
	}
}

func validateProfileRevisionSpec(spec contracts.ProfileRevisionSpec) error {
	if !profileNamePattern.MatchString(spec.Pool) {
		return errors.New("SecondBox Profile runner pool selector is invalid")
	}
	if spec.Architecture != "amd64" && spec.Architecture != "arm64" {
		return errors.New("SecondBox Profile architecture must be amd64 or arm64")
	}
	if !digestPattern.MatchString(spec.RuntimeBundleDigest) || !digestPattern.MatchString(spec.ToolchainBundleDigest) {
		return errors.New("SecondBox Profile immutable artifact references must be sha256 digests")
	}
	if spec.Resources.CPUMillis < 1 || spec.Resources.MemoryBytes < 1 || spec.Resources.WorkspaceBytes < 1 ||
		spec.Resources.ProcessLimit < 1 || spec.Resources.ConcurrentOperations < 1 {
		return errors.New("SecondBox Profile resource limits must be positive")
	}
	if spec.Lifecycle.InitialState != contracts.SandboxDesiredStateStopped &&
		spec.Lifecycle.InitialState != contracts.SandboxDesiredStateRunning {
		return errors.New("SecondBox Profile initial state must be stopped or running")
	}
	if spec.Lifecycle.DrainGraceSeconds < 1 || spec.Lifecycle.IdleSeconds < 1 ||
		spec.Lifecycle.MaximumDurationSeconds < 1 || spec.Lifecycle.LeaseSeconds < 1 {
		return errors.New("SecondBox Profile lifecycle limits must be positive")
	}
	if spec.Retention.SnapshotRetentionSeconds < 1 ||
		spec.Retention.SnapshotLimit < 0 || spec.Retention.ArtifactRetentionSeconds < 1 {
		return errors.New("SecondBox Profile retention limits are invalid")
	}
	if spec.Execution.MaximumDeadlineMilliseconds < 1 || spec.Execution.MaximumBufferedOutputBytes < 1 ||
		spec.Execution.StreamWindowBytes < 4096 || spec.Execution.MaximumTransferBytes < 1 ||
		spec.Execution.TerminalDetachSeconds < 0 {
		return errors.New("SecondBox Profile execution limits are invalid")
	}
	if spec.Network.Mode != "deny_all" && spec.Network.Mode != "allow_list" {
		return errors.New("SecondBox Profile network mode must be deny_all or allow_list")
	}
	if spec.Network.Mode == "deny_all" && len(spec.Network.Destinations) != 0 {
		return errors.New("SecondBox Profile deny_all network policy cannot contain destinations")
	}
	if len(spec.Network.Destinations) > 128 || len(spec.Ports) > 32 {
		return errors.New("SecondBox Profile network or port policy exceeds its bounded size")
	}
	for _, port := range spec.Ports {
		if port.Name == "" || port.Port < 1 || port.Port > 65535 ||
			(port.Protocol != "tcp" && port.Protocol != "http") ||
			port.MaximumSessions < 1 || port.MaximumSessionSeconds < 1 {
			return errors.New("SecondBox Profile exposed-port policy is invalid")
		}
	}
	return nil
}

func validateSandboxMetadata(metadata map[string]string) error {
	if metadata == nil {
		return errors.New("SecondBox Sandbox metadata object is required")
	}
	if len(metadata) > 32 {
		return errors.New("SecondBox Sandbox metadata must not exceed 32 entries")
	}
	for key, value := range metadata {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 1024 {
			return errors.New("SecondBox Sandbox metadata key or value exceeds its bound")
		}
	}
	return validateReservedSandboxName(metadata)
}

// validateReservedSandboxName bounds the reserved name so that it identifies one
// Sandbox unambiguously. Uniqueness is the database's to enforce; this rejects
// the names that could never resolve in the first place.
func validateReservedSandboxName(metadata map[string]string) error {
	name, present := metadata[contracts.SandboxNameMetadataKey]
	if !present {
		return nil
	}
	if name != strings.TrimSpace(name) || name == "" {
		return fmt.Errorf(
			"SecondBox Sandbox metadata %s must not be blank or surrounded by whitespace",
			contracts.SandboxNameMetadataKey,
		)
	}
	if strings.HasPrefix(name, contracts.SandboxIDPrefix) {
		return fmt.Errorf(
			"SecondBox Sandbox metadata %s must not begin with %q, which identifies a Sandbox",
			contracts.SandboxNameMetadataKey, contracts.SandboxIDPrefix,
		)
	}
	return nil
}

func validateIdempotencyKey(key string) error {
	if len(key) < 8 || len(key) > 200 || !idempotencyKeyPattern.MatchString(key) {
		return errors.New("SecondBox Idempotency-Key must contain 8 to 200 permitted ASCII characters")
	}
	return nil
}

func hashCanonicalRequest(request any) (string, error) {
	canonical, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("SecondBox request canonicalization failed: %w", err)
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func validSandboxState(state string) bool {
	switch state {
	case contracts.SandboxStateCreating,
		contracts.SandboxStateStopped,
		contracts.SandboxStateStarting,
		contracts.SandboxStateReady,
		contracts.SandboxStateDraining,
		contracts.SandboxStateStopping,
		contracts.SandboxStateFailed,
		contracts.SandboxStateDeleting,
		contracts.SandboxStateDeleted:
		return true
	default:
		return false
	}
}

func validateQuotaLimits(name string, quota contracts.QuotaLimits) error {
	if quota.MaxSandboxes < 1 {
		return fmt.Errorf("SecondBox %s Sandbox quota must be positive", name)
	}
	values := []int64{
		quota.MaxActiveInstances, quota.MaxCPUMillis,
		quota.MaxMemoryBytes, quota.MaxArtifactBytes, quota.MaxSnapshots,
		quota.MaxArtifacts, quota.MaxPortSessions, quota.MaxConcurrentOperations,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("SecondBox %s quota limits must be non-negative", name)
		}
	}
	return nil
}

func boundedLimit(limit int) int {
	if limit < 1 {
		return 100
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func sortedUnique(values []string) []string {
	unique := make(map[string]bool, len(values))
	for _, value := range values {
		unique[value] = true
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneMetadata(metadata map[string]string) map[string]string {
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
