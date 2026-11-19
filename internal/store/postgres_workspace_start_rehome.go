package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/imagepreparation"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func (store *PostgresControlPlaneStore) prepareResidentWorkspaceStarts(ctx context.Context, now time.Time, profileIdleSeconds map[string]int64) error {
	profiles := make([]string, 0, len(profileIdleSeconds))
	for profile := range profileIdleSeconds {
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		return nil
	}
	// Automatic re-homing shares the explicit Profile opt-in of cold tiering.
	rows, err := store.pool.Query(ctx, `SELECT s.id FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.id=s.workspace_id JOIN secondbox.operations o ON o.id=w.mutation_operation_id WHERE s.profile_name=ANY($1) AND s.state='stopped' AND s.desired_state='running' AND s.current_instance_id='' AND w.state='ready' AND w.home_runner_id<>'' AND w.mutation_kind='start' AND w.mutation_state='queued' AND o.kind IN ('start','create') AND o.state IN ('pending','running') ORDER BY s.id LIMIT 32`, profiles)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := store.prepareResidentWorkspaceStart(ctx, id, now); err != nil {
			return fmt.Errorf("SecondBox resident start offload %s: %w", id, err)
		}
	}
	return nil
}

func (store *PostgresControlPlaneStore) prepareResidentWorkspaceStart(ctx context.Context, sandboxID string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return err
	}
	w := locked.Workspace
	if locked.SandboxState != "stopped" || locked.DesiredState != "running" || locked.CurrentInstanceID != "" || w.State != "ready" || w.Mutation.Kind != "start" || w.Mutation.State != "queued" {
		return nil
	}
	var operationKind, operationState string
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `SELECT kind,state,capacity_wait_deadline FROM secondbox.operations WHERE id=$1`, w.Mutation.OperationID).Scan(&operationKind, &operationState, &deadline); err != nil {
		return err
	}
	if (operationKind != "start" && operationKind != "create") || (operationState != "pending" && operationState != "running") || deadline != nil && !deadline.After(now) {
		return nil
	}
	var prohibited bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.snapshots WHERE workspace_id=$1 AND state<>'deleted') OR EXISTS(SELECT 1 FROM secondbox.assignments WHERE sandbox_id=$2 AND state NOT IN ('fenced','released','failed_terminal'))`, w.ID, sandboxID).Scan(&prohibited); err != nil {
		return err
	}
	if prohibited {
		return nil
	}
	var specJSON []byte
	if err := tx.QueryRow(ctx, `SELECT spec_json FROM secondbox.profile_revisions WHERE id=$1`, locked.ProfileRevisionID).Scan(&specJSON); err != nil {
		return err
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return err
	}
	spec = sandboxPlacementSpec(spec, locked.Resources)
	source, err := scanRunnerPlacementCandidate(tx.QueryRow(ctx, `SELECT id,pool_name,state,drain_phase,active_connection_id,backend_kind,architectures_json,capabilities_json,protocol_versions_json,capacity_json,reserved_capacity_json,artifact_cache_json,supported_egress_contexts_json FROM secondbox.runners WHERE id=$1 FOR UPDATE`, w.HomeRunnerID), "SecondBox resident start home", false)
	if err != nil {
		return err
	}
	if (source.state != "ready" && source.state != "draining") || source.activeConnectionID == "" {
		return nil
	}
	assigned, pending, err := runnerHomeComputeReservations(ctx, tx, source.id)
	if err != nil {
		return err
	}
	// This intent already holds its home reservation. Test its capacity once.
	pending.VCPUCount = max(0, pending.VCPUCount-locked.Resources.VCPUCount)
	pending.MemoryBytes = max(0, pending.MemoryBytes-locked.Resources.MemoryBytes)
	pending.Instances = max(0, pending.Instances-1)
	pending.Operations = max(0, pending.Operations-spec.Resources.ConcurrentOperations)
	reserved := maxRunnerPlacementCapacity(source.reportedReserved, assigned)
	reserved.VCPUCount += pending.VCPUCount
	reserved.MemoryBytes += pending.MemoryBytes
	reserved.Instances += pending.Instances
	reserved.Operations += pending.Operations
	if runnerPlacementCompatible(source, spec, runnerPlacementOptions{requiredEgressContext: locked.EgressContext}, reserved) {
		return nil
	}
	identifier, err := newWorkspaceArchiveID()
	if err != nil {
		return err
	}
	fence := make([]byte, 32)
	if _, err := rand.Read(fence); err != nil {
		return err
	}
	key := fmt.Sprintf("workspaces/%s/%d/%s.sbx.gz", w.ID, w.Generation, identifier)
	if _, err := imagepreparation.CancelSandboxPreparations(ctx, tx, sandboxID, now); err != nil {
		return err
	}
	adopted, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET mutation_kind='archive',mutation_id=$2,mutation_effect_id=$2,mutation_expected_generation=generation,mutation_target_generation=generation,updated_at=$3 WHERE id=$1 AND mutation_kind='start' AND mutation_state='queued'`, w.ID, identifier, now)
	if err != nil {
		return err
	}
	if adopted.RowsAffected() != 1 {
		return ports.ErrWorkspaceMutation
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'queued',$1,$9,$10,$10)`, identifier, w.Mutation.OperationID, sandboxID, w.ID, source.id, w.Generation, w.LogicalCapacityBytes, key, fence, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET next_reconcile_at=NULL,reconcile_owner='',reconcile_claim_expires_at=NULL,revision=revision+1,updated_at=$2 WHERE id=$1`, sandboxID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *PostgresControlPlaneStore) expireArchiveStartWaits(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT s.id FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.id=s.workspace_id JOIN secondbox.operations o ON o.id=w.mutation_operation_id WHERE s.current_instance_id='' AND s.desired_state='running' AND w.mutation_kind IN ('archive','archive_restore') AND o.kind IN ('start','create') AND o.state IN ('pending','running') AND o.capacity_wait_deadline<=$1 ORDER BY s.id LIMIT 32`, now)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := store.pool.Begin(ctx)
		if err != nil {
			return err
		}
		locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, id)
		if err == nil && locked.CurrentInstanceID == "" && locked.DesiredState == "running" && (locked.Workspace.Mutation.Kind == "archive" || locked.Workspace.Mutation.Kind == "archive_restore") {
			_, err = tx.Exec(ctx, `WITH expired AS(UPDATE secondbox.operations SET state='failed',error_code='capacity_wait_expired',error_message='workspace transfer capacity wait expired before compute admission',retryable=true,completed_at=$2,updated_at=$2 WHERE id=$1 AND state IN ('pending','running') AND capacity_wait_deadline<=$2 RETURNING sandbox_id) UPDATE secondbox.sandboxes SET desired_state='stopped',reconcile_owner='',reconcile_claim_expires_at=NULL,next_reconcile_at=NULL,revision=revision+1,updated_at=$2 WHERE id IN(SELECT sandbox_id FROM expired)`, locked.Workspace.Mutation.OperationID, now)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}
