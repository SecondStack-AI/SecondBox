// Package service validates and coordinates standalone SecondBox authority.
package service

import (
	"context"
	"crypto/hmac"
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
	"unicode/utf8"

	"github.com/SecondStack-AI/SecondBox/internal/objectstore"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const idempotencyRetention = 24 * time.Hour

var (
	profileNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,79}$`)
	digestPattern         = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~:+/=-]+$`)
)

// ControlPlaneConfig contains explicit authority, quota, time, and identity dependencies.
type ControlPlaneConfig struct {
	Store                 ports.ControlPlaneStore
	BootstrapAdminToken   string
	APIKeyHashSecret      []byte
	DefaultProjectQuota   contracts.QuotaLimits
	DefaultProfileQuota   contracts.QuotaLimits
	Now                   func() time.Time
	NewID                 func(string) string
	NewCredentialMaterial func() string
	ObjectStore           objectstore.Store
	DataPlaneRelay        DataPlaneRelay
	DataPlanePollInterval time.Duration
	PortSessionRelay      runnercontrol.PortSessionRelay
	PublicBaseURL         string
}

// ControlPlaneService owns validation, authentication, and transaction inputs.
type ControlPlaneService struct {
	store                   ports.ControlPlaneStore
	bootstrapCredentialHash []byte
	apiKeyHashSecret        []byte
	defaultProjectQuota     contracts.QuotaLimits
	defaultProfileQuota     contracts.QuotaLimits
	now                     func() time.Time
	newID                   func(string) string
	newCredentialMaterial   func() string
	objectStore             objectstore.Store
	dataPlaneRelay          DataPlaneRelay
	dataPlanePollInterval   time.Duration
	portSessionRelay        runnercontrol.PortSessionRelay
	publicBaseURL           string
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
	if len(config.BootstrapAdminToken) < 24 {
		return nil, errors.New("SecondBox bootstrap administrator token must contain at least 24 bytes")
	}
	if len(config.APIKeyHashSecret) < 32 {
		return nil, errors.New("SecondBox API key hash secret must contain at least 32 bytes")
	}
	if err := validateQuotaLimits("default Project", config.DefaultProjectQuota); err != nil {
		return nil, err
	}
	if err := validateQuotaLimits("default Profile", config.DefaultProfileQuota); err != nil {
		return nil, err
	}
	if config.Now == nil || config.NewID == nil || config.NewCredentialMaterial == nil {
		return nil, errors.New("SecondBox clock, identifier, and credential generators are required")
	}
	controlPlane := &ControlPlaneService{
		store:               config.Store,
		apiKeyHashSecret:    append([]byte(nil), config.APIKeyHashSecret...),
		defaultProjectQuota: config.DefaultProjectQuota, defaultProfileQuota: config.DefaultProfileQuota,
		now: config.Now, newID: config.NewID, newCredentialMaterial: config.NewCredentialMaterial,
		objectStore:    config.ObjectStore,
		dataPlaneRelay: config.DataPlaneRelay, dataPlanePollInterval: config.DataPlanePollInterval,
		portSessionRelay: config.PortSessionRelay, publicBaseURL: config.PublicBaseURL,
	}
	if config.DataPlaneRelay != nil && config.DataPlanePollInterval <= 0 {
		return nil, errors.New("SecondBox data-plane poll interval is required with the relay")
	}
	if config.PortSessionRelay != nil {
		if _, err := validatedPublicBaseURL(config.PublicBaseURL); err != nil {
			return nil, err
		}
	}
	controlPlane.bootstrapCredentialHash = controlPlane.hashCredential(config.BootstrapAdminToken)
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

// BootstrapAdmin returns the persisted bootstrap operator's administrative authority.
func (service *ControlPlaneService) BootstrapAdmin() contracts.Principal {
	return contracts.Principal{
		Kind: "operator", ID: "bootstrap_operator", BootstrapAdmin: true,
		Scopes: []string{
			contracts.ScopeAdminProjects, contracts.ScopeAdminKeys, contracts.ScopeAdminProfiles,
			contracts.ScopeAdminRunners, contracts.ScopeAdminAudit, contracts.ScopeDiagnostics,
		},
	}
}

// InitializeBootstrapAdmin creates the first operator or verifies its durable credential.
func (service *ControlPlaneService) InitializeBootstrapAdmin(ctx context.Context) error {
	now := service.now().UTC()
	audit := service.newAudit(
		ctx, service.BootstrapAdmin(), "operator.bootstrapped", "operator",
		"bootstrap_operator", "", now,
	)
	return service.store.InitializeBootstrapAdmin(ctx, service.bootstrapCredentialHash, now, audit)
}

// AuthenticateCredential derives exactly one Project principal from a bearer credential.
func (service *ControlPlaneService) AuthenticateCredential(
	ctx context.Context,
	credential string,
) (contracts.Principal, error) {
	prefix, err := parseAPIKeyPrefix(credential)
	if err != nil {
		now := service.now().UTC()
		audit := service.newAudit(
			ctx, contracts.Principal{Kind: "operator"}, "operator.authenticated",
			"operator", "", "", now,
		)
		return service.store.AuthenticateBootstrapAdmin(
			ctx, service.hashCredential(credential), now, audit,
		)
	}
	now := service.now().UTC()
	audit := service.newAudit(
		ctx, contracts.Principal{Kind: "service_account"},
		"api_key.authenticated", "api_key", "", "", now,
	)
	return service.store.AuthenticateAPIKey(ctx, prefix, service.hashCredential(credential), now, audit)
}

// CreateProject creates one Project with explicit process-configured quota limits.
func (service *ControlPlaneService) CreateProject(
	ctx context.Context,
	principal contracts.Principal,
	request contracts.CreateProjectRequest,
) (contracts.Project, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.Project{}, err
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || utf8.RuneCountInString(name) > 120 {
		return contracts.Project{}, errors.New("SecondBox Project name must contain between 1 and 120 characters")
	}
	now := service.now().UTC()
	project := contracts.Project{
		ID: service.newID("prj"), Name: name, State: contracts.ProjectStateActive,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "project.created", "project", project.ID, project.ID, now)
	return service.store.CreateProject(ctx, project, service.defaultProjectQuota, audit)
}

// UpdateProject changes supplied Project fields under optimistic revision fencing.
func (service *ControlPlaneService) UpdateProject(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	request contracts.UpdateProjectRequest,
	expectedRevision int64,
) (contracts.Project, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.Project{}, err
	}
	if request.Name == nil && request.State == nil {
		return contracts.Project{}, errors.New("SecondBox Project update requires a mutable field")
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" || utf8.RuneCountInString(name) > 120 {
			return contracts.Project{}, errors.New("SecondBox Project name must contain between 1 and 120 characters")
		}
		request.Name = &name
	}
	if request.State != nil && *request.State != contracts.ProjectStateActive && *request.State != contracts.ProjectStateDisabled {
		return contracts.Project{}, errors.New("SecondBox Project state must be active or disabled")
	}
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "project.updated", "project", projectID, projectID, now)
	return service.store.UpdateProject(ctx, projectID, request, expectedRevision, now, audit)
}

