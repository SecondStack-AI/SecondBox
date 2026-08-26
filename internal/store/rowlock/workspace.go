// Package rowlock owns the invariant PostgreSQL lock order for durable resource
// mutations. Every caller locks quota ledgers before Sandbox, then Workspace,
// then Snapshot when present, before acquiring operation-specific rows.
package rowlock

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/jackc/pgx/v5"
)

// SandboxWorkspace is the durable authority protected by the first two locks.
type SandboxWorkspace struct {
	SandboxID         string
	TenantRef         string
	SubjectRef        string
	WorkspaceID       string
	ProfileRevisionID string
	SandboxState      string
	DesiredState      string
	Generation        int64
	Revision          int64
	CurrentInstanceID string
	ReconcileOwner    string
	Workspace         ports.HomeWorkspace
}

// Snapshot identifies the third row in the invariant lock order.
type Snapshot struct {
	ID           string
	SandboxID    string
	WorkspaceID  string
	HomeRunnerID string
	State        string
}

// SandboxWorkspaceForSubject locks one tenant-scoped Sandbox followed by its Workspace.
func SandboxWorkspaceForSubject(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	sandboxID string,
) (SandboxWorkspace, error) {
	if err := TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return SandboxWorkspace{}, err
	}
	var locked SandboxWorkspace
	locked.SandboxID = sandboxID
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref,subject_ref,workspace_id,profile_revision_id,state,desired_state,generation,revision,
		       current_instance_id,COALESCE(reconcile_owner,'')
		FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3 AND state<>'deleted'
		FOR UPDATE`,
		sandboxID, tenantRef, subjectRef,
	).Scan(
		&locked.TenantRef, &locked.SubjectRef,
		&locked.WorkspaceID, &locked.ProfileRevisionID, &locked.SandboxState,
		&locked.DesiredState, &locked.Generation, &locked.Revision,
		&locked.CurrentInstanceID, &locked.ReconcileOwner,
	); err != nil {
		return SandboxWorkspace{}, err
	}
	workspace, err := lockWorkspace(ctx, tx, locked.WorkspaceID, sandboxID)
	if err != nil {
		return SandboxWorkspace{}, err
	}
	locked.Workspace = workspace
	return locked, nil
}

// SandboxWorkspaceByID locks one internally identified Sandbox followed by its Workspace.
func SandboxWorkspaceByID(
	ctx context.Context,
	tx pgx.Tx,
	sandboxID string,
) (SandboxWorkspace, error) {
	var tenantRef, subjectRef string
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref,subject_ref FROM secondbox.sandboxes WHERE id=$1`, sandboxID,
	).Scan(&tenantRef, &subjectRef); err != nil {
		return SandboxWorkspace{}, err
	}
	if err := TenantAndSubjectQuota(ctx, tx, tenantRef, subjectRef); err != nil {
		return SandboxWorkspace{}, err
	}
	var locked SandboxWorkspace
	locked.SandboxID = sandboxID
	if err := tx.QueryRow(ctx, `
		SELECT tenant_ref,subject_ref,workspace_id,profile_revision_id,state,desired_state,generation,revision,
		       current_instance_id,COALESCE(reconcile_owner,'')
		FROM secondbox.sandboxes
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		FOR UPDATE`,
		sandboxID, tenantRef, subjectRef,
	).Scan(
		&locked.TenantRef, &locked.SubjectRef,
		&locked.WorkspaceID, &locked.ProfileRevisionID, &locked.SandboxState,
		&locked.DesiredState, &locked.Generation, &locked.Revision,
		&locked.CurrentInstanceID, &locked.ReconcileOwner,
	); err != nil {
		return SandboxWorkspace{}, err
	}
	workspace, err := lockWorkspace(ctx, tx, locked.WorkspaceID, sandboxID)
	if err != nil {
		return SandboxWorkspace{}, err
	}
	locked.Workspace = workspace
	return locked, nil
}

