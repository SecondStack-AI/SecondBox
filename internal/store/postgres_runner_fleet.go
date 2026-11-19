package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

const fleetRunnerQuery = `SELECT runner.id,runner.revision,runner.state,runner.fleet_state,
 runner.active_connection_id<>'',runner.last_seen_at,runner.capacity_json,runner.reserved_capacity_json,
 GREATEST((SELECT max(a.updated_at) FROM secondbox.assignments a WHERE a.runner_id=runner.id),
 (SELECT max(s.updated_at) FROM secondbox.sandboxes s JOIN secondbox.workspaces w ON w.sandbox_id=s.id WHERE w.home_runner_id=runner.id)),
 COALESCE(runner.storage_pressure_json->>'status'='admission_denied',false),
 (SELECT count(*) FROM secondbox.workspaces w WHERE w.home_runner_id=runner.id AND w.state<>'deleted'),
 (SELECT count(*) FROM secondbox.snapshots s WHERE s.home_runner_id=runner.id AND s.state<>'deleted'),
 (SELECT count(*) FROM secondbox.workspaces w WHERE w.home_runner_id=runner.id AND w.mutation_state<>''),
 (SELECT count(*) FROM secondbox.runner_commands c WHERE c.runner_id=runner.id AND c.state IN ('pending','delivering','delivered')),
 (SELECT count(*) FROM secondbox.assignments a WHERE a.runner_id=runner.id AND a.state NOT IN ('fenced','released','failed_terminal'))
 FROM secondbox.runners runner`

func (store *PostgresControlPlaneStore) ReadRunnerFleet(ctx context.Context, pool string, now time.Time) (contracts.FleetPoolObservation, error) {
	observation := contracts.FleetPoolObservation{PoolName: pool, ObservedAt: now.UTC(), Runners: []contracts.FleetRunner{}, Workspaces: []contracts.FleetWorkspace{}}
	if err := store.expireFencedRunnerCancellations(ctx, pool, now); err != nil {
		return observation, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return observation, fmt.Errorf("SecondBox Runner fleet observation transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.runner_pools WHERE name=$1)`, pool).Scan(&exists); err != nil {
		return observation, err
	}
	if !exists {
		return observation, ports.ErrRunnerPoolNotFound
	}
	if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(s.vcpu_count),0),COALESCE(sum(s.memory_bytes),0),
 COALESCE(sum(s.workspace_bytes),0),min(s.updated_at)
 FROM secondbox.sandboxes s JOIN secondbox.profile_revisions p ON p.id=s.profile_revision_id
 JOIN secondbox.workspaces w ON w.id=s.workspace_id
 WHERE p.spec_json->>'pool'=$1 AND s.current_instance_id=''
 AND (s.desired_state='running' OR (s.desired_state='stopped' AND s.state='creating'
   AND w.state='creating' AND w.home_runner_id='' AND w.mutation_kind='create' AND w.mutation_state='queued'))
 AND s.state IN ('creating','stopped','starting','failed') AND s.lifecycle_failure_class=''
 AND w.state IN ('creating','ready','archived')
 AND EXISTS(SELECT 1 FROM secondbox.operations o WHERE o.sandbox_id=s.id
   AND o.kind IN ('create','start') AND o.state IN ('pending','running')
   AND (o.capacity_wait_deadline IS NULL OR o.capacity_wait_deadline>$2))`, pool, now).Scan(
		&observation.Demand.Instances, &observation.Demand.VCPUCount, &observation.Demand.MemoryBytes,
		&observation.Demand.WorkspaceBytes, &observation.Demand.OldestWaitAt); err != nil {
		return observation, fmt.Errorf("SecondBox Runner fleet demand: %w", err)
	}
	rows, err := tx.Query(ctx, fleetRunnerQuery+` WHERE runner.pool_name=$1 ORDER BY runner.id LIMIT 1001`, pool)
	if err != nil {
		return observation, err
	}
	for rows.Next() {
		runner, err := scanFleetRunner(rows)
		if err != nil {
			rows.Close()
			return observation, err
		}
		if len(observation.Runners) == 1000 {
			observation.Truncated = true
			break
		}
		observation.Runners = append(observation.Runners, runner)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observation, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT s.id,s.tenant_ref,s.subject_ref,w.home_runner_id,s.revision,s.state,s.desired_state,w.mutation_state
 FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.id=w.sandbox_id
 JOIN secondbox.runners r ON r.id=w.home_runner_id
 WHERE r.pool_name=$1 AND w.state<>'deleted' AND s.state<>'deleted' ORDER BY r.id,s.id LIMIT 1001`, pool)
	if err != nil {
		return observation, err
	}
	for rows.Next() {
		var workspace contracts.FleetWorkspace
		if err := rows.Scan(&workspace.SandboxID, &workspace.TenantRef, &workspace.SubjectRef, &workspace.RunnerID,
			&workspace.Revision, &workspace.State, &workspace.DesiredState, &workspace.Mutation); err != nil {
			rows.Close()
			return observation, err
		}
		if len(observation.Workspaces) == 1000 {
			observation.Truncated = true
			break
		}
		observation.Workspaces = append(observation.Workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return observation, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return observation, err
	}
	return observation, nil
}

