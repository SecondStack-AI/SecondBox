package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

type startRehomeArchiveProvider struct{}

func (startRehomeArchiveProvider) Transfer(_ context.Context, _ string, _ string, descriptor *runnerv1.WorkspaceArchiveDescriptor) (*runnerv1.WorkspaceArchiveTransfer, error) {
	return &runnerv1.WorkspaceArchiveTransfer{Url: "https://archive.example/workspace", ExpiresAtUnixMs: uint64(time.Now().Add(time.Hour).UnixMilli()), ArchiveDescriptor: descriptor}, nil
}
func (startRehomeArchiveProvider) Delete(context.Context, string) error { return nil }

func pendingStartArchiveCommand(t *testing.T, fixture *teardownFixture, sandboxID string, kind runnerv1.LocalWorkspaceCommandKind) *runnerv1.LocalWorkspaceCommand {
	t.Helper()
	rows, err := fixture.pool.Query(t.Context(), `SELECT c.payload FROM secondbox.runner_commands c JOIN secondbox.workspace_archives a ON a.id=c.assignment_id WHERE a.sandbox_id=$1 AND c.state IN ('pending','delivering','delivered') ORDER BY c.created_at,c.id`, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var envelope runnerv1.ControlPlaneToRunner
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			t.Fatal(err)
		}
		if command := envelope.GetLocalWorkspace(); command != nil && command.Kind == kind {
			return command
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("Sandbox %s has no pending archive command %v", sandboxID, kind)
	return nil
}

func recordStartArchiveReceipt(t *testing.T, fixture *teardownFixture, runner, connection string, sequence uint64, command *runnerv1.LocalWorkspaceCommand, terminal runnerv1.LocalWorkspaceTerminalKind, descriptor *runnerv1.WorkspaceArchiveDescriptor) {
	t.Helper()
	detail := ""
	if terminal != runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED {
		detail = "local workspace operation failed"
	}
	fixture.recordRunnerEvent(t, runner, connection, runnercontrol.EventLocalWorkspace, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_LocalWorkspaceResult{LocalWorkspaceResult: &runnerv1.LocalWorkspaceResult{MessageId: fmt.Sprintf("start-rehome-%s-%d", runner, sequence), Sequence: sequence, CommandVersion: 1, Kind: command.Kind, OperationId: command.OperationId, EffectId: command.EffectId, SandboxId: command.SandboxId, WorkspaceId: command.WorkspaceId, Generation: command.ExpectedGeneration, LogicalCapacityBytes: command.LogicalCapacityBytes, ReceiptRecordedAtUnixMs: uint64(time.Now().UnixMilli()), Terminal: terminal, SafeDetail: detail, ArchiveDescriptor: descriptor, Correlation: proto.Clone(command.Correlation).(*runnerv1.Correlation)}}})
}

