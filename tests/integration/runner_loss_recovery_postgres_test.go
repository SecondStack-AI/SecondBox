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
	"github.com/SecondStack-AI/SecondBox/internal/reconcile"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/scheduler"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestRunnerLossRecoveryFencesOldAuthorityAndRestoresCheckpointOnAnotherRunner(
	t *testing.T,
) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "runner-loss-recovery",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "runner-loss-recovery-profile",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "runner-loss-recovery-create",
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

	checkpointBytes := []byte("published portable workspace survives runner loss")
	checkpointSum := sha256.Sum256(checkpointBytes)
	checkpointID := "chk-runner-loss-" + sandbox.ID
	checkpointStorageKey := "checkpoints/" + checkpointID
	checkpoint := contracts.WorkspaceCheckpoint{
		ID: checkpointID, WorkspaceID: sandbox.Workspace.ID,
		SourceGeneration: sandbox.Generation,
		SHA256:           hex.EncodeToString(checkpointSum[:]),
		SizeBytes:        int64(len(checkpointBytes)),
		Compatibility: map[string]string{
			"architecture": "amd64", "backend": "firecracker",
			"profileRevisionId": sandbox.ProfileRevisionID, "workspaceFormat": "ext4",
			"runtimeManifestDigest":   profile.CurrentRevision.Spec.RuntimeBundleDigest,
			"toolchainManifestDigest": profile.CurrentRevision.Spec.ToolchainBundleDigest,
			"guestProtocolGeneration": "1", "mandatoryGuestFeatures": "",
		},
		RetainUntil: now.Add(24 * time.Hour), CreatedAt: now,
	}
	publication := ports.CheckpointPublicationInput{
		Checkpoint: checkpoint, StorageKey: checkpointStorageKey,
		ExpectedWorkspaceGeneration: sandbox.Generation,
	}
	if _, err := databaseStore.StageCheckpoint(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.VerifyCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseStore.PublishCheckpoint(t.Context(), publication, now); err != nil {
		t.Fatal(err)
	}
	objects := &checkpointObjectStore{
		objects: map[string][]byte{checkpointStorageKey: bytes.Clone(checkpointBytes)},
	}

	const (
		lostRunnerID     = "runner-loss-a"
		lostConnectionID = "connection-runner-loss-a"
		nextRunnerID     = "runner-loss-b"
		nextConnectionID = "connection-runner-loss-b"
	)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.runners
			SET state='offline',updated_at=now()
			WHERE id IN ($1,$2)`,
			lostRunnerID, nextRunnerID,
		); err != nil {
			t.Errorf("clean Runner-loss registrations: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `
			UPDATE secondbox.sandboxes
			SET state='stopped',desired_state='stopped',
			    next_reconcile_at='2999-01-01 00:00:00+00',
			    reconcile_owner='',reconcile_claim_expires_at='1970-01-01 00:00:01+00'
			WHERE id=$1`, sandbox.ID,
		); err != nil {
			t.Errorf("clean Runner-loss Sandbox lifecycle work: %v", err)
		}
	})
	registerCrossRunnerForRestore(
		t, stateStore, lostRunnerID, lostConnectionID,
		profile.CurrentRevision.Spec.Pool, now,
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
				return fmt.Sprintf("%s-runner-loss-%d", prefix, idSequence)
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
	lifecycleWorker := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID: "runner-loss-lifecycle-a", ClaimDuration: time.Minute,
		PollInterval: time.Millisecond,
	}
	if _, err := controlPlane.StartSandbox(
		t.Context(), principal, sandbox.ID, "runner-loss-start", sandbox.Revision,
	); err != nil {
		t.Fatal(err)
	}
	assertCrossRunnerLifecycleAction(
		t, &lifecycleWorker, sandbox.ID, now, lifecycle.ActionMaterialize,
	)
	firstDelivery := claimCrossRunnerCommand(
		t, stateStore, lostRunnerID, lostConnectionID, now.Add(time.Millisecond),
	)
	firstAssignment := firstDelivery.Message.GetAssignment()
	if firstAssignment == nil || firstAssignment.SourceCheckpointId != checkpointID {
		t.Fatalf("lost Runner Assignment = %#v", firstAssignment)
	}
	restoreSender, err := lifecycle.NewCheckpointRestoreSender(
		t.Context(), lifecycle.CheckpointRestoreSenderConfig{
			DatabaseURL: integrationDatabaseURL, ObjectStore: objects,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restoreSender.Close)
	var firstRestoreFrames []*runnerv1.ControlPlaneToRunner
	if err := restoreSender.StreamRestore(
		t.Context(), firstAssignment,
		func(frame *runnerv1.ControlPlaneToRunner) error {
			firstRestoreFrames = append(firstRestoreFrames, frame)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if restored := collectCrossRunnerRestoreBytes(
		t, firstRestoreFrames, firstAssignment,
	); !bytes.Equal(restored, checkpointBytes) {
		t.Fatalf("lost Runner restored bytes = %q, want %q", restored, checkpointBytes)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), firstDelivery.ID, lostConnectionID, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	recordCrossRunnerAssignmentReady(
		t, stateStore, lostRunnerID, lostConnectionID, firstAssignment,
		2, "lost-runner-instance", now.Add(2*time.Millisecond),
	)
	assertCrossRunnerLifecycleAction(
		t, &lifecycleWorker, sandbox.ID, now.Add(3*time.Millisecond), lifecycle.ActionMarkReady,
	)
	ready, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	lossAt := now.Add(30 * time.Second)
	registerCrossRunnerForRestore(
		t, stateStore, nextRunnerID, nextConnectionID,
		profile.CurrentRevision.Spec.Pool, lossAt.Add(-time.Second),
	)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), principal, sandbox.ID, ready.Generation, "runner-loss-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	activity, err := databaseStore.OpenActivitySession(t.Context(), ports.ActivityInput{
		GenerationInput: ports.GenerationInput{
			TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
			SubjectRef: principal.SubjectRef,
			Generation: ready.Generation, Now: now.Add(4 * time.Millisecond),
		},
		Session: contracts.ActivitySession{
			ID: "activity-runner-loss-" + sandbox.ID, Kind: contracts.ActivitySessionKindExec,
		},
		LeaseID: lease.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(
		t.Context(), runnercontrol.PostgresFrameRelayConfig{
			DatabaseURL: integrationDatabaseURL, ClaimDuration: time.Second,
			Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 4 << 20,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	execSession, _, err := relay.AdmitDataPlane(
		t.Context(), runnercontrol.DataPlaneAdmission{
			ID: "dps-runner-loss-" + sandbox.ID, StreamID: "stream-runner-loss-" + sandbox.ID,
			TenantRef: principal.TenantRef, SandboxID: sandbox.ID,
			SubjectRef: principal.SubjectRef, LeaseID: lease.ID,
			Generation: ready.Generation, Kind: "exec", Operation: "exec",
			RequestID: "request-runner-loss-exec", IdempotencyKey: "runner-loss-exec",
			RequestHash: "runner-loss-exec-hash", DeadlineAt: now.Add(50 * time.Second),
			MaximumResponseBytes: 1024,
			ExecOpen: &runnerv1.ExecOpen{
				Command:          &runnerv1.ExecOpen_Shell{Shell: "printf runner-loss"},
				DeadlineUnixMs:   uint64(now.Add(50 * time.Second).UnixMilli()),
				OutputLimitBytes: 1024,
			},
			Now: now.Add(4 * time.Millisecond),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	portID := "port-runner-loss-" + sandbox.ID
	portTunnel, _, err := relay.AdmitPortSession(
		t.Context(), runnercontrol.PortSessionAdmission{
			Session: contracts.PortSession{
				ID: portID, SandboxID: sandbox.ID, Generation: ready.Generation,
				Name: "web", ExpiresAt: now.Add(50 * time.Second),
			},
			StreamID: "stream-" + portID, TenantRef: principal.TenantRef,
			SubjectRef: principal.SubjectRef, RequestID: "request-" + portID,
			LeaseID: lease.ID, IdempotencyKey: "idempotency-" + portID,
			RequestHash: "hash-" + portID, Now: now.Add(5 * time.Millisecond),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	reconcileStore, err := reconcile.NewPostgresStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	commandSequence := 0
	assignmentWorker := reconcile.AssignmentWorker{
		Store: reconcileStore, WorkerID: "runner-loss-assignment-a",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
		CommandDeadline: time.Minute, HeartbeatTimeout: 10 * time.Second,
		NewCommandID: func(prefix string) string {
			commandSequence++
			return fmt.Sprintf("%s-runner-loss-%d", prefix, commandSequence)
		},
	}
	decision, found, err := assignmentWorker.RunOnce(t.Context(), lossAt)
	if err != nil || !found || decision.Action != reconcile.ActionFence || decision.MayReassign {
		t.Fatalf("unproved Runner loss decision = %#v, %t, %v", decision, found, err)
	}
	var generation, assignmentCount int64
	var currentInstanceID, currentCheckpointID string
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.generation,sandbox.current_instance_id,workspace.current_checkpoint_id,
		       (SELECT count(*) FROM secondbox.assignments
		        WHERE sandbox_id=sandbox.id)
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.id=$1`, sandbox.ID,
	).Scan(&generation, &currentInstanceID, &currentCheckpointID, &assignmentCount); err != nil {
		t.Fatal(err)
	}
	if generation != ready.Generation || currentInstanceID != firstAssignment.Fence.InstanceId ||
		currentCheckpointID != checkpointID || assignmentCount != 1 {
		t.Fatalf(
			"unproved Runner loss changed authority: generation=%d instance=%q checkpoint=%q assignments=%d",
			generation, currentInstanceID, currentCheckpointID, assignmentCount,
		)
	}
	fenceDelivery := claimCrossRunnerCommand(
		t, stateStore, lostRunnerID, lostConnectionID, lossAt,
	)
	fenceCommand := fenceDelivery.Message.GetFence()
	if fenceCommand == nil ||
		fenceCommand.Fence.AssignmentId != firstAssignment.Fence.AssignmentId {
		t.Fatalf("Runner-loss Fence command = %#v", fenceDelivery.Message)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), fenceDelivery.ID, lostConnectionID, lossAt,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventFence, RunnerID: lostRunnerID,
		ConnectionID: lostConnectionID,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_FenceResult{
				FenceResult: &runnerv1.FenceResult{
					MessageId: "runner-loss-fence-result", Sequence: 3,
					Fence:                     fenceCommand.Fence,
					Result:                    runnerv1.FenceResultKind_FENCE_RESULT_KIND_STOPPED,
					TerminationEvidenceDigest: "sha256:runner-loss-termination-proof",
					Correlation:               proto.Clone(fenceCommand.Correlation).(*runnerv1.Correlation),
				},
			},
		},
	}, lossAt.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	reconcileStore.Close()
	restartedReconcileStore, err := reconcile.NewPostgresStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedReconcileStore.Close)
	restartedWorker := assignmentWorker
	restartedWorker.Store = restartedReconcileStore
	restartedWorker.WorkerID = "runner-loss-assignment-b"
	decision, found, err = restartedWorker.RunOnce(t.Context(), lossAt.Add(2*time.Millisecond))
	if err != nil || !found || decision.Action != reconcile.ActionAdvanceGeneration {
		t.Fatalf("restarted Runner-loss decision = %#v, %t, %v", decision, found, err)
	}
	var operationTenantRef, operationSubjectRef string
	if err := pool.QueryRow(t.Context(), `
		SELECT tenant_ref,subject_ref FROM secondbox.operations WHERE id=$1`,
		"operation-runner-loss-"+firstAssignment.Fence.AssignmentId,
	).Scan(&operationTenantRef, &operationSubjectRef); err != nil {
		t.Fatal(err)
	}
	if operationTenantRef != principal.TenantRef || operationSubjectRef != principal.SubjectRef {
		t.Fatalf(
			"Runner-loss replacement Operation ownership = %q/%q, want %q/%q",
			operationTenantRef, operationSubjectRef, principal.TenantRef, principal.SubjectRef,
		)
	}

	var (
		sandboxState, lifecycleReason, instanceState, guestLiveness, terminationReason     string
		leaseState, activityState, execState, execFailure, portState, materializationState string
		nextReconcileAt                                                                    time.Time
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT sandbox.generation,sandbox.state,sandbox.current_instance_id,
		       sandbox.lifecycle_termination_reason,sandbox.next_reconcile_at,
		       workspace.generation,workspace.current_checkpoint_id,
		       instance.state,instance.guest_liveness,instance.termination_reason,
		       lease.state,activity.state,exec.state,exec.infrastructure_failure_reason,
		       port.state,materialization.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.instances AS instance ON instance.id=$2
		JOIN secondbox.leases AS lease ON lease.id=$3
		JOIN secondbox.activity_sessions AS activity ON activity.id=$4
		JOIN secondbox.data_plane_sessions AS exec ON exec.id=$5
		JOIN secondbox.port_sessions AS port ON port.id=$6
		JOIN secondbox.workspace_materializations AS materialization
		  ON materialization.assignment_id=$7
		WHERE sandbox.id=$1`,
		sandbox.ID, firstAssignment.Fence.InstanceId, lease.ID, activity.ID,
		execSession.ID, portID, firstAssignment.Fence.AssignmentId,
	).Scan(
		&generation, &sandboxState, &currentInstanceID, &lifecycleReason, &nextReconcileAt,
		&assignmentCount, &currentCheckpointID,
		&instanceState, &guestLiveness, &terminationReason,
		&leaseState, &activityState, &execState, &execFailure,
		&portState, &materializationState,
	); err != nil {
		t.Fatal(err)
	}
	if generation != ready.Generation+1 || assignmentCount != ready.Generation+1 ||
		sandboxState != contracts.SandboxStateStopped || currentInstanceID != "" ||
		lifecycleReason != contracts.TerminationReasonRunnerLost ||
		nextReconcileAt.After(lossAt.Add(2*time.Millisecond)) ||
		currentCheckpointID != checkpointID ||
		instanceState != "stopped" ||
		guestLiveness != contracts.GuestLivenessLost ||
		terminationReason != contracts.TerminationReasonRunnerLost ||
		leaseState != contracts.LeaseStateFenced ||
		activityState != contracts.ActivitySessionStateClosed ||
		execState != "failed" ||
		execFailure != "INFRASTRUCTURE_FAILURE_REASON_GENERATION_FENCED" ||
		portState != contracts.PortSessionStateFenced ||
		materializationState != contracts.MaterializationStateReleased {
		t.Fatalf(
			"Runner-loss authority generation=%d workspaceGeneration=%d sandbox=%q instance=%q lifecycle=%q due=%s checkpoint=%q instanceState=%q guest=%q termination=%q lease=%q activity=%q exec=%q/%q port=%q materialization=%q",
			generation, assignmentCount, sandboxState, currentInstanceID, lifecycleReason,
			nextReconcileAt, currentCheckpointID, instanceState, guestLiveness,
			terminationReason, leaseState, activityState, execState, execFailure,
			portState, materializationState,
		)
	}
	if _, err := controlPlane.TouchSandbox(
		t.Context(), principal, sandbox.ID, ready.Generation, lease.ID,
		"runner-loss-stale-touch",
	); !errors.Is(err, ports.ErrGenerationFenced) {
		t.Fatalf("old Lease touch error = %v, want generation fence", err)
	}
	staleProgress := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_AssignmentProgress{
			AssignmentProgress: &runnerv1.AssignmentProgress{
				MessageId: "runner-loss-stale-progress", Sequence: 4,
				Fence:       firstAssignment.Fence,
				Stage:       runnerv1.AssignmentProgressStage_ASSIGNMENT_PROGRESS_STAGE_READY,
				Correlation: proto.Clone(firstAssignment.Correlation).(*runnerv1.Correlation),
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), runnercontrol.Event{
		Kind: runnercontrol.EventAssignment, RunnerID: lostRunnerID,
		ConnectionID: lostConnectionID, Message: staleProgress,
	}, lossAt.Add(3*time.Millisecond)); !errors.Is(err, runnercontrol.ErrStaleAssignmentEvidence) {
		t.Fatalf("old Runner evidence error = %v, want stale Assignment evidence", err)
	}
	staleOutput := &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_Exec{
			Exec: &runnerv1.ExecFrame{
				Fence: firstAssignment.Fence, OperationId: execSession.ID,
				StreamId: execSession.StreamID, Sequence: 1,
				Payload: &runnerv1.ExecFrame_Output{
					Output: &runnerv1.ExecOutput{
						Channel: runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT,
						Data:    []byte("stale"),
					},
				},
			},
		},
	}
	if _, err := relay.PersistInboundFrame(t.Context(), runnercontrol.InboundRelayFrame{
		RunnerID: lostRunnerID, ConnectionID: lostConnectionID, Message: staleOutput,
	}, lossAt.Add(3*time.Millisecond)); !errors.Is(err, runnercontrol.ErrRelayFence) {
		t.Fatalf("old data-plane evidence error = %v, want relay fence", err)
	}

	restartedSchedulerStore, err := scheduler.NewPostgresStore(
		t.Context(), integrationDatabaseURL,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedSchedulerStore.Close)
	restartedEffectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, restartedSchedulerStore,
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
				return fmt.Sprintf("%s-runner-loss-restart-%d", prefix, idSequence)
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte(fmt.Sprintf("%032d", idSequence)), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedEffectBroker.Close)
	restartedLifecycleWorker := lifecycle.Reconciler{
		Store: databaseStore, Effects: restartedEffectBroker,
		WorkerID: "runner-loss-lifecycle-b", ClaimDuration: time.Minute,
		PollInterval: time.Millisecond,
	}
	assertCrossRunnerLifecycleAction(
		t, &restartedLifecycleWorker, sandbox.ID,
		lossAt.Add(3*time.Millisecond), lifecycle.ActionMaterialize,
	)
	nextDelivery := claimCrossRunnerCommand(
		t, stateStore, nextRunnerID, nextConnectionID, lossAt.Add(4*time.Millisecond),
	)
	nextAssignment := nextDelivery.Message.GetAssignment()
	if nextAssignment == nil || nextAssignment.SourceCheckpointId != checkpointID ||
		nextAssignment.Fence.SandboxGeneration != uint64(ready.Generation+1) ||
		nextAssignment.Fence.AssignmentId == firstAssignment.Fence.AssignmentId ||
		bytes.Equal(nextAssignment.Fence.FencingToken, firstAssignment.Fence.FencingToken) ||
		nextAssignment.Correlation == nil ||
		nextAssignment.Correlation.OperationId == "" ||
		nextAssignment.Correlation.RequestId == "" ||
		portTunnel.RunnerID != lostRunnerID {
		t.Fatalf("replacement Runner Assignment = %#v; old Port tunnel = %#v", nextAssignment, portTunnel)
	}
	var nextRestoreFrames []*runnerv1.ControlPlaneToRunner
	if err := restoreSender.StreamRestore(
		t.Context(), nextAssignment,
		func(frame *runnerv1.ControlPlaneToRunner) error {
			nextRestoreFrames = append(nextRestoreFrames, frame)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if restored := collectCrossRunnerRestoreBytes(
		t, nextRestoreFrames, nextAssignment,
	); !bytes.Equal(restored, checkpointBytes) {
		t.Fatalf("replacement Runner restored bytes = %q, want %q", restored, checkpointBytes)
	}
	if err := stateStore.MarkCommandDelivered(
		t.Context(), nextDelivery.ID, nextConnectionID, lossAt.Add(4*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	recordCrossRunnerAssignmentReady(
		t, stateStore, nextRunnerID, nextConnectionID, nextAssignment,
		2, "replacement-after-runner-loss", lossAt.Add(5*time.Millisecond),
	)
	assertCrossRunnerLifecycleAction(
		t, &restartedLifecycleWorker, sandbox.ID,
		lossAt.Add(6*time.Millisecond), lifecycle.ActionMarkReady,
	)
	assertExclusiveCrossRunnerMaterialization(
		t, pool, sandbox.Workspace.ID, lostRunnerID, nextRunnerID,
		checkpointID, checkpointID, ready.Generation, ready.Generation+1,
		contracts.MaterializationStateReady,
	)
}
