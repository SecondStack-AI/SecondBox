package integration_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSandboxStorageObservationHTTPReadsAndListsPersistedEvidence(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(t, controlPlane, admin, "storage-observation-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "storage-observation-http-profile")
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(t.Context(), principal, "storage-observation-http-create", contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, persistedHTTPHandler(t, controlPlane, databaseStore))
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	allocated := int64(4096)
	exclusive := int64(0)
	for _, test := range []struct {
		name   string
		stored *contracts.WorkspaceStorageObservation
		want   contracts.WorkspaceStorageObservation
	}{
		{
			name:   "available with zero exclusive bytes",
			stored: &contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated, ExclusiveBytes: &exclusive},
			want:   contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated, ExclusiveBytes: &exclusive, Pressure: contracts.StoragePressureObservation{Status: "unavailable"}},
		},
		{
			name:   "exclusive unsupported preserves allocated",
			stored: &contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated, ExclusiveReason: "fiemap_unsupported"},
			want:   contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated, ExclusiveReason: "fiemap_unsupported", Pressure: contracts.StoragePressureObservation{Status: "unavailable"}},
		},
		{
			name: "absent",
			want: contracts.WorkspaceStorageObservation{Status: "unavailable", Reason: "not_observed", Pressure: contracts.StoragePressureObservation{Status: "unavailable"}},
		},
		{
			name:   "available without reason",
			stored: &contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated},
			want:   contracts.WorkspaceStorageObservation{Status: "available", ObservedAt: &now, AllocatedBytes: &allocated, Pressure: contracts.StoragePressureObservation{Status: "unavailable"}},
		},
		{
			name:   "unavailable with reason",
			stored: &contracts.WorkspaceStorageObservation{Status: "unavailable", ObservedAt: &now, Reason: "probe_failed"},
			want:   contracts.WorkspaceStorageObservation{Status: "unavailable", ObservedAt: &now, Reason: "probe_failed", Pressure: contracts.StoragePressureObservation{Status: "unavailable"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var encoded []byte
			if test.stored != nil {
				encoded, err = json.Marshal(test.stored)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET storage_observation_json=$2 WHERE id=$1`, sandbox.Workspace.ID, encoded); err != nil {
				t.Fatal(err)
			}
			got := getHTTPSandbox(t, server.URL, credential, sandbox.ID)
			if !reflect.DeepEqual(got.Workspace.StorageObservation, test.want) {
				t.Fatalf("GET observation = %+v, want %+v", got.Workspace.StorageObservation, test.want)
			}
			response := lifecycleHTTPRequest(t, server.URL, credential, http.MethodGet, "/v1/sandboxes", "", "", "", nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("list Sandbox status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var page contracts.SandboxPage
			decodeResponseJSON(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].ID != sandbox.ID || !reflect.DeepEqual(page.Items[0].Workspace.StorageObservation, test.want) {
				t.Fatalf("list observation = %+v, want %+v", page.Items, test.want)
			}
		})
	}
}