func rehomeImageManifest(t *testing.T) []byte {
	t.Helper()
	runtimeComponent, toolchain := selectedImageComponents()
	manifest, err := json.Marshal(map[string]any{"architecture": "amd64", "guestProtocol": map[string]int{"minimum": 1, "maximum": 1}, "runtimeBundle": runtimeComponent, "toolchainBundle": toolchain})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestStoppedStartRehomesFromUnavailableCapacityPreservingOperation(t *testing.T) {
	for _, mode := range []string{"full", "draining", "storage_denied"} {
		t.Run(mode, func(t *testing.T) {
			draining := mode == "draining"
			fixture := newTeardownFixture(t)
			if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, map[string]int64{fixture.profileName: 3600}); err != nil {
				t.Fatal(err)
			}
			sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "rehome")
			target, targetConnection := fixture.addEligibleRunner(t, "rehome-target")
			if mode == "storage_denied" {
				registration := task4Registration(fixture.runnerID, fixture.connectionID, fixture.poolName)
				registration.Sequence = fixture.nextSequence()
				if _, err := fixture.stateStore.RecordRegistration(t.Context(), registration, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET protocol_versions_json='["8"]',capabilities_json=capabilities_json || '["workspace-relocation"]'::jsonb WHERE pool_name=$1`, fixture.poolName); err != nil {
				t.Fatal(err)
			}
			if draining {
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='draining',drain_phase='draining' WHERE id=$1`, fixture.runnerID); err != nil {
					t.Fatal(err)
				}
			} else if mode == "storage_denied" {
				heartbeat := task4Heartbeat(fixture.runnerID, fixture.connectionID, "denied-home-heartbeat", fixture.nextSequence(), runnerv1.DrainPhase_DRAIN_PHASE_ACTIVE)
				heartbeat.StoragePressure = &runnerv1.StoragePressureObservation{Status: "admission_denied", ObservedAtUnixMs: uint64(time.Now().UnixMilli())}
				if _, err := fixture.stateStore.RecordHeartbeat(t.Context(), heartbeat, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json=capacity_json WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			image := contracts.ExecutionImage{Reference: "registry.example/secondbox/integration-agent:mutable"}
			operation, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "rehome-start", stopped.Revision, contracts.StartSandboxRequest{Image: image, CapacityWaitMilliseconds: 60_000})
			if err != nil {
				t.Fatal(err)
			}
			// The old home may already be retrieving this same start's image.
			fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
			if draining {
				sequence := fixture.nextSequence()
				fixture.recordRunnerEvent(t, fixture.runnerID, fixture.connectionID, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: "resolved-source-image", Sequence: sequence, OperationId: operation.ID, ResolvedDigest: "sha256:" + strings.Repeat("c", 64), Manifest: rehomeImageManifest(t)}}})
			}
			if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), map[string]int64{fixture.profileName: 3600}); err != nil {
				t.Fatal(err)
			}
			export := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT)
			if export.OperationId == operation.ID || export.Correlation.GetOperationId() != operation.ID {
				t.Fatal("offload must use private receipt identity and preserve public start correlation")
			}
			descriptor := &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}
			recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
			exported, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
			if err != nil || exported.State != contracts.OperationStatePending || exported.CompletedAt != nil {
				t.Fatalf("start completed at export: %#v error=%v", exported, err)
			}
			if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), map[string]int64{fixture.profileName: 3600}); err != nil {
				t.Fatal(err)
			}
			restore := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT)
			cleanup := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE)
			if restore.Correlation.RunnerId != target || restore.Correlation.OperationId != operation.ID || restore.OperationId == operation.ID || restore.OperationId == export.OperationId {
				t.Fatalf("restore target=%q operation=%q", restore.Correlation.RunnerId, restore.OperationId)
			}
			recordStartArchiveReceipt(t, fixture, target, targetConnection, 1, restore, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
			restored, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
			if err != nil || restored.State != contracts.OperationStatePending {
				t.Fatalf("start completed at restore: %#v error=%v", restored, err)
			}
			fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
			var targetPreparation string
			if err := fixture.pool.QueryRow(t.Context(), `SELECT id FROM secondbox.lifecycle_effects WHERE sandbox_id=$1 AND runner_id=$2 AND kind='prepare_image' AND state='queued'`, sandboxID, target).Scan(&targetPreparation); err != nil {
				t.Fatal(err)
			}
			if targetPreparation == operation.ID {
				t.Fatal("new home reused the old source's image preparation identity")
			}
			var targetPayload []byte
			if err := fixture.pool.QueryRow(t.Context(), `SELECT payload FROM secondbox.runner_commands WHERE assignment_id=$1 AND kind='prepare-image'`, targetPreparation).Scan(&targetPayload); err != nil {
				t.Fatal(err)
			}
			var targetEnvelope runnerv1.ControlPlaneToRunner
			if err := proto.Unmarshal(targetPayload, &targetEnvelope); err != nil {
				t.Fatal(err)
			}
			wantReference := image.Reference
			if draining {
				wantReference = contracts.ExecutionImageDigestReference(image.Reference, "sha256:"+strings.Repeat("c", 64))
			}
			if got := targetEnvelope.GetPrepareImage().Reference; got != wantReference {
				t.Fatalf("target image reference=%q want=%q (source preparation completed=%t)", got, wantReference, draining)
			}
			manifest := rehomeImageManifest(t)
			lateSequence := fixture.nextSequence()
			fixture.recordRunnerEvent(t, fixture.runnerID, fixture.connectionID, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: fmt.Sprintf("old-home-image-%d", lateSequence), Sequence: lateSequence, OperationId: operation.ID, ResolvedDigest: "sha256:" + strings.Repeat("c", 64), Manifest: manifest}}})
			var prepState string
			if err := fixture.pool.QueryRow(t.Context(), `SELECT state FROM secondbox.lifecycle_effects WHERE id=$1`, targetPreparation).Scan(&prepState); err != nil || prepState != "queued" {
				t.Fatalf("late source changed target preparation state=%q error=%v", prepState, err)
			}
			fixture.recordRunnerEvent(t, target, targetConnection, runnercontrol.EventImagePreparation, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_PrepareImageResult{PrepareImageResult: &runnerv1.PrepareImageResult{MessageId: "target-rehome-image", Sequence: 2, OperationId: targetPreparation, ResolvedDigest: "sha256:" + strings.Repeat("c", 64), Manifest: manifest}}})
			fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
			assignment := fixture.pendingCommand(t, sandboxID, "assignment").GetAssignment()
			if assignment.Correlation.RunnerId != target || assignment.Correlation.OperationId != operation.ID {
				t.Fatalf("assignment lost target/start correlation: %#v", assignment.Correlation)
			}
			fixture.recordRunnerEvent(t, target, targetConnection, runnercontrol.EventAssignment, &runnerv1.RunnerToControlPlane{Message: &runnerv1.RunnerToControlPlane_AssignmentResult{AssignmentResult: &runnerv1.AssignmentResult{MessageId: "target-rehome-ready", Sequence: 3, Fence: proto.Clone(assignment.Fence).(*runnerv1.AssignmentFence), Terminal: runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY, BackendKind: "firecracker", BackendReference: "rehome-test", RequestedImageReference: assignment.ExecutionImage.Reference, ResolvedImageDigest: "sha256:" + strings.Repeat("c", 64), Correlation: proto.Clone(assignment.Correlation).(*runnerv1.Correlation)}}})
			recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), cleanup, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
			current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
			if err != nil || current.State != contracts.SandboxStateReady || current.Generation != stopped.Generation {
				t.Fatalf("rehome changed Sandbox identity/generation: %#v error=%v", current, err)
			}
			var requestedReference string
			if err := fixture.pool.QueryRow(t.Context(), `SELECT execution_image_reference FROM secondbox.sandboxes WHERE id=$1`, sandboxID).Scan(&requestedReference); err != nil || requestedReference != image.Reference {
				t.Fatalf("public requested image=%q want=%q error=%v", requestedReference, image.Reference, err)
			}
			done, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
			if err != nil || done.State != contracts.OperationStateSucceeded {
				t.Fatalf("original start final=%#v error=%v", done, err)
			}
		})
	}
}

