package integration_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestPublicSnapshotCreateDeleteAreAsyncIdempotentAndProviderNeutral(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t,
		controlPlane,
		admin,
		"snapshot-http",
	)
	profile := createGrantedProfile(
		t,
		controlPlane,
		databaseStore,
		admin,
		account,
		"profile-snapshot-http",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(),
		principal,
		"snapshot-http-create-sandbox",
		contracts.CreateSandboxRequest{
			Profile:  profile.Name,
			Metadata: map[string]string{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const readyRevision = int64(7)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET state='ready',
		    mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',
		    local_receipt_json='{"durable":true}',updated_at=$2
		WHERE id=(SELECT workspace_id FROM secondbox.sandboxes WHERE id=$1)`,
		sandbox.ID,
		time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='stopped',desired_state='stopped',current_instance_id='',
		    reconcile_owner='',revision=$3,updated_at=$2
		WHERE id=$1`,
		sandbox.ID,
		time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC),
		readyRevision,
	); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: controlPlane, PlatformToken: testPlatformToken,
		Logger:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	createResponse := lifecycleHTTPRequest(
		t,
		server.URL,
		credential,
		http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		"snapshot-http-create",
		strconv.FormatInt(readyRevision, 10),
		"",
		contracts.CreateSnapshotRequest{
			Name: "before-upgrade",
			Metadata: map[string]string{
				"purpose": "integration",
			},
		},
	)
	createBody := readSnapshotHTTPBody(t, createResponse, http.StatusAccepted, false)
	var createOperation contracts.Operation
	if err := json.Unmarshal(createBody, &createOperation); err != nil {
		t.Fatal(err)
	}
	if createOperation.State != contracts.OperationStatePending ||
		createOperation.Snapshot == nil ||
		createOperation.Snapshot.State != "creating" ||
		createOperation.Snapshot.SizeBytes != 8<<30 {
		t.Fatalf("Snapshot create Operation = %#v", createOperation)
	}
	assertSnapshotJSONProviderNeutral(t, createBody)

	replayResponse := lifecycleHTTPRequest(
		t,
		server.URL,
		credential,
		http.MethodPost,
		"/v1/sandboxes/"+sandbox.ID+"/snapshots",
		"snapshot-http-create",
		strconv.FormatInt(readyRevision, 10),
		"",
		contracts.CreateSnapshotRequest{
			Name: "before-upgrade",
			Metadata: map[string]string{
				"purpose": "integration",
			},
		},
	)
	replayBody := readSnapshotHTTPBody(t, replayResponse, http.StatusAccepted, true)
	var replayedCreate contracts.Operation
	if err := json.Unmarshal(replayBody, &replayedCreate); err != nil {
		t.Fatal(err)
	}
	if replayedCreate.ID != createOperation.ID {
		t.Fatalf(
			"Snapshot create replay Operation = %q, want %q",
			replayedCreate.ID,
			createOperation.ID,
		)
	}

	snapshotID := createOperation.Snapshot.ID
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.snapshots
		SET state='ready',runner_receipt_json='{"durable":true}',updated_at=$2
		WHERE id=$1`,
		snapshotID,
		time.Date(2026, 7, 28, 12, 2, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',updated_at=$2
		WHERE id=(SELECT workspace_id FROM secondbox.snapshots WHERE id=$1)`,
		snapshotID,
		time.Date(2026, 7, 28, 12, 2, 0, 0, time.UTC),
	); err != nil {
		t.Fatal(err)
	}
	getResponse := lifecycleHTTPRequest(
		t,
		server.URL,
		credential,
		http.MethodGet,
		"/v1/snapshots/"+snapshotID,
		"",
		"",
		"",
		nil,
	)
	getBody := readSnapshotHTTPBody(t, getResponse, http.StatusOK, false)
	assertSnapshotJSONProviderNeutral(t, getBody)
	var snapshot contracts.Snapshot
	if err := json.Unmarshal(getBody, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != snapshotID || snapshot.State != "ready" {
		t.Fatalf("GET Snapshot = %#v", snapshot)
	}

	deleteResponse := lifecycleHTTPRequest(
		t,
		server.URL,
		credential,
		http.MethodDelete,
		"/v1/snapshots/"+snapshotID,
		"snapshot-http-delete",
		"",
		"",
		nil,
	)
	deleteBody := readSnapshotHTTPBody(t, deleteResponse, http.StatusAccepted, false)
	assertSnapshotJSONProviderNeutral(t, deleteBody)
	var deleteOperation contracts.Operation
	if err := json.Unmarshal(deleteBody, &deleteOperation); err != nil {
		t.Fatal(err)
	}
	if deleteOperation.State != contracts.OperationStatePending ||
		deleteOperation.Snapshot == nil ||
		deleteOperation.Snapshot.State != "deleting" {
		t.Fatalf("Snapshot delete Operation = %#v", deleteOperation)
	}
	deleteReplay := lifecycleHTTPRequest(
		t,
		server.URL,
		credential,
		http.MethodDelete,
		"/v1/snapshots/"+snapshotID,
		"snapshot-http-delete",
		"",
		"",
		nil,
	)
	deleteReplayBody := readSnapshotHTTPBody(
		t,
		deleteReplay,
		http.StatusAccepted,
		true,
	)
	var replayedDelete contracts.Operation
	if err := json.Unmarshal(deleteReplayBody, &replayedDelete); err != nil {
		t.Fatal(err)
	}
	if replayedDelete.ID != deleteOperation.ID {
		t.Fatalf(
			"Snapshot delete replay Operation = %q, want %q",
			replayedDelete.ID,
			deleteOperation.ID,
		)
	}
}

func readSnapshotHTTPBody(
	t *testing.T,
	response *http.Response,
	status int,
	replayed bool,
) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf(
			"Snapshot HTTP status=%d want=%d body=%s",
			response.StatusCode,
			status,
			body,
		)
	}
	if status == http.StatusAccepted &&
		response.Header.Get("Idempotency-Replayed") != strconv.FormatBool(replayed) {
		t.Fatalf(
			"Snapshot HTTP replay=%q want=%t body=%s",
			response.Header.Get("Idempotency-Replayed"),
			replayed,
			body,
		)
	}
	return body
}

func assertSnapshotJSONProviderNeutral(t *testing.T, body []byte) {
	t.Helper()
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{
		"workspaceid",
		"homerunner",
		"sha256",
		"storagekey",
		"hostpath",
		"backend",
		"firecracker",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("Snapshot response exposes %q: %s", forbidden, body)
		}
	}
}
