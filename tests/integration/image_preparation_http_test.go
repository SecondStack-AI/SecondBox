package integration_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

func TestPrepareImageHTTPResolvesOnceAndWarmsWithoutSandbox(t *testing.T) {
	for _, selectProfile := range []bool{false, true} {
		t.Run(fmt.Sprintf("profile-%t", selectProfile), func(t *testing.T) {
			fixture := newTeardownFixture(t)
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.tenants SET allowed_profile_grants_json=jsonb_build_array($2::text) WHERE ref=$1`, fixture.tenantRef, fixture.profileName); err != nil {
				t.Fatal(err)
			}
			request := contracts.PrepareImageRequest{Image: contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:stable"}}
			if selectProfile {
				request.Profile = fixture.profileName
			}
			response := authenticatedJSONRequest(t, http.MethodPost, fixture.server+"/v1/images:prepare", fixture.credential, "prepare-once", request)
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("prepare: %d %s", response.StatusCode, readResponse(t, response))
			}
			var operation contracts.Operation
			decodeResponseJSON(t, response, &operation)
			spec := testProfileSpec(1)
			runtime, err := (multirunnerAssetCatalog{}).Resolve(spec.RuntimeBundleDigest)
			if err != nil {
				t.Fatal(err)
			}
			toolchain, err := (multirunnerAssetCatalog{}).Resolve(spec.ToolchainBundleDigest)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := json.Marshal(map[string]any{"architecture": "amd64", "guestProtocol": map[string]int{"minimum": 1, "maximum": 1}, "runtimeBundle": runtime, "toolchainBundle": toolchain})
			if err != nil {
				t.Fatal(err)
			}
			key, err := integrationImagePublisher()
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(manifest)
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			// The only eligible Runner resolves the reference, and that one fetch
			// is its preparation: it must not receive a second digest command.
			var payload []byte
			if err := fixture.pool.QueryRow(t.Context(), `SELECT payload FROM secondbox.runner_commands WHERE id=$1`, operation.ID+"-resolve").Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var envelope runnerv1.ControlPlaneToRunner
			if err := proto.Unmarshal(payload, &envelope); err != nil {
				t.Fatal(err)
			}
			command := envelope.GetPrepareImage()
			if command == nil || command.Reference != request.Image.Reference || command.TenantRef != fixture.tenantRef {
				t.Fatalf("preparation command: %v", command)
			}
			sequence := fixture.nextSequence()
			fixture.recordEvent(t, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: fmt.Sprintf("prepare-result-%d", sequence), Sequence: sequence, OperationId: command.OperationId, ResolvedDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Manifest: manifest, Signature: signature}}})
			if _, err := fixture.reconciler.PrepareImages(t.Context(), time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			response = authenticatedJSONRequest(t, http.MethodPost, fixture.server+"/v1/images:prepare", fixture.credential, "prepare-once", request)
			if response.StatusCode != http.StatusAccepted || response.Header.Get("Idempotency-Replayed") != "true" {
				t.Fatalf("replay: %d %s", response.StatusCode, readResponse(t, response))
			}
			var replay contracts.Operation
			decodeResponseJSON(t, response, &replay)
			if replay.ID != operation.ID || replay.State != "succeeded" || replay.ImagePreparation == nil || replay.ImagePreparation.PreparedRunners != 1 || replay.ImagePreparation.TargetRunners != 1 {
				t.Fatalf("preparation replay: %+v", replay)
			}
			var sandboxes, effects, fetches int
			if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.sandboxes WHERE tenant_ref=$1`, fixture.tenantRef).Scan(&sandboxes); err != nil {
				t.Fatal(err)
			}
			if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.lifecycle_effects WHERE assignment_id=$1`, operation.ID).Scan(&effects); err != nil {
				t.Fatal(err)
			}
			if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.runner_commands WHERE kind='prepare-image' AND id LIKE $1||'%'`, operation.ID).Scan(&fetches); err != nil {
				t.Fatal(err)
			}
			if sandboxes != 0 || effects != 2 || fetches != 1 {
				t.Fatalf("preparation allocated sandboxes=%d, effects=%d, or repeated fetches=%d", sandboxes, effects, fetches)
			}
		})
	}
}

