package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestStoppedCheckpointRelocatesThroughProductionSchedulerToAnotherRunnerExclusively(
	t *testing.T,
) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := controlPlane.BootstrapAdmin()
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "cross-runner-relocation",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "operator-cross-runner-profile",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "cross-runner-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	unrelated, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "cross-runner-unrelated-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, unrelated.ID, "cross-runner-unrelated-start", unrelated.Revision,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.sandboxes
			SET state='stopped',desired_state='stopped',
			    next_reconcile_at='2999-01-01 00:00:00+00',
			    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
			WHERE id=$1`, unrelated.ID,
		); err != nil {
			t.Errorf("clean unrelated lifecycle work: %v", err)
		}
	})
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	schedulerStore, err := scheduler.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(schedulerStore.Close)

	const (
		firstRunnerID     = "runner-cross-restore-a"
		firstConnectionID = "connection-cross-restore-a"
		nextRunnerID      = "runner-cross-restore-b"
		nextConnectionID  = "connection-cross-restore-b"
	)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.runners
			SET state='offline',updated_at=now()
			WHERE id IN ($1,$2)`,
			firstRunnerID, nextRunnerID,
		); err != nil {
			t.Errorf("clean cross-Runner registrations: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.sandboxes
			SET state='stopped',desired_state='stopped',
			    next_reconcile_at='2999-01-01 00:00:00+00',
			    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
			WHERE id=$1`, sandbox.ID,
		); err != nil {
			t.Errorf("clean cross-Runner Sandbox lifecycle work: %v", err)
		}
	})
	registerCrossRunnerForRestore(
		t, stateStore, firstRunnerID, firstConnectionID, profile.CurrentRevision.Spec.Pool, now,
	)
	registerCrossRunnerForRestore(
		t, stateStore, nextRunnerID, nextConnectionID, profile.CurrentRevision.Spec.Pool, now,
	)

	idSequence := 0
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, schedulerStore,
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              1,
			SerializationRetryLimit: 1,
			AssetCatalog:            profileLifecycleAssetCatalog{},
			SessionCanceller:        profileLifecycleSessionCanceller{},
			NewID: func(prefix string) string {
				idSequence++
				return fmt.Sprintf("%s-cross-runner-%d", prefix, idSequence)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte(fmt.Sprintf("%032d", idSequence)), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(effectBroker.Close)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID: "cross-runner-lifecycle", ClaimDuration: time.Minute,
		PollInterval: time.Millisecond,
	}

	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "cross-runner-first-start", sandbox.Revision,
	); err != nil {
		t.Fatal(err)
	}
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now, lifecycle.ActionMaterialize,
	)
	firstDelivery := claimCrossRunnerCommand(
		t, stateStore, firstRunnerID, firstConnectionID, now.Add(time.Millisecond),
	)
	firstAssignment := firstDelivery.Message.GetAssignment()
	if firstAssignment == nil || firstAssignment.SourceCheckpointId != "" ||
		firstAssignment.Fence.SandboxGeneration != uint64(sandbox.Generation) {
		t.Fatalf("first Runner Assignment = %#v", firstAssignment)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), firstDelivery.ID, firstConnectionID, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	recordCrossRunnerAssignmentReady(
		t, stateStore, firstRunnerID, firstConnectionID, firstAssignment,
		2, "first-runner-instance", now.Add(2*time.Millisecond),
	)
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(3*time.Millisecond), lifecycle.ActionMarkReady,
	)
	readySandbox, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldLease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, readySandbox.Generation,
		"cross-runner-old-lease", 30,
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "cross-runner-stop", readySandbox.Revision,
	); err != nil {
		t.Fatal(err)
	}
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(4*time.Millisecond), lifecycle.ActionDrain,
	)
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(5*time.Millisecond), lifecycle.ActionCheckpoint,
	)
	checkpointDelivery := claimCrossRunnerCommand(
		t, stateStore, firstRunnerID, firstConnectionID, now.Add(6*time.Millisecond),
	)
	checkpointCommand := checkpointDelivery.Message.GetCheckpoint()
	if checkpointCommand == nil ||
		checkpointCommand.Fence.AssignmentId != firstAssignment.Fence.AssignmentId {
		t.Fatalf("first Runner Checkpoint command = %#v", checkpointDelivery.Message)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), checkpointDelivery.ID, firstConnectionID, now.Add(6*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	checkpointBytes := []byte("portable bytes committed by runner A and restored by runner B")
	checkpointSum := sha256.Sum256(checkpointBytes)
	checkpointCompatibility := map[string]string{
		"architecture": "amd64", "backend": "firecracker",
		"profileRevisionId": sandbox.ProfileRevisionID, "workspaceFormat": "ext4",
		"runtimeManifestDigest":   profile.CurrentRevision.Spec.RuntimeBundleDigest,
		"toolchainManifestDigest": profile.CurrentRevision.Spec.ToolchainBundleDigest,
		"guestProtocolGeneration": "1", "mandatoryGuestFeatures": "",
	}
	objects := &checkpointObjectStore{objects: make(map[string][]byte)}
	receiver, err := lifecycle.NewCheckpointReceiver(
		t.Context(),
		lifecycle.CheckpointReceiverConfig{
			DatabaseURL: integrationDatabaseURL, SpoolDirectory: t.TempDir(),
			ObjectStore: objects, LifecycleStore: databaseStore,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	chunkEvent := runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: firstRunnerID,
		ConnectionID: firstConnectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointChunk{
				CheckpointChunk: &runnerv1.CheckpointChunk{
					MessageId: "cross-runner-checkpoint-chunk", Sequence: 3,
					Fence: checkpointCommand.Fence, CheckpointId: checkpointCommand.CheckpointId,
					StorageObjectId: checkpointCommand.StorageObjectId, Data: checkpointBytes,
				},
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), chunkEvent, now.Add(7*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), chunkEvent, now.Add(7*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	resultEvent := runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: firstRunnerID,
		ConnectionID: firstConnectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointResult{
				CheckpointResult: &runnerv1.CheckpointResult{
					MessageId: "cross-runner-checkpoint-result", Sequence: 4,
					Fence: checkpointCommand.Fence, CheckpointId: checkpointCommand.CheckpointId,
					StorageObjectId: checkpointCommand.StorageObjectId,
					Terminal:        runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED,
					Sha256:          hex.EncodeToString(checkpointSum[:]), SizeBytes: uint64(len(checkpointBytes)),
					Compatibility:             checkpointCompatibility,
					TerminationEvidenceDigest: "sha256:first-runner-checkpoint-termination",
					Correlation:               proto.Clone(checkpointCommand.Correlation).(*runnerv1.Correlation),
				},
			},
		},
	}
	if correlation := resultEvent.Message.GetCheckpointResult().Correlation; correlation == nil ||
		correlation.RequestId == "" || correlation.OperationId == "" ||
		correlation.SandboxId != checkpointCommand.Fence.SandboxId ||
		correlation.InstanceId != checkpointCommand.Fence.InstanceId ||
		correlation.SandboxGeneration != checkpointCommand.Fence.SandboxGeneration ||
		correlation.AssignmentId != checkpointCommand.Fence.AssignmentId ||
		correlation.RunnerId != firstRunnerID {
		t.Fatalf(
			"checkpoint correlation = %#v, fence=%#v, runner=%q",
			correlation, checkpointCommand.Fence, firstRunnerID,
		)
	}
	if _, err := stateStore.RecordEvent(t.Context(), resultEvent, now.Add(8*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), resultEvent, now.Add(8*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(9*time.Millisecond), lifecycle.ActionStopInstance,
	)
	fenceDelivery := claimCrossRunnerCommand(
		t, stateStore, firstRunnerID, firstConnectionID, now.Add(10*time.Millisecond),
	)
	fenceCommand := fenceDelivery.Message.GetFence()
	if fenceCommand == nil ||
		fenceCommand.Fence.AssignmentId != firstAssignment.Fence.AssignmentId {
		t.Fatalf("first Runner Fence command = %#v", fenceDelivery.Message)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), fenceDelivery.ID, firstConnectionID, now.Add(10*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	fenceResult := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_FenceResult{
			FenceResult: &runnerv1.FenceResult{
				MessageId: "cross-runner-fence-result", Sequence: 5, Fence: fenceCommand.Fence,
				Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
				TerminationEvidenceDigest: "sha256:first-runner-release-proof",
				Correlation:               proto.Clone(fenceCommand.Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventFence, RunnerID: firstRunnerID,
		ConnectionID: firstConnectionID, Message: fenceResult,
	}, now.Add(11*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(12*time.Millisecond), lifecycle.ActionFinishStop,
	)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET state='stopped',desired_state='stopped',
		    next_reconcile_at='2999-01-01 00:00:00+00',
		    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
		WHERE id<>$1`, sandbox.ID,
	); err != nil {
		t.Fatal(err)
	}

	stopped, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Generation != sandbox.Generation+1 ||
		stopped.Workspace.Generation != sandbox.Workspace.Generation+1 ||
		stopped.Workspace.CurrentCheckpointID != checkpointCommand.CheckpointId {
		t.Fatalf("completed stop generation/checkpoint = %#v", stopped)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(13*time.Millisecond),
	); err != nil || found {
		t.Fatalf("idempotent stopped reconciliation = %#v, %t, %v", decision, found, err)
	}
	reloadedStopped, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedStopped.Generation != stopped.Generation {
		t.Fatalf(
			"idempotent finish_stop advanced generation from %d to %d",
			stopped.Generation, reloadedStopped.Generation,
		)
	}
	var oldLeaseState string
	if err := pool.QueryRow(
		t.Context(), `SELECT state FROM secondbox.leases WHERE id=$1`, oldLease.ID,
	).Scan(&oldLeaseState); err != nil {
		t.Fatal(err)
	}
	if oldLeaseState != contracts.LeaseStateFenced {
		t.Fatalf("old generation Lease state = %q", oldLeaseState)
	}
	if _, err := controlPlane.TouchSandbox(
		t.Context(), principal, sandbox.ID, sandbox.Generation, oldLease.ID,
		"cross-runner-stale-touch",
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("old generation touch error = %v, want generation fence", err)
	}

	if duplicate, err := stateStore.RecordHeartbeat(
		t.Context(),
		task4Heartbeat(
			firstRunnerID, firstConnectionID, "cross-runner-draining", 6,
			runnerv1.DrainPhase_DRAIN_PHASE_DRAINING,
		),
		now.Add(14*time.Millisecond),
	); err != nil || duplicate {
		t.Fatalf("first Runner drain heartbeat duplicate, error = %t, %v", duplicate, err)
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "cross-runner-second-start", stopped.Revision,
	); err != nil {
		t.Fatal(err)
	}
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(15*time.Millisecond), lifecycle.ActionMaterialize,
	)
	nextDelivery := claimCrossRunnerCommand(
		t, stateStore, nextRunnerID, nextConnectionID, now.Add(16*time.Millisecond),
	)
	nextAssignment := nextDelivery.Message.GetAssignment()
	if nextAssignment == nil ||
		nextAssignment.SourceCheckpointId != checkpointCommand.CheckpointId ||
		nextAssignment.Fence.SandboxGeneration != uint64(stopped.Generation) ||
		nextAssignment.Fence.AssignmentId == firstAssignment.Fence.AssignmentId ||
		bytes.Equal(nextAssignment.Fence.FencingToken, firstAssignment.Fence.FencingToken) {
		t.Fatalf("replacement Runner Assignment = %#v", nextAssignment)
	}

	restoreSender, err := lifecycle.NewCheckpointRestoreSender(
		t.Context(),
		lifecycle.CheckpointRestoreSenderConfig{
			DatabaseURL: integrationDatabaseURL, ObjectStore: objects,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoreSender.Close)
	var restoreFrames []*runnerv1.ControlPlaneToRunner
	if err := restoreSender.StreamRestore(
		t.Context(), nextAssignment,
		func(frame *runnerv1.ControlPlaneToRunner) error {
			restoreFrames = append(restoreFrames, frame)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	restoredBytes := collectCrossRunnerRestoreBytes(t, restoreFrames, nextAssignment)
	if !bytes.Equal(restoredBytes, checkpointBytes) {
		t.Fatalf("replacement Runner restored bytes = %q, want %q", restoredBytes, checkpointBytes)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), nextDelivery.ID, nextConnectionID, now.Add(16*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	staleProgress := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
			AssignmentProgress: &runnerv1.AssignmentProgress{
				MessageId: "cross-runner-stale-progress", Sequence: 7,
				Fence:       firstAssignment.Fence,
				Stage:       runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY,
				Correlation: proto.Clone(firstAssignment.Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: firstRunnerID,
		ConnectionID: firstConnectionID, Message: staleProgress,
	}, now.Add(17*time.Millisecond)); !errors.Is(err, runnercontrol.ErrStaleAssignmentEvidence) {
		t.Fatalf("old Runner progress error = %v, want stale Assignment evidence", err)
	}

	assertExclusiveCrossRunnerMaterialization(
		t, pool, sandbox.Workspace.ID, firstRunnerID, nextRunnerID,
		"", checkpointCommand.CheckpointId, sandbox.Generation, stopped.Generation,
		contracts.MaterializationStatePreparing,
	)
	recordCrossRunnerAssignmentReady(
		t, stateStore, nextRunnerID, nextConnectionID, nextAssignment,
		2, "replacement-runner-instance", now.Add(18*time.Millisecond),
	)
	assertCrossRunnerLifecycleAction(
		t, &reconciler, sandbox.ID, now.Add(19*time.Millisecond), lifecycle.ActionMarkReady,
	)
	assertExclusiveCrossRunnerMaterialization(
		t, pool, sandbox.Workspace.ID, firstRunnerID, nextRunnerID,
		"", checkpointCommand.CheckpointId, sandbox.Generation, stopped.Generation,
		contracts.MaterializationStateReady,
	)
}

func registerCrossRunnerForRestore(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	poolName string,
	now time.Time,
) {
	t.Helper()
	caCertificate, caPrivateKey := task4CertificateAuthority(t, now)
	authority := task4CredentialAuthority(t, caCertificate, caPrivateKey, now)
	enrollment, err := authority.CreateEnrollment(t.Context(), runnercontrol.EnrollmentRequest{
		TokenID: "enrollment-" + runnerID, RunnerID: runnerID, PoolName: poolName,
		RunnerName: runnerID, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.RedeemEnrollment(
		t.Context(), enrollment.Token, task4CertificateRequest(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.OpenConnection(
		t.Context(), issued.Identity, connectionID, 1, now,
	); err != nil {
		t.Fatal(err)
	}
	registration := task4Registration(runnerID, connectionID, poolName)
	registration.ArtifactCache = []*runnerv1.ArtifactCacheEvidence{
		{
			ArtifactId:       "runtime",
			ManifestDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			VerifiedAtUnixMs: uint64(now.UnixMilli()),
		},
		{
			ArtifactId:       "toolchain",
			ManifestDigest:   "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			VerifiedAtUnixMs: uint64(now.UnixMilli()),
		},
	}
	if duplicate, err := stateStore.RecordRegistration(
		t.Context(), registration, now,
	); err != nil || duplicate {
		t.Fatalf("Runner registration duplicate, error = %t, %v", duplicate, err)
	}
}

func assertCrossRunnerLifecycleAction(
	t *testing.T,
	reconciler *lifecycle.Reconciler,
	sandboxID string,
	now time.Time,
	want lifecycle.Action,
) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.sandboxes
		SET next_reconcile_at='1970-01-01 00:00:01+00',
		    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
		WHERE id=$1`, sandboxID,
	); err != nil {
		t.Fatal(err)
	}
	decision, found, err := reconciler.RunOnce(t.Context(), now)
	if err != nil || !found || decision.Action != want {
		t.Fatalf("cross-Runner lifecycle decision = %#v, %t, %v; want %q", decision, found, err, want)
	}
}

func claimCrossRunnerCommand(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	now time.Time,
) runnercontrol.CommandDelivery {
	t.Helper()
	delivery, found, err := stateStore.ClaimCommand(
		t.Context(), runnerID, connectionID, now,
	)
	if err != nil || !found {
		t.Fatalf("cross-Runner command delivery found, error = %t, %v", found, err)
	}
	return delivery
}

func recordCrossRunnerAssignmentReady(
	t *testing.T,
	stateStore *runnercontrol.PostgresStateStore,
	runnerID string,
	connectionID string,
	assignment *runnerv1.AssignmentCommand,
	sequence uint64,
	backendReference string,
	now time.Time,
) {
	t.Helper()
	result := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentResult{
			AssignmentResult: &runnerv1.AssignmentResult{
				MessageId: fmt.Sprintf("%s-ready-%d", runnerID, sequence), Sequence: sequence,
				Fence:       assignment.Fence,
				Terminal:    runnerv1.AssignmentTerminalKind_ASSIGNMENT_TERMINAL_KIND_READY,
				BackendKind: "firecracker", BackendReference: backendReference,
				Correlation: proto.Clone(assignment.Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: runnerID,
		ConnectionID: connectionID, Message: result,
	}, now); err != nil {
		t.Fatal(err)
	}
}

func collectCrossRunnerRestoreBytes(
	t *testing.T,
	frames []*runnerv1.ControlPlaneToRunner,
	assignment *runnerv1.AssignmentCommand,
) []byte {
	t.Helper()
	if len(frames) < 3 {
		t.Fatalf("cross-Runner restore frames = %#v", frames)
	}
	begin := frames[0].GetRestoreBegin()
	if begin == nil || begin.CheckpointId != assignment.SourceCheckpointId ||
		begin.Fence.AssignmentId != assignment.Fence.AssignmentId ||
		begin.Fence.SandboxGeneration != assignment.Fence.SandboxGeneration {
		t.Fatalf("cross-Runner RestoreBegin = %#v", begin)
	}
	restored := make([]byte, 0, begin.SizeBytes)
	for index, frame := range frames[1:] {
		chunk := frame.GetRestoreChunk()
		if chunk == nil || chunk.Fence.AssignmentId != assignment.Fence.AssignmentId ||
			chunk.Fence.SandboxGeneration != assignment.Fence.SandboxGeneration ||
			chunk.Offset != uint64(len(restored)) {
			t.Fatalf("cross-Runner RestoreChunk %d = %#v", index, chunk)
		}
		restored = append(restored, chunk.Data...)
		if index == len(frames)-2 && !chunk.EndOfObject {
			t.Fatalf("cross-Runner terminal RestoreChunk = %#v", chunk)
		}
	}
	sum := sha256.Sum256(restored)
	if hex.EncodeToString(sum[:]) != begin.Sha256 || uint64(len(restored)) != begin.SizeBytes {
		t.Fatalf("cross-Runner restore integrity = %x/%d, want %s/%d", sum, len(restored), begin.Sha256, begin.SizeBytes)
	}
	return restored
}

func assertExclusiveCrossRunnerMaterialization(
	t *testing.T,
	pool *pgxpool.Pool,
	workspaceID string,
	firstRunnerID string,
	nextRunnerID string,
	firstCheckpointID string,
	checkpointID string,
	firstGeneration int64,
	nextGeneration int64,
	nextState string,
) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT runner_id,generation,source_checkpoint_id,state
		FROM secondbox.workspace_materializations
		WHERE workspace_id=$1 ORDER BY generation,id`,
		workspaceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type evidence struct {
		runnerID          string
		generation        int64
		sourceCheckpoint  string
		materializedState string
	}
	var materializations []evidence
	for rows.Next() {
		var item evidence
		if err := rows.Scan(
			&item.runnerID, &item.generation,
			&item.sourceCheckpoint, &item.materializedState,
		); err != nil {
			t.Fatal(err)
		}
		materializations = append(materializations, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(materializations) != 2 ||
		materializations[0] != (evidence{
			runnerID: firstRunnerID, generation: firstGeneration,
			sourceCheckpoint:  firstCheckpointID,
			materializedState: contracts.MaterializationStateReleased,
		}) ||
		materializations[1] != (evidence{
			runnerID: nextRunnerID, generation: nextGeneration,
			sourceCheckpoint: checkpointID, materializedState: nextState,
		}) {
		t.Fatalf("exclusive cross-Runner materializations = %#v", materializations)
	}
}
