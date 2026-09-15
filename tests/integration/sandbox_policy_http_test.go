package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestSandboxPolicyHTTPPinsFutureLifecycleAndDelegatedBounds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("policy-http-%d", integrationIdentitySequence.Add(1))
	if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{Name: name, State: contracts.RunnerPoolStateReady, Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"}, ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seedFixtureHomeRunner(t, name, name)
	spec := testProfileSpec(1)
	spec.Pool = name
	spec.Lifecycle.MaximumDurationSeconds = contracts.Unlimited
	spec.LifecycleCeiling = &contracts.SandboxLifecycleLimits{IdleSeconds: 600, MaximumDurationSeconds: contracts.Unlimited}
	if _, _, err := controlPlane.CreateProfileIdempotent(t.Context(), contracts.Principal{Kind: "platform", ID: "operator"}, name, contracts.CreateProfileRequest{Name: name, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	tenantRequest := persistedHTTPTenantRequest(name)
	tenantRequest.AllowedProfileGrants = []string{name}
	tenant, err := operator.CreateTenant(t.Context(), tenantRequest, name)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := operator.CreateTenantControllerAuthority(t.Context(), name, secondboxclient.CreateTenantControllerAuthorityRequest{ExpiresAt: now.Add(time.Hour), Metadata: map[string]string{}}, name)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, credential.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	quota := secondboxclient.SubjectQuota{MaxSandboxes: secondboxclient.Unlimited, MaxActiveInstances: secondboxclient.Unlimited, MaxVcpuCount: secondboxclient.Unlimited, MaxMemoryBytes: secondboxclient.Unlimited, MaxSnapshots: 0, MaxPortSessions: secondboxclient.Unlimited, MaxConcurrentOperations: secondboxclient.Unlimited}
	if _, err := controller.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{Ref: name, Quota: quota, Metadata: map[string]string{}}, name); err != nil {
		t.Fatal(err)
	}
	app, err := controller.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{SubjectRef: name, ProfileGrants: []string{name}, Scopes: []string{"sandbox:read", "sandbox:lifecycle"}, ExpiresAt: now.Add(time.Hour), Metadata: map[string]string{}}, name)
	if err != nil {
		t.Fatal(err)
	}
	application, err := secondboxclient.NewSecondBoxSubjectClient(server.URL, app.BearerToken, name, name, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	initial, err := controller.GetSubjectSandboxPolicy(t.Context(), name, name)
	if err != nil || initial.Desired != nil || initial.Effective.MaximumDurationSeconds != contracts.Unlimited {
		t.Fatalf("initial policy = %+v, %v", initial, err)
	}
	capacity, err := application.GetSubjectCapacity(t.Context())
	if err != nil || capacity.Available.Sandboxes != 10 || capacity.ConstrainingScopes.Sandboxes != "tenant" || capacity.Limits.MaxSandboxes != secondboxclient.Unlimited {
		t.Fatalf("finite parent = %+v, %v", capacity, err)
	}
	tenant.AggregateQuota = secondboxclient.TenantQuota{MaxSandboxes: secondboxclient.Unlimited, MaxActiveInstances: secondboxclient.Unlimited, MaxVcpuCount: secondboxclient.Unlimited, MaxMemoryBytes: secondboxclient.Unlimited, MaxSnapshots: secondboxclient.Unlimited, MaxPortSessions: secondboxclient.Unlimited, MaxConcurrentOperations: secondboxclient.Unlimited, MaxActiveSubjects: secondboxclient.Unlimited, MaxApplicationAuthorities: secondboxclient.Unlimited}
	if _, err := operator.UpdateTenantQuota(t.Context(), name, secondboxclient.UpdateTenantQuotaRequest{AggregateQuota: tenant.AggregateQuota}, tenant.Revision, name+"-unlimited"); err != nil {
		t.Fatal(err)
	}
	capacity, err = application.GetSubjectCapacity(t.Context())
	if err != nil || capacity.Available.Sandboxes != secondboxclient.Unlimited || capacity.ConstrainingScopes.Sandboxes != "none" || capacity.Available.Snapshots != 0 {
		t.Fatalf("unlimited parent = %+v, %v", capacity, err)
	}
	selection := secondboxclient.SubjectSandboxPolicy{Profile: name, Lifecycle: secondboxclient.SandboxLifecycleLimits{IdleSeconds: 60, MaximumDurationSeconds: secondboxclient.Unlimited}}
	selected, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-policy")
	if err != nil || selected.Effective.IdleSeconds != 60 {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
	replay, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-policy")
	if err != nil || replay.Revision != selected.Revision {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-stale"); secondboxclient.ProblemCodeOf(err) != "precondition_failed" {
		t.Fatalf("stale policy = %v", err)
	}
	first, _, err := application.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{Image: testExecutionImage(), Profile: name, Metadata: map[string]string{}}, name+"-first")
	if err != nil {
		t.Fatal(err)
	}
	selection.Lifecycle = secondboxclient.SandboxLifecycleLimits{IdleSeconds: secondboxclient.Unlimited, MaximumDurationSeconds: 120}
	selected, err = controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, name+"-finite")
	if err != nil || selected.Effective.IdleSeconds != 600 || selected.Effective.MaximumDurationSeconds != 120 {
		t.Fatalf("clamped policy = %+v, %v", selected, err)
	}
	second, _, err := application.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{Image: testExecutionImage(), Profile: name, Metadata: map[string]string{}}, name+"-second")
	if err != nil {
		t.Fatal(err)
	}
	firstSandbox, err := first.Refresh(t.Context())
	if err != nil || firstSandbox.Lifecycle.IdleSeconds != 60 || firstSandbox.Lifecycle.MaximumDurationSeconds != secondboxclient.Unlimited {
		t.Fatalf("first policy changed = %+v, %v", firstSandbox, err)
	}
	secondSandbox, err := second.Refresh(t.Context())
	if err != nil || secondSandbox.Lifecycle.IdleSeconds != 600 || secondSandbox.Lifecycle.MaximumDurationSeconds != 120 {
		t.Fatalf("second policy = %+v, %v", secondSandbox, err)
	}
	selection.Lifecycle.IdleSeconds = 601
	if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, name+"-denied"); err == nil {
		t.Fatal("accepted policy above operator ceiling")
	}
	response := applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-policy?profile="+name, app.BearerToken, name, name, "", nil)
	var observed secondboxclient.SubjectSandboxPolicyObservation
	decodeResponseJSON(t, response, &observed)
	if observed.Revision != selected.Revision || observed.Effective != selected.Effective {
		t.Fatalf("application effective policy = %+v", observed)
	}
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodPut, server.URL+"/v1/subjects/"+name+"/sandbox-policy", app.BearerToken, name, name, "unauthorized", selection), http.StatusUnauthorized)
}
