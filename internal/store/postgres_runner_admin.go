package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

const runnerPoolSelect = `
	SELECT name,state,architectures_json,capabilities_json,capacity_policy_json,
	       ready_runner_count,revision,created_at,updated_at
	FROM secondbox.runner_pools`

const runnerAdminSelect = `
	SELECT runner.id,runner.pool_name,runner.name,runner.state,
	       COALESCE((
	         SELECT credential.state
	         FROM secondbox.runner_credentials AS credential
	         WHERE credential.runner_id=runner.id
	         ORDER BY
	           CASE credential.state
	             WHEN 'active' THEN 0
	             WHEN 'retiring' THEN 1
	             WHEN 'revoked' THEN 2
	             ELSE 3
	           END,
	           credential.created_at DESC,
	           credential.serial_number
	         LIMIT 1
	       ),'absent') AS credential_state,
	       runner.architectures_json,runner.capabilities_json,runner.capacity_json,
	       runner.protocol_versions_json,runner.last_seen_at,runner.revision,
	       runner.created_at,runner.updated_at
	FROM secondbox.runners AS runner`

// CreateRunnerPool persists one new operator-owned placement boundary and audit event.
func (store *PostgresControlPlaneStore) CreateRunnerPool(
	ctx context.Context,
	pool contracts.RunnerPool,
	audit contracts.AuditEvent,
) (contracts.RunnerPool, error) {
	architecturesJSON, capabilitiesJSON, capacityPolicyJSON, err := encodeRunnerPoolPolicy(pool)
	if err != nil {
		return contracts.RunnerPool{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool create transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		"secondbox-runner-pool-create\x1f"+pool.Name,
	); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool create lock failed: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM secondbox.runner_pools WHERE name=$1)`,
		pool.Name,
	).Scan(&exists); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool existence lookup failed: %w", err)
	}
	if exists {
		return contracts.RunnerPool{}, ports.ErrRunnerPoolExists
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO secondbox.runner_pools (
			name,state,architectures_json,capabilities_json,capacity_policy_json,
			ready_runner_count,revision,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		pool.Name, pool.State, architecturesJSON, capabilitiesJSON,
		capacityPolicyJSON, pool.ReadyRunnerCount, pool.Revision,
		pool.CreatedAt, pool.UpdatedAt,
	); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool insert failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.RunnerPool{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool create commit failed: %w", err)
	}
	return pool, nil
}

// UpdateRunnerPool changes explicit scheduling policy under optimistic concurrency.
func (store *PostgresControlPlaneStore) UpdateRunnerPool(
	ctx context.Context,
	name string,
	update contracts.UpdateRunnerPoolRequest,
	expectedRevision int64,
	now time.Time,
	audit contracts.AuditEvent,
) (contracts.RunnerPool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool update transaction failed: %w", err)
	}
	defer tx.Rollback(ctx)
	pool, err := scanRunnerPool(tx.QueryRow(ctx, runnerPoolSelect+` WHERE name=$1 FOR UPDATE`, name))
	if err != nil {
		return contracts.RunnerPool{}, mapNotFound(err, ports.ErrRunnerPoolNotFound)
	}
	if expectedRevision != pool.Revision {
		return contracts.RunnerPool{}, ports.ErrRevisionConflict
	}
	if update.State != nil {
		pool.State = *update.State
	}
	if update.Architectures != nil {
		pool.Architectures = append([]string(nil), (*update.Architectures)...)
	}
	if update.Capabilities != nil {
		pool.Capabilities = append([]string(nil), (*update.Capabilities)...)
	}
	if update.CapacityPolicy != nil {
		pool.CapacityPolicy = cloneRunnerCapacityPolicy(*update.CapacityPolicy)
	}
	architecturesJSON, capabilitiesJSON, capacityPolicyJSON, err := encodeRunnerPoolPolicy(pool)
	if err != nil {
		return contracts.RunnerPool{}, err
	}
	pool.Revision++
	pool.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE secondbox.runner_pools
		SET state=$2,architectures_json=$3,capabilities_json=$4,
		    capacity_policy_json=$5,revision=$6,updated_at=$7
		WHERE name=$1`,
		pool.Name, pool.State, architecturesJSON, capabilitiesJSON,
		capacityPolicyJSON, pool.Revision, pool.UpdatedAt,
	); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool update failed: %w", err)
	}
	if err := insertAuditEvent(ctx, tx, audit); err != nil {
		return contracts.RunnerPool{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool update commit failed: %w", err)
	}
	return pool, nil
}

