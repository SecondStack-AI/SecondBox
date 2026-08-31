package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestPostgresPersistedAuthoritiesAuthenticateAcrossRestartAndNeverReturnVerifierMaterial(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("persisted-restart-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "shared-subject", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	application, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "authority-persisted-restart", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{"owner": "restart"}, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(
		application.BearerToken,
		ports.ApplicationBearerTokenPrefix+application.Authority.LookupID+"_",
	) {
		t.Fatalf("generated application credential has unexpected shape")
	}
	var verifier []byte
	var storedToken bool
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT token_verifier_sha256,
		       token_verifier_sha256=convert_to($2,'UTF8')
		FROM secondbox.application_authorities WHERE id=$1`,
		application.Authority.ID, application.BearerToken,
	).Scan(&verifier, &storedToken); err != nil {
		t.Fatal(err)
	}
	if len(verifier) != 32 || storedToken || bytes.Contains(verifier, []byte(application.BearerToken)) {
		t.Fatalf("stored credential representation is recoverable: verifier length=%d plaintext=%t", len(verifier), storedToken)
	}
	safe, err := controlPlaneStore.GetApplicationAuthority(
		t.Context(), tenant.Ref, application.Authority.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{application.BearerToken, "bearerToken", "verifier", "sha256"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("safe authority read contains %q: %s", forbidden, encoded)
		}
	}

	controlPlaneStore.Close()
	restarted, err := NewPostgresControlPlaneStore(t.Context(), storeTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	authenticated, err := restarted.AuthenticateApplicationAuthority(
		t.Context(), application.BearerToken, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.ID != application.Authority.ID ||
		authenticated.TenantRef != tenant.Ref || authenticated.SubjectRef != subject.Ref {
		t.Fatalf("restarted authentication = %#v", authenticated)
	}
}

func TestPostgresPersistedAuthorityRevocationExpiryRotationAndKindIsolation(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("persisted-lifecycle-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "lifecycle-subject", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	controller, err := controlPlaneStore.CreateTenantControllerAuthority(t.Context(), contracts.TenantControllerAuthority{
		ID: "authority-controller-lifecycle", TenantRef: tenant.Ref,
		State: contracts.AuthorityStateActive, Metadata: map[string]string{}, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	application, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "authority-application-lifecycle", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), controller.BearerToken, now,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("controller authenticated as application: %v", err)
	}
	if _, err := controlPlaneStore.AuthenticateTenantControllerAuthority(
		t.Context(), application.BearerToken, now,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("application authenticated as controller: %v", err)
	}

	rotated, err := controlPlaneStore.RotateApplicationAuthority(
		t.Context(), tenant.Ref, application.Authority.ID, 1, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.BearerToken == application.BearerToken ||
		rotated.Authority.LookupID == application.Authority.LookupID || rotated.Authority.Revision != 2 {
		t.Fatalf("rotated authority = %#v", rotated.Authority)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), application.BearerToken, now.Add(time.Minute),
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("replaced application token authentication = %v", err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), rotated.BearerToken, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.RevokeApplicationAuthority(
		t.Context(), tenant.Ref, application.Authority.ID, 2, now.Add(2*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), rotated.BearerToken, now.Add(2*time.Minute),
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("revoked application token authentication = %v", err)
	}

	expiresAt := now.Add(time.Minute)
	expiring, err := controlPlaneStore.CreateTenantControllerAuthority(t.Context(), contracts.TenantControllerAuthority{
		ID: "authority-controller-expiring", TenantRef: tenant.Ref,
		State: contracts.AuthorityStateActive, Metadata: map[string]string{},
		ExpiresAt: &expiresAt, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateTenantControllerAuthority(
		t.Context(), expiring.BearerToken, expiresAt,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("expired controller authentication = %v", err)
	}
	if _, err := controlPlaneStore.RevokeTenantControllerAuthority(
		t.Context(), tenant.Ref, controller.Authority.ID, 1, now.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateTenantControllerAuthority(
		t.Context(), controller.BearerToken, now.Add(time.Minute),
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("revoked controller token authentication = %v", err)
	}
}

func TestManagedTenantEgressContextUpdateIsRevisionFencedIdempotentAndRecoverable(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("tenant-egress-context-update", now)
	initialContext := "secondstack-initial"
	tenant.EgressContext = &initialContext
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	created, err := controlPlaneStore.GetTenant(t.Context(), tenant.Ref)
	if err != nil || created.EgressContext == nil || *created.EgressContext != initialContext {
		t.Fatalf("created Tenant egress context = %#v error=%v", created.EgressContext, err)
	}

	contextName := "secondstack-staging"
	input := adminTestInput(tenant.Ref, "platform", "tenant.egress_context.update", "set-egress-context", now)
	updated, result, err := controlPlaneStore.UpdateManagedTenantEgressContext(
		t.Context(), tenant.Ref, &contextName, tenant.Revision, now.Add(time.Second), input,
	)
	if err != nil || result.Replayed || updated.EgressContext == nil ||
		*updated.EgressContext != contextName || updated.Revision != tenant.Revision+1 {
		t.Fatalf("Tenant egress-context update = %#v result=%#v error=%v", updated, result, err)
	}
	replayed, result, err := controlPlaneStore.UpdateManagedTenantEgressContext(
		t.Context(), tenant.Ref, &contextName, tenant.Revision, now.Add(time.Second), input,
	)
	if err != nil || !result.Replayed || replayed.Revision != updated.Revision ||
		replayed.EgressContext == nil || *replayed.EgressContext != contextName {
		t.Fatalf("Tenant egress-context replay = %#v result=%#v error=%v", replayed, result, err)
	}
	conflictInput := adminTestInput(tenant.Ref, "platform", "tenant.egress_context.update", "stale-egress-context", now)
	if _, _, err := controlPlaneStore.UpdateManagedTenantEgressContext(
		t.Context(), tenant.Ref, nil, tenant.Revision, now.Add(2*time.Second), conflictInput,
	); !errors.Is(err, ports.ErrRevisionConflict) {
		t.Fatalf("stale Tenant egress-context update error = %v", err)
	}

	clearInput := adminTestInput(tenant.Ref, "platform", "tenant.egress_context.update", "clear-egress-context", now)
	cleared, _, err := controlPlaneStore.UpdateManagedTenantEgressContext(
		t.Context(), tenant.Ref, nil, updated.Revision, now.Add(3*time.Second), clearInput,
	)
	if err != nil || cleared.EgressContext != nil || cleared.Revision != updated.Revision+1 {
		t.Fatalf("Tenant egress-context clear = %#v error=%v", cleared, err)
	}

	controlPlaneStore.Close()
	restarted, err := NewPostgresControlPlaneStore(t.Context(), storeTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	recovered, err := restarted.GetTenant(t.Context(), tenant.Ref)
	if err != nil || recovered.EgressContext != nil || recovered.Revision != cleared.Revision {
		t.Fatalf("recovered Tenant = %#v error=%v", recovered, err)
	}
}

func TestPostgresPersistedAuthoritiesFailClosedForInvalidVerifierAndCrossTenantReads(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	firstTenant := managementTestTenant("persisted-isolation-a", now)
	secondTenant := managementTestTenant("persisted-isolation-b", now)
	for _, tenant := range []contracts.Tenant{firstTenant, secondTenant} {
		if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
			t.Fatal(err)
		}
		if _, err := controlPlaneStore.CreateSubject(
			t.Context(), managementTestSubject(tenant.Ref, "same-local-subject", now),
		); err != nil {
			t.Fatal(err)
		}
	}
	created, err := controlPlaneStore.CreateApplicationAuthority(t.Context(), contracts.ApplicationAuthority{
		ID: "authority-invalid-verifier", TenantRef: firstTenant.Ref, SubjectRef: "same-local-subject",
		State:  contracts.AuthorityStateActive,
		Scopes: []string{"sandbox:read"}, ProfileGrants: []string{"coding"},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.GetApplicationAuthority(
		t.Context(), secondTenant.Ref, created.Authority.ID,
	); !errors.Is(err, ports.ErrManagementNotFound) {
		t.Fatalf("cross-tenant read error = %v", err)
	}
	if _, err := controlPlaneStore.GetApplicationAuthority(
		t.Context(), secondTenant.Ref, "authority-does-not-exist",
	); !errors.Is(err, ports.ErrManagementNotFound) {
		t.Fatalf("absent read error = %v", err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET state='closed'
		WHERE tenant_ref=$1 AND ref=$2`, firstTenant.Ref, created.Authority.SubjectRef,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), created.BearerToken, now,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("closed Subject authentication = %v", err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.subjects SET state='active'
		WHERE tenant_ref=$1 AND ref=$2`, firstTenant.Ref, created.Authority.SubjectRef,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.tenants SET allowed_application_scopes_json='[]'::jsonb
		WHERE ref=$1`, firstTenant.Ref,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), created.BearerToken, now,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("scope above current Tenant ceiling authentication = %v", err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.tenants
		SET allowed_application_scopes_json='["sandbox:read"]'::jsonb
		WHERE ref=$1`, firstTenant.Ref,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		UPDATE secondbox.application_authorities
		SET token_verifier_sha256='\\x01'::bytea WHERE id=$1`, created.Authority.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlaneStore.AuthenticateApplicationAuthority(
		t.Context(), created.BearerToken, now,
	); !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("invalid verifier authentication = %v", err)
	}
	failed, err := controlPlaneStore.CreateTenantControllerAuthority(t.Context(), contracts.TenantControllerAuthority{
		ID: created.Authority.ID, TenantRef: secondTenant.Ref,
		State: contracts.AuthorityStateActive, Metadata: map[string]string{}, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	})
	if err == nil || failed.BearerToken != "" {
		t.Fatalf("cross-kind identity collision response = %#v error=%v", failed, err)
	}
}

