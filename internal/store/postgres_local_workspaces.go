package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// AcquireWorkspaceMutation serializes all local workspace changes under the
// invariant row order Sandbox, Workspace, then Snapshot when present.
func (store *PostgresControlPlaneStore) AcquireWorkspaceMutation(
	ctx context.Context,
	input ports.WorkspaceMutationInput,
) (ports.HomeWorkspace, bool, error) {
	if err := validateWorkspaceMutationInput(input); err != nil {
		return ports.HomeWorkspace{}, false, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ports.HomeWorkspace{}, false, fmt.Errorf("SecondBox Workspace mutation transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	workspace, err := lockLocalWorkspaceRows(ctx, tx, input)
	if err != nil {
		return ports.HomeWorkspace{}, false, err
	}
	if workspace.HomeRunnerID != input.HomeRunnerID {
		return ports.HomeWorkspace{}, false, ports.ErrWorkspaceHomeConflict
	}
	if workspace.Generation != input.ExpectedGeneration {
		return ports.HomeWorkspace{}, false, ports.ErrGenerationFenced
	}
	if workspace.Mutation.State != "" {
		if workspace.Mutation.ID != input.MutationID ||
			workspace.Mutation.Kind != input.Kind ||
			workspace.Mutation.EffectID != input.EffectID ||
			workspace.Mutation.OperationID != input.OperationID ||
			workspace.Mutation.ExpectedGeneration != input.ExpectedGeneration ||
			workspace.Mutation.TargetGeneration != input.TargetGeneration {
			return ports.HomeWorkspace{}, false, ports.ErrWorkspaceMutation
		}
		if err := tx.Commit(ctx); err != nil {
			return ports.HomeWorkspace{}, false, fmt.Errorf("SecondBox Workspace mutation replay commit failed: %w", err)
		}
		return workspace, false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET mutation_kind=$2,mutation_id=$3,mutation_effect_id=$4,
		    mutation_operation_id=$5,mutation_expected_generation=$6,
		    mutation_target_generation=$7,mutation_state='requested',updated_at=$8
		WHERE id=$1`,
		input.WorkspaceID, input.Kind, input.MutationID, input.EffectID,
		input.OperationID, input.ExpectedGeneration, nullableTargetGeneration(input.TargetGeneration),
		input.Now.UTC(),
	); err != nil {
		return ports.HomeWorkspace{}, false, fmt.Errorf("SecondBox Workspace mutation acquisition failed: %w", err)
	}
	workspace.Mutation = ports.WorkspaceMutation{
		Kind: input.Kind, ID: input.MutationID, EffectID: input.EffectID,
		OperationID: input.OperationID, ExpectedGeneration: input.ExpectedGeneration,
		TargetGeneration: input.TargetGeneration, State: "requested",
	}
	workspace.UpdatedAt = input.Now.UTC()
	if err := tx.Commit(ctx); err != nil {
		return ports.HomeWorkspace{}, false, fmt.Errorf("SecondBox Workspace mutation commit failed: %w", err)
	}
	return workspace, true, nil
}

// CompleteWorkspaceMutation records runner evidence and clears exactly the
// matching durable slot in one transaction.
func (store *PostgresControlPlaneStore) CompleteWorkspaceMutation(
	ctx context.Context,
	input ports.WorkspaceMutationCompletion,
) (ports.HomeWorkspace, error) {
	if err := validateWorkspaceMutationInput(input.WorkspaceMutationInput); err != nil {
		return ports.HomeWorkspace{}, err
	}
	if input.WorkspaceState == "" || input.CommittedGeneration < 1 || input.LocalReceipt == nil {
		return ports.HomeWorkspace{}, errors.New("SecondBox Workspace mutation completion requires state, generation, and receipt")
	}
	receiptJSON, err := json.Marshal(input.LocalReceipt)
	if err != nil {
		return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace local receipt encoding failed: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace mutation completion transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	workspace, err := lockLocalWorkspaceRows(ctx, tx, input.WorkspaceMutationInput)
	if err != nil {
		return ports.HomeWorkspace{}, err
	}
	if workspace.HomeRunnerID != input.HomeRunnerID {
		return ports.HomeWorkspace{}, ports.ErrWorkspaceHomeConflict
	}
	if workspace.Mutation.ID == "" {
		if workspace.Generation == input.CommittedGeneration &&
			workspace.State == input.WorkspaceState {
			if err := tx.Commit(ctx); err != nil {
				return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace mutation completion replay commit failed: %w", err)
			}
			return workspace, nil
		}
		return ports.HomeWorkspace{}, ports.ErrWorkspaceMutation
	}
	if workspace.Mutation.ID != input.MutationID ||
		workspace.Mutation.Kind != input.Kind ||
		workspace.Mutation.EffectID != input.EffectID ||
		workspace.Mutation.OperationID != input.OperationID {
		return ports.HomeWorkspace{}, ports.ErrWorkspaceMutation
	}
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.workspaces
		SET state=$2,generation=$3,local_receipt_json=$4,
		    mutation_kind='',mutation_id='',mutation_effect_id='',
		    mutation_operation_id='',mutation_expected_generation=NULL,
		    mutation_target_generation=NULL,mutation_state='',updated_at=$5
		WHERE id=$1`,
		input.WorkspaceID, input.WorkspaceState, input.CommittedGeneration,
		receiptJSON, input.Now.UTC(),
	); err != nil {
		return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace mutation completion failed: %w", err)
	}
	workspace.State = input.WorkspaceState
	workspace.Generation = input.CommittedGeneration
	workspace.LocalReceipt = cloneReceiptMap(input.LocalReceipt)
	workspace.Mutation = ports.WorkspaceMutation{}
	workspace.UpdatedAt = input.Now.UTC()
	if err := tx.Commit(ctx); err != nil {
		return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace mutation completion commit failed: %w", err)
	}
	return workspace, nil
}

type lockedSandboxWorkspace = rowlock.SandboxWorkspace

// lockSandboxWorkspace establishes the invariant quota-ledger, Sandbox, then
// Workspace acquisition order used by every PostgreSQL mutation path.
func lockSandboxWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (lockedSandboxWorkspace, error) {
	locked, err := rowlock.SandboxWorkspaceForSubject(
		ctx, tx, tenantRef, subjectRef, sandboxID,
	)
	if err != nil {
		return lockedSandboxWorkspace{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	return locked, nil
}

// lockSnapshotAfterWorkspace acquires a Snapshot only after its Sandbox and
// Workspace are already locked by lockSandboxWorkspace.
func lockSnapshotAfterWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	locked lockedSandboxWorkspace,
	snapshotID string,
) (contracts.Snapshot, string, error) {
	identity, err := rowlock.SnapshotForSubject(
		ctx, tx, tenantRef, subjectRef, locked, snapshotID,
	)
	if err != nil {
		return contracts.Snapshot{}, "", mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	snapshot, err := scanSnapshot(tx.QueryRow(
		ctx, snapshotSelect+` WHERE id=$1`, snapshotID,
	))
	if err != nil {
		return contracts.Snapshot{}, "", mapNotFound(err, ports.ErrSnapshotNotFound)
	}
	return snapshot, identity.HomeRunnerID, nil
}

func lockLocalWorkspaceRows(
	ctx context.Context,
	tx pgx.Tx,
	input ports.WorkspaceMutationInput,
) (ports.HomeWorkspace, error) {
	locked, err := lockSandboxWorkspace(
		ctx, tx, input.TenantRef, input.SubjectRef, input.SandboxID,
	)
	if err != nil {
		return ports.HomeWorkspace{}, err
	}
	if locked.WorkspaceID != input.WorkspaceID {
		return ports.HomeWorkspace{}, ports.ErrSandboxNotFound
	}
	if input.SnapshotID != "" {
		snapshot, _, err := lockSnapshotAfterWorkspace(
			ctx, tx, input.TenantRef, input.SubjectRef, locked, input.SnapshotID,
		)
		if err != nil {
			return ports.HomeWorkspace{}, err
		}
		if snapshot.State != "ready" {
			return ports.HomeWorkspace{}, ports.ErrSnapshotUnavailable
		}
	}
	return locked.Workspace, nil
}

func validateWorkspaceMutationInput(input ports.WorkspaceMutationInput) error {
	if input.TenantRef == "" || input.SubjectRef == "" || input.SandboxID == "" ||
		input.WorkspaceID == "" || input.HomeRunnerID == "" || input.Kind == "" ||
		input.MutationID == "" || input.EffectID == "" || input.ExpectedGeneration < 1 ||
		input.TargetGeneration < 0 || input.Now.IsZero() {
		return errors.New("SecondBox Workspace mutation requires complete logical identity and generation")
	}
	return nil
}

func nullableTargetGeneration(generation int64) any {
	if generation == 0 {
		return nil
	}
	return generation
}

func cloneReceiptMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