// GetRunnerPool returns one administrative placement boundary.
func (store *PostgresControlPlaneStore) GetRunnerPool(
	ctx context.Context,
	name string,
) (contracts.RunnerPool, error) {
	pool, err := scanRunnerPool(store.pool.QueryRow(ctx, runnerPoolSelect+` WHERE name=$1`, name))
	return pool, mapNotFound(err, ports.ErrRunnerPoolNotFound)
}

// ListRunnerPools returns a bounded stable administrative page.
func (store *PostgresControlPlaneStore) ListRunnerPools(
	ctx context.Context,
	limit int,
	cursor string,
) (contracts.RunnerPoolPage, error) {
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		runnerPoolListCursorResource,
		"",
		cursor,
		`SELECT created_at FROM secondbox.runner_pools WHERE name=$1`,
	)
	if err != nil {
		return contracts.RunnerPoolPage{}, err
	}
	rows, err := store.pool.Query(ctx, runnerPoolSelect+`
		WHERE NOT $1 OR (created_at,name) > ($2,$3)
		ORDER BY created_at,name
		LIMIT $4`,
		boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1)
	if err != nil {
		return contracts.RunnerPoolPage{}, fmt.Errorf("SecondBox RunnerPool list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.RunnerPoolPage{Items: make([]contracts.RunnerPool, 0)}
	for rows.Next() {
		pool, scanErr := scanRunnerPool(rows)
		if scanErr != nil {
			return contracts.RunnerPoolPage{}, scanErr
		}
		page.Items = append(page.Items, pool)
	}
	if err := rows.Err(); err != nil {
		return contracts.RunnerPoolPage{}, fmt.Errorf("SecondBox RunnerPool list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			runnerPoolListCursorResource, "", page.Items[limit-1].Name,
		)
		if err != nil {
			return contracts.RunnerPoolPage{}, err
		}
	}
	return page, nil
}

// GetRunner returns one administrative runner projection without credential material.
func (store *PostgresControlPlaneStore) GetRunner(
	ctx context.Context,
	runnerID string,
) (contracts.Runner, error) {
	runner, err := scanRunnerAdmin(store.pool.QueryRow(ctx, runnerAdminSelect+` WHERE runner.id=$1`, runnerID))
	return runner, mapNotFound(err, ports.ErrRunnerNotFound)
}

// ListRunners returns stable runner projections optionally filtered by one exact pool.
func (store *PostgresControlPlaneStore) ListRunners(
	ctx context.Context,
	poolName string,
	limit int,
	cursor string,
) (contracts.RunnerPage, error) {
	scope := "pool=" + poolName
	boundary, err := store.resolvePostgresListCursor(
		ctx,
		runnerListCursorResource,
		scope,
		cursor,
		`SELECT created_at FROM secondbox.runners
		 WHERE ($1='' OR pool_name=$1) AND id=$2`,
		poolName,
	)
	if err != nil {
		return contracts.RunnerPage{}, err
	}
	query := runnerAdminSelect + `
		WHERE ($1='' OR runner.pool_name=$1)
		  AND (NOT $2 OR (runner.created_at,runner.id) > ($3,$4))
		ORDER BY runner.created_at,runner.id
		LIMIT $5`
	rows, err := store.pool.Query(
		ctx, query, poolName, boundary.Active, boundary.CreatedAt, boundary.ItemKey, limit+1,
	)
	if err != nil {
		return contracts.RunnerPage{}, fmt.Errorf("SecondBox Runner list failed: %w", err)
	}
	defer rows.Close()
	page := contracts.RunnerPage{Items: make([]contracts.Runner, 0)}
	for rows.Next() {
		runner, scanErr := scanRunnerAdmin(rows)
		if scanErr != nil {
			return contracts.RunnerPage{}, scanErr
		}
		page.Items = append(page.Items, runner)
	}
	if err := rows.Err(); err != nil {
		return contracts.RunnerPage{}, fmt.Errorf("SecondBox Runner list rows failed: %w", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor, err = encodePostgresListNextCursor(
			runnerListCursorResource, scope, page.Items[limit-1].ID,
		)
		if err != nil {
			return contracts.RunnerPage{}, err
		}
	}
	return page, nil
}

func encodeRunnerPoolPolicy(pool contracts.RunnerPool) ([]byte, []byte, []byte, error) {
	architecturesJSON, err := json.Marshal(pool.Architectures)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("SecondBox RunnerPool architectures encoding failed: %w", err)
	}
	capabilitiesJSON, err := json.Marshal(pool.Capabilities)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("SecondBox RunnerPool capabilities encoding failed: %w", err)
	}
	capacityPolicyJSON, err := json.Marshal(pool.CapacityPolicy)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("SecondBox RunnerPool capacity policy encoding failed: %w", err)
	}
	return architecturesJSON, capabilitiesJSON, capacityPolicyJSON, nil
}