func TestPostgresCredentialIdempotencyStoresOnlyAuthorityAndRejectsReplay(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("credential-idempotency-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	subject := managementTestSubject(tenant.Ref, "credential-idempotency-subject", now)
	if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(30 * time.Minute)
	idempotency := func(operation, targetID, key, requestHash string) ports.AdminIdempotencyInput {
		return ports.AdminIdempotencyInput{
			TenantRef: tenant.Ref, SubjectRef: subject.Ref, Operation: operation,
			TargetID: targetID, Key: key, RequestHash: requestHash,
			Now: now, Ends: now.Add(time.Hour), AuditEvent: adminTestAudit(key, now),
		}
	}

	controllerAuthority := contracts.TenantControllerAuthority{
		ID: "credential-idempotency-controller", TenantRef: tenant.Ref,
		Grant: contracts.TenantControllerGrantManagement, State: contracts.AuthorityStateActive,
		Metadata: map[string]string{}, ExpiresAt: &expiresAt, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	controllerCreateIdempotency := idempotency("tenant_controller.created", controllerAuthority.ID, "controller-create-key", "controller-create-hash")
	controller, _, err := controlPlaneStore.CreateManagedTenantControllerAuthority(t.Context(), controllerAuthority, controllerCreateIdempotency)
	if err != nil || controller.BearerToken == "" {
		t.Fatalf("create managed controller = %#v error=%v", controller, err)
	}
	controlPlaneStore.Close()
	restartedStore, err := NewPostgresControlPlaneStore(t.Context(), storeTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedStore.Close)
	controlPlaneStore = restartedStore
	if _, result, err := controlPlaneStore.CreateManagedTenantControllerAuthority(t.Context(), controllerAuthority, controllerCreateIdempotency); !errors.Is(err, ports.ErrCredentialResponseUnavailable) || !result.Replayed {
		t.Fatalf("replay managed controller after restart replay=%t error=%v", result.Replayed, err)
	}
	var controllerCreateAuditEvents int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.audit_events WHERE id=$1`, controllerCreateIdempotency.AuditEvent.ID,
	).Scan(&controllerCreateAuditEvents); err != nil {
		t.Fatal(err)
	}
	if controllerCreateAuditEvents != 1 {
		t.Fatalf("controller creation audit events after replay = %d", controllerCreateAuditEvents)
	}
	conflictingControllerCreate := controllerCreateIdempotency
	conflictingControllerCreate.RequestHash = "different-controller-create-hash"
	if _, _, err := controlPlaneStore.CreateManagedTenantControllerAuthority(t.Context(), controllerAuthority, conflictingControllerCreate); !errors.Is(err, ports.ErrIdempotencyConflict) {
		t.Fatalf("conflicting managed controller replay error = %v", err)
	}

	applicationAuthority := contracts.ApplicationAuthority{
		ID: "credential-idempotency-application", TenantRef: tenant.Ref, SubjectRef: subject.Ref,
		State: contracts.AuthorityStateActive, Scopes: []string{"sandbox:read"},
		ProfileGrants: []string{"coding"}, Metadata: map[string]string{}, ExpiresAt: &expiresAt,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	applicationCreateIdempotency := idempotency("application_authority.created", applicationAuthority.ID, "application-create-key", "application-create-hash")
	application, _, err := controlPlaneStore.CreateManagedApplicationAuthority(t.Context(), applicationAuthority, applicationCreateIdempotency)
	if err != nil || application.BearerToken == "" {
		t.Fatalf("create managed application = %#v error=%v", application, err)
	}
	if _, result, err := controlPlaneStore.CreateManagedApplicationAuthority(t.Context(), applicationAuthority, applicationCreateIdempotency); !errors.Is(err, ports.ErrCredentialResponseUnavailable) || !result.Replayed {
		t.Fatalf("replay managed application replay=%t error=%v", result.Replayed, err)
	}

	controllerRotateIdempotency := idempotency("tenant_controller.rotated", controller.Authority.ID, "controller-rotate-key", "controller-rotate-hash")
	rotatedController, _, err := controlPlaneStore.RotateManagedTenantControllerAuthority(t.Context(), tenant.Ref, controller.Authority.ID, controller.Authority.Revision, now, controllerRotateIdempotency)
	if err != nil {
		t.Fatal(err)
	}
	if _, result, err := controlPlaneStore.RotateManagedTenantControllerAuthority(t.Context(), tenant.Ref, controller.Authority.ID, controller.Authority.Revision, now, controllerRotateIdempotency); !errors.Is(err, ports.ErrCredentialResponseUnavailable) || !result.Replayed {
		t.Fatalf("replay managed controller rotation replay=%t error=%v", result.Replayed, err)
	}
	storedController, err := controlPlaneStore.GetTenantControllerAuthority(t.Context(), tenant.Ref, controller.Authority.ID)
	if err != nil || storedController.Revision != controller.Authority.Revision+1 || storedController.LookupID != rotatedController.Authority.LookupID {
		t.Fatalf("stored controller rotation = %#v error=%v", storedController, err)
	}

	applicationRotateIdempotency := idempotency("application_authority.rotated", application.Authority.ID, "application-rotate-key", "application-rotate-hash")
	rotatedApplication, _, err := controlPlaneStore.RotateManagedApplicationAuthority(t.Context(), tenant.Ref, application.Authority.ID, application.Authority.Revision, now, applicationRotateIdempotency)
	if err != nil {
		t.Fatal(err)
	}
	if _, result, err := controlPlaneStore.RotateManagedApplicationAuthority(t.Context(), tenant.Ref, application.Authority.ID, application.Authority.Revision, now, applicationRotateIdempotency); !errors.Is(err, ports.ErrCredentialResponseUnavailable) || !result.Replayed {
		t.Fatalf("replay managed application rotation replay=%t error=%v", result.Replayed, err)
	}
	storedApplication, err := controlPlaneStore.GetApplicationAuthority(t.Context(), tenant.Ref, application.Authority.ID)
	if err != nil || storedApplication.Revision != application.Authority.Revision+1 || storedApplication.LookupID != rotatedApplication.Authority.LookupID {
		t.Fatalf("stored application rotation = %#v error=%v", storedApplication, err)
	}

	var controllerCount, applicationCount, secretRecordCount int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.tenant_controller_authorities WHERE tenant_ref=$1`, tenant.Ref).Scan(&controllerCount); err != nil {
		t.Fatal(err)
	}
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.application_authorities WHERE tenant_ref=$1`, tenant.Ref).Scan(&applicationCount); err != nil {
		t.Fatal(err)
	}
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.idempotency_records
		WHERE response_json::text LIKE '%secondbox_tenant_controller_%'
		   OR response_json::text LIKE '%secondbox_application_%'`).Scan(&secretRecordCount); err != nil {
		t.Fatal(err)
	}
	if controllerCount != 1 || applicationCount != 1 || secretRecordCount != 0 {
		t.Fatalf("credential idempotency records controllers=%d applications=%d secret_records=%d", controllerCount, applicationCount, secretRecordCount)
	}
}

