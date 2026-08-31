package integration_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestTenantEgressContextPinsOnlyNewRequiringSandboxesAndRecovers(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "egress-pinning",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	requiringProfile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-egress-pinning",
	)
	requiringSpec := requiringProfile.CurrentRevision.Spec
	requiresContext := true
	requiringSpec.Network.RequiresTenantEgressContext = &requiresContext
	requiringProfile, err := controlPlane.ReviseProfile(
		t.Context(), admin, requiringProfile.Name,
		contracts.ReviseProfileRequest{Spec: requiringSpec},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "missing-egress-context",
		contracts.CreateSandboxRequest{Profile: requiringProfile.Name, Metadata: map[string]string{}},
	); !errors.Is(err, ports.ErrTenantEgressContextRequired) {
		t.Fatalf("requiring Profile without Tenant context error = %v", err)
	}
	assertNoSandboxIntent(t, project.ID, account.ID)

	firstContext := "secondstack-staging"
	tenant, replayed, err := controlPlane.UpdateTenantEgressContext(
		t.Context(), admin, project.ID, "set-staging-context", 1,
		contracts.UpdateTenantEgressContextRequest{EgressContext: &firstContext},
	)
	if err != nil || replayed || tenant.EgressContext == nil || *tenant.EgressContext != firstContext {
		t.Fatalf("set first Tenant context = %#v replayed=%t error=%v", tenant, replayed, err)
	}
	first, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "first-egress-context-sandbox",
		contracts.CreateSandboxRequest{Profile: requiringProfile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.EgressContext == nil || *first.EgressContext != firstContext {
		t.Fatalf("first Sandbox egress context = %#v", first.EgressContext)
	}

	secondContext := "secondstack-development"
	tenant, _, err = controlPlane.UpdateTenantEgressContext(
		t.Context(), admin, project.ID, "set-development-context", tenant.Revision,
		contracts.UpdateTenantEgressContextRequest{EgressContext: &secondContext},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "second-egress-context-sandbox",
		contracts.CreateSandboxRequest{Profile: requiringProfile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.EgressContext == nil || *second.EgressContext != secondContext {
		t.Fatalf("second Sandbox egress context = %#v", second.EgressContext)
	}

	isolatedProfile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "isolated",
	)
	isolated, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "isolated-with-tenant-context",
		contracts.CreateSandboxRequest{Profile: isolatedProfile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.EgressContext != nil {
		t.Fatalf("isolated Sandbox pinned Tenant context %#v", isolated.EgressContext)
	}
	auditEvents, err := databaseStore.ListAuditEvents(t.Context(), project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	firstAudit := findAuditEvent(auditEvents, "sandbox.created", first.ID)
	if firstAudit == nil || firstAudit.Details["egressContext"] != firstContext {
		t.Fatalf("first Sandbox creation audit = %#v", firstAudit)
	}
	isolatedAudit := findAuditEvent(auditEvents, "sandbox.created", isolated.ID)
	if isolatedAudit == nil {
		t.Fatalf("isolated Sandbox creation audit is absent: %#v", auditEvents)
	}
	if _, exists := isolatedAudit.Details["egressContext"]; exists {
		t.Fatalf("isolated Sandbox creation audit exposed a context: %#v", isolatedAudit)
	}

	databaseStore.Close()
	restarted, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	for _, expected := range []struct {
		id      string
		context *string
	}{{first.ID, &firstContext}, {second.ID, &secondContext}, {isolated.ID, nil}} {
		recovered, err := restarted.GetSandbox(t.Context(), project.ID, account.ID, expected.id)
		if err != nil {
			t.Fatal(err)
		}
		if !equalOptionalString(recovered.EgressContext, expected.context) {
			t.Fatalf("recovered Sandbox %s context = %#v, want %#v", expected.id, recovered.EgressContext, expected.context)
		}
	}
}

func assertNoSandboxIntent(t *testing.T, tenantRef, subjectRef string) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var sandboxes, workspaces, operations int64
	if err := pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM secondbox.sandboxes WHERE tenant_ref=$1 AND subject_ref=$2),
		       (SELECT count(*) FROM secondbox.workspaces WHERE tenant_ref=$1 AND subject_ref=$2),
		       (SELECT count(*) FROM secondbox.operations WHERE tenant_ref=$1 AND subject_ref=$2)`,
		tenantRef, subjectRef,
	).Scan(&sandboxes, &workspaces, &operations); err != nil {
		t.Fatal(err)
	}
	if sandboxes != 0 || workspaces != 0 || operations != 0 {
		t.Fatalf("durable intent after context rejection: sandboxes=%d workspaces=%d operations=%d", sandboxes, workspaces, operations)
	}
}

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