type runnerPoolRow interface {
	Scan(...any) error
}

func scanRunnerPool(row runnerPoolRow) (contracts.RunnerPool, error) {
	var pool contracts.RunnerPool
	var architecturesJSON, capabilitiesJSON, capacityPolicyJSON []byte
	if err := row.Scan(
		&pool.Name, &pool.State, &architecturesJSON, &capabilitiesJSON,
		&capacityPolicyJSON, &pool.ReadyRunnerCount, &pool.Revision,
		&pool.CreatedAt, &pool.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.RunnerPool{}, err
		}
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool scan failed: %w", err)
	}
	if err := json.Unmarshal(architecturesJSON, &pool.Architectures); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool architectures decoding failed: %w", err)
	}
	if err := json.Unmarshal(capabilitiesJSON, &pool.Capabilities); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool capabilities decoding failed: %w", err)
	}
	if err := json.Unmarshal(capacityPolicyJSON, &pool.CapacityPolicy); err != nil {
		return contracts.RunnerPool{}, fmt.Errorf("SecondBox RunnerPool capacity policy decoding failed: %w", err)
	}
	return pool, nil
}

func scanRunnerAdmin(row runnerPoolRow) (contracts.Runner, error) {
	var runner contracts.Runner
	var architecturesJSON, capabilitiesJSON, capacityJSON, protocolVersionsJSON []byte
	if err := row.Scan(
		&runner.ID, &runner.PoolName, &runner.Name, &runner.State,
		&runner.CredentialState, &architecturesJSON, &capabilitiesJSON,
		&capacityJSON, &protocolVersionsJSON, &runner.LastSeenAt,
		&runner.Revision, &runner.CreatedAt, &runner.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Runner{}, err
		}
		return contracts.Runner{}, fmt.Errorf("SecondBox Runner scan failed: %w", err)
	}
	if err := json.Unmarshal(architecturesJSON, &runner.Architectures); err != nil {
		return contracts.Runner{}, fmt.Errorf("SecondBox Runner architectures decoding failed: %w", err)
	}
	if err := json.Unmarshal(capabilitiesJSON, &runner.Capabilities); err != nil {
		return contracts.Runner{}, fmt.Errorf("SecondBox Runner capabilities decoding failed: %w", err)
	}
	if err := json.Unmarshal(capacityJSON, &runner.Capacity); err != nil {
		return contracts.Runner{}, fmt.Errorf("SecondBox Runner capacity decoding failed: %w", err)
	}
	if err := json.Unmarshal(protocolVersionsJSON, &runner.ProtocolVersions); err != nil {
		return contracts.Runner{}, fmt.Errorf("SecondBox Runner protocol versions decoding failed: %w", err)
	}
	return runner, nil
}

func cloneRunnerCapacityPolicy(source map[string]int64) map[string]int64 {
	cloned := make(map[string]int64, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}