func TestTenantUsagePaginatesBeyondHistoricalSubjectChurn(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("usage-history-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	for index := range 205 {
		subject := managementTestSubject(tenant.Ref, fmt.Sprintf("historical-%03d", index), now.Add(time.Duration(index)*time.Millisecond))
		if _, err := controlPlaneStore.CreateSubject(t.Context(), subject); err != nil {
			t.Fatal(err)
		}
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			UPDATE secondbox.subjects SET state=$3,cleanup_state=$4
			WHERE tenant_ref=$1 AND ref=$2`, tenant.Ref, subject.Ref,
			contracts.SubjectStateClosed, contracts.SubjectCleanupStateSucceeded); err != nil {
			t.Fatal(err)
		}
	}
	active := managementTestSubject(tenant.Ref, "active-subject", now.Add(time.Second))
	if _, err := controlPlaneStore.CreateSubject(t.Context(), active); err != nil {
		t.Fatal(err)
	}

	var cursor string
	seen := 0
	for {
		usage, err := controlPlaneStore.GetTenantUsage(t.Context(), tenant.Ref, 100, cursor, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if usage.Usage.ActiveSubjects != 1 {
			t.Fatalf("active Subject usage = %d", usage.Usage.ActiveSubjects)
		}
		seen += len(usage.Subjects)
		if usage.NextCursor == nil {
			break
		}
		cursor = *usage.NextCursor
	}
	if seen != 206 {
		t.Fatalf("paginated Subject usage count = %d", seen)
	}
}

func TestManagedMutationRollsBackWhenItsAuditCannotCommit(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	tenant := managementTestTenant("atomic-audit-tenant", now)
	if _, err := controlPlaneStore.CreateTenant(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	duplicate := adminTestAudit("atomic-audit", now)
	if err := controlPlaneStore.AppendAuditEvent(t.Context(), *duplicate); err != nil {
		t.Fatal(err)
	}
	input := ports.AdminIdempotencyInput{
		TenantRef: tenant.Ref, Operation: "tenant.suspend", TargetID: tenant.Ref,
		Key: "atomic-audit", RequestHash: "atomic-audit-hash", Now: now,
		Ends: now.Add(time.Hour), AuditEvent: duplicate,
	}
	if _, _, err := controlPlaneStore.SetTenantState(
		t.Context(), tenant.Ref, contracts.TenantStateSuspended, tenant.Revision, now, input,
	); err == nil {
		t.Fatal("Tenant mutation committed without its audit event")
	}
	stored, err := controlPlaneStore.GetTenant(t.Context(), tenant.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != contracts.TenantStateActive || stored.Revision != tenant.Revision {
		t.Fatalf("Tenant changed after audit failure = %#v", stored)
	}
	var idempotencyRecords int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.idempotency_records
		WHERE operation=$1 AND idempotency_key=$2`, input.Operation, input.Key).Scan(&idempotencyRecords); err != nil {
		t.Fatal(err)
	}
	if idempotencyRecords != 0 {
		t.Fatalf("idempotency records committed after audit failure = %d", idempotencyRecords)
	}
}

