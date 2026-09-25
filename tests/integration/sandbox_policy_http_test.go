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
	tenantRequest.AllowedProfileGrants = []string{name, name + "-other"}
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
	first, _, err := application.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{Profile: name, Metadata: map[string]string{}}, name+"-first")
	if err != nil {
		t.Fatal(err)
	}
	selection.Lifecycle = secondboxclient.SandboxLifecycleLimits{IdleSeconds: secondboxclient.Unlimited, MaximumDurationSeconds: 120}
	selected, err = controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, name+"-finite")
	if err != nil || selected.Effective.IdleSeconds != 600 || selected.Effective.MaximumDurationSeconds != 120 {
		t.Fatalf("clamped policy = %+v, %v", selected, err)
	}
	second, _, err := application.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{Profile: name, Metadata: map[string]string{}}, name+"-second")
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
	// Extend the same management contract without altering lifecycle pins.
	profile, err := operator.GetProfile(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	spec.Network.RequiresTenantEgressContext = new(bool)
	*spec.Network.RequiresTenantEgressContext = true
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{Gateway: "gateway", MaximumConnections: 128}
	spec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{MaximumConnections: 4096}
	profile, err = operator.ReviseProfile(t.Context(), name, profile.Revision, secondboxclient.ReviseProfileRequest{Spec: spec}, name+"-connections")
	if err != nil {
		t.Fatal(err)
	}
	initial, err = controller.GetSubjectSandboxPolicy(t.Context(), name, name)
	if err != nil || initial.AttributedExecution == nil || initial.AttributedExecution.MaximumConnections != 128 || initial.AttributedExecution.MaximumConnectionsCeiling != 4096 {
		t.Fatalf("inherited numeric policy: %+v %v", initial, err)
	}
	selection.Lifecycle = selected.Desired.Lifecycle
	selection.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 256}
	selected, err = controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-numeric")
	if err != nil || selected.AttributedExecution.MaximumConnections != 256 {
		t.Fatalf("selected numeric policy: %+v %v", selected, err)
	}
	replay, err = controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-numeric")
	if err != nil || replay.Revision != selected.Revision || *replay.AttributedExecution != *selected.AttributedExecution {
		t.Fatalf("numeric replay: %+v %v", replay, err)
	}
	selection.AttributedExecution.MaximumConnections = 512
	if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, initial.Revision, name+"-numeric"); secondboxclient.ProblemCodeOf(err) != "idempotency_conflict" {
		t.Fatalf("changed replay = %v", err)
	}
	spec.AttributedExecutionCeiling.MaximumConnections = 128
	if _, err := operator.ReviseProfile(t.Context(), name, profile.Revision, secondboxclient.ReviseProfileRequest{Spec: spec}, name+"-tighten"); err != nil {
		t.Fatal(err)
	}
	response = applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-policy?profile="+name, app.BearerToken, name, name, "", nil)
	decodeResponseJSON(t, response, &observed)
	if observed.AttributedExecution.MaximumConnections != 128 || observed.Desired.AttributedExecution.MaximumConnections != 256 {
		t.Fatalf("tightened observation: %+v %v", observed, err)
	}
	if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, name+"-ceiling-denied"); secondboxclient.ProblemCodeOf(err) != "profile_policy_ceiling_exceeded" {
		t.Fatalf("numeric ceiling error=%v", err)
	}
	for _, invalid := range []int64{0, -1, 4097} {
		selection.AttributedExecution.MaximumConnections = invalid
		if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, fmt.Sprintf("%s-invalid-%d", name, invalid)); secondboxclient.ProblemCodeOf(err) != "invalid_request" {
			t.Fatalf("numeric %d accepted: %v", invalid, err)
		}
	}
	selection.AttributedExecution = nil
	reset, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, selected.Revision, name+"-inherit")
	if err != nil || reset.Desired.AttributedExecution != nil || reset.AttributedExecution.MaximumConnections != 128 {
		t.Fatalf("numeric inheritance reset: %+v %v", reset, err)
	}

	assertSandboxPolicyPreservesUnchangedBlocks(t, operator, controller, name, spec, reset)

}

