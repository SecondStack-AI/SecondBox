package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApplicationSubjectCapacityHTTPRespectsSharedQuotaAndAuthority(t *testing.T) {
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
	tenantRef := fmt.Sprintf("capacity-http-%d", integrationIdentitySequence.Add(1))
	controller, application := bootstrapPersistedHTTPAuthority(t, operator, server.URL, server.Client(), tenantRef, "", now, tenantRef)
	response := applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-usage?subjectRef=other", application.BearerToken, tenantRef, "same-local-subject", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("capacity response = %d: %s", response.StatusCode, readResponse(t, response))
	}
	var capacity secondboxclient.SubjectCapacity
	decodeResponseJSON(t, response, &capacity)
	if capacity.SubjectRef != "same-local-subject" || capacity.Limits.MaxSandboxes != 10 || capacity.Available.Sandboxes != 10 || !capacity.ObservedAt.Equal(now) {
		t.Fatalf("capacity = %+v", capacity)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Persist terminal and live sessions around the observation-time boundary.
	for index, session := range []struct {
		state     string
		expiresAt time.Time
	}{
		{"open", now.Add(-time.Second)},
		{"closing", now.Add(-time.Second)},
		{"open", now},
		{"closing", now},
		{"open", now.Add(time.Second)},
		{"closing", now.Add(time.Second)},
		{"closed", now.Add(time.Second)},
	} {
		id := fmt.Sprintf("%s-port-%d", tenantRef, index)
		if _, err := pool.Exec(t.Context(), `INSERT INTO secondbox.port_sessions (
			id,tenant_ref,subject_ref,sandbox_id,profile_revision_id,data_plane_session_id,
			lease_id,generation,name,guest_port,protocol,stream_window_bytes,client_credit_bytes,
			client_bytes,runner_bytes,state,idempotency_key,request_hash,expires_at,created_at,updated_at,acknowledged_inbound_sequence
		) VALUES ($1,$2,'same-local-subject','capacity-sandbox','profile','session','lease',1,
			'web',8080,'tcp',1024,0,0,0,$3,$1,$1,$4,$5,$5,0)`, id, tenantRef, session.state, session.expiresAt, now); err != nil {
			t.Fatal(err)
		}
	}
	response = applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-usage", application.BearerToken, tenantRef, "same-local-subject", "", nil)
	decodeResponseJSON(t, response, &capacity)
	if capacity.Usage.PortSessions != 2 || capacity.Available.PortSessions != capacity.Limits.MaxPortSessions-2 || !capacity.ObservedAt.Equal(now) {
		t.Fatalf("port expiry accounting = %+v", capacity)
	}
	// Model shared-tenant admission pressure independently of the Subject ceiling.
	if _, err := pool.Exec(t.Context(), `UPDATE secondbox.tenant_quotas SET max_sandboxes=2,max_active_instances=3,max_vcpu_count=4,max_memory_bytes=5,max_snapshots=6,max_port_sessions=7,max_concurrent_operations=8 WHERE tenant_ref=$1`, tenantRef); err != nil {
		t.Fatal(err)
	}
	response = applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-usage", application.BearerToken, tenantRef, "same-local-subject", "", nil)
	decodeResponseJSON(t, response, &capacity)
	if capacity.Available.Sandboxes != 2 || capacity.Available.ActiveInstances != 3 || capacity.Available.VcpuCount != 4 || capacity.Available.MemoryBytes != 5 || capacity.Available.Snapshots != 6 || capacity.Available.PortSessions != 5 || capacity.Available.ConcurrentOperations != 8 {
		t.Fatalf("shared admission = %+v", capacity)
	}
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-usage", application.BearerToken, tenantRef, "other", "", nil), http.StatusForbidden)
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodGet, server.URL+"/v1/subject-usage", application.BearerToken, "other", "same-local-subject", "", nil), http.StatusForbidden)
	assertHTTPStatusAndClose(t, applicationRequest(t, http.MethodGet, server.URL+"/v1/usage", application.BearerToken, tenantRef, "same-local-subject", "", nil), http.StatusUnauthorized)
	assertHTTPStatusAndClose(t, bearerRequest(t, http.MethodGet, server.URL+"/v1/usage", controller.BearerToken), http.StatusOK)
}
