package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/resourceapply"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStandardResourcesFreshUpgradeAndReplayConvergeThroughLiveControlPlane(t *testing.T) {
	// The fixture control plane validates credential creation on its frozen
	// clock while HTTP authentication checks the wall clock, so the tenant
	// ceiling must span both and expiry must sit in the wall-clock future.
	now := time.Now().UTC()
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "standard-isolated")
	server := httptest.NewServer(persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	client, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	tenantRef := secondboxclient.OwnershipRef(fmt.Sprintf("standard-isolated-%d", integrationIdentitySequence.Add(1)))
	tenantRequest := persistedHTTPTenantRequest(tenantRef)
	tenantRequest.AllowedProfileGrants = []string{standardresources.AgentCompartmentIsolated}
	tenantRequest.ExpiryPolicy.MaximumAuthorityLifetimeSeconds = int64(365 * 24 * time.Hour / time.Second)
	tenantRequest.ExpiryPolicy.MaximumSubjectLifetimeSeconds = int64(365 * 24 * time.Hour / time.Second)
	if _, err := client.CreateTenant(t.Context(), tenantRequest, "standard-isolated-tenant"); err != nil {
		t.Fatal(err)
	}
	controller, err := client.CreateTenantControllerAuthority(t.Context(), tenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{ExpiresAt: now.Add(45 * time.Minute), Metadata: map[string]string{}}, "standard-isolated-controller")
	if err != nil {
		t.Fatal(err)
	}
	controllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controller.BearerToken, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerClient.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{Ref: "standard-isolated-subject", Quota: secondboxclient.SubjectQuota{MaxSandboxes: 10, MaxActiveInstances: 10, MaxVcpuCount: 10, MaxMemoryBytes: 10 << 30, MaxSnapshots: 10, MaxPortSessions: 10, MaxConcurrentOperations: 10}, Metadata: map[string]string{}}, "standard-isolated-subject"); err != nil {
		t.Fatal(err)
	}
	application, err := controllerClient.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{SubjectRef: "standard-isolated-subject", Scopes: []string{"sandbox:read", "sandbox:lifecycle"}, ProfileGrants: []string{standardresources.AgentCompartmentIsolated}, Metadata: map[string]string{}, ExpiresAt: now.Add(30 * time.Minute)}, "standard-isolated-application")
	if err != nil {
		t.Fatal(err)
	}

	document := liveStandardDocument(t)
	fresh, err := resourceapply.Apply(t.Context(), client, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Results) != 5 || fresh.Results[1].Action != resourceapply.ActionCreate || fresh.Results[2].Action != resourceapply.ActionAppend {
		t.Fatalf("fresh results = %#v", fresh.Results)
	}
	agent, err := client.GetProfile(t.Context(), standardresources.AgentCompartment)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.Revisions) != 2 || agent.Revisions[0].Spec.Execution.MaximumDeadlineMilliseconds != 120000 || agent.CurrentRevision.Number != 2 || agent.CurrentRevision.Spec.Execution.MaximumDeadlineMilliseconds != 900000 {
		t.Fatalf("fresh agent-compartment lineage = %#v", agent)
	}
	isolated, err := client.GetProfile(t.Context(), standardresources.AgentCompartmentIsolated)
	if err != nil {
		t.Fatal(err)
	}
	applicationClient, err := secondboxclient.NewSecondBoxSubjectClient(server.URL, application.BearerToken, string(tenantRef), "standard-isolated-subject", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := applicationClient.GetProfile(t.Context(), standardresources.AgentCompartmentIsolated)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Name != isolated.Name || inspected.CurrentRevision.Number != isolated.CurrentRevision.Number || inspected.CurrentRevision.Spec.Network.Mode != "deny_all" || len(inspected.CurrentRevision.Spec.Network.Destinations) != 0 {
		t.Fatalf("application-inspected isolated Profile = %#v", inspected)
	}
	grants := append([]string{}, account.ProfileGrants...)
	grants = append(grants, standardresources.AgentCompartmentIsolated)
	if _, err := updateFixtureServiceAccount(t, controlPlane, t.Context(), admin, account.TenantRef, account.ID, fixtureUpdateServiceAccountRequest{ProfileGrants: &grants}); err != nil {
		t.Fatal(err)
	}
	upgraded := document
	upgraded.Profiles = append([]resourceapply.Profile(nil), document.Profiles...)
	isolatedIndex := slices.IndexFunc(upgraded.Profiles, func(profile resourceapply.Profile) bool {
		return profile.Name == standardresources.AgentCompartmentIsolated
	})
	if isolatedIndex < 0 {
		t.Fatal("isolated Profile is absent from standard document")
	}
	upgraded.Profiles[isolatedIndex].Revisions = append([]resourceapply.ProfileRevision(nil), document.Profiles[isolatedIndex].Revisions...)
	second := upgraded.Profiles[isolatedIndex].Revisions[0].Spec
	second.Lifecycle.InitialState = secondboxclient.SandboxDesiredStateStopped
	digest, err := resourceapply.SpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.Profiles[isolatedIndex].Revisions = append(upgraded.Profiles[isolatedIndex].Revisions, resourceapply.ProfileRevision{Number: 2, SpecDigest: digest, Spec: second})
	if _, err := resourceapply.Apply(t.Context(), client, upgraded); err != nil {
		t.Fatal(err)
	}
	seedFixtureHomeRunner(t, standardresources.PoolAMD64, "runner-standard-isolated")
	database, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	if _, err := database.Exec(t.Context(), `UPDATE secondbox.runner_pools SET ready_runner_count=1 WHERE name=$1`, standardresources.PoolAMD64); err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "standard-isolated-pinning", secondboxclient.CreateSandboxRequest{Profile: standardresources.AgentCompartmentIsolated, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	pinnedRevisionID := sandbox.ProfileRevisionID
	third := second
	third.Resources.VCPUCount++
	thirdDigest, err := resourceapply.SpecDigest(third)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.Profiles[isolatedIndex].Revisions = append(upgraded.Profiles[isolatedIndex].Revisions, resourceapply.ProfileRevision{Number: 3, SpecDigest: thirdDigest, Spec: third})
	if _, err := resourceapply.Apply(t.Context(), client, upgraded); err != nil {
		t.Fatal(err)
	}
	pinned, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ProfileRevisionID != pinnedRevisionID {
		t.Fatalf("isolated Sandbox Profile revision changed from %q to %q", pinnedRevisionID, pinned.ProfileRevisionID)
	}
	replayed, err := resourceapply.Apply(t.Context(), client, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range replayed.Results {
		if result.Action != resourceapply.ActionNoop {
			t.Fatalf("replay changed resource: %#v", replayed)
		}
	}
	for _, desired := range upgraded.Profiles {
		profile, err := client.GetProfile(t.Context(), desired.Name)
		if err != nil {
			t.Fatal(err)
		}
		head := desired.Revisions[len(desired.Revisions)-1]
		actual, err := resourceapply.SpecDigest(profile.CurrentRevision.Spec)
		if err != nil || profile.CurrentRevision.Number != head.Number || actual != head.SpecDigest {
			t.Fatalf("Profile %s identity = revision %d digest %s error %v", desired.Name, profile.CurrentRevision.Number, actual, err)
		}
	}
}

func liveStandardDocument(t *testing.T) resourceapply.Document {
	t.Helper()
	runtimeDigest := "sha256:9279ca3f8bc3eac4adcd1953926a33fc42da99641d60af042eea12eb12ba0335"
	toolchainDigest := "sha256:cd859a7b0ef9849cc842c8b9c4d0b3b21340e50bed1ac712126585a9fa5553b4"
	agent, err := standardresources.ProfileLineage(standardresources.AgentCompartment, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	coding, err := standardresources.ProfileLineage(standardresources.DurableCoding, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := standardresources.ProfileLineage(standardresources.AgentCompartmentIsolated, runtimeDigest, toolchainDigest)
	if err != nil {
		t.Fatal(err)
	}
	return resourceapply.Document{SchemaVersion: resourceapply.SchemaVersion, RunnerPools: []resourceapply.RunnerPool{{Name: standardresources.PoolAMD64, Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"}, CapacityPolicy: map[string]int64{"maxSandboxes": 20, "maxVcpuCount": 80, "maxMemoryBytes": 171798691840}, State: "ready", MutableFields: []string{"capacityPolicy", "state"}}}, Profiles: []resourceapply.Profile{agent, coding, isolated}}
}