func TestStoppedStartRehomePreservesIntentAndAuthorityOnCancellationAndFailure(t *testing.T) {
	for _, mode := range []string{"export-stop", "export-delete", "export-expire", "export-failure", "import-stop", "import-delete", "import-failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newTeardownFixture(t)
			if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, map[string]int64{fixture.profileName: 3600}); err != nil {
				t.Fatal(err)
			}
			sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "rehome-"+mode)
			target, targetConnection := "", ""
			if strings.HasPrefix(mode, "import-") {
				target, targetConnection = fixture.addEligibleRunner(t, "failure-target")
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET protocol_versions_json='["8"]',capabilities_json=capabilities_json || '["workspace-relocation"]'::jsonb WHERE id=$1`, target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json=capacity_json WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			operation, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "rehome-intent-start", stopped.Revision, contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000})
			if err != nil {
				t.Fatal(err)
			}
			reconcile := func() {
				t.Helper()
				if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), map[string]int64{fixture.profileName: 3600}); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			export := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT)
			descriptor := &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}
			var transfer *runnerv1.LocalWorkspaceCommand
			var cleanup *runnerv1.LocalWorkspaceCommand
			if strings.HasPrefix(mode, "import-") {
				recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
				reconcile()
				transfer = pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT)
				cleanup = pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE)
			}
			var intent contracts.Operation
			current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(mode, "stop") {
				intent, err = fixture.controlPlane.StopSandbox(t.Context(), fixture.principal, sandboxID, "rehome-intent-stop", current.Revision)
			}
			if strings.HasSuffix(mode, "delete") {
				intent, err = fixture.controlPlane.DeleteSandbox(t.Context(), fixture.principal, sandboxID, "rehome-intent-delete", current.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "export-expire" {
				if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.operations SET capacity_wait_deadline=$2 WHERE id=$1`, operation.ID, time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
				reconcile()
			}
			var heldMutation string
			if err := fixture.pool.QueryRow(t.Context(), `SELECT w.mutation_kind FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.workspace_id=w.id WHERE s.id=$1`, sandboxID).Scan(&heldMutation); err != nil {
				t.Fatal(err)
			}
			if heldMutation != "archive" && heldMutation != "archive_restore" {
				t.Fatalf("intent released in-flight transfer fence: %q", heldMutation)
			}
			if mode == "export-failure" {
				recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED, nil)
				beforeAbort, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
				if err != nil || beforeAbort.State != contracts.OperationStatePending {
					t.Fatalf("failed upload completed start before source unseal: %#v error=%v", beforeAbort, err)
				}
				reconcile()
				abort := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE)
				recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), abort, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
			} else if strings.HasPrefix(mode, "import-") {
				terminal := runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED
				if mode == "import-failure" {
					terminal = runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_RUNNER_FAILED
				}
				recordStartArchiveReceipt(t, fixture, target, targetConnection, 1, transfer, terminal, descriptor)
				if mode == "import-delete" {
					fixture.runLifecycle(t, sandboxID, lifecycle.ActionDelete)
					deletion := fixture.pendingLocalWorkspaceCommand(t, sandboxID)
					recordStartArchiveReceipt(t, fixture, target, targetConnection, 2, deletion, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
				}
				recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), cleanup, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
			} else {
				recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
				reconcile()
			}
			final, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			wanted := contracts.OperationStateCancelled
			if strings.HasSuffix(mode, "failure") || strings.HasSuffix(mode, "expire") {
				wanted = contracts.OperationStateFailed
			}
			if final.State != wanted {
				t.Fatalf("original start state=%q want=%q error=%#v", final.State, wanted, final.Error)
			}
			if mode == "export-expire" && (final.Error == nil || final.Error.Code != "capacity_wait_expired") {
				t.Fatalf("expiry error=%#v", final.Error)
			}
			var state, desired, home, workspaceState, mutation, archiveKey string
			if err := fixture.pool.QueryRow(t.Context(), `SELECT s.state,s.desired_state,w.home_runner_id,w.state,w.mutation_kind,w.archive_key FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, sandboxID).Scan(&state, &desired, &home, &workspaceState, &mutation, &archiveKey); err != nil {
				t.Fatal(err)
			}
			if mutation != "" {
				t.Fatalf("terminal transfer kept mutation %q", mutation)
			}
			if mode == "export-failure" {
				if workspaceState != "ready" || home != fixture.runnerID || desired != "stopped" || archiveKey != "" {
					t.Fatalf("failed upload lost source state=%q home=%q desired=%q key=%q", workspaceState, home, desired, archiveKey)
				}
			}
			if mode == "import-failure" {
				if workspaceState != "archived" || home != "" || archiveKey == "" || desired != "stopped" {
					t.Fatalf("failed restore lost blob authority state=%q home=%q key=%q desired=%q", workspaceState, home, archiveKey, desired)
				}
			}
			if strings.HasSuffix(mode, "delete") {
				done, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, intent.ID)
				if err != nil || done.State != contracts.OperationStateSucceeded || state != "deleted" || desired != "deleted" {
					t.Fatalf("delete state=%q/%q operation=%#v error=%v", state, desired, done, err)
				}
			} else if desired != "stopped" {
				t.Fatalf("cancelled start remains wanted %q", desired)
			}
			if count := sandboxAssignmentCount(t, fixture, sandboxID); count != 0 {
				t.Fatalf("cancelled/failed rehome admitted %d Assignments", count)
			}
		})
	}
}

func TestStoppedStartDoesNotOffloadItsOwnPendingReservation(t *testing.T) {
	fixture := newTeardownFixture(t)
	if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, map[string]int64{fixture.profileName: 3600}); err != nil {
		t.Fatal(err)
	}
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "rehome-self")
	capacity, err := json.Marshal(map[string]int64{"VCPUCount": stopped.Resources.VCPUCount, "MemoryBytes": stopped.Resources.MemoryBytes, "Instances": 1, "Operations": 1000, "DiskBytes": stopped.Resources.WorkspaceBytes * 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET capacity_json=$2 WHERE id=$1`, fixture.runnerID, capacity); err != nil {
		t.Fatal(err)
	}
	start, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "rehome-self-start", stopped.Revision, contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
	for range 2 {
		if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), map[string]int64{fixture.profileName: 3600}); err != nil {
			t.Fatal(err)
		}
	}
	var archives int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.workspace_archives WHERE operation_id=$1`, start.ID).Scan(&archives); err != nil {
		t.Fatal(err)
	}
	if archives != 0 {
		t.Fatal("image preparation's own pending reservation triggered offload")
	}
	current, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlPlane.StopSandbox(t.Context(), fixture.principal, sandboxID, "rehome-self-stop", current.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedStartUnconfiguredProfileStaysResidentUntilHomeCapacityReturns(t *testing.T) {
	for _, enabled := range []map[string]int64{nil, {"separate-code-profile": 3600}} {
		t.Run(fmt.Sprintf("configured-profiles-%d", len(enabled)), func(t *testing.T) {
			fixture := newTeardownFixture(t)
			if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, enabled); err != nil {
				t.Fatal(err)
			}
			sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "static-profile")
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json=capacity_json WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			operation, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "static-profile-start", stopped.Revision, contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000})
			if err != nil {
				t.Fatal(err)
			}
			fixture.reconcileWithoutPreparation(t, sandboxID, time.Now().UTC())
			if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), enabled); err != nil {
				t.Fatal(err)
			}
			var home, mutation string
			var archives int
			if err := fixture.pool.QueryRow(t.Context(), `SELECT w.home_runner_id,w.mutation_kind,(SELECT count(*) FROM secondbox.workspace_archives a WHERE a.workspace_id=w.id) FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.workspace_id=w.id WHERE s.id=$1`, sandboxID).Scan(&home, &mutation, &archives); err != nil {
				t.Fatal(err)
			}
			if home != fixture.runnerID || mutation != "start" || archives != 0 {
				t.Fatalf("unconfigured Profile offloaded: home=%q mutation=%q archives=%d", home, mutation, archives)
			}
			if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json='{}' WHERE id=$1`, fixture.runnerID); err != nil {
				t.Fatal(err)
			}
			fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
			fixture.completeAssignmentReady(t, sandboxID)
			done, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
			if err != nil || done.State != contracts.OperationStateSucceeded {
				t.Fatalf("resident start did not resume: operation=%#v error=%v", done, err)
			}
		})
	}
}