func managementTestTenant(ref string, now time.Time) contracts.Tenant {
	return contracts.Tenant{
		Ref: ref, State: contracts.TenantStateActive,
		AllowedProfileGrants: []string{"coding"},
		AllowedApplicationScopes: []string{
			"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports",
		},
		AggregateQuota: contracts.TenantQuota{
			MaxSandboxes: 10, MaxActiveInstances: 10, MaxVCPUCount: 10,
			MaxMemoryBytes: 10 << 30, MaxSnapshots: 10, MaxPortSessions: 10,
			MaxConcurrentOperations: 10, MaxActiveSubjects: 10, MaxApplicationAuthorities: 10,
		},
		ExpiryPolicy: contracts.TenantExpiryPolicy{
			MaximumSubjectLifetimeSeconds: 3600, MaximumAuthorityLifetimeSeconds: 3600,
		},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func managementTestSubject(tenantRef string, subjectRef string, now time.Time) contracts.Subject {
	return contracts.Subject{
		TenantRef: tenantRef, Ref: subjectRef,
		State: contracts.SubjectStateActive, CleanupState: contracts.SubjectCleanupStateNone,
		Quota: contracts.QuotaLimits{
			MaxSandboxes: 2, MaxActiveInstances: 2, MaxVCPUCount: 2,
			MaxMemoryBytes: 2 << 30, MaxSnapshots: 2, MaxPortSessions: 2,
			MaxConcurrentOperations: 2,
		},
		Metadata: map[string]string{}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