// GetProject returns one Project to bootstrap administration.
func (service *ControlPlaneService) GetProject(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
) (contracts.Project, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.Project{}, err
	}
	return service.store.GetProject(ctx, projectID)
}

// ListProjects returns a bounded stable Project page.
func (service *ControlPlaneService) ListProjects(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
) ([]contracts.Project, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return nil, err
	}
	return service.store.ListProjects(ctx, boundedLimit(limit))
}

// CreateServiceAccount creates one project-scoped application identity.
func (service *ControlPlaneService) CreateServiceAccount(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	request contracts.CreateServiceAccountRequest,
) (contracts.ServiceAccount, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.ServiceAccount{}, err
	}
	if err := validateServiceAccountAuthority(request.Name, request.Scopes, request.ProfileGrants); err != nil {
		return contracts.ServiceAccount{}, err
	}
	now := service.now().UTC()
	account := contracts.ServiceAccount{
		ID: service.newID("svc"), ProjectID: projectID, Name: strings.TrimSpace(request.Name),
		State: contracts.ServiceAccountStateActive, Scopes: sortedUnique(request.Scopes),
		ProfileGrants: sortedUnique(request.ProfileGrants), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "service_account.created", "service_account", account.ID, projectID, now)
	return service.store.CreateServiceAccount(ctx, account, audit)
}

