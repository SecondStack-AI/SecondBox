package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestSubjectCloseAndCleanupHTTPAreImmediateIdempotentAndTenantIsolated(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	tenantA := secondboxclient.OwnershipRef(newFixtureID("cleanup-http-tenant-a"))
	tenantB := secondboxclient.OwnershipRef(newFixtureID("cleanup-http-tenant-b"))
	tenantRequest := func(ref secondboxclient.OwnershipRef) secondboxclient.CreateTenantRequest {
		return secondboxclient.CreateTenantRequest{
			Ref: ref, AllowedProfileGrants: []string{"coding"},
			AllowedApplicationScopes: []string{"sandbox:read", "sandbox:lifecycle"},
			AggregateQuota: secondboxclient.TenantQuota{
				MaxSandboxes: 4, MaxActiveInstances: 4, MaxCpuMillis: 4000,
				MaxMemoryBytes: 4 << 30, MaxSnapshots: 4, MaxPortSessions: 4,
				MaxConcurrentOperations: 4, MaxActiveSubjects: 4, MaxApplicationAuthorities: 4,
			},
			ExpiryPolicy: secondboxclient.TenantExpiryPolicy{
				MaximumSubjectLifetimeSeconds: 3600, MaximumAuthorityLifetimeSeconds: 3600,
			}, Metadata: map[string]string{},
		}
	}
	for _, tenantRef := range []secondboxclient.OwnershipRef{tenantA, tenantB} {
		if _, err := operator.CreateTenant(t.Context(), tenantRequest(tenantRef), "create-"+string(tenantRef)); err != nil {
			t.Fatal(err)
		}
	}
	controllerRequest := secondboxclient.CreateTenantControllerAuthorityRequest{
		Metadata: map[string]string{}, ExpiresAt: now.Add(time.Hour),
	}
	controllerA, err := operator.CreateTenantControllerAuthority(t.Context(), tenantA, controllerRequest, "controller-a")
	if err != nil {
		t.Fatal(err)
	}
	controllerB, err := operator.CreateTenantControllerAuthority(t.Context(), tenantB, controllerRequest, "controller-b")
	if err != nil {
		t.Fatal(err)
	}
	clientA, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controllerA.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	clientB, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controllerB.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	subjectRequest := secondboxclient.CreateSubjectRequest{
		Ref: "preview", Metadata: map[string]string{},
		Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 2, MaxActiveInstances: 2, MaxCpuMillis: 2000,
			MaxMemoryBytes: 2 << 30, MaxSnapshots: 2, MaxPortSessions: 2,
			MaxConcurrentOperations: 2,
		},
	}
	subjectA, err := clientA.CreateSubject(t.Context(), subjectRequest, "subject-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientB.CreateSubject(t.Context(), subjectRequest, "subject-b"); err != nil {
		t.Fatal(err)
	}
	authorityRequest := secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: subjectA.Ref, Scopes: []string{"sandbox:read", "sandbox:lifecycle"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{}, ExpiresAt: now.Add(30 * time.Minute),
	}
	application, err := clientA.CreateApplicationAuthority(t.Context(), authorityRequest, "application-a")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := clientA.CloseSubject(t.Context(), subjectA.Ref, subjectA.Revision, "close-first")
	if err != nil || closed.State != secondboxclient.SubjectStateClosed {
		t.Fatalf("closed Subject = %#v error=%v", closed, err)
	}
	repeated, err := clientA.CloseSubject(t.Context(), subjectA.Ref, subjectA.Revision, "close-second")
	if err != nil || repeated.Revision != closed.Revision {
		t.Fatalf("repeated close = %#v error=%v", repeated, err)
	}
	assertApplicationAuthenticationStatus(
		t, server.URL+"/v1/sandboxes", application.BearerToken,
		string(tenantA), string(subjectA.Ref), http.StatusUnauthorized,
	)
	if _, err := clientA.CreateApplicationAuthority(t.Context(), authorityRequest, "application-after-close"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeGrantEscalationDenied {
		t.Fatalf("authority creation after close error = %v", err)
	}
	cleanup, err := clientA.CleanupSubject(t.Context(), subjectA.Ref, closed.Revision, "cleanup-first")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := clientA.CleanupSubject(t.Context(), subjectA.Ref, closed.Revision, "cleanup-second")
	if err != nil || replayed.ID != cleanup.ID {
		t.Fatalf("cleanup identity replay = %#v error=%v", replayed, err)
	}
	var inspected secondboxclient.Operation
	if err := clientA.RequestJSON(t.Context(), "getOperation", secondboxclient.CallOptions{
		PathParameters: map[string]string{"operationId": cleanup.ID},
	}, &inspected); err != nil || inspected.ID != cleanup.ID {
		t.Fatalf("controller cleanup inspection = %#v error=%v", inspected, err)
	}
	otherSubject := subjectRequest
	otherSubject.Ref = "still-usable"
	if _, err := clientB.CreateSubject(t.Context(), otherSubject, "subject-b-after-cleanup"); err != nil {
		t.Fatalf("other tenant admission during cleanup: %v", err)
	}
}