func assertSandboxPolicyPreservesUnchangedBlocks(t *testing.T, operator, controller *secondboxclient.Client, name string, spec contracts.ProfileRevisionSpec, current contracts.SubjectSandboxPolicyObservation) {
	t.Helper()
	sequence := 0
	publish := func(profileName string, spec contracts.ProfileRevisionSpec) {
		t.Helper()
		profile, err := operator.GetProfile(t.Context(), profileName)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		if _, err := operator.ReviseProfile(t.Context(), profileName, profile.Revision, secondboxclient.ReviseProfileRequest{Spec: spec}, fmt.Sprintf("%s-preserve-revision-%d", name, sequence)); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(selection contracts.SubjectSandboxPolicy) contracts.SubjectSandboxPolicyObservation {
		t.Helper()
		sequence++
		result, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, current.Revision, fmt.Sprintf("%s-preserve-%d", name, sequence))
		if err != nil {
			t.Fatal(err)
		}
		current = result
		return result
	}
	refuse := func(selection contracts.SubjectSandboxPolicy, code string) {
		t.Helper()
		sequence++
		if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, current.Revision, fmt.Sprintf("%s-refuse-%d", name, sequence)); secondboxclient.ProblemCodeOf(err) != code {
			t.Fatalf("want %s, got %v", code, err)
		}
	}
	selection := contracts.SubjectSandboxPolicy{Profile: name, Lifecycle: contracts.SandboxLifecycleLimits{IdleSeconds: 300, MaximumDurationSeconds: 240}, AttributedExecution: &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 64}}
	apply(selection)
	spec.Lifecycle.IdleSeconds = 60
	spec.Lifecycle.MaximumDurationSeconds = 120
	spec.LifecycleCeiling = &contracts.SandboxLifecycleLimits{IdleSeconds: 60, MaximumDurationSeconds: 120}
	publish(name, spec)
	// A connection edit preserves both lifecycle values above their new ceilings.
	selection.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 96}
	priorRevision := current.Revision
	selected := apply(selection)
	if selected.Desired.Lifecycle != selection.Lifecycle || selected.Effective.IdleSeconds != 60 || selected.Effective.MaximumDurationSeconds != 120 || selected.AttributedExecution.MaximumConnections != 96 {
		t.Fatalf("lifecycle preservation: %+v", selected)
	}
	replay, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, priorRevision, fmt.Sprintf("%s-preserve-%d", name, sequence))
	if err != nil || replay.Revision != selected.Revision || replay.Desired.Lifecycle != selection.Lifecycle {
		t.Fatalf("preservation replay: %+v %v", replay, err)
	}
	if _, err := controller.UpdateSubjectSandboxPolicy(t.Context(), name, selection, priorRevision, name+"-preserve-stale"); secondboxclient.ProblemCodeOf(err) != "precondition_failed" {
		t.Fatalf("stale preservation: %v", err)
	}
	changed := selection
	changed.Lifecycle.IdleSeconds = 301
	refuse(changed, "profile_policy_ceiling_exceeded")
	// Switching Profile cannot carry either block's prior exception with it.
	otherSpec := spec
	otherSpec.AttributedExecution = &contracts.AttributedExecutionPolicy{Gateway: "gateway", MaximumConnections: 32}
	otherSpec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{MaximumConnections: 32}
	if _, err := operator.CreateProfile(t.Context(), secondboxclient.CreateProfileRequest{Name: name + "-other", Spec: otherSpec}, name+"-other"); err != nil {
		t.Fatal(err)
	}
	changed = selection
	changed.Profile = name + "-other"
	refuse(changed, "profile_policy_ceiling_exceeded")
	// A lifecycle edit can likewise preserve a now-above-ceiling connection value.
	spec.AttributedExecution = &contracts.AttributedExecutionPolicy{Gateway: "gateway", MaximumConnections: 32}
	spec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{MaximumConnections: 32}
	publish(name, spec)
	selection.Lifecycle = contracts.SandboxLifecycleLimits{IdleSeconds: 30, MaximumDurationSeconds: 60}
	selected = apply(selection)
	if selected.Desired.AttributedExecution.MaximumConnections != 96 || selected.AttributedExecution.MaximumConnections != 32 || selected.Effective.IdleSeconds != 30 {
		t.Fatalf("connection preservation: %+v", selected)
	}
	changed = selection
	changed.Profile = name + "-other"
	refuse(changed, "profile_policy_ceiling_exceeded")
	changed = selection
	changed.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 95}
	refuse(changed, "profile_policy_ceiling_exceeded")
	// Removal of the head grant still allows preservation, but no changed value.
	spec.AttributedExecution = nil
	spec.AttributedExecutionCeiling = contracts.AttributedExecutionConnectionLimits{}
	publish(name, spec)
	selection.Lifecycle.IdleSeconds = 20
	selected = apply(selection)
	if selected.AttributedExecution != nil || selected.Desired.AttributedExecution.MaximumConnections != 96 {
		t.Fatalf("removed grant preservation: %+v", selected)
	}
	changed = selection
	changed.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 1}
	refuse(changed, "invalid_request")
	selection.AttributedExecution = nil
	selected = apply(selection)
	if selected.Desired.AttributedExecution != nil {
		t.Fatal("omission did not clear desired connection policy")
	}
	selection.AttributedExecution = &contracts.AttributedExecutionConnectionLimits{MaximumConnections: 96}
	refuse(selection, "invalid_request")
}