// UpdateServiceAccount changes supplied application authority fields.
func (service *ControlPlaneService) UpdateServiceAccount(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	request contracts.UpdateServiceAccountRequest,
) (contracts.ServiceAccount, error) {
	return service.updateServiceAccountAtRevision(ctx, principal, projectID, accountID, request, 0)
}

// UpdateServiceAccountAtRevision applies HTTP If-Match fencing.
func (service *ControlPlaneService) UpdateServiceAccountAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	request contracts.UpdateServiceAccountRequest,
	expectedRevision int64,
) (contracts.ServiceAccount, error) {
	return service.updateServiceAccountAtRevision(ctx, principal, projectID, accountID, request, expectedRevision)
}

func (service *ControlPlaneService) updateServiceAccountAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	request contracts.UpdateServiceAccountRequest,
	expectedRevision int64,
) (contracts.ServiceAccount, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.ServiceAccount{}, err
	}
	if request.Name == nil && request.State == nil && request.Scopes == nil && request.ProfileGrants == nil {
		return contracts.ServiceAccount{}, errors.New("SecondBox ServiceAccount update requires a mutable field")
	}
	if request.Name != nil && (strings.TrimSpace(*request.Name) == "" || utf8.RuneCountInString(*request.Name) > 120) {
		return contracts.ServiceAccount{}, errors.New("SecondBox ServiceAccount name must contain between 1 and 120 characters")
	}
	if request.State != nil && *request.State != contracts.ServiceAccountStateActive && *request.State != contracts.ServiceAccountStateDisabled {
		return contracts.ServiceAccount{}, errors.New("SecondBox ServiceAccount state must be active or disabled")
	}
	if request.Scopes != nil {
		if err := validateApplicationScopes(*request.Scopes); err != nil {
			return contracts.ServiceAccount{}, err
		}
		scopes := sortedUnique(*request.Scopes)
		request.Scopes = &scopes
	}
	if request.ProfileGrants != nil {
		grants := sortedUnique(*request.ProfileGrants)
		if len(grants) > 128 {
			return contracts.ServiceAccount{}, errors.New("SecondBox ServiceAccount profile grants must not exceed 128 entries")
		}
		request.ProfileGrants = &grants
	}
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "service_account.updated", "service_account", accountID, projectID, now)
	return service.store.UpdateServiceAccount(ctx, projectID, accountID, request, expectedRevision, now, audit)
}

// GetServiceAccount returns one identity inside an explicit Project.
func (service *ControlPlaneService) GetServiceAccount(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
) (contracts.ServiceAccount, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return contracts.ServiceAccount{}, err
	}
	return service.store.GetServiceAccount(ctx, projectID, accountID)
}

// ListServiceAccounts returns a bounded stable Project identity page.
func (service *ControlPlaneService) ListServiceAccounts(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	limit int,
) ([]contracts.ServiceAccount, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProjects); err != nil {
		return nil, err
	}
	return service.store.ListServiceAccounts(ctx, projectID, boundedLimit(limit))
}