// SnapshotForSubject locks a Snapshot only after the caller holds its Sandbox
// and Workspace through SandboxWorkspaceForSubject or SandboxWorkspaceByID.
func SnapshotForSubject(
	ctx context.Context,
	tx pgx.Tx,
	tenantRef string,
	subjectRef string,
	locked SandboxWorkspace,
	snapshotID string,
) (Snapshot, error) {
	var snapshot Snapshot
	if err := tx.QueryRow(ctx, `
		SELECT id,sandbox_id,workspace_id,home_runner_id,state
		FROM secondbox.snapshots
		WHERE id=$1 AND tenant_ref=$2 AND subject_ref=$3
		FOR UPDATE`,
		snapshotID, tenantRef, subjectRef,
	).Scan(
		&snapshot.ID, &snapshot.SandboxID, &snapshot.WorkspaceID,
		&snapshot.HomeRunnerID, &snapshot.State,
	); err != nil {
		return Snapshot{}, err
	}
	if snapshot.SandboxID != locked.SandboxID ||
		snapshot.WorkspaceID != locked.WorkspaceID ||
		snapshot.HomeRunnerID != locked.Workspace.HomeRunnerID {
		return Snapshot{}, pgx.ErrNoRows
	}
	return snapshot, nil
}

// SnapshotByID locks a Snapshot only after the caller holds its Sandbox and Workspace.
func SnapshotByID(
	ctx context.Context,
	tx pgx.Tx,
	locked SandboxWorkspace,
	snapshotID string,
) (Snapshot, error) {
	var snapshot Snapshot
	if err := tx.QueryRow(ctx, `
		SELECT id,sandbox_id,workspace_id,home_runner_id,state
		FROM secondbox.snapshots
		WHERE id=$1
		FOR UPDATE`,
		snapshotID,
	).Scan(
		&snapshot.ID, &snapshot.SandboxID, &snapshot.WorkspaceID,
		&snapshot.HomeRunnerID, &snapshot.State,
	); err != nil {
		return Snapshot{}, err
	}
	if snapshot.SandboxID != locked.SandboxID ||
		snapshot.WorkspaceID != locked.WorkspaceID ||
		snapshot.HomeRunnerID != locked.Workspace.HomeRunnerID {
		return Snapshot{}, pgx.ErrNoRows
	}
	return snapshot, nil
}

func lockWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	sandboxID string,
) (ports.HomeWorkspace, error) {
	var workspace ports.HomeWorkspace
	var expectedGeneration, targetGeneration int64
	var receiptJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT id,sandbox_id,home_runner_id,state,logical_capacity_bytes,generation,
		       mutation_kind,mutation_id,mutation_effect_id,mutation_operation_id,
		       COALESCE(mutation_expected_generation,0),
		       COALESCE(mutation_target_generation,0),mutation_state,
		       local_receipt_json,created_at,updated_at
		FROM secondbox.workspaces
		WHERE id=$1 AND sandbox_id=$2
		FOR UPDATE`,
		workspaceID, sandboxID,
	).Scan(
		&workspace.ID, &workspace.SandboxID, &workspace.HomeRunnerID, &workspace.State,
		&workspace.LogicalCapacityBytes, &workspace.Generation,
		&workspace.Mutation.Kind, &workspace.Mutation.ID, &workspace.Mutation.EffectID,
		&workspace.Mutation.OperationID, &expectedGeneration, &targetGeneration,
		&workspace.Mutation.State, &receiptJSON, &workspace.CreatedAt, &workspace.UpdatedAt,
	); err != nil {
		return ports.HomeWorkspace{}, err
	}
	workspace.Mutation.ExpectedGeneration = expectedGeneration
	workspace.Mutation.TargetGeneration = targetGeneration
	if err := json.Unmarshal(receiptJSON, &workspace.LocalReceipt); err != nil {
		return ports.HomeWorkspace{}, fmt.Errorf("SecondBox Workspace local receipt decoding failed: %w", err)
	}
	return workspace, nil
}
