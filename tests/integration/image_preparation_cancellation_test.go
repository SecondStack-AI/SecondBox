package integration_test

import (
	"net/http"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestQueuedStartCancellationReleasesImagePreparationAndHistoricalDeletedWork(t *testing.T) {
	for _, deliveryState := range []string{"pending", "delivering", "delivered"} {
		for _, action := range []string{"stop", "delete"} {
			t.Run(deliveryState+"-"+action, func(t *testing.T) {
				fixture := newTeardownFixture(t)
				task4CompleteWorkspaceReconciliation(t, fixture.stateStore, fixture.runnerID, fixture.connectionID, 2, nil, time.Now().UTC())
				fixture.sequence = 2
				setFixtureProfileInitialState(t, fixture, contracts.SandboxDesiredStateRunning)
				created, _, err := fixture.controlPlane.CreateSandboxOperation(t.Context(), fixture.principal, "cancel-image-create", contracts.CreateSandboxRequest{Image: testExecutionImage(), Profile: fixture.profileName, Metadata: map[string]string{}})
				if err != nil {
					t.Fatal(err)
				}
				fixture.completeWorkspaceCreate(t, created.SandboxID, created.ID)
				fixture.deferOtherSandboxes(t, created.SandboxID)
				decision, found, err := fixture.reconciler.RunOnce(t.Context(), time.Now().UTC(), ports.LifecycleWakeTriggerNotify)
				if err != nil || !found || decision.Action != lifecycle.ActionStartInstance {
					t.Fatalf("actual preparation decision=%#v found=%t err=%v", decision, found, err)
				}
				command := fixture.pendingCommand(t, created.SandboxID, "prepare-image").GetPrepareImage()
				if command == nil || command.OperationId != created.ID {
					t.Fatalf("actual image preparation=%#v", command)
				}
				if deliveryState != "pending" {
					delivery, found, err := fixture.stateStore.ClaimCommand(t.Context(), fixture.runnerID, fixture.connectionID, time.Now().UTC())
					if err != nil || !found || delivery.Message.GetPrepareImage() == nil {
						t.Fatalf("prepare delivery=%#v found=%t err=%v", delivery, found, err)
					}
					if deliveryState == "delivered" {
						if err := fixture.stateStore.MarkCommandDelivered(t.Context(), delivery, fixture.connectionID, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
					}
				}
				current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, created.SandboxID)
				if err != nil {
					t.Fatal(err)
				}
				method, path := http.MethodPost, "/v1/sandboxes/"+created.SandboxID+":stop"
				if action == "delete" {
					method, path = http.MethodDelete, "/v1/sandboxes/"+created.SandboxID
				}
				response := quiescenceHTTPRequest(t, fixture, method, path, "cancel-image-"+action, current.Revision, nil)
				if response.StatusCode != http.StatusAccepted {
					t.Fatalf("cancel status=%d body=%s", response.StatusCode, readResponse(t, response))
				}
				response.Body.Close()
				var effectState, commandState string
				assertCancelled := func() {
					t.Helper()
					if err := fixture.pool.QueryRow(t.Context(), `SELECT effect.state,command.state FROM secondbox.lifecycle_effects effect JOIN secondbox.runner_commands command ON command.id=effect.command_id WHERE effect.id=$1`, command.OperationId).Scan(&effectState, &commandState); err != nil {
						t.Fatal(err)
					}
					if effectState != "failed" || commandState != "expired" {
						t.Fatalf("cancelled cache preparation effect=%q command=%q", effectState, commandState)
					}
					if sandboxAssignmentCount(t, fixture, created.SandboxID) != 0 {
						t.Fatal("image cancellation created compute")
					}
				}
				assertCancelled()
				if action == "stop" {
					current, err = fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, created.SandboxID)
					if err != nil {
						t.Fatal(err)
					}
					response = quiescenceHTTPRequest(t, fixture, http.MethodDelete, "/v1/sandboxes/"+created.SandboxID, "cancel-image-delete-after-stop", current.Revision, nil)
					if response.StatusCode != http.StatusAccepted {
						t.Fatalf("delete status=%d body=%s", response.StatusCode, readResponse(t, response))
					}
					response.Body.Close()
				}
				driveStoppedDeleteToCompletion(t, fixture, created.SandboxID)
				// Reproduce the orphan left by a control plane that deleted the Workspace without cancelling its cache fetch.
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.lifecycle_effects SET state='queued',failure_class='',failure_message='' WHERE id=$1`, command.OperationId); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runner_commands SET state=$2 WHERE assignment_id=$1 AND kind='prepare-image'`, command.OperationId, deliveryState); err != nil {
					t.Fatal(err)
				}
				if changed, err := fixture.reconciler.PrepareImages(t.Context(), time.Now().UTC()); err != nil || !changed {
					t.Fatalf("historical preparation cleanup changed=%t err=%v", changed, err)
				}
				assertCancelled()
				sequence := fixture.nextSequence()
				fixture.recordEvent(t, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: "late-cache-" + command.OperationId, Sequence: sequence, OperationId: command.OperationId, Failure: "late cache fetch failure"}}})
				assertCancelled()
				fleet, err := fixture.store.ReadRunnerFleet(t.Context(), fixture.poolName, time.Now().UTC())
				if err != nil || len(fleet.Runners) != 1 || fleet.Runners[0].PendingCommands != 0 || fleet.Runners[0].WorkspaceCount != 0 || fleet.Runners[0].ActiveAssignments != 0 {
					t.Fatalf("deleted image preparation still blocks fleet=%#v err=%v", fleet, err)
				}
			})
		}
	}
}
