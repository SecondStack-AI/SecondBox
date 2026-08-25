package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const testPlatformToken = "test-platform-token-at-least-24-bytes"

var fixtureCredentialPrincipals sync.Map
var fixtureServiceQuotas sync.Map

func createFixtureProject(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	ctx context.Context,
	admin contracts.Principal,
	request fixtureCreateProjectRequest,
) (fixtureProject, error) {
	t.Helper()
	tenantRef, _ := newSubject(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	project := fixtureProject{
		ID: tenantRef, Name: request.Name, State: fixtureProjectStateActive,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	quota := generousQuota()
	if quotaValue, ok := fixtureServiceQuotas.Load(controlPlane); ok {
		quota = quotaValue.(contracts.QuotaLimits)
	}
	profileSuffix := strings.TrimPrefix(request.Name, "project-")
	_, _, err := controlPlane.CreateTenant(ctx, admin, "fixture-tenant-"+project.ID, contracts.CreateTenantRequest{
		Ref: project.ID,
		AllowedProfileGrants: []string{
			"profile-" + profileSuffix, "coding", "isolated", "quota-profile", "restart-profile", "http-profile",
		},
		AllowedApplicationScopes: []string{
			"sandbox:read", "sandbox:lifecycle", "sandbox:exec",
			"sandbox:files", "sandbox:ports", "sandbox:ports:direct",
		},
		AggregateQuota: tenantQuotaForSubjectQuota(quota),
		ExpiryPolicy: contracts.TenantExpiryPolicy{
			MaximumSubjectLifetimeSeconds:   86400,
			MaximumAuthorityLifetimeSeconds: 86400,
		},
		Metadata: map[string]string{},
	})
	return project, err
}

func createFixtureServiceAccount(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	ctx context.Context,
	_ contracts.Principal,
	projectID string,
	request fixtureCreateServiceAccountRequest,
) (fixtureServiceAccount, error) {
	t.Helper()
	_, subjectRef := newSubject(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	account := fixtureServiceAccount{
		ID: subjectRef, TenantRef: projectID, Name: request.Name,
		State: fixtureServiceAccountStateActive, Scopes: request.Scopes,
		ProfileGrants: request.ProfileGrants, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	quota := generousQuota()
	if quotaValue, ok := fixtureServiceQuotas.Load(controlPlane); ok {
		quota = quotaValue.(contracts.QuotaLimits)
	}
	_, _, err := controlPlane.CreateSubject(ctx, contracts.Principal{
		Kind: contracts.AuthorityKindTenantController, ID: "fixture-controller-" + projectID,
		TenantRef: projectID,
	}, "fixture-subject-"+account.ID, contracts.CreateSubjectRequest{
		Ref: account.ID, Quota: quota, Metadata: map[string]string{},
	})
	return account, err
}

func createFixtureAPIKey(
	t *testing.T,
	_ *service.ControlPlaneService,
	_ context.Context,
	_ contracts.Principal,
	projectID string,
	accountID string,
	request fixtureCreateAPIKeyRequest,
) (fixtureCreateAPIKeyResponse, error) {
	t.Helper()
	sequence := integrationIdentitySequence.Add(1)
	credential := fmt.Sprintf("fixture_%012d_%032d", sequence, sequence)
	response := fixtureCreateAPIKeyResponse{
		APIKey: fixtureAPIKey{
			ID: fmt.Sprintf("key_%d", sequence), SubjectRef: accountID,
			Name: request.Name, Prefix: fmt.Sprintf("%012d", sequence),
			State: fixtureAPIKeyStateActive, Scopes: request.Scopes,
			Revision: 1, CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		},
		Credential: credential,
	}
	fixtureCredentialPrincipals.Store(credential, contracts.Principal{
		Kind: "platform", ID: accountID,
		TenantRef: projectID, SubjectRef: accountID,
	})
	return response, nil
}

func updateFixtureServiceAccount(
	t *testing.T,
	_ *service.ControlPlaneService,
	_ context.Context,
	_ contracts.Principal,
	projectID string,
	accountID string,
	request fixtureUpdateServiceAccountRequest,
) (fixtureServiceAccount, error) {
	t.Helper()
	account := fixtureServiceAccount{
		ID: accountID, TenantRef: projectID, State: fixtureServiceAccountStateActive, Revision: 2,
	}
	if request.Name != nil {
		account.Name = *request.Name
	}
	if request.Scopes != nil {
		account.Scopes = *request.Scopes
	}
	if request.ProfileGrants != nil {
		account.ProfileGrants = *request.ProfileGrants
	}
	return account, nil
}

func newControlPlaneFixture(
	t *testing.T,
	projectQuota contracts.QuotaLimits,
) (*service.ControlPlaneService, *store.PostgresControlPlaneStore) {
	t.Helper()
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	return newControlPlaneService(t, databaseStore, projectQuota), databaseStore
}

func newControlPlaneService(
	t *testing.T,
	databaseStore *store.PostgresControlPlaneStore,
	projectQuota contracts.QuotaLimits,
) *service.ControlPlaneService {
	t.Helper()
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store:                 databaseStore,
		PlatformToken:         testPlatformToken,
		DefaultSubjectQuota:   projectQuota,
		Now:                   func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) },
		NewID:                 newFixtureID,
		NewCredentialMaterial: func() string { return fmt.Sprintf("credential-material-%032d", integrationIdentitySequence.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	fixtureServiceQuotas.Store(controlPlane, projectQuota)
	return controlPlane
}

func newSubject(t *testing.T) (tenantRef, subjectRef string) {
	t.Helper()
	sequence := integrationIdentitySequence.Add(1)
	return fmt.Sprintf("tenant-%d", sequence), fmt.Sprintf("subject-%d", sequence)
}

func fixtureAdmin(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
) contracts.Principal {
	t.Helper()
	return contracts.Principal{
		Kind: "platform", ID: "secondbox-admin",
		TenantRef: "secondbox", SubjectRef: "secondbox-admin",
	}
}

func createProjectAccountAndCredential(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	admin contracts.Principal,
	suffix string,
) (fixtureProject, fixtureServiceAccount, string) {
	t.Helper()
	project, err := createFixtureProject(t, controlPlane,
		t.Context(),
		admin,
		fixtureCreateProjectRequest{Name: "project-" + suffix},
	)
	if err != nil {
		t.Fatal(err)
	}
	account, err := createFixtureServiceAccount(t, controlPlane,
		t.Context(),
		admin,
		project.ID,
		fixtureCreateServiceAccountRequest{
			Name: "service-" + suffix,
			Scopes: []string{
				"sandbox:lifecycle",
				"sandbox:read",
			},
			ProfileGrants: []string{
				"profile-" + suffix,
				"coding",
				"isolated",
				"quota-profile",
				"restart-profile",
				"http-profile",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	createdKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(),
		admin,
		project.ID,
		account.ID,
		fixtureCreateAPIKeyRequest{
			Name: "key-" + suffix,
			Scopes: []string{
				"sandbox:lifecycle",
				"sandbox:read",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixtureCredentialPrincipals.Store(createdKey.Credential, contracts.Principal{
		Kind: "platform", ID: account.ID,
		TenantRef: project.ID, SubjectRef: account.ID,
	})
	return project, account, createdKey.Credential
}

func tenantQuotaForSubjectQuota(quota contracts.QuotaLimits) contracts.TenantQuota {
	return contracts.TenantQuota{
		MaxSandboxes: quota.MaxSandboxes, MaxActiveInstances: quota.MaxActiveInstances,
		MaxCPUMillis: quota.MaxCPUMillis, MaxMemoryBytes: quota.MaxMemoryBytes,
		MaxSnapshots: quota.MaxSnapshots, MaxPortSessions: quota.MaxPortSessions,
		MaxConcurrentOperations: quota.MaxConcurrentOperations,
		MaxActiveSubjects:       100, MaxApplicationAuthorities: 100,
	}
}

func createGrantedProfile(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	databaseStore *store.PostgresControlPlaneStore,
	admin contracts.Principal,
	account fixtureServiceAccount,
	name string,
) contracts.Profile {
	return createGrantedProfileWithDataPlaneTransport(
		t, controlPlane, databaseStore, admin, account, name,
		contracts.DataPlaneTransportProxied,
	)
}

func createGrantedProfileWithDataPlaneTransport(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	databaseStore *store.PostgresControlPlaneStore,
	admin contracts.Principal,
	account fixtureServiceAccount,
	name string,
	transport string,
) contracts.Profile {
	t.Helper()
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
		Name: "default-pool", State: contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"},
		CapacityPolicy: map[string]int64{"maxInstances": 100}, ReadyRunnerCount: 1,
		Revision: 1, CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	seedFixtureHomeRunner(t, "default-pool", "runner-fixture-"+name)
	spec := testProfileSpec(1000)
	spec.Execution.DataPlaneTransport = transport
	profile, err := controlPlane.CreateProfile(
		t.Context(),
		admin,
		contracts.CreateProfileRequest{Name: name, Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	grants := append([]string{}, account.ProfileGrants...)
	if !containsString(grants, name) {
		grants = append(grants, name)
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(),
		admin,
		account.TenantRef,
		account.ID,
		fixtureUpdateServiceAccountRequest{ProfileGrants: &grants},
	); err != nil {
		t.Fatal(err)
	}
	return profile
}

func seedFixtureHomeRunner(t *testing.T, poolName string, runnerID string) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	now := time.Date(2099, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			$1,$2,$1,'ready','["amd64"]',
			'["compute","network-policy","storage","cleanup","local-workspace"]',
			'{"CPUMillis":1000000,"MemoryBytes":1099511627776,"DiskBytes":10995116277760,
			  "Instances":1000,"Operations":1000}',
			'[1]',1,1,'fixture','fixture-connection',1,'active',
			'{"CPUMillis":0,"MemoryBytes":0,"DiskBytes":0,"Instances":0,"Operations":0}',
			'{"artifactDigests":[]}',
			0,0,$3,1,$3,$3
		)
		ON CONFLICT (id) DO UPDATE SET
			state='ready',active_connection_id='fixture-connection',drain_phase='active',
			last_seen_at=EXCLUDED.last_seen_at,updated_at=EXCLUDED.updated_at`,
		runnerID, poolName, now,
	); err != nil {
		t.Fatal(err)
	}
}

func completeFixtureSandboxCreation(t *testing.T, sandboxID string) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var homeRunnerID, workspaceID, operationID string
	if err := pool.QueryRow(t.Context(), `
		SELECT workspace.home_runner_id,workspace.id,operation.id
		FROM secondbox.workspaces AS workspace
		JOIN secondbox.operations AS operation
		  ON operation.sandbox_id=workspace.sandbox_id
		 AND operation.kind='create'
		WHERE workspace.sandbox_id=$1`,
		sandboxID,
	).Scan(&homeRunnerID, &workspaceID, &operationID); err != nil {
		t.Fatal(err)
	}
	stateStore, err := runnercontrol.NewPostgresStateStore(
		t.Context(),
		integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	connectionID := fmt.Sprintf(
		"fixture-workspace-create-%d",
		integrationIdentitySequence.Add(1),
	)
	if err := stateStore.OpenConnection(
		t.Context(),
		runnercontrol.RunnerIdentity{
			RunnerID:         homeRunnerID,
			CredentialSerial: "fixture-workspace-create",
		},
		connectionID,
		1,
		time.Date(2026, 7, 28, 12, 0, 1, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	multirunnerCompleteWorkspaceCreate(
		t,
		stateStore,
		pool,
		homeRunnerID,
		connectionID,
		sandboxID,
		workspaceID,
		operationID,
		1,
		time.Date(2026, 7, 28, 12, 0, 2, 0, time.UTC),
	)
}

func testProfileSpec(cpuMillis int64) contracts.ProfileRevisionSpec {
	return contracts.ProfileRevisionSpec{
		Pool: "default-pool", Architecture: "amd64",
		RuntimeBundleDigest:   "sha256:" + strings.Repeat("a", 64),
		ToolchainBundleDigest: "sha256:" + strings.Repeat("b", 64),
		Resources: contracts.ResourcePolicy{
			CPUMillis: cpuMillis, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30,
			ProcessLimit: 128, ConcurrentOperations: 4,
		},
		Startup: contracts.StartupPolicy{Mode: contracts.StartupModeColdBoot},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState: "stopped", DrainGraceSeconds: 30, IdleSeconds: 300,
			MaximumDurationSeconds: 3600, LeaseSeconds: 60,
		},
		Retention: contracts.RetentionPolicy{
			SnapshotRetentionSeconds: 86400,
			SnapshotLimit:            8,
		},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000, MaximumBufferedOutputBytes: 1 << 20,
			StreamWindowBytes: 65536, MaximumTransferBytes: 1 << 30,
			TerminalDetachSeconds: 30, DataPlaneTransport: contracts.DataPlaneTransportProxied,
		},
		Network: contracts.NetworkPolicy{
			Mode:         "deny_all",
			Destinations: []contracts.NetworkDestination{},
		},
		Ports: []contracts.PortPolicy{{
			Name: "web", Port: 8080, Protocol: "tcp",
			MaximumSessions: 2, MaximumSessionSeconds: 300,
		}},
	}
}

func generousQuota() contracts.QuotaLimits {
	return contracts.QuotaLimits{
		MaxSandboxes: 100, MaxActiveInstances: 100, MaxCPUMillis: 100000,
		MaxMemoryBytes: 100 << 30, MaxSnapshots: 1000, MaxPortSessions: 100,
		MaxConcurrentOperations: 100,
	}
}

func authenticateCredential(
	t *testing.T,
	controlPlane *service.ControlPlaneService,
	credential string,
) contracts.Principal {
	t.Helper()
	if credential == "bootstrap-administrator-secret" || credential == testPlatformToken {
		return contracts.Principal{
			Kind: "platform", ID: "secondbox-admin",
			TenantRef: "secondbox", SubjectRef: "secondbox-admin",
		}
	}
	if principal, ok := fixtureCredentialPrincipals.Load(credential); ok {
		return principal.(contracts.Principal)
	}
	return fixturePrincipalForCredential(t, integrationDatabaseURL, credential)
}

func fixturePrincipalForCredential(
	t *testing.T,
	_ string,
	credential string,
) contracts.Principal {
	t.Helper()
	if principal, ok := fixtureCredentialPrincipals.Load(credential); ok {
		return principal.(contracts.Principal)
	}
	t.Fatalf("unknown fixture credential %q", credential)
	return contracts.Principal{}
}

func setPlatformAuthorization(
	t *testing.T,
	request *http.Request,
	credential string,
) {
	t.Helper()
	setPlatformAuthorizationHeaders(t, request.Header, credential)
}

func setPlatformAuthorizationHeaders(
	t *testing.T,
	headers http.Header,
	credential string,
) {
	t.Helper()
	principal := authenticateCredential(t, nil, credential)
	headers.Set("Authorization", "Bearer "+testPlatformToken)
	headers.Set("X-SecondBox-Tenant-Ref", principal.TenantRef)
	headers.Set("X-SecondBox-Subject-Ref", principal.SubjectRef)
}

func TestFixtureSubjectReferencesAreOpaqueAndUnique(t *testing.T) {
	firstTenant, firstSubject := newSubject(t)
	secondTenant, secondSubject := newSubject(t)
	if firstTenant == secondTenant || firstSubject == secondSubject {
		t.Fatalf(
			"fixture subjects are not unique: first=(%q,%q) second=(%q,%q)",
			firstTenant,
			firstSubject,
			secondTenant,
			secondSubject,
		)
	}
	for _, reference := range []string{
		firstTenant,
		firstSubject,
		secondTenant,
		secondSubject,
	} {
		if len(reference) == 0 || len(reference) > 128 {
			t.Fatalf("fixture reference %q is outside the public bound", reference)
		}
	}
}
