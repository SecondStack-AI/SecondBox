package integration_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/lifecycle"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestCheckpointPublicationInterruptionRequeuesDurableCheckpointEffect(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	_, account, credential := createProjectAccountAndCredential(
		t, controlPlane, admin, "checkpoint-publication-recovery",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, account, "profile-checkpoint-publication-recovery",
	)
	principal := authenticateCredential(t, controlPlane, credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "checkpoint-publication-recovery-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	current, err := controlPlane.GetSandbox(t.Context(), principal, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlPlane.StopSandbox(
		t.Context(), principal, sandbox.ID, "checkpoint-publication-recovery-stop",
		current.Revision,
	); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(
		t.Context(), `UPDATE secondbox.sandboxes SET next_reconcile_at=$2 WHERE id<>$1`,
		sandbox.ID, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	stateStore, err := runnercontrol.NewPostgresStateStore(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateStore.Close)
	effectBroker, err := lifecycle.NewPostgresEffectBroker(
		t.Context(), integrationDatabaseURL, checkpointUnusedScheduler{},
		lifecycle.EffectBrokerConfig{
			AssignmentClaimDuration: time.Minute,
			AssignmentDeadline:      time.Minute,
			HeartbeatTimeout:        time.Minute,
			RetryLimit:              1,
			SerializationRetryLimit: 1,
			AssetCatalog:            checkpointUnusedAssetCatalog{},
			SessionCanceller:        checkpointUnusedSessionCanceller{},
			NewID: func(prefix string) string {
				return prefix + "-unused"
			},
			NewFencingToken: func() ([]byte, error) {
				return []byte("01234567890123456789012345678901"), nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(effectBroker.Close)
	reconciler := lifecycle.Reconciler{
		Store: databaseStore, Effects: effectBroker,
		WorkerID:      "checkpoint-publication-recovery-worker",
		ClaimDuration: time.Minute, PollInterval: time.Millisecond,
	}
	if decision, found, err := reconciler.RunOnce(t.Context(), now); err != nil ||
		!found || decision.Action != lifecycle.ActionDrain {
		t.Fatalf("checkpoint publication recovery drain = %#v, %t, %v", decision, found, err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), now.Add(time.Millisecond),
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint publication recovery queue = %#v, %t, %v", decision, found, err)
	}

	var effectID, initialCommandID string
	var commandPayload []byte
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.id,effect.command_id,command.payload
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.sandbox_id=$1 AND effect.kind='checkpoint'`,
		sandbox.ID,
	).Scan(&effectID, &initialCommandID, &commandPayload); err != nil {
		t.Fatal(err)
	}
	var commandFrame runnerv1.ControlPlaneToRunner
	if err := proto.Unmarshal(commandPayload, &commandFrame); err != nil {
		t.Fatal(err)
	}
	checkpointCommand := commandFrame.GetCheckpoint()
	if checkpointCommand == nil {
		t.Fatalf("checkpoint publication recovery command = %#v", commandFrame.Message)
	}

	objects := &checkpointObjectStore{
		objects:              make(map[string][]byte),
		putFailuresRemaining: 1,
	}
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
	checkpointBytes := []byte("durable checkpoint publication retry")
	checkpointSum := sha256.Sum256(checkpointBytes)
	chunkEvent := runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: seed.RunnerID,
		ConnectionID: seed.ConnectionOne,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointChunk{
				CheckpointChunk: &runnerv1.CheckpointChunk{
					MessageId: "checkpoint-publication-recovery-chunk", Sequence: 1,
					Fence: checkpointCommand.Fence, CheckpointId: checkpointCommand.CheckpointId,
					StorageObjectId: checkpointCommand.StorageObjectId, Data: checkpointBytes,
				},
			},
		},
	}
	if _, err := stateStore.RecordEvent(t.Context(), chunkEvent, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ReceiveCheckpoint(t.Context(), chunkEvent, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	resultEvent := runnercontrol.Event{
		Kind: runnercontrol.EventCheckpoint, RunnerID: seed.RunnerID,
		ConnectionID: seed.ConnectionOne,
		Message: &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_CheckpointResult{
				CheckpointResult: &runnerv1.CheckpointResult{
					MessageId: "checkpoint-publication-recovery-result", Sequence: 2,
					Fence: checkpointCommand.Fence, CheckpointId: checkpointCommand.CheckpointId,
					StorageObjectId: checkpointCommand.StorageObjectId,
					Terminal:        runnerv1.CheckpointTerminalKind_CHECKPOINT_TERMINAL_KIND_CREATED,
					Sha256:          hex.EncodeToString(checkpointSum[:]),
					SizeBytes:       uint64(len(checkpointBytes)),
					Compatibility: map[string]string{
						"architecture": "amd64", "backend": "firecracker",
						"profileRevisionId": sandbox.ProfileRevisionID, "workspaceFormat": "ext4",
						"runtimeManifestDigest":   profile.CurrentRevision.Spec.RuntimeBundleDigest,
						"toolchainManifestDigest": profile.CurrentRevision.Spec.ToolchainBundleDigest,
						"guestProtocolGeneration": "1", "mandatoryGuestFeatures": "",
					},
					TerminationEvidenceDigest: "sha256:checkpoint-publication-recovery",
					Correlation: proto.Clone(
						checkpointCommand.Correlation,
					).(*runnerv1.Correlation),
				},
			},
		},
	}
	if _, err := stateStore.RecordEvent(
		t.Context(), resultEvent, now.Add(3*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if err := receiver.ReceiveCheckpoint(
		t.Context(), resultEvent, now.Add(3*time.Millisecond),
	); err == nil {
		t.Fatal("checkpoint publication succeeded during injected object-store interruption")
	}

	var interruptedEffectState string
	if err := pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.lifecycle_effects WHERE id=$1`,
		effectID,
	).Scan(&interruptedEffectState); err != nil {
		t.Fatal(err)
	}
	if interruptedEffectState != "queued" {
		t.Fatalf(
			"interrupted checkpoint effect state = %q, want queued for durable retry",
			interruptedEffectState,
		)
	}

	retryAt := now.Add(2 * time.Minute)
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.lifecycle_effects SET effect_deadline=$2,updated_at=$2 WHERE id=$1`,
		effectID, retryAt.Add(-time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE secondbox.runner_commands
		SET state='delivered',target_connection_id=$2,delivered_at=$3,updated_at=$3
		WHERE id=$1`,
		initialCommandID, seed.ConnectionOne, retryAt.Add(-time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if decision, found, err := reconciler.RunOnce(
		t.Context(), retryAt,
	); err != nil || !found || decision.Action != lifecycle.ActionCheckpoint {
		t.Fatalf("checkpoint publication durable retry = %#v, %t, %v", decision, found, err)
	}

	var effectState, retryCommandID, retryCommandState string
	var retryCount int64
	if err := pool.QueryRow(t.Context(), `
		SELECT effect.state,effect.retry_count,effect.command_id,command.state
		FROM secondbox.lifecycle_effects AS effect
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		WHERE effect.id=$1`,
		effectID,
	).Scan(&effectState, &retryCount, &retryCommandID, &retryCommandState); err != nil {
		t.Fatal(err)
	}
	if effectState != "queued" || retryCount != 1 ||
		retryCommandID == initialCommandID || retryCommandState != "pending" {
		t.Fatalf(
			"checkpoint publication retry evidence = state %q retry %d command %q state %q",
			effectState, retryCount, retryCommandID, retryCommandState,
		)
	}
}
