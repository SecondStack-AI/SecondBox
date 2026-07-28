package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

// PostgresEnvironmentStore persists Sandbox Service authority in the sandbox schema.
type PostgresEnvironmentStore struct {
	pool *pgxpool.Pool
}

// NewPostgresEnvironmentStore connects to the required PostgreSQL authority.
func NewPostgresEnvironmentStore(ctx context.Context, databaseURL string) (*PostgresEnvironmentStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create Sandbox Service PostgreSQL pool: %w", err)
	}
	store := &PostgresEnvironmentStore{pool: pool}
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// Close releases all Sandbox Service PostgreSQL connections.
func (s *PostgresEnvironmentStore) Close() { s.pool.Close() }

func (s *PostgresEnvironmentStore) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping Sandbox Service PostgreSQL: %w", err)
	}
	return nil
}

func (s *PostgresEnvironmentStore) CountRetainedWorkspaces(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox.workspaces`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count retained Sandbox workspaces: %w", err)
	}
	return count, nil
}

func (s *PostgresEnvironmentStore) GetWorkspaceUsage(ctx context.Context, tenantRef, subjectRef string) (contracts.WorkspaceUsage, error) {
	usage := contracts.WorkspaceUsage{
		ContractVersion: contracts.ContractVersionV1,
		TenantRef:       tenantRef,
		SubjectRef:      subjectRef,
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(resource_class.disk_bytes), 0), COALESCE(sum(snapshot.size_bytes), 0)
		FROM sandbox.environments AS environment
		JOIN sandbox.resource_classes AS resource_class ON resource_class.id = environment.resource_class_id
		LEFT JOIN sandbox.snapshots AS snapshot ON snapshot.id = NULLIF(environment.snapshot_id, '')
		WHERE environment.tenant_ref = $1 AND environment.subject_ref = $2`,
		tenantRef, subjectRef,
	).Scan(&usage.EnvironmentCount, &usage.QuotaBytes, &usage.UsageBytes); err != nil {
		return contracts.WorkspaceUsage{}, fmt.Errorf("read subject workspace usage: %w", err)
	}
	return usage, nil
}

func postgresEnvironmentNaturalKey(environment contracts.Environment) (string, error) {
	encoded, err := json.Marshal([]string{
		environment.TenantRef,
		environment.SubjectRef,
		environment.EnvironmentKey,
	})
	if err != nil {
		return "", fmt.Errorf("encode Environment natural key for PostgreSQL lock: %w", err)
	}
	return string(encoded), nil
}