// CreateAPIKey returns plaintext exactly once while persisting only a keyed hash.
func (service *ControlPlaneService) CreateAPIKey(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	request contracts.CreateAPIKeyRequest,
) (contracts.CreateAPIKeyResponse, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminKeys); err != nil {
		return contracts.CreateAPIKeyResponse{}, err
	}
	if strings.TrimSpace(request.Name) == "" || utf8.RuneCountInString(request.Name) > 120 {
		return contracts.CreateAPIKeyResponse{}, errors.New("SecondBox APIKey name must contain between 1 and 120 characters")
	}
	if err := validateApplicationScopes(request.Scopes); err != nil {
		return contracts.CreateAPIKeyResponse{}, err
	}
	now := service.now().UTC()
	if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
		return contracts.CreateAPIKeyResponse{}, errors.New("SecondBox APIKey expiry must be in the future")
	}
	credential, prefix := service.issueCredential()
	key := contracts.APIKey{
		ID: service.newID("key"), ServiceAccountID: accountID, Name: strings.TrimSpace(request.Name),
		Prefix: prefix, State: contracts.APIKeyStateActive, Scopes: sortedUnique(request.Scopes),
		ExpiresAt: request.ExpiresAt, Revision: 1, CreatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "api_key.created", "api_key", key.ID, projectID, now)
	persisted, err := service.store.CreateAPIKey(ctx, ports.StoredAPIKey{
		APIKey: key, ProjectID: projectID, CredentialHash: service.hashCredential(credential), UpdatedAt: now,
	}, audit)
	if err != nil {
		return contracts.CreateAPIKeyResponse{}, err
	}
	return contracts.CreateAPIKeyResponse{APIKey: persisted, Credential: credential}, nil
}

// RotateAPIKey invalidates old plaintext and returns one replacement credential.
func (service *ControlPlaneService) RotateAPIKey(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	keyID string,
) (contracts.CreateAPIKeyResponse, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminKeys); err != nil {
		return contracts.CreateAPIKeyResponse{}, err
	}
	credential, prefix := service.issueCredential()
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "api_key.rotated", "api_key", keyID, projectID, now)
	key, err := service.store.RotateAPIKey(
		ctx, projectID, accountID, keyID, prefix, service.hashCredential(credential), now, audit,
	)
	if err != nil {
		return contracts.CreateAPIKeyResponse{}, err
	}
	return contracts.CreateAPIKeyResponse{APIKey: key, Credential: credential}, nil
}

// RevokeAPIKey immediately prevents future admission and lease renewal.
func (service *ControlPlaneService) RevokeAPIKey(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
	keyID string,
) (contracts.APIKey, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminKeys); err != nil {
		return contracts.APIKey{}, err
	}
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "api_key.revoked", "api_key", keyID, projectID, now)
	return service.store.RevokeAPIKey(ctx, projectID, accountID, keyID, now, audit)
}

// ListAPIKeys returns non-secret key metadata and last-use evidence.
func (service *ControlPlaneService) ListAPIKeys(
	ctx context.Context,
	principal contracts.Principal,
	projectID string,
	accountID string,
) ([]contracts.APIKey, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminKeys); err != nil {
		return nil, err
	}
	return service.store.ListAPIKeys(ctx, projectID, accountID, 200)
}

// CreateProfile creates one explicit Profile and immutable revision.
func (service *ControlPlaneService) CreateProfile(
	ctx context.Context,
	principal contracts.Principal,
	request contracts.CreateProfileRequest,
) (contracts.Profile, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProfiles); err != nil {
		return contracts.Profile{}, err
	}
	if !profileNamePattern.MatchString(request.Name) {
		return contracts.Profile{}, errors.New("SecondBox Profile name must match ^[a-z][a-z0-9-]{0,79}$")
	}
	if err := validateProfileRevisionSpec(request.Spec); err != nil {
		return contracts.Profile{}, err
	}
	now := service.now().UTC()
	profile := contracts.Profile{
		Name: request.Name, State: contracts.ProfileStateEnabled, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
		CurrentRevision: contracts.ProfileRevision{
			ID: service.newID("prv"), Number: 1, Spec: request.Spec, CreatedAt: now,
		},
	}
	audit := service.newAudit(ctx, principal, "profile.created", "profile", profile.Name, "", now)
	return service.store.CreateProfile(ctx, profile, service.defaultProfileQuota, audit)
}