func TestStoppedStartCanReturnToFormerHomeOnlyAfterExactSourceCleanup(t *testing.T) {
	fixture := newTeardownFixture(t)
	if err := fixture.store.ConfigureWorkspaceArchives(t.Context(), startRehomeArchiveProvider{}, map[string]int64{fixture.profileName: 3600}); err != nil {
		t.Fatal(err)
	}
	sandboxID, stopped := createStoppedFixtureSandbox(t, fixture, "return-home")
	_, _ = fixture.addEligibleRunner(t, "return-home-full-target")
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json=capacity_json,protocol_versions_json='["8"]',capabilities_json=capabilities_json || '["workspace-relocation"]'::jsonb WHERE pool_name=$1`, fixture.poolName); err != nil {
		t.Fatal(err)
	}
	operation, _, err := fixture.controlPlane.StartSandboxOperation(t.Context(), fixture.principal, sandboxID, "return-home-start", stopped.Revision, contracts.StartSandboxRequest{Image: testExecutionImage(), CapacityWaitMilliseconds: 60_000})
	if err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if err := fixture.store.ReconcileWorkspaceArchives(t.Context(), time.Now().UTC(), map[string]int64{fixture.profileName: 3600}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	export := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT)
	descriptor := &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), export, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
	reconcile()
	cleanup := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE)
	if _, err := fixture.pool.Exec(t.Context(), `UPDATE secondbox.runners SET reserved_capacity_json='{}' WHERE id=$1`, fixture.runnerID); err != nil {
		t.Fatal(err)
	}
	reconcile()
	var imports int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.workspace_archives WHERE operation_id=$1 AND target_runner_id<>''`, operation.ID).Scan(&imports); err != nil || imports != 0 {
		t.Fatalf("source selected before cleanup proof: imports=%d error=%v", imports, err)
	}
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), cleanup, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
	reconcile()
	restore := pendingStartArchiveCommand(t, fixture, sandboxID, runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT)
	if restore.Correlation.RunnerId != fixture.runnerID || restore.Correlation.OperationId != operation.ID || restore.OperationId == operation.ID {
		t.Fatalf("freed original home was not selected: %#v", restore.Correlation)
	}
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), restore, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, descriptor)
	fixture.runLifecycle(t, sandboxID, lifecycle.ActionStartInstance)
	fixture.completeAssignmentReady(t, sandboxID)
	before, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil || before.State != contracts.SandboxStateReady {
		t.Fatalf("restored original home is not ready: %#v error=%v", before, err)
	}
	// The old source and new home are the same Runner and generation. Only
	// exact command identity makes this late cleanup reply harmless.
	recordStartArchiveReceipt(t, fixture, fixture.runnerID, fixture.connectionID, fixture.nextSequence(), cleanup, runnerv1.LocalWorkspaceTerminalKind_LOCAL_WORKSPACE_TERMINAL_KIND_SUCCEEDED, nil)
	after, err := fixture.controlPlane.GetSandbox(t.Context(), fixture.principal, sandboxID)
	if err != nil || after.State != before.State || after.Generation != before.Generation || after.Revision != before.Revision {
		t.Fatalf("cleanup replay changed restored home: before=%#v after=%#v error=%v", before, after, err)
	}
	var home string
	if err := fixture.pool.QueryRow(t.Context(), `SELECT home_runner_id FROM secondbox.workspaces WHERE sandbox_id=$1`, sandboxID).Scan(&home); err != nil || home != fixture.runnerID {
		t.Fatalf("cleanup replay cleared home=%q error=%v", home, err)
	}
	done, err := fixture.controlPlane.GetOperation(t.Context(), fixture.principal, operation.ID)
	if err != nil || done.State != contracts.OperationStateSucceeded {
		t.Fatalf("original start failed after safe return: %#v error=%v", done, err)
	}
}