func (s *PostgresEnvironmentStore) ResolveEnvironment(ctx context.Context, input ports.ResolveEnvironmentInput) (contracts.Environment, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Environment{}, false, fmt.Errorf("begin Environment resolve: %w", err)
	}
	defer tx.Rollback(ctx)
	lockKey, err := postgresEnvironmentNaturalKey(input.Environment)
	if err != nil {
		return contracts.Environment{}, false, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return contracts.Environment{}, false, fmt.Errorf("lock Environment natural key: %w", err)
	}
	existing, err := scanEnvironment(tx.QueryRow(ctx, environmentByNaturalKeySQL,
		input.Environment.TenantRef, input.Environment.SubjectRef, input.Environment.EnvironmentKey))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return contracts.Environment{}, false, fmt.Errorf("commit existing Environment resolve: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.Environment{}, false, fmt.Errorf("query Environment natural key: %w", err)
	}
	workspace := input.Workspace
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.workspaces (
			id, tenant_ref, subject_ref, storage_ref, generation, retain_until, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		workspace.ID, workspace.TenantRef, workspace.SubjectRef, workspace.StorageRef, workspace.Generation,
		workspace.RetainUntil, workspace.CreatedAt, workspace.UpdatedAt,
	); err != nil {
		return contracts.Environment{}, false, fmt.Errorf("insert Environment workspace: %w", err)
	}
	environment := input.Environment
	portsJSON, metadataJSON, err := environmentJSON(environment)
	if err != nil {
		return contracts.Environment{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.environments (
			id,tenant_ref,subject_ref,environment_key,workspace_id,image_ref,toolchain_ref,
			resource_class_id,lifecycle_policy_id,desired_state,state,current_generation,
			current_instance_id,snapshot_id,exposed_ports_json,metadata_json,last_activity_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		environment.ID, environment.TenantRef, environment.SubjectRef, environment.EnvironmentKey,
		environment.WorkspaceID, environment.ImageRef, environment.ToolchainRef, environment.ResourceClassID,
		environment.LifecyclePolicyID, environment.DesiredState, environment.State, environment.CurrentGeneration,
		environment.CurrentInstanceID, environment.SnapshotID, portsJSON, metadataJSON,
		environment.LastActivityAt, environment.CreatedAt, environment.UpdatedAt,
	); err != nil {
		return contracts.Environment{}, false, fmt.Errorf("insert Environment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Environment{}, false, fmt.Errorf("commit Environment resolve: %w", err)
	}
	return environment, true, nil
}

func (s *PostgresEnvironmentStore) GetEnvironment(ctx context.Context, id string) (contracts.Environment, error) {
	environment, err := scanEnvironment(s.pool.QueryRow(ctx, environmentByIDSQL, id))
	return environment, mapEnvironmentError(err)
}

func (s *PostgresEnvironmentStore) GetWorkspace(ctx context.Context, id string) (contracts.Workspace, error) {
	var workspace contracts.Workspace
	err := s.pool.QueryRow(ctx, `
		SELECT id,tenant_ref,subject_ref,storage_ref,generation,retain_until,created_at,updated_at
		FROM sandbox.workspaces WHERE id=$1`, id).Scan(
		&workspace.ID, &workspace.TenantRef, &workspace.SubjectRef, &workspace.StorageRef,
		&workspace.Generation, &workspace.RetainUntil, &workspace.CreatedAt, &workspace.UpdatedAt,
	)
	workspace.ContractVersion = contracts.ContractVersionV1
	return workspace, mapEnvironmentError(err)
}

func (s *PostgresEnvironmentStore) GetCurrentInstance(ctx context.Context, environmentID string) (*contracts.Instance, error) {
	var instance contracts.Instance
	err := scanInstance(s.pool.QueryRow(ctx, `
		SELECT i.id,i.environment_id,i.generation,i.state,i.backend_ref,i.failure_code,
		       i.prepared_at,i.ready_at,i.stopped_at,i.updated_at
		FROM sandbox.environments e
		JOIN sandbox.instances i ON i.id=e.current_instance_id
		WHERE e.id=$1 AND e.current_instance_id<>''`, environmentID), &instance)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, environmentErr := s.GetEnvironment(ctx, environmentID); environmentErr != nil {
			return nil, environmentErr
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get current Environment Instance: %w", err)
	}
	return &instance, nil
}

func (s *PostgresEnvironmentStore) ListInstances(ctx context.Context, environmentID string) ([]contracts.Instance, error) {
	if _, err := s.GetEnvironment(ctx, environmentID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id,environment_id,generation,state,backend_ref,failure_code,prepared_at,ready_at,stopped_at,updated_at
		FROM sandbox.instances WHERE environment_id=$1 ORDER BY generation,id`, environmentID)
	if err != nil {
		return nil, fmt.Errorf("list Environment Instances: %w", err)
	}
	defer rows.Close()
	items := make([]contracts.Instance, 0)
	for rows.Next() {
		var instance contracts.Instance
		if err := scanInstance(rows, &instance); err != nil {
			return nil, fmt.Errorf("scan Environment Instance: %w", err)
		}
		items = append(items, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Environment Instances: %w", err)
	}
	return items, nil
}

func (s *PostgresEnvironmentStore) GetResourceClass(ctx context.Context, id string) (contracts.ResourceClass, error) {
	var resourceClass contracts.ResourceClass
	err := s.pool.QueryRow(ctx, `
		SELECT id,cpu_millis,memory_bytes,disk_bytes,process_limit,max_exposed_ports,created_at,updated_at
		FROM sandbox.resource_classes WHERE id=$1`, id).Scan(
		&resourceClass.ID, &resourceClass.CPUMillis, &resourceClass.MemoryBytes, &resourceClass.DiskBytes,
		&resourceClass.ProcessLimit, &resourceClass.MaxExposedPorts, &resourceClass.CreatedAt, &resourceClass.UpdatedAt,
	)
	resourceClass.ContractVersion = contracts.ContractVersionV1
	return resourceClass, mapEnvironmentError(err)
}

func (s *PostgresEnvironmentStore) ListResourceClasses(ctx context.Context) ([]contracts.ResourceClass, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,cpu_millis,memory_bytes,disk_bytes,process_limit,max_exposed_ports,created_at,updated_at
		FROM sandbox.resource_classes ORDER BY id LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list Resource Classes: %w", err)
	}
	defer rows.Close()
	items := make([]contracts.ResourceClass, 0)
	for rows.Next() {
		var item contracts.ResourceClass
		if err := rows.Scan(&item.ID, &item.CPUMillis, &item.MemoryBytes, &item.DiskBytes,
			&item.ProcessLimit, &item.MaxExposedPorts, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Resource Class: %w", err)
		}
		item.ContractVersion = contracts.ContractVersionV1
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresEnvironmentStore) GetLifecyclePolicy(ctx context.Context, id string) (contracts.LifecyclePolicy, error) {
	var policy contracts.LifecyclePolicy
	err := s.pool.QueryRow(ctx, `
		SELECT id,idle_stop_after_seconds,retention_seconds,stop_compute_when_idle,
		       retain_on_explicit_stop,keep_running_without_wake,created_at,updated_at
		FROM sandbox.lifecycle_policies WHERE id=$1`, id).Scan(
		&policy.ID, &policy.IdleStopAfterSeconds, &policy.RetentionSeconds, &policy.StopComputeWhenIdle,
		&policy.RetainOnExplicitStop, &policy.KeepRunningWithoutWake, &policy.CreatedAt, &policy.UpdatedAt,
	)
	policy.ContractVersion = contracts.ContractVersionV1
	return policy, mapEnvironmentError(err)
}

func (s *PostgresEnvironmentStore) ListLifecyclePolicies(ctx context.Context) ([]contracts.LifecyclePolicy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,idle_stop_after_seconds,retention_seconds,stop_compute_when_idle,
		       retain_on_explicit_stop,keep_running_without_wake,created_at,updated_at
		FROM sandbox.lifecycle_policies ORDER BY id LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list Lifecycle Policies: %w", err)
	}
	defer rows.Close()
	items := make([]contracts.LifecyclePolicy, 0)
	for rows.Next() {
		var item contracts.LifecyclePolicy
		if err := rows.Scan(&item.ID, &item.IdleStopAfterSeconds, &item.RetentionSeconds, &item.StopComputeWhenIdle,
			&item.RetainOnExplicitStop, &item.KeepRunningWithoutWake, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan Lifecycle Policy: %w", err)
		}
		item.ContractVersion = contracts.ContractVersionV1
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresEnvironmentStore) BeginStart(ctx context.Context, environmentID string, expectedGeneration int64, instanceID string, now time.Time) (ports.StartGenerationResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ports.StartGenerationResult{}, fmt.Errorf("begin Environment start: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return ports.StartGenerationResult{}, mapEnvironmentError(err)
	}
	if expectedGeneration > 0 && expectedGeneration != environment.CurrentGeneration {
		return ports.StartGenerationResult{}, ports.ErrGenerationFenced
	}
	if (environment.State == contracts.EnvironmentStatePreparing ||
		environment.State == contracts.EnvironmentStateReady) &&
		environment.CurrentInstanceID != "" {
		var instance contracts.Instance
		if err := scanInstance(tx.QueryRow(ctx, instanceByIDSQL, environment.CurrentInstanceID), &instance); err != nil {
			return ports.StartGenerationResult{}, fmt.Errorf("load active Instance start: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ports.StartGenerationResult{}, fmt.Errorf("commit reused Environment start: %w", err)
		}
		return ports.StartGenerationResult{Environment: environment, Instance: instance}, nil
	}
	generation := environment.CurrentGeneration + 1
	instance := contracts.Instance{
		ContractVersion: contracts.ContractVersionV1,
		ID:              instanceID, EnvironmentID: environmentID, Generation: generation,
		State: contracts.InstanceStatePreparing, PreparedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.instances (
			id,environment_id,generation,state,backend_ref,failure_code,prepared_at,ready_at,stopped_at,updated_at
		) VALUES ($1,$2,$3,$4,'','',$5,NULL,NULL,$5)`,
		instance.ID, instance.EnvironmentID, instance.Generation, instance.State, now.UTC()); err != nil {
		return ports.StartGenerationResult{}, fmt.Errorf("insert preparing Instance: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET current_generation=$2,current_instance_id=$3,
		       desired_state=$4,state=$5,updated_at=$6 WHERE id=$1`,
		environmentID, generation, instanceID, contracts.DesiredStateRunning, contracts.EnvironmentStatePreparing, now.UTC()); err != nil {
		return ports.StartGenerationResult{}, fmt.Errorf("reserve Environment generation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.leases SET state=$3,updated_at=$4
		WHERE environment_id=$1 AND generation<$2 AND state=$5`,
		environmentID, generation, contracts.LeaseStateExpired, now.UTC(), contracts.LeaseStateActive); err != nil {
		return ports.StartGenerationResult{}, fmt.Errorf("fence prior Environment leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ports.StartGenerationResult{}, fmt.Errorf("commit Environment start: %w", err)
	}
	environment.CurrentGeneration = generation
	environment.CurrentInstanceID = instanceID
	environment.DesiredState = contracts.DesiredStateRunning
	environment.State = contracts.EnvironmentStatePreparing
	environment.UpdatedAt = now.UTC()
	return ports.StartGenerationResult{Environment: environment, Instance: instance, Created: true}, nil
}

func (s *PostgresEnvironmentStore) MarkInstanceReady(ctx context.Context, environmentID, instanceID string, generation int64, backendRef string, now time.Time) (contracts.Environment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Environment{}, fmt.Errorf("begin Instance ready: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return contracts.Environment{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != generation || environment.CurrentInstanceID != instanceID {
		return contracts.Environment{}, ports.ErrGenerationFenced
	}
	tag, err := tx.Exec(ctx, `
		UPDATE sandbox.instances SET state=$4,backend_ref=$5,ready_at=$6,updated_at=$6
		WHERE id=$1 AND environment_id=$2 AND generation=$3`,
		instanceID, environmentID, generation, contracts.InstanceStateReady, backendRef, now.UTC())
	if err != nil {
		return contracts.Environment{}, fmt.Errorf("mark Instance ready: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.Environment{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET desired_state=$2,state=$3,last_activity_at=$4,updated_at=$4 WHERE id=$1`,
		environmentID, contracts.DesiredStateRunning, contracts.EnvironmentStateReady, now.UTC()); err != nil {
		return contracts.Environment{}, fmt.Errorf("mark Environment ready: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Environment{}, fmt.Errorf("commit Instance ready: %w", err)
	}
	environment.State = contracts.EnvironmentStateReady
	environment.DesiredState = contracts.DesiredStateRunning
	environment.LastActivityAt = now.UTC()
	environment.UpdatedAt = now.UTC()
	return environment, nil
}

func (s *PostgresEnvironmentStore) MarkInstanceFailed(ctx context.Context, environmentID, instanceID string, generation int64, failureCode string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Instance failure: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != generation || environment.CurrentInstanceID != instanceID {
		return ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.instances SET state=$4,failure_code=$5,updated_at=$6
		WHERE id=$1 AND environment_id=$2 AND generation=$3`,
		instanceID, environmentID, generation, contracts.InstanceStateFailed, failureCode, now.UTC()); err != nil {
		return fmt.Errorf("mark Instance failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET desired_state=$2,state=$3,updated_at=$4 WHERE id=$1`,
		environmentID, contracts.DesiredStateStopped, contracts.EnvironmentStateFailed, now.UTC()); err != nil {
		return fmt.Errorf("mark Environment failed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Instance failure: %w", err)
	}
	return nil
}

func (s *PostgresEnvironmentStore) BeginStop(ctx context.Context, environmentID string, expectedGeneration int64, now time.Time) (contracts.Environment, *contracts.Instance, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Environment{}, nil, fmt.Errorf("begin Environment stop: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return contracts.Environment{}, nil, mapEnvironmentError(err)
	}
	if expectedGeneration > 0 && expectedGeneration != environment.CurrentGeneration {
		return contracts.Environment{}, nil, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.leases SET state=$2,updated_at=$3
		WHERE environment_id=$1 AND state=$4`,
		environmentID, contracts.LeaseStateExpired, now.UTC(), contracts.LeaseStateActive); err != nil {
		return contracts.Environment{}, nil, fmt.Errorf("fence Environment leases for stop: %w", err)
	}
	var instance *contracts.Instance
	if environment.CurrentInstanceID != "" && environment.State != contracts.EnvironmentStateStopped {
		current := contracts.Instance{}
		if err := scanInstance(tx.QueryRow(ctx, instanceByIDSQL, environment.CurrentInstanceID), &current); err != nil {
			return contracts.Environment{}, nil, fmt.Errorf("load stopping Instance: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE sandbox.instances SET state=$2,updated_at=$3 WHERE id=$1`,
			current.ID, contracts.InstanceStateStopping, now.UTC()); err != nil {
			return contracts.Environment{}, nil, fmt.Errorf("mark Instance stopping: %w", err)
		}
		current.State = contracts.InstanceStateStopping
		current.UpdatedAt = now.UTC()
		instance = &current
		environment.State = contracts.EnvironmentStateStopping
	} else {
		environment.State = contracts.EnvironmentStateStopped
	}
	environment.DesiredState = contracts.DesiredStateStopped
	environment.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET desired_state=$2,state=$3,updated_at=$4 WHERE id=$1`,
		environmentID, environment.DesiredState, environment.State, now.UTC()); err != nil {
		return contracts.Environment{}, nil, fmt.Errorf("mark Environment stopping: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Environment{}, nil, fmt.Errorf("commit Environment stop: %w", err)
	}
	return environment, instance, nil
}

func (s *PostgresEnvironmentStore) CompleteStop(ctx context.Context, environmentID, instanceID string, generation int64, instanceState string, now time.Time) (contracts.Environment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Environment{}, fmt.Errorf("begin Environment stop completion: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return contracts.Environment{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != generation || environment.CurrentInstanceID != instanceID {
		return contracts.Environment{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.instances SET state=$4,stopped_at=$5,updated_at=$5
		WHERE id=$1 AND environment_id=$2 AND generation=$3`,
		instanceID, environmentID, generation, instanceState, now.UTC()); err != nil {
		return contracts.Environment{}, fmt.Errorf("complete Instance stop: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET desired_state=$2,state=$3,current_instance_id='',updated_at=$4 WHERE id=$1`,
		environmentID, contracts.DesiredStateStopped, contracts.EnvironmentStateStopped, now.UTC()); err != nil {
		return contracts.Environment{}, fmt.Errorf("complete Environment stop: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Environment{}, fmt.Errorf("commit Environment stop completion: %w", err)
	}
	environment.DesiredState = contracts.DesiredStateStopped
	environment.State = contracts.EnvironmentStateStopped
	environment.CurrentInstanceID = ""
	environment.UpdatedAt = now.UTC()
	return environment, nil
}

func (s *PostgresEnvironmentStore) MarkInstanceLost(ctx context.Context, environmentID, instanceID string, generation int64, now time.Time) (contracts.Environment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Environment{}, fmt.Errorf("begin lost Instance transition: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return contracts.Environment{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != generation || environment.CurrentInstanceID != instanceID {
		return contracts.Environment{}, ports.ErrGenerationFenced
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox.instances SET state=$2,updated_at=$3 WHERE id=$1`,
		instanceID, contracts.InstanceStateLost, now.UTC()); err != nil {
		return contracts.Environment{}, fmt.Errorf("mark Instance lost: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.environments SET desired_state=$2,state=$3,current_instance_id='',updated_at=$4 WHERE id=$1`,
		environmentID, contracts.DesiredStateStopped, contracts.EnvironmentStateLost, now.UTC()); err != nil {
		return contracts.Environment{}, fmt.Errorf("mark Environment lost: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sandbox.leases SET state=$2,updated_at=$3
		WHERE environment_id=$1 AND state=$4`,
		environmentID, contracts.LeaseStateExpired, now.UTC(), contracts.LeaseStateActive); err != nil {
		return contracts.Environment{}, fmt.Errorf("fence leases after Instance loss: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Environment{}, fmt.Errorf("commit lost Instance transition: %w", err)
	}
	environment.DesiredState = contracts.DesiredStateStopped
	environment.State = contracts.EnvironmentStateLost
	environment.CurrentInstanceID = ""
	environment.UpdatedAt = now.UTC()
	return environment, nil
}

func (s *PostgresEnvironmentStore) TouchEnvironment(ctx context.Context, environmentID string, generation int64, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sandbox.environments SET last_activity_at=$3,updated_at=$3
		WHERE id=$1 AND current_generation=$2`, environmentID, generation, now.UTC())
	if err != nil {
		return fmt.Errorf("touch Environment activity: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrGenerationFenced
	}
	return nil
}

func (s *PostgresEnvironmentStore) CreateLease(ctx context.Context, lease contracts.Lease, now time.Time) (contracts.Lease, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("begin lease acquisition: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, lease.EnvironmentID))
	if err != nil {
		return contracts.Lease{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != lease.Generation || environment.State != contracts.EnvironmentStateReady {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	lease.ContractVersion = contracts.ContractVersionV1
	lease.State = contracts.LeaseStateActive
	lease.CreatedAt = now.UTC()
	lease.UpdatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.leases (
			id,environment_id,generation,holder_ref,state,expires_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`,
		lease.ID, lease.EnvironmentID, lease.Generation, lease.HolderRef, lease.State,
		lease.ExpiresAt, now.UTC()); err != nil {
		return contracts.Lease{}, fmt.Errorf("insert Environment lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Lease{}, fmt.Errorf("commit lease acquisition: %w", err)
	}
	return lease, nil
}

func (s *PostgresEnvironmentStore) GetLease(ctx context.Context, id string) (contracts.Lease, error) {
	var lease contracts.Lease
	err := s.pool.QueryRow(ctx, `
		SELECT id,environment_id,generation,holder_ref,state,expires_at,created_at,updated_at
		FROM sandbox.leases WHERE id=$1`, id).Scan(
		&lease.ID, &lease.EnvironmentID, &lease.Generation, &lease.HolderRef, &lease.State,
		&lease.ExpiresAt, &lease.CreatedAt, &lease.UpdatedAt,
	)
	lease.ContractVersion = contracts.ContractVersionV1
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	return lease, err
}

func (s *PostgresEnvironmentStore) HasActiveLease(ctx context.Context, environmentID string, now time.Time) (bool, error) {
	if _, err := s.GetEnvironment(ctx, environmentID); err != nil {
		return false, err
	}
	var active bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sandbox.leases
			WHERE environment_id=$1 AND state=$2 AND expires_at>$3
		)`, environmentID, contracts.LeaseStateActive, now.UTC()).Scan(&active)
	return active, err
}

func (s *PostgresEnvironmentStore) RenewLease(ctx context.Context, id string, expiresAt, now time.Time) (contracts.Lease, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("begin lease renewal: %w", err)
	}
	defer tx.Rollback(ctx)
	lease, err := scanLease(tx.QueryRow(ctx, `
		SELECT id,environment_id,generation,holder_ref,state,expires_at,created_at,updated_at
		FROM sandbox.leases WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("load lease for renewal: %w", err)
	}
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDSQL, lease.EnvironmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("load lease Environment for renewal: %w", err)
	}
	if environment.CurrentGeneration != lease.Generation {
		return contracts.Lease{}, ports.ErrGenerationFenced
	}
	if lease.State != contracts.LeaseStateActive {
		return contracts.Lease{}, ports.ErrLeaseReleased
	}
	if !lease.ExpiresAt.After(now) {
		if _, err := tx.Exec(ctx, `UPDATE sandbox.leases SET state=$2,updated_at=$3 WHERE id=$1`,
			id, contracts.LeaseStateExpired, now.UTC()); err != nil {
			return contracts.Lease{}, fmt.Errorf("expire stale lease: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return contracts.Lease{}, fmt.Errorf("commit stale lease expiry: %w", err)
		}
		return contracts.Lease{}, ports.ErrLeaseExpired
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox.leases SET expires_at=$2,updated_at=$3 WHERE id=$1`,
		id, expiresAt.UTC(), now.UTC()); err != nil {
		return contracts.Lease{}, fmt.Errorf("renew Environment lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Lease{}, fmt.Errorf("commit lease renewal: %w", err)
	}
	lease.ExpiresAt = expiresAt.UTC()
	lease.UpdatedAt = now.UTC()
	return lease, nil
}

func (s *PostgresEnvironmentStore) ReleaseLease(ctx context.Context, id string, now time.Time) (contracts.Lease, error) {
	lease, err := s.GetLease(ctx, id)
	if err != nil {
		return contracts.Lease{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sandbox.leases SET state=$2,updated_at=$3 WHERE id=$1`,
		id, contracts.LeaseStateReleased, now.UTC())
	if err != nil {
		return contracts.Lease{}, fmt.Errorf("release Environment lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contracts.Lease{}, ports.ErrLeaseNotFound
	}
	lease.State = contracts.LeaseStateReleased
	lease.UpdatedAt = now.UTC()
	return lease, nil
}

func (s *PostgresEnvironmentStore) SaveSnapshot(ctx context.Context, snapshot contracts.Snapshot, now time.Time) (contracts.Snapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("begin Snapshot persistence: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, snapshot.EnvironmentID))
	if err != nil {
		return contracts.Snapshot{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != snapshot.Generation {
		return contracts.Snapshot{}, ports.ErrGenerationFenced
	}
	metadataJSON, err := json.Marshal(snapshot.Metadata)
	if err != nil {
		return contracts.Snapshot{}, fmt.Errorf("encode Snapshot metadata: %w", err)
	}
	snapshot.ContractVersion = contracts.ContractVersionV1
	snapshot.CreatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.snapshots (
			id,environment_id,workspace_id,generation,parent_snapshot_id,opaque_ref,content_hash,size_bytes,metadata_json,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		snapshot.ID, snapshot.EnvironmentID, snapshot.WorkspaceID, snapshot.Generation,
		snapshot.ParentSnapshotID, snapshot.OpaqueRef, snapshot.ContentHash, snapshot.SizeBytes, metadataJSON, snapshot.CreatedAt); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("insert Environment Snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sandbox.environments SET snapshot_id=$2,updated_at=$3 WHERE id=$1`,
		snapshot.EnvironmentID, snapshot.ID, now.UTC()); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("attach Environment Snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("commit Snapshot persistence: %w", err)
	}
	return snapshot, nil
}

func (s *PostgresEnvironmentStore) GetSnapshot(ctx context.Context, id string) (contracts.Snapshot, error) {
	var snapshot contracts.Snapshot
	var metadataJSON []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id,environment_id,workspace_id,generation,parent_snapshot_id,opaque_ref,content_hash,size_bytes,metadata_json,created_at
		FROM sandbox.snapshots WHERE id=$1`, id).Scan(
		&snapshot.ID, &snapshot.EnvironmentID, &snapshot.WorkspaceID, &snapshot.Generation,
		&snapshot.ParentSnapshotID, &snapshot.OpaqueRef, &snapshot.ContentHash, &snapshot.SizeBytes,
		&metadataJSON, &snapshot.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.Snapshot{}, ports.ErrEnvironmentNotFound
	}
	if err != nil {
		return contracts.Snapshot{}, err
	}
	snapshot.ContractVersion = contracts.ContractVersionV1
	if err := json.Unmarshal(metadataJSON, &snapshot.Metadata); err != nil {
		return contracts.Snapshot{}, fmt.Errorf("decode Snapshot metadata: %w", err)
	}
	return snapshot, nil
}

func (s *PostgresEnvironmentStore) CommitWorkspaceVersion(ctx context.Context, version contracts.WorkspaceVersion) (contracts.WorkspaceVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "sandbox-workspace-version:"+version.EnvironmentID); err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	existing, err := scanWorkspaceVersion(tx.QueryRow(ctx, workspaceVersionByTurnSQL, version.EnvironmentID, version.TerminalTurnID))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return contracts.WorkspaceVersion{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceVersion{}, err
	}
	var previous *contracts.WorkspaceVersion
	current, err := scanWorkspaceVersion(tx.QueryRow(ctx, currentWorkspaceVersionForUpdateSQL, version.EnvironmentID))
	if err == nil {
		previous = &current
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceVersion{}, err
	}
	version.ContractVersion = contracts.ContractVersionV1
	version.LogicalVersion = 1
	if previous != nil {
		version.LogicalVersion = previous.LogicalVersion + 1
		if !version.Dirty && previous.SourceGeneration == version.SourceGeneration {
			version.SnapshotID = previous.SnapshotID
			version.SnapshotLogicalVersion = previous.SnapshotLogicalVersion
		}
	}
	if version.Dirty {
		version.SnapshotLogicalVersion = version.LogicalVersion
	}
	version, err = scanWorkspaceVersion(tx.QueryRow(ctx, `
		INSERT INTO sandbox.workspace_versions (
			environment_id,logical_version,source_generation,terminal_turn_id,terminal_status,
			workspace_present,dirty,content_hash,snapshot_id,snapshot_logical_version,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING environment_id,logical_version,source_generation,terminal_turn_id,terminal_status,
		          workspace_present,dirty,content_hash,snapshot_id,snapshot_logical_version,created_at`,
		version.EnvironmentID, version.LogicalVersion, version.SourceGeneration, version.TerminalTurnID,
		version.TerminalStatus, version.WorkspacePresent, version.Dirty, version.ContentHash,
		version.SnapshotID, version.SnapshotLogicalVersion, version.CreatedAt,
	))
	if err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.WorkspaceVersion{}, err
	}
	return version, nil
}

func (s *PostgresEnvironmentStore) GetCurrentWorkspaceVersion(ctx context.Context, environmentID string) (*contracts.WorkspaceVersion, error) {
	version, err := scanWorkspaceVersion(s.pool.QueryRow(ctx, currentWorkspaceVersionSQL, environmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, environmentErr := s.GetEnvironment(ctx, environmentID); environmentErr != nil {
			return nil, environmentErr
		}
		return nil, nil
	}
	return &version, err
}

func (s *PostgresEnvironmentStore) GetWorkspaceVersion(ctx context.Context, environmentID string, logicalVersion int64) (contracts.WorkspaceVersion, error) {
	version, err := scanWorkspaceVersion(s.pool.QueryRow(ctx, workspaceVersionByNumberSQL, environmentID, logicalVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return contracts.WorkspaceVersion{}, ports.ErrEnvironmentNotFound
	}
	return version, err
}

func (s *PostgresEnvironmentStore) SaveArtifact(ctx context.Context, artifact contracts.Artifact, now time.Time) (contracts.Artifact, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("begin Artifact persistence: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, artifact.EnvironmentID))
	if err != nil {
		return contracts.Artifact{}, mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != artifact.Generation {
		return contracts.Artifact{}, ports.ErrGenerationFenced
	}
	metadataJSON, err := json.Marshal(artifact.Metadata)
	if err != nil {
		return contracts.Artifact{}, fmt.Errorf("encode Artifact metadata: %w", err)
	}
	artifact.ContractVersion = contracts.ContractVersionV1
	artifact.CreatedAt = now.UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sandbox.artifacts (
			id,environment_id,generation,name,mime_type,size_bytes,sha256,opaque_ref,metadata_json,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		artifact.ID, artifact.EnvironmentID, artifact.Generation, artifact.Name, artifact.MimeType,
		artifact.SizeBytes, artifact.SHA256, artifact.OpaqueRef, metadataJSON, artifact.CreatedAt); err != nil {
		return contracts.Artifact{}, fmt.Errorf("insert Environment Artifact: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return contracts.Artifact{}, fmt.Errorf("commit Artifact persistence: %w", err)
	}
	return artifact, nil
}

func (s *PostgresEnvironmentStore) ListLifecycleCandidates(ctx context.Context, now time.Time, limit int) ([]contracts.Environment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e.id,e.tenant_ref,e.subject_ref,e.environment_key,e.workspace_id,e.image_ref,e.toolchain_ref,
		       e.resource_class_id,e.lifecycle_policy_id,e.desired_state,e.state,e.current_generation,
		       e.current_instance_id,e.snapshot_id,e.exposed_ports_json,e.metadata_json,
		       e.last_activity_at,e.created_at,e.updated_at
		FROM sandbox.environments e
		WHERE NOT EXISTS (
			SELECT 1 FROM sandbox.leases l
			WHERE l.environment_id=e.id AND l.state=$1 AND l.expires_at>$2
		)
		ORDER BY e.last_activity_at,e.id LIMIT $3`,
		contracts.LeaseStateActive, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list Environment lifecycle candidates: %w", err)
	}
	defer rows.Close()
	items := make([]contracts.Environment, 0)
	for rows.Next() {
		item, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan Environment lifecycle candidate: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *PostgresEnvironmentStore) PurgeEnvironment(ctx context.Context, environmentID string, generation int64, _ time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Environment purge: %w", err)
	}
	defer tx.Rollback(ctx)
	environment, err := scanEnvironment(tx.QueryRow(ctx, environmentByIDForUpdateSQL, environmentID))
	if err != nil {
		return mapEnvironmentError(err)
	}
	if environment.CurrentGeneration != generation {
		return ports.ErrGenerationFenced
	}
	for _, statement := range []string{
		`DELETE FROM sandbox.artifacts WHERE environment_id=$1`,
		`DELETE FROM sandbox.workspace_versions WHERE environment_id=$1`,
		`DELETE FROM sandbox.snapshots WHERE environment_id=$1`,
		`DELETE FROM sandbox.leases WHERE environment_id=$1`,
		`DELETE FROM sandbox.instances WHERE environment_id=$1`,
		`DELETE FROM sandbox.environments WHERE id=$1`,
	} {
		if _, err := tx.Exec(ctx, statement, environmentID); err != nil {
			return fmt.Errorf("purge Environment authority rows: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sandbox.workspaces WHERE id=$1`, environment.WorkspaceID); err != nil {
		return fmt.Errorf("purge Environment workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Environment purge: %w", err)
	}
	return nil
}

const workspaceVersionColumns = `
	environment_id,logical_version,source_generation,terminal_turn_id,terminal_status,
	workspace_present,dirty,content_hash,snapshot_id,snapshot_logical_version,created_at`
const workspaceVersionByTurnSQL = `SELECT ` + workspaceVersionColumns + ` FROM sandbox.workspace_versions WHERE environment_id=$1 AND terminal_turn_id=$2`
const workspaceVersionByNumberSQL = `SELECT ` + workspaceVersionColumns + ` FROM sandbox.workspace_versions WHERE environment_id=$1 AND logical_version=$2`
const currentWorkspaceVersionSQL = `SELECT ` + workspaceVersionColumns + ` FROM sandbox.workspace_versions WHERE environment_id=$1 ORDER BY logical_version DESC LIMIT 1`
const currentWorkspaceVersionForUpdateSQL = currentWorkspaceVersionSQL + ` FOR UPDATE`

func scanWorkspaceVersion(row rowScanner) (contracts.WorkspaceVersion, error) {
	var version contracts.WorkspaceVersion
	err := row.Scan(
		&version.EnvironmentID, &version.LogicalVersion, &version.SourceGeneration,
		&version.TerminalTurnID, &version.TerminalStatus, &version.WorkspacePresent,
		&version.Dirty, &version.ContentHash, &version.SnapshotID,
		&version.SnapshotLogicalVersion, &version.CreatedAt,
	)
	version.ContractVersion = contracts.ContractVersionV1
	return version, err
}

const environmentColumns = `
	id,tenant_ref,subject_ref,environment_key,workspace_id,image_ref,toolchain_ref,
	resource_class_id,lifecycle_policy_id,desired_state,state,current_generation,
	current_instance_id,snapshot_id,exposed_ports_json,metadata_json,last_activity_at,created_at,updated_at`

const environmentByIDSQL = `SELECT ` + environmentColumns + ` FROM sandbox.environments WHERE id=$1`
const environmentByIDForUpdateSQL = environmentByIDSQL + ` FOR UPDATE`
const environmentByNaturalKeySQL = `SELECT ` + environmentColumns + `
	FROM sandbox.environments WHERE tenant_ref=$1 AND subject_ref=$2 AND environment_key=$3`

type rowScanner interface {
	Scan(...any) error
}

func scanEnvironment(row rowScanner) (contracts.Environment, error) {
	var environment contracts.Environment
	var portsJSON, metadataJSON []byte
	err := row.Scan(
		&environment.ID, &environment.TenantRef, &environment.SubjectRef, &environment.EnvironmentKey,
		&environment.WorkspaceID, &environment.ImageRef, &environment.ToolchainRef, &environment.ResourceClassID,
		&environment.LifecyclePolicyID, &environment.DesiredState, &environment.State, &environment.CurrentGeneration,
		&environment.CurrentInstanceID, &environment.SnapshotID, &portsJSON, &metadataJSON,
		&environment.LastActivityAt, &environment.CreatedAt, &environment.UpdatedAt,
	)
	if err != nil {
		return contracts.Environment{}, err
	}
	environment.ContractVersion = contracts.ContractVersionV1
	if err := json.Unmarshal(portsJSON, &environment.ExposedPorts); err != nil {
		return contracts.Environment{}, fmt.Errorf("decode Environment exposed ports: %w", err)
	}
	if err := json.Unmarshal(metadataJSON, &environment.Metadata); err != nil {
		return contracts.Environment{}, fmt.Errorf("decode Environment metadata: %w", err)
	}
	return environment, nil
}

const instanceByIDSQL = `
	SELECT id,environment_id,generation,state,backend_ref,failure_code,prepared_at,ready_at,stopped_at,updated_at
	FROM sandbox.instances WHERE id=$1`

func scanInstance(row rowScanner, instance *contracts.Instance) error {
	var readyAt, stoppedAt *time.Time
	err := row.Scan(
		&instance.ID, &instance.EnvironmentID, &instance.Generation, &instance.State, &instance.BackendRef,
		&instance.FailureCode, &instance.PreparedAt, &readyAt, &stoppedAt, &instance.UpdatedAt,
	)
	if err != nil {
		return err
	}
	instance.ContractVersion = contracts.ContractVersionV1
	if readyAt != nil {
		instance.ReadyAt = *readyAt
	}
	if stoppedAt != nil {
		instance.StoppedAt = *stoppedAt
	}
	return nil
}

func scanLease(row rowScanner) (contracts.Lease, error) {
	var lease contracts.Lease
	err := row.Scan(
		&lease.ID, &lease.EnvironmentID, &lease.Generation, &lease.HolderRef, &lease.State,
		&lease.ExpiresAt, &lease.CreatedAt, &lease.UpdatedAt,
	)
	lease.ContractVersion = contracts.ContractVersionV1
	return lease, err
}

func environmentJSON(environment contracts.Environment) ([]byte, []byte, error) {
	portsJSON, err := json.Marshal(environment.ExposedPorts)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Environment exposed ports: %w", err)
	}
	metadataJSON, err := json.Marshal(environment.Metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Environment metadata: %w", err)
	}
	return portsJSON, metadataJSON, nil
}

func mapEnvironmentError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrEnvironmentNotFound
	}
	return err
}