// ReviseProfile appends immutable policy without mutating pinned Sandboxes.
func (service *ControlPlaneService) ReviseProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	request contracts.ReviseProfileRequest,
) (contracts.Profile, error) {
	return service.reviseProfileAtRevision(ctx, principal, name, request, 0)
}

// ReviseProfileAtRevision applies HTTP If-Match fencing before appending policy.
func (service *ControlPlaneService) ReviseProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	request contracts.ReviseProfileRequest,
	expectedRevision int64,
) (contracts.Profile, error) {
	return service.reviseProfileAtRevision(ctx, principal, name, request, expectedRevision)
}

func (service *ControlPlaneService) reviseProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	request contracts.ReviseProfileRequest,
	expectedRevision int64,
) (contracts.Profile, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProfiles); err != nil {
		return contracts.Profile{}, err
	}
	if err := validateProfileRevisionSpec(request.Spec); err != nil {
		return contracts.Profile{}, err
	}
	now := service.now().UTC()
	revision := contracts.ProfileRevision{
		ID: service.newID("prv"), Spec: request.Spec, CreatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "profile.revised", "profile", name, "", now)
	return service.store.ReviseProfile(ctx, name, revision, expectedRevision, now, audit)
}

// DisableProfile blocks future creation without rewriting pinned Sandboxes.
func (service *ControlPlaneService) DisableProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
) (contracts.Profile, error) {
	return service.disableProfileAtRevision(ctx, principal, name, 0)
}

// DisableProfileAtRevision applies HTTP If-Match fencing before disabling creation.
func (service *ControlPlaneService) DisableProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	expectedRevision int64,
) (contracts.Profile, error) {
	return service.disableProfileAtRevision(ctx, principal, name, expectedRevision)
}

func (service *ControlPlaneService) disableProfileAtRevision(
	ctx context.Context,
	principal contracts.Principal,
	name string,
	expectedRevision int64,
) (contracts.Profile, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProfiles); err != nil {
		return contracts.Profile{}, err
	}
	now := service.now().UTC()
	audit := service.newAudit(ctx, principal, "profile.disabled", "profile", name, "", now)
	return service.store.DisableProfile(ctx, name, expectedRevision, now, audit)
}

// GetProfile returns a Profile head and its current immutable revision.
func (service *ControlPlaneService) GetProfile(
	ctx context.Context,
	principal contracts.Principal,
	name string,
) (contracts.Profile, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProfiles); err != nil {
		return contracts.Profile{}, err
	}
	return service.store.GetProfile(ctx, name)
}

// ListProfiles returns a bounded stable Profile page.
func (service *ControlPlaneService) ListProfiles(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
) ([]contracts.Profile, error) {
	if err := requireAdminScope(principal, contracts.ScopeAdminProfiles); err != nil {
		return nil, err
	}
	return service.store.ListProfiles(ctx, boundedLimit(limit))
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
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" || principal.ServiceAccountID == "" {
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
	canonicalRequest, err := json.Marshal(request)
	if err != nil {
		return contracts.Sandbox{}, contracts.Operation{}, false, fmt.Errorf("SecondBox Sandbox request canonicalization failed: %w", err)
	}
	requestHash := sha256.Sum256(canonicalRequest)
	now := service.now().UTC()
	sandboxID := service.newID("sbx")
	workspaceID := service.newID("wsp")
	operationID := service.newID("op")
	requestID := service.requestID(ctx)
	sandbox := contracts.Sandbox{
		ID: sandboxID, Profile: request.Profile, State: contracts.SandboxStateCreating,
		DesiredState: contracts.SandboxDesiredStateStopped, Generation: 1,
		Workspace: contracts.Workspace{
			ID: workspaceID, Generation: 1, RetainedBytes: 0, CreatedAt: now, UpdatedAt: now,
		},
		Metadata: cloneMetadata(request.Metadata), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	operation := contracts.Operation{
		ID: operationID, SandboxID: sandboxID, Kind: "create", State: contracts.OperationStatePending,
		RequestID: requestID, CreatedAt: now, UpdatedAt: now,
	}
	audit := service.newAudit(ctx, principal, "sandbox.created", "sandbox", sandboxID, principal.ProjectID, now)
	return service.store.CreateSandbox(ctx, ports.CreateSandboxInput{
		Principal: principal, Sandbox: sandbox, Workspace: sandbox.Workspace, Operation: operation,
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyEnds: now.Add(idempotencyRetention), Audit: audit,
	})
}

// GetSandbox enforces non-enumerating Project isolation.
func (service *ControlPlaneService) GetSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
) (contracts.Sandbox, error) {
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return contracts.Sandbox{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetSandbox(ctx, principal.ProjectID, sandboxID)
}

// ListSandboxes returns only the authenticated Project projection.
func (service *ControlPlaneService) ListSandboxes(
	ctx context.Context,
	principal contracts.Principal,
	limit int,
) ([]contracts.Sandbox, error) {
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return nil, ports.ErrAuthorizationDenied
	}
	return service.store.ListSandboxes(ctx, principal.ProjectID, boundedLimit(limit))
}

