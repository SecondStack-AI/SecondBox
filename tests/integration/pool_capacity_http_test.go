package integration_test

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/store"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

// TestPoolCapacityIsScopedToPlaceablePoolsAndNamesNoRunner covers the
// observation each authority reads. An application authority sees the pools
// of its enabled granted Profiles and of its Subject's Sandboxes, a tenant
// controller the same over the Tenant ceiling and every Sandbox of the Tenant,
// and the platform every pool. Figures aggregate the placeable Runners only,
// count connected storage-latched Runners, count Sandboxes waiting to be
// placed in the pool, and never carry a Runner identity.
func TestPoolCapacityIsScopedToPlaceablePoolsAndNamesNoRunner(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	databaseStore, err := store.NewPostgresControlPlaneStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(databaseStore.Close)
	database, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(database.Close)
	controlPlane := newManagementControlPlane(t, databaseStore, now)
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	t.Cleanup(server.Close)
	admin := fixtureAdmin(t, controlPlane)

	sequence := integrationIdentitySequence.Add(1)
	granted := fmt.Sprintf("capacity-granted-%d", sequence)
	ceiling := fmt.Sprintf("capacity-ceiling-%d", sequence)
	disabled := fmt.Sprintf("capacity-disabled-%d", sequence)
	foreign := fmt.Sprintf("capacity-foreign-%d", sequence)
	runnerIDs := map[string]string{}
	profiles := map[string]contracts.Profile{}
	for _, name := range []string{granted, ceiling, disabled, foreign} {
		if err := databaseStore.RegisterRunnerPool(t.Context(), contracts.RunnerPool{
			Name: name, State: contracts.RunnerPoolStateReady,
			Architectures: []string{"amd64"}, Capabilities: []string{"compute", "local-workspace"},
			ReadyRunnerCount: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		runnerIDs[name] = "runner-" + name
		seedFixtureHomeRunner(t, name, runnerIDs[name])
		spec := testProfileSpec(1)
		spec.Pool = name
		profile, _, err := controlPlane.CreateProfileIdempotent(
			t.Context(), admin, "create-"+name, contracts.CreateProfileRequest{Name: name, Spec: spec},
		)
		if err != nil {
			t.Fatal(err)
		}
		profiles[name] = profile
	}
	// A disabled Profile admits no create, so its pool is not one the caller
	// can place into.
	if _, _, err := controlPlane.DisableProfileAtRevisionIdempotent(
		t.Context(), admin, disabled, "disable-"+disabled, profiles[disabled].Revision,
	); err != nil {
		t.Fatal(err)
	}

	operator, err := secondboxclient.NewSecondBoxClient(server.URL, testPlatformToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	tenantRequest := persistedHTTPTenantRequest(secondboxclient.OwnershipRef(newFixtureID("capacity-tenant")))
	tenantRequest.AllowedProfileGrants = []string{granted, ceiling, disabled}
	tenant, err := operator.CreateTenant(t.Context(), tenantRequest, "capacity-tenant-create")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Hour)
	controller, err := operator.CreateTenantControllerAuthority(t.Context(), tenant.Ref, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: expiresAt, Metadata: map[string]string{},
	}, "capacity-controller-create")
	if err != nil {
		t.Fatal(err)
	}
	controllerClient, err := secondboxclient.NewSecondBoxTenantControllerClient(server.URL, controller.BearerToken, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	subject, err := controllerClient.CreateSubject(t.Context(), secondboxclient.CreateSubjectRequest{
		Ref: "capacity-subject", Metadata: map[string]string{}, ExpiresAt: &expiresAt,
		Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 4, MaxActiveInstances: 4, MaxVcpuCount: 4, MaxMemoryBytes: 4 << 30,
			MaxSnapshots: 2, MaxPortSessions: 2, MaxConcurrentOperations: 2,
		},
	}, "capacity-subject-create")
	if err != nil {
		t.Fatal(err)
	}
	application, err := controllerClient.CreateApplicationAuthority(t.Context(), secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef: subject.Ref, Scopes: []string{"sandbox:read", "sandbox:lifecycle"},
		ProfileGrants: []string{granted, disabled},
		Metadata:      map[string]string{}, ExpiresAt: expiresAt,
	}, "capacity-application-create")
	if err != nil {
		t.Fatal(err)
	}
	applicationClient, err := secondboxclient.NewSecondBoxSubjectClient(
		server.URL, application.BearerToken, string(tenant.Ref), string(subject.Ref), server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Three Sandboxes of this Subject in the granted pool: an admitted start
	// still queued, an automatic restart that holds no mutation, and a failed
	// Sandbox parked on a failure class, which waits for an operator.
	sandboxIDs := make([]string, 0, 3)
	for index := range 3 {
		_, created, err := applicationClient.CreateSandbox(t.Context(), secondboxclient.CreateSandboxRequest{
			Profile: granted, Metadata: map[string]string{},
		}, fmt.Sprintf("capacity-pending-create-%d", index))
		if err != nil {
			t.Fatal(err)
		}
		sandboxIDs = append(sandboxIDs, created.SandboxID)
	}
	for index, shape := range []struct{ state, failureClass, mutationKind, mutationState string }{
		{"stopped", "", "start", "queued"},
		{"stopped", "", "", ""},
		{"failed", "compatibility", "", ""},
	} {
		if _, err := database.Exec(t.Context(), `
			WITH waiting AS (
				UPDATE secondbox.sandboxes
				SET state=$2,desired_state='running',lifecycle_failure_class=$3
				WHERE id=$1 RETURNING workspace_id
			)
			UPDATE secondbox.workspaces
			SET state='ready',mutation_kind=$4,mutation_state=$5,mutation_id='',mutation_effect_id='',
			    mutation_operation_id='',mutation_expected_generation=NULL,mutation_target_generation=NULL
			WHERE id=(SELECT workspace_id FROM waiting)`,
			sandboxIDs[index], shape.state, shape.failureClass, shape.mutationKind, shape.mutationState,
		); err != nil {
			t.Fatal(err)
		}
	}
	// The platform places one of this Subject's Sandboxes on a Profile outside
	// both the grants and the Tenant ceiling; its pool is still the caller's.
	if _, _, err := controlPlane.CreateSandboxOperation(t.Context(), contracts.Principal{
		Kind: contracts.AuthorityKindPlatform, ID: "platform",
		TenantRef: string(tenant.Ref), SubjectRef: string(subject.Ref),
	}, "capacity-platform-create", contracts.CreateSandboxRequest{Profile: foreign, Metadata: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	// Distinct figures make the placeable sums observable. A connected Runner
	// latched on storage pressure is still placeable here and is counted in
	// both; a disconnected latched Runner is in neither.
	if _, err := database.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET capacity_json='{"VCPUCount":20,"MemoryBytes":40,"DiskBytes":100,"Instances":10,"Operations":10}',
		    reserved_capacity_json='{"VCPUCount":8,"MemoryBytes":16,"DiskBytes":0,"Instances":4,"Operations":4}'
		WHERE id=$1`, runnerIDs[granted],
	); err != nil {
		t.Fatal(err)
	}
	latchedRunnerID := "runner-latched-" + granted
	disconnectedRunnerID := "runner-disconnected-" + granted
	seedFixtureHomeRunner(t, granted, latchedRunnerID)
	seedFixtureHomeRunner(t, granted, disconnectedRunnerID)
	if _, err := database.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET capacity_json='{"VCPUCount":4,"MemoryBytes":8,"DiskBytes":100,"Instances":2,"Operations":2}',
		    storage_pressure_json=jsonb_build_object('status','admission_denied','observedAt',$2::timestamptz),
		    state=CASE WHEN id=$3 THEN 'offline' ELSE state END,
		    active_connection_id=CASE WHEN id=$3 THEN '' ELSE active_connection_id END
		WHERE id IN ($1,$3)`, latchedRunnerID, now, disconnectedRunnerID,
	); err != nil {
		t.Fatal(err)
	}

	wantGranted := secondboxclient.RunnerPoolCapacity{
		Name: granted, State: contracts.RunnerPoolStateReady,
		PlaceableRunnerCount: 2, StorageAdmissionDeniedRunnerCount: 1,
		Allocatable:       secondboxclient.PoolComputeCapacity{Instances: 12, VcpuCount: 24, MemoryBytes: 48, Operations: 12},
		Reserved:          secondboxclient.PoolComputeCapacity{Instances: 4, VcpuCount: 8, MemoryBytes: 16, Operations: 4},
		PendingStartCount: 2,
	}
	applicationView, err := applicationClient.GetPoolCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(poolCapacityNames(applicationView), sortedNames(granted, foreign)) ||
		!applicationView.ObservedAt.Equal(now) {
		t.Fatalf("application pool capacity names = %v, want its granted and Sandbox pools", poolCapacityNames(applicationView))
	}
	if got := poolCapacityByName(t, applicationView, granted); got != wantGranted {
		t.Fatalf("granted pool capacity = %#v, want %#v", got, wantGranted)
	}
	controllerView, err := controllerClient.GetTenantPoolCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if names := poolCapacityNames(controllerView); !slices.Equal(names, sortedNames(granted, ceiling, foreign)) {
		t.Fatalf("tenant-controller pool capacity names = %v, want the enabled ceiling and Tenant Sandbox pools", names)
	}
	platformView, err := operator.GetPoolCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{granted, ceiling, disabled, foreign} {
		if !slices.Contains(poolCapacityNames(platformView), name) {
			t.Fatalf("platform pool capacity omits %s: %v", name, poolCapacityNames(platformView))
		}
	}

	// Each operation admits only its own authority kind, and no response names a Runner.
	for _, check := range []struct {
		name, path, token string
		owned             bool
		status            int
	}{
		{"application", "/v1/pool-capacity", application.BearerToken, true, http.StatusOK},
		{"controller", "/v1/tenant-pool-capacity", controller.BearerToken, false, http.StatusOK},
		{"controller on the application operation", "/v1/pool-capacity", controller.BearerToken, true, http.StatusUnauthorized},
		{"application on the controller operation", "/v1/tenant-pool-capacity", application.BearerToken, false, http.StatusUnauthorized},
	} {
		t.Run(check.name, func(t *testing.T) {
			request := newBearerHTTPRequest(t, http.MethodGet, server.URL+check.path, check.token)
			if check.owned {
				request.Header.Set("X-SecondBox-Tenant-Ref", string(tenant.Ref))
				request.Header.Set("X-SecondBox-Subject-Ref", string(subject.Ref))
			}
			response := doHTTP(t, request)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != check.status {
				t.Fatalf("%s status = %d body=%s", check.path, response.StatusCode, body)
			}
			for _, runnerID := range append(mapValues(runnerIDs), latchedRunnerID, disconnectedRunnerID) {
				if strings.Contains(string(body), runnerID) {
					t.Fatalf("%s response names Runner %s: %s", check.path, runnerID, body)
				}
			}
		})
	}
}

func poolCapacityNames(observation secondboxclient.PoolCapacityObservation) []string {
	names := make([]string, 0, len(observation.Pools))
	for _, pool := range observation.Pools {
		names = append(names, pool.Name)
	}
	return names
}

func poolCapacityByName(
	t *testing.T,
	observation secondboxclient.PoolCapacityObservation,
	name string,
) secondboxclient.RunnerPoolCapacity {
	t.Helper()
	for _, pool := range observation.Pools {
		if pool.Name == name {
			return pool
		}
	}
	t.Fatalf("pool capacity omits %s", name)
	return secondboxclient.RunnerPoolCapacity{}
}

func sortedNames(names ...string) []string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return sorted
}

func mapValues(values map[string]string) []string {
	collected := make([]string, 0, len(values))
	for _, value := range values {
		collected = append(collected, value)
	}
	return collected
}