// preparationFixture drives one images:prepare Operation across several eligible
// Runners through the public API and the Runner reporting path.
type preparationFixture struct {
	fixture   *teardownFixture
	manifest  []byte
	signature []byte
}

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	fixture := newTeardownFixture(t)
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.tenants SET allowed_profile_grants_json=jsonb_build_array($2::text) WHERE ref=$1`, fixture.tenantRef, fixture.profileName); err != nil {
		t.Fatal(err)
	}
	spec := testProfileSpec(1)
	runtime, err := (multirunnerAssetCatalog{}).Resolve(spec.RuntimeBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	toolchain, err := (multirunnerAssetCatalog{}).Resolve(spec.ToolchainBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"architecture": "amd64", "guestProtocol": map[string]int{"minimum": 1, "maximum": 1}, "runtimeBundle": runtime, "toolchainBundle": toolchain})
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrationImagePublisher()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return &preparationFixture{fixture: fixture, manifest: manifest, signature: signature}
}

// effectRunner reports which Runner holds one preparation effect, so the test
// follows the captured target set instead of assuming a Runner ordering.
func (preparation *preparationFixture) effectRunner(t *testing.T, effectID string) string {
	t.Helper()
	var runnerID string
	if err := preparation.fixture.pool.QueryRow(t.Context(), `SELECT runner_id FROM secondbox.lifecycle_effects WHERE id=$1`, effectID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	return runnerID
}

type queuedPreparation struct {
	effectID string
	runnerID string
}

func (preparation *preparationFixture) queuedPreparations(t *testing.T, operationID string) []queuedPreparation {
	t.Helper()
	rows, err := preparation.fixture.pool.Query(t.Context(), `SELECT id,runner_id FROM secondbox.lifecycle_effects WHERE assignment_id=$1 AND kind='prepare_image' AND state='queued' ORDER BY id`, operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var queued []queuedPreparation
	for rows.Next() {
		var entry queuedPreparation
		if err := rows.Scan(&entry.effectID, &entry.runnerID); err != nil {
			t.Fatal(err)
		}
		queued = append(queued, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return queued
}

func (preparation *preparationFixture) report(t *testing.T, runnerID, connectionID, effectID, failure string) {
	t.Helper()
	sequence := preparation.fixture.nextSequence()
	result := &runnerv1.PrepareImageResult{MessageId: fmt.Sprintf("prepare-result-%d", sequence), Sequence: sequence, OperationId: effectID}
	if failure == "" {
		result.ResolvedDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		result.Manifest = preparation.manifest
		result.Signature = preparation.signature
	} else {
		result.Failure = failure
	}
	preparation.fixture.recordRunnerEvent(t, runnerID, connectionID, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: result}})
	if _, err := preparation.fixture.reconciler.PrepareImages(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func (preparation *preparationFixture) operation(t *testing.T, operationID string) contracts.Operation {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, preparation.fixture.server+"/v1/operations/"+operationID, nil)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, request, preparation.fixture.credential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("operation read: %d %s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	return operation
}

func TestPrepareImageFetchesEachTargetRunnerOnce(t *testing.T) {
	preparation := newPreparationFixture(t)
	fixture := preparation.fixture
	secondRunner, secondConnection := fixture.addEligibleRunner(t, "second")
	request := contracts.PrepareImageRequest{Image: contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:stable"}}
	response := authenticatedJSONRequest(t, http.MethodPost, fixture.server+"/v1/images:prepare", fixture.credential, "prepare-each-once", request)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("prepare: %d %s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	resolveRunner := preparation.effectRunner(t, operation.ID+"-resolve")
	resolveConnection := fixture.connectionID
	if resolveRunner == secondRunner {
		resolveConnection = secondConnection
	}
	preparation.report(t, resolveRunner, resolveConnection, operation.ID+"-resolve", "")
	// The resolve Runner already holds the verified bytes: only the remaining
	// target receives a digest fetch.
	var fetches int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.runner_commands WHERE kind='prepare-image' AND runner_id=$1 AND id LIKE $2||'%'`, resolveRunner, operation.ID).Scan(&fetches); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("resolve Runner received %d fetches", fetches)
	}
	var remaining string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT id FROM secondbox.lifecycle_effects WHERE assignment_id=$1 AND kind='prepare_image' AND state='queued'`, operation.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	pendingRunner, pendingConnection := secondRunner, secondConnection
	if resolveRunner == secondRunner {
		pendingRunner, pendingConnection = fixture.runnerID, fixture.connectionID
	}
	if running := preparation.operation(t, operation.ID); running.State != contracts.OperationStateRunning ||
		running.ImagePreparation.TargetRunners != 2 || running.ImagePreparation.PreparedRunners != 1 {
		t.Fatalf("partially prepared Operation: %s %+v", running.State, running.ImagePreparation)
	}
	preparation.report(t, pendingRunner, pendingConnection, remaining, "")
	prepared := preparation.operation(t, operation.ID)
	if prepared.State != "succeeded" || prepared.ImagePreparation.TargetRunners != 2 ||
		prepared.ImagePreparation.PreparedRunners != 2 ||
		prepared.ImagePreparation.Image.ResolvedDigest == "" {
		t.Fatalf("prepared Operation: %s %+v", prepared.State, prepared.ImagePreparation)
	}
}

func TestPrepareImageReportsPerTargetPreparation(t *testing.T) {
	preparation := newPreparationFixture(t)
	fixture := preparation.fixture
	connections := map[string]string{fixture.runnerID: fixture.connectionID}
	for _, suffix := range []string{"second", "third"} {
		runnerID, connectionID := fixture.addEligibleRunner(t, suffix)
		connections[runnerID] = connectionID
	}
	request := contracts.PrepareImageRequest{Image: contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:stable"}}
	response := authenticatedJSONRequest(t, http.MethodPost, fixture.server+"/v1/images:prepare", fixture.credential, "prepare-per-target", request)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("prepare: %d %s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	// Admission captured the target set, so the first poll already reports it.
	early := preparation.operation(t, operation.ID)
	if early.State != contracts.OperationStatePending || early.ImagePreparation == nil ||
		early.ImagePreparation.TargetRunners != 3 || early.ImagePreparation.PreparedRunners != 0 {
		t.Fatalf("early preparation poll: %s %+v", early.State, early.ImagePreparation)
	}
	resolveRunner := preparation.effectRunner(t, operation.ID+"-resolve")
	preparation.report(t, resolveRunner, connections[resolveRunner], operation.ID+"-resolve", "")
	remaining := preparation.queuedPreparations(t, operation.ID)
	if len(remaining) != 2 {
		t.Fatalf("queued preparations = %v", remaining)
	}
	failedEffect, failedRunner := remaining[0].effectID, remaining[0].runnerID
	preparation.report(t, failedRunner, connections[failedRunner], failedEffect, "registry rejected the digest")
	// One failed target must not decide the Operation while another is pending.
	waiting := preparation.operation(t, operation.ID)
	if waiting.State != contracts.OperationStateRunning {
		t.Fatalf("Operation decided before every target reported: %s %+v", waiting.State, waiting.Error)
	}
	preparation.report(t, remaining[1].runnerID, connections[remaining[1].runnerID], remaining[1].effectID, "")
	decided := preparation.operation(t, operation.ID)
	if decided.State != "failed" || decided.Error == nil ||
		decided.ImagePreparation.TargetRunners != 3 || decided.ImagePreparation.PreparedRunners != 2 {
		t.Fatalf("partially prepared Operation: %s %+v %+v", decided.State, decided.Error, decided.ImagePreparation)
	}
	if !strings.Contains(decided.Error.Title, failedRunner) {
		t.Fatalf("Operation error does not name the failed Runner: %q", decided.Error.Title)
	}
}

func TestPrepareImageRejectsEligibleRunnerSetAboveItsLimit(t *testing.T) {
	preparation := newPreparationFixture(t)
	fixture := preparation.fixture
	for index := 0; index < imagepreparation.MaximumTargets; index++ {
		seedFixtureHomeRunner(t, fixture.poolName, fmt.Sprintf("%s-bulk-%02d", fixture.runnerID, index))
	}
	request := contracts.PrepareImageRequest{Image: contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:stable"}}
	response := authenticatedJSONRequest(t, http.MethodPost, fixture.server+"/v1/images:prepare", fixture.credential, "prepare-above-limit", request)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("prepare above the target limit: %d %s", response.StatusCode, readResponse(t, response))
	}
	var problem contracts.Problem
	decodeResponseJSON(t, response, &problem)
	if problem.Code != "image_preparation_targets_exceeded" || problem.Retryable ||
		!strings.Contains(strings.ToLower(problem.Title), "profile") {
		t.Fatalf("problem = %+v", problem)
	}
}
