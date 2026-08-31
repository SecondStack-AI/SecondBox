package contracts

import "time"

const (
	AuthorityKindPlatform         = "platform"
	AuthorityKindTenantController = "tenant_controller"
	AuthorityKindApplication      = "application"

	AuthorityStateActive  = "active"
	AuthorityStateExpired = "expired"
	AuthorityStateRevoked = "revoked"

	TenantStateActive    = "active"
	TenantStateSuspended = "suspended"
	TenantStateExpired   = "expired"

	SubjectStateActive  = "active"
	SubjectStateClosing = "closing"
	SubjectStateClosed  = "closed"
	SubjectStateExpired = "expired"

	SubjectCleanupStateNone      = "none"
	SubjectCleanupStatePending   = "pending"
	SubjectCleanupStateRunning   = "running"
	SubjectCleanupStateSucceeded = "succeeded"
	SubjectCleanupStateFailed    = "failed"

	TenantControllerGrantManagement = "tenant_management"
)

// TenantExpiryPolicy bounds tenant-local subject and authority lifetimes.
type TenantExpiryPolicy struct {
	MaximumSubjectLifetimeSeconds   int64 `json:"maximumSubjectLifetimeSeconds"`
	MaximumAuthorityLifetimeSeconds int64 `json:"maximumAuthorityLifetimeSeconds"`
}

// TenantQuota bounds aggregate tenant reservations and management resources.
type TenantQuota struct {
	MaxSandboxes              int64 `json:"maxSandboxes"`
	MaxActiveInstances        int64 `json:"maxActiveInstances"`
	MaxVCPUCount              int64 `json:"maxVcpuCount"`
	MaxMemoryBytes            int64 `json:"maxMemoryBytes"`
	MaxSnapshots              int64 `json:"maxSnapshots"`
	MaxPortSessions           int64 `json:"maxPortSessions"`
	MaxConcurrentOperations   int64 `json:"maxConcurrentOperations"`
	MaxActiveSubjects         int64 `json:"maxActiveSubjects"`
	MaxApplicationAuthorities int64 `json:"maxApplicationAuthorities"`
}

// TenantQuotaUsage projects one tenant's aggregate persisted reservations.
type TenantQuotaUsage struct {
	Sandboxes              int64 `json:"sandboxes"`
	ActiveInstances        int64 `json:"activeInstances"`
	VCPUCount              int64 `json:"vcpuCount"`
	MemoryBytes            int64 `json:"memoryBytes"`
	Snapshots              int64 `json:"snapshots"`
	PortSessions           int64 `json:"portSessions"`
	ConcurrentOperations   int64 `json:"concurrentOperations"`
	ActiveSubjects         int64 `json:"activeSubjects"`
	ApplicationAuthorities int64 `json:"applicationAuthorities"`
}

// TenantUsage reports aggregate and per-Subject reservations for one tenant.
type TenantUsage struct {
	TenantRef  string           `json:"tenantRef"`
	Limits     TenantQuota      `json:"limits"`
	Usage      TenantQuotaUsage `json:"usage"`
	Subjects   []SubjectUsage   `json:"subjects"`
	NextCursor *string          `json:"nextCursor,omitempty"`
	ObservedAt time.Time        `json:"observedAt"`
}

// TenantAggregateUsage reports aggregate reservations for one tenant.
type TenantAggregateUsage struct {
	TenantRef string           `json:"tenantRef"`
	Limits    TenantQuota      `json:"limits"`
	Usage     TenantQuotaUsage `json:"usage"`
}

// DeploymentUsage reports deployment-wide reservations and one Tenant page.
type DeploymentUsage struct {
	Usage      TenantQuotaUsage       `json:"usage"`
	Tenants    []TenantAggregateUsage `json:"tenants"`
	NextCursor *string                `json:"nextCursor,omitempty"`
	ObservedAt time.Time              `json:"observedAt"`
}

// Tenant is one stable management and aggregate-admission boundary.
type Tenant struct {
	Ref                      string             `json:"ref"`
	State                    string             `json:"state"`
	EgressContext            *string            `json:"egressContext"`
	AllowedProfileGrants     []string           `json:"allowedProfileGrants"`
	AllowedApplicationScopes []string           `json:"allowedApplicationScopes"`
	AggregateQuota           TenantQuota        `json:"aggregateQuota"`
	ExpiryPolicy             TenantExpiryPolicy `json:"expiryPolicy"`
	Metadata                 map[string]string  `json:"metadata"`
	ExpiresAt                *time.Time         `json:"expiresAt,omitempty"`
	Revision                 int64              `json:"revision"`
	CreatedAt                time.Time          `json:"createdAt"`
	UpdatedAt                time.Time          `json:"updatedAt"`
}

