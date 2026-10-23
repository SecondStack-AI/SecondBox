package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

// GetPoolCapacity aggregates the RunnerPools the scope may observe. Runner
// identities stay inside the query; only per-pool sums and counts leave it.
func (store *PostgresControlPlaneStore) GetPoolCapacity(
	ctx context.Context,
	scope ports.PoolCapacityScope,
	observedAt time.Time,
) (contracts.PoolCapacityObservation, error) {
	if !scope.AllPools && scope.TenantRef == "" {
		return contracts.PoolCapacityObservation{}, errors.New("SecondBox pool capacity scope requires a Tenant")
	}
	profileNames := scope.ProfileNames
	if profileNames == nil {
		profileNames = []string{}
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return contracts.PoolCapacityObservation{}, fmt.Errorf("SecondBox pool capacity transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	// A placeable Runner is one placement may choose: the same state, drain
	// phase, and live connection runnerPlacementCompatible requires. Only a
	// connected Runner's storage report is current; a disconnected Runner's
	// last report may describe a host that no longer exists.
	//
	// A pending start is a Sandbox wanted running with no Instance yet whose
	// ready Workspace is free or holds a queued start: an admitted start, the
	// start a running-initial create hands over, or an automatic restart. A
	// failed Sandbox parked on a failure class waits for an operator, not for
	// capacity, and is not counted.
	rows, err := tx.Query(ctx, `
		WITH visible_pools AS (
			SELECT revision.spec_json->>'pool' AS pool_name
			FROM secondbox.profiles AS profile
			JOIN secondbox.profile_revisions AS revision ON revision.id=profile.current_revision_id
			WHERE profile.state='enabled'
			  AND (profile.name=ANY($2::text[])
			    OR ($5 AND EXISTS (
			      SELECT 1 FROM secondbox.tenants AS tenant
			      WHERE tenant.ref=$3 AND tenant.allowed_profile_grants_json ? profile.name)))
			UNION
			SELECT revision.spec_json->>'pool'
			FROM secondbox.sandboxes AS sandbox
			JOIN secondbox.profile_revisions AS revision ON revision.id=sandbox.profile_revision_id
			WHERE sandbox.tenant_ref=$3 AND ($4='' OR sandbox.subject_ref=$4)
			  AND sandbox.state<>'deleted'
		)
		SELECT pool.name,pool.state,
		       COALESCE(placeable.runner_count,0),
		       COALESCE(placeable.instances,0),COALESCE(placeable.vcpu_count,0),
		       COALESCE(placeable.memory_bytes,0),COALESCE(placeable.operations,0),
		       COALESCE(placeable.reserved_instances,0),COALESCE(placeable.reserved_vcpu_count,0),
		       COALESCE(placeable.reserved_memory_bytes,0),COALESCE(placeable.reserved_operations,0),
		       (SELECT count(*) FROM secondbox.runners AS runner
		        WHERE runner.pool_name=pool.name AND runner.active_connection_id<>''
		          AND runner.storage_pressure_json->>'status'='admission_denied'),
		       (SELECT count(*)
		        FROM secondbox.sandboxes AS sandbox
		        JOIN secondbox.workspaces AS workspace ON workspace.id=sandbox.workspace_id
		        JOIN secondbox.runners AS home ON home.id=workspace.home_runner_id
		        WHERE home.pool_name=pool.name
		          AND sandbox.desired_state='running' AND sandbox.current_instance_id=''
		          AND (sandbox.state='stopped'
		            OR (sandbox.state='failed' AND sandbox.lifecycle_failure_class=''))
		          AND workspace.state='ready'
		          AND (workspace.mutation_state=''
		            OR (workspace.mutation_kind='start' AND workspace.mutation_state='queued')))
		FROM secondbox.runner_pools AS pool
		LEFT JOIN LATERAL (
			SELECT count(*) AS runner_count,
			       sum((runner.capacity_json->>'Instances')::bigint) AS instances,
			       sum((runner.capacity_json->>'VCPUCount')::bigint) AS vcpu_count,
			       sum((runner.capacity_json->>'MemoryBytes')::bigint) AS memory_bytes,
			       sum((runner.capacity_json->>'Operations')::bigint) AS operations,
			       sum((runner.reserved_capacity_json->>'Instances')::bigint) AS reserved_instances,
			       sum((runner.reserved_capacity_json->>'VCPUCount')::bigint) AS reserved_vcpu_count,
			       sum((runner.reserved_capacity_json->>'MemoryBytes')::bigint) AS reserved_memory_bytes,
			       sum((runner.reserved_capacity_json->>'Operations')::bigint) AS reserved_operations
			FROM secondbox.runners AS runner
			WHERE runner.pool_name=pool.name AND runner.state='ready'
			  AND runner.drain_phase='active' AND runner.active_connection_id<>''
		) AS placeable ON true
		WHERE $1 OR pool.name IN (SELECT pool_name FROM visible_pools)
		ORDER BY pool.name`,
		scope.AllPools, profileNames, scope.TenantRef, scope.SubjectRef, scope.TenantCeiling,
	)
	if err != nil {
		return contracts.PoolCapacityObservation{}, fmt.Errorf("SecondBox pool capacity lookup failed: %w", err)
	}
	observation := contracts.PoolCapacityObservation{Pools: []contracts.RunnerPoolCapacity{}, ObservedAt: observedAt.UTC()}
	for rows.Next() {
		var pool contracts.RunnerPoolCapacity
		if err := rows.Scan(
			&pool.Name, &pool.State, &pool.PlaceableRunnerCount,
			&pool.Allocatable.Instances, &pool.Allocatable.VCPUCount,
			&pool.Allocatable.MemoryBytes, &pool.Allocatable.Operations,
			&pool.Reserved.Instances, &pool.Reserved.VCPUCount,
			&pool.Reserved.MemoryBytes, &pool.Reserved.Operations,
			&pool.StorageAdmissionDeniedRunnerCount, &pool.PendingStartCount,
		); err != nil {
			rows.Close()
			return contracts.PoolCapacityObservation{}, fmt.Errorf("SecondBox pool capacity scan failed: %w", err)
		}
		observation.Pools = append(observation.Pools, pool)
	}
	if err := rows.Err(); err != nil {
		return contracts.PoolCapacityObservation{}, fmt.Errorf("SecondBox pool capacity rows failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.PoolCapacityObservation{}, fmt.Errorf("SecondBox pool capacity commit failed: %w", err)
	}
	return observation, nil
}
