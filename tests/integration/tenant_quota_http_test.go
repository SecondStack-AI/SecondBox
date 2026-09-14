package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestPlatformTenantQuotaUpdateGrowsCapacityWithoutRecreation(t *testing.T) {
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
	tenantRef := fmt.Sprintf("quota-http-%d", integrationIdentitySequence.Add(1))
	controller, application := bootstrapPersistedHTTPAuthority(t, operator, server.URL, server.Client(), tenantRef, "", now, tenantRef)
	tenant, err := operator.GetTenant(t.Context(), tenantRef)
	if err != nil {
		t.Fatal(err)
	}
	quota := tenant.AggregateQuota
	encodedQuota, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	var completeQuota map[string]json.RawMessage
	if err := json.Unmarshal(encodedQuota, &completeQuota); err != nil {
		t.Fatal(err)
	}
	for field := range completeQuota {
		for _, malformed := range []string{"omitted"} {
			t.Run(field+"/"+malformed, func(t *testing.T) {
				var incompleteQuota map[string]json.RawMessage
				if err := json.Unmarshal(encodedQuota, &incompleteQuota); err != nil {
					t.Fatal(err)
				}
				if malformed == "omitted" {
					delete(incompleteQuota, field)
				} else {
					incompleteQuota[field] = json.RawMessage("null")
				}
				body, err := json.Marshal(map[string]any{"aggregateQuota": incompleteQuota})
				if err != nil {
					t.Fatal(err)
				}
				httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+"/v1/tenants/"+tenantRef+"/quota", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				httpRequest.Header.Set("Authorization", "Bearer "+testPlatformToken)
				httpRequest.Header.Set("Content-Type", "application/json")
				httpRequest.Header.Set("If-Match", fmt.Sprintf(`"revision-%d"`, tenant.Revision))
				httpRequest.Header.Set("Idempotency-Key", field+"-"+malformed)
				response, err := server.Client().Do(httpRequest)
				if err != nil {
					t.Fatal(err)
				}
				assertHTTPStatusAndClose(t, response, http.StatusBadRequest)
				persisted, err := operator.GetTenant(t.Context(), tenantRef)
				if err != nil || persisted.Revision != tenant.Revision || persisted.AggregateQuota != quota {
					t.Fatalf("invalid quota changed Tenant: %+v, %v", persisted, err)
				}
			})
		}
	}
	quota.MaxSandboxes = 1000
	quota.MaxSnapshots = 0 // An explicit zero is a valid limit when there is no usage.
	request := secondboxclient.UpdateTenantQuotaRequest{AggregateQuota: quota}
	updated, err := operator.UpdateTenantQuota(t.Context(), tenantRef, request, tenant.Revision, "grow-tenant-capacity")
	if err != nil || updated.Ref != tenant.Ref || updated.Revision != tenant.Revision+1 || updated.AggregateQuota.MaxSandboxes != 1000 || updated.AggregateQuota.MaxSnapshots != 0 {
		t.Fatalf("expanded Tenant = %+v, %v", updated, err)
	}
	replayed, err := operator.UpdateTenantQuota(t.Context(), tenantRef, request, tenant.Revision, "grow-tenant-capacity")
	if err != nil || replayed.Revision != updated.Revision {
		t.Fatalf("quota replay = %+v, %v", replayed, err)
	}
	if _, err := operator.UpdateTenantQuota(t.Context(), tenantRef, request, tenant.Revision, "stale-tenant-capacity"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodePreconditionFailed {
		t.Fatalf("stale quota = %v", err)
	}
	request.AggregateQuota.MaxActiveSubjects = 0
	if _, err := operator.UpdateTenantQuota(t.Context(), tenantRef, request, updated.Revision, "below-tenant-usage"); secondboxclient.ProblemCodeOf(err) != secondboxclient.ProblemCodeStateConflict {
		t.Fatalf("below committed usage = %v", err)
	}
	controllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controller.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	usage, err := controllerClient.GetTenantUsage(t.Context())
	if err != nil || usage.Limits.MaxSandboxes != 1000 || len(usage.Subjects) != 1 || usage.Subjects[0].Limits.MaxSandboxes != 10 {
		t.Fatalf("quota ledgers or Subject changed: %+v, %v", usage, err)
	}
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodPut, server.URL+"/v1/tenants/"+tenantRef+"/quota", application.BearerToken, tenantRef, "same-local-subject", "unauthorized-quota", request), http.StatusUnauthorized)
	if _, err := controllerClient.UpdateTenantQuota(t.Context(), tenantRef, request, updated.Revision, "controller-quota-denied"); err == nil {
		t.Fatal("controller changed tenant quota")
	}
}