func (store *PostgresControlPlaneStore) expireFencedRunnerCancellations(ctx context.Context, pool string, now time.Time) error {
	// A session marked failed by the control plane is not Runner teardown proof.
	// Only an exact, durably terminal Assignment makes its old cancels obsolete.
	_, err := store.pool.Exec(ctx, `UPDATE secondbox.runner_commands command SET state='expired',target_connection_id='',updated_at=$2
 FROM secondbox.assignments assignment,secondbox.runners runner
 WHERE command.kind='data-plane-cancel' AND command.state IN ('pending','delivering','delivered')
 AND assignment.id=command.assignment_id AND assignment.runner_id=command.runner_id
 AND assignment.state IN ('fenced','released','failed_terminal')
 AND runner.id=command.runner_id AND runner.pool_name=$1`, pool, now)
	if err != nil {
		return fmt.Errorf("SecondBox fenced Runner cancellation expiry: %w", err)
	}
	return nil
}

func scanFleetRunner(row pgx.Row) (contracts.FleetRunner, error) {
	var runner contracts.FleetRunner
	var allocatedJSON, reservedJSON []byte
	if err := row.Scan(&runner.ID, &runner.Revision, &runner.State, &runner.FleetState,
		&runner.Connected, &runner.LastSeenAt, &allocatedJSON, &reservedJSON,
		&runner.LastActivityAt, &runner.StorageAdmissionDenied,
		&runner.WorkspaceCount, &runner.SnapshotCount, &runner.PendingMutations, &runner.PendingCommands, &runner.ActiveAssignments); err != nil {
		return runner, err
	}
	var allocated, reserved runnerCapacity
	if err := json.Unmarshal(allocatedJSON, &allocated); err != nil {
		return runner, err
	}
	if err := json.Unmarshal(reservedJSON, &reserved); err != nil {
		return runner, err
	}
	runner.Allocatable = contracts.PoolComputeCapacity{Instances: allocated.Instances, VCPUCount: allocated.VCPUCount, MemoryBytes: allocated.MemoryBytes, Operations: allocated.Operations}
	runner.Reserved = contracts.PoolComputeCapacity{Instances: reserved.Instances, VCPUCount: reserved.VCPUCount, MemoryBytes: reserved.MemoryBytes, Operations: reserved.Operations}
	runner.WorkspaceCapacityBytes = allocated.DiskBytes
	runner.ReservedWorkspaceBytes = reserved.DiskBytes
	if runner.FleetState == "evacuating" && fleetRunnerEmpty(runner) {
		runner.FleetState = "evacuated"
	}
	return runner, nil
}

func fleetRunnerEmpty(runner contracts.FleetRunner) bool {
	return runner.WorkspaceCount == 0 && runner.SnapshotCount == 0 && runner.PendingMutations == 0 &&
		runner.PendingCommands == 0 && runner.ActiveAssignments == 0
}

func (store *PostgresControlPlaneStore) ChangeRunnerFleetState(ctx context.Context, pool, id, action string, revision int64, now time.Time) (contracts.FleetRunner, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.FleetRunner{}, err
	}
	defer tx.Rollback(ctx)
	var actualRevision int64
	var fleetState string
	if err := tx.QueryRow(ctx, `SELECT revision,fleet_state FROM secondbox.runners WHERE id=$1 AND pool_name=$2 FOR UPDATE`, id, pool).Scan(&actualRevision, &fleetState); err != nil {
		return contracts.FleetRunner{}, mapNotFound(err, ports.ErrRunnerNotFound)
	}
	if action == "retire" && fleetState == "retired" {
		return scanFleetRunner(tx.QueryRow(ctx, fleetRunnerQuery+` WHERE runner.id=$1`, id))
	}
	if actualRevision != revision {
		return contracts.FleetRunner{}, ports.ErrRevisionConflict
	}
	runner, err := scanFleetRunner(tx.QueryRow(ctx, fleetRunnerQuery+` WHERE runner.id=$1`, id))
	if err != nil {
		return runner, err
	}
	if fleetState == "retired" {
		return runner, ports.ErrInvalidLifecycleTransition
	}
	if action == "evacuate" {
		if runner.ActiveAssignments != 0 {
			return runner, fmt.Errorf("%w: Runner still has active Assignments", ports.ErrInvalidLifecycleTransition)
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.runners SET fleet_state='evacuating',state='draining',drain_phase='draining',revision=revision+1,updated_at=$2 WHERE id=$1`, id, now); err != nil {
			return runner, err
		}
		commandID := "fleet-drain-" + id
		payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_Drain{Drain: &runnerv1.DrainCommand{
			MessageId: commandID, Mode: runnerv1.DrainMode_DRAIN_MODE_GRACEFUL,
		}}})
		if err != nil {
			return runner, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at,delivered_at)
 VALUES($1,$2,$1,'drain',$3,'pending','',0,$4,$4,NULL) ON CONFLICT(id) DO NOTHING`, commandID, id, payload, now); err != nil {
			return runner, err
		}
	} else if action == "retire" {
		if fleetState != "evacuating" || !fleetRunnerEmpty(runner) {
			return runner, fmt.Errorf("%w: Runner has not evacuated all Workspace, Snapshot and command authority", ports.ErrInvalidLifecycleTransition)
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.runners SET fleet_state='retired',state='offline',retired_at=$2,active_connection_id='',revision=revision+1,updated_at=$2 WHERE id=$1`, id, now); err != nil {
			return runner, err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.runner_connections SET state='closed',disconnected_at=$2,last_seen_at=$2 WHERE runner_id=$1 AND state='active'`, id, now); err != nil {
			return runner, err
		}
	} else {
		return runner, errors.New("SecondBox Runner fleet action is invalid")
	}
	runner, err = scanFleetRunner(tx.QueryRow(ctx, fleetRunnerQuery+` WHERE runner.id=$1`, id))
	if err != nil {
		return runner, err
	}
	if err := tx.Commit(ctx); err != nil {
		return runner, err
	}
	return runner, nil
}
