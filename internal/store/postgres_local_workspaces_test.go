package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestWorkspaceMutationIsReplayableExclusiveAndClearedWithReceipt(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "replay", now)
	input := ports.WorkspaceMutationInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SandboxID: sandboxID, WorkspaceID: workspaceID, HomeRunnerID: "runner-home",
		Kind: "start", MutationID: "mutation-start", EffectID: "effect-start",
		OperationID: "operation-start", ExpectedGeneration: 3, TargetGeneration: 3, Now: now,
	}
	acquired, created, err := store.AcquireWorkspaceMutation(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !created || acquired.Mutation.State != "requested" || acquired.HomeRunnerID != "runner-home" {
		t.Fatalf("acquired Workspace = %#v, created=%t", acquired, created)
	}
	replayed, created, err := store.AcquireWorkspaceMutation(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if created || replayed.Mutation.ID != input.MutationID {
		t.Fatalf("replayed Workspace = %#v, created=%t", replayed, created)
	}
	conflict := input
	conflict.MutationID = "mutation-other"
	conflict.EffectID = "effect-other"
	if _, _, err := store.AcquireWorkspaceMutation(t.Context(), conflict); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("conflicting acquisition error = %v", err)
	}
	completed, err := store.CompleteWorkspaceMutation(t.Context(), ports.WorkspaceMutationCompletion{
		WorkspaceMutationInput: input,
		WorkspaceState:         "ready",
		CommittedGeneration:    3,
		LocalReceipt:           map[string]any{"operationId": input.MutationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Mutation.State != "" || completed.LocalReceipt["operationId"] != input.MutationID {
		t.Fatalf("completed Workspace = %#v", completed)
	}
	replayedCompletion, err := store.CompleteWorkspaceMutation(t.Context(), ports.WorkspaceMutationCompletion{
		WorkspaceMutationInput: input,
		WorkspaceState:         "ready",
		CommittedGeneration:    3,
		LocalReceipt:           map[string]any{"operationId": input.MutationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayedCompletion.Generation != 3 {
		t.Fatalf("completion replay generation = %d", replayedCompletion.Generation)
	}
}

func TestWorkspaceMutationConcurrentAdmissionHasOneWinner(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "concurrent", now)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, _, err := store.AcquireWorkspaceMutation(t.Context(), ports.WorkspaceMutationInput{
				TenantRef: "tenant-local", SubjectRef: "subject-local",
				SandboxID: sandboxID, WorkspaceID: workspaceID, HomeRunnerID: "runner-home",
				Kind: "snapshot-create", MutationID: fmt.Sprintf("mutation-%d", index),
				EffectID: fmt.Sprintf("effect-%d", index), OperationID: fmt.Sprintf("operation-%d", index),
				ExpectedGeneration: 3, TargetGeneration: 3, Now: now,
			})
			results <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ports.ErrWorkspaceMutation):
			conflicts++
		default:
			t.Fatalf("concurrent acquisition error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent acquisition successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestLifecycleStartAdmissionOwnsWorkspaceMutationBeforeDesiredStateChanges(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 30, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "start-admission", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	operation := contracts.Operation{
		ID: "operation-start-admission", Kind: "start",
		State: contracts.OperationStatePending, RequestID: "request-start-admission",
		CreatedAt: now, UpdatedAt: now,
	}
	stored, err := store.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{
		Principal: contracts.Principal{
			TenantRef: "tenant-local", SubjectRef: "subject-local",
		},
		SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning,
		Operation: operation, Now: now, ExpectedRevision: 1,
		IdempotencyKey: "start-admission", RequestHash: "start-admission-hash",
		IdempotencyEnds: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != operation.ID {
		t.Fatalf("stored start Operation = %#v", stored)
	}
	var desiredState, mutationKind, mutationID, mutationOperationID, mutationState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT sandbox.desired_state,workspace.mutation_kind,workspace.mutation_id,
		       workspace.mutation_operation_id,workspace.mutation_state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		WHERE sandbox.id=$1`,
		sandboxID,
	).Scan(
		&desiredState, &mutationKind, &mutationID, &mutationOperationID, &mutationState,
	); err != nil {
		t.Fatal(err)
	}
	if desiredState != contracts.SandboxDesiredStateRunning ||
		mutationKind != "start" ||
		mutationID != operation.ID ||
		mutationOperationID != operation.ID ||
		mutationState != "queued" {
		t.Fatalf(
			"desired=%q mutation=%q/%q/%q/%q",
			desiredState, mutationKind, mutationID, mutationOperationID, mutationState,
		)
	}
	if _, _, err := store.AcquireWorkspaceMutation(t.Context(), ports.WorkspaceMutationInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SandboxID: sandboxID, WorkspaceID: workspaceID, HomeRunnerID: "runner-home",
		Kind: "snapshot_create", MutationID: "conflicting-snapshot",
		EffectID: "conflicting-snapshot", OperationID: "conflicting-snapshot",
		ExpectedGeneration: 3, TargetGeneration: 3, Now: now.Add(time.Second),
	}); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("Snapshot mutation after start admission error = %v", err)
	}
	deleteOperation := contracts.Operation{
		ID: "operation-delete-after-start", Kind: "delete",
		State: contracts.OperationStatePending, RequestID: "request-delete-after-start",
		CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
	}
	if _, err := store.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{
		Principal: contracts.Principal{
			TenantRef: "tenant-local", SubjectRef: "subject-local",
		},
		SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateDeleted,
		Operation: deleteOperation, Now: now.Add(time.Second), ExpectedRevision: 2,
		IdempotencyKey: "delete-after-start", RequestHash: "delete-after-start-hash",
		IdempotencyEnds: now.Add(time.Hour),
	}); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("Sandbox delete after start admission error = %v", err)
	}
}

func TestSandboxDeleteIntentDominatesPendingWorkspaceCreationWithoutReplacingItsReceiptSlot(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 35, 0, 0, time.UTC)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			'workspace-delete-create','tenant-local','subject-local','sandbox-delete-create',
			'runner-home','creating',8589934592,1,'create','effect-create-pending',
			'effect-create-pending','operation-create-pending',1,1,'queued','{}',$1,$1
		);
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			'sandbox-delete-create','tenant-local','subject-local','profile','revision',
			'creating','running',1,'workspace-delete-create','','{}','{}',1,$1,$1
		)`,
		pgx.QueryExecModeSimpleProtocol, now,
	); err != nil {
		t.Fatal(err)
	}
	operation := localTestOperation("operation-delete-create", "delete", now)
	if _, err := store.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{
		Principal: contracts.Principal{
			TenantRef: "tenant-local", SubjectRef: "subject-local",
		},
		SandboxID:    "sandbox-delete-create",
		DesiredState: contracts.SandboxDesiredStateDeleted,
		Operation:    operation, Now: now, ExpectedRevision: 1,
		IdempotencyKey: "delete-create", RequestHash: "delete-create-hash",
		IdempotencyEnds: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var desiredState, mutationKind, mutationID, mutationState, operationState string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT sandbox.desired_state,workspace.mutation_kind,workspace.mutation_id,
		       workspace.mutation_state,operation.state
		FROM secondbox.sandboxes AS sandbox
		JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		JOIN secondbox.operations AS operation ON operation.id=$2
		WHERE sandbox.id=$1`,
		"sandbox-delete-create", operation.ID,
	).Scan(
		&desiredState, &mutationKind, &mutationID, &mutationState, &operationState,
	); err != nil {
		t.Fatal(err)
	}
	if desiredState != "deleted" ||
		mutationKind != "create" ||
		mutationID != "effect-create-pending" ||
		mutationState != "queued" ||
		operationState != "pending" {
		t.Fatalf(
			"delete/create desired=%q mutation=%q/%q/%q operation=%q",
			desiredState, mutationKind, mutationID, mutationState, operationState,
		)
	}
}

func TestConcurrentStartAndRestoreSerializeWithoutDeadlock(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 40, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "start-restore", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(t, store, "start-restore", sandboxID, workspaceID, now)
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		<-start
		_, err := store.SetSandboxDesiredState(ctx, ports.LifecycleIntentInput{
			Principal: contracts.Principal{
				TenantRef: "tenant-local", SubjectRef: "subject-local",
			},
			SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning,
			Operation: localTestOperation("operation-concurrent-start", "start", now),
			Now:       now, ExpectedRevision: 1, IdempotencyKey: "concurrent-start",
			RequestHash: "concurrent-start-hash", IdempotencyEnds: now.Add(time.Hour),
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := store.RestoreSnapshot(ctx, localRestoreInput(
			sandboxID, snapshotID, "concurrent-start-restore", now,
		))
		results <- err
	}()
	close(start)
	assertOneMutationWinner(t, results)
}

func TestConcurrentSnapshotDeleteAndRestoreSerializeWithoutDeadlock(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 50, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "delete-restore", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(t, store, "delete-restore", sandboxID, workspaceID, now)
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		<-start
		_, err := store.DeleteSnapshot(ctx, ports.SnapshotDeletionInput{
			TenantRef: "tenant-local", SubjectRef: "subject-local", SnapshotID: snapshotID,
			Operation: localTestOperation("operation-concurrent-delete", "snapshot_delete", now),
			EffectID:  "effect-concurrent-delete", CommandID: "command-concurrent-delete",
			FencingToken:   []byte("01234567890123456789012345678901"),
			IdempotencyKey: "concurrent-delete", RequestHash: "concurrent-delete-hash",
			IdempotencyEnds: now.Add(time.Hour), Now: now,
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := store.RestoreSnapshot(ctx, localRestoreInput(
			sandboxID, snapshotID, "concurrent-delete-restore", now,
		))
		results <- err
	}()
	close(start)
	assertOneMutationWinner(t, results)
}

func TestSnapshotDeleteQueuesWhileHomeRunnerIsOffline(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 25, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "delete-offline", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(
		t, store, "delete-offline", sandboxID, workspaceID, now,
	)
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.runners
		SET state='offline',active_connection_id='',updated_at=$2
		WHERE id=$1`,
		"runner-home", now,
	); err != nil {
		t.Fatal(err)
	}
	operation := localTestOperation(
		"operation-delete-offline",
		"snapshot_delete",
		now,
	)
	stored, err := store.DeleteSnapshot(t.Context(), ports.SnapshotDeletionInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local", SnapshotID: snapshotID,
		Operation: operation, EffectID: "effect-delete-offline",
		CommandID:      "command-delete-offline",
		FencingToken:   []byte("01234567890123456789012345678901"),
		IdempotencyKey: "delete-offline", RequestHash: "delete-offline-hash",
		IdempotencyEnds: now.Add(time.Hour), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != operation.ID || stored.State != contracts.OperationStatePending {
		t.Fatalf("offline Snapshot delete Operation = %#v", stored)
	}
	var snapshotState, commandState, runnerID, mutationKind string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT snapshot.state,command.state,command.runner_id,workspace.mutation_kind
		FROM secondbox.snapshots AS snapshot
		JOIN secondbox.workspaces AS workspace ON workspace.id=snapshot.workspace_id
		JOIN secondbox.runner_commands AS command ON command.id='command-delete-offline'
		WHERE snapshot.id=$1`,
		snapshotID,
	).Scan(&snapshotState, &commandState, &runnerID, &mutationKind); err != nil {
		t.Fatal(err)
	}
	if snapshotState != "deleting" ||
		commandState != "pending" ||
		runnerID != "runner-home" ||
		mutationKind != "snapshot_delete" {
		t.Fatalf(
			"offline delete state snapshot=%q command=%q runner=%q mutation=%q",
			snapshotState,
			commandState,
			runnerID,
			mutationKind,
		)
	}
}

func TestSnapshotDeleteRejectsDatabaseCommittedUnfinalizedRestore(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 27, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "delete-unfinalized", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(
		t, store, "delete-unfinalized", sandboxID, workspaceID, now,
	)
	restore := localRestoreInput(
		sandboxID,
		snapshotID,
		"delete-unfinalized",
		now,
	)
	if _, err := store.RestoreSnapshot(t.Context(), restore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.workspace_restores
		SET state='database_committed',database_committed_at=$2,updated_at=$2
		WHERE id=$1;
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',updated_at=$2
		WHERE id=$3`,
		pgx.QueryExecModeSimpleProtocol,
		restore.RestoreID,
		now.Add(time.Second),
		workspaceID,
	); err != nil {
		t.Fatal(err)
	}
	_, err := store.DeleteSnapshot(t.Context(), ports.SnapshotDeletionInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local", SnapshotID: snapshotID,
		Operation: localTestOperation(
			"operation-delete-unfinalized",
			"snapshot_delete",
			now.Add(2*time.Second),
		),
		EffectID: "effect-delete-unfinalized", CommandID: "command-delete-unfinalized",
		FencingToken:   []byte("01234567890123456789012345678901"),
		IdempotencyKey: "delete-unfinalized", RequestHash: "delete-unfinalized-hash",
		IdempotencyEnds: now.Add(time.Hour), Now: now.Add(2 * time.Second),
	})
	if !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("delete Snapshot with unfinalized restore error = %v", err)
	}
	var state string
	if err := store.pool.QueryRow(t.Context(), `
		SELECT state FROM secondbox.snapshots WHERE id=$1`,
		snapshotID,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ready" {
		t.Fatalf("Snapshot state after rejected delete = %q", state)
	}
}

func TestExpiredSnapshotQueuesLocalDeleteWithoutRetainedByteAccounting(t *testing.T) {
	store := openStoreTest(t)
	createdAt := time.Date(2026, 7, 28, 18, 29, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(
		t, store, "retention-delete", createdAt,
	)
	seedLocalWorkspacePolicyAndRunner(t, store, createdAt)
	snapshotID := seedReadyLocalSnapshot(
		t, store, "retention-delete", sandboxID, workspaceID, createdAt,
	)
	now := createdAt.Add(25 * time.Hour)
	queued, err := store.QueueExpiredSnapshotDelete(
		t.Context(),
		ports.SnapshotRetentionInput{
			OperationID: "operation-retention-delete",
			EffectID:    "effect-retention-delete",
			CommandID:   "command-retention-delete",
			RequestID:   "request-retention-delete",
			FencingToken: []byte(
				"01234567890123456789012345678901",
			),
			Now: now,
		},
	)
	if err != nil || !queued {
		t.Fatalf("expired Snapshot queue = %t, %v", queued, err)
	}
	var (
		snapshotState, operationState, effectState string
		commandState, mutationKind, commandKind    string
	)
	var payload []byte
	if err := store.pool.QueryRow(t.Context(), `
		SELECT snapshot.state,operation.state,effect.state,command.state,
		       workspace.mutation_kind,command.kind,command.payload
		FROM secondbox.snapshots AS snapshot
		JOIN secondbox.operations AS operation ON operation.id=snapshot.operation_id
		JOIN secondbox.lifecycle_effects AS effect ON effect.id=snapshot.effect_id
		JOIN secondbox.runner_commands AS command ON command.id=effect.command_id
		JOIN secondbox.workspaces AS workspace ON workspace.id=snapshot.workspace_id
		WHERE snapshot.id=$1`,
		snapshotID,
	).Scan(
		&snapshotState,
		&operationState,
		&effectState,
		&commandState,
		&mutationKind,
		&commandKind,
		&payload,
	); err != nil {
		t.Fatal(err)
	}
	envelope := &runnerv1.ControlPlaneToRunner{}
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	if snapshotState != "deleting" ||
		operationState != string(contracts.OperationStatePending) ||
		effectState != "queued" ||
		commandState != "pending" ||
		mutationKind != "snapshot_delete" ||
		commandKind != "local-workspace" ||
		envelope.GetLocalWorkspace().GetKind() !=
			runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_SNAPSHOT_DELETE {
		t.Fatalf(
			"retention delete state snapshot=%q operation=%q effect=%q command=%q/%q mutation=%q envelope=%#v",
			snapshotState,
			operationState,
			effectState,
			commandState,
			commandKind,
			mutationKind,
			envelope.GetLocalWorkspace(),
		)
	}
}

func TestSnapshotCreateAndDeleteAreIdempotentDurableEffects(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 40, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "snapshot-idempotency", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	create := localSnapshotCreateInput(
		sandboxID,
		"snapshot-local-idempotency",
		"snapshot-idempotency",
		now,
	)
	firstCreate, err := store.CreateSnapshot(t.Context(), create)
	if err != nil {
		t.Fatal(err)
	}
	replayedCreate, err := store.CreateSnapshot(t.Context(), create)
	if err != nil {
		t.Fatal(err)
	}
	if replayedCreate.ID != firstCreate.ID {
		t.Fatalf(
			"Snapshot create replay Operation = %q, want %q",
			replayedCreate.ID,
			firstCreate.ID,
		)
	}
	var createEffectCount, createCommandCount int
	if err := store.pool.QueryRow(t.Context(), `
		SELECT
		  (SELECT count(*) FROM secondbox.lifecycle_effects WHERE id=$1),
		  (SELECT count(*) FROM secondbox.runner_commands WHERE id=$2)`,
		create.EffectID,
		create.CommandID,
	).Scan(&createEffectCount, &createCommandCount); err != nil {
		t.Fatal(err)
	}
	if createEffectCount != 1 || createCommandCount != 1 {
		t.Fatalf(
			"Snapshot create durable effects = %d/%d",
			createEffectCount,
			createCommandCount,
		)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.snapshots
		SET state='ready',runner_receipt_json='{"durable":true}',updated_at=$2
		WHERE id=$1;
		UPDATE secondbox.workspaces
		SET mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',updated_at=$2
		WHERE id=$3`,
		pgx.QueryExecModeSimpleProtocol,
		create.Snapshot.ID,
		now.Add(time.Second),
		workspaceID,
	); err != nil {
		t.Fatal(err)
	}
	deleteInput := ports.SnapshotDeletionInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SnapshotID: create.Snapshot.ID,
		Operation: localTestOperation(
			"operation-snapshot-idempotency-delete",
			"snapshot_delete",
			now.Add(2*time.Second),
		),
		EffectID:        "effect-snapshot-idempotency-delete",
		CommandID:       "command-snapshot-idempotency-delete",
		FencingToken:    []byte("01234567890123456789012345678901"),
		IdempotencyKey:  "idempotency-snapshot-idempotency-delete",
		RequestHash:     "hash-snapshot-idempotency-delete",
		IdempotencyEnds: now.Add(time.Hour),
		Now:             now.Add(2 * time.Second),
	}
	firstDelete, err := store.DeleteSnapshot(t.Context(), deleteInput)
	if err != nil {
		t.Fatal(err)
	}
	replayedDelete, err := store.DeleteSnapshot(t.Context(), deleteInput)
	if err != nil {
		t.Fatal(err)
	}
	if replayedDelete.ID != firstDelete.ID {
		t.Fatalf(
			"Snapshot delete replay Operation = %q, want %q",
			replayedDelete.ID,
			firstDelete.ID,
		)
	}
	var deleteEffectCount, deleteCommandCount int
	if err := store.pool.QueryRow(t.Context(), `
		SELECT
		  (SELECT count(*) FROM secondbox.lifecycle_effects WHERE id=$1),
		  (SELECT count(*) FROM secondbox.runner_commands WHERE id=$2)`,
		deleteInput.EffectID,
		deleteInput.CommandID,
	).Scan(&deleteEffectCount, &deleteCommandCount); err != nil {
		t.Fatal(err)
	}
	if deleteEffectCount != 1 || deleteCommandCount != 1 {
		t.Fatalf(
			"Snapshot delete durable effects = %d/%d",
			deleteEffectCount,
			deleteCommandCount,
		)
	}
}

func TestSnapshotCreateRequiresStoppedSandboxAndOnlineHomeRunner(t *testing.T) {
	tests := map[string]struct {
		mutate func(*testing.T, *PostgresControlPlaneStore, string, time.Time)
		want   error
	}{
		"running Sandbox": {
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				sandboxID string,
				now time.Time,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET state='ready',desired_state='running',updated_at=$2
					WHERE id=$1`,
					sandboxID,
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrSnapshotUnavailable,
		},
		"offline home runner": {
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				_ string,
				now time.Time,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.runners
					SET state='offline',active_connection_id='',updated_at=$2
					WHERE id=$1`,
					"runner-home",
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrHomeRunnerUnavailable,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := openStoreTest(t)
			now := time.Date(2026, 7, 29, 18, 45, 0, 0, time.UTC)
			_, sandboxID := seedLocalWorkspace(t, store, "snapshot-"+name, now)
			seedLocalWorkspacePolicyAndRunner(t, store, now)
			test.mutate(t, store, sandboxID, now.Add(time.Second))
			_, err := store.CreateSnapshot(
				t.Context(),
				localSnapshotCreateInput(
					sandboxID,
					"snapshot-local-validation",
					"snapshot-validation",
					now.Add(2*time.Second),
				),
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("Snapshot create error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestConcurrentSnapshotCreateEnforcesSubjectCountQuota(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 50, 0, 0, time.UTC)
	_, sandboxA := seedLocalWorkspace(t, store, "snapshot-quota-a", now)
	_, sandboxB := seedLocalWorkspace(t, store, "snapshot-quota-b", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	var existingSnapshots int64
	if err := store.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.snapshots
		WHERE tenant_ref=$1 AND subject_ref=$2
		  AND state IN ('creating','ready','deleting')`,
		"tenant-local",
		"subject-local",
	).Scan(&existingSnapshots); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		UPDATE secondbox.subject_quotas
		SET max_snapshots=$3,updated_at=$4
		WHERE tenant_ref=$1 AND subject_ref=$2`,
		"tenant-local",
		"subject-local",
		existingSnapshots+1,
		now,
	); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, sandboxID := range []string{sandboxA, sandboxB} {
		go func(index int, sandboxID string) {
			<-start
			_, err := store.CreateSnapshot(
				t.Context(),
				localSnapshotCreateInput(
					sandboxID,
					fmt.Sprintf("snapshot-local-quota-%d", index),
					fmt.Sprintf("snapshot-quota-%d", index),
					now,
				),
			)
			results <- err
		}(index, sandboxID)
	}
	close(start)
	var successes, quotaFailures int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ports.ErrQuotaExceeded):
			quotaFailures++
		default:
			t.Fatalf("concurrent Snapshot quota error = %v", err)
		}
	}
	if successes != 1 || quotaFailures != 1 {
		t.Fatalf(
			"concurrent Snapshot quota successes=%d failures=%d",
			successes,
			quotaFailures,
		)
	}
}

func TestConcurrentSnapshotCreateAndRestoreSerializeWithoutDeadlock(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 55, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "create-restore", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(t, store, "create-restore", sandboxID, workspaceID, now)
	retainUntil := now.Add(24 * time.Hour)
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		<-start
		_, err := store.CreateSnapshot(ctx, ports.SnapshotCreationInput{
			Snapshot: contracts.Snapshot{
				ID: "snapshot-concurrent-create", TenantRef: "tenant-local",
				SubjectRef: "subject-local", SandboxID: sandboxID, Name: "concurrent",
				Metadata: map[string]string{}, RetainUntil: &retainUntil, CreatedAt: now,
			},
			Operation: localTestOperation(
				"operation-concurrent-create", "snapshot_create", now,
			),
			EffectID: "effect-concurrent-create", CommandID: "command-concurrent-create",
			FencingToken:   []byte("01234567890123456789012345678901"),
			IdempotencyKey: "concurrent-create", RequestHash: "concurrent-create-hash",
			IdempotencyEnds: now.Add(time.Hour), ExpectedRevision: 1,
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := store.RestoreSnapshot(ctx, localRestoreInput(
			sandboxID, snapshotID, "concurrent-create-restore", now,
		))
		results <- err
	}()
	close(start)
	assertOneMutationWinner(t, results)
}

func TestConcurrentSandboxDeleteAndRestoreSerializeWithoutDeadlock(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 17, 58, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "sandbox-delete-restore", now)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(
		t, store, "sandbox-delete-restore", sandboxID, workspaceID, now,
	)
	start := make(chan struct{})
	results := make(chan error, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		<-start
		_, err := store.SetSandboxDesiredState(ctx, ports.LifecycleIntentInput{
			Principal: contracts.Principal{
				TenantRef: "tenant-local", SubjectRef: "subject-local",
			},
			SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateDeleted,
			Operation: localTestOperation("operation-concurrent-sandbox-delete", "delete", now),
			Now:       now, ExpectedRevision: 1, IdempotencyKey: "concurrent-sandbox-delete",
			RequestHash:     "concurrent-sandbox-delete-hash",
			IdempotencyEnds: now.Add(time.Hour),
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := store.RestoreSnapshot(ctx, localRestoreInput(
			sandboxID, snapshotID, "concurrent-sandbox-delete-restore", now,
		))
		results <- err
	}()
	close(start)
	assertOneMutationWinner(t, results)
}

func TestSnapshotRestoreAdmissionPersistsEveryLocalPhaseIdentity(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(t, store, "restore", now)
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,backend_kind,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool-local','runner-home','ready','["amd64"]',
			'["compute","local-workspace"]','{}','[1]',1,1,'test','connection-home',0,
			'active','{}','` + placementTestCacheJSON + `','firecracker',0,0,$1,1,$1,$1
		) ON CONFLICT (id) DO UPDATE SET state='ready',last_seen_at=EXCLUDED.last_seen_at`,
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			'snapshot-local-restore','tenant-local','subject-local',$1,$2,'runner-home',
			'operation-snapshot-create','effect-snapshot-create','{}',3,'before',8589934592,
			'{}','ready',$4,$3,$3,NULL
		)`,
		sandboxID, workspaceID, now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	operation := contracts.Operation{
		ID: "operation-restore", SandboxID: sandboxID, Kind: "snapshot_restore",
		State: contracts.OperationStatePending, RequestID: "request-restore",
		CreatedAt: now, UpdatedAt: now,
	}
	stored, err := store.RestoreSnapshot(t.Context(), ports.SnapshotRestoreInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SandboxID: sandboxID, SnapshotID: "snapshot-local-restore",
		Operation: operation, RestoreID: "restore-local",
		PrepareEffectID: "effect-restore-prepare", SwapEffectID: "effect-restore-swap",
		FinalizeEffectID: "effect-restore-finalize", AbortEffectID: "effect-restore-abort",
		PrepareCommandID: "command-restore-prepare", SwapCommandID: "command-restore-swap",
		FinalizeCommandID: "command-restore-finalize", AbortCommandID: "command-restore-abort",
		FencingToken:   []byte("01234567890123456789012345678901"),
		IdempotencyKey: "restore-idempotency", RequestHash: "restore-hash",
		IdempotencyEnds: now.Add(time.Hour), ExpectedRevision: 1, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != operation.ID || stored.Snapshot == nil ||
		stored.Snapshot.ID != "snapshot-local-restore" {
		t.Fatalf("stored restore Operation = %#v", stored)
	}
	var (
		restoreState, mutationKind, mutationID, mutationEffectID           string
		prepareCommandID, swapCommandID, finalizeCommandID, abortCommandID string
		sandboxRevision                                                    int64
		payload                                                            []byte
	)
	if err := store.pool.QueryRow(t.Context(), `
		SELECT restore.state,restore.prepare_command_id,restore.swap_command_id,
		       restore.finalize_command_id,restore.abort_command_id,
		       workspace.mutation_kind,workspace.mutation_id,workspace.mutation_effect_id,
		       sandbox.revision,command.payload
		FROM secondbox.workspace_restores AS restore
		JOIN secondbox.workspaces AS workspace ON workspace.id=restore.workspace_id
		JOIN secondbox.sandboxes AS sandbox ON sandbox.id=restore.sandbox_id
		JOIN secondbox.runner_commands AS command
		  ON command.id=restore.prepare_command_id
		WHERE restore.id='restore-local'`,
	).Scan(
		&restoreState, &prepareCommandID, &swapCommandID,
		&finalizeCommandID, &abortCommandID,
		&mutationKind, &mutationID, &mutationEffectID,
		&sandboxRevision, &payload,
	); err != nil {
		t.Fatal(err)
	}
	if restoreState != "requested" ||
		prepareCommandID != "command-restore-prepare" ||
		swapCommandID != "command-restore-swap" ||
		finalizeCommandID != "command-restore-finalize" ||
		abortCommandID != "command-restore-abort" ||
		mutationKind != "snapshot_restore" ||
		mutationID != "restore-local" ||
		mutationEffectID != "effect-restore-prepare" ||
		sandboxRevision != 2 {
		t.Fatalf(
			"restore state=%q commands=%q/%q/%q/%q mutation=%q/%q/%q revision=%d",
			restoreState, prepareCommandID, swapCommandID, finalizeCommandID,
			abortCommandID, mutationKind, mutationID, mutationEffectID, sandboxRevision,
		)
	}
	envelope := &runnerv1.ControlPlaneToRunner{}
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	command := envelope.GetLocalWorkspace()
	if command == nil ||
		command.Kind != runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RESTORE_PREPARE ||
		command.OperationId != operation.ID ||
		command.EffectId != "effect-restore-prepare" ||
		command.ExpectedGeneration != 3 ||
		command.NextGeneration != 4 {
		t.Fatalf("restore prepare command = %#v", command)
	}
}

func assertOneMutationWinner(t *testing.T, results <-chan error) {
	t.Helper()
	var successes, conflicts int
	for index := 0; index < 2; index++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ports.ErrWorkspaceMutation),
				errors.Is(err, ports.ErrRevisionConflict):
				conflicts++
			default:
				t.Fatalf("concurrent mutation error = %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("concurrent Workspace mutations deadlocked")
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent mutation successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestSnapshotRestoreAdmissionRejectsEveryInvalidAuthority(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
		mutate func(
			*testing.T,
			*PostgresControlPlaneStore,
			string,
			string,
			string,
			time.Time,
			*ports.SnapshotRestoreInput,
		)
		want error
	}{
		{
			name:   "stale revision",
			suffix: "stale-revision",
			mutate: func(
				_ *testing.T,
				_ *PostgresControlPlaneStore,
				_ string,
				_ string,
				_ string,
				_ time.Time,
				input *ports.SnapshotRestoreInput,
			) {
				input.ExpectedRevision = 99
			},
			want: ports.ErrRevisionConflict,
		},
		{
			name:   "running Sandbox",
			suffix: "running",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				sandboxID string,
				_ string,
				_ string,
				now time.Time,
				_ *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET state='ready',desired_state='running',updated_at=$2
					WHERE id=$1`,
					sandboxID,
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrSnapshotUnavailable,
		},
		{
			name:   "active Instance",
			suffix: "active-instance",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				sandboxID string,
				_ string,
				_ string,
				now time.Time,
				_ *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET current_instance_id='instance-still-owning-workspace',updated_at=$2
					WHERE id=$1`,
					sandboxID,
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrWorkspaceMutation,
		},
		{
			name:   "lifecycle claim",
			suffix: "lifecycle-claim",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				sandboxID string,
				_ string,
				_ string,
				now time.Time,
				_ *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.sandboxes
					SET reconcile_owner='worker-active',
					    reconcile_claim_expires_at=$2,updated_at=$3
					WHERE id=$1`,
					sandboxID,
					now.Add(time.Minute),
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrWorkspaceMutation,
		},
		{
			name:   "Workspace not ready",
			suffix: "workspace-not-ready",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				_ string,
				workspaceID string,
				_ string,
				now time.Time,
				_ *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.workspaces
					SET state='failed',updated_at=$2 WHERE id=$1`,
					workspaceID,
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrGenerationFenced,
		},
		{
			name:   "offline home runner",
			suffix: "offline-home",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				_ string,
				_ string,
				_ string,
				now time.Time,
				_ *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				if _, err := store.pool.Exec(t.Context(), `
					UPDATE secondbox.runners
					SET state='offline',active_connection_id='',updated_at=$2
					WHERE id=$1`,
					"runner-home",
					now,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: ports.ErrHomeRunnerUnavailable,
		},
		{
			name:   "Snapshot owned by another Sandbox",
			suffix: "wrong-sandbox",
			mutate: func(
				t *testing.T,
				store *PostgresControlPlaneStore,
				_ string,
				_ string,
				_ string,
				now time.Time,
				input *ports.SnapshotRestoreInput,
			) {
				t.Helper()
				otherWorkspaceID, otherSandboxID := seedLocalWorkspace(
					t,
					store,
					"restore-other-owner",
					now,
				)
				input.SnapshotID = seedReadyLocalSnapshot(
					t,
					store,
					"restore-other-owner",
					otherSandboxID,
					otherWorkspaceID,
					now,
				)
			},
			want: ports.ErrSnapshotNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openStoreTest(t)
			now := time.Date(2026, 7, 29, 18, 10, 0, 0, time.UTC)
			workspaceID, sandboxID := seedLocalWorkspace(
				t,
				store,
				"restore-validation-"+test.suffix,
				now,
			)
			seedLocalWorkspacePolicyAndRunner(t, store, now)
			snapshotID := seedReadyLocalSnapshot(
				t,
				store,
				"restore-validation-"+test.suffix,
				sandboxID,
				workspaceID,
				now,
			)
			input := localRestoreInput(
				sandboxID,
				snapshotID,
				"restore-validation-"+test.suffix,
				now,
			)
			test.mutate(
				t,
				store,
				sandboxID,
				workspaceID,
				snapshotID,
				now.Add(time.Second),
				&input,
			)
			if _, err := store.RestoreSnapshot(
				t.Context(),
				input,
			); !errors.Is(err, test.want) {
				t.Fatalf("Snapshot restore error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSnapshotRestoreMutationBlocksEveryConflictingSandboxAction(t *testing.T) {
	store := openStoreTest(t)
	now := time.Date(2026, 7, 29, 18, 20, 0, 0, time.UTC)
	workspaceID, sandboxID := seedLocalWorkspace(
		t,
		store,
		"restore-blocks-actions",
		now,
	)
	seedLocalWorkspacePolicyAndRunner(t, store, now)
	snapshotID := seedReadyLocalSnapshot(
		t,
		store,
		"restore-blocks-actions",
		sandboxID,
		workspaceID,
		now,
	)
	if _, err := store.RestoreSnapshot(
		t.Context(),
		localRestoreInput(
			sandboxID,
			snapshotID,
			"restore-blocks-actions",
			now,
		),
	); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []struct {
		kind    string
		desired string
	}{
		{kind: "start", desired: contracts.SandboxDesiredStateRunning},
		{kind: "stop", desired: contracts.SandboxDesiredStateStopped},
		{kind: "delete", desired: contracts.SandboxDesiredStateDeleted},
	} {
		_, err := store.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{
			Principal: contracts.Principal{
				TenantRef:  "tenant-local",
				SubjectRef: "subject-local",
			},
			SandboxID:    sandboxID,
			DesiredState: intent.desired,
			Operation: localTestOperation(
				"operation-restore-block-"+intent.kind,
				intent.kind,
				now.Add(time.Second),
			),
			Now:              now.Add(time.Second),
			ExpectedRevision: 2,
			IdempotencyKey:   "restore-block-" + intent.kind,
			RequestHash:      "restore-block-" + intent.kind + "-hash",
			IdempotencyEnds:  now.Add(time.Hour),
		})
		if !errors.Is(err, ports.ErrWorkspaceMutation) {
			t.Fatalf("%s during restore error = %v", intent.kind, err)
		}
	}
	create := localSnapshotCreateInput(
		sandboxID,
		"snapshot-restore-block-create",
		"restore-block-create",
		now.Add(time.Second),
	)
	create.ExpectedRevision = 2
	if _, err := store.CreateSnapshot(
		t.Context(),
		create,
	); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("Snapshot create during restore error = %v", err)
	}
	if _, err := store.DeleteSnapshot(t.Context(), ports.SnapshotDeletionInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SnapshotID: snapshotID,
		Operation: localTestOperation(
			"operation-restore-block-snapshot-delete",
			"snapshot_delete",
			now.Add(time.Second),
		),
		EffectID:        "effect-restore-block-snapshot-delete",
		CommandID:       "command-restore-block-snapshot-delete",
		FencingToken:    []byte("01234567890123456789012345678901"),
		IdempotencyKey:  "restore-block-snapshot-delete",
		RequestHash:     "restore-block-snapshot-delete-hash",
		IdempotencyEnds: now.Add(time.Hour),
		Now:             now.Add(time.Second),
	}); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("Snapshot delete during restore error = %v", err)
	}
	secondRestore := localRestoreInput(
		sandboxID,
		snapshotID,
		"restore-block-second-restore",
		now.Add(time.Second),
	)
	secondRestore.ExpectedRevision = 2
	if _, err := store.RestoreSnapshot(
		t.Context(),
		secondRestore,
	); !errors.Is(err, ports.ErrWorkspaceMutation) {
		t.Fatalf("second restore during restore error = %v", err)
	}
}

func localTestOperation(id string, kind string, now time.Time) contracts.Operation {
	return contracts.Operation{
		ID: id, Kind: kind, State: contracts.OperationStatePending,
		RequestID: "request-" + id, CreatedAt: now, UpdatedAt: now,
	}
}

func localSnapshotCreateInput(
	sandboxID string,
	snapshotID string,
	suffix string,
	now time.Time,
) ports.SnapshotCreationInput {
	retainUntil := now.Add(24 * time.Hour)
	return ports.SnapshotCreationInput{
		Snapshot: contracts.Snapshot{
			ID: snapshotID, TenantRef: "tenant-local", SubjectRef: "subject-local",
			SandboxID: sandboxID, Name: suffix, Metadata: map[string]string{},
			RetainUntil: &retainUntil, CreatedAt: now,
		},
		Operation: localTestOperation(
			"operation-"+suffix,
			"snapshot_create",
			now,
		),
		EffectID:         "effect-" + suffix,
		CommandID:        "command-" + suffix,
		FencingToken:     []byte("01234567890123456789012345678901"),
		IdempotencyKey:   "idempotency-" + suffix,
		RequestHash:      "hash-" + suffix,
		IdempotencyEnds:  now.Add(time.Hour),
		ExpectedRevision: 1,
	}
}

func localRestoreInput(
	sandboxID string,
	snapshotID string,
	suffix string,
	now time.Time,
) ports.SnapshotRestoreInput {
	return ports.SnapshotRestoreInput{
		TenantRef: "tenant-local", SubjectRef: "subject-local",
		SandboxID: sandboxID, SnapshotID: snapshotID,
		Operation:         localTestOperation("operation-"+suffix, "snapshot_restore", now),
		RestoreID:         "restore-" + suffix,
		PrepareEffectID:   "effect-" + suffix + "-prepare",
		SwapEffectID:      "effect-" + suffix + "-swap",
		FinalizeEffectID:  "effect-" + suffix + "-finalize",
		AbortEffectID:     "effect-" + suffix + "-abort",
		PrepareCommandID:  "command-" + suffix + "-prepare",
		SwapCommandID:     "command-" + suffix + "-swap",
		FinalizeCommandID: "command-" + suffix + "-finalize",
		AbortCommandID:    "command-" + suffix + "-abort",
		FencingToken:      []byte("01234567890123456789012345678901"),
		IdempotencyKey:    "idempotency-" + suffix,
		RequestHash:       "hash-" + suffix,
		IdempotencyEnds:   now.Add(time.Hour),
		ExpectedRevision:  1,
		Now:               now,
	}
}

func seedLocalWorkspacePolicyAndRunner(
	t *testing.T,
	store *PostgresControlPlaneStore,
	now time.Time,
) {
	t.Helper()
	specJSON, err := json.Marshal(contracts.ProfileRevisionSpec{
		Pool: "pool-local", Architecture: "amd64",
		RuntimeBundleDigest:   placementTestRuntimeDigest,
		ToolchainBundleDigest: placementTestToolchainDigest,
		Resources: contracts.ResourcePolicy{
			VCPUCount: 1, MemoryBytes: 1 << 30, WorkspaceBytes: 8 << 30,
			ConcurrentOperations: 4,
		},
		Startup: contracts.StartupPolicy{Mode: contracts.StartupModeColdBoot},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState: "stopped", DrainGraceSeconds: 30, IdleSeconds: 300,
			MaximumDurationSeconds: 3600, LeaseSeconds: 60,
		},
		Retention: contracts.RetentionPolicy{
			SnapshotLimit: 8, SnapshotRetentionSeconds: 86400,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.tenants (
			ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
			aggregate_quota_json,expiry_policy_json,metadata_json,revision,created_at,updated_at
		) VALUES ('tenant-local','active','[]','[]','{}','{}','{}',1,$2,$2)
		ON CONFLICT (ref) DO UPDATE SET state='active',updated_at=EXCLUDED.updated_at;
		INSERT INTO secondbox.tenant_quotas (
			tenant_ref,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
			max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
			max_application_authorities,updated_at
		) VALUES ('tenant-local',100,100,100000,1099511627776,100,100,100,100,100,$2)
		ON CONFLICT (tenant_ref) DO UPDATE SET
			max_sandboxes=EXCLUDED.max_sandboxes,
			max_active_instances=EXCLUDED.max_active_instances,
			max_cpu_millis=EXCLUDED.max_cpu_millis,
			max_memory_bytes=EXCLUDED.max_memory_bytes,
			max_snapshots=EXCLUDED.max_snapshots,
			max_port_sessions=EXCLUDED.max_port_sessions,
			max_concurrent_operations=EXCLUDED.max_concurrent_operations,
			max_active_subjects=EXCLUDED.max_active_subjects,
			max_application_authorities=EXCLUDED.max_application_authorities,
			updated_at=EXCLUDED.updated_at;
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,cleanup_operation_id,quota_json,
			metadata_json,expires_at,revision,created_at,updated_at
		) VALUES (
			'tenant-local','subject-local','active','none','',
			'{"maxSandboxes":100,"maxActiveInstances":100,"maxCpuMillis":100000,"maxMemoryBytes":1099511627776,"maxSnapshots":100,"maxPortSessions":100,"maxConcurrentOperations":100}',
			'{}',NULL,1,$2,$2
		) ON CONFLICT (tenant_ref,ref) DO UPDATE SET
			state='active',cleanup_state='none',expires_at=NULL,updated_at=EXCLUDED.updated_at;
		INSERT INTO secondbox.profile_revisions (
			id,profile_name,revision_number,spec_json,created_at
		) VALUES ('revision-local','profile-local',1,$1,$2)
		ON CONFLICT (id) DO NOTHING;
		INSERT INTO secondbox.subject_quotas (
			tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_vcpu_count,
			max_memory_bytes,max_snapshots,
			max_port_sessions,max_concurrent_operations,updated_at
		) VALUES (
			'tenant-local','subject-local',100,100,100000,1099511627776,
			100,100,100,$2
		) ON CONFLICT (tenant_ref,subject_ref) DO UPDATE SET
			max_sandboxes=EXCLUDED.max_sandboxes,
			max_active_instances=EXCLUDED.max_active_instances,
			max_vcpu_count=EXCLUDED.max_vcpu_count,
			max_memory_bytes=EXCLUDED.max_memory_bytes,
			max_snapshots=EXCLUDED.max_snapshots,
			max_port_sessions=EXCLUDED.max_port_sessions,
			max_concurrent_operations=EXCLUDED.max_concurrent_operations,
			updated_at=EXCLUDED.updated_at;
		INSERT INTO secondbox.runners (
			id,pool_name,name,state,architectures_json,capabilities_json,capacity_json,
			protocol_versions_json,guest_protocol_minimum,guest_protocol_maximum,
			software_version,active_connection_id,last_sequence,drain_phase,
			reserved_capacity_json,artifact_cache_json,backend_kind,sandbox_start_sample_count,
			sandbox_start_p95_milliseconds,last_seen_at,revision,created_at,updated_at
		) VALUES (
			'runner-home','pool-local','runner-home','ready','["amd64"]',
			'["compute","local-workspace"]','{}','[1]',1,1,'test','connection-home',0,
			'active','{}','` + placementTestCacheJSON + `','firecracker',0,0,$2,1,$2,$2
		) ON CONFLICT (id) DO UPDATE SET state='ready',last_seen_at=EXCLUDED.last_seen_at`,
		pgx.QueryExecModeSimpleProtocol, string(specJSON), now,
	); err != nil {
		t.Fatal(err)
	}
}

func seedReadyLocalSnapshot(
	t *testing.T,
	store *PostgresControlPlaneStore,
	suffix string,
	sandboxID string,
	workspaceID string,
	now time.Time,
) string {
	t.Helper()
	snapshotID := "snapshot-local-" + suffix
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.snapshots (
			id,tenant_ref,subject_ref,sandbox_id,workspace_id,home_runner_id,
			operation_id,effect_id,runner_receipt_json,source_generation,name,size_bytes,
			metadata_json,state,retain_until,created_at,updated_at,retention_ended_at
		) VALUES (
			$1,'tenant-local','subject-local',$2,$3,'runner-home',
			$4,$5,'{}',3,$6,8589934592,'{}','ready',$8,$7,$7,NULL
		)`,
		snapshotID, sandboxID, workspaceID,
		"operation-create-"+suffix, "effect-create-"+suffix, suffix,
		now, now.Add(24*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	return snapshotID
}

func seedLocalWorkspace(
	t *testing.T,
	store *PostgresControlPlaneStore,
	suffix string,
	now time.Time,
) (string, string) {
	t.Helper()
	ensureStoreTestQuotaLedgers(t, store, "tenant-local", "subject-local", now)
	workspaceID := "workspace-local-" + suffix
	sandboxID := "sandbox-local-" + suffix
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.workspaces (
			id,tenant_ref,subject_ref,sandbox_id,home_runner_id,state,logical_capacity_bytes,
			generation,mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
			mutation_expected_generation,mutation_target_generation,mutation_state,
			local_receipt_json,created_at,updated_at
		) VALUES (
			$1,'tenant-local','subject-local',$2,'runner-home','ready',8589934592,
			3,'','','','',NULL,NULL,'','{}',$3,$3
		)`,
		workspaceID, sandboxID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `
		INSERT INTO secondbox.sandboxes (
			id,tenant_ref,subject_ref,profile_name,profile_revision_id,state,desired_state,
			generation,workspace_id,current_instance_id,metadata_json,compatibility_summary_json,
			revision,created_at,updated_at
		) VALUES (
			$1,'tenant-local','subject-local','profile-local','revision-local','stopped','stopped',
			3,$2,'','{}','{}',1,$3,$3
		)`,
		sandboxID, workspaceID, now,
	); err != nil {
		t.Fatal(err)
	}
	return workspaceID, sandboxID
}