// GetOperation returns one durable mutation observation inside the authenticated Project.
func (service *ControlPlaneService) GetOperation(
	ctx context.Context,
	principal contracts.Principal,
	operationID string,
) (contracts.Operation, error) {
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return contracts.Operation{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetOperation(ctx, principal.ProjectID, operationID)
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

// DrainSandbox rejects new work and converges the Sandbox to stopped.
func (service *ControlPlaneService) DrainSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
) (contracts.Operation, error) {
	return service.setSandboxDesiredState(
		ctx, principal, sandboxID, "drain", contracts.SandboxDesiredStateStopped,
		idempotencyKey, expectedRevision, nil, nil,
	)
}

// StopSandbox converges the Sandbox to stopped under its pinned checkpoint policy.
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

// CheckpointSandbox drains before publishing an immutable stopped-state checkpoint.
func (service *ControlPlaneService) CheckpointSandbox(
	ctx context.Context,
	principal contracts.Principal,
	sandboxID string,
	idempotencyKey string,
	expectedRevision int64,
	metadata map[string]string,
) (contracts.Operation, error) {
	if err := validateSandboxMetadata(metadata); err != nil {
		return contracts.Operation{}, err
	}
	return service.setSandboxDesiredState(
		ctx, principal, sandboxID, "checkpoint", contracts.SandboxDesiredStateStopped,
		idempotencyKey, expectedRevision, metadata, nil,
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
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" {
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
		ctx, principal, "sandbox."+kind, "sandbox", sandboxID, principal.ProjectID, now,
	)
	storedOperation, err := service.store.SetSandboxDesiredState(ctx, ports.LifecycleIntentInput{
		Principal: principal, SandboxID: sandboxID, DesiredState: desiredState,
		Operation: operation, Audit: audit, Now: now,
		IdempotencyKey: idempotencyKey, RequestHash: hex.EncodeToString(requestHash[:]),
		IdempotencyEnds: now.Add(idempotencyRetention), ExpectedRevision: expectedRevision,
	})
	if err != nil {
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
	case "checkpoint":
		if err := validateSandboxMetadata(metadata); err != nil {
			return contracts.Operation{}, false, err
		}
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
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" ||
		principal.ServiceAccountID == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if generation < 1 {
		return contracts.Lease{}, errors.New("SecondBox Lease generation must be positive")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	policy, _, err := service.store.GetSandboxLifecyclePolicy(ctx, principal.ProjectID, sandboxID)
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
		ProjectID: principal.ProjectID, SandboxID: sandboxID, Generation: generation,
		ServiceAccountID: principal.ServiceAccountID,
		ExpiresAt:        now.Add(time.Duration(durationSeconds) * time.Second), Now: now,
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
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	return service.store.GetLeaseByID(ctx, principal.ProjectID, leaseID)
}

// RenewSandboxLease renews only current, active generation authority.
func (service *ControlPlaneService) RenewSandboxLease(
	ctx context.Context,
	principal contracts.Principal,
	leaseID string,
	idempotencyKey string,
	durationSeconds int64,
) (contracts.Lease, error) {
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" ||
		principal.ServiceAccountID == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := service.store.GetLeaseByID(ctx, principal.ProjectID, leaseID)
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.ServiceAccountID != principal.ServiceAccountID {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	policy, _, err := service.store.GetSandboxLifecyclePolicy(ctx, principal.ProjectID, lease.SandboxID)
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
		Lease: lease, ProjectID: principal.ProjectID, SandboxID: lease.SandboxID,
		Generation: lease.Generation, ServiceAccountID: principal.ServiceAccountID,
		ExpiresAt: now.Add(time.Duration(durationSeconds) * time.Second), Now: now,
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
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" ||
		principal.ServiceAccountID == "" {
		return contracts.Lease{}, ports.ErrAuthorizationDenied
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return contracts.Lease{}, err
	}
	lease, err := service.store.GetLeaseByID(ctx, principal.ProjectID, leaseID)
	if err != nil {
		return contracts.Lease{}, err
	}
	if lease.ServiceAccountID != principal.ServiceAccountID {
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
		Lease: lease, ProjectID: principal.ProjectID, SandboxID: lease.SandboxID,
		Generation: lease.Generation, ServiceAccountID: principal.ServiceAccountID,
		Now: now, IdempotencyKey: idempotencyKey, RequestHash: requestHash,
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
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return contracts.Instance{}, ports.ErrAuthorizationDenied
	}
	return service.store.PingGuest(ctx, ports.GenerationInput{
		ProjectID: principal.ProjectID, SandboxID: sandboxID,
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
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
		return contracts.SandboxInspection{}, ports.ErrAuthorizationDenied
	}
	if generation < 1 {
		return contracts.SandboxInspection{}, errors.New("SecondBox inspection generation must be positive")
	}
	return service.store.ReadSandboxInspection(ctx, ports.GenerationInput{
		ProjectID: principal.ProjectID, SandboxID: sandboxID,
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
	if !principal.HasScope(contracts.ScopeSandboxLifecycle) || principal.ProjectID == "" ||
		principal.ServiceAccountID == "" {
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
			ProjectID: principal.ProjectID, SandboxID: sandboxID,
			Generation: generation, Now: service.now().UTC(),
		},
		LeaseID: leaseID, ServiceAccountID: principal.ServiceAccountID,
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
	if !principal.HasScope(contracts.ScopeSandboxRead) || principal.ProjectID == "" {
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
		sandbox, err := service.store.GetSandbox(ctx, principal.ProjectID, sandboxID)
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
	if principal.ProjectID == "" || principal.ServiceAccountID == "" ||
		!principal.HasScope(scopeForActivityKind(kind)) {
		return contracts.ActivitySession{}, ports.ErrAuthorizationDenied
	}
	now := service.now().UTC()
	return service.store.OpenActivitySession(ctx, ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			ProjectID: principal.ProjectID, SandboxID: sandboxID, Generation: generation, Now: now,
		},
		Session: contracts.ActivitySession{ID: service.newID("act"), Kind: kind},
		LeaseID: leaseID, ServiceAccountID: principal.ServiceAccountID,
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
	if principal.ProjectID == "" || principal.ServiceAccountID == "" {
		return contracts.ActivitySession{}, ports.ErrAuthorizationDenied
	}
	return service.store.CloseActivitySession(ctx, ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			ProjectID: principal.ProjectID, SandboxID: sandboxID,
			Generation: generation, Now: service.now().UTC(),
		},
		Session:          contracts.ActivitySession{ID: sessionID},
		ServiceAccountID: principal.ServiceAccountID,
	})
}

func scopeForActivityKind(kind string) string {
	switch kind {
	case contracts.ActivitySessionKindExec, contracts.ActivitySessionKindPTY:
		return contracts.ScopeSandboxExec
	case contracts.ActivitySessionKindFile:
		return contracts.ScopeSandboxFiles
	case contracts.ActivitySessionKindPort:
		return contracts.ScopeSandboxPorts
	default:
		return ""
	}
}

// Ready proves the database authority is reachable.
func (service *ControlPlaneService) Ready(ctx context.Context) error {
	return service.store.Ping(ctx)
}

// Metrics returns only fixed-cardinality state counts.
func (service *ControlPlaneService) Metrics(ctx context.Context) (contracts.MetricsSnapshot, error) {
	return service.store.ReadMetricsSnapshot(ctx)
}

func (service *ControlPlaneService) issueCredential() (string, string) {
	material := service.newCredentialMaterial()
	prefixDigest := sha256.Sum256([]byte(material))
	prefix := hex.EncodeToString(prefixDigest[:6])
	return "sbx_" + prefix + "_" + material, prefix
}

func (service *ControlPlaneService) hashCredential(credential string) []byte {
	hasher := hmac.New(sha256.New, service.apiKeyHashSecret)
	_, _ = hasher.Write([]byte(credential))
	return hasher.Sum(nil)
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
	return contracts.AuditEvent{
		ID: service.newID("aud"), ProjectID: projectID, ActorKind: principal.Kind,
		ActorID: principal.ID, Action: action, ResourceKind: resourceKind,
		ResourceID: resourceID, Outcome: "accepted", RequestID: service.requestID(ctx),
		Details: map[string]string{}, CreatedAt: now,
	}
}

func requireAdminScope(principal contracts.Principal, scope string) error {
	if !principal.HasScope(scope) {
		return ports.ErrAuthorizationDenied
	}
	return nil
}

func parseAPIKeyPrefix(credential string) (string, error) {
	parts := strings.SplitN(credential, "_", 3)
	if len(parts) != 3 || parts[0] != "sbx" || len(parts[1]) != 12 || parts[2] == "" {
		return "", ports.ErrAuthenticationFailed
	}
	return parts[1], nil
}

func validateServiceAccountAuthority(name string, scopes []string, grants []string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 120 {
		return errors.New("SecondBox ServiceAccount name must contain between 1 and 120 characters")
	}
	if err := validateApplicationScopes(scopes); err != nil {
		return err
	}
	if len(sortedUnique(grants)) > 128 {
		return errors.New("SecondBox ServiceAccount profile grants must not exceed 128 entries")
	}
	for _, grant := range grants {
		if !profileNamePattern.MatchString(grant) {
			return errors.New("SecondBox ServiceAccount profile grant is invalid")
		}
	}
	return nil
}

func validateApplicationScopes(scopes []string) error {
	if len(scopes) == 0 || len(scopes) > 32 {
		return errors.New("SecondBox application scopes must contain between 1 and 32 entries")
	}
	allowed := map[string]bool{
		contracts.ScopeSandboxRead: true, contracts.ScopeSandboxLifecycle: true,
		contracts.ScopeSandboxExec: true, contracts.ScopeSandboxFiles: true,
		contracts.ScopeSandboxArtifacts: true, contracts.ScopeSandboxPorts: true,
	}
	for _, scope := range scopes {
		if !allowed[scope] {
			return fmt.Errorf("SecondBox application scope is unsupported: %s", scope)
		}
	}
	return nil
}

func validateProfileRevisionSpec(spec contracts.ProfileRevisionSpec) error {
	if spec.Backend != "firecracker" {
		return errors.New("SecondBox Profile backend must be firecracker")
	}
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
	if spec.Checkpoint.RetentionSeconds < 1 ||
		spec.Checkpoint.SnapshotLimit < 0 || spec.Checkpoint.ArtifactRetentionSeconds < 1 {
		return errors.New("SecondBox Profile checkpoint limits are invalid")
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
		contracts.SandboxStateCheckpointing,
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
		quota.MaxMemoryBytes, quota.MaxRetainedBytes, quota.MaxSnapshots,
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
