package integration_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
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
			for _, stage := range []string{"resolve", "prepare-0"} {
				var payload []byte
				if err := fixture.pool.QueryRow(t.Context(), `SELECT payload FROM secondbox.runner_commands WHERE id=$1`, operation.ID+"-"+stage).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var envelope runnerv1.ControlPlaneToRunner
				if err := proto.Unmarshal(payload, &envelope); err != nil {
					t.Fatal(err)
				}
				command := envelope.GetPrepareImage()
				want := request.Image.Reference
				if stage != "resolve" {
					want = testExecutionImage().Reference
				}
				if command == nil || command.Reference != want || command.TenantRef != fixture.tenantRef {
					t.Fatalf("preparation command: %v", command)
				}
				sequence := fixture.nextSequence()
				fixture.recordEvent(t, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: fmt.Sprintf("prepare-result-%d", sequence), Sequence: sequence, OperationId: command.OperationId, ResolvedDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Manifest: manifest, Signature: signature}}})
				if _, err := fixture.reconciler.PrepareImages(t.Context(), time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
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
			var sandboxes, commands int
			if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.sandboxes WHERE tenant_ref=$1`, fixture.tenantRef).Scan(&sandboxes); err != nil {
				t.Fatal(err)
			}
			if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.lifecycle_effects WHERE assignment_id=$1`, operation.ID).Scan(&commands); err != nil {
				t.Fatal(err)
			}
			if sandboxes != 0 || commands != 2 {
				t.Fatalf("preparation allocated sandboxes=%d or repeated commands=%d", sandboxes, commands)
			}
		})
	}
}