// Subject is one tenant-scoped application ownership identity.
type Subject struct {
	TenantRef    string            `json:"tenantRef"`
	Ref          string            `json:"ref"`
	State        string            `json:"state"`
	CleanupState string            `json:"cleanupState"`
	Quota        QuotaLimits       `json:"quota"`
	Metadata     map[string]string `json:"metadata"`
	ExpiresAt    *time.Time        `json:"expiresAt,omitempty"`
	Revision     int64             `json:"revision"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

// TenantControllerAuthority is the non-secret projection of one tenant controller.
type TenantControllerAuthority struct {
	ID        string            `json:"id"`
	LookupID  string            `json:"lookupId"`
	Kind      string            `json:"kind"`
	TenantRef string            `json:"tenantRef"`
	Grant     string            `json:"grant"`
	State     string            `json:"state"`
	Metadata  map[string]string `json:"metadata"`
	ExpiresAt *time.Time        `json:"expiresAt,omitempty"`
	Revision  int64             `json:"revision"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// TenantControllerCredentialResponse carries bearer material only on successful creation or rotation.
type TenantControllerCredentialResponse struct {
	Authority   TenantControllerAuthority `json:"authority"`
	BearerToken string                    `json:"bearerToken"`
}

// ApplicationAuthority is the non-secret projection of one application credential.
type ApplicationAuthority struct {
	ID            string            `json:"id"`
	LookupID      string            `json:"lookupId"`
	Kind          string            `json:"kind"`
	TenantRef     string            `json:"tenantRef"`
	SubjectRef    string            `json:"subjectRef"`
	State         string            `json:"state"`
	Scopes        []string          `json:"scopes"`
	ProfileGrants []string          `json:"profileGrants"`
	Metadata      map[string]string `json:"metadata"`
	ExpiresAt     *time.Time        `json:"expiresAt,omitempty"`
	Revision      int64             `json:"revision"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

// ApplicationCredentialResponse carries bearer material only on successful creation or rotation.
type ApplicationCredentialResponse struct {
	Authority   ApplicationAuthority `json:"authority"`
	BearerToken string               `json:"bearerToken"`
}

// CreateTenantRequest supplies the complete operator-owned Tenant boundary.
type CreateTenantRequest struct {
	Ref                      string             `json:"ref"`
	EgressContext            *string            `json:"egressContext,omitempty"`
	AllowedProfileGrants     []string           `json:"allowedProfileGrants"`
	AllowedApplicationScopes []string           `json:"allowedApplicationScopes"`
	AggregateQuota           TenantQuota        `json:"aggregateQuota"`
	ExpiryPolicy             TenantExpiryPolicy `json:"expiryPolicy"`
	Metadata                 map[string]string  `json:"metadata"`
	ExpiresAt                *time.Time         `json:"expiresAt,omitempty"`
}

// UpdateTenantEgressContextRequest replaces or clears the operator-owned routing context.
type UpdateTenantEgressContextRequest struct {
	EgressContext *string `json:"egressContext"`
}

// CreateSubjectRequest supplies one tenant-scoped subject and its quota.
type CreateSubjectRequest struct {
	Ref       string            `json:"ref"`
	Quota     QuotaLimits       `json:"quota"`
	Metadata  map[string]string `json:"metadata"`
	ExpiresAt *time.Time        `json:"expiresAt,omitempty"`
}

// UpdateSubjectQuotaRequest replaces one Subject's complete quota set.
type UpdateSubjectQuotaRequest struct {
	Quota QuotaLimits `json:"quota"`
}

// CreateTenantControllerAuthorityRequest supplies bounded non-secret controller attributes.
type CreateTenantControllerAuthorityRequest struct {
	Metadata  map[string]string `json:"metadata"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

// CreateApplicationAuthorityRequest supplies a subject-bound application grant.
type CreateApplicationAuthorityRequest struct {
	SubjectRef    string            `json:"subjectRef"`
	Scopes        []string          `json:"scopes"`
	ProfileGrants []string          `json:"profileGrants"`
	Metadata      map[string]string `json:"metadata"`
	ExpiresAt     time.Time         `json:"expiresAt"`
}

// TenantPage is one bounded stable Tenant traversal page.
type TenantPage struct {
	Items      []Tenant `json:"items"`
	NextCursor *string  `json:"nextCursor,omitempty"`
}

// SubjectPage is one bounded stable tenant-scoped Subject traversal page.
type SubjectPage struct {
	Items      []Subject `json:"items"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// TenantControllerAuthorityPage is one bounded stable controller traversal page.
type TenantControllerAuthorityPage struct {
	Items      []TenantControllerAuthority `json:"items"`
	NextCursor *string                     `json:"nextCursor,omitempty"`
}

// ApplicationAuthorityPage is one bounded stable application-authority traversal page.
type ApplicationAuthorityPage struct {
	Items      []ApplicationAuthority `json:"items"`
	NextCursor *string                `json:"nextCursor,omitempty"`
}
