package integration_test

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestDelegatedTenantManagementEndToEndAcrossIsolationRestartAndConcurrency(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}

	tenantARef := secondboxclient.OwnershipRef(newFixtureID("tenant-a"))
	tenantBRef := secondboxclient.OwnershipRef(newFixtureID("tenant-b"))
	tenantRequest := func(ref secondboxclient.OwnershipRef) secondboxclient.CreateTenantRequest {
		expiresAt := now.Add(24 * time.Hour)
		return secondboxclient.CreateTenantRequest{
			Ref: ref, AllowedProfileGrants: []string{"coding"},
			AllowedApplicationScopes: []string{"sandbox:read", "sandbox:lifecycle"},
			AggregateQuota: secondboxclient.TenantQuota{
				MaxSandboxes: 8, MaxActiveInstances: 8, MaxCpuMillis: 8000,
				MaxMemoryBytes: 8 << 30, MaxSnapshots: 8, MaxPortSessions: 8,
				MaxConcurrentOperations: 8, MaxActiveSubjects: 8, MaxApplicationAuthorities: 8,
			},
			ExpiryPolicy: secondboxclient.TenantExpiryPolicy{
				MaximumSubjectLifetimeSeconds: 86400, MaximumAuthorityLifetimeSeconds: 86400,
			}, Metadata: map[string]string{"external/id": string(ref)}, ExpiresAt: &expiresAt,
		}
	}

	const tenantIdempotencyKey = "tenant-a-create-idempotency"
	const contenders = 8
	createdTenants := make(chan secondboxclient.Tenant, contenders)
	failures := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			tenant, createErr := operator.CreateTenant(t.Context(), tenantRequest(tenantARef), tenantIdempotencyKey)
			if createErr != nil {
				failures <- createErr
				return
			}
			createdTenants <- tenant
		}()
	}
	waitGroup.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	close(createdTenants)
	for tenant := range createdTenants {
		if tenant.Ref != tenantARef || tenant.Revision != 1 {
			t.Fatalf("idempotent Tenant response = %#v", tenant)
		}
	}
	if _, err := operator.CreateTenant(t.Context(), tenantRequest(tenantBRef), "tenant-b-create-idempotency"); err != nil {
		t.Fatal(err)
	}

	controllerExpiry := now.Add(12 * time.Hour)
	controllerRequest := secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: controllerExpiry, Metadata: map[string]string{"purpose": "integration"},
	}
	controllerA, err := operator.CreateTenantControllerAuthority(t.Context(), tenantARef, controllerRequest, "controller-a-create-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.CreateTenantControllerAuthority(t.Context(), tenantARef, controllerRequest, "controller-a-create-idempotency")
	assertCredentialResponseUnavailableError(t, err)
	controllerB, err := operator.CreateTenantControllerAuthority(t.Context(), tenantBRef, controllerRequest, "controller-b-create-idempotency")
	if err != nil {
		t.Fatal(err)
	}

	server.Close()
	databaseStore.Close()
	restartedStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedStore.Close)
	restarted := newManagementControlPlane(t, restartedStore, now)
	restartedServer := contractServer(t, persistedHTTPHandler(t, restarted, restartedStore))
	t.Cleanup(restartedServer.Close)
	operator, err = secondboxclient.NewSecondBoxClient(restartedServer.URL, testPlatformToken, restartedServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	controllerAClient, err := secondboxclient.NewSecondBoxTenantControllerClient(restartedServer.URL, controllerA.BearerToken, restartedServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	controllerBClient, err := secondboxclient.NewSecondBoxTenantControllerClient(restartedServer.URL, controllerB.BearerToken, restartedServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := operator.ListTenants(t.Context(), secondboxclient.PageOptions{Limit: 200})
	if err != nil || len(tenants.Items) < 2 {
		t.Fatalf("Tenant list = %#v error=%v", tenants, err)
	}
	controllers, err := operator.ListTenantControllerAuthorities(t.Context(), tenantARef, secondboxclient.PageOptions{})
	if err != nil || len(controllers.Items) != 1 || controllers.Items[0].ID != controllerA.Authority.ID {
		t.Fatalf("controller list = %#v error=%v", controllers, err)
	}

	subjectExpiry := now.Add(6 * time.Hour)
	subjectRequest := secondboxclient.CreateSubjectRequest{
		Ref: "shared-subject", Metadata: map[string]string{"environment": "preview"}, ExpiresAt: &subjectExpiry,
		Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 2, MaxActiveInstances: 2, MaxCpuMillis: 2000,
			MaxMemoryBytes: 2 << 30, MaxSnapshots: 2, MaxPortSessions: 2, MaxConcurrentOperations: 2,
		},
	}
	if _, err := controllerAClient.CreateSubject(t.Context(), subjectRequest, "subject-a-create-idempotency"); err != nil {
		t.Fatal(err)
	}
	if _, err := controllerBClient.CreateSubject(t.Context(), subjectRequest, "subject-b-create-idempotency"); err != nil {
		t.Fatal(err)
	}
	isolatedSubject := subjectRequest
	isolatedSubject.Ref = "tenant-a-only-subject"
	if _, err := controllerAClient.CreateSubject(t.Context(), isolatedSubject, "subject-a-isolated-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := controllerBClient.GetSubject(t.Context(), isolatedSubject.Ref); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeNotFound {
		t.Fatalf("cross-Tenant Subject lookup error = %v", err)
	}
	escalatedSubject := subjectRequest
	escalatedSubject.Ref = "over-ceiling-subject"
	escalatedSubject.Quota.MaxSandboxes = 9
	if _, err := controllerAClient.CreateSubject(t.Context(), escalatedSubject, "subject-over-ceiling-key"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeGrantEscalationDenied {
		t.Fatalf("subject quota escalation error = %v", err)
	}

	applicationExpiry := now.Add(3 * time.Hour)
	applicationRequest := secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: "shared-subject", Scopes: []string{"sandbox:read"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{"purpose": "sandbox"},
		ExpiresAt: applicationExpiry,
	}
	applicationA, err := controllerAClient.CreateApplicationAuthority(t.Context(), applicationRequest, "application-a-create-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = controllerAClient.CreateApplicationAuthority(t.Context(), applicationRequest, "application-a-create-key")
	assertCredentialResponseUnavailableError(t, err)
	applications, err := controllerAClient.ListApplicationAuthorities(t.Context(), "shared-subject", secondboxclient.PageOptions{})
	if err != nil || len(applications.Items) != 1 || applications.Items[0].ID != applicationA.Authority.ID {
		t.Fatalf("application authority list after replay = %#v error=%v", applications, err)
	}
	recoveryRequest := applicationRequest
	recoveryRequest.Metadata = map[string]string{"purpose": "uncertain-response"}
	uncertainApplication, err := controllerAClient.CreateApplicationAuthority(t.Context(), recoveryRequest, "application-uncertain-create-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = controllerAClient.CreateApplicationAuthority(t.Context(), recoveryRequest, "application-uncertain-create-key")
	assertCredentialResponseUnavailableError(t, err)
	applications, err = controllerAClient.ListApplicationAuthorities(t.Context(), "shared-subject", secondboxclient.PageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var discoveredUncertain secondboxclient.ApplicationAuthority
	for _, authority := range applications.Items {
		if authority.Metadata["purpose"] == "uncertain-response" {
			discoveredUncertain = authority
		}
	}
	if discoveredUncertain.ID == "" || discoveredUncertain.ID != uncertainApplication.Authority.ID {
		t.Fatalf("uncertain application authority discovery = %#v", discoveredUncertain)
	}
	if _, err := controllerAClient.RevokeApplicationAuthority(t.Context(), discoveredUncertain.ID, discoveredUncertain.Revision, "application-uncertain-revoke-key"); err != nil {
		t.Fatal(err)
	}
	replacementRequest := applicationRequest
	replacementRequest.Metadata = map[string]string{"purpose": "uncertain-response-replacement"}
	replacementApplication, err := controllerAClient.CreateApplicationAuthority(t.Context(), replacementRequest, "application-uncertain-replacement-key")
	if err != nil {
		t.Fatal(err)
	}
	if replacementApplication.Authority.ID == discoveredUncertain.ID || replacementApplication.BearerToken == uncertainApplication.BearerToken {
		t.Fatal("revoke-and-replace returned the uncertain application credential")
	}
	assertApplicationAuthenticationStatus(t, restartedServer.URL+"/v1/sandboxes", replacementApplication.BearerToken, string(tenantARef), "shared-subject", http.StatusOK)
	if _, err := controllerBClient.GetApplicationAuthority(t.Context(), applicationA.Authority.ID); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeNotFound {
		t.Fatalf("cross-Tenant authority lookup error = %v", err)
	}
	if _, err := controllerBClient.RotateApplicationAuthority(t.Context(), applicationA.Authority.ID, applicationA.Authority.Revision, "cross-tenant-rotate-key"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeNotFound {
		t.Fatalf("cross-Tenant authority mutation error = %v", err)
	}
	escalatedAuthority := applicationRequest
	escalatedAuthority.Scopes = []string{"sandbox:exec"}
	if _, err := controllerAClient.CreateApplicationAuthority(t.Context(), escalatedAuthority, "application-escalation-key"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeGrantEscalationDenied {
		t.Fatalf("application scope escalation error = %v", err)
	}
	unboundAuthority := applicationRequest
	unboundAuthority.SubjectRef = "absent-subject"
	if _, err := controllerAClient.CreateApplicationAuthority(t.Context(), unboundAuthority, "application-unbound-key"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeGrantEscalationDenied {
		t.Fatalf("application Subject binding error = %v", err)
	}

	assertControllerCannotCallOrdinaryRoute(t, restartedServer.URL+"/v1/profiles", controllerA.BearerToken, "", "")
	assertControllerCannotCallOrdinaryRoute(t, restartedServer.URL+"/v1/runner-pools", controllerA.BearerToken, "", "")
	assertControllerCannotCallOrdinaryRoute(t, restartedServer.URL+"/v1/sandboxes", controllerA.BearerToken, string(tenantBRef), "shared-subject")

	type applicationRotationOutcome struct {
		response secondboxclient.ApplicationCredentialResponse
		err      error
	}
	rotations := make(chan applicationRotationOutcome, contenders)
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			rotated, rotateErr := controllerAClient.RotateApplicationAuthority(t.Context(), applicationA.Authority.ID, applicationA.Authority.Revision, "application-a-rotate-key")
			rotations <- applicationRotationOutcome{response: rotated, err: rotateErr}
		}()
	}
	waitGroup.Wait()
	close(rotations)
	var rotatedApplication secondboxclient.ApplicationCredentialResponse
	rotationSuccesses, unavailableRotations := 0, 0
	for outcome := range rotations {
		switch code := secondboxclient.ProblemCodeOf(outcome.err); code {
		case "":
			rotationSuccesses++
			rotatedApplication = outcome.response
		case secondboxclient.ProblemCodeCredentialResponseUnavailable:
			assertCredentialResponseUnavailableError(t, outcome.err)
			unavailableRotations++
		default:
			t.Fatalf("concurrent application rotation error = %v", outcome.err)
		}
	}
	if rotationSuccesses != 1 || unavailableRotations != contenders-1 {
		t.Fatalf("concurrent application rotations succeeded=%d unavailable=%d", rotationSuccesses, unavailableRotations)
	}
	storedRotatedApplication, err := controllerAClient.GetApplicationAuthority(t.Context(), applicationA.Authority.ID)
	if err != nil || storedRotatedApplication.Revision != applicationA.Authority.Revision+1 || storedRotatedApplication.LookupID != rotatedApplication.Authority.LookupID {
		t.Fatalf("stored application rotation = %#v error=%v", storedRotatedApplication, err)
	}
	revocations := make(chan secondboxclient.ApplicationAuthority, contenders)
	failures = make(chan error, contenders)
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			revoked, revokeErr := controllerAClient.RevokeApplicationAuthority(t.Context(), applicationA.Authority.ID, rotatedApplication.Authority.Revision, "application-a-revoke-key")
			if revokeErr != nil {
				failures <- revokeErr
				return
			}
			revocations <- revoked
		}()
	}
	waitGroup.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	close(revocations)
	for revoked := range revocations {
		if revoked.State != secondboxclient.AuthorityStateRevoked || revoked.Revision != rotatedApplication.Authority.Revision+1 {
			t.Fatalf("concurrent application revocation = %#v", revoked)
		}
	}
	if _, err := controllerAClient.RotateApplicationAuthority(t.Context(), applicationA.Authority.ID, rotatedApplication.Authority.Revision, "stale-application-rotate-key"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed {
		t.Fatalf("stale application revision error = %v", err)
	}
	activeApplication, err := controllerAClient.CreateApplicationAuthority(t.Context(), applicationRequest, "application-a-suspension-key")
	if err != nil {
		t.Fatal(err)
	}
	assertApplicationAuthenticationStatus(t, restartedServer.URL+"/v1/sandboxes", activeApplication.BearerToken, string(tenantARef), "shared-subject", http.StatusOK)

	tenantA, err := operator.GetTenant(t.Context(), tenantARef)
	if err != nil {
		t.Fatal(err)
	}
	suspended, err := operator.SuspendTenant(t.Context(), tenantARef, tenantA.Revision, "tenant-a-suspend-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerAClient.ListSubjects(t.Context(), secondboxclient.PageOptions{}); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeAuthenticationFailed {
		t.Fatalf("suspended controller admission error = %v", err)
	}
	assertApplicationAuthenticationStatus(t, restartedServer.URL+"/v1/sandboxes", activeApplication.BearerToken, string(tenantARef), "shared-subject", http.StatusUnauthorized)
	if _, err := controllerBClient.ListSubjects(t.Context(), secondboxclient.PageOptions{}); err != nil {
		t.Fatalf("other Tenant admission after suspension: %v", err)
	}
	if _, err := operator.ReactivateTenant(t.Context(), tenantARef, suspended.Revision, "tenant-a-reactivate-key"); err != nil {
		t.Fatal(err)
	}

	currentController, err := operator.GetTenantControllerAuthority(t.Context(), tenantARef, controllerA.Authority.ID)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := make(chan string, 2)
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		_, rotateErr := operator.RotateTenantControllerAuthority(t.Context(), tenantARef, currentController.ID, currentController.Revision, "controller-race-rotate-key")
		conflicts <- secondboxclient.ProblemCodeOf(rotateErr)
	}()
	go func() {
		defer waitGroup.Done()
		_, revokeErr := operator.RevokeTenantControllerAuthority(t.Context(), tenantARef, currentController.ID, currentController.Revision, "controller-race-revoke-key")
		conflicts <- secondboxclient.ProblemCodeOf(revokeErr)
	}()
	waitGroup.Wait()
	close(conflicts)
	successes, revisionConflicts := 0, 0
	for code := range conflicts {
		if code == "" {
			successes++
		} else if code == secondboxclient.ProblemCodePreconditionFailed {
			revisionConflicts++
		} else {
			t.Fatalf("controller lifecycle race code = %q", code)
		}
	}
	if successes != 1 || revisionConflicts != 1 {
		t.Fatalf("controller lifecycle race successes=%d revision conflicts=%d", successes, revisionConflicts)
	}

	type controllerCreationOutcome struct {
		response secondboxclient.TenantControllerCredentialResponse
		err      error
	}
	createdControllers := make(chan controllerCreationOutcome, contenders)
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			created, createErr := operator.CreateTenantControllerAuthority(t.Context(), tenantBRef, controllerRequest, "controller-b-concurrent-create-key")
			createdControllers <- controllerCreationOutcome{response: created, err: createErr}
		}()
	}
	waitGroup.Wait()
	close(createdControllers)
	var lifecycleController secondboxclient.TenantControllerCredentialResponse
	controllerCreationSuccesses, unavailableControllerCreations := 0, 0
	for outcome := range createdControllers {
		switch code := secondboxclient.ProblemCodeOf(outcome.err); code {
		case "":
			controllerCreationSuccesses++
			lifecycleController = outcome.response
		case secondboxclient.ProblemCodeCredentialResponseUnavailable:
			assertCredentialResponseUnavailableError(t, outcome.err)
			unavailableControllerCreations++
		default:
			t.Fatalf("concurrent controller credential creation error = %v", outcome.err)
		}
	}
	if controllerCreationSuccesses != 1 || unavailableControllerCreations != contenders-1 {
		t.Fatalf("concurrent controller creations succeeded=%d unavailable=%d", controllerCreationSuccesses, unavailableControllerCreations)
	}
	tenantBControllers, err := operator.ListTenantControllerAuthorities(t.Context(), tenantBRef, secondboxclient.PageOptions{})
	if err != nil || len(tenantBControllers.Items) != 2 {
		t.Fatalf("controller list after concurrent creation = %#v error=%v", tenantBControllers, err)
	}
	rotatedController, err := operator.RotateTenantControllerAuthority(t.Context(), tenantBRef, lifecycleController.Authority.ID, lifecycleController.Authority.Revision, "controller-b-rotate-replay-key")
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.RotateTenantControllerAuthority(t.Context(), tenantBRef, lifecycleController.Authority.ID, lifecycleController.Authority.Revision, "controller-b-rotate-replay-key")
	assertCredentialResponseUnavailableError(t, err)
	storedRotatedController, err := operator.GetTenantControllerAuthority(t.Context(), tenantBRef, lifecycleController.Authority.ID)
	if err != nil || storedRotatedController.Revision != lifecycleController.Authority.Revision+1 || storedRotatedController.LookupID != rotatedController.Authority.LookupID {
		t.Fatalf("stored controller rotation = %#v error=%v", storedRotatedController, err)
	}
	revokedController, err := operator.RevokeTenantControllerAuthority(t.Context(), tenantBRef, lifecycleController.Authority.ID, rotatedController.Authority.Revision, "controller-b-revoke-replay-key")
	if err != nil || revokedController.State != secondboxclient.AuthorityStateRevoked {
		t.Fatalf("controller revocation = %#v error=%v", revokedController, err)
	}
	revokedControllerReplay, err := operator.RevokeTenantControllerAuthority(t.Context(), tenantBRef, lifecycleController.Authority.ID, rotatedController.Authority.Revision, "controller-b-revoke-replay-key")
	if err != nil || revokedControllerReplay.Revision != revokedController.Revision {
		t.Fatalf("controller revocation replay = %#v error=%v", revokedControllerReplay, err)
	}

	auditPool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer auditPool.Close()
	var secretIdempotencyRecords int
	if err := auditPool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.idempotency_records
		WHERE response_json::text LIKE '%secondbox_tenant_controller_%'
		   OR response_json::text LIKE '%secondbox_application_%'`).Scan(&secretIdempotencyRecords); err != nil {
		t.Fatal(err)
	}
	if secretIdempotencyRecords != 0 {
		t.Fatalf("idempotency records containing bearer-token prefixes = %d", secretIdempotencyRecords)
	}
	var accepted, denied int
	if err := auditPool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE outcome='accepted'),
		count(*) FILTER (WHERE outcome='denied')
		FROM secondbox.audit_events WHERE tenant_ref=$1`, tenantARef).Scan(&accepted, &denied); err != nil {
		t.Fatal(err)
	}
	if accepted == 0 || denied == 0 {
		t.Fatalf("management audit outcomes accepted=%d denied=%d", accepted, denied)
	}
}

func newManagementControlPlane(t *testing.T, databaseStore *store.PostgresControlPlaneStore, now time.Time) *service.ControlPlaneService {
	t.Helper()
	controlPlane, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: contracts.QuotaLimits{
			MaxSandboxes: 1, MaxActiveInstances: 1, MaxCPUMillis: 1000,
			MaxMemoryBytes: 1 << 30, MaxSnapshots: 1, MaxPortSessions: 1, MaxConcurrentOperations: 1,
		},
		Now: func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: func() string { return "management-test-credential-material-000000000000" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return controlPlane
}

func assertControllerCannotCallOrdinaryRoute(t *testing.T, url, token, tenantRef, subjectRef string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if tenantRef != "" {
		request.Header.Set("X-SecondBox-Tenant-Ref", tenantRef)
	}
	if subjectRef != "" {
		request.Header.Set("X-SecondBox-Subject-Ref", subjectRef)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("controller ordinary route status=%d body=%s", response.StatusCode, body)
	}
}

func assertApplicationAuthenticationStatus(t *testing.T, url, token, tenantRef, subjectRef string, want int) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-SecondBox-Tenant-Ref", tenantRef)
	request.Header.Set("X-SecondBox-Subject-Ref", subjectRef)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("application admission status=%d want=%d body=%s", response.StatusCode, want, body)
	}
}

func assertCredentialResponseUnavailableError(t *testing.T, err error) {
	t.Helper()
	var failure *secondboxclient.APIError
	if !errors.As(err, &failure) || failure.StatusCode != http.StatusConflict ||
		secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeCredentialResponseUnavailable {
		t.Fatalf("credential replay error = %v", err)
	}
}
