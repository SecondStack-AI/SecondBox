package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// Exercise the real HTTP admission and PostgreSQL reconciliation paths, holding
// Runner evidence at each stop boundary. Runner results use the existing
// protocol fixture; this test does not run compute or mutate a local image.
func TestDeleteDuringStopRequiresRetryAfterGenerationCommit(t *testing.T) {
	for _, trigger := range []string{"explicit_stop", "idle_timeout"} {
		t.Run(trigger, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			sandboxID, _ := fixture.createReadySandbox(t)
			path := "/v1/sandboxes/" + sandboxID
			readyRevision, _ := quiescenceSandboxETag(t, fixture, sandboxID)
			var stopOperationID string
			if trigger == "explicit_stop" {
				response := quiescenceHTTPRequest(t, fixture, http.MethodPost, path+":stop", "stop-before-delete", readyRevision, nil)
				if response.StatusCode != http.StatusAccepted {
					t.Fatalf("stop status=%d body=%s", response.StatusCode, readResponse(t, response))
				}
				var operation contracts.Operation
				decodeResponseJSON(t, response, &operation)
				stopOperationID = operation.ID
			} else {
				// Expire useful activity without waiting for the Profile's idle
				// window. The normal reconciler still decides and commits stop.
				idleOrigin := time.Now().UTC().Add(-time.Duration(testProfileSpec(1).Lifecycle.IdleSeconds+1) * time.Second)
				if _, err := fixture.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET last_activity_at=$2,next_reconcile_at=$2
					WHERE id=$1`, sandboxID, idleOrigin); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.pool.Exec(t.Context(), `
					UPDATE secondbox.instances SET ready_at=$2
					WHERE id=(SELECT current_instance_id FROM secondbox.sandboxes WHERE id=$1)`,
					sandboxID, idleOrigin); err != nil {
					t.Fatal(err)
				}
			}
			fixture.runLifecycle(t, sandboxID, lifecycle.ActionDrain)
			if trigger == "idle_timeout" {
				var reason string
				if err := fixture.pool.QueryRow(t.Context(), `SELECT lifecycle_termination_reason FROM secondbox.sandboxes WHERE id=$1`, sandboxID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != contracts.TerminationReasonIdleTimeout {
					t.Fatalf("automatic stop reason=%q, want idle_timeout", reason)
				}
			}
			fixture.runLifecycle(t, sandboxID, lifecycle.ActionStopInstance)

			// Stale If-Match takes precedence over the mutation-slot conflict.
			response := quiescenceHTTPRequest(t, fixture, http.MethodDelete, path, "delete-after-stop", readyRevision, nil)
			assertProblem(t, response, http.StatusPreconditionFailed, "precondition_failed")

			var heldRevision int64
			for _, phase := range []string{"fence_pending", "generation_receipt_pending", "generation_commit_pending"} {
				if phase == "generation_receipt_pending" {
					fixture.completeFence(t, sandboxID)
				}
				if phase == "generation_commit_pending" {
					fixture.completeGenerationAdvance(t, sandboxID)
				}
				heldRevision, _ = quiescenceSandboxETag(t, fixture, sandboxID)
				before := deleteDuringStopAuthority(t, fixture, sandboxID)
				response := quiescenceHTTPRequest(t, fixture, http.MethodDelete, path, "delete-after-stop", heldRevision, nil)
				if response.StatusCode != http.StatusConflict || response.Header.Get("Content-Type") != "application/problem+json" {
					t.Fatalf("%s delete status=%d body=%s", phase, response.StatusCode, readResponse(t, response))
				}
				var problem contracts.Problem
				decodeResponseJSON(t, response, &problem)
				if problem.Code != "workspace_mutation_conflict" || problem.Retryable {
					t.Fatalf("%s problem=%#v; callers explicitly re-read and retry this state conflict", phase, problem)
				}
				if after := deleteDuringStopAuthority(t, fixture, sandboxID); after != before {
					t.Fatalf("%s rejected delete changed durable authority:\nbefore %s\nafter %s", phase, before, after)
				}
				if stopOperationID != "" {
					operation := deleteDuringStopOperation(t, fixture, stopOperationID)
					if operation.State != contracts.OperationStatePending && operation.State != contracts.OperationStateRunning {
						t.Fatalf("%s stop completed before generation commit: %#v", phase, operation)
					}
				}
			}

			fixture.runLifecycle(t, sandboxID, lifecycle.ActionFinishStop)
			stopped, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if stopped.State != contracts.SandboxStateStopped || stopped.DesiredState != contracts.SandboxDesiredStateStopped || stopped.Generation != 2 {
				t.Fatalf("stop did not commit the next generation: %#v", stopped)
			}
			if stopOperationID != "" {
				if operation := deleteDuringStopOperation(t, fixture, stopOperationID); operation.State != contracts.OperationStateSucceeded {
					t.Fatalf("stop Operation=%#v", operation)
				}
			}
			// A failed attempt did not reserve its key. Refresh If-Match and
			// reuse that key; the accepted delete gets its own durable Operation.
			response = quiescenceHTTPRequest(t, fixture, http.MethodDelete, path, "delete-after-stop", heldRevision, nil)
			assertProblem(t, response, http.StatusPreconditionFailed, "precondition_failed")
			response = quiescenceHTTPRequest(t, fixture, http.MethodDelete, path, "delete-after-stop", stopped.Revision, nil)
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("delete retry status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var deletion contracts.Operation
			decodeResponseJSON(t, response, &deletion)
			if deletion.Kind != "delete" || deletion.State != contracts.OperationStatePending || deletion.ID == stopOperationID {
				t.Fatalf("delete Operation=%#v", deletion)
			}
			fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
			if operation := deleteDuringStopOperation(t, fixture, deletion.ID); operation.State == contracts.OperationStateSucceeded {
				t.Fatalf("delete succeeded without Workspace deletion receipt: %#v", operation)
			}
			fixture.completeWorkspaceDelete(t, sandboxID)
			if operation := deleteDuringStopOperation(t, fixture, deletion.ID); operation.State != contracts.OperationStateSucceeded {
				t.Fatalf("delete Operation=%#v", operation)
			}
			// Replaying an accepted request returns the same Operation despite
			// the now-stale precondition and the terminal Sandbox state.
			response = quiescenceHTTPRequest(t, fixture, http.MethodDelete, path, "delete-after-stop", stopped.Revision, nil)
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("delete replay status=%d body=%s", response.StatusCode, readResponse(t, response))
			}
			var replay contracts.Operation
			decodeResponseJSON(t, response, &replay)
			if replay.ID != deletion.ID || replay.State != contracts.OperationStateSucceeded {
				t.Fatalf("delete replay=%#v", replay)
			}
		})
	}
}

func deleteDuringStopOperation(t *testing.T, fixture *teardownFixture, operationID string) contracts.Operation {
	t.Helper()
	response := quiescenceHTTPRequest(t, fixture, http.MethodGet, "/v1/operations/"+operationID, "", 0, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get Operation status=%d body=%s", response.StatusCode, readResponse(t, response))
	}
	var operation contracts.Operation
	decodeResponseJSON(t, response, &operation)
	return operation
}

// Compare all durable admission authority, including public revision/time,
// stop effect identity, mutation ownership, Operations, and idempotency keys.
// A rejected request must not cancel, supersede, or queue anything.
func deleteDuringStopAuthority(t *testing.T, fixture *teardownFixture, sandboxID string) string {
	t.Helper()
	var authority string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT jsonb_build_object(
		  'sandbox',to_jsonb(sandbox),'workspace',to_jsonb(workspace),
		  'effects',(SELECT jsonb_agg(to_jsonb(effect) ORDER BY id)
		    FROM secondbox.lifecycle_effects effect WHERE sandbox_id=$1),
		  'operations',(SELECT jsonb_agg(to_jsonb(operation) ORDER BY id)
		    FROM secondbox.operations operation WHERE sandbox_id=$1),
		  'idempotency',(SELECT jsonb_agg(to_jsonb(record) ORDER BY operation,idempotency_key)
		    FROM secondbox.idempotency_records record WHERE target_id=$1))::text
		FROM secondbox.sandboxes sandbox
		JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.id=$1`, sandboxID).Scan(&authority); err != nil {
		t.Fatal(err)
	}
	return authority
}
